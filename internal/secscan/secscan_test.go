package secscan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/target"
)

// ---- 测试替身 ----

// fakeVault 是可逆的假密封(前缀标记,足够驱动密封/解封路径)。
type fakeVault struct{}

func (fakeVault) SealSecret(p []byte) ([]byte, error) { return append([]byte("s:"), p...), nil }
func (fakeVault) OpenSecret(s []byte) ([]byte, error) {
	if len(s) > 2 && s[0] == 's' && s[1] == ':' {
		return s[2:], nil
	}
	return nil, errors.New("bad blob")
}

// fakeRunner 按命令回调返回固定结果;记录全部命令供断言。
type fakeRunner struct {
	servers []ServerInfo
	execs   [][]string
	fn      func(cmd []string) (*target.ExecResult, error)
}

func (r *fakeRunner) Servers(context.Context) ([]ServerInfo, error) { return r.servers, nil }
func (r *fakeRunner) Server(_ context.Context, id string) (ServerInfo, error) {
	for _, s := range r.servers {
		if s.ID == id {
			return s, nil
		}
	}
	return ServerInfo{}, ErrServerNotFound
}
func (r *fakeRunner) Exec(_ context.Context, _ string, cmd []string) (*target.ExecResult, error) {
	r.execs = append(r.execs, append([]string{}, cmd...))
	return r.fn(cmd)
}

// fakeArtifacts 是内存 artifact store。
type fakeArtifacts struct{ m map[string][]byte }

func newFakeArtifacts() *fakeArtifacts { return &fakeArtifacts{m: map[string][]byte{}} }
func (a *fakeArtifacts) Put(r io.Reader) (string, int64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", 0, err
	}
	k := fmt.Sprintf("k%d", len(a.m))
	a.m[k] = b
	return k, int64(len(b)), nil
}
func (a *fakeArtifacts) Open(k string) (io.ReadCloser, error) {
	b, ok := a.m[k]
	if !ok {
		return nil, errors.New("missing key")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// newTestService 构造注入替身的真实 Service(不动内置 seed)。
func newTestService(t *testing.T, st *store.Store, runner Runner) (Service, *fakeArtifacts) {
	t.Helper()
	arts := newFakeArtifacts()
	return New(st.DB, WithVault(fakeVault{}), WithRunner(runner), WithArtifacts(arts)), arts
}

// waitRunDone 轮询等待 run 离开 running(异步 goroutine 完成)。
func waitRunDone(t *testing.T, svc Service, id string) *Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := svc.GetRun(context.Background(), id)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status != StatusRunning {
			return run
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("run 未在期限内完成")
	return nil
}

// ---- 扫描器登记 ----

func TestScannerCreateRegister(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, _ := newTestService(t, st, &fakeRunner{})
		ctx := context.Background()

		// 手动登记:token 密封存在性可见,响应体无 token 字段(领域结构本身不含)。
		inst, err := svc.CreateScanner(ctx, CreateInput{Name: "外部 Trivy", Host: "10.0.0.8", Port: 4954, Token: "sekrit"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if !inst.TokenConfigured || inst.Source != SourceManual {
			t.Fatalf("create 结果不符:%+v", inst)
		}
		if _, err := svc.CreateScanner(ctx, CreateInput{Name: "重复", Host: "10.0.0.8", Port: 4954}); err != ErrEndpointTaken {
			t.Fatalf("同地址应 ErrEndpointTaken,得 %v", err)
		}

		// 应用市场 Register:同地址 → 原地轮换令牌(Updated=true);新地址 → appstore 条目。
		res, err := svc.Register(ctx, RegisterInput{Name: "Trivy Server · 机器A", Host: "10.0.0.8", Port: 4954, Token: "tok2", ServerID: "s1", StackName: "trivy-server"})
		if err != nil || !res.Updated || res.Instance.ID != inst.ID {
			t.Fatalf("register 应原地轮换:%v %+v", err, res)
		}
		res, err = svc.Register(ctx, RegisterInput{Name: "Trivy Server · 机器B", Host: "10.0.0.9", Port: 4954, Token: "tok3", ServerID: "s2", StackName: "trivy-server"})
		if err != nil || res.Updated || res.Instance.Source != SourceAppStore {
			t.Fatalf("register 新条目:%v %+v", err, res)
		}

		// Update:Token 为 nil 保持既有令牌配置。
		name := "改名"
		updated, err := svc.UpdateScanner(ctx, inst.ID, UpdateInput{Name: &name})
		if err != nil || !updated.TokenConfigured {
			t.Fatalf("update 应保持令牌:%v %+v", err, updated)
		}

		if err := svc.DeleteScanner(ctx, inst.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
	})
}

// ---- 定时规则 ----

func TestScheduleSave(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, _ := newTestService(t, st, &fakeRunner{})
		ctx := context.Background()

		// 未保存过 → 默认关闭 + 每日 03:00 + trivy。
		sch, err := svc.GetSchedule(ctx)
		if err != nil || sch.Enabled || sch.Expression != "0 3 * * *" {
			t.Fatalf("默认规则不符:%v %+v", err, sch)
		}
		// 非法 cron / 空类型 → ErrInvalidInput。
		if _, err := svc.SaveSchedule(ctx, ScheduleInput{Enabled: true, Expression: "bad", ScanTypes: []string{"trivy"}}); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("非法 cron 应 ErrInvalidInput,得 %v", err)
		}
		if _, err := svc.SaveSchedule(ctx, ScheduleInput{Enabled: true, Expression: "0 4 * * *", ScanTypes: nil}); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("空类型应 ErrInvalidInput,得 %v", err)
		}
		// 合法保存(含非法类型过滤 + 排序归一)后可读回。
		sch, err = svc.SaveSchedule(ctx, ScheduleInput{Enabled: true, Expression: "0 4 * * *", ScanTypes: []string{"nuclei", "trivy", "bogus"}})
		if err != nil || !sch.Enabled || sch.Expression != "0 4 * * *" || len(sch.ScanTypes) != 2 || sch.ScanTypes[0] != ScanTypeTrivy {
			t.Fatalf("保存结果不符:%v %+v", err, sch)
		}
		sch2, _ := svc.GetSchedule(ctx)
		if !sch2.Enabled || len(sch2.ScanTypes) != 2 {
			t.Fatalf("读回不符:%+v", sch2)
		}
		if err := svc.TouchScheduleLastRun(ctx, time.Now()); err != nil {
			t.Fatalf("touch: %v", err)
		}
	})
}

