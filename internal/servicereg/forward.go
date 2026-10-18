// forward.go 实现跨环境服务转发(mesh forward)的运行时:
//   - 目录生产:MeshCatalog 把本端网关主机(解析 IP)+ 可转发服务打包,供授权对端经
//     /api/mesh/servicereg/catalog 拉取(授权 = 对端在本端 mesh ACL 的 forwardAllowed);
//   - 目录消费:对端目录缓存(TTL 新鲜期 + 陈旧宽限期,容忍对端瞬断不撕路由/闪 503),
//     buildForwardPlans 把 forward 服务解析为「对端网关 IP:端口」渲染成员;
//   - IP 冲突强校验:本端网关 IP 与各对端网关 IP 重复(/32 路由无法区分归属)→ 该对端
//     整体降级 503 并在状态里告警,绝不带病下发路由;
//   - 数据面路由对账见 meshroute.go。
package servicereg

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/huangchengsir/pipewright/internal/mesh"
)

// 目录缓存节奏:新鲜期内直接用缓存;拉取失败时陈旧宽限期内继续用旧目录
// (瞬断不撕路由),超宽限才判不可达(渲染 503 + 撤路由)。
const (
	forwardCatalogTTL   = 30 * time.Second
	forwardCatalogStale = 10 * time.Minute
	forwardFetchTimeout = 10 * time.Second
)

// ErrForwardUnavailable 表示跨环境转发功能不可用(mesh 未装配/未启用)。
var ErrForwardUnavailable = errors.New("servicereg: mesh forward unavailable")

// ForwardPeer 是可用作转发来源的 mesh 对端(注册表视图;由 main 注入的 listPeers 产生)。
type ForwardPeer struct {
	NodeID    string
	Name      string
	VirtualIP string
}

// ForwardPeerInfo 是 ForwardPeers 的行(服务表单级联下拉):注册表信息 + 目录可达性。
// Available = 对端已授权(forward ACL)且目录可达;Reason 标注不可用原因。
type ForwardPeerInfo struct {
	NodeID    string               `json:"nodeId"`
	Name      string               `json:"name"`
	VirtualIP string               `json:"virtualIp"`
	Available bool                 `json:"available"`
	Reason    string               `json:"reason"` // forbidden | unreachable | disabled | ""
	Catalog   *mesh.ServiceCatalog `json:"catalog,omitempty"`
}

// ForwardPlan 是一个 forward 服务经对端目录解析出的渲染规划(纯数据,renderNginxConf 消费)。
type ForwardPlan struct {
	PeerNodeID string
	PeerName   string // 展示注释用
	// HostHeader 是对端服务的 FQDN(http 转发的 Host 头 / SNI 名)。
	HostHeader string
	// Secure 表示对端基域有证书 → 走 https(对端 80 是 301 跳转,必须打 443)。
	Secure bool
	// Port 是对端端口(http: Secure ? 对端 HTTPS 宿主端口 : HTTP 宿主端口;tcp: 对端 TCP 监听端口)。
	Port int
	// Members 是对端网关 IP 列表(升序去重;空 = 无可渲染成员 → 503 维护态)。
	Members []string
}

// GatewayRouteStatus 是一台网关机上一条跨环境 /32 路由的对账结果(状态页展示)。
type GatewayRouteStatus struct {
	ServerID string `json:"serverId"`
	// Dest 是对端网关 IP(路由目标 /32)。
	Dest string `json:"dest"`
	// State 是 present(已在位)/ applied(本轮补下)/ removed(本轮撤除)/ missing(下发失败)。
	State string `json:"state"`
	Error string `json:"error"`
}

// ForwardStatus 是跨环境转发数据面的聚合状态(网关页展示)。
type ForwardStatus struct {
	IPForwardOK     bool                `json:"ipForwardOk"` // 本机 ip_forward 已开启
	IPForwardDetail string              `json:"ipForwardDetail"`
	Routes          []GatewayRouteStatus `json:"routes"`
	// Conflicts 是冲突告警(本端与对端网关 IP 重复等;空 = 无)。
	Conflicts []string `json:"conflicts"`
}

// forwardRuntime 聚跨环境转发可变态(并发安全:缓存/注入器 + 对账快照)。
type forwardRuntime struct {
	mu           sync.Mutex
	listPeers    func(ctx context.Context) ([]ForwardPeer, error)
	fetchCatalog func(ctx context.Context, nodeID string) (*mesh.ServiceCatalog, error)
	cache        map[string]*forwardCatalogEntry
	routeStatus  []GatewayRouteStatus
	conflicts    []string
}

