package pipeline

import (
	"context"
	"testing"
)

// TestRegServiceRefsNoConfigIsReadOnly 验证无流水线配置行时返回空引用,
// 且不触发惰性默认生成(项目卡片列表读取绑定不应产生写库副作用)。
func TestRegServiceRefsNoConfigIsReadOnly(t *testing.T) {
	svc, db, projID := newSvc(t)
	refs, err := svc.RegServiceRefs(context.Background(), projID)
	if err != nil {
		t.Fatalf("RegServiceRefs: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("无配置应返回空引用, got %v", refs)
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pipeline_configs WHERE project_id = ?`, projID).Scan(&n); err != nil {
		t.Fatalf("count pipeline_configs: %v", err)
	}
	if n != 0 {
		t.Fatalf("RegServiceRefs 不应落库默认配置, pipeline_configs 行数 = %d", n)
	}
}

// TestRegServiceRefsCollectsDedupsAndOrders 覆盖:按出现顺序收集、去重、
// 空值跳过,以及 gatewayService(legacy 服务名)在 regServiceId 缺省时兜底。
func TestRegServiceRefsCollectsDedupsAndOrders(t *testing.T) {
	svc, _, projID := newSvc(t)
	spec := Spec{Stages: []Stage{
		{Name: "流水线源", Kind: KindSource, Jobs: []Job{
			{Name: "Gitee 源", Type: "git_source", Config: map[string]any{}},
		}},
		{Name: "部署", Kind: KindDeploy, Jobs: []Job{
			{Name: "d1", Type: "deploy", Config: map[string]any{"regServiceId": "svc-1", "regPort": "8080"}},
			{Name: "d2", Type: "deploy", Config: map[string]any{"regServiceId": "svc-2"}},
			{Name: "d3-legacy", Type: "deploy", Config: map[string]any{"gatewayService": "shop"}},
			{Name: "d4-dup", Type: "deploy", Config: map[string]any{"regServiceId": "svc-1"}},
			{Name: "d5-legacy-dup", Type: "deploy", Config: map[string]any{"regServiceId": "  ", "gatewayService": " shop "}},
			{Name: "d6-none", Type: "deploy", Config: map[string]any{"containerName": "web"}},
		}},
	}}
	if _, err := svc.Save(context.Background(), projID, spec); err != nil {
		t.Fatalf("Save: %v", err)
	}
	refs, err := svc.RegServiceRefs(context.Background(), projID)
	if err != nil {
		t.Fatalf("RegServiceRefs: %v", err)
	}
	want := []string{"svc-1", "svc-2", "shop"}
	if len(refs) != len(want) {
		t.Fatalf("refs = %v, want %v", refs, want)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Fatalf("refs[%d] = %q, want %q(总: %v)", i, refs[i], want[i], refs)
		}
	}
}

// TestRegServiceRefsPrefersServiceID 验证同一 job 同时存在 regServiceId 与
// gatewayService 时取 regServiceId(与部署期 regServiceRef 语义一致)。
func TestRegServiceRefsPrefersServiceID(t *testing.T) {
	svc, _, projID := newSvc(t)
	spec := validSpec()
	spec.Stages[2].Jobs = []Job{{
		Name:   "d1",
		Type:   "deploy",
		Config: map[string]any{"regServiceId": "svc-9", "gatewayService": "legacy-name"},
	}}
	if _, err := svc.Save(context.Background(), projID, spec); err != nil {
		t.Fatalf("Save: %v", err)
	}
	refs, err := svc.RegServiceRefs(context.Background(), projID)
	if err != nil {
		t.Fatalf("RegServiceRefs: %v", err)
	}
	if len(refs) != 1 || refs[0] != "svc-9" {
		t.Fatalf("refs = %v, want [svc-9]", refs)
	}
}