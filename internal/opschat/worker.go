package opschat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/target"
)

const leaseDuration = 15 * time.Second

func (s *Service) Start(root context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.closing {
		return ErrConflict
	}
	waitCtx, cancel := context.WithTimeout(root, leaseDuration+500*time.Millisecond)
	defer cancel()
	var seen leaseObservation
	for {
		err := s.acquireLease(waitCtx, &seen)
		if err == nil {
			break
		}
		if root.Err() != nil {
			return root.Err()
		}
		if err != ErrLease || seen.renewed || waitCtx.Err() != nil {
			return err
		}
		select {
		case <-waitCtx.Done():
			if root.Err() != nil {
				return root.Err()
			}
			return ErrLease
		case <-time.After(100 * time.Millisecond):
		}
	}
	s.base = root
	s.root, s.stop = context.WithCancel(root)
	s.leasedUntil = time.Now().Add(leaseDuration).UnixMilli()
	s.started = true
	s.leaseDone = make(chan struct{})
	s.loopDone = make(chan struct{})
	go s.leaseLoop(s.loopDone)
	log.Printf("[opschat] started instance=%s", s.instance)
	return nil
}

type leaseObservation struct {
	owner   string
	until   int64
	renewed bool
}

func (s *Service) acquireLease(root context.Context, seen *leaseObservation) error {
	return s.transaction(root, false, func(tx *sql.Tx) error {
		if _, e := tx.ExecContext(root, "UPDATE ops_chat_state SET lease_until=lease_until WHERE id=1"); e != nil {
			return ErrStorage
		}
		var owner string
		var until int64
		if tx.QueryRowContext(root, "SELECT lease_owner,lease_until FROM ops_chat_state WHERE id=1").Scan(&owner, &until) != nil {
			return ErrStorage
		}
		if owner != "" && until > time.Now().UnixMilli() {
			if seen.owner != "" && (seen.owner != owner || until > seen.until) {
				seen.renewed = true
			}
			seen.owner, seen.until = owner, until
			return ErrLease
		}
		// Validate every encrypted record before modifying recovery state.
		for _, table := range []string{"ops_chat_sessions", "ops_chat_entries", "ops_chat_runs", "ops_chat_calls", "ops_chat_approvals"} {
			rows, e := tx.QueryContext(root, "SELECT body FROM "+table)
			if e != nil {
				return ErrStorage
			}
			for rows.Next() {
				var b []byte
				if rows.Scan(&b) != nil {
					rows.Close()
					return ErrStorage
				}
				var plain any
				if e = s.open(b, &plain); e != nil {
					rows.Close()
					return e
				}
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return ErrStorage
			}
		}
		recovered, e := s.recoverActiveRunsTx(root, tx)
		if e != nil {
			return e
		}
		if recovered > 0 {
			log.Printf("[opschat] lease acquired: recovered %d interrupted/unknown run(s)", recovered)
		}
		_, e = tx.ExecContext(root, "UPDATE ops_chat_state SET lease_owner=?,lease_until=? WHERE id=1", s.instance, time.Now().Add(leaseDuration).UnixMilli())
		return databaseError(e)
	})
}

// recoverActiveRunsTx 把仍处于活动态(queued/planning/running)的 run 收敛为
// Interrupted/Unknown:已派发的调用效果不确定,标记 Unknown;未派发的标记
// Interrupted。仅本地对账,绝不重放模型/SSH/已消费的变更。启动获取与运行期
// 重取租约共用同一恢复语义。返回收敛的 run 数。
func (s *Service) recoverActiveRunsTx(root context.Context, tx *sql.Tx) (int, error) {
	rows, e := tx.QueryContext(root, `SELECT session_id,id FROM ops_chat_runs WHERE status IN ('queued','planning','running')`)
	if e != nil {
		return 0, ErrStorage
	}
	ids := [][2]string{}
	for rows.Next() {
		var pair [2]string
		if rows.Scan(&pair[0], &pair[1]) != nil {
			rows.Close()
			return 0, ErrStorage
		}
		ids = append(ids, pair)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return 0, ErrStorage
	}
	for _, pair := range ids {
		r, e := s.run(root, tx, pair[0], pair[1])
		if e != nil {
			return 0, e
		}
		calls, e := s.calls(root, tx, pair[0], pair[1])
		if e != nil {
			return 0, e
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
			if e = s.saveCall(root, tx, c); e != nil {
				return 0, e
			}
		}
		if e = s.finishTx(root, tx, r, status); e != nil {
			return 0, e
		}
	}
	return len(ids), nil
}

// beginCall commits the dispatch intent before even a read-only resolver reaches SSH.
func (s *Service) beginCall(ctx context.Context, c *callRecord) error {
	if ctx.Err() != nil {
		return ErrConflict
	}
	v := &c.Body.View
	return s.persist(v.SessionID, func(cc context.Context, tx *sql.Tx) error {
		r, e := s.run(cc, tx, v.SessionID, v.RunID)
		if e != nil {
			return e
		}
		if r.View.CancelRequested || terminal(r.View.Status) {
			return ErrConflict
		}
		var status string
		if tx.QueryRowContext(cc, "SELECT status FROM ops_chat_calls WHERE id=? AND session_id=? AND run_id=?", v.ID, v.SessionID, v.RunID).Scan(&status) != nil {
			return ErrStorage
		}
		if status != Queued {
			return ErrConflict
		}
		v.Status = Running
		c.Body.Dispatched = false
		if e = s.saveCall(cc, tx, *c); e != nil {
			return e
		}
		return s.append(cc, tx, Entry{SessionID: v.SessionID, RunID: v.RunID, CallID: v.ID, Kind: "call_status", Status: Running}, false, false)
	})
}

// leaseLoop 每秒维护一次执行器租约:持有时续期(renewLease),失去后在租约
// 空窗期 CAS 重取(tryReacquire)。失租不再是终态——旧实现里续期被业务流量
// 挤超时约 15s 后,自己的租约自然过期会被误判为"被其他实例接管"而永久自杀,
// 只能重启进程;现在 fence 之后循环继续跑,空窗一到自动恢复服务。
// 循环只在 Close(close loopDone)时退出。
// loopDone 由 Start 以参数传入(Close 会把字段置 nil,循环内不得再读字段)。
func (s *Service) leaseLoop(loopDone chan struct{}) {
	defer close(s.leaseDone)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-loopDone:
			return
		case <-ticker.C:
			s.maintainLease()
		}
	}
}

func (s *Service) maintainLease() {
	s.mu.Lock()
	if s.closing || !s.started {
		s.mu.Unlock()
		return
	}
	lost := s.lost
	s.mu.Unlock()
	if lost {
		s.tryReacquire()
		return
	}
	s.renewLease()
}

// renewLease 单语句 CAS 续期(隐式事务,比旧的 6 步显式事务在 SQLite 唯一
// 连接上的排队时间短得多):仅当租约仍归本实例且未过期才顺延。未生效则交给
// auditLease 读租约行,区分瞬时存储失败、自己过期(续期被饿死)与被接管。
func (s *Service) renewLease() {
	now := time.Now().UnixMilli()
	until := time.Now().Add(leaseDuration).UnixMilli()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	res, err := s.db.ExecContext(ctx, "UPDATE ops_chat_state SET lease_until=? WHERE id=1 AND lease_owner=? AND lease_until>?", until, s.instance, now)
	cancel()
	if err == nil {
		if n, _ := res.RowsAffected(); n == 1 {
			s.leasedUntil = until
			s.leaseFails = 0
			return
		}
	}
	s.auditLease(err)
}

