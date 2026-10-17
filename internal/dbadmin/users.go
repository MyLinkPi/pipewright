package dbadmin

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// 授权模板枚举(前端下拉:全部权限 / 只读 / 增删改查)。
const (
	GrantAll  = "all"
	GrantRead = "read"
	GrantCRUD = "crud"
)

// DBUser 是一个数据库账号(MySQL 专有 host 通配段;PG 的 Host 恒空)。
type DBUser struct {
	Name  string `json:"name"`
	Host  string `json:"host,omitempty"`
	Super bool   `json:"super,omitempty"`
}

// UserInput 是建号/改密入参(Password 用完即弃,绝不持久化/审计)。
type UserInput struct {
	Name     string
	Host     string
	Password string
}

// UserRef 是账号定位(名 + MySQL host 段)。
type UserRef struct {
	Name string
	Host string
}

// GrantInput 是库级授权/回收入参(Template: all | read | crud;回收忽略模板)。
type GrantInput struct {
	Name     string
	Host     string
	Database string
	Template string
}

// GrantsView 是账号授权展示:MySQL 为 SHOW GRANTS 原文行;PG 为库级 CONNECT 矩阵。
type GrantsView struct {
	Engine string       `json:"engine"`
	Lines  []string     `json:"lines,omitempty"`
	Rows   []GrantDBRow `json:"rows,omitempty"`
}

// GrantDBRow 是 PG 一个库的连接权限。
type GrantDBRow struct {
	Database string `json:"database"`
	Connect  bool   `json:"connect"`
}

// reMySQLUser / reMySQLHost:账号名与 host 段白名单(防引号/控制字符进 DDL 字面量)。
var (
	reMySQLUser = regexp.MustCompile(`^[A-Za-z0-9_$.,%*+-]{1,96}$`)
	reMySQLHost = regexp.MustCompile(`^[A-Za-z0-9_:.%/\[\]-]{1,255}$`)
	rePGRole    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,63}$`)
)

// validateUserRef 校验账号定位(MySQL 还校验 host 段)。
func validateUserRef(engine string, ref UserRef) error {
	ref.Name = strings.TrimSpace(ref.Name)
	if ref.Name == "" {
		return fmt.Errorf("%w: user name", ErrInvalidInput)
	}
	if engine == EngineMySQL {
		if !reMySQLUser.MatchString(ref.Name) {
			return fmt.Errorf("%w: user name", ErrInvalidInput)
		}
		if ref.Host != "" && !reMySQLHost.MatchString(ref.Host) {
			return fmt.Errorf("%w: user host", ErrInvalidInput)
		}
	} else if !rePGRole.MatchString(ref.Name) {
		return fmt.Errorf("%w: role name", ErrInvalidInput)
	}
	return nil
}

// mysqlUserLiteral 组 'u'@'h' 字面量(单引号双写转义)。
func mysqlUserLiteral(ref UserRef) string {
	host := ref.Host
	if host == "" {
		host = "%"
	}
	return "'" + strings.ReplaceAll(ref.Name, "'", "''") + "'@'" + strings.ReplaceAll(host, "'", "''") + "'"
}

// mysqlPasswordLiteral 组 'p' 字面量(反斜杠与单引号双写,兼容两种转义模式)。
func mysqlPasswordLiteral(p string) string {
	p = strings.ReplaceAll(p, `\`, `\\`)
	p = strings.ReplaceAll(p, `'`, `''`)
	return "'" + p + "'"
}

// pgPasswordLiteral 组 'p' 字面量(standard_conforming_strings:仅单引号双写)。
func pgPasswordLiteral(p string) string {
	return "'" + strings.ReplaceAll(p, `'`, `''`) + "'"
}

// userCtx 统一「取实例 + 解锁守卫」前置。
func (s *service) userCtx(ctx context.Context, id string) (*Instance, error) {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := ensureWritable(inst); err != nil {
		return nil, err
	}
	return inst, nil
}

