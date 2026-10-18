// Package pveadmin 是「PVE 集群纳管与管理」的领域层。
//
// 纳管:登记 PVE 物理机时 SSH 探测(pveversion + corosync.conf/pvecm nodes)自动发现
// 集群,建 pve_clusters 记录与 pve-<集群名> 服务器池(kind=pve),同凭据登记其余兄弟
// 节点(已存在的按 host 归池不重建);幂等(集群名唯一,二次登记归池)。
//
// 纪律:
//   - API Token 经 vault.SealSecret 密文 BLOB 落库,绝不明文入库、出响应、进日志/审计;
//   - PVE 物理机(kind=pve_node)不允许作为构建/部署/应用部署目标(拦截在 target/deploy/
//     runner/httpapi 各层,本包只负责打标);
//   - 电源操作默认 readonly=1(领域层拒绝),解锁是显式、留审计的独立动作;
//   - node/vmid/action 入参白名单校验;命令与请求只含固定 argv/路径,绝不拼 shell;
//   - 错误体永不携带 Token 或密文(客户端错误只含 URL 路径/状态码/PVE 自身错误文本)。
package pveadmin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// 集群状态机:created(探测发现/未配 Token)→ ready(Token 测连通过)。
const (
	StateCreated = "created"
	StateReady   = "ready"
)

// TLS 模式枚举。
const (
	TLSModeVerify      = "verify"
	TLSModeInsecure    = "insecure"
	TLSModeFingerprint = "fingerprint"
)

// 领域错误(人话由 httpapi 层映射状态码;错误体绝不含 Token/密文/地址凭据)。
var (
	// ErrNotFound 表示集群(或关联记录)不存在。
	ErrNotFound = errors.New("pveadmin: cluster not found")
	// ErrNameTaken 表示集群名已被占用(不同集群重名)。
	ErrNameTaken = errors.New("pveadmin: cluster name already taken")
	// ErrInvalidInput 表示入参非法(URL/Token 格式/白名单外动作等)。
	ErrInvalidInput = errors.New("pveadmin: invalid input")
	// ErrVaultUnconfigured 表示保险库未配置 master key,Token 无法密封/解封。
	ErrVaultUnconfigured = errors.New("pveadmin: vault unconfigured")
	// ErrClusterNotReady 表示集群尚未配置可用 Token(管理面操作不可用)。
	ErrClusterNotReady = errors.New("pveadmin: cluster not ready")
	// ErrReadonly 表示集群处于只读模式,电源操作被拒绝。
	ErrReadonly = errors.New("pveadmin: cluster is read-only")
	// ErrConnectFailed 表示 PVE API 连接/请求失败(错误体不含 Token)。
	ErrConnectFailed = errors.New("pveadmin: pve api request failed")
	// ErrNoPrivilege 表示探测目标机非 root 且无免密 sudo。
	ErrNoPrivilege = errors.New("pveadmin: no privilege on host")
	// ErrProbe 表示探测命令执行失败(非零退出/不可达;错误体带 stderr 尾部)。
	ErrProbe = errors.New("pveadmin: probe command failed")
)

