package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/repocache"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// --- 缓存单测(注入时钟,隔离实例)---

// waitHeadsEntry 轮询等待 key 的缓存条目落定(后台刷新是异步的)。
func waitHeadsEntry(t *testing.T, c *projectHeadsCache, key string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		_, ok := c.entries[key]
		c.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待 heads 缓存条目超时: %q", key)
}

// waitHeadsValue 轮询等待 key 的快照刷新到指定分支 HEAD。
func waitHeadsValue(t *testing.T, c *projectHeadsCache, key, branch, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		e, ok := c.entries[key]
		c.mu.Unlock()
		if ok && e.heads[branch] == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待 heads 快照超时: %q[%s] != %q", key, branch, want)
}

// waitHeadsCalls 轮询等待 refresh 调用次数达到 want。
func waitHeadsCalls(t *testing.T, n *int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(n) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待 refresh 调用次数超时: got %d, want %d", atomic.LoadInt32(n), want)
}

// TestProjectHeadsCacheTTLAndSWR 验证:TTL 内回放不重探;过期先回放旧值(stale-while-revalidate)
// 再后台刷新到新值。
func TestProjectHeadsCacheTTLAndSWR(t *testing.T) {
	c := newProjectHeadsCache(time.Minute)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	var calls int32
	refresh := func(_ context.Context) (map[string]string, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return map[string]string{"main": "aaa"}, nil
		}
		return map[string]string{"main": "bbb"}, nil
	}

	// 首次:未知 + 触发后台刷新。
	if heads, known := c.lookup("k", refresh); known || heads != nil {
		t.Fatalf("首次 lookup 应为未知, got %v %v", heads, known)
	}
	waitHeadsValue(t, c, "k", "main", "aaa")
	if heads, known := c.lookup("k", refresh); !known || heads["main"] != "aaa" {
		t.Fatalf("TTL 内应回放 aaa, got %v %v", heads, known)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("TTL 内不应重探, calls = %d", got)
	}

	// 过期:立即回放旧值 aaa,后台刷新到 bbb。
	now = now.Add(2 * time.Minute)
	if heads, known := c.lookup("k", refresh); !known || heads["main"] != "aaa" {
		t.Fatalf("过期应先回放旧值 aaa(SWR), got %v %v", heads, known)
	}
	waitHeadsValue(t, c, "k", "main", "bbb")
	if heads, known := c.lookup("k", refresh); !known || heads["main"] != "bbb" {
		t.Fatalf("刷新后应回放 bbb, got %v %v", heads, known)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("过期应恰好重探一次, calls = %d", got)
	}
}

// TestProjectHeadsCacheSingleflight 验证同一 key 的并发未命中合并为一次探测。
func TestProjectHeadsCacheSingleflight(t *testing.T) {
	c := newProjectHeadsCache(time.Minute)
	release := make(chan struct{})
	var calls int32
	refresh := func(_ context.Context) (map[string]string, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return map[string]string{"main": "aaa"}, nil
	}
	for i := 0; i < 5; i++ {
		if _, known := c.lookup("k", refresh); known {
			t.Fatalf("探测在飞时应为未知")
		}
	}
	waitHeadsCalls(t, &calls, 1)
	close(release)
	waitHeadsValue(t, c, "k", "main", "aaa")
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("并发未命中应合并为一次探测, calls = %d", got)
	}
}

// TestProjectHeadsCacheFailureBackoff 验证失败结果同样缓存:无旧值时 TTL 内不重试
// (前端短轮询不放大成 fetch 风暴),TTL 过期后才允许再探。
func TestProjectHeadsCacheFailureBackoff(t *testing.T) {
	c := newProjectHeadsCache(time.Minute)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	var calls int32
	refresh := func(_ context.Context) (map[string]string, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("repo unreachable")
	}
	if _, known := c.lookup("k", refresh); known {
		t.Fatalf("首次应为未知")
	}
	waitHeadsEntry(t, c, "k")
	if _, known := c.lookup("k", refresh); known {
		t.Fatalf("失败后 TTL 内仍应为未知")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("失败应退避,TTL 内不重试, calls = %d", got)
	}
	now = now.Add(2 * time.Minute)
	c.lookup("k", refresh)
	waitHeadsCalls(t, &calls, 2)
}

