package run

import (
	"context"
	"testing"
	"time"
)

// TestRecentCommitTags 验证镜像轮转的保留集数据源:项目最近 n 个去重 trigger_commit(新→旧)。
func TestRecentCommitTags(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	projID := seedProject(t, db)

	// 依次建 4 个 run 并回写 commit(c1 最旧、c3 最新;c2 重复出现模拟同 commit 反复触发)。
	// created_at 为秒级精度且 id 是随机 UUID,同秒创建的 run 顺序不稳定(平台 List 同口径),
	// 故显式回写错开的时间戳来固定顺序。
	commits := []string{"c1aaa", "c2bbb", "c2bbb", "c3ccc"}
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i, c := range commits {
		r, err := svc.Create(context.Background(), projID, Trigger{Type: TriggerManual})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := svc.SetCommit(context.Background(), r.ID, c, time.Time{}); err != nil {
			t.Fatalf("SetCommit: %v", err)
		}
		ts := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		if _, err := db.Exec(`UPDATE pipeline_runs SET created_at = ? WHERE id = ?`, ts, r.ID); err != nil {
			t.Fatalf("回写 created_at: %v", err)
		}
	}

	got, err := svc.RecentCommitTags(context.Background(), projID, 2)
	if err != nil {
		t.Fatalf("RecentCommitTags: %v", err)
	}
	if len(got) != 2 || got[0] != "c3ccc" || got[1] != "c2bbb" {
		t.Fatalf("最近 2 个去重 commit 应为 [c3ccc c2bbb],实际:%v", got)
	}

	all, err := svc.RecentCommitTags(context.Background(), projID, 10)
	if err != nil {
		t.Fatalf("RecentCommitTags(10): %v", err)
	}
	if len(all) != 3 { // c3ccc, c2bbb, c1aaa
		t.Fatalf("全量去重应为 3 个,实际:%v", all)
	}

	// n<=0 → 空;其它项目 → 空。
	if got, _ := svc.RecentCommitTags(context.Background(), projID, 0); len(got) != 0 {
		t.Fatalf("n=0 应返回空,实际:%v", got)
	}
	if got, _ := svc.RecentCommitTags(context.Background(), "no-such-project", 5); len(got) != 0 {
		t.Fatalf("未知项目应返回空,实际:%v", got)
	}
}
