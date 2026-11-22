package pveadmin

// create_test.go — 建机域测试:cloud-config golden、入参校验、三管线调用序列、
// 批量容错、import-from SSH 兜底、snippet 落盘、登记幂等、readonly/ErrBusy 闸门。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// ---- cloud-config golden ---------------------------------------------------------------

// TestRenderCloudConfigGolden 全开关组合:主机名恒定注入、凭据、runcmd 顺序(换源先于装 agent)。
func TestRenderCloudConfigGolden(t *testing.T) {
	yaml, err := renderCloudConfig(CloudInitOptions{
		Hostname: "web-1", User: "ops", Password: "p@ss:word",
		SSHKeys:         []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample ops@host"},
		SSHPasswordAuth: true, Timezone: "Asia/Shanghai", DNS: []string{"223.5.5.5"},
		Mirror: "ustc", InstallAgent: true, DisableSELinux: true, DisableFirewall: true,
		RootExpand: true, FixStatic: true,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"#cloud-config", "hostname: web-1", "preserve_hostname: false", "timezone: Asia/Shanghai",
		"ssh_pwauth: true", "- name: ops", "lock_passwd: false", "sudo: ALL=(ALL) NOPASSWD:ALL",
		"ops:p@ss:word", "expire: false", "ssh-ed25519 AAAA", "PasswordAuthentication yes",
		"mirrors.ustc.edu.cn", "qemu-guest-agent", "SELINUX=disabled", "firewalld", "growpart",
		"dhcp_to_static_done", "hostnamectl set-hostname web-1",
	} {
		if !strings.Contains(yaml, want) {
			t.Fatalf("yaml 缺 %q:\n%s", want, yaml)
		}
	}
	// 顺序:换源必须排在装 agent 之前;hostnamectl 在换源之后。
	iMirror := strings.Index(yaml, "mirrors.ustc.edu.cn")
	iAgent := strings.Index(yaml, "apt-get install -y qemu-guest-agent")
	iHost := strings.Index(yaml, "hostnamectl set-hostname")
	if !(iMirror < iHost && iHost < iAgent) {
		t.Fatalf("runcmd 顺序错误: mirror=%d host=%d agent=%d", iMirror, iHost, iAgent)
	}
	// 无密码 → 锁定密码登录,无 chpasswd。
	yaml2, err := renderCloudConfig(CloudInitOptions{Hostname: "k-1", SSHKeys: []string{"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQDx example"}})
	if err != nil {
		t.Fatalf("render2: %v", err)
	}
	if !strings.Contains(yaml2, "lock_passwd: true") || strings.Contains(yaml2, "chpasswd") {
		t.Fatalf("无密码应锁定密码登录:\n%s", yaml2)
	}
}

// TestRenderCloudConfigValidation 非法入参拒绝(主机名/密码换行/密钥格式/DNS/时区/换源预设)。
func TestRenderCloudConfigValidation(t *testing.T) {
	bad := []CloudInitOptions{
		{Hostname: "bad name"},
		{Hostname: "ok", Password: "line\nbreak"},
		{Hostname: "ok", SSHKeys: []string{"not-a-key"}},
		{Hostname: "ok", DNS: []string{"999.1.1.1"}},
		{Hostname: "ok", Timezone: "Bad/Zone!"},
		{Hostname: "ok", Mirror: "unknown"},
		{Hostname: "ok", User: "bad user"},
	}
	for i, o := range bad {
		if _, err := renderCloudConfig(o); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("case %d: err = %v, want ErrInvalidInput", i, err)
		}
	}
}

// ---- 批量助手 ---------------------------------------------------------------------------

func TestBatchNameAndIncrementIP(t *testing.T) {
	if batchName("web", 0, 1) != "web" || batchName("web", 2, 3) != "web-3" {
		t.Fatal("batchName")
	}
	if incrementIP("10.1.0.250", 0) != "10.1.0.250" || incrementIP("10.1.0.250", 2) != "10.1.0.252" {
		t.Fatal("incrementIP 进位")
	}
	if incrementIP("10.1.0.255", 1) != "10.1.1.0" {
		t.Fatal("incrementIP 跨 octet")
	}
}

// ---- 入参校验 ---------------------------------------------------------------------------

func TestValidateCreateInput(t *testing.T) {
	base := CreateInput{Mode: CreateModeImage, Node: "pve1", Name: "web", Storage: "local", ImageVolid: "local:import/x.qcow2"}
	if _, err := validateCreateInput(base); err != nil {
		t.Fatalf("base: %v", err)
	}
	bad := []CreateInput{
		{Mode: "vmware", Node: "pve1", Name: "x"},
		{Mode: CreateModeImage, Node: "pve1", Name: "x", Storage: "local"},                                                              // 源缺失
		{Mode: CreateModeImage, Node: "pve1", Name: "x", Storage: "local", ImageURL: "https://h/x.qcow2", ImageVolid: "local:import/x"}, // 双源
		{Mode: CreateModeImage, Node: "pve1", Name: "x", ImageVolid: "local:import/x"},                                                  // 缺存储
		{Mode: CreateModeClone, Name: "x", TemplateVMID: 9000, BackupVolid: "local:backup/y.vma"},                                       // 双源
		{Mode: CreateModeLXC, Node: "pve1", Name: "x"},                                                                                  // 源缺失
		{Mode: CreateModeImage, Node: "pve1", Name: "x", Storage: "local", ImageVolid: "v", Count: 51},
		{Mode: CreateModeImage, Node: "pve1", Name: "x", Storage: "local", ImageVolid: "v", IPMode: "static", IP: "not-ip"},
		{Mode: CreateModeImage, Node: "pve1", Name: "x", Storage: "local", ImageVolid: "v", IPMode: "static", IP: "10.0.0.1", Prefix: 33},
		{Mode: CreateModeImage, Node: "pve1", Name: "x", Storage: "local", ImageVolid: "v", Register: &RegisterInput{SSHUser: "root"}},                                                                                 // 凭据缺失
		{Mode: CreateModeImage, Node: "pve1", Name: "x", Storage: "local", ImageVolid: "v", Register: &RegisterInput{SSHUser: "root", CredentialID: "c1", InlineSecret: &InlineSecret{Kind: "password", Secret: "s"}}}, // 双凭据
	}
	for i, in := range bad {
		if _, err := validateCreateInput(in); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("case %d: err = %v, want ErrInvalidInput", i, err)
		}
	}
	// 缺省归一。
	in, err := validateCreateInput(CreateInput{Mode: CreateModeLXC, Node: "pve1", Name: "ct", OSTemplate: "local:vztmpl/x.tar.zst"})
	if err != nil {
		t.Fatalf("lxc: %v", err)
	}
	if in.Count != 1 || in.Cores != 2 || in.Memory != 2048 || in.Bridge != "vmbr0" || in.IPMode != "dhcp" {
		t.Fatalf("defaults: %+v", in)
	}
}

