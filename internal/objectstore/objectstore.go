// Package objectstore 是「对象存储集成层」的领域层(流水线 OSS 上传节点配套)。
//
// 一条 Provider 代表一个对象存储账户接入(阿里云 OSS / S3 兼容的 RustFS;类型枚举可继续扩展多云/多账户)。
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
	"regexp"
	"strings"
	"time"
)

// 提供商类型枚举(DB 存小写字串;JSON 同名)。
const (
	// TypeAliyunOSS 是阿里云对象存储 OSS(已真实实现)。
	TypeAliyunOSS = "aliyun_oss"
	// TypeRustFS 是 RustFS(Rust 实现的 S3 兼容对象存储;经 minio-go 以 S3 API/SigV4 访问)。
	TypeRustFS = "rustfs"
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
	// ErrEmptySecret 表示自动登记入参的 Secret 明文为空。
	ErrEmptySecret = errors.New("objectstore: secret must not be empty")
	// ErrCredentialNotFound 表示引用的凭据不存在。
	ErrCredentialNotFound = errors.New("objectstore: referenced credential not found")
	// ErrVaultUnconfigured 表示保险库未配置 master key,无法取 AccessKeySecret。
	ErrVaultUnconfigured = errors.New("objectstore: vault unconfigured")
	// ErrTestFailed 表示连通性测试失败(凭据无效 / 桶不存在 / API 不可达)。错误体绝不含密钥。
	ErrTestFailed = errors.New("objectstore: connection test failed")
	// ErrUploadFailed 表示上传失败。
	ErrUploadFailed = errors.New("objectstore: upload failed")
	// ErrBrowseFailed 表示列举(浏览)失败。
	ErrBrowseFailed = errors.New("objectstore: browse failed")
	// ErrInvalidBucket 表示新建桶名非法(3-63 位小写字母/数字/连字符,两端为字母或数字)。
	ErrInvalidBucket = errors.New("objectstore: invalid bucket name")
	// ErrDuplicateBucket 表示该账户下此桶已登记为一份配置(新建桶不产生重复条目)。
	ErrDuplicateBucket = errors.New("objectstore: bucket already registered")
	// ErrBucketCreateFailed 表示服务端建桶失败(桶名被他人占用 / 凭据无建桶权限)。错误体绝不含密钥。
	ErrBucketCreateFailed = errors.New("objectstore: bucket create failed")
	// ErrListBucketsFailed 表示列举账户下桶失败(凭据无效 / API 不可达)。错误体绝不含密钥。
	ErrListBucketsFailed = errors.New("objectstore: list buckets failed")
)

// Provider 是一条对象存储接入配置的领域模型。绝不含 AccessKeySecret 明文,只持 CredentialID 引用;
// AccessKeyID 非机密,明文存储可回显。
type Provider struct {
	ID           string
	Type         string // aliyun_oss | rustfs
	Name         string
	Endpoint     string // 服务接入点(如 oss-cn-hangzhou.aliyuncs.com;不含 scheme)
	UseSSL       bool   // 是否走 HTTPS(Endpoint 不存 scheme,是否 TLS 由本字段决定)
	Bucket       string // 默认桶
	PathPrefix   string // 可选:所有上传对象键统一附加的路径前缀(存储级默认;已归一化,无首尾斜杠)
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
	UseSSL       *bool // nil = 按类型补默认(defaultUseSSL);显式传值尊重调用方
	Bucket       string
	PathPrefix   string // 可选,归一化后落库
	AccessKeyID  string
	CredentialID string
}

// UpdateInput 是编辑对象存储的入参;指针字段为 nil 表示不修改(Secret 轮换由 httpapi 层
// 直接经 vault.Update 完成,credentialId 不变,不经此入参)。
type UpdateInput struct {
	Name        *string
	Endpoint    *string
	Bucket      *string
	PathPrefix  *string // 可改可清空(传非 nil 空串 = 清空前缀)
	AccessKeyID *string
	UseSSL      *bool // 是否走 HTTPS;nil = 不修改
}

// RegisterInput 是自动登记入参(应用商店部署 RustFS 的联动链路)。与 CreateInput 的差别:
// Secret 明文直达本层,由本层经 vault 密封后落库,调用方(httpapi 部署链路)不留存。
type RegisterInput struct {
	Type        string
	Name        string
	Endpoint    string // 如 192.168.1.10:9000
	UseSSL      bool   // RustFS 部署形态为明文 host:port,调用方显式传 false
	Bucket      string
	AccessKeyID string
	Secret      string
}

