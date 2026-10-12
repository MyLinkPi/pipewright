package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// fakeVault 是 vaultReader/vaultWriter 的内存实现(便于单测,不触真库)。
type fakeVault struct {
	configured bool
	secrets    map[string]string
	nextID     int
}

func (f *fakeVault) Reveal(id string) (string, error) {
	if !f.configured {
		return "", vault.ErrVaultUnconfigured
	}
	s, ok := f.secrets[id]
	if !ok {
		return "", vault.ErrNotFound
	}
	return s, nil
}

func (f *fakeVault) Exists(id string) (bool, error) {
	if !f.configured {
		return false, vault.ErrVaultUnconfigured
	}
	_, ok := f.secrets[id]
	return ok, nil
}

// Create 是 vaultWriter 写入半边:存明文、发新 ID(Register 自动登记链路用)。
func (f *fakeVault) Create(in vault.CreateInput) (*vault.Credential, error) {
	if !f.configured {
		return nil, vault.ErrVaultUnconfigured
	}
	if in.Secret == "" {
		return nil, vault.ErrEmptySecret
	}
	f.nextID++
	id := fmt.Sprintf("cred-%d", f.nextID)
	f.secrets[id] = in.Secret
	return &vault.Credential{ID: id, Name: in.Name, Type: in.Type}, nil
}

// Update 原地轮换(Secret 非 nil 即重写)。
func (f *fakeVault) Update(id string, in vault.UpdateInput) (*vault.Credential, error) {
	if !f.configured {
		return nil, vault.ErrVaultUnconfigured
	}
	if _, ok := f.secrets[id]; !ok {
		return nil, vault.ErrNotFound
	}
	if in.Secret != nil {
		f.secrets[id] = *in.Secret
	}
	return &vault.Credential{ID: id}, nil
}

func (f *fakeVault) Delete(id string) error {
	if !f.configured {
		return vault.ErrVaultUnconfigured
	}
	delete(f.secrets, id)
	return nil
}

// fakeClient 是 ObjectClient 的内存实现(记录上传内容)。
type fakeClient struct {
	verifyErr error
	putErr    error
	puts      map[string]string
}

func (c *fakeClient) VerifyBucket(ctx context.Context) error { return c.verifyErr }

func (c *fakeClient) PutObject(ctx context.Context, objectKey string, body io.Reader) error {
	if c.putErr != nil {
		return c.putErr
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if c.puts == nil {
		c.puts = map[string]string{}
	}
	c.puts[objectKey] = string(b)
	return nil
}

// newTestService 用内存 DB + fake vault + fake client 构造 Service。
// fv 为 nil 时代表 vault 未注入(Test/Upload 应返回 ErrVaultUnconfigured)。
func newTestService(t *testing.T, fv *fakeVault, client ObjectClient) Service {
	t.Helper()
	db := storetest.OpenDB(t)
	var vr vaultReader
	if fv != nil {
		vr = fv
	}
	return &service{
		store: NewStore(db),
		vault: vr,
		dial: func(providerType, endpoint, accessKeyID, accessKeySecret, bucket string) (ObjectClient, error) {
			if providerType != TypeAliyunOSS {
				return nil, ErrInvalidType
			}
			return client, nil
		},
	}
}

func sampleInput() CreateInput {
	return CreateInput{
		Type: TypeAliyunOSS, Name: "prod", Endpoint: "https://oss-cn-hangzhou.aliyuncs.com/",
		Bucket: "my-bucket", AccessKeyID: "LTAIxxxx", CredentialID: "cred-1",
	}
}

func TestCreateRequiresExistingCredential(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{}}
	svc := newTestService(t, fv, &fakeClient{})

	_, err := svc.Create(ctx, sampleInput())
	if !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("凭据不存在应返回 ErrCredentialNotFound, got %v", err)
	}
}

func TestCRUD(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{"cred-1": "secret-value"}}
	svc := newTestService(t, fv, &fakeClient{})

	p, err := svc.Create(ctx, sampleInput())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.Endpoint != "oss-cn-hangzhou.aliyuncs.com" {
		t.Fatalf("endpoint 应去 scheme: %q", p.Endpoint)
	}
	if p.CredentialID != "cred-1" || p.AccessKeyID != "LTAIxxxx" {
		t.Fatalf("字段不符: %+v", p)
	}

	got, err := svc.Get(ctx, p.ID)
	if err != nil || got.ID != p.ID {
		t.Fatalf("Get: %v, %+v", err, got)
	}
	list, err := svc.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("List: %v, len=%d", err, len(list))
	}

	name := "renamed"
	bucket := "new-bucket"
	upd, err := svc.Update(ctx, p.ID, UpdateInput{Name: &name, Bucket: &bucket})
	if err != nil || upd.Name != "renamed" || upd.Bucket != "new-bucket" {
		t.Fatalf("Update: %v, %+v", err, upd)
	}

	if err := svc.Delete(ctx, p.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后 Get 应 ErrNotFound, got %v", err)
	}
}