// TestListTemplatesQemuOnly 尚未实现 LXC 克隆,不能把它暴露为必然失败的克隆源。
func TestListTemplatesQemuOnly(t *testing.T) {
	s, _, _, fc := newTestService(t)
	cl := seedCluster(t, s)
	seedReadyToken(t, s, cl.ID)
	fc.getFn = func(path string) (any, error) {
		if strings.Contains(path, "/cluster/resources") {
			return []map[string]any{
				{"type": "qemu", "node": "pve1", "name": "vm-tpl", "vmid": 9000, "template": 1},
				{"type": "lxc", "node": "pve1", "name": "ct-tpl", "vmid": 9001, "template": 1},
			}, nil
		}
		return nil, fmt.Errorf("unexpected GET %s", path)
	}
	items, err := s.ListTemplates(context.Background(), cl.ID)
	if err != nil || len(items) != 1 || items[0].Type != "qemu" || items[0].VMID != 9000 {
		t.Fatalf("templates=%+v err=%v", items, err)
	}
}

// ---- 管线测试脚手架 ---------------------------------------------------------------------

// seedCreateCluster 建 ready 集群 + pve 池 + 节点登记机器(snippet/SSH 兜底前提)。
func seedCreateCluster(t *testing.T, s *service, ft *fakeTarget) *Cluster {
	t.Helper()
	cl := seedCluster(t, s)
	seedReadyToken(t, s, cl.ID)
	if err := s.SetReadonly(context.Background(), cl.ID, false); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	poolID, _, err := s.ensurePool(context.Background(), cl.ID, cl.Name)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	ft.db = s.db // Create 同步写真 servers 行(pve_vm_links 外键)
	ft.servers["node1"] = &target.Server{ID: "node1", Name: "pve1", Host: "10.1.0.11", Port: 22, User: "root", PoolID: poolID, Kind: target.KindPVENode}
	return cl
}

// seedSSHCredential 代建一条真实 vault SSH 凭据(servers 外键需要真实行)。
func seedSSHCredential(t *testing.T, s *service) string {
	t.Helper()
	cred, err := s.vault.Create(vault.CreateInput{Name: "reg-cred", Type: vault.TypeSSHPassword, Secret: "pw"})
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	return cred.ID
}

// wireCreateFake 接标准建机响应(version/nextid/resources/tasks/config/storage/agent)。
func wireCreateFake(fc *fakePVEClient, release string) {
	fc.getFn = func(path string) (any, error) {
		switch {
		case strings.HasSuffix(path, "/version"):
			return apiVersion{Release: release}, nil
		case strings.Contains(path, "/cluster/nextid"):
			return "100", nil
		case strings.Contains(path, "/cluster/resources"):
			return []map[string]any{
				{"id": "qemu/9000", "type": "qemu", "node": "pve1", "name": "tpl", "vmid": "9000", "template": 1},
			}, nil
		case strings.Contains(path, "/tasks/"):
			return map[string]any{"status": "stopped", "exitstatus": "OK"}, nil
		case strings.Contains(path, "/nodes/") && strings.HasSuffix(path, "/storage"):
			return []map[string]any{
				{"storage": "local", "type": "dir", "active": 1, "enabled": 1, "content": "images,snippets,backup,import,vztmpl"},
			}, nil
		case strings.HasSuffix(path, "/storage/local"):
			return map[string]any{"path": "/var/lib/vz", "type": "dir"}, nil
		case strings.Contains(path, "/qemu/") && strings.HasSuffix(path, "/config"):
			return map[string]any{"boot": "order=scsi0", "scsi0": "local:vm-100-disk-0"}, nil
		case strings.Contains(path, "/agent/network-get-interfaces"):
			return map[string]any{"result": []map[string]any{
				{"name": "eth0", "ip-addresses": []map[string]any{{"ip-address-type": "ipv4", "ip-address": "10.1.0.50"}}},
			}}, nil
		case strings.Contains(path, "/aplinfo"):
			return []map[string]any{{"template": "ubuntu-24.04-standard", "location": "system/ubuntu-24.04-standard_24.04-1_amd64.tar.zst"}}, nil
		}
		return nil, fmt.Errorf("unexpected GET %s", path)
	}
	fc.postFn = func(path string) (any, error) {
		switch {
		case strings.HasSuffix(path, "/config"):
			return nil, nil // 同步改配:data=null
		case strings.Contains(path, "/clone"), strings.HasSuffix(path, "/qemu"), strings.HasSuffix(path, "/lxc"),
			strings.Contains(path, "/status/start"), strings.Contains(path, "/aplinfo"),
			strings.Contains(path, "/download-url"), strings.Contains(path, "/content"):
			return "UPID:pve1:0001:1:task:100:", nil
		}
		return nil, fmt.Errorf("unexpected POST %s", path)
	}
	fc.putFn = func(path string, _ url.Values) (any, error) {
		if strings.Contains(path, "/config") || strings.Contains(path, "/resize") {
			return "UPID:pve1:0002:1:qmconfig:100:", nil
		}
		return nil, nil // cloudinit 同步再生
	}
}

