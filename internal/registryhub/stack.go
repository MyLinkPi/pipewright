package registryhub

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// 栈布局常量(容器名固定,便于 docker inspect 巡检与外部运维定位)。
const (
	defaultBaseDir = "/var/lib/pipewright/registry"
	composeFile    = "docker-compose.yml"
	stackProject   = "pipewright-registry"
	// registryImage 是双服务共用的官方镜像(首次部署需控制机能直连上游拉取,自举不依赖缓存)。
	registryImage = "registry:2"
	artifactCont  = "pipewright-registry"
	cacheCont     = "pipewright-registry-cache"
)

// DeployResult 是 DeployStack 的结果(ok=false 时 error 人读)。
type DeployResult struct {
	OK     bool   `json:"ok"`
	Output string `json:"output"` // compose up 输出尾部(人读;截断)
	Error  string `json:"error"`
}

// Status 是栈当前状态(全部 best-effort 探测,单项失败不影响其余字段)。
type Status struct {
	Enabled           bool   `json:"enabled"`
	Deployed          bool   `json:"deployed"`          // compose 文件已生成
	ArtifactRunning   bool   `json:"artifactRunning"`   // 制品容器 State=running
	CacheRunning      bool   `json:"cacheRunning"`      // 缓存容器 State=running
	ArtifactReachable bool   `json:"artifactReachable"` // 本机 /v2/ 探活
	CacheReachable    bool   `json:"cacheReachable"`    // 本机 /v2/ 探活
	DataDirBytes      int64  `json:"dataDirBytes"`      // 制品存储占用
	CacheDirBytes     int64  `json:"cacheDirBytes"`     // 缓存存储占用
	Image             string `json:"image"`             // 栈镜像
	Error             string `json:"error"`             // 探测层错误汇总(可为空)
}

// execRunner 是 LocalRunner 的 os/exec 默认实现(array 传入,绝不经 shell)。
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args []string, stdin string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return outBuf.String(), errBuf.String(), 0, nil
	case errors.As(err, &ee):
		return outBuf.String(), errBuf.String(), ee.ExitCode(), nil
	default:
		return "", "", -1, err
	}
}

// composeBin 探测本机 compose 命令形态:v2(`docker compose`)优先,v1(`docker-compose`)兜底。
// 返回命令前缀([]string);无 docker/compose → ErrNoLocalDocker。
func (h *Hub) composeBin(ctx context.Context) ([]string, error) {
	if out, _, code, err := h.opts.Runner.Run(ctx, "docker", []string{"compose", "version"}, ""); err == nil && code == 0 && out != "" {
		return []string{"docker", "compose"}, nil
	}
	if _, _, code, err := h.opts.Runner.Run(ctx, "docker-compose", []string{"version"}, ""); err == nil && code == 0 {
		return []string{"docker-compose"}, nil
	}
	return nil, ErrNoLocalDocker
}

// renderCompose 生成栈 compose 文件内容(端口/上游/存储路径全部具值渲染,不依赖 env 替换)。
//   - 制品服务:开 REGISTRY_STORAGE_DELETE_ENABLED(保留策略 DELETE manifest 需要)
//   - 缓存服务:REGISTRY_PROXY_REMOTEURL=上游(pull-through 只读代理;切上游改此值重建即可)
func (h *Hub) renderCompose(cfg *Config) string {
	dataDir := filepath.Join(h.opts.BaseDir, "data")
	cacheDir := filepath.Join(h.opts.BaseDir, "cache")
	return fmt.Sprintf(`# 由 pipewright 自动生成,手动修改会在下次部署时被覆盖。
services:
  registry:
    image: %s
    container_name: %s
    restart: unless-stopped
    ports:
      - "%d:5000"
    environment:
      REGISTRY_STORAGE_DELETE_ENABLED: "true"
    volumes:
      - %s:/var/lib/registry
  registry-cache:
    image: %s
    container_name: %s
    restart: unless-stopped
    ports:
      - "%d:5000"
    environment:
      REGISTRY_PROXY_REMOTEURL: %q
    volumes:
      - %s:/var/lib/registry
`,
		registryImage, artifactCont, cfg.ArtifactPort, dataDir,
		registryImage, cacheCont, cfg.CachePort, cfg.UpstreamURL, cacheDir)
}

