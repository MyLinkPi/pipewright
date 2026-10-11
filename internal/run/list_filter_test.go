package run

import (
	"context"
	"testing"
)

// TestListAllowedProjectIDsEmpty 验证 AllowedProjectIDs 为非 nil 空切片时直接空结果
// (不生成非法 IN ();total=0;不报错)。
func TestListAllowedProjectIDsEmpty(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	projA := seedProject(t, db)
	seedRunRow(t, svc, projA, StatusSuccess)

	res, err := svc.List(context.Background(), ListFilter{AllowedProjectIDs: []string{}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res == nil {
		t.Fatal("List 返回 nil 结果")
	}
	if res.Total != 0 || len(res.Items) != 0 {
		t.Fatalf("空白名单应返回空结果, got total=%d items=%d", res.Total, len(res.Items))
	}
}

// TestListAllowedProjectIDsFilters 验证按成员项目集合过滤;nil 表示不过滤(返回全部)。
func TestListAllowedProjectIDsFilters(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	projA := seedProject(t, db)
	projB := seedProject(t, db)
	runA := seedRunRow(t, svc, projA, StatusSuccess)
	seedRunRow(t, svc, projB, StatusSuccess)

	// 仅 projA → 只返回 A 的运行。
	res, err := svc.List(context.Background(), ListFilter{AllowedProjectIDs: []string{projA}})
	if err != nil {
		t.Fatalf("List(projA): %v", err)
	}
	if res.Total != 1 || len(res.Items) != 1 {
		t.Fatalf("按 projA 过滤应恰 1 条, got total=%d items=%d", res.Total, len(res.Items))
	}
	if res.Items[0].ID != runA || res.Items[0].ProjectID != projA {
		t.Fatalf("返回了非 A 的运行: %+v", res.Items[0])
	}

	// nil → 不启用过滤,返回全部。
	all, err := svc.List(context.Background(), ListFilter{})
	if err != nil {
		t.Fatalf("List(nil): %v", err)
	}
	if all.Total != 2 || len(all.Items) != 2 {
		t.Fatalf("nil 白名单应返回全部 2 条, got total=%d items=%d", all.Total, len(all.Items))
	}
}
