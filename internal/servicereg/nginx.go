package servicereg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/huangchengsir/pipewright/internal/target"
)

// 网关容器编排常量(与 caddy.go 同构,但资源名独立,避免与 Caddy 反代冲突)。
const (
	// nginxConfPath 是容器内托管主配置路径(挂具名卷,容器重建不丢)。
	nginxConfPath = "/etc/pipewright/nginx.conf"
	// nginxConfDir 是容器内每服务配置目录(同卷):每个 http 站点(同 FQDN 多 path 合并)
	// 一个 <fqdn>.conf,每个 tcp 服务一个 <fqdn>.tcp.conf,由主配置显式 include。
	nginxConfDir = "/etc/pipewright/conf.d"
	// nginxCertsDir 是容器内证书目录:certs/<base_domain>/{fullchain,privkey}.pem。
	nginxCertsDir = "/etc/pipewright/certs"
	// nginxStageDir 是配置下发中转目录(宿主与容器内同路径):先测后换的隔离沙箱,
	// 测试版主配置的 include 指向中转 conf.d,校验通过才替换正式配置。
	nginxStageDir = "/tmp/pipewright-nginx-stage"
	// certTmpPrefix 是证书宿主临时落盘路径(Upload 后 docker cp)。
	certTmpPrefix = "/tmp/pipewright-sr-cert-"
	// nginxImageEnv 允许经环境变量覆盖镜像(默认 nginx:stable-alpine,官方镜像自带
	// stream 动态模块 + docker 内嵌 DNS 可用)。
	nginxImageEnv = "PIPEWRIGHT_NGINX_IMAGE"
	// streamModulePath 是官方 nginx 镜像的 stream 动态模块路径(TCP 反代需要)。
	streamModulePath = "/usr/lib/nginx/modules/ngx_stream_module.so"
	// nginxBootstrapLabel/Ver 是容器自举脚本/挂载形态的版本标签:入口脚本在 docker run 时烧进
	// 容器,平台升级改了脚本或挂载不会作用于既有容器,故打版本标签,ensureNginx 见版本不符即
	// 重建容器(配置/证书在具名卷,重建不丢;重建后同轮 apply 会覆盖真实配置)。
	// v3:新增静态发布目录 bind mount(StaticRoot → /etc/pipewright/static,只读)。
	nginxBootstrapLabel = "pipewright.bootstrap"
	nginxBootstrapVer   = "3"
	// nginxStaticDir 是容器内静态托管目录(bind mount 自宿主 StaticRoot,只读):
	// static/<serviceID>/current/ 由「网关静态发布」部署落盘,nginx alias 直接托管。
	nginxStaticDir = "/etc/pipewright/static"
)

// nginxImageRefRe 防注入:镜像引用字符集(首字符字母数字,防经 env 注入 docker flag)。
var nginxImageRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)

// nginxVersionRe 从 nginx -v 输出提取版本号(输出形如 "nginx version: nginx/1.27.4")。
var nginxVersionRe = regexp.MustCompile(`nginx/(\S+)`)

// nginxBootstrapMinConf 是最小合法自举配置(80 兜底 404):return 必须落在 server 上下文。
const nginxBootstrapMinConf = "events { worker_connections 64; }\nhttp { server { return 404; } }\n"

// nginxBootstrapPoisonConf 是旧版曾写入卷里的坏自举配置(return 不允许出现在 http 上下文,
// nginx 拒绝启动 → 容器崩溃循环),字节精确保留,仅用于自举时识别并修复存量脏卷。
const nginxBootstrapPoisonConf = "events { worker_connections 64; }\nhttp { return 404; }\n"

// nginxBootstrapScript 是容器启动命令(sh -c 的**固定常量**脚本,无任何用户输入,非注入面):
// 卷内配置缺失,或恰好是旧版坏自举配置时,重写最小合法配置;其余情况一律不动托管配置
// (宿主重启时上游容器可能晚起,nginx -t 瞬时解析失败也不能清掉真实配置);随后 exec 成
// 真正主进程 nginx(-c 指定托管配置,daemon off 前台运行;docker stop 信号直达)。
const nginxBootstrapScript = `conf=/etc/pipewright/nginx.conf; mkdir -p /etc/pipewright/certs; ` +
	`if [ ! -f "$conf" ] || printf '` + nginxBootstrapPoisonConf + `' | cmp -s - "$conf"; then ` +
	`printf '` + nginxBootstrapMinConf + `' > "$conf"; fi; ` +
	`exec nginx -c "$conf" -g "daemon off;"`

// nginxConfFile 是 conf.d/ 下的一份服务配置文件(纯文本,见 nginxConfRender)。
type nginxConfFile struct {
	Name    string // 文件名(http 站点 <fqdn>.conf / tcp 服务 <fqdn>.tcp.conf)
	Content string
}

// nginxConfRender 是一次渲染的完整产物:主配置 + 每服务(站点)配置文件。主配置只含
// 全局指令与兜底 server,经显式 include 逐个引用 Files(不用 conf.d/*.conf 通配:
// 引用文件逐个可见,配置回读与缺失校验都精确到文件)。
type nginxConfRender struct {
	Main  string          // /etc/pipewright/nginx.conf(include 指向 confDir)
	Files []nginxConfFile // conf.d/ 下全部文件,按文件名升序(确定性输出)
}

// Text 拼接主配置与全部服务文件(单测整体断言用;生产路径分别落盘)。
func (r *nginxConfRender) Text() string {
	var b strings.Builder
	b.WriteString(r.Main)
	for _, f := range r.Files {
		b.WriteString("\n# ---- " + f.Name + " ----\n")
		b.WriteString(f.Content)
	}
	return b.String()
}

