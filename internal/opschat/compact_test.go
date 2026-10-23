package opschat

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/mask"
	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// ─── 策略数学 ─────────────────────────────────────────────────────────────────

func TestContextPolicyDefaults(t *testing.T) {
	p := ContextPolicy{}.withDefaults()
	if p.WindowTokens != 200_000 || p.MaxOutputTokens != 8_192 || p.HistoryMessages != 100 ||
		p.HistoryBytes != 256<<10 || p.KeepRecentTokens != 15_000 {
		t.Fatalf("defaults: %+v", p)
	}
	if p.AutoCompact == nil || !*p.AutoCompact {
		t.Fatalf("autocompact must default on: %+v", p.AutoCompact)
	}
	// 默认预留:200K 窗口 → max(20K, 16K) = 20K;阈值 180K。
	if p.ReserveTokens() != 20_000 || p.Ceiling() != 180_000 {
		t.Fatalf("reserve=%d ceiling=%d", p.ReserveTokens(), p.Ceiling())
	}
	// 小窗口(32K 以下):预留 = 10% 窗口。
	small := ContextPolicy{WindowTokens: 20_000}.withDefaults()
	if small.ReserveTokens() != 2_000 || small.Ceiling() != 18_000 {
		t.Fatalf("small reserve=%d ceiling=%d", small.ReserveTokens(), small.Ceiling())
	}
	// 32K 以上窗口:预留至少 16K。
	mid := ContextPolicy{WindowTokens: 100_000, BufferTokens: 4_000}.withDefaults()
	if mid.ReserveTokens() != 4_000 {
		t.Fatalf("explicit buffer must win: %d", mid.ReserveTokens())
	}
	// 默认 200K 窗口 × 6 = 1.2M,被 2MB 下限抬到 2MB;500K 窗口 × 6 = 3M 生效。
	if p.MaxRequestBytes() != 2<<20 {
		t.Fatalf("request cap floor: %d", p.MaxRequestBytes())
	}
	if got := (ContextPolicy{WindowTokens: 500_000}).MaxRequestBytes(); got != 3_000_000 {
		t.Fatalf("request cap: %d", got)
	}
	// keep 不得超过窗口一半。
	big := ContextPolicy{WindowTokens: 100_000, KeepRecentTokens: 90_000}.withDefaults()
	if big.KeepRecentTokens != 50_000 {
		t.Fatalf("keep clamp: %d", big.KeepRecentTokens)
	}
}

// ─── 估算与切分 ───────────────────────────────────────────────────────────────

func TestEstimateRequestTokens(t *testing.T) {
	base := AgentRequest{System: "sys", Question: "q"}
	n0 := estimateRequestTokens(base)
	if n0 == 0 {
		t.Fatal("baseline estimate is zero")
	}
	base.History = []Message{{Role: "user", Text: strings.Repeat("a", 400)}}
	if n1 := estimateRequestTokens(base); n1 <= n0 {
		t.Fatalf("history not counted: %d → %d", n0, n1)
	}
	base.Rounds = []AgentExchange{{
		Calls:   []AgentToolCall{{ID: "c1", ToolID: "docker_logs", Args: []byte(`{"container":"app"}`)}},
		Results: []AgentToolResult{{CallID: "c1", Output: strings.Repeat("x", 800)}},
	}}
	if n2 := estimateRequestTokens(base); n2 <= n0 {
		t.Fatalf("rounds not counted: %d → %d", n0, n2)
	}
}

