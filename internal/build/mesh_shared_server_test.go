package build

// mesh_shared_server_test.go 用 fake 调度器/目标机驱动集群共享构建提供方运行时(0086)的
// 核心语义:受理校验、脚本任务端到端(上机→执行→产物回拉)、取消传播、目录过滤。
import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/mesh"
	"github.com/huangchengsir/pipewright/internal/target"
)

// fakeSharedAcquirer 立即占槽的假调度器(可切换为阻塞以测排队/取消)。
type fakeSharedAcquirer struct {
	mu       sync.Mutex
	blocking bool     // true = AcquireShared 挂起直到 ctx 结束
	acquired []string // 记录 pipelineID
	released int
}

func (f *fakeSharedAcquirer) AcquireShared(ctx context.Context, pipelineID, selector string, log func(string)) (string, string, func(), error) {
	f.mu.Lock()
	f.acquired = append(f.acquired, pipelineID+"|"+selector)
	blocking := f.blocking
	f.mu.Unlock()
	if blocking {
		<-ctx.Done()
		f.mu.Lock()
		f.released++
		f.mu.Unlock()
		return "", "", func() {}, ctx.Err()
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			f.mu.Lock()
			f.released++
			f.mu.Unlock()
		})
	}
	return "srv-1", "shared-host", release, nil
}

// fakeSharedTarget 记录命令/上传并在 Download 时回放假产物归档。
type fakeSharedTarget struct {
	mu        sync.Mutex
	execs     [][]string
	uploads   []string
	downloads []string
	dlBytes   []byte
}

func (f *fakeSharedTarget) Exec(_ context.Context, _ string, cmd []string) (*target.ExecResult, error) {
	f.mu.Lock()
	f.execs = append(f.execs, cmd)
	f.mu.Unlock()
	// CLI 探测命令回 docker,其余命令静默成功(解包/收集/清理)。
	if len(cmd) == 3 && strings.Contains(cmd[2], "command -v") {
		return &target.ExecResult{Stdout: "docker\n", ExitCode: 0}, nil
	}
	return &target.ExecResult{ExitCode: 0}, nil
}

func (f *fakeSharedTarget) Upload(_ context.Context, _ string, _ io.Reader, remotePath string) error {
	f.mu.Lock()
	f.uploads = append(f.uploads, remotePath)
	f.mu.Unlock()
	return nil
}

func (f *fakeSharedTarget) Download(_ context.Context, _ string, remotePath string, w io.Writer) (int64, error) {
	f.mu.Lock()
	f.downloads = append(f.downloads, remotePath)
	dl := f.dlBytes
	f.mu.Unlock()
	if dl == nil {
		dl = tarGzBytesForTest(map[string]string{"out/hello.txt": "built-artifact"})
	}
	return io.Copy(w, bytes.NewReader(dl))
}

// fakeSharedMachines 是目录数据源。
type fakeSharedMachines struct{ srvs []*target.Server }

func (f *fakeSharedMachines) List(context.Context) ([]*target.Server, error) { return f.srvs, nil }

// tarGzBytesForTest 构造一个 tar.gz 字节流(path → content)。
func tarGzBytesForTest(files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))})
		_, _ = tw.Write([]byte(content))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// waitTerminal 轮询任务到终态(超时 fail)。
