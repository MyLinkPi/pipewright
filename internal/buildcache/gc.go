package buildcache

// gc.go 是缓存库的总量上限 + LRU 淘汰(磁盘治理,0094):缓存 key = 分支 × lockfile 内容 ×
// 用户模板,依赖演进/分支删除/模板修改都会留下旧 key 归档,此前零回收(无限增长)。
//
// GC 语义:
//   - .save-* 半截临时文件(进程被杀时 Save 的 rename 未达)按年龄门删除(tempMinAge,
//     防误删在途 Save 正在写的临时文件);
//   - maxBytes > 0 时,.tgz 归档按 mtime **从最旧开始**删除,直到总量回到上限内(LRU 语义:
//     Restore 命中会刷新 mtime,Save 的原子 rename 天然刷新——「最近使用」而非仅「最近写入」);
//   - maxBytes <= 0 = 不限总量,只清 .save-* 残留。
//
// best-effort:单条删除失败跳过继续;误删的代价只是下次冷构建,无正确性风险。与在途 Save
// 的竞态由 .save-* 年龄门 + rename 原子性化解(正式条目只会整份出现/整份消失)。

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// GC 按上限与年龄门清理缓存库,返回被删除文件的路径清单(供清理留痕展示)与回收字节数。
func (s *Store) GC(maxBytes int64, tempMinAge time.Duration) ([]string, int64, error) {
	if s == nil {
		return nil, 0, nil
	}
	now := time.Now()
	type archive struct {
		path    string
		size    int64
		modTime time.Time
	}
	var archives []archive
	var total int64
	var deleted []string
	reclaimed := int64(0)

	err := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // 根不可读等:交给调用方看到的最终 err;子目录失败跳过
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		name := d.Name()
		switch {
		case strings.HasPrefix(name, ".save-"):
			if now.Sub(info.ModTime()) >= tempMinAge {
				if os.Remove(path) == nil {
					deleted = append(deleted, path)
					reclaimed += info.Size()
				}
			}
		case strings.HasSuffix(name, ".tgz"):
			archives = append(archives, archive{path: path, size: info.Size(), modTime: info.ModTime()})
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return deleted, reclaimed, err
	}
	if maxBytes <= 0 || total <= maxBytes {
		return deleted, reclaimed, nil
	}
	// 超限:按 mtime 降序(最新先占预算)累积到上限,之后的(更旧)全部淘汰——即 LRU 语义。
	sort.Slice(archives, func(i, j int) bool { return archives[i].modTime.After(archives[j].modTime) })
	var kept int64
	for _, a := range archives {
		kept += a.size
		if kept <= maxBytes {
			continue // 最近使用的条目优先保留
		}
		if os.Remove(a.path) == nil {
			deleted = append(deleted, a.path)
			reclaimed += a.size
		}
	}
	return deleted, reclaimed, nil
}
