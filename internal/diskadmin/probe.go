// probe.go — 存储拓扑发现:单次 SSH 合并采集(lsblk/pvs/vgs/lvs/df/工具探测),段间
// 以 ##PW: 标记分隔(与 httpapi/server_metrics.go 采集模式同形),解析为 Topology。
//
// lsblk 老版本(<2.37)无 MOUNTPOINTS 列会整条命令失败:采集脚本先试 MOUNTPOINTS、
// 失败回退 MOUNTPOINT 单列;解析层两者兼容。pvs/vgs/lvs 缺 lvm2 时段为空,经工具
// 探测区分「未安装」(tools.LVM=false,LVM 视图为空)与「无 PV/VG/LV」。
package diskadmin

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// 采集段名。
const (
	secLsblk = "lsblk"
	secPVS   = "pvs"
	secVGS   = "vgs"
	secLVS   = "lvs"
	secDF    = "df"
	secTools = "tools"
)

// topologyCollectArgs 返回一次性拓扑采集命令(sh -c 静态脚本,无用户输入拼接;
// 与 metricsCollectArgs 同款做法)。lsblk 列:MOUNTPOINTS 老版本回退 MOUNTPOINT。
func topologyCollectArgs() []string {
	lsblkCols := "PATH,NAME,TYPE,SIZE,FSTYPE,UUID,PARTUUID,SERIAL,MODEL,RO,PTTYPE,PKNAME"
	parts := []string{
		`echo "##PW:lsblk"`,
		`lsblk -J -b -n -o ` + lsblkCols + `,MOUNTPOINTS 2>/dev/null || lsblk -J -b -n -o ` + lsblkCols + `,MOUNTPOINT 2>/dev/null`,
		`echo "##PW:pvs"`,
		`pvs --reportformat json --units b --noheadings -o pv_name,vg_name,pv_size,pv_free 2>/dev/null`,
		`echo "##PW:vgs"`,
		`vgs --reportformat json --units b --noheadings -o vg_name,vg_size,vg_free,pv_count,lv_count 2>/dev/null`,
		`echo "##PW:lvs"`,
		`lvs --reportformat json --units b --noheadings -o lv_name,vg_name,lv_path,lv_size,lv_attr,lv_active 2>/dev/null`,
		`echo "##PW:df"`,
		`df -B1 -P 2>/dev/null`,
		`echo "##PW:tools"`,
		`for c in parted partx mkfs.ext4 mkfs.xfs mkfs.btrfs e2fsck resize2fs xfs_growfs btrfs pvs findmnt; do if command -v "$c" >/dev/null 2>&1; then echo "$c ok"; fi; done`,
	}
	return []string{"sh", "-c", strings.Join(parts, "; ")}
}

