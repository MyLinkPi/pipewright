// Package appstore 是「应用商店模板」的领域层(DPanel 式一键部署)。
//
// 模板 = docker-compose YAML + 参数 schema:预置内置模板(MySQL/Redis/Postgres/Nginx/MinIO/
// RabbitMQ,启动幂等 seed,builtin=1 不可改删),用户可建自定义模板。部署本身不在本包 ——
// httpapi 侧复用 Stacks 受管链路(Upload 到 /opt/pipewright/stacks/<name> → docker compose up -d),
// 本包只负责模板 CRUD 与「参数校验 + 占位符替换 + secret 自动生成」的纯渲染。
//
// 设计纪律:
//   - 参数值白名单校验(可打印 ASCII、无换行)防 YAML 结构注入;int 参数数值校验。
//   - secret 参数空缺时 crypto/rand 自动生成,生成值仅部署响应一次性返回(部署后存在于目标机
//     compose 文件中,与 Stacks 语义一致)。
//   - 内置模板不可改删(升级由新版内置 seed 幂等覆盖);自定义模板完整 CRUD。
package appstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// 参数类型枚举。
const (
	ParamTypeString = "string"
	ParamTypeInt    = "int"
	ParamTypeSecret = "secret"
)

// 领域错误(httpapi 层映射状态码)。
var (
	// ErrNotFound 表示模板不存在。
	ErrNotFound = errors.New("appstore: template not found")
	// ErrNameTaken 表示模板名已被占用(name 唯一,兼作 compose 项目名)。
	ErrNameTaken = errors.New("appstore: template name already in use")
	// ErrBuiltin 表示对内置模板执行了不允许的修改/删除。
	ErrBuiltin = errors.New("appstore: builtin template cannot be modified or deleted")
	// ErrInvalidName 表示模板名非法。
	ErrInvalidName = errors.New("appstore: invalid template name")
	// ErrInvalidParam 表示参数 schema 非法(名称/类型/默认值)。
	ErrInvalidParam = errors.New("appstore: invalid param spec")
	// ErrInvalidCompose 表示 compose YAML 为空/过大。
	ErrInvalidCompose = errors.New("appstore: invalid compose yaml")
	// ErrMissingParam 表示必填参数缺失且无默认值/自动生成。
	ErrMissingParam = errors.New("appstore: missing required param")
	// ErrUnknownParam 表示提交了 schema 之外的参数(防拼写错静默落空)。
	ErrUnknownParam = errors.New("appstore: unknown param")
	// ErrInvalidValue 表示参数值非法(类型/字符集/长度)。
	ErrInvalidValue = errors.New("appstore: invalid param value")
)

// composeMaxBytes 与 Stacks 部署上限一致。
const composeMaxBytes = 512 << 10

