package pveadmin

// aptrepo_test.go — 仓库管理:双格式解析(fixture)、镜像/订阅/suite 变换、
// 备份→原子写序列(fakeRunner)、路径白名单、Ceph 只读、批量应用。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/target"
)

const listFixture = `# 主源
deb http://deb.debian.org/debian bookworm main contrib
deb http://security.debian.org/debian-security bookworm-security main
# deb http://download.proxmox.com/debian/pve bookworm pve-no-subscription
deb [arch=amd64 signed-by=/usr/share/keyrings/proxmox-archive-keyring.gpg] https://enterprise.proxmox.com/debian/pve bookworm pve-enterprise
deb https://enterprise.proxmox.com/debian/ceph-squid bookworm enterprise
# 普通注释,不是仓库行
`

const sourcesFixture = `Types: deb
URIs: http://deb.debian.org/debian
Suites: bookworm bookworm-updates
Components: main contrib
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg

Types: deb
URIs: https://enterprise.proxmox.com/debian/pve
Suites: bookworm
Components: pve-enterprise
Enabled: no
`

func TestParseListFile(t *testing.T) {
	f := parseListFile("/etc/apt/sources.list", strings.Split(listFixture, "\n"))
	if len(f.entries) != 5 {
		t.Fatalf("entries = %d, want 5", len(f.entries))
	}
	// 第 3 行是被注释的 no-subscription(禁用条目)。
	e := f.findEntry("4")
	if e == nil || e.enabled || e.kind != "pve" || e.enterprise {
		t.Fatalf("disabled pve entry = %+v", e)
	}
	// 第 5 行带方括号选项的企业源。
	e = f.findEntry("5")
	if e == nil || !e.enabled || !e.enterprise || e.signedBy == "" {
		t.Fatalf("enterprise entry = %+v", e)
	}
	// 第 6 行 ceph。
	e = f.findEntry("6")
	if e == nil || e.kind != "ceph" {
		t.Fatalf("ceph entry = %+v", e)
	}
	// 普通注释不成为条目。
	if f.findEntry("7") != nil {
		t.Fatal("plain comment became entry")
	}
}

func TestParseSourcesFile(t *testing.T) {
	f := parseSourcesFile("/etc/apt/sources.list.d/debian.sources", strings.Split(sourcesFixture, "\n"))
	if len(f.entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(f.entries))
	}
	if f.entries[0].kind != "debian" || !f.entries[0].enabled || len(f.entries[0].suites) != 2 {
		t.Fatalf("stanza 1 = %+v", f.entries[0])
	}
	if f.entries[1].kind != "pve" || f.entries[1].enabled || !f.entries[1].enterprise {
		t.Fatalf("stanza 2 = %+v", f.entries[1])
	}
}

func TestParseAptDump(t *testing.T) {
	dump := "##PWFILE:/etc/apt/sources.list\n" + listFixture + "\n##PWEND\n" +
		"##PWFILE:/etc/apt/sources.list.d/pve.sources\n" + sourcesFixture + "\n##PWEND\n" +
		"##PWFILE:/etc/passwd\nroot:x:0:0\n##PWEND\n"
	files, err := parseAptDump(dump)
	if err != nil {
		t.Fatalf("parseAptDump: %v", err)
	}
	if len(files) != 2 { // /etc/passwd 白名单外丢弃
		t.Fatalf("files = %d, want 2", len(files))
	}
	if files[0].format != "list" || files[1].format != "sources" {
		t.Fatalf("formats = %s/%s", files[0].format, files[1].format)
	}
}

