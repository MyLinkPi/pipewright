package systemcfg

import (
	"context"
	"errors"
	"testing"

	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/storetest"
)

func TestNormalizeURL(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},    // 清除
		{"   ", "", false}, // 全空白 = 清除
		{"https://ci.example.com", "https://ci.example.com", false},
		{" https://ci.example.com/ ", "https://ci.example.com", false}, // 去空白 + 去尾斜杠
		{"http://10.0.0.8:8080", "http://10.0.0.8:8080", false},
		{"ftp://ci.example.com", "", true},          // 非 http(s)
		{"ci.example.com", "", true},                // 缺 scheme
		{"https://", "", true},                      // 缺 host
		{"https://ci.example.com/prefix", "", true}, // 带路径
		{"https://ci.example.com?a=1", "", true},    // 带查询
	}
	for _, c := range cases {
		got, err := NormalizeURL(c.in)
		if c.wantErr {
			if err == nil {
				t.Fatalf("NormalizeURL(%q) 应报错,得 %q", c.in, got)
			}
			if !errors.Is(err, ErrInvalidURL) {
				t.Fatalf("NormalizeURL(%q) 应 ErrInvalidURL,得 %v", c.in, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Fatalf("NormalizeURL(%q) = %q, %v;期望 %q", c.in, got, err, c.want)
		}
	}
}

func TestGetAndSetPublicURL(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc := New(st.DB)
		ctx := context.Background()

		// 迁移种子:默认空(未配置)。
		c, err := svc.Get(ctx)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if c.PublicURL != "" {
			t.Fatalf("默认 public_url 应为空,得 %q", c.PublicURL)
		}

		// 设置 → 读回(归一化:去尾斜杠)。
		c, err = svc.SetPublicURL(ctx, "https://ci.example.com/")
		if err != nil {
			t.Fatalf("SetPublicURL: %v", err)
		}
		if c.PublicURL != "https://ci.example.com" {
			t.Fatalf("public_url 应归一化,得 %q", c.PublicURL)
		}
		if c.UpdatedAt.IsZero() {
			t.Fatalf("updated_at 应回填")
		}
		c2, _ := svc.Get(ctx)
		if c2.PublicURL != "https://ci.example.com" {
			t.Fatalf("落库不符:%q", c2.PublicURL)
		}

		// 非法值 → ErrInvalidURL,原值不动。
		if _, err := svc.SetPublicURL(ctx, "not-a-url"); err == nil || !errors.Is(err, ErrInvalidURL) {
			t.Fatalf("非法 URL 应 ErrInvalidURL,得 %v", err)
		}
		c3, _ := svc.Get(ctx)
		if c3.PublicURL != "https://ci.example.com" {
			t.Fatalf("失败写入不应改动原值:%q", c3.PublicURL)
		}

		// 清除(空串)。
		if _, err := svc.SetPublicURL(ctx, "  "); err != nil {
			t.Fatalf("清除: %v", err)
		}
		c4, _ := svc.Get(ctx)
		if c4.PublicURL != "" {
			t.Fatalf("清除后应为空,得 %q", c4.PublicURL)
		}

		// Resolve:正常读 + nil 服务降级为空串。
		if _, err := svc.SetPublicURL(ctx, "https://ci.example.com"); err != nil {
			t.Fatalf("重设: %v", err)
		}
		if got := Resolve(ctx, svc); got != "https://ci.example.com" {
			t.Fatalf("Resolve 应返回当前值,得 %q", got)
		}
		if got := Resolve(ctx, nil); got != "" {
			t.Fatalf("nil 服务应降级空串,得 %q", got)
		}
	})
}