// ---- 解析器(纯函数) ----

const trivySample = `{"SchemaVersion":2,"ArtifactName":"alpine:3.10","Results":[
 {"Target":"alpine:3.10 (alpine 3.10.2)","Class":"os-pkgs","Vulnerabilities":[
  {"VulnerabilityID":"CVE-2019-1549","PkgName":"openssl","InstalledVersion":"1.1.1c-r0","FixedVersion":"1.1.1d-r0","Severity":"HIGH","Title":"openssl: memory leak","PrimaryURL":"https://avd.aquasec.com/nvd/cve-2019-1549"},
  {"VulnerabilityID":"CVE-2020-99999","PkgName":"bash","InstalledVersion":"5.0","Severity":"CRITICAL"}]},
 {"Target":"node","Class":"lang-pkgs","Vulnerabilities":[
  {"VulnerabilityID":"GHSA-x","PkgName":"lodash","Severity":"","PrimaryURL":"https://github.com/advisories/GHSA-x"}]}]}`

const nucleiSample = `{"template":"http/exposures/docker.yaml","template-id":"docker-detect","info":{"name":"Docker Detection","severity":"info","reference":["https://localhost"]},"host":"http://10.0.0.1","matched-at":"http://10.0.0.1:2375"}
[WRN] some noise line
{"template-id":"tech-nginx","info":{"name":"Nginx Detection","severity":"info"},"host":"http://10.0.0.1","matched-at":"http://10.0.0.1"}`