// renderNginxConf 据基域(enabled 服务所属)与实例渲染网关配置(纯函数,可直接 golden 单测):
// 主配置 nginx.conf + conf.d/ 每服务一个文件。confDir 是 include 引用的目录(正式下发
// 用 /etc/pipewright/conf.d,先测后换的沙箱校验用中转目录,两者仅 include 路径不同)。
//
// 结构(确定性输出:基域/服务/FQDN/实例容器名全部升序,渲染幂等):
//
//	nginx.conf(主配置):
//	  load_module stream 模块      —— 仅当存在 tcp 服务
//	  events / http
//	    resolver 127.0.0.11 valid=10s  —— Docker 内嵌 DNS(address 上游 / TCP 透传的动态解析)
//	    include <confDir>/<fqdn>.conf  —— 每个 http 站点一个文件(显式逐个引用)
//	    80 default → 404;443 default → 444(未知 SNI 兜底,需任一证书)
//	  stream(tcp 服务存在时)
//	    include <confDir>/<fqdn>.tcp.conf —— 每个 tcp 服务一个文件
//
//	conf.d/<fqdn>.conf(http 站点文件;同 FQDN 多 path 的服务必须共用一个 server 块,
//	                     故文件粒度 = FQDN 站点,单服务单域名时即每服务一个文件):
//	  upstream pw_<name>[_<path>]   —— 有 attached 实例的服务的集群实例池,成员 = 服务器
//	    地址:宿主端口(静态 IP,reload 原子增减);max_fails/fail_timeout 被动健康兜底,
//	    keepalive 上游长连接池。forward 服务成员来自对端目录规划。
//	  80 → 301 跳 https             —— 证书就绪的站点(每站点自己的跳转 server 块)
//	  vhost server 块               —— 证书域 443 ssl / 无证书域 80;组内每服务一个
//	    location(实例全摘除 → return 503 维护态;static → alias 静态托管)
//
//	conf.d/<fqdn>.tcp.conf(tcp 服务文件):stream server(listen 端口 → proxy_pass
//	  上游;forward 有规划走 upstream,否则变量 + resolver 动态解析)。
//
// 所有进入文本的值已在领域层严格校验过(域名/服务名/上游/端口白名单),渲染处不再二次防注入。
// 无 enabled 服务时渲染最小合法主配置(80 返回 404,无 include),保证网关容器始终可运行。
// 集群模型:upstream 成员 = 全部 attached 且地址可解析的实例 —— 各网关主机渲染**同一份**
// 配置,本机没有实例的服务同样代理远端。skipped 是被跳过的实例数(遗留行/地址解析失败),
// 写进主配置头部注释便于诊断;故障实例交给 max_fails 熔断,不阻断渲染。
//
// forward(跨环境转发):forward 服务无本地实例,upstream 成员来自 forwardPlans(对端
// 网关 IP:端口,经 mesh 服务目录解析);无规划/无可渲染成员 → 503 维护态。forwardConflicts
// 是网关 IP 冲突告警(渲染进主配置头部注释,便于运维从配置文件侧定位)。
func renderNginxConf(confDir string, domains []Domain, services []RegisteredService, instances []Instance, serverAddr map[string]string, skipped int, forwardPlans map[string]ForwardPlan, forwardConflicts []string) *nginxConfRender {
	// 基域索引(domain_id → Domain)。
	byID := make(map[string]Domain, len(domains))
	for _, d := range domains {
		byID[d.ID] = d
	}
	// 实例索引(service_id → 可渲染 attached 实例,按(服务器, 容器)升序保证渲染确定)。
	instByService := make(map[string][]Instance)
	declaredByService := make(map[string]int)
	for _, inst := range instances {
		declaredByService[inst.ServiceID]++
		if !inst.Attached {
			continue
		}
		if _, ok := serverAddr[inst.ServerID]; !ok {
			continue // 遗留行(server_id='')或地址解析失败:跳过渲染
		}
		if inst.Container != "" && inst.HostPort <= 0 {
			continue // 容器实例未发布宿主端口:渲染出来是死成员(宿主无监听),跳过
		}
		instByService[inst.ServiceID] = append(instByService[inst.ServiceID], inst)
	}
	for sid := range instByService {
		list := instByService[sid]
		sort.Slice(list, func(i, j int) bool {
			if list[i].ServerID != list[j].ServerID {
				return list[i].ServerID < list[j].ServerID
			}
			return list[i].Container < list[j].Container
		})
		instByService[sid] = list
	}

	// http 服务按 FQDN 分组(同域名不同 path 的服务共存,合并进同一 server 块的多 location;
	// nginx 前缀 location 最长匹配与书写顺序无关);tcp 服务按监听端口升序。
	var groups []*httpGroup
	byFqdn := make(map[string]*httpGroup)
	var tcps []RegisteredService
	for _, svc := range services {
		if !svc.Enabled {
			continue
		}
		dom, ok := byID[svc.DomainID]
		if !ok {
			continue // 孤儿服务(基域已删):跳过,不渲染
		}
		if svc.Protocol == ProtocolTCP {
			tcps = append(tcps, svc)
			continue
		}
		fqdn := svc.Name + "." + dom.BaseDomain
		g := byFqdn[fqdn]
		if g == nil {
			g = &httpGroup{fqdn: fqdn, dom: dom, certd: dom.HasCert}
			byFqdn[fqdn] = g
			groups = append(groups, g)
		}
		g.sites = append(g.sites, httpSite{svc: svc, fqdn: fqdn, dom: dom, certd: dom.HasCert})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].fqdn < groups[j].fqdn })
	for _, g := range groups {
		sort.Slice(g.sites, func(i, j int) bool { return g.sites[i].svc.Path < g.sites[j].svc.Path })
	}
	sort.Slice(tcps, func(i, j int) bool { return tcps[i].TCPListenPort < tcps[j].TCPListenPort })

	// 有可渲染成员的 http 反代服务统一走 upstream 块(容器与非容器实例一视同仁);
	// 声明了实例但无可渲染成员(全摘除/地址不可解析)→ 503 维护态;
	// 无实例声明的 address 服务 → 保留单上游变量 + resolver 动态解析(人工 address 模式);
	// static 服务 → 网关直接托管静态文件(location alias),无上游/实例语义;
	// forward 服务 → 成员来自对端目录规划(有成员走 upstream,无成员/冲突 → 503 维护态)。
	needsUpstream := func(s httpSite) bool {
		return s.svc.UpstreamKind != UpstreamKindStatic && len(instByService[s.svc.ID]) > 0
	}
	maintenance := func(s httpSite) bool {
		if s.svc.UpstreamKind == UpstreamKindStatic {
			return false
		}
		if s.svc.UpstreamKind == UpstreamKindForward {
			return true // 无规划/无可渲染成员(目录不可达/对端已删/IP 冲突)→ 维护态
		}
		// 容器服务恒为部署托管(无实例即维护态);address 服务声明了实例或上游为空
		// (部署托管待首批实例上线)时同样进入维护态;仅「无实例且配了静态上游」的
		// 人工 address 模式走变量 + resolver 动态解析。
		return !needsUpstream(s) && (s.svc.UpstreamKind == UpstreamKindContainer ||
			declaredByService[s.svc.ID] > 0 || s.svc.Upstream == "")
	}

	// 每站点渲染决策(upstream/变量名 + 维护态)按 (FQDN, path) 升序一次性分配:
	// 命名 pw_<name>[_<path>] / $up_<name>[_<path>],重名(跨基域同名服务等)追加序号去重。
	upAlloc, varAlloc := newNameAlloc(), newNameAlloc()
	renders := make(map[string]*siteRender)
	for _, g := range groups {
		for _, st := range g.sites {
			r := &siteRender{}
			if st.svc.UpstreamKind != UpstreamKindStatic {
				if st.svc.UpstreamKind == UpstreamKindForward {
					if p, ok := forwardPlans[st.svc.ID]; ok && len(p.Members) > 0 {
						r.useUpstream = true
						r.upsName = upAlloc.alloc("pw_" + nginxNameID(st.svc.Name, st.svc.Path))
						plan := p
						r.plan = &plan
					} else {
						r.maintenance = true
					}
				} else {
					r.useUpstream = needsUpstream(st)
					r.maintenance = maintenance(st)
					if r.useUpstream {
						r.upsName = upAlloc.alloc("pw_" + nginxNameID(st.svc.Name, st.svc.Path))
					} else if !r.maintenance {
						r.varName = varAlloc.alloc("up_" + nginxNameID(st.svc.Name, st.svc.Path))
					}
				}
			}
			renders[st.svc.ID] = r
		}
	}

	// ---- 每服务(站点)一个文件 ------------------------------------------------
	// http 站点文件:组内各服务的 upstream 块 + 证书域的 80→443 跳转 + vhost server 块。
	httpFiles := make([]nginxConfFile, 0, len(groups))
	for _, g := range groups {
		var b strings.Builder
		b.WriteString("# 由 Pipewright 自动生成,请勿手改。\n")
		for _, st := range g.sites {
			r := renders[st.svc.ID]
			if !r.useUpstream {
				continue
			}
			if r.plan != nil {
				// forward:成员 = 对端网关 IP:端口(mesh 服务目录; Secure 时对端为 https)。
				b.WriteString("# 服务 " + st.fqdn + st.svc.Path + " 跨环境转发 → 对端 " +
					r.plan.HostHeader + "(成员 = 对端网关主机)。\n")
				b.WriteString("upstream " + r.upsName + " {\n")
				for _, ip := range r.plan.Members {
					b.WriteString("    server " + net.JoinHostPort(ip, strconv.Itoa(r.plan.Port)) +
						" max_fails=2 fail_timeout=10s;\n")
				}
				b.WriteString("    keepalive 32;\n")
				b.WriteString("}\n\n")
				continue
			}
			b.WriteString("# 服务 " + st.fqdn + st.svc.Path + " 的集群实例池(成员 = 服务器地址:宿主端口,经 reload 原子增减)。\n")
			b.WriteString("upstream " + r.upsName + " {\n")
			for _, inst := range instByService[st.svc.ID] {
				// net.JoinHostPort:IPv6 地址自动加方括号(裸 IPv6 冒号拼接会让 nginx -t 失败)。
				b.WriteString("    server " + net.JoinHostPort(serverAddr[inst.ServerID], strconv.Itoa(inst.EffectiveHostPort(&st.svc))) +
					" max_fails=2 fail_timeout=10s;\n")
			}
			// keepalive:上游长连接池(配合 proxy_http_version 1.1 + 非 WS 请求清空 Connection)。
			b.WriteString("    keepalive 32;\n")
			b.WriteString("}\n\n")
		}
		if g.certd {
			b.WriteString("# 证书就绪:80 → 443。\n")
			b.WriteString("server {\n")
			b.WriteString("    listen 80;\n")
			b.WriteString("    server_name " + g.fqdn + ";\n")
			b.WriteString("    return 301 https://$host$request_uri;\n")
			b.WriteString("}\n\n")
		}
		proto := "http"
		if g.certd {
			proto = "https"
		}
		renderHTTPServer(&b, "", g, proto, renders)
		httpFiles = append(httpFiles, nginxConfFile{Name: g.fqdn + ".conf", Content: b.String()})
	}

	// tcp 服务文件:forward 规划的 stream upstream + stream server 块(端口升序,
	// upstream 命名分配顺序随之确定)。
	tcpFiles := make([]nginxConfFile, 0, len(tcps))
	streamAlloc := newNameAlloc()
	for _, svc := range tcps {
		fqdn := svc.Name + "." + byID[svc.DomainID].BaseDomain
		var b strings.Builder
		b.WriteString("# 由 Pipewright 自动生成,请勿手改。\n")
		if svc.UpstreamKind == UpstreamKindForward {
			if p, ok := forwardPlans[svc.ID]; ok && len(p.Members) > 0 && p.Port > 0 {
				name := streamAlloc.alloc("pwf_" + nginxNameID(svc.Name, ""))
				b.WriteString("# 服务 " + svc.Name + " 跨环境转发 → 对端 TCP " + strconv.Itoa(p.Port) + "(成员 = 对端网关主机)。\n")
				b.WriteString("upstream " + name + " {\n")
				for _, ip := range p.Members {
					b.WriteString("    server " + net.JoinHostPort(ip, strconv.Itoa(p.Port)) +
						" max_fails=2 fail_timeout=10s;\n")
				}
				b.WriteString("}\n\n")
				b.WriteString("server {\n")
				b.WriteString("    listen " + strconv.Itoa(svc.TCPListenPort) + ";\n")
				b.WriteString("    proxy_pass " + name + ";\n")
				b.WriteString("}\n")
				tcpFiles = append(tcpFiles, nginxConfFile{Name: fqdn + ".tcp.conf", Content: b.String()})
				continue
			}
			// 目录不可达/对端已删/IP 冲突:stream 无 503 语义,转发到本机拒绝端口(连接拒绝即维护态)。
			b.WriteString("# 跨环境转发暂不可用(目录不可达/冲突),连接将被拒绝。\n")
			b.WriteString("server {\n")
			b.WriteString("    listen " + strconv.Itoa(svc.TCPListenPort) + ";\n")
			b.WriteString("    proxy_pass 127.0.0.1:1;\n")
			b.WriteString("}\n")
		} else {
			b.WriteString("server {\n")
			b.WriteString("    listen " + strconv.Itoa(svc.TCPListenPort) + ";\n")
			b.WriteString("    set " + nginxVarName(svc.Name) + " " + upstreamTarget(svc) + ";\n")
			b.WriteString("    proxy_pass " + nginxVarName(svc.Name) + ";\n")
			b.WriteString("}\n")
		}
		tcpFiles = append(tcpFiles, nginxConfFile{Name: fqdn + ".tcp.conf", Content: b.String()})
	}
	sort.Slice(httpFiles, func(i, j int) bool { return httpFiles[i].Name < httpFiles[j].Name })
	sort.Slice(tcpFiles, func(i, j int) bool { return tcpFiles[i].Name < tcpFiles[j].Name })
	files := append(httpFiles, tcpFiles...)

	// ---- 主配置 ---------------------------------------------------------------
	var b strings.Builder
	b.WriteString("# 由 Pipewright 自动生成,请勿手改。各注册服务(站点)的配置每服务一个文件放在 " +
		nginxConfDir + "/,由下方 include 引用;本文件与 conf.d/ 均由平台 apply 全量管理。\n")
	if skipped > 0 {
		b.WriteString("# 注意:有 " + strconv.Itoa(skipped) + " 个实例因遗留数据(待部署认领)或服务器地址解析失败未渲染。\n")
	}
	if len(forwardConflicts) > 0 {
		b.WriteString("# 注意:跨环境转发存在网关 IP 冲突(相关对端已降级为 503 维护态):" +
			strings.Join(forwardConflicts, ";") + "\n")
	}
	if len(tcps) > 0 {
		b.WriteString("load_module " + streamModulePath + ";\n")
	}
	b.WriteString("\nworker_processes auto;\nerror_log /var/log/nginx/error.log warn;\n\n")
	b.WriteString("events {\n    worker_connections 1024;\n}\n\n")
	b.WriteString("http {\n")
	b.WriteString("    include /etc/nginx/mime.types;\n")
	b.WriteString("    default_type application/octet-stream;\n")
	b.WriteString("    access_log /var/log/nginx/access.log;\n")
	b.WriteString("    sendfile on;\n")
	b.WriteString("    # 下游(客户端)长连接超时;上游长连接池见各 upstream 块的 keepalive 指令。\n")
	b.WriteString("    keepalive_timeout 65;\n\n")
	b.WriteString("    # Docker 内嵌 DNS:address 上游 / TCP 透传的运行期动态解析。\n")
	b.WriteString("    # (实例池成员为静态 IP:宿主端口,成员经 reload 原子增减。)\n")
	b.WriteString("    resolver 127.0.0.11 valid=10s ipv6=off;\n\n")
	b.WriteString("    # WebSocket 升级用($http_upgrade 非空 → Connection: upgrade)。非升级请求:\n")
	b.WriteString("    #   实例池:清空 Connection → 交给 upstream keepalive 复用后端连接;\n")
	b.WriteString("    #   address 单上游(变量反代,不复用连接):显式 close → 不留悬挂空闲连接。\n")
	b.WriteString("    map $http_upgrade $connection_upgrade {\n")
	b.WriteString("        default upgrade;\n")
	b.WriteString("        ''      \"\";\n")
	b.WriteString("    }\n")
	b.WriteString("    map $http_upgrade $connection_upgrade_addr {\n")
	b.WriteString("        default upgrade;\n")
	b.WriteString("        ''      close;\n")
	b.WriteString("    }\n\n")
	if len(httpFiles) > 0 {
		b.WriteString("    # 各注册服务的 http 站点配置(每服务/站点一个文件,文件名 = FQDN)。\n")
		for _, f := range httpFiles {
			b.WriteString("    include " + confDir + "/" + f.Name + ";\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("    server {\n")
	b.WriteString("        listen 80 default_server;\n")
	b.WriteString("        server_name _;\n")
	b.WriteString("        return 404;\n")
	b.WriteString("    }\n")

	// 未知 SNI 的 443 兜底(需任一证书);无任何证书时不监听 443。
	var https []*httpGroup
	for _, g := range groups {
		if g.certd {
			https = append(https, g)
		}
	}
	if len(https) > 0 {
		certDir := nginxCertsDir + "/" + https[0].dom.BaseDomain
		b.WriteString("\n    server {\n")
		b.WriteString("        listen 443 ssl default_server;\n")
		b.WriteString("        server_name _;\n")
		b.WriteString("        ssl_certificate " + certDir + "/fullchain.pem;\n")
		b.WriteString("        ssl_certificate_key " + certDir + "/privkey.pem;\n")
		b.WriteString("        ssl_protocols TLSv1.2 TLSv1.3;\n")
		b.WriteString("        return 444;\n")
		b.WriteString("    }\n")
	}
	b.WriteString("}\n")

	if len(tcpFiles) > 0 {
		b.WriteString("\nstream {\n")
		b.WriteString("    resolver 127.0.0.11 valid=10s;\n\n")
		b.WriteString("    # 各注册服务的 tcp 透传配置(每服务一个文件)。\n")
		for _, f := range tcpFiles {
			b.WriteString("    include " + confDir + "/" + f.Name + ";\n")
		}
		b.WriteString("}\n")
	}
	return &nginxConfRender{Main: b.String(), Files: files}
}

// httpSite 是渲染期的一个 http 服务视图(附 FQDN / 所属基域 / 证书就绪位)。
type httpSite struct {
	svc   RegisteredService
	fqdn  string
	dom   Domain
	certd bool
}

// httpGroup 是同一 FQDN 下的全部 http 服务(同域名不同 path 共存,合并进同一 server 块)。
type httpGroup struct {
	fqdn  string
	dom   Domain
	certd bool
	sites []httpSite // 按 path 升序(确定性;nginx 前缀 location 最长匹配与顺序无关)
}

// siteRender 是单站点(单 location)的渲染决策,由 renderNginxConf 按 (FQDN, path) 升序
// 预计算(含确定性分配的 upstream/变量名)。
type siteRender struct {
	useUpstream bool // 走实例池/转发 upstream 块(proxy_pass 指向集群成员或对端网关)
	maintenance bool // 声明了实例池但无可渲染成员(全摘除/不可解析)→ 503 维护态
	upsName     string
	varName     string // 不含 $ 前缀(address 单上游变量)
	// plan 非 nil = 跨环境转发:成员/端口/Host 头/SNI 来自对端目录规划。
	plan *ForwardPlan
}

// renderHTTPServer 输出一个 FQDN 的 vhost server 块(组内每个服务一个 location,path 升序),
// 写进该站点的 conf.d/<fqdn>.conf 文件(indent 传文件内顶层缩进)。
func renderHTTPServer(b *strings.Builder, indent string, group *httpGroup, proto string, renders map[string]*siteRender) {
	if group.certd {
		certDir := nginxCertsDir + "/" + group.dom.BaseDomain
		b.WriteString(indent + "# " + groupTargetComment(group) + "。\n")
		b.WriteString(indent + "server {\n")
		b.WriteString(indent + "    listen 443 ssl;\n")
		b.WriteString(indent + "    server_name " + group.fqdn + ";\n")
		b.WriteString(indent + "    ssl_certificate " + certDir + "/fullchain.pem;\n")
		b.WriteString(indent + "    ssl_certificate_key " + certDir + "/privkey.pem;\n")
		b.WriteString(indent + "    ssl_protocols TLSv1.2 TLSv1.3;\n")
	} else {
		b.WriteString(indent + "# " + groupTargetComment(group) + "(基域未上传证书,仅 HTTP)。\n")
		b.WriteString(indent + "server {\n")
		b.WriteString(indent + "    listen 80;\n")
		b.WriteString(indent + "    server_name " + group.fqdn + ";\n")
	}
	for _, st := range group.sites {
		if st.svc.UpstreamKind == UpstreamKindStatic {
			writeStaticLocation(b, indent+"    ", st.svc)
			continue
		}
		writeProxyLocation(b, indent+"    ", st.svc, renders[st.svc.ID], proto)
	}
	b.WriteString(indent + "}\n\n")
}

// groupTargetComment 生成 server 块头部注释:单一反代服务保持旧格式「fqdn → 上游:端口」,
// 静态托管 / 跨环境转发 / 多服务(同域名多 path)逐项列出「path → 目标」。
func groupTargetComment(group *httpGroup) string {
	targetOf := func(st httpSite) string {
		switch {
		case st.svc.UpstreamKind == UpstreamKindStatic:
			return "静态托管"
		case st.svc.UpstreamKind == UpstreamKindForward:
			return "跨环境转发 " + st.svc.PeerNodeID
		default:
			return upstreamTarget(st.svc)
		}
	}
	if len(group.sites) == 1 && group.sites[0].svc.UpstreamKind == UpstreamKindContainer {
		return group.fqdn + " → " + upstreamTarget(group.sites[0].svc)
	}
	if len(group.sites) == 1 && group.sites[0].svc.UpstreamKind == UpstreamKindAddress {
		return group.fqdn + " → " + upstreamTarget(group.sites[0].svc)
	}
	parts := make([]string, 0, len(group.sites))
	for _, st := range group.sites {
		parts = append(parts, st.svc.DisplayPath()+" → "+targetOf(st))
	}
	return group.fqdn + " → " + strings.Join(parts, " · ")
}

// writeProxyLocation 输出反代 location 块(location 路径 = 服务 path,根路径为 /):
//   - useUpstream(有集群实例成员/转发规划):proxy_pass [http|https]://pw_<name>[_<path>];
//   - maintenance(声明实例池但无成员/转发不可用):return 503 维护态;
//   - 其余(address 单上游):set 变量 + proxy_pass http://$var(resolver 运行期解析)。
//
// 跨环境转发(plan 非 nil):Host 头改写为对端 FQDN(对端按 vhost 路由);对端有证书
// (Secure)时打 https + SNI(对端 80 是 301 跳转,不能走),verify off(mesh 链路自身
// WireGuard 加密 + 节点签名授权,证书校验交由对端自己的网关)。
//
// StripPathPrefix(仅 path 非根时生效):实例池用 proxy_pass 尾斜杠(URI "/",nginx 以之
// 替换匹配的 location 前缀);address 变量反代的 proxy_pass 不允许 URI,改用 rewrite 剥前缀。
//
// 反代头:proxy_http_version 1.1 + Upgrade $http_upgrade。Connection 分两种:
//
//	实例池/转发 → $connection_upgrade(非 WS 为空 → 不发送,使 upstream keepalive 复用连接);
//	address 单上游 → $connection_upgrade_addr(非 WS 为 close → 不复用即显式关闭)。
func writeProxyLocation(b *strings.Builder, indent string, svc RegisteredService, r *siteRender, proto string) {
	loc := svc.DisplayPath()
	if r.maintenance && !r.useUpstream {
		// 实例全部摘除/地址不可解析/转发目录不可达:维护态,不做反代。
		b.WriteString(indent + "location " + loc + " {\n")
		b.WriteString(indent + "    return 503;\n")
		b.WriteString(indent + "}\n")
		return
	}
	strip := svc.StripPathPrefix && svc.Path != ""
	b.WriteString(indent + "location " + loc + " {\n")
	if strip && !r.useUpstream {
		b.WriteString(indent + "    rewrite ^" + escapeLocationRegex(svc.Path) + "(/.*)?$ /$1 break;\n")
	}
	if r.useUpstream {
		scheme := "http"
		if r.plan != nil && r.plan.Secure {
			scheme = "https"
		}
		pass := scheme + "://" + r.upsName
		if strip {
			pass += "/"
		}
		b.WriteString(indent + "    proxy_pass " + pass + ";\n")
	} else {
		b.WriteString(indent + "    set $" + r.varName + " " + upstreamTarget(svc) + ";\n")
		b.WriteString(indent + "    proxy_pass http://$" + r.varName + ";\n")
	}
	b.WriteString(indent + "    proxy_http_version 1.1;\n")
	if r.plan != nil {
		// 对端按 vhost(Host 头)路由;SNI/证书名用对端 FQDN。
		b.WriteString(indent + "    proxy_set_header Host " + r.plan.HostHeader + ";\n")
		if r.plan.Secure {
			b.WriteString(indent + "    proxy_ssl_server_name on;\n")
			b.WriteString(indent + "    proxy_ssl_name " + r.plan.HostHeader + ";\n")
			b.WriteString(indent + "    proxy_ssl_verify off;\n")
		}
	} else {
		b.WriteString(indent + "    proxy_set_header Host $host;\n")
	}
	b.WriteString(indent + "    proxy_set_header X-Real-IP $remote_addr;\n")
	b.WriteString(indent + "    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n")
	b.WriteString(indent + "    proxy_set_header X-Forwarded-Proto " + proto + ";\n")
	b.WriteString(indent + "    proxy_set_header Upgrade $http_upgrade;\n")
	// 实例池/转发:非 WS 清空 Connection(upstream keepalive 复用);address 单上游:非 WS 显式 close。
	if r.useUpstream {
		b.WriteString(indent + "    proxy_set_header Connection $connection_upgrade;\n")
	} else {
		b.WriteString(indent + "    proxy_set_header Connection $connection_upgrade_addr;\n")
	}
	b.WriteString(indent + "}\n")
}

// writeStaticLocation 输出静态托管 location:alias 指向网关容器内该服务的发布目录
// (<nginxStaticDir>/<服务ID>/current/,由「网关静态发布」部署节点落盘,current 软链
// 原子切换,nginx 按请求读盘即生效,无需 reload);try_files 兜底回退本站点 index.html
// (SPA 路由;末参是 URI,内部重定向回本 location 由 alias 命中文件)。发布目录不存在
// (尚未首发)时请求 404。
func writeStaticLocation(b *strings.Builder, indent string, svc RegisteredService) {
	loc := svc.DisplayPath()
	fallback := "/index.html"
	if loc != "/" {
		fallback = loc + "/index.html"
	}
	b.WriteString(indent + "location " + loc + " {\n")
	b.WriteString(indent + "    alias " + nginxStaticDir + "/" + svc.ID + "/current/;\n")
	b.WriteString(indent + "    index index.html;\n")
	b.WriteString(indent + "    try_files $uri " + fallback + ";\n")
	b.WriteString(indent + "}\n")
}

// escapeLocationRegex 把已白名单校验的 path 转为 rewrite 模式的字面量(仅 "." 等
// 正则元字符需转义;字符集经 servicePathRe 白名单,无注入面)。
func escapeLocationRegex(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '/', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('\\')
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ngNameAlloc 按确定性顺序分配 nginx upstream/变量名:基础名重复时追加 _2/_3 去重
// (跨基域同名服务等场景;分配顺序由 (FQDN, path) 升序保证稳定)。
type ngNameAlloc struct {
	used map[string]int
}

func newNameAlloc() *ngNameAlloc { return &ngNameAlloc{used: make(map[string]int)} }

func (a *ngNameAlloc) alloc(base string) string {
	n := a.used[base]
	a.used[base] = n + 1
	if n == 0 {
		return base
	}
	return base + "_" + strconv.Itoa(n+1)
}

// nginxNameID 把「服务名 + 子路径」净化为 nginx upstream/变量名标识片段(保留
// [A-Za-z0-9_],其余字符映射为 _;如 web + /api → web_api,a-b + /api/v1 → a_b_api_v1)。
// path 的前导 / 由分隔符 _ 承担(不再叠加,免出双下划线);不同输入净化后可能撞名
// (如 /api/v1 与 /api_v1),由 ngNameAlloc 兜底去重。
func nginxNameID(name, path string) string {
	var b strings.Builder
	writeSanitized(&b, name)
	if path == "" {
		return b.String()
	}
	b.WriteByte('_')
	for i, r := range path {
		if i == 0 && r == '/' {
			continue // 前导 / 由上面的分隔符 _ 承担
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default: // '/' '-' '.' '~' 等 → _
			b.WriteByte('_')
		}
	}
	return b.String()
}

func writeSanitized(b *strings.Builder, s string) {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default: // '-' '/' '.' '~' 等 → _
			b.WriteByte('_')
		}
	}
}

// upstreamTarget 返回「上游:端口」渲染串(container=容器名,由 resolver 动态解析;address=HOST)。
// net.JoinHostPort:IPv6 上游地址自动加方括号。
func upstreamTarget(svc RegisteredService) string {
	return net.JoinHostPort(svc.Upstream, strconv.Itoa(svc.UpstreamPort))
}

// nginxVarName 返回该服务的 nginx 变量名($up_<name>,`-` 归一为 `_`;服务名字符集无 `_`,映射无碰撞)。
func nginxVarName(name string) string {
	return "$up_" + strings.ReplaceAll(name, "-", "_")
}

// ---------- 容器生命周期(照抄 ensureCaddy 手法) ----------

// ensureNginx 幂等地在网关主机上保证 nginx 容器就绪:建共享网络 + 起容器(若缺)。
// tcpPorts 是全部 enabled tcp 服务声明的监听端口:必须由容器对宿主发布(-p)才可达;Docker 端口
// 创建时固定,故既有容器缺端口(或 80/443 宿主端口设置变更)时,带「既有映射 ∪ 期望映射」重建。
// 另:容器自举脚本烧死在 docker run 参数里,版本标签与当前不符(旧版创建)时也重建,让新
// 自举脚本生效(配置/证书在具名卷,重建不丢;重建后 applyCaddyfile→applyNginxConf 同轮覆盖)。
// 镜像引用漂移(设置/环境变量换了镜像)同样重建:容器记录的 .Config.Image 与当前期望引用不符
// 时带新引用重建(run 内 best-effort pull 取新);同名可变 tag 的上游更新走 UpgradeGatewayImage
// (pull 后按镜像 ID 比对决定重建)。
func ensureNginx(ctx context.Context, tg target.Service, st *Settings, serverID string, tcpPorts []int) error {
	// 1) 共享网络(存在即跳过)。
	netInsp, err := tg.Exec(ctx, serverID, []string{"docker", "network", "inspect", st.Network})
	if err != nil {
		return err
	}
	if netInsp.ExitCode != 0 {
		res, cerr := tg.Exec(ctx, serverID, []string{"docker", "network", "create", st.Network})
		if cerr != nil {
			return cerr
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("%w:%s", ErrNginxStart, strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout)))
		}
	}

	want := desiredPortMap(st, tcpPorts)

	// 2) 容器存在:校验端口映射/自举版本/镜像引用是否与期望一致;任一不符 → 重建。
	insp, err := tg.Exec(ctx, serverID, []string{"docker", "inspect", st.ContainerName})
	if err != nil {
		return err
	}
	if insp.ExitCode == 0 {
		cur, perr := inspectPortMap(ctx, tg, st, serverID)
		if perr != nil {
			return perr
		}
		ver, verr := inspectBootstrapVer(ctx, tg, st, serverID)
		if verr != nil {
			return verr
		}
		img, ierr := inspectContainerImageRef(ctx, tg, st, serverID)
		if ierr != nil {
			return ierr
		}
		if portsMatch(cur, want) && ver == nginxBootstrapVer && img == nginxImageRef(st) {
			return nil
		}
		if rmRes, rmErr := tg.Exec(ctx, serverID, []string{"docker", "rm", "-f", st.ContainerName}); rmErr != nil {
			return rmErr
		} else if rmRes.ExitCode != 0 {
			return fmt.Errorf("%w:%s", ErrNginxStart, strings.TrimSpace(firstNonEmpty(rmRes.Stderr, rmRes.Stdout)))
		}
		return runNginxContainer(ctx, tg, st, serverID, mergePortMap(cur, want))
	}

	// 3) 全新部署:先探测宿主端口占用(避免与既有进程抢端口),冲突不强起。
	if busy, detail := hostPortsBusy(ctx, tg, st, serverID); busy {
		return fmt.Errorf("%w%s", ErrPortConflict, detail)
	}
	return runNginxContainer(ctx, tg, st, serverID, want)
}

