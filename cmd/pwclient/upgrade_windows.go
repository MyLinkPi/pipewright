//go:build windows

// upgrade_windows.go 是 `pwclient upgrade` 子命令:检查最新发布 → 下载 + sha256 校验 +
// 解包 → 停服务 → 旧 exe 改名让位并落位新版(含随包 EasyTier bin/ 与 licenses)→ 重启服务。
//
// 托盘经 UAC(runas)调用独立的升级进程执行;进度写 <dataDir>/upgrade-status.json 供 GUI 展示。
// 升级源默认 GitHub 官方 → gh-proxy 公共代理自动兜底;显式配置镜像则只走镜像。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/huangchengsir/pipewright/internal/pwclient"
	"github.com/huangchengsir/pipewright/internal/version"
)

// upgradeOptions 是 `pwclient upgrade` 的解析结果。
type upgradeOptions struct {
	CheckOnly bool
	Tag       string
	Force     bool
}

// runUpgrade 处理 `pwclient upgrade [--check] [--tag vX.Y.Z] [--force]`。
func runUpgrade(args []string) {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	checkOnly := fs.Bool("check", false, "只检查最新版本,不下载/不改动文件(无需管理员)")
	tag := fs.String("tag", "", "升级到指定版本 tag(如 v1.2.3;缺省 = 最新)")
	force := fs.Bool("force", false, "忽略版本比较强制重装/升级")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if err := doUpgrade(upgradeOptions{CheckOnly: *checkOnly, Tag: *tag, Force: *force}); err != nil {
		fmt.Fprintln(os.Stderr, "升级失败:", err)
		os.Exit(1)
	}
}

