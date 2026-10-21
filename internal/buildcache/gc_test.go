package buildcache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 直接在分片目录造一个归档条目(绕过 Save,便于指定 mtime)。
func seedArchive(t *testing.T, root, key string, size int, age time.Duration) string {
	t.Helper()
	path := (&Store{root: root}).pathFor(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir shard: %v", err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatalf("seed archive: %v", err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return path
}

// 超限时按 mtime 从最旧开始淘汰,直到回到上限内。
func TestGCEvictsOldestBeyondCap(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	old1 := seedArchive(t, root, "k1", 400, 72*time.Hour)
	old2 := seedArchive(t, root, "k2", 400, 48*time.Hour)
	fresh := seedArchive(t, root, "k3", 400, time.Hour)

	// 上限 1000:三个共 1200 → 最旧的 k1(72h)被淘汰,k2/k3 保留。
	deleted, reclaimed, err := s.GC(1000, time.Hour)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if len(deleted) != 1 || reclaimed != 400 {
		t.Fatalf("应淘汰最旧 1 个(400B),实际 deleted=%v reclaimed=%d", deleted, reclaimed)
	}
	if len(deleted) != 1 || deleted[0] != old1 {
		t.Fatalf("应删除最旧的 %s,实际:%v", old1, deleted)
	}
	if _, err := os.Stat(old1); !os.IsNotExist(err) {
		t.Fatalf("最旧条目应被删除:%v", err)
	}
	for _, keep := range []string{old2, fresh} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("不应删除 %s: %v", keep, err)
		}
	}
}

// .save-* 半截文件按年龄门清理:超龄删、在途(新鲜)保留;maxBytes<=0 时也只清残留。
func TestGCCleansStaleSaveTempsOnly(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	shard := filepath.Join(root, "ab")
	if err := os.MkdirAll(shard, 0o700); err != nil {
		t.Fatalf("mkdir shard: %v", err)
	}
	stale := filepath.Join(shard, ".save-stale")
	if err := os.WriteFile(stale, []byte("half"), 0o600); err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(stale, old, old)
	inflight := filepath.Join(shard, ".save-fresh")
	if err := os.WriteFile(inflight, []byte("writing"), 0o600); err != nil {
		t.Fatalf("seed inflight: %v", err)
	}

	deleted, _, err := s.GC(0, time.Hour) // 0=不限总量,仅清残留
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != stale {
		t.Fatalf("应只清超龄 .save-*,实际:%v", deleted)
	}
	if _, err := os.Stat(inflight); err != nil {
		t.Fatalf("在途 .save-* 不应被删: %v", err)
	}
}

// Restore 命中应刷新 mtime(LRU 语义:最近使用优先保留)。
func TestRestoreRefreshesMTime(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	if err := os.MkdirAll(filepath.Join(ws, "node_modules", "pkg"), 0o755); err != nil {
		t.Fatalf("ws: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "node_modules", "pkg", "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s0(dir).Save(t.Context(), "k", []string{"node_modules"}, ws); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path := (&Store{root: dir}).pathFor("k")
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	target := filepath.Join(dir, "ws2")
	hit, err := s0(dir).Restore(t.Context(), "k", nil, target)
	if err != nil || !hit {
		t.Fatalf("Restore hit=%v err=%v", hit, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if time.Since(fi.ModTime()) > time.Minute {
		t.Fatalf("Restore 命中后 mtime 应被刷新,实际 %s", fi.ModTime())
	}
}

func s0(root string) *Store { return &Store{root: root} }
