// Package objectstore 是「对象存储集成层」的领域层(流水线 OSS 上传节点配套)。
//
// 一条 Provider 代表一个对象存储账户接入(本期仅阿里云 OSS;类型枚举可扩展多云/多账户)。
//
// 凭据按机密性拆开存储:
//   - AccessKeyId 非机密 → 明文列存储,可回显可编辑;
//   - AccessKeySecret 机密 → vault 凭据加密存储(credential_id 引用),绝不入库明文/日志/响应/错误体。
//
// 能力:
//   - ObjectClient 抽象「校验桶可访问」「上传一个对象」(注入便于单测,不触真网络);
//     阿里云 OSS 用官方 SDK(aliyun-oss-go-sdk/oss)真实实现。
//   - Upload 供流水线 oss_upload 节点调用:按 providerID 解析 endpoint/bucket/AK,上传一段字节流。
package objectstore

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"
)

// 提供商类型枚举(DB 存小写字串;JSON 同名)。本期仅阿里云 OSS。
const (
	// TypeAliyunOSS 是阿里云对象存储 OSS(已真实实现)。
	TypeAliyunOSS = "aliyun_oss"
)

// 领域错误(人话由 httpapi 层映射状态码;错误体绝不含敏感信息)。
var (
	// ErrNotFound 表示对象存储配置不存在。
	ErrNotFound = errors.New("objectstore: object store not found")
	// ErrInvalidType 表示提供商类型枚举非法。
	ErrInvalidType = errors.New("objectstore: invalid object store type")
	// ErrEmptyName 表示展示名为空。
	ErrEmptyName = errors.New("objectstore: name must not be empty")
	// ErrEmptyEndpoint 表示 Endpoint 为空。
	ErrEmptyEndpoint = errors.New("objectstore: endpoint must not be empty")
	// ErrEmptyBucket 表示 Bucket 为空。
	ErrEmptyBucket = errors.New("objectstore: bucket must not be empty")
	// ErrEmptyAccessKeyID 表示 AccessKeyId 为空。
	ErrEmptyAccessKeyID = errors.New("objectstore: access key id must not be empty")
	// ErrEmptyCredentialID 表示未选择凭据。
	ErrEmptyCredentialID = errors.New("objectstore: credential id must not be empty")
	// ErrCredentialNotFound 表示引用的凭据不存在。
	ErrCredentialNotFound = errors.New("objectstore: referenced credential not found")
	// ErrVaultUnconfigured 表示保险库未配置 master key,无法取 AccessKeySecret。
	ErrVaultUnconfigured = errors.New("objectstore: vault unconfigured")
	// ErrTestFailed 表示连通性测试失败(凭据无效 / 桶不存在 / API 不可达)。错误体绝不含密钥。
	ErrTestFailed = errors.New("objectstore: connection test failed")
	// ErrUploadFailed 表示上传失败。
	ErrUploadFailed = errors.New("objectstore: upload failed")
)

