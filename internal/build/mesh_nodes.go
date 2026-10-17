// mesh_nodes.go 是集群两类流水线节点的执行器(对等 mesh,经 EasyTier 虚拟 IP 直连对端):
//   - mesh_push   :推送产物/镜像到对端节点(文件走制品库流,镜像 docker save 归档直传;
//                  对端按 origin 幂等定位/自动建项目,首推建「被推送源」项目并落修订);
//   - mesh_trigger:启动对端节点的特定工作流(经对端 /api/mesh API 建运行,可等待完成并
//                  把对端步骤状态/日志回流到本节点日志)。
//
// 两类节点都只在控制机本地执行(不入 remote_stage_exec 的可远程清单)。依赖经
// WithMeshStage / WithPushedSource 选项注入,未注入时退化为明确失败日志(诚实不假装)。
package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/dagrun"
	"github.com/huangchengsir/pipewright/internal/mesh"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/pushsource"
	"github.com/huangchengsir/pipewright/internal/run"
)

// 集群节点类型常量(Job.Type 判别值)。
const (
	JobTypeMeshPush    = "mesh_push"
	JobTypeMeshTrigger = "mesh_trigger"
	// JobTypePushedSource 是「被推送的产物/镜像」源节点(pushed 项目 source 阶段的声明式源)。
	JobTypePushedSource = "pushed_source"
)

func isMeshPushJob(jobType string) bool    { return strings.TrimSpace(jobType) == JobTypeMeshPush }
func isMeshTriggerJob(jobType string) bool { return strings.TrimSpace(jobType) == JobTypeMeshTrigger }
func isPushedSourceJob(jobType string) bool {
	return strings.TrimSpace(jobType) == JobTypePushedSource
}

// meshPeerLookup 是 mesh 节点执行所需的「对端解析 + 本节点身份」最小能力面
// (mesh.Service 原样满足;收窄接口便于单测 fake)。
type meshPeerLookup interface {
	GetPeer(ctx context.Context, nodeID string) (*mesh.Peer, error)
	// NodeID 返回本节点 id(mesh_trigger 的 origin 寻址要用)。
	NodeID(ctx context.Context) (string, error)
}

// meshSvcAdapter 把 mesh.Service 适配为 meshPeerLookup。
type meshSvcAdapter struct{ svc mesh.Service }

func (a meshSvcAdapter) GetPeer(ctx context.Context, nodeID string) (*mesh.Peer, error) {
	return a.svc.GetPeer(ctx, nodeID)
}

func (a meshSvcAdapter) NodeID(ctx context.Context) (string, error) {
	st, err := a.svc.Settings(ctx)
	if err != nil {
		return "", err
	}
	return st.NodeID, nil
}

// WithMeshStage 注入集群节点执行依赖:对端解析/身份(mesh.Service)、出站签名客户端、
// 镜像导出器。任一为 nil 时对应节点退化为明确失败(未装配,不假跑)。
func WithMeshStage(svc mesh.Service, client *mesh.Client, images *ImageTransfer) BuilderOption {
	return func(b *Builder) {
		if svc != nil {
			b.meshPeers = meshSvcAdapter{svc: svc}
		}
		b.meshClient = client
		b.meshImages = images
	}
}

// WithPushedSource 注入「被推送源」物化依赖(修订存储)。nil 不改(物化退化为失败日志)。
func WithPushedSource(revs pushsource.Store) BuilderOption {
	return func(b *Builder) {
		if revs != nil {
			b.pushedRevs = revs
		}
	}
}

// resolvePushed 取本 run 的推送取用意图并解析修订(标签空=最新;条目空=全部)。
func (b *Builder) resolvePushed(ctx context.Context, r *run.Run) (*pushsource.Revision, []pushsource.Item, error) {
	if b.pushedRevs == nil {
		return nil, nil, fmt.Errorf("pushed 源未装配(修订存储缺失)")
	}
	if b.artStore == nil {
		return nil, nil, fmt.Errorf("pushed 源未装配(制品库缺失)")
	}
	revLabel := ""
	itemNames := []string{}
	if r.Trigger.Params != nil {
		revLabel = strings.TrimSpace(r.Trigger.Params[pushsource.ParamRevision])
		if raw := strings.TrimSpace(r.Trigger.Params[pushsource.ParamItems]); raw != "" {
			for _, n := range strings.Split(raw, ",") {
				if n = strings.TrimSpace(n); n != "" {
					itemNames = append(itemNames, n)
				}
			}
		}
	}
	return pushsource.Resolve(ctx, b.pushedRevs, r.ProjectID, revLabel, itemNames)
}

