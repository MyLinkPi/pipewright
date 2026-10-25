package dbbackup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/dbadmin"
	"github.com/huangchengsir/pipewright/internal/redisadmin"
	"github.com/huangchengsir/pipewright/internal/storetest"
)

// newTestServiceWithRows 开临时库,直插六个实例行(external/managed × mysql/pg/redis)
// 并返回真实骨架服务(SSH/协议路径不触网;测试只覆盖骨架面与纯函数)。
func newTestServiceWithRows(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	db := storetest.OpenDB(t)
	t.Cleanup(func() { _ = db.Close() })
	now := "2026-01-01T00:00:00Z"

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, query)
		}
	}
	exec(`INSERT INTO db_instances (id, name, engine, host, port, username, password_sealed, tls, readonly, source, server_id, stack_name, created_at, updated_at)
		VALUES ('mysql-ext', 'm1', 'mysql', '10.0.0.1', 3306, 'root', x'00', '', 1, 'manual', '', '', ?, ?)`, now, now)
	exec(`INSERT INTO db_instances (id, name, engine, host, port, username, password_sealed, tls, readonly, source, server_id, stack_name, created_at, updated_at)
		VALUES ('mysql-managed', 'm2', 'mysql', '10.0.0.2', 3306, 'root', x'00', '', 1, 'appstore', 'srv1', 'db-m2', ?, ?)`, now, now)
	exec(`INSERT INTO db_instances (id, name, engine, host, port, username, password_sealed, tls, readonly, source, server_id, stack_name, created_at, updated_at)
		VALUES ('pg-ext', 'p1', 'postgres', '10.0.0.3', 5432, 'postgres', x'00', '', 1, 'manual', '', '', ?, ?)`, now, now)
	exec(`INSERT INTO db_instances (id, name, engine, host, port, username, password_sealed, tls, readonly, source, server_id, stack_name, created_at, updated_at)
		VALUES ('pg-managed', 'p2', 'postgres', '10.0.0.4', 5432, 'postgres', x'00', '', 1, 'appstore', 'srv2', 'db-p2', ?, ?)`, now, now)
	exec(`INSERT INTO redis_instances (id, name, host, port, password_sealed, db_num, readonly, source, server_id, stack_name, created_at, updated_at)
		VALUES ('redis-ext', 'r1', '10.0.0.5', 6379, x'00', 0, 1, 'manual', '', '', ?, ?)`, now, now)
	exec(`INSERT INTO redis_instances (id, name, host, port, password_sealed, db_num, readonly, source, server_id, stack_name, created_at, updated_at)
		VALUES ('redis-managed', 'r2', '10.0.0.6', 6379, x'00', 0, 1, 'appstore', 'srv3', 'db-r2', ?, ?)`, now, now)

	svc := New(db, dbadmin.New(db, nil), redisadmin.New(db, nil), nil, Options{DataDir: t.TempDir()})
	return svc, db
}

// kindOf 由测试实例 id 前缀推类别。
func kindOf(id string) string {
	switch {
	case strings.HasPrefix(id, "mysql"):
		return KindMySQL
	case strings.HasPrefix(id, "pg"):
		return KindPostgres
	default:
		return KindRedis
	}
}

func TestCapabilityMatrix(t *testing.T) {
	svc, _ := newTestServiceWithRows(t)
	ctx := context.Background()

	cases := []struct {
		id         string
		wantMode   string
		wantBackup bool
		wantNote   bool
	}{
		{"mysql-ext", "protocol", true, false},
		{"mysql-managed", "protocol", true, false},
		{"pg-ext", "protocol", true, true},
		{"pg-managed", "ssh", true, false},
		{"redis-ext", "none", false, true},
		{"redis-managed", "ssh", true, false},
	}
	for _, c := range cases {
		capability, err := svc.Capability(ctx, kindOf(c.id), c.id)
		if err != nil {
			t.Fatalf("%s: %v", c.id, err)
		}
		if capability.Mode != c.wantMode || capability.Backupable != c.wantBackup || capability.Restoreable != c.wantBackup || (capability.Note != "") != c.wantNote {
			t.Fatalf("%s: got %+v want mode=%s backup=%v note=%v", c.id, capability, c.wantMode, c.wantBackup, c.wantNote)
		}
	}
	// 外部 Redis 不能备份(RunBackup 应拒绝)。
	if _, err := svc.RunBackup(ctx, KindRedis, "redis-ext", "manual"); err == nil {
		t.Fatal("external redis backup should be rejected")
	}
}

