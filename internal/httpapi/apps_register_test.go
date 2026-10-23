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
// lastParams 记录最近一次 Render 入参(升级测试断言参数合并结果)。
type deployAppFake struct {
	appstore.Service
	tpl        *appstore.Template
	rendered   *appstore.RenderResult
	lastParams map[string]string
}

func (f *deployAppFake) Get(ctx context.Context, id string) (*appstore.Template, error) {
	return f.tpl, nil
}

func (f *deployAppFake) Render(ctx context.Context, id string, params map[string]string) (*appstore.RenderResult, error) {
	f.lastParams = params
	return f.rendered, nil
}

// deployTargetFake:Exec/Upload 全成功(execFail=true 时模拟远端失败);Get 返回固定服务器。
// execCmds/uploads 记录调用参数供断言(栈名/路径是否带实例后缀);
// artifacts 按路径留底上传内容,Download 据此回放(升级链路读参数边车用;无记录 → 空 = 读不到)。
type deployTargetFake struct {
	target.Service
	srv       *target.Server
	execFail  bool
	execCmds  [][]string
	uploads   []string
	artifacts map[string]string
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
	b, _ := io.ReadAll(content)
	if f.artifacts == nil {
		f.artifacts = map[string]string{}
	}
	f.artifacts[remotePath] = string(b)
	return nil
}

func (f *deployTargetFake) Download(ctx context.Context, serverID string, remotePath string, w io.Writer) (int64, error) {
	body, ok := f.artifacts[remotePath]
	if !ok {
		return 0, nil
	}
	n, _ := io.Copy(w, strings.NewReader(body))
	return n, nil
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

// 响应解码直接用生产类型 appDeployResponse(apps.go 的冻结契约,字段超集)。

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

// ---------- 实例升级(参数边车) ----------

// upgradeFakes:带参数 schema 的 rustfs 模板(secret_key 为 secret 型),供升级链路测试。
func upgradeFakes() (*deployAppFake, *deployTargetFake) {
	apps, servers := rustfsDeployFakes()
	apps.tpl.Params = []appstore.ParamSpec{
		{Name: "access_key"},
		{Name: "secret_key", Type: appstore.ParamTypeSecret},
		{Name: "api_port"},
		{Name: "bucket"},
	}
	return apps, servers
}

// upgradeRouter 挂部署 + 升级上下文 + 升级三个端点(直构造 handler,不触真 SSH/DB)。
func upgradeRouter(apps appstore.Service, servers target.Service) *chi.Mux {
	r := chi.NewRouter()
	r.Post("/servers/{id}/apps/deploy", makeDeployAppHandler(apps, servers, nil, nil, nil, nil))
	r.Get("/servers/{id}/apps/upgrade-context", makeAppUpgradeContextHandler(apps, servers))
	r.Post("/servers/{id}/apps/upgrade", makeUpgradeAppHandler(apps, servers, nil))
	return r
}

// appUpgradeContextDTO 是升级上下文端点的测试解码结构。
type appUpgradeContextDTO struct {
	Found     bool              `json:"found"`
	StackName string            `json:"stackName"`
	Params    map[string]string `json:"params"`
	Secrets   map[string]string `json:"secrets"`
}

func getUpgradeContext(t *testing.T, r *chi.Mux, query string) appUpgradeContextDTO {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/servers/srv-1/apps/upgrade-context?"+query, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("升级上下文状态码 %d: %s", rec.Code, rec.Body.String())
	}
	var out appUpgradeContextDTO
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("解码上下文: %v", err)
	}
	return out
}

func postUpgrade(t *testing.T, r *chi.Mux, body string) appDeployResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/servers/srv-1/apps/upgrade", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("升级状态码 %d: %s", rec.Code, rec.Body.String())
	}
	var out appDeployResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("解码升级响应: %v", err)
	}
	return out
}

// sidecarPath 返回栈参数边车的远端路径。
func sidecarPath(stackName string) string {
	return stacksBaseDir + "/" + stackName + "/" + appParamsFileName
}

