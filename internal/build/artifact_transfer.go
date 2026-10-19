// artifact_transfer.go 是「产物传输到其他环境」的发送侧(产物管理页手动触发):
//   - pushArtifactsToPeer 共享推送核心:组清单 →(镜像)EnsureLocal + docker save 导出 →
//     preflight 只传对端缺失 blob → multipart 直传。mesh_push 流水线节点与本服务同一
//     代码路径;
//   - ArtifactTransferService:HTTP 面按需触发的传输任务表(进程内存态,同共享构建任务表
//     取舍,重启即失),支持状态轮询与取消;OnSettled 终态回调供装配层接审计(build 不依赖 audit)。
//
// 接收侧零改动:对端经既有 /api/mesh/push 端点完成 blob 内容寻址去重、镜像导入内置
// registry、按 origin 幂等定位/自动建「被推送源」项目并落修订;对端 ACL(push_allowed)把门。
package build

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/huangchengsir/pipewright/internal/artifactstore"
	"github.com/huangchengsir/pipewright/internal/mesh"
	"github.com/huangchengsir/pipewright/internal/pushsource"
	"github.com/huangchengsir/pipewright/internal/run"
)

// pushClient 是集群推送所需的出站能力面(*mesh.Client 原样满足;单测 fake)。
type pushClient interface {
	Do(ctx context.Context, p *mesh.Peer, method, path string, body, out any) error
	Push(ctx context.Context, p *mesh.Peer, manifest mesh.PushManifest, blobs map[string]io.Reader) (*mesh.PushResult, error)
}

// pushImageExporter 抽象「导出镜像归档」能力(*ImageTransfer 原样满足;单测 fake)。
type pushImageExporter interface {
	// EnsureLocal 确保镜像在本机 daemon(不在则 pull;外置私有仓库可能失败)。
	EnsureLocal(ctx context.Context, ref string) error
	// Export 把镜像归档(docker save)写到 destPath,调用方负责清理临时文件。
	Export(ctx context.Context, ref, destPath string) error
}

// pushPreflightResult 是 /api/mesh/push/preflight 的响应(对端缺失 blob 清单)。
type pushPreflightResult struct {
	Missing []string `json:"missing"`
}

// transferRunSource 抽象传输校验所需的运行数据面(run.Service 原样满足;单测 fake)。
type transferRunSource interface {
	Get(ctx context.Context, id string) (*run.Run, error)
	ListArtifacts(ctx context.Context, runID string) ([]run.Artifact, error)
}

