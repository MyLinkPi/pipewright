package deploy

// static_release_test.go 验证「网关静态发布」(deploy_static 节点 → cfg["staticGateway"]="1"):
// 目标 = 全部网关主机(注册绑定解析,不走 selector)、releaseBase = 静态发布目录、
// 默认健康门控 = current/index.html 存在性、不做实例注册/摘挂/清理。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/run"
)

// TestDeployStaticPublishesToAllGateways:两台网关均发布到 <BaseDir>/releases/<runId>,
// 健康门控命中 index.html 存在性检查,全程不触实例接口。
func TestDeployStaticPublishesToAllGateways(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	fs := &fakeFS{}
	tgt := newFakeTarget(fs)
	gw1 := seedServer(t, tgt, "gw-1")
	gw2 := seedServer(t, tgt, "gw-2")
	runID, _ := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop.tar.gz")

	base := "/var/lib/pipewright-nginx/static/svc-static-1"
	gw := &fakeGateway{static: &StaticDeploySpec{
		ServiceID: "svc-static-1", FQDN: "app.efg.com",
		ServerIDs: []string{gw1.ID, gw2.ID}, BaseDir: base,
	}}
	svc := New(tgt, rsvc, WithInstanceGateway(gw))

	res, err := svc.DeployForStage(context.Background(), runID, "", map[string]string{
		"staticGateway": "1", "regServiceId": "svc-static-1", "artifactType": "dist",
	}, "")
	if err != nil {
		t.Fatalf("DeployForStage(static): %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("应覆盖 2 台网关,得 %d:%+v", len(res), res)
	}
	for _, r := range res {
		if r.Status != run.TargetSuccess {
			t.Fatalf("网关 %s 应成功:%+v", r.ServerName, r)
		}
	}
	// 发布目录:releaseBase 覆盖为静态目录;current 软链已切到新发布。
	mkdir := findCmd(fs.calls, "mkdir")
	if mkdir == nil || !strings.Contains(mkdir[len(mkdir)-1], base+"/releases/") {
		t.Fatalf("mkdir 未落静态发布目录: %v", mkdir)
	}
	if !strings.Contains(fs.current, base+"/releases/") {
		t.Fatalf("current 未指向静态新发布: %q", fs.current)
	}
	// 默认健康门控:index.html 存在性。
	hc := findCmd(fs.calls, "test")
	if hc == nil || strings.Join(hc, " ") != "test -f "+base+"/current/index.html" {
		t.Fatalf("默认健康门控应为 index.html 存在性: %v", hc)
	}
	// 静态服务无实例语义:不触任何实例接口。
	if len(gw.ensures) != 0 || len(gw.ensuredRefs) != 0 || gw.detachCount() != 0 || gw.attachCount() != 0 || len(gw.pruned) != 0 {
		t.Fatalf("静态发布不应触实例接口: ensures=%v ensuredRefs=%v detach=%d attach=%d pruned=%v",
			gw.ensures, gw.ensuredRefs, gw.detachCount(), gw.attachCount(), gw.pruned)
	}
}

// TestDeployStaticHealthFailRollsBack:门控失败(缺 index.html)且无上一发布 → failed,
// 不触实例接口。
func TestDeployStaticHealthFailRollsBack(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	fs := &fakeFS{failTest: true}
	tgt := newFakeTarget(fs)
	gw1 := seedServer(t, tgt, "gw-1")
	runID, _ := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop.tar.gz")

	gw := &fakeGateway{static: &StaticDeploySpec{
		ServiceID: "svc-static-1", FQDN: "app.efg.com", ServerIDs: []string{gw1.ID},
		BaseDir: "/var/lib/pipewright-nginx/static/svc-static-1",
	}}
	svc := New(tgt, rsvc, WithInstanceGateway(gw))
	res, err := svc.DeployForStage(context.Background(), runID, "", map[string]string{
		"staticGateway": "1", "regServiceId": "svc-static-1", "artifactType": "dist",
	}, "")
	if err != nil {
		t.Fatalf("DeployForStage(static): %v", err)
	}
	if res[0].Status != run.TargetFailed {
		t.Fatalf("门控失败应 failed:%+v", res[0])
	}
	if !strings.Contains(res[0].Message, "健康检查失败") {
		t.Fatalf("失败信息应说明健康检查:%s", res[0].Message)
	}
}

// TestDeployStaticRejectsNonDist:非 dist 产物 → 明确报错。
func TestDeployStaticRejectsNonDist(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	fs := &fakeFS{}
	tgt := newFakeTarget(fs)
	gw1 := seedServer(t, tgt, "gw-1")
	runID, _ := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactJar, "dist/shop.jar")

	gw := &fakeGateway{static: &StaticDeploySpec{
		ServiceID: "svc-static-1", ServerIDs: []string{gw1.ID},
		BaseDir: "/var/lib/pipewright-nginx/static/svc-static-1",
	}}
	svc := New(tgt, rsvc, WithInstanceGateway(gw))
	_, err := svc.DeployForStage(context.Background(), runID, "", map[string]string{
		"staticGateway": "1", "regServiceId": "svc-static-1", "artifactType": "jar",
	}, "")
	if err == nil || !strings.Contains(err.Error(), "dist") {
		t.Fatalf("非 dist 产物应明确报错,得 %v", err)
	}
}

// TestDeployStaticRequiresBinding:缺注册绑定 → ErrStaticNoBinding。
func TestDeployStaticRequiresBinding(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	fs := &fakeFS{}
	tgt := newFakeTarget(fs)
	runID, _ := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop.tar.gz")

	svc := New(tgt, rsvc, WithInstanceGateway(&fakeGateway{static: &StaticDeploySpec{}}))
	_, err := svc.DeployForStage(context.Background(), runID, "", map[string]string{
		"staticGateway": "1", "artifactType": "dist",
	}, "")
	if !errors.Is(err, ErrStaticNoBinding) {
		t.Fatalf("缺绑定应 ErrStaticNoBinding,得 %v", err)
	}
}
