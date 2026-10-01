// Package servercmd 是「批量执行命令」的领域层(服务器状态页 → 勾选多机 → 同步执行 + 历史回看)。
//
// 在 target.Service(通用 SSH 执行基座)之上组合出跨机能力:
//   - Run:对一批已登记服务器并发执行同一条 shell 命令(信号量限并发、每机独立超时与容错,
//     一台失败不连累其它台),并把头部 + 逐机结果落库(保留最近 HistoryKeep 次)。
//   - ListRuns / GetRun:历史回看(不含 / 含逐机输出)。
//
// 任意 shell 命令串是本功能的本质:平台为单管理员、既有 WS 终端(单机)本就能执行任意命令,
// 批量入口不提升权限面。护栏:命令长度/机器数/单机超时/输出截断上限;每次执行由 HTTP 层写
// append-only 审计。执行经 `sh -c <命令>` —— 与 target.Upload 的 `sh -c <固定脚本>` 同一先例。
//
// 降级铁律:历史落库失败只记日志、绝不影响本次执行结果(命令已在机器上真实跑过)。
package servercmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/target"
)

// 上限常量(领域层兜底钳制;HTTP 层同样校验,见 server_commands.go)。
const (
	// Concurrency 是逐机执行的并发上限(信号量),与 containers/metrics 批量端点一致。
	Concurrency = 6
	// DefaultTimeoutSec / MinTimeoutSec / MaxTimeoutSec 是单机执行超时(秒);0 → 默认,越界钳到边界。
	DefaultTimeoutSec = 60
	MinTimeoutSec     = 5
	MaxTimeoutSec     = 300
	// MaxServers 是单次批量机器数上限(去重后)。
	MaxServers = 100
	// MaxCommandLen 是命令长度上限(字节)。
	MaxCommandLen = 8 << 10
	// OutMax 是单流(stdout/stderr)截断上限(字节)。
	OutMax = 64 << 10
	// HistoryKeep 是历史保留条数;每次插入后裁掉更早的(防输出 blob 无限膨胀)。
	HistoryKeep = 200

	historyDefaultLimit = 50
	historyMaxLimit     = 200
)

// 领域错误(错误体绝无凭据明文/内部栈)。
var (
	// ErrInvalidInput 输入非法(命令空/过长、机器数为 0/超上限)。HTTP 层映射 400。
	ErrInvalidInput = errors.New("servercmd: invalid input")
	// ErrNotFound 历史记录不存在。HTTP 层映射 404。
	ErrNotFound = errors.New("servercmd: run not found")
	// ErrUninitialized 服务未装配(db / targets 缺失)。HTTP 层映射 503。
	ErrUninitialized = errors.New("servercmd: service uninitialized")
)

// RunInput 是一次批量执行的入参。
type RunInput struct {
	Command    string   // shell 命令串(经 sh -c 执行)
	ServerIDs  []string // 目标机 id(重复项去重)
	TimeoutSec int      // 单机超时(秒);0 = 默认,越界钳到 [Min, Max]
}

// ResultItem 是单机结果。OK=false 时 Error 为人读串(连接失败/非零退出码等)。
type ResultItem struct {
	ServerID   string
	ServerName string
	OK         bool
	ExitCode   int
	Stdout     string
	Stderr     string
	Error      string
	DurationMs int64
}

// RunResult 是一次批量执行的结果(同步返回;持久化失败不影响其完整性)。
type RunResult struct {
	RunID  string
	Items  []ResultItem
	Total  int
	OK     int
	Failed int
}

// RunSummary 是历史列表条目(不含逐机输出)。
type RunSummary struct {
	ID        string
	Command   string
	Total     int
	OK        int
	Failed    int
	CreatedAt time.Time
}

// RunDetail 是历史详情(头部 + 逐机结果)。
type RunDetail struct {
	RunSummary
	Items []ResultItem
}

// Service 组合 target.Service(SSH 执行)与本地持久化(server_command_runs/results)。
type Service struct {
	db      *sql.DB
	targets target.Service
}

// New 构造 Service。db / targets 由装配方保证非 nil(缺失时 Run 返回 ErrUninitialized)。
func New(db *sql.DB, targets target.Service) *Service {
	return &Service{db: db, targets: targets}
}

