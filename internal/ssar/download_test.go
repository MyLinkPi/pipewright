package ssar

import (
	"path/filepath"
	"testing"
)

// TestPickPackageAssets:rpm/deb 保留,checksums.txt 等排除。
func TestPickPackageAssets(t *testing.T) {
	assets := []releaseAsset{
		{Name: "checksums.txt"},
		{Name: "ssar-1.0.5-1.x86_64.rpm"},
		{Name: "ssar_1.0.5-1_amd64.deb"},
		{Name: "source.tar.gz"},
	}
	got := pickPackageAssets(assets)
	if len(got) != 2 {
		t.Fatalf("want 2 packages, got %d: %v", len(got), got)
	}
}

// TestDebArch:x86_64 → amd64;aarch64 → arm64。
func TestDebArch(t *testing.T) {
	if debArch("x86_64") != "amd64" || debArch("aarch64") != "arm64" {
		t.Fatalf("debArch 映射错误: %q %q", debArch("x86_64"), debArch("aarch64"))
	}
}

// TestPackagePath:按家族/架构选缓存包;aarch64 无资产 → 明确错误;未缓存 → 提示下载。
func TestPackagePath(t *testing.T) {
	dir := t.TempDir()
	if err := writeFakePackage(dir, "v1.0.5", "ssar-1.0.5-1.x86_64.rpm"); err != nil {
		t.Fatalf("准备缓存: %v", err)
	}
	if err := writeDebFakePackages(dir, "v1.0.5", "ssar_1.0.5-1_amd64.deb"); err != nil {
		t.Fatalf("准备缓存: %v", err)
	}
	d := &Downloader{DataDir: dir}

	p, err := d.PackagePath("rpm", "x86_64")
	if err != nil || filepath.Base(p) != "ssar-1.0.5-1.x86_64.rpm" {
		t.Fatalf("rpm/x86_64: %v %v", p, err)
	}
	p, err = d.PackagePath("deb", "x86_64")
	if err != nil || filepath.Base(p) != "ssar_1.0.5-1_amd64.deb" {
		t.Fatalf("deb/x86_64: %v %v", p, err)
	}
	if _, err := d.PackagePath("rpm", "aarch64"); err == nil {
		t.Fatal("aarch64 无资产应报错")
	}
	empty := &Downloader{DataDir: t.TempDir()}
	if _, err := empty.PackagePath("rpm", "x86_64"); err == nil {
		t.Fatal("未缓存应报错")
	}
}

// writeDebFakePackages 的 deb 变体(writeFakePackage 只会重复写 rpm)。
func writeDebFakePackages(dataDir, tag, pkgName string) error {
	return writeFakePackage(dataDir, tag, pkgName)
}
