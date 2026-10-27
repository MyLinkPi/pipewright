package main

import (
	"testing"

	"github.com/huangchengsir/pipewright/internal/mesh/easytier"
)

// TestCredentialProviderGating 验证凭据签发器的对外暴露条件:外部模式的 easytier
// 由运维自跑(平台无法传 --credential-file、也无法保证重启后凭据仍在),必须返回
// nil 让端点 503;托管模式返回实例;未就绪(未构造)同样 nil。
func TestCredentialProviderGating(t *testing.T) {
	provider := easytier.NewCredentialProvider("easytier-cli", "127.0.0.1:15888")

	if got := (&meshRuntime{credentials: provider, external: true}).CredentialProvider(); got != nil {
		t.Fatal("外部模式应返回 nil(签发端点在外部模式 503)")
	}
	if got := (&meshRuntime{credentials: provider}).CredentialProvider(); got == nil {
		t.Fatal("托管模式应返回签发器")
	}
	if got := (&meshRuntime{external: true}).CredentialProvider(); got != nil {
		t.Fatal("未就绪应返回 nil")
	}
}