func waitTerminal(t *testing.T, s *SharedBuildServer, jobID string) mesh.SharedJobStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := s.Status(jobID)
		if err == nil {
			switch st.State {
			case mesh.SharedJobStateSucceeded, mesh.SharedJobStateFailed, mesh.SharedJobStateCanceled:
				return st
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("任务 %s 未在期限内到终态", jobID)
	return mesh.SharedJobStatus{}
}

func newTestSharedServer(sched SharedAcquirer, tgt *fakeSharedTarget, machines SharedMachineLister) *SharedBuildServer {
	return NewSharedBuildServer(&Builder{}, sched, tgt, machines)
}

func TestSharedServerScriptJobEndToEnd(t *testing.T) {
	sched := &fakeSharedAcquirer{}
	tgt := &fakeSharedTarget{}
	s := newTestSharedServer(sched, tgt, &fakeSharedMachines{})
	var events []string
	s.OnEvent = func(action string, st mesh.SharedJobStatus) { events = append(events, action) }

	ws := tarGzBytesForTest(map[string]string{"src/main.go": "package main"})
	spec := &mesh.SharedJobSpec{
		RunID: "run-1", StageName: "build", JobNodeID: "j1", JobName: "编译",
		JobType:        mesh.SharedJobTypeScript,
		Selector:       "linux",
		Image:          "golang:1.22",
		Commands:       []string{"go build ./..."},
		ArtifactPaths:  []string{"out/*"},
		TimeoutMinutes: 0,
	}
	jobID, err := s.Accept(context.Background(), "peer-node-1", "prod", spec, bytes.NewReader(ws))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	st := waitTerminal(t, s, jobID)
	if st.State != mesh.SharedJobStateSucceeded {
		t.Fatalf("state = %s, err = %s;want succeeded", st.State, st.Error)
	}
	if !st.HasResult || st.ServerName != "shared-host" {
		t.Fatalf("HasResult/ServerName = %v/%q", st.HasResult, st.ServerName)
	}

	// 调度器收到了剥掉 peer: 后的选择器与亲和键。
	sched.mu.Lock()
	got := strings.Join(sched.acquired, ";")
	sched.mu.Unlock()
	if !strings.Contains(got, "mesh:peer-node-1:run-1|linux") {
		t.Fatalf("调度器入参 = %q,应含亲和键与选择器 linux", got)
	}

	// 产物归档可回拉,内容是 Download 注入的假归档。
	path, _, size, rerr := s.Result(jobID)
	if rerr != nil || path == "" || size == 0 {
		t.Fatalf("Result = %q/%d/%v", path, size, rerr)
	}

	// 审计钩子:received + succeeded。
	if strings.Join(events, ",") != "received,succeeded" {
		t.Fatalf("事件序列 = %v", events)
	}

	// 订阅(终态后)应回放历史日志并立即收流。
	ch, unsub, serr := s.Subscribe(jobID)
	if serr != nil {
		t.Fatalf("Subscribe: %v", serr)
	}
	defer unsub()
	sawLog := false
	for ev := range ch {
		if ev.Event == "log" {
			sawLog = true
		}
	}
	if !sawLog {
		t.Fatal("终态订阅应回放历史日志")
	}
}

func TestSharedServerAcceptValidation(t *testing.T) {
	s := newTestSharedServer(&fakeSharedAcquirer{}, &fakeSharedTarget{}, &fakeSharedMachines{})
	ws := bytes.NewReader(tarGzBytesForTest(nil))

	if _, err := s.Accept(context.Background(), "p1", "prod", &mesh.SharedJobSpec{JobType: "script"}, ws); !errors.Is(err, ErrSharedSpecInvalid) {
		t.Fatalf("缺选择器应报 spec 非法,got %v", err)
	}
	if _, err := s.Accept(context.Background(), "p1", "prod", &mesh.SharedJobSpec{Selector: "linux"}, ws); !errors.Is(err, ErrSharedSpecInvalid) {
		t.Fatalf("缺任务类型应报 spec 非法,got %v", err)
	}
	if _, err := s.Accept(context.Background(), "p1", "prod", &mesh.SharedJobSpec{Selector: "linux", JobType: mesh.SharedJobTypeScript}, ws); !errors.Is(err, ErrSharedSpecInvalid) {
		t.Fatalf("script 缺 image/commands 应报 spec 非法,got %v", err)
	}
	// sha 不符 → 受理撤销(任务不入册)。
	spec := &mesh.SharedJobSpec{Selector: "linux", JobType: mesh.SharedJobTypeScript, Image: "alpine", Commands: []string{"true"}, WorkspaceSHA256: "deadbeef"}
	if _, err := s.Accept(context.Background(), "p1", "prod", spec, bytes.NewReader(tarGzBytesForTest(nil))); !errors.Is(err, ErrSharedChecksum) {
		t.Fatalf("sha 不符应报 ErrSharedChecksum,got %v", err)
	}
	if jobs := s.List(); len(jobs) != 0 {
		t.Fatalf("校验失败的任务不应入册,got %d", len(jobs))
	}
}

func TestSharedServerCancelQueuedJob(t *testing.T) {
	sched := &fakeSharedAcquirer{blocking: true} // 挂在排队处
	s := newTestSharedServer(sched, &fakeSharedTarget{}, &fakeSharedMachines{})
	spec := &mesh.SharedJobSpec{RunID: "r", JobNodeID: "j", JobType: mesh.SharedJobTypeScript, Selector: "linux", Image: "alpine", Commands: []string{"true"}}
	jobID, err := s.Accept(context.Background(), "p1", "prod", spec, bytes.NewReader(tarGzBytesForTest(nil)))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := s.Cancel(jobID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	st := waitTerminal(t, s, jobID)
	if st.State != mesh.SharedJobStateCanceled {
		t.Fatalf("state = %s,want canceled", st.State)
	}
	// 幂等:终态后再取消是 no-op。
	if err := s.Cancel(jobID); err != nil {
		t.Fatalf("终态 Cancel 应幂等:%v", err)
	}
}

func TestSharedServerCatalogFiltersMachines(t *testing.T) {
	s := newTestSharedServer(&fakeSharedAcquirer{}, &fakeSharedTarget{}, &fakeSharedMachines{srvs: []*target.Server{
		{ID: "a", Name: "shared", Labels: "linux", SharedBuild: true},
		{ID: "b", Name: "plain", Labels: "linux"},
		{ID: "c", Name: "shared-no-labels", SharedBuild: true},
		{ID: "d", Name: "pve", Kind: target.KindPVENode, SharedBuild: true, Labels: "linux"},
	}})
	machines, err := s.CatalogMachines(context.Background())
	if err != nil {
		t.Fatalf("CatalogMachines: %v", err)
	}
	if len(machines) != 1 || machines[0].ServerID != "a" {
		t.Fatalf("目录应只含 a(共享+已打标签+standard),got %+v", machines)
	}
}

func TestSharedServerStatusNotFound(t *testing.T) {
	s := newTestSharedServer(&fakeSharedAcquirer{}, &fakeSharedTarget{}, &fakeSharedMachines{})
	if _, err := s.Status("nope"); !errors.Is(err, ErrSharedJobNotFound) {
		t.Fatalf("未知任务应报 ErrSharedJobNotFound,got %v", err)
	}
}
