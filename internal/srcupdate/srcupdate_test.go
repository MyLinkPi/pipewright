package srcupdate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitOut 执行一条 git 命令(dir 可空)并返回 stdout;身份/默认分支固定,保证可复现。
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不可用,跳过源码升级测试")
	}
	full := append([]string{"-c", "user.email=t@t.test", "-c", "user.name=t"}, args...)
	cmd := exec.Command("git", full...)
	if dir != "" {
		cmd.Dir = dir
	}
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

func init() {
	// 管线在本测试进程内以 stub 命令执行,与宿主 OS 无关;绕过生产端的 unix 守卫。
	unixLike = true
}

// mkRepo 造「上游裸仓库 + 已克隆且跟踪 origin 的工作仓」,含一个初始提交;返回工作仓目录。
func mkRepo(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	up := filepath.Join(base, "up.git")
	work := filepath.Join(base, "work")

	gitOut(t, "", "-c", "init.defaultBranch=main", "init", "--bare", up)
	gitOut(t, "", "-c", "init.defaultBranch=main", "init", work)
	gitOut(t, work, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "app.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, work, "add", ".")
	gitOut(t, work, "commit", "-m", "init")
	gitOut(t, work, "remote", "add", "origin", up)
	gitOut(t, work, "push", "-u", "origin", "main")
	return work
}

// pushExtraCommit 从上游侧追加一个新提交(模拟远端有新版本)。
func pushExtraCommit(t *testing.T, work string) {
	t.Helper()
	second := filepath.Join(filepath.Dir(work), "work2")
	gitOut(t, "", "clone", upstreamOf(t, work), second)
	if err := os.WriteFile(filepath.Join(second, "app.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, second, "add", ".")
	gitOut(t, second, "commit", "-m", "feat: next")
	gitOut(t, second, "push", "origin", "HEAD:main")
}

func upstreamOf(t *testing.T, work string) string {
	return gitOut(t, work, "remote", "get-url", "origin")
}

// waitStatus 轮询直到 pred 成立或超时(任务异步,避免固定 sleep)。
func waitStatus(t *testing.T, s *Service, pred func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st := s.Status()
		if pred(st) {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("状态未在超时内到达:%+v", s.Status())
	return Status{}
}

func TestCheck_UpToDate(t *testing.T) {
	work := mkRepo(t)
	s := New(work)
	info := s.Check(context.Background())
	if info.CheckError != "" {
		t.Fatalf("检查不应报错:%s", info.CheckError)
	}
	if info.UpdateAvailable {
		t.Errorf("与上游一致不应报有更新:%+v", info)
	}
	if info.Latest == "" || len(info.Latest) > 8 {
		t.Errorf("Latest 应为上游短 SHA,得 %q", info.Latest)
	}
	if info.Current == "" {
		t.Errorf("Current 应取当前构建版本")
	}
}

func TestCheck_UpdateAvailable(t *testing.T) {
	work := mkRepo(t)
	pushExtraCommit(t, work)
	s := New(work)
	info := s.Check(context.Background())
	if info.CheckError != "" {
		t.Fatalf("检查不应报错:%s", info.CheckError)
	}
	if !info.UpdateAvailable {
		t.Fatalf("上游领先应报有更新:%+v", info)
	}
	if !strings.Contains(info.Notes, "落后 1 个提交") || !strings.Contains(info.Notes, "feat: next") {
		t.Errorf("Notes 应含落后计数与提交列表,得 %q", info.Notes)
	}
}

func TestCheck_NoUpstream(t *testing.T) {
	work := mkRepo(t)
	gitOut(t, work, "remote", "remove", "origin")
	s := New(work)
	info := s.Check(context.Background())
	if info.CheckError == "" {
		t.Fatalf("无上游应报 CheckError:%+v", info)
	}
	if info.UpdateAvailable {
		t.Errorf("检查失败时不应误报有更新")
	}
}

// 完整管线:pull(无新提交,成功)→ build/install(stub 脚本)→ 无 systemd unit →
// 注入的 no-op 重启 → done;日志含 stub 输出。
func TestUpdatePipeline(t *testing.T) {
	work := mkRepo(t)
	s := New(work)
	s.unitPath = filepath.Join(t.TempDir(), "absent.service") // 不存在 → 走 restart 分支
	s.restart = func() error { return nil }
	var order []string
	s.buildCmd = []string{"sh", "-c", `echo built >> steps.log; echo [build] ok >&2`}
	s.installCmd = []string{"sh", "-c", `echo installed >> steps.log; echo [install] ok`}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh 不可用,跳过管线测试")
	}

	if err := s.StartUpdate(); err != nil {
		t.Fatalf("StartUpdate: %v", err)
	}
	st := waitStatus(t, s, func(st Status) bool { return !st.Running })
	if st.Error != "" {
		t.Fatalf("管线失败:%s\n日志:%v", st.Error, st.Log)
	}
	if !st.Done {
		t.Errorf("完成后 done 应为 true:%+v", st)
	}
	raw, err := os.ReadFile(filepath.Join(work, "steps.log"))
	if err != nil {
		t.Fatalf("stub 步骤未执行: %v", err)
	}
	order = strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(order) != 2 || order[0] != "built" || order[1] != "installed" {
		t.Errorf("步骤顺序应为 build→install,得 %v", order)
	}
	joined := strings.Join(st.Log, "\n")
	if !strings.Contains(joined, "[build] ok") || !strings.Contains(joined, "[install] ok") {
		t.Errorf("日志应含构建与安装输出,得:%s", joined)
	}
}

// 管线中途失败:pull 阶段故意失败(无效 upstream),错误进状态,后续步骤不执行。
func TestUpdatePipeline_Fails(t *testing.T) {
	work := mkRepo(t)
	s := New(work)
	s.unitPath = filepath.Join(t.TempDir(), "absent.service")
	s.restart = func() error { return nil }
	s.buildCmd = []string{"sh", "-c", "echo built >> steps.log"}
	s.installCmd = []string{"sh", "-c", "echo installed >> steps.log"}
	// 破坏上游跟踪:把 @{u} 指向不存在的引用 → pull 失败。
	gitOut(t, work, "remote", "set-url", "origin", filepath.Join(filepath.Dir(work), "nonexistent.git"))

	if err := s.StartUpdate(); err != nil {
		t.Fatalf("StartUpdate: %v", err)
	}
	st := waitStatus(t, s, func(st Status) bool { return !st.Running })
	if st.Error == "" {
		t.Fatalf("pull 失败应上报错误:%+v", st)
	}
	if st.Done {
		t.Errorf("失败后 done 不应为 true")
	}
	if _, err := os.Stat(filepath.Join(work, "steps.log")); !os.IsNotExist(err) {
		t.Errorf("pull 失败后不应继续构建")
	}
}

// 串行化:任务进行中再次 StartUpdate 返回 ErrBusy;完成后可再次启动。
func TestStartUpdate_Busy(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh 不可用")
	}
	work := mkRepo(t)
	s := New(work)
	s.unitPath = filepath.Join(t.TempDir(), "absent.service")
	s.restart = func() error { return nil }
	s.buildCmd = []string{"sh", "-c", "sleep 0.4; echo built"}

	if err := s.StartUpdate(); err != nil {
		t.Fatalf("第一次启动: %v", err)
	}
	if err := s.StartUpdate(); !errors.Is(err, ErrBusy) {
		t.Fatalf("进行中应返回 ErrBusy,得 %v", err)
	}
	waitStatus(t, s, func(st Status) bool { return !st.Running })
	if err := s.StartUpdate(); err != nil {
		t.Fatalf("完成后应可再次启动,得 %v", err)
	}
	waitStatus(t, s, func(st Status) bool { return !st.Running })
}

// 已装 systemd 服务(unit 存在):不调用本地 restart,直接 finish —— 重启由 install.sh
// 的 systemctl 完成。
func TestUpdatePipeline_WithSystemdUnit(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh 不可用")
	}
	work := mkRepo(t)
	s := New(work)
	unit := filepath.Join(t.TempDir(), "pipewright.service")
	if err := os.WriteFile(unit, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.unitPath = unit
	called := false
	s.restart = func() error { called = true; return nil }
	s.buildCmd = []string{"sh", "-c", "echo built"}
	s.installCmd = []string{"sh", "-c", "echo installed"}

	if err := s.StartUpdate(); err != nil {
		t.Fatalf("StartUpdate: %v", err)
	}
	st := waitStatus(t, s, func(st Status) bool { return !st.Running })
	if st.Error != "" || !st.Done {
		t.Fatalf("管线应完成:%+v", st)
	}
	if called {
		t.Errorf("已装服务时不应本地 re-exec(重启由 install.sh 完成)")
	}
}

// privEnv:safe.directory 经 GIT_CONFIG_COUNT 追加,且不吞掉既有的 git env 配置。
func TestPrivEnv(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "2")
	s := New(t.TempDir())
	env := s.privEnv()
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "GIT_CONFIG_COUNT=3") {
		t.Errorf("应在既有 count 上追加,得:%s", joined)
	}
	if !strings.Contains(joined, "GIT_CONFIG_KEY_2=safe.directory") ||
		!strings.Contains(joined, "GIT_CONFIG_VALUE_2="+s.dir) {
		t.Errorf("safe.directory 应写到既有槽位之后,得:%s", joined)
	}
	if !strings.Contains(joined, "GIT_TERMINAL_PROMPT=0") {
		t.Errorf("应禁用终端凭据提示,得:%s", joined)
	}
}

// lineWriter:按行切分 + 尾行无换行时 flush。
func TestLineWriter(t *testing.T) {
	var got []string
	w := &lineWriter{fn: func(l string) { got = append(got, l) }}
	_, _ = w.Write([]byte("a\nbb\r\ncc"))
	w.flush()
	if len(got) != 3 || got[0] != "a" || got[1] != "bb" || got[2] != "cc" {
		t.Errorf("行切分不符:%q", got)
	}
}
