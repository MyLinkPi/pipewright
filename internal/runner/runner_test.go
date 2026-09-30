package runner

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/storetest"
)

type fakeExister struct{ ids map[string]bool }

func (f fakeExister) Exists(_ context.Context, id string) bool { return f.ids[id] }

func testDB(t *testing.T) *store.Store {
	return storetest.Open(t)
}

func seedProject(t *testing.T, st *store.Store) string {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	credID := uuid.NewString()
	if _, err := st.DB.Exec(`INSERT INTO credentials (id,name,type,scope,ciphertext,masked_value,created_at,updated_at) VALUES (?,'c','git_token','',X'00','m',?,?)`, credID, now, now); err != nil {
		t.Fatalf("seed cred: %v", err)
	}
	projID := uuid.NewString()
	if _, err := st.DB.Exec(`INSERT INTO projects (id,name,repo_url,default_branch,credential_id,created_at,updated_at) VALUES (?,'p','https://x/p.git','main',?,?,?)`, projID, credID, now, now); err != nil {
		t.Fatalf("seed proj: %v", err)
	}
	return projID
}

func TestGetDefaultIsLocal(t *testing.T) {
	st := testDB(t)
	svc := New(st.DB, nil)
	projID := seedProject(t, st)
	cfg, err := svc.Get(context.Background(), projID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if cfg.RunnerServerID != "" {
		t.Fatalf("默认应本地构建(空 runner),实际 %q", cfg.RunnerServerID)
	}
	if _, ok := svc.RunnerFor(context.Background(), projID); ok {
		t.Fatal("默认不应有远程 runner")
	}
}

func TestSaveAndGet(t *testing.T) {
	st := testDB(t)
	svc := New(st.DB, fakeExister{ids: map[string]bool{"srv-1": true}})
	projID := seedProject(t, st)

	if _, err := svc.Save(context.Background(), projID, "srv-1"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	cfg, _ := svc.Get(context.Background(), projID)
	if cfg.RunnerServerID != "srv-1" {
		t.Fatalf("runner = %q, want srv-1", cfg.RunnerServerID)
	}
	if id, ok := svc.RunnerFor(context.Background(), projID); !ok || id != "srv-1" {
		t.Fatalf("RunnerFor = %q/%v, want srv-1/true", id, ok)
	}
	// 清空 → 回本地。
	if _, err := svc.Save(context.Background(), projID, ""); err != nil {
		t.Fatalf("Save clear: %v", err)
	}
	if _, ok := svc.RunnerFor(context.Background(), projID); ok {
		t.Fatal("清空后应回本地构建")
	}
}

func TestSaveValidatesServerAndProject(t *testing.T) {
	st := testDB(t)
	svc := New(st.DB, fakeExister{ids: map[string]bool{"srv-1": true}})
	projID := seedProject(t, st)

	if _, err := svc.Save(context.Background(), projID, "ghost"); err != ErrServerNotFound {
		t.Fatalf("配不存在的机应 ErrServerNotFound,实际 %v", err)
	}
	if _, err := svc.Save(context.Background(), "no-such-project", "srv-1"); err != ErrProjectNotFound {
		t.Fatalf("不存在项目应 ErrProjectNotFound,实际 %v", err)
	}
}

func TestSaveSelector(t *testing.T) {
	st := testDB(t)
	svc := New(st.DB, fakeExister{ids: map[string]bool{"srv-1": true}})
	projID := seedProject(t, st)
	ctx := context.Background()

	// 标签选择器:存取往返 + RunnerFor 不适用(非钉死)。
	if _, err := svc.SaveSelector(ctx, projID, "linux,arch=arm64"); err != nil {
		t.Fatalf("SaveSelector: %v", err)
	}
	cfg, _ := svc.Get(ctx, projID)
	if cfg.Selector != "linux,arch=arm64" || cfg.RunnerServerID != "" {
		t.Fatalf("selector=%q runner=%q, want 标签形式/空派生", cfg.Selector, cfg.RunnerServerID)
	}
	if sel, ok := svc.SelectorFor(ctx, projID); !ok || sel != "linux,arch=arm64" {
		t.Fatalf("SelectorFor = %q/%v", sel, ok)
	}
	if _, ok := svc.RunnerFor(ctx, projID); ok {
		t.Fatal("标签选择器下 RunnerFor 应为 false(无单机语义)")
	}

	// 钉死形式:兼容旧入口 Save → server:<id>,派生字段回填。
	if _, err := svc.Save(ctx, projID, "srv-1"); err != nil {
		t.Fatalf("Save 旧入口: %v", err)
	}
	cfg, _ = svc.Get(ctx, projID)
	if cfg.Selector != "server:srv-1" || cfg.RunnerServerID != "srv-1" {
		t.Fatalf("钉死形式 selector=%q runner=%q", cfg.Selector, cfg.RunnerServerID)
	}
	if id, ok := svc.RunnerFor(ctx, projID); !ok || id != "srv-1" {
		t.Fatalf("RunnerFor = %q/%v, want srv-1/true", id, ok)
	}

	// 非法语法 → ErrInvalidSelector;钉死不存在机 → ErrServerNotFound;清空 → 回本地。
	if _, err := svc.SaveSelector(ctx, projID, "arch="); err != ErrInvalidSelector {
		t.Fatalf("坏语法应 ErrInvalidSelector,实际 %v", err)
	}
	if _, err := svc.SaveSelector(ctx, projID, "server:ghost"); err != ErrServerNotFound {
		t.Fatalf("钉死不存在机应 ErrServerNotFound,实际 %v", err)
	}
	if _, err := svc.SaveSelector(ctx, projID, ""); err != nil {
		t.Fatalf("清空: %v", err)
	}
	if _, ok := svc.SelectorFor(ctx, projID); ok {
		t.Fatal("清空后应回本地构建")
	}
}
