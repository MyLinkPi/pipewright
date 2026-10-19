package ssar

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/target"
)

// fptr 便捷构造 *float64。
func fptr(f float64) *float64 { return &f }

// ─── 采样解析(sampleFromSections / lastJSONRow / ssarNum / splitSections)────────

// TestSampleFromSections:各视图段独立解析,取尾行;占位符/缺失段 → 对应字段 nil。
func TestSampleFromSections(t *testing.T) {
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	out := strings.Join([]string{
		"##PW:cpu",
		`[{"collect_datetime":"2026-10-19T11:58:00","user":"10.5","system":"3.2","idle":"850.1","iowait":"1.1","steal":"0"},
		  {"collect_datetime":"2026-10-19T11:59:00","user":"11.5","system":"4.2","idle":"840.1","iowait":"2.1","steal":"0.5"}]`,
		"##PW:mem",
		`[{"collect_datetime":"2026-10-19T11:59:00","memtotal":"16384000","memfree":"2000000","memavailable":"8000000"}]`,
		"##PW:tcp",
		`[{"collect_datetime":"2026-10-19T11:59:00","currestab":"42","retranssegs":"0.35"}]`,
		"##PW:retran",
		`[{"collect_datetime":"2026-10-19T11:59:00","synretrans":"0.1","fastretrans":"0.2","retransfail":"-"}]`,
		"##PW:load5s",
		`[{"collect_datetime":"2026-10-19T11:58:55","threads":"27515","load1":"0.42","runq":"4","stype":"5s","sstate":"Y"},
		  {"collect_datetime":"2026-10-19T11:59:00","threads":"27600","load1":"0.50","runq":"2","stype":"5s","sstate":"Y"}]`,
		"##PW:cend",
	}, "\n")
	sm := sampleFromSections("srv1", at, splitSections(out))

	if sm.CPUUser == nil || *sm.CPUUser != 11.5 {
		t.Fatalf("cpu user: want 11.5(尾行), got %v", sm.CPUUser)
	}
	if sm.CPUSystem == nil || *sm.CPUSystem != 4.2 {
		t.Fatalf("cpu system: want 4.2, got %v", sm.CPUSystem)
	}
	if sm.CPUIowait == nil || *sm.CPUIowait != 2.1 {
		t.Fatalf("cpu iowait: want 2.1, got %v", sm.CPUIowait)
	}
	if sm.CPUSteal == nil || *sm.CPUSteal != 0.5 {
		t.Fatalf("cpu steal: want 0.5, got %v", sm.CPUSteal)
	}
	if sm.MemTotalKB == nil || *sm.MemTotalKB != 16384000 {
		t.Fatalf("mem total: got %v", sm.MemTotalKB)
	}
	if sm.MemAvailKB == nil || *sm.MemAvailKB != 8000000 {
		t.Fatalf("mem avail: got %v", sm.MemAvailKB)
	}
	if sm.TCPCurrEstab == nil || *sm.TCPCurrEstab != 42 {
		t.Fatalf("tcp currestab: got %v", sm.TCPCurrEstab)
	}
	if sm.TCPRetransSegs == nil || *sm.TCPRetransSegs != 0.35 {
		t.Fatalf("tcp retranssegs: got %v", sm.TCPRetransSegs)
	}
	// retran 段占位符 "-" → nil;可解析项照常。
	if sm.TCPRetransFail != nil {
		t.Fatalf("retransfail 占位符应为 nil, got %v", *sm.TCPRetransFail)
	}
	if sm.TCPSynRetrans == nil || *sm.TCPSynRetrans != 0.1 {
		t.Fatalf("synretrans: got %v", sm.TCPSynRetrans)
	}
	// load5s 取尾行;非数值列(stype)自然不取。
	if sm.Load1 == nil || *sm.Load1 != 0.50 {
		t.Fatalf("load1: want 0.50(尾行), got %v", sm.Load1)
	}
	if sm.Runq == nil || *sm.Runq != 2 {
		t.Fatalf("runq: want 2, got %v", sm.Runq)
	}
	if !sampleHasAny(sm) {
		t.Fatal("sampleHasAny 应为 true")
	}
	// 入库时间是 pipewright 采样时刻,不是行内时间。
	if !sm.At.Equal(at) {
		t.Fatalf("At 应为采样时刻 %v, got %v", at, sm.At)
	}
}

