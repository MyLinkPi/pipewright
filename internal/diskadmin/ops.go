// ops.go — 分区/格式化/挂载/扩缩容操作。
//
// 统一纪律:每个写操作先过解锁闸门 → 校验入参(白名单正则)→ 实时重查拓扑做守卫
// (不信任请求时的快照,防 TOCTOU)→ argv 执行 → 返回。parted 一律 -s 脚本模式;
// 分区表变更后 partx -u 通知内核并轮询新节点出现。
package diskadmin

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// run 是 runner.Run 的薄封装(错误已在 runner 层包装)。
func (s *service) run(ctx context.Context, serverID string, cmd []string) error {
	_, err := s.runner.Run(ctx, serverID, cmd)
	return err
}

// fsCtx 返回文件系统级长操作的 ctx(e2fsck/resize2fs 等)。
func fsCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, fsTimeout)
}

// firstMount 取第一个挂载点(空 = 未挂载)。
func firstMount(mounts []string) string {
	if len(mounts) == 0 {
		return ""
	}
	return mounts[0]
}

// parseSizeSpec 校验并规范化分区大小入参("100%" 或 "<N>GiB",N≥1)。
func parseSizeSpec(spec string) (string, error) {
	if spec == "100%" {
		return "100%", nil
	}
	if strings.HasSuffix(spec, "GiB") {
		if n, err := strconv.Atoi(strings.TrimSuffix(spec, "GiB")); err == nil && n >= 1 && n <= 1024*1024 {
			return strconv.Itoa(n) + "GiB", nil
		}
	}
	return "", fmt.Errorf("%w: 分区大小须为 100%% 或 <N>GiB(N≥1)", ErrInvalidInput)
}

// resolveDevice 按路径解析树节点(LV 的 /dev/<vg>/<lv> 路径经 /dev/mapper 名兜底)。
func (t *Topology) resolveDevice(path string) *BlockDevice {
	if d := t.findDevice(path); d != nil {
		return d
	}
	if rest := strings.TrimPrefix(path, "/dev/"); rest != path {
		if parts := strings.SplitN(rest, "/", 2); len(parts) == 2 {
			return t.findDevice(dmName(parts[0], parts[1]))
		}
	}
	return nil
}

