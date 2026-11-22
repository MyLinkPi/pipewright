package httpapi

// apps_instances_cache.go —— 「已部署实例」批量端点(GET /api/servers/apps/instances)的
// per-server 采集缓存。不缓存则每次打开应用商店都对全部机器逐台 SSH(find 栈目录 +
// docker ps + 逐实例读边车 + 容器内版本探测,机内串行),不可达台每次吃满 30s 拨号超时。
//
// 与 containersCache(server_containers.go)同款 TTL + single-flight 思路,但应用商店是
// 偶尔打开的页面(非轮询),阻塞式 TTL 过期后重开照样慢 —— 故采用 stale-while-revalidate
// (仿 metricsSnapshot / projectHeadsCache):TTL 内直接回放;过期**先回旧值**、后台单飞
// 刷新;仅冷启动(进程重启/写操作失效后)同步采集。写操作(部署/升级/卸载/栈保存/模板
// 增删改)主动失效,操作后刷新即拿新数据;目标机上手工 docker 操作无通知,靠 TTL 收敛。

import (
	"context"
	"sync"
	"time"

	"github.com/huangchengsir/pipewright/internal/target"
)

// appInstancesCacheTTL 是单台实例清单采集结果的进程内缓存时长。SWR 下 TTL 只约束后台
// 刷新频率(过期不阻塞请求);30s 对齐 containersCacheTTL,实例清单本就是准实时视图。
const appInstancesCacheTTL = 30 * time.Second

// appInstancesCacheEntry 是一台服务器的实例清单缓存。rows 按只读契约共享(handlers 仅
// 序列化,绝不修改;含 reachable:false 的失败结果——不可达台恰是采集最慢的台,不缓存会
// 让它们拖慢每一次打开)。
type appInstancesCacheEntry struct {
	rows []appInstanceDTO
	at   time.Time
}

// appInstancesCache 是 per-server 实例清单缓存:TTL + stale-while-revalidate + 单飞。
// inflight 同时承担冷启动同步采集与后台刷新两种单飞:同一台并发未命中只允许一个采集在
// 飞,冷启动的其余等待方共享结果,过期路径的读取方则拿旧值即走。gen 是失效世代:采集
// 期间发生 invalidate 时丢弃在飞采集的结果,防止写操作前的旧清单被回填锁死一个 TTL
// (部署/卸载后前端立即 refetch 必须读到新数据)。进程内状态:单实例部署,重启即冷。
type appInstancesCache struct {
	mu        sync.Mutex
	ttl       time.Duration
	now       func() time.Time // 注入时钟,便于单测 TTL 过期路径
	entries   map[string]appInstancesCacheEntry
	inflight  map[string]chan struct{}
	gen       map[string]uint64
	sem       chan struct{} // 后台刷新并发上限(与请求路径采集并发同界),防一批同时过期把 SSH 打爆
	refreshed func()        // 测试钩子:每次后台刷新收尾后调用(单测等待异步刷新完成)
}

func newAppInstancesCache(ttl time.Duration) *appInstancesCache {
	return &appInstancesCache{
		ttl:      ttl,
		now:      time.Now,
		entries:  map[string]appInstancesCacheEntry{},
		inflight: map[string]chan struct{}{},
		gen:      map[string]uint64{},
		sem:      make(chan struct{}, appsInstancesConcurrency),
	}
}

// appInstances 是实例清单的进程级缓存实例(批量端点读,写操作失效它)。
var appInstances = newAppInstancesCache(appInstancesCacheTTL)

// resetAppInstancesCache 清空缓存(测试隔离用:缓存是包级全局,跨用例会串扰)。
func resetAppInstancesCache() {
	appInstances.mu.Lock()
	appInstances.entries = map[string]appInstancesCacheEntry{}
	appInstances.inflight = map[string]chan struct{}{}
	appInstances.gen = map[string]uint64{}
	appInstances.now = time.Now
	appInstances.refreshed = nil
	appInstances.mu.Unlock()
}

// appInstancesCollectFunc 采集一台机器的实例清单(含模板 join;失败以 reachable:false
// 条目表达,恒可缓存)。批量 handler 每台现查模板库后闭包传入,模板写操作经 invalidateAll
// 保证 join 字段(displayName/icon/upgradeAvailable/templateGone/legacy 反推)新鲜。
type appInstancesCollectFunc func(ctx context.Context) []appInstanceDTO

