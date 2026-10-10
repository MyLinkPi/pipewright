package deploy

// rolling_binding_test.go 覆盖统一滚动编排与产物精确绑定的新语义:
//   - 产物精确绑定:artifactJob 命中 / 多件歧义报错 / 零件报错 / glob 消歧;
//   - 健康门控从节点 config 构造(healthUrl / healthCommand);
//   - 滚动批次:firstBatchSize / batchSize、任一批失败停止铺开(未轮到机 pending)、
//     预检故障机优先排序、重试可继续推进 pending 机;
//   - 标签选择器 selectorMode=all/any;
//   - health_check 节点探测(CheckHealth:目标机通过 / 失败 / http 本机兜底且末次不空等)。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/target"
)

// ---- 产物精确绑定 ------------------------------------------------------------

func TestPickStageArtifactExplicitHit(t *testing.T) {
	arts := []run.Artifact{
		{ID: "a1", Name: "shop-dist", Type: run.ArtifactDist, Metadata: map[string]any{"sourceJob": "前端构建"}},
		{ID: "a2", Name: "shop-jar", Type: run.ArtifactJar, Metadata: map[string]any{"sourceJob": "后端构建"}},
	}
	a, err := pickStageArtifactExplicit(arts, "后端构建", "")
	if err != nil {
		t.Fatalf("explicit pick: %v", err)
	}
	if a.ID != "a2" {
		t.Fatalf("应精确命中后端构建的 jar, got %s", a.ID)
	}
}

func TestPickStageArtifactExplicitAmbiguous(t *testing.T) {
	arts := []run.Artifact{
		{ID: "a1", Name: "web-dist", Type: run.ArtifactDist, Metadata: map[string]any{"sourceJob": "构建"}},
		{ID: "a2", Name: "admin-dist", Type: run.ArtifactDist, Metadata: map[string]any{"sourceJob": "构建"}},
	}
	if _, err := pickStageArtifactExplicit(arts, "构建", ""); err == nil || !strings.Contains(err.Error(), "artifactName") {
		t.Fatalf("多件且未消歧应报歧义(提示 artifactName), got %v", err)
	}
	// glob 消歧命中。
	a, err := pickStageArtifactExplicit(arts, "构建", "admin-*")
	if err != nil || a.ID != "a2" {
		t.Fatalf("glob 消歧应命中 a2, got %v / %v", a, err)
	}
	// glob 无命中 → 明确报错(绝不静默换产物)。
	if _, err := pickStageArtifactExplicit(arts, "构建", "nope-*"); err == nil {
		t.Fatal("glob 无命中应报错")
	}
}

func TestPickStageArtifactExplicitEmpty(t *testing.T) {
	arts := []run.Artifact{
		{ID: "a1", Name: "shop-jar", Type: run.ArtifactJar, Metadata: map[string]any{"sourceJob": "后端构建"}},
	}
	if _, err := pickStageArtifactExplicit(arts, "不存在的节点", ""); err == nil || !strings.Contains(err.Error(), "未产出") {
		t.Fatalf("来源节点无产物应明确报错, got %v", err)
	}
}

func TestDeployForStageRejectsOldAutoWhenSourceMissing(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{execFn: func(string, []string) (*target.ExecResult, error) {
		return &target.ExecResult{ExitCode: 0}, nil
	}}
	srv := seedServer(t, tgt, "web-1")
	runID, _ := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop")
	svc := New(tgt, rsvc)

	// 指定了 artifactJob 但无命中 → 明确报错(不再静默兜底挑别的产物)。
	_, err := svc.DeployForStage(context.Background(), runID, "server:"+srv.ID,
		map[string]string{"artifactJob": "ghost"}, "")
	if err == nil || !strings.Contains(err.Error(), "未产出") {
		t.Fatalf("指定来源无产物应报错, got %v", err)
	}
}

// ---- 健康门控构造 --------------------------------------------------------------

