// artifact_transfer_test.go 覆盖产物跨环境传输:可传输性判定、Start 同步校验
// (未知 run/产物、format=files 拒绝、peer 未组网、并发上限)、文件/镜像成功路径
// (清单组装与 blob 内容)、对端 403 → 人读错误、取消语义。
package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/artifactstore"
	"github.com/huangchengsir/pipewright/internal/mesh"
	"github.com/huangchengsir/pipewright/internal/run"
)

// fakeTransferPeers 实现meshPeerLookup:单一对端 + 固定本节点 id。
type fakeTransferPeers struct {
	peer   *mesh.Peer
	nodeID string
}

func (f *fakeTransferPeers) GetPeer(context.Context, string) (*mesh.Peer, error) { return f.peer, nil }
func (f *fakeTransferPeers) NodeID(context.Context) (string, error)              { return f.nodeID, nil }

// fakeTransferRuns 实现transferRunSource:固定 run + 产物清单。
type fakeTransferRuns struct {
	r    *run.Run
	arts []run.Artifact
}

func (f *fakeTransferRuns) Get(context.Context, string) (*run.Run, error) { return f.r, nil }
func (f *fakeTransferRuns) ListArtifacts(context.Context, string) ([]run.Artifact, error) {
	return f.arts, nil
}

// fakePushClient 实现pushClient:preflight 回固定缺失清单;Push 捕获清单与 blob 内容;
// 可注入 preflight/Push 错误,block 非空时 Push 阻塞到 close 或 ctx 完成(取消测试)。
type fakePushClient struct {
	missing []string
	doErr   error
	pushErr error
	result  *mesh.PushResult
	block   chan struct{}

	gotManifest mesh.PushManifest
	gotBlobs    map[string][]byte
}

func (f *fakePushClient) Do(_ context.Context, _ *mesh.Peer, _, _ string, _ any, out any) error {
	if f.doErr != nil {
		return f.doErr
	}
	if pf, ok := out.(*pushPreflightResult); ok {
		pf.Missing = f.missing
	}
	return nil
}

func (f *fakePushClient) Push(ctx context.Context, _ *mesh.Peer, manifest mesh.PushManifest, blobs map[string]io.Reader) (*mesh.PushResult, error) {
	f.gotManifest = manifest
	f.gotBlobs = map[string][]byte{}
	for k, r := range blobs {
		b, _ := io.ReadAll(r)
		f.gotBlobs[k] = b
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.pushErr != nil {
		return nil, f.pushErr
	}
	return f.result, nil
}

// fakeTransferImages 实现pushImageExporter:Export 落固定字节。
type fakeTransferImages struct {
	ensureRefs []string
	exportRefs []string
	data       []byte
}

func (f *fakeTransferImages) EnsureLocal(_ context.Context, ref string) error {
	f.ensureRefs = append(f.ensureRefs, ref)
	return nil
}

func (f *fakeTransferImages) Export(_ context.Context, ref, destPath string) error {
	f.exportRefs = append(f.exportRefs, ref)
	return os.WriteFile(destPath, f.data, 0o644)
}

func testTransferPeer() *mesh.Peer {
	return &mesh.Peer{NodeID: "peer-1", Name: "prod", VirtualIP: "10.8.0.2", APIPort: 8080, PublicURL: "https://prod.example.com"}
}

func testTransferRun() *run.Run {
	return &run.Run{ID: "run-1", ProjectID: "proj-1", ProjectName: "acme"}
}

// newTestTransferService 构造指向 fake 依赖的服务(真实制品库落在临时目录);默认 runs
// 为空产物清单,测试存好 blob 后经 svc.runs 覆写。
func newTestTransferService(t *testing.T, fc *fakePushClient, imgs *fakeTransferImages) (*ArtifactTransferService, *artifactstore.Store) {
	t.Helper()
	store, err := artifactstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("artifactstore: %v", err)
	}
	svc := &ArtifactTransferService{
		peers:  &fakeTransferPeers{peer: testTransferPeer(), nodeID: "node-self"},
		client: fc,
		store:  store,
		runs:   &fakeTransferRuns{r: testTransferRun()},
		items:  map[string]*transferEntry{},
	}
	if imgs != nil { // 类型化 nil 赋给接口会得到非 nil 接口值,须判空后赋值
		svc.images = imgs
	}
	return svc, store
}

