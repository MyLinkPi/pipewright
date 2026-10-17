// mesh_runtime.go 是集群运行时(main 装配的伴随协程):
//   - 监督循环:随 mesh 设置变化(重新)拉起/停掉托管的 easytier-core 子进程
//     (设置指纹变化才重启,避免无谓抖动;external 模式只读状态不托管);
//   - 发现循环:周期读 EasyTier 链路表,对每个虚拟 IP 探测 /api/mesh/hello,
//     TOFU 登记/刷新 mesh_peers(公钥变更拒绝并告警)。
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/huangchengsir/pipewright/internal/mesh"
	"github.com/huangchengsir/pipewright/internal/mesh/easytier"
)

// meshRuntime 聚集群运行时可变态(监督器与状态查询器随设置重建)。实现 httpapi 的
// meshRuntimeView,供集群页动态读取(不持静态快照)。
type meshRuntime struct {
	svc      mesh.Service
	client   *mesh.Client
	corePath string
	cliPath  string
	dataDir  string
	// downloader 非 nil 时:托管模式启用但二进制缺失 → 自动下载(失败退避);管理端点
	// 亦可手动触发。nil = 禁用自动下载(退回手工放置二进制的既有行为)。
	downloader *easytier.Downloader

	mu          sync.Mutex
	supervisor  *easytier.Supervisor
	status      *easytier.StatusProvider
	external    bool
	// supervisorKey 是当前监督器对应的设置指纹;变化即重建。
	supervisorKey string
	// statusRPC 记录当前状态查询器绑定的 RPC 地址。
	statusRPC string
	// 下载状态:进行中标志 / 最近错误 / 已装版本 / 失败后的退避时刻。
	dlBusy    bool
	dlErr     string
	dlVersion string
	dlRetryAt time.Time
}

// EasyTierStatus 实现 httpapi.meshRuntimeView。
func (rt *meshRuntime) EasyTierStatus(ctx context.Context) *easytier.Status {
	rt.mu.Lock()
	sp := rt.status
	rt.mu.Unlock()
	if sp == nil {
		return &easytier.Status{}
	}
	return sp.Get(ctx)
}

// SupervisorSnapshot 实现 httpapi.meshRuntimeView。
func (rt *meshRuntime) SupervisorSnapshot() easytier.Runtime {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.supervisor == nil {
		return easytier.Runtime{}
	}
	return rt.supervisor.Snapshot()
}

// ExternalMode 实现 httpapi.meshRuntimeView。
func (rt *meshRuntime) ExternalMode() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.external
}

// newMeshRuntime 构造运行时(不启动);start 拉起监督/发现两循环(随 ctx 停机)。
// downloader 可为 nil(禁用自动下载)。
func newMeshRuntime(svc mesh.Service, client *mesh.Client, corePath, cliPath, dataDir string, downloader *easytier.Downloader) *meshRuntime {
	return &meshRuntime{svc: svc, client: client, corePath: corePath, cliPath: cliPath, dataDir: dataDir, downloader: downloader}
}

func (rt *meshRuntime) start(ctx context.Context) {
	go rt.superviseLoop(ctx)
	go rt.discoverLoop(ctx)
}

// Binaries 实现 httpapi.meshRuntimeView:二进制就位状态 + 自动下载进度。
func (rt *meshRuntime) Binaries() easytier.BinStatus {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return easytier.BinStatus{
		CorePath:    rt.corePath,
		CliPath:     rt.cliPath,
		Version:     rt.dlVersion,
		Downloading: rt.dlBusy,
		LastErr:     rt.dlErr,
	}
}

// TriggerDownload 手动触发一次二进制下载(设置页按钮;已在下载中则幂等返回)。
func (rt *meshRuntime) TriggerDownload() error {
	if rt.downloader == nil {
		return fmt.Errorf("easytier 自动下载未装配")
	}
	rt.mu.Lock()
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

// runDownload 执行一次下载并更新状态(调用方已置 dlBusy;结束时释放)。
func (rt *meshRuntime) runDownload(trigger string) {
	// 独立超时,不挂请求/循环 ctx(手动触发不等结果,自动触发不占监督节拍)。
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	version, err := rt.downloader.Download(ctx)
	rt.mu.Lock()
	rt.dlBusy = false
	if err != nil {
		rt.dlErr = err.Error()
		// 失败退避:10 分钟内不自动重试(手动触发不受限)。
		rt.dlRetryAt = time.Now().Add(10 * time.Minute)
		log.Printf("[mesh] easytier 二进制下载失败(%s):%v", trigger, err)
	} else {
		rt.dlErr = ""
		rt.dlVersion = version
		// 重新定位二进制路径,下个监督节拍即可拉起托管进程。
		rt.corePath = easytier.Locate(os.Getenv("PIPEWRIGHT_EASYTIER_BIN"), rt.dataDir, easytier.CoreBinary())
		rt.cliPath = easytier.Locate(os.Getenv("PIPEWRIGHT_EASYTIER_CLI"), rt.dataDir, easytier.CliBinary())
		log.Printf("[mesh] easytier 二进制已就位(%s,%s):core=%s", trigger, version, rt.corePath)
	}
	rt.mu.Unlock()
}

// maybeAutoDownload 在「托管模式已启用但 core 缺失」时触发自动下载(带失败退避)。
func (rt *meshRuntime) maybeAutoDownload() {
	if rt.downloader == nil {
		return
	}
	rt.mu.Lock()
	missing := rt.corePath == ""
	busy := rt.dlBusy
	cooling := time.Now().Before(rt.dlRetryAt)
	if !missing || busy || cooling {
		rt.mu.Unlock()
		return
	}
	rt.dlBusy = true
	rt.mu.Unlock()
	log.Printf("[mesh] 未定位到 %s,开始自动下载(升级源:平台镜像设置)", easytier.CoreBinary())
	go rt.runDownload("auto")
}

// superviseLoop 按设置指纹管理托管子进程。
func (rt *meshRuntime) superviseLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			rt.mu.Lock()
			if rt.supervisor != nil {
				rt.supervisor.Stop()
			}
			rt.mu.Unlock()
			return
		case <-ticker.C:
			rt.reconcile(ctx)
		}
	}
}