func TestHealthCheckFromCfg(t *testing.T) {
	if hc := healthCheckFromCfg(map[string]string{}, "app"); hc != nil {
		t.Fatalf("未配置应返回 nil, got %+v", hc)
	}
	hc := healthCheckFromCfg(map[string]string{"healthUrl": "http://x/healthz", "healthRetries": "5"}, "app")
	if hc == nil || hc.Type != HealthCheckHTTP || hc.URL != "http://x/healthz" || hc.Retries != 5 {
		t.Fatalf("http 型构造错误: %+v", hc)
	}
	hc = healthCheckFromCfg(map[string]string{"healthCommand": "curl -fsS localhost:8080/healthz"}, "app")
	if hc == nil || hc.Type != HealthCheckCommand || len(hc.Command) != 3 {
		t.Fatalf("command 型构造错误: %+v", hc)
	}
}

func TestHealthCheckFromCfgExec(t *testing.T) {
	// 容器内命令探测(docker exec):不发布端口的后台容器用它在容器里探活。
	hc := healthCheckFromCfg(map[string]string{"healthExec": "pg_isready -U postgres"}, "my-app")
	if hc == nil || hc.Type != HealthCheckCommand {
		t.Fatalf("exec 型构造错误: %+v", hc)
	}
	want := []string{"docker", "exec", "my-app", "sh", "-c", "pg_isready -U postgres"}
	if len(hc.Command) != len(want) {
		t.Fatalf("exec 命令应 array 化逐元素,得 %v", hc.Command)
	}
	for i := range want {
		if hc.Command[i] != want[i] {
			t.Fatalf("exec 命令[%d] = %q, want %q(完整: %v)", i, hc.Command[i], want[i], hc.Command)
		}
	}
	// 主机命令优先级高于容器内命令(存量语义不让位)。
	hc = healthCheckFromCfg(map[string]string{"healthCommand": "echo host", "healthExec": "echo in"}, "app")
	if hc.Type != HealthCheckCommand || hc.Command[0] != "sh" {
		t.Fatalf("healthCommand 应优先于 healthExec,得 %+v", hc.Command)
	}
	// 容器内命令优先于端口探测。
	hc = healthCheckFromCfg(map[string]string{"healthExec": "true", "healthPort": "8080"}, "app")
	if hc.Type != HealthCheckCommand || hc.Command[0] != "docker" {
		t.Fatalf("healthExec 应优先于 healthPort,得 %+v", hc.Command)
	}
}

// ---- 滚动批次 / 预检排序 --------------------------------------------------------

// rollingRecorder 记录每批执行的 server 顺序与注入失败。
type rollingRecorder struct {
	order []string
	fail  map[string]bool
}

func (r *rollingRecorder) exec(serverID string, cmd []string) (*target.ExecResult, error) {
	if cmd[0] == "readlink" {
		return &target.ExecResult{ExitCode: 0}, nil
	}
	if cmd[0] != "curl" { // 只记部署命令(预检 curl 不算部署顺序)
		for _, c := range r.order {
			if c == serverID {
				goto seen
			}
		}
		r.order = append(r.order, serverID)
	seen:
	}
	if r.fail[serverID] {
		return &target.ExecResult{ExitCode: 1, Stderr: "boom"}, nil
	}
	return &target.ExecResult{ExitCode: 0}, nil
}

func TestDeployRollingBatches(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	rec := &rollingRecorder{}
	tgt := &stubTarget{execFn: rec.exec}
	servers := []*target.Server{
		seedServer(t, tgt, "web-1"), seedServer(t, tgt, "web-2"), seedServer(t, tgt, "web-3"),
	}
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop")
	svc := New(tgt, rsvc)

	// 3 台:首批 1 台,每批 1 台 → 全部 success,顺序 = 输入顺序。
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID,
		ServerIDs: []string{servers[0].ID, servers[1].ID, servers[2].ID},
		Config:    map[string]string{"firstBatchSize": "1", "batchSize": "1"},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	for _, r := range res {
		if r.Status != run.TargetSuccess {
			t.Fatalf("全批成功场景不应有失败: %+v", r)
		}
	}
}

