package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/vault"
	"github.com/minio/minio-go/v7"
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

// fakeClient 是 ObjectClient 的内存实现(记录上传内容与列举/建桶调用)。
type fakeClient struct {
	verifyErr error
	putErr    error
	puts      map[string]string
	// getErr 非空时 GetObject 原样返回(模拟网络/凭据失败);否则从 puts 取值,
	// 未命中返回 ErrObjectNotFound(与真实客户端语义一致)。
	getErr  error
	listErr error
	// listPages 按 "prefix|marker" 键返回预设页(未命中返回空页)。
	listPages map[string]ListPageResult
	listCalls []ListPageInput
	// 建桶:createErr 非空时原样返回(模拟服务端拒绝);createdBuckets 记录调用。
	createErr      error
	createdBuckets []string
	// 列桶:buckets 为预设候选;listBucketsErr 非空时模拟失败。
	buckets        []BucketEntry
	listBucketsErr error
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

func (c *fakeClient) GetObject(ctx context.Context, objectKey string, w io.Writer) error {
	if c.getErr != nil {
		return c.getErr
	}
	v, ok := c.puts[objectKey]
	if !ok {
		return fmt.Errorf("%w:%s", ErrObjectNotFound, objectKey)
	}
	_, err := io.WriteString(w, v)
	return err
}

func (c *fakeClient) ListPage(ctx context.Context, in ListPageInput) (ListPageResult, error) {
	c.listCalls = append(c.listCalls, in)
	if c.listErr != nil {
		return ListPageResult{}, c.listErr
	}
	if res, ok := c.listPages[in.Prefix+"|"+in.Marker]; ok {
		return res, nil
	}
	return ListPageResult{}, nil
}

func (c *fakeClient) CreateBucket(ctx context.Context, bucketName string) error {
	c.createdBuckets = append(c.createdBuckets, bucketName)
	return c.createErr
}

// listBucketsErr 非空时 ListBuckets 原样返回(模拟凭据无效/不可达);否则返回 buckets。
func (c *fakeClient) ListBuckets(ctx context.Context) ([]BucketEntry, error) {
	if c.listBucketsErr != nil {
		return nil, c.listBucketsErr
	}
	return c.buckets, nil
}

// newTestService 用内存 DB + fake vault + fake client 构造 Service。
// fv 为 nil 时代表 vault 未注入(Test/Upload/Browse/CreateBucket 应返回 ErrVaultUnconfigured);
// 非空时同时注入读/写两半边(fakeVault 均满足)。dialed 记录 dial 收到的参数(断言 useSSL 传递)。
func newTestService(t *testing.T, fv *fakeVault, client ObjectClient) Service {
	t.Helper()
	db := storetest.OpenDB(t)
	var vr vaultReader
	var vw vaultWriter
	if fv != nil {
		vr = fv
		vw = fv
	}
	return &service{
		store:  NewStore(db),
		vault:  vr,
		vaultW: vw,
		dial: func(providerType, endpoint string, useSSL bool, accessKeyID, accessKeySecret, bucket string) (ObjectClient, error) {
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

// newRecordingTestService 同 newTestService,但记录每次 dial 收到的参数(断言 useSSL 传递)。
func newRecordingTestService(t *testing.T, fv *fakeVault, client ObjectClient, dialed *[]dialRecord) Service {
	t.Helper()
	db := storetest.OpenDB(t)
	var vr vaultReader
	var vw vaultWriter
	if fv != nil {
		vr = fv
		vw = fv
	}
	return &service{
		store:  NewStore(db),
		vault:  vr,
		vaultW: vw,
		dial: func(providerType, endpoint string, useSSL bool, accessKeyID, accessKeySecret, bucket string) (ObjectClient, error) {
			if providerType != TypeAliyunOSS {
				return nil, ErrInvalidType
			}
			*dialed = append(*dialed, dialRecord{providerType: providerType, endpoint: endpoint, useSSL: useSSL, accessKeyID: accessKeyID, bucket: bucket})
			return client, nil
		},
	}
}

// TestCreateUseSSLPersisted 覆盖 useSSL 落库与更新:OSS 缺省 true;Update 指针切换为 false 生效。
func TestCreateUseSSLPersisted(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{"cred-1": "secret-value"}}
	svc := newTestService(t, fv, &fakeClient{})

	p, err := svc.Create(ctx, sampleInput()) // UseSSL=nil → 按类型默认 true
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !p.UseSSL {
		t.Fatalf("OSS 缺省应 useSSL=true: %+v", p)
	}
	f := false
	upd, err := svc.Update(ctx, p.ID, UpdateInput{UseSSL: &f})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if upd.UseSSL {
		t.Fatalf("Update 应切换 useSSL=false: %+v", upd)
	}
	// nil = 不修改,开关保持 false。
	if _, err := svc.Update(ctx, p.ID, UpdateInput{}); err != nil {
		t.Fatalf("空 Update: %v", err)
	}
	got, _ := svc.Get(ctx, p.ID)
	if got.UseSSL {
		t.Fatalf("空 Update 不应改动 useSSL: %+v", got)
	}
}

// TestTestPassesUseSSLToDial 覆盖 useSSL 向 dial 的传递:配置的开关原样到达客户端工厂。
func TestTestPassesUseSSLToDial(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{"cred-1": "secret-value"}}
	var dialed []dialRecord
	svc := newRecordingTestService(t, fv, &fakeClient{}, &dialed)

	f := false
	p, err := svc.Create(ctx, func() CreateInput { in := sampleInput(); in.UseSSL = &f; return in }())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Test(ctx, p.ID); err != nil {
		t.Fatalf("Test: %v", err)
	}
	if len(dialed) != 1 || dialed[0].useSSL {
		t.Fatalf("dial 应收到 useSSL=false: %+v", dialed)
	}
}

// TestListBuckets 覆盖既有配置列桶:按名字排序、失败映射 ErrListBucketsFailed、vault 未配置拒绝。
func TestListBuckets(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{"cred-1": "secret-value"}}
	fc := &fakeClient{buckets: []BucketEntry{{Name: "zeta"}, {Name: "alpha"}}}
	svc := newTestService(t, fv, fc)
	p, _ := svc.Create(ctx, sampleInput())

	res, err := svc.ListBuckets(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if len(res.Buckets) != 2 || res.Buckets[0].Name != "alpha" || res.Buckets[1].Name != "zeta" || res.Truncated {
		t.Fatalf("桶列表应按名字排序: %+v", res)
	}

	failSvc := newTestService(t, fv, &fakeClient{listBucketsErr: errors.New("AccessDenied")})
	fp, _ := failSvc.Create(ctx, sampleInput())
	if _, err := failSvc.ListBuckets(ctx, fp.ID); !errors.Is(err, ErrListBucketsFailed) {
		t.Fatalf("列桶失败应 ErrListBucketsFailed, got %v", err)
	}

	noVault := newTestService(t, nil, fc) // vault 未注入
	np, _ := noVault.Create(ctx, sampleInput())
	if _, err := noVault.ListBuckets(ctx, np.ID); !errors.Is(err, ErrVaultUnconfigured) {
		t.Fatalf("vault 未配置应 ErrVaultUnconfigured, got %v", err)
	}
}

// TestProbeListBuckets 覆盖未落库直连探测:参数原样(含 useSSL/归一化 endpoint)到达 dial、
// 校验拒绝、失败映射;全程不触 vault。
func TestProbeListBuckets(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{}}
	var dialed []dialRecord
	svc := newRecordingTestService(t, fv, &fakeClient{buckets: []BucketEntry{{Name: "b1"}}}, &dialed)

	res, err := svc.ProbeListBuckets(ctx, ProbeInput{
		Type: "Aliyun_OSS", Endpoint: "https://oss-cn-hangzhou.aliyuncs.com/", UseSSL: true,
		AccessKeyID: "LTAIxxxx", Secret: "probe-secret",
	})
	if err != nil {
		t.Fatalf("ProbeListBuckets: %v", err)
	}
	if len(res.Buckets) != 1 || res.Buckets[0].Name != "b1" {
		t.Fatalf("探测结果不符: %+v", res)
	}
	if len(dialed) != 1 || !dialed[0].useSSL || dialed[0].endpoint != "oss-cn-hangzhou.aliyuncs.com" {
		t.Fatalf("dial 应收到归一化 endpoint + useSSL=true: %+v", dialed)
	}
	if len(fv.secrets) != 0 {
		t.Fatalf("探测绝不写 vault: %+v", fv.secrets)
	}

	badType := ProbeInput{Type: "minio", Endpoint: "e", AccessKeyID: "ak", Secret: "s"}
	if _, err := svc.ProbeListBuckets(ctx, badType); !errors.Is(err, ErrInvalidType) {
		t.Fatalf("非法类型应 ErrInvalidType, got %v", err)
	}
	noSecret := ProbeInput{Type: TypeAliyunOSS, Endpoint: "e", AccessKeyID: "ak"}
	if _, err := svc.ProbeListBuckets(ctx, noSecret); !errors.Is(err, ErrEmptySecret) {
		t.Fatalf("空 secret 应 ErrEmptySecret, got %v", err)
	}
	noEndpoint := ProbeInput{Type: TypeAliyunOSS, AccessKeyID: "ak", Secret: "s"}
	if _, err := svc.ProbeListBuckets(ctx, noEndpoint); !errors.Is(err, ErrEmptyEndpoint) {
		t.Fatalf("空 endpoint 应 ErrEmptyEndpoint, got %v", err)
	}
}

// TestCapBucketList 覆盖候选截断:超上限取前 N 并置 Truncated;nil 归一为空切片。
func TestCapBucketList(t *testing.T) {
	big := make([]BucketEntry, maxBucketCandidates+10)
	for i := range big {
		big[i].Name = fmt.Sprintf("bucket-%04d", len(big)-i) // 倒序填充,验证排序
	}
	res := capBucketList(big)
	if len(res.Buckets) != maxBucketCandidates || !res.Truncated {
		t.Fatalf("超上限应截断: len=%d truncated=%v", len(res.Buckets), res.Truncated)
	}
	if res.Buckets[0].Name != "bucket-0001" {
		t.Fatalf("应按名字排序: %q", res.Buckets[0].Name)
	}
	empty := capBucketList(nil)
	if empty.Buckets == nil || len(empty.Buckets) != 0 || empty.Truncated {
		t.Fatalf("nil 应归一为空切片: %+v", empty)
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

func TestDownloadRoundtrip(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{"cred-1": "secret-value"}}
	fc := &fakeClient{}
	svc := newTestService(t, fv, fc)
	p, _ := svc.Create(ctx, sampleInput())

	if err := svc.Upload(ctx, p.ID, "platform-backups/a.pwb", strings.NewReader("archive-bytes")); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	var sb strings.Builder
	if err := svc.Download(ctx, p.ID, "platform-backups/a.pwb", &sb); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if sb.String() != "archive-bytes" {
		t.Fatalf("下载内容不符: %q", sb.String())
	}
	// 对象不存在 → 语义化 ErrObjectNotFound(供 httpapi 转 404)。
	if err := svc.Download(ctx, p.ID, "missing.pwb", &sb); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("缺失对象应 ErrObjectNotFound, got %v", err)
	}
	// 其余错误统一裹 ErrDownloadFailed。
	fc.getErr = errors.New("AccessDenied")
	if err := svc.Download(ctx, p.ID, "platform-backups/a.pwb", &sb); !errors.Is(err, ErrDownloadFailed) {
		t.Fatalf("下载失败应 ErrDownloadFailed, got %v", err)
	}
}

func TestDownloadRespectsPathPrefix(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{"cred-1": "secret-value"}}
	fc := &fakeClient{}
	svc := newTestService(t, fv, fc)
	in := sampleInput()
	in.PathPrefix = "team-a"
	p, _ := svc.Create(ctx, in)

	if err := svc.Upload(ctx, p.ID, "backups/a.pwb", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	// 逻辑键(相对 PathPrefix)下载应命中同一条物理键 —— 与 Upload 对称。
	var sb strings.Builder
	if err := svc.Download(ctx, p.ID, "backups/a.pwb", &sb); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if sb.String() != "x" || fc.puts["team-a/backups/a.pwb"] != "x" {
		t.Fatalf("PathPrefix 对称性不符: puts=%+v got=%q", fc.puts, sb.String())
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

// ---------- Browse(对象存储浏览;marker 游标单页,绝不聚合全量)----------

// TestBrowse 覆盖基本链路:prefix 透传(以 / 结尾,根为空)、结果原样返回。
func TestBrowse(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{"cred-1": "secret-value"}}
	fc := &fakeClient{listPages: map[string]ListPageResult{
		"releases/|": {Prefixes: []string{"releases/2026/"}, Objects: []ObjectEntry{{Key: "releases/app.jar", Size: 12}}},
		"|":          {Prefixes: []string{"releases/"}, Objects: nil},
	}}
	svc := newTestService(t, fv, fc)
	p, _ := svc.Create(ctx, sampleInput())

	res, err := svc.Browse(ctx, p.ID, "releases/", "", 100)
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if len(res.Prefixes) != 1 || res.Prefixes[0] != "releases/2026/" {
		t.Fatalf("子目录不符: %+v", res.Prefixes)
	}
	if len(res.Objects) != 1 || res.Objects[0].Key != "releases/app.jar" || res.Objects[0].Size != 12 {
		t.Fatalf("对象不符: %+v", res.Objects)
	}

	// 根目录(prefix 空)→ 列举前缀为空串。
	if _, err := svc.Browse(ctx, p.ID, "", "", 100); err != nil {
		t.Fatalf("Browse(根): %v", err)
	}
	if got := fc.listCalls[len(fc.listCalls)-1].Prefix; got != "" {
		t.Fatalf("根目录前缀应为空串, got %q", got)
	}
}

// TestBrowseAppliesPathPrefix 浏览根锚定在配置级 PathPrefix 之下(与 Upload 写入位置一致):
// 用户所见「根」即前缀根,无前缀配置时行为不变。
func TestBrowseAppliesPathPrefix(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{"cred-1": "secret-value"}}
	fc := &fakeClient{}
	svc := newTestService(t, fv, fc)
	in := sampleInput()
	in.PathPrefix = "/ci/prod/"
	p, _ := svc.Create(ctx, in)

	if _, err := svc.Browse(ctx, p.ID, "", "", 100); err != nil {
		t.Fatalf("Browse(前缀根): %v", err)
	}
	if got := fc.listCalls[0].Prefix; got != "ci/prod/" {
		t.Fatalf("前缀根应锚定 PathPrefix: %q", got)
	}

	// 子目录拼在前缀之后;入参不规范(首尾斜杠)也应归一。
	if _, err := svc.Browse(ctx, p.ID, "/releases", "", 100); err != nil {
		t.Fatalf("Browse(子目录): %v", err)
	}
	if got := fc.listCalls[1].Prefix; got != "ci/prod/releases/" {
		t.Fatalf("子目录前缀不符: %q", got)
	}
}

// TestBrowsePagination 分页契约:marker 原样透传给客户端(S3/OSS V1 游标语义);
// maxKeys 钳制(<=0 → 默认 100,超上限 → 1000)。
func TestBrowsePagination(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{"cred-1": "secret-value"}}
	fc := &fakeClient{}
	svc := newTestService(t, fv, fc)
	p, _ := svc.Create(ctx, sampleInput())

	if _, err := svc.Browse(ctx, p.ID, "", "releases/obj-0100", 50); err != nil {
		t.Fatalf("Browse(page2): %v", err)
	}
	call := fc.listCalls[0]
	if call.Marker != "releases/obj-0100" {
		t.Fatalf("marker 应透传: %q", call.Marker)
	}
	if call.MaxKeys != 50 {
		t.Fatalf("maxKeys 应透传: %d", call.MaxKeys)
	}

	if _, err := svc.Browse(ctx, p.ID, "", "", 0); err != nil {
		t.Fatalf("Browse(maxKeys=0): %v", err)
	}
	if got := fc.listCalls[1].MaxKeys; got != DefaultBrowsePageSize {
		t.Fatalf("maxKeys<=0 应取默认 %d, got %d", DefaultBrowsePageSize, got)
	}

	if _, err := svc.Browse(ctx, p.ID, "", "", 99999); err != nil {
		t.Fatalf("Browse(maxKeys 超上限): %v", err)
	}
	if got := fc.listCalls[2].MaxKeys; got != MaxBrowsePageSize {
		t.Fatalf("超上限应钳到 %d, got %d", MaxBrowsePageSize, got)
	}
}

