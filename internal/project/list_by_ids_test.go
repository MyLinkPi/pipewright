package project

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// insertCredAndProject 直插一条凭据 + 一条项目(ListPagedByIDs 只读 projects/credentials),
// createdAt 用于控制列表排序(created_at DESC)。
func insertCredAndProject(t *testing.T, db *sql.DB, id, name string, createdAt time.Time) {
	t.Helper()
	ts := createdAt.UTC().Format(time.RFC3339)
	credID := "cred-" + id
	if _, err := db.Exec(
		`INSERT INTO credentials (id, name, type, scope, ciphertext, masked_value, created_at, updated_at)
		 VALUES (?, 'c', 'git_token', '', X'00', 'm', ?, ?)`,
		credID, ts, ts,
	); err != nil {
		t.Fatalf("insert credential: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO projects (id, name, repo_url, default_branch, credential_id, created_at, updated_at)
		 VALUES (?, ?, 'https://example.com/p.git', 'main', ?, ?, ?)`,
		id, name, credID, ts, ts,
	); err != nil {
		t.Fatalf("insert project: %v", err)
	}
}

// TestListPagedByIDsEmpty 空/nil 集合 → 空结果(total=0),不生成非法 IN ())。
func TestListPagedByIDsEmpty(t *testing.T) {
	db := testDB(t)
	svc := New(db, nil, nil)
	insertCredAndProject(t, db, "p1", "one", time.Now())

	for _, ids := range [][]string{nil, {}} {
		res, err := svc.ListPagedByIDs(context.Background(), ids, 1, 0)
		if err != nil {
			t.Fatalf("ListPagedByIDs(%v): %v", ids, err)
		}
		if res == nil {
			t.Fatalf("ListPagedByIDs(%v) 返回 nil", ids)
		}
		if res.Total != 0 || len(res.Items) != 0 {
			t.Fatalf("空集合应返回空结果, got total=%d items=%d", res.Total, len(res.Items))
		}
	}
}

// TestListPagedByIDsFiltersAndPaginates 只返回集合内且实际存在的项目;分页按过滤后计。
func TestListPagedByIDsFiltersAndPaginates(t *testing.T) {
	db := testDB(t)
	svc := New(db, nil, nil)
	base := time.Now().Add(-time.Hour)
	insertCredAndProject(t, db, "p1", "one", base.Add(1*time.Second))
	insertCredAndProject(t, db, "p2", "two", base.Add(2*time.Second))
	insertCredAndProject(t, db, "p3", "three", base.Add(3*time.Second))
	insertCredAndProject(t, db, "p4", "four", base.Add(4*time.Second))

	// 集合内含一个不存在的 id:只返回实际存在的(3 条),total 按存在计。
	ids := []string{"p1", "p3", "p4", "no-such-id"}

	page1, err := svc.ListPagedByIDs(context.Background(), ids, 1, 2)
	if err != nil {
		t.Fatalf("ListPagedByIDs(page1): %v", err)
	}
	if page1.Total != 3 {
		t.Fatalf("total = %d, want 3", page1.Total)
	}
	if page1.Page != 1 || page1.PageSize != 2 {
		t.Fatalf("分页元信息异常: page=%d pageSize=%d", page1.Page, page1.PageSize)
	}
	if len(page1.Items) != 2 {
		t.Fatalf("第一页应 2 条, got %d", len(page1.Items))
	}
	// created_at DESC:p4(4s) > p3(3s) > p1(1s)。
	if page1.Items[0].ID != "p4" || page1.Items[1].ID != "p3" {
		t.Fatalf("第一页排序/集合异常: %+v", page1.Items)
	}
	for _, it := range page1.Items {
		if it.ID == "p2" {
			t.Fatalf("返回了未请求的 p2: %+v", page1.Items)
		}
	}

	page2, err := svc.ListPagedByIDs(context.Background(), ids, 2, 2)
	if err != nil {
		t.Fatalf("ListPagedByIDs(page2): %v", err)
	}
	if page2.Total != 3 || len(page2.Items) != 1 {
		t.Fatalf("第二页应 total=3 items=1, got total=%d items=%d", page2.Total, len(page2.Items))
	}
	if page2.Items[0].ID != "p1" {
		t.Fatalf("第二页应 p1, got %q", page2.Items[0].ID)
	}
}