func TestRewriteAptURI(t *testing.T) {
	ustc := aptMirrorPresets["ustc"]
	tsinghua := aptMirrorPresets["tsinghua"]
	official := aptMirrorPresets["official"]
	aliyun := aptMirrorPresets["aliyun"]
	main := []string{"bookworm"}
	sec := []string{"bookworm-security"}
	cases := []struct {
		uri, kind  string
		enterprise bool
		p          aptMirrorPreset
		suites     []string
		want       string
	}{
		{"http://deb.debian.org/debian", "debian", false, ustc, main, "https://mirrors.ustc.edu.cn/debian"},
		{"http://security.debian.org/debian-security", "debian", false, ustc, sec, "https://mirrors.ustc.edu.cn/debian-security"},
		{"https://mirrors.ustc.edu.cn/debian", "debian", false, tsinghua, main, "https://mirrors.tuna.tsinghua.edu.cn/debian"},
		{"https://mirrors.ustc.edu.cn/debian-security", "debian", false, official, sec, "https://security.debian.org/debian-security"},
		{"https://mirrors.ustc.edu.cn/debian", "debian", false, official, main, "https://deb.debian.org/debian"},
		{"http://download.proxmox.com/debian/pve", "pve", false, ustc, main, "https://mirrors.ustc.edu.cn/proxmox/debian/pve"},
		{"https://mirrors.ustc.edu.cn/proxmox/debian/pve", "pve", false, tsinghua, main, "https://mirrors.tuna.tsinghua.edu.cn/proxmox/debian/pve"},
		{"https://mirrors.ustc.edu.cn/proxmox/debian/pve", "pve", false, official, main, "https://download.proxmox.com/debian/pve"},
		// 企业源不被镜像改写;阿里云 PVE 源不动;ceph 不动。
		{"https://enterprise.proxmox.com/debian/pve", "pve", true, ustc, main, "https://enterprise.proxmox.com/debian/pve"},
		{"http://download.proxmox.com/debian/pve", "pve", false, aliyun, main, "http://download.proxmox.com/debian/pve"},
		{"http://deb.debian.org/debian", "debian", false, aliyun, main, "https://mirrors.aliyun.com/debian"},
		{"https://enterprise.proxmox.com/debian/ceph-squid", "ceph", false, ustc, main, "https://enterprise.proxmox.com/debian/ceph-squid"},
		// 路径规范化回归:原始 security 行路径缺失/非标准,换源后仍补全 /debian-security。
		{"http://security.debian.org", "debian", false, ustc, sec, "https://mirrors.ustc.edu.cn/debian-security"},
		{"http://deb.debian.org", "debian", false, ustc, sec, "https://mirrors.ustc.edu.cn/debian-security"},
		{"http://security.debian.org/", "debian", false, ustc, sec, "https://mirrors.ustc.edu.cn/debian-security"},
		{"http://deb.debian.org/", "debian", false, ustc, main, "https://mirrors.ustc.edu.cn/debian"},
		// security suite 旧式(buster/updates)与滚动别名(stable-security)均归 /debian-security。
		{"http://security.debian.org/debian-security", "debian", false, ustc, []string{"buster/updates"}, "https://mirrors.ustc.edu.cn/debian-security"},
		{"http://deb.debian.org/debian-security", "debian", false, ustc, []string{"stable-security"}, "https://mirrors.ustc.edu.cn/debian-security"},
		// 已镜像但 security 路径损坏 → 原地规范化(changed=true)。
		{"https://mirrors.ustc.edu.cn", "debian", false, ustc, sec, "https://mirrors.ustc.edu.cn/debian-security"},
		// Ubuntu 源路径:常规 /ubuntu、ports /ubuntu-ports 不被误改(回归防护)。
		{"http://archive.ubuntu.com/ubuntu", "debian", false, ustc, []string{"jammy"}, "https://mirrors.ustc.edu.cn/ubuntu"},
		{"http://security.ubuntu.com/ubuntu", "debian", false, ustc, []string{"jammy-security"}, "https://mirrors.ustc.edu.cn/ubuntu"},
		{"http://ports.ubuntu.com/ubuntu-ports", "debian", false, ustc, []string{"jammy"}, "https://mirrors.ustc.edu.cn/ubuntu-ports"},
		{"https://mirrors.ustc.edu.cn/ubuntu-ports", "debian", false, official, []string{"jammy"}, "https://ports.ubuntu.com/ubuntu-ports"},
		{"https://mirrors.ustc.edu.cn/ubuntu", "debian", false, official, []string{"jammy"}, "https://archive.ubuntu.com/ubuntu"},
		{"https://mirrors.ustc.edu.cn/ubuntu", "debian", false, official, []string{"jammy-security"}, "https://security.ubuntu.com/ubuntu"},
	}
	for _, c := range cases {
		got, _ := rewriteAptURI(c.uri, c.kind, c.enterprise, c.p, c.suites)
		if got != c.want {
			t.Errorf("rewrite(%s, %s, ent=%v, suites=%v) = %s, want %s", c.uri, c.kind, c.enterprise, c.suites, got, c.want)
		}
	}
}

