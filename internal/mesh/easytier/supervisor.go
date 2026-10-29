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
	"log"
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

// Config 是监督器启动参数(来自 mesh 设置;Secret 为明文,仅进程内使用,落日志前
// 须经 maskArgs/sensitive 打码,见 supervisor_log.go)。
type Config struct {
	Mode          string // managed | external
	RPCAddr       string // easytier-cli --rpc-portal 目标(默认 127.0.0.1:15888)
	NetworkName   string
	NetworkSecret string
	// Credential 是 EasyTier 临时凭据(--credential,base64 私钥):临时设备入网的
	// 认证方式,隐含 Secure Mode(凭据走 Noise XX 静态密钥校验)。与 NetworkSecret
	// 互斥:非空时优先凭据、不再传 --network-secret(凭据设备本就不持有组网密钥)。
	Credential string
	VirtualIP  string // 静态虚拟 IP;空 = --dhcp 自动分配
	Hostname   string // easytier --hostname(展示名)
	Peers      []string
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
	// SecureMode 启用 EasyTier 安全模式(v2.6.0+):节点间 Noise XX 握手认证,并接受
	// 管理节点签发的临时凭据入网(临时凭据签发/验证的前提)。network_secret 持有者
	// 在该模式下照常接入(两套身份并存)。全集群经统一开关协议同时翻转,避免混跑分裂。
	SecureMode bool
	// CredentialFile 是凭据持久化文件路径(--credential-file):管理节点重启后保留
	// 已签发凭据(不配则重启全失效)。签发能力的硬前提,SecureMode 开启时必配。
	CredentialFile string
	// NetworkDir 是该实例的工作目录(子进程 cwd)。注:v2.6.4 已移除 --network-directory
	// 旗子,实例身份改为每进程随机生成、不再落盘(2026-10-27 双实例实测确认);目录仅
	// 用于隔离任何以 cwd 相对路径写数据的行为,身份隔离由"每进程独立身份"天然保证。
	NetworkDir string
	DataDir    string // 二进制定位目录(<data>/bin)
	CorePath   string // 已定位的 easytier-core(托管模式必填)
	CliPath    string // 已定位的 easytier-cli
}

// Supervisor 是托管模式子进程监督器。零值不可用,经 New / NewAdopted 构造;线程安全。
type Supervisor struct {
	cfg Config
	// done 在监督循环退出(子进程已收尸)时关闭。
	done     chan struct{}
	doneOnce sync.Once

	// adoptedPID 非 0 = 收养态:沿用宿主升级/重启遗留的外部 easytier-core(参数
	// 与期望完全一致,见 adopt.go),不拉子进程、无监督循环;失联回收由上层对账
	// 节拍探 rpc-portal 负责(supervise 的退避重启只作用于自己拉起的子进程)。
	adoptedPID int
	adoptedAt  time.Time

	mu sync.Mutex
	// cmd/childExited/lastPID 全部由 mu 保护;托管采 Start→Wait 拆分,
	// 保证 cmd.Process 的写入(Start 内)也在持 mu 时完成(若跨 cmd.Run/Wait
	// 持锁,Stop 将无法取锁发信号;不持锁则 Stop/快照与 Start 内部写竞态)。
	cmd         *exec.Cmd
	lastPID     int           // 最近一次子进程 PID(Stop 兜底强杀与测试用)
	childExited chan struct{} // 最近一次托管子进程已被回收(Run 返回)时关闭;收养态恒已关
	stopped     bool
	lastErr     string
	startedAt   time.Time
	restarts    int

	// argsFn 生成启动参数(恒为 cfg.args;留作测试钩子,验证 Stop 对不响应
	// 温和信号的子进程兜底强杀)。nil 回落 cfg.args。
	argsFn func() []string
}

// New 构造监督器(不启动;Start 拉起)。
func New(cfg Config) *Supervisor {
	if cfg.RPCAddr == "" {
		cfg.RPCAddr = "127.0.0.1:15888"
	}
	exited := make(chan struct{})
	close(exited) // 未启动过子进程:视为已退出,Stop 不等待
	return &Supervisor{cfg: cfg, done: make(chan struct{}), childExited: exited}
}

// NewAdopted 构造收养态监督器:沿用既有 easytier-core(升级/服务重启遗留的孤儿,
// 参数已经 SameArgs 比对一致),不拉子进程。状态查询本就经 rpc-portal 走 cli,
// 与父子关系无关,收养期间一切观测照常。
func NewAdopted(cfg Config, pid int) *Supervisor {
	if cfg.RPCAddr == "" {
		cfg.RPCAddr = "127.0.0.1:15888"
	}
	return &Supervisor{cfg: cfg, done: make(chan struct{}), adoptedPID: pid, adoptedAt: time.Now()}
}

// args 生成 easytier-core 参数(固定 RPC portal 端口便于本机状态查询)。
func (c Config) args() []string {
	return buildArgs(c, probeHelp(c.CorePath))
}

