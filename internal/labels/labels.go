// Package labels — 机器标签登记处(标签字典)。
//
// 解决「悬置标签无处可建」:标签可先建后挂——先建 `gpu`、`arch=arm64`,机器后面打上
// 同名标签即入池(servers.labels 仍是机器实际标签的唯一事实来源,本表只是字典)。
// 消费方:
//   - 设置 → 服务器 的标签管理卡(建/删/看用量);
//   - 各选择器下拉的候选集 = 登记处 ∪ 机器实际标签(LabelSelectorEditor / StageDrawer /
//     EnvCredsTab / RunnerPanel 统一从 useLabels 取),悬置标签到处可选。
//
// 删除策略:仍被任一服务器引用 → ErrInUse(带台数),先从机器上移除再删;
// 悬置标签可直接删。命名校验复用构建机池同一字符集(runner.ValidateTerm),
// 机器打一次标签、登记处记一个名字,两边语法恒一致。
package labels

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/runner"
	"github.com/huangchengsir/pipewright/internal/target"
)

// Label 是登记处里的一条标签(字典行;不含机器关联,用量由消费方按 servers.labels 统计)。
type Label struct {
	Name      string `json:"name"`
	CreatedAt string `json:"createdAt"`
}

var (
	// ErrInvalidName 标签名非法(空白/字符集/超长;与选择器项同一规则)。
	ErrInvalidName = errors.New("labels: invalid name")
	// ErrExists 同名标签已登记。
	ErrExists = errors.New("labels: already exists")
	// ErrNotFound 标签不存在。
	ErrNotFound = errors.New("labels: not found")
)

// InUseError 表示标签仍被机器引用,不可删(Servers = 引用它的机器台数)。
type InUseError struct{ Servers int }

func (e *InUseError) Error() string {
	return fmt.Sprintf("labels: still used by %d server(s)", e.Servers)
}

// ServerLister 取全部服务器(删标签时判引用;用 target.Service 注入,便于测试 stub)。
type ServerLister interface {
	List(ctx context.Context) ([]*target.Server, error)
}

type service struct {
	db      *sql.DB
	servers ServerLister
}

// Service 是标签登记处接口。
type Service interface {
	// List 返回全部登记标签(按名字排序;悬置标签也在列)。
	List(ctx context.Context) ([]Label, error)
	// Create 登记一个新标签(同名已存在 → ErrExists;名字非法 → ErrInvalidName)。
	Create(ctx context.Context, name string) (*Label, error)
	// Delete 删除登记标签(不存在 → ErrNotFound;仍被机器引用 → *InUseError)。
	Delete(ctx context.Context, name string) error
}

// New 构造标签登记处服务。servers 用于删除时的引用检查(可为 nil:跳过检查,仅测试用)。
func New(db *sql.DB, servers ServerLister) Service {
	return &service{db: db, servers: servers}
}

func (s *service) List(ctx context.Context) ([]Label, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, created_at FROM labels ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Label, 0, 16)
	for rows.Next() {
		var l Label
		if err := rows.Scan(&l.Name, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *service) Create(ctx context.Context, name string) (*Label, error) {
	name = trimTerm(name)
	if err := runner.ValidateTerm(name); err != nil {
		return nil, ErrInvalidName
	}
	var exists string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM labels WHERE name = ?`, name).Scan(&exists)
	switch {
	case err == nil:
		return nil, ErrExists
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO labels (name, created_at) VALUES (?, ?)`, name, now); err != nil {
		return nil, err
	}
	return &Label{Name: name, CreatedAt: now}, nil
}

func (s *service) Delete(ctx context.Context, name string) error {
	name = trimTerm(name)
	if name == "" {
		return ErrNotFound
	}
	// 引用检查:任一机器的 labels 含该项(精确项相等,与匹配语义一致)→ 拒删。
	if s.servers != nil {
		servers, err := s.servers.List(ctx)
		if err != nil {
			return err
		}
		used := 0
		for _, srv := range servers {
			if srv == nil {
				continue
			}
			if _, ok := runner.ParseLabels(srv.Labels)[name]; ok {
				used++
			}
		}
		if used > 0 {
			return &InUseError{Servers: used}
		}
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM labels WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// trimTerm 去首尾空白(与选择器解析同规;内部小工具,不导出)。
func trimTerm(s string) string {
	return strings.TrimSpace(s)
}
