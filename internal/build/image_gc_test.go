package build

import (
	"context"
	"strings"
	"testing"
)

func TestPruneDanglingRunsImagePrune(t *testing.T) {
	cmdr := newFakeCommander()
	cmdr.script("image", fakeCmd{exitCode: 0, stdoutLines: []string{"Total reclaimed space: 1.2GB"}})
	drv := &shellDriver{bin: "docker", cmdr: cmdr}

	ok, err := drv.PruneDangling(context.Background(), func(string, string) {})
	if err != nil || !ok {
		t.Fatalf("PruneDangling ok=%v err=%v", ok, err)
	}
	// 只能是 `docker image prune -f`(只清悬空,绝不带 -a / 不删有 tag 镜像)。
	all := cmdr.allArgsText()
	if !strings.Contains(all, "image prune -f") {
		t.Fatalf("应执行 image prune -f,实际:%s", all)
	}
	if strings.Contains(all, "prune -a") || strings.Contains(all, "--all") {
		t.Fatalf("绝不应带 -a/--all(会删有 tag 镜像):%s", all)
	}
}

func TestPruneImagesBestEffortRunsWhenEnabled(t *testing.T) {
	cmdr := newFakeCommander()
	cmdr.script("image", fakeCmd{exitCode: 0})
	b := &Builder{driver: &shellDriver{bin: "docker", cmdr: cmdr}}
	b.pruneImagesBestEffort(context.Background(), func(string, string) {})
	if !strings.Contains(cmdr.allArgsText(), "image prune -f") {
		t.Fatalf("默认应清悬空镜像,实际:%s", cmdr.allArgsText())
	}
}

func TestPruneImagesBestEffortSkipsWhenDisabled(t *testing.T) {
	cmdr := newFakeCommander()
	cmdr.script("image", fakeCmd{exitCode: 0})
	b := &Builder{driver: &shellDriver{bin: "docker", cmdr: cmdr}, disableImageGC: true}
	b.pruneImagesBestEffort(context.Background(), func(string, string) {})
	if strings.Contains(cmdr.allArgsText(), "prune") {
		t.Fatalf("关闭 GC 后不应清镜像,实际:%s", cmdr.allArgsText())
	}
}

// 驱动不支持镜像清理(如远程/桩)→ 静默跳过,不 panic、不报错。
func TestPruneImagesBestEffortNoopForNonPruner(t *testing.T) {
	b := &Builder{driver: &recordingDriver{}} // recordingDriver 不实现 imagePruner
	b.pruneImagesBestEffort(context.Background(), func(string, string) {})
}

// 构建后清理必须连带清 BuildKit 缓存(image prune 清不到的独立缓存区,不清就无限增长),
// 且带 7 天保留窗口(活跃项目近期层照常命中)。
func TestPruneImagesBestEffortAlsoPrunesBuilderCache(t *testing.T) {
	cmdr := newFakeCommander()
	cmdr.script("image", fakeCmd{exitCode: 0})
	cmdr.script("builder", fakeCmd{exitCode: 0})
	b := &Builder{driver: &shellDriver{bin: "docker", cmdr: cmdr}}
	b.pruneImagesBestEffort(context.Background(), func(string, string) {})
	all := cmdr.allArgsText()
	if !strings.Contains(all, "builder prune -f") {
		t.Fatalf("构建后应清 BuildKit 缓存,实际:%s", all)
	}
	if !strings.Contains(all, "until=168h") {
		t.Fatalf("BuildKit 清理应带 7 天保留窗口,实际:%s", all)
	}
}

// PruneBuilderCache:keep=0 全清(不带 filter);老 docker 报错透出由调用方忽略。
func TestPruneBuilderCacheFullWithoutKeep(t *testing.T) {
	cmdr := newFakeCommander()
	cmdr.script("builder", fakeCmd{exitCode: 125}) // 模拟老 docker 无 builder 子命令
	drv := &shellDriver{bin: "docker", cmdr: cmdr}
	if err := drv.PruneBuilderCache(context.Background(), 0, func(string, string) {}); err == nil {
		t.Fatalf("非零退出码应报错(调用方按 best-effort 忽略)")
	}
	all := cmdr.allArgsText()
	if strings.Contains(all, "until=") {
		t.Fatalf("keep=0 不应带 filter,实际:%s", all)
	}
}

// ExtraImageClean:BuildKit 缓存全清 + 停止容器回收,命令留痕。
func TestExtraImageCleanRunsBuilderAndContainerPrune(t *testing.T) {
	cmdr := newFakeCommander()
	cmdr.script("builder", fakeCmd{exitCode: 0})
	cmdr.script("container", fakeCmd{exitCode: 0})
	b := &Builder{driver: &shellDriver{bin: "docker", cmdr: cmdr}}
	echos := b.ExtraImageClean(context.Background())
	all := cmdr.allArgsText()
	if !strings.Contains(all, "builder prune -f") || !strings.Contains(all, "container prune -f") {
		t.Fatalf("应执行 builder/container prune,实际:%s", all)
	}
	joined := strings.Join(echos, "\n")
	if !strings.Contains(joined, "$ docker builder prune -f") || !strings.Contains(joined, "$ docker container prune -f") {
		t.Fatalf("应返回命令回显,实际:%v", echos)
	}
}