// 白名单正则。
var (
	// nameRe 集群名/池名/节点名白名单(corosync cluster_name 常规字符)。
	nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
	// tokenNameRe 是 PVE API Token 标识 user@realm!tokenid。
	tokenNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*@[A-Za-z0-9][A-Za-z0-9_.-]*![A-Za-z0-9][A-Za-z0-9_.-]*$`)
	// fingerprintRe 是 SHA256 指纹(64 hex,容忍冒号分隔与大写)。
	fingerprintRe = regexp.MustCompile(`(?i)^([0-9a-f]{2}:?){32}$`)
	// upidRe 是 PVE UPID 粗形校验(不含路径分隔与空白)。
	upidRe = regexp.MustCompile(`^UPID:[A-Za-z0-9_.\-+:,]{1,256}$`)
)

// Cluster 是 pve_clusters 行模型(TokenSealed 绝不出响应)。
type Cluster struct {
	ID             string
	Name           string
	APIURL         string
	TokenName      string
	TokenSealed    []byte
	TLSMode        string
	TLSFingerprint string
	State          string
	Readonly       bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ClusterView 是出响应的集群视图(无密文;HasToken 代替)。
type ClusterView struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	APIURL         string `json:"apiUrl"`
	TokenName      string `json:"tokenName"`
	HasToken       bool   `json:"hasToken"`
	TLSMode        string `json:"tlsMode"`
	TLSFingerprint string `json:"tlsFingerprint"`
	State          string `json:"state"`
	Readonly       bool   `json:"readonly"`
	PoolID         string `json:"poolId"`
	PoolName       string `json:"poolName"`
	NodeCount      int    `json:"nodeCount"`
	CreatedAt      string `json:"createdAt"`
	UpdatedAt      string `json:"updatedAt"`
}

// DiscoveryResult 是登记钩子的发现结果(附在服务器登记响应上供前端提示)。
type DiscoveryResult struct {
	ClusterName     string   `json:"clusterName"`
	ClusterID       string   `json:"clusterId"`
	PoolID          string   `json:"poolId"`
	PoolName        string   `json:"poolName"`
	Standalone      bool     `json:"standalone"`
	CreatedSiblings []string `json:"createdSiblings"`
	AttachedCount   int      `json:"attachedCount"`
}

// NodeStatus 是集群总览里的一个节点。
type NodeStatus struct {
	Name   string `json:"name"`
	IP     string `json:"ip"`
	Online bool   `json:"online"`
}

// ResourceSummary 是集群资源汇总(全集群合计)。
type ResourceSummary struct {
	CPUUsed      float64 `json:"cpuUsed"`
	CPUTotal     int64   `json:"cpuTotal"`
	MemUsed      int64   `json:"memUsed"`
	MemTotal     int64   `json:"memTotal"`
	StorageUsed  int64   `json:"storageUsed"`
	StorageTotal int64   `json:"storageTotal"`
}

// Overview 是集群总览。
type Overview struct {
	Version   string          `json:"version"`
	Quorate   bool            `json:"quorate"`
	Nodes     []NodeStatus    `json:"nodes"`
	Resources ResourceSummary `json:"resources"`
}

// VM 是集群资源里的一个虚拟机/容器(qemu | lxc)。
type VM struct {
	VMID   int64   `json:"vmid"`
	Name   string  `json:"name"`
	Node   string  `json:"node"`
	Type   string  `json:"type"`
	Status string  `json:"status"`
	CPU    float64 `json:"cpu"`
	MaxCPU int64   `json:"maxCpu"`
	Mem    int64   `json:"mem"`
	MaxMem int64   `json:"maxMem"`
	Uptime int64   `json:"uptime"`
}

// TaskRef 是电源操作受理结果(异步任务句柄)。
type TaskRef struct {
	Node string `json:"node"`
	UPID string `json:"upid"`
}

// TaskStatusView 是任务状态(前端轮询至 done)。
type TaskStatusView struct {
	UPID       string `json:"upid"`
	Node       string `json:"node"`
	Status     string `json:"status"` // running | stopped
	ExitStatus string `json:"exitStatus"`
	Done       bool   `json:"done"`
	OK         bool   `json:"ok"`
}

// ClusterTask 是任务中心条目。
type ClusterTask struct {
	UPID      string `json:"upid"`
	Node      string `json:"node"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	User      string `json:"user"`
	StartTime int64  `json:"startTime"`
	EndTime   int64  `json:"endTime"`
}

