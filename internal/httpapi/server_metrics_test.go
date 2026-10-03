package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// --- 纯函数解析器单测(AC:健壮容错,空/格式异常 → 不 panic、false) ---

func TestParseLoadavg(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"0.42 0.35 0.30 1/234 5678\n", 0.42, true},
		{"1.00 0.50 0.10 2/300 999", 1.00, true},
		{"", 0, false},
		{"garbage", 0, false},
		{"  \n ", 0, false},
	}
	for _, c := range cases {
		v, ok := parseLoadavg(c.in)
		if ok != c.ok || (ok && v != c.want) {
			t.Fatalf("parseLoadavg(%q) = %v,%v want %v,%v", c.in, v, ok, c.want, c.ok)
		}
	}
}

func TestParseUptimeLoadavg(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"12:00  up 5 days,  load averages: 1.23 1.10 1.05", 1.23, true},   // macOS
		{"12:00:00 up 1 day,  load average: 0.50, 0.40, 0.30", 0.50, true}, // Linux
		{"no load info here", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		v, ok := parseUptimeLoadavg(c.in)
		if ok != c.ok || (ok && v != c.want) {
			t.Fatalf("parseUptimeLoadavg(%q) = %v,%v want %v,%v", c.in, v, ok, c.want, c.ok)
		}
	}
}

func TestParseFreeBytes(t *testing.T) {
	out := "              total        used        free      shared  buff/cache   available\n" +
		"Mem:    17179869184  4123456789  1000000000   100000   500000000  12000000000\n" +
		"Swap:    2147483648           0  2147483648\n"
	used, usedWithCache, total, ok := parseFreeBytes(out)
	// used = 「used」列(不含缓存);usedWithCache = total - free(含页缓存)。
	if !ok || total != 17179869184 || used != 4123456789 || usedWithCache != 17179869184-1000000000 {
		t.Fatalf("parseFreeBytes = %d,%d,%d,%v", used, usedWithCache, total, ok)
	}
	if _, _, _, ok := parseFreeBytes(""); ok {
		t.Fatalf("empty should be false")
	}
	if _, _, _, ok := parseFreeBytes("no mem line here\n"); ok {
		t.Fatalf("no Mem line should be false")
	}
}

func TestParseSwapBytes(t *testing.T) {
	out := "" +
		"              total        used        free\n" +
		"Mem:    8054091776  2855583744  1000000000\n" +
		"Swap:   2147483648   536870912  1610612736\n"
	used, total, ok := parseSwapBytes(out)
	if !ok || total != 2147483648 || used != 536870912 {
		t.Fatalf("parseSwapBytes = %d,%d,%v", used, total, ok)
	}
	// 未配置 swap:total=0 仍算成功。
	if u, tt, ok := parseSwapBytes("Swap:   0   0   0\n"); !ok || u != 0 || tt != 0 {
		t.Fatalf("zero swap should be ok with 0/0, got %d,%d,%v", u, tt, ok)
	}
	// 无 Swap 行 → false。
	if _, _, ok := parseSwapBytes("Mem:  1 2 3\n"); ok {
		t.Fatalf("no swap line should be false")
	}
}

func TestParseDmidecodeMemBytes(t *testing.T) {
	// 现版本 dmidecode:二进制单位 GiB;含一个未装设备(应跳过)。
	out := "" +
		"Memory Device\n\tSize: 8 GiB\n\tLocator: DIMM 0\n" +
		"Memory Device\n\tSize: No Module Installed\n\tLocator: DIMM 1\n"
	got, ok := parseDmidecodeMemBytes(out)
	if !ok || got != 8*1024*1024*1024 {
		t.Fatalf("parseDmidecodeMemBytes(GiB) = %d,%v want %d", got, ok, int64(8*1024*1024*1024))
	}

	// 旧版 dmidecode:十进制 MB,多条相加。
	out2 := "\tSize: 4096 MB\n\tSize: 4096 MB\n"
	got2, ok2 := parseDmidecodeMemBytes(out2)
	if !ok2 || got2 != 2*4096*1000*1000 {
		t.Fatalf("parseDmidecodeMemBytes(MB) = %d,%v", got2, ok2)
	}

	// 全部未装 / 空 → false(上层留 0 不展示)。
	if _, ok := parseDmidecodeMemBytes("Memory Device\n\tSize: No Module Installed\n"); ok {
		t.Fatalf("no installed module should be false")
	}
	if _, ok := parseDmidecodeMemBytes(""); ok {
		t.Fatalf("empty should be false")
	}
}