// TestBrowseFailed 列举失败(凭据无读权限 / 网络不可达)→ ErrBrowseFailed。
func TestBrowseFailed(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{"cred-1": "secret-value"}}
	svc := newTestService(t, fv, &fakeClient{listErr: errors.New("AccessDenied")})
	p, _ := svc.Create(ctx, sampleInput())

	if _, err := svc.Browse(ctx, p.ID, "", "", 100); !errors.Is(err, ErrBrowseFailed) {
		t.Fatalf("列举失败应 ErrBrowseFailed, got %v", err)
	}
}

// TestBrowseWithoutVaultAndNotFound vault 未配置 → ErrVaultUnconfigured;配置不存在 → ErrNotFound。
func TestBrowseWithoutVaultAndNotFound(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, nil, &fakeClient{})
	fvInput := sampleInput()
	normalizeCreate(&fvInput)
	p := newProvider(fvInput)
	if err := svc.(*service).store.create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := svc.Browse(ctx, p.ID, "", "", 100); !errors.Is(err, ErrVaultUnconfigured) {
		t.Fatalf("vault 未配时 Browse 应 ErrVaultUnconfigured, got %v", err)
	}
	if _, err := svc.Browse(ctx, "no-such-id", "", "", 100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("配置不存在应 ErrNotFound, got %v", err)
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

// ---------- CreateBucket(浏览页「新建桶」:建桶 + 自动登记新配置)----------

// TestCreateBucket 覆盖成功链路:服务端建桶 → 凭据独立重封 → 登记新配置(同账户接入参数,
// 新桶、空前缀);展示名缺省取桶名;源配置凭据不受影响。
func TestCreateBucket(t *testing.T) {
	ctx := context.Background()
	// nextID: 1 让重封凭据从 cred-2 起,不与预置的 cred-1 撞号(便于断言凭据独立)。
	fv := &fakeVault{configured: true, nextID: 1, secrets: map[string]string{"cred-1": "secret-value"}}
	fc := &fakeClient{}
	svc := newTestService(t, fv, fc)
	p, _ := svc.Create(ctx, sampleInput())

	np, err := svc.CreateBucket(ctx, p.ID, "brand-new-bucket", "展示名")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if !reflect.DeepEqual(fc.createdBuckets, []string{"brand-new-bucket"}) {
		t.Fatalf("应恰好建桶一次: %v", fc.createdBuckets)
	}
	if np.Type != TypeAliyunOSS || np.Endpoint != p.Endpoint || np.Bucket != "brand-new-bucket" ||
		np.AccessKeyID != p.AccessKeyID || np.Name != "展示名" || np.PathPrefix != "" {
		t.Fatalf("新配置字段不符: %+v", np)
	}
	// 凭据独立:新配置持自己的 credentialId,vault 里两条凭据同 secret 不同 id。
	if np.CredentialID == p.CredentialID {
		t.Fatalf("凭据应独立重封,不应共享 credentialId")
	}
	if got := fv.secrets[np.CredentialID]; got != "secret-value" {
		t.Fatalf("新凭据应可解出同源 secret: %q", got)
	}
	list, _ := svc.List(ctx)
	if len(list) != 2 {
		t.Fatalf("应恰有 2 条配置, got %d", len(list))
	}

	// 展示名缺省 → 桶名。
	np2, err := svc.CreateBucket(ctx, p.ID, "another-bucket", "")
	if err != nil || np2.Name != "another-bucket" {
		t.Fatalf("空展示名应取桶名: %v %+v", err, np2)
	}
}

// TestCreateBucketValidation 桶名规则(3-63 位小写字母/数字/连字符,两端字母数字);
// 大写输入自动归一为小写(不作为错误)。
func TestCreateBucketValidation(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{"cred-1": "secret-value"}}
	svc := newTestService(t, fv, &fakeClient{})
	p, _ := svc.Create(ctx, sampleInput())

	for _, bad := range []string{"", "ab", "-abc", "abc-", "a_b", "a.b.c", strings.Repeat("a", 64), "has Upper"} {
		if _, err := svc.CreateBucket(ctx, p.ID, bad, ""); !errors.Is(err, ErrInvalidBucket) {
			t.Fatalf("桶名 %q 应 ErrInvalidBucket, got %v", bad, err)
		}
	}
	// 合法名 + 前后空白 → 归一化通过。
	np, err := svc.CreateBucket(ctx, p.ID, "  Good-Bucket-2  ", "")
	if err != nil || np.Bucket != "good-bucket-2" {
		t.Fatalf("大写/空白应归一化: %v %+v", err, np)
	}
}