func TestDeployRollingStopsOnBatchFailure(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	rec := &rollingRecorder{fail: map[string]bool{}}
	tgt := &stubTarget{execFn: rec.exec}
	s1 := seedServer(t, tgt, "web-1")
	s2 := seedServer(t, tgt, "web-2")
	s3 := seedServer(t, tgt, "web-3")
	rec.fail[s2.ID] = true // 第 2 批失败
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop")
	svc := New(tgt, rsvc)

	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID,
		ServerIDs: []string{s1.ID, s2.ID, s3.ID},
		Config:    map[string]string{"firstBatchSize": "1", "batchSize": "1"},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	byID := map[string]TargetResult{}
	for _, r := range res {
		byID[r.ServerID] = r
	}
	if byID[s1.ID].Status != run.TargetSuccess {
		t.Fatalf("首批机应 success: %+v", byID[s1.ID])
	}
	if byID[s2.ID].Status != run.TargetFailed {
		t.Fatalf("失败机应 failed: %+v", byID[s2.ID])
	}
	if byID[s3.ID].Status != run.TargetPending {
		t.Fatalf("未轮到机应 pending(停止铺开), got %+v", byID[s3.ID])
	}
}

// TestRetryFailedContinuesPendingTargets:滚动在某批失败后停止铺开 → 未轮到机为 pending;
// 修好失败机后「重试」必须把 failed **与 pending** 一并推进(否则 pending 机永久停在旧版本、
// run 终态也永远回不到 success),且已成功的机器不被重复部署。
func TestRetryFailedContinuesPendingTargets(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	rec := &rollingRecorder{fail: map[string]bool{}}
	tgt := &stubTarget{execFn: rec.exec}
	s1 := seedServer(t, tgt, "web-1")
	s2 := seedServer(t, tgt, "web-2")
	s3 := seedServer(t, tgt, "web-3")
	rec.fail[s2.ID] = true // 第 2 批失败 → s3 停止铺开
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop")
	svc := New(tgt, rsvc)

	if _, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID,
		ServerIDs: []string{s1.ID, s2.ID, s3.ID},
		Config:    map[string]string{"firstBatchSize": "1", "batchSize": "1"},
	}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	targets, err := rsvc.ListDeployTargets(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListDeployTargets: %v", err)
	}
	byID := map[string]string{}
	for _, dt := range targets {
		byID[dt.ServerID] = dt.Status
	}
	if byID[s1.ID] != run.TargetSuccess || byID[s2.ID] != run.TargetFailed || byID[s3.ID] != run.TargetPending {
		t.Fatalf("第一波终态不符: %v", byID)
	}

	// 修好 s2 后重试:全部命令成功。
	tgt.execFn = func(string, []string) (*target.ExecResult, error) {
		return &target.ExecResult{ExitCode: 0}, nil
	}
	out, err := svc.RetryFailed(context.Background(), RetryInput{RunID: runID, ArtifactID: artID})
	if err != nil {
		t.Fatalf("RetryFailed: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("应返回全量 3 台目标, got %d", len(out))
	}
	for _, r := range out {
		if r.Status != run.TargetSuccess {
			t.Fatalf("重试后 %s 应 success(failed 与 pending 都要被推进), got %+v", r.ServerName, r)
		}
	}
	rn, err := rsvc.Get(context.Background(), runID)
	if err != nil {
		t.Fatalf("Get run: %v", err)
	}
	if rn.Status != run.StatusSuccess {
		t.Fatalf("全部推进后 run 应回到 success, got %s", rn.Status)
	}
}

func TestDeployRollingPrecheckOrdersFailingFirst(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	rec := &rollingRecorder{fail: map[string]bool{}}
	tgt := &stubTarget{execFn: rec.exec}
	ok := seedServer(t, tgt, "web-ok")
	bad := seedServer(t, tgt, "web-bad")
	// 预检(curl)对 bad 机失败;部署命令(tar/mkdir)全部成功。
	probeFail := map[string]bool{bad.ID: true}
	execFn := func(serverID string, cmd []string) (*target.ExecResult, error) {
		if len(cmd) > 0 && cmd[0] == "curl" && probeFail[serverID] {
			return &target.ExecResult{ExitCode: 7, Stderr: "conn refused"}, nil
		}
		return rec.exec(serverID, cmd)
	}
	tgt.execFn = execFn
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop")
	svc := New(tgt, rsvc)

	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID,
		ServerIDs:   []string{ok.ID, bad.ID}, // ok 在前:预检后 bad 应排到队首
		Config:      map[string]string{"firstBatchSize": "1", "batchSize": "1"},
		HealthCheck: &HealthCheck{Type: HealthCheckHTTP, URL: "http://x/healthz", Retries: 1, IntervalSeconds: 0},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	// bad 预检失败 → 排队首先部署(只影响首批 1 台);其部署后门控仍失败 → 回滚,
	// 批次失败停止铺开 → ok 保持 pending(仍运行旧版本,未被波及)。
	if len(rec.order) != 1 || rec.order[0] != bad.ID {
		t.Fatalf("预检未通过的机器应排到队首先部署且故障只波及首批, got order=%v", rec.order)
	}
	byID := map[string]TargetResult{}
	for _, r := range res {
		byID[r.ServerID] = r
		if r.ServerID == bad.ID && !strings.Contains(r.Message, "预检") {
			t.Fatalf("故障机结果 message 应记预检原因: %+v", r)
		}
	}
	// 首次部署无上一发布可回滚 → failed(有上一发布时为 rolled_back,release_test 已覆盖)。
	if byID[bad.ID].Status != run.TargetFailed {
		t.Fatalf("故障机应 failed(首次部署无可回滚), got %+v", byID[bad.ID])
	}
	if byID[ok.ID].Status != run.TargetPending {
		t.Fatalf("健康机应 pending(未铺开), got %+v", byID[ok.ID])
	}
}

