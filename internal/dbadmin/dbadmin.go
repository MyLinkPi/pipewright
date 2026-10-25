// Package dbadmin 是「数据库管理」的领域层(MySQL/PostgreSQL 实例注册 + phpMyAdmin
// 式在线管理)。
//
// 一条 Instance 代表一个外部数据库接入(engine + host:port + 用户名);连接密码经
// vault.SealSecret 密文 BLOB 落库,绝不明文入库、出响应、进日志/审计。
//
// 只读纪律:实例默认 readonly=1,所有写操作(行编辑、DDL、导入、用户管理、SQL 控制台
// 非查询语句)在领域层拒绝(ErrReadonly);解锁是显式、留审计的独立动作。只读模式下
// SQL 控制台额外包只读事务 + 回滚双保险(服务端兜底,不信任前端开关)。
//
// 查询纪律:按实例每请求开短连接用完即关(免缓存失效/陈旧凭据);标识符一律校验 +
// 引号转义后拼接;排序列白名单 = 表实际列;结果集限行限字节。
package dbadmin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// 引擎枚举(DB 存小写字串;JSON 同名)。
const (
	EngineMySQL    = "mysql"
	EnginePostgres = "postgres"
)

// 来源枚举:manual = 设置页手工登记;appstore = 应用市场部署 mysql/postgres 自动登记。
const (
	SourceManual   = "manual"
	SourceAppStore = "appstore"
)

// TLS 模式:” = 不加密;TLSSkipVerify = 加密不校验证书(自签名场景)。
const TLSSkipVerify = "skip-verify"

// 领域错误(人话由 httpapi 层映射状态码;错误体绝不含密码/DSN)。
var (
	// ErrNotFound 表示实例不存在。
	ErrNotFound = errors.New("dbadmin: instance not found")
	// ErrInvalidEngine 表示引擎枚举非法。
	ErrInvalidEngine = errors.New("dbadmin: invalid engine")
	// ErrEmptyName 表示实例名为空。
	ErrEmptyName = errors.New("dbadmin: name must not be empty")
	// ErrEmptyHost 表示主机为空。
	ErrEmptyHost = errors.New("dbadmin: host must not be empty")
	// ErrEmptyUsername 表示用户名为空。
	ErrEmptyUsername = errors.New("dbadmin: username must not be empty")
	// ErrEmptyPassword 表示密码为空。
	ErrEmptyPassword = errors.New("dbadmin: password must not be empty")
	// ErrInvalidPort 表示端口越界(1-65535)。
	ErrInvalidPort = errors.New("dbadmin: port out of range")
	// ErrEndpointTaken 表示同引擎同地址实例已存在(唯一索引 engine+host+port)。
	ErrEndpointTaken = errors.New("dbadmin: endpoint already registered")
	// ErrReadonly 表示实例处于只读模式,写操作被拒绝。
	ErrReadonly = errors.New("dbadmin: instance is read-only")
	// ErrVaultUnconfigured 表示保险库未配置 master key,密码无法密封/解封。
	ErrVaultUnconfigured = errors.New("dbadmin: vault unconfigured")
	// ErrConnectFailed 表示数据库连接失败(错误体不含密码)。
	ErrConnectFailed = errors.New("dbadmin: connection failed")
	// ErrInvalidIdentifier 表示库/表/列标识符非法。
	ErrInvalidIdentifier = errors.New("dbadmin: invalid identifier")
	// ErrNoPrimaryKey 表示表没有主键,无法定位行(行编辑/删除需要)。
	ErrNoPrimaryKey = errors.New("dbadmin: table has no primary key")
	// ErrInvalidInput 表示管理操作入参非法(排序方向/模板枚举/用户名等)。
	ErrInvalidInput = errors.New("dbadmin: invalid input")
)

