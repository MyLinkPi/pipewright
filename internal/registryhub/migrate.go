package registryhub

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// 本文件是存储目录收敛的纯 FS 辅助:整目录迁移(rename 优先、跨设备退化为 copy+delete)
// 与已落盘 compose 的卷源解析(上一部署目录的唯一事实来源)。

// moveDir 把 src 整体移动为 dst:同设备 rename 一步到位;跨设备(EXDEV 等)退化为
// copyTree + 删除旧目录。dst 的父目录须已存在,且 dst 本身不存在或为空目录。
func moveDir(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyTree(src, dst); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

// copyTree 递归复制 src 目录树到 dst(保留权限位;符号链接按其指向的内容落成普通文件,
// registry 存储内部不使用符号链接,此简化无影响)。
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

// parseComposeDataDirs 从已落盘 compose 提取上一部署的 [制品, 缓存] 存储目录。
// 两个服务都挂 /var/lib/registry,按出现顺序区分(渲染方 renderCompose 恒制品在前);
// 兼容带/不带引号的卷行与 Windows 盘符路径(取最后一个 ":/var/lib/registry" 前缀)。
// 解析不出两个卷行 → ok=false(视为无上一部署可依据)。
func parseComposeDataDirs(raw string) (artifact, cache string) {
	var dirs []string
	for _, line := range strings.Split(raw, "\n") {
		item := strings.TrimSpace(line)
		if !strings.HasPrefix(item, "- ") {
			continue
		}
		item = strings.TrimSpace(strings.TrimPrefix(item, "- "))
		const suffix = ":/var/lib/registry"
		i := strings.LastIndex(item, suffix)
		if i <= 0 {
			continue
		}
		src := strings.Trim(item[:i], `"`)
		if src == "" {
			continue
		}
		dirs = append(dirs, src)
		if len(dirs) == 2 {
			break
		}
	}
	if len(dirs) < 2 {
		return "", ""
	}
	return dirs[0], dirs[1]
}