// TestProjectHeadsCacheFailureKeepsStale 验证刷新失败时保留旧值(徽标按最后已知 HEAD 展示)
// 且同样获得 TTL 退避。
func TestProjectHeadsCacheFailureKeepsStale(t *testing.T) {
	c := newProjectHeadsCache(time.Minute)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	var calls int32
	refresh := func(_ context.Context) (map[string]string, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return map[string]string{"main": "aaa"}, nil
		}
		return nil, errors.New("boom")
	}
	c.lookup("k", refresh)
	waitHeadsValue(t, c, "k", "main", "aaa")

	now = now.Add(2 * time.Minute)
	if heads, known := c.lookup("k", refresh); !known || heads["main"] != "aaa" {
		t.Fatalf("过期应先回放旧值, got %v %v", heads, known)
	}
	waitHeadsCalls(t, &calls, 2)
	if heads, known := c.lookup("k", refresh); !known || heads["main"] != "aaa" {
		t.Fatalf("刷新失败应保留旧值 aaa, got %v %v", heads, known)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("失败后应退避, calls = %d", got)
	}
}

// TestProjectHeadsCacheConcurrencyCap 验证后台探测全局并发上限:超出槽位的探测排队执行。
func TestProjectHeadsCacheConcurrencyCap(t *testing.T) {
	c := newProjectHeadsCache(time.Minute)
	release := make(chan struct{})
	var active, maxActive int32
	refresh := func(_ context.Context) (map[string]string, error) {
		cur := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&maxActive)
			if cur <= old || atomic.CompareAndSwapInt32(&maxActive, old, cur) {
				break
			}
		}
		<-release
		atomic.AddInt32(&active, -1)
		return map[string]string{"main": "aaa"}, nil
	}
	n := projectHeadsMaxInflight + 2
	for i := 0; i < n; i++ {
		c.lookup(fmt.Sprintf("k%d", i), refresh)
	}
	// 等并发槽占满(若上限失效,active 会冲到 n)。
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&active) < int32(projectHeadsMaxInflight) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	for i := 0; i < n; i++ {
		waitHeadsValue(t, c, fmt.Sprintf("k%d", i), "main", "aaa")
	}
	if got := atomic.LoadInt32(&maxActive); got > projectHeadsMaxInflight {
		t.Fatalf("后台探测并发 = %d, 超过上限 %d", got, projectHeadsMaxInflight)
	}
}

// TestProjectHeadsCacheBounded 验证项目/凭据 key 持续增加时快照总量有硬上限,
// 删除项目遗留的旧 key 不会随进程运行时间无界积累。
func TestProjectHeadsCacheBounded(t *testing.T) {
	c := newProjectHeadsCache(time.Minute)
	refresh := func(_ context.Context) (map[string]string, error) {
		return map[string]string{"main": "aaaaaaa"}, nil
	}
	for i := 0; i < projectHeadsMaxEntries+8; i++ {
		key := fmt.Sprintf("k%d", i)
		c.lookup(key, refresh)
		waitHeadsEntry(t, c, key)
	}
	c.mu.Lock()
	got := len(c.entries)
	c.mu.Unlock()
	if got > projectHeadsMaxEntries {
		t.Fatalf("heads 缓存条目 = %d, 超过上限 %d", got, projectHeadsMaxEntries)
	}
}

// --- sameCommit / resolve 规则 ---