// TestSampleFromSectionsEmpty:全段缺失(未装 ssar)→ 全字段 nil → sampleHasAny=false。
func TestSampleFromSectionsEmpty(t *testing.T) {
	out := strings.Join([]string{
		"##PW:cpu", "",
		"##PW:mem", "",
		"##PW:tcp", "",
		"##PW:retran", "",
		"##PW:load5s", "",
		"##PW:cend", "",
	}, "\n")
	sm := sampleFromSections("srv1", time.Now(), splitSections(out))
	if sampleHasAny(sm) {
		t.Fatal("全空样本 sampleHasAny 应为 false")
	}
	if sm == nil {
		t.Fatal("sampleFromSections 不应返回 nil(字段级降级)")
	}
}

// TestLastJSONRow:JSON 前后夹带噪声文本时按括号片段重试;空/无数组 → false。
func TestLastJSONRow(t *testing.T) {
	if _, ok := lastJSONRow(``); ok {
		t.Fatal("空段应 false")
	}
	if _, ok := lastJSONRow(`Exception: Data Paths is not exist`); ok {
		t.Fatal("无 JSON 应 false")
	}
	row, ok := lastJSONRow("提示文本\n[{\"a\":\"1\"}]\n尾部噪声")
	if !ok || row["a"] != "1" {
		t.Fatalf("噪声包裹的 JSON 应解析成功: %v %v", ok, row)
	}
}

// TestSsarNum:占位符/百分比/非数值。
func TestSsarNum(t *testing.T) {
	for _, c := range []struct {
		in   string
		want *float64
	}{
		{"12.5", fptr(12.5)},
		{"  3  ", fptr(3)},
		{"80.73%", fptr(80.73)},
		{"-", nil},
		{"*", nil},
		{"<", nil},
		{">", nil},
		{"", nil},
		{"5s", nil},
	} {
		got := ssarNum(c.in)
		if (got == nil) != (c.want == nil) {
			t.Fatalf("ssarNum(%q): nil-ness mismatch (want nil=%v)", c.in, c.want == nil)
		}
		if got != nil && *got != *c.want {
			t.Fatalf("ssarNum(%q) = %v, want %v", c.in, *got, *c.want)
		}
	}
}

// ─── 按需查询(queryArgs / parseQueryResult / 参数校验)────────────────────────

// TestQueryArgs:视图白名单映射到固定 argv;数值经 strconv 进入,绝无用户输入拼接。
func TestQueryArgs(t *testing.T) {
	got := queryArgs("cpu", 30, 5)
	want := []string{"ssar", "--cpu", "--api", "-r", "30", "-i", "5m", "-P"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("cpu argv:\n got %v\nwant %v", got, want)
	}
	got = queryArgs("load5s", 60, 1)
	want = []string{"ssar", "load5s", "--api", "-r", "60"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("load5s argv:\n got %v\nwant %v", got, want)
	}
}

// TestQueryValidation:view 白名单外 / 区间越界 / 行数超限 → ErrInvalidQuery。
func TestQueryValidation(t *testing.T) {
	svc := newTestService(t, newFakeRunner())
	ctx := context.Background()
	cases := []struct {
		view          string
		minutes, step int
	}{
		{"evil; rm -rf /", 60, 1},        // 白名单外
		{"cpu", 0, 1},                    // 区间下界
		{"cpu", queryMaxMinutes + 1, 60}, // 区间上界
		{"cpu", 2000, 1},                 // 行数超限
		{"cpu", 60, 0},                   // 步长下界
	}
	for _, c := range cases {
		if _, err := svc.Query(ctx, "s1", c.view, c.minutes, c.step); err == nil {
			t.Fatalf("Query(%q,%d,%d) 应报参数错误", c.view, c.minutes, c.step)
		}
	}
	// 合法参数(边界值)应通过校验并落到执行(fake runner 回空)。
	if _, err := svc.Query(ctx, "s1", "cpu", 1440, 1); err != nil {
		t.Fatalf("边界行数 1440 应通过: %v", err)
	}
}

