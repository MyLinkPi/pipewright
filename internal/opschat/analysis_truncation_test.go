package opschat

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"testing"

	"github.com/google/uuid"
)

func TestSelectAnalysisCallsPrefersInformative(t *testing.T) {
	mk := func(i int, output string) callBody {
		return callBody{View: Call{ID: "c" + strconv.Itoa(i), Status: Succeeded, Output: output}}
	}
	var candidates []callBody
	for i := 0; i < 30; i++ {
		if i < 10 {
			candidates = append(candidates, mk(i, "")) // 低价值:成功但空输出
		} else {
			candidates = append(candidates, mk(i, "data")) // 高价值:有输出
		}
	}
	// 不超限:原样返回。
	if got := selectAnalysisCalls(candidates[:24], 24); len(got) != 24 {
		t.Fatal("under limit should keep all", len(got))
	}
	// 超限:保留全部 20 条高价值 + 最早的 4 条低价值,按时间序。
	got := selectAnalysisCalls(candidates, 24)
	if len(got) != 24 {
		t.Fatal("expected cap at limit", len(got))
	}
	want := []string{"c0", "c1", "c2", "c3"}
	for i := 10; i < 30; i++ {
		want = append(want, "c"+strconv.Itoa(i))
	}
	for i, c := range got {
		if c.View.ID != want[i] {
			t.Fatalf("selection order/value mismatch at %d: got %s want %s", i, c.View.ID, want[i])
		}
	}
}

// 大运行(>24 调用)的汇总预览应优雅截断而非整轮失败:保留高价值调用、计入
// Omitted,且用截断后的 CallIDs 重算预览 Hash 一致(漂移闸不误触发)。
func TestAnalysisPreviewTruncatesLargeRun(t *testing.T) {
	m := model()
	id := uuid.NewString()
	f := executor(id)
	s, _, _ := fixture(t, f, m)
	c := chat(t, s, id)
	r := submitToolPlan(t, s, m, c, "host_ports", "{}")
	snap := waitRun(t, s, c.ID, r.ID, Succeeded)
	waitJobs(t, s)
	base := snap.Calls[0].ID
	ids := []string{base}
	informative := map[string]bool{base: true}
	if e := s.transaction(context.Background(), c.ID, true, func(tx *sql.Tx) error {
		rec, e := s.call(context.Background(), tx, c.ID, base)
		if e != nil {
			return e
		}
		for i := 0; i < 30; i++ {
			clone := rec
			clone.Body.View.ID = uuid.NewString()
			clone.Body.View.Status = Succeeded
			if i < 10 {
				clone.Body.View.Output = ""
			} else {
				clone.Body.View.Output = "data-" + strconv.Itoa(i)
				informative[clone.Body.View.ID] = true
			}
			b, e := s.seal(clone.Body)
			if e != nil {
				return e
			}
			v := clone.Body.View
			if _, e := tx.ExecContext(context.Background(), `INSERT INTO ops_chat_calls(id,session_id,run_id,server_id,tool_id,status,args_hash,target_hash,body,created_at,ordinal) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
				v.ID, c.ID, r.ID, v.ServerID, v.ToolID, v.Status, v.ArgsHash, v.TargetHash, b, clone.Created, clone.Ordinal+i+1); e != nil {
				return e
			}
			ids = append(ids, v.ID)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	// 非源顺序的全量预览也必须可重放,不能依赖调用 ID 的提交顺序恢复省略数。
	ids = append(ids[7:], ids[:7]...)
	preview, err := s.AnalysisPreview(context.Background(), c.ID, ids)
	if err != nil {
		t.Fatal("large run should truncate, not fail", err)
	}
	if len(preview.CallIDs) != resourceLimits.Calls {
		t.Fatal("expected capped CallIDs", len(preview.CallIDs))
	}
	if preview.Omitted != len(ids)-resourceLimits.Calls {
		t.Fatal("omitted count", preview.Omitted, len(ids))
	}
	kept := map[string]bool{}
	for _, cid := range preview.CallIDs {
		kept[cid] = true
	}
	for cid := range informative {
		if !kept[cid] {
			t.Fatal("informative call dropped", cid)
		}
	}
	again, err := s.analysisPreview(context.Background(), c.ID, preview.CallIDs, "", preview.Omitted)
	if err != nil || again.Hash != preview.Hash {
		t.Fatal("hash drift on truncated re-preview", err, again.Hash, preview.Hash)
	}
	if again.Omitted != preview.Omitted {
		t.Fatal("re-preview lost bound omission count", again.Omitted, preview.Omitted)
	}

	// 预览确认重放也必须保留省略数,不能把截断汇总冒充全量汇总。
	if _, err := s.Analyze(context.Background(), c.ID, AnalysisInput{ClientRequestID: uuid.NewString(), Revision: c.Revision, CallIDs: preview.CallIDs, PreviewHash: preview.Hash, Provider: preview.Provider, Consent: true}); !errors.Is(err, ErrConsent) {
		t.Fatal("dropping Omitted must fail preview binding", err)
	}
	analysisRun, err := s.Analyze(context.Background(), c.ID, AnalysisInput{ClientRequestID: uuid.NewString(), Revision: c.Revision, CallIDs: preview.CallIDs, PreviewHash: preview.Hash, Omitted: preview.Omitted, Provider: preview.Provider, Consent: true})
	if err != nil {
		t.Fatal("preview/consent replay", err)
	}
	waitRun(t, s, c.ID, analysisRun.ID, Succeeded)
	waitJobs(t, s)
	m.mu.Lock()
	if len(m.analyses) != 1 || m.analyses[0].Omitted != len(ids)-resourceLimits.Calls {
		m.mu.Unlock()
		t.Fatal("preview/consent replay lost omission count", m.analyses)
	}
	m.mu.Unlock()

	// 任意手动子集即使来自同一轮,也不能误报为上限省略。
	single, err := s.AnalysisPreview(context.Background(), c.ID, []string{base})
	if err != nil {
		t.Fatal(err)
	}
	subsetRun, err := s.Analyze(context.Background(), c.ID, AnalysisInput{ClientRequestID: uuid.NewString(), Revision: c.Revision, CallIDs: single.CallIDs, PreviewHash: single.Hash, Provider: single.Provider, Consent: true})
	if err != nil {
		t.Fatal("manual subset analysis", err)
	}
	waitRun(t, s, c.ID, subsetRun.ID, Succeeded)
	waitJobs(t, s)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.analyses) != 2 || m.analyses[1].Omitted != 0 {
		t.Fatal("manual subset mislabeled as truncated", m.analyses)
	}
}
