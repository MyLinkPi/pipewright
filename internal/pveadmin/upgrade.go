// upgrade.go — 日常 apt 升级(纯 API)+ 更新可见性 + 节点重启。
//
// 任务模型照 createJob:每集群一个内存任务(mu + upgradeJobs),StartUpgrade 校验 +
// readonly 闸门 + busy 检查(与建机/大版本升级互斥)后 go runUpgrade(context.Background,
// 不随请求取消);UpgradeStatus 返回步骤/日志/逐节点结果快照,前端 2s 轮询。
//
// 语义:严格串行逐节点(corosync/pve-cluster 包更新会重启服务,并行有丢 quorum 风险),
// 但逐节点独立容错(一台失败不阻断后续,区别大版本升级的 fail-fast)。每节点:
// 刷新索引(POST apt/update → waitTask)→ 全量升级(POST apt/upgrade → waitTask)→
// 内核需重启检测(GET apt/versions 的 Running vs Version)。重启是单独的显式动作
// (RebootNode,输入确认),绝不自动重启。
//
// 纪律:固定路径/argv,绝不拼 shell;错误体不含 Token;节点名白名单校验。
package pveadmin

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// 日常升级步骤名(UpgradeStatus.Step 取值)。
const (
	UpgradeStepPrepare = "prepare"
	UpgradeStepRefresh = "refresh"
	UpgradeStepUpgrade = "upgrade"
	UpgradeStepCheck   = "check"
)

// 各步超时与扇出参数(整条管线无总闸,各步独立限时)。
const (
	updatesConcurrency = 4
	updatesNodeTO      = 45 * time.Second
	upgradeRefreshTO   = 10 * time.Minute
	upgradeTO          = 60 * time.Minute
	maxUpgradeLogLines = 600
	maxUpdatesPackages = 300
)

// AptPackage 是一个待更新包(更新矩阵展开列表用)。
type AptPackage struct {
	Package    string `json:"package"`
	Title      string `json:"title,omitempty"`
	Version    string `json:"version,omitempty"`
	OldVersion string `json:"oldVersion,omitempty"`
	Origin     string `json:"origin,omitempty"`
}

// NodeUpdates 是更新矩阵里一个节点的聚合视图(逐节点错误隔离)。
type NodeUpdates struct {
	Node          string       `json:"node"`
	Online        bool         `json:"online"`
	Version       string       `json:"version,omitempty"` // PVE release,如 8.4.1
	KernelRunning string       `json:"kernelRunning,omitempty"`
	Pending       int          `json:"pending"`
	Packages      []AptPackage `json:"packages,omitempty"`
	Subscription  string       `json:"subscription,omitempty"` // active/inactive/...
	NeedsReboot   bool         `json:"needsReboot"`
	Error         string       `json:"error,omitempty"`
}

// UpdatesOverview 是集群更新矩阵。
type UpdatesOverview struct {
	Quorate bool          `json:"quorate"`
	Items   []NodeUpdates `json:"items"`
}

// RefreshItem 是索引刷新受理结果(UPID 经既有 task 端点轮询)。
type RefreshItem struct {
	Node  string `json:"node"`
	UPID  string `json:"upid,omitempty"`
	Error string `json:"error,omitempty"`
}

// UpgradeInput 是日常升级入参(nodes 空 = 全部在线节点,按名排序)。
type UpgradeInput struct {
	Nodes []string `json:"nodes"`
}

// UpgradeNodeResult 是日常升级里一个节点的结果。
type UpgradeNodeResult struct {
	Node        string `json:"node"`
	OK          bool   `json:"ok"`
	Err         string `json:"err,omitempty"`
	Updated     int    `json:"updated"` // 升级前待更新包数
	NeedsReboot bool   `json:"needsReboot"`
	Note        string `json:"note,omitempty"`
}

// UpgradeStatus 是日常升级任务快照(前端轮询至 done;形状照 CreateStatus)。
type UpgradeStatus struct {
	Running bool                `json:"running"`
	Step    string              `json:"step"`
	Message string              `json:"message"`
	Done    bool                `json:"done"`
	Error   string              `json:"error,omitempty"`
	Log     []string            `json:"log"`
	Items   []UpgradeNodeResult `json:"items"`
	Total   int                 `json:"total"`
	OK      int                 `json:"ok"`
	Failed  int                 `json:"failed"`
}

