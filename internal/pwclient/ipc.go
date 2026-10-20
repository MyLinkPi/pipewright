// ipc.go 是服务 ↔ 托盘 GUI 的通信通道:服务监听 127.0.0.1 随机端口,端口与 token 落
// <dataDir>/ipc.json(GUI 读取后携带 Bearer token 访问)。只绑 loopback + 每请求验 token,
// 即使端口被本机其他用户猜到也无法读写(配置含组网密钥,响应永不回显密钥)。
package pwclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// ipcInfo 是 GUI 侧定位服务通道的落盘文件(<dataDir>/ipc.json;服务每次启动重写)。
type ipcInfo struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

// Controller 是 IPC 端点背后的运行时契约(*Runtime 实现;测试可注入 fake)。
type Controller interface {
	Snapshot(ctx context.Context) StatusSnapshot
	LoadConfig() (Config, error)
	SaveConfig(cfg Config) error
	ImportJoin(raw []byte, replaceRoutes bool) (Config, error)
	RestartNow()
	TriggerDownload() error
	LogTail(n int) []string
}

// ipcServer 是 IPC HTTP 服务(构造后 Serve 在独立 goroutine,Addr 暴露实际监听地址)。
type ipcServer struct {
	srv   *http.Server
	ln    net.Listener
	token string
	ctl   Controller
}

// StartIPC 启动 IPC 服务并落盘 ipc.json(导出供装配层:cmd/pwclient 的服务与控制台形态)。
// 返回服务句柄;ctx 取消时自动关停并移除 ipc.json。
func StartIPC(ctx context.Context, dataDir string, ctl Controller) (*ipcServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("pwclient: ipc listen: %w", err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("pwclient: ipc token: %w", err)
	}
	s := &ipcServer{ln: ln, token: hex.EncodeToString(raw), ctl: ctl}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/config", s.handleConfigGet)
	mux.HandleFunc("PUT /api/config", s.handleConfigPut)
	mux.HandleFunc("POST /api/import", s.handleImport)
	mux.HandleFunc("POST /api/restart", s.handleRestart)
	mux.HandleFunc("POST /api/download", s.handleDownload)
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	s.srv = &http.Server{
		Handler:           s.authed(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	port := ln.Addr().(*net.TCPAddr).Port
	infoPath := filepath.Join(dataDir, "ipc.json")
	info := ipcInfo{Port: port, Token: s.token}
	blob, _ := json.Marshal(info)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("pwclient: ipc mkdir: %w", err)
	}
	if err := os.WriteFile(infoPath, blob, 0o644); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("pwclient: write ipc info: %w", err)
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
		_ = os.Remove(infoPath)
	}()
	go func() {
		_ = s.srv.Serve(ln)
	}()
	return s, nil
}

// authed 校验 Bearer token(常量时间比较;失败 403,不区分原因防探测)。
func (s *ipcServer) authed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := ""
		if h := r.Header.Get("Authorization"); len(h) > 7 && h[:7] == "Bearer " {
			got = h[7:]
		}
		if !subtleEqual(got, s.token) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func writeIPCJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeIPCError(w http.ResponseWriter, status int, err error) {
	writeIPCJSON(w, status, map[string]string{"error": err.Error()})
}

// configView 是配置回显(密钥只报存在性;与 Config 同构但绝不携带 networkSecret)。
type configView struct {
	Enabled  bool           `json:"enabled"`
	Join     joinView       `json:"join"`
	Routes   []string       `json:"routes"`
	Socks5   Socks5Config   `json:"socks5"`
	Proxies  []ProxyConfig  `json:"proxies"`
	EasyTier EasyTierConfig `json:"easytier"`
}

type joinView struct {
	NetworkName string   `json:"networkName"`
	HasSecret   bool     `json:"hasSecret"`
	Peers       []string `json:"peers"`
	VirtualIP   string   `json:"virtualIp"`
	Hostname    string   `json:"hostname"`
}

func toConfigView(cfg Config) configView {
	proxies := cfg.Proxies
	if proxies == nil {
		proxies = []ProxyConfig{}
	}
	for i := range proxies {
		if proxies[i].ExitNodes == nil {
			proxies[i].ExitNodes = []string{}
		}
	}
	return configView{
		Enabled: cfg.Enabled,
		Join: joinView{
			NetworkName: cfg.Join.NetworkName,
			HasSecret:   cfg.Join.NetworkSecret != "",
			Peers:       nonNil(cfg.Join.Peers),
			VirtualIP:   cfg.Join.VirtualIP,
			Hostname:    cfg.Join.Hostname,
		},
		Routes:   nonNil(cfg.Routes),
		Socks5:   cfg.Socks5,
		Proxies:  proxies,
		EasyTier: cfg.EasyTier,
	}
}

func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func (s *ipcServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeIPCJSON(w, http.StatusOK, s.ctl.Snapshot(r.Context()))
}

