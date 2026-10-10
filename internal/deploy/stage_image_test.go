package deploy

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/target"
)

// stage_image_test.go 覆盖「流水线部署节点」(DeployForStage)对 **镜像产物** 的部署编排:
//   - 仅镜像产物时,DeployForStage 选中镜像并 docker pull → rm → run(不再 ErrArtifactNotFound)。
//   - 镜像 + 文件产物并存时的优先级(默认文件优先;cfg["artifactType"]=image 选镜像)。
//   - cfg 驱动的容器名 / 端口 / runArgs 原样进 docker run(array 化,不拼 shell)。
//   - 蓝绿策略下镜像走 stage(pull)→ cutover(run);健康失败回滚上一镜像。
//   - 文件产物路径不受影响(回归保护)。
// 全部用 stubTarget / touchRecorder 捕获命令断言,不触真实 SSH / docker。

// addArtifact 给已有 run 追加一件产物(用于「镜像+文件并存」场景)。
func addArtifact(t *testing.T, rsvc run.Service, runID, artType, name, ref string) {
	t.Helper()
	if _, err := rsvc.AddArtifact(context.Background(), run.Artifact{
		RunID: runID, Type: artType, Name: name, Reference: ref,
	}); err != nil {
		t.Fatalf("AddArtifact(%s): %v", artType, err)
	}
}

// hasCmd 报告命令序列里是否出现「以 prefix 开头」的命令。
func hasCmd(calls [][]string, prefix ...string) bool {
	for _, c := range calls {
		if len(c) < len(prefix) {
			continue
		}
		ok := true
		for i := range prefix {
			if c[i] != prefix[i] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// runCmd 取序列里首个 `docker run` 命令(无则 nil)。
func runCmd(calls [][]string) []string {
	for _, c := range calls {
		if len(c) >= 2 && c[0] == "docker" && c[1] == "run" {
			return c
		}
	}
	return nil
}

// TestStageDeploysImageArtifact 仅镜像产物 → DeployForStage 部署镜像(此前被跳过 → ErrArtifactNotFound)。
func TestStageDeploysImageArtifact(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{}
	srv := seedServer(t, tgt, "web-1")
	runID, _ := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactImage, "registry/shop:1.0")

	svc := New(tgt, rsvc)
	res, err := svc.DeployForStage(context.Background(), runID, "server:"+srv.ID, nil, "")
	if err != nil {
		t.Fatalf("DeployForStage: %v", err)
	}
	if len(res) != 1 || res[0].Status != run.TargetSuccess {
		t.Fatalf("want 1 success, got %+v", res)
	}
	if !hasCmd(tgt.calls, "docker", "pull", "registry/shop:1.0") {
		t.Fatalf("应有 docker pull 镜像: %v", tgt.calls)
	}
	if !hasCmd(tgt.calls, "docker", "run") {
		t.Fatalf("应有 docker run 起容器: %v", tgt.calls)
	}
	// 不应走文件发布(无 mkdir releases / ln -sfn)。
	if hasCmd(tgt.calls, "ln", "-sfn") {
		t.Fatalf("镜像部署不应出现软链切换: %v", tgt.calls)
	}
}

// TestStageImageEchoesDockerLogin 配了 registryCredentialId → 目标机 docker login 命令先回显
// (「用什么登录」可见;用户名/口令由 target 层经 stdin 注入,绝不回显)。
func TestStageImageEchoesDockerLogin(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{}
	srv := seedServer(t, tgt, "web-1")
	runID, _ := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactImage, "registry.example.com/shop:1.0")

	var mu sync.Mutex
	var lines []string
	ctx := WithCmdLog(context.Background(), func(_, _, text string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, text)
	})
	svc := New(tgt, rsvc)
	res, err := svc.DeployForStage(ctx, runID, "server:"+srv.ID, map[string]string{
		"registryCredentialId": "cred-1", "registryUrl": "registry.example.com",
	}, "")
	if err != nil {
		t.Fatalf("DeployForStage: %v", err)
	}
	if len(res) != 1 || res[0].Status != run.TargetSuccess {
		t.Fatalf("want success, got %+v", res)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "$ docker login registry.example.com --password-stdin") {
		t.Fatalf("应回显 docker login 命令: %q", joined)
	}
}