// Topology 实时采集(经提权 Runner 跑合并脚本;无提权 → ErrNoPrivilege)。
func (s *service) Topology(ctx context.Context, serverID string) (*Topology, error) {
	sv, err := s.servers.Get(ctx, serverID)
	if err != nil {
		return nil, fmt.Errorf("%w: 服务器 %s 不可用: %v", ErrServerNotFound, serverID, err)
	}
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	topo, err := s.collect(cctx, serverID)
	if err != nil {
		return nil, err
	}
	topo.LoginUser = sv.User
	topo.Privileged = true // collect 经提权 Runner 成功即视为可提权
	if st := s.unlockState(serverID); st.Unlocked {
		topo.Unlocked = true
		topo.UnlockExpiresAt = st.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return topo, nil
}

// collect 跑采集脚本并解析(不含登录用户/解锁状态,变更操作守卫复用)。
func (s *service) collect(ctx context.Context, serverID string) (*Topology, error) {
	res, err := s.runner.Run(ctx, serverID, topologyCollectArgs())
	if err != nil {
		return nil, err
	}
	topo := parseTopology(res.Stdout)
	if len(topo.Devices) == 0 {
		return nil, fmt.Errorf("%w: lsblk 无输出(命令缺失或权限不足)", ErrExec)
	}
	return topo, nil
}

// parseTopology 把采集脚本 stdout 解析为完整拓扑(纯函数,可单测)。
func parseTopology(out string) *Topology {
	sections := splitSections(out)
	t := &Topology{}
	t.Tools = parseTools(sections[secTools])
	t.Devices = parseLsblk(sections[secLsblk])
	usage := parseDfUsage(sections[secDF])
	if t.Tools.LVM {
		t.PVs = parsePVs(sections[secPVS])
		t.VGs = parseVGs(sections[secVGS])
		t.LVs = parseLVs(sections[secLVS])
	}
	applyLVMContext(t)
	markProtected(t)
	applyUsage(t, usage)
	return t
}

// parseLsblk 解析 lsblk -J 树(children 嵌套递归)。
func parseLsblk(stdout string) []*BlockDevice {
	root := asObj(parseJSON(stdout))
	if root == nil {
		return nil
	}
	var build func(o obj) *BlockDevice
	build = func(o obj) *BlockDevice {
		if o == nil {
			return nil
		}
		d := &BlockDevice{
			Path:        pickStr(o, "path", "name"),
			Name:        pickStr(o, "name"),
			Type:        pickStr(o, "type"),
			SizeBytes:   uint64(pickNum(o, "size")),
			FSType:      pickStr(o, "fstype"),
			Mountpoints: pickMountpoints(o),
			UUID:        pickStr(o, "uuid"),
			PARTUUID:    pickStr(o, "partuuid"),
			Serial:      pickStr(o, "serial"),
			Model:       pickStr(o, "model"),
			ReadOnly:    pickBool(o, "ro"),
			PTType:      pickStr(o, "pttype"),
			Parent:      pickStr(o, "pkname"),
		}
		for _, c := range asArr(o["children"]) {
			if cd := build(asObj(c)); cd != nil {
				d.Children = append(d.Children, cd)
			}
		}
		return d
	}
	var out []*BlockDevice
	for _, e := range asArr(root["blockdevices"]) {
		if d := build(asObj(e)); d != nil {
			out = append(out, d)
		}
	}
	return out
}

// lvmReport 解析 LVM --reportformat json:官方形态为 {"report":[{"pv|vg|lv":[行...]}]}
// (report 数组的元素是单键对象,键为报告名);宽容兼容行对象直接平铺在 report 数组的
// 老形态。
func lvmReport(stdout string) []obj {
	root := asObj(parseJSON(stdout))
	if root == nil {
		return nil
	}
	var out []obj
	for _, rep := range asArr(root["report"]) {
		if o := asObj(rep); o != nil {
			for _, v := range o { // 单键(pv/vg/lv)→ 行数组
				for _, e := range asArr(v) {
					if row := asObj(e); row != nil {
						out = append(out, row)
					}
				}
			}
		}
	}
	if len(out) == 0 {
		for _, e := range asArr(root["report"]) {
			if row := asObj(e); row != nil {
				out = append(out, row)
			}
		}
	}
	return out
}

// parsePVs 解析 pvs 报告。
func parsePVs(stdout string) []PhysicalVolume {
	var out []PhysicalVolume
	for _, o := range lvmReport(stdout) {
		out = append(out, PhysicalVolume{
			Path:      pickStr(o, "pv_name"),
			VGName:    pickStr(o, "vg_name"),
			SizeBytes: pickBytes(o, "pv_size"),
			FreeBytes: pickBytes(o, "pv_free"),
		})
	}
	return out
}

// parseVGs 解析 vgs 报告。
func parseVGs(stdout string) []VolumeGroup {
	var out []VolumeGroup
	for _, o := range lvmReport(stdout) {
		out = append(out, VolumeGroup{
			Name:      pickStr(o, "vg_name"),
			SizeBytes: pickBytes(o, "vg_size"),
			FreeBytes: pickBytes(o, "vg_free"),
			PVCount:   int(pickNum(o, "pv_count")),
			LVCount:   int(pickNum(o, "lv_count")),
		})
	}
	return out
}

// parseLVs 解析 lvs 报告(fs/挂载/用量由 applyLVMContext 经设备树回填)。
func parseLVs(stdout string) []LogicalVolume {
	var out []LogicalVolume
	for _, o := range lvmReport(stdout) {
		lv := LogicalVolume{
			Name:      pickStr(o, "lv_name"),
			VGName:    pickStr(o, "vg_name"),
			Path:      pickStr(o, "lv_path"),
			SizeBytes: pickBytes(o, "lv_size"),
			Attr:      pickStr(o, "lv_attr"),
		}
		if a := pickStr(o, "lv_active"); a != "" {
			lv.Active = strings.EqualFold(a, "active") || a == "a"
		} else if len(lv.Attr) >= 5 {
			lv.Active = lv.Attr[4] == 'a'
		}
		if lv.Path == "" && lv.VGName != "" && lv.Name != "" {
			lv.Path = "/dev/" + lv.VGName + "/" + lv.Name
		}
		out = append(out, lv)
	}
	return out
}

// parseTools 解析 command -v 探测行("<cmd> ok")。
func parseTools(stdout string) ToolStatus {
	present := map[string]bool{}
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "ok" {
			present[fields[0]] = true
		}
	}
	return ToolStatus{
		Parted:    present["parted"],
		Partx:     present["partx"],
		MkfsExt4:  present["mkfs.ext4"],
		MkfsXfs:   present["mkfs.xfs"],
		MkfsBtrfs: present["mkfs.btrfs"],
		E2fsck:    present["e2fsck"],
		Resize2fs: present["resize2fs"],
		XfsGrowfs: present["xfs_growfs"],
		Btrfs:     present["btrfs"],
		LVM:       present["pvs"],
		Findmnt:   present["findmnt"],
	}
}

