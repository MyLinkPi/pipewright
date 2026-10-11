package objectstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/huangchengsir/pipewright/internal/vault"
)

// vaultReader 抽象「按 id 取凭据明文(不刷新 last_used_at)+ 校验存在」的最小能力(注入便于单测)。
// 复用 vault.Vault 的 Reveal/Exists;本包只读取 AccessKeySecret 明文,绝不入库/日志/响应。
type vaultReader interface {
	Reveal(id string) (string, error)
	Exists(id string) (bool, error)
}

// dialFactory 据提供商类型 + 接入参数造一个 ObjectClient(注入便于单测;生产用 prodDialFactory)。
type dialFactory func(providerType, endpoint, accessKeyID, accessKeySecret, bucket string) (ObjectClient, error)

// prodDialFactory 是生产工厂:真实 SDK 客户端(阿里云 OSS)。
func prodDialFactory(providerType, endpoint, accessKeyID, accessKeySecret, bucket string) (ObjectClient, error) {
	return newObjectClient(providerType, endpoint, accessKeyID, accessKeySecret, bucket)
}

// service 是 store + vault + dialFactory 支撑的 Service 实现。
type service struct {
	store *Store
	vault vaultReader
	dial  dialFactory
}

// New 构造 Service。
//   - db:参数化 SQL 触库。
//   - v:凭据保险库(取 AccessKeySecret 明文);nil/未配置时 Test/Upload 返回 ErrVaultUnconfigured。
//
// 不在此做任何重活(无 init 副作用)。
func New(db *sql.DB, v vault.Vault) Service {
	var vr vaultReader
	if v != nil {
		vr = v
	}
	return &service{
		store: NewStore(db),
		vault: vr,
		dial:  prodDialFactory,
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
	if err := client.PutObject(ctx, objectKey, body); err != nil {
		return fmt.Errorf("%w:%s", ErrUploadFailed, describeClientError(err))
	}
	return nil
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
