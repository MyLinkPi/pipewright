package registryhub

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/huangchengsir/pipewright/internal/target"
)

// LocalRunner 抽象「在控制机本机执行一条 array 命令」的能力(生产实现基于 os/exec;
// 测试注入 fake,不碰真 docker)。形态与 build.Commander.Run 对齐,但不跨包依赖。
type LocalRunner interface {
	// Run 同步执行 name+args(绝不经 shell 解释;stdin 可空),返回 stdout/stderr/退出码。
	// exitCode<0 表示无法启动(命令不存在等);err 为传输/启动层错误。
	Run(ctx context.Context, name string, args []string, stdin string) (stdout, stderr string, exitCode int, err error)
}

// MachineRunner 抽象「在某台目标机器上执行命令 + 写文件」的能力,是 daemon.json 下发流程的
// 唯一机器通道(远程实现经 target.Service SSH;本机实现经 LocalRunner/os.WriteFile)。
type MachineRunner interface {
	// Exec 在目标机跑 cmd(array;复合命令经 `sh -c` 且脚本为包内常量,参数走位置参,AC-SEC-02)。
	Exec(ctx context.Context, cmd []string) (stdout, stderr string, exitCode int, err error)
	// Upload 把 content 流式写到目标机的 remotePath(远端自动建父目录)。
	Upload(ctx context.Context, content io.Reader, remotePath string) error
}

// RegistryAPI 抽象保留策略所需的制品 registry HTTP API 子集(*Client 实现;测试注入 fake)。
type RegistryAPI interface {
	Catalog(ctx context.Context) ([]string, error)
	TagInfos(ctx context.Context, repo string) ([]TagInfo, error)
	DeleteManifest(ctx context.Context, repo, digest string) error
}

// CertInfo 是证书管理侧的证书摘要(域名覆盖校验用;无 PEM)。
type CertInfo struct {
	ID            string
	PrimaryDomain string
	Domains       []string
	Status        string
	NotAfter      time.Time
}

// CertSource 抽象「证书元数据 + 进程内解密 PEM」的能力(生产由 main.go 适配 certmgmt.Service
// 注入,避免包环;与 platformhttps.CertSource 同构)。PEM 仅进程内传递,绝不过 HTTP/不落日志。
type CertSource interface {
	// GetCert 返回证书摘要;不存在 → not found 语义。
	GetCert(ctx context.Context, id string) (*CertInfo, error)
	// OpenCertPEM 解密返回证书/私钥 PEM 明文(部署时落盘挂载用,仅进程内)。
	OpenCertPEM(ctx context.Context, id string) (certPEM, keyPEM string, err error)
}

// Options 是 Hub 的可注入依赖(零值可用生产默认;测试逐项覆盖)。
type Options struct {
	// BaseDir 是 registry 栈在控制机的落盘目录(compose 文件 + data/cache 卷)。
	// 默认 /var/lib/pipewright/registry(与 install.sh 数据目录对齐)。
	BaseDir string
	// Runner 是控制机本机命令执行器;nil 用 os/exec 默认实现。
	Runner LocalRunner
	// HTTPClient 用于本机探活 registry /v2/;nil 用 3s 超时默认。
	HTTPClient *http.Client
	// RegistryClient 供保留策略访问制品 registry API;nil 按端口自建默认。
	RegistryClient RegistryAPI
	// LocalMachine 覆盖「控制机本机」目标的 MachineRunner(生产 nil → LocalRunner + 文件系统
	// 实现;测试注入 fake,避免碰真实 /etc/docker)。
	LocalMachine MachineRunner
	// Certs 是 TLS 证书来源(certmgmt);nil 时选了证书的保存会被拒绝。
	Certs CertSource
	// ApplyConcurrency 是 daemon.json 批量下发的并发上限;0 → 4。
	ApplyConcurrency int
}

// Hub 组合内置 registry 的全部能力:配置(config.go)、本机栈(stack.go)、
// registry API 客户端(client.go)、保留策略(retention.go)、daemon.json 下发(daemon.go)。
type Hub struct {
	cfg configService

	targetSvc target.Service // 远程机 SSH 通道(下发/巡检);可为 nil(纯本机部署场景)
	opts      Options
}

// New 构造 Hub。db 为配置存储;targetSvc 为远程机 SSH 通道(daemon.json 下发到已登记
// 服务器与巡检用;nil 则仅本机目标可用)。无重活、无 init 副作用。
func New(db *sql.DB, targetSvc target.Service, opts Options) *Hub {
	if opts.BaseDir == "" {
		opts.BaseDir = defaultBaseDir
	}
	if opts.Runner == nil {
		opts.Runner = execRunner{}
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 3 * time.Second}
	}
	if opts.ApplyConcurrency <= 0 {
		opts.ApplyConcurrency = 4
	}
	if opts.LocalMachine == nil {
		opts.LocalMachine = localMachine{runner: opts.Runner}
	}
	return &Hub{
		cfg:       configService{db: db, baseDir: opts.BaseDir, certs: opts.Certs},
		targetSvc: targetSvc,
		opts:      opts,
	}
}

// Get/Save 透传单例配置(config.go;Save 只写库,不部署、不下发)。
func (h *Hub) Get(ctx context.Context) (*Config, error) { return h.cfg.Get(ctx) }
func (h *Hub) Save(ctx context.Context, in SaveInput) (*Config, error) {
	return h.cfg.Save(ctx, in)
}

// ResolveDirs 返回制品/缓存存储的实际生效目录(配置值优先,空回退默认)。
// 供 httpapi 在 DTO 里回显默认路径(UI 占位/提示用),不触网不碰盘。
func (h *Hub) ResolveDirs(cfg *Config) (artifact, cache string) {
	return h.resolveArtifactDir(cfg), h.resolveCacheDir(cfg)
}

// ResolveBuiltin 返回内置制品 registry 地址(构建接入用:环境未绑定外部仓库时的推送兜底;
// main 经 build.WithBuiltinRegistry 注入,build 包不反向依赖本包)。未启用 → ok=false,
// 构建保持旧行为(本地 tag、不推送)。读取失败按未启用处理(构建不因配置库故障而失败)。
func (h *Hub) ResolveBuiltin(ctx context.Context) (addr string, ok bool) {
	cfg, err := h.Get(ctx)
	if err != nil || !cfg.Enabled {
		return "", false
	}
	return cfg.ArtifactAddr(), true
}

// UsesCert 报告内置 registry 是否正引用指定证书(certmgmt 删除前的占用检查)。
func (h *Hub) UsesCert(ctx context.Context, certID string) (bool, error) {
	cfg, err := h.Get(ctx)
	if err != nil {
		return false, err
	}
	return cfg.TLSCertID != "" && cfg.TLSCertID == certID, nil
}

// RedeployCert 证书续期/重下发后的联动:重新部署栈(部署流程会重新解密落盘 PEM 并按
// 配置漂移重建容器)。未引用该证书时静默跳过;部署层失败如实返回(人读,无凭据)。
func (h *Hub) RedeployCert(ctx context.Context, certID string) error {
	cfg, err := h.Get(ctx)
	if err != nil {
		return err
	}
	if cfg.TLSCertID == "" || cfg.TLSCertID != certID {
		return nil
	}
	res, err := h.DeployStack(ctx)
	if err != nil {
		return err
	}
	if !res.OK {
		return fmt.Errorf("registry TLS 证书重新部署失败:%s", res.Error)
	}
	return nil
}
