package httpapi

// 实例清单 SWR 缓存(appInstancesCache)的契约测试。
// 单元级:本地缓存实例 + 注入时钟,验证 TTL 回放、过期回旧值 + 后台单飞刷新、冷启动
// 单飞合并、失败结果缓存、写失效、gen 守卫(在飞采集期间失效 → 丢弃旧结果)、等待方
// ctx 取消占位。HTTP 级:批量端点缓存回放与 ?refresh=1 强制重采。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/appstore"
	"github.com/huangchengsir/pipewright/internal/target"
)

// instCacheRows 造一条可达实例行(version 区分采集批次)。
func instCacheRows(version string) []appInstanceDTO {
	return []appInstanceDTO{{ServerID: "s1", ServerName: "build-1", Reachable: true, StackName: "gitea", Version: version}}
}

// waitCalls 轮询等待采集计数到达 n(后台 goroutine 进入采集的同步点)。
func waitCalls(t *testing.T, calls *int32, n int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(calls) < n {
		if time.Now().After(deadline) {
			t.Fatalf("采集计数停在 %d,期望到达 %d", atomic.LoadInt32(calls), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitRefreshed 等待后台刷新收尾钩子(refreshed 信号)。
func waitRefreshed(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("后台刷新未在期限内完成")
	}
}

// TestAppInstancesCache_TTLReplayAndInvalidate:TTL 内回放零重采;失败结果同样缓存;
// 主动失效后立即重采。
func TestAppInstancesCache_TTLReplayAndInvalidate(t *testing.T) {
	c := newAppInstancesCache(30 * time.Second)
	now := time.Now()
	c.now = func() time.Time { return now }
	srv := &target.Server{ID: "s1", Name: "build-1"}

	calls := 0
	collect := func(context.Context) []appInstanceDTO {
		calls++
		return instCacheRows("v1")
	}
	c.get(context.Background(), srv, collect)
	c.get(context.Background(), srv, collect)
	if calls != 1 {
		t.Fatalf("collect calls = %d, want 1(TTL 内应回放缓存)", calls)
	}

	// 失败结果(reachable:false)同样缓存:不可达台恰是最慢的台,不缓存会拖慢每次打开。
	failCalls := 0
	failing := func(context.Context) []appInstanceDTO {
		failCalls++
		return []appInstanceDTO{{ServerID: "s2", ServerName: "build-2", Error: "ssh: connection refused"}}
	}
	srv2 := &target.Server{ID: "s2", Name: "build-2"}
	c.get(context.Background(), srv2, failing)
	rows := c.get(context.Background(), srv2, failing)
	if failCalls != 1 || len(rows) != 1 || rows[0].Reachable {
		t.Fatalf("失败结果应缓存回放: calls=%d rows=%+v", failCalls, rows)
	}

	c.invalidate("s1") // 写操作失效 → 不等 TTL 立即重采
	c.get(context.Background(), srv, collect)
	if calls != 2 {
		t.Fatalf("collect calls after invalidate = %d, want 2", calls)
	}
}

// TestAppInstancesCache_StaleWhileRevalidate:过期先回旧值(不阻塞),后台单飞刷新收敛;
// 刷新落缓存后 TTL 内再读不再触发刷新。
func TestAppInstancesCache_StaleWhileRevalidate(t *testing.T) {
	c := newAppInstancesCache(30 * time.Second)
	now := time.Now()
	c.now = func() time.Time { return now }
	done := make(chan struct{}, 1)
	c.refreshed = func() { done <- struct{}{} }
	srv := &target.Server{ID: "s1", Name: "build-1"}

	var calls, phase int32
	collect := func(context.Context) []appInstanceDTO {
		atomic.AddInt32(&calls, 1)
		if atomic.LoadInt32(&phase) == 2 {
			return instCacheRows("v2")
		}
		return instCacheRows("v1")
	}

	if rows := c.get(context.Background(), srv, collect); rows[0].Version != "v1" {
		t.Fatalf("冷启动应同步采集真数据: %+v", rows)
	}
	now = now.Add(31 * time.Second) // 推进过 TTL
	atomic.StoreInt32(&phase, 2)

	// 过期读取:立即拿旧值,刷新在后台(请求路径零阻塞)。
	if rows := c.get(context.Background(), srv, collect); rows[0].Version != "v1" {
		t.Fatalf("过期应先回旧值(stale-while-revalidate): %+v", rows)
	}
	waitRefreshed(t, done)

	if rows := c.get(context.Background(), srv, collect); rows[0].Version != "v2" {
		t.Fatalf("后台刷新收敛后应读到新值: %+v", rows)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("collect calls = %d, want 2(冷启动 1 + 后台刷新 1)", got)
	}
	c.get(context.Background(), srv, collect) // 刷新后 TTL 内:不再触发
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("刷新后 TTL 内不应再采集: calls = %d", got)
	}
}

// TestAppInstancesCache_ColdSingleFlight:并发冷启动合并为一次采集,等待方共享同一结果。
func TestAppInstancesCache_ColdSingleFlight(t *testing.T) {
	c := newAppInstancesCache(time.Minute)
	srv := &target.Server{ID: "s1", Name: "build-1"}
	var calls int32
	collect := func(context.Context) []appInstanceDTO {
		atomic.AddInt32(&calls, 1)
		time.Sleep(100 * time.Millisecond) // 留窗口让并发调用都落到 inflight 上等待
		return instCacheRows("v1")
	}

	const n = 8
	var wg sync.WaitGroup
	results := make([][]appInstanceDTO, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = c.get(context.Background(), srv, collect)
		}(i)
	}
	wg.Wait()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("collect calls = %d, want 1(并发冷启动应合并为一次采集)", got)
	}
	for i, rows := range results {
		if len(rows) != 1 || rows[0].Version != "v1" {
			t.Fatalf("waiter %d 共享结果不符: %+v", i, rows)
		}
	}
}

