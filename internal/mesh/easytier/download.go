// download.go 是 EasyTier 二进制的按需自动下载(托管模式开箱即用):
//
//   - 升级源复用平台的镜像机制(version.Source:设置库 release_mirror > env
//     PIPEWRIGHT_RELEASE_MIRROR > GitHub 官方),仅换 Repo 为 EasyTier/EasyTier —— 自建
//     镜像按 GitHub 路径布局转发即可同样生效;
//   - 资产选择:官方发布命名 easytier-<os>-<arch>-v<版本>.zip(core 与 cli 都在包内),
//     按 runtime.GOOS/GOARCH 精确匹配(GUI/仪表盘/APK 等其它资产排除);
//   - 完整性:GitHub release 资产的 sha256 digest 逐字节流式校验(镜像属管理员显式
//     信任源,与信任 GitHub 同级);校验不过即弃,不留半截文件;
//   - 落位:<data>/bin/{easytier-core,easytier-cli}(.exe),unix 置 0755;版本记入
//     <data>/bin/easytier-version.txt —— 已就位即跳过(Ensure 幂等)。
package easytier

import (
	"archive/zip"
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
	"runtime"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/version"
)

// ErrDownload 表示自动下载/校验/解包失败(错误体不含敏感数据)。
var ErrDownload = errors.New("easytier: download failed")

// easyTierRepo 是 EasyTier 官方发布仓库(经镜像时由 Source 的 base 承载)。
const easyTierRepo = "EasyTier/EasyTier"

// Downloader 是二进制下载器。零值不可用:Source 与 DataDir 必填;线程安全(内部互斥)。
type Downloader struct {
	// Source 返回本次下载所用升级源(镜像感知;每次下载前调用,改镜像即时生效)。
	Source func(ctx context.Context) version.Source
	// DataDir 是数据目录;二进制落 <DataDir>/bin。
	DataDir string
	// GOOS/GOARCH 资产匹配用;零值取 runtime(测试注入)。
	GOOS   string
	GOARCH string
	// Client 可注入(测试);零值用 5 分钟超时客户端(大包慢链路)。
	Client *http.Client
}

// BinStatus 是二进制就位状态(集群页/设置页展示)。
type BinStatus struct {
	CorePath    string `json:"corePath"`
	CliPath     string `json:"cliPath"`
	Version     string `json:"version"`     // 已安装版本(tag;空 = 未知/手工放置)
	Downloading bool   `json:"downloading"` // 自动下载进行中
	LastErr     string `json:"lastErr"`     // 最近一次下载失败原因(空 = 无)
}

// VersionMarker 记录已安装版本的标记文件名(<data>/bin/ 下)。
const VersionMarker = "easytier-version.txt"

// InstalledVersion 返回已安装版本(标记文件;无标记返回空串)。
func InstalledVersion(dataDir string) string {
	b, err := os.ReadFile(filepath.Join(dataDir, "bin", VersionMarker))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// goosName / goarchName 把 Go 目标名映射到 EasyTier 资产命名。
func (d *Downloader) goosName() string {
	osName := d.GOOS
	if osName == "" {
		osName = runtime.GOOS
	}
	if osName == "darwin" {
		return "macos"
	}
	return osName
}

func (d *Downloader) goarchName() string {
	arch := d.GOARCH
	if arch == "" {
		arch = runtime.GOARCH
	}
	switch arch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "386":
		return "i686"
	case "loong64":
		return "loongarch64"
	case "mipsle":
		return "mipsel"
	default:
		return arch // arm / riscv64 / mips 等同名
	}
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

// Ensure 已就位(easytier-core 存在)则直接返回;否则执行一次下载。幂等。
func (d *Downloader) Ensure(ctx context.Context) (string, error) {
	if Locate(os.Getenv("PIPEWRIGHT_EASYTIER_BIN"), d.DataDir, CoreBinary()) != "" {
		return InstalledVersion(d.DataDir), nil
	}
	return d.Download(ctx)
}

// Download 解析最新 release → 选资产 → 下载 + sha256 校验 → 解包落位。返回安装的版本 tag。
func (d *Downloader) Download(ctx context.Context) (string, error) {
	if d.Source == nil || d.DataDir == "" {
		return "", fmt.Errorf("%w: source/dataDir not configured", ErrDownload)
	}
	src := d.Source(ctx)
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}

	rel, err := d.fetchLatestRelease(ctx, client, src)
	if err != nil {
		return "", err
	}
	asset, err := pickAsset(rel.Assets, d.goosName(), d.goarchName())
	if err != nil {
		return "", err
	}

	// 下载地址按镜像 base 组装({DLBase}/{Repo}/releases/download/{tag}/{资产}),
	// 不信任 API 返回的 browser_download_url(其恒指 github.com,镜像场景会绕开镜像)。
	dlURL := fmt.Sprintf("%s/%s/releases/download/%s/%s", src.DLBase, easyTierRepo, rel.TagName, asset.Name)
	archivePath, err := d.downloadVerified(ctx, client, dlURL, asset)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(archivePath) }()

	if err := extractBinaries(archivePath, filepath.Join(d.DataDir, "bin")); err != nil {
		return "", err
	}
	_ = os.WriteFile(filepath.Join(d.DataDir, "bin", VersionMarker), []byte(rel.TagName), 0o644)
	return rel.TagName, nil
}

