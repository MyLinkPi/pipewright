// upgrade_test.go 覆盖客户端自升级的平台无关核心:资产命名、校验和解析、安全解包、
// 多候选源回退下载、落位/清理与升级状态文件。平台无关,Linux 即可跑。
package pwclient

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/version"
)

// makeClientZip 生成一个最小客户端归档(键 = 归档内路径)。
func makeClientZip(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// 固定顺序,避免 map 迭代随机导致 flaky(对内容无影响)。
	order := []string{"pwclient.exe", "bin/easytier-core.exe", "bin/easytier-cli.exe",
		"licenses/easytier/LICENSE", "LICENSE"}
	written := map[string]bool{}
	for _, name := range order {
		body, ok := entries[name]
		if !ok {
			continue
		}
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
		written[name] = true
	}
	for name, body := range entries {
		if written[name] {
			continue
		}
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestClientAssetName(t *testing.T) {
	cases := []struct {
		tag, goos, goarch, want string
	}{
		{"v1.2.3", "windows", "amd64", "pwclient_1.2.3_windows_amd64.zip"},
		{"1.2.3", "windows", "arm64", "pwclient_1.2.3_windows_arm64.zip"},
		{" v1.2.3-rc.1 ", "windows", "amd64", "pwclient_1.2.3-rc.1_windows_amd64.zip"},
		{"v1.2.3", "linux", "amd64", "pwclient_1.2.3_linux_amd64.tar.gz"},
		{"v1.2.3", "darwin", "arm64", "pwclient_1.2.3_darwin_arm64.tar.gz"},
	}
	for _, c := range cases {
		if got := clientAssetName(c.tag, c.goos, c.goarch); got != c.want {
			t.Errorf("clientAssetName(%q,%q,%q)=%q want %q", c.tag, c.goos, c.goarch, got, c.want)
		}
	}
}

func TestFetchChecksum(t *testing.T) {
	const text = "aaaa  pwclient_1.0.0_windows_amd64.zip\nBBBB  pwclient_1.0.0_windows_arm64.zip\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(text))
	}))
	defer srv.Close()

	got, err := fetchChecksum(context.Background(), srv.Client(), srv.URL, "pwclient_1.0.0_windows_arm64.zip")
	if err != nil {
		t.Fatal(err)
	}
	if got != "bbbb" {
		t.Errorf("校验和应小写归一,得 %q", got)
	}
	if _, err := fetchChecksum(context.Background(), srv.Client(), srv.URL, "missing.zip"); err == nil {
		t.Errorf("缺失资产应报错")
	}
}