// waitChildrenAppear 轮询等待整盘下出现 ≥want 个分区节点(内核 udev 建节点可能滞后;
// 约 10 秒;超时返回 false,调用方给出人话提示)。
func (s *service) waitChildrenAppear(ctx context.Context, serverID, diskPath string, want int) bool {
	for i := 0; i < 5; i++ {
		if topo, err := s.collect(ctx, serverID); err == nil {
			if d := topo.findDevice(diskPath); d != nil && len(d.Children) >= want {
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
	return false
}

// ── 分区 ─────────────────────────────────────────────────────────────────────

// CreatePartition 在空盘上创建首个分区(首期范围:无既有分区;可选同时建 GPT 标签)。
func (s *service) CreatePartition(ctx context.Context, serverID string, in PartitionCreateInput) error {
	if err := s.requireUnlocked(serverID); err != nil {
		return err
	}
	if !devicePathRe.MatchString(in.Device) {
		return fmt.Errorf("%w: 设备路径非法", ErrInvalidInput)
	}
	endSpec, err := parseSizeSpec(in.SizeSpec)
	if err != nil {
		return err
	}
	octx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	topo, err := s.collect(octx, serverID)
	if err != nil {
		return err
	}
	if !topo.Tools.Parted {
		return fmt.Errorf("%w: 远端缺少 parted", ErrInvalidInput)
	}
	dev := topo.findDevice(in.Device)
	if dev == nil || dev.Type != "disk" {
		return fmt.Errorf("%w: %s 不是整盘设备", ErrDeviceNotFound, in.Device)
	}
	if dev.Protected {
		return fmt.Errorf("%w: %s 承载系统内容(关键挂载/swap/根 VG)", ErrProtectedDevice, in.Device)
	}
	if topo.findPV(in.Device) != nil {
		return fmt.Errorf("%w: %s 已是 LVM PV,须先 pvremove", ErrInvalidInput, in.Device)
	}
	if len(dev.Children) > 0 {
		return fmt.Errorf("%w: %s 上已有分区(首期仅支持空盘创建首个分区)", ErrInvalidInput, in.Device)
	}
	hasLabel := dev.PTType != ""
	if hasLabel && in.MakeLabel {
		return fmt.Errorf("%w: %s 已有分区表(%s),拒绝重建标签", ErrInvalidInput, in.Device, dev.PTType)
	}
	if !hasLabel && !in.MakeLabel {
		return fmt.Errorf("%w: %s 无分区表;须显式确认创建 GPT 标签(将清空整盘)", ErrInvalidInput, in.Device)
	}
	if !hasLabel {
		if err := s.run(octx, serverID, []string{"parted", "-s", in.Device, "--", "mklabel", "gpt"}); err != nil {
			return err
		}
	}
	if err := s.run(octx, serverID, []string{"parted", "-s", in.Device, "--", "mkpart", "primary", "1MiB", endSpec}); err != nil {
		return err
	}
	if topo.Tools.Partx {
		_ = s.run(octx, serverID, []string{"partx", "-u", in.Device})
	}
	if !s.waitChildrenAppear(octx, serverID, in.Device, 1) {
		return fmt.Errorf("%w: 分区表已写入,但新分区节点约 10 秒内未出现;请稍后刷新确认", ErrExec)
	}
	return nil
}

// DeletePartition 删除分区(须未挂载、非 PV、非系统分区;confirm 逐字确认)。
func (s *service) DeletePartition(ctx context.Context, serverID string, in PartitionDeleteInput) error {
	if err := s.requireUnlocked(serverID); err != nil {
		return err
	}
	if !devicePathRe.MatchString(in.Partition) {
		return fmt.Errorf("%w: 分区路径非法", ErrInvalidInput)
	}
	if in.Confirm != in.Partition {
		return fmt.Errorf("%w: 须逐字输入 %s 确认", ErrConfirmMismatch, in.Partition)
	}
	octx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	topo, err := s.collect(octx, serverID)
	if err != nil {
		return err
	}
	if !topo.Tools.Parted {
		return fmt.Errorf("%w: 远端缺少 parted", ErrInvalidInput)
	}
	dev, parent := topo.locate(in.Partition)
	if dev == nil || dev.Type != "part" {
		return fmt.Errorf("%w: %s 不是分区", ErrDeviceNotFound, in.Partition)
	}
	if parent == nil {
		return fmt.Errorf("%w: 无法定位 %s 的父盘", ErrInvalidInput, in.Partition)
	}
	if dev.Protected {
		return fmt.Errorf("%w: %s 是系统分区(关键挂载/swap)", ErrProtectedDevice, in.Partition)
	}
	if len(dev.Mountpoints) > 0 {
		return fmt.Errorf("%w: %s 已挂载(%s),须先卸载", ErrDeviceBusy, in.Partition, strings.Join(dev.Mountpoints, ","))
	}
	if topo.findPV(in.Partition) != nil {
		return fmt.Errorf("%w: %s 是 LVM PV,须先 pvremove", ErrInvalidInput, in.Partition)
	}
	num, err := partitionNumber(dev)
	if err != nil {
		return err
	}
	if err := s.run(octx, serverID, []string{"parted", "-s", parent.Path, "--", "rm", strconv.Itoa(num)}); err != nil {
		return err
	}
	if topo.Tools.Partx {
		_ = s.run(octx, serverID, []string{"partx", "-u", parent.Path})
	}
	return nil
}

// GrowPartition 把末分区扩到盘尾并按文件系统类型扩容(xfs/btrfs 须已挂载)。
func (s *service) GrowPartition(ctx context.Context, serverID string, target string) error {
	if err := s.requireUnlocked(serverID); err != nil {
		return err
	}
	if !devicePathRe.MatchString(target) {
		return fmt.Errorf("%w: 目标路径非法", ErrInvalidInput)
	}
	octx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	topo, err := s.collect(octx, serverID)
	if err != nil {
		return err
	}
	if !topo.Tools.Parted {
		return fmt.Errorf("%w: 远端缺少 parted", ErrInvalidInput)
	}
	dev, parent := topo.locate(target)
	if dev == nil || dev.Type != "part" {
		return fmt.Errorf("%w: %s 不是分区", ErrDeviceNotFound, target)
	}
	if parent == nil || len(parent.Children) == 0 || parent.Children[len(parent.Children)-1].Path != dev.Path {
		return fmt.Errorf("%w: %s 不是盘上最后一个分区(扩容仅支持末分区)", ErrNotLastPartition, target)
	}
	if dev.Protected {
		return fmt.Errorf("%w: %s 是系统分区", ErrProtectedDevice, target)
	}
	num, err := partitionNumber(dev)
	if err != nil {
		return err
	}
	if err := s.run(octx, serverID, []string{"parted", "-s", parent.Path, "--", "resizepart", strconv.Itoa(num), "100%"}); err != nil {
		return err
	}
	if topo.Tools.Partx {
		_ = s.run(octx, serverID, []string{"partx", "-u", parent.Path})
	}
	return s.growFilesystem(ctx, serverID, dev, topo.Tools)
}

// growFilesystem 按文件系统类型执行扩容(分区/LV 共用;无文件系统则仅底层扩完)。
func (s *service) growFilesystem(ctx context.Context, serverID string, dev *BlockDevice, tools ToolStatus) error {
	fctx, cancel := fsCtx(ctx)
	defer cancel()
	switch dev.FSType {
	case "":
		return nil
	case "ext2", "ext3", "ext4":
		if !tools.Resize2fs {
			return fmt.Errorf("%w: 远端缺少 resize2fs", ErrInvalidInput)
		}
		return s.run(fctx, serverID, []string{"resize2fs", dev.Path})
	case "xfs":
		mnt := firstMount(dev.Mountpoints)
		if mnt == "" {
			return fmt.Errorf("%w: xfs 扩容须在挂载状态下进行(xfs_growfs 按挂载点操作)", ErrMountRequired)
		}
		if !tools.XfsGrowfs {
			return fmt.Errorf("%w: 远端缺少 xfs_growfs", ErrInvalidInput)
		}
		return s.run(fctx, serverID, []string{"xfs_growfs", mnt})
	case "btrfs":
		mnt := firstMount(dev.Mountpoints)
		if mnt == "" {
			return fmt.Errorf("%w: btrfs 扩容须在挂载状态下进行", ErrMountRequired)
		}
		if !tools.Btrfs {
			return fmt.Errorf("%w: 远端缺少 btrfs", ErrInvalidInput)
		}
		return s.run(fctx, serverID, []string{"btrfs", "filesystem", "resize", "max", mnt})
	default:
		return fmt.Errorf("%w: %s 暂不支持在线扩容", ErrFSTypeUnsupported, dev.FSType)
	}
}

// ShrinkPartition 缩分区(ext4 须未挂载;btrfs 须已挂载;xfs 不支持)。
func (s *service) ShrinkPartition(ctx context.Context, serverID string, in ShrinkInput) error {
	if err := s.requireUnlocked(serverID); err != nil {
		return err
	}
	if !devicePathRe.MatchString(in.Target) {
		return fmt.Errorf("%w: 目标路径非法", ErrInvalidInput)
	}
	if in.Confirm != in.Target {
		return fmt.Errorf("%w: 须逐字输入 %s 确认", ErrConfirmMismatch, in.Target)
	}
	if in.TargetGiB < 1 {
		return fmt.Errorf("%w: 目标容量须 ≥1GiB", ErrInvalidInput)
	}
	fctx, cancel := fsCtx(ctx)
	defer cancel()
	topo, err := s.collect(fctx, serverID)
	if err != nil {
		return err
	}
	if !topo.Tools.Parted {
		return fmt.Errorf("%w: 远端缺少 parted", ErrInvalidInput)
	}
	dev, parent := topo.locate(in.Target)
	if dev == nil || dev.Type != "part" {
		return fmt.Errorf("%w: %s 不是分区", ErrDeviceNotFound, in.Target)
	}
	if parent == nil {
		return fmt.Errorf("%w: 无法定位 %s 的父盘", ErrInvalidInput, in.Target)
	}
	if dev.Protected {
		return fmt.Errorf("%w: %s 是系统分区", ErrProtectedDevice, in.Target)
	}
	if uint64(in.TargetGiB) >= dev.SizeBytes>>30 {
		return fmt.Errorf("%w: 目标容量须小于当前容量(%dGiB)", ErrInvalidInput, dev.SizeBytes>>30)
	}
	num, err := partitionNumber(dev)
	if err != nil {
		return err
	}
	gibSpec := strconv.Itoa(in.TargetGiB) + "GiB"
	switch dev.FSType {
	case "ext4":
		if mnt := firstMount(dev.Mountpoints); mnt != "" {
			return fmt.Errorf("%w: ext4 缩容须先卸载(当前挂载于 %s),请自行 umount 后重试", ErrDeviceBusy, mnt)
		}
		if !topo.Tools.E2fsck || !topo.Tools.Resize2fs {
			return fmt.Errorf("%w: 远端缺少 e2fsck/resize2fs", ErrInvalidInput)
		}
		// 安全顺序:fsck → fs 缩到目标-1GiB(留余量) → 分区表回缩 → fs 回填到分区大小。
		if err := s.run(fctx, serverID, []string{"e2fsck", "-f", in.Target}); err != nil {
			return err
		}
		if err := s.run(fctx, serverID, []string{"resize2fs", in.Target, strconv.Itoa(in.TargetGiB-1) + "G"}); err != nil {
			return err
		}
		if err := s.run(fctx, serverID, []string{"parted", "-s", parent.Path, "--", "resizepart", strconv.Itoa(num), gibSpec}); err != nil {
			return err
		}
		if topo.Tools.Partx {
			_ = s.run(fctx, serverID, []string{"partx", "-u", parent.Path})
		}
		return s.run(fctx, serverID, []string{"resize2fs", in.Target})
	case "btrfs":
		mnt := firstMount(dev.Mountpoints)
		if mnt == "" {
			return fmt.Errorf("%w: btrfs 缩容须在挂载状态下进行(在线缩容)", ErrMountRequired)
		}
		if !topo.Tools.Btrfs {
			return fmt.Errorf("%w: 远端缺少 btrfs", ErrInvalidInput)
		}
		targetBytes := uint64(in.TargetGiB) << 30
		if dev.UsedBytes > 0 && dev.UsedBytes+512<<20 > targetBytes {
			return fmt.Errorf("%w: 目标容量过小(该文件系统已用约 %dGiB)", ErrInvalidInput, dev.UsedBytes>>30)
		}
		if err := s.run(fctx, serverID, []string{"btrfs", "filesystem", "resize", strconv.FormatUint(targetBytes, 10), mnt}); err != nil {
			return err
		}
		if err := s.run(fctx, serverID, []string{"parted", "-s", parent.Path, "--", "resizepart", strconv.Itoa(num), gibSpec}); err != nil {
			return err
		}
		if topo.Tools.Partx {
			_ = s.run(fctx, serverID, []string{"partx", "-u", parent.Path})
		}
		return s.run(fctx, serverID, []string{"btrfs", "filesystem", "resize", "max", mnt})
	case "xfs":
		return fmt.Errorf("%w: XFS 文件系统不支持缩容(仅 ext4/btrfs)", ErrFSTypeUnsupported)
	default:
		return fmt.Errorf("%w: %s 暂不支持缩容", ErrFSTypeUnsupported, dev.FSType)
	}
}

// ── 格式化 ───────────────────────────────────────────────────────────────────

// Format 格式化分区/整盘/LV(未挂载、非 PV;confirm 逐字确认)。
func (s *service) Format(ctx context.Context, serverID string, in FormatInput) error {
	if err := s.requireUnlocked(serverID); err != nil {
		return err
	}
	if !devicePathRe.MatchString(in.Target) {
		return fmt.Errorf("%w: 目标路径非法", ErrInvalidInput)
	}
	if in.Confirm != in.Target {
		return fmt.Errorf("%w: 须逐字输入 %s 确认", ErrConfirmMismatch, in.Target)
	}
	if in.Label != "" && !fsLabelRe.MatchString(in.Label) {
		return fmt.Errorf("%w: 卷标非法(≤12 字符,字母数字开头)", ErrInvalidInput)
	}
	octx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	topo, err := s.collect(octx, serverID)
	if err != nil {
		return err
	}
	dev := topo.resolveDevice(in.Target)
	var lv *LogicalVolume
	if dev == nil {
		lv = topo.findLV(in.Target)
	}
	if dev == nil && lv == nil {
		return fmt.Errorf("%w: %s 在拓扑中不可见", ErrDeviceNotFound, in.Target)
	}
	if dev != nil {
		if dev.Protected {
			return fmt.Errorf("%w: %s 是系统设备", ErrProtectedDevice, in.Target)
		}
		if len(dev.Mountpoints) > 0 {
			return fmt.Errorf("%w: %s 已挂载,格式化前须先卸载", ErrDeviceBusy, in.Target)
		}
		if dev.FSType == "swap" {
			return fmt.Errorf("%w: swap 分区不在此功能范围", ErrInvalidInput)
		}
		if topo.findPV(in.Target) != nil {
			return fmt.Errorf("%w: %s 是 LVM PV,须先 pvremove", ErrInvalidInput, in.Target)
		}
	} else {
		if lv.Protected {
			return fmt.Errorf("%w: %s 是系统逻辑卷", ErrProtectedDevice, in.Target)
		}
		if len(lv.Mountpoints) > 0 {
			return fmt.Errorf("%w: %s 已挂载,格式化前须先卸载", ErrDeviceBusy, in.Target)
		}
	}
	return s.formatDirect(octx, serverID, in.Target, in.FSType, in.Label, topo.Tools)
}

// formatDirect 直接执行 mkfs(守卫由调用方完成;LVCreate 建卷后直接格式化复用)。
func (s *service) formatDirect(ctx context.Context, serverID, target, fstype, label string, tools ToolStatus) error {
	var cmd []string
	switch fstype {
	case "ext4":
		if !tools.MkfsExt4 {
			return fmt.Errorf("%w: 远端缺少 mkfs.ext4", ErrInvalidInput)
		}
		cmd = []string{"mkfs.ext4", "-F", target}
	case "xfs":
		if !tools.MkfsXfs {
			return fmt.Errorf("%w: 远端缺少 mkfs.xfs", ErrInvalidInput)
		}
		cmd = []string{"mkfs.xfs", "-f", target}
	case "btrfs":
		if !tools.MkfsBtrfs {
			return fmt.Errorf("%w: 远端缺少 mkfs.btrfs", ErrInvalidInput)
		}
		cmd = []string{"mkfs.btrfs", "-f", target}
	default:
		return fmt.Errorf("%w: 文件系统须为 ext4/xfs/btrfs", ErrInvalidInput)
	}
	if label != "" {
		cmd = append(cmd, "-L", label)
	}
	return s.run(ctx, serverID, cmd)
}

// ── 挂载 / 卸载 ─────────────────────────────────────────────────────────────

// validateMountpoint 校验挂载点入参(绝对路径白名单、非关键目录及其子目录、深度)。
func validateMountpoint(mnt string) error {
	if !mountpointRe.MatchString(mnt) {
		return fmt.Errorf("%w: 挂载点须为绝对路径(受限字符集)", ErrInvalidInput)
	}
	if mnt == "/" || isCriticalMount(mnt) {
		return fmt.Errorf("%w: 挂载点落在系统目录(/、/boot、/usr、/var、/etc 等)内,拒绝", ErrInvalidInput)
	}
	if depth := strings.Count(strings.Trim(mnt, "/"), "/"); depth > 8 {
		return fmt.Errorf("%w: 挂载点层级过深", ErrInvalidInput)
	}
	return nil
}

// Mount 挂载分区/整盘/LV 并持久化 fstab(备份-校验-原子替换;校验不过不动 /etc/fstab)。
func (s *service) Mount(ctx context.Context, serverID string, in MountInput) error {
	if err := s.requireUnlocked(serverID); err != nil {
		return err
	}
	if err := validateMountpoint(in.Mountpoint); err != nil {
		return err
	}
	options := in.Options
	if options == "" {
		options = "defaults"
	}
	if !mountOptsRe.MatchString(options) {
		return fmt.Errorf("%w: 挂载选项非法", ErrInvalidInput)
	}
	octx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	topo, err := s.collect(octx, serverID)
	if err != nil {
		return err
	}
	dev := topo.resolveDevice(in.Target)
	var fstype string
	if dev != nil {
		fstype = dev.FSType
		if dev.Protected {
			return fmt.Errorf("%w: %s 是系统设备", ErrProtectedDevice, in.Target)
		}
		if len(dev.Mountpoints) > 0 {
			return fmt.Errorf("%w: %s 已挂载(%s)", ErrAlreadyMounted, in.Target, strings.Join(dev.Mountpoints, ","))
		}
	} else if lv := topo.findLV(in.Target); lv != nil {
		fstype = lv.FSType
		if lv.Protected {
			return fmt.Errorf("%w: %s 是系统逻辑卷", ErrProtectedDevice, in.Target)
		}
		if len(lv.Mountpoints) > 0 {
			return fmt.Errorf("%w: %s 已挂载", ErrAlreadyMounted, in.Target)
		}
	} else {
		return fmt.Errorf("%w: %s 在拓扑中不可见", ErrDeviceNotFound, in.Target)
	}
	if fstype == "" {
		return fmt.Errorf("%w: %s 未格式化,挂载前须先格式化", ErrInvalidInput, in.Target)
	}
	if fstype == "swap" {
		return fmt.Errorf("%w: swap 不在此功能范围", ErrInvalidInput)
	}
	if topo.mountpointInUse(in.Mountpoint) {
		return fmt.Errorf("%w: %s 上已有文件系统挂载", ErrMountpointTaken, in.Mountpoint)
	}
	return s.mountDirect(octx, serverID, in.Target, fstype, in.Mountpoint, options)
}

// mountDirect 执行 mkdir + mount + fstab 持久化(守卫由调用方完成)。
// fstab 持久化失败不影响已生效的挂载,以错误形式告知(重启后不保留)。
func (s *service) mountDirect(ctx context.Context, serverID, target, fstype, mnt, options string) error {
	if err := s.run(ctx, serverID, []string{"mkdir", "-p", mnt}); err != nil {
		return err
	}
	if err := s.run(ctx, serverID, []string{"mount", target, mnt}); err != nil {
		return err
	}
	spec := target
	if res, err := s.runner.Run(ctx, serverID, []string{"blkid", "-s", "UUID", "-o", "value", target}); err == nil {
		if uuid := strings.TrimSpace(res.Stdout); uuid != "" {
			spec = "UUID=" + uuid
		}
	}
	pass := 0
	if strings.HasPrefix(fstype, "ext") {
		pass = 2
	}
	if err := s.fstabAdd(ctx, serverID, spec, mnt, fstype, options, pass); err != nil {
		return fmt.Errorf("已挂载,但 fstab 持久化失败(重启后不保留): %w", err)
	}
	return nil
}

// Unmount 卸载并移除 fstab 条目(系统挂载点拒绝;不做 lazy umount)。
func (s *service) Unmount(ctx context.Context, serverID string, in UnmountInput) error {
	if err := s.requireUnlocked(serverID); err != nil {
		return err
	}
	if !mountpointRe.MatchString(in.Mountpoint) || in.Mountpoint == "/" {
		return fmt.Errorf("%w: 挂载点非法", ErrInvalidInput)
	}
	octx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	topo, err := s.collect(octx, serverID)
	if err != nil {
		return err
	}
	mounted := false
	protected := false
	walkDevices(topo.Devices, func(d *BlockDevice) {
		for _, m := range d.Mountpoints {
			if m == in.Mountpoint {
				mounted = true
				if d.Protected {
					protected = true
				}
			}
		}
	})
	for i := range topo.LVs {
		for _, m := range topo.LVs[i].Mountpoints {
			if m == in.Mountpoint {
				mounted = true
				if topo.LVs[i].Protected {
					protected = true
				}
			}
		}
	}
	if !mounted {
		return fmt.Errorf("%w: %s 未处于挂载状态", ErrInvalidInput, in.Mountpoint)
	}
	if protected {
		return fmt.Errorf("%w: %s 承载系统内容,拒绝卸载", ErrProtectedDevice, in.Mountpoint)
	}
	if err := s.run(octx, serverID, []string{"umount", in.Mountpoint}); err != nil {
		return fmt.Errorf("%w: 卸载失败(挂载点被占用,可用 lsof/fuser 排查)\n%v", ErrDeviceBusy, err)
	}
	if err := s.fstabRemove(octx, serverID, in.Mountpoint); err != nil {
		return fmt.Errorf("已卸载,但移除 fstab 条目失败(重启可能报错,请手工处理): %w", err)
	}
	if in.RemoveDir {
		_ = s.run(octx, serverID, []string{"rmdir", in.Mountpoint}) // 尽力而为(非空目录保留)
	}
	return nil
}
