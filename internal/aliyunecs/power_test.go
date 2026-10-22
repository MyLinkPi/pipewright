package aliyunecs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/target"
)

// collectLogs 返回记录进 slice 的 Log 回调。
func collectLogs() (*[]string, func(string)) {
	var logs []string
	return &logs, func(msg string) { logs = append(logs, msg) }
}

func mustServer(t *testing.T, env *testEnv, name, host, labels string) *target.Server {
	t.Helper()
	srv, err := env.target.Create(context.Background(), target.CreateInput{
		Name: name, Host: host, User: "root", CredentialID: env.newSSHCred(t, "c-"+name), Labels: labels,
	})
	if err != nil {
		t.Fatalf("create server %s: %v", name, err)
	}
	return srv
}

func mustLink(t *testing.T, env *testEnv, accountID, instanceID, serverID string) {
	t.Helper()
	if err := env.svc.(*service).upsertLink(context.Background(), accountID, instanceID, serverID); err != nil {
		t.Fatalf("link %s: %v", instanceID, err)
	}
}

func TestPowerDirectStartAndSkip(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	logs, logFn := collectLogs()
	env.fake.statuses = map[string]string{"i-1": "Stopped", "i-2": "Running", "i-gone": ""}

	res, err := env.svc.Power(ctx, PowerInput{
		AccountID:   env.account.ID,
		Selector:    SelectorInstances,
		InstanceIDs: []string{"i-1", "i-2", "i-gone"},
		Action:      PowerActionStart,
		WaitSeconds: 0,
		Log:         logFn,
	})
	if err != nil {
		t.Fatalf("Power: %v", err)
	}
	if res.Total != 3 || res.OK != 2 || res.Failed != 1 {
		t.Fatalf("res = %+v", res)
	}
	// i-1 发起开机;i-2 已运行跳过;i-gone 不存在逐台失败。
	if len(env.fake.started) != 1 || env.fake.started[0] != "i-1" {
		t.Errorf("started = %v", env.fake.started)
	}
	byID := map[string]PowerItemResult{}
	for _, it := range res.Items {
		byID[it.InstanceID] = it
	}
	if !byID["i-2"].Skipped || !byID["i-2"].OK {
		t.Errorf("i-2 应幂等跳过: %+v", byID["i-2"])
	}
	if byID["i-gone"].OK || !strings.Contains(byID["i-gone"].Err, "不存在") {
		t.Errorf("i-gone = %+v", byID["i-gone"])
	}
	if !strings.Contains(strings.Join(*logs, "\n"), "已是 Running,跳过") {
		t.Errorf("跳过应记日志: %v", *logs)
	}
}

func TestPowerStopEconomyModeAndForce(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	env.fake.statuses = map[string]string{"i-1": "Running", "i-2": "Running"}
	env.fake.powerErr = map[string]error{
		"i-2": errors.New("aliyunecs: list instances failed:经典网络实例不支持节省停机模式"),
	}

	res, err := env.svc.Power(ctx, PowerInput{
		AccountID:   env.account.ID,
		InstanceIDs: []string{"i-1", "i-2"},
		Action:      PowerActionStop,
		ForceStop:   true,
		WaitSeconds: 0,
	})
	if err != nil {
		t.Fatalf("Power: %v", err)
	}
	if res.OK != 1 || res.Failed != 1 {
		t.Fatalf("res = %+v", res)
	}
	// 关机走 StopInstance;force 透传;i-2 被阿里云拒绝逐台失败。
	if len(env.fake.stopped) != 1 || env.fake.stopped[0] != "i-1" || !env.fake.stopForce[0] {
		t.Errorf("stopped = %v force = %v", env.fake.stopped, env.fake.stopForce)
	}
	for _, it := range res.Items {
		if it.InstanceID == "i-2" && !strings.Contains(it.Err, "节省停机") {
			t.Errorf("i-2 err = %q", it.Err)
		}
	}
}

