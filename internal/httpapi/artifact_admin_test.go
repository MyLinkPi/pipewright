// artifact_admin_test.go 覆盖产物管理面:跨运行列表(筛选/分页/错误码)、删除
// (终态护栏 / blob 回收 / 镜像联动删内置 registry tag / registry 失败中止)、镜像 tar 导出。
package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/artifactstore"
	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// fakeArtifactRegistryHub 是产物管理测试用的 registryhub fake:只实现 ResolveBuiltin /
// DeleteTag(其余方法嵌入 nil 接口,调用即 panic——测试路径绝不触碰)。
type fakeArtifactRegistryHub struct {
	registryHubService
	addr        string
	enabled     bool
	deletedTags []string // "repo:tag"
	failDelete  bool
}

func (f *fakeArtifactRegistryHub) ResolveBuiltin(context.Context) (string, bool) {
	return f.addr, f.enabled
}

func (f *fakeArtifactRegistryHub) DeleteTag(_ context.Context, repo, tag string) (bool, error) {
	if f.failDelete {
		return false, io.ErrUnexpectedEOF
	}
	f.deletedTags = append(f.deletedTags, repo+":"+tag)
	return true, nil
}

// fakeArtifactImageExporter 是镜像导出 fake:记录 EnsureLocal/ExportStream 收到的 ref,
// 导出固定字节;可注入 EnsureLocal 失败。
type fakeArtifactImageExporter struct {
	ensureRefs []string
	ensureErr  error
	exportRefs []string
	exportData []byte
}

func (f *fakeArtifactImageExporter) EnsureLocal(_ context.Context, ref string) error {
	f.ensureRefs = append(f.ensureRefs, ref)
	return f.ensureErr
}

func (f *fakeArtifactImageExporter) ExportStream(_ context.Context, ref string, w io.Writer) error {
	f.exportRefs = append(f.exportRefs, ref)
	_, err := w.Write(f.exportData)
	return err
}

// setupArtifactAdminServer 起一个带产物管理依赖(制品库/fake registry/fake 导出器)的
// 测试服务:登录 admin、建一个项目。运行不入队(保持 queued 由测试直接置终态)。
// 返回值末位的 db 供测试直改运行状态。
func setupArtifactAdminServer(t *testing.T, regHub *fakeArtifactRegistryHub, exporter *fakeArtifactImageExporter) (
	*httptest.Server, *http.Client, string, string, run.Service, *artifactstore.Store, *sql.DB) {
	t.Helper()
	st := testStoreAuth(t)
	svc := auth.NewService(st.DB, nil)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	v := vault.New(st.DB, testMasterKey())
	psvc := project.New(st.DB, v, stubProber{branch: "main"})
	rsvc := run.New(st.DB)
	artStore, err := artifactstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("artifactstore: %v", err)
	}

	opts := []Option{WithVault(v), WithProjects(psvc), WithRuns(rsvc, nil), WithArtifactStore(artStore)}
	if regHub != nil {
		opts = append(opts, WithRegistryHub(regHub))
	}
	if exporter != nil {
		opts = append(opts, WithArtifactImageTransfer(exporter))
	}
	srv := httptest.NewServer(New(testWebFSAuth(), svc, opts...))
	t.Cleanup(srv.Close)

	client := newTestClient(t)
	csrf := loginWithClient(t, client, srv.URL)

	credID := newGitCred(t, client, srv.URL, csrf, "ghp_token_for_artifacts")
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/projects", csrf,
		`{"name":"acme","repoUrl":"https://gitee.com/acme/shop.git","credentialId":"`+credID+`"}`)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	projID, _ := p["id"].(string)
	if projID == "" {
		t.Fatalf("create project failed: %s", raw)
	}
	return srv, client, csrf, projID, rsvc, artStore, st.DB
}