// TestAppInstancesCache_GenGuardBackground:后台刷新在飞时写操作失效 → 旧结果被丢弃
// (不回填锁死一个 TTL),随后的读取冷启动重采新数据。
func TestAppInstancesCache_GenGuardBackground(t *testing.T) {
	c := newAppInstancesCache(30 * time.Second)
	now := time.Now()
	c.now = func() time.Time { return now }
	done := make(chan struct{}, 1)
	c.refreshed = func() { done <- struct{}{} }
	srv := &target.Server{ID: "s1", Name: "build-1"}

	gate := make(chan struct{})
	var calls int32
	var mu sync.Mutex
	current := "old"
	collect := func(context.Context) []appInstanceDTO {
		n := atomic.AddInt32(&calls, 1)
		if n == 2 { // 第二次采集 = 后台刷新:阻塞,等 invalidate 先落地
			<-gate
		}
		mu.Lock()
		v := current
		mu.Unlock()
		return instCacheRows(v)
	}

	c.get(context.Background(), srv, collect) // 冷启动,缓存 old
	now = now.Add(31 * time.Second)
	mu.Lock()
	current = "new"
	mu.Unlock()
	c.get(context.Background(), srv, collect) // 过期 → 回旧值,后台刷新阻塞在 gate
	waitCalls(t, &calls, 2)

	c.invalidate("s1") // 写操作落在刷新在飞期间
	close(gate)
	waitRefreshed(t, done)

	c.mu.Lock()
	_, cached := c.entries["s1"]
	c.mu.Unlock()
	if cached {
		t.Fatal("失效前启动的后台刷新结果应被 gen 守卫丢弃,不得回填")
	}
	rows := c.get(context.Background(), srv, collect) // 冷启动重采
	if rows[0].Version != "new" || atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("失效后应冷启动重采新数据: rows=%+v calls=%d", rows, atomic.LoadInt32(&calls))
	}
}

// TestAppInstancesCache_GenGuardCold:冷启动采集在飞时写操作失效 → 发起方仍拿自己的
// 结果,但不回填缓存;随后的读取重新采集。
func TestAppInstancesCache_GenGuardCold(t *testing.T) {
	c := newAppInstancesCache(time.Minute)
	srv := &target.Server{ID: "s1", Name: "build-1"}
	gate := make(chan struct{})
	var calls int32
	collect := func(context.Context) []appInstanceDTO {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			<-gate
		}
		return instCacheRows("v1")
	}

	var leaderRows []appInstanceDTO
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		leaderRows = c.get(context.Background(), srv, collect)
	}()
	waitCalls(t, &calls, 1)

	c.invalidate("s1")
	close(gate)
	wg.Wait()
	if len(leaderRows) != 1 {
		t.Fatalf("发起方应拿到自己的采集结果: %+v", leaderRows)
	}
	c.mu.Lock()
	_, cached := c.entries["s1"]
	c.mu.Unlock()
	if cached {
		t.Fatal("失效前启动的冷采集结果应被 gen 守卫丢弃,不得回填")
	}
	c.get(context.Background(), srv, collect)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("失效后应重新采集: calls = %d, want 2", got)
	}
}