func TestParseDf(t *testing.T) {
	// df -B1 (bytes)
	out := "Filesystem      1B-blocks         Used    Available Use% Mounted on\n" +
		"/dev/disk1   494384795648  123456789012  370927006636  25% /\n"
	used, total, ok := parseDf(out, 1)
	if !ok || total != 494384795648 || used != 123456789012 {
		t.Fatalf("parseDf bytes = %d,%d,%v", used, total, ok)
	}
	// df -k (KiB) → ×1024
	outK := "Filesystem 1024-blocks    Used Available Capacity Mounted on\n" +
		"/dev/disk1   1000000  400000    600000      40% /\n"
	usedK, totalK, okK := parseDf(outK, 1024)
	if !okK || totalK != 1000000*1024 || usedK != 400000*1024 {
		t.Fatalf("parseDf kib = %d,%d,%v", usedK, totalK, okK)
	}
	// folded long device name (data on its own line)
	outFold := "Filesystem                 1B-blocks         Used    Available Use% Mounted on\n" +
		"/dev/mapper/very-long-name\n" +
		"            494384795648  123456789012  370927006636  25% /\n"
	uf, tf, of := parseDf(outFold, 1)
	if !of || tf != 494384795648 || uf != 123456789012 {
		t.Fatalf("parseDf folded = %d,%d,%v", uf, tf, of)
	}
	if _, _, ok := parseDf("", 1); ok {
		t.Fatalf("empty df should be false")
	}
}

func TestParseInt(t *testing.T) {
	cases := map[string]struct {
		v  int
		ok bool
	}{
		"8\n":   {8, true},
		"  16 ": {16, true},
		"":      {0, false},
		"x":     {0, false},
		"-1":    {0, false},
	}
	for in, want := range cases {
		v, ok := parseInt(in)
		if ok != want.ok || (ok && v != want.v) {
			t.Fatalf("parseInt(%q) = %d,%v want %d,%v", in, v, ok, want.v, want.ok)
		}
	}
}

// --- 合并采集脚本与分段(1 台 = 1 次 SSH 连接的核心) ---

func TestMetricsCollectScript(t *testing.T) {
	base := metricsCollectArgs(false)
	if len(base) != 3 || base[0] != "sh" || base[1] != "-c" {
		t.Fatalf("collect args should be sh -c script: %v", base)
	}
	script := base[2]
	for _, want := range []string{
		`cat /proc/loadavg 2>/dev/null || uptime`,
		`nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null`,
		`free -b 2>/dev/null`,
		`df -B1 / 2>/dev/null`,
		`df -k / 2>/dev/null`,
		metricMarker + secLoadavg, metricMarker + secCores, metricMarker + secMemory,
		metricMarker + secDiskB, metricMarker + secDiskK, metricMarker + secEnd,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "dmidecode") {
		t.Fatalf("non-probe script should not contain dmidecode:\n%s", script)
	}

	// 探测版:追加 dmidecode 段,其余不变。
	probe := metricsCollectArgs(true)
	if !strings.Contains(probe[2], `dmidecode -t 17 2>/dev/null`) ||
		!strings.Contains(probe[2], metricMarker+secPhysMem) {
		t.Fatalf("probe script missing dmidecode section:\n%s", probe[2])
	}
}