// newFinishedArtifactRun 建一个运行并直接置为 success(不入队执行)。
func newFinishedArtifactRun(t *testing.T, rsvc run.Service, db *sql.DB, projID string) *run.Run {
	t.Helper()
	r, err := rsvc.Create(context.Background(), projID, run.Trigger{Type: run.TriggerManual, Branch: "main"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := db.Exec(`UPDATE pipeline_runs SET status = ? WHERE id = ?`, run.StatusSuccess, r.ID); err != nil {
		t.Fatalf("置终态 success: %v", err)
	}
	return r
}

// artifactAdminList 解析列表响应。
func artifactAdminList(t *testing.T, client *http.Client, url string) (int, map[string]any) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	return resp.StatusCode, body
}

func TestArtifactAdminListAndFilters(t *testing.T) {
	srv, client, _, projID, rsvc, _, db := setupArtifactAdminServer(t, nil, nil)

	r1 := newFinishedArtifactRun(t, rsvc, db, projID)
	r2 := newFinishedArtifactRun(t, rsvc, db, projID)
	if _, err := rsvc.AddArtifact(context.Background(), run.Artifact{
		RunID: r1.ID, Type: run.ArtifactImage, Name: "api", Reference: "reg.local:5000/acme/api:v1",
		Metadata: map[string]any{"digest": "sha256:abc"},
	}); err != nil {
		t.Fatalf("AddArtifact(image): %v", err)
	}
	if _, err := rsvc.AddArtifact(context.Background(), run.Artifact{
		RunID: r2.ID, Type: run.ArtifactJar, Name: "worker", Reference: "worker.jar",
		SizeBytes: 1024, Metadata: map[string]any{"stored": true},
	}); err != nil {
		t.Fatalf("AddArtifact(jar): %v", err)
	}

	// 全量:2 条 + 关联字段。
	code, body := artifactAdminList(t, client, srv.URL+"/api/artifacts")
	if code != http.StatusOK {
		t.Fatalf("list status = %d: %v", code, body)
	}
	if body["total"].(float64) != 2 || body["page"].(float64) != 1 {
		t.Fatalf("total/page 不符: %v", body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("应 2 条, got %d", len(items))
	}
	sawImage := false
	for _, it := range items {
		m, _ := it.(map[string]any)
		for _, k := range []string{"id", "type", "name", "reference", "runId", "projectId", "projectName", "runStatus"} {
			if _, ok := m[k]; !ok {
				t.Fatalf("列表行缺字段 %q: %v", k, m)
			}
		}
		if m["type"] == "image" {
			sawImage = true
			if m["runStatus"] != run.StatusSuccess || m["projectId"] != projID {
				t.Fatalf("image 行关联字段不符: %v", m)
			}
		}
	}
	if !sawImage {
		t.Fatalf("应有 image 产物: %v", items)
	}

	// 类型过滤。
	code, body = artifactAdminList(t, client, srv.URL+"/api/artifacts?type=jar")
	if code != http.StatusOK || body["total"].(float64) != 1 {
		t.Fatalf("type=jar 应 1 条: %d %v", code, body)
	}
	// 非法类型 → 400。
	code, _ = artifactAdminList(t, client, srv.URL+"/api/artifacts?type=tarball")
	if code != http.StatusBadRequest {
		t.Fatalf("非法 type 应 400, got %d", code)
	}
	// 非法分页 → 400。
	code, _ = artifactAdminList(t, client, srv.URL+"/api/artifacts?page=0")
	if code != http.StatusBadRequest {
		t.Fatalf("page=0 应 400, got %d", code)
	}
	code, _ = artifactAdminList(t, client, srv.URL+"/api/artifacts?pageSize=999")
	if code != http.StatusBadRequest {
		t.Fatalf("pageSize=999 应 400, got %d", code)
	}
	// 分页:pageSize=1 第 2 页。
	code, body = artifactAdminList(t, client, srv.URL+"/api/artifacts?page=2&pageSize=1")
	if code != http.StatusOK || body["page"].(float64) != 2 || len(body["items"].([]any)) != 1 {
		t.Fatalf("分页第 2 页不符: %d %v", code, body)
	}
}

func TestArtifactAdminDeleteStoredBlob(t *testing.T) {
	srv, client, csrf, projID, rsvc, artStore, db := setupArtifactAdminServer(t, nil, nil)
	r := newFinishedArtifactRun(t, rsvc, db, projID)

	key, _, err := artStore.Put(strings.NewReader("hello-artifact-bytes"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	a, err := rsvc.AddArtifact(context.Background(), run.Artifact{
		RunID: r.ID, Type: run.ArtifactJar, Name: "app.jar", Reference: key,
		SizeBytes: 21, Metadata: map[string]any{"stored": true, "filename": "app.jar"},
	})
	if err != nil {
		t.Fatalf("AddArtifact: %v", err)
	}

	resp := doJSON(t, client, http.MethodDelete, srv.URL+"/api/runs/"+r.ID+"/artifacts/"+a.ID, csrf, "")
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d: %s", resp.StatusCode, raw)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	if body["deleted"] != true || body["blobsRemoved"].(float64) != 1 {
		t.Fatalf("删除响应不符: %v", body)
	}
	// 记录已删 + blob 物理文件已删。
	if arts, _ := rsvc.ListArtifacts(context.Background(), r.ID); len(arts) != 0 {
		t.Fatalf("产物行应已删除")
	}
	if artStore.Has(key) {
		t.Fatalf("无引用 blob 应被物理删除")
	}
}

func TestArtifactAdminDeleteGuards(t *testing.T) {
	srv, client, csrf, projID, rsvc, _, db := setupArtifactAdminServer(t, nil, nil)

	// 进行中(queued)运行 → 409。
	rLive, err := rsvc.Create(context.Background(), projID, run.Trigger{Type: run.TriggerManual})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	aLive, err := rsvc.AddArtifact(context.Background(), run.Artifact{
		RunID: rLive.ID, Type: run.ArtifactJar, Name: "live", Reference: "live.jar",
	})
	if err != nil {
		t.Fatalf("AddArtifact: %v", err)
	}
	resp := doJSON(t, client, http.MethodDelete, srv.URL+"/api/runs/"+rLive.ID+"/artifacts/"+aLive.ID, csrf, "")
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("进行中运行应 409, got %d: %s", resp.StatusCode, raw)
	}
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	if e.Error.Code != "run_not_finished" {
		t.Fatalf("错误码应 run_not_finished, got %q", e.Error.Code)
	}

	// 缺产物 → 404;缺 run → 404。
	rFin := newFinishedArtifactRun(t, rsvc, db, projID)
	resp = doJSON(t, client, http.MethodDelete, srv.URL+"/api/runs/"+rFin.ID+"/artifacts/no-such", csrf, "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("缺产物应 404, got %d", resp.StatusCode)
	}
	resp = doJSON(t, client, http.MethodDelete, srv.URL+"/api/runs/no-such-run/artifacts/x", csrf, "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("缺 run 应 404, got %d", resp.StatusCode)
	}
}

func TestArtifactAdminDeleteImageRegistry(t *testing.T) {
	regHub := &fakeArtifactRegistryHub{addr: "reg.local:5000", enabled: true}
	srv, client, csrf, projID, rsvc, _, db := setupArtifactAdminServer(t, regHub, nil)
	r := newFinishedArtifactRun(t, rsvc, db, projID)

	// 内置 registry 镜像 → 联动删 tag(repo 含路径、tag 正确拆分)。
	aBuiltin, err := rsvc.AddArtifact(context.Background(), run.Artifact{
		RunID: r.ID, Type: run.ArtifactImage, Name: "api", Reference: "reg.local:5000/acme/api:v1",
	})
	if err != nil {
		t.Fatalf("AddArtifact: %v", err)
	}
	resp := doJSON(t, client, http.MethodDelete, srv.URL+"/api/runs/"+r.ID+"/artifacts/"+aBuiltin.ID, csrf, "")
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d: %s", resp.StatusCode, raw)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	if body["registryTagDeleted"] != true {
		t.Fatalf("应联动删 tag: %v", body)
	}
	if len(regHub.deletedTags) != 1 || regHub.deletedTags[0] != "acme/api:v1" {
		t.Fatalf("DeleteTag 参数不符: %v", regHub.deletedTags)
	}

	// 外置 registry 镜像 → 不动 registry,只删记录。
	aExternal, err := rsvc.AddArtifact(context.Background(), run.Artifact{
		RunID: r.ID, Type: run.ArtifactImage, Name: "ext", Reference: "docker.io/library/nginx:1.27",
	})
	if err != nil {
		t.Fatalf("AddArtifact: %v", err)
	}
	resp = doJSON(t, client, http.MethodDelete, srv.URL+"/api/runs/"+r.ID+"/artifacts/"+aExternal.ID, csrf, "")
	raw, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("外置镜像删除应 200, got %d: %s", resp.StatusCode, raw)
	}
	body = map[string]any{}
	_ = json.Unmarshal(raw, &body)
	if body["registryTagDeleted"] != false {
		t.Fatalf("外置镜像不应动 registry: %v", body)
	}
	if len(regHub.deletedTags) != 1 {
		t.Fatalf("DeleteTag 不应被再次调用: %v", regHub.deletedTags)
	}
	if arts, _ := rsvc.ListArtifacts(context.Background(), r.ID); len(arts) != 0 {
		t.Fatalf("产物行应已删除")
	}
}

