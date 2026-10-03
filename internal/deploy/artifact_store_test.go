package deploy

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/artifactstore"
	"github.com/huangchengsir/pipewright/internal/run"
)

// seedSuccessRunWithStoredArtifact 像 seedSuccessRunWithArtifact,但产物带制品库句柄 + metadata
// (Story 8-16:reference=storeKey,stored=true)。
func seedSuccessRunWithStoredArtifact(t *testing.T, db *sql.DB, rsvc run.Service, artType, storeKey string, meta map[string]any) (string, string) {
	t.Helper()
	runID, _ := seedSuccessRunWithArtifact(t, db, rsvc, artType, "placeholder")
	art, err := rsvc.AddArtifact(context.Background(), run.Artifact{
		RunID: runID, Type: artType, Name: "shop", Reference: storeKey, Metadata: meta,
	})
	if err != nil {
		t.Fatalf("AddArtifact stored: %v", err)
	}
	return runID, art.ID
}

// 制品库支撑的 jar 部署:目标机落的是**真 jar 字节**(经 Upload),不是占位 reference 串。
func TestDeployStoredJarUploadsRealBytes(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	store, err := artifactstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	jarBytes := []byte("PK\x03\x04 REAL jar payload \x00\xff\xfe")
	key, _, err := store.Put(bytes.NewReader(jarBytes))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	tgt := &stubTarget{}
	srv := seedServer(t, tgt, "web-1")
	runID, artID := seedSuccessRunWithStoredArtifact(t, db, rsvc, run.ArtifactJar, key,
		map[string]any{"stored": true, "format": "file", "filename": "app.jar"})

	svc := New(tgt, rsvc, WithArtifactStore(store))
	base := "/srv/app-" + uuid.NewString()[:6]
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srv.ID},
		Config: map[string]string{"releaseBase": base},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res[0].Status != run.TargetSuccess {
		t.Fatalf("status = %s (msg %q), want success", res[0].Status, res[0].Message)
	}
	// 目标机发布目录里应被 Upload 了**真 jar 字节**(非 reference 串占位)。
	wantPath := base + "/releases/" + runID + "/app.jar"
	got, ok := tgt.uploads[wantPath]
	if !ok {
		t.Fatalf("未在 %s 上传产物;uploads=%v", wantPath, keysOf(tgt.uploads))
	}
	if !bytes.Equal(got, jarBytes) {
		t.Fatalf("上传的不是真 jar 字节")
	}
	// 且绝不应再走旧的「base64 写 reference 串」占位路径。
	for _, c := range tgt.calls {
		if len(c) >= 3 && strings.Contains(strings.Join(c, " "), "base64 -d") {
			t.Fatalf("制品库产物不应再用 base64 占位写入:%v", c)
		}
	}
}

// 制品库支撑的 dist 部署:上传 tar.gz 并远端解包(命令含 tar -xzf)。
func TestDeployStoredDistUploadsAndUntars(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	store, _ := artifactstore.New(t.TempDir())
	key, _, _ := store.Put(bytes.NewReader([]byte("\x1f\x8b fake tar.gz bytes")))

	tgt := &stubTarget{}
	srv := seedServer(t, tgt, "web-1")
	runID, artID := seedSuccessRunWithStoredArtifact(t, db, rsvc, run.ArtifactDist, key,
		map[string]any{"stored": true, "format": "tar.gz"})

	svc := New(tgt, rsvc, WithArtifactStore(store))
	base := "/srv/web-" + uuid.NewString()[:6]
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srv.ID},
		Config: map[string]string{"releaseBase": base},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res[0].Status != run.TargetSuccess {
		t.Fatalf("status = %s (msg %q), want success", res[0].Status, res[0].Message)
	}
	// 应上传了 tar.gz。
	tarPath := base + "/releases/" + runID + "/.pw-artifact.tar.gz"
	if _, ok := tgt.uploads[tarPath]; !ok {
		t.Fatalf("未上传 dist tar.gz 到 %s", tarPath)
	}
	// 远端应解包(命令序列含 tar -xzf)。
	var sawUntar bool
	for _, c := range tgt.calls {
		if len(c) >= 2 && c[0] == "tar" && c[1] == "-xzf" {
			sawUntar = true
		}
	}
	if !sawUntar {
		t.Fatalf("dist 部署应在远端 tar -xzf 解包;calls=%v", tgt.calls)
	}
}

