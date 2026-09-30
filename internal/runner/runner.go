// Package runner 是「构建机池配置 + 调度」领域层(FR-8-14 远程 runner 续 / FR-8-19 池化)。
//
// 每项目可配一个**标签选择器**(selector):空 = 本地构建;`server:<id>` = 钉死一台已登记服务器
// (旧 runner_server_id 的规范形式);`linux,arch=arm64` = 在打了标签的服务器池里按
// 「优先级 → 流水线亲和 → 负载」调度(scheduler.go)。stage 可覆盖(stage.Runner)。
// 本包管配置存取(Service)与选机调度(Scheduler);真实远程执行在 build 包的远程阶段执行器。
package runner

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/store"
)

// 领域错误。
var (
	// ErrProjectNotFound 表示项目不存在。
	ErrProjectNotFound = errors.New("runner: project not found")
	// ErrServerNotFound 表示指定的 runner 服务器不存在。
	ErrServerNotFound = errors.New("runner: runner server not found")
)

// Config 是某项目的构建机配置(Selector 空 = 本地构建)。
// RunnerServerID 为钉死形式(server:<id>)时的派生只读字段,便于旧 UI/DTO 兼容展示。
type Config struct {
	ProjectID      string
	Selector       string
	RunnerServerID string // 仅当 selector 为 server:<id> 时非空
}

// ServerExister 抽象「校验服务器是否存在」(target.Service 即满足;避免 runner 直依赖 target)。
type ServerExister interface {
	Exists(ctx context.Context, serverID string) bool
}

// Service 是项目 runner 配置读写接口。
type Service interface {
	// Get 取某项目的 runner 配置(无行 → Selector 空,即本地构建)。
	Get(ctx context.Context, projectID string) (*Config, error)
	// SaveSelector 设/清某项目的选择器(空 = 清,回本地构建)。校验项目存在、选择器语法,
	// server:<id> 形式还校验机器存在。
	SaveSelector(ctx context.Context, projectID, selector string) (*Config, error)
	// Save 是旧入口的兼容壳:按 runner 服务器 id 存(等价 server:<id>),空串 = 清。
	Save(ctx context.Context, projectID, runnerServerID string) (*Config, error)
	// SelectorFor 取某项目的选择器(无 → "", false),供构建派发快速判定。
	SelectorFor(ctx context.Context, projectID string) (string, bool)
	// RunnerFor 取钉死形式的机器 id(标签选择器/本地 → "", false)。池化前的旧判定入口,保留兼容。
	RunnerFor(ctx context.Context, projectID string) (string, bool)
}

type service struct {
	db      *sql.DB
	servers ServerExister
}

// New 构造 runner 配置服务。servers 用于 Save 时校验 runner 服务器存在(可为 nil:跳过校验)。
func New(db *sql.DB, servers ServerExister) Service {
	return &service{db: db, servers: servers}
}

func (s *service) Get(ctx context.Context, projectID string) (*Config, error) {
	cfg := &Config{ProjectID: projectID}
	var legacy string
	row := s.db.QueryRowContext(ctx, `SELECT COALESCE(selector,''), COALESCE(runner_server_id,'') FROM project_runners WHERE project_id = ?`, projectID)
	switch err := row.Scan(&cfg.Selector, &legacy); {
	case errors.Is(err, sql.ErrNoRows):
		return cfg, nil // 无配置 = 本地构建
	case err != nil:
		return nil, err
	}
	// 迁移 0052 已回填;此兜底覆盖"回填后又被旧版本写过"的边角(旧版本只写 runner_server_id)。
	if cfg.Selector == "" && legacy != "" {
		cfg.Selector = "server:" + legacy
	}
	cfg.RunnerServerID, _ = PinnedServer(cfg.Selector)
	return cfg, nil
}

func (s *service) SaveSelector(ctx context.Context, projectID, selector string) (*Config, error) {
	selector = strings.TrimSpace(selector)

	// 校验项目存在。
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id = ?`, projectID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrProjectNotFound
		}
		return nil, err
	}
	// 语法 + 钉死形式的机器存在性(防配了不存在的机)。
	if err := ValidateSelector(selector); err != nil {
		return nil, err
	}
	if id, ok := PinnedServer(selector); ok && s.servers != nil && !s.servers.Exists(ctx, id) {
		return nil, ErrServerNotFound
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO project_runners (project_id, runner_server_id, selector, created_at, updated_at)
		VALUES (?, '', ?, ?, ?) `+
		store.UpsertSuffix(store.DialectOf(s.db), []string{"project_id"}, []string{"selector", "updated_at"}),
		projectID, selector, now, now)
	if err != nil {
		return nil, err
	}
	return &Config{ProjectID: projectID, Selector: selector}, nil
}

func (s *service) Save(ctx context.Context, projectID, runnerServerID string) (*Config, error) {
	selector := strings.TrimSpace(runnerServerID)
	if selector != "" {
		selector = "server:" + selector
	}
	return s.SaveSelector(ctx, projectID, selector)
}

func (s *service) SelectorFor(ctx context.Context, projectID string) (string, bool) {
	cfg, err := s.Get(ctx, projectID)
	if err != nil || cfg.Selector == "" {
		return "", false
	}
	return cfg.Selector, true
}

func (s *service) RunnerFor(ctx context.Context, projectID string) (string, bool) {
	cfg, err := s.Get(ctx, projectID)
	if err != nil || cfg.RunnerServerID == "" {
		return "", false
	}
	return cfg.RunnerServerID, true
}