// TestStagePrefersFileArtifactByDefault 镜像+文件并存,默认(cfg 空)优先文件发布(保持既有行为)。
func TestStagePrefersFileArtifactByDefault(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{}
	srv := seedServer(t, tgt, "web-1")
	runID, _ := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactImage, "registry/shop:1.0")
	addArtifact(t, rsvc, runID, run.ArtifactDist, "web", "dist/shop.tar.gz")

	svc := New(tgt, rsvc)
	res, err := svc.DeployForStage(context.Background(), runID, "server:"+srv.ID, nil, "")
	if err != nil {
		t.Fatalf("DeployForStage: %v", err)
	}
	if len(res) != 1 || res[0].Status != run.TargetSuccess {
		t.Fatalf("want 1 success, got %+v", res)
	}
	// 默认应走文件发布(dist → ln -sfn current),不应 docker pull。
	if !hasCmd(tgt.calls, "ln", "-sfn") {
		t.Fatalf("默认应走文件发布(软链切换): %v", tgt.calls)
	}
	if hasCmd(tgt.calls, "docker", "pull") {
		t.Fatalf("默认不应部署镜像: %v", tgt.calls)
	}
}

// TestStageSelectsImageWhenConfigured 镜像+文件并存,cfg["artifactType"]=image → 选镜像。
func TestStageSelectsImageWhenConfigured(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{}
	srv := seedServer(t, tgt, "web-1")
	runID, _ := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop.tar.gz")
	addArtifact(t, rsvc, runID, run.ArtifactImage, "shop", "registry/shop:2.0")

	svc := New(tgt, rsvc)
	res, err := svc.DeployForStage(context.Background(), runID, "server:"+srv.ID,
		map[string]string{"artifactType": "image"}, "")
	if err != nil {
		t.Fatalf("DeployForStage: %v", err)
	}
	if len(res) != 1 || res[0].Status != run.TargetSuccess {
		t.Fatalf("want 1 success, got %+v", res)
	}
	if !hasCmd(tgt.calls, "docker", "pull", "registry/shop:2.0") {
		t.Fatalf("cfg image 偏好应部署镜像: %v", tgt.calls)
	}
	if hasCmd(tgt.calls, "ln", "-sfn") {
		t.Fatalf("选镜像时不应走文件发布: %v", tgt.calls)
	}
}

// TestStageImageHonorsContainerNamePortsRunArgs 验证 cfg 的容器名 / 端口 / runArgs 原样进 docker run。
func TestStageImageHonorsContainerNamePortsRunArgs(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{}
	srv := seedServer(t, tgt, "web-1")
	runID, _ := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactImage, "registry/shop:1.0")

	svc := New(tgt, rsvc)
	cfg := map[string]string{
		"containerName": "shopapp",
		"ports":         "8080:80, 9000:9000",
		"runArgs":       "-e KEY=value --restart always",
	}
	res, err := svc.DeployForStage(context.Background(), runID, "server:"+srv.ID, cfg, "")
	if err != nil {
		t.Fatalf("DeployForStage: %v", err)
	}
	if res[0].Status != run.TargetSuccess {
		t.Fatalf("want success, got %+v", res)
	}
	rc := runCmd(tgt.calls)
	if rc == nil {
		t.Fatalf("无 docker run 命令: %v", tgt.calls)
	}
	// 期望:docker run -d --name shopapp -p 8080:80 -p 9000:9000 -e KEY=value --restart always registry/shop:1.0
	want := []string{
		"docker", "run", "-d", "--name", "shopapp",
		"-p", "8080:80", "-p", "9000:9000",
		"-e", "KEY=value", "--restart", "always",
		"registry/shop:1.0",
	}
	if len(rc) != len(want) {
		t.Fatalf("docker run 参数不符\n got: %v\nwant: %v", rc, want)
	}
	for i := range want {
		if rc[i] != want[i] {
			t.Fatalf("docker run[%d]=%q, want %q\n got: %v", i, rc[i], want[i], rc)
		}
	}
	// rm 旧容器也应用 cfg 容器名(幂等替换)。
	if !hasCmd(tgt.calls, "docker", "rm", "-f", "shopapp") {
		t.Fatalf("应按 cfg 容器名移除旧容器: %v", tgt.calls)
	}
}

