package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/target"
)

// PM2 管理 —— 进程清单/聚合。与容器管理(server_containers.go)同一设计:经 SSH 跑
// **固定白名单只读命令**(`pm2 jlist`)采集,目标机零侵入。容错纪律相同:
//   - 某台不可达 / 认证失败 → 该台 reachable:false + 人读 error,**不 500**,不连累其它台。
//   - 某台连得上但无 pm2(未安装 / 不在 PATH)→ runtime:"" + 人读 error,processes 空,
//     仍 reachable:true(连接本身没问题)。
//   - 批量端点逐台并行采集,有界并发(信号量防 N 台同时 SSH 打爆)。
//   - 采集结果按台进程内 TTL 缓存 + single-flight(见 pm2Cache):前端轮询与多标签页
//     不再放大 SSH 次数;pm2 变更类写操作成功投递后主动失效(server_ops.go)。
//
// AC-SEC-02:采集命令是纯静态命令 array;进程**生命周期**(起停/重启/重载/删除)复用既有
// `POST /servers/{id}/service/action`(type=pm2),那里已做严格白名单 + 审计;pm2 分支
// 经 execPM2 投递(见下),PATH 回退只拼白名单值。

const (
	// pm2Concurrency 是批量采集的最大并发 SSH 数(与容器域一致,防打爆)。
	pm2Concurrency = 6
	// pm2CmdTimeout 是单台采集的整体超时。pm2 守护进程冷启动(God daemon 首次拉起)比
	// docker ps 慢,放宽到 20s。
	pm2CmdTimeout = 20 * time.Second
	// pm2OutMax 是 jlist 输出解析前的截断上限(防超大输出撑爆内存)。
	pm2OutMax = 4 << 20 // 4 MiB
	// pm2NotFoundExit 是「命令不存在」的约定退出码(SSH exec 与本机 os/exec 均如此)。
	pm2NotFoundExit = 127
)

// execPM2 在目标机执行 pm2 子命令(args 只允许白名单枚举/正则校验值,或静态字面量)。
// pm2 常经 npm 全局安装,而 nvm 系安装的 bin 目录只在**登录 shell** 注入 PATH —— 非交互
// SSH 会话里 `pm2` 会 127(命令不存在)。故 127 时回退 `bash -lc`(登录 shell,读
// ~/.profile / ~/.bashrc 的 nvm 注入)再试一次。内层命令串由白名单值静态拼接,无用户
// 自由文本,注入面与 servercmd 的 sh -c 同一先例,且外层仍经 quoteArgs 转义。
func execPM2(ctx context.Context, svc target.Service, id string, args ...string) (*target.ExecResult, error) {
	res, err := svc.Exec(ctx, id, append([]string{"pm2"}, args...))
	if err != nil {
		return res, err
	}
	if res.ExitCode == pm2NotFoundExit {
		return svc.Exec(ctx, id, []string{"bash", "-lc", "pm2 " + strings.Join(args, " ")})
	}
	return res, nil
}

// pm2ProcessDTO 是单个 pm2 进程的展示 DTO(冻结契约字段形状)。
// cluster 模式下同一 name 有多个实例(不同 pm_id),前端行 key 用 name#id。
type pm2ProcessDTO struct {
	ID          int     `json:"id"`          // pm_id
	Name        string  `json:"name"`        // 进程名(pm2 start --name / 脚本名)
	PID         int     `json:"pid"`         // 系统 PID;stopped 时为 0
	Status      string  `json:"status"`      // online/stopping/stopped/launching/errored
	Restarts    int     `json:"restarts"`    // 累计重启次数
	Uptime      int64   `json:"uptime"`      // 最近一次启动的毫秒时间戳(pm_uptime);stopped 为 0
	CPU         float64 `json:"cpu"`         // 采样 CPU 百分比(monit.cpu)
	Memory      int64   `json:"memory"`      // 常驻内存字节(monit.memory)
	ExecMode    string  `json:"execMode"`    // fork_mode / cluster_mode
	Instances   string  `json:"instances"`   // 实例数(数字或 "max")
	Interpreter string  `json:"interpreter"` // 解释器(通常 node)
	Script      string  `json:"script"`      // 启动脚本(pm2_env.script)
	Cwd         string  `json:"cwd"`         // 工作目录(pm2_env.pm_cwd)
	CreatedAt   int64   `json:"createdAt"`   // 首次托管时间(毫秒时间戳)
	OutLog      string  `json:"outLog"`      // stdout 日志文件路径
	ErrLog      string  `json:"errLog"`      // stderr 日志文件路径
}

// serverPm2DTO 是单台服务器的 pm2 清单响应体(冻结契约)。语义与 serverContainersDTO
// 一致:reachable:false → 连接/认证失败;reachable:true 且 runtime:"" → 连得上但无 pm2。
type serverPm2DTO struct {
	ServerID    string          `json:"serverId"`
	Reachable   bool            `json:"reachable"`
	Runtime     string          `json:"runtime"` // "pm2" | ""(未检测到)
	Error       string          `json:"error"`
	Processes   []pm2ProcessDTO `json:"processes"`
	Online      int             `json:"online"`
	Total       int             `json:"total"`
	CollectedAt string          `json:"collectedAt"`
}

