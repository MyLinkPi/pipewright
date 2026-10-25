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
	servers     []ServerInfo
	execs       [][]string
	execServers []string
	fn          func(cmd []string) (*target.ExecResult, error)
	// streamFn/stdinFn 是流式注入点(缺省 = 调用即报错,防测试路径静默走偏)。
	streamFn func(cmd []string) (io.ReadCloser, error)
	stdinFn  func(cmd []string, stdin io.Reader) (*target.ExecResult, error)
	streams  [][]string
	stdinCmd [][]string
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
func (r *fakeRunner) Exec(_ context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	r.execs = append(r.execs, append([]string{}, cmd...))
	r.execServers = append(r.execServers, serverID)
	return r.fn(cmd)
}

func (r *fakeRunner) ExecStream(_ context.Context, _ string, cmd []string) (io.ReadCloser, error) {
	r.streams = append(r.streams, append([]string{}, cmd...))
	if r.streamFn == nil {
		return nil, errors.New("fakeRunner: ExecStream 未配置")
	}
	return r.streamFn(cmd)
}
func (r *fakeRunner) ExecWithStdin(_ context.Context, _ string, cmd []string, stdin io.Reader) (*target.ExecResult, error) {
	r.stdinCmd = append(r.stdinCmd, append([]string{}, cmd...))
	if r.stdinFn == nil {
		return nil, errors.New("fakeRunner: ExecWithStdin 未配置")
	}
	return r.stdinFn(cmd, stdin)
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
		res, err := svc.Register(ctx, RegisterInput{Kind: KindTrivyServer, Name: "Trivy Server · 机器A", Host: "10.0.0.8", Port: 4954, Token: "tok2", ServerID: "s1", StackName: "trivy-server"})
		if err != nil || !res.Updated || res.Instance.ID != inst.ID {
			t.Fatalf("register 应原地轮换:%v %+v", err, res)
		}
		res, err = svc.Register(ctx, RegisterInput{Kind: KindTrivyServer, Name: "Trivy Server · 机器B", Host: "10.0.0.9", Port: 4954, Token: "tok3", ServerID: "s2", StackName: "trivy-server"})
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

		reps, err := svc.CollectReport(ctx, nil)
		if err != nil || len(reps) != 1 || reps[0].Trivy == nil {
			t.Fatalf("报告数据不符:%v %+v", err, reps)
		}
		if reps[0].Trivy.Doc == nil || len(reps[0].Trivy.Doc.Findings) != 3 {
			t.Fatalf("报告明细缺失:%+v", reps[0].Trivy)
		}
		if reps, _ = svc.CollectReport(ctx, []string{"s1"}); len(reps) != 1 || reps[0].ServerName != "机器A" {
			t.Fatalf("单机报告不符:%+v", reps)
		}
		// 多选 + 未知机器跳过(不炸整份报告)。
		if reps, _ = svc.CollectReport(ctx, []string{"s1", "gone", "s1"}); len(reps) != 1 {
			t.Fatalf("多选报告应去重且跳过未知机器:%+v", reps)
		}
	})
}

// ---- 客户端本地模式:DB 镜像仓库注入 + 失败备注取 stderr 末尾 ----

