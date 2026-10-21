package repocache

// prune.go 是仓库镜像的回收(磁盘治理):镜像此前只增不减——项目删除后镜像不删、任何历史
// 遗留也无兜底,repos/ 单调增长。两个入口:
//
//   - Remove(repoURL):精确删除单镜像(项目删除且无其它项目共用该 URL 时的即时清理);
//   - Prune(keepURLs):扫 root 删除不在保留集内的全部镜像(定时兜底,覆盖历史遗留)。
//     保留集按 mirrorPath 同款规则(sha256(trimmed url)[:16])计算,多项目共用同一 URL
//     天然只算一份。
//
// 竞态:删除前取该镜像的**同一把** per-repo 锁(Clone 全程持锁),等在途克隆结束再删,
// 不会撕开正在 fetch/checkout 的镜像目录。

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Remove 删除 repoURL 对应的本地镜像(不存在视为成功,幂等)。顺带清锁表条目防增长。
func (c *Cache) Remove(repoURL string) error {
	mirror := c.mirrorPath(repoURL)
	m, _ := c.locks.LoadOrStore(mirror, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	c.locks.Delete(mirror)
	if err := os.RemoveAll(mirror); err != nil {
		return err
	}
	return nil
}

// Prune 删除 root 下不在 keepURLs 内的镜像(裸 *.git 目录),返回删除数与回收字节数。
func (c *Cache) Prune(keepURLs []string) (int, int64, error) {
	keep := make(map[string]struct{}, len(keepURLs))
	for _, u := range keepURLs {
		keep[filepath.Base(c.mirrorPath(u))] = struct{}{}
	}
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return 0, 0, err
	}
	removed, reclaimed := 0, int64(0)
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !strings.HasSuffix(name, ".git") {
			continue
		}
		if _, ok := keep[name]; ok {
			continue
		}
		full := filepath.Join(c.root, name)
		// 与 Clone 同一把锁:等在途克隆结束再删(锁 key 即镜像路径,LoadOrStore 与 repoLock 同款)。
		m, _ := c.locks.LoadOrStore(full, &sync.Mutex{})
		mu := m.(*sync.Mutex)
		mu.Lock()
		size := mirrorSize(full)
		rerr := os.RemoveAll(full)
		mu.Unlock()
		c.locks.Delete(full)
		if rerr == nil {
			removed++
			reclaimed += size
		}
	}
	return removed, reclaimed, nil
}

// mirrorSize 递归统计镜像目录字节数(只用于回报,失败按 0 计)。
func mirrorSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			if info, ierr := d.Info(); ierr == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}
