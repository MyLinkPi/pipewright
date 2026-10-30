// upgrade.go 是客户端自升级的平台无关核心(可单测):检查最新发布、按候选源
// (GitHub 官方 → gh-proxy 公共代理兜底)下载客户端归档(Windows 为 .zip,Linux/macOS
// 为 .tar.gz)、以 checksums.txt 做 sha256 硬校验、安全解包到临时目录,以及把已解包
// 文件落位到安装目录(旧 exe 改名让位、新 exe 原子替换、随包 bin/licenses 同步)。
//
// Windows 特有的服务停启与 UAC 提权由 cmd/pwclient 侧编排;非 Windows 侧由 systemd/
// launchd 编排;本文件不碰服务管理器。
package pwclient

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/version"
)

// ErrUpgrade 表示检查/下载/校验/解包/落位失败(错误体不含敏感数据)。
var ErrUpgrade = errors.New("pwclient: upgrade failed")

// ErrPartialUpgrade 标记「主程序已落位成功、仅附属文件(bin/licenses)同步失败」的非致命
// 失败:调用方据此报成功 + 警示,而非回滚/报 error(回滚已不可能,exe 已替换)。
var ErrPartialUpgrade = errors.New("pwclient: partial upgrade")

// TagPattern 是版本 tag 的白名单格式(vX.Y.Z,可选 -prerelease/+build 后缀)。升级
// tag 会拼入提权命令行与下载 URL 路径,CLI 与界面入口都必须先校验(拒绝空白/
// 引号/路径分隔符,防命令注入与请求路径改写)。
var TagPattern = regexp.MustCompile(`^v?\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.+-]+)?$`)

// ValidateTag 校验升级目标版本 tag(空 = 最新版,合法)。非法时返回可读错误。
func ValidateTag(tag string) error {
	tag = strings.TrimSpace(tag)
	if tag == "" || TagPattern.MatchString(tag) {
		return nil
	}
	return fmt.Errorf("非法版本号 %q(应为 vX.Y.Z 格式)", tag)
}

// SkipUpgradeDecision 是「是否跳过升级」的纯函数判定(CLI 与单测共用,平台无关)。
// 比较基准恒为 installVer(安装目标 exe 的版本,读不到时调用方传当前进程版本)——
// 从新版解压目录运行升级器时当前进程版本(self)与安装目标不同,以 self 判定会
// 误报「已是最新」而跳过真实更新。latest 为升级源最新 tag(空 = 未知)。
// 返回 skip=true 时 reason 为给用户的原因说明。
func SkipUpgradeDecision(installVer, latest, tag string, force bool) (skip bool, reason string) {
	if force {
		return false, ""
	}
	target := strings.TrimSpace(tag)
	if target == "" {
		target = strings.TrimSpace(latest)
	}
	if target == "" {
		return true, "升级源暂无发布版本"
	}
	if !version.IsComparable(installVer) {
		if tag == "" {
			return true, fmt.Sprintf("当前为开发构建(%s),无法比较版本;请用 --tag <版本> 或 --force 升级到最新(%s)", installVer, target)
		}
		return false, "" // 显式指定版本:照办(装同版本由语义比较拦截)
	}
	switch cmp := version.CompareVersions(target, installVer); {
	case cmp == 0:
		return true, fmt.Sprintf("当前已是 %s;如需重装请加 --force。", target)
	case cmp < 0:
		if tag == "" {
			return true, fmt.Sprintf("已是最新(%s)。", installVer)
		}
		return true, fmt.Sprintf("当前已是 %s,不低于目标 %s;如需降级重装请加 --force。", installVer, target)
	}
	return false, ""
}

// 升级进度阶段(写入 upgrade-status.json 供托盘 GUI 展示)。
const (
	UpgradePhaseDownloading = "downloading" // 正在下载升级包
	UpgradePhaseApplying    = "applying"    // 正在替换 exe / 同步 bin(服务已停)
	UpgradePhaseRestarting  = "restarting"  // 正在重启服务
	UpgradePhaseDone        = "done"        // 升级成功
	UpgradePhaseSkipped     = "skipped"     // 无需升级终态(已是目标版本/开发构建无法比较)
	UpgradePhaseError       = "error"       // 升级失败
)

// upgradeStatusFile 是升级进度落盘文件名(<dataDir>/upgrade-status.json)。
const upgradeStatusFile = "upgrade-status.json"

