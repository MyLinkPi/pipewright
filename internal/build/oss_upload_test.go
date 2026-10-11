package build

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/artifactstore"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/run"
)

// fakeUploader 记录上传内容(objectKey → 字节)。
type fakeUploader struct {
	got map[string]string
	err error
}

func (u *fakeUploader) Upload(_ context.Context, providerID, objectKey string, body io.Reader) error {
	if u.err != nil {
		return u.err
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if u.got == nil {
		u.got = map[string]string{}
	}
	u.got[providerID+"|"+objectKey] = string(b)
	return nil
}

func newArtStore(t *testing.T) *artifactstore.Store {
	t.Helper()
	s, err := artifactstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("artifactstore.New: %v", err)
	}
	return s
}

func ossTestRun() *run.Run {
	return &run.Run{
		ID:          "run-1",
		ProjectName: "proj",
		Trigger:     run.Trigger{Branch: "main", Commit: "abcdef1234"},
	}
}

func ossTestJob(cfg map[string]any) pipeline.Job {
	return pipeline.Job{ID: "oss", Name: "上传 OSS", Type: "oss_upload", Config: cfg}
}

// --- 单文件产物(file):按 prefix 拼接对象键上传 ---

func TestRunOssUploadSingleFile(t *testing.T) {
	store := newArtStore(t)
	key, _, err := store.Put(strings.NewReader("jar-bytes"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	art := run.Artifact{
		ID: "a1", Type: run.ArtifactJar, Name: "app.jar", Reference: key,
		Metadata: map[string]any{"stored": true, "format": "file", "filename": "app.jar", "sourceJob": "build"},
	}
	up := &fakeUploader{}
	b := &Builder{
		artStore:       store,
		artifactLister: func(context.Context, string) ([]run.Artifact, error) { return []run.Artifact{art}, nil },
		objectUploader: up,
	}
	rep := &fakeReporter{}
	jb := ossTestJob(map[string]any{"storageId": "os-1", "artifactJob": "build", "prefix": "releases/{{runId}}"})

	if err := b.runOssUploadJob(context.Background(), rep, jb, ossTestRun()); err != nil {
		t.Fatalf("runOssUploadJob: %v", err)
	}
	if got := up.got["os-1|releases/run-1/app.jar"]; got != "jar-bytes" {
		t.Fatalf("上传对象键/内容不符: %+v", up.got)
	}
}

// --- format=files:按清单逐文件上传,保留相对路径 ---

func TestRunOssUploadFilesManifest(t *testing.T) {
	store := newArtStore(t)
	k1, _, _ := store.Put(strings.NewReader("A"))
	k2, _, _ := store.Put(strings.NewReader("B"))
	art := run.Artifact{
		ID: "a1", Type: run.ArtifactDist, Name: "web", Reference: "unused",
		Metadata: map[string]any{
			"stored": true, "format": "files", "sourceJob": "build",
			"files": []any{
				map[string]any{"path": "dist/a.js", "key": k1, "size": 1},
				map[string]any{"path": "dist/b.js", "key": k2, "size": 1},
			},
		},
	}
	up := &fakeUploader{}
	b := &Builder{
		artStore:       store,
		artifactLister: func(context.Context, string) ([]run.Artifact, error) { return []run.Artifact{art}, nil },
		objectUploader: up,
	}
	rep := &fakeReporter{}
	jb := ossTestJob(map[string]any{"storageId": "os-1", "artifactJob": "build", "prefix": "rel/1"})

	if err := b.runOssUploadJob(context.Background(), rep, jb, ossTestRun()); err != nil {
		t.Fatalf("runOssUploadJob: %v", err)
	}
	if up.got["os-1|rel/1/dist/a.js"] != "A" || up.got["os-1|rel/1/dist/b.js"] != "B" {
		t.Fatalf("多文件上传不符: %+v", up.got)
	}
}

// --- 未选 storageId → 强失败 ---

func TestRunOssUploadMissingStorageID(t *testing.T) {
	b := &Builder{objectUploader: &fakeUploader{}}
	rep := &fakeReporter{}
	jb := ossTestJob(map[string]any{"artifactJob": "build"})
	if err := b.runOssUploadJob(context.Background(), rep, jb, ossTestRun()); !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("缺 storageId 应 ErrBuildFailed, got %v", err)
	}
}

// --- 来源节点无产物 → 强失败 ---

func TestRunOssUploadNoArtifact(t *testing.T) {
	store := newArtStore(t)
	b := &Builder{
		artStore:       store,
		artifactLister: func(context.Context, string) ([]run.Artifact, error) { return nil, nil },
		objectUploader: &fakeUploader{},
	}
	rep := &fakeReporter{}
	jb := ossTestJob(map[string]any{"storageId": "os-1", "artifactJob": "build"})
	if err := b.runOssUploadJob(context.Background(), rep, jb, ossTestRun()); !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("无产物应 ErrBuildFailed, got %v", err)
	}
}

// --- 上传失败 → 强失败 ---

func TestRunOssUploadUploaderError(t *testing.T) {
	store := newArtStore(t)
	key, _, _ := store.Put(strings.NewReader("x"))
	art := run.Artifact{
		ID: "a1", Type: run.ArtifactJar, Name: "app.jar", Reference: key,
		Metadata: map[string]any{"stored": true, "format": "file", "filename": "app.jar", "sourceJob": "build"},
	}
	b := &Builder{
		artStore:       store,
		artifactLister: func(context.Context, string) ([]run.Artifact, error) { return []run.Artifact{art}, nil },
		objectUploader: &fakeUploader{err: errors.New("AccessDenied")},
	}
	rep := &fakeReporter{}
	jb := ossTestJob(map[string]any{"storageId": "os-1", "artifactJob": "build"})
	if err := b.runOssUploadJob(context.Background(), rep, jb, ossTestRun()); !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("上传失败应 ErrBuildFailed, got %v", err)
	}
}

// --- 对象存储服务未注入 → 跳过(向后兼容) ---

func TestRunOssUploadNoServiceSkips(t *testing.T) {
	b := &Builder{}
	rep := &fakeReporter{}
	jb := ossTestJob(map[string]any{"storageId": "os-1", "artifactJob": "build"})
	if err := b.runOssUploadJob(context.Background(), rep, jb, ossTestRun()); err != nil {
		t.Fatalf("未注入应放行, got %v", err)
	}
}

// --- prefix/对象键拼接与占位渲染 ---

func TestJoinObjectKeyAndRenderPrefix(t *testing.T) {
	if got := joinObjectKey("", "a.js"); got != "a.js" {
		t.Fatalf("空前缀: %q", got)
	}
	if got := joinObjectKey("/rel/1/", "/dist/a.js"); got != "rel/1/dist/a.js" {
		t.Fatalf("斜杠归一: %q", got)
	}
	r := ossTestRun()
	if got := renderRunPlaceholders("releases/{{runId}}/{{branch}}", r); got != "releases/run-1/main" {
		t.Fatalf("占位渲染: %q", got)
	}
	if got := renderRunPlaceholders("plain/path", r); got != "plain/path" {
		t.Fatalf("无占位应原样: %q", got)
	}
}
