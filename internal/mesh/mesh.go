// Package mesh 是「集群」领域层:对等节点(每套独立部署的 pipewright 实例)+ EasyTier 自组网。
//
// 设计要点(对等,无中心):
//   - 节点身份:首用惰性生成 UUID + ed25519 密钥对;私钥经 vault.SealSecret 密文落库,
//     公钥(hex)随 /api/mesh/hello 发布。node_id 是 ACL 授权与「被推送源项目」对应关系
//     (origin_node_id + origin_project_ref)的锚点。
//   - 权限默认全拒绝:mesh_acl 三张单(visible_to / push_allowed / trigger_allowed),
//     均为 JSON 节点 id 数组;验签通过 ≠ 放行,还须过对应 ACL。
//   - 通信:所有节点间请求走 EasyTier 虚拟 IP(网络层 network-secret + WireGuard 加密),
//     应用层再叠加节点密钥签名(X-PW-Mesh-* 头,防重放),双层防御。
package mesh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// 源类型枚举(mesh_peers.source)。
const (
	// PeerSourceAuto 表示由发现循环经 /api/mesh/hello 探测登记。
	PeerSourceAuto = "auto"
	// PeerSourceManual 表示管理员手工补录(发现不到时的兜底)。
	PeerSourceManual = "manual"
)

// EasyTier 模式枚举。
const (
	// ModeManaged 表示 pipewright 托管 easytier-core 子进程(默认)。
	ModeManaged = "managed"
	// ModeExternal 表示运维自跑 EasyTier,pipewright 只读其 RPC portal。
	ModeExternal = "external"
)

// 领域错误。错误体不含密钥/密文。
var (
	// ErrNotFound 表示节点/配置不存在。
	ErrNotFound = errors.New("mesh: not found")
	// ErrInvalidInput 表示入参非法(名称为空、模式枚举外等)。
	ErrInvalidInput = errors.New("mesh: invalid input")
	// ErrVaultUnconfigured 表示保险库未配置 master key,无法生成/解密节点密钥。
	ErrVaultUnconfigured = errors.New("mesh: vault unconfigured")
	// ErrKeyMismatch 表示对端公钥与本地登记不一致(疑似身份冒用,须人工核查)。
	ErrKeyMismatch = errors.New("mesh: peer public key mismatch")
	// ErrPeerUnreachable 表示对端不可达(hello 失败/无虚拟 IP)。
	ErrPeerUnreachable = errors.New("mesh: peer unreachable")
	// ErrForbidden 表示对端未获授权(ACL 默认全拒绝)。
	ErrForbidden = errors.New("mesh: forbidden by acl")
	// ErrSecureModeOff 表示 Secure Mode 未启用,临时凭据签发不可用。
	ErrSecureModeOff = errors.New("mesh: secure mode not enabled")
	// ErrDuplicate 表示唯一键冲突(API 令牌重名等)。
	ErrDuplicate = errors.New("mesh: duplicate")
)

// Settings 是本节点 mesh 配置(单行;私钥/网络密钥只回显「是否已配置」)。
type Settings struct {
	NodeID string
	Name   string
	// PublicKey 是本节点 ed25519 公钥(hex,可公开)。
	PublicKey string
	// Enabled 是否启用集群(mesh 未启用时对内不暴露 /api/mesh/*,对外不发现)。
	Enabled bool
	// APIPort 是广播给对端的 HTTP 端口(0 = 跟随服务监听口,由装配层解析)。
	APIPort int
	// EasyTierMode 是 managed | external。
	EasyTierMode string
	// EasyTierRPC 是本机 easytier RPC portal 地址(状态查询用)。
	EasyTierRPC string
	// NetworkName / VirtualIP / Peers 是 EasyTier 组网参数(密钥不回显)。
	NetworkName string
	VirtualIP   string
	Peers       []string
	// ProxyNetworksCustom 是管理员手工配置的「本节点可达、通告进集群虚拟网络」的网段
	// 列表(cidr;与 servicereg 自动产出的网关 /32 合并后作为 easytier 子网代理参数)。
	// 通告后 EasyTier 内置用户态 NAT:被代理网段内设备无需回程路由即可返程 —— 客户端
	// (pwclient)与各节点经此访问这些远端网段。注意:通告 ≠ 自动路由 —— 其他节点仅经
	// 路由白名单(见 routes.go)才接受;**不允许 /32**(该粒度保留给网关自动通告)。
	ProxyNetworksCustom []string
	// AcceptNetworksCustom 是本节点显式接受的跨环境通告网段(路由白名单的手动项;
	// 自动项仅各环境网关 /32)。访问其他环境的自定义网段必须在此逐项确认。
	AcceptNetworksCustom []string
	// EnableExitNode 允许本节点充当客户端本地代理的出口节点(foreign network NAT;
	// EasyTier 以 network secret 校验接入,默认关闭 —— 管理员显式开启)。
	EnableExitNode bool
	// SecureMode 以 --secure-mode 运行本节点 easytier(v2.6.0+:Noise XX 握手认证 +
	// 接受临时凭据入网;network_secret 持有者照常接入)。常规路径经全集群统一开关协议
	// 在约定时刻翻转(modeswitch.go);PUT settings 直改仅作节点失联时的恢复舱口。
	SecureMode bool
	// HasSecret / HasKey 报告密钥是否已配置(绝不回显内容)。
	HasSecret bool
	HasKey    bool
}

