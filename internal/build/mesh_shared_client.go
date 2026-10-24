package build

// mesh_shared_client.go 实现集群级共享构建机的发起方(0086):把含 `peer:<环境名>` 选择器的
// 构建节点派发到对端环境的共享机上执行 ——
//   - 本地克隆(含恢复上游产物/被推送源物化;token 只在本端)→ 打包工作区 → 经 mesh 流式
//     提交对端(排队/上机在对端调度器,跨环境 FIFO 语义);
//   - SSE 日志实时回流(断线重连 + 轮询兜底,awaitMeshRun 同模式);
//   - 产物回传:script 产物归档解包后走本端 collectScriptArtifacts 分类入制品库;
//     build_image 的 docker save 归档 load 进本端 daemon,再按本端推送计划推送 ——
//     registry 凭据与产物落库都只在本端,对端(共享机所属环境)只出算力。
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/dagrun"
	"github.com/huangchengsir/pipewright/internal/mesh"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/runner"
)

// sharedBuildGateway 抽象发起方对共享构建端点的调用面(meshSharedGateway 实现;测试可 fake)。
type sharedBuildGateway interface {
	// ResolvePeer 按环境名或 nodeID 解析对端(未知/名称歧义 → 明确错误)。
	ResolvePeer(ctx context.Context, nameOrID string) (*mesh.Peer, error)
	Submit(ctx context.Context, peer *mesh.Peer, spec *mesh.SharedJobSpec, workspace io.Reader) (jobID string, err error)
	Stream(ctx context.Context, peer *mesh.Peer, jobID string, onEvent func(mesh.SharedJobEvent) bool) error
	Status(ctx context.Context, peer *mesh.Peer, jobID string) (*mesh.SharedJobStatus, error)
	FetchResult(ctx context.Context, peer *mesh.Peer, jobID string, w io.Writer) error
	Cancel(ctx context.Context, peer *mesh.Peer, jobID string) error
}

// meshSharedGateway 是基于 mesh.Service + mesh.Client 的真实网关(装配层注入)。
type meshSharedGateway struct {
	svc    mesh.Service
	client *mesh.Client
}

// newMeshSharedGateway 构造网关(svc/client 为 nil → 返回 nil,功能整体缺席)。
func newMeshSharedGateway(svc mesh.Service, client *mesh.Client) *meshSharedGateway {
	if svc == nil || client == nil {
		return nil
	}
	return &meshSharedGateway{svc: svc, client: client}
}

// ResolvePeer 按 nodeID 精确匹配优先,其次按环境名精确匹配;名称命中多台 → 歧义错误
// (选择器引用的是环境身份,歧义必须显式失败而非随机取一)。
func (g *meshSharedGateway) ResolvePeer(ctx context.Context, nameOrID string) (*mesh.Peer, error) {
	peers, err := g.svc.ListPeers(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取集群节点注册表失败:%w", err)
	}
	for i := range peers {
		if peers[i].NodeID == nameOrID {
			return &peers[i], nil
		}
	}
	var byName []*mesh.Peer
	for i := range peers {
		if peers[i].Name != "" && peers[i].Name == nameOrID {
			byName = append(byName, &peers[i])
		}
	}
	switch len(byName) {
	case 1:
		return byName[0], nil
	case 0:
		return nil, fmt.Errorf("集群中不存在环境 %q(先在对端开启集群并完成发现,或改用 nodeID)", nameOrID)
	default:
		return nil, fmt.Errorf("环境名 %q 命中 %d 个节点,存在歧义;请改用 peer:<nodeID>", nameOrID, len(byName))
	}
}

func (g *meshSharedGateway) Submit(ctx context.Context, peer *mesh.Peer, spec *mesh.SharedJobSpec, workspace io.Reader) (string, error) {
	acc, err := g.client.SubmitSharedJob(ctx, peer, spec, workspace)
	if err != nil {
		return "", err
	}
	return acc.JobID, nil
}

func (g *meshSharedGateway) Stream(ctx context.Context, peer *mesh.Peer, jobID string, onEvent func(mesh.SharedJobEvent) bool) error {
	return g.client.StreamSharedJobEvents(ctx, peer, jobID, onEvent)
}

func (g *meshSharedGateway) Status(ctx context.Context, peer *mesh.Peer, jobID string) (*mesh.SharedJobStatus, error) {
	return g.client.GetSharedJob(ctx, peer, jobID)
}

