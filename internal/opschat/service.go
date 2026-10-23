package opschat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/mask"
	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/vault"
)

type job struct {
	retry   bool
	session string
	cancel  context.CancelFunc
	done    chan struct{}
}
type Service struct {
	// db 是运行期连接池句柄(SQLite 下为守卫包装,见 txguard.go);写路径
	// 一律经 transaction() 的 *sql.Tx,不经此句柄。
	db poolDB
	// raw 仅供事务开启(BeginTx)、方言判定与构造期初始化使用。
	raw         *sql.DB
	txg         txGuard
	vault       vault.Vault
	executor    target.LimitedExecutor
	model       Model
	projects    ProjectBackend
	web         WebBackend
	audit       audit.Recorder
	masker      *mask.Masker
	instance    string
	runtime     RuntimeLimits
	contextSrc  func() ContextPolicy
	mu          sync.Mutex
	root        context.Context
	stop        context.CancelFunc
	started     bool
	closing     bool
	jobs        map[string]*job
	wake        map[string]map[chan struct{}]bool
	slots       chan struct{}
	serverSlots map[string]chan struct{}
	// compactFails 按 session 记录模型压缩连续失败次数(≥3 熔断为仅确定性裁剪;
	// 内存态,重启重置可接受)。compactOK 清零。
	compactFails map[string]int
}

// policy 返回当前上下文策略(带默认值兜底;装配层未注入时全默认)。
// ⚠ 只能在**未持有 opschat 事务**时调用:注入的 contextSrc 可能同步读库
// (aiContextPolicy→ai.load),持事务调用会在单连接下自死锁。事务内需要
// 策略时,先在事务外取好再传参(见 sessionUsage)。
func (s *Service) policy() ContextPolicy {
	s.txg.check("policy")
	if s.contextSrc == nil {
		return ContextPolicy{}.withDefaults()
	}
	return s.contextSrc().withDefaults()
}