// DeployStack 在控制机本机部署/更新 registry 栈:渲染 compose → 落盘 → compose up -d
// (声明式:端口/上游变更后重跑即收敛,compose 只重建配置变化的服务)。
// 未 enabled → ErrDisabled;本机无 docker/compose → ErrNoLocalDocker(不假装成功)。
func (h *Hub) DeployStack(ctx context.Context) (*DeployResult, error) {
	cfg, err := h.Get(ctx)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, ErrDisabled
	}
	bin, err := h.composeBin(ctx)
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"", "data", "cache"} {
		dir := h.opts.BaseDir
		if sub != "" {
			dir = filepath.Join(h.opts.BaseDir, sub)
		}
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return nil, fmt.Errorf("registryhub: mkdir %s: %w", dir, mkErr)
		}
	}
	composePath := filepath.Join(h.opts.BaseDir, composeFile)
	if wErr := os.WriteFile(composePath, []byte(h.renderCompose(cfg)), 0o644); wErr != nil {
		return nil, fmt.Errorf("registryhub: write compose: %w", wErr)
	}
	args := append(bin, "-p", stackProject, "-f", composePath, "up", "-d")
	upCtx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	stdout, stderr, code, err := h.opts.Runner.Run(upCtx, args[0], args[1:], "")
	if err != nil || code != 0 {
		out := strings.TrimSpace(stderr)
		if out == "" {
			out = strings.TrimSpace(stdout)
		}
		res := &DeployResult{OK: false, Output: tail(out, 4096)}
		if out == "" {
			res.Error = "compose up 以非零状态退出"
		} else {
			res.Error = tail(out, 1024)
		}
		return res, nil
	}
	return &DeployResult{OK: true, Output: tail(strings.TrimSpace(stdout), 4096)}, nil
}

// Status 汇总栈当前状态(compose 文件存在性 + 容器 State + 本机 /v2/ 探活 + 存储占用)。
// 全部探测 best-effort:任何子项失败仅置 false/汇总 Error,绝不因单项失败整体报错。
func (h *Hub) Status(ctx context.Context) (*Status, error) {
	cfg, err := h.Get(ctx)
	if err != nil {
		return nil, err
	}
	st := &Status{Enabled: cfg.Enabled, Image: registryImage}
	composePath := filepath.Join(h.opts.BaseDir, composeFile)
	if _, serr := os.Stat(composePath); serr == nil {
		st.Deployed = true
	}
	st.ArtifactRunning = h.containerRunning(ctx, artifactCont)
	st.CacheRunning = h.containerRunning(ctx, cacheCont)
	st.ArtifactReachable = pingV2(ctx, h.opts.HTTPClient, cfg.ArtifactPort)
	st.CacheReachable = pingV2(ctx, h.opts.HTTPClient, cfg.CachePort)
	st.DataDirBytes = dirSize(filepath.Join(h.opts.BaseDir, "data"))
	st.CacheDirBytes = dirSize(filepath.Join(h.opts.BaseDir, "cache"))
	return st, nil
}

// containerRunning 经 `docker inspect -f {{.State.Status}}` 查容器是否 running(best-effort)。
func (h *Hub) containerRunning(ctx context.Context, name string) bool {
	out, _, code, err := h.opts.Runner.Run(ctx, "docker", []string{"inspect", "-f", "{{.State.Status}}", name}, "")
	if err != nil || code != 0 {
		return false
	}
	return strings.TrimSpace(out) == "running"
}

// pingV2 对本机 <port>/v2/ 发 GET:200/401 都算可达(registry 无鉴权时 200)。
func pingV2(ctx context.Context, hc *http.Client, port int) bool {
	if hc == nil {
		hc = &http.Client{Timeout: 3 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/v2/", port), nil)
	if err != nil {
		return false
	}
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, err := hc.Do(req.WithContext(pingCtx))
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnauthorized
}

// dirSize 递归统计目录字节占用(跨平台、无 CGO;registry 层文件数可控,状态页频率下可接受)。
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// tail 截断字符串到最多 n 字节(超长省略中间,保尾部;供人读输出)。
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