// upgradeJob 是一次日常升级任务的可变状态(createJob 同形)。
type upgradeJob struct {
	mu      sync.Mutex
	id      string
	running bool
	step    string
	message string
	done    bool
	errMsg  string
	log     []string
	items   []UpgradeNodeResult
}

func (j *upgradeJob) set(step, msg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.step, j.message = step, msg
}

func (j *upgradeJob) logf(format string, args ...any) {
	line := time.Now().Format("15:04:05 ") + fmt.Sprintf(format, args...)
	j.mu.Lock()
	defer j.mu.Unlock()
	j.log = append(j.log, line)
	if len(j.log) > maxUpgradeLogLines {
		j.log = j.log[len(j.log)-maxUpgradeLogLines:]
	}
}

func (j *upgradeJob) finish(errMsg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.running = false
	j.done = true
	j.errMsg = errMsg
}

func (j *upgradeJob) setItem(i int, item UpgradeNodeResult) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for i >= len(j.items) {
		j.items = append(j.items, UpgradeNodeResult{})
	}
	j.items[i] = item
}

func (j *upgradeJob) snapshot() UpgradeStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	st := UpgradeStatus{Running: j.running, Step: j.step, Message: j.message, Done: j.done, Error: j.errMsg}
	st.Log = append([]string(nil), j.log...)
	st.Items = append([]UpgradeNodeResult(nil), j.items...)
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

// ---- 集群节点枚举与入参解析 -------------------------------------------------------------

// clusterNodeList 是 /cluster/status 的节点视图(单机形态视作有仲裁,口径同 Overview)。
type clusterNodeList struct {
	Quorate bool
	Nodes   []NodeStatus // 按名排序
}

func fetchClusterNodes(ctx context.Context, c pveClient) (*clusterNodeList, error) {
	var entries []apiClusterStatusEntry
	if err := c.Get(ctx, "/api2/json/cluster/status", &entries); err != nil {
		return nil, err
	}
	out := &clusterNodeList{}
	sawCluster := false
	for _, e := range entries {
		switch e.Type {
		case "cluster":
			sawCluster = true
			out.Quorate = bool(e.Quorate)
		case "node":
			out.Nodes = append(out.Nodes, NodeStatus{Name: e.Name, IP: e.IP, Online: bool(e.Online)})
		}
	}
	if !sawCluster {
		out.Quorate = true
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].Name < out.Nodes[j].Name })
	return out, nil
}

