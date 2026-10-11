package objectstore

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// fakeVault 是 vaultReader 的内存实现(便于单测,不触真库)。
type fakeVault struct {
	configured bool
	secrets    map[string]string
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
