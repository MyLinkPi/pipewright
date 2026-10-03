package build

// artifact_files_manifest_test.go 覆盖「产物打包方式显式化」新增的三条路径:
//   - packMode=none:逐文件清单归档(format=files)+ **经 metadata 入库往返后**仍能按清单恢复;
//   - 空目录:归档空清单(与 tar 路径一致),绝不静默降级为占位;
//   - layout=top / contents:tar 包内是否保留顶层目录。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/huangchengsir/pipewright/internal/run"
)

// roundTripMetadata 复刻 run.encodeMetadata / run.decodeMetadata 的行为(产物 metadata 入库再读出):
// json.Marshal(map[string]any) → 字符串 → json.Unmarshal 回 map[string]any。
//
// format=files 的清单在写入侧是 json.RawMessage,经此往返**必然**变成 []any —— 读取侧
// (restoreFilesManifest / deploy.stageFilesManifest)必须兼容两种形状,否则 artifactPackMode=none
// 的产物在跨阶段恢复与部署时全部报「缺少 files 清单」。本文件的用例都强制走这一往返。
func roundTripMetadata(t *testing.T, md map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(md)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	return out
}

// seedDir 在 ws 下按「相对斜杠路径 → 内容」建出一棵目录树,返回根目录绝对路径。
func seedDir(t *testing.T, ws string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(ws, "dist")
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	return dir
}

// TestStoreDirFilesManifestSurvivesMetadataRoundTrip:packMode=none 归档出的清单,
// 经 metadata 入库往返(RawMessage → []any)后仍能被 restoreFilesManifest 完整还原
// (目录结构 + 字节),这是「不打包」模式端到端可用的关键不变量。
func TestStoreDirFilesManifestSurvivesMetadataRoundTrip(t *testing.T) {
	b, _ := newStoreBuilder(t)
	ws := t.TempDir()
	want := map[string]string{
		"index.html":       "<html>hi</html>",
		"assets/app.js":    "console.log(1)",
		"assets/img/a.png": "PNG-bytes",
	}
	dir := seedDir(t, ws, want)

	art := &run.Artifact{Name: "shop-dist", Type: run.ArtifactDist, Metadata: map[string]any{}}
	if !b.storeDistDir(art, dir, "none", "", func(string, string) {}) {
		t.Fatal("packMode=none 应归档成功")
	}
	if art.Metadata["format"] != "files" || art.Metadata["stored"] != true {
		t.Fatalf("metadata 应记 format=files + stored=true,得 %v", art.Metadata)
	}

	// 关键:模拟入库再读出(生产路径必经),再交给恢复侧。
	art.Metadata = roundTripMetadata(t, art.Metadata)
	if _, isRaw := art.Metadata["files"].(json.RawMessage); isRaw {
		t.Fatal("往返后 files 不应再是 json.RawMessage(用例前提失效)")
	}

	out := t.TempDir()
	if err := b.restoreFilesManifest(*art, out, func(string, string) {}); err != nil {
		t.Fatalf("restoreFilesManifest: %v", err)
	}
	for rel, body := range want {
		got, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("未恢复 %s: %v", rel, err)
		}
		if string(got) != body {
			t.Fatalf("%s 内容不符: got %q want %q", rel, got, body)
		}
	}
}

