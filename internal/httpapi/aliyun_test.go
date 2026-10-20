package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/aliyunecs"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/mask"
)

// fakeAliyunSvc 是注入用的假 aliyunecs.Service:记录调用、返回可配置结果。
type fakeAliyunSvc struct {
	accounts   []aliyunecs.Account
	createErr  error
	updateErr  error
	deleteErr  error
	verifyErr  error
	listErr    error
	importErr  error
	importRes  *aliyunecs.ImportResult
	lastImport aliyunecs.ImportInput
}

func (f *fakeAliyunSvc) ListAccounts(context.Context) ([]aliyunecs.Account, error) {
	return f.accounts, nil
}

func (f *fakeAliyunSvc) CreateAccount(_ context.Context, in aliyunecs.AccountCreateInput) (*aliyunecs.Account, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &aliyunecs.Account{
		ID: "acc-1", Name: in.Name, AccessKeyID: in.AccessKeyID, Region: in.Region,
		SecretSealed: []byte{0x1, 0x2}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}, nil
}

func (f *fakeAliyunSvc) UpdateAccount(_ context.Context, id string, in aliyunecs.AccountUpdateInput) (*aliyunecs.Account, error) {
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	name, ak, region := "旧名", "LTAI5tOld", "cn-hangzhou"
	if in.Name != nil {
		name = *in.Name
	}
	if in.AccessKeyID != nil {
		ak = *in.AccessKeyID
	}
	if in.Region != nil {
		region = *in.Region
	}
	return &aliyunecs.Account{ID: id, Name: name, AccessKeyID: ak, Region: region,
		CreatedAt: time.Now(), UpdatedAt: time.Now()}, nil
}

func (f *fakeAliyunSvc) DeleteAccount(context.Context, string) error { return f.deleteErr }

func (f *fakeAliyunSvc) VerifyAccount(context.Context, string) error { return f.verifyErr }

func (f *fakeAliyunSvc) ListInstances(context.Context, string) ([]aliyunecs.Instance, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return []aliyunecs.Instance{{
		InstanceID: "i-1", Name: "web-1", PrivateIPs: []string{"10.0.0.1"},
		PublicIPs: []string{"1.1.1.1"}, OSType: "linux", Status: "Running", RegionID: "cn-hangzhou",
	}}, nil
}

func (f *fakeAliyunSvc) Import(_ context.Context, in aliyunecs.ImportInput) (*aliyunecs.ImportResult, error) {
	f.lastImport = in
	if f.importErr != nil {
		return nil, f.importErr
	}
	if f.importRes != nil {
		return f.importRes, nil
	}
	return &aliyunecs.ImportResult{Total: 1, OK: 1, Items: []aliyunecs.ImportItemResult{{
		InstanceID: "i-1", Name: "web-1", ServerID: "srv-1", Action: "created", OK: true,
	}}}, nil
}

// Power 桩:HTTP 面不暴露开关机端点(流水线执行器直连领域服务),此处仅满足接口。
func (f *fakeAliyunSvc) Power(context.Context, aliyunecs.PowerInput) (*aliyunecs.PowerResult, error) {
	return nil, aliyunecs.ErrPowerFailed
}

// setupAliyunAPI 构造挂了阿里云 ECS 端点的测试 server(admin/testpass)。
func setupAliyunAPI(t *testing.T, fake *fakeAliyunSvc) (srv *httptest.Server, client *http.Client, csrf string, rec audit.Recorder) {
	t.Helper()
	st := testStoreAuth(t)
	authSvc := auth.NewService(st.DB, nil)
	if err := authSvc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	rec = audit.New(st.DB, mask.NewMasker(), nil)
	srv = httptest.NewServer(New(testWebFSAuth(), authSvc,
		WithAliyunEcs(fake), WithAudit(rec)))
	t.Cleanup(srv.Close)
	client = newTestClient(t)
	csrf = loginWithClient(t, client, srv.URL)
	return srv, client, csrf, rec
}

func doAliyunJSON(t *testing.T, client *http.Client, method, url, csrf, body string) (*http.Response, []byte) {
	t.Helper()
	resp := doJSON(t, client, method, url, csrf, body)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, raw
}

