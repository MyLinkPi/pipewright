// lvm.go — LVM 变更(PV/VG/LV;创建/扩容/缩容/移除)。
//
// 破坏性命令(pvremove/vgremove/lvremove/lvreduce)统一带 --yes(非交互通道下 LVM
// 工具遇提示会中止);LV 扩缩容的文件系统层复用 ops.go 的能力矩阵例程:
// ext4 离线缩(e2fsck → resize2fs 留余量 → lvreduce → resize2fs 回填)、
// btrfs 在线缩、XFS 不可缩。
package diskadmin

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// requireLVM 返回 LVM 工具可用性(缺 lvm2 → ErrLvmMissing)。
func (s *service) requireLVM(topo *Topology) error {
	if !topo.Tools.LVM {
		return fmt.Errorf("%w: 请先安装 lvm2(如 apt install lvm2 / dnf install lvm2)", ErrLvmMissing)
	}
	return nil
}

// lvDevice 在设备树上定位 LV 对应节点(/dev/<vg>/<lv> 或 /dev/mapper/<vg>-<lv>)。
func lvDevice(topo *Topology, lv *LogicalVolume) *BlockDevice {
	if d := topo.findDevice(lv.Path); d != nil {
		return d
	}
	return topo.findDevice(dmName(lv.VGName, lv.Name))
}

// ── PV ───────────────────────────────────────────────────────────────────────

// PVCreate 把空设备(整盘或分区)初始化为 PV(无文件系统、未挂载)。
func (s *service) PVCreate(ctx context.Context, serverID string, device string) error {
	if err := s.requireUnlocked(serverID); err != nil {
		return err
	}
	if !devicePathRe.MatchString(device) {
		return fmt.Errorf("%w: 设备路径非法", ErrInvalidInput)
	}
	octx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	topo, err := s.collect(octx, serverID)
	if err != nil {
		return err
	}
	if err := s.requireLVM(topo); err != nil {
		return err
	}
	dev := topo.resolveDevice(device)
	if dev == nil {
		return fmt.Errorf("%w: %s 在拓扑中不可见", ErrDeviceNotFound, device)
	}
	if dev.Protected {
		return fmt.Errorf("%w: %s 承载系统内容", ErrProtectedDevice, device)
	}
	if len(dev.Mountpoints) > 0 {
		return fmt.Errorf("%w: %s 已挂载(%s)", ErrDeviceBusy, device, strings.Join(dev.Mountpoints, ","))
	}
	if dev.FSType != "" {
		return fmt.Errorf("%w: %s 已有文件系统(%s),PV 需要空设备", ErrInvalidInput, device, dev.FSType)
	}
	if topo.findPV(device) != nil {
		return fmt.Errorf("%w: %s 已是 PV", ErrInvalidInput, device)
	}
	return s.run(octx, serverID, []string{"pvcreate", device})
}

// PVRemove 移除未入 VG 的 PV(confirm 逐字确认)。
func (s *service) PVRemove(ctx context.Context, serverID string, in RemoveTargetInput) error {
	if err := s.requireUnlocked(serverID); err != nil {
		return err
	}
	if !devicePathRe.MatchString(in.Target) {
		return fmt.Errorf("%w: 设备路径非法", ErrInvalidInput)
	}
	if in.Confirm != in.Target {
		return fmt.Errorf("%w: 须逐字输入 %s 确认", ErrConfirmMismatch, in.Target)
	}
	octx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	topo, err := s.collect(octx, serverID)
	if err != nil {
		return err
	}
	if err := s.requireLVM(topo); err != nil {
		return err
	}
	pv := topo.findPV(in.Target)
	if pv == nil {
		return fmt.Errorf("%w: %s 不是 PV", ErrDeviceNotFound, in.Target)
	}
	if pv.VGName != "" {
		return fmt.Errorf("%w: %s 属于 VG %s,须先 vgreduce 摘出", ErrInvalidInput, in.Target, pv.VGName)
	}
	if pv.Protected {
		return fmt.Errorf("%w: %s 属于受保护 VG", ErrProtectedDevice, in.Target)
	}
	return s.run(octx, serverID, []string{"pvremove", "--yes", in.Target})
}

