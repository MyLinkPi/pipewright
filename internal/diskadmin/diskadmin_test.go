// diskadmin_test.go — 离线单测:fake Runner 注入(不触网),覆盖拓扑解析/保护标记、
// 各操作守卫(解锁闸门/系统盘/挂载态/确认文本)与命令构造(parted/mkfs/LVM/fstab 链路)。
package diskadmin

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/huangchengsir/pipewright/internal/target"
)

// ── 测试夹具 ─────────────────────────────────────────────────────────────────

// fakeRunner 记录全部命令;同名命令可排队多次响应(queues 优先,耗尽后粘住最后一条
// —— 模拟「守卫采集 + 轮询采集」的多次调用),否则查 outputs/fails。
type fakeRunner struct {
	mu      sync.Mutex
	calls   []string
	stdins  []string // 与 WithStdin 调用一一对应的 stdin 内容
	queues  map[string][]*target.ExecResult
	sticky  map[string]*target.ExecResult
	outputs map[string]*target.ExecResult
	fails   map[string]error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		queues:  map[string][]*target.ExecResult{},
		sticky:  map[string]*target.ExecResult{},
		outputs: map[string]*target.ExecResult{},
		fails:   map[string]error{},
	}
}

func cmdKey(cmd []string) string { return strings.Join(cmd, " ") }

func (f *fakeRunner) Run(_ context.Context, _ string, cmd []string) (*target.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cmdKey(cmd))
	return f.dispatch(cmdKey(cmd))
}

func (f *fakeRunner) RunWithStdin(_ context.Context, _ string, cmd []string, stdin io.Reader) (*target.ExecResult, error) {
	b, _ := io.ReadAll(stdin)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cmdKey(cmd))
	f.stdins = append(f.stdins, string(b))
	return f.dispatch(cmdKey(cmd))
}

func (f *fakeRunner) dispatch(key string) (*target.ExecResult, error) {
	if q := f.queues[key]; len(q) > 0 {
		r := q[0]
		f.queues[key] = q[1:]
		f.sticky[key] = r
		return r, nil
	}
	if r, ok := f.sticky[key]; ok {
		return r, nil
	}
	if err, ok := f.fails[key]; ok {
		return nil, err
	}
	if r, ok := f.outputs[key]; ok {
		return r, nil
	}
	return &target.ExecResult{}, nil
}

func (f *fakeRunner) called(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == key {
			return true
		}
	}
	return false
}

func (f *fakeRunner) lastStdin() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.stdins) == 0 {
		return ""
	}
	return f.stdins[len(f.stdins)-1]
}

// fakeServers 仅实现 Get(Topology 用;其余方法继承 nil 接口,测试不触)。
type fakeServers struct {
	target.Service
	user string
}

func (fs *fakeServers) Get(_ context.Context, _ string) (*target.Server, error) {
	return &target.Server{ID: "s1", Name: "srv", User: fs.user}, nil
}

func newTestService(fr *fakeRunner, user string) Service {
	return New(&fakeServers{user: user}, WithRunner(fr))
}

