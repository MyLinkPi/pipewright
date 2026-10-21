package objectstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/vault"
)

// vaultReader 抽象「按 id 取凭据明文(不刷新 last_used_at)+ 校验存在」的最小能力(注入便于单测)。
// 复用 vault.Vault 的 Reveal/Exists;本包只读取 AccessKeySecret 明文,绝不入库/日志/响应。
type vaultReader interface {
	Reveal(id string) (string, error)
	Exists(id string) (bool, error)
}

// vaultWriter 抽象「创建/轮换/删除凭据」的最小能力(Register 自动登记用;vault.Vault 原样满足)。
type vaultWriter interface {
	Create(vault.CreateInput) (*vault.Credential, error)
	Update(id string, in vault.UpdateInput) (*vault.Credential, error)
	Delete(id string) error
}

// dialFactory 据提供商类型 + 接入参数造一个 ObjectClient(注入便于单测;生产用 prodDialFactory)。
// useSSL 决定传输层是否走 TLS(Endpoint 本身不含 scheme)。
type dialFactory func(providerType, endpoint string, useSSL bool, accessKeyID, accessKeySecret, bucket string) (ObjectClient, error)

// prodDialFactory 是生产工厂:真实 SDK 客户端(阿里云 OSS / RustFS)。
func prodDialFactory(providerType, endpoint string, useSSL bool, accessKeyID, accessKeySecret, bucket string) (ObjectClient, error) {
	return newObjectClient(providerType, endpoint, useSSL, accessKeyID, accessKeySecret, bucket)
}

// service 是 store + vault + dialFactory 支撑的 Service 实现。
type service struct {
	store  *Store
	vault  vaultReader
	vaultW vaultWriter // vault 的写半边(Register 用);v 未实现写入能力时为 nil
	dial   dialFactory
}

// New 构造 Service。
//   - db:参数化 SQL 触库。
//   - v:凭据保险库(取 AccessKeySecret 明文);nil/未配置时 Test/Upload 返回 ErrVaultUnconfigured。
//
// 不在此做任何重活(无 init 副作用)。
func New(db *sql.DB, v vault.Vault) Service {
	var vr vaultReader
	var vw vaultWriter
	if v != nil {
		vr = v
		vw = v // vault.Vault 接口本身即满足 vaultWriter
	}
	return &service{
		store:  NewStore(db),
		vault:  vr,
		vaultW: vw,
		dial:   prodDialFactory,
	}
}

func (s *service) List(ctx context.Context) ([]Provider, error) {
	return s.store.list(ctx)
}

func (s *service) Get(ctx context.Context, id string) (*Provider, error) {
	return s.store.get(ctx, id)
}

func (s *service) Create(ctx context.Context, in CreateInput) (*Provider, error) {
	normalizeCreate(&in)
	if err := validateCreate(in); err != nil {
		return nil, err
	}
	// 校验凭据存在(避免悬挂引用);vault 未配置时仍允许登记(存在性在 Test/Upload 时再校)。
	if s.vault != nil {
		ok, err := s.vault.Exists(in.CredentialID)
		if err != nil {
			if !errors.Is(err, vault.ErrVaultUnconfigured) {
				return nil, fmt.Errorf("objectstore: check credential: %w", err)
			}
		} else if !ok {
			return nil, ErrCredentialNotFound
		}
	}
	p := newProvider(in)
	if err := s.store.create(ctx, p); err != nil {
		return nil, err
	}
	return s.store.get(ctx, p.ID)
}

func (s *service) Update(ctx context.Context, id string, in UpdateInput) (*Provider, error) {
	// 先取原行以校验存在(UpdateInput 的字段级校验在 store.update 内)。
	if _, err := s.store.get(ctx, id); err != nil {
		return nil, err
	}
	return s.store.update(ctx, id, in)
}

func (s *service) Delete(ctx context.Context, id string) error {
	return s.store.del(ctx, id)
}