func TestSplitKeepRecent(t *testing.T) {
	mk := func(role, text string) Message { return Message{Role: role, Text: text} }
	fill := func(n int) string { return strings.Repeat("a", n) }
	// user/assistant 交替;keep 设小,强制切分(每条 ~254 tokens)。
	msgs := []Message{
		mk("user", fill(1000)),
		mk("assistant", fill(1000)),
		mk("user", fill(1000)),
		mk("assistant", fill(1000)),
		mk("user", fill(1000)),
		mk("assistant", fill(1000)),
	}
	recent, older := splitKeepRecent(msgs, 300)
	if len(recent) == 0 || len(older) == 0 {
		t.Fatalf("no split: recent=%d older=%d", len(recent), len(older))
	}
	if recent[0].Role != "user" {
		t.Fatalf("recent must start at a user boundary, got %q", recent[0].Role)
	}
	if len(recent)+len(older) != len(msgs) {
		t.Fatalf("lost messages: %d + %d != %d", len(recent), len(older), len(msgs))
	}
	// keep 覆盖全部消息:older 为空。
	recent2, older2 := splitKeepRecent(msgs, 100_000)
	if len(older2) != 0 || len(recent2) != len(msgs) {
		t.Fatalf("no-op split broken: %d/%d", len(recent2), len(older2))
	}
	// 只有首条 user、其余 assistant:边界回退到 0 → 整体划入 older。
	solo := []Message{mk("user", fill(1000)), mk("assistant", fill(1000)), mk("assistant", fill(1000))}
	recent3, older3 := splitKeepRecent(solo, 1)
	if len(older3) != len(solo) || len(recent3) != 0 {
		t.Fatalf("full compact split broken: %d/%d", len(recent3), len(older3))
	}
}

func TestTrimRounds(t *testing.T) {
	policy := ContextPolicy{WindowTokens: 2_000, KeepRecentTokens: 100, BufferTokens: 0}.withDefaults()
	req := &AgentRequest{
		System: "sys",
		Rounds: []AgentExchange{},
	}
	for i := 0; i < 6; i++ {
		req.Rounds = append(req.Rounds, AgentExchange{
			Calls:   []AgentToolCall{{ID: "c", ToolID: "docker_logs", Args: []byte(`{"container":"app","lines":200}`)}},
			Results: []AgentToolResult{{CallID: "c", Output: strings.Repeat("r", 2000)}},
		})
	}
	before := len(req.Rounds)
	trimRounds(req, policy, "auto")
	if len(req.Rounds) >= before {
		t.Fatalf("rounds not trimmed: %d", len(req.Rounds))
	}
	if len(req.Rounds) == 0 {
		t.Fatal("last round must be kept")
	}
	if !strings.Contains(req.System, "Context budget note") {
		t.Fatal("omission note missing from system prompt")
	}
}

// ─── 服务级:策略注入 + checkpoint 起读 + 压缩 ───────────────────────────────

// policyService 构造带自定义上下文策略的服务(其余同 newService);serverIDs
// 同时喂给 fakeExecutor(Submit 要求会话已绑定目标机)。
func policyService(t *testing.T, m Model, policy ContextPolicy, serverIDs ...string) (*Service, *sql.DB) {
	t.Helper()
	db := storetest.OpenDB(t)
	key := &[32]byte{1}
	v := vault.New(db, key)
	masker := mask.NewMasker()
	s, err := New(Options{DB: db, Vault: v, Executor: executor(serverIDs...), Model: m,
		LocalRecorder: audit.New(db, masker, nil), Masker: masker,
		Context: func() ContextPolicy { return policy }})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s, db
}

