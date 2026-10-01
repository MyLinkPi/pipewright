package build

import (
	"context"
	"testing"

	"github.com/huangchengsir/pipewright/internal/pipeline"
)

// resolveRegistry 三态:环境显式绑定优先;未绑定回退内置 registry(装配且启用);
// 两者皆无 → nil(旧行为:本地 tag、不推送)。

func TestResolveRegistryEnvBindingWins(t *testing.T) {
	b := &Builder{builtinRegistry: func(context.Context) (string, bool) { return "builtin:5000", true }}
	settings := &pipeline.Settings{Environments: []pipeline.Environment{
		{Name: "prod", ImageRegistry: pipeline.ImageRegistry{Type: "harbor", URL: "https://harbor.corp"}},
	}}
	reg := b.resolveRegistry(context.Background(), settings, "prod")
	if reg == nil || reg.URL != "https://harbor.corp" || reg.Type != "harbor" {
		t.Fatalf("环境绑定应优先: %+v", reg)
	}
}

func TestResolveRegistryFallbackToBuiltin(t *testing.T) {
	b := &Builder{builtinRegistry: func(context.Context) (string, bool) { return "192.168.1.10:5000", true }}
	settings := &pipeline.Settings{Environments: []pipeline.Environment{
		{Name: "prod"}, // 未绑定仓库
	}}
	// 环境存在但未绑定。
	reg := b.resolveRegistry(context.Background(), settings, "prod")
	if reg == nil || reg.URL != "192.168.1.10:5000" || reg.Type != "builtin" || reg.CredentialID != "" {
		t.Fatalf("未绑定应回退内置: %+v", reg)
	}
	// 手动触发(无环境名)同样回退。
	reg = b.resolveRegistry(context.Background(), settings, "")
	if reg == nil || reg.URL != "192.168.1.10:5000" {
		t.Fatalf("无环境名应回退内置: %+v", reg)
	}
}

func TestResolveRegistryBuiltinUnavailableKeepsLegacy(t *testing.T) {
	b := &Builder{builtinRegistry: func(context.Context) (string, bool) { return "", false }}
	settings := &pipeline.Settings{Environments: []pipeline.Environment{{Name: "prod"}}}
	if reg := b.resolveRegistry(context.Background(), settings, "prod"); reg != nil {
		t.Fatalf("内置不可用应保持旧行为(nil): %+v", reg)
	}
}

func TestResolveRegistryNoBuiltinInjected(t *testing.T) {
	b := &Builder{} // 未注入回调(向后兼容:旧装配/旧测试)
	settings := &pipeline.Settings{Environments: []pipeline.Environment{{Name: "prod"}}}
	if reg := b.resolveRegistry(context.Background(), settings, "prod"); reg != nil {
		t.Fatalf("未装配回调应保持旧行为(nil): %+v", reg)
	}
}
