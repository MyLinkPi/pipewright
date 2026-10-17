// Package ceph 是「Ceph 集群部署与管理」的领域层。
//
// 部署:cephadm 引导(后台任务 + 步骤/日志快照轮询,srcupdate 同形),经 target.Service
// 以 SSH 在登记服务器上执行命令;管理:统一经 bootstrap 主机上的 ceph/rbd CLI
// (bootstrap 步骤安装 ceph-common,admin keyring 落 /etc/ceph),覆盖主机/OSD 纳管、
// 存储池、RBD 块设备、CephFS 子卷与集群状态采集。
//
// 纪律:
//   - 命令一律 []string argv(target 层逐参转义,AC-SEC-02),名称类入参先白名单校验;
//   - 提权仅 root / 免密 sudo(sudo -n),不实现密码 sudo;
//   - dashboard 密码自动生成后只入 vault(ceph_dashboard 类型),绝不落日志/响应明文;
//   - 密码之外,错误信息可携带远端 stderr 尾部(命令不含凭据)。
package ceph

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// 领域错误。错误体永不含明文口令/私钥/内部栈。
var (
	// ErrNotFound 表示集群(或关联记录)不存在。
	ErrNotFound = errors.New("ceph: cluster not found")
	// ErrNameTaken 表示集群名已被占用。
	ErrNameTaken = errors.New("ceph: cluster name already taken")
	// ErrInvalidInput 表示入参非法(名称不合规/主机为空/数值越界等)。
	ErrInvalidInput = errors.New("ceph: invalid input")
	// ErrBusy 表示该集群已有一次部署任务在进行。
	ErrBusy = errors.New("ceph: deploy already running")
	// ErrClusterNotReady 表示集群尚未完成部署(bootstrap 成功前管理操作不可用)。
	ErrClusterNotReady = errors.New("ceph: cluster not ready")
	// ErrNoPrivilege 表示目标主机既非 root 登录也无免密 sudo。
	ErrNoPrivilege = errors.New("ceph: no privilege on host")
	// ErrExec 表示远端命令执行失败(非零退出/不可达;错误体带 stderr 尾部)。
	ErrExec = errors.New("ceph: remote command failed")
	// ErrVaultUnconfigured 表示保险库未配置,无法保管 dashboard 密码。
	ErrVaultUnconfigured = errors.New("ceph: vault unconfigured")
)

// 集群状态机:created(登记)→ deploying(部署中)→ ready(可用)/ failed(部署失败)。
const (
	StateCreated   = "created"
	StateDeploying = "deploying"
	StateReady     = "ready"
	StateFailed    = "failed"
)

// DefaultImage 是未显式指定时的 Ceph 容器镜像(cephadm 拉起所有守护进程用)。
const DefaultImage = "quay.io/ceph/ceph:v19.2.1"

// DefaultDashboardUser 是 bootstrap 时创建的 dashboard 管理员用户名。
const DefaultDashboardUser = "admin"

// nameRe 是名称类入参(集群/池/镜像/快照/子卷)的白名单。
var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// deviceRe 是 OSD 块设备路径白名单(/dev/ 前缀的绝对路径)。
var deviceRe = regexp.MustCompile(`^/dev/[a-zA-Z0-9/_.-]+$`)

// cidrRe 是网段入参的白名单(宽容:IPv4/IPv6 CIDR 或留空)。
var cidrRe = regexp.MustCompile(`^[0-9A-Fa-f:.]+/\d{1,3}$`)

// labelRe 是 orch host 标签 token 白名单。
var labelRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// allowedRoles 是主机角色白名单(mon/mgr/osd)。
var allowedRoles = map[string]bool{"mon": true, "mgr": true, "osd": true}

// allowedApps 是可启用的 pool 应用标签白名单。
var allowedApps = map[string]bool{"rbd": true, "cephfs": true, "rgw": true}

