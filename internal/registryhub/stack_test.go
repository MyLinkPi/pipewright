package registryhub

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/storetest"
)

// storetestOpen 打开全量迁移的 sqlite 测试库。
func storetestOpen(t *testing.T) *sql.DB {
	t.Helper()
	return storetest.OpenDB(t)
}

// scriptRunner 按命令首段 + 子命令脚本化响应的 LocalRunner(记录全部调用)。
type scriptRunner struct {
	steps    []scriptStep
	commands [][]string
	index    int
}

type scriptStep struct {
	match func(name string, args []string) bool
	code  int
	out   string
}

func (r *scriptRunner) Run(_ context.Context, name string, args []string, _ string) (string, string, int, error) {
	r.commands = append(r.commands, append([]string{name}, args...))
	for i := r.index; i < len(r.steps); i++ {
		if r.steps[i].match != nil && r.steps[i].match(name, args) {
			r.index = i + 1
			if r.steps[i].code != 0 {
				return "", r.steps[i].out, r.steps[i].code, nil
			}
			return r.steps[i].out, "", 0, nil
		}
	}
	return "", "", 0, nil
}

func argHas(v string) func([]string) bool {
	return func(args []string) bool {
		for _, a := range args {
			if a == v {
				return true
			}
		}
		return false
	}
}

func TestDeployStackWritesComposeAndUps(t *testing.T) {
	dir := t.TempDir()
	runner := &scriptRunner{steps: []scriptStep{
		{match: func(name string, args []string) bool {
			return name == "docker" && len(args) >= 1 && args[0] == "compose" && argHas("version")(args)
		}, out: "v2"},
		{match: func(name string, args []string) bool { return argHas("up")(args) && argHas("-d")(args) }, out: "started"},
	}}
	db := storetestOpen(t)
	h := New(db, nil, Options{BaseDir: dir, Runner: runner})
	if _, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "ctrl", UpstreamURL: "https://docker.m.daocloud.io", ArtifactPort: 6000, CachePort: 6001}); err != nil {
		t.Fatalf("save: %v", err)
	}
	res, err := h.DeployStack(context.Background())
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if !res.OK || res.Error != "" {
		t.Fatalf("部署应成功: %+v", res)
	}
	// compose 文件落盘且内容含端口/上游/卷。
	raw, rerr := os.ReadFile(filepath.Join(dir, composeFile))
	if rerr != nil {
		t.Fatalf("compose 未落盘: %v", rerr)
	}
	content := string(raw)
	for _, want := range []string{
		"\"6000:5000\"", "\"6001:5000\"",
		"REGISTRY_PROXY_REMOTEURL: \"https://docker.m.daocloud.io\"",
		"REGISTRY_STORAGE_DELETE_ENABLED: \"true\"",
		registryImage, artifactCont, cacheCont,
		filepath.Join(dir, "data"), filepath.Join(dir, "cache"),
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("compose 缺 %q:\n%s", want, content)
		}
	}
	// data/cache 目录已建。
	for _, sub := range []string{"data", "cache"} {
		if _, serr := os.Stat(filepath.Join(dir, sub)); serr != nil {
			t.Fatalf("目录 %s 未创建: %v", sub, serr)
		}
	}
	// up 命令带 -p 与 -f。
	joined := ""
	for _, c := range runner.commands {
		joined += strings.Join(c, " ") + "\n"
	}
	if !strings.Contains(joined, "compose -p pipewright-registry -f ") || !strings.Contains(joined, " up -d") {
		t.Fatalf("compose up 命令不符: %s", joined)
	}
}

func TestDeployStackDisabled(t *testing.T) {
	h := newTestHub(t, Options{BaseDir: t.TempDir(), Runner: &scriptRunner{}})
	if _, err := h.DeployStack(context.Background()); err != ErrDisabled {
		t.Fatalf("未启用应 ErrDisabled, got %v", err)
	}
}

