package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/mask"
	"github.com/huangchengsir/pipewright/internal/objectstore"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// setupObjectStoresAPI 构造挂了对象存储端点的测试 server(admin/testpass)。
func setupObjectStoresAPI(t *testing.T) (srv *httptest.Server, client *http.Client, csrf string, v vault.Vault, rec audit.Recorder) {
	t.Helper()
	st := testStoreAuth(t)
	authSvc := auth.NewService(st.DB, nil)
	if err := authSvc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	v = vault.New(st.DB, testMasterKey())
	rec = audit.New(st.DB, mask.NewMasker(), nil)
	svc := objectstore.New(st.DB, v)

	srv = httptest.NewServer(New(testWebFSAuth(), authSvc,
		WithVault(v), WithObjectStores(svc), WithAudit(rec)))
	t.Cleanup(srv.Close)

	client = newTestClient(t)
	csrf = loginWithClient(t, client, srv.URL)
	return srv, client, csrf, v, rec
}

// createObjectStorePOST 经 POST /api/object-stores 建一条配置,返回 (id, 原始响应体, 状态码)。
func createObjectStorePOST(t *testing.T, client *http.Client, baseURL, csrf, body string) (string, []byte, int) {
	t.Helper()
	resp := doJSON(t, client, http.MethodPost, baseURL+"/api/object-stores", csrf, body)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create 应 201, got %d body=%s", resp.StatusCode, raw)
	}
	var dto struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &dto); err != nil {
		t.Fatalf("decode create: %v body=%s", err, raw)
	}
	return dto.ID, raw, resp.StatusCode
}

// --- POST:成功创建 → Secret 只写不回显,DTO 不含 credentialId/Secret;凭据入 vault ---

func TestObjectStoreCreateSecretWriteOnly(t *testing.T) {
	srv, client, csrf, v, _ := setupObjectStoresAPI(t)

	id, raw, _ := createObjectStorePOST(t, client, srv.URL, csrf,
		`{"type":"aliyun_oss","name":"prod","endpoint":"https://oss-cn-hangzhou.aliyuncs.com","bucket":"my-bucket","accessKeyId":"LTAIxxxx","secret":"super-secret-ak"}`)
	if id == "" {
		t.Fatalf("创建应返回 id")
	}
	// 响应绝不含 Secret / credentialId;AccessKeyId 非机密可回显。
	body := string(raw)
	if strings.Contains(body, "super-secret-ak") || strings.Contains(body, "credentialId") {
		t.Fatalf("响应不应含 Secret/credentialId: %s", body)
	}
	var dto struct {
		AccessKeyID          string `json:"accessKeyId"`
		Endpoint             string `json:"endpoint"`
		CredentialConfigured bool   `json:"credentialConfigured"`
	}
	if err := json.Unmarshal(raw, &dto); err != nil {
		t.Fatalf("decode: %v body=%s", err, raw)
	}
	if dto.AccessKeyID != "LTAIxxxx" || dto.Endpoint != "oss-cn-hangzhou.aliyuncs.com" || !dto.CredentialConfigured {
		t.Fatalf("DTO 不符: %+v", dto)
	}

	// 凭据已入 vault,类型为 object_store_secret,明文只在此进程内取出验证。
	creds, err := v.List()
	if err != nil || len(creds) != 1 {
		t.Fatalf("应恰有 1 条凭据: %v / %d", err, len(creds))
	}
	if creds[0].Type != vault.TypeObjectStoreSecret {
		t.Fatalf("凭据类型应为 %s, got %s", vault.TypeObjectStoreSecret, creds[0].Type)
	}
	secret, err := v.Reveal(creds[0].ID)
	if err != nil || secret != "super-secret-ak" {
		t.Fatalf("vault 中 Secret 应可解出: %v", err)
	}
}

// --- POST:空 Secret 须落库前拒绝,且不产生凭据 ---