func TestTrivyDBRepositoryInjection(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		runner := &fakeRunner{
			servers: []ServerInfo{{ID: "s1", Name: "机器A", Host: "10.0.0.1"}},
			fn: func(cmd []string) (*target.ExecResult, error) {
				// 主机扫描失败:stderr 前半是拉镜像进度,末尾才是 FATAL(真实场景样本)。
				if strings.Contains(strings.Join(cmd, " "), " rootfs ") {
					progress := strings.Repeat("Pulling fs layer progress ... ", 20)
					return &target.ExecResult{ExitCode: 1, Stderr: progress + "FATAL Fatal error: init error: DB error: failed to download vulnerability DB"}, nil
				}
				return &target.ExecResult{ExitCode: 0, Stdout: "{}"}, nil
			},
		}
		svc := New(st.DB, WithVault(fakeVault{}), WithRunner(runner), WithArtifacts(newFakeArtifacts()),
			WithTrivyDBRepository("ghcr.nju.edu.cn/aquasecurity/trivy-db"))
		run, err := svc.StartScan(context.Background(), ScanInput{ServerID: "s1", ScanType: ScanTypeTrivy})
		if err != nil {
			t.Fatal(err)
		}
		done := waitRunDone(t, svc, run.ID)
		// 镜像扫描在桩里成功 → 整体 done + 部分失败备注;备注含 FATAL 真因(取尾部),
		// 不被拉取进度截断。
		if done.Status != StatusDone || done.Error == "" {
			t.Fatalf("应 done 带部分失败备注:%+v", done)
		}
		if !strings.Contains(done.Error, "FATAL") || !strings.Contains(done.Error, "failed to download vulnerability DB") {
			t.Fatalf("失败备注应含末尾 FATAL 行:%s", done.Error)
		}
		// 客户端命令注入了 DB 镜像仓库 env(本地模式免 ghcr.io)。
		injected := false
		for _, cmd := range runner.execs {
			for i, c := range cmd {
				if c == "TRIVY_DB_REPOSITORY=ghcr.nju.edu.cn/aquasecurity/trivy-db" && i > 0 && cmd[i-1] == "-e" {
					injected = true
				}
			}
		}
		if !injected {
			t.Fatalf("trivy 命令应注入 -e TRIVY_DB_REPOSITORY:%v", runner.execs[0])
		}
		// 未配置时不注入(默认命令形状不变)。
		plain := &fakeRunner{
			servers: runner.servers,
			fn: func(cmd []string) (*target.ExecResult, error) {
				return &target.ExecResult{ExitCode: 0, Stdout: "{}"}, nil
			},
		}
		svc2 := New(st.DB, WithVault(fakeVault{}), WithRunner(plain), WithArtifacts(newFakeArtifacts()))
		if _, err := svc2.StartScan(context.Background(), ScanInput{ServerID: "s1", ScanType: ScanTypeTrivy}); err != nil {
			t.Fatal(err)
		}
		for _, cmd := range plain.execs {
			for _, c := range cmd {
				if strings.HasPrefix(c, "TRIVY_DB_REPOSITORY=") {
					t.Fatalf("未配置不应注入 env:%v", c)
				}
			}
		}
	})
}

// ---- trivy 主机包代扫(目标机无 docker,经 Trivy Server 宿主机) ----

