// Package diskadmin 是「服务器磁盘管理」的领域层(块设备/LVM 拓扑发现 + 分区/格式化/
// 挂载/扩缩容/LVM 变更)。
//
// 所有操作经 target.Service 以 SSH 在登记服务器上执行:命令一律 []string argv
// (AC-SEC-02),提权仅 root / 免密 sudo(sudo -n,探测结果按服务器缓存 10 分钟,同
// ceph/exec.go 先例)。变更操作同步执行,执行前实时重查拓扑做守卫(不信任请求时的
// 快照,防 TOCTOU)。
//
// 只读纪律(对齐 redisadmin):每台服务器默认只读(拓扑可浏览),写操作在领域层拒绝
// (ErrReadonly);解锁是显式动作(30 分钟 TTL,进程内状态,重启自动回锁),由 HTTP
// 层留审计。危险操作(格式化/删除/缩容/移除)要求 confirm 字段逐字等于目标路径。
//
// 安全纪律:
//   - 挂载 /、/boot、/usr、/var、/etc 等关键目录或 swap 的分区/LV 标记 protected,
//     其所属 VG、PV 与整盘一并 protected,一切写操作拒绝(系统盘保护);
//   - fstab 持久化走「临时文件 → findmnt --verify 校验 → 备份 → 原子替换」,校验不过
//     绝不触碰 /etc/fstab;
//   - 文件系统能力矩阵:ext4 在线扩容/离线缩容(已挂载拒绝缩容,由用户先自行卸载);
//     XFS 在线扩容/不可缩容;Btrfs 在线扩容/在线缩容。
package diskadmin

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/huangchengsir/pipewright/internal/target"
)

// 领域错误(人话由 httpapi 层映射状态码;错误体不含凭据,可带远端 stderr 尾部)。
var (
	// ErrInvalidInput 表示入参非法(路径不合规/数值越界/动作未知等)。
	ErrInvalidInput = errors.New("diskadmin: invalid input")
	// ErrServerNotFound 表示目标服务器不存在。
	ErrServerNotFound = errors.New("diskadmin: server not found")
	// ErrReadonly 表示该服务器处于只读模式,写操作被拒绝。
	ErrReadonly = errors.New("diskadmin: server is read-only")
	// ErrNoPrivilege 表示既非 root 登录也无免密 sudo。
	ErrNoPrivilege = errors.New("diskadmin: no privilege on host")
	// ErrExec 表示远端命令执行失败(非零退出/不可达;错误体带 stderr 尾部)。
	ErrExec = errors.New("diskadmin: remote command failed")
	// ErrDeviceNotFound 表示目标块设备/LVM 对象在拓扑中不可见。
	ErrDeviceNotFound = errors.New("diskadmin: device not found")
	// ErrDeviceBusy 表示设备已挂载(或挂载点被占用),当前操作要求相反状态。
	ErrDeviceBusy = errors.New("diskadmin: device busy")
	// ErrMountRequired 表示该操作要求文件系统处于已挂载状态(xfs/btrfs 扩缩容)。
	ErrMountRequired = errors.New("diskadmin: filesystem must be mounted")
	// ErrAlreadyMounted 表示挂载时目标已处于挂载态。
	ErrAlreadyMounted = errors.New("diskadmin: already mounted")
	// ErrMountpointTaken 表示挂载点上已有别的文件系统。
	ErrMountpointTaken = errors.New("diskadmin: mountpoint in use")
	// ErrProtectedDevice 表示目标是受保护的系统设备(关键挂载/swap/根 VG)。
	ErrProtectedDevice = errors.New("diskadmin: protected system device")
	// ErrFSTypeUnsupported 表示该文件系统不支持此操作(如 XFS 缩容)。
	ErrFSTypeUnsupported = errors.New("diskadmin: filesystem type unsupported")
	// ErrNotLastPartition 表示分区不是盘上最后一个(扩容仅支持末分区)。
	ErrNotLastPartition = errors.New("diskadmin: not the last partition")
	// ErrConfirmMismatch 表示危险操作的确认文本与目标不一致。
	ErrConfirmMismatch = errors.New("diskadmin: confirm text mismatch")
	// ErrLvmMissing 表示远端未安装 lvm2。
	ErrLvmMissing = errors.New("diskadmin: lvm2 not installed")
	// ErrFstabVerify 表示 fstab 校验未通过(未提交,原文件未动)。
	ErrFstabVerify = errors.New("diskadmin: fstab verify failed")
)