func TestExtractClientZip(t *testing.T) {
	zipBytes := makeClientZip(t, map[string][]byte{
		"pwclient.exe":                  []byte("exe"),
		"bin/easytier-core.exe":         []byte("core"),
		"licenses/easytier/LICENSE":     []byte("lgpl"),
		"README-not-used-by-upgrade.md": []byte("x"),
	})
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "a.zip")
	if err := os.WriteFile(zipPath, zipBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	if err := extractClientZip(zipPath, stage, "pwclient.exe"); err != nil {
		t.Fatalf("extractClientZip: %v", err)
	}
	for name, want := range map[string]string{
		"pwclient.exe":              "exe",
		"bin/easytier-core.exe":     "core",
		"licenses/easytier/LICENSE": "lgpl",
	} {
		got, err := os.ReadFile(filepath.Join(stage, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q err=%v,期望 %q", name, got, err, want)
		}
	}
}

func TestExtractClientZip_RejectsMissingExeAndTraversal(t *testing.T) {
	dir := t.TempDir()

	// 缺 pwclient.exe。
	noExe := makeClientZip(t, map[string][]byte{"bin/easytier-core.exe": []byte("core")})
	p1 := filepath.Join(dir, "noexe.zip")
	if err := os.WriteFile(p1, noExe, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := extractClientZip(p1, t.TempDir(), "pwclient.exe"); err == nil ||
		!strings.Contains(err.Error(), "pwclient.exe") {
		t.Errorf("缺少 pwclient.exe 应报错,得 %v", err)
	}

	// 路径穿越(正斜杠与反斜杠两种形态)。
	for _, evil := range []string{"../evil.txt", `..\evil.txt`} {
		bad := makeClientZip(t, map[string][]byte{
			"pwclient.exe": []byte("exe"),
			evil:           []byte("evil"),
		})
		p := filepath.Join(dir, "bad-"+strings.ReplaceAll(evil, "/", "_")+".zip")
		if err := os.WriteFile(p, bad, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := extractClientZip(p, t.TempDir(), "pwclient.exe"); err == nil ||
			!strings.Contains(err.Error(), "非法路径") {
			t.Errorf("越界路径 %q 应被拒绝,得 %v", evil, err)
		}
	}
}

// makeClientTarGz 生成一个最小 Linux/macOS tar.gz 客户端归档(键 = 归档内路径,值 = 内容+权限位)。
func makeClientTarGz(t *testing.T, entries map[string]struct {
	body []byte
	mode int64
}) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	order := []string{"pwclient", "bin/easytier-core", "bin/easytier-cli", "licenses/easytier/LICENSE", "LICENSE"}
	written := map[string]bool{}
	writeOne := func(name string, body []byte, mode int64) {
		hdr := &tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range order {
		if e, ok := entries[name]; ok {
			writeOne(name, e.body, e.mode)
			written[name] = true
		}
	}
	for name, e := range entries {
		if written[name] {
			continue
		}
		writeOne(name, e.body, e.mode)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractClientTarGz(t *testing.T) {
	blob := makeClientTarGz(t, map[string]struct {
		body []byte
		mode int64
	}{
		"pwclient":                  {[]byte("bin"), 0o755},
		"bin/easytier-core":         {[]byte("core"), 0o755},
		"licenses/easytier/LICENSE": {[]byte("lgpl"), 0o644},
	})
	dir := t.TempDir()
	tzPath := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(tzPath, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	if err := extractClientArchive(tzPath, stage, "pwclient"); err != nil {
		t.Fatalf("extractClientArchive(tar.gz): %v", err)
	}
	for name, want := range map[string]string{
		"pwclient":                  "bin",
		"bin/easytier-core":         "core",
		"licenses/easytier/LICENSE": "lgpl",
	} {
		got, err := os.ReadFile(filepath.Join(stage, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q err=%v,期望 %q", name, got, err, want)
		}
	}
	// 主程序与随包二进制需保留可执行位(否则 easytier-core 落位后无法直接执行)。
	for _, name := range []string{"pwclient", "bin/easytier-core"} {
		fi, err := os.Stat(filepath.Join(stage, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&0o111 == 0 {
			t.Errorf("%s 应保留可执行位,实际 %v", name, fi.Mode())
		}
	}
}

func TestExtractClientTarGz_RejectsMissingBinaryTraversalSymlink(t *testing.T) {
	dir := t.TempDir()

	// 缺根级 pwclient(只在子目录有)。
	noBin := makeClientTarGz(t, map[string]struct {
		body []byte
		mode int64
	}{"bin/pwclient": {[]byte("x"), 0o755}})
	p1 := filepath.Join(dir, "nobin.tar.gz")
	if err := os.WriteFile(p1, noBin, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := extractClientTarGz(p1, t.TempDir(), "pwclient"); err == nil ||
		!strings.Contains(err.Error(), "pwclient") {
		t.Errorf("缺根级 pwclient 应报错,得 %v", err)
	}

	// 路径穿越:手工构造带 ../ 条目的 tar.gz(不能经由 makeClientTarGz 的正常键)。
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	_ = tw.WriteHeader(&tar.Header{Name: "pwclient", Mode: 0o755, Size: 1, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("x"))
	_ = tw.WriteHeader(&tar.Header{Name: "../evil", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("y"))
	_ = tw.Close()
	_ = gw.Close()
	p2 := filepath.Join(dir, "trav.tar.gz")
	if err := os.WriteFile(p2, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := extractClientTarGz(p2, t.TempDir(), "pwclient"); err == nil ||
		!strings.Contains(err.Error(), "非法路径") {
		t.Errorf("路径穿越应被拒绝,得 %v", err)
	}

	// 符号链接条目:拒绝。
	var sb bytes.Buffer
	sgw := gzip.NewWriter(&sb)
	stw := tar.NewWriter(sgw)
	_ = stw.WriteHeader(&tar.Header{Name: "pwclient", Mode: 0o755, Size: 1, Typeflag: tar.TypeReg})
	_, _ = stw.Write([]byte("x"))
	_ = stw.WriteHeader(&tar.Header{Name: "link", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	_ = stw.Close()
	_ = sgw.Close()
	p3 := filepath.Join(dir, "sym.tar.gz")
	if err := os.WriteFile(p3, sb.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := extractClientTarGz(p3, t.TempDir(), "pwclient"); err == nil ||
		!strings.Contains(err.Error(), "非普通文件") {
		t.Errorf("符号链接应被拒绝,得 %v", err)
	}
}

// goodUpgradeServer 提供 releases/latest、checksums.txt 与客户端 zip。
func goodUpgradeServer(t *testing.T, tag string, zipBytes []byte) *httptest.Server {
	t.Helper()
	asset := clientAssetName(tag, "windows", "amd64")
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			_, _ = w.Write([]byte(`{"tag_name":"` + tag + `"}`))
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			_, _ = w.Write([]byte(sha256Hex(zipBytes) + "  " + asset + "\n"))
		case strings.HasSuffix(r.URL.Path, "/"+asset):
			_, _ = w.Write(zipBytes)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func newTestUpgrader(client *http.Client, srcs ...version.Source) *Upgrader {
	return &Upgrader{
		Checker: version.NewCheckerWithClient(client),
		Sources: func(context.Context) []version.Source { return srcs },
		GOOS:    "windows",
		GOARCH:  "amd64",
		Client:  client,
		// 测试不碰真实 GitHub:校验和参照指向首源(与旧同源校验行为一致)。
		ChecksumURLFn: func(_ context.Context, repo, tag string) string {
			return strings.TrimRight(srcs[0].DLBase, "/") + "/" + repo + "/releases/download/" + tag + "/checksums.txt"
		},
	}
}

func TestUpgraderCheckAndStageFallback(t *testing.T) {
	zipBytes := makeClientZip(t, map[string][]byte{
		"pwclient.exe":          []byte("new-exe"),
		"bin/easytier-core.exe": []byte("new-core"),
	})
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	good := goodUpgradeServer(t, "v9.9.9", zipBytes)
	defer good.Close()

	badSrc := version.Source{Repo: "o/r", APIBase: bad.URL, WebBase: bad.URL, DLBase: bad.URL}
	goodSrc := version.Source{Repo: "o/r", APIBase: good.URL, WebBase: good.URL, DLBase: good.URL}
	up := newTestUpgrader(bad.Client(), badSrc, goodSrc)

	// 检查:第一源 500,自动回退第二源。
	info := up.Check(context.Background(), false)
	if info.CheckError != "" || info.Latest != "v9.9.9" {
		t.Fatalf("检查应回退到第二源,得 %+v", info)
	}

	// 下载/校验/解包:同样回退到第二源。
	stage, err := up.Stage(context.Background(), "v9.9.9")
	if err != nil {
		t.Fatalf("Stage 应回退成功: %v", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()

	if got, _ := os.ReadFile(filepath.Join(stage, "pwclient.exe")); string(got) != "new-exe" {
		t.Errorf("staged pwclient.exe = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(stage, "bin", "easytier-core.exe")); string(got) != "new-core" {
		t.Errorf("staged easytier-core = %q", got)
	}
}

func TestUpgraderStageChecksumMismatch(t *testing.T) {
	zipBytes := makeClientZip(t, map[string][]byte{"pwclient.exe": []byte("exe")})
	asset := clientAssetName("v9.9.9", "windows", "amd64")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			_, _ = w.Write([]byte("deadbeef  " + asset + "\n"))
		case strings.HasSuffix(r.URL.Path, "/"+asset):
			_, _ = w.Write(zipBytes)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	src := version.Source{Repo: "o/r", APIBase: srv.URL, WebBase: srv.URL, DLBase: srv.URL}
	up := newTestUpgrader(srv.Client(), src)
	if _, err := up.Stage(context.Background(), "v9.9.9"); err == nil ||
		!strings.Contains(err.Error(), "校验和不匹配") {
		t.Fatalf("校验和不匹配应报错,得 %v", err)
	}
}

func TestUpgraderStageAllSourcesFail(t *testing.T) {
	bad := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
	}
	s1, s2 := bad(), bad()
	defer s1.Close()
	defer s2.Close()
	src := func(s *httptest.Server) version.Source {
		return version.Source{Repo: "o/r", APIBase: s.URL, WebBase: s.URL, DLBase: s.URL}
	}
	up := newTestUpgrader(s1.Client(), src(s1), src(s2))
	if _, err := up.Stage(context.Background(), "v9.9.9"); err == nil ||
		!errors.Is(err, ErrUpgrade) || !strings.Contains(err.Error(), "2 个升级源") {
		t.Fatalf("全部候选失败应带源数量报错,得 %v", err)
	}
	if _, err := up.Stage(context.Background(), ""); err == nil {
		t.Errorf("空 tag 应报错")
	}
	if _, err := up.Stage(context.Background(), "v1.0.0 --force"); err == nil {
		t.Errorf("含空格的非法 tag 应拒绝")
	}
}

// TestUpgraderStageCrossOriginChecksum 镜像篡改「同源 checksums + zip」时,
// 官方异源清单仍须拦下 tampered 包(真实性校验不因回退镜像而降级为完整性)。
func TestUpgraderStageCrossOriginChecksum(t *testing.T) {
	goodZip := makeClientZip(t, map[string][]byte{"pwclient.exe": []byte("good")})
	evilZip := makeClientZip(t, map[string][]byte{"pwclient.exe": []byte("evil")})

	// 官方参照:只认 good。
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/checksums.txt") {
			_, _ = w.Write([]byte(sha256Hex(goodZip) + "  " + clientAssetName("v9.9.9", "windows", "amd64") + "\n"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer official.Close()

	// 镜像:提供 evil zip + 配套的「同源」清单(篡改者做法)。
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asset := clientAssetName("v9.9.9", "windows", "amd64")
		switch {
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			_, _ = w.Write([]byte(sha256Hex(evilZip) + "  " + asset + "\n"))
		case strings.HasSuffix(r.URL.Path, "/"+asset):
			_, _ = w.Write(evilZip)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mirror.Close()

	src := version.Source{Repo: "o/r", APIBase: mirror.URL, WebBase: mirror.URL, DLBase: mirror.URL}
	up := newTestUpgrader(mirror.Client(), src)
	up.ChecksumURLFn = func(_ context.Context, repo, tag string) string {
		return official.URL + "/" + repo + "/releases/download/" + tag + "/checksums.txt"
	}
	if _, err := up.Stage(context.Background(), "v9.9.9"); err == nil ||
		!strings.Contains(err.Error(), "校验和不匹配") {
		t.Fatalf("异源校验应拦下篡改包,得 %v", err)
	}

	// 官方清单不可达:回退同源仍硬校验(篡改者若同源配套造假则无法区分 —— 已知
	// 降级路径,行为为下载成功;此处验证回退链路本身可用)。
	up.ChecksumURLFn = func(context.Context, string, string) string { return mirror.URL + "/missing/checksums.txt" }
	if _, err := up.Stage(context.Background(), "v9.9.9"); err != nil {
		t.Fatalf("官方不可达应回退同源清单,得 %v", err)
	}
}

func TestApplyStagedAndCleanup(t *testing.T) {
	installDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(installDir, "pwclient.exe"), []byte("old-exe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(installDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "bin", "easytier-core.exe"), []byte("old-core"), 0o755); err != nil {
		t.Fatal(err)
	}

	stage := t.TempDir()
	for name, body := range map[string][]byte{
		"pwclient.exe":              []byte("new-exe"),
		"bin/easytier-core.exe":     []byte("new-core"),
		"bin/easytier-cli.exe":      []byte("new-cli"),
		"licenses/easytier/LICENSE": []byte("lgpl"),
		"LICENSE":                   []byte("license"),
	} {
		p := filepath.Join(stage, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	oldExe, err := ApplyStaged(stage, installDir, "pwclient.exe")
	if err != nil {
		t.Fatalf("ApplyStaged: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(installDir, "pwclient.exe")); string(got) != "new-exe" {
		t.Errorf("新 exe 未落位,得 %q", got)
	}
	if got, _ := os.ReadFile(oldExe); string(got) != "old-exe" {
		t.Errorf("旧 exe 应以 .old-* 保留,得 %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(installDir, "bin", "easytier-core.exe")); string(got) != "new-core" {
		t.Errorf("bin 未刷新,得 %q", got)
	}
	if _, err := os.Stat(filepath.Join(installDir, "bin", "easytier-cli.exe")); err != nil {
		t.Errorf("新增 bin 文件缺失: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(installDir, "licenses", "easytier", "LICENSE")); string(got) != "lgpl" {
		t.Errorf("licenses 未同步,得 %q", got)
	}

	CleanupStaleExecutables(installDir, "pwclient.exe")
	if _, err := os.Stat(oldExe); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("CleanupStaleExecutables 应删除旧 exe,得 %v", err)
	}
}

// TestApplyStagedPartialFailure 验证「主程序已落位、附属文件同步失败」的非致命路径:
// 错误包装 ErrPartialUpgrade、不回滚新版 exe、旧镜像就地清理(调用方据此报 done+警示)。
func TestApplyStagedPartialFailure(t *testing.T) {
	installDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(installDir, "pwclient.exe"), []byte("old-exe"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 作障:bin/easytier-core.exe 位置放一个同名目录,rename 覆盖必失败(跨平台均然)。
	if err := os.MkdirAll(filepath.Join(installDir, "bin", "easytier-core.exe"), 0o755); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	for name, body := range map[string][]byte{
		"pwclient.exe":          []byte("new-exe"),
		"bin/easytier-core.exe": []byte("new-core"),
	} {
		p := filepath.Join(stage, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	oldExe, err := ApplyStaged(stage, installDir, "pwclient.exe")
	if !errors.Is(err, ErrPartialUpgrade) {
		t.Fatalf("附属失败应包装 ErrPartialUpgrade,得 %v", err)
	}
	if oldExe != "" {
		t.Errorf("非致命失败不应回抛旧镜像路径,得 %q", oldExe)
	}
	if got, _ := os.ReadFile(filepath.Join(installDir, "pwclient.exe")); string(got) != "new-exe" {
		t.Errorf("主程序应已落位不回滚,得 %q", got)
	}
	entries, _ := os.ReadDir(installDir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".old-") {
			t.Errorf("旧镜像应就地清理,残留 %q", e.Name())
		}
	}
}

// TestSkipUpgradeDecision 表驱动验证升级跳过的纯函数判定(落点先行:基准恒为
// 安装目标版本,不看当前进程版本;4 段 FileVersion vs 3 段 tag 语义比较)。
func TestSkipUpgradeDecision(t *testing.T) {
	cases := []struct {
		name         string
		install, lat string
		tag          string
		force        bool
		wantSkip     bool
		wantReason   string // "" = 只断言 skip;"!" 前缀 = 断言不含
	}{
		{"落后于最新→升级", "1.6.7.0", "v1.6.9", "", false, false, ""},
		{"已是最新(4段vs3段)→跳过", "1.6.9.0", "v1.6.9", "", false, true, "当前已是"},
		{"解压新版升级旧服务→升级(误报回归)", "1.6.7.0", "v1.6.9", "", false, false, ""},
		{"当前领先最新→跳过", "1.7.0.0", "v1.6.9", "", false, true, "已是最新"},
		{"指定同版本tag→跳过(重装需force)", "1.6.7.0", "v1.6.9", "v1.6.7", false, true, "--force"},
		{"指定更低版本→跳过", "1.6.9.0", "", "v1.6.7", false, true, "--force"},
		{"指定更高版本tag→升级", "1.6.7.0", "", "v2.0.0", false, false, ""},
		{"force→恒升级", "1.6.9.0", "v1.6.9", "", true, false, ""},
		{"开发构建无tag→跳过", "dev", "v1.6.9", "", false, true, "--tag"},
		{"开发构建指定tag→照办", "dev", "", "v1.6.9", false, false, ""},
		{"无任何版本信息→跳过", "1.6.7.0", "", "", false, true, "暂无发布"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			skip, reason := SkipUpgradeDecision(c.install, c.lat, c.tag, c.force)
			if skip != c.wantSkip {
				t.Fatalf("skip = %v (%q), want %v", skip, reason, c.wantSkip)
			}
			if c.wantReason != "" && !strings.Contains(reason, c.wantReason) {
				t.Errorf("reason = %q, 应含 %q", reason, c.wantReason)
			}
		})
	}
}

// TestValidateTag 验证升级 tag 白名单:正常 semver 放行,注入字符/路径符号拒绝。
func TestValidateTag(t *testing.T) {
	for _, ok := range []string{"", "v1.2.3", "1.2.3", "v1.2.3-rc.1", "v1.2.3+build.5", "v0.0.1"} {
		if err := ValidateTag(ok); err != nil {
			t.Errorf("合法 tag %q 被拒:%v", ok, err)
		}
	}
	for _, bad := range []string{
		"v1.2", "--force", "v1.0.0 --force", `v1.0.0" x`, "v1.0.0/../evil", "v1.0.0?x=1",
		"v1.0.0#frag", "v 1.0.0", "$(rm -rf)", "v1.0.0;ls",
	} {
		if err := ValidateTag(bad); err == nil {
			t.Errorf("非法 tag %q 应拒绝", bad)
		}
	}
}

func TestCopyStagedBinariesToExtraDir(t *testing.T) {
	stage := t.TempDir()
	for name, body := range map[string][]byte{
		"bin/easytier-core.exe": []byte("new-core"),
		"bin/easytier-cli.exe":  []byte("new-cli"),
	} {
		p := filepath.Join(stage, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dataBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataBin, "easytier-core.exe"), []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataBin, "easytier-version.txt"), []byte("v1.0.0"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := CopyStagedBinaries(stage, dataBin); err != nil {
		t.Fatalf("CopyStagedBinaries: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dataBin, "easytier-core.exe")); string(got) != "new-core" {
		t.Errorf("dataDir bin 未刷新,得 %q", got)
	}
	// 非归档内文件保持不动(版本标记由下载器管理)。
	if got, _ := os.ReadFile(filepath.Join(dataBin, "easytier-version.txt")); string(got) != "v1.0.0" {
		t.Errorf("无关文件不应被覆盖,得 %q", got)
	}
	// 目标 bin 不存在时直接返回(调用方未必有该落点)。
	if err := CopyStagedBinaries(stage, filepath.Join(t.TempDir(), "no-such-bin")); err != nil {
		t.Errorf("目标目录不存在应自动创建,得 %v", err)
	}
}

func TestParseServiceBinaryPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"C:\Program Files\Pipewright Client\pwclient.exe" service run`,
			`C:\Program Files\Pipewright Client\pwclient.exe`},
		{`C:\Program Files\Pipewright Client\pwclient.exe service run`,
			`C:\Program Files\Pipewright Client\pwclient.exe`},
		{`C:\Tools\pwclient.exe`, `C:\Tools\pwclient.exe`},
		{`  `, ""},
	}
	for _, c := range cases {
		if got := ParseServiceBinaryPath(c.in); got != c.want {
			t.Errorf("ParseServiceBinaryPath(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestUpgradeStatusRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadUpgradeStatus(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("未写入时应返回 NotExist,得 %v", err)
	}
	want := UpgradeStatus{Phase: UpgradePhaseApplying, Current: "v1.0.0", Target: "v1.2.0",
		Message: "正在替换程序文件"}
	if err := WriteUpgradeStatus(dir, want); err != nil {
		t.Fatalf("WriteUpgradeStatus: %v", err)
	}
	got, err := ReadUpgradeStatus(dir)
	if err != nil {
		t.Fatalf("ReadUpgradeStatus: %v", err)
	}
	if got.Phase != want.Phase || got.Current != want.Current || got.Target != want.Target ||
		got.Message != want.Message || got.Time == "" {
		t.Errorf("状态回路不一致: %+v", got)
	}
	if st, err := os.Stat(UpgradeStatusPath(dir)); err != nil || st.Mode().Perm() != 0o644 {
		t.Errorf("状态文件应为 0644(托盘普通用户可读),得 %v %v", st, err)
	}
}

// TestUpgradeStatusSkippedRoundTrip 验证「无需升级」终态(skipped)可跨进程回读:
// 升级命令提前退出时托盘据此立即结束,不落入超时兜底。
func TestUpgradeStatusSkippedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := UpgradeStatus{Phase: UpgradePhaseSkipped, Current: "1.1.0", Target: "1.2.0",
		Message: "当前已是 1.2.0;如需重装请加 --force。"}
	if err := WriteUpgradeStatus(dir, want); err != nil {
		t.Fatalf("WriteUpgradeStatus: %v", err)
	}
	got, err := ReadUpgradeStatus(dir)
	if err != nil {
		t.Fatalf("ReadUpgradeStatus: %v", err)
	}
	if got.Phase != UpgradePhaseSkipped || got.Message != want.Message || got.Time == "" {
		t.Errorf("skipped 终态回路不一致: %+v", got)
	}
}