// resolveUpgradeNodes 解析目标节点:requested 空 = 全部(onlineOnly 时仅在线),按名排序;
// 显式列表逐个白名单 + 集群成员校验(去重,保持给定顺序)。
func resolveUpgradeNodes(requested []string, cn *clusterNodeList, onlineOnly bool) ([]string, error) {
	member := make(map[string]bool, len(cn.Nodes))
	for _, n := range cn.Nodes {
		member[n.Name] = n.Online
	}
	if len(requested) == 0 {
		var out []string
		for _, n := range cn.Nodes {
			if !onlineOnly || n.Online {
				out = append(out, n.Name)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("%w: 集群无可用节点", ErrInvalidInput)
		}
		return out, nil
	}
	seen := make(map[string]bool, len(requested))
	out := make([]string, 0, len(requested))
	for _, n := range requested {
		if !nameRe.MatchString(n) {
			return nil, fmt.Errorf("%w: 节点名 %q 不合规", ErrInvalidInput, n)
		}
		online, ok := member[n]
		if !ok {
			return nil, fmt.Errorf("%w: 节点 %s 不在集群内", ErrInvalidInput, n)
		}
		if onlineOnly && !online {
			return nil, fmt.Errorf("%w: 节点 %s 离线", ErrInvalidInput, n)
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out, nil
}

// fanOutNodes 以 limit 并发闸对 n 个下标执行 fn(等待全部完成;逐下标错误隔离由 fn 自理)。
func fanOutNodes(n, limit int, fn func(i int)) {
	if n <= 0 {
		return
	}
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(idx)
		}(i)
	}
	wg.Wait()
}

// ---- 更新矩阵与索引刷新 -----------------------------------------------------------------

// ListUpdates 逐节点聚合更新视图(只读;并发扇出,单节点失败不拖垮整体)。
func (s *service) ListUpdates(ctx context.Context, id string) (*UpdatesOverview, error) {
	_, c, err := s.clientFor(ctx, id)
	if err != nil {
		return nil, err
	}
	cn, err := fetchClusterNodes(ctx, c)
	if err != nil {
		return nil, err
	}
	out := &UpdatesOverview{Quorate: cn.Quorate, Items: make([]NodeUpdates, len(cn.Nodes))}
	fanOutNodes(len(cn.Nodes), updatesConcurrency, func(i int) {
		n := cn.Nodes[i]
		item := NodeUpdates{Node: n.Name, Online: n.Online}
		defer func() { out.Items[i] = item }()
		if !n.Online {
			item.Error = "节点离线"
			return
		}
		nctx, cancel := context.WithTimeout(ctx, updatesNodeTO)
		defer cancel()
		base := "/api2/json/nodes/" + n.Name
		var errs []string
		note := func(err error) {
			if err != nil {
				errs = append(errs, unwrapTail(err))
			}
		}
		var ver apiVersion
		if err := c.Get(nctx, base+"/version", &ver); err != nil {
			note(err)
		} else {
			item.Version = ver.Release
		}
		var ns apiNodeStatus
		if err := c.Get(nctx, base+"/status", &ns); err != nil {
			note(err)
		} else {
			item.KernelRunning = ns.Kversion
		}
		var pending []apiAptUpdate
		if err := c.Get(nctx, base+"/apt/update", &pending); err != nil {
			note(err)
		} else {
			item.Pending = len(pending)
			for _, p := range pending {
				if len(item.Packages) >= maxUpdatesPackages {
					break
				}
				item.Packages = append(item.Packages, AptPackage{
					Package: p.Package, Title: p.Title, Version: p.Version, OldVersion: p.OldVersion, Origin: p.Origin,
				})
			}
		}
		var versions []apiAptVersion
		if err := c.Get(nctx, base+"/apt/versions", &versions); err != nil {
			note(err)
		} else {
			item.NeedsReboot = kernelNeedsReboot(versions)
		}
		var sub apiSubscription
		if err := c.Get(nctx, base+"/subscription", &sub); err != nil {
			// 无订阅时 PVE 可能返回错误而非空状态:按未订阅处理,不计节点错误。
			item.Subscription = "notfound"
		} else {
			item.Subscription = sub.Status
		}
		if len(errs) > 0 {
			item.Error = strings.Join(errs, "; ")
		}
	})
	return out, nil
}

// kernelNeedsReboot 启发式:关键包条目里 Running(运行版本)与 Version(已装版本)
// 不一致 → 需重启。仅内核类条目带 Running,其余条目跳过。
func kernelNeedsReboot(versions []apiAptVersion) bool {
	for _, v := range versions {
		if v.Running == "" || v.Version == "" {
			continue
		}
		if !sameKernelVersion(v.Running, v.Version) {
			return true
		}
	}
	return false
}

// sameKernelVersion 宽容比较内核版本串(容忍 "Linux " 前缀/括号注释/-pve 后缀):
// 相等或边界安全的前缀包含视为同版本(6.8.12-9-pve ↔ 6.8.12-9)。
func sameKernelVersion(a, b string) bool {
	a, b = normalizeKernelVersion(a), normalizeKernelVersion(b)
	if a == b {
		return true
	}
	longer, shorter := a, b
	if len(b) > len(a) {
		longer, shorter = b, a
	}
	if strings.HasPrefix(longer, shorter) {
		rest := longer[len(shorter):]
		return rest == "" || rest[0] == '-' || rest[0] == '.'
	}
	return false
}

func normalizeKernelVersion(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "linux ")
	if i := strings.IndexAny(s, " ("); i > 0 {
		s = s[:i]
	}
	return s
}

