package build

// mesh_shared_server.go 实现集群级共享构建机的提供方(owner)运行时(00:0086):
// 受理集群对端环境的共享构建任务(workspace tar.gz 流式上传,sha256 校验后落 OS 临时文件),
// 经 runner 调度器 AcquireShared 进入本端构建机池**同一 FIFO** 与本端任务统一排队,在共享机
// 上容器执行;产物(script 产物归档 / build_image 的 docker save 归档)经 Download 拉回本端
// OS 临时文件,供发起方回拉 —— **绝不写入本端制品库**。
//
// 安全/内存边界:
//   - spec 与环境变量(含发起方 secret)只在内存,不落盘;日志驻内存 ring(重订阅回放),
//     终态 + TTL 后连暂存文件一起清理;
//   - SSH 凭据仍只由本端(target.Service)解密使用,发起方永不接触;
//   - 任务表为进程内存态:重启即失,发起方轮询 404 判失败(与平台调度态语义一致)。
//
// B 端(发起方)提交/跟踪在 mesh_shared_client.go;入站 HTTP 端点在 httpapi(mesh_inbound)。
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/mesh"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/target"
)

// 共享构建任务的规模/生命周期边界。
const (
	// sharedJobLogRingMax 是单任务日志回放 ring 上限(防长构建把内存打爆;超限后旧日志
	// 不再回放,实时 SSE 不受影响)。
	sharedJobLogRingMax = 5000
	// sharedResultTTL 是终态后产物暂存与任务记录的保留期(给发起方留回拉窗口)。
	sharedResultTTL = 15 * time.Minute
	// sharedMaxJobs 是同时在册(排队+运行+待回拉)的任务上限(防共享队列把宿主临时盘打爆)。
	sharedMaxJobs = 32
)

// 领域错误(httpapi 映射状态码)。
var (
	// ErrSharedQueueFull 表示共享构建任务在册数已达上限,受理拒绝(发起方稍后重试)。
	ErrSharedQueueFull = errors.New("build: shared build queue full")
	// ErrSharedJobNotFound 表示任务不存在或已过保留期(含本端重启后的在途任务丢失)。
	ErrSharedJobNotFound = errors.New("build: shared job not found")
	// ErrSharedSpecInvalid 表示任务 spec 非法(缺选择器/类型/命令等;受理即拒,不占队列)。
	ErrSharedSpecInvalid = errors.New("build: invalid shared job spec")
	// ErrSharedChecksum 表示工作区 sha256/字节数与 spec 声明不符(传输损坏,受理撤销)。
	ErrSharedChecksum = errors.New("build: workspace checksum mismatch")
)

// SharedAcquirer 抽象「共享构建取机」能力(runner.Scheduler 即满足;main 注入,防包循环)。
// AcquireShared 与本端任务共用同一 FIFO/槽位 —— 跨环境排队语义的根。
type SharedAcquirer interface {
	AcquireShared(ctx context.Context, pipelineID, selector string, log func(string)) (serverID, serverName string, release func(), err error)
}

// SharedExecTarget 抽象共享机执行所需的 target 能力(target.Service 即满足)。
type SharedExecTarget interface {
	RemoteExecer
	Upload(ctx context.Context, serverID string, content io.Reader, remotePath string) error
	Download(ctx context.Context, serverID, remotePath string, w io.Writer) (int64, error)
}

// SharedMachineLister 抽象共享机目录数据源(target.Service 即满足)。
type SharedMachineLister interface {
	List(ctx context.Context) ([]*target.Server, error)
}

// sharedBuildJob 是一个在册共享构建任务的全部可变态(含日志 ring 与 SSE 订阅者;
// 全部字段由 SharedBuildServer.mu 保护)。
type sharedBuildJob struct {
	id          string
	peerNodeID  string
	peerName    string
	spec        *mesh.SharedJobSpec
	wsTar       string // 受理时的工作区暂存(上机成功即删;空 = 已消费/清理)
	state       string // mesh.SharedJobState*
	serverID    string
	serverName  string
	exitCode    int
	errMsg      string
	resultPath  string // 产物归档暂存(成功且有声明的产物时非空)
	resultSHA   string
	resultSize  int64
	queuedAt    time.Time
	startedAt   time.Time
	endedAt     time.Time
	ctx         context.Context
	cancel      context.CancelFunc
	logs        []mesh.SharedJobEvent
	logsDropped int // ring 截断计数(状态页提示)
	subs        map[chan mesh.SharedJobEvent]struct{}
	cleanupOnce sync.Once
}