// Cluster 是 Ceph 集群领域模型(配置 + 部署回填字段;绝无明文口令)。
type Cluster struct {
	ID                    string
	Name                  string
	FSID                  string // 部署成功后回填
	BootstrapServerID     string
	BootstrapServerName   string // join servers 冗余展示名;非持久列
	Image                 string
	CephadmSource         string // auto | package | curl:<脚本 URL>
	PublicNetwork         string // 记录用(bootstrap 以 mon-ip 推导)
	ClusterNetwork        string // 非空时传 --cluster-network
	State                 string
	DashboardURL          string
	DashboardUser         string
	DashboardCredentialID string // vault 引用(ceph_dashboard 类型)
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// HostSpec 是登记集群时的主机意图(部署管线消费;管理期 AddHost 复用)。
type HostSpec struct {
	ServerID string   `json:"serverId"`
	Roles    []string `json:"roles"`    // mon/mgr/osd 子集
	Labels   []string `json:"labels"`   // orch host 标签
	Devices  []string `json:"devices"`  // OSD 块设备路径(如 /dev/sdb)
	Hostname string   `json:"hostname"` // 部署回填(orch host 名)
	Addr     string   `json:"addr"`     // 部署回填(orch host 地址)
}

// HostView 是主机管理视图:DB 意图 + orch 实时状态合并(未部署/未纳管时 orch 为空)。
type HostView struct {
	HostSpec
	ServerName string
	InOrch     bool   // orch host ls 是否可见(纳管成功)
	OrchStatus string // orch 报告的 status(空 = 未知/未纳管)
}

// CreateInput 是登记集群的入参。
type CreateInput struct {
	Name              string
	BootstrapServerID string
	Image             string
	CephadmSource     string
	PublicNetwork     string
	ClusterNetwork    string
	Hosts             []HostSpec
}

// AddHostInput 是集群 ready 后追加主机的入参(orch host add + 可选批量加 OSD)。
type AddHostInput struct {
	ServerID string
	Labels   []string
	Devices  []string
}

// AddOSDInput 是给已纳管主机追加 OSD 的入参(host 为 orch 主机名)。
type AddOSDInput struct {
	Host   string
	Device string
}

// ClusterStatus 是集群健康/容量快照(ceph -s + health detail 解析;TTL 缓存)。
type ClusterStatus struct {
	Health       string // HEALTH_OK / HEALTH_WARN / HEALTH_ERR / HEALTH_UNKNOWN
	HealthDetail []string
	TotalBytes   uint64
	UsedBytes    uint64
	AvailBytes   uint64
	MonCount     int
	MgrCount     int
	OsdUp        int
	OsdIn        int
	PGsByState   []PGStateCount
}

// PGStateCount 是按状态聚合的 PG 数量(ceph -s pgmap.pgs_by_state)。
type PGStateCount struct {
	State string `json:"state"`
	Count int    `json:"count"`
}

// DeployStatus 是部署任务进度快照(GET .../deploy/status 载荷;srcupdate 同形)。
type DeployStatus struct {
	Running bool     `json:"running"`
	Step    string   `json:"step"`              // preflight|install|bootstrap|hosts|osds|verify
	Message string   `json:"message,omitempty"` // 当前步骤人读简述
	Done    bool     `json:"done"`              // 管线走完(成功或失败)
	Error   string   `json:"error,omitempty"`   // 非空 = 失败
	Log     []string `json:"log,omitempty"`     // 最近输出(环形,新在后)
}

// OrchHost 是 ceph orch host ls 的条目。
type OrchHost struct {
	Host   string   `json:"host"`
	Addr   string   `json:"addr"`
	Labels []string `json:"labels"`
	Status string   `json:"status"`
}

// OSD 是 OSD 列表条目(osd df + osd metadata 合并)。
type OSD struct {
	ID          int64   `json:"id"`
	Host        string  `json:"host"`
	Device      string  `json:"device"`
	Up          bool    `json:"up"`
	In          bool    `json:"in"`
	SizeBytes   uint64  `json:"sizeBytes"`
	UsedBytes   uint64  `json:"usedBytes"`
	Utilization float64 `json:"utilization"`
}

// Pool 是存储池条目(osd pool ls detail 解析)。
type Pool struct {
	Name          string   `json:"name"`
	Type          string   `json:"type"` // replicated | erasure
	Size          int      `json:"size"`
	ECProfile     string   `json:"ecProfile"`
	PGNum         int      `json:"pgNum"`
	BytesUsed     uint64   `json:"bytesUsed"`
	QuotaMaxBytes uint64   `json:"quotaMaxBytes"`
	Apps          []string `json:"apps"`
}

// PoolCreateInput 是创建存储池的入参。
type PoolCreateInput struct {
	Name      string   `json:"name"`
	PGNum     int      `json:"pgNum"`
	Type      string   `json:"type"`      // replicated(默认) | erasure
	Size      int      `json:"size"`      // replicated 副本数(0 = 用集群默认)
	ECProfile string   `json:"ecProfile"` // erasure profile 名(空 = default)
	Apps      []string `json:"apps"`      // rbd/cephfs/rgw 子集
}

// PoolUpdateInput 是编辑存储池的入参(指针字段 nil = 不修改)。
type PoolUpdateInput struct {
	QuotaMaxBytes *uint64 `json:"quotaMaxBytes"` // 0 = 清除配额
	Size          *int    `json:"size"`
}

// RBDImage 是 RBD 镜像条目(rbd ls -l 解析)。
type RBDImage struct {
	Pool         string `json:"pool"`
	Name         string `json:"name"`
	SizeBytes    uint64 `json:"sizeBytes"`
	Provisioning string `json:"provisioning"`
}

// ImageCreateInput 是创建镜像的入参。
type ImageCreateInput struct {
	Name   string `json:"name"`
	SizeGB int64  `json:"sizeGb"`
}

// ImageResizeInput 是扩容/缩容镜像的入参。
type ImageResizeInput struct {
	SizeGB      int64 `json:"sizeGb"`
	AllowShrink bool  `json:"allowShrink"`
}

// RBDSnapshot 是 RBD 快照条目(rbd snap ls 解析)。
type RBDSnapshot struct {
	Name      string `json:"name"`
	SizeBytes uint64 `json:"sizeBytes"`
	Protected bool   `json:"protected"`
}

// Filesystem 是 CephFS 条目(ceph fs ls 解析)。
type Filesystem struct {
	Name         string   `json:"name"`
	MetadataPool string   `json:"metadataPool"`
	DataPools    []string `json:"dataPools"`
	MdsCount     int      `json:"mdsCount"`
}

// Subvolume 是 CephFS 子卷条目(subvolumels + getpath + info 合并)。
type Subvolume struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	BytesUsed  uint64 `json:"bytesUsed"`
	QuotaBytes uint64 `json:"quotaBytes"` // 0 = 未限额
}

