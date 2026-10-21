package build

// image_rotate.go 是「产物镜像轮转」(磁盘治理):build_image 每次构建都产出带 tag 的本地镜像
// pipewright/<slug>:<commit>,推送后留在构建机 daemon 上且历史版本无人回收,长期单调增长。
// 本文件在构建/推送完成后按「每项目保留最近 N 个 commit tag」轮转:枚举 daemon 上本项目
// 前缀的镜像,删除不在保留集内的旧 tag。
//
// 安全边界(与 image_gc.go 的 dangling 清理互补,本文件动的是**有 tag** 镜像):
//   - 只处理 repo 前缀为 pipewright/<slug>: 的引用——基础镜像/工具链缓存(node:20 等)绝不触碰;
//   - 保留集来自 DB 最近 N 个 trigger_commit(可追溯)+ 当前构建的 tag(被推送源等非 commit
//     命名兜底),绝不误删刚构建的镜像;
//   - rmi 不带 -f(被容器占用即失败跳过),全程 best-effort,任何失败只记日志。
//
// 作用面:本地 daemon(控制机)与远程构建机(rb 浅拷贝换 SSH 驱动后,runBuildImageJob 内
// 的轮转自然落到远程机)。**共享构建机除外**:接收方 DB 没有发起方项目的提交历史,保留集
// 无法构成,故 execSharedBuildImage 只做 dangling prune 不做轮转;发起方在 load 回本端后
// 用自己的 DB 轮转本端副本(loadAndPushSharedImage)。

import (
	"context"
	"fmt"
	"strings"

	"github.com/huangchengsir/pipewright/internal/dagrun"
	"github.com/huangchengsir/pipewright/internal/project"
)

// imageRotator 是「产物镜像轮转」的可选能力(shellDriver 实现;本地与 SSH 远程驱动同体,
// 经类型断言发现,Driver 接口不扩——与 imagePruner/Load 同一先例)。
type imageRotator interface {
	// ListImageRefs 列出 daemon 上全部 repo:tag 引用(悬空 <none> 跳过)。
	ListImageRefs(ctx context.Context) ([]string, error)
	// RemoveImage 删除一个镜像引用(不带 -f;被占用等冲突以错误透出,调用方记日志跳过)。
	RemoveImage(ctx context.Context, ref string, onLine func(stream, line string)) error
}

// ListImageRefs 实现 imageRotator:逐字段模板列出 repo:tag(不用 json 模板函数——老 docker
// 如 1.13 对 json 模板恒输出 {},同 httpapi/server_images.go 的口径)。
func (d *shellDriver) ListImageRefs(ctx context.Context) ([]string, error) {
	args := []string{"images", "--format", "{{.Repository}}:{{.Tag}}"}
	stdout, _, code, err := d.cmdr.Run(ctx, d.bin, args, "")
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("%s images 退出码 %d", d.bin, code)
	}
	var refs []string
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		// 悬空镜像的 repo/tag 呈 <none>:不算可轮转引用;顺带挡掉空行/模板异常输出。
		if line == "" || strings.Contains(line, "<none>") || strings.ContainsAny(line, " \t") {
			continue
		}
		refs = append(refs, line)
	}
	return refs, nil
}

// RemoveImage 实现 imageRotator:rmi 单个引用(array 实参,ref 来自本机 docker images 输出
// 而非用户输入,无注入面)。
func (d *shellDriver) RemoveImage(ctx context.Context, ref string, onLine func(stream, line string)) error {
	args := []string{"rmi", ref}
	emitCmd(onLine, d.bin, args)
	_, _, code, err := d.cmdr.Run(ctx, d.bin, args, "")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("退出码 %d", code)
	}
	return nil
}

// ImageRotationConfig 是镜像轮转依赖(零值 = 不轮转,向后兼容;由 main 经 WithImageRotation 注入)。
type ImageRotationConfig struct {
	// KeepN 返回当前每项目保留个数(每轮构建现读 DB 设置,免重启生效;<=0 = 关闭轮转)。
	KeepN func(ctx context.Context) int
	// RecentCommits 返回项目最近 n 个去重 trigger_commit(新→旧;run.Service 实现)。
	RecentCommits func(ctx context.Context, projectID string, n int) ([]string, error)
}