// Service 是 PVE 集群管理领域接口。
type Service interface {
	// OnServerRegistered 是服务器登记钩子:探测 PVE → 自动建集群/池/兄弟节点。
	// 非 PVE 或探测不可达 → (nil, nil)(尽力而为,绝不阻断登记)。
	OnServerRegistered(ctx context.Context, serverID string) (*DiscoveryResult, error)
	// ListClusters 列集群(含池信息与节点数;无密文)。
	ListClusters(ctx context.Context) ([]ClusterView, error)
	// GetCluster 取单集群视图。
	GetCluster(ctx context.Context, id string) (*ClusterView, error)
	// SetToken 保存/更换 API Token(密封落库)并立即测连:成功 → ready;失败 → 保持
	// created 并返回测连错误(Token 已保存)。
	SetToken(ctx context.Context, id, apiURL, tokenName, tokenSecret, tlsMode, tlsFingerprint string) error
	// TestCluster 实连 /version 往返;成功则置 ready。
	TestCluster(ctx context.Context, id string) error
	// SetReadonly 切换只读开关(电源操作闸门)。
	SetReadonly(ctx context.Context, id string, readonly bool) error
	// DeleteCluster 删除集群:池内机器归位 default/standard,再删池与集群行。
	DeleteCluster(ctx context.Context, id string) error
	// Overview 集群总览(版本/quorum/节点/资源汇总)。
	Overview(ctx context.Context, id string) (*Overview, error)
	// ListVMs 列全部虚拟机与容器(/cluster/resources?type=vm)。
	ListVMs(ctx context.Context, id string) ([]VM, error)
	// VMAction 电源操作(qemu|lxc;start|shutdown|stop|reboot|reset|suspend|resume),
	// 返回异步任务句柄。readonly → ErrReadonly。
	VMAction(ctx context.Context, id, node, kind string, vmid int64, action string) (*TaskRef, error)
	// TaskStatus 查询任务状态(前端轮询至 done)。
	TaskStatus(ctx context.Context, id, node, upid string) (*TaskStatusView, error)
	// ListTasks 最近集群任务(默认 50,上限 500)。
	ListTasks(ctx context.Context, id string, limit int) ([]ClusterTask, error)
}

type service struct {
	db      *sql.DB
	vault   vault.Vault
	servers target.Service
	runner  Runner
	clientF clientFactory
}

// New 构造 PVE 集群管理服务。vault 未配置 master key 时 Token 登记/管理面不可用
// (ErrVaultUnconfigured),但不影响平台启动与探测建池。
func New(db *sql.DB, v vault.Vault, servers target.Service) Service {
	return &service{db: db, vault: v, servers: servers, runner: &sshRunner{servers: servers}, clientF: prodClientFactory}
}

// ---- 探测登记钩子 -------------------------------------------------------------------

// OnServerRegistered 探测并纳管(见 Service 接口注释)。所有失败路径都返回 (nil, nil):
// 探测是尽力而为的增值动作,不能因它失败令服务器登记报错。
func (s *service) OnServerRegistered(ctx context.Context, serverID string) (*DiscoveryResult, error) {
	seed, err := s.servers.Get(ctx, serverID)
	if err != nil {
		return nil, nil
	}
	info, err := s.probeServer(ctx, serverID)
	if err != nil || info == nil {
		return nil, nil
	}

	// 集群 upsert(名唯一:已有则复用,实现二次登记幂等)。
	cl, err := s.ensureCluster(ctx, info.ClusterName)
	if err != nil {
		return nil, nil // 库错误不阻断登记
	}
	poolID, poolName, err := s.ensurePool(ctx, cl.ID, info.ClusterName)
	if err != nil {
		return nil, nil
	}

	// 种子机归池。
	if err := s.servers.SetPlacement(ctx, serverID, poolID, target.KindPVENode); err != nil {
		return nil, nil
	}

	// 兄弟节点:按 host 归集既有机器,缺的建占位(同种子机凭据,待逐台验证)。
	all, err := s.servers.List(ctx)
	if err != nil {
		return &DiscoveryResult{ClusterName: cl.Name, ClusterID: cl.ID, PoolID: poolID, PoolName: poolName, Standalone: info.Standalone}, nil
	}
	byHost := make(map[string]*target.Server, len(all))
	for _, srv := range all {
		if srv.ID != serverID {
			byHost[srv.Host] = srv
		}
	}
	res := &DiscoveryResult{ClusterName: cl.Name, ClusterID: cl.ID, PoolID: poolID, PoolName: poolName, Standalone: info.Standalone}
	for _, n := range info.Nodes {
		if isSelfNode(n, seed, info.SelfName) {
			continue // 种子机自身(按 ring0 地址或短主机名匹配)。
		}
		if existing, found := byHost[n.Addr]; found {
			if existing.IsPVENode() {
				continue // 已在本池(先前探测建过)。
			}
			if err := s.servers.SetPlacement(ctx, existing.ID, poolID, target.KindPVENode); err == nil {
				res.AttachedCount++
			}
			continue
		}
		if n.Addr == "" {
			continue
		}
		created, err := s.servers.Create(ctx, target.CreateInput{
			Name: n.Name, Host: n.Addr, Port: seed.Port, User: seed.User,
			CredentialID:     seed.CredentialID,
			SudoCredentialID: seed.SudoCredentialID,
			PoolID:           poolID,
			Kind:             target.KindPVENode,
		})
		if err != nil {
			continue // 单台失败不拖垮整批(占位可后续手工补)。
		}
		res.CreatedSiblings = append(res.CreatedSiblings, created.Name)
	}
	return res, nil
}

