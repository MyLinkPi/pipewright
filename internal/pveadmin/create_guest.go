// create_guest.go — 三条建机管线(模板克隆/.vma 恢复、云镜像 cloud-init、LXC)
// 与 qemu/lxc 配置助手。全部经 PVE REST API;两处 SSH 兜底:cloud-init snippet 落盘
// (API 无上传端点)与老版本 qm importdisk(import-from 参数被拒时)。
//
// 端点形态(官方 apidoc 验证,allowtoken=1):
//   - POST /nodes/{n}/qemu/{src}/clone {newid,full,name,storage} → UPID
//   - POST /nodes/{n}/qemu {vmid,archive,...} → UPID(.vma 恢复)
//   - POST /nodes/{n}/qemu {vmid,...cloud-init 参数} → UPID(云镜像建机)
//   - PUT  /nodes/{n}/qemu/{id}/config {scsi0:"STORAGE:0,import-from=<volid>"} → UPID
//   - PUT  /nodes/{n}/qemu/{id}/resize {disk,size} / PUT .../cloudinit
//   - POST /nodes/{n}/lxc {vmid,ostemplate,...} → UPID
//   - 9.x: POST .../storage/{s}/download-url;8.x: POST .../storage/{s}/content+url
package pveadmin

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// createOne 按模式分派单台建机(批量循环里逐台调用,独立容错)。
func (s *service) createOne(ctx context.Context, j *createJob, cl *Cluster, c pveClient, major int, in CreateInput, vmid int64, name, ip string, item *CreateItemResult) error {
	switch {
	case in.Mode == CreateModeClone && in.TemplateVMID > 0:
		return s.cloneGuest(ctx, j, cl, c, in, vmid, name, ip, item)
	case in.Mode == CreateModeClone:
		return s.restoreGuest(ctx, j, cl, c, in, vmid, name, ip, item)
	case in.Mode == CreateModeImage:
		return s.imageGuest(ctx, j, cl, c, major, in, vmid, name, ip, item)
	default:
		return s.lxcGuest(ctx, j, c, major, in, vmid, name, ip, item)
	}
}

// ---- 管线 A:模板克隆 / .vma 恢复 --------------------------------------------------------

