package registryhub

import (
	"context"
	"crypto/tls"
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

	// TLS PEM 落盘文件名与容器内路径(挂载 <certsDir>:/certs:ro,registry:2 原生支持)。
	certPEMName    = "fullchain.pem"
	certKeyName    = "privkey.pem"
	certPathInCont = "/certs/" + certPEMName
	keyPathInCont  = "/certs/" + certKeyName
)

// defaultArtifactDataDir / defaultCacheDataDir 返回存储目录默认值;baseDir 未知(空)时
// 返回空串,调用方须自行兜底。
func defaultArtifactDataDir(baseDir string) string {
	if baseDir == "" {
		return ""
	}
	return filepath.Join(baseDir, "data")
}

func defaultCacheDataDir(baseDir string) string {
	if baseDir == "" {
		return ""
	}
	return filepath.Join(baseDir, "cache")
}

// resolveArtifactDir / resolveCacheDir 返回实际生效的存储目录(配置优先,空回退默认)。
func (h *Hub) resolveArtifactDir(cfg *Config) string {
	if cfg.ArtifactDataDir != "" {
		return cfg.ArtifactDataDir
	}
	return defaultArtifactDataDir(h.opts.BaseDir)
}

func (h *Hub) resolveCacheDir(cfg *Config) string {
	if cfg.CacheDataDir != "" {
		return cfg.CacheDataDir
	}
	return defaultCacheDataDir(h.opts.BaseDir)
}

// resolveCertsDir 返回 TLS 证书 PEM 在控制机的落盘目录(部署时挂载进双服务容器)。
func (h *Hub) resolveCertsDir() string {
	return filepath.Join(h.opts.BaseDir, "certs")
}

// localRegistryURL 返回控制机本机访问 registry 的 base URL(TLS on → https;127.0.0.1 直打,
// 不经 external_addr,避免依赖外部 DNS/防火墙)。
func (h *Hub) localRegistryURL(cfg *Config, port int) string {
	scheme := "http"
	if cfg.TLSEnabled() {
		scheme = "https"
	}
	return fmt.Sprintf("%s://127.0.0.1:%d", scheme, port)
}

