package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/target"
)

// --- 纯解析单元测试 ---

func TestParseContainers_WithStateField(t *testing.T) {
	// Docker 20.10+ 的 `docker ps --format {{json .}}`:逐行 JSON,含 State 字段。
	out := `{"ID":"abc123def456","Names":"web","Image":"nginx:latest","State":"running","Status":"Up 2 hours","Ports":"0.0.0.0:80->80/tcp","CreatedAt":"2026-06-01 10:00:00 +0800 CST"}
{"ID":"def789","Names":"/db","Image":"mysql:8","State":"exited","Status":"Exited (0) 3 days ago","Ports":"","CreatedAt":"2026-05-28 09:00:00 +0800 CST"}
`
	got := parseContainers(out)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}
	if got[0].ID != "abc123def456" || got[0].Names != "web" || got[0].State != "running" {
		t.Fatalf("c0 wrong: %+v", got[0])
	}
	if got[0].Image != "nginx:latest" || got[0].Ports != "0.0.0.0:80->80/tcp" {
		t.Fatalf("c0 image/ports wrong: %+v", got[0])
	}
	// 前导 `/` 应去掉。
	if got[1].Names != "db" || got[1].State != "exited" {
		t.Fatalf("c1 wrong: %+v", got[1])
	}
}

func TestParseContainers_OldDockerNoState(t *testing.T) {
	// 旧 Docker 无 State 字段 → 据 Status 前缀推断。
	out := `{"ID":"a1","Names":"running-one","Image":"img","Status":"Up 5 minutes","Ports":"","CreatedAt":""}
{"ID":"a2","Names":"paused-one","Image":"img","Status":"Up 5 minutes (Paused)","Ports":"","CreatedAt":""}
{"ID":"a3","Names":"stopped-one","Image":"img","Status":"Exited (137) 1 hour ago","Ports":"","CreatedAt":""}
{"ID":"a4","Names":"created-one","Image":"img","Status":"Created","Ports":"","CreatedAt":""}`
	got := parseContainers(out)
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4", len(got))
	}
	want := []string{"running", "paused", "exited", "created"}
	for i, w := range want {
		if got[i].State != w {
			t.Fatalf("c%d state = %q, want %q", i, got[i].State, w)
		}
	}
}

func TestParseContainers_SkipsGarbageLines(t *testing.T) {
	out := "not json at all\n{\"ID\":\"ok\",\"Names\":\"good\",\"Status\":\"Up 1 second\"}\n   \n"
	got := parseContainers(out)
	if len(got) != 1 || got[0].ID != "ok" {
		t.Fatalf("want 1 valid container, got %+v", got)
	}
}

func TestDeriveState(t *testing.T) {
	cases := map[string]string{
		"Up 2 hours":            "running",
		"Up 2 hours (Paused)":   "paused",
		"Exited (0) 3 days ago": "exited",
		"Created":               "created",
		"Restarting (1) 2s ago": "restarting",
		"Dead":                  "dead",
		"some weird status":     "unknown",
	}
	for status, want := range cases {
		if got := deriveState(status); got != want {
			t.Fatalf("deriveState(%q) = %q, want %q", status, got, want)
		}
	}
}

// --- 集成测试:经真 HTTP 端点 + 假 dialer ---

// dockerLikeDialer 模拟一台装了 docker 的服务器:`docker ps` 回显两个容器(一跑一停)。
func dockerLikeDialer() cmdDialer {
	return cmdDialer{fn: func(cmd []string) (*target.ExecResult, error) {
		if len(cmd) >= 2 && cmd[0] == "docker" && cmd[1] == "ps" {
			out := `{"ID":"c0full","Names":"web","Image":"nginx:latest","State":"running","Status":"Up 2 hours","Ports":"0.0.0.0:80->80/tcp","CreatedAt":"2026-06-01 10:00:00 +0800 CST"}
{"ID":"c1full","Names":"db","Image":"mysql:8","State":"exited","Status":"Exited (0) 3 days ago","Ports":"","CreatedAt":"2026-05-28 09:00:00 +0800 CST"}
`
			return &target.ExecResult{Stdout: out, ExitCode: 0}, nil
		}
		return &target.ExecResult{Stderr: "command not found", ExitCode: 127}, nil
	}}
}

// noDockerDialer 模拟连得上但没装 docker 的服务器:`docker ps` 命令找不到(127)。
func noDockerDialer() cmdDialer {
	return cmdDialer{fn: func(cmd []string) (*target.ExecResult, error) {
		return &target.ExecResult{Stderr: "docker: command not found", ExitCode: 127}, nil
	}}
}

func TestServerContainers_DockerPresent(t *testing.T) {
	srv, client, csrf := setupServerAPI(t, dockerLikeDialer())
	credID := newSSHCredAPI(t, client, srv.URL, csrf, "pw")
	id := createServerAPI(t, client, srv.URL, csrf, credID)

	resp, _ := client.Get(srv.URL + "/api/servers/" + id + "/containers")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	var out serverContainersDTO
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, raw)
	}
	if !out.Reachable || out.Runtime != "docker" {
		t.Fatalf("want reachable+docker: %+v", out)
	}
	if out.Total != 2 || out.Running != 1 {
		t.Fatalf("counts wrong: total=%d running=%d", out.Total, out.Running)
	}
	if out.Containers[0].Names != "web" || out.Containers[0].State != "running" {
		t.Fatalf("c0 wrong: %+v", out.Containers[0])
	}
}

