package aptcacherng

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// 栈布局常量(容器名固定,便于 docker inspect 巡检与外部运维定位)。
const (
	defaultBaseDir = "/var/lib/pipewright/apt-cacher-ng"
	composeFile    = "docker-compose.yml"
	nginxConfFile  = "nginx-stream.conf" // TLS 前端主配置(与 compose 同目录落盘)
	stackProject   = "pipewright-apt-cacher"
	stackNetwork   = "pipewright-apt-cacher-net"
	// aptImage 是缓存主体镜像;tlsImage 是 TLS 态前端(官方镜像自带 stream 动态模块)。
	aptImage = "sameersbn/apt-cacher-ng"
	tlsImage = "nginx:stable-alpine"
	aptCont  = "pipewright-apt-cacher-ng"
	tlsCont  = "pipewright-apt-cacher-tls"
	contPort = "3142" // apt-cacher-ng 容器内监听端口
	// tlsFrontPort 是 nginx TLS 前端在**容器内**的监听端口(固定 443):宿主经
	// "<cfg.Port>:443" 发布,用户可改的是宿主端口,容器侧恒 443(与 nginx listen 一致)。
	tlsFrontPort = 443
	cacheVol     = "/var/cache/apt-cacher-ng"
	certsVol     = "/certs" // 宿主 certs 目录挂进 TLS 前端(只读)
)

// 证书 PEM 落盘文件名(与容器内路径;TLS 前端挂载读取)。
const (
	certPEMName    = "fullchain.pem"
	certKeyName    = "privkey.pem"
	certPathInCont = certsVol + "/" + certPEMName
	keyPathInCont  = certsVol + "/" + certKeyName
)

// defaultCacheDataDir 返回缓存目录默认值;baseDir 未知(空)时返回空串,调用方须自行兜底。
func defaultCacheDataDir(baseDir string) string {
	if baseDir == "" {
		return ""
	}
	return filepath.Join(baseDir, "cache")
}

// resolveCacheDir 返回实际生效的缓存目录(配置优先,空回退默认)。
func (h *Hub) resolveCacheDir(cfg *Config) string {
	if cfg.DataDir != "" {
		return cfg.DataDir
	}
	return defaultCacheDataDir(h.opts.BaseDir)
}

// resolveCertsDir 返回 TLS 证书 PEM 在控制机的落盘目录(部署时挂载进 TLS 前端容器)。
func (h *Hub) resolveCertsDir() string {
	return filepath.Join(h.opts.BaseDir, "certs")
}

// nginxConfHostPath / nginxConfInCont 是 TLS 前端主配置的宿主落盘路径与容器内路径。
// 容器内必须挂到 /etc/nginx/nginx.conf:生成内容是完整主配置(load_module/events/stream
// 顶层块),而官方 nginx 镜像只加载默认主配置,不会 include 任何 stream.conf 之类的
// 旁路文件——挂别处等于整个 TLS 前端配置完全不生效。
func (h *Hub) nginxConfHostPath() string { return filepath.Join(h.opts.BaseDir, nginxConfFile) }
func nginxConfInCont() string            { return "/etc/nginx/nginx.conf" }

// localProxyURL 返回控制机本机访问代理的 base URL(TLS on → https 打 TLS 前端发布端口;
// 127.0.0.1 直打,不经 external_addr,避免依赖外部 DNS/防火墙)。
func (h *Hub) localProxyURL(cfg *Config) string {
	scheme := "http"
	if cfg.TLSEnabled() {
		scheme = "https"
	}
	return fmt.Sprintf("%s://127.0.0.1:%d", scheme, cfg.Port)
}

// probeHTTPClient 返回本机访问代理的 HTTP 客户端:TLS on 时跳过证书校验 —— 请求打的是
// 127.0.0.1,证书 SAN 不覆盖回环地址;此处只验证「服务在 speak TLS/HTTP」,不验证身份
// (控制机自调用,无中间人面,gosec 豁免)。TLS off 原样返回共享客户端。
func (h *Hub) probeHTTPClient(cfg *Config) *http.Client {
	if !cfg.TLSEnabled() {
		return h.opts.HTTPClient
	}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // 本机探活,见上
	}
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