// desiredPortMap 返回期望的「容器端口 → 宿主端口」映射:80→HTTPPort、443→HTTPSPort + TCP p→p。
func desiredPortMap(st *Settings, tcpPorts []int) map[int]int {
	m := map[int]int{80: st.HTTPPort, 443: st.HTTPSPort}
	for _, p := range tcpPorts {
		m[p] = p
	}
	return m
}

// runNginxContainer 用给定「容器端口 → 宿主端口」映射起网关容器(array 不拼 shell)。
// --add-host host.docker.internal:host-gateway 让 address 类上游能反代到宿主机服务。
func runNginxContainer(ctx context.Context, tg target.Service, st *Settings, serverID string, ports map[int]int) error {
	image := nginxImageRef(st)
	// best-effort pull(离线/已缓存场景交由 docker run 决断)。
	if _, perr := tg.Exec(ctx, serverID, []string{"docker", "pull", image}); perr != nil {
		return perr
	}
	runCmd := []string{
		"docker", "run", "-d",
		"--name", st.ContainerName,
		"--restart", "unless-stopped",
		"--network", st.Network,
		"--add-host", "host.docker.internal:host-gateway",
		"--label", nginxBootstrapLabel + "=" + nginxBootstrapVer,
	}
	// 端口按容器端口升序发布(渲染确定,便于诊断)。
	cports := make([]int, 0, len(ports))
	for cp := range ports {
		cports = append(cports, cp)
	}
	sort.Ints(cports)
	for _, cp := range cports {
		runCmd = append(runCmd, "-p", strconv.Itoa(ports[cp])+":"+strconv.Itoa(cp))
	}
	runCmd = append(runCmd,
		"-v", st.VolumeName+":/etc/pipewright",
		// 静态托管:各网关主机 StaticRoot(同一路径约定)只读挂进容器,nginx alias 直接
		// 服务 <serviceID>/current/(发布侧软链切换,免 reload;宿主目录缺失时 Docker 自动建)。
		"-v", st.StaticRoot+":"+nginxStaticDir+":ro",
		image,
		"sh", "-c", nginxBootstrapScript,
	)
	res, err := tg.Exec(ctx, serverID, runCmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%w:%s", ErrNginxStart, strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout)))
	}
	return nil
}

