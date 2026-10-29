//go:build !windows

// upgrade_other.go 是 `pwclient upgrade` 在非 Windows 平台的实现:检查最新发布 →
// 下载 + sha256 校验 + 解包 tar.gz → 停服务 → 旧程序改名让位并落位新版(含随包 bin/
// EasyTier 与 licenses)→ 重启服务。平台无关的重活(下载/校验/解包/落位)在 internal/
// pwclient.Upgrade 内;本文件只做服务停启编排与状态落盘。
//
// 与 Windows 一致地把进度写 <dataDir>/upgrade-status.json 供界面展示。升级源默认 GitHub
// 官方 → gh-proxy 公共代理自动兜底;显式配置镜像则只走镜像。安装/服务操作需 root。
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
	"time"

	"github.com/huangchengsir/pipewright/internal/pwclient"
	"github.com/huangchengsir/pipewright/internal/version"
)

// upgradeOptions 是 `pwclient upgrade` 的解析结果。
type upgradeOptions struct {
	CheckOnly bool
	Tag       string
	Force     bool
	// Unstable 接受非稳定版本:把预发布(beta/rc)纳入「最新版」口径。
	Unstable bool
}

// runUpgrade 处理 `pwclient upgrade [--check] [--tag vX.Y.Z] [--force] [--unstable]`。
func runUpgrade(args []string) {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	checkOnly := fs.Bool("check", false, "只检查最新版本,不下载/不改动文件")
	tag := fs.String("tag", "", "升级到指定版本 tag(如 v1.2.3;缺省 = 最新)")
	force := fs.Bool("force", false, "忽略版本比较强制重装/升级")
	unstable := fs.Bool("unstable", false, "接受非稳定版本(预发布 beta/rc 纳入「最新版」口径)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if err := doUpgrade(upgradeOptions{CheckOnly: *checkOnly, Tag: *tag, Force: *force, Unstable: *unstable}); err != nil {
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

	info := up.Check(ctx, opts.Unstable)
	if opts.CheckOnly {
		printUpdateInfo(info)
		if info.CheckError != "" {
			return errors.New(info.CheckError)
		}
		return nil
	}

	// 升级落点:已安装服务 → 服务 exe 所在目录;未安装 → 当前 exe 所在目录。
	installDirPath, exeName, svcInstalled, svcRunning, err := upgradeTarget()
	if err != nil {
		return err
	}
	if svcInstalled && !isElevated() {
		return errors.New("升级已安装的服务需要 root 权限:请以 sudo 运行 `pwclient upgrade`")
	}
	// 非 Windows 无 PE 版本资源可比,以当前进程版本为比较基准(与安装目标通常一致)。
	targetVer := current

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

	fmt.Printf("当前版本 %s,准备升级到 %s(安装目录 %s)…\n", current, target, installDirPath)
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
	restoreService := func() {
		if stopped && svcRunning {
			if serr := startService(); serr != nil {
				log.Printf("[pwclient] 升级失败后恢复服务启动失败:%v", serr)
			}
		}
	}

	// partial 标记「主程序已落位成功、仅附属文件同步失败」:不报失败/不回滚,后续照常
	// 重启服务,终态 done + 警示(避免界面误报失败而实际已升级)。
	partial := false
	oldExe, err := pwclient.ApplyStaged(stageDir, installDirPath, exeName)
	if err != nil {
		if !errors.Is(err, pwclient.ErrPartialUpgrade) {
			restoreService()
			return fail("替换程序文件失败", err)
		}
		partial = true
		log.Printf("[pwclient] 升级部分完成:%v", err)
	}
	// 刷新历史自动下载落点 <dataDir>/bin:EasyTier 查找优先级 dataDir > exe 同级 bin。
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
	pwclient.CleanupStaleExecutables(installDirPath, exeName)
	if oldExe != "" {
		_ = os.Remove(oldExe)
	}

	if partial {
		report(pwclient.UpgradePhaseDone, "已升级到 "+target+"(部分附属文件同步失败,不影响主程序;可重新执行升级补全)")
		fmt.Printf("已升级到 %s(安装目录 %s),但部分附属文件同步失败,可重新升级补全。\n", target, installDirPath)
	} else {
		report(pwclient.UpgradePhaseDone, "已升级到 "+target)
		fmt.Printf("已升级到 %s(安装目录 %s)。\n", target, installDirPath)
	}
	fmt.Println("如仍在运行界面进程,请退出后重新打开以使用新版本(后台服务已使用新版本)。")
	return nil
}

// upgradeTarget 解析升级落点:已安装服务 → 服务 exe 所在目录;未安装 → 当前 exe 所在目录。
func upgradeTarget() (dir, exe string, installed, running bool, err error) {
	p, err := serviceBinaryPath()
	if err != nil {
		// 区分「未安装」(占位/无服务管理器或确无 unit)与真错误。查询失败按未安装处理,
		// 但仍以 root 校验兜底:非 root 时下面 requireRoot 会在启停服务处拦下。
		p = ""
		err = nil
	}
	if p != "" {
		return filepath.Dir(p), filepath.Base(p), true, serviceRunning(), nil
	}
	self, oerr := os.Executable()
	if oerr != nil {
		return "", "", false, false, fmt.Errorf("定位当前程序失败:%w", oerr)
	}
	return filepath.Dir(self), filepath.Base(self), false, false, nil
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
	if info.Prerelease {
		fmt.Println("版本类型:  预发布(非稳定)")
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

// skipUpgrade 在「无需/无法升级」的提前退出路径落一个终态(skipped)并返回原因。
func skipUpgrade(dataDir, current, target, reason string) error {
	_ = pwclient.WriteUpgradeStatus(dataDir, pwclient.UpgradeStatus{
		Phase: pwclient.UpgradePhaseSkipped, Current: current, Target: target, Message: reason,
	})
	fmt.Println(reason)
	return nil
}