func TestSplitMetricSections(t *testing.T) {
	out := metricMarker + secLoadavg + "\n0.42 0.35 0.30 1/234 5678\n" +
		metricMarker + secMemory + "\nMem: 1 2 3\nSwap: 0 0 0\n" +
		metricMarker + secCores + "\n" + // 空段(命令缺失的常态)
		metricMarker + secEnd + "\n"
	sections := splitMetricSections(out)
	if got := sections[secLoadavg]; got != "0.42 0.35 0.30 1/234 5678\n" {
		t.Fatalf("loadavg section = %q", got)
	}
	if got := sections[secMemory]; got != "Mem: 1 2 3\nSwap: 0 0 0\n" {
		t.Fatalf("memory section = %q", got)
	}
	if v, ok := sections[secCores]; !ok || v != "" {
		t.Fatalf("cores should be present-but-empty, got %q,%v", v, ok)
	}
	// 无标记的前导内容丢弃;空输出 → 空表。
	if s := splitMetricSections("junk\n" + metricMarker + secLoadavg + "\n1\n"); s[secLoadavg] != "1\n" {
		t.Fatalf("prefix junk should be dropped: %q", s[secLoadavg])
	}
	if len(splitMetricSections("")) != 0 {
		t.Fatalf("empty output should give empty map")
	}
}

// --- 假远端机:按合并脚本回一份带 ##PW: 标记的完整 stdout ---

// fakeMetricsHost 是一台假远端机的各采集段内容(段空 = 该命令在远端缺失/失败 → 指标 null)。
// 回退逻辑(cat || uptime、nproc || getconf、df -B1 || df -k)在远端脚本里执行,假机给的
// 段内容即「回退后的最终产物」。
type fakeMetricsHost struct {
	loadavg string
	cores   string
	memory  string
	diskB   string
	diskK   string
	phys    string
}

// linuxFakeHost:全命令可用(真 Linux 形态)。
var linuxFakeHost = fakeMetricsHost{
	loadavg: "0.42 0.35 0.30 1/234 5678\n",
	cores:   "8\n",
	memory:  "              total        used        free\nMem:    17179869184  4123456789  1000000000\n",
	diskB:   "Filesystem 1B-blocks Used Available Use% Mounted on\n/dev/sda1 494384795648 123456789012 370927006636 25% /\n",
}

// fakeMetricsStdout 把假机段内容拼成合并脚本输出。dmidecode 段仅当脚本带探测(含
// "dmidecode")时输出,与真脚本行为一致;end 标记收尾(脚本恒 0 退出)。
func fakeMetricsStdout(script string, h fakeMetricsHost) string {
	var b strings.Builder
	sec := func(name, content string) {
		b.WriteString(metricMarker + name + "\n")
		b.WriteString(content)
	}
	sec(secLoadavg, h.loadavg)
	sec(secCores, h.cores)
	sec(secMemory, h.memory)
	sec(secDiskB, h.diskB)
	sec(secDiskK, h.diskK)
	if strings.Contains(script, "dmidecode") {
		sec(secPhysMem, h.phys)
	}
	b.WriteString(metricMarker + secEnd + "\n")
	return b.String()
}

// scriptFakeExec 造 ExecResult:合并采集命令(sh -c)→ 假机输出;其它命令 → 127。
func scriptFakeExec(cmd []string, h fakeMetricsHost) *target.ExecResult {
	if len(cmd) == 3 && cmd[0] == "sh" && cmd[1] == "-c" {
		return &target.ExecResult{Stdout: fakeMetricsStdout(cmd[2], h), ExitCode: 0}
	}
	return &target.ExecResult{Stderr: "command not found", ExitCode: 127}
}

type cmdDialer struct {
	// fn 按命令(合并采集即 sh -c 脚本)路由 stdout / exitCode。
	fn func(cmd []string) (*target.ExecResult, error)
}

func (d cmdDialer) Run(_ context.Context, _ string, _ target.SSHConfig, cmd []string) (*target.ExecResult, error) {
	return d.fn(cmd)
}

