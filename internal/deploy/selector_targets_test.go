package deploy

// selector_targets_test.go 验证「部署目标圈选」语义(选择器语法复用构建机池):
//   - 显式 serverIDs 优先,存在性从严(任一不存在 → ErrServerNotFound);
//   - 选择器空 / 零命中(含 server:<id> 指向已删机器)→ 无目标,Deploy/DeployForStage 跳过即成功
//     (空结果、不写 deploy_targets、不动 run 终态);
//   - 标签项 AND 圈选多机;serverIDs 非空时优先于 selector;
//   - 语法非法 → ErrInvalidSelector(供 HTTP 层 422)。

import (
	"context"
	"errors"
	"testing"

	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/target"
)

// TestDeploySelectorResolvesTargets 标签圈选:只有打了匹配标签的机器被部署,顺序无关、AND 语义。
func TestDeploySelectorResolvesTargets(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{}
	web1 := seedLabeledServer(t, tgt, "web-1", "web,env=prod")
	seedLabeledServer(t, tgt, "db-1", "db,env=prod")
	seedLabeledServer(t, tgt, "web-staging", "web,env=staging")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop.tar.gz")

	svc := New(tgt, rsvc)
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, Selector: "web,env=prod",
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res) != 1 || res[0].ServerID != web1.ID || res[0].Status != run.TargetSuccess {
		t.Fatalf("want only web-1 success, got %+v", res)
	}
	// 纯 tag ≠ k=v:`env` 不命中 `env=prod`(严格逐项相等)。
	res, err = svc.Deploy(context.Background(), DeployInput{RunID: runID, ArtifactID: artID, Selector: "env"})
	if err != nil || len(res) != 0 {
		t.Fatalf("纯 tag env 不应命中 env=prod 机器: res=%v err=%v", res, err)
	}
}

// TestDeployServerIDsTakePrecedence 显式 serverIDs 优先于 selector(回滚历史回放语义)。
func TestDeployServerIDsTakePrecedence(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{}
	db1 := seedLabeledServer(t, tgt, "db-1", "db")
	seedLabeledServer(t, tgt, "web-1", "web")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop.tar.gz")

	svc := New(tgt, rsvc)
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID,
		ServerIDs: []string{db1.ID}, Selector: "web", // selector 指向 web,但显式 IDs 优先
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res) != 1 || res[0].ServerID != db1.ID {
		t.Fatalf("serverIDs 应优先于 selector, want db-1, got %+v", res)
	}
}

// TestDeployNoTargetsSkips 无目标 → 跳过即成功:空结果、不写 deploy_targets、run 终态不动。
func TestDeployNoTargetsSkips(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{}
	seedLabeledServer(t, tgt, "db-1", "db")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop.tar.gz")

	svc := New(tgt, rsvc)
	for name, selector := range map[string]string{
		"空选择器":     "",
		"零命中":      "gpu",
		"钉单机但机器已删": "server:gone",
	} {
		res, err := svc.Deploy(context.Background(), DeployInput{RunID: runID, ArtifactID: artID, Selector: selector})
		if err != nil || len(res) != 0 {
			t.Fatalf("%s: want empty success, got res=%v err=%v", name, res, err)
		}
	}
	// 不写 deploy_targets、不动终态:run 仍 success 且无 targets 行。
	rn, err := rsvc.Get(context.Background(), runID)
	if err != nil {
		t.Fatalf("Get run: %v", err)
	}
	if rn.Status != run.StatusSuccess {
		t.Fatalf("跳过部署不应改 run 终态, got %s", rn.Status)
	}
	targets, err := rsvc.ListDeployTargets(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListDeployTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("跳过部署不应写 deploy_targets, got %+v", targets)
	}
}

// TestDeployForStagePinnedAndLabel 阶段部署:`server:<id>` 钉单机与标签圈选都走通;零命中 → 空结果 nil。
func TestDeployForStagePinnedAndLabel(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{}
	web1 := seedLabeledServer(t, tgt, "web-1", "web")
	runID, _ := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop.tar.gz")

	svc := New(tgt, rsvc)
	// 钉单机。
	res, err := svc.DeployForStage(context.Background(), runID, "server:"+web1.ID, nil, "")
	if err != nil || len(res) != 1 || res[0].Status != run.TargetSuccess {
		t.Fatalf("pinned DeployForStage: res=%v err=%v", res, err)
	}
	// 标签圈选。
	res, err = svc.DeployForStage(context.Background(), runID, "web", nil, "")
	if err != nil || len(res) != 1 || res[0].ServerID != web1.ID {
		t.Fatalf("label DeployForStage: res=%v err=%v", res, err)
	}
	// 零命中 → 空结果 nil(节点按跳过成功)。
	res, err = svc.DeployForStage(context.Background(), runID, "nonexistent-label", nil, "")
	if err != nil || len(res) != 0 {
		t.Fatalf("zero-match DeployForStage: res=%v err=%v", res, err)
	}
	// 语法非法 → ErrInvalidSelector。
	if _, err := svc.DeployForStage(context.Background(), runID, "a b!!", nil, ""); err != ErrInvalidSelector {
		t.Fatalf("want ErrInvalidSelector, got %v", err)
	}
}

