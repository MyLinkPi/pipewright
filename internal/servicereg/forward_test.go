// forward_test.go 覆盖跨环境服务转发:nginx 渲染四态(http 有/无证书、tcp、维护态/冲突)、
// prepareService 转发校验(经注入的假目录拉取器)、buildForwardPlans 冲突降级、
// 路由对账命令构造(fake target)与 ip route 输出解析。
package servicereg

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/mesh"
	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/target"
)

// --- 渲染(纯函数) ---------------------------------------------------------

func TestRenderForwardHTTPSecure(t *testing.T) {
	dom := Domain{ID: "d1", BaseDomain: "efg.com", HasCert: true}
	svc := RegisteredService{ID: "s1", DomainID: "d1", Name: "api", Protocol: ProtocolHTTP, UpstreamKind: UpstreamKindForward, PeerNodeID: "peerA", Enabled: true}
	plan := ForwardPlan{
		PeerNodeID: "peerA", HostHeader: "api.remote.com", Secure: true, Port: 443,
		Members: []string{"10.20.0.2", "10.20.0.1"},
	}
	out := renderNginxConf(nginxConfDir, []Domain{dom}, []RegisteredService{svc}, nil, nil, 0, map[string]ForwardPlan{"s1": plan}, nil).Text()
	for _, want := range []string{
		"upstream pw_api {",
		"server 10.20.0.1:443 max_fails=2 fail_timeout=10s;",
		"server 10.20.0.2:443 max_fails=2 fail_timeout=10s;",
		"proxy_pass https://pw_api;",
		"proxy_set_header Host api.remote.com;",
		"proxy_ssl_server_name on;",
		"proxy_ssl_name api.remote.com;",
		"proxy_ssl_verify off;",
		"proxy_set_header Connection $connection_upgrade;",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "return 503") {
		t.Fatalf("有规划时不应渲染维护态:\n%s", out)
	}
}

func TestRenderForwardHTTPPlain(t *testing.T) {
	dom := Domain{ID: "d1", BaseDomain: "efg.com"}
	svc := RegisteredService{ID: "s1", DomainID: "d1", Name: "api", Protocol: ProtocolHTTP, UpstreamKind: UpstreamKindForward, Enabled: true}
	plan := ForwardPlan{PeerNodeID: "p", HostHeader: "api.remote.com", Port: 8080, Members: []string{"10.20.0.3"}}
	out := renderNginxConf(nginxConfDir, []Domain{dom}, []RegisteredService{svc}, nil, nil, 0, map[string]ForwardPlan{"s1": plan}, nil).Text()
	if !strings.Contains(out, "server 10.20.0.3:8080 max_fails=2 fail_timeout=10s;") {
		t.Fatalf("缺对端成员:\n%s", out)
	}
	if !strings.Contains(out, "proxy_pass http://pw_api;") {
		t.Fatalf("无证书应走 http:\n%s", out)
	}
	if !strings.Contains(out, "proxy_set_header Host api.remote.com;") {
		t.Fatalf("应改写 Host 为对端 FQDN:\n%s", out)
	}
	if strings.Contains(out, "proxy_ssl_server_name") {
		t.Fatalf("无证书不应有 SNI 配置:\n%s", out)
	}
}

func TestRenderForwardMaintenanceAndConflict(t *testing.T) {
	dom := Domain{ID: "d1", BaseDomain: "efg.com"}
	httpFwd := RegisteredService{ID: "s1", DomainID: "d1", Name: "api", Protocol: ProtocolHTTP, UpstreamKind: UpstreamKindForward, PeerNodeID: "p", Enabled: true}
	tcpFwd := RegisteredService{ID: "s2", DomainID: "d1", Name: "db", Protocol: ProtocolTCP, UpstreamKind: UpstreamKindForward, PeerNodeID: "p", TCPListenPort: 3306, Enabled: true}
	// 无规划(目录不可达/冲突):http → 503;tcp → 拒绝端口兜底。
	out := renderNginxConf(nginxConfDir, []Domain{dom}, []RegisteredService{httpFwd, tcpFwd}, nil, nil, 0, nil, []string{"10.0.0.9(本端 ↔ prod)"}).Text()
	for _, want := range []string{
		"# 注意:跨环境转发存在网关 IP 冲突(相关对端已降级为 503 维护态):10.0.0.9(本端 ↔ prod)",
		"return 503;",
		"proxy_pass 127.0.0.1:1;",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺 %q:\n%s", want, out)
		}
	}
}

