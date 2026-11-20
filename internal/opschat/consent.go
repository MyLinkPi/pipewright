package opschat

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

func bound(c Call) BoundCall {
	return BoundCall{CallID: c.ID, ServerID: c.ServerID, ToolID: c.ToolID, Object: c.Object, Purpose: c.Purpose, Reason: c.Reason, ArgsHash: c.ArgsHash, TargetHash: c.TargetHash}
}

func mutationCommand(argv []string) bool {
	if len(argv) >= 4 && argv[0] == "docker" && argv[1] == "exec" {
		return true
	}
	if len(argv) != 3 || (argv[0] != "docker" && argv[0] != "systemctl") {
		return false
	}
	switch argv[1] {
	case "start", "stop", "restart", "pause", "unpause":
		return true
	}
	return false
}

func (s *Service) consumedApproval(ctx context.Context, tx *sql.Tx, call Call) error {
	a, err := s.approval(ctx, tx, call.SessionID, call.RunID)
	if err != nil {
		return err
	}
	if a.Status != "consumed" || a.Instance != s.instance || !a.View.ExpiresAt.After(time.Now()) || call.ArgsHash != digest(call.Args) {
		return ErrApproval
	}
	if slices.Contains(a.Body.Calls, bound(call)) {
		return nil
	}
	return ErrApproval
}
func (s *Service) issueApproval(ctx context.Context, tx *sql.Tx, id, run string, calls []callRecord) error {
	body := approvalBody{Nonce: uuid.NewString(), Calls: []BoundCall{}}
	for _, c := range calls {
		if c.Body.View.Status == AwaitingConfirmation {
			body.Calls = append(body.Calls, bound(c.Body.View))
		}
	}
	if len(body.Calls) == 0 {
		return ErrApproval
	}
	b, err := s.seal(body)
	if err != nil {
		return err
	}
	var existing string
	err = tx.QueryRowContext(ctx, "SELECT id FROM ops_chat_approvals WHERE run_id=?", run).Scan(&existing)
	switch err {
	case sql.ErrNoRows:
		_, err = tx.ExecContext(ctx, `INSERT INTO ops_chat_approvals(id,session_id,run_id,status,instance_id,expires_at,body) VALUES(?,?,?,'pending',?,?,?)`, uuid.NewString(), id, run, s.instance, time.Now().Add(s.runtime.ApprovalTTL).UnixMilli(), b)
	case nil:
		_, err = tx.ExecContext(ctx, "UPDATE ops_chat_approvals SET status='pending',instance_id=?,expires_at=?,body=? WHERE id=?", s.instance, time.Now().Add(s.runtime.ApprovalTTL).UnixMilli(), b, existing)
	}
	return databaseError(err)
}
func (s *Service) ReissueConfirmation(ctx context.Context, id, run string) (*Confirmation, error) {
	if err := s.registerSecrets(); err != nil {
		return nil, err
	}
	if s.executor == nil {
		return nil, ErrUnavailable
	}
	if err := s.ready(); err != nil {
		return nil, err
	}
	calls, err := s.calls(ctx, s.db, id, run)
	if err != nil {
		return nil, err
	}
	for _, c := range calls {
		if c.Body.View.Status == AwaitingConfirmation {
			snap, e := s.executor.Capture(ctx, c.Body.View.ServerID)
			if e != nil || snap.Fingerprint() != c.Body.View.TargetHash {
				return nil, ErrApproval
			}
		}
	}
	err = s.transaction(ctx, id, true, func(tx *sql.Tx) error {
		r, e := s.run(ctx, tx, id, run)
		if e != nil {
			return e
		}
		if r.View.Status != AwaitingConfirmation || r.View.CancelRequested {
			return ErrApproval
		}
		current, e := s.calls(ctx, tx, id, run)
		if e != nil {
			return e
		}
		if digest(current) != digest(calls) {
			return ErrApproval
		}
		if e = s.issueApproval(ctx, tx, id, run, current); e != nil {
			return e
		}
		return s.append(ctx, tx, Entry{SessionID: id, RunID: run, Kind: "confirmation_ready", Status: AwaitingConfirmation}, false, false)
	})
	if err != nil {
		return nil, err
	}
	s.notify(id)
	a, err := s.approval(ctx, s.db, id, run)
	view := s.publicConfirmation(a.View)
	return &view, err
}
func (s *Service) Confirm(ctx context.Context, id string, in ConfirmInput) (*Run, error) {
	if err := s.registerSecrets(); err != nil {
		return nil, err
	}
	if s.executor == nil {
		return nil, ErrUnavailable
	}
	if err := s.ready(); err != nil {
		return nil, err
	}
	if len(in.Calls) < 1 || len(in.Calls) > resourceLimits.Calls || !validID(in.Nonce) {
		return nil, ErrApproval
	}
	a, err := s.approval(ctx, s.db, id, in.RunID)
	if err != nil {
		return nil, err
	}
	if !a.View.Valid || a.Body.Nonce != in.Nonce || digest(s.publicConfirmation(a.View).Calls) != digest(in.Calls) {
		return nil, ErrApproval
	}
	// Capture is local and must run outside the SQLite transaction.
	calls, err := s.calls(ctx, s.db, id, in.RunID)
	if err != nil {
		return nil, err
	}
	for _, c := range calls {
		if c.Body.View.Status != AwaitingConfirmation {
			continue
		}
		if isProjectTool(c.Body.View.ToolID) {
			// 项目调用:经项目快照指纹复核(与机器 Capture 闸门同语义)。
			if s.projects == nil {
				return nil, ErrApproval
			}
			psnap, e := s.projects.Snapshot(ctx, c.Body.View.ServerID)
			if e != nil || psnap.Fingerprint() != c.Body.View.TargetHash {
				return nil, ErrApproval
			}
			continue
		}
		snap, e := s.executor.Capture(ctx, c.Body.View.ServerID)
		if e != nil || snap.Fingerprint() != c.Body.View.TargetHash {
			return nil, ErrApproval
		}
	}
	err = s.transaction(ctx, id, true, func(tx *sql.Tx) error {
		r, e := s.run(ctx, tx, id, in.RunID)
		if e != nil {
			return e
		}
		if r.View.Status != AwaitingConfirmation || r.View.CancelRequested {
			return ErrApproval
		}
		current, e := s.approval(ctx, tx, id, in.RunID)
		if e != nil {
			return e
		}
		if !current.View.Valid || current.Body.Nonce != in.Nonce || digest(s.publicConfirmation(current.View).Calls) != digest(in.Calls) {
			return ErrApproval
		}
		cs, e := s.calls(ctx, tx, id, in.RunID)
		if e != nil {
			return e
		}
		if digest(cs) != digest(calls) {
			return ErrApproval
		}
		result, e := tx.ExecContext(ctx, "UPDATE ops_chat_approvals SET status='consumed' WHERE id=? AND status='pending' AND instance_id=?", current.ID, s.instance)
		if e != nil {
			return ErrStorage
		}
		n, e := result.RowsAffected()
		if e != nil || n != 1 {
			return ErrApproval
		}
		for _, c := range cs {
			if c.Body.View.Status == AwaitingConfirmation {
				c.Body.View.Status = Queued
				if e = s.saveCall(ctx, tx, c); e != nil {
					return e
				}
			}
		}
		r.View.Status = Queued
		if e = s.saveRun(ctx, tx, r); e != nil {
			return e
		}
		return s.append(ctx, tx, Entry{SessionID: id, RunID: in.RunID, Kind: "run_status", Status: Queued}, false, false)
	})
	if err != nil {
		return nil, err
	}
	log.Printf("[opschat] confirmation consumed session=%s run=%s calls=%d", id, in.RunID, len(in.Calls))
	s.notify(id)
	s.launch(id, in.RunID)
	r, err := s.run(ctx, s.db, id, in.RunID)
	if err != nil {
		return nil, err
	}
	r, _ = s.projectRun(ctx, s.db, r)
	return &r.View, nil
}
func validProvider(p ProviderInfo) bool {
	return p.Provider != "" && p.Model != "" && len(p.Provider) <= 256 && len(p.Model) <= 256 && len(p.ConfigHash) == 64
}
func (s *Service) AnalysisPreview(ctx context.Context, id string, ids []string) (*AnalysisPreview, error) {
	return s.analysisPreview(ctx, id, ids, "")
}

