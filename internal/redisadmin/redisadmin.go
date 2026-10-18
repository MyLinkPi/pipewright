// Package redisadmin 是「Redis 管理」的领域层(实例注册 + 键浏览/删改;模式对齐 dbadmin)。
//
// 一条 Instance 代表一个外部 Redis 接入(host:port + 可选密码 + 默认逻辑库号);连接密码
// 经 vault.SealSecret 密文 BLOB 落库,绝不明文入库、出响应、进日志/审计。无密码实例
// (内网自建)合法:password_sealed 为空即无密码。
//
// 只读纪律:实例默认 readonly=1,写操作(删键、改 TTL)在领域层拒绝(ErrReadonly);
// 解锁是显式、留审计的独立动作(管理页写模式开关)。
//
// 键空间纪律:Redis 键可能百万级,浏览一律 SCAN 游标分页(领域层循环补齐页大小,
// 游标归零即遍历完成),绝不 KEYS *;值预览按类型截断(string 前 2KB / 集合前 100 成员)。
//
// 连接纪律:每操作即建即弃短连接(免缓存失效/陈旧凭据),拨号 3s/读写 5s 快速失败。
package redisadmin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// 来源枚举:manual = 设置页手工登记;appstore = 应用市场部署 redis 自动登记。
const (
	SourceManual   = "manual"
	SourceAppStore = "appstore"
)

// 领域错误(人话由 httpapi 层映射状态码;错误体绝不含密码/地址凭据)。
var (
	// ErrNotFound 表示实例不存在。
	ErrNotFound = errors.New("redisadmin: instance not found")
	// ErrEmptyName 表示实例名为空。
	ErrEmptyName = errors.New("redisadmin: name must not be empty")
	// ErrEmptyHost 表示主机为空。
	ErrEmptyHost = errors.New("redisadmin: host must not be empty")
	// ErrInvalidPort 表示端口越界(1-65535)。
	ErrInvalidPort = errors.New("redisadmin: port out of range")
	// ErrInvalidDB 表示逻辑库号越界(0-15)。
	ErrInvalidDB = errors.New("redisadmin: db out of range")
	// ErrEndpointTaken 表示同地址实例已存在(唯一索引 host+port)。
	ErrEndpointTaken = errors.New("redisadmin: endpoint already registered")
	// ErrReadonly 表示实例处于只读模式,写操作被拒绝。
	ErrReadonly = errors.New("redisadmin: instance is read-only")
	// ErrVaultUnconfigured 表示保险库未配置 master key,密码无法密封/解封。
	ErrVaultUnconfigured = errors.New("redisadmin: vault unconfigured")
	// ErrConnectFailed 表示 Redis 连接失败(错误体不含密码)。
	ErrConnectFailed = errors.New("redisadmin: connection failed")
	// ErrKeyNotFound 表示目标键不存在(删除/改 TTL 时已被移除)。
	ErrKeyNotFound = errors.New("redisadmin: key not found")
	// ErrInvalidInput 表示管理操作入参非法。
	ErrInvalidInput = errors.New("redisadmin: invalid input")
	// ErrEmptyPassword 表示自动登记入参的密码为空(应用商店 redis 模板必有 requirepass)。
	ErrEmptyPassword = errors.New("redisadmin: password must not be empty")
)