// TestStageImageStructuredContainerParams 验证结构化容器参数键(cpuLimit/memoryLimit/
// restartPolicy/envVars)翻译为对应 docker run flag 且排在 runArgs 自由参数之前
//(同名 flag 后写生效,runArgs 作逃生舱;envVars 整行为单值,含空格不被拆)。
func TestStageImageStructuredContainerParams(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{}
	srv := seedServer(t, tgt, "web-1")
	runID, _ := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactImage, "registry/shop:1.0")

	svc := New(tgt, rsvc)
	cfg := map[string]string{
		"containerName": "shopapp",
		"cpuLimit":      "1.5",
		"memoryLimit":   "512m",
		"restartPolicy": "unless-stopped",
		"envVars":       "KEY=value\nGREETING=hello world\n\n",
		"runArgs":       "--log-opt max-size=10m",
	}
	res, err := svc.DeployForStage(context.Background(), runID, "server:"+srv.ID, cfg, "")
	if err != nil {
		t.Fatalf("DeployForStage: %v", err)
	}
	if res[0].Status != run.TargetSuccess {
		t.Fatalf("want success, got %+v", res)
	}
	rc := runCmd(tgt.calls)
	if rc == nil {
		t.Fatalf("无 docker run 命令: %v", tgt.calls)
	}
	want := []string{
		"docker", "run", "-d", "--name", "shopapp",
		"--cpus", "1.5", "--memory", "512m", "--restart", "unless-stopped",
		"-e", "KEY=value", "-e", "GREETING=hello world",
		"--log-opt", "max-size=10m",
		"registry/shop:1.0",
	}
	if len(rc) != len(want) {
		t.Fatalf("docker run 参数不符\n got: %v\nwant: %v", rc, want)
	}
	for i := range want {
		if rc[i] != want[i] {
			t.Fatalf("docker run[%d]=%q, want %q\n got: %v", i, rc[i], want[i], rc)
		}
	}
}

// TestImageBaseRunArgs 单测 imageBaseRunArgs:空配置零参数、仅 runArgs 向后兼容、
// 结构化键单独/组合、空行与空白裁剪。
func TestImageBaseRunArgs(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]string
		want []string
	}{
		{"nil 配置", nil, nil},
		{"空配置", map[string]string{}, nil},
		{"仅 runArgs 兼容", map[string]string{"runArgs": "-e A=1 --rm"}, []string{"-e", "A=1", "--rm"}},
		{"仅结构化键", map[string]string{"cpuLimit": "0.5", "memoryLimit": "1g"}, []string{"--cpus", "0.5", "--memory", "1g"}},
		{"空白值忽略", map[string]string{"cpuLimit": "  ", "restartPolicy": ""}, nil},
		{"envVars 裁空行", map[string]string{"envVars": "\n A=1 \n\nB=2\n"}, []string{"-e", "A=1", "-e", "B=2"}},
		{
			"组合:结构化在前 runArgs 在后",
			map[string]string{"cpuLimit": "1", "restartPolicy": "always", "runArgs": "--cpus 2"},
			[]string{"--cpus", "1", "--restart", "always", "--cpus", "2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := imageBaseRunArgs(tc.cfg)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("got[%d]=%q, want %q(全量 got=%v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}



// hasRunWithRef 报告命令序列里是否有「docker run … <ref>」(ref 为末参,用于断言回滚到上一镜像)。
func hasRunWithRef(calls [][]string, ref string) bool {
	for _, c := range calls {
		if len(c) >= 3 && c[0] == "docker" && c[1] == "run" && c[len(c)-1] == ref {
			return true
		}
	}
	return false
}

// TestRollingImageHealthFailureRollsBack 滚动(默认)image 部署:切后健康失败 → 回滚到上一镜像
// (rolled_back)。此前 buildImageDeploy 扁平路径无回滚,健康失败会把目标机留在坏容器上。
func TestRollingImageHealthFailureRollsBack(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	rec := &touchRecorder{
		inspectImage: "registry/app:v1", // 已有上一镜像 → 可回滚
		failOn: func(_ string, cmd []string) bool {
			return len(cmd) > 0 && cmd[0] == "true" // 健康命令失败
		},
	}
	tgt := &stubTarget{execFn: rec.exec}
	srv := seedServer(t, tgt, "web-1")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactImage, "registry/app:v2")

	svc := New(tgt, rsvc)
	hc := &HealthCheck{Type: HealthCheckCommand, Command: []string{"true"}, Retries: 1}
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srv.ID},
		Strategy: "rolling", HealthCheck: hc,
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res) != 1 || res[0].Status != run.TargetRolledBack {
		t.Fatalf("滚动 image 健康失败应 rolled_back,实际 %+v", res)
	}
	if !hasRunWithRef(rec.calls2, "registry/app:v1") {
		t.Fatalf("应回滚到上一镜像 registry/app:v1: %v", rec.calls2)
	}
	// 回滚删容器前必须回捞失败容器日志(否则失败原因随容器一起消失)。
	var runName string
	for _, c := range rec.calls2 {
		if len(c) >= 5 && c[0] == "docker" && c[1] == "run" {
			runName = c[4] // dockerRunCmd 序:[docker run -d --name <name> …]
			break
		}
	}
	logsIdx, rbIdx := -1, -1
	for i, c := range rec.calls2 {
		if logsIdx < 0 && len(c) >= 5 && c[0] == "docker" && c[1] == "logs" && c[4] == runName {
			logsIdx = i
		}
		if rbIdx < 0 && len(c) >= 3 && c[0] == "docker" && c[1] == "run" && c[len(c)-1] == "registry/app:v1" {
			rbIdx = i
		}
	}
	if logsIdx < 0 || rbIdx < 0 || logsIdx > rbIdx {
		t.Fatalf("回滚前应回捞失败容器日志:logsIdx=%d rbIdx=%d: %v", logsIdx, rbIdx, rec.calls2)
	}
}

