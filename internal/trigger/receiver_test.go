package trigger

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/vault"
)

// fakeRunCreator 记录被请求创建的运行(不触 run 包,避免领域互引)。
type fakeRunCreator struct {
	mu    sync.Mutex
	calls []RunRequest
	id    string
}

func (f *fakeRunCreator) CreateWebhookRun(_ context.Context, _ string, in RunRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	if f.id == "" {
		f.id = "run-1"
	}
	return f.id, nil
}

func (f *fakeRunCreator) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// newReceiver 装配 Receiver + 已配 push 事件 + 分支过滤 [main, release/*] 的项目;返回 token、明文密钥。
func newReceiver(t *testing.T) (*Receiver, *fakeRunCreator, string, string) {
	t.Helper()
	db, _ := testDB(t)
	v := vault.New(db, testMasterKey())
	svc := New(db, v)
	projID := seedProject(t, db)

	ctx := context.Background()
	cfg, err := svc.Get(ctx, projID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	token := cfg.WebhookToken

	reset, err := svc.ResetSecret(ctx, projID)
	if err != nil {
		t.Fatalf("reset secret: %v", err)
	}
	secret := reset.Secret

	if _, err := svc.Save(ctx, projID, SaveInput{
		Events:        Events{Push: true},
		BranchFilters: []string{"main", "release/*"},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	fake := &fakeRunCreator{}
	rc := NewReceiver(db, v, fake)
	return rc, fake, token, secret
}

func pushBody(branch, commit string) []byte {
	return []byte(`{"ref":"refs/heads/` + branch + `","after":"` + commit + `"}`)
}

func TestWebhookPasswordModeCreatesRun(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	res, err := rc.Handle(context.Background(), Delivery{
		Token:      token,
		Event:      eventPush,
		TokenHdr:   secret,
		DeliveryID: "d-1",
		RawBody:    pushBody("main", "abc123"),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Accepted || res.RunID == "" {
		t.Fatalf("expected accepted run, got %+v", res)
	}
	if fake.count() != 1 {
		t.Fatalf("expected 1 run created, got %d", fake.count())
	}
	got := fake.calls[0]
	if got.Branch != "main" || got.Commit != "abc123" {
		t.Fatalf("unexpected run req: %+v", got)
	}
	if got.Actor != "gitee" {
		t.Fatalf("expected actor gitee, got %q", got.Actor)
	}
}

func TestWebhookSignatureModeCreatesRun(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	// 新鲜 timestamp(秒 epoch),落在防重放窗口内。
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	res, err := rc.Handle(context.Background(), Delivery{
		Token:      token,
		Event:      eventPush,
		TokenHdr:   sig,
		Timestamp:  ts,
		DeliveryID: "d-sig",
		RawBody:    pushBody("main", "deadbeef"),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Accepted {
		t.Fatalf("expected accepted, got %+v", res)
	}
	if fake.count() != 1 {
		t.Fatalf("expected 1 run, got %d", fake.count())
	}
}

// TestWebhookSignatureReplayRejected 验证防重放:旧 timestamp(超窗)即便签名正确也 401。
func TestWebhookSignatureReplayRejected(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	ts := "1700000000" // 2023,远超 5min 窗口
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	_, err := rc.Handle(context.Background(), Delivery{
		Token:      token,
		Event:      eventPush,
		TokenHdr:   sig,
		Timestamp:  ts,
		DeliveryID: "d-replay",
		RawBody:    pushBody("main", "deadbeef"),
	})
	if err != ErrUnauthorized {
		t.Fatalf("旧 timestamp 应 401(防重放), got %v", err)
	}
	if fake.count() != 0 {
		t.Fatalf("重放不应创建运行, got %d", fake.count())
	}
}

// TestWebhookSignatureMissingTimestampRejected 验证签名模式缺失 timestamp → 401。
func TestWebhookSignatureMissingTimestampRejected(t *testing.T) {
	rc, _, token, secret := newReceiver(t)
	// 用空 timestamp 计算的签名(攻击者可构造),但缺失 timestamp 头本身应被拒。
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("\n" + secret))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	_, err := rc.Handle(context.Background(), Delivery{
		Token:      token,
		Event:      eventPush,
		TokenHdr:   sig,
		Timestamp:  "",
		DeliveryID: "d-no-ts",
		RawBody:    pushBody("main", "deadbeef"),
	})
	if err != ErrUnauthorized {
		t.Fatalf("签名模式缺 timestamp 应 401, got %v", err)
	}
}

// TestWebhookTimestampMillisAccepted 验证毫秒 epoch 也被正确解析为新鲜。
func TestWebhookTimestampMillisAccepted(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10) // 13 位毫秒
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	res, err := rc.Handle(context.Background(), Delivery{
		Token:      token,
		Event:      eventPush,
		TokenHdr:   sig,
		Timestamp:  ts,
		DeliveryID: "d-ms",
		RawBody:    pushBody("main", "deadbeef"),
	})
	if err != nil {
		t.Fatalf("Handle(ms ts): %v", err)
	}
	if !res.Accepted || fake.count() != 1 {
		t.Fatalf("毫秒 timestamp 应被接受, res=%+v count=%d", res, fake.count())
	}
}

func TestWebhookWrongSecretUnauthorized(t *testing.T) {
	rc, fake, token, _ := newReceiver(t)
	_, err := rc.Handle(context.Background(), Delivery{
		Token:      token,
		Event:      eventPush,
		TokenHdr:   "whsec_wrong",
		DeliveryID: "d-bad",
		RawBody:    pushBody("main", "x"),
	})
	if err != ErrUnauthorized {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
	if fake.count() != 0 {
		t.Fatalf("expected no run on bad secret, got %d", fake.count())
	}
}

// ---- Gitea / Gogs 兼容 ----------------------------------------------------

// giteaBodySig 按 Gitea/Gogs 协议计算签名:hex(HMAC-SHA256(key=secret, msg=body))。
func giteaBodySig(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// TestWebhookGiteaBodySignatureCreatesRun 验证 Gitea/Gogs body 签名模式
// (Signature 头 = hex HMAC over RawBody,协议不带 timestamp)创建运行且 actor=gitea。
func TestWebhookGiteaBodySignatureCreatesRun(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	body := pushBody("main", "cafe01")

	res, err := rc.Handle(context.Background(), Delivery{
		Token:         token,
		Event:         eventGiteaPush,
		BodySignature: giteaBodySig(secret, body),
		DeliveryID:    "gitea-d-1",
		Actor:         "gitea",
		RawBody:       body,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Accepted || res.RunID == "" {
		t.Fatalf("expected accepted run, got %+v", res)
	}
	if fake.count() != 1 {
		t.Fatalf("expected 1 run created, got %d", fake.count())
	}
	if got := fake.calls[0]; got.Branch != "main" || got.Commit != "cafe01" || got.Actor != "gitea" {
		t.Fatalf("unexpected run req: %+v", got)
	}
}

// TestWebhookGiteaWrongBodySignatureUnauthorized 验证 body 签名不匹配 → 401。
func TestWebhookGiteaWrongBodySignatureUnauthorized(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	body := pushBody("main", "cafe01")
	sig := giteaBodySig(secret, body)
	tampered := "0" + sig[1:]
	if tampered == sig {
		tampered = "1" + sig[1:]
	}

	_, err := rc.Handle(context.Background(), Delivery{
		Token:         token,
		Event:         eventGiteaPush,
		BodySignature: tampered,
		DeliveryID:    "gitea-d-bad",
		RawBody:       body,
	})
	if err != ErrUnauthorized {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
	if fake.count() != 0 {
		t.Fatalf("expected no run on bad signature, got %d", fake.count())
	}
}

// TestWebhookGiteaEventNames 验证 Gitea/Gogs 小写事件名映射到对应订阅开关。
func TestWebhookGiteaEventNames(t *testing.T) {
	all := Events{Push: true, Tag: true, PullRequest: true, Release: true}
	cases := []struct {
		event string
		ev    Events
		want  bool
	}{
		{eventGiteaPush, Events{Push: true}, true},
		{eventGiteaPush, Events{}, false},
		{eventGiteaCreate, Events{Tag: true}, true},
		{eventGiteaCreate, Events{Push: true}, false},
		{eventGiteaPullRequest, Events{PullRequest: true}, true},
		{eventGiteaReleaseEvent, Events{Release: true}, true},
		{"issues", all, false},                // 未支持的事件一律不订阅
		{eventPush, Events{Push: true}, true}, // Gitee 取值不受影响
	}
	for _, c := range cases {
		if got := eventSubscribed(c.event, c.ev); got != c.want {
			t.Errorf("eventSubscribed(%q, %+v) = %v, want %v", c.event, c.ev, got, c.want)
		}
	}
}

// TestWebhookGiteaTagCreateEvent 验证 Gitea create(新建 tag)事件在仅订阅 Tag 时触发;
// 引用名取 payload 的 ref(tag 名,无 refs/tags/ 前缀),commit 取 sha 字段。
func TestWebhookGiteaTagCreateEvent(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	// newReceiver 固定订阅 Push + 分支过滤 [main, release/*];改为仅 Tag、清空分支过滤。
	if _, err := rc.db.Exec(`UPDATE pipeline_triggers SET events_json = ?`, `{"tag":true}`); err != nil {
		t.Fatalf("update events: %v", err)
	}
	if _, err := rc.db.Exec(`UPDATE pipeline_triggers SET branch_filters_json = '[]'`); err != nil {
		t.Fatalf("clear branch filters: %v", err)
	}
	body := []byte(`{"ref":"v1.2.0","ref_type":"tag","sha":"tagsha9"}`)

	res, err := rc.Handle(context.Background(), Delivery{
		Token:         token,
		Event:         eventGiteaCreate,
		BodySignature: giteaBodySig(secret, body),
		DeliveryID:    "gitea-d-create",
		Actor:         "gitea",
		RawBody:       body,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Accepted {
		t.Fatalf("expected accepted, got %+v", res)
	}
	if got := fake.calls[0]; got.Branch != "v1.2.0" || got.Commit != "tagsha9" {
		t.Fatalf("expected branch v1.2.0 / commit tagsha9, got %+v", got)
	}
}

// ---- GitHub 兼容 -----------------------------------------------------------

// githubBodySig 按 GitHub 协议计算签名:sha256=<hex(HMAC-SHA256(key=secret, msg=body))>。
func githubBodySig(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// TestWebhookGitHubBodySignatureCreatesRun 验证 GitHub 签名模式(X-Hub-Signature-256,
// "sha256=" 前缀 + hex HMAC over RawBody,协议不带 timestamp)创建运行且 actor=github;
// 事件名 push 与 Gitea/Gogs 取值一致,沿用同一订阅映射。
func TestWebhookGitHubBodySignatureCreatesRun(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	body := pushBody("main", "ghbeef")

	res, err := rc.Handle(context.Background(), Delivery{
		Token:         token,
		Event:         eventGiteaPush, // GitHub 取值与 Gitea 一致:push
		BodySignature: githubBodySig(secret, body),
		DeliveryID:    "gh-d-1",
		Actor:         "github",
		RawBody:       body,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Accepted || res.RunID == "" {
		t.Fatalf("expected accepted run, got %+v", res)
	}
	if fake.count() != 1 {
		t.Fatalf("expected 1 run created, got %d", fake.count())
	}
	if got := fake.calls[0]; got.Branch != "main" || got.Commit != "ghbeef" || got.Actor != "github" {
		t.Fatalf("unexpected run req: %+v", got)
	}
}

// TestWebhookGitHubWrongSignatureUnauthorized 验证 GitHub 签名不匹配(前缀正确但
// hex 部分被篡改)→ 401,不创建。
func TestWebhookGitHubWrongSignatureUnauthorized(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	body := pushBody("main", "ghbeef")
	sig := githubBodySig(secret, body)
	tampered := "sha256=" + "0" + sig[len("sha256=")+1:]
	if tampered == sig {
		tampered = "sha256=" + "1" + sig[len("sha256=")+1:]
	}

	_, err := rc.Handle(context.Background(), Delivery{
		Token:         token,
		Event:         eventGiteaPush,
		BodySignature: tampered,
		DeliveryID:    "gh-d-bad",
		Actor:         "github",
		RawBody:       body,
	})
	if err != ErrUnauthorized {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
	if fake.count() != 0 {
		t.Fatalf("expected no run on bad signature, got %d", fake.count())
	}
}

// TestWebhookGitHubCreateEvent 验证 GitHub create(新建 tag)事件在仅订阅 Tag 时触发;
// GitHub create 载荷不含提交字段(无 sha)→ commit 留空,运行按引用名取 head。
func TestWebhookGitHubCreateEvent(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	if _, err := rc.db.Exec(`UPDATE pipeline_triggers SET events_json = ?`, `{"tag":true}`); err != nil {
		t.Fatalf("update events: %v", err)
	}
	if _, err := rc.db.Exec(`UPDATE pipeline_triggers SET branch_filters_json = '[]'`); err != nil {
		t.Fatalf("clear branch filters: %v", err)
	}
	body := []byte(`{"ref":"v3.0.0","ref_type":"tag"}`)

	res, err := rc.Handle(context.Background(), Delivery{
		Token:         token,
		Event:         eventGiteaCreate, // GitHub 取值与 Gitea 一致:create
		BodySignature: githubBodySig(secret, body),
		DeliveryID:    "gh-d-create",
		Actor:         "github",
		RawBody:       body,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Accepted {
		t.Fatalf("expected accepted, got %+v", res)
	}
	if got := fake.calls[0]; got.Branch != "v3.0.0" || got.Commit != "" {
		t.Fatalf("expected branch v3.0.0 / empty commit, got %+v", got)
	}
}

// TestWebhookGitHubPingIgnored 验证 GitHub 建立 webhook 时的 ping 事件
// 不在订阅映射内,按 event_not_subscribed 忽略(不创建运行)。
func TestWebhookGitHubPingIgnored(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	body := []byte(`{"zen":"Keep it logically awesome.","hook_id":1}`)

	res, err := rc.Handle(context.Background(), Delivery{
		Token:         token,
		Event:         "ping",
		BodySignature: githubBodySig(secret, body),
		DeliveryID:    "gh-d-ping",
		Actor:         "github",
		RawBody:       body,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Accepted || res.Ignored != IgnoredEventNotSubscribed {
		t.Fatalf("expected event_not_subscribed for ping, got %+v", res)
	}
	if fake.count() != 0 {
		t.Fatalf("expected no run for ping, got %d", fake.count())
	}
}

func TestWebhookEventNotSubscribed(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	// Tag 未勾选(仅 push 开)。
	res, err := rc.Handle(context.Background(), Delivery{
		Token:      token,
		Event:      eventTag,
		TokenHdr:   secret,
		DeliveryID: "d-tag",
		RawBody:    []byte(`{"ref":"refs/tags/v1","after":"t1"}`),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Accepted || res.Ignored != IgnoredEventNotSubscribed {
		t.Fatalf("expected event_not_subscribed, got %+v", res)
	}
	if fake.count() != 0 {
		t.Fatalf("expected no run, got %d", fake.count())
	}
}

func TestWebhookNoBranchMatchIgnored(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	// dev 不匹配任何过滤 glob(main / release/*)。
	res, err := rc.Handle(context.Background(), Delivery{
		Token:      token,
		Event:      eventPush,
		TokenHdr:   secret,
		DeliveryID: "d-dev",
		RawBody:    pushBody("dev", "c1"),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Accepted || res.Ignored != IgnoredNoBranchMatch {
		t.Fatalf("expected no_branch_match, got %+v", res)
	}
	if fake.count() != 0 {
		t.Fatalf("expected no run, got %d", fake.count())
	}
}

func TestWebhookWildcardBranchMatch(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	res, err := rc.Handle(context.Background(), Delivery{
		Token:      token,
		Event:      eventPush,
		TokenHdr:   secret,
		DeliveryID: "d-rel",
		RawBody:    pushBody("release/1.2", "r12"),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Accepted {
		t.Fatalf("expected accepted for release/1.2, got %+v", res)
	}
	if fake.count() != 1 {
		t.Fatalf("expected 1 run, got %d", fake.count())
	}
}

// TestWebhookEmptyFiltersTriggerAllBranches 验证空过滤列表 = 所有分支触发。
func TestWebhookEmptyFiltersTriggerAllBranches(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	// 清空过滤(空列表放行一切)。
	rc2 := clearBranchFilters(t, rc)
	res, err := rc2.Handle(context.Background(), Delivery{
		Token:      token,
		Event:      eventPush,
		TokenHdr:   secret,
		DeliveryID: "d-any",
		RawBody:    pushBody("feature/anything", "c9"),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Accepted {
		t.Fatalf("expected accepted with empty filters, got %+v", res)
	}
	if fake.count() != 1 {
		t.Fatalf("expected 1 run, got %d", fake.count())
	}
}

func TestWebhookDuplicateDeliveryIgnored(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	d := Delivery{
		Token:      token,
		Event:      eventPush,
		TokenHdr:   secret,
		DeliveryID: "dup-1",
		RawBody:    pushBody("main", "samecommit"),
	}
	first, err := rc.Handle(context.Background(), d)
	if err != nil || !first.Accepted {
		t.Fatalf("first delivery should be accepted: %+v err=%v", first, err)
	}
	second, err := rc.Handle(context.Background(), d)
	if err != nil {
		t.Fatalf("second Handle: %v", err)
	}
	if second.Accepted || second.Ignored != IgnoredDuplicate {
		t.Fatalf("expected duplicate ignored, got %+v", second)
	}
	if fake.count() != 1 {
		t.Fatalf("expected exactly 1 run despite duplicate, got %d", fake.count())
	}
}

// TestDerivedKeyNoColonCollision 验证派生去重键对含 ':' 的分支名不碰撞
// (长度前缀+hash 消歧:不同分段不能产生相同键)。
func TestDerivedKeyNoColonCollision(t *testing.T) {
	// 旧实现 "derived:event:branch:commit" 下,这两组会拼成相同串。
	a := derivedKey("push", "a:b", "c")
	b := derivedKey("push", "a", "b:c")
	if a == b {
		t.Fatalf("含冒号分支名派生键碰撞: %q == %q", a, b)
	}
	// 空 payload(空 branch/commit)与非空也应区分。
	if derivedKey("push", "", "") == derivedKey("push", "x", "") {
		t.Fatalf("空 payload 与非空派生键碰撞")
	}
}

// TestWebhookNoBranchMatchIdempotent 验证分支不匹配路径走同一 dedup claim:
// 同 delivery 重复投递不无界灌库(第二次判 duplicate),只入一行。
func TestWebhookNoBranchMatchIdempotent(t *testing.T) {
	db, _ := testDB(t)
	v := vault.New(db, testMasterKey())
	svc := New(db, v)
	projID := seedProject(t, db)
	ctx := context.Background()
	cfg, err := svc.Get(ctx, projID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	reset, _ := svc.ResetSecret(ctx, projID)
	// Push 订阅 + 仅 main 过滤(feature/x 必不匹配 → no_branch_match)。
	if _, err := svc.Save(ctx, projID, SaveInput{
		Events:        Events{Push: true},
		BranchFilters: []string{"main"},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	rc := NewReceiver(db, v, &fakeRunCreator{})
	d := Delivery{
		Token:      cfg.WebhookToken,
		Event:      eventPush,
		TokenHdr:   reset.Secret,
		DeliveryID: "rec-1",
		RawBody:    pushBody("feature/x", "c1"),
	}
	r1, err := rc.Handle(ctx, d)
	if err != nil || r1.Ignored != IgnoredNoBranchMatch {
		t.Fatalf("first: %+v err=%v, want no_branch_match", r1, err)
	}
	r2, err := rc.Handle(ctx, d)
	if err != nil || r2.Ignored != IgnoredDuplicate {
		t.Fatalf("second(same delivery): %+v err=%v, want duplicate", r2, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(1) FROM webhook_deliveries WHERE project_id = ?`, projID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("同 delivery 应只入 1 行(防无界灌库), got %d", n)
	}
}

// TestWebhookEmptyBranchIgnored 验证畸形 payload(解析不出引用名)即便过滤为空也不触发(防御)。
func TestWebhookEmptyBranchIgnored(t *testing.T) {
	db, _ := testDB(t)
	v := vault.New(db, testMasterKey())
	svc := New(db, v)
	projID := seedProject(t, db)
	ctx := context.Background()
	cfg, err := svc.Get(ctx, projID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	reset, _ := svc.ResetSecret(ctx, projID)
	if _, err := svc.Save(ctx, projID, SaveInput{Events: Events{Push: true}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	rc := NewReceiver(db, v, &fakeRunCreator{})
	res, err := rc.Handle(ctx, Delivery{
		Token:      cfg.WebhookToken,
		Event:      eventPush,
		TokenHdr:   reset.Secret,
		DeliveryID: "d-noref",
		RawBody:    []byte(`{"after":"c0"}`), // 无 ref → 空分支
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Accepted || res.Ignored != IgnoredNoBranchMatch {
		t.Fatalf("expected no_branch_match for empty ref, got %+v", res)
	}
}

// newTagReleaseReceiver 装配 Receiver + 已配 tag+release 事件 + 分支过滤 [v*] 的项目;
// 返回 token、明文密钥(供 tag/release 上下文与订阅用例复用)。
func newTagReleaseReceiver(t *testing.T) (*Receiver, *fakeRunCreator, string, string) {
	t.Helper()
	db, _ := testDB(t)
	v := vault.New(db, testMasterKey())
	svc := New(db, v)
	projID := seedProject(t, db)

	ctx := context.Background()
	cfg, err := svc.Get(ctx, projID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	token := cfg.WebhookToken

	reset, err := svc.ResetSecret(ctx, projID)
	if err != nil {
		t.Fatalf("reset secret: %v", err)
	}
	secret := reset.Secret

	if _, err := svc.Save(ctx, projID, SaveInput{
		Events:        Events{Tag: true, Release: true},
		BranchFilters: []string{"v*"},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	fake := &fakeRunCreator{}
	rc := NewReceiver(db, v, fake)
	return rc, fake, token, secret
}

// TestWebhookTagPushCarriesTagRefAndCommit 验证 Tag Push Hook 命中后:
// 运行携带标签名(refs/tags/v1.2.3 → v1.2.3,落入 Branch)与 commit,并经 v* 过滤放行。
func TestWebhookTagPushCarriesTagRefAndCommit(t *testing.T) {
	rc, fake, token, secret := newTagReleaseReceiver(t)
	res, err := rc.Handle(context.Background(), Delivery{
		Token:      token,
		Event:      eventTag,
		TokenHdr:   secret,
		DeliveryID: "d-tagpush",
		RawBody:    []byte(`{"ref":"refs/tags/v1.2.3","after":"tagsha123"}`),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Accepted || res.RunID == "" {
		t.Fatalf("expected accepted run for tag push, got %+v", res)
	}
	if fake.count() != 1 {
		t.Fatalf("expected 1 run, got %d", fake.count())
	}
	got := fake.calls[0]
	if got.Branch != "v1.2.3" {
		t.Fatalf("expected tag name v1.2.3 in Branch, got %q", got.Branch)
	}
	if got.Commit != "tagsha123" {
		t.Fatalf("expected commit tagsha123, got %q", got.Commit)
	}
}

// TestWebhookReleaseHookCreatesRunWithTagName 验证 Release Hook 命中后:
// 运行携带 release.tag_name(落入 Branch),commit 回退到 target_commitish,经 v* 过滤放行。
func TestWebhookReleaseHookCreatesRunWithTagName(t *testing.T) {
	rc, fake, token, secret := newTagReleaseReceiver(t)
	res, err := rc.Handle(context.Background(), Delivery{
		Token:      token,
		Event:      eventRelease,
		TokenHdr:   secret,
		DeliveryID: "d-release",
		RawBody:    []byte(`{"action":"published","release":{"tag_name":"v2.0.0","target_commitish":"main"}}`),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Accepted || res.RunID == "" {
		t.Fatalf("expected accepted run for release, got %+v", res)
	}
	if fake.count() != 1 {
		t.Fatalf("expected 1 run, got %d", fake.count())
	}
	got := fake.calls[0]
	if got.Branch != "v2.0.0" {
		t.Fatalf("expected release tag v2.0.0 in Branch, got %q", got.Branch)
	}
	if got.Commit != "main" {
		t.Fatalf("expected commit fallback to target_commitish 'main', got %q", got.Commit)
	}
}

// TestWebhookReleaseNotSubscribed 验证未勾选 Release 时,Release Hook 投递被忽略(不创建运行)。
func TestWebhookReleaseNotSubscribed(t *testing.T) {
	// newReceiver 只开 Push(Release=false)。
	rc, fake, token, secret := newReceiver(t)
	res, err := rc.Handle(context.Background(), Delivery{
		Token:      token,
		Event:      eventRelease,
		TokenHdr:   secret,
		DeliveryID: "d-rel-nosub",
		RawBody:    []byte(`{"release":{"tag_name":"v9","target_commitish":"main"}}`),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Accepted || res.Ignored != IgnoredEventNotSubscribed {
		t.Fatalf("expected event_not_subscribed, got %+v", res)
	}
	if fake.count() != 0 {
		t.Fatalf("expected no run, got %d", fake.count())
	}
}

func TestWebhookTokenNotFound(t *testing.T) {
	rc, _, _, secret := newReceiver(t)
	_, err := rc.Handle(context.Background(), Delivery{
		Token:    "nonexistent",
		Event:    eventPush,
		TokenHdr: secret,
		RawBody:  pushBody("main", "x"),
	})
	if err != ErrTokenNotFound {
		t.Fatalf("expected ErrTokenNotFound, got %v", err)
	}
}

func TestWebhookVaultUnconfiguredRejects(t *testing.T) {
	db, _ := testDB(t)
	v := vault.New(db, testMasterKey())
	svc := New(db, v)
	projID := seedProject(t, db)
	cfg, err := svc.Get(context.Background(), projID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// 用未配置 vault 的 Receiver(master key 缺失)→ 验签不可用,明确拒绝。
	rc := NewReceiver(db, vault.New(db, nil), &fakeRunCreator{})
	_, err = rc.Handle(context.Background(), Delivery{
		Token:    cfg.WebhookToken,
		Event:    eventPush,
		TokenHdr: "anything",
		RawBody:  pushBody("main", "x"),
	})
	if err != ErrUnauthorized {
		t.Fatalf("expected ErrUnauthorized when vault unconfigured, got %v", err)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"main", "main", true},
		{"main", "master", false},
		{"release/*", "release/1.2", true},
		{"release/*", "release/", true},
		{"release/*", "dev", false},
		{"feature/*-hotfix", "feature/x-hotfix", true},
		{"feature/*-hotfix", "feature/x", false},
		{"*", "anything", true},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.s); got != c.want {
			t.Errorf("globMatch(%q,%q)=%v want %v", c.pattern, c.s, got, c.want)
		}
	}
}

// TestMatchBranchFilter 验证分支过滤白名单语义:空列表放行、命中放行、不匹配/空分支拒绝。
func TestMatchBranchFilter(t *testing.T) {
	cases := []struct {
		name    string
		branch  string
		filters []string
		want    bool
	}{
		{"空过滤放行任意分支", "anything", nil, true},
		{"空过滤放行空列表形式", "dev", []string{}, true},
		{"精确命中", "main", []string{"main", "dev"}, true},
		{"glob 命中", "release/1.2", []string{"release/*"}, true},
		{"不匹配拒绝", "dev", []string{"main"}, false},
		{"空分支一律拒绝(防御)", "", nil, false},
		{"空分支在空过滤下也拒绝", "", []string{}, false},
	}
	for _, c := range cases {
		if got := matchBranchFilter(c.branch, c.filters); got != c.want {
			t.Errorf("%s: matchBranchFilter(%q,%v)=%v want %v", c.name, c.branch, c.filters, got, c.want)
		}
	}
}

// clearBranchFilters 清空 newReceiver 所建项目的分支过滤(改为"全分支触发")。
func clearBranchFilters(t *testing.T, rc *Receiver) *Receiver {
	t.Helper()
	// 直接对库内唯一项目的触发配置清空过滤。
	_, err := rc.db.Exec(`UPDATE pipeline_triggers SET branch_filters_json = '[]'`)
	if err != nil {
		t.Fatalf("clear branch filters: %v", err)
	}
	return rc
}