func TestParsers(t *testing.T) {
	fs, err := TrivyFindings([]byte(trivySample), "host")
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 3 {
		t.Fatalf("应有 3 条 finding,得 %d", len(fs))
	}
	if fs[0].Severity != SeverityHigh || fs[0].Target != "openssl" || fs[0].FixedVersion != "1.1.1d-r0" || fs[0].URL == "" {
		t.Fatalf("trivy finding 映射不符:%+v", fs[0])
	}
	// 空严重度 → unknown;题图回退到 PrimaryURL。
	if fs[2].Severity != SeverityUnknown || fs[2].Title != fs[2].URL {
		t.Fatalf("空严重度/题图回退不符:%+v", fs[2])
	}

	nf, err := NucleiFindings([]byte(nucleiSample))
	if err != nil || len(nf) != 2 {
		t.Fatalf("nuclei 应解析 2 条(噪声行跳过):%v %d", err, len(nf))
	}
	if nf[0].ID != "docker-detect" || nf[0].Source != "nuclei" || nf[0].Target != "http://10.0.0.1:2375" || nf[0].URL != "https://localhost" {
		t.Fatalf("nuclei finding 映射不符:%+v", nf[0])
	}

	// Summarize / FilterFindings / SortFindings。
	all := append(append([]Finding{}, fs...), nf...)
	s := Summarize(all)
	if s.Critical != 1 || s.High != 1 || s.Unknown != 1 || s.Info != 2 || s.Total != 5 {
		t.Fatalf("汇总不符:%+v", s)
	}
	SortFindings(all)
	if all[0].Severity != SeverityCritical {
		t.Fatalf("排序后 critical 应在前:%+v", all[0])
	}
	total, page := FilterFindings(all, FindingFilter{Severity: SeverityInfo, Limit: 1})
	if total != 2 || len(page) != 1 {
		t.Fatalf("过滤分页不符:%d %d", total, len(page))
	}
	total, page = FilterFindings(all, FindingFilter{Query: "openssl"})
	if total != 1 || page[0].Target != "openssl" {
		t.Fatalf("关键词过滤不符:%d", total)
	}
	total, _ = FilterFindings(all, FindingFilter{Source: "image"})
	if total != 0 {
		t.Fatalf("来源前缀过滤不符:%d", total)
	}
}

// ---- 扫描生命周期(异步执行 + 明细读取) ----

func TestScanLifecycleTrivy(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		runner := &fakeRunner{
			servers: []ServerInfo{{ID: "s1", Name: "机器A", Host: "10.0.0.1"}},
			fn: func(cmd []string) (*target.ExecResult, error) {
				joined := strings.Join(cmd, " ")
				switch {
				case strings.Contains(joined, " rootfs "):
					return &target.ExecResult{ExitCode: 0, Stdout: trivySample}, nil
				case joined == "docker ps --format {{.Image}}":
					return &target.ExecResult{ExitCode: 0, Stdout: "alpine:3.10\nnginx:1.25\nalpine:3.10\n"}, nil
				case strings.Contains(joined, " image "):
					return &target.ExecResult{ExitCode: 1, Stderr: "image scan boom"}, nil // 镜像失败 → 部分成功
				default:
					return &target.ExecResult{ExitCode: 0, Stdout: "{}"}, nil
				}
			},
		}
		svc, _ := newTestService(t, st, runner)
		ctx := context.Background()

		run, err := svc.StartScan(ctx, ScanInput{ServerID: "s1", ScanType: ScanTypeTrivy})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		done := waitRunDone(t, svc, run.ID)
		if done.Status != StatusDone {
			t.Fatalf("主机扫描成功即应 done(镜像失败只记 notes):%+v", done)
		}
		if done.Summary.Critical != 1 || done.Summary.High != 1 || done.Summary.Total != 3 {
			t.Fatalf("summary 不符:%+v", done.Summary)
		}
		// 明细:severity 过滤 + 全量计数;host 来源命中。
		page, err := svc.RunFindings(ctx, run.ID, FindingFilter{Severity: SeverityHigh})
		if err != nil || page.Total != 1 || page.Items[0].Target != "openssl" || page.Summary.Total != 3 {
			t.Fatalf("明细过滤不符:%v %+v", err, page)
		}
		// 服务器不可用/PVE 拒绝。
		if _, err := svc.StartScan(ctx, ScanInput{ServerID: "nope", ScanType: ScanTypeTrivy}); err != ErrServerNotFound {
			t.Fatalf("不存在服务器应 ErrServerNotFound,得 %v", err)
		}
	})
}

