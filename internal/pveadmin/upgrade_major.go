// upgrade_major.go — PVE 大版本升级(8→9)滚动编排(SSH + 持久化任务)。
//
// 官方流程(pve.proxmox.com/wiki/Upgrade_from_8_to_9)的忠实还原:先完成日常升级
// (8.4 最新)→ pve8to9 --full 预检 → 逐节点 [可选关停 guest] → 换源 bookworm→trixie
// → apt update → apt dist-upgrade(noninteractive + confdef/confold)→ pve8to9 复检
// → 重启 → 等节点回线且集群重新 quorate → 验证 9.x → 下一节点。严格串行、fail-fast
// (任一节点失败立即暂停,绝不带病推进);中止只在步骤边界生效,绝不中断 dist-upgrade
// (防 dpkg 半配置)。Ceph 集群直接阻断(须人工先升 Ceph 到 Squid)。
//
// 与 createJob/upgradeJob 的关键区别:任务持久化(0117 pve_upgrade_jobs)。升级横跨
// 1-2 小时与多次重启,控制端自身重启后内存 job 丢失 = 集群处于「升了一半」的未知状态;
// 状态懒加载时 DB running 无内存对应 → interrupted,只允许人工 resume/retry。
//
// 纪律:固定 argv 绝不拼 shell;密钥不进日志/审计/错误体;guest 关停超时即节点失败,
// 绝不强停;每步状态落库。
package pveadmin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/huangchengsir/pipewright/internal/target"
)

// 任务状态与节点状态机。
const (
	MajorStatusRunning     = "running"
	MajorStatusPaused      = "paused" // 失败/中止后暂停(可 retry/resume)
	MajorStatusDone        = "done"
	MajorStatusInterrupted = "interrupted" // 控制端重启导致中断

	MajorNodePending   = "pending"
	MajorNodeGuests    = "guests"
	MajorNodeSources   = "sources"
	MajorNodeUpgrading = "upgrading"
	MajorNodeRebooting = "rebooting"
	MajorNodeWaiting   = "waiting"
	MajorNodeVerifying = "verifying"
	MajorNodeDone      = "done"
	MajorNodeFailed    = "failed"
)

// guest 处理策略。
const (
	GuestPolicyWarn     = "warn"     // 仅警告:重启会硬停运行中 guest
	GuestPolicyShutdown = "shutdown" // 升级前逐台优雅关机(超时即节点失败,不强停)
)

// 各步超时(整条管线无总闸,各步独立限时)。
const (
	majorCmdTO                = 30 * time.Second
	majorPve8to9TO            = 5 * time.Minute
	majorAptUpdateTO          = 15 * time.Minute
	majorDistUpgradeTO        = 60 * time.Minute
	majorGuestShutdownTO      = 5 * time.Minute
	majorRebootDownTO         = 10 * time.Minute // 等节点进入重启(下线;含 guest 关停耗时)
	majorRebootWaitTO         = 15 * time.Minute // 等节点回线 + quorate
	majorRebootPoll           = 10 * time.Second
	majorPreflightFreshness   = 30 * time.Minute
	majorPreflightNodeTO      = 7 * time.Minute
	majorPreflightConcurrency = 4
	maxMajorLogLines          = 600
	majorMinDiskGB            = 5
)

// errMajorAbort 是内部哨兵:步骤边界检测到中止请求。
var errMajorAbort = errors.New("pveadmin: major upgrade aborted")

// ---- 对外类型 ---------------------------------------------------------------------------