// cloneGuest 完整克隆模板 → 改配/扩容/cloud-init(若有盘)→ 启动。
func (s *service) cloneGuest(ctx context.Context, j *createJob, cl *Cluster, c pveClient, in CreateInput, vmid int64, name, ip string, item *CreateItemResult) error {
	j.set(CreateStepCreate, "克隆模板…")
	form := url.Values{"newid": {strconv.FormatInt(vmid, 10)}, "full": {"1"}, "name": {name}}
	if in.Storage != "" {
		form.Set("storage", in.Storage)
	}
	upid, err := postUPID(ctx, c, fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/clone", in.Node, in.TemplateVMID), form)
	if err != nil {
		return err
	}
	if err := waitTask(ctx, c, in.Node, upid, createCloneTO); err != nil {
		return err
	}
	return s.finishQemu(ctx, j, cl, c, in, vmid, name, ip, item)
}

// restoreGuest 从 .vma 备份恢复(archive 接受存储卷 ID 或绝对路径)→ 改配 → 启动。
func (s *service) restoreGuest(ctx context.Context, j *createJob, cl *Cluster, c pveClient, in CreateInput, vmid int64, name, ip string, item *CreateItemResult) error {
	j.set(CreateStepCreate, "恢复备份…")
	form := url.Values{"vmid": {strconv.FormatInt(vmid, 10)}, "archive": {in.BackupVolid}}
	if in.Storage != "" {
		form.Set("storage", in.Storage)
	}
	upid, err := postUPID(ctx, c, "/api2/json/nodes/"+in.Node+"/qemu", form)
	if err != nil {
		return err
	}
	if err := waitTask(ctx, c, in.Node, upid, createCloneTO); err != nil {
		return err
	}
	return s.finishQemu(ctx, j, cl, c, in, vmid, name, ip, item)
}

// finishQemu 克隆/恢复后的公共收尾:改配 → 扩系统盘 → cloud-init(若模板有盘)→ 启动。
func (s *service) finishQemu(ctx context.Context, j *createJob, cl *Cluster, c pveClient, in CreateInput, vmid int64, name, ip string, item *CreateItemResult) error {
	j.set(CreateStepConfig, "调整配置…")
	cfg, err := qemuConfig(ctx, c, in.Node, vmid)
	if err != nil {
		return err
	}
	form := qemuConfigForm(in, name)
	if in.BIOS == "ovmf" {
		if _, ok := cfg["efidisk0"]; !ok {
			st := in.Storage
			if st == "" {
				st = "local"
			}
			form.Set("efidisk0", st+":0,efitype=4m,pre-enrolled-keys=0")
		}
	}
	// POST /config 是同步变体(data=null);只有 PUT import-from/resize 返回 UPID。
	cctx, cancel := context.WithTimeout(ctx, createConfigTO)
	err = c.Post(cctx, fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/config", in.Node, vmid), form, nil)
	cancel()
	if err != nil {
		return err
	}

	if in.DiskGB > 0 {
		if disk := bootDisk(cfg); disk != "" {
			j.set(CreateStepResize, "扩展系统盘…")
			rerr := resizeQemuDisk(ctx, c, in.Node, vmid, disk, in.DiskGB)
			if rerr != nil {
				// 缩盘被拒/盘已更大:非致命,记提示(cloud-init 扩根脚本幂等空跑)。
				item.Note = "系统盘未扩容(目标不大于现有或 PVE 拒绝)"
				j.logf("[%s] 扩容跳过: %v", name, rerr)
			}
		}
	}

	if hasCloudInitDrive(cfg) {
		cicustom, _, serr := s.writeSnippet(ctx, j, cl, c, in, vmid, name)
		if serr != nil {
			return serr
		}
		cform := url.Values{"cicustom": {cicustom}, "ipconfig0": {ipconfig0Value(in, ip)}}
		cctx2, cancel2 := context.WithTimeout(ctx, createConfigTO)
		err = c.Post(cctx2, fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/config", in.Node, vmid), cform, nil)
		cancel2()
		if err != nil {
			return err
		}
		s.regenCloudInit(ctx, j, c, in.Node, vmid, name)
	} else if in.IPMode == "static" {
		item.Note = "模板无 cloud-init 盘,静态 IP 未注入"
		j.logf("[%s] %s", name, item.Note)
	}
	return s.startGuest(ctx, j, c, in, "qemu", vmid, name)
}

// ---- 管线 B:云镜像 + cloud-init ---------------------------------------------------------

// imageGuest 镜像准备(URL 下载/已有卷)→ snippet → 建 VM → 导入磁盘 → 启动。
func (s *service) imageGuest(ctx context.Context, j *createJob, cl *Cluster, c pveClient, major int, in CreateInput, vmid int64, name, ip string, item *CreateItemResult) error {
	imageVolid := in.ImageVolid
	if in.ImageURL != "" {
		j.set(CreateStepSource, "下载云镜像…")
		volid, err := s.downloadImage(ctx, c, major, in.Node, in.Storage, in.ImageURL)
		if err != nil {
			return err
		}
		imageVolid = volid
		j.logf("[%s] 镜像就绪: %s", name, imageVolid)
	}

	cicustom, cleanupSnippet, err := s.writeSnippet(ctx, j, cl, c, in, vmid, name)
	if err != nil {
		return err
	}

	j.set(CreateStepCreate, "创建虚机…")
	st := in.Storage
	bios := in.BIOS
	if bios == "" {
		bios = "ovmf"
	}
	machine := in.Machine
	if machine == "" {
		machine = "q35"
	}
	form := url.Values{
		"vmid":      {strconv.FormatInt(vmid, 10)},
		"name":      {name},
		"cores":     {strconv.Itoa(in.Cores)},
		"memory":    {strconv.Itoa(in.Memory)},
		"ostype":    {"l26"},
		"scsihw":    {"virtio-scsi-pci"},
		"net0":      {"virtio,bridge=" + in.Bridge},
		"bios":      {bios},
		"machine":   {machine},
		"ide2":      {st + ":cloudinit"},
		"ipconfig0": {ipconfig0Value(in, ip)},
		"cicustom":  {cicustom},
		"boot":      {"order=scsi0"},
	}
	if bios == "ovmf" {
		form.Set("efidisk0", st+":0,efitype=4m,pre-enrolled-keys=0")
	}
	agent := "enabled=0"
	if boolOr(in.Agent, true) {
		agent = "enabled=1"
	}
	form.Set("agent", agent)
	if in.Onboot {
		form.Set("onboot", "1")
	} else {
		form.Set("onboot", "0")
	}
	upid, err := postUPID(ctx, c, "/api2/json/nodes/"+in.Node+"/qemu", form)
	if err != nil {
		var apiErr *pveAPIError
		if errors.As(err, &apiErr) && apiErr.status >= 400 && apiErr.status < 500 {
			cleanupSnippet()
		}
		return err
	}
	if err := waitTask(ctx, c, in.Node, upid, createConfigTO); err != nil {
		return err
	}

	j.set(CreateStepImport, "导入系统盘…")
	importForm := url.Values{"scsi0": {st + ":0,import-from=" + imageVolid}}
	ictx, icancel := context.WithTimeout(ctx, createConfigTO)
	iupid, ierr := putUPID(ictx, c, fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/config", in.Node, vmid), importForm)
	icancel()
	if ierr != nil {
		if !isImportFromUnsupported(ierr) {
			return ierr
		}
		// 仅在 PVE 明确拒绝 import-from 参数时兜底;网络结果不确定时绝不重复导入。
		j.logf("[%s] import-from 不可用,回退 SSH qm importdisk: %v", name, ierr)
		volid, ferr := s.importDiskSSH(ctx, cl, in.Node, vmid, imageVolid, st)
		if ferr != nil {
			return ferr
		}
		cctx, cancel := context.WithTimeout(ctx, createConfigTO)
		perr := c.Post(cctx, fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/config", in.Node, vmid), url.Values{"scsi0": {volid}}, nil)
		cancel()
		if perr != nil {
			return perr
		}
	} else if err := waitTask(ctx, c, in.Node, iupid, createImportTO); err != nil {
		return err
	}

	if in.DiskGB > 0 {
		j.set(CreateStepResize, "扩展系统盘…")
		if rerr := resizeQemuDisk(ctx, c, in.Node, vmid, "scsi0", in.DiskGB); rerr != nil {
			item.Note = "系统盘未扩容(目标不大于现有或 PVE 拒绝)"
			j.logf("[%s] 扩容跳过: %v", name, rerr)
		}
	}
	s.regenCloudInit(ctx, j, c, in.Node, vmid, name)
	return s.startGuest(ctx, j, c, in, "qemu", vmid, name)
}

// isImportFromUnsupported 只识别明确的“不支持 import-from 参数”响应。
func isImportFromUnsupported(err error) bool {
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "import-from") {
		return false
	}
	return strings.Contains(msg, "unknown parameter") ||
		strings.Contains(msg, "unknown option") ||
		strings.Contains(msg, "not implemented")
}

// resizeQemuDisk 提交异步扩容并等待完成,避免后续再生/启动与 VM 锁竞争。
func resizeQemuDisk(ctx context.Context, c pveClient, node string, vmid int64, disk string, diskGB int) error {
	rctx, cancel := context.WithTimeout(ctx, createConfigTO)
	upid, err := putUPID(rctx, c, fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/resize", node, vmid),
		url.Values{"disk": {disk}, "size": {strconv.Itoa(diskGB) + "G"}})
	cancel()
	if err != nil {
		return err
	}
	return waitTask(ctx, c, node, upid, createConfigTO)
}

// downloadImage 经 PVE 原生 URL 下载端点把云镜像落到存储(9.x download-url /
// 8.x content+url;content=import 被拒则回退 images),返回目标卷 ID。
func (s *service) downloadImage(ctx context.Context, c pveClient, major int, node, storage, rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("%w: 镜像 URL 非法", ErrInvalidInput)
	}
	filename := path.Base(u.Path)
	if filename == "" || filename == "." || filename == "/" {
		return "", fmt.Errorf("%w: 镜像 URL 缺文件名", ErrInvalidInput)
	}
	compression := ""
	for ext, alg := range map[string]string{".gz": "gz", ".xz": "xz", ".zst": "zst", ".bz2": "bz2"} {
		if strings.HasSuffix(filename, ext) {
			compression = alg
			filename = strings.TrimSuffix(filename, ext)
			break
		}
	}
	dlPath := "/api2/json/nodes/" + node + "/storage/" + storage + "/content"
	if major >= 9 {
		dlPath = "/api2/json/nodes/" + node + "/storage/" + storage + "/download-url"
	}
	form := url.Values{"content": {"import"}, "filename": {filename}, "url": {rawURL}}
	if compression != "" {
		form.Set("compression", compression)
	}
	upid, err := postUPID(ctx, c, dlPath, form)
	if err != nil && major < 9 {
		// 8.2 以前无 import 内容类型 → images 回退。
		form.Set("content", "images")
		upid, err = postUPID(ctx, c, dlPath, form)
		if err == nil {
			if werr := waitTask(ctx, c, node, upid, createDownloadTO); werr != nil {
				return "", werr
			}
			return storage + ":images/" + filename, nil
		}
	}
	if err != nil {
		return "", err
	}
	if err := waitTask(ctx, c, node, upid, createDownloadTO); err != nil {
		return "", err
	}
	return storage + ":import/" + filename, nil
}

// importDiskSSHRe 从 qm importdisk 输出提取导入卷 ID(unusedN:storage:vm-...-disk-N)。
var importDiskSSHRe = regexp.MustCompile(`unused\d+:([A-Za-z0-9_.:\-]+)`)

// importDiskSSH 老版本磁盘导入兜底:SSH 跑 qm importdisk,解析输出卷 ID。
func (s *service) importDiskSSH(ctx context.Context, cl *Cluster, node string, vmid int64, src, storage string) (string, error) {
	serverID, err := s.nodeServerID(ctx, cl, node)
	if err != nil {
		return "", err
	}
	ictx, cancel := context.WithTimeout(ctx, createImportTO)
	defer cancel()
	res, err := s.runner.Run(ictx, serverID, []string{"qm", "importdisk", strconv.FormatInt(vmid, 10), src, storage})
	if err != nil {
		return "", err
	}
	m := importDiskSSHRe.FindStringSubmatch(res.Stdout)
	if m == nil {
		return "", fmt.Errorf("%w: qm importdisk 输出未含卷 ID\n%s", ErrProbe, tailOutput(res))
	}
	return m[1], nil
}

// ---- 管线 C:LXC ------------------------------------------------------------------------

// lxcGuest (可选在线下载模板)→ 创建容器 → 启动。
func (s *service) lxcGuest(ctx context.Context, j *createJob, c pveClient, major int, in CreateInput, vmid int64, name, ip string, item *CreateItemResult) error {
	ostemplate := in.OSTemplate
	if in.AplDownload != nil {
		j.set(CreateStepSource, "下载 LXC 模板…")
		tpl, err := s.downloadApl(ctx, c, in.Node, in.AplDownload.Storage, in.AplDownload.Template)
		if err != nil {
			return err
		}
		ostemplate = tpl
		j.logf("[%s] 模板就绪: %s", name, ostemplate)
	}

	j.set(CreateStepCreate, "创建容器…")
	form := url.Values{
		"vmid":       {strconv.FormatInt(vmid, 10)},
		"ostemplate": {ostemplate},
		"rootfs":     {in.Storage + ":" + strconv.Itoa(in.DiskGB)},
		"hostname":   {name},
		"cores":      {strconv.Itoa(in.Cores)},
		"memory":     {strconv.Itoa(in.Memory)},
		"swap":       {"512"},
		"net0":       {lxcNet0(in, ip)},
	}
	if boolOr(in.Unprivileged, true) {
		form.Set("unprivileged", "1")
	}
	if boolOr(in.Nesting, true) {
		form.Set("features", "nesting=1,keyctl=1")
	}
	if in.Onboot {
		form.Set("onboot", "1")
	}
	if in.Timezone != "" {
		form.Set("timezone", in.Timezone)
	}
	if in.LXCPassword != "" {
		form.Set("password", in.LXCPassword)
	}
	if keys := sshKeyList(in.SSHKeys); len(keys) > 0 {
		form.Set("ssh-public-keys", strings.Join(keys, "\n"))
	}
	if dns := dnsList(in.DNS); len(dns) > 0 {
		form.Set("nameserver", strings.Join(dns, " "))
	}
	if in.DataGB > 0 {
		ds := in.DataStorage
		if ds == "" {
			ds = "local"
		}
		form.Set("mp0", ds+":"+strconv.Itoa(in.DataGB)+",mp=/data")
	}
	upid, err := postUPID(ctx, c, "/api2/json/nodes/"+in.Node+"/lxc", form)
	if err != nil {
		return err
	}
	if err := waitTask(ctx, c, in.Node, upid, createCloneTO); err != nil {
		return err
	}
	return s.startGuest(ctx, j, c, in, "lxc", vmid, name)
}

// downloadApl 在线下载 LXC 模板并返回卷 ID(按 aplinfo location 基名定位)。
func (s *service) downloadApl(ctx context.Context, c pveClient, node, storage, template string) (string, error) {
	var entries []apiAplEntry
	if err := c.Get(ctx, "/api2/json/nodes/"+node+"/aplinfo", &entries); err != nil {
		return "", err
	}
	location := ""
	for _, e := range entries {
		if e.Template == template {
			location = e.Location
			break
		}
	}
	if location == "" {
		return "", fmt.Errorf("%w: aplinfo 目录无模板 %q", ErrInvalidInput, template)
	}
	upid, err := postUPID(ctx, c, "/api2/json/nodes/"+node+"/aplinfo", url.Values{"storage": {storage}, "template": {template}})
	if err != nil {
		return "", err
	}
	if err := waitTask(ctx, c, node, upid, createDownloadTO); err != nil {
		return "", err
	}
	return storage + ":vztmpl/" + path.Base(location), nil
}

// ---- 公共助手 ---------------------------------------------------------------------------

// writeSnippet 生成 #cloud-config 并经 SSH 落到节点 snippets 存储,返回 cicustom 值与清理函数。
// YAML 内容绝不进日志(只记文件名);密码/密钥经 stdin 流式写入,权限 0600。
func (s *service) writeSnippet(ctx context.Context, j *createJob, cl *Cluster, c pveClient, in CreateInput, vmid int64, name string) (string, func(), error) {
	j.set(CreateStepSnippet, "注入 cloud-init 配置…")
	opts := CloudInitOptions{
		Hostname:        name,
		User:            in.CIUser,
		Password:        in.CIPassword,
		SSHKeys:         sshKeyList(in.SSHKeys),
		SSHPasswordAuth: in.SSHPasswordAuth,
		Timezone:        in.Timezone,
		DNS:             dnsList(in.DNS),
		Mirror:          in.Mirror,
		InstallAgent:    boolOr(in.InstallAgent, true),
		DisableSELinux:  in.DisableSELinux,
		DisableFirewall: in.DisableFirewall,
		RootExpand:      in.DiskGB > 0,
		FixStatic:       in.FixStatic && in.IPMode != "static",
	}
	yaml, err := renderCloudConfig(opts)
	if err != nil {
		return "", nil, err
	}
	sid, spath, err := snippetsStorage(ctx, c, in.Node)
	if err != nil {
		return "", nil, err
	}
	serverID, err := s.nodeServerID(ctx, cl, in.Node)
	if err != nil {
		return "", nil, err
	}
	fname := cloudInitSnippetName(vmid, j.id)
	remote := strings.TrimRight(spath, "/") + "/snippets/" + fname
	wctx, cancel := context.WithTimeout(ctx, createConfigTO)
	defer cancel()
	if err := s.runner.WriteFile(wctx, serverID, remote, 0o600, strings.NewReader(yaml)); err != nil {
		return "", nil, err
	}
	cleanup := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), probeTimeout)
		defer ccancel()
		if _, err := s.runner.Run(cctx, serverID, []string{"rm", "-f", "--", remote}); err != nil {
			j.logf("[%s] 清理未使用 cloud-init snippet 失败: %v", name, err)
		}
	}
	j.logf("[%s] cloud-init snippet 已写入 %s:snippets/%s", name, sid, fname)
	return "user=" + sid + ":snippets/" + fname, cleanup, nil
}

