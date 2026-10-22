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

	mu         sync.Mutex
	supervisor *easytier.Supervisor
	status     *easytier.StatusProvider
	external   bool
	// supervisorKey 是当前监督器对应的设置指纹;变化即重建。
	supervisorKey string
	// statusRPC 记录当前状态查询器绑定的 RPC 地址。
	statusRPC string
	// 下载状态:进行中标志 / 最近错误 / 已装版本 / 失败后的退避时刻。
	dlBusy    bool
	dlErr     string
	dlVersion string
	dlRetryAt time.Time
	// proxyNetworks 提供本端需通告的子网代理列表(跨环境转发:本端网关 IP 的 /32;
	// main 注入,变化纳入设置指纹重启 easytier)。nil = 不通告。
	proxyNetworks func(ctx context.Context) []string
	// afterDiscover 是发现循环收尾钩子(跨环境转发:路由调和 + ip_forward 保活;
	// main 注入,nil = 无)。每个发现节拍(30s)在 discoverOnce 后调用。
	afterDiscover func(ctx context.Context)
	// failedKey 记录最近一次启动失败对应的指纹(同指纹失败不逐拍刷屏)。
	failedKey string
	// lastSeenCIDRs 是最近一次成功快照里的全网通告网段(路由白名单自动项数据源;
	// 仅 superviseLoop 单协程读写)。快照瞬时不可用(core 重启窗口)时沿用上轮值:
	// 清零会让白名单缩水 → 指纹翻转 → 刚拉起的核心又被重启,失败窗口级联放大。
	lastSeenCIDRs []string
}

// SetProxyNetworks 注入「本端子网代理通告列表」提供器(跨环境转发数据面:
// 本端网关 IP 的 /32;变化会重启 easytier 使通告生效)。
func (rt *meshRuntime) SetProxyNetworks(f func(ctx context.Context) []string) { rt.proxyNetworks = f }

// SetAfterDiscover 注入发现循环收尾钩子(跨环境转发:网关机路由对账 + ip_forward 保活)。
func (rt *meshRuntime) SetAfterDiscover(f func(ctx context.Context)) { rt.afterDiscover = f }

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

// statusSnapshot 取当前路由表通告清单(白名单发现远端网关用;不可用时返回 nil)。
func (rt *meshRuntime) statusSnapshot() *easytier.Status {
	rt.mu.Lock()
	sp := rt.status
	rt.mu.Unlock()
	if sp == nil {
		return nil
	}
	st := sp.Get(context.Background())
	if st == nil || st.Err != "" {
		return nil
	}
	return st
}