// inspectPortMap 读网关容器已发布的「容器端口 → 宿主端口」(仅 tcp)。无容器/解析失败 → 空 map。
func inspectPortMap(ctx context.Context, tg target.Service, st *Settings, serverID string) (map[int]int, error) {
	res, err := tg.Exec(ctx, serverID, []string{
		"docker", "inspect", st.ContainerName,
		"--format", "{{range $p, $c := .HostConfig.PortBindings}}{{$p}}={{(index $c 0).HostPort}} {{end}}",
	})
	if err != nil {
		return nil, err
	}
	m := map[int]int{}
	for _, tok := range strings.Fields(res.Stdout) {
		eq := strings.IndexByte(tok, '=')
		if eq <= 0 {
			continue
		}
		proto := tok[:eq] // 形如 "80/tcp"
		if !strings.HasSuffix(proto, "/tcp") {
			continue
		}
		cp, e1 := strconv.Atoi(strings.TrimSuffix(proto, "/tcp"))
		hp, e2 := strconv.Atoi(tok[eq+1:])
		if e1 == nil && e2 == nil {
			m[cp] = hp
		}
	}
	return m, nil
}

// inspectBootstrapVer 读网关容器的自举脚本版本标签(旧版创建的容器无标签 → "")。
func inspectBootstrapVer(ctx context.Context, tg target.Service, st *Settings, serverID string) (string, error) {
	res, err := tg.Exec(ctx, serverID, []string{
		"docker", "inspect", st.ContainerName,
		"--format", `{{index .Config.Labels "` + nginxBootstrapLabel + `"}}`,
	})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("%w:%s", ErrNginxStart, strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout)))
	}
	return strings.TrimSpace(res.Stdout), nil
}

