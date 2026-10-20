// Package firewall 是「集群防火墙访问限制」的领域层:把平台本机的指定端口(默认
// 8080/80/443/5000/5001)经 iptables 限制为仅本机与 mesh 虚拟网段(可加补充网段)可访问。
//
// 两步生效(防误锁死):
//   - apply   → 临时生效:规则只写运行时 iptables(phase=pending,不参与持久断言)。
//     机器重启规则自然消失;平台进程重启由 KeepAlive 启动时主动拆掉并重放固化版;
//     超时(默认 10 分钟)未确认自动回退 —— 三条路都回到「未开启或上一个固化版本」。
//   - confirm → 固化:调用方传入经反代信任模型解析出的真实来源 IP(internal/httpapi
//     的 clientIP:仅回环对端/显式可信反代才采信转发头,X-Real-IP 优先),仅当其在
//     放行集(回环 ∪ pending 网段快照)内才把 pending 快照晋升 confirmed;此后
//     KeepAlive(启动 + 每 60s)幂等断言 confirmed 配置,跨重启自愈。
//     绝不碰 iptables 持久化体系(iptables-save/netfilter-persistent 一律不动)。
//
// 规则形态(全部带 -m comment --comment pipewright:mesh-guard 标签,幂等,见 iptables.go):
//   - INPUT 链 PW-MESH-GUARD(主机进程端口:平台自身 8080、宿主 nginx 80/443);
//   - DOCKER-USER 链 PW-MESH-GUARD-D(Docker 发布端口:内置 registry 5000/5001、网关
//     容器 80/443 —— 这类流量走 FORWARD 不走 INPUT),未命中端口 RETURN 不误伤其他容器;
//   - ip6tables 同限(防 IPv6 绕过;best-effort)。
//
// 设计纪律(与 platformhttps 一致):
//   - 命令一律 array,绝不拼 shell(AC-SEC-02);提权 root 直行 > sudo -n 免密,皆不可用
//     报人话指引(不涉密码,绝不挂起等 TTY)。
//   - 保存草稿不碰机器;apply/confirm/rollback/disable 是显式动作,结果(人话)记回单行。
//   - rollback/disable 不设来源门 —— 它们就是逃生通道,永远可用。
package firewall

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Phase 是守卫的生命周期阶段(单行 state 机)。
const (
	// PhaseOff 表示未开启(无任何内核规则;草稿可编辑)。
	PhaseOff = "off"
	// PhasePending 表示临时生效中:规则已在运行时 iptables,等待从放行来源确认固化;
	// 机器重启/平台重启/超时/手动回退都回到 off 或上一个固化版本。
	PhasePending = "pending"
	// PhaseActive 表示已固化:confirmed 快照生效,KeepAlive 跨重启幂等断言。
	PhaseActive = "active"
)

// 最近一次动作结果(展示用;” = 从未)。
const (
	resultOK       = "ok"
	resultFailed   = "failed"
	resultReverted = "reverted"
)

const (
	// pendingTTL 是临时生效的确认窗口:超时未确认自动回退(安全网,防确认入口彻底
	// 不可达时把管理员锁在外面;确认动作本身随时可重新 apply)。
	pendingTTL = 10 * time.Minute
	// keepAliveTick 是 KeepAlive 周期(断言固化版规则在位、检查 pending 超时)。
	keepAliveTick = 60 * time.Second
)

// PendingTTL 返回临时生效的确认窗口时长(展示层倒计时用)。
func PendingTTL() time.Duration { return pendingTTL }

