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
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	// ErrBusy 表示集群已有建机/升级任务在跑(每集群同时一个,跨任务类型互斥)。
	ErrBusy = errors.New("pveadmin: create job busy")
	// ErrConfirmMismatch 表示危险操作的输入确认不匹配(须原样输入集群名/节点名)。
	ErrConfirmMismatch = errors.New("pveadmin: confirm mismatch")
	// ErrPreflightRequired 表示大版本升级缺少新鲜预检报告,或预检未通过/警告未确认。
	ErrPreflightRequired = errors.New("pveadmin: preflight required")
	// ErrCephDetected 表示集群部署了 Ceph,大版本升级须人工按官方流程处理(先升 Ceph 到 Squid)。
	ErrCephDetected = errors.New("pveadmin: ceph detected, manual upgrade required")
)

// 白名单正则。
var (
	// nameRe 集群名/池名/节点名白名单(corosync cluster_name 常规字符)。
	nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
	// tokenNameRe 是 PVE API Token 标识 user@realm!tokenid。
	tokenNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*@[A-Za-z0-9][A-Za-z0-9_.-]*![A-Za-z0-9][A-Za-z0-9_.-]*$`)
	// fingerprintRe 是 SHA256 指纹(64 hex,容忍冒号分隔与大写)。
	fingerprintRe = regexp.MustCompile(`(?i)^([0-9a-f]{2}:?){32}$`)
	// upidRe 是 PVE UPID 粗形校验(不含路径分隔与空白)。user 段按 PVE UPID.pm
	// 原样输出 root@pam / user@realm!tokenid(官方 decode 为 [^:\s/]+),须放行 @!%。
	upidRe = regexp.MustCompile(`^UPID:[A-Za-z0-9_.\-+:,@!%]{1,256}$`)
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
	// ListVMs 列全部虚拟机与容器(/cluster/resources 全量后按 qemu/lxc 过滤)。
	ListVMs(ctx context.Context, id string) ([]VM, error)
	// VMAction 电源操作(qemu|lxc;start|shutdown|stop|reboot|reset|suspend|resume),
	// 返回异步任务句柄。readonly → ErrReadonly。
	VMAction(ctx context.Context, id, node, kind string, vmid int64, action string) (*TaskRef, error)
	// TaskStatus 查询任务状态(前端轮询至 done)。
	TaskStatus(ctx context.Context, id, node, upid string) (*TaskStatusView, error)
	// ListTasks 最近集群任务(默认 50,上限 500)。
	ListTasks(ctx context.Context, id string, limit int) ([]ClusterTask, error)
	// ListTemplates 列集群全部 qemu/lxc 模板(建机克隆源下拉)。
	ListTemplates(ctx context.Context, id string) ([]Template, error)
	// ListStorages 列节点存储状态(建机目标存储/镜像存储下拉)。
	ListStorages(ctx context.Context, id, node string) ([]Storage, error)
	// ListBridges 列节点网桥(net0 bridge 下拉)。
	ListBridges(ctx context.Context, id, node string) ([]Bridge, error)
	// NextVMID 取集群下一个空闲 VMID。
	NextVMID(ctx context.Context, id string) (int64, error)
	// ListStorageContent 列存储内容(vztmpl/import/images/backup 卷下拉)。
	ListStorageContent(ctx context.Context, id, node, storage, content string) ([]ContentItem, error)
	// ListAplInfo 列节点可下载的 LXC 模板目录。
	ListAplInfo(ctx context.Context, id, node string) ([]AplTemplate, error)
	// DownloadAplTemplate 下载 LXC 模板到存储(写操作,readonly 闸门),返回任务句柄。
	DownloadAplTemplate(ctx context.Context, id, node, storage, template string) (*TaskRef, error)
	// StartCreate 异步启动建机管线(模板克隆/.vma 恢复/云镜像/LXC,批量 1..50)。
	// readonly → ErrReadonly;已有任务在跑 → ErrBusy。入参白名单校验前置。
	StartCreate(ctx context.Context, id string, in CreateInput) error
	// CreateStatus 建机任务快照(步骤/日志/逐台结果;前端轮询至 done)。
	CreateStatus(ctx context.Context, id string) (CreateStatus, error)
	// UploadBackup 流式上传 .vma 备份到节点存储(写操作,readonly 闸门),返回恢复源
	// (卷 ID 或绝对路径;9.x 经 SSH 落文件型存储,8.x 走 API)。
	UploadBackup(ctx context.Context, id, node, storage, fileName string, content io.Reader) (string, error)

	// ---- 升级面(日常 apt / 仓库管理 / 大版本 8→9) ----

	// ListUpdates 逐节点聚合版本/内核/待更新包/订阅/需重启标记(只读,并发扇出错误隔离)。
	ListUpdates(ctx context.Context, id string) (*UpdatesOverview, error)
	// RefreshUpdates 刷新节点 apt 索引(写操作,readonly 闸门),返回逐节点 UPID 句柄。
	RefreshUpdates(ctx context.Context, id string, nodes []string) ([]RefreshItem, error)
	// StartUpgrade 异步启动日常 apt 升级(纯 API,严格串行,逐节点容错)。
	// readonly → ErrReadonly;已有任务在跑 → ErrBusy。
	StartUpgrade(ctx context.Context, id string, in UpgradeInput) error
	// UpgradeStatus 日常升级任务快照(步骤/日志/逐节点结果;前端轮询至 done)。
	UpgradeStatus(ctx context.Context, id string) (UpgradeStatus, error)
	// RebootNode 重启节点(写操作,readonly 闸门;confirm 须等于节点名),返回任务句柄。
	RebootNode(ctx context.Context, id, node, confirm string) (*TaskRef, error)
	// ListAptRepos 读节点 apt 仓库清单(.list/.sources 双格式解析 + 警告;只读)。
	ListAptRepos(ctx context.Context, id, node string) (*AptRepoInventory, error)
	// ToggleAptRepo 启停单个仓库条目(写操作:备份→原子替换→apt update 验证)。
	ToggleAptRepo(ctx context.Context, id, node string, in AptRepoToggleInput) (*AptRepoWriteResult, error)
	// SetAptRepoMirror 切换 Debian/PVE 源镜像预设(official/ustc/tsinghua/aliyun;Ceph 源不动)。
	SetAptRepoMirror(ctx context.Context, id, node, preset string) (*AptRepoWriteResult, error)
	// SetAptRepoPreset 企业↔无订阅一键切换(no-subscription/enterprise;Ceph 源不动)。
	SetAptRepoPreset(ctx context.Context, id, node, preset string) (*AptRepoWriteResult, error)
	// RestoreAptRepo 从备份恢复仓库文件(backupPath 白名单校验)。
	RestoreAptRepo(ctx context.Context, id, node, path, backupPath string) (*AptRepoWriteResult, error)
	// ApplyAptRepos 把镜像/订阅预设批量应用到多节点(并发闸,逐节点结果隔离)。
	ApplyAptRepos(ctx context.Context, id string, in AptRepoApplyInput) ([]AptRepoApplyItem, error)
	// MajorPreflight 大版本升级预检(同步:pve8to9 --full/版本/磁盘/Ceph/仓库/任务),
	// 报告缓存 30min 供 StartMajorUpgrade 校验。
	MajorPreflight(ctx context.Context, id string) (*MajorPreflightReport, error)
	// StartMajorUpgrade 异步启动 8→9 滚动升级(持久化任务;confirm 须等于集群名,
	// 预检须新鲜且无 FAIL、警告全 ack)。readonly → ErrReadonly;占用 → ErrBusy。
	StartMajorUpgrade(ctx context.Context, id string, in MajorUpgradeInput) error
	// MajorUpgradeStatus 大版本升级快照(DB 持久化-backed;重启后 running → interrupted)。
	MajorUpgradeStatus(ctx context.Context, id string) (MajorUpgradeStatus, error)
	// AbortMajorUpgrade 请求中止(只在步骤边界生效,绝不中断 dist-upgrade)。
	AbortMajorUpgrade(ctx context.Context, id string) error
	// RetryMajorNode 重跑失败节点(confirm 须等于集群名;幂等)。
	RetryMajorNode(ctx context.Context, id, node, confirm string) error
	// ResumeMajorUpgrade 从首个未完成节点续跑 interrupted/paused 任务(confirm 须等于集群名)。
	ResumeMajorUpgrade(ctx context.Context, id, confirm string) error
}

type service struct {
	db      *sql.DB
	vault   vault.Vault
	servers target.Service
	runner  Runner
	clientF clientFactory
	// mu 保护后台任务表与预检缓存(每集群一个任务,跨类型互斥;ceph.deployJob 同形)。
	mu          sync.Mutex
	createJobs  map[string]*createJob
	upgradeJobs map[string]*upgradeJob // 日常 apt 升级(内存态)
	majorJobs   map[string]*majorJob   // 大版本升级(DB 持久化-backed)
	preflights  map[string]preflightEntry
}

// New 构造 PVE 集群管理服务。vault 未配置 master key 时 Token 登记/管理面不可用
// (ErrVaultUnconfigured),但不影响平台启动与探测建池。
func New(db *sql.DB, v vault.Vault, servers target.Service) Service {
	return &service{db: db, vault: v, servers: servers, runner: &sshRunner{servers: servers}, clientF: prodClientFactory,
		createJobs: map[string]*createJob{}, upgradeJobs: map[string]*upgradeJob{},
		majorJobs: map[string]*majorJob{}, preflights: map[string]preflightEntry{}}
}

// anyJobRunningLocked 判断集群是否有任一后台任务在跑(建机/日常升级/大版本升级互斥)。
// 调用方须持 s.mu。
func (s *service) anyJobRunningLocked(id string) bool {
	if j := s.createJobs[id]; j != nil && j.snapshot().Running {
		return true
	}
	if j := s.upgradeJobs[id]; j != nil && j.snapshot().Running {
		return true
	}
	if j := s.majorJobs[id]; j != nil && j.snapshot().Running {
		return true
	}
	return false
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.anyJobRunningLocked(id) {
		return fmt.Errorf("%w: 集群 %s 有建机/升级任务运行中,禁止删除", ErrBusy, cl.Name)
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
	// 建机映射随集群清理(不设外键,显式连带;机器行已归位 default,映射无意义)。
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pve_vm_links WHERE cluster_id = ?`, cl.ID); err != nil {
		return fmt.Errorf("pveadmin: delete vm links: %w", err)
	}
	// 升级任务历史随集群清理(节点行经 job 外键 CASCADE)。
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pve_upgrade_jobs WHERE cluster_id = ?`, cl.ID); err != nil {
		return fmt.Errorf("pveadmin: delete upgrade jobs: %w", err)
	}
	delete(s.preflights, id)
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
	var entries []apiClusterStatusEntry
	if err := c.Get(ctx2, "/api2/json/cluster/status", &entries); err != nil {
		return nil, err
	}
	var res []apiResource
	if err := c.Get(ctx2, "/api2/json/cluster/resources", &res); err != nil {
		return nil, err
	}

	ov := &Overview{Version: ver.Release}
	sawCluster := false
	for _, e := range entries {
		switch e.Type {
		case "cluster":
			sawCluster = true
			ov.Quorate = bool(e.Quorate)
		case "node":
			ov.Nodes = append(ov.Nodes, NodeStatus{Name: e.Name, IP: e.IP, Online: bool(e.Online)})
		}
	}
	if !sawCluster {
		// 单机形态只有 node 条目、无 quorate(Cluster.pm else 分支)—— 节点自治,视作有仲裁。
		ov.Quorate = true
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
			// storage 条目用量/总量在 disk/maxdisk(storage 字段是存储名,无 maxstorage)。
			ov.Resources.StorageUsed += int64(r.Disk)
			ov.Resources.StorageTotal += int64(r.MaxDisk)
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
	// type 枚举无 vm(node|storage|pool|qemu|lxc|openvz|sdn),取全量后本地过滤 qemu/lxc。
	if err := c.Get(ctx, "/api2/json/cluster/resources", &res); err != nil {
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

// vmActionsByKind 是电源操作白名单,按 kind 对齐 PVE 文档的
// /nodes/{node}/{qemu|lxc}/{vmid}/status 子端点:lxc 没有 reset(qemu 独有),
// 其余 start/shutdown/stop/reboot/suspend/resume 两边一致。
var vmActionsByKind = map[string]map[string]bool{
	"qemu": {"start": true, "shutdown": true, "stop": true, "reboot": true, "reset": true, "suspend": true, "resume": true},
	"lxc":  {"start": true, "shutdown": true, "stop": true, "reboot": true, "suspend": true, "resume": true},
}

func (s *service) VMAction(ctx context.Context, id, node, kind string, vmid int64, action string) (*TaskRef, error) {
	actions, ok := vmActionsByKind[kind]
	if !ok {
		return nil, fmt.Errorf("%w: 未知虚拟化类型 %q", ErrInvalidInput, kind)
	}
	if !actions[action] {
		return nil, fmt.Errorf("%w: 未知电源操作 %q(类型 %q 不支持)", ErrInvalidInput, action, kind)
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