// ── VG ───────────────────────────────────────────────────────────────────────

// VGCreate 用一组未入组的 PV 建 VG。
func (s *service) VGCreate(ctx context.Context, serverID string, in VGCreateInput) error {
	if err := s.requireUnlocked(serverID); err != nil {
		return err
	}
	if !lvmNameRe.MatchString(in.Name) {
		return fmt.Errorf("%w: VG 名非法(字母数字开头,≤64 字符)", ErrInvalidInput)
	}
	if len(in.Devices) == 0 || len(in.Devices) > 32 {
		return fmt.Errorf("%w: PV 列表须为 1-32 个设备", ErrInvalidInput)
	}
	for _, d := range in.Devices {
		if !devicePathRe.MatchString(d) {
			return fmt.Errorf("%w: 设备路径非法(%s)", ErrInvalidInput, d)
		}
	}
	octx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	topo, err := s.collect(octx, serverID)
	if err != nil {
		return err
	}
	if err := s.requireLVM(topo); err != nil {
		return err
	}
	if topo.findVG(in.Name) != nil {
		return fmt.Errorf("%w: VG %s 已存在", ErrInvalidInput, in.Name)
	}
	cmd := []string{"vgcreate", in.Name}
	for _, d := range in.Devices {
		pv := topo.findPV(d)
		if pv == nil {
			return fmt.Errorf("%w: %s 不是 PV,须先 pvcreate", ErrInvalidInput, d)
		}
		if pv.VGName != "" {
			return fmt.Errorf("%w: %s 已属于 VG %s", ErrInvalidInput, d, pv.VGName)
		}
		if pv.Protected {
			return fmt.Errorf("%w: %s 属于受保护 VG", ErrProtectedDevice, d)
		}
		cmd = append(cmd, d)
	}
	return s.run(octx, serverID, cmd)
}

// VGExtend 给 VG 追加一个未入组的 PV。
func (s *service) VGExtend(ctx context.Context, serverID string, in VGExtendInput) error {
	if err := s.requireUnlocked(serverID); err != nil {
		return err
	}
	if !devicePathRe.MatchString(in.Device) {
		return fmt.Errorf("%w: 设备路径非法", ErrInvalidInput)
	}
	octx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	topo, err := s.collect(octx, serverID)
	if err != nil {
		return err
	}
	if err := s.requireLVM(topo); err != nil {
		return err
	}
	if topo.findVG(in.Name) == nil {
		return fmt.Errorf("%w: VG %s 不存在", ErrDeviceNotFound, in.Name)
	}
	pv := topo.findPV(in.Device)
	if pv == nil {
		return fmt.Errorf("%w: %s 不是 PV,须先 pvcreate", ErrInvalidInput, in.Device)
	}
	if pv.VGName != "" {
		return fmt.Errorf("%w: %s 已属于 VG %s", ErrInvalidInput, in.Device, pv.VGName)
	}
	return s.run(octx, serverID, []string{"vgextend", in.Name, in.Device})
}

// VGReduce 从 VG 摘出一个无已分配 extent 的 PV(confirm 逐字确认;不做 pvmove)。
func (s *service) VGReduce(ctx context.Context, serverID string, in VGReduceInput) error {
	if err := s.requireUnlocked(serverID); err != nil {
		return err
	}
	if !devicePathRe.MatchString(in.Device) {
		return fmt.Errorf("%w: 设备路径非法", ErrInvalidInput)
	}
	if in.Confirm != in.Device {
		return fmt.Errorf("%w: 须逐字输入 %s 确认", ErrConfirmMismatch, in.Device)
	}
	octx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	topo, err := s.collect(octx, serverID)
	if err != nil {
		return err
	}
	if err := s.requireLVM(topo); err != nil {
		return err
	}
	if topo.findVG(in.Name) == nil {
		return fmt.Errorf("%w: VG %s 不存在", ErrDeviceNotFound, in.Name)
	}
	pv := topo.findPV(in.Device)
	if pv == nil || pv.VGName != in.Name {
		return fmt.Errorf("%w: %s 不属于 VG %s", ErrDeviceNotFound, in.Device, in.Name)
	}
	// extent 有分配(余量远小于总量)→ 拒绝:本功能不做 pvmove 数据迁移。
	if pv.FreeBytes+1<<20 < pv.SizeBytes {
		return fmt.Errorf("%w: %s 上仍有已分配 extent;本功能不做 pvmove,请先迁移数据", ErrDeviceBusy, in.Device)
	}
	return s.run(octx, serverID, []string{"vgreduce", in.Name, in.Device})
}

