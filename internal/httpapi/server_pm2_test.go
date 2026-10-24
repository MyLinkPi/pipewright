package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/target"
)

// pm2JListFixture 是一台「装了 pm2」的服务器的 jlist 输出:cluster 模式同名双实例
// (在线)+ 一个 stopped + 一个 errored(instances 为 "max"),前面混一行 pm2 告警输出。
const pm2JListFixture = `(node:1234) ExperimentalWarning: ... someline
[{"name":"api","pid":111,"pm_id":0,"monit":{"memory":104857600,"cpu":1.5},"pm2_env":{"status":"online","pm_uptime":1730000000000,"restart_time":3,"created_at":1729000000000,"exec_mode":"cluster_mode","instances":2,"script":"server.js","pm_cwd":"/srv/api","interpreter":"node","error_file":"/home/u/.pm2/logs/api-error.log","out_file":"/home/u/.pm2/logs/api-out.log"}},
{"name":"api","pid":112,"pm_id":1,"monit":{"memory":100663296,"cpu":2},"pm2_env":{"status":"online","pm_uptime":1730000000000,"restart_time":3,"created_at":1729000000000,"exec_mode":"cluster_mode","instances":2,"script":"server.js","pm_cwd":"/srv/api","interpreter":"node","error_file":"/home/u/.pm2/logs/api-error.log","out_file":"/home/u/.pm2/logs/api-out.log"}},
{"name":"worker","pid":0,"pm_id":2,"monit":{"memory":0,"cpu":0},"pm2_env":{"status":"stopped","pm_uptime":0,"restart_time":1,"created_at":1728000000000,"exec_mode":"fork_mode","instances":1,"script":"worker.js","pm_cwd":"/srv/worker","interpreter":"node","error_file":"/home/u/.pm2/logs/worker-error.log","out_file":"/home/u/.pm2/logs/worker-out.log"}},
{"name":"cron","pid":0,"pm_id":3,"monit":{"memory":0,"cpu":0},"pm2_env":{"status":"errored","pm_uptime":1729500000000,"restart_time":15,"created_at":1728500000000,"exec_mode":"fork_mode","instances":"max","script":"cron.js","pm_cwd":"/srv/cron","interpreter":"node","error_file":"/home/u/.pm2/logs/cron-error.log","out_file":"/home/u/.pm2/logs/cron-out.log"}}]`

// --- 纯解析单元测试 ---

func TestParsePM2JList_MixedProcesses(t *testing.T) {
	got := parsePM2JList(pm2JListFixture)
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4: %+v", len(got), got)
	}
	first := got[0]
	if first.ID != 0 || first.Name != "api" || first.Status != "online" || first.PID != 111 {
		t.Fatalf("p0 wrong: %+v", first)
	}
	if first.CPU != 1.5 || first.Memory != 104857600 {
		t.Fatalf("p0 monit wrong: %+v", first)
	}
	if first.ExecMode != "cluster_mode" || first.Instances != "2" {
		t.Fatalf("p0 exec/instances wrong: %+v", first)
	}
	if first.Restarts != 3 || first.Uptime != 1730000000000 || first.CreatedAt != 1729000000000 {
		t.Fatalf("p0 counters wrong: %+v", first)
	}
	if first.Cwd != "/srv/api" || first.OutLog != "/home/u/.pm2/logs/api-out.log" || first.ErrLog != "/home/u/.pm2/logs/api-error.log" {
		t.Fatalf("p0 paths wrong: %+v", first)
	}
	// cluster 同名第二实例(pm_id 不同)。
	if got[1].ID != 1 || got[1].Name != "api" {
		t.Fatalf("p1 wrong: %+v", got[1])
	}
	// stopped / errored 与 instances "max" 透传。
	if got[2].Status != "stopped" || got[3].Status != "errored" {
		t.Fatalf("p2/p3 status wrong: %+v %+v", got[2], got[3])
	}
	if got[3].Instances != "max" {
		t.Fatalf("p3 instances = %q, want max", got[3].Instances)
	}
}