func TestScanLifecycleNucleiAndFailures(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		pve := ServerInfo{ID: "pve", Name: "PVE", Host: "10.9.9.9", IsPVE: true}
		runner := &fakeRunner{
			servers: []ServerInfo{{ID: "s1", Name: "机器A", Host: "10.0.0.1"}, pve},
			fn: func(cmd []string) (*target.ExecResult, error) {
				joined := strings.Join(cmd, " ")
				if strings.Contains(joined, "nuclei") {
					return &target.ExecResult{ExitCode: 0, Stdout: nucleiSample}, nil
				}
				return &target.ExecResult{ExitCode: 127, Stderr: "docker: not found"}, nil
			},
		}
		svc, _ := newTestService(t, st, runner)
		ctx := context.Background()

		if _, err := svc.StartScan(ctx, ScanInput{ServerID: "pve", ScanType: ScanTypeNuclei}); err != ErrServerNotTarget {
			t.Fatalf("PVE 应 ErrServerNotTarget,得 %v", err)
		}
		// 非法目标 URL fail fast。
		if _, err := svc.StartScan(ctx, ScanInput{ServerID: "s1", ScanType: ScanTypeNuclei, Targets: []string{"ftp://x"}}); err == nil {
			t.Fatal("非法目标 URL 应报错")
		}
		// nuclei 正常路径。
		run, err := svc.StartScan(ctx, ScanInput{ServerID: "s1", ScanType: ScanTypeNuclei})
		if err != nil {
			t.Fatal(err)
		}
		done := waitRunDone(t, svc, run.ID)
		if done.Status != StatusDone || done.Summary.Info != 2 {
			t.Fatalf("nuclei 结果不符:%+v", done)
		}
		// trivy 在该机 docker 不可用 → failed + 人话错误。
		run, err = svc.StartScan(ctx, ScanInput{ServerID: "s1", ScanType: ScanTypeTrivy})
		if err != nil {
			t.Fatal(err)
		}
		done = waitRunDone(t, svc, run.ID)
		if done.Status != StatusFailed || !strings.Contains(done.Error, "docker") {
			t.Fatalf("docker 不可用应 failed:%+v", done)
		}
		// 概览聚合:s1 的 trivy failed + nuclei done。
		ov, err := svc.Overview(ctx)
		if err != nil || len(ov.Servers) != 1 {
			t.Fatalf("overview:%v %+v", err, ov)
		}
		if ov.Servers[0].Trivy == nil || ov.Servers[0].Trivy.Status != StatusFailed || ov.Servers[0].Nuclei == nil {
			t.Fatalf("overview 每机最新不符:%+v", ov.Servers[0])
		}
		// 陈旧 running 标记。
		if err := svc.MarkStaleRuns(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// ---- 报告数据组装 ----

func TestCollectReport(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		runner := &fakeRunner{
			servers: []ServerInfo{{ID: "s1", Name: "机器A", Host: "10.0.0.1"}},
			fn: func(cmd []string) (*target.ExecResult, error) {
				if strings.Contains(strings.Join(cmd, " "), " rootfs ") {
					return &target.ExecResult{ExitCode: 0, Stdout: trivySample}, nil
				}
				return &target.ExecResult{ExitCode: 0, Stdout: ""}, nil
			},
		}
		svc, _ := newTestService(t, st, runner)
		ctx := context.Background()
		run, err := svc.StartScan(ctx, ScanInput{ServerID: "s1", ScanType: ScanTypeTrivy})
		if err != nil {
			t.Fatal(err)
		}
		waitRunDone(t, svc, run.ID)

		reps, err := svc.CollectReport(ctx, "")
		if err != nil || len(reps) != 1 || reps[0].Trivy == nil {
			t.Fatalf("报告数据不符:%v %+v", err, reps)
		}
		if reps[0].Trivy.Doc == nil || len(reps[0].Trivy.Doc.Findings) != 3 {
			t.Fatalf("报告明细缺失:%+v", reps[0].Trivy)
		}
		if reps, _ = svc.CollectReport(ctx, "s1"); len(reps) != 1 || reps[0].ServerName != "机器A" {
			t.Fatalf("单机报告不符:%+v", reps)
		}
	})
}
