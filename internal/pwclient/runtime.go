// runtime.go 是客户端运行时:托管 easytier-core 子进程组网(对账循环随配置变化重启)、
// 二进制定位与缺失自动下载、状态快照(经 IPC 供托盘 GUI 消费)。GUI 无关,可单测。
package pwclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/huangchengsir/pipewright/internal/mesh/easytier"
	"github.com/huangchengsir/pipewright/internal/version"
)

// reconcileInterval 是对账节拍(与平台 meshRuntime 一致量级;配置变化经文件写直达,
// 节拍只决定「多久注意到」,重启只在指纹变化时发生)。
const reconcileInterval = 10 * time.Second

// downloadRetryBackoff 是二进制下载失败后的自动重试退避(手动触发不受限)。
const downloadRetryBackoff = 10 * time.Minute

// Runtime 是客户端核心运行时。零值不可用,经 NewRuntime 构造;线程安全。
type Runtime struct {
	store   *Store
	dataDir string

	// newDownloader 由装配层注入(测试可替换);nil = 禁用自动下载。
	newDownloader func() *easytier.Downloader

	mu            sync.Mutex
	supervisor    *easytier.Supervisor
	status        *easytier.StatusProvider
	supervisorKey string
	failedKey     string
	corePath      string
	cliPath       string
	rpcAddr       string
	dlBusy        bool
	dlErr         string
	dlVersion     string
	dlRetryAt     time.Time
	fwEnsured     bool
	// socks5GuardPort 记录当前 SOCKS5 阻断规则覆盖的端口(0 = 无规则;对账幂等用)。
	socks5GuardPort int
	// svc 是装配层注入的服务进程信息(IPC 状态展示;console 直跑时 RunningAs=console)。
	svc ServiceInfo
	// logs 是进程内日志环(GUI「日志」尾部展示)。
	logs *logRing
}

// NewRuntime 构造运行时(dataDir 是配置/二进制/日志的根目录)。不启动;Start 拉起对账循环。
func NewRuntime(store *Store, dataDir string) *Runtime {
	return &Runtime{store: store, dataDir: dataDir, newDownloader: defaultDownloader(dataDir), logs: newLogRing(500)}
}

// SetServiceInfo 注入服务进程信息(Snapshot 的 service 字段)。
func (rt *Runtime) SetServiceInfo(info ServiceInfo) {
	rt.mu.Lock()
	rt.svc = info
	rt.mu.Unlock()
}

// LogWriter 返回日志环的写入端(装配层设为 log.Output 的目标之一)。
func (rt *Runtime) LogWriter() *logRing { return rt.logs }

func defaultDownloader(dataDir string) func() *easytier.Downloader {
	return func() *easytier.Downloader {
		return &easytier.Downloader{
			// 升级源:env PIPEWRIGHT_RELEASE_MIRROR > GitHub 官方(客户端无平台设置库,
			// 镜像经环境变量配置;Source 每次下载时解析,改 env 重启服务即生效)。
			Source:  func(ctx context.Context) version.Source { return version.ResolveSource("") },
			DataDir: dataDir,
		}
	}
}

// Start 拉起对账循环(随 ctx 退出;循环退出时停掉托管子进程)。
func (rt *Runtime) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(reconcileInterval)
		defer ticker.Stop()
		for {
			rt.reconcile(ctx)
			select {
			case <-ctx.Done():
				rt.mu.Lock()
				if rt.supervisor != nil {
					rt.supervisor.Stop()
				}
				rt.mu.Unlock()
				return
			case <-ticker.C:
			}
		}
	}()
}

