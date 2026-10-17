// dbadmin_test.go 是数据库管理领域的存取/纯函数回归测试(不依赖真实 MySQL/PG;
// 连接层仅测 DSN/标识符/语句分类等纯函数)。
package dbadmin

import (
	"context"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/vault"
)

func testKey() *[32]byte {
	var k [32]byte
	for i := range k {
		k[i] = byte(i + 7)
	}
	return &k
}

func newTestService(t *testing.T) Service {
	t.Helper()
	st := storetest.Open(t)
	return New(st.DB, vault.New(st.DB, testKey()))
}

func strp(s string) *string { return &s }
func intp(i int) *int       { return &i }

func TestInstanceCRUD(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	created, err := svc.Create(ctx, CreateInput{
		Name: "主库", Engine: EngineMySQL, Host: "10.0.0.8", Port: 3306,
		Username: "root", Password: "s3cret-pw", TLS: "",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ReadOnly != true {
		t.Fatalf("new instance must default to readonly")
	}
	if created.Source != SourceManual {
		t.Fatalf("source = %s, want manual", created.Source)
	}

	// 同引擎同地址重复登记 → ErrEndpointTaken。
	if _, err := svc.Create(ctx, CreateInput{
		Name: "重复", Engine: EngineMySQL, Host: "10.0.0.8", Port: 3306,
		Username: "root", Password: "x",
	}); err != ErrEndpointTaken {
		t.Fatalf("duplicate endpoint err = %v, want ErrEndpointTaken", err)
	}

	got, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "主库" || got.Port != 3306 || got.Username != "root" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	upd, err := svc.Update(ctx, created.ID, UpdateInput{Name: strp("主库-新"), Port: intp(3307), Password: strp("rotated")})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Name != "主库-新" || upd.Port != 3307 {
		t.Fatalf("update mismatch: %+v", upd)
	}

	unlocked, err := svc.SetReadOnly(ctx, created.ID, false)
	if err != nil {
		t.Fatalf("set readonly: %v", err)
	}
	if unlocked.ReadOnly {
		t.Fatalf("unlock failed")
	}
	if _, err := svc.SetReadOnly(ctx, created.ID, true); err != nil {
		t.Fatalf("relock: %v", err)
	}

	list, err := svc.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	if err := svc.Delete(ctx, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Get(ctx, created.ID); err != ErrNotFound {
		t.Fatalf("get after delete err = %v, want ErrNotFound", err)
	}
}

func TestRegisterIdempotentRotate(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	first, err := svc.Register(ctx, RegisterInput{
		Engine: EngineMySQL, Name: "MySQL · srv1", Host: "10.0.0.9", Port: 3306,
		Username: "root", Password: "first-pw", ServerID: "srv-1", StackName: "mysql",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if first.Updated {
		t.Fatalf("first register must insert, not update")
	}
	if first.Instance.Source != SourceAppStore || first.Instance.ReadOnly != true {
		t.Fatalf("register defaults wrong: %+v", first.Instance)
	}

	// 重新部署(密码轮换):同 engine+host+port → 原地更新,不新增条目、不改名。
	second, err := svc.Register(ctx, RegisterInput{
		Engine: EngineMySQL, Name: "MySQL · 忽略", Host: "10.0.0.9", Port: 3306,
		Username: "root", Password: "second-pw", ServerID: "srv-1", StackName: "mysql-a",
	})
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if !second.Updated {
		t.Fatalf("re-register must hit existing row")
	}
	if second.Instance.ID != first.Instance.ID {
		t.Fatalf("re-register created new row")
	}
	if second.Instance.Name != "MySQL · srv1" {
		t.Fatalf("re-register must keep user-visible name, got %q", second.Instance.Name)
	}
	if second.Instance.StackName != "mysql-a" {
		t.Fatalf("re-register must refresh stack name, got %q", second.Instance.StackName)
	}
	list, _ := svc.List(ctx)
	if len(list) != 1 {
		t.Fatalf("register must be idempotent, rows = %d", len(list))
	}
}

func TestValidation(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	cases := []struct {
		name string
		in   CreateInput
		want error
	}{
		{"bad engine", CreateInput{Name: "n", Engine: "oracle", Host: "h", Port: 1, Username: "u", Password: "p"}, ErrInvalidEngine},
		{"empty name", CreateInput{Engine: EngineMySQL, Host: "h", Port: 1, Username: "u", Password: "p"}, ErrEmptyName},
		{"empty host", CreateInput{Name: "n", Engine: EngineMySQL, Port: 1, Username: "u", Password: "p"}, ErrEmptyHost},
		{"bad port", CreateInput{Name: "n", Engine: EngineMySQL, Host: "h", Port: 70000, Username: "u", Password: "p"}, ErrInvalidPort},
		{"empty username", CreateInput{Name: "n", Engine: EngineMySQL, Host: "h", Port: 1, Password: "p"}, ErrEmptyUsername},
		{"empty password", CreateInput{Name: "n", Engine: EngineMySQL, Host: "h", Port: 1, Username: "u"}, ErrEmptyPassword},
	}
	for _, c := range cases {
		if _, err := svc.Create(ctx, c.in); err != c.want {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
}

func TestVaultUnconfigured(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	var noKey vault.Vault = vault.New(st.DB, nil) // 未配置 master key
	svc := New(st.DB, noKey)
	_, err := svc.Create(ctx, CreateInput{Name: "n", Engine: EngineMySQL, Host: "h", Port: 1, Username: "u", Password: "p"})
	if err == nil || !strings.Contains(err.Error(), ErrVaultUnconfigured.Error()) {
		t.Fatalf("create without master key err = %v, want ErrVaultUnconfigured", err)
	}
}

func TestQuoteIdent(t *testing.T) {
	if q, _ := quoteIdent(EngineMySQL, "order"); q != "`order`" {
		t.Fatalf("mysql quote = %s", q)
	}
	if q, _ := quoteIdent(EngineMySQL, "we`ird"); q != "`we``ird`" {
		t.Fatalf("mysql backtick doubling = %s", q)
	}
	if q, _ := quoteIdent(EnginePostgres, `say"hi`); q != `"say""hi"` {
		t.Fatalf("pg dquote doubling = %s", q)
	}
	if _, err := quoteIdent(EngineMySQL, ""); err != ErrInvalidIdentifier {
		t.Fatalf("empty ident err = %v", err)
	}
	if _, err := quoteIdent(EnginePostgres, "a\x00b"); err != ErrInvalidIdentifier {
		t.Fatalf("NUL ident err = %v", err)
	}
}

func TestQuoteTableRef(t *testing.T) {
	if q, _ := quoteTableRef(EngineMySQL, "db1", "t1"); q != "`db1`.`t1`" {
		t.Fatalf("mysql ref = %s", q)
	}
	if q, _ := quoteTableRef(EnginePostgres, "db1", "public.t1"); q != `"public"."t1"` {
		t.Fatalf("pg ref = %s", q)
	}
	if q, _ := quoteTableRef(EnginePostgres, "db1", "t1"); q != `"public"."t1"` {
		t.Fatalf("pg bare table defaults public = %s", q)
	}
}

func TestSelectLeading(t *testing.T) {
	yes := []string{"SELECT 1", "select * from t", "  (select 1)", "SHOW TABLES", "explain select 1", "WITH x AS (SELECT 1) SELECT * FROM x", "DESC t", "VALUES (1)"}
	no := []string{"INSERT INTO t VALUES (1)", "update t set a=1", "DELETE FROM t", "DROP TABLE t", "truncate table t", "CREATE TABLE t (a int)"}
	for _, s := range yes {
		if !selectLeading(s) {
			t.Errorf("%q should classify as select-leading", s)
		}
	}
	for _, s := range no {
		if selectLeading(s) {
			t.Errorf("%q should classify as write", s)
		}
	}
}

func TestSQLLiteral(t *testing.T) {
	if got := sqlLiteral(EngineMySQL, "it's\\"); got != `'it\'s\\'` {
		t.Fatalf("mysql string literal = %s", got)
	}
	if got := sqlLiteral(EnginePostgres, "it's"); got != `'it''s'` {
		t.Fatalf("pg string literal = %s", got)
	}
	if got := sqlLiteral(EnginePostgres, `a\b`); got != `E'a\\b'` {
		t.Fatalf("pg E-literal = %s", got)
	}
	if got := sqlLiteral(EngineMySQL, nil); got != "NULL" {
		t.Fatalf("nil = %s", got)
	}
	if got := sqlLiteral(EngineMySQL, int64(42)); got != "42" {
		t.Fatalf("int = %s", got)
	}
	bin := []byte{0x00, 0xff, 0x10}
	if got := sqlLiteral(EngineMySQL, bin); got != "0x00ff10" {
		t.Fatalf("mysql bin = %s", got)
	}
	if got := sqlLiteral(EnginePostgres, bin); got != `E'\\x00ff10'` {
		t.Fatalf("pg bin = %s", got)
	}
}

func TestValidateDatabaseName(t *testing.T) {
	for _, ok := range []string{"a", "my_db-2", "ABC"} {
		if err := validateDatabaseName(ok); err != nil {
			t.Errorf("%q should pass, got %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a b", "a;b", "`x`", `"x"`, strings.Repeat("x", 64), "a.b", "a'b"} {
		if err := validateDatabaseName(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestMySQLUserLiteral(t *testing.T) {
	if got := mysqlUserLiteral(UserRef{Name: "app", Host: "192.168.%"}); got != `'app'@'192.168.%'` {
		t.Fatalf("user literal = %s", got)
	}
	if got := mysqlUserLiteral(UserRef{Name: "app"}); got != `'app'@'%'` {
		t.Fatalf("default host = %s", got)
	}
	if got := mysqlPasswordLiteral(`p'w\d`); got != `'p''w\\d'` {
		t.Fatalf("password literal = %s", got)
	}
}

func TestWhereClause(t *testing.T) {
	if got := whereClause("age > 3"); got != " WHERE (age > 3)" {
		t.Fatalf("where = %q", got)
	}
	if got := whereClause(""); got != "" {
		t.Fatalf("empty where = %q", got)
	}
	if got := whereClause("note = 'a;b'"); got != " WHERE (note = 'a;b')" {
		t.Fatalf("literal semicolon must pass through, got %q", got)
	}
	if got := whereClause("a\x00b"); got != "" {
		t.Fatalf("NUL condition must degrade to no-filter, got %q", got)
	}
}

func TestPlaceholderSeq(t *testing.T) {
	ph := &pholder{engine: EngineMySQL}
	if ph.next() != "?" || ph.next() != "?" {
		t.Fatalf("mysql placeholders")
	}
	pg := &pholder{engine: EnginePostgres}
	if pg.next() != "$1" || pg.next() != "$2" || pg.next() != "$3" {
		t.Fatalf("pg placeholders")
	}
}