// forwardCatalogEntry 是单个对端的目录缓存行。
type forwardCatalogEntry struct {
	catalog   *mesh.ServiceCatalog
	fetchedAt time.Time
	// err/errAt 记录最近一次拉取失败(陈旧宽限期内继续用旧目录)。
	err error
	errAt time.Time
}

// forwardEnabled 报告转发功能是否装配(mesh 依赖注入齐备)。
func (s *service) forwardEnabled() bool {
	s.fwd.mu.Lock()
	defer s.fwd.mu.Unlock()
	return s.fwd.fetchCatalog != nil
}

// peerCatalog 返回某对端的服务目录(缓存 + 陈旧宽限;拉取超时收紧)。
// 不可用(未装配/未授权/不可达超宽限)返回错误。
func (s *service) peerCatalog(ctx context.Context, nodeID string) (*mesh.ServiceCatalog, error) {
	s.fwd.mu.Lock()
	fetch := s.fwd.fetchCatalog
	ent := s.fwd.cache[nodeID]
	s.fwd.mu.Unlock()
	if fetch == nil {
		return nil, ErrForwardUnavailable
	}
	if ent != nil && time.Since(ent.fetchedAt) < forwardCatalogTTL {
		return ent.catalog, nil
	}
	fctx, cancel := context.WithTimeout(ctx, forwardFetchTimeout)
	cat, err := fetch(fctx, nodeID)
	cancel()
	now := time.Now()
	s.fwd.mu.Lock()
	defer s.fwd.mu.Unlock()
	if s.fwd.cache == nil {
		s.fwd.cache = map[string]*forwardCatalogEntry{}
	}
	if err != nil {
		// 陈旧宽限:旧目录仍在窗口内 → 继续用(瞬断不撕路由);否则透出错误。
		if ent != nil && ent.catalog != nil && now.Sub(ent.fetchedAt) < forwardCatalogStale {
			s.fwd.cache[nodeID] = &forwardCatalogEntry{catalog: ent.catalog, fetchedAt: ent.fetchedAt, err: err, errAt: now}
			return ent.catalog, nil
		}
		s.fwd.cache[nodeID] = &forwardCatalogEntry{err: err, errAt: now}
		return nil, err
	}
	s.fwd.cache[nodeID] = &forwardCatalogEntry{catalog: cat, fetchedAt: now}
	return cat, nil
}

// ---------- 目录生产(本端) ----------

// MeshCatalog 实现接口:打包本端网关主机 + 可转发服务(授权对端拉取)。
func (s *service) MeshCatalog(ctx context.Context) (*mesh.ServiceCatalog, error) {
	st, err := s.store.getOrCreateSettings(ctx)
	if err != nil {
		return nil, err
	}
	domains, err := s.store.listDomains(ctx)
	if err != nil {
		return nil, err
	}
	certByDomain := make(map[string]bool, len(domains))
	for _, d := range domains {
		certByDomain[d.ID] = d.HasCert
	}
	services, err := s.store.listServices(ctx)
	if err != nil {
		return nil, err
	}
	out := &mesh.ServiceCatalog{HTTPPort: st.HTTPPort, HTTPSPort: st.HTTPSPort}
	for _, svc := range services {
		if !svc.Enabled || svc.UpstreamKind == UpstreamKindForward {
			continue // 只暴露本端真实上游(static/container/address);forward 不进目录(禁链式转发防环)
		}
		dom, derr := s.store.getDomain(ctx, svc.DomainID)
		if derr != nil {
			continue
		}
		out.Services = append(out.Services, mesh.CatalogService{
			ServiceID: svc.ID, FQDN: svc.Name + "." + dom.BaseDomain, Path: svc.Path,
			Protocol: svc.Protocol, HasCert: certByDomain[svc.DomainID],
			TCPListenPort: svc.TCPListenPort,
		})
	}
	sort.Slice(out.Services, func(i, j int) bool { return out.Services[i].FQDN < out.Services[j].FQDN })
	// 网关主机:serverAddr 解析为可达 IP(对端网关机据此配 /32 路由);解析失败的台带空 IP,
	// 对端跳过渲染并标注。
	for _, sid := range st.ServerIDs {
		g := mesh.CatalogGateway{ServerID: sid}
		if srv, serr := s.tg.Get(ctx, sid); serr == nil && srv != nil {
			g.ServerName = srv.Name
		}
		if s.serverAddr != nil {
			if ip, aerr := s.serverAddr(ctx, sid); aerr == nil {
				g.IP = ip
			}
		}
		out.Gateways = append(out.Gateways, g)
	}
	return out, nil
}