// SaveSettingsInput 是保存本节点配置的入参;密钥类字段经专用方法轮换,不在此携带。
type SaveSettingsInput struct {
	Name          *string
	Enabled       *bool
	APIPort       *int
	EasyTierMode  *string
	EasyTierRPC   *string
	NetworkName   *string
	NetworkSecret *string // 明文入参,进程内 Seal 后落库;nil = 不改
	VirtualIP     *string
	Peers         *[]string
	// ProxyNetworksCustom 逐项须为合法 cidr 且非 /32(该粒度保留给网关自动通告,
	// 见 routes.go 路由隔离约定);含非法项返回 ErrInvalidInput。
	ProxyNetworksCustom *[]string
	// AcceptNetworksCustom 逐项须为合法 cidr;nil = 不改。
	AcceptNetworksCustom *[]string
	// EnableExitNode nil = 不改。
	EnableExitNode *bool
	// SecureMode nil = 不改。恢复舱口:统一开关协议之外的单节点手动对齐(节点失联
	// 错过齐切后重新入 cluster 用);常规启停务必走 mode-change 协议,防混跑分裂。
	SecureMode *bool
}

// Peer 是已知对等节点。
type Peer struct {
	NodeID    string `json:"nodeId"`
	Name      string `json:"name"`
	VirtualIP string `json:"virtualIp"`
	APIPort   int    `json:"apiPort"`
	PublicKey string `json:"publicKey"`
	Version   string `json:"version"`
	Source    string `json:"source"`
	// PublicURL 是对端对外可访问的 Web 地址(人用深链;浏览器通常不在 EasyTier 网内,
	// 虚拟 IP 打不开)。空 = 回退虚拟 IP 基址。
	PublicURL string `json:"publicUrl"`
	LastSeen  string `json:"lastSeen"`
	UpdatedAt string `json:"updatedAt"`
}

// ACL 是五张默认全拒绝的授权单(JSON 节点 id 数组)。
type ACL struct {
	// VisibleTo 允许查询我的服务器指标、项目列表/详情、运行状态(集群页展开数据)。
	VisibleTo []string `json:"visibleTo"`
	// PushAllowed 允许向我推送产物/镜像(自动建项目 / 新修订)。
	PushAllowed []string `json:"pushAllowed"`
	// TriggerAllowed 允许启动我的工作流。
	TriggerAllowed []string `json:"triggerAllowed"`
	// ForwardAllowed 允许查询我的服务目录并转发我的服务(跨环境服务转发;
	// 网关 IP 清单随目录下发,对端网关机据此配 /32 路由)。
	ForwardAllowed []string `json:"forwardAllowed"`
	// BuildAllowed 允许查询我的共享构建机目录并向我的集群共享构建机提交构建任务
	// (任务进入我的构建机调度器 FIFO,与本端任务统一排队;SSH 凭据仍只由本端使用)。
	BuildAllowed []string `json:"buildAllowed"`
	// SwitchAllowed 允许向我下发/撤销 Secure Mode 统一切换指令(全集群开关协议的
	// 入站端点鉴权;发起节点须先获各对端授权,否则齐切 fan-out 会被对端 403)。
	SwitchAllowed []string `json:"switchAllowed"`
}