// Instance 是一条受管数据库实例的领域模型。密码只持密文 BLOB,永不出此包边界。
type Instance struct {
	ID        string
	Name      string
	Engine    string // mysql | postgres
	Host      string
	Port      int
	Username  string
	TLS       string // '' | skip-verify
	ReadOnly  bool
	Source    string // manual | appstore
	ServerID  string // 应用市场部署来源的目标服务器(可空)
	StackName string // 应用市场部署来源的栈名(可空)
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreateInput 是设置页登记实例的入参(Password 明文直达本层,由本层密封落库)。
type CreateInput struct {
	Name     string
	Engine   string
	Host     string
	Port     int
	Username string
	Password string
	TLS      string
}

// UpdateInput 是编辑实例的入参;指针字段 nil 表示不修改(Password 非 nil 即轮换)。
type UpdateInput struct {
	Name     *string
	Host     *string
	Port     *int
	Username *string
	Password *string
	TLS      *string // 非 nil 即改;空串 = 关闭加密
}

// ConnectInput 是未保存凭据的直连测试入参(密码用完即弃,绝不持久化)。
type ConnectInput struct {
	Engine   string
	Host     string
	Port     int
	Username string
	Password string
	TLS      string
}

// RegisterInput 是应用市场部署联动登记的入参(镜像 objectstore.RegisterInput:
// Secret 明文直达本层,由本层经 vault 密封后落库,调用方不留存)。
type RegisterInput struct {
	Engine    string
	Name      string
	Host      string
	Port      int
	Username  string
	Password  string
	ServerID  string
	StackName string
}

// RegisterResult 是联动登记结果:实例视图 + 是否命中既有配置原地更新。
type RegisterResult struct {
	Instance *Instance
	Updated  bool // true = 命中同 engine+host+port 既有实例,原地轮换密码
}

// TestResult 是连接测试结果(Err 文本人话,绝不含密码/DSN)。
type TestResult struct {
	OK        bool
	LatencyMs int64
	Version   string // 服务端版本(如 8.4.3 / PostgreSQL 16.4)
	Err       error
}

// Service 定义数据库管理领域对外接口(httpapi 消费)。
type Service interface {
	// ---- 实例注册 ----
	List(ctx context.Context) ([]Instance, error)
	Get(ctx context.Context, id string) (*Instance, error)
	Create(ctx context.Context, in CreateInput) (*Instance, error)
	Update(ctx context.Context, id string, in UpdateInput) (*Instance, error)
	Delete(ctx context.Context, id string) error
	// SetReadOnly 显式解锁/上锁(管理页的写模式开关;解锁留审计)。
	SetReadOnly(ctx context.Context, id string, readonly bool) (*Instance, error)
	// Test 测试已存实例连通性(解密密码 → 连接 → 取版本)。
	Test(ctx context.Context, id string) (*TestResult, error)
	// TestDirect 用未保存的凭据直连测试(密码用完即弃)。
	TestDirect(ctx context.Context, in ConnectInput) (*TestResult, error)
	// Register 应用市场部署联动登记:同 engine+host+port 已有实例 → 原地轮换密码
	// (重新部署会重新生成密码),否则新建(source=appstore)。尽力而为语义由调用方负责。
	Register(ctx context.Context, in RegisterInput) (*RegisterResult, error)

	// ---- 备份恢复受控出口(internal/dbbackup 消费;密码纪律见 backup_export.go) ----
	// ConnectBackup 开备份/回放专用连接(multiStatements、宽超时;database 空 = MySQL
	// 服务器级连接)。调用方负责 Close。
	ConnectBackup(ctx context.Context, id, database string) (*sql.DB, error)
	// RevealPassword 解密连接密码,仅供服务端备份链路构造 SSH 执行环境;绝不进响应/日志。
	RevealPassword(ctx context.Context, id string) (string, error)

	// ---- 管理操作(phpMyAdmin 式;全部按实例开短连接) ----
	ServerInfo(ctx context.Context, id string) (*ServerInfo, error)
	ListDatabases(ctx context.Context, id string) ([]DatabaseInfo, error)
	CreateDatabase(ctx context.Context, id, name string) error
	DropDatabase(ctx context.Context, id, name string) error
	ListTables(ctx context.Context, id, db string) ([]TableInfo, error)
	TableStructure(ctx context.Context, id, db, table string) (*TableStructure, error)
	Browse(ctx context.Context, id, db, table string, q BrowseQuery) (*BrowseResult, error)
	GetRow(ctx context.Context, id, db, table string, key RowKey) (map[string]any, error)
	InsertRow(ctx context.Context, id, db, table string, values map[string]any) error
	UpdateRow(ctx context.Context, id, db, table string, key RowKey, values map[string]any) error
	DeleteRow(ctx context.Context, id, db, table string, key RowKey) error
	DropTable(ctx context.Context, id, db, table string) error
	TruncateTable(ctx context.Context, id, db, table string) error
	// RunSQL 执行单条语句(SQL 控制台):SELECT 类返回行集,其余返回影响行数;
	// 只读模式下包只读事务 + 回滚,任何写语句由服务端拒绝。
	RunSQL(ctx context.Context, id, database, statement string) (*QueryResult, error)
	// Export 流式导出(Table 为空 = 整库;CSV 仅支持单表)。w 为响应流,可 Flush 则分块刷出。
	Export(ctx context.Context, id string, spec ExportSpec, w io.Writer) error
	// Import 执行多语句 .sql 文本(要求已解锁)。
	Import(ctx context.Context, id, database, dump string) (*ImportResult, error)

	// ---- 用户管理 ----
	ListUsers(ctx context.Context, id string) ([]DBUser, error)
	CreateUser(ctx context.Context, id string, in UserInput) error
	SetUserPassword(ctx context.Context, id string, in UserInput) error
	DropUser(ctx context.Context, id string, ref UserRef) error
	// UserGrants 展示一个用户的授权(MySQL: SHOW GRANTS 原文行;PG: 库级权限矩阵)。
	UserGrants(ctx context.Context, id string, ref UserRef) (*GrantsView, error)
	GrantDatabase(ctx context.Context, id string, in GrantInput) error
	RevokeDatabase(ctx context.Context, id string, in GrantInput) error
}

// service 是 store + vault 支撑的 Service 实现。
type service struct {
	db    *sql.DB
	vault vault.Vault
}

// New 构造数据库管理服务。vault 未配置 master key 时实例登记/连接不可用
// (ErrVaultUnconfigured),但不影响平台启动。
func New(db *sql.DB, v vault.Vault) Service {
	return &service{db: db, vault: v}
}

// ---- 实例 CRUD ----

// validEngine 报告引擎枚举是否受支持。
func validEngine(e string) bool {
	return e == EngineMySQL || e == EnginePostgres
}

// validTLS 报告 TLS 模式枚举是否受支持。
func validTLS(t string) bool {
	return t == "" || t == TLSSkipVerify
}

// normalizeCreate 归一化登记入参(去空白;引擎/TLS 小写)。
func normalizeCreate(in *CreateInput) {
	in.Engine = strings.ToLower(strings.TrimSpace(in.Engine))
	in.TLS = strings.ToLower(strings.TrimSpace(in.TLS))
	in.Name = strings.TrimSpace(in.Name)
	in.Host = strings.TrimSpace(in.Host)
	in.Username = strings.TrimSpace(in.Username)
}

// validateCreate 校验登记入参。
func validateCreate(in CreateInput) error {
	if !validEngine(in.Engine) {
		return ErrInvalidEngine
	}
	if in.Name == "" {
		return ErrEmptyName
	}
	if in.Host == "" {
		return ErrEmptyHost
	}
	if in.Port < 1 || in.Port > 65535 {
		return ErrInvalidPort
	}
	if in.Username == "" {
		return ErrEmptyUsername
	}
	if in.Password == "" {
		return ErrEmptyPassword
	}
	if !validTLS(in.TLS) {
		return fmt.Errorf("%w: tls", ErrInvalidInput)
	}
	return nil
}

// isUniqueViolation 判断是否 engine+host+port 唯一索引冲突(双方言文本匹配)。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToUpper(err.Error())
	return strings.Contains(msg, "IDX_DB_INSTANCES_ENDPOINT") ||
		strings.Contains(msg, "UNIQUE CONSTRAINT FAILED: DB_INSTANCES") ||
		strings.Contains(msg, "DUPLICATE ENTRY")
}