// renderCompose 生成栈 compose 文件内容(端口/存储路径全部具值渲染,不依赖 env 替换)。
//   - 明文态:单服务 apt-cacher-ng,直接对宿主发布端口。
//   - TLS 态:apt-cacher-ng 不发布宿主端口(仅自定义 bridge 网络内按容器名可达),
//     前置 nginx stream 前端(声明式渲染的极简主配置 + 只读证书卷)对宿主发布端口。
//   - apt-cacher-ng 服务带 init: true:sameersbn 镜像官方 compose 即如此(会 fork 子
//     进程,无 init 的长跑容器会积累僵尸进程)。
//   - 卷源整行加引号:路径可含空格;解析方(parseComposeCacheDir)按
//     `- "<dir>:/var/cache/apt-cacher-ng"` 形态还原,该行格式不得变。
func (h *Hub) renderCompose(cfg *Config) string {
	cacheDir := h.resolveCacheDir(cfg)
	if !cfg.TLSEnabled() {
		return fmt.Sprintf(`# 由 pipewright 自动生成,手动修改会在下次部署时被覆盖。
services:
  apt-cacher-ng:
    image: %s
    container_name: %s
    restart: unless-stopped
    init: true
    ports:
      - "%d:%s"
    volumes:
      - "%s:%s"
`,
			aptImage, aptCont, cfg.Port, contPort, cacheDir, cacheVol)
	}
	return fmt.Sprintf(`# 由 pipewright 自动生成,手动修改会在下次部署时被覆盖。
services:
  apt-cacher-ng:
    image: %s
    container_name: %s
    restart: unless-stopped
    init: true
    networks:
      - cache
    volumes:
      - "%s:%s"
  apt-cacher-tls:
    image: %s
    container_name: %s
    restart: unless-stopped
    ports:
      - "%d:%s"
    networks:
      - cache
    volumes:
      - "%s:/certs:ro"
      - "%s:%s:ro"
networks:
  cache:
    name: %s
`,
		aptImage, aptCont, cacheDir, cacheVol,
		tlsImage, tlsCont, cfg.Port, fmt.Sprint(tlsFrontPort),
		h.resolveCertsDir(),
		h.nginxConfHostPath(), nginxConfInCont(),
		stackNetwork)
}

// renderNginxStreamConf 生成 TLS 前端主配置(纯函数,可 golden 单测):nginx stream
// 做 TLS 终止 + TCP 透传到栈内 apt-cacher-ng(客户端与源站之间的 TLS 由 apt 端到端完成,
// CONNECT 隧道原样透传)。上游按容器名经 Docker 内嵌 DNS 运行期解析(变量形式 proxy_pass
// 强制每请求重解析,缓存容器重建换 IP 不影响);配置缺失容器会拒绝启动(诚实失败,
// 由探活/状态暴露),不带兜底 http server。listen 端口 = 容器侧固定 tlsFrontPort(443),
// 与 compose 里 "<cfg.Port>:443" 的容器端口一致(用户改的是宿主端口,不是这个)。
func renderNginxStreamConf() string {
	return fmt.Sprintf(`# 由 pipewright 自动生成,请勿手改(HTTPS apt 代理的 TLS 终止前端)。
load_module /usr/lib/nginx/modules/ngx_stream_module.so;
events { worker_connections 1024; }
stream {
    resolver 127.0.0.11 valid=10s ipv6=off;
    server {
        listen %d ssl;
        ssl_certificate %s;
        ssl_certificate_key %s;
        ssl_protocols TLSv1.2 TLSv1.3;
        set $upstream_backend %s;
        proxy_pass $upstream_backend;
        proxy_connect_timeout 10s;
        proxy_timeout 3600s;
    }
}
`, tlsFrontPort, certPathInCont, keyPathInCont, aptCont+":"+contPort)
}

// parseComposeCacheDir 从已落盘 compose 读出上一部署的缓存卷宿主目录(两行卷源格式
// 分别对应明文/TLS 态,取首个命中;解析不出 → 空串,调用方视为无上一部署)。
func parseComposeCacheDir(content string) string {
	marker := ":" + cacheVol + `"`
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, `- "`) || !strings.HasSuffix(t, marker) {
			continue
		}
		host := strings.TrimSuffix(strings.TrimPrefix(t, `- "`), marker)
		if host != "" {
			return host
		}
	}
	return ""
}