// SharedBuildServer 是提供方共享构建运行时。构造后并发安全;装配见 main.go。
type SharedBuildServer struct {
	b        *Builder
	sched    SharedAcquirer
	tgt      SharedExecTarget
	machines SharedMachineLister
	// OnEvent 是审计/观测钩子(action: received|succeeded|failed|canceled;可空)。
	OnEvent func(action string, st mesh.SharedJobStatus)

	mu   sync.Mutex
	jobs map[string]*sharedBuildJob
}

// NewSharedBuildServer 构造提供方运行时。b 为本端 Builder(仅 build_image 构建复用其
// driver 装配面);sched/tgt/machines 任一为 nil → 返回 nil(装配缺席即功能整体不启用)。
func NewSharedBuildServer(b *Builder, sched SharedAcquirer, tgt SharedExecTarget, machines SharedMachineLister) *SharedBuildServer {
	if b == nil || sched == nil || tgt == nil || machines == nil {
		return nil
	}
	return &SharedBuildServer{
		b:        b,
		sched:    sched,
		tgt:      tgt,
		machines: machines,
		jobs:     make(map[string]*sharedBuildJob),
	}
}

// ActiveJobs 返回非终态共享构建任务数(queued/running —— 含其他环境提交的入站任务;
// 自更新 2a 门槛:有活跃任务时推迟 re-exec,避免孤儿构建进程)。nil 接收者安全。
func (s *SharedBuildServer) ActiveJobs() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, j := range s.jobs {
		if j.state == mesh.SharedJobStateQueued || j.state == mesh.SharedJobStateRunning {
			n++
		}
	}
	return n
}

// Accept 受理一个共享构建任务:校验 spec → 流式落盘工作区(sha256/字节数校验)→ 入册为
// queued 并起 goroutine 排队执行。peerNodeID 来自 mesh 签名验证的调用方身份(可信)。
// 返回任务 id(发起方据此跟踪/回拉/取消)。
func (s *SharedBuildServer) Accept(ctx context.Context, peerNodeID, peerName string, spec *mesh.SharedJobSpec, ws io.Reader) (string, error) {
	if spec == nil || ws == nil || strings.TrimSpace(peerNodeID) == "" {
		return "", ErrSharedSpecInvalid
	}
	spec.Selector = strings.TrimSpace(spec.Selector)
	if spec.Selector == "" {
		return "", fmt.Errorf("%w: 缺少选择器", ErrSharedSpecInvalid)
	}
	if err := validateSharedSpec(spec); err != nil {
		return "", err
	}

	s.mu.Lock()
	if len(s.jobs) >= sharedMaxJobs {
		s.mu.Unlock()
		return "", ErrSharedQueueFull
	}
	id := uuid.NewString()
	wsTar := filepath.Join(os.TempDir(), "pipewright-shared-"+id+".tar.gz")
	job := &sharedBuildJob{
		id:         id,
		peerNodeID: peerNodeID,
		peerName:   peerName,
		spec:       spec,
		wsTar:      wsTar,
		state:      mesh.SharedJobStateQueued,
		queuedAt:   time.Now().UTC(),
		subs:       make(map[chan mesh.SharedJobEvent]struct{}),
	}
	s.jobs[id] = job
	s.mu.Unlock()

	// 任务生命周期上下文在入册即建(Cancel 不与 run goroutine 竞态;任务超时在此生效)。
	if spec.TimeoutMinutes > 0 {
		job.ctx, job.cancel = context.WithTimeout(context.Background(), time.Duration(spec.TimeoutMinutes)*time.Minute)
	} else {
		job.ctx, job.cancel = context.WithCancel(context.Background())
	}

	// 流式落盘 + 完整性校验(锁外;失败即撤销受理,不留半任务)。
	if err := s.storeWorkspace(job, ws); err != nil {
		s.mu.Lock()
		delete(s.jobs, id)
		s.mu.Unlock()
		_ = os.Remove(wsTar)
		return "", err
	}

	if s.OnEvent != nil {
		s.OnEvent("received", s.snapshotLocked(job))
	}
	go s.run(job)
	return id, nil
}