// TestDeployExplicitServerIDsDeduped 显式 serverIDs 重复 → 保序去重:同机只部署一次、
// deploy_targets 只写一行(不去重会同机部署两次,RetryFailed 并行扇出还会同机竞态)。
func TestDeployExplicitServerIDsDeduped(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{}
	s1 := seedServer(t, tgt, "s1")
	s2 := seedServer(t, tgt, "s2")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop.tar.gz")

	svc := New(tgt, rsvc)
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID,
		ServerIDs: []string{s1.ID, s2.ID, s1.ID, s1.ID, s2.ID}, // 重复 ID
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res) != 2 || res[0].ServerID != s1.ID || res[1].ServerID != s2.ID {
		t.Fatalf("重复 ID 应保序去重为 2 台, got %+v", res)
	}
	targets, err := rsvc.ListDeployTargets(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListDeployTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("deploy_targets 应只写 2 行, got %+v", targets)
	}
}

// TestResolveTargetsExplicitMissing 显式 serverIDs 任一不存在 → 整次拒绝(存在性从严,不留半截)。
func TestResolveTargetsExplicitMissing(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{}
	srv := seedServer(t, tgt, "s1")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop.tar.gz")

	svc := New(tgt, rsvc)
	if _, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srv.ID, "nope"},
	}); err != ErrServerNotFound {
		t.Fatalf("want ErrServerNotFound, got %v", err)
	}
}

// ---- 0084:PVE 集群物理机不允许作为部署目标 ------------------------------------------

// TestDeployRejectsPVENodeExplicit 显式 serverIDs(回滚历史回放语义)指向 PVE 物理机 →
// 整次拒绝(ErrPVENotTarget),不部署任何机器、不写 deploy_targets。
func TestDeployRejectsPVENodeExplicit(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{}
	pve := seedLabeledServer(t, tgt, "pve-1", "web")
	pve.Kind = target.KindPVENode
	std := seedLabeledServer(t, tgt, "std-1", "web")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop.tar.gz")

	svc := New(tgt, rsvc)
	for name, in := range map[string]DeployInput{
		"仅 PVE 机":        {RunID: runID, ArtifactID: artID, ServerIDs: []string{pve.ID}},
		"混合(PVE 在前列)": {RunID: runID, ArtifactID: artID, ServerIDs: []string{pve.ID, std.ID}},
	} {
		_, err := svc.Deploy(context.Background(), in)
		if err == nil || !errors.Is(err, target.ErrPVENotTarget) {
			t.Fatalf("%s: Deploy err = %v, want ErrPVENotTarget", name, err)
		}
	}
}

// TestDeployRejectsPVENodePinned `server:<id>` 钉到 PVE 物理机 → 显式拒绝
// (区别于「已删除→跳过」:钉 PVE 是配置错误,须暴露)。
func TestDeployRejectsPVENodePinned(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{}
	pve := seedLabeledServer(t, tgt, "pve-1", "")
	pve.Kind = target.KindPVENode
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop.tar.gz")

	svc := New(tgt, rsvc)
	_, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, Selector: "server:" + pve.ID,
	})
	if err == nil || !errors.Is(err, target.ErrPVENotTarget) {
		t.Fatalf("Deploy pinned pve err = %v, want ErrPVENotTarget", err)
	}
}

// TestDeployLabelSkipsPVENode 标签圈选静默排除 PVE 物理机:即使打了匹配标签也不命中;
// 圈内只剩 PVE 机 → 零命中跳过即成功(与标签配错同语义)。
func TestDeployLabelSkipsPVENode(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{}
	pve := seedLabeledServer(t, tgt, "pve-1", "web")
	pve.Kind = target.KindPVENode
	std := seedLabeledServer(t, tgt, "std-1", "web")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop.tar.gz")

	svc := New(tgt, rsvc)
	res, err := svc.Deploy(context.Background(), DeployInput{RunID: runID, ArtifactID: artID, Selector: "web"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res) != 1 || res[0].ServerID != std.ID {
		t.Fatalf("want only std-1 deployed, got %+v", res)
	}

	// 圈内只剩 PVE 机 → 零命中跳过。
	if err := tgt.Delete(context.Background(), std.ID); err != nil {
		t.Fatalf("delete std: %v", err)
	}
	delete(tgt.servers, std.ID)
	res, err = svc.Deploy(context.Background(), DeployInput{RunID: runID, ArtifactID: artID, Selector: "web"})
	if err != nil || len(res) != 0 {
		t.Fatalf("pve-only match should skip: res=%v err=%v", res, err)
	}
}