func TestSameCommit(t *testing.T) {
	cases := []struct {
		head, stored string
		want         bool
	}{
		{"abcdef1234567890abcdef1234567890abcdef12", "abcdef1234567890abcdef1234567890abcdef12", true},
		{"abcdef1234567890abcdef1234567890abcdef12", "abcdef1", true}, // 短 SHA 前缀(SetCommit 回写形态)
		{"abcdef1", "abcdef1234567890abcdef1234567890abcdef12", true}, // 反向容忍
		{"ABCDEF1234567890ABCDEF1234567890ABCDEF12", "abcdef1", true}, // 大小写不敏感
		{"abcdef1234567890abcdef1234567890abcdef12", "bbbbbbb", false},
		{" abcdef1 ", "abcdef1", true},
		{"abcdef1234567890abcdef1234567890abcdef12", "a", false},       // 任意短字符串不能当前缀
		{"abcdef1234567890abcdef1234567890abcdef12", "zzzzzzz", false}, // 非 hex 不合法
		{"", "abc", false},
		{"abc", "", false},
	}
	for _, tc := range cases {
		if got := sameCommit(tc.head, tc.stored); got != tc.want {
			t.Fatalf("sameCommit(%q, %q) = %v, want %v", tc.head, tc.stored, got, tc.want)
		}
	}
}

