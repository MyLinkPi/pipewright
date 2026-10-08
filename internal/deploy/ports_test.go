package deploy

// ports_test.go 覆盖容器宿主端口「范围探测自动分配」:
//   - ports 语法解析(显式 host:container / 仅容器端口 / auto:容器端口 / 非法);
//   - autoPortRange 解析与夹紧;
//   - runContainerWithPorts:探测首个空闲端口 → -p host:container(端口在前);
//     docker 报端口占用 → 换候选重试;显式映射不重试;成功日志带映射摘要。

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/target"
)

func TestParsePortSpecs(t *testing.T) {
	specs, err := parsePortSpecs("8080:80, auto:9000  7000")
	if err != nil {
		t.Fatal(err)
	}
	want := []portSpec{{host: 8080, container: 80}, {host: 0, container: 9000}, {host: 0, container: 7000}}
	if len(specs) != len(want) {
		t.Fatalf("解析数不符:%v", specs)
	}
	for i := range want {
		if specs[i] != want[i] {
			t.Fatalf("spec[%d] = %+v, want %+v", i, specs[i], want[i])
		}
	}
	if specs, _ := parsePortSpecs(""); len(specs) != 0 {
		t.Fatalf("空串应无映射:%v", specs)
	}
	for _, bad := range []string{"0:0", "70000", "x:80", "80:", "8080:abc"} {
		if _, err := parsePortSpecs(bad); err == nil {
			t.Fatalf("%q 应报错", bad)
		}
	}
	// "0:8080" 与 "auto:8080" 同义(宿主自动)。
	s, err := parsePortSpecs("0:8080")
	if err != nil || s[0].host != 0 || s[0].container != 8080 {
		t.Fatalf("0:8080 应为自动:%v %v", s, err)
	}
}

func TestAutoPortBounds(t *testing.T) {
	if lo, hi := autoPortBounds(nil); lo != defaultAutoPortLo || hi != defaultAutoPortHi {
		t.Fatalf("缺省段:%d-%d", lo, hi)
	}
	if lo, hi := autoPortBounds(map[string]string{"autoPortRange": "30000-30100"}); lo != 30000 || hi != 30100 {
		t.Fatalf("自定义段:%d-%d", lo, hi)
	}
	if lo, hi := autoPortBounds(map[string]string{"autoPortRange": "30100-30000"}); lo != defaultAutoPortLo || hi != defaultAutoPortHi {
		t.Fatalf("倒置段应回默认:%d-%d", lo, hi)
	}
	if lo, hi := autoPortBounds(map[string]string{"autoPortRange": "1-65535"}); hi-lo > maxAutoPortSpan {
		t.Fatalf("跨度应夹紧:%d-%d", lo, hi)
	}
}

// portExecFn 构造端口探测桩:busy 集合内的端口连接成功(占用),其余连接失败(空闲)。
func portExecFn(busy map[int]bool, runFailOn map[int]bool) func(string, []string) (*target.ExecResult, error) {
	return func(_ string, cmd []string) (*target.ExecResult, error) {
		if len(cmd) >= 3 && cmd[0] == "bash" && cmd[1] == "-c" && strings.Contains(cmd[2], "/dev/tcp/") {
			if p := probePortOf(cmd[2]); p > 0 && busy[p] {
				return &target.ExecResult{ExitCode: 0}, nil // 连接成功 = 占用
			}
			return &target.ExecResult{ExitCode: 1}, nil
		}
		if len(cmd) > 0 && cmd[0] == "docker" && cmd[1] == "run" {
			// docker 仲裁:指定宿主端口若在 runFailOn → 报端口占用。
			for i, a := range cmd {
				if a == "-p" && i+1 < len(cmd) {
					hp := hostPortOfMapping(cmd[i+1])
					if hp > 0 && runFailOn[hp] {
						return &target.ExecResult{ExitCode: 1, Stderr: "docker: Bind for 0.0.0.0:" + strconv.Itoa(hp) + " failed: port is already allocated."}, nil
					}
				}
			}
		}
		return &target.ExecResult{ExitCode: 0}, nil
	}
}

