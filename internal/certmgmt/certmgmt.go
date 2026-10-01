// Package certmgmt 是「证书管理」的领域层:统一管理平台全部 TLS 证书的生命周期 ——
// acme.sh 自动签发(DNS-01)/ 手动导入,到期监控 + 平台后台调度自动续期,并同步下发到
// 服务注册网关(nginx)供其 443 server 块使用。
//
// 与既有体系的关系:
//   - 取代 internal/proxy(Caddy)内置的 ACME 签发:Caddy 反代整体下线,签发能力收拢到本包。
//   - 补齐 internal/servicereg 的证书短板:网关本身不做 ACME,基域证书改由本包签发后经
//     CertSink(servicereg.UploadCert)同步,nginx.conf 渲染与 reload 复用既有收敛管道。
//
// acme.sh 运行形态:控制机本地脚本(无容器/无镜像链/不依赖网关配置)—— fork 最小集
// go:embed 进平台二进制,首次使用时释放到本地 acme home(DB 同级 acme/)直接运行;
// 平台是唯一续期驱动(单一控制面,无 cron)。证书由平台读回、密文入库,再经 CertSink
// 下发网关(网关未配置不影响签发)。详见 acmesh.go 头注释。
//
// 设计纪律(与 proxy/servicereg 一致):
//   - acme.sh 经 os/exec 以参数数组在控制机本地调用(AC-SEC-02 不拼 shell);域名/CA/
//     密钥类型均已领域层白名单校验,DNS 凭据经进程环境变量传递 ——
//     凭据绝不进命令行/日志/回库/回 API。
//   - 证书 PEM 经 vault SealSecret 加密存 BLOB(与 servicereg 同手法),DB 为权威副本,
//     网关卷丢失也能重新下发。
//   - 签发/续期异步执行(ACME + DNS 传播耗时不可控),状态机 pending → issued|failed,
//     status_detail 永远给人话。
package certmgmt

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/target"
)

// source / validation / status 枚举(DB 存小写字串)。
const (
	// SourceACME 表示由平台集成的 acme.sh 签发(可续期)。
	SourceACME = "acme"
	// SourceManual 表示用户导入的现成证书(不可续期,可重新下发)。
	SourceManual = "manual"

	// ValidationDNS 表示 DNS-01 挑战(需绑 DNS 提供商;支持泛域名)。
	ValidationDNS = "dns"
	// ValidationManual 表示导入(无 ACME 挑战)。
	ValidationManual = "manual"

	// StatusPending 表示签发/续期/下发进行中。
	StatusPending = "pending"
	// StatusIssued 表示证书就绪(密文可用)。
	StatusIssued = "issued"
	// StatusFailed 表示最近一次签发/续期失败(status_detail 有人话原因)。
	StatusFailed = "failed"
)

// CA 枚举(创建时可选;渲染为 acme.sh --server 的完整目录 URL,常量无注入面)。
const (
	// CALetsEncrypt 是 Let's Encrypt(默认)。
	CALetsEncrypt = "letsencrypt"
	// CAZeroSSL 是 ZeroSSL。
	CAZeroSSL = "zerossl"
	// CABuyPass 是 BuyPass。
	CABuyPass = "buypass"
)

// caServerURL 把 CA 枚举映射为 ACME 目录 URL(acme.sh --server 直接吃完整 URL,最稳)。
var caServerURL = map[string]string{
	CALetsEncrypt: "https://acme-v02.api.letsencrypt.org/directory",
	CAZeroSSL:     "https://acme.zerossl.com/v2/DV90",
	CABuyPass:     "https://api.buypass.com/acme/directory",
}

// 密钥类型枚举(acme.sh --keylength 直通)。
const (
	KeyTypeEC256   = "ec-256"
	KeyTypeEC384   = "ec-384"
	KeyTypeRSA2048 = "rsa-2048"
)

