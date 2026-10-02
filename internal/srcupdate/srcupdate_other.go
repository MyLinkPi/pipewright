//go:build !linux && !darwin

package srcupdate

import (
	"os/exec"
)

// applyOwnerCred 在非 unix 平台为 no-op(Windows 无 uid/降权概念;源码升级本身也仅
// 支持 Linux/macOS,见 StartUpdate 的 unixLike 守卫)。
func (s *Service) applyOwnerCred(cmd *exec.Cmd) {}