// runPushedSourceJob 执行 pushed_source 声明节点:解析修订 → 播报 → 把各条目登记为本 run
// 产物(file → 制品库句柄 + workspacePath;image → 本机 registry 引用),下游 deploy 节点
// 沿既有 artifactJob/artifactName 绑定消费,部署链路零改动。
func (b *Builder) runPushedSourceJob(ctx context.Context, rep dagrun.StageReporter, jb pipeline.Job, stage pipeline.Stage, r *run.Run) error {
	rev, items, err := b.resolvePushed(ctx, r)
	if err != nil {
		_ = rep.Log(ctx, streamStderr, "推送源解析失败:"+err.Error()+"(项目尚无推送修订,或所选修订/条目不存在)")
		return ErrBuildFailed
	}
	_ = rep.Log(ctx, streamStdout, fmt.Sprintf("· 推送源「%s」:修订 %s(%d 个条目,来自集群推送)", jb.Name, rev.Label, len(items)))
	for _, it := range items {
		art := run.Artifact{
			Name:      it.Name,
			Reference: it.ImageRef,
			SizeBytes: it.Size,
			Metadata:  map[string]any{"sourceStage": stage.Name, "sourceJob": jb.Name},
		}
		switch it.Kind {
		case pushsource.KindImage:
			art.Type = run.ArtifactImage
			if it.Digest != "" {
				art.Metadata["digest"] = it.Digest
			}
			_ = rep.Log(ctx, streamStdout, fmt.Sprintf("  · 镜像 %s → %s", it.Name, it.ImageRef))
		default:
			art.Type = artifactTypeForPath(it.DestName)
			art.Reference = it.SHA256 // 制品库内容寻址句柄
			dest := it.DestName
			if dest == "" {
				dest = it.Name
			}
			art.Metadata["workspacePath"] = dest
			_ = rep.Log(ctx, streamStdout, fmt.Sprintf("  · 产物 %s(%s)→ %s", it.Name, art.Type, dest))
		}
		if err := rep.EmitArtifact(ctx, art); err != nil {
			_ = rep.Log(ctx, streamStderr, "登记推送源条目失败:"+err.Error())
			return ErrBuildFailed
		}
	}
	return nil
}

// artifactTypeForPath 按落盘路径推断文件产物类型(与 collectOneFileArtifact 同口径)。
func artifactTypeForPath(p string) string {
	if strings.HasSuffix(strings.ToLower(p), ".jar") {
		return run.ArtifactJar
	}
	if strings.HasSuffix(p, ".tar.gz") || strings.HasSuffix(strings.ToLower(p), ".tgz") {
		return run.ArtifactDist
	}
	return run.ArtifactArchive
}

// materializePushedWorkspace 把推送修订的 file 条目恢复进工作区(替代 git 克隆;
// image 条目不入工作区——它们以 registry 引用形态供部署节点消费)。返回修订标签(作 commitTag)。
func (b *Builder) materializePushedWorkspace(ctx context.Context, r *run.Run, workspace string, rep dagrun.StageReporter) (string, error) {
	rev, items, err := b.resolvePushed(ctx, r)
	if err != nil {
		_ = rep.Log(ctx, streamStderr, "推送源物化失败:"+err.Error())
		return "", ErrBuildFailed
	}
	for _, it := range items {
		if it.Kind != pushsource.KindFile {
			continue
		}
		dest := it.DestName
		if dest == "" {
			dest = it.Name
		}
		clean := filepath.Clean(dest)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			_ = rep.Log(ctx, streamStderr, "推送条目路径越界,已拒绝:"+dest)
			return "", ErrBuildFailed
		}
		full := filepath.Join(workspace, clean)
		if mkErr := os.MkdirAll(filepath.Dir(full), 0o755); mkErr != nil {
			return "", fmt.Errorf("创建条目目录失败:%w", mkErr)
		}
		src, oerr := b.artStore.Open(it.SHA256)
		if oerr != nil {
			_ = rep.Log(ctx, streamStderr, "推送条目制品缺失:"+it.Name)
			return "", ErrBuildFailed
		}
		dst, derr := os.Create(full)
		if derr != nil {
			_ = src.Close()
			return "", fmt.Errorf("创建条目文件失败:%w", derr)
		}
		_, cerr := io.Copy(dst, src)
		_ = src.Close()
		_ = dst.Close()
		if cerr != nil {
			return "", fmt.Errorf("恢复条目字节失败:%w", cerr)
		}
	}
	return rev.Label, nil
}