// 领域错误(httpapi 层映射状态码;错误体绝不含敏感信息)。
var (
	// ErrNotFound 表示证书不存在。
	ErrNotFound = errors.New("certmgmt: certificate not found")
	// ErrDomainTaken 表示该主域名已有证书(primary_domain 唯一)。
	ErrDomainTaken = errors.New("certmgmt: primary domain already has a certificate")
	// ErrInvalidDomain 表示主域名格式非法(非 FQDN / 非法通配符)。
	ErrInvalidDomain = errors.New("certmgmt: invalid domain")
	// ErrNoDNSProvider 表示 ACME 签发必须绑定 DNS 提供商(仅支持 DNS-01)。
	ErrNoDNSProvider = errors.New("certmgmt: ACME 签发必须绑定 DNS 提供商(DNS-01)")
	// ErrInvalidDNSProvider 表示引用的 DNS 提供商不存在 / 类型非法 / 凭据不可用。
	ErrInvalidDNSProvider = errors.New("certmgmt: invalid dns provider reference")
	// ErrZoneUncovered 表示某域名不在所绑 DNS 提供商托管的任何根区下。
	ErrZoneUncovered = errors.New("certmgmt: domain not covered by any zone of the bound dns provider")
	// ErrInvalidCA 表示 CA 枚举非法。
	ErrInvalidCA = errors.New("certmgmt: invalid ca")
	// ErrInvalidKeyType 表示密钥类型枚举非法。
	ErrInvalidKeyType = errors.New("certmgmt: invalid key type")
	// ErrTooManyDomains 表示 SAN 数超上限。
	ErrTooManyDomains = errors.New("certmgmt: too many domains")
	// ErrInvalidCert 表示导入的 PEM 非法或证书/私钥不配对。
	ErrInvalidCert = errors.New("certmgmt: invalid certificate or key")
	// ErrBusy 表示该证书已有签发/续期在进行中。
	ErrBusy = errors.New("certmgmt: operation already in progress")
	// ErrCertInUse 表示证书正被平台 HTTPS 使用(删除前须先在设置中更换或关闭)。
	ErrCertInUse = errors.New("certmgmt: certificate is in use")
	// ErrAcmeshStart 表示 acme.sh 脚本集在控制机本地的安装/依赖校验失败。
	ErrAcmeshStart = errors.New("certmgmt: 安装 acme.sh 签发引擎失败")
	// ErrIssue 表示 acme.sh 签发/续期执行失败(附 stderr 人话摘要)。
	ErrIssue = errors.New("certmgmt: acme.sh 执行失败")
	// ErrReadBack 表示签发成功但证书文件读回/校验失败。
	ErrReadBack = errors.New("certmgmt: 证书文件读取失败")
)

// maxDomains 是单张证书 SAN 上限(含主域)。
const maxDomains = 32

// maxPEMBytes 是单段 PEM 的大小上限(与 servicereg 一致)。
const maxPEMBytes = 64 * 1024

// opTimeout 是单次签发/续期的总超时(ACME + DNS 传播等待)。
const opTimeout = 10 * time.Minute