// ListUsers 列账号(MySQL 读 mysql.user——需管理员凭据;PG 读 pg_roles)。
func (s *service) ListUsers(ctx context.Context, id string) ([]DBUser, error) {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	db, err := s.connectAnyDB(qctx, inst, connOpts{})
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if inst.Engine == EngineMySQL {
		rows, err := db.QueryContext(qctx, `SELECT user, host FROM mysql.user ORDER BY user, host`)
		if err != nil {
			return nil, humanQueryError(err)
		}
		defer rows.Close()
		out := []DBUser{}
		for rows.Next() {
			var u DBUser
			if err := rows.Scan(&u.Name, &u.Host); err != nil {
				return nil, humanQueryError(err)
			}
			out = append(out, u)
		}
		return out, humanQueryError(rows.Err())
	}
	rows, err := db.QueryContext(qctx, `SELECT rolname, rolsuper FROM pg_roles WHERE rolcanlogin ORDER BY rolname`)
	if err != nil {
		return nil, humanQueryError(err)
	}
	defer rows.Close()
	out := []DBUser{}
	for rows.Next() {
		var u DBUser
		if err := rows.Scan(&u.Name, &u.Super); err != nil {
			return nil, humanQueryError(err)
		}
		out = append(out, u)
	}
	return out, humanQueryError(rows.Err())
}

