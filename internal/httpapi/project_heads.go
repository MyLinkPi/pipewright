package httpapi

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// project_heads.go —— 项目卡片「有新提交」探测(hasNewCommits):比较上次运行的 trigger_commit
// 与该运行分支的当前 HEAD(数据源 = repocache 本地镜像增量 fetch 后的分支快照)。
//
// 硬约束:列表/PATCH 请求路径零网络 I/O —— HEAD 探测(fetch + 凭据解密)全部在后台 goroutine
// 执行,请求路径只做内存 map 查找:未命中 → 返回 null(未知),由前端短轮询收敛;过期 → 先回放
// 旧值再后台刷新(stale-while-revalidate),徽标不闪 null、加载不等探测。与 containersCache
// (server_containers.go)同款 TTL + singleflight 思路,但那是阻塞式采集,此处为非阻塞式。

const (
	// projectHeadsTTL 是单仓库分支 HEAD 快照的进程内缓存时长。项目列表是准实时视图,60s 陈旧
	// 可接受;TTL 内重复加载/多标签页/前端短轮询全部回放内存,零 fetch。
	projectHeadsTTL = 60 * time.Second
	// projectHeadsFetchTimeout 约束单次后台探测「排队 + fetch」总时长(经 ctx 传导进
	// repocache.ensureMirror 的 FetchContext),不可达仓库不会无限占用并发槽。
	projectHeadsFetchTimeout = 90 * time.Second
	// projectHeadsMaxInflight 是后台探测的全局并发上限:冷启动最坏一页 50 个项目也只排队
	// 执行,不与请求处理抢资源。
	projectHeadsMaxInflight = 4
	// projectHeadsMaxEntries 限制进程内快照总量;项目删除/凭据换绑产生的旧 key 不主动清理,
	// 达到上限时按最后刷新时间淘汰最旧条目,避免长期运行后缓存无界增长。
	projectHeadsMaxEntries = 256
)

// projectGitAuthGetter 取仓库凭据(窄接口:vault.Vault 的只读一面,便于测试替身)。
type projectGitAuthGetter interface {
	GetGitAuth(id string) (vault.GitAuth, error)
}

// projectHeadsCache 是 per-仓库(repoURL+credentialID)分支 HEAD 快照缓存:TTL + singleflight
// + stale-while-revalidate + 失败退避。**失败结果同样缓存**(at=now):不可达仓库恰是探测最慢
// 的,不缓存会让前端 3s 轮询放大成 fetch 风暴;有旧值则保留旧值(徽标按最后已知 HEAD 展示)。
// 进程内状态(与 containersCache 同款思路):单实例部署,重启即清空。
type projectHeadsCache struct {
	mu         sync.Mutex
	ttl        time.Duration
	now        func() time.Time // 注入时钟,便于单测 TTL 过期路径
	entries    map[string]projectHeadsEntry
	refreshing map[string]struct{}
	sem        chan struct{} // 后台探测并发上限
}

// projectHeadsEntry 是一个仓库的 HEAD 快照。heads 按只读契约共享(resolve 仅查找,绝不修改);
// nil = 无已知 HEAD(探测失败且无旧值)。
type projectHeadsEntry struct {
	heads map[string]string // 分支名 → 完整 SHA
	at    time.Time
}

func newProjectHeadsCache(ttl time.Duration) *projectHeadsCache {
	return &projectHeadsCache{
		ttl:        ttl,
		now:        time.Now,
		entries:    map[string]projectHeadsEntry{},
		refreshing: map[string]struct{}{},
		sem:        make(chan struct{}, projectHeadsMaxInflight),
	}
}

// projectHeads 是分支 HEAD 探测的进程级缓存实例(列表/PATCH 共享)。
var projectHeads = newProjectHeadsCache(projectHeadsTTL)

// evictOldestLocked 淘汰最后刷新最早的快照;调用方必须持有 c.mu。
// 在飞 key 也可淘汰:其请求已完成旧值回放,刷新结束后会按同一上限重新写入。
func (c *projectHeadsCache) evictOldestLocked() {
	var oldestKey string
	var oldestAt time.Time
	for key, entry := range c.entries {
		if oldestKey == "" || entry.at.Before(oldestAt) {
			oldestKey, oldestAt = key, entry.at
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

// lookup 返回 key 的分支 HEAD 快照:TTL 内命中直接回放;过期先回放旧值并触发后台刷新;
// 未命中触发后台刷新并返回未知。**绝不阻塞网络**:刷新在 goroutine 内跑(Background + 超时,
// 经信号量限并发),同一 key 的并发未命中合并为一次探测。bool = heads 是否可用(nil = 未知)。
func (c *projectHeadsCache) lookup(key string, refresh func(ctx context.Context) (map[string]string, error)) (map[string]string, bool) {
	c.mu.Lock()
	e, has := c.entries[key]
	fresh := has && c.now().Sub(e.at) < c.ttl
	_, busy := c.refreshing[key]
	if fresh || busy {
		c.mu.Unlock()
		return e.heads, has && e.heads != nil
	}
	c.refreshing[key] = struct{}{}
	c.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), projectHeadsFetchTimeout)
		defer cancel()
		// 并发上限:排队也计入 ctx 超时;排到即超时则放弃本次探测(下次 lookup 再触发)。
		select {
		case c.sem <- struct{}{}:
			defer func() { <-c.sem }()
		case <-ctx.Done():
			c.mu.Lock()
			delete(c.refreshing, key)
			c.mu.Unlock()
			return
		}
		heads, err := refresh(ctx)
		c.mu.Lock()
		if _, exists := c.entries[key]; !exists && len(c.entries) >= projectHeadsMaxEntries {
			c.evictOldestLocked()
		}
		switch {
		case err == nil:
			c.entries[key] = projectHeadsEntry{heads: heads, at: c.now()}
		case has && e.heads != nil:
			// 失败退避:保留旧值,at 推至 now,TTL 内不再重试。
			c.entries[key] = projectHeadsEntry{heads: e.heads, at: c.now()}
		default:
			// 无旧值:记占位条目(heads=nil),同样获得 TTL 退避。
			c.entries[key] = projectHeadsEntry{at: c.now()}
		}
		delete(c.refreshing, key)
		c.mu.Unlock()
	}()

	if has && e.heads != nil {
		return e.heads, true // stale-while-revalidate:旧值立即可用,刷新在飞
	}
	return nil, false
}

