package objectstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
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
type dialFactory func(providerType, endpoint, accessKeyID, accessKeySecret, bucket string) (ObjectClient, error)

// prodDialFactory 是生产工厂:真实 SDK 客户端(阿里云 OSS / RustFS)。
func prodDialFactory(providerType, endpoint, accessKeyID, accessKeySecret, bucket string) (ObjectClient, error) {
	return newObjectClient(providerType, endpoint, accessKeyID, accessKeySecret, bucket)
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
	client, err := s.dial(p.Type, p.Endpoint, p.AccessKeyID, secret, p.Bucket)
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
	client, err := s.dial(p.Type, p.Endpoint, p.AccessKeyID, secret, p.Bucket)
	if err != nil {
		return fmt.Errorf("%w:%s", ErrUploadFailed, describeClientError(err))
	}
	if err := client.PutObject(ctx, joinPrefixKey(p.PathPrefix, objectKey), body); err != nil {
		return fmt.Errorf("%w:%s", ErrUploadFailed, describeClientError(err))
	}
	return nil
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
	client, err := s.dial(in.Type, in.Endpoint, in.AccessKeyID, in.Secret, in.Bucket)
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
	p, err := s.Create(ctx, CreateInput{
		Type: in.Type, Name: in.Name, Endpoint: in.Endpoint, Bucket: in.Bucket,
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