// fetchLatestRelease 拉最新 release 元数据。
func (d *Downloader) fetchLatestRelease(ctx context.Context, client *http.Client, src version.Source) (*releaseInfo, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", src.APIBase, easyTierRepo)
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

// pickAsset 按目标平台挑 CLI 资产:easytier-<os>-<arch>-v*.zip(freebsd 资产名内嵌
// OS 版本段,放宽为前缀+arch 后缀匹配);GUI/仪表盘/APK/Magisk 等一律排除。
func pickAsset(assets []releaseAsset, osName, archName string) (*releaseAsset, error) {
	suffix := "-" + archName + "-"
	var fallback *releaseAsset
	for i := range assets {
		name := assets[i].Name
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "easytier-") || !strings.HasSuffix(lower, ".zip") {
			continue
		}
		if strings.Contains(lower, "gui") || strings.Contains(lower, "dashboard") ||
			strings.Contains(lower, "magisk") || strings.HasSuffix(lower, ".apk") {
			continue
		}
		if !strings.Contains(lower, "-"+osName+"-") && !strings.HasPrefix(lower, "easytier-"+osName+"-") {
			continue
		}
		// 精确形:easytier-<os>-<arch>-v…;宽松形(freebsd-13.2-x86_64 等):os 与 arch 间夹版本段。
		if strings.HasPrefix(lower, "easytier-"+osName+"-"+archName+"-v") {
			return &assets[i], nil
		}
		if strings.HasPrefix(lower, "easytier-"+osName+"-") && strings.Contains(lower, suffix) {
			fallback = &assets[i]
		}
	}
	if fallback != nil {
		return fallback, nil
	}
	return nil, fmt.Errorf("%w: no asset for %s/%s", ErrDownload, osName, archName)
}

// downloadVerified 流式下载到临时文件并同时计算 sha256,与资产 digest 比对
// (digest 形如 "sha256:<hex>";缺失 digest 时拒绝 —— 校验是硬门槛,不可选)。
func (d *Downloader) downloadVerified(ctx context.Context, client *http.Client, url string, asset *releaseAsset) (string, error) {
	want := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(asset.Digest)), "sha256:")
	if want == "" {
		return "", fmt.Errorf("%w: asset %s has no digest", ErrDownload, asset.Name)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDownload, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDownload, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: asset %d", ErrDownload, resp.StatusCode)
	}

	tmp, err := os.CreateTemp("", "pw-easytier-*.zip")
	if err != nil {
		return "", fmt.Errorf("%w: temp file: %v", ErrDownload, err)
	}
	name := tmp.Name()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return "", fmt.Errorf("%w: copy: %v", ErrDownload, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("%w: temp close: %v", ErrDownload, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		_ = os.Remove(name)
		return "", fmt.Errorf("%w: digest mismatch for %s", ErrDownload, asset.Name)
	}
	return name, nil
}

// extractBinaries 从 zip 里解出 easytier-core / easytier-cli(任意目录层级;Windows
// 资产带 .exe 后缀,按去后缀基名匹配)落位 destDir,unix 置可执行位。
func extractBinaries(archivePath, destDir string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("%w: open zip: %v", ErrDownload, err)
	}
	defer func() { _ = zr.Close() }()

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("%w: mkdir: %v", ErrDownload, err)
	}
	found := map[string]bool{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		base := strings.TrimSuffix(filepath.Base(f.Name), ".exe")
		if base != "easytier-core" && base != "easytier-cli" {
			continue
		}
		src, err := f.Open()
		if err != nil {
			return fmt.Errorf("%w: zip entry: %v", ErrDownload, err)
		}
		dest := filepath.Join(destDir, filepath.Base(f.Name))
		dst, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
		if err != nil {
			_ = src.Close()
			return fmt.Errorf("%w: write binary: %v", ErrDownload, err)
		}
		_, cerr := io.Copy(dst, src)
		_ = src.Close()
		_ = dst.Close()
		if cerr != nil {
			return fmt.Errorf("%w: copy binary: %v", ErrDownload, cerr)
		}
		found[base] = true
	}
	if !found["easytier-core"] || !found["easytier-cli"] {
		return fmt.Errorf("%w: archive misses core/cli (found %v)", ErrDownload, found)
	}
	return nil
}
