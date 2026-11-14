package servicereg

// local_gateway_test.go 覆盖本机(控制节点)网关角色语义(specs/local-node-roles.md 7.1/7.2/7.3):
//   - 回环端口校验(范围/互异/不得与集群端口相同;0/0 = 未配置合法);
//   - desiredPortBindings 的共存/直连分支与 HostIp;
//   - portsMatch 的 HostIp 归一(全网卡 ↔ 回环必须触发重建);
//   - UpdateSettings 的角色门禁(钩子未注入/未启用 → 拒绝 __local__);
//   - 渲染期混合网关守卫(本机承载实例 + 远端网关 → 显式失败;纯本机网关放行)。

import (
	"context"
	"testing"

	"github.com/huangchengsir/pipewright/internal/platformhttps"
	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/target"
)

func TestValidateSettingsLocalPorts(t *testing.T) {
	base := Settings{ServerIDs: []string{"srv1"}, HTTPPort: 80, HTTPSPort: 443,
		Image: defaultNginxImage, Network: defaultNetwork, ContainerName: defaultContainerName,
		VolumeName: defaultVolumeName, StaticRoot: defaultStaticRoot}

	// 0/0 = 未配置,合法(零值 Settings 兼容)。
	if err := validateSettings(base); err != nil {
		t.Fatalf("零值回环端口应合法:%v", err)
	}
	ok := base
	ok.LocalHTTPPort, ok.LocalHTTPSPort = 18080, 18443
	if err := validateSettings(ok); err != nil {
		t.Fatalf("合法回环端口应通过:%v", err)
	}
	for _, tc := range []struct{ name string; http, https int }{
		{"互异被破坏", 18443, 18443},
		{"与集群 HTTP 冲突", 80, 18443},
		{"与集群 HTTPS 冲突", 18080, 443},
		{"范围越界", 0, 70000},
		{"一端未配置", 0, 18443},
		{"负端口", -1, 18443},
		{"负端口(双向)", -18080, -18443},
	} {
		bad := base
		bad.LocalHTTPPort, bad.LocalHTTPSPort = tc.http, tc.https
		if err := validateSettings(bad); err == nil {
			t.Fatalf("%s 应拒绝", tc.name)
		}
	}
}

func TestDesiredPortBindings(t *testing.T) {
	st := &Settings{HTTPPort: 80, HTTPSPort: 443, LocalHTTPPort: 18080, LocalHTTPSPort: 18443}

	// 直连模式(或非本机):恒全网卡集群端口。
	got := desiredPortBindings(st, "srv1", []int{3306}, true)
	if got[80].HostIP != "" || got[80].HostPort != 80 || got[443].HostPort != 443 {
		t.Fatalf("直连模式端口错误:%+v", got)
	}
	if got[3306].HostIP != "" || got[3306].HostPort != 3306 {
		t.Fatalf("TCP 服务端口应全网卡:%+v", got[3306])
	}
	// 本机 + 直连:同样集群端口。
	got = desiredPortBindings(st, targetLocalID(), []int{}, false)
	if got[80].HostPort != 80 || got[80].HostIP != "" {
		t.Fatalf("本机直连应绑集群端口:%+v", got[80])
	}
	// 本机 + 共存:回环绑定 80/443,TCP 恒全网卡。
	got = desiredPortBindings(st, targetLocalID(), []int{3306}, true)
	if got[80].HostIP != "127.0.0.1" || got[80].HostPort != 18080 ||
		got[443].HostIP != "127.0.0.1" || got[443].HostPort != 18443 {
		t.Fatalf("共存模式应回环绑定本地端口:%+v", got)
	}
	if got[3306].HostIP != "" {
		t.Fatalf("共存模式 TCP 服务端口仍应全网卡:%+v", got[3306])
	}
	// 回环端口未配置(0)→ 回落集群端口(直连语义兜底)。
	st0 := &Settings{HTTPPort: 80, HTTPSPort: 443}
	got = desiredPortBindings(st0, targetLocalID(), nil, true)
	if got[80].HostPort != 80 || got[80].HostIP != "" {
		t.Fatalf("回环端口未配置应回落集群端口:%+v", got[80])
	}
}

func TestPortsMatchHostIPNormalization(t *testing.T) {
	cur := map[int]portBinding{
		80:  {HostIP: "0.0.0.0", HostPort: 80},   // docker 对 -p 80:80 的另一种返回形态
		443: {HostIP: "127.0.0.1", HostPort: 18443},
	}
	want := map[int]portBinding{
		80:  {HostIP: "", HostPort: 80},
		443: {HostIP: "127.0.0.1", HostPort: 18443},
	}
	if !portsMatch(cur, want) {
		t.Fatal("HostIp 空与 0.0.0.0 应视为同语义")
	}
	// 绑定地址变化(全网卡 → 回环)必须判定为不符(触发重建)。
	want[80] = portBinding{HostIP: "127.0.0.1", HostPort: 80}
	if portsMatch(cur, want) {
		t.Fatal("HostIp 变化应判定不符")
	}
}

