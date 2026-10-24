package build

import (
	"context"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/project"
)

// rotationTestBuilder 构造带 fake 驱动与脚本化轮转依赖的 Builder。
func rotationTestBuilder(images []string, keepN int, commits []string) (*Builder, *fakeCommander) {
	cmdr := newFakeCommander()
	cmdr.script("images", fakeCmd{stdoutLines: images})
	cmdr.script("rmi", fakeCmd{exitCode: 0})
	b := &Builder{
		driver: &shellDriver{bin: "docker", cmdr: cmdr},
		imageRotation: ImageRotationConfig{
			KeepN: func(context.Context) int { return keepN },
			RecentCommits: func(context.Context, string, int) ([]string, error) {
				return commits, nil
			},
		},
	}
	return b, cmdr
}

func rmiRefs(cmdr *fakeCommander) []string {
	cmdr.mu.Lock()
	defer cmdr.mu.Unlock()
	refs := []string{}
	for _, e := range cmdr.execs {
		if len(e.args) >= 2 && e.args[0] == "rmi" {
			refs = append(refs, e.args[1])
		}
	}
	return refs
}

// 保留集 = 最近 N 个 commit + 当前构建 tag;同前缀更旧的删除;其它 repo/项目绝不触碰。
func TestRotateBuildImagesKeepsRecentAndCurrent(t *testing.T) {
	b, cmdr := rotationTestBuilder([]string{
		"pipewright/my-app:aaa1111",
		"pipewright/my-app:bbb2222",
		"pipewright/my-app:ccc3333",
		"pipewright/my-app:ddd4444", // 超出保留集(最近2+当前之外)→ 删
		"pipewright/other-proj:aaa1111",
		"node:20",
		"<none>:<none>",
	}, 2, []string{"bbb2222", "aaa1111"})
	b.rotateBuildImages(context.Background(), &project.Project{ID: "p1", Name: "My App"}, "", "ccc3333", func(string, string) {})

	refs := rmiRefs(cmdr)
	if len(refs) != 1 || refs[0] != "pipewright/my-app:ddd4444" {
		t.Fatalf("应只删除超出保留集的 ddd4444,实际 rmi:%v", refs)
	}
}

// target 非空时轮转作用于 pipewright/<slug>-<target> 族:该族旧 tag 删、其它族(含裸 slug
// 族与兄弟 target 族)绝不触碰。
func TestRotateBuildImagesTargetsSuffixedFamily(t *testing.T) {
	b, cmdr := rotationTestBuilder([]string{
		"pipewright/my-app-backend:aaa1111",
		"pipewright/my-app-backend:bbb2222",
		"pipewright/my-app-backend:old9999",  // 本族超出保留集 → 删
		"pipewright/my-app:old9999",          // 裸 slug 族不在此轮转范围
		"pipewright/my-app-frontend:old9999", // 兄弟 target 族不在此轮转范围
	}, 2, []string{"bbb2222", "aaa1111"})
	b.rotateBuildImages(context.Background(), &project.Project{ID: "p1", Name: "My App"}, "backend", "aaa1111", func(string, string) {})

	refs := rmiRefs(cmdr)
	if len(refs) != 1 || refs[0] != "pipewright/my-app-backend:old9999" {
		t.Fatalf("应只删除 backend 族超出保留集的 old9999,实际 rmi:%v", refs)
	}
}