func TestParsePM2JList_Garbage(t *testing.T) {
	// 无 JSON 数组 / 坏 JSON → 空列表 best-effort,不 panic。
	for _, out := range []string{"", "not json at all", "garbage [ broken", "[{\"name\":1}]extra"} {
		if got := parsePM2JList(out); len(got) != 0 {
			t.Fatalf("parsePM2JList(%q) = %+v, want empty", out, got)
		}
	}
}

func TestNormalizePM2Status(t *testing.T) {
	cases := map[string]string{
		"online":            "online",
		"ONLINE":            "online",
		"one-launch-status": "stopped",
		"errored":           "errored",
		"":                  "",
	}
	for in, want := range cases {
		if got := normalizePM2Status(in); got != want {
			t.Fatalf("normalizePM2Status(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- 集成测试:经真 HTTP 端点 + 假 dialer ---

// pm2LikeDialer 模拟一台 pm2 在 PATH 上的服务器。
func pm2LikeDialer() cmdDialer {
	return cmdDialer{fn: func(cmd []string) (*target.ExecResult, error) {
		if len(cmd) >= 2 && cmd[0] == "pm2" && cmd[1] == "jlist" {
			return &target.ExecResult{Stdout: pm2JListFixture, ExitCode: 0}, nil
		}
		if len(cmd) >= 1 && cmd[0] == "pm2" {
			return &target.ExecResult{Stdout: "[PM2] Applying action restart to all", ExitCode: 0}, nil
		}
		return &target.ExecResult{Stderr: "command not found", ExitCode: 127}, nil
	}}
}

// noPM2Dialer 模拟连得上但没装 pm2 的服务器:直接与登录 shell 回退都 127。
func noPM2Dialer() cmdDialer {
	return cmdDialer{fn: func(cmd []string) (*target.ExecResult, error) {
		return &target.ExecResult{Stderr: "pm2: command not found", ExitCode: 127}, nil
	}}
}

// nvmPM2Dialer 模拟 nvm 安装:非交互 PATH 找不到 pm2(127),登录 shell(bash -lc)可用。
func nvmPM2Dialer(lastCmd *[]string) cmdDialer {
	return cmdDialer{fn: func(cmd []string) (*target.ExecResult, error) {
		if lastCmd != nil {
			*lastCmd = cmd
		}
		if len(cmd) == 3 && cmd[0] == "bash" && cmd[1] == "-lc" {
			if cmd[2] == "pm2 jlist" {
				return &target.ExecResult{Stdout: pm2JListFixture, ExitCode: 0}, nil
			}
			return &target.ExecResult{Stdout: "[PM2] ok", ExitCode: 0}, nil
		}
		return &target.ExecResult{Stderr: "pm2: command not found", ExitCode: 127}, nil
	}}
}

func TestServerPM2_Present(t *testing.T) {
	srv, client, csrf := setupServerAPI(t, pm2LikeDialer())
	credID := newSSHCredAPI(t, client, srv.URL, csrf, "pw")
	id := createServerAPI(t, client, srv.URL, csrf, credID)

	resp, _ := client.Get(srv.URL + "/api/servers/" + id + "/pm2")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	var out serverPm2DTO
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, raw)
	}
	if !out.Reachable || out.Runtime != "pm2" {
		t.Fatalf("want reachable+pm2: %+v", out)
	}
	if out.Total != 4 || out.Online != 2 {
		t.Fatalf("counts wrong: total=%d online=%d", out.Total, out.Online)
	}
	if out.Processes[0].Name != "api" || out.Processes[0].Status != "online" {
		t.Fatalf("p0 wrong: %+v", out.Processes[0])
	}
}

func TestServerPM2_NoRuntime(t *testing.T) {
	srv, client, csrf := setupServerAPI(t, noPM2Dialer())
	credID := newSSHCredAPI(t, client, srv.URL, csrf, "pw")
	id := createServerAPI(t, client, srv.URL, csrf, credID)

	resp, _ := client.Get(srv.URL + "/api/servers/" + id + "/pm2")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	var out serverPm2DTO
	_ = json.Unmarshal(raw, &out)
	// 连得上(命令投递成功,含 bash -lc 回退)但无 pm2:reachable true、runtime 空、error 人读。
	if !out.Reachable {
		t.Fatalf("want reachable true (connection ok): %+v", out)
	}
	if out.Runtime != "" || out.Error == "" {
		t.Fatalf("want empty runtime + human error: %+v", out)
	}
	if len(out.Processes) != 0 {
		t.Fatalf("want no processes: %+v", out.Processes)
	}
}

// TestServerPM2_PathFallback:直接 pm2 127 → 登录 shell 回退采集成功(批量端点也可用)。
func TestServerPM2_PathFallback(t *testing.T) {
	var last []string
	srv, client, csrf := setupServerAPI(t, nvmPM2Dialer(&last))
	credID := newSSHCredAPI(t, client, srv.URL, csrf, "pw")
	id := createServerAPI(t, client, srv.URL, csrf, credID)

	resp, _ := client.Get(srv.URL + "/api/servers/" + id + "/pm2")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	var out serverPm2DTO
	_ = json.Unmarshal(raw, &out)
	if !out.Reachable || out.Runtime != "pm2" || out.Total != 4 {
		t.Fatalf("want fallback-collected pm2 inventory: %+v", out)
	}
	if len(last) != 3 || last[0] != "bash" || last[1] != "-lc" || last[2] != "pm2 jlist" {
		t.Fatalf("fallback cmd = %v, want bash -lc pm2 jlist", last)
	}
}

func TestAllServerPM2_BatchAggregate(t *testing.T) {
	srv, client, csrf := setupServerAPI(t, pm2LikeDialer())
	credID := newSSHCredAPI(t, client, srv.URL, csrf, "pw")
	_ = createServerAPI(t, client, srv.URL, csrf, credID)
	_ = createServerAPI(t, client, srv.URL, csrf, credID)

	resp, _ := client.Get(srv.URL + "/api/servers/pm2")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	var body struct {
		Items []serverPm2DTO `json:"items"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, raw)
	}
	if len(body.Items) != 2 {
		t.Fatalf("want 2 servers in aggregate, got %d", len(body.Items))
	}
	for _, it := range body.Items {
		if it.Total != 4 || it.Online != 2 {
			t.Fatalf("server %s counts wrong: %+v", it.ServerID, it)
		}
	}
}

// TestServiceAction_PM2_LifecycleAndCacheInvalidate(HTTP 级):pm2 生命周期操作经 PATH
// 回退投递成功 → ok:true;成功投递后该台 pm2 清单缓存失效,随后的读取重采。
func TestServiceAction_PM2_LifecycleAndCacheInvalidate(t *testing.T) {
	var jlistCalls int32
	var last []string
	d := cmdDialer{fn: func(cmd []string) (*target.ExecResult, error) {
		last = cmd
		if len(cmd) == 3 && cmd[0] == "bash" && cmd[1] == "-lc" {
			if cmd[2] == "pm2 jlist" {
				atomic.AddInt32(&jlistCalls, 1)
				return &target.ExecResult{Stdout: pm2JListFixture, ExitCode: 0}, nil
			}
			return &target.ExecResult{Stdout: "[PM2] ok", ExitCode: 0}, nil
		}
		return &target.ExecResult{Stderr: "pm2: command not found", ExitCode: 127}, nil
	}}
	srv, client, csrf := setupServerAPI(t, d)
	credID := newSSHCredAPI(t, client, srv.URL, csrf, "pw")
	id := createServerAPI(t, client, srv.URL, csrf, credID)

	getPm2 := func() serverPm2DTO {
		t.Helper()
		resp, err := client.Get(srv.URL + "/api/servers/" + id + "/pm2")
		if err != nil {
			t.Fatalf("get pm2: %v", err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("get pm2 status = %d: %s", resp.StatusCode, raw)
		}
		var out serverPm2DTO
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal: %v: %s", err, raw)
		}
		return out
	}

	if out := getPm2(); out.Total != 4 {
		t.Fatalf("first collection wrong: %+v", out)
	}
	if got := atomic.LoadInt32(&jlistCalls); got != 1 {
		t.Fatalf("jlist calls = %d, want 1", got)
	}
	// TTL 内回放缓存。
	getPm2()
	if got := atomic.LoadInt32(&jlistCalls); got != 1 {
		t.Fatalf("jlist calls = %d, want still 1 (cache replay)", got)
	}

	// pm2 生命周期操作:直接 pm2 127 → bash -lc 回退成功。
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/servers/"+id+"/service/action", csrf,
		`{"type":"pm2","target":"api","action":"restart"}`)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("action status = %d: %s", resp.StatusCode, raw)
	}
	var act serviceActionDTO
	_ = json.Unmarshal(raw, &act)
	if !act.OK {
		t.Fatalf("action should succeed via fallback: %+v", act)
	}
	if len(last) != 3 || last[0] != "bash" || last[1] != "-lc" || last[2] != "pm2 restart api" {
		t.Fatalf("fallback action cmd = %v, want bash -lc 'pm2 restart api'", last)
	}

	// 成功投递 → 缓存失效,随后的读取重采。
	getPm2()
	if got := atomic.LoadInt32(&jlistCalls); got != 2 {
		t.Fatalf("jlist calls = %d, want 2 after lifecycle action invalidated cache", got)
	}
}

// --- 采集缓存(TTL + 写失效,结构镜像 containersCache)---

func TestPM2Cache_TTLAndInvalidate(t *testing.T) {
	c := newPM2Cache(30 * time.Second)
	now := time.Now()
	c.now = func() time.Time { return now }

	calls := 0
	collect := func(context.Context) (serverPm2DTO, error) {
		calls++
		return serverPm2DTO{ServerID: "s1", Runtime: "pm2", Processes: []pm2ProcessDTO{}}, nil
	}

	if _, err := c.get(context.Background(), "s1", collect); err != nil {
		t.Fatalf("first get: %v", err)
	}
	if _, err := c.get(context.Background(), "s1", collect); err != nil {
		t.Fatalf("second get: %v", err)
	}
	if calls != 1 {
		t.Fatalf("collect calls = %d, want 1 (TTL 内应回放缓存)", calls)
	}

	now = now.Add(31 * time.Second)
	if _, err := c.get(context.Background(), "s1", collect); err != nil {
		t.Fatalf("get after TTL: %v", err)
	}
	if calls != 2 {
		t.Fatalf("collect calls after TTL = %d, want 2", calls)
	}

	c.invalidate("s1")
	if _, err := c.get(context.Background(), "s1", collect); err != nil {
		t.Fatalf("get after invalidate: %v", err)
	}
	if calls != 3 {
		t.Fatalf("collect calls after invalidate = %d, want 3", calls)
	}

	// 定位类错误透传且不缓存。
	locErr := errors.New("locate boom")
	calls = 0
	failing := func(context.Context) (serverPm2DTO, error) {
		calls++
		return serverPm2DTO{ServerID: "s2"}, locErr
	}
	if _, err := c.get(context.Background(), "s2", failing); !errors.Is(err, locErr) {
		t.Fatalf("want locErr passthrough, got %v", err)
	}
	if _, err := c.get(context.Background(), "s2", failing); !errors.Is(err, locErr) {
		t.Fatalf("want locErr passthrough again, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("collect calls = %d, want 2 (locErr 不应缓存)", calls)
	}
}