// Create 登记(密封密码)→ 落库;同引擎同地址已存在 → ErrEndpointTaken。
func (s *service) Create(ctx context.Context, in CreateInput) (*Instance, error) {
	normalizeCreate(&in)
	if err := validateCreate(in); err != nil {
		return nil, err
	}
	sealed, err := s.vault.SealSecret([]byte(in.Password))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrVaultUnconfigured, "master key not configured")
	}
	now := time.Now().UTC()
	inst := &Instance{
		ID: uuid.NewString(), Name: in.Name, Engine: in.Engine, Host: in.Host, Port: in.Port,
		Username: in.Username, TLS: in.TLS, ReadOnly: true, Source: SourceManual,
		CreatedAt: now, UpdatedAt: now,
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO db_instances (id, name, engine, host, port, username, password_sealed, tls, readonly, source, server_id, stack_name, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, 'manual', '', '', ?, ?)`,
		inst.ID, inst.Name, inst.Engine, inst.Host, inst.Port, inst.Username, sealed, inst.TLS,
		now.Format(time.RFC3339), now.Format(time.RFC3339),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrEndpointTaken
		}
		return nil, fmt.Errorf("dbadmin: insert instance: %w", err)
	}
	return inst, nil
}

// List 返回全部实例(创建时间倒序;无密码)。
func (s *service) List(ctx context.Context) ([]Instance, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, engine, host, port, username, tls, readonly, source, server_id, stack_name, created_at, updated_at
		 FROM db_instances ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("dbadmin: list instances: %w", err)
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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dbadmin: iterate instances: %w", err)
	}
	return out, nil
}

// Get 读取单条实例;不存在 → ErrNotFound。
func (s *service) Get(ctx context.Context, id string) (*Instance, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, engine, host, port, username, tls, readonly, source, server_id, stack_name, created_at, updated_at
		 FROM db_instances WHERE id = ?`, id)
	inst, err := scanInstance(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return inst, nil
}

