package build

// image_gc.go 是「镜像缓存自动管理」(Story 8-17 / FR-8-17):构建跑在中控机的容器 daemon 上,
// `docker run --rm` 只删容器**不删镜像** —— 重复构建会攒下大量**悬空(dangling/<none>)镜像**与层缓存,
// 磁盘随时间增长。本文件在每次构建后(尽力)清理悬空镜像,默认开启,可经 WithImageGC(false) 关。
//
// 安全边界:**只清悬空镜像**(`image prune -f`,即 dangling=true,被新构建顶替的无 tag 旧层),
// **绝不动有 tag 的镜像**(工具链缓存 node:20 等、本次 localTag 产物镜像都保留),不会误删在用镜像、
// 不会破坏缓存命中。清理是 best-effort:失败只记日志,绝不影响构建成败。

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// imagePruner 是「清悬空镜像」的可选能力(shellDriver 实现)。独立于 Driver 接口,
// 避免给所有 Driver 实现/假驱动新增方法;Builder 经类型断言择优调用。
type imagePruner interface {
	// PruneDangling 清理悬空(无 tag)镜像与构建层缓存(`<bin> image prune -f`)。
	// 返回是否成功执行(退出码 0)。逐行输出经 onLine。
	PruneDangling(ctx context.Context, onLine func(stream, line string)) (ok bool, err error)
}

// PruneDangling 实现 imagePruner:`<bin> image prune -f`(只清 dangling,不碰有 tag 镜像)。
func (d *shellDriver) PruneDangling(ctx context.Context, onLine func(stream, line string)) (bool, error) {
	args := []string{"image", "prune", "-f"}
	emitCmd(onLine, d.bin, args)
	code, err := d.cmdr.Stream(ctx, d.bin, args, "", onLine)
	if err != nil {
		return false, err
	}
	return code == 0, nil
}

// builderCacheKeep 是构建后 BuildKit 缓存清理的保留窗口:只清 7 天前的缓存——活跃项目
// 近期层照常命中(重建提速),总量有界。全量清留给维护手动入口(Builder.ExtraImageClean)。
const builderCacheKeep = 7 * 24 * time.Hour

// PruneBuilderCache 清 BuildKit 构建缓存(`<bin> builder prune -f`)。keep>0 时加
// `--filter until=<keep>`(保留最近窗口,活跃项目重建仍提速);keep=0 全清。
//
// 关键背景:现代 Docker 默认 BuildKit,构建缓存存在**独立的缓存区**,`image prune` 清不到、
// 保留带 tag 镜像也换不来缓存命中——不清它就无限增长(磁盘泄漏大头之一)。老 docker
// (如 1.13,无 builder 子命令)执行报错 → 调用方按 best-effort 忽略(传统构建器的缓存
// 本就是悬空镜像,已由 PruneDangling 覆盖)。
func (d *shellDriver) PruneBuilderCache(ctx context.Context, keep time.Duration, onLine func(stream, line string)) error {
	args := []string{"builder", "prune", "-f"}
	if keep > 0 {
		args = append(args, "--filter", "until="+keep.String())
	}
	emitCmd(onLine, d.bin, args)
	_, _, code, err := d.cmdr.Run(ctx, d.bin, args, "")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("%s builder prune 退出码 %d", d.bin, code)
	}
	return nil
}

// ExtraImageClean 维护手动入口的追加清理:BuildKit 构建缓存全清 + 停止容器回收
// (crash 残留的 `docker run --rm` 尸体)。返回执行的命令回显(留痕用)。仅动缓存与
// **已停止**容器,运行中容器绝不碰。瘦控制机返回空。
func (b *Builder) ExtraImageClean(ctx context.Context) []string {
	d, ok := b.driver.(*shellDriver)
	if !ok {
		return nil
	}
	var echos []string
	for _, args := range [][]string{
		{"builder", "prune", "-f"},
		{"container", "prune", "-f"},
	} {
		emitCmd(func(stream, line string) {}, d.bin, args)
		echos = append(echos, "$ "+d.bin+" "+strings.Join(args, " "))
		if _, _, code, err := d.cmdr.Run(ctx, d.bin, args, ""); err != nil || code != 0 {
			// 老 docker 无 builder 子命令等:best-effort,已执行即留痕,失败不阻断后续。
			continue
		}
	}
	return echos
}

// pruneImagesBestEffort 在构建后尽力清悬空镜像(Story 8-17):driver 支持且未关 GC 时执行。
// 任何失败仅记一行日志,绝不影响构建结果(NFR-10:运维便利不能反噬核心 CI/CD)。
func (b *Builder) pruneImagesBestEffort(ctx context.Context, onLine func(stream, line string)) {
	if b.disableImageGC {
		return
	}
	pruner, ok := b.driver.(imagePruner)
	if !ok {
		return // 桩驱动/瘦控制机(errDriver)等未实现镜像清理的驱动:跳过(不报错)。
		// 注:远程与共享构建机的驱动同为 *shellDriver(仅命令通道换 SSH),断言命中,prune 落到对应机器。
	}
	if _, err := pruner.PruneDangling(ctx, onLine); err != nil {
		onLine(streamError, "镜像清理(悬空)失败,忽略不影响构建:"+err.Error())
	}
	// BuildKit 缓存按 7 天窗口清理(image prune 清不到的独立缓存区;见 PruneBuilderCache)。
	if bc, ok := b.driver.(buildCachePruner); ok {
		if err := bc.PruneBuilderCache(ctx, builderCacheKeep, onLine); err != nil {
			onLine(streamError, "构建缓存清理(BuildKit)失败,忽略不影响构建:"+err.Error())
		}
	}
}

// buildCachePruner 是「清 BuildKit 构建缓存」的可选能力(shellDriver 实现;远程 SSH 驱动同体)。
type buildCachePruner interface {
	PruneBuilderCache(ctx context.Context, keep time.Duration, onLine func(stream, line string)) error
}
