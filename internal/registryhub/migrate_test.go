package registryhub

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestParseComposeDataDirs(t *testing.T) {
	base := t.TempDir()
	quoted := `# 由 pipewright 自动生成,手动修改会在下次部署时被覆盖。
services:
  registry:
    image: registry:2
    ports:
      - "5000:5000"
    volumes:
      - "` + filepath.Join(base, "art") + `:/var/lib/registry"
  registry-cache:
    image: registry:2
    environment:
      REGISTRY_PROXY_REMOTEURL: "https://registry-1.docker.io"
    volumes:
      - "` + filepath.Join(base, "cch") + `:/var/lib/registry"
`
	art, cch := parseComposeDataDirs(quoted)
	if art != filepath.Join(base, "art") || cch != filepath.Join(base, "cch") {
		t.Fatalf("带引号解析不符: %q %q", art, cch)
	}
	// 旧版渲染不带引号,也须能识别(存量部署的目录变更依赖此解析)。
	legacy := `services:
  registry:
    volumes:
      - ` + filepath.Join(base, "art") + `:/var/lib/registry
  registry-cache:
    volumes:
      - ` + filepath.Join(base, "cch") + `:/var/lib/registry
`
	art, cch = parseComposeDataDirs(legacy)
	if art != filepath.Join(base, "art") || cch != filepath.Join(base, "cch") {
		t.Fatalf("无引号解析不符: %q %q", art, cch)
	}
	// 缺卷行/单卷 → 视为无上一部署。
	if a, c := parseComposeDataDirs("services: {}"); a != "" || c != "" {
		t.Fatalf("空 compose 应返回空: %q %q", a, c)
	}
}

func TestMoveDir(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	dst := filepath.Join(base, "nested", "dst-parent")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "f"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	// moveDir 不创建 dst 父目录(调用方 DeployStack 已 MkdirAll),这里先建。
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := moveDir(src, filepath.Join(dst, "dst")); err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, serr := os.Stat(src); !os.IsNotExist(serr) {
		t.Fatalf("源应消失: %v", serr)
	}
	got, rerr := os.ReadFile(filepath.Join(dst, "dst", "sub", "f"))
	if rerr != nil || string(got) != "data" {
		t.Fatalf("内容应完整迁移: %q %v", got, rerr)
	}
}

func TestCopyTree(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	dst := filepath.Join(base, "dst")
	if err := os.MkdirAll(filepath.Join(src, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a", "b", "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copy: %v", err)
	}
	info, err := os.Stat(filepath.Join(dst, "a", "b", "f"))
	if err != nil {
		t.Fatalf("目标缺失: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("权限位应保留: %v", info.Mode().Perm()) // Windows 不保留 POSIX 权限位
	}
	// 源目录在复制后保留(moveDir 才删除)。
	if _, serr := os.Stat(filepath.Join(src, "a", "b", "f")); serr != nil {
		t.Fatalf("copyTree 不应删源: %v", serr)
	}
}