func appendEntry(t *testing.T, s *Service, sessionID, kind, text string, allowed bool) {
	t.Helper()
	if err := s.persist(sessionID, func(c context.Context, tx *sql.Tx) error {
		return s.append(c, tx, Entry{SessionID: sessionID, Kind: kind, Text: text}, true, allowed)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestContextMessagesPolicyAndCheckpoint(t *testing.T) {
	m := model()
	policy := ContextPolicy{HistoryMessages: 3, HistoryBytes: 4 << 10}.withDefaults()
	s, _ := policyService(t, m, policy)
	c := chat(t, s)
	appendEntry(t, s, c.ID, "user", strings.Repeat("u", 500), true)
	appendEntry(t, s, c.ID, "assistant", strings.Repeat("a", 500), true)
	appendEntry(t, s, c.ID, "summary_checkpoint", "old summary text", true)
	appendEntry(t, s, c.ID, "user", "after-checkpoint-question", true)
	appendEntry(t, s, c.ID, "assistant", "after-checkpoint-answer", true)
	appendEntry(t, s, c.ID, "user", "blocked-from-context", false)
	msgs, err := s.contextMessages(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	// checkpoint 起读:前两条(压缩前)被摘要取代;context_allowed=0 的不进上下文。
	if len(msgs) != 3 {
		t.Fatalf("msgs = %d, want 3 (checkpoint + user + assistant): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || !strings.Contains(msgs[0].Text, "<conversation-checkpoint>") || !strings.Contains(msgs[0].Text, "old summary text") {
		t.Fatalf("checkpoint not mapped to wrapped user message: %+v", msgs[0])
	}
	if msgs[1].Text != "after-checkpoint-question" || msgs[2].Text != "after-checkpoint-answer" {
		t.Fatalf("post-checkpoint order wrong: %+v", msgs)
	}
}

// compacterModel 扩展 fakeModel:记录摘要请求,可注入失败。
type compacterModel struct {
	*fakeModel
	summaries []SummarizeRequest
	reply     string
	fail      error
	mu2       sync.Mutex
}

func (m *compacterModel) Summarize(ctx context.Context, in SummarizeRequest) (string, error) {
	m.mu2.Lock()
	defer m.mu2.Unlock()
	m.summaries = append(m.summaries, in)
	if m.fail != nil {
		return "", m.fail
	}
	return m.reply, nil
}

func TestMaybeCompactWritesCheckpoint(t *testing.T) {
	base := model()
	m := &compacterModel{fakeModel: base, reply: "## 目标\n- check nginx status"}
	on := true
	s, db := policyService(t, m, ContextPolicy{
		WindowTokens: 4_000, KeepRecentTokens: 100, AutoCompact: &on,
	}.withDefaults())
	c := chat(t, s)
	// 单条消息进上下文前被 8KB scrub 截断,用多条大消息堆过阈值(3×8000 字符 ≈ 6000 tokens)。
	for i := 0; i < 3; i++ {
		appendEntry(t, s, c.ID, "user", strings.Repeat("u", 8000), true)
		appendEntry(t, s, c.ID, "assistant", strings.Repeat("a", 100), true)
	}
	appendEntry(t, s, c.ID, "user", "latest question", true)
	msgs, err := s.contextMessages(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	req := &AgentRequest{Provider: m.Info(), System: strings.Repeat("s", 1000), Question: "q", History: msgs}
	used := estimateRequestTokens(*req)
	if used < s.policy().Ceiling() {
		t.Fatalf("test setup: estimate %d below ceiling %d", used, s.policy().Ceiling())
	}
	if !s.maybeCompact(context.Background(), c.ID, req, "auto") {
		t.Fatal("compaction did not run")
	}
	m.mu2.Lock()
	n := len(m.summaries)
	m.mu2.Unlock()
	if n != 1 {
		t.Fatalf("summarize calls = %d", n)
	}
	// checkpoint 落库且为后续上下文的起点。
	var kinds []string
	if err = db.QueryRow("SELECT 1").Scan(new(int)); err != nil { // conn warmup
		t.Fatal(err)
	}
	rows, err := db.Query("SELECT kind FROM ops_chat_entries WHERE session_id=? ORDER BY seq", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		kinds = append(kinds, k)
	}
	rows.Close()
	found := false
	for _, k := range kinds {
		if k == KindCheckpoint {
			found = true
		}
	}
	if !found {
		t.Fatalf("checkpoint entry missing: %v", kinds)
	}
	// 请求历史被改写:checkpoint(user) 开头 + 保留段。
	if len(req.History) == 0 || req.History[0].Role != "user" || !strings.Contains(req.History[0].Text, "<conversation-checkpoint>") {
		t.Fatalf("request history not rewritten: %+v", req.History)
	}
	// 下一轮 contextMessages 从 checkpoint 起读。
	next, err := s.contextMessages(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) == 0 || !strings.Contains(next[0].Text, "<conversation-checkpoint>") {
		t.Fatalf("next round context must start at checkpoint: %+v", next)
	}
}

func TestCompactFailureBreaker(t *testing.T) {
	base := model()
	m := &compacterModel{fakeModel: base, fail: errors.New("summarizer down")}
	on := true
	s, _ := policyService(t, m, ContextPolicy{WindowTokens: 4_000, KeepRecentTokens: 100, AutoCompact: &on}.withDefaults())
	c := chat(t, s)
	for i := 0; i < 3; i++ {
		appendEntry(t, s, c.ID, "user", strings.Repeat("u", 8000), true)
		appendEntry(t, s, c.ID, "assistant", "r", true)
	}
	appendEntry(t, s, c.ID, "user", "q", true)
	msgs, _ := s.contextMessages(context.Background(), c.ID)
	build := func() *AgentRequest {
		return &AgentRequest{Provider: m.Info(), System: strings.Repeat("s", 1000), Question: "q", History: msgs}
	}
	// 三次失败后熔断:第四次不再调模型,但确定性裁剪(history 换不成 rounds,
	// 这里验证 maybeCompact 不再尝试且不 panic)。
	for i := 0; i < 3; i++ {
		req := build()
		s.maybeCompact(context.Background(), c.ID, req, "auto")
	}
	if !s.compactBroken(c.ID) {
		t.Fatal("breaker not open after 3 failures")
	}
	req := build()
	s.maybeCompact(context.Background(), c.ID, req, "auto")
	m.mu2.Lock()
	n := len(m.summaries)
	m.mu2.Unlock()
	if n != 3 {
		t.Fatalf("summarize called after breaker: %d", n)
	}
}

// overflowModel 先返回一次溢出错误,之后给终答;同时记录请求。
type overflowModel struct {
	*fakeModel
	overflowErr error
	overflows   int
	retried     *AgentRequest
}

func (m *overflowModel) AgentRound(ctx context.Context, in AgentRequest) (AgentReply, error) {
	if m.overflows > 0 {
		m.overflows--
		return AgentReply{}, m.overflowErr
	}
	m.retried = &in
	return AgentReply{Text: "recovered answer"}, nil
}
func (m *overflowModel) IsContextOverflow(err error) bool { return errors.Is(err, m.overflowErr) }

func TestOverflowRetryRecoversRun(t *testing.T) {
	base := model()
	m := &overflowModel{fakeModel: base, overflowErr: opsOverflowErr, overflows: 1}
	serverID := uuid.NewString()
	s, _ := policyService(t, m, ContextPolicy{WindowTokens: 60_000}.withDefaults(), serverID)
	c := chat(t, s, serverID)
	r, err := s.Submit(context.Background(), c.ID, TurnInput{ClientRequestID: uuid.NewString(), Revision: c.Revision, Text: "check status"})
	if err != nil {
		t.Fatal(err)
	}
	snap := waitRun(t, s, c.ID, r.ID, Succeeded)
	var answer string
	for _, e := range snap.Entries.Entries {
		if e.Kind == "assistant" && e.Text != "" {
			answer = e.Text
		}
	}
	if answer != "recovered answer" {
		t.Fatalf("overflow retry did not recover: %q", answer)
	}
}

// opsOverflowErr 模拟 provider 溢出错误(独立哨兵;opschat 只经接口判定)。
var opsOverflowErr = errors.New("fake context_length_exceeded")

func TestUsageRecordedAndProjected(t *testing.T) {
	base := model()
	usage := &AgentUsage{InputTokens: 12_345, OutputTokens: 678}
	base.mu.Lock()
	base.queue = append(base.queue, AgentReply{Text: "final answer", Usage: usage})
	base.mu.Unlock()
	serverID := uuid.NewString()
	s, _ := policyService(t, base, ContextPolicy{WindowTokens: 60_000}.withDefaults(), serverID)
	c := chat(t, s, serverID)
	r, err := s.Submit(context.Background(), c.ID, TurnInput{ClientRequestID: uuid.NewString(), Revision: c.Revision, Text: "check status"})
	if err != nil {
		t.Fatal(err)
	}
	snap := waitRun(t, s, c.ID, r.ID, Succeeded)
	var seen *AgentUsage
	for _, e := range snap.Entries.Entries {
		if e.Kind == "assistant" {
			seen = e.Usage
		}
	}
	if seen == nil || seen.InputTokens != 12_345 || seen.OutputTokens != 678 {
		t.Fatalf("assistant entry usage not persisted: %+v", seen)
	}
	// 会话投影:percent = ceil((12345+678)/60000) = 22。
	got, err := s.Get(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContextUsage == nil || got.ContextUsage.UsedTokens != 13_023 || got.ContextUsage.WindowTokens != 60_000 || got.ContextUsage.Percent != 22 {
		t.Fatalf("context usage projection wrong: %+v", got.ContextUsage)
	}
}