func TestTest(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{"cred-1": "secret-value"}}
	svc := newTestService(t, fv, &fakeClient{})
	p, err := svc.Create(ctx, sampleInput())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	res, err := svc.Test(ctx, p.ID)
	if err != nil || !res.OK {
		t.Fatalf("Test 应成功: %v, %+v", err, res)
	}
	if !strings.Contains(res.Detail, "my-bucket") {
		t.Fatalf("成功 detail 应含桶名: %q", res.Detail)
	}
}

func TestTestVerifyFailed(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{"cred-1": "secret-value"}}
	svc := newTestService(t, fv, &fakeClient{verifyErr: errors.New("AccessDenied")})
	p, _ := svc.Create(ctx, sampleInput())

	res, err := svc.Test(ctx, p.ID)
	if err != nil {
		t.Fatalf("Test 不应返回 error(失败在 result 里): %v", err)
	}
	if res.OK || !errors.Is(res.Err, ErrTestFailed) {
		t.Fatalf("校验失败应 OK=false/ErrTestFailed: %+v", res)
	}
}

func TestUpload(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{"cred-1": "secret-value"}}
	fc := &fakeClient{}
	svc := newTestService(t, fv, fc)
	p, _ := svc.Create(ctx, sampleInput())

	if err := svc.Upload(ctx, p.ID, "releases/app.jar", strings.NewReader("bytes")); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if fc.puts["releases/app.jar"] != "bytes" {
		t.Fatalf("上传内容不符: %+v", fc.puts)
	}
}

func TestUploadFailed(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{"cred-1": "secret-value"}}
	svc := newTestService(t, fv, &fakeClient{putErr: errors.New("AccessDenied")})
	p, _ := svc.Create(ctx, sampleInput())

	err := svc.Upload(ctx, p.ID, "k", strings.NewReader("x"))
	if !errors.Is(err, ErrUploadFailed) {
		t.Fatalf("上传失败应 ErrUploadFailed, got %v", err)
	}
}

func TestUploadAndTestWithoutVault(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, nil, &fakeClient{})
	// 直接构造一条配置(vault nil 时不校验凭据存在性)。
	fvInput := sampleInput()
	normalizeCreate(&fvInput)
	p := newProvider(fvInput)
	if err := svc.(*service).store.create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := svc.Test(ctx, p.ID); !errors.Is(err, ErrVaultUnconfigured) {
		t.Fatalf("vault 未配时 Test 应 ErrVaultUnconfigured, got %v", err)
	}
	if err := svc.Upload(ctx, p.ID, "k", strings.NewReader("x")); !errors.Is(err, ErrVaultUnconfigured) {
		t.Fatalf("vault 未配时 Upload 应 ErrVaultUnconfigured, got %v", err)
	}
}