// Service 定义集群领域对外接口。
type Service interface {
	// Settings 返回本节点配置;首用惰性生成身份(node_id + 密钥对)。
	Settings(ctx context.Context) (*Settings, error)
	// SaveSettings 更新本节点配置(密钥轮换经 NetworkSecret 指针字段)。
	SaveSettings(ctx context.Context, in SaveSettingsInput) (*Settings, error)
	// PrivateKey 解密返回本节点 ed25519 私钥(仅进程内签名用,绝不外泄/落日志)。
	PrivateKey(ctx context.Context) (ed25519.PrivateKey, error)
	// NetworkSecret 解密返回 EasyTier 组网密钥(仅进程内拉起 easytier-core 用,绝不外泄/落日志;
	// 未配置返回空串)。
	NetworkSecret(ctx context.Context) (string, error)

	// ListPeers / GetPeer / DeletePeer 管理已知节点注册表。
	ListPeers(ctx context.Context) ([]Peer, error)
	GetPeer(ctx context.Context, nodeID string) (*Peer, error)
	DeletePeer(ctx context.Context, nodeID string) error
	// SaveManualPeer 手工补录/改名对端(发现兜底;node_id 不可空)。
	SaveManualPeer(ctx context.Context, p Peer) error
	// ApplyHello 应用一次发现探测结果(TOFU:首次登记公钥;已登记且不一致 → ErrKeyMismatch)。
	ApplyHello(ctx context.Context, virtualIP string, hello HelloInfo) error

	// ACL 返回五张授权单(无行时返回全空单,不报错)。
	ACL(ctx context.Context) (*ACL, error)
	// SaveACL 保存授权单(整单覆盖)。
	SaveACL(ctx context.Context, in ACL) error
	// AllowsVisible / AllowsPush / AllowsTrigger / AllowsForward / AllowsBuild / AllowsSwitch
	// 判定某节点是否过对应授权(默认拒绝)。
	AllowsVisible(ctx context.Context, nodeID string) bool
	AllowsPush(ctx context.Context, nodeID string) bool
	AllowsTrigger(ctx context.Context, nodeID string) bool
	AllowsForward(ctx context.Context, nodeID string) bool
	AllowsBuild(ctx context.Context, nodeID string) bool
	AllowsSwitch(ctx context.Context, nodeID string) bool

	// ---- 临时凭据(EasyTier Secure Mode 签发登记;签发动作本身经 CredentialProvider,
	// 存储只落登记簿,绝落 credential_secret) ----

	// IssueCredentialRecord 登记一条已签发凭据(credentialID 预生成,expires 按 TTL 推算)。
	IssueCredentialRecord(ctx context.Context, credentialID, remark string, ttlSeconds int64, actor, source string) (*CredentialRecord, error)
	// ListCredentialRecords 返回登记簿(新→旧;不与 easytier 活跃态合并,合并在上层)。
	ListCredentialRecords(ctx context.Context) ([]CredentialRecord, error)
	// MarkCredentialRevoked 标记撤销(幂等;未知 ID 返回 ErrNotFound)。
	MarkCredentialRevoked(ctx context.Context, credentialID string) error

	// ---- 凭据自动申请 API 令牌(明文仅创建响应一次;库内只存 SHA-256) ----

	// CreateApiToken 创建令牌,返回记录与明文(仅此一次)。
	CreateApiToken(ctx context.Context, name string) (*ApiToken, string, error)
	// ListApiTokens 列出令牌(含 revoked;不含哈希)。
	ListApiTokens(ctx context.Context) ([]ApiToken, error)
	// RevokeApiToken 按 name 撤销(立即失效)。
	RevokeApiToken(ctx context.Context, name string) error
	// ApiTokenLookup 校证明文令牌:有效返回令牌名(顺带刷新 last_used_at),否则空串。
	ApiTokenLookup(ctx context.Context, plaintext string) string

	// ---- Secure Mode 全集群统一开关(本地暂存与应用;fan-out 协调在 ModeSwitcher) ----

	// GetModePending 返回本地暂存的切换指令(无 → nil)。
	GetModePending(ctx context.Context) (*ModePending, error)
	// SaveModePending 落一条切换指令(单行覆盖;入站下发与本端发起共用)。
	SaveModePending(ctx context.Context, p ModePending) error
	// ClearModePending 清除暂存(仅当 change_id 匹配,防误清新指令;已应用的不清)。
	ClearModePending(ctx context.Context, changeID string) error
	// ApplyDueModeChange 应用到期的切换:翻转 secure_mode 并标记 applied(供对账节拍
	// 调用;不到期/无指令/已应用返回 false)。返回是否发生了翻转。
	ApplyDueModeChange(ctx context.Context, now time.Time) (bool, error)
}

