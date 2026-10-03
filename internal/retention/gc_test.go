package retention

// gc_test.go 覆盖制品孤儿 GC:有引用保留 / 无引用超宽限期删除 / 宽限期内跳过 /
// metadata 清单句柄(format=files)被正确认作引用。

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/storetest"
)

type memStore struct {
	keys  []string
	mtime map[string]time.Time
}

// GC 用 64 位小写 hex 句柄(与 artifactstore 同口径;短串不算引用)。
const (
	keyA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	keyB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	keyC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	keyD = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

func (m *memStore) Keys() ([]string, error)               { return m.keys, nil }
func (m *memStore) Remove(key string) error               { return nil }
func (m *memStore) ModTime(key string) (time.Time, error) { return m.mtime[key], nil }

func seedGCDB(t *testing.T) *Service {
	return NewService(seedGCDBWithRun(t))
}

// seedGCDBWithRun 建库 + 一条 success run('r1'),满足 run_artifacts 的外键。
func seedGCDBWithRun(t *testing.T) *sql.DB {
	db := storetest.OpenDB(t)
	now := time.Now().UTC().Format(time.RFC3339)
	credID := "gc-cred"
	mustExec(t, db, `INSERT INTO credentials (id, name, type, scope, ciphertext, masked_value, created_at, updated_at)
		VALUES (?, 'c', 'git_token', '', X'00', 'm', ?, ?)`, credID, now, now)
	mustExec(t, db, `INSERT INTO projects (id, name, repo_url, default_branch, credential_id, created_at, updated_at)
		VALUES ('p1', 'acme', 'https://example.com/p.git', 'main', ?, ?, ?)`, credID, now, now)
	mustExec(t, db, `INSERT INTO pipeline_runs (id, project_id, status, trigger_type, created_at)
		VALUES ('r1', 'p1', 'success', 'manual', '2026-01-01T00:00:00Z')`)
	return db
}

func TestGCRemovesOrphansAfterGrace(t *testing.T) {
	svc := seedGCDB(t)
	if _, err := svc.db.Exec(`INSERT INTO run_artifacts (id, run_id, type, name, reference, size_bytes, metadata_json, created_at) VALUES ('a1','r1','dist','d','` + keyA + `',2,'{"stored":true}','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	store := &memStore{
		keys:  []string{keyA, keyB, keyC},
		mtime: map[string]time.Time{keyA: old, keyB: old, keyC: time.Now()},
	}
	removed, err := GC(context.Background(), store, svc.referencedKeys, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// keyA 有引用保留;keyB 无引用且超宽限 → 删;keyC 无引用但在宽限期内 → 保留。
	if removed != 1 {
		t.Fatalf("应删 1 个孤儿 blob, got %d", removed)
	}
}

func TestGCRespectsMetadataManifestRefs(t *testing.T) {
	svc := seedGCDB(t)
	// format=files:首句柄在 reference,其余只在 metadata.files 清单里。
	meta := `{"stored":true,"format":"files","files":[{"path":"a.txt","key":"` + keyD + `","size":1}]}`
	if _, err := svc.db.Exec(`INSERT INTO run_artifacts (id, run_id, type, name, reference, size_bytes, metadata_json, created_at) VALUES ('a1','r1','dist','d','` + keyC + `',2,'` + meta + `','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	store := &memStore{keys: []string{keyC, keyD}, mtime: map[string]time.Time{keyC: old, keyD: old}}
	removed, err := GC(context.Background(), store, svc.referencedKeys, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("清单句柄应被认作引用, 不应删除, got %d", removed)
	}
}

func TestGCBlobStoreDisk(t *testing.T) {
	root := t.TempDir()
	bs := NewBlobStore(root)
	if ks, err := bs.Keys(); err != nil || len(ks) != 0 {
		t.Fatalf("空库应 0 keys: %v", err)
	}
	key := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := os.MkdirAll(filepath.Join(root, key[:2]), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, key[:2], key), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ks, _ := bs.Keys()
	if len(ks) != 1 || ks[0] != key {
		t.Fatalf("应扫到 1 个 blob, got %v", ks)
	}
	if _, err := bs.ModTime(key); err != nil {
		t.Fatalf("ModTime: %v", err)
	}
	if err := bs.Remove(key); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := bs.Remove(key); err != nil { // 幂等
		t.Fatalf("Remove 幂等: %v", err)
	}
}