// Provider 是一条对象存储接入配置的领域模型。绝不含 AccessKeySecret 明文,只持 CredentialID 引用;
// AccessKeyID 非机密,明文存储可回显。
type Provider struct {
	ID           string
	Type         string // aliyun_oss
	Name         string
	Endpoint     string // 服务接入点(如 oss-cn-hangzhou.aliyuncs.com;不含 scheme)
	Bucket       string // 默认桶
	AccessKeyID  string // 非机密:AccessKeyId,明文存储可回显
	CredentialID string // 指向 vault 凭据(只存 AccessKeySecret);明文绝不入此结构体
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// CreateInput 是登记对象存储的入参(Secret 由 httpapi 层先入 vault,此处只收 credentialId)。
type CreateInput struct {
	Type         string
	Name         string
	Endpoint     string
	Bucket       string
	AccessKeyID  string
	CredentialID string
}

// UpdateInput 是编辑对象存储的入参;指针字段为 nil 表示不修改(Secret 轮换由 httpapi 层
// 直接经 vault.Update 完成,credentialId 不变,不经此入参)。
type UpdateInput struct {
	Name        *string
	Endpoint    *string
	Bucket      *string
	AccessKeyID *string
}

// TestResult 是连通性测试结果(Err 为 nil 表示通过;Detail 为人话,绝不含密钥)。
type TestResult struct {
	OK        bool
	LatencyMs int64
	Detail    string
	Err       error
}

// ObjectClient 抽象一个对象存储账户的写/校验能力(注入便于单测,不触真网络)。
type ObjectClient interface {
	// VerifyBucket 校验凭据可访问目标桶(连通性 + 桶存在),失败返回人话错误(绝不含密钥)。
	VerifyBucket(ctx context.Context) error
	// PutObject 上传一个对象(objectKey 为对象键,body 为内容流)。
	PutObject(ctx context.Context, objectKey string, body io.Reader) error
}

// Service 定义对象存储领域对外接口(httpapi 消费 + 流水线节点上传)。
type Service interface {
	// List 返回全部对象存储配置(创建时间倒序;无 Secret)。
	List(ctx context.Context) ([]Provider, error)
	// Get 返回单条配置(无 Secret);不存在 → ErrNotFound。
	Get(ctx context.Context, id string) (*Provider, error)
	// Create 校验入参(含凭据存在性)→ 落库;返回配置视图(无 Secret)。
	Create(ctx context.Context, in CreateInput) (*Provider, error)
	// Update 编辑展示名 / Endpoint / Bucket / AccessKeyId(Secret 轮换走 vault,不经此);返回更新后视图。
	Update(ctx context.Context, id string, in UpdateInput) (*Provider, error)
	// Delete 删除配置;不存在 → ErrNotFound。
	Delete(ctx context.Context, id string) error

	// Test 取该配置凭据 → 经 ObjectClient 校验桶可访问,返回耗时/结果。Secret 用完即弃,绝不外泄。
	Test(ctx context.Context, id string) (*TestResult, error)

	// Upload 取该配置凭据 → 上传一段字节流到 objectKey(流水线 oss_upload 节点用)。
	// Secret 用完即弃,绝不外泄。
	Upload(ctx context.Context, providerID, objectKey string, body io.Reader) error
}

// ValidType 报告提供商类型是否为受支持枚举。
func ValidType(t string) bool {
	switch t {
	case TypeAliyunOSS:
		return true
	default:
		return false
	}
}

// normalizeEndpoint 归一化 Endpoint:去空白、去 scheme 前缀、去尾斜杠(OSS SDK 需不带 scheme 的接入点)。
func normalizeEndpoint(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimRight(s, "/")
	return s
}

// normalizeCreate 原地归一化登记入参(去空白 / 类型小写 / Endpoint 去 scheme)。
func normalizeCreate(in *CreateInput) {
	in.Type = strings.ToLower(strings.TrimSpace(in.Type))
	in.Name = strings.TrimSpace(in.Name)
	in.Endpoint = normalizeEndpoint(in.Endpoint)
	in.Bucket = strings.TrimSpace(in.Bucket)
	in.AccessKeyID = strings.TrimSpace(in.AccessKeyID)
	in.CredentialID = strings.TrimSpace(in.CredentialID)
}

// validateCreate 校验登记入参(类型 / 展示名 / Endpoint / Bucket / AccessKeyId / 凭据引用)。
func validateCreate(in CreateInput) error {
	if !ValidType(in.Type) {
		return ErrInvalidType
	}
	if in.Name == "" {
		return ErrEmptyName
	}
	if in.Endpoint == "" {
		return ErrEmptyEndpoint
	}
	if in.Bucket == "" {
		return ErrEmptyBucket
	}
	if in.AccessKeyID == "" {
		return ErrEmptyAccessKeyID
	}
	if in.CredentialID == "" {
		return ErrEmptyCredentialID
	}
	return nil
}
