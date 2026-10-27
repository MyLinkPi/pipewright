// modeswitch.go 是 Secure Mode 全集群统一开关:
//   - 存储侧(*service 落地 mesh.Service 新增方法):mesh_mode_pending 单行暂存 +
//     ApplyDueModeChange 到期翻转(对账节拍调用,翻转经 supervisor 指纹触发 easytier
//     重启,本文件不碰进程);
//   - 协调侧(ModeSwitcher,发起节点用):两阶段 + 定时齐切 —— fan-out 下发指令 →
//     全员 ack(armed)→ 各节点本地在 effective_at 自翻转,不再依赖通信(时钟偏差者
//     晚切,自愈)。任一节点未 ack → aborted:向已 ack 节点清除指令并报失败名单。
//
// 撤销(abort)是尽力而为:effective_at 前可撤;已 ack 但错过撤销消息的节点仍会在
// 到期时应用(Status 聚合会暴露这种漂移,补救 = 发起反向切换或单节点恢复舱口)。
//
// 无虚拟 IP 的对端不经 mesh 寻址(既不收指令也不可查询),无法纳入齐切:这类节点
// 在 Initiate 结果与 Status 中显式列为 skipped —— 静默跳过会形成无人知晓的混跑。
package mesh

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/huangchengsir/pipewright/internal/store"
)

// 切换动作枚举。
const (
	ModeActionEnable  = "enable"
	ModeActionDisable = "disable"
)

// 统一开关的护栏:生效前置 1~60 分钟(默认 2;下限保证 fan-out+人工反悔窗口,
// 上限防手滑把生效定到几小时后无人值守),fan-out 收齐应答的整体超时。
const (
	switchLeadMin     = 1
	switchLeadMax     = 60
	switchLeadDefault = 2
	switchFanoutWait  = 15 * time.Second
)

// ModePending 是本地暂存的一条切换指令(mesh_mode_pending 单行,最新覆盖)。
type ModePending struct {
	ChangeID    string    `json:"changeId"`
	Action      string    `json:"action"` // enable | disable
	EffectiveAt time.Time `json:"effectiveAt"`
	InitiatedBy string    `json:"initiatedBy"` // 发起节点 id
	AckedAt     time.Time `json:"ackedAt"`
	AppliedAt   time.Time `json:"appliedAt"` // 空 = 未应用
}

// ModeChangeMsg 是节点间下发的切换指令(入站 POST /api/mesh/mode-change 的载荷)。
type ModeChangeMsg struct {
	ChangeID    string `json:"changeId"`
	Action      string `json:"action"`
	EffectiveAt string `json:"effectiveAt"` // RFC3339(以发起节点时钟为准)
	InitiatedBy string `json:"initiatedBy"`
}

// ModeNodeState 是状态聚合的单节点行(本端 + 各对端)。
type ModeNodeState struct {
	NodeID     string `json:"nodeId"`
	Name       string `json:"name"`
	Online     bool   `json:"online"`     // 本轮查询可达(fan-out GET 成功)
	SecureMode bool   `json:"secureMode"` // 对端本地 secure_mode 当前值
	ChangeID   string `json:"changeId"`   // 对端暂存的指令(空 = 无)
	Action     string `json:"action"`
	Acked      bool   `json:"acked"`
	Applied    bool   `json:"applied"`
	IsSelf     bool   `json:"isSelf"`
}

// SwitchStatus 是统一开关的集群级视图(设置页轮询)。
type SwitchStatus struct {
	SecureMode bool            `json:"secureMode"` // 本端当前态
	Pending    *ModePending    `json:"pending"`    // 本端暂存指令(无 = null)
	Nodes      []ModeNodeState `json:"nodes"`
	// Skipped 是无虚拟 IP、无法纳入齐切的对端(不参与 ack 判定;列出防静默混跑)
	Skipped []string `json:"skipped"`
}

// SwitchResult 是 Initiate 的结果(armed = 已下发且全员确认;aborted = 已回滚)。
type SwitchResult struct {
	ChangeID    string
	Action      string
	EffectiveAt time.Time
	Armed       bool
	FailedNodes []string // 未确认节点名(aborted 的原因)
	// SkippedNodes 是无虚拟 IP、无法纳入本次齐切的对端名(它们保持旧态)。
	SkippedNodes []string
}

// ---- 存储侧 ----

func formatTS(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func parseTS(raw string) time.Time {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}
	}
	return t
}