// VGRemove 移除空 VG(无 LV;confirm 逐字等于 VG 名)。
func (s *service) VGRemove(ctx context.Context, serverID string, in RemoveConfirmInput) error {
	if err := s.requireUnlocked(serverID); err != nil {
		return err
	}
	if in.Confirm != in.Name {
		return fmt.Errorf("%w: 须逐字输入 %s 确认", ErrConfirmMismatch, in.Name)
	}
	octx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	topo, err := s.collect(octx, serverID)
	if err != nil {
		return err
	}
	if err := s.requireLVM(topo); err != nil {
		return err
	}
	vg := topo.findVG(in.Name)
	if vg == nil {
		return fmt.Errorf("%w: VG %s 不存在", ErrDeviceNotFound, in.Name)
	}
	if vg.Protected {
		return fmt.Errorf("%w: VG %s 含系统逻辑卷", ErrProtectedDevice, in.Name)
	}
	for i := range topo.LVs {
		if topo.LVs[i].VGName == in.Name {
			return fmt.Errorf("%w: VG %s 内仍有逻辑卷,须先 lvremove", ErrDeviceBusy, in.Name)
		}
	}
	return s.run(octx, serverID, []string{"vgremove", "--yes", in.Name})
}

// ── LV ───────────────────────────────────────────────────────────────────────

// LVCreate 建逻辑卷(SizeGiB / UseAllFree 二选一;可选直接格式化并挂载)。
func (s *service) LVCreate(ctx context.Context, serverID string, in LVCreateInput) error {
	if err := s.requireUnlocked(serverID); err != nil {
		return err
	}
	if !lvmNameRe.MatchString(in.Name) {
		return fmt.Errorf("%w: LV 名非法(字母数字开头,≤64 字符)", ErrInvalidInput)
	}
	if in.FSType != "" && in.FSType != "ext4" && in.FSType != "xfs" && in.FSType != "btrfs" {
		return fmt.Errorf("%w: 文件系统须为 ext4/xfs/btrfs", ErrInvalidInput)
	}
	if in.Mountpoint != "" {
		if in.FSType == "" {
			return fmt.Errorf("%w: 指定挂载点时须同时指定文件系统", ErrInvalidInput)
		}
		if err := validateMountpoint(in.Mountpoint); err != nil {
			return err
		}
	}
	if (in.SizeGiB > 0) == in.UseAllFree {
		return fmt.Errorf("%w: SizeGiB 与 UseAllFree 须二选一", ErrInvalidInput)
	}
	octx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	topo, err := s.collect(octx, serverID)
	if err != nil {
		return err
	}
	if err := s.requireLVM(topo); err != nil {
		return err
	}
	if topo.findVG(in.VG) == nil {
		return fmt.Errorf("%w: VG %s 不存在", ErrDeviceNotFound, in.VG)
	}
	for i := range topo.LVs {
		if topo.LVs[i].VGName == in.VG && topo.LVs[i].Name == in.Name {
			return fmt.Errorf("%w: %s/%s 已存在", ErrInvalidInput, in.VG, in.Name)
		}
	}
	cmd := []string{"lvcreate", "-n", in.Name}
	if in.UseAllFree {
		cmd = append(cmd, "-l", "100%FREE")
	} else {
		cmd = append(cmd, "-L", strconv.Itoa(in.SizeGiB)+"G")
	}
	cmd = append(cmd, in.VG)
	if err := s.run(octx, serverID, cmd); err != nil {
		return err
	}
	path := "/dev/" + in.VG + "/" + in.Name
	if in.FSType != "" {
		if err := s.formatDirect(octx, serverID, path, in.FSType, "", topo.Tools); err != nil {
			return fmt.Errorf("LV 已创建,但格式化失败: %w", err)
		}
	}
	if in.Mountpoint != "" {
		if topo.mountpointInUse(in.Mountpoint) {
			return fmt.Errorf("%w: %s 上已有文件系统挂载(格式化已完成)", ErrMountpointTaken, in.Mountpoint)
		}
		if err := s.mountDirect(octx, serverID, path, in.FSType, in.Mountpoint, "defaults"); err != nil {
			return fmt.Errorf("LV 已创建并格式化,但挂载失败: %w", err)
		}
	}
	return nil
}