func TestDeployStackNoCompose(t *testing.T) {
	h := newTestHub(t, Options{BaseDir: t.TempDir(), Runner: &scriptRunner{}}) // 全部命令 exit 0 但 compose version 输出为空 → 探测失败
	if _, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "ctrl"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// compose 探测:docker compose version 无输出 + docker-compose version exit 0 → v1 兜底可用。
	// 这里脚本 runner 无步匹配返回 0/空;docker compose version out 为空 → 走 v1 分支。
	res, err := h.DeployStack(context.Background())
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if !res.OK {
		t.Fatalf("v1 compose 兜底应成功: %+v", res)
	}
}

func TestStatusBestEffort(t *testing.T) {
	dir := t.TempDir()
	// 预置 compose 文件与 data 文件,验证 Deployed/占用统计。
	if werr := os.WriteFile(filepath.Join(dir, composeFile), []byte("services: {}"), 0o644); werr != nil {
		t.Fatalf("seed compose: %v", werr)
	}
	if werr := os.MkdirAll(filepath.Join(dir, "data"), 0o755); werr != nil {
		t.Fatalf("mkdir data: %v", werr)
	}
	if werr := os.WriteFile(filepath.Join(dir, "data", "blob"), []byte(strings.Repeat("x", 1024)), 0o644); werr != nil {
		t.Fatalf("seed blob: %v", werr)
	}
	runner := &scriptRunner{steps: []scriptStep{
		{match: func(name string, args []string) bool { return name == "docker" && argHas(artifactCont)(args) }, out: "running"},
		{match: func(name string, args []string) bool { return name == "docker" && argHas(cacheCont)(args) }, out: "exited"},
	}}
	h := New(storetestOpen(t), nil, Options{
		BaseDir: dir, Runner: runner,
		HTTPClient: &http.Client{Timeout: 300 * time.Millisecond}, // 探活必败(无真服务),限时限速
	})
	st, err := h.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Deployed || !st.ArtifactRunning || st.CacheRunning {
		t.Fatalf("状态不符: %+v", st)
	}
	if st.ArtifactReachable || st.CacheReachable {
		t.Fatalf("无真服务时探活应 false: %+v", st)
	}
	if st.DataDirBytes != 1024 || st.CacheDirBytes != 0 {
		t.Fatalf("存储占用不符: %+v", st)
	}
	if st.Image != registryImage {
		t.Fatalf("镜像名不符: %s", st.Image)
	}
}

// fakeRegistryAPI 是保留策略测试用的 registry API fake。
type fakeRegistryAPI struct {
	repos   map[string][]TagInfo
	deleted []string // "repo@digest"
}

func (f *fakeRegistryAPI) Catalog(context.Context) ([]string, error) {
	repos := make([]string, 0, len(f.repos))
	for r := range f.repos {
		repos = append(repos, r)
	}
	return repos, nil
}

func (f *fakeRegistryAPI) TagInfos(_ context.Context, repo string) ([]TagInfo, error) {
	return f.repos[repo], nil
}

func (f *fakeRegistryAPI) DeleteManifest(_ context.Context, repo, digest string) error {
	f.deleted = append(f.deleted, repo+"@"+digest)
	return nil
}

func TestPruneKeepAndAge(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	api := &fakeRegistryAPI{repos: map[string][]TagInfo{
		"acme": {
			{Tag: "newest", Digest: "d1", Created: now.Add(-1 * time.Hour)},
			{Tag: "mid", Digest: "d2", Created: now.Add(-48 * time.Hour)},
			{Tag: "oldest", Digest: "d3", Created: now.Add(-240 * time.Hour)},
		},
	}}
	gcRun := false
	runner := &scriptRunner{steps: []scriptStep{
		{match: func(name string, args []string) bool {
			gcRun = argHas("garbage-collect")(args)
			return true
		}},
	}}
	h := New(storetestOpen(t), nil, Options{Runner: runner, RegistryClient: api})
	// keep=1(每仓库留最新 1 个)+ age=3 天:mid(48h,排名第2 超量)+ oldest(240h 超龄)删。
	if _, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "ctrl", KeepPerProject: 1, MaxAgeDays: 3}); err != nil {
		t.Fatalf("save: %v", err)
	}
	res, err := h.Prune(context.Background(), now)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !res.OK || res.DeletedTags != 2 || res.Error != "" {
		t.Fatalf("清理结果不符: %+v", res)
	}
	if len(api.deleted) != 2 || api.deleted[0] != "acme@d2" || api.deleted[1] != "acme@d3" {
		t.Fatalf("删除对象不符: %v", api.deleted)
	}
	if !gcRun {
		t.Fatalf("有删除时应触发 garbage-collect")
	}
}