// projectHeadsResolver 为一批项目解析「有新提交」:上次运行 trigger_commit vs 该分支当前 HEAD。
// 附加展示,与 resolveProjectsLastRun 同语义:任一环节缺席(pushed 源 / 无运行 / commit 空 /
// 代码管理区未启用 / 探测未完成或失败 / 分支已删)降级为 null,绝不阻断项目列表。
type projectHeadsResolver struct {
	vault projectGitAuthGetter
	refs  RefsLister
	cache *projectHeadsCache
}

func newProjectHeadsResolver(v projectGitAuthGetter, refs RefsLister) *projectHeadsResolver {
	return &projectHeadsResolver{vault: v, refs: refs, cache: projectHeads}
}

// resolve 返回 projectID → hasNewCommits(缺席 = null)。nil receiver / refs 未装配(代码管理区
// 关闭)→ 空 map。请求路径只做内存查找;fetch 与凭据解密在缓存的后台 goroutine 内执行。
func (rz *projectHeadsResolver) resolve(projects []project.Project, lastRuns map[string]run.LatestRun) map[string]*bool {
	out := make(map[string]*bool, len(projects))
	if rz == nil || rz.refs == nil || rz.cache == nil {
		return out
	}
	for i := range projects {
		p := &projects[i]
		if p.SourceType == project.SourceTypePushed || strings.TrimSpace(p.RepoURL) == "" {
			continue
		}
		lr, ok := lastRuns[p.ID]
		if !ok || strings.TrimSpace(lr.Commit) == "" {
			continue
		}
		branch := strings.TrimSpace(lr.Branch)
		if branch == "" {
			branch = p.DefaultBranch
		}
		if branch == "" {
			continue
		}
		repoURL, credID := p.RepoURL, p.CredentialID
		v, refs := rz.vault, rz.refs
		heads, known := rz.cache.lookup(repoURL+"\x00"+credID, func(ctx context.Context) (map[string]string, error) {
			// 取凭据同 refs.go:取不到不致命 → 空凭据(公开仓库可成,私有走拉取失败)。
			cred := vault.GitAuth{}
			if v != nil && strings.TrimSpace(credID) != "" {
				if auth, terr := v.GetGitAuth(credID); terr == nil {
					cred = auth
				}
			}
			r, err := refs.ListRefs(ctx, repoURL, cred)
			if err != nil {
				return nil, err
			}
			m := make(map[string]string, len(r.Branches))
			for _, b := range r.Branches {
				m[b.Name] = b.Commit
			}
			return m, nil
		})
		if !known {
			continue
		}
		head, ok := heads[branch]
		if !ok {
			continue // 分支已删 → 未知
		}
		hasNew := !sameCommit(head, lr.Commit)
		out[p.ID] = &hasNew
	}
	return out
}

// sameCommit 判断两个 commit 标识是否指向同一提交:trigger_commit 可能是 7 位短 SHA(克隆后
// SetCommit 回写),HEAD 为完整 SHA,故容忍前缀匹配。仅接受 7~40 位 hex 并忽略大小写,
// 避免手工触发写入的任意短字符串被误认为 HEAD 前缀。
func sameCommit(head, stored string) bool {
	head, ok := normalizeCommit(head)
	if !ok {
		return false
	}
	stored, ok = normalizeCommit(stored)
	return ok && (strings.HasPrefix(head, stored) || strings.HasPrefix(stored, head))
}

func normalizeCommit(commit string) (string, bool) {
	commit = strings.ToLower(strings.TrimSpace(commit))
	if len(commit) < 7 || len(commit) > 40 {
		return "", false
	}
	for i := 0; i < len(commit); i++ {
		if (commit[i] < '0' || commit[i] > '9') && (commit[i] < 'a' || commit[i] > 'f') {
			return "", false
		}
	}
	return commit, true
}