// waitCreateDone 轮询任务快照至 done(5s 上限)。
func waitCreateDone(t *testing.T, s *service, id string) CreateStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := s.CreateStatus(context.Background(), id)
		if err != nil {
			t.Fatalf("CreateStatus: %v", err)
		}
		if st.Done {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("建机任务 5s 未完成")
	return CreateStatus{}
}

// ---- 管线 A:模板克隆 --------------------------------------------------------------------

// TestClonePipelineSequence 克隆 → 改配 → 扩容 → 启动;表单形态与模板节点解析。
func TestClonePipelineSequence(t *testing.T) {
	s, ft, _, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireCreateFake(fc, "9.0.3")

	in := CreateInput{Mode: CreateModeClone, Name: "web", TemplateVMID: 9000, Storage: "local",
		Cores: 4, Memory: 4096, DiskGB: 40, Bridge: "vmbr1", IPMode: "dhcp"}
	if err := s.StartCreate(context.Background(), cl.ID, in); err != nil {
		t.Fatalf("StartCreate: %v", err)
	}
	st := waitCreateDone(t, s, cl.ID)
	if st.Error != "" || st.OK != 1 || st.Failed != 0 {
		t.Fatalf("status: %+v", st)
	}
	if st.Items[0].VMID != 100 || st.Items[0].Name != "web" {
		t.Fatalf("item: %+v", st.Items[0])
	}
	// 调用序列:clone → config(POST) → resize(PUT) → start。
	var seq []string
	for _, p := range fc.posts {
		seq = append(seq, "POST "+p)
	}
	joined := strings.Join(seq, "\n")
	if !strings.Contains(joined, "/nodes/pve1/qemu/9000/clone") {
		t.Fatalf("缺克隆调用(模板节点解析):\n%s", joined)
	}
	if !strings.Contains(joined, "/nodes/pve1/qemu/100/status/start") {
		t.Fatalf("缺启动调用:\n%s", joined)
	}
	// 克隆表单:newid/full/name/storage。
	var cloneForm url.Values
	for i, p := range fc.posts {
		if strings.Contains(p, "/clone") {
			cloneForm = fc.postForms[i]
		}
	}
	if cloneForm.Get("newid") != "100" || cloneForm.Get("full") != "1" || cloneForm.Get("name") != "web" || cloneForm.Get("storage") != "local" {
		t.Fatalf("clone form: %v", cloneForm)
	}
	// 扩容 PUT:绝对尺寸。
	if len(fc.puts) == 0 || !strings.Contains(fc.puts[0], "/resize") || fc.putForms[0].Get("size") != "40G" || fc.putForms[0].Get("disk") != "scsi0" {
		t.Fatalf("resize: %v %v", fc.puts, fc.putForms)
	}
	// 无 cloudinit 盘 + dhcp:不写 snippet。
	if len(fc.putForms) > 1 {
		t.Fatalf("不应有 cloudinit PUT: %v", fc.puts)
	}
}

// TestQemuConfigFormExplicitFalse 克隆/恢复时显式关闭 agent/onboot 必须覆盖源配置。
func TestQemuConfigFormExplicitFalse(t *testing.T) {
	disabled := false
	form := qemuConfigForm(CreateInput{Cores: 2, Memory: 2048, Bridge: "vmbr0", Agent: &disabled, Onboot: false}, "vm")
	if form.Get("agent") != "enabled=0" || form.Get("onboot") != "0" {
		t.Fatalf("显式关闭未提交: %v", form)
	}
}

// TestCloneReadonlyAndBusy 只读拒绝 + 任务占用 ErrBusy。
func TestCloneReadonlyAndBusy(t *testing.T) {
	s, ft, _, fc := newTestService(t)
	cl := seedCluster(t, s)
	seedReadyToken(t, s, cl.ID) // readonly 保持默认 1
	in := CreateInput{Mode: CreateModeClone, Name: "web", TemplateVMID: 9000}
	if err := s.StartCreate(context.Background(), cl.ID, in); !errors.Is(err, ErrReadonly) {
		t.Fatalf("readonly err = %v", err)
	}
	_ = s.SetReadonly(context.Background(), cl.ID, false)
	wireCreateFake(fc, "9.0.3")
	poolID, _, _ := s.ensurePool(context.Background(), cl.ID, cl.Name)
	ft.servers["node1"] = &target.Server{ID: "node1", Name: "pve1", Host: "10.1.0.11", PoolID: poolID, Kind: target.KindPVENode, User: "root"}
	innerGet := fc.getFn
	taskStarted := make(chan struct{}, 1)
	releaseTask := make(chan struct{})
	fc.getFn = func(path string) (any, error) {
		if strings.Contains(path, "/tasks/") {
			select {
			case taskStarted <- struct{}{}:
			default:
			}
			<-releaseTask
		}
		return innerGet(path)
	}
	if err := s.StartCreate(context.Background(), cl.ID, in); err != nil {
		t.Fatalf("start: %v", err)
	}
	<-taskStarted
	if err := s.StartCreate(context.Background(), cl.ID, in); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy err = %v", err)
	}
	if err := s.DeleteCluster(context.Background(), cl.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("delete running cluster err = %v", err)
	}
	close(releaseTask)
	waitCreateDone(t, s, cl.ID)
}