func TestPowerWaitUntilTarget(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	// i-1 处于 Starting:不重复发起,直接等待;首轮轮询后迁移到 Running。
	env.fake.statuses = map[string]string{"i-1": statusStarting}
	polls := 0
	env.fake.pollHook = func() {
		polls++
		env.fake.statuses["i-1"] = statusRunning
	}
	res, err := env.svc.Power(ctx, PowerInput{
		AccountID:   env.account.ID,
		InstanceIDs: []string{"i-1"},
		Action:      PowerActionStart,
		WaitSeconds: 60,
	})
	if err != nil {
		t.Fatalf("Power: %v", err)
	}
	if res.OK != 1 || res.Failed != 0 {
		t.Fatalf("res = %+v", res)
	}
	if len(env.fake.started) != 0 {
		t.Errorf("转换中不应重复发起: %v", env.fake.started)
	}
	if polls < 1 {
		t.Errorf("应至少轮询一次")
	}
}

func TestPowerWaitTimeout(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	// 一直 Stopping,永不 Stopped → 超时改判失败并带最后状态。
	env.fake.statuses = map[string]string{"i-1": statusStopping}
	start := time.Now()
	res, err := env.svc.Power(ctx, PowerInput{
		AccountID:   env.account.ID,
		InstanceIDs: []string{"i-1"},
		Action:      PowerActionStop,
		WaitSeconds: 1,
	})
	if err != nil {
		t.Fatalf("Power: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("超时收尾过慢: %v", elapsed)
	}
	if res.OK != 0 || res.Failed != 1 {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.Items[0].Err, "超时") || !strings.Contains(res.Items[0].Err, statusStopping) {
		t.Errorf("err = %q, want 超时 + 最后状态", res.Items[0].Err)
	}
}

func TestPowerAliyunTagSelector(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	env.fake.instances = []Instance{
		{InstanceID: "i-1", Status: statusStopped, Tags: map[string]string{"env": "staging", "team": "web"}},
		{InstanceID: "i-2", Status: statusStopped, Tags: map[string]string{"env": "prod"}},
		{InstanceID: "i-3", Status: statusStopped, Tags: map[string]string{"env": "staging", "team": "db"}},
	}
	env.fake.statuses = map[string]string{"i-1": statusStopped}

	// AND 语义:env=staging + team=db 只命中 i-3;key-only(team 任意值)与 env=staging 命中 i-1/i-3。
	res, err := env.svc.Power(ctx, PowerInput{
		AccountID:   env.account.ID,
		Selector:    SelectorTags,
		TagSource:   TagSourceAliyun,
		Tags:        map[string]string{"env": "staging", "team": "db"},
		Action:      PowerActionStart,
		WaitSeconds: 0,
	})
	if err != nil {
		t.Fatalf("Power: %v", err)
	}
	if res.Total != 1 || res.Items[0].InstanceID != "i-3" {
		t.Fatalf("res = %+v, want 仅 i-3", res)
	}

	res2, err := env.svc.Power(ctx, PowerInput{
		AccountID: env.account.ID,
		Selector:  SelectorTags,
		Tags:      map[string]string{"env": "staging", "team": ""},
		Action:    PowerActionStart,
	})
	if err != nil {
		t.Fatalf("Power key-only: %v", err)
	}
	if res2.Total != 2 {
		t.Fatalf("key-only 应命中 i-1/i-3, got %+v", res2)
	}

	// 0 命中 → 整批失败。
	_, err = env.svc.Power(ctx, PowerInput{
		AccountID: env.account.ID,
		Selector:  SelectorTags,
		Tags:      map[string]string{"env": "nope"},
		Action:    PowerActionStart,
	})
	if !errors.Is(err, ErrNoTagMatch) {
		t.Errorf("0 命中 err = %v, want ErrNoTagMatch", err)
	}
}

func TestPowerPlatformTagSelector(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()

	// 三台机器:导入过(有映射)/未导入/标签不匹配;另加一台「独占标签且未导入」验证全无映射路径。
	imported := mustServer(t, env, "已导入", "10.0.0.1", "env=staging,team=web")
	unlinked := mustServer(t, env, "未导入", "10.0.0.2", "env=staging")
	mustServer(t, env, "不匹配", "10.0.0.3", "env=prod")
	mustServer(t, env, "纯未导入", "10.0.0.9", "solo=1")
	mustLink(t, env, env.account.ID, "i-imp", imported.ID)
	env.fake.statuses = map[string]string{"i-imp": statusStopped}
	logs, logFn := collectLogs()

	res, err := env.svc.Power(ctx, PowerInput{
		AccountID:   env.account.ID,
		Selector:    SelectorTags,
		TagSource:   TagSourcePlatform,
		Tags:        map[string]string{"env": "staging"},
		Action:      PowerActionStart,
		WaitSeconds: 0,
		Log:         logFn,
	})
	if err != nil {
		t.Fatalf("Power: %v", err)
	}
	if res.Total != 1 || res.Items[0].InstanceID != "i-imp" || res.Items[0].ServerID != imported.ID {
		t.Fatalf("res = %+v, want 仅已导入机器的实例", res)
	}
	if !strings.Contains(strings.Join(*logs, "\n"), unlinked.Name) {
		t.Errorf("未导入机器应记跳过日志: %v", *logs)
	}

	// 平台标签 AND 语义:env=staging + team=web 只剩已导入那台(未导入的不带 team=web 也命中
	// 不了——它只有 env=staging)。
	res2, err := env.svc.Power(ctx, PowerInput{
		AccountID: env.account.ID,
		Selector:  SelectorTags,
		TagSource: TagSourcePlatform,
		Tags:      map[string]string{"env": "staging", "team": "web"},
		Action:    PowerActionStart,
	})
	if err != nil || res2.Total != 1 || res2.Items[0].InstanceID != "i-imp" {
		t.Fatalf("res2 = %+v err=%v", res2, err)
	}

	// 全部命中机器均未导入 → 失败(防静默空跑)。
	_, err = env.svc.Power(ctx, PowerInput{
		AccountID: env.account.ID,
		Selector:  SelectorTags,
		TagSource: TagSourcePlatform,
		Tags:      map[string]string{"solo": "1"},
		Action:    PowerActionStart,
	})
	if !errors.Is(err, ErrNoTagMatch) {
		t.Errorf("无映射可操作 err = %v, want ErrNoTagMatch", err)
	}
	// 平台标签完全无命中 → 同样失败。
	_, err = env.svc.Power(ctx, PowerInput{
		AccountID: env.account.ID,
		Selector:  SelectorTags,
		TagSource: TagSourcePlatform,
		Tags:      map[string]string{"env": "staging", "no": "link"},
		Action:    PowerActionStart,
	})
	if !errors.Is(err, ErrNoTagMatch) {
		t.Errorf("无命中 err = %v, want ErrNoTagMatch", err)
	}
}

func TestPowerValidation(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()

	cases := []struct {
		name string
		in   PowerInput
		want error
	}{
		{"非法动作", PowerInput{AccountID: env.account.ID, InstanceIDs: []string{"i-1"}, Action: "reboot"}, ErrPowerFailed},
		{"未知账户", PowerInput{AccountID: "nope", InstanceIDs: []string{"i-1"}, Action: PowerActionStart}, ErrNotFound},
		{"等待超上限", PowerInput{AccountID: env.account.ID, InstanceIDs: []string{"i-1"}, Action: PowerActionStart, WaitSeconds: powerMaxWaitSeconds + 1}, ErrPowerFailed},
		{"空标签条件", PowerInput{AccountID: env.account.ID, Selector: SelectorTags, Action: PowerActionStart}, ErrNoTagMatch},
		{"非法标签来源", PowerInput{AccountID: env.account.ID, Selector: SelectorTags, TagSource: "both", Tags: map[string]string{"a": "b"}, Action: PowerActionStart}, ErrPowerFailed},
	}
	for _, tc := range cases {
		if _, err := env.svc.Power(ctx, tc.in); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}

	// 超批量上限(唯一 id,去重不生效)。
	uniq := make([]string, MaxPowerBatch+1)
	for i := range uniq {
		uniq[i] = "i-" + itoaPad(i)
	}
	env.fake.statuses = map[string]string{}
	if _, err := env.svc.Power(ctx, PowerInput{AccountID: env.account.ID, InstanceIDs: uniq, Action: PowerActionStart}); !errors.Is(err, ErrPowerFailed) {
		t.Errorf("超上限 err = %v, want ErrPowerFailed", err)
	}
}

func itoaPad(n int) string {
	digits := "0123456789abcdef"
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{digits[n%16]}, b...)
		n /= 16
	}
	return string(b)
}