// RotateImages 删除 driver 所指机器上、前缀 prefix 内且不在保留集 keep 里的产物镜像引用,
// 返回被删除的引用清单。维护手动入口(设置页「构建产物镜像」)与构建后轮转共用:
//   - prefix 取 "pipewright/"(全局清)或 "pipewright/<slug>:"(单项目轮转);
//   - rmi 不带 -f(被容器占用即失败跳过),全程 best-effort。
func RotateImages(ctx context.Context, driver Driver, prefix string, keep map[string]struct{}, onLine func(stream, line string)) ([]string, error) {
	rot, ok := driver.(imageRotator)
	if !ok {
		return nil, nil // 桩驱动/瘦控制机(errDriver)无镜像枚举能力:跳过(不报错)
	}
	refs, err := rot.ListImageRefs(ctx)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, ref := range refs {
		if !strings.HasPrefix(ref, prefix) {
			continue // 只动产物镜像;基础镜像/工具链缓存绝不碰
		}
		if _, kept := keep[ref]; kept {
			continue
		}
		if rerr := rot.RemoveImage(ctx, ref, onLine); rerr != nil {
			onLine(streamError, "镜像清理:删除 "+ref+" 失败,忽略:"+rerr.Error())
			continue
		}
		removed = append(removed, ref)
	}
	return removed, nil
}

// rotateBuildImages 轮转 b.driver 所指机器上本项目的前缀镜像:保留最近 keepN 个 commit tag
// + 当前构建 tag,其余 rmi(best-effort,绝不影响构建结果)。
func (b *Builder) rotateBuildImages(ctx context.Context, proj *project.Project, commitTag string, onLine func(stream, line string)) {
	if b.imageRotation.KeepN == nil || b.imageRotation.RecentCommits == nil || proj == nil {
		return
	}
	keepN := b.imageRotation.KeepN(ctx)
	if keepN <= 0 {
		return // 未配置/关闭轮转(0=不限保留)
	}
	commits, err := b.imageRotation.RecentCommits(ctx, proj.ID, keepN)
	if err != nil {
		onLine(streamError, "镜像轮转:查询最近提交失败,忽略不影响构建:"+err.Error())
		return
	}
	prefix := localTagPrefix + "/" + slugify(proj.Name) + ":"
	keep := make(map[string]struct{}, len(commits)+1)
	keep[prefix+commitTag] = struct{}{} // 当前构建必保留(被推送源的修订 label 非 trigger_commit)
	for _, c := range commits {
		if c == "" {
			continue
		}
		keep[prefix+c] = struct{}{}
	}
	if _, err := RotateImages(ctx, b.driver, prefix, keep, onLine); err != nil {
		onLine(streamError, "镜像轮转:枚举镜像失败,忽略不影响构建:"+err.Error())
	}
}

// RotateImagesWithKeep 按外部给定保留集清理控制机(b.driver)上的产物镜像(维护手动入口用;
// prefix 传 "pipewright/" 即全项目)。瘦控制机(errDriver)返回空清单。
func (b *Builder) RotateImagesWithKeep(ctx context.Context, prefix string, keep map[string]struct{}, onLine func(stream, line string)) ([]string, error) {
	return RotateImages(ctx, b.driver, prefix, keep, onLine)
}

// removeLocalTagAfterPush 推送成功后回收构建机上的本机 tag(磁盘治理):镜像已在 registry,
// 构建机无需留存。经 WithImagePushCleanup 可关——注意:现代 Docker(BuildKit)下保留本机
// 副本对重建速度**没有**收益(构建缓存在 BuildKit 独立缓存区,与本机 tag 无关),仅老版
// Docker 的传统构建器能用镜像层做缓存;关闭仅建议在老 docker 环境下。
// best-effort:删除失败只记日志,绝不影响构建结果;清理回调 nil 视为开启(默认回收)。
func (b *Builder) removeLocalTagAfterPush(ctx context.Context, rep dagrun.StageReporter, localTag string) {
	if b.imageRemoveAfterPush != nil && !b.imageRemoveAfterPush(ctx) {
		return // 设置里关闭了「推送后回收本机镜像」
	}
	rot, ok := b.driver.(imageRotator)
	if !ok {
		return
	}
	pctx := context.WithoutCancel(ctx)
	_ = rep.Log(pctx, streamStdout, "→ 已推送,回收构建机本机镜像 "+localTag+"(设置可关闭)")
	if err := rot.RemoveImage(pctx, localTag, func(stream, line string) { _ = rep.Log(pctx, stream, line) }); err != nil {
		_ = rep.Log(pctx, streamError, "回收本机镜像失败(best-effort,忽略):"+err.Error())
	}
}
