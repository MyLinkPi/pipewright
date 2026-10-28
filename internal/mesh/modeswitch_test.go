package mesh

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestCredentialRecordLifecycle 覆盖登记簿:签发登记 → 列表 → 撤销标记(幂等)→
// 未知 ID 撤销报 ErrNotFound;TTL/备注护栏。
func TestCredentialRecordLifecycle(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	rec, err := svc.IssueCredentialRecord(ctx, "cred-1", "手机测试", 86400, "admin", "admin")
	if err != nil {
		t.Fatalf("登记失败:%v", err)
	}
	if rec.ExpiresAt == "" || rec.Source != "admin" {
		t.Fatalf("登记行字段异常:%+v", rec)
	}
	// 同 ID 重复登记幂等返回既有行。
	again, err := svc.IssueCredentialRecord(ctx, "cred-1", "重复", 3600, "admin", "admin")
	if err != nil || again.CredentialID != "cred-1" || again.Remark != "手机测试" {
		t.Fatalf("重复登记应幂等:got %+v err %v", again, err)
	}

	list, err := svc.ListCredentialRecords(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("列表应 1 条:got %d err %v", len(list), err)
	}

	if err := svc.MarkCredentialRevoked(ctx, "cred-1"); err != nil {
		t.Fatalf("撤销失败:%v", err)
	}
	if err := svc.MarkCredentialRevoked(ctx, "cred-1"); err != nil {
		t.Fatalf("重复撤销应幂等:%v", err)
	}
	list, _ = svc.ListCredentialRecords(ctx)
	if list[0].RevokedAt == "" {
		t.Fatal("撤销时间未落库")
	}
	if err := svc.MarkCredentialRevoked(ctx, "ghost"); err != ErrNotFound {
		t.Fatalf("未知 ID 应 ErrNotFound:got %v", err)
	}

	if _, err := svc.IssueCredentialRecord(ctx, "cred-2", "", 0, "admin", "admin"); err == nil {
		t.Fatal("ttl=0 应拒绝")
	}
	if _, err := svc.IssueCredentialRecord(ctx, "cred-2", strings.Repeat("长", 201), 60, "admin", "admin"); err == nil {
		t.Fatal("备注超长应拒绝")
	}
}

// TestApiTokenLifecycle 覆盖令牌:创建(明文一次性、pwmc_ 前缀)→ 查验有效/失效 →
// 撤销 → 重名拒绝 → last_used_at 刷新。
func TestApiTokenLifecycle(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	rec, plaintext, err := svc.CreateApiToken(ctx, "ci-bot")
	if err != nil {
		t.Fatalf("创建失败:%v", err)
	}
	if !strings.HasPrefix(plaintext, "pwmc_") || len(plaintext) < 40 {
		t.Fatalf("明文形态异常:%q", plaintext)
	}
	if name := svc.ApiTokenLookup(ctx, plaintext); name != "ci-bot" {
		t.Fatalf("查验应命中:got %q", name)
	}
	if name := svc.ApiTokenLookup(ctx, plaintext+"x"); name != "" {
		t.Fatalf("篡改令牌不应命中:got %q", name)
	}
	if name := svc.ApiTokenLookup(ctx, ""); name != "" {
		t.Fatal("空令牌不应命中")
	}
	// last_used_at 尽力刷新:查一次后列表可见非空。
	tokens, _ := svc.ListApiTokens(ctx)
	if len(tokens) != 1 || tokens[0].LastUsedAt == "" {
		t.Fatalf("last_used_at 应刷新:%+v", tokens)
	}
	_ = rec

	if _, _, err := svc.CreateApiToken(ctx, "ci-bot"); err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("重名应拒绝:got %v", err)
	}
	if err := svc.RevokeApiToken(ctx, "ci-bot"); err != nil {
		t.Fatalf("撤销失败:%v", err)
	}
	if name := svc.ApiTokenLookup(ctx, plaintext); name != "" {
		t.Fatal("撤销后查验应失效")
	}
	if err := svc.RevokeApiToken(ctx, "ci-bot"); err == nil {
		t.Fatal("重复撤销按未找到报错")
	}
}

