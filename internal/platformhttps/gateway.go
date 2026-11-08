package platformhttps

// gateway.go 是「本机网关入口 vhost」(specs/local-node-roles.md 7.3):本机为网关主机且
// 平台 HTTPS 启用时,宿主 nginx 独占 80/443 作为唯一入口 —— 平台 vhost(精确域名)之外,
// 为每个证书就绪的网关基域渲染一组 server 块,由 servicereg 经 ApplyGateway/RemoveGateway
// 联动发布/摘除(servicereg → platformhttps 单向调用;本包不感知 servicereg)。
//
// 端口拓扑:网关容器在共存模式绑定 127.0.0.1:<LoopbackHTTPPort/LoopbackHTTPSPort>
// (servicereg 侧编排);外部流量全部走宿主 nginx。443 块**再加密**转发到网关容器 443 回环
// 端口(proxy_ssl_verify off + SNI 透传 $host)—— 网关自身 80 块是 301 跳转,转发 80 会
// 死循环;双重 TLS 终结让网关语义分毫未动(与 mesh 反代网关同款惯用法)。80 块统一 301。
//
// 事务纪律(与平台 vhost 同款):写入(证书 + vhost)→ nginx -t(失败整体回滚上一版)→
// nginx -T 验证 include → reload。同一事务覆盖两个 conf 文件:平台 vhost 与网关 vhost 同时
// 在位,一次 -t 校验的是「合并后的真实配置」。
//
// Disable/RedeployCert 覆盖:平台禁用时先摘除入口 vhost(它引用的证书目录可能被平台清理);
// 证书续期时按 CertID 刷新入口 vhost 引用的 PEM 并重发布。

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

const (
	// managedGatewayConf 是网关入口 vhost 落点(conf.d 与平台 vhost 并存,独立 marker)。
	managedGatewayConf = "/etc/nginx/conf.d/pipewright-gateway.conf"
	// gatewayConfMarker 是入口 vhost 的唯一标记(nginx -T 验证 + 平台识别)。
	gatewayConfMarker = "pipewright-gateway-entry"
	// gatewayBackupConf 是入口 vhost 的回滚备份(先删陈旧再备份,同平台 vhost 惯例)。
	gatewayBackupConf = tmpDir + "/pipewright-gateway.conf.bak"
	// gatewayConfTmp / gatewayCertTmpFmt 是临时落盘路径(s.mu 串行化,与平台 Apply 互斥)。
	gatewayConfTmp             = tmpDir + "/pipewright-gateway.conf"
	gatewayCertTmpFullchainFmt = tmpDir + "/gwcert-%d-fullchain.pem"
	gatewayCertTmpKeyFmt       = tmpDir + "/gwcert-%d-privkey.pem"
	// gatewayCertBackupFullFmt / gatewayCertBackupKeyFmt 是安装前既有证书的备份落点:
	// certs/<基域>/ 可能与平台 vhost 共用,安装即覆盖,校验失败必须连证书一起还原。
	gatewayCertBackupFullFmt = tmpDir + "/gwprev-%d-fullchain.pem"
	gatewayCertBackupKeyFmt  = tmpDir + "/gwprev-%d-privkey.pem"
)

// GatewayEntryCert 是一个基域的入口证书(PEM 由调用方解密传入,仅进程内传递,绝不日志)。
type GatewayEntryCert struct {
	BaseDomain   string // efg.com
	CertID       string // 证书来源 id(certmgmt 证书 id / servicereg 域名 id;续期联动用)
	FullchainPEM string
	PrivkeyPEM   string
}

// GatewayEntrySpec 是入口 vhost 的期望态(全量收敛;幂等重放安全)。
type GatewayEntrySpec struct {
	// LoopbackHTTPPort / LoopbackHTTPSPort 是网关容器的回环宿主端口(servicereg Settings)。
	LoopbackHTTPPort  int
	LoopbackHTTPSPort int
	// Certs 是证书就绪的基域集合(就绪门由调用方把关;空 = 无可发布 → 调用方应走 RemoveGateway)。
	Certs []GatewayEntryCert
}