// TestCreateBucketDuplicate 同账户同桶已有配置 → ErrDuplicateBucket(校验先于触网,
// createErr 不会被触发)。
func TestCreateBucketDuplicate(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{"cred-1": "secret-value"}}
	fc := &fakeClient{}
	svc := newTestService(t, fv, fc)
	p, _ := svc.Create(ctx, sampleInput()) // bucket = my-bucket

	if _, err := svc.CreateBucket(ctx, p.ID, "my-bucket", ""); !errors.Is(err, ErrDuplicateBucket) {
		t.Fatalf("重复桶应 ErrDuplicateBucket, got %v", err)
	}
	if len(fc.createdBuckets) != 0 {
		t.Fatalf("重复检查应在触网前拒绝, got %v", fc.createdBuckets)
	}
}

// TestCreateBucketFailureOrphanRecovery 建桶失败的两种恢复/报错路径:
//   - 「已存在」但凭据可访问(半程失败孤儿桶) → 校验通过视为成功,照常登记;
//   - 「已存在」且无权访问(名被他人占用) → ErrBucketCreateFailed;
//   - 服务端明示 BucketAlreadyOwnedByYou → 直接视为成功。
func TestCreateBucketFailureOrphanRecovery(t *testing.T) {
	ctx := context.Background()
	fv := &fakeVault{configured: true, secrets: map[string]string{"cred-1": "secret-value"}}

	// 孤儿桶接续:建桶报已存在,但 VerifyBucket 通过。
	svc := newTestService(t, fv, &fakeClient{createErr: minio.ErrorResponse{Code: "BucketAlreadyExists"}})
	p, _ := svc.Create(ctx, sampleInput())
	if np, err := svc.CreateBucket(ctx, p.ID, "orphan-bucket", ""); err != nil || np.Bucket != "orphan-bucket" {
		t.Fatalf("孤儿桶应接续登记成功: %v %+v", err, np)
	}

	// 名被他人占用:建桶报已存在且无权访问。
	svc2 := newTestService(t, fv, &fakeClient{
		createErr: minio.ErrorResponse{Code: "BucketAlreadyExists"},
		verifyErr: errors.New("AccessDenied"),
	})
	p2, _ := svc2.Create(ctx, sampleInput())
	if _, err := svc2.CreateBucket(ctx, p2.ID, "taken-bucket", ""); !errors.Is(err, ErrBucketCreateFailed) {
		t.Fatalf("他人占用应 ErrBucketCreateFailed, got %v", err)
	}

	// 服务端明示归己有 → 直接成功。
	svc3 := newTestService(t, fv, &fakeClient{
		createErr: minio.ErrorResponse{Code: "BucketAlreadyOwnedByYou"},
		verifyErr: errors.New("AccessDenied"), // 不应走到校验
	})
	p3, _ := svc3.Create(ctx, sampleInput())
	if _, err := svc3.CreateBucket(ctx, p3.ID, "mine-bucket", ""); err != nil {
		t.Fatalf("BucketAlreadyOwnedByYou 应视为成功: %v", err)
	}
}