// Upgrader 是客户端升级器。零值需至少设置 Sources;建议用 NewUpgrader。
// 线程安全:每次 Check/Stage 只读取自身字段,不做可变缓存(缓存由 Checker 内部负责)。
type Upgrader struct {
	// Checker 做「查最新发布」;零值用 version.NewChecker()。
	Checker *version.Checker
	// Sources 返回按序尝试的升级源(GitHub 官方 → gh-proxy 兜底);零值同 NewUpgrader。
	Sources func(context.Context) []version.Source
	// GOOS/GOARCH 是资产匹配目标;零值取 runtime(测试注入)。
	GOOS   string
	GOARCH string
	// Client 是下载客户端;零值用 10 分钟超时客户端(大包慢链路)。
	Client *http.Client
	// ChecksumURLFn 返回 checksums.txt 的异源参照地址(默认 GitHub 官方;
	// 测试注入 httptest 地址避免真实网络)。nil 用官方拼接。
	ChecksumURLFn func(ctx context.Context, repo, tag string) string
}

// NewUpgrader 返回默认升级器:GitHub 官方 + gh-proxy 自动兜底,当前平台资产。
func NewUpgrader() *Upgrader {
	return &Upgrader{
		Checker: version.NewChecker(),
		Sources: func(context.Context) []version.Source { return version.CandidateSources("") },
		GOOS:    runtime.GOOS,
		GOARCH:  runtime.GOARCH,
		Client:  &http.Client{Timeout: 10 * time.Minute},
	}
}

func (u *Upgrader) sources(ctx context.Context) []version.Source {
	if u.Sources != nil {
		return u.Sources(ctx)
	}
	return version.CandidateSources("")
}

func (u *Upgrader) goos() string {
	if u.GOOS != "" {
		return u.GOOS
	}
	return runtime.GOOS
}

func (u *Upgrader) goarch() string {
	if u.GOARCH != "" {
		return u.GOARCH
	}
	return runtime.GOARCH
}

func (u *Upgrader) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

// Check 查最新发布:按候选源顺序尝试,首个成功(或「源上暂无 release」)的结果即返回。
// includeUnstable=true 时把预发布(beta/rc)纳入「最新版本」口径(改走 releases 列表端点)。
func (u *Upgrader) Check(ctx context.Context, includeUnstable bool) version.UpdateInfo {
	chk := u.Checker
	if chk == nil {
		chk = version.NewChecker()
	}
	return chk.CheckAny(ctx, u.sources(ctx), includeUnstable)
}

// clientAssetName 是 goreleaser 产出的客户端归档名(与 .goreleaser.yaml 的
// `pwclient_{{ .Version }}_{{ .Os }}_{{ .Arch }}` 对齐;tag 去掉前导 v)。扩展名按平台:
// Windows 为 .zip,Linux/macOS 为 .tar.gz(与归档 format_overrides 一致)。
func clientAssetName(tag, goos, goarch string) string {
	return fmt.Sprintf("pwclient_%s_%s_%s%s", strings.TrimPrefix(strings.TrimSpace(tag), "v"), goos, goarch, clientArchiveExt(goos))
}

// clientArchiveExt 返回指定平台的归档扩展名(含点)。Windows 用 zip,其余用 tar.gz。
func clientArchiveExt(goos string) string {
	if goos == "windows" {
		return ".zip"
	}
	return ".tar.gz"
}