func (d cmdDialer) RunStream(_ context.Context, _ string, _ target.SSHConfig, _ []string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (d cmdDialer) RunInteractive(_ context.Context, _ string, _ target.SSHConfig, _ []string) (target.Session, error) {
	return nil, nil
}

func (d cmdDialer) RunWithStdin(_ context.Context, _ string, _ target.SSHConfig, cmd []string, _ io.Reader) (*target.ExecResult, error) {
	return d.fn(cmd)
}

// linuxLikeDialer 模拟一台 Linux 服务器:loadavg/nproc/free/df 全可回显。
func linuxLikeDialer() cmdDialer {
	return cmdDialer{fn: func(cmd []string) (*target.ExecResult, error) {
		return scriptFakeExec(cmd, linuxFakeHost), nil
	}}
}

// macLikeDialer 模拟 macOS:无 /proc/loadavg(uptime 回退)、无 nproc(getconf 回退)、
// 无 free(memory null)、df -B1 不识别(df -k 回退)—— 段内容即脚本回退后的产物。
func macLikeDialer() cmdDialer {
	h := fakeMetricsHost{
		loadavg: "12:00  up 5 days, load averages: 1.23 1.10 1.05\n",
		cores:   "10\n",
		diskK:   "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/disk1 1000000 400000 600000 40% /\n",
	}
	return cmdDialer{fn: func(cmd []string) (*target.ExecResult, error) {
		return scriptFakeExec(cmd, h), nil
	}}
}

// resetMetricsCacheForTest 清包级全局缓存(快照 + dmidecode 探测),保证用例互不串扰。
func resetMetricsCacheForTest(t *testing.T) {
	t.Helper()
	resetMetricsSnapshotCache()
	physMemMu.Lock()
	physMemCache = map[string]physMemEntry{}
	physMemMu.Unlock()
	t.Cleanup(resetMetricsSnapshotCache)
}

// newMetricsTestService 造真 target.Service(内存库 + 假拨号器 + 真保险库凭据)并登记一台
// 服务器,返回 (svc, noVaultSvc, serverID):noVaultSvc 同库但保险库未配,专测定位类错误。
func newMetricsTestService(t *testing.T, dialer target.SSHDialer) (svc, noVaultSvc target.Service, id string) {
	t.Helper()
	st := testStoreAuth(t)
	v := vault.New(st.DB, testMasterKey())
	svc = target.New(st.DB, v, dialer)
	cred, err := v.Create(vault.CreateInput{Name: "ssh", Type: vault.TypeSSHKey, Secret: "pw"})
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	srv, err := svc.Create(context.Background(), target.CreateInput{
		Name: "m1", Host: "127.0.0.1", User: "root", CredentialID: cred.ID,
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	noVaultSvc = target.New(st.DB, nil, dialer)
	return svc, noVaultSvc, srv.ID
}

// --- HTTP 端点集成测试(经真 router + 认证;冷启动路径) ---

func TestServerMetricsLinux(t *testing.T) {
	resetMetricsCacheForTest(t)
	srv, client, csrf := setupServerAPI(t, linuxLikeDialer())
	credID := newSSHCredAPI(t, client, srv.URL, csrf, "pw")
	id := createServerAPI(t, client, srv.URL, csrf, credID)

	resp, _ := client.Get(srv.URL + "/api/servers/" + id + "/metrics")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	var out serverMetricsDTO
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, raw)
	}
	if !out.Reachable {
		t.Fatalf("want reachable true: %+v", out)
	}
	if out.CPU == nil || out.CPU.Loadavg1 == nil || *out.CPU.Loadavg1 != 0.42 {
		t.Fatalf("cpu loadavg wrong: %+v", out.CPU)
	}
	if out.CPU.Cores == nil || *out.CPU.Cores != 8 {
		t.Fatalf("cores wrong: %+v", out.CPU)
	}
	if out.Memory == nil || out.Memory.TotalBytes != 17179869184 || out.Memory.UsedBytes != 4123456789 {
		t.Fatalf("memory wrong: %+v", out.Memory)
	}
	if out.Disk == nil || out.Disk.Path != "/" || out.Disk.TotalBytes != 494384795648 || out.Disk.UsedBytes != 123456789012 {
		t.Fatalf("disk wrong: %+v", out.Disk)
	}
	if out.CollectedAt == "" {
		t.Fatalf("collectedAt empty")
	}
}

func TestServerMetricsMacFallback(t *testing.T) {
	resetMetricsCacheForTest(t)
	srv, client, csrf := setupServerAPI(t, macLikeDialer())
	credID := newSSHCredAPI(t, client, srv.URL, csrf, "pw")
	id := createServerAPI(t, client, srv.URL, csrf, credID)

	resp, _ := client.Get(srv.URL + "/api/servers/" + id + "/metrics")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	var out serverMetricsDTO
	_ = json.Unmarshal(raw, &out)
	if !out.Reachable {
		t.Fatalf("want reachable true: %+v", out)
	}
	// loadavg via uptime fallback
	if out.CPU == nil || out.CPU.Loadavg1 == nil || *out.CPU.Loadavg1 != 1.23 {
		t.Fatalf("cpu loadavg fallback wrong: %+v", out.CPU)
	}
	// cores via getconf fallback
	if out.CPU.Cores == nil || *out.CPU.Cores != 10 {
		t.Fatalf("cores fallback wrong: %+v", out.CPU)
	}
	// free missing → memory null (AC: 该指标 null 不报错)
	if out.Memory != nil {
		t.Fatalf("memory should be null on macOS: %+v", out.Memory)
	}
	// df -k fallback
	if out.Disk == nil || out.Disk.TotalBytes != 1000000*1024 || out.Disk.UsedBytes != 400000*1024 {
		t.Fatalf("disk fallback wrong: %+v", out.Disk)
	}
}

func TestServerMetricsUnreachableNot500(t *testing.T) {
	// dialer 返回不可达错误 → reachable:false + 人读 error,200 不 500。
	dialer := cmdDialer{fn: func(_ []string) (*target.ExecResult, error) {
		return nil, target.ErrUnreachable
	}}
	resetMetricsCacheForTest(t)
	srv, client, csrf := setupServerAPI(t, dialer)
	credID := newSSHCredAPI(t, client, srv.URL, csrf, "pw")
	id := createServerAPI(t, client, srv.URL, csrf, credID)

	resp, _ := client.Get(srv.URL + "/api/servers/" + id + "/metrics")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (not 500): %s", resp.StatusCode, raw)
	}
	var out serverMetricsDTO
	_ = json.Unmarshal(raw, &out)
	if out.Reachable {
		t.Fatalf("want reachable false")
	}
	if out.Error == "" {
		t.Fatalf("want human error")
	}
	if out.CPU != nil || out.Memory != nil || out.Disk != nil {
		t.Fatalf("metrics should be null when unreachable: %+v", out)
	}
}

