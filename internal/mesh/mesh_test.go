// mesh_test.go 是 mesh 领域的存取回归测试:
//   - TestSaveSettingsFullRoundtrip:全字段保存 → 逐字段读回(防"占位符/参数错位"类
//     bug 复发——历史上 network_secret 曾因此写串列,密钥静默丢失);
//   - TestSaveSettingsKeepsExistingSecret:密钥留空提交不覆盖已存密钥("留空不修改"语义);
//   - TestACLRoundtrip:三张授权单保存/判定/默认拒绝。
package mesh

import (
	"context"
	"testing"

	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/vault"
)

func testKey() *[32]byte {
	var k [32]byte
	for i := range k {
		k[i] = byte(i + 11)
	}
	return &k
}

func newTestService(t *testing.T) Service {
	t.Helper()
	st := storetest.Open(t)
	return New(st.DB, vault.New(st.DB, testKey()))
}

func strp(s string) *string { return &s }
func intp(i int) *int       { return &i }
func boolp(b bool) *bool    { return &b }

func TestSaveSettingsFullRoundtrip(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	st, err := svc.Settings(ctx)
	if err != nil {
		t.Fatalf("initial settings: %v", err)
	}
	if st.NodeID == "" || st.PublicKey == "" {
		t.Fatalf("identity not generated: %+v", st)
	}

	saved, err := svc.SaveSettings(ctx, SaveSettingsInput{
		Name:          strp("node-alpha"),
		Enabled:       boolp(true),
		APIPort:       intp(9090),
		EasyTierMode:  strp(ModeManaged),
		EasyTierRPC:   strp("127.0.0.1:15888"),
		NetworkName:   strp("pipewright-net"),
		NetworkSecret: strp("shared-secret-24-chars-xxxx"),
		VirtualIP:     strp("10.126.126.5"),
		Peers:         &[]string{"tcp://1.2.3.4:11010", "tcp://5.6.7.8:11010", "tcp://1.2.3.4:11010"},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	// 逐字段断言:任何一列错位都会在这里现形。
	if saved.Name != "node-alpha" || !saved.Enabled || saved.APIPort != 9090 {
		t.Fatalf("identity fields mismatch: %+v", saved)
	}
	if saved.EasyTierMode != ModeManaged || saved.EasyTierRPC != "127.0.0.1:15888" {
		t.Fatalf("easytier fields mismatch: %+v", saved)
	}
	if saved.NetworkName != "pipewright-net" || saved.VirtualIP != "10.126.126.5" {
		t.Fatalf("network fields mismatch: %+v", saved)
	}
	if len(saved.Peers) != 2 || saved.Peers[0] != "tcp://1.2.3.4:11010" {
		t.Fatalf("peers not deduped/trimmed: %+v", saved.Peers)
	}
	if !saved.HasSecret {
		t.Fatalf("HasSecret should be true after saving secret")
	}
	secret, err := svc.NetworkSecret(ctx)
	if err != nil || secret != "shared-secret-24-chars-xxxx" {
		t.Fatalf("NetworkSecret = %q err=%v (列错位会表现为空串/密文不可解)", secret, err)
	}
	// 身份不受保存影响;私钥仍可解出。
	if saved.NodeID != st.NodeID {
		t.Fatalf("node id changed across save: %q -> %q", st.NodeID, saved.NodeID)
	}
	if _, err := svc.PrivateKey(ctx); err != nil {
		t.Fatalf("private key unreadable after save: %v", err)
	}
}

func TestSaveSettingsKeepsExistingSecret(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	if _, err := svc.SaveSettings(ctx, SaveSettingsInput{NetworkSecret: strp("first-secret")}); err != nil {
		t.Fatalf("save secret: %v", err)
	}
	// 二次保存不带密钥(NetworkSecret = nil,即前端"留空不修改"路径)。
	after, err := svc.SaveSettings(ctx, SaveSettingsInput{Name: strp("renamed")})
	if err != nil {
		t.Fatalf("save without secret: %v", err)
	}
	if after.Name != "renamed" || !after.HasSecret {
		t.Fatalf("fields mismatch: %+v", after)
	}
	if secret, err := svc.NetworkSecret(ctx); err != nil || secret != "first-secret" {
		t.Fatalf("secret must survive no-secret save: %q err=%v", secret, err)
	}
}

func TestACLRoundtrip(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	// 默认全拒绝(含 0085 新增的 forward 单)。
	if svc.AllowsVisible(ctx, "n1") || svc.AllowsPush(ctx, "n1") || svc.AllowsTrigger(ctx, "n1") || svc.AllowsForward(ctx, "n1") {
		t.Fatalf("default must be deny-all")
	}
	if err := svc.SaveACL(ctx, ACL{
		VisibleTo:      []string{"n1", "n2", ""},
		PushAllowed:    []string{"n1"},
		TriggerAllowed: []string{"n2"},
		ForwardAllowed: []string{"n3", "n3"},
	}); err != nil {
		t.Fatalf("save acl: %v", err)
	}
	if !svc.AllowsVisible(ctx, "n1") || !svc.AllowsVisible(ctx, "n2") {
		t.Fatalf("visible grant broken")
	}
	if !svc.AllowsPush(ctx, "n1") || svc.AllowsPush(ctx, "n2") {
		t.Fatalf("push grant broken")
	}
	if !svc.AllowsTrigger(ctx, "n2") || svc.AllowsTrigger(ctx, "n1") {
		t.Fatalf("trigger grant broken")
	}
	if !svc.AllowsForward(ctx, "n3") || svc.AllowsForward(ctx, "n1") {
		t.Fatalf("forward grant broken")
	}
	if svc.AllowsVisible(ctx, "n3") {
		t.Fatalf("unlisted node must stay denied")
	}
	// 回读:四张单齐备(空单归一为 [])。
	acl, err := svc.ACL(ctx)
	if err != nil {
		t.Fatalf("load acl: %v", err)
	}
	if len(acl.ForwardAllowed) != 1 || acl.ForwardAllowed[0] != "n3" {
		t.Fatalf("forward acl roundtrip: %+v", acl.ForwardAllowed)
	}
}

// TestRouteWhitelist 验证路由隔离白名单:自动项仅 /32(本端网关 + 路由表发现的远端
// 网关),自定义网段(非 /32)绝不自动进入;手动接受项并入;排序去重;空 = 拒绝全部。
func TestRouteWhitelist(t *testing.T) {
	got := RouteWhitelist(
		[]string{"10.1.1.7/32", "10.1.1.7/32", ""},      // 本端网关(重复/空)
		[]string{"192.168.50.0/24", "10.2.2.9/32", ""},  // 路由表:自定义网段必须被忽略,仅取 /32
		[]string{"172.20.0.0/16"},                       // 手动接受
	)
	want := []string{"10.1.1.7/32", "10.2.2.9/32", "172.20.0.0/16"}
	if len(got) != len(want) {
		t.Fatalf("whitelist = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("whitelist[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
	if all := RouteWhitelist(nil, nil, nil); len(all) != 0 {
		t.Fatalf("空输入应为空列表(fail-closed),got %v", all)
	}
}

// TestSaveSettingsRejectsHostRouteAdvertise 验证通告侧禁止 /32(该粒度保留给网关
// 自动通告,保证「/32 即网关」分类成立),显式接受侧不受限。
func TestSaveSettingsRejectsHostRouteAdvertise(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	bad := []string{"192.168.30.0/24", "10.9.9.9/32"}
	if _, err := svc.SaveSettings(ctx, SaveSettingsInput{ProxyNetworksCustom: &bad}); err == nil {
		t.Fatal("通告 /32 应被拒绝")
	}
	good := []string{"192.168.30.0/24"}
	st, err := svc.SaveSettings(ctx, SaveSettingsInput{ProxyNetworksCustom: &good})
	if err != nil {
		t.Fatalf("合法通告失败:%v", err)
	}
	if len(st.ProxyNetworksCustom) != 1 || st.ProxyNetworksCustom[0] != "192.168.30.0/24" {
		t.Fatalf("通告回显 = %v", st.ProxyNetworksCustom)
	}
	accept := []string{"10.9.9.9/32", "172.16.0.0/12"}
	st, err = svc.SaveSettings(ctx, SaveSettingsInput{AcceptNetworksCustom: &accept})
	if err != nil {
		t.Fatalf("显式接受 /32 应允许:%v", err)
	}
	if len(st.AcceptNetworksCustom) != 2 {
		t.Fatalf("接受列表回显 = %v", st.AcceptNetworksCustom)
	}
}

// TestSaveSettingsEnableExitNode 验证出口节点开关存取回路(默认关)。
func TestSaveSettingsEnableExitNode(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	st, err := svc.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.EnableExitNode {
		t.Fatal("默认应关闭")
	}
	on := true
	st, err = svc.SaveSettings(ctx, SaveSettingsInput{EnableExitNode: &on})
	if err != nil {
		t.Fatal(err)
	}
	if !st.EnableExitNode {
		t.Fatal("保存后应开启")
	}
	off := false
	st, err = svc.SaveSettings(ctx, SaveSettingsInput{EnableExitNode: &off})
	if err != nil {
		t.Fatal(err)
	}
	if st.EnableExitNode {
		t.Fatal("应能关闭")
	}
}
