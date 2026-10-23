package opschat

import (
	"bytes"
	"context"
	"database/sql"
	"runtime"
	"strconv"
	"sync"
)

// ─── 事务守卫:把"事务作用域内的嵌套池 IO"从生产死锁变成开发期爆炸 ───
//
// 背景:SQLite 部署下全进程共享一条连接(MaxOpenConns=1)。只读/写事务持有
// 这条连接期间,同一 goroutine 再向连接池要连接(s.db.*、或经注入闭包间接
// 查库)就是自死锁——线上曾因 policy()→aiContextPolicy→ai.load 整个后端
// 冻结(见 TestSnapshotSurvivesPoolReadingContextSource 与 store.go 的
// transaction 注释)。死锁发生在 database/sql 的池等待里,连驱动层都感知
// 不到,唯一可靠的拦截点是 *sql.DB 方法入口——因此本包把运行期池句柄换成
// 下面的窄接口 guardedPool:事务窗口内同 goroutine 的任何池调用立即 panic
// (httpapi 有 Recoverer,表现为 500+栈日志,远好于无声冻结)。
//
// 已知边界:守卫只看得见本包句柄上的 IO。经其它服务自有句柄(ai/vault/
// projects)的间接查库不可见——那类缝由 policy()/registerSecrets() 入口的
// 显式 check 与既有回归测试兜底;新增注入闭包时请同样在入口加 check。
//
// MySQL 部署(多连接)嵌套 IO 不致命,守卫自动关闭(active=false)。

// poolDB 是 opschat 运行期访问连接池的最小面(与 querier 对齐)。刻意不含
// Exec/BeginTx:写路径一律走 transaction() 的 *sql.Tx;这里出现 Exec 调用
// 无法编译,倒逼调用者显式选择事务内还是事务外。
type poolDB interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// txGuard 按 goroutine 记录"事务持有中"标记;enter/exit 支持嵌套计数。
// active=false(MySQL)时所有 check 均为 no-op。
type txGuard struct {
	active bool
	mu     sync.Mutex
	held   map[int64]int
}

func (g *txGuard) enter() {
	if !g.active {
		return
	}
	id := goid()
	g.mu.Lock()
	if g.held == nil {
		g.held = map[int64]int{}
	}
	g.held[id]++
	g.mu.Unlock()
}

func (g *txGuard) exit() {
	if !g.active {
		return
	}
	id := goid()
	g.mu.Lock()
	if n := g.held[id]; n <= 1 {
		delete(g.held, id)
	} else {
		g.held[id] = n - 1
	}
	g.mu.Unlock()
}

// check 在 opschat 自有池句柄或已知会外呼的入口调用:当前 goroutine 若正
// 持有事务,立即 panic 并给出修复指引。
func (g *txGuard) check(op string) {
	if !g.active {
		return
	}
	id := goid()
	g.mu.Lock()
	n := g.held[id]
	g.mu.Unlock()
	if n > 0 {
		panic("opschat: nested pool IO (" + op + ") inside transaction on the same goroutine" +
			" — with a single SQLite connection this is a self-deadlock that freezes the whole backend." +
			" Use the *sql.Tx you already hold, or fetch external state (policy / ai config / vault / projects)" +
			" BEFORE entering the transaction. See internal/opschat/txguard.go.")
	}
}

// guardedPool 包装真实池:同 goroutine 事务窗口内的查询直接 panic。
type guardedPool struct {
	inner *sql.DB
	g     *txGuard
}

func (p guardedPool) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	p.g.check("QueryContext")
	return p.inner.QueryContext(ctx, query, args...)
}

func (p guardedPool) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	p.g.check("QueryRowContext")
	return p.inner.QueryRowContext(ctx, query, args...)
}

// goid 取当前 goroutine ID(解析 runtime.Stack 头部 "goroutine N [running]:"
// 的 N)。仅在守卫路径调用,百纳秒级,可忽略;go 不暴露官方 API,这是标准做法。
func goid() int64 {
	var b [64]byte
	n := runtime.Stack(b[:], false)
	s := b[:n]
	i := bytes.IndexByte(s, ' ')
	if i < 0 {
		return 0
	}
	rest := s[i+1:]
	if j := bytes.IndexByte(rest, ' '); j > 0 {
		id, _ := strconv.ParseInt(string(rest[:j]), 10, 64)
		return id
	}
	return 0
}