// ---- 标签选择器 all/any --------------------------------------------------------

func TestResolveTargetsSelectorMode(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{execFn: func(string, []string) (*target.ExecResult, error) {
		return &target.ExecResult{ExitCode: 0}, nil
	}}
	seedLabeledServer(t, tgt, "web-linux", "web,linux")
	seedLabeledServer(t, tgt, "db-linux", "db,linux")
	svc := New(tgt, rsvc).(*service)

	// all:须同时命中 web+linux → 只有 web-linux。
	got, err := svc.resolveTargets(context.Background(), nil, "web,linux", SelectorModeAll)
	if err != nil || len(got) != 1 || got[0].Name != "web-linux" {
		t.Fatalf("all 模式应只命中 web-linux, got %v / %v", got, err)
	}
	// any:任一命中 web 或 linux → 两台都中。
	got, err = svc.resolveTargets(context.Background(), nil, "web,linux", SelectorModeAny)
	if err != nil || len(got) != 2 {
		t.Fatalf("any 模式应命中两台, got %d / %v", len(got), err)
	}
	// 空 mode → all(存量语义)。
	got, _ = svc.resolveTargets(context.Background(), nil, "web,linux", "")
	if len(got) != 1 {
		t.Fatalf("空 selectorMode 应保持 AND 存量语义")
	}
}

// ---- CheckHealth(health_check 节点)-------------------------------------------

func TestCheckHealthOnTargets(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{execFn: func(_ string, cmd []string) (*target.ExecResult, error) {
		if len(cmd) > 0 && cmd[0] == "curl" {
			return &target.ExecResult{ExitCode: 0}, nil
		}
		return &target.ExecResult{ExitCode: 0}, nil
	}}
	good := seedServer(t, tgt, "web-1")
	svc := New(tgt, rsvc)

	res, err := svc.CheckHealth(context.Background(), "server:"+good.ID, "", &HealthCheck{Type: HealthCheckHTTP, URL: "http://x/hz", Retries: 1})
	if err != nil || len(res) != 1 || !res[0].OK {
		t.Fatalf("目标机健康通过: %v / %v", res, err)
	}

	// 探测失败 → 返回结果 + error。
	tgt.execFn = func(_ string, cmd []string) (*target.ExecResult, error) {
		if len(cmd) > 0 && cmd[0] == "curl" {
			return &target.ExecResult{ExitCode: 7, Stderr: "refused"}, nil
		}
		return &target.ExecResult{ExitCode: 0}, nil
	}
	res, err = svc.CheckHealth(context.Background(), "server:"+good.ID, "", &HealthCheck{Type: HealthCheckHTTP, URL: "http://x/hz", Retries: 1})
	if err == nil || res[0].OK {
		t.Fatalf("探测失败应返回 error: %v / %v", res, err)
	}
}