// hostname 取 easytier --hostname:配置优先,空 = 计算机名。
func (rt *Runtime) hostname(cfg Config) string {
	if cfg.Join.Hostname != "" {
		return cfg.Join.Hostname
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "pwclient"
}

// reconcile 对齐「配置期望」与「运行中的监督器」(照抄平台 meshRuntime 模式):
// 未启用/未配置完整 → 停;指纹变化 → 重建;密钥轮换经密钥哈希纳入指纹。
func (rt *Runtime) reconcile(ctx context.Context) {
	cfg, err := rt.store.Load()
	if err != nil {
		log.Printf("[pwclient] 读取配置失败,本轮不调整组网:%v", err)
		return
	}
	if !cfg.Enabled || !cfg.Configured() {
		rt.mu.Lock()
		if rt.supervisor != nil {
			rt.supervisor.Stop()
			rt.supervisor = nil
			rt.supervisorKey = ""
			log.Printf("[pwclient] 组网已停止(未启用或未配置完整)")
		}
		rt.mu.Unlock()
		rt.reconcileSocks5Guard(0)
		return
	}

	binDir := cfg.EasyTier.BinDir
	if binDir == "" {
		binDir = rt.dataDir
	}
	rt.mu.Lock()
	rt.corePath = easytier.Locate(os.Getenv("PIPEWRIGHT_EASYTIER_BIN"), binDir, easytier.CoreBinary())
	rt.cliPath = easytier.Locate(os.Getenv("PIPEWRIGHT_EASYTIER_CLI"), binDir, easytier.CliBinary())
	corePath := rt.corePath
	// 状态查询器随 RPC 地址重建(cli 路径固定)。
	if rt.status == nil || rt.rpcAddr != cfg.EasyTier.RPCAddr {
		rt.status = easytier.NewStatusProvider(rt.cliPath, cfg.EasyTier.RPCAddr)
		rt.rpcAddr = cfg.EasyTier.RPCAddr
	}
	rt.mu.Unlock()

	if corePath == "" {
		rt.maybeAutoDownload()
		return
	}

	key := fmt.Sprintf("%s|%s|%s|%s|%v|%v|%d", cfg.Join.NetworkName, secretFingerprint(cfg.Join.NetworkSecret),
		cfg.Join.VirtualIP, rt.hostname(cfg), cfg.Join.Peers, cfg.Routes, socks5Port(cfg))
	rt.mu.Lock()
	unchanged := rt.supervisor != nil && rt.supervisorKey == key
	rt.mu.Unlock()
	if unchanged {
		return
	}

	sup := easytier.New(easytier.Config{
		Mode:          "managed",
		RPCAddr:       cfg.EasyTier.RPCAddr,
		NetworkName:   cfg.Join.NetworkName,
		NetworkSecret: cfg.Join.NetworkSecret,
		VirtualIP:     cfg.Join.VirtualIP,
		Hostname:      rt.hostname(cfg),
		Peers:         cfg.Join.Peers,
		ManualRoutes:  cfg.Routes, // ProxyNetworks 恒空:客户端不通告任何子网
		Socks5Port:    socks5Port(cfg),
		DataDir:       rt.dataDir,
		CorePath:      corePath,
		CliPath:       rt.cliPath,
	})
	if err := sup.Start(context.WithoutCancel(ctx)); err != nil {
		rt.mu.Lock()
		if rt.failedKey != key {
			log.Printf("[pwclient] EasyTier 启动失败:%v", err)
			rt.failedKey = key
		}
		rt.mu.Unlock()
		return
	}
	rt.ensureFirewall(corePath)
	rt.reconcileSocks5Guard(socks5Port(cfg))
	rt.mu.Lock()
	rt.failedKey = ""
	if rt.supervisor != nil {
		rt.supervisor.Stop()
	}
	rt.supervisor = sup
	rt.supervisorKey = key
	rt.mu.Unlock()
	log.Printf("[pwclient] EasyTier 已启动(网络 %s,虚拟 IP %s,peers %d,限定网段 %d,socks5 %s)",
		cfg.Join.NetworkName, orDHCP(cfg.Join.VirtualIP), len(cfg.Join.Peers), len(cfg.Routes), orOff(socks5Port(cfg)))
}

// socks5Port 取生效的 SOCKS5 端口(未启用 = 0)。
func socks5Port(cfg Config) int {
	if cfg.Socks5.Enabled {
		return cfg.Socks5.Port
	}
	return 0
}

func orOff(port int) string {
	if port == 0 {
		return "关"
	}
	return fmt.Sprintf("127.0.0.1:%d", port)
}

func orDHCP(vip string) string {
	if vip == "" {
		return "(dhcp)"
	}
	return vip
}

// secretFingerprint 把密钥压成 8 位哈希进指纹(密钥轮换触发重启;指纹绝不回显密钥)。
func secretFingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:4])
}

// RestartNow 让下一个对账节拍强制重建监督器(配置保存后 IPC 立即触发,免等节拍)。
func (rt *Runtime) RestartNow() {
	rt.mu.Lock()
	rt.supervisorKey = ""
	rt.mu.Unlock()
	go rt.reconcile(context.Background())
}

// TriggerDownload 手动触发一次二进制下载(幂等;未装配下载器时返回错误)。
func (rt *Runtime) TriggerDownload() error {
	rt.mu.Lock()
	if rt.newDownloader == nil {
		rt.mu.Unlock()
		return fmt.Errorf("pwclient: 自动下载未装配")
	}
	if rt.dlBusy {
		rt.mu.Unlock()
		return nil
	}
	rt.dlBusy = true
	rt.dlErr = ""
	rt.mu.Unlock()
	go rt.runDownload("manual")
	return nil
}