// 领域错误(httpapi 层映射状态码;错误体人话)。
var (
	// ErrNotLinux 表示运行平台非 Linux(功能仅面向 Linux 宿主机)。
	ErrNotLinux = fmt.Errorf("firewall: 仅支持 Linux 宿主机")
	// ErrInContainer 表示平台跑在容器里(容器内 iptables 管不到宿主网络命名空间)。
	ErrInContainer = fmt.Errorf("firewall: 容器部署形态下无法管理宿主防火墙")
	// ErrNoIptables 表示本机找不到 iptables 二进制。
	ErrNoIptables = fmt.Errorf("firewall: 本机未找到 iptables")
	// ErrNoPrivilege 表示平台运行用户非 root 且免密 sudo 不可用。
	ErrNoPrivilege = fmt.Errorf("firewall: 无 root 权限且免密 sudo 不可用")
	// ErrMeshUnreachable 表示无法推导 mesh 网段(集群未启用/未连接)且未配置补充网段。
	ErrMeshUnreachable = fmt.Errorf("firewall: 无法推导 mesh 网段且未配置补充网段")
	// ErrInvalidPorts 表示端口列表非法(越界/过多;iptables multiport 上限 15)。
	ErrInvalidPorts = fmt.Errorf("firewall: invalid ports")
	// ErrInvalidCIDR 表示补充网段非法(CIDR 格式)。
	ErrInvalidCIDR = fmt.Errorf("firewall: invalid cidr")
	// ErrNotPending 表示仅临时生效中(phase=pending)可执行该动作。
	ErrNotPending = fmt.Errorf("firewall: 仅临时生效中的规则可确认/回退")
	// ErrIPNotAllowed 表示确认门的真实来源不在放行集。
	ErrIPNotAllowed = fmt.Errorf("firewall: 真实来源不在放行集")
	// ErrApply 表示应用/清理防火墙规则失败(附人话摘要)。
	ErrApply = fmt.Errorf("firewall: 应用防火墙规则失败")
)

// hostGOOS/inContainerMarker 是环境探针的可注入点(单测改写;生产用 runtime/os)。
var (
	hostGOOS          = runtime.GOOS
	inContainerMarker = func() bool {
		// /.dockerenv 由 docker/podman 在容器根目录落盘;distroless 亦有。存在即按
		// 容器形态拒绝(诚实降级好过假装管到了宿主)。
		_, err := osStat("/.dockerenv")
		return err == nil
	}
)