// TestRollingImageFirstDeployHealthFailureFails 滚动 image 首次部署(无上一镜像)健康失败 → failed
// (无可回滚目标,不谎报回滚)。
func TestRollingImageFirstDeployHealthFailureFails(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	rec := &touchRecorder{
		// inspectImage 空 → 无上一镜像(首次部署)。
		failOn: func(_ string, cmd []string) bool {
			return len(cmd) > 0 && cmd[0] == "true"
		},
	}
	tgt := &stubTarget{execFn: rec.exec}
	srv := seedServer(t, tgt, "web-1")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactImage, "registry/app:v2")

	svc := New(tgt, rsvc)
	hc := &HealthCheck{Type: HealthCheckCommand, Command: []string{"true"}, Retries: 1}
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srv.ID},
		Strategy: "rolling", HealthCheck: hc,
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res) != 1 || res[0].Status != run.TargetFailed {
		t.Fatalf("首次部署健康失败应 failed,实际 %+v", res)
	}
}

// TestRollingImageSwapFailureRollsBack 滚动 image 部署:起新容器(docker run 新镜像)失败 →
// 回滚到上一镜像(避免目标机被 rm 旧容器后留在无容器状态)。
func TestRollingImageSwapFailureRollsBack(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	rec := &touchRecorder{
		inspectImage: "registry/app:v1",
		failOn: func(_ string, cmd []string) bool {
			// 仅让「起新容器」(docker run … v2)失败;回滚的 docker run v1 放行。
			return len(cmd) >= 3 && cmd[0] == "docker" && cmd[1] == "run" && cmd[len(cmd)-1] == "registry/app:v2"
		},
	}
	tgt := &stubTarget{execFn: rec.exec}
	srv := seedServer(t, tgt, "web-1")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactImage, "registry/app:v2")

	svc := New(tgt, rsvc)
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srv.ID}, Strategy: "rolling",
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res) != 1 || res[0].Status != run.TargetRolledBack {
		t.Fatalf("起新容器失败应回滚 rolled_back,实际 %+v", res)
	}
	if !hasRunWithRef(rec.calls2, "registry/app:v1") {
		t.Fatalf("应回滚到上一镜像 v1: %v", rec.calls2)
	}
}