// TestModePendingLifecycle 覆盖本地暂存:保存/读取、清除护栏(仅匹配 ID 且未应用)、
// 到期应用翻转 secure_mode 并幂等。
func TestModePendingLifecycle(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// 新部署默认 Secure Mode 开启;本用例验证 enable 翻转,先显式关闭作为起点。
	off := false
	if _, err := svc.SaveSettings(ctx, SaveSettingsInput{SecureMode: &off}); err != nil {
		t.Fatalf("关闭 Secure Mode: %v", err)
	}

	p := ModePending{ChangeID: "ch-1", Action: ModeActionEnable,
		EffectiveAt: now.Add(time.Hour), InitiatedBy: "n1", AckedAt: now}
	if err := svc.SaveModePending(ctx, p); err != nil {
		t.Fatalf("保存失败:%v", err)
	}
	got, err := svc.GetModePending(ctx)
	if err != nil || got == nil || got.ChangeID != "ch-1" || got.Action != ModeActionEnable {
		t.Fatalf("读取异常:%+v err %v", got, err)
	}
	bad := p
	bad.Action = "boom"
	if err := svc.SaveModePending(ctx, bad); err == nil {
		t.Fatal("非法 action 应拒绝")
	}

	// 未到期不应用;到期应用翻转 + 幂等。
	if applied, err := svc.ApplyDueModeChange(ctx, now); err != nil || applied {
		t.Fatalf("未到期不应应用:applied=%v err=%v", applied, err)
	}
	st, _ := svc.Settings(ctx)
	if st.SecureMode {
		t.Fatal("secure_mode 不应被提前翻转")
	}
	if applied, err := svc.ApplyDueModeChange(ctx, now.Add(2*time.Hour)); err != nil || !applied {
		t.Fatalf("到期应应用:applied=%v err=%v", applied, err)
	}
	st, _ = svc.Settings(ctx)
	if !st.SecureMode {
		t.Fatal("enable 未翻转 secure_mode")
	}
	if applied, _ := svc.ApplyDueModeChange(ctx, now.Add(3*time.Hour)); applied {
		t.Fatal("已应用应幂等 no-op")
	}

	// 清除护栏:ID 不匹配不清;已应用的行 ClearModePending 不删(保留 applied 痕迹,
	// 新指令到达时 SaveModePending 覆盖)。
	if err := svc.ClearModePending(ctx, "other"); err != nil {
		t.Fatalf("清除调用不应报错:%v", err)
	}
	if got, _ := svc.GetModePending(ctx); got == nil {
		t.Fatal("ID 不匹配不应清除")
	}
}

// enableMeshFor 启用集群并返回设置(开关协议测试的前置)。新部署默认 Secure Mode
// 开启,开关用例统一显式关闭作为起点,以便测试 enable 流程。
func enableMeshFor(t *testing.T, svc Service) (*Settings, error) {
	t.Helper()
	off := false
	return svc.SaveSettings(context.Background(), SaveSettingsInput{
		Enabled:     boolp(true),
		NetworkName: strp("net"),
		SecureMode:  &off,
	})
}

// TestModeSwitcherSingleNode 单节点集群:Initiate 全程本端路径 —— armed、指令落库、
// 前置拒绝(非法动作/已是目标态/进行中指令),Abort 在到期前可撤销。
func TestModeSwitcherSingleNode(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	if _, err := enableMeshFor(t, svc); err != nil {
		t.Fatalf("启用集群:%v", err)
	}
	sw := NewModeSwitcher(svc, nil) // 无对端:单节点路径

	if _, err := sw.Initiate(ctx, "boom", 2); err == nil {
		t.Fatal("非法 action 应拒绝")
	}
	res, err := sw.Initiate(ctx, ModeActionEnable, 2)
	if err != nil || !res.Armed {
		t.Fatalf("单节点应 armed:%+v err %v", res, err)
	}
	if res.EffectiveAt.Before(time.Now().UTC().Add(time.Minute)) {
		t.Fatalf("生效时刻应至少留 1 分钟:%v", res.EffectiveAt)
	}
	if pending, _ := svc.GetModePending(ctx); pending == nil || pending.ChangeID != res.ChangeID {
		t.Fatalf("本端指令应落库:%+v", pending)
	}
	// 进行中指令叠加拒绝。
	if _, err := sw.Initiate(ctx, ModeActionEnable, 2); err == nil {
		t.Fatal("进行中指令应拒绝叠加")
	}
	// Abort 后可重新发起。
	if err := sw.Abort(ctx, res.ChangeID); err != nil {
		t.Fatalf("撤销失败:%v", err)
	}
	if pending, _ := svc.GetModePending(ctx); pending != nil {
		t.Fatal("撤销后指令应清除")
	}
	if err := sw.Abort(ctx, "ghost"); err != ErrNotFound {
		t.Fatalf("未知指令应 ErrNotFound:got %v", err)
	}
}