// inspectContainerImageRef 读网关容器创建时的镜像引用(.Config.Image;供镜像漂移重建判定)。
func inspectContainerImageRef(ctx context.Context, tg target.Service, st *Settings, serverID string) (string, error) {
	res, err := tg.Exec(ctx, serverID, []string{
		"docker", "inspect", st.ContainerName,
		"--format", "{{.Config.Image}}",
	})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("%w:%s", ErrNginxStart, strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout)))
	}
	return strings.TrimSpace(res.Stdout), nil
}

// portsMatch 报告既有映射是否与期望完全一致(容器端口集合 + 各自宿主端口)。
func portsMatch(cur, want map[int]int) bool {
	if len(cur) != len(want) {
		return false
	}
	for cp, hp := range want {
		if cur[cp] != hp {
			return false
		}
	}
	return true
}

// mergePortMap 期望映射优先,叠加既有映射中期望未覆盖的容器端口(保守保留用户既有发布)。
func mergePortMap(cur, want map[int]int) map[int]int {
	out := make(map[int]int, len(cur)+len(want))
	for cp, hp := range cur {
		out[cp] = hp
	}
	for cp, hp := range want {
		out[cp] = hp
	}
	return out
}

// nginxContainerStatus 是网关容器探测结果(GatewayStatus 的容器部分)。
type nginxContainerStatus struct {
	Installed bool
	Running   bool
	Image     string
	Ports     string
	Version   string
}

