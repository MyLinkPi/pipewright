package opschat

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func (s *Service) publicSession(c Session) Session {
	c.Title = s.scrub(c.Title, 320)
	c.Draft = s.scrub(c.Draft, 8<<10)
	return c
}

// ─── run 生命周期权威:内存 jobs + 读取时惰性投影 ─────────────────────────
//
// run 是否「活着」的唯一权威是内存 s.jobs(活 worker);DB 里的 run.status 与
// sessions.active_run 只是事实记录,可能因 worker 悬死/落库失败而残留活动态
// (幻影)。HTTP 读取边界用下面的投影把幻影只读降级为终态视图,绝不回写 DB;
// Cancel/enqueue 在写事务内用 recoverRunTx 就地持久收敛。

// jobLive 报告该 run 是否有活 worker 在内存中执行。只持 mu,绝不做 DB IO。
func (s *Service) jobLive(runID string) bool {
	s.mu.Lock()
	_, live := s.jobs[runID]
	s.mu.Unlock()
	return live
}

// freshQueuedTol 容忍 enqueue 事务提交到 launch 注册 job 之间的注册窗口:
// 刚入库的 queued run 即使尚无 job 也按活处理,避免读取投影误杀正在启动的轮。
const freshQueuedTol = 5 * time.Second

// phantomRun 判定幻影 run:DB 非终态(queued/planning/running)且无活 worker,
// 且不在注册窗口内。awaiting_confirmation / awaiting_continuation 是按设计
// 挂起(worker 已退出,等 Confirm/Reject/Continue 重新 launch),不算幻影。
func phantomRun(r runRecord, live bool) bool {
	if terminal(r.View.Status) {
		return false
	}
	switch r.View.Status {
	case Queued:
		return !live && time.Since(r.View.CreatedAt) > freshQueuedTol
	case Planning, Running:
		return !live
	default:
		return false
	}
}

// projectRun 在 HTTP 发射边界把幻影 run 只读降级:queued/planning →
// Interrupted;running 按 calls 分类(存在已派发未归档或 Unknown 的调用 →
// Unknown,效果不确定;否则 Interrupted),与持久收敛同语义。返回投影后的
// run 与是否幻影(调用方据此同步投影该 run 的 calls)。调用读取失败时保守
// 返回原状,不虚构终态。q 必须是调用方既有的 queryer(同事务/同连接)。
func (s *Service) projectRun(ctx context.Context, q querier, r runRecord) (runRecord, bool) {
	if !phantomRun(r, s.jobLive(r.View.ID)) {
		return r, false
	}
	status := Interrupted
	if r.View.Status == Running {
		calls, err := s.calls(ctx, q, r.View.SessionID, r.View.ID)
		if err != nil {
			return r, false
		}
		for _, c := range calls {
			if c.Body.View.Status == Unknown || (c.Body.View.Status == Running && c.Body.Dispatched) {
				status = Unknown
				break
			}
		}
	}
	r.View.Status = status
	return r, true
}

// projectRunCalls 在 run 已判定为幻影时同步投影其调用视图:已派发未归档 →
// Unknown(效果不确定),其余非终态 → Interrupted,与 run 终态一致;
// 非幻影时原样返回。与 recoverRunTx 的持久改写同语义,但只改内存视图。
func projectRunCalls(phantom bool, calls []callRecord) []callRecord {
	if !phantom {
		return calls
	}
	for i := range calls {
		if calls[i].Body.View.Status == Running && calls[i].Body.Dispatched {
			calls[i].Body.View.Status = Unknown
		} else if !terminal(calls[i].Body.View.Status) {
			calls[i].Body.View.Status = Interrupted
		}
	}
	return calls
}

// projectSessionActive 输出会话视图:active_run 指向幻影 run 时在视图里清空
// ActiveRunID——「会话忙」的真值是活 job 或合法挂起,不是 DB 残留指针。
func (s *Service) projectSessionActive(ctx context.Context, q querier, sr sessionRecord) Session {
	view := s.publicSession(sr.View)
	if view.ActiveRunID == "" {
		return view
	}
	r, err := s.run(ctx, q, sr.View.ID, view.ActiveRunID)
	if err == nil && phantomRun(r, s.jobLive(r.View.ID)) {
		view.ActiveRunID = ""
	}
	return view
}