// Stage 下载并解包指定 tag 的客户端归档到临时目录,返回该目录(调用方负责 RemoveAll)。
// 按候选源顺序逐个尝试:每个源独立走完「checksums → 下载 → 校验 → 解包」,成功即返回;
// 全部失败返回最后一个错误。checksums.txt 缺失/不匹配一律视为失败(校验是硬门槛)。
// 校验和优先从官方源取(与候选下载源异源,镜像同时替换 zip 与 checksums 也无法绕过
// 真实性校验),取不到才退回候选源自带清单(仍硬校验 sha256,降为完整性保障并日志警示)。
func (u *Upgrader) Stage(ctx context.Context, tag string) (string, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return "", fmt.Errorf("%w: 目标版本为空", ErrUpgrade)
	}
	if err := ValidateTag(tag); err != nil {
		return "", fmt.Errorf("%w: %v", ErrUpgrade, err)
	}
	srcs := u.sources(ctx)
	if len(srcs) == 0 {
		return "", fmt.Errorf("%w: 未配置升级源", ErrUpgrade)
	}
	asset := clientAssetName(tag, u.goos(), u.goarch())
	client := u.client()

	want, checksumOK := u.resolveChecksum(ctx, client, tag, asset, srcs)

	var lastErr error
	for _, src := range srcs {
		if !checksumOK {
			// 官方清单不可达:退回该源自带的 checksums.txt(同源性风险已由日志警示)。
			var cerr error
			if want, cerr = fetchChecksum(ctx, client, downloadBase(src)+"/"+src.Repo+"/releases/download/"+tag+"/checksums.txt", asset); cerr != nil {
				lastErr = cerr
				log.Printf("[pwclient] 升级包下载失败(源 %s):%v,尝试下一个源", src.APIBase, cerr)
				continue
			}
		}
		dir, err := u.stageFromSource(ctx, client, src, tag, asset, want)
		if err == nil {
			log.Printf("[pwclient] 升级包下载成功(源 %s,版本 %s,校验和来源 %s)", src.APIBase, tag,
				map[bool]string{true: "官方源", false: "同源回退"}[checksumOK])
			return dir, nil
		}
		lastErr = err
		log.Printf("[pwclient] 升级包下载失败(源 %s):%v,尝试下一个源", src.APIBase, err)
	}
	return "", fmt.Errorf("%w: 已尝试 %d 个升级源,最后错误:%v", ErrUpgrade, len(srcs), lastErr)
}

// resolveChecksum 优先从官方 GitHub 取 checksums.txt 中 asset 的 sha256。
// 未配置镜像(候选首源即官方)时不重复请求;显式配置镜像/公共代理兜底时仍先试
// 官方(异源校验),官方不可达则返回 ok=false,由调用方回退到候选源自带清单并日志警示。
func (u *Upgrader) resolveChecksum(ctx context.Context, client *http.Client, tag, asset string, srcs []version.Source) (string, bool) {
	if downloadBase(srcs[0]) == officialDownloadBase {
		// 首选源已是官方:异源无意义,直接用它(命中典型路径,不重复请求)。
		want, err := fetchChecksum(ctx, client, u.checksumURL(ctx, srcs[0].Repo, tag), asset)
		return want, err == nil
	}
	want, ferr := fetchChecksum(ctx, client, u.checksumURL(ctx, srcs[0].Repo, tag), asset)
	if ferr == nil {
		return want, true
	}
	log.Printf("[pwclient] 官方校验和清单不可达(%v),回退使用下载源自带清单(仅完整性保障)", ferr)
	return "", false
}

// officialDownloadBase 是 GitHub 官方下载 base(异源校验和的参照)。
const officialDownloadBase = "https://github.com"

// checksumURL 取校验和清单参照地址(注入优先)。
func (u *Upgrader) checksumURL(ctx context.Context, repo, tag string) string {
	if u.ChecksumURLFn != nil {
		return u.ChecksumURLFn(ctx, repo, tag)
	}
	return officialDownloadBase + "/" + repo + "/releases/download/" + tag + "/checksums.txt"
}

// downloadBase 取源的下载 base(去尾斜杠;空 = 未配置下载域,回落官方)。
func downloadBase(src version.Source) string {
	if base := strings.TrimRight(src.DLBase, "/"); base != "" {
		return base
	}
	return officialDownloadBase
}

// stageFromSource 从单个源完成一次下载+校验+解包(want 为预期 sha256)。
func (u *Upgrader) stageFromSource(ctx context.Context, client *http.Client, src version.Source,
	tag, asset, want string) (string, error) {
	base := strings.TrimRight(src.DLBase, "/")
	if base == "" {
		return "", fmt.Errorf("%w: 升级源缺少下载地址", ErrUpgrade)
	}
	relBase := fmt.Sprintf("%s/%s/releases/download/%s", base, src.Repo, tag)

	archivePath, err := downloadToTemp(ctx, client, relBase+"/"+asset, want, clientArchiveExt(u.goos()))
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(archivePath) }()

	stageDir, err := os.MkdirTemp("", "pwclient-upgrade-*")
	if err != nil {
		return "", fmt.Errorf("%w: 创建解包目录失败:%v", ErrUpgrade, err)
	}
	if err := extractClientArchive(archivePath, stageDir, u.rootBinaryName()); err != nil {
		_ = os.RemoveAll(stageDir)
		return "", err
	}
	return stageDir, nil
}

