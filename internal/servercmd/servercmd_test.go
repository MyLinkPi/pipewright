package servercmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/target"
)

// fakeTarget 是实现 target.Service 的假目标机:按 serverID 路由 Exec 结果/错误。
type fakeTarget struct {
	servers map[string]*target.Server
	exec    func(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error)
}

func (f *fakeTarget) Get(_ context.Context, id string) (*target.Server, error) {
	if s, ok := f.servers[id]; ok {
		return s, nil
	}
	return nil, target.ErrNotFound
}

func (f *fakeTarget) List(context.Context) ([]*target.Server, error) {
	out := make([]*target.Server, 0, len(f.servers))
	for _, s := range f.servers {
		out = append(out, s)
	}
	return out, nil
}

// Exec 先尊重调用方 ctx 的取消/超时(阻塞型 fake 由 exec 闭包自行等待 ctx.Done)。
func (f *fakeTarget) Exec(ctx context.Context, id string, cmd []string) (*target.ExecResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.exec != nil {
		return f.exec(ctx, id, cmd)
	}
	return &target.ExecResult{ExitCode: 0}, nil
}

func (f *fakeTarget) Create(context.Context, target.CreateInput) (*target.Server, error) {
	return nil, nil
}
func (f *fakeTarget) Update(context.Context, string, target.UpdateInput) (*target.Server, error) {
	return nil, nil
}
func (f *fakeTarget) Delete(context.Context, string) error { return nil }
func (f *fakeTarget) Test(context.Context, string) (*target.TestResult, error) {
	return nil, nil
}
func (f *fakeTarget) ExecStream(context.Context, string, []string) (io.ReadCloser, error) {
	return nil, nil
}
func (f *fakeTarget) ExecInteractive(context.Context, string, []string) (target.Session, error) {
	return nil, nil
}

// ExecWithStdin 满足 target.Service 接口(sudo -S 追加);批量命令不用 stdin,转发 Exec 语义。
func (f *fakeTarget) ExecWithStdin(ctx context.Context, id string, cmd []string, _ io.Reader) (*target.ExecResult, error) {
	return f.Exec(ctx, id, cmd)
}
func (f *fakeTarget) Upload(context.Context, string, io.Reader, string) error { return nil }

func newTestService(t *testing.T, ft *fakeTarget) *Service {
	t.Helper()
	st := storetest.Open(t)
	return New(st.DB, ft)
}

func newFakeTarget() *fakeTarget {
	return &fakeTarget{servers: map[string]*target.Server{
		"a": {ID: "a", Name: "web-1"},
		"b": {ID: "b", Name: "web-2"},
	}}
}

// --- Run:成功路径 + 历史落库 ---