func (g *meshSharedGateway) FetchResult(ctx context.Context, peer *mesh.Peer, jobID string, w io.Writer) error {
	_, _, err := g.client.FetchSharedJobResult(ctx, peer, jobID, w)
	return err
}

func (g *meshSharedGateway) Cancel(ctx context.Context, peer *mesh.Peer, jobID string) error {
	return g.client.CancelSharedJob(ctx, peer, jobID)
}

// WithSharedBuildGateway 注入集群共享构建发起网关(mesh.Service + mesh.Client 装配;
// 任一为 nil 不改 —— 含 peer: 选择器的节点将明确失败,不静默本地执行)。
func WithSharedBuildGateway(svc mesh.Service, client *mesh.Client) BuilderOption {
	return func(b *Builder) {
		if gw := newMeshSharedGateway(svc, client); gw != nil {
			b.sharedGW = gw
		}
	}
}

// peerScopedSelector 报告选择器是否含 peer: 项(集群共享构建;非法选择器按非 peer 处理,
// 由原有路径的校验兜底报错)。
func peerScopedSelector(sel string) bool {
	_, _, ok, err := runner.SplitPeerSelector(sel)
	return err == nil && ok
}

// runJobShared 执行一个 peer: 选择器节点:本地备工作区 → 提交对端 → 跟踪 → 回收产物。
// 日志行归属「对端环境/共享机」(步骤 × 机器分组);失败映射 ErrBuildFailed / 取消映射 run.ErrCanceled。
func (b *Builder) runJobShared(ctx context.Context, r *run.Run, stage pipeline.Stage, jb pipeline.Job, jrep dagrun.StageReporter, selector string) error {
	peerName, restSel, _, serr := runner.SplitPeerSelector(selector)
	if serr != nil {
		_ = jrep.Log(ctx, streamError, "选择器非法:"+selector)
		return ErrBuildFailed
	}
	if b.sharedGW == nil {
		_ = jrep.Log(ctx, streamError, "集群共享构建未装配(本节点 mesh 未启用),无法执行 peer: 选择器")
		return ErrBuildFailed
	}
	peer, perr := b.sharedGW.ResolvePeer(ctx, peerName)
	if perr != nil {
		_ = jrep.Log(ctx, streamError, "解析 peer: 环境失败:"+perr.Error())
		return ErrBuildFailed
	}
	// 全部日志先归属对端环境;上机后再按「对端/机名」细分(历史行不回溯,与远程构建同口径)。
	lctx := run.WithLogMachine(ctx, peerDisplayName(peer))

	proj, settings, verr := b.resolve(ctx, r)
	if verr != nil {
		_ = jrep.Log(lctx, streamError, "无法加载项目构建配置:"+verr.Error())
		return ErrBuildFailed
	}

	// 1) 本地备工作区:克隆(含被推送源物化)+ 恢复上游产物 —— 源码从发起方上传,对端不触 git。
	workspace, commitTag, cleanup, cerr := b.cloneJobWorkspace(ctx, r, proj, jrep)
	if cerr != nil {
		return cerr
	}
	defer cleanup()

	// 2) 组装 spec(env/构建变量在发起方解析完毕,secret 以值随 spec 经签名通道传输)。
	spec, serr2 := b.buildSharedJobSpec(r, stage, jb, proj, settings, commitTag, restSel)
	if serr2 != nil {
		_ = jrep.Log(lctx, streamError, serr2.Error())
		return ErrBuildFailed
	}

	// 3) 打包工作区 → 流式提交。
	wsTar, err := os.CreateTemp("", "pipewright-shared-ws-*.tar.gz")
	if err != nil {
		_ = jrep.Log(lctx, streamError, "创建工作区打包文件失败:"+err.Error())
		return ErrBuildFailed
	}
	wsPath := wsTar.Name()
	defer func() { _ = os.Remove(wsPath) }()
	h := sha256.New()
	if err := tarGzDir(workspace, io.MultiWriter(wsTar, h), false); err != nil {
		_ = wsTar.Close()
		_ = jrep.Log(lctx, streamError, "打包工作区失败:"+err.Error())
		return ErrBuildFailed
	}
	fi, _ := wsTar.Stat()
	size := int64(0)
	if fi != nil {
		size = fi.Size()
	}
	if err := wsTar.Close(); err != nil {
		_ = jrep.Log(lctx, streamError, "写工作区打包文件失败:"+err.Error())
		return ErrBuildFailed
	}
	spec.WorkspaceSHA256 = hex.EncodeToString(h.Sum(nil))
	spec.WorkspaceSize = size

	_ = jrep.Log(lctx, streamStdout, fmt.Sprintf("→ 节点「%s」→ 集群共享构建机(对端 %s,选择器 %s):上传工作区(%d 字节)…", jb.Name, peerDisplayName(peer), restSel, size))
	fr, ferr := os.Open(wsPath)
	if ferr != nil {
		_ = jrep.Log(lctx, streamError, "读取工作区打包文件失败:"+ferr.Error())
		return ErrBuildFailed
	}
	jobID, aerr := b.sharedGW.Submit(ctx, peer, spec, fr)
	_ = fr.Close()
	if aerr != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return run.ErrCanceled
		}
		_ = jrep.Log(lctx, streamError, "提交对端共享构建失败:"+aerr.Error())
		return ErrBuildFailed
	}

	// 4) 跟踪到终态(SSE 实时日志 + 断线轮询兜底);本端取消 → 传播取消对端任务。
	st, terr := b.trackSharedJob(ctx, jrep, lctx, peer, jobID)
	if ctx.Err() != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		_ = b.sharedGW.Cancel(cctx, peer, jobID)
		cancel()
	}
	if terr != nil {
		return terr
	}

	// 5) 产物回收(成功且对端有归档时)。
	if st == nil || !st.HasResult {
		_ = jrep.Log(lctx, streamStdout, "✓ 集群共享构建完成(无声明产物,不回传)")
		return nil
	}
	return b.recoverSharedResult(ctx, r, stage, jb, jrep, lctx, proj, settings, peer, jobID, spec)
}