func (s *service) Test(ctx context.Context, id string) (*TestResult, error) {
	p, err := s.store.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.vault == nil {
		return nil, ErrVaultUnconfigured
	}
	secret, err := s.vault.Reveal(p.CredentialID)
	if err != nil {
		return nil, mapVaultErr(err)
	}
	client, err := s.dial(p.Type, p.Endpoint, p.UseSSL, p.AccessKeyID, secret, p.Bucket)
	if err != nil {
		return &TestResult{OK: false, Err: ErrTestFailed, Detail: describeClientError(err)}, nil
	}
	start := time.Now()
	verr := client.VerifyBucket(ctx)
	latency := time.Since(start).Milliseconds()
	if verr != nil {
		return &TestResult{OK: false, LatencyMs: latency, Err: ErrTestFailed, Detail: describeClientError(verr)}, nil
	}
	return &TestResult{OK: true, LatencyMs: latency, Detail: "连接成功,桶「" + p.Bucket + "」可访问"}, nil
}

func (s *service) Upload(ctx context.Context, providerID, objectKey string, body io.Reader) error {
	p, err := s.store.get(ctx, providerID)
	if err != nil {
		return err
	}
	if s.vault == nil {
		return ErrVaultUnconfigured
	}
	secret, err := s.vault.Reveal(p.CredentialID)
	if err != nil {
		return mapVaultErr(err)
	}
	client, err := s.dial(p.Type, p.Endpoint, p.UseSSL, p.AccessKeyID, secret, p.Bucket)
	if err != nil {
		return fmt.Errorf("%w:%s", ErrUploadFailed, describeClientError(err))
	}
	if err := client.PutObject(ctx, joinPrefixKey(p.PathPrefix, objectKey), body); err != nil {
		return fmt.Errorf("%w:%s", ErrUploadFailed, describeClientError(err))
	}
	return nil
}

// 浏览分页大小契约:默认 100,上限 1000(桶可能对象巨多,单页绝不大口吞)。
const (
	DefaultBrowsePageSize = 100
	MaxBrowsePageSize     = 1000
)

// Browse 取该配置凭据 → 单页列举 prefix 目录(delimiter="/",marker 游标分页)。
// prefix 为相对 PathPrefix 的路径(浏览根锚定在配置前缀之下,与 Upload 写入位置一致);
// marker 为空 = 第一页。Secret 用完即弃,绝不外泄。
func (s *service) Browse(ctx context.Context, id, prefix, marker string, maxKeys int) (*ListPageResult, error) {
	p, err := s.store.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.vault == nil {
		return nil, ErrVaultUnconfigured
	}
	if maxKeys <= 0 {
		maxKeys = DefaultBrowsePageSize
	}
	if maxKeys > MaxBrowsePageSize {
		maxKeys = MaxBrowsePageSize
	}
	secret, err := s.vault.Reveal(p.CredentialID)
	if err != nil {
		return nil, mapVaultErr(err)
	}
	client, err := s.dial(p.Type, p.Endpoint, p.UseSSL, p.AccessKeyID, secret, p.Bucket)
	if err != nil {
		return nil, fmt.Errorf("%w:%s", ErrBrowseFailed, describeClientError(err))
	}
	// 拼配置级 PathPrefix 并规范成目录前缀形态(以 / 结尾;根目录为空串)。
	full := strings.Trim(joinPrefixKey(p.PathPrefix, prefix), "/")
	if full != "" {
		full += "/"
	}
	res, err := client.ListPage(ctx, ListPageInput{Prefix: full, Marker: marker, MaxKeys: maxKeys})
	if err != nil {
		return nil, fmt.Errorf("%w:%s", ErrBrowseFailed, describeClientError(err))
	}
	return &res, nil
}

// ListBuckets 取该配置凭据 → 列举账户下的桶(设置页 Bucket 字段的候选下拉)。
// 按名字排序,上限 maxBucketCandidates 条(超出置 Truncated)。Secret 用完即弃,绝不外泄。
func (s *service) ListBuckets(ctx context.Context, id string) (*BucketListResult, error) {
	p, err := s.store.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.vault == nil {
		return nil, ErrVaultUnconfigured
	}
	secret, err := s.vault.Reveal(p.CredentialID)
	if err != nil {
		return nil, mapVaultErr(err)
	}
	client, err := s.dial(p.Type, p.Endpoint, p.UseSSL, p.AccessKeyID, secret, p.Bucket)
	if err != nil {
		return nil, fmt.Errorf("%w:%s", ErrListBucketsFailed, describeClientError(err))
	}
	buckets, err := client.ListBuckets(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w:%s", ErrListBucketsFailed, describeClientError(err))
	}
	return capBucketList(buckets), nil
}

