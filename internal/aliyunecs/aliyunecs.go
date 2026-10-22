// Package aliyunecs 是「阿里云 ECS 批量导入」(设置→阿里云 ECS)的领域层。
//
// 一个 Account 代表一套阿里云账户接入配置(名称 + AccessKeyId + AccessKeySecret + Region;
// 多账户并存,多 Region 用多条配置表达)。凭据按机密性拆开存储(同 dnsprovider/objectstore 先例):
//   - AccessKeyId 非机密 → aliyun_ecs_accounts.access_key_id 明文列,可回显可编辑;
//   - AccessKeySecret 机密 → secret_sealed 密文 BLOB(vault.SealSecret 封存,同 pve_clusters 先例),
//     绝不入库明文/日志/响应/错误体。
//
// ECS OpenAPI 客户端为 net/http 手写实现(无 SDK,同 alidns 先例),采用阿里云官方推荐的
// V3 请求结构 + ACS3-HMAC-SHA256 签名(新服务已只认 V3;旧版 RPC V1/HMAC-SHA1 逐步下线)。
// 本期只消费 DescribeInstances(列表 + 导入前重取实例当前 IP)。
//
// 导入(Import):按账户拉取实例 → 用户手选范围 → 每批统一的 SSH 认证(选已有 vault 凭据或
// 内联新建)→ 逐台落库。默认用内网 IP 连接(可切公网)。更新匹配:aliyun_ecs_instance_links
// 记录「实例 ID ↔ servers 行」映射(首选,IP 变化仍命中)→ 按连接 IP 兜底(兼容导入前手工
// 登记过的机器行)→ 都无则新建。逐台独立容错(一台失败不 500)。
package aliyunecs

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// IP 偏好枚举(导入时连接地址取内网还是公网;默认内网)。
const (
	IPPreferencePrivate = "private"
	IPPreferencePublic  = "public"
)

// 单批导入的机器数上限(与批量命令护栏一致,防误选全量)。
const MaxImportBatch = 100

// 领域错误(人话由 httpapi 层映射状态码;错误体绝不含 ak/sk 任何片段)。
var (
	// ErrNotFound 表示阿里云账户不存在。
	ErrNotFound = errors.New("aliyunecs: account not found")
	// ErrEmptyName 表示账户展示名为空。
	ErrEmptyName = errors.New("aliyunecs: name must not be empty")
	// ErrInvalidAccessKeyID 表示 AccessKeyId 为空或格式非法。
	ErrInvalidAccessKeyID = errors.New("aliyunecs: invalid access key id")
	// ErrEmptySecret 表示创建/轮换时 AccessKeySecret 为空。
	ErrEmptySecret = errors.New("aliyunecs: secret must not be empty")
	// ErrInvalidRegion 表示 Region 格式非法。
	ErrInvalidRegion = errors.New("aliyunecs: invalid region")
	// ErrVaultUnconfigured 表示保险库未配置 master key,无法封存/解封 Secret。
	ErrVaultUnconfigured = errors.New("aliyunecs: vault unconfigured")
	// ErrNoInstances 表示导入时未选择任何实例。
	ErrNoInstances = errors.New("aliyunecs: no instances selected")
	// ErrTooManyInstances 表示单批导入实例数超上限。
	ErrTooManyInstances = errors.New("aliyunecs: too many instances in one batch")
	// ErrInvalidCredential 表示认证入参非法(未选凭据、内联 Secret 为空或类型非法)。
	ErrInvalidCredential = errors.New("aliyunecs: invalid credential input")
	// ErrInvalidIPPreference 表示 IP 偏好枚举非法。
	ErrInvalidIPPreference = errors.New("aliyunecs: invalid ip preference")
	// ErrInvalidSSHUser 表示 SSH 用户为空。
	ErrInvalidSSHUser = errors.New("aliyunecs: ssh user must not be empty")
	// ErrListFailed 表示拉取 ECS 实例列表失败(网络/凭据/权限)。
	ErrListFailed = errors.New("aliyunecs: list instances failed")
	// ErrVerifyFailed 表示账户测连失败。
	ErrVerifyFailed = errors.New("aliyunecs: account verification failed")
	// ErrPowerFailed 表示 ECS 开关机操作失败(网络/凭据/实例状态/等待超时)。
	ErrPowerFailed = errors.New("aliyunecs: ecs power operation failed")
	// ErrNoTagMatch 表示标签选择方式未匹配到任何可操作实例(0 命中即失败,防拼错静默空跑)。
	ErrNoTagMatch = errors.New("aliyunecs: tag selector matched no instances")
	// ErrServerNotLinked 表示目标服务器行未关联任何导入的 ECS 实例(不是经导入落库的机器)。
	ErrServerNotLinked = errors.New("aliyunecs: server not linked to any imported ecs instance")
)