// buildSharedJobSpec 按节点类型组装对端任务 spec(env/vars 全部在本端解析为值)。
func (b *Builder) buildSharedJobSpec(r *run.Run, stage pipeline.Stage, jb pipeline.Job, proj *project.Project, settings *pipeline.Settings, commitTag, restSel string) (*mesh.SharedJobSpec, error) {
	spec := &mesh.SharedJobSpec{
		RunID:     r.ID,
		StageName: stage.Name,
		JobNodeID: jb.ID,
		JobName:   jb.Name,
		Selector:  restSel,
	}
	switch {
	case isScriptJob(jb.Type):
		step, verr := scriptStepFromJob(jb)
		if verr != nil {
			return nil, fmt.Errorf("script job「%s」配置无效:%v", jb.Name, verr)
		}
		spec.JobType = mesh.SharedJobTypeScript
		// 注入序与本地对齐:运行参数 → 流水线级变量(含 secret,本端 vault 解析为值)→
		// job env → PIPEWRIGHT_ENV;远程容器里不二次解析。
		base := runParamsAsEnv(r.Trigger.Params)
		if settings != nil && len(settings.Build.Vars) > 0 {
			base = append(base, settings.Build.Vars...)
		}
		step.Env = append(base, step.Env...)
		step.Env = append(step.Env, pipewrightEnvVar())
		plain, secret := b.buildArgs(step.Env)
		spec.Image = step.Image
		spec.Workdir = strings.TrimSpace(step.WorkDir)
		spec.Commands = step.Commands
		spec.Env = append(append([]string{}, plain...), secret...)
		spec.ResourceCPU = strings.TrimSpace(step.Resource.CPU)
		spec.ResourceMemory = strings.TrimSpace(step.Resource.Memory)
		// 产物收集路径:渲染 {{参数}} 后按行拆分(与 collectScriptArtifacts 同口径;
		// 分类/打包/重命名策略留在本端回收侧)。
		for _, rel := range splitCommands(renderTemplate(cfgString(jb.Config, "artifactPath"), templateContext(jb.Config))) {
			if rel = strings.TrimSpace(rel); rel != "" {
				spec.ArtifactPaths = append(spec.ArtifactPaths, rel)
			}
		}
	case isBuildImageJob(jb.Type):
		// 与远程构建同边界:jar/dist 产物文件无法跨环境回收 → 诚实失败。
		if at := strings.TrimSpace(cfgString(jb.Config, "artifactType")); at != "" && at != pipeline.ArtifactImage {
			return nil, fmt.Errorf("构建节点「%s」:集群共享构建机暂仅支持镜像(image)产物;jar/dist 产物请配置为本地执行", jb.Name)
		}
		spec.JobType = mesh.SharedJobTypeBuildImage
		var vars []pipeline.BuildVar
		if settings != nil {
			vars = settings.Build.Vars
		}
		plain, secret := b.buildArgs(vars)
		spec.ImageSpec = &mesh.SharedImageSpec{
			BuildModel:        cfgString(jb.Config, "buildModel"),
			DockerfilePath:    cfgString(jb.Config, "dockerfilePath"),
			Context:           cfgString(jb.Config, "context"),
			BuildTarget:       cfgString(jb.Config, "buildTarget"),
			ToolchainLanguage: cfgString(jb.Config, "toolchainLanguage"),
			ToolchainVersion:  cfgString(jb.Config, "toolchainVersion"),
			ProjectName:       proj.Name,
			CommitTag:         commitTag,
			Vars:              append(append([]string{}, plain...), secret...),
		}
	default:
		return nil, fmt.Errorf("节点「%s」(%s)不支持集群共享构建机执行", jb.Name, jb.Type)
	}
	return spec, nil
}