func TestObjectStoreCreateEmptySecretRejected(t *testing.T) {
	srv, client, csrf, v, _ := setupObjectStoresAPI(t)

	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/object-stores", csrf,
		`{"type":"aliyun_oss","name":"prod","endpoint":"oss-cn-hangzhou.aliyuncs.com","bucket":"b","accessKeyId":"LTAI","secret":"  "}`)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("空 Secret 应 400, got %d body=%s", resp.StatusCode, raw)
	}
	creds, err := v.List()
	if err != nil || len(creds) != 0 {
		t.Fatalf("拒绝路径不应留凭据, got %d err=%v", len(creds), err)
	}
}

// --- PUT:改名 + 轮换 Secret(vault 原地重封,credentialId 不变) ---

func TestObjectStoreUpdateRenameAndRotate(t *testing.T) {
	srv, client, csrf, v, _ := setupObjectStoresAPI(t)
	id, _, _ := createObjectStorePOST(t, client, srv.URL, csrf,
		`{"type":"aliyun_oss","name":"prod","endpoint":"oss-cn-hangzhou.aliyuncs.com","bucket":"b","accessKeyId":"LTAI","secret":"old-secret"}`)

	resp := doJSON(t, client, http.MethodPut, srv.URL+"/api/object-stores/"+id, csrf, `{"name":"renamed"}`)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("改名应 200, got %d body=%s", resp.StatusCode, raw)
	}
	var dto struct {
		Name                 string `json:"name"`
		CredentialConfigured bool   `json:"credentialConfigured"`
	}
	if err := json.Unmarshal(raw, &dto); err != nil || dto.Name != "renamed" || !dto.CredentialConfigured {
		t.Fatalf("改名 DTO 不符: %v %+v", err, dto)
	}

	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/object-stores/"+id, csrf, `{"secret":"new-secret"}`)
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("轮换应 200, got %d body=%s", resp.StatusCode, raw)
	}
	creds, err := v.List()
	if err != nil || len(creds) != 1 {
		t.Fatalf("应恰有 1 条凭据: %v / %d", err, len(creds))
	}
	secret, err := v.Reveal(creds[0].ID)
	if err != nil || secret != "new-secret" {
		t.Fatalf("轮换后 vault 应为新 Secret: %v", err)
	}
}

// --- POST/PUT:pathPrefix 创建时归一化落库,编辑时可改可清空(空串=清空) ---

func TestObjectStorePathPrefix(t *testing.T) {
	srv, client, csrf, _, _ := setupObjectStoresAPI(t)

	// 创建带前缀(带空白与斜杠,应归一化为 ci/prod)。
	id, raw, _ := createObjectStorePOST(t, client, srv.URL, csrf,
		`{"type":"aliyun_oss","name":"prod","endpoint":"oss-cn-hangzhou.aliyuncs.com","bucket":"b","pathPrefix":" /ci/prod/ ","accessKeyId":"LTAI","secret":"s"}`)
	var dto struct {
		PathPrefix string `json:"pathPrefix"`
	}
	if err := json.Unmarshal(raw, &dto); err != nil || dto.PathPrefix != "ci/prod" {
		t.Fatalf("创建后 pathPrefix 应归一化: %v %+v", err, dto)
	}

	// 编辑改前缀。
	resp := doJSON(t, client, http.MethodPut, srv.URL+"/api/object-stores/"+id, csrf, `{"pathPrefix":"/backups/2026/"}`)
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("改前缀应 200, got %d body=%s", resp.StatusCode, raw)
	}
	dto = struct {
		PathPrefix string `json:"pathPrefix"`
	}{}
	if err := json.Unmarshal(raw, &dto); err != nil || dto.PathPrefix != "backups/2026" {
		t.Fatalf("改后 pathPrefix 应归一化: %v %+v", err, dto)
	}

	// 空串 = 清空。
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/object-stores/"+id, csrf, `{"pathPrefix":""}`)
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("清空前缀应 200, got %d body=%s", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &dto); err != nil || dto.PathPrefix != "" {
		t.Fatalf("清空后 pathPrefix 应为空: %v %+v", err, dto)
	}
}

// --- DELETE:删除配置 → 级联删除凭据 ---