// pm2JListEntry 映射 `pm2 jlist` 的输出条目(仅取展示所需字段)。
type pm2JListEntry struct {
	Name  string `json:"name"`
	PID   int    `json:"pid"`
	PmID  int    `json:"pm_id"`
	Monit struct {
		Memory int64   `json:"memory"`
		CPU    float64 `json:"cpu"`
	} `json:"monit"`
	Pm2Env struct {
		Status      string `json:"status"`
		PmUptime    int64  `json:"pm_uptime"`
		RestartTime int    `json:"restart_time"`
		CreatedAt   int64  `json:"created_at"`
		ExecMode    string `json:"exec_mode"`
		Instances   any    `json:"instances"` // 数字或 "max"
		Script      string `json:"script"`
		PmCwd       string `json:"pm_cwd"`
		Interpreter string `json:"interpreter"`
		ErrorFile   string `json:"error_file"`
		OutFile     string `json:"out_file"`
	} `json:"pm2_env"`
}

// normalizePM2Instances 把 pm2_env.instances(数字或 "max" 字符串)归一为展示字符串。
func normalizePM2Instances(v any) string {
	switch n := v.(type) {
	case float64:
		return strconv.Itoa(int(n))
	case string:
		return n
	default:
		return ""
	}
}

// normalizePM2Status 归一 pm2 状态枚举(one-launch-status → stopped;未知值原样透传,
// 前端按 unknown 桶展示)。
func normalizePM2Status(s string) string {
	switch s {
	case "one-launch-status":
		return "stopped"
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}

// parsePM2JList 解析 `pm2 jlist` 输出为 DTO 切片。jlist 是单行紧凑 JSON 数组,但 pm2
// 可能在 JSON 前打印告警行 → 定位首个 `[` 再整体反序列化。解析失败 → 空列表(best-effort,
// 不报错,与容器域逐行跳过同精神)。
func parsePM2JList(out string) []pm2ProcessDTO {
	list := []pm2ProcessDTO{}
	if len(out) > pm2OutMax {
		out = out[:pm2OutMax]
	}
	idx := strings.Index(out, "[")
	if idx < 0 {
		return list
	}
	var raw []pm2JListEntry
	if err := json.Unmarshal([]byte(out[idx:]), &raw); err != nil {
		return list
	}
	for _, e := range raw {
		list = append(list, pm2ProcessDTO{
			ID:          e.PmID,
			Name:        e.Name,
			PID:         e.PID,
			Status:      normalizePM2Status(e.Pm2Env.Status),
			Restarts:    e.Pm2Env.RestartTime,
			Uptime:      e.Pm2Env.PmUptime,
			CPU:         e.Monit.CPU,
			Memory:      e.Monit.Memory,
			ExecMode:    e.Pm2Env.ExecMode,
			Instances:   normalizePM2Instances(e.Pm2Env.Instances),
			Interpreter: e.Pm2Env.Interpreter,
			Script:      e.Pm2Env.Script,
			Cwd:         e.Pm2Env.PmCwd,
			CreatedAt:   e.Pm2Env.CreatedAt,
			OutLog:      e.Pm2Env.OutFile,
			ErrLog:      e.Pm2Env.ErrorFile,
		})
	}
	return list
}

// collectServerPM2 对单台服务器采集 pm2 清单。永不返回 error 给批量端点用(与容器域
// 同约定):不可达/无 pm2 均落到 DTO;第二个返回值是「定位类」错误,仅供单台端点映射
// 404/422/503。
func collectServerPM2(ctx context.Context, svc target.Service, id string) (serverPm2DTO, error) {
	out := serverPm2DTO{
		ServerID:    id,
		Processes:   []pm2ProcessDTO{},
		CollectedAt: time.Now().UTC().Format(time.RFC3339),
	}

	cctx, cancel := context.WithTimeout(ctx, pm2CmdTimeout)
	defer cancel()

	// jlist 是静态命令;execPM2 负责 127 → bash -lc 的 PATH 回退。
	res, err := execPM2(cctx, svc, id, "jlist")
	if err != nil {
		// SSH 层失败(连接/认证/定位)→ reachable:false。
		out.Reachable = false
		out.Error = humanPM2Error(err)
		if isLocateError(err) {
			return out, err
		}
		return out, nil
	}
	// 命令已投递 → 连接本身没问题。
	out.Reachable = true
	if res.ExitCode != 0 {
		// 回退后仍非零:未安装,或 pm2 守护进程自身故障。人读 error,不 500。
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		if msg == "" {
			out.Error = "未检测到 pm2(未安装,或不在 PATH)"
		} else {
			out.Error = "pm2 不可用:" + truncateLog(msg, 200)
		}
		return out, nil
	}
	out.Runtime = "pm2"
	out.Processes = parsePM2JList(res.Stdout)
	out.Total = len(out.Processes)
	for _, p := range out.Processes {
		if p.Status == "online" || p.Status == "launching" {
			out.Online++
		}
	}
	return out, nil
}

// humanPM2Error 把领域错误映射为人读文案(绝不含凭据明文/内部栈)。
func humanPM2Error(err error) string {
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
		return "采集 pm2 清单失败:连接或命令执行错误"
	}
}