func (s *ipcServer) handleConfigGet(w http.ResponseWriter, _ *http.Request) {
	cfg, err := s.ctl.LoadConfig()
	if err != nil {
		writeIPCError(w, http.StatusInternalServerError, err)
		return
	}
	writeIPCJSON(w, http.StatusOK, toConfigView(cfg))
}

// ConfigSetRequest 是配置更新请求:全部字段可选,仅提交的字段覆盖;networkSecret 只写不读。
type ConfigSetRequest struct {
	Enabled         *bool          `json:"enabled"`
	NetworkName     *string        `json:"networkName"`
	NetworkSecret   *string        `json:"networkSecret"`
	Peers           *[]string      `json:"peers"`
	VirtualIP       *string        `json:"virtualIp"`
	Hostname        *string        `json:"hostname"`
	Routes          *[]string      `json:"routes"`
	Socks5Enabled   *bool          `json:"socks5Enabled"`
	Socks5Port      *int           `json:"socks5Port"`
	Socks5ExitNodes *[]string      `json:"socks5ExitNodes"`
	Socks5Proxies   *[]ProxyConfig `json:"socks5Proxies"`
	EasyTierBinDir  *string        `json:"easytierBinDir"`
	EasyTierRPC     *string        `json:"easytierRpc"`
}

func (s *ipcServer) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	var body ConfigSetRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeIPCError(w, http.StatusBadRequest, fmt.Errorf("请求体不是合法 JSON"))
		return
	}
	cfg, err := s.ctl.LoadConfig()
	if err != nil {
		writeIPCError(w, http.StatusInternalServerError, err)
		return
	}
	if body.Enabled != nil {
		cfg.Enabled = *body.Enabled
	}
	if body.NetworkName != nil {
		cfg.Join.NetworkName = *body.NetworkName
	}
	if body.NetworkSecret != nil && *body.NetworkSecret != "" {
		cfg.Join.NetworkSecret = *body.NetworkSecret
	}
	if body.Peers != nil {
		cfg.Join.Peers = *body.Peers
	}
	if body.VirtualIP != nil {
		cfg.Join.VirtualIP = *body.VirtualIP
	}
	if body.Hostname != nil {
		cfg.Join.Hostname = *body.Hostname
	}
	if body.Routes != nil {
		cfg.Routes = *body.Routes
	}
	if body.Socks5Enabled != nil {
		cfg.Socks5.Enabled = *body.Socks5Enabled
	}
	if body.Socks5Port != nil {
		cfg.Socks5.Port = *body.Socks5Port
	}
	if body.Socks5ExitNodes != nil {
		cfg.Socks5.ExitNodes = *body.Socks5ExitNodes
	}
	if body.Socks5Proxies != nil {
		cfg.Proxies = *body.Socks5Proxies
	}
	if body.EasyTierBinDir != nil {
		cfg.EasyTier.BinDir = *body.EasyTierBinDir
	}
	if body.EasyTierRPC != nil {
		cfg.EasyTier.RPCAddr = *body.EasyTierRPC
	}
	if err := s.ctl.SaveConfig(cfg); err != nil {
		writeIPCError(w, http.StatusBadRequest, err)
		return
	}
	s.ctl.RestartNow()
	writeIPCJSON(w, http.StatusOK, toConfigView(cfg))
}

// importBody 是导入平台导出文件:base64 内容 + 是否用文件内网段替换本机 routes。
type importBody struct {
	Content       string `json:"content"`       // pipewright-client.yaml 原文
	ReplaceRoutes bool   `json:"replaceRoutes"` // false = 并集去重,true = 用导出的网段覆盖
}

func (s *ipcServer) handleImport(w http.ResponseWriter, r *http.Request) {
	var body importBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeIPCError(w, http.StatusBadRequest, fmt.Errorf("请求体不是合法 JSON"))
		return
	}
	cfg, err := s.ctl.ImportJoin([]byte(body.Content), body.ReplaceRoutes)
	if err != nil {
		writeIPCError(w, http.StatusBadRequest, err)
		return
	}
	s.ctl.RestartNow()
	writeIPCJSON(w, http.StatusOK, toConfigView(cfg))
}