// validateSharedSpec 校验任务 spec 的类型相关必填(受理即拒,不占队列)。
func validateSharedSpec(spec *mesh.SharedJobSpec) error {
	switch spec.JobType {
	case mesh.SharedJobTypeScript:
		if strings.TrimSpace(spec.Image) == "" || len(spec.Commands) == 0 {
			return fmt.Errorf("%w: script 任务缺 image/commands", ErrSharedSpecInvalid)
		}
	case mesh.SharedJobTypeBuildImage:
		is := spec.ImageSpec
		if is == nil || strings.TrimSpace(is.ProjectName) == "" || strings.TrimSpace(is.CommitTag) == "" {
			return fmt.Errorf("%w: build_image 任务缺 imageSpec(projectName/commitTag)", ErrSharedSpecInvalid)
		}
	default:
		return fmt.Errorf("%w: 未知任务类型 %q", ErrSharedSpecInvalid, spec.JobType)
	}
	return nil
}

// storeWorkspace 把上传流写入暂存文件并做 sha256/字节数校验。
func (s *SharedBuildServer) storeWorkspace(job *sharedBuildJob, ws io.Reader) error {
	f, err := os.Create(job.wsTar)
	if err != nil {
		return fmt.Errorf("build: create workspace temp: %w", err)
	}
	h := sha256.New()
	n, cpErr := io.Copy(io.MultiWriter(f, h), ws)
	cerr := f.Close()
	if cpErr != nil {
		return fmt.Errorf("build: store workspace: %w", cpErr)
	}
	if cerr != nil {
		return fmt.Errorf("build: close workspace temp: %w", cerr)
	}
	if job.spec.WorkspaceSize > 0 && n != job.spec.WorkspaceSize {
		return fmt.Errorf("%w: 期望 %d 字节,收到 %d", ErrSharedChecksum, job.spec.WorkspaceSize, n)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if job.spec.WorkspaceSHA256 != "" && got != job.spec.WorkspaceSHA256 {
		return fmt.Errorf("%w: sha256 不符", ErrSharedChecksum)
	}
	return nil
}

// run 是任务的执行主线:排队取机(共享 FIFO)→ 上机 → 执行 → 收产物 → 终态。
// ctx 在 Accept 入册时创建(background + 任务超时),与 HTTP 请求解耦;取消经 Cancel 注入。
func (s *SharedBuildServer) run(job *sharedBuildJob) {
	s.mu.Lock()
	ctx, cancel := job.ctx, job.cancel
	s.mu.Unlock()
	if ctx == nil || cancel == nil {
		return // 防御:入册即建,不应发生
	}
	defer cancel()

	logf := func(stream, line string) { s.emitLog(job, stream, line) }
	// 亲和键:同一发起方的同一 run 尽量复用同一台共享机(命中镜像层缓存),与本地任务口径一致。
	affinity := "mesh:" + job.peerNodeID + ":" + job.spec.RunID
	serverID, serverName, release, err := s.sched.AcquireShared(ctx, affinity, job.spec.Selector, func(msg string) { logf(streamStdout, msg) })
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
			s.finish(job, mesh.SharedJobStateCanceled, 0, "等待共享构建机期间取消")
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			s.finish(job, mesh.SharedJobStateFailed, 0, "任务超时(等待共享构建机)")
		default:
			s.finish(job, mesh.SharedJobStateFailed, 0, "取机失败:"+err.Error())
		}
		return
	}
	defer release()

	s.mu.Lock()
	job.state = mesh.SharedJobStateRunning
	job.serverID = serverID
	job.serverName = serverName
	job.startedAt = time.Now().UTC()
	snapshot := s.snapshotLocked(job)
	s.mu.Unlock()
	s.broadcastStatus(job, snapshot)
	logf(streamStdout, "→ 已上共享构建机:"+serverName)

	execErr := s.execute(ctx, job, serverID, logf)
	if execErr == nil {
		s.finish(job, mesh.SharedJobStateSucceeded, 0, "")
		return
	}
	if errors.Is(execErr, run.ErrCanceled) || errors.Is(ctx.Err(), context.Canceled) {
		s.finish(job, mesh.SharedJobStateCanceled, 0, "执行期间取消")
		return
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		s.finish(job, mesh.SharedJobStateFailed, 0, "任务超时("+fmt.Sprintf("%d 分钟)", job.spec.TimeoutMinutes))
		return
	}
	s.finish(job, mesh.SharedJobStateFailed, 1, execErr.Error())
}