// TestRollingImageRollbackTargetsDigest 可变 tag 假回滚修复:prevImage 必须是镜像 digest
// (inspect {{.Image}}),回滚 docker run 用 digest —— 用引用串({{.Config.Image}})的话,
// app:latest 在本次 pull 后已指向刚拉的坏镜像,回滚等于把坏镜像再跑一遍。
func TestRollingImageRollbackTargetsDigest(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	const digest = "sha256:01d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d"
	var mu sync.Mutex
	var inspectFormats []string
	tgt := &stubTarget{execFn: func(_ string, cmd []string) (*target.ExecResult, error) {
		switch {
		case len(cmd) >= 4 && cmd[0] == "docker" && cmd[1] == "inspect":
			mu.Lock()
			inspectFormats = append(inspectFormats, cmd[3])
			mu.Unlock()
			return &target.ExecResult{ExitCode: 0, Stdout: digest + "\n"}, nil
		case len(cmd) > 0 && cmd[0] == "true": // 健康命令必失败 → 触发回滚
			return &target.ExecResult{ExitCode: 1, Stderr: "unhealthy"}, nil
		default:
			return &target.ExecResult{ExitCode: 0}, nil
		}
	}}
	srv := seedServer(t, tgt, "web-1")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactImage, "registry/app:latest")

	svc := New(tgt, rsvc)
	hc := &HealthCheck{Type: HealthCheckCommand, Command: []string{"true"}, Retries: 1}
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srv.ID}, HealthCheck: hc,
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res) != 1 || res[0].Status != run.TargetRolledBack {
		t.Fatalf("健康失败有上一镜像应 rolled_back,实际 %+v", res)
	}
	// 回滚的 docker run 必须用 digest,不是可变 tag。
	if !hasRunWithRef(tgt.calls, digest) {
		t.Fatalf("回滚应用 digest 起旧镜像: %v", tgt.calls)
	}
	// 回滚目标探测必须读 {{.Image}}(digest),不得再读 {{.Config.Image}}(引用串)。
	mu.Lock()
	defer mu.Unlock()
	if len(inspectFormats) == 0 {
		t.Fatalf("应有回滚目标 inspect 探测")
	}
	for _, f := range inspectFormats {
		if f == "{{.Config.Image}}" {
			t.Fatalf("回滚目标不得再读引用串 {{.Config.Image}}(可变 tag 假回滚): %v", inspectFormats)
		}
	}
	if inspectFormats[0] != "{{.Image}}" {
		t.Fatalf("回滚目标应读 digest {{.Image}},实际 %q", inspectFormats[0])
	}
}

// TestImageHealthExhaustRollbackUsesFreshCtx 健康门控耗尽后回滚命令仍被执行,且用**独立预算**
// 的新 ctx(不复用切换阶段的 60s execCtx —— 健康耗尽后它已过期,复用会让第一条 docker rm
// 就 DeadlineExceeded,坏容器还在跑状态却 rolled_back)。
func TestImageHealthExhaustRollbackUsesFreshCtx(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	const digest = "sha256:prev0ld"
	rec := &touchRecorder{
		inspectImage: digest,
		failOn: func(_ string, cmd []string) bool {
			return len(cmd) > 0 && cmd[0] == "true" // 健康命令必失败 → 门控耗尽
		},
	}
	tgt := &ctxCaptureTarget{stubTarget: &stubTarget{execFn: rec.exec}}
	srv := seedServer(t, tgt.stubTarget, "web-1")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactImage, "registry/app:v2")

	svc := New(tgt, rsvc)
	hc := &HealthCheck{Type: HealthCheckCommand, Command: []string{"true"}, Retries: 1, IntervalSeconds: 0}
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srv.ID}, HealthCheck: hc,
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res) != 1 || res[0].Status != run.TargetRolledBack {
		t.Fatalf("健康耗尽有上一镜像应 rolled_back,实际 %+v", res)
	}
	// 回滚命令仍被执行:docker rm -f 出现两次(切换一次 + 回滚一次),且回滚 run digest。
	deployCtx := tgt.ctxAt("docker rm -f shop", 0)
	rollbackCtx := tgt.ctxAt("docker rm -f shop", 1)
	if deployCtx == nil || rollbackCtx == nil {
		t.Fatalf("健康耗尽后回滚命令未执行(rm 应出现两次)")
	}
	if !hasRunWithRef(rec.calls2, digest) {
		t.Fatalf("回滚应用 digest 起旧镜像: %v", rec.calls2)
	}
	// 回滚用新 ctx:与切换阶段 execCtx 不是同一个,且有独立 execTimeout 级预算。
	if deployCtx == rollbackCtx {
		t.Fatalf("回滚不应复用切换阶段的 execCtx(健康耗尽后它已过期)")
	}
	dl, ok := rollbackCtx.Deadline()
	if !ok || time.Until(dl) < 30*time.Second {
		t.Fatalf("回滚应有独立 execTimeout 级预算,deadline=%v ok=%v", dl, ok)
	}
}

// TestStageImagePullUsesLongTimeout docker pull 走独立长超时(pullTimeout,对齐 uploadTimeout
// 的 15min 约定),不占 60s 命令预算(大镜像慢链路 60s 拉不完)。
func TestStageImagePullUsesLongTimeout(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	rec := &touchRecorder{}
	tgt := &ctxCaptureTarget{stubTarget: &stubTarget{execFn: rec.exec}}
	srv := seedServer(t, tgt.stubTarget, "web-1")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactImage, "registry/shop:1.0")

	svc := New(tgt, rsvc)
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srv.ID},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res) != 1 || res[0].Status != run.TargetSuccess {
		t.Fatalf("want success, got %+v", res)
	}
	pullCtx := tgt.ctxAt("docker pull registry/shop:1.0", 0)
	if pullCtx == nil {
		t.Fatalf("应有 docker pull 命令: %v", rec.calls2)
	}
	dl, ok := pullCtx.Deadline()
	if !ok || time.Until(dl) < 10*time.Minute {
		t.Fatalf("docker pull 应走独立长超时(pullTimeout=15min),deadline=%v ok=%v", dl, ok)
	}
}