// TestParseQueryResult:列收集 + 数值/字符串/占位符分型;collect_datetime → time。
func TestParseQueryResult(t *testing.T) {
	out := `[{"collect_datetime":"2026-10-19T12:00:00","user":"1.5","stype":"5s","idle":"-"},
	         {"collect_datetime":"2026-10-19T12:01:00","user":"2.5","stype":"5s","idle":"900.0"}]`
	res, err := parseQueryResult("load5s", out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Rows) != 2 || len(res.Columns) != 3 {
		t.Fatalf("rows=%d cols=%d, want 2/3", len(res.Rows), len(res.Columns))
	}
	// 列序:首行键序(user,stype,idle),均带单位口径(未知列为空)。
	keys := []string{res.Columns[0].Key, res.Columns[1].Key, res.Columns[2].Key}
	if strings.Join(keys, ",") != "user,stype,idle" {
		t.Fatalf("列序 want user,stype,idle got %v", keys)
	}
	if res.Columns[0].Unit != "jiffies/s" {
		t.Fatalf("user 单位 want jiffies/s got %q", res.Columns[0].Unit)
	}
	r0 := res.Rows[0]
	if r0.Time != "2026-10-19T12:00:00" {
		t.Fatalf("time: %q", r0.Time)
	}
	if v, ok := r0.Values["user"].(float64); !ok || v != 1.5 {
		t.Fatalf("user: %v", r0.Values["user"])
	}
	if v, ok := r0.Values["stype"].(string); !ok || v != "5s" {
		t.Fatalf("stype 应保留字符串: %v", r0.Values["stype"])
	}
	if _, exists := r0.Values["idle"]; exists {
		t.Fatal("占位符 idle 在首行不应出现")
	}
	if v, ok := res.Rows[1].Values["idle"].(float64); !ok || v != 900.0 {
		t.Fatalf("次行 idle: %v", res.Rows[1].Values["idle"])
	}
}

// TestParseQueryResultEmpty:空/异常输出 → 空 rows 不报错(机器无数据常态)。
func TestParseQueryResultEmpty(t *testing.T) {
	for _, in := range []string{"", "Exception: Data Paths /var/log/sre_proc/data is not exist", "garbage"} {
		res, err := parseQueryResult("cpu", in)
		if err != nil || len(res.Rows) != 0 {
			t.Fatalf("parse(%q): err=%v rows=%d, want 空 rows 无错", in, err, len(res.Rows))
		}
	}
}

// ─── 状态探测解析 ─────────────────────────────────────────────────────────────