// TestApplyMirrorFixesBrokenSecurityPath 端到端:原始 security 行裸主机(缺路径),
// 镜像换源后产出规范 /debian-security URL(复现并修复用户报告的 404)。
func TestApplyMirrorFixesBrokenSecurityPath(t *testing.T) {
	src := "deb http://deb.debian.org/debian bookworm main contrib\n" +
		"deb http://security.debian.org bookworm-security main contrib\n"
	f := parseListFile("/etc/apt/sources.list", strings.Split(src, "\n"))
	if !f.applyMirror(aptMirrorPresets["ustc"]) {
		t.Fatal("applyMirror no change")
	}
	out := f.render()
	if !strings.Contains(out, "deb https://mirrors.ustc.edu.cn/debian-security bookworm-security main contrib") {
		t.Fatalf("security 路径未规范化:\n%s", out)
	}
	if strings.Contains(out, "mirrors.ustc.edu.cn bookworm-security") {
		t.Fatalf("仍残留裸主机 security:\n%s", out)
	}
}

func TestApplySuite(t *testing.T) {
	f := parseListFile("/etc/apt/sources.list", strings.Split(listFixture, "\n"))
	if !f.applySuite("bookworm", "trixie") {
		t.Fatal("applySuite no change")
	}
	out := f.render()
	// debian/pve 条目 suite 已切 trixie;ceph 条目不被 PVE 升级触碰(合法保留 bookworm)。
	if strings.Contains(out, "deb http://deb.debian.org/debian bookworm") ||
		strings.Contains(out, "/debian/pve bookworm") || strings.Contains(out, "bookworm-security") {
		t.Fatalf("debian/pve bookworm 残留:\n%s", out)
	}
	if !strings.Contains(out, "trixie-security") || !strings.Contains(out, "deb http://deb.debian.org/debian trixie main") {
		t.Fatalf("suite 替换不正确:\n%s", out)
	}
	// URI/组件不被误改。
	if !strings.Contains(out, "pve-no-subscription") || !strings.Contains(out, "/debian/pve") {
		t.Fatalf("URI/组件被误改:\n%s", out)
	}
	// deb822。
	fs := parseSourcesFile("/etc/apt/sources.list.d/x.sources", strings.Split(sourcesFixture, "\n"))
	if !fs.applySuite("bookworm", "trixie") {
		t.Fatal("sources applySuite no change")
	}
	if !strings.Contains(fs.render(), "Suites: trixie trixie-updates") {
		t.Fatalf("sources suite 替换不正确:\n%s", fs.render())
	}
	// 幂等:再跑一次无变化。
	if f.applySuite("bookworm", "trixie") {
		t.Fatal("applySuite 应幂等")
	}
}

func TestToggleEntry(t *testing.T) {
	f := parseListFile("/etc/apt/sources.list", strings.Split(listFixture, "\n"))
	e := f.findEntry("4") // 禁用的 no-subscription
	f.toggleEntry(e, true)
	if !strings.Contains(f.render(), "\ndeb http://download.proxmox.com/debian/pve bookworm pve-no-subscription") {
		t.Fatalf("启用后:\n%s", f.render())
	}
	e2 := f.findEntry("2")
	f.toggleEntry(e2, false)
	if !strings.Contains(f.render(), "# deb http://deb.debian.org/debian bookworm main") {
		t.Fatalf("禁用后:\n%s", f.render())
	}
	// deb822:Enabled 字段替换与插入。
	fs := parseSourcesFile("/etc/apt/sources.list.d/x.sources", strings.Split(sourcesFixture, "\n"))
	fs.toggleEntry(&fs.entries[0], false)
	if !strings.Contains(fs.render(), "Enabled: no") {
		t.Fatalf("sources 禁用:\n%s", fs.render())
	}
	fs.toggleEntry(&fs.entries[1], true)
	if !strings.Contains(fs.render(), "Enabled: yes") {
		t.Fatalf("sources 启用:\n%s", fs.render())
	}
}