func TestTrivyWithoutDockerViaServerHost(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		runner := &fakeRunner{
			servers: []ServerInfo{{ID: "s1", Name: "边缘机", Host: "10.0.0.1"}, {ID: "s2", Name: "中心机", Host: "10.0.0.2"}},
			fn: func(cmd []string) (*target.ExecResult, error) {
				joined := strings.Join(cmd, " ")
				switch {
				case cmd[0] == "ls":
					return &target.ExecResult{ExitCode: 2, Stdout: "/etc/os-release\n/var/lib/dpkg/status\n"}, nil
				case strings.Contains(joined, " rootfs ") || strings.HasPrefix(joined, "docker ps"):
					return &target.ExecResult{ExitCode: 127, Stderr: "bash: docker: command not found"}, nil
				default:
					return &target.ExecResult{ExitCode: 0, Stdout: "{}"}, nil
				}
			},
			streamFn: func(cmd []string) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("TARBITS")), nil
			},
			stdinFn: func(cmd []string, stdin io.Reader) (*target.ExecResult, error) {
				// 中继通道必须通:目标机 tar 流要真能喂进代扫容器 stdin。
				if _, err := io.ReadAll(stdin); err != nil {
					return nil, err
				}
				return &target.ExecResult{ExitCode: 0, Stdout: trivySample}, nil
			},
		}
		svc, _ := newTestService(t, st, runner)
		ctx := context.Background()
		if _, err := svc.Register(ctx, RegisterInput{Kind: KindTrivyServer, Name: "中心 Trivy", Host: "10.0.0.2", Port: 4954, Token: "tok", ServerID: "s2", StackName: "trivy-server"}); err != nil {
			t.Fatal(err)
		}

		run, err := svc.StartScan(ctx, ScanInput{ServerID: "s1", ScanType: ScanTypeTrivy})
		if err != nil {
			t.Fatal(err)
		}
		done := waitRunDone(t, svc, run.ID)
		if done.Status != StatusDone {
			t.Fatalf("目标机无 docker 应经 Server 宿主机代跑成功,得:%+v", done)
		}
		if done.Summary.Total != 3 {
			t.Fatalf("代扫 summary 不符:%+v", done.Summary)
		}
		// 代扫命令落在 Server 宿主机侧,经环境变量注入接入点与令牌;镜像 ENTRYPOINT 是
		// trivy 本体,必须 --entrypoint sh 才能跑「解包+扫描」脚本(sh 若出现在镜像名后面
		// 会被当成 trivy 子命令秒退——零结果事故的回归防线)。
		if len(runner.stdinCmd) == 0 {
			t.Fatalf("应经 ExecWithStdin 代扫:%v", runner.stdinCmd)
		}
		cmd := runner.stdinCmd[0]
		epIdx, imgIdx := -1, -1
		for i, c := range cmd {
			switch {
			case c == "--entrypoint":
				epIdx = i
			case strings.HasPrefix(c, "aquasec/trivy"):
				imgIdx = i
			}
		}
		if epIdx < 0 || imgIdx <= 0 || cmd[epIdx+1] != "sh" || cmd[imgIdx+1] != "-c" {
			t.Fatalf("代扫命令形状不符(须 --entrypoint sh … <镜像> -c <脚本>):%v", cmd)
		}
		joined := strings.Join(cmd, " ")
		if !strings.Contains(joined, "TRIVY_SERVER=http://10.0.0.2:4954") ||
			!strings.Contains(joined, "TRIVY_TOKEN=tok") ||
			!strings.Contains(joined, "tar -xf - -C /scan && ls -laR /scan >&2 && trivy rootfs") {
			t.Fatalf("代扫命令内容不符:%v", cmd)
		}
		if len(runner.streams) == 0 || runner.streams[0][0] != "tar" {
			t.Fatalf("应先从目标机流式打包元数据:%v", runner.streams)
		}
		// 报告注明代跑与镜像扫描跳过。
		page, err := svc.RunFindings(ctx, run.ID, FindingFilter{})
		if err != nil || page.Total != 3 {
			t.Fatalf("代扫明细不符:%v %+v", err, page)
		}
		// 执行日志落 artifact:本地尝试 + 探测/中继/代扫都可见,查看日志可诊断。
		logsView, err := svc.RunLogs(ctx, run.ID)
		if err != nil || len(logsView.Logs) < 4 {
			t.Fatalf("代扫日志不符:%v %+v", err, logsView)
		}
		steps := make(map[string]StepLog, len(logsView.Logs))
		for _, l := range logsView.Logs {
			steps[l.Step] = l
		}
		relay, ok := steps["元数据 tar 中继(目标机 → 面板 → Server 宿主机)"]
		if !ok || relay.Stdout != "共中继 7 字节" {
			t.Fatalf("中继日志不符:%+v", relay)
		}
		if _, ok := steps["代扫容器(解包 + trivy rootfs /scan)"]; !ok {
			t.Fatalf("缺代扫容器日志:%+v", logsView.Logs)
		}
	})
}