func TestRenderForwardTCP(t *testing.T) {
	dom := Domain{ID: "d1", BaseDomain: "efg.com"}
	svc := RegisteredService{ID: "s2", DomainID: "d1", Name: "db", Protocol: ProtocolTCP, UpstreamKind: UpstreamKindForward, TCPListenPort: 3306, Enabled: true}
	plan := ForwardPlan{PeerNodeID: "p", Port: 13306, Members: []string{"10.20.0.5"}}
	out := renderNginxConf(nginxConfDir, []Domain{dom}, []RegisteredService{svc}, nil, nil, 0, map[string]ForwardPlan{"s2": plan}, nil).Text()
	for _, want := range []string{
		"upstream pwf_db {",
		"server 10.20.0.5:13306 max_fails=2 fail_timeout=10s;",
		"listen 3306;",
		"proxy_pass pwf_db;",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺 %q:\n%s", want, out)
		}
	}
}

// --- prepareService / 目录消费(注入假 mesh 依赖) ---------------------------

// fakeCatalogFixture 构造注入用的目录拉取器:nodeID → 固定目录;未知节点返回错误。
func fakeCatalogFixture() (func(context.Context, string) (*mesh.ServiceCatalog, error), *mesh.ServiceCatalog) {
	cat := &mesh.ServiceCatalog{
		HTTPPort: 80, HTTPSPort: 443,
		Gateways: []mesh.CatalogGateway{
			{ServerID: "g1", IP: "10.20.0.2"},
			{ServerID: "g2", IP: "10.20.0.1"},
		},
		Services: []mesh.CatalogService{
			{ServiceID: "ps1", FQDN: "api.remote.com", Protocol: ProtocolHTTP, HasCert: true},
			{ServiceID: "ps2", FQDN: "db.remote.com", Protocol: ProtocolTCP, TCPListenPort: 13306},
			{ServiceID: "ps3", FQDN: "plain.remote.com", Protocol: ProtocolHTTP},
		},
	}
	return func(_ context.Context, nodeID string) (*mesh.ServiceCatalog, error) {
		if nodeID != "peerA" {
			return nil, mesh.ErrForbidden
		}
		return cat, nil
	}, cat
}