func New(o Options) (*Service, error) {
	if o.DB == nil || o.Vault == nil {
		return nil, ErrUnavailable
	}
	if o.Masker == nil {
		o.Masker = mask.NewMasker()
	}
	runtime := o.Runtime.withDefaults()
	s := &Service{raw: o.DB, vault: o.Vault, executor: o.Executor, model: o.Model, projects: o.Projects, web: o.Web, audit: o.LocalRecorder, masker: o.Masker, instance: uuid.NewString(), runtime: runtime, contextSrc: o.Context, jobs: map[string]*job{}, wake: map[string]map[chan struct{}]bool{}, slots: make(chan struct{}, runtime.SSHSlots), serverSlots: map[string]chan struct{}{}, compactFails: map[string]int{}}
	// SQLite 单连接:嵌套池 IO 即自死锁,装事务守卫;MySQL 多连接不致命,关闭。
	if store.DialectOf(o.DB) == store.SQLite {
		s.txg.active = true
		s.db = guardedPool{inner: o.DB, g: &s.txg}
	} else {
		s.db = o.DB
	}
	check, err := s.seal("pipewright.opschat.keycheck.v1")
	if err != nil {
		return nil, err
	}
	var stored []byte
	err = s.raw.QueryRow("SELECT keycheck FROM ops_chat_state WHERE id=1").Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		var count int
		if s.raw.QueryRow("SELECT COUNT(*) FROM ops_chat_sessions").Scan(&count) != nil {
			return nil, ErrStorage
		}
		if count != 0 {
			return nil, ErrDecrypt
		}
		_, err = s.raw.Exec("INSERT INTO ops_chat_state(id,keycheck) VALUES(1,?)", check)
		if err != nil {
			if s.raw.QueryRow("SELECT keycheck FROM ops_chat_state WHERE id=1").Scan(&stored) != nil {
				return nil, ErrStorage
			}
		} else {
			stored = check
		}
	} else if err != nil {
		return nil, ErrStorage
	}
	var plain string
	if s.open(stored, &plain) != nil || plain != "pipewright.opschat.keycheck.v1" {
		return nil, ErrDecrypt
	}
	return s, nil
}
func (s *Service) Capabilities() Capabilities {
	p := ProviderInfo{}
	if s.model != nil {
		p = s.model.Info()
	}
	s.mu.Lock()
	available := s.started && !s.closing
	s.mu.Unlock()
	limits := resourceLimits
	policy := s.policy()
	limits.ContextBytes = policy.HistoryBytes
	limits.ContextMessages = policy.HistoryMessages
	return Capabilities{Available: available, ModelAvailable: s.model != nil && validProvider(p), Provider: p, Limits: limits}
}
func (s *Service) ready() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.closing {
		return ErrUnavailable
	}
	return nil
}
func validID(id string) bool { _, err := uuid.Parse(id); return err == nil }
func validTargets(ids []string) bool {
	if len(ids) > resourceLimits.Targets {
		return false
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !validID(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

// validTargetsFor 按维度校验目标机集合:机器维度 ≤ Targets(8);项目维度
// target_0 固定留给项目,目标机别名 target_1..7,故 ≤ Targets-1。
func validTargetsFor(scope string, ids []string) bool {
	if !validTargets(ids) {
		return false
	}
	return scope != ScopeProject || len(ids) < resourceLimits.Targets
}
func (s *Service) Create(ctx context.Context, in CreateInput) (*Session, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if err := s.validateContext(ctx, in.Context); err != nil {
		return nil, err
	}
	if in.Scope != ScopeServer && in.Scope != ScopeProject {
		return nil, ErrInvalid
	}
	if in.Scope == ScopeProject {
		// 项目维度:后端必须注入,项目必须存在且可解析。
		if s.projects == nil || !validID(in.ProjectID) {
			return nil, ErrInvalid
		}
		if _, err := s.projects.Snapshot(ctx, in.ProjectID); err != nil {
			return nil, ErrInvalid
		}
	}
	if !validTargetsFor(in.Scope, in.ServerIDs) || !utf8.ValidString(in.Title) || utf8.RuneCountInString(in.Title) > 80 {
		return nil, ErrInvalid
	}
	if err := s.registerSecrets(); err != nil {
		return nil, err
	}
	title := s.scrub(in.Title, 320)
	named := title != ""
	if title == "" {
		title = "New session"
	}
	body := sessionBody{Title: title, ServerIDs: append([]string{}, in.ServerIDs...), Named: named, Scope: in.Scope, ProjectID: in.ProjectID}
	sealed, err := s.seal(body)
	if err != nil {
		return nil, err
	}
	id := uuid.NewString()
	now := time.Now().UnixMilli()
	err = s.transaction(ctx, id, true, func(tx *sql.Tx) error {
		var count int
		if tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM ops_chat_sessions").Scan(&count) != nil {
			return ErrStorage
		}
		if count >= resourceLimits.Sessions {
			return ErrQuota
		}
		_, e := tx.ExecContext(ctx, "INSERT INTO ops_chat_sessions(id,revision,body,created_at,updated_at) VALUES(?,1,?,?,?)", id, sealed, now, now)
		if e != nil {
			return ErrStorage
		}
		if in.Context != "" {
			if e = s.bindContext(ctx, tx, in.Context, id); e != nil {
				return e
			}
		}
		return s.append(ctx, tx, Entry{SessionID: id, Kind: "session_updated"}, false, false)
	})
	if err != nil {
		return nil, err
	}
	s.notify(id)
	return s.Get(ctx, id)
}
func (s *Service) Get(ctx context.Context, id string) (*Session, error) {
	if err := s.registerSecrets(); err != nil {
		return nil, err
	}
	r, err := s.session(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	r.View.ContextUsage = s.sessionUsage(ctx, s.db, id, s.policy())
	view := s.projectSessionActive(ctx, s.db, r)
	return &view, nil
}
func (s *Service) Patch(ctx context.Context, id string, in PatchInput) (*Session, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if in.Title != nil && (!utf8.ValidString(*in.Title) || utf8.RuneCountInString(*in.Title) > 80) {
		return nil, ErrInvalid
	}
	if in.Draft != nil && (!utf8.ValidString(*in.Draft) || len(*in.Draft) > 8<<10) {
		return nil, ErrInvalid
	}
	// 预取会话做维度相关校验(项目 Snapshot 走后端自身连接,不得嵌套本包写事务)。
	current, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.ServerIDs != nil && !validTargetsFor(current.Scope, *in.ServerIDs) {
		return nil, ErrInvalid
	}
	if in.ProjectID != nil {
		// 切换绑定项目:仅项目维度会话;需后端在线且项目存在(维度本身不可切换)。
		if current.Scope != ScopeProject || s.projects == nil || !validID(*in.ProjectID) {
			return nil, ErrInvalid
		}
		if _, e := s.projects.Snapshot(ctx, *in.ProjectID); e != nil {
			return nil, ErrInvalid
		}
	}
	if err := s.registerSecrets(); err != nil {
		return nil, err
	}
	err = s.transaction(ctx, id, true, func(tx *sql.Tx) error {
		r, e := s.session(ctx, tx, id)
		if e != nil {
			return e
		}
		if r.View.Revision != in.Revision {
			return ErrConflict
		}
		if in.ServerIDs != nil || in.ProjectID != nil {
			// 目标/项目仅在会话无进行中的轮时可改(活 job 或合法挂起;幻影不算)。
			busy, e := s.sessionBusy(ctx, tx, r)
			if e != nil {
				return e
			}
			if busy {
				return ErrConflict
			}
		}
		if in.Title != nil {
			r.Body.Title = s.scrub(*in.Title, 320)
			r.Body.Named = true
		}
		if in.Draft != nil {
			r.Body.Draft = s.scrub(*in.Draft, 8<<10)
		}
		if in.ServerIDs != nil {
			r.Body.ServerIDs = append([]string{}, (*in.ServerIDs)...)
		}
		if in.ProjectID != nil {
			r.Body.ProjectID = *in.ProjectID
		}
		r.View.Revision++
		if e = s.saveSession(ctx, tx, r); e != nil {
			return e
		}
		return s.append(ctx, tx, Entry{SessionID: id, Kind: "session_updated"}, false, false)
	})
	if err != nil {
		return nil, err
	}
	s.notify(id)
	return s.Get(ctx, id)
}
func (s *Service) Activate(ctx context.Context, id string) error {
	return s.ActivateContext(ctx, id, "")
}
func (s *Service) duplicate(ctx context.Context, session, request, hash string) (*Run, error) {
	var id, chat, payload string
	err := s.db.QueryRowContext(ctx, "SELECT id,session_id,payload_hash FROM ops_chat_runs WHERE request_id=?", request).Scan(&id, &chat, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, ErrStorage
	}
	if chat != session || payload != hash {
		return nil, ErrConflict
	}
	r, err := s.run(ctx, s.db, session, id)
	if err != nil {
		return nil, err
	}
	r, _ = s.projectRun(ctx, s.db, r)
	return &r.View, nil
}
func (s *Service) Submit(ctx context.Context, id string, in TurnInput) (*Run, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	// 工具只归 AI 调用:turn 一律是自然语言文本;目标机必选(Agent 循环需要执行目标)。
	if !validID(in.ClientRequestID) || strings.TrimSpace(in.Text) == "" || !utf8.ValidString(in.Text) || len(in.Text) > 8<<10 {
		return nil, ErrInvalid
	}
	hash := digest(in)
	if old, err := s.duplicate(ctx, id, in.ClientRequestID, hash); old != nil || err != nil {
		return old, err
	}
	if s.model == nil || !validProvider(s.model.Info()) {
		return nil, ErrUnavailable
	}
	if in.AnalyzeResults && (s.model == nil || !validProvider(in.Provider) || in.Provider != s.model.Info()) {
		return nil, ErrConsent
	}
	chat, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if chat.Revision != in.Revision {
		return nil, ErrConflict
	}
	// 项目维度:提交时冻结项目快照(必须存在);可以不关联机器(仅项目工具)。
	// 机器维度:目标机必选(Agent 循环需要执行目标)。
	var project *ProjectSnapshot
	if chat.Scope == ScopeProject {
		if s.projects == nil || !validID(chat.ProjectID) {
			return nil, ErrInvalid
		}
		snap, e := s.projects.Snapshot(ctx, chat.ProjectID)
		if e != nil {
			return nil, ErrConflict
		}
		project = &snap
	} else if len(chat.ServerIDs) == 0 {
		return nil, ErrInvalid
	}
	snapshots := []target.ConnectionSnapshot{}
	for _, server := range chat.ServerIDs {
		if s.executor == nil {
			return nil, ErrUnavailable
		}
		snap, e := s.executor.Capture(ctx, server)
		if e != nil {
			return nil, ErrConflict
		}
		if snap.Server.ID != server {
			return nil, ErrInvalid
		}
		snapshots = append(snapshots, snap)
	}
	if err = s.registerSecrets(); err != nil {
		return nil, err
	}
	in.Text = s.scrub(in.Text, 8<<10)
	provider := in.Provider
	if !in.AnalyzeResults {
		provider = s.model.Info()
	}
	return s.enqueue(ctx, id, in.ClientRequestID, hash, runBody{Turn: in, Targets: snapshots, Project: project, Provider: provider, AnalysisAuthorized: in.AnalyzeResults, Question: in.Text}, in.Revision)
}
func (s *Service) enqueue(ctx context.Context, id, request, hash string, body runBody, revision int64) (*Run, error) {
	for _, snap := range body.Targets {
		encoded, e := json.Marshal(snap)
		if e != nil || len(encoded) > 8<<10 {
			return nil, ErrQuota
		}
	}
	runID := uuid.NewString()
	now := time.Now().UTC()
	r := runRecord{View: Run{ID: runID, SessionID: id, ClientRequestID: request, Status: Queued, CreatedAt: now}, Body: body, Hash: hash}
	var old *Run
	err := s.transaction(ctx, id, true, func(tx *sql.Tx) error {
		var existing, chat, payload string
		e := tx.QueryRowContext(ctx, "SELECT id,session_id,payload_hash FROM ops_chat_runs WHERE request_id=?", request).Scan(&existing, &chat, &payload)
		if e == nil {
			if chat != id || payload != hash {
				return ErrConflict
			}
			rr, e := s.run(ctx, tx, id, existing)
			if e != nil {
				return e
			}
			rr, _ = s.projectRun(ctx, tx, rr)
			old = &rr.View
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return ErrStorage
		}
		sr, e := s.session(ctx, tx, id)
		if e != nil {
			return e
		}
		if sr.View.Revision != revision {
			return ErrConflict
		}
		busy, e := s.sessionBusy(ctx, tx, sr)
		if e != nil {
			return e
		}
		if busy {
			return ErrConflict
		}
		if sr.View.ActiveRunID != "" {
			// 幻影 run 占着指针:同事务内持久收敛再接管(sessionBusy 已确认无
			// 活 worker/合法挂起;finishTx 发出终态事件并清空指针)。
			old, e := s.run(ctx, tx, id, sr.View.ActiveRunID)
			if e != nil {
				return e
			}
			if e = s.recoverRunTx(ctx, tx, old); e != nil {
				return e
			}
		}
		if !activeBudget(sr) {
			return ErrQuota
		}
		if e = s.validateRetryTx(ctx, tx, id, body, sr); e != nil {
			return e
		}
		if body.Analysis == nil && len(body.RetryActions) == 0 {
			body.Question, e = s.turnQuestion(ctx, tx, id, body)
			if e != nil {
				return e
			}
			r.Body.Question = body.Question
		}
		b, e := s.seal(body)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO ops_chat_runs(id,session_id,request_id,payload_hash,status,body,created_at) VALUES(?,?,?,?,?,?,?)`, runID, id, request, hash, Queued, b, now.UnixMilli())
		if e != nil {
			return ErrStorage
		}
		// 无条件覆盖:幻影指针已在上面同事务收敛,合法占用已被 sessionBusy 拒绝。
		if _, e = tx.ExecContext(ctx, "UPDATE ops_chat_sessions SET active_run=? WHERE id=?", runID, id); e != nil {
			return ErrStorage
		}
		if body.Analysis == nil {
			if !sr.Body.Named && sr.Count == 0 && body.Turn.Text != "" {
				t := strings.SplitN(body.Turn.Text, "\n", 2)[0]
				runes := []rune(t)
				if len(runes) > 40 {
					runes = runes[:40]
				}
				sr.Body.Title = string(runes)
				if e = s.saveSession(ctx, tx, sr); e != nil {
					return e
				}
			}
			text := body.Turn.Text
			allowed := true
			kind := "user"
			if len(body.RetryActions) > 0 {
				text = "retry_failed"
				allowed = false
				kind = "tool_request"
			}
			if e = s.append(ctx, tx, Entry{SessionID: id, RunID: runID, Kind: kind, Text: text}, true, allowed); e != nil {
				return e
			}
		}
		return s.append(ctx, tx, Entry{SessionID: id, RunID: runID, Kind: "run_status", Status: Queued}, false, false)
	})
	if err != nil {
		return nil, err
	}
	if old != nil {
		return old, nil
	}
	s.notify(id)
	s.launch(id, runID)
	return &r.View, nil
}
func (s *Service) Cancel(ctx context.Context, id, runID string) (*Run, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	err := s.transaction(ctx, id, true, func(tx *sql.Tx) error {
		r, e := s.run(ctx, tx, id, runID)
		if e != nil {
			return e
		}
		if terminal(r.View.Status) {
			return nil
		}
		r.View.CancelRequested = true
		if e = s.saveRun(ctx, tx, r); e != nil {
			return e
		}
		switch r.View.Status {
		case AwaitingConfirmation, AwaitingAnalysisConsent, AwaitingContinuation:
			// 挂起轮(worker 已按设计退出):不等任何 worker,直接收敛终态。
			calls, e := s.calls(ctx, tx, id, runID)
			if e != nil {
				return e
			}
			for _, c := range calls {
				if !terminal(c.Body.View.Status) {
					c.Body.View.Status = Interrupted
					if e = s.saveCall(ctx, tx, c); e != nil {
						return e
					}
				}
			}
			return s.finishTx(ctx, tx, r, Interrupted)
		case Queued, Planning, Running:
			if phantomRun(r, s.jobLive(runID)) {
				// 幻影:没有会响应插旗的 worker,就地持久收敛
				//(已派发调用 → Unknown,与启动恢复同语义)。
				return s.recoverRunTx(ctx, tx, r)
			}
			// 活 worker(或正在 launch 注册窗口的新轮):插旗 + 记条目,
			// worker 在各落库点复核后自行收敛,Cancel 返回后再取消其上下文。
			return s.append(ctx, tx, Entry{SessionID: id, RunID: runID, Kind: "cancel_requested"}, false, false)
		}
		return ErrConflict
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if j := s.jobs[runID]; j != nil {
		j.cancel()
	}
	s.mu.Unlock()
	s.notify(id)
	r, err := s.run(ctx, s.db, id, runID)
	if err != nil {
		return nil, err
	}
	r, _ = s.projectRun(ctx, s.db, r)
	return &r.View, nil
}
func (s *Service) Delete(ctx context.Context, id string) error {
	if err := s.ready(); err != nil {
		return err
	}
	// Explicit Cancel is required before deleting an active session.
	sr, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if sr.ActiveRunID != "" {
		return ErrConflict
	}
	s.mu.Lock()
	for _, j := range s.jobs {
		if j.session != id {
			continue
		}
		select {
		case <-j.done:
		default:
			s.mu.Unlock()
			return ErrConflict
		}
	}
	s.mu.Unlock()
	err = s.transaction(ctx, id, true, func(tx *sql.Tx) error {
		r, e := s.session(ctx, tx, id)
		if e != nil {
			return e
		}
		busy, e := s.sessionBusy(ctx, tx, r)
		if e != nil {
			return e
		}
		if busy {
			return ErrConflict
		}
		if _, e = tx.ExecContext(ctx, "UPDATE ops_chat_state SET active_session='' WHERE id=1 AND active_session=?", id); e != nil {
			return ErrStorage
		}
		_, e = tx.ExecContext(ctx, "DELETE FROM ops_chat_sessions WHERE id=?", id)
		return databaseError(e)
	})
	if err == nil {
		s.notify(id)
	}
	return err
}