// sessionBusy 判定会话是否真有进行中的轮:活 job(内存权威)、合法挂起
// (awaiting_*,worker 按设计退出等人工动作)、或刚入队尚在 launch 注册窗口
// 的 queued run。DB active_run 指向幻影 run 时返回 false——调用方可安全接管
// (enqueue 会在同一事务内用 recoverRunTx 收敛幻影后覆盖指针)。
func (s *Service) sessionBusy(ctx context.Context, q querier, sr sessionRecord) (bool, error) {
	if sr.View.ActiveRunID == "" {
		return false, nil
	}
	if s.jobLive(sr.View.ActiveRunID) {
		return true, nil
	}
	r, err := s.run(ctx, q, sr.View.ID, sr.View.ActiveRunID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil // 悬空指针(行已不存在),按不忙处理
		}
		return true, err
	}
	return !phantomRun(r, false), nil
}

// recoverRunTx 把单个幻影 run 持久收敛为终态:已派发未归档的调用 → Unknown
// (效果不确定),其余非终态调用 → Interrupted,run 终态随之 Unknown/
// Interrupted,finishTx 清 active_run 并发出 run_status 终态事件。仅对账本地
// 事实,绝不重放模型/SSH/已消费变更。调用方须已确认该 run 无活 worker。
func (s *Service) recoverRunTx(ctx context.Context, tx *sql.Tx, r runRecord) error {
	calls, err := s.calls(ctx, tx, r.View.SessionID, r.View.ID)
	if err != nil {
		return err
	}
	status := Interrupted
	for _, c := range calls {
		if c.Body.View.Status == Unknown {
			status = Unknown
		}
		if c.Body.View.Status == Running && c.Body.Dispatched {
			c.Body.View.Status = Unknown
			status = Unknown
		} else if !terminal(c.Body.View.Status) {
			c.Body.View.Status = Interrupted
		}
		if err = s.saveCall(ctx, tx, c); err != nil {
			return err
		}
	}
	return s.finishTx(ctx, tx, r, status)
}