// reconcile 对齐「设置期望」与「运行中的监督器」。
func (rt *meshRuntime) reconcile(ctx context.Context) {
	st, err := rt.svc.Settings(ctx)
	if err != nil {
		return
	}
	rt.mu.Lock()
	// 状态查询器:RPC 地址变化才重建(cli 路径固定,装配时定位)。
	if rt.status == nil || rt.statusRPC != st.EasyTierRPC {
		rt.status = easytier.NewStatusProvider(rt.cliPath, st.EasyTierRPC)
		rt.statusRPC = st.EasyTierRPC
	}
	rt.external = st.EasyTierMode == mesh.ModeExternal
	rt.mu.Unlock()

	key := fmt.Sprintf("%s|%s|%s|%s|%v", st.EasyTierMode, st.NetworkName, st.VirtualIP, st.Peers, st.Enabled)
	if !st.Enabled || st.EasyTierMode != mesh.ModeManaged || st.NetworkName == "" {
		rt.mu.Lock()
		if rt.supervisor != nil {
			rt.supervisor.Stop()
			rt.supervisor = nil
			rt.supervisorKey = ""
			log.Printf("[mesh] EasyTier 托管进程已停止(未启用/外部模式)")
		}
		rt.mu.Unlock()
		return
	}
	rt.mu.Lock()
	unchanged := rt.supervisor != nil && rt.supervisorKey == key
	rt.mu.Unlock()
	if unchanged {
		return
	}
	secret, serr := rt.svc.NetworkSecret(ctx)
	if serr != nil {
		log.Printf("[mesh] 读取组网密钥失败,本轮不拉起 EasyTier:%v", serr)
		return
	}
	rt.mu.Lock()
	corePath := rt.corePath
	rt.mu.Unlock()
	if corePath == "" {
		// 托管模式缺二进制:触发自动下载(带退避),就位后下个节拍自动拉起。
		rt.maybeAutoDownload()
		return
	}
	sup := easytier.New(easytier.Config{
		Mode:          mesh.ModeManaged,
		RPCAddr:       st.EasyTierRPC,
		NetworkName:   st.NetworkName,
		NetworkSecret: secret,
		VirtualIP:     st.VirtualIP,
		Hostname:      meshHostname(st),
		Peers:         st.Peers,
		DataDir:       rt.dataDir,
		CorePath:      rt.corePath,
		CliPath:       rt.cliPath,
	})
	if err := sup.Start(context.WithoutCancel(ctx)); err != nil {
		log.Printf("[mesh] EasyTier 托管启动失败:%v", err)
		return
	}
	rt.mu.Lock()
	if rt.supervisor != nil {
		rt.supervisor.Stop()
	}
	rt.supervisor = sup
	rt.supervisorKey = key
	rt.mu.Unlock()
	log.Printf("[mesh] EasyTier 托管进程已启动(网络 %s,虚拟 IP %s,peers %d)",
		st.NetworkName, orDash(st.VirtualIP), len(st.Peers))
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(dhcp)"
	}
	return s
}

// meshHostname 取 easytier --hostname(节点名空 → node_id 前 8 位)。
func meshHostname(st *mesh.Settings) string {
	if st.Name != "" {
		return st.Name
	}
	if len(st.NodeID) > 8 {
		return st.NodeID[:8]
	}
	return st.NodeID
}

// discoverLoop 周期发现:EasyTier 链路表 → 每虚拟 IP 探测 hello → TOFU upsert。
func (rt *meshRuntime) discoverLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rt.discoverOnce(ctx)
		}
	}
}

// discoverOnce 执行一轮发现。已知对端按其登记端口探测;新虚拟 IP 先试默认 8080
// (hello 响应携带真实端口,随后按真实端口刷新登记)。
func (rt *meshRuntime) discoverOnce(ctx context.Context) {
	st, err := rt.svc.Settings(ctx)
	if err != nil || !st.Enabled {
		return
	}
	rt.mu.Lock()
	sp := rt.status
	rt.mu.Unlock()
	if sp == nil {
		return
	}
	snap := sp.Get(ctx)
	if snap == nil {
		return
	}
	peers, _ := rt.svc.ListPeers(ctx)
	knownPorts := map[string]int{}
	for _, p := range peers {
		if p.VirtualIP != "" && p.APIPort > 0 {
			knownPorts[p.VirtualIP] = p.APIPort
		}
	}
	for _, link := range snap.Links {
		if link.VirtualIP == "" || link.VirtualIP == snap.SelfIP {
			continue
		}
		ports := []int{8080}
		if p, ok := knownPorts[link.VirtualIP]; ok && p > 0 && p != 8080 {
			ports = append(ports, p)
		}
		for _, port := range ports {
			base := "http://" + net.JoinHostPort(link.VirtualIP, strconv.Itoa(port))
			hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			hello, herr := rt.client.Hello(hctx, base)
			cancel()
			if herr != nil {
				continue
			}
			if aerr := rt.svc.ApplyHello(ctx, link.VirtualIP, *hello); aerr != nil {
				log.Printf("[mesh] 发现节点 %s 登记失败(公钥变更?拒绝对其服务):%v", hello.NodeID, aerr)
			}
			break
		}
	}
}
