package store

import (
	"database/sql"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// TestMigration0076DropsEnvironmentFeature 验证 0076 迁移:环境功能表/列被 DROP,
// 且存量分支映射的 pattern 被抽取进 branch_filters_json(行为保真:仅映射分支触发的
// 旧语义保留,而非突变为"全分支触发");脏 JSON / 空映射安全退化为 '[]'。
func TestMigration0076DropsEnvironmentFeature(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// 真正的旧库路径:按序重放 0001..0075(不含 0076),模拟 0075 时代的存量库。
	entries, err := fs.Glob(sqliteMigrationFS, "migrations/sqlite/*.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	sort.Strings(entries)
	for _, e := range entries {
		if strings.HasSuffix(e, "0076_drop_environment_feature.sql") {
			continue
		}
		b, err := fs.ReadFile(sqliteMigrationFS, e)
		if err != nil {
			t.Fatalf("read %s: %v", e, err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("replay %s: %v", e, err)
		}
	}

	// 存量数据:三条触发配置(正常映射/空映射/脏 JSON)+ 一条晋级审批残留 + 最小外键行。
	for _, proj := range []string{"p1", "p2", "p3"} {
		if _, err := db.Exec(
			`INSERT INTO credentials (id, name, type, scope, ciphertext, masked_value, created_at, updated_at)
			 VALUES (?, 'c', 'git_token', '', X'00', 'm', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, proj+"-cred"); err != nil {
			t.Fatalf("seed credential %s: %v", proj, err)
		}
		if _, err := db.Exec(
			`INSERT INTO projects (id, name, repo_url, default_branch, credential_id, created_at, updated_at)
			 VALUES (?, 'p', 'https://example.com/p.git', 'main', ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
			proj, proj+"-cred"); err != nil {
			t.Fatalf("seed project %s: %v", proj, err)
		}
	}
	seed := []struct{ proj, mappings string }{
		// 正常:两条映射(其中一条模式重复)→ 去重抽取。
		{"p1", `[{"id":"m1","branchPattern":"main","environment":"prod","targetServerIds":[]},{"id":"m2","branchPattern":"release/*","environment":"staging"},{"id":"m3","branchPattern":"main","environment":"prod"}]`},
		// 空:无映射 → '[]'。
		{"p2", `[]`},
		// 脏 JSON:安全退化 '[]'。
		{"p3", `{broken`},
	}
	for _, r := range seed {
		if _, err := db.Exec(
			`INSERT INTO pipeline_triggers (project_id, webhook_token, webhook_secret_ciphertext, events_json, branch_mappings_json, unmatched_policy, created_at, updated_at)
			 VALUES (?, ?, X'00', '{}', ?, 'ignore', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
			r.proj, r.proj+"-tok", r.mappings); err != nil {
			t.Fatalf("seed trigger %s: %v", r.proj, err)
		}
	}
	if _, err := db.Exec(
		`INSERT INTO pipeline_runs (id, project_id, status, trigger_type, trigger_branch, trigger_commit, trigger_actor, resolved_environment, created_at)
		 VALUES ('r1', 'p1', 'success', 'webhook', 'main', 'abc', 'ci', 'prod', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO run_approvals (id, run_id, stage_id, status, created_at) VALUES ('a1', 'r1', 'promote:prod', 'waiting', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed approval: %v", err)
	}

	// 应用 0076。
	sqlText, err := sqliteMigrationFS.ReadFile("migrations/sqlite/0076_drop_environment_feature.sql")
	if err != nil {
		t.Fatalf("read 0076: %v", err)
	}
	if _, err := db.Exec(string(sqlText)); err != nil {
		t.Fatalf("apply 0076 on legacy db: %v", err)
	}

	// 表已删。
	for _, tbl := range []string{"project_environments", "environment_variables", "run_promotions", "preview_configs", "preview_envs"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&n); err != nil {
			t.Fatalf("check table %s: %v", tbl, err)
		}
		if n != 0 {
			t.Fatalf("table %s should be dropped", tbl)
		}
	}
	// 晋级审批残留清理。
	var n int
	if err := db.QueryRow(`SELECT COUNT(1) FROM run_approvals WHERE stage_id LIKE 'promote:%'`).Scan(&n); err != nil {
		t.Fatalf("count approvals: %v", err)
	}
	if n != 0 {
		t.Fatalf("promote approvals should be cleaned, got %d", n)
	}
	// 分支过滤转换:pattern 去重抽取;空/脏 → '[]'。
	cases := map[string]string{
		"p1": `["main","release/*"]`,
		"p2": `[]`,
		"p3": `[]`,
	}
	for proj, want := range cases {
		var got string
		if err := db.QueryRow(`SELECT branch_filters_json FROM pipeline_triggers WHERE project_id = ?`, proj).Scan(&got); err != nil {
			t.Fatalf("read filters %s: %v", proj, err)
		}
		if got != want {
			t.Fatalf("project %s branch_filters_json = %s, want %s", proj, got, want)
		}
	}
	// 旧列已删(引用旧列应报错)。
	for _, dead := range []string{
		`SELECT branch_mappings_json FROM pipeline_triggers LIMIT 1`,
		`SELECT unmatched_policy FROM pipeline_triggers LIMIT 1`,
		`SELECT resolved_environment FROM pipeline_runs LIMIT 1`,
		`SELECT resolved_target_server_ids FROM pipeline_runs LIMIT 1`,
		`SELECT environments_json FROM pipeline_settings LIMIT 1`,
	} {
		if _, err := db.Exec(dead); err == nil {
			t.Fatalf("legacy column should be dropped: %s", dead)
		}
	}
}

// readFileMigration 读回 sqlite 方言的 0076 迁移文本(测试内嵌 FS 直读)。
func readFileMigration(name string) (string, error) {
	b, err := sqliteMigrationFS.ReadFile("migrations/sqlite/" + name)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