// maybeAutoDownload 在「已配置完整但 core 缺失」时触发自动下载(带退避)。
func (rt *Runtime) maybeAutoDownload() {
	rt.mu.Lock()
	missing := rt.corePath == ""
	busy := rt.dlBusy
	cooling := time.Now().Before(rt.dlRetryAt)
	if !missing || busy || cooling || rt.newDownloader == nil {
		rt.mu.Unlock()
		return
	}
	rt.dlBusy = true
	rt.mu.Unlock()
	log.Printf("[pwclient] 未定位到 %s,开始自动下载(镜像可经 PIPEWRIGHT_RELEASE_MIRROR 配置)", easytier.CoreBinary())
	go rt.runDownload("auto")
}

// runDownload 执行一次下载并更新状态(调用方已置 dlBusy;结束时释放)。
func (rt *Runtime) runDownload(trigger string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	versionTag, err := rt.newDownloader().Download(ctx)
	rt.mu.Lock()
	rt.dlBusy = false
	if err != nil {
		rt.dlErr = err.Error()
		rt.dlRetryAt = time.Now().Add(downloadRetryBackoff)
		log.Printf("[pwclient] easytier 二进制下载失败(%s):%v", trigger, err)
	} else {
		rt.dlErr = ""
		rt.dlVersion = versionTag
		log.Printf("[pwclient] easytier 二进制已就位(%s,%s)", trigger, versionTag)
	}
	rt.mu.Unlock()
}

// StatusSnapshot 是 IPC /api/status 的载荷(不含密钥)。
type StatusSnapshot struct {
	Configured bool               `json:"configured"`
	Enabled    bool               `json:"enabled"`
	Join       JoinStatus         `json:"join"`
	Routes     []string           `json:"routes"`
	Socks5     Socks5Status       `json:"socks5"`
	Supervisor easytier.Runtime   `json:"supervisor"`
	EasyTier   *easytier.Status   `json:"easytier"`
	Binaries   easytier.BinStatus `json:"binaries"`
	Service    ServiceInfo        `json:"service"`
}

// Socks5Status 是本地 SOCKS5 服务的状态回显(Listening 由装配/doctor 侧探测补充,
// 此处回显配置生效值)。
type Socks5Status struct {
	Enabled bool `json:"enabled"`
	Port    int  `json:"port"`
}

// JoinStatus 是加入参数回显(密钥只报存在性)。
type JoinStatus struct {
	NetworkName string   `json:"networkName"`
	HasSecret   bool     `json:"hasSecret"`
	Peers       []string `json:"peers"`
	VirtualIP   string   `json:"virtualIp"`
	Hostname    string   `json:"hostname"`
}

// ServiceInfo 是服务进程自身信息(装配层注入服务名/启动时刻;CLI 直跑时为空)。
type ServiceInfo struct {
	RunningAs string `json:"runningAs"` // "service" | "console" | ""
	PID       int    `json:"pid"`
}

// Snapshot 采集一份当前状态(配置 + 监督器 + EasyTier 链路;绝不包含密钥)。
func (rt *Runtime) Snapshot(ctx context.Context) StatusSnapshot {
	cfg, _ := rt.store.Load()
	snap := StatusSnapshot{
		Configured: cfg.Configured(),
		Enabled:    cfg.Enabled && cfg.Configured(),
		Join: JoinStatus{
			NetworkName: cfg.Join.NetworkName,
			HasSecret:   cfg.Join.NetworkSecret != "",
			Peers:       cfg.Join.Peers,
			VirtualIP:   cfg.Join.VirtualIP,
			Hostname:    rt.hostname(cfg),
		},
		Routes: cfg.Routes,
		Socks5: Socks5Status{Enabled: socks5Port(cfg) > 0, Port: socks5Port(cfg)},
	}
	rt.mu.Lock()
	if rt.supervisor != nil {
		snap.Supervisor = rt.supervisor.Snapshot()
	}
	if rt.status != nil {
		snap.EasyTier = rt.status.Get(ctx)
	}
	snap.Binaries = easytier.BinStatus{
		CorePath:    rt.corePath,
		CliPath:     rt.cliPath,
		Version:     rt.dlVersion,
		Downloading: rt.dlBusy,
		LastErr:     rt.dlErr,
	}
	snap.Service = rt.svc
	rt.mu.Unlock()
	if snap.Routes == nil {
		snap.Routes = []string{}
	}
	if snap.Join.Peers == nil {
		snap.Join.Peers = []string{}
	}
	if snap.EasyTier == nil {
		snap.EasyTier = &easytier.Status{}
	}
	return snap
}
