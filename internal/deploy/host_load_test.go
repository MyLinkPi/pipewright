package deploy

// host_load_test.go 验证「部署目标数量上限(maxTargets)」的选机语义:
//   - 负载采集是**纯静态 array 命令**(AC-SEC-02,无用户输入拼接);
//   - 饱和度 = CPU 负载率与内存使用率的**高者**;单维取不到时用可得维度,两维皆无 → 探测失败;
//   - 探测失败的机器排最后(宁可不用「确认不了负载」的机器);
//   - limit ≤ 0 / ≥ 命中数 → 一台都不探测,原样返回(行为与现状一致);
//   - 同分保持圈选原序(稳定排序);
//   - DeployForStage 端到端:命中 5 台 + 上限 3 → 只部署最空的 3 台,deploy_targets 只落 3 行。

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/target"
)

// loadOut 拼一段负载采集脚本的 stdout(缺段 = 该段传 "")。
func loadOut(loadavg, cores, mem string) string {
	var b strings.Builder
	write := func(name, body string) {
		b.WriteString(loadMarker + name + "\n")
		if body != "" {
			b.WriteString(body + "\n")
		}
	}
	write(loadSecLoadavg, loadavg)
	write(loadSecCores, cores)
	write(loadSecMemory, mem)
	write(loadSecEnd, "")
	return b.String()
}

// memLine 造一行 `free -b` 的 Mem 输出(total/used,字节)。
func memLine(total, used int64) string {
	return "              total        used        free      shared  buff/cache   available\n" +
		"Mem:      " + strconv.FormatInt(total, 10) + "     " + strconv.FormatInt(used, 10) + "     1000      100      2000      5000"
}

func TestParseHostLoad(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		wantCPU float64
		wantMem float64
		wantFull float64
		wantOK  bool
		cpuOK   bool
		memOK   bool
	}{
		{
			name:     "两维取高者(内存更忙)",
			out:      loadOut("0.52 0.58 0.59 1/123 4567", "4", memLine(8000, 6000)),
			wantCPU:  0.13, wantMem: 0.75, wantFull: 0.75, wantOK: true, cpuOK: true, memOK: true,
		},
		{
			name:     "两维取高者(CPU 更忙且超载)",
			out:      loadOut("8 4 2 1/123 4567", "4", memLine(8000, 1000)),
			wantCPU:  2, wantMem: 0.125, wantFull: 2, wantOK: true, cpuOK: true, memOK: true,
		},
		{
			name:     "uptime 回退格式(macOS 无 /proc)",
			out:      loadOut("14:23:01 up 3 days, 2:04, 1 user, load average: 1.50, 1.00, 0.50", "2", ""),
			wantCPU:  0.75, wantFull: 0.75, wantOK: true, cpuOK: true,
		},
		{
			name:     "无 free → 仅 CPU 维度可用",
			out:      loadOut("2.0 1 1 1/2 3", "4", ""),
			wantCPU:  0.5, wantFull: 0.5, wantOK: true, cpuOK: true,
		},
		{
			name:     "无核数 → 仅内存维度可用",
			out:      loadOut("2.0 1 1 1/2 3", "", memLine(1000, 900)),
			wantMem:  0.9, wantFull: 0.9, wantOK: true, memOK: true,
		},
		{
			name:    "全段空 → 探测失败",
			out:     loadOut("", "", ""),
			wantOK:  false,
		},
		{
			name:     "核数为 0 → CPU 维不可用",
			out:      loadOut("2.0 1 1 1/2 3", "0", memLine(1000, 100)),
			wantMem:  0.1, wantFull: 0.1, wantOK: true, memOK: true,
		},
		{
			name:    "free 的 Mem total 为 0 → 内存维不可用(仅剩 CPU)",
			out:     loadOut("1.0 1 1 1/2 3", "2", memLine(0, 0)),
			wantCPU: 0.5, wantFull: 0.5, wantOK: true, cpuOK: true,
		},
		{
			name:   "负载段是垃圾文本 → 两维皆不可用",
			out:    loadOut("not-a-number", "abc", "Mem:  x y"),
			wantOK: false,
		},
		{
			name:    "空输出",
			out:     "",
			wantOK:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := parseHostLoad(tc.out)
			if h.cpuOK != tc.cpuOK {
				t.Errorf("cpuOK = %v, want %v", h.cpuOK, tc.cpuOK)
			}
			if tc.cpuOK && !almostEqual(h.cpuRatio, tc.wantCPU) {
				t.Errorf("cpuRatio = %v, want %v", h.cpuRatio, tc.wantCPU)
			}
			if h.memOK != tc.memOK {
				t.Errorf("memOK = %v, want %v", h.memOK, tc.memOK)
			}
			if tc.memOK && !almostEqual(h.memRatio, tc.wantMem) {
				t.Errorf("memRatio = %v, want %v", h.memRatio, tc.wantMem)
			}
			f, ok := h.fullness()
			if ok != tc.wantOK {
				t.Fatalf("fullness ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && !almostEqual(f, tc.wantFull) {
				t.Errorf("fullness = %v, want %v", f, tc.wantFull)
			}
		})
	}
}

