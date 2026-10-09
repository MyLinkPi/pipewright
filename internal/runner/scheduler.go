package runner

// scheduler.go 是「构建机池调度器」(FR-8-19):把 stage 的构建派发到打了标签的服务器池里
// 最合适的一台机上执行。与 runner.Service(项目→选择器的配置存取)并列;不改其冻结签名。
//
// 选机算法(Acquire,三要素:标签 → 优先级 → 亲和):
//  1. 标签匹配:`server:<id>` 钉死单机,或标签项 AND 匹配 servers.labels → 候选集
//     (未打标签的服务器永不入选;无命中 → ErrNoRunnerMatch,配置错立即失败不排队)。
//  2. 健康过滤:经注入的 HealthProber SSH 实连探测(可达缓存 5min / 不可达缓存 30s,
//     防每次调度都拨号、也防坏机被反复锤);全不可达 → ErrNoRunnerAvailable。
//  3. 排序抢槽:priority 降序 → 本流水线最近用过的机器优先(亲和:命中该机 docker 镜像层
//     缓存,省拉取)→ in-flight 升序(同优先级无亲和时最少负载)→ 名称。依序非阻塞抢
//     内存槽位(每机容量 = servers.max_builds,0 = 全局默认,默认 1 → 单机构建串行)。
//  4. 全忙:进 FIFO 等待队列,任一 release 唤醒队首重跑 3(亲和机忙则用其他空闲机,不空等);
//     ctx 取消可中断(运行中止/取消)。公平性:槽空但已有等待者排队、或存在已送达未兑现
//     的唤醒(pending,被点名者弹出队列尚未重试)时,新请求直接入队不插队;放弃的等待者
//     若已收到唤醒,会把唤醒转发给队列下一位,不吞信号。
//
// 槽位与亲和均为进程内存态:平台是单进程,重启即失(语义本就是"当前负载/最近使用",不持久化)。
// run 级准入闸门(run 包全局/项目计数)不受影响 —— 机器维度在本层约束,两层正交。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// 领域错误。
var (
	// ErrNoRunnerMatch 表示选择器在服务器池中无命中(标签拼错/机器未打标签)。
	ErrNoRunnerMatch = errors.New("runner: no server matches selector")
	// ErrNoRunnerAvailable 表示有命中但全部健康探测不可达(或钉死的机器已不存在)。
	ErrNoRunnerAvailable = errors.New("runner: matched servers all unreachable")
)

// 健康探测缓存 TTL:可达的结果信任更久,不可达的快速重试。
const (
	healthOKTTL   = 5 * time.Minute
	healthFailTTL = 30 * time.Second
	// slotCapMax 是单机槽位上限(与 httpapi 校验一致,防误填把机器打爆)。
	slotCapMax = 64
)

// HealthProber 抽象「探测某服务器是否可经 SSH 连通」(main.go 里用 target.Service 适配,
// 同 ServerExister 模式,保持本包不直依赖 target)。nil = 不探测(全部视为可达,便于单测)。
type HealthProber interface {
	Probe(ctx context.Context, serverID string) bool
}

// Scheduler 是构建机池调度器。构造后并发安全;Acquire 阻塞语义见文件头。
type Scheduler struct {
	db           *sql.DB
	prober       HealthProber
	defaultSlots int // max_builds=0 时的默认槽位;<=0 → 1

	mu       sync.Mutex
	inflight map[string]int      // serverID → 当前占用槽位数
	lastUsed map[string]string   // pipelineID(项目 id)→ 最近一次选中机器(亲和)
	health   map[string]healthAt // serverID → 探测缓存
	waiters  []chan struct{}     // FIFO 等待队列(全忙时挂起,release 唤醒队首)
	// pending 是已送达未兑现的唤醒数:等待者被 wakeNextLocked 弹出队列后即收到一笔
	// 唤醒,但其重试尚未发生 —— 这段窗口内它不在 waiters 里,却已对某个空槽有在先
	// 主张(claim)。新 Acquire 的公平性闸门必须把它算进去,否则会在这个窗口里插队
	// 抢走本属于被唤醒者的槽(见 pickOrEnqueue)。
	pending int
}

