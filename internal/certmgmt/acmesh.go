package certmgmt

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// 签发引擎容器编排常量(与 servicereg/nginx.go 同构,资源名独立)。
const (
	// acmeContainer 是网关主机上的 acme.sh 常驻工具箱容器名。
	acmeContainer = "pipewright-acme"
	// acmeStateVolume 是 acme.sh 自身状态卷(ACME 账户密钥/域配置,独立于 nginx 卷)。
	acmeStateVolume = "pipewright_acme"
	// acmeStateMount 是状态卷在容器内的挂载点(与镜像 ENV LE_CONFIG_HOME=/acme.sh 一致)。
	acmeStateMount = "/acme.sh"
	// acmeSharedMount 是网关 nginx 具名卷在容器内的挂载点(与 nginx 容器一致,
	// --install-cert 直写 /etc/pipewright/certs/<base_domain>/)。
	acmeSharedMount = "/etc/pipewright"
	// acmeshImageEnv 允许经环境变量覆盖 acme.sh 镜像。
	acmeshImageEnv = "PIPEWRIGHT_ACMESH_IMAGE"
	// acmeHostScriptPrefix 是一次性脚本的宿主临时路径前缀(Upload 后 docker cp)。
	acmeHostScriptPrefix = "/tmp/pw-cert-"
	// acmeScriptPath 是脚本在容器内的固定落点(执行完即删)。
	acmeScriptPath = "/tmp/pw-cert-run.sh"
	// defaultAcmeshImage 是自构建 acme.sh 镜像(deploy/acmesh vendored fork,workflow 推 GHCR)。
	defaultAcmeshImage = "ghcr.io/huangchengsir/pipewright-acmesh:latest"
)

// acmeImageRefRe 防注入:镜像引用字符集(首字符字母数字,防经 env 注入 docker flag)。
var acmeImageRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)

// acmeshImageRef 返回 acme.sh 镜像:PIPEWRIGHT_ACMESH_IMAGE 优先(字符集白名单校验),默认 GHCR。
func acmeshImageRef() string {
	if v := strings.TrimSpace(os.Getenv(acmeshImageEnv)); v != "" && acmeImageRefRe.MatchString(v) {
		return v
	}
	return defaultAcmeshImage
}

// ---------- 一次性脚本构造(纯函数,golden 可测) ----------

// runOp 是脚本要执行的 acme.sh 主操作。
type runOp string

const (
	issueOp runOp = "issue"
	renewOp runOp = "renew"
)

// buildRunScript 生成一次性执行脚本:mkdir 下发目录 → export DNS 凭据(单引号转义)→
// acme.sh --issue/--renew → 逐基域 --install-cert 直写 nginx 证书路径。
//
// 注入面分析(与 servicereg 固定自举脚本同一例外纪律):
//   - 域名/CA/密钥类型:领域层白名单校验过(FQDN/通配符正则 + 枚举),再经 shQuote 单引号包裹;
//   - DNS 凭据:任意字节,仅经 shQuote POSIX 单引号转义(' → '\''),不进命令行、不落日志;
//   - install 目标:基域名经 servicereg 同款校验。
func buildRunScript(op runOp, c *Certificate, dnsAPI string, dnsEnv [][2]string, installBases []string) string {
	sort.Strings(installBases) // 渲染确定(幂等输出,便于诊断/测试)
	var b strings.Builder
	b.WriteString("#!/bin/sh\nset -e\n")
	for _, base := range installBases {
		b.WriteString("mkdir -p " + shQuote(acmeSharedMount+"/certs/"+base) + "\n")
	}
	for _, kv := range dnsEnv {
		b.WriteString("export " + kv[0] + "=" + shQuote(kv[1]) + "\n")
	}
	b.WriteString("acme.sh --" + string(op))
	if op == issueOp {
		b.WriteString(" --dns " + dnsAPI)
		for _, d := range c.Domains {
			b.WriteString(" -d " + shQuote(d))
		}
		b.WriteString(" --keylength " + c.KeyType)
		b.WriteString(" --server " + shQuote(caServerURL[c.CA]))
	} else {
		// 续期:域配置(含 --dns/--keylength/--server)已持久化在 acme.sh 状态卷,
		// 仅需凭据 + --force(是否重签由平台续期窗口决定,不让 acme.sh 自己判断)。
		b.WriteString(" -d " + shQuote(c.PrimaryDomain) + " --force")
	}
	b.WriteString("\n")
	for _, base := range installBases {
		dir := acmeSharedMount + "/certs/" + base
		b.WriteString("acme.sh --install-cert -d " + shQuote(c.PrimaryDomain) +
			" --fullchain-file " + shQuote(dir+"/fullchain.pem") +
			" --key-file " + shQuote(dir+"/privkey.pem") + "\n")
	}
	return b.String()
}