// meshPushItemNames 解析 mesh_push 的条目选择(逗号/换行分隔的产物名;空=全部产物)。
func meshPushItemNames(cfg map[string]any) []string {
	raw := cfgString(cfg, "items")
	names := []string{}
	for _, n := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' }) {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// runMeshPushJob 执行 mesh_push:收集本 run 产物 →(镜像)docker save 导出 → preflight 只传
// 对端缺失 blob → multipart 直传 → 播报对端项目/修订。对端离线/未授权 → 快速失败。
func (b *Builder) runMeshPushJob(ctx context.Context, rep dagrun.StageReporter, jb pipeline.Job, r *run.Run) error {
	if b.meshClient == nil || b.meshPeers == nil || b.artifactLister == nil {
		_ = rep.Log(ctx, streamStderr, "集群推送节点「"+jb.Name+"」:集群依赖未装配(客户端/节点表/产物列举缺失)")
		return ErrBuildFailed
	}
	nodeID := strings.TrimSpace(cfgString(jb.Config, "node"))
	if nodeID == "" {
		_ = rep.Log(ctx, streamStderr, "集群推送节点「"+jb.Name+"」:未指定目标节点")
		return ErrBuildFailed
	}
	peer, err := b.meshPeers.GetPeer(ctx, nodeID)
	if errors.Is(err, mesh.ErrNotFound) || peer == nil {
		_ = rep.Log(ctx, streamStderr, "集群推送节点「"+jb.Name+"」:目标节点不存在(节点表未登记)")
		return ErrBuildFailed
	}
	if peer.VirtualIP == "" {
		_ = rep.Log(ctx, streamStderr, "集群推送节点「"+jb.Name+"」:目标节点无虚拟 IP(EasyTier 未组网)")
		return ErrBuildFailed
	}

	arts, err := b.artifactLister(ctx, r.ID)
	if err != nil {
		_ = rep.Log(ctx, streamStderr, "列举本 run 产物失败:"+err.Error())
		return ErrBuildFailed
	}
	names := meshPushItemNames(jb.Config)
	selected := make([]run.Artifact, 0, len(arts))
	for _, a := range arts {
		if len(names) == 0 {
			selected = append(selected, a)
			continue
		}
		for _, n := range names {
			if a.Name == n {
				selected = append(selected, a)
				break
			}
		}
	}
	if len(selected) == 0 {
		_ = rep.Log(ctx, streamStderr, fmt.Sprintf("集群推送节点「%s」:未匹配到任何产物(本 run 共 %d 个)", jb.Name, len(arts)))
		return ErrBuildFailed
	}

	selfNode, err := b.meshPeers.NodeID(ctx)
	if err != nil {
		_ = rep.Log(ctx, streamStderr, "解析本节点身份失败:"+err.Error())
		return ErrBuildFailed
	}
	label, lerr := renderRunPlaceholders(strings.TrimSpace(cfgString(jb.Config, "revisionLabel")), r)
	if lerr != nil {
		_ = rep.Log(ctx, streamStderr, "集群推送节点「"+jb.Name+"」:修订标签占位渲染失败:"+lerr.Error())
		return ErrBuildFailed
	}

	manifest := mesh.PushManifest{
		OriginNodeID:      selfNode,
		OriginProjectRef:  r.ProjectID,
		OriginProjectName: r.ProjectName,
		OriginRunID:       r.ID,
		RevisionLabel:     label,
		Items:             []mesh.PushItem{},
	}
	// 镜像归档临时文件(发送后清理);文件产物直接流制品库句柄。
	tempFiles := []string{}
	defer func() {
		for _, f := range tempFiles {
			_ = os.Remove(f)
		}
	}()
	readers := map[string]io.Reader{}

	for _, a := range selected {
		if a.Type == run.ArtifactImage {
			if b.meshImages == nil {
				_ = rep.Log(ctx, streamStderr, "集群推送节点「"+jb.Name+"」:镜像导出器未装配,无法推送镜像产物")
				return ErrBuildFailed
			}
			tmp, terr := b.meshImages.exportImageArchiveToTemp(ctx, a.Reference)
			if terr != nil {
				_ = rep.Log(ctx, streamStderr, "导出镜像归档失败("+a.Name+"):"+terr.Error())
				return ErrBuildFailed
			}
			tempFiles = append(tempFiles, tmp)
			sha, size, serr := hashFile(tmp)
			if serr != nil {
				_ = rep.Log(ctx, streamStderr, "计算镜像归档哈希失败:"+serr.Error())
				return ErrBuildFailed
			}
			item := mesh.PushItem{Name: a.Name, Kind: pushsource.KindImage, SHA256: sha, Size: size, ImageRef: a.Reference}
			if d, ok := a.Metadata["digest"].(string); ok {
				item.Digest = d
			}
			manifest.Items = append(manifest.Items, item)
			readers[sha] = &lazyFileReader{path: tmp}
			continue
		}
		// 文件产物:reference 即制品库句柄(sha256)。
		dest := a.Name
		if wp, ok := a.Metadata["workspacePath"].(string); ok && strings.TrimSpace(wp) != "" {
			dest = strings.TrimSpace(wp)
		}
		manifest.Items = append(manifest.Items, mesh.PushItem{
			Name: a.Name, Kind: pushsource.KindFile, SHA256: a.Reference, Size: a.SizeBytes, DestName: dest,
		})
		readers[a.Reference] = &lazyStoreReader{b: b, key: a.Reference}
	}

	// preflight:只传对端缺失的 blob(内容寻址去重;文件产物高频重推时省大量带宽)。
	missing := make([]string, 0)
	pf := struct {
		Missing []string `json:"missing"`
	}{}
	if err := b.meshClient.Do(ctx, peer, "POST", "/api/mesh/push/preflight", manifest, &pf); err != nil {
		_ = rep.Log(ctx, streamStderr, "推送预检失败:"+meshPeerErrText(err))
		return ErrBuildFailed
	}
	for _, m := range pf.Missing {
		missing = append(missing, m)
	}

	_ = rep.Log(ctx, streamStdout, fmt.Sprintf("· 集群推送「%s」→ 节点 %s(%s):%d 个条目,%d 个 blob 待传",
		jb.Name, peerDisplayName(peer), peer.VirtualIP, len(manifest.Items), len(missing)))

	result, err := b.meshClient.Push(ctx, peer, manifest, pickReaders(readers, missing))
	if err != nil {
		_ = rep.Log(ctx, streamStderr, "集群推送失败:"+meshPeerErrText(err))
		return ErrBuildFailed
	}
	created := ""
	if result.Created {
		created = "(首次推送,已在对方自动创建对应项目)"
	}
	_ = rep.Log(ctx, streamStdout, fmt.Sprintf("  ✓ 推送完成%s:对方项目 %s · 修订 %s", created, result.ProjectID, result.RevisionID))
	return nil
}

// runMeshTriggerJob 执行 mesh_trigger:对对端项目建运行(explicit=显式项目 id;
// origin=按本项目的对应关系寻址,即推送到对方的那份)→ 可选等待完成并回流日志/状态。
func (b *Builder) runMeshTriggerJob(ctx context.Context, rep dagrun.StageReporter, jb pipeline.Job, r *run.Run) error {
	if b.meshClient == nil || b.meshPeers == nil {
		_ = rep.Log(ctx, streamStderr, "集群触发节点「"+jb.Name+"」:集群依赖未装配")
		return ErrBuildFailed
	}
	nodeID := strings.TrimSpace(cfgString(jb.Config, "node"))
	if nodeID == "" {
		_ = rep.Log(ctx, streamStderr, "集群触发节点「"+jb.Name+"」:未指定目标节点")
		return ErrBuildFailed
	}
	peer, err := b.meshPeers.GetPeer(ctx, nodeID)
	if errors.Is(err, mesh.ErrNotFound) || peer == nil {
		_ = rep.Log(ctx, streamStderr, "集群触发节点「"+jb.Name+"」:目标节点不存在(节点表未登记)")
		return ErrBuildFailed
	}

	// 参数(扁平 K=V 行,与前端冻结契约 Record<string,string> 对齐):每行 KEY=VALUE。
	params := map[string]string{}
	for _, line := range strings.Split(cfgString(jb.Config, "params"), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			if k = strings.TrimSpace(k); k != "" {
				params[k] = strings.TrimSpace(v)
			}
		}
	}
	body := mesh.RunTriggerRequest{
		Params:   params,
		Revision: strings.TrimSpace(cfgString(jb.Config, "revision")),
	}

	// 寻址:origin 模式按 (本节点 id, 本项目 id) 找对方对应项目;explicit 用显式项目 id。
	var path string
	if strings.TrimSpace(cfgString(jb.Config, "targetMode")) == "origin" {
		selfNode, nerr := b.meshPeers.NodeID(ctx)
		if nerr != nil {
			_ = rep.Log(ctx, streamStderr, "解析本节点身份失败:"+nerr.Error())
			return ErrBuildFailed
		}
		path = fmt.Sprintf("/api/mesh/projects/by-origin/%s/%s/runs", selfNode, r.ProjectID)
	} else {
		pid := strings.TrimSpace(cfgString(jb.Config, "project"))
		if pid == "" {
			_ = rep.Log(ctx, streamStderr, "集群触发节点「"+jb.Name+"」:未指定对方项目")
			return ErrBuildFailed
		}
		path = "/api/mesh/projects/" + pid + "/runs"
	}

	var result mesh.RunTriggerResult
	if err := b.meshClient.Do(ctx, peer, "POST", path, body, &result); err != nil {
		_ = rep.Log(ctx, streamStderr, "触发对方运行失败:"+meshPeerErrText(err))
		return ErrBuildFailed
	}
	deep := peer.DeepURL() + result.RunPath
	paramNote := ""
	if len(params) > 0 {
		paramNote = fmt.Sprintf(",%d 个参数", len(params))
	}
	_ = rep.Log(ctx, streamStdout, fmt.Sprintf("· 已触发节点 %s 的运行 %s%s:%s",
		peerDisplayName(peer), result.RunID, paramNote, deep))

	wait := true
	if raw, ok := jb.Config["waitForCompletion"].(bool); ok {
		wait = raw
	}
	if !wait {
		return nil
	}
	return b.awaitMeshRun(ctx, rep, jb, peer, result.RunID, deep)
}

