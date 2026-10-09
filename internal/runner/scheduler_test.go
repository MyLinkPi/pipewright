package runner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/storetest"
)

// openMigrated 开一个全新已迁移库(含 0052 的 labels/max_builds/priority/selector 列)。
func openMigrated(t *testing.T) *store.Store {
	t.Helper()
	return storetest.Open(t)
}

// seedServer 插一台 servers 行(自带占位凭据满足 FK;调度器不触凭据)。
func seedServer(t *testing.T, st *store.Store, id, name, labels string, maxBuilds, priority int) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	credID := uuid.NewString()
	if _, err := st.DB.Exec(`INSERT INTO credentials (id,name,type,scope,ciphertext,masked_value,created_at,updated_at) VALUES (?,'c','ssh_key','',X'00','m',?,?)`, credID, now, now); err != nil {
		t.Fatalf("seed cred: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO servers (id,name,host,port,user,credential_id,created_at,updated_at,labels,max_builds,priority) VALUES (?,?,?,22,'u',?,?,?,?,?,?)`,
		id, name, name+".example", credID, now, now, labels, maxBuilds, priority); err != nil {
		t.Fatalf("seed server %s: %v", name, err)
	}
}

type fakeProber struct {
	mu   sync.Mutex
	down map[string]bool
	hits map[string]int
}

func (f *fakeProber) Probe(_ context.Context, id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hits == nil {
		f.hits = map[string]int{}
	}
	f.hits[id]++
	return !f.down[id]
}

func TestAcquirePicksHighestPriority(t *testing.T) {
	st := openMigrated(t)
	seedServer(t, st, "s-low", "low", "linux", 4, 1)
	seedServer(t, st, "s-high", "high", "linux", 4, 10)
	s := NewScheduler(st.DB, nil, 1)
	id, _, rel, err := s.Acquire(context.Background(), "p1", "linux", nil)
	if err != nil || id != "s-high" {
		t.Fatalf("Acquire = %q/%v, want s-high", id, err)
	}
	rel()
}

func TestAcquirePriorityBeatsAffinity(t *testing.T) {
	st := openMigrated(t)
	seedServer(t, st, "s-a", "alpha", "linux", 4, 1)
	seedServer(t, st, "s-b", "beta", "linux", 4, 10)
	s := NewScheduler(st.DB, nil, 1)
	id, _, rel, _ := s.Acquire(context.Background(), "p1", "linux", nil)
	_ = id
	rel() // p1 最近用过 s-b(高优先级)
	id2, _, rel2, _ := s.Acquire(context.Background(), "p1", "linux", nil)
	if id2 != "s-b" {
		t.Fatalf("高优先级应胜过亲和:got %s", id2)
	}
	rel2()
}

func TestAcquireAffinityWithinSamePriority(t *testing.T) {
	st := openMigrated(t)
	seedServer(t, st, "s-a", "alpha", "linux", 4, 5)
	seedServer(t, st, "s-b", "beta", "linux", 4, 5)
	s := NewScheduler(st.DB, nil, 1)
	id, _, rel, _ := s.Acquire(context.Background(), "p1", "linux", nil)
	rel()
	// 首选按名字兜底是 alpha;释放后同流水线再来,应亲和复用 alpha(而非因名字序回到 alpha ——
	// 换个反例:把 alpha 改名排后,仍应选它)。
	id2, _, rel2, _ := s.Acquire(context.Background(), "p1", "linux", nil)
	if id2 != id {
		t.Fatalf("同优先级应亲和最近使用机:首次 %s,二次 %s", id, id2)
	}
	rel2()
	// 其他流水线不共享亲和,走名字序 → 应选 alpha(排序兜底)而非被 p1 带偏。
	id3, _, rel3, _ := s.Acquire(context.Background(), "p2", "linux", nil)
	if id3 != "s-a" {
		t.Fatalf("无亲和历史应走名字序:got %s", id3)
	}
	rel3()
}