// shQuote 返回 POSIX 单引号包裹的字面量(' → '\''),任意字节安全。
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// nginxInstallDirs 返回 install 目标基域列表(升序,渲染确定)。
func nginxInstallDirs(bases []BaseDomain) []string {
	out := make([]string, 0, len(bases))
	for _, b := range bases {
		out = append(out, b.BaseDomain)
	}
	sort.Strings(out)
	return out
}

// ---------- 容器生命周期(照抄 ensureNginx 手法) ----------

// engineEnv 是一次编排所需的网关环境(主机 + nginx 卷)。
type engineEnv struct {
	serverID    string
	nginxVolume string
}

// resolveEngineEnv 读取网关信息;未配置主机 → ErrNoGateway。
func (s *service) resolveEngineEnv(ctx context.Context) (*engineEnv, error) {
	if s.gw == nil {
		return nil, ErrNoGateway
	}
	serverID, vol, ok, err := s.gw.Gateway(ctx)
	if err != nil {
		return nil, err
	}
	if !ok || serverID == "" {
		return nil, ErrNoGateway
	}
	return &engineEnv{serverID: serverID, nginxVolume: vol}, nil
}

// ensureEngine 幂等地保证 acme.sh 容器就绪(网关未配置 → ErrNoGateway)。
func (s *service) ensureEngine(ctx context.Context) error {
	env, err := s.resolveEngineEnv(ctx)
	if err != nil {
		return err
	}
	return s.ensureEngineAt(ctx, env.serverID, env.nginxVolume)
}

// ensureEngineAt 在指定网关主机上保证容器就绪:不存在 → 起;已停 → start;
// 卷挂载不符(如 nginx 卷改名)→ 删除重建(acme 状态卷独立,重建不丢账户/域配置)。
func (s *service) ensureEngineAt(ctx context.Context, serverID, nginxVolume string) error {
	if s.tg == nil {
		return ErrAcmeshStart
	}
	insp, err := s.tg.Exec(ctx, serverID, []string{
		"docker", "inspect", acmeContainer, "--format", "{{.State.Running}}|{{range .Mounts}}{{.Name}}:{{.Destination}} {{end}}",
	})
	if err != nil {
		return err
	}
	if insp.ExitCode == 0 {
		line := strings.TrimSpace(insp.Stdout)
		running := strings.HasPrefix(line, "true|")
		mounts := ""
		if i := strings.IndexByte(line, '|'); i >= 0 {
			mounts = line[i+1:]
		}
		wantShared := nginxVolume + ":" + acmeSharedMount
		wantState := acmeStateVolume + ":" + acmeStateMount
		if running && strings.Contains(" "+mounts, " "+wantShared+" ") && strings.Contains(" "+mounts, " "+wantState+" ") {
			return nil
		}
		if !running {
			if st, serr := s.tg.Exec(ctx, serverID, []string{"docker", "start", acmeContainer}); serr == nil && st.ExitCode == 0 {
				return nil
			}
			// start 失败(如卷缺失的僵尸容器)→ 落到删除重建。
		}
		if rm, rerr := s.tg.Exec(ctx, serverID, []string{"docker", "rm", "-f", acmeContainer}); rerr != nil {
			return rerr
		} else if rm.ExitCode != 0 {
			return fmt.Errorf("%w:%s", ErrAcmeshStart, strings.TrimSpace(firstNonEmpty(rm.Stderr, rm.Stdout)))
		}
	}
	return s.runAcmeshContainer(ctx, serverID, nginxVolume)
}