// storeTestBlob 往服务所用的制品库写入内容,返回句柄与大小。
func storeTestBlob(t *testing.T, store *artifactstore.Store, content string) (string, int64) {
	t.Helper()
	key, size, err := store.Put(strings.NewReader(content))
	if err != nil {
		t.Fatalf("store.Put: %v", err)
	}
	return key, size
}

// waitTransferTerminal 轮询任务到终态(3s 上限)。
func waitTransferTerminal(t *testing.T, s *ArtifactTransferService, id string) *TransferStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := s.Get(id); ok && st.Status != TransferRunning {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("传输未在时限内到终态")
	return nil
}

func TestTransferCheck(t *testing.T) {
	cases := []struct {
		name    string
		art     run.Artifact
		ok      bool
		wantSub string // ok=false 时 reason 应包含的子串
	}{
		{name: "镜像有引用", art: run.Artifact{Type: run.ArtifactImage, Reference: "reg/app:1"}, ok: true},
		{name: "镜像无引用", art: run.Artifact{Type: run.ArtifactImage}, wantSub: "无镜像引用"},
		{name: "jar已归档", art: run.Artifact{Type: run.ArtifactJar, Reference: "ab", Metadata: map[string]any{"stored": true, "format": "file"}}, ok: true},
		{name: "未归档占位", art: run.Artifact{Type: run.ArtifactJar, Name: "app.jar", Reference: "app.jar"}, wantSub: "未归档"},
		{name: "多文件清单", art: run.Artifact{Type: run.ArtifactDist, Reference: "ab", Metadata: map[string]any{"stored": true, "format": "files"}}, wantSub: "format=files"},
		{name: "未知类型", art: run.Artifact{Type: "weird"}, wantSub: "未知产物类型"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := transferCheck(tc.art)
			if ok != tc.ok {
				t.Fatalf("transferCheck ok = %v, want %v(reason %s)", ok, tc.ok, reason)
			}
			if !ok && !strings.Contains(reason, tc.wantSub) {
				t.Fatalf("reason %q 不含 %q", reason, tc.wantSub)
			}
		})
	}
}

