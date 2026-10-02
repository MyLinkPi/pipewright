// Package srcupdate 是「源码部署」形态的自升级(internal/version.Mode() == ModeSource):
//
//	检查:git fetch 后比对本地 HEAD 与上游分支,落后即有更新(不走 GitHub release,
//	      远端可以是任意 git 源 —— 官方仓库、fork 或镜像,天然与升级源镜像配置解耦);
//	升级:git pull --ff-only → make build → install.sh,后台任务串行执行并维护进度快照,
//	      供 /api/version/update/status 轮询。
//
// 仓库根目录由 version.SourceDir() 探测(env PIPEWRIGHT_SOURCE_DIR > 安装目录标记文件 >
// 可执行文件在仓库内)。git/make/install.sh 均为固定命令,无外部输入拼接,不经 shell 解释。
package srcupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/huangchengsir/pipewright/internal/version"
)

// ErrBusy 表示已有一次升级任务在进行(同一时刻仅允许一个)。
var ErrBusy = errors.New("srcupdate: update already running")

const (
	// jobTimeout 是整条升级管线(pull → build → install)的总时限;make build 含前端
	// npm ci,慢机可达数分钟,给足余量。
	jobTimeout = 30 * time.Minute

	// gitTimeout 是单条 git 命令的时限;fetch 涉及网络单独放宽。
	gitTimeout     = 30 * time.Second
	gitFetchSubTO  = 2 * time.Minute
	fetchNoteLimit = 20

	// maxLogLines 是任务日志环形缓冲的行数上限(前端展示 + 排障用)。
	maxLogLines = 120

	systemdUnitPath = "/etc/systemd/system/pipewright.service"
)

// Status 是升级任务的进度快照(GET /api/version/update/status 的载荷)。
type Status struct {
	Running bool   `json:"running"`
	Step    string `json:"step"`              // pull | build | install | restart
	Message string `json:"message,omitempty"` // 当前步骤的人读简述
	Done    bool   `json:"done"`              // 管线完成,已触发重启(服务重启 / re-exec)
	Error   string `json:"error,omitempty"`   // 非空 = 任务失败(管线中止)
	Log     []string `json:"log,omitempty"`   // 最近输出(环形,新在后)
}

// Service 源码升级服务。零值不可用,请用 New。
type Service struct {
	dir string // 仓库根(version.SourceDir() 探测结果)

	// 可注入覆盖(测试);生产为空用默认 "make build" / "sh install.sh"。
	buildCmd   []string
	installCmd []string
	// installDir 是 install.sh 的 INSTALL_DIR(对齐当前可执行文件所在目录,升级即原位替换)。
	installDir string
	// restart 默认 version.Reexec(裸跑自重启);测试注入 no-op,避免在测试进程里真 exec。
	restart func() error
	// unitPath 是 systemd unit 的探测路径;测试指向临时文件以模拟「已装服务」分支。
	unitPath string

	mu  sync.Mutex
	job *jobT
}

// New 构造 Service;dir 须经调用方(version.SourceDir)校验为仓库根。
func New(dir string) *Service {
	installDir := ""
	if exe, err := os.Executable(); err == nil {
		installDir = filepath.Dir(exe)
	}
	return &Service{dir: dir, installDir: installDir, unitPath: systemdUnitPath}
}

// ── 检查 ─────────────────────────────────────────────────────────────────────

