//go:build linux || darwin

package srcupdate

import (
	"os"
	"os/exec"
	"syscall"
)

// applyOwnerCred 让 git 子进程以仓库属主的 uid/gid 运行(仅当平台以 root 运行且仓库属
// 普通用户)。这是 root 服务 + 用户克隆仓库场景的解药:root 的 ~/.ssh 里既没有克隆用的
// 密钥、也没有服务端主机指纹(非交互 ssh 又无法确认),直接拉必然
// "Host key verification failed" / "Permission denied (publickey)"。降权到属主后,
// ssh/git 自然使用属主的 $HOME(密钥、known_hosts、config、全局 gitconfig),与当初
// 克隆完全一致。属主即 root 或非 root 运行时不降权(维持原身份)。
func (s *Service) applyOwnerCred(cmd *exec.Cmd) {
	if os.Geteuid() != 0 {
		return
	}
	fi, err := os.Stat(s.dir)
	if err != nil {
		return
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid == 0 {
		return // 属主即 root:root 自身身份即可(指纹问题由 accept-new 兜底)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: st.Uid, Gid: st.Gid}}
}