// MajorCheck 是一条预检结果(level: pass | warn | fail;warn 须逐条 ack)。
type MajorCheck struct {
	Key     string `json:"key"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

// Pve8to9Result 是 pve8to9 --full 输出解析(计数 + WARN/FAIL 行)。
type Pve8to9Result struct {
	Passed   int      `json:"passed"`
	Skipped  int      `json:"skipped"`
	Warnings int      `json:"warnings"`
	Failures int      `json:"failures"`
	Lines    []string `json:"lines,omitempty"` // WARN/FAIL 行(截断)
	Summary  string   `json:"summary,omitempty"`
}

// MajorNodePreflight 是一个节点的预检结果。
type MajorNodePreflight struct {
	Node       string         `json:"node"`
	Online     bool           `json:"online"`
	Version    string         `json:"version,omitempty"`
	Codename   string         `json:"codename,omitempty"`
	DiskFreeGB float64        `json:"diskFreeGb"`
	Pending    int            `json:"pending"`
	Pve8to9    *Pve8to9Result `json:"pve8to9,omitempty"`
	Checks     []MajorCheck   `json:"checks"`
	Error      string         `json:"error,omitempty"`
}

// MajorPreflightReport 是集群预检报告(缓存 30min 供启动校验)。
type MajorPreflightReport struct {
	ClusterID     string               `json:"clusterId"`
	At            string               `json:"at"`
	Quorate       bool                 `json:"quorate"`
	CephDetected  bool                 `json:"cephDetected"`
	Nodes         []MajorNodePreflight `json:"nodes"`
	ClusterChecks []MajorCheck         `json:"clusterChecks"`
	Blocked       bool                 `json:"blocked"`
	AcksRequired  []string             `json:"acksRequired"`
}

// preflightEntry 是预检缓存条目(s.mu 保护)。
type preflightEntry struct {
	at     time.Time
	report *MajorPreflightReport
}

// MajorUpgradeInput 是大版本升级启动入参。
type MajorUpgradeInput struct {
	Confirm     string   `json:"confirm"`     // 须原样等于集群名
	Acks        []string `json:"acks"`        // 已确认的警告 key
	GuestPolicy string   `json:"guestPolicy"` // warn | shutdown;空 → warn
	Nodes       []string `json:"nodes"`       // 可选顺序;空 = 全部在线 8.x 节点按名排序
}

// MajorNodeStatus 是滚动升级里一个节点的状态(每步落库)。
type MajorNodeStatus struct {
	Node       string `json:"node"`
	Seq        int    `json:"seq"`
	State      string `json:"state"`
	Error      string `json:"error,omitempty"`
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
}

// MajorUpgradeStatus 是大版本升级快照(DB 持久化-backed;前端轮询)。
type MajorUpgradeStatus struct {
	JobID          string            `json:"jobId,omitempty"`
	Running        bool              `json:"running"`
	Status         string            `json:"status,omitempty"`
	GuestPolicy    string            `json:"guestPolicy,omitempty"`
	Step           string            `json:"step"`
	Message        string            `json:"message"`
	Done           bool              `json:"done"` // 非 running 即 done(含 paused/interrupted,前端停轮询)
	Error          string            `json:"error,omitempty"`
	Log            []string          `json:"log"`
	Nodes          []MajorNodeStatus `json:"nodes"`
	AbortRequested bool              `json:"abortRequested"`
	CreatedAt      string            `json:"createdAt,omitempty"`
}

// majorJob 是一次大版本升级任务的可变状态(内存日志 + DB 持久化状态)。
type majorJob struct {
	mu            sync.Mutex
	id            string
	clusterID     string
	running       bool
	status        string
	guestPolicy   string
	step          string
	message       string
	errMsg        string
	log           []string
	nodes         []MajorNodeStatus
	nodeOrder     []string
	abort         bool
	preflightJSON string
	createdAt     string
}

func (j *majorJob) set(step, msg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.step, j.message = step, msg
}

func (j *majorJob) logf(format string, args ...any) {
	line := time.Now().Format("15:04:05 ") + fmt.Sprintf(format, args...)
	j.mu.Lock()
	defer j.mu.Unlock()
	j.log = append(j.log, line)
	if len(j.log) > maxMajorLogLines {
		j.log = j.log[len(j.log)-maxMajorLogLines:]
	}
}

func (j *majorJob) abortRequested() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.abort
}

func (j *majorJob) snapshot() MajorUpgradeStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	st := MajorUpgradeStatus{
		JobID: j.id, Running: j.running, Status: j.status, GuestPolicy: j.guestPolicy,
		Step: j.step, Message: j.message, Done: !j.running, Error: j.errMsg,
		AbortRequested: j.abort, CreatedAt: j.createdAt,
	}
	st.Log = append([]string(nil), j.log...)
	st.Nodes = append([]MajorNodeStatus(nil), j.nodes...)
	return st
}

// ---- 预检 -------------------------------------------------------------------------------

// MajorPreflight 同步预检(并发扇出;pve8to9 --full 单节点 5min)。报告缓存供启动校验。
func (s *service) MajorPreflight(ctx context.Context, id string) (*MajorPreflightReport, error) {
	cl, c, err := s.clientFor(ctx, id)
	if err != nil {
		return nil, err
	}
	cn, err := fetchClusterNodes(ctx, c)
	if err != nil {
		return nil, err
	}
	rep := &MajorPreflightReport{ClusterID: id, At: time.Now().UTC().Format(time.RFC3339), Quorate: cn.Quorate}
	if !cn.Quorate {
		rep.ClusterChecks = append(rep.ClusterChecks, MajorCheck{Key: "quorate", Level: "fail", Message: "集群无仲裁(quorate),禁止升级"})
	} else {
		rep.ClusterChecks = append(rep.ClusterChecks, MajorCheck{Key: "quorate", Level: "pass", Message: "集群有仲裁"})
	}

	// Ceph 检测(pmxcfs 集群-wide,任一在线节点 SSH 可判)。
	ceph, cephErr := s.detectCeph(ctx, cl, cn.Nodes)
	rep.CephDetected = ceph
	switch {
	case ceph:
		rep.ClusterChecks = append(rep.ClusterChecks, MajorCheck{Key: "ceph", Level: "fail",
			Message: "检测到 Ceph 集群:须人工按官方流程先把 Ceph 升到 Squid 19.2 再升级 PVE,本平台不自动处理"})
	case cephErr != nil:
		rep.ClusterChecks = append(rep.ClusterChecks, MajorCheck{Key: "ceph", Level: "fail", Message: "无法检测 Ceph(SSH 不可达): " + unwrapTail(cephErr)})
	default:
		rep.ClusterChecks = append(rep.ClusterChecks, MajorCheck{Key: "ceph", Level: "pass", Message: "未检测到 Ceph"})
	}

	// 运行中任务(全集群一次取,按节点归集)。
	runningTasks := map[string][]string{}
	var tasks []apiTask
	if terr := c.Get(ctx, "/api2/json/cluster/tasks", &tasks); terr == nil {
		for _, t := range tasks {
			if t.Status == "" { // 运行中
				runningTasks[t.Node] = append(runningTasks[t.Node], t.Type)
			}
		}
	}
	// HA 在用检测(resources hastate)。
	haUsed := false
	var resources []apiResource
	if rerr := c.Get(ctx, "/api2/json/cluster/resources", &resources); rerr == nil {
		for _, r := range resources {
			if (r.Type == "qemu" || r.Type == "lxc") && r.HAState != "" && r.HAState != "ignored" {
				haUsed = true
			}
		}
	}

	rep.Nodes = make([]MajorNodePreflight, len(cn.Nodes))
	fanOutNodes(len(cn.Nodes), majorPreflightConcurrency, func(i int) {
		rep.Nodes[i] = s.majorNodePreflight(ctx, cl, c, cn.Nodes[i], runningTasks[cn.Nodes[i].Name])
	})

	onlineCount := 0
	for _, n := range cn.Nodes {
		if n.Online {
			onlineCount++
		}
	}
	if onlineCount == 2 {
		rep.ClusterChecks = append(rep.ClusterChecks, MajorCheck{Key: "two-node-quorum", Level: "warn",
			Message: "两节点集群:重启期间会丢失仲裁(除非已配 qdevice),建议先加 qdevice 或确认接受风险"})
	}
	rep.ClusterChecks = append(rep.ClusterChecks, MajorCheck{Key: "self-hosting", Level: "warn",
		Message: "请确认 Pipewright 控制端未运行在本集群的虚机/容器内,否则节点重启会中断升级任务"})
	if haUsed {
		rep.ClusterChecks = append(rep.ClusterChecks, MajorCheck{Key: "ha-managed", Level: "warn",
			Message: "集群有 HA 管理的 guest:重启节点会触发 HA 迁移/恢复,请确认 HA 状态健康"})
	}

	// 汇总阻断与待 ack 警告。
	for _, ck := range rep.ClusterChecks {
		if ck.Level == "fail" {
			rep.Blocked = true
		}
		if ck.Level == "warn" {
			rep.AcksRequired = append(rep.AcksRequired, ck.Key)
		}
	}
	for _, np := range rep.Nodes {
		for _, ck := range np.Checks {
			if ck.Level == "fail" {
				rep.Blocked = true
			}
			if ck.Level == "warn" {
				rep.AcksRequired = append(rep.AcksRequired, ck.Key)
			}
		}
	}
	sort.Strings(rep.AcksRequired)
	s.mu.Lock()
	if s.preflights == nil {
		s.preflights = map[string]preflightEntry{}
	}
	s.preflights[id] = preflightEntry{at: time.Now(), report: rep}
	s.mu.Unlock()
	return rep, nil
}

// majorNodePreflight 单节点预检(版本/待更新/代号/磁盘/仓库/pve8to9/运行任务)。
func (s *service) majorNodePreflight(ctx context.Context, cl *Cluster, c pveClient, n NodeStatus, running []string) MajorNodePreflight {
	np := MajorNodePreflight{Node: n.Name, Online: n.Online}
	add := func(key, level, msg string) {
		np.Checks = append(np.Checks, MajorCheck{Key: key, Level: level, Message: msg})
	}
	if !n.Online {
		add("online", "fail", "节点离线")
		return np
	}
	add("online", "pass", "节点在线")
	nctx, cancel := context.WithTimeout(ctx, majorPreflightNodeTO)
	defer cancel()
	base := "/api2/json/nodes/" + n.Name

	var ver apiVersion
	if err := c.Get(nctx, base+"/version", &ver); err != nil {
		add("version", "fail", "取版本失败: "+unwrapTail(err))
	} else {
		np.Version = ver.Release
		major, minor := parsePveRelease(ver.Release)
		switch {
		case major == 9:
			// 已升级节点(混合集群/续跑场景):跳过其余检查且不参与升级目标,
			// 避免其 trixie 代号/待更新包把预检误判为 FAIL。
			add("version", "pass", "已是 9.x(升级时将跳过)")
			return np
		case major == 8 && minor >= 4:
			add("version", "pass", "版本 "+ver.Release+"(8.4+)")
		default:
			add("version", "fail", "版本 "+ver.Release+" 不满足 8.4+ 前提,请先完成日常升级")
		}
	}
	var pending []apiAptUpdate
	if err := c.Get(nctx, base+"/apt/update", &pending); err != nil {
		add("pending", "fail", "取待更新列表失败: "+unwrapTail(err))
	} else {
		np.Pending = len(pending)
		if len(pending) == 0 {
			add("pending", "pass", "无待更新包")
		} else {
			add("pending", "fail", fmt.Sprintf("有 %d 个待更新包:请先完成日常 apt 升级", len(pending)))
		}
	}
	if len(running) > 0 {
		add("tasks", "fail", "节点有运行中任务: "+strings.Join(running, ", "))
	} else {
		add("tasks", "pass", "无运行中任务")
	}

	sid, serr := s.nodeServerID(ctx, cl, n.Name)
	if serr != nil {
		add("ssh", "fail", "节点 SSH 不可达/未登记: "+unwrapTail(serr))
		return np
	}
	add("ssh", "pass", "SSH 可达")

	// Debian 代号。
	if res, err := s.runMajorCmd(nctx, sid, majorCmdTO, []string{"cat", "/etc/os-release"}); err == nil {
		np.Codename = osReleaseCodename(res.Stdout)
	}
	if np.Codename == "bookworm" {
		add("codename", "pass", "Debian 代号 bookworm")
	} else {
		add("codename", "fail", "Debian 代号为 "+np.Codename+"(须 bookworm;非 8.x 系统?)")
	}
	// 根分区剩余空间。
	if res, err := s.runMajorCmd(nctx, sid, majorCmdTO, []string{"df", "-B1", "--output=avail", "/"}); err == nil {
		lines := strings.Fields(strings.TrimSpace(res.Stdout))
		if len(lines) > 0 {
			if b, perr := strconv.ParseInt(lines[len(lines)-1], 10, 64); perr == nil {
				np.DiskFreeGB = float64(b) / 1e9
			}
		}
	}
	if np.DiskFreeGB >= majorMinDiskGB {
		add("disk", "pass", fmt.Sprintf("根分区剩余 %.1f GB", np.DiskFreeGB))
	} else {
		add("disk", "fail", fmt.Sprintf("根分区剩余 %.1f GB,不足 5 GB", np.DiskFreeGB))
	}
	// 仓库状态(企业源无订阅 → 阻断;suite 混杂 → 警告)。
	subStatus := ""
	var sb apiSubscription
	if err := c.Get(nctx, base+"/subscription", &sb); err == nil {
		subStatus = sb.Status
	}
	if files, _, rerr := s.readAptRepoFiles(nctx, cl, n.Name); rerr == nil {
		enterpriseEnabled, pveEnabled, suiteMixed := false, false, false
		for _, f := range files {
			for _, e := range f.entries {
				if e.kind == "pve" && e.enabled {
					pveEnabled = true
					if e.enterprise {
						enterpriseEnabled = true
					}
				}
				if e.enabled && (e.kind == "debian" || e.kind == "pve") && np.Codename != "" {
					for _, st := range e.suites {
						suiteBase := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(st, "-security"), "-updates"), "-backports")
						if suiteBase != np.Codename {
							suiteMixed = true
						}
					}
				}
			}
		}
		if !pveEnabled {
			// 无 PVE 源 → dist-upgrade 只升 Debian 包,pveversion 仍 8.x,节点必失败:
			// 必须在预检拦住(仓库管理「应用镜像」可自动补建 PVE 非订阅源)。
			add("repo-pve:"+n.Name, "fail", "未启用任何 PVE 软件源:无法升级 PVE 包,请先在仓库管理应用镜像(会自动补建非订阅源)")
		}
		if enterpriseEnabled && !strings.EqualFold(subStatus, "active") {
			add("repo-sub", "fail", "企业源已启用但无有效订阅:请先在仓库管理切换到 no-subscription 或激活订阅")
		} else {
			add("repo-sub", "pass", "仓库订阅状态可用")
		}
		if suiteMixed {
			add("repo-suite:"+n.Name, "warn", "存在 suite 与运行代号不符的启用条目,换源前请核对")
		}
	}
	// pve8to9 --full(官方检查清单;FAIL 阻断,WARN 须 ack)。
	res, perr := s.runMajorCmd(nctx, sid, majorPve8to9TO, []string{"pve8to9", "--full"})
	if res == nil {
		add("pve8to9", "fail", "pve8to9 执行失败: "+unwrapTail(perr))
	} else {
		r := parsePve8to9(res.Stdout)
		np.Pve8to9 = &r
		if res.ExitCode == 127 {
			add("pve8to9", "fail", "pve8to9 不存在:请先升级到 8.4 最新 pve-manager")
		} else if r.Failures > 0 {
			add("pve8to9", "fail", fmt.Sprintf("pve8to9 有 %d 项 FAIL,须先修复", r.Failures))
		} else {
			add("pve8to9", "pass", fmt.Sprintf("pve8to9 通过(%d PASS / %d SKIP)", r.Passed, r.Skipped))
		}
		if r.Warnings > 0 {
			add("pve8to9-warn:"+n.Name, "warn", fmt.Sprintf("pve8to9 有 %d 项 WARN,请逐条核对(详见报告)", r.Warnings))
		}
	}
	return np
}

// pve8to9 输出解析(容错:PASS/FAIL/WARN/SKIP/INFO 前缀行 + TOTAL 汇总;
// 实现时以真机输出校准 fixture)。
var (
	pve8to9LineRe    = regexp.MustCompile(`(?i)^\s*\[?(PASS|FAIL|WARN|WARNING|SKIP|INFO)\]?[:\s]`)
	pve8to9SummaryRe = regexp.MustCompile(`(?i)TOTAL:\s*(\d+)\s*PASSED,\s*(\d+)\s*SKIPPED,\s*(\d+)\s*WARNINGS?,\s*(\d+)\s*FAILURES?`)
)

func parsePve8to9(out string) Pve8to9Result {
	var r Pve8to9Result
	for _, ln := range strings.Split(out, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		if m := pve8to9SummaryRe.FindStringSubmatch(t); m != nil {
			r.Summary = t
			r.Passed, _ = strconv.Atoi(m[1])
			r.Skipped, _ = strconv.Atoi(m[2])
			r.Warnings, _ = strconv.Atoi(m[3])
			r.Failures, _ = strconv.Atoi(m[4])
			continue
		}
		m := pve8to9LineRe.FindStringSubmatch(t)
		if m == nil {
			continue
		}
		switch strings.ToUpper(m[1]) {
		case "PASS":
			r.Passed++
		case "SKIP":
			r.Skipped++
		case "WARN", "WARNING":
			r.Warnings++
			if len(r.Lines) < 100 {
				r.Lines = append(r.Lines, truncateLine(t, 300))
			}
		case "FAIL":
			r.Failures++
			if len(r.Lines) < 100 {
				r.Lines = append(r.Lines, truncateLine(t, 300))
			}
		}
	}
	return r
}

func truncateLine(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// detectCeph 经任一在线节点 SSH 判 /etc/pve/ceph.conf 是否存在(pmxcfs 集群-wide)。
func (s *service) detectCeph(ctx context.Context, cl *Cluster, nodes []NodeStatus) (bool, error) {
	var lastErr error
	for _, n := range nodes {
		if !n.Online {
			continue
		}
		sid, err := s.nodeServerID(ctx, cl, n.Name)
		if err != nil {
			lastErr = err
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, majorCmdTO)
		res, rerr := s.runner.Run(rctx, sid, []string{"test", "-f", "/etc/pve/ceph.conf"})
		cancel()
		if rerr == nil {
			return true, nil
		}
		if res != nil && res.ExitCode == 1 {
			return false, nil // test 假:文件不存在
		}
		lastErr = rerr
	}
	if lastErr != nil {
		return false, fmt.Errorf("%w: %v", ErrProbe, unwrapTail(lastErr))
	}
	return false, nil
}

// runMajorCmd 带独立超时的提权 SSH 命令(非零退出返回 res+err,stdout 仍可解析)。
func (s *service) runMajorCmd(ctx context.Context, sid string, timeout time.Duration, cmd []string) (*target.ExecResult, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return s.runner.Run(rctx, sid, cmd)
}

// parsePveRelease 拆 release 串(如 "8.4.1")为主/次版本。
func parsePveRelease(rel string) (major, minor int) {
	parts := strings.SplitN(strings.TrimSpace(rel), ".", 3)
	major, _ = strconv.Atoi(parts[0])
	if len(parts) > 1 {
		minor, _ = strconv.Atoi(parts[1])
	}
	return major, minor
}

var pveVersionRe = regexp.MustCompile(`pve-manager/(\d+)\.(\d+)`)

// pveversionMajor 解析 pveversion 输出的主版本(失败 → 0)。
func pveversionMajor(out string) int {
	m := pveVersionRe.FindStringSubmatch(out)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// ---- 持久化 -----------------------------------------------------------------------------

func (s *service) insertMajorJob(ctx context.Context, j *majorJob) error {
	now := time.Now().UTC().Format(time.RFC3339)
	j.mu.Lock()
	j.createdAt = now
	orderJSON, _ := json.Marshal(j.nodeOrder)
	id, clusterID, status, policy, preflight := j.id, j.clusterID, j.status, j.guestPolicy, j.preflightJSON
	nodes := append([]MajorNodeStatus(nil), j.nodes...)
	j.mu.Unlock()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO pve_upgrade_jobs (id, cluster_id, kind, status, guest_policy, node_order, preflight, error, created_at, updated_at) VALUES (?, ?, 'major', ?, ?, ?, ?, '', ?, ?)`,
		id, clusterID, status, policy, string(orderJSON), preflight, now, now); err != nil {
		return fmt.Errorf("pveadmin: insert upgrade job: %w", err)
	}
	for _, n := range nodes {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO pve_upgrade_job_nodes (job_id, node, seq, state, error, started_at, finished_at) VALUES (?, ?, ?, ?, '', '', '')`,
			id, n.Node, n.Seq, n.State); err != nil {
			return fmt.Errorf("pveadmin: insert upgrade job node: %w", err)
		}
	}
	return nil
}

func (s *service) persistMajorStatus(ctx context.Context, j *majorJob) {
	j.mu.Lock()
	id, status, errMsg := j.id, j.status, j.errMsg
	j.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx, `UPDATE pve_upgrade_jobs SET status = ?, error = ?, updated_at = ? WHERE id = ?`,
		status, errMsg, now, id); err != nil {
		j.logf("落库任务状态失败: %v", err)
	}
}

func (s *service) persistMajorNode(ctx context.Context, j *majorJob, n MajorNodeStatus) {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE pve_upgrade_job_nodes SET state = ?, error = ?, started_at = ?, finished_at = ? WHERE job_id = ? AND node = ?`,
		n.State, n.Error, n.StartedAt, n.FinishedAt, j.id, n.Node); err != nil {
		// 落库失败只记日志(状态机继续;重启后按 DB 旧状态恢复,节点步骤幂等)。
		j.logf("落库节点 %s 状态失败: %v", n.Node, err)
	}
}