func TestPruneDisabledOrNoPolicy(t *testing.T) {
	api := &fakeRegistryAPI{repos: map[string][]TagInfo{"acme": {{Tag: "t", Digest: "d"}}}}
	h := New(storetestOpen(t), nil, Options{RegistryClient: api})
	if _, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "ctrl"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	res, err := h.Prune(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if res.DeletedTags != 0 || len(api.deleted) != 0 {
		t.Fatalf("无策略不应删除: %+v", res)
	}
}

func TestPruneZeroCreatedRankLast(t *testing.T) {
	// 取不到 created(零值)的 tag 排最后=最旧:keep=1 时它先被删(安全方向)。
	now := time.Now().UTC().Truncate(time.Second)
	api := &fakeRegistryAPI{repos: map[string][]TagInfo{
		"acme": {
			{Tag: "known", Digest: "d1", Created: now.Add(-1 * time.Hour)},
			{Tag: "unknown", Digest: "d2"}, // 零值时间
		},
	}}
	runner := &scriptRunner{}
	h := New(storetestOpen(t), nil, Options{Runner: runner, RegistryClient: api})
	if _, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "ctrl", KeepPerProject: 1}); err != nil {
		t.Fatalf("save: %v", err)
	}
	res, _ := h.Prune(context.Background(), now)
	if res.DeletedTags != 1 || len(api.deleted) != 1 || api.deleted[0] != "acme@d2" {
		t.Fatalf("零值时间应排最旧先删: %+v %v", res, api.deleted)
	}
}

func TestPruneLatestExempt(t *testing.T) {
	// latest 永久豁免(0059 迁移注释承诺):即使超量(keep=1)且超龄(240h>3 天)也不删;
	// 其余 tag 仍按策略清理。
	now := time.Now().UTC().Truncate(time.Second)
	api := &fakeRegistryAPI{repos: map[string][]TagInfo{
		"acme": {
			{Tag: "newest", Digest: "d1", Created: now.Add(-1 * time.Hour)},
			{Tag: "mid", Digest: "d2", Created: now.Add(-48 * time.Hour)},
			{Tag: "latest", Digest: "d3", Created: now.Add(-240 * time.Hour)},
		},
	}}
	runner := &scriptRunner{}
	h := New(storetestOpen(t), nil, Options{Runner: runner, RegistryClient: api})
	if _, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "ctrl", KeepPerProject: 1, MaxAgeDays: 3}); err != nil {
		t.Fatalf("save: %v", err)
	}
	res, err := h.Prune(context.Background(), now)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if res.DeletedTags != 1 || len(api.deleted) != 1 || api.deleted[0] != "acme@d2" {
		t.Fatalf("latest 应豁免、仅删 mid: %+v %v", res, api.deleted)
	}
}

func TestPruneSharedDigestSkipped(t *testing.T) {
	// DELETE manifest 按 digest 生效:待删 tag(old,超龄+超量)与保留 tag(stable,最新)
	// 共享同一 digest 时必须跳过,否则 stable 会连带失效。
	now := time.Now().UTC().Truncate(time.Second)
	api := &fakeRegistryAPI{repos: map[string][]TagInfo{
		"acme": {
			{Tag: "stable", Digest: "dShared", Created: now.Add(-1 * time.Hour)},
			{Tag: "old", Digest: "dShared", Created: now.Add(-240 * time.Hour)},
			{Tag: "gone", Digest: "dGone", Created: now.Add(-200 * time.Hour)},
		},
	}}
	runner := &scriptRunner{}
	h := New(storetestOpen(t), nil, Options{Runner: runner, RegistryClient: api})
	if _, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "ctrl", KeepPerProject: 1, MaxAgeDays: 3}); err != nil {
		t.Fatalf("save: %v", err)
	}
	res, err := h.Prune(context.Background(), now)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	// old 因共享 digest 被跳过;gone 独占 digest 正常删除。
	if res.DeletedTags != 1 || len(api.deleted) != 1 || api.deleted[0] != "acme@dGone" {
		t.Fatalf("共享 digest 应跳过、独占 digest 应删除: %+v %v", res, api.deleted)
	}
}