// TestRollingImageRollbackCmdFailMarkedFailed 回滚命令失败 → status=failed(回滚未确认):
// rolled_back 语义是「仍运行旧版本」;rm 成功 run 失败时机器上没有容器在跑,记 rolled_back
// 是撒谎。failed 仍在 retryableTargetStatus 集合内,修复后可重试。
func TestRollingImageRollbackCmdFailMarkedFailed(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	const digest = "sha256:prev0ld"
	rec := &touchRecorder{
		inspectImage: digest,
		failOn: func(_ string, cmd []string) bool {
			if len(cmd) > 0 && cmd[0] == "true" {
				return true // 健康命令失败 → 触发回滚
			}
			// 回滚的 docker run <digest> 也失败(rm 成功、run 失败 → 机器上无容器在跑)。
			return len(cmd) >= 3 && cmd[0] == "docker" && cmd[1] == "run" && cmd[len(cmd)-1] == digest
		},
	}
	tgt := &stubTarget{execFn: rec.exec}
	srv := seedServer(t, tgt, "web-1")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactImage, "registry/app:v2")

	svc := New(tgt, rsvc)
	hc := &HealthCheck{Type: HealthCheckCommand, Command: []string{"true"}, Retries: 1}
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srv.ID}, HealthCheck: hc,
	})
	if err != nil {
		t.Fatalf("Deploy 不应上抛(回滚失败也内化): %v", err)
	}
	if len(res) != 1 || res[0].Status != run.TargetFailed {
		t.Fatalf("回滚命令失败应记 failed(回滚未确认),实际 %+v", res)
	}
	if !strings.Contains(res[0].Message, "回滚命令失败") || !strings.Contains(res[0].Message, "回滚未确认") {
		t.Fatalf("message 应说明回滚未确认: %q", res[0].Message)
	}
	if !retryableTargetStatus(res[0].Status) {
		t.Fatalf("回滚失败记 failed 后仍应可重试,实际 %s", res[0].Status)
	}
}

// mustArtID 取该 run 首件产物 id(测试便捷)。
func mustArtID(t *testing.T, rsvc run.Service, runID string) string {
	t.Helper()
	arts, err := rsvc.ListArtifacts(context.Background(), runID)
	if err != nil || len(arts) == 0 {
		t.Fatalf("ListArtifacts: %v / %d", err, len(arts))
	}
	return arts[0].ID
}

// TestPickStageArtifact 单测产物选取优先级矩阵。
func TestPickStageArtifact(t *testing.T) {
	img := run.Artifact{Type: run.ArtifactImage, Reference: "img:1"}
	dist := run.Artifact{Type: run.ArtifactDist, Reference: "dist"}
	jar := run.Artifact{Type: run.ArtifactJar, Reference: "app.jar"}

	cases := []struct {
		name   string
		arts   []run.Artifact
		prefer string
		want   string // 期望选中的 Type;"" = nil
	}{
		{"empty", nil, "", ""},
		{"image only / no prefer", []run.Artifact{img}, "", run.ArtifactImage},
		{"file only / no prefer", []run.Artifact{dist}, "", run.ArtifactDist},
		{"both / no prefer → file first", []run.Artifact{img, dist}, "", run.ArtifactDist},
		{"both / prefer image", []run.Artifact{img, dist}, "image", run.ArtifactImage},
		{"both / prefer image (order swapped)", []run.Artifact{dist, img}, "image", run.ArtifactImage},
		{"prefer image but none → fall back file", []run.Artifact{dist}, "image", run.ArtifactDist},
		{"prefer jar exact", []run.Artifact{dist, jar}, "jar", run.ArtifactJar},
		{"prefer dist but only jar → any release", []run.Artifact{jar}, "dist", run.ArtifactJar},
		{"unknown type skipped", []run.Artifact{{Type: "weird"}}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pickStageArtifact(tc.arts, tc.prefer)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("want nil, got %+v", got)
				}
				return
			}
			if got == nil || got.Type != tc.want {
				t.Fatalf("want %s, got %+v", tc.want, got)
			}
		})
	}
}