// GatewayProxyNetworks 返回本端网关 IP 的 /32 子网代理通告列表(EasyTier --proxy-network;
// mesh 运行时消费)。仅当「已配置网关主机且存在可被对端转发的已启用服务」时通告 ——
// 没有可转发服务时不向 mesh 暴露本端网关路由;服务/网关变化经设置指纹自动重启生效。
func (s *service) GatewayProxyNetworks(ctx context.Context) []string {
	st, err := s.store.getOrCreateSettings(ctx)
	if err != nil || len(st.ServerIDs) == 0 || s.serverAddr == nil {
		return nil
	}
	forwardable := false
	services, serr := s.store.listServices(ctx)
	if serr == nil {
		for _, svc := range services {
			if svc.Enabled && svc.UpstreamKind != UpstreamKindForward {
				forwardable = true
				break
			}
		}
	}
	if !forwardable {
		return nil
	}
	out := make([]string, 0, len(st.ServerIDs))
	for _, sid := range st.ServerIDs {
		if ip, aerr := s.serverAddr(ctx, sid); aerr == nil && ip != "" {
			out = append(out, ip+"/32")
		}
	}
	return out
}

// ---------- 目录消费(对端) ----------

// ForwardPeers 实现接口:可用转发来源列表 + 各自目录可达性(级联下拉数据)。
func (s *service) ForwardPeers(ctx context.Context) ([]ForwardPeerInfo, error) {
	s.fwd.mu.Lock()
	list := s.fwd.listPeers
	fetch := s.fwd.fetchCatalog
	s.fwd.mu.Unlock()
	if list == nil || fetch == nil {
		return []ForwardPeerInfo{}, nil
	}
	peers, err := list(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ForwardPeerInfo, 0, len(peers))
	for _, p := range peers {
		row := ForwardPeerInfo{NodeID: p.NodeID, Name: p.Name, VirtualIP: p.VirtualIP}
		cat, cerr := s.peerCatalog(ctx, p.NodeID)
		switch {
		case cerr == nil:
			row.Available = true
			row.Catalog = cat
		case errors.Is(cerr, mesh.ErrForbidden):
			row.Reason = "forbidden"
		case errors.Is(cerr, ErrForwardUnavailable):
			row.Reason = "disabled"
		default:
			row.Reason = "unreachable"
		}
		out = append(out, row)
	}
	return out, nil
}

// buildForwardPlans 为全部 enabled forward 服务计算渲染规划。
// 返回 plans(服务 ID → 规划;缺项 = 维护态)与冲突告警列表(本端/对端网关 IP 重复)。
func (s *service) buildForwardPlans(ctx context.Context, enabled []RegisteredService) (map[string]ForwardPlan, []string) {
	plans := map[string]ForwardPlan{}
	// 引用的对端去重拉目录。
	peerIDs := map[string]struct{}{}
	for _, svc := range enabled {
		if svc.UpstreamKind == UpstreamKindForward && svc.PeerNodeID != "" {
			peerIDs[svc.PeerNodeID] = struct{}{}
		}
	}
	if len(peerIDs) == 0 {
		s.fwd.mu.Lock()
		s.fwd.conflicts = nil
		s.fwd.mu.Unlock()
		return plans, nil
	}
	catalogs := map[string]*mesh.ServiceCatalog{}
	for id := range peerIDs {
		if cat, err := s.peerCatalog(ctx, id); err == nil {
			catalogs[id] = cat
		}
	}
	// IP 冲突强校验:本端网关 IP ∪ 各可达对端网关 IP 查重;重复 → 相关对端整体降级。
	conflicts := detectForwardConflicts(ctx, s, catalogs)
	blocked := map[string]bool{}
	for _, c := range conflicts {
		for _, id := range c.RemotePeers {
			blocked[id] = true
		}
	}
	for _, svc := range enabled {
		if svc.UpstreamKind != UpstreamKindForward || svc.PeerNodeID == "" {
			continue
		}
		cat, ok := catalogs[svc.PeerNodeID]
		if !ok || blocked[svc.PeerNodeID] {
			continue // 目录不可达或 IP 冲突 → 503 维护态
		}
		var peerSvc *mesh.CatalogService
		for i := range cat.Services {
			if cat.Services[i].ServiceID == svc.PeerServiceID {
				peerSvc = &cat.Services[i]
				break
			}
		}
		if peerSvc == nil || peerSvc.Protocol != svc.Protocol {
			continue // 对端服务已删/协议漂移 → 维护态
		}
		plan := ForwardPlan{PeerNodeID: svc.PeerNodeID, HostHeader: peerSvc.FQDN, Members: catalogGatewayIPs(cat)}
		switch svc.Protocol {
		case ProtocolTCP:
			if peerSvc.TCPListenPort <= 0 {
				continue
			}
			plan.Port = peerSvc.TCPListenPort
		default:
			if peerSvc.HasCert {
				plan.Secure = true
				plan.Port = cat.HTTPSPort
			} else {
				plan.Port = cat.HTTPPort
			}
		}
		if len(plan.Members) == 0 {
			continue
		}
		plans[svc.ID] = plan
	}
	s.fwd.mu.Lock()
	if conflicts != nil {
		s.fwd.conflicts = make([]string, 0, len(conflicts))
		for _, c := range conflicts {
			s.fwd.conflicts = append(s.fwd.conflicts, c.Text)
		}
	} else {
		s.fwd.conflicts = nil
	}
	s.fwd.mu.Unlock()
	return plans, conflictsText(conflicts)
}

