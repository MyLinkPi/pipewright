package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/target"
)

// 容器管理 —— 列表/聚合(Portainer 式「统计所有服务器上的容器」入口能力)。
//
// 设计与 6-1(server_metrics.go)一脉相承:经 SSH 跑**固定白名单只读命令**采集,目标机
// 零侵入(不装 agent、不开 Docker API socket)。容错纪律相同:
//   - 某台不可达 / 认证失败 → 该台 reachable:false + 人读 error,**不 500**,不连累其它台。
//   - 某台连得上但无容器运行时(docker 未装 / 当前用户无权限)→ runtime:"" + 人读 error,
//     containers 空,仍 reachable:true(连接本身没问题)。
//   - 批量端点逐台并行采集,有界并发(信号量防 N 台同时 SSH 打爆)。
//   - 采集结果按台进程内 TTL 缓存 + single-flight(见 containersCache):前端轮询与多
//     标签页不再放大 SSH 次数;容器变更类写操作成功投递后主动失效,操作后刷新即拿新数据。
//
// AC-SEC-02:采集命令是**纯静态命令 array**(`docker ps -a --format {{json .}}`),绝不接受
// 任何用户输入拼接 —— 无注入面。容器**生命周期**(起停重启/暂停/kill/删除)复用既有
// `POST /servers/{id}/service/action`(type=docker),那里已做严格白名单 + 审计。

const (
	// containersConcurrency 是批量采集的最大并发 SSH 数(有界,防打爆)。
	containersConcurrency = 6
	// containersCmdTimeout 是单台采集的整体超时。
	containersCmdTimeout = 15 * time.Second
	// containersOutMax 是 ps 输出解析前的截断上限(防超大输出撑爆内存;几百容器也远不及)。
	containersOutMax = 4 << 20 // 4 MiB
)

// containerRuntimePref 是容器运行时探测优先级。**当前仅 docker 已实现**;命令形态高度一致,
// 后续补 podman/nerdctl(containerd)只需在此 append + 适配个别 format 差异 —— 抽象已预留。
var containerRuntimePref = []string{"docker"}

// cmdContainerPS 构造列容器命令 array(不拼 shell)。`-a` 含已停止容器;`--no-trunc` 给完整
// ID(生命周期操作用全 ID 无歧义);`--format {{json .}}` 每行一个 JSON 对象。
func cmdContainerPS(bin string) []string {
	return []string{bin, "ps", "-a", "--no-trunc", "--format", "{{json .}}"}
}

// containerDTO 是单个容器的展示 DTO(冻结契约字段形状)。
type containerDTO struct {
	ID        string `json:"id"`        // 完整容器 ID(FE 自行截短展示)
	Names     string `json:"names"`     // 主名(已去前导 `/`)
	Image     string `json:"image"`     // 镜像名:tag
	State     string `json:"state"`     // running/exited/paused/created/restarting/dead/unknown
	Status    string `json:"status"`    // 人读状态(如 "Up 2 hours" / "Exited (0) 3 days ago")
	Ports     string `json:"ports"`     // 端口映射原文(如 "0.0.0.0:80->80/tcp")
	CreatedAt string `json:"createdAt"` // 创建时间原文
}

// serverContainersDTO 是单台服务器的容器清单响应体(冻结契约)。
//   - reachable:false 时 containers 空、error 人读非空(连接/认证/定位类失败)。
//   - reachable:true 且 runtime:"" 时表示连得上但无容器运行时,error 人读说明。
//   - reachable:true 且 runtime 非空时 containers 为实际清单,running/total 为计数。
type serverContainersDTO struct {
	ServerID    string         `json:"serverId"`
	Reachable   bool           `json:"reachable"`
	Runtime     string         `json:"runtime"` // "docker" | ""(未检测到)
	Error       string         `json:"error"`
	Containers  []containerDTO `json:"containers"`
	Running     int            `json:"running"`
	Total       int            `json:"total"`
	CollectedAt string         `json:"collectedAt"`
}

