package httpapi

// 应用商店 RustFS 部署 → 对象存储自动登记的联动契约测试。
// 直接构造 makeDeployAppHandler(fakes):不触真 SSH / DB / vault,只验证联动语义 ——
// rustfs 模板部署成功才触发 Register、入参来自生效参数与服务器地址、登记失败不影响部署结果。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/appstore"
	"github.com/huangchengsir/pipewright/internal/objectstore"
	"github.com/huangchengsir/pipewright/internal/target"
)

// deployAppFake:仅实现部署链路用到的 Get/Render(嵌入接口其余方法零成本)。
type deployAppFake struct {
	appstore.Service
	tpl      *appstore.Template
	rendered *appstore.RenderResult
}

func (f *deployAppFake) Get(ctx context.Context, id string) (*appstore.Template, error) {
	return f.tpl, nil
}

func (f *deployAppFake) Render(ctx context.Context, id string, params map[string]string) (*appstore.RenderResult, error) {
	return f.rendered, nil
}

// deployTargetFake:Exec/Upload 全成功(execFail=true 时模拟远端失败);Get 返回固定服务器。
// execCmds/uploads 记录调用参数供断言(栈名/路径是否带实例后缀)。
type deployTargetFake struct {
	target.Service
	srv      *target.Server
	execFail bool
	execCmds [][]string
	uploads  []string
}

func (f *deployTargetFake) Get(ctx context.Context, id string) (*target.Server, error) {
	return f.srv, nil
}

func (f *deployTargetFake) Exec(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	f.execCmds = append(f.execCmds, append([]string{}, cmd...))
	if f.execFail {
		return &target.ExecResult{ExitCode: 1, Stderr: "boom"}, nil
	}
	return &target.ExecResult{ExitCode: 0, Stdout: "ok"}, nil
}

func (f *deployTargetFake) Upload(ctx context.Context, serverID string, content io.Reader, remotePath string) error {
	f.uploads = append(f.uploads, remotePath)
	return nil
}

// deployObjStoreFake:记录 Register 入参,回放预设结果。
type deployObjStoreFake struct {
	objectstore.Service
	calls []objectstore.RegisterInput
	res   *objectstore.RegisterResult
	err   error
}

func (f *deployObjStoreFake) Register(ctx context.Context, in objectstore.RegisterInput) (*objectstore.RegisterResult, error) {
	f.calls = append(f.calls, in)
	return f.res, f.err
}

type appDeployResponse struct {
	ServerID              string            `json:"serverId"`
	Name                  string            `json:"name"`
	OK                    bool              `json:"ok"`
	Error                 string            `json:"error"`
	Params                map[string]string `json:"params"`
	ObjectStoreRegistered bool              `json:"objectStoreRegistered"`
	ObjectStoreNote       string            `json:"objectStoreNote"`
}