func TestApplySubscriptionPreset(t *testing.T) {
	f := parseListFile("/etc/apt/sources.list", strings.Split(listFixture, "\n"))
	// 切到 no-subscription:企业源禁用、公共源启用。
	if !f.applySubscriptionPreset(false) {
		t.Fatal("preset no change")
	}
	out := f.render()
	if !strings.Contains(out, "# deb [arch=amd64") && !strings.Contains(out, "# deb https://enterprise.proxmox.com/debian/pve") {
		t.Fatalf("企业源未禁用:\n%s", out)
	}
	if !strings.Contains(out, "\ndeb http://download.proxmox.com/debian/pve bookworm pve-no-subscription") {
		t.Fatalf("公共源未启用:\n%s", out)
	}
	// ceph 条目不受影响。
	if !strings.Contains(out, "\ndeb https://enterprise.proxmox.com/debian/ceph-squid bookworm enterprise") {
		t.Fatalf("ceph 被误改:\n%s", out)
	}
}

// ---- 领域流(fakeRunner 路由 dump/os-release/apt) ----------------------------------------

// wireAptRepoFake 接仓库管理 SSH fixture(dump 脚本/os-release/apt-get/cp/mv)。
func wireAptRepoFake(fr *fakeRunner, dump string) {
	fr.runFn = func(_ string, cmd []string) (*target.ExecResult, error) {
		joined := strings.Join(cmd, " ")
		switch {
		case strings.Contains(joined, "##PWFILE"):
			return &target.ExecResult{ExitCode: 0, Stdout: dump}, nil
		case strings.Contains(joined, "/etc/os-release"):
			return &target.ExecResult{ExitCode: 0, Stdout: "ID=debian\nVERSION_CODENAME=bookworm\n"}, nil
		default:
			return &target.ExecResult{ExitCode: 0}, nil
		}
	}
}

func aptDumpOf(files map[string]string) string {
	var b strings.Builder
	for path, content := range files {
		b.WriteString("##PWFILE:" + path + "\n")
		b.WriteString(content)
		b.WriteString("\n##PWEND\n")
	}
	return b.String()
}

func TestToggleAptRepoFlow(t *testing.T) {
	s, ft, fr, _ := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	dump := aptDumpOf(map[string]string{"/etc/apt/sources.list": listFixture})
	wireAptRepoFake(fr, dump)

	// 启用第 4 行(no-subscription)。
	res, err := s.ToggleAptRepo(context.Background(), cl.ID, "pve1", AptRepoToggleInput{
		Path: "/etc/apt/sources.list", Key: "4", Enabled: true,
	})
	if err != nil {
		t.Fatalf("ToggleAptRepo: %v", err)
	}
	if !res.OK || !res.UpdateOK || len(res.Backups) != 1 {
		t.Fatalf("res = %+v", res)
	}
	// 序列:cp -a 备份 → WriteFile tmp → mv -f → apt-get update。
	var sawBackup, sawMv, sawUpdate bool
	for _, run := range fr.runs {
		joined := strings.Join(run, " ")
		if strings.HasPrefix(joined, "cp -a /etc/apt/sources.list /etc/apt/sources.list.bak.pipewright.") {
			sawBackup = true
		}
		if joined == "mv -f /etc/apt/sources.list.pw-tmp /etc/apt/sources.list" {
			sawMv = true
		}
		if joined == "apt-get update" {
			sawUpdate = true
		}
	}
	if !sawBackup || !sawMv || !sawUpdate {
		t.Fatalf("序列缺步: backup=%v mv=%v update=%v runs=%v", sawBackup, sawMv, sawUpdate, fr.runs)
	}
	if len(fr.writes) != 1 || fr.writes[0].path != "/etc/apt/sources.list.pw-tmp" {
		t.Fatalf("writes = %+v", fr.writes)
	}
	if !strings.Contains(fr.writes[0].content, "\ndeb http://download.proxmox.com/debian/pve bookworm pve-no-subscription") {
		t.Fatalf("写入内容不正确:\n%s", fr.writes[0].content)
	}
}