// dockerPSLine 映射 `docker ps --format {{json .}}` 的逐行 JSON。仅取展示所需字段。
// State 字段于 Docker 20.10+ 才有;旧版缺失 → 留空,由 deriveState 据 Status 回退推断。
type dockerPSLine struct {
	ID        string `json:"ID"`
	Names     string `json:"Names"`
	Image     string `json:"Image"`
	State     string `json:"State"`
	Status    string `json:"Status"`
	Ports     string `json:"Ports"`
	CreatedAt string `json:"CreatedAt"`
}

// parseContainers 解析 `docker ps -a --format {{json .}}` 的多行 JSON 为 DTO 切片。
// 单行解析失败(格式异常)→ 跳过该行,不报错、不影响其它行(best-effort)。
func parseContainers(out string) []containerDTO {
	list := []containerDTO{}
	if len(out) > containersOutMax {
		out = out[:containersOutMax]
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var p dockerPSLine
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			continue
		}
		state := strings.ToLower(strings.TrimSpace(p.State))
		if state == "" {
			state = deriveState(p.Status)
		}
		list = append(list, containerDTO{
			ID:        p.ID,
			Names:     strings.TrimPrefix(p.Names, "/"),
			Image:     p.Image,
			State:     state,
			Status:    p.Status,
			Ports:     p.Ports,
			CreatedAt: p.CreatedAt,
		})
	}
	return list
}

// deriveState 在 ps 输出缺 State 字段(旧 Docker)时,据人读 Status 前缀推断容器状态。
func deriveState(status string) string {
	s := strings.TrimSpace(status)
	switch {
	case strings.HasPrefix(s, "Up") && strings.Contains(s, "(Paused)"):
		return "paused"
	case strings.HasPrefix(s, "Up"):
		return "running"
	case strings.HasPrefix(s, "Exited"):
		return "exited"
	case strings.HasPrefix(s, "Created"):
		return "created"
	case strings.HasPrefix(s, "Restarting"):
		return "restarting"
	case strings.HasPrefix(s, "Dead"):
		return "dead"
	default:
		return "unknown"
	}
}

// collectServerContainers 对单台服务器采集容器清单。永不返回 error 给批量端点用:
// 不可达/无运行时均落到 DTO(reachable/runtime/error)。第二个返回值是「定位类」错误
// (服务器/凭据不存在、保险库未配),仅供单台端点映射 404/422/503;批量端点忽略它。
func collectServerContainers(ctx context.Context, svc target.Service, id string) (serverContainersDTO, error) {
	out := serverContainersDTO{
		ServerID:    id,
		Containers:  []containerDTO{},
		CollectedAt: time.Now().UTC().Format(time.RFC3339),
	}

	cctx, cancel := context.WithTimeout(ctx, containersCmdTimeout)
	defer cancel()

	for _, bin := range containerRuntimePref {
		res, err := svc.Exec(cctx, id, cmdContainerPS(bin))
		if err != nil {
			// SSH 层失败(连接/认证/定位)——对任何运行时都一样,直接判 reachable:false。
			out.Reachable = false
			out.Error = humanContainersError(err)
			if isLocateError(err) {
				return out, err
			}
			return out, nil
		}
		// 命令已投递 → 连接本身没问题。
		out.Reachable = true
		if res.ExitCode != 0 {
			// 该运行时不存在 / 无权限(命令非零退出)→ 尝试下一个候选。
			continue
		}
		out.Runtime = bin
		out.Containers = parseContainers(res.Stdout)
		out.Total = len(out.Containers)
		for _, c := range out.Containers {
			if c.State == "running" {
				out.Running++
			}
		}
		return out, nil
	}

	// 连得上但没有任何候选运行时可用。
	out.Error = "未检测到容器运行时(docker 未安装,或当前用户无权限访问 docker)"
	return out, nil
}