// RegisterResult 是自动登记结果:配置视图 + 是否命中既有配置原地更新 + 尽力而为环节
// (建桶)的结果说明(空串 = 无异常)。
type RegisterResult struct {
	Provider *Provider
	Updated  bool
	Note     string
}

// TestResult 是连通性测试结果(Err 为 nil 表示通过;Detail 为人话,绝不含密钥)。
type TestResult struct {
	OK        bool
	LatencyMs int64
	Detail    string
	Err       error
}

// ListPageInput 是单页目录列举入参(S3 ListObjects V1 语义;delimiter 固定 "/" 折叠子目录)。
// 桶内对象数可能巨大,列举永远单页返回,绝不聚合全量。
type ListPageInput struct {
	// Prefix 是当前目录前缀(以 "/" 结尾;空串 = 根目录)。PathPrefix 由 service 层拼好再传入,
	// ObjectClient 拿到的已是最终列举前缀。
	Prefix string
	// Marker 是分页游标:返回 key 字典序严格大于它的条目(S3/OSS V1 语义,不含自身);空串 = 第一页。
	Marker string
	// MaxKeys 是本页最大返回条数(对象 + 子目录合计;调用方已钳制合法区间)。
	MaxKeys int
}

// ObjectEntry 是一个对象条目(目录占位对象已被过滤,只含当前层的真实文件)。
type ObjectEntry struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// ListPageResult 是单页列举结果。Prefixes 为子目录 common prefix(以 "/" 结尾),
// Objects 为当前层对象;两者已按字典序各自有序。NextMarker 为下一页游标,空串 = 没有更多页。
type ListPageResult struct {
	Prefixes   []string
	Objects    []ObjectEntry
	NextMarker string
}

// ObjectClient 抽象一个对象存储账户的写/校验/列举能力(注入便于单测,不触真网络)。
type ObjectClient interface {
	// VerifyBucket 校验凭据可访问目标桶(连通性 + 桶存在),失败返回人话错误(绝不含密钥)。
	VerifyBucket(ctx context.Context) error
	// PutObject 上传一个对象(objectKey 为对象键,body 为内容流)。
	PutObject(ctx context.Context, objectKey string, body io.Reader) error
	// ListPage 单页列举一个目录(桶可能对象巨多,永远只返回一页,绝不全量聚合)。
	ListPage(ctx context.Context, in ListPageInput) (ListPageResult, error)
	// ListBuckets 列举该账户下的桶(候选列表;实现可分页拉全,截断在 service 层统一处理)。
	ListBuckets(ctx context.Context) ([]BucketEntry, error)
}

// BucketEntry 是账户下一个桶的候选条目(设置页 Bucket 字段的下拉建议来源)。
type BucketEntry struct {
	Name      string
	CreatedAt time.Time
}

// bucketEnsurer 是 ObjectClient 的可选能力:支持自动建桶的实现(自建 S3 兼容存储如 RustFS;
// 阿里云 OSS 客户端不实现)。自动登记链路(Register)据此决定是否在建配置前确保默认桶存在。
type bucketEnsurer interface {
	EnsureBucket(ctx context.Context) error
}

// bucketCreator 是 ObjectClient 的可选能力:显式新建一个桶(浏览页「新建桶」,用户主动
// 发起的动作;区别于自动登记链路的 bucketEnsurer 幂等建桶)。阿里云 OSS 与 RustFS 均实现。
type bucketCreator interface {
	CreateBucket(ctx context.Context, bucketName string) error
}