func (s *service) GetModePending(ctx context.Context) (*ModePending, error) {
	var p ModePending
	var effAt, ackAt, appliedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT change_id, action, effective_at, initiated_by, acked_at, applied_at
		 FROM mesh_mode_pending WHERE id = 1`).
		Scan(&p.ChangeID, &p.Action, &effAt, &p.InitiatedBy, &ackAt, &appliedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mesh: load mode pending: %w", err)
	}
	p.EffectiveAt = parseTS(effAt)
	p.AckedAt = parseTS(ackAt)
	p.AppliedAt = parseTS(appliedAt)
	return &p, nil
}

func (s *service) SaveModePending(ctx context.Context, p ModePending) error {
	if p.ChangeID == "" || (p.Action != ModeActionEnable && p.Action != ModeActionDisable) {
		return fmt.Errorf("%w: 切换指令需 changeId 与合法 action", ErrInvalidInput)
	}
	if p.EffectiveAt.IsZero() {
		return fmt.Errorf("%w: 切换指令缺 effectiveAt", ErrInvalidInput)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO mesh_mode_pending (id, change_id, action, effective_at, initiated_by, acked_at, applied_at, aborted_at)
		 VALUES (1, ?, ?, ?, ?, ?, ?, '')
		 `+storeUpsertModePending(s.dialect),
		p.ChangeID, p.Action, formatTS(p.EffectiveAt), p.InitiatedBy, formatTS(p.AckedAt), formatTS(p.AppliedAt))
	if err != nil {
		return fmt.Errorf("mesh: save mode pending: %w", err)
	}
	return nil
}

// storeUpsertModePending 生成单行覆盖的 upsert 尾句(方言差异由 store 生成)。
func storeUpsertModePending(d store.Dialect) string {
	cols := []string{"change_id", "action", "effective_at", "initiated_by", "acked_at", "applied_at"}
	assigns := make([]string, 0, len(cols)+1)
	for _, c := range cols {
		assigns = append(assigns, c+" = "+store.Excluded(d, c))
	}
	assigns = append(assigns, "aborted_at = ''")
	return store.UpsertAssignSuffix(d, []string{"id"}, assigns)
}

func (s *service) ClearModePending(ctx context.Context, changeID string) error {
	// 仅当 change_id 匹配且未应用(防误清后到的 新指令/已应用历史)。
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM mesh_mode_pending WHERE id = 1 AND change_id = ? AND applied_at = ''`, strings.TrimSpace(changeID))
	if err != nil {
		return fmt.Errorf("mesh: clear mode pending: %w", err)
	}
	return nil
}

// ApplyDueModeChange 应用到期指令:翻转 secure_mode(与 supervisor 指纹联动触发
// easytier 重启)并标记 applied。不到期/无指令/已应用 → false。
func (s *service) ApplyDueModeChange(ctx context.Context, now time.Time) (bool, error) {
	p, err := s.GetModePending(ctx)
	if err != nil {
		return false, err
	}
	if p == nil || !p.AppliedAt.IsZero() || now.Before(p.EffectiveAt) {
		return false, nil
	}
	target := 0
	if p.Action == ModeActionEnable {
		target = 1
	}
	nowStr := formatTS(now)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE mesh_settings SET secure_mode = ?, updated_at = ? WHERE id = 1`, target, nowStr); err != nil {
		return false, fmt.Errorf("mesh: apply mode change: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE mesh_mode_pending SET applied_at = ? WHERE id = 1 AND change_id = ?`, nowStr, p.ChangeID); err != nil {
		return false, fmt.Errorf("mesh: mark mode change applied: %w", err)
	}
	return true, nil
}

// ---- 协调侧(发起节点) ----

// ModeSwitcher 协调全集群统一开关(fan-out 下发/撤销/状态聚合)。
type ModeSwitcher struct {
	svc    Service
	client *Client
}

// NewModeSwitcher 构造协调器(client 为节点间签名客户端,可为 nil —— 单节点集群
// 只剩本端自翻转路径,仍可用)。
func NewModeSwitcher(svc Service, client *Client) *ModeSwitcher {
	return &ModeSwitcher{svc: svc, client: client}
}