// sampleScript 返回一次拓扑采集脚本的完整 stdout(__VDB_PTTYPE__/__VDB_CHILDREN__
// 占位符用于生成「vdb 已有分区」变体;均为裸 JSON 值,不加引号)。
func sampleScript(vdbPTType, vdbChildren string) string {
	lsblk := `{
 "blockdevices": [
  {"path":"/dev/vda","name":"vda","type":"disk","size":21474836480,"fstype":null,"mountpoints":[],"uuid":null,"serial":"VG01","model":"Virtio","ro":false,"pttype":"gpt","pkname":null,"children":[
   {"path":"/dev/vda1","name":"vda1","type":"part","size":268435456,"fstype":"vfat","mountpoints":["/boot/efi"],"uuid":"U1","ro":false,"pttype":null,"pkname":"vda"},
   {"path":"/dev/vda2","name":"vda2","type":"part","size":1073741824,"fstype":"ext4","mountpoints":["/boot"],"uuid":"U2","ro":false,"pttype":null,"pkname":"vda"},
   {"path":"/dev/vda3","name":"vda3","type":"part","size":19327352832,"fstype":"LVM2_member","mountpoints":[],"uuid":"U3","ro":false,"pttype":null,"pkname":"vda","children":[
    {"path":"/dev/mapper/rootvg-root","name":"rootvg-root","type":"lvm","size":10737418240,"fstype":"ext4","mountpoints":["/"],"uuid":"R1","ro":false,"pkname":"vda3"},
    {"path":"/dev/mapper/rootvg-data","name":"rootvg-data","type":"lvm","size":5368709120,"fstype":"ext4","mountpoints":["/data"],"uuid":"R2","ro":false,"pkname":"vda3"}]}]},
  {"path":"/dev/vdb","name":"vdb","type":"disk","size":107374182400,"fstype":null,"mountpoints":[],"uuid":null,"ro":false,"pttype":__VDB_PTTYPE__,"pkname":null,"children":__VDB_CHILDREN__},
  {"path":"/dev/vdc","name":"vdc","type":"disk","size":107374182400,"fstype":null,"mountpoints":[],"uuid":null,"ro":false,"pttype":"gpt","pkname":null,"children":[
   {"path":"/dev/vdc1","name":"vdc1","type":"part","size":53687091200,"fstype":"ext4","mountpoints":["/mnt/data1"],"uuid":"C1","ro":false,"pkname":"vdc"},
   {"path":"/dev/vdc2","name":"vdc2","type":"part","size":53687091200,"fstype":"xfs","mountpoints":[],"uuid":"C2","ro":false,"pkname":"vdc"}]},
  {"path":"/dev/vdd","name":"vdd","type":"disk","size":107374182400,"fstype":null,"mountpoints":[],"uuid":null,"ro":false,"pttype":"gpt","pkname":null,"children":[
   {"path":"/dev/vdd1","name":"vdd1","type":"part","size":53687091200,"fstype":"btrfs","mountpoints":["/mnt/b1"],"uuid":"D1","ro":false,"pkname":"vdd"}]},
  {"path":"/dev/vde","name":"vde","type":"disk","size":53687091200,"fstype":"LVM2_member","mountpoints":[],"uuid":"E1","ro":false,"pttype":null,"pkname":null,"children":[
   {"path":"/dev/mapper/datavg-lv1","name":"datavg-lv1","type":"lvm","size":10737418240,"fstype":"ext4","mountpoints":[],"uuid":"L1","ro":false,"pkname":"vde"}]}
 ]}`
	pvs := `{"report":[{"pv":[{"pv_name":"/dev/vda3","vg_name":"rootvg","pv_size":"19327352832B","pv_free":"3221225472B"},{"pv_name":"/dev/vde","vg_name":"datavg","pv_size":"53687091200B","pv_free":"42949672960B"}]}]}`
	vgs := `{"report":[{"vg":[{"vg_name":"rootvg","vg_size":"19327352832B","vg_free":"3221225472B","pv_count":"1","lv_count":"2"},{"vg_name":"datavg","vg_size":"53687091200B","vg_free":"42949672960B","pv_count":"1","lv_count":"1"}]}]}`
	lvs := `{"report":[{"lv":[{"lv_name":"root","vg_name":"rootvg","lv_path":"/dev/rootvg/root","lv_size":"10737418240B","lv_attr":"-wi-ao----","lv_active":"active"},{"lv_name":"data","vg_name":"rootvg","lv_path":"/dev/rootvg/data","lv_size":"5368709120B","lv_attr":"-wi-ao----","lv_active":"active"},{"lv_name":"lv1","vg_name":"datavg","lv_path":"/dev/datavg/lv1","lv_size":"10737418240B","lv_attr":"-wi-a-----","lv_active":"active"}]}]}`
	df := `Filesystem 1024-blocks Used Available Capacity Mounted on
/dev/mapper/rootvg-root 10485760000 5242880000 5242880000 50% /
/dev/mapper/rootvg-data 5368709120 2147483648 3221225472 40% /data
/dev/vdc1 53687091200 26843545600 26843545600 50% /mnt/data1
/dev/vdd1 53687091200 26843545600 26843545600 50% /mnt/b1`
	tools := `parted ok
partx ok
mkfs.ext4 ok
mkfs.xfs ok
mkfs.btrfs ok
e2fsck ok
resize2fs ok
xfs_growfs ok
btrfs ok
pvs ok
findmnt ok`
	var b strings.Builder
	b.WriteString("##PW:lsblk\n" + lsblk + "\n")
	b.WriteString("##PW:pvs\n" + pvs + "\n")
	b.WriteString("##PW:vgs\n" + vgs + "\n")
	b.WriteString("##PW:lvs\n" + lvs + "\n")
	b.WriteString("##PW:df\n" + df + "\n")
	b.WriteString("##PW:tools\n" + tools + "\n")
	out := b.String()
	out = strings.ReplaceAll(out, "__VDB_PTTYPE__", vdbPTType)
	out = strings.ReplaceAll(out, "__VDB_CHILDREN__", vdbChildren)
	return out
}