// Update 编辑实例(指针字段 nil 不改;Password 非 nil 即轮换);返回更新后视图。
func (s *service) Update(ctx context.Context, id string, in UpdateInput) (*Instance, error) {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" {
			return nil, ErrEmptyName
		}
		inst.Name = name
	}
	if in.Host != nil {
		host := strings.TrimSpace(*in.Host)
		if host == "" {
			return nil, ErrEmptyHost
		}
		inst.Host = host
	}
	if in.Port != nil {
		if *in.Port < 1 || *in.Port > 65535 {
			return nil, ErrInvalidPort
		}
		inst.Port = *in.Port
	}
	if in.Username != nil {
		u := strings.TrimSpace(*in.Username)
		if u == "" {
			return nil, ErrEmptyUsername
		}
		inst.Username = u
	}
	if in.TLS != nil {
		tls := strings.ToLower(strings.TrimSpace(*in.TLS))
		if !validTLS(tls) {
			return nil, fmt.Errorf("%w: tls", ErrInvalidInput)
		}
		inst.TLS = tls
	}
	setPassword, sealed := "", []byte(nil)
	if in.Password != nil {
		if strings.TrimSpace(*in.Password) == "" {
			return nil, ErrEmptyPassword
		}
		var err error
		sealed, err = s.vault.SealSecret([]byte(*in.Password))
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrVaultUnconfigured, "master key not configured")
		}
		setPassword = ", password_sealed = ?"
	}
	// 端点变更可能与既有实例撞唯一索引 → ErrEndpointTaken。
	var res sql.Result
	if setPassword != "" {
		res, err = s.db.ExecContext(ctx,
			`UPDATE db_instances SET name = ?, host = ?, port = ?, username = ?, tls = ?`+setPassword+`, updated_at = ? WHERE id = ?`,
			inst.Name, inst.Host, inst.Port, inst.Username, inst.TLS, sealed, time.Now().UTC().Format(time.RFC3339), id)
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE db_instances SET name = ?, host = ?, port = ?, username = ?, tls = ?, updated_at = ? WHERE id = ?`,
			inst.Name, inst.Host, inst.Port, inst.Username, inst.TLS, time.Now().UTC().Format(time.RFC3339), id)
	}
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrEndpointTaken
		}
		return nil, fmt.Errorf("dbadmin: update instance: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.Get(ctx, id)
}

// Delete 删除实例(仅注销登记;不影响数据库本身)。
func (s *service) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM db_instances WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("dbadmin: delete instance: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetReadOnly 解锁/上锁写模式。
func (s *service) SetReadOnly(ctx context.Context, id string, readonly bool) (*Instance, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	ro := 0
	if readonly {
		ro = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE db_instances SET readonly = ?, updated_at = ? WHERE id = ?`,
		ro, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return nil, fmt.Errorf("dbadmin: set readonly: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.Get(ctx, id)
}

// Test 测试已存实例连通性:解密密码 → Ping + 取版本 → 报延迟。
func (s *service) Test(ctx context.Context, id string) (*TestResult, error) {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.testConnect(ctx, inst.Engine, inst.Host, inst.Port, inst.Username, inst.TLS, func() (string, error) {
		pass, err := s.vault.OpenSecret(s.passwordSealed(ctx, id))
		if err != nil {
			return "", err
		}
		return string(pass), nil
	})
}

// TestDirect 用未保存的凭据直连测试(密码用完即弃)。
func (s *service) TestDirect(ctx context.Context, in ConnectInput) (*TestResult, error) {
	in.Engine = strings.ToLower(strings.TrimSpace(in.Engine))
	in.TLS = strings.ToLower(strings.TrimSpace(in.TLS))
	if !validEngine(in.Engine) {
		return nil, ErrInvalidEngine
	}
	if in.Host == "" || in.Port < 1 || in.Port > 65535 || in.Username == "" || in.Password == "" {
		return nil, fmt.Errorf("%w: connect fields", ErrInvalidInput)
	}
	return s.testConnect(ctx, in.Engine, in.Host, in.Port, in.Username, in.TLS, func() (string, error) {
		return in.Password, nil
	})
}

// testConnect 统一连接测试:惰性取密码 → 连接取版本 → 报延迟;连接失败返回
// OK=false + 人话错误(绝不含密码/DSN),不算领域错误(200 响应)。
func (s *service) testConnect(ctx context.Context, engine, host string, port int, username, tlsMode string, password func() (string, error)) (*TestResult, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	db, err := s.openEngine(ctx, engine, host, port, username, tlsMode, "", password, connOpts{})
	if err != nil {
		return &TestResult{OK: false, Err: err}, nil
	}
	defer db.Close()
	version, err := serverVersion(ctx, engine, db)
	if err != nil {
		return &TestResult{OK: false, Err: humanConnectError(err)}, nil
	}
	return &TestResult{OK: true, LatencyMs: time.Since(start).Milliseconds(), Version: version}, nil
}

// passwordSealed 读取实例的密码密文 BLOB(仅进程内解密用)。
func (s *service) passwordSealed(ctx context.Context, id string) []byte {
	var sealed []byte
	_ = s.db.QueryRowContext(ctx, `SELECT password_sealed FROM db_instances WHERE id = ?`, id).Scan(&sealed)
	return sealed
}

// Register 应用市场联动登记:同 engine+host+port 已有实例 → 原地轮换密码(幂等,
// 不改名称/只读状态);否则新建 source=appstore 实例。
func (s *service) Register(ctx context.Context, in RegisterInput) (*RegisterResult, error) {
	in.Engine = strings.ToLower(strings.TrimSpace(in.Engine))
	if !validEngine(in.Engine) {
		return nil, ErrInvalidEngine
	}
	if in.Host == "" || in.Port < 1 || in.Port > 65535 || in.Username == "" || in.Password == "" {
		return nil, fmt.Errorf("%w: register fields", ErrInvalidInput)
	}
	sealed, err := s.vault.SealSecret([]byte(in.Password))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrVaultUnconfigured, "master key not configured")
	}
	var existingID string
	err = s.db.QueryRowContext(ctx,
		`SELECT id FROM db_instances WHERE engine = ? AND host = ? AND port = ?`, in.Engine, in.Host, in.Port,
	).Scan(&existingID)
	now := time.Now().UTC().Format(time.RFC3339)
	if err == nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE db_instances SET username = ?, password_sealed = ?, stack_name = ?, updated_at = ? WHERE id = ?`,
			in.Username, sealed, in.StackName, now, existingID); err != nil {
			return nil, fmt.Errorf("dbadmin: register rotate: %w", err)
		}
		inst, err := s.Get(ctx, existingID)
		if err != nil {
			return nil, err
		}
		return &RegisterResult{Instance: inst, Updated: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("dbadmin: register lookup: %w", err)
	}
	id := uuid.NewString()
	inst := &Instance{
		ID: id, Name: in.Name, Engine: in.Engine, Host: in.Host, Port: in.Port,
		Username: in.Username, TLS: "", ReadOnly: true, Source: SourceAppStore,
		ServerID: in.ServerID, StackName: in.StackName,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO db_instances (id, name, engine, host, port, username, password_sealed, tls, readonly, source, server_id, stack_name, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, '', 1, 'appstore', ?, ?, ?, ?)`,
		id, inst.Name, inst.Engine, inst.Host, inst.Port, inst.Username, sealed, in.ServerID, in.StackName, now, now,
	); err != nil {
		if isUniqueViolation(err) {
			return s.Register(ctx, RegisterInput(in)) // 并发窗口:命中唯一索引则按更新语义重试一次
		}
		return nil, fmt.Errorf("dbadmin: register insert: %w", err)
	}
	return &RegisterResult{Instance: inst, Updated: false}, nil
}

// scanner 抽象 *sql.Row 与 *sql.Rows 的 Scan。
type scanner interface {
	Scan(dest ...any) error
}

// scanInstance 把一行扫描为 Instance(永不读 password_sealed)。
func scanInstance(sc scanner) (*Instance, error) {
	var (
		inst       Instance
		ro         int
		createdStr string
		updatedStr string
	)
	if err := sc.Scan(&inst.ID, &inst.Name, &inst.Engine, &inst.Host, &inst.Port, &inst.Username,
		&inst.TLS, &ro, &inst.Source, &inst.ServerID, &inst.StackName, &createdStr, &updatedStr); err != nil {
		return nil, err
	}
	inst.ReadOnly = ro != 0
	var err error
	if inst.CreatedAt, err = time.Parse(time.RFC3339, createdStr); err != nil {
		return nil, fmt.Errorf("dbadmin: parse created_at: %w", err)
	}
	if inst.UpdatedAt, err = time.Parse(time.RFC3339, updatedStr); err != nil {
		return nil, fmt.Errorf("dbadmin: parse updated_at: %w", err)
	}
	return &inst, nil
}

// hostPort 拼接 host:port(地址形态统一给连接层/错误信息)。
func hostPort(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}
