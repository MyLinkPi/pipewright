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

// TestBinStatusPackagesNeverNil:packages 恒为数组(未缓存 / 已缓存),绝不为 null
// (前端模板按数组消费;nil 切片会序列化成 null 打断渲染 —— 回归守护)。
func TestBinStatusPackagesNeverNil(t *testing.T) {
	empty := &Downloader{DataDir: t.TempDir()}
	if st := empty.BinStatus(); st.Packages == nil || len(st.Packages) != 0 {
		t.Fatalf("未缓存时 packages 应为空数组, got %#v", st.Packages)
	}

	dir := t.TempDir()
	if err := writeFakePackage(dir, "v1.0.5", "ssar-1.0.5-1.x86_64.rpm"); err != nil {
		t.Fatalf("准备缓存: %v", err)
	}
	if err := writeDebFakePackages(dir, "v1.0.5", "ssar_1.0.5-1_amd64.deb"); err != nil {
		t.Fatalf("准备缓存: %v", err)
	}
	d := &Downloader{DataDir: dir}
	st := d.BinStatus()
	if st.Version != "v1.0.5" || st.Packages == nil || len(st.Packages) != 2 {
		t.Fatalf("已缓存: version=%q packages=%#v", st.Version, st.Packages)
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
