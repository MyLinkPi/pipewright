package httpapi

import (
	"strings"
	"testing"
)

// TestInjectComposeLimits 验证资源限制注入:每个 service 获得 deploy.resources.limits;
// 已有 deploy 节保留合并、同名键被覆盖;两者皆空零开销原样返回;非法 YAML 报错。
func TestInjectComposeLimits(t *testing.T) {
	t.Run("空限制原样返回", func(t *testing.T) {
		in := "services:\n  web:\n    image: nginx\n"
		out, err := injectComposeLimits(in, "", "")
		if err != nil || out != in {
			t.Fatalf("空限制应原样返回: out=%q err=%v", out, err)
		}
	})

	t.Run("无 deploy 节注入新键", func(t *testing.T) {
		out, err := injectComposeLimits("services:\n  web:\n    image: nginx\n  db:\n    image: mysql:8\n", "1.5", "512m")
		if err != nil {
			t.Fatalf("inject: %v", err)
		}
		for _, want := range []string{"cpus", "1.5", "memory", "512m", "resources", "limits", "deploy"} {
			if !strings.Contains(out, want) {
				t.Fatalf("注入结果缺 %q:\n%s", want, out)
			}
		}
		// 两个 service 都应被注入。
		if strings.Count(out, "limits:") != 2 {
			t.Fatalf("应为每个 service 注入 limits:\n%s", out)
		}
	})

	t.Run("已有 deploy 节合并且同名键被覆盖", func(t *testing.T) {
		in := "services:\n  web:\n    image: nginx\n    deploy:\n      replicas: 2\n      resources:\n        limits:\n          cpus: '9'\n"
		out, err := injectComposeLimits(in, "1", "")
		if err != nil {
			t.Fatalf("inject: %v", err)
		}
		if !strings.Contains(out, "replicas: 2") {
			t.Fatalf("已有 deploy 键应保留:\n%s", out)
		}
		if !strings.Contains(out, "cpus: \"1\"") && !strings.Contains(out, "cpus: '1'") && !strings.Contains(out, "cpus: 1\n") {
			t.Fatalf("cpus 应被覆盖为 1:\n%s", out)
		}
		if strings.Contains(out, "memory") {
			t.Fatalf("未给 memory 不应注入:\n%s", out)
		}
	})

	t.Run("非法 YAML 报错", func(t *testing.T) {
		if _, err := injectComposeLimits("services: [unclosed", "1", ""); err == nil {
			t.Fatal("非法 YAML 应报错")
		}
	})

	t.Run("无 services 节不注入不报错", func(t *testing.T) {
		in := "volumes:\n  data: {}\n"
		out, err := injectComposeLimits(in, "1", "512m")
		if err != nil {
			t.Fatalf("无 services 不应报错: %v", err)
		}
		if strings.Contains(out, "limits") {
			t.Fatalf("无 services 不应注入:\n%s", out)
		}
	})
}