func TestObjectStoreDeleteCascadesCredential(t *testing.T) {
	srv, client, csrf, v, rec := setupObjectStoresAPI(t)
	id, _, _ := createObjectStorePOST(t, client, srv.URL, csrf,
		`{"type":"aliyun_oss","name":"prod","endpoint":"oss-cn-hangzhou.aliyuncs.com","bucket":"b","accessKeyId":"LTAI","secret":"s"}`)

	resp := doJSON(t, client, http.MethodDelete, srv.URL+"/api/object-stores/"+id, csrf, "")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("删除应 200, got %d body=%s", resp.StatusCode, raw)
	}
	creds, err := v.List()
	if err != nil || len(creds) != 0 {
		t.Fatalf("删除配置应级联删凭据, got %d err=%v", len(creds), err)
	}
	// 审计留痕。
	entries, err := rec.List(context.Background(), audit.ListFilter{Action: auditActionObjectStoreDelete})
	if err != nil || len(entries.Entries) != 1 {
		t.Fatalf("应恰有 1 条 delete 审计: %v / %d", err, len(entries.Entries))
	}
}

// --- POST /{id}/test:不存在的配置 → 404(不触网) ---

func TestObjectStoreTestNotFound(t *testing.T) {
	srv, client, csrf, _, _ := setupObjectStoresAPI(t)
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/object-stores/missing/test", csrf, "")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在应 404, got %d body=%s", resp.StatusCode, raw)
	}
}

// --- GET:列表初始为空 ---