// forwardConflict 是一条网关 IP 冲突(本端 ↔ 对端或对端 ↔ 对端;/32 路由按目标 IP 唯一
// 定向,重复即歧义)。RemotePeers 是涉及冲突的对端 nodeID(整体降级用);Text 是展示文案。
type forwardConflict struct {
	IP         string
	RemotePeers []string
	Text        string
}

// conflictsText 把冲突列表转为展示文案(nil → nil)。
func conflictsText(cs []forwardConflict) []string {
	if len(cs) == 0 {
		return nil
	}
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Text)
	}
	return out
}

// detectForwardConflicts 检查「本端网关 IP ∪ 各对端网关 IP」重复。
func detectForwardConflicts(ctx context.Context, s *service, catalogs map[string]*mesh.ServiceCatalog) []forwardConflict {
	// IP → 归属(「本端」或对端 nodeID);对端展示名尽力补进文案。
	peerNames := map[string]string{}
	s.fwd.mu.Lock()
	if s.fwd.listPeers != nil {
		if peers, lerr := s.fwd.listPeers(ctx); lerr == nil {
			for _, p := range peers {
				peerNames[p.NodeID] = p.Name
			}
		}
	}
	s.fwd.mu.Unlock()
	label := func(nodeID string) string {
		if n := peerNames[nodeID]; n != "" {
			return n
		}
		return nodeID
	}

	type owner struct {
		local bool
		peer  string
	}
	ownerByIP := map[string]owner{}
	var dups []forwardConflict
	claim := func(ip string, o owner) {
		if ip == "" {
			return
		}
		prev, ok := ownerByIP[ip]
		if !ok {
			ownerByIP[ip] = o
			return
		}
		if prev == o {
			return
		}
		// 两个不同归属争同一 IP → 冲突(去重:同一 IP 只记一条)。
		for _, c := range dups {
			if c.IP == ip {
				if !o.local && !containsStr(c.RemotePeers, o.peer) {
					c.RemotePeers = append(c.RemotePeers, o.peer)
				}
				return
			}
		}
		c := forwardConflict{IP: ip}
		var names []string
		if prev.local {
			names = append(names, "本端")
		} else {
			c.RemotePeers = append(c.RemotePeers, prev.peer)
			names = append(names, label(prev.peer))
		}
		if o.local {
			names = append(names, "本端")
		} else {
			c.RemotePeers = append(c.RemotePeers, o.peer)
			names = append(names, label(o.peer))
		}
		c.Text = fmt.Sprintf("%s(%s)", ip, strings.Join(names, " ↔ "))
		dups = append(dups, c)
	}
	st, err := s.store.getOrCreateSettings(ctx)
	if err == nil && s.serverAddr != nil {
		for _, sid := range st.ServerIDs {
			if ip, aerr := s.serverAddr(ctx, sid); aerr == nil {
				claim(ip, owner{local: true})
			}
		}
	}
	for nodeID, cat := range catalogs {
		for _, ip := range catalogGatewayIPs(cat) {
			claim(ip, owner{peer: nodeID})
		}
	}
	sort.Slice(dups, func(i, j int) bool { return dups[i].IP < dups[j].IP })
	return dups
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// catalogGatewayIPs 提取目录网关 IP(去空、去重、升序)。
func catalogGatewayIPs(cat *mesh.ServiceCatalog) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(cat.Gateways))
	for _, g := range cat.Gateways {
		ip := strings.TrimSpace(g.IP)
		if ip == "" {
			continue
		}
		if _, dup := seen[ip]; dup {
			continue
		}
		seen[ip] = struct{}{}
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}