// inspectNginx 经 docker inspect 探测网关容器:不存在 → Installed:false(非错误);
// 仅 target 层传输错误才返回 error。端口摘要 best-effort(失败给 "")。
func inspectNginx(ctx context.Context, tg target.Service, st *Settings, serverID string) (*nginxContainerStatus, error) {
	out := &nginxContainerStatus{}
	insp, err := tg.Exec(ctx, serverID, []string{
		"docker", "inspect", st.ContainerName,
		"--format", "{{.State.Running}}|{{.Config.Image}}",
	})
	if err != nil {
		return nil, err
	}
	if insp.ExitCode != 0 {
		return out, nil
	}
	out.Installed = true
	line := strings.TrimSpace(insp.Stdout)
	if i := strings.IndexByte(line, '|'); i >= 0 {
		out.Running = strings.EqualFold(strings.TrimSpace(line[:i]), "true")
		out.Image = strings.TrimSpace(line[i+1:])
	}
	out.Ports = inspectNginxPorts(ctx, tg, st, serverID)
	out.Version = inspectNginxVersion(ctx, tg, st, serverID)
	return out, nil
}

// inspectNginxVersion best-effort 读容器内 nginx 二进制版本(如 "1.27.4";失败给 "")。
// nginx -v 把版本打到 stderr;容器未运行/无 nginx 二进制时 exec 非零,静默降级。
func inspectNginxVersion(ctx context.Context, tg target.Service, st *Settings, serverID string) string {
	res, err := tg.Exec(ctx, serverID, []string{"docker", "exec", st.ContainerName, "nginx", "-v"})
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	if m := nginxVersionRe.FindStringSubmatch(res.Stderr + res.Stdout); m != nil {
		return m[1]
	}
	return ""
}