func TestArtifactTransferStartValidation(t *testing.T) {
	key := strings.Repeat("a", 64)
	runsFake := &fakeTransferRuns{r: testTransferRun(), arts: []run.Artifact{
		{ID: "a1", Type: run.ArtifactJar, Name: "app.jar", Reference: key, Metadata: map[string]any{"stored": true, "format": "file"}},
		{ID: "a2", Type: run.ArtifactDist, Name: "web", Reference: key, Metadata: map[string]any{"stored": true, "format": "files"}},
		{ID: "a3", Type: run.ArtifactJar, Name: "stub.jar", Reference: "stub.jar"},
	}}

	t.Run("未知产物", func(t *testing.T) {
		svc, _ := newTestTransferService(t, &fakePushClient{}, nil)
		svc.runs = runsFake
		_, err := svc.Start(context.Background(), TransferInput{RunID: "run-1", PeerNodeID: "peer-1", ArtifactIDs: []string{"nope"}})
		if !errors.Is(err, run.ErrArtifactNotFound) {
			t.Fatalf("err = %v, want ErrArtifactNotFound", err)
		}
	})
	t.Run("format=files 拒绝", func(t *testing.T) {
		svc, _ := newTestTransferService(t, &fakePushClient{}, nil)
		svc.runs = runsFake
		_, err := svc.Start(context.Background(), TransferInput{RunID: "run-1", PeerNodeID: "peer-1", ArtifactIDs: []string{"a2"}})
		if !errors.Is(err, ErrArtifactNotTransferable) {
			t.Fatalf("err = %v, want ErrArtifactNotTransferable", err)
		}
		if msg := err.Error(); !strings.Contains(msg, "web") || !strings.Contains(msg, "format=files") {
			t.Fatalf("错误文本应含产物名与原因:%s", msg)
		}
	})
	t.Run("未归档拒绝", func(t *testing.T) {
		svc, _ := newTestTransferService(t, &fakePushClient{}, nil)
		svc.runs = runsFake
		_, err := svc.Start(context.Background(), TransferInput{RunID: "run-1", PeerNodeID: "peer-1", ArtifactIDs: []string{"a3"}})
		if !errors.Is(err, ErrArtifactNotTransferable) {
			t.Fatalf("err = %v, want ErrArtifactNotTransferable", err)
		}
	})
	t.Run("镜像产物无导出器", func(t *testing.T) {
		svc, _ := newTestTransferService(t, &fakePushClient{}, nil)
		svc.runs = &fakeTransferRuns{r: testTransferRun(), arts: []run.Artifact{
			{ID: "i1", Type: run.ArtifactImage, Name: "app", Reference: "reg/app:1"},
		}}
		_, err := svc.Start(context.Background(), TransferInput{RunID: "run-1", PeerNodeID: "peer-1", ArtifactIDs: []string{"i1"}})
		if !errors.Is(err, ErrTransferUnavailable) {
			t.Fatalf("err = %v, want ErrTransferUnavailable", err)
		}
	})
	t.Run("对端未组网", func(t *testing.T) {
		svc, _ := newTestTransferService(t, &fakePushClient{}, nil)
		svc.runs = runsFake
		svc.peers = &fakeTransferPeers{peer: &mesh.Peer{NodeID: "peer-1", Name: "prod"}, nodeID: "node-self"}
		_, err := svc.Start(context.Background(), TransferInput{RunID: "run-1", PeerNodeID: "peer-1", ArtifactIDs: []string{"a1"}})
		if !errors.Is(err, ErrTransferPeerNotNetworked) {
			t.Fatalf("err = %v, want ErrTransferPeerNotNetworked", err)
		}
	})
	t.Run("并发上限", func(t *testing.T) {
		svc, _ := newTestTransferService(t, &fakePushClient{}, nil)
		svc.runs = runsFake
		for i := 0; i < maxRunningTransfers; i++ {
			id := fmt.Sprintf("fill-%d", i)
			svc.items[id] = &transferEntry{status: TransferStatus{ID: id, Status: TransferRunning}}
			svc.order = append(svc.order, id)
		}
		if _, err := svc.Start(context.Background(), TransferInput{RunID: "run-1", PeerNodeID: "peer-1", ArtifactIDs: []string{"a1"}}); !errors.Is(err, ErrTransferBusy) {
			t.Fatalf("err = %v, want ErrTransferBusy", err)
		}
	})
	t.Run("依赖未装配", func(t *testing.T) {
		svc, _ := newTestTransferService(t, &fakePushClient{}, nil)
		svc.runs = runsFake
		svc.client = nil
		if _, err := svc.Start(context.Background(), TransferInput{RunID: "run-1", PeerNodeID: "peer-1", ArtifactIDs: []string{"a1"}}); !errors.Is(err, ErrTransferUnavailable) {
			t.Fatalf("err = %v, want ErrTransferUnavailable", err)
		}
	})
}