// Run 对一批服务器并发执行同一命令,返回逐机结果并落历史。
//
// 语义:每台独立容错 —— 未知 serverId / SSH 连接失败 / 非零退出码都只标记该机 OK=false,
// 绝不让整体 5xx;targets.List 失败(库不可用)才是整体错误。落库 best-effort:失败仅记日志。
func (s *Service) Run(ctx context.Context, in RunInput) (*RunResult, error) {
	if s == nil || s.db == nil || s.targets == nil {
		return nil, ErrUninitialized
	}
	command := strings.TrimSpace(in.Command)
	if command == "" || len(command) > MaxCommandLen {
		return nil, ErrInvalidInput
	}
	ids := dedupeIDs(in.ServerIDs)
	if len(ids) == 0 || len(ids) > MaxServers {
		return nil, ErrInvalidInput
	}
	timeoutSec := ClampTimeoutSec(in.TimeoutSec)

	// 一次 List 解析 id → 服务器(拿展示名);未知 id → 该机 per-item 错误,不整体失败。
	servers, err := s.targets.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("servercmd: list servers: %w", err)
	}
	byID := make(map[string]*target.Server, len(servers))
	for _, srv := range servers {
		byID[srv.ID] = srv
	}

	// 并发模式与 server_containers.go 批量端点一致:信号量限并发 + 预分配切片按索引写(无锁)。
	items := make([]ResultItem, len(ids))
	sem := make(chan struct{}, Concurrency)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			items[i] = s.execOne(ctx, id, byID[id], command, timeoutSec)
		}(i, id)
	}
	wg.Wait()

	res := &RunResult{RunID: uuid.NewString(), Items: items, Total: len(items)}
	for _, it := range items {
		if it.OK {
			res.OK++
		} else {
			res.Failed++
		}
	}

	if err := s.persist(ctx, res, command); err != nil {
		// 命令已在机器上真实跑过:落库失败绝不影响返回,仅记日志(历史里缺这一条)。
		log.Printf("[servercmd] 警告:批量命令历史落库失败(runId=%s):%v", res.RunID, err)
	}
	return res, nil
}

// execOne 对单机执行命令并组装结果(永不返回 error:一切失败都折叠为该机的 OK=false + 人读 Error)。
func (s *Service) execOne(ctx context.Context, id string, srv *target.Server, command string, timeoutSec int) ResultItem {
	item := ResultItem{ServerID: id}
	if srv == nil {
		item.Error = "服务器不存在或已删除"
		return item
	}
	item.ServerName = srv.Name

	start := time.Now()
	execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	res, err := s.targets.Exec(execCtx, id, []string{"sh", "-c", command})
	item.DurationMs = time.Since(start).Milliseconds()

	if err != nil {
		// 单机批量语境:凭据缺失/保险库未配也是「这一台」的问题,折叠为该机错误
		// (与单机 serviceAction 的 422/503 语义不同,批量不连累其它台)。
		item.ExitCode = -1
		item.Error = humanError(err)
		return item
	}
	item.ExitCode = res.ExitCode
	item.Stdout = truncateOut(res.Stdout)
	item.Stderr = truncateOut(res.Stderr)
	item.OK = res.ExitCode == 0
	return item
}