// TestNormalizePathPrefix 覆盖配置级路径前缀的归一化(去空白 / 首尾斜杠;/a/b/ 与 a/b 等价)。
func TestNormalizePathPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"/", ""},
		{"releases", "releases"},
		{" /releases/ ", "releases"},
		{"releases/", "releases"},
		{"ci/prod/apps/", "ci/prod/apps"},
		{"/ci//prod/", "ci//prod"}, // 仅去首尾斜杠,内部双斜杠保留(调用方自担)
	}
	for _, c := range cases {
		if got := NormalizePathPrefix(c.in); got != c.want {
			t.Errorf("NormalizePathPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestUploadAppliesPathPrefix 配置了 pathPrefix 时,Upload 应把前缀附加到所有对象键前
// (存储级默认前缀,调用方无感知;无前缀配置时行为不变)。
func TestUploadAppliesPathPrefix(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{"cred-1": "secret-value"}}
	fc := &fakeClient{}
	svc := newTestService(t, fv, fc)

	in := sampleInput()
	in.PathPrefix = " /ci/prod/ " // 归一化为 ci/prod
	p, err := svc.Create(ctx, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.PathPrefix != "ci/prod" {
		t.Fatalf("pathPrefix 应归一化: %q", p.PathPrefix)
	}

	if err := svc.Upload(ctx, p.ID, "releases/app.jar", strings.NewReader("bytes")); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if fc.puts["ci/prod/releases/app.jar"] != "bytes" {
		t.Fatalf("对象键应带配置前缀: %+v", fc.puts)
	}

	// 无前缀配置 → 键原样(回归)。
	plain, _ := svc.Create(ctx, sampleInput())
	if err := svc.Upload(ctx, plain.ID, "k", strings.NewReader("x")); err != nil {
		t.Fatalf("Upload(无前缀): %v", err)
	}
	if fc.puts["k"] != "x" {
		t.Fatalf("无前缀时对象键应原样: %+v", fc.puts)
	}
}

// TestUpdatePathPrefix 编辑可改、可清空 pathPrefix(空串 = 清空,非 nil 即生效)。
func TestUpdatePathPrefix(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{"cred-1": "secret-value"}}
	svc := newTestService(t, fv, &fakeClient{})
	in := sampleInput()
	in.PathPrefix = "ci/prod"
	p, _ := svc.Create(ctx, in)

	changed := "/backups/2026/"
	upd, err := svc.Update(ctx, p.ID, UpdateInput{PathPrefix: &changed})
	if err != nil || upd.PathPrefix != "backups/2026" {
		t.Fatalf("改前缀: %v, %+v", err, upd)
	}

	cleared := ""
	upd, err = svc.Update(ctx, p.ID, UpdateInput{PathPrefix: &cleared})
	if err != nil || upd.PathPrefix != "" {
		t.Fatalf("清前缀(空串): %v, %+v", err, upd)
	}

	// 缺省(nil)不改动。
	upd, err = svc.Update(ctx, p.ID, UpdateInput{})
	if err != nil || upd.PathPrefix != "" {
		t.Fatalf("nil 前缀应保持: %v, %+v", err, upd)
	}
}

// ---------- Register(应用商店 RustFS 部署联动自动登记)----------

// fakeEnsurerClient 是带 EnsureBucket 能力的 fake(模拟 RustFS 客户端实现的 bucketEnsurer)。
type fakeEnsurerClient struct {
	fakeClient
	ensureErr error
	ensured   int
}

func (c *fakeEnsurerClient) EnsureBucket(ctx context.Context) error {
	c.ensured++
	return c.ensureErr
}

// newRegisterTestService 构造走 rustfs 类型 + 可控 dial 的 Service(Register 链路专用)。
func newRegisterTestService(t *testing.T, fv *fakeVault, client ObjectClient) *service {
	t.Helper()
	db := storetest.OpenDB(t)
	var vw vaultWriter
	if fv != nil {
		vw = fv
	}
	return &service{
		store:  NewStore(db),
		vaultW: vw,
		dial: func(providerType, endpoint, accessKeyID, accessKeySecret, bucket string) (ObjectClient, error) {
			if providerType != TypeRustFS {
				return nil, ErrInvalidType
			}
			return client, nil
		},
	}
}

func registerInput() RegisterInput {
	return RegisterInput{
		Type: TypeRustFS, Name: "RustFS · build-1", Endpoint: "http://192.168.1.10:9000/",
		Bucket: "pipewright", AccessKeyID: "rustfsadmin", Secret: "generated-secret",
	}
}

// TestRegisterCreates 覆盖新建链路:建桶 → 凭据入 vault → 落库;Endpoint 归一化去 scheme。
func TestRegisterCreates(t *testing.T) {
	restoreRetryDelay(t)
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{}}
	fc := &fakeEnsurerClient{}
	svc := newRegisterTestService(t, fv, fc)

	res, err := svc.Register(ctx, registerInput())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if res.Updated || res.Note != "" {
		t.Fatalf("首次登记应 Updated=false 且 Note 空: %+v", res)
	}
	if fc.ensured != 1 {
		t.Fatalf("应恰好建桶一次, got %d", fc.ensured)
	}
	p := res.Provider
	if p.Type != TypeRustFS || p.Endpoint != "192.168.1.10:9000" || p.Bucket != "pipewright" ||
		p.AccessKeyID != "rustfsadmin" || p.CredentialID == "" {
		t.Fatalf("落库字段不符: %+v", p)
	}
	if got := fv.secrets[p.CredentialID]; got != "generated-secret" {
		t.Fatalf("vault 应存入 secret: %q", got)
	}
}

// TestRegisterRotatesExisting 覆盖幂等锚点:同 type+endpoint 重复登记 → 原地轮换凭据,
// 不产生重复条目;Name 不被覆盖(尊重用户手改的展示名)。
func TestRegisterRotatesExisting(t *testing.T) {
	restoreRetryDelay(t)
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{}}
	svc := newRegisterTestService(t, fv, &fakeEnsurerClient{})

	first, err := svc.Register(ctx, registerInput())
	if err != nil {
		t.Fatalf("首次 Register: %v", err)
	}
	// 用户手改展示名(重新部署不应覆盖)。
	renamed := "生产 RustFS"
	if _, err := svc.Update(ctx, first.Provider.ID, UpdateInput{Name: &renamed}); err != nil {
		t.Fatalf("改名: %v", err)
	}

	in := registerInput()
	in.AccessKeyID = "new-ak"
	in.Secret = "rotated-secret"
	second, err := svc.Register(ctx, in)
	if err != nil {
		t.Fatalf("二次 Register: %v", err)
	}
	if !second.Updated {
		t.Fatalf("同 endpoint 重复登记应 Updated=true")
	}
	if second.Provider.ID != first.Provider.ID {
		t.Fatalf("应命中同一条配置: %q vs %q", second.Provider.ID, first.Provider.ID)
	}
	if second.Provider.Name != renamed {
		t.Fatalf("展示名不应被覆盖: %q", second.Provider.Name)
	}
	if second.Provider.AccessKeyID != "new-ak" {
		t.Fatalf("AccessKeyID 应更新: %q", second.Provider.AccessKeyID)
	}
	if got := fv.secrets[second.Provider.CredentialID]; got != "rotated-secret" {
		t.Fatalf("vault 凭据应轮换为新 secret: %q", got)
	}
	list, _ := svc.List(ctx)
	if len(list) != 1 {
		t.Fatalf("不应产生重复条目, len=%d", len(list))
	}
}

