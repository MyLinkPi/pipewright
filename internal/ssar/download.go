// download.go — ssar 安装包的平台侧自动下载(仿 internal/mesh/easytier/download.go)。
//
//   - 升级源复用平台镜像机制(version.Source:设置库 release_mirror > env
//     PIPEWRIGHT_RELEASE_MIRROR > GitHub 官方),仅换 Repo 为自维护 fork MyLinkPi/ssar;
//   - 与 EasyTier 不同,资产是**安装包**(为目标机准备,与本机运行时无关):
//     ssar-<ver>-1.x86_64.rpm(RHEL 系)+ ssar_<ver>-1_amd64.deb(Debian 系);
//     存在哪些缓存哪些(至少一个),后续新增 aarch64 包自动纳管;
//   - 完整性:GitHub release 资产的 sha256 digest 逐字节流式校验(镜像属管理员显式
//     信任源);校验不过即弃,不留半截文件;
//   - 落位:<data>/bin/ssar/<tag>/<资产名>;版本记入 <data>/bin/ssar/version.txt;
//     Ensure 幂等;TriggerDownload 异步单飞(状态经 BinStatus 暴露)。
package ssar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/huangchengsir/pipewright/internal/version"
)

// ErrDownload 表示自动下载/校验失败(错误体不含敏感数据)。
var ErrDownload = errors.New("ssar: download failed")

// Repo 是 ssar 自维护 fork 的发布仓库(经镜像时由 Source 的 base 承载)。
const Repo = "MyLinkPi/ssar"

// VersionMarker 记录已缓存版本的标记文件名(<data>/bin/ssar/ 下)。
const VersionMarker = "version.txt"

// Downloader 是安装包下载器。零值不可用:Source 与 DataDir 必填;线程安全(内部互斥)。
type Downloader struct {
	// Source 返回本次下载所用升级源(镜像感知;每次下载前调用,改镜像即时生效)。
	Source func(ctx context.Context) version.Source
	// DataDir 是数据目录;包落 <DataDir>/bin/ssar/<tag>/。
	DataDir string
	// Client 可注入(测试);零值用 10 分钟超时客户端(慢链路大包)。
	Client *http.Client
	// pinTag 可注入钉死版本(生产经 env PIPEWRIGHT_SSAR_VERSION;测试注入)。
	pinTag string

	// 下载单飞 + 状态。
	mu          sync.Mutex
	downloading bool
	lastErr     string
	// now 可注入时钟(单测)。
	now func() time.Time
}