// Check 拉取上游并与本地 HEAD 比对,产出与 GitHub 检查同形的 UpdateInfo:
// Latest = 上游短 SHA;Notes = 落后提交列表(≤20 行)。任何失败以 CheckError 返回
// (不返 error),让 /version/check 稳定 200、前端优雅降级。
func (s *Service) Check(ctx context.Context) version.UpdateInfo {
	out := version.UpdateInfo{Current: version.Version}

	local, err := s.git(ctx, gitTimeout, "rev-parse", "HEAD")
	if err != nil {
		out.CheckError = "读取本地提交失败: " + err.Error()
		return out
	}

	// fetch 拿上游最新(网络;无凭据/断网在此失败,信息里带 git 原因)。
	if err := s.gitRun(ctx, gitFetchSubTO, "fetch", "--quiet"); err != nil {
		out.CheckError = "拉取远端失败: " + err.Error()
		return out
	}

	upstream, err := s.git(ctx, gitTimeout, "rev-parse", "@{u}")
	if err != nil {
		out.CheckError = "当前分支未配置上游(git branch --set-upstream-to=<remote>/<branch>): " + err.Error()
		return out
	}

	if short, err := s.git(ctx, gitTimeout, "rev-parse", "--short", "@{u}"); err == nil {
		out.Latest = short
	} else {
		out.Latest = upstream[:min(7, len(upstream))]
	}
	if local != upstream {
		out.UpdateAvailable = true
		if n, err := s.git(ctx, gitTimeout, "rev-list", "--count", "HEAD..@{u}"); err == nil {
			out.Notes = "落后 " + n + " 个提交:"
		}
		if log, err := s.git(ctx, gitTimeout, "log", "--oneline", fmt.Sprintf("-%d", fetchNoteLimit), "HEAD..@{u}"); err == nil && log != "" {
			if out.Notes == "" {
				out.Notes = "落后若干提交:"
			}
			out.Notes += "\n" + log
		}
	}
	return out
}

// ── 升级管线 ─────────────────────────────────────────────────────────────────

// unixLike 报告当前 OS 是否具备源码升级管线的前提(make/install.sh/systemd 均为 unix 生态)。
// 包级变量,便于测试在任意平台覆盖。
var unixLike = runtime.GOOS == "linux" || runtime.GOOS == "darwin"

// StartUpdate 异步启动升级管线(pull → build → install → 重启)。已有任务在跑返回 ErrBusy。
func (s *Service) StartUpdate() error {
	if !unixLike {
		return fmt.Errorf("源码升级依赖 make/install.sh,仅支持 Linux/macOS,当前 %s 请手动执行 git pull && make build && sh install.sh", runtime.GOOS)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job != nil && s.job.running {
		return ErrBusy
	}
	j := &jobT{running: true, step: "pull", message: "准备拉取最新代码…"}
	s.job = j
	go s.run(j)
	return nil
}

// Status 返回任务快照;从未启动过返回 idle 空快照。
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		return Status{}
	}
	s.job.mu.Lock()
	defer s.job.mu.Unlock()
	st := Status{
		Running: s.job.running,
		Step:    s.job.step,
		Message: s.job.message,
		Done:    s.job.done,
		Error:   s.job.errMsg,
	}
	st.Log = append([]string(nil), s.job.log...)
	return st
}

type jobT struct {
	mu      sync.Mutex // 守护以下可变字段(Status 快照与日志追加并发)
	running bool
	step    string
	message string
	done    bool
	errMsg  string
	log     []string
}

func (j *jobT) set(step, msg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.step, j.message = step, msg
}

func (j *jobT) appendLine(line string) {
	if line == "" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.log = append(j.log, line)
	if len(j.log) > maxLogLines {
		j.log = j.log[len(j.log)-maxLogLines:]
	}
}

func (j *jobT) fail(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.running, j.errMsg = false, err.Error()
}

func (j *jobT) finish() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.running, j.done = false, true
}

// run 执行管线;仅在 StartUpdate 的 goroutine 中运行。
func (s *Service) run(j *jobT) {
	ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
	defer cancel()

	type stepT struct {
		step string
		msg  string
		fn   func() error
	}
	steps := []stepT{
		{"pull", "git pull --ff-only", func() error {
			return s.stream(ctx, j, nil, []string{"git", "-C", s.dir, "pull", "--ff-only"})
		}},
		{"build", "make build(含前端构建,可能需要数分钟)", func() error {
			return s.stream(ctx, j, nil, s.buildCommand())
		}},
		{"install", "install.sh(替换二进制 / 重启服务)", func() error {
			// INSTALL_DIR 对齐当前可执行文件所在目录:升级即原位替换(装在 ~/.local/bin
			// 的免 sudo 部署也成立);默认 /usr/local/bin 与脚本缺省一致。
			env := []string{"INSTALL_DIR=" + s.installDir}
			return s.stream(ctx, j, env, s.installCommand())
		}},
	}
	for _, st := range steps {
		j.set(st.step, st.msg)
		if err := st.fn(); err != nil {
			j.fail(fmt.Errorf("%s 失败: %w", st.step, err))
			return
		}
	}

	// 安装完成。装了 systemd 服务:install.sh 升级模式已 systemctl restart(脚本内
	// 仅在服务 active 时重启),本进程随之被替换;裸跑:re-exec 换出新二进制(同 PID)。
	j.set("restart", "安装完成,正在重启…")
	if _, err := os.Stat(s.unitPath); err != nil {
		if err := s.restartFn()(); err != nil {
			j.fail(fmt.Errorf("自动重启失败(新版本已安装,请手动重启进程): %w", err))
			return
		}
	}
	j.finish()
}

