package build

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 造一个指定 mtime 的文件,并把从 base 起的每一层目录 mtime 也一并设旧
//(SweepLocalOrphans 只看顶层条目的 mtime,目录必须显式设旧才代表"孤儿目录")。
func touchOld(t *testing.T, base, path string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	old := time.Now().Add(-age)
	rel, err := filepath.Rel(base, path)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	cur := base
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		_ = os.Chtimes(cur, old, old)
	}
}

// 年龄门前缀双闸:只删 pipewright 前缀且超龄的条目;新鲜条目与无关文件绝不动。
func TestSweepLocalOrphansAgeGateAndPrefix(t *testing.T) {
	dir := t.TempDir()
	// 孤儿(超龄 + 前缀命中)→ 删。
	touchOld(t, dir, filepath.Join(dir, "pipewright-build-abc", "ws.txt"), 72*time.Hour)
	touchOld(t, dir, filepath.Join(dir, "pipewright-shared-xyz.tar.gz"), 72*time.Hour)
	touchOld(t, dir, filepath.Join(dir, "pw-mesh-img-1.tar"), 72*time.Hour)
	// 前缀命中但新鲜(活跃构建现场)→ 保留。
	touchOld(t, dir, filepath.Join(dir, "pipewright-build-fresh", "ws.txt"), time.Hour)
	// 超龄但非 pipewright 前缀 → 绝不碰。
	touchOld(t, dir, filepath.Join(dir, "someone-else.tmp"), 72*time.Hour)

	deleted, bytes, err := SweepLocalOrphans(dir, 48*time.Hour)
	if err != nil {
		t.Fatalf("SweepLocalOrphans: %v", err)
	}
	if len(deleted) != 3 {
		t.Fatalf("应删 3 个孤儿条目,实际 %d:%v", len(deleted), deleted)
	}
	if bytes <= 0 {
		t.Fatalf("应统计到回收字节数,实际 %d", bytes)
	}
	for _, d := range deleted {
		if !strings.HasPrefix(d, filepath.Join(dir, "pipewright-")) && !strings.HasPrefix(d, filepath.Join(dir, "pw-mesh-img-")) {
			t.Fatalf("删除清单不应包含无关路径:%s", d)
		}
	}
	for _, keep := range []string{
		filepath.Join(dir, "pipewright-build-fresh", "ws.txt"),
		filepath.Join(dir, "someone-else.tmp"),
	} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("不应删除 %s: %v", keep, err)
		}
	}
}

// 全部条目都在年龄门内 → 零删除(防误删活跃构建)。
func TestSweepLocalOrphansKeepsFreshEntries(t *testing.T) {
	dir := t.TempDir()
	touchOld(t, dir, filepath.Join(dir, "pipewright-build-new"), 2*time.Hour)
	touchOld(t, dir, filepath.Join(dir, "pipewright-previous-new"), 2*time.Hour)

	deleted, _, err := SweepLocalOrphans(dir, 48*time.Hour)
	if err != nil {
		t.Fatalf("SweepLocalOrphans: %v", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("新鲜条目不应被删除,实际删除 %d 个:%v", len(deleted), deleted)
	}
}