// loadMajorJob 从 DB 取集群最近一次大版本任务;running 无内存对应 → interrupted。
// 无任务 → (nil, nil)。
func (s *service) loadMajorJob(ctx context.Context, clusterID string) (*majorJob, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, status, guest_policy, node_order, preflight, error, created_at FROM pve_upgrade_jobs WHERE cluster_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`, clusterID)
	var j majorJob
	var orderJSON, preflight, errMsg, createdAt string
	if err := row.Scan(&j.id, &j.status, &j.guestPolicy, &orderJSON, &preflight, &errMsg, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pveadmin: load upgrade job: %w", err)
	}
	j.clusterID = clusterID
	j.preflightJSON = preflight
	j.errMsg = errMsg
	j.createdAt = createdAt
	_ = json.Unmarshal([]byte(orderJSON), &j.nodeOrder)
	rows, err := s.db.QueryContext(ctx,
		`SELECT node, seq, state, error, started_at, finished_at FROM pve_upgrade_job_nodes WHERE job_id = ? ORDER BY seq`, j.id)
	if err != nil {
		return nil, fmt.Errorf("pveadmin: load upgrade job nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var n MajorNodeStatus
		if err := rows.Scan(&n.Node, &n.Seq, &n.State, &n.Error, &n.StartedAt, &n.FinishedAt); err != nil {
			return nil, fmt.Errorf("pveadmin: scan upgrade job node: %w", err)
		}
		j.nodes = append(j.nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pveadmin: iterate upgrade job nodes: %w", err)
	}
	if j.status == MajorStatusRunning {
		j.status = MajorStatusInterrupted
		j.errMsg = "控制端重启,任务中断;请核对集群状态后人工续跑"
		now := time.Now().UTC().Format(time.RFC3339)
		if _, uerr := s.db.ExecContext(ctx, `UPDATE pve_upgrade_jobs SET status = ?, error = ?, updated_at = ? WHERE id = ?`,
			j.status, j.errMsg, now, j.id); uerr != nil {
			return nil, fmt.Errorf("pveadmin: mark interrupted: %w", uerr)
		}
	}
	j.running = false
	return &j, nil
}

// majorJobFor 取内存任务,缺失则从 DB 懒加载(并放入内存表)。
func (s *service) majorJobFor(ctx context.Context, id string) (*majorJob, error) {
	s.mu.Lock()
	j := s.majorJobs[id]
	s.mu.Unlock()
	if j != nil {
		return j, nil
	}
	loaded, err := s.loadMajorJob(ctx, id)
	if err != nil || loaded == nil {
		return nil, err
	}
	s.mu.Lock()
	if s.majorJobs == nil {
		s.majorJobs = map[string]*majorJob{}
	}
	// 并发加载竞态:已有内存任务则用内存的。
	if cur := s.majorJobs[id]; cur != nil {
		s.mu.Unlock()
		return cur, nil
	}
	s.majorJobs[id] = loaded
	s.mu.Unlock()
	return loaded, nil
}

// ---- 启动 / 快照 / 中止 / 重试 / 续跑 ----------------------------------------------------

// MajorUpgradeStatus 返回大版本升级快照(DB-backed;从未启动 → 空快照)。
func (s *service) MajorUpgradeStatus(ctx context.Context, id string) (MajorUpgradeStatus, error) {
	if _, err := s.clusterByID(ctx, id); err != nil {
		return MajorUpgradeStatus{}, err
	}
	j, err := s.majorJobFor(ctx, id)
	if err != nil {
		return MajorUpgradeStatus{}, err
	}
	if j == nil {
		return MajorUpgradeStatus{}, nil
	}
	return j.snapshot(), nil
}

// StartMajorUpgrade 校验闸门后异步启动滚动升级(持久化)。
func (s *service) StartMajorUpgrade(ctx context.Context, id string, in MajorUpgradeInput) error {
	cl, c, err := s.clientFor(ctx, id)
	if err != nil {
		return err
	}
	if cl.Readonly {
		return fmt.Errorf("%w: 集群 %s 处于只读模式,请在设置页解锁后再升级", ErrReadonly, cl.Name)
	}
	if in.Confirm != cl.Name {
		return fmt.Errorf("%w: 须原样输入集群名 %q 以确认大版本升级", ErrConfirmMismatch, cl.Name)
	}
	policy := in.GuestPolicy
	if policy == "" {
		policy = GuestPolicyWarn
	}
	if policy != GuestPolicyWarn && policy != GuestPolicyShutdown {
		return fmt.Errorf("%w: guestPolicy 须为 warn/shutdown", ErrInvalidInput)
	}
	// 预检新鲜度 + 阻断 + 警告 ack。
	s.mu.Lock()
	entry, ok := s.preflights[id]
	s.mu.Unlock()
	if !ok || time.Since(entry.at) > majorPreflightFreshness {
		return fmt.Errorf("%w: 请先运行预检(报告 30 分钟内有效)", ErrPreflightRequired)
	}
	rep := entry.report
	if rep.Blocked {
		return fmt.Errorf("%w: 预检未通过,请先修复 FAIL 项", ErrPreflightRequired)
	}
	acked := make(map[string]bool, len(in.Acks))
	for _, k := range in.Acks {
		acked[k] = true
	}
	for _, k := range rep.AcksRequired {
		if !acked[k] {
			return fmt.Errorf("%w: 警告 %s 未确认", ErrPreflightRequired, k)
		}
	}
	// 快速复查阻断项(预检后集群状态可能变化)。
	cn, err := fetchClusterNodes(ctx, c)
	if err != nil {
		return err
	}
	if !cn.Quorate {
		return fmt.Errorf("%w: 集群已无仲裁,禁止启动", ErrPreflightRequired)
	}
	if ceph, cephErr := s.detectCeph(ctx, cl, cn.Nodes); ceph {
		return fmt.Errorf("%w: 检测到 Ceph,须人工按官方流程处理", ErrCephDetected)
	} else if cephErr != nil {
		return fmt.Errorf("%w: 无法复查 Ceph 状态: %v", ErrPreflightRequired, unwrapTail(cephErr))
	}
	nodes, err := s.majorTargetNodes(ctx, c, cn, in.Nodes)
	if err != nil {
		return err
	}
	jobID, err := newCreateJobID()
	if err != nil {
		return fmt.Errorf("pveadmin: 创建任务标识: %w", err)
	}
	preflightJSON, _ := json.Marshal(rep)

	s.mu.Lock()
	cl, err = s.clusterByID(ctx, id)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if cl.Readonly {
		s.mu.Unlock()
		return fmt.Errorf("%w: 集群 %s 处于只读模式,请在设置页解锁后再升级", ErrReadonly, cl.Name)
	}
	if s.majorJobs == nil {
		s.majorJobs = map[string]*majorJob{}
	}
	if s.anyJobRunningLocked(id) {
		s.mu.Unlock()
		return fmt.Errorf("%w: 集群 %s 已有建机/升级任务在运行", ErrBusy, cl.Name)
	}
	j := &majorJob{
		id: jobID, clusterID: cl.ID, running: true, status: MajorStatusRunning,
		guestPolicy: policy, step: MajorNodePending, message: "准备滚动升级…",
		nodeOrder: nodes, preflightJSON: string(preflightJSON),
	}
	for i, n := range nodes {
		j.nodes = append(j.nodes, MajorNodeStatus{Node: n, Seq: i, State: MajorNodePending})
	}
	s.majorJobs[id] = j
	s.mu.Unlock()

	if err := s.insertMajorJob(ctx, j); err != nil {
		s.mu.Lock()
		delete(s.majorJobs, id)
		s.mu.Unlock()
		return err
	}
	go s.runMajor(j)
	return nil
}

// majorTargetNodes 解析升级目标:在线 8.x 节点(9.x 跳过/显式指定则报错),按名排序或给定顺序。
func (s *service) majorTargetNodes(ctx context.Context, c pveClient, cn *clusterNodeList, requested []string) ([]string, error) {
	member := make(map[string]bool, len(cn.Nodes))
	for _, n := range cn.Nodes {
		member[n.Name] = n.Online
	}
	versionOf := func(node string) (int, error) {
		var ver apiVersion
		if err := c.Get(ctx, "/api2/json/nodes/"+node+"/version", &ver); err != nil {
			return 0, err
		}
		major, _ := parsePveRelease(ver.Release)
		return major, nil
	}
	if len(requested) == 0 {
		var out []string
		for _, n := range cn.Nodes {
			if !n.Online {
				// 官方前提是集群健康:任一节点离线即禁止开始,避免静默漏升级
				// 后终验只查 nodeOrder 而误报「全部完成」。
				return nil, fmt.Errorf("%w: 节点 %s 离线,禁止开始升级", ErrPreflightRequired, n.Name)
			}
			major, err := versionOf(n.Name)
			if err != nil {
				return nil, err
			}
			if major == 8 {
				out = append(out, n.Name)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("%w: 无在线 8.x 节点可升级", ErrInvalidInput)
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
		if !ok || !online {
			return nil, fmt.Errorf("%w: 节点 %s 不在集群内或离线", ErrInvalidInput, n)
		}
		if seen[n] {
			continue
		}
		major, err := versionOf(n)
		if err != nil {
			return nil, err
		}
		if major != 8 {
			return nil, fmt.Errorf("%w: 节点 %s 不是 8.x(当前主版本 %d)", ErrInvalidInput, n, major)
		}
		seen[n] = true
		out = append(out, n)
	}
	return out, nil
}

// AbortMajorUpgrade 请求中止(只在步骤边界生效,绝不中断 dist-upgrade)。
func (s *service) AbortMajorUpgrade(ctx context.Context, id string) error {
	if _, err := s.clusterByID(ctx, id); err != nil {
		return err
	}
	j, err := s.majorJobFor(ctx, id)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("%w: 无大版本升级任务", ErrInvalidInput)
	}
	j.mu.Lock()
	if !j.running {
		j.mu.Unlock()
		return fmt.Errorf("%w: 任务未在运行", ErrInvalidInput)
	}
	j.abort = true
	j.mu.Unlock()
	j.logf("中止请求已受理:将在当前步骤结束后生效(绝不中断 dist-upgrade)")
	return nil
}

// RetryMajorNode 重跑失败节点(幂等:已 trixie 跳过换源,dist-upgrade 天然幂等)。
func (s *service) RetryMajorNode(ctx context.Context, id, node, confirm string) error {
	cl, err := s.clusterByID(ctx, id)
	if err != nil {
		return err
	}
	if confirm != cl.Name {
		return fmt.Errorf("%w: 须原样输入集群名 %q 以确认重试", ErrConfirmMismatch, cl.Name)
	}
	j, err := s.majorJobFor(ctx, id)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("%w: 无大版本升级任务", ErrInvalidInput)
	}
	j.mu.Lock()
	if j.running {
		j.mu.Unlock()
		return fmt.Errorf("%w: 任务运行中,无需重试", ErrBusy)
	}
	if j.status != MajorStatusPaused && j.status != MajorStatusInterrupted {
		j.mu.Unlock()
		return fmt.Errorf("%w: 任务状态 %s 不允许重试", ErrInvalidInput, j.status)
	}
	idx := -1
	for i := range j.nodes {
		if j.nodes[i].Node == node {
			idx = i
			break
		}
	}
	if idx < 0 {
		j.mu.Unlock()
		return fmt.Errorf("%w: 节点 %s 不在本任务中", ErrInvalidInput, node)
	}
	if j.nodes[idx].State == MajorNodeDone {
		j.mu.Unlock()
		return fmt.Errorf("%w: 节点 %s 已完成,无需重试", ErrInvalidInput, node)
	}
	j.nodes[idx].State = MajorNodePending
	j.nodes[idx].Error = ""
	j.nodes[idx].StartedAt = ""
	j.nodes[idx].FinishedAt = ""
	reset := j.nodes[idx]
	j.status = MajorStatusRunning
	j.running = true
	j.errMsg = ""
	j.abort = false
	j.mu.Unlock()
	s.persistMajorNode(ctx, j, reset)
	s.persistMajorStatus(ctx, j)
	j.logf("重试节点 %s", node)
	go s.runMajor(j)
	return nil
}

// ResumeMajorUpgrade 从首个未完成节点续跑(interrupted/paused;绝不自动续跑)。
func (s *service) ResumeMajorUpgrade(ctx context.Context, id, confirm string) error {
	cl, c, err := s.clientFor(ctx, id)
	if err != nil {
		return err
	}
	if confirm != cl.Name {
		return fmt.Errorf("%w: 须原样输入集群名 %q 以确认续跑", ErrConfirmMismatch, cl.Name)
	}
	j, err := s.majorJobFor(ctx, id)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("%w: 无大版本升级任务", ErrInvalidInput)
	}
	j.mu.Lock()
	if j.running {
		j.mu.Unlock()
		return fmt.Errorf("%w: 任务已在运行", ErrBusy)
	}
	if j.status != MajorStatusPaused && j.status != MajorStatusInterrupted {
		j.mu.Unlock()
		return fmt.Errorf("%w: 任务状态 %s 不允许续跑", ErrInvalidInput, j.status)
	}
	j.status = MajorStatusRunning
	j.running = true
	j.errMsg = ""
	j.abort = false
	j.mu.Unlock()
	cn, err := fetchClusterNodes(ctx, c)
	if err != nil {
		s.majorPause(ctx, j, "续跑前取集群状态失败: "+unwrapTail(err))
		return err
	}
	if !cn.Quorate {
		s.majorPause(ctx, j, "集群无仲裁,禁止续跑")
		return fmt.Errorf("%w: 集群无仲裁,禁止续跑", ErrPreflightRequired)
	}
	s.persistMajorStatus(ctx, j)
	j.logf("人工续跑:从首个未完成节点继续")
	go s.runMajor(j)
	return nil
}

// ---- 滚动状态机 -------------------------------------------------------------------------

// majorPause 暂停任务(fail-fast/中止/续跑失败),落库。
func (s *service) majorPause(ctx context.Context, j *majorJob, reason string) {
	j.mu.Lock()
	j.running = false
	j.status = MajorStatusPaused
	j.errMsg = reason
	j.mu.Unlock()
	j.logf("任务暂停: %s", reason)
	s.persistMajorStatus(ctx, j)
}

func (s *service) majorDone(ctx context.Context, j *majorJob) {
	j.mu.Lock()
	j.running = false
	j.status = MajorStatusDone
	j.errMsg = ""
	j.step = MajorNodeDone
	j.message = "全部节点升级完成"
	j.mu.Unlock()
	j.logf("集群升级完成:全部节点 9.x")
	s.persistMajorStatus(ctx, j)
}

// setMajorNodeState 更新节点状态(内存 + 落库)并同步任务 step/message。
func (s *service) setMajorNodeState(ctx context.Context, j *majorJob, i int, state, errMsg string) {
	now := time.Now().UTC().Format(time.RFC3339)
	j.mu.Lock()
	n := &j.nodes[i]
	n.State = state
	n.Error = errMsg
	if state != MajorNodePending && n.StartedAt == "" {
		n.StartedAt = now
	}
	if state == MajorNodeDone || state == MajorNodeFailed {
		n.FinishedAt = now
	}
	cp := *n
	j.step = state
	j.message = fmt.Sprintf("节点 %s: %s", n.Node, state)
	j.mu.Unlock()
	s.persistMajorNode(ctx, j, cp)
}

// runMajor 是滚动升级主体(严格串行;fail-fast;跳过已完成节点)。
func (s *service) runMajor(j *majorJob) {
	ctx := context.Background()
	for i := range j.nodes {
		if j.abortRequested() {
			s.majorPause(ctx, j, "已中止(用户请求)")
			return
		}
		j.mu.Lock()
		state := j.nodes[i].State
		node := j.nodes[i].Node
		j.mu.Unlock()
		if state == MajorNodeDone {
			continue
		}
		j.logf("开始节点 %s(%d/%d)", node, i+1, len(j.nodes))
		err := s.runMajorNode(ctx, j, i)
		if err != nil {
			if errors.Is(err, errMajorAbort) {
				s.majorPause(ctx, j, "已中止(用户请求,当前步骤已完成)")
				return
			}
			s.setMajorNodeState(ctx, j, i, MajorNodeFailed, err.Error())
			j.logf("[%s] 失败: %v", node, err)
			s.majorPause(ctx, j, fmt.Sprintf("节点 %s 失败,任务暂停(可重试该节点或中止)", node))
			return
		}
		s.setMajorNodeState(ctx, j, i, MajorNodeDone, "")
		j.logf("[%s] 升级完成", node)
	}
	if err := s.majorFinalVerify(ctx, j); err != nil {
		s.majorPause(ctx, j, "终验失败: "+unwrapTail(err))
		return
	}
	s.majorDone(ctx, j)
}

// runMajorNode 单节点状态机:guests → sources → upgrading → rebooting → waiting → verifying。
func (s *service) runMajorNode(ctx context.Context, j *majorJob, i int) error {
	j.mu.Lock()
	node := j.nodes[i].Node
	policy := j.guestPolicy
	j.mu.Unlock()
	cl, c, err := s.clientFor(ctx, j.clusterID)
	if err != nil {
		return err
	}
	sid, err := s.nodeServerID(ctx, cl, node)
	if err != nil {
		return err
	}

	// 1) guest 处理。
	s.setMajorNodeState(ctx, j, i, MajorNodeGuests, "")
	var resources []apiResource
	if err := c.Get(ctx, "/api2/json/cluster/resources", &resources); err != nil {
		return err
	}
	var running []apiResource
	for _, r := range resources {
		if r.Node == node && (r.Type == "qemu" || r.Type == "lxc") && r.Status == "running" {
			running = append(running, r)
		}
	}
	if policy == GuestPolicyShutdown {
		for _, r := range running {
			if j.abortRequested() {
				return errMajorAbort
			}
			j.logf("[%s] 关停 %s/%d…", node, r.Type, int64(r.VMID))
			upid, perr := postUPID(ctx, c, fmt.Sprintf("/api2/json/nodes/%s/%s/%d/status/shutdown", node, r.Type, int64(r.VMID)), url.Values{})
			if perr != nil {
				return fmt.Errorf("关停 %s/%d 失败: %w", r.Type, int64(r.VMID), perr)
			}
			if werr := waitTask(ctx, c, node, upid, majorGuestShutdownTO); werr != nil {
				return fmt.Errorf("关停 %s/%d 超时/失败(不强停): %w", r.Type, int64(r.VMID), werr)
			}
		}
	} else if len(running) > 0 {
		names := make([]string, 0, len(running))
		for _, r := range running {
			names = append(names, fmt.Sprintf("%s/%d", r.Type, int64(r.VMID)))
			if r.HAState != "" && r.HAState != "ignored" {
				j.logf("[%s] 注意:%s 由 HA 管理,重启会触发 HA 恢复", node, r.Name)
			}
		}
		j.logf("[%s] 警告:重启将硬停 %d 个运行中 guest(%s)", node, len(running), strings.Join(names, ", "))
	}

	// 2) 换源 bookworm→trixie(复用仓库管理的解析/备份/原子写)。
	if j.abortRequested() {
		return errMajorAbort
	}
	s.setMajorNodeState(ctx, j, i, MajorNodeSources, "")
	files, _, err := s.readAptRepoFiles(ctx, cl, node)
	if err != nil {
		return err
	}
	var changes []aptFileChange
	for _, f := range files {
		if f.applySuite("bookworm", "trixie") {
			changes = append(changes, aptFileChange{path: f.path, content: f.render()})
		}
	}
	if len(changes) > 0 {
		res, cerr := s.commitAptFiles(ctx, cl, node, changes, majorAptUpdateTO)
		if cerr != nil {
			return cerr
		}
		if !res.UpdateOK {
			return fmt.Errorf("换源后 apt update 失败(备份已保留): %s", res.UpdateTail)
		}
		j.logf("[%s] 已换源 bookworm→trixie(%d 个文件)", node, len(changes))
	} else {
		j.logf("[%s] 源已是 trixie,跳过换源", node)
		if _, uerr := s.runMajorCmd(ctx, sid, majorAptUpdateTO, []string{"apt-get", "update"}); uerr != nil {
			return fmt.Errorf("apt update 失败: %w", uerr)
		}
	}

	// 3) dist-upgrade(期间不可中止;noninteractive + 保留旧配置)。
	if j.abortRequested() {
		return errMajorAbort
	}
	s.setMajorNodeState(ctx, j, i, MajorNodeUpgrading, "")
	j.logf("[%s] 开始 dist-upgrade(可能 10-30 分钟,期间不可中止)…", node)
	res, uerr := s.runMajorCmd(ctx, sid, majorDistUpgradeTO, []string{
		"env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "-y",
		"-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold",
		"dist-upgrade",
	})
	if uerr != nil {
		tail := ""
		if res != nil {
			tail = tailOutput(res)
		}
		return fmt.Errorf("dist-upgrade 失败: %v\n%s", unwrapTail(uerr), tail)
	}
	if res != nil {
		j.logf("[%s] dist-upgrade 输出尾部:\n%s", node, tailOutput(res))
	}
	// 升级后复检:pve8to9(信息性)+ pveversion 须 9.x。
	if r8, e8 := s.runMajorCmd(ctx, sid, majorPve8to9TO, []string{"pve8to9"}); e8 == nil && r8 != nil {
		pr := parsePve8to9(r8.Stdout)
		j.logf("[%s] 升级后 pve8to9: %d PASS / %d WARN / %d FAIL", node, pr.Passed, pr.Warnings, pr.Failures)
	}
	pv, perr := s.runMajorCmd(ctx, sid, majorCmdTO, []string{"pveversion"})
	if perr != nil {
		return fmt.Errorf("升级后取 pveversion 失败: %w", perr)
	}
	if m := pveversionMajor(pv.Stdout); m != 9 {
		return fmt.Errorf("升级后 pveversion 主版本为 %d(须 9),dist-upgrade 可能未完成", m)
	}

	// 4) 重启(API;不 waitTask——节点会下线)。
	if j.abortRequested() {
		return errMajorAbort
	}
	// 记录重启前 uptime,回线后据此确认节点真的重启过(防 SSH 瞬断误判下线)。
	preUptime := int64(-1)
	var preSt apiNodeStatus
	if err := c.Get(ctx, "/api2/json/nodes/"+node+"/status", &preSt); err == nil {
		preUptime = int64(preSt.Uptime)
	}
	s.setMajorNodeState(ctx, j, i, MajorNodeRebooting, "")
	j.logf("[%s] 重启节点…", node)
	_, rerr := postUPID(ctx, c, "/api2/json/nodes/"+node+"/status", url.Values{"command": {"reboot"}})
	if rerr != nil {
		var apiErr *pveAPIError
		if errors.As(rerr, &apiErr) {
			return fmt.Errorf("重启指令被拒: %w", rerr)
		}
		// 传输层错误:节点可能在响应前已下线,视为重启中,由等待阶段裁决。
		j.logf("[%s] 重启指令发出后连接中断(视为重启中)", node)
	}

	// 5) 等节点回线 + 集群重新 quorate。
	s.setMajorNodeState(ctx, j, i, MajorNodeWaiting, "")
	if err := s.waitNodeBack(ctx, j, c, node, sid, preUptime); err != nil {
		return err
	}

	// 6) 验证 9.x + 在线 + quorate。
	s.setMajorNodeState(ctx, j, i, MajorNodeVerifying, "")
	var ver apiVersion
	if err := c.Get(ctx, "/api2/json/nodes/"+node+"/version", &ver); err != nil {
		return err
	}
	if m, _ := parsePveRelease(ver.Release); m != 9 {
		return fmt.Errorf("回线后版本仍为 %s(须 9.x)", ver.Release)
	}
	cn, err := fetchClusterNodes(ctx, c)
	if err != nil {
		return err
	}
	if !cn.Quorate {
		return fmt.Errorf("节点回线但集群无仲裁")
	}
	j.logf("[%s] 验证通过: %s", node, ver.Release)
	return nil
}

// waitNodeBack 两阶段等待:先等节点下线(重启生效),再等回线 + quorate + SSH 可达。
// preUptime 是重启前 uptime(秒,<0 表示未取到);回线后 uptime 未重置 → 判定未真正重启。
func (s *service) waitNodeBack(ctx context.Context, j *majorJob, c pveClient, node, sid string, preUptime int64) error {
	// 阶段 1:等下线(SSH 失败或 API 离线;重启可能因 guest 关停耗时)。
	downCtx, cancel := context.WithTimeout(ctx, majorRebootDownTO)
	defer cancel()
	down := false
	for !down {
		cn, err := fetchClusterNodes(downCtx, c)
		if err == nil {
			for _, n := range cn.Nodes {
				if n.Name == node && !n.Online {
					down = true
				}
			}
		}
		if !down {
			rctx, rcancel := context.WithTimeout(downCtx, majorCmdTO)
			_, serr := s.runner.Run(rctx, sid, []string{"pveversion"})
			rcancel()
			if serr != nil {
				down = true
			}
		}
		if down {
			break
		}
		select {
		case <-downCtx.Done():
			return fmt.Errorf("节点 %s 在 %s 内未进入重启(仍在线),请人工核查", node, majorRebootDownTO)
		case <-time.After(majorRebootPoll):
		}
	}
	j.logf("[%s] 节点已下线,等待回线…", node)
	// 阶段 2:等回线 + quorate + SSH。
	upCtx, cancel2 := context.WithTimeout(ctx, majorRebootWaitTO)
	defer cancel2()
	for {
		cn, err := fetchClusterNodes(upCtx, c)
		if err == nil && cn.Quorate {
			online := false
			for _, n := range cn.Nodes {
				if n.Name == node && n.Online {
					online = true
				}
			}
			if online {
				rctx, rcancel := context.WithTimeout(upCtx, majorCmdTO)
				_, serr := s.runner.Run(rctx, sid, []string{"pveversion"})
				rcancel()
				if serr == nil {
					// uptime 未重置 → 节点其实没重启(阶段 1 的 SSH 失败可能是瞬断)。
					if preUptime > 60 {
						var postSt apiNodeStatus
						if gerr := c.Get(upCtx, "/api2/json/nodes/"+node+"/status", &postSt); gerr == nil &&
							int64(postSt.Uptime) >= preUptime {
							return fmt.Errorf("节点 %s 未实际重启(uptime %ds ≥ 重启前 %ds),请人工核查",
								node, int64(postSt.Uptime), preUptime)
						}
					}
					return nil
				}
			}
		}
		select {
		case <-upCtx.Done():
			return fmt.Errorf("节点 %s 在 %s 内未回线/集群未恢复仲裁", node, majorRebootWaitTO)
		case <-time.After(majorRebootPoll):
		}
	}
}

// majorFinalVerify 终验:全部目标节点 9.x + 集群 quorate。
func (s *service) majorFinalVerify(ctx context.Context, j *majorJob) error {
	_, c, err := s.clientFor(ctx, j.clusterID)
	if err != nil {
		return err
	}
	cn, err := fetchClusterNodes(ctx, c)
	if err != nil {
		return err
	}
	if !cn.Quorate {
		return fmt.Errorf("集群无仲裁")
	}
	j.mu.Lock()
	nodes := append([]string(nil), j.nodeOrder...)
	j.mu.Unlock()
	for _, node := range nodes {
		var ver apiVersion
		if err := c.Get(ctx, "/api2/json/nodes/"+node+"/version", &ver); err != nil {
			return fmt.Errorf("节点 %s 取版本失败: %w", node, err)
		}
		if m, _ := parsePveRelease(ver.Release); m != 9 {
			return fmt.Errorf("节点 %s 版本 %s 不是 9.x", node, ver.Release)
		}
	}
	return nil
}