// baseScript 是基线拓扑(vdb 空盘无分区表)。
func baseScript() string { return sampleScript("null", "[]") }

// withVdbPartScript 是 vdb 已有 1 分区的变体(waitChildrenAppear 轮询用;PTType 为
// JSON 字符串须自带引号)。
func withVdbPartScript() string {
	return sampleScript(`"gpt"`, `[{"path":"/dev/vdb1","name":"vdb1","type":"part","size":107374182400,"fstype":null,"mountpoints":[],"uuid":"B1","ro":false,"pkname":"vdb"}]`)
}

// seedCollect 给 fake 掮入 collect 响应队列(首条守卫用基线,后续轮询用变体)。
func seedCollect(fr *fakeRunner, outs ...string) {
	key := cmdKey(topologyCollectArgs())
	q := fr.queues[key]
	for _, o := range outs {
		q = append(q, &target.ExecResult{Stdout: o})
	}
	fr.queues[key] = q
}

// unlocked 解锁并忽略返回。
func unlock(svc Service) { _ = svc.Unlock("s1") }

// ── 纯函数测试 ───────────────────────────────────────────────────────────────

func TestParseTopology(t *testing.T) {
	topo := parseTopology(baseScript())
	if len(topo.Devices) != 5 {
		t.Fatalf("整盘数 = %d,期望 5", len(topo.Devices))
	}
	if len(topo.LVs) != 3 || len(topo.VGs) != 2 || len(topo.PVs) != 2 {
		t.Fatalf("LVM 视图解析异常: lv=%d vg=%d pv=%d", len(topo.LVs), len(topo.VGs), len(topo.PVs))
	}
	// LV ↔ 树匹配回填 fstype/dm 路径。
	lv1 := topo.findLV("/dev/datavg/lv1")
	if lv1 == nil || lv1.FSType != "ext4" || !lv1.Active {
		t.Fatalf("datavg/lv1 回填异常: %+v", lv1)
	}
	root := topo.findLV("/dev/rootvg/root")
	if root == nil || !root.Protected || root.FSType != "ext4" {
		t.Fatalf("root LV 应受保护且 fstype 回填: %+v", root)
	}
	// 保护传播:根 LV → VG rootvg → PV vda3 → vda 整盘。
	if vg := topo.findVG("rootvg"); !vg.Protected {
		t.Fatal("rootvg 应受保护")
	}
	if pv := topo.findPV("/dev/vda3"); !pv.Protected {
		t.Fatal("vda3(PV)应受保护")
	}
	if d := topo.findDevice("/dev/vda"); !d.Protected {
		t.Fatal("系统盘 vda 应受保护")
	}
	if d := topo.findDevice("/dev/vda2"); !d.Protected {
		t.Fatal("/boot 分区应受保护")
	}
	if d := topo.findDevice("/dev/vde"); d.Protected {
		t.Fatal("数据盘 vde(datavg)不应受保护")
	}
	// df 用量回填。
	if d := topo.findDevice("/dev/vdc1"); d.UsedBytes != 26843545600 {
		t.Fatalf("vdc1 已用 = %d,期望 26843545600", d.UsedBytes)
	}
	// 工具探测。
	if !(topo.Tools.Parted && topo.Tools.LVM && topo.Tools.Findmnt && topo.Tools.Btrfs) {
		t.Fatalf("工具探测异常: %+v", topo.Tools)
	}
}

func TestParseTopologyNoLVM(t *testing.T) {
	// lvm2 缺失:tools 段无 pvs,LVM 视图为空但设备树正常。
	out := strings.ReplaceAll(baseScript(), "pvs ok\n", "")
	topo := parseTopology(out)
	if topo.Tools.LVM {
		t.Fatal("pvs 缺失应判定 LVM 不可用")
	}
	if len(topo.LVs) != 0 || len(topo.Devices) != 5 {
		t.Fatalf("LVM 视图应为空、设备树正常: lv=%d devs=%d", len(topo.LVs), len(topo.Devices))
	}
}

func TestIsCriticalMount(t *testing.T) {
	cases := map[string]bool{
		"/": true, "/boot": true, "/boot/efi": true, "/var/lib/docker": true,
		"/home": false, "/data": false, "/mnt/x": false, "/database": false,
	}
	for m, want := range cases {
		if got := isCriticalMount(m); got != want {
			t.Fatalf("isCriticalMount(%q) = %v,期望 %v", m, got, want)
		}
	}
}

