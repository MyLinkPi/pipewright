package aptcacherng

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/storetest"
)

// storetestOpen 打开全量迁移的 sqlite 测试库。
func storetestOpen(t *testing.T) *sql.DB {
	t.Helper()
	return storetest.OpenDB(t)
}

// scriptRunner 按命令子串脚本化响应的 LocalRunner(记录全部调用;首个匹配步消费后前进)。
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

func joined(cmds [][]string) string {
	var b strings.Builder
	for _, c := range cmds {
		b.WriteString(strings.Join(c, " "))
		b.WriteByte('\n')
	}
	return b.String()
}

// stubRunner 是无状态 LocalRunner 替身:compose 探测/up/inspect/pull 恒放行(记录全部调用),
// 适配 deploy/clear 这类会多次探测 compose 的流程(scriptRunner 的顺序消费不适用)。
type stubRunner struct {
	commands [][]string
}

func (r *stubRunner) Run(_ context.Context, name string, args []string, _ string) (string, string, int, error) {
	r.commands = append(r.commands, append([]string{name}, args...))
	j := name + " " + strings.Join(args, " ")
	switch {
	case strings.Contains(j, "compose version"):
		return "v2", "", 0, nil
	case strings.Contains(j, " up -d"):
		return "started", "", 0, nil
	case strings.Contains(j, "image inspect"):
		return "sha256:stubstubstub", "", 0, nil
	case strings.Contains(j, "pull"):
		return "", "pulled", 0, nil
	default:
		return "", "", 0, nil
	}
}