// trackSharedJob 跟踪任务到终态:实时回流日志/状态,断线时轮询兜底 + 退避重试。
// 返回终态快照(对端不可达超时 → 错误,与 awaitMeshRun 的兜底节奏一致)。
func (b *Builder) trackSharedJob(ctx context.Context, jrep dagrun.StageReporter, lctx context.Context, peer *mesh.Peer, jobID string) (*mesh.SharedJobStatus, error) {
	logf := func(stream, line string) { _ = jrep.Log(lctx, stream, line) }
	var terminal *mesh.SharedJobStatus
	for {
		if ctx.Err() != nil {
			return nil, run.ErrCanceled
		}
		sctx, cancel := context.WithCancel(ctx)
		err := b.sharedGW.Stream(sctx, peer, jobID, func(ev mesh.SharedJobEvent) bool {
			switch ev.Event {
			case "log":
				if ev.Line != "" {
					logf(ev.Stream, ev.Line)
				}
			case "status":
				if ev.Status != nil {
					terminal = ev.Status
					if ev.Status.State == mesh.SharedJobStateRunning && ev.Status.ServerName != "" {
						_ = jrep.Log(run.WithLogMachine(ctx, peerDisplayName(peer)+"/"+ev.Status.ServerName), streamStdout,
							fmt.Sprintf("· 已上共享构建机:%s", ev.Status.ServerName))
					}
				}
			}
			return terminal == nil
		})
		cancel()
		if terminal != nil {
			break
		}
		if err == nil {
			// 流正常结束但未见终态(如对端收流):轮询兜底。
			err = fmt.Errorf("event stream ended without terminal status")
		}
		if ctx.Err() != nil {
			return nil, run.ErrCanceled
		}
		// 轮询兜底(对端重启/断连后任务可能已 404 = 丢失)。
		if st, perr := b.sharedGW.Status(ctx, peer, jobID); perr == nil {
			if isSharedTerminal(st.State) {
				terminal = st
				break
			}
		} else if strings.Contains(perr.Error(), "404") {
			return nil, fmt.Errorf("对端任务丢失(对端环境可能重启),本次构建失败")
		}
		logf(streamStdout, "· 与对端事件流断开,3s 后重连…")
		select {
		case <-ctx.Done():
			return nil, run.ErrCanceled
		case <-time.After(3 * time.Second):
		}
	}
	switch terminal.State {
	case mesh.SharedJobStateSucceeded:
		return terminal, nil
	case mesh.SharedJobStateCanceled:
		return nil, run.ErrCanceled
	default:
		msg := terminal.Error
		if msg == "" {
			msg = "对端执行失败"
		}
		logf(streamError, "✗ 集群共享构建失败:"+msg)
		return nil, ErrBuildFailed
	}
}

// isSharedTerminal 报告对端任务状态是否终态。
func isSharedTerminal(state string) bool {
	switch state {
	case mesh.SharedJobStateSucceeded, mesh.SharedJobStateFailed, mesh.SharedJobStateCanceled:
		return true
	}
	return false
}