// HelloInfo 是 /api/mesh/hello 的响应载荷(发现握手)。
type HelloInfo struct {
	NodeID    string `json:"nodeId"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	PublicKey string `json:"publicKey"`
	APIPort   int    `json:"apiPort"`
}

// service 是 store + vault 支撑的 Service 实现。dialect 驱动 upsert 尾句跨方言生成。
type service struct {
	db      *sql.DB
	dialect store.Dialect
	vault   vault.Vault
}

// New 构造 Service。vault 为 nil/未配置时身份生成与签名不可用(优雅降级:集群功能关闭)。
func New(db *sql.DB, v vault.Vault) Service {
	return &service{db: db, dialect: store.DialectOf(db), vault: v}
}

// ---- 身份与配置 ----

// Settings 实现接口:读取(必要时惰性创建)mesh_settings 单行。
func (s *service) Settings(ctx context.Context) (*Settings, error) {
	row := s.db.QueryRowContext(ctx, `SELECT node_id, name, public_key, private_key,
		enabled, api_port, easytier_mode, easytier_rpc, network_name, network_secret, virtual_ip, peers,
		proxy_networks_custom, accept_networks_custom, enable_exit_node, secure_mode
		FROM mesh_settings WHERE id = 1`)
	st, err := scanSettings(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return s.createDefault(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("mesh: load settings: %w", err)
	}
	// 创建时保险库未配置的补生成路径:首次能加密时补齐密钥对(一次性写,幂等)。
	if !st.HasKey {
		return s.ensureKeyPair(ctx, st)
	}
	return st, nil
}

// createDefault 首用生成身份:node_id(uuid)+ ed25519 密钥对(私钥 Seal 落库)。
// 保险库未配置时仍建行(公钥/私钥留空,启用时 SaveSettings 会再补),保证 Settings 永可用。
func (s *service) createDefault(ctx context.Context) (*Settings, error) {
	nodeID := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339)
	pubHex, privSealed := "", []byte(nil)
	if s.vault != nil {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("mesh: generate keypair: %w", err)
		}
		sealed, err := s.vault.SealSecret(priv)
		if err != nil {
			return nil, fmt.Errorf("mesh: seal private key: %w", err)
		}
		pubHex = hex.EncodeToString(pub)
		privSealed = sealed
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO mesh_settings (id, node_id, name, public_key, private_key, enabled, api_port,
		 easytier_mode, easytier_rpc, network_name, network_secret, virtual_ip, peers, created_at, updated_at)
		 VALUES (1, ?, '', ?, ?, 0, 0, 'managed', '127.0.0.1:15888', '', NULL, '', '', ?, ?)
		 `+store.DoNothingSuffix(s.dialect, []string{"id"}),
		nodeID, pubHex, privSealed, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("mesh: insert settings: %w", err)
	}
	return s.Settings(ctx)
}