// selectAnalysisCalls 在证据上限内选取最有价值的调用:成功/部分成功且有输出或
// 资源者优先(2),其次是有输出/错误/资源者(1),空/未知者最后(0);同分保持时间序,
// 选中集再恢复时间序,保证汇总按采集顺序叙述。不超限时原样返回。
func selectAnalysisCalls(candidates []callBody, limit int) []callBody {
	if len(candidates) <= limit {
		return candidates
	}
	score := func(c callBody) int {
		switch {
		case (c.View.Status == Succeeded || c.View.Status == PartialFailed) && (c.View.Output != "" || c.View.Resources != nil):
			return 2
		case c.View.Output != "" || c.View.Error != "" || c.View.Resources != nil:
			return 1
		default:
			return 0
		}
	}
	idx := make([]int, len(candidates))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int { return score(candidates[b]) - score(candidates[a]) })
	kept := idx[:limit]
	slices.Sort(kept)
	out := make([]callBody, 0, limit)
	for _, i := range kept {
		out = append(out, candidates[i])
	}
	return out
}
func (s *Service) analysisPreview(ctx context.Context, id string, ids []string, ownRun string, expectedOmitted ...int) (*AnalysisPreview, error) {
	if len(expectedOmitted) > 1 || (len(expectedOmitted) == 1 && expectedOmitted[0] < 0) {
		return nil, ErrInvalid
	}
	if err := s.registerSecrets(); err != nil {
		return nil, err
	}
	if s.model == nil {
		return nil, ErrUnavailable
	}
	p := s.model.Info()
	if !validProvider(p) {
		return nil, ErrUnavailable
	}
	if len(ids) == 0 {
		return nil, ErrInvalid
	}
	out := &AnalysisPreview{Items: []AnalysisItem{}, Provider: p}
	seen := map[string]bool{}
	aliases := map[string]string{}
	targetHashes := map[string]string{}
	var candidates []callBody
	err := s.transaction(ctx, id, false, func(tx *sql.Tx) error {
		if _, e := s.session(ctx, tx, id); e != nil {
			return e
		}
		for _, cid := range ids {
			if !validID(cid) || seen[cid] {
				return ErrInvalid
			}
			seen[cid] = true
			var b []byte
			var status string
			if e := tx.QueryRowContext(ctx, "SELECT body,status FROM ops_chat_calls WHERE id=? AND session_id=?", cid, id).Scan(&b, &status); e != nil {
				return databaseError(e)
			}
			var c callBody
			if e := s.open(b, &c); e != nil {
				return e
			}
			if !terminal(status) {
				return ErrConflict
			}
			r, e := s.run(ctx, tx, id, c.View.RunID)
			if e != nil {
				return e
			}
			r, _ = s.projectRun(ctx, tx, r)
			if !terminal(r.View.Status) && (ownRun != r.View.ID || !r.Body.AnalysisStarted || r.View.Status != Running) {
				return ErrConflict
			}
			candidates = append(candidates, c)
		}
		// 超证据上限时优雅截断:选最有价值的 ≤Calls 条,其余计入 Omitted 由汇总注明,
		// 不再因调用数超限整轮失败。CallIDs 落选中集,与 Items 一致。
		selected := selectAnalysisCalls(candidates, resourceLimits.Calls)
		out.Omitted = len(candidates) - len(selected)
		// 预览确认/执行重放只带截断后的 CallIDs,此时由调用方回传预览时的
		// Omitted;它一并进 Hash,既不能伪造省略数,也与 ID 顺序无关。
		if len(expectedOmitted) == 1 {
			out.Omitted = expectedOmitted[0]
		}
		size := 0
		for _, c := range selected {
			out.CallIDs = append(out.CallIDs, c.View.ID)
			targetHashes[c.View.ServerID] = c.View.TargetHash
			alias, ok := aliases[c.View.ServerID]
			if !ok {
				alias = "target_" + string(rune('A'+len(aliases)))
				aliases[c.View.ServerID] = alias
			}
			item := AnalysisItem{CallID: c.View.ID, TargetAlias: alias, ToolID: c.View.ToolID, Output: c.View.Output, Error: c.View.Error, Status: c.View.Status, Truncated: c.View.Truncated, CollectedAt: c.View.CollectedAt, ExitCode: c.View.ExitCode}
			if c.View.Resources != nil {
				encoded, _ := json.Marshal(c.View.Resources)
				item.Output = string(encoded)
			}
			item.Output = s.scrub(item.Output, 64<<10)
			item.Error = s.scrub(item.Error, 1024)
			encoded, _ := json.Marshal(item)
			size += len(encoded)
			if size > 30<<10 {
				return ErrQuota
			}
			out.Items = append(out.Items, item)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for server, hash := range targetHashes {
		if s.executor == nil {
			return nil, ErrUnavailable
		}
		snap, err := s.executor.Capture(ctx, server)
		if err != nil || snap.Fingerprint() != hash {
			return nil, ErrConsent
		}
	}
	out.Hash = digest(struct {
		SessionID string
		CallIDs   []string
		Items     []AnalysisItem
		Provider  ProviderInfo
		Omitted   int `json:"omitted,omitempty"`
	}{id, out.CallIDs, out.Items, out.Provider, out.Omitted})
	return out, nil
}
func (s *Service) Analyze(ctx context.Context, id string, in AnalysisInput) (*Run, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if (!in.Consent && !in.AnalyzeResults) || !validID(in.ClientRequestID) {
		return nil, ErrConsent
	}
	hash := digest(in)
	if old, err := s.duplicate(ctx, id, in.ClientRequestID, hash); old != nil || err != nil {
		return old, err
	}
	if err := s.registerSecrets(); err != nil {
		return nil, err
	}
	ids := in.CallIDs
	question := ""
	if in.RunID != "" {
		if len(ids) > 0 {
			return nil, ErrInvalid
		}
		r, err := s.run(ctx, s.db, id, in.RunID)
		if err != nil {
			return nil, err
		}
		r, _ = s.projectRun(ctx, s.db, r)
		if !terminal(r.View.Status) {
			return nil, ErrConflict
		}
		question = r.Body.Question
		if question == "" {
			question = r.Body.Turn.Text
		}
		calls, err := s.calls(ctx, s.db, id, in.RunID)
		if err != nil {
			return nil, err
		}
		for _, c := range calls {
			ids = append(ids, c.Body.View.ID)
		}
	} else {
		// Explicit IDs must belong to one completed source turn, never mixed turns.
		source := ""
		for _, cid := range ids {
			c, err := s.call(ctx, s.db, id, cid)
			if err != nil {
				return nil, err
			}
			if source != "" && source != c.Body.View.RunID {
				return nil, ErrInvalid
			}
			source = c.Body.View.RunID
		}
		if source != "" {
			r, err := s.run(ctx, s.db, id, source)
			if err != nil {
				return nil, err
			}
			question = r.Body.Question
			if question == "" {
				question = r.Body.Turn.Text
			}
		}
	}
	var expectedOmitted []int
	if in.PreviewHash != "" {
		expectedOmitted = append(expectedOmitted, in.Omitted)
	}
	preview, err := s.analysisPreview(ctx, id, ids, "", expectedOmitted...)
	if err != nil {
		return nil, err
	}
	if (in.PreviewHash != "" && preview.Hash != in.PreviewHash) || preview.Provider != in.Provider || (!in.AnalyzeResults && in.PreviewHash == "") {
		return nil, ErrConsent
	}
	preview.Items = nil // Only references and permission persist; result text has a single owner.
	return s.enqueue(ctx, id, in.ClientRequestID, hash, runBody{Analysis: preview, Provider: preview.Provider, Question: question}, in.Revision)
}

type RejectInput struct {
	RunID  string      `json:"runId"`
	Nonce  string      `json:"nonce"`
	Calls  []BoundCall `json:"calls"`
	Reason string      `json:"reason,omitempty"`
}

// Reject 在确认卡上拒绝执行:该批调用置 Rejected 终态(可选原因,≤512B,脱敏),
// approval 置 rejected;run 重排队续 Agent 循环,拒绝结果回流模型。
func (s *Service) Reject(ctx context.Context, id string, in RejectInput) (*Run, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if !utf8.ValidString(in.Reason) || len(in.Reason) > 512 {
		return nil, ErrInvalid
	}
	reason := s.scrub(in.Reason, 512)
	if len(in.Calls) < 1 || !validID(in.Nonce) {
		return nil, ErrApproval
	}
	err := s.transaction(ctx, id, true, func(tx *sql.Tx) error {
		r, e := s.run(ctx, tx, id, in.RunID)
		if e != nil {
			return e
		}
		if r.View.Status != AwaitingConfirmation || r.View.CancelRequested {
			return ErrApproval
		}
		current, e := s.approval(ctx, tx, id, in.RunID)
		if e != nil {
			return e
		}
		if !current.View.Valid || current.Body.Nonce != in.Nonce || digest(s.publicConfirmation(current.View).Calls) != digest(in.Calls) {
			return ErrApproval
		}
		if _, e = tx.ExecContext(ctx, "UPDATE ops_chat_approvals SET status='rejected' WHERE id=? AND status='pending'", current.ID); e != nil {
			return ErrStorage
		}
		cs, e := s.calls(ctx, tx, id, in.RunID)
		if e != nil {
			return e
		}
		for _, c := range cs {
			if c.Body.View.Status == AwaitingConfirmation {
				c.Body.View.Status = Rejected
				c.Body.DenialReason = reason
				if e = s.saveCall(ctx, tx, c); e != nil {
					return e
				}
				if e = s.append(ctx, tx, Entry{SessionID: id, RunID: in.RunID, CallID: c.Body.View.ID, Kind: "call_status", Status: Rejected}, false, false); e != nil {
					return e
				}
			}
		}
		r.View.Status = Queued
		if e = s.saveRun(ctx, tx, r); e != nil {
			return e
		}
		return s.append(ctx, tx, Entry{SessionID: id, RunID: in.RunID, Kind: "run_status", Status: Queued}, false, false)
	})
	if err != nil {
		return nil, err
	}
	log.Printf("[opschat] confirmation rejected session=%s run=%s reason_len=%d", id, in.RunID, len(reason))
	s.notify(id)
	s.launch(id, in.RunID)
	r, err := s.run(ctx, s.db, id, in.RunID)
	if err != nil {
		return nil, err
	}
	r, _ = s.projectRun(ctx, s.db, r)
	return &r.View, nil
}

type ContinueInput struct {
	Advice string `json:"advice,omitempty"`
}

// Continue 在轮次预算用尽后授权续跑:RoundBudget += MaxAgentRounds、预算窗口顺延,
// 可附 ≤8KB 建议(脱敏后作为可见 user entry 进入续跑上下文)。
func (s *Service) Continue(ctx context.Context, id, run string, in ContinueInput) (*Run, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if !utf8.ValidString(in.Advice) || len(in.Advice) > resourceLimits.MessageBytes {
		return nil, ErrInvalid
	}
	advice := s.scrub(in.Advice, resourceLimits.MessageBytes)
	err := s.transaction(ctx, id, true, func(tx *sql.Tx) error {
		r, e := s.run(ctx, tx, id, run)
		if e != nil {
			return e
		}
		if r.View.Status != AwaitingContinuation || r.View.CancelRequested {
			return ErrConflict
		}
		r.View.Status = Queued
		r.Body.RoundBudget += s.runtime.MaxAgentRounds
		r.Body.ActiveMillis -= int64(s.runtime.WorkBudget) // 授予新的预算窗口
		if e = s.saveRun(ctx, tx, r); e != nil {
			return e
		}
		if advice != "" {
			if e = s.append(ctx, tx, Entry{SessionID: id, RunID: run, Kind: "user", Text: advice}, true, true); e != nil {
				return e
			}
		}
		return s.append(ctx, tx, Entry{SessionID: id, RunID: run, Kind: "run_status", Status: Queued}, false, false)
	})
	if err != nil {
		return nil, err
	}
	log.Printf("[opschat] continuation granted session=%s run=%s advice_len=%d", id, run, len(advice))
	s.notify(id)
	s.launch(id, run)
	r, err := s.run(ctx, s.db, id, run)
	if err != nil {
		return nil, err
	}
	r, _ = s.projectRun(ctx, s.db, r)
	return &r.View, nil
}
