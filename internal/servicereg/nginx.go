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
	// nginxConfPath 是容器内托管配置路径(挂具名卷,容器重建不丢)。
	nginxConfPath = "/etc/pipewright/nginx.conf"
	// nginxCertsDir 是容器内证书目录:certs/<base_domain>/{fullchain,privkey}.pem。
	nginxCertsDir = "/etc/pipewright/certs"
	// nginxConfTmpPath / certTmpPrefix 是宿主临时落盘路径(Upload 后 docker cp)。
	nginxConfTmpPath = "/tmp/pipewright-nginx.conf"
	certTmpPrefix    = "/tmp/pipewright-sr-cert-"
	// nginxImageEnv 允许经环境变量覆盖镜像(默认 nginx:stable-alpine,官方镜像自带
	// stream 动态模块 + docker 内嵌 DNS 可用)。
	nginxImageEnv = "PIPEWRIGHT_NGINX_IMAGE"
	// streamModulePath 是官方 nginx 镜像的 stream 动态模块路径(TCP 反代需要)。
	streamModulePath = "/usr/lib/nginx/modules/ngx_stream_module.so"
	// nginxBootstrapLabel/Ver 是容器自举脚本版本标签:入口脚本在 docker run 时烧进容器,
	// 平台升级改了脚本不会作用于既有容器,故打版本标签,ensureNginx 见版本不符即重建容器
	// (配置/证书在具名卷,重建不丢;重建后同轮 apply 会覆盖真实配置)。
	nginxBootstrapLabel = "pipewright.bootstrap"
	nginxBootstrapVer   = "2"
)

// nginxImageRefRe 防注入:镜像引用字符集(首字符字母数字,防经 env 注入 docker flag)。
var nginxImageRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)

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

// nginxConfRenderInput 是 renderNginxConf 的已就绪输入(领域对象直接进,纯函数无 I/O)。