// runAcmeshContainer 起签发引擎容器(array 不拼 shell)。命令 sleep infinity:
// 不带 cron —— 平台是唯一续期驱动(单一控制面,续期记录 UI 可见)。
func (s *service) runAcmeshContainer(ctx context.Context, serverID, nginxVolume string) error {
	image := acmeshImageRef()
	// best-effort pull(离线/已缓存场景交由 docker run 决断)。
	if _, perr := s.tg.Exec(ctx, serverID, []string{"docker", "pull", image}); perr != nil {
		return perr
	}
	res, err := s.tg.Exec(ctx, serverID, []string{
		"docker", "run", "-d",
		"--name", acmeContainer,
		"--restart", "unless-stopped",
		"-v", nginxVolume + ":" + acmeSharedMount,
		"-v", acmeStateVolume + ":" + acmeStateMount,
		image,
		"sleep", "infinity",
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%w:%s", ErrAcmeshStart, strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout)))
	}
	return nil
}

// inspectEngine 探测引擎状态(网关未配置 → Configured:false,非错误;容器缺失非错误)。
func (s *service) inspectEngine(ctx context.Context) (*EngineStatus, error) {
	out := &EngineStatus{}
	if s.gw == nil {
		return out, nil
	}
	serverID, _, ok, err := s.gw.Gateway(ctx)
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
	insp, err := s.tg.Exec(ctx, serverID, []string{
		"docker", "inspect", acmeContainer, "--format", "{{.State.Running}}|{{.Config.Image}}",
	})
	if err != nil {
		return nil, err
	}
	if insp.ExitCode != 0 {
		return out, nil // 容器不存在:Installed=false
	}
	out.Installed = true
	line := strings.TrimSpace(insp.Stdout)
	if i := strings.IndexByte(line, '|'); i >= 0 {
		out.Running = strings.EqualFold(strings.TrimSpace(line[:i]), "true")
		out.Image = strings.TrimSpace(line[i+1:])
	}
	return out, nil
}

// ---------- 脚本执行 / 回读 / 移除 ----------

// execScriptAt 上传一次性脚本到宿主 → docker cp 进容器 → sh 执行 → 双端删除。
// 凭据只经 Upload 通道与脚本文件存在,绝不进命令行/日志。
func (s *service) execScriptAt(ctx context.Context, serverID, id, script string) error {
	hostTmp := acmeHostScriptPrefix + id + ".sh"
	if err := s.tg.Upload(ctx, serverID, strings.NewReader(script), hostTmp); err != nil {
		return err
	}
	defer func() { _, _ = s.tg.Exec(context.Background(), serverID, []string{"rm", "-f", hostTmp}) }()
	if res, err := s.tg.Exec(ctx, serverID, []string{"docker", "cp", hostTmp, acmeContainer + ":" + acmeScriptPath}); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("%w:%s", ErrIssue, strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout)))
	}
	defer func() {
		_, _ = s.tg.Exec(context.Background(), serverID, []string{"docker", "exec", acmeContainer, "rm", "-f", acmeScriptPath})
	}()
	res, err := s.tg.Exec(ctx, serverID, []string{"docker", "exec", acmeContainer, "sh", acmeScriptPath})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%w:%s", ErrIssue, truncate(strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout)), 800))
	}
	return nil
}

// readBackCert 从 acme.sh 状态卷读回签发产物(fullchain.cer + <primary>.key)。
// 路径参数经 target.Exec 单引号转义,含 '*' 的通配符主域是字面量,无 glob 展开。
func (s *service) readBackCert(ctx context.Context, serverID, primary string) (string, string, error) {
	dir := acmeStateMount + "/" + primary
	certRes, err := s.tg.Exec(ctx, serverID, []string{"docker", "exec", acmeContainer, "cat", dir + "/fullchain.cer"})
	if err != nil {
		return "", "", err
	}
	if certRes.ExitCode != 0 {
		return "", "", fmt.Errorf("%w:%s", ErrReadBack, strings.TrimSpace(firstNonEmpty(certRes.Stderr, certRes.Stdout)))
	}
	keyRes, err := s.tg.Exec(ctx, serverID, []string{"docker", "exec", acmeContainer, "cat", dir + "/" + primary + ".key"})
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

// removeFromAcmesh best-effort 从 acme.sh 续期清单移除域名(不向 CA revoke;容器不在也静默)。
func (s *service) removeFromAcmesh(ctx context.Context, c *Certificate) {
	if s.gw == nil {
		return
	}
	serverID, _, ok, err := s.gw.Gateway(ctx)
	if err != nil || !ok || serverID == "" || s.tg == nil {
		return
	}
	_, _ = s.tg.Exec(ctx, serverID, []string{
		"docker", "exec", acmeContainer, "acme.sh", "--remove", "-d", c.PrimaryDomain,
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
