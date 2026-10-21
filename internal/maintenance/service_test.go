package maintenance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/retention"
	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/target"
)

// fakeLister/fakeExecer 脚本化远程清扫依赖。
type fakeLister struct {
	servers []*target.Server
}

func (f fakeLister) List(context.Context) ([]*target.Server, error) { return f.servers, nil }

// fakeExecer:images 命令回放脚本化清单,rmi 逐条记录;failIDs 模拟整机不可达。
type fakeExecer struct {
	mu        sync.Mutex
	calls     map[string][]string // serverID → 最后一次命令
	failIDs   map[string]bool
	imagesOut string   // `docker images` 的回放输出
	rmiRefs   []string // 全部 rmi 的镜像引用(跨机器)
	history   []string // 全部命令的单行回显(断言 builder/container prune 用)
}

func (f *fakeExecer) Exec(_ context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string][]string{}
	}
	f.calls[serverID] = cmd
	f.history = append(f.history, strings.Join(cmd, " "))
	if f.failIDs[serverID] {
		return nil, errors.New("ssh unreachable")
	}
	if len(cmd) >= 2 && (cmd[0] == "docker" || cmd[0] == "nerdctl" || cmd[0] == "podman") && cmd[1] == "images" {
		return &target.ExecResult{Stdout: f.imagesOut, ExitCode: 0}, nil
	}
	if len(cmd) >= 2 && cmd[1] == "rmi" {
		f.rmiRefs = append(f.rmiRefs, cmd[2])
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
	// 每台机器的结果必须带实际执行的命令原文(可审计:在哪台机器上执行了什么一目了然)。
	for _, m := range res.Machines {
		if !strings.HasPrefix(m.Command, "$ sh -c find /tmp/pipewright-remote") || !strings.Contains(m.Command, "-mtime +2") {
			t.Fatalf("机器结果应带命令原文,实际:%+v", m)
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
	// Steps 必须带扫描说明与被删路径(点了清理就能看到动了哪些文件)。
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "扫描目录") || !strings.Contains(joined, old) {
		t.Fatalf("Steps 应含扫描说明与删除路径,实际:%v", res.Steps)
	}
}

// 依赖未装配(缓存关闭等)→ OK + 说明,不报错。
func TestCleanBuildcacheSkipsWhenNotConfigured(t *testing.T) {
	res := New(nil).CleanBuildcache(context.Background())
	if !res.OK || res.Items != 0 || res.Detail == "" {
		t.Fatalf("未装配缓存应 OK+说明,实际:%+v", res)
	}
}

// CleanImages:保留集 = 每项目最近 N 个 commit tag;控制机与构建机上超出保留集的
// pipewright/* 被删,基础镜像绝不碰;保留集含最新提交(在途构建安全)。
func TestCleanImagesKeepsRecentPerProject(t *testing.T) {
	db := storetest.OpenDB(t)
	now := time.Now().UTC().Format(time.RFC3339)
	credID := "cred-1"
	if _, err := db.Exec(`INSERT INTO credentials (id, name, type, scope, ciphertext, masked_value, created_at, updated_at)
		VALUES (?, 'c', 'git_token', '', X'00', 'm', ?, ?)`, credID, now, now); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, name, repo_url, default_branch, credential_id, created_at, updated_at)
		VALUES ('p1', 'My App', 'https://example.com/a.git', 'main', ?, ?, ?)`, credID, now, now); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	// 3 个 commit,c1 最旧、c3 最新;保留 2 个 → c1 应被清理。
	for i, c := range []string{"c1aaa", "c2bbb", "c3ccc"} {
		ts := time.Now().UTC().Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		if _, err := db.Exec(`INSERT INTO pipeline_runs (id, project_id, status, trigger_type, trigger_branch, trigger_commit, trigger_actor, params_json, chain_source_run_id, chain_depth, created_at, finished_at)
			VALUES (?, 'p1', 'success', 'manual', 'main', ?, '', '{}', '', 0, ?, '')`,
			fmt.Sprintf("run-%d", i), c, ts); err != nil {
			t.Fatalf("seed run: %v", err)
		}
	}

	ex := &fakeExecer{imagesOut: "pipewright/my-app:c1aaa\npipewright/my-app:c2bbb\npipewright/my-app:c3ccc\nnode:20\n"}
	var localKeep map[string]struct{}
	s := New(db)
	s.lister = fakeLister{servers: []*target.Server{{ID: "b1", Name: "build-1", Labels: "go"}}}
	s.execer = ex
	s.localRotate = func(_ context.Context, _ string, keep map[string]struct{}, _ func(string, string)) ([]string, error) {
		localKeep = keep
		return []string{"pipewright/my-app:c1aaa"}, nil
	}
	s.localExtra = func(context.Context) []string {
		return []string{"$ docker builder prune -f", "$ docker container prune -f"}
	}
	if err := s.retention.SetConfig(context.Background(), retention.Config{ImageKeepTags: 2}); err != nil {
		t.Fatalf("set config: %v", err)
	}

	res := s.CleanImages(context.Background())
	if !res.OK {
		t.Fatalf("CleanImages 应成功:%+v", res)
	}
	if res.Items != 2 { // 控制机 1 + 构建机 1(同一保留集各自删 c1)
		t.Fatalf("应共删 2 个引用,实际:%+v", res)
	}
	// 保留集:最近 2 个 commit(c3ccc/c2bbb),不含 c1aaa。
	if _, ok := localKeep["pipewright/my-app:c3ccc"]; !ok {
		t.Fatalf("保留集应含最新 c3ccc:%v", localKeep)
	}
	if _, ok := localKeep["pipewright/my-app:c2bbb"]; !ok {
		t.Fatalf("保留集应含 c2bbb:%v", localKeep)
	}
	if _, ok := localKeep["pipewright/my-app:c1aaa"]; ok {
		t.Fatalf("保留集不应含 c1aaa:%v", localKeep)
	}
	// 构建机只 rmi 超出保留集的引用,基础镜像(node:20)绝不碰。
	if len(ex.rmiRefs) != 1 || ex.rmiRefs[0] != "pipewright/my-app:c1aaa" {
		t.Fatalf("构建机应只 rmi c1aaa,实际:%v", ex.rmiRefs)
	}
	// 追加清理必须下发到构建机:BuildKit 缓存全清 + 停止容器回收(镜像侧真正的空间大头)。
	hist := strings.Join(ex.history, "\n")
	if !strings.Contains(hist, "docker builder prune -f") || !strings.Contains(hist, "docker container prune -f") {
		t.Fatalf("构建机应执行 builder/container prune,实际:%v", ex.history)
	}
	// 明细:控制机与构建机的删除都要留痕,机器结果带命令摘要。
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "控制机 删除 pipewright/my-app:c1aaa") || !strings.Contains(joined, "构建机 build-1 删除 pipewright/my-app:c1aaa") {
		t.Fatalf("Steps 应含控制机与构建机删除明细:%v", res.Steps)
	}
	if len(res.Machines) != 1 || !res.Machines[0].OK || !strings.Contains(res.Machines[0].Command, "images") {
		t.Fatalf("机器结果应带命令摘要:%+v", res.Machines)
	}
}

// keepN=0(不轮转)→ 如实说明并跳过,不误删。
func TestCleanImagesSkipsWhenRotationOff(t *testing.T) {
	db := storetest.OpenDB(t)
	s := New(db)
	if err := s.retention.SetConfig(context.Background(), retention.Config{ImageKeepTags: 0}); err != nil {
		t.Fatalf("set config: %v", err)
	}
	res := s.CleanImages(context.Background())
	if !res.OK || res.Items != 0 || res.Detail == "" {
		t.Fatalf("keepN=0 应跳过并说明,实际:%+v", res)
	}
}