// Certificate 是一张证书的领域模型(冻结契约;JSON 形状在 httpapi DTO)。
type Certificate struct {
	ID            string
	PrimaryDomain string    // acme.sh 的证书主键(-d 第一域);导入证书 = 首个 SAN
	Domains       []string  // 全量 SAN(含主域,小写去重)
	Source        string    // acme | manual
	CA            string    // letsencrypt | zerossl | buypass(manual 为空)
	Validation    string    // dns | manual
	DNSProviderID string    // DNS-01 凭据来源(acme 必填)
	KeyType       string    // ec-256 | ec-384 | rsa-2048
	AutoRenew     bool      // 平台 Sweeper 是否自动续期(仅 acme 有意义)
	Status        string    // pending | issued | failed
	StatusDetail  string    // 人话:进行中说明 / 失败原因 / 下发提示
	HasPEM        bool      // 密文对是否就绪(issued 恒 true)
	Subject       string    // 叶子 Subject 摘要
	Issuer        string    // 签发机构 CN
	NotBefore     time.Time // 叶子有效期起
	NotAfter      time.Time // 叶子有效期止(到期监控/续期窗口依据)
	LastIssuedAt  time.Time // 最近一次签发/续期成功时间
	LastAttemptAt time.Time // 最近一次尝试时间(Sweeper 失败退避依据)
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// EngineStatus 是签发引擎(控制机本地的 acme.sh 脚本集)的探测快照。
type EngineStatus struct {
	Installed bool   // acme.sh 脚本集是否已释放到本地 home
	Ready     bool   // 运行期依赖(sh + curl + openssl)是否就绪
	Version   string // acme.sh 版本串(best-effort,如 v3.1.1)
}

// CreateInput 是创建 ACME 证书的入参。
type CreateInput struct {
	PrimaryDomain string   // 主域名(FQDN 或 *.FQDN)
	Domains       []string // 附加 SAN(可含通配符;自动并入主域、去重)
	DNSProviderID string   // 必填:DNS-01 凭据来源
	CA            string   // letsencrypt(默认)| zerossl | buypass
	KeyType       string   // ec-256(默认)| ec-384 | rsa-2048
	AutoRenew     bool     // 默认开
}

// ImportInput 是手动导入证书的入参(PEM 全文;cert/key 须配对)。
type ImportInput struct {
	CertPEM string
	KeyPEM  string
}

// CredentialsResolver 抽象「按 DNS 提供商 id 取 (类型, API ID, Secret 明文) + 根区清单」
// (与既有 proxy.DNSResolver 同形;由 main.go 用 dnsprovider 适配注入,避免包环)。
// Secret 仅在生成一次性签发脚本时进程内使用,绝不日志/回库/回 API。
type CredentialsResolver interface {
	// Resolve 返回 providerType(cloudflare|dnspod|alidns)与凭据 (API ID, Secret 明文)。
	// 提供商不存在 → ok=false。
	Resolve(ctx context.Context, providerID string) (providerType, apiID, secret string, ok bool, err error)
	// ProviderType 仅返回类型(校验引用存在/合法,不取凭据)。
	ProviderType(ctx context.Context, providerID string) (providerType string, ok bool, err error)
	// ProviderZones 返回该提供商托管的全部根域(域名覆盖校验用)。
	ProviderZones(ctx context.Context, providerID string) (baseDomains []string, ok bool, err error)
}

// BaseDomain 是 CertSink 暴露的网关基域引用。
type BaseDomain struct {
	ID         string
	BaseDomain string
}

// CertSink 抽象「把证书同步进服务注册网关」的能力(由 main.go 适配 servicereg.Service 注入;
// UploadCert 内部会落库 + 全量收敛 nginx + reload)。PEM 仅进程内传递,绝不过 HTTP。
type CertSink interface {
	// ListBaseDomains 返回网关全部已注册基域(判定证书 SAN 覆盖关系)。
	ListBaseDomains(ctx context.Context) ([]BaseDomain, error)
	// UploadCert 把证书写入某基域(落库 + apply 网关)。
	UploadCert(ctx context.Context, domainID, certPEM, keyPEM string) error
	// CurrentCertPEM 返回某基域当前证书 PEM 明文(删除证书时比对归属;无证书 ok=false)。
	CurrentCertPEM(ctx context.Context, domainID string) (certPEM string, ok bool, err error)
	// ClearCert 清空某基域证书(回退 HTTP-only 并 apply)。
	ClearCert(ctx context.Context, domainID string) error
}

// SecretSealer 抽象 vault 的密封能力(与 servicereg.SecretSealer 同形)。
type SecretSealer interface {
	SealSecret(plaintext []byte) ([]byte, error)
	OpenSecret(sealed []byte) ([]byte, error)
}

// PlatformHTTPS 抽象「平台 HTTPS(宿主 nginx)的证书联动」能力(由 main.go 适配
// platformhttps.Service 注入):续期/重下发后联动重新落盘证书 + reload;删除证书前拦截占用。
type PlatformHTTPS interface {
	// UsesCert 报告平台 HTTPS 是否正在使用该证书。
	UsesCert(ctx context.Context, certID string) (bool, error)
	// RedeployCert 证书更新后联动重下发(未引用该证书时 no-op)。
	RedeployCert(ctx context.Context, certID string) error
}

// Service 定义证书管理领域对外接口(冻结契约;httpapi 消费)。
type Service interface {
	// List 返回全部证书(创建时间倒序)。
	List(ctx context.Context) ([]Certificate, error)
	// Get 读取单张证书;不存在 → ErrNotFound。
	Get(ctx context.Context, id string) (*Certificate, error)
	// Create 校验入参 → 落库(pending)→ 异步签发,立即返回。
	Create(ctx context.Context, in CreateInput) (*Certificate, error)
	// Import 校验并解析 PEM → 密文落库(issued)→ 同步网关(失败不丢证书,记入 detail)。
	Import(ctx context.Context, in ImportInput) (*Certificate, error)
	// Renew 立即续期(acme:--force 重签;manual:按密文重新下发网关),异步执行,立即返回。
	Renew(ctx context.Context, id string) (*Certificate, error)
	// Delete 删除证书(acme 先 acme.sh --remove 停续期;正被某基域使用(内容一致)则清空该基域)。
	Delete(ctx context.Context, id string) error
	// SetAutoRenew 开/关自动续期。
	SetAutoRenew(ctx context.Context, id string, on bool) (*Certificate, error)
	// EngineStatus 探测本地 acme.sh 引擎(脚本就位/依赖就绪/版本;纯本地探测)。
	EngineStatus(ctx context.Context) (*EngineStatus, error)
	// DeployEngine 显式安装本地签发引擎脚本集(无需先建证书)。
	DeployEngine(ctx context.Context) (*EngineStatus, error)
	// OpenCertPEM 进程内解密返回证书/私钥 PEM 明文(供平台 HTTPS 下发到宿主 nginx;
	// PEM 仅进程内传递,绝不过 HTTP)。证书不存在 → ErrNotFound;密文缺失/解密失败 → 人读错误。
	OpenCertPEM(ctx context.Context, id string) (certPEM, keyPEM string, err error)
	// SweepOnce 到期自动续期一轮(Sweeper 调度入口;返回本轮触发的续期数)。
	SweepOnce(ctx context.Context) (int, error)
	// BackfillFromServiceReg 启动期一次性迁移:把 service_reg_domains 既有手动证书幂等回填进
	// certificates(老数据在证书管理页可见)。返回回填条数。
	BackfillFromServiceReg(ctx context.Context) (int, error)
}

// service 是 store + target + vault (+ resolver/gateway/sink/phttps 注入) 支撑的 Service 实现。
type service struct {
	store  *Store
	tg     target.Service
	vault  SecretSealer
	dns    CredentialsResolver
	sink   CertSink
	home   string // 本地 acme home(空 → homeDir() 兜底:env / "acme")
	phttps PlatformHTTPS

	mu       sync.Mutex
	inflight map[string]struct{} // 正在签发/续期的证书 id(防重入)
}

// New 构造 Service。resolver/gateway/sink 经 setter 晚绑(main.go 装配时注入适配器,
// 避免 import 环;nil 时对应能力优雅降级:ACME 创建报错、手动导入/列表照常)。
func New(db *sql.DB, tg target.Service, vault SecretSealer) Service {
	return &service{
		store:    NewStore(db),
		tg:       tg,
		vault:    vault,
		inflight: map[string]struct{}{},
	}
}

// SetCredentialsResolver 注入 DNS 提供商凭据解析器。
func (s *service) SetCredentialsResolver(r CredentialsResolver) { s.dns = r }

// SetHomeDir 注入本地 acme home 目录(生产装配传 DB 同级 acme/;不传则按 env/默认兜底)。
func (s *service) SetHomeDir(dir string) {
	if v := strings.TrimSpace(dir); v != "" {
		s.home = v
	}
}

// SetCertSink 注入网关证书同步器。
func (s *service) SetCertSink(cs CertSink) { s.sink = cs }

// SetPlatformHTTPS 注入平台 HTTPS(宿主 nginx)联动器(nil = 联动能力降级,证书本体不受影响)。
func (s *service) SetPlatformHTTPS(p PlatformHTTPS) { s.phttps = p }

func (s *service) logf(format string, args ...any) {
	log.Printf(format, args...)
}

// ---------- 查询 ----------

func (s *service) List(ctx context.Context) ([]Certificate, error) { return s.store.list(ctx) }

func (s *service) Get(ctx context.Context, id string) (*Certificate, error) {
	return s.store.get(ctx, id)
}

// ---------- 创建(ACME) ----------

func (s *service) Create(ctx context.Context, in CreateInput) (*Certificate, error) {
	in = normalizeCreate(in)
	if err := s.validateCreate(ctx, in); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	c := &Certificate{
		ID:            newID(),
		PrimaryDomain: in.PrimaryDomain,
		Domains:       append([]string{in.PrimaryDomain}, in.Domains...),
		Source:        SourceACME,
		CA:            in.CA,
		Validation:    ValidationDNS,
		DNSProviderID: in.DNSProviderID,
		KeyType:       in.KeyType,
		AutoRenew:     in.AutoRenew,
		Status:        StatusPending,
		StatusDetail:  "已排队,等待签发…",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.store.insert(ctx, c); err != nil {
		return nil, err
	}
	s.runAsync(c.ID, "签发中…", func(ctx context.Context) error { return s.issueCert(ctx, c.ID) })
	return s.store.get(ctx, c.ID)
}

// ---------- 导入 ----------

func (s *service) Import(ctx context.Context, in ImportInput) (*Certificate, error) {
	if s.vault == nil {
		return nil, target.ErrVaultUnconfigured
	}
	certPEM := strings.TrimSpace(in.CertPEM)
	keyPEM := strings.TrimSpace(in.KeyPEM)
	if certPEM == "" || keyPEM == "" || len(certPEM) > maxPEMBytes || len(keyPEM) > maxPEMBytes {
		return nil, ErrInvalidCert
	}
	leaf := validatePEMPair(certPEM, keyPEM)
	if leaf == nil {
		return nil, ErrInvalidCert
	}
	meta := leafMeta(leaf)
	// 主域 = 首个 SAN(无 SAN 的极端老证书回退 CN);domains = SAN 全集。
	primary := strings.TrimSpace(leaf.Subject.CommonName)
	if len(leaf.DNSNames) > 0 {
		primary = leaf.DNSNames[0]
	}
	primary = strings.ToLower(primary)
	domains := lowerUnique(append([]string{primary}, leaf.DNSNames...))
	if primary == "" || len(domains) == 0 || len(domains) > maxDomains {
		return nil, ErrInvalidCert
	}
	now := time.Now().UTC()
	c := &Certificate{
		ID:            newID(),
		PrimaryDomain: primary,
		Domains:       domains,
		Source:        SourceManual,
		Validation:    ValidationManual,
		Status:        StatusIssued,
		Subject:       meta.subject,
		Issuer:        meta.issuer,
		NotBefore:     meta.notBefore,
		NotAfter:      meta.notAfter,
		LastIssuedAt:  now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	sealedCert, err := s.vault.SealSecret([]byte(certPEM))
	if err != nil {
		return nil, err
	}
	sealedKey, err := s.vault.SealSecret([]byte(keyPEM))
	if err != nil {
		return nil, err
	}
	if err := s.store.insertWithPEM(ctx, c, sealedCert, sealedKey); err != nil {
		return nil, err
	}
	// 同步网关 best-effort:证书本体已落库,失败只记提示,绝不丢证书。
	if hint := s.syncGateway(ctx, c.ID); hint != "" {
		_ = s.store.setStatusDetail(ctx, c.ID, hint)
	}
	// 平台 HTTPS(宿主 nginx)联动下发,同样 best-effort。
	if hint := s.syncPlatformHTTPS(ctx, c.ID); hint != "" {
		_ = s.store.appendStatusDetail(ctx, c.ID, hint)
	}
	return s.store.get(ctx, c.ID)
}

// ---------- 续期 / 重新下发 ----------

// Renew 立即触发:acme 证书 → --force 重签;manual 证书 → 按库内密文重新下发网关(同步执行)。
func (s *service) Renew(ctx context.Context, id string) (*Certificate, error) {
	c, err := s.store.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.Source != SourceACME {
		if hint := s.syncGateway(ctx, id); hint != "" {
			_ = s.store.setStatusDetail(ctx, id, hint)
			return nil, fmt.Errorf("%w:%s", ErrIssue, hint)
		}
		if hint := s.syncPlatformHTTPS(ctx, id); hint != "" {
			_ = s.store.appendStatusDetail(ctx, id, hint)
			return nil, fmt.Errorf("%w:%s", ErrIssue, hint)
		}
		_ = s.store.setStatusDetail(ctx, id, "已重新下发网关")
		return s.store.get(ctx, id)
	}
	if c.DNSProviderID == "" || s.dns == nil {
		return nil, ErrNoDNSProvider
	}
	// runAsync 内 inflightAdd 原子占位:占位失败 = 已有签发/续期在进行中 → ErrBusy。
	if !s.runAsync(id, "续期中…", func(ctx context.Context) error {
		return s.issueCert(ctx, id) // issue/renew 同路径;renewOp 由「是否签发过」推导
	}) {
		return nil, ErrBusy
	}
	return s.store.get(ctx, id)
}

// ---------- 删除 / 自动续期开关 ----------

func (s *service) Delete(ctx context.Context, id string) error {
	c, err := s.store.get(ctx, id)
	if err != nil {
		return err
	}
	if s.inflightHas(id) {
		return ErrBusy
	}
	// 平台 HTTPS 正在使用该证书 → 拦截删除(避免平台自身 HTTPS 访问直接中断)。
	if s.phttps != nil {
		if used, uerr := s.phttps.UsesCert(ctx, id); uerr == nil && used {
			return fmt.Errorf("%w:平台 HTTPS 正在使用该证书,请先在「设置 → HTTPS 访问」中更换或关闭", ErrCertInUse)
		}
	}
	// acme 证书:best-effort 从 acme.sh 续期清单移除(不向 CA revoke,避免误删不可逆)。
	if c.Source == SourceACME && c.Status == StatusIssued {
		s.removeFromAcmesh(c)
	}
	// 正被某基域使用(基域当前证书内容 == 本证书)→ 清空该基域并回退 HTTP-only。
	s.clearOwningDomains(ctx, c)
	return s.store.delete(ctx, id)
}

func (s *service) SetAutoRenew(ctx context.Context, id string, on bool) (*Certificate, error) {
	if _, err := s.store.get(ctx, id); err != nil {
		return nil, err
	}
	if err := s.store.setAutoRenew(ctx, id, on); err != nil {
		return nil, err
	}
	return s.store.get(ctx, id)
}

// ---------- 引擎 ----------

func (s *service) EngineStatus(ctx context.Context) (*EngineStatus, error) {
	return s.inspectEngine()
}

func (s *service) DeployEngine(ctx context.Context) (*EngineStatus, error) {
	if err := s.ensureEngine(); err != nil {
		return nil, err
	}
	return s.inspectEngine()
}

// OpenCertPEM 进程内解密返回证书/私钥 PEM(平台 HTTPS 下发到宿主 nginx 用;绝不过 HTTP)。
func (s *service) OpenCertPEM(ctx context.Context, id string) (string, string, error) {
	if s.vault == nil {
		return "", "", target.ErrVaultUnconfigured
	}
	sealedCert, sealedKey, ok, err := s.store.getSealed(ctx, id)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", ErrNotFound
	}
	certPEM, err := s.vault.OpenSecret(sealedCert)
	if err != nil {
		return "", "", fmt.Errorf("%w:证书解密失败", ErrReadBack)
	}
	keyPEM, err := s.vault.OpenSecret(sealedKey)
	if err != nil {
		return "", "", fmt.Errorf("%w:私钥解密失败", ErrReadBack)
	}
	return string(certPEM), string(keyPEM), nil
}

// ---------- 异步执行骨架 ----------

// runAsync 是签发/续期的异步壳:同步占位 in-flight 锁(占位成功才异步执行),结果(人话)回写状态机。
// 返回 false = 该证书已有操作进行中(调用方据此报 ErrBusy / 跳过),不再起 goroutine。
func (s *service) runAsync(id, phase string, fn func(ctx context.Context) error) bool {
	if !s.inflightAdd(id) {
		return false
	}
	go func() {
		defer s.inflightRemove(id)
		_ = s.store.markAttempt(context.Background(), id, phase)
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		if err := fn(ctx); err != nil {
			detail := truncate(err.Error(), 2000)
			_ = s.store.markFailed(context.Background(), id, detail)
			s.logf("[certmgmt] 证书 %s 失败:%s", id, detail)
		}
	}()
	return true
}

// issueCert 是签发/续期的主编排:凭据 → 引擎 → acme.sh → 读回 → 落库 → 同步网关。
func (s *service) issueCert(ctx context.Context, id string) error {
	c, err := s.store.get(ctx, id)
	if err != nil {
		return err
	}
	if s.vault == nil {
		return target.ErrVaultUnconfigured
	}
	pt, apiID, secret, ok, err := s.dns.Resolve(ctx, c.DNSProviderID)
	if err != nil {
		return err
	}
	if !ok || secret == "" {
		return fmt.Errorf("%w:DNS 提供商不可用或凭据缺失", ErrInvalidDNSProvider)
	}

	// 安装/校验本地引擎(嵌入脚本缺失则释放,依赖缺失报人话错误)。
	if err := s.ensureEngine(); err != nil {
		return err
	}

	// 签发/续期同路径:凭据经进程环境变量传递,绝不进命令行/日志。
	if err := s.runIssue(ctx, c, dnsAPIName(pt), dnsEnvFor(pt, apiID, secret)); err != nil {
		return err
	}
	certPEM, keyPEM, err := s.readBackCert(c.PrimaryDomain)
	if err != nil {
		return err
	}
	leaf := validatePEMPair(certPEM, keyPEM)
	if leaf == nil {
		return fmt.Errorf("%w:acme.sh 产物校验失败(证书/私钥不配对)", ErrReadBack)
	}
	meta := leafMeta(leaf)
	sealedCert, err := s.vault.SealSecret([]byte(certPEM))
	if err != nil {
		return err
	}
	sealedKey, err := s.vault.SealSecret([]byte(keyPEM))
	if err != nil {
		return err
	}
	detail := "签发成功"
	if !c.LastIssuedAt.IsZero() {
		detail = "续期成功"
	}
	if err := s.store.markIssued(ctx, id, sealedCert, sealedKey, meta, detail); err != nil {
		return err
	}
	// 同步网关 best-effort:失败不回滚签发结果,记入 detail 供 UI 提示。
	if hint := s.syncGateway(ctx, id); hint != "" {
		_ = s.store.appendStatusDetail(ctx, id, hint)
	}
	// 平台 HTTPS(宿主 nginx)联动下发,同样 best-effort。
	if hint := s.syncPlatformHTTPS(ctx, id); hint != "" {
		_ = s.store.appendStatusDetail(ctx, id, hint)
	}
	return nil
}

// syncGateway 把证书下发给所有被覆盖的网关基域(解密 → sink.UploadCert,其内部 apply+reload)。
// 返回非空串 = 有人话提示(未下发/部分失败);空串 = 全部成功或无可同步目标。
func (s *service) syncGateway(ctx context.Context, id string) string {
	if s.vault == nil {
		return "下发提示:凭据保险库未配置,证书已入库但未下发"
	}
	c, err := s.store.get(ctx, id)
	if err != nil || !c.HasPEM {
		return ""
	}
	if s.sink == nil {
		return "下发提示:网关未接入,证书已入库但未下发"
	}
	sealedCert, sealedKey, ok, err := s.store.getSealed(ctx, id)
	if err != nil || !ok {
		return "下发提示:证书密文缺失,未下发"
	}
	certPEM, err := s.vault.OpenSecret(sealedCert)
	if err != nil {
		return "下发提示:证书解密失败,未下发"
	}
	keyPEM, err := s.vault.OpenSecret(sealedKey)
	if err != nil {
		return "下发提示:私钥解密失败,未下发"
	}
	bases, err := s.coveredBaseDomains(ctx, c.Domains)
	if err != nil || len(bases) == 0 {
		return "" // 没有被覆盖的基域:无事可做,不算失败
	}
	var failed []string
	for _, b := range bases {
		if uerr := s.sink.UploadCert(ctx, b.ID, string(certPEM), string(keyPEM)); uerr != nil {
			failed = append(failed, b.BaseDomain+"("+truncate(uerr.Error(), 120)+")")
		}
	}
	if len(failed) > 0 {
		return "下发提示:部分基域下发失败 → " + strings.Join(failed, "、")
	}
	return ""
}

// syncPlatformHTTPS 把证书联动下发给平台 HTTPS(宿主 nginx):未接入/未引用该证书时 no-op。
// 返回非空串 = 人话提示(重下发失败)。
func (s *service) syncPlatformHTTPS(ctx context.Context, id string) string {
	if s.phttps == nil {
		return ""
	}
	if err := s.phttps.RedeployCert(ctx, id); err != nil {
		return "下发提示:平台 HTTPS 证书同步失败 → " + truncate(err.Error(), 300)
	}
	return ""
}

// coveredBaseDomains 返回证书 SAN 覆盖的网关基域(基域 == 某 SAN,或被某通配符 SAN 覆盖;
// 一张含 *.efg.com + efg.com 的证书即覆盖基域 efg.com)。
func (s *service) coveredBaseDomains(ctx context.Context, domains []string) ([]BaseDomain, error) {
	if s.sink == nil {
		return nil, nil
	}
	bases, err := s.sink.ListBaseDomains(ctx)
	if err != nil {
		return nil, err
	}
	var out []BaseDomain
	for _, b := range bases {
		if domainCoversAny(domains, b.BaseDomain) {
			out = append(out, b)
		}
	}
	return out, nil
}

// domainCoversAny 报告证书 SAN 集是否覆盖 base:某 SAN 精确等于 base,或某通配符 SAN 的
// 基域等于 base(nginx 按基域共用一张证书,SAN a.efg.com 这种部分覆盖不认,宁缺毋滥)。
func domainCoversAny(sans []string, base string) bool {
	for _, san := range sans {
		if san == base {
			return true
		}
		if strings.HasPrefix(san, "*.") && strings.TrimPrefix(san, "*.") == base {
			return true
		}
	}
	return false
}

// clearOwningDomains 删除证书时,清空所有「当前证书内容 == 本证书」的基域(best-effort)。
func (s *service) clearOwningDomains(ctx context.Context, c *Certificate) {
	if s.vault == nil || s.sink == nil || !c.HasPEM {
		return
	}
	sealedCert, _, ok, err := s.store.getSealed(ctx, c.ID)
	if err != nil || !ok {
		return
	}
	own, uerr := s.vault.OpenSecret(sealedCert)
	if uerr != nil {
		return
	}
	bases, err := s.coveredBaseDomains(ctx, c.Domains)
	if err != nil {
		return
	}
	for _, b := range bases {
		cur, has, err := s.sink.CurrentCertPEM(ctx, b.ID)
		if err != nil || !has || cur != string(own) {
			continue // 基域证书已被用户另行替换:不动
		}
		if cerr := s.sink.ClearCert(ctx, b.ID); cerr != nil {
			s.logf("[certmgmt] 删除证书 %s:清空基域 %s 失败:%v", c.ID, b.BaseDomain, cerr)
		}
	}
}

// ---------- SweepOnce(到期自动续期) ----------

// sweepRenewWindow 是自动续期窗口(到期前 N 天);sweepRetryAfter 是失败退避(距上次尝试)。
const (
	sweepRenewWindow = 30 * 24 * time.Hour
	sweepRetryAfter  = 24 * time.Hour
)

// SweepOnce 扫描全部证书,对「acme + 自动续期 + 进入 30 天窗口 + 非进行中 + 距上次尝试 >24h」
// 的证书触发异步续期。返回触发的数量。
func (s *service) SweepOnce(ctx context.Context) (int, error) {
	certs, err := s.store.list(ctx)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	n := 0
	for _, c := range certs {
		if c.Source != SourceACME || !c.AutoRenew || c.Status == StatusPending {
			continue
		}
		if c.NotAfter.IsZero() || now.Add(sweepRenewWindow).Before(c.NotAfter) {
			continue // 未进续期窗口
		}
		if !c.LastAttemptAt.IsZero() && now.Sub(c.LastAttemptAt) < sweepRetryAfter {
			continue // 失败退避:24h 内不重试(LE 有速率限制)
		}
		// runAsync 同步占位 in-flight 锁:占位失败 = 进行中,跳过本轮。
		if !s.runAsync(c.ID, "到期自动续期中…", func(ctx context.Context) error {
			return s.issueCert(ctx, c.ID)
		}) {
			continue
		}
		n++
		s.logf("[certmgmt] 证书 %s(%s)距到期不足 %.0f 天,自动续期", c.ID, c.PrimaryDomain, sweepRenewWindow.Hours()/24)
	}
	return n, nil
}

// BackfillFromServiceReg 幂等回填 servicereg 既有手动证书(source=manual;失败仅记日志不阻断启动)。
func (s *service) BackfillFromServiceReg(ctx context.Context) (int, error) {
	return s.store.BackfillFromServiceReg(ctx)
}

// ---------- in-flight 锁 ----------

func (s *service) inflightAdd(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.inflight[id]; busy {
		return false
	}
	s.inflight[id] = struct{}{}
	return true
}

func (s *service) inflightRemove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, id)
}

