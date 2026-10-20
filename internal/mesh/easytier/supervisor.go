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
	"strconv"
	"strings"
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

// Locate 在 env 覆盖 → dataDir/bin → exe 同级 bin(发行包随附) → PATH 中定位二进制;
// 找不到返回空串(调用方给引导提示)。
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
	// 发行包布局:pwclient.exe 与 bin/easytier-*.exe 同级(免首连下载)。
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "bin", name)
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
	Mode          string // managed | external
	RPCAddr       string // easytier-cli --rpc-portal 目标(默认 127.0.0.1:15888)
	NetworkName   string
	NetworkSecret string
	VirtualIP     string // 静态虚拟 IP;空 = --dhcp 自动分配
	Hostname      string // easytier --hostname(展示名)
	Peers         []string
	// ProxyNetworks 是子网代理通告列表(cidr;跨环境转发:本端网关 IP 的 /32,或管理员
	// 自定义的可达网段)。非空要求二进制支持子网代理参数(v2.1+),不支持则 Start 报错
	// (上层记录并提示升级)。通告后 EasyTier 默认启用内置用户态 NAT:被代理网段内的设备
	// 无需回程路由即可返程(官方 point-to-networking 文档),对端(含客户端)自动获得路由。
	ProxyNetworks []string
	// ManualRoutes 手工限定本节点路由进虚拟网络的网段(cidr)。语义由 StrictManualRoutes
	// 决定:非空列表恒为「仅路由列表内网段」(--manual-routes 逐值传,禁用广播路由)。
	// 客户端(pwclient)用它实现「CIDR 自由配置」;平台节点通常留空且不设 Strict。
	ManualRoutes []string
	// StrictManualRoutes 声明空 ManualRoutes 的语义(安全默认 fail-closed 的开关):
	// true = 空列表也传裸 --manual-routes(值可选,num_args=0..)→ 拒绝全部子网代理
	// 通告,仅虚拟网互通(pwclient 语义:留空 = 什么都不路由);
	// false(默认)= 空列表不传参 → 接受全部通告(平台中枢节点语义)。
	StrictManualRoutes bool
	// Socks5Port 是本地 SOCKS5 服务端口(0 = 关闭)。开启后本机 SOCKS5 客户端(浏览器/
	// 终端/Clash 等)经此端口访问虚拟网络 —— 拨号目标按虚拟路由表转发,含经子网代理
	// NAT 的远端网段;不动系统路由,与其他流量互不影响。注意 EasyTier 侧无认证,仅本机
	// 使用(客户端侧需防火墙阻断非本机访问)。
	Socks5Port int
	// ExitNodes 是出口节点虚拟 IP 列表(序即优先级;空 = 不设)。设置后本机经虚拟网
	// 转发的外来流量(典型:本地 SOCKS5 拨号到非虚拟网目标)经所选节点 NAT 出网;
	// 目标节点需以 EnableExitNode 允许充当出口(EasyTier 以 network secret 校验接入)。
	ExitNodes []string
	// EnableExitNode 允许本节点充当其他节点的出口(foreign network NAT;默认关,
	// 平台节点显式开启)。
	EnableExitNode bool
	// NetworkDir 是该实例的网络数据目录(--network-directory;实例身份/密钥落此)。
	// 同机多实例(pwclient 多出口代理)必须各给各的目录,否则共享 instance-id 冲突;
	// 空默认。除旗子外同时设为子进程 cwd,兼容以工作目录存放数据的旧版二进制。
	NetworkDir string
	DataDir    string // 二进制定位目录(<data>/bin)
	CorePath   string // 已定位的 easytier-core(托管模式必填)
	CliPath    string // 已定位的 easytier-cli
}

// Supervisor 是托管模式子进程监督器。零值不可用,经 New 构造;线程安全。
type Supervisor struct {
	cfg Config
	// done 在监督循环退出(子进程已收尸)时关闭;StopAndWait 等待用。
	done     chan struct{}
	doneOnce sync.Once

	mu        sync.Mutex
	cmd       *exec.Cmd
	stopped   bool
	lastErr   string
	startedAt time.Time
	restarts  int
}

// New 构造监督器(不启动;Start 拉起)。
func New(cfg Config) *Supervisor {
	if cfg.RPCAddr == "" {
		cfg.RPCAddr = "127.0.0.1:15888"
	}
	return &Supervisor{cfg: cfg, done: make(chan struct{})}
}