// renderNginxConf 据基域(enabled 服务所属)与实例生成完整 nginx.conf(纯函数,可直接 golden 单测)。
//
// 结构(确定性输出:基域/服务/FQDN/实例容器名全部升序,渲染幂等):
//
//	load_module stream 模块          —— 仅当存在 tcp 服务
//	events / http
//	  resolver 127.0.0.11 valid=10s  —— Docker 内嵌 DNS(address 上游 / TCP 透传的运行期动态解析)
//	  upstream pw_<name> { server <实例容器>:<端口> max_fails=2 fail_timeout=10s; ... }
//	                                  —— http+container 服务:成员 = attached 实例;nginx 在 reload
//	                                     时解析成员名(平台每次变更/轮转都伴随 reload,解析始终新鲜);
//	                                     max_fails/fail_timeout 为被动健康兜底(实例无响应临时摘除)
//	  80:证书基域的 http 服务 → 301 跳 https;无证书基域的 http 服务 → 直接反代;default → 404
//	  443:证书基域的 http 服务逐个 server 块(泛域名证书按基域共用);default → 444
//	  http+container 服务实例全摘除 → 该 server 块 location 返回 503(维护态)
//	stream(tcp 服务):listen <端口> → proxy_pass 上游(变量 + resolver 动态解析)
//
// 所有进入文本的值已在领域层严格校验过(域名/服务名/上游/端口白名单),渲染处不再二次防注入。
// 无 enabled 服务时渲染最小合法配置(80 返回 404),保证网关容器始终可运行。
// renderNginxConf 渲染完整 nginx.conf(纯函数,确定性输出)。集群模型:upstream 成员 =
// 全部 attached 且地址可解析的实例(服务器地址:宿主端口)—— 各网关主机渲染**同一份**配置,
// 本机没有实例的服务同样代理远端。skipped 是被跳过的实例数(遗留行/地址解析失败),写进
// 头部注释便于诊断;故障实例交给 max_fails 熔断,不阻断渲染。
func renderNginxConf(domains []Domain, services []RegisteredService, instances []Instance, serverAddr map[string]string, skipped int) string {
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

	// 拆分:http 服务(附 FQDN 与基域)按 FQDN 升序;tcp 服务按监听端口升序。
	var https []httpSite  // cert 域的 http 服务(443 server 块)
	var plain []httpSite  // 无证书域的 http 服务(80 server 块)
	var redirects []string // cert 域 http 服务的 FQDN(80 → 443 跳转名单)
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
		site := httpSite{svc: svc, fqdn: svc.Name + "." + dom.BaseDomain, dom: dom, certd: dom.HasCert}
		if dom.HasCert {
			https = append(https, site)
			redirects = append(redirects, site.fqdn)
		} else {
			plain = append(plain, site)
		}
	}
	sort.Slice(https, func(i, j int) bool { return https[i].fqdn < https[j].fqdn })
	sort.Slice(plain, func(i, j int) bool { return plain[i].fqdn < plain[j].fqdn })
	sort.Strings(redirects)
	sort.Slice(tcps, func(i, j int) bool { return tcps[i].TCPListenPort < tcps[j].TCPListenPort })

	// 有可渲染成员的 http 服务统一走 upstream 块(容器与非容器实例一视同仁);
	// 声明了实例但无可渲染成员(全摘除/地址不可解析)→ 503 维护态;
	// 无实例声明的 address 服务 → 保留单上游变量 + resolver 动态解析(人工 address 模式)。
	needsUpstream := func(s httpSite) bool {
		return len(instByService[s.svc.ID]) > 0
	}
	maintenance := func(s httpSite) bool {
		// 容器服务恒为部署托管(无实例即维护态);address 服务声明了实例或上游为空
		// (部署托管待首批实例上线)时同样进入维护态;仅「无实例且配了静态上游」的
		// 人工 address 模式走变量 + resolver 动态解析。
		return !needsUpstream(s) && (s.svc.UpstreamKind == UpstreamKindContainer ||
			declaredByService[s.svc.ID] > 0 || s.svc.Upstream == "")
	}
	upstreamName := func(s httpSite) string { return "pw_" + nginxUpstreamID(s.svc.Name) }

	var b strings.Builder
	b.WriteString("# 由 Pipewright 自动生成,请勿手改。\n")
	if skipped > 0 {
		b.WriteString("# 注意:有 " + strconv.Itoa(skipped) + " 个实例因遗留数据(待部署认领)或服务器地址解析失败未渲染。\n")
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
	b.WriteString("    keepalive_timeout 65;\n\n")
	b.WriteString("    # Docker 内嵌 DNS:address 上游 / TCP 透传的运行期动态解析。\n")
	b.WriteString("    # (实例池成员为静态 IP:宿主端口,成员经 reload 原子增减。)\n")
	b.WriteString("    resolver 127.0.0.11 valid=10s ipv6=off;\n\n")
	b.WriteString("    map $http_upgrade $connection_upgrade {\n")
	b.WriteString("        default upgrade;\n")
	b.WriteString("        ''      close;\n")
	b.WriteString("    }\n\n")

	// upstream 块(全部有实例成员的 http 服务,plain/https 两类;按名升序输出)。
	var ups []httpSite
	ups = append(ups, plain...)
	ups = append(ups, https...)
	sort.Slice(ups, func(i, j int) bool { return ups[i].svc.Name < ups[j].svc.Name })
	for _, s := range ups {
		if !needsUpstream(s) {
			continue
		}
		b.WriteString("    # 服务 " + s.fqdn + " 的集群实例池(成员 = 服务器地址:宿主端口,经 reload 原子增减)。\n")
		b.WriteString("    upstream " + upstreamName(s) + " {\n")
		for _, inst := range instByService[s.svc.ID] {
			// net.JoinHostPort:IPv6 地址自动加方括号(裸 IPv6 冒号拼接会让 nginx -t 失败)。
			b.WriteString("        server " + net.JoinHostPort(serverAddr[inst.ServerID], strconv.Itoa(inst.EffectiveHostPort(&s.svc))) +
				" max_fails=2 fail_timeout=10s;\n")
		}
		b.WriteString("    }\n\n")
	}

	if len(redirects) > 0 {
		b.WriteString("    # 证书就绪的域名:80 → 443。\n")
		b.WriteString("    server {\n")
		b.WriteString("        listen 80;\n")
		b.WriteString("        server_name " + strings.Join(redirects, " ") + ";\n")
		b.WriteString("        return 301 https://$host$request_uri;\n")
		b.WriteString("    }\n\n")
	}
	for _, site := range plain {
		renderHTTPServer(&b, "    ", site, needsUpstream(site), maintenance(site), upstreamName(site), "http")
	}
	b.WriteString("    server {\n")
	b.WriteString("        listen 80 default_server;\n")
	b.WriteString("        server_name _;\n")
	b.WriteString("        return 404;\n")
	b.WriteString("    }\n\n")

	for _, site := range https {
		renderHTTPServer(&b, "    ", site, needsUpstream(site), maintenance(site), upstreamName(site), "https")
	}
	// 未知 SNI 的 443 兜底(需任一证书);无任何证书时不监听 443。
	if len(https) > 0 {
		certDir := nginxCertsDir + "/" + https[0].dom.BaseDomain
		b.WriteString("    server {\n")
		b.WriteString("        listen 443 ssl default_server;\n")
		b.WriteString("        server_name _;\n")
		b.WriteString("        ssl_certificate " + certDir + "/fullchain.pem;\n")
		b.WriteString("        ssl_certificate_key " + certDir + "/privkey.pem;\n")
		b.WriteString("        ssl_protocols TLSv1.2 TLSv1.3;\n")
		b.WriteString("        return 444;\n")
		b.WriteString("    }\n")
	}
	b.WriteString("}\n")

	if len(tcps) > 0 {
		b.WriteString("\nstream {\n")
		b.WriteString("    resolver 127.0.0.11 valid=10s;\n\n")
		for _, svc := range tcps {
			b.WriteString("    server {\n")
			b.WriteString("        listen " + strconv.Itoa(svc.TCPListenPort) + ";\n")
			b.WriteString("        set " + nginxVarName(svc.Name) + " " + upstreamTarget(svc) + ";\n")
			b.WriteString("        proxy_pass " + nginxVarName(svc.Name) + ";\n")
			b.WriteString("    }\n\n")
		}
		b.WriteString("}\n")
	}
	return b.String()
}