// ProbeListBuckets 用未落库的明文入参直连存储列举桶(添加弹窗配置尚未创建、无 id 可用)。
// 不落库、不写 vault、不审计;Secret 仅在内存态参与 dial,用完即弃,绝不外泄。
func (s *service) ProbeListBuckets(ctx context.Context, in ProbeInput) (*BucketListResult, error) {
	in.Type = strings.ToLower(strings.TrimSpace(in.Type))
	in.Endpoint = normalizeEndpoint(in.Endpoint)
	in.AccessKeyID = strings.TrimSpace(in.AccessKeyID)
	if !ValidType(in.Type) {
		return nil, ErrInvalidType
	}
	if in.Endpoint == "" {
		return nil, ErrEmptyEndpoint
	}
	if in.AccessKeyID == "" {
		return nil, ErrEmptyAccessKeyID
	}
	// Secret 不 Trim(逐字节保真),只拒空。
	if in.Secret == "" {
		return nil, ErrEmptySecret
	}
	client, err := s.dial(in.Type, in.Endpoint, in.UseSSL, in.AccessKeyID, in.Secret, "")
	if err != nil {
		return nil, fmt.Errorf("%w:%s", ErrListBucketsFailed, describeClientError(err))
	}
	buckets, err := client.ListBuckets(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w:%s", ErrListBucketsFailed, describeClientError(err))
	}
	return capBucketList(buckets), nil
}

// capBucketList 把桶候选按名字排序并截断到上限(返回前 N 名 + Truncated 标记;
// 空结果归一为空切片而非 nil,稳定 JSON 形态)。
func capBucketList(buckets []BucketEntry) *BucketListResult {
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Name < buckets[j].Name })
	if len(buckets) > maxBucketCandidates {
		return &BucketListResult{Buckets: buckets[:maxBucketCandidates], Truncated: true}
	}
	if buckets == nil {
		buckets = []BucketEntry{}
	}
	return &BucketListResult{Buckets: buckets}
}