func TestTrivyWithoutDockerNoServerHost(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		ctx := context.Background()
		dockerDown := func(cmd []string) (*target.ExecResult, error) {
			return &target.ExecResult{ExitCode: 127, Stderr: "bash: docker: command not found"}, nil
		}
		// 未接入 Trivy Server → 原 docker 指引。
		runner := &fakeRunner{servers: []ServerInfo{{ID: "s1", Name: "边缘机", Host: "10.0.0.1"}}, fn: dockerDown}
		svc, _ := newTestService(t, st, runner)
		run, err := svc.StartScan(ctx, ScanInput{ServerID: "s1", ScanType: ScanTypeTrivy})
		if err != nil {
			t.Fatal(err)
		}
		if done := waitRunDone(t, svc, run.ID); done.Status != StatusFailed ||
			!strings.Contains(done.Error, "应用市场部署 Trivy Server") {
			t.Fatalf("未接入 Trivy Server 应给人话指引:%+v", done)
		}

		// 手工登记(无宿主机 ServerID)→ 无法代跑的专门指引。
		manual := &fakeRunner{servers: runner.servers, fn: dockerDown}
		svc2, _ := newTestService(t, st, manual)
		if _, err := svc2.CreateScanner(ctx, CreateInput{Name: "外部 Trivy", Host: "10.9.9.9", Port: 4954}); err != nil {
			t.Fatal(err)
		}
		run, err = svc2.StartScan(ctx, ScanInput{ServerID: "s1", ScanType: ScanTypeTrivy})
		if err != nil {
			t.Fatal(err)
		}
		done := waitRunDone(t, svc2, run.ID)
		if done.Status != StatusFailed || !strings.Contains(done.Error, "手工接入") {
			t.Fatalf("手工登记 Trivy Server 无法代跑应给人话指引:%+v", done)
		}
	})
}

// ---- Nuclei 经常驻 runner 执行(目标机无需 docker)----

func TestNucleiViaRunner(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		runner := &fakeRunner{
			servers: []ServerInfo{
				{ID: "s1", Name: "目标机", Host: "10.0.0.1"},
				{ID: "s2", Name: "Runner机", Host: "10.0.0.2"},
			},
			fn: func(cmd []string) (*target.ExecResult, error) {
				joined := strings.Join(cmd, " ")
				switch {
				case strings.HasPrefix(joined, "docker ps --filter name=nuclei-runner"):
					return &target.ExecResult{ExitCode: 0, Stdout: "nuclei-runner-nuclei-runner-1\nnuclei-runner-nuclei-runner-1-2"}, nil
				case strings.HasPrefix(joined, "docker exec nuclei-runner-nuclei-runner-1 "):
					return &target.ExecResult{ExitCode: 0, Stdout: nucleiSample}, nil
				default:
					return &target.ExecResult{ExitCode: 127, Stderr: "docker: not found"}, nil
				}
			},
		}
		svc, _ := newTestService(t, st, runner)
		ctx := context.Background()

		// 登记常驻 runner(应用市场部署联动语义):kind 校验 + port 归零 + 唯一。
		res, err := svc.Register(ctx, RegisterInput{
			Kind: KindNucleiRunner, Name: "Nuclei Runner · Runner机",
			Host: "10.0.0.2", ServerID: "s2", StackName: "nuclei-runner",
		})
		if err != nil || res.Instance.Port != 0 || res.Instance.Source != SourceAppStore {
			t.Fatalf("runner 登记不符:%v %+v", err, res.Instance)
		}
		if _, err := svc.Register(ctx, RegisterInput{
			Kind: KindTrivyServer, Name: "x", Host: "10.0.0.9",
		}); err != ErrInvalidPort && err == nil {
			t.Fatalf("trivy-server 缺端口应报错,得 %v", err)
		}

		// 目标机无 docker(default 分支返回 127)仍应扫描成功:执行全走 runner 主机。
		run, err := svc.StartScan(ctx, ScanInput{ServerID: "s1", ScanType: ScanTypeNuclei})
		if err != nil {
			t.Fatal(err)
		}
		done := waitRunDone(t, svc, run.ID)
		if done.Status != StatusDone || done.Summary.Info != 2 {
			t.Fatalf("经 runner 扫描不符:%+v", done)
		}
		if done.ScannerID != res.Instance.ID {
			t.Fatalf("run 应记录 runner 实例:%s", done.ScannerID)
		}
		for i, cmd := range runner.execs {
			if len(cmd) > 0 && (cmd[0] == "docker") && runner.execServers[i] != "s2" {
				t.Fatalf("docker 命令应发往 runner 主机 s2,发往了 %s:%v", runner.execServers[i], cmd)
			}
		}

		// runner 容器不在 → 人话失败。
		runner.fn = func(cmd []string) (*target.ExecResult, error) {
			if strings.HasPrefix(strings.Join(cmd, " "), "docker ps --filter name=nuclei-runner") {
				return &target.ExecResult{ExitCode: 0, Stdout: ""}, nil
			}
			return &target.ExecResult{ExitCode: 1, Stderr: "x"}, nil
		}
		run, err = svc.StartScan(ctx, ScanInput{ServerID: "s1", ScanType: ScanTypeNuclei})
		if err != nil {
			t.Fatal(err)
		}
		done = waitRunDone(t, svc, run.ID)
		if done.Status != StatusFailed || !strings.Contains(done.Error, "runner 容器未在运行") {
			t.Fatalf("runner 缺席应人话失败:%+v", done)
		}
	})
}