// TestProbeHostLoadStreamsCmdLog 负载探测经 s.exec → 采集命令与输出回流步骤日志
// (「用什么探测的」可见,与部署命令同口径);命令归属到被探测机器的单机作用域。
func TestProbeHostLoadStreamsCmdLog(t *testing.T) {
	st := loadStubTarget(t, map[string]string{"s1": loadOut("0.5 0.4 0.3 1/2 3", "4", memLine(8000, 2000))}, nil)
	svc := New(st, nil).(*service)

	var mu sync.Mutex
	var lines []string
	ctx := WithCmdLog(context.Background(), func(stream, machine, text string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, stream+"|"+machine+"|"+text)
	})
	h, ok := svc.probeHostLoad(scopeCmdLog(ctx, "s1"), "s1")
	if !ok {
		t.Fatalf("探测应成功: %+v", h)
	}
	joined := strings.Join(lines, "\n")
	// 命令回显口径与部署命令一致:sh -c 展示脚本本体(displayCmd)。
	if !strings.Contains(joined, "stdout|s1|$ echo \"##PW:loadavg\"") || !strings.Contains(joined, "free -b 2>/dev/null") {
		t.Fatalf("应回显采集命令(脚本本体): %q", joined)
	}
	if !strings.Contains(joined, "stdout|s1|"+loadMarker+loadSecLoadavg) || !strings.Contains(joined, "0.5 0.4 0.3") {
		t.Fatalf("采集输出应回流且带机器归属: %q", joined)
	}
}

// TestHostLoadArgsStaticArray 采集命令必须是 array 化的 sh -c 静态脚本(AC-SEC-02:无参数拼接面)。
func TestHostLoadArgsStaticArray(t *testing.T) {
	cmd := hostLoadArgs()
	if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("want [sh -c <script>], got %v", cmd)
	}
	for _, frag := range []string{"/proc/loadavg", "nproc", "free -b"} {
		if !strings.Contains(cmd[2], frag) {
			t.Errorf("脚本应含 %q,got %q", frag, cmd[2])
		}
	}
	// 纯静态:两次调用逐字节一致(不含任何运行时输入)。
	if strings.Join(hostLoadArgs(), "|") != strings.Join(cmd, "|") {
		t.Fatal("采集脚本必须是静态文本")
	}
}

func TestMaxTargetsLimit(t *testing.T) {
	cases := map[string]int{
		"":          0,
		"  ":        0,
		"3":         3,
		" 4 ":       4,
		"0":         0,
		"-1":        0,
		"abc":       0,
		"1.5":       0,
		"999999999": 999999999,
	}
	for raw, want := range cases {
		if got := maxTargetsLimit(map[string]string{"maxTargets": raw}); got != want {
			t.Errorf("maxTargetsLimit(%q) = %d, want %d", raw, got, want)
		}
	}
	if got := maxTargetsLimit(nil); got != 0 {
		t.Errorf("cfg 为 nil 应返回 0, got %d", got)
	}
}

// loadStubTarget 按机器**名字**返回预制负载输出(端到端用例里 seedLabeledServer 发的是 uuid,
// 故按 name 索引;手工构造的 servers 直接用 ID 当名字亦可);非负载命令统一成功。
func loadStubTarget(t *testing.T, outputs map[string]string, fail map[string]bool) *stubTarget {
	t.Helper()
	st := &stubTarget{}
	st.execFn = func(serverID string, cmd []string) (*target.ExecResult, error) {
		if !isLoadProbe(cmd) {
			return &target.ExecResult{ExitCode: 0, Stdout: "ok"}, nil
		}
		key := serverID
		if srv, ok := st.servers[serverID]; ok {
			key = srv.Name
		}
		if fail[key] {
			return nil, errors.New("ssh: connection refused")
		}
		out, ok := outputs[key]
		if !ok {
			return &target.ExecResult{ExitCode: 0, Stdout: loadOut("", "", "")}, nil
		}
		return &target.ExecResult{ExitCode: 0, Stdout: out}, nil
	}
	return st
}