// Instance 是一条受管 Redis 实例的领域模型。密码只持密文 BLOB,永不出此包边界;
// PasswordConfigured 为 store 层计算的「是否配置了密码」(无密码实例合法)。
type Instance struct {
	ID        string
	Name      string
	Host      string
	Port      int
	DBNum     int // 默认逻辑库号(0-15;页面可临时切换,实例只记默认值)
	ReadOnly  bool
	Source    string // manual | appstore
	ServerID  string // 应用市场部署来源的目标服务器(可空)
	StackName string // 应用市场部署来源的栈名(可空)
	PasswordConfigured bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreateInput 是设置页登记实例的入参(Password 明文直达本层,由本层密封落库;空 = 无密码实例)。
type CreateInput struct {
	Name     string
	Host     string
	Port     int
	DBNum    int
	Password string
}

// UpdateInput 是编辑实例的入参;指针字段 nil 表示不修改(Password 非 nil 即轮换,空串 = 清除密码)。
type UpdateInput struct {
	Name     *string
	Host     *string
	Port     *int
	DBNum    *int
	Password *string
}

// ConnectInput 是未保存凭据的直连测试入参(密码用完即弃,绝不持久化)。
type ConnectInput struct {
	Host     string
	Port     int
	DBNum    int
	Password string
}

// RegisterInput 是应用市场部署联动登记的入参(镜像 dbadmin.RegisterInput 语义:
// 密码明文直达本层,由本层经 vault 密封后落库,调用方不留存)。
type RegisterInput struct {
	Name      string
	Host      string
	Port      int
	Password  string
	ServerID  string
	StackName string
}

// RegisterResult 是联动登记结果:实例视图 + 是否命中既有实例原地更新。
type RegisterResult struct {
	Instance *Instance
	Updated  bool // true = 命中同 host+port 既有实例,原地轮换密码
}

// TestResult 是连接测试结果(Err 文本人话,绝不含密码)。
type TestResult struct {
	OK        bool
	LatencyMs int64
	Version   string // 服务端版本(如 7.4.1)
	Err       error
}

// Service 定义 Redis 管理领域对外接口(httpapi 消费)。
type Service interface {
	// ---- 实例注册 ----
	List(ctx context.Context) ([]Instance, error)
	Get(ctx context.Context, id string) (*Instance, error)
	Create(ctx context.Context, in CreateInput) (*Instance, error)
	Update(ctx context.Context, id string, in UpdateInput) (*Instance, error)
	Delete(ctx context.Context, id string) error
	// SetReadOnly 显式解锁/上锁(管理页写模式开关;解锁留审计)。
	SetReadOnly(ctx context.Context, id string, readonly bool) (*Instance, error)
	// Test 测试已存实例连通性(解密密码 → PING → 取版本)。
	Test(ctx context.Context, id string) (*TestResult, error)
	// TestDirect 用未保存的凭据直连测试(密码用完即弃)。
	TestDirect(ctx context.Context, in ConnectInput) (*TestResult, error)
	// Register 应用市场部署联动登记:同 host+port 已有实例 → 原地轮换密码
	// (重新部署会重新生成密码),否则新建(source=appstore)。尽力而为语义由调用方负责。
	Register(ctx context.Context, in RegisterInput) (*RegisterResult, error)

	// ---- 管理操作(每操作即建即弃短连接;dbNum 为本次操作的目标逻辑库)----
	// Info 服务端概览:版本/uptime/内存/连接数 + 各逻辑库键数(keyspace)与当前库 DBSIZE。
	Info(ctx context.Context, id string, dbNum int) (*ServerInfo, error)
	// ScanKeys SCAN 游标分页浏览键(match 为 glob 模式;键空间可能百万级,绝不 KEYS *)。
	ScanKeys(ctx context.Context, id string, dbNum int, in ScanInput) (*ScanResult, error)
	// KeyDetail 单键详情:类型/TTL/内存占用 + 按类型的值预览(截断)。
	KeyDetail(ctx context.Context, id string, dbNum int, key string) (*KeyDetail, error)
	// DeleteKey 删除键(须已解锁写模式;键不存在 → ErrKeyNotFound)。
	DeleteKey(ctx context.Context, id string, dbNum int, key string) error
	// SetTTL 设置/清除过期(ttlSeconds >= 0 = EXPIRE;-1 = PERSIST 持久化;须已解锁)。
	SetTTL(ctx context.Context, id string, dbNum int, key string, ttlSeconds int64) error
}

// service 是 store + vault + dialFactory 支撑的 Service 实现。
type service struct {
	db    *sql.DB
	vault vault.Vault
	dial  dialFunc
}

// New 构造 Redis 管理服务。vault 未配置 master key 时带密码实例的登记/连接不可用
//(ErrVaultUnconfigured),但不影响平台启动。
func New(db *sql.DB, v vault.Vault) Service {
	return &service{db: db, vault: v, dial: prodDial}
}

// ---- 实例 CRUD ----

// normalizeCreate 归一化登记入参(去空白)。
func normalizeCreate(in *CreateInput) {
	in.Name = strings.TrimSpace(in.Name)
	in.Host = strings.TrimSpace(in.Host)
}

// validateCreate 校验登记入参(密码可空 = 无密码实例)。
func validateCreate(in CreateInput) error {
	if in.Name == "" {
		return ErrEmptyName
	}
	if in.Host == "" {
		return ErrEmptyHost
	}
	if in.Port < 1 || in.Port > 65535 {
		return ErrInvalidPort
	}
	if in.DBNum < 0 || in.DBNum > 15 {
		return ErrInvalidDB
	}
	return nil
}

// isUniqueViolation 判断是否 host+port 唯一索引冲突(双方言文本匹配)。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToUpper(err.Error())
	return strings.Contains(msg, "IDX_REDIS_INSTANCES_ENDPOINT") ||
		strings.Contains(msg, "UNIQUE CONSTRAINT FAILED: REDIS_INSTANCES") ||
		strings.Contains(msg, "DUPLICATE ENTRY")
}