func TestPrepareForwardServiceValidation(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		ctx := context.Background()
		ft := &fakeTarget{}
		svc := New(st.DB, ft, fakeSealer{})
		svc.SetServerAddrResolver(func(context.Context, string) (string, error) { return "10.9.9.9", nil })
		fetch, _ := fakeCatalogFixture()
		svc.SetMeshForward(nil, fetch)
		dom, err := svc.CreateDomain(ctx, "efg.com")
		if err != nil {
			t.Fatalf("建域:%v", err)
		}

		// 未注入 mesh 依赖 → 拒绝。
		svc.SetMeshForward(nil, nil)
		if _, err := svc.CreateService(ctx, CreateServiceInput{
			DomainID: dom.ID, Name: "api", Protocol: ProtocolHTTP,
			UpstreamKind: UpstreamKindForward, PeerNodeID: "peerA", PeerServiceID: "ps1",
		}); err != ErrForwardUnavailable && err != ErrInvalidForwardPeer {
			t.Fatalf("未装配 mesh 时应拒绝,得 %v", err)
		}
		svc.SetMeshForward(nil, fetch)

		// 协议不匹配(http 声明转发 tcp 服务)→ 拒绝。
		if _, err := svc.CreateService(ctx, CreateServiceInput{
			DomainID: dom.ID, Name: "bad", Protocol: ProtocolHTTP,
			UpstreamKind: UpstreamKindForward, PeerNodeID: "peerA", PeerServiceID: "ps2",
		}); err != ErrInvalidForwardPeer {
			t.Fatalf("协议不匹配应拒绝,得 %v", err)
		}
		// 对端服务不存在 → 拒绝。
		if _, err := svc.CreateService(ctx, CreateServiceInput{
			DomainID: dom.ID, Name: "bad2", Protocol: ProtocolHTTP,
			UpstreamKind: UpstreamKindForward, PeerNodeID: "peerA", PeerServiceID: "gone",
		}); err != ErrInvalidForwardPeer {
			t.Fatalf("不存在的对端服务应拒绝,得 %v", err)
		}
		// 未授权节点 → 拒绝。
		if _, err := svc.CreateService(ctx, CreateServiceInput{
			DomainID: dom.ID, Name: "bad3", Protocol: ProtocolHTTP,
			UpstreamKind: UpstreamKindForward, PeerNodeID: "peerB", PeerServiceID: "ps1",
		}); err != ErrInvalidForwardPeer {
			t.Fatalf("未授权节点应拒绝,得 %v", err)
		}
		// http 转发成功(有证书对端)。
		rs, err := svc.CreateService(ctx, CreateServiceInput{
			DomainID: dom.ID, Name: "api", Protocol: ProtocolHTTP,
			UpstreamKind: UpstreamKindForward, PeerNodeID: "peerA", PeerServiceID: "ps1",
		})
		if err != nil {
			t.Fatalf("创建转发服务:%v", err)
		}
		if rs.Upstream != "" || rs.UpstreamPort != 0 || rs.PeerNodeID != "peerA" || rs.PeerServiceID != "ps1" {
			t.Fatalf("forward 存库形态异常:%+v", rs)
		}
		// forward 服务拒绝实例操作。
		if _, err := svc.AddInstance(ctx, rs.ID, "sv1", "c1", 80, 8080); err != ErrForwardService {
			t.Fatalf("forward 应拒绝实例,得 %v", err)
		}
		// tcp 转发:须显式本端监听端口。
		if _, err := svc.CreateService(ctx, CreateServiceInput{
			DomainID: dom.ID, Name: "db", Protocol: ProtocolTCP,
			UpstreamKind: UpstreamKindForward, PeerNodeID: "peerA", PeerServiceID: "ps2",
		}); err != ErrInvalidPort {
			t.Fatalf("tcp 转发缺监听端口应拒绝,得 %v", err)
		}
		if _, err := svc.CreateService(ctx, CreateServiceInput{
			DomainID: dom.ID, Name: "db", Protocol: ProtocolTCP,
			UpstreamKind: UpstreamKindForward, PeerNodeID: "peerA", PeerServiceID: "ps2", TCPListenPort: 3306,
		}); err != nil {
			t.Fatalf("创建 tcp 转发服务:%v", err)
		}
	})
}

func TestBuildForwardPlansConflictDropsPeer(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		ctx := context.Background()
		ft := &fakeTarget{}
		svc := New(st.DB, ft, fakeSealer{})
		// 本端网关 IP = 10.9.9.9;对端网关 10.20.0.1/10.20.0.2。
		svc.SetServerAddrResolver(func(_ context.Context, serverID string) (string, error) {
			if serverID == "local-gw" {
				return "10.9.9.9", nil
			}
			return "", target.ErrNotFound
		})
		fetch, cat := fakeCatalogFixture()
		svc.SetMeshForward(func(context.Context) ([]ForwardPeer, error) {
			return []ForwardPeer{{NodeID: "peerA", Name: "prod"}}, nil
		}, fetch)
		if _, err := svc.UpdateSettings(ctx, SettingsUpdate{ServerIDs: &[]string{"local-gw"}}); err != nil {
			t.Fatalf("配置网关:%v", err)
		}
		dom, err := svc.CreateDomain(ctx, "efg.com")
		if err != nil {
			t.Fatalf("建域:%v", err)
		}
		in := CreateServiceInput{
			DomainID: dom.ID, Name: "api", Protocol: ProtocolHTTP,
			UpstreamKind: UpstreamKindForward, PeerNodeID: "peerA", PeerServiceID: "ps1",
		}
		enabled, err := svc.prepareService(ctx, "", in)
		if err != nil {
			t.Fatalf("prepare:%v", err)
		}
		// 无冲突:规划就绪(有证书 → 443;成员升序)。
		plans, conflicts := svc.buildForwardPlans(ctx, []RegisteredService{*enabled})
		if len(conflicts) != 0 {
			t.Fatalf("不应有冲突:%v", conflicts)
		}
		p, ok := plans[enabled.ID]
		if !ok || !p.Secure || p.Port != 443 || p.HostHeader != "api.remote.com" {
			t.Fatalf("规划异常:%+v ok=%v", p, ok)
		}
		if strings.Join(p.Members, ",") != "10.20.0.1,10.20.0.2" {
			t.Fatalf("成员应升序去重:%v", p.Members)
		}
		// 冲突:本端网关 IP 与对端网关重复 → 整端降级。
		svc.SetServerAddrResolver(func(context.Context, string) (string, error) { return "10.20.0.1", nil })
		plans, conflicts = svc.buildForwardPlans(ctx, []RegisteredService{*enabled})
		if len(conflicts) == 0 {
			t.Fatalf("IP 重复应报冲突")
		}
		if _, ok := plans[enabled.ID]; ok {
			t.Fatalf("冲突对端应降级为无规划(503)")
		}
		if len(cat.Gateways) != 2 {
			t.Fatalf("目录不应被修改")
		}
	})
}