// TestRestoreFilesManifestRejectsEscape:清单里的越界路径(../ 逃逸)不得写出 destDir。
func TestRestoreFilesManifestRejectsEscape(t *testing.T) {
	b, st := newStoreBuilder(t)
	key, _, err := st.Put(bytes.NewReader([]byte("evil")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	manifest, _ := json.Marshal([]map[string]any{{"path": "../../escape.txt", "key": key, "size": 4}})
	art := run.Artifact{
		Name: "evil", Type: run.ArtifactDist, Reference: key,
		// 直接用往返后的形状([]any),与生产一致。
		Metadata: roundTripMetadata(t, map[string]any{
			"stored": true, "format": "files", "files": json.RawMessage(manifest),
		}),
	}
	ws := t.TempDir()
	dest := filepath.Join(ws, "work")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}
	if err := b.restoreFilesManifest(art, dest, func(string, string) {}); err != nil {
		t.Fatalf("restoreFilesManifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(ws), "escape.txt")); err == nil {
		t.Fatal("越界路径不应写出 destDir")
	}
}

// TestStoreDirFilesEmptyDirArchivesEmptyManifest:空目录归档为「空清单」而非降级占位
// (占位会把 reference 字符串当产物字节写到目标机,比空目录更糟);清单须序列化成 "[]" 而非 "null"。
func TestStoreDirFilesEmptyDirArchivesEmptyManifest(t *testing.T) {
	b, st := newStoreBuilder(t)
	dir := filepath.Join(t.TempDir(), "empty-dist")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	art := &run.Artifact{Name: "empty-dist", Type: run.ArtifactDist, Metadata: map[string]any{}}
	var logs []string
	if !b.storeDistDir(art, dir, "none", "", func(_, line string) { logs = append(logs, line) }) {
		t.Fatalf("空目录应归档为空清单而非失败,logs=%v", logs)
	}
	if art.Metadata["stored"] != true || art.Metadata["format"] != "files" {
		t.Fatalf("空目录也应 stored=true + format=files,得 %v", art.Metadata)
	}
	raw, err := json.Marshal(art.Metadata["files"])
	if err != nil {
		t.Fatalf("marshal files: %v", err)
	}
	if string(raw) != "[]" {
		t.Fatalf("空清单应序列化为 [] 而非 %s(null 会被读侧判成「缺少清单」)", raw)
	}
	rc, oerr := st.Open(art.Reference)
	if oerr != nil {
		t.Fatalf("Reference 应是可打开的合法句柄: %v", oerr)
	}
	_ = rc.Close() // 必须关闭:Windows 下未释放的句柄会锁住文件,导致 t.TempDir 清理失败
	// 日志应说明「无普通文件」,绝不出现 "<nil>"。
	joined := ""
	for _, l := range logs {
		joined += l
	}
	if bytes.Contains([]byte(joined), []byte("<nil>")) {
		t.Fatalf("日志不应出现 <nil>:%s", joined)
	}
	// 往返后读侧仍能解出空清单(不报错、恢复 0 个文件)。
	art.Metadata = roundTripMetadata(t, art.Metadata)
	if err := b.restoreFilesManifest(*art, t.TempDir(), func(string, string) {}); err != nil {
		t.Fatalf("空清单恢复不应报错: %v", err)
	}
}

// TestTarGzDirLayout:layout=top 保留顶层目录(包内路径以基名开头),
// layout=contents(默认)去掉顶层目录(内容铺包根)。
func TestTarGzDirLayout(t *testing.T) {
	ws := t.TempDir()
	dir := seedDir(t, ws, map[string]string{"index.html": "a", "assets/app.js": "b"})

	for _, tc := range []struct {
		keepTop bool
		want    []string
	}{
		{keepTop: false, want: []string{"assets", "assets/app.js", "index.html"}},
		{keepTop: true, want: []string{"dist", "dist/assets", "dist/assets/app.js", "dist/index.html"}},
	} {
		var buf bytes.Buffer
		if err := tarGzDir(dir, &buf, tc.keepTop); err != nil {
			t.Fatalf("tarGzDir(keepTop=%v): %v", tc.keepTop, err)
		}
		gz, err := gzip.NewReader(&buf)
		if err != nil {
			t.Fatalf("gzip reader: %v", err)
		}
		tr := tar.NewReader(gz)
		var got []string
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("tar next: %v", err)
			}
			name := h.Name
			if h.Typeflag == tar.TypeDir {
				name = filepath.ToSlash(filepath.Clean(name))
			}
			got = append(got, name)
		}
		_ = gz.Close()
		sort.Strings(got)
		if len(got) != len(tc.want) {
			t.Fatalf("keepTop=%v 包内条目 = %v, want %v", tc.keepTop, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("keepTop=%v 包内条目 = %v, want %v", tc.keepTop, got, tc.want)
			}
		}
	}
}