func TestToggleAptRepoGuards(t *testing.T) {
	s, ft, fr, _ := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireAptRepoFake(fr, aptDumpOf(map[string]string{"/etc/apt/sources.list": listFixture}))
	ctx := context.Background()

	// Ceph 条目只读(第 6 行)。
	_, err := s.ToggleAptRepo(ctx, cl.ID, "pve1", AptRepoToggleInput{Path: "/etc/apt/sources.list", Key: "6", Enabled: false})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("ceph toggle = %v, want ErrInvalidInput", err)
	}
	// 路径白名单。
	_, err = s.ToggleAptRepo(ctx, cl.ID, "pve1", AptRepoToggleInput{Path: "/etc/passwd", Key: "1", Enabled: true})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("path whitelist = %v", err)
	}
	// 只读闸门。
	if err := s.SetReadonly(ctx, cl.ID, true); err != nil {
		t.Fatalf("lock: %v", err)
	}
	_, err = s.ToggleAptRepo(ctx, cl.ID, "pve1", AptRepoToggleInput{Path: "/etc/apt/sources.list", Key: "4", Enabled: true})
	if !errors.Is(err, ErrReadonly) {
		t.Fatalf("readonly = %v", err)
	}
}

func TestSetAptRepoMirrorCreatesNoChanges(t *testing.T) {
	s, ft, fr, _ := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	// 已是完整规范配置(ustc:main/updates/backports/security + PVE 非订阅)→ 再切无变化。
	dump := aptDumpOf(map[string]string{
		"/etc/apt/sources.list": "deb https://mirrors.ustc.edu.cn/debian bookworm main\n" +
			"deb https://mirrors.ustc.edu.cn/debian bookworm-updates main\n" +
			"deb https://mirrors.ustc.edu.cn/debian bookworm-backports main\n" +
			"deb https://mirrors.ustc.edu.cn/debian-security bookworm-security main\n",
		"/etc/apt/sources.list.d/pve-no-subscription.list": "deb https://mirrors.ustc.edu.cn/proxmox/debian/pve bookworm pve-no-subscription\n",
	})
	wireAptRepoFake(fr, dump)
	res, err := s.SetAptRepoMirror(context.Background(), cl.ID, "pve1", "ustc")
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if !res.OK || len(fr.writes) != 0 {
		t.Fatalf("res=%+v writes=%d", res, len(fr.writes))
	}
}

// TestApplyMirrorEnsuresPveAndBackports 只有 debian 三件套、无 PVE 源 → 切镜像后补建
// PVE 非订阅源(镜像基址)与 backports 行(组件沿用主源),对齐 pvetools 的完整配置语义。
func TestApplyMirrorEnsuresPveAndBackports(t *testing.T) {
	s, ft, fr, _ := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	dump := aptDumpOf(map[string]string{"/etc/apt/sources.list": "deb http://deb.debian.org/debian bookworm main contrib\n" +
		"deb http://deb.debian.org/debian bookworm-updates main contrib\n" +
		"deb http://security.debian.org/debian-security bookworm-security main contrib\n"})
	wireAptRepoFake(fr, dump)
	res, err := s.SetAptRepoMirror(context.Background(), cl.ID, "pve1", "ustc")
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if !res.OK || !res.UpdateOK {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.Note, "已补建 PVE 非订阅源") || !strings.Contains(res.Note, "已补建 backports 源") {
		t.Fatalf("note = %q", res.Note)
	}
	var pveFile, mainFile string
	for _, w := range fr.writes {
		if strings.HasSuffix(w.path, "/pve-no-subscription.list.pw-tmp") {
			pveFile = w.content
		}
		if w.path == "/etc/apt/sources.list.pw-tmp" {
			mainFile = w.content
		}
	}
	if pveFile == "" {
		t.Fatal("未创建 pve-no-subscription.list")
	}
	if !strings.Contains(pveFile, "deb https://mirrors.ustc.edu.cn/proxmox/debian/pve bookworm pve-no-subscription") {
		t.Fatalf("PVE 源内容不正确:\n%s", pveFile)
	}
	if mainFile == "" {
		t.Fatal("未写入 sources.list")
	}
	if !strings.Contains(mainFile, "deb https://mirrors.ustc.edu.cn/debian bookworm-backports main contrib") {
		t.Fatalf("backports 未补建/组件不对:\n%s", mainFile)
	}
	if !strings.Contains(mainFile, "deb https://mirrors.ustc.edu.cn/debian-security bookworm-security main contrib") {
		t.Fatalf("security 行不正确:\n%s", mainFile)
	}
}

