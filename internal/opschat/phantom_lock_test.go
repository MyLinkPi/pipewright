package opschat

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/target"
)

// 回归(线上「AI 运维助手选不了机器 + 面板卡死」):幻影 run(DB 非终态 +
// 无活 worker)不得锁死会话——读取投影立即解锁,Cancel 持久收敛,
// Patch/Submit 直接接管。
func TestPhantomRunDoesNotLockSession(t *testing.T) {
	id := uuid.NewString()
	f := executor(id)
	m := model()
	s, v, db := fixture(t, f, m)
	defer func() { _ = s.Close(context.Background()) }()
	c := chat(t, s, id)

	// 播种幻影:running + 已派发调用 + active_run 指向 + 无 worker。
	snap0, _ := f.Capture(context.Background(), id)
	run := uuid.NewString()
	body, _ := s.seal(runBody{Turn: TurnInput{Text: "q"}, Targets: []target.ConnectionSnapshot{snap0}})
	if _, err := db.Exec("INSERT INTO ops_chat_runs(id,session_id,request_id,payload_hash,status,body,created_at) VALUES(?,?,?,?,?,?,?)",
		run, c.ID, uuid.NewString(), "h", Running, body, time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	cid := uuid.NewString()
	cb, _ := s.seal(callBody{Snapshot: snap0, Dispatched: true, View: Call{ID: cid, SessionID: c.ID, RunID: run, ServerID: id, ToolID: "host_ports", Status: Running}})
	if _, err := db.Exec("INSERT INTO ops_chat_calls(id,session_id,run_id,server_id,tool_id,status,args_hash,target_hash,body,created_at,ordinal) VALUES(?,?,?,?,?,?,?,?,?,?,?)",
		cid, c.ID, run, id, "host_ports", Running, "h", snap0.Fingerprint(), cb, time.Now().Add(-time.Minute).UnixMilli(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE ops_chat_sessions SET active_run=? WHERE id=?", run, c.ID); err != nil {
		t.Fatal(err)
	}

	// 1) 读取投影:面板立即解锁,run/call 视图为 Unknown(派发未归档)。
	snap, err := s.Snapshot(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Session.ActiveRunID != "" {
		t.Fatal("phantom run still locks session:", snap.Session)
	}
	var projected *Run
	for i := range snap.Runs {
		if snap.Runs[i].ID == run {
			projected = &snap.Runs[i]
		}
	}
	if projected == nil || projected.Status != Unknown {
		t.Fatalf("run not projected to unknown: %+v", projected)
	}
	if len(snap.Calls) != 1 || snap.Calls[0].Status != Unknown {
		t.Fatalf("call not projected: %+v", snap.Calls)
	}

	// 2) Cancel:持久收敛而非插旗空转。
	if _, err = s.Cancel(context.Background(), c.ID, run); err != nil {
		t.Fatal(err)
	}
	var status string
	if err = db.QueryRow("SELECT status FROM ops_chat_runs WHERE id=?", run).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != Unknown {
		t.Fatal("cancel did not durably converge phantom:", status)
	}
	var active any
	if err = db.QueryRow("SELECT active_run FROM ops_chat_sessions WHERE id=?", c.ID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != "" {
		t.Fatal("active_run not released:", active)
	}

	// 3) 目标修改不再被幻影指针挡住。
	targets := []string{id}
	if _, err = s.Patch(context.Background(), c.ID, PatchInput{ServerIDs: &targets, Revision: snap.Session.Revision}); err != nil {
		t.Fatal("patch targets blocked by phantom:", err)
	}

	// 4) 再播种一个 planning 幻影(未派发),新一轮提交应同事务接管收敛。
	run2 := seedReviewRun(t, s, db, c, Planning, runBody{Turn: TurnInput{Text: "stale"}})
	if _, err = db.Exec("UPDATE ops_chat_runs SET created_at=? WHERE id=?", time.Now().Add(-time.Minute).UnixMilli(), run2); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.Get(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	fresh.ServerIDs = []string{id}
	r3 := submitToolPlan(t, s, m, fresh, "host_ports", `{}`)
	var stale string
	if err = db.QueryRow("SELECT status FROM ops_chat_runs WHERE id=?", run2).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != Interrupted {
		t.Fatal("enqueue did not converge stale phantom:", stale)
	}
	waitRun(t, s, c.ID, r3.ID, Succeeded, PartialFailed, Failed, Interrupted, Unknown)
	waitJobs(t, s)
	_ = v
}

// 活 run(有 worker 在跑)不被投影误伤;worker 悬死后无需重启即自愈,
// Cancel 随后把 DB 行持久收敛。
func TestLiveRunNotProjectedAndSelfHealsWithoutRestart(t *testing.T) {
	id := uuid.NewString()
	f := executor(id)
	m := model()
	s, _, db := fixture(t, f, m)
	defer func() { _ = s.Close(context.Background()) }()
	c := chat(t, s, id)
	started := make(chan struct{})
	f.run = func(context.Context, target.ConnectionSnapshot, []string, target.ExecutionLimits) (*target.LimitedResult, error) {
		started <- struct{}{}
		select {} // 悬死:永不返回
	}
	r := submitToolPlan(t, s, m, c, "host_ports", `{}`)
	// 执行器已被调用:派发检查点(Running+Dispatched)已先行落库,投影可判 Unknown。
	<-started

	// 等 worker 真正进入执行(running),活 run 必须原样呈现。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snap, err := s.Snapshot(context.Background(), c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if snap.Session.ActiveRunID == r.ID {
			for _, rr := range snap.Runs {
				if rr.ID == r.ID && rr.Status == Running {
					goto live
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("run never reached running")
live:
	// worker 悬死(内存 job 消失,DB 仍是 running):读取投影立即自愈。
	s.mu.Lock()
	delete(s.jobs, r.ID)
	s.mu.Unlock()
	var healed *Snapshot
	for time.Now().Before(deadline) {
		h, err := s.Snapshot(context.Background(), c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if h.Session.ActiveRunID == "" {
			healed = h
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if healed == nil {
		t.Fatal("phantom did not self-heal without restart")
	}
	for _, rr := range healed.Runs {
		if rr.ID == r.ID && rr.Status != Unknown {
			t.Fatalf("hanging run not projected to unknown: %+v", rr)
		}
	}

	// Cancel 把 DB 行持久收敛(派发未归档 → Unknown)。
	if _, err := s.Cancel(context.Background(), c.ID, r.ID); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := db.QueryRow("SELECT status FROM ops_chat_runs WHERE id=?", r.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != Unknown {
		t.Fatal("cancel did not converge hanging run:", status)
	}
}

// awaiting_continuation(预算等待「继续」)同样是 worker 已退出的挂起态:
// Cancel 必须直落收敛,不得只插旗造成永久锁死。
func TestCancelAwaitingContinuationConverges(t *testing.T) {
	id := uuid.NewString()
	f := executor(id)
	m := model()
	s, v, db := fixture(t, f, m)
	defer func() { _ = s.Close(context.Background()) }()
	c := chat(t, s, id)
	run := seedReviewRun(t, s, db, c, AwaitingContinuation, runBody{Turn: TurnInput{Text: "q"}})
	if _, err := s.Cancel(context.Background(), c.ID, run); err != nil {
		t.Fatal(err)
	}
	snap := waitRun(t, s, c.ID, run, Interrupted)
	if snap.Session.ActiveRunID != "" {
		t.Fatal(snap.Session)
	}
	_ = v
}

// 回归(线上后端整体冻结的根因):注入的 Context 策略闭包会同步读库
// (aiContextPolicy→ai.load 走连接池),而只读事务持有唯一连接——若在事务
// 内调用 policy(),会等待被自己占用的连接,自死锁并冻结整个进程。会话需
// 有带 token usage 的 assistant 条目(usage==nil 时提前返回,不触发 policy)。
func TestSnapshotSurvivesPoolReadingContextSource(t *testing.T) {
	id := uuid.NewString()
	f := executor(id)
	s, _, db := fixture(t, f, nil)
	defer func() { _ = s.Close(context.Background()) }()
	c := chat(t, s, id)
	// 模拟 aiContextPolicy:闭包内部直接查库(经连接池)。
	s.contextSrc = func() ContextPolicy {
		var one int
		_ = s.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM ops_chat_sessions").Scan(&one)
		return ContextPolicy{}
	}
	// 播种带 usage 的 assistant 条目,使 sessionUsage 走到 policy() 那一行。
	body, err := s.seal(struct {
		Text  string
		Usage *AgentUsage
	}{"answer", &AgentUsage{InputTokens: 100, OutputTokens: 50}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO ops_chat_entries(session_id,seq,kind,run_id,call_id,context_allowed,visible,body,body_size,created_at) VALUES(?,?,?,?,?,1,1,?,?,?)`,
		c.ID, 2, "assistant", "", "", body, 6, time.Now().UnixMilli()-1000); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE ops_chat_sessions SET seq=2,visible_count=2 WHERE id=?`, c.ID); err != nil {
		t.Fatal(err)
	}

	// 看门狗:修复前 Snapshot 在事务内调 contextSrc → 死锁 → 超时失败。
	done := make(chan error, 1)
	go func() {
		_, err := s.Snapshot(context.Background(), c.ID)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Snapshot deadlocked: context source did pool IO inside the read transaction")
	}

	// Events(SSE 路径)同样不得死锁。
	evDone := make(chan error, 1)
	go func() {
		_, err := s.Events(context.Background(), c.ID, c.ID+":0", 10)
		evDone <- err
	}()
	select {
	case err := <-evDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Events deadlocked: context source did pool IO inside the read transaction")
	}
}

// P2 回归:快照/事件投影按展示预算裁剪 call 输出并标记 truncated;单 call
// GET 端点始终返回完整输出(前端按需展开)。这是大会话快照 30+MB 响应的
// 体积防线。
func TestSnapshotTrimsCallOutputPreview(t *testing.T) {
	id := uuid.NewString()
	f := executor(id)
	s, _, db := fixture(t, f, nil)
	defer func() { _ = s.Close(context.Background()) }()
	c := chat(t, s, id)
	big := strings.Repeat("o", 60<<10)
	cid := uuid.NewString()
	run := uuid.NewString()
	rb, _ := s.seal(runBody{Turn: TurnInput{Text: "q"}})
	if _, err := db.Exec("INSERT INTO ops_chat_runs(id,session_id,request_id,payload_hash,status,body,created_at) VALUES(?,?,?,?,?,?,?)",
		run, c.ID, uuid.NewString(), "h", Succeeded, rb, time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	cb, _ := s.seal(callBody{View: Call{ID: cid, SessionID: c.ID, RunID: run, ServerID: id, ToolID: "host_resources", Status: Succeeded, Output: big}})
	if _, err := db.Exec("INSERT INTO ops_chat_calls(id,session_id,run_id,server_id,tool_id,status,args_hash,target_hash,body,created_at,ordinal) VALUES(?,?,?,?,?,?,?,?,?,?,?)",
		cid, c.ID, run, id, "host_resources", Succeeded, "a", "t", cb, time.Now().Add(-time.Minute).UnixMilli(), 0); err != nil {
		t.Fatal(err)
	}
	eb, _ := s.seal(struct{ Text string }{"done"})
	if _, err := db.Exec("INSERT INTO ops_chat_entries(session_id,seq,kind,run_id,call_id,context_allowed,visible,body,body_size,created_at) VALUES(?,?,?,?,?,1,1,?,?,?)",
		c.ID, 2, "tool_call", run, cid, eb, 4, time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE ops_chat_sessions SET seq=2,visible_count=2 WHERE id=?", c.ID); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Calls) != 1 {
		t.Fatalf("calls: %+v", snap.Calls)
	}
	if !snap.Calls[0].Truncated || len(snap.Calls[0].Output) > snapshotOutputLimit+64 {
		t.Fatalf("snapshot output not trimmed: truncated=%v len=%d", snap.Calls[0].Truncated, len(snap.Calls[0].Output))
	}
	full, err := s.Call(context.Background(), c.ID, cid)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Output) != len(big) {
		t.Fatalf("per-call endpoint lost full output: %d", len(full.Output))
	}
}