func (s *service) SaveSettings(ctx context.Context, in SaveSettingsInput) (*Settings, error) {
	cur, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}
	name, enabled, apiPort := cur.Name, cur.Enabled, cur.APIPort
	mode, rpc, netName, vip, peers := cur.EasyTierMode, cur.EasyTierRPC, cur.NetworkName, cur.VirtualIP, cur.Peers
	proxies := cur.ProxyNetworksCustom
	accepts := cur.AcceptNetworksCustom
	exitNode := cur.EnableExitNode
	secure := cur.SecureMode
	if in.Name != nil {
		name = strings.TrimSpace(*in.Name)
	}
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	if in.APIPort != nil {
		apiPort = *in.APIPort
	}
	if in.EasyTierMode != nil {
		mode = strings.TrimSpace(*in.EasyTierMode)
		if mode != ModeManaged && mode != ModeExternal {
			return nil, fmt.Errorf("%w: easytier mode must be managed|external", ErrInvalidInput)
		}
	}
	if in.EasyTierRPC != nil {
		rpc = strings.TrimSpace(*in.EasyTierRPC)
	}
	if in.NetworkName != nil {
		netName = strings.TrimSpace(*in.NetworkName)
	}
	if in.VirtualIP != nil {
		vip = strings.TrimSpace(*in.VirtualIP)
	}
	if in.Peers != nil {
		peers = normalizePeers(*in.Peers)
	}
	if in.ProxyNetworksCustom != nil {
		var err error
		if proxies, err = normalizeCIDRs(*in.ProxyNetworksCustom); err != nil {
			return nil, err
		}
		if bad := firstHostRoute(proxies); bad != "" {
			return nil, fmt.Errorf("%w: 通告网段 %s 为 /32 —— 该粒度保留给跨环境网关自动通告(路由隔离约定,见 routes.go),请使用更宽网段", ErrInvalidInput, bad)
		}
	}
	if in.EnableExitNode != nil {
		exitNode = *in.EnableExitNode
	}
	if in.SecureMode != nil {
		secure = *in.SecureMode
	}
	if in.AcceptNetworksCustom != nil {
		var err error
		if accepts, err = normalizeCIDRs(*in.AcceptNetworksCustom); err != nil {
			return nil, err
		}
	}

	nowStr := time.Now().UTC().Format(time.RFC3339)
	setSecret := ""
	secretArg := any(nil)
	if in.NetworkSecret != nil && strings.TrimSpace(*in.NetworkSecret) != "" {
		if s.vault == nil {
			return nil, ErrVaultUnconfigured
		}
		sealed, err := s.vault.SealSecret([]byte(strings.TrimSpace(*in.NetworkSecret)))
		if err != nil {
			return nil, fmt.Errorf("mesh: seal network secret: %w", err)
		}
		setSecret = ", network_secret = ?"
		secretArg = sealed
	}
	// 参数序必须与占位符序严格对齐:network_secret 的占位符位于 network_name 之后、
	// virtual_ip 之前(此前误把密钥参数追加在末尾,导致各列错位写串)。
	args := []any{name, enabled, apiPort, mode, rpc, netName}
	if setSecret != "" {
		args = append(args, secretArg)
	}
	args = append(args, vip, strings.Join(peers, "\n"), strings.Join(proxies, "\n"),
		strings.Join(accepts, "\n"), exitNode, secure, nowStr)
	_, err = s.db.ExecContext(ctx,
		`UPDATE mesh_settings SET name = ?, enabled = ?, api_port = ?, easytier_mode = ?, easytier_rpc = ?,
		 network_name = ?`+setSecret+`, virtual_ip = ?, peers = ?, proxy_networks_custom = ?, accept_networks_custom = ?, enable_exit_node = ?, secure_mode = ?, updated_at = ? WHERE id = 1`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("mesh: update settings: %w", err)
	}
	return s.Settings(ctx)
}

// PrivateKey 实现接口:解密返回 ed25519 私钥。
func (s *service) PrivateKey(ctx context.Context) (ed25519.PrivateKey, error) {
	var sealed []byte
	err := s.db.QueryRowContext(ctx, `SELECT private_key FROM mesh_settings WHERE id = 1`).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) || sealed == nil {
		return nil, ErrVaultUnconfigured
	}
	if err != nil {
		return nil, fmt.Errorf("mesh: load private key: %w", err)
	}
	if s.vault == nil {
		return nil, ErrVaultUnconfigured
	}
	raw, err := s.vault.OpenSecret(sealed)
	if err != nil {
		return nil, fmt.Errorf("mesh: open private key: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("mesh: private key size %d invalid", len(raw))
	}
	return ed25519.PrivateKey(raw), nil
}

// NetworkSecret 实现 Service.NetworkSecret:解密组网密钥(未配置 → 空串,非错误)。
func (s *service) NetworkSecret(ctx context.Context) (string, error) {
	var sealed []byte
	err := s.db.QueryRowContext(ctx, `SELECT network_secret FROM mesh_settings WHERE id = 1`).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) || sealed == nil {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("mesh: load network secret: %w", err)
	}
	if s.vault == nil {
		return "", ErrVaultUnconfigured
	}
	raw, err := s.vault.OpenSecret(sealed)
	if err != nil {
		return "", fmt.Errorf("mesh: open network secret: %w", err)
	}
	return string(raw), nil
}