// TestApplyMirrorEnablesDisabledPve 已有禁用的 PVE 非订阅条目 → 切镜像后启用并改写为
// 镜像基址;不重复新建源文件。
func TestApplyMirrorEnablesDisabledPve(t *testing.T) {
	s, ft, fr, _ := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	dump := aptDumpOf(map[string]string{"/etc/apt/sources.list": "deb http://deb.debian.org/debian bookworm main\n" +
		"# deb http://download.proxmox.com/debian/pve bookworm pve-no-subscription\n"})
	wireAptRepoFake(fr, dump)
	res, err := s.SetAptRepoMirror(context.Background(), cl.ID, "pve1", "ustc")
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if !strings.Contains(res.Note, "已启用原禁用的 PVE 非订阅源") {
		t.Fatalf("note = %q", res.Note)
	}
	for _, w := range fr.writes {
		if strings.Contains(w.path, "pve-no-subscription.list") {
			t.Fatalf("不应新建源文件: %s", w.path)
		}
		if w.path == "/etc/apt/sources.list.pw-tmp" &&
			!strings.Contains(w.content, "\ndeb https://mirrors.ustc.edu.cn/proxmox/debian/pve bookworm pve-no-subscription") {
			t.Fatalf("PVE 条目未启用/未改写:\n%s", w.content)
		}
	}
}

// TestApplyMirrorAliyunCreatesOfficialPve 阿里云无 proxmox 镜像:PVE 源补建为官方,
// backports 补建到阿里云 Debian。
func TestApplyMirrorAliyunCreatesOfficialPve(t *testing.T) {
	s, ft, fr, _ := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	dump := aptDumpOf(map[string]string{"/etc/apt/sources.list": "deb http://deb.debian.org/debian bookworm main\n"})
	wireAptRepoFake(fr, dump)
	res, err := s.SetAptRepoMirror(context.Background(), cl.ID, "pve1", "aliyun")
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if !res.OK || !res.UpdateOK {
		t.Fatalf("res = %+v", res)
	}
	var pveFile, mainFile string
	for _, w := range fr.writes {
		if strings.HasSuffix(w.path, "/pve-no-subscription.list.pw-tmp") {
			pveFile = w.content
		}
		if w.path == "/etc/apt/sources.list.pw-tmp" {
			mainFile = w.content
		}
	}
	if !strings.Contains(pveFile, "deb http://download.proxmox.com/debian/pve bookworm pve-no-subscription") {
		t.Fatalf("阿里云应补建官方 PVE 源:\n%s", pveFile)
	}
	if !strings.Contains(mainFile, "deb https://mirrors.aliyun.com/debian bookworm-backports main") {
		t.Fatalf("backports 应补建到阿里云 Debian:\n%s", mainFile)
	}
}