// DeployResult 是 DeployStack/ClearCache 的结果(ok=false 时 error 人读)。
type DeployResult struct {
	OK     bool   `json:"ok"`
	Output string `json:"output"` // compose 输出尾部(人读;截断)
	Error  string `json:"error"`
}

// Status 是栈当前状态(全部 best-effort 探测,单项失败不影响其余字段)。
type Status struct {
	Enabled    bool   `json:"enabled"`
	Deployed   bool   `json:"deployed"`   // compose 文件已生成
	Running    bool   `json:"running"`    // 主体容器 running(TLS 态另看 tlsRunning)
	TLSRunning bool   `json:"tlsRunning"` // TLS 前端 running(TLS 态有意义;明文态恒 false)
	TLSEnabled bool   `json:"tlsEnabled"` // 配置是否走 TLS 前端
	Reachable  bool   `json:"reachable"`  // 本机代理端口探活(有 HTTP 响应即算)
	CacheBytes int64  `json:"cacheBytes"` // 缓存存储占用
	Image      string `json:"image"`      // 栈主体镜像
	Error      string `json:"error"`      // 探测层错误汇总(可为空)
}

// ImageUpgradeResult 是 UpgradeImage 的结果(ok=false 时 error 人读)。
type ImageUpgradeResult struct {
	OK      bool   `json:"ok"`
	Changed bool   `json:"changed"` // 是否拉到了新镜像并重建容器(false = 已是最新,未动容器)
	Image   string `json:"image"`
	Before  string `json:"before"` // 升级前栈镜像 ID(截短 sha;TLS 态为 "主体,前端" 拼接;首次/本地无镜像 = "")
	After   string `json:"after"`  // 升级后栈镜像 ID(形态同 Before)
	Output  string `json:"output"` // pull/compose 输出尾部(人读;截断)
	Error   string `json:"error"`
}

// localImageID 返回本机指定镜像的 ID(`docker image inspect -f {{.Id}}`;本地无此镜像 → "")。
func (h *Hub) localImageID(ctx context.Context, image string) string {
	out, _, code, err := h.opts.Runner.Run(ctx, "docker", []string{"image", "inspect", "-f", "{{.Id}}", image}, "")
	if err != nil || code != 0 {
		return ""
	}
	return strings.TrimSpace(out)
}