// inspectNginxPorts best-effort 读网关容器发布端口摘要(如 "80,443,3306")。
func inspectNginxPorts(ctx context.Context, tg target.Service, st *Settings, serverID string) string {
	res, err := tg.Exec(ctx, serverID, []string{
		"docker", "inspect", st.ContainerName,
		"--format", "{{json .NetworkSettings.Ports}}",
	})
	if err != nil || res == nil || res.ExitCode != 0 {
		return ""
	}
	var ports map[string]any
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &ports); jerr != nil {
		return ""
	}
	seen := map[string]struct{}{}
	nums := make([]int, 0, len(ports))
	for k := range ports {
		p := k
		if i := strings.IndexByte(p, '/'); i >= 0 {
			p = p[:i]
		}
		n, cerr := strconv.Atoi(p)
		if cerr != nil {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		nums = append(nums, n)
	}
	if len(nums) == 0 {
		return ""
	}
	sort.Ints(nums)
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

// NginxConfFile 是网关容器内的一份配置文件内容(主配置或其引用文件)。
type NginxConfFile struct {
	Path    string `json:"path"` // 容器内绝对路径
	Content string `json:"content"`
	Error   string `json:"error,omitempty"` // 单文件读取失败原因(best-effort,不阻断其余文件)
}

// nginxConfRefRe 从 nginx.conf 文本提取 include 引用的文件路径。含 glob 通配符的
// include(如 conf.d/*.conf)无法 cat,由调用方跳过。ssl_certificate / ssl_certificate_key
// 不提取:证书(尤其私钥)内容不该经查看配置接口下发到界面。
var nginxConfRefRe = regexp.MustCompile(`(?m)\binclude\s+([^;]+);`)

// stripNginxComments 逐行去掉 # 注释(nginx 中 # 起至行尾均为注释),防注释里的指令样
// 文本(如头部说明「由下方 include 引用」)被当成真实引用提取。
func stripNginxComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// nginxConfRefs 提取主配置里 include 的非通配文件路径(去注释后匹配,保持出现顺序)。
func nginxConfRefs(main string) []string {
	var refs []string
	for _, m := range nginxConfRefRe.FindAllStringSubmatch(stripNginxComments(main), -1) {
		p := strings.TrimSpace(m[1])
		if p == "" || strings.ContainsAny(p, "*?[") {
			continue
		}
		refs = append(refs, p)
	}
	return refs
}

// readNginxConfFiles 读取网关容器内实际生效的 nginx.conf 及其引用文件。主配置读取失败
// (容器未部署/配置缺失)整体报错;引用文件逐个 best-effort,单个失败只记 Error 字段。
func readNginxConfFiles(ctx context.Context, tg target.Service, st *Settings, serverID string) ([]NginxConfFile, error) {
	main, err := execCatFile(ctx, tg, st, serverID, nginxConfPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrConfRead, err)
	}
	files := []NginxConfFile{{Path: nginxConfPath, Content: main}}
	seen := map[string]struct{}{nginxConfPath: {}}
	for _, p := range nginxConfRefs(main) {
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		f := NginxConfFile{Path: p}
		if content, cerr := execCatFile(ctx, tg, st, serverID, p); cerr != nil {
			f.Error = cerr.Error()
		} else {
			f.Content = content
		}
		files = append(files, f)
	}
	return files, nil
}

// execCatFile 经 docker exec cat 读容器内单个文件。传输失败或 cat 非零(不存在/权限)都转为人读错误。
func execCatFile(ctx context.Context, tg target.Service, st *Settings, serverID, path string) (string, error) {
	res, err := tg.Exec(ctx, serverID, []string{"docker", "exec", st.ContainerName, "cat", path})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout))
		if msg == "" {
			msg = "cat 退出码 " + strconv.Itoa(res.ExitCode)
		}
		return "", fmt.Errorf("%s", msg)
	}
	return res.Stdout, nil
}

// removeNginx 停止并删除网关容器(保留具名卷:证书/配置持久)。幂等:容器已不存在视为成功。
func removeNginx(ctx context.Context, tg target.Service, st *Settings, serverID string) error {
	if _, err := tg.Exec(ctx, serverID, []string{"docker", "stop", st.ContainerName}); err != nil {
		return err
	}
	if _, err := tg.Exec(ctx, serverID, []string{"docker", "rm", st.ContainerName}); err != nil {
		return err
	}
	return nil
}

// hostPortsBusy 探测网关主机 HTTP/HTTPS 宿主端口是否被占用。best-effort:探测失败不阻断。
func hostPortsBusy(ctx context.Context, tg target.Service, st *Settings, serverID string) (bool, string) {
	res, err := tg.Exec(ctx, serverID, []string{"ss", "-ltn"})
	if err != nil || res == nil || res.ExitCode != 0 {
		return false, ""
	}
	var hit []string
	for _, p := range []int{st.HTTPPort, st.HTTPSPort} {
		if strings.Contains(res.Stdout, ":"+strconv.Itoa(p)+" ") {
			hit = append(hit, strconv.Itoa(p))
		}
	}
	if len(hit) == 0 {
		return false, ""
	}
	return true, "(" + strings.Join(hit, "、") + " 已被占用)"
}

// connectUpstream 把上游容器接入共享网络,使 nginx 能按容器名解析它(幂等容忍 already)。
func connectUpstream(ctx context.Context, tg target.Service, st *Settings, serverID, container string) error {
	res, err := tg.Exec(ctx, serverID, []string{"docker", "network", "connect", st.Network, container})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		msg := strings.ToLower(firstNonEmpty(res.Stderr, res.Stdout))
		if strings.Contains(msg, "already") {
			return nil
		}
		return fmt.Errorf("%w:%s", ErrUpstreamConnect, strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout)))
	}
	return nil
}

// ---------- 证书下发 ----------

// deployCerts 把全部基域证书解密并落进网关容器卷(每轮 apply 全量重放,幂等;文件小代价可忽略)。
// 私钥落 0600。解密仅在进程内进行,绝不日志/回库。
func (s *service) deployCerts(ctx context.Context, st *Settings, serverID string, domains []Domain) error {
	for _, d := range domains {
		if !d.HasCert {
			continue
		}
		sealedCert, sealedKey, ok, err := s.store.getCertSealed(ctx, d.ID)
		if err != nil || !ok {
			continue
		}
		certPEM, uerr := s.vault.OpenSecret(sealedCert)
		if uerr != nil {
			return uerr
		}
		keyPEM, uerr := s.vault.OpenSecret(sealedKey)
		if uerr != nil {
			return uerr
		}
		dir := nginxCertsDir + "/" + d.BaseDomain
		if _, err := s.tg.Exec(ctx, serverID, []string{"docker", "exec", st.ContainerName, "mkdir", "-p", dir}); err != nil {
			return err
		}
		if err := s.cpToContainer(ctx, st, serverID, certPEM, d.BaseDomain+"-fullchain.pem", dir+"/fullchain.pem"); err != nil {
			return err
		}
		if err := s.cpToContainer(ctx, st, serverID, keyPEM, d.BaseDomain+"-privkey.pem", dir+"/privkey.pem"); err != nil {
			return err
		}
		// 私钥收紧权限(best-effort)。
		_, _ = s.tg.Exec(ctx, serverID, []string{"docker", "exec", st.ContainerName, "chmod", "600", dir + "/privkey.pem"})
	}
	return nil
}