func TestSetAptRepoPresetCreatesFile(t *testing.T) {
	s, ft, fr, _ := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	// 只有 debian 源,无 pve 源 → 切 no-subscription 应创建标准文件。
	dump := aptDumpOf(map[string]string{"/etc/apt/sources.list": "deb http://deb.debian.org/debian bookworm main\n"})
	wireAptRepoFake(fr, dump)
	res, err := s.SetAptRepoPreset(context.Background(), cl.ID, "pve1", "no-subscription")
	if err != nil {
		t.Fatalf("preset: %v", err)
	}
	if len(res.Created) != 1 || res.Created[0] != "/etc/apt/sources.list.d/pve-no-subscription.list" {
		t.Fatalf("created = %v", res.Created)
	}
	found := false
	for _, w := range fr.writes {
		if w.path == "/etc/apt/sources.list.d/pve-no-subscription.list.pw-tmp" &&
			strings.Contains(w.content, "deb http://download.proxmox.com/debian/pve bookworm pve-no-subscription") {
			found = true
		}
	}
	if !found {
		t.Fatalf("新建文件内容不正确: %+v", fr.writes)
	}
}

func TestRestoreAptRepoWhitelist(t *testing.T) {
	s, ft, fr, _ := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireAptRepoFake(fr, aptDumpOf(map[string]string{"/etc/apt/sources.list": listFixture}))
	ctx := context.Background()
	// 备份名不合规。
	_, err := s.RestoreAptRepo(ctx, cl.ID, "pve1", "/etc/apt/sources.list", "/etc/apt/sources.list.bak")
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("backup whitelist = %v", err)
	}
	// 备份与目标不对应。
	_, err = s.RestoreAptRepo(ctx, cl.ID, "pve1", "/etc/apt/sources.list", "/etc/apt/sources.list.d/x.list.bak.pipewright.20260101T000000Z")
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("backup mismatch = %v", err)
	}
	// 合法恢复。
	res, err := s.RestoreAptRepo(ctx, cl.ID, "pve1", "/etc/apt/sources.list", "/etc/apt/sources.list.bak.pipewright.20260101T000000Z")
	if err != nil || !res.OK {
		t.Fatalf("restore = %+v / %v", res, err)
	}
}