// seal 密码入 vault(空密码 → 空 BLOB,无需 vault)。
func (s *service) seal(password string) ([]byte, error) {
	if password == "" {
		return nil, nil
	}
	if s.vault == nil {
		return nil, fmt.Errorf("%w: %s", ErrVaultUnconfigured, "master key not configured")
	}
	sealed, err := s.vault.SealSecret([]byte(password))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrVaultUnconfigured, "master key not configured")
	}
	return sealed, nil
}

// open 解封密码(空 BLOB → 空密码)。
func (s *service) open(sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	if s.vault == nil {
		return "", ErrVaultUnconfigured
	}
	plain, err := s.vault.OpenSecret(sealed)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrVaultUnconfigured, "master key not configured")
	}
	return string(plain), nil
}

// Create 登记(密封密码)→ 落库;同地址已存在 → ErrEndpointTaken。
func (s *service) Create(ctx context.Context, in CreateInput) (*Instance, error) {
	normalizeCreate(&in)
	if err := validateCreate(in); err != nil {
		return nil, err
	}
	sealed, err := s.seal(in.Password)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	inst := &Instance{
		ID: uuid.NewString(), Name: in.Name, Host: in.Host, Port: in.Port, DBNum: in.DBNum,
		ReadOnly: true, Source: SourceManual,
		CreatedAt: now, UpdatedAt: now,
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO redis_instances (id, name, host, port, password_sealed, db_num, readonly, source, server_id, stack_name, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, 1, 'manual', '', '', ?, ?)`,
		inst.ID, inst.Name, inst.Host, inst.Port, sealed, inst.DBNum,
		now.Format(time.RFC3339), now.Format(time.RFC3339),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrEndpointTaken
		}
		return nil, fmt.Errorf("redisadmin: insert instance: %w", err)
	}
	return s.Get(ctx, inst.ID) // 回读:PasswordConfigured 等派生字段由 store 计算
}

// List 返回全部实例(创建时间倒序;无密码)。
func (s *service) List(ctx context.Context) ([]Instance, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, host, port, db_num, readonly, source, server_id, stack_name, created_at, updated_at, LENGTH(COALESCE(password_sealed, ''))
		 FROM redis_instances ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("redisadmin: list instances: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]Instance, 0)
	for rows.Next() {
		inst, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *inst)
	}
	return out, rows.Err()
}

// scanner 抽象 *sql.Row 与 *sql.Rows 的 Scan。
type scanner interface {
	Scan(dest ...any) error
}

// scanInstance 把一行扫描为 Instance(LENGTH(COALESCE(password_sealed, '')) 计算 PasswordConfigured,
// 绝不读密文本身)。
func scanInstance(sc scanner) (*Instance, error) {
	var (
		inst        Instance
		ro          int
		pwLen       int
		createdStr  string
		updatedStr  string
	)
	if err := sc.Scan(&inst.ID, &inst.Name, &inst.Host, &inst.Port, &inst.DBNum,
		&ro, &inst.Source, &inst.ServerID, &inst.StackName, &createdStr, &updatedStr, &pwLen); err != nil {
		return nil, err
	}
	inst.ReadOnly = ro != 0
	inst.PasswordConfigured = pwLen > 0
	var err error
	if inst.CreatedAt, err = time.Parse(time.RFC3339, createdStr); err != nil {
		return nil, fmt.Errorf("redisadmin: parse created_at: %w", err)
	}
	if inst.UpdatedAt, err = time.Parse(time.RFC3339, updatedStr); err != nil {
		return nil, fmt.Errorf("redisadmin: parse updated_at: %w", err)
	}
	return &inst, nil
}

// Get 返回单条实例(无密码);不存在 → ErrNotFound。
func (s *service) Get(ctx context.Context, id string) (*Instance, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, host, port, db_num, readonly, source, server_id, stack_name, created_at, updated_at, LENGTH(COALESCE(password_sealed, ''))
		 FROM redis_instances WHERE id = ?`, id)
	inst, err := scanInstance(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("redisadmin: get instance: %w", err)
	}
	return inst, nil
}

