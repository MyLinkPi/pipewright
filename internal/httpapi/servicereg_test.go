package httpapi

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/appstore"
	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/servicereg"
	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/target"
)

// fakeSRSealer 是明文透传的假保险库。
type fakeSRSealer struct{}

func (fakeSRSealer) SealSecret(p []byte) ([]byte, error) { return p, nil }
func (fakeSRSealer) OpenSecret(s []byte) ([]byte, error) { return s, nil }

// setupServiceRegServer 构造挂了服务注册 + 应用商店的测试 server(admin/testpass)。
func setupServiceRegServer(t *testing.T) (*httptest.Server, *store.Store, *fakeSRTarget) {
	t.Helper()
	st := testStoreAuth(t)
	ft := &fakeSRTarget{}
	srSvc := servicereg.New(st.DB, ft, fakeSRSealer{})
	appSvc := appstore.New(st.DB)
	if err := appSvc.EnsureBuiltins(t.Context()); err != nil {
		t.Fatalf("seed builtins: %v", err)
	}
	authSvc := auth.NewService(st.DB, nil)
	if err := authSvc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	srv := httptest.NewServer(New(testWebFSAuth(), authSvc,
		WithServiceReg(srSvc), WithAppStore(appSvc), WithServers(ft)))
	t.Cleanup(srv.Close)
	return srv, st, ft
}

// fakeSRTarget 是实现完整 target.Service 的假目标机(捕获 Exec/Upload;Get 返回占位主机)。
type fakeSRTarget struct {
	execCalls [][]string
	uploads   []string
}

// DockerLogin 满足 target.Service 接口(部署私有仓库登录追加);测试桩不触网,直接成功。
func (f *fakeSRTarget) DockerLogin(context.Context, string, string, string) error { return nil }

func (f *fakeSRTarget) Get(_ context.Context, id string) (*target.Server, error) {
	return &target.Server{ID: id, Name: "srv-" + id}, nil
}
func (f *fakeSRTarget) Exec(ctx context.Context, _ string, cmd []string) (*target.ExecResult, error) {
	f.execCalls = append(f.execCalls, cmd)
	// compose CLI 探测:v2 版本命令返回 0,其余默认 0。
	return &target.ExecResult{ExitCode: 0}, nil
}

// ExecWithStdin 满足 target.Service 接口(sudo -S 追加);servicereg 不用 stdin,转发 Exec 语义。
func (f *fakeSRTarget) ExecWithStdin(ctx context.Context, _ string, cmd []string, stdin io.Reader) (*target.ExecResult, error) {
	_, _ = io.Copy(io.Discard, stdin)
	return f.Exec(ctx, "", cmd)
}
func (f *fakeSRTarget) Upload(ctx context.Context, _ string, content io.Reader, remotePath string) error {
	f.uploads = append(f.uploads, remotePath)
	_, _ = io.Copy(io.Discard, content)
	return nil
}
func (f *fakeSRTarget) List(context.Context) ([]*target.Server, error) { return nil, nil }
func (f *fakeSRTarget) Create(context.Context, target.CreateInput) (*target.Server, error) {
	return nil, nil
}
func (f *fakeSRTarget) Update(context.Context, string, target.UpdateInput) (*target.Server, error) {
	return nil, nil
}
func (f *fakeSRTarget) Delete(context.Context, string) error                     { return nil }
func (f *fakeSRTarget) Test(context.Context, string) (*target.TestResult, error) { return nil, nil }
func (f *fakeSRTarget) ExecStream(context.Context, string, []string) (io.ReadCloser, error) {
	return nil, nil
}
func (f *fakeSRTarget) ExecInteractive(context.Context, string, []string) (target.Session, error) {
	return nil, nil
}

// loginSR 登录拿 CSRF(复用 loginWithClient:从 Set-Cookie 提取)。
func loginSR(t *testing.T, srvURL string) (*http.Client, string) {
	t.Helper()
	client := newTestClient(t)
	return client, loginWithClient(t, client, srvURL)
}

