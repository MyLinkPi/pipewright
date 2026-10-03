//go:build linux || darwin

package srcupdate

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"slices"
	"testing"
)

// ownerEnvPatch:降权时改写身份环境(HOME/USER/LOGNAME),缺家目录不生成。
func TestOwnerEnvPatch(t *testing.T) {
	got := ownerEnvPatch(&user.User{Username: "deploy", HomeDir: "/home/deploy"})
	want := []string{"HOME=/home/deploy", "USER=deploy", "LOGNAME=deploy"}
	if !slices.Equal(got, want) {
		t.Errorf("ownerEnvPatch = %v,期望 %v", got, want)
	}
	if got := ownerEnvPatch(&user.User{Username: "x", HomeDir: ""}); got != nil {
		t.Errorf("无家目录应返回 nil,得 %v", got)
	}
	if got := ownerEnvPatch(nil); got != nil {
		t.Errorf("nil 用户应返回 nil,得 %v", got)
	}
}

// stream/streamSelf 的身份分流:root 运行 + 仓库属普通用户时,stream 应降权到属主
// (git/make 要用属主的 SSH 凭据与家目录),streamSelf 应保持 root(install.sh 要写
// /usr/local/bin 并 systemctl restart;降权后回退 sudo,而服务无终端,升级必败)。
// 非 root 运行时 applyOwnerCred 本就不降权,无从分辨,跳过。
func TestStreamSelfKeepsIdentity(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh 不可用,跳过")
	}
	if os.Geteuid() != 0 {
		t.Skip("仅 root 运行时可验证降权分流")
	}
	const nobody = 65534 // Linux 的 nobody;其他系统 chown 失败即跳过
	dir := t.TempDir()
	if err := os.Chown(dir, nobody, nobody); err != nil {
		t.Skipf("无法把临时目录交给非 root 属主: %v", err)
	}
	s := New(dir)
	j := &jobT{running: true}
	whoami := []string{"sh", "-c", "id -u"}
	if err := s.stream(context.Background(), j, nil, whoami); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if err := s.streamSelf(context.Background(), j, nil, whoami); err != nil {
		t.Fatalf("streamSelf: %v", err)
	}
	if got := j.log; len(got) != 2 || got[0] != "65534" || got[1] != "0" {
		t.Errorf("stream 应降权到属主 65534、streamSelf 应保持 root 0,得 %q", got)
	}
}