// httpSite 是渲染期的一个 http 服务视图(附 FQDN / 所属基域 / 证书就绪位)。
type httpSite struct {
	svc   RegisteredService
	fqdn  string
	dom   Domain
	certd bool
}

// renderHTTPServer 输出一个 http 服务的 server 块。useUpstream 表示走实例池 upstream 块
// (proxy_pass 指向集群成员);maintenance 表示声明了实例池但无可渲染成员(全摘除/地址不可
// 解析)→ 503 维护态;其余(无实例声明的 address 服务)走变量 + resolver 单上游动态解析。
func renderHTTPServer(b *strings.Builder, indent string, site httpSite, useUpstream, maintenance bool, upsName, proto string) {
	svc := site.svc
	if site.certd {
		certDir := nginxCertsDir + "/" + site.dom.BaseDomain
		b.WriteString(indent + "# " + site.fqdn + " → " + upstreamTarget(svc) + "。\n")
		b.WriteString(indent + "server {\n")
		b.WriteString(indent + "    listen 443 ssl;\n")
		b.WriteString(indent + "    server_name " + site.fqdn + ";\n")
		b.WriteString(indent + "    ssl_certificate " + certDir + "/fullchain.pem;\n")
		b.WriteString(indent + "    ssl_certificate_key " + certDir + "/privkey.pem;\n")
		b.WriteString(indent + "    ssl_protocols TLSv1.2 TLSv1.3;\n")
	} else {
		b.WriteString(indent + "# " + site.fqdn + " → " + upstreamTarget(svc) + "(基域未上传证书,仅 HTTP)。\n")
		b.WriteString(indent + "server {\n")
		b.WriteString(indent + "    listen 80;\n")
		b.WriteString(indent + "    server_name " + site.fqdn + ";\n")
	}
	writeProxyLocation(b, indent+"    ", svc, useUpstream, maintenance, upsName, proto)
	b.WriteString(indent + "}\n\n")
}