// TestParseSsarVersion:取首行最后字段。
func TestParseSsarVersion(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"ssar 1.0.5", "1.0.5"},
		{"1.0.5", "1.0.5"},
		{"", ""},
	} {
		got, ok := parseSsarVersion(c.in)
		if c.want == "" && ok {
			t.Fatalf("parseSsarVersion(%q) 应失败", c.in)
		}
		if c.want != "" && got != c.want {
			t.Fatalf("parseSsarVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDetectFamily:rpm 优先;皆无 → false。
func TestDetectFamily(t *testing.T) {
	if f, ok := detectFamily(map[string]string{"family": "/usr/bin/rpm"}); !ok || f != "rpm" {
		t.Fatalf("rpm: %v %v", f, ok)
	}
	if f, ok := detectFamily(map[string]string{"family": "/usr/bin/dpkg"}); !ok || f != "deb" {
		t.Fatalf("deb: %v %v", f, ok)
	}
	if _, ok := detectFamily(map[string]string{"family": ""}); ok {
		t.Fatal("皆无应 false")
	}
}

// ─── store(record / History / LastSampleAt / Sweep)──────────────────────────

// TestStoreRoundTrip:写入 → 按窗回读(升序、null 保持)→ 最近采样 → 清理。
func TestStoreRoundTrip(t *testing.T) {
	svc := newTestService(t, &fakeRunner{})
	ctx := context.Background()
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

	if err := svc.record(ctx, []Sample{
		{ServerID: "s1", At: base, CPUUser: fptr(10), MemTotalKB: fptr(1000), Load1: fptr(0.5)},
		{ServerID: "s2", At: base, CPUUser: nil},
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := svc.record(ctx, []Sample{
		{ServerID: "s1", At: base.Add(time.Minute), CPUUser: fptr(12), Load1: nil},
	}); err != nil {
		t.Fatalf("record2: %v", err)
	}

	pts, err := svc.History(ctx, "s1", base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(pts) != 2 {
		t.Fatalf("want 2 points, got %d", len(pts))
	}
	if !pts[0].At.Before(pts[1].At) {
		t.Fatal("应按时间升序")
	}
	if pts[1].Load1 != nil {
		t.Fatal("第二轮 Load1 未采样应保持 nil")
	}
	if pts[0].MemTotalKB == nil || *pts[0].MemTotalKB != 1000 {
		t.Fatalf("mem: %v", pts[0].MemTotalKB)
	}

	last, err := svc.LastSampleAt(ctx, "s1")
	if err != nil || !last.Equal(base.Add(time.Minute)) {
		t.Fatalf("lastSampleAt: %v %v", last, err)
	}
	if last, err := svc.LastSampleAt(ctx, "nope"); err != nil || !last.IsZero() {
		t.Fatalf("无样本应返回零值: %v %v", last, err)
	}

	n, err := svc.Sweep(ctx, base.Add(30*time.Second))
	if err != nil || n != 2 {
		t.Fatalf("sweep: n=%d err=%v, want 2(s1 与 s2 的 base 行)", n, err)
	}
	if pts, _ := svc.History(ctx, "s1", base.Add(-time.Hour)); len(pts) != 1 {
		t.Fatalf("sweep 后应剩 1 点, got %d", len(pts))
	}
}

// ─── 安装流程(fake runner 序列)───────────────────────────────────────────────

// fakeRunner 记录命令序列并按脚本回放结果,供 installOne / Status / Query 离线测试。
type fakeRunner struct {
	mu      sync.Mutex
	calls   []string                      // "run|runPlain|upload:<path>" + cmd 拼接
	plain   map[string]*target.ExecResult // RunPlain 按首命令(探测绘 uname / 查询)匹配
	priv    map[string]*target.ExecResult // Run 按首命令匹配
	uploads map[string]string             // remotePath → 内容
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		plain:   map[string]*target.ExecResult{},
		priv:    map[string]*target.ExecResult{},
		uploads: map[string]string{},
	}
}

func (f *fakeRunner) Run(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "run:"+strings.Join(cmd, " "))
	if res, ok := f.priv[cmd[0]]; ok {
		return res, nil
	}
	return &target.ExecResult{ExitCode: 0}, nil
}

func (f *fakeRunner) RunPlain(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "runPlain:"+strings.Join(cmd, " "))
	// 探测脚本按 uname 特征匹配;ssar 查询按 ssar 前缀匹配。
	joined := strings.Join(cmd, " ")
	if strings.HasPrefix(joined, "sh -c") {
		if res, ok := f.plain["probe"]; ok {
			return res, nil
		}
	}
	if res, ok := f.plain[joined]; ok {
		return res, nil
	}
	return &target.ExecResult{ExitCode: 0}, nil
}

func (f *fakeRunner) Upload(ctx context.Context, serverID string, content io.Reader, remotePath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(content)
	f.calls = append(f.calls, "upload:"+remotePath)
	f.uploads[remotePath] = string(b)
	return nil
}

// linuxProbe 是典型 x86_64 + rpm + 已装 ssar 的探测输出。
func linuxProbe(version, active string) *target.ExecResult {
	script := strings.Join([]string{
		"##PW:env", "Linux", "x86_64",
		"##PW:family", "/usr/bin/rpm",
		"##PW:ssarver", "ssar " + version,
		"##PW:sresar", active,
		"##PW:end",
	}, "\n")
	return &target.ExecResult{Stdout: script, ExitCode: 0}
}

// newTestService 构造挂 fake runner 的 service(库已迁移;downloader 指向临时目录)。
func newTestService(t *testing.T, r Runner) *service {
	t.Helper()
	s := &service{
		db:          storetest.OpenDB(t),
		runner:      r,
		statusCache: map[string]statusEntry{},
	}
	return s
}

// TestInstallOneSuccess:完整链路(探测 → 上传 → rpm 安装 → 清理 → 复探验证)。
func TestInstallOneSuccess(t *testing.T) {
	dir := t.TempDir()
	if err := writeFakePackage(dir, "v1.0.5", "ssar-1.0.5-1.x86_64.rpm"); err != nil {
		t.Fatalf("准备缓存包: %v", err)
	}
	fr := newFakeRunner()
	fr.plain["probe"] = linuxProbe("1.0.5", "active")
	svc := newTestService(t, fr)
	svc.downloader = &Downloader{DataDir: dir, now: time.Now}

	res := svc.installOne(context.Background(), "s1")
	if !res.OK || res.Version != "1.0.5" {
		t.Fatalf("install: ok=%v ver=%q err=%q", res.OK, res.Version, res.Error)
	}
	// 命令序列:探查(runPlain sh -c)→ 上传 → rpm -Uvh → rm -f → 复探(runPlain sh -c)。
	joined := strings.Join(fr.calls, "\n")
	for _, want := range []string{
		"runPlain:sh -c",
		"upload:/tmp/pipewright-ssar-v1.0.5.rpm",
		"run:rpm -Uvh --replacepkgs /tmp/pipewright-ssar-v1.0.5.rpm",
		"run:rm -f /tmp/pipewright-ssar-v1.0.5.rpm",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("命令序列缺少 %q:\n%s", want, joined)
		}
	}
	// 上传内容即缓存包字节。
	if fr.uploads["/tmp/pipewright-ssar-v1.0.5.rpm"] != "fake-rpm-bytes" {
		t.Fatal("上传内容与缓存包不一致")
	}
}

// TestInstallOneNonLinux:非 Linux 目标 → 明确人读错误,不触安装命令。
func TestInstallOneNonLinux(t *testing.T) {
	fr := newFakeRunner()
	fr.plain["probe"] = &target.ExecResult{Stdout: "##PW:env\nDarwin\narm64\n##PW:family\n\n##PW:end\n"}
	svc := newTestService(t, fr)
	res := svc.installOne(context.Background(), "s1")
	if res.OK || res.Error == "" {
		t.Fatalf("非 Linux 应失败: %+v", res)
	}
	if !strings.Contains(res.Error, "Linux") {
		t.Fatalf("错误文案应含 Linux: %q", res.Error)
	}
	for _, c := range fr.calls {
		if strings.HasPrefix(c, "run:rpm") || strings.HasPrefix(c, "run:dpkg") || strings.HasPrefix(c, "upload") {
			t.Fatalf("非 Linux 不应安装/上传: %v", fr.calls)
		}
	}
}

// TestInstallOneNoCache:平台未缓存安装包 → 人读错误(请先下载)。
func TestInstallOneNoCache(t *testing.T) {
	fr := newFakeRunner()
	fr.plain["probe"] = linuxProbe("1.0.5", "active")
	svc := newTestService(t, fr)
	svc.downloader = &Downloader{DataDir: t.TempDir(), now: time.Now}
	res := svc.installOne(context.Background(), "s1")
	if res.OK || !strings.Contains(res.Error, "下载") {
		t.Fatalf("未缓存应提示先下载: %+v", res)
	}
}

// writeFakePackage 在缓存目录落一个假包 + 版本标记(模拟 Downloader.Download 产物)。
func writeFakePackage(dataDir, tag, pkgName string) error {
	dir := filepath.Join(dataDir, "bin", "ssar", tag)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, pkgName), []byte("fake-rpm-bytes"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, "bin", "ssar", VersionMarker), []byte(tag), 0o644)
}