// TestRegisterBucketFailure 建桶失败是尽力而为:记入 Note 但登记照常完成。
func TestRegisterBucketFailure(t *testing.T) {
	restoreRetryDelay(t)
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{}}
	fc := &fakeEnsurerClient{ensureErr: errors.New("connection refused")}
	svc := newRegisterTestService(t, fv, fc)

	res, err := svc.Register(ctx, registerInput())
	if err != nil {
		t.Fatalf("建桶失败不应阻断登记: %v", err)
	}
	if res.Provider == nil || res.Updated {
		t.Fatalf("登记应完成: %+v", res)
	}
	if !strings.Contains(res.Note, "建桶失败") {
		t.Fatalf("Note 应说明建桶失败: %q", res.Note)
	}
	if fc.ensured != 3 {
		t.Fatalf("建桶应重试 3 次, got %d", fc.ensured)
	}
}

// TestRegisterDialFailure dial 失败(如 endpoint 非法)→ 跳过建桶记 Note,登记照常。
func TestRegisterDialFailure(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{}}
	db := storetest.OpenDB(t)
	svc := &service{
		store:  NewStore(db),
		vaultW: fv,
		dial: func(providerType, endpoint, accessKeyID, accessKeySecret, bucket string) (ObjectClient, error) {
			return nil, errString("bad endpoint")
		},
	}
	res, err := svc.Register(ctx, registerInput())
	if err != nil {
		t.Fatalf("dial 失败不应阻断登记: %v", err)
	}
	if !strings.Contains(res.Note, "跳过建桶") {
		t.Fatalf("Note 应说明跳过建桶: %q", res.Note)
	}
}

// TestRegisterValidation 入参校验与 vault 前置。
func TestRegisterValidation(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{}}
	svc := newRegisterTestService(t, fv, &fakeEnsurerClient{})

	badType := registerInput()
	badType.Type = "minio"
	if _, err := svc.Register(ctx, badType); !errors.Is(err, ErrInvalidType) {
		t.Fatalf("非法类型应 ErrInvalidType, got %v", err)
	}
	emptySecret := registerInput()
	emptySecret.Secret = ""
	if _, err := svc.Register(ctx, emptySecret); !errors.Is(err, ErrEmptySecret) {
		t.Fatalf("空 secret 应 ErrEmptySecret, got %v", err)
	}
	// vaultW 未注入(vault 未配置)→ ErrVaultUnconfigured。
	noVault := newRegisterTestService(t, nil, &fakeEnsurerClient{})
	if _, err := noVault.Register(ctx, registerInput()); !errors.Is(err, ErrVaultUnconfigured) {
		t.Fatalf("vault 未配置应 ErrVaultUnconfigured, got %v", err)
	}
}

// restoreRetryDelay 把建桶重试间隔置零并注册恢复(测试加速;失败路径 3 次重试不真等)。
func restoreRetryDelay(t *testing.T) {
	t.Helper()
	orig := registerBucketRetryDelay
	registerBucketRetryDelay = 0
	t.Cleanup(func() { registerBucketRetryDelay = orig })
}