// execute 上机执行:传工作区 → 解包 → 按类型执行 → 收产物归档 → 清远程现场(尽力)。
func (s *SharedBuildServer) execute(ctx context.Context, job *sharedBuildJob, serverID string, logf func(stream, line string)) error {
	remoteWS := "/tmp/pipewright-shared/" + sanitizeRemoteSeg(job.spec.RunID) + "-" + sanitizeRemoteSeg(job.spec.JobNodeID)
	remoteTar := remoteWS + ".tar.gz"
	// 清理在 upload 之前注册并带上 tar 包(与 remote_stage_exec 同口径):传输中断/解包失败/取消
	// 都会绕过"解包成功后才挂 defer"的写法;rm 对不存在路径幂等。
	defer func() {
		rmCmd := []string{"rm", "-rf", remoteWS, remoteTar, remoteWS + "-artifacts.tar.gz", remoteWS + "-image.tar.gz"}
		logf(streamStdout, "$ "+strings.Join(rmCmd, " "))
		_, _ = s.tgt.Exec(context.WithoutCancel(ctx), serverID, rmCmd)
	}()

	// 1) 工作区上传 + 解包(命令形态与 remote_stage_exec 同口径:array + 位置参数转义)。
	f, err := os.Open(job.wsTar)
	if err != nil {
		return fmt.Errorf("读取工作区暂存失败:%w", err)
	}
	logf(streamStdout, "→ 传输工作区到共享构建机…")
	upErr := s.tgt.Upload(ctx, serverID, f, remoteTar)
	_ = f.Close()
	if upErr == nil {
		// 上机完成,本端暂存即无价值(发起方字节已在共享机上)。
		_ = os.Remove(job.wsTar)
		s.mu.Lock()
		job.wsTar = ""
		s.mu.Unlock()
	}
	if upErr != nil {
		return fmt.Errorf("传输工作区失败:%w", upErr)
	}
	unpackCmd := []string{"sh", "-c", `mkdir -p "$0" && tar -xzf "$1" -C "$0" && rm -f "$1"`, remoteWS, remoteTar}
	logf(streamStdout, "$ "+strings.Join(unpackCmd, " "))
	if out, eerr := s.tgt.Exec(ctx, serverID, unpackCmd); eerr != nil || (out != nil && out.ExitCode != 0) {
		detail := ""
		if out != nil {
			if d := truncateLine(strings.TrimSpace(out.Stderr), remoteErrDetailLimit); d != "" {
				detail = ":" + d
			}
		}
		return errors.New("共享机解包工作区失败" + detail)
	}

	// 2) 按类型执行。
	switch job.spec.JobType {
	case mesh.SharedJobTypeScript:
		if err := s.execSharedScript(ctx, job, serverID, remoteWS, logf); err != nil {
			return err
		}
		return s.collectSharedArtifacts(ctx, job, serverID, remoteWS, logf)
	case mesh.SharedJobTypeBuildImage:
		return s.execSharedBuildImage(ctx, job, serverID, remoteWS, logf)
	default:
		return fmt.Errorf("未知任务类型 %q", job.spec.JobType)
	}
}