// deployRunner 是 compose 探测/up 均放行、记录全部命令的 LocalRunner。
func deployRunner() *scriptRunner {
	return &scriptRunner{steps: []scriptStep{
		{match: func(name string, args []string) bool {
			return name == "docker" && len(args) >= 1 && args[0] == "compose" && argHas("version")(args)
		}, out: "v2"},
		{match: func(name string, args []string) bool { return argHas("up")(args) && argHas("-d")(args) }, out: "started"},
	}}
}

func TestDeployStackCustomDataDirsRendered(t *testing.T) {
	base := t.TempDir()
	art, cch := filepath.Join(base, "art"), filepath.Join(base, "cch")
	runner := deployRunner()
	h := newTestHub(t, Options{BaseDir: base, Runner: runner})
	if _, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "ctrl", ArtifactDataDir: art, CacheDataDir: cch}); err != nil {
		t.Fatalf("save: %v", err)
	}
	res, err := h.DeployStack(context.Background())
	if err != nil || !res.OK {
		t.Fatalf("deploy: %v %+v", err, res)
	}
	content, rerr := os.ReadFile(filepath.Join(base, composeFile))
	if rerr != nil {
		t.Fatalf("compose 未落盘: %v", rerr)
	}
	for _, want := range []string{`- "` + art + `:/var/lib/registry"`, `- "` + cch + `:/var/lib/registry"`} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("compose 缺卷行 %q:\n%s", want, content)
		}
	}
	// 自定义目录已建。
	for _, dir := range []string{art, cch} {
		if _, serr := os.Stat(dir); serr != nil {
			t.Fatalf("目录 %s 未创建: %v", dir, serr)
		}
	}
}

func TestDeployStackMigratesArtifactAndClearsCache(t *testing.T) {
	base := t.TempDir()
	// 第一次部署:默认目录(data/cache),并在两个目录里留下数据。
	runner := deployRunner()
	h := newTestHub(t, Options{BaseDir: base, Runner: runner})
	if _, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "ctrl"}); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	if _, err := h.DeployStack(context.Background()); err != nil {
		t.Fatalf("deploy 1: %v", err)
	}
	oldData, oldCache := filepath.Join(base, "data"), filepath.Join(base, "cache")
	newArt, newCache := filepath.Join(base, "art"), filepath.Join(base, "cch")
	if err := os.MkdirAll(filepath.Join(oldData, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldData, "blobs", "x"), []byte("artifact-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(oldCache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldCache, "layer"), []byte("cache"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 第二次部署:两个目录都改 → 制品迁移、缓存清除。
	if _, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "ctrl", ArtifactDataDir: newArt, CacheDataDir: newCache}); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	res, err := h.DeployStack(context.Background())
	if err != nil || !res.OK {
		t.Fatalf("deploy 2: %v %+v", err, res)
	}
	// 制品:旧目录消失,新目录含完整数据。
	if _, serr := os.Stat(oldData); !os.IsNotExist(serr) {
		t.Fatalf("制品旧目录应已移走: %v", serr)
	}
	moved, rerr := os.ReadFile(filepath.Join(newArt, "blobs", "x"))
	if rerr != nil || string(moved) != "artifact-data" {
		t.Fatalf("制品数据应迁移到新目录: %q %v", moved, rerr)
	}
	// 缓存:旧目录整体清除。
	if _, serr := os.Stat(oldCache); !os.IsNotExist(serr) {
		t.Fatalf("缓存旧目录应已清除: %v", serr)
	}
	// 输出应含迁移/清除说明;容器命令应含 rm -f 制品容器。
	if !strings.Contains(res.Output, "已迁移制品数据") || !strings.Contains(res.Output, "已清除缓存旧目录") {
		t.Fatalf("输出缺说明: %q", res.Output)
	}
	joined := ""
	for _, c := range runner.commands {
		joined += strings.Join(c, " ") + "\n"
	}
	if !strings.Contains(joined, "docker rm -f pipewright-registry") || !strings.Contains(joined, "docker rm -f pipewright-registry-cache") {
		t.Fatalf("迁移/清除前应移除对应容器: %s", joined)
	}
	// 新 compose 以新目录渲染。
	content, _ := os.ReadFile(filepath.Join(base, composeFile))
	if !strings.Contains(string(content), newArt) || strings.Contains(string(content), oldCache) {
		t.Fatalf("compose 应指向新目录:\n%s", content)
	}
}