// ---- 管线 B:云镜像 ----------------------------------------------------------------------

// TestImagePipelineSnippetAndImport 9.x:download-url → snippet(SSH 0600)→ 建 VM →
// import-from(PUT)→ cloudinit 再生 → 启动;断言 snippet 内容与 cicustom 引用。
func TestImagePipelineSnippetAndImport(t *testing.T) {
	s, ft, fr, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireCreateFake(fc, "9.0.3")

	in := CreateInput{Mode: CreateModeImage, Node: "pve1", Name: "cloud", Storage: "local",
		ImageURL: "https://mirrors.example/ubuntu-24.04.qcow2.gz", Cores: 2, Memory: 2048, DiskGB: 20,
		IPMode: "static", IP: "10.1.0.60", Gateway: "10.1.0.1", Prefix: 24,
		CIUser: "ops", CIPassword: "secret-pw", SSHPasswordAuth: true, Mirror: "aliyun"}
	if err := s.StartCreate(context.Background(), cl.ID, in); err != nil {
		t.Fatalf("StartCreate: %v", err)
	}
	st := waitCreateDone(t, s, cl.ID)
	if st.Error != "" || st.OK != 1 {
		t.Fatalf("status: %+v log=%v", st, st.Log)
	}
	// 下载端点(9.x download-url)+ 解压文件名。
	dl := ""
	for i, p := range fc.posts {
		if strings.Contains(p, "/download-url") {
			dl = p
			if fc.postForms[i].Get("filename") != "ubuntu-24.04.qcow2" || fc.postForms[i].Get("compression") != "gz" || fc.postForms[i].Get("content") != "import" {
				t.Fatalf("download form: %v", fc.postForms[i])
			}
		}
	}
	if dl == "" {
		t.Fatalf("缺 download-url 调用: %v", fc.posts)
	}
	// snippet:路径/权限/内容(含主机名与密码,但日志不含)。
	if len(fr.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(fr.writes))
	}
	w := fr.writes[0]
	if !strings.HasPrefix(w.path, "/var/lib/vz/snippets/cloud-") || !strings.HasSuffix(w.path, "-100-user.yaml") || w.mode != 0o600 || w.serverID != "node1" {
		t.Fatalf("write: %+v", w)
	}
	snippetName := strings.TrimPrefix(w.path, "/var/lib/vz/snippets/")
	if snippetName == "cloud-100-user.yaml" {
		t.Fatal("snippet 必须包含任务级唯一标识,不能按 VMID 覆盖")
	}
	if !strings.Contains(w.content, "hostname: cloud") || !strings.Contains(w.content, "ops:secret-pw") || !strings.Contains(w.content, "mirrors.aliyun.com") {
		t.Fatalf("snippet 内容:\n%s", w.content)
	}
	for _, line := range st.Log {
		if strings.Contains(line, "secret-pw") {
			t.Fatalf("日志泄漏密码: %s", line)
		}
	}
	// 建 VM 表单:cicustom/ipconfig0/ide2/efidisk0/bios。
	var createForm url.Values
	for i, p := range fc.posts {
		if strings.HasSuffix(p, "/nodes/pve1/qemu") {
			createForm = fc.postForms[i]
		}
	}
	if createForm.Get("cicustom") != "user=local:snippets/"+snippetName ||
		createForm.Get("ipconfig0") != "ip=10.1.0.60/24,gw=10.1.0.1" ||
		createForm.Get("ide2") != "local:cloudinit" || createForm.Get("bios") != "ovmf" ||
		createForm.Get("efidisk0") != "local:0,efitype=4m,pre-enrolled-keys=0" {
		t.Fatalf("create form: %v", createForm)
	}
	// 磁盘导入:PUT config import-from;DiskGB 随后落实为绝对尺寸扩容。
	if len(fc.puts) == 0 || fc.putForms[0].Get("scsi0") != "local:0,import-from=local:import/ubuntu-24.04.qcow2" {
		t.Fatalf("import: %v %v", fc.puts, fc.putForms)
	}
	resized := false
	for i, p := range fc.puts {
		if strings.Contains(p, "/resize") && fc.putForms[i].Get("disk") == "scsi0" && fc.putForms[i].Get("size") == "20G" {
			resized = true
		}
	}
	if !resized {
		t.Fatalf("云镜像 DiskGB 未应用: %v %v", fc.puts, fc.putForms)
	}
}

// TestImageImportFallbackSSH import-from 被拒 → SSH qm importdisk 兜底 → 挂盘。
func TestImageImportFallbackSSH(t *testing.T) {
	s, ft, fr, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireCreateFake(fc, "8.2.4")
	fr.importOut = "Successfully imported disk as 'unused0:local:vm-100-disk-0'\n"
	fc.putFn = func(path string, form url.Values) (any, error) {
		if strings.Contains(path, "/config") && strings.Contains(form.Get("scsi0"), "import-from") {
			return nil, fmt.Errorf("%w: PUT config → HTTP 501: unknown parameter import-from", ErrConnectFailed)
		}
		return nil, nil
	}
	in := CreateInput{Mode: CreateModeImage, Node: "pve1", Name: "old", Storage: "local", ImageVolid: "local:import/x.qcow2"}
	if err := s.StartCreate(context.Background(), cl.ID, in); err != nil {
		t.Fatalf("StartCreate: %v", err)
	}
	st := waitCreateDone(t, s, cl.ID)
	if st.Error != "" || st.OK != 1 {
		t.Fatalf("status: %+v log=%v", st, st.Log)
	}
	// 兜底后挂盘:POST config scsi0=导入卷。
	attached := false
	for i, p := range fc.posts {
		if strings.HasSuffix(p, "/config") && fc.postForms[i].Get("scsi0") == "local:vm-100-disk-0" {
			attached = true
		}
	}
	if !attached {
		t.Fatalf("缺兜底挂盘调用: %v", fc.postForms)
	}
	for _, cmd := range fr.runs {
		if len(cmd) > 1 && cmd[0] == "qm" && cmd[1] == "importdisk" {
			if len(cmd) != 5 {
				t.Fatalf("importdisk 不应强制 qcow2(块存储须由 PVE 选格式): %v", cmd)
			}
		}
	}
	if isImportFromUnsupported(fmt.Errorf("%w: PUT config: context deadline exceeded", ErrConnectFailed)) {
		t.Fatal("网络结果不确定时不得触发重复 SSH 导入")
	}
}