// LVGrow 扩 LV(+GiB 或用尽 VG 剩余)并按文件系统类型扩容。
func (s *service) LVGrow(ctx context.Context, serverID string, in LVGrowInput) error {
	if err := s.requireUnlocked(serverID); err != nil {
		return err
	}
	if !devicePathRe.MatchString(in.Path) {
		return fmt.Errorf("%w: LV 路径非法", ErrInvalidInput)
	}
	if (in.SizeGiB > 0) == in.UseAllFree {
		return fmt.Errorf("%w: SizeGiB 与 UseAllFree 须二选一", ErrInvalidInput)
	}
	octx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	topo, err := s.collect(octx, serverID)
	if err != nil {
		return err
	}
	if err := s.requireLVM(topo); err != nil {
		return err
	}
	lv := topo.findLV(in.Path)
	if lv == nil {
		return fmt.Errorf("%w: LV %s 不存在", ErrDeviceNotFound, in.Path)
	}
	if lv.Protected {
		return fmt.Errorf("%w: %s 是系统逻辑卷", ErrProtectedDevice, in.Path)
	}
	cmd := []string{"lvextend"}
	if in.UseAllFree {
		cmd = append(cmd, "-l", "+100%FREE")
	} else {
		cmd = append(cmd, "-L", "+"+strconv.Itoa(in.SizeGiB)+"G")
	}
	cmd = append(cmd, in.Path)
	if err := s.run(octx, serverID, cmd); err != nil {
		return err
	}
	// 扩完重查拓扑拿最新 fs/挂载状态再扩文件系统。
	topo2, err := s.collect(octx, serverID)
	if err != nil {
		return err
	}
	dev := lvDevice(topo2, lv)
	if dev == nil {
		return nil // 树上不可见(裸 LV 无 fs)则只扩了底层,符合预期
	}
	return s.growFilesystem(ctx, serverID, dev, topo2.Tools)
}