// rootBinaryName 返回归档根目录中预期的主程序名(Windows 带 .exe 后缀)。
func (u *Upgrader) rootBinaryName() string {
	if u.goos() == "windows" {
		return "pwclient.exe"
	}
	return "pwclient"
}

// fetchChecksum 从 checksums.txt 取指定资产的 sha256(格式 `<sha256>  <文件名>`。
func fetchChecksum(ctx context.Context, client *http.Client, url, asset string) (string, error) {
	var buf strings.Builder
	if err := downloadTo(ctx, client, url, &buf); err != nil {
		return "", fmt.Errorf("%w: 下载校验和失败:%v", ErrUpgrade, err)
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == asset {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("%w: 校验和清单中无 %s", ErrUpgrade, asset)
}

// downloadToTemp 下载 url 到系统临时文件,流式算 sha256 并与 want 比对(不匹配即删并报错)。
// ext 为归档扩展名(带点,如 .zip / .tar.gz),仅用于临时文件命名,便于后续解包分发。
func downloadToTemp(ctx context.Context, client *http.Client, url, want, ext string) (string, error) {
	if ext == "" {
		ext = ".zip"
	}
	tmp, err := os.CreateTemp("", "pwclient-upgrade-*"+ext)
	if err != nil {
		return "", fmt.Errorf("%w: 创建临时文件失败:%v", ErrUpgrade, err)
	}
	name := tmp.Name()
	h := sha256.New()
	derr := downloadTo(ctx, client, url, io.MultiWriter(tmp, h))
	if cerr := tmp.Close(); derr == nil && cerr != nil {
		derr = fmt.Errorf("写入临时文件失败:%v", cerr)
	}
	if derr != nil {
		_ = os.Remove(name)
		return "", derr
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		_ = os.Remove(name)
		return "", fmt.Errorf("%w: 校验和不匹配(期望 %s,实际 %s)", ErrUpgrade, want, got)
	}
	return name, nil
}

// downloadTo 把 url 内容写入 w(5 分钟超时 + 状态码校验;客户端自身可带更长超时)。
func downloadTo(ctx context.Context, client *http.Client, url string, w io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("%w: 构造请求失败:%v", ErrUpgrade, err)
	}
	req.Header.Set("User-Agent", "pwclient/"+version.Version)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: 下载失败:%v", ErrUpgrade, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: 下载返回 %d: %s", ErrUpgrade, resp.StatusCode, url)
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("%w: 写入失败:%v", ErrUpgrade, err)
	}
	return nil
}

// extractClientArchive 按扩展名分发解包:.tar.gz/.tgz 走 tar.gz,其余走 zip。
// rootBinary 为归档根目录必须包含的主程序名(Windows "pwclient.exe",其余 "pwclient")。
func extractClientArchive(archivePath, destDir, rootBinary string) error {
	lower := strings.ToLower(archivePath)
	if strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz") {
		return extractClientTarGz(archivePath, destDir, rootBinary)
	}
	return extractClientZip(archivePath, destDir, rootBinary)
}

// extractClientZip 把客户端 zip 归档安全解包到 destDir:拒绝绝对路径/路径穿越/符号链接,
// 并要求解出根级主程序(Windows 文件名大小写不敏感)。
func extractClientZip(zipPath, destDir, rootBinary string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("%w: 打开升级包失败:%v", ErrUpgrade, err)
	}
	defer func() { _ = zr.Close() }()

	foundExe := false
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if f.FileInfo().Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: 升级包含符号链接 %q(拒绝)", ErrUpgrade, f.Name)
		}
		rel, dest, verr := sanitizeEntryPath(destDir, f.Name)
		if verr != nil {
			return verr
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("%w: 创建目录失败:%v", ErrUpgrade, err)
		}
		src, err := f.Open()
		if err != nil {
			return fmt.Errorf("%w: 读取归档条目失败:%v", ErrUpgrade, err)
		}
		// 单文件上限 400MB:防恶意超大归档(实际 exe 约 40-60MB)。
		werr := writeFileLimited(dest, src, 400<<20)
		_ = src.Close()
		if werr != nil {
			return werr
		}
		if strings.EqualFold(filepath.Base(rel), rootBinary) && !strings.Contains(strings.TrimPrefix(rel, "./"), "/") {
			foundExe = true
		}
	}
	if !foundExe {
		return fmt.Errorf("%w: 升级包中未找到根级 %s", ErrUpgrade, rootBinary)
	}
	return nil
}