// Initiate 发起一次全集群切换:前置校验(集群启用/托管模式/非目标态)→ 本端落指令
// → fan-out 下发 → 全员 ack = armed;任一失败 = aborted(回滚已 ack 节点与本端)。
func (m *ModeSwitcher) Initiate(ctx context.Context, action string, effectiveInMinutes int) (*SwitchResult, error) {
	if action != ModeActionEnable && action != ModeActionDisable {
		return nil, fmt.Errorf("%w: action 须为 enable|disable", ErrInvalidInput)
	}
	if effectiveInMinutes <= 0 {
		effectiveInMinutes = switchLeadDefault
	}
	if effectiveInMinutes < switchLeadMin || effectiveInMinutes > switchLeadMax {
		return nil, fmt.Errorf("%w: 生效前置须在 %d~%d 分钟", ErrInvalidInput, switchLeadMin, switchLeadMax)
	}
	st, err := m.svc.Settings(ctx)
	if err != nil {
		return nil, err
	}
	if !st.Enabled || st.EasyTierMode != ModeManaged {
		return nil, fmt.Errorf("%w: 集群未启用或非托管模式,无法统一切换", ErrInvalidInput)
	}
	if want := action == ModeActionEnable; want == st.SecureMode {
		return nil, fmt.Errorf("%w: 本节点已是目标态(%s)", ErrInvalidInput, action)
	}
	// 已有 armed 指令未落地时拒绝叠加(防两条指令竞争)。读失败也拒绝 —— 此时
	// 无法确认无进行中指令,放行会在窄窗口下静默覆盖旧指令(旧指令已 fan-out 到
	// 对端,对端到期自翻转 → 集群撕裂)。
	pending, perr := m.svc.GetModePending(ctx)
	if perr != nil {
		return nil, fmt.Errorf("mesh: 查询进行中切换指令失败: %w", perr)
	}
	if pending != nil && pending.AppliedAt.IsZero() {
		return nil, fmt.Errorf("%w: 已有进行中的切换指令 %s(生效 %s),先等它落地或撤销",
			ErrInvalidInput, pending.ChangeID, formatTS(pending.EffectiveAt))
	}

	peers, skipped := m.switchPeers(ctx, st.NodeID)
	changeID := uuid.NewString()
	effectiveAt := time.Now().UTC().Add(time.Duration(effectiveInMinutes) * time.Minute)
	msg := ModeChangeMsg{ChangeID: changeID, Action: action, EffectiveAt: formatTS(effectiveAt), InitiatedBy: st.NodeID}

	// 本端先落指令并视为已 ack(本端也是集群一员,到期同样走本地自翻转)。
	if err := m.svc.SaveModePending(ctx, ModePending{
		ChangeID: changeID, Action: action, EffectiveAt: effectiveAt,
		InitiatedBy: st.NodeID, AckedAt: time.Now().UTC(),
	}); err != nil {
		return nil, err
	}

	res := &SwitchResult{ChangeID: changeID, Action: action, EffectiveAt: effectiveAt, Armed: true,
		SkippedNodes: peerLabels(skipped)}
	if len(peers) == 0 {
		return res, nil // 单节点集群:本端指令即全部。
	}
	acked, failed := m.fanOutChange(ctx, peers, msg)
	if len(failed) > 0 {
		res.Armed = false
		for _, p := range failed {
			res.FailedNodes = append(res.FailedNodes, peerLabel(p))
		}
		m.fanOutAbort(ctx, acked, changeID)
		_ = m.svc.ClearModePending(ctx, changeID)
		return res, nil
	}
	return res, nil
}

// Abort 撤销进行中的指令(生效时刻前;已应用/已过期拒绝)。
func (m *ModeSwitcher) Abort(ctx context.Context, changeID string) error {
	p, err := m.svc.GetModePending(ctx)
	if err != nil {
		return err
	}
	if p == nil || p.ChangeID != strings.TrimSpace(changeID) {
		return ErrNotFound
	}
	if !p.AppliedAt.IsZero() {
		return fmt.Errorf("%w: 指令已生效,请发起反向切换", ErrInvalidInput)
	}
	if time.Now().UTC().After(p.EffectiveAt) {
		return fmt.Errorf("%w: 已到生效时刻,撤销窗口已过", ErrInvalidInput)
	}
	if st, serr := m.svc.Settings(ctx); serr == nil {
		peers, _ := m.switchPeers(ctx, st.NodeID)
		m.fanOutAbort(ctx, peers, p.ChangeID)
	}
	return m.svc.ClearModePending(ctx, p.ChangeID)
}

// Status 聚合本端与各对端的开关态(设置页轮询;对端不可达标 Online=false)。
func (m *ModeSwitcher) Status(ctx context.Context) (*SwitchStatus, error) {
	st, err := m.svc.Settings(ctx)
	if err != nil {
		return nil, err
	}
	pending, err := m.svc.GetModePending(ctx)
	if err != nil {
		return nil, err
	}
	out := &SwitchStatus{SecureMode: st.SecureMode, Pending: pending, Nodes: []ModeNodeState{{
		NodeID: st.NodeID, Name: st.Name, Online: true, SecureMode: st.SecureMode,
		ChangeID: pendingID(pending), Action: pendingAction(pending),
		Acked: pending != nil, Applied: pending != nil && !pending.AppliedAt.IsZero(), IsSelf: true,
	}}}
	peers, skipped := m.switchPeers(ctx, st.NodeID)
	out.Skipped = peerLabels(skipped)
	if len(peers) == 0 || m.client == nil {
		return out, nil
	}
	fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	type reply struct {
		peer Peer
		ok   bool
		body ModeRemoteState
	}
	replies := make(chan reply, len(peers))
	for _, p := range peers {
		go func(p Peer) {
			rep := reply{peer: p}
			if err := m.client.Do(fctx, &p, http.MethodGet, "/api/mesh/mode-change", nil, &rep.body); err == nil {
				rep.ok = true
			}
			replies <- rep
		}(p)
	}
	for range peers {
		r := <-replies
		row := ModeNodeState{NodeID: r.peer.NodeID, Name: r.peer.Name, Online: r.ok, SecureMode: r.body.SecureMode}
		if r.body.Pending != nil {
			row.ChangeID = r.body.Pending.ChangeID
			row.Action = r.body.Pending.Action
			row.Acked = true
			row.Applied = r.body.Pending.Applied
		}
		out.Nodes = append(out.Nodes, row)
	}
	return out, nil
}