// isLoadProbe 判定一条命令是不是本包的负载采集脚本。
func isLoadProbe(cmd []string) bool {
	return len(cmd) == 3 && cmd[0] == "sh" && cmd[1] == "-c" && strings.Contains(cmd[2], loadMarker+loadSecLoadavg)
}

// TestLimitServersByLoadPicksEmptyest 5 台命中 + 上限 3 → 按饱和度升序取最空 3 台;
// 探测失败的两台(连接失败 / 输出无指标)排最后且不被选中。
func TestLimitServersByLoadPicksEmptyest(t *testing.T) {
	st := loadStubTarget(t, map[string]string{
		"w-busy": loadOut("8.0 4 2 1/2 3", "4", memLine(8000, 2000)),  // max(2.0, 0.25) = 2.0
		"w-empty": loadOut("0.40 0.4 0.4 1/2 3", "4", memLine(8000, 800)), // max(0.10, 0.10) = 0.10
		"w-mid":   loadOut("2.0 1 1 1/2 3", "4", memLine(8000, 2400)),  // max(0.50, 0.30) = 0.50
		"w-mem":   loadOut("0.2 0.2 0.2 1/2 3", "4", memLine(8000, 5600)), // max(0.05, 0.70) = 0.70
	}, map[string]bool{"w-down": true})

	svc := New(st, nil).(*service)
	servers := []*target.Server{
		{ID: "w-busy", Name: "w-busy"},
		{ID: "w-down", Name: "w-down"},
		{ID: "w-mid", Name: "w-mid"},
		{ID: "w-empty", Name: "w-empty"},
		{ID: "w-mem", Name: "w-mem"},
	}
	got := svc.limitServersByLoad(context.Background(), servers, 3)
	want := []string{"w-empty", "w-mid", "w-mem"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, namesOf(got))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("入选顺序 = %v, want %v", namesOf(got), want)
		}
	}
}

// TestLimitServersByLoadNoProbeWhenNotNeeded limit ≤ 0 / ≥ 命中数 → 一台都不探测、原样返回。
func TestLimitServersByLoadNoProbeWhenNotNeeded(t *testing.T) {
	var probes int
	st := &stubTarget{}
	st.execFn = func(_ string, cmd []string) (*target.ExecResult, error) {
		if isLoadProbe(cmd) {
			probes++
		}
		return &target.ExecResult{ExitCode: 0}, nil
	}
	svc := New(st, nil).(*service)
	servers := []*target.Server{{ID: "a", Name: "a"}, {ID: "b", Name: "b"}}

	for _, limit := range []int{0, -1, 2, 5} {
		probes = 0
		got := svc.limitServersByLoad(context.Background(), servers, limit)
		if probes != 0 {
			t.Fatalf("limit=%d 不应探测, probes=%d", limit, probes)
		}
		if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
			t.Fatalf("limit=%d 应原样返回, got %v", limit, namesOf(got))
		}
	}
	// 上限 1 → 才探测,且只留一台。
	probes = 0
	got := svc.limitServersByLoad(context.Background(), servers, 1)
	if probes != 2 {
		t.Fatalf("裁切前应对全部 2 台各探测一次, got %d", probes)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 server, got %v", namesOf(got))
	}
}

// TestLimitServersByLoadTiesKeepOrder 同分保持圈选原序(稳定排序);全部探测失败的机器
// 也保持原序并排最后 —— 上限内不得不选它们时,仍按圈选顺序取。
func TestLimitServersByLoadTiesKeepOrder(t *testing.T) {
	out := loadOut("2.0 1 1 1/2 3", "4", memLine(8000, 2000)) // max(0.5, 0.25) = 0.5
	st := loadStubTarget(t, map[string]string{
		"s1": out, "s2": out, "s3": out,
	}, map[string]bool{"s4": true, "s5": true})
	svc := New(st, nil).(*service)
	servers := []*target.Server{
		{ID: "s5", Name: "s5"}, {ID: "s3", Name: "s3"}, {ID: "s1", Name: "s1"},
		{ID: "s4", Name: "s4"}, {ID: "s2", Name: "s2"},
	}
	got := namesOf(svc.limitServersByLoad(context.Background(), servers, 3))
	want := []string{"s3", "s1", "s2"} // 三台同分 → 按圈选出现顺序
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("同分应保序: got %v, want %v", got, want)
	}

	// 上限 4 > 可用 3 台 → 补一台「负载未知」的(圈选里最先出现的 s5),仍排在可用者之后。
	got4 := namesOf(svc.limitServersByLoad(context.Background(), servers, 4))
	if strings.Join(got4, ",") != "s3,s1,s2,s5" {
		t.Fatalf("未知负载应排在可确认者之后: got %v", got4)
	}
}