// extractClientTarGz 把 Linux/macOS 的 tar.gz 客户端归档安全解包到 destDir:同样拒绝
// 绝对路径/路径穿越/符号链接(与 zip 一致的安全边界),要求根级主程序,并沿用归档内
// 文件权限位(主程序与 bin/ 下的 easytier 二进制需保留可执行位)。
func extractClientTarGz(tzPath, destDir, rootBinary string) error {
	fh, err := os.Open(tzPath)
	if err != nil {
		return fmt.Errorf("%w: 打开升级包失败:%v", ErrUpgrade, err)
	}
	defer func() { _ = fh.Close() }()
	gz, err := gzip.NewReader(fh)
	if err != nil {
		return fmt.Errorf("%w: 解压 gzip 失败:%v", ErrUpgrade, err)
	}
	defer func() { _ = gz.Close() }()

	foundExe := false
	tr := tar.NewReader(gz)
	for {
		hdr, rerr := tr.Next()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return fmt.Errorf("%w: 读取 tar 条目失败:%v", ErrUpgrade, rerr)
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		// 符号/硬链接、设备文件一律拒绝(与 zip 的 symlink 拒绝策略对齐)。
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("%w: 升级包含非普通文件 %q(类型 %c,拒绝)", ErrUpgrade, hdr.Name, hdr.Typeflag)
		}
		rel, dest, verr := sanitizeEntryPath(destDir, hdr.Name)
		if verr != nil {
			return verr
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("%w: 创建目录失败:%v", ErrUpgrade, err)
		}
		// 权限取归档 mode 的可执行位;缺失(0)时按 0755 落主程序/bin。
		mode := os.FileMode(hdr.Mode & 0o777)
		if mode == 0 {
			mode = 0o755
		}
		if werr := writeFileMode(dest, tr, 400<<20, mode); werr != nil {
			return werr
		}
		if filepath.Base(rel) == rootBinary && !strings.Contains(rel, "/") {
			foundExe = true
		}
	}
	if !foundExe {
		return fmt.Errorf("%w: 升级包中未找到根级 %s", ErrUpgrade, rootBinary)
	}
	return nil
}