// ModeRemoteState 是对端 GET /api/mesh/mode-change 的响应(入站 handler 组装)。
type ModeRemoteState struct {
	NodeID     string          `json:"nodeId"`
	SecureMode bool            `json:"secureMode"`
	Pending    *ModeRemotePing `json:"pending"`
}

// ModeRemotePing 是对端暂存指令的对外形态(时刻转 ISO 串,布尔化 applied)。
type ModeRemotePing struct {
	ChangeID    string `json:"changeId"`
	Action      string `json:"action"`
	EffectiveAt string `json:"effectiveAt"`
	Applied     bool   `json:"applied"`
}

// switchPeers 取可下发的对端名单:有虚拟 IP(可经 mesh 寻址)为可达组,其余为
// skipped —— 无虚拟 IP 的对端既不能下发也不能查询,只能在结果/状态里显式暴露。
func (m *ModeSwitcher) switchPeers(ctx context.Context, selfID string) (reachable, skipped []Peer) {
	peers, err := m.svc.ListPeers(ctx)
	if err != nil {
		return nil, nil
	}
	for _, p := range peers {
		if p.NodeID == selfID {
			continue
		}
		if strings.TrimSpace(p.VirtualIP) == "" {
			skipped = append(skipped, p)
			continue
		}
		reachable = append(reachable, p)
	}
	return reachable, skipped
}

// fanOutChange 并发下发指令,收齐 acked/failed 两组(webhook_forward 的收齐模式)。
// client 为 nil(单节点装配)时无可达路径,全部按失败计 —— Initiate 随即 aborted 回滚,
// 与 fanOutAbort 的 nil 防护对齐(防 goroutine 内 nil 解引用 panic 且无 recover)。
func (m *ModeSwitcher) fanOutChange(ctx context.Context, peers []Peer, msg ModeChangeMsg) (acked, failed []Peer) {
	if m.client == nil {
		return nil, peers
	}
	fctx, cancel := context.WithTimeout(ctx, switchFanoutWait)
	defer cancel()
	type reply struct {
		peer Peer
		ok   bool
	}
	replies := make(chan reply, len(peers))
	for _, p := range peers {
		go func(p Peer) {
			var resp struct {
				NodeID string `json:"nodeId"`
			}
			err := m.client.Do(fctx, &p, http.MethodPost, "/api/mesh/mode-change", msg, &resp)
			replies <- reply{p, err == nil}
		}(p)
	}
	for range peers {
		r := <-replies
		if r.ok {
			acked = append(acked, r.peer)
		} else {
			failed = append(failed, r.peer)
		}
	}
	return acked, failed
}

// fanOutAbort 并发清除指令(尽力而为;错过清除的对端到期仍会应用,见文件头注释)。
func (m *ModeSwitcher) fanOutAbort(ctx context.Context, peers []Peer, changeID string) {
	if m.client == nil || len(peers) == 0 {
		return
	}
	fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, p := range peers {
		wg.Add(1)
		go func(p Peer) {
			defer wg.Done()
			_ = m.client.Do(fctx, &p, http.MethodDelete, "/api/mesh/mode-change/"+changeID, nil, nil)
		}(p)
	}
	wg.Wait()
}

func peerLabel(p Peer) string {
	if p.Name != "" {
		return p.Name
	}
	return p.NodeID
}

// peerLabels 批量取展示名(空输入返回 nil,JSON 序列化为 null)。
func peerLabels(peers []Peer) []string {
	if len(peers) == 0 {
		return nil
	}
	out := make([]string, 0, len(peers))
	for _, p := range peers {
		out = append(out, peerLabel(p))
	}
	return out
}

func pendingID(p *ModePending) string {
	if p == nil {
		return ""
	}
	return p.ChangeID
}

func pendingAction(p *ModePending) string {
	if p == nil {
		return ""
	}
	return p.Action
}