func TestRunSuccessAndPersist(t *testing.T) {
	var calls []string
	ft := newFakeTarget()
	ft.exec = func(_ context.Context, id string, cmd []string) (*target.ExecResult, error) {
		calls = append(calls, id)
		if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" {
			t.Errorf("命令应经 sh -c array 形式执行, got %v", cmd)
		}
		return &target.ExecResult{Stdout: "out-" + id, ExitCode: 0}, nil
	}
	svc := newTestService(t, ft)

	res, err := svc.Run(context.Background(), RunInput{Command: " echo ok ", ServerIDs: []string{"a", "b"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Total != 2 || res.OK != 2 || res.Failed != 0 {
		t.Fatalf("计数不符: total=%d ok=%d failed=%d", res.Total, res.OK, res.Failed)
	}
	if res.RunID == "" {
		t.Fatal("runId 应非空")
	}
	if len(calls) != 2 {
		t.Fatalf("应执行 2 台, got %d", len(calls))
	}
	names := map[string]string{"a": "web-1", "b": "web-2"}
	for _, it := range res.Items {
		if !it.OK || it.Stdout != "out-"+it.ServerID || it.ServerName != names[it.ServerID] {
			t.Fatalf("item 不符: %+v", it)
		}
	}

	// 历史落库:列表 1 条,详情逐机含输出。
	runs, err := svc.ListRuns(context.Background(), 0)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns 应 1 条, got %d err=%v", len(runs), err)
	}
	if runs[0].Command != "echo ok" {
		t.Fatalf("命令应去除首尾空白后落库, got %q", runs[0].Command)
	}
	det, err := svc.GetRun(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if len(det.Items) != 2 || det.Items[0].Stdout == "" {
		t.Fatalf("详情应含逐机输出: %+v", det.Items)
	}
}

// --- Run:逐机独立容错(未知 id / 非零退出码 / SSH 失败互不连累) ---

func TestRunPerItemFaultTolerance(t *testing.T) {
	ft := newFakeTarget()
	ft.exec = func(_ context.Context, id string, _ []string) (*target.ExecResult, error) {
		switch id {
		case "a":
			return &target.ExecResult{ExitCode: 0}, nil
		case "b":
			return &target.ExecResult{Stderr: "boom", ExitCode: 3}, nil
		}
		return nil, target.ErrUnreachable
	}
	svc := newTestService(t, ft)

	res, err := svc.Run(context.Background(), RunInput{Command: "x", ServerIDs: []string{"a", "b", "ghost"}})
	if err != nil {
		t.Fatalf("Run 不应整体失败: %v", err)
	}
	if res.Total != 3 || res.OK != 1 || res.Failed != 2 {
		t.Fatalf("计数不符: total=%d ok=%d failed=%d", res.Total, res.OK, res.Failed)
	}
	byID := map[string]ResultItem{}
	for _, it := range res.Items {
		byID[it.ServerID] = it
	}
	if !byID["a"].OK {
		t.Fatalf("a 应成功: %+v", byID["a"])
	}
	if byID["b"].OK || byID["b"].ExitCode != 3 || byID["b"].Stderr != "boom" {
		t.Fatalf("b 应 ok:false + 退出码 3: %+v", byID["b"])
	}
	if byID["ghost"].OK || byID["ghost"].Error == "" || byID["ghost"].ServerName != "" {
		t.Fatalf("ghost 应 per-item 错误: %+v", byID["ghost"])
	}
}

// --- Run:SSH 错误 → 人读文案(整体 200 语义、绝无凭据明文) ---

func TestRunSSHErrorHumanReadable(t *testing.T) {
	ft := newFakeTarget()
	ft.exec = func(context.Context, string, []string) (*target.ExecResult, error) {
		return nil, target.ErrUnreachable
	}
	svc := newTestService(t, ft)

	res, err := svc.Run(context.Background(), RunInput{Command: "uptime", ServerIDs: []string{"a"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	it := res.Items[0]
	if it.OK || it.ExitCode != -1 || it.Error == "" {
		t.Fatalf("SSH 失败应 ok:false + 人读 error: %+v", it)
	}
}

// --- Run:单机 ctx 取消传播(fake 阻塞至 ctx.Done) ---

func TestRunContextCancelPropagates(t *testing.T) {
	ft := newFakeTarget()
	ft.exec = func(ctx context.Context, id string, _ []string) (*target.ExecResult, error) {
		if id != "b" {
			return &target.ExecResult{ExitCode: 0}, nil
		}
		<-ctx.Done() // 模拟长命令:由调用方 ctx(单机超时/请求取消)打断
		return nil, ctx.Err()
	}
	svc := newTestService(t, ft)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	res, err := svc.Run(ctx, RunInput{Command: "sleep 100", ServerIDs: []string{"a", "b"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	byID := map[string]ResultItem{}
	for _, it := range res.Items {
		byID[it.ServerID] = it
	}
	if !byID["a"].OK {
		t.Fatalf("a 应成功: %+v", byID["a"])
	}
	if byID["b"].OK || byID["b"].Error == "" {
		t.Fatalf("b 应被取消打断为 ok:false: %+v", byID["b"])
	}
}

// --- Run:输入校验 ---

func TestRunInvalidInput(t *testing.T) {
	svc := newTestService(t, newFakeTarget())
	ctx := context.Background()

	over := make([]string, 0, MaxServers+1)
	for i := 0; i <= MaxServers; i++ {
		over = append(over, fmt.Sprintf("s%d", i))
	}
	cases := []struct {
		name string
		in   RunInput
	}{
		{"空命令", RunInput{ServerIDs: []string{"a"}}},
		{"命令过长", RunInput{Command: strings.Repeat("a", MaxCommandLen+1), ServerIDs: []string{"a"}}},
		{"无机器", RunInput{Command: "ls"}},
		{"机器数超上限", RunInput{Command: "ls", ServerIDs: over}},
	}
	for _, c := range cases {
		if _, err := svc.Run(ctx, c.in); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s 应 ErrInvalidInput, got %v", c.name, err)
		}
	}

	// 未装配服务 → ErrUninitialized。
	var nilSvc *Service
	if _, err := nilSvc.Run(ctx, RunInput{Command: "ls", ServerIDs: []string{"a"}}); !errors.Is(err, ErrUninitialized) {
		t.Fatalf("nil 服务应 ErrUninitialized, got %v", err)
	}
}

// --- Run:重复 id 去重 ---

func TestRunDedupes(t *testing.T) {
	ft := newFakeTarget()
	var calls int
	ft.exec = func(context.Context, string, []string) (*target.ExecResult, error) {
		calls++
		return &target.ExecResult{ExitCode: 0}, nil
	}
	svc := newTestService(t, ft)

	res, err := svc.Run(context.Background(), RunInput{Command: "ls", ServerIDs: []string{"a", "a", "b", ""}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 2 || res.Total != 2 {
		t.Fatalf("去重后应只执行 2 台, calls=%d total=%d", calls, res.Total)
	}
}

// --- 历史:保留窗口裁剪(直接打 persist 快速灌历史) ---

func TestHistoryRetentionPrunes(t *testing.T) {
	ft := newFakeTarget()
	st := storetest.Open(t)
	svc := New(st.DB, ft)

	total := HistoryKeep + 5
	for i := 0; i < total; i++ {
		res := &RunResult{
			RunID: fmt.Sprintf("run-%05d", i),
			Items: []ResultItem{{ServerID: "a", ServerName: "web-1", OK: true}},
			Total: 1, OK: 1,
		}
		if err := svc.persist(context.Background(), res, fmt.Sprintf("cmd-%05d", i)); err != nil {
			t.Fatalf("persist #%d: %v", i, err)
		}
		time.Sleep(2 * time.Millisecond) // 保证 created_at 严格递增,边界确定
	}

	var count int
	if err := st.DB.QueryRow("SELECT COUNT(*) FROM server_command_runs").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != HistoryKeep {
		t.Fatalf("应裁剪到 %d 条, got %d", HistoryKeep, count)
	}
	// results 同步裁剪(不残留孤儿)。
	var rcount int
	if err := st.DB.QueryRow("SELECT COUNT(*) FROM server_command_results").Scan(&rcount); err != nil {
		t.Fatalf("rcount: %v", err)
	}
	if rcount != HistoryKeep {
		t.Fatalf("results 应同步裁剪到 %d 条, got %d", HistoryKeep, rcount)
	}
	// 最旧的被裁、最新保留:列表第一条是最后一次。
	runs, err := svc.ListRuns(context.Background(), 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns: %v len=%d", err, len(runs))
	}
	if want := fmt.Sprintf("cmd-%05d", total-1); runs[0].Command != want {
		t.Fatalf("最新一条应保留, got %q want %q", runs[0].Command, want)
	}
}

// --- GetRun:不存在 → ErrNotFound ---

func TestGetRunNotFound(t *testing.T) {
	svc := newTestService(t, newFakeTarget())
	if _, err := svc.GetRun(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("应 ErrNotFound, got %v", err)
	}
}

// --- ClampTimeoutSec 钳制 ---

func TestClampTimeoutSec(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, DefaultTimeoutSec}, {3, MinTimeoutSec}, {400, MaxTimeoutSec},
		{60, 60}, {5, 5}, {300, 300},
	}
	for _, c := range cases {
		if got := ClampTimeoutSec(c.in); got != c.want {
			t.Fatalf("ClampTimeoutSec(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// --- truncateOut 截断 ---

func TestTruncateOut(t *testing.T) {
	if got := truncateOut("abc"); got != "abc" {
		t.Fatalf("短输出不应截断: %q", got)
	}
	got := truncateOut(strings.Repeat("x", OutMax+10))
	if len(got) <= OutMax || !strings.Contains(got, "truncated") {
		t.Fatalf("超限输出应截断 + 省略标记, got len=%d", len(got))
	}
}

// --- humanError:超时映射人读 ---

func TestHumanErrorDeadline(t *testing.T) {
	if msg := humanError(context.DeadlineExceeded); !strings.Contains(msg, "超时") {
		t.Fatalf("DeadlineExceeded 应映射超时文案, got %q", msg)
	}
}