func TestArtifactAdminDeleteImageRegistryFailure(t *testing.T) {
	regHub := &fakeArtifactRegistryHub{addr: "reg.local:5000", enabled: true, failDelete: true}
	srv, client, csrf, projID, rsvc, _, db := setupArtifactAdminServer(t, regHub, nil)
	r := newFinishedArtifactRun(t, rsvc, db, projID)

	a, err := rsvc.AddArtifact(context.Background(), run.Artifact{
		RunID: r.ID, Type: run.ArtifactImage, Name: "api", Reference: "reg.local:5000/acme/api:v2",
	})
	if err != nil {
		t.Fatalf("AddArtifact: %v", err)
	}
	resp := doJSON(t, client, http.MethodDelete, srv.URL+"/api/runs/"+r.ID+"/artifacts/"+a.ID, csrf, "")
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("registry 删除失败应 502, got %d: %s", resp.StatusCode, raw)
	}
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	if e.Error.Code != "registry_delete_failed" {
		t.Fatalf("错误码应 registry_delete_failed, got %q", e.Error.Code)
	}
	// 记录不动(可重试)。
	if arts, _ := rsvc.ListArtifacts(context.Background(), r.ID); len(arts) != 1 {
		t.Fatalf("registry 失败时产物行不应被删")
	}
}