// isSelfNode 判断 nodelist 条目是否种子机自身(ring0 地址 == 登记地址,或名字命中)。
// 网络/登记地址不一致时靠名字兜底;都未命中时该节点会被按普通兄弟登记(占位可与种子机
// 重复,由管理员后续清理——探测信息不足以区分的场景宁可多登记不可漏登记)。
func isSelfNode(n NodeInfo, seed *target.Server, selfName string) bool {
	return (n.Addr != "" && n.Addr == seed.Host) || (n.Name != "" && n.Name == seed.Host) ||
		(selfName != "" && n.Name == selfName)
}

// ensureCluster 按集群名 upsert pve_clusters 行(存在即复用;名是幂等键)。
func (s *service) ensureCluster(ctx context.Context, name string) (*Cluster, error) {
	if !nameRe.MatchString(name) {
		return nil, fmt.Errorf("%w: 集群名 %q 不合规", ErrInvalidInput, name)
	}
	if cl, err := s.clusterByName(ctx, name); err == nil {
		return cl, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	id := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pve_clusters (id, name, state, readonly, created_at, updated_at) VALUES (?, ?, ?, 1, ?, ?)`,
		id, name, StateCreated, now, now)
	if err != nil {
		if isUniqueViolation(err) { // 并发登记同集群:读回既有行。
			return s.clusterByName(ctx, name)
		}
		return nil, fmt.Errorf("pveadmin: insert cluster: %w", err)
	}
	return s.clusterByName(ctx, name)
}

// ensurePool 按池名 pve-<集群名> upsert server_pools 行(kind=pve,cluster_id 关联)。
func (s *service) ensurePool(ctx context.Context, clusterID, clusterName string) (string, string, error) {
	poolName := "pve-" + clusterName
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM server_pools WHERE name = ?`, poolName).Scan(&id)
	if err == nil {
		return id, poolName, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("pveadmin: query pool: %w", err)
	}
	id = uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339)
	_, ierr := s.db.ExecContext(ctx,
		`INSERT INTO server_pools (id, name, kind, cluster_id, created_at, updated_at) VALUES (?, ?, 'pve', ?, ?, ?)`,
		id, poolName, clusterID, now, now)
	if ierr != nil {
		if isUniqueViolation(err) || isUniqueViolation(ierr) {
			if err := s.db.QueryRowContext(ctx, `SELECT id FROM server_pools WHERE name = ?`, poolName).Scan(&id); err == nil {
				return id, poolName, nil
			}
		}
		return "", "", fmt.Errorf("pveadmin: insert pool: %w", ierr)
	}
	return id, poolName, nil
}

func (s *service) clusterByName(ctx context.Context, name string) (*Cluster, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, api_url, token_name, token_sealed, tls_mode, tls_fingerprint, state, readonly, created_at, updated_at FROM pve_clusters WHERE name = ?`, name)
	return scanCluster(row)
}

func (s *service) clusterByID(ctx context.Context, id string) (*Cluster, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, api_url, token_name, token_sealed, tls_mode, tls_fingerprint, state, readonly, created_at, updated_at FROM pve_clusters WHERE id = ?`, id)
	return scanCluster(row)
}