// ---- SafeLine WAF:登记/探活/态势 ----

// fakeWafClient 按实例 Token 区分行为(空 Token 场景由 Service 层拦截,不会到这)。
type fakeWafClient struct {
	pings   int
	stats   map[string]*WafStats
	pingErr map[string]bool
}

func (f *fakeWafClient) Ping(ctx context.Context, ep WafEndpoint) *TestResult {
	f.pings++
	if f.pingErr[ep.BaseURL] {
		return &TestResult{Err: errors.New("refused"), Detail: "SafeLine 探活失败:连接被拒绝"}
	}
	return &TestResult{OK: true, Detail: "ok"}
}

func (f *fakeWafClient) Stats(ctx context.Context, ep WafEndpoint) (*WafStats, error) {
	if st, ok := f.stats[ep.BaseURL]; ok {
		return st, nil
	}
	return nil, errors.New("stats unavailable")
}

func (f *fakeWafClient) UpsertCert(ctx context.Context, ep WafEndpoint, name, crt, key string) error {
	return nil
}

func TestSafelineWafOverview(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		runner := &fakeRunner{servers: []ServerInfo{{ID: "s1", Name: "边缘机", Host: "10.0.0.1"}}}
		waf := &fakeWafClient{
			stats:   map[string]*WafStats{"https://10.0.0.1:9443": {BlockedToday: 3, Blocked7d: 21}},
			pingErr: map[string]bool{},
		}
		svc := New(st.DB, WithVault(fakeVault{}), WithRunner(runner), WithArtifacts(newFakeArtifacts()), WithWafClient(waf))
		ctx := context.Background()

		// 登记两台:一台带 Token,一台不带(应用市场联动形态)。
		withTok, err := svc.Register(ctx, RegisterInput{Kind: KindSafeline, Name: "WAF A", Host: "10.0.0.1", Port: 9443, Token: "tk", ServerID: "s1", StackName: "safeline"})
		if err != nil || withTok.Instance.TokenConfigured != true {
			t.Fatalf("登记带 Token 实例:%v", err)
		}
		noTok, err := svc.Register(ctx, RegisterInput{Kind: KindSafeline, Name: "WAF B", Host: "10.0.0.2", Port: 9443, ServerID: "s1", StackName: "safeline-2"})
		if err != nil || noTok.Instance.TokenConfigured {
			t.Fatalf("登记无 Token 实例:%v", err)
		}

		items, err := svc.WafOverview(ctx)
		if err != nil || len(items) != 2 {
			t.Fatalf("WafOverview:%v %+v", err, items)
		}
		byName := map[string]WafInstanceStatus{}
		for _, it := range items {
			byName[it.Name] = it
		}
		a := byName["WAF A"]
		if !a.OK || a.Stats == nil || a.Stats.BlockedToday != 3 || a.Stats.Blocked7d != 21 {
			t.Fatalf("WAF A 态势不符:%+v", a)
		}
		b := byName["WAF B"]
		if b.OK || !strings.Contains(b.Detail, "API Token") {
			t.Fatalf("WAF B 应提示待配置 Token:%+v", b)
		}

		// 推证书适配:无 Token 实例 → 人话错误;带 Token 实例成功(fake)。
		if err := svc.UpsertCert(ctx, noTok.Instance.ID, "a.com", "CRT", "KEY"); err == nil || !strings.Contains(err.Error(), "API Token") {
			t.Fatalf("无 Token 推证书应报错,得 %v", err)
		}
		if err := svc.UpsertCert(ctx, withTok.Instance.ID, "a.com", "CRT", "KEY"); err != nil {
			t.Fatalf("推证书:%v", err)
		}
		// 归属解析。
		sid, err := svc.WafInstanceServerID(ctx, withTok.Instance.ID)
		if err != nil || sid != "s1" {
			t.Fatalf("归属解析:%v %s", err, sid)
		}
	})
}