// renderGatewayConf 纯函数渲染(golden 单测直接断言文本)。每基域一组 server 块:
//   - 443 ssl:TLS 终结(挂 certs/<基域>/)→ proxy_pass https://127.0.0.1:<回环HTTPS>
//     (proxy_ssl_verify off:回环自连;proxy_ssl_name $host:SNI 透传原域名,网关按
//     server_name 选证书/站点)+ WS 升级 + SSE 长读超时(与平台 vhost 同款 3600s);
//   - 80:return 301 https://$host$request_uri($host 保留原子域名,网关子域自然升级)。
//
// WS 升级的 map 变量用 $pw_gw_connection_upgrade(平台 vhost 用的 $connection_upgrade 定义在
// 平台文件里 —— 两文件必须各自自洽,重复定义同名 map 会让 nginx -t 报 duplicate)。
func renderGatewayConf(spec GatewayEntrySpec) string {
	var b strings.Builder
	b.WriteString("# " + gatewayConfMarker + ":由 Pipewright 自动生成(本机网关入口),请勿手改。\n\n")
	b.WriteString("map $http_upgrade $pw_gw_connection_upgrade {\n")
	b.WriteString("    default upgrade;\n")
	b.WriteString("    ''      close;\n")
	b.WriteString("}\n\n")

	target := "https://127.0.0.1:" + strconv.Itoa(spec.LoopbackHTTPSPort)
	for _, c := range spec.Certs {
		dir := certDir(c.BaseDomain)
		b.WriteString("server {\n")
		b.WriteString("    listen 443 ssl;\n")
		b.WriteString("    server_name *." + c.BaseDomain + " " + c.BaseDomain + ";\n\n")
		b.WriteString("    ssl_certificate     " + dir + "/fullchain.pem;\n")
		b.WriteString("    ssl_certificate_key " + dir + "/privkey.pem;\n")
		b.WriteString("    ssl_protocols TLSv1.2 TLSv1.3;\n\n")
		b.WriteString("    client_max_body_size 512m;\n\n")
		b.WriteString("    location / {\n")
		b.WriteString("        # 再加密转发网关容器 443 回环端口(网关自身 80 块是 301 跳转,转发 80 会死循环)。\n")
		b.WriteString("        proxy_pass " + target + ";\n")
		b.WriteString("        proxy_ssl_verify off;\n")
		b.WriteString("        proxy_ssl_server_name on;\n")
		b.WriteString("        proxy_ssl_name $host;\n")
		b.WriteString("        proxy_http_version 1.1;\n")
		b.WriteString("        proxy_set_header Host $host;\n")
		b.WriteString("        proxy_set_header X-Real-IP $remote_addr;\n")
		b.WriteString("        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n")
		b.WriteString("        proxy_set_header X-Forwarded-Proto https;\n")
		b.WriteString("        proxy_set_header Upgrade $http_upgrade;\n")
		b.WriteString("        proxy_set_header Connection $pw_gw_connection_upgrade;\n")
		b.WriteString("        # SSE 日志流/长连接需要较长读超时(与平台 vhost 同款)。\n")
		b.WriteString("        proxy_read_timeout 3600s;\n")
		b.WriteString("        proxy_send_timeout 3600s;\n")
		b.WriteString("    }\n")
		b.WriteString("}\n\n")

		b.WriteString("server {\n")
		b.WriteString("    listen 80;\n")
		b.WriteString("    server_name *." + c.BaseDomain + " " + c.BaseDomain + ";\n\n")
		b.WriteString("    return 301 https://$host$request_uri;\n")
		b.WriteString("}\n\n")
	}
	return b.String()
}

