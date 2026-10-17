// download_test.go 用 httptest 伪升级源走通自动下载全链路(不触真网):
// release API → 资产挑选 → 下载 + sha256 digest 校验 → 解包落位 → Ensure 幂等跳过。
package easytier

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/huangchengsir/pipewright/internal/version"
)

// fakeCoreBytes / fakeCliBytes 是伪二进制内容(解包后逐字节比对)。
var (
	fakeCore = []byte("#!/bin/sh\n# fake easytier-core v9.9.9\n")
	fakeCli  = []byte("#!/bin/sh\n# fake easytier-cli v9.9.9\n")
)

// buildAssetZip 构造包含 easytier-core/easytier-cli 的 zip 资产。
func buildAssetZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// 条目名用平台形态(Windows 资产带 .exe),与解包落位/定位逻辑一致。
	for name, body := range map[string][]byte{
		"easytier-linux-x86_64/" + CoreBinary(): fakeCore,
		"easytier-linux-x86_64/" + CliBinary():  fakeCli,
		"README.md":                             []byte("readme"),
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// serveFakeRelease 起一个伪 GitHub 升级源(镜像路径布局),返回 Downloader 与请求计数。
func serveFakeRelease(t *testing.T, asset []byte, digestOverride string, apiCalls *int) *Downloader {
	t.Helper()
	zipSHA := sha256.Sum256(asset)
	digest := "sha256:" + hex.EncodeToString(zipSHA[:])
	if digestOverride != "" {
		digest = digestOverride
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/EasyTier/EasyTier/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		*apiCalls++
		release := map[string]any{
			"tag_name": "v9.9.9",
			"assets": []map[string]any{
				{"name": "easytier-gui_9.9.9_amd64.deb", "digest": "sha256:" + string(bytes.Repeat([]byte("0"), 64))},
				{"name": "easytier-web-dashboard-v9.9.9.zip", "digest": "sha256:" + string(bytes.Repeat([]byte("1"), 64))},
				{"name": "easytier-linux-x86_64-v9.9.9.zip", "digest": digest, "size": len(asset)},
				{"name": "easytier-linux-aarch64-v9.9.9.zip", "digest": "sha256:" + string(bytes.Repeat([]byte("2"), 64))},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(release)
	})
	mux.HandleFunc("/EasyTier/EasyTier/releases/download/v9.9.9/easytier-linux-x86_64-v9.9.9.zip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(asset)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dataDir := t.TempDir()
	return &Downloader{
		DataDir: dataDir,
		GOOS:    "linux",
		GOARCH:  "amd64",
		Client:  srv.Client(),
		Source: func(ctx context.Context) version.Source {
			return version.Source{Repo: "EasyTier/EasyTier", APIBase: srv.URL, WebBase: srv.URL, DLBase: srv.URL}
		},
	}
}

func TestDownloaderFullFlow(t *testing.T) {
	asset := buildAssetZip(t)
	apiCalls := 0
	d := serveFakeRelease(t, asset, "", &apiCalls)

	ver, err := d.Download(context.Background())
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if ver != "v9.9.9" {
		t.Fatalf("version = %q", ver)
	}
	core, err := os.ReadFile(filepath.Join(d.DataDir, "bin", CoreBinary()))
	if err != nil || !bytes.Equal(core, fakeCore) {
		t.Fatalf("core binary mismatch: err=%v", err)
	}
	cli, err := os.ReadFile(filepath.Join(d.DataDir, "bin", CliBinary()))
	if err != nil || !bytes.Equal(cli, fakeCli) {
		t.Fatalf("cli binary mismatch: err=%v", err)
	}
	if got := InstalledVersion(d.DataDir); got != "v9.9.9" {
		t.Fatalf("installed version marker = %q", got)
	}

	// Ensure 幂等:已就位 → 不再打 release API。
	before := apiCalls
	if v, err := d.Ensure(context.Background()); err != nil || v != "v9.9.9" {
		t.Fatalf("ensure: %v %q", err, v)
	}
	if apiCalls != before {
		t.Fatalf("ensure should skip download (api calls %d -> %d)", before, apiCalls)
	}
}

func TestDownloaderDigestMismatch(t *testing.T) {
	asset := buildAssetZip(t)
	apiCalls := 0
	// digest 声明与实际内容不符 → 必须拒绝且不落任何二进制。
	d := serveFakeRelease(t, asset, "sha256:"+string(bytes.Repeat([]byte("f"), 64)), &apiCalls)
	if _, err := d.Download(context.Background()); err == nil {
		t.Fatalf("digest mismatch must fail")
	}
	if _, err := os.Stat(filepath.Join(d.DataDir, "bin", CoreBinary())); !os.IsNotExist(err) {
		t.Fatalf("no binary should be left on mismatch")
	}
}

func TestPickAssetPrefersDigestBearing(t *testing.T) {
	// 资产挑选:同平台多资产时无 digest 的不应干扰;digest 校验是下载层的硬门槛。
	assets := []releaseAsset{
		{Name: "easytier-linux-x86_64-v9.9.9.zip", Digest: "sha256:ab"},
	}
	got, err := pickAsset(assets, "linux", "x86_64")
	if err != nil || got.Name != "easytier-linux-x86_64-v9.9.9.zip" {
		t.Fatalf("pick digest-bearing asset: %v %+v", err, got)
	}
}

func TestPickAssetSelection(t *testing.T) {
	assets := []releaseAsset{
		{Name: "easytier-gui_9.9.9_x64.dmg", Digest: "sha256:a"},
		{Name: "easytier-linux-aarch64-v9.9.9.zip", Digest: "sha256:b"},
		{Name: "easytier-linux-x86_64-v9.9.9.zip", Digest: "sha256:c"},
		{Name: "easytier-windows-x86_64-v9.9.9.zip", Digest: "sha256:d"},
		{Name: "easytier-freebsd-13.2-x86_64-v9.9.9.zip", Digest: "sha256:e"},
	}
	cases := []struct {
		osName, archName, want string
	}{
		{"linux", "x86_64", "easytier-linux-x86_64-v9.9.9.zip"},
		{"linux", "aarch64", "easytier-linux-aarch64-v9.9.9.zip"},
		{"windows", "x86_64", "easytier-windows-x86_64-v9.9.9.zip"},
		{"freebsd", "x86_64", "easytier-freebsd-13.2-x86_64-v9.9.9.zip"},
	}
	for _, c := range cases {
		got, err := pickAsset(assets, c.osName, c.archName)
		if err != nil {
			t.Fatalf("pick(%s,%s): %v", c.osName, c.archName, err)
		}
		if got.Name != c.want {
			t.Fatalf("pick(%s,%s) = %q, want %q", c.osName, c.archName, got.Name, c.want)
		}
	}
	if _, err := pickAsset(assets, "plan9", "x86_64"); err == nil {
		t.Fatalf("unsupported platform should fail")
	}
}
