//go:build linux || darwin

package srcupdate

import (
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