// --- 采集缓存(TTL + single-flight)---

// pm2CacheTTL 是单台 pm2 清单采集结果的进程内缓存时长(前端 12s 轮询,30s ≈ 2.5 个
// 周期;与容器域同款权衡,见 containersCacheTTL 注释)。变更类写操作(service/action
// type=pm2)成功投递后主动失效。
const pm2CacheTTL = 30 * time.Second

// pm2Cache 是 per-server 采集结果缓存(与 containersCache 同结构:失败结果同样缓存,
// 同台并发未命中合并为一次采集;进程内状态,重启即清空)。
type pm2Cache struct {
	mu       sync.Mutex
	ttl      time.Duration
	now      func() time.Time
	entries  map[string]pm2CacheEntry
	inflight map[string]chan struct{}
}

type pm2CacheEntry struct {
	dto serverPm2DTO
	at  time.Time
}

func newPM2Cache(ttl time.Duration) *pm2Cache {
	return &pm2Cache{
		ttl:      ttl,
		now:      time.Now,
		entries:  map[string]pm2CacheEntry{},
		inflight: map[string]chan struct{}{},
	}
}

// serverPM2Cache 是 pm2 清单的进程级缓存实例(两个只读端点共享,写操作失效它)。
var serverPM2Cache = newPM2Cache(pm2CacheTTL)

type pm2CollectFunc func(ctx context.Context) (serverPm2DTO, error)

// get 语义与 containersCache.get 相同:TTL 内回放;未命中采集一次(定位类错误不入
// 缓存);并发对同一 id 合并为一次采集;等待方随 ctx 取消拿到占位 DTO。
func (c *pm2Cache) get(ctx context.Context, id string, collect pm2CollectFunc) (serverPm2DTO, error) {
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

		// 脱离请求 ctx 采集(结果共享,不随发起请求断开中止);collect 内部再套超时。
		dto, locErr := collect(context.Background())
		c.mu.Lock()
		delete(c.inflight, id)
		if locErr == nil {
			c.entries[id] = pm2CacheEntry{dto: dto, at: c.now()}
		}
		c.mu.Unlock()
		close(wait)
		return dto, locErr
	}
	c.mu.Unlock()

	select {
	case <-wait:
		c.mu.Lock()
		e, ok := c.entries[id]
		c.mu.Unlock()
		if ok {
			return e.dto, nil
		}
	case <-ctx.Done():
		return serverPm2DTO{ServerID: id, Processes: []pm2ProcessDTO{}, Error: "等待采集结果时请求已取消"}, nil
	}
	return collect(ctx)
}

// invalidate 丢弃 id 的缓存条目(pm2 变更类写操作成功投递后调用)。
func (c *pm2Cache) invalidate(id string) {
	c.mu.Lock()
	delete(c.entries, id)
	c.mu.Unlock()
}

// collectServerPM2Cached 经缓存采集单台(handlers 的唯一入口)。
func collectServerPM2Cached(ctx context.Context, svc target.Service, id string) (serverPm2DTO, error) {
	return serverPM2Cache.get(ctx, id, func(ctx context.Context) (serverPm2DTO, error) {
		return collectServerPM2(ctx, svc, id)
	})
}

// --- HTTP handlers ---

// makeServerPM2Handler 返回 GET /api/servers/{id}/pm2(认证,只读;支持 __local__ 本机)。
// 服务器不存在/凭据不存在/保险库未配 → 标准状态码;连接/采集失败 → 200 + reachable:false,
// 不 500。结果经进程内 TTL 缓存回放;?refresh=1 强制重采。
func makeServerPM2Handler(svc target.Service) http.HandlerFunc {
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
			serverPM2Cache.invalidate(id)
		}
		out, locErr := collectServerPM2Cached(r.Context(), svc, id)
		if locErr != nil {
			writeServerError(w, locErr)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// makeAllServerPM2Handler 返回 GET /api/servers/pm2(认证,只读;批量聚合)。
// 逐台并行采集(有界并发),各自独立:某台失败仅该台 reachable:false,不连累其它台、
// 不 500。结果经进程内 TTL 缓存回放;?refresh=1 对全部列出的服务器强制重采。
func makeAllServerPM2Handler(svc target.Service) http.HandlerFunc {
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

		items := make([]serverPm2DTO, len(servers))
		sem := make(chan struct{}, pm2Concurrency)
		var wg sync.WaitGroup
		for i, srv := range servers {
			if refresh {
				serverPM2Cache.invalidate(srv.ID)
			}
			wg.Add(1)
			go func(i int, id string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				items[i], _ = collectServerPM2Cached(r.Context(), svc, id)
			}(i, srv.ID)
		}
		wg.Wait()
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}
