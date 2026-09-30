package appstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/huangchengsir/pipewright/internal/store"
)

// Store 持久化应用模板(参数化 SQL,sqlite/mysql 两方言一致)。
type Store struct {
	db *sql.DB
}

// NewStore 构造持久层。
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

const tplCols = `SELECT id, name, display_name, description, icon, compose_yaml, params_json, builtin, created_at, updated_at
FROM app_templates`

// insert 落库模板;name 唯一冲突 → ErrNameTaken。
func (s *Store) insert(ctx context.Context, t *Template) error {
	paramsJSON, err := marshalParams(t.Params)
	if err != nil {
		return err
	}
	builtin := 0
	if t.Builtin {
		builtin = 1
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO app_templates
		   (id, name, display_name, description, icon, compose_yaml, params_json, builtin, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, t.DisplayName, t.Description, t.Icon, t.ComposeYAML, paramsJSON, builtin,
		t.CreatedAt.UTC().Format(time.RFC3339), t.UpdatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		if store.IsUniqueErr(err) {
			return ErrNameTaken
		}
		return fmt.Errorf("appstore: insert template: %w", err)
	}
	return nil
}

// get 读取单个模板;不存在 → ErrNotFound。
func (s *Store) get(ctx context.Context, id string) (*Template, error) {
	row := s.db.QueryRowContext(ctx, tplCols+` WHERE id = ?`, id)
	t, err := scanTemplate(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return t, nil
}

// getByName 按名字读取模板(内置 seed 幂等用);不存在 → ErrNotFound。
func (s *Store) getByName(ctx context.Context, name string) (*Template, error) {
	row := s.db.QueryRowContext(ctx, tplCols+` WHERE name = ?`, name)
	t, err := scanTemplate(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return t, nil
}

// list 返回全部模板(内置在前,组内按名字升序)。
func (s *Store) list(ctx context.Context) ([]Template, error) {
	rows, err := s.db.QueryContext(ctx, tplCols+` ORDER BY builtin DESC, name ASC`)
	if err != nil {
		return nil, fmt.Errorf("appstore: list templates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]Template, 0)
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// update 更新模板列(调用方已校验)。
func (s *Store) update(ctx context.Context, t *Template) error {
	paramsJSON, err := marshalParams(t.Params)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE app_templates
		 SET display_name = ?, description = ?, icon = ?, compose_yaml = ?, params_json = ?, updated_at = ?
		 WHERE id = ?`,
		t.DisplayName, t.Description, t.Icon, t.ComposeYAML, paramsJSON,
		t.UpdatedAt.UTC().Format(time.RFC3339), t.ID,
	)
	if err != nil {
		return fmt.Errorf("appstore: update template: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// delete 删除模板;不存在 → ErrNotFound。
func (s *Store) delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM app_templates WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("appstore: delete template: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// marshalParams 序列化参数 schema;nil → [](前端拿到 [] 而非 null)。
func marshalParams(ps []ParamSpec) (string, error) {
	if ps == nil {
		ps = []ParamSpec{}
	}
	b, err := json.Marshal(ps)
	if err != nil {
		return "", fmt.Errorf("appstore: marshal params: %w", err)
	}
	return string(b), nil
}

// scanner 抽象 *sql.Row 与 *sql.Rows 的 Scan。
type scanner interface {
	Scan(dest ...any) error
}

func scanTemplate(sc scanner) (*Template, error) {
	var (
		t         Template
		builtin   int
		paramsRaw string
		createdS  string
		updatedS  string
	)
	if err := sc.Scan(
		&t.ID, &t.Name, &t.DisplayName, &t.Description, &t.Icon, &t.ComposeYAML,
		&paramsRaw, &builtin, &createdS, &updatedS,
	); err != nil {
		return nil, err
	}
	t.Builtin = builtin != 0
	if paramsRaw != "" {
		if err := json.Unmarshal([]byte(paramsRaw), &t.Params); err != nil {
			return nil, fmt.Errorf("appstore: unmarshal params: %w", err)
		}
	}
	if t.Params == nil {
		t.Params = []ParamSpec{}
	}
	t.CreatedAt = parseTime(createdS)
	t.UpdatedAt = parseTime(updatedS)
	return &t, nil
}

func parseTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}