// shortImageID 把镜像 ID 截短成展示形态(去 sha256: 前缀,留 12 位)。
func shortImageID(id string) string {
	id = strings.TrimPrefix(strings.TrimSpace(id), "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// stackImages 返回当前配置形态下栈用到的镜像(TLS 态含 nginx 前端)。
func stackImages(cfg *Config) []string {
	if cfg.TLSEnabled() {
		return []string{aptImage, tlsImage}
	}
	return []string{aptImage}
}

// UpgradeImage 升级栈镜像:pull 栈用到的全部镜像 → 比对本地镜像 ID —— 任一变了才走
// DeployStack 重建容器(compose 声明式收敛;缓存数据在宿主卷,重建不丢),全部未变直接
// 收工(不打扰运行中的容器)。比对覆盖全栈镜像(TLS 态含 nginx 前端),只看主体镜像会
// 漏掉"仅前端镜像有新版"的情形(Changed=false 跳过重建,新镜像永远不生效)。
// 未启用 → ErrDisabled;本机无 docker/compose → ErrNoLocalDocker;pull 失败 →
// ok:false + 人读 error(不触碰现有容器)。
func (h *Hub) UpgradeImage(ctx context.Context) (*ImageUpgradeResult, error) {
	cfg, err := h.Get(ctx)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, ErrDisabled
	}
	if _, err := h.composeBin(ctx); err != nil {
		return nil, err
	}
	images := stackImages(cfg)
	res := &ImageUpgradeResult{Image: aptImage, Before: h.stackImageIDs(ctx, images)}
	pullCtx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	var pulled []string
	for _, img := range images {
		_, stderr, code, err := h.opts.Runner.Run(pullCtx, "docker", []string{"pull", img}, "")
		if err != nil || code != 0 {
			res.Error = tail(strings.TrimSpace(firstNonEmptyStr(stderr, "docker pull 以非零状态退出")), 1024)
			return res, nil
		}
		pulled = append(pulled, tail(strings.TrimSpace(stderr), 1024)) // docker pull 进度走 stderr
	}
	res.Output = tail(strings.Join(pulled, "\n"), 2048)
	res.After = h.stackImageIDs(ctx, images)
	res.Changed = res.After != res.Before
	if !res.Changed {
		res.OK = true
		return res, nil
	}
	dep, derr := h.DeployStack(ctx)
	if derr != nil {
		return res, derr
	}
	res.OK = dep.OK
	res.Error = dep.Error
	if dep.Output != "" {
		res.Output = tail(res.Output+"\n"+dep.Output, 4096)
	}
	return res, nil
}

// stackImageIDs 返回栈全部镜像本地 ID 的拼接形态(变更比对与展示用;明文单镜像时即
// 其截短 ID 本身,TLS 态为 "主体,前端" 两个截短 ID)。
func (h *Hub) stackImageIDs(ctx context.Context, images []string) string {
	ids := make([]string, 0, len(images))
	for _, img := range images {
		ids = append(ids, shortImageID(h.localImageID(ctx, img)))
	}
	return strings.Join(ids, ",")
}

// DeployStack 在控制机本机部署/更新 apt 缓存栈:渲染 compose → 落盘 → compose up -d
// (声明式:端口/形态(明文↔TLS)/存储路径变更后重跑即收敛,compose 只重建配置变化的服务)。
// 缓存目录变更在此收敛(以已落盘 compose 记录的上一部署为准):清旧目录(可再生,不迁移)。
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
	notes, err := h.convergeCacheDir(ctx, cfg, composePath)
	if err != nil {
		return &DeployResult{OK: false, Output: tail(strings.Join(notes, "\n"), 4096), Error: err.Error()}, nil
	}
	if cerr := h.syncCertPEMs(ctx, cfg); cerr != nil {
		return &DeployResult{OK: false, Error: cerr.Error()}, nil
	}
	for _, dir := range []string{h.opts.BaseDir, h.resolveCacheDir(cfg)} {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return nil, fmt.Errorf("aptcacherng: mkdir %s: %w", dir, mkErr)
		}
	}
	if cfg.TLSEnabled() {
		if wErr := os.WriteFile(h.nginxConfHostPath(), []byte(renderNginxStreamConf()), 0o644); wErr != nil {
			return nil, fmt.Errorf("aptcacherng: write nginx conf: %w", wErr)
		}
	}
	if wErr := os.WriteFile(composePath, []byte(h.renderCompose(cfg)), 0o644); wErr != nil {
		return nil, fmt.Errorf("aptcacherng: write compose: %w", wErr)
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

// convergeCacheDir 在写新 compose 前处理缓存目录变更:从已落盘 compose 读出上一部署的
// 缓存卷源,与本轮生效目录比较——不同则清除旧目录(缓存可再生,不迁移)。
// 无 compose 文件(首次部署)或解析不出 → 无上一部署可依据,直接跳过(诚实不动手)。
func (h *Hub) convergeCacheDir(ctx context.Context, cfg *Config, composePath string) ([]string, error) {
	raw, rerr := os.ReadFile(composePath)
	if rerr != nil {
		return nil, nil
	}
	oldCache := parseComposeCacheDir(string(raw))
	newCache := h.resolveCacheDir(cfg)
	if oldCache == "" || oldCache == newCache {
		return nil, nil
	}
	n, err := h.resetCacheData(ctx, oldCache)
	if err != nil {
		return n, err
	}
	return n, nil
}

// resetCacheData 清除缓存旧目录(缓存可再生,不迁移):1) rm -f 主体容器(停写;不存在
// 则无操作);2) RemoveAll 旧目录。
func (h *Hub) resetCacheData(ctx context.Context, oldDir string) ([]string, error) {
	if _, serr := os.Stat(oldDir); serr != nil {
		return []string{"缓存旧目录不存在,跳过清除:" + oldDir}, nil
	}
	_, _, _, _ = h.opts.Runner.Run(ctx, "docker", []string{"rm", "-f", aptCont}, "")
	if err := os.RemoveAll(oldDir); err != nil {
		return nil, fmt.Errorf("清除缓存旧目录 %s 失败:%w", oldDir, err)
	}
	return []string{"已清除缓存旧目录:" + oldDir + "(缓存按需重建)"}, nil
}

// syncCertPEMs 在写 compose 前收敛 TLS 证书落盘:TLS on → 经 CertSource 进程内解密,把
// fullchain(0644)/私钥(0600)写入 certs 目录(挂载进 TLS 前端,私钥不留全局可读);证书
// 读取失败诚实报错(compose 不动,绝不让半成品证书进容器)。TLS off → 清掉 certs 目录
// (切换回明文后私钥不留盘)。基目录未知时拒绝(无处落盘,与 DeployStack 的 MkdirAll 语义一致)。
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

// ClearCache 手动清空 apt 缓存(对标 registry 缓存侧整目录清除):停删主体容器 →
// RemoveAll 生效缓存目录 → compose up -d 重建。目录本就不存在 → 不碰容器,直接报成功
// (幂等)。未启用 → ErrDisabled;无 docker/compose → ErrNoLocalDocker。
func (h *Hub) ClearCache(ctx context.Context) (*DeployResult, error) {
	cfg, err := h.Get(ctx)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, ErrDisabled
	}
	if _, err := h.composeBin(ctx); err != nil {
		return nil, err
	}
	cacheDir := h.resolveCacheDir(cfg)
	if _, serr := os.Stat(cacheDir); serr == nil {
		_, _, _, _ = h.opts.Runner.Run(ctx, "docker", []string{"rm", "-f", aptCont}, "")
		if rerr := os.RemoveAll(cacheDir); rerr != nil {
			return &DeployResult{OK: false, Error: fmt.Sprintf("清除缓存目录 %s 失败:%v", cacheDir, rerr)}, nil
		}
	}
	dep, derr := h.DeployStack(ctx)
	if derr != nil {
		return nil, derr
	}
	if dep.OK {
		dep.Output = tail("已清除缓存目录:"+cacheDir+"\n"+dep.Output, 4096)
	}
	return dep, nil
}