// regenCloudInit 再生 cloud-init 盘(PUT cloudinit;失败非致命——开机时 PVE 会再生)。
func (s *service) regenCloudInit(ctx context.Context, j *createJob, c pveClient, node string, vmid int64, name string) {
	rctx, cancel := context.WithTimeout(ctx, createConfigTO)
	defer cancel()
	if err := c.Put(rctx, fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/cloudinit", node, vmid), url.Values{}, nil); err != nil {
		j.logf("[%s] cloud-init 盘再生跳过(开机时自动再生): %v", name, err)
	}
}

// startGuest 启动虚机/容器并等待任务完成(Start=false 时跳过)。
func (s *service) startGuest(ctx context.Context, j *createJob, c pveClient, in CreateInput, kind string, vmid int64, name string) error {
	if !boolOr(in.Start, true) {
		j.logf("[%s] 未勾选启动,跳过", name)
		return nil
	}
	j.set(CreateStepStart, "启动…")
	upid, err := postUPID(ctx, c, fmt.Sprintf("/api2/json/nodes/%s/%s/%d/status/start", in.Node, kind, vmid), url.Values{})
	if err != nil {
		return err
	}
	return waitTask(ctx, c, in.Node, upid, createStartTO)
}

// qemuConfig 取虚机配置(扁平 map;值多为字符串)。
func qemuConfig(ctx context.Context, c pveClient, node string, vmid int64) (map[string]any, error) {
	var cfg map[string]any
	if err := c.Get(ctx, fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/config", node, vmid), &cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// qemuConfigForm 组克隆/恢复后的改配表单(cores/memory/name/net0/agent/onboot/bios/machine)。
func qemuConfigForm(in CreateInput, name string) url.Values {
	f := url.Values{
		"cores":  {strconv.Itoa(in.Cores)},
		"memory": {strconv.Itoa(in.Memory)},
		"name":   {name},
		"net0":   {"virtio,bridge=" + in.Bridge},
	}
	agent := "enabled=0"
	if boolOr(in.Agent, true) {
		agent = "enabled=1"
	}
	f.Set("agent", agent)
	if in.Onboot {
		f.Set("onboot", "1")
	} else {
		f.Set("onboot", "0")
	}
	if in.BIOS != "" {
		f.Set("bios", in.BIOS)
	}
	if in.Machine != "" {
		f.Set("machine", in.Machine)
	}
	return f
}

// ipconfig0Value 组 cloud-init 网络参数(静态 ip=IP/prefix,gw=GW / DHCP ip=dhcp)。
func ipconfig0Value(in CreateInput, ip string) string {
	if in.IPMode != "static" || ip == "" {
		return "ip=dhcp"
	}
	v := "ip=" + ip + "/" + strconv.Itoa(in.Prefix)
	if in.Gateway != "" {
		v += ",gw=" + in.Gateway
	}
	return v
}

// lxcNet0 组 LXC 网卡参数(name=eth0,bridge=X,ip=dhcp|IP/prefix,gw=GW)。
func lxcNet0(in CreateInput, ip string) string {
	v := "name=eth0,bridge=" + in.Bridge
	if in.IPMode == "static" && ip != "" {
		v += ",ip=" + ip + "/" + strconv.Itoa(in.Prefix)
		if in.Gateway != "" {
			v += ",gw=" + in.Gateway
		}
	} else {
		v += ",ip=dhcp"
	}
	return v
}

// bootDiskRe 匹配启动盘槽位(scsi0/virtio0/sata0/ide0...)。
var bootDiskRe = regexp.MustCompile(`^(scsi|virtio|sata|ide)\d+$`)

// bootDisk 从配置解析启动盘槽位(boot order 首个磁盘项;无 boot 键按常见槽位探测)。
func bootDisk(cfg map[string]any) string {
	if boot, _ := cfg["boot"].(string); boot != "" {
		boot = strings.TrimPrefix(boot, "order=")
		for _, part := range strings.Split(boot, ";") {
			p := strings.TrimSpace(part)
			if bootDiskRe.MatchString(p) {
				if _, ok := cfg[p]; ok {
					return p
				}
			}
		}
	}
	for _, slot := range []string{"scsi0", "virtio0", "sata0", "ide0"} {
		if _, ok := cfg[slot]; ok {
			return slot
		}
	}
	return ""
}

// hasCloudInitDrive 判断配置是否含 cloud-init 盘(ideN 值含 cloudinit)。
func hasCloudInitDrive(cfg map[string]any) bool {
	for _, slot := range []string{"ide0", "ide1", "ide2", "ide3"} {
		if v, _ := cfg[slot].(string); strings.Contains(v, "cloudinit") {
			return true
		}
	}
	return false
}