func TestForwardPeersClassifiesReasons(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		ctx := context.Background()
		svc := New(st.DB, &fakeTarget{}, fakeSealer{})
		svc.SetMeshForward(
			func(context.Context) ([]ForwardPeer, error) {
				return []ForwardPeer{{NodeID: "ok", Name: "n1"}, {NodeID: "deny", Name: "n2"}, {NodeID: "down", Name: "n3"}}, nil
			},
			func(_ context.Context, nodeID string) (*mesh.ServiceCatalog, error) {
				switch nodeID {
				case "ok":
					return &mesh.ServiceCatalog{HTTPPort: 80, HTTPSPort: 443, Gateways: []mesh.CatalogGateway{{IP: "10.20.0.1"}}}, nil
				case "deny":
					return nil, mesh.ErrForbidden
				default:
					return nil, mesh.ErrPeerUnreachable
				}
			})
		peers, err := svc.ForwardPeers(ctx)
		if err != nil {
			t.Fatalf("ForwardPeers:%v", err)
		}
		if len(peers) != 3 || !peers[0].Available || peers[1].Reason != "forbidden" || peers[2].Reason != "unreachable" {
			t.Fatalf("分类异常:%+v", peers)
		}
	})
}

// --- 路由对账 ---------------------------------------------------------------

func TestParseRoutesVia(t *testing.T) {
	out := "default via 192.168.1.1 dev eth0\n" +
		"10.20.0.1 via 192.168.1.10 dev eth0\n" +
		"10.20.0.2 via 192.168.1.10 dev eth0 metric 5\n" +
		"10.8.0.0/24 dev tun0 proto static\n" +
		"10.20.0.3 via 192.168.1.1 dev eth0\n"
	got := parseRoutesVia(out, "192.168.1.10")
	if len(got) != 2 || !got["10.20.0.1"] || !got["10.20.0.2"] || got["10.20.0.3"] {
		t.Fatalf("解析异常:%v", got)
	}
}

func TestLocalSourceIP(t *testing.T) {
	// 回环目标必有源;空目标 → 空。
	if ip := localSourceIP("127.0.0.1"); ip == "" {
		t.Fatalf("loopback 应有源地址")
	}
	if ip := localSourceIP(""); ip != "" {
		t.Fatalf("空目标应返回空")
	}
}

