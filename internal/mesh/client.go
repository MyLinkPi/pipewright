// client.go 是节点间出站客户端:对对等节点的 /api/mesh/* 端点发起**已签名**请求。
// 所有请求走 EasyTier 虚拟 IP;签名头由本节点 ed25519 私钥计算(见 sign.go)。
// 对端返回沿用平台错误信封 {error:{code,message}};403 → ErrForbidden(未获对端授权)。
package mesh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// PushItem 是推送清单里的一个条目(文件产物或镜像归档)。
type PushItem struct {
	// Name 条目名(接收端产物名;部署节点按 artifactName 绑定)。
	Name string `json:"name"`
	// Kind 取 "file" | "image"。
	Kind string `json:"kind"`
	// SHA256 是 blob 内容哈希(接收端校验 + 制品库内容寻址键)。
	SHA256 string `json:"sha256"`
	// Size 是 blob 字节数(接收端预检磁盘/上限)。
	Size int64 `json:"size"`
	// DestName 是恢复进工作区时的相对路径(file 条目);image 条目忽略。
	DestName string `json:"destName,omitempty"`
	// ImageRef 是来源镜像引用(元数据;接收端导入自己 registry 后用本地可用引用替换)。
	ImageRef string `json:"imageRef,omitempty"`
	// Digest 是镜像内容 digest(sha256:…,元数据):比归档哈希稳定,接收端据此跨推送
	// 去重(同 digest 不重复 load/push)。
	Digest string `json:"digest,omitempty"`
}

// PushManifest 是 POST /api/mesh/push 的清单段。
type PushManifest struct {
	// Origin 携带来源项目身份(接收端按 origin_node_id+origin_project_ref 幂等定位/自动建项目)。
	OriginNodeID      string `json:"originNodeId"`
	OriginProjectRef  string `json:"originProjectRef"`
	OriginProjectName string `json:"originProjectName"`
	OriginRunID       string `json:"originRunId"`
	// RevisionLabel 是本次修订标签(支持 {{branch}}/{{commit}} 等占位,由发送侧渲染)。
	RevisionLabel string `json:"revisionLabel"`
	Items         []PushItem `json:"items"`
}

// PushResult 是推送成功响应。
type PushResult struct {
	ProjectID  string `json:"projectId"`
	RevisionID string `json:"revisionId"`
	Created    bool   `json:"created"` // 本次是否自动创建了对应项目(首次推送)
}

// RunTriggerRequest 是 POST /api/mesh/projects/{id}/runs 的入参。
type RunTriggerRequest struct {
	Params   map[string]string `json:"params"`
	Revision string            `json:"revision,omitempty"` // pushed 项目可指定修订标签(空=最新)
	Actor    string            `json:"actor,omitempty"`
}

// RunTriggerResult 是触发成功响应。
type RunTriggerResult struct {
	RunID   string `json:"runId"`
	RunPath string `json:"runPath"` // 接收端运行详情路径(拼对端 URL 成深链)
	Status  string `json:"status"`
}

// Client 是出站签名客户端。零值不可用,经 NewClient 构造。
type Client struct {
	svc Service
	hc  *http.Client
}

// NewClient 构造客户端。svc 提供本节点身份(签名);传输层走 EasyTier 虚拟网络,
// 拨号超时收紧(对端理论可达,失联快速失败),总超时由各请求的 ctx 控制。
func NewClient(svc Service) *Client {
	return &Client{
		svc: svc,
		hc: &http.Client{
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				MaxIdleConnsPerHost: 4,
			},
		},
	}
}

// BaseURL 返回对端 API 基址(虚拟 IP + 端口;未配端口回退 8080)。
func (p *Peer) BaseURL() string {
	port := p.APIPort
	if port <= 0 {
		port = 8080
	}
	return "http://" + net.JoinHostPort(strings.TrimSpace(p.VirtualIP), strconv.Itoa(port))
}

// DeepURL 返回对端**人用** Web 基址:优先登记的对外地址(publicUrl),回退虚拟 IP 基址。
// 运行深链等面向浏览器的链接用此地址(浏览器通常不在 EasyTier 网内)。
func (p *Peer) DeepURL() string {
	if u := strings.TrimSpace(p.PublicURL); u != "" {
		return strings.TrimSuffix(u, "/")
	}
	return p.BaseURL()
}

// identity 取本节点 id + 私钥(签名用;私钥进程内即取即用)。
func (c *Client) identity(ctx context.Context) (string, ed25519.PrivateKey, error) {
	st, err := c.svc.Settings(ctx)
	if err != nil {
		return "", nil, err
	}
	priv, err := c.svc.PrivateKey(ctx)
	if err != nil {
		return "", nil, err
	}
	return st.NodeID, priv, nil
}

// Do 发起一次签名 JSON 请求并把响应解析进 out(out 为 nil 时丢弃)。
// 对端 403 → ErrForbidden;其他非 2xx → 携带对端 message 的错误。
func (c *Client) Do(ctx context.Context, p *Peer, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("mesh: marshal request: %w", err)
		}
		payload = b
	}
	raw, err := c.DoRaw(ctx, p, method, path, payload)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// DoRaw 发起签名请求并返回原始响应体字节(管理面代理透传给本节点 UI 用)。