// CreateUser 建账号(MySQL CREATE USER;PG CREATE ROLE ... LOGIN)。
func (s *service) CreateUser(ctx context.Context, id string, in UserInput) error {
	inst, err := s.userCtx(ctx, id)
	if err != nil {
		return err
	}
	if err := validateUserRef(inst.Engine, UserRef{Name: in.Name, Host: in.Host}); err != nil {
		return err
	}
	if in.Password == "" {
		return fmt.Errorf("%w: password", ErrInvalidInput)
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	db, err := s.connectAnyDB(qctx, inst, connOpts{})
	if err != nil {
		return err
	}
	defer db.Close()
	var stmt string
	if inst.Engine == EngineMySQL {
		stmt = fmt.Sprintf("CREATE USER %s IDENTIFIED BY %s", mysqlUserLiteral(UserRef{Name: in.Name, Host: in.Host}), mysqlPasswordLiteral(in.Password))
	} else {
		stmt = fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD %s`, mustQuote(EnginePostgres, in.Name), pgPasswordLiteral(in.Password))
	}
	if _, err := db.ExecContext(qctx, stmt); err != nil {
		return humanQueryError(err)
	}
	return nil
}

// SetUserPassword 改密。
func (s *service) SetUserPassword(ctx context.Context, id string, in UserInput) error {
	inst, err := s.userCtx(ctx, id)
	if err != nil {
		return err
	}
	if err := validateUserRef(inst.Engine, UserRef{Name: in.Name, Host: in.Host}); err != nil {
		return err
	}
	if in.Password == "" {
		return fmt.Errorf("%w: password", ErrInvalidInput)
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	db, err := s.connectAnyDB(qctx, inst, connOpts{})
	if err != nil {
		return err
	}
	defer db.Close()
	var stmt string
	if inst.Engine == EngineMySQL {
		stmt = fmt.Sprintf("ALTER USER %s IDENTIFIED BY %s", mysqlUserLiteral(UserRef{Name: in.Name, Host: in.Host}), mysqlPasswordLiteral(in.Password))
	} else {
		stmt = fmt.Sprintf(`ALTER ROLE %s PASSWORD %s`, mustQuote(EnginePostgres, in.Name), pgPasswordLiteral(in.Password))
	}
	if _, err := db.ExecContext(qctx, stmt); err != nil {
		return humanQueryError(err)
	}
	return nil
}

// DropUser 删账号。
func (s *service) DropUser(ctx context.Context, id string, ref UserRef) error {
	inst, err := s.userCtx(ctx, id)
	if err != nil {
		return err
	}
	if err := validateUserRef(inst.Engine, ref); err != nil {
		return err
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	db, err := s.connectAnyDB(qctx, inst, connOpts{})
	if err != nil {
		return err
	}
	defer db.Close()
	var stmt string
	if inst.Engine == EngineMySQL {
		stmt = fmt.Sprintf("DROP USER %s", mysqlUserLiteral(ref))
	} else {
		stmt = fmt.Sprintf(`DROP ROLE %s`, mustQuote(EnginePostgres, ref.Name))
	}
	if _, err := db.ExecContext(qctx, stmt); err != nil {
		return humanQueryError(err)
	}
	return nil
}

// UserGrants 展示账号授权(MySQL: SHOW GRANTS;PG: 库级 CONNECT 矩阵)。
func (s *service) UserGrants(ctx context.Context, id string, ref UserRef) (*GrantsView, error) {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := validateUserRef(inst.Engine, ref); err != nil {
		return nil, err
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	db, err := s.connectAnyDB(qctx, inst, connOpts{})
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if inst.Engine == EngineMySQL {
		rows, err := db.QueryContext(qctx, fmt.Sprintf("SHOW GRANTS FOR %s", mysqlUserLiteral(ref)))
		if err != nil {
			return nil, humanQueryError(err)
		}
		defer rows.Close()
		out := &GrantsView{Engine: inst.Engine, Lines: []string{}}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				return nil, humanQueryError(err)
			}
			out.Lines = append(out.Lines, line)
		}
		return out, humanQueryError(rows.Err())
	}
	// PG:库级 CONNECT 矩阵(角色对每个可连接库是否有 CONNECT 权限)。
	rows, err := db.QueryContext(qctx, `
		SELECT d.datname, has_database_privilege($1, d.datname, 'CONNECT')
		FROM pg_database d
		WHERE d.datallowconn AND NOT d.datistemplate
		ORDER BY d.datname`, ref.Name)
	if err != nil {
		return nil, humanQueryError(err)
	}
	defer rows.Close()
	out := &GrantsView{Engine: inst.Engine, Rows: []GrantDBRow{}}
	for rows.Next() {
		var r GrantDBRow
		if err := rows.Scan(&r.Database, &r.Connect); err != nil {
			return nil, humanQueryError(err)
		}
		out.Rows = append(out.Rows, r)
	}
	return out, humanQueryError(rows.Err())
}

// validateGrant 校验授权入参(模板枚举 + 账号 + 库名标识符)。
func validateGrant(engine string, in GrantInput) error {
	if err := validateUserRef(engine, UserRef{Name: in.Name, Host: in.Host}); err != nil {
		return err
	}
	switch in.Template {
	case GrantAll, GrantRead, GrantCRUD:
	default:
		return fmt.Errorf("%w: grant template", ErrInvalidInput)
	}
	if err := validateDatabaseName(in.Database); err != nil {
		return err
	}
	return nil
}

// GrantDatabase 库级授权:MySQL 一条 GRANT;PG 为 CONNECT(任意库可发)+ SCHEMA
// USAGE + 既有表/序列 + 默认权限(未来表)成套发放——SCHEMA/表级语句须连到目标库执行。
func (s *service) GrantDatabase(ctx context.Context, id string, in GrantInput) error {
	inst, err := s.userCtx(ctx, id)
	if err != nil {
		return err
	}
	if err := validateGrant(inst.Engine, in); err != nil {
		return err
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	if inst.Engine == EngineMySQL {
		db, err := s.connectAnyDB(qctx, inst, connOpts{})
		if err != nil {
			return err
		}
		defer db.Close()
		privs := "ALL PRIVILEGES"
		switch in.Template {
		case GrantRead:
			privs = "SELECT"
		case GrantCRUD:
			privs = "SELECT, INSERT, UPDATE, DELETE"
		}
		stmt := fmt.Sprintf("GRANT %s ON %s.* TO %s", privs, mustQuote(EngineMySQL, in.Database), mysqlUserLiteral(UserRef{Name: in.Name, Host: in.Host}))
		if _, err := db.ExecContext(qctx, stmt); err != nil {
			return humanQueryError(err)
		}
		return nil
	}
	// PG:CONNECT 在维护库发(SCHEMA 引用按名全局生效);其余连目标库发。
	maint, err := s.connect(qctx, inst, "postgres", connOpts{})
	if err != nil {
		return err
	}
	defer maint.Close()
	if _, err := maint.ExecContext(qctx,
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", mustQuote(EnginePostgres, in.Database), mustQuote(EnginePostgres, in.Name))); err != nil {
		return humanQueryError(err)
	}
	target, err := s.connect(qctx, inst, in.Database, connOpts{})
	if err != nil {
		return err
	}
	defer target.Close()
	for _, stmt := range pgGrantStatements(in) {
		if _, err := target.ExecContext(qctx, stmt); err != nil {
			return humanQueryError(fmt.Errorf("%s | %w", stmt, err))
		}
	}
	return nil
}

// pgGrantStatements 组 PG 目标库内的成套授权语句(SCHEMA/表级,不含 DATABASE 级)。
// 说明:ALTER DEFAULT PRIVILEGES 只作用于「当前角色未来创建的对象」(部署场景
// 常为 postgres 超级用户,即覆盖典型新建表);既有表用 ON ALL TABLES 兜底。
func pgGrantStatements(in GrantInput) []string {
	role := mustQuote(EnginePostgres, in.Name)
	tablePrivs, seqPrivs := "SELECT", ""
	switch in.Template {
	case GrantCRUD:
		tablePrivs, seqPrivs = "SELECT, INSERT, UPDATE, DELETE", "USAGE, SELECT"
	case GrantAll:
		tablePrivs, seqPrivs = "ALL PRIVILEGES", "ALL PRIVILEGES"
	}
	stmts := []string{
		fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s", role),
		fmt.Sprintf("GRANT %s ON ALL TABLES IN SCHEMA public TO %s", tablePrivs, role),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT %s ON TABLES TO %s", tablePrivs, role),
	}
	if seqPrivs != "" {
		stmts = append(stmts,
			fmt.Sprintf("GRANT %s ON ALL SEQUENCES IN SCHEMA public TO %s", seqPrivs, role),
			fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT %s ON SEQUENCES TO %s", seqPrivs, role),
		)
	}
	if in.Template == GrantAll {
		stmts = append(stmts, fmt.Sprintf("GRANT CREATE ON SCHEMA public TO %s", role))
	}
	return stmts
}

// RevokeDatabase 回收一个库的全部授权。
func (s *service) RevokeDatabase(ctx context.Context, id string, in GrantInput) error {
	inst, err := s.userCtx(ctx, id)
	if err != nil {
		return err
	}
	in.Template = GrantAll // 回收与模板无关:全量回收
	if err := validateGrant(inst.Engine, in); err != nil {
		return err
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	if inst.Engine == EngineMySQL {
		db, err := s.connectAnyDB(qctx, inst, connOpts{})
		if err != nil {
			return err
		}
		defer db.Close()
		stmt := fmt.Sprintf("REVOKE ALL PRIVILEGES ON %s.* FROM %s",
			mustQuote(EngineMySQL, in.Database), mysqlUserLiteral(UserRef{Name: in.Name, Host: in.Host}))
		if _, err := db.ExecContext(qctx, stmt); err != nil {
			return humanQueryError(err)
		}
		return nil
	}
	// PG:DATABASE 级回收在维护库;SCHEMA/表级回收连目标库。
	maint, err := s.connect(qctx, inst, "postgres", connOpts{})
	if err != nil {
		return err
	}
	defer maint.Close()
	if _, err := maint.ExecContext(qctx,
		fmt.Sprintf("REVOKE ALL PRIVILEGES ON DATABASE %s FROM %s", mustQuote(EnginePostgres, in.Database), mustQuote(EnginePostgres, in.Name))); err != nil {
		return humanQueryError(err)
	}
	target, err := s.connect(qctx, inst, in.Database, connOpts{})
	if err != nil {
		return err
	}
	defer target.Close()
	role := mustQuote(EnginePostgres, in.Name)
	for _, stmt := range []string{
		fmt.Sprintf("REVOKE ALL PRIVILEGES ON SCHEMA public FROM %s", role),
		fmt.Sprintf("REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM %s", role),
		fmt.Sprintf("REVOKE ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public FROM %s", role),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL PRIVILEGES ON TABLES FROM %s", role),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL PRIVILEGES ON SEQUENCES FROM %s", role),
	} {
		if _, err := target.ExecContext(qctx, stmt); err != nil {
			return humanQueryError(fmt.Errorf("%s | %w", stmt, err))
		}
	}
	return nil
}
