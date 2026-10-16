package build

import (
	"context"
	"testing"
)

// resolveRegistry 两态:内置 registry 回调(装配且启用)→ 回 builtin;否则 nil(旧行为:
// 本地 tag、不推送;push_image 节点仍可用节点级 registryUrl 显式指定仓库)。

func TestResolveRegistryFallbackToBuiltin(t *testing.T) {
	b := &Builder{builtinRegistry: func(context.Context) (string, bool) { return "192.168.1.10:5000", true }}
	reg := b.resolveRegistry(context.Background())
	if reg == nil || reg.URL != "192.168.1.10:5000" || reg.Type != "builtin" || reg.CredentialID != "" {
		t.Fatalf("应回退内置: %+v", reg)
	}
}

func TestResolveRegistryBuiltinUnavailableKeepsLegacy(t *testing.T) {
	b := &Builder{builtinRegistry: func(context.Context) (string, bool) { return "", false }}
	if reg := b.resolveRegistry(context.Background()); reg != nil {
		t.Fatalf("内置不可用应保持旧行为(nil): %+v", reg)
	}
}

func TestResolveRegistryNoBuiltinInjected(t *testing.T) {
	b := &Builder{} // 未注入回调(向后兼容:旧装配/旧测试)
	if reg := b.resolveRegistry(context.Background()); reg != nil {
		t.Fatalf("未装配回调应保持旧行为(nil): %+v", reg)
	}
}