// auditLease 判定续期未生效后的态势:租约仍归本实例且未过期 → 瞬时存储失败,
// 记录后继续按 tick 重试;自己过期(续期被饿死)或被其他实例接管 → fence。
// 连租约行都读不出时,以内存中最后一次成功续期的 leasedUntil 兜底:过了那个
// 点别的实例可能合法接管,必须按失租处理。
func (s *Service) auditLease(renewErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	var owner string
	var until int64
	readErr := s.db.QueryRowContext(ctx, "SELECT lease_owner,lease_until FROM ops_chat_state WHERE id=1").Scan(&owner, &until)
	cancel()
	if readErr == nil {
		now := time.Now().UnixMilli()
		switch {
		case owner == s.instance && until > now:
			s.noteLeaseFailure(renewErr)
		case owner == s.instance:
			s.fence("own lease expired after renewal starvation")
		default:
			s.fence("lease taken over by another instance")
		}
		return
	}
	s.noteLeaseFailure(readErr)
	if time.Now().UnixMilli() > s.leasedUntil {
		s.fence("lease expired amid storage failure")
	}
}

// noteLeaseFailure 记录续期/重取的连续失败并节流日志(前 3 次全打,之后每
// 10 次一条)。底层驱动错误原样透出——旧实现除一处外全吞成裸 ErrStorage,
// 线上只剩 "local persistence failed",无法定位。仅 leaseLoop 调用,计数无锁。
// err 为 nil 表示 CAS 未生效(租约已不按预期持有),以 ErrLease 记入日志。
func (s *Service) noteLeaseFailure(err error) {
	if err == nil {
		err = ErrLease
	}
	s.leaseFails++
	if s.leaseFails <= 3 || s.leaseFails%10 == 0 {
		log.Printf("[opschat] lease maintenance failed (%d): %v", s.leaseFails, err)
	}
}

// fence 把服务切到失租不可用态(幂等、可恢复):取消在途 jobs 的工作上下文,
// 后续写操作在事务预检处得到 ErrLease,capabilities 报 unavailable。leaseLoop
// 会在租约空窗期自动重取并重建工作上下文恢复服务,无需进程重启。
func (s *Service) fence(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lost || !s.started || s.closing {
		return
	}
	s.lost = true
	if s.stop != nil {
		s.stop()
		s.stop = nil
		s.root = nil
	}
	log.Printf("[opschat] lease lost (%s); fenced until reacquired", reason)
}