// ApplyGateway 全量收敛本机网关入口 vhost(幂等;spec 与现配置一致时仍安全重放)。
// 与平台 Apply 共用 s.mu 串行(临时目录/提权判定/nginx -t 序列不可交错)。
func (s *service) ApplyGateway(ctx context.Context, spec GatewayEntrySpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(spec.Certs) == 0 {
		return s.removeGatewayEntryLocked(ctx)
	}
	// 回环端口是渲染与容器绑定的共同前提:0/越界会渲染 127.0.0.1:0 并让网关容器启动即失败。
	// servicereg 侧已在 syncGatewayEntry 先行拦截,这里保留最后一道防线(直接调用方)。
	if spec.LoopbackHTTPPort < 1 || spec.LoopbackHTTPPort > 65535 ||
		spec.LoopbackHTTPSPort < 1 || spec.LoopbackHTTPSPort > 65535 {
		return fmt.Errorf("%w:回环端口非法(%d/%d)", ErrApply, spec.LoopbackHTTPPort, spec.LoopbackHTTPSPort)
	}
	if _, _, code, err := s.host.Run(ctx, "nginx", []string{"-v"}); err != nil || code != 0 {
		return ErrNoNginx
	}
	p, err := buildPrivilege(ctx, s.host)
	if err != nil {
		return err
	}
	conf := renderGatewayConf(spec)

	// 1) 临时目录 + conf + 各域证书落盘(700 目录兜底;私钥 600)。
	for _, cmd := range [][]string{{"mkdir", "-p", tmpDir}, {"chmod", "700", tmpDir}} {
		if res, ok, err := execOK(ctx, s.host, cmd...); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("%w:创建临时目录失败:%s", ErrApply, trimOut(res))
		}
	}
	type certFile struct{ path, data string }
	files := []certFile{{gatewayConfTmp, conf}}
	for i, c := range spec.Certs {
		files = append(files,
			certFile{fmt.Sprintf(gatewayCertTmpFullchainFmt, i), c.FullchainPEM},
			certFile{fmt.Sprintf(gatewayCertTmpKeyFmt, i), c.PrivkeyPEM},
		)
	}
	for _, f := range files {
		if err := s.host.WriteFile(f.path, []byte(f.data), 0o600); err != nil {
			return fmt.Errorf("%w:写临时文件失败:%s", ErrApply, err)
		}
	}
	defer func() { cleanupTmp(ctx, s.host, p) }()

	// 2) 证书先备份后安装(certDir 与平台 vhost 共用 certs 根;per-domain 目录互不覆盖,
	// 但「平台域名恰为基域」时同名文件会被本步覆盖)→ 收紧私钥。任何一步失败按记录整体
	// 回滚(证书 + conf),不留下「conf 已换、证书已换、校验却失败」的半态。
	records := make([]certInstallRecord, 0, len(spec.Certs))
	for i, c := range spec.Certs {
		dir := certDir(c.BaseDomain)
		rec := certInstallRecord{dir: dir}
		if _, ok, err := execPriv(ctx, s.host, p, "test", "-d", dir); err != nil {
			return err
		} else {
			rec.dirExisted = ok
		}
		if rec.backupFull, err = backupRemoteFile(ctx, s.host, p, dir+"/fullchain.pem", fmt.Sprintf(gatewayCertBackupFullFmt, i)); err != nil {
			return err
		}
		if rec.backupKey, err = backupRemoteFile(ctx, s.host, p, dir+"/privkey.pem", fmt.Sprintf(gatewayCertBackupKeyFmt, i)); err != nil {
			return err
		}
		records = append(records, rec)
	}
	for i, c := range spec.Certs {
		dir := certDir(c.BaseDomain)
		full := fmt.Sprintf(gatewayCertTmpFullchainFmt, i)
		key := fmt.Sprintf(gatewayCertTmpKeyFmt, i)
		for _, cmd := range [][]string{
			{"mkdir", "-p", dir},
			{"cp", full, dir + "/fullchain.pem"},
			{"cp", key, dir + "/privkey.pem"},
			{"chmod", "600", dir + "/privkey.pem"},
		} {
			if res, ok, err := execPriv(ctx, s.host, p, cmd...); err != nil {
				return err
			} else if !ok {
				rollbackGatewayEntry(ctx, s.host, p, records)
				return fmt.Errorf("%w:%s", ErrApply, trimOut(res))
			}
		}
	}

	// 3) 备份既有入口 vhost(可能不存在,容忍)→ 覆盖安装;先删陈旧备份保证回滚还原的
	// 只会是本次备份。
	_, _, _ = execPriv(ctx, s.host, p, "rm", "-f", gatewayBackupConf)
	_, _, _ = execPriv(ctx, s.host, p, "cp", managedGatewayConf, gatewayBackupConf)
	if res, ok, err := execPriv(ctx, s.host, p, "cp", gatewayConfTmp, managedGatewayConf); err != nil {
		return err
	} else if !ok {
		rollbackGatewayEntry(ctx, s.host, p, records)
		return fmt.Errorf("%w:%s", ErrApply, trimOut(res))
	}

	// 4) nginx -t:两个 conf 文件(平台 + 网关入口)合并校验;失败整体回滚上一版。
	if res, ok, err := execPriv(ctx, s.host, p, "nginx", "-t"); err != nil {
		return err
	} else if !ok {
		rollbackGatewayEntry(ctx, s.host, p, records)
		return fmt.Errorf("%w:nginx -t 校验失败(网关入口 vhost):%s", ErrApply, trimOut(res))
	}

	// 5) nginx -T 验证入口 vhost 真被 include。
	if res, ok, err := execPriv(ctx, s.host, p, "nginx", "-T"); err != nil {
		return err
	} else if !ok || !strings.Contains(res.stdout, gatewayConfMarker) {
		rollbackGatewayEntry(ctx, s.host, p, records)
		return fmt.Errorf("%w:网关入口 vhost 未被 nginx 加载(conf.d 未被 include)", ErrApply)
	}

	// 6) 热加载(nginx -s reload → 失败回退 systemctl reload nginx;不自动 start)。
	if res, ok, err := execPriv(ctx, s.host, p, "nginx", "-s", "reload"); err != nil {
		return err
	} else if !ok {
		if res2, ok2, err2 := execPriv(ctx, s.host, p, "systemctl", "reload", "nginx"); err2 != nil || !ok2 {
			return fmt.Errorf("%w:网关入口 vhost 已就位但热加载失败(nginx 可能未运行):%s",
				ErrApply, firstNonEmpty(trimOut(res), trimOut(res2)))
		}
	}
	// 记住已发布 spec(RedeployCert 按 CertID 刷新 PEM 用;进程内存态,重启后由 servicereg
	// 下一次联动重放)。
	s.gwLast = &spec
	return nil
}