// CreateBucket 在 sourceID 配置的账户下新建 bucket 并登记为一份新配置(浏览页「新建桶」)。
// 校验(桶名/重复配置)先于触网;凭据从源配置取出后经 vault 独立重封(不共享 credentialId:
// 删除配置的级联删凭据会误伤同账户其他配置)。半程失败在服务端留下的孤儿桶(桶已建成但
// 配置没登记)在重试时经归属校验自动接续,不阻断登记。
func (s *service) CreateBucket(ctx context.Context, sourceID, bucket, name string) (*Provider, error) {
	p, err := s.store.get(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	bucket = strings.ToLower(strings.TrimSpace(bucket))
	if !bucketNameRe.MatchString(bucket) {
		return nil, ErrInvalidBucket
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = bucket
	}
	// 同账户已登记过该桶 → 直接拒绝(不产生重复条目;想浏览旧桶在设置页即可管理)。
	if existing, err := s.findByTypeEndpointBucket(ctx, p.Type, p.Endpoint, bucket); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, ErrDuplicateBucket
	}
	if s.vault == nil || s.vaultW == nil {
		return nil, ErrVaultUnconfigured
	}
	secret, err := s.vault.Reveal(p.CredentialID)
	if err != nil {
		return nil, mapVaultErr(err)
	}
	client, err := s.dial(p.Type, p.Endpoint, p.UseSSL, p.AccessKeyID, secret, p.Bucket)
	if err != nil {
		return nil, fmt.Errorf("%w:%s", ErrBucketCreateFailed, describeClientError(err))
	}
	creator, ok := client.(bucketCreator)
	if !ok {
		return nil, fmt.Errorf("%w:%s", ErrBucketCreateFailed, "该存储类型不支持新建桶")
	}
	cerr := creator.CreateBucket(ctx, bucket)
	if cerr != nil {
		if isBucketAlreadyOwned(cerr) {
			// 服务端明示桶已存在且归当前账户 → 视为建桶成功(重试接续半程失败)。
			cerr = nil
		} else if vclient, derr := s.dial(p.Type, p.Endpoint, p.UseSSL, p.AccessKeyID, secret, bucket); derr == nil {
			// 「桶已存在」也可能归当前账户所有(部分 S3 实现不区分归属):用新桶名绑一个客户端
			// 校验访问权——校验通过视为成功(孤儿桶接续);无权限(名被他人占用)保留原错误。
			if verr := vclient.VerifyBucket(ctx); verr == nil {
				cerr = nil
			}
		}
	}
	if cerr != nil {
		return nil, fmt.Errorf("%w:%s", ErrBucketCreateFailed, describeClientError(cerr))
	}
	// 凭据独立重封:同账户多份配置各持各的 credentialId,删除级联互不影响。
	cred, err := s.vaultW.Create(vault.CreateInput{
		Name:   "对象存储 · " + name,
		Type:   vault.TypeObjectStoreSecret,
		Secret: secret,
	})
	if err != nil {
		return nil, mapVaultErr(err)
	}
	useSSL := p.UseSSL
	np, err := s.Create(ctx, CreateInput{
		Type: p.Type, Name: name, Endpoint: p.Endpoint, UseSSL: &useSSL, Bucket: bucket,
		AccessKeyID: p.AccessKeyID, CredentialID: cred.ID,
	})
	if err != nil {
		// 登记失败 → 回滚刚存的凭据,避免悬挂。
		_ = s.vaultW.Delete(cred.ID)
		return nil, err
	}
	return np, nil
}

// registerBucketRetryDelay 是自动登记建桶的重试间隔(部署后容器可能尚未就绪);包级变量便于测试置零。
var registerBucketRetryDelay = 2 * time.Second

// Register 自动登记一份对象存储配置(应用商店部署 RustFS 联动)。
// 尽力而为先确保默认桶存在(客户端支持才做;失败记入 Note 不阻断)→ 凭据经 vault 密封 →
// 同 type+endpoint 命中既有配置则原地轮换,否则新建。Secret 明文用完即弃,绝不入库/日志/响应。
func (s *service) Register(ctx context.Context, in RegisterInput) (*RegisterResult, error) {
	in.Type = strings.ToLower(strings.TrimSpace(in.Type))
	in.Name = strings.TrimSpace(in.Name)
	in.Endpoint = normalizeEndpoint(in.Endpoint)
	in.Bucket = strings.TrimSpace(in.Bucket)
	in.AccessKeyID = strings.TrimSpace(in.AccessKeyID)
	if !ValidType(in.Type) {
		return nil, ErrInvalidType
	}
	if in.Name == "" {
		return nil, ErrEmptyName
	}
	if in.Endpoint == "" {
		return nil, ErrEmptyEndpoint
	}
	if in.Bucket == "" {
		return nil, ErrEmptyBucket
	}
	if in.AccessKeyID == "" {
		return nil, ErrEmptyAccessKeyID
	}
	// Secret 不 Trim(逐字节保真),只拒空。
	if in.Secret == "" {
		return nil, ErrEmptySecret
	}
	if s.vaultW == nil {
		return nil, ErrVaultUnconfigured
	}

	// 尽力而为建桶:客户端实现 bucketEnsurer 才做(自建 S3 兼容存储);失败不阻断登记
	//(配置如实反映部署结果,桶可稍后手动补建或重新部署)。
	note := ""
	client, err := s.dial(in.Type, in.Endpoint, in.UseSSL, in.AccessKeyID, in.Secret, in.Bucket)
	if err != nil {
		note = "跳过建桶:" + describeClientError(err)
	} else if be, ok := client.(bucketEnsurer); ok {
		if berr := ensureBucketWithRetry(ctx, be); berr != nil {
			note = "建桶失败(可稍后在存储控制台手动创建,或重新部署重试):" + describeClientError(berr)
		}
	}

	existing, err := s.findByEndpoint(ctx, in.Type, in.Endpoint)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		// 先轮换密文凭据再改接入参数(密钥是认证关键半边);Name 不动(尊重用户手改的展示名)。
		if _, err := s.vaultW.Update(existing.CredentialID, vault.UpdateInput{Secret: &in.Secret}); err != nil {
			return nil, mapVaultErr(err)
		}
		ak, bucket := in.AccessKeyID, in.Bucket
		p, err := s.Update(ctx, existing.ID, UpdateInput{Bucket: &bucket, AccessKeyID: &ak})
		if err != nil {
			return nil, err
		}
		return &RegisterResult{Provider: p, Updated: true, Note: note}, nil
	}
	cred, err := s.vaultW.Create(vault.CreateInput{
		Name:   "对象存储 · " + in.Name,
		Type:   vault.TypeObjectStoreSecret,
		Secret: in.Secret,
	})
	if err != nil {
		return nil, mapVaultErr(err)
	}
	useSSL := in.UseSSL
	p, err := s.Create(ctx, CreateInput{
		Type: in.Type, Name: in.Name, Endpoint: in.Endpoint, UseSSL: &useSSL, Bucket: in.Bucket,
		AccessKeyID: in.AccessKeyID, CredentialID: cred.ID,
	})
	if err != nil {
		// 登记失败 → 回滚刚存的凭据,避免悬挂(配置没建成,凭据不应留存)。
		_ = s.vaultW.Delete(cred.ID)
		return nil, err
	}
	return &RegisterResult{Provider: p, Note: note}, nil
}

