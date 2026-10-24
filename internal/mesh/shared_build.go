// shared_build.go 定义集群级共享构建机(0086)的 wire 契约:
//   - 目录:提供方(机器所属环境)GET /api/mesh/build/catalog(签名 + build ACL,默认拒绝)
//     返回本端标记为集群共享的构建机清单(不含 host/凭据等敏感字段);
//   - 任务:发起方 POST /api/mesh/build/jobs(multipart:spec 段 + workspace 段流式上传),
//     经 GET /jobs/{id}/events(SSE 日志/状态)、GET /jobs/{id}(轮询兜底)、
//     GET /jobs/{id}/result(产物 tar.gz 流式回拉)、DELETE /jobs/{id}(取消)跟踪。
//
// 排队权威在提供方:任务进入提供方构建机调度器的同一 FIFO,与其本端任务统一排队;
// SSH 凭据只在提供方解密使用,工作区与产物经提供方 OS 临时文件中转,绝不进其制品库。
// 类型归 mesh 包(它就是 /api/mesh/* 的 wire 契约);build 只引用类型,不反向依赖。
package mesh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
)

// 共享构建任务的节点类型(与流水线 job 类型对应;仅 script / build_image 可共享)。
const (
	SharedJobTypeScript     = "script"
	SharedJobTypeBuildImage = "build_image"
)

// 任务状态机:queued(受理,排队中)→ running(已上机)→ succeeded | failed | canceled。
const (
	SharedJobStateQueued    = "queued"
	SharedJobStateRunning   = "running"
	SharedJobStateSucceeded = "succeeded"
	SharedJobStateFailed    = "failed"
	SharedJobStateCanceled  = "canceled"
)

// BuildCatalog 是 GET /api/mesh/build/catalog 的响应载荷:本端全部集群共享构建机。
// 节点身份由拉取方自己的 peer 注册表提供,载荷内不再冗余;MachineName 供发起方日志展示。
type BuildCatalog struct {
	NodeName string                `json:"nodeName"`
	Machines []CatalogBuildMachine `json:"machines"`
}

// CatalogBuildMachine 是目录里的一台共享构建机(脱敏:只有调度元数据,无 host/凭据)。
type CatalogBuildMachine struct {
	ServerID   string `json:"serverId"`
	ServerName string `json:"serverName"`
	// Labels 是构建机池标签原样(逗号分隔 tag/k=v;发起方用剥掉 peer: 项后的选择器匹配)。
	Labels    string `json:"labels"`
	MaxBuilds int    `json:"maxBuilds"`
	Priority  int    `json:"priority"`
	Gpu       bool   `json:"gpu"`
}

// SharedJobSpec 是 POST /api/mesh/build/jobs 的 spec 段(multipart 字段,≤1MB JSON)。
// Env 与 ImageSpec.Vars 携带发起方已解析的环境变量/构建变量(含 secret 值)——传输经
// 节点签名 + EasyTier(WireGuard)加密,提供方不落盘、只在容器执行时注入,绝不回显。
type SharedJobSpec struct {
	RunID     string `json:"runId"`
	StageName string `json:"stageName"`
	JobNodeID string `json:"jobNodeId"`
	JobName   string `json:"jobName"`
	JobType   string `json:"jobType"` // SharedJobTypeScript | SharedJobTypeBuildImage
	// Selector 是剥掉 peer: 项后的选择器(标签串或 server:<id> 钉死),在提供方共享机
	// 目录内匹配(与提供方本端任务同一调度器、同一 FIFO)。
	Selector string `json:"selector"`
	// TimeoutMinutes 是任务级超时(0 = 不限;由提供方强制)。
	TimeoutMinutes int `json:"timeoutMinutes,omitempty"`
	// WorkspaceSHA256 / WorkspaceSize 是 workspace 段的完整性元数据(接收方流式校验)。
	WorkspaceSHA256 string `json:"workspaceSha256"`
	WorkspaceSize   int64  `json:"workspaceSize"`

	// ---- script 类(SharedJobTypeScript)----
	Image    string   `json:"image,omitempty"`
	Workdir  string   `json:"workdir,omitempty"`
	Env      []string `json:"env,omitempty"` // KEY=VAL(已解析,含 secret)
	Commands []string `json:"commands,omitempty"`
	// ResourceCPU / ResourceMemory 透传容器资源规格(docker run --cpus/--memory)。
	ResourceCPU    string `json:"resourceCpu,omitempty"`
	ResourceMemory string `json:"resourceMemory,omitempty"`
	// ArtifactPaths 是发起方渲染好的产物收集 glob(相对工作区;空 = 不收集产物)。
	ArtifactPaths []string `json:"artifactPaths,omitempty"`

	// ---- build_image 类(SharedJobTypeBuildImage)----
	ImageSpec *SharedImageSpec `json:"imageSpec,omitempty"`
}