// probeHTTPClient 返回本机访问 registry 的 HTTP 客户端:TLS on 时跳过证书校验 —— 请求打的是
// 127.0.0.1,证书 SAN 不覆盖回环地址;此处只验证「服务在 speak TLS/HTTP」,不验证身份
// (控制机自调用,无中间人面,goosec 豁免)。TLS off 原样返回共享客户端。
func (h *Hub) probeHTTPClient(cfg *Config) *http.Client {
	if !cfg.TLSEnabled() {
		return h.opts.HTTPClient
	}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // 本机探活,见上
	}
}

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
//   - TLS:选了证书 → 双服务同挂只读证书卷 + REGISTRY_HTTP_TLS_*(同域名不同端口共用一张);
//     空 = 明文 HTTP
//   - 卷源整行加引号:路径可含空格;解析方(parseComposeDataDirs)按 `- "<dir>:/var/lib/registry"`
//     形态还原,该行格式不得变
func (h *Hub) renderCompose(cfg *Config) string {
	dataDir := h.resolveArtifactDir(cfg)
	cacheDir := h.resolveCacheDir(cfg)
	tlsEnv, tlsVol := "", ""
	if cfg.TLSEnabled() {
		tlsEnv = fmt.Sprintf("      %s: %s\n      %s: %s\n", "REGISTRY_HTTP_TLS_CERTIFICATE", certPathInCont, "REGISTRY_HTTP_TLS_KEY", keyPathInCont)
		tlsVol = fmt.Sprintf("      - \"%s:/certs:ro\"\n", h.resolveCertsDir())
	}
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
%s    volumes:
      - "%s:/var/lib/registry"
%s  registry-cache:
    image: %s
    container_name: %s
    restart: unless-stopped
    ports:
      - "%d:5000"
    environment:
      REGISTRY_PROXY_REMOTEURL: %q
%s    volumes:
      - "%s:/var/lib/registry"
%s`,
		registryImage, artifactCont, cfg.ArtifactPort, tlsEnv, dataDir, tlsVol,
		registryImage, cacheCont, cfg.CachePort, cfg.UpstreamURL, tlsEnv, cacheDir, tlsVol)
}

// DeployStack 在控制机本机部署/更新 registry 栈:渲染 compose → 落盘 → compose up -d
// (声明式:端口/上游变更后重跑即收敛,compose 只重建配置变化的服务)。
// 存储目录变更在此收敛(以已落盘 compose 记录的上一部署为准):制品目录变更 → 停制品容器
// 后整体迁移旧数据(不可再生);缓存目录变更 → 停缓存容器后清除旧目录(可再生)。
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
	composePath := filepath.Join(h.opts.BaseDir, composeFile)
	notes, err := h.convergeDataDirs(ctx, cfg, composePath)
	if err != nil {
		return &DeployResult{OK: false, Output: tail(strings.Join(notes, "\n"), 4096), Error: err.Error()}, nil
	}
	if cerr := h.syncCertPEMs(ctx, cfg); cerr != nil {
		return &DeployResult{OK: false, Error: cerr.Error()}, nil
	}
	for _, dir := range []string{h.opts.BaseDir, h.resolveArtifactDir(cfg), h.resolveCacheDir(cfg)} {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return nil, fmt.Errorf("registryhub: mkdir %s: %w", dir, mkErr)
		}
	}
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
	out := strings.TrimSpace(stdout)
	if len(notes) > 0 {
		out = strings.Join(notes, "\n") + "\n" + out
	}
	return &DeployResult{OK: true, Output: tail(out, 4096)}, nil
}

// convergeDataDirs 在写新 compose 前处理存储目录变更:从已落盘 compose 读出上一部署的
// data/cache 卷源,与本轮生效目录比较——制品目录变更则迁移旧数据,缓存目录变更则清除旧目录。
// 无 compose 文件(首次部署)或解析不出 → 无上一部署可依据,直接跳过(诚实不假装迁移)。
func (h *Hub) convergeDataDirs(ctx context.Context, cfg *Config, composePath string) ([]string, error) {
	raw, rerr := os.ReadFile(composePath)
	if rerr != nil {
		return nil, nil
	}
	oldArtifact, oldCache := parseComposeDataDirs(string(raw))
	newArtifact, newCache := h.resolveArtifactDir(cfg), h.resolveCacheDir(cfg)
	var notes []string
	if oldArtifact != "" && oldArtifact != newArtifact {
		n, err := h.migrateArtifactData(ctx, oldArtifact, newArtifact)
		notes = append(notes, n...)
		if err != nil {
			return notes, err
		}
	}
	if oldCache != "" && oldCache != newCache {
		n, err := h.resetCacheData(ctx, oldCache, newArtifact)
		notes = append(notes, n...)
		if err != nil {
			return notes, err
		}
	}
	return notes, nil
}

// migrateArtifactData 把制品旧数据整体搬到新目录(制品数据不可再生,必须迁移):
// 1) rm -f 制品容器(停写;容器不存在则无操作);2) 新目录已存在且非空 → 拒绝(防覆盖事故);
// 3) rename 优先,跨设备退化为 copy+delete。失败即中止部署(半迁移状态不假装成功)。
func (h *Hub) migrateArtifactData(ctx context.Context, oldDir, newDir string) ([]string, error) {
	if _, serr := os.Stat(oldDir); serr != nil {
		return []string{"制品旧目录不存在,跳过迁移:" + oldDir}, nil
	}
	// 容器可能正往旧目录写数据,迁移前强制移除(compose up 随后按新卷重建);容器不存在
	// 会以非零退出,忽略——迁移 FS 操作以真实目录状态为准。
	_, _, _, _ = h.opts.Runner.Run(ctx, "docker", []string{"rm", "-f", artifactCont}, "")
	if entries, derr := os.ReadDir(newDir); derr == nil && len(entries) > 0 {
		return nil, fmt.Errorf("制品新目录已存在且非空,为防覆盖拒绝迁移,请先清空或改用其他目录:%s", newDir)
	}
	if err := moveDir(oldDir, newDir); err != nil {
		return nil, fmt.Errorf("迁移制品数据 %s → %s 失败:%w", oldDir, newDir, err)
	}
	return []string{"已迁移制品数据:" + oldDir + " → " + newDir}, nil
}

// resetCacheData 清除缓存旧目录(缓存可再生,不迁移):1) rm -f 缓存容器;2) RemoveAll 旧目录。
// oldCache 不得等于 newArtifact(历史目录恰好与新一轮制品目录重合时,清除会吞掉刚迁入的数据)。
func (h *Hub) resetCacheData(ctx context.Context, oldDir, newArtifactDir string) ([]string, error) {
	if oldDir == newArtifactDir {
		return nil, fmt.Errorf("缓存旧目录 %s 与新制品目录重合,拒绝清除(会吞掉制品数据),请改用其他目录", oldDir)
	}
	if _, serr := os.Stat(oldDir); serr != nil {
		return []string{"缓存旧目录不存在,跳过清除:" + oldDir}, nil
	}
	_, _, _, _ = h.opts.Runner.Run(ctx, "docker", []string{"rm", "-f", cacheCont}, "")
	if err := os.RemoveAll(oldDir); err != nil {
		return nil, fmt.Errorf("清除缓存旧目录 %s 失败:%w", oldDir, err)
	}
	return []string{"已清除缓存旧目录:" + oldDir + "(缓存将按需重新拉取)"}, nil
}

// syncCertPEMs 在写 compose 前收敛 TLS 证书落盘:TLS on → 经 CertSource 进程内解密,把
// fullchain(0644)/私钥(0600)写入 certs 目录(挂载进容器,私钥不留全局可读);证书读取
// 失败诚实报错(compose 不动,绝不让半成品证书进容器)。TLS off → 清掉 certs 目录(切换
// 回明文后私钥不留盘)。基目录未知时拒绝(无处落盘,与 DeployStack 的 MkdirAll 语义一致)。
func (h *Hub) syncCertPEMs(ctx context.Context, cfg *Config) error {
	if h.opts.BaseDir == "" {
		return errors.New("栈基目录未知,无法收敛 TLS 证书")
	}
	dir := h.resolveCertsDir()
	if !cfg.TLSEnabled() {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("清除证书目录 %s 失败:%w", dir, err)
		}
		return nil
	}
	if h.opts.Certs == nil {
		return errors.New("证书能力未装配,无法部署 TLS 栈")
	}
	certPEM, keyPEM, err := h.opts.Certs.OpenCertPEM(ctx, cfg.TLSCertID)
	if err != nil {
		return fmt.Errorf("读取证书 %s 失败:%w", cfg.TLSCertID, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建证书目录 %s 失败:%w", dir, err)
	}
	if werr := os.WriteFile(filepath.Join(dir, certPEMName), []byte(certPEM), 0o644); werr != nil {
		return fmt.Errorf("写入 %s 失败:%w", certPEMName, werr)
	}
	if werr := os.WriteFile(filepath.Join(dir, certKeyName), []byte(keyPEM), 0o600); werr != nil {
		return fmt.Errorf("写入 %s 失败:%w", certKeyName, werr)
	}
	return nil
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
	st.ArtifactReachable = pingV2(ctx, h.probeHTTPClient(cfg), h.localRegistryURL(cfg, cfg.ArtifactPort))
	st.CacheReachable = pingV2(ctx, h.probeHTTPClient(cfg), h.localRegistryURL(cfg, cfg.CachePort))
	st.DataDirBytes = dirSize(h.resolveArtifactDir(cfg))
	st.CacheDirBytes = dirSize(h.resolveCacheDir(cfg))
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

// pingV2 对本机 <base>/v2/ 发 GET:200/401 都算可达(registry 无鉴权时 200;TLS 栈的
// base 为 https 且客户端已带 InsecureSkipVerify,见 probeHTTPClient)。
func pingV2(ctx context.Context, hc *http.Client, base string) bool {
	if hc == nil {
		hc = &http.Client{Timeout: 3 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v2/", nil)
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