func (s *ipcServer) handleRestart(w http.ResponseWriter, _ *http.Request) {
	s.ctl.RestartNow()
	writeIPCJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *ipcServer) handleDownload(w http.ResponseWriter, _ *http.Request) {
	if err := s.ctl.TriggerDownload(); err != nil {
		writeIPCError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeIPCJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func (s *ipcServer) handleLogs(w http.ResponseWriter, r *http.Request) {
	n := 200
	if v := r.URL.Query().Get("n"); v != "" {
		var parsed int
		if _, err := fmt.Sscanf(v, "%d", &parsed); err == nil && parsed > 0 && parsed <= 5000 {
			n = parsed
		}
	}
	writeIPCJSON(w, http.StatusOK, map[string]any{"lines": s.ctl.LogTail(n)})
}

// ---- Runtime 对 Controller 的实现 ----

// LoadConfig 实现 Controller。
func (rt *Runtime) LoadConfig() (Config, error) { return rt.store.Load() }

// SaveConfig 实现 Controller(归一化经 Store.Save)。
func (rt *Runtime) SaveConfig(cfg Config) error { return rt.store.Save(cfg) }

// ImportJoin 实现 Controller:合并导出文件的 join 段;routes 按 replaceRoutes 决定
// 覆盖或并集;enabled 置 true(导入即加入是用户意图)。
func (rt *Runtime) ImportJoin(raw []byte, replaceRoutes bool) (Config, error) {
	cfg, err := rt.store.Load()
	if err != nil {
		return cfg, err
	}
	join, routes, _, err := ImportExport(raw)
	if err != nil {
		return cfg, err
	}
	cfg.Join = join
	if replaceRoutes {
		cfg.Routes = routes
	} else {
		cfg.Routes = append(append([]string{}, cfg.Routes...), routes...)
	}
	cfg.Enabled = true
	if err := rt.store.Save(cfg); err != nil {
		return cfg, err
	}
	return rt.store.Load()
}

// RestartNow 实现 Controller(别名已在 runtime.go)。
// LogTail 实现 Controller。
func (rt *Runtime) LogTail(n int) []string { return rt.logs.Tail(n) }

// ErrIPCUnavailable 表示 IPC 通道不可用(服务未运行/信息文件缺失)。
var ErrIPCUnavailable = errors.New("pwclient: 服务通道不可用(服务未运行?)")

// IPCClient 是 GUI/CLI 侧的 IPC 客户端(读 ipc.json 定位服务)。
type IPCClient struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// DialIPC 读取 <dataDir>/ipc.json 并构造客户端(服务未运行时返回 ErrIPCUnavailable)。
func DialIPC(dataDir string) (*IPCClient, error) {
	blob, err := os.ReadFile(filepath.Join(dataDir, "ipc.json"))
	if err != nil {
		return nil, ErrIPCUnavailable
	}
	var info ipcInfo
	if err := json.Unmarshal(blob, &info); err != nil || info.Port <= 0 || info.Token == "" {
		return nil, ErrIPCUnavailable
	}
	return &IPCClient{
		Base:  fmt.Sprintf("http://127.0.0.1:%d", info.Port),
		Token: info.Token,
		HTTP:  &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// do 发起一次带 token 的请求并解包 JSON(错误经 {error} 信封透出)。
func (c *IPCClient) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr *bytes.Reader
	if body != nil {
		blob, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(blob)
	}
	var req *http.Request
	var err error
	if rdr != nil {
		req, err = http.NewRequestWithContext(ctx, method, c.Base+path, rdr)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, c.Base+path, nil)
	}
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("pwclient: 服务通道鉴权失败(服务重启后请重试)")
	}
	if resp.StatusCode >= 400 {
		// 错误信封 {error: "..."} 统一转 error(调用方据此向用户展示原因)。
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if body.Error == "" {
			body.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return errors.New(body.Error)
	}
	if out != nil {
		if resp.StatusCode == http.StatusNoContent {
			return nil
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Status 拉取状态快照。
func (c *IPCClient) Status(ctx context.Context) (*StatusSnapshot, error) {
	var snap StatusSnapshot
	if err := c.do(ctx, http.MethodGet, "/api/status", nil, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// PutConfig 提交配置部分更新。
func (c *IPCClient) PutConfig(ctx context.Context, body ConfigSetRequest) (*configView, error) {
	var view configView
	if err := c.do(ctx, http.MethodPut, "/api/config", body, &view); err != nil {
		return nil, err
	}
	return &view, nil
}

// Import 提交导出文件导入。
func (c *IPCClient) Import(ctx context.Context, content string, replaceRoutes bool) (*configView, error) {
	var view configView
	if err := c.do(ctx, http.MethodPost, "/api/import", importBody{Content: content, ReplaceRoutes: replaceRoutes}, &view); err != nil {
		return nil, err
	}
	return &view, nil
}

// TriggerDownload 触发二进制下载。
func (c *IPCClient) TriggerDownload(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/download", map[string]bool{}, nil)
}

// Logs 拉取日志尾部 n 行。
func (c *IPCClient) Logs(ctx context.Context, n int) ([]string, error) {
	var out struct {
		Lines []string `json:"lines"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/logs?n=%d", n), nil, &out); err != nil {
		return nil, err
	}
	return out.Lines, nil
}

// LoadConfigView 拉取配置回显(密钥只报存在性)。
func (c *IPCClient) LoadConfigView(ctx context.Context) (*configView, error) {
	var view configView
	if err := c.do(ctx, http.MethodGet, "/api/config", nil, &view); err != nil {
		return nil, err
	}
	return &view, nil
}

// Restart 触发立即对账。
func (c *IPCClient) Restart(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/restart", map[string]bool{}, nil)
}