// TestImageDownload8xFallback 8.x:content+url 端点;import 被拒 → images 回退。
func TestImageDownload8xFallback(t *testing.T) {
	s, ft, _, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireCreateFake(fc, "8.1.0")
	// images 回退成功:第一次 content(import)被拒,第二次(images)放行。
	calls := 0
	fc.postFn = func(path string) (any, error) {
		if strings.Contains(path, "/content") {
			calls++
			if calls == 1 {
				return nil, fmt.Errorf("%w: HTTP 400: invalid content type import", ErrConnectFailed)
			}
			return "UPID:pve1:0001:1:download:100:", nil
		}
		if strings.HasSuffix(path, "/config") {
			return nil, nil
		}
		return "UPID:pve1:0001:1:task:100:", nil
	}
	in := CreateInput{Mode: CreateModeImage, Node: "pve1", Name: "old8", Storage: "local", ImageURL: "https://h/x.qcow2"}
	if err := s.StartCreate(context.Background(), cl.ID, in); err != nil {
		t.Fatalf("StartCreate: %v", err)
	}
	st := waitCreateDone(t, s, cl.ID)
	if st.Error != "" || st.OK != 1 {
		t.Fatalf("status: %+v log=%v", st, st.Log)
	}
	if calls != 2 {
		t.Fatalf("content 调用次数 = %d, want 2(import→images 回退)", calls)
	}
}

// TestImageBatchDownloadsOnce 同一 URL 批量建机只下载一次并复用卷。
func TestImageBatchDownloadsOnce(t *testing.T) {
	s, ft, _, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireCreateFake(fc, "9.0.3")
	innerPost := fc.postFn
	downloads := 0
	fc.postFn = func(path string) (any, error) {
		if strings.Contains(path, "/download-url") {
			downloads++
		}
		return innerPost(path)
	}
	in := CreateInput{Mode: CreateModeImage, Node: "pve1", Name: "cloud", Storage: "local", Count: 2,
		ImageURL: "https://mirrors.example/cloud.qcow2"}
	if err := s.StartCreate(context.Background(), cl.ID, in); err != nil {
		t.Fatalf("StartCreate: %v", err)
	}
	st := waitCreateDone(t, s, cl.ID)
	if st.Error != "" || st.OK != 2 || downloads != 1 {
		t.Fatalf("status=%+v downloads=%d log=%v", st, downloads, st.Log)
	}
}

// ---- 管线 C:LXC -------------------------------------------------------------------------

// TestLXCPipelineStaticIP LXC 静态 IP 进 net0;nesting/unprivileged/数据盘/密码表单。
func TestLXCPipelineStaticIP(t *testing.T) {
	s, ft, _, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireCreateFake(fc, "9.0.3")
	in := CreateInput{Mode: CreateModeLXC, Node: "pve1", Name: "ct", OSTemplate: "local:vztmpl/ubuntu.tar.zst", Storage: "fast", DiskGB: 12,
		IPMode: "static", IP: "10.1.0.70", Gateway: "10.1.0.1", Prefix: 24, DNS: "223.5.5.5,119.29.29.29",
		LXCPassword: "ct-pw", DataGB: 8, DataStorage: "local"}
	if err := s.StartCreate(context.Background(), cl.ID, in); err != nil {
		t.Fatalf("StartCreate: %v", err)
	}
	st := waitCreateDone(t, s, cl.ID)
	if st.Error != "" || st.OK != 1 {
		t.Fatalf("status: %+v log=%v", st, st.Log)
	}
	var form url.Values
	for i, p := range fc.posts {
		if strings.HasSuffix(p, "/nodes/pve1/lxc") {
			form = fc.postForms[i]
		}
	}
	if form.Get("net0") != "name=eth0,bridge=vmbr0,ip=10.1.0.70/24,gw=10.1.0.1" ||
		form.Get("rootfs") != "fast:12" ||
		form.Get("unprivileged") != "1" || form.Get("features") != "nesting=1,keyctl=1" ||
		form.Get("mp0") != "local:8,mp=/data" || form.Get("nameserver") != "223.5.5.5 119.29.29.29" ||
		form.Get("hostname") != "ct" || form.Get("password") != "ct-pw" {
		t.Fatalf("lxc form: %v", form)
	}
}