// State 是守卫单行的完整投影(草稿 + 阶段 + 两份快照 + 最近结果)。
type State struct {
	Ports      []int    // 草稿端口(下次 apply 的参数)
	ExtraCIDRs []string // 草稿补充网段(cidr;空 = 仅本机 + mesh 自动网段)

	Phase            string
	PendingPorts     []int
	PendingCIDRs     []string
	PendingAppliedAt time.Time
	ConfirmedPorts   []int
	ConfirmedCIDRs   []string

	LastResult    string // '' | ok | failed | reverted
	StatusDetail  string
	LastAppliedAt time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// DraftInput 是保存草稿的入参(指针字段,nil = 保持不变)。
type DraftInput struct {
	Ports      *[]int
	ExtraCIDRs *[]string
}

// Service 定义守卫领域对外接口(httpapi 消费;StartKeepAlive 供 main 装配)。
type Service interface {
	// Get 返回单行状态(库为空时插入惰性默认行,无副作用)。
	Get(ctx context.Context) (*State, error)
	// SaveDraft 校验并保存草稿(只写库,不碰机器)。
	SaveDraft(ctx context.Context, in DraftInput) (*State, error)
	// Detect 探测本机执行前提(OS/容器形态/iptables/提权/DOCKER-USER),全 best-effort。
	Detect(ctx context.Context) *DetectInfo
	// Apply 把草稿写进运行时 iptables(临时生效,phase=pending)。前置:Detect 全绿 +
	// 放行网段可推导(mesh 自身 IP 所在 /24 ∪ 补充网段)。
	Apply(ctx context.Context) (*State, error)
	// Confirm 把 pending 快照晋升 confirmed(phase=active)。realIP 是调用方按反代
	// 信任模型解析出的真实来源 IP;不在放行集 → ErrIPNotAllowed(防经 nginx 反代
	// 伪装成本机)。
	Confirm(ctx context.Context, realIP string) (*State, error)
	// Rollback 立即回退临时规则:有固化版则重放之(phase=active),否则清空(phase=off)。
	Rollback(ctx context.Context) (*State, error)
	// Disable 拆除全部规则并复位 off(草稿保留,便于再次启用)。
	Disable(ctx context.Context) (*State, error)
	// AllowsConfirm 报告真实来源 IP 是否在 pending 放行集(非 pending 恒 false)。
	AllowsConfirm(ctx context.Context, ip string) (bool, error)
	// StartKeepAlive 启动保活循环(阻塞;由 main 以 goroutine 调用):
	// 启动即收敛(pending 拆临时规则重放固化版;active 断言规则在位),此后每分钟
	// 检查固化版漂移(重启/人工 flush 后重放)与 pending 超时回退。
	StartKeepAlive(ctx context.Context)
}

// Options 是 New 的装配项。
type Options struct {
	// Host 是本机命令执行抽象(nil → localHost,os/exec)。
	Host Host
	// DefaultPorts 是草稿惰性默认端口(如 80,443,5000,5001,平台实际监听口)。
	DefaultPorts []int
	// SelfIP 返回 mesh 运行时本节点虚拟 IP(空 = 拿不到;nil 函数同)。由 main 从
	// mesh runtime 注入;apply 据此推导自动放行网段 <ip>/24。
	SelfIP func() string
}

// service 是 store + Host 支撑的 Service 实现。
type service struct {
	store        *Store
	host         Host
	defaultPorts []int
	selfIP       func() string

	// livePending 标记「pending 规则是本进程写入的」:KeepAlive 首拍看到 phase=pending
	// 而 livePending=false,说明规则是上一次平台运行的遗留(内核规则跨进程存活)→ 主动
	// 拆掉并重放固化版,即「平台重启回退到未开或上一个固化版本」的落地。
	livePending atomic.Bool

	// mu 互斥 Apply/Rollback/Disable/KeepAlive 收敛:HTTP 手动动作与后台保活可能并发,
	// 交错执行 iptables 建链/清链会互相破坏。
	mu sync.Mutex
}

// New 构造 Service。host 为 nil 时用本机默认实现;defaultPorts 空时兜底 5 个默认端口。
func New(db *sql.DB, opts Options) Service {
	h := opts.Host
	if h == nil {
		h = localHost{}
	}
	dp := normalizePorts(opts.DefaultPorts)
	if len(dp) == 0 {
		dp = []int{80, 443, 5000, 5001, 8080}
	}
	return &service{store: NewStore(db, dp), host: h, defaultPorts: dp, selfIP: opts.SelfIP}
}

// ---------- 草稿 ----------

// Get 返回单行状态(库为空时插入惰性默认行,无副作用)。
func (s *service) Get(ctx context.Context) (*State, error) {
	return s.store.getOrCreate(ctx)
}

// SaveDraft 校验并保存草稿(ports/extraCidrs;nil 字段保持不变)。纯落库,不碰机器。
func (s *service) SaveDraft(ctx context.Context, in DraftInput) (*State, error) {
	st, err := s.store.getOrCreate(ctx)
	if err != nil {
		return nil, err
	}
	if in.Ports != nil {
		p := normalizePorts(*in.Ports)
		if len(p) == 0 || len(p) > maxPorts {
			return nil, ErrInvalidPorts
		}
		st.Ports = p
	}
	if in.ExtraCIDRs != nil {
		c, err := normalizeCIDRs(*in.ExtraCIDRs)
		if err != nil {
			return nil, err
		}
		st.ExtraCIDRs = c
	}
	if err := s.store.saveDraft(ctx, st); err != nil {
		return nil, err
	}
	return st, nil
}

// ---------- 探测 ----------

// Detect 汇总本机执行前提(不动作、不报错;由 UI 展示人话原因)。
func (s *service) Detect(ctx context.Context) *DetectInfo {
	return s.detect(ctx)
}

// ---------- 应用 / 确认 / 回退 / 禁用 ----------

// Apply 把草稿规则写进运行时 iptables:临时生效(phase=pending),落 pending 快照。
// 前置:Linux 宿主、非容器、iptables 就位、有提权、放行网段可推导。
func (s *service) Apply(ctx context.Context) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.preconditions(ctx); err != nil {
		return nil, err
	}
	st, err := s.store.getOrCreate(ctx)
	if err != nil {
		return nil, err
	}
	cidrs, err := s.resolveAllowCIDRs(st)
	if err != nil {
		return nil, err
	}
	notes, err := s.applyRules(ctx, normalizePorts(st.Ports), cidrs)
	if err != nil {
		// 半途失败:清理我们自己的链,不留半套规则;phase 不变(旧状态继续成立)。
		if cerr := s.cleanupRules(ctx); cerr != nil {
			err = fmt.Errorf("%w:%v(回滚清理亦失败:%v)", ErrApply, trimErr(err), cerr)
		} else {
			err = fmt.Errorf("%w:%v(已回滚清理)", ErrApply, trimErr(err))
		}
		_ = s.store.markResult(ctx, resultFailed, truncate(err.Error(), 2000))
		return nil, err
	}
	detail := strings.Join(notes, ";")
	now := time.Now().UTC()
	if err := s.store.setPending(ctx, normalizePorts(st.Ports), cidrs, detail, now); err != nil {
		return nil, err
	}
	s.livePending.Store(true)
	return s.store.getOrCreate(ctx)
}