func (s *Service) restartFn() func() error {
	if s.restart != nil {
		return s.restart
	}
	return version.Reexec
}

// buildCommand / installCommand 返回构建与安装命令;测试可经 buildCmd/installCmd 字段
// 覆盖为 stub 脚本,生产为固定值(不经 shell 拼接)。
func (s *Service) buildCommand() []string {
	if len(s.buildCmd) > 0 {
		return s.buildCmd
	}
	return []string{"make", "build"}
}

func (s *Service) installCommand() []string {
	if len(s.installCmd) > 0 {
		return s.installCmd
	}
	return []string{"sh", "install.sh"}
}

// stream 在仓库目录执行一条命令(argv 完整数组,不经 shell),输出逐行进任务日志;
// extraEnv 追加到进程环境。
func (s *Service) stream(ctx context.Context, j *jobT, extraEnv, argv []string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = s.dir
	cmd.Env = append(append(os.Environ(), extraEnv...), s.privEnv()...)
	lw := &lineWriter{fn: j.appendLine}
	cmd.Stdout, cmd.Stderr = lw, lw
	if err := cmd.Run(); err != nil {
		lw.flush()
		return fmt.Errorf("%s: %v", argv[0], err)
	}
	lw.flush()
	return nil
}

// lineWriter 把字节流按行切给 fn(兼容 \r\n 与不带换行的尾行)。
type lineWriter struct {
	buf strings.Builder
	fn  func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		s := w.buf.String()
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			break
		}
		w.fn(strings.TrimRight(s[:i], "\r")) // 兼容 \r\n
		w.buf.Reset()
		w.buf.WriteString(s[i+1:])
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	if r := w.buf.String(); r != "" {
		w.fn(r)
		w.buf.Reset()
	}
}

// ── git 底层 ─────────────────────────────────────────────────────────────────

// git 执行一条 git 命令并返回 stdout(去空白);to 为该命令超时。
func (s *Service) git(ctx context.Context, to time.Duration, args ...string) (string, error) {
	c, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	cmd := exec.CommandContext(c, "git", append([]string{"-C", s.dir}, args...)...)
	cmd.Env = append(os.Environ(), s.privEnv()...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimSpace(out.String()), nil
}

func (s *Service) gitRun(ctx context.Context, to time.Duration, args ...string) error {
	_, err := s.git(ctx, to, args...)
	return err
}

// privEnv 是给 git / make / install.sh 注入的环境:
//   - GIT_TERMINAL_PROMPT=0 与 ssh BatchMode:非交互场景凭据缺失时立即失败,而非挂死等输入;
//   - safe.directory:平台以 root 运行、仓库属主为普通用户时,git 会拒绝操作(dubious
//     ownership)。该配置只认受保护配置源(system/global/命令行/env),故经
//     GIT_CONFIG_COUNT 追加(只增不改,不影响用户既有 git 配置)。
func (s *Service) privEnv() []string {
	env := []string{"GIT_TERMINAL_PROMPT=0"}
	if os.Getenv("GIT_SSH_COMMAND") == "" {
		env = append(env, "GIT_SSH_COMMAND=ssh -oBatchMode=yes")
	}
	n := 0
	if v := os.Getenv("GIT_CONFIG_COUNT"); v != "" {
		if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && i >= 0 {
			n = i
		}
	}
	return append(env,
		fmt.Sprintf("GIT_CONFIG_COUNT=%d", n+1),
		fmt.Sprintf("GIT_CONFIG_KEY_%d=safe.directory", n),
		fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n, s.dir),
	)
}
