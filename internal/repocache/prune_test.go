package repocache

import (
	"os"
	"path/filepath"
	"testing"
)

// Prune 删除不在保留集内的镜像;保留集按 URL 同款 hash 规则计算(多项目同 URL 一份)。
func TestPruneRemovesUnreferencedMirrors(t *testing.T) {
	root := t.TempDir()
	c, err := New(root, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// 两个在用 URL + 一个无引用 URL。
	keepURLs := []string{"https://example.com/a.git", "https://example.com/b.git"}
	goneURL := "https://example.com/old.git"
	for _, u := range append(append([]string{}, keepURLs...), goneURL) {
		mirror := c.mirrorPath(u)
		if err := os.MkdirAll(filepath.Join(mirror, "refs"), 0o755); err != nil {
			t.Fatalf("seed mirror: %v", err)
		}
		if err := os.WriteFile(filepath.Join(mirror, "HEAD"), []byte("ref"), 0o644); err != nil {
			t.Fatalf("seed HEAD: %v", err)
		}
	}
	// 非镜像目录(杂物)不碰。
	if err := os.MkdirAll(filepath.Join(root, "not-a-mirror.txt"), 0o755); err != nil {
		t.Fatalf("seed junk: %v", err)
	}

	deleted, reclaimed, err := c.Prune(keepURLs)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(deleted) != 1 || reclaimed <= 0 {
		t.Fatalf("应删 1 个无引用镜像并统计字节,实际 deleted=%v reclaimed=%d", deleted, reclaimed)
	}
	if len(deleted) != 1 || deleted[0] != c.mirrorPath(goneURL) {
		t.Fatalf("删除清单应是那个无引用镜像,实际:%v", deleted)
	}
	if _, err := os.Stat(c.mirrorPath(goneURL)); !os.IsNotExist(err) {
		t.Fatalf("无引用镜像应被删除:%v", err)
	}
	for _, u := range keepURLs {
		if _, err := os.Stat(c.mirrorPath(u)); err != nil {
			t.Fatalf("在用镜像不应删除(%s): %v", u, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "not-a-mirror.txt")); err != nil {
		t.Fatalf("非镜像目录不应删除: %v", err)
	}
}

// Remove 幂等:不存在也成功;删除后目录消失。
func TestRemoveIdempotent(t *testing.T) {
	root := t.TempDir()
	c, err := New(root, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	u := "https://example.com/x.git"
	mirror := c.mirrorPath(u)
	if err := os.MkdirAll(mirror, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := c.Remove(u); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(mirror); !os.IsNotExist(err) {
		t.Fatalf("镜像应已删除")
	}
	if err := c.Remove(u); err != nil {
		t.Fatalf("重复 Remove 应幂等: %v", err)
	}
}