func TestAcquireAffinityBusyFallsBack(t *testing.T) {
	st := openMigrated(t)
	seedServer(t, st, "s-a", "alpha", "linux", 1, 5) // 单槽
	seedServer(t, st, "s-b", "beta", "linux", 1, 5)
	s := NewScheduler(st.DB, nil, 1)
	id, _, hold, _ := s.Acquire(context.Background(), "p1", "linux", nil)
	if id != "s-a" {
		t.Fatalf("首个应选 alpha,got %s", id)
	}
	// alpha(亲和机)忙 → 不空等,退让给同优先级的 beta。
	id2, _, rel2, _ := s.Acquire(context.Background(), "p1", "linux", nil)
	if id2 != "s-b" {
		t.Fatalf("亲和机忙应退让其他空闲机:got %s", id2)
	}
	rel2()
	hold()
}

func TestAcquireDefaultOneSlotSerializes(t *testing.T) {
	st := openMigrated(t)
	seedServer(t, st, "s-a", "alpha", "linux", 0, 0) // max_builds=0 → 默认槽位
	s := NewScheduler(st.DB, nil, 1)                 // 默认 1
	_, _, hold, err := s.Acquire(context.Background(), "p1", "linux", nil)
	if err != nil {
		t.Fatalf("首次 Acquire: %v", err)
	}
	done := make(chan struct{})
	go func() {
		id2, _, rel2, err2 := s.Acquire(context.Background(), "p1", "linux", nil)
		if err2 != nil || id2 != "s-a" {
			t.Errorf("排队后应等到同机:got %s/%v", id2, err2)
		}
		rel2()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("默认 1 槽应阻塞等待,不应立即拿到")
	case <-time.After(80 * time.Millisecond):
	}
	hold()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("release 后等待者应被唤醒")
	}
}