func TestServerContainers_NoRuntime(t *testing.T) {
	srv, client, csrf := setupServerAPI(t, noDockerDialer())
	credID := newSSHCredAPI(t, client, srv.URL, csrf, "pw")
	id := createServerAPI(t, client, srv.URL, csrf, credID)

	resp, _ := client.Get(srv.URL + "/api/servers/" + id + "/containers")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	var out serverContainersDTO
	_ = json.Unmarshal(raw, &out)
	// 连得上(命令投递成功)但无运行时:reachable true、runtime 空、error 人读、容器空。
	if !out.Reachable {
		t.Fatalf("want reachable true (connection ok): %+v", out)
	}
	if out.Runtime != "" || out.Error == "" {
		t.Fatalf("want empty runtime + human error: %+v", out)
	}
	if len(out.Containers) != 0 {
		t.Fatalf("want no containers: %+v", out.Containers)
	}
}

func TestAllServerContainers_BatchAggregate(t *testing.T) {
	srv, client, csrf := setupServerAPI(t, dockerLikeDialer())
	credID := newSSHCredAPI(t, client, srv.URL, csrf, "pw")
	_ = createServerAPI(t, client, srv.URL, csrf, credID)
	_ = createServerAPI(t, client, srv.URL, csrf, credID)

	resp, _ := client.Get(srv.URL + "/api/servers/containers")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	var body struct {
		Items []serverContainersDTO `json:"items"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, raw)
	}
	if len(body.Items) != 2 {
		t.Fatalf("want 2 servers in aggregate, got %d", len(body.Items))
	}
	for _, it := range body.Items {
		if it.Total != 2 || it.Running != 1 {
			t.Fatalf("server %s counts wrong: %+v", it.ServerID, it)
		}
	}
}

// --- 生命周期 action 白名单扩展(docker pause/unpause/kill/rm) ---

func TestValidateServiceParams_DockerLifecycleWidened(t *testing.T) {
	ok := []string{"restart", "stop", "start", "pause", "unpause", "kill", "rm"}
	for _, a := range ok {
		if err := validateServiceParams("docker", "myapp", a); err != nil {
			t.Fatalf("docker action %q should be valid: %v", a, err)
		}
	}
	// systemd 不放宽:pause/kill/rm 仍非法。
	for _, a := range []string{"pause", "unpause", "kill", "rm"} {
		if err := validateServiceParams("systemd", "nginx.service", a); err == nil {
			t.Fatalf("systemd action %q should be rejected", a)
		}
	}
	// flag 注入仍被正则挡住。
	if err := validateServiceParams("docker", "-rf", "rm"); err == nil {
		t.Fatalf("container name starting with - must be rejected")
	}
}

// buildServiceCmd 对新增 docker action 仍是 `docker <action> <name>`。
func TestBuildServiceCmd_DockerNewActions(t *testing.T) {
	for _, a := range []string{"pause", "unpause", "kill", "rm"} {
		got := buildServiceCmd("docker", a, "myapp")
		want := []string{"docker", a, "myapp"}
		if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Fatalf("buildServiceCmd docker %s = %v, want %v", a, got, want)
		}
	}
}

// --- 采集缓存(TTL + single-flight + 写失效)---

// TestContainersCache_TTLInvalidateAndLocErr:冻结时钟驱动 TTL 过期;验证回放、过期重采、
// 主动失效、定位类错误不缓存。
func TestContainersCache_TTLInvalidateAndLocErr(t *testing.T) {
	c := newContainersCache(30 * time.Second)
	now := time.Now()
	c.now = func() time.Time { return now }

	calls := 0
	collect := func(context.Context) (serverContainersDTO, error) {
		calls++
		return serverContainersDTO{ServerID: "s1", Runtime: "docker", Containers: []containerDTO{}}, nil
	}

	if _, err := c.get(context.Background(), "s1", collect); err != nil {
		t.Fatalf("first get: %v", err)
	}
	if _, err := c.get(context.Background(), "s1", collect); err != nil {
		t.Fatalf("second get: %v", err)
	}
	if calls != 1 {
		t.Fatalf("collect calls = %d, want 1(TTL 内应回放缓存)", calls)
	}

	now = now.Add(31 * time.Second) // 推进过 TTL
	if _, err := c.get(context.Background(), "s1", collect); err != nil {
		t.Fatalf("get after TTL: %v", err)
	}
	if calls != 2 {
		t.Fatalf("collect calls after TTL = %d, want 2", calls)
	}

	c.invalidate("s1") // 主动失效 → 不等 TTL 立即重采
	if _, err := c.get(context.Background(), "s1", collect); err != nil {
		t.Fatalf("get after invalidate: %v", err)
	}
	if calls != 3 {
		t.Fatalf("collect calls after invalidate = %d, want 3", calls)
	}

	// 定位类错误(locErr)透传且不缓存:下次调用仍要真采集。
	locErr := errors.New("locate boom")
	calls = 0
	failing := func(context.Context) (serverContainersDTO, error) {
		calls++
		return serverContainersDTO{ServerID: "s2"}, locErr
	}
	if _, err := c.get(context.Background(), "s2", failing); !errors.Is(err, locErr) {
		t.Fatalf("want locErr passthrough, got %v", err)
	}
	if _, err := c.get(context.Background(), "s2", failing); !errors.Is(err, locErr) {
		t.Fatalf("want locErr passthrough again, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("collect calls = %d, want 2(locErr 不应缓存)", calls)
	}
}

// TestContainersCache_SingleFlight:并发未命中合并为一次采集,等待方共享同一结果。
func TestContainersCache_SingleFlight(t *testing.T) {
	c := newContainersCache(time.Minute)
	var calls int32
	collect := func(context.Context) (serverContainersDTO, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(100 * time.Millisecond) // 留窗口让并发调用都落到 inflight 上等待
		return serverContainersDTO{ServerID: "s1", Containers: []containerDTO{{ID: "c0"}}}, nil
	}

	const n = 8
	var wg sync.WaitGroup
	results := make([]serverContainersDTO, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = c.get(context.Background(), "s1", collect)
		}(i)
	}
	wg.Wait()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("collect calls = %d, want 1(并发应合并为一次采集)", got)
	}
	for i, r := range results {
		if len(r.Containers) != 1 || r.Containers[0].ID != "c0" {
			t.Fatalf("waiter %d got wrong shared result: %+v", i, r)
		}
	}
}

// countingDockerDialer 在 dockerLikeDialer 基础上计数 `docker ps` 次数,并把其余 docker
// 子命令(restart/stop/...)当作成功 —— 供缓存命中/失效的 HTTP 级测试观察真采集次数。
func countingDockerDialer(psCalls *int32) cmdDialer {
	base := dockerLikeDialer()
	return cmdDialer{fn: func(cmd []string) (*target.ExecResult, error) {
		if len(cmd) >= 2 && cmd[0] == "docker" && cmd[1] == "ps" {
			atomic.AddInt32(psCalls, 1)
			return base.fn(cmd)
		}
		if len(cmd) >= 1 && cmd[0] == "docker" {
			return &target.ExecResult{ExitCode: 0}, nil
		}
		return &target.ExecResult{Stderr: "command not found", ExitCode: 127}, nil
	}}
}

// TestAllServerContainers_CacheReplayAndInvalidate(HTTP 级):轮询在 TTL 内回放缓存
// (SSH ps 只跑一次);docker 生命周期操作成功投递后失效;?refresh=1 强制重采。
func TestAllServerContainers_CacheReplayAndInvalidate(t *testing.T) {
	var ps int32
	srv, client, csrf := setupServerAPI(t, countingDockerDialer(&ps))
	credID := newSSHCredAPI(t, client, srv.URL, csrf, "pw")
	id := createServerAPI(t, client, srv.URL, csrf, credID)

	getAll := func(url string) []serverContainersDTO {
		t.Helper()
		resp, err := client.Get(srv.URL + url)
		if err != nil {
			t.Fatalf("get %s: %v", url, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("get %s status = %d: %s", url, resp.StatusCode, raw)
		}
		var body struct {
			Items []serverContainersDTO `json:"items"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("unmarshal: %v: %s", err, raw)
		}
		return body.Items
	}

	// 首次未命中 → 真采集;TTL 内再读 → 回放缓存(ps 次数不变,CollectedAt 原样)。
	first := getAll("/api/servers/containers")
	if atomic.LoadInt32(&ps) != 1 {
		t.Fatalf("ps calls = %d, want 1 after first get", ps)
	}
	replay := getAll("/api/servers/containers")
	if atomic.LoadInt32(&ps) != 1 {
		t.Fatalf("ps calls = %d, want still 1(TTL 内应回放缓存)", ps)
	}
	if len(first) != 1 || len(replay) != 1 || replay[0].CollectedAt != first[0].CollectedAt {
		t.Fatalf("replay should return cached copy: first=%+v replay=%+v", first, replay)
	}

	// docker 生命周期写操作成功投递 → 缓存失效,随后的读取重采。
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/servers/"+id+"/service/action", csrf,
		`{"type":"docker","target":"web","action":"restart"}`)
	io.ReadAll(resp.Body)
	resp.Body.Close()
	afterAction := getAll("/api/servers/containers")
	if atomic.LoadInt32(&ps) != 2 {
		t.Fatalf("ps calls = %d, want 2 after lifecycle action invalidated cache", ps)
	}
	if len(afterAction) != 1 || afterAction[0].Total != 2 {
		t.Fatalf("post-action read should be a fresh collection: %+v", afterAction)
	}

	// ?refresh=1 强制重采(操作员逃生口)。
	getAll("/api/servers/containers?refresh=1")
	if atomic.LoadInt32(&ps) != 3 {
		t.Fatalf("ps calls = %d, want 3 after refresh=1", ps)
	}
}
