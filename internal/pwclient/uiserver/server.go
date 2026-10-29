// server.go 是 uiserver 的装配与安全边界:
//   - 只监听 127.0.0.1(默认随机端口);
//   - 所有请求校验 Host 必须等于实际监听地址(防 DNS rebinding);
//   - /api/* 与 /local/* 额外校验 X-PW-Token(常量时间比较),静态资源只需 Host;
//   - 端口与令牌落 <dataDir>/ui.json(0600),进程退出移除。
//
// 界面令牌经 URL query(?t=…)交给前端:部分宿主/自动化环境导航时会丢失 fragment;
// 前端读取后立即 replaceState 清掉 query,服务端也不记录请求行(不写访问日志)。
package uiserver

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Options 是 Start 的装配参数。
type Options struct {
	DataDir string
	Local   LocalActions
	Listen  string // 默认 127.0.0.1:0
	Token   string // 空 = 随机生成(测试可注入固定值)
}

// Server 是本地界面服务句柄。
type Server struct {
	ln       net.Listener
	srv      *http.Server
	dist     fs.FS
	token    string
	dataDir  string
	local    LocalActions
	infoPath string
	port     int
}

// uiInfo 是落盘的定位文件(与 IPC 的 ipc.json 同款,权限 0600)。
type uiInfo struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

// Start 启动界面服务并落盘 ui.json;ctx 取消时关停并移除 ui.json。
func Start(ctx context.Context, opts Options) (*Server, error) {
	if opts.Local == nil {
		return nil, fmt.Errorf("uiserver: LocalActions 不能为空")
	}
	if strings.TrimSpace(opts.DataDir) == "" {
		return nil, fmt.Errorf("uiserver: DataDir 不能为空")
	}
	addr := strings.TrimSpace(opts.Listen)
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("uiserver: listen: %w", err)
	}
	token := opts.Token
	if token == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("uiserver: token: %w", err)
		}
		token = hex.EncodeToString(raw)
	}
	dist, err := distRoot()
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("uiserver: 前端资源: %w", err)
	}
	if err := os.MkdirAll(opts.DataDir, 0o755); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("uiserver: mkdir: %w", err)
	}
	s := &Server{
		ln:       ln,
		dist:     dist,
		token:    token,
		dataDir:  opts.DataDir,
		local:    opts.Local,
		infoPath: filepath.Join(opts.DataDir, "ui.json"),
		port:     ln.Addr().(*net.TCPAddr).Port,
	}
	s.srv = &http.Server{Handler: s.routes(), ReadHeaderTimeout: 5 * time.Second}

	blob, _ := json.Marshal(uiInfo{Port: s.port, Token: token})
	if err := os.WriteFile(s.infoPath, blob, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("uiserver: 写 ui.json: %w", err)
	}

	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(sctx)
		_ = os.Remove(s.infoPath)
	}()
	go func() {
		_ = s.srv.Serve(ln)
	}()
	return s, nil
}

// URLBase 返回不带界面令牌的基址(测试与日志用)。
func (s *Server) URLBase() string {
	return fmt.Sprintf("http://127.0.0.1:%d", s.port)
}

// URL 返回带界面令牌的访问地址。令牌放 query 而非 fragment:部分宿主/自动化环境
// 导航时会丢失 fragment;前端读到后会立即 replaceState 清掉 query,服务端也不记录请求行。
func (s *Server) URL() string {
	return s.URLBase() + "/?t=" + s.token
}

// Addr 返回实际监听地址(测试与日志用)。
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Token 返回界面令牌(测试用;宿主一律用 URL)。
func (s *Server) Token() string { return s.token }

// Close 主动关停(宿主退出时兜底;ctx 取消路径已覆盖常规流程)。
func (s *Server) Close() error {
	_ = os.Remove(s.infoPath)
	return s.srv.Close()
}

// routes 组装路由:最长前缀优先(/api/、/local/ 先于 /)。
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/", s.requireToken(http.HandlerFunc(s.handleAPI)))
	mux.Handle("/local/", s.requireToken(http.HandlerFunc(s.handleLocal)))
	mux.HandleFunc("/", s.handleStatic)
	return s.requireHost(mux)
}

// requireHost 校验 Host 为实际监听地址,拒绝 DNS rebinding。
func (s *Server) requireHost(next http.Handler) http.Handler {
	want := fmt.Sprintf("127.0.0.1:%d", s.port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != want {
			writeError(w, http.StatusForbidden, "非法 Host")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireToken 校验界面令牌;常量时间比较,失败不区分原因。
func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-PW-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeError(w, http.StatusForbidden, "未授权:界面令牌缺失或错误")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleStatic 提供内嵌前端资源;未命中回落到 index.html(hash 路由不需要服务端 rewrite,
// 但保留回落以容忍直接访问 /index.html 之外的深层路径)。
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if p == "" || p == "." {
		p = "index.html"
	}
	if strings.HasSuffix(p, "/") {
		p += "index.html"
	}
	data, name, err := s.readDist(p)
	if err != nil {
		data, name, err = s.readDist("index.html")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "前端资源缺失:请运行 make client-web")
			return
		}
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if name == "index.html" {
		w.Header().Set("Cache-Control", "no-store")
		// 注入持久化主题到 <html data-theme>:首屏(JS 执行前)即为正确主题,
		// 绕开随机端口下失效的 localStorage;no-store 已禁用缓存,覆写安全。
		data = injectTheme(data, s.LoadTheme())
	} else {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}

// readDist 读取资源文件(拒绝目录)。
func (s *Server) readDist(name string) ([]byte, string, error) {
	f, err := s.dist.Open(name)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		return nil, "", fmt.Errorf("uiserver: %q 不是文件", name)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, "", err
	}
	return data, name, nil
}

// writeJSON 写出 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError 写出统一错误信封 {error:"..."},前端据此展示。
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