func TestAcquireParallelStagesFanOutThenQueue(t *testing.T) {
	st := openMigrated(t)
	seedServer(t, st, "s-a", "alpha", "linux", 2, 5)
	s := NewScheduler(st.DB, nil, 1)
	var rels []func()
	for i := 0; i < 2; i++ {
		_, _, r, err := s.Acquire(context.Background(), "p1", "linux", nil)
		if err != nil {
			t.Fatalf("第 %d 个槽应立即可得: %v", i+1, err)
		}
		rels = append(rels, r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, _, _, err := s.Acquire(ctx, "p1", "linux", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("满槽应阻塞至 ctx 超时: %v", err)
	}
	for _, r := range rels {
		r()
	}
	// release 后(等待者已放弃)再取应立即可得。
	if _, _, r, err := s.Acquire(context.Background(), "p1", "linux", nil); err != nil {
		t.Fatalf("全部释放后应可再取: %v", err)
	} else {
		r()
	}
}

func TestAcquireNoMatchAndUnreachable(t *testing.T) {
	st := openMigrated(t)
	seedServer(t, st, "s-a", "alpha", "", 1, 0) // 无标签 → 永不入选
	s := NewScheduler(st.DB, nil, 1)
	if _, _, _, err := s.Acquire(context.Background(), "p1", "linux", nil); !errors.Is(err, ErrNoRunnerMatch) {
		t.Fatalf("无命中应 ErrNoRunnerMatch, got %v", err)
	}

	seedServer(t, st, "s-b", "beta", "linux", 1, 0)
	p := &fakeProber{down: map[string]bool{"s-b": true}}
	s2 := NewScheduler(st.DB, p, 1)
	if _, _, _, err := s2.Acquire(context.Background(), "p1", "linux", nil); !errors.Is(err, ErrNoRunnerAvailable) {
		t.Fatalf("全不可达应 ErrNoRunnerAvailable, got %v", err)
	}
}

func TestAcquirePinnedServer(t *testing.T) {
	st := openMigrated(t)
	seedServer(t, st, "s-a", "alpha", "windows", 1, 0) // 标签不匹配也无所谓:钉死形式只看 id
	s := NewScheduler(st.DB, nil, 1)
	id, _, rel, err := s.Acquire(context.Background(), "p1", "server:s-a", nil)
	if err != nil || id != "s-a" {
		t.Fatalf("钉死形式应直取该机:got %s/%v", id, err)
	}
	rel()
	if _, _, _, err := s.Acquire(context.Background(), "p1", "server:ghost", nil); !errors.Is(err, ErrNoRunnerMatch) {
		t.Fatalf("钉死不存在的机应 ErrNoRunnerMatch, got %v", err)
	}
}

func TestAcquireInvalidSelector(t *testing.T) {
	st := openMigrated(t)
	s := NewScheduler(st.DB, nil, 1)
	if _, _, _, err := s.Acquire(context.Background(), "p1", "", nil); !errors.Is(err, ErrInvalidSelector) {
		t.Fatalf("空选择器应报非法: %v", err)
	}
	if _, _, _, err := s.Acquire(context.Background(), "p1", "arch=", nil); !errors.Is(err, ErrInvalidSelector) {
		t.Fatalf("坏语法应报非法: %v", err)
	}
}

func TestHealthProbeCached(t *testing.T) {
	st := openMigrated(t)
	seedServer(t, st, "s-a", "alpha", "linux", 4, 0)
	p := &fakeProber{}
	s := NewScheduler(st.DB, p, 1)
	for i := 0; i < 3; i++ {
		if _, _, rel, err := s.Acquire(context.Background(), "p1", "linux", nil); err != nil {
			t.Fatalf("Acquire #%d: %v", i+1, err)
		} else {
			rel()
		}
	}
	if n := p.hits["s-a"]; n != 1 {
		t.Fatalf("可达缓存 5min 内应只探测 1 次,实际 %d", n)
	}
}

func TestReleaseIdempotent(t *testing.T) {
	st := openMigrated(t)
	seedServer(t, st, "s-a", "alpha", "linux", 1, 0)
	s := NewScheduler(st.DB, nil, 1)
	_, _, rel, _ := s.Acquire(context.Background(), "p1", "linux", nil)
	rel()
	rel() // 重复 release 不得把计数打成负、不得二次唤醒
	if _, _, r, err := s.Acquire(context.Background(), "p1", "linux", nil); err != nil {
		t.Fatalf("幂等 release 后槽位应只归还一次并可再取: %v", err)
	} else {
		r()
	}
}

// probeFunc 把函数适配成 HealthProber(单测注入非常规探测行为)。
type probeFunc func(context.Context, string) bool

func (f probeFunc) Probe(ctx context.Context, id string) bool { return f(ctx, id) }

// 同优先级、无亲和历史时,应避开已占载的机器选更空闲者(in-flight 升序兜底,而非名字序)。
func TestAcquireLeastLoadedFallback(t *testing.T) {
	st := openMigrated(t)
	seedServer(t, st, "s-a", "alpha", "linux", 2, 5)
	seedServer(t, st, "s-b", "beta", "linux", 2, 5)
	s := NewScheduler(st.DB, nil, 1)
	// p1 占住 alpha 一个槽(名字序首选),不释放。
	id1, _, hold, err := s.Acquire(context.Background(), "p1", "linux", nil)
	if err != nil || id1 != "s-a" {
		t.Fatalf("首选应为 alpha: %s/%v", id1, err)
	}
	defer hold()
	// p2 无亲和历史:alpha 载 1、beta 载 0,应选更空闲的 beta(若走名字序会错选 alpha)。
	id2, _, rel2, err := s.Acquire(context.Background(), "p2", "linux", nil)
	if err != nil || id2 != "s-b" {
		t.Fatalf("最少负载兜底应选 beta: %s/%v", id2, err)
	}
	rel2()
}

// 不可达结果应被负缓存(30s 内不反复锤坏机):连续两次 Acquire 只探测一次。
func TestHealthProbeUnreachableCached(t *testing.T) {
	st := openMigrated(t)
	seedServer(t, st, "s-a", "alpha", "linux", 1, 0)
	p := &fakeProber{down: map[string]bool{"s-a": true}}
	s := NewScheduler(st.DB, p, 1)
	for i := 0; i < 2; i++ {
		if _, _, _, err := s.Acquire(context.Background(), "p1", "linux", nil); !errors.Is(err, ErrNoRunnerAvailable) {
			t.Fatalf("第 %d 次应 ErrNoRunnerAvailable: %v", i+1, err)
		}
	}
	if n := p.hits["s-a"]; n != 1 {
		t.Fatalf("不可达缓存 30s 内应只探测 1 次,实际 %d", n)
	}
}

// 全忙排队时显式 cancel(非超时)应可中断等待并返回 context.Canceled。
func TestAcquireExplicitCancel(t *testing.T) {
	st := openMigrated(t)
	seedServer(t, st, "s-a", "alpha", "linux", 1, 0)
	s := NewScheduler(st.DB, nil, 1)
	_, _, hold, err := s.Acquire(context.Background(), "p1", "linux", nil)
	if err != nil {
		t.Fatalf("首次 Acquire: %v", err)
	}
	defer hold()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, _, err := s.Acquire(ctx, "p1", "linux", nil)
		done <- err
	}()
	time.Sleep(30 * time.Millisecond) // 让等待者先入队
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("显式取消应返回 context.Canceled: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("显式取消未中断排队等待")
	}
}

// 回归:release 与等待者「抢槽失败 → 入队」竞态不得丢唤醒(有空槽却挂死)。
// 修复前 tryPick 与入队分两把锁,release 落在窗口内会丢信号;现同一临界区,循环压测验证。
func TestAcquireReleaseRacingNoMissedWakeup(t *testing.T) {
	st := openMigrated(t)
	seedServer(t, st, "s-a", "alpha", "linux", 1, 0)
	s := NewScheduler(st.DB, nil, 1)
	for i := 0; i < 100; i++ {
		_, _, hold, err := s.Acquire(context.Background(), "p1", "linux", nil)
		if err != nil {
			t.Fatalf("iter %d 首次 Acquire: %v", i, err)
		}
		done := make(chan error, 1)
		go func() {
			_, _, rel, err := s.Acquire(context.Background(), "p2", "linux", nil)
			if err == nil {
				rel()
			}
			done <- err
		}()
		hold() // 立即释放,与等待者入队竞态
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("iter %d 等待者出错: %v", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d: 已有空槽等待者却未被唤醒(missed wakeup)", i)
		}
	}
}

// 探测进行中 ctx 被取消(用户中止运行)造成的失败不得写入 30s 负缓存,
// 否则后续其他流水线会误判该机不可达。
func TestProbeAbortOnCancelNotCached(t *testing.T) {
	st := openMigrated(t)
	seedServer(t, st, "s-a", "alpha", "linux", 1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	s := NewScheduler(st.DB, probeFunc(func(context.Context, string) bool {
		cancel() // 模拟探测进行中运行被取消
		return false
	}), 1)
	if _, _, _, err := s.Acquire(ctx, "p1", "linux", nil); err == nil {
		t.Fatal("取消下不应拿到机器")
	}
	// 若取消造成的失败被负缓存,这次会误判 ErrNoRunnerAvailable;修复后应重新探测成功。
	s.prober = probeFunc(func(context.Context, string) bool { return true })
	if _, _, rel, err := s.Acquire(context.Background(), "p1", "linux", nil); err != nil {
		t.Fatalf("取消造成的探测失败不应写入负缓存: %v", err)
	} else {
		rel()
	}
}

// 有空槽时不得误报「构建机池全忙,排队等待」——排队日志只应在真正入队(pick 失败)后打。
// 回归:日志块曾被放在 pickOrEnqueue 之前,首次 Acquire 即使立即选中也会先打全忙日志。
func TestAcquireNoBusyLogWhenSlotFree(t *testing.T) {
	st := openMigrated(t)
	seedServer(t, st, "s-a", "alpha", "linux", 1, 0)
	seedServer(t, st, "s-b", "beta", "linux", 1, 0)
	s := NewScheduler(st.DB, nil, 1)

	// 日志收集加锁:等待者 goroutine 与断言方并发读写同一 slice(race detector 下必须同步)。
	var mu sync.Mutex
	var logged []string
	logf := func(msg string) { mu.Lock(); defer mu.Unlock(); logged = append(logged, msg) }
	sawBusyLog := func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, m := range logged {
			if strings.Contains(m, "全忙") {
				return true
			}
		}
		return false
	}

	// 两个空槽,两次 Acquire 都应立即可得,零排队日志。
	for i := 0; i < 2; i++ {
		_, _, rel, err := s.Acquire(context.Background(), "p1", "linux", logf)
		if err != nil {
			t.Fatalf("Acquire #%d: %v", i+1, err)
		}
		rel()
	}
	if sawBusyLog() {
		t.Fatal("有空槽时不应打全忙日志")
	}

	// 反向兜底:占满**全部**槽位后,排队路径的全忙日志确实会打(修位置没把日志修没)。
	var holds []func()
	for i := 0; i < 2; i++ {
		_, _, rel, err := s.Acquire(context.Background(), "p2", "linux", logf)
		if err != nil {
			t.Fatalf("占满 Acquire #%d: %v", i+1, err)
		}
		holds = append(holds, rel)
	}
	if sawBusyLog() {
		t.Fatal("占满阶段自身的两次 Acquire 有空槽,不应打全忙日志")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, rel, err := s.Acquire(context.Background(), "p3", "linux", logf)
		if err == nil {
			rel()
		}
	}()
	time.Sleep(50 * time.Millisecond) // 让等待者入队并打出排队日志
	busy := sawBusyLog()
	for _, rel := range holds { // 释放全部,唤醒等待者收尾
		rel()
	}
	<-done
	if !busy {
		t.Fatal("真正全忙时应打排队日志")
	}
}

func TestAcquireConcurrentNoOversubscribe(t *testing.T) {
	st := openMigrated(t)
	seedServer(t, st, "s-a", "alpha", "linux", 3, 0)
	s := NewScheduler(st.DB, nil, 1)
	var held int32
	var errs int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, rel, err := s.Acquire(context.Background(), "p", "linux", nil); err != nil {
				atomic.AddInt32(&errs, 1)
				return
			} else {
				cur := atomic.AddInt32(&held, 1)
				if cur > 3 {
					t.Errorf("超卖:同时持有 %d > 槽位 3", cur)
				}
				time.Sleep(5 * time.Millisecond)
				atomic.AddInt32(&held, -1)
				rel()
			}
		}()
	}
	wg.Wait()
	if errs > 0 {
		t.Fatalf("%d 个 Acquire 出错", errs)
	}
}