// args 生成 easytier-core 参数(固定 RPC portal 端口便于本机状态查询)。
func (c Config) args() []string {
	return buildArgs(c, probeHelp(c.CorePath))
}

// buildArgs 是 args 的纯函数形态(flags 为 --help 探测出的旗子集合;单测直接构造,
// 不触真机二进制)。
func buildArgs(c Config, flags helpFlags) []string {
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
	// 子网代理:官方 CLI 参数为复数 --proxy-networks(-n),历史上曾有单数 --proxy-network
	// 形态 —— 以 --help 探测结果为准传参。逐值重复传(clap Vec 追加语义,兼容是否配置
	// value_delimiter 的两种实现)。
	for _, n := range c.ProxyNetworks {
		if n != "" && flags.proxyNetwork != "" {
			args = append(args, flags.proxyNetwork, n)
		}
	}
	// 手工路由:同上逐值重复传。空列表默认不传参(接受全部通告,平台中枢语义);
	// StrictManualRoutes时空列表传裸旗子 —— 空值可选,= 拒绝全部子网代理通告(fail-closed)。
	if flags.manualRoutes {
		switch {
		case len(c.ManualRoutes) > 0:
			for _, r := range c.ManualRoutes {
				if r != "" {
					args = append(args, "--manual-routes", r)
				}
			}
		case c.StrictManualRoutes:
			args = append(args, "--manual-routes")
		}
	}
	// 本地 SOCKS5 服务(端口>0 且二进制支持)。
	if c.Socks5Port > 0 && flags.socks5 {
		args = append(args, "--socks5", strconv.Itoa(c.Socks5Port))
	}
	// 出口节点:逐值重复传(序即优先级);目标节点需 --enable-exit-node 才会承接。
	if len(c.ExitNodes) > 0 && flags.exitNodes {
		for _, ip := range c.ExitNodes {
			if ip != "" {
				args = append(args, "--exit-nodes", ip)
			}
		}
	}
	if c.EnableExitNode && flags.enableExitNode {
		args = append(args, "--enable-exit-node", "true")
	}
	// 实例数据目录:同机多实例隔离身份必需;不支持该旗子的二进制仅靠 cwd 兜底。
	if c.NetworkDir != "" && flags.networkDir {
		args = append(args, "--network-directory", c.NetworkDir)
	}
	return args
}

// probeCache 缓存 --help 探测结果(--help 进程输出 → 目标旗子集合;进程内每路径一次)。
var probeCache sync.Map // corePath → helpFlags

// helpFlags 是一次 --help 探测解析出的旗子集合(探测失败为空集,按不支持,保守)。
type helpFlags struct {
	proxyNetwork   string // "--proxy-networks" | "--proxy-network" | ""
	manualRoutes   bool
	exitNodes      bool
	enableExitNode bool
	socks5         bool
	networkDir     bool
}

// probeHelp 以 5s 超时跑一次 --help 并解析旗子(结果经 probeCache 缓存,每路径只跑一次)。
func probeHelp(corePath string) helpFlags {
	if corePath == "" {
		return helpFlags{}
	}
	if v, ok := probeCache.Load(corePath); ok {
		return v.(helpFlags)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, corePath, "--help").CombinedOutput()
	flags := parseHelpFlags(string(out), err == nil)
	probeCache.Store(corePath, flags)
	return flags
}

// parseHelpFlags 是 --help 输出 → 旗子集合的纯函数(子网代理优先匹配复数官方形态,
// 兼容历史单数;manual-routes/socks5 按子串匹配)。
func parseHelpFlags(out string, ok bool) helpFlags {
	if !ok {
		return helpFlags{}
	}
	flags := helpFlags{}
	switch {
	case strings.Contains(out, "--proxy-networks"):
		flags.proxyNetwork = "--proxy-networks"
	case strings.Contains(out, "--proxy-network"):
		flags.proxyNetwork = "--proxy-network"
	}
	flags.manualRoutes = strings.Contains(out, "--manual-routes")
	flags.socks5 = strings.Contains(out, "--socks5")
	flags.exitNodes = strings.Contains(out, "--exit-nodes")
	flags.enableExitNode = strings.Contains(out, "--enable-exit-node")
	flags.networkDir = strings.Contains(out, "--network-directory")
	return flags
}