func TestDeployStackArtifactMigrateRefusesNonEmptyTarget(t *testing.T) {
	base := t.TempDir()
	runner := deployRunner()
	h := newTestHub(t, Options{BaseDir: base, Runner: runner})
	if _, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "ctrl"}); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	if _, err := h.DeployStack(context.Background()); err != nil {
		t.Fatalf("deploy 1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "data", "blob"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 新目录预置非空内容 → 拒绝迁移,部署失败且不写新 compose / 不 up。
	newArt := filepath.Join(base, "art")
	if err := os.MkdirAll(newArt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newArt, "stale"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "ctrl", ArtifactDataDir: newArt}); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	res, err := h.DeployStack(context.Background())
	if err != nil {
		t.Fatalf("deploy 2: %v", err)
	}
	if res.OK || !strings.Contains(res.Error, "拒绝迁移") {
		t.Fatalf("非空目标应拒绝迁移: %+v", res)
	}
	// 旧数据原封未动。
	if _, serr := os.Stat(filepath.Join(base, "data", "blob")); serr != nil {
		t.Fatalf("旧数据不得被破坏: %v", serr)
	}
}

func TestDeployStackCacheOldDirEqualsNewArtifactRefuses(t *testing.T) {
	// 旧缓存目录与新制品目录重合:清除会吞掉刚迁入的制品数据 → 必须拒绝。
	base := t.TempDir()
	runner := deployRunner()
	h := newTestHub(t, Options{BaseDir: base, Runner: runner})
	if _, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "ctrl"}); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	if _, err := h.DeployStack(context.Background()); err != nil {
		t.Fatalf("deploy 1: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(base, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 新制品目录 = 旧缓存目录(base/cache);新缓存目录另选。
	if _, err := h.Save(context.Background(), SaveInput{
		Enabled: true, ExternalAddr: "ctrl",
		ArtifactDataDir: filepath.Join(base, "cache"), CacheDataDir: filepath.Join(base, "cch"),
	}); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	runner.commands = nil // 只检查第二次部署的命令
	res, err := h.DeployStack(context.Background())
	if err != nil {
		t.Fatalf("deploy 2: %v", err)
	}
	if res.OK || !strings.Contains(res.Error, "重合") {
		t.Fatalf("缓存旧目录与新制品目录重合应拒绝: %+v", res)
	}
	// compose 未重写(up 未执行),up 命令不应出现在记录里。
	for _, c := range runner.commands {
		if argHas("up")(c) {
			t.Fatalf("失败后不得再 compose up: %v", c)
		}
	}
}

func TestStatusUsesConfiguredDataDirs(t *testing.T) {
	base := t.TempDir()
	art := filepath.Join(base, "art")
	if err := os.MkdirAll(art, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(art, "blob"), []byte(strings.Repeat("x", 2048)), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newTestHub(t, Options{BaseDir: base, Runner: &scriptRunner{}, HTTPClient: &http.Client{Timeout: 300 * time.Millisecond}})
	if _, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "ctrl", ArtifactDataDir: art}); err != nil {
		t.Fatalf("save: %v", err)
	}
	st, err := h.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.DataDirBytes != 2048 {
		t.Fatalf("存储占用应按配置目录统计: %+v", st)
	}
}

func TestResolveBuiltin(t *testing.T) {
	h := newTestHub(t, Options{})
	if addr, ok := h.ResolveBuiltin(context.Background()); ok || addr != "" {
		t.Fatalf("未启用应 false: %q %v", addr, ok)
	}
	if _, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "10.1.1.1", ArtifactPort: 6000}); err != nil {
		t.Fatalf("save: %v", err)
	}
	addr, ok := h.ResolveBuiltin(context.Background())
	if !ok || addr != "10.1.1.1:6000" {
		t.Fatalf("启用后应返回制品地址: %q %v", addr, ok)
	}
}