// Acquire 应返回机器的人读显示名 name(host):派发日志靠它明确「调度到哪台机」,而非甩 uuid。
func TestAcquireReturnsDisplayName(t *testing.T) {
	st := openMigrated(t)
	seedServer(t, st, "s-a", "alpha", "linux", 1, 0) // host = alpha.example
	s := NewScheduler(st.DB, nil, 1)
	id, name, rel, err := s.Acquire(context.Background(), "p1", "linux", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer rel()
	if id != "s-a" || name != "alpha(alpha.example)" {
		t.Fatalf("显示名应为 name(host):got id=%s name=%q", id, name)
	}
}

// name/host 缺失(历史/异常行)时显示名应回退 id,不得返回空串。
func TestAcquireDisplayNameFallsBackToID(t *testing.T) {
	st := openMigrated(t)
	seedServer(t, st, "s-a", "alpha", "linux", 1, 0)
	if _, err := st.DB.Exec(`UPDATE servers SET name='', host='' WHERE id='s-a'`); err != nil {
		t.Fatalf("清空 name/host: %v", err)
	}
	s := NewScheduler(st.DB, nil, 1)
	id, name, rel, err := s.Acquire(context.Background(), "p1", "server:s-a", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer rel()
	if id != "s-a" || name != "s-a" {
		t.Fatalf("name/host 缺失应回退 id:got id=%s name=%q", id, name)
	}
}

// —— 迁移回填:旧 runner_server_id 行应带上 server:<id> 选择器。——
// storetest 库是全新迁移完成的,测不到"升级时旧行已存在"的时序;此处按方言重放 0052 的
// 回填语句,验证其 SQL 在 sqlite/mysql 下都能正确转换(migrate() 按文件名序只跑一次,
// 真实升级场景旧行必然先于 0052 存在)。

func TestMigrationBackfillsSelector(t *testing.T) {
	st := openMigrated(t)
	projID := seedProject(t, st) // project_runners 有 FK → projects
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := st.DB.Exec(`INSERT INTO project_runners (project_id, runner_server_id, selector, created_at, updated_at) VALUES (?,'srv-old','',?,?)`, projID, now, now); err != nil {
		t.Fatalf("seed 旧行: %v", err)
	}
	stmt := `UPDATE project_runners SET selector = 'server:' || runner_server_id WHERE runner_server_id <> ''`
	if store.DialectOf(st.DB) == store.MySQL {
		stmt = `UPDATE project_runners SET selector = CONCAT('server:', runner_server_id) WHERE runner_server_id <> ''`
	}
	if _, err := st.DB.Exec(stmt); err != nil {
		t.Fatalf("回填: %v", err)
	}
	var sel string
	if err := st.DB.QueryRow(`SELECT selector FROM project_runners WHERE project_id=?`, projID).Scan(&sel); err != nil {
		t.Fatalf("查 selector: %v", err)
	}
	if sel != "server:srv-old" {
		t.Fatalf("回填 selector = %q, want server:srv-old", sel)
	}
}

// waitWaiters 轮询直到等待队列长度达到 n(确定性等待,替代 sleep 猜时序:
// 高负载下 goroutine 调度延迟可能超过固定 sleep,导致"以为已入队其实还没"的误判)。
func waitWaiters(t *testing.T, s *Scheduler, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		cur := len(s.waiters)
		s.mu.Unlock()
		if cur >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待者未入队:当前 %d,期望 %d", cur, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// 回归(唤醒丢失):队首 A 已收到 release 的唤醒信号但 ctx 同时超时、选择放弃时,
// 不得吞掉这笔唤醒——应转发给队列下一位 B,B 必须被唤醒拿到槽,而不是干等下一次 release。
// 修复前 A 走 removeWaiter 直接剔出,信号烂在 buffered channel 里,B 挂到超时。
func TestAcquireWaiterGiveUpForwardsWakeup(t *testing.T) {
	for i := 0; i < 100; i++ {
		st := openMigrated(t)
		seedServer(t, st, "s-a", "alpha", "linux", 1, 0)
		s := NewScheduler(st.DB, nil, 1)

		_, _, hold, err := s.Acquire(context.Background(), "p0", "linux", nil)
		if err != nil {
			t.Fatalf("iter %d 占槽: %v", i, err)
		}

		// A 先排队(ctx 可取消),B 排其后;确定性等 A 入队再启动 B,保证 A 是队首。
		ctxA, cancelA := context.WithCancel(context.Background())
		doneA := make(chan error, 1)
		go func() {
			_, _, rel, err := s.Acquire(ctxA, "pa", "linux", nil)
			if err == nil {
				rel() // A 若抢到也立即归还,让 B 继续
			}
			doneA <- err
		}()
		waitWaiters(t, s, 1)
		doneB := make(chan error, 1)
		go func() {
			_, _, rel, err := s.Acquire(context.Background(), "pb", "linux", nil)
			if err == nil {
				rel()
			}
			doneB <- err
		}()
		waitWaiters(t, s, 2)

		hold()    // 释放:唤醒信号同步投进队首 A 的 buffered channel
		cancelA() // 随即取消 A:其 select 两路就绪,走 ctx.Done 时即触发被测竞态

		select {
		case err := <-doneB:
			if err != nil {
				t.Fatalf("iter %d B 出错: %v", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d: A 放弃后唤醒被吞,B 未被转发唤醒(missed wakeup)", i)
		}
		select {
		case <-doneA: // A 必然退出:要么 ctx 取消返回,要么抢到并已归还
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d: A 未退出", i)
		}
	}
}

// 回归(FIFO 插队):槽空但等待队列非空时,新 Acquire 不得抢在既有排队者之前拿槽,
// 必须入队,让队首按 FIFO 拿走。修复前新请求直接抢空槽,排队者继续等。
func TestAcquireNoQueueJumping(t *testing.T) {
	for i := 0; i < 100; i++ {
		st := openMigrated(t)
		seedServer(t, st, "s-a", "alpha", "linux", 1, 0)
		s := NewScheduler(st.DB, nil, 1)

		_, _, hold, err := s.Acquire(context.Background(), "p0", "linux", nil)
		if err != nil {
			t.Fatalf("iter %d 占槽: %v", i, err)
		}

		// W1、W2 依次排队(谁前谁后不影响断言:X 不得先于队首拿槽)。
		// 用原子序号记录各自 Acquire 返回(=拿到槽)的先后,判定是否插队;
		// 等待者的错误不得静默(它会吞掉一笔唤醒,表现为后继超时/次序错乱)。
		var seq, w1Order, xOrder int32
		w1done := make(chan error, 1)
		go func() {
			_, _, rel, err := s.Acquire(context.Background(), "pw1", "linux", nil)
			if err == nil {
				atomic.StoreInt32(&w1Order, atomic.AddInt32(&seq, 1))
				rel()
			}
			w1done <- err
		}()
		w2done := make(chan error, 1)
		go func() {
			_, _, rel, err := s.Acquire(context.Background(), "pw2", "linux", nil)
			if err == nil {
				rel()
			}
			w2done <- err
		}()
		waitWaiters(t, s, 2) // 确定性等 W1、W2 都入队

		hold() // 释放:唤醒队首,槽空、队列还剩一人 —— 正是"槽空+队列非空"窗口

		// 主 goroutine 同步发起新 Acquire:修复后它应入队,直到 W1、W2 各拿一轮才返回;
		// 修复前它会立刻抢到刚空出的槽(插队)。
		_, _, relX, err := s.Acquire(context.Background(), "px", "linux", nil)
		if err != nil {
			t.Fatalf("iter %d 新 Acquire: %v", i, err)
		}
		atomic.StoreInt32(&xOrder, atomic.AddInt32(&seq, 1))
		// 修复后 X 拿槽必然晚于 W1:X 直接抢槽的前提是队列空且无 pending 主张,这要求
		// W1 已兑现其唤醒(retry 时注销)→ W1 的 Acquire 已返回并记录 w1Order;
		// X 走排队路径则更晚。故 w1Order 必已记录且小于 xOrder。
		if atomic.LoadInt32(&w1Order) == 0 || atomic.LoadInt32(&xOrder) < atomic.LoadInt32(&w1Order) {
			relX() // 先归还,放 W1/W2 走完,避免泄漏 goroutine 互等
			t.Fatalf("iter %d: 新 Acquire 插队抢在排队者之前拿到槽 (w1=%d, x=%d, w1err=%v)",
				i, w1Order, xOrder, <-w1done)
		}
		relX()
		select {
		case err := <-w1done:
			if err != nil {
				t.Fatalf("iter %d: W1 出错: %v", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d: W1 未拿到槽", i)
		}
		select {
		case err := <-w2done:
			if err != nil {
				t.Fatalf("iter %d: W2 出错: %v", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d: W2 未拿到槽", i)
		}
	}
}