func TestAliyunAccountListCreateAndUpdate(t *testing.T) {
	fake := &fakeAliyunSvc{accounts: []aliyunecs.Account{{
		ID: "acc-1", Name: "主账户", AccessKeyID: "LTAI5tFake", Region: "cn-hangzhou",
		SecretSealed: []byte{0xDE, 0xAD}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}}}
	srv, client, csrf, rec := setupAliyunAPI(t, fake)

	// 列表:DTO 不含任何 secret 字段/密文。
	resp, raw := doAliyunJSON(t, client, http.MethodGet, srv.URL+"/api/aliyun/accounts", csrf, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list 应 200, got %d body=%s", resp.StatusCode, raw)
	}
	if strings.Contains(strings.ToLower(string(raw)), "secret") {
		t.Fatalf("响应泄漏 secret 字段: %s", raw)
	}
	var list struct {
		Items []struct {
			ID          string `json:"id"`
			AccessKeyID string `json:"accessKeyId"`
			Region      string `json:"region"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("list decode err=%v items=%d body=%s", err, len(list.Items), raw)
	}
	if list.Items[0].AccessKeyID != "LTAI5tFake" || list.Items[0].Region != "cn-hangzhou" {
		t.Errorf("dto = %+v", list.Items[0])
	}

	// 新建:201 + DTO + 审计。
	resp, raw = doAliyunJSON(t, client, http.MethodPost, srv.URL+"/api/aliyun/accounts", csrf,
		`{"name":"华北","accessKeyId":"LTAI5tNew","secret":"sk-new","region":"cn-beijing"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create 应 201, got %d body=%s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), `"accessKeyId":"LTAI5tNew"`) {
		t.Errorf("create dto = %s", raw)
	}
	res, err := rec.List(context.Background(), audit.ListFilter{Action: auditActionAliyunAccountCreate})
	if err != nil || len(res.Entries) != 1 {
		t.Fatalf("create 审计 = %d err=%v", len(res.Entries), err)
	}

	// 非法 Region → 400。
	fake.createErr = aliyunecs.ErrInvalidRegion
	resp, raw = doAliyunJSON(t, client, http.MethodPost, srv.URL+"/api/aliyun/accounts", csrf,
		`{"name":"坏","accessKeyId":"ak","secret":"sk","region":"CN!!"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 region 应 400, got %d body=%s", resp.StatusCode, raw)
	}

	// 更新:secret 留空(nil)不轮换;改 region 生效。
	resp, raw = doAliyunJSON(t, client, http.MethodPut, srv.URL+"/api/aliyun/accounts/acc-1", csrf,
		`{"name":"改名","region":"cn-shanghai"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update 应 200, got %d body=%s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), `"region":"cn-shanghai"`) || !strings.Contains(string(raw), `"name":"改名"`) {
		t.Errorf("update dto = %s", raw)
	}

	// 删除 → {ok:true}。
	resp, raw = doAliyunJSON(t, client, http.MethodDelete, srv.URL+"/api/aliyun/accounts/acc-1", csrf, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"ok":true`) {
		t.Fatalf("delete = %d %s", resp.StatusCode, raw)
	}
}

func TestAliyunAccountTestAndInstances(t *testing.T) {
	fake := &fakeAliyunSvc{}
	srv, client, csrf, _ := setupAliyunAPI(t, fake)

	resp, raw := doAliyunJSON(t, client, http.MethodPost, srv.URL+"/api/aliyun/accounts/acc-1/test", csrf, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"ok":true`) {
		t.Fatalf("test = %d %s", resp.StatusCode, raw)
	}

	// 测连失败 → 502 错误信封(阿里云 API 家族)。
	fake.verifyErr = aliyunecs.ErrVerifyFailed
	resp, raw = doAliyunJSON(t, client, http.MethodPost, srv.URL+"/api/aliyun/accounts/acc-1/test", csrf, "")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("test 失败应 502, got %d body=%s", resp.StatusCode, raw)
	}

	// 实例列表:camelCase + privateIps 恒为数组。
	fake.verifyErr = nil
	resp, raw = doAliyunJSON(t, client, http.MethodGet, srv.URL+"/api/aliyun/accounts/acc-1/instances", csrf, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("instances 应 200, got %d body=%s", resp.StatusCode, raw)
	}
	var list struct {
		Items []struct {
			InstanceID string   `json:"instanceId"`
			PrivateIPs []string `json:"privateIps"`
			OSType     string   `json:"osType"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("instances decode err=%v body=%s", err, raw)
	}
	if list.Items[0].InstanceID != "i-1" || len(list.Items[0].PrivateIPs) != 1 || list.Items[0].PrivateIPs[0] != "10.0.0.1" {
		t.Errorf("instance dto = %+v", list.Items[0])
	}

	// 拉取失败 → 502。
	fake.listErr = aliyunecs.ErrListFailed
	resp, _ = doAliyunJSON(t, client, http.MethodGet, srv.URL+"/api/aliyun/accounts/acc-1/instances", csrf, "")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("instances 失败应 502, got %d", resp.StatusCode)
	}
}

func TestAliyunImportHandler(t *testing.T) {
	fake := &fakeAliyunSvc{}
	srv, client, csrf, rec := setupAliyunAPI(t, fake)

	// 正常导入:200 + items + 汇总 + 审计;请求体字段透传(domain 收到)。
	body := `{"accountId":"acc-1","instanceIds":["i-1"],"ipPreference":"private","credentialId":"cred-9","sshUser":"root","sshPort":22}`
	resp, raw := doAliyunJSON(t, client, http.MethodPost, srv.URL+"/api/aliyun/import", csrf, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import 应 200, got %d body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Items []struct {
			InstanceID string `json:"instanceId"`
			Action     string `json:"action"`
			OK         bool   `json:"ok"`
		} `json:"items"`
		Total  int `json:"total"`
		OK     int `json:"ok"`
		Failed int `json:"failed"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, raw)
	}
	if out.Total != 1 || out.OK != 1 || out.Failed != 0 || out.Items[0].Action != "created" || !out.Items[0].OK {
		t.Fatalf("out = %+v", out)
	}
	if fake.lastImport.AccountID != "acc-1" || fake.lastImport.CredentialID != "cred-9" ||
		fake.lastImport.IPPreference != "private" || fake.lastImport.SSHUser != "root" {
		t.Errorf("domain 入参 = %+v", fake.lastImport)
	}
	auditRes, err := rec.List(context.Background(), audit.ListFilter{Action: auditActionAliyunImport})
	if err != nil || len(auditRes.Entries) != 1 {
		t.Fatalf("import 审计 = %d err=%v", len(auditRes.Entries), err)
	}

	// 内联凭据透传。
	resp, raw = doAliyunJSON(t, client, http.MethodPost, srv.URL+"/api/aliyun/import", csrf,
		`{"accountId":"acc-1","instanceIds":["i-1"],"inlineSecret":{"kind":"password","secret":"pw"},"sshUser":"root"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("inline import 应 200, got %d body=%s", resp.StatusCode, raw)
	}
	if fake.lastImport.InlineSecret == nil || fake.lastImport.InlineSecret.Kind != "password" || fake.lastImport.InlineSecret.Secret != "pw" {
		t.Errorf("inline 入参 = %+v", fake.lastImport.InlineSecret)
	}

	// 整批校验失败(未选实例)→ 400;逐台失败仍 200。
	fake.importErr = aliyunecs.ErrNoInstances
	resp, raw = doAliyunJSON(t, client, http.MethodPost, srv.URL+"/api/aliyun/import", csrf,
		`{"accountId":"acc-1","instanceIds":[],"sshUser":"root"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("空实例应 400, got %d body=%s", resp.StatusCode, raw)
	}
	fake.importErr = nil
	fake.importRes = &aliyunecs.ImportResult{Total: 2, OK: 1, Failed: 1, Items: []aliyunecs.ImportItemResult{
		{InstanceID: "i-1", OK: true, Action: "created", ServerID: "s1"},
		{InstanceID: "i-2", Action: "created", Err: "该实例没有公网 IP"},
	}}
	resp, raw = doAliyunJSON(t, client, http.MethodPost, srv.URL+"/api/aliyun/import", csrf,
		`{"accountId":"acc-1","instanceIds":["i-1","i-2"],"sshUser":"root"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("部分失败应 200, got %d body=%s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), `"failed":1`) || !strings.Contains(string(raw), "该实例没有公网 IP") {
		t.Errorf("partial body = %s", raw)
	}
}

func TestAliyunImportHandlerValidationMapping(t *testing.T) {
	fake := &fakeAliyunSvc{importErr: aliyunecs.ErrInvalidCredential}
	srv, client, csrf, _ := setupAliyunAPI(t, fake)
	resp, raw := doAliyunJSON(t, client, http.MethodPost, srv.URL+"/api/aliyun/import", csrf,
		`{"accountId":"acc-1","instanceIds":["i-1"],"credentialId":"c","inlineSecret":{"kind":"password","secret":"x"},"sshUser":"root"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("双认证应 400, got %d body=%s", resp.StatusCode, raw)
	}

	// 未知错误 → 500 internal(信封不外泄细节)。
	fake.importErr = errors.New("secret leak?")
	resp, raw = doAliyunJSON(t, client, http.MethodPost, srv.URL+"/api/aliyun/import", csrf,
		`{"accountId":"acc-1","instanceIds":["i-1"],"sshUser":"root"}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("未知错误应 500, got %d body=%s", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), "secret leak") {
		t.Errorf("500 信封不应回显内部错误: %s", raw)
	}
}