// SharedImageSpec 是 build_image 任务的构建输入(在提供方共享机上构建,产物以
// docker save 归档回传,由发起方 load 后用自己的凭据推送自己的 registry)。
type SharedImageSpec struct {
	BuildModel        string `json:"buildModel,omitempty"` // dockerfile | toolchain
	DockerfilePath    string `json:"dockerfilePath,omitempty"`
	Context           string `json:"context,omitempty"`
	BuildTarget       string `json:"buildTarget,omitempty"` // 多阶段 Dockerfile 目标 stage(空 = 最后一个)
	ToolchainLanguage string `json:"toolchainLanguage,omitempty"`
	ToolchainVersion  string `json:"toolchainVersion,omitempty"`
	// ProjectName / CommitTag 参与镜像本地命名(与发起方本地构建同构,load 后同名)。
	ProjectName string `json:"projectName"`
	CommitTag   string `json:"commitTag"`
	// Vars 是已解析的构建变量(含 secret 值;docker build --build-arg 注入)。
	Vars []string `json:"vars,omitempty"`
}

// SharedJobAccepted 是任务受理响应。
type SharedJobAccepted struct {
	JobID string `json:"jobId"`
}

// SharedJobStatus 是任务当前状态(轮询兜底 / 管理页共用)。
type SharedJobStatus struct {
	JobID   string `json:"jobId"`
	State   string `json:"state"` // SharedJobState*
	Peer    string `json:"peer"`  // 发起方节点名(提供方视角)
	RunID   string `json:"runId"` // 发起方运行 id(溯源)
	JobName string `json:"jobName"`
	// ServerID / ServerName 是已上机的共享机(排队中为空)。
	ServerID   string `json:"serverId"`
	ServerName string `json:"serverName"`
	ExitCode   int    `json:"exitCode"`
	Error      string `json:"error,omitempty"`
	// HasResult 表示产物归档就绪、可经 result 端点回拉(失败/取消恒 false)。
	HasResult bool   `json:"hasResult"`
	QueuedAt  string `json:"queuedAt"`
	StartedAt string `json:"startedAt,omitempty"`
	EndedAt   string `json:"endedAt,omitempty"`
}

// SharedJobEvent 是 SSE 事件载荷(event: log | status)。
type SharedJobEvent struct {
	Event string `json:"event"` // "log" | "status"
	// log:
	Stream string `json:"stream,omitempty"` // stdout | stderr
	Line   string `json:"line,omitempty"`
	// status(完整 SharedJobStatus 快照,状态迁移即推送)。
	Status *SharedJobStatus `json:"status,omitempty"`
}