// Confirm 把 pending 快照晋升 confirmed(phase=active)。realIP 为反代信任模型解析出的
// 真实来源:须在 回环 ∪ pending 网段 内,否则 ErrIPNotAllowed(nginx 转发的请求读到的
// 是外部真实 IP,不会误判为本机)。
func (s *service) Confirm(ctx context.Context, realIP string) (*State, error) {
	st, err := s.store.getOrCreate(ctx)
	if err != nil {
		return nil, err
	}
	if st.Phase != PhasePending {
		return nil, ErrNotPending
	}
	if !ipAllowed(realIP, st.PendingCIDRs) {
		return nil, ErrIPNotAllowed
	}
	if err := s.store.setConfirmed(ctx, st.PendingPorts, st.PendingCIDRs); err != nil {
		return nil, err
	}
	return s.store.getOrCreate(ctx)
}

// Rollback 立即回退临时规则(手动逃生通道,不设来源门):有固化版重放之(active),
// 否则清空(off)。
func (s *service) Rollback(ctx context.Context) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rollbackLocked(ctx, "手动回退")
}

// rollbackLocked 是回退的共用实现(调用方持 mu)。reason 进人话 detail。
func (s *service) rollbackLocked(ctx context.Context, reason string) (*State, error) {
	st, err := s.store.getOrCreate(ctx)
	if err != nil {
		return nil, err
	}
	if st.Phase != PhasePending {
		return nil, ErrNotPending
	}
	detail := ""
	if len(st.ConfirmedPorts) > 0 {
		// 重放上一个固化版本(回退 ≠ 关闭)。
		if _, aerr := s.applyRules(ctx, st.ConfirmedPorts, st.ConfirmedCIDRs); aerr != nil {
			err := fmt.Errorf("%w:回退到固化版本失败:%v", ErrApply, trimErr(aerr))
			_ = s.store.markResult(ctx, resultFailed, truncate(err.Error(), 2000))
			return nil, err
		}
		detail = reason + ":已回退到上一个固化版本"
	} else {
		if cerr := s.cleanupRules(ctx); cerr != nil {
			err := fmt.Errorf("%w:清理临时规则失败:%v", ErrApply, trimErr(cerr))
			_ = s.store.markResult(ctx, resultFailed, truncate(err.Error(), 2000))
			return nil, err
		}
		detail = reason + ":临时规则已移除(回到未开启)"
	}
	if err := s.store.setPhase(ctx, st.ConfirmedPorts, detail); err != nil {
		return nil, err
	}
	s.livePending.Store(false)
	return s.store.getOrCreate(ctx)
}

// Disable 拆除全部规则并复位 off(草稿保留)。任何 phase 可用;清理失败则报错且
// phase 不动(避免「显示已禁用但规则仍在」的假象)。
func (s *service) Disable(ctx context.Context) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.preconditions(ctx); err != nil {
		return nil, err
	}
	if err := s.cleanupRules(ctx); err != nil {
		_ = s.store.markResult(ctx, resultFailed, truncate(err.Error(), 2000))
		return nil, fmt.Errorf("%w:%v", ErrApply, trimErr(err))
	}
	if err := s.store.setDisabled(ctx); err != nil {
		return nil, err
	}
	s.livePending.Store(false)
	return s.store.getOrCreate(ctx)
}

// AllowsConfirm 报告真实来源 IP 是否在 pending 放行集(非 pending 恒 false)。
func (s *service) AllowsConfirm(ctx context.Context, ip string) (bool, error) {
	st, err := s.store.getOrCreate(ctx)
	if err != nil {
		return false, err
	}
	if st.Phase != PhasePending {
		return false, nil
	}
	return ipAllowed(ip, st.PendingCIDRs), nil
}

// ---------- 保活 ----------

// StartKeepAlive 阻塞运行保活循环:启动即收敛一次(pending 拆临时规则重放固化版 ——
// 平台重启的回退路径;active 断言固化规则在位 —— 机器重启后的自愈路径),此后每分钟
// 检查固化版漂移与 pending 超时。
func (s *service) StartKeepAlive(ctx context.Context) {
	s.reconcileOnce(ctx)
	t := time.NewTicker(keepAliveTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reconcileOnce(ctx)
		}
	}
}