// TestAppUpgradeContextNoSidecar:无参数边车(未部署/旧版部署)→ found:false,栈名仍回显。
func TestAppUpgradeContextNoSidecar(t *testing.T) {
	apps, servers := upgradeFakes()
	r := upgradeRouter(apps, servers)

	out := getUpgradeContext(t, r, "templateId=tpl-1")
	if out.Found || out.StackName != "rustfs" {
		t.Fatalf("无边车应 found:false 且栈名回显: %+v", out)
	}
}

// TestAppUpgradeNoSidecar:无参数边车 → 200 + ok:false + 人读指引(不假装能原位升级)。
func TestAppUpgradeNoSidecar(t *testing.T) {
	apps, servers := upgradeFakes()
	r := upgradeRouter(apps, servers)

	out := postUpgrade(t, r, `{"templateId":"tpl-1","params":{}}`)
	if out.OK || !strings.Contains(out.Error, "参数记录") {
		t.Fatalf("无边车应 ok:false + 指引: %+v", out)
	}
}

// TestAppUpgradeKeepsSecretsAndMergesOverrides:部署后原位升级 ——
// 上下文预填非密参数、secret 只回名;升级时显式值覆盖、secret 保留边车现值不重新生成;
// 成功后边车重写为最新生效参数。
func TestAppUpgradeKeepsSecretsAndMergesOverrides(t *testing.T) {
	apps, servers := upgradeFakes()
	r := upgradeRouter(apps, servers)

	// 首次部署(写参数边车)。
	dreq := httptest.NewRequest(http.MethodPost, "/servers/srv-1/apps/deploy", strings.NewReader(`{"templateId":"tpl-1","params":{}}`))
	drec := httptest.NewRecorder()
	r.ServeHTTP(drec, dreq)
	var dep appDeployResponse
	if err := json.NewDecoder(drec.Body).Decode(&dep); err != nil || !dep.OK {
		t.Fatalf("部署应成功: %d %s", drec.Code, drec.Body.String())
	}
	if _, ok := servers.artifacts[sidecarPath("rustfs")]; !ok {
		t.Fatalf("部署后应写参数边车: %v", servers.uploads)
	}

	// 升级上下文:非密参数回现值,secret 只回名不回值。
	ctx := getUpgradeContext(t, r, "templateId=tpl-1")
	if !ctx.Found || ctx.Params["api_port"] != "9000" || ctx.Params["access_key"] != "rustfsadmin" {
		t.Fatalf("上下文应预填非密参数: %+v", ctx)
	}
	if _, leaked := ctx.Params["secret_key"]; leaked {
		t.Fatalf("secret 值不得混入 params: %+v", ctx.Params)
	}
	// secret 现值随 secrets 回显(升级表单预填:不动 = 保持,改 = 覆盖;与 compose 明文同级姿态)。
	if ctx.Secrets["secret_key"] == "" {
		t.Fatalf("secret 现值应回显: %+v", ctx.Secrets)
	}

	// 升级:只改 api_port,secret_key 不动。
	out := postUpgrade(t, r, `{"templateId":"tpl-1","params":{"api_port":"9001"}}`)
	if !out.OK {
		t.Fatalf("升级应成功: %+v", out)
	}
	merged := apps.lastParams
	if merged["api_port"] != "9001" {
		t.Fatalf("显式参数应覆盖: %+v", merged)
	}
	if merged["secret_key"] != "s3cr3t-value" || merged["access_key"] != "rustfsadmin" {
		t.Fatalf("未给参数应保留边车现值(secret 不重新生成): %+v", merged)
	}
	// 成功后边车重写(deploy 一次 + upgrade 一次,共两次)。
	count := 0
	for _, u := range servers.uploads {
		if u == sidecarPath("rustfs") {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("边车应部署/升级各写一次,实际 %d: %v", count, servers.uploads)
	}
}
