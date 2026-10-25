package dbaas

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/huangchengsir/pipewright/internal/store"
)

func TestValidateCreate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*CreateInput)
		wantErr bool
	}{
		{"ok", func(in *CreateInput) {}, false},
		{"bad name upper", func(in *CreateInput) { in.Name = "Orders" }, true},
		{"bad name short", func(in *CreateInput) { in.Name = "ab" }, true},
		{"bad engine", func(in *CreateInput) { in.Engine = "mongodb" }, true},
		{"bad version", func(in *CreateInput) { in.Version = "5.7" }, true},
		{"bad port", func(in *CreateInput) { in.Port = 80 }, true},
		{"password with quote", func(in *CreateInput) { in.AdminPassword = `abc"defghijklm` }, true},
		{"password with backslash", func(in *CreateInput) { in.AdminPassword = `abc\defghijkl` }, true},
		{"password ok charset", func(in *CreateInput) { in.AdminPassword = `aZ9_-.~@+=:bcdef` }, false},
		{"bad cpus", func(in *CreateInput) { in.CPUs = "two" }, true},
		{"bad memory", func(in *CreateInput) { in.Memory = "512兆" }, true},
		{"bad interval", func(in *CreateInput) { in.BackupIntervalHours = -1 }, true},
	}
	for _, c := range cases {
		in := CreateInput{Name: "orders", Engine: "mysql", Version: "8.4", ServerID: "s1"}
		c.mutate(&in)
		err := validateCreate(&in)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", c.name, err, c.wantErr)
		}
	}
}

func TestRenderComposeAllEngines(t *testing.T) {
	// 口令含字符集内的全部符号类,验证渲染产物是合法 YAML 且关键片段就位。
	pass := "aZ90_-.~@+=:bcdef"
	cases := []struct {
		engine, version string
		inner           int
		needle          string
	}{
		{"mysql", "8.4", 3306, "MYSQL_ROOT_PASSWORD"},
		{"postgres", "16", 5432, "POSTGRES_PASSWORD"},
		{"redis", "7", 6379, "--requirepass"},
	}
	for _, c := range cases {
		out, err := renderCompose(c.engine, c.version, "orders", 33001, pass)
		if err != nil {
			t.Fatalf("%s: render: %v", c.engine, err)
		}
		// 必须是合法 compose YAML(services 是映射)。
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("%s: invalid yaml: %v\n%s", c.engine, err, out)
		}
		if _, ok := doc["services"].(map[string]any); !ok {
			t.Fatalf("%s: missing services map", c.engine)
		}
		if !strings.Contains(out, c.needle) {
			t.Fatalf("%s: missing %s", c.engine, c.needle)
		}
		want := `"33001:` + itoa(c.inner) + `"`
		if !strings.Contains(out, want) {
			t.Fatalf("%s: port mapping %s missing\n%s", c.engine, want, out)
		}
		if !strings.Contains(out, pass) {
			t.Fatalf("%s: password not injected", c.engine)
		}
		if !strings.Contains(out, "db-orders-data") {
			t.Fatalf("%s: named volume missing", c.engine)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestGeneratePasswordCharset(t *testing.T) {
	for i := 0; i < 50; i++ {
		p, err := generatePassword()
		if err != nil {
			t.Fatal(err)
		}
		if !rePassword.MatchString(p) {
			t.Fatalf("generated password violates safe charset: %q", p)
		}
	}
}

func TestSetScheduleAndList(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	svc := New(st.DB, nil, nil, nil, nil, Options{DataDir: dir})

	ctx := context.Background()
	now := "2026-01-01T00:00:00Z"
	if _, err := st.DB.Exec(
		`INSERT INTO dbaas_instances (id, name, engine, version, server_id, host_port, admin_password_sealed, stack_name, status, backup_interval_hours, backup_retention, created_at, updated_at)
		 VALUES ('i1', 'orders', 'mysql', '8.4', 'srv', 33001, x'00', 'db-orders', 'running', 0, 7, ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetSchedule(ctx, "i1", 24, 3); err != nil {
		t.Fatal(err)
	}
	list, err := svc.List(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].BackupIntervalHours != 24 || list[0].BackupRetention != 3 {
		t.Fatalf("schedule not persisted: %+v", list)
	}
	if !list[0].PasswordConfigured {
		t.Fatal("password_configured should be true for non-empty sealed blob")
	}
	if err := svc.SetSchedule(ctx, "missing", 1, 1); err == nil {
		t.Fatal("expected ErrNotFound")
	}
}

func TestApplyRetentionSQL(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	svc := New(st.DB, nil, nil, nil, nil, Options{DataDir: dir})
	ctx := context.Background()

	inst := &Instance{ID: "i1", Name: "orders", BackupRetention: 2}
	if _, err := st.DB.Exec(
		`INSERT INTO dbaas_instances (id, name, engine, version, server_id, host_port, admin_password_sealed, stack_name, status, created_at, updated_at)
		 VALUES ('i1', 'orders', 'mysql', '8.4', 'srv', 33001, x'00', 'db-orders', 'running', 't', 't')`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := st.DB.Exec(
			`INSERT INTO dbaas_backups (id, instance_id, file_name, status, created_at) VALUES (?, 'i1', ?, 'completed', ?)`,
			"b"+string(rune('a'+i)), "f"+string(rune('a'+i))+".sql", string(rune('0'+i))+":00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	svc.applyRetention(ctx, inst)
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(1) FROM dbaas_backups WHERE instance_id = 'i1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("retention should keep 2, got %d", n)
	}
	// 最老的两条(b0/b1)应被裁掉。
	var cnt int
	if err := st.DB.QueryRow(`SELECT COUNT(1) FROM dbaas_backups WHERE id IN ('b0','b1')`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 0 {
		t.Fatal("oldest backups should be pruned")
	}
}

func TestShqAndShSingle(t *testing.T) {
	if got := shq("plain"); got != `"plain"` {
		t.Fatalf("shq: %s", got)
	}
	if got := shq(`a"b\c$d`); got != `"a\"b\\c\$d"` {
		t.Fatalf("shq escape: %s", got)
	}
	if got := shSingle("it's"); got != `'it'\''s'` {
		t.Fatalf("shSingle: %s", got)
	}
}