func scanCluster(sc scanner2) (*Cluster, error) {
	var cl Cluster
	var sealed []byte
	var createdStr, updatedStr string
	var ro int
	if err := sc.Scan(&cl.ID, &cl.Name, &cl.APIURL, &cl.TokenName, &sealed, &cl.TLSMode, &cl.TLSFingerprint, &cl.State, &ro, &createdStr, &updatedStr); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("pveadmin: scan cluster: %w", err)
	}
	cl.TokenSealed = sealed
	cl.Readonly = ro != 0
	var err error
	if cl.CreatedAt, err = time.Parse(time.RFC3339, createdStr); err != nil {
		return nil, fmt.Errorf("pveadmin: parse created_at: %w", err)
	}
	if cl.UpdatedAt, err = time.Parse(time.RFC3339, updatedStr); err != nil {
		return nil, fmt.Errorf("pveadmin: parse updated_at: %w", err)
	}
	return &cl, nil
}

// scanner2 抽象 Row/Rows(名字避开与 target 包混淆;仅本包内使用)。
type scanner2 interface {
	Scan(dest ...any) error
}

// isUniqueViolation 判断双方言唯一索引冲突文本。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToUpper(err.Error())
	return strings.Contains(msg, "IDX_PVE_CLUSTERS_NAME") ||
		strings.Contains(msg, "IDX_SERVER_POOLS_NAME") ||
		strings.Contains(msg, "UNIQUE CONSTRAINT FAILED: PVE_CLUSTERS") ||
		strings.Contains(msg, "UNIQUE CONSTRAINT FAILED: SERVER_POOLS") ||
		strings.Contains(msg, "DUPLICATE ENTRY")
}

// ---- 集群视图与 CRUD ----------------------------------------------------------------