func (s *service) inflightHas(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, busy := s.inflight[id]
	return busy
}

// ---------- 校验 ----------

// domainRe 校验 FQDN(与 proxy/servicereg 同规则:每段字母数字/连字符,顶级域 ≥2 字母)。
var domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// wildcardDomainRe 校验通配符域名 `*.<FQDN>`(仅最左一级通配)。
var wildcardDomainRe = regexp.MustCompile(`^\*\.([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

func newID() string { return "cert-" + uuid.NewString() }

// normalizeCreate 整形创建入参:小写去空白、附加域名去重(剔除主域)、CA/密钥类型补默认。
func normalizeCreate(in CreateInput) CreateInput {
	in.PrimaryDomain = strings.ToLower(strings.TrimSpace(in.PrimaryDomain))
	in.DNSProviderID = strings.TrimSpace(in.DNSProviderID)
	in.CA = strings.ToLower(strings.TrimSpace(in.CA))
	in.KeyType = strings.ToLower(strings.TrimSpace(in.KeyType))
	if in.CA == "" {
		in.CA = CALetsEncrypt
	}
	if in.KeyType == "" {
		in.KeyType = KeyTypeEC256
	}
	doms := make([]string, 0, len(in.Domains))
	for _, d := range in.Domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" || d == in.PrimaryDomain {
			continue
		}
		dup := false
		for _, x := range doms {
			if x == d {
				dup = true
				break
			}
		}
		if !dup {
			doms = append(doms, d)
		}
	}
	in.Domains = doms
	return in
}

func (s *service) validateCreate(ctx context.Context, in CreateInput) error {
	in = normalizeCreate(in) // 容忍调用方未归一化(测试/内部复用),Create 路径双归一幂等
	if err := validateDomainName(in.PrimaryDomain); err != nil {
		return err
	}
	if len(in.Domains) > maxDomains-1 {
		return ErrTooManyDomains
	}
	for _, d := range in.Domains {
		if err := validateDomainName(d); err != nil {
			return err
		}
	}
	if _, ok := caServerURL[in.CA]; !ok {
		return ErrInvalidCA
	}
	switch in.KeyType {
	case KeyTypeEC256, KeyTypeEC384, KeyTypeRSA2048:
	default:
		return ErrInvalidKeyType
	}
	// DNS-01 必备:提供商存在 + 类型合法 + 每个域名都落在其托管的根区下
	// (TXT 记录要建在权威区里,通配符/普通域一视同仁)。
	if in.DNSProviderID == "" {
		return ErrNoDNSProvider
	}
	if s.dns == nil {
		return ErrInvalidDNSProvider
	}
	pt, ok, err := s.dns.ProviderType(ctx, in.DNSProviderID)
	if err != nil {
		return err
	}
	if !ok || dnsAPIName(pt) == "" {
		return ErrInvalidDNSProvider
	}
	return s.validateZoneCoverage(ctx, in.DNSProviderID, append([]string{in.PrimaryDomain}, in.Domains...))
}

// validateZoneCoverage 校验每个域名都落在提供商托管的某根区下(域 == 根区 或 以 .根区 结尾;
// 通配符剥掉 "*." 后同规则)。
func (s *service) validateZoneCoverage(ctx context.Context, providerID string, domains []string) error {
	zones, ok, err := s.dns.ProviderZones(ctx, providerID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidDNSProvider
	}
	for _, d := range domains {
		base := strings.TrimPrefix(d, "*.")
		covered := false
		for _, z := range zones {
			if base == z || strings.HasSuffix(base, "."+z) {
				covered = true
				break
			}
		}
		if !covered {
			return fmt.Errorf("%w:%s", ErrZoneUncovered, d)
		}
	}
	return nil
}

// validateDomainName 校验普通 FQDN 或 `*.FQDN`。
func validateDomainName(d string) error {
	if d == "" || len(d) > 253 {
		return ErrInvalidDomain
	}
	if strings.HasPrefix(d, "*.") {
		if !wildcardDomainRe.MatchString(d) {
			return ErrInvalidDomain
		}
		return nil
	}
	if !domainRe.MatchString(d) {
		return ErrInvalidDomain
	}
	return nil
}

// dnsAPIName 把平台 DNS 提供商类型映射到 acme.sh 的 dnsapi 插件名;未知类型返回 ""。
func dnsAPIName(providerType string) string {
	switch providerType {
	case "cloudflare":
		return "dns_cf"
	case "dnspod":
		return "dns_dp"
	case "alidns":
		return "dns_ali"
	default:
		return ""
	}
}

// dnsEnvFor 按提供商类型把 (API ID, Secret) 映射为 acme.sh dnsapi 的环境变量(有序,供脚本渲染)。
func dnsEnvFor(providerType, apiID, secret string) [][2]string {
	switch providerType {
	case "cloudflare":
		return [][2]string{{"CF_Token", secret}}
	case "dnspod":
		return [][2]string{{"DP_Id", apiID}, {"DP_Key", secret}}
	case "alidns":
		return [][2]string{{"Ali_Key", apiID}, {"Ali_Secret", secret}}
	default:
		return nil
	}
}

// ---------- PEM 解析 ----------

// certMeta 是从叶子证书提取的展示/生命周期元数据。
type certMeta struct {
	subject   string
	issuer    string
	notBefore time.Time
	notAfter  time.Time
}

// validatePEMPair 校验 PEM 形态与证书/私钥配对;返回叶子证书,非法返回 nil。
func validatePEMPair(certPEM, keyPEM string) *x509.Certificate {
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil || len(pair.Certificate) == 0 {
		return nil
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil
	}
	return leaf
}

// leafMeta 提取叶子证书元数据(subject/issuer CN 优先,有效期 UTC)。
func leafMeta(leaf *x509.Certificate) certMeta {
	m := certMeta{
		subject:   strings.TrimSpace(leaf.Subject.String()),
		issuer:    strings.TrimSpace(leaf.Issuer.CommonName),
		notBefore: leaf.NotBefore.UTC(),
		notAfter:  leaf.NotAfter.UTC(),
	}
	if m.issuer == "" && len(leaf.Issuer.Organization) > 0 {
		m.issuer = leaf.Issuer.Organization[0]
	}
	if m.subject == "" && len(leaf.Subject.Organization) > 0 {
		m.subject = leaf.Subject.Organization[0]
	}
	return m
}

// lowerUnique 小写去重保序;空项丢弃。
func lowerUnique(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// truncate 截断长文本(入库/回显限制)。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