// ListRuns 返回历史列表(新→旧;limit <=0 用默认,越界钳到上限;不含逐机输出)。
func (s *Service) ListRuns(ctx context.Context, limit int) ([]RunSummary, error) {
	if s == nil || s.db == nil {
		return nil, ErrUninitialized
	}
	if limit <= 0 {
		limit = historyDefaultLimit
	}
	if limit > historyMaxLimit {
		limit = historyMaxLimit
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, command, total, ok_count, fail_count, created_at
		 FROM server_command_runs ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("servercmd: list runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]RunSummary, 0, limit)
	for rows.Next() {
		var sum RunSummary
		var createdStr string
		if err := rows.Scan(&sum.ID, &sum.Command, &sum.Total, &sum.OK, &sum.Failed, &createdStr); err != nil {
			return nil, fmt.Errorf("servercmd: scan run: %w", err)
		}
		sum.CreatedAt = parseTime(createdStr)
		out = append(out, sum)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("servercmd: iterate runs: %w", err)
	}
	return out, nil
}

// GetRun 返回一次批量执行的头部 + 逐机结果(按落库顺序)。不存在 → ErrNotFound。
func (s *Service) GetRun(ctx context.Context, id string) (*RunDetail, error) {
	if s == nil || s.db == nil {
		return nil, ErrUninitialized
	}
	var det RunDetail
	var createdStr string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, command, total, ok_count, fail_count, created_at FROM server_command_runs WHERE id = ?`, id,
	).Scan(&det.ID, &det.Command, &det.Total, &det.OK, &det.Failed, &createdStr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("servercmd: get run: %w", err)
	}
	det.CreatedAt = parseTime(createdStr)

	rows, err := s.db.QueryContext(ctx,
		`SELECT server_id, server_name, ok, exit_code, stdout, stderr, error, duration_ms
		 FROM server_command_results WHERE run_id = ? ORDER BY id ASC`, id)
	if err != nil {
		return nil, fmt.Errorf("servercmd: get run items: %w", err)
	}
	defer func() { _ = rows.Close() }()

	det.Items = make([]ResultItem, 0, det.Total)
	for rows.Next() {
		var it ResultItem
		if err := rows.Scan(&it.ServerID, &it.ServerName, &it.OK, &it.ExitCode, &it.Stdout, &it.Stderr, &it.Error, &it.DurationMs); err != nil {
			return nil, fmt.Errorf("servercmd: scan run item: %w", err)
		}
		det.Items = append(det.Items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("servercmd: iterate run items: %w", err)
	}
	return &det, nil
}

// persist 在一个事务里写 runs + results 并裁剪超出保留条数的旧记录。
func (s *Service) persist(ctx context.Context, res *RunResult, command string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	createdStr := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO server_command_runs (id, command, total, ok_count, fail_count, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		res.RunID, command, res.Total, res.OK, res.Failed, createdStr,
	); err != nil {
		return fmt.Errorf("insert run: %w", err)
	}
	for _, it := range res.Items {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO server_command_results
			 (run_id, server_id, server_name, ok, exit_code, stdout, stderr, error, duration_ms)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			res.RunID, it.ServerID, it.ServerName, it.OK, it.ExitCode, it.Stdout, it.Stderr, it.Error, it.DurationMs,
		); err != nil {
			return fmt.Errorf("insert result: %w", err)
		}
	}
	if err := pruneHistory(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// pruneHistory 删掉保留窗口之外的历史:先定位第 HistoryKeep+1 新的边界行
// (created_at, id),再删该边界行及其更旧的 runs 与 results(比较含等号,边界行本身
// 也在删除窗口内)。显式先删 results 再删 runs(不依赖 FK 级联,sqlite 驱动默认不开
// foreign_keys);边界法避开 MySQL 不支持的 `LIMIT -1` 与 IN 子查询带 LIMIT 两种方言差异。
func pruneHistory(ctx context.Context, tx *sql.Tx) error {
	var bts, bid string
	err := tx.QueryRowContext(ctx,
		`SELECT created_at, id FROM server_command_runs ORDER BY created_at DESC, id DESC LIMIT 1 OFFSET ?`,
		HistoryKeep,
	).Scan(&bts, &bid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // 未超保留窗口,无需裁剪
	}
	if err != nil {
		return fmt.Errorf("locate prune boundary: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM server_command_results WHERE run_id IN (
		     SELECT id FROM server_command_runs
		     WHERE created_at < ? OR (created_at = ? AND id <= ?)
		 )`, bts, bts, bid,
	); err != nil {
		return fmt.Errorf("prune results: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM server_command_runs
		 WHERE created_at < ? OR (created_at = ? AND id <= ?)`, bts, bts, bid,
	); err != nil {
		return fmt.Errorf("prune runs: %w", err)
	}
	return nil
}

// ClampTimeoutSec 把超时入参钳到合法区间;0 → 默认。
func ClampTimeoutSec(sec int) int {
	switch {
	case sec == 0:
		return DefaultTimeoutSec
	case sec < MinTimeoutSec:
		return MinTimeoutSec
	case sec > MaxTimeoutSec:
		return MaxTimeoutSec
	default:
		return sec
	}
}

// dedupeIDs 去重(保序)并丢弃空串。
func dedupeIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// truncateOut 按 OutMax 截断输出,越界时追加省略标记。
func truncateOut(s string) string {
	if len(s) <= OutMax {
		return s
	}
	return s[:OutMax] + "\n…(output truncated)"
}

// humanError 把领域/SSH 错误映射为人读文案(与 server_ops.humanServiceError 同义;
// 绝不含凭据明文/内部栈)。
func humanError(err error) string {
	switch {
	case errors.Is(err, target.ErrAuth):
		return "SSH 认证失败:密钥或口令无效,或无登录权限"
	case errors.Is(err, target.ErrUnreachable):
		return "无法连接服务器:端口未开放、主机不可达或超时"
	case errors.Is(err, target.ErrInvalidCredential):
		return "凭据不是可用的 SSH 私钥或口令"
	case errors.Is(err, target.ErrCredentialNotFound):
		return "服务器引用的 SSH 凭据不存在"
	case errors.Is(err, target.ErrVaultUnconfigured):
		return "保险库未配置 master key,无法取 SSH 凭据"
	case errors.Is(err, context.DeadlineExceeded):
		return "执行超时:命令超出单机超时上限被中止"
	default:
		return "执行失败:连接或命令执行错误"
	}
}

// parseTime 解析 RFC3339(Nano)落库串;坏数据回退零值(与 audit 包同容错策略)。
func parseTime(s string) time.Time {
	if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return ts
	}
	if ts, err := time.Parse(time.RFC3339, s); err == nil {
		return ts
	}
	return time.Time{}
}