// healthAt 是一条健康探测缓存结果。
type healthAt struct {
	ok bool
	at time.Time
}

// candidate 是调度候选机(一次 Acquire 尝试的快照)。
type candidate struct {
	id        string
	name      string
	host      string
	labels    string
	maxBuilds int
	priority  int
}

// displayName 返回机器的人读显示名:优先「name(host)」,host 空 → name,name 也空 → id 兜底
// (历史数据/异常行不至于退回不可读 uuid,除非真的只有 uuid 可给)。
func (c candidate) displayName() string {
	switch {
	case c.name != "" && c.host != "":
		return c.name + "(" + c.host + ")"
	case c.name != "":
		return c.name
	default:
		return c.id
	}
}

// NewScheduler 构造调度器。defaultSlots 为 max_builds=0 机器的默认槽位(<=0 视为 1,
// 与环境变量 PIPEWRIGHT_RUNNER_SLOTS 的默认一致)。
func NewScheduler(db *sql.DB, prober HealthProber, defaultSlots int) *Scheduler {
	if defaultSlots <= 0 {
		defaultSlots = 1
	}
	return &Scheduler{
		db:           db,
		prober:       prober,
		defaultSlots: defaultSlots,
		inflight:     make(map[string]int),
		lastUsed:     make(map[string]string),
		health:       make(map[string]healthAt),
	}
}

// Acquire 按选择器选中一台构建机并占用其一个槽位,返回 (机器 id, 人读显示名 name(host), release)。
// serverName 供派发日志/步骤快照展示(机器改名后不回溯历史日志,故取当时的快照即可);
// release 必须被调用(派发侧 defer)以归还槽位并唤醒等待者;幂等安全。
// log 可为 nil;用于把"等待槽位/选中哪台"写进阶段日志(可见性)。
func (s *Scheduler) Acquire(ctx context.Context, pipelineID, selector string, log func(string)) (string, string, func(), error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return "", "", nil, ErrInvalidSelector // 空=本地,派发侧负责短路,不该走到这
	}
	pinned, isPinned := PinnedServer(selector)
	terms := []string(nil)
	if !isPinned { // 钉死形式含 ':',不走标签项解析
		var err error
		if terms, err = ParseSelector(selector); err != nil {
			return "", "", nil, fmt.Errorf("%w: %s", ErrInvalidSelector, selector)
		}
	}

	for attempt := 0; ; attempt++ {
		woken := attempt > 0 // 被唤醒后的重试:已消费一笔唤醒,提前失败须转发(见 forwardWake)
		cands, err := s.candidates(ctx, pinned, isPinned, terms)
		if err != nil {
			s.forwardWake(woken)
			return "", "", nil, err
		}
		if len(cands) == 0 {
			s.forwardWake(woken)
			return "", "", nil, ErrNoRunnerMatch
		}

		healthy := s.filterHealthy(ctx, cands)
		if len(healthy) == 0 {
			s.forwardWake(woken)
			return "", "", nil, ErrNoRunnerAvailable
		}

		// 抢槽与入队在同一临界区完成(pickOrEnqueue):若分成两步,两步之间的 release
		// 会打向空队列丢信号,等待者将在已有空槽的情况下挂到 ctx 超时(missed wakeup)。
		w := make(chan struct{}, 1)
		// woken ⟺ 上一轮是被 release/转发点名唤醒后的重试:允许越过队列抢槽
		// (本等待者本就是 FIFO 里最早的一位),公平性细则见 pickOrEnqueue。
		if c, release, ok := s.pickOrEnqueue(pipelineID, healthy, w, woken); ok {
			return c.id, c.displayName(), release, nil
		}
		// 走到这 = 候选机确实全忙且本等待者已入队;仅首次尝试打日志(被唤醒重试不刷屏),
		// 且必须在 pickOrEnqueue 之后 —— 否则有空槽时也会误报"全忙排队"。
		if attempt == 0 && log != nil {
			busy := make([]string, 0, len(healthy))
			for _, c := range healthy {
				busy = append(busy, c.name)
			}
			log(fmt.Sprintf("构建机池全忙(%s),排队等待槽位…", strings.Join(busy, ", ")))
		}
		select {
		case <-w:
			// 已收到唤醒但 ctx 同时取消:不能把这笔唤醒带进坟墓——转发给队列下一位
			// 再退出,语义与下方 ctx.Done 分支一致。
			if ctx.Err() != nil {
				s.forwardWake(true)
				return "", "", nil, ctx.Err()
			}
			continue
		case <-ctx.Done():
			// 与 release 的唤醒竞态:信号可能已投进 w(buffered)但本等待者选择放弃。
			// 若直接剔出队列,这笔已送达的唤醒被吞——槽已空,其余等待者无人唤醒,
			// 只能干等下一次 release(missed wakeup)。故持锁先非阻塞查证:已收到就
			// 替它把唤醒转发给队列下一位(等价于替它再执行一次 release 的唤醒);
			// 未收到才把自己剔出队列。查证与剔除必须在同一把锁内,否则 release 的
			// 投递可能落在两者之间,再次丢信号。
			s.mu.Lock()
			select {
			case <-w:
				s.forwardWakeLocked()
			default:
				s.removeWaiterLocked(w)
			}
			s.mu.Unlock()
			return "", "", nil, ctx.Err()
		}
	}
}