// writeProxyLocation 输出反代 location 块:
//   - useUpstream(有集群实例成员):proxy_pass http://pw_<name>(成员 = attached 实例);
//   - maintenance(声明实例池但无成员):return 503 维护态;
//   - 其余(address 单上游):set 变量 + proxy_pass http://$var(resolver 运行期解析)。
func writeProxyLocation(b *strings.Builder, indent string, svc RegisteredService, useUpstream, maintenance bool, upsName, proto string) {
	if maintenance && !useUpstream {
		// 实例全部摘除/地址不可解析:维护态,不做反代。
		b.WriteString(indent + "location / {\n")
		b.WriteString(indent + "    return 503;\n")
		b.WriteString(indent + "}\n")
		return
	}
	b.WriteString(indent + "location / {\n")
	if useUpstream {
		b.WriteString(indent + "    proxy_pass http://" + upsName + ";\n")
	} else {
		b.WriteString(indent + "    set " + nginxVarName(svc.Name) + " " + upstreamTarget(svc) + ";\n")
		b.WriteString(indent + "    proxy_pass http://" + nginxVarName(svc.Name) + ";\n")
	}
	b.WriteString(indent + "    proxy_http_version 1.1;\n")
	b.WriteString(indent + "    proxy_set_header Host $host;\n")
	b.WriteString(indent + "    proxy_set_header X-Real-IP $remote_addr;\n")
	b.WriteString(indent + "    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n")
	b.WriteString(indent + "    proxy_set_header X-Forwarded-Proto " + proto + ";\n")
	b.WriteString(indent + "    proxy_set_header Upgrade $http_upgrade;\n")
	b.WriteString(indent + "    proxy_set_header Connection $connection_upgrade;\n")
	b.WriteString(indent + "}\n")
}

// nginxUpstreamID 返回 nginx upstream 名后缀(服务名 `-` 归一为 `_`,与服务名字符集无碰撞)。
func nginxUpstreamID(name string) string {
	return strings.ReplaceAll(name, "-", "_")
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

	// 2) 容器存在:校验端口映射是否与期望一致(容器端口 + 宿主端口都要对);不一致 → 重建。
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
		if portsMatch(cur, want) && ver == nginxBootstrapVer {
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
	return out, nil
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
		_, _ = s.tg.Exec(ctx, serverID, []string{"docker", "exec", st.ContainerName, "chmod", "600", dir+"/privkey.pem"})
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

// applyNginxConf 把渲染好的 nginx.conf 下发并热加载:
//
//	Upload 到宿主 /tmp → docker cp 进容器 /tmp(容器看不到宿主文件系统,必须先拷入)
//	→ docker exec nginx -t -c /tmp/...(先测后换,校验失败不动活配置)
//	→ docker cp 到卷内正式路径 → docker exec nginx -s reload(master 优雅热加载)。
func applyNginxConf(ctx context.Context, tg target.Service, st *Settings, serverID, conf string) error {
	if err := tg.Upload(ctx, serverID, bytes.NewReader([]byte(conf)), nginxConfTmpPath); err != nil {
		return err
	}
	// 1) 拷进容器临时路径(校验只能对容器内文件做)。
	cpIn, err := tg.Exec(ctx, serverID, []string{"docker", "cp", nginxConfTmpPath, st.ContainerName + ":" + nginxConfTmpPath})
	if err != nil {
		return err
	}
	if cpIn.ExitCode != 0 {
		return fmt.Errorf("%w:%s", ErrApply, strings.TrimSpace(firstNonEmpty(cpIn.Stderr, cpIn.Stdout)))
	}
	// 2) 校验临时文件(失败即回,活配置未动)。
	val, err := tg.Exec(ctx, serverID, []string{"docker", "exec", st.ContainerName, "nginx", "-t", "-c", nginxConfTmpPath})
	if err != nil {
		return err
	}
	if val.ExitCode != 0 {
		return fmt.Errorf("%w: nginx -t 校验失败:%s", ErrApply, strings.TrimSpace(firstNonEmpty(val.Stderr, val.Stdout)))
	}
	// 2) 覆盖正式配置并热加载。
	res, err := tg.Exec(ctx, serverID, []string{"docker", "cp", nginxConfTmpPath, st.ContainerName + ":" + nginxConfPath})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%w:%s", ErrApply, strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout)))
	}
	rl, err := tg.Exec(ctx, serverID, []string{"docker", "exec", st.ContainerName, "nginx", "-s", "reload"})
	if err != nil {
		return err
	}
	if rl.ExitCode != 0 {
		return fmt.Errorf("%w:%s", ErrApply, strings.TrimSpace(firstNonEmpty(rl.Stderr, rl.Stdout)))
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
