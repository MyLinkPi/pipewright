// create.go — 建机后台任务(三管线编排 + 批量 + 逐台容错)。
//
// 任务模型照 ceph.deployJob:每集群一个内存任务(mu + createJobs),StartCreate 校验
// + readonly 闸门 + busy 检查后 go runCreate(context.Background,不随请求取消);
// CreateStatus 返回步骤/日志/逐台结果快照,前端轮询。建机是多步编排(克隆/导入/下载
// 单步可达分钟级),绝不做同步长 HTTP 请求。
//
// 纪律:密码/密钥/Token 绝不进任务日志/审计/错误体;UPID 等待各步独立超时;
// 批量逐台独立容错(一台失败不阻断后续,参照 aliyunecs.Import)。
package pveadmin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 建机模式。
const (
	CreateModeClone = "clone" // 模板克隆 / .vma 备份恢复
	CreateModeImage = "image" // 云镜像 + cloud-init
	CreateModeLXC   = "lxc"   // LXC 容器
)

// 建机步骤名(CreateStatus.Step 取值;前端按序展示)。
const (
	CreateStepPrepare  = "prepare"
	CreateStepSource   = "source"
	CreateStepSnippet  = "snippet"
	CreateStepCreate   = "create"
	CreateStepConfig   = "config"
	CreateStepImport   = "import"
	CreateStepResize   = "resize"
	CreateStepStart    = "start"
	CreateStepRegister = "register"
)

// 各步超时与轮询间隔(整条管线无总闸,各步独立限时)。
const (
	createCloneTO     = 15 * time.Minute
	createImportTO    = 15 * time.Minute
	createDownloadTO  = 30 * time.Minute
	createStartTO     = 3 * time.Minute
	createConfigTO    = 2 * time.Minute
	createIPTO        = 3 * time.Minute
	createTaskPoll    = 3 * time.Second
	maxCreateLogLines = 600
	// MaxCreateBatch 是单次建机台数上限。
	MaxCreateBatch = 50
)

// CreateInput 是建机入参(JSON camelCase;域内白名单校验)。
// 默认 true 的开关用 *bool(区分缺省与显式 false)。
type CreateInput struct {
	Mode  string `json:"mode"` // clone | image | lxc
	Node  string `json:"node"` // clone 模式可空(取模板所在节点)
	Name  string `json:"name"`
	Count int    `json:"count"` // 1..50;0 → 1
	VMID  int64  `json:"vmid"`  // 起始 ID;0 → 自动 nextid

	// clone 模式源(二选一):模板 VMID 或 .vma 备份卷/路径。
	TemplateVMID int64  `json:"templateVmid"`
	BackupVolid  string `json:"backupVolid"`
	Storage      string `json:"storage"` // 目标存储(克隆/恢复/镜像磁盘)

	// image 模式源(二选一):镜像 URL(任务内下载)或已有卷。
	ImageURL   string `json:"imageUrl"`
	ImageVolid string `json:"imageVolid"`

	// lxc 模式源(二选一):已有 vztmpl 卷或任务内 aplinfo 下载。
	OSTemplate  string            `json:"ostemplate"`
	AplDownload *AplDownloadInput `json:"aplDownload,omitempty"`

	// 资源。
	Cores       int    `json:"cores"`  // 0 → 2
	Memory      int    `json:"memory"` // MB;0 → 2048
	DiskGB      int    `json:"diskGb"` // 系统盘;0 → 模板原大小/镜像默认 10
	DataGB      int    `json:"dataGb"` // lxc 数据盘(/data)
	DataStorage string `json:"dataStorage"`

	// 网络。
	Bridge  string `json:"bridge"` // 空 → vmbr0
	IPMode  string `json:"ipMode"` // dhcp | static;空 → dhcp
	IP      string `json:"ip"`     // 静态基址(批量自动递增)
	Gateway string `json:"gateway"`
	Prefix  int    `json:"prefix"` // 0 → 24
	DNS     string `json:"dns"`    // 逗号分隔

	// cloud-init / 凭据(image 模式与带 cloudinit 盘的克隆)。
	CIUser          string `json:"ciUser"`
	CIPassword      string `json:"ciPassword"`
	SSHKeys         string `json:"sshKeys"` // 换行分隔公钥
	SSHPasswordAuth bool   `json:"sshPasswordAuth"`
	Timezone        string `json:"timezone"`
	Mirror          string `json:"mirror"`       // none/ustc/tsinghua/aliyun
	InstallAgent    *bool  `json:"installAgent"` // 默认 true
	DisableSELinux  bool   `json:"disableSelinux"`
	DisableFirewall bool   `json:"disableFirewall"`
	FixStatic       bool   `json:"fixStatic"`

	// 通用开关。
	Onboot  bool   `json:"onboot"`
	Start   *bool  `json:"start"`   // 默认 true
	Agent   *bool  `json:"agent"`   // 默认 true
	BIOS    string `json:"bios"`    // seabios | ovmf;空 → image=ovmf/clone 保持
	Machine string `json:"machine"` // 空 → image=q35/clone 保持

	// lxc 专属。
	Unprivileged *bool  `json:"unprivileged"` // 默认 true
	Nesting      *bool  `json:"nesting"`      // 默认 true
	LXCPassword  string `json:"lxcPassword"`

	// 建机后自动登记服务器(nil = 不登记)。
	Register *RegisterInput `json:"register,omitempty"`
}