func TestNormalizeMirrorURL(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},    // 清除(回退 GitHub 官方源)
		{"   ", "", false}, // 全空白 = 清除
		{"https://mirror.example.com", "https://mirror.example.com", false},
		{" https://mirror.example.com/ ", "https://mirror.example.com", false},     // 去空白 + 去尾斜杠
		{"https://mirror.example.com/gh/", "https://mirror.example.com/gh", false}, // 子路径前缀合法
		{"http://10.0.0.8:8080", "http://10.0.0.8:8080", false},
		{"ftp://mirror.example.com", "", true},        // 非 http(s)
		{"mirror.example.com", "", true},              // 缺 scheme
		{"https://", "", true},                        // 缺 host
		{"https://mirror.example.com?a=1", "", true},  // 带查询
		{"https://mirror.example.com/gh#x", "", true}, // 带片段
	}
	for _, c := range cases {
		got, err := NormalizeMirrorURL(c.in)
		if c.wantErr {
			if err == nil {
				t.Fatalf("NormalizeMirrorURL(%q) 应报错,得 %q", c.in, got)
			}
			if !errors.Is(err, ErrInvalidMirror) {
				t.Fatalf("NormalizeMirrorURL(%q) 应 ErrInvalidMirror,得 %v", c.in, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Fatalf("NormalizeMirrorURL(%q) = %q, %v;期望 %q", c.in, got, err, c.want)
		}
	}
}

func TestGetAndSetReleaseMirror(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc := New(st.DB)
		ctx := context.Background()

		// 迁移种子:默认空(回退 GitHub 官方源)。
		c, err := svc.Get(ctx)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if c.ReleaseMirror != "" {
			t.Fatalf("默认 release_mirror 应为空,得 %q", c.ReleaseMirror)
		}

		// 设置 → 读回(归一化:去尾斜杠,允许子路径)。
		c, err = svc.SetReleaseMirror(ctx, "https://mirror.example.com/gh/")
		if err != nil {
			t.Fatalf("SetReleaseMirror: %v", err)
		}
		if c.ReleaseMirror != "https://mirror.example.com/gh" {
			t.Fatalf("release_mirror 应归一化,得 %q", c.ReleaseMirror)
		}
		if c.UpdatedAt.IsZero() {
			t.Fatalf("updated_at 应回填")
		}
		c2, _ := svc.Get(ctx)
		if c2.ReleaseMirror != "https://mirror.example.com/gh" {
			t.Fatalf("落库不符:%q", c2.ReleaseMirror)
		}

		// 非法值 → ErrInvalidMirror,原值不动。
		if _, err := svc.SetReleaseMirror(ctx, "mirror.example.com"); err == nil || !errors.Is(err, ErrInvalidMirror) {
			t.Fatalf("非法镜像应 ErrInvalidMirror,得 %v", err)
		}
		c3, _ := svc.Get(ctx)
		if c3.ReleaseMirror != "https://mirror.example.com/gh" {
			t.Fatalf("失败写入不应改动原值:%q", c3.ReleaseMirror)
		}

		// 清除(空串)。
		if _, err := svc.SetReleaseMirror(ctx, ""); err != nil {
			t.Fatalf("清除: %v", err)
		}
		c4, _ := svc.Get(ctx)
		if c4.ReleaseMirror != "" {
			t.Fatalf("清除后应为空,得 %q", c4.ReleaseMirror)
		}

		// ResolveReleaseMirror:正常读 + nil 服务/读失败降级为空串。
		if _, err := svc.SetReleaseMirror(ctx, "https://mirror.example.com"); err != nil {
			t.Fatalf("重设: %v", err)
		}
		if got := ResolveReleaseMirror(ctx, svc); got != "https://mirror.example.com" {
			t.Fatalf("ResolveReleaseMirror 应返回当前值,得 %q", got)
		}
		if got := ResolveReleaseMirror(ctx, nil); got != "" {
			t.Fatalf("nil 服务应降级空串,得 %q", got)
		}

		// 与 public_url 互不干扰。
		if c5, _ := svc.Get(ctx); c5.PublicURL != "" {
			t.Fatalf("release_mirror 写入不应影响 public_url,得 %q", c5.PublicURL)
		}
	})
}