func TestUpdateSettingsLocalGate(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		ctx := context.Background()
		local := target.LocalHostID

		// 钩子未注入:__local__ 必拒(哨兵走显式通道,不存在默认放行)。
		svc := New(st.DB, &fakeTarget{}, nil)
		if _, err := svc.UpdateSettings(ctx, SettingsUpdate{ServerIDs: &[]string{local}}); err == nil {
			t.Fatal("钩子未注入应拒绝 __local__")
		}
		// 钩子注入但角色未启用:仍拒。
		svc.SetLocalGatewayHooks(LocalGatewayHooks{GatewayEnabled: func() bool { return false }})
		if _, err := svc.UpdateSettings(ctx, SettingsUpdate{ServerIDs: &[]string{local}}); err == nil {
			t.Fatal("角色未启用应拒绝 __local__")
		}
		// 启用:接受;移出本机后再保存照常(共存逻辑不干扰既有路径)。
		svc.SetLocalGatewayHooks(LocalGatewayHooks{GatewayEnabled: func() bool { return true }})
		if _, err := svc.UpdateSettings(ctx, SettingsUpdate{ServerIDs: &[]string{local}}); err != nil {
			t.Fatalf("角色启用应接受 __local__:%v", err)
		}
		if _, err := svc.UpdateSettings(ctx, SettingsUpdate{ServerIDs: &[]string{"srv1"}}); err != nil {
			t.Fatalf("真实机器保存不受影响:%v", err)
		}
	})
}

func TestGuardMixedGateway(t *testing.T) {
	local := target.LocalHostID
	instances := []Instance{
		{ServiceID: "web", ServerID: local, Attached: true},
		{ServiceID: "web", ServerID: "srv1", Attached: true},
	}
	// 纯本机网关:放行(host.docker.internal 在本机网关容器内指向宿主,正确)。
	if err := guardMixedGateway(&Settings{ServerIDs: []string{local}}, instances); err != nil {
		t.Fatalf("纯本机网关应放行:%v", err)
	}
	// 本机不在网关主机列表:拒绝(远端网关容器解析不到本机实例)。
	if err := guardMixedGateway(&Settings{ServerIDs: []string{"srv1"}}, instances); err == nil {
		t.Fatal("本机不在网关列表应拒绝")
	}
	// 混合网关(本机 + 远端):拒绝 —— host.docker.internal 在远端容器指向远端宿主。
	if err := guardMixedGateway(&Settings{ServerIDs: []string{local, "srv1"}}, instances); err == nil {
		t.Fatal("混合网关下本机承载实例应拒绝")
	}
	// 混合网关但无本机实例:放行(实例全在远端,serverHost 原逻辑可达)。
	if err := guardMixedGateway(&Settings{ServerIDs: []string{local, "srv1"}}, []Instance{
		{ServiceID: "web", ServerID: "srv1", Attached: true},
	}); err != nil {
		t.Fatalf("混合网关无本机实例应放行:%v", err)
	}
	// detached 实例不参与渲染,不触发守卫。
	if err := guardMixedGateway(&Settings{ServerIDs: []string{local, "srv1"}}, []Instance{
		{ServiceID: "web", ServerID: local, Attached: false},
	}); err != nil {
		t.Fatalf("detached 实例不应触发守卫:%v", err)
	}
}

// targetLocalID 便于断言书写(与 target.LocalHostID 同值)。
func targetLocalID() string { return target.LocalHostID }

// fakeGatewayPublisher 记录入口 vhost 的发布/摘除调用。
type fakeGatewayPublisher struct {
	applied []platformhttps.GatewayEntrySpec
	removed int
}

func (f *fakeGatewayPublisher) ApplyGateway(_ context.Context, spec platformhttps.GatewayEntrySpec) error {
	f.applied = append(f.applied, spec)
	return nil
}

func (f *fakeGatewayPublisher) RemoveGateway(context.Context) error {
	f.removed++
	return nil
}

// TestSyncGatewayEntryGuards 覆盖入口 vhost 收敛的三条分支(specs 7.3):
//   - 共存 + 回环端口未配置 → 显式错误(绝不渲染 127.0.0.1:0);
//   - 无基域 → 摘除(幂等,不因空集失败);
//   - 本机网关角色关闭 → 摘除(即使仍在 ServerIDs 里)。
func TestSyncGatewayEntryGuards(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		ctx := context.Background()
		pub := &fakeGatewayPublisher{}
		svc := New(st.DB, &fakeTarget{}, nil)
		svc.SetLocalGatewayHooks(LocalGatewayHooks{
			GatewayEnabled: func() bool { return true },
			Coexistence:    func() bool { return true },
			Publisher:      pub,
		})
		bad := &Settings{ServerIDs: []string{target.LocalHostID}}
		if err := svc.syncGatewayEntry(ctx, bad); err == nil {
			t.Fatal("共存模式回环端口未配置应显式失败")
		}
		if len(pub.applied) != 0 || pub.removed != 0 {
			t.Fatalf("端口未配置不应触达发布面:%+v/%d", pub.applied, pub.removed)
		}

		spec := &Settings{ServerIDs: []string{target.LocalHostID}, LocalHTTPPort: 18080, LocalHTTPSPort: 18443}
		if err := svc.syncGatewayEntry(ctx, spec); err != nil {
			t.Fatalf("无基域时应幂等摘除:%v", err)
		}
		if pub.removed != 1 || len(pub.applied) != 0 {
			t.Fatalf("无基域应走摘除:%+v/%d", pub.applied, pub.removed)
		}

		svc.SetLocalGatewayHooks(LocalGatewayHooks{
			GatewayEnabled: func() bool { return false },
			Coexistence:    func() bool { return true },
			Publisher:      pub,
		})
		if err := svc.syncGatewayEntry(ctx, spec); err != nil {
			t.Fatalf("角色关闭应幂等摘除:%v", err)
		}
		if pub.removed != 2 {
			t.Fatalf("角色关闭应摘除入口 vhost,removed=%d", pub.removed)
		}
	})
}