func TestReconcileForwardRoutesIssuesCommands(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		ctx := context.Background()
		ft := &fakeTarget{}
		svc := New(st.DB, ft, fakeSealer{})
		// 网关地址取回环:localSourceIP(UDP 拨号取源)在任意环境必然可路由,测试不依赖外网。
		svc.SetServerAddrResolver(func(context.Context, string) (string, error) { return "127.0.0.1", nil })
		fetch, _ := fakeCatalogFixture()
		svc.SetMeshForward(nil, fetch)
		if _, err := svc.UpdateSettings(ctx, SettingsUpdate{ServerIDs: &[]string{"gw1"}}); err != nil {
			t.Fatalf("配置网关:%v", err)
		}
		dom, err := svc.CreateDomain(ctx, "efg.com")
		if err != nil {
			t.Fatalf("建域:%v", err)
		}
		// 直接落一条 forward 服务行(绕过 prepareService 的目录校验,聚焦路由对账)。
		enabled, err := svc.prepareService(ctx, "", CreateServiceInput{
			DomainID: dom.ID, Name: "api", Protocol: ProtocolHTTP,
			UpstreamKind: UpstreamKindForward, PeerNodeID: "peerA", PeerServiceID: "ps1",
		})
		if err != nil {
			t.Fatalf("prepare:%v", err)
		}
		if _, ierr := st.DB.ExecContext(ctx,
			`INSERT INTO service_reg_services (id, domain_id, name, path, protocol, upstream_kind, upstream, upstream_port,
			 tcp_listen_port, strip_path_prefix, enabled, peer_node_id, peer_service_id, created_at, updated_at)
			 VALUES (?, ?, ?, '', ?, ?, '', 0, 0, 0, 1, ?, ?, ?, ?)`,
			enabled.ID, enabled.DomainID, enabled.Name, enabled.Protocol, enabled.UpstreamKind,
			enabled.PeerNodeID, enabled.PeerServiceID, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"); ierr != nil {
			t.Fatalf("插入服务行:%v", ierr)
		}

		svc.ReconcileForwardRoutes(ctx)
		// 期望成员 = 对端网关 IP(升序);每台网关各一条 replace;ip route show 已读取。
		if !hasCmd(ft, "ip route show") {
			t.Fatalf("应读取路由表:%v", ft.execCalls)
		}
		if !hasCmd(ft, "ip route replace 10.20.0.1/32 via") {
			t.Fatalf("应下发 10.20.0.1 路由:%v", ft.execCalls)
		}
		if !hasCmd(ft, "ip route replace 10.20.0.2/32 via") {
			t.Fatalf("应下发 10.20.0.2 路由:%v", ft.execCalls)
		}
		// 账本持久化:网关 → 两条路由。
		ledger, err := svc.store.getForwardRoutes(ctx)
		if err != nil || len(ledger["gw1"]) != 2 {
			t.Fatalf("账本应含 2 条:%v err=%v", ledger, err)
		}
		// 转发状态快照可见。
		gs, err := svc.GatewayStatus(ctx)
		if err != nil || gs.Forward == nil {
			t.Fatalf("网关状态应含转发快照:%v err=%v", gs, err)
		}

		// 第二轮:路由已“在位”(ip route show 返回已配)→ 不再 replace,只 present。
		ft2 := &fakeTarget{resultFor: func(cmd []string) *target.ExecResult {
			if len(cmd) >= 3 && cmd[0] == "ip" && cmd[1] == "route" && cmd[2] == "show" {
				return &target.ExecResult{ExitCode: 0, Stdout: "10.20.0.1 via 127.0.0.1 dev eth0\n10.20.0.2 via 127.0.0.1 dev eth0\n"}
			}
			return &target.ExecResult{ExitCode: 0}
		}}
		svc.tg = ft2
		svc.ReconcileForwardRoutes(ctx)
		for _, c := range ft2.execCalls {
			if len(c) >= 3 && c[0] == "ip" && c[2] == "replace" {
				t.Fatalf("在位路由不应重复 replace:%v", c)
			}
		}

		// 第三轮:目录不可达且超宽限 → 期望空 → 账本路由撤除。
		svc.SetMeshForward(nil, func(context.Context, string) (*mesh.ServiceCatalog, error) {
			return nil, mesh.ErrPeerUnreachable
		})
		svc.fwd.mu.Lock()
		for _, e := range svc.fwd.cache { // 直接耗尽陈旧宽限(单测不等 10 分钟)。
			e.fetchedAt = e.fetchedAt.Add(-forwardCatalogStale - time.Minute)
		}
		svc.fwd.mu.Unlock()
		svc.ReconcileForwardRoutes(ctx)
		if !hasCmd(ft2, "ip route del 10.20.0.1/32") || !hasCmd(ft2, "ip route del 10.20.0.2/32") {
			t.Fatalf("应撤除路由:%v", ft2.execCalls)
		}
		ledger, err = svc.store.getForwardRoutes(ctx)
		if err != nil || len(ledger["gw1"]) != 0 {
			t.Fatalf("账本应清空:%v err=%v", ledger, err)
		}
	})
}