func TestRenderComposePlain(t *testing.T) {
	dir := t.TempDir()
	h := New(nil, nil, Options{BaseDir: dir, Runner: &scriptRunner{}})
	cfg := &Config{Enabled: true, ExternalAddr: "ctrl", Port: 3142}
	got := h.renderCompose(cfg)
	for _, want := range []string{
		aptImage, aptCont,
		`"3142:3142"`,
		`"` + filepath.Join(dir, "cache") + `:` + cacheVol + `"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("明文 compose 缺 %q:\n%s", want, got)
		}
	}
	// 明文态不应出现 TLS 前端服务/networks。
	if strings.Contains(got, tlsCont) || strings.Contains(got, "networks:") {
		t.Fatalf("明文 compose 不应含 TLS 前端:\n%s", got)
	}
}

func TestRenderComposeTLS(t *testing.T) {
	dir := t.TempDir()
	h := New(nil, nil, Options{BaseDir: dir, Runner: &scriptRunner{}})
	cfg := &Config{Enabled: true, ExternalAddr: "apt.example.com", Port: 3142, TLSCertID: "cert-1"}
	got := h.renderCompose(cfg)
	// 主体不发布宿主端口,只挂 cache 卷 + 接入栈网络。
	if strings.Contains(got, aptCont+"\n    restart: unless-stopped\n    ports") {
		t.Fatalf("TLS 态主体容器不应发布宿主端口:\n%s", got)
	}
	for _, want := range []string{
		aptImage, aptCont, tlsImage, tlsCont, stackNetwork,
		`"` + h.resolveCertsDir() + `:` + certsVol + `:ro"`,
		`"3142:443"`, // TLS 前端对宿主发布端口
		h.nginxConfHostPath(),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("TLS compose 缺 %q:\n%s", want, got)
		}
	}
	// 主配置必须挂到 /etc/nginx/nginx.conf:官方 nginx 镜像只加载默认主配置,不 include
	// 任何旁路文件,挂到 stream.conf 之类的路径会使整个 TLS 前端配置不生效。
	if !strings.Contains(got, `:/etc/nginx/nginx.conf:ro`) {
		t.Fatalf("nginx 主配置应挂载为容器主配置 nginx.conf:\n%s", got)
	}
	if strings.Contains(got, ":/etc/nginx/stream.conf") {
		t.Fatalf("nginx 配置不应挂到 stream.conf(不会被加载):\n%s", got)
	}
}

func TestRenderNginxStreamConf(t *testing.T) {
	got := renderNginxStreamConf()
	for _, want := range []string{
		"load_module /usr/lib/nginx/modules/ngx_stream_module.so;",
		"stream {",
		"listen 443 ssl;",
		"ssl_certificate " + certPathInCont + ";",
		"ssl_certificate_key " + keyPathInCont + ";",
		aptCont + ":" + contPort,
		"resolver 127.0.0.11",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("nginx stream 配置缺 %q:\n%s", want, got)
		}
	}
}

func TestDeployStackPlainWritesComposeAndUp(t *testing.T) {
	dir := t.TempDir()
	runner := &stubRunner{}
	db := storetestOpen(t)
	h := New(db, nil, Options{BaseDir: dir, Runner: runner})
	ctx := context.Background()
	if _, err := h.Save(ctx, SaveInput{Enabled: true, ExternalAddr: "ctrl", Port: 3142}); err != nil {
		t.Fatalf("save: %v", err)
	}
	res, err := h.DeployStack(ctx)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if !res.OK || res.Error != "" {
		t.Fatalf("部署应成功: %+v", res)
	}
	raw, rerr := os.ReadFile(filepath.Join(dir, composeFile))
	if rerr != nil {
		t.Fatalf("compose 未落盘: %v", rerr)
	}
	if !strings.Contains(string(raw), `"3142:3142"`) {
		t.Fatalf("compose 缺端口映射:\n%s", raw)
	}
	if _, serr := os.Stat(filepath.Join(dir, "cache")); serr != nil {
		t.Fatalf("缓存目录未创建: %v", serr)
	}
	j := joined(runner.commands)
	if !strings.Contains(j, "compose -p "+stackProject+" -f ") || !strings.Contains(j, " up -d") {
		t.Fatalf("compose up 命令不符:\n%s", j)
	}
	// 明文态不写 nginx stream 配置。
	if _, nerr := os.Stat(h.nginxConfHostPath()); nerr == nil {
		t.Fatalf("明文态不应落 nginx 配置")
	}
}

func TestDeployStackTLSWritesNginxAndCerts(t *testing.T) {
	dir := t.TempDir()
	runner := &stubRunner{}
	db := storetestOpen(t)
	h := New(db, nil, Options{BaseDir: dir, Runner: runner, Certs: fakeCerts{}})
	ctx := context.Background()
	if _, err := h.Save(ctx, SaveInput{Enabled: true, ExternalAddr: "apt.example.com", TLSCertID: "cert-1"}); err != nil {
		t.Fatalf("save tls: %v", err)
	}
	res, err := h.DeployStack(ctx)
	if err != nil || !res.OK {
		t.Fatalf("tls 部署应成功: %+v err=%v", res, err)
	}
	// nginx stream 配置已落盘。
	nc, nerr := os.ReadFile(h.nginxConfHostPath())
	if nerr != nil || !strings.Contains(string(nc), "listen 443 ssl;") {
		t.Fatalf("nginx 配置未正确落盘: err=%v content=%q", nerr, nc)
	}
	// 证书 PEM 已落盘(私钥 0600)。
	certsDir := h.resolveCertsDir()
	if _, cerr := os.Stat(filepath.Join(certsDir, certPEMName)); cerr != nil {
		t.Fatalf("fullchain 未落盘: %v", cerr)
	}
	keyPath := filepath.Join(certsDir, certKeyName)
	info, kerr := os.Stat(keyPath)
	if kerr != nil {
		t.Fatalf("privkey 未落盘: %v", kerr)
	}
	// NTFS 不承载 Unix 权限位,0600 断言仅在非 Windows 主机上有意义(生产控制机为 Linux)。
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("privkey 权限应 0600, got %v", info.Mode().Perm())
	}
}

func TestDeployStackDisabled(t *testing.T) {
	db := storetestOpen(t)
	h := New(db, nil, Options{BaseDir: t.TempDir(), Runner: &scriptRunner{}})
	if _, err := h.DeployStack(context.Background()); err != ErrDisabled {
		t.Fatalf("未启用应 ErrDisabled, got %v", err)
	}
}

// downRunner 恒返回非零退出码(模拟无 docker/compose 的机器)。
type downRunner struct{}

func (downRunner) Run(_ context.Context, _ string, _ []string, _ string) (string, string, int, error) {
	return "", "", 1, nil
}

func TestDeployStackNoLocalDocker(t *testing.T) {
	db := storetestOpen(t)
	h := New(db, nil, Options{BaseDir: t.TempDir(), Runner: downRunner{}})
	if _, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "ctrl"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// compose 探测两次(docker compose / docker-compose)均非零 → ErrNoLocalDocker。
	if _, err := h.DeployStack(context.Background()); err != ErrNoLocalDocker {
		t.Fatalf("无 compose 应 ErrNoLocalDocker, got %v", err)
	}
}

func TestDeployCacheDirChangeClearsOldDir(t *testing.T) {
	dir := t.TempDir()
	db := storetestOpen(t)
	runner := &stubRunner{}
	h := New(db, nil, Options{BaseDir: dir, Runner: runner})
	ctx := context.Background()
	// 首次部署到默认目录。
	if _, err := h.Save(ctx, SaveInput{Enabled: true, ExternalAddr: "ctrl"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := h.DeployStack(ctx); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	oldCache := filepath.Join(dir, "cache")
	if mkErr := os.MkdirAll(filepath.Join(oldCache, "sub"), 0o755); mkErr != nil {
		t.Fatal(mkErr)
	}
	if wErr := os.WriteFile(filepath.Join(oldCache, "pkg"), []byte("x"), 0o644); wErr != nil {
		t.Fatal(wErr)
	}
	// 换缓存目录再部署 → 旧目录应被清除(可再生,不迁移)。
	newCache := filepath.Join(dir, "cache2")
	if _, err := h.Save(ctx, SaveInput{Enabled: true, ExternalAddr: "ctrl", DataDir: newCache}); err != nil {
		t.Fatalf("save new dir: %v", err)
	}
	res, err := h.DeployStack(ctx)
	if err != nil || !res.OK {
		t.Fatalf("redeploy: err=%v res=%+v", err, res)
	}
	if _, serr := os.Stat(oldCache); !os.IsNotExist(serr) {
		t.Fatalf("旧缓存目录应被清除, got stat err=%v", serr)
	}
	if !strings.Contains(res.Output, "已清除缓存旧目录") {
		t.Fatalf("输出应含清除说明: %q", res.Output)
	}
}

func TestClearCacheRemovesDirAndRebuilds(t *testing.T) {
	dir := t.TempDir()
	db := storetestOpen(t)
	runner := &stubRunner{}
	h := New(db, nil, Options{BaseDir: dir, Runner: runner})
	ctx := context.Background()
	if _, err := h.Save(ctx, SaveInput{Enabled: true, ExternalAddr: "ctrl"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	cacheDir := filepath.Join(dir, "cache")
	if mkErr := os.MkdirAll(cacheDir, 0o755); mkErr != nil {
		t.Fatal(mkErr)
	}
	if wErr := os.WriteFile(filepath.Join(cacheDir, "f"), []byte("data"), 0o644); wErr != nil {
		t.Fatal(wErr)
	}
	res, err := h.ClearCache(ctx)
	if err != nil || !res.OK {
		t.Fatalf("clear: err=%v res=%+v", err, res)
	}
	if _, serr := os.Stat(cacheDir); serr == nil {
		// ClearCache 走 DeployStack(MkdirAll)会重建空目录;这里验证内部文件已被清空。
		entries, _ := os.ReadDir(cacheDir)
		if len(entries) != 0 {
			t.Fatalf("缓存文件应清空, still: %+v", entries)
		}
	}
	// 应有一次 docker rm -f 主体容器。
	if !strings.Contains(joined(runner.commands), "rm -f "+aptCont) {
		t.Fatalf("应先停删主体容器:\n%s", joined(runner.commands))
	}
}

func TestClearCacheDisabled(t *testing.T) {
	db := storetestOpen(t)
	h := New(db, nil, Options{BaseDir: t.TempDir(), Runner: &scriptRunner{}})
	if _, err := h.ClearCache(context.Background()); err != ErrDisabled {
		t.Fatalf("未启用应 ErrDisabled, got %v", err)
	}
}

// upgradeRunner 模拟镜像升级链:compose 探测/pull 放行;image inspect 按调用次序返回
// beforeID(首次)、afterID(其后)。afterChanged=false 时两者相同。
type upgradeRunner struct {
	beforeID string
	afterID  string
	inspects int
	upRan    bool
}

func (r *upgradeRunner) Run(_ context.Context, name string, args []string, _ string) (string, string, int, error) {
	j := name + " " + strings.Join(args, " ")
	switch {
	case strings.Contains(j, "compose version"):
		return "v2", "", 0, nil
	case strings.Contains(j, " up -d"):
		r.upRan = true
		return "started", "", 0, nil
	case strings.Contains(j, "image inspect"):
		r.inspects++
		if r.inspects >= 2 {
			return r.afterID, "", 0, nil
		}
		return r.beforeID, "", 0, nil
	case strings.Contains(j, "pull"):
		return "", "pulled", 0, nil
	default:
		return "", "", 0, nil
	}
}

func TestUpgradeImageChangedRebuilds(t *testing.T) {
	dir := t.TempDir()
	db := storetestOpen(t)
	runner := &upgradeRunner{beforeID: "sha256:oldoldold", afterID: "sha256:newnewnew"}
	h := New(db, nil, Options{BaseDir: dir, Runner: runner})
	ctx := context.Background()
	if _, err := h.Save(ctx, SaveInput{Enabled: true, ExternalAddr: "ctrl"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	res, err := h.UpgradeImage(ctx)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if !res.OK || !res.Changed {
		t.Fatalf("镜像变了应 ok+changed: %+v", res)
	}
	if res.Before != "oldoldold" || res.After != "newnewnew" {
		t.Fatalf("前后 ID 应截短回显: %+v", res)
	}
	if !runner.upRan {
		t.Fatalf("变了应 compose up 重建")
	}
}

func TestUpgradeImageUnchangedNoRebuild(t *testing.T) {
	dir := t.TempDir()
	db := storetestOpen(t)
	const sameID = "sha256:samesamesame"
	runner := &upgradeRunner{beforeID: sameID, afterID: sameID}
	h := New(db, nil, Options{BaseDir: dir, Runner: runner})
	ctx := context.Background()
	if _, err := h.Save(ctx, SaveInput{Enabled: true, ExternalAddr: "ctrl"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	res, err := h.UpgradeImage(ctx)
	if err != nil || !res.OK || res.Changed {
		t.Fatalf("未变应 ok + changed=false: %+v err=%v", res, err)
	}
	if runner.upRan {
		t.Fatalf("未变不应 compose up")
	}
}

// tlsUpgradeRunner 按镜像名脚本化 image inspect(每镜像首查返回旧 ID、其后返回新 ID),
// 用于 TLS 态双镜像的升级链。
type tlsUpgradeRunner struct {
	inspected map[string]int
	upRan     bool
}

func (r *tlsUpgradeRunner) Run(_ context.Context, name string, args []string, _ string) (string, string, int, error) {
	j := name + " " + strings.Join(args, " ")
	switch {
	case strings.Contains(j, "compose version"):
		return "v2", "", 0, nil
	case strings.Contains(j, " up -d"):
		r.upRan = true
		return "started", "", 0, nil
	case strings.Contains(j, "image inspect"):
		image := args[len(args)-1]
		if r.inspected == nil {
			r.inspected = map[string]int{}
		}
		r.inspected[image]++
		if r.inspected[image] >= 2 {
			return "sha256:new" + strings.ReplaceAll(image, "/", ""), "", 0, nil
		}
		return "sha256:old" + strings.ReplaceAll(image, "/", ""), "", 0, nil
	case strings.Contains(j, "pull"):
		return "", "pulled", 0, nil
	default:
		return "", "", 0, nil
	}
}

// TLS 态仅 nginx 前端镜像更新也应触发重建:比对若只看主体镜像,Changed=false 会跳过
// DeployStack,新前端镜像永远不生效。
func TestUpgradeImageTLSFrontOnlyChangedRebuilds(t *testing.T) {
	dir := t.TempDir()
	db := storetestOpen(t)
	runner := &tlsUpgradeRunner{}
	h := New(db, nil, Options{BaseDir: dir, Runner: runner, Certs: fakeCerts{}})
	ctx := context.Background()
	if _, err := h.Save(ctx, SaveInput{Enabled: true, ExternalAddr: "apt.example.com", TLSCertID: "cert-1"}); err != nil {
		t.Fatalf("save tls: %v", err)
	}
	res, err := h.UpgradeImage(ctx)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if !res.OK || !res.Changed {
		t.Fatalf("前端镜像变了应 ok+changed: %+v", res)
	}
	if !runner.upRan {
		t.Fatalf("前端镜像变了应 compose up 重建")
	}
	if !strings.Contains(res.Before, ",") || !strings.Contains(res.After, ",") {
		t.Fatalf("TLS 态 Before/After 应含双镜像 ID: %+v", res)
	}
	if res.Before == res.After {
		t.Fatalf("前后镜像 ID 应不同: %+v", res)
	}
}

func TestParseComposeCacheDir(t *testing.T) {
	plain := `services:
  apt-cacher-ng:
    volumes:
      - "/data/apt-cache:/var/cache/apt-cacher-ng"
`
	if got := parseComposeCacheDir(plain); got != "/data/apt-cache" {
		t.Fatalf("明文解析 = %q", got)
	}
	tls := `services:
  apt-cacher-ng:
    volumes:
      - "/srv/cache:/var/cache/apt-cacher-ng"
  apt-cacher-tls:
    volumes:
      - "/certs:/certs:ro"
`
	if got := parseComposeCacheDir(tls); got != "/srv/cache" {
		t.Fatalf("TLS 态应取 cache 卷 = %q", got)
	}
	if got := parseComposeCacheDir("no volumes here"); got != "" {
		t.Fatalf("无卷应空串, got %q", got)
	}
}

func TestDirSizeAndTail(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := dirSize(dir); got != 5 {
		t.Fatalf("dirSize = %d, want 5", got)
	}
	long := strings.Repeat("x", 100)
	if got := tail(long, 10); !strings.HasPrefix(got, "…") || len(got) != 3+10 { // 省略号(3 字节)+ 尾部 10 字节
		t.Fatalf("tail 长度/前缀不符: %q", got)
	}
	if got := tail("short", 10); got != "short" {
		t.Fatalf("短串应原样: %q", got)
	}
}