// 机器选择方式(ecs_power 节点)与标签来源枚举。
const (
	SelectorInstances = "instances" // 直接多选实例(配置时快照)
	SelectorTags      = "tags"      // 按标签运行时解析
	TagSourceAliyun   = "aliyun"    // 阿里云 ECS 标签(云控制台打的)
	TagSourcePlatform = "platform"  // 平台服务器标签(servers.labels,构建机池同一套)
)

// 开关机动作枚举。
const (
	PowerActionStart = "start"
	PowerActionStop  = "stop"
)

// 单批开关机的实例数上限(与导入护栏一致)。
const MaxPowerBatch = 100

// regionPattern 约束 Region 形如 cn-hangzhou / ap-southeast-1(小写字母数字连字符)。
var regionPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Account 是一套阿里云账户接入配置的领域模型。绝不含 Secret 明文,只持密文 BLOB
// (进程外不可读;解封仅在构建 API 客户端时即用即弃)。
type Account struct {
	ID           string
	Name         string
	AccessKeyID  string // 非机密半段,明文存储可回显
	Region       string
	SecretSealed []byte // vault.SealSecret 产出的密文;绝不入响应/日志
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// AccountCreateInput 是登记账户的入参(Secret 含明文,仅加密前进程内存在)。
type AccountCreateInput struct {
	Name        string
	AccessKeyID string
	Secret      string
	Region      string
}

// AccountUpdateInput 是编辑账户的入参;指针字段为 nil 表示不修改(Secret 非空即轮换重封)。
type AccountUpdateInput struct {
	Name        *string
	AccessKeyID *string
	Secret      *string
	Region      *string
}

// Instance 是一台 ECS 实例的只读视图(DescribeInstances 子集,导入选型所需)。
type Instance struct {
	InstanceID   string
	Name         string
	HostName     string
	PrivateIPs   []string
	PublicIPs    []string // 公网来源合并:分配公网 IP + EIP
	OSType       string   // linux | windows
	Status       string   // Running | Stopped | ...
	InstanceType string
	RegionID     string
	Tags         map[string]string // 阿里云侧标签(云控制台打的);空 map = 无标签
}

// InlineSecret 是导入向导「内联新建凭据」的入参:整批共用一条新 vault 凭据。
// Kind 决定凭据类型:password → ssh_password;key → ssh_key(PEM 私钥)。
type InlineSecret struct {
	Kind   string // "password" | "key"
	Name   string // 凭据展示名;空 → 后端默认名
	Secret string // 明文,仅经 vault 加密前进程内存在,绝不回显
}

// ImportInput 是批量导入的入参(整批机器统一认证/端口/用户,IP 偏好一致)。
type ImportInput struct {
	AccountID    string
	InstanceIDs  []string // 手选机器范围,≤ MaxImportBatch,去重
	IPPreference string   // private(默认) | public
	// 认证二选一(每批统一):CredentialID 引用已有 vault 凭据(ssh_key/ssh_password),
	// 或 InlineSecret 由后端代建一条新凭据后同样以 ID 落库。
	CredentialID string
	InlineSecret *InlineSecret
	SSHUser      string
	SSHPort      int // <=0 时归一为 22
}

// ImportItemResult 是单台实例的导入结果(独立容错:一台失败不影响整批)。
type ImportItemResult struct {
	InstanceID string
	Name       string
	ServerID   string
	Action     string // created | updated
	OK         bool
	Err        string // 人读错误;绝不含凭据明文
}

// ImportResult 是整批导入的汇总。
type ImportResult struct {
	Items  []ImportItemResult
	Total  int
	OK     int
	Failed int
}

// ecsAPI 抽象「对该账户 Region 拉实例/测连/开关机」的能力(注入便于单测,不触真网络)。
type ecsAPI interface {
	// ListInstances 返回该 Region 全部 ECS 实例(分页聚合,防失控有页数上限)。
	ListInstances(ctx context.Context) ([]Instance, error)
	// Verify 用最小请求(PageSize=1)校验凭据与 Region 可用。
	Verify(ctx context.Context) error
	// StartInstance 开机一台(异步发起);StopInstance 关机一台(固定节省停机模式)。
	StartInstance(ctx context.Context, instanceID string) error
	StopInstance(ctx context.Context, instanceID string, forceStop bool) error
	// InstanceStatuses 批量查实例当前状态(id → Running/Stopped/...)。
	InstanceStatuses(ctx context.Context, instanceIDs []string) (map[string]string, error)
}

// Service 定义阿里云 ECS 导入领域对外接口(httpapi 消费)。
type Service interface {
	// ListAccounts 返回全部账户(创建时间倒序;无 Secret 明文)。
	ListAccounts(ctx context.Context) ([]Account, error)
	// CreateAccount 校验入参 → 封存 Secret → 落库;返回账户视图(无明文)。
	CreateAccount(ctx context.Context, in AccountCreateInput) (*Account, error)
	// UpdateAccount 编辑名称/AK/Region,Secret 非空即轮换重封;返回更新后视图。
	UpdateAccount(ctx context.Context, id string, in AccountUpdateInput) (*Account, error)
	// DeleteAccount 删除账户(连同其实例映射);不存在 → ErrNotFound。
	DeleteAccount(ctx context.Context, id string) error
	// VerifyAccount 用账户凭据对该 Region 发一次最小 DescribeInstances(测连)。
	VerifyAccount(ctx context.Context, id string) error
	// ListInstances 拉取该账户 Region 下的全部 ECS 实例(导入选型数据源)。
	ListInstances(ctx context.Context, accountID string) ([]Instance, error)
	// Import 批量导入/更新所选实例为 servers 行(逐台容错,见包注释)。
	Import(ctx context.Context, in ImportInput) (*ImportResult, error)
	// Power 对所选实例执行开机/关机(流水线 ecs_power 节点消费;关机固定节省停机模式,
	// 选择支持实例直选 / 阿里云标签 / 平台服务器标签三种方式,详见 power.go)。
	Power(ctx context.Context, in PowerInput) (*PowerResult, error)
	// PowerServer 对某登记服务器关联的 ECS 实例发起开机/关机(服务器状态页电源按钮消费):
	// 经导入映射反查实例、发起即返回不等待;无映射 → ErrServerNotLinked。详见 power.go。
	PowerServer(ctx context.Context, serverID, action string) (*PowerResult, error)
	// LinkedPowerStatus 返回全部「已导入 ECS ↔ servers 行」映射及其实时电源状态
	// (服务器状态页电源徽标数据源;逐账户独立容错)。
	LinkedPowerStatus(ctx context.Context) ([]LinkedPowerItem, error)
}

// service 是 store + vault + target 支撑的实现。newClient 供测试注入假 API 客户端。
type service struct {
	db        *sql.DB
	vault     vault.Vault
	servers   target.Service
	newClient func(ak, sk, region string) ecsAPI
}

// New 构建阿里云 ECS 导入领域服务。servers 是机器登记域(target),导入的落库/更新经它
// 完成(校验、审计语义与手工登记完全一致)。
func New(db *sql.DB, v vault.Vault, servers target.Service) Service {
	return &service{
		db:      db,
		vault:   v,
		servers: servers,
		newClient: func(ak, sk, region string) ecsAPI {
			return newEcsClient(ak, sk, region)
		},
	}
}

// clientFor 取账户 → 解封 Secret → 构建 API 客户端。Secret 明文仅在本次调用内即用即弃。
func (s *service) clientFor(ctx context.Context, id string) (ecsAPI, *Account, error) {
	acc, err := s.getAccount(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if s.vault == nil {
		return nil, nil, ErrVaultUnconfigured
	}
	secret, err := s.vault.OpenSecret(acc.SecretSealed)
	if err != nil {
		return nil, nil, ErrVaultUnconfigured
	}
	return s.newClient(acc.AccessKeyID, string(secret), acc.Region), acc, nil
}

// newID 生成账户/映射行主键(uuid v4,同 target/vault 先例)。
func newID() string { return uuid.NewString() }
