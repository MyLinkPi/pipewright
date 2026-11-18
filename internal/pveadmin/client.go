// client.go — PVE REST API 薄客户端(pveproxy,默认 :8006,自签 TLS)。
//
// 鉴权:API Token 一行头 `Authorization: PVEAPIToken=<user@realm!tokenid>=<uuid>`;
// TLS 三模式:verify(系统 CA)/ insecure(内网自签,跳过校验)/ fingerprint(证书
// SHA256 指纹固定)。PVE JSON 的类型怪癖(数值序列化为字符串、布尔 0/1)经 flexInt/
// flexFloat/flexBool 自定义 UnmarshalJSON 收口在本文件,领域层只见普通值。
//
// 纪律:Token 只进请求头,绝不进错误体/日志;非 2xx 解析 PVE 自身错误体
// {data:{errors,message}} 后包装 ErrConnectFailed(携带 URL 路径与状态码,不含 Token)。
package pveadmin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// pveRequestTimeout 是单次 API 请求时限。
const pveRequestTimeout = 10 * time.Second

// pveClient 是最小 API 面(注入 fake 可离线单测)。path 形如 /cluster/status,
// 查询参数由调用方拼进 path。out 收到的已是解包后的 data。
type pveClient interface {
	Get(ctx context.Context, path string, out any) error
	Post(ctx context.Context, path string, form url.Values, out any) error
	Put(ctx context.Context, path string, form url.Values, out any) error
	// Upload 以 multipart/form-data 流式上传文件(fileField 为文件表单字段名,
	// PVE 存储上传固定为 "filename");fields 为随附表单字段。content 流式
	// 读取绝不整文件缓冲(大 .vma 友好);用独立无超时 http.Client(默认 10s 会切断上传)。
	Upload(ctx context.Context, path string, fields url.Values, fileField, fileName string, content io.Reader, out any) error
}

// clientFactory 按集群配置构造客户端(生产实现经 vault 解封 Token;测试注入 fake)。
type clientFactory func(apiURL, tokenName, tokenSecret, tlsMode, tlsFingerprint string) (pveClient, error)

// liveClient 是生产 pveClient。
type liveClient struct {
	base  *url.URL
	token string // 完整头值 PVEAPIToken=name=secret;只进请求头
	http  *http.Client
	// uploadHTTP 专供大文件 multipart 上传:无总超时(10s 会切断 .vma 上传),
	// 生命周期由调用方 ctx 控制;TLS Transport 与 http 同源。
	uploadHTTP *http.Client
}

// prodClientFactory 是生产工厂:校验 apiURL 并组 TLS。
func prodClientFactory(apiURL, tokenName, tokenSecret, tlsMode, tlsFingerprint string) (pveClient, error) {
	u, err := url.Parse(strings.TrimRight(apiURL, "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("%w: API 地址必须形如 https://host:8006", ErrInvalidInput)
	}
	tc, err := tlsClientConfig(tlsMode, tlsFingerprint)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{TLSClientConfig: tc}
	return &liveClient{
		base:       u,
		token:      "PVEAPIToken=" + tokenName + "=" + tokenSecret,
		http:       &http.Client{Timeout: pveRequestTimeout, Transport: tr},
		uploadHTTP: &http.Client{Transport: tr},
	}, nil
}

// tlsClientConfig 组三种 TLS 模式。fingerprint 模式收 SHA256(十六进制,容忍冒号/大写)。
func tlsClientConfig(mode, fingerprint string) (*tls.Config, error) {
	switch mode {
	case TLSModeVerify:
		return &tls.Config{}, nil
	case TLSModeInsecure:
		return &tls.Config{InsecureSkipVerify: true}, nil
	case TLSModeFingerprint:
		want := strings.ToLower(strings.ReplaceAll(fingerprint, ":", ""))
		if want == "" {
			return nil, fmt.Errorf("%w: fingerprint 模式必须提供证书 SHA256 指纹", ErrInvalidInput)
		}
		return &tls.Config{
			InsecureSkipVerify: true, // 校验由 VerifyPeerCertificate 按指纹完成
			VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				for _, raw := range rawCerts {
					sum := sha256.Sum256(raw)
					if hex.EncodeToString(sum[:]) == want {
						return nil
					}
				}
				return fmt.Errorf("证书指纹不匹配")
			},
		}, nil
	default:
		return nil, fmt.Errorf("%w: 未知 TLS 模式 %q", ErrInvalidInput, mode)
	}
}