// TestDeployForStageRespectsMaxTargets 端到端:选择器命中 5 台 + maxTargets=3 →
// 只部署最空的 3 台,deploy_targets 只落 3 行;未入选的 2 台完全不出现。
func TestDeployForStageRespectsMaxTargets(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	st := loadStubTarget(t, map[string]string{
		"c-1": loadOut("6.0 3 2 1/2 3", "4", memLine(8000, 1000)), // 1.5
		"c-2": loadOut("0.8 0.8 0.8 1/2 3", "4", memLine(8000, 800)), // 0.2
		"c-3": loadOut("2.4 2 1 1/2 3", "4", memLine(8000, 1200)), // 0.6/0.15 → 0.6
		"c-4": loadOut("0.4 0.4 0.4 1/2 3", "4", memLine(8000, 400)), // 0.1
		"c-5": loadOut("1.2 1 1 1/2 3", "4", memLine(8000, 600)),  // 0.3/0.075 → 0.3
	}, nil)
	seedLabeledServer(t, st, "c-3", "app")
	seedLabeledServer(t, st, "c-1", "app")
	seedLabeledServer(t, st, "c-5", "app")
	seedLabeledServer(t, st, "c-2", "app")
	seedLabeledServer(t, st, "c-4", "app")
	runID, _ := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop.tar.gz")

	svc := New(st, rsvc)
	res, err := svc.DeployForStage(context.Background(), runID, "app", map[string]string{"maxTargets": "3"}, "")
	if err != nil {
		t.Fatalf("DeployForStage: %v", err)
	}
	got := map[string]bool{}
	for _, r := range res {
		if r.Status != run.TargetSuccess {
			t.Fatalf("入选机器应全部成功: %+v", res)
		}
		got[r.ServerName] = true
	}
	for _, want := range []string{"c-4", "c-2", "c-5"} {
		if !got[want] {
			t.Errorf("最空的 %s 应入选, got %v", want, got)
		}
	}
	for _, skip := range []string{"c-3", "c-1"} {
		if got[skip] {
			t.Errorf("负载更高的 %s 不应入选(上限 3), got %v", skip, got)
		}
	}
	if len(res) != 3 {
		t.Fatalf("want 3 results, got %d", len(res))
	}
	targets, err := rsvc.ListDeployTargets(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListDeployTargets: %v", err)
	}
	if len(targets) != 3 {
		t.Fatalf("未入选机器不应落 deploy_targets, got %+v", targets)
	}
}

// TestDeployForStageWithoutMaxTargetsUnchanged 不配上限 → 命中几台部署几台(回归保护)。
func TestDeployForStageWithoutMaxTargetsUnchanged(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	var probes int
	st := &stubTarget{}
	st.execFn = func(_ string, cmd []string) (*target.ExecResult, error) {
		if isLoadProbe(cmd) {
			probes++
		}
		return &target.ExecResult{ExitCode: 0}, nil
	}
	seedLabeledServer(t, st, "n-1", "app")
	seedLabeledServer(t, st, "n-2", "app")
	runID, _ := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop.tar.gz")

	svc := New(st, rsvc)
	for name, cfg := range map[string]map[string]string{
		"未配置":  nil,
		"空串":   {"maxTargets": ""},
		"非法值":  {"maxTargets": "abc"},
		"零":    {"maxTargets": "0"},
		"上限≥命中": {"maxTargets": "5"},
	} {
		probes = 0
		res, err := svc.DeployForStage(context.Background(), runID, "app", cfg, "")
		if err != nil {
			t.Fatalf("%s: DeployForStage: %v", name, err)
		}
		if len(res) != 2 {
			t.Fatalf("%s: 应部署全部 2 台, got %+v", name, res)
		}
		if probes != 0 {
			t.Fatalf("%s: 不该触发负载探测, probes=%d", name, probes)
		}
	}
}

func namesOf(servers []*target.Server) []string {
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		out = append(out, s.Name)
	}
	return out
}

func almostEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