// ---- 规则库状态与手动更新 ----

func TestScannerLib(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		runner := &fakeRunner{
			servers: []ServerInfo{{ID: "s1", Name: "机A", Host: "10.0.0.1"}},
			fn: func(cmd []string) (*target.ExecResult, error) {
				joined := strings.Join(cmd, " ")
				switch {
				case strings.HasPrefix(joined, "docker ps --filter name="):
					if strings.Contains(joined, "trivy-server") {
						return &target.ExecResult{ExitCode: 0, Stdout: "trivy-server-trivy-server-1"}, nil
					}
					return &target.ExecResult{ExitCode: 0, Stdout: "nuclei-runner-nuclei-runner-1"}, nil
				case strings.HasPrefix(joined, "docker inspect --format {{.State.StartedAt}}"):
					return &target.ExecResult{ExitCode: 0, Stdout: "2026-10-24T10:00:00.123456789Z"}, nil
				case strings.HasPrefix(joined, "docker restart"):
					return &target.ExecResult{ExitCode: 0, Stdout: "trivy-server-trivy-server-1"}, nil
				case strings.Contains(joined, "stat -c %Y /root/nuclei-templates"):
					return &target.ExecResult{ExitCode: 0, Stdout: "1774000000"}, nil
				case strings.Contains(joined, "nuclei -update-templates"):
					return &target.ExecResult{ExitCode: 0, Stdout: "[INF] Templates updated to latest"}, nil
				default:
					return &target.ExecResult{ExitCode: 1, Stderr: "unexpected"}, nil
				}
			},
		}
		svc, _ := newTestService(t, st, runner)
		ctx := context.Background()

		trivyInst, err := svc.Register(ctx, RegisterInput{Kind: KindTrivyServer, Name: "TS", Host: "10.0.0.1", Port: 4954, Token: "t", ServerID: "s1", StackName: "trivy-server"})
		if err != nil {
			t.Fatal(err)
		}
		runnerInst, err := svc.Register(ctx, RegisterInput{Kind: KindNucleiRunner, Name: "NR", Host: "10.0.0.1", ServerID: "s1", StackName: "nuclei-runner"})
		if err != nil {
			t.Fatal(err)
		}
		safeInst, err := svc.Register(ctx, RegisterInput{Kind: KindSafeline, Name: "WAF", Host: "10.0.0.1", Port: 9443, ServerID: "s1", StackName: "safeline"})
		if err != nil {
			t.Fatal(err)
		}

		// trivy:info = 容器启动时间(归一 RFC3339);update = docker restart + note。
		info, err := svc.ScannerLibInfo(ctx, trivyInst.Instance.ID)
		if err != nil || info.UpdatedAt != "2026-10-24T10:00:00Z" {
			t.Fatalf("trivy lib info:%v %+v", err, info)
		}
		info, err = svc.ScannerLibUpdate(ctx, trivyInst.Instance.ID)
		if err != nil || !strings.Contains(info.Note, "最新漏洞库") {
			t.Fatalf("trivy lib update:%v %+v", err, info)
		}

		// nuclei:info = 模板目录 mtime(epoch→RFC3339);update 带输出尾部 note。
		info, err = svc.ScannerLibInfo(ctx, runnerInst.Instance.ID)
		if err != nil || !strings.HasPrefix(info.UpdatedAt, "2026-") {
			t.Fatalf("nuclei lib info:%v %+v", err, info)
		}
		info, err = svc.ScannerLibUpdate(ctx, runnerInst.Instance.ID)
		if err != nil || !strings.Contains(info.Note, "已更新") {
			t.Fatalf("nuclei lib update:%v %+v", err, info)
		}

		// safeline:拒绝(控制台自管理)。
		if _, err := svc.ScannerLibInfo(ctx, safeInst.Instance.ID); err == nil || !strings.Contains(err.Error(), "控制台") {
			t.Fatalf("safeline 应拒绝:%v", err)
		}

		// 无宿主信息的外部实例:拒绝并提示自维护。
		ext, _ := svc.CreateScanner(ctx, CreateInput{Name: "外部", Host: "10.0.0.9", Port: 4954, Token: "x"})
		if _, err := svc.ScannerLibUpdate(ctx, ext.ID); err == nil || !strings.Contains(err.Error(), "自维护") {
			t.Fatalf("外部实例应拒绝:%v", err)
		}
	})
}

