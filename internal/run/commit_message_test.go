package run

import (
	"context"
	"testing"
	"time"
)

// TestSetCommitPersistsCommitMessage:SetCommit 连带 subject 首行落库,Get 回读带出;
// 空 message(未解析出)不覆盖既有值;运行详情「COMMIT 旁展示 message」依赖此回读。
func TestSetCommitPersistsCommitMessage(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	ctx := context.Background()
	projID := seedProject(t, db)

	r, err := svc.Create(ctx, projID, Trigger{Type: TriggerManual, Branch: "main", Actor: "admin"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.SetCommit(ctx, r.ID, "abc1234", time.Time{}, "fix(pveadmin): 适配混合形态"); err != nil {
		t.Fatalf("SetCommit: %v", err)
	}
	got, err := svc.Get(ctx, r.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Trigger.Commit != "abc1234" {
		t.Fatalf("Commit = %q, want abc1234", got.Trigger.Commit)
	}
	if got.Trigger.CommitMessage != "fix(pveadmin): 适配混合形态" {
		t.Fatalf("CommitMessage = %q, want 修复 subject", got.Trigger.CommitMessage)
	}

	// 列表(运行列表 / ops 列表同源)也必须回读 message,而不是恒为空串。
	list, err := svc.List(ctx, ListFilter{ProjectID: projID, Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Trigger.CommitMessage != "fix(pveadmin): 适配混合形态" {
		t.Fatalf("List CommitMessage = %+v, want 修复 subject", list.Items)
	}

	// 再次回写但 message 为空(幂等重放)→ 不清空既有 message。
	if err := svc.SetCommit(ctx, r.ID, "abc1234", time.Time{}, ""); err != nil {
		t.Fatalf("SetCommit replay: %v", err)
	}
	got, err = svc.Get(ctx, r.ID)
	if err != nil {
		t.Fatalf("Get after replay: %v", err)
	}
	if got.Trigger.CommitMessage != "fix(pveadmin): 适配混合形态" {
		t.Fatalf("空 message 回写不应清空既有值,实际 = %q", got.Trigger.CommitMessage)
	}
}