// LVShrink 缩 LV(ext4 须未挂载;btrfs 须已挂载;xfs 不支持;confirm 逐字确认)。
func (s *service) LVShrink(ctx context.Context, serverID string, in ShrinkInput) error {
	if err := s.requireUnlocked(serverID); err != nil {
		return err
	}
	if !devicePathRe.MatchString(in.Target) {
		return fmt.Errorf("%w: LV 路径非法", ErrInvalidInput)
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
	if err := s.requireLVM(topo); err != nil {
		return err
	}
	lv := topo.findLV(in.Target)
	if lv == nil {
		return fmt.Errorf("%w: LV %s 不存在", ErrDeviceNotFound, in.Target)
	}
	if lv.Protected {
		return fmt.Errorf("%w: %s 是系统逻辑卷", ErrProtectedDevice, in.Target)
	}
	if uint64(in.TargetGiB) >= lv.SizeBytes>>30 {
		return fmt.Errorf("%w: 目标容量须小于当前容量(%dGiB)", ErrInvalidInput, lv.SizeBytes>>30)
	}
	gibSpec := strconv.Itoa(in.TargetGiB) + "G"
	dev := lvDevice(topo, lv)
	switch lv.FSType {
	case "":
		// 裸 LV:直接缩(无文件系统可保护)。
		return s.run(fctx, serverID, []string{"lvreduce", "--yes", "-L", gibSpec, in.Target})
	case "ext4":
		if mnt := firstMount(lv.Mountpoints); mnt != "" {
			return fmt.Errorf("%w: ext4 缩容须先卸载(当前挂载于 %s),请自行 umount 后重试", ErrDeviceBusy, mnt)
		}
		if !topo.Tools.E2fsck || !topo.Tools.Resize2fs {
			return fmt.Errorf("%w: 远端缺少 e2fsck/resize2fs", ErrInvalidInput)
		}
		if err := s.run(fctx, serverID, []string{"e2fsck", "-f", in.Target}); err != nil {
			return err
		}
		// fs 先缩到目标-1GiB(留余量)→ LV 缩到目标 → fs 回填到 LV 大小。
		if err := s.run(fctx, serverID, []string{"resize2fs", in.Target, strconv.Itoa(in.TargetGiB-1) + "G"}); err != nil {
			return err
		}
		if err := s.run(fctx, serverID, []string{"lvreduce", "--yes", "-L", gibSpec, in.Target}); err != nil {
			return err
		}
		return s.run(fctx, serverID, []string{"resize2fs", in.Target})
	case "btrfs":
		mnt := firstMount(lv.Mountpoints)
		if mnt == "" {
			return fmt.Errorf("%w: btrfs 缩容须在挂载状态下进行(在线缩容)", ErrMountRequired)
		}
		if !topo.Tools.Btrfs {
			return fmt.Errorf("%w: 远端缺少 btrfs", ErrInvalidInput)
		}
		targetBytes := uint64(in.TargetGiB) << 30
		if dev != nil && dev.UsedBytes > 0 && dev.UsedBytes+512<<20 > targetBytes {
			return fmt.Errorf("%w: 目标容量过小(该文件系统已用约 %dGiB)", ErrInvalidInput, dev.UsedBytes>>30)
		}
		if err := s.run(fctx, serverID, []string{"btrfs", "filesystem", "resize", strconv.FormatUint(targetBytes, 10), mnt}); err != nil {
			return err
		}
		if err := s.run(fctx, serverID, []string{"lvreduce", "--yes", "-L", gibSpec, in.Target}); err != nil {
			return err
		}
		return s.run(fctx, serverID, []string{"btrfs", "filesystem", "resize", "max", mnt})
	case "xfs":
		return fmt.Errorf("%w: XFS 文件系统不支持缩容(仅 ext4/btrfs)", ErrFSTypeUnsupported)
	default:
		return fmt.Errorf("%w: %s 暂不支持缩容", ErrFSTypeUnsupported, lv.FSType)
	}
}

// LVRemove 移除未挂载的 LV(confirm 逐字确认)。
func (s *service) LVRemove(ctx context.Context, serverID string, in RemoveTargetInput) error {
	if err := s.requireUnlocked(serverID); err != nil {
		return err
	}
	if !devicePathRe.MatchString(in.Target) {
		return fmt.Errorf("%w: LV 路径非法", ErrInvalidInput)
	}
	if in.Confirm != in.Target {
		return fmt.Errorf("%w: 须逐字输入 %s 确认", ErrConfirmMismatch, in.Target)
	}
	octx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	topo, err := s.collect(octx, serverID)
	if err != nil {
		return err
	}
	if err := s.requireLVM(topo); err != nil {
		return err
	}
	lv := topo.findLV(in.Target)
	if lv == nil {
		return fmt.Errorf("%w: LV %s 不存在", ErrDeviceNotFound, in.Target)
	}
	if lv.Protected {
		return fmt.Errorf("%w: %s 是系统逻辑卷", ErrProtectedDevice, in.Target)
	}
	if len(lv.Mountpoints) > 0 {
		return fmt.Errorf("%w: %s 已挂载(%s),须先卸载", ErrDeviceBusy, in.Target, strings.Join(lv.Mountpoints, ","))
	}
	return s.run(octx, serverID, []string{"lvremove", "--yes", in.Target})
}