// Status 汇总栈当前状态(compose 文件存在性 + 容器 State + 本机代理探活 + 缓存占用)。
// 全部探测 best-effort:任何子项失败仅置 false/汇总 Error,绝不因单项失败整体报错。
func (h *Hub) Status(ctx context.Context) (*Status, error) {
	cfg, err := h.Get(ctx)
	if err != nil {
		return nil, err
	}
	st := &Status{Enabled: cfg.Enabled, Image: aptImage, TLSEnabled: cfg.TLSEnabled()}
	composePath := filepath.Join(h.opts.BaseDir, composeFile)
	if _, serr := os.Stat(composePath); serr == nil {
		st.Deployed = true
	}
	st.Running = h.containerRunning(ctx, aptCont)
	if cfg.TLSEnabled() {
		st.TLSRunning = h.containerRunning(ctx, tlsCont)
	}
	st.Reachable = pingProxy(ctx, h.probeHTTPClient(cfg), h.localProxyURL(cfg))
	st.CacheBytes = dirSize(h.resolveCacheDir(cfg))
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

// pingProxy 对本机 <base>/ 发 GET:**收到任何 HTTP 响应都算可达**(apt-cacher-ng 对根
// 路径回 403/302 等——无 ACL 配置时拒绝访问是常态,「有响应」才说明代理在服务;
// 请求失败/超时才算不可达)。TLS 栈 base 为 https 且客户端已带 InsecureSkipVerify。
func pingProxy(ctx context.Context, hc *http.Client, base string) bool {
	if hc == nil {
		hc = &http.Client{Timeout: 3 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/", nil)
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
	return true
}

// dirSize 递归统计目录字节占用(跨平台、无 CGO;缓存层状态页频率下可接受)。
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

// firstNonEmptyStr 返回首个非空白字符串(本包局部小工具)。
func firstNonEmptyStr(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// detectOutboundAddr 返回控制机的出网网卡 IP(UDP dial 不发包,仅取本地路由选出的源地址)。
func detectOutboundAddr() string {
	conn, err := net.DialTimeout("udp", "8.8.8.8:53", 2*time.Second)
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	return addr.IP.String()
}
