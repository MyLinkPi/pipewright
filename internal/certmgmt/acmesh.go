// acme.sh 以**控制机本地脚本**方式运行 —— 签发/续期不依赖任何远程主机(不需要配置网关):
//   - 自维护 fork(szxufan/acme.sh)的最小集 vendored 在本包 acmesh/ 下(acme.sh 主脚本 +
//     dns_cf/dns_dp/dns_ali 三个 dnsapi),经 go:embed 嵌进平台二进制 —— 升级 fork =
//     换文件重编译,不依赖任何镜像构建/发布链路;
//   - 首次签发(或显式「安装引擎」)时释放到本地 acme home 目录(默认 DB 同级 acme/,
//     main.go 注入;env PIPEWRIGHT_ACMESH_HOME 可覆盖),经 os/exec 直接运行。acme.sh 是
//     纯 shell 脚本,运行期依赖仅 sh + curl + openssl(控制机须为 Linux/POSIX 环境);
//   - 证书产物落在本目录(<home>/<主域>/fullchain.cer + .key),由平台读回、密文入库,
//     再经 CertSink(servicereg)下发网关并 reload —— 网关未配置也不影响签发,配好后随
//     全量收敛自动带上。
//
// 注入面纪律:acme.sh 经 os/exec 以**参数数组**调用(绝不拼 shell);DNS 凭据经进程环境
// 变量传递(不进命令行/日志/库),随进程结束即散。
package certmgmt

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// acmeHomeEnv 允许经环境变量覆盖本地 acme home 目录(默认 main.go 注入的 DB 同级 acme/)。
const acmeHomeEnv = "PIPEWRIGHT_ACMESH_HOME"

// defaultAcmeHome 是未注入且连用户缓存目录都取不到时的极端兜底(相对工作目录);
// 生产装配由 main.go 注入绝对路径,正常走不到这里。
const defaultAcmeHome = "acme"

// acmeshFS 嵌入自维护 fork 的最小运行集(主脚本 + 三家 DNS 提供商的 dnsapi)。
//
//go:embed acmesh/acme.sh
//go:embed acmesh/dnsapi/dns_cf.sh
//go:embed acmesh/dnsapi/dns_dp.sh
//go:embed acmesh/dnsapi/dns_ali.sh
var acmeshFS embed.FS

// acmeshFiles 是「嵌入源路径 → 本地目标路径(相对 home)」的安装清单(确定性顺序)。
var acmeshFiles = []struct {
	src  string
	dest string
}{
	{"acmesh/acme.sh", "acme.sh"},
	{"acmesh/dnsapi/dns_cf.sh", "dnsapi/dns_cf.sh"},
	{"acmesh/dnsapi/dns_dp.sh", "dnsapi/dns_dp.sh"},
	{"acmesh/dnsapi/dns_ali.sh", "dnsapi/dns_ali.sh"},
}

// acmeDeps 是 acme.sh 的运行期依赖(控制机本地查找;缺任一 → 引擎未就绪)。
var acmeDeps = []string{"sh", "curl", "openssl"}

// ---------- 路径解析 ----------

// homeDir 返回本地 acme home(SetHomeDir 注入优先,env 次之,最后兜底用户缓存目录下
// 的固定位置 —— 绝不用相对 cwd:进程工作目录不可控,含私钥的 home 会随处漂移)。
func (s *service) homeDir() string {
	if s.home != "" {
		return s.home
	}
	if v := strings.TrimSpace(os.Getenv(acmeHomeEnv)); v != "" {
		return filepath.Clean(v)
	}
	if cache, err := os.UserCacheDir(); err == nil {
		return filepath.Join(cache, "pipewright", "acme")
	}
	return defaultAcmeHome
}

// scriptPath 返回 acme.sh 主脚本路径。
func (s *service) scriptPath() string {
	return filepath.Join(s.homeDir(), "acme.sh")
}

// ---------- 引擎安装 / 探测 ----------

// ensureEngine 幂等地在本地安装/更新 acme.sh 脚本集并校验运行期依赖:
// 建目录 → 逐文件与嵌入内容比对、不一致即覆盖(升级 fork = 换嵌入文件重编译即生效;
// 只覆盖这 4 个脚本,ACME 账户/证书产物不动)→ 校验 sh/curl/openssl(缺 → ErrAcmeshStart 人话)。
func (s *service) ensureEngine() error {
	home := s.homeDir()
	if err := os.MkdirAll(filepath.Join(home, "dnsapi"), 0o700); err != nil {
		return fmt.Errorf("%w:无法创建 %s(%v)", ErrAcmeshStart, home, err)
	}
	for _, f := range acmeshFiles {
		dest := filepath.Join(home, f.dest)
		content, rerr := acmeshFS.ReadFile(f.src)
		if rerr != nil {
			return fmt.Errorf("%w:嵌入文件缺失 %s", ErrAcmeshStart, f.src)
		}
		if existing, rerr := os.ReadFile(dest); rerr == nil && bytes.Equal(existing, content) {
			continue // 已安装且与嵌入版本一致
		}
		mode := os.FileMode(0o600)
		if f.dest == "acme.sh" {
			mode = 0o700
		}
		if werr := os.WriteFile(dest, content, mode); werr != nil {
			return fmt.Errorf("%w:写入 %s 失败(%v)", ErrAcmeshStart, dest, werr)
		}
	}
	if missing := missingDeps(); len(missing) > 0 {
		return fmt.Errorf("%w:控制机缺少 %s(acme.sh 运行需要;控制机须为 Linux/POSIX 环境)",
			ErrAcmeshStart, strings.Join(missing, "、"))
	}
	return nil
}