// Update 编辑实例(指针字段部分更新;Password 非 nil 即轮换,空串 = 清除密码)。
func (s *service) Update(ctx context.Context, id string, in UpdateInput) (*Instance, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	sets, args := []string{"updated_at = ?"}, []any{time.Now().UTC().Format(time.RFC3339)}
	if in.Name != nil {
		if strings.TrimSpace(*in.Name) == "" {
			return nil, ErrEmptyName
		}
		sets = append(sets, "name = ?")
		args = append(args, strings.TrimSpace(*in.Name))
	}
	if in.Host != nil {
		if strings.TrimSpace(*in.Host) == "" {
			return nil, ErrEmptyHost
		}
		sets = append(sets, "host = ?")
		args = append(args, strings.TrimSpace(*in.Host))
	}
	if in.Port != nil {
		if *in.Port < 1 || *in.Port > 65535 {
			return nil, ErrInvalidPort
		}
		sets = append(sets, "port = ?")
		args = append(args, *in.Port)
	}
	if in.DBNum != nil {
		if *in.DBNum < 0 || *in.DBNum > 15 {
			return nil, ErrInvalidDB
		}
		sets = append(sets, "db_num = ?")
		args = append(args, *in.DBNum)
	}
	if in.Password != nil {
		sealed, err := s.seal(*in.Password)
		if err != nil {
			return nil, err
		}
		sets = append(sets, "password_sealed = ?")
		args = append(args, sealed)
	}
	args = append(args, id)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE redis_instances SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrEndpointTaken
		}
		return nil, fmt.Errorf("redisadmin: update instance: %w", err)
	}
	return s.Get(ctx, id)
}

// Delete 删除实例;不存在 → ErrNotFound。
func (s *service) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM redis_instances WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("redisadmin: delete instance: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetReadOnly 解锁/上锁写模式。
func (s *service) SetReadOnly(ctx context.Context, id string, readonly bool) (*Instance, error) {
	ro := 0
	if readonly {
		ro = 1
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE redis_instances SET readonly = ?, updated_at = ? WHERE id = ?`,
		ro, time.Now().UTC().Format(time.RFC3339), id); err != nil {
		return nil, fmt.Errorf("redisadmin: set readonly: %w", err)
	}
	return s.Get(ctx, id)
}

// passwordSealedOf 取指定实例的密码密文 BLOB(内部用)。
func (s *service) passwordSealedOf(ctx context.Context, id string) []byte {
	var sealed []byte
	_ = s.db.QueryRowContext(ctx, `SELECT password_sealed FROM redis_instances WHERE id = ?`, id).Scan(&sealed)
	return sealed
}

// Register 应用市场联动登记:同 host+port 已有实例 → 原地轮换密码(幂等,不改名称/只读
// 状态);否则新建 source=appstore 实例。密码必填(应用商店 redis 模板必有 requirepass)。
func (s *service) Register(ctx context.Context, in RegisterInput) (*RegisterResult, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Host = strings.TrimSpace(in.Host)
	if in.Name == "" {
		return nil, ErrEmptyName
	}
	if in.Host == "" {
		return nil, ErrEmptyHost
	}
	if in.Port < 1 || in.Port > 65535 {
		return nil, ErrInvalidPort
	}
	if in.Password == "" {
		return nil, ErrEmptyPassword
	}
	sealed, err := s.seal(in.Password)
	if err != nil {
		return nil, err
	}
	var existingID string
	err = s.db.QueryRowContext(ctx,
		`SELECT id FROM redis_instances WHERE host = ? AND port = ?`, in.Host, in.Port,
	).Scan(&existingID)
	now := time.Now().UTC().Format(time.RFC3339)
	if err == nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE redis_instances SET password_sealed = ?, stack_name = ?, updated_at = ? WHERE id = ?`,
			sealed, in.StackName, now, existingID); err != nil {
			return nil, fmt.Errorf("redisadmin: register rotate: %w", err)
		}
		inst, err := s.Get(ctx, existingID)
		if err != nil {
			return nil, err
		}
		return &RegisterResult{Instance: inst, Updated: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("redisadmin: register lookup: %w", err)
	}
	id := uuid.NewString()
	inst := &Instance{
		ID: id, Name: in.Name, Host: in.Host, Port: in.Port, DBNum: 0,
		ReadOnly: true, Source: SourceAppStore,
		ServerID: in.ServerID, StackName: in.StackName,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO redis_instances (id, name, host, port, password_sealed, db_num, readonly, source, server_id, stack_name, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, 0, 1, 'appstore', ?, ?, ?, ?)`,
		id, inst.Name, inst.Host, inst.Port, sealed, in.ServerID, in.StackName, now, now,
	); err != nil {
		if isUniqueViolation(err) {
			return s.Register(ctx, in) // 并发窗口:命中唯一索引则按更新语义重试一次
		}
		return nil, fmt.Errorf("redisadmin: register insert: %w", err)
	}
	created, err := s.Get(ctx, id) // 回读:PasswordConfigured 等派生字段由 store 计算
	if err != nil {
		return nil, err
	}
	return &RegisterResult{Instance: created, Updated: false}, nil
}

// hostPort 拼接 host:port(地址形态统一给连接层/错误信息)。
func hostPort(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}