func TestPartitionNumber(t *testing.T) {
	cases := []struct {
		name, parent string
		want         int
	}{
		{"sdb3", "sdb", 3},
		{"nvme0n1p2", "nvme0n1", 2},
		{"md126p5", "md126", 5},
	}
	for _, c := range cases {
		d := &BlockDevice{Name: c.name, Parent: c.parent}
		if n, err := partitionNumber(d); err != nil || n != c.want {
			t.Fatalf("partitionNumber(%s) = %d,%v;期望 %d", c.name, n, err, c.want)
		}
	}
	if _, err := partitionNumber(&BlockDevice{Name: "sr0", Parent: ""}); err == nil {
		t.Fatal("无后缀分区号应报错")
	}
}

func TestParseSizeSpec(t *testing.T) {
	if s, _ := parseSizeSpec("100%"); s != "100%" {
		t.Fatal("100% 应通过")
	}
	if s, _ := parseSizeSpec("10GiB"); s != "10GiB" {
		t.Fatal("10GiB 应通过")
	}
	for _, bad := range []string{"", "0GiB", "10GB", "-5GiB", "10 GiB", "50%"} {
		if _, err := parseSizeSpec(bad); err == nil {
			t.Fatalf("%q 应被拒绝", bad)
		}
	}
}

func TestFstabFilter(t *testing.T) {
	content := "# /etc/fstab\nUUID=old /mnt/keep ext4 defaults 0 2\nUUID=x /mnt/data xfs defaults 0 0\n"
	if !fstabHasMountpoint(content, "/mnt/data") {
		t.Fatal("应识别已有条目")
	}
	next, removed := fstabWithoutMountpoint(content, "/mnt/data")
	if !removed || strings.Contains(next, "UUID=x") || !strings.Contains(next, "/mnt/keep") {
		t.Fatalf("移除结果异常: %q removed=%v", next, removed)
	}
	if _, removed := fstabWithoutMountpoint(content, "/mnt/none"); removed {
		t.Fatal("不存在条目不应移除")
	}
}

// ── 闸门与守卫测试 ───────────────────────────────────────────────────────────

func TestReadonlyGate(t *testing.T) {
	fr := newFakeRunner()
	svc := newTestService(fr, "root")
	err := svc.CreatePartition(context.Background(), "s1", PartitionCreateInput{Device: "/dev/vdb", SizeSpec: "100%", MakeLabel: true})
	if !errors.Is(err, ErrReadonly) {
		t.Fatalf("未解锁应拒绝: %v", err)
	}
}

func TestCreatePartitionGuards(t *testing.T) {
	fr := newFakeRunner()
	svc := newTestService(fr, "root")
	unlock(svc)
	seedCollect(fr, baseScript())

	// 系统盘拒绝。
	err := svc.CreatePartition(context.Background(), "s1", PartitionCreateInput{Device: "/dev/vda", SizeSpec: "100%"})
	if !errors.Is(err, ErrProtectedDevice) {
		t.Fatalf("系统盘应拒绝: %v", err)
	}
	// 无分区表但未显式确认建标签。
	err = svc.CreatePartition(context.Background(), "s1", PartitionCreateInput{Device: "/dev/vdb", SizeSpec: "100%"})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("空盘未确认建标签应拒绝: %v", err)
	}
	// 非整盘。
	err = svc.CreatePartition(context.Background(), "s1", PartitionCreateInput{Device: "/dev/vdc2", SizeSpec: "100%", MakeLabel: true})
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("非整盘应拒绝: %v", err)
	}
}

func TestCreatePartitionSuccess(t *testing.T) {
	fr := newFakeRunner()
	svc := newTestService(fr, "root")
	unlock(svc)
	// 守卫采集(基线)→ 分区命令 → 轮询采集(变体,出现 vdb1)。
	seedCollect(fr, baseScript(), withVdbPartScript())
	if err := svc.CreatePartition(context.Background(), "s1", PartitionCreateInput{Device: "/dev/vdb", SizeSpec: "100%", MakeLabel: true}); err != nil {
		t.Fatalf("空盘初始化应成功: %v", err)
	}
	for _, want := range []string{
		"parted -s /dev/vdb -- mklabel gpt",
		"parted -s /dev/vdb -- mkpart primary 1MiB 100%",
		"partx -u /dev/vdb",
	} {
		if !fr.called(want) {
			t.Fatalf("缺少命令: %s;实际: %v", want, fr.calls)
		}
	}
}