// tryReacquire 失租态下尝试夺回租约:CAS 条件要求行内租约为空或已过期——
// 别的实例持有且未过期时绝不去抢,等下个 tick。成功则在同一事务内把活动态
// run 收敛为 Interrupted/Unknown(与启动恢复同语义,不重放远端动作),随后
// 重建工作上下文恢复服务。
func (s *Service) tryReacquire() {
	now := time.Now().UnixMilli()
	until := time.Now().Add(leaseDuration).UnixMilli()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	acquired := false
	recovered := 0
	err := s.transaction(ctx, false, func(tx *sql.Tx) error {
		res, e := tx.ExecContext(ctx, "UPDATE ops_chat_state SET lease_owner=?,lease_until=? WHERE id=1 AND (lease_owner='' OR lease_until<=?)", s.instance, until, now)
		if e != nil {
			// 底层驱动错误(SQLite busy/IO 等)原样进日志,否则只剩静态
			// ErrStorage 文案,线上无法定位;errors.Is 语义不受影响。
			return fmt.Errorf("%w: %v", ErrStorage, e)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		acquired = true
		recovered, e = s.recoverActiveRunsTx(ctx, tx)
		return e
	})
	if err != nil {
		s.noteLeaseFailure(err)
		return
	}
	if !acquired {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || !s.started {
		return
	}
	s.root, s.stop = context.WithCancel(s.base)
	s.leasedUntil = until
	s.lost = false
	s.leaseFails = 0
	if recovered > 0 {
		log.Printf("[opschat] lease reacquired: service resumed, recovered %d interrupted/unknown run(s)", recovered)
	} else {
		log.Printf("[opschat] lease reacquired: service resumed")
	}
}
func (s *Service) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	if s.loopDone != nil {
		close(s.loopDone)
		s.loopDone = nil
	}
	if s.stop != nil {
		s.stop()
	}
	jobs := []*job{}
	for _, j := range s.jobs {
		j.cancel()
		jobs = append(jobs, j)
	}
	for id, subs := range s.wake {
		for ch := range subs {
			close(ch)
		}
		delete(s.wake, id)
	}
	leaseDone := s.leaseDone
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, j := range jobs {
		select {
		case <-j.done:
		case <-ctx.Done():
			return ErrConflict
		}
	}
	if leaseDone != nil {
		select {
		case <-leaseDone:
		case <-ctx.Done():
			return ErrConflict
		}
	}
	_, err := s.db.ExecContext(ctx, "UPDATE ops_chat_state SET lease_owner='',lease_until=0 WHERE id=1 AND lease_owner=?", s.instance)
	return databaseError(err)
}
func (s *Service) launch(id, run string) {
	s.mu.Lock()
	if !s.started || s.closing || s.lost {
		s.mu.Unlock()
		return
	}
	if existing := s.jobs[run]; existing != nil {
		existing.retry = true
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(s.root)
	j := &job{session: id, cancel: cancel, done: make(chan struct{})}
	s.jobs[run] = j
	s.mu.Unlock()
	go func() {
		defer cancel()
		for {
			s.work(ctx, id, run)
			s.mu.Lock()
			if j.retry && ctx.Err() == nil && !s.closing && !s.lost {
				j.retry = false
				s.mu.Unlock()
				continue
			}
			delete(s.jobs, run)
			close(j.done)
			s.mu.Unlock()
			return
		}
	}()
}
func (s *Service) persist(id string, fn func(context.Context, *sql.Tx) error) error {
	return s.persistAttempts(id, fn, 3)
}

func (s *Service) persistAttempts(id string, fn func(context.Context, *sql.Tx) error, attempts int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		err = s.transaction(ctx, true, func(tx *sql.Tx) error { return fn(ctx, tx) })
		if err != ErrStorage || ctx.Err() != nil || attempt == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
		if ctx.Err() != nil {
			break
		}
	}
	if err == ErrLease {
		s.fence("lease check rejected persist")
	}
	if err == nil {
		s.notify(id)
	}
	return err
}
func (s *Service) failRun(id, run, status string) {
	if err := s.convergeRun(id, run, status, false); err != nil {
		s.fenceWorker()
	}
}

// failFrom converges a run after a worker error: cancellation and conflict
// races (cancel requested, concurrent state change) resolve to Interrupted,
// matching Cancel semantics; genuine execution failures resolve to Failed.
func (s *Service) failFrom(id, run string, err error, ctx context.Context) {
	if errors.Is(err, ErrConflict) || ctx.Err() != nil {
		s.failRun(id, run, Interrupted)
		return
	}
	s.failRun(id, run, Failed)
}

func (s *Service) planningFailure(id, run, status string, reason error) {
	text := strings.Replace(analysisFailureText(reason), "[analysis:", "[planning:", 1)
	if errors.Is(reason, ErrInvalid) {
		text = "[planning:invalid_plan]"
		// 上游校验失败的具体原因直接透出给用户(已脱敏、限长)。
		detail := s.scrub(strings.TrimPrefix(reason.Error(), ErrInvalid.Error()), 512)
		if trimmed := strings.TrimSpace(strings.TrimPrefix(detail, ":")); trimmed != "" {
			text += " " + trimmed
		}
	}
	// 详细诊断:服务端日志 + 可见标记附带 HTTP 状态与 provider 错误原文
	// (错误字符串形如 "ai: ops model rejected (HTTP 400): <provider detail>",已脱敏)。
	if reason != nil {
		log.Printf("[opschat] planning failed session=%s run=%s reason=%v", id, run, reason)
	}
	msg := ""
	if reason != nil {
		msg = reason.Error()
	}
	if i := strings.Index(msg, "(HTTP "); i >= 0 {
		if detail := msg[min(i+7, len(msg)):]; strings.Contains(detail, ": ") {
			text += " " + msg[i:] // 含 "(HTTP n): detail"
		} else if j := strings.Index(msg[i:], ")"); j >= 0 {
			text += " " + msg[i:i+j+1] // 仅 "(HTTP n)"
		}
	}
	if err := s.persistAttempts(id, func(ctx context.Context, tx *sql.Tx) error {
		r, e := s.run(ctx, tx, id, run)
		if e != nil || terminal(r.View.Status) {
			return e
		}
		if r.View.CancelRequested {
			return s.finishTx(ctx, tx, r, Interrupted)
		}
		if e = s.append(ctx, tx, Entry{SessionID: id, RunID: run, Kind: "assistant", Text: text, Status: Failed}, true, false); e != nil {
			return e
		}
		return s.finishTx(ctx, tx, r, status)
	}, 50); err != nil {
		s.fenceWorker()
	}
}

// retryCalls 把 RetryFailed 预计算的动作展开为待执行调用(不经过模型)。
func (s *Service) retryCalls(r runRecord) ([]callRecord, error) {
	candidates := []callRecord{}
	for _, a := range r.Body.RetryActions {
		args, e := parseArgs(a.ToolID, a.Args)
		if e != nil || s.secretArgs(args) || len(a.TargetIndexes) == 0 {
			return nil, ErrInvalid
		}
		for _, index := range a.TargetIndexes {
			if index < 0 || index >= len(r.Body.Targets) {
				return nil, ErrInvalid
			}
			snap := r.Body.Targets[index]
			object := args.Container
			if object == "" {
				object = args.Unit
			}
			view := Call{ID: uuid.NewString(), RunID: r.View.ID, SessionID: r.View.SessionID, ServerID: snap.Server.ID, ServerName: s.scrub(snap.Server.Name, 1024), ToolID: a.ToolID, Args: args, Object: object, Status: Queued, ArgsHash: digest(args), TargetHash: snap.Fingerprint()}
			candidates = append(candidates, callRecord{Body: callBody{View: view, Snapshot: snap}, Created: time.Now().UnixMilli(), Ordinal: len(candidates)})
			if len(candidates) > 24 {
				return nil, ErrInvalid
			}
		}
	}
	return candidates, nil
}

// fenceWorker 在 worker 持久化彻底失败时熔断本实例,等价于失租;leaseLoop
// 会在租约空窗期自动重取恢复,不再是只能重启的终态。
func (s *Service) fenceWorker() {
	s.fence("worker persistence failed")
}

// Only reconcile local facts. Never retry model, SSH, or consumed mutations.
func (s *Service) convergeRun(id, run, status string, onlyWorking bool) error {
	err := s.persistAttempts(id, func(ctx context.Context, tx *sql.Tx) error {
		r, e := s.run(ctx, tx, id, run)
		if e != nil {
			return e
		}
		if terminal(r.View.Status) {
			return nil
		}
		// A confirmation may have queued a fresh phase while this worker exits.
		if onlyWorking && r.View.Status != Planning && r.View.Status != Running {
			return nil
		}
		calls, e := s.calls(ctx, tx, id, run)
		if e != nil {
			return e
		}
		successes := 0
		uncertain := false
		for _, c := range calls {
			if c.Body.View.Status == Unknown {
				uncertain = true
			}
			if c.Body.View.Status == Running && c.Body.Dispatched {
				c.Body.View.Status = Unknown
				uncertain = true
			}
			if c.Body.View.Status == Succeeded {
				successes++
			}
			if !terminal(c.Body.View.Status) {
				c.Body.View.Status = Interrupted
			}
			if e = s.saveCall(ctx, tx, c); e != nil {
				return e
			}
		}
		if uncertain {
			status = Unknown
		} else if successes > 0 && len(calls) > successes {
			status = PartialFailed
		}
		return s.finishTx(ctx, tx, r, status)
	}, 50)
	if err == nil {
		log.Printf("[opschat] run converged session=%s run=%s status=%s", id, run, status)
	}
	return err
}
func (s *Service) work(parent context.Context, id, run string) {
	defer func() {
		if err := s.convergeRun(id, run, Failed, true); err != nil {
			s.fenceWorker()
		}
	}()
	r, err := s.run(parent, s.db, id, run)
	if err != nil {
		s.failRun(id, run, Interrupted)
		return
	}
	if r.View.Status != Queued {
		return
	}
	remaining := s.runtime.WorkBudget - time.Duration(r.Body.ActiveMillis)*time.Millisecond
	if remaining <= 0 {
		s.failRun(id, run, Interrupted)
		return
	}
	ctx, cancel := context.WithTimeout(parent, remaining)
	defer cancel()
	began := time.Now()
	defer func() {
		if ctx.Err() != nil {
			s.failRun(id, run, Interrupted)
		}
	}()
	err = s.persist(id, func(c context.Context, tx *sql.Tx) error {
		current, e := s.run(c, tx, id, run)
		if e != nil {
			return e
		}
		if current.View.Status != Queued || current.View.CancelRequested {
			return ErrConflict
		}
		current.View.Status = Planning
		if e = s.saveRun(c, tx, current); e != nil {
			return e
		}
		return s.append(c, tx, Entry{SessionID: id, RunID: run, Kind: "run_status", Status: Planning}, false, false)
	})
	if err != nil {
		s.failRun(id, run, Interrupted)
		return
	}
	if err = s.registerSecrets(); err != nil {
		s.failRun(id, run, Failed)
		return
	}
	if r.Body.Analysis != nil {
		s.analyzeWork(ctx, id, run, r)
		return
	}
	calls, err := s.calls(ctx, s.db, id, run)
	if err != nil {
		s.failRun(id, run, Failed)
		return
	}
	lastReplyText := ""
	var lastReplyUsage *AgentUsage
	if len(calls) == 0 {
		var candidates []callRecord
		if len(r.Body.RetryActions) > 0 {
			var e error
			candidates, e = s.retryCalls(r)
			if e != nil {
				s.planningFailure(id, run, Failed, e)
				return
			}
		} else {
			var e error
			firstReply, cand, e := s.requestRound(ctx, r, false)
			if e != nil {
				status := Failed
				if ctx.Err() != nil {
					status = Interrupted
				}
				s.planningFailure(id, run, status, e)
				return
			}
			candidates = cand
			lastReplyText = firstReply.Text
			lastReplyUsage = firstReply.Usage
		}
		// Group only wholly read-only plans; mutation barriers retain their order.
		readOnly := true
		for _, c := range candidates {
			readOnly = readOnly && !mutatesTool(c.Body.View.ToolID)
		}
		if readOnly {
			order := map[string]int{}
			for i, snap := range r.Body.Targets {
				order[snap.Server.ID] = i
			}
			sort.SliceStable(candidates, func(i, j int) bool {
				return order[candidates[i].Body.View.ServerID] < order[candidates[j].Body.View.ServerID]
			})
			for i := range candidates {
				candidates[i].Ordinal = i
			}
		}
		finalText := len(candidates) == 0
		err = s.persist(id, func(c context.Context, tx *sql.Tx) error {
			current, e := s.run(c, tx, id, run)
			if e != nil {
				return e
			}
			if current.View.CancelRequested {
				return ErrConflict
			}
			if finalText && lastReplyText != "" {
				if e = s.append(c, tx, Entry{SessionID: id, RunID: run, Kind: "assistant", Text: s.scrub(lastReplyText, 32<<10), Status: Succeeded, Usage: lastReplyUsage}, true, true); e != nil {
					return e
				}
			}
			for _, candidate := range candidates {
				b, e := s.seal(candidate.Body)
				if e != nil {
					return e
				}
				v := candidate.Body.View
				_, e = tx.ExecContext(c, `INSERT INTO ops_chat_calls(id,session_id,run_id,server_id,tool_id,status,args_hash,target_hash,body,created_at,ordinal) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, v.ID, id, run, v.ServerID, v.ToolID, v.Status, v.ArgsHash, v.TargetHash, b, candidate.Created, candidate.Ordinal)
				if e != nil {
					return ErrStorage
				}
				if e = s.append(c, tx, Entry{SessionID: id, RunID: run, CallID: v.ID, Kind: "tool_call"}, true, false); e != nil {
					return e
				}
			}
			current.View.Status = Running
			if e = s.saveRun(c, tx, current); e != nil {
				return e
			}
			return s.append(c, tx, Entry{SessionID: id, RunID: run, Kind: "run_status", Status: Running}, false, false)
		})
		if err != nil {
			s.failFrom(id, run, err, ctx)
			return
		}
		calls = candidates
	}
	// A confirmation replay skips the planning block above, which owns the only
	// Running transition; publish it here so execution never shows as planning.
	err = s.persist(id, func(c context.Context, tx *sql.Tx) error {
		current, e := s.run(c, tx, id, run)
		if e != nil {
			return e
		}
		if current.View.Status != Planning {
			return nil
		}
		if current.View.CancelRequested {
			return ErrConflict
		}
		current.View.Status = Running
		if e = s.saveRun(c, tx, current); e != nil {
			return e
		}
		return s.append(c, tx, Entry{SessionID: id, RunID: run, Kind: "run_status", Status: Running}, false, false)
	})
	if err != nil {
		s.failFrom(id, run, err, ctx)
		return
	}
	for {
		// Calls in a run are serial; separate sessions can use the four global SSH slots.
		barrier := false
		for _, call := range calls {
			if ctx.Err() != nil {
				break
			}
			if call.Body.View.Status == AwaitingConfirmation {
				barrier = true
				continue
			}
			if terminal(call.Body.View.Status) {
				continue
			}
			if needsApproval(call.Body.View) {
				a, e := s.approval(ctx, s.db, id, run)
				// call 级绑定:确认单消费后只放行其 BoundCall 列表内的调用。
				// 续轮新提议的 mutation 不在列表内 → 重新挂起新确认卡,而非被旧单误放行。
				approved := e == nil && a.Status == "consumed" && a.Instance == s.instance && a.View.ExpiresAt.After(time.Now())
				if approved {
					want := bound(call.Body.View)
					listed := false
					approved = false
					for _, bc := range a.Body.Calls {
						if bc.CallID != want.CallID {
							continue
						}
						listed = true
						if bc == want {
							approved = true
						}
					}
					if listed && !approved {
						// 在确认单列表内但哈希不匹配:批准后参数被篡改,fail-closed。
						s.failFrom(id, run, ErrApproval, ctx)
						return
					}
				}
				if !approved {
					barrier = true
					if e = s.prepareMutation(ctx, call); e != nil {
						s.failFrom(id, run, e, ctx)
						return
					}
					continue
				}
			}
			// Only mutation object resolution may cross the confirmation barrier.
			if barrier {
				continue
			}
			callStart := time.Now()
			if err = s.executeCall(ctx, call); err != nil {
				s.failFrom(id, run, err, ctx)
				return
			}
			log.Printf("[opschat] call done session=%s run=%s call=%s tool=%s server=%s status=%s elapsed=%s",
				id, run, call.Body.View.ID, call.Body.View.ToolID, call.Body.View.ServerID, call.Body.View.Status, time.Since(callStart).Round(time.Millisecond))
		}
		if ctx.Err() != nil {
			return
		}
		current, e := s.run(ctx, s.db, id, run)
		if e != nil {
			s.failRun(id, run, Failed)
			return
		}
		if current.View.CancelRequested || terminal(current.View.Status) {
			break
		}
		cs, e := s.calls(ctx, s.db, id, run)
		if e != nil {
			s.failRun(id, run, Failed)
			return
		}
		settled := len(cs) > 0
		pendingConfirm := false
		for _, c := range cs {
			if !terminal(c.Body.View.Status) {
				settled = false
			}
			if c.Body.View.Status == AwaitingConfirmation {
				pendingConfirm = true
			}
		}
		if !settled || pendingConfirm {
			break
		}
		budget := current.Body.RoundBudget
		if budget <= 0 {
			budget = s.runtime.MaxAgentRounds
		}
		wrapUp := current.Body.Rounds >= budget
		reply, next, e := s.requestRound(ctx, current, wrapUp)
		if e == nil {
			e = s.publishRound(ctx, current, reply, next, wrapUp)
		}
		if e != nil {
			status := Failed
			if ctx.Err() != nil {
				status = Interrupted
			}
			s.planningFailure(id, run, status, e)
			return
		}
		if len(next) == 0 {
			if wrapUp {
				// 收尾轮:publishRound 已置 awaiting_continuation(挂起等用户),不收敛。
				return
			}
			// 终答:文本已落 assistant entry,break 落 phase-five 终态收敛(summary/finishTx)。
			break
		}
		calls = next
	}
	auto := false
	toolStatus := Succeeded
	err = s.persist(id, func(c context.Context, tx *sql.Tx) error {
		current, e := s.run(c, tx, id, run)
		if e != nil {
			return e
		}
		if terminal(current.View.Status) {
			return nil
		}
		current.Body.ActiveMillis += time.Since(began).Milliseconds()
		cs, e := s.calls(c, tx, id, run)
		if e != nil {
			return e
		}
		pending := false
		for _, call := range cs {
			pending = pending || call.Body.View.Status == AwaitingConfirmation
		}
		successes, failures, unknowns := 0, 0, 0
		for _, call := range cs {
			if call.Body.View.Status == Running {
				call.Body.View.Status = Interrupted
				if call.Body.Dispatched {
					call.Body.View.Status = Unknown
				}
				if e = s.saveCall(c, tx, call); e != nil {
					return e
				}
			}
			if call.Body.View.Status == Queued && !pending {
				call.Body.View.Status = Interrupted
				if e = s.saveCall(c, tx, call); e != nil {
					return e
				}
			}
			switch call.Body.View.Status {
			case AwaitingConfirmation:
				pending = true
			case Succeeded:
				successes++
			case PartialFailed:
				successes++
				failures++
			case Unknown:
				unknowns++
			default:
				failures++
			}
		}
		if pending {
			if e = s.issueApproval(c, tx, id, run, cs); e != nil {
				return e
			}
			current.View.Status = AwaitingConfirmation
			if e = s.saveRun(c, tx, current); e != nil {
				return e
			}
			return s.append(c, tx, Entry{SessionID: id, RunID: run, Kind: "confirmation_ready", Status: AwaitingConfirmation}, false, false)
		}
		status := Succeeded
		if unknowns > 0 {
			status = Unknown
		} else if failures > 0 {
			status = Failed
			if successes > 0 {
				status = PartialFailed
			}
		}
		toolStatus = status
		if current.Body.AnalysisAuthorized && !current.Body.AnalysisStarted && !current.View.CancelRequested && len(cs) > 0 {
			current.Body.AnalysisStarted = true // persisted before any external call; never recovered/retried
			current.View.Status = Running
			if e = s.summaryTx(c, tx, &current); e != nil {
				return e
			}
			auto = true
			if e = s.saveRun(c, tx, current); e != nil {
				return e
			}
			return s.append(c, tx, Entry{SessionID: id, RunID: run, Kind: "run_status", Status: Running}, false, false)
		}
		return s.finishTx(c, tx, current, status)
	})
	if err == nil && auto {
		current, e := s.run(ctx, s.db, id, run)
		if e != nil {
			return
		}
		ids := []string{}
		cs, e := s.calls(ctx, s.db, id, run)
		if e != nil {
			return
		}
		for _, c := range cs {
			ids = append(ids, c.Body.View.ID)
		}
		preview, e := s.analysisPreview(ctx, id, ids, run)
		if e != nil || preview.Provider != current.Body.Provider {
			s.analysisFailure(id, run, toolStatus, e)
			return
		}
		current.Body.Analysis = preview
		s.analyzeWorkStatus(ctx, id, run, current, toolStatus)
	}
}
func (s *Service) analyzeWork(ctx context.Context, id, run string, r runRecord) {
	s.analyzeWorkStatus(ctx, id, run, r, Succeeded)
}
func (s *Service) analysisFailure(id, run, status string, reason error) {
	text := analysisFailureText(reason)
	_ = s.persist(id, func(ctx context.Context, tx *sql.Tx) error {
		r, e := s.run(ctx, tx, id, run)
		if e != nil {
			return e
		}
		if r.View.CancelRequested {
			status = Interrupted
		}
		if !r.Body.AnalysisAuthorized {
			if !r.View.CancelRequested {
				status = Failed
			}
			return s.finishTx(ctx, tx, r, status)
		}
		if e = s.append(ctx, tx, Entry{SessionID: id, RunID: run, Kind: "analysis", Text: text, Status: Failed}, true, false); e != nil {
			return e
		}
		return s.finishTx(ctx, tx, r, status)
	})
}
func (s *Service) analyzeWorkStatus(ctx context.Context, id, run string, r runRecord, finalStatus string) {
	ownRun := ""
	if r.Body.AnalysisStarted {
		ownRun = run
	}
	preview, err := s.analysisPreview(ctx, id, r.Body.Analysis.CallIDs, ownRun)
	if err != nil || preview.Hash != r.Body.Analysis.Hash || preview.Provider != r.Body.Analysis.Provider {
		s.analysisFailure(id, run, finalStatus, err)
		return
	}
	req := AnalysisRequest{Provider: preview.Provider, Items: preview.Items, Question: r.Body.Question, MaxResponseBytes: 16 << 10}
	raw, _ := json.Marshal(req)
	if len(raw) > 32<<10 {
		s.analysisFailure(id, run, finalStatus, ErrQuota)
		return
	}
	err = s.persist(id, func(c context.Context, tx *sql.Tx) error {
		current, e := s.run(c, tx, id, run)
		if e != nil {
			return e
		}
		// 分析流一次性:AnalysisStarted 已在触发前持久化,这里再以 Rounds==0 作防重放闸。
		if current.View.CancelRequested || current.Body.Rounds >= 1 {
			return ErrConflict
		}
		current.Body.Rounds++
		return s.saveRun(c, tx, current)
	})
	if err != nil {
		return
	}
	modelCtx, stop := context.WithTimeout(ctx, s.runtime.ModelTimeout)
	reply := s.reply(modelCtx, id, run, "analysis", 16<<10)
	var text string
	if streaming, ok := s.model.(StreamModel); ok {
		text, err = streaming.AnalyzeStream(modelCtx, req, reply.chunk)
	} else {
		text, err = s.model.Analyze(modelCtx, req)
	}
	if err != nil && reply.seq != 0 {
		_ = reply.finish(reply.text, Interrupted, false)
	}
	if err == nil && reply.seq != 0 {
		err = reply.finish(s.scrub(text, 16<<10), Succeeded, false)
	}
	stop()
	if err != nil || len(text) > 16<<10 || s.registerSecrets() != nil {
		s.analysisFailure(id, run, finalStatus, err)
		return
	}
	_ = s.persist(id, func(c context.Context, tx *sql.Tx) error {
		current, e := s.run(c, tx, id, run)
		if e != nil {
			return e
		}
		if current.View.CancelRequested || ctx.Err() != nil {
			return s.finishTx(c, tx, current, Interrupted)
		}
		// Derived analysis is excluded from every future planning context.
		if reply.seq == 0 {
			if e = s.append(c, tx, Entry{SessionID: id, RunID: run, Kind: "analysis", Text: s.scrub(text, 16<<10)}, true, false); e != nil {
				return e
			}
		}
		return s.finishTx(c, tx, current, finalStatus)
	})
}
func (s *Service) takeSlot(ctx context.Context, server string) (func(), error) {
	s.mu.Lock()
	slot := s.serverSlots[server]
	if slot == nil {
		slot = make(chan struct{}, 1)
		s.serverSlots[server] = slot
	}
	s.mu.Unlock()
	select {
	case slot <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		<-slot
		return nil, ctx.Err()
	}
	return func() { <-s.slots; <-slot }, nil
}

type readBudget struct {
	bytes     int
	lines     int
	truncated bool
}

func newReadBudget() *readBudget { return &readBudget{bytes: 64 << 10, lines: 1000} }
func remainingReadBudget(c callRecord) *readBudget {
	return &readBudget{bytes: max(0, (64<<10)-c.Body.ReadBytes), lines: max(0, 1000-c.Body.ReadLines), truncated: c.Body.ReadTruncated}
}
func (s *Service) bounded(ctx context.Context, c *callRecord, argv []string, b *readBudget) (*target.LimitedResult, error) {
	release, err := s.takeSlot(ctx, c.Body.Snapshot.Server.ID)
	if err != nil {
		return nil, err
	}
	defer release()
	return s.boundedInSlot(ctx, c, argv, b)
}

// The caller owns both slots across mutation and verification; never acquire recursively.
func (s *Service) boundedInSlot(ctx context.Context, c *callRecord, argv []string, b *readBudget) (*target.LimitedResult, error) {
	snap := c.Body.Snapshot
	if b.bytes < 1 || b.lines < 1 {
		b.truncated = true
		c.Body.ReadTruncated = true
		return nil, ErrQuota
	}
	if s.executor == nil {
		return nil, ErrUnavailable
	}
	if err := s.registerSecrets(); err != nil {
		return nil, err
	}
	if s.secretArgs(c.Body.View.Args) {
		return nil, ErrInvalid
	}
	// Revalidate the lease after waiting for slots, before each SSH dispatch.
	limitBytes := b.bytes
	err := s.persist(c.Body.View.SessionID, func(cc context.Context, tx *sql.Tx) error {
		r, e := s.run(cc, tx, c.Body.View.SessionID, c.Body.View.RunID)
		if e != nil {
			return e
		}
		if r.View.CancelRequested || ctx.Err() != nil || terminal(r.View.Status) {
			return ErrConflict
		}
		limitBytes = min(limitBytes, max(0, (512<<10)-r.Body.ReadBytes))
		if limitBytes < 1 {
			return ErrQuota
		}
		stored, e := s.call(cc, tx, c.Body.View.SessionID, c.Body.View.ID)
		if e != nil {
			return e
		}
		if stored.Body.View.Status != Running {
			return ErrConflict
		}
		// Only the action dispatch needs approval; read-only effect verification can
		// finish after expiry. This check is after both SSH slot waits.
		if needsApproval(c.Body.View) && mutationCommand(argv) {
			if e = s.consumedApproval(cc, tx, c.Body.View); e != nil {
				return e
			}
			if bound(stored.Body.View) != bound(c.Body.View) {
				return ErrApproval
			}
		}
		checkpoint := *c
		checkpoint.Body.Dispatched = true
		checkpoint.Body.View.Status = Running
		return s.saveCall(cc, tx, checkpoint)
	})
	if err != nil {
		if err == ErrQuota {
			b.truncated = true
			c.Body.ReadTruncated = true
		}
		return nil, err
	}
	c.Body.Dispatched = true
	if ctx.Err() != nil {
		return nil, ErrConflict
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := s.executor.ExecLimited(callCtx, snap, argv, target.ExecutionLimits{Bytes: limitBytes, Lines: b.lines})
	// Credentials may rotate while SSH is in flight. No returned stream may be
	// projected or persisted until the latest local secret set is available.
	if secretErr := s.registerSecrets(); secretErr != nil {
		return nil, secretErr
	}
	if res != nil {
		total := len(res.Stdout) + len(res.Stderr)
		lines := 0
		for _, text := range []string{res.Stdout, res.Stderr} {
			lines += strings.Count(text, "\n")
			if text != "" && !strings.HasSuffix(text, "\n") {
				lines++
			}
		}
		if total > limitBytes || lines > b.lines {
			c.Body.ReadTruncated = true
			return nil, ErrInvalid
		}
		b.bytes -= total
		b.lines -= lines
		b.truncated = b.truncated || res.Truncated
		c.Body.ReadBytes += total
		c.Body.ReadLines += lines
		c.Body.ReadTruncated = b.truncated
		// Mask while the original bounded streams are still intact, before any
		// projection/line clipping or persistence callback can consume a result.
		if res.Truncated {
			res.Stdout = s.masker.ScrubTruncated(res.Stdout)
			res.Stderr = s.masker.ScrubTruncated(res.Stderr)
		} else {
			res.Stdout = s.masker.Scrub(res.Stdout)
			res.Stderr = s.masker.Scrub(res.Stderr)
		}
		checkpointErr := s.persist(c.Body.View.SessionID, func(cc context.Context, tx *sql.Tx) error {
			r, e := s.run(cc, tx, c.Body.View.SessionID, c.Body.View.RunID)
			if e != nil {
				return e
			}
			r.Body.ReadBytes += total
			if e = s.saveRun(cc, tx, r); e != nil {
				return e
			}
			checkpoint := *c
			checkpoint.Body.View.Status = Running
			return s.saveCall(cc, tx, checkpoint)
		})
		if checkpointErr != nil {
			return res, checkpointErr
		}
	}
	if err != nil {
		return res, ErrUnavailable
	}
	if res == nil {
		return nil, ErrUnavailable
	}
	return res, nil
}
func (s *Service) resolveContainer(ctx context.Context, c *callRecord, b *readBudget) (string, error) {
	res, err := s.bounded(ctx, c, []string{"docker", "inspect", "--type", "container", "--format", "{{.Id}}", c.Body.View.Args.Container}, b)
	if err != nil || res == nil || res.ExitCode != 0 || res.Truncated {
		return "", ErrInvalid
	}
	id := strings.TrimSpace(res.Stdout)
	if !immutableContainer.MatchString(id) {
		return "", ErrInvalid
	}
	return id, nil
}
func (s *Service) prepareMutation(ctx context.Context, c callRecord) error {
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ctx = callCtx
	b := remainingReadBudget(c)
	v := &c.Body.View
	if err := s.beginCall(ctx, &c); err != nil {
		return err
	}
	if v.Args.Container != "" {
		id, err := s.resolveContainer(ctx, &c, b)
		if err != nil {
			v.Status = Failed
			v.Error = "container resolution failed"
			return s.recordResult(c)
		}
		v.Args.Container = id
		v.Object = id
		v.ArgsHash = digest(v.Args)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	v.Status = AwaitingConfirmation
	return s.persist(v.SessionID, func(cc context.Context, tx *sql.Tx) error {
		r, e := s.run(cc, tx, v.SessionID, v.RunID)
		if e != nil {
			return e
		}
		if r.View.CancelRequested {
			return ErrConflict
		}
		if e = s.saveCall(cc, tx, c); e != nil {
			return e
		}
		return s.append(cc, tx, Entry{SessionID: v.SessionID, RunID: v.RunID, CallID: v.ID, Kind: "call_status", Status: v.Status}, false, false)
	})
}
func (s *Service) recordResult(c callRecord) error {
	v := &c.Body.View
	v.Truncated = v.Truncated || c.Body.ReadTruncated
	return s.persist(v.SessionID, func(ctx context.Context, tx *sql.Tx) error {
		// Charge result text exactly once. Events contain a call reference, never another copy.
		stored, e := s.call(ctx, tx, v.SessionID, v.ID)
		if e != nil {
			return e
		}
		if terminal(stored.Body.View.Status) {
			return ErrConflict
		}
		r, e := s.run(ctx, tx, v.SessionID, v.RunID)
		if e != nil {
			return e
		}
		// Recover a failed read-count checkpoint without charging a persisted one twice.
		r.Body.ReadBytes += max(0, c.Body.ReadBytes-stored.Body.ReadBytes)
		remaining := max(0, (512<<10)-r.Body.OutputBytes)
		v.Error = clip(v.Error, min(1024, remaining))
		if v.Resources != nil && resultBytes(*v) > remaining {
			v.Resources = nil
			v.Truncated = true
			v.Status = Failed
			v.Error = clip("resource output budget exhausted", remaining)
		}
		if len(v.Output)+len(v.Error) > remaining {
			v.Output = clip(v.Output, max(0, remaining-len(v.Error)))
			v.Truncated = true
		}
		r.Body.OutputBytes += resultBytes(*v)
		if e = s.saveRun(ctx, tx, r); e != nil {
			return e
		}
		if e = s.saveCall(ctx, tx, c); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE ops_chat_sessions SET body_bytes=body_bytes+? WHERE id=?", resultBytes(*v), v.SessionID); e != nil {
			return ErrStorage
		}
		return s.append(ctx, tx, Entry{SessionID: v.SessionID, RunID: v.RunID, CallID: v.ID, Kind: "call_status", Status: v.Status}, false, false)
	})
}
func (s *Service) executeCall(ctx context.Context, c callRecord) error {
	if isWebTool(c.Body.View.ToolID) {
		return s.executeWebCall(ctx, c)
	}
	if isProjectTool(c.Body.View.ToolID) {
		return s.executeProjectCall(ctx, c)
	}
	v := &c.Body.View
	b := remainingReadBudget(c)
	callCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	if err := s.beginCall(callCtx, &c); err != nil {
		return err
	}
	r, budgetErr := s.run(callCtx, s.db, v.SessionID, v.RunID)
	if budgetErr != nil {
		return budgetErr
	}
	b.bytes = min(b.bytes, (512<<10)-r.Body.OutputBytes-1024)
	if b.bytes < 1 {
		v.Status = Failed
		v.Truncated = true
		v.Error = "run output budget exhausted"
		return s.recordResult(c)
	}
	if v.Args.Container != "" && !mutation(v.ToolID) {
		id, err := s.resolveContainer(callCtx, &c, b)
		if err != nil {
			v.Status = Failed
			v.Error = "container resolution failed"
			return s.recordResult(c)
		}
		v.Args.Container = id
		v.Object = id
		v.ArgsHash = digest(v.Args)
	}
	err := error(nil)
	if needsApproval(*v) {
		if s.audit == nil {
			v.Status = Failed
			v.Error = "local audit unavailable"
			return s.recordResult(c)
		}
		// Outside DB transactions; the injected recorder only writes the local append-only log.
		err = s.audit.Record(callCtx, audit.Entry{Actor: "admin", Action: "ops_chat_intent", TargetType: "ops_chat_call", TargetID: v.ID,
			Detail: map[string]any{"sessionId": v.SessionID, "runId": v.RunID, "callId": v.ID, "serverId": v.ServerID, "toolId": v.ToolID, "argsHash": v.ArgsHash, "targetHash": v.TargetHash, "status": "intent"}})
		if err != nil {
			v.Status = Failed
			v.Error = "local audit failed"
			return s.recordResult(c)
		}
		// Audit can block; recheck cancellation, lease and approval immediately before dispatch.
		err = s.persist(v.SessionID, func(cc context.Context, tx *sql.Tx) error {
			r, e := s.run(cc, tx, v.SessionID, v.RunID)
			if e != nil {
				return e
			}
			if r.View.CancelRequested || callCtx.Err() != nil {
				return context.Canceled
			}
			return s.consumedApproval(cc, tx, *v)
		})
		if err != nil {
			if errors.Is(err, context.Canceled) || callCtx.Err() != nil {
				// 用户取消:软中断,带原因。
				v.Status = Interrupted
				v.Error = "dispatch blocked: " + dispatchBlockReason(callCtx, err)
				return s.recordResult(c)
			}
			// 确认单绑定不匹配/租约失效等安全事件:fail-closed,run 置 Failed。
			v.Error = "dispatch blocked: " + dispatchBlockReason(callCtx, err)
			return err
		}
	}
	// Capture again outside the transaction. Never fall back to the old unbounded Exec.
	current, e := s.executor.Capture(callCtx, v.ServerID)
	if e != nil || current.Fingerprint() != v.TargetHash {
		v.Status = Failed
		v.Error = "target configuration changed"
		return s.recordResult(c)
	}
	v.Status = Succeeded
	exec := s.bounded
	if needsApproval(*v) {
		release, slotErr := s.takeSlot(callCtx, v.ServerID)
		if slotErr != nil {
			v.Status = Interrupted
			v.Error = "dispatch blocked: SSH 槽位等待超时(并发执行已满)"
			return s.recordResult(c)
		}
		defer release()
		exec = s.boundedInSlot
	}
	if v.ToolID == "host_resources" {
		v.Resources = &HostResources{Unavailable: []string{}}
	}
	// docker_exec 允许传容器名:执行时(已持有变更槽位)解析名字→ID,
	// 之后用不可变 ID 执行,避免"批准后名字被重建指向别的容器"。
	if v.ToolID == "docker_exec" && !immutableContainer.MatchString(v.Args.Container) {
		res, e := exec(callCtx, &c, []string{"docker", "ps", "-a", "--no-trunc", "--filter", "name=^/" + v.Args.Container + "$", "--format", "{{.ID}}"}, b)
		if e != nil || res.ExitCode != 0 {
			v.Status = Failed
			v.Error = "container name could not be resolved"
			return s.recordResult(c)
		}
		id := strings.TrimSpace(res.Stdout)
		if !immutableContainer.MatchString(id) {
			v.Status = Failed
			v.Error = "container name did not resolve to a unique container ID"
			return s.recordResult(c)
		}
		v.Args.Container = id
	}
	cmds := commands(v.ToolID, v.Args)
	if len(cmds) == 0 {
		v.Status = Failed
		v.Error = "invalid command"
		return s.recordResult(c)
	}
	for _, argv := range cmds {
		res, e := exec(callCtx, &c, argv, b)
		if e != nil {
			if v.Resources != nil && callCtx.Err() == nil && e != ErrLease && e != ErrStorage && e != ErrConflict {
				v.Resources.collect(resourcePart(argv), "", false)
				continue
			}
			v.Status = Failed
			if needsApproval(*v) && c.Body.Dispatched {
				v.Status = Unknown
			} else if needsApproval(*v) {
				v.Status = Interrupted
			}
			v.Error = "bounded execution failed"
			break
		}
		now := time.Now().UTC()
		v.CollectedAt = &now
		exit := res.ExitCode
		v.ExitCode = &exit
		stdout := res.Stdout
		if v.ToolID == "host_processes" {
			lines := strings.Split(stdout, "\n")
			if len(lines) > 31 {
				stdout = strings.Join(lines[:31], "\n")
				b.truncated = true
			}
		}
		if v.ToolID == "host_resources" {
			v.Resources.collect(resourcePart(argv), s.scrub(stdout, 64<<10), exit == 0 && !res.Truncated)
			continue
		}
		v.Output += s.scrub(stdout, b.bytes+len(stdout))
		if res.Stderr != "" {
			v.Output += "\n" + s.scrub(res.Stderr, 64<<10)
		}
		v.Truncated = b.truncated
		if exit != 0 {
			v.Status = Failed
			if needsApproval(*v) && v.ToolID != "docker_exec" {
				v.Status = Unknown
			}
			v.Error = "remote command exited nonzero"
			break
		}
	}
	if v.Resources != nil {
		if len(v.Resources.Unavailable) == 3 {
			v.Status = Failed
			v.Error = "resource metrics unavailable"
		} else if len(v.Resources.Unavailable) > 0 {
			v.Status = PartialFailed
			v.Error = "some resource metrics unavailable"
		}
		encoded, _ := json.Marshal(v.Resources)
		if len(encoded) > 64<<10 {
			v.Resources = nil
			v.Status = Failed
			v.Error = "resource projection exceeded output budget"
			v.Truncated = true
		}
	}
	// Exec exit status proves command completion only, never a verified business effect.
	if mutation(v.ToolID) && v.ToolID != "docker_exec" && v.ToolID != "host_exec" && c.Body.Dispatched && v.ExitCode != nil && callCtx.Err() == nil {
		if s.verifyMutation(callCtx, &c, b) {
			v.Status = Succeeded
			v.Error = ""
		} else {
			v.Status = Unknown
			v.Error = "remote effect not verified"
		}
	}
	v.Output = s.scrub(v.Output, 64<<10)
	v.Truncated = v.Truncated || b.truncated || c.Body.ReadTruncated || len(v.Output) >= 64<<10
	if callCtx.Err() != nil && needsApproval(*v) && c.Body.Dispatched {
		v.Status = Unknown
	}
	if v.CollectedAt == nil {
		now := time.Now().UTC()
		v.CollectedAt = &now
	}
	return s.recordResult(c)
}

// executeProjectCall 执行项目维度工具:本地后端调用(非 SSH,不占 SSH 槽位)。
// 变更工具(run_trigger/run_cancel)与机器路径同强度:意图审计 → 确认单绑定
// 复核 → 项目指纹闸门 → 派发检查点 → 效果校验(经 RunDetail)。
func (s *Service) executeProjectCall(ctx context.Context, c callRecord) error {
	v := &c.Body.View
	if s.projects == nil {
		v.Status = Failed
		v.Error = "project backend unavailable"
		return s.recordResult(c)
	}
	b := remainingReadBudget(c)
	callCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	if err := s.beginCall(callCtx, &c); err != nil {
		return err
	}
	r, budgetErr := s.run(callCtx, s.db, v.SessionID, v.RunID)
	if budgetErr != nil {
		return budgetErr
	}
	b.bytes = min(b.bytes, (512<<10)-r.Body.OutputBytes-1024)
	if b.bytes < 1 {
		v.Status = Failed
		v.Truncated = true
		v.Error = "run output budget exhausted"
		return s.recordResult(c)
	}
	if needsApproval(*v) {
		if s.audit == nil {
			v.Status = Failed
			v.Error = "local audit unavailable"
			return s.recordResult(c)
		}
		// Outside DB transactions; the injected recorder only writes the local append-only log.
		err := s.audit.Record(callCtx, audit.Entry{Actor: "admin", Action: "ops_chat_intent", TargetType: "ops_chat_call", TargetID: v.ID,
			Detail: map[string]any{"sessionId": v.SessionID, "runId": v.RunID, "callId": v.ID, "projectId": v.ServerID, "toolId": v.ToolID, "argsHash": v.ArgsHash, "targetHash": v.TargetHash, "status": "intent"}})
		if err != nil {
			v.Status = Failed
			v.Error = "local audit failed"
			return s.recordResult(c)
		}
		// Audit can block; recheck cancellation, lease and approval immediately before dispatch.
		err = s.persist(v.SessionID, func(cc context.Context, tx *sql.Tx) error {
			rr, e := s.run(cc, tx, v.SessionID, v.RunID)
			if e != nil {
				return e
			}
			if rr.View.CancelRequested || callCtx.Err() != nil {
				return context.Canceled
			}
			return s.consumedApproval(cc, tx, *v)
		})
		if err != nil {
			if errors.Is(err, context.Canceled) || callCtx.Err() != nil {
				v.Status = Interrupted
				v.Error = "dispatch blocked: " + dispatchBlockReason(callCtx, err)
				return s.recordResult(c)
			}
			v.Error = "dispatch blocked: " + dispatchBlockReason(callCtx, err)
			return err
		}
	}
	// 指纹闸门:项目配置在批准窗口内变化则拒绝派发(与机器目标同语义)。
	snap, e := s.projects.Snapshot(callCtx, v.ServerID)
	if e != nil || snap.ID != v.ServerID || snap.Fingerprint() != v.TargetHash {
		v.Status = Failed
		v.Error = "target configuration changed"
		return s.recordResult(c)
	}
	// 派发检查点:先落 Dispatched 再执行;崩溃恢复按 Unknown 收敛。
	if err := s.persist(v.SessionID, func(cc context.Context, tx *sql.Tx) error {
		rr, e := s.run(cc, tx, v.SessionID, v.RunID)
		if e != nil {
			return e
		}
		if rr.View.CancelRequested || callCtx.Err() != nil || terminal(rr.View.Status) {
			return ErrConflict
		}
		stored, e := s.call(cc, tx, v.SessionID, v.ID)
		if e != nil {
			return e
		}
		if stored.Body.View.Status != Running {
			return ErrConflict
		}
		checkpoint := c
		checkpoint.Body.Dispatched = true
		return s.saveCall(cc, tx, checkpoint)
	}); err != nil {
		if errors.Is(err, context.Canceled) || callCtx.Err() != nil {
			v.Status = Interrupted
			v.Error = "dispatch blocked: " + dispatchBlockReason(callCtx, err)
			return s.recordResult(c)
		}
		v.Error = "dispatch blocked: " + dispatchBlockReason(callCtx, err)
		return err
	}
	c.Body.Dispatched = true
	output, execErr := s.runProjectTool(callCtx, v)
	now := time.Now().UTC()
	v.CollectedAt = &now
	exit := 0
	v.ExitCode = &exit
	if execErr != nil {
		v.Status = Failed
		v.Error = clip(execErr.Error(), 1024)
		// 变更已派发但失败:效果不确定,标记 Unknown 强制下次先校验。
		if c.Body.Dispatched && projectMutation(v.ToolID) {
			v.Status = Unknown
		}
	} else {
		v.Output = s.scrub(output, 64<<10)
		v.Truncated = len(v.Output) >= 64<<10
		v.Status = Succeeded
		// Exec 成功只证明受理,不代表业务效果;经 RunDetail 校验(与机器 verify 同语义)。
		if projectMutation(v.ToolID) && c.Body.Dispatched && callCtx.Err() == nil && !s.verifyProjectMutation(callCtx, v) {
			v.Status = Unknown
			v.Error = "remote effect not verified"
		}
	}
	return s.recordResult(c)
}

// executeWebCall 执行网页搜索/抓取工具:本地后端调用(非 SSH、不占 SSH 槽位、
// 只读不走确认卡)。TargetHash 落轮次提供方指纹,执行前复核当前模型指纹一致,
// 配置漂移即 fail-closed(与机器/项目指纹闸门同语义)。
func (s *Service) executeWebCall(ctx context.Context, c callRecord) error {
	v := &c.Body.View
	if s.web == nil {
		v.Status = Failed
		v.Error = "web tools backend unavailable"
		return s.recordResult(c)
	}
	b := remainingReadBudget(c)
	callCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	if err := s.beginCall(callCtx, &c); err != nil {
		return err
	}
	r, budgetErr := s.run(callCtx, s.db, v.SessionID, v.RunID)
	if budgetErr != nil {
		return budgetErr
	}
	b.bytes = min(b.bytes, (512<<10)-r.Body.OutputBytes-1024)
	if b.bytes < 1 {
		v.Status = Failed
		v.Truncated = true
		v.Error = "run output budget exhausted"
		return s.recordResult(c)
	}
	// 配置漂移闸门:网页目的地复用 AI 配置,轮次开始时的提供方指纹与当前不一致
	//(密钥轮换/换供应商/改开关)则拒绝派发。
	if s.model == nil || s.model.Info().ConfigHash != v.TargetHash {
		v.Status = Failed
		v.Error = "target configuration changed"
		return s.recordResult(c)
	}
	var output string
	var execErr error
	switch v.ToolID {
	case "web_search":
		output, execErr = s.web.Search(callCtx, v.Args.Query, v.Args.Count)
	case "web_fetch":
		output, execErr = s.web.Fetch(callCtx, v.Args.URL)
	default:
		execErr = ErrInvalid
	}
	now := time.Now().UTC()
	v.CollectedAt = &now
	if execErr != nil {
		v.Status = Failed
		v.Error = clip(execErr.Error(), 1024)
		return s.recordResult(c)
	}
	exit := 0
	v.ExitCode = &exit
	v.Output = s.scrub(output, 64<<10)
	v.Truncated = len(v.Output) >= 64<<10
	v.Status = Succeeded
	return s.recordResult(c)
}

// runProjectTool 把已校验的调用分发到项目后端;输出统一为有界文本。
func (s *Service) runProjectTool(ctx context.Context, v *Call) (string, error) {
	switch v.ToolID {
	case "project_info":
		return s.projects.Info(ctx, v.ServerID)
	case "code_tree":
		return s.projects.CodeTree(ctx, v.ServerID, v.Args.Ref, v.Args.Path)
	case "code_read":
		return s.projects.CodeRead(ctx, v.ServerID, v.Args.Ref, v.Args.Path, v.Args.Lines)
	case "pipeline_config":
		return s.projects.PipelineConfig(ctx, v.ServerID)
	case "repo_pipeline_config":
		return s.projects.RepoConfig(ctx, v.ServerID, v.Args.Branch)
	case "project_runs":
		return s.projects.Runs(ctx, v.ServerID, v.Args.Status, v.Args.Limit)
	case "run_detail":
		return s.projects.RunDetail(ctx, v.Args.RunID)
	case "run_logs":
		ordinal := -1
		if v.Args.StepOrdinal != nil {
			ordinal = *v.Args.StepOrdinal
		}
		return s.projects.RunLogs(ctx, v.Args.RunID, ordinal, v.Args.Lines)
	case "run_trigger":
		return s.projects.TriggerRun(ctx, v.ServerID, v.Args.Branch, v.Args.Params)
	case "run_cancel":
		return s.projects.CancelRun(ctx, v.Args.RunID)
	}
	return "", ErrInvalid
}

// verifyProjectMutation 校验项目变更的业务效果:trigger 要求新 run 已受理
// (输出 JSON 契约含 runId;RunDetail 输出含 status);cancel 的同步受理本身
// 即效果证明(queued 立即落终态,running 经 context 传播由 worker 异步收敛)。
func (s *Service) verifyProjectMutation(ctx context.Context, v *Call) bool {
	if v.ToolID == "run_cancel" {
		return true
	}
	var created struct {
		RunID string `json:"runId"`
	}
	if json.Unmarshal([]byte(v.Output), &created) != nil || !validID(created.RunID) {
		return false
	}
	detail, err := s.projects.RunDetail(ctx, created.RunID)
	if err != nil {
		return false
	}
	var state struct {
		Status string `json:"status"`
	}
	return json.Unmarshal([]byte(detail), &state) == nil && projectRunStatuses[state.Status]
}
func (s *Service) verifyMutation(ctx context.Context, c *callRecord, b *readBudget) bool {
	v := c.Body.View
	if v.ToolID == "docker_action" {
		res, err := s.boundedInSlot(ctx, c, []string{"docker", "inspect", "--type", "container", "--format", "{{json .State}}", v.Object}, b)
		if err != nil || res.ExitCode != 0 || res.Truncated {
			return false
		}
		var state struct {
			Running *bool
			Paused  *bool
		}
		if json.Unmarshal([]byte(res.Stdout), &state) != nil || state.Running == nil || state.Paused == nil {
			return false
		}
		if v.Args.Action == "restart" && (v.ExitCode == nil || *v.ExitCode != 0) {
			return false
		}
		switch v.Args.Action {
		case "start", "restart":
			return *state.Running && !*state.Paused
		case "stop":
			return !*state.Running
		case "pause":
			return *state.Paused
		case "unpause":
			return !*state.Paused
		}
	}
	res, err := s.boundedInSlot(ctx, c, []string{"systemctl", "show", v.Object, "--property=ActiveState", "--value"}, b)
	if err != nil || res.ExitCode != 0 || res.Truncated {
		return false
	}
	state := strings.TrimSpace(res.Stdout)
	if v.Args.Action == "restart" && (v.ExitCode == nil || *v.ExitCode != 0) {
		return false
	}
	if v.Args.Action == "stop" {
		return state == "inactive"
	}
	return state == "active"
}

// dispatchBlockReason 把派发闸门失败映射为人读原因(绝无密钥)。
func dispatchBlockReason(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, context.Canceled) || ctx.Err() != nil:
		return "任务已取消"
	case errors.Is(err, ErrLease):
		return "实例租约失效(重启或多实例)"
	case errors.Is(err, ErrApproval):
		return "确认单已失效或与调用不匹配"
	default:
		return "状态校验未通过"
	}
}