// ensureKeyPair 在启用/签名前确保密钥对存在(创建时保险库未配置的补生成路径)。
func (s *service) ensureKeyPair(ctx context.Context, st *Settings) (*Settings, error) {
	if st.HasKey || s.vault == nil {
		return st, nil
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("mesh: generate keypair: %w", err)
	}
	sealed, err := s.vault.SealSecret(priv)
	if err != nil {
		return nil, fmt.Errorf("mesh: seal private key: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE mesh_settings SET public_key = ?, private_key = ?, updated_at = ? WHERE id = 1`,
		hex.EncodeToString(pub), sealed, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return nil, fmt.Errorf("mesh: save keypair: %w", err)
	}
	return s.Settings(ctx)
}

// scanSettings 把 mesh_settings 一行扫描为 Settings(私钥/密钥只回显存在性)。
func scanSettings(scan func(dest ...any) error) (*Settings, error) {
	var st Settings
	var priv, secret []byte
	var enabled int
	var peersRaw, proxiesRaw, acceptsRaw string
	var exitNode, secure int
	if err := scan(&st.NodeID, &st.Name, &st.PublicKey, &priv,
		&enabled, &st.APIPort, &st.EasyTierMode, &st.EasyTierRPC,
		&st.NetworkName, &secret, &st.VirtualIP, &peersRaw, &proxiesRaw, &acceptsRaw, &exitNode, &secure); err != nil {
		return nil, err
	}
	st.Enabled = enabled != 0
	st.HasSecret = secret != nil
	st.HasKey = priv != nil && st.PublicKey != ""
	st.Peers = splitPeers(peersRaw)
	// 存储值来自归一化后的写入,理论恒合法;解析兜底为静默丢弃(防御旧数据/手工改库)。
	st.ProxyNetworksCustom = keepValidCIDRs(strings.Split(proxiesRaw, "\n"))
	st.AcceptNetworksCustom = keepValidCIDRs(strings.Split(acceptsRaw, "\n"))
	st.EnableExitNode = exitNode != 0
	st.SecureMode = secure != 0
	return &st, nil
}

// firstHostRoute 返回列表中第一个 /32(无则空串;路由隔离:通告侧不允许主机路由)。
func firstHostRoute(cidrs []string) string {
	for _, c := range cidrs {
		if strings.HasSuffix(c, "/32") {
			return c
		}
	}
	return ""
}

// normalizeCIDRs 归一化 cidr 列表:去空白/去空项/去重(保序),逐项须为合法 cidr
// (net.ParseCIDR;非法项带原值报 ErrInvalidInput,管理员手输需即时反馈)。
func normalizeCIDRs(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, c := range in {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(c); err != nil {
			return nil, fmt.Errorf("%w: %q 不是合法的 CIDR 网段(形如 192.168.30.0/24)", ErrInvalidInput, c)
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out, nil
}

// keepValidCIDRs 保留合法 cidr(去重保序),非法项静默丢弃(读取侧兜底,非用户输入路径)。
func keepValidCIDRs(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, c := range in {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(c); err != nil {
			continue
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out
}

// normalizePeers/splitPeers 处理 peer URL 列表(换行分隔存储)。
func normalizePeers(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

func splitPeers(raw string) []string {
	return normalizePeers(strings.Split(raw, "\n"))
}

// ---- 对等节点注册表 ----

func (s *service) ListPeers(ctx context.Context) ([]Peer, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT node_id, name, virtual_ip, api_port, public_key, version, source, public_url, last_seen, updated_at
		 FROM mesh_peers ORDER BY created_at, node_id`)
	if err != nil {
		return nil, fmt.Errorf("mesh: list peers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Peer{}
	for rows.Next() {
		p, err := scanPeer(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *service) GetPeer(ctx context.Context, nodeID string) (*Peer, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT node_id, name, virtual_ip, api_port, public_key, version, source, public_url, last_seen, updated_at
		 FROM mesh_peers WHERE node_id = ?`, nodeID)
	p, err := scanPeer(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("mesh: get peer: %w", err)
	}
	return p, nil
}

// SaveManualPeer 手工补录/改名:按 node_id upsert,source 固定 manual(已存在 auto 行时保留
// public_key 等探测信息,仅补名称/地址)。
func (s *service) SaveManualPeer(ctx context.Context, p Peer) error {
	p.NodeID = strings.TrimSpace(p.NodeID)
	if p.NodeID == "" {
		return fmt.Errorf("%w: node id must not be empty", ErrInvalidInput)
	}
	nowStr := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO mesh_peers (node_id, name, virtual_ip, api_port, public_key, version, source, public_url, last_seen, created_at, updated_at)
		 VALUES (?, ?, ?, ?, '', '', 'manual', ?, '', ?, ?)
		 `+store.UpsertAssignSuffix(s.dialect, []string{"node_id"}, []string{
			"name = " + store.Excluded(s.dialect, "name"),
			"virtual_ip = " + store.Excluded(s.dialect, "virtual_ip"),
			"api_port = " + store.Excluded(s.dialect, "api_port"),
			"public_url = " + store.Excluded(s.dialect, "public_url"),
			"updated_at = " + store.Excluded(s.dialect, "updated_at"),
		}),
		p.NodeID, strings.TrimSpace(p.Name), strings.TrimSpace(p.VirtualIP), p.APIPort,
		strings.TrimSpace(p.PublicURL), nowStr, nowStr,
	)
	if err != nil {
		return fmt.Errorf("mesh: save peer: %w", err)
	}
	return nil
}

// ApplyHello 实现发现探测 upsert:首次见到的 node_id 登记其公钥(TOFU);已登记且公钥不一致
// → ErrKeyMismatch(疑似冒用,绝不静默覆盖)。
func (s *service) ApplyHello(ctx context.Context, virtualIP string, hello HelloInfo) error {
	hello.NodeID = strings.TrimSpace(hello.NodeID)
	hello.PublicKey = strings.TrimSpace(hello.PublicKey)
	if hello.NodeID == "" || hello.PublicKey == "" {
		return fmt.Errorf("%w: hello missing node id/public key", ErrInvalidInput)
	}
	existing, err := s.GetPeer(ctx, hello.NodeID)
	nowStr := time.Now().UTC().Format(time.RFC3339)
	switch {
	case errors.Is(err, ErrNotFound):
		_, ierr := s.db.ExecContext(ctx,
			`INSERT INTO mesh_peers (node_id, name, virtual_ip, api_port, public_key, version, source, public_url, last_seen, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, 'auto', '', ?, ?, ?)`,
			hello.NodeID, strings.TrimSpace(hello.Name), strings.TrimSpace(virtualIP), hello.APIPort,
			hello.PublicKey, strings.TrimSpace(hello.Version), nowStr, nowStr, nowStr,
		)
		if ierr != nil {
			return fmt.Errorf("mesh: insert peer: %w", ierr)
		}
		return nil
	case err != nil:
		return err
	case existing.PublicKey != "" && existing.PublicKey != hello.PublicKey:
		return ErrKeyMismatch
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE mesh_peers SET name = ?, virtual_ip = ?, api_port = ?, version = ?, last_seen = ?, updated_at = ?
		 WHERE node_id = ?`,
		strings.TrimSpace(hello.Name), strings.TrimSpace(virtualIP), hello.APIPort,
		strings.TrimSpace(hello.Version), nowStr, nowStr, hello.NodeID,
	)
	if err != nil {
		return fmt.Errorf("mesh: touch peer: %w", err)
	}
	return nil
}

func (s *service) DeletePeer(ctx context.Context, nodeID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM mesh_peers WHERE node_id = ?`, nodeID)
	if err != nil {
		return fmt.Errorf("mesh: delete peer: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanPeer(scan func(dest ...any) error) (*Peer, error) {
	var p Peer
	if err := scan(&p.NodeID, &p.Name, &p.VirtualIP, &p.APIPort, &p.PublicKey,
		&p.Version, &p.Source, &p.PublicURL, &p.LastSeen, &p.UpdatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

// ---- 授权单(默认全拒绝) ----

func (s *service) ACL(ctx context.Context) (*ACL, error) {
	var vis, push, trig, fwd, build, switchL string
	err := s.db.QueryRowContext(ctx,
		`SELECT visible_to, push_allowed, trigger_allowed, forward_allowed, build_allowed, switch_allowed FROM mesh_acl WHERE id = 1`).Scan(&vis, &push, &trig, &fwd, &build, &switchL)
	if errors.Is(err, sql.ErrNoRows) {
		return &ACL{}, nil // 无行 = 全空单(默认全拒绝)
	}
	if err != nil {
		return nil, fmt.Errorf("mesh: load acl: %w", err)
	}
	out := &ACL{}
	_ = jsonUnmarshalList(vis, &out.VisibleTo)
	_ = jsonUnmarshalList(push, &out.PushAllowed)
	_ = jsonUnmarshalList(trig, &out.TriggerAllowed)
	_ = jsonUnmarshalList(fwd, &out.ForwardAllowed)
	_ = jsonUnmarshalList(build, &out.BuildAllowed)
	_ = jsonUnmarshalList(switchL, &out.SwitchAllowed)
	return out, nil
}

func (s *service) SaveACL(ctx context.Context, in ACL) error {
	vis, err := jsonMarshalList(normalizeIDList(in.VisibleTo))
	if err != nil {
		return fmt.Errorf("mesh: marshal visible_to: %w", err)
	}
	push, err := jsonMarshalList(normalizeIDList(in.PushAllowed))
	if err != nil {
		return fmt.Errorf("mesh: marshal push_allowed: %w", err)
	}
	trig, err := jsonMarshalList(normalizeIDList(in.TriggerAllowed))
	if err != nil {
		return fmt.Errorf("mesh: marshal trigger_allowed: %w", err)
	}
	fwd, err := jsonMarshalList(normalizeIDList(in.ForwardAllowed))
	if err != nil {
		return fmt.Errorf("mesh: marshal forward_allowed: %w", err)
	}
	build, err := jsonMarshalList(normalizeIDList(in.BuildAllowed))
	if err != nil {
		return fmt.Errorf("mesh: marshal build_allowed: %w", err)
	}
	switchL, err := jsonMarshalList(normalizeIDList(in.SwitchAllowed))
	if err != nil {
		return fmt.Errorf("mesh: marshal switch_allowed: %w", err)
	}
	nowStr := time.Now().UTC().Format(time.RFC3339)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO mesh_acl (id, visible_to, push_allowed, trigger_allowed, forward_allowed, build_allowed, switch_allowed, updated_at)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?)
		 `+store.UpsertAssignSuffix(s.dialect, []string{"id"}, []string{
			"visible_to = " + store.Excluded(s.dialect, "visible_to"),
			"push_allowed = " + store.Excluded(s.dialect, "push_allowed"),
			"trigger_allowed = " + store.Excluded(s.dialect, "trigger_allowed"),
			"forward_allowed = " + store.Excluded(s.dialect, "forward_allowed"),
			"build_allowed = " + store.Excluded(s.dialect, "build_allowed"),
			"switch_allowed = " + store.Excluded(s.dialect, "switch_allowed"),
			"updated_at = " + store.Excluded(s.dialect, "updated_at"),
		}),
		vis, push, trig, fwd, build, switchL, nowStr,
	)
	if err != nil {
		return fmt.Errorf("mesh: save acl: %w", err)
	}
	return nil
}

func (s *service) AllowsVisible(ctx context.Context, nodeID string) bool {
	return s.aclHas(ctx, "visible_to", nodeID)
}

func (s *service) AllowsPush(ctx context.Context, nodeID string) bool {
	return s.aclHas(ctx, "push_allowed", nodeID)
}

func (s *service) AllowsTrigger(ctx context.Context, nodeID string) bool {
	return s.aclHas(ctx, "trigger_allowed", nodeID)
}

func (s *service) AllowsForward(ctx context.Context, nodeID string) bool {
	return s.aclHas(ctx, "forward_allowed", nodeID)
}

func (s *service) AllowsBuild(ctx context.Context, nodeID string) bool {
	return s.aclHas(ctx, "build_allowed", nodeID)
}

func (s *service) AllowsSwitch(ctx context.Context, nodeID string) bool {
	return s.aclHas(ctx, "switch_allowed", nodeID)
}

// aclHas 判定授权单(单列 JSON 数组)是否包含 node_id。查询失败按拒绝(保守)。
func (s *service) aclHas(ctx context.Context, column, nodeID string) bool {
	var raw string
	err := s.db.QueryRowContext(ctx,
		`SELECT `+column+` FROM mesh_acl WHERE id = 1`).Scan(&raw)
	if err != nil {
		return false
	}
	var ids []string
	if err := jsonUnmarshalList(raw, &ids); err != nil {
		return false
	}
	for _, id := range ids {
		if id == nodeID {
			return true
		}
	}
	return false
}

// normalizeIDList 去空/去重(授权单保存前的规范化)。
func normalizeIDList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, id := range in {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// jsonMarshalList 序列化字符串数组为 JSON(空 → [])。
func jsonMarshalList(in []string) (string, error) {
	if in == nil {
		in = []string{}
	}
	b, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// jsonUnmarshalList 反序列化 JSON 数组列(空串 → 空数组,不报错)。
func jsonUnmarshalList(raw string, out *[]string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		*out = []string{}
		return nil
	}
	return json.Unmarshal([]byte(raw), out)
}