// TestModeSwitcherSkippedPeer 无虚拟 IP 的对端不经 mesh 寻址,无法纳入齐切:Initiate
// 在可达侧全部确认后仍 armed,但显式报出 skipped 名单;Status 同步暴露(不静默混跑),
// 且该对端不伪装成在线节点。
func TestModeSwitcherSkippedPeer(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	if _, err := enableMeshFor(t, svc); err != nil {
		t.Fatalf("启用集群:%v", err)
	}
	if err := svc.SaveManualPeer(ctx, Peer{NodeID: "peer-novip", Name: "无IP节点"}); err != nil {
		t.Fatalf("登记无虚拟 IP 对端:%v", err)
	}
	sw := NewModeSwitcher(svc, nil) // 无可达对端:单节点路径

	res, err := sw.Initiate(ctx, ModeActionEnable, 2)
	if err != nil || !res.Armed {
		t.Fatalf("应 armed:%+v err %v", res, err)
	}
	if len(res.SkippedNodes) != 1 || res.SkippedNodes[0] != "无IP节点" {
		t.Fatalf("skipped 名单异常:%v", res.SkippedNodes)
	}
	st, err := sw.Status(ctx)
	if err != nil {
		t.Fatalf("status:%v", err)
	}
	if len(st.Skipped) != 1 || st.Skipped[0] != "无IP节点" {
		t.Fatalf("状态 skipped 异常:%v", st.Skipped)
	}
	if len(st.Nodes) != 1 || !st.Nodes[0].IsSelf {
		t.Fatalf("跳过节点不应进节点表:%+v", st.Nodes)
	}
}

// TestModeSwitcherFanoutFailure 有对端但不可达:fan-out 失败 → aborted + 回滚本端指令,
// 失败名单含对端标识。
func TestModeSwitcherFanoutFailure(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	st, err := enableMeshFor(t, svc)
	if err != nil {
		t.Fatalf("启用集群:%v", err)
	}
	// 登记一个不可达对端(虚拟 IP 指向保留段,连接必然失败)。
	if err := svc.ApplyHello(ctx, "192.0.2.9", HelloInfo{
		NodeID: "peer-1", Name: "离线节点", PublicKey: "aa", APIPort: 8080,
	}); err != nil {
		t.Fatalf("登记对端:%v", err)
	}
	sw := NewModeSwitcher(svc, NewClient(svc)) // 真客户端,目标不可达
	_ = st
	res, err := sw.Initiate(ctx, ModeActionEnable, 2)
	if err != nil {
		t.Fatalf("Initiate 不应直接报错(失败是业务结果):%v", err)
	}
	if res.Armed {
		t.Fatal("对端不可达不应 armed")
	}
	if len(res.FailedNodes) != 1 || res.FailedNodes[0] != "离线节点" {
		t.Fatalf("失败名单应含对端名:%v", res.FailedNodes)
	}
	if pending, _ := svc.GetModePending(ctx); pending != nil {
		t.Fatal("aborted 后本端指令应回滚")
	}
}

// TestModeSwitcherNilClientReachablePeer nil client + 可达对端(有虚拟 IP)的组合:
// fanOutChange 必须按失败计并 aborted 回滚,而不是 nil 解引用 panic(goroutine 内
// panic 无 recover 会崩整个进程;生产装配恒传非 nil,此测试钉死防御契约)。
func TestModeSwitcherNilClientReachablePeer(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	if _, err := enableMeshFor(t, svc); err != nil {
		t.Fatalf("启用集群:%v", err)
	}
	if err := svc.ApplyHello(ctx, "192.0.2.9", HelloInfo{
		NodeID: "peer-1", Name: "对端节点", PublicKey: "aa", APIPort: 8080,
	}); err != nil {
		t.Fatalf("登记对端:%v", err)
	}
	sw := NewModeSwitcher(svc, nil) // nil client:fanOutChange 应防护而非 panic
	res, err := sw.Initiate(ctx, ModeActionEnable, 2)
	if err != nil {
		t.Fatalf("Initiate 不应报错(失败是业务结果):%v", err)
	}
	if res.Armed {
		t.Fatal("nil client 下对端全部失败,不应 armed")
	}
	if len(res.FailedNodes) != 1 || res.FailedNodes[0] != "对端节点" {
		t.Fatalf("失败名单应含对端名:%v", res.FailedNodes)
	}
	if pending, _ := svc.GetModePending(ctx); pending != nil {
		t.Fatal("aborted 后本端指令应回滚")
	}
}
