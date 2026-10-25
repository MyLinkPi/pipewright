package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/target"
)

// uninstallStubTarget 按命令应答的 target.Service 桩(其余方法嵌入接口,不触即不炸):
// responses 键 = strings.Join(cmd, " ")。
type uninstallStubTarget struct {
	target.Service
	server    *target.Server
	responses map[string]uninstallStubResp
	calls     [][]string
}

type uninstallStubResp struct {
	stdout  string
	stderr  string
	exit    int
	execErr error
}

func (s *uninstallStubTarget) Get(ctx context.Context, id string) (*target.Server, error) {
	if s.server == nil || s.server.ID != id {
		return nil, target.ErrNotFound
	}
	return s.server, nil
}

func (s *uninstallStubTarget) Exec(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	s.calls = append(s.calls, cmd)
	r, ok := s.responses[strings.Join(cmd, " ")]
	if !ok {
		return &target.ExecResult{}, nil
	}
	if r.execErr != nil {
		return nil, r.execErr
	}
	return &target.ExecResult{Stdout: r.stdout, Stderr: r.stderr, ExitCode: r.exit}, nil
}

func (s *uninstallStubTarget) ran(cmd string) bool {
	for _, c := range s.calls {
		if strings.Join(c, " ") == cmd {
			return true
		}
	}
	return false
}

// callUninstall 直调 handler(chi 路由参数注入,同 waits_test 纪律),返回响应体。
func callUninstall(t *testing.T, svc *uninstallStubTarget, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/servers/s1/apps/uninstall", strings.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "s1")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	makeAppUninstallHandler(svc, nil)(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应体解析失败:%v %s", err, rec.Body.String())
	}
	return rec.Code, out
}

func newUninstallStub() *uninstallStubTarget {
	return &uninstallStubTarget{
		server: &target.Server{ID: "s1", Name: "机器A", Host: "10.0.0.1"},
		responses: map[string]uninstallStubResp{
			"docker ps -aq --filter label=com.docker.compose.project=gitea-2":       {stdout: "abc123def456"},
			"docker volume ls -q --filter label=com.docker.compose.project=gitea-2": {stdout: "gitea-2_gitea-data"},
		},
	}
}

func TestAppUninstallHappyPath(t *testing.T) {
	svc := newUninstallStub()
	code, out := callUninstall(t, svc, `{"stackName":"gitea-2","deleteVolumes":true}`)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("应 200 + ok:true,得 %d %v", code, out)
	}
	// 三步齐走:rm 容器 → rm 卷 → rm 受管目录(路径段过白名单,无穿越)。
	for _, cmd := range []string{
		"docker rm -f abc123def456",
		"docker volume rm gitea-2_gitea-data",
		"rm -rf /opt/pipewright/stacks/gitea-2",
	} {
		if !svc.ran(cmd) {
			t.Fatalf("缺命令:%q(实际 %v)", cmd, svc.calls)
		}
	}
	if msg, _ := out["output"].(string); !strings.Contains(msg, "已删除容器") || !strings.Contains(msg, "已删除数据卷") || !strings.Contains(msg, "已清理受管目录") {
		t.Fatalf("output 应含三步摘要:%v", out["output"])
	}
}

func TestAppUninstallKeepsVolumesByDefault(t *testing.T) {
	svc := newUninstallStub()
	_, out := callUninstall(t, svc, `{"stackName":"gitea-2"}`)
	if out["ok"] != true {
		t.Fatalf("应 ok:true,得 %v", out)
	}
	if svc.ran("docker volume ls -q --filter label=com.docker.compose.project=gitea-2") {
		t.Fatal("未勾选删卷不应枚举/删除数据卷")
	}
	if !svc.ran("rm -rf /opt/pipewright/stacks/gitea-2") {
		t.Fatal("无论是否删卷都应清理受管目录")
	}
}

func TestAppUninstallStopsOnFailureKeepsDir(t *testing.T) {
	svc := newUninstallStub()
	svc.responses["docker rm -f abc123def456"] = uninstallStubResp{stderr: "no such container", exit: 1}
	_, out := callUninstall(t, svc, `{"stackName":"gitea-2","deleteVolumes":true}`)
	if out["ok"] != false {
		t.Fatalf("容器删除失败应 ok:false,得 %v", out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "删除容器失败") || !strings.Contains(msg, "no such container") {
		t.Fatalf("error 应含人读原因:%v", out["error"])
	}
	// 任一步失败即停:不删卷、不清目录(实例仍列出,可重试)。
	if svc.ran("rm -rf /opt/pipewright/stacks/gitea-2") || svc.ran("docker volume rm gitea-2_gitea-data") {
		t.Fatal("失败后不应继续删卷/清目录")
	}
}

func TestAppUninstallIdempotentWhenNothingExists(t *testing.T) {
	// 无容器、无卷、目录不在:各步幂等通过,仍 ok:true。
	svc := &uninstallStubTarget{
		server:    &target.Server{ID: "s1", Name: "机器A", Host: "10.0.0.1"},
		responses: map[string]uninstallStubResp{},
	}
	_, out := callUninstall(t, svc, `{"stackName":"ghost","deleteVolumes":true}`)
	if out["ok"] != true {
		t.Fatalf("空栈卸载应幂等成功,得 %v", out)
	}
}

func TestAppUninstallRejectsBadName(t *testing.T) {
	for _, body := range []string{`{"stackName":"../etc"}`, `{"stackName":"-x"}`, `{"stackName":""}`} {
		code, out := callUninstall(t, newUninstallStub(), body)
		if code != http.StatusBadRequest || out["ok"] == true {
			t.Fatalf("非法栈名应 400(%s),得 %d %v", body, code, out)
		}
	}
}

func TestAppUninstallServerNotFound(t *testing.T) {
	svc := newUninstallStub()
	svc.server = nil
	code, _ := callUninstall(t, svc, `{"stackName":"gitea-2"}`)
	if code != http.StatusNotFound {
		t.Fatalf("服务器不存在应 404,得 %d", code)
	}
}