// execSharedScript 在共享机上用容器执行 script 命令(镜像/环境/资源规格全来自 spec;
// 环境变量为发起方已解析值,本端不经 vault —— 命令形态与 runScriptOnDriver 对齐)。
func (s *SharedBuildServer) execSharedScript(ctx context.Context, job *sharedBuildJob, serverID, remoteWS string, logf func(stream, line string)) error {
	bin := DetectRemoteCLIWithLog(ctx, s.tgt, serverID, func(line string) { logf(streamStdout, line) })
	logf(streamStdout, "共享机容器 CLI:"+bin)
	driver := NewRemoteDriver(s.tgt, serverID, bin)

	spec := job.spec
	workdir := scriptWorkspaceMount
	if sub := strings.TrimSpace(spec.Workdir); sub != "" {
		workdir = joinContainerPath(scriptWorkspaceMount, sub)
	}
	script := "set -e\n" + strings.Join(spec.Commands, "\n")
	cmd := []string{"sh", "-c", script}
	res := pipeline.Resource{CPU: spec.ResourceCPU, Memory: spec.ResourceMemory}
	code, err := driver.RunToolchain(ctx, spec.Image, remoteWS, workdir, spec.Env, cmd, res, func(stream, line string) { logf(stream, line) })
	if err != nil && code < 0 {
		if errors.Is(ctx.Err(), context.Canceled) {
			return run.ErrCanceled
		}
		return fmt.Errorf("容器无法启动:%w", err)
	}
	if code != 0 {
		return fmt.Errorf("构建命令退出码 %d", code)
	}
	return nil
}

// collectSharedArtifacts 把 spec.ArtifactPaths(shell glob)在共享机工作区内打包为 tar.gz
// 并回拉本端暂存。glob 在远端 sh 展开;零命中时归档不落地(hasResult=false,任务仍成功,
// 由发起方按其产物配置如实记录「未匹配产物」—— 分类/重命名等产物策略全部留在发起方)。
func (s *SharedBuildServer) collectSharedArtifacts(ctx context.Context, job *sharedBuildJob, serverID, remoteWS string, logf func(stream, line string)) error {
	if len(job.spec.ArtifactPaths) == 0 {
		return nil
	}
	remoteOut := remoteWS + "-artifacts.tar.gz"
	args := append([]string{"sh", "-c",
		`cd "$0" && tar czf "$1" "$@" 2>/dev/null || true; tar -tzf "$1" >/dev/null 2>&1 || rm -f "$1"`,
		remoteWS, remoteOut}, job.spec.ArtifactPaths...)
	logf(streamStdout, "$ "+strings.Join(args, " "))
	if out, err := s.tgt.Exec(ctx, serverID, args); err != nil || (out != nil && out.ExitCode != 0) {
		detail := ""
		if out != nil {
			if d := truncateLine(strings.TrimSpace(out.Stderr), remoteErrDetailLimit); d != "" {
				detail = ":" + d
			}
		}
		return fmt.Errorf("共享机产物打包失败%s", detail)
	}
	return s.pullResult(ctx, job, serverID, remoteOut, logf)
}