// TestLXCAplDownload 任务内在线下载模板:aplinfo location 基名定位卷。
func TestLXCAplDownload(t *testing.T) {
	s, ft, _, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireCreateFake(fc, "9.0.3")
	in := CreateInput{Mode: CreateModeLXC, Node: "pve1", Name: "ct2",
		AplDownload: &AplDownloadInput{Storage: "local", Template: "ubuntu-24.04-standard"}}
	if err := s.StartCreate(context.Background(), cl.ID, in); err != nil {
		t.Fatalf("StartCreate: %v", err)
	}
	st := waitCreateDone(t, s, cl.ID)
	if st.Error != "" || st.OK != 1 {
		t.Fatalf("status: %+v log=%v", st, st.Log)
	}
	var form url.Values
	for i, p := range fc.posts {
		if strings.HasSuffix(p, "/nodes/pve1/lxc") {
			form = fc.postForms[i]
		}
	}
	if form.Get("ostemplate") != "local:vztmpl/ubuntu-24.04-standard_24.04-1_amd64.tar.zst" {
		t.Fatalf("ostemplate: %v", form)
	}
}

// ---- 批量容错 ---------------------------------------------------------------------------

// TestBatchPerItemTolerance 第二台克隆失败不阻断第三台;逐台结果与计数。
func TestBatchPerItemTolerance(t *testing.T) {
	s, ft, _, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireCreateFake(fc, "9.0.3")
	// 按调用序:第 2 次 clone 失败,其余放行。
	cloneCalls := 0
	fc.postFn = func(path string) (any, error) {
		if strings.Contains(path, "/clone") {
			cloneCalls++
			if cloneCalls == 2 {
				return nil, fmt.Errorf("%w: clone → HTTP 500: disk full", ErrConnectFailed)
			}
		}
		if strings.HasSuffix(path, "/config") {
			return nil, nil
		}
		return "UPID:pve1:0001:1:task:100:", nil
	}
	in := CreateInput{Mode: CreateModeClone, Name: "web", TemplateVMID: 9000, Count: 3, IPMode: "static", IP: "10.1.0.80", Prefix: 24}
	if err := s.StartCreate(context.Background(), cl.ID, in); err != nil {
		t.Fatalf("StartCreate: %v", err)
	}
	st := waitCreateDone(t, s, cl.ID)
	if st.Total != 3 || st.OK != 2 || st.Failed != 1 {
		t.Fatalf("counts: %+v", st)
	}
	if st.Items[0].Name != "web-1" || st.Items[1].Name != "web-2" || st.Items[2].Name != "web-3" {
		t.Fatalf("names: %+v", st.Items)
	}
	if st.Items[0].IP != "10.1.0.80" || st.Items[2].IP != "10.1.0.82" {
		t.Fatalf("ips: %+v", st.Items)
	}
	if !st.Items[0].OK || st.Items[1].OK || !st.Items[2].OK || st.Items[1].Err == "" {
		t.Fatalf("tolerance: %+v", st.Items)
	}
}

// ---- 登记 -------------------------------------------------------------------------------

// TestRegisterCreatedGuest 静态 IP + 已有凭据 → standard 服务器 + 映射;二次登记更新原行。
func TestRegisterCreatedGuest(t *testing.T) {
	s, ft, _, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireCreateFake(fc, "9.0.3")
	credID := seedSSHCredential(t, s)
	in := CreateInput{Mode: CreateModeClone, Name: "reg", TemplateVMID: 9000,
		IPMode: "static", IP: "10.1.0.90", Prefix: 24,
		Register: &RegisterInput{SSHUser: "root", SSHPort: 22, CredentialID: credID, WaitSeconds: 1}}
	if err := s.StartCreate(context.Background(), cl.ID, in); err != nil {
		t.Fatalf("StartCreate: %v", err)
	}
	st := waitCreateDone(t, s, cl.ID)
	if st.Error != "" || st.OK != 1 || st.Items[0].ServerID == "" || st.Items[0].RegisterErr != "" {
		t.Fatalf("status: %+v log=%v", st, st.Log)
	}
	sv, err := ft.Get(context.Background(), st.Items[0].ServerID)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if sv.Host != "10.1.0.90" || sv.Kind != target.KindStandard || sv.User != "root" || sv.CredentialID != credID {
		t.Fatalf("server: %+v", sv)
	}
	// 映射落库。
	var serverID string
	if err := s.db.QueryRow(`SELECT server_id FROM pve_vm_links WHERE cluster_id = ? AND node = ? AND vmid = ?`, cl.ID, "pve1", 100).Scan(&serverID); err != nil || serverID != sv.ID {
		t.Fatalf("link: %q / %v", serverID, err)
	}
	// 幂等:同 vmid 再登记 → 更新原行(不新建)。管线内节点名已解析,直调须带上。
	before := len(ft.servers)
	in.Node = "pve1"
	sid, err := s.registerCreatedGuest(context.Background(), &createJob{}, cl, fc, in, 100, "reg", "10.1.0.91", &CreateItemResult{})
	if err != nil || sid != sv.ID {
		t.Fatalf("second register: %q / %v", sid, err)
	}
	if len(ft.servers) != before {
		t.Fatalf("二次登记不应新建机器行: %d → %d", before, len(ft.servers))
	}
	if updated, _ := ft.Get(context.Background(), sid); updated.Host != "10.1.0.91" {
		t.Fatalf("update host: %+v", updated)
	}
}