func TestDeletePartitionGuards(t *testing.T) {
	fr := newFakeRunner()
	svc := newTestService(fr, "root")
	unlock(svc)
	seedCollect(fr, baseScript())
	ctx := context.Background()

	// 确认文本不符。
	err := svc.DeletePartition(ctx, "s1", PartitionDeleteInput{Partition: "/dev/vdc2", Confirm: "/dev/vdc1"})
	if !errors.Is(err, ErrConfirmMismatch) {
		t.Fatalf("确认不符应拒绝: %v", err)
	}
	// 已挂载分区。
	err = svc.DeletePartition(ctx, "s1", PartitionDeleteInput{Partition: "/dev/vdc1", Confirm: "/dev/vdc1"})
	if !errors.Is(err, ErrDeviceBusy) {
		t.Fatalf("已挂载应拒绝: %v", err)
	}
	// 系统分区。
	err = svc.DeletePartition(ctx, "s1", PartitionDeleteInput{Partition: "/dev/vda2", Confirm: "/dev/vda2"})
	if !errors.Is(err, ErrProtectedDevice) {
		t.Fatalf("系统分区应拒绝: %v", err)
	}
	// PV 分区(vda3 同属受保护 VG,双重拒绝均合法)。
	err = svc.DeletePartition(ctx, "s1", PartitionDeleteInput{Partition: "/dev/vda3", Confirm: "/dev/vda3"})
	if !errors.Is(err, ErrProtectedDevice) && !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("PV/系统分区应拒绝: %v", err)
	}
}

func TestDeletePartitionSuccess(t *testing.T) {
	fr := newFakeRunner()
	svc := newTestService(fr, "root")
	unlock(svc)
	seedCollect(fr, baseScript())
	if err := svc.DeletePartition(context.Background(), "s1", PartitionDeleteInput{Partition: "/dev/vdc2", Confirm: "/dev/vdc2"}); err != nil {
		t.Fatalf("删除未挂载分区应成功: %v", err)
	}
	if !fr.called("parted -s /dev/vdc -- rm 2") {
		t.Fatalf("缺少 parted rm 命令;实际: %v", fr.calls)
	}
}

func TestFormatGuards(t *testing.T) {
	fr := newFakeRunner()
	svc := newTestService(fr, "root")
	unlock(svc)
	seedCollect(fr, baseScript())
	ctx := context.Background()

	err := svc.Format(ctx, "s1", FormatInput{Target: "/dev/vdc1", FSType: "xfs", Confirm: "/dev/vdc1"})
	if !errors.Is(err, ErrDeviceBusy) {
		t.Fatalf("已挂载应拒绝: %v", err)
	}
	err = svc.Format(ctx, "s1", FormatInput{Target: "/dev/vdc2", FSType: "xfs", Confirm: "wrong"})
	if !errors.Is(err, ErrConfirmMismatch) {
		t.Fatalf("确认不符应拒绝: %v", err)
	}
	err = svc.Format(ctx, "s1", FormatInput{Target: "/dev/vdc2", FSType: "ntfs", Confirm: "/dev/vdc2"})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("不支持的 fs 应拒绝: %v", err)
	}
}

func TestFormatSuccess(t *testing.T) {
	fr := newFakeRunner()
	svc := newTestService(fr, "root")
	unlock(svc)
	seedCollect(fr, baseScript())
	err := svc.Format(context.Background(), "s1", FormatInput{Target: "/dev/vdc2", FSType: "xfs", Label: "data", Confirm: "/dev/vdc2"})
	if err != nil {
		t.Fatalf("格式化应成功: %v", err)
	}
	if !fr.called("mkfs.xfs -f /dev/vdc2 -L data") {
		t.Fatalf("mkfs 命令异常: %v", fr.calls)
	}
}

func TestMountGuards(t *testing.T) {
	fr := newFakeRunner()
	svc := newTestService(fr, "root")
	unlock(svc)
	seedCollect(fr, baseScript())
	ctx := context.Background()

	// 系统目录内。
	err := svc.Mount(ctx, "s1", MountInput{Target: "/dev/vdc2", Mountpoint: "/var/data"})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("系统目录应拒绝: %v", err)
	}
	// 挂载点被占。
	err = svc.Mount(ctx, "s1", MountInput{Target: "/dev/vdc2", Mountpoint: "/mnt/data1"})
	if !errors.Is(err, ErrMountpointTaken) {
		t.Fatalf("挂载点被占应拒绝: %v", err)
	}
	// 目标已挂载。
	err = svc.Mount(ctx, "s1", MountInput{Target: "/dev/vdc1", Mountpoint: "/mnt/data2"})
	if !errors.Is(err, ErrAlreadyMounted) {
		t.Fatalf("已挂载目标应拒绝: %v", err)
	}
}