// ─── waitReachable(开机到达 Running 后继续等 TCP 可达) ─────────────────────────

// stubProbe 注入探测桩:记录每次探测的地址,按 okSeq 序号决定成败(越界一律失败)。
func stubProbe(env *testEnv, okSeq []bool) *[][2]any {
	var calls [][2]any
	env.svc.(*service).probeTCP = func(_ context.Context, host string, port int, _ time.Duration) bool {
		calls = append(calls, [2]any{host, port})
		i := len(calls) - 1
		if i < len(okSeq) {
			return okSeq[i]
		}
		return false
	}
	return &calls
}

func TestPowerWaitReachableProbesInstanceIP(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	// 转换中 → 等到 Running;无导入映射 → 地址取阿里云公网 IP(公网优先),端口兜底 22。
	env.fake.instances = []Instance{{InstanceID: "i-1", PrivateIPs: []string{"10.0.0.1"}, PublicIPs: []string{"1.2.3.4"}, Status: statusStarting}}
	env.fake.statuses = map[string]string{"i-1": statusStarting}
	env.fake.pollHook = func() { env.fake.statuses["i-1"] = statusRunning }
	calls := stubProbe(env, []bool{true})

	res, err := env.svc.Power(ctx, PowerInput{
		AccountID: env.account.ID, InstanceIDs: []string{"i-1"},
		Action: PowerActionStart, WaitSeconds: 30, WaitReachable: true,
	})
	if err != nil {
		t.Fatalf("Power: %v", err)
	}
	if res.OK != 1 || res.Failed != 0 {
		t.Fatalf("res = %+v", res)
	}
	if len(*calls) == 0 || (*calls)[0][0] != "1.2.3.4" || (*calls)[0][1] != 22 {
		t.Fatalf("探测地址应取公网 IP:22, got %v", *calls)
	}
	if res.Items[0].Skipped {
		t.Errorf("可达等待的台不应记跳过: %+v", res.Items[0])
	}
}

