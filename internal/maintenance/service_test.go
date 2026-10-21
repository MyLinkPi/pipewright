package maintenance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/target"
)

// fakeLister/fakeExecer 脚本化远程清扫依赖。
type fakeLister struct {
	servers []*target.Server
}

func (f fakeLister) List(context.Context) ([]*target.Server, error) { return f.servers, nil }

type fakeExecer struct {
	mu       sync.Mutex
	calls    map[string][]string // serverID → 最后一次命令
	failIDs  map[string]bool
}

func (f *fakeExecer) Exec(_ context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string][]string{}
	}
	f.calls[serverID] = cmd
	if f.failIDs[serverID] {
		return nil, errors.New("ssh unreachable")
	}
	return &target.ExecResult{ExitCode: 0}, nil
}

// 只对 Labels 非空的标准机构建机下发清扫;PVE 机与无标签机跳过;逐机独立成败。
func TestSweepRemoteWorkspacesTargetsBuildMachinesOnly(t *testing.T) {
	ex := &fakeExecer{}
	s := New(nil)
	s.lister, s.execer = fakeLister{servers: []*target.Server{
		{ID: "b1", Name: "build-1", Labels: "go"},
		{ID: "b2", Name: "build-2", Labels: "go,shared", SharedBuild: true},
		{ID: "pve", Name: "pve", Kind: target.KindPVENode, Labels: "pve"},
		{ID: "plain", Name: "deploy-only"}, // 无标签:不参与构建机池
	}}, ex

	res := s.SweepRemoteWorkspaces(context.Background())
	if !res.OK || res.Items != 2 || len(res.Machines) != 2 {
		t.Fatalf("应清扫 2 台构建机,实际 ok=%v items=%d machines=%v", res.OK, res.Items, res.Machines)
	}
	for id := range ex.calls {
		if id == "pve" || id == "plain" {
			t.Fatalf("不应清扫非构建机 %s", id)
		}
		cmd := ex.calls[id]
		if len(cmd) != 3 || cmd[0] != "sh" || !strings.Contains(cmd[2], "pipewright-remote") || !strings.Contains(cmd[2], "-mtime +2") {
			t.Fatalf("清扫命令形态不符:%v", cmd)
		}
	}
}

// 单机 SSH 失败不影响其它机器,汇总如实反映。
func TestSweepRemoteWorkspacesPartialFailure(t *testing.T) {
	ex := &fakeExecer{failIDs: map[string]bool{"bad": true}}
	s := New(nil)
	s.lister, s.execer = fakeLister{servers: []*target.Server{
		{ID: "good", Name: "g", Labels: "x"},
		{ID: "bad", Name: "b", Labels: "x"},
	}}, ex

	res := s.SweepRemoteWorkspaces(context.Background())
	if !res.OK || res.Items != 1 {
		t.Fatalf("部分失败仍应 ok 且成功 1 台,实际 ok=%v items=%d", res.OK, res.Items)
	}
	if res.Detail == "" || !strings.Contains(res.Detail, "1/2") {
		t.Fatalf("detail 应说明 1/2 台成功,实际:%s", res.Detail)
	}
	var badOK bool
	for _, m := range res.Machines {
		if m.ServerID == "bad" && !m.OK && m.Error != "" {
			badOK = true
		}
	}
	if !badOK {
		t.Fatalf("失败机器应带 error,实际:%v", res.Machines)
	}
}

// SweepLocalTemp 经 WithTempDir 指向测试目录,验证年龄门与统计透出。
func TestSweepLocalTempViaService(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "pipewright-build-orphan")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(old, "ws.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	stale := time.Now().Add(-72 * time.Hour)
	_ = os.Chtimes(old, stale, stale)

	s := New(nil).WithTempDir(dir)
	res := s.SweepLocalTemp(context.Background())
	if !res.OK || res.Items != 1 || res.Bytes <= 0 {
		t.Fatalf("应清 1 项并统计字节,实际:%+v", res)
	}
}

// 依赖未装配(缓存关闭等)→ OK + 说明,不报错。
func TestCleanBuildcacheSkipsWhenNotConfigured(t *testing.T) {
	res := New(nil).CleanBuildcache(context.Background())
	if !res.OK || res.Items != 0 || res.Detail == "" {
		t.Fatalf("未装配缓存应 OK+说明,实际:%+v", res)
	}
}