func TestMountSuccessPersistsFstab(t *testing.T) {
	fr := newFakeRunner()
	svc := newTestService(fr, "root")
	unlock(svc)
	seedCollect(fr, baseScript())
	fr.outputs[cmdKey([]string{"cat", fstabPath})] = &target.ExecResult{Stdout: "UUID=old /mnt/keep ext4 defaults 0 2\n"}
	fr.outputs[cmdKey([]string{"blkid", "-s", "UUID", "-o", "value", "/dev/vdc2"})] = &target.ExecResult{Stdout: "C2\n"}

	err := svc.Mount(context.Background(), "s1", MountInput{Target: "/dev/vdc2", Mountpoint: "/mnt/data2"})
	if err != nil {
		t.Fatalf("挂载应成功: %v", err)
	}
	for _, want := range []string{
		"mkdir -p /mnt/data2",
		"mount /dev/vdc2 /mnt/data2",
		"findmnt --verify --tab-file /etc/fstab",
		"findmnt --verify --tab-file /etc/.fstab.pw-tmp",
		"cp -a /etc/fstab /etc/fstab.bak-pw-",
	} {
		if !fr.called(want) && !containsPrefix(fr, want) {
			t.Fatalf("缺少命令: %s;实际: %v", want, fr.calls)
		}
	}
	stdin := fr.lastStdin()
	if !strings.Contains(stdin, "UUID=C2 /mnt/data2 xfs defaults 0 0") || !strings.Contains(stdin, "UUID=old /mnt/keep") {
		t.Fatalf("fstab 新内容异常:\n%s", stdin)
	}
}

// containsPrefix 处理带时间戳的备份文件名。
func containsPrefix(fr *fakeRunner, prefix string) bool {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	for _, c := range fr.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func TestMountFstabVerifyFailure(t *testing.T) {
	fr := newFakeRunner()
	svc := newTestService(fr, "root")
	unlock(svc)
	seedCollect(fr, baseScript())
	fr.outputs[cmdKey([]string{"blkid", "-s", "UUID", "-o", "value", "/dev/vdc2"})] = &target.ExecResult{Stdout: "C2\n"}
	// 原 fstab 校验通过、新文件校验失败 → 放弃提交。
	fr.fails[cmdKey([]string{"findmnt", "--verify", "--tab-file", fstabTmpPath})] = errors.New("bad")

	err := svc.Mount(context.Background(), "s1", MountInput{Target: "/dev/vdc2", Mountpoint: "/mnt/data2"})
	if !errors.Is(err, ErrFstabVerify) {
		t.Fatalf("校验失败应报 ErrFstabVerify: %v", err)
	}
	if containsPrefix(fr, "mv -f /etc/.fstab.pw-tmp") {
		t.Fatal("校验失败绝不应提交 fstab")
	}
}

func TestUnmountGuardAndSuccess(t *testing.T) {
	fr := newFakeRunner()
	svc := newTestService(fr, "root")
	unlock(svc)
	seedCollect(fr, baseScript())
	ctx := context.Background()

	// 系统挂载点拒绝。
	err := svc.Unmount(ctx, "s1", UnmountInput{Mountpoint: "/boot"})
	if !errors.Is(err, ErrProtectedDevice) {
		t.Fatalf("系统挂载点应拒绝: %v", err)
	}
	// 未挂载。
	err = svc.Unmount(ctx, "s1", UnmountInput{Mountpoint: "/mnt/none"})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("未挂载应拒绝: %v", err)
	}
	// 正常卸载:fstab 里已有条目应被移除。
	fr.outputs[cmdKey([]string{"cat", fstabPath})] = &target.ExecResult{
		Stdout: "UUID=C1 /mnt/data1 ext4 defaults 0 2\nUUID=old /mnt/keep ext4 defaults 0 2\n",
	}
	if err := svc.Unmount(ctx, "s1", UnmountInput{Mountpoint: "/mnt/data1", RemoveDir: true}); err != nil {
		t.Fatalf("卸载应成功: %v", err)
	}
	if !fr.called("umount /mnt/data1") || !fr.called("rmdir /mnt/data1") {
		t.Fatalf("卸载命令异常: %v", fr.calls)
	}
	stdin := fr.lastStdin()
	if strings.Contains(stdin, "/mnt/data1") || !strings.Contains(stdin, "UUID=old /mnt/keep") {
		t.Fatalf("fstab 移除结果异常:\n%s", stdin)
	}
}