// humanContainersError 把领域错误映射为人读文案(绝不含凭据明文/内部栈)。
func humanContainersError(err error) string {
	switch {
	case errors.Is(err, target.ErrAuth):
		return "SSH 认证失败:密钥或口令无效,或无登录权限"
	case errors.Is(err, target.ErrUnreachable):
		return "无法连接服务器:端口未开放、主机不可达或超时"
	case errors.Is(err, target.ErrInvalidCredential):
		return "凭据不是可用的 SSH 私钥或口令"
	case errors.Is(err, context.DeadlineExceeded):
		return "采集超时"
	default:
		return "采集容器清单失败:连接或命令执行错误"
	}
}

// --- 采集缓存(TTL + single-flight)---

// containersCacheTTL 是单台容器清单采集结果的进程内缓存时长。前端容器页每 12s 轮询一次
// 批量端点,不缓存则每次轮询都对全部服务器逐台 SSH 跑 docker ps —— 既慢(单台超时上限
// 15s),又对目标机形成连接风暴。缓存让 TTL 内的轮询直接回放上次结果;CollectedAt 保持
// 真实采集时间,消费方可感知数据年龄。30s ≈ 轮询周期的 2.5 倍(多数轮询命中),容器清单
// 本就是准实时视图,30s 陈旧可接受;变更类写操作(生命周期/建容器/栈部署/清理)成功投递
// 后主动失效缓存,操作后刷新即拿新数据。
const containersCacheTTL = 30 * time.Second

// containersCache 是 per-server 采集结果缓存,附带 single-flight:同一台并发未命中只允许
// 一个采集在飞,其余等待共享结果 —— 多标签页/多客户端同时轮询不再放大 SSH 次数。**失败
// 结果同样缓存**:不可达台恰是采集最慢的台(连接超时撑满 15s),不缓存会让它们拖慢每
// 一次轮询。进程内状态(与 build 包 remoteCLICache 同款思路):单实例部署,重启即清空。
type containersCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	now      func() time.Time // 注入时钟,便于单测 TTL 过期路径
	entries  map[string]containersCacheEntry
	inflight map[string]chan struct{}
}

// containersCacheEntry 是一台服务器的采集缓存。dto 按只读契约共享(handlers 仅序列化,
// 绝不修改;struct 拷贝返回,Containers 切片底层数组同样只读)。
type containersCacheEntry struct {
	dto serverContainersDTO
	at  time.Time
}

func newContainersCache(ttl time.Duration) *containersCache {
	return &containersCache{
		ttl:      ttl,
		now:      time.Now,
		entries:  map[string]containersCacheEntry{},
		inflight: map[string]chan struct{}{},
	}
}

// serverContainersCache 是容器清单的进程级缓存实例(两个只读端点共享,写操作失效它)。
var serverContainersCache = newContainersCache(containersCacheTTL)

// containersCollectFunc 采集一台服务器的容器清单。locErr 是「定位类」错误(服务器/凭据不
// 存在、保险库未配):带它返回的结果**不缓存** —— 那是调用参数/装配问题,缓存会把本应
// 映射 404/422/503 的错误错当成 reachable:false 的采集失败回放。
type containersCollectFunc func(ctx context.Context) (serverContainersDTO, error)