// RemoveGateway 摘除本机网关入口 vhost 与其证书目录(幂等;未发布过 = 成功)。
func (s *service) RemoveGateway(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.removeGatewayEntryLocked(ctx)
}

// removeGatewayEntryLocked 摘除入口 vhost(须持 s.mu;conf 不在 = no-op)。
func (s *service) removeGatewayEntryLocked(ctx context.Context) error {
	// 先取已发布基域清单(证书目录清理依据)再清状态。
	var domains []string
	if s.gwLast != nil {
		for _, c := range s.gwLast.Certs {
			domains = append(domains, c.BaseDomain)
		}
	}
	s.gwLast = nil
	if _, ok, err := execOK(ctx, s.host, "test", "-f", managedGatewayConf); err != nil {
		return err
	} else if !ok {
		return nil
	}
	p, err := buildPrivilege(ctx, s.host)
	if err != nil {
		return err
	}
	defer func() { cleanupTmp(ctx, s.host, p) }()
	_, _, _ = execPriv(ctx, s.host, p, "rm", "-f", managedGatewayConf)
	// 基域证书目录仅在空时移除(rmdir 非空失败可容忍):平台 vhost 可能与基域共用同一目录
	// (平台域名恰为基域裸域),绝不误删他处引用的证书。
	for _, d := range domains {
		_, _, _ = execPriv(ctx, s.host, p, "rmdir", certDir(d))
	}
	_, _, _ = execPriv(ctx, s.host, p, "rmdir", certsBaseDir)
	if res, ok, err := execPriv(ctx, s.host, p, "nginx", "-t"); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%w:摘除网关入口 vhost 后 nginx -t 校验失败(本机其余配置自身有误):%s", ErrApply, trimOut(res))
	}
	if res, ok, err := execPriv(ctx, s.host, p, "nginx", "-s", "reload"); err != nil {
		return err
	} else if !ok {
		if _, ok2, err2 := execPriv(ctx, s.host, p, "systemctl", "reload", "nginx"); err2 != nil || !ok2 {
			return fmt.Errorf("%w:网关入口 vhost 已摘除但热加载失败(nginx 可能未运行):%s", ErrApply, trimOut(res))
		}
	}
	return nil
}