// RefreshUpdates 刷新目标节点 apt 索引(写操作):逐节点 POST apt/update 收 UPID,
// 前端经既有 GET /task 轮询。单节点失败不阻断其余。
func (s *service) RefreshUpdates(ctx context.Context, id string, nodes []string) ([]RefreshItem, error) {
	cl, c, err := s.clientFor(ctx, id)
	if err != nil {
		return nil, err
	}
	if cl.Readonly {
		return nil, fmt.Errorf("%w: 集群 %s 处于只读模式,请在设置页解锁后再刷新索引", ErrReadonly, cl.Name)
	}
	cn, err := fetchClusterNodes(ctx, c)
	if err != nil {
		return nil, err
	}
	targets, err := resolveUpgradeNodes(nodes, cn, true)
	if err != nil {
		return nil, err
	}
	items := make([]RefreshItem, len(targets))
	for i, n := range targets {
		items[i] = RefreshItem{Node: n}
		upid, perr := postUPID(ctx, c, "/api2/json/nodes/"+n+"/apt/update", url.Values{})
		if perr != nil {
			items[i].Error = unwrapTail(perr)
			continue
		}
		items[i].UPID = upid
	}
	return items, nil
}

// ---- 日常升级任务 -----------------------------------------------------------------------

// UpgradeStatus 返回日常升级快照;集群不存在 → ErrNotFound,从未启动 → 空快照。
func (s *service) UpgradeStatus(ctx context.Context, id string) (UpgradeStatus, error) {
	if _, err := s.clusterByID(ctx, id); err != nil {
		return UpgradeStatus{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if j := s.upgradeJobs[id]; j != nil {
		return j.snapshot(), nil
	}
	return UpgradeStatus{}, nil
}

// StartUpgrade 校验 + readonly 闸门 + busy 检查后异步启动日常升级(严格串行)。
func (s *service) StartUpgrade(ctx context.Context, id string, in UpgradeInput) error {
	for _, n := range in.Nodes {
		if !nameRe.MatchString(n) {
			return fmt.Errorf("%w: 节点名 %q 不合规", ErrInvalidInput, n)
		}
	}
	cl, c, err := s.clientFor(ctx, id)
	if err != nil {
		return err
	}
	if cl.Readonly {
		return fmt.Errorf("%w: 集群 %s 处于只读模式,请在设置页解锁后再升级", ErrReadonly, cl.Name)
	}
	cn, err := fetchClusterNodes(ctx, c)
	if err != nil {
		return err
	}
	nodes, err := resolveUpgradeNodes(in.Nodes, cn, true)
	if err != nil {
		return err
	}
	jobID, err := newCreateJobID()
	if err != nil {
		return fmt.Errorf("pveadmin: 创建任务标识: %w", err)
	}

	s.mu.Lock()
	// DeleteCluster 持同一互斥锁;复查避免已删集群被重新放入后台任务。
	cl, err = s.clusterByID(ctx, id)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if cl.Readonly {
		s.mu.Unlock()
		return fmt.Errorf("%w: 集群 %s 处于只读模式,请在设置页解锁后再升级", ErrReadonly, cl.Name)
	}
	if s.upgradeJobs == nil {
		s.upgradeJobs = map[string]*upgradeJob{}
	}
	if s.anyJobRunningLocked(id) {
		s.mu.Unlock()
		return fmt.Errorf("%w: 集群 %s 已有建机/升级任务在运行", ErrBusy, cl.Name)
	}
	j := &upgradeJob{id: jobID, running: true, step: UpgradeStepPrepare, message: "准备升级…"}
	s.upgradeJobs[id] = j
	s.mu.Unlock()

	go s.runUpgrade(j, cl.ID, nodes)
	return nil
}

// runUpgrade 是日常升级主体(后台 goroutine;逐节点独立容错)。
func (s *service) runUpgrade(j *upgradeJob, clusterID string, nodes []string) {
	ctx := context.Background()
	_, c, err := s.clientFor(ctx, clusterID)
	if err != nil {
		j.logf("升级失败: %v", err)
		j.finish(err.Error())
		return
	}
	for i, node := range nodes {
		item := UpgradeNodeResult{Node: node}
		j.set(UpgradeStepRefresh, fmt.Sprintf("节点 %d/%d: %s", i+1, len(nodes), node))
		j.setItem(i, item)
		nerr := s.upgradeOneNode(ctx, j, c, node, &item)
		if nerr != nil {
			item.Err = nerr.Error()
			j.logf("[%s] 失败: %v", node, nerr)
		} else {
			item.OK = true
			j.logf("[%s] 升级完成(更新 %d 包,需重启=%v)", node, item.Updated, item.NeedsReboot)
		}
		j.setItem(i, item)
	}
	st := j.snapshot()
	j.set(UpgradeStepPrepare, fmt.Sprintf("完成: 成功 %d / 失败 %d", st.OK, st.Failed))
	j.finish("")
}

// upgradeOneNode 单节点:刷新索引 → (有待更新则)全量升级 → 需重启检测。
func (s *service) upgradeOneNode(ctx context.Context, j *upgradeJob, c pveClient, node string, item *UpgradeNodeResult) error {
	base := "/api2/json/nodes/" + node
	var pending []apiAptUpdate
	if err := c.Get(ctx, base+"/apt/update", &pending); err == nil {
		item.Updated = len(pending)
	}
	j.logf("[%s] 刷新 apt 索引…", node)
	upid, err := postUPID(ctx, c, base+"/apt/update", url.Values{})
	if err != nil {
		return err
	}
	if err := waitTask(ctx, c, node, upid, upgradeRefreshTO); err != nil {
		return err
	}
	// 刷新后重取待更新数:取到且为 0 → 跳过升级(已是最新);取失败则照常升级
	// (绝不因一次瞬时 API 错误误判「已是最新」而漏升级)。
	pending = nil
	pErr := c.Get(ctx, base+"/apt/update", &pending)
	if pErr == nil {
		item.Updated = len(pending)
	}
	if pErr == nil && len(pending) == 0 {
		item.Note = "已是最新,无需升级"
	} else {
		j.set(UpgradeStepUpgrade, fmt.Sprintf("%s: 升级 %d 个包(可能数分钟)…", node, len(pending)))
		j.logf("[%s] 开始全量升级(%d 包)…", node, len(pending))
		upid, err = postUPID(ctx, c, base+"/apt/upgrade", url.Values{})
		if err != nil {
			return err
		}
		if err := waitTask(ctx, c, node, upid, upgradeTO); err != nil {
			return err
		}
	}
	j.set(UpgradeStepCheck, fmt.Sprintf("%s: 检查内核…", node))
	var versions []apiAptVersion
	if err := c.Get(ctx, base+"/apt/versions", &versions); err == nil {
		item.NeedsReboot = kernelNeedsReboot(versions)
		if item.NeedsReboot {
			item.Note = "内核已更新,需重启节点生效"
		}
	}
	return nil
}

// ---- 节点重启 ---------------------------------------------------------------------------

// RebootNode 重启节点(显式危险动作):confirm 须原样等于节点名;readonly 闸门;
// 任一后台任务运行中拒绝(升级途中重启会破坏编排)。返回 UPID 句柄。
func (s *service) RebootNode(ctx context.Context, id, node, confirm string) (*TaskRef, error) {
	if !nameRe.MatchString(node) {
		return nil, fmt.Errorf("%w: 节点名不合规", ErrInvalidInput)
	}
	if confirm != node {
		return nil, fmt.Errorf("%w: 须原样输入节点名 %q 以确认重启", ErrConfirmMismatch, node)
	}
	cl, c, err := s.clientFor(ctx, id)
	if err != nil {
		return nil, err
	}
	if cl.Readonly {
		return nil, fmt.Errorf("%w: 集群 %s 处于只读模式,请在设置页解锁后再重启节点", ErrReadonly, cl.Name)
	}
	s.mu.Lock()
	busy := s.anyJobRunningLocked(id)
	s.mu.Unlock()
	if busy {
		return nil, fmt.Errorf("%w: 集群 %s 有建机/升级任务运行中,禁止重启节点", ErrBusy, cl.Name)
	}
	cn, err := fetchClusterNodes(ctx, c)
	if err != nil {
		return nil, err
	}
	online := false
	for _, n := range cn.Nodes {
		if n.Name == node {
			online = n.Online
			break
		}
	}
	if !online {
		return nil, fmt.Errorf("%w: 节点 %s 不在集群内或已离线", ErrInvalidInput, node)
	}
	upid, err := postUPID(ctx, c, "/api2/json/nodes/"+node+"/status", url.Values{"command": {"reboot"}})
	if err != nil {
		return nil, err
	}
	return &TaskRef{Node: node, UPID: upid}, nil
}