// cpToContainer:Upload 到宿主临时路径 → docker cp 进容器目标路径(与 applyCaddyfile 同手法)。
func (s *service) cpToContainer(ctx context.Context, st *Settings, serverID string, content []byte, tmpName, containerPath string) error {
	tmp := certTmpPrefix + tmpName
	if err := s.tg.Upload(ctx, serverID, bytes.NewReader(content), tmp); err != nil {
		return err
	}
	res, err := s.tg.Exec(ctx, serverID, []string{"docker", "cp", tmp, st.ContainerName + ":" + containerPath})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%w:%s", ErrApply, strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout)))
	}
	return nil
}

// ---------- 配置下发 ----------

// applyNginxConf 把渲染产物(主配置 + conf.d/ 每服务文件)下发并热加载:
//
//	宿主与容器各建中转目录 /tmp/pipewright-nginx-stage(先测后换的沙箱)
//	→ 全部产物 Upload 到宿主中转、docker cp 进容器中转(live 正式主配置 + test 沙箱
//	  主配置两份:test 的 include 指向中转 conf.d,使 nginx -t 校验的就是**本轮新文件**)
//	→ docker exec nginx -t -c <中转>/nginx.test.conf(校验失败不动活配置)
//	→ 全量替换卷内 conf.d(先清后写,防已删服务残留旧文件被误加载)→ 主配置覆盖
//	→ docker exec nginx -s reload(master 优雅热加载)→ best-effort 清理中转。
//
// live / test 是同一输入的两次渲染,仅 include 目录不同(live 用 nginxConfDir,test 用
// 中转 conf.d);Files 内容完全一致,只需落盘一份。
func applyNginxConf(ctx context.Context, tg target.Service, st *Settings, serverID string, live, test *nginxConfRender) error {
	uploads := []struct{ path, content string }{
		{nginxStageDir + "/nginx.conf", live.Main},
		{nginxStageDir + "/nginx.test.conf", test.Main},
	}
	for _, f := range live.Files {
		uploads = append(uploads, struct{ path, content string }{nginxStageDir + "/conf.d/" + f.Name, f.Content})
	}

	// 0) 宿主中转目录重建(Upload 不建目录,须显式 mkdir)。
	if err := execOK(ctx, tg, serverID, []string{"rm", "-rf", nginxStageDir}); err != nil {
		return err
	}
	if err := execOK(ctx, tg, serverID, []string{"mkdir", "-p", nginxStageDir + "/conf.d"}); err != nil {
		return err
	}
	for _, u := range uploads {
		if err := tg.Upload(ctx, serverID, strings.NewReader(u.content), u.path); err != nil {
			return err
		}
	}

	// 1) 容器中转目录重建 + 全部产物拷入(容器看不到宿主文件系统,必须先拷入)。
	if err := execOK(ctx, tg, serverID, []string{"docker", "exec", st.ContainerName, "rm", "-rf", nginxStageDir}); err != nil {
		return err
	}
	if err := execOK(ctx, tg, serverID, []string{"docker", "exec", st.ContainerName, "mkdir", "-p", nginxStageDir + "/conf.d"}); err != nil {
		return err
	}
	for _, u := range uploads {
		if err := dockerCpOK(ctx, tg, serverID, u.path, st.ContainerName+":"+u.path); err != nil {
			return err
		}
	}

	// 2) 先测后换:校验沙箱副本(失败即回,活配置未动)。
	val, err := tg.Exec(ctx, serverID, []string{"docker", "exec", st.ContainerName, "nginx", "-t", "-c", nginxStageDir + "/nginx.test.conf"})
	if err != nil {
		return err
	}
	if val.ExitCode != 0 {
		return fmt.Errorf("%w: nginx -t 校验失败:%s", ErrApply, strings.TrimSpace(firstNonEmpty(val.Stderr, val.Stdout)))
	}

	// 3) 全量替换正式 conf.d(先清后建:已删服务的旧文件不再残留)→ 逐文件落位。
	if err := execOK(ctx, tg, serverID, []string{"docker", "exec", st.ContainerName, "rm", "-rf", nginxConfDir}); err != nil {
		return err
	}
	if err := execOK(ctx, tg, serverID, []string{"docker", "exec", st.ContainerName, "mkdir", "-p", nginxConfDir}); err != nil {
		return err
	}
	for _, f := range live.Files {
		if err := dockerCpOK(ctx, tg, serverID, nginxStageDir+"/conf.d/"+f.Name, st.ContainerName+":"+nginxConfDir+"/"+f.Name); err != nil {
			return err
		}
	}
	// 4) 主配置覆盖到卷内正式路径并热加载(最后换主配置:conf.d 先行就位)。
	if err := dockerCpOK(ctx, tg, serverID, nginxStageDir+"/nginx.conf", st.ContainerName+":"+nginxConfPath); err != nil {
		return err
	}
	rl, err := tg.Exec(ctx, serverID, []string{"docker", "exec", st.ContainerName, "nginx", "-s", "reload"})
	if err != nil {
		return err
	}
	if rl.ExitCode != 0 {
		return fmt.Errorf("%w:%s", ErrApply, strings.TrimSpace(firstNonEmpty(rl.Stderr, rl.Stdout)))
	}

	// 5) best-effort 清理中转(失败不影响交付结果)。
	_, _ = tg.Exec(ctx, serverID, []string{"docker", "exec", st.ContainerName, "rm", "-rf", nginxStageDir})
	_, _ = tg.Exec(ctx, serverID, []string{"rm", "-rf", nginxStageDir})
	return nil
}

// execOK 执行远端命令,非零退出码转人话错误(ErrApply)。
func execOK(ctx context.Context, tg target.Service, serverID string, cmd []string) error {
	res, err := tg.Exec(ctx, serverID, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%w:%s", ErrApply, strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout)))
	}
	return nil
}

// dockerCpOK 把宿主文件拷进容器(路径均为平台自管理常量,无用户输入)。
func dockerCpOK(ctx context.Context, tg target.Service, serverID, hostPath, containerPath string) error {
	res, err := tg.Exec(ctx, serverID, []string{"docker", "cp", hostPath, containerPath})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%w:%s", ErrApply, strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout)))
	}
	return nil
}

// nginxImageRef 返回网关镜像:设置里的 image 优先,PIPEWRIGHT_NGINX_IMAGE 次之,默认 stable-alpine。
// 全部经字符集白名单校验(防注入 docker flag)。
func nginxImageRef(st *Settings) string {
	if st.Image != "" && nginxImageRefRe.MatchString(st.Image) {
		return st.Image
	}
	if v := strings.TrimSpace(os.Getenv(nginxImageEnv)); v != "" && nginxImageRefRe.MatchString(v) {
		return v
	}
	return defaultNginxImage
}

// firstNonEmpty 返回首个非空白字符串。
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