// doUpgrade 执行检查/下载/替换/重启全流程。
func doUpgrade(opts upgradeOptions) error {
	ctx := context.Background()
	dir := dataDir()
	current := version.Version
	up := pwclient.NewUpgrader()

	info := up.Check(ctx)
	if opts.CheckOnly {
		printUpdateInfo(info)
		if info.CheckError != "" {
			return errors.New(info.CheckError)
		}
		return nil
	}

	// 升级落点先行:版本比较/状态展示都以「安装目标」的现状为准 —— 从新版
	// 解压目录运行升级而服务仍是旧版时,若拿当前进程版本(self)比较会误判
	// 「已是目标版本」而跳过真实更新。
	installDir, exeName, svcInstalled, svcRunning, err := upgradeTarget()
	if err != nil {
		return err
	}
	if svcInstalled && !isElevated() {
		return fmt.Errorf("升级已安装的服务需要管理员权限:请以管理员身份运行 `pwclient upgrade`,或从托盘点「升级」(会弹 UAC)")
	}
	targetVer := current
	if svcInstalled {
		targetExe := filepath.Join(installDir, exeName)
		if v, verr := exeFileVersion(targetExe); verr == nil && v != "" {
			targetVer = v
		} else {
			log.Printf("[pwclient] 读取目标安装版本失败(%s),退回当前进程版本比较:%v", targetExe, verr)
		}
	}

	target := strings.TrimSpace(opts.Tag)
	if target != "" {
		if err := pwclient.ValidateTag(target); err != nil {
			return err
		}
	}
	if target == "" {
		target = info.Latest
	}
	if !opts.Force {
		if skip, reason := pwclient.SkipUpgradeDecision(targetVer, info.Latest, opts.Tag, opts.Force); skip {
			if target == "" && info.CheckError != "" {
				reason = fmt.Sprintf("检查更新失败:%s", info.CheckError)
			}
			return skipUpgrade(dir, targetVer, target, reason)
		}
	}

	report := func(phase, msg string) {
		_ = pwclient.WriteUpgradeStatus(dir, pwclient.UpgradeStatus{
			Phase: phase, Current: targetVer, Target: target, Message: msg,
		})
	}
	fail := func(msg string, cause error) error {
		_ = pwclient.WriteUpgradeStatus(dir, pwclient.UpgradeStatus{
			Phase: pwclient.UpgradePhaseError, Current: targetVer, Target: target,
			Message: msg, Error: cause.Error(),
		})
		return cause
	}

	fmt.Printf("当前版本 %s,准备升级到 %s(安装目录 %s)…\n", current, target, installDir)
	report(pwclient.UpgradePhaseDownloading, "正在下载升级包…")
	stageDir, err := up.Stage(ctx, target)
	if err != nil {
		return fail("下载升级包失败", err)
	}
	defer func() { _ = os.RemoveAll(stageDir) }()

	report(pwclient.UpgradePhaseApplying, "正在停止服务并替换程序文件…")
	stopped := false
	if svcInstalled {
		if err := stopServiceAndWait(30 * time.Second); err != nil {
			return fail("停止服务失败", err)
		}
		stopped = true
	}
	// 失败时尽力把服务恢复为升级前的运行状态(程序文件回位由 ApplyStaged 负责)。
	restoreService := func() {
		if stopped && svcRunning {
			if serr := startService(); serr != nil {
				log.Printf("[pwclient] 升级失败后恢复服务启动失败:%v", serr)
			}
		}
	}

	// partial 标记「主程序已落位成功、仅附属文件同步失败」:不报失败/不回滚,
	// 后续照常重启服务,终态 done + 警示(避免界面误报失败而实际已升级)。
	partial := false
	oldExe, err := pwclient.ApplyStaged(stageDir, installDir, exeName)
	if err != nil {
		if !errors.Is(err, pwclient.ErrPartialUpgrade) {
			restoreService()
			return fail("替换程序文件失败", err)
		}
		partial = true
		log.Printf("[pwclient] 升级部分完成:%v", err)
	}
	// 刷新历史自动下载落点 <dataDir>/bin:EasyTier 查找优先级 dataDir > exe 同级 bin,
	// 不刷新会继续跑旧二进制。
	if !partial {
		if st, serr := os.Stat(filepath.Join(dir, "bin")); serr == nil && st.IsDir() {
			if cerr := pwclient.CopyStagedBinaries(stageDir, filepath.Join(dir, "bin")); cerr != nil {
				partial = true
				log.Printf("[pwclient] 刷新 %s 失败(不影响主程序已完成的升级):%v", filepath.Join(dir, "bin"), cerr)
			}
		}
	}

	report(pwclient.UpgradePhaseRestarting, "正在重启服务…")
	if stopped && svcRunning {
		if err := startService(); err != nil {
			return fail("服务重启失败", err)
		}
	}
	pwclient.CleanupStaleExecutables(installDir, exeName)
	if oldExe != "" {
		// 托盘仍在运行时会占用旧镜像而删除失败,留到下次启动由 CleanupStaleExecutables 清理。
		_ = os.Remove(oldExe)
	}

	if partial {
		report(pwclient.UpgradePhaseDone, "已升级到 "+target+"(部分附属文件同步失败,不影响主程序;可重新执行升级补全)")
		fmt.Printf("已升级到 %s(安装目录 %s),但部分附属文件同步失败,可重新升级补全。\n", target, installDir)
	} else {
		report(pwclient.UpgradePhaseDone, "已升级到 "+target)
		fmt.Printf("已升级到 %s(安装目录 %s)。\n", target, installDir)
	}
	fmt.Println("托盘客户端如仍在运行,请退出后重新打开以使用新版本(后台服务已使用新版本)。")
	return nil
}

// printUpdateInfo 打印 --check 结果。
func printUpdateInfo(info version.UpdateInfo) {
	fmt.Println("当前版本:  ", info.Current)
	if info.Latest == "" {
		fmt.Println("最新版本:  (升级源上暂无发布)")
	} else {
		fmt.Println("最新版本:  ", info.Latest)
	}
	if info.UpdateAvailable {
		fmt.Println("有可用更新: 是")
	} else {
		fmt.Println("有可用更新: 否")
	}
	if info.ReleaseURL != "" {
		fmt.Println("发布页:    ", info.ReleaseURL)
	}
	if info.CheckError != "" {
		fmt.Println("检查失败:  ", info.CheckError)
	}
	if notes := strings.TrimSpace(info.Notes); notes != "" {
		fmt.Println("更新说明:")
		fmt.Println(notes)
	}
}

