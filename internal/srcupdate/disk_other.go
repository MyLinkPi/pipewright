//go:build !linux && !darwin

package srcupdate

import "errors"

// 源码升级仅支持 Linux/macOS;其他平台会在 StartUpdate 的平台守卫处提前返回。
func availableDiskSpace(path string) (uint64, error) {
	return 0, errors.New("当前平台不支持磁盘空间检查")
}