// pushArtifactsToPeer 是 mesh_push 节点与产物传输服务共用的推送核心:组清单 →
// (镜像)EnsureLocal + docker save 导出临时文件并算哈希 → preflight 只传对端缺失 blob →
// multipart 直传。openBlob 按制品库句柄开流;logf 输出阶段行(前缀与去向由调用方决定)。
// 镜像先 EnsureLocal:产物页手动传输时镜像可能已被本机 daemon 清理(pull 兜底);对
// 流水线路径仅多一次本地 inspect,刚构建完的镜像必然命中。
func pushArtifactsToPeer(
	ctx context.Context,
	client pushClient,
	images pushImageExporter,
	openBlob func(key string) (io.ReadCloser, error),
	logf func(format string, args ...any),
	peer *mesh.Peer,
	originNodeID string,
	r *run.Run,
	selected []run.Artifact,
	revisionLabel string,
) (*mesh.PushResult, error) {
	manifest := mesh.PushManifest{
		OriginNodeID:      originNodeID,
		OriginProjectRef:  r.ProjectID,
		OriginProjectName: r.ProjectName,
		OriginRunID:       r.ID,
		RevisionLabel:     revisionLabel,
		Items:             []mesh.PushItem{},
	}
	readers := map[string]io.Reader{}
	// 镜像归档临时文件(发送后清理);文件产物直接流制品库句柄。
	// 收尾统一关流:lazy reader 持有的句柄(os.File)在此释放,不依赖 GC。
	tempFiles := []string{}
	defer func() {
		for _, f := range tempFiles {
			_ = os.Remove(f)
		}
		for _, r := range readers {
			if c, ok := r.(io.Closer); ok {
				_ = c.Close()
			}
		}
	}()

	for _, a := range selected {
		if a.Type == run.ArtifactImage {
			if images == nil {
				return nil, fmt.Errorf("镜像导出器未装配,无法推送镜像产物(%s)", a.Name)
			}
			if err := images.EnsureLocal(ctx, a.Reference); err != nil {
				return nil, fmt.Errorf("定位/拉取镜像失败(%s):%w", a.Name, err)
			}
			logf("导出镜像归档:%s", a.Name)
			tmp, terr := os.CreateTemp("", "pw-mesh-img-*.tar")
			if terr != nil {
				return nil, fmt.Errorf("创建临时归档失败:%w", terr)
			}
			name := tmp.Name()
			_ = tmp.Close()
			if err := images.Export(ctx, a.Reference, name); err != nil {
				_ = os.Remove(name)
				return nil, fmt.Errorf("导出镜像归档失败(%s):%w", a.Name, err)
			}
			tempFiles = append(tempFiles, name)
			sha, size, serr := hashFile(name)
			if serr != nil {
				return nil, fmt.Errorf("计算镜像归档哈希失败(%s):%w", a.Name, serr)
			}
			item := mesh.PushItem{Name: a.Name, Kind: pushsource.KindImage, SHA256: sha, Size: size, ImageRef: a.Reference}
			if d, ok := a.Metadata["digest"].(string); ok {
				item.Digest = d
			}
			manifest.Items = append(manifest.Items, item)
			readers[sha] = &lazyFileReader{path: name}
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
		readers[a.Reference] = &lazyOpenReader{open: openBlob, key: a.Reference}
	}

	// preflight:只传对端缺失的 blob(内容寻址去重;文件产物高频重推时省大量带宽)。
	pf := &pushPreflightResult{}
	if err := client.Do(ctx, peer, "POST", "/api/mesh/push/preflight", manifest, pf); err != nil {
		return nil, err
	}
	blobs := pickReaders(readers, pf.Missing)
	logf("待传 %d 个 blob(对端已持有 %d 个,内容寻址去重跳过)", len(blobs), len(readers)-len(blobs))
	result, err := client.Push(ctx, peer, manifest, blobs)
	if err != nil {
		return nil, err
	}
	return result, nil
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

// Close 释放已打开的句柄(未打开时 no-op;推送收尾统一调用)。
func (l *lazyFileReader) Close() error {
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// lazyOpenReader 延迟打开的制品库 reader(open 按句柄开流;同上)。
type lazyOpenReader struct {
	open func(key string) (io.ReadCloser, error)
	key  string
	rc   io.ReadCloser
}

func (l *lazyOpenReader) Read(p []byte) (int, error) {
	if l.rc == nil {
		if l.open == nil {
			return 0, io.ErrUnexpectedEOF
		}
		rc, err := l.open(l.key)
		if err != nil {
			return 0, err
		}
		l.rc = rc
	}
	return l.rc.Read(p)
}

// Close 释放已打开的制品库句柄(未打开时 no-op;推送收尾统一调用)。
func (l *lazyOpenReader) Close() error {
	if l.rc == nil {
		return nil
	}
	err := l.rc.Close()
	l.rc = nil
	return err
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

// pickReaders 按缺失清单挑出待传 reader。missing 为 nil(预检响应未给清单)时防御性
// 全量;**空清单 = 对端已持有全部,不传**(接收端内容寻址去重,空传只带清单段)。
func pickReaders(all map[string]io.Reader, missing []string) map[string]io.Reader {
	if missing == nil {
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

// ─── 产物跨环境传输服务(产物管理页手动触发) ─────────────────────────────────

const (
	maxRunningTransfers = 4                // 并发上限(镜像导出占本机磁盘/带宽,防滥用)
	transferTimeout     = 60 * time.Minute // 单次传输硬超时(对齐 mesh_trigger 等待上限)
	transferRetention   = 30 * time.Minute // 终态条目保留时长(惰性剪枝)
	maxKeptTransfers    = 100              // 终态条目数量上限(超出按插入序淘汰)
)

// 传输状态机取值(TransferStatus.Status)。
const (
	TransferRunning  = "running"
	TransferSuccess  = "success"
	TransferFailed   = "failed"
	TransferCanceled = "canceled"
)

// 传输校验/调度错误(HTTP 层据此映射状态码)。
var (
	ErrTransferUnavailable      = errors.New("build: artifact transfer unavailable")
	ErrInvalidTransferInput     = errors.New("build: invalid transfer input")
	ErrArtifactNotTransferable  = errors.New("build: artifact not transferable")
	ErrTransferPeerNotNetworked = errors.New("build: peer not networked")
	ErrTransferBusy             = errors.New("build: transfer busy")
)

// notTransferableError 携带产物名与原因,Error() 即人读文本(HTTP 层原样展示);
// 经 Unwrap 保持 errors.Is(err, ErrArtifactNotTransferable) 判别。
type notTransferableError struct{ name, reason string }

func (e *notTransferableError) Error() string { return "产物「" + e.name + "」" + e.reason }
func (e *notTransferableError) Unwrap() error { return ErrArtifactNotTransferable }

// TransferInput 是一次手动传输的入参(HTTP 层组装)。
type TransferInput struct {
	RunID         string
	PeerNodeID    string
	ArtifactIDs   []string
	RevisionLabel string
}

// TransferResultView 是传输成功后的对端落点(ProjectURL 为对端人用深链)。
type TransferResultView struct {
	ProjectID  string `json:"projectId"`
	RevisionID string `json:"revisionId"`
	Created    bool   `json:"created"`
	ProjectURL string `json:"projectUrl"`
}

// TransferStatus 是一次传输的对外快照(前端轮询;Phase 为人读阶段文本)。
type TransferStatus struct {
	ID          string              `json:"id"`
	RunID       string              `json:"runId"`
	ProjectName string              `json:"projectName"`
	PeerNodeID  string              `json:"peerNodeId"`
	PeerName    string              `json:"peerName"`
	Items       []string            `json:"items"`
	Status      string              `json:"status"`
	Phase       string              `json:"phase"`
	Error       string              `json:"error,omitempty"`
	Result      *TransferResultView `json:"result,omitempty"`
	CreatedAt   time.Time           `json:"createdAt"`
	UpdatedAt   time.Time           `json:"updatedAt"`
}

// transferEntry 是任务表条目(状态快照 + 取消句柄;经服务 mu 护并发)。
type transferEntry struct {
	status   TransferStatus
	cancel   context.CancelFunc
	canceled bool // 用户主动取消(终态区分 canceled 与超时 failed)
}

// ArtifactTransferService 是产物跨环境传输服务。零值不可用,经 NewArtifactTransferService
// 构造;线程安全(互斥锁护任务表)。任务表为进程内存态:服务重启即丢(前端轮询 404 后
// 可重新发起;对端内容寻址去重,重传只补缺)。
type ArtifactTransferService struct {
	peers  meshPeerLookup
	client pushClient
	images pushImageExporter
	store  *artifactstore.Store
	runs   transferRunSource

	onSettled func(*TransferStatus) // 终态回调(装配层接审计;可能为 nil)

	mu    sync.Mutex
	items map[string]*transferEntry
	order []string // 插入序(剪枝/淘汰用)
}

// NewArtifactTransferService 构造传输服务。client(*mesh.Client)、images(*ImageTransfer)
// 原样满足窄接口;任一为 nil 时 Start 返回 ErrTransferUnavailable(端点 503)。
func NewArtifactTransferService(svc mesh.Service, client *mesh.Client, images *ImageTransfer, store *artifactstore.Store, runs run.Service) *ArtifactTransferService {
	s := &ArtifactTransferService{store: store, items: map[string]*transferEntry{}}
	if svc != nil {
		s.peers = meshSvcAdapter{svc: svc}
	}
	if client != nil {
		s.client = client
	}
	if images != nil {
		s.images = images
	}
	if runs != nil {
		s.runs = runs
	}
	return s
}

// OnSettled 注册终态回调(成功/失败/取消各一次;后台 goroutine 调用,回调需自行并发安全)。
func (s *ArtifactTransferService) OnSettled(fn func(*TransferStatus)) { s.onSettled = fn }

// transferCheck 判定产物可否跨环境传输:镜像(有引用)→ 可;文件类(stored + 句柄 +
// 非多文件清单)→ 可;其余(占位/未归档/format=files)→ 不可并给原因。
// format=files 的多文件清单在接收模型(一条目一 blob)下无法忠实还原,显式拒绝
// (mesh_push 节点对它同样只传首文件,属既有限制)。
func transferCheck(a run.Artifact) (ok bool, reason string) {
	if a.Type == run.ArtifactImage {
		if strings.TrimSpace(a.Reference) == "" {
			return false, "无镜像引用"
		}
		return true, ""
	}
	switch a.Type {
	case run.ArtifactJar, run.ArtifactDist, run.ArtifactArchive:
	default:
		return false, "未知产物类型 " + a.Type
	}
	if stored, _ := a.Metadata["stored"].(bool); !stored {
		return false, "未归档真字节(占位产物)"
	}
	if format, _ := a.Metadata["format"].(string); format == "files" {
		return false, "多文件清单(format=files)暂不支持跨环境传输"
	}
	if strings.TrimSpace(a.Reference) == "" {
		return false, "无制品库句柄"
	}
	return true, ""
}

// Start 校验并入队一次传输(异步执行,返回任务 id)。校验失败同步报错(HTTP 层映射);
// 传输成败异步落任务状态。同一次传输可含同 run 的多个产物(接收端落为对方项目的一个修订)。
func (s *ArtifactTransferService) Start(ctx context.Context, in TransferInput) (string, error) {
	if s == nil || s.client == nil || s.peers == nil || s.runs == nil || s.store == nil {
		return "", ErrTransferUnavailable
	}
	runID := strings.TrimSpace(in.RunID)
	peerID := strings.TrimSpace(in.PeerNodeID)
	if runID == "" || peerID == "" || len(in.ArtifactIDs) == 0 {
		return "", ErrInvalidTransferInput
	}
	r, err := s.runs.Get(ctx, runID)
	if err != nil {
		return "", err
	}
	peer, err := s.peers.GetPeer(ctx, peerID)
	if err != nil || peer == nil {
		return "", mesh.ErrNotFound
	}
	if peer.VirtualIP == "" {
		return "", ErrTransferPeerNotNetworked
	}
	arts, err := s.runs.ListArtifacts(ctx, runID)
	if err != nil {
		return "", err
	}
	byID := make(map[string]run.Artifact, len(arts))
	for _, a := range arts {
		byID[a.ID] = a
	}
	selected := make([]run.Artifact, 0, len(in.ArtifactIDs))
	names := make([]string, 0, len(in.ArtifactIDs))
	seen := map[string]bool{}
	hasImage := false
	for _, want := range in.ArtifactIDs {
		if seen[want] {
			continue
		}
		a, found := byID[want]
		if !found {
			return "", fmt.Errorf("%w: %s", run.ErrArtifactNotFound, want)
		}
		seen[want] = true
		if ok, reason := transferCheck(a); !ok {
			return "", &notTransferableError{name: a.Name, reason: reason}
		}
		if a.Type == run.ArtifactImage {
			hasImage = true
		}
		selected = append(selected, a)
		names = append(names, a.Name)
	}
	if len(selected) == 0 {
		return "", ErrInvalidTransferInput
	}
	if hasImage && s.images == nil {
		return "", ErrTransferUnavailable
	}
	selfNode, err := s.peers.NodeID(ctx)
	if err != nil {
		return "", err
	}

	now := time.Now()
	ctxExec, cancel := context.WithTimeout(context.Background(), transferTimeout)
	s.mu.Lock()
	running := 0
	for _, e := range s.items {
		if e.status.Status == TransferRunning {
			running++
		}
	}
	if running >= maxRunningTransfers {
		s.mu.Unlock()
		cancel()
		return "", ErrTransferBusy
	}
	id := newTransferID()
	e := &transferEntry{cancel: cancel, status: TransferStatus{
		ID: id, RunID: r.ID, ProjectName: r.ProjectName,
		PeerNodeID: peer.NodeID, PeerName: peerDisplayName(peer),
		Items: names, Status: TransferRunning, Phase: "准备传输",
		CreatedAt: now, UpdatedAt: now,
	}}
	s.items[id] = e
	s.order = append(s.order, id)
	s.pruneLocked(now)
	s.mu.Unlock()

	go s.execute(ctxExec, e, r, peer, selfNode, selected, strings.TrimSpace(in.RevisionLabel))
	return id, nil
}

// Get 返回任务状态快照(未知 id → ok=false)。
func (s *ArtifactTransferService) Get(id string) (*TransferStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.items[id]
	if !ok {
		return nil, false
	}
	s.pruneLocked(time.Now())
	snapshot := cloneTransferStatus(&e.status)
	return &snapshot, true
}

// Cancel 取消进行中的传输(docker save 经 ctx 取消终止);已终态或未知 → false。
func (s *ArtifactTransferService) Cancel(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.items[id]
	if !ok || e.status.Status != TransferRunning {
		return false
	}
	e.canceled = true
	if e.cancel != nil {
		e.cancel()
	}
	return true
}

// execute 在后台 goroutine 执行传输并落终态(成功/失败/取消;onSettled 回调)。
func (s *ArtifactTransferService) execute(ctx context.Context, e *transferEntry, r *run.Run, peer *mesh.Peer, selfNode string, selected []run.Artifact, label string) {
	defer func() {
		if v := recover(); v != nil {
			s.settle(e, fmt.Sprintf("内部错误:%v", v), nil, peer)
		}
	}()
	setPhase := func(format string, args ...any) {
		s.mu.Lock()
		e.status.Phase = fmt.Sprintf(format, args...)
		e.status.UpdatedAt = time.Now()
		s.mu.Unlock()
	}
	result, err := pushArtifactsToPeer(ctx, s.client, s.images, s.store.Open, setPhase, peer, selfNode, r, selected, label)
	s.mu.Lock()
	canceled := e.canceled
	s.mu.Unlock()
	if err != nil {
		s.settle(e, transferErrText(err), nil, peer)
		return
	}
	if canceled {
		s.settle(e, "", nil, peer) // Push 已成功但用户同时点了取消:按取消收尾(对端幂等,无害)
		return
	}
	s.settle(e, "", result, peer)
}

// settle 落终态并触发回调(errMsg 非空 → failed;取消标记优先 → canceled;
// 否则 success + result)。
func (s *ArtifactTransferService) settle(e *transferEntry, errMsg string, result *mesh.PushResult, peer *mesh.Peer) {
	s.mu.Lock()
	switch {
	case e.canceled:
		e.status.Status = TransferCanceled
		e.status.Phase = "已取消"
		e.status.Error = ""
	case errMsg != "":
		e.status.Status = TransferFailed
		e.status.Phase = "传输失败"
		e.status.Error = errMsg
	case result != nil:
		e.status.Status = TransferSuccess
		e.status.Phase = "传输完成"
		e.status.Result = &TransferResultView{
			ProjectID:  result.ProjectID,
			RevisionID: result.RevisionID,
			Created:    result.Created,
			ProjectURL: peer.DeepURL() + "/projects",
		}
	default:
		e.status.Status = TransferFailed
		e.status.Phase = "传输失败"
		e.status.Error = "未知错误"
	}
	e.status.UpdatedAt = time.Now()
	snapshot := cloneTransferStatus(&e.status)
	cb := s.onSettled
	s.mu.Unlock()
	if cb != nil {
		cb(&snapshot)
	}
}

// transferErrText 把传输错误转为人读文本(取消/超时/未授权/不可达语义)。
func transferErrText(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "已取消"
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("传输超时(上限 %s)", transferTimeout)
	}
	return meshPeerErrText(err)
}

// newTransferID 生成任务 id(at- + 16 位 hex;crypto/rand 失败回退时间戳)。
func newTransferID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("at-%d", time.Now().UnixNano())
	}
	return "at-" + hex.EncodeToString(b)
}

// cloneTransferStatus 深拷贝状态快照(Items 切片与 Result 指针;对外只读隔离)。
func cloneTransferStatus(st *TransferStatus) TransferStatus {
	out := *st
	out.Items = append([]string(nil), st.Items...)
	if st.Result != nil {
		r := *st.Result
		out.Result = &r
	}
	return out
}

// pruneLocked 惰性剪枝(调用方持 mu):移除终态且超保留时长的条目;终态总量超上限时
// 按插入序淘汰最旧。进行中条目永不剪除。
func (s *ArtifactTransferService) pruneLocked(now time.Time) {
	for _, id := range s.order {
		if e, ok := s.items[id]; ok && e.status.Status != TransferRunning &&
			now.Sub(e.status.UpdatedAt) > transferRetention {
			delete(s.items, id)
		}
	}
	kept := len(s.items)
	for _, id := range s.order {
		if kept <= maxKeptTransfers {
			break
		}
		if e, ok := s.items[id]; ok && e.status.Status != TransferRunning {
			delete(s.items, id)
			kept--
		}
	}
	out := s.order[:0]
	for _, id := range s.order {
		if _, ok := s.items[id]; ok {
			out = append(out, id)
		}
	}
	s.order = out
}