// rollbackGatewayConf 在校验失败时恢复入口 vhost:有备份 → 还原;无备份(首次)→ 摘除。
func rollbackGatewayConf(ctx context.Context, h Host, p privilege) {
	if _, ok, _ := execPriv(ctx, h, p, "test", "-f", gatewayBackupConf); ok {
		_, _, _ = execPriv(ctx, h, p, "cp", gatewayBackupConf, managedGatewayConf)
		return
	}
	_, _, _ = execPriv(ctx, h, p, "rm", "-f", managedGatewayConf)
}

// certInstallRecord 记录一个基域证书目录在安装前的原状,供校验失败时回滚(conf 与证书
// 同事务):backup* 非空 = 原文件已备份;空 = 原本不存在(回滚时删除本次安装的文件)。
type certInstallRecord struct {
	dir        string
	backupFull string
	backupKey  string
	dirExisted bool
}

// backupRemoteFile 把 remote 备份到 backup:remote 不存在 → ("", nil);备份失败 → error
// (无法保证回滚,直接中止本次 apply,绝不冒险覆盖原文件)。
func backupRemoteFile(ctx context.Context, h Host, p privilege, remote, backup string) (string, error) {
	if _, ok, err := execPriv(ctx, h, p, "test", "-f", remote); err != nil {
		return "", err
	} else if !ok {
		return "", nil
	}
	if res, ok, err := execPriv(ctx, h, p, "cp", remote, backup); err != nil {
		return "", err
	} else if !ok {
		return "", fmt.Errorf("%w:备份既有证书失败(无法保证回滚):%s", ErrApply, trimOut(res))
	}
	return backup, nil
}

// rollbackGatewayEntry 回滚一次失败的 ApplyGateway:先还原证书(旧 conf 引用旧证书,
// 顺序不可反),再按原有备份/摘除语义恢复入口 vhost。首次发布(无备份)时删掉新文件。
func rollbackGatewayEntry(ctx context.Context, h Host, p privilege, records []certInstallRecord) {
	for _, r := range records {
		if r.backupFull != "" {
			_, _, _ = execPriv(ctx, h, p, "cp", r.backupFull, r.dir+"/fullchain.pem")
		} else {
			_, _, _ = execPriv(ctx, h, p, "rm", "-f", r.dir+"/fullchain.pem")
		}
		if r.backupKey != "" {
			_, _, _ = execPriv(ctx, h, p, "cp", r.backupKey, r.dir+"/privkey.pem")
		} else {
			_, _, _ = execPriv(ctx, h, p, "rm", "-f", r.dir+"/privkey.pem")
		}
		if !r.dirExisted {
			_, _, _ = execPriv(ctx, h, p, "rmdir", r.dir)
		}
	}
	rollbackGatewayConf(ctx, h, p)
}
