// acme.sh 以**宿主机脚本**方式运行在网关主机上(无容器、无镜像链):
//   - 自维护 fork(szxufan/acme.sh)的最小集 vendored 在本包 acmesh/ 下(acme.sh 主脚本 +
//     dns_cf/dns_dp/dns_ali 三个 dnsapi),经 go:embed 嵌进平台二进制 —— 升级 fork =
//     换文件重编译,不依赖任何镜像构建/发布链路;
//   - 首次使用(或显式「部署引擎」)时经 SSH 复制到网关主机 /opt/pipewright/acme/,
//     之后直接以 sh 运行。acme.sh 是纯 shell 脚本,运行期依赖仅系统 curl + openssl;
//   - 证书产物落在本目录(<home>/<主域>/fullchain.cer + .key),由平台读回、密文入库,
//     再经 CertSink(servicereg)下发 nginx 并 reload —— acme.sh 不直接写 nginx 卷。
//
// 注入面纪律不变:一切命令经 target.Exec 以 array 形式执行(AC-SEC-02 不拼 shell);
// 唯一例外仍是一次性签发脚本(Upload 到 /tmp → sh 执行 → 删除),内容 = 白名单校验过的
// 域名/CA/密钥类型 + POSIX 单引号转义过的 DNS 凭据,凭据绝不进命令行/日志。
package certmgmt

import (
	"context"
	"embed"
	"fmt"
	"strings"
)

// 网关主机上的 acme.sh 安装布局(常量,无 env 覆盖 —— 路径即契约)。
const (
	// acmeHomeDir 是 acme.sh 在网关主机上的安装目录(同时作为 --home 配置目录:
	// ACME 账户密钥、域配置、证书产物全部落在此,重复部署幂等)。
	acmeHomeDir = "/opt/pipewright/acme"
	// acmeScript 是主脚本的绝对路径(安装后存在)。
	acmeScript = acmeHomeDir + "/acme.sh"
	// acmeHostScriptPrefix 是一次性签发脚本的宿主临时路径前缀(Upload 后直接 sh 执行)。
	acmeHostScriptPrefix = "/tmp/pw-cert-"
)

// acmeshFS 嵌入自维护 fork 的最小运行集(主脚本 + 三家 DNS 提供商的 dnsapi)。
//
//go:embed acmesh/acme.sh
//go:embed acmesh/dnsapi/dns_cf.sh
//go:embed acmesh/dnsapi/dns_dp.sh
//go:embed acmesh/dnsapi/dns_ali.sh
var acmeshFS embed.FS

// acmeshFiles 是「嵌入源路径 → 网关主机目标路径」的安装清单(确定性顺序)。
var acmeshFiles = []struct {
	src  string
	dest string
}{
	{"acmesh/acme.sh", acmeScript},
	{"acmesh/dnsapi/dns_cf.sh", acmeHomeDir + "/dnsapi/dns_cf.sh"},
	{"acmesh/dnsapi/dns_dp.sh", acmeHomeDir + "/dnsapi/dns_dp.sh"},
	{"acmesh/dnsapi/dns_ali.sh", acmeHomeDir + "/dnsapi/dns_ali.sh"},
}

// depsProbe 是运行期依赖探测的固定常量脚本(无任何用户输入,非注入面;
// 与 servicereg 自举脚本同一例外纪律):acme.sh 只需系统 sh + curl + openssl。
const depsProbe = `command -v curl >/dev/null 2>&1 && command -v openssl >/dev/null 2>&1`

// ---------- 引擎安装 / 探测 ----------