// AplDownloadInput 是任务内 LXC 模板在线下载参数。
type AplDownloadInput struct {
	Storage  string `json:"storage"`
	Template string `json:"template"` // aplinfo 模板标识
}

// CreateItemResult 是批量建机里一台的结果(创建与登记分开计错)。
type CreateItemResult struct {
	Index       int    `json:"index"`
	VMID        int64  `json:"vmid"`
	Name        string `json:"name"`
	IP          string `json:"ip,omitempty"`
	OK          bool   `json:"ok"`
	Err         string `json:"err,omitempty"`
	ServerID    string `json:"serverId,omitempty"`
	RegisterErr string `json:"registerErr,omitempty"`
	Note        string `json:"note,omitempty"`
}

// CreateStatus 是建机任务快照(前端轮询至 done)。
type CreateStatus struct {
	Running bool               `json:"running"`
	Mode    string             `json:"mode"`
	Step    string             `json:"step"`
	Message string             `json:"message"`
	Done    bool               `json:"done"`
	Error   string             `json:"error,omitempty"`
	Log     []string           `json:"log"`
	Items   []CreateItemResult `json:"items"`
	Total   int                `json:"total"`
	OK      int                `json:"ok"`
	Failed  int                `json:"failed"`
}

// createJob 是一次建机任务的可变状态(快照与日志追加并发安全;ceph.deployJob 同形)。
type createJob struct {
	mu      sync.Mutex
	id      string
	running bool
	mode    string
	step    string
	message string
	done    bool
	errMsg  string
	log     []string
	items   []CreateItemResult
}

func (j *createJob) set(step, msg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.step, j.message = step, msg
}

func (j *createJob) logf(format string, args ...any) {
	line := time.Now().Format("15:04:05 ") + fmt.Sprintf(format, args...)
	j.mu.Lock()
	defer j.mu.Unlock()
	j.log = append(j.log, line)
	if len(j.log) > maxCreateLogLines {
		j.log = j.log[len(j.log)-maxCreateLogLines:]
	}
}

func (j *createJob) finish(errMsg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.running = false
	j.done = true
	j.errMsg = errMsg
}

func (j *createJob) setItem(i int, item CreateItemResult) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for i >= len(j.items) {
		j.items = append(j.items, CreateItemResult{})
	}
	j.items[i] = item
}

func (j *createJob) snapshot() CreateStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	st := CreateStatus{Running: j.running, Mode: j.mode, Step: j.step, Message: j.message, Done: j.done, Error: j.errMsg}
	st.Log = append([]string(nil), j.log...)
	st.Items = append([]CreateItemResult(nil), j.items...)
	st.Total = len(j.items)
	for _, it := range j.items {
		if it.OK {
			st.OK++
		} else if it.Err != "" {
			st.Failed++
		}
	}
	return st
}