// recoverSharedResult 回拉产物归档并按类型回收:script → 解包 + collectScriptArtifacts 分类
// 入本端制品库;build_image → 本端 docker load + 按本端推送计划 push(凭据不出本端)。
func (b *Builder) recoverSharedResult(ctx context.Context, r *run.Run, stage pipeline.Stage, jb pipeline.Job, jrep dagrun.StageReporter, lctx context.Context, proj *project.Project, settings *pipeline.Settings, peer *mesh.Peer, jobID string, spec *mesh.SharedJobSpec) error {
	logf := func(stream, line string) { _ = jrep.Log(lctx, stream, line) }
	resPath, err := func() (string, error) {
		f, ferr := os.CreateTemp("", "pipewright-shared-result-*.tar.gz")
		if ferr != nil {
			return "", ferr
		}
		defer func() { _ = f.Close() }()
		if ferr := b.sharedGW.FetchResult(ctx, peer, jobID, f); ferr != nil {
			return "", ferr
		}
		return f.Name(), nil
	}()
	defer func() { _ = os.Remove(resPath) }()
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return run.ErrCanceled
		}
		logf(streamError, "回拉产物归档失败:"+err.Error())
		return ErrBuildFailed
	}
	logf(streamStdout, "✓ 产物归档已回传,本端回收…")

	switch spec.JobType {
	case mesh.SharedJobTypeScript:
		out := resPath + ".d"
		rf, rerr := os.Open(resPath)
		if rerr != nil {
			logf(streamError, "读取产物归档失败:"+rerr.Error())
			return ErrBuildFailed
		}
		extErr := extractTarGzTo(rf, out)
		_ = rf.Close()
		if extErr != nil {
			_ = os.RemoveAll(out)
			logf(streamError, "解包产物归档失败:"+extErr.Error())
			return ErrBuildFailed
		}
		defer func() { _ = os.RemoveAll(out) }()
		b.collectScriptArtifacts(ctx, []pipeline.Job{jb}, out, slugify(proj.Name), stage.Name, jrep)
		return nil
	case mesh.SharedJobTypeBuildImage:
		return b.loadAndPushSharedImage(ctx, r, stage, jb, jrep, lctx, proj, settings, spec, resPath)
	default:
		return ErrBuildFailed
	}
}

// loadAndPushSharedImage 把回传的镜像归档 load 进本端 daemon,构造产物并按本端推送计划推送
// (三态语义与本地构建一致;推送经本端 driver,凭据/registry 全在本端解析)。
func (b *Builder) loadAndPushSharedImage(ctx context.Context, r *run.Run, stage pipeline.Stage, jb pipeline.Job, jrep dagrun.StageReporter, lctx context.Context, proj *project.Project, settings *pipeline.Settings, spec *mesh.SharedJobSpec, imageTar string) error {
	logf := func(stream, line string) { _ = jrep.Log(lctx, stream, line) }
	loader, ok := b.driver.(interface {
		Load(ctx context.Context, tarPath string, onLine func(stream, line string)) (int, error)
	})
	if !ok {
		logf(streamError, "本端容器驱动不支持镜像导入(瘦控制机无容器 CLI),集群共享 build_image 不可用;请在本端装配容器 CLI 或改用 script 构建产物")
		return ErrBuildFailed
	}
	if code, lerr := loader.Load(ctx, imageTar, logf); lerr != nil || code != 0 {
		if errors.Is(ctx.Err(), context.Canceled) {
			return run.ErrCanceled
		}
		logf(streamError, fmt.Sprintf("镜像导入本端失败(退出码 %d)", code))
		return ErrBuildFailed
	}
	slug := slugify(proj.Name)
	// 与共享机构建侧同构的镜像名(含 target 后缀,见 imageArtName):load 进来的镜像带的就是
	// 这个 tag,按它 inspect/推送。
	artName := imageArtName(slug, spec.ImageSpec.BuildTarget)
	localTag := localTagPrefix + "/" + artName + ":" + spec.ImageSpec.CommitTag
	digest, size, _ := b.driver.InspectImage(ctx, localTag)
	art := &run.Artifact{
		Type:      run.ArtifactImage,
		Name:      artName,
		Reference: localTag,
		SizeBytes: size,
		Metadata:  map[string]any{"builder": b.driver.Binary(), "builtOn": "shared:" + spec.ImageSpec.ProjectName},
	}
	if digest != "" {
		art.Metadata["digest"] = digest
	}
	var plan imagePushPlan
	if settings != nil {
		plans := b.pushPlansByBuildNode(ctx, stage.Jobs, settings, r)
		plan = plans[jb.Name]
	}
	sink := &reporterSink{rep: jrep}
	perr := b.pushImageArtifact(ctx, sink, jrep, jb, stage.Name, proj, plan, art, localTag, spec.ImageSpec.CommitTag)
	// 发起方本端也轮转(load 进来的镜像副本):保留集来自本端 DB,与本地构建同语义
	// (共享机上不轮转,见 image_rotate.go)。best-effort,绝不影响构建结果。
	b.rotateBuildImages(context.WithoutCancel(ctx), proj, spec.ImageSpec.BuildTarget, spec.ImageSpec.CommitTag, logf)
	return perr
}