// ensureEngineAt 幂等地在网关主机上安装/更新 acme.sh 脚本集并校验运行期依赖:
// 建目录 → 覆盖复制嵌入文件(逐个 Upload)→ chmod → 探测 curl/openssl(缺 → ErrAcmeshStart)。
func (s *service) ensureEngineAt(ctx context.Context, serverID string) error {
	if s.tg == nil {
		return ErrAcmeshStart
	}
	if res, err := s.tg.Exec(ctx, serverID, []string{"mkdir", "-p", acmeHomeDir + "/dnsapi"}); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("%w:无法创建 %s(请确认 SSH 用户有权限;通常需要 root 或先 sudo mkdir 并授权)%s",
			ErrAcmeshStart, acmeHomeDir, strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout)))
	}
	// 逐文件安装:无条件覆盖上传(内容确定性、幂等;fork 升级 = 换嵌入文件重编译后,
	// 任何一次签发/显式部署都会把新脚本同步到网关,无版本漂移)。
	for _, f := range acmeshFiles {
		content, rerr := acmeshFS.ReadFile(f.src)
		if rerr != nil {
			return fmt.Errorf("%w:嵌入文件缺失 %s", ErrAcmeshStart, f.src)
		}
		if err := s.tg.Upload(ctx, serverID, strings.NewReader(string(content)), f.dest); err != nil {
			return fmt.Errorf("%w:复制 %s 到网关主机失败:%v", ErrAcmeshStart, f.src, err)
		}
	}
	if res, err := s.tg.Exec(ctx, serverID, []string{"chmod", "700", acmeScript}); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("%w:%s", ErrAcmeshStart, strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout)))
	}
	return s.ensureDeps(ctx, serverID)
}

// ensureDeps 探测系统 curl + openssl(acme.sh 唯一的运行期依赖),缺失给人话错误。
func (s *service) ensureDeps(ctx context.Context, serverID string) error {
	res, err := s.tg.Exec(ctx, serverID, []string{"sh", "-c", depsProbe})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%w:网关主机缺少 curl 或 openssl(DNS-01 签发需要,请先安装)", ErrAcmeshStart)
	}
	return nil
}

// inspectEngine 探测引擎状态(单次 SSH,固定常量脚本):脚本就位?依赖就绪?版本号?
// 网关未配置主机 → Configured:false(非错误);探测传输错误才返回 error。
func (s *service) inspectEngine(ctx context.Context) (*EngineStatus, error) {
	out := &EngineStatus{}
	if s.gw == nil {
		return out, nil
	}
	serverID, ok, err := s.gw.Gateway(ctx)
	if err != nil {
		return nil, err
	}
	if !ok || serverID == "" {
		return out, nil
	}
	out.Configured = true
	out.ServerID = serverID
	// 主机展示名 best-effort(取不到回退 id)。
	if s.tg != nil {
		if srv, gerr := s.tg.Get(ctx, serverID); gerr == nil && srv != nil && srv.Name != "" {
			out.ServerName = srv.Name
		}
	}
	if out.ServerName == "" {
		out.ServerName = serverID
	}
	if s.tg == nil {
		return out, nil
	}
	probe := `if [ -f ` + acmeScript + ` ]; then echo PW_INSTALLED=1; else echo PW_INSTALLED=0; fi; ` +
		`if ` + depsProbe + `; then echo PW_READY=1; else echo PW_READY=0; fi; ` +
		`sh ` + acmeScript + ` --version 2>/dev/null | tail -1 || true`
	res, err := s.tg.Exec(ctx, serverID, []string{"sh", "-c", probe})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return out, nil // 探测失败按未部署处理,不阻断页面
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "PW_INSTALLED=1":
			out.Installed = true
		case line == "PW_READY=1":
			out.Ready = true
		case line != "" && !strings.HasPrefix(line, "PW_") && out.Version == "":
			out.Version = truncate(line, 64) // acme.sh --version 的末行(如 v3.1.1)
		}
	}
	return out, nil
}

// ---------- 一次性脚本构造(纯函数,golden 可测) ----------

// runOp 是脚本要执行的 acme.sh 主操作。
type runOp string

const (
	issueOp runOp = "issue"
	renewOp runOp = "renew"
)