func TestConfigPersistenceAndRetention(t *testing.T) {
	svc, db := newTestServiceWithRows(t)
	ctx := context.Background()

	if err := svc.SetConfig(ctx, KindMySQL, "mysql-ext", Config{IntervalHours: 24, Retention: 3, AutoUploadProviderID: " p1 "}); err != nil {
		t.Fatal(err)
	}
	cfg, err := svc.GetConfig(ctx, KindMySQL, "mysql-ext")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IntervalHours != 24 || cfg.Retention != 3 || cfg.AutoUploadProviderID != "p1" {
		t.Fatalf("config roundtrip mismatch: %+v", cfg)
	}
	if err := svc.SetConfig(ctx, KindMySQL, "mysql-ext", Config{IntervalHours: -1}); err == nil {
		t.Fatal("negative interval should be rejected")
	}
	if err := svc.SetConfig(ctx, KindMySQL, "missing", Config{}); err == nil {
		t.Fatal("unknown instance should be rejected")
	}
	if err := svc.TouchLastRun(ctx, KindMySQL, "mysql-ext"); err != nil {
		t.Fatal(err)
	}
	if cfg, _ = svc.GetConfig(ctx, KindMySQL, "mysql-ext"); cfg.LastBackupAt == "" {
		t.Fatal("last_backup_at should be set")
	}

	// 保留裁剪:4 条 completed,保留 3 → 最老 1 条被删(注意 db_instances 里 mysql-ext
	// 与 mysql-managed 是不同实例,记录只挂在 mysql-ext)。
	for i := 0; i < 4; i++ {
		if _, err := db.Exec(
			`INSERT INTO db_backup_records (id, instance_kind, instance_id, file_name, status, created_at) VALUES (?, 'mysql', 'mysql-ext', ?, 'completed', ?)`,
			"r"+string(rune('a'+i)), "f"+string(rune('a'+i))+".sql", string(rune('0'+i))+":00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.applyRetention(ctx, KindMySQL, "mysql-ext", 3); err != nil {
		t.Fatal(err)
	}
	list, err := svc.List(ctx, KindMySQL, "mysql-ext")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("retention should keep 3, got %d", len(list))
	}
	if list[0].ID != "rd" || list[2].ID != "rb" {
		t.Fatalf("oldest should be pruned, got newest=%s oldest=%s", list[0].ID, list[2].ID)
	}

	if _, err := svc.Get(ctx, "nope"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := svc.DeleteForInstance(ctx, KindMySQL, "mysql-ext"); err != nil {
		t.Fatal(err)
	}
	if list, _ := svc.List(ctx, KindMySQL, "mysql-ext"); len(list) != 0 {
		t.Fatal("DeleteForInstance should remove all records")
	}
}

func TestSanitizeFileName(t *testing.T) {
	out := sanitizeFileName("订单库/main")
	if out == "" {
		t.Fatal("must not return empty")
	}
	for _, c := range out {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			t.Fatalf("unsafe char %q in output %q", c, out)
		}
	}
}

func TestRenderPGPure(t *testing.T) {
	if got := renderPGColumn(pgColumn{Name: `o"dd`, Type: "integer", NotNull: true, HasDef: true, Default: "nextval('s')"}); got != `"o""dd" integer DEFAULT nextval('s') NOT NULL` {
		t.Fatalf("renderPGColumn: %s", got)
	}
	if got := pgQuoteString("it's"); got != "'it''s'" {
		t.Fatalf("pgQuoteString: %s", got)
	}
	if got := renderPGValue(nil); got != "NULL" {
		t.Fatalf("nil: %s", got)
	}
	if got := renderPGValue([]byte{0x0a, 0xff}); got != `E'\x0aff'` {
		t.Fatalf("bytes: %s", got)
	}
	if got := renderPGValue(true); got != "true" {
		t.Fatalf("bool: %s", got)
	}
	if got := renderPGValue("x\\y"); got != `'x\y'` {
		t.Fatalf("string keeps backslash raw (no escape): %s", got)
	}
	if got := pgQuoteIdent(`a"b`); got != `"a""b"` {
		t.Fatalf("pgQuoteIdent: %s", got)
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func TestPGMarkers(t *testing.T) {
	if got := pwdbName("-- PWDB: appdb"); got != "appdb" {
		t.Fatalf("pwdbName: %q", got)
	}
	if got := pwdbName("INSERT INTO t VALUES (1);"); got != "" {
		t.Fatalf("pwdbName non-marker: %q", got)
	}
	p := filepath.Join(t.TempDir(), "dump.sql")
	if err := writeFile(p, "SET x;\n\\connect appdb\n"); err != nil {
		t.Fatal(err)
	}
	if !pgNeedsPsql(p) {
		t.Fatal("pg_dumpall dump should be detected")
	}
	if err := writeFile(p, "-- PWDB: appdb\nCREATE TABLE t();\n"); err != nil {
		t.Fatal(err)
	}
	if pgNeedsPsql(p) {
		t.Fatal("protocol dump should not need psql")
	}
}

func TestSweeperScanDue(t *testing.T) {
	svc, db := newTestServiceWithRows(t)
	sw := NewSweeper(svc, time.Minute)
	ctx := context.Background()

	// 到点:mysql-ext(interval 1h,last 空);未到点:redis-managed(last 刚写)。
	if _, err := db.Exec(`UPDATE db_instances SET backup_interval_hours = 1 WHERE id = 'mysql-ext'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE redis_instances SET backup_interval_hours = 24, last_backup_at = ? WHERE id = 'redis-managed'`,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	due := sw.scanDue(ctx)
	if len(due) != 1 || due[0].kind != KindMySQL || due[0].id != "mysql-ext" {
		t.Fatalf("scanDue mismatch: %+v", due)
	}
}