func TestArtifactTransferFileSuccess(t *testing.T) {
	fc := &fakePushClient{result: &mesh.PushResult{ProjectID: "p-remote", RevisionID: "rev-9", Created: true}}
	svc, store := newTestTransferService(t, fc, nil)
	key, size := storeTestBlob(t, store, "hello-jar-bytes")
	fc.missing = []string{key}
	svc.runs = &fakeTransferRuns{r: testTransferRun(), arts: []run.Artifact{
		{ID: "a1", Type: run.ArtifactJar, Name: "app.jar", Reference: key, SizeBytes: size,
			Metadata: map[string]any{"stored": true, "format": "file", "workspacePath": "target/app.jar"}},
	}}

	settled := make(chan *TransferStatus, 1)
	svc.OnSettled(func(st *TransferStatus) { settled <- st })

	id, err := svc.Start(context.Background(), TransferInput{RunID: "run-1", PeerNodeID: "peer-1", ArtifactIDs: []string{"a1"}, RevisionLabel: "promote-1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	st := waitTransferTerminal(t, svc, id)
	if st.Status != TransferSuccess {
		t.Fatalf("status = %s(%s), want success", st.Status, st.Error)
	}
	if st.Result == nil || st.Result.ProjectID != "p-remote" || st.Result.RevisionID != "rev-9" || !st.Result.Created {
		t.Fatalf("result 不符:%+v", st.Result)
	}
	if st.Result.ProjectURL != "https://prod.example.com/projects" {
		t.Fatalf("ProjectURL = %s, want publicUrl 深链", st.Result.ProjectURL)
	}
	// 清单断言:origin 三元组、修订标签、条目字段。
	m := fc.gotManifest
	if m.OriginNodeID != "node-self" || m.OriginProjectRef != "proj-1" || m.OriginProjectName != "acme" || m.OriginRunID != "run-1" {
		t.Fatalf("manifest origin 不符:%+v", m)
	}
	if m.RevisionLabel != "promote-1" || len(m.Items) != 1 {
		t.Fatalf("manifest label/items 不符:%+v", m)
	}
	it := m.Items[0]
	if it.Kind != "file" || it.SHA256 != key || it.Name != "app.jar" || it.DestName != "target/app.jar" || it.Size != size {
		t.Fatalf("item 不符:%+v", it)
	}
	// blob 内容:preflight 报缺失 → Push 收到真字节。
	if string(fc.gotBlobs[key]) != "hello-jar-bytes" {
		t.Fatalf("blob 内容不符:%q", fc.gotBlobs[key])
	}
	// 终态回调触发一次。
	select {
	case cb := <-settled:
		if cb.Status != TransferSuccess {
			t.Fatalf("回调状态 = %s", cb.Status)
		}
	default:
		t.Fatal("OnSettled 未触发")
	}
}

func TestArtifactTransferPreflightDedup(t *testing.T) {
	fc := &fakePushClient{missing: []string{}, result: &mesh.PushResult{ProjectID: "p", RevisionID: "r"}} // 空清单(非 nil)= 对端已持有
	svc, store := newTestTransferService(t, fc, nil)
	key, size := storeTestBlob(t, store, "already-there")
	svc.runs = &fakeTransferRuns{r: testTransferRun(), arts: []run.Artifact{
		{ID: "a1", Type: run.ArtifactArchive, Name: "pkg.tgz", Reference: key, SizeBytes: size,
			Metadata: map[string]any{"stored": true, "format": "tar.gz"}},
	}}
	id, err := svc.Start(context.Background(), TransferInput{RunID: "run-1", PeerNodeID: "peer-1", ArtifactIDs: []string{"a1"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st := waitTransferTerminal(t, svc, id); st.Status != TransferSuccess {
		t.Fatalf("status = %s(%s)", st.Status, st.Error)
	}
	if len(fc.gotBlobs) != 0 {
		t.Fatalf("对端已持有时不应传 blob,实传 %d 个", len(fc.gotBlobs))
	}
	if len(fc.gotManifest.Items) != 1 {
		t.Fatal("清单条目应保留(去重只省字节不省条目)")
	}
}

func TestArtifactTransferImageSuccess(t *testing.T) {
	imgs := &fakeTransferImages{data: []byte("FAKE-TAR-BYTES")}
	fc := &fakePushClient{result: &mesh.PushResult{ProjectID: "p", RevisionID: "r"}}
	svc, _ := newTestTransferService(t, fc, imgs)
	svc.runs = &fakeTransferRuns{r: testTransferRun(), arts: []run.Artifact{
		{ID: "i1", Type: run.ArtifactImage, Name: "app", Reference: "reg.example/app:1",
			Metadata: map[string]any{"digest": "sha256:abc"}},
	}}
	id, err := svc.Start(context.Background(), TransferInput{RunID: "run-1", PeerNodeID: "peer-1", ArtifactIDs: []string{"i1"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st := waitTransferTerminal(t, svc, id); st.Status != TransferSuccess {
		t.Fatalf("status = %s(%s)", st.Status, st.Error)
	}
	if len(fc.gotManifest.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(fc.gotManifest.Items))
	}
	it := fc.gotManifest.Items[0]
	if it.Kind != "image" || it.ImageRef != "reg.example/app:1" || it.Digest != "sha256:abc" {
		t.Fatalf("image item 不符:%+v", it)
	}
	if it.Size != int64(len("FAKE-TAR-BYTES")) {
		t.Fatalf("镜像归档大小不符:%d", it.Size)
	}
	if string(fc.gotBlobs[it.SHA256]) != "FAKE-TAR-BYTES" {
		t.Fatal("镜像归档 blob 内容不符")
	}
	if len(imgs.ensureRefs) != 1 || imgs.ensureRefs[0] != "reg.example/app:1" {
		t.Fatalf("EnsureLocal 未被调用:%v", imgs.ensureRefs)
	}
}

func TestArtifactTransferForbidden(t *testing.T) {
	fc := &fakePushClient{doErr: fmt.Errorf("mesh: peer responded 403: %w", mesh.ErrForbidden)}
	svc, store := newTestTransferService(t, fc, nil)
	key, size := storeTestBlob(t, store, "x")
	svc.runs = &fakeTransferRuns{r: testTransferRun(), arts: []run.Artifact{
		{ID: "a1", Type: run.ArtifactJar, Name: "app.jar", Reference: key, SizeBytes: size,
			Metadata: map[string]any{"stored": true, "format": "file"}},
	}}
	id, err := svc.Start(context.Background(), TransferInput{RunID: "run-1", PeerNodeID: "peer-1", ArtifactIDs: []string{"a1"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	st := waitTransferTerminal(t, svc, id)
	if st.Status != TransferFailed {
		t.Fatalf("status = %s, want failed", st.Status)
	}
	if !strings.Contains(st.Error, "未授权") {
		t.Fatalf("错误文本应含未授权语义:%s", st.Error)
	}
}

func TestArtifactTransferCancel(t *testing.T) {
	fc := &fakePushClient{block: make(chan struct{}), result: &mesh.PushResult{ProjectID: "p", RevisionID: "r"}}
	svc, store := newTestTransferService(t, fc, nil)
	key, size := storeTestBlob(t, store, "y")
	svc.runs = &fakeTransferRuns{r: testTransferRun(), arts: []run.Artifact{
		{ID: "a1", Type: run.ArtifactJar, Name: "app.jar", Reference: key, SizeBytes: size,
			Metadata: map[string]any{"stored": true, "format": "file"}},
	}}
	id, err := svc.Start(context.Background(), TransferInput{RunID: "run-1", PeerNodeID: "peer-1", ArtifactIDs: []string{"a1"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// 等传输进入 Push(阻塞)后取消。
	time.Sleep(50 * time.Millisecond)
	if !svc.Cancel(id) {
		t.Fatal("Cancel 应成功")
	}
	st := waitTransferTerminal(t, svc, id)
	if st.Status != TransferCanceled {
		t.Fatalf("status = %s, want canceled", st.Status)
	}
	if st.Error != "" {
		t.Fatalf("取消态不应带错误文本:%s", st.Error)
	}
	// 终态后再取消 → false。
	if svc.Cancel(id) {
		t.Fatal("终态后 Cancel 应返回 false")
	}
}

func TestArtifactTransferGetUnknown(t *testing.T) {
	svc, _ := newTestTransferService(t, &fakePushClient{}, nil)
	if _, ok := svc.Get("at-nope"); ok {
		t.Fatal("未知 id 应返回 ok=false")
	}
	if svc.Cancel("at-nope") {
		t.Fatal("未知 id Cancel 应返回 false")
	}
}