// get 返回 srv 的实例清单:TTL 内命中直接回放;过期先回旧值并触发后台单飞刷新;冷启动
// 同步采集一次(首屏真数据),并发冷请求经 inflight 合并为一次采集。冷采集以 WithoutCancel
// 脱钩请求生命周期(仿 metricsSnapshot 冷路径):发起方断开不浪费已完成大半的采集,结果
// 照常入缓存;等待方可随自身 ctx 取消,拿 reachable:false 占位(不静默吞机器)。
func (c *appInstancesCache) get(ctx context.Context, srv *target.Server, collect appInstancesCollectFunc) []appInstanceDTO {
	id := srv.ID
	c.mu.Lock()
	e, has := c.entries[id]
	if has && c.now().Sub(e.at) < c.ttl {
		c.mu.Unlock()
		return e.rows
	}
	wait, busy := c.inflight[id]
	if has {
		// 过期:先回旧值(stale-while-revalidate);无人刷新则标记单飞并后台刷新。
		if !busy {
			wait = make(chan struct{})
			c.inflight[id] = wait
			c.mu.Unlock()
			go c.refresh(srv, collect, wait)
		} else {
			c.mu.Unlock()
		}
		return e.rows
	}
	// 冷启动:无条目。无人采集则自采(同步),否则等待共享结果。
	if !busy {
		wait = make(chan struct{})
		c.inflight[id] = wait
		g := c.gen[id]
		c.mu.Unlock()

		rows := collect(context.WithoutCancel(ctx))
		c.mu.Lock()
		if c.gen[id] == g { // 采集期间被写操作失效 → 丢弃旧结果,不回填
			c.entries[id] = appInstancesCacheEntry{rows: rows, at: c.now()}
		}
		delete(c.inflight, id)
		c.mu.Unlock()
		close(wait)
		return rows
	}
	c.mu.Unlock()

	select {
	case <-wait:
		// 共享到他人结果(已入缓存;若采集期间被失效则未入,重查落空 → 自采)。
		c.mu.Lock()
		e, ok := c.entries[id]
		c.mu.Unlock()
		if ok {
			return e.rows
		}
	case <-ctx.Done():
		return []appInstanceDTO{{ServerID: id, ServerName: srv.Name, Error: "等待采集结果时请求已取消"}}
	}
	// 等到了他人采集但结果未入缓存(恰被写操作失效)→ 自采一次直接返回,不回填缓存,
	// 避免与可能仍在飞的采集竞态(与 containersCache 同纪律)。
	return collect(ctx)
}

// refresh 后台单飞刷新一台(不持锁、不用请求 ctx;collectAppInstances 自带单机超时)。
// 结果无条件落缓存(可达与否都算数,把重试频率自然压到每 TTL 最多一次);采集期间被
// invalidate 则丢弃(gen 守卫),让写操作后的读取冷启动重采而非吃旧清单。
func (c *appInstancesCache) refresh(srv *target.Server, collect appInstancesCollectFunc, wait chan struct{}) {
	defer func() {
		// 钩子与 resetAppInstancesCache 的写入同锁读取,避免测试隔离路径上的数据竞争。
		c.mu.Lock()
		hook := c.refreshed
		c.mu.Unlock()
		if hook != nil {
			hook()
		}
	}()
	c.sem <- struct{}{}
	defer func() { <-c.sem }()

	id := srv.ID
	c.mu.Lock()
	g := c.gen[id]
	c.mu.Unlock()

	rows := collect(context.Background())
	c.mu.Lock()
	if c.gen[id] == g {
		c.entries[id] = appInstancesCacheEntry{rows: rows, at: c.now()}
	}
	delete(c.inflight, id)
	c.mu.Unlock()
	close(wait)
}

// invalidate 丢弃 id 的缓存条目并推进世代(实例变更类写操作成功投递后调用:部署/升级/
// 卸载/栈保存与动作)。在飞采集收尾时见世代已变即丢弃结果,随后的读取冷启动重采。
func (c *appInstancesCache) invalidate(id string) {
	c.mu.Lock()
	delete(c.entries, id)
	c.gen[id]++
	c.mu.Unlock()
}

// invalidateAll 丢弃全部条目(模板增删改后调用:实例条目的展示名/图标/更新标记/legacy
// 反推全部 join 自模板库,模板一变全部机器的缓存都可能是旧的)。
func (c *appInstancesCache) invalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[string]appInstancesCacheEntry{}
	for id := range c.gen {
		c.gen[id]++
	}
	for id := range c.inflight {
		if _, ok := c.gen[id]; !ok {
			c.gen[id]++
		}
	}
}