// get 返回 id 的容器清单:TTL 内命中直接回放;未命中/过期经 collect 采集一次并缓存(含
// reachable:false 的失败结果)。并发对同一 id 的调用合并为一次采集。ctx 取消只影响「等待
// 他人结果」的调用方(拿到人读 error 的占位 DTO);采集本身以 Background 跑满自身超时,
// 发起请求断开不连累共享同一结果的其它等待方,采集结果仍会入缓存供下次轮询使用。
func (c *containersCache) get(ctx context.Context, id string, collect containersCollectFunc) (serverContainersDTO, error) {
	c.mu.Lock()
	if e, ok := c.entries[id]; ok && c.now().Sub(e.at) < c.ttl {
		c.mu.Unlock()
		return e.dto, nil
	}
	wait, inflight := c.inflight[id]
	if !inflight {
		wait = make(chan struct{})
		c.inflight[id] = wait
		c.mu.Unlock()

		// 脱离请求 ctx 采集(结果会被其它等待方共享,不能随发起请求断开而中止);
		// collectServerContainers 内部再套 containersCmdTimeout,时长有界。
		dto, locErr := collect(context.Background())
		c.mu.Lock()
		delete(c.inflight, id)
		if locErr == nil {
			c.entries[id] = containersCacheEntry{dto: dto, at: c.now()}
		}
		c.mu.Unlock()
		close(wait)
		return dto, locErr
	}
	c.mu.Unlock()

	select {
	case <-wait:
		// 共享到他人结果(已入缓存;若其采集 locErr 失败则未入,重查落空 → 自采)。
		c.mu.Lock()
		e, ok := c.entries[id]
		c.mu.Unlock()
		if ok {
			return e.dto, nil
		}
	case <-ctx.Done():
		return serverContainersDTO{ServerID: id, Containers: []containerDTO{}, Error: "等待采集结果时请求已取消"}, nil
	}
	// 等到了他人采集但结果未入缓存(定位类失败 / 恰好被写操作失效)→ 自采一次直接返回,
	// 不回填缓存,避免与可能仍在飞的采集竞态。
	return collect(ctx)
}

// invalidate 丢弃 id 的缓存条目(容器变更类写操作成功投递后调用,让随后的读取立刻重新
// 采集而非回放旧清单)。
func (c *containersCache) invalidate(id string) {
	c.mu.Lock()
	delete(c.entries, id)
	c.mu.Unlock()
}

// collectServerContainersCached 经缓存采集单台(handlers 的唯一入口;refresh 时先失效)。
func collectServerContainersCached(ctx context.Context, svc target.Service, id string) (serverContainersDTO, error) {
	return serverContainersCache.get(ctx, id, func(ctx context.Context) (serverContainersDTO, error) {
		return collectServerContainers(ctx, svc, id)
	})
}

// wantContainersRefresh 解析只读端点的 ?refresh=1(操作员强制重采的逃生口:怀疑缓存与
// 真相不符时 curl 一下即可,无需等 TTL 或重启)。
func wantContainersRefresh(r *http.Request) bool {
	return r.URL.Query().Get("refresh") == "1"
}

// --- HTTP handlers ---

// makeServerContainersHandler 返回 GET /api/servers/{id}/containers(认证,只读)。
// 服务器不存在/凭据不存在/保险库未配 → 标准状态码;连接/采集失败 → 200 + reachable:false,不 500。
// 结果经进程内 TTL 缓存回放(见 containersCache);?refresh=1 强制重采。
func makeServerContainersHandler(svc target.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务器服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		if _, err := svc.Get(r.Context(), id); err != nil {
			writeServerError(w, err)
			return
		}
		if wantContainersRefresh(r) {
			serverContainersCache.invalidate(id)
		}
		out, locErr := collectServerContainersCached(r.Context(), svc, id)
		if locErr != nil {
			writeServerError(w, locErr)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// makeAllServerContainersHandler 返回 GET /api/servers/containers(认证,只读;批量聚合)。
// 逐台并行采集(有界并发),各自独立:某台失败仅该台 reachable:false,不连累其它台、不 500。
// 结果经进程内 TTL 缓存回放(前端每 12s 轮询不再每次都打 SSH;见 containersCache);
// ?refresh=1 对全部列出的服务器强制重采。
func makeAllServerContainersHandler(svc target.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务器服务未初始化")
			return
		}
		servers, err := svc.List(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		refresh := wantContainersRefresh(r)

		items := make([]serverContainersDTO, len(servers))
		sem := make(chan struct{}, containersConcurrency)
		var wg sync.WaitGroup
		for i, srv := range servers {
			if refresh {
				serverContainersCache.invalidate(srv.ID)
			}
			wg.Add(1)
			go func(i int, id string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				items[i], _ = collectServerContainersCached(r.Context(), svc, id)
			}(i, srv.ID)
		}
		wg.Wait()
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}