// TestCreateBucketWithoutVault vault 未配置 → ErrVaultUnconfigured;源配置不存在 → ErrNotFound。
func TestCreateBucketWithoutVaultAndNotFound(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, nil, &fakeClient{})
	fvInput := sampleInput()
	normalizeCreate(&fvInput)
	p := newProvider(fvInput)
	if err := svc.(*service).store.create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := svc.CreateBucket(ctx, p.ID, "some-bucket", ""); !errors.Is(err, ErrVaultUnconfigured) {
		t.Fatalf("vault 未配时应 ErrVaultUnconfigured, got %v", err)
	}
	if _, err := svc.CreateBucket(ctx, "no-such-id", "some-bucket", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("配置不存在应 ErrNotFound, got %v", err)
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

// dialRecord 记录 dial 工厂收到的一次调用参数(断言 useSSL 传递用)。
type dialRecord struct {
	providerType string
	endpoint     string
	useSSL       bool
	accessKeyID  string
	bucket       string
}

// newRegisterTestService 构造走 rustfs 类型 + 可控 dial 的 Service(Register 链路专用);
// dialed 非 nil 时记录每次 dial 参数。
func newRegisterTestService(t *testing.T, fv *fakeVault, client ObjectClient, dialed *[]dialRecord) *service {
	t.Helper()
	db := storetest.OpenDB(t)
	var vw vaultWriter
	if fv != nil {
		vw = fv
	}
	return &service{
		store:  NewStore(db),
		vaultW: vw,
		dial: func(providerType, endpoint string, useSSL bool, accessKeyID, accessKeySecret, bucket string) (ObjectClient, error) {
			if providerType != TypeRustFS {
				return nil, ErrInvalidType
			}
			if dialed != nil {
				*dialed = append(*dialed, dialRecord{providerType: providerType, endpoint: endpoint, useSSL: useSSL, accessKeyID: accessKeyID, bucket: bucket})
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
	svc := newRegisterTestService(t, fv, fc, nil)

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
	if p.UseSSL {
		t.Fatalf("RustFS 自动登记应为明文(useSSL=false): %+v", p)
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
	svc := newRegisterTestService(t, fv, &fakeEnsurerClient{}, nil)

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
	svc := newRegisterTestService(t, fv, fc, nil)

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
		dial: func(providerType, endpoint string, useSSL bool, accessKeyID, accessKeySecret, bucket string) (ObjectClient, error) {
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
	svc := newRegisterTestService(t, fv, &fakeEnsurerClient{}, nil)

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
	noVault := newRegisterTestService(t, nil, &fakeEnsurerClient{}, nil)
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