// bucketNameRe 是新建桶名的合法规则:S3/OSS 交集 —— 3-63 位小写字母/数字/连字符,两端为
// 字母或数字。不含点号(点会破坏 OSS 泛域名证书校验,也规避 IP 形态桶名)。
var bucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

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

	// Register 自动登记一份对象存储配置(应用商店部署 RustFS 联动):尽力而为确保默认桶存在,
	// Secret 明文经 vault 密封。同 type+endpoint 已有配置 → 原地轮换凭据与接入参数(Updated=true),
	// 不产生重复条目(重新部署会重新生成凭据,旧凭据必须跟着换)。
	Register(ctx context.Context, in RegisterInput) (*RegisterResult, error)

	// Upload 取该配置凭据 → 上传一段字节流到 objectKey(流水线 oss_upload 节点用)。
	// objectKey 前会自动附加配置的 PathPrefix(存储级默认前缀;节点级前缀拼在其后)。
	// Secret 用完即弃,绝不外泄。
	Upload(ctx context.Context, providerID, objectKey string, body io.Reader) error

	// Browse 取该配置凭据 → 单页列举 prefix 目录(delimiter="/",marker 游标分页)。
	// prefix 相对 PathPrefix(浏览根锚定在配置前缀之下,与 Upload 写入位置一致);空 = 前缀根。
	// 桶内对象可能巨多:永远单页返回(MaxKeys 钳制),绝不聚合全量。Secret 用完即弃,绝不外泄。
	Browse(ctx context.Context, id, prefix, marker string, maxKeys int) (*ListPageResult, error)

	// CreateBucket 在 sourceID 配置的账户下新建 bucket,并登记为一份新配置(浏览页「新建桶」)。
	// 新配置复用同账户 Type/Endpoint/AccessKeyId,凭据经 vault 独立重封(删除级联互不影响);
	// 展示名 name 为空时取桶名。桶名非法 → ErrInvalidBucket;同账户同桶已登记 → ErrDuplicateBucket;
	// 服务端建桶失败(名被他人占用/无权限) → ErrBucketCreateFailed。Secret 用完即弃,绝不外泄。
	CreateBucket(ctx context.Context, sourceID, bucket, name string) (*Provider, error)

	// ListBuckets 取该配置凭据 → 列举账户下的桶(按名字排序,上限 maxBucketCandidates 条,
	// 超出置 Truncated=true)。给设置页 Bucket 字段提供候选下拉。Secret 用完即弃,绝不外泄。
	ListBuckets(ctx context.Context, id string) (*BucketListResult, error)

	// ProbeListBuckets 用未落库的明文入参直连存储列举桶(添加弹窗配置尚未创建、无 id 可用)。
	// 不落库、不写 vault、不审计;Secret 用完即弃,绝不外泄。入参非法 → 与 Create 同族校验错误。
	ProbeListBuckets(ctx context.Context, in ProbeInput) (*BucketListResult, error)
}

// ProbeInput 是未落库的直连探测入参(添加弹窗「获取桶列表」)。凭据仅为内存态,任何路径不持久化。
type ProbeInput struct {
	Type        string
	Endpoint    string
	UseSSL      bool
	AccessKeyID string
	Secret      string
}

// BucketListResult 是账户下桶的候选列表(按名字排序;Truncated=true 表示超出上限被截断)。
type BucketListResult struct {
	Buckets    []BucketEntry
	Truncated  bool
}

// maxBucketCandidates 是桶候选列表的硬上限(防巨账户撑爆响应;按名字排序后取前 N)。
const maxBucketCandidates = 500

// defaultUseSSL 返回类型的默认传输安全策略:公云 OSS 必走 HTTPS(明文上传 AK 签名的对象
// 属安全隐患);RustFS 自建内网部署默认明文(可手动开启)。
func defaultUseSSL(providerType string) bool {
	return providerType == TypeAliyunOSS
}

// ValidType 报告提供商类型是否为受支持枚举。
func ValidType(t string) bool {
	switch t {
	case TypeAliyunOSS, TypeRustFS:
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

// NormalizePathPrefix 归一化路径前缀:去空白、去首尾斜杠(「/a/b/」与「a/b」等价,
// 拼接时统一由本包补分隔符)。归一化结果为空 = 不附加前缀。
func NormalizePathPrefix(s string) string {
	return strings.Trim(strings.TrimSpace(s), "/")
}

// normalizeCreate 原地归一化登记入参(去空白 / 类型小写 / Endpoint 去 scheme / 前缀去首尾斜杠 /
// UseSSL 未显式指定时按类型补默认)。
func normalizeCreate(in *CreateInput) {
	in.Type = strings.ToLower(strings.TrimSpace(in.Type))
	in.Name = strings.TrimSpace(in.Name)
	in.Endpoint = normalizeEndpoint(in.Endpoint)
	if in.UseSSL == nil {
		v := defaultUseSSL(in.Type)
		in.UseSSL = &v
	}
	in.Bucket = strings.TrimSpace(in.Bucket)
	in.PathPrefix = NormalizePathPrefix(in.PathPrefix)
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
