//go:build linux || darwin

package srcupdate

import (
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
)

// applyOwnerCred 让 git/make 子进程以仓库属主的 uid/gid 运行(仅当平台以 root 运行且仓库
// 属普通用户)。这是 root 服务 + 用户克隆仓库场景的解药:root 的 ~/.ssh 里既没有克隆用的
// 密钥、也没有服务端主机指纹(非交互 ssh 又无法确认),直接拉必然
// "Host key verification failed" / "Permission denied (publickey)"。降权到属主后,
// ssh/git 自然使用属主的家目录(密钥、known_hosts、config、全局 gitconfig),与当初
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
	u, err := user.LookupId(strconv.FormatUint(uint64(st.Uid), 10))
	if err != nil {
		return
	}
	// 降权必须连身份环境一起降:子进程继承的是 root 的环境(HOME=/root),npm/ssh 等按
	// $HOME 找缓存与凭据 —— uid 1000 写 /root/.npm 必然 EACCES("Your cache folder
	// contains root-owned files")。Go 的 exec 对 Env 重复键保留最后一个(Go 1.19+),
	// 故追加即覆盖。
	cmd.Env = append(cmd.Env, ownerEnvPatch(u)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: st.Uid, Gid: st.Gid}}
}

// ownerEnvPatch 生成属主身份的环境补丁(HOME/USER/LOGNAME)。独立成纯函数便于测试。
func ownerEnvPatch(u *user.User) []string {
	if u == nil || u.HomeDir == "" {
		return nil
	}
	patch := []string{"HOME=" + u.HomeDir}
	if u.Username != "" {
		patch = append(patch, "USER="+u.Username, "LOGNAME="+u.Username)
	}
	return patch
}