func TestShrinkGuards(t *testing.T) {
	fr := newFakeRunner()
	svc := newTestService(fr, "root")
	unlock(svc)
	seedCollect(fr, baseScript())
	ctx := context.Background()

	// XFS 不支持缩容。
	err := svc.ShrinkPartition(ctx, "s1", ShrinkInput{Target: "/dev/vdc2", TargetGiB: 10, Confirm: "/dev/vdc2"})
	if !errors.Is(err, ErrFSTypeUnsupported) {
		t.Fatalf("xfs 缩容应拒绝: %v", err)
	}
	// ext4 已挂载。
	err = svc.ShrinkPartition(ctx, "s1", ShrinkInput{Target: "/dev/vdc1", TargetGiB: 30, Confirm: "/dev/vdc1"})
	if !errors.Is(err, ErrDeviceBusy) {
		t.Fatalf("ext4 已挂载缩容应拒绝: %v", err)
	}
	// btrfs 目标小于已用。
	err = svc.ShrinkPartition(ctx, "s1", ShrinkInput{Target: "/dev/vdd1", TargetGiB: 10, Confirm: "/dev/vdd1"})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("btrfs 目标过小应拒绝: %v", err)
	}
}

func TestShrinkBtrfsSuccess(t *testing.T) {
	fr := newFakeRunner()
	svc := newTestService(fr, "root")
	unlock(svc)
	seedCollect(fr, baseScript())
	err := svc.ShrinkPartition(context.Background(), "s1", ShrinkInput{Target: "/dev/vdd1", TargetGiB: 40, Confirm: "/dev/vdd1"})
	if err != nil {
		t.Fatalf("btrfs 在线缩容应成功: %v", err)
	}
	for _, want := range []string{
		"btrfs filesystem resize 42949672960 /mnt/b1",
		"parted -s /dev/vdd -- resizepart 1 40GiB",
		"btrfs filesystem resize max /mnt/b1",
	} {
		if !fr.called(want) {
			t.Fatalf("缺少命令: %s;实际: %v", want, fr.calls)
		}
	}
}

func TestGrowPartitionGuards(t *testing.T) {
	fr := newFakeRunner()
	svc := newTestService(fr, "root")
	unlock(svc)
	seedCollect(fr, baseScript())
	ctx := context.Background()

	// 非末分区(vdc2 是最后一个 → 用 vdc1 试)。
	err := svc.GrowPartition(ctx, "s1", "/dev/vdc1")
	if !errors.Is(err, ErrNotLastPartition) {
		t.Fatalf("非末分区应拒绝: %v", err)
	}
	// XFS 未挂载不可扩。
	err = svc.GrowPartition(ctx, "s1", "/dev/vdc2")
	if !errors.Is(err, ErrMountRequired) {
		t.Fatalf("xfs 未挂载扩容应拒绝: %v", err)
	}
}

// ── LVM 测试 ─────────────────────────────────────────────────────────────────

func TestLVMGuards(t *testing.T) {
	fr := newFakeRunner()
	svc := newTestService(fr, "root")
	unlock(svc)
	seedCollect(fr, baseScript())
	ctx := context.Background()

	// PVCreate:有文件系统的未挂载设备。
	err := svc.PVCreate(ctx, "s1", "/dev/vdc2")
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("有 fs 设备建 PV 应拒绝: %v", err)
	}
	// PVRemove:PV 属于 VG。
	err = svc.PVRemove(ctx, "s1", RemoveTargetInput{Target: "/dev/vda3", Confirm: "/dev/vda3"})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("在组 PV 移除应拒绝: %v", err)
	}
	// VGReduce:PV 有已分配 extent(vda3 free 3G < size 18G)。
	err = svc.VGReduce(ctx, "s1", VGReduceInput{Name: "rootvg", Device: "/dev/vda3", Confirm: "/dev/vda3"})
	if !errors.Is(err, ErrDeviceBusy) {
		t.Fatalf("有分配 extent 的摘盘应拒绝: %v", err)
	}
	// VGRemove:VG 内有 LV。
	err = svc.VGRemove(ctx, "s1", RemoveConfirmInput{Name: "datavg", Confirm: "datavg"})
	if !errors.Is(err, ErrDeviceBusy) {
		t.Fatalf("非空 VG 移除应拒绝: %v", err)
	}
	// LVRemove:系统 LV。
	err = svc.LVRemove(ctx, "s1", RemoveTargetInput{Target: "/dev/rootvg/root", Confirm: "/dev/rootvg/root"})
	if !errors.Is(err, ErrProtectedDevice) {
		t.Fatalf("系统 LV 移除应拒绝: %v", err)
	}
}