// execSharedBuildImage 在共享机上执行 build_image:Builder 浅拷贝换远程 driver 后复用
// b.build(构建输入全来自 spec,构建变量为发起方已解析值)→ docker save 成归档回拉。
// 产物是镜像本地 tag(与发起方本地构建同构);推送留在发起方(load 后用自己的凭据推自己的
// registry)—— 本端绝不做 docker login/push,不接触发起方 registry 凭据。
func (s *SharedBuildServer) execSharedBuildImage(ctx context.Context, job *sharedBuildJob, serverID, remoteWS string, logf func(stream, line string)) error {
	is := job.spec.ImageSpec
	bin := DetectRemoteCLIWithLog(ctx, s.tgt, serverID, func(line string) { logf(streamStdout, line) })
	logf(streamStdout, "共享机容器 CLI:"+bin)
	rb := *s.b
	rb.driver = NewRemoteDriver(s.tgt, serverID, bin)
	// 构建后清共享机悬空镜像(尽力,成败/取消都执行):rb.driver 是 SSH 驱动,prune 落到共享机自己
	// 的 daemon;WithoutCancel 保证取消后清理仍发。
	defer rb.pruneImagesBestEffort(context.WithoutCancel(ctx), func(stream, line string) { logf(stream, line) })

	cfg := pipeline.BuildConfig{
		Model:          is.BuildModel,
		DockerfilePath: is.DockerfilePath,
		Context:        is.Context,
		Toolchain:      pipeline.Toolchain{Language: is.ToolchainLanguage, Version: is.ToolchainVersion},
		ArtifactType:   pipeline.ArtifactImage,
		Vars:           plainBuildVars(is.Vars),
	}
	if cfg.Model == "" {
		cfg.Model = pipeline.BuildModelDockerfile
	}
	// 合成项目视图:b.build 仅消费名称(镜像 localTag 命名,与发起方本地构建同构)。
	proj := &project.Project{ID: "mesh-shared", Name: is.ProjectName}
	localTag, _, berr := rb.build(ctx, &sharedLogSink{logf: logf}, 0, proj, cfg, remoteWS, is.CommitTag)
	if berr != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return run.ErrCanceled
		}
		return fmt.Errorf("镜像构建失败:%v", berr)
	}

	// docker save | gzip 成归档(save 的 tar 已未压缩,体积大;gzip 换传输带宽)。
	remoteOut := remoteWS + "-image.tar.gz"
	saveCmd := []string{"sh", "-c", `"$0" save "$1" | gzip > "$2"`, bin, localTag, remoteOut}
	logf(streamStdout, "$ "+strings.Join(saveCmd, " "))
	if out, err := s.tgt.Exec(ctx, serverID, saveCmd); err != nil || (out != nil && out.ExitCode != 0) {
		detail := ""
		if out != nil {
			if d := truncateLine(strings.TrimSpace(out.Stderr), remoteErrDetailLimit); d != "" {
				detail = ":" + d
			}
		}
		return fmt.Errorf("共享机镜像归档失败(docker save)%s", detail)
	}
	logf(streamStdout, "→ 镜像已归档,回传发起方…")
	return s.pullResult(ctx, job, serverID, remoteOut, logf)
}

// pullResult 把共享机上的归档回拉本端 OS 临时文件,记 sha256/大小供 result 端点下发。
func (s *SharedBuildServer) pullResult(ctx context.Context, job *sharedBuildJob, serverID, remotePath string, logf func(stream, line string)) error {
	tmp, err := os.CreateTemp("", "pipewright-shared-result-*.tar.gz")
	if err != nil {
		return fmt.Errorf("创建产物暂存失败:%w", err)
	}
	local := tmp.Name()
	h := sha256.New()
	_, dlErr := s.tgt.Download(ctx, serverID, remotePath, io.MultiWriter(tmp, h))
	cerr := tmp.Close()
	if dlErr != nil || cerr != nil {
		_ = os.Remove(local)
		return fmt.Errorf("回拉产物归档失败:%v", dlErr)
	}
	fi, serr := os.Stat(local)
	if serr != nil {
		_ = os.Remove(local)
		return fmt.Errorf("回拉产物归档失败:%v", serr)
	}
	s.mu.Lock()
	job.resultPath = local
	job.resultSHA = hex.EncodeToString(h.Sum(nil))
	job.resultSize = fi.Size()
	s.mu.Unlock()
	logf(streamStdout, fmt.Sprintf("✓ 产物归档就绪(%d 字节),等待发起方回拉", fi.Size()))
	return nil
}

// finish 置终态、通知订阅者、挂 TTL 清理与审计。
func (s *SharedBuildServer) finish(job *sharedBuildJob, state string, exitCode int, errMsg string) {
	s.mu.Lock()
	job.state = state
	job.exitCode = exitCode
	job.errMsg = errMsg
	job.endedAt = time.Now().UTC()
	snapshot := s.snapshotLocked(job)
	for ch := range job.subs {
		sendSharedEvent(ch, mesh.SharedJobEvent{Event: "status", Status: &snapshot})
		close(ch)
		delete(job.subs, ch)
	}
	s.mu.Unlock()

	if s.OnEvent != nil {
		s.OnEvent(state, snapshot)
	}

	// TTL 后清理暂存文件并在册任务移除(成功任务的产物窗口 / 失败任务的残局)。
	time.AfterFunc(sharedResultTTL, func() {
		s.mu.Lock()
		delete(s.jobs, job.id)
		ws, res := job.wsTar, job.resultPath
		s.mu.Unlock()
		if ws != "" {
			_ = os.Remove(ws)
		}
		if res != "" {
			_ = os.Remove(res)
		}
	})
}