// sanitizeEntryPath 归一归档条目名(统一 / 分隔)并校验不越出 destDir;返回清洗后的
// 相对路径与目标绝对路径。绝对路径/穿越/盘符一律拒绝。
func sanitizeEntryPath(destDir, name string) (rel, dest string, err error) {
	rel = path.Clean(strings.ReplaceAll(name, `\`, "/"))
	rel = strings.TrimPrefix(rel, "./")
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || path.IsAbs(rel) ||
		strings.Contains(rel, ":") {
		return "", "", fmt.Errorf("%w: 升级包含非法路径 %q", ErrUpgrade, name)
	}
	dest = filepath.Join(destDir, filepath.FromSlash(rel))
	if back, rerr := filepath.Rel(destDir, dest); rerr != nil || back == ".." ||
		strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("%w: 升级包含越界路径 %q", ErrUpgrade, name)
	}
	return rel, dest, nil
}

// writeFileMode 把 r 写入 dst(上限 limit 字节,指定权限;读超上限即报错)。
func writeFileMode(dst string, r io.Reader, limit int64, mode os.FileMode) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("%w: 写入 %s 失败:%v", ErrUpgrade, dst, err)
	}
	n, cerr := io.Copy(out, io.LimitReader(r, limit+1))
	if cerr == nil && n > limit {
		cerr = fmt.Errorf("文件超过大小上限 %d", limit)
	}
	if cerr == nil {
		cerr = out.Close()
	} else {
		_ = out.Close()
	}
	if cerr != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("%w: 写入 %s 失败:%v", ErrUpgrade, dst, cerr)
	}
	// umask 可能吃掉权限位,显式 Chmod 兜底(尤其可执行位)。
	if cherr := os.Chmod(dst, mode); cherr != nil {
		return fmt.Errorf("%w: 设置 %s 权限失败:%v", ErrUpgrade, dst, cherr)
	}
	return nil
}

// writeFileLimited 把 r 写入 dst(上限 limit 字节,可执行权限 0755)。
func writeFileLimited(dst string, r io.Reader, limit int64) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("%w: 写入 %s 失败:%v", ErrUpgrade, dst, err)
	}
	n, cerr := io.Copy(out, io.LimitReader(r, limit+1))
	if cerr == nil && n > limit {
		cerr = fmt.Errorf("文件超过大小上限 %d", limit)
	}
	if cerr == nil {
		cerr = out.Close()
	} else {
		_ = out.Close()
	}
	if cerr != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("%w: 写入 %s 失败:%v", ErrUpgrade, dst, cerr)
	}
	return nil
}

// ApplyStaged 把解包目录落位到安装目录:旧 exe 改名让位 → 新 exe 原子替换 →
// 同步 bin/(随包 EasyTier)与 licenses/。返回旧 exe 路径(供调用方稍后清理;
// 运行中的托盘仍占用时删除会失败,留到下次启动由 CleanupStaleExecutables 处理)。
func ApplyStaged(stageDir, installDir, exeName string) (oldExe string, err error) {
	newExe := filepath.Join(stageDir, exeName)
	if _, serr := os.Stat(newExe); serr != nil {
		return "", fmt.Errorf("%w: 升级包缺少 %s", ErrUpgrade, exeName)
	}
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return "", fmt.Errorf("%w: 创建安装目录失败:%v", ErrUpgrade, err)
	}
	target := filepath.Join(installDir, exeName)
	ts := strconv.FormatInt(time.Now().UnixNano(), 10)

	if _, serr := os.Stat(target); serr == nil {
		old := target + ".old-" + ts
		if rerr := os.Rename(target, old); rerr != nil {
			return "", fmt.Errorf("%w: 无法替换 %s(程序被占用或权限不足,请关闭托盘客户端并以管理员运行):%v",
				ErrUpgrade, target, rerr)
		}
		oldExe = old
	}

	restore := func() {
		if oldExe != "" {
			_ = os.Remove(target)
			_ = os.Rename(oldExe, target)
		}
	}

	tmpExe := target + ".new-" + ts
	if cerr := copyFile(newExe, tmpExe); cerr != nil {
		_ = os.Remove(tmpExe)
		restore()
		return "", fmt.Errorf("%w: 写入新程序失败:%v", ErrUpgrade, cerr)
	}
	if cerr := os.Chmod(tmpExe, 0o755); cerr != nil {
		_ = os.Remove(tmpExe)
		restore()
		return "", fmt.Errorf("%w: 设置可执行权限失败:%v", ErrUpgrade, cerr)
	}
	if rerr := os.Rename(tmpExe, target); rerr != nil {
		_ = os.Remove(tmpExe)
		restore()
		return "", fmt.Errorf("%w: 替换程序失败:%v", ErrUpgrade, rerr)
	}

	auxWarn := func(err error) (string, error) {
		if oldExe != "" {
			_ = os.Remove(oldExe) // 已落位新版,旧镜像不再有用
		}
		return "", fmt.Errorf("%w: 主程序已替换成功,但附属文件同步失败:%v", ErrPartialUpgrade, err)
	}
	if berr := CopyStagedBinaries(stageDir, filepath.Join(installDir, "bin")); berr != nil {
		return auxWarn(berr)
	}
	if src := filepath.Join(stageDir, "licenses"); dirExists(src) {
		if lerr := copyTree(src, filepath.Join(installDir, "licenses")); lerr != nil {
			return auxWarn(lerr)
		}
	}
	if src := filepath.Join(stageDir, "LICENSE"); fileExists(src) {
		if cerr := copyFile(src, filepath.Join(installDir, "LICENSE")); cerr != nil {
			return auxWarn(fmt.Errorf("同步 LICENSE 失败:%v", cerr))
		}
	}
	return oldExe, nil
}

// CopyStagedBinaries 把解包目录的 bin/* 同步到指定 bin 目录(逐文件临时名 + rename,
// 避免半截文件)。目标 bin 目录不存在则跳过(调用方用其刷新 <dataDir>/bin 这类历史落点)。
func CopyStagedBinaries(stageDir, destBinDir string) error {
	srcDir := filepath.Join(stageDir, "bin")
	entries, err := os.ReadDir(srcDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: 读取升级包 bin 失败:%v", ErrUpgrade, err)
	}
	if err := os.MkdirAll(destBinDir, 0o755); err != nil {
		return fmt.Errorf("%w: 创建 bin 目录失败:%v", ErrUpgrade, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		src := filepath.Join(srcDir, e.Name())
		dst := filepath.Join(destBinDir, e.Name())
		tmp := dst + ".new-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		if cerr := copyFile(src, tmp); cerr != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("%w: 复制 %s 失败:%v", ErrUpgrade, e.Name(), cerr)
		}
		_ = os.Chmod(tmp, 0o755)
		if rerr := os.Rename(tmp, dst); rerr != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("%w: 替换 %s 失败(文件被占用?):%v", ErrUpgrade, dst, rerr)
		}
	}
	return nil
}

// CleanupStaleExecutables 尽力删除升级留下的 <exe>.old-* / <exe>.new-*。
// 失败忽略:运行中的托盘/服务仍占用旧镜像时,留到下次启动再删。
func CleanupStaleExecutables(installDir, exeName string) {
	entries, err := os.ReadDir(installDir)
	if err != nil || exeName == "" {
		return
	}
	lower := strings.ToLower(exeName)
	for _, e := range entries {
		n := strings.ToLower(e.Name())
		if strings.HasPrefix(n, lower+".old-") || strings.HasPrefix(n, lower+".new-") {
			_ = os.Remove(filepath.Join(installDir, e.Name()))
		}
	}
}

// UpgradeStatus 是跨进程(提权升级进程 → 托盘 GUI)的升级进度/结果。
type UpgradeStatus struct {
	Phase   string `json:"phase"`   // downloading / applying / restarting / done / error
	Current string `json:"current"` // 升级前版本
	Target  string `json:"target"`  // 目标版本 tag
	Message string `json:"message"` // 人读进度说明
	Error   string `json:"error,omitempty"`
	Time    string `json:"time"` // RFC3339
}

// UpgradeStatusPath 返回升级状态文件路径。
func UpgradeStatusPath(dataDir string) string { return filepath.Join(dataDir, upgradeStatusFile) }

// WriteUpgradeStatus 原子写入升级状态(临时文件 + rename;0644 允许托盘用户读取)。
func WriteUpgradeStatus(dataDir string, st UpgradeStatus) error {
	st.Time = time.Now().Format(time.RFC3339)
	blob, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("%w: 序列化升级状态失败:%v", ErrUpgrade, err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("%w: 创建数据目录失败:%v", ErrUpgrade, err)
	}
	target := UpgradeStatusPath(dataDir)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o644); err != nil {
		return fmt.Errorf("%w: 写入升级状态失败:%v", ErrUpgrade, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("%w: 落盘升级状态失败:%v", ErrUpgrade, err)
	}
	return nil
}

// ReadUpgradeStatus 读取升级状态(文件不存在返回 os.ErrNotExist 包装的错误)。
func ReadUpgradeStatus(dataDir string) (UpgradeStatus, error) {
	var st UpgradeStatus
	blob, err := os.ReadFile(UpgradeStatusPath(dataDir))
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(blob, &st); err != nil {
		return st, fmt.Errorf("%w: 解析升级状态失败:%v", ErrUpgrade, err)
	}
	return st, nil
}

// copyTree 递归复制目录(目录不存在视为成功;只处理普通文件)。
func copyTree(src, dst string) error {
	entries, err := os.ReadDir(src)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: 读取 %s 失败:%v", ErrUpgrade, src, err)
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyTree(s, d); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(d), 0o755); err != nil {
			return fmt.Errorf("%w: 创建目录失败:%v", ErrUpgrade, err)
		}
		if err := copyFile(s, d); err != nil {
			return fmt.Errorf("%w: 复制 %s 失败:%v", ErrUpgrade, s, err)
		}
	}
	return nil
}

// copyFile 覆盖复制单个文件(目标先截断;权限沿用源文件)。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// ParseServiceBinaryPath 从 Windows 服务配置的 BinaryPathName 里取 exe 路径:
// 兼容 `"C:\...\pwclient.exe" service run`(带引号+参数)与裸路径(可带参数)。
// 空串/无法识别返回空串。
func ParseServiceBinaryPath(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if strings.HasPrefix(name, `"`) {
		rest := name[1:]
		if i := strings.IndexByte(rest, '"'); i >= 0 {
			return rest[:i]
		}
		return rest
	}
	// 未加引号但含空格:以 .exe 结尾处截断最稳(Windows 路径里不会出现 .exe 两次)。
	if i := strings.Index(strings.ToLower(name), ".exe"); i >= 0 {
		return name[:i+len(".exe")]
	}
	if f := strings.Fields(name); len(f) > 0 {
		return f[0]
	}
	return ""
}