// findByEndpoint 按 type+endpoint 找既有配置(无 → nil):自动登记的幂等锚点,
// 重新部署同一实例时原地轮换而不是产生重复条目。
func (s *service) findByEndpoint(ctx context.Context, providerType, endpoint string) (*Provider, error) {
	list, err := s.store.list(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Type == providerType && list[i].Endpoint == endpoint {
			return &list[i], nil
		}
	}
	return nil, nil
}

// findByTypeEndpointBucket 按 type+endpoint+bucket 找既有配置(无 → nil):新建桶的
// 重复登记检查锚点,同账户同桶只允许存在一份配置。
func (s *service) findByTypeEndpointBucket(ctx context.Context, providerType, endpoint, bucket string) (*Provider, error) {
	list, err := s.store.list(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Type == providerType && list[i].Endpoint == endpoint && list[i].Bucket == bucket {
			return &list[i], nil
		}
	}
	return nil, nil
}

// ensureBucketWithRetry 短重试建桶(容器刚起时可能连接拒绝;共 3 次,间隔 registerBucketRetryDelay,
// 总耗时上界约 2×间隔)。
func ensureBucketWithRetry(ctx context.Context, be bucketEnsurer) error {
	var err error
	for i := 0; i < 3; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(registerBucketRetryDelay):
			}
		}
		if err = be.EnsureBucket(ctx); err == nil {
			return nil
		}
	}
	return err
}

// mapVaultErr 把 vault 领域错误映射为本包语义(其余原样透出)。
func mapVaultErr(err error) error {
	switch {
	case errors.Is(err, vault.ErrVaultUnconfigured):
		return ErrVaultUnconfigured
	case errors.Is(err, vault.ErrNotFound):
		return ErrCredentialNotFound
	default:
		return err
	}
}

// joinPrefixKey 把配置级路径前缀拼到调用方给的对象键前(两侧各去首尾斜杠,空侧省略分隔符)。
// 语义:存储配置的 PathPrefix 在最前(存储级默认),节点级前缀/文件名拼在其后。
func joinPrefixKey(prefix, objectKey string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	key := strings.Trim(strings.TrimSpace(objectKey), "/")
	switch {
	case prefix == "":
		return key
	case key == "":
		return prefix
	default:
		return prefix + "/" + key
	}
}