// emitLog 追加日志进 ring 并广播给在册订阅者(慢消费者丢行:有 ring 回放与状态轮询兜底)。
func (s *SharedBuildServer) emitLog(job *sharedBuildJob, stream, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(job.logs) >= sharedJobLogRingMax {
		// 环满:丢最旧的一半并记数(防长构建无限增长;新订阅者拿不到最旧日志,如实计数)。
		job.logs = job.logs[len(job.logs)/2:]
		job.logsDropped += len(job.logs)/2 + 1
	}
	ev := mesh.SharedJobEvent{Event: "log", Stream: stream, Line: line}
	job.logs = append(job.logs, ev)
	for ch := range job.subs {
		sendSharedEvent(ch, ev)
	}
}

// broadcastStatus 广播一次状态快照(持锁外调用;快照已拷贝)。
func (s *SharedBuildServer) broadcastStatus(job *sharedBuildJob, snapshot mesh.SharedJobStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range job.subs {
		sendSharedEvent(ch, mesh.SharedJobEvent{Event: "status", Status: &snapshot})
	}
}

// sendSharedEvent 非阻塞投递(缓冲满 = 消费者太慢,丢行保活;订阅 chan 容量 = ring 上限+余量,
// 正常消费不丢)。
func sendSharedEvent(ch chan mesh.SharedJobEvent, ev mesh.SharedJobEvent) {
	select {
	case ch <- ev:
	default:
	}
}

// Subscribe 订阅任务事件:先回放 ring 历史日志,再实时推送;终态事件送达后 chan 关闭。
// 返回取消订阅函数。任务不存在 → ErrSharedJobNotFound。
func (s *SharedBuildServer) Subscribe(jobID string) (<-chan mesh.SharedJobEvent, func(), error) {
	ch := make(chan mesh.SharedJobEvent, sharedJobLogRingMax+64)
	s.mu.Lock()
	job, ok := s.jobs[jobID]
	if !ok {
		s.mu.Unlock()
		return nil, nil, ErrSharedJobNotFound
	}
	// 回放与注册同一临界区:与 emitLog 的追加/广播互斥,保证不重不漏且顺序一致。
	for _, ev := range job.logs {
		ch <- ev // 容量 ≥ ring 上限,必不阻塞
	}
	snapshot := s.snapshotLocked(job)
	if isTerminalState(snapshot.State) {
		st := snapshot
		ch <- mesh.SharedJobEvent{Event: "status", Status: &st}
		close(ch)
		s.mu.Unlock()
		return ch, func() {}, nil
	}
	job.subs[ch] = struct{}{}
	s.mu.Unlock()

	unsub := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := job.subs[ch]; ok {
			delete(job.subs, ch)
			close(ch)
		}
	}
	return ch, unsub, nil
}

// Status 返回任务状态快照(轮询兜底)。
func (s *SharedBuildServer) Status(jobID string) (mesh.SharedJobStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return mesh.SharedJobStatus{}, ErrSharedJobNotFound
	}
	return s.snapshotLocked(job), nil
}

// Result 返回产物归档的暂存路径与完整性元数据(终态成功且有声明的产物时可用)。
func (s *SharedBuildServer) Result(jobID string) (path, sha string, size int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return "", "", 0, ErrSharedJobNotFound
	}
	if job.state != mesh.SharedJobStateSucceeded || job.resultPath == "" {
		return "", "", 0, fmt.Errorf("build: shared job result not ready (state=%s)", job.state)
	}
	return job.resultPath, job.resultSHA, job.resultSize, nil
}

// Cancel 取消任务:排队中撤销等待,运行中中断执行;已终态为幂等 no-op。
func (s *SharedBuildServer) Cancel(jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return ErrSharedJobNotFound
	}
	if isTerminalState(job.state) {
		return nil
	}
	if job.cancel != nil {
		job.cancel()
	}
	return nil
}