func (s *service) ListClusters(ctx context.Context) ([]ClusterView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM pve_clusters ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("pveadmin: list: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("pveadmin: list scan: %w", err)
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pveadmin: list iterate: %w", err)
	}
	out := make([]ClusterView, 0, len(ids))
	for _, id := range ids {
		v, err := s.view(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, nil
}

func (s *service) GetCluster(ctx context.Context, id string) (*ClusterView, error) {
	return s.view(ctx, id)
}

// view 组装集群视图(join 池与节点计数;无密文)。
func (s *service) view(ctx context.Context, id string) (*ClusterView, error) {
	cl, err := s.clusterByID(ctx, id)
	if err != nil {
		return nil, err
	}
	v := &ClusterView{
		ID: cl.ID, Name: cl.Name, APIURL: cl.APIURL, TokenName: cl.TokenName,
		HasToken: len(cl.TokenSealed) > 0, TLSMode: cl.TLSMode, TLSFingerprint: cl.TLSFingerprint,
		State: cl.State, Readonly: cl.Readonly,
		CreatedAt: cl.CreatedAt.Format(time.RFC3339), UpdatedAt: cl.UpdatedAt.Format(time.RFC3339),
	}
	_ = s.db.QueryRowContext(ctx, `SELECT id, name FROM server_pools WHERE cluster_id = ?`, cl.ID).Scan(&v.PoolID, &v.PoolName)
	if v.PoolID != "" {
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM servers WHERE pool_id = ?`, v.PoolID).Scan(&v.NodeCount)
	}
	return v, nil
}

// SetToken 校验并保存 Token(密封),随即测连(成功 → ready,失败 → 保持 created 并返回错误)。
func (s *service) SetToken(ctx context.Context, id, apiURL, tokenName, tokenSecret, tlsMode, tlsFingerprint string) error {
	apiURL = strings.TrimRight(strings.TrimSpace(apiURL), "/")
	if apiURL == "" {
		return fmt.Errorf("%w: API 地址不能为空", ErrInvalidInput)
	}
	u, err := url.Parse(apiURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%w: API 地址必须形如 https://host:8006", ErrInvalidInput)
	}
	tokenName = strings.TrimSpace(tokenName)
	if !tokenNameRe.MatchString(tokenName) {
		return fmt.Errorf("%w: Token 标识须形如 user@pve!tokenid", ErrInvalidInput)
	}
	tokenSecret = strings.TrimSpace(tokenSecret)
	if tokenSecret == "" || len(tokenSecret) > 256 {
		return fmt.Errorf("%w: Token 值不能为空且不超过 256 字符", ErrInvalidInput)
	}
	tlsMode = strings.TrimSpace(tlsMode)
	if tlsMode == "" {
		tlsMode = TLSModeInsecure
	}
	tlsFingerprint = strings.TrimSpace(tlsFingerprint)
	switch tlsMode {
	case TLSModeVerify, TLSModeInsecure:
		tlsFingerprint = ""
	case TLSModeFingerprint:
		if !fingerprintRe.MatchString(tlsFingerprint) {
			return fmt.Errorf("%w: 指纹须为 64 位十六进制 SHA256(容忍冒号分隔)", ErrInvalidInput)
		}
	default:
		return fmt.Errorf("%w: 未知 TLS 模式 %q", ErrInvalidInput, tlsMode)
	}

	if s.vault == nil {
		return fmt.Errorf("%w: %s", ErrVaultUnconfigured, "master key not configured")
	}
	sealed, err := s.vault.SealSecret([]byte(tokenSecret))
	if err != nil {
		return fmt.Errorf("%w: %s", ErrVaultUnconfigured, "master key not configured")
	}

	cl, err := s.clusterByID(ctx, id)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE pve_clusters SET api_url = ?, token_name = ?, token_sealed = ?, tls_mode = ?, tls_fingerprint = ?, state = ?, updated_at = ? WHERE id = ?`,
		apiURL, tokenName, sealed, tlsMode, tlsFingerprint, StateCreated, now, cl.ID); err != nil {
		return fmt.Errorf("pveadmin: save token: %w", err)
	}
	// 立即测连:成功 → ready;失败 → Token 已保存但保持 created,把测连错误交还调用方。
	if terr := s.testClusterRow(ctx, cl.ID); terr != nil {
		return fmt.Errorf("%w: Token 已保存,但连通测试失败: %v", ErrConnectFailed, unwrapTail(terr))
	}
	return nil
}

// unwrapTail 取错误链末段人话(去掉重复的领域前缀)。
func unwrapTail(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 && len(msg)-i < 400 {
		msg = msg[i+2:]
	}
	return msg
}

// TestCluster 实连测连(GET /version);成功置 ready。
func (s *service) TestCluster(ctx context.Context, id string) error {
	return s.testClusterRow(ctx, id)
}

func (s *service) testClusterRow(ctx context.Context, id string) error {
	cl, c, err := s.clientFor(ctx, id)
	if err != nil {
		return err
	}
	var ver apiVersion
	if err := c.Get(ctx, "/api2/json/version", &ver); err != nil {
		return err
	}
	if ver.Release == "" && ver.Version == "" {
		return fmt.Errorf("%w: /version 响应缺版本字段", ErrConnectFailed)
	}
	_, uerr := s.db.ExecContext(ctx, `UPDATE pve_clusters SET state = ?, updated_at = ? WHERE id = ?`,
		StateReady, time.Now().UTC().Format(time.RFC3339), cl.ID)
	if uerr != nil {
		return fmt.Errorf("pveadmin: mark ready: %w", uerr)
	}
	return nil
}

func (s *service) SetReadonly(ctx context.Context, id string, readonly bool) error {
	cl, err := s.clusterByID(ctx, id)
	if err != nil {
		return err
	}
	ro := 0
	if readonly {
		ro = 1
	}
	_, err = s.db.ExecContext(ctx, `UPDATE pve_clusters SET readonly = ?, updated_at = ? WHERE id = ?`,
		ro, time.Now().UTC().Format(time.RFC3339), cl.ID)
	if err != nil {
		return fmt.Errorf("pveadmin: set readonly: %w", err)
	}
	return nil
}

// DeleteCluster 删除集群:先归位池内机器(default/standard),再删池与集群行。
// Token 密文随行删除。
func (s *service) DeleteCluster(ctx context.Context, id string) error {
	cl, err := s.clusterByID(ctx, id)
	if err != nil {
		return err
	}
	var poolID string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM server_pools WHERE cluster_id = ?`, cl.ID).Scan(&poolID); err == nil && poolID != "" {
		now := time.Now().UTC().Format(time.RFC3339)
		if _, err := s.db.ExecContext(ctx, `UPDATE servers SET pool_id = ?, kind = ?, updated_at = ? WHERE pool_id = ?`,
			target.DefaultPoolID, target.KindStandard, now, poolID); err != nil {
			return fmt.Errorf("pveadmin: revert servers: %w", err)
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM server_pools WHERE id = ?`, poolID); err != nil {
			return fmt.Errorf("pveadmin: delete pool: %w", err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pve_clusters WHERE id = ?`, cl.ID); err != nil {
		return fmt.Errorf("pveadmin: delete cluster: %w", err)
	}
	return nil
}

// ---- 管理面(经 PVE API) -------------------------------------------------------------

// clientFor 取集群行并构造已解封 Token 的客户端(ready 校验前置)。
func (s *service) clientFor(ctx context.Context, id string) (*Cluster, pveClient, error) {
	cl, err := s.clusterByID(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if cl.APIURL == "" || len(cl.TokenSealed) == 0 || cl.TokenName == "" {
		return nil, nil, fmt.Errorf("%w: 集群 %s 未配置 API Token", ErrClusterNotReady, cl.Name)
	}
	if s.vault == nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrVaultUnconfigured, "master key not configured")
	}
	secret, err := s.vault.OpenSecret(cl.TokenSealed)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrVaultUnconfigured, "master key not configured")
	}
	c, err := s.clientF(cl.APIURL, cl.TokenName, string(secret), cl.TLSMode, cl.TLSFingerprint)
	if err != nil {
		return nil, nil, err
	}
	if c == nil {
		return nil, nil, errNilClient
	}
	return cl, c, nil
}

func (s *service) Overview(ctx context.Context, id string) (*Overview, error) {
	_, c, err := s.clientFor(ctx, id)
	if err != nil {
		return nil, err
	}
	ctx2, cancel := context.WithTimeout(ctx, pveRequestTimeout*2)
	defer cancel()

	var ver apiVersion
	if err := c.Get(ctx2, "/api2/json/version", &ver); err != nil {
		return nil, err
	}
	var cs apiClusterStatus
	if err := c.Get(ctx2, "/api2/json/cluster/status", &cs); err != nil {
		return nil, err
	}
	var res []apiResource
	if err := c.Get(ctx2, "/api2/json/cluster/resources", &res); err != nil {
		return nil, err
	}

	ov := &Overview{Version: ver.Release, Quorate: bool(cs.Quorate)}
	for _, n := range cs.Nodes {
		ov.Nodes = append(ov.Nodes, NodeStatus{Name: n.Name, IP: n.IP, Online: bool(n.Online)})
	}
	sort.Slice(ov.Nodes, func(i, j int) bool { return ov.Nodes[i].Name < ov.Nodes[j].Name })
	for _, r := range res {
		switch r.Type {
		case "node":
			ov.Resources.CPUUsed += float64(r.CPU) * float64(r.MaxCPU)
			ov.Resources.CPUTotal += int64(r.MaxCPU)
			ov.Resources.MemUsed += int64(r.Mem)
			ov.Resources.MemTotal += int64(r.MaxMem)
		case "storage":
			ov.Resources.StorageUsed += int64(r.Storage)
			ov.Resources.StorageTotal += int64(r.MaxStorage)
		}
	}
	return ov, nil
}

func (s *service) ListVMs(ctx context.Context, id string) ([]VM, error) {
	_, c, err := s.clientFor(ctx, id)
	if err != nil {
		return nil, err
	}
	var res []apiResource
	if err := c.Get(ctx, "/api2/json/cluster/resources?type=vm", &res); err != nil {
		return nil, err
	}
	out := make([]VM, 0, len(res))
	for _, r := range res {
		if r.Type != "qemu" && r.Type != "lxc" {
			continue
		}
		name := r.Name
		if name == "" {
			name = strconv.FormatInt(int64(r.VMID), 10)
		}
		out = append(out, VM{
			VMID: int64(r.VMID), Name: name, Node: r.Node, Type: r.Type, Status: r.Status,
			CPU: float64(r.CPU), MaxCPU: int64(r.MaxCPU), Mem: int64(r.Mem), MaxMem: int64(r.MaxMem), Uptime: int64(r.Uptime),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].VMID < out[j].VMID
	})
	return out, nil
}

// allowedVMActions 是电源操作白名单。
var allowedVMActions = map[string]bool{
	"start": true, "shutdown": true, "stop": true, "reboot": true, "reset": true, "suspend": true, "resume": true,
}

// allowedVMKinds 是虚拟化类型白名单(决定 API 路径前缀)。
var allowedVMKinds = map[string]bool{"qemu": true, "lxc": true}

func (s *service) VMAction(ctx context.Context, id, node, kind string, vmid int64, action string) (*TaskRef, error) {
	if !allowedVMActions[action] {
		return nil, fmt.Errorf("%w: 未知电源操作 %q", ErrInvalidInput, action)
	}
	if !allowedVMKinds[kind] {
		return nil, fmt.Errorf("%w: 未知虚拟化类型 %q", ErrInvalidInput, kind)
	}
	if !nameRe.MatchString(node) {
		return nil, fmt.Errorf("%w: 节点名不合规", ErrInvalidInput)
	}
	if vmid < 100 || vmid > 999999999 {
		return nil, fmt.Errorf("%w: vmid 越界", ErrInvalidInput)
	}
	cl, c, err := s.clientFor(ctx, id)
	if err != nil {
		return nil, err
	}
	if cl.Readonly {
		return nil, fmt.Errorf("%w: 集群 %s 处于只读模式,请在设置页解锁后再执行电源操作", ErrReadonly, cl.Name)
	}
	var upid string
	path := fmt.Sprintf("/api2/json/nodes/%s/%s/%d/status/%s", node, kind, vmid, action)
	if err := c.Post(ctx, path, url.Values{}, &upid); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(upid, "UPID:") {
		return nil, fmt.Errorf("%w: %s 响应缺 UPID", ErrConnectFailed, path)
	}
	return &TaskRef{Node: node, UPID: upid}, nil
}

func (s *service) TaskStatus(ctx context.Context, id, node, upid string) (*TaskStatusView, error) {
	if !nameRe.MatchString(node) {
		return nil, fmt.Errorf("%w: 节点名不合规", ErrInvalidInput)
	}
	if !upidRe.MatchString(upid) {
		return nil, fmt.Errorf("%w: UPID 不合规", ErrInvalidInput)
	}
	_, c, err := s.clientFor(ctx, id)
	if err != nil {
		return nil, err
	}
	var ts apiTaskStatus
	path := "/api2/json/nodes/" + node + "/tasks/" + url.PathEscape(upid) + "/status"
	if err := c.Get(ctx, path, &ts); err != nil {
		return nil, err
	}
	done := ts.Status != "running"
	ok := done && strings.EqualFold(strings.TrimSpace(ts.ExitStatus), "OK")
	return &TaskStatusView{
		UPID: upid, Node: node, Status: ts.Status, ExitStatus: ts.ExitStatus, Done: done, OK: ok,
	}, nil
}

func (s *service) ListTasks(ctx context.Context, id string, limit int) ([]ClusterTask, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	_, c, err := s.clientFor(ctx, id)
	if err != nil {
		return nil, err
	}
	var tasks []apiTask
	if err := c.Get(ctx, "/api2/json/cluster/tasks", &tasks); err != nil {
		return nil, err
	}
	sort.SliceStable(tasks, func(i, j int) bool { return int64(tasks[i].StartTime) > int64(tasks[j].StartTime) })
	out := make([]ClusterTask, 0, limit)
	for _, t := range tasks {
		if len(out) >= limit {
			break
		}
		out = append(out, ClusterTask{
			UPID: t.UPID, Node: t.Node, Type: t.Type, Status: t.Status, User: t.User,
			StartTime: int64(t.StartTime), EndTime: int64(t.EndTime),
		})
	}
	return out, nil
}