// candidates 查库取候选集:钉死形式按 id 取一台(不存在 → ErrNoRunnerMatch 语义由空集表达);
// 标签形式全表扫描后 AND 匹配。
func (s *Scheduler) candidates(ctx context.Context, pinned string, isPinned bool, terms []string) ([]candidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, COALESCE(name,''), COALESCE(host,''), COALESCE(labels,''), COALESCE(max_builds,0), COALESCE(priority,0) FROM servers`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.name, &c.host, &c.labels, &c.maxBuilds, &c.priority); err != nil {
			return nil, err
		}
		if isPinned {
			if c.id == pinned {
				out = append(out, c)
			}
			continue
		}
		if MatchSelector(c.labels, terms) {
			out = append(out, c)
		}
	}
	return out, rows.Err()
}

// filterHealthy 按探测缓存过滤候选;无 prober 时全保留。缓存 TTL 见常量。
func (s *Scheduler) filterHealthy(ctx context.Context, cands []candidate) []candidate {
	if s.prober == nil {
		return cands
	}
	now := time.Now()
	out := make([]candidate, 0, len(cands))
	for _, c := range cands {
		s.mu.Lock()
		h, seen := s.health[c.id]
		s.mu.Unlock()
		ttl := healthFailTTL
		if h.ok {
			ttl = healthOKTTL
		}
		if seen && now.Sub(h.at) < ttl {
			if h.ok {
				out = append(out, c)
			}
			continue
		}
		ok := s.prober.Probe(ctx, c.id)
		if ctx.Err() != nil {
			// ctx 取消造成的探测失败不写入负缓存:否则后续 30s 内其他流水线会误判该机不可达。
			continue
		}
		s.mu.Lock()
		s.health[c.id] = healthAt{ok: ok, at: now}
		s.mu.Unlock()
		if ok {
			out = append(out, c)
		}
	}
	return out
}

// pickOrEnqueue 按序(优先级降序 → 亲和 → 负载 → 名称)非阻塞抢槽;抢到则占用槽位并更新
// 亲和记录,返回选中的候选与 release;全忙则把 w 原子追加进等待队列(与抢槽同一临界区,防丢唤醒)。
// wasWoken 表示本次调用是被 release/转发点名唤醒后的重试(见 Acquire 的 attempt)。
func (s *Scheduler) pickOrEnqueue(pipelineID string, cands []candidate, w chan struct{}, wasWoken bool) (candidate, func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 公平性:有空槽但已有更早的主张者(队列中的等待者,或已被点名唤醒却尚未重试的
	// pending 主张)时,新 Acquire 必须排队,不得插队抢槽,否则 FIFO 语义被破坏。
	// 被唤醒重试的等待者不受此闸:它是被 release 按 FIFO 点名的最老者,先注销自己那笔
	// pending 主张再抢槽;若让它重排队尾,既颠倒次序,又会在"槽空 + 队列非空"时无人能
	// 拿槽而死锁。
	if wasWoken && s.pending > 0 {
		s.pending--
	}
	if !wasWoken && (len(s.waiters) > 0 || s.pending > 0) {
		s.waiters = append(s.waiters, w)
		return candidate{}, nil, false
	}
	last := s.lastUsed[pipelineID]
	sorted := make([]candidate, len(cands))
	copy(sorted, cands)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.priority != b.priority {
			return a.priority > b.priority // 优先级高者先
		}
		if (a.id == last) != (b.id == last) {
			return a.id == last // 同优先级:本流水线最近用过的机优先(亲和)
		}
		ai, bi := s.inflight[a.id], s.inflight[b.id]
		if ai != bi {
			return ai < bi // 再看谁更空闲
		}
		return a.name < b.name
	})
	for _, c := range sorted {
		cap := c.maxBuilds
		if cap <= 0 {
			cap = s.defaultSlots
		}
		if cap > slotCapMax {
			cap = slotCapMax
		}
		if s.inflight[c.id] >= cap {
			continue
		}
		s.inflight[c.id]++
		s.lastUsed[pipelineID] = c.id
		return c, s.makeRelease(c.id), true
	}
	s.waiters = append(s.waiters, w)
	return candidate{}, nil, false
}

// makeRelease 返回幂等的槽位归还函数:减计数并唤醒队首等待者。
func (s *Scheduler) makeRelease(serverID string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			if s.inflight[serverID] > 0 {
				s.inflight[serverID]--
			}
			s.wakeNextLocked()
			s.mu.Unlock()
		})
	}
}

// forwardWake 把一笔已消费的唤醒转发给队列下一位。用于"被唤醒重试却提前失败/放弃"
// 的路径(ctx 取消、候选集或健康状态在重试期间变化等):那笔唤醒代表"可能有空槽",
// 若随失败被吞,槽空着而其余等待者无人唤醒,只能干等下一次 release(missed wakeup)。
// woken=false 表示首次尝试就失败,未消费任何唤醒,不得多发(会凭空多唤醒一人)。
// 转发是保守的:被多唤醒的等待者重试发现全忙会重新入队,无副作用。
func (s *Scheduler) forwardWake(woken bool) {
	if !woken {
		return
	}
	s.mu.Lock()
	s.forwardWakeLocked()
	s.mu.Unlock()
}

// forwardWakeLocked 是 forwardWake 的持锁版(须持锁调用):注销自己那笔 pending 主张,
// 再把唤醒点名给队列下一位(若送达,wakeNextLocked 会为下一位重新记账 —— 主张被转移
// 而非新增;队列已空则主张随空槽蒸发,之后的新 Acquire 可直取)。
func (s *Scheduler) forwardWakeLocked() {
	if s.pending > 0 {
		s.pending--
	}
	s.wakeNextLocked()
}

// removeWaiterLocked 剔除一个已放弃的等待者(须持锁调用)。
// 防 release 信号打向无人接收的管道,饿死后继。调用方须先确认 w 内无已送达信号
// (见 Acquire 的 ctx.Done 分支),否则那笔唤醒会随剔除一起被吞。
func (s *Scheduler) removeWaiterLocked(w chan struct{}) {
	for i, x := range s.waiters {
		if x == w {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			return
		}
	}
}

// wakeNextLocked 唤醒队首等待者(须持锁调用)。非阻塞投递:等待者收不到说明已放弃,剔出即可。
// 每送达一笔,s.pending 记一笔"已送达未兑现"的主张 —— 被点名者弹出队列后、重试发生前
// 不再出现在 waiters 里,公平性闸门靠 pending 认出它(见 pickOrEnqueue)。
func (s *Scheduler) wakeNextLocked() {
	for len(s.waiters) > 0 {
		w := s.waiters[0]
		s.waiters = s.waiters[1:]
		select {
		case w <- struct{}{}:
			s.pending++
			return
		default:
		}
	}
}