func TestObjectStoreListEmpty(t *testing.T) {
	srv, client, csrf, _, _ := setupObjectStoresAPI(t)
	resp := doJSON(t, client, http.MethodGet, srv.URL+"/api/object-stores", csrf, "")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("列表应 200, got %d body=%s", resp.StatusCode, raw)
	}
	var list struct {
		Items []any `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil || len(list.Items) != 0 {
		t.Fatalf("初始列表应为空: %v body=%s", err, raw)
	}
}

// --- GET /{id}/objects:不存在的配置 → 404(不触网;浏览语义/分页契约由 service 层单测覆盖) ---

func TestObjectStoreBrowseNotFound(t *testing.T) {
	srv, client, csrf, _, _ := setupObjectStoresAPI(t)
	resp := doJSON(t, client, http.MethodGet, srv.URL+"/api/object-stores/missing/objects?prefix=&marker=&maxKeys=100", csrf, "")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在应 404, got %d body=%s", resp.StatusCode, raw)
	}
}

// --- POST /{id}/buckets:校验先于触网 —— 非法桶名 400、同账户重复桶 409(均不触网) ---

func TestObjectStoreCreateBucketValidation(t *testing.T) {
	srv, client, csrf, _, _ := setupObjectStoresAPI(t)
	id, _, _ := createObjectStorePOST(t, client, srv.URL, csrf,
		`{"type":"aliyun_oss","name":"prod","endpoint":"oss-cn-hangzhou.aliyuncs.com","bucket":"my-bucket","accessKeyId":"LTAI","secret":"s"}`)

	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/object-stores/"+id+"/buckets", csrf, `{"bucket":"my-bucket"}`)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("重复桶应 409, got %d body=%s", resp.StatusCode, raw)
	}

	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/object-stores/"+id+"/buckets", csrf, `{"bucket":"Bad_Name"}`)
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法桶名应 400, got %d body=%s", resp.StatusCode, raw)
	}
}

// --- POST/PUT:useSSL 缺省按类型回填(OSS=true),显式传值尊重,Update 指针切换 ---

func TestObjectStoreUseSSL(t *testing.T) {
	srv, client, csrf, _, _ := setupObjectStoresAPI(t)

	// OSS 缺省 → true。
	_, raw, _ := createObjectStorePOST(t, client, srv.URL, csrf,
		`{"type":"aliyun_oss","name":"prod","endpoint":"oss-cn-hangzhou.aliyuncs.com","bucket":"b","accessKeyId":"LTAI","secret":"s"}`)
	var dto struct {
		UseSSL bool `json:"useSSL"`
	}
	if err := json.Unmarshal(raw, &dto); err != nil || !dto.UseSSL {
		t.Fatalf("OSS 缺省应 useSSL=true: %v %+v", err, dto)
	}

	// RustFS 缺省 → false;显式 true 尊重调用方。
	_, raw, _ = createObjectStorePOST(t, client, srv.URL, csrf,
		`{"type":"rustfs","name":"self","endpoint":"192.168.1.10:9000","accessKeyId":"ak","secret":"s","bucket":"pipewright"}`)
	if err := json.Unmarshal(raw, &dto); err != nil || dto.UseSSL {
		t.Fatalf("RustFS 缺省应 useSSL=false: %v %+v", err, dto)
	}
	_, raw, _ = createObjectStorePOST(t, client, srv.URL, csrf,
		`{"type":"rustfs","name":"tls","endpoint":"minio.internal:9000","useSSL":true,"accessKeyId":"ak","secret":"s","bucket":"pipewright"}`)
	if err := json.Unmarshal(raw, &dto); err != nil || !dto.UseSSL {
		t.Fatalf("显式 useSSL=true 应生效: %v %+v", err, dto)
	}

	// Update 切换开关(缺省字段不修改)。
	id, _, _ := createObjectStorePOST(t, client, srv.URL, csrf,
		`{"type":"rustfs","name":"flip","endpoint":"192.168.1.10:9000","accessKeyId":"ak","secret":"s","bucket":"pipewright"}`)
	resp := doJSON(t, client, http.MethodPut, srv.URL+"/api/object-stores/"+id, csrf, `{"useSSL":true}`)
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("切换 useSSL 应 200, got %d body=%s", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &dto); err != nil || !dto.UseSSL {
		t.Fatalf("切换后应 useSSL=true: %v %+v", err, dto)
	}
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/object-stores/"+id, csrf, `{"name":"flip2"}`)
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err := json.Unmarshal(raw, &dto); err != nil || !dto.UseSSL {
		t.Fatalf("无关 Update 不应改动 useSSL: %v %+v", err, dto)
	}
}

// --- GET /{id}/buckets:不存在的配置 → 404;不可达端点 → 502(错误码映射) ---

func TestObjectStoreListBuckets(t *testing.T) {
	srv, client, csrf, _, _ := setupObjectStoresAPI(t)
	resp := doJSON(t, client, http.MethodGet, srv.URL+"/api/object-stores/missing/buckets", csrf, "")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在应 404, got %d body=%s", resp.StatusCode, raw)
	}

	// 不可路由端点(127.0.0.1:1)→ 快速连接拒绝 → 502 + 专用错误码。
	id, _, _ := createObjectStorePOST(t, client, srv.URL, csrf,
		`{"type":"rustfs","name":"dead","endpoint":"127.0.0.1:1","accessKeyId":"ak","secret":"s","bucket":"pipewright"}`)
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/api/object-stores/"+id+"/buckets", csrf, "")
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("不可达应 502, got %d body=%s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "object_store_list_buckets_failed") {
		t.Fatalf("应为列桶专用错误码: %s", raw)
	}
}

// --- POST /buckets/probe:校验先于触网(非法类型/缺参 → 400);响应绝不回显 Secret ---

func TestObjectStoreProbeBuckets(t *testing.T) {
	srv, client, csrf, _, _ := setupObjectStoresAPI(t)

	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/object-stores/buckets/probe", csrf,
		`{"type":"minio","endpoint":"127.0.0.1:1","accessKeyId":"ak","secret":"s"}`)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法类型应 400, got %d body=%s", resp.StatusCode, raw)
	}

	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/object-stores/buckets/probe", csrf,
		`{"type":"aliyun_oss","endpoint":"127.0.0.1:1","accessKeyId":"ak","secret":"  "}`)
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("空 secret 应 400, got %d body=%s", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), "probe") && strings.Contains(string(raw), "secret") {
		t.Fatalf("错误体不应回显 Secret: %s", raw)
	}
}