// TestRegisterInlineCredential 内联凭据代建 vault 凭据(明文不入库)。
func TestRegisterInlineCredential(t *testing.T) {
	s, ft, _, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireCreateFake(fc, "9.0.3")
	in := CreateInput{Mode: CreateModeClone, Name: "inl", TemplateVMID: 9000, Count: 2,
		Register: &RegisterInput{SSHUser: "root", WaitSeconds: 1,
			InlineSecret: &InlineSecret{Kind: "password", Secret: "inline-pw"}}}
	if err := s.StartCreate(context.Background(), cl.ID, in); err != nil {
		t.Fatalf("StartCreate: %v", err)
	}
	st := waitCreateDone(t, s, cl.ID)
	if st.Error != "" || st.OK != 2 || st.Items[0].ServerID == "" || st.Items[1].ServerID == "" {
		t.Fatalf("status: %+v log=%v", st, st.Log)
	}
	sv, _ := ft.Get(context.Background(), st.Items[0].ServerID)
	sv2, _ := ft.Get(context.Background(), st.Items[1].ServerID)
	if sv.CredentialID == "" || sv2.CredentialID != sv.CredentialID {
		t.Fatalf("内联凭据应代建一次并供整批共用: %q / %q", sv.CredentialID, sv2.CredentialID)
	}
	// DHCP 模式:agent 探测 IP(wireCreateFake 回 10.1.0.50)。
	in2 := CreateInput{Mode: CreateModeClone, Name: "dhcp", TemplateVMID: 9000, VMID: 200,
		Register: &RegisterInput{SSHUser: "root", WaitSeconds: 5, CredentialID: seedSSHCredential(t, s)}}
	if err := s.StartCreate(context.Background(), cl.ID, in2); err != nil {
		t.Fatalf("StartCreate2: %v", err)
	}
	st2 := waitCreateDone(t, s, cl.ID)
	if st2.Items[0].IP != "10.1.0.50" || st2.Items[0].ServerID == "" {
		t.Fatalf("dhcp register: %+v log=%v", st2.Items[0], st2.Log)
	}
}

// ---- 节点解析 / SSH 写文件 ---------------------------------------------------------------

// TestNodeServerIDResolution 名字 → 地址 → 集群 IP → 节点网卡地址 多级匹配;
// 未登记 → ErrInvalidInput 且错误体列出池内候选。
func TestNodeServerIDResolution(t *testing.T) {
	s, ft, _, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireCreateFake(fc, "9.0.3")
	// 屏蔽真实 DNS(未命中场景的节点名解析走注入点,保持测试封闭且快)。
	orig := dnsLookupHost
	dnsLookupHost = func(context.Context, string) ([]string, error) { return nil, errors.New("no dns") }
	defer func() { dnsLookupHost = orig }()
	fc.getFn = func(path string) (any, error) {
		if strings.Contains(path, "/cluster/status") {
			return []map[string]any{{"type": "node", "name": "pve2", "ip": "10.1.0.12"}}, nil
		}
		return nil, fmt.Errorf("unexpected %s", path)
	}
	ctx := context.Background()
	if id, err := s.nodeServerID(ctx, cl, "pve1"); err != nil || id != "node1" {
		t.Fatalf("by name: %q / %v", id, err)
	}
	// 按集群 IP 匹配(pve2 登记地址是 IP,名字不同)。
	ft.servers["node2"] = &target.Server{ID: "node2", Name: "other", Host: "10.1.0.12", PoolID: ft.servers["node1"].PoolID, Kind: target.KindPVENode}
	if id, err := s.nodeServerID(ctx, cl, "pve2"); err != nil || id != "node2" {
		t.Fatalf("by ip: %q / %v", id, err)
	}
	if _, err := s.nodeServerID(ctx, cl, "pve9"); !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "池内 pve 机器") {
		t.Fatalf("missing node err = %v", err)
	}
}

// TestNodeServerIDStandaloneViaNetwork 单机形态用户踩坑场景:/cluster/status 节点条目
// 无 ip 字段,机器按 IP 登记、名字与主机名不同 → 经 /nodes/{node}/network 接口地址匹配。
func TestNodeServerIDStandaloneViaNetwork(t *testing.T) {
	s, ft, _, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	poolID := ft.servers["node1"].PoolID
	ft.servers["node3"] = &target.Server{ID: "node3", Name: "pve01", Host: "10.1.0.30", PoolID: poolID, Kind: target.KindPVENode}
	// 屏蔽真实 DNS(命中发生在网卡地址匹配,DNS 注入仅用于保持封闭且快)。
	orig := dnsLookupHost
	dnsLookupHost = func(context.Context, string) ([]string, error) { return nil, errors.New("no dns") }
	defer func() { dnsLookupHost = orig }()
	fc.getFn = func(path string) (any, error) {
		if strings.Contains(path, "/cluster/status") {
			// 单机形态:只有 node 条目,无 ip 字段。
			return []map[string]any{{"type": "node", "name": "xufan-9600kf", "online": 1}}, nil
		}
		if strings.HasSuffix(path, "/network") {
			return []map[string]any{{"iface": "vmbr0", "type": "bridge", "address": "10.1.0.30", "active": 1}}, nil
		}
		return nil, fmt.Errorf("unexpected %s", path)
	}
	ctx := context.Background()
	if id, err := s.nodeServerID(ctx, cl, "xufan-9600kf"); err != nil || id != "node3" {
		t.Fatalf("by network api: %q / %v", id, err)
	}
}

// TestNodeServerIDHostnameResolutionIntersect 登记地址是 DDNS/内网域名(非 IP):
// 节点网卡地址经 /nodes/{node}/network 取到,登记域名解析成 IP 后与地址集求交命中。
func TestNodeServerIDHostnameResolutionIntersect(t *testing.T) {
	s, ft, _, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	poolID := ft.servers["node1"].PoolID
	ft.servers["node4"] = &target.Server{ID: "node4", Name: "9600kf", Host: "9600kf.mylinkpi.cn", PoolID: poolID, Kind: target.KindPVENode}
	orig := dnsLookupHost
	dnsLookupHost = func(_ context.Context, host string) ([]string, error) {
		if host == "9600kf.mylinkpi.cn" {
			return []string{"10.1.0.40"}, nil
		}
		return nil, errors.New("no dns")
	}
	defer func() { dnsLookupHost = orig }()
	fc.getFn = func(path string) (any, error) {
		if strings.Contains(path, "/cluster/status") {
			return []map[string]any{{"type": "node", "name": "xufan-9600kf", "online": 1}}, nil
		}
		if strings.HasSuffix(path, "/network") {
			return []map[string]any{{"iface": "vmbr0", "type": "bridge", "address": "10.1.0.40", "active": 1}}, nil
		}
		return nil, fmt.Errorf("unexpected %s", path)
	}
	ctx := context.Background()
	if id, err := s.nodeServerID(ctx, cl, "xufan-9600kf"); err != nil || id != "node4" {
		t.Fatalf("by hostname intersect: %q / %v", id, err)
	}
}