func TestPowerWaitReachablePrefersServerRow(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	// 已导入机器行(自定义端口):探测地址用行上 Host:Port(与 deploy_ssh 同源)。
	srv, err := env.target.Create(ctx, target.CreateInput{
		Name: "带端口", Host: "10.0.0.6", Port: 2222, User: "root",
		CredentialID: env.newSSHCred(t, "c-port"), Labels: "env=staging",
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	mustLink(t, env, env.account.ID, "i-1", srv.ID)
	env.fake.instances = []Instance{{InstanceID: "i-1", PublicIPs: []string{"1.2.3.4"}, Status: statusRunning}}
	env.fake.statuses = map[string]string{"i-1": statusRunning}
	calls := stubProbe(env, []bool{true})

	res, err := env.svc.Power(ctx, PowerInput{
		AccountID: env.account.ID, InstanceIDs: []string{"i-1"},
		Action: PowerActionStart, WaitSeconds: 30, WaitReachable: true,
	})
	if err != nil {
		t.Fatalf("Power: %v", err)
	}
	if res.OK != 1 || res.Failed != 0 {
		t.Fatalf("res = %+v", res)
	}
	if len(*calls) == 0 || (*calls)[0][0] != "10.0.0.6" || (*calls)[0][1] != 2222 {
		t.Fatalf("探测地址应取机器行 Host:Port, got %v", *calls)
	}
}

func TestPowerWaitReachableCustomPortFallback(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	// 无映射实例:reachablePort 兜底端口生效。
	env.fake.instances = []Instance{{InstanceID: "i-1", PrivateIPs: []string{"10.0.0.1"}, Status: statusRunning}}
	env.fake.statuses = map[string]string{"i-1": statusRunning}
	calls := stubProbe(env, []bool{true})

	_, err := env.svc.Power(ctx, PowerInput{
		AccountID: env.account.ID, InstanceIDs: []string{"i-1"},
		Action: PowerActionStart, WaitSeconds: 30, WaitReachable: true, ReachablePort: 8022,
	})
	if err != nil {
		t.Fatalf("Power: %v", err)
	}
	if len(*calls) == 0 || (*calls)[0][1] != 8022 {
		t.Fatalf("自定义端口应生效, got %v", *calls)
	}
}

func TestPowerWaitReachableTimeout(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	env.fake.instances = []Instance{{InstanceID: "i-1", PublicIPs: []string{"1.2.3.4"}, Status: statusRunning}}
	env.fake.statuses = map[string]string{"i-1": statusRunning}
	stubProbe(env, []bool{false})
	start := time.Now()

	res, err := env.svc.Power(ctx, PowerInput{
		AccountID: env.account.ID, InstanceIDs: []string{"i-1"},
		Action: PowerActionStart, WaitSeconds: 1, WaitReachable: true,
	})
	if err != nil {
		t.Fatalf("Power: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("超时收尾过慢: %v", elapsed)
	}
	if res.OK != 0 || res.Failed != 1 {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.Items[0].Err, "不可达") || !strings.Contains(res.Items[0].Err, statusRunning) {
		t.Errorf("err = %q, want 超时不可达 + Running", res.Items[0].Err)
	}
}

func TestPowerWaitReachableNoAddr(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	// 未导入且云上无 IP:无可探测地址,即时判失败(不空等超时)。
	env.fake.instances = []Instance{{InstanceID: "i-1", Status: statusRunning}}
	env.fake.statuses = map[string]string{"i-1": statusRunning}
	calls := stubProbe(env, nil)

	res, err := env.svc.Power(ctx, PowerInput{
		AccountID: env.account.ID, InstanceIDs: []string{"i-1"},
		Action: PowerActionStart, WaitSeconds: 1, WaitReachable: true,
	})
	if err != nil {
		t.Fatalf("Power: %v", err)
	}
	if res.OK != 0 || res.Failed != 1 {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.Items[0].Err, "无可探测地址") {
		t.Errorf("err = %q", res.Items[0].Err)
	}
	if len(*calls) != 0 {
		t.Errorf("无地址不应探测: %v", *calls)
	}
}

func TestPowerWaitReachableValidation(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()

	cases := []struct {
		name string
		in   PowerInput
	}{
		{"stop 不可等可达", PowerInput{AccountID: env.account.ID, InstanceIDs: []string{"i-1"}, Action: PowerActionStop, WaitSeconds: 30, WaitReachable: true}},
		{"等可达须预算", PowerInput{AccountID: env.account.ID, InstanceIDs: []string{"i-1"}, Action: PowerActionStart, WaitReachable: true}},
		{"端口超上限", PowerInput{AccountID: env.account.ID, InstanceIDs: []string{"i-1"}, Action: PowerActionStart, WaitSeconds: 30, WaitReachable: true, ReachablePort: 65536}},
	}
	for _, tc := range cases {
		if _, err := env.svc.Power(ctx, tc.in); !errors.Is(err, ErrPowerFailed) {
			t.Errorf("%s: err = %v, want ErrPowerFailed", tc.name, err)
		}
	}
}

// ─── PowerServer / LinkedPowerStatus(服务器状态页电源入口) ────────────────────

func TestPowerServerStartSkipAndInvalid(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	srv := mustServer(t, env, "云主机", "10.0.0.1", "")
	mustLink(t, env, env.account.ID, "i-1", srv.ID)

	// 未关联的机器 → ErrServerNotLinked。
	other := mustServer(t, env, "物理机", "192.168.1.1", "")
	if _, err := env.svc.PowerServer(ctx, other.ID, PowerActionStart); !errors.Is(err, ErrServerNotLinked) {
		t.Fatalf("未关联 err = %v, want ErrServerNotLinked", err)
	}
	// 动作非法 → ErrPowerFailed。
	if _, err := env.svc.PowerServer(ctx, srv.ID, "reboot"); !errors.Is(err, ErrPowerFailed) {
		t.Fatalf("非法动作 err = %v, want ErrPowerFailed", err)
	}

	// Stopped → 发起开机;再查已 Running → 幂等跳过。
	env.fake.statuses = map[string]string{"i-1": statusStopped}
	res, err := env.svc.PowerServer(ctx, srv.ID, PowerActionStart)
	if err != nil {
		t.Fatalf("PowerServer: %v", err)
	}
	if res.Total != 1 || res.OK != 1 || res.Failed != 0 {
		t.Fatalf("res = %+v", res)
	}
	if len(env.fake.started) != 1 || env.fake.started[0] != "i-1" {
		t.Errorf("started = %v", env.fake.started)
	}
	if res.Items[0].ServerID != srv.ID {
		t.Errorf("item 应带 ServerID: %+v", res.Items[0])
	}

	env.fake.statuses = map[string]string{"i-1": statusRunning}
	res2, err := env.svc.PowerServer(ctx, srv.ID, PowerActionStop)
	if err != nil {
		t.Fatalf("PowerServer stop: %v", err)
	}
	if len(env.fake.stopped) != 1 || env.fake.stopped[0] != "i-1" {
		t.Errorf("stopped = %v", env.fake.stopped)
	}
	_ = res2
}

func TestPowerServerPerAccountFaultTolerance(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	srv := mustServer(t, env, "云主机", "10.0.0.1", "")
	mustLink(t, env, env.account.ID, "i-1", srv.ID)
	// 阿里云拒绝发起 → 逐台失败,不返回 error(200 + ok:false 语义)。
	env.fake.statuses = map[string]string{"i-1": statusRunning}
	env.fake.powerErr = map[string]error{"i-1": errors.New("aliyunecs: ecs power operation failed:操作被拒绝")}

	res, err := env.svc.PowerServer(ctx, srv.ID, PowerActionStop)
	if err != nil {
		t.Fatalf("逐台失败不应整批 500: %v", err)
	}
	if res.OK != 0 || res.Failed != 1 || res.Items[0].Err == "" {
		t.Fatalf("res = %+v, want 单台失败带 Err", res)
	}
}

func TestLinkedPowerStatusAggregate(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()

	// 双账户(不同 AK),按 AK 分发到不同 fake:正常账户 + 失败账户。
	fakeOK := &fakeEcs{statuses: map[string]string{"i-ok": statusRunning}}
	fakeBad := &fakeEcs{statusErr: errors.New("aliyunecs: list instances failed:凭据无效")}
	env.svc.(*service).newClient = func(ak, _, _ string) ecsAPI {
		if ak == "LTAI5tBadFake" {
			return fakeBad
		}
		return fakeOK
	}
	bad, err := env.svc.(*service).CreateAccount(ctx, AccountCreateInput{
		Name: "坏账户", AccessKeyID: "LTAI5tBadFake", Secret: "fake-secret-bad", Region: "cn-beijing",
	})
	if err != nil {
		t.Fatalf("CreateAccount bad: %v", err)
	}

	// 两台机器分属两账户;第三台机器不关联(不应出现在结果里)。
	s1 := mustServer(t, env, "正常机", "10.0.0.1", "")
	s2 := mustServer(t, env, "坏账户外", "10.0.0.2", "")
	mustServer(t, env, "非云机", "192.168.1.1", "")
	mustLink(t, env, env.account.ID, "i-ok", s1.ID)
	mustLink(t, env, bad.ID, "i-bad", s2.ID)

	items, err := env.svc.LinkedPowerStatus(ctx)
	if err != nil {
		t.Fatalf("LinkedPowerStatus: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %+v, want 2 条", items)
	}
	byServer := map[string]LinkedPowerItem{}
	for _, it := range items {
		byServer[it.ServerID] = it
	}
	okItem := byServer[s1.ID]
	if okItem.InstanceID != "i-ok" || okItem.Status != statusRunning || okItem.Err != "" {
		t.Errorf("正常条目 = %+v", okItem)
	}
	badItem := byServer[s2.ID]
	if badItem.InstanceID != "i-bad" || badItem.Status != "" || badItem.Err == "" {
		t.Errorf("失败账户条目应带 Err 且状态未知: %+v", badItem)
	}
}