// 时限与解锁 TTL。
const (
	// opTimeout 常规变更命令(分区/格式化/挂载/LVM 创建)的单命令时限。
	opTimeout = 5 * time.Minute
	// fsTimeout 文件系统级长操作(e2fsck/resize2fs/btrfs resize/lvreduce)时限:
	// 多 TB 盘的强制检查可长达数分钟,中断 resize2fs 有致 fs 不一致的风险,给足余量。
	fsTimeout = 60 * time.Minute
	// UnlockTTL 解锁有效期(到时自动回锁;再次解锁即续期)。进程内状态,重启回锁。
	UnlockTTL = 30 * time.Minute
)

// 入参白名单。
var (
	// devicePathRe 块设备/PV/LV 路径白名单(/dev/ 前缀绝对路径)。
	devicePathRe = regexp.MustCompile(`^/dev/[a-zA-Z0-9/_.-]+$`)
	// mountpointRe 挂载点路径白名单(绝对路径,受限字符集)。
	mountpointRe = regexp.MustCompile(`^/[a-zA-Z0-9/_.-]{0,120}$`)
	// mountOptsRe mount 选项白名单(defaults,noatime 等逗号键值)。
	mountOptsRe = regexp.MustCompile(`^[a-zA-Z0-9,=_.-]{1,64}$`)
	// fsLabelRe 文件系统卷标白名单(XFS 上限 12 字符,取最严)。
	fsLabelRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9 _.-]{0,11}$`)
	// lvmNameRe VG/LV 名白名单(与 ceph nameRe 同形)。
	lvmNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
)

// criticalMountPrefixes 关键挂载目录:挂在其上(或其子目录)的分区/LV 受保护;
// 新挂载也不允许指向它们(避免遮蔽系统内容)。"/" 精确匹配(否则一切路径都命中)。
var criticalMountPrefixes = []string{
	"/boot", "/etc", "/usr", "/var", "/bin", "/sbin", "/lib", "/lib32",
	"/lib64", "/libx32", "/dev", "/proc", "/sys", "/run",
}

// isCriticalMount 报告挂载点是否承载系统内容("/" 精确;其余含子目录前缀)。
func isCriticalMount(m string) bool {
	if m == "/" {
		return true
	}
	for _, p := range criticalMountPrefixes {
		if m == p || strings.HasPrefix(m, p+"/") {
			return true
		}
	}
	return false
}

// BlockDevice 是 lsblk 树上的一个块设备(整盘/分区/LVM/加密层;宽容解析,采不到的
// 字段为零值)。
type BlockDevice struct {
	Path        string         `json:"path"`
	Name        string         `json:"name"`
	Type        string         `json:"type"` // disk|part|lvm|crypt|rom|loop
	SizeBytes   uint64         `json:"sizeBytes"`
	FSType      string         `json:"fstype"`
	Mountpoints []string       `json:"mountpoints"`
	UUID        string         `json:"uuid"`
	PARTUUID    string         `json:"partuuid"`
	Serial      string         `json:"serial"`
	Model       string         `json:"model"`
	ReadOnly    bool           `json:"readOnly"`
	PTType      string         `json:"pttype"` // gpt|dos(整盘的分区表类型;分区为空)
	Parent      string         `json:"parent"` // 父设备内核名(lsblk PKNAME;顶层为空)
	Protected   bool           `json:"protected"`
	UsedBytes   uint64         `json:"usedBytes"` // df 口径已用(未挂载为 0)
	Children    []*BlockDevice `json:"children,omitempty"`
}

// PhysicalVolume 是 pvs 报告的一个 PV。
type PhysicalVolume struct {
	Path      string `json:"path"`
	VGName    string `json:"vgName"`
	SizeBytes uint64 `json:"sizeBytes"`
	FreeBytes uint64 `json:"freeBytes"`
	Protected bool   `json:"protected"`
}

// VolumeGroup 是 vgs 报告的一个 VG。
type VolumeGroup struct {
	Name      string `json:"name"`
	SizeBytes uint64 `json:"sizeBytes"`
	FreeBytes uint64 `json:"freeBytes"`
	PVCount   int    `json:"pvCount"`
	LVCount   int    `json:"lvCount"`
	Protected bool   `json:"protected"`
}

// LogicalVolume 是 lvs 报告的一个 LV(fs/挂载/用量经设备树匹配回填)。
type LogicalVolume struct {
	Path        string   `json:"path"` // /dev/<vg>/<lv>
	VGName      string   `json:"vgName"`
	Name        string   `json:"name"`
	SizeBytes   uint64   `json:"sizeBytes"`
	Attr        string   `json:"attr"`
	FSType      string   `json:"fstype"`
	Mountpoints []string `json:"mountpoints"`
	UsedBytes   uint64   `json:"usedBytes"`
	Active      bool     `json:"active"`
	Protected   bool     `json:"protected"`
}

// ToolStatus 是远端工具可用性(command -v 探测;前端据此禁用对应操作)。
type ToolStatus struct {
	Parted    bool `json:"parted"`
	Partx     bool `json:"partx"`
	MkfsExt4  bool `json:"mkfsExt4"`
	MkfsXfs   bool `json:"mkfsXfs"`
	MkfsBtrfs bool `json:"mkfsBtrfs"`
	E2fsck    bool `json:"e2fsck"`
	Resize2fs bool `json:"resize2fs"`
	XfsGrowfs bool `json:"xfsGrowfs"`
	Btrfs     bool `json:"btrfs"`
	LVM       bool `json:"lvm"`
	Findmnt   bool `json:"findmnt"`
}

// Topology 是一台服务器的一次性存储拓扑快照(单次 SSH 合并采集)。
type Topology struct {
	Devices         []*BlockDevice   `json:"devices"`
	PVs             []PhysicalVolume `json:"pvs"`
	VGs             []VolumeGroup    `json:"vgs"`
	LVs             []LogicalVolume  `json:"lvs"`
	Tools           ToolStatus       `json:"tools"`
	LoginUser       string           `json:"loginUser"`
	Privileged      bool             `json:"privileged"` // root 或免密 sudo 可用
	Unlocked        bool             `json:"unlocked"`
	UnlockExpiresAt string           `json:"unlockExpiresAt,omitempty"` // RFC3339;空 = 未解锁
}

// UnlockState 是解锁闸门的当前状态。
type UnlockState struct {
	Unlocked  bool
	ExpiresAt time.Time
}

// ── 操作入参 ─────────────────────────────────────────────────────────────────

// PartitionCreateInput 建分区入参(首期仅支持空盘创建首个分区;sizeSpec "100%" 或
// "<N>GiB";盘无分区表时 MakeLabel 须显式 true——创建 GPT 标签会清空整盘)。
type PartitionCreateInput struct {
	Device    string
	SizeSpec  string // "100%" | "<N>GiB"(N≥1)
	MakeLabel bool
}

// PartitionDeleteInput 删分区入参。
type PartitionDeleteInput struct {
	Partition string // 如 /dev/sdb1
	Confirm   string // 须逐字等于 Partition
}

// ShrinkInput 缩容入参(分区或 LV 共用;TargetGiB 为缩容后的最终容量)。
type ShrinkInput struct {
	Target    string
	TargetGiB int
	Confirm   string // 须逐字等于 Target
}

// FormatInput 格式化入参。
type FormatInput struct {
	Target  string
	FSType  string // ext4|xfs|btrfs
	Label   string
	Confirm string // 须逐字等于 Target
}

// MountInput 挂载入参。
type MountInput struct {
	Target     string
	Mountpoint string
	Options    string // 空 = defaults
}

// UnmountInput 卸载入参。
type UnmountInput struct {
	Mountpoint string
	RemoveDir  bool
}

// VGCreateInput 建 VG 入参(devices 须均为未入组的 PV)。
type VGCreateInput struct {
	Name    string
	Devices []string
}

// VGExtendInput VG 加盘入参。
type VGExtendInput struct {
	Name   string
	Device string
}

// VGReduceInput VG 摘盘入参(PV 上须无已分配 extent;本功能不做 pvmove)。
type VGReduceInput struct {
	Name    string
	Device  string
	Confirm string // 须逐字等于 Device
}

// RemoveConfirmInput 按「名字确认」的移除入参(VG 移除用)。
type RemoveConfirmInput struct {
	Name    string
	Confirm string // 须逐字等于 Name
}

// LVCreateInput 建 LV 入参(SizeGiB / UseAllFree 二选一;可选创建后直接格式化挂载)。
type LVCreateInput struct {
	VG         string
	Name       string
	SizeGiB    int
	UseAllFree bool
	FSType     string // 可选:ext4|xfs|btrfs
	Mountpoint string // 可选(FSType 非空时生效)
}

// LVGrowInput LV 扩容入参(SizeGiB 为增量;UseAllFree = 用尽 VG 剩余)。
type LVGrowInput struct {
	Path       string // /dev/<vg>/<lv>
	SizeGiB    int
	UseAllFree bool
}

// RemoveTargetInput 按「路径确认」的移除入参(PV/LV 移除用)。
type RemoveTargetInput struct {
	Target  string
	Confirm string // 须逐字等于 Target
}

// Service 是磁盘管理领域接口(全部操作即时经 SSH 执行;写操作要求先 Unlock)。
type Service interface {
	// Topology 实时采集存储拓扑(要求可提权;lsblk 全量 + LVM 视图 + 工具探测 +
	// 解锁状态)。
	Topology(ctx context.Context, serverID string) (*Topology, error)
	// Unlock 解锁写模式(30 分钟 TTL,续期式)。
	Unlock(serverID string) UnlockState
	// Lock 立即回锁(幂等)。
	Lock(serverID string) UnlockState

	// ── 分区 ──
	CreatePartition(ctx context.Context, serverID string, in PartitionCreateInput) error
	DeletePartition(ctx context.Context, serverID string, in PartitionDeleteInput) error
	// GrowPartition 把末分区扩到盘尾并按文件系统类型在线扩容。
	GrowPartition(ctx context.Context, serverID string, target string) error
	// ShrinkPartition 缩分区(ext4 须未挂载;btrfs 须已挂载;xfs 不支持)。
	ShrinkPartition(ctx context.Context, serverID string, in ShrinkInput) error

	// ── 文件系统 ──
	Format(ctx context.Context, serverID string, in FormatInput) error
	Mount(ctx context.Context, serverID string, in MountInput) error
	Unmount(ctx context.Context, serverID string, in UnmountInput) error

	// ── LVM ──
	PVCreate(ctx context.Context, serverID string, device string) error
	PVRemove(ctx context.Context, serverID string, in RemoveTargetInput) error
	VGCreate(ctx context.Context, serverID string, in VGCreateInput) error
	VGExtend(ctx context.Context, serverID string, in VGExtendInput) error
	VGReduce(ctx context.Context, serverID string, in VGReduceInput) error
	VGRemove(ctx context.Context, serverID string, in RemoveConfirmInput) error
	LVCreate(ctx context.Context, serverID string, in LVCreateInput) error
	LVGrow(ctx context.Context, serverID string, in LVGrowInput) error
	LVShrink(ctx context.Context, serverID string, in ShrinkInput) error
	LVRemove(ctx context.Context, serverID string, in RemoveTargetInput) error
}

// Option 配置 New(测试注入 fake Runner 等)。
type Option func(*service)

// WithRunner 覆盖 SSH 执行器(单测注入 fake,不触网)。
func WithRunner(r Runner) Option { return func(s *service) { s.runner = r } }

// service 是 runner + 解锁闸门支撑的 Service 实现(操作无状态,无持久层)。
type service struct {
	servers target.Service
	runner  Runner

	mu       sync.Mutex
	unlocked map[string]time.Time // serverID → 解锁到期时刻
}

// New 构造磁盘管理服务。
func New(servers target.Service, opts ...Option) Service {
	s := &service{
		servers:  servers,
		runner:   newSSHRunner(servers),
		unlocked: map[string]time.Time{},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ── 解锁闸门 ─────────────────────────────────────────────────────────────────

// Unlock 解锁(续期式)。
func (s *service) Unlock(serverID string) UnlockState {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp := time.Now().Add(UnlockTTL)
	s.unlocked[serverID] = exp
	return UnlockState{Unlocked: true, ExpiresAt: exp}
}

// Lock 立即回锁(幂等)。
func (s *service) Lock(serverID string) UnlockState {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.unlocked, serverID)
	return UnlockState{}
}

// unlockState 返回当前闸门状态(过期视同未解锁)。
func (s *service) unlockState(serverID string) UnlockState {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.unlocked[serverID]
	if !ok || time.Now().After(exp) {
		return UnlockState{}
	}
	return UnlockState{Unlocked: true, ExpiresAt: exp}
}

// requireUnlocked 写操作前置检查。
func (s *service) requireUnlocked(serverID string) error {
	if st := s.unlockState(serverID); !st.Unlocked {
		return fmt.Errorf("%w: 服务器 %s 未解锁写模式", ErrReadonly, serverID)
	}
	return nil
}

// ── 拓扑查询辅助 ─────────────────────────────────────────────────────────────

// findDevice 按路径在设备树中查找(递归)。
func (t *Topology) findDevice(path string) *BlockDevice {
	for _, d := range t.Devices {
		if found := findDeviceIn(d, path); found != nil {
			return found
		}
	}
	return nil
}

func findDeviceIn(d *BlockDevice, path string) *BlockDevice {
	if d.Path == path {
		return d
	}
	for _, c := range d.Children {
		if found := findDeviceIn(c, path); found != nil {
			return found
		}
	}
	return nil
}

// locate 返回设备及其父设备(分区守卫要用父盘路径;顶层父为 nil)。
func (t *Topology) locate(path string) (*BlockDevice, *BlockDevice) {
	for _, d := range t.Devices {
		if found, parent, ok := locateIn(d, nil, path); ok {
			return found, parent
		}
	}
	return nil, nil
}

func locateIn(d, parent *BlockDevice, path string) (*BlockDevice, *BlockDevice, bool) {
	if d.Path == path {
		return d, parent, true
	}
	for _, c := range d.Children {
		if f, p, ok := locateIn(c, d, path); ok {
			return f, p, true
		}
	}
	return nil, nil, false
}

// findVG / findPV / findLV 按键查找 LVM 对象。
func (t *Topology) findVG(name string) *VolumeGroup {
	for i := range t.VGs {
		if t.VGs[i].Name == name {
			return &t.VGs[i]
		}
	}
	return nil
}

func (t *Topology) findPV(path string) *PhysicalVolume {
	for i := range t.PVs {
		if t.PVs[i].Path == path {
			return &t.PVs[i]
		}
	}
	return nil
}

func (t *Topology) findLV(path string) *LogicalVolume {
	for i := range t.LVs {
		if t.LVs[i].Path == path {
			return &t.LVs[i]
		}
	}
	return nil
}

// mountpointInUse 报告挂载点是否已被任何设备(树节点或 LV)占用。
func (t *Topology) mountpointInUse(mnt string) bool {
	if mnt == "" {
		return false
	}
	var hit bool
	walkDevices(t.Devices, func(d *BlockDevice) {
		for _, m := range d.Mountpoints {
			if m == mnt {
				hit = true
			}
		}
	})
	return hit
}

// walkDevices 先序遍历设备树。
func walkDevices(devs []*BlockDevice, fn func(*BlockDevice)) {
	for _, d := range devs {
		fn(d)
		walkDevices(d.Children, fn)
	}
}

// partitionNumber 从设备内核名推导分区号(如 sdb3→3、nvme0n1p2→2)。
func partitionNumber(d *BlockDevice) (int, error) {
	name := d.Name
	if d.Parent != "" && strings.HasPrefix(name, d.Parent) {
		suffix := name[len(d.Parent):]
		// nvme/md 系列分区名带 p 分隔(nvme0n1p2);去 p 时要求其后是数字,防误伤。
		if len(suffix) > 1 && suffix[0] == 'p' && suffix[1] >= '0' && suffix[1] <= '9' {
			suffix = suffix[1:]
		}
		if n, err := strconv.Atoi(suffix); err == nil && n > 0 {
			return n, nil
		}
	}
	return 0, fmt.Errorf("%w: 无法从 %s 推导分区号", ErrInvalidInput, d.Path)
}