func TestServerMetricsNotFound(t *testing.T) {
	resetMetricsCacheForTest(t)
	srv, client, csrf := setupServerAPI(t, linuxLikeDialer())
	_ = csrf
	resp, _ := client.Get(srv.URL + "/api/servers/does-not-exist/metrics")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.StatusCode, raw)
	}
}

func TestServerMetricsRequiresAuth(t *testing.T) {
	resetMetricsCacheForTest(t)
	srv, _, _ := setupServerAPI(t, linuxLikeDialer())
	// 不带 session 的裸客户端。
	bare := &http.Client{}
	resp, _ := bare.Get(srv.URL + "/api/servers/x/metrics")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("metrics status = %d, want 401: %s", resp.StatusCode, raw)
	}
	resp2, _ := bare.Get(srv.URL + "/api/servers/metrics")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("batch metrics status = %d, want 401", resp2.StatusCode)
	}
}

func TestAllServerMetricsBatchIndependent(t *testing.T) {
	// 批量端点逐台独立:可回显的 Linux 正常,失败仅落在该台(不 500、不连累其它台)。
	resetMetricsCacheForTest(t)
	srv, client, csrf := setupServerAPI(t, linuxLikeDialer())
	credID := newSSHCredAPI(t, client, srv.URL, csrf, "pw")
	_ = createServerAPI(t, client, srv.URL, csrf, credID) // s1 (host 127.0.0.1)

	resp, _ := client.Get(srv.URL + "/api/servers/metrics")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch status = %d, want 200: %s", resp.StatusCode, raw)
	}
	var body struct {
		Items []serverMetricsDTO `json:"items"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, raw)
	}
	if len(body.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(body.Items))
	}
	if !body.Items[0].Reachable || body.Items[0].Disk == nil {
		t.Fatalf("item should be reachable with disk: %+v", body.Items[0])
	}
}

// --- 快照缓存(stale-while-revalidate,单飞)单测 ---

func TestSnapshotServerMetricsCache(t *testing.T) {
	resetMetricsCacheForTest(t)

	// 可注入时钟:TTL 过期路径不靠真实睡眠。用原子变量 —— 后台刷新 goroutine 也会读时钟。
	base := time.Unix(1_700_000_000, 0)
	var nowNano atomic.Int64
	nowNano.Store(base.UnixNano())
	metricsNow = func() time.Time { return time.Unix(0, nowNano.Load()) }
	t.Cleanup(func() { metricsNow = time.Now })

	var calls atomic.Int32
	dialer := cmdDialer{fn: func(cmd []string) (*target.ExecResult, error) {
		calls.Add(1)
		return scriptFakeExec(cmd, linuxFakeHost), nil
	}}
	svc, _, id := newMetricsTestService(t, dialer)
	ctx := context.Background()

	// 冷启动:同步采集一次。
	dto, err := snapshotServerMetrics(ctx, svc, id)
	if err != nil || !dto.Reachable {
		t.Fatalf("cold collect: err=%v reachable=%v error=%q", err, dto.Reachable, dto.Error)
	}
	if calls.Load() != 1 {
		t.Fatalf("cold collect should SSH once, got %d", calls.Load())
	}

	// TTL 内:命中缓存,0 SSH。
	if _, err := snapshotServerMetrics(ctx, svc, id); err != nil {
		t.Fatalf("warm hit: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("warm hit should not SSH, got %d calls", calls.Load())
	}

	// 过期:立即回旧值 + 后台单飞刷新(等钩子确认完成)。
	refreshDone := make(chan struct{}, 8)
	metricsRefreshed = func() { refreshDone <- struct{}{} }
	t.Cleanup(func() { metricsRefreshed = nil })
	nowNano.Store(base.Add(11 * time.Second).UnixNano())
	if _, err := snapshotServerMetrics(ctx, svc, id); err != nil {
		t.Fatalf("stale serve: %v", err)
	}
	select {
	case <-refreshDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("background refresh did not finish in time")
	}
	if calls.Load() != 2 {
		t.Fatalf("stale should trigger exactly one background refresh, got %d calls", calls.Load())
	}

	// 刷新后再次过期:两次(并发)请求只触发一次刷新 —— 单飞。
	nowNano.Store(nowNano.Load() + (11 * time.Second).Nanoseconds())
	_, _ = snapshotServerMetrics(ctx, svc, id)
	_, _ = snapshotServerMetrics(ctx, svc, id)
	select {
	case <-refreshDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("single-flight refresh did not finish")
	}
	// 不该有多余的第二次刷新。
	select {
	case <-refreshDone:
		t.Fatalf("single-flight violated: more than one refresh")
	case <-time.After(200 * time.Millisecond):
	}
	if calls.Load() != 3 {
		t.Fatalf("expected 3 SSH calls total, got %d", calls.Load())
	}
}

func TestSnapshotServerMetricsLocateErrorNotCached(t *testing.T) {
	resetMetricsCacheForTest(t)
	// 保险库未配(vault=nil)→ 定位类错误:每次冷启动同步返回、不入缓存 ——
	// 这样单台端点能持续把该错误映射为 503,而不是吃过期的 200。
	_, noVault, id := newMetricsTestService(t, linuxLikeDialer())
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := snapshotServerMetrics(ctx, noVault, id); !errors.Is(err, target.ErrVaultUnconfigured) {
			t.Fatalf("call %d: want ErrVaultUnconfigured, got %v", i, err)
		}
	}
	metricsSnapMu.Lock()
	n := len(metricsSnapshots)
	metricsSnapMu.Unlock()
	if n != 0 {
		t.Fatalf("locate error should not be cached, %d entries", n)
	}
}