// SubvolumeCreateInput 是创建子卷的入参(sizeGb 0 = 不限额)。
type SubvolumeCreateInput struct {
	Name   string `json:"name"`
	SizeGB int64  `json:"sizeGb"`
}

// Service 定义 Ceph 域对外接口(httpapi 消费;冻结契约)。
type Service interface {
	// ── 集群 ──
	ListClusters(ctx context.Context) ([]*Cluster, error)
	GetCluster(ctx context.Context, id string) (*Cluster, error)
	CreateCluster(ctx context.Context, in CreateInput) (*Cluster, error)
	// DeleteCluster 仅解除纳管(删除登记数据),不触碰远端集群。
	DeleteCluster(ctx context.Context, id string) error
	// StartDeploy 异步启动部署管线(cephadm 引导);已在跑返回 ErrBusy。
	StartDeploy(ctx context.Context, id string) error
	// DeployStatus 返回部署任务快照;从未启动返回空快照。
	DeployStatus(ctx context.Context, id string) (DeployStatus, error)
	// ClusterStatus 返回健康/容量快照(15s TTL 缓存;集群未 ready → ErrClusterNotReady)。
	ClusterStatus(ctx context.Context, id string) (ClusterStatus, error)

	// ── 主机 / OSD ──
	ListHosts(ctx context.Context, id string) ([]HostView, error)
	AddHost(ctx context.Context, id string, in AddHostInput) error
	RemoveHost(ctx context.Context, id string, hostname string) error
	ListOSDs(ctx context.Context, id string) ([]OSD, error)
	AddOSD(ctx context.Context, id string, in AddOSDInput) error
	RemoveOSD(ctx context.Context, id string, osdID int64) error

	// ── 存储池 ──
	ListPools(ctx context.Context, id string) ([]Pool, error)
	CreatePool(ctx context.Context, id string, in PoolCreateInput) error
	UpdatePool(ctx context.Context, id string, name string, in PoolUpdateInput) error
	DeletePool(ctx context.Context, id string, name string) error

	// ── RBD 块设备 ──
	ListImages(ctx context.Context, id string, pool string) ([]RBDImage, error)
	CreateImage(ctx context.Context, id string, pool string, in ImageCreateInput) error
	ResizeImage(ctx context.Context, id string, pool string, image string, in ImageResizeInput) error
	DeleteImage(ctx context.Context, id string, pool string, image string) error
	ListSnapshots(ctx context.Context, id string, pool string, image string) ([]RBDSnapshot, error)
	CreateSnapshot(ctx context.Context, id string, pool string, image string, snap string) error
	DeleteSnapshot(ctx context.Context, id string, pool string, image string, snap string) error
	RollbackSnapshot(ctx context.Context, id string, pool string, image string, snap string) error

	// ── CephFS ──
	ListFilesystems(ctx context.Context, id string) ([]Filesystem, error)
	ListSubvolumes(ctx context.Context, id string, fs string) ([]Subvolume, error)
	CreateSubvolume(ctx context.Context, id string, fs string, in SubvolumeCreateInput) error
	DeleteSubvolume(ctx context.Context, id string, fs string, name string) error
}

