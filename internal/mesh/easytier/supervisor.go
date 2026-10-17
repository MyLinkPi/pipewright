// Package easytier 托管并观测 EasyTier(去中心化 mesh VPN,WireGuard 加密,打洞优先+中继兜底)。
//
// 集成方式(对 LGPL-3.0 友好的聚合使用,不修改/不链接其代码):
//   - 托管模式:本包定位 easytier-core 二进制(env PIPEWRIGHT_EASYTIER_BIN > <dataDir>/bin >
//     PATH),按 mesh 设置生成参数并作为子进程监督(退出按退避重启;Stop 后不再拉起);
//   - 外部模式:运维自跑 EasyTier,本包只读其 RPC portal(easytier-cli --rpc-portal)。
//
// 状态查询:白名单子进程调用 easytier-cli peer/route/node(--output json),TTL 缓存 +
// stale-while-revalidate;字段解析对 EasyTier 版本差异保持宽容(缺字段降级为空,不整体失败)。
// p2p/中继判定:peer 表的 tunnel 字段(p2p / relay(节点))+ route 表 next_hop 交叉验证。
package easytier

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// CoreBinary / CliBinary 是二进制名(Windows 带后缀)。
func CoreBinary() string { return withExe("easytier-core") }

func CliBinary() string { return withExe("easytier-cli") }

func withExe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// Locate 在 env 覆盖 → dataDir/bin → PATH 中定位二进制;找不到返回空串(调用方给引导提示)。
func Locate(envPath, dataDir, name string) string {
	if p := filepath.Clean(envPath); p != "." && p != "" {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if dataDir != "" {
		p := filepath.Join(dataDir, "bin", name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

// Config 是监督器启动参数(来自 mesh 设置;Secret 为明文,仅进程内使用,绝不落日志)。
type Config struct {
	Mode         string // managed | external
	RPCAddr      string // easytier-cli --rpc-portal 目标(默认 127.0.0.1:15888)
	NetworkName  string
	NetworkSecret string
	VirtualIP    string // 静态虚拟 IP;空 = --dhcp 自动分配
	Hostname     string // easytier --hostname(展示名)
	Peers        []string
	DataDir      string // 二进制定位目录(<data>/bin)
	CorePath     string // 已定位的 easytier-core(托管模式必填)
	CliPath      string // 已定位的 easytier-cli
}

// Supervisor 是托管模式子进程监督器。零值不可用,经 New 构造;线程安全。
type Supervisor struct {
	cfg Config

	mu       sync.Mutex
	cmd      *exec.Cmd
	stopped  bool
	lastErr  string
	startedAt time.Time
	restarts int
}

// New 构造监督器(不启动;Start 拉起)。
func New(cfg Config) *Supervisor {
	if cfg.RPCAddr == "" {
		cfg.RPCAddr = "127.0.0.1:15888"
	}
	return &Supervisor{cfg: cfg}
}

// args 生成 easytier-core 参数(固定 RPC portal 端口便于本机状态查询)。
func (c Config) args() []string {
	args := []string{
		"--network-name", c.NetworkName,
		"--rpc-portal", c.RPCAddr,
		"--hostname", c.Hostname,
	}
	if c.NetworkSecret != "" {
		args = append(args, "--network-secret", c.NetworkSecret)
	}
	if c.VirtualIP != "" {
		args = append(args, "-i", c.VirtualIP)
	} else {
		args = append(args, "--dhcp")
	}
	for _, p := range c.Peers {
		if p != "" {
			args = append(args, "-p", p)
		}
	}
	return args
}

// Start 拉起子进程并进入监督循环(退出按退避重启;ctx 取消或 Stop 后退出循环)。
// 运行在独立 goroutine;Start 本身只在参数明显非法时同步报错。
func (s *Supervisor) Start(ctx context.Context) error {
	if s.cfg.Mode != "managed" {
		return fmt.Errorf("easytier: supervisor only manages managed mode")
	}
	if s.cfg.CorePath == "" {
		return fmt.Errorf("easytier: easytier-core binary not found (set PIPEWRIGHT_EASYTIER_BIN or place %s under <data>/bin)", CoreBinary())
	}
	if s.cfg.NetworkName == "" {
		return fmt.Errorf("easytier: network name required")
	}
	go s.supervise(ctx)
	return nil
}

// supervise 是监督循环:启动 → 等待退出 → 指数退避重启(stopped 后不再拉起)。
func (s *Supervisor) supervise(ctx context.Context) {
	backoff := 2 * time.Second
	for {
		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			return
		}
		cmd := exec.CommandContext(ctx, s.cfg.CorePath, s.cfg.args()...)
		// 密钥经 argv 传递是 EasyTier 的既定接口;输出里含密钥的概率低,但为稳妥丢弃子进程
		// stdout/stderr 的内容,只在内部记「退出码/时刻」,不透传到平台日志。
		cmd.Stdout = nil
		cmd.Stderr = nil
		s.cmd = cmd
		s.startedAt = time.Now()
		s.mu.Unlock()

		err := cmd.Run()

		s.mu.Lock()
		s.cmd = nil
		if err != nil {
			s.lastErr = err.Error()
			s.restarts++
		} else {
			s.lastErr = ""
		}
		stopped := s.stopped
		s.mu.Unlock()

		if stopped || ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// Stop 停止子进程并不再拉起(配置变更后由装配层重建监督器)。
func (s *Supervisor) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(os.Interrupt)
		// 优雅退出窗口;超时强杀。
		go func(p *os.Process) {
			time.Sleep(5 * time.Second)
			_ = p.Kill()
		}(s.cmd.Process)
	}
}

// Runtime 是托管进程的运行快照(状态页展示)。
type Runtime struct {
	Running   bool      `json:"running"`
	LastErr   string    `json:"lastErr"`
	Restarts  int       `json:"restarts"`
	StartedAt time.Time `json:"startedAt"`
}

// Snapshot 返回监督器运行快照(不触碰子进程)。
func (s *Supervisor) Snapshot() Runtime {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Runtime{
		Running:   s.cmd != nil,
		LastErr:   s.lastErr,
		Restarts:  s.restarts,
		StartedAt: s.startedAt,
	}
}