// probePortOf 从探测脚本(exec 3<>/dev/tcp/127.0.0.1/<p> && …)抠出端口号;解析不出 → 0。
func probePortOf(script string) int {
	i := strings.LastIndex(script, "/")
	if i < 0 {
		return 0
	}
	tail := script[i+1:]
	end := strings.IndexAny(tail, " &")
	if end >= 0 {
		tail = tail[:end]
	}
	p, err := strconv.Atoi(tail)
	if err != nil {
		return 0
	}
	return p
}

// hostPortOfMapping 从 "20001:8080" 取宿主端口;格式不符 → 0。
func hostPortOfMapping(m string) int {
	parts := strings.SplitN(m, ":", 2)
	p, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0
	}
	return p
}

func TestRunContainerWithPortsAutoAlloc(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{execFn: portExecFn(map[int]bool{20000: true}, nil)}
	srv := seedServer(t, tgt, "web-1")

	svc := New(tgt, rsvc).(*service)
	specs, _ := parsePortSpecs("8080") // 容器 8080,宿主自动
	bindings, msg, ok := svc.runContainerWithPorts(context.Background(), srv.ID, "app", []string{"-e", "K=V"}, specs, map[string]string{"autoPortRange": "20000-20010"}, "reg/app:1")
	if !ok {
		t.Fatalf("应成功:%s", msg)
	}
	// 20000 被 bash 探测判占用 → 分配 20001。
	if len(bindings) != 1 || bindings[0].host != 20001 || bindings[0].spec.container != 8080 {
		t.Fatalf("应分配 20001:%+v", bindings)
	}
	// docker run 参数序:端口映射在前、runArgs 在后。
	for _, c := range tgt.calls {
		if len(c) > 8 && c[0] == "docker" && c[1] == "run" {
			foundP := false
			for i, a := range c {
				if a == "-p" && c[i+1] == "20001:8080" {
					foundP = true
				}
			}
			if !foundP {
				t.Fatalf("docker run 应带 -p 20001:8080:%v", c)
			}
		}
	}
}

func TestRunContainerWithPortsDockerRetry(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	// bash 探测全部空闲,但 docker 对 20000 报占用(探测/docker 竞态)→ 换 20001。
	tgt := &stubTarget{execFn: portExecFn(nil, map[int]bool{20000: true})}
	srv := seedServer(t, tgt, "web-1")

	svc := New(tgt, rsvc).(*service)
	specs, _ := parsePortSpecs("8080")
	bindings, msg, ok := svc.runContainerWithPorts(context.Background(), srv.ID, "app", nil, specs, map[string]string{"autoPortRange": "20000-20010"}, "reg/app:1")
	if !ok {
		t.Fatalf("应换候选重试成功:%s", msg)
	}
	if bindings[0].host != 20001 {
		t.Fatalf("应换到 20001:%+v", bindings)
	}
	// 第一次 run(20000)失败 + 第二次 run(20001)成功。
	runs := 0
	for _, c := range tgt.calls {
		if len(c) > 0 && c[0] == "docker" && c[1] == "run" {
			runs++
		}
	}
	if runs != 2 {
		t.Fatalf("应恰好两次 docker run,得 %d", runs)
	}
}

func TestRunContainerWithPortsExplicitNoRetry(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	// 显式映射端口占用:重试同因无意义 → 立即失败。
	tgt := &stubTarget{execFn: portExecFn(nil, map[int]bool{8080: true})}
	srv := seedServer(t, tgt, "web-1")

	svc := New(tgt, rsvc).(*service)
	specs, _ := parsePortSpecs("8080:80")
	_, msg, ok := svc.runContainerWithPorts(context.Background(), srv.ID, "app", nil, specs, nil, "reg/app:1")
	if ok || !strings.Contains(msg, "port is already allocated") {
		t.Fatalf("显式占用应直接失败:%v %q", ok, msg)
	}
}

func TestAllocFreePortExhausted(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{execFn: portExecFn(map[int]bool{20000: true, 20001: true}, nil)}
	srv := seedServer(t, tgt, "web-1")
	svc := New(tgt, rsvc).(*service)
	if _, err := svc.allocFreePort(context.Background(), srv.ID, 20000, 20001, nil); err == nil {
		t.Fatalf("全段占用应报错")
	}
}