// runDeployRaw 以给定请求体打部署端点,返回状态码与响应体(供预期非 200 的用例)。
func runDeployRaw(t *testing.T, apps appstore.Service, servers target.Service, objStores objectstore.Service, body string) (int, string) {
	t.Helper()
	r := chi.NewRouter()
	r.Post("/servers/{id}/apps/deploy", makeDeployAppHandler(apps, servers, nil, objStores, nil, nil))
	req := httptest.NewRequest(http.MethodPost, "/servers/srv-1/apps/deploy", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// runDeployBody 以给定请求体部署并解码 200 响应。
func runDeployBody(t *testing.T, apps appstore.Service, servers target.Service, objStores objectstore.Service, body string) appDeployResponse {
	t.Helper()
	code, raw := runDeployRaw(t, apps, servers, objStores, body)
	if code != http.StatusOK {
		t.Fatalf("状态码 %d: %s", code, raw)
	}
	var out appDeployResponse
	if err := json.NewDecoder(strings.NewReader(raw)).Decode(&out); err != nil {
		t.Fatalf("解码响应: %v", err)
	}
	return out
}

func runDeploy(t *testing.T, apps appstore.Service, servers target.Service, objStores objectstore.Service) appDeployResponse {
	t.Helper()
	return runDeployBody(t, apps, servers, objStores, `{"templateId":"tpl-1","params":{}}`)
}

func rustfsDeployFakes() (*deployAppFake, *deployTargetFake) {
	apps := &deployAppFake{
		tpl: &appstore.Template{ID: "tpl-1", Name: "rustfs", DisplayName: "RustFS"},
		rendered: &appstore.RenderResult{Compose: "services: {}\n", Used: map[string]string{
			"access_key": "rustfsadmin",
			"secret_key": "s3cr3t-value",
			"api_port":   "9000",
			"bucket":     "pipewright",
		}},
	}
	servers := &deployTargetFake{srv: &target.Server{ID: "srv-1", Name: "build-1", Host: "192.168.1.10"}}
	return apps, servers
}

// TestDeployRustFSRegistersObjectStore:rustfs 模板部署成功 → 触发 Register,
// endpoint = 服务器 Host + api_port,凭据/桶名取自生效参数。
func TestDeployRustFSRegistersObjectStore(t *testing.T) {
	apps, servers := rustfsDeployFakes()
	objStores := &deployObjStoreFake{res: &objectstore.RegisterResult{
		Provider: &objectstore.Provider{ID: "os-1"},
	}}

	out := runDeploy(t, apps, servers, objStores)
	if !out.OK {
		t.Fatalf("部署应成功: %+v", out)
	}
	if !out.ObjectStoreRegistered || out.ObjectStoreNote == "" {
		t.Fatalf("应报告已自动登记: %+v", out)
	}
	if len(objStores.calls) != 1 {
		t.Fatalf("Register 应恰好一次, got %d", len(objStores.calls))
	}
	in := objStores.calls[0]
	if in.Type != objectstore.TypeRustFS || in.Endpoint != "192.168.1.10:9000" ||
		in.Bucket != "pipewright" || in.AccessKeyID != "rustfsadmin" ||
		in.Secret != "s3cr3t-value" || in.Name != "RustFS · build-1" {
		t.Fatalf("Register 入参不符: %+v", in)
	}
}

// TestDeployRustFSRegisterFailureBestEffort:登记失败不影响部署结果(尽力而为)。
func TestDeployRustFSRegisterFailureBestEffort(t *testing.T) {
	apps, servers := rustfsDeployFakes()
	objStores := &deployObjStoreFake{err: errors.New("vault down")}

	out := runDeploy(t, apps, servers, objStores)
	if !out.OK {
		t.Fatalf("部署不应受登记失败影响: %+v", out)
	}
	if out.ObjectStoreRegistered {
		t.Fatalf("登记失败不应报告 registered")
	}
	if !strings.Contains(out.ObjectStoreNote, "自动登记对象存储失败") {
		t.Fatalf("备注应说明登记失败: %q", out.ObjectStoreNote)
	}
}

// TestDeployOtherTemplateSkipsRegister:非 rustfs 模板不触发联动。
func TestDeployOtherTemplateSkipsRegister(t *testing.T) {
	apps, servers := rustfsDeployFakes()
	apps.tpl.Name = "mysql"
	apps.rendered.Used = map[string]string{"root_password": "x", "port": "3306"}
	objStores := &deployObjStoreFake{}

	out := runDeploy(t, apps, servers, objStores)
	if !out.OK || len(objStores.calls) != 0 {
		t.Fatalf("mysql 模板不应触发登记: %+v, calls=%d", out, len(objStores.calls))
	}
	if out.ObjectStoreRegistered || out.ObjectStoreNote != "" {
		t.Fatalf("响应不应携带登记字段: %+v", out)
	}
}

// TestDeployFailureSkipsRegister:部署失败不登记。
func TestDeployFailureSkipsRegister(t *testing.T) {
	apps, servers := rustfsDeployFakes()
	servers.execFail = true
	objStores := &deployObjStoreFake{}

	out := runDeploy(t, apps, servers, objStores)
	if out.OK || len(objStores.calls) != 0 {
		t.Fatalf("部署失败不应触发登记: %+v, calls=%d", out, len(objStores.calls))
	}
}

// TestDeployRustFSWithoutObjectStoreService:对象存储服务未注入(裁剪装配)→ 静默跳过。
func TestDeployRustFSWithoutObjectStoreService(t *testing.T) {
	apps, servers := rustfsDeployFakes()

	out := runDeploy(t, apps, servers, nil)
	if !out.OK {
		t.Fatalf("部署应成功: %+v", out)
	}
	if out.ObjectStoreRegistered || out.ObjectStoreNote != "" {
		t.Fatalf("未注入服务应静默跳过登记: %+v", out)
	}
}