// 全局清理(RotateImages 直调,维护入口路径)的保留集只按项目构造 pipewright/<slug>:<commit>;
// 同 commit 的 target 变体族镜像须被该键一并保护(keptByFamilyVariant),变体族更旧的仍删。
func TestRotateImagesKeepsTargetVariantsByFamilyKey(t *testing.T) {
	_, cmdr := rotationTestBuilder([]string{
		"pipewright/my-app:aaa1111",
		"pipewright/my-app-backend:aaa1111",  // 同 commit 变体 → 保留集键 pipewright/my-app:aaa1111 保护
		"pipewright/my-app-frontend:aaa1111", // 同上
		"pipewright/my-app-backend:old9999",  // 变体族超出保留集 → 删
		"pipewright/other:aaa1111",           // 无键保护(commit 不在保留集)→ 删
	}, 2, []string{"bbb2222", "aaa1111"})
	drv := &shellDriver{bin: "docker", cmdr: cmdr}
	keep := map[string]struct{}{
		"pipewright/my-app:aaa1111": {},
		"pipewright/my-app:bbb2222": {},
	}
	removed, err := RotateImages(context.Background(), drv, "pipewright/", keep, func(string, string) {})
	if err != nil {
		t.Fatalf("RotateImages: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("应删除变体族旧 tag 与无键保护引用,实际:%v", removed)
	}
	got := map[string]bool{}
	for _, r := range removed {
		got[r] = true
	}
	if !got["pipewright/my-app-backend:old9999"] || !got["pipewright/other:aaa1111"] {
		t.Fatalf("删错对象,实际:%v", removed)
	}
}

// keepN<=0(未配置/关闭轮转)→ 完全不动镜像。
func TestRotateBuildImagesDisabledWhenKeepZero(t *testing.T) {
	b, cmdr := rotationTestBuilder([]string{"pipewright/my-app:aaa1111"}, 0, nil)
	b.rotateBuildImages(context.Background(), &project.Project{ID: "p1", Name: "My App"}, "", "aaa1111", func(string, string) {})
	if refs := rmiRefs(cmdr); len(refs) != 0 {
		t.Fatalf("keepN=0 不应轮转,实际 rmi:%v", refs)
	}
}

// 零值依赖(未注入 WithImageRotation)→ 静默跳过,不报错。
func TestRotateBuildImagesNoopWithoutDeps(t *testing.T) {
	cmdr := newFakeCommander()
	cmdr.script("images", fakeCmd{stdoutLines: []string{"pipewright/my-app:aaa1111"}})
	b := &Builder{driver: &shellDriver{bin: "docker", cmdr: cmdr}}
	b.rotateBuildImages(context.Background(), &project.Project{ID: "p1", Name: "My App"}, "", "aaa1111", func(string, string) {})
	if all := cmdr.allArgsText(); strings.Contains(all, "images") || strings.Contains(all, "rmi") {
		t.Fatalf("未注入轮转依赖不应执行任何镜像命令,实际:%s", all)
	}
}

// 当前构建 tag(如被推送源的修订 label,非 trigger_commit)必保留。
func TestRotateBuildImagesAlwaysKeepsCurrentTag(t *testing.T) {
	b, cmdr := rotationTestBuilder([]string{
		"pipewright/my-app:rev-xyz",
		"pipewright/my-app:aaa1111",
		"pipewright/my-app:bbb2222",
	}, 1, []string{"aaa1111"})
	b.rotateBuildImages(context.Background(), &project.Project{ID: "p1", Name: "My App"}, "", "rev-xyz", func(string, string) {})

	refs := rmiRefs(cmdr)
	if len(refs) != 1 || refs[0] != "pipewright/my-app:bbb2222" {
		t.Fatalf("应删超出保留集的 bbb2222、保留当前 rev-xyz 与最近 aaa1111,实际 rmi:%v", refs)
	}
}

// ListImageRefs:逐行解析、剔除 <none>/空行/含空白异常行。
func TestListImageRefsParsesAndFilters(t *testing.T) {
	cmdr := newFakeCommander()
	cmdr.script("images", fakeCmd{stdoutLines: []string{
		"pipewright/my-app:aaa1111",
		"<none>:<none>",
		"",
	}})
	drv := &shellDriver{bin: "docker", cmdr: cmdr}
	refs, err := drv.ListImageRefs(context.Background())
	if err != nil {
		t.Fatalf("ListImageRefs err=%v", err)
	}
	if len(refs) != 1 || refs[0] != "pipewright/my-app:aaa1111" {
		t.Fatalf("应只剩一个有效引用,实际:%v", refs)
	}
}