func TestArtifactAdminImageArchive(t *testing.T) {
	exporter := &fakeArtifactImageExporter{exportData: []byte("FAKE-IMAGE-TAR")}
	srv, client, csrf, projID, rsvc, _, db := setupArtifactAdminServer(t, nil, exporter)
	r := newFinishedArtifactRun(t, rsvc, db, projID)

	aImg, err := rsvc.AddArtifact(context.Background(), run.Artifact{
		RunID: r.ID, Type: run.ArtifactImage, Name: "api", Reference: "reg.local:5000/acme/api:v3",
	})
	if err != nil {
		t.Fatalf("AddArtifact: %v", err)
	}
	aJar, err := rsvc.AddArtifact(context.Background(), run.Artifact{
		RunID: r.ID, Type: run.ArtifactJar, Name: "app", Reference: "app.jar",
	})
	if err != nil {
		t.Fatalf("AddArtifact: %v", err)
	}

	// 非镜像 → 400。
	resp := doJSON(t, client, http.MethodGet, srv.URL+"/api/runs/"+r.ID+"/artifacts/"+aJar.ID+"/image-archive", csrf, "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非镜像应 400, got %d", resp.StatusCode)
	}

	// 镜像 → 200 + 流内容 + 文件名。
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/api/runs/"+r.ID+"/artifacts/"+aImg.ID+"/image-archive", csrf, "")
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("镜像导出应 200, got %d: %s", resp.StatusCode, raw)
	}
	if string(raw) != "FAKE-IMAGE-TAR" {
		t.Fatalf("导出内容不符: %q", raw)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "api.tar") {
		t.Fatalf("Content-Disposition 不符: %q", cd)
	}
	if len(exporter.ensureRefs) != 1 || exporter.ensureRefs[0] != "reg.local:5000/acme/api:v3" {
		t.Fatalf("EnsureLocal ref 不符: %v", exporter.ensureRefs)
	}
	if len(exporter.exportRefs) != 1 {
		t.Fatalf("ExportStream 应被调用一次: %v", exporter.exportRefs)
	}

	// EnsureLocal 失败 → 502。
	exporter.ensureErr = io.ErrUnexpectedEOF
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/api/runs/"+r.ID+"/artifacts/"+aImg.ID+"/image-archive", csrf, "")
	raw, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("EnsureLocal 失败应 502, got %d: %s", resp.StatusCode, raw)
	}
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	if e.Error.Code != "image_export_failed" {
		t.Fatalf("错误码应 image_export_failed, got %q", e.Error.Code)
	}
}