// proxyNetworkFlag 返回该二进制实际支持的子网代理参数名(不支持/无法探测返回空串)。
func proxyNetworkFlag(corePath string) string { return probeHelp(corePath).proxyNetwork }

// SupportsProxyNetwork 探测 easytier-core 是否提供子网代理参数(v2.1+)。
func SupportsProxyNetwork(corePath string) bool { return proxyNetworkFlag(corePath) != "" }

// SupportsManualRoutes 探测 easytier-core 是否提供 --manual-routes 参数。
func SupportsManualRoutes(corePath string) bool { return probeHelp(corePath).manualRoutes }

// SupportsSocks5 探测 easytier-core 是否提供 --socks5 参数。
func SupportsSocks5(corePath string) bool { return probeHelp(corePath).socks5 }

// SupportsExitNodes 探测是否提供 --exit-nodes(出口节点;虚拟 IP 列表,序即优先级)。
func SupportsExitNodes(corePath string) bool { return probeHelp(corePath).exitNodes }

// SupportsEnableExitNode 探测是否提供 --enable-exit-node(允许本节点充当出口)。
func SupportsEnableExitNode(corePath string) bool { return probeHelp(corePath).enableExitNode }

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
	if len(s.cfg.ProxyNetworks) > 0 && !SupportsProxyNetwork(s.cfg.CorePath) {
		return fmt.Errorf("easytier: %s 不支持子网代理参数 --proxy-networks(需 v2.1+),无法通告子网(跨环境转发/自定义网段不可用,请升级二进制)", CoreBinary())
	}
	if (len(s.cfg.ManualRoutes) > 0 || s.cfg.StrictManualRoutes) && !SupportsManualRoutes(s.cfg.CorePath) {
		return fmt.Errorf("easytier: %s 不支持 --manual-routes,无法限定路由网段(留空=不路由亦需该参数;请升级二进制)", CoreBinary())
	}
	if len(s.cfg.ExitNodes) > 0 && !SupportsExitNodes(s.cfg.CorePath) {
		return fmt.Errorf("easytier: %s 不支持 --exit-nodes,无法配置出口节点(请升级二进制)", CoreBinary())
	}
	if s.cfg.EnableExitNode && !SupportsEnableExitNode(s.cfg.CorePath) {
		return fmt.Errorf("easytier: %s 不支持 --enable-exit-node,无法充当出口节点(请升级二进制)", CoreBinary())
	}
	if s.cfg.Socks5Port > 0 && !SupportsSocks5(s.cfg.CorePath) {
		return fmt.Errorf("easytier: %s 不支持 --socks5,无法开启本地 SOCKS5 服务(请升级二进制)", CoreBinary())
	}
	if s.cfg.Socks5Port < 0 || s.cfg.Socks5Port > 65535 {
		return fmt.Errorf("easytier: socks5 端口 %d 超出范围(1-65535)", s.cfg.Socks5Port)
	}
	go s.supervise(ctx)
	return nil
}

// supervise 是监督循环:启动 → 等待退出 → 指数退避重启(stopped 后不再拉起)。
func (s *Supervisor) supervise(ctx context.Context) {
	defer s.doneOnce.Do(func() { close(s.done) })
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
		if s.cfg.NetworkDir != "" {
			// 实例身份隔离:显式数据目录(旧版二进制无旗子时以 cwd 兜底)。
			_ = os.MkdirAll(s.cfg.NetworkDir, 0o755)
			cmd.Dir = s.cfg.NetworkDir
		}
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

// StopAndWait 停止子进程并**等待进程真正退出**(优雅窗口内未退则强杀再等)。
// re-exec 自更新前必须走这里:syscall.Exec 不执行清理,不等退出会把子进程
// 留成孤儿,占着监听/rpc-portal 端口让新进程起不来(v1.6.4 事故)。
func (s *Supervisor) StopAndWait(grace time.Duration) {
	s.Stop()
	select {
	case <-s.done:
		return
	case <-time.After(grace):
	}
	s.mu.Lock()
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	s.mu.Unlock()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second): // 理论不可达:SIGKILL 后 cmd.Run 必返回
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