// apiResponse 是 PVE 统一信封 {data: ...}。
type apiResponse struct {
	Data json.RawMessage `json:"data"`
}

// errorBody 是 PVE 错误响应体。
type errorBody struct {
	Errors  map[string]string `json:"errors"`
	Message string            `json:"message"`
}

// pveAPIError 保留 HTTP 状态供轮询区分永久错误与可重试错误,文本与原有错误契约一致。
type pveAPIError struct {
	status int
	err    error
}

func (e *pveAPIError) Error() string { return e.err.Error() }
func (e *pveAPIError) Unwrap() error { return e.err }

func (c *liveClient) do(ctx context.Context, method, path string, form url.Values, out any) error {
	rel, err := url.Parse(path)
	if err != nil {
		return fmt.Errorf("%w: API 路径非法: %v", ErrInvalidInput, err)
	}
	full := *c.base
	full.Path = strings.TrimRight(full.Path, "/") + rel.Path
	full.RawPath = strings.TrimRight(c.base.EscapedPath(), "/") + rel.EscapedPath()
	full.RawQuery = rel.RawQuery
	if q := form.Encode(); method == http.MethodGet && q != "" {
		full.RawQuery = q
	}
	var body io.Reader
	if method == http.MethodPost || method == http.MethodPut {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, full.String(), body)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConnectFailed, err)
	}
	req.Header.Set("Authorization", c.token)
	if method == http.MethodPost || method == http.MethodPut {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return c.roundTrip(c.http, req, method, path, out)
}

// roundTrip 发请求并解 PVE 信封(非 2xx → ErrConnectFailed,错误体不含 Token)。
// do 与 Upload 共用;hc 区分常规客户端(10s 超时)与上传客户端(无总超时)。
func (c *liveClient) roundTrip(hc *http.Client, req *http.Request, method, path string, out any) error {
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConnectFailed, redactURL(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("%w: 读取响应失败: %v", ErrConnectFailed, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var eb errorBody
		_ = json.Unmarshal(data, &eb)
		msg := strings.TrimSpace(eb.Message)
		if msg == "" && len(eb.Errors) > 0 {
			var parts []string
			for k, v := range eb.Errors {
				parts = append(parts, k+": "+v)
			}
			msg = strings.Join(parts, "; ")
		}
		if msg == "" {
			msg = strings.TrimSpace(string(bytes.ToValidUTF8(data, nil)))
			if len(msg) > 300 {
				msg = msg[:300]
			}
		}
		return &pveAPIError{status: resp.StatusCode, err: fmt.Errorf("%w: %s %s → HTTP %d: %s", ErrConnectFailed, method, path, resp.StatusCode, msg)}
	}
	var env apiResponse
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("%w: 响应不是合法 JSON: %v", ErrConnectFailed, err)
	}
	if out != nil && len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("%w: 响应数据解析失败: %v", ErrConnectFailed, err)
		}
	}
	return nil
}

func (c *liveClient) Get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, url.Values{}, out)
}

func (c *liveClient) Post(ctx context.Context, path string, form url.Values, out any) error {
	if form == nil {
		form = url.Values{}
	}
	return c.do(ctx, http.MethodPost, path, form, out)
}

func (c *liveClient) Put(ctx context.Context, path string, form url.Values, out any) error {
	if form == nil {
		form = url.Values{}
	}
	return c.do(ctx, http.MethodPut, path, form, out)
}

// Upload 流式 multipart 上传(语义见 pveClient 接口注释)。文件字节经 io.Pipe 直灌
// 请求体,内存占用与文件大小无关;返回前关闭管道并等待 producer 退出,避免调用方
// 关闭/复用 content 时与异步读取并发。
func (c *liveClient) Upload(ctx context.Context, path string, fields url.Values, fileField, fileName string, content io.Reader, out any) error {
	if content == nil {
		return fmt.Errorf("%w: 上传内容为空", ErrInvalidInput)
	}
	full := *c.base
	full.Path = strings.TrimRight(full.Path, "/") + path
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, full.String(), pr)
	if err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return fmt.Errorf("%w: %v", ErrConnectFailed, err)
	}
	req.Header.Set("Authorization", c.token)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	done := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			_ = pw.CloseWithError(err)
			done <- err
		}()
		for k, vs := range fields {
			for _, v := range vs {
				if err = mw.WriteField(k, v); err != nil {
					return
				}
			}
		}
		var fw io.Writer
		if fw, err = mw.CreateFormFile(fileField, fileName); err != nil {
			return
		}
		if _, err = io.Copy(fw, content); err != nil {
			return
		}
		err = mw.Close()
	}()

	uploadErr := c.roundTrip(c.uploadHTTP, req, http.MethodPost, path, out)
	_ = pr.CloseWithError(io.ErrClosedPipe)
	producerErr := <-done
	if uploadErr != nil {
		return uploadErr
	}
	if producerErr != nil && !errors.Is(producerErr, io.ErrClosedPipe) {
		return fmt.Errorf("%w: 上传流读取失败: %v", ErrConnectFailed, producerErr)
	}
	return nil
}

// apiMajor 取 PVE 主版本号(GET /version 的 release,如 "9.0.3" → 9),供 8.x/9.x
// 端点自适应;请求失败或解析不出 → 0(调用方按最旧形态处理并保留运行时兜底)。
func apiMajor(ctx context.Context, c pveClient) int {
	var ver apiVersion
	if err := c.Get(ctx, "/api2/json/version", &ver); err != nil {
		return 0
	}
	rel := strings.TrimSpace(ver.Release)
	if i := strings.IndexByte(rel, '.'); i > 0 {
		rel = rel[:i]
	}
	n, err := strconv.Atoi(rel)
	if err != nil {
		return 0
	}
	return n
}

// redactURL 从传输错误文本里抹掉可能内嵌的 query(保守:query 一律不外泄)。
func redactURL(s string) string {
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[:i]
	}
	return s
}

// ---- PVE JSON 类型怪癖收口 -----------------------------------------------------------

// flexInt 接受数字 / "123" / null。
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "null" || s == "" {
		*f = 0
		return nil
	}
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		// 数字前缀(如 "123.0")兜底:截到非数字位再试。
		i := 0
		for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '-' && i == 0) {
			i++
		}
		if i == 0 {
			return nil // 非数值容忍为 0
		}
		if _, err := fmt.Sscanf(s[:i], "%d", &n); err != nil {
			return nil
		}
	}
	*f = flexInt(n)
	return nil
}

// flexFloat 接受数字 / "1.25" / null。
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "null" || s == "" {
		*f = 0
		return nil
	}
	var v float64
	if _, err := fmt.Sscanf(s, "%g", &v); err != nil {
		*f = 0
		return nil
	}
	*f = flexFloat(v)
	return nil
}

// flexBool 接受 true/false/1/0/"1"/"0"/null。
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	s := strings.ToLower(strings.Trim(strings.TrimSpace(string(b)), `"`))
	switch s {
	case "true", "1", "yes", "on":
		*f = true
	case "false", "0", "no", "off", "null", "":
		*f = false
	default:
		*f = false
	}
	return nil
}

// ---- 端点响应模型(只建 P0 用到的字段) ----------------------------------------------

// apiVersion 对应 GET /version。
type apiVersion struct {
	Release string `json:"release"`
	Version string `json:"version"`
	Repoid  string `json:"repoid"`
}

// apiClusterStatusEntry 对应 GET /cluster/status 返回数组的一个条目。返回体是
// 混合扁平数组(Cluster.pm get_status):type=cluster 条目在前(集群名/成员数/
// quorate),type=node 条目随后(节点明细);单机形态只有一条 node 条目、无 quorate。
type apiClusterStatusEntry struct {
	Type    string   `json:"type"` // cluster | node
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	IP      string   `json:"ip"`      // [node]
	Level   string   `json:"level"`   // [node] 订阅级别
	NodeID  flexInt  `json:"nodeid"`  // [node] corosync nodeid
	Nodes   flexInt  `json:"nodes"`   // [cluster] 成员数量(含离线)
	Online  flexBool `json:"online"`  // [node]
	Local   flexBool `json:"local"`   // [node] 是否响应节点本机
	Quorate flexBool `json:"quorate"` // [cluster]
	Version flexInt  `json:"version"` // [cluster] corosync 配置版本
}

// apiResource 对应 GET /cluster/resources 的一个条目(node/vm/storage 共用)。
type apiResource struct {
	ID          string    `json:"id"` // "qemu/101"、"node/pve1"、"storage/local"
	Type        string    `json:"type"`
	Node        string    `json:"node"`
	Name        string    `json:"name"` // vm 条目的显示名
	Status      string    `json:"status"`
	VMID        flexInt   `json:"vmid"`
	Template    flexBool  `json:"template"` // vm 条目:是否模板(建机克隆源筛选用)
	CPU         flexFloat `json:"cpu"`
	MaxCPU      flexInt   `json:"maxcpu"`
	Mem         flexInt   `json:"mem"`
	MaxMem      flexInt   `json:"maxmem"`
	StorageName string    `json:"storage"` // storage 条目的存储名(字符串,如 local);用量看 disk/maxdisk
	Disk        flexInt   `json:"disk"`    // storage 条目:已用字节;vm 条目:rootfs 用量
	MaxDisk     flexInt   `json:"maxdisk"` // storage 条目:总字节
	Uptime      flexInt   `json:"uptime"`
	PluginType  string    `json:"plugintype"`
}

// apiTask 对应 GET /cluster/tasks 的一个条目。
type apiTask struct {
	UPID      string  `json:"upid"`
	Node      string  `json:"node"`
	Type      string  `json:"type"`
	Status    string  `json:"status"` // OK | ERROR | ""(运行中)
	User      string  `json:"user"`
	StartTime flexInt `json:"starttime"`
	EndTime   flexInt `json:"endtime"`
}

// apiTaskStatus 对应 GET /nodes/{node}/tasks/{upid}/status。
type apiTaskStatus struct {
	Status     string  `json:"status"` // running | stopped
	ExitStatus string  `json:"exitstatus"`
	Node       string  `json:"node"`
	Type       string  `json:"type"`
	StartTime  flexInt `json:"starttime"`
	EndTime    flexInt `json:"endtime"`
}

// apiStorageEntry 对应 GET /nodes/{node}/storage 的一个条目(存储状态;无 path,
// 文件型存储的路径经 GET /storage/{storage} 配置另取)。
type apiStorageEntry struct {
	Storage      string    `json:"storage"`
	Type         string    `json:"type"` // 插件类型:dir/nfs/lvmthin/zfs/rbd/...
	Active       flexBool  `json:"active"`
	Enabled      flexBool  `json:"enabled"`
	Shared       flexBool  `json:"shared"`
	Content      string    `json:"content"` // 逗号分隔内容类型:images,iso,vztmpl,backup,snippets,import...
	Used         flexInt   `json:"used"`
	Total        flexInt   `json:"total"`
	Avail        flexInt   `json:"avail"`
	UsedFraction flexFloat `json:"used_fraction"`
}

// apiNetworkEntry 对应 GET /nodes/{node}/network 的一个条目(网桥/网卡/VLAN 共用)。
type apiNetworkEntry struct {
	Iface       string   `json:"iface"`
	Type        string   `json:"type"` // bridge/OVSBridge/bond/eth/vlan/loopback/...
	Active      flexBool `json:"active"`
	Autostart   flexBool `json:"autostart"`
	Address     string   `json:"address"`
	Netmask     string   `json:"netmask"`
	Gateway     string   `json:"gateway"`
	BridgePorts string   `json:"bridge_ports"`
	Comments    string   `json:"comments"`
}

// apiAplEntry 对应 GET /nodes/{node}/aplinfo 的一个条目(LXC 模板目录)。
type apiAplEntry struct {
	Template    string `json:"template"` // 模板标识(如 ubuntu-24.04-standard)
	Package     string `json:"package"`
	Version     string `json:"version"`
	Section     string `json:"section"` // system/mail/...
	Description string `json:"description"`
	Location    string `json:"location"`
}

// apiContentEntry 对应 GET /nodes/{node}/storage/{storage}/content 的一个条目。
type apiContentEntry struct {
	Volid   string  `json:"volid"` // local:backup/vzdump-qemu-100-....vma.zst / local:import/x.qcow2
	Content string  `json:"content"`
	Format  string  `json:"format"`
	Size    flexInt `json:"size"`
	Used    flexInt `json:"used"`
	VMID    flexInt `json:"vmid"`
	Notes   string  `json:"notes"`
}

// errNilClient 防御:工厂返回 nil 客户端(配置错误)时给出可读错误。
var errNilClient = errors.New("pve client is nil")