// releaseAsset 是 GitHub release 资产的所需字段。
type releaseAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	Digest             string `json:"digest"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type releaseInfo struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

// NewDownloader 构造下载器;pinTag 空 = 跟随最新 release(env PIPEWRIGHT_SSAR_VERSION
// 钉死具体 tag,便于回滚到旧版)。
func NewDownloader(source func(ctx context.Context) version.Source, dataDir, pinTag string) *Downloader {
	return &Downloader{Source: source, DataDir: dataDir, pinTag: pinTag, now: time.Now}
}

// BinStatus 是安装包缓存状态(机器监控页展示)。
type BinStatus struct {
	// Version 是已缓存的 release tag(空 = 尚未缓存)。
	Version string `json:"version"`
	// Packages 是已缓存的资产文件名(如 ssar-1.0.5-1.x86_64.rpm)。
	Packages    []string `json:"packages"`
	Dir         string   `json:"dir"`
	Downloading bool     `json:"downloading"`
	LastErr     string   `json:"lastErr"`
}

// BinStatus 返回当前缓存状态(无 DataDir 配置返回空状态)。
func (d *Downloader) BinStatus() BinStatus {
	if d == nil || d.DataDir == "" {
		return BinStatus{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	st := BinStatus{Downloading: d.downloading, LastErr: d.lastErr, Dir: d.binDir()}
	st.Version, st.Packages = cachedPackages(d.binDir())
	return st
}

// binDir 是缓存根(<DataDir>/bin/ssar)。
func (d *Downloader) binDir() string { return filepath.Join(d.DataDir, "bin", "ssar") }

// cachedPackages 读取缓存目录的版本标记与包文件名(目录缺失 → 空版本)。
func cachedPackages(dir string) (string, []string) {
	verB, err := os.ReadFile(filepath.Join(dir, VersionMarker))
	if err != nil {
		return "", nil
	}
	ver := strings.TrimSpace(string(verB))
	if ver == "" {
		return "", nil
	}
	entries, err := os.ReadDir(filepath.Join(dir, ver))
	if err != nil {
		return ver, nil
	}
	var pkgs []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".rpm") || strings.HasSuffix(name, ".deb") {
			pkgs = append(pkgs, name)
		}
	}
	return ver, pkgs
}

// Ensure 已有缓存(标记 + 至少一个包)则返回版本;否则执行一次同步下载。幂等。
func (d *Downloader) Ensure(ctx context.Context) (string, error) {
	if d == nil || d.Source == nil || d.DataDir == "" {
		return "", fmt.Errorf("%w: source/dataDir not configured", ErrDownload)
	}
	if ver, pkgs := cachedPackages(d.binDir()); ver != "" && len(pkgs) > 0 {
		return ver, nil
	}
	return d.Download(ctx)
}

// TriggerDownload 异步触发一次下载(单飞:进行中重复触发为 no-op)。状态经 BinStatus 轮询。
func (d *Downloader) TriggerDownload(ctx context.Context) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.downloading {
		d.mu.Unlock()
		return
	}
	d.downloading = true
	d.mu.Unlock()

	go func() {
		// 后台下载不用请求 ctx(响应返回后会被取消),独立超时兜底。
		dlCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		_, err := d.Download(dlCtx)
		d.mu.Lock()
		d.downloading = false
		if err != nil {
			d.lastErr = err.Error()
		} else {
			d.lastErr = ""
		}
		d.mu.Unlock()
	}()
}

// PackagePath 返回适用给定机器的缓存包路径(family: "rpm"|"deb";arch: "x86_64"|"aarch64"|…)。
// 未缓存 / 无匹配资产(如 aarch64 暂无)→ 明确错误。
func (d *Downloader) PackagePath(family, arch string) (string, error) {
	if d == nil {
		return "", fmt.Errorf("%w: downloader not configured", ErrDownload)
	}
	ver, pkgs := cachedPackages(d.binDir())
	if ver == "" || len(pkgs) == 0 {
		return "", fmt.Errorf("%w: 安装包尚未缓存,请先在机器监控页触发下载", ErrDownload)
	}
	var wantSuffix string
	switch family {
	case "rpm":
		wantSuffix = "." + strings.ToLower(arch) + ".rpm" // ssar-1.0.5-1.x86_64.rpm
	case "deb":
		wantSuffix = "_" + debArch(arch) + ".deb" // ssar_1.0.5-1_amd64.deb
	default:
		return "", fmt.Errorf("%w: 不支持的包家族 %q", ErrDownload, family)
	}
	for _, name := range pkgs {
		if strings.HasSuffix(strings.ToLower(name), wantSuffix) {
			return filepath.Join(d.binDir(), ver, name), nil
		}
	}
	return "", fmt.Errorf("%w: 缓存中无 %s/%s 安装包(当前:%v)", ErrDownload, family, arch, pkgs)
}

// debArch 把 uname 架构映射为 deb 架构命名(x86_64 → amd64;aarch64 → arm64)。
func debArch(arch string) string {
	switch arch {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	default:
		return strings.ToLower(arch)
	}
}

// Download 解析 release → 挑全部安装包资产 → 逐个下载校验落位。返回缓存版本 tag。
func (d *Downloader) Download(ctx context.Context) (string, error) {
	if d.Source == nil || d.DataDir == "" {
		return "", fmt.Errorf("%w: source/dataDir not configured", ErrDownload)
	}
	src := d.Source(ctx)
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}

	rel, err := d.fetchRelease(ctx, client, src)
	if err != nil {
		return "", err
	}
	assets := pickPackageAssets(rel.Assets)
	if len(assets) == 0 {
		return "", fmt.Errorf("%w: release %s 无 rpm/deb 安装包资产", ErrDownload, rel.TagName)
	}

	dest := filepath.Join(d.binDir(), rel.TagName)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", fmt.Errorf("%w: mkdir: %v", ErrDownload, err)
	}
	// 逐个资产落位;任一失败即弃(整目录作废,版本标记不写)。
	for i := range assets {
		asset := &assets[i]
		// 下载地址按镜像 base 组装,不信任 API 返回的 browser_download_url
		//(其恒指 github.com,镜像场景会绕开镜像)。
		dlURL := fmt.Sprintf("%s/%s/releases/download/%s/%s", src.DLBase, Repo, rel.TagName, asset.Name)
		if err := d.downloadVerified(ctx, client, dlURL, asset, filepath.Join(dest, asset.Name)); err != nil {
			_ = os.RemoveAll(dest)
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(d.binDir(), VersionMarker), []byte(rel.TagName), 0o644); err != nil {
		return "", fmt.Errorf("%w: write marker: %v", ErrDownload, err)
	}
	// 换版本后清掉旧版本目录(只保留当前 tag 一个,防累积)。
	entries, _ := os.ReadDir(d.binDir())
	for _, e := range entries {
		if e.IsDir() && e.Name() != rel.TagName {
			_ = os.RemoveAll(filepath.Join(d.binDir(), e.Name()))
		}
	}
	return rel.TagName, nil
}

// fetchRelease 拉指定(或最新)release 元数据。
func (d *Downloader) fetchRelease(ctx context.Context, client *http.Client, src version.Source) (*releaseInfo, error) {
	path := "releases/latest"
	if tag := strings.TrimSpace(d.pinTag); tag != "" {
		path = "releases/tags/" + tag
	}
	url := fmt.Sprintf("%s/repos/%s/%s", src.APIBase, Repo, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDownload, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDownload, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: release api %d", ErrDownload, resp.StatusCode)
	}
	var rel releaseInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("%w: release json: %v", ErrDownload, err)
	}
	if rel.TagName == "" || len(rel.Assets) == 0 {
		return nil, fmt.Errorf("%w: release has no assets", ErrDownload)
	}
	return &rel, nil
}

// pickPackageAssets 挑出安装包资产(.rpm/.deb;checksums.txt 等排除)。
func pickPackageAssets(assets []releaseAsset) []releaseAsset {
	var out []releaseAsset
	for _, a := range assets {
		lower := strings.ToLower(a.Name)
		if strings.HasSuffix(lower, ".rpm") || strings.HasSuffix(lower, ".deb") {
			out = append(out, a)
		}
	}
	return out
}

// downloadVerified 流式下载到 dest 并同时计算 sha256,与资产 digest 比对
// (digest 形如 "sha256:<hex>";缺失 digest 时拒绝 —— 校验是硬门槛,不可选)。
// 先写临时文件,校验过后再原子改名,失败不留半截文件。
func (d *Downloader) downloadVerified(ctx context.Context, client *http.Client, url string, asset *releaseAsset, dest string) error {
	want := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(asset.Digest)), "sha256:")
	if want == "" {
		return fmt.Errorf("%w: asset %s has no digest", ErrDownload, asset.Name)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDownload, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDownload, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: asset %s http %d", ErrDownload, asset.Name, resp.StatusCode)
	}

	tmp := dest + ".part"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("%w: temp file: %v", ErrDownload, err)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("%w: copy: %v", ErrDownload, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("%w: temp close: %v", ErrDownload, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		_ = os.Remove(tmp)
		return fmt.Errorf("%w: digest mismatch for %s", ErrDownload, asset.Name)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("%w: rename: %v", ErrDownload, err)
	}
	return nil
}