// TestProjectHeadsResolveRules 验证 resolve 的降级规则:空分支回退默认分支;分支已删 /
// pushed 源 / commit 空 / 无运行 → null;refs 未装配 / nil receiver → 全 null。
func TestProjectHeadsResolveRules(t *testing.T) {
	const headSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const repoURL = "https://gitee.com/acme/resolve-rules.git"
	c := newProjectHeadsCache(time.Minute)
	rz := &projectHeadsResolver{
		refs:  fakeRefsLister{refs: &repocache.Refs{Branches: []repocache.Ref{{Name: "main", Commit: headSHA}}}},
		cache: c,
	}
	projs := []project.Project{
		{ID: "fallback", RepoURL: repoURL, DefaultBranch: "main", SourceType: project.SourceTypeGit},
		{ID: "gone", RepoURL: repoURL, DefaultBranch: "main", SourceType: project.SourceTypeGit},
		{ID: "pushed", SourceType: project.SourceTypePushed},
		{ID: "nocommit", RepoURL: repoURL, DefaultBranch: "main"},
		{ID: "norun", RepoURL: repoURL, DefaultBranch: "main"},
	}
	lastRuns := map[string]run.LatestRun{
		"fallback": {Status: "success", Branch: "", Commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"gone":     {Status: "success", Branch: "deleted", Commit: headSHA},
		"pushed":   {Status: "success", Branch: "main", Commit: headSHA},
		"nocommit": {Status: "queued", Branch: "main", Commit: ""},
	}

	// 首次:探测在飞 → 全 null(fallback 最先 lookup,必触发刷新并返回未知)。
	if out := rz.resolve(projs, lastRuns); len(out) != 0 {
		t.Fatalf("首次 resolve 应全未知, got %v", out)
	}
	waitHeadsValue(t, c, repoURL+"\x00", "main", headSHA)

	out := rz.resolve(projs, lastRuns)
	if out["fallback"] == nil || !*out["fallback"] {
		t.Fatalf("空分支应回退默认分支比较出 true, got %v", out["fallback"])
	}
	for _, id := range []string{"gone", "pushed", "nocommit", "norun"} {
		if out[id] != nil {
			t.Fatalf("%s 应为 null, got %v", id, *out[id])
		}
	}

	// refs 未装配(代码管理区关闭)→ 全 null。
	if out := (&projectHeadsResolver{cache: c}).resolve(projs, lastRuns); len(out) != 0 {
		t.Fatalf("refs 未装配应全 null, got %v", out)
	}
	// nil receiver 安全。
	var rzNil *projectHeadsResolver
	if out := rzNil.resolve(projs, lastRuns); len(out) != 0 {
		t.Fatalf("nil receiver 应全 null, got %v", out)
	}
}

// --- HTTP 集成:非阻塞硬约束 ---

// slowRefsLister 模拟慢仓库:ListRefs 阻塞 delay 后才返回(尊重 ctx 取消)。
type slowRefsLister struct {
	delay time.Duration
	refs  *repocache.Refs
}

func (s slowRefsLister) ListRefs(ctx context.Context, _ string, _ vault.GitAuth) (*repocache.Refs, error) {
	select {
	case <-time.After(s.delay):
		return s.refs, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s slowRefsLister) ListCommits(_ context.Context, _ string, _ vault.GitAuth, _ string, _ int) ([]repocache.Commit, error) {
	return nil, nil
}

// setupHeadsServer 构造带 auth + vault + project + runs + refs 的测试 server(HEAD 探测链路)。
func setupHeadsServer(t *testing.T, lister RefsLister) (*httptest.Server, *http.Client, string, run.Service, project.Service) {
	t.Helper()
	st := testStoreAuth(t)
	svc := auth.NewService(st.DB, nil)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	v := vault.New(st.DB, testMasterKey())
	psvc := project.New(st.DB, v, stubProber{branch: "main"})
	rsvc := run.New(st.DB)
	srv := httptest.NewServer(New(testWebFSAuth(), svc,
		WithVault(v), WithProjects(psvc), WithRuns(rsvc, nil), WithRefs(lister)))
	t.Cleanup(srv.Close)
	client := newTestClient(t)
	csrf := loginWithClient(t, client, srv.URL)
	return srv, client, csrf, rsvc, psvc
}

// createHeadsProject 经 API 建项目,返回 id。
func createHeadsProject(t *testing.T, client *http.Client, srvURL, csrf, name, repoURL, credID string) string {
	t.Helper()
	resp := doJSON(t, client, http.MethodPost, srvURL+"/api/projects", csrf,
		`{"name":"`+name+`","repoUrl":"`+repoURL+`","credentialId":"`+credID+`"}`)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var created map[string]any
	_ = json.Unmarshal(raw, &created)
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("create project failed: %s", raw)
	}
	return id
}

// listProjectField 取列表接口中指定项目的指定字段(项目缺席则 Fatal)。
func listProjectField(t *testing.T, client *http.Client, srv *httptest.Server, projID, field string) any {
	t.Helper()
	resp := doJSON(t, client, http.MethodGet, srv.URL+"/api/projects", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var list []map[string]any
	_ = json.Unmarshal(raw, &list)
	for _, p := range list {
		if p["id"] == projID {
			return p[field]
		}
	}
	t.Fatalf("project %s not in list: %s", projID, raw)
	return nil
}

// waitProjectField 轮询等待字段落定到 want(HEAD 探测是异步的;3s 上限)。
func waitProjectField(t *testing.T, client *http.Client, srv *httptest.Server, projID, field string, want any) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := listProjectField(t, client, srv, projID, field)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("project %s 字段 %s = %v, want %v(等待超时)", projID, field, got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestProjectListNotBlockedByHeadProbe 钉死「请求路径零网络 I/O」硬约束:注入 2s 慢仓库,
// 列表请求仍须毫秒级返回(探测异步,首请 hasNewCommits=null)。未来若有人把探测挪回同步
// 路径,此测试立即失败。
func TestProjectListNotBlockedByHeadProbe(t *testing.T) {
	srv, client, csrf, rsvc, _ := setupHeadsServer(t, slowRefsLister{
		delay: 2 * time.Second,
		refs:  &repocache.Refs{Branches: []repocache.Ref{{Name: "main", Commit: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}},
	})
	credID := newGitCred(t, client, srv.URL, csrf, "ghp_token_slow_probe")
	projID := createHeadsProject(t, client, srv.URL, csrf, "slow-probe", "https://gitee.com/acme/slow-probe.git", credID)
	if _, err := rsvc.Create(context.Background(), projID, run.Trigger{
		Type: run.TriggerManual, Branch: "main", Commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "admin",
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	start := time.Now()
	got := listProjectField(t, client, srv, projID, "hasNewCommits")
	elapsed := time.Since(start)
	if got != nil {
		t.Fatalf("慢仓库首请 hasNewCommits 应为 null(异步探测), got %v", got)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("列表被 HEAD 探测拖慢:耗时 %v(探测延迟 2s);请求路径必须零网络 I/O", elapsed)
	}
}