// missingDeps 返回本地缺失的运行期依赖(经 exec.LookPath 查找)。
func missingDeps() []string {
	var missing []string
	for _, dep := range acmeDeps {
		if _, err := exec.LookPath(dep); err != nil {
			missing = append(missing, dep)
		}
	}
	return missing
}

// inspectEngine 探测本地引擎状态:脚本就位?依赖就绪?版本号?纯本地探测,不触网不触网关。
func (s *service) inspectEngine() (*EngineStatus, error) {
	out := &EngineStatus{}
	if _, err := os.Stat(s.scriptPath()); err == nil {
		out.Installed = true
	}
	out.Ready = len(missingDeps()) == 0
	if out.Installed && out.Ready {
		out.Version = s.acmeVersion()
	}
	return out, nil
}

// acmeVersion best-effort 读 acme.sh 版本(`acme.sh --version` 末行,如 v3.1.1)。
func (s *service) acmeVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := s.runAcmesh(ctx, []string{"--version"}, nil)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 {
		return ""
	}
	return truncate(strings.TrimSpace(lines[len(lines)-1]), 64)
}

// ---------- acme.sh 调用 ----------

// runAcmesh 在本地以参数数组运行 acme.sh(绝不拼 shell);extraEnv 追加到进程环境
// (DNS 凭据走这里,不进命令行/日志)。返回 stdout;非零退出 → ErrIssue(附人话摘要)。
func (s *service) runAcmesh(ctx context.Context, args []string, extraEnv [][2]string) (string, error) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		return "", fmt.Errorf("%w:控制机缺少 sh(acme.sh 需要 POSIX shell;控制机须为 Linux)", ErrAcmeshStart)
	}
	full := append([]string{"--home", s.homeDir()}, args...)
	cmd := exec.CommandContext(ctx, sh, append([]string{s.scriptPath()}, full...)...)
	cmd.Env = os.Environ()
	for _, kv := range extraEnv {
		cmd.Env = append(cmd.Env, kv[0]+"="+kv[1])
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if rerr := cmd.Run(); rerr != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if ctx.Err() == context.DeadlineExceeded {
			detail = "执行超时(ACME/DNS 传播等待过久)"
		}
		return "", fmt.Errorf("%w:%s", ErrIssue, truncate(detail, 800))
	}
	return stdout.String(), nil
}

// issueArgs 构造签发的 acme.sh 参数(纯函数,golden 可测;值均已过白名单校验)。
func issueArgs(c *Certificate, dnsAPI string) []string {
	args := []string{"--issue", "--dns", dnsAPI, "-d", c.PrimaryDomain}
	for _, d := range c.Domains {
		if d == c.PrimaryDomain {
			continue
		}
		args = append(args, "-d", d)
	}
	return append(args,
		"--keylength", c.KeyType,
		"--server", caServerURL[c.CA],
	)
}

// renewArgs 构造续期参数(域配置已持久化在 home 下;--force 由平台续期窗口决定,不让
// acme.sh 自己判断是否到期)。
func renewArgs(c *Certificate) []string {
	return []string{"--renew", "-d", c.PrimaryDomain, "--force"}
}

// runIssue 执行一次签发/续期:已成功签发过 → 续期,否则签发(同一条路径,状态机驱动)。
func (s *service) runIssue(ctx context.Context, c *Certificate, dnsAPI string, dnsEnv [][2]string) error {
	args := renewArgs(c)
	if c.LastIssuedAt.IsZero() {
		args = issueArgs(c, dnsAPI)
	}
	_, err := s.runAcmesh(ctx, args, dnsEnv)
	return err
}

// readBackCert 读回本地签发产物(fullchain.cer + <primary>.key)。
func (s *service) readBackCert(primary string) (string, string, error) {
	dir := filepath.Join(s.homeDir(), primary)
	certPEM, err := os.ReadFile(filepath.Join(dir, "fullchain.cer"))
	if err != nil {
		return "", "", fmt.Errorf("%w:fullchain.cer 读取失败(%v)", ErrReadBack, err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, primary+".key"))
	if err != nil {
		return "", "", fmt.Errorf("%w:私钥读取失败(%v)", ErrReadBack, err)
	}
	cert, key := strings.TrimSpace(string(certPEM)), strings.TrimSpace(string(keyPEM))
	if cert == "" || key == "" {
		return "", "", fmt.Errorf("%w:产物为空(签发可能尚未完成)", ErrReadBack)
	}
	return cert, key, nil
}

// removeFromAcmesh best-effort 从 acme.sh 续期清单移除域名(不向 CA revoke;未安装也静默)。
func (s *service) removeFromAcmesh(c *Certificate) {
	if _, err := os.Stat(s.scriptPath()); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = s.runAcmesh(ctx, []string{"--remove", "-d", c.PrimaryDomain}, nil)
}