// sessionUsage 在**既有 queryer**(同事务/同连接;SQLite 下另开连接会锁死)
// 上取会话最近一条带实测 usage 的 assistant 回复并折算占用(最多回看 5 条;
// 无实测时返回 nil,前端隐藏占用条)。
// sessionUsage 的 policy 必须由调用方在**事务外**获取后传入:contextSrc 可能
// 同步读库(如 aiContextPolicy→ai.load 走连接池),在持有只读事务时调用会等
// 待被自己占用的唯一连接,自死锁并冻结整个后端(单连接 SQLite)。
func (s *Service) sessionUsage(ctx context.Context, q querier, sessionID string, policy ContextPolicy) *ContextUsage {
	queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	rows, err := q.QueryContext(queryCtx,
		`SELECT body FROM ops_chat_entries WHERE session_id=? AND kind='assistant' AND context_allowed=1 ORDER BY seq DESC LIMIT 5`, sessionID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var usage *AgentUsage
	for rows.Next() {
		var b []byte
		if rows.Scan(&b) != nil {
			return nil
		}
		var body struct {
			Usage *AgentUsage
		}
		if s.open(b, &body) != nil {
			return nil
		}
		if body.Usage != nil && body.Usage.InputTokens > 0 {
			usage = body.Usage
			break
		}
	}
	if usage == nil {
		return nil
	}
	window := policy.WindowTokens
	used := usage.InputTokens + usage.OutputTokens
	percent := 0
	if window > 0 {
		percent = (used*100 + window - 1) / window
	}
	return &ContextUsage{UsedTokens: used, WindowTokens: window, Percent: percent}
}

func (s *Service) publicEntry(e Entry) Entry {
	e.Text = s.masker.ScrubTruncated(e.Text)
	if e.TargetNames != nil {
		names := map[string]string{}
		for alias, name := range e.TargetNames {
			if validTargetAlias(alias) && s.masker.Scrub(alias) == alias {
				if name = s.scrub(name, 1024); name != "" {
					names[alias] = name
				}
			}
		}
		e.TargetNames = names
	}
	if e.Summary != nil {
		copy := &RunSummary{Servers: append([]ServerSummary(nil), e.Summary.Servers...)}
		for i := range copy.Servers {
			server := &copy.Servers[i]
			server.ServerName = s.scrub(server.ServerName, 256)
			server.Facts = append([]CallFact(nil), server.Facts...)
			for j := range server.Facts {
				f := &server.Facts[j]
				v := s.publicCall(Call{Output: f.Excerpt, Error: f.Error, Resources: f.Resources})
				f.Excerpt, f.Error, f.Resources = v.Output, v.Error, v.Resources
				f.Occupants = append([]ResourceOccupant(nil), f.Occupants...)
				for k := range f.Occupants {
					o := &f.Occupants[k]
					o.ID = s.scrub(o.ID, 128)
					o.Name = s.scrub(o.Name, 128)
					o.MemoryUsage = s.scrub(o.MemoryUsage, 128)
				}
			}
		}
		e.Summary = copy
	}
	return e
}

// Require the exact projected values: escapes and masked prefixes must not hide parameters.
func (s *Service) secretArgs(a ToolArgs) bool {
	for _, value := range a.Argv {
		if s.scrub(value, 2048) != value {
			return true
		}
	}
	for _, value := range []string{a.Container, a.Unit, a.Action, a.Path, a.Ref, a.Branch, a.RunID, a.Status} {
		if s.scrub(value, 1024) != value {
			return true
		}
	}
	for key, value := range a.Params {
		if s.scrub(key, 256) != key || s.scrub(value, 1024) != value {
			return true
		}
	}
	return false
}

func (s *Service) publicCall(c Call) Call {
	c.Args.Argv = append([]string(nil), c.Args.Argv...)
	for i := range c.Args.Argv {
		c.Args.Argv[i] = s.scrub(c.Args.Argv[i], 2048)
	}
	c.Args.Container = s.scrub(c.Args.Container, 1024)
	c.Args.Unit = s.scrub(c.Args.Unit, 1024)
	c.Args.Action = s.scrub(c.Args.Action, 1024)
	c.Args.Path = s.scrub(c.Args.Path, 1024)
	c.Args.Ref = s.scrub(c.Args.Ref, 1024)
	c.Args.Branch = s.scrub(c.Args.Branch, 1024)
	c.Args.RunID = s.scrub(c.Args.RunID, 1024)
	c.Args.Status = s.scrub(c.Args.Status, 1024)
	if c.Args.Params != nil {
		params := map[string]string{}
		for k, v := range c.Args.Params {
			params[s.scrub(k, 256)] = s.scrub(v, 1024)
		}
		c.Args.Params = params
	}
	c.Object = s.scrub(c.Object, 1024)
	c.ServerName = s.scrub(c.ServerName, 1024)
	c.Output = s.scrub(c.Output, 64<<10)
	c.Error = s.scrub(c.Error, 1024)
	if c.Resources != nil {
		r := *c.Resources
		r.Disks = append([]DiskResource(nil), r.Disks...)
		for i := range r.Disks {
			r.Disks[i].Filesystem = s.scrub(r.Disks[i].Filesystem, 1024)
			r.Disks[i].Mount = s.scrub(r.Disks[i].Mount, 1024)
		}
		c.Resources = &r
	}
	return c
}

func (s *Service) publicConfirmation(c Confirmation) Confirmation {
	c.Calls = append([]BoundCall(nil), c.Calls...)
	for i := range c.Calls {
		c.Calls[i].Object = s.scrub(c.Calls[i].Object, 1024)
	}
	return c
}