// buildRunScript 生成一次性执行脚本:export DNS 凭据(单引号转义)→ acme.sh --issue/--renew。
//
// 注入面分析(与 servicereg 固定自举脚本同一例外纪律):
//   - 域名/CA/密钥类型:领域层白名单校验过(FQDN/通配符正则 + 枚举),再经 shQuote 单引号包裹;
//   - DNS 凭据:任意字节,仅经 shQuote POSIX 单引号转义(' → '\''),不进命令行、不落日志;
//   - 其余内容全为包内常量路径。
func buildRunScript(op runOp, c *Certificate, dnsAPI string, dnsEnv [][2]string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\nset -e\n")
	for _, kv := range dnsEnv {
		b.WriteString("export " + kv[0] + "=" + shQuote(kv[1]) + "\n")
	}
	b.WriteString(acmeScript + " --home " + shQuote(acmeHomeDir) + " --" + string(op))
	if op == issueOp {
		b.WriteString(" --dns " + dnsAPI)
		for _, d := range c.Domains {
			b.WriteString(" -d " + shQuote(d))
		}
		b.WriteString(" --keylength " + c.KeyType)
		b.WriteString(" --server " + shQuote(caServerURL[c.CA]))
	} else {
		// 续期:域配置(含 --dns/--keylength/--server)已持久化在 home 下,
		// 仅需凭据 + --force(是否重签由平台续期窗口决定,不让 acme.sh 自己判断)。
		b.WriteString(" -d " + shQuote(c.PrimaryDomain) + " --force")
	}
	b.WriteString("\n")
	return b.String()
}

// shQuote 返回 POSIX 单引号包裹的字面量(' → '\''),任意字节安全。
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ---------- 脚本执行 / 回读 / 移除 ----------

// execScriptAt 上传一次性脚本到网关主机 /tmp → sh 执行 → 删除。
// 凭据只经 Upload 通道与脚本文件短暂存在,绝不进命令行/日志。
func (s *service) execScriptAt(ctx context.Context, serverID, id, script string) error {
	hostTmp := acmeHostScriptPrefix + id + ".sh"
	if err := s.tg.Upload(ctx, serverID, strings.NewReader(script), hostTmp); err != nil {
		return err
	}
	defer func() { _, _ = s.tg.Exec(context.Background(), serverID, []string{"rm", "-f", hostTmp}) }()
	res, err := s.tg.Exec(ctx, serverID, []string{"sh", hostTmp})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%w:%s", ErrIssue, truncate(strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout)), 800))
	}
	return nil
}

// readBackCert 从 acme.sh home 读回签发产物(fullchain.cer + <primary>.key)。
// 路径参数经 target.Exec 单引号转义,含 '*' 的通配符主域是字面量,无 glob 展开。
func (s *service) readBackCert(ctx context.Context, serverID, primary string) (string, string, error) {
	dir := acmeHomeDir + "/" + primary
	certRes, err := s.tg.Exec(ctx, serverID, []string{"cat", dir + "/fullchain.cer"})
	if err != nil {
		return "", "", err
	}
	if certRes.ExitCode != 0 {
		return "", "", fmt.Errorf("%w:%s", ErrReadBack, strings.TrimSpace(firstNonEmpty(certRes.Stderr, certRes.Stdout)))
	}
	keyRes, err := s.tg.Exec(ctx, serverID, []string{"cat", dir + "/" + primary + ".key"})
	if err != nil {
		return "", "", err
	}
	if keyRes.ExitCode != 0 {
		return "", "", fmt.Errorf("%w:%s", ErrReadBack, strings.TrimSpace(firstNonEmpty(keyRes.Stderr, keyRes.Stdout)))
	}
	certPEM, keyPEM := strings.TrimSpace(certRes.Stdout), strings.TrimSpace(keyRes.Stdout)
	if certPEM == "" || keyPEM == "" {
		return "", "", fmt.Errorf("%w:产物为空(签发可能尚未完成)", ErrReadBack)
	}
	return certPEM, keyPEM, nil
}

// removeFromAcmesh best-effort 从 acme.sh 续期清单移除域名(不向 CA revoke;脚本未安装也静默)。
func (s *service) removeFromAcmesh(ctx context.Context, c *Certificate) {
	if s.gw == nil {
		return
	}
	serverID, ok, err := s.gw.Gateway(ctx)
	if err != nil || !ok || serverID == "" || s.tg == nil {
		return
	}
	_, _ = s.tg.Exec(ctx, serverID, []string{
		acmeScript, "--home", acmeHomeDir, "--remove", "-d", c.PrimaryDomain,
	})
}

// firstNonEmpty 返回首个非空白字符串(与 servicereg 同名手法)。
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