// 错误语义同 Do(403 → ErrForbidden;非 2xx → 携带对端 message)。
func (c *Client) DoRaw(ctx context.Context, p *Peer, method, path string, payload []byte) ([]byte, error) {
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.BaseURL()+path, rdr)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := c.sign(ctx, req, payload); err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPeerUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, ErrForbidden
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var envelope struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &envelope)
		if envelope.Error.Message != "" {
			return nil, fmt.Errorf("mesh: peer responded %d: %s", resp.StatusCode, envelope.Error.Message)
		}
		return nil, fmt.Errorf("mesh: peer responded %d", resp.StatusCode)
	}
	return raw, nil
}

// Hello 对某地址(虚拟 IP[:端口])做发现探测,返回对端身份。
func (c *Client) Hello(ctx context.Context, baseURL string) (*HelloInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/mesh/hello", nil)
	if err != nil {
		return nil, err
	}
	if err := c.sign(ctx, req, nil); err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPeerUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out HelloInfo
	if err := consumeResponse(resp, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Push 流式推送:multipart,manifest 段 + 每 blob 一段(段名 = blob sha256)。
// blobs 的键必须覆盖 manifest 里全部条目的 SHA256;接收端按内容寻址去重。
func (c *Client) Push(ctx context.Context, p *Peer, manifest PushManifest, blobs map[string]io.Reader) (*PushResult, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		var werr error
		defer func() { _ = pw.CloseWithError(werr) }()
		mj, err := mw.CreateFormField("manifest")
		if err != nil {
			werr = err
			return
		}
		mb, _ := json.Marshal(manifest)
		if _, err := mj.Write(mb); err != nil {
			werr = err
			return
		}
		for key, r := range blobs {
			fw, err := mw.CreateFormFile("blob", key)
			if err != nil {
				werr = err
				return
			}
			if _, err := io.Copy(fw, r); err != nil {
				werr = err
				return
			}
		}
		werr = mw.Close()
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL()+"/api/mesh/push", pr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	// 流式请求不携带 body 哈希头(签名串取 "-");完整性由接收端逐 blob sha256 校验兜底。
	if err := c.sign(ctx, req, nil); err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPeerUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out PushResult
	if err := consumeResponse(resp, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StreamRunEvents 订阅对端某运行的 SSE 事件流,逐事件回调 onEvent(返回 false 停止)。
// 连接断开/ctx 结束即返回;调用方需重订阅时自行重试(对端 run 详情可轮询兜底)。
func (c *Client) StreamRunEvents(ctx context.Context, p *Peer, runID string, onEvent func(event, data string) bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.BaseURL()+"/api/mesh/runs/"+runID+"/events", nil)
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
	return readSSE(resp.Body, onEvent)
}

// sign 附加签名头(body 为小请求体;nil → 流式)。
func (c *Client) sign(ctx context.Context, req *http.Request, body []byte) error {
	nodeID, priv, err := c.identity(ctx)
	if err != nil {
		return err
	}
	return SignHeaders(req.Header, req.Method, req.URL.Path, body, nodeID, priv)
}

// consumeResponse 校验状态码并解析 JSON 响应体。
func consumeResponse(resp *http.Response, out any) error {
	if resp.StatusCode == http.StatusForbidden {
		return ErrForbidden
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return statusError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// statusError 把非 2xx 响应转为携带对端错误 message 的错误(信封 {error:{code,message}})。
func statusError(resp *http.Response) error {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&envelope)
	if envelope.Error.Message != "" {
		return fmt.Errorf("mesh: peer responded %d: %s", resp.StatusCode, envelope.Error.Message)
	}
	return fmt.Errorf("mesh: peer responded %d", resp.StatusCode)
}

// readSSE 极简 SSE 读取器:仅消费 "event:"/"data:" 行(与平台 runs SSE 的事件形一致)。
func readSSE(body io.Reader, onEvent func(event, data string) bool) error {
	var event, data strings.Builder
	deliver := func() bool {
		if data.Len() == 0 && event.Len() == 0 {
			return true
		}
		ok := onEvent(strings.TrimSpace(event.String()), strings.TrimSpace(data.String()))
		event.Reset()
		data.Reset()
		return ok
	}
	buf := make([]byte, 0, 64*1024)
	tmp := make([]byte, 32*1024)
	for {
		n, err := body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for {
				idx := bytes.IndexByte(buf, '\n')
				if idx < 0 {
					break
				}
				line := string(buf[:idx])
				buf = buf[idx+1:]
				switch {
				case strings.HasPrefix(line, "event:"):
					event.WriteString(strings.TrimPrefix(line, "event:"))
				case strings.HasPrefix(line, "data:"):
					data.WriteString(strings.TrimPrefix(line, "data:"))
				case line == "":
					if !deliver() {
						return nil
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				_ = deliver()
				return nil
			}
			return err
		}
	}
}