// reconcileOnce 是一拍保活:pending 超时回退;active 断言固化规则在位(漂移重放)。
func (s *service) reconcileOnce(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, err := s.store.getOrCreate(ctx)
	if err != nil {
		return
	}
	switch st.Phase {
	case PhasePending:
		// 两种回退:pending 是上次平台运行的遗留(livePending=false → 平台重启路径,
		// 主动拆掉重放固化版);或确认窗口超时(TTL 安全网)。
		stale := !s.livePending.Load() || st.PendingAppliedAt.IsZero()
		expired := !st.PendingAppliedAt.IsZero() && time.Since(st.PendingAppliedAt) > pendingTTL
		if stale || expired {
			reason := "确认窗口超时"
			if stale {
				reason = "平台重启"
			}
			if _, rerr := s.rollbackLocked(ctx, reason); rerr != nil {
				logLine("firewall keepalive: 自动回退失败:%v", rerr)
			}
		}
	case PhaseActive:
		if err := s.assertRules(ctx, st.ConfirmedPorts, st.ConfirmedCIDRs); err != nil {
			logLine("firewall keepalive: 断言固化规则失败:%v", err)
			_ = s.store.markResult(ctx, resultFailed, truncate(err.Error(), 2000))
		}
	}
}

// ---------- 内部:前置 / 网段推导 / 判定 ----------

// preconditions 校验 apply/disable 的执行前提(人话哨兵错误)。
func (s *service) preconditions(ctx context.Context) error {
	if hostGOOS != "linux" {
		return ErrNotLinux
	}
	if inContainerMarker() {
		return ErrInContainer
	}
	d := s.detect(ctx)
	if d.Iptables == "" {
		return ErrNoIptables
	}
	if !d.IsRoot && !d.SudoOk {
		return ErrNoPrivilege
	}
	return nil
}

// resolveAllowCIDRs 计算本次放行网段:mesh 自身 IP 所在 /24 ∪ 草稿补充网段。
// SelfIP 拿不到且无补充网段 → ErrMeshUnreachable(宁可不应用,不瞎猜网段)。
func (s *service) resolveAllowCIDRs(st *State) ([]string, error) {
	out := append([]string{}, st.ExtraCIDRs...)
	if s.selfIP != nil {
		if c := meshCIDRFromSelfIP(s.selfIP()); c != "" {
			out = append(out, c)
		}
	}
	out = dedupeStrings(out)
	if len(out) == 0 {
		return nil, ErrMeshUnreachable
	}
	sort.Strings(out)
	return out, nil
}

// meshCIDRFromSelfIP 由虚拟 IP 推导所在 /24(EasyTier 默认 10.126.126.0/24 网形)。
func meshCIDRFromSelfIP(ipStr string) string {
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	if ip == nil || ip.To4() == nil {
		return ""
	}
	b := ip.To4()
	return fmt.Sprintf("%d.%d.%d.0/24", b[0], b[1], b[2])
}

// ipAllowed 报告 ip 是否在 回环 ∪ cidrs 放行集(经 nginx 的请求已由调用方解析成真实
// 外部 IP,此处只做成员判定)。
func ipAllowed(ipStr string, cidrs []string) bool {
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

// ---------- 归一化小件 ----------

// maxPorts 是 multiport 匹配的端口数上限;maxCIDRs 防滥用护栏。
const (
	maxPorts = 15
	maxCIDRs = 16
)

// normalizePorts 去重排序端口(1..65535 之外的丢弃;空切片原样返回)。
func normalizePorts(in []int) []int {
	seen := map[int]bool{}
	out := []int{}
	for _, p := range in {
		if p >= 1 && p <= 65535 && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Ints(out)
	return out
}

// normalizeCIDRs 逐条 ParseCIDR 并归一(返回 canonical cidr 列表,去重排序)。
func normalizeCIDRs(in []string) ([]string, error) {
	if len(in) > maxCIDRs {
		return nil, ErrInvalidCIDR
	}
	out := []string{}
	for _, raw := range in {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		_, n, err := net.ParseCIDR(raw)
		if err != nil {
			return nil, fmt.Errorf("%w:%s", ErrInvalidCIDR, raw)
		}
		out = append(out, n.String())
	}
	out = dedupeStrings(out)
	sort.Strings(out)
	return out, nil
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// joinPorts 把端口列表拼成 multiport 参数(如 "80,443,5000,5001,8080")。
func joinPorts(ports []int) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ",")
}

// truncate 人话截断(与 platformhttps 同款护栏)。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