// 制品库支撑的「不打包」dist 部署(format=files):按 metadata.files 清单逐文件上传到
// <release>/<相对路径>,目录结构原样保留。
//
// 关键不变量:清单在构建侧写入时是 json.RawMessage,但**经 DB 往返**(run.decodeMetadata 用
// json.Unmarshal 到 map[string]any)后必然变成 []any —— 本用例经 AddArtifact/ListArtifacts
// 真实走一遍往返,钉住部署侧必须兼容该形状(否则该机恒失败「产物缺少 files 清单」)。
func TestDeployStoredFilesManifestUploadsEachFile(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	store, err := artifactstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	indexBody := []byte("<html>hi</html>")
	jsBody := []byte("console.log(1)")
	indexKey, _, err := store.Put(bytes.NewReader(indexBody))
	if err != nil {
		t.Fatalf("Put index: %v", err)
	}
	jsKey, _, err := store.Put(bytes.NewReader(jsBody))
	if err != nil {
		t.Fatalf("Put js: %v", err)
	}
	manifest, err := json.Marshal([]map[string]any{
		{"path": "index.html", "key": indexKey, "size": len(indexBody)},
		{"path": "assets/app.js", "key": jsKey, "size": len(jsBody)},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	tgt := &stubTarget{}
	srv := seedServer(t, tgt, "web-1")
	runID, artID := seedSuccessRunWithStoredArtifact(t, db, rsvc, run.ArtifactDist, indexKey,
		map[string]any{"stored": true, "format": "files", "files": json.RawMessage(manifest)})

	// 往返后的形状必须是 []any(用例前提;若哪天 decodeMetadata 改成保留 RawMessage,这里会提醒)。
	arts, err := rsvc.ListArtifacts(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	for i := range arts {
		if arts[i].ID == artID {
			if _, isRaw := arts[i].Metadata["files"].(json.RawMessage); isRaw {
				t.Fatal("DB 往返后 files 不应再是 json.RawMessage(用例前提失效)")
			}
		}
	}

	svc := New(tgt, rsvc, WithArtifactStore(store))
	base := "/srv/web-" + uuid.NewString()[:6]
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srv.ID},
		Config: map[string]string{"releaseBase": base},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res[0].Status != run.TargetSuccess {
		t.Fatalf("status = %s (msg %q), want success", res[0].Status, res[0].Message)
	}
	release := base + "/releases/" + runID
	for _, want := range []struct {
		path string
		body []byte
	}{
		{release + "/index.html", indexBody},
		{release + "/assets/app.js", jsBody},
	} {
		got, ok := tgt.uploads[want.path]
		if !ok {
			t.Fatalf("未按清单上传 %s;uploads=%v", want.path, keysOf(tgt.uploads))
		}
		if !bytes.Equal(got, want.body) {
			t.Fatalf("%s 上传的不是真字节", want.path)
		}
	}
	// 不应再出现「缺少 files 清单」这类因类型断言失败导致的降级文案。
	if strings.Contains(res[0].Message, "缺少 files 清单") {
		t.Fatalf("清单应被正确解出:%s", res[0].Message)
	}
}

// 清单缺失(非 artifactPackMode=none 产物却标了 format=files)→ 该机 failed + 人读原因,不静默。
func TestDeployStoredFilesManifestMissingFails(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	store, _ := artifactstore.New(t.TempDir())
	key, _, _ := store.Put(bytes.NewReader([]byte("x")))

	tgt := &stubTarget{}
	srv := seedServer(t, tgt, "web-1")
	runID, artID := seedSuccessRunWithStoredArtifact(t, db, rsvc, run.ArtifactDist, key,
		map[string]any{"stored": true, "format": "files"}) // 故意不给 files 清单

	svc := New(tgt, rsvc, WithArtifactStore(store))
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srv.ID},
		Config: map[string]string{"releaseBase": "/srv/web-x"},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res[0].Status == run.TargetSuccess {
		t.Fatalf("清单缺失不应 success: %+v", res[0])
	}
	if !strings.Contains(res[0].Message, "缺少 files 清单") {
		t.Fatalf("message 应说明缺少清单:%s", res[0].Message)
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