// args 取启动参数(测试可经 argsFn 注入;生产恒为 cfg.args)。
func (s *Supervisor) args() []string {
	if s.argsFn != nil {
		return s.argsFn()
	}
	return s.cfg.args()
}

// Args 导出该配置将生成的 easytier-core 参数(收养比对:与孤儿进程命令行逐旗子
// 比对,一致则沿用不重启;见 adopt.go)。
func (c Config) Args() []string { return c.args() }

// buildArgs 是 args 的纯函数形态(flags 为 --help 探测出的旗子集合;单测直接构造,
// 不触真机二进制)。
func buildArgs(c Config, flags helpFlags) []string {
	args := []string{
		"--network-name", c.NetworkName,
		"--rpc-portal", c.RPCAddr,
		"--hostname", c.Hostname,
	}
	// 私有模式:拒绝为陌生网络(他网)中继流量 —— 公网可达节点不设防时可被任意第三方
	// EasyTier 指为免费中继(耗带宽、流量责任在本节点)。本集群只承载自身网络,无任何
	// 外来中继需求,故无条件启用且不设开关(安全默认;旗子自 v2.3.1 存在,全量二进制
	// ≥ v2.6.4 均支持,不走 --help 探测)。注意 EasyTier 的布尔旗子要求显式带值
	// (<PRIVATE_MODE>,possible values true/false),裸旗子会被 clap 拒绝导致进程秒退
	// (与 --enable-exit-node true 同款约定)。
	args = append(args, "--private-mode", "true")
	// 认证二选一:临时凭据优先(隐含 secure mode,官方入网示例即显式带 --secure-mode);
	// 无凭据才走组网密钥(平台节点/pwclient 的长期成员形态)。
	if c.Credential != "" {
		args = append(args, "--secure-mode", "true", "--credential", c.Credential)
	} else if c.NetworkSecret != "" {
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
	// Secure Mode:布尔旗子可选带值,按仓库惯例显式 true(与 --enable-exit-node 同款)。
	// 凭据路径已在上方随 --credential 传过,不重复。
	if c.SecureMode && c.Credential == "" {
		args = append(args, "--secure-mode", "true")
	}
	// 凭据持久化:管理节点重启后保留已签发凭据的落盘文件(签发能力硬前提)。
	if c.CredentialFile != "" {
		args = append(args, "--credential-file", c.CredentialFile)
	}
	return args
}

// probeCache 缓存 --help 探测结果(--help 进程输出 → 目标旗子集合;进程内每路径一次)。
var probeCache sync.Map // corePath → helpFlags

// helpFlags 是一次 --help 探测解析出的旗子集合(探测失败为空集,按不支持,保守)。
// 注:networkDir 已删 —— v2.6.4 移除了 --network-directory,该旗子永不应再传。
type helpFlags struct {
	proxyNetwork   string // "--proxy-networks" | "--proxy-network" | ""
	manualRoutes   bool
	exitNodes      bool
	enableExitNode bool
	socks5         bool
	// secureMode 探测 --secure-mode(v2.6.0+)。服务端已开启 Secure Mode 时客户端也必须
	// 同模式;旧二进制传该旗子会被 clap 拒绝并秒退,须提前给出可读错误。
	secureMode bool
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
	flags.secureMode = strings.Contains(out, "--secure-mode")
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

// SupportsSecureMode 探测 easytier-core 是否提供 --secure-mode(需 v2.6.0+)。
func SupportsSecureMode(corePath string) bool { return probeHelp(corePath).secureMode }

// Start 拉起子进程并进入监督循环(退出按退避重启;ctx 取消或 Stop 后退出循环)。
// 运行在独立 goroutine;Start 本身只在参数明显非法时同步报错。
func (s *Supervisor) Start(ctx context.Context) error {
	if s.cfg.Mode != "managed" {
		return fmt.Errorf("easytier: supervisor only manages managed mode")
	}
	if s.adoptedPID != 0 {
		return fmt.Errorf("easytier: adopted supervisor cannot start a child process")
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
	if (s.cfg.SecureMode || s.cfg.Credential != "") && !SupportsSecureMode(s.cfg.CorePath) {
		return fmt.Errorf("easytier: %s 不支持 --secure-mode(需 v2.6.0+),无法与服务端 Secure Mode 对齐;请升级/删除旧二进制后重试", CoreBinary())
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
		cmd := exec.CommandContext(ctx, s.cfg.CorePath, s.args()...)
		// 完整命令行与子进程输出都进平台日志;密钥/凭据由 maskArgs/sensitive 打码
		// 后再落日志(仍不写入任何配置/状态文件,见 Config 注释)。
		log.Printf("easytier: 启动 %s", strings.Join(maskArgs(s.cfg, cmd.Args), " "))
		outPipe, err := logPipe(cmd, "stdout", s.cfg)
		if err != nil {
			log.Printf("easytier: %v(该路输出不进日志)", err)
		}
		errPipe, err := logPipe(cmd, "stderr", s.cfg)
		if err != nil {
			log.Printf("easytier: %v(该路输出不进日志)", err)
		}
		if s.cfg.NetworkDir != "" {
			// 实例身份隔离:显式数据目录(旧版二进制无旗子时以 cwd 兜底)。
			_ = os.MkdirAll(s.cfg.NetworkDir, 0o755)
			cmd.Dir = s.cfg.NetworkDir
		}
		// 先置新 childExited 再启动:Stop 任何时刻取锁都能见到与当前 cmd 匹配的通道。
		s.childExited = make(chan struct{})
		s.cmd = cmd
		s.startedAt = time.Now()
		err = cmd.Start() // cmd.Process 的写入在此、在持 mu 时完成
		// 子进程已继承管道写端(或 Start 失败、根本未创建),本端副本此时一律可关;
		// 不关会占住写端让读不到 EOF,扫日志协程挂死。
		outPipe.closeWrite()
		errPipe.closeWrite()
		if err != nil {
			s.cmd = nil
			s.lastErr = err.Error()
			s.restarts++
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		s.lastPID = cmd.Process.Pid
		s.mu.Unlock()

		err = cmd.Wait()

		s.mu.Lock()
		s.cmd = nil
		close(s.childExited) // 置于 Stop 取锁之前:等待方拿锁时必能见到已关闭的通道
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

// stopReapGrace 是 Stop 温和信号后等待子进程退出的上限,超时后强杀并再等回收;
// 变量化供测试缩短等待。
var stopReapGrace = 8 * time.Second

// Stop 停止托管对象并不再拉起(配置变更后由装配层重建监督器)。
// 返回时保证子进程已退出并被回收(温和信号 → 限时等待 → 兜底强杀):调用方
// (如升级替换 easytier 二进制、服务交回 SCM)据此可安全操作被占用的文件/端口。
func (s *Supervisor) Stop() {
	s.mu.Lock()
	s.stopped = true
	if s.adoptedPID != 0 {
		// 收养对象非子进程:无法优雅信号线,直接终止。调用场景(设置停用/
		// 参数变更/失联回收)均以停止为目的。找不到(已死)视为已完成。
		if p, err := os.FindProcess(s.adoptedPID); err == nil {
			_ = p.Kill()
		}
		s.mu.Unlock()
		return
	}
	var proc *os.Process
	var exited chan struct{}
	if s.cmd != nil && s.cmd.Process != nil {
		// 子进程仍存活(childExited 未关)才发温和信号(Windows 无进程间信号,
		// stopSignal 内部直接强杀);已退出的旧 cmd 引用不再碰。
		select {
		case <-s.childExited:
		default:
			proc = s.cmd.Process
			_ = stopSignal(proc)
			exited = s.childExited
		}
	}
	s.mu.Unlock()
	if exited == nil {
		// 无在管子进程(未启动/已回收):直接返回。
		return
	}
	select {
	case <-exited:
	case <-time.After(stopReapGrace):
		// 子进程忽略温和信号:兜底强杀并再等回收。不能只等超时就直接返回 ——
		// 那样违背「返回时已回收」的契约,遗留无人监管的孤儿占用二进制/端口。
		// (exec.CommandContext 的强杀不可依赖:宿主主进程退出后其 kill goroutine 已不存在。)
		if proc == nil {
			if p, err := os.FindProcess(s.PID()); err == nil {
				proc = p
			}
		}
		if proc != nil {
			_ = proc.Kill()
		}
		select {
		case <-exited:
		case <-time.After(stopReapGrace):
			if proc != nil {
				log.Printf("easytier: 子进程 %d 强制终止后仍未回收(可能仍在退出中)", proc.Pid)
			}
		}
	}
}

// PID 返回最近一次托管子进程的 PID(0 = 从未启动)。
func (s *Supervisor) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastPID
}

// Runtime 是托管进程的运行快照(状态页展示)。
type Runtime struct {
	Running   bool      `json:"running"`
	LastErr   string    `json:"lastErr"`
	Restarts  int       `json:"restarts"`
	StartedAt time.Time `json:"startedAt"`
	// Adopted 表示当前沿用的是宿主升级/重启遗留的外部进程(零中断收养),非本
	// 进程拉起的子进程;失联由对账节拍回收重拉。
	Adopted bool `json:"adopted"`
}

// Snapshot 返回监督器运行快照(不触碰子进程;收养态 Running 按收养关系报告,
// 存活探测由上层对账节拍经 rpc-portal 负责)。
func (s *Supervisor) Snapshot() Runtime {
	s.mu.Lock()
	defer s.mu.Unlock()
	startedAt := s.startedAt
	if s.adoptedPID != 0 {
		startedAt = s.adoptedAt
	}
	return Runtime{
		Running:   s.cmd != nil || s.adoptedPID != 0,
		LastErr:   s.lastErr,
		Restarts:  s.restarts,
		StartedAt: startedAt,
		Adopted:   s.adoptedPID != 0,
	}
}