// FetchBuildCatalog 拉取对端共享构建机目录(GET /api/mesh/build/catalog)。
// 对端 build ACL 未授权 → ErrForbidden;网络不可达 → ErrPeerUnreachable。
func (c *Client) FetchBuildCatalog(ctx context.Context, p *Peer) (*BuildCatalog, error) {
	var out BuildCatalog
	if err := c.Do(ctx, p, http.MethodGet, "/api/mesh/build/catalog", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SubmitSharedJob 流式提交一个共享构建任务(multipart:spec 段 + workspace 段)。
// workspace 为发起方打好包的工作区 tar.gz(流式,无全量内存驻留;签名串取 "-",
// 完整性由 spec.WorkspaceSHA256 在接收端流式校验兜底)。
func (c *Client) SubmitSharedJob(ctx context.Context, p *Peer, spec *SharedJobSpec, workspace io.Reader) (*SharedJobAccepted, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		var werr error
		defer func() { _ = pw.CloseWithError(werr) }()
		fw, err := mw.CreateFormField("spec")
		if err != nil {
			werr = err
			return
		}
		mb, err := json.Marshal(spec)
		if err != nil {
			werr = err
			return
		}
		if _, err := fw.Write(mb); err != nil {
			werr = err
			return
		}
		ws, err := mw.CreateFormFile("workspace", "workspace.tar.gz")
		if err != nil {
			werr = err
			return
		}
		if _, err := io.Copy(ws, workspace); err != nil {
			werr = err
			return
		}
		werr = mw.Close()
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL()+"/api/mesh/build/jobs", pr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if err := c.sign(ctx, req, nil); err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPeerUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out SharedJobAccepted
	if err := consumeResponse(resp, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StreamSharedJobEvents 订阅任务 SSE 事件流,逐事件回调 onEvent(返回 false 停止)。
// 连接断开/ctx 结束即返回;调用方需重订阅时自行重试(状态可轮询兜底)。
func (c *Client) StreamSharedJobEvents(ctx context.Context, p *Peer, jobID string, onEvent func(SharedJobEvent) bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.BaseURL()+"/api/mesh/build/jobs/"+jobID+"/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if err := c.sign(ctx, req, nil); err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPeerUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return statusError(resp)
	}
	return readSSE(resp.Body, func(event, data string) bool {
		var ev SharedJobEvent
		if json.Unmarshal([]byte(data), &ev) != nil {
			return true // 非事件载荷(如心跳注释已被 readSSE 过滤);跳过不中断
		}
		ev.Event = event
		return onEvent(ev)
	})
}

// GetSharedJob 轮询任务状态(GET /api/mesh/build/jobs/{id})。404 = 任务已过期/对端重启。
func (c *Client) GetSharedJob(ctx context.Context, p *Peer, jobID string) (*SharedJobStatus, error) {
	var out SharedJobStatus
	if err := c.Do(ctx, p, http.MethodGet, "/api/mesh/build/jobs/"+jobID, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelSharedJob 取消任务(排队中撤销/运行中中断;DELETE /api/mesh/build/jobs/{id})。
func (c *Client) CancelSharedJob(ctx context.Context, p *Peer, jobID string) error {
	return c.Do(ctx, p, http.MethodDelete, "/api/mesh/build/jobs/"+jobID, nil, nil)
}

// FetchSharedJobResult 流式拉取产物归档(GET /api/mesh/build/jobs/{id}/result),写入 w。
// 归档格式按任务类型:script = 产物路径集合的 tar.gz;build_image = docker save 的 gzip
// 归档(单文件)。返回归档 sha256 与字节数;提供方声明的 sha(X-Pw-Sha256 头)不符时报错
// (传输损坏不落盘使用)。
func (c *Client) FetchSharedJobResult(ctx context.Context, p *Peer, jobID string, w io.Writer) (sha string, size int64, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.BaseURL()+"/api/mesh/build/jobs/"+jobID+"/result", nil)
	if err != nil {
		return "", 0, err
	}
	if err := c.sign(ctx, req, nil); err != nil {
		return "", 0, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %v", ErrPeerUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusForbidden {
		return "", 0, ErrForbidden
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, statusError(resp)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), resp.Body)
	if err != nil {
		return "", 0, err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if want := strings.TrimSpace(resp.Header.Get("X-Pw-Sha256")); want != "" && want != got {
		return "", 0, fmt.Errorf("mesh: shared job result checksum mismatch (want %s, got %s)", want, got)
	}
	return got, n, nil
}