func TestLVMOperations(t *testing.T) {
	fr := newFakeRunner()
	svc := newTestService(fr, "root")
	unlock(svc)
	seedCollect(fr, baseScript())
	ctx := context.Background()

	// 空盘建 PV。
	if err := svc.PVCreate(ctx, "s1", "/dev/vdb"); err != nil {
		t.Fatalf("空盘建 PV 应成功: %v", err)
	}
	if !fr.called("pvcreate /dev/vdb") {
		t.Fatalf("pvcreate 命令异常: %v", fr.calls)
	}
	// LV 扩容用尽剩余。
	if err := svc.LVGrow(ctx, "s1", LVGrowInput{Path: "/dev/datavg/lv1", UseAllFree: true}); err != nil {
		t.Fatalf("LV 扩容应成功: %v", err)
	}
	if !fr.called("lvextend -l +100%FREE /dev/datavg/lv1") {
		t.Fatalf("lvextend 命令异常: %v", fr.calls)
	}
	// ext4 LV 离线缩容链路。
	err := svc.LVShrink(ctx, "s1", ShrinkInput{Target: "/dev/datavg/lv1", TargetGiB: 5, Confirm: "/dev/datavg/lv1"})
	if err != nil {
		t.Fatalf("LV 缩容应成功: %v", err)
	}
	for _, want := range []string{
		"e2fsck -f /dev/datavg/lv1",
		"resize2fs /dev/datavg/lv1 4G",
		"lvreduce --yes -L 5G /dev/datavg/lv1",
		"resize2fs /dev/datavg/lv1",
	} {
		if !fr.called(want) {
			t.Fatalf("缺少命令: %s;实际: %v", want, fr.calls)
		}
	}
}

func TestLVCreateWithFormatMount(t *testing.T) {
	fr := newFakeRunner()
	svc := newTestService(fr, "root")
	unlock(svc)
	seedCollect(fr, baseScript())
	fr.outputs[cmdKey([]string{"blkid", "-s", "UUID", "-o", "value", "/dev/datavg/newlv"})] = &target.ExecResult{Stdout: "NEW1\n"}

	err := svc.LVCreate(context.Background(), "s1", LVCreateInput{
		VG: "datavg", Name: "newlv", UseAllFree: true, FSType: "ext4", Mountpoint: "/mnt/newlv",
	})
	if err != nil {
		t.Fatalf("建 LV+格式化+挂载应成功: %v", err)
	}
	for _, want := range []string{
		"lvcreate -n newlv -l 100%FREE datavg",
		"mkfs.ext4 -F /dev/datavg/newlv",
		"mount /dev/datavg/newlv /mnt/newlv",
	} {
		if !fr.called(want) {
			t.Fatalf("缺少命令: %s;实际: %v", want, fr.calls)
		}
	}
	if !strings.Contains(fr.lastStdin(), "UUID=NEW1 /mnt/newlv ext4 defaults 0 2") {
		t.Fatalf("fstab 条目异常:\n%s", fr.lastStdin())
	}
}

func TestTopologyView(t *testing.T) {
	fr := newFakeRunner()
	seedCollect(fr, baseScript())
	svc := newTestService(fr, "root")
	topo, err := svc.Topology(context.Background(), "s1")
	if err != nil {
		t.Fatalf("Topology 应成功: %v", err)
	}
	if topo.LoginUser != "root" || !topo.Privileged || topo.Unlocked {
		t.Fatalf("登录/提权/解锁状态异常: %+v", topo)
	}
	// 解锁后视图翻转。
	svc.Unlock("s1")
	topo, _ = svc.Topology(context.Background(), "s1")
	if !topo.Unlocked || topo.UnlockExpiresAt == "" {
		t.Fatal("解锁后状态应可见")
	}
	svc.Lock("s1")
	topo, _ = svc.Topology(context.Background(), "s1")
	if topo.Unlocked {
		t.Fatal("回锁后状态应翻转")
	}
}

func TestLvmMissing(t *testing.T) {
	fr := newFakeRunner()
	svc := newTestService(fr, "root")
	unlock(svc)
	seedCollect(fr, strings.ReplaceAll(baseScript(), "pvs ok\n", ""))
	err := svc.PVCreate(context.Background(), "s1", "/dev/vdb")
	if !errors.Is(err, ErrLvmMissing) {
		t.Fatalf("缺 lvm2 应报 ErrLvmMissing: %v", err)
	}
}