// upgradeTarget 解析升级落点:已安装服务 → 服务 exe 所在目录;未安装 → 当前 exe 所在
// 目录。查询失败(如普通权限连不上 SCM)上抛错误 —— 不能降级为「未安装」,否则
// 普通权限运行会把下载目录副本误当升级目标、绕过服务需要管理员的校验。
func upgradeTarget() (dir, exe string, installed, running bool, err error) {
	p, err := serviceBinaryPath()
	if err != nil {
		return "", "", false, false, err
	}
	if p != "" {
		return filepath.Dir(p), filepath.Base(p), true, serviceRunning(), nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", "", false, false, fmt.Errorf("定位当前程序失败:%w", err)
	}
	return filepath.Dir(self), filepath.Base(self), false, false, nil
}

// skipUpgrade 在「无需/无法升级」的提前退出路径落一个终态(skipped)并返回原因:
// 托盘轮询升级进度时据此立即结束,不会因没有终态记录而等超超时。
func skipUpgrade(dataDir, current, target, reason string) error {
	_ = pwclient.WriteUpgradeStatus(dataDir, pwclient.UpgradeStatus{
		Phase: pwclient.UpgradePhaseSkipped, Current: current, Target: target, Message: reason,
	})
	fmt.Println(reason)
	return nil
}

// exeFileVersion 读取磁盘上 PE 文件版本资源里的 FileVersion(major.minor.patch.build)。
// 升级比较需要「安装目标文件的实际版本」而非当前进程注入的 version.Version(从新版
// 解压目录运行升级器时两者不同)。x/sys 未封装 version.dll,经 LazyFindProc 手工调用。
func exeFileVersion(path string) (string, error) {
	modversion := syscall.NewLazyDLL("version.dll")
	procSize := modversion.NewProc("GetFileVersionInfoSizeW")
	procGet := modversion.NewProc("GetFileVersionInfoW")
	procQuery := modversion.NewProc("VerQueryValueW")
	if err := procSize.Find(); err != nil {
		return "", fmt.Errorf("加载 version.dll 失败:%w", err)
	}

	pathp, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	size, _, _ := procSize.Call(uintptr(unsafe.Pointer(pathp)), 0)
	if size == 0 {
		return "", fmt.Errorf("文件无版本信息(或文件不可读):%s", path)
	}
	buf := make([]uint16, size)
	r1, _, _ := procGet.Call(uintptr(unsafe.Pointer(pathp)), 0, size, uintptr(unsafe.Pointer(&buf[0])))
	if r1 == 0 {
		return "", fmt.Errorf("读取版本信息失败:%s", path)
	}
	var blockPtr unsafe.Pointer
	var blockLen uint32
	r1, _, _ = procQuery.Call(uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(`\`))),
		uintptr(unsafe.Pointer(&blockPtr)), uintptr(unsafe.Pointer(&blockLen)))
	if r1 == 0 || blockPtr == nil || blockLen < uint32(unsafe.Sizeof(win32FixedFileInfo{})) {
		return "", fmt.Errorf("版本信息缺少固定信息块:%s", path)
	}
	ffi := *(*win32FixedFileInfo)(blockPtr)
	major := uint16(ffi.FileVersionMS >> 16)
	minor := uint16(ffi.FileVersionMS & 0xffff)
	patch := uint16(ffi.FileVersionLS >> 16)
	build := uint16(ffi.FileVersionLS & 0xffff)
	return fmt.Sprintf("%d.%d.%d.%d", major, minor, patch, build), nil
}

// win32FixedFileInfo 是版本资源 VS_VERSION_INFO 固定块(小端 uint32)。
type win32FixedFileInfo struct {
	Magic            uint32
	StructVersion    uint32
	FileDateMS       uint32
	FileDateLS       uint32
	ProductVersionMS uint32
	ProductVersionLS uint32
	FileFlagsMask    uint32
	FileFlags        uint32
	FileOS           uint32
	FileType         uint32
	FileSubtype      uint32
	FileVersionMS    uint32
	FileVersionLS    uint32
}

// isElevated 报告当前进程是否已提权(UAC)。
func isElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}