// applyLVMContext 给 LV 回填文件系统/挂载点(LVM 报告无 fs 信息,经 /dev/mapper 名
// 与 lsblk 树上的 lvm 节点匹配;名中 "-" 有 "--" 转义差异)。
func applyLVMContext(t *Topology) {
	if len(t.LVs) == 0 {
		return
	}
	for i := range t.LVs {
		lv := &t.LVs[i]
		dev := t.findDevice(lv.Path)
		if dev == nil {
			dev = t.findDevice(dmName(lv.VGName, lv.Name))
		}
		if dev == nil {
			continue
		}
		lv.FSType = dev.FSType
		lv.Mountpoints = dev.Mountpoints
	}
}

// markProtected 计算受保护标记(LV 关键挂载/swap → VG → PV → 设备树自底向上)。
func markProtected(t *Topology) {
	// 1) VG 名 → 是否含受保护 LV(关键挂载或 swap)。
	vgProtected := map[string]bool{}
	for i := range t.LVs {
		lv := &t.LVs[i]
		if isCriticalAny(lv.Mountpoints) || lv.FSType == "swap" {
			lv.Protected = true
			vgProtected[lv.VGName] = true
		}
	}
	for i := range t.VGs {
		if vgProtected[t.VGs[i].Name] {
			t.VGs[i].Protected = true
		}
	}
	// 2) PV:所属 VG 受保护 → PV 受保护,其底层设备(树节点)标 protected。
	pvProtected := map[string]bool{}
	for i := range t.PVs {
		if vgProtected[t.PVs[i].VGName] {
			t.PVs[i].Protected = true
			pvProtected[t.PVs[i].Path] = true
		}
	}
	// 3) 设备树:叶子关键挂载/swap/PV-in-保护VG → protected,自底向上传播到整盘。
	var mark func(d *BlockDevice) bool
	mark = func(d *BlockDevice) bool {
		prot := isCriticalAny(d.Mountpoints) || d.FSType == "swap" || pvProtected[d.Path]
		for _, c := range d.Children {
			if mark(c) {
				prot = true
			}
		}
		d.Protected = prot
		return prot
	}
	for _, d := range t.Devices {
		mark(d)
	}
}

// isCriticalAny 报告挂载点列表中是否含关键目录。
func isCriticalAny(mounts []string) bool {
	for _, m := range mounts {
		if isCriticalMount(m) {
			return true
		}
	}
	return false
}

// applyUsage 把 df 用量按挂载点回填到设备树与 LV。
func applyUsage(t *Topology, usage map[string]dfUsage) {
	if len(usage) == 0 {
		return
	}
	walkDevices(t.Devices, func(d *BlockDevice) {
		for _, m := range d.Mountpoints {
			if u, ok := usage[m]; ok {
				d.UsedBytes = u.Used
				return
			}
		}
	})
	for i := range t.LVs {
		for _, m := range t.LVs[i].Mountpoints {
			if u, ok := usage[m]; ok {
				t.LVs[i].UsedBytes = u.Used
				break
			}
		}
	}
}