// ---- 代扫路径:命令形状回归(镜像 ENTRYPOINT 恒为 trivy,跑 shell 须显式覆盖) ----

// runnerExecForRemote 是代扫测试的命令分发(Exec 与 ExecWithStdin 共用)。
func runnerExecForRemote(cmd []string) (*target.ExecResult, error) {
	joined := strings.Join(cmd, " ")
	switch {
	case strings.HasPrefix(joined, "docker run --rm -i --entrypoint sh"):
		if !strings.Contains(joined, "aquasec/trivy") || !strings.Contains(joined, "-c mkdir -p /scan") {
			return nil, fmt.Errorf("代扫命令形状不符:%s", joined)
		}
		return &target.ExecResult{ExitCode: 0, Stdout: trivySample}, nil
	default:
		return &target.ExecResult{ExitCode: 127, Stderr: "docker: not found"}, nil
	}
}

func TestTrivyRemoteScanCommandShape(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		// 目标机无 docker;Runner 宿主机有 docker 且镜像能产出 trivy JSON。
		runner := &fakeRunner{
			servers: []ServerInfo{
				{ID: "s1", Name: "无docker机", Host: "10.0.0.1"},
				{ID: "s2", Name: "Server宿主", Host: "10.0.0.2"},
			},
			streamFn: func(cmd []string) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("tar-bytes")), nil
			},
			stdinFn: func(cmd []string, _ io.Reader) (*target.ExecResult, error) {
				return runnerExecForRemote(cmd)
			},
			fn: func(cmd []string) (*target.ExecResult, error) {
				joined := strings.Join(cmd, " ")
				switch {
				case strings.HasPrefix(joined, "ls -d /etc/os-release"):
					return &target.ExecResult{ExitCode: 1, Stdout: "/etc/os-release\n/var/lib/dpkg/status\n"}, nil
				case strings.HasPrefix(joined, "docker run --rm -i --entrypoint sh"):
					if !strings.Contains(joined, "aquasec/trivy") || !strings.Contains(joined, "-c mkdir -p /scan") {
						t.Fatalf("代扫命令形状不符:%s", joined)
					}
					return &target.ExecResult{ExitCode: 0, Stdout: trivySample}, nil
				default:
					return &target.ExecResult{ExitCode: 127, Stderr: "docker: not found"}, nil
				}
			},
		}
		svc, _ := newTestService(t, st, runner)
		ctx := context.Background()

		// 登记带宿主的 trivy-server(代扫执行机 = ServerID)。
		if _, err := svc.Register(ctx, RegisterInput{
			Kind: KindTrivyServer, Name: "TS", Host: "10.0.0.2", Port: 4954, Token: "tk",
			ServerID: "s2", StackName: "trivy-server",
		}); err != nil {
			t.Fatal(err)
		}
		run, err := svc.StartScan(ctx, ScanInput{ServerID: "s1", ScanType: ScanTypeTrivy})
		if err != nil {
			t.Fatal(err)
		}
		done := waitRunDone(t, svc, run.ID)
		if done.Status != StatusDone || done.Summary.Critical != 1 {
			t.Fatalf("代扫结果不符:%+v", done)
		}
	})
}