// Option 配置 New(测试注入 fake Runner 等)。
type Option func(*service)

// WithRunner 覆盖 SSH 执行器(单测注入 fake,不触网)。
func WithRunner(r Runner) Option { return func(s *service) { s.runner = r } }

// service 是 store + runner + vault 支撑的 Service 实现。
type service struct {
	db      *sql.DB
	servers target.Service
	vault   vault.Vault
	runner  Runner

	mu      sync.Mutex // 守护 jobs / stCache
	jobs    map[string]*deployJob
	stCache map[string]cachedStatus
}

type cachedStatus struct {
	at  time.Time
	val ClusterStatus
}

// New 构造 Service。runner 缺省为基于 target.Service 的 sshRunner(提权探测 + sudo -n 前缀)。
func New(db *sql.DB, servers target.Service, vlt vault.Vault, opts ...Option) Service {
	s := &service{
		db:      db,
		servers: servers,
		vault:   vlt,
		runner:  newSSHRunner(servers),
		jobs:    map[string]*deployJob{},
		stCache: map[string]cachedStatus{},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ── 集群 CRUD ─────────────────────────────────────────────────────────────────

// CreateCluster 校验入参后落库(集群 + 主机意图 + OSD 意图);不在此触网。
func (s *service) CreateCluster(ctx context.Context, in CreateInput) (*Cluster, error) {
	in.Name = strings.TrimSpace(in.Name)
	if !nameRe.MatchString(in.Name) {
		return nil, fmt.Errorf("%w: 集群名仅允许字母数字开头,含字母数字/点/横线/下划线,≤64 字符", ErrInvalidInput)
	}
	if in.BootstrapServerID == "" {
		return nil, fmt.Errorf("%w: 未指定 bootstrap 主机", ErrInvalidInput)
	}
	if len(in.Hosts) == 0 {
		return nil, fmt.Errorf("%w: 至少登记一台主机", ErrInvalidInput)
	}
	bootstrapSeen := false
	seen := map[string]bool{}
	for i := range in.Hosts {
		h := &in.Hosts[i]
		if h.ServerID == "" || seen[h.ServerID] {
			return nil, fmt.Errorf("%w: 主机列表含空/重复 serverId", ErrInvalidInput)
		}
		seen[h.ServerID] = true
		if h.ServerID == in.BootstrapServerID {
			bootstrapSeen = true
		}
		for _, r := range h.Roles {
			if !allowedRoles[r] {
				return nil, fmt.Errorf("%w: 非法角色 %q(仅 mon/mgr/osd)", ErrInvalidInput, r)
			}
		}
		for _, l := range h.Labels {
			if !labelRe.MatchString(l) {
				return nil, fmt.Errorf("%w: 非法标签 %q", ErrInvalidInput, l)
			}
		}
		for _, d := range h.Devices {
			if !deviceRe.MatchString(d) {
				return nil, fmt.Errorf("%w: 非法设备路径 %q(须为 /dev/ 绝对路径)", ErrInvalidInput, d)
			}
		}
	}
	if !bootstrapSeen {
		return nil, fmt.Errorf("%w: bootstrap 主机不在主机列表内", ErrInvalidInput)
	}
	for _, nw := range []string{in.PublicNetwork, in.ClusterNetwork} {
		if nw != "" && !cidrRe.MatchString(nw) {
			return nil, fmt.Errorf("%w: 网段须为 CIDR 形式(如 10.0.0.0/24)", ErrInvalidInput)
		}
	}
	in.Image = strings.TrimSpace(in.Image)
	if in.Image == "" {
		in.Image = DefaultImage
	}
	in.CephadmSource = normalizeCephadmSource(in.CephadmSource)

	// serverId 引用存在性校验(防悬挂引用;不触网只查登记)。
	for i := range in.Hosts {
		if _, err := s.servers.Get(ctx, in.Hosts[i].ServerID); err != nil {
			return nil, fmt.Errorf("%w: 主机 %s 未登记(target: %v)", ErrInvalidInput, in.Hosts[i].ServerID, err)
		}
	}

	now := time.Now().UTC()
	cl := &Cluster{
		ID:                uuid.NewString(),
		Name:              in.Name,
		BootstrapServerID: in.BootstrapServerID,
		Image:             in.Image,
		CephadmSource:     in.CephadmSource,
		PublicNetwork:     in.PublicNetwork,
		ClusterNetwork:    in.ClusterNetwork,
		State:             StateCreated,
		DashboardUser:     DefaultDashboardUser,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := s.insertCluster(ctx, cl, in.Hosts); err != nil {
		return nil, err
	}
	return s.GetCluster(ctx, cl.ID)
}

// normalizeCephadmSource 归一安装来源:auto/package/留空 → auto;curl:<url> 原样;其余报错。
func normalizeCephadmSource(src string) string {
	src = strings.TrimSpace(src)
	switch {
	case src == "" || src == "auto" || src == "package":
		return src
	case strings.HasPrefix(src, "curl:"):
		return src
	default:
		return "auto"
	}
}

// cephadmCurlURL 返回 curl 安装来源的 cephadm 脚本地址(空 = 非 curl 来源)。
func cephadmCurlURL(src string) string {
	if u := strings.TrimPrefix(src, "curl:"); u != "" && u != src {
		return strings.TrimRight(u, "/")
	}
	return ""
}

// DeleteCluster 删除登记(集群 + 主机/OSD 行);部署任务进行中时拒绝。
func (s *service) DeleteCluster(ctx context.Context, id string) error {
	s.mu.Lock()
	if j := s.jobs[id]; j != nil && j.snapshot().Running {
		s.mu.Unlock()
		return ErrBusy
	}
	s.mu.Unlock()
	return s.deleteCluster(ctx, id)
}

// ── 状态采集(TTL 缓存)───────────────────────────────────────────────────────

const statusTTL = 15 * time.Second

// ClusterStatus 经 bootstrap 主机跑 ceph -s / health detail 并解析(15s TTL 缓存)。
func (s *service) ClusterStatus(ctx context.Context, id string) (ClusterStatus, error) {
	s.mu.Lock()
	if c, ok := s.stCache[id]; ok && time.Since(c.at) < statusTTL {
		s.mu.Unlock()
		return c.val, nil
	}
	s.mu.Unlock()

	cl, err := s.readyCluster(ctx, id)
	if err != nil {
		return ClusterStatus{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	res, err := s.runner.Run(ctx, cl.BootstrapServerID, []string{"ceph", "-s", "-f", "json"})
	if err != nil {
		return ClusterStatus{}, err
	}
	st := parseClusterStatus(res.Stdout)
	// 健康明细尽力而为(失败不影响状态主体)。
	if res2, err := s.runner.Run(ctx, cl.BootstrapServerID, []string{"ceph", "health", "detail", "-f", "json"}); err == nil {
		st.HealthDetail = parseHealthDetail(res2.Stdout)
	}
	s.mu.Lock()
	s.stCache[id] = cachedStatus{at: time.Now(), val: st}
	s.mu.Unlock()
	return st, nil
}

// readyCluster 返回集群并校验已就绪(管理/状态操作的前置)。
func (s *service) readyCluster(ctx context.Context, id string) (*Cluster, error) {
	cl, err := s.GetCluster(ctx, id)
	if err != nil {
		return nil, err
	}
	if cl.State != StateReady {
		return nil, fmt.Errorf("%w: 集群 %s 当前状态 %s(须为 ready)", ErrClusterNotReady, cl.Name, cl.State)
	}
	return cl, nil
}

// run 在 bootstrap 主机执行 cmd(argv),非零退出 → ErrExec(stderr 尾部随错误)。
func (s *service) run(ctx context.Context, cl *Cluster, cmd []string) (*target.ExecResult, error) {
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	return s.runner.Run(ctx, cl.BootstrapServerID, cmd)
}

// runOut 在 bootstrap 主机执行 cmd 并返回整理后的 stdout(解析器统一吃字符串,
// 宽容处理非法 JSON;失败仍走 ErrExec)。
func (s *service) runOut(ctx context.Context, cl *Cluster, cmd []string) (string, error) {
	res, err := s.run(ctx, cl, cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// randPassword 生成 n 位字母数字口令(crypto/rand;绝不入日志)。
func randPassword(n int) string {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, n)
	for i := range b {
		v, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			b[i] = alphabet[i%len(alphabet)]
			continue
		}
		b[i] = alphabet[v.Int64()]
	}
	return string(b)
}

// formatBytes 把字节数格式化为人类可读(集群卡片/日志用)。
func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatUint(n, 10) + " B"
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
