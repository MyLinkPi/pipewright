package build

// tempsweep.go 是「本机孤儿临时文件清扫」(磁盘治理):构建的临时工作区/归档都靠进程内
// defer 或 TTL 定时器删除,进程被 SIGKILL/断电后全部成为孤儿,永久占盘。本文件提供一次性
// 扫描删除(启动时 + 维护入口手动触发),按**前缀 + 年龄门**判定孤儿:
//
//   - 前缀:只扫 pipewright 各构建路径自己创建的命名前缀(见 orphanPrefixes),绝不碰其它文件;
//   - 年龄门:仅删 mtime 早于 olderThan 的条目。活跃构建的现场 mtime 持续更新,天然不受
//     影响;调用方应给 48h 一类的保守值,兼顾「mesh 暂存 tar 的 TTL 随进程蒸发」场景。
//
// 删除失败仅跳过该条(best-effort),不让整个清扫失败。

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// orphanPrefixes 是构建在本机 os.TempDir 下创建的全部临时物前缀(MkdirTemp/CreateTemp 的
// pattern 前缀):工作区、结果暂存、替换备份、镜像归档、mesh 共享暂存(pipewright-shared-
// 覆盖 ws/result 两类)。新增临时物时必须同步登记此处,否则它不受清扫保护。
var orphanPrefixes = []string{
	"pipewright-build-",
	"pipewright-result-",
	"pipewright-previous-",
	"pw-mesh-img-",
	"pipewright-shared-",
}

// SweepLocalOrphans 删除 dir 下匹配构建前缀且 mtime 超过 olderThan 的孤儿文件/目录,
// 返回删除条目数与回收字节数。目录大小按递归统计(删除前),统计失败按 0 计。
func SweepLocalOrphans(dir string, olderThan time.Duration) (int, int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, err
	}
	cutoff := time.Now().Add(-olderThan)
	var removed int
	var bytes int64
	for _, e := range entries {
		name := e.Name()
		matched := false
		for _, p := range orphanPrefixes {
			if strings.HasPrefix(name, p) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		full := filepath.Join(dir, name)
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue // 消失了或未到年龄门:跳过
		}
		size := int64(0)
		if e.IsDir() {
			size = dirSize(full)
		} else {
			size = info.Size()
		}
		if err := os.RemoveAll(full); err != nil {
			continue // best-effort:跳过失败条目
		}
		removed++
		bytes += size
	}
	return removed, bytes, nil
}