// TestAppInstancesCache_InvalidateAll:模板增删改 → 全部机器条目失效重采。
func TestAppInstancesCache_InvalidateAll(t *testing.T) {
	c := newAppInstancesCache(time.Minute)
	calls := 0
	collect := func(context.Context) []appInstanceDTO {
		calls++
		return instCacheRows("v1")
	}
	srv1 := &target.Server{ID: "s1", Name: "build-1"}
	srv2 := &target.Server{ID: "s2", Name: "build-2"}
	c.get(context.Background(), srv1, collect)
	c.get(context.Background(), srv2, collect)
	if calls != 2 {
		t.Fatalf("collect calls = %d, want 2", calls)
	}
	c.invalidateAll()
	c.get(context.Background(), srv1, collect)
	c.get(context.Background(), srv2, collect)
	if calls != 4 {
		t.Fatalf("invalidateAll 后两台都应重采: calls = %d, want 4", calls)
	}
}

// TestAppInstancesCache_WaiterCancel:冷启动等待方可随自身 ctx 取消,拿 reachable:false
// 占位(不静默吞机器);发起方采集不受连累,结果照常入缓存。
func TestAppInstancesCache_WaiterCancel(t *testing.T) {
	c := newAppInstancesCache(time.Minute)
	srv := &target.Server{ID: "s1", Name: "build-1"}
	gate := make(chan struct{})
	var calls int32
	collect := func(context.Context) []appInstanceDTO {
		atomic.AddInt32(&calls, 1)
		<-gate
		return instCacheRows("v1")
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // 发起方:阻塞在 gate
		defer wg.Done()
		c.get(context.Background(), srv, collect)
	}()
	waitCalls(t, &calls, 1)

	ctx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan []appInstanceDTO, 1)
	go func() { waiterDone <- c.get(ctx, srv, collect) }()
	time.Sleep(50 * time.Millisecond) // 让等待方落到 inflight 等待上
	cancel()
	rows := <-waiterDone
	if len(rows) != 1 || rows[0].Reachable || rows[0].Error == "" || rows[0].ServerID != "s1" || rows[0].ServerName != "build-1" {
		t.Fatalf("取消的等待方应拿 reachable:false 占位: %+v", rows)
	}
	close(gate)
	wg.Wait()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("等待方取消不应连累发起方采集: calls = %d, want 1", got)
	}
	if rows := c.get(context.Background(), srv, collect); rows[0].Version != "v1" {
		t.Fatalf("发起方结果应照常入缓存: %+v", rows)
	}
}

// runInstancesQuery 打枚举端点(带 query)并解码 items;不重置缓存(供跨请求缓存测试)。
func runInstancesQuery(t *testing.T, apps appstore.Service, servers target.Service, query string) []appInstanceDTO {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/servers/apps/instances", makeAllAppInstancesHandler(apps, servers))
	req := httptest.NewRequest(http.MethodGet, "/servers/apps/instances"+query, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items []appInstanceDTO `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("解码响应: %v", err)
	}
	return out.Items
}

// TestAllAppInstances_CacheReplayAndRefresh(HTTP 级):TTL 内回放缓存(罐头输出变了也
// 不受影响);?refresh=1 强制重采并把新结果入缓存。
func TestAllAppInstances_CacheReplayAndRefresh(t *testing.T) {
	resetAppInstancesCache()
	t.Cleanup(resetAppInstancesCache)

	gitea := appstore.Template{ID: "tpl-1", Name: "gitea", DisplayName: "Gitea"}
	findOut := "/opt/pipewright/stacks/gitea/.appstore-params.json\n/opt/pipewright/stacks/gitea/docker-compose.yml\n"
	m := &instMachine{
		srv:     &target.Server{ID: "srv-1", Name: "build-1"},
		findOut: findOut,
		sidecars: map[string]string{
			"gitea": mustSidecar(t, appDeploySidecar{TemplateID: "tpl-1", Params: map[string]string{}}),
		},
	}
	apps := &instAppFake{tpls: []appstore.Template{gitea}}
	servers := &instTargetFake{machines: []*instMachine{m}}

	if items := runInstancesQuery(t, apps, servers, ""); len(items) != 1 {
		t.Fatalf("首次应真采集出 1 条: %+v", items)
	}
	m.findOut = "" // 罐头输出变空:TTL 内回放,不受影响
	if items := runInstancesQuery(t, apps, servers, ""); len(items) != 1 {
		t.Fatalf("TTL 内应回放缓存(1 条): %+v", items)
	}
	if items := runInstancesQuery(t, apps, servers, "?refresh=1"); len(items) != 0 {
		t.Fatalf("?refresh=1 应强制重采(空 find → 0 条): %+v", items)
	}
	m.findOut = findOut // 重采结果已入缓存:再读仍回放空,不再采集
	if items := runInstancesQuery(t, apps, servers, ""); len(items) != 0 {
		t.Fatalf("重采结果应入缓存回放(0 条): %+v", items)
	}
}