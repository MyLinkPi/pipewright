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
	"net/http"
	"net/url"
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
}

// clientFactory 按集群配置构造客户端(生产实现经 vault 解封 Token;测试注入 fake)。
type clientFactory func(apiURL, tokenName, tokenSecret, tlsMode, tlsFingerprint string) (pveClient, error)

// liveClient 是生产 pveClient。
type liveClient struct {
	base  *url.URL
	token string // 完整头值 PVEAPIToken=name=secret;只进请求头
	http  *http.Client
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
	return &liveClient{
		base:  u,
		token: "PVEAPIToken=" + tokenName + "=" + tokenSecret,
		http:  &http.Client{Timeout: pveRequestTimeout, Transport: &http.Transport{TLSClientConfig: tc}},
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

func (c *liveClient) do(ctx context.Context, method, path string, form url.Values, out any) error {
	full := *c.base
	full.Path = strings.TrimRight(full.Path, "/") + path
	if q := form.Encode(); method == http.MethodGet && q != "" {
		full.RawQuery = q
	}
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, full.String(), body)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConnectFailed, err)
	}
	req.Header.Set("Authorization", c.token)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
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
		return fmt.Errorf("%w: %s %s → HTTP %d: %s", ErrConnectFailed, method, path, resp.StatusCode, msg)
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

// apiClusterStatus 对应 GET /cluster/status(集群与单机两种形态共用宽松字段)。
type apiClusterStatus struct {
	Type    string         `json:"type"` // cluster | standalone
	Name    string         `json:"name"`
	ID      string         `json:"id"`
	Quorate flexBool       `json:"quorate"`
	Nodes   []apiClusterNd `json:"nodes"`
}
type apiClusterNd struct {
	ID     flexInt  `json:"id"`
	Name   string   `json:"name"`
	IP     string   `json:"ip"`
	Level  string   `json:"level"`
	Online flexBool `json:"online"`
	Local  flexBool `json:"local"`
	NodeID flexInt  `json:"nodeid"`
}

// apiResource 对应 GET /cluster/resources 的一个条目(node/vm/storage 共用)。
type apiResource struct {
	ID         string    `json:"id"` // "qemu/101"、"node/pve1"、"storage/local"
	Type       string    `json:"type"`
	Node       string    `json:"node"`
	Name       string    `json:"name"` // vm 条目的显示名
	Status     string    `json:"status"`
	VMID       flexInt   `json:"vmid"`
	CPU        flexFloat `json:"cpu"`
	MaxCPU     flexInt   `json:"maxcpu"`
	Mem        flexInt   `json:"mem"`
	MaxMem     flexInt   `json:"maxmem"`
	Storage    flexInt   `json:"storage"`
	MaxStorage flexInt   `json:"maxstorage"`
	Uptime     flexInt   `json:"uptime"`
	PluginType string    `json:"plugintype"`
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

// errNilClient 防御:工厂返回 nil 客户端(配置错误)时给出可读错误。
var errNilClient = errors.New("pve client is nil")
