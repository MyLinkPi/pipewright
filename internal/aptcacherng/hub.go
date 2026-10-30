package aptcacherng

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
// 测试注入 fake,不碰真 docker)。形态与 registryhub.LocalRunner 对齐,但不跨包依赖。
type LocalRunner interface {
	// Run 同步执行 name+args(绝不经 shell 解释;stdin 可空),返回 stdout/stderr/退出码。
	// exitCode<0 表示无法启动(命令不存在等);err 为传输/启动层错误。
	Run(ctx context.Context, name string, args []string, stdin string) (stdout, stderr string, exitCode int, err error)
}

// MachineRunner 抽象「在某台目标机器上执行命令 + 写文件」的能力,是 apt 代理片段下发
// 流程的唯一机器通道(远程实现经 target.Service SSH;本机实现经 LocalRunner/os.WriteFile)。
type MachineRunner interface {
	// Exec 在目标机跑 cmd(array;复合命令经 `sh -c` 且脚本为包内常量,参数走位置参,AC-SEC-02)。
	Exec(ctx context.Context, cmd []string) (stdout, stderr string, exitCode int, err error)
	// Upload 把 content 流式写到目标机的 remotePath(远端自动建父目录)。
	Upload(ctx context.Context, content io.Reader, remotePath string) error
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
// 注入,避免包环;与 registryhub.CertSource 同构)。PEM 仅进程内传递,绝不过 HTTP/不落日志。
type CertSource interface {
	// GetCert 返回证书摘要;不存在 → not found 语义。
	GetCert(ctx context.Context, id string) (*CertInfo, error)
	// OpenCertPEM 解密返回证书/私钥 PEM 明文(部署时落盘挂载用,仅进程内)。
	OpenCertPEM(ctx context.Context, id string) (certPEM, keyPEM string, err error)
}

// Options 是 Hub 的可注入依赖(零值可用生产默认;测试逐项覆盖)。
type Options struct {
	// BaseDir 是 apt 缓存栈在控制机的落盘目录(compose 文件 + nginx conf + cache 卷)。
	// 默认 /var/lib/pipewright/apt-cacher-ng(与 install.sh 数据目录对齐)。
	BaseDir string
	// Runner 是控制机本机命令执行器;nil 用 os/exec 默认实现。
	Runner LocalRunner
	// HTTPClient 用于本机探活代理端口;nil 用 3s 超时默认。
	HTTPClient *http.Client
	// LocalMachine 覆盖「控制机本机」目标的 MachineRunner(生产 nil → LocalRunner + 文件系统
	// 实现;测试注入 fake,避免碰真实 /etc/apt)。
	LocalMachine MachineRunner
	// Certs 是 TLS 证书来源(certmgmt);nil 时选了证书的保存会被拒绝。
	Certs CertSource
	// ApplyConcurrency 是代理片段批量下发的并发上限;0 → 4。
	ApplyConcurrency int
}

// Hub 组合内置 apt 缓存的全部能力:配置(config.go)、本机栈(stack.go)、
// 代理片段下发/巡检(proxy.go)。
type Hub struct {
	cfg configService

	targetSvc target.Service // 远程机 SSH 通道(下发/巡检);可为 nil(纯本机部署场景)
	opts      Options
}

// New 构造 Hub。db 为配置存储;targetSvc 为远程机 SSH 通道(代理片段下发到已登记
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

// ResolveDir 返回缓存存储的实际生效目录(配置值优先,空回退默认)。
// 供 httpapi 在 DTO 里回显默认路径(UI 占位/提示用),不触网不碰盘。
func (h *Hub) ResolveDir(cfg *Config) string { return h.resolveCacheDir(cfg) }

// UsesCert 报告内置 apt 缓存是否正引用指定证书(certmgmt 删除前的占用检查)。
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
		return fmt.Errorf("apt 缓存 TLS 证书重新部署失败:%s", res.Error)
	}
	return nil
}