// StopMeshAndWait 同步停止托管 easytier 并等待进程退出(re-exec 前置清理:不等退出
// 会留孤儿占端口,v1.6.4 事故)。停后到 exec 之间若撞上对账节拍会重拉一次,
// 无害(新进程启动时有孤儿清理兜底)。
func (rt *meshRuntime) StopMeshAndWait(grace time.Duration) {
	rt.mu.Lock()
	sp := rt.supervisor
	rt.supervisor = nil
	rt.supervisorKey = ""
	rt.mu.Unlock()
	if sp != nil {
		sp.StopAndWait(grace)
	}
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

// normalizeProxyNetworks 归一化子网代理列表:去空白/去空项/去重(保序),只保留合法 cidr。
func normalizeProxyNetworks(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, n := range in {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(n); err != nil {
			continue // 非法 cidr 静默丢弃(提供方只产合法值,防御兜底)
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
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

	// 子网代理通告 = 管理员自定义网段(集群设置)∪ servicereg 网关 /32(跨环境转发),
	// 经 normalizeProxyNetworks 归一去重;变化纳入下方指纹,自动重启 easytier 生效。
	proxies := normalizeProxyNetworks(st.ProxyNetworksCustom)
	gateways := []string{}
	if rt.proxyNetworks != nil {
		gateways = rt.proxyNetworks(ctx)
		proxies = normalizeProxyNetworks(append(proxies, gateways...))
	}
	// 路由白名单(路由隔离,见 internal/mesh/routes.go):自动项仅**对端**网关 /32
	//(路由表已见,首连引导:远端网关经发现进入路由表后并入,指纹变化重启生效);
	// 手动项 = 管理员显式接受网段。本端通告(proxies,含本端网关 /32)只通告不路由
	// —— 本端网段经局域网可达,进白名单会被装成 tun 路由吸走本端流量(v1.6.4 事故:
	// 本端网关 SSH 断联)。空白名单 = 裸 --manual-routes,拒绝全部通告。
	//
	// 同网段多通告者互备:多节点可通告同一网段,EasyTier 数据面按跳数择优单一网关,
	// 该网关失联后自动切换其余通告者(实测 2.6.4 秒级)。主备切换只变「网段→网关」
	// 映射、不变 CIDR 集合,故不触发下方指纹、不重启。切勿把网关映射写进 supervisorKey
	//—— 切换会引发全网 easytier 重启风暴;只有某网段**所有**通告者失联(CIDR 从
	// lastSeenCIDRs 消失)才缩白名单重启,此时该网段确已不可达,属合理收敛。
	if sp := rt.statusSnapshot(); sp != nil {
		rt.lastSeenCIDRs = sp.ProxyCIDRs
	}
	manual := mesh.RouteWhitelist(proxies, rt.lastSeenCIDRs, st.AcceptNetworksCustom)
	key := fmt.Sprintf("%s|%s|%s|%s|%v|%v|%v|%v", st.EasyTierMode, st.NetworkName, st.VirtualIP, st.Peers, st.Enabled, proxies, manual, st.EnableExitNode)
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
	// 先清理孤儿 easytier(re-exec/崩溃遗留,占着监听与 rpc-portal 端口会让新进程
	// 起不来,孤儿以旧参数持续服务 —— v1.6.4 事故链)。
	easytier.KillStaleByRPCPortal(st.EasyTierRPC)

	sup := easytier.New(easytier.Config{
		Mode:          mesh.ModeManaged,
		RPCAddr:       st.EasyTierRPC,
		NetworkName:   st.NetworkName,
		NetworkSecret: secret,
		VirtualIP:     st.VirtualIP,
		Hostname:      meshHostname(st),
		Peers:         st.Peers,
		ProxyNetworks: proxies,
		// 路由隔离:平台节点也白名单化(manual 仅含网关 /32 + 显式接受项),
		// Strict = 空白名单时拒绝全部通告(与 pwclient 客户端同构的 fail-closed)。
		ManualRoutes:       manual,
		StrictManualRoutes: true,
		// 出口节点:平台节点可显式允许充当客户端出口(foreign network NAT)。
		EnableExitNode: st.EnableExitNode,
		DataDir:        rt.dataDir,
		CorePath:       rt.corePath,
		CliPath:        rt.cliPath,
	})
	if err := sup.Start(context.WithoutCancel(ctx)); err != nil {
		// 同一指纹的启动失败不逐拍刷屏(探测类失败每进程只发生一次,配置修复后指纹变化即重试)。
		if rt.failedKey != key {
			log.Printf("[mesh] EasyTier 托管启动失败:%v", err)
			rt.failedKey = key
		}
		return
	}
	rt.failedKey = ""
	rt.mu.Lock()
	if rt.supervisor != nil {
		rt.supervisor.Stop()
	}
	rt.supervisor = sup
	rt.supervisorKey = key
	rt.mu.Unlock()
	log.Printf("[mesh] EasyTier 托管进程已启动(网络 %s,虚拟 IP %s,peers %d,子网代理 %d)",
		st.NetworkName, orDash(st.VirtualIP), len(st.Peers), len(proxies))
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

// discoverLoop 周期发现:EasyTier 链路表 → 每虚拟 IP 探测 hello → TOFU upsert;
// 收尾执行 afterDiscover 钩子(跨环境转发的路由对账/ip_forward 保活)。
func (rt *meshRuntime) discoverLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rt.discoverOnce(ctx)
			if rt.afterDiscover != nil {
				rt.afterDiscover(ctx)
			}
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
	// 清理历史误登记的本节点自身行(曾因 SelfIP 解析失败期自环探测混入;对端注册表
	// 语义不含本机,误留行在集群页恒显离线)。幂等:仅存在时删一次。
	if _, gerr := rt.svc.GetPeer(ctx, st.NodeID); gerr == nil {
		if derr := rt.svc.DeletePeer(ctx, st.NodeID); derr == nil {
			log.Printf("[mesh] 已清理误登记的本节点自身注册条目(历史发现循环缺陷遗留)")
		}
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
			// hello 响应身份即本节点 → 虚拟 IP 自环(SelfIP 解析失败期会漏过滤),
			// 不登记进对端注册表,也不再试其余端口。
			if strings.TrimSpace(hello.NodeID) == st.NodeID {
				break
			}
			if aerr := rt.svc.ApplyHello(ctx, link.VirtualIP, *hello); aerr != nil {
				log.Printf("[mesh] 发现节点 %s 登记失败(公钥变更?拒绝对其服务):%v", hello.NodeID, aerr)
			}
			break
		}
	}
}