// 校验白名单。
var (
	// nameRe:模板名(兼 compose 项目名):小写字母数字 + -,不以 - 开头。
	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	// paramNameRe:参数名(占位符 {{name}} 内合法标识符)。
	paramNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,63}$`)
	// valueRe:参数值:可打印 ASCII、无换行/引号/反斜杠(防 YAML/Shell 结构注入)。
	valueRe = regexp.MustCompile(`^[\x20-\x21\x23-\x5B\x5D-\x7E]{1,256}$`)
)

// ParamSpec 是一个模板参数的定义(前端据此渲染表单)。
type ParamSpec struct {
	Name         string `json:"name"`
	Label        string `json:"label,omitempty"`
	Type         string `json:"type"` // string | int | secret
	Default      string `json:"default,omitempty"`
	Required     bool   `json:"required"`
	AutoGenerate bool   `json:"autoGenerate,omitempty"` // 仅 secret:空缺时自动生成
}

// Template 是一个应用模板。
type Template struct {
	ID          string
	Name        string // slug,唯一;部署时兼作 compose 项目名
	DisplayName string
	Description string
	Icon        string // emoji
	ComposeYAML string
	Params      []ParamSpec
	Builtin     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CreateInput 是创建自定义模板的入参。
type CreateInput struct {
	Name        string
	DisplayName string
	Description string
	Icon        string
	ComposeYAML string
	Params      []ParamSpec
}

// UpdateInput 是更新自定义模板的入参(内置模板拒绝)。
type UpdateInput struct {
	DisplayName *string
	Description *string
	Icon        *string
	ComposeYAML *string
	Params      []ParamSpec // nil = 保持
}

// RenderResult 是一次参数渲染的结果:Compose 为最终 YAML;Used 含每个参数的生效值
// (含自动生成的 secret,仅本次部署响应一次性展示)。
type RenderResult struct {
	Compose string
	Used    map[string]string
}

// Service 定义应用商店对外接口(httpapi 消费)。
type Service interface {
	// List 返回全部模板(内置在前,按名字升序)。
	List(ctx context.Context) ([]Template, error)
	// Get 返回单个模板;不存在 → ErrNotFound。
	Get(ctx context.Context, id string) (*Template, error)
	// Create 校验并落库自定义模板。
	Create(ctx context.Context, in CreateInput) (*Template, error)
	// Update 更新自定义模板;内置模板 → ErrBuiltin。
	Update(ctx context.Context, id string, in UpdateInput) (*Template, error)
	// Delete 删除自定义模板;内置模板 → ErrBuiltin。
	Delete(ctx context.Context, id string) error
	// EnsureBuiltins 幂等 seed 内置模板(main 启动时调用一次;已存在同名内置行则跳过)。
	EnsureBuiltins(ctx context.Context) error
	// Render 校验参数 → 应用默认值/自动生成 secret → 替换 {{param}} 占位符。
	Render(ctx context.Context, id string, params map[string]string) (*RenderResult, error)
}

// service 是 store 支撑的 Service 实现。
type service struct {
	store *Store
}

// New 构造 Service(不做重活;内置 seed 由装配层显式调 EnsureBuiltins)。
func New(db *sql.DB) Service { return &service{store: NewStore(db)} }

func (s *service) List(ctx context.Context) ([]Template, error) { return s.store.list(ctx) }

func (s *service) Get(ctx context.Context, id string) (*Template, error) { return s.store.get(ctx, id) }

func (s *service) Create(ctx context.Context, in CreateInput) (*Template, error) {
	in.Name = strings.ToLower(strings.TrimSpace(in.Name))
	if !nameRe.MatchString(in.Name) {
		return nil, ErrInvalidName
	}
	if err := validateCompose(in.ComposeYAML); err != nil {
		return nil, err
	}
	if err := validateParams(in.Params); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	tpl := &Template{
		ID: uuid.NewString(), Name: in.Name,
		DisplayName: strings.TrimSpace(in.DisplayName), Description: strings.TrimSpace(in.Description),
		Icon: strings.TrimSpace(in.Icon), ComposeYAML: in.ComposeYAML, Params: in.Params,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.insert(ctx, tpl); err != nil {
		return nil, err
	}
	return tpl, nil
}

func (s *service) Update(ctx context.Context, id string, in UpdateInput) (*Template, error) {
	cur, err := s.store.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if cur.Builtin {
		return nil, ErrBuiltin
	}
	next := *cur
	if in.DisplayName != nil {
		next.DisplayName = strings.TrimSpace(*in.DisplayName)
	}
	if in.Description != nil {
		next.Description = strings.TrimSpace(*in.Description)
	}
	if in.Icon != nil {
		next.Icon = strings.TrimSpace(*in.Icon)
	}
	if in.ComposeYAML != nil {
		if err := validateCompose(*in.ComposeYAML); err != nil {
			return nil, err
		}
		next.ComposeYAML = *in.ComposeYAML
	}
	if in.Params != nil {
		if err := validateParams(in.Params); err != nil {
			return nil, err
		}
		next.Params = in.Params
	}
	next.UpdatedAt = time.Now().UTC()
	if err := s.store.update(ctx, &next); err != nil {
		return nil, err
	}
	return &next, nil
}

func (s *service) Delete(ctx context.Context, id string) error {
	cur, err := s.store.get(ctx, id)
	if err != nil {
		return err
	}
	if cur.Builtin {
		return ErrBuiltin
	}
	return s.store.delete(ctx, id)
}

func (s *service) EnsureBuiltins(ctx context.Context) error {
	for i := range builtins {
		tpl := builtins[i]
		existing, err := s.store.getByName(ctx, tpl.Name)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				now := time.Now().UTC()
				tpl.ID = uuid.NewString()
				tpl.CreatedAt, tpl.UpdatedAt = now, now
				if err := s.store.insert(ctx, &tpl); err != nil && !errors.Is(err, ErrNameTaken) {
					return err
				}
				continue
			}
			return err
		}
		_ = existing // 已有同名行(含用户改名遗留)→ 跳过,不覆盖用户数据
	}
	return nil
}

// Render 校验并渲染参数。
func (s *service) Render(ctx context.Context, id string, params map[string]string) (*RenderResult, error) {
	tpl, err := s.store.get(ctx, id)
	if err != nil {
		return nil, err
	}
	// 未知参数先拒绝(防拼写错静默落空)。
	specs := make(map[string]ParamSpec, len(tpl.Params))
	for _, p := range tpl.Params {
		specs[p.Name] = p
	}
	for name := range params {
		if _, ok := specs[name]; !ok {
			return nil, fmt.Errorf("%w:%s", ErrUnknownParam, name)
		}
	}

	used := make(map[string]string, len(tpl.Params))
	for _, p := range tpl.Params {
		raw := strings.TrimSpace(params[p.Name])
		if raw == "" {
			raw = p.Default
		}
		if raw == "" && p.AutoGenerate && p.Type == ParamTypeSecret {
			gen, gerr := generateSecret()
			if gerr != nil {
				return nil, gerr
			}
			raw = gen
		}
		if raw == "" {
			if p.Required {
				return nil, fmt.Errorf("%w:%s", ErrMissingParam, p.Name)
			}
			continue
		}
		if err := validateValue(p, raw); err != nil {
			return nil, err
		}
		used[p.Name] = raw
	}

	compose := tpl.ComposeYAML
	for name, v := range used {
		compose = strings.ReplaceAll(compose, "{{"+name+"}}", v)
	}
	return &RenderResult{Compose: compose, Used: used}, nil
}

// ---------- 校验 ----------

func validateCompose(yaml string) error {
	yaml = strings.TrimSpace(yaml)
	if yaml == "" || len(yaml) > composeMaxBytes {
		return ErrInvalidCompose
	}
	return nil
}

func validateParams(ps []ParamSpec) error {
	if len(ps) > 32 {
		return ErrInvalidParam
	}
	seen := map[string]bool{}
	for _, p := range ps {
		if !paramNameRe.MatchString(p.Name) || seen[p.Name] {
			return ErrInvalidParam
		}
		seen[p.Name] = true
		switch p.Type {
		case ParamTypeString, ParamTypeInt, ParamTypeSecret:
		default:
			return ErrInvalidParam
		}
		if p.Default != "" {
			if err := validateValue(p, p.Default); err != nil {
				return ErrInvalidParam
			}
		}
	}
	return nil
}

func validateValue(p ParamSpec, v string) error {
	if !valueRe.MatchString(v) {
		return fmt.Errorf("%w:%s", ErrInvalidValue, p.Name)
	}
	if p.Type == ParamTypeInt {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%w:%s(须为 1..65535)", ErrInvalidValue, p.Name)
		}
	}
	return nil
}

// generateSecret 生成 24 位字母数字口令(crypto/rand)。
func generateSecret() (string, error) {
	const chars = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("appstore: generate secret: %w", err)
	}
	out := make([]byte, 24)
	for i, b := range buf {
		out[i] = chars[int(b)%len(chars)]
	}
	return string(out), nil
}