// CreateStatus 返回建机任务快照;集群不存在 → ErrNotFound,从未启动 → 空快照。
func (s *service) CreateStatus(ctx context.Context, id string) (CreateStatus, error) {
	if _, err := s.clusterByID(ctx, id); err != nil {
		return CreateStatus{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if j := s.createJobs[id]; j != nil {
		return j.snapshot(), nil
	}
	return CreateStatus{}, nil
}

// StartCreate 校验入参 + readonly 闸门 + busy 检查后异步启动建机管线。
func (s *service) StartCreate(ctx context.Context, id string, in CreateInput) error {
	in, err := validateCreateInput(in)
	if err != nil {
		return err
	}
	cl, c, err := s.clientFor(ctx, id)
	if err != nil {
		return err
	}
	if cl.Readonly {
		return fmt.Errorf("%w: 集群 %s 处于只读模式,请在设置页解锁后再建机", ErrReadonly, cl.Name)
	}
	if in.VMID == 0 {
		var next flexInt
		if err := c.Get(ctx, "/api2/json/cluster/nextid", &next); err != nil {
			return err
		}
		if next < 100 {
			return fmt.Errorf("%w: /cluster/nextid 返回 %d 越界", ErrConnectFailed, int64(next))
		}
		in.VMID = int64(next)
	}
	if in.VMID+int64(in.Count)-1 > 999999999 {
		return fmt.Errorf("%w: 批量 vmid 区间越界", ErrInvalidInput)
	}
	jobID, err := newCreateJobID()
	if err != nil {
		return fmt.Errorf("pveadmin: 创建任务标识: %w", err)
	}

	s.mu.Lock()
	// DeleteCluster 持同一互斥锁完成删除;此处复查避免已删集群被重新放入后台任务。
	cl, err = s.clusterByID(ctx, id)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if cl.Readonly {
		s.mu.Unlock()
		return fmt.Errorf("%w: 集群 %s 处于只读模式,请在设置页解锁后再建机", ErrReadonly, cl.Name)
	}
	if s.createJobs == nil {
		s.createJobs = map[string]*createJob{}
	}
	if s.anyJobRunningLocked(id) {
		s.mu.Unlock()
		return fmt.Errorf("%w: 集群 %s 已有建机/升级任务在运行", ErrBusy, cl.Name)
	}
	j := &createJob{id: jobID, running: true, mode: in.Mode, step: CreateStepPrepare, message: "准备建机…"}
	s.createJobs[id] = j
	s.mu.Unlock()

	go s.runCreate(j, cl.ID, in)
	return nil
}

func newCreateJobID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// runCreate 是管线主体(后台 goroutine;bg 不随请求取消)。
func (s *service) runCreate(j *createJob, clusterID string, in CreateInput) {
	ctx := context.Background()
	if err := s.createPipeline(ctx, j, clusterID, in); err != nil {
		j.logf("建机失败: %v", err)
		j.finish(err.Error())
		return
	}
	j.finish("")
}

// createPipeline 准备客户端/版本/源,逐台执行(独立容错),可选登记。
func (s *service) createPipeline(ctx context.Context, j *createJob, clusterID string, in CreateInput) error {
	cl, c, err := s.clientFor(ctx, clusterID)
	if err != nil {
		return err
	}
	major := apiMajor(ctx, c)

	// clone 模式:解析模板所在节点(模板列表里找 vmid)。
	if in.Mode == CreateModeClone && in.TemplateVMID > 0 {
		var res []apiResource
		if err := c.Get(ctx, "/api2/json/cluster/resources", &res); err != nil {
			return err
		}
		found := false
		for _, r := range res {
			if r.Type == "qemu" && int64(r.VMID) == in.TemplateVMID && bool(r.Template) {
				in.Node = r.Node
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: 模板 %d 不存在或不是模板", ErrInvalidInput, in.TemplateVMID)
		}
		j.logf("模板 %d 位于节点 %s", in.TemplateVMID, in.Node)
	}
	if !nameRe.MatchString(in.Node) {
		return fmt.Errorf("%w: 节点名不合规", ErrInvalidInput)
	}

	// 同一 URL 的批量建机只下载一次,复用返回卷,避免后续实例因目标文件已存在而失败。
	if in.Mode == CreateModeImage && in.ImageURL != "" {
		j.set(CreateStepSource, "下载云镜像…")
		volid, err := s.downloadImage(ctx, c, major, in.Node, in.Storage, in.ImageURL)
		if err != nil {
			return err
		}
		in.ImageURL = ""
		in.ImageVolid = volid
		j.logf("批量镜像就绪: %s", volid)
	}

	// 内联凭据按任务代建一次并供整批共用;若最终没有任何服务器引用则清理。
	inlineCredentialID := ""
	inlineCredentialUsed := false
	if in.Register != nil && in.Register.InlineSecret != nil {
		inlineCredentialID, err = s.createInlineCredential(in.Register.InlineSecret, in.Name)
		if err != nil {
			return err
		}
		in.Register.CredentialID = inlineCredentialID
		in.Register.InlineSecret = nil
		defer func() {
			if !inlineCredentialUsed {
				if err := s.vault.Delete(inlineCredentialID); err != nil {
					j.logf("清理未使用的内联凭据失败: %v", err)
				}
			}
		}()
	}

	for i := 0; i < in.Count; i++ {
		vmid := in.VMID + int64(i)
		name := batchName(in.Name, i, in.Count)
		ip := ""
		if in.IPMode == "static" {
			ip = incrementIP(in.IP, i)
		}
		item := CreateItemResult{Index: i, VMID: vmid, Name: name, IP: ip}
		j.set(CreateStepPrepare, fmt.Sprintf("建机 %d/%d: %s (vmid=%d)", i+1, in.Count, name, vmid))
		j.setItem(i, item)

		cerr := s.createOne(ctx, j, cl, c, major, in, vmid, name, ip, &item)
		if cerr != nil {
			item.Err = cerr.Error()
			j.logf("[%s] 失败: %v", name, cerr)
		} else {
			item.OK = true
			j.logf("[%s] 创建成功 (vmid=%d)", name, vmid)
			if in.Register != nil {
				j.set(CreateStepRegister, fmt.Sprintf("登记服务器 %s…", name))
				sid, rerr := s.registerCreatedGuest(ctx, j, cl, c, in, vmid, name, item.IP, &item)
				if rerr != nil {
					item.RegisterErr = rerr.Error()
					j.logf("[%s] 登记失败(虚机已创建): %v", name, rerr)
				} else {
					item.ServerID = sid
					inlineCredentialUsed = inlineCredentialUsed || inlineCredentialID != ""
					j.logf("[%s] 已登记为服务器 %s", name, sid)
				}
			}
		}
		j.setItem(i, item)
	}
	st := j.snapshot()
	j.set(CreateStepPrepare, fmt.Sprintf("完成: 成功 %d / 失败 %d", st.OK, st.Failed))
	return nil
}

// batchName 批量命名:单台原名,多台 name-{i+1}。
func batchName(name string, i, count int) string {
	if count <= 1 {
		return name
	}
	return fmt.Sprintf("%s-%d", name, i+1)
}

// incrementIP 静态基址按台序递增(netip.Addr.Next;非法基址原样返回,校验已前置)。
func incrementIP(base string, i int) string {
	addr, err := netip.ParseAddr(base)
	if err != nil {
		return base
	}
	for n := 0; n < i; n++ {
		addr = addr.Next()
	}
	return addr.String()
}

// retryableTaskHTTPStatus 仅让瞬时状态进入下一轮;501 表示端点永久未实现。
func retryableTaskHTTPStatus(status int) bool {
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
		return true
	}
	if status >= 400 && status < 500 {
		return false
	}
	return status != http.StatusNotImplemented
}

// waitTask 轮询 UPID 至完成(各步独立超时;瞬时 GET 错误容忍重试)。
func waitTask(ctx context.Context, c pveClient, node, upid string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	path := "/api2/json/nodes/" + node + "/tasks/" + url.PathEscape(upid) + "/status"
	var lastErr error
	timeoutErr := func() error {
		if lastErr != nil {
			return fmt.Errorf("%w: 等待任务 %s 超时(%s);最后错误: %v", ErrConnectFailed, upid, timeout, lastErr)
		}
		return fmt.Errorf("%w: 等待任务 %s 超时(%s)", ErrConnectFailed, upid, timeout)
	}
	for {
		var ts apiTaskStatus
		err := c.Get(ctx, path, &ts)
		if err == nil {
			lastErr = nil
			if ts.Status != "running" {
				if strings.EqualFold(strings.TrimSpace(ts.ExitStatus), "OK") {
					return nil
				}
				return fmt.Errorf("%w: 任务 %s 结束于 %s", ErrConnectFailed, upid, ts.ExitStatus)
			}
		} else {
			lastErr = err
			var apiErr *pveAPIError
			if errors.As(err, &apiErr) && !retryableTaskHTTPStatus(apiErr.status) {
				return err
			}
			if !errors.Is(err, ErrConnectFailed) {
				return err
			}
			if ctx.Err() != nil {
				return timeoutErr()
			}
		}
		select {
		case <-ctx.Done():
			return timeoutErr()
		case <-time.After(createTaskPoll):
		}
	}
}

// postUPID 发 POST 并收 UPID(异步端点统一形态)。
func postUPID(ctx context.Context, c pveClient, path string, form url.Values) (string, error) {
	var upid string
	if err := c.Post(ctx, path, form, &upid); err != nil {
		return "", err
	}
	if !strings.HasPrefix(upid, "UPID:") {
		return "", fmt.Errorf("%w: %s 响应缺 UPID", ErrConnectFailed, path)
	}
	return upid, nil
}

// putUPID 发异步 PUT 并收 UPID(如 config import-from、resize)。
func putUPID(ctx context.Context, c pveClient, path string, form url.Values) (string, error) {
	var upid string
	if err := c.Put(ctx, path, form, &upid); err != nil {
		return "", err
	}
	if !strings.HasPrefix(upid, "UPID:") {
		return "", fmt.Errorf("%w: %s 响应缺 UPID", ErrConnectFailed, path)
	}
	return upid, nil
}

// dnsList 拆逗号/空格分隔的 DNS 列表(过滤空项)。
func dnsList(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// sshKeyList 拆换行分隔的公钥列表(过滤空行)。
func sshKeyList(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if k := strings.TrimSpace(line); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// boolOr 取 *bool 缺省值。
func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// tcpReachable 等待 IP:port 可连(登记前探活;aliyunecs.power 同形)。
func tcpReachable(ctx context.Context, ip string, port int, wait time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	addr := net.JoinHostPort(ip, fmt.Sprintf("%d", port))
	for {
		d := net.Dialer{Timeout: 3 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(3 * time.Second):
		}
	}
}