// TestNodeServerIDSSHHostnameFallback 网络 API 不可用 + 无 DNS → 并发 SSH
// `hostname -s` 兜底:机器真实主机名与节点名一致即命中(身份最终证据)。
// 移除 node1 使池内仅剩唯一候选,断言确定性。
func TestNodeServerIDSSHHostnameFallback(t *testing.T) {
	s, ft, _, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	poolID := ft.servers["node1"].PoolID
	delete(ft.servers, "node1")
	ft.hostname = "xufan-9600kf"
	ft.servers["node5"] = &target.Server{ID: "node5", Name: "9600kf", Host: "10.1.0.50", PoolID: poolID, Kind: target.KindPVENode}
	orig := dnsLookupHost
	dnsLookupHost = func(context.Context, string) ([]string, error) { return nil, errors.New("no dns") }
	defer func() { dnsLookupHost = orig }()
	fc.getFn = func(path string) (any, error) {
		if strings.Contains(path, "/cluster/status") {
			return []map[string]any{{"type": "node", "name": "xufan-9600kf", "online": 1}}, nil
		}
		return nil, fmt.Errorf("unexpected %s", path) // /network 不可用
	}
	ctx := context.Background()
	if id, err := s.nodeServerID(ctx, cl, "xufan-9600kf"); err != nil || id != "node5" {
		t.Fatalf("by ssh hostname: %q / %v", id, err)
	}
}

// TestSSHRunnerWriteFile root 直跑 / 非 root 提权;路径位置参数 + stdin 内容 + chmod。
func TestSSHRunnerWriteFile(t *testing.T) {
	ft := newFakeTarget()
	ft.servers["n1"] = &target.Server{ID: "n1", Name: "pve1", Host: "10.1.0.11", User: "root"}
	ft.servers["n2"] = &target.Server{ID: "n2", Name: "pve2", Host: "10.1.0.12", User: "ops"}
	r := &sshRunner{servers: ft}
	ctx := context.Background()
	if err := r.WriteFile(ctx, "n1", "/var/lib/vz/snippets/x.yaml", 0o600, strings.NewReader("body")); err != nil {
		t.Fatalf("root write: %v", err)
	}
	if err := r.WriteFile(ctx, "n2", "/var/lib/vz/snippets/y.yaml", 0o640, strings.NewReader("body2")); err != nil {
		t.Fatalf("sudo write: %v", err)
	}
	if len(ft.stdinCmds) != 2 {
		t.Fatalf("stdinCmds = %d", len(ft.stdinCmds))
	}
	root := ft.stdinCmds[0]
	if !strings.HasPrefix(root.path, "sh -c ") || !strings.Contains(root.path, "/var/lib/vz/snippets/x.yaml") || !strings.Contains(root.path, "0600") || root.content != "body" {
		t.Fatalf("root cmd: %+v", root)
	}
	sudo := ft.stdinCmds[1]
	if !strings.HasPrefix(sudo.path, "sudo -n sh -c ") || sudo.content != "body2" {
		t.Fatalf("sudo cmd: %+v", sudo)
	}
}

// ---- waitTask ---------------------------------------------------------------------------

// TestWaitTaskErrorAndTimeout 任务 ERROR → 错误;一直 running → 超时。
func TestWaitTaskErrorAndTimeout(t *testing.T) {
	fc := &fakePVEClient{}
	fc.getFn = func(string) (any, error) {
		return map[string]any{"status": "stopped", "exitstatus": "ERROR: disk full"}, nil
	}
	err := waitTask(context.Background(), fc, "pve1", "UPID:pve1:1:1:task:100:", time.Second)
	if !errors.Is(err, ErrConnectFailed) || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v", err)
	}
	calls := 0
	fc.getFn = func(string) (any, error) {
		calls++
		return nil, &pveAPIError{status: 403, err: fmt.Errorf("%w: GET status → HTTP 403: forbidden", ErrConnectFailed)}
	}
	err = waitTask(context.Background(), fc, "pve1", "UPID:pve1:1:1:task:100:", time.Minute)
	if !errors.Is(err, ErrConnectFailed) || calls != 1 || !strings.Contains(err.Error(), "403") {
		t.Fatalf("永久错误应立即返回: calls=%d err=%v", calls, err)
	}
	for _, tc := range []struct {
		status int
		want   bool
	}{{http.StatusRequestTimeout, true}, {http.StatusTooManyRequests, true}, {http.StatusInternalServerError, true}, {http.StatusNotImplemented, false}, {http.StatusNotFound, false}} {
		if got := retryableTaskHTTPStatus(tc.status); got != tc.want {
			t.Fatalf("retryableTaskHTTPStatus(%d) = %v, want %v", tc.status, got, tc.want)
		}
	}
	fc.getFn = func(string) (any, error) {
		return map[string]any{"status": "running"}, nil
	}
	err = waitTask(context.Background(), fc, "pve1", "UPID:pve1:1:1:task:100:", 50*time.Millisecond)
	if !errors.Is(err, ErrConnectFailed) || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("timeout err = %v", err)
	}
}