// awaitMeshRun 订阅对端运行 SSE,把状态/日志回流到本节点;终态映射成败。
// SSE 断开自动重订阅(对端重启/网络抖动);超时(cfg timeoutMinutes,默认 60)失败。
func (b *Builder) awaitMeshRun(ctx context.Context, rep dagrun.StageReporter, jb pipeline.Job, peer *mesh.Peer, runID, deep string) error {
	timeout := time.Duration(cfgNonNegInt(jb.Config, "timeoutMinutes")) * time.Minute
	if timeout <= 0 {
		timeout = 60 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	terminal := ""
	for {
		if ctx.Err() != nil {
			return run.ErrCanceled
		}
		if time.Now().After(deadline) {
			_ = rep.Log(ctx, streamStderr, fmt.Sprintf("等待对方运行超时(%s):%s", timeout, deep))
			return ErrBuildFailed
		}
		remains := time.Until(deadline)
		sctx, cancel := context.WithTimeout(ctx, remains)
		err := b.meshClient.StreamRunEvents(sctx, peer, runID, func(event, data string) bool {
			switch event {
			case "status":
				var st struct {
					Status string `json:"status"`
				}
				if json.Unmarshal([]byte(data), &st) == nil && st.Status != "" {
					terminal = st.Status
					_ = rep.Log(ctx, streamStdout, "  · 对方运行状态:"+st.Status)
				}
			case "log":
				var lg struct {
					Stream string `json:"stream"`
					Line   string `json:"line"`
				}
				if json.Unmarshal([]byte(data), &lg) == nil && lg.Line != "" {
					_ = rep.Log(ctx, streamStdout, "  │ "+lg.Line)
				}
			}
			return !run.IsTerminal(terminal)
		})
		cancel()
		if run.IsTerminal(terminal) {
			break
		}
		if err != nil && ctx.Err() != nil {
			return run.ErrCanceled
		}
		if err != nil {
			// 非取消错误(连接断开等):轮询兜底 + 短退避重试。
			var poll struct {
				Status string `json:"status"`
			}
			if perr := b.meshClient.Do(ctx, peer, "GET", "/api/mesh/runs/"+runID, nil, &poll); perr == nil && run.IsTerminal(poll.Status) {
				terminal = poll.Status
				break
			}
			select {
			case <-ctx.Done():
				return run.ErrCanceled
			case <-time.After(3 * time.Second):
			}
		}
		if time.Now().After(deadline) {
			_ = rep.Log(ctx, streamStderr, fmt.Sprintf("等待对方运行超时(%s):%s", timeout, deep))
			return ErrBuildFailed
		}
	}
	switch terminal {
	case run.StatusSuccess:
		_ = rep.Log(ctx, streamStdout, "  ✓ 对方运行成功:"+deep)
		return nil
	case run.StatusPartialFailed:
		_ = rep.Log(ctx, streamStderr, "  ✗ 对方运行部分失败:"+deep)
		return ErrBuildFailed
	default:
		_ = rep.Log(ctx, streamStderr, "  ✗ 对方运行未成功("+terminal+"):"+deep)
		return ErrBuildFailed
	}
}

// peerDisplayName 取对端展示名(空回退 node id 前 8 位)。
func peerDisplayName(p *mesh.Peer) string {
	if p.Name != "" {
		return p.Name
	}
	if len(p.NodeID) > 8 {
		return p.NodeID[:8]
	}
	return p.NodeID
}

// meshPeerErrText 把对端交互错误转为人读文本(含未授权/不可达语义)。
func meshPeerErrText(err error) string {
	switch {
	case errors.Is(err, mesh.ErrForbidden):
		return "对方未授权本节点(需在对端「设置 → 集群」授权)"
	case errors.Is(err, mesh.ErrPeerUnreachable):
		return "对方不可达(EasyTier 链路不通或对方服务未运行)"
	default:
		return err.Error()
	}
}

// lazyFileReader 延迟打开的文件 reader(首次 Read 才打开,读完保持句柄直至调用方丢弃)。
type lazyFileReader struct {
	path string
	f    *os.File
}

func (l *lazyFileReader) Read(p []byte) (int, error) {
	if l.f == nil {
		f, err := os.Open(l.path)
		if err != nil {
			return 0, err
		}
		l.f = f
	}
	return l.f.Read(p)
}

// lazyStoreReader 延迟打开的制品库 reader(同上)。
type lazyStoreReader struct {
	b   *Builder
	key string
	rc  io.ReadCloser
}

func (l *lazyStoreReader) Read(p []byte) (int, error) {
	if l.rc == nil {
		if l.b == nil || l.b.artStore == nil {
			return 0, io.ErrUnexpectedEOF
		}
		rc, err := l.b.artStore.Open(l.key)
		if err != nil {
			return 0, err
		}
		l.rc = rc
	}
	return l.rc.Read(p)
}

// hashFile 流式计算文件 sha256 与大小。
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// pickReaders 按缺失清单挑出待传 reader(全部缺失时等价全量)。
func pickReaders(all map[string]io.Reader, missing []string) map[string]io.Reader {
	if len(missing) == 0 {
		return all
	}
	out := make(map[string]io.Reader, len(missing))
	for _, m := range missing {
		if r, ok := all[m]; ok {
			out[m] = r
		}
	}
	return out
}