// List 返回全部在册任务快照(管理页;按受理序)。
func (s *SharedBuildServer) List() []mesh.SharedJobStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]mesh.SharedJobStatus, 0, len(s.jobs))
	for _, job := range s.jobs {
		out = append(out, s.snapshotLocked(job))
	}
	// 稳定序:受理时间升序(map 迭代无序)。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].QueuedAt > out[j].QueuedAt; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// CatalogMachines 返回共享构建机目录条目(shared=1 且 standard 且已打标签;脱敏:仅调度元数据)。
func (s *SharedBuildServer) CatalogMachines(ctx context.Context) ([]mesh.CatalogBuildMachine, error) {
	all, err := s.machines.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]mesh.CatalogBuildMachine, 0, 4)
	for _, srv := range all {
		if !srv.SharedBuild || srv.IsPVENode() || strings.TrimSpace(srv.Labels) == "" {
			continue
		}
		out = append(out, mesh.CatalogBuildMachine{
			ServerID:   srv.ID,
			ServerName: srv.Name,
			Labels:     srv.Labels,
			MaxBuilds:  srv.MaxBuilds,
			Priority:   srv.Priority,
			Gpu:        srv.Gpu,
		})
	}
	return out, nil
}

// snapshotLocked 生成对外状态快照(须持 s.mu)。
func (s *SharedBuildServer) snapshotLocked(job *sharedBuildJob) mesh.SharedJobStatus {
	st := mesh.SharedJobStatus{
		JobID:      job.id,
		State:      job.state,
		Peer:       job.peerName,
		RunID:      job.spec.RunID,
		JobName:    job.spec.JobName,
		ServerID:   job.serverID,
		ServerName: job.serverName,
		ExitCode:   job.exitCode,
		Error:      job.errMsg,
		HasResult:  job.state == mesh.SharedJobStateSucceeded && job.resultPath != "",
		QueuedAt:   job.queuedAt.Format(time.RFC3339),
	}
	if !job.startedAt.IsZero() {
		st.StartedAt = job.startedAt.Format(time.RFC3339)
	}
	if !job.endedAt.IsZero() {
		st.EndedAt = job.endedAt.Format(time.RFC3339)
	}
	return st
}

// isTerminalState 报告状态是否终态。
func isTerminalState(state string) bool {
	switch state {
	case mesh.SharedJobStateSucceeded, mesh.SharedJobStateFailed, mesh.SharedJobStateCanceled:
		return true
	}
	return false
}

// sharedLogSink 把 build 构建日志桥接到共享任务的 SSE 日志通道(其余 StepSink 方法 no-op:
// 提供方不落 run_steps/制品,构建产物以归档回传代替)。
type sharedLogSink struct {
	logf func(stream, line string)
}

func (s *sharedLogSink) Plan(context.Context, []run.StepDecl) error          { return nil }
func (s *sharedLogSink) StepRunning(context.Context, int) error              { return nil }
func (s *sharedLogSink) StepDone(context.Context, int, string) error         { return nil }
func (s *sharedLogSink) SetFailureLog(context.Context, string) error         { return nil }
func (s *sharedLogSink) SetSpecSource(context.Context, run.SpecSource) error { return nil }
func (s *sharedLogSink) EmitArtifact(context.Context, run.Artifact) error    { return nil }
func (s *sharedLogSink) Log(_ context.Context, stream string, _ int, line string) error {
	s.logf(stream, line)
	return nil
}

// plainBuildVars 把发起方已解析的 K=V 列表转回 BuildVar(Secret=false:本端 buildArgs 直通,
// 不触碰本端 vault —— 发起方 secret 到此即值,不再按引用解析)。
func plainBuildVars(pairs []string) []pipeline.BuildVar {
	out := make([]pipeline.BuildVar, 0, len(pairs))
	for _, p := range pairs {
		k, v, _ := strings.Cut(p, "=")
		if k == "" {
			continue
		}
		out = append(out, pipeline.BuildVar{Key: k, Value: v})
	}
	return out
}
