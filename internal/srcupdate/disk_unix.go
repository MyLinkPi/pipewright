//go:build linux || darwin

package srcupdate

import "golang.org/x/sys/unix"

// availableDiskSpace 返回 path 所在文件系统对普通调用者可用的字节数。
// Bavail 不含为 root 保留的块,与构建降权到仓库属主后的实际可写空间一致。
func availableDiskSpace(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