// TestCheckHealthLocalProbeNoTrailingWait:无目标机 + http 型 → 从平台本机探测;
// 末次尝试失败后不得再空等一个间隔(与 runHealthCheck 的 `i < attempts-1` 同语义)。
func TestCheckHealthLocalProbeNoTrailingWait(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	svc := New(&stubTarget{}, rsvc)

	// 127.0.0.1:1 立即拒连:3 次尝试只需 2 个 1s 间隔(末次若仍空等则 >= 3s)。
	hc := &HealthCheck{
		Type: HealthCheckHTTP, URL: "http://127.0.0.1:1/healthz",
		Retries: 3, IntervalSeconds: 1, TimeoutSeconds: 1,
	}
	start := time.Now()
	res, err := svc.CheckHealth(context.Background(), "", "", hc)
	elapsed := time.Since(start)

	if err == nil || len(res) != 1 || res[0].OK {
		t.Fatalf("本机探测失败应返回 error + 未通过结果: %v / %+v", err, res)
	}
	if res[0].ServerName != "platform" {
		t.Fatalf("本机探测应标 platform, got %q", res[0].ServerName)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("末次尝试后不应再空等间隔,elapsed=%s", elapsed)
	}
	if elapsed < 2*time.Second {
		t.Fatalf("前两次尝试后应各等一个间隔,elapsed=%s", elapsed)
	}
}

// TestCheckHealthLocalProbeEchoesCurlCommand 本机 http 探测(不经 SSH)也要在日志里说清
// 「用什么检查的」:回显等效 curl 命令;失败时回显失败原因(如 HTTP 5xx)。
func TestCheckHealthLocalProbeEchoesCurlCommand(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	svc := New(&stubTarget{}, rsvc)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var mu sync.Mutex
	var lines []string
	ctx := WithCmdLog(context.Background(), func(stream, _, text string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, stream+"|"+text)
	})
	hc := &HealthCheck{Type: HealthCheckHTTP, URL: srv.URL + "/healthz", Retries: 1, TimeoutSeconds: 2}
	res, err := svc.CheckHealth(ctx, "", "", hc)
	if err == nil || len(res) != 1 || res[0].OK {
		t.Fatalf("500 应探测失败: %v / %+v", err, res)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "$ curl -fsS --max-time 2 "+srv.URL+"/healthz") {
		t.Fatalf("应回显等效 curl 探测命令: %q", joined)
	}
	if !strings.Contains(joined, "✗ HTTP 500") {
		t.Fatalf("应回显探测失败原因: %q", joined)
	}
}

func TestNormalizeStrategyLegacyValues(t *testing.T) {
	for _, in := range []string{"", "rolling", "canary", "blue_green", "blue-green", "interactive", "instance_rolling", "recreate", "nonsense"} {
		if got := NormalizeStrategy(in); got != StrategyRolling {
			t.Fatalf("NormalizeStrategy(%q) = %q, want rolling(统一滚动)", in, got)
		}
	}
}

func TestHealthCheckFromCfgPortPath(t *testing.T) {
	// 端口 + 路径 → 127.0.0.1 完整 URL(探测在部署目标服务器本机执行)。
	hc := healthCheckFromCfg(map[string]string{"healthPort": "8080", "healthPath": "healthz"}, "app")
	if hc == nil || hc.Type != HealthCheckHTTP || hc.URL != "http://127.0.0.1:8080/healthz" {
		t.Fatalf("port+path 应拼成 127.0.0.1 URL,得 %+v", hc)
	}
	// 只给端口 → 根路径。
	hc = healthCheckFromCfg(map[string]string{"healthPort": "8080"}, "app")
	if hc == nil || hc.URL != "http://127.0.0.1:8080" {
		t.Fatalf("只给端口应得根 URL,得 %+v", hc)
	}
	// 路径已带斜杠 → 不重复加。
	hc = healthCheckFromCfg(map[string]string{"healthPort": "8080", "healthPath": "/a/b"}, "app")
	if hc.URL != "http://127.0.0.1:8080/a/b" {
		t.Fatalf("路径斜杠应保留,得 %s", hc.URL)
	}
	// 存量完整 URL 优先于端口写法。
	hc = healthCheckFromCfg(map[string]string{"healthUrl": "http://localhost:9000/x", "healthPort": "8080"}, "app")
	if hc.URL != "http://localhost:9000/x" {
		t.Fatalf("存量 healthUrl 应优先,得 %s", hc.URL)
	}
	// 全空 → 不做门控。
	if hc := healthCheckFromCfg(map[string]string{"healthPath": "/x"}, "app"); hc != nil {
		t.Fatalf("只给路径不算配置门控,得 %+v", hc)
	}
}