// TestHealthSpecResolve 健康检查端口自动推导链:显式 healthPort > 部署期端口;推导不出 → nil。
func TestHealthSpecResolve(t *testing.T) {
	// 端口型:仅 path,显式端口优先。
	sp := healthCheckSpecFromCfg(map[string]string{"healthPath": "/healthz", "healthPort": "9000"}, "app")
	hc := sp.resolve(20001)
	if hc == nil || hc.URL != "http://127.0.0.1:9000/healthz" {
		t.Fatalf("显式端口优先:%+v", hc)
	}
	// 无显式端口 → 部署期推导(自动分配出的宿主端口)。
	sp = healthCheckSpecFromCfg(map[string]string{"healthPath": "healthz"}, "app")
	hc = sp.resolve(20001)
	if hc == nil || hc.URL != "http://127.0.0.1:20001/healthz" {
		t.Fatalf("端口应自动推导 + path 归一前导斜杠:%+v", hc)
	}
	// 推导不出 → nil(跳过,与未配置一致)。
	if hc := sp.resolve(0); hc != nil {
		t.Fatalf("无端口可推导应 nil:%+v", hc)
	}
	// path 缺省 = 根(不带尾斜杠,与旧契约一致)。
	sp = healthCheckSpecFromCfg(map[string]string{"healthPort": "8080"}, "app")
	if hc := sp.resolve(0); hc == nil || hc.URL != "http://127.0.0.1:8080" {
		t.Fatalf("缺省 path 应为根 URL:%+v", hc)
	}
	// command / exec / url 原样。
	sp = healthCheckSpecFromCfg(map[string]string{"healthExec": "pg_isready"}, "shop")
	hc = sp.resolve(0)
	if hc == nil || hc.Type != HealthCheckCommand || strings.Join(hc.Command, " ") != "docker exec shop sh -c pg_isready" {
		t.Fatalf("容器内探测:%+v", hc)
	}
	// 未配置 → nil。
	if sp := healthCheckSpecFromCfg(map[string]string{}, "app"); sp != nil {
		t.Fatalf("未配置应 nil:%+v", sp)
	}
	// 显式 DTO 缺 URL → 保留失败语义(resolve 仍构造,探测时报缺 url)。
	fixed := fixedHealthSpec(&HealthCheck{Type: HealthCheckHTTP})
	if hc := fixed.resolve(0); hc == nil || hc.URL != "" {
		t.Fatalf("缺 URL 的显式检查应保留:%+v", hc)
	}
}

// TestDeployForStageRegistersAndPrunes 注册联动:容器部署 ensure + 首装落行 / 全部成功后清理。
func TestDeployForStageRegistersAndPrunes(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{execFn: portExecFn(nil, nil)}
	srv := seedServer(t, tgt, "app-1")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactImage, "reg/shop:1")

	gw := &fakeGateway{refs: nil} // 反查为空 → 回退硬切;成功后 fallback ensure 落行
	svc := New(tgt, rsvc, WithInstanceGateway(gw))
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srv.ID},
		Config: map[string]string{
			"regServiceId": "svc-9", "containerName": "shop", "ports": "8080",
			"autoPortRange": "20000-20010",
		},
	})
	if err != nil || res[0].Status != run.TargetSuccess {
		t.Fatalf("部署应成功:%v %+v", err, res)
	}
	found := false
	for _, e := range gw.ensures {
		if e == "svc-9/"+srv.ID+"/shop" {
			found = true
		}
	}
	if !found {
		t.Fatalf("回退路径成功后应 ensure 实例(带实际宿主端口):%v", gw.ensures)
	}
	// 回归(deployImageOne 值语义):末次 ensure(回退路径成功后)的 hostPort 必须是激活阶段
	// 实际分配的 20000,而非 stage 阶段副本的 0(否则注册落行丢 hostPort,upstream 指向容器端口)。
	// (首次 (8080,0) 是轮转路径的预落行,hostPort 待 Swap/回退 ensure 补齐,属正常。)
	n := len(gw.ensurePorts)
	if n < 2 || gw.ensurePorts[n-2] != 8080 || gw.ensurePorts[n-1] != 20000 {
		t.Fatalf("末次 ensure 端口应为 (8080, 20000):%v", gw.ensurePorts)
	}
}