func TestApplyAptReposBatch(t *testing.T) {
	s, ft, fr, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	// 第二个节点。
	poolID, _, _ := s.ensurePool(context.Background(), cl.ID, cl.Name)
	ft.servers["node2"] = &target.Server{ID: "node2", Name: "pve2", Host: "10.1.0.12", Port: 22, User: "root", PoolID: poolID, Kind: target.KindPVENode}
	wireAptRepoFake(fr, aptDumpOf(map[string]string{"/etc/apt/sources.list": "deb http://deb.debian.org/debian bookworm main\n"}))
	fc.getFn = func(path string) (any, error) {
		if strings.Contains(path, "/cluster/status") {
			return []map[string]any{
				{"type": "cluster", "name": "pve-prod", "quorate": 1},
				{"type": "node", "name": "pve1", "ip": "10.1.0.11", "online": 1},
				{"type": "node", "name": "pve2", "ip": "10.1.0.12", "online": 1},
			}, nil
		}
		return nil, fmt.Errorf("unexpected GET %s", path)
	}
	items, err := s.ApplyAptRepos(context.Background(), cl.ID, AptRepoApplyInput{Action: "mirror", Preset: "ustc"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	for _, it := range items {
		if !it.OK || !it.UpdateOK {
			t.Fatalf("item = %+v", it)
		}
	}
}

// ---- 总览摘要 ---------------------------------------------------------------------------

func TestAptRepoSummary(t *testing.T) {
	cases := []struct {
		name        string
		content     string
		wantPve     string
		wantMirror  string
		wantCeph    bool
		wantEnabled int
	}{
		{"enterprise", "deb https://enterprise.proxmox.com/debian/pve bookworm pve-enterprise\n", "enterprise", "official", false, 1},
		{"no-sub", "deb http://download.proxmox.com/debian/pve bookworm pve-no-subscription\n", "no-subscription", "official", false, 1},
		{"mixed-pve", "deb https://enterprise.proxmox.com/debian/pve bookworm pve-enterprise\ndeb http://download.proxmox.com/debian/pve bookworm pve-no-subscription\n", "mixed", "official", false, 2},
		{"none-disabled", "# deb http://download.proxmox.com/debian/pve bookworm pve-no-subscription\n", "none", "official", false, 0},
		{"ustc", "deb https://mirrors.ustc.edu.cn/debian bookworm main\ndeb https://mirrors.ustc.edu.cn/proxmox/debian/pve bookworm pve-no-subscription\n", "no-subscription", "ustc", false, 2},
		{"aliyun-debian-only", "deb https://mirrors.aliyun.com/debian bookworm main\ndeb http://download.proxmox.com/debian/pve bookworm pve-no-subscription\n", "no-subscription", "aliyun", false, 2},
		{"mixed-mirror", "deb https://mirrors.ustc.edu.cn/debian bookworm main\ndeb https://mirrors.tuna.tsinghua.edu.cn/proxmox/debian/pve bookworm pve-no-subscription\n", "no-subscription", "mixed", false, 2},
		{"ceph", "deb http://deb.debian.org/debian bookworm main\ndeb https://enterprise.proxmox.com/debian/ceph-squid bookworm enterprise\n", "none", "official", true, 2},
	}
	for _, c := range cases {
		files := []*aptRepoFileRaw{parseListFile("/etc/apt/sources.list", strings.Split(c.content, "\n"))}
		pve, mirror, ceph, n := aptRepoSummary(files)
		if pve != c.wantPve || mirror != c.wantMirror || ceph != c.wantCeph || n != c.wantEnabled {
			t.Errorf("%s: got (%s,%s,ceph=%v,n=%d), want (%s,%s,ceph=%v,n=%d)",
				c.name, pve, mirror, ceph, n, c.wantPve, c.wantMirror, c.wantCeph, c.wantEnabled)
		}
	}
}

func TestListAptRepoOverview(t *testing.T) {
	s, ft, fr, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	poolID, _, _ := s.ensurePool(context.Background(), cl.ID, cl.Name)
	ft.servers["node2"] = &target.Server{ID: "node2", Name: "pve2", Host: "10.1.0.12", Port: 22, User: "root", PoolID: poolID, Kind: target.KindPVENode}

	enterpriseDump := "##PWFILE:/etc/apt/sources.list\ndeb http://deb.debian.org/debian bookworm main\ndeb https://enterprise.proxmox.com/debian/pve bookworm pve-enterprise\n\n##PWEND\n"
	fr.runFn = func(sid string, cmd []string) (*target.ExecResult, error) {
		joined := strings.Join(cmd, " ")
		if strings.Contains(joined, "##PWFILE") {
			if sid == "node2" {
				return nil, fmt.Errorf("%w: SSH 不可达", ErrProbe) // 错误隔离
			}
			return &target.ExecResult{ExitCode: 0, Stdout: enterpriseDump}, nil
		}
		if strings.Contains(joined, "/etc/os-release") {
			return &target.ExecResult{ExitCode: 0, Stdout: "VERSION_CODENAME=bookworm\n"}, nil
		}
		return &target.ExecResult{ExitCode: 0}, nil
	}
	fc.getFn = func(path string) (any, error) {
		switch {
		case strings.Contains(path, "/cluster/status"):
			return []map[string]any{
				{"type": "cluster", "name": "pve-prod", "quorate": 1},
				{"type": "node", "name": "pve1", "ip": "10.1.0.11", "online": 1},
				{"type": "node", "name": "pve2", "ip": "10.1.0.12", "online": 1},
			}, nil
		case strings.Contains(path, "/subscription"):
			return map[string]any{"status": "active"}, nil
		}
		return nil, fmt.Errorf("unexpected GET %s", path)
	}

	ov, err := s.ListAptRepoOverview(context.Background(), cl.ID)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if len(ov.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(ov.Items))
	}
	byNode := map[string]AptRepoNodeSummary{}
	for _, it := range ov.Items {
		byNode[it.Node] = it
	}
	pve1 := byNode["pve1"]
	if pve1.Codename != "bookworm" || pve1.Subscription != "active" || pve1.PveRepo != "enterprise" ||
		pve1.Mirror != "official" || pve1.Ceph || pve1.EnabledCount != 2 || pve1.Error != "" {
		t.Fatalf("pve1 = %+v", pve1)
	}
	if pve2 := byNode["pve2"]; pve2.Error == "" {
		t.Fatalf("pve2 应错误隔离: %+v", pve2)
	}
}