func TestServiceRegDomainAndServiceFlow(t *testing.T) {
	srv, _, ft := setupServiceRegServer(t)
	client, csrf := loginSR(t, srv.URL)

	// 注册基域(未配网关 → 纯配置态,无 SSH 命令)。
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/servicereg/domains", csrf,
		`{"baseDomain":"EFG.COM"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("注册基域:%d", resp.StatusCode)
	}
	var dom struct {
		ID         string `json:"id"`
		BaseDomain string `json:"baseDomain"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&dom)
	resp.Body.Close()
	if dom.BaseDomain != "efg.com" {
		t.Fatalf("基域应小写归一:%q", dom.BaseDomain)
	}
	if len(ft.execCalls) != 0 {
		t.Fatalf("未配网关不应编排:%v", ft.execCalls)
	}

	// 注册服务。
	body := `{"domainId":"` + dom.ID + `","name":"abc","protocol":"http","upstreamKind":"container","upstream":"web","upstreamPort":8080}`
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/servicereg/services", csrf, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("注册服务:%d", resp.StatusCode)
	}
	resp.Body.Close()

	// 列表应含 FQDN。
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/api/servicereg/services", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("列表:%d", resp.StatusCode)
	}
	var list struct {
		Items []struct {
			FQDN string `json:"fqdn"`
		} `json:"items"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Items) != 1 || list.Items[0].FQDN != "abc.efg.com" {
		t.Fatalf("服务列表不符:%+v", list.Items)
	}

	// 重复基域 → 409。
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/servicereg/domains", csrf, `{"baseDomain":"efg.com"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("重复基域应 409:%d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestServiceRegCertPEMNeverLeaked:基域列表响应绝不含 PEM 内容(证书经进程内 UploadCert 种入,
// 模拟证书管理页同步后的形态)。
func TestServiceRegCertPEMNeverLeaked(t *testing.T) {
	srv, st, _ := setupServiceRegServer(t)
	client, csrf := loginSR(t, srv.URL)

	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/servicereg/domains", csrf, `{"baseDomain":"efg.com"}`)
	var dom struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&dom)
	resp.Body.Close()

	// 进程内种入证书(证书管理 CertSink 同链路;HTTP 上传端点已随脚本上传体系下线)。
	srSvc := servicereg.New(st.DB, &fakeSRTarget{}, fakeSRSealer{})
	certPEM, keyPEM := srSelfSignedCert(t, "*.efg.com")
	if _, err := srSvc.UploadCert(t.Context(), dom.ID, certPEM, keyPEM); err != nil {
		t.Fatalf("UploadCert: %v", err)
	}

	resp = doJSON(t, client, http.MethodGet, srv.URL+"/api/servicereg/domains", "", "")
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	resp.Body.Close()
	if strings.Contains(buf.String(), "BEGIN") {
		t.Fatalf("基域列表泄漏 PEM: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"hasCert":true`) {
		t.Fatalf("列表应反映证书就绪: %s", buf.String())
	}
}

// TestAppStoreListAndDeploy:内置模板列表 + 部署链路(自动生成 secret 一次性返回)。
func TestAppStoreListAndDeploy(t *testing.T) {
	srv, _, ft := setupServiceRegServer(t)
	client, csrf := loginSR(t, srv.URL)

	resp := doJSON(t, client, http.MethodGet, srv.URL+"/api/ops/apps", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("模板列表:%d", resp.StatusCode)
	}
	var list struct {
		Items []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Builtin bool   `json:"builtin"`
			Params  []struct {
				Name string `json:"name"`
			} `json:"params"`
		} `json:"items"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Items) < 6 {
		t.Fatalf("应至少 6 个内置模板:%d", len(list.Items))
	}
	var mysqlID string
	for _, it := range list.Items {
		if it.Name == "mysql" {
			mysqlID = it.ID
		}
	}
	if mysqlID == "" {
		t.Fatal("缺 mysql 模板")
	}

	// 部署(端口自定义,密码自动生成)。
	body := `{"templateId":"` + mysqlID + `","params":{"port":"13306"}}`
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/servers/srv1/apps/deploy", csrf, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("部署:%d", resp.StatusCode)
	}
	var dep struct {
		OK     bool              `json:"ok"`
		Name   string            `json:"name"`
		Error  string            `json:"error"`
		Params map[string]string `json:"params"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&dep)
	resp.Body.Close()
	if !dep.OK || dep.Error != "" {
		t.Fatalf("部署失败:%+v", dep)
	}
	if dep.Params["root_password"] == "" || dep.Params["port"] != "13306" {
		t.Fatalf("参数回显不符:%+v", dep.Params)
	}
	// 部署命令:mkdir → Upload compose → compose up -d(项目名 = 模板名)。
	joined := srJoinCmds(ft)
	for _, want := range []string{
		"mkdir -p /opt/pipewright/stacks/mysql",
		"docker compose -p mysql -f /opt/pipewright/stacks/mysql/docker-compose.yml up -d",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺命令 %q:\n%s", want, joined)
		}
	}
	// 内置模板删除 → 拒绝。
	resp = doJSON(t, client, http.MethodDelete, srv.URL+"/api/ops/apps/"+mysqlID, csrf, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("删内置模板应 400:%d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestServiceRegInstanceFlow:实例 CRUD + 摘挂(未配网关 → 配置态,无 SSH 命令)。
func TestServiceRegInstanceFlow(t *testing.T) {
	srv, _, ft := setupServiceRegServer(t)
	client, csrf := loginSR(t, srv.URL)

	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/servicereg/domains", csrf, `{"baseDomain":"efg.com"}`)
	var dom struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&dom)
	resp.Body.Close()
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/servicereg/services", csrf,
		`{"domainId":"`+dom.ID+`","name":"abc","protocol":"http","upstreamKind":"container","upstream":"web","upstreamPort":8080}`)
	var created struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	// 列表:自动实例 #1。
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/api/servicereg/services/"+created.ID+"/instances", "", "")
	var list struct {
		Items []struct {
			ID        string `json:"id"`
			Container string `json:"container"`
			Attached  bool   `json:"attached"`
		} `json:"items"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Items) != 1 || list.Items[0].Container != "web" {
		t.Fatalf("应自动建实例 #1:%+v", list.Items)
	}

	// 加实例 + 摘挂 + 删除。
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/servicereg/services/"+created.ID+"/instances", csrf,
		`{"container":"web-2","port":9090}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("加实例:%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/servicereg/services/"+created.ID+"/instances", csrf,
		`{"container":"web-2","port":0}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("重名实例应 409:%d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doJSON(t, client, http.MethodGet, srv.URL+"/api/servicereg/services/"+created.ID+"/instances", "", "")
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	var inst2 string
	for _, it := range list.Items {
		if it.Container == "web-2" {
			inst2 = it.ID
		}
	}
	if inst2 == "" {
		t.Fatalf("实例未落库:%+v", list.Items)
	}
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/servicereg/instances/"+inst2+"/attached", csrf, `{"attached":false}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("摘除:%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doJSON(t, client, http.MethodDelete, srv.URL+"/api/servicereg/instances/"+inst2, csrf, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("删除:%d", resp.StatusCode)
	}
	resp.Body.Close()

	// 未配网关:全程无编排命令。
	if len(ft.execCalls) != 0 {
		t.Fatalf("未配网关不应编排:%v", ft.execCalls)
	}
}

// --- 工具 -----------------------------------------------------------------

func srJoinCmds(ft *fakeSRTarget) string {
	var b strings.Builder
	for _, c := range ft.execCalls {
		b.WriteString(strings.Join(c, " ") + "\n")
	}
	return b.String()
}

func srSelfSignedCert(t *testing.T, cn string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     []string{cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}
