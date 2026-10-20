package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/firewall"
)

// 集群防火墙访问限制写操作审计 action / target。confirm 的审计 IP 即真实来源
// (clientIP 解析,经反代时不被 127.0.0.1 假象蒙蔽)。
const (
	auditActionFirewallSave     = "firewall.settings.save"
	auditActionFirewallApply    = "firewall.apply"
	auditActionFirewallConfirm  = "firewall.confirm"
	auditActionFirewallRollback = "firewall.rollback"
	auditActionFirewallDisable  = "firewall.disable"
	auditTargetFirewall         = "firewall_guard"
)

// firewallSnapshotDTO 是一份规则快照(pending/confirmed)的对外形态。
type firewallSnapshotDTO struct {
	Ports     []int    `json:"ports"`
	CIDRs     []string `json:"cidrs"`
	AppliedAt string   `json:"appliedAt"` // pending 的应用时刻(confirmed 为空)
}

// firewallDetectDTO 是本机执行前提探测的对外形态。
type firewallDetectDTO struct {
	Supported   bool     `json:"supported"`
	Reason      string   `json:"reason"`
	Linux       bool     `json:"linux"`
	InContainer bool     `json:"inContainer"`
	Iptables    bool     `json:"iptables"`
	Ip6tables   bool     `json:"ip6tables"`
	DockerUser  bool     `json:"dockerUser"`
	IsRoot      bool     `json:"isRoot"`
	SudoOk      bool     `json:"sudoOk"`
	Notes       []string `json:"notes"`
}

// firewallStatusDTO 是 GET /api/firewall/status 的响应体(冻结契约)。
type firewallStatusDTO struct {
	Ports      []int               `json:"ports"`      // 草稿端口
	ExtraCIDRs []string            `json:"extraCidrs"` // 草稿补充网段
	Phase      string              `json:"phase"`      // off | pending | active
	Pending    firewallSnapshotDTO `json:"pending"`
	Confirmed  firewallSnapshotDTO `json:"confirmed"`

	LastResult    string `json:"lastResult"`   // '' | ok | failed | reverted
	StatusDetail  string `json:"statusDetail"` // 人话(应用降级 note / 失败原因)
	LastAppliedAt string `json:"lastAppliedAt"`

	// PendingExpiresInSec 是临时生效的剩余确认秒数(非 pending 为 0;UI 倒计时用)。
	PendingExpiresInSec int `json:"pendingExpiresInSec"`

	Detect firewallDetectDTO `json:"detect"`
	// SourceIP 是本次请求的真实来源 IP(经反代时已按信任模型解析;UI 展示 + 确认门预判)。
	SourceIP string `json:"sourceIp"`
	// ConfirmAllowed 报告该真实来源当前能否通过确认门(仅 pending 有意义)。
	ConfirmAllowed bool `json:"confirmAllowed"`
}

// toFirewallStatusDTO 组装响应体。fw 为 nil 不可达(路由已挡);d 为探测快照;realIP 为
// 本次请求的真实来源。
func toFirewallStatusDTO(st *firewall.State, d *firewall.DetectInfo, realIP string, allowed bool) firewallStatusDTO {
	t := func(v time.Time) string {
		if v.IsZero() {
			return ""
		}
		return v.UTC().Format(time.RFC3339)
	}
	expires := 0
	if st.Phase == firewall.PhasePending && !st.PendingAppliedAt.IsZero() {
		if left := int((firewall.PendingTTL() - time.Since(st.PendingAppliedAt)).Seconds()); left > 0 {
			expires = left
		}
	}
	return firewallStatusDTO{
		Ports: st.Ports, ExtraCIDRs: st.ExtraCIDRs, Phase: st.Phase,
		Pending:    firewallSnapshotDTO{Ports: st.PendingPorts, CIDRs: st.PendingCIDRs, AppliedAt: t(st.PendingAppliedAt)},
		Confirmed:  firewallSnapshotDTO{Ports: st.ConfirmedPorts, CIDRs: st.ConfirmedCIDRs},
		LastResult: st.LastResult, StatusDetail: st.StatusDetail, LastAppliedAt: t(st.LastAppliedAt),
		PendingExpiresInSec: expires,
		Detect: firewallDetectDTO{
			Supported: d.Supported(), Reason: d.Reason(),
			Linux: d.Linux, InContainer: d.InContainer,
			Iptables: d.Iptables != "", Ip6tables: d.Ip6tables != "", DockerUser: d.DockerUser,
			IsRoot: d.IsRoot, SudoOk: d.SudoOk, Notes: d.Notes,
		},
		SourceIP: realIP, ConfirmAllowed: allowed,
	}
}

// writeFirewallError 把领域错误映射为契约错误码/状态码(照 writePlatformHTTPSError 风格)。
func writeFirewallError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, firewall.ErrNotLinux):
		writeError(w, http.StatusBadRequest, "firewall_not_linux", "防火墙限制仅支持 Linux 宿主机")
	case errors.Is(err, firewall.ErrInContainer):
		writeError(w, http.StatusBadRequest, "firewall_in_container", "容器部署形态下无法管理宿主防火墙:请在宿主机上直接配置,或改用宿主网络形态部署")
	case errors.Is(err, firewall.ErrNoIptables):
		writeError(w, http.StatusBadRequest, "firewall_no_iptables", "本机未找到 iptables(请安装 iptables,或确认 /usr/sbin 在可执行路径)")
	case errors.Is(err, firewall.ErrNoPrivilege):
		writeError(w, http.StatusBadRequest, "firewall_no_privilege", "平台运行用户非 root 且免密 sudo 不可用:请以 root 运行平台,或为该用户配置免密 sudo")
	case errors.Is(err, firewall.ErrMeshUnreachable):
		writeError(w, http.StatusBadRequest, "firewall_mesh_unreachable", "无法推导 mesh 网段(集群未启用或未连接)且未配置补充网段:请先连上集群,或填写补充网段")
	case errors.Is(err, firewall.ErrInvalidPorts):
		writeError(w, http.StatusBadRequest, "firewall_invalid_ports", "端口列表非法:应为 1-65535 且最多 15 个")
	case errors.Is(err, firewall.ErrInvalidCIDR):
		writeError(w, http.StatusBadRequest, "firewall_invalid_cidr", "补充网段非法:应为 CIDR 形式(如 192.168.30.0/24)")
	case errors.Is(err, firewall.ErrNotPending):
		writeError(w, http.StatusConflict, "firewall_not_pending", "仅临时生效中的规则可确认固化/回退")
	case errors.Is(err, firewall.ErrIPNotAllowed):
		writeError(w, http.StatusForbidden, "firewall_ip_not_allowed", "请从本机或 mesh 网络访问平台后再确认固化(当前来源不在放行集)")
	case errors.Is(err, firewall.ErrApply):
		writeError(w, http.StatusBadGateway, "firewall_apply_failed", "应用/清理防火墙规则失败(详情见错误信息与设置页状态)")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// makeGetFirewallStatusHandler 返回 GET /api/firewall/status(单行状态 + 探测 + 确认门预判)。
func makeGetFirewallStatusHandler(fw firewall.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fw == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "防火墙限制未初始化")
			return
		}
		st, err := fw.Get(r.Context())
		if err != nil {
			writeFirewallError(w, err)
			return
		}
		ip := clientIP(r)
		allowed, err := fw.AllowsConfirm(r.Context(), ip)
		if err != nil {
			writeFirewallError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, toFirewallStatusDTO(st, fw.Detect(r.Context()), ip, allowed))
	}
}

// makeUpdateFirewallSettingsHandler 返回 PUT /api/firewall/settings(保存草稿,不碰机器)。
func makeUpdateFirewallSettingsHandler(fw firewall.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		Ports      *[]int    `json:"ports"`
		ExtraCIDRs *[]string `json:"extraCidrs"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if fw == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "防火墙限制未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		st, err := fw.SaveDraft(r.Context(), firewall.DraftInput{Ports: req.Ports, ExtraCIDRs: req.ExtraCIDRs})
		if err != nil {
			recordAudit(r.Context(), aud, audit.Entry{
				Actor: auditActor, Action: auditActionFirewallSave, TargetType: auditTargetFirewall,
				TargetID: "-", Detail: map[string]any{"ok": false}, IP: clientIP(r),
			})
			writeFirewallError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionFirewallSave, TargetType: auditTargetFirewall,
			TargetID: "-", Detail: map[string]any{"ok": true, "ports": st.Ports, "extraCidrs": st.ExtraCIDRs}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ports": st.Ports, "extraCidrs": st.ExtraCIDRs})
	}
}

// makeApplyFirewallHandler 返回 POST /api/firewall/apply(草稿 → 运行时规则,临时生效)。
func makeApplyFirewallHandler(fw firewall.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fw == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "防火墙限制未初始化")
			return
		}
		st, err := fw.Apply(r.Context())
		if err != nil {
			recordAudit(r.Context(), aud, audit.Entry{
				Actor: auditActor, Action: auditActionFirewallApply, TargetType: auditTargetFirewall,
				TargetID: "-", Detail: map[string]any{"ok": false}, IP: clientIP(r),
			})
			writeFirewallError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionFirewallApply, TargetType: auditTargetFirewall,
			TargetID: "-", Detail: map[string]any{"ok": true, "ports": st.PendingPorts, "cidrs": st.PendingCIDRs}, IP: clientIP(r),
		})
		ip := clientIP(r)
		allowed, _ := fw.AllowsConfirm(r.Context(), ip)
		writeJSON(w, http.StatusOK, toFirewallStatusDTO(st, fw.Detect(r.Context()), ip, allowed))
	}
}

// makeConfirmFirewallHandler 返回 POST /api/firewall/confirm(固化)。真实来源 IP 经
// clientIP 信任模型解析(直连回环且无转发头 → 本机;经本机 nginx → X-Real-IP 的外部
// 真实 IP;mesh 对端直连 → 虚拟 IP),域内按 pending 快照做成员判定。
func makeConfirmFirewallHandler(fw firewall.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fw == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "防火墙限制未初始化")
			return
		}
		ip := clientIP(r)
		st, err := fw.Confirm(r.Context(), ip)
		if err != nil {
			recordAudit(r.Context(), aud, audit.Entry{
				Actor: auditActor, Action: auditActionFirewallConfirm, TargetType: auditTargetFirewall,
				TargetID: "-", Detail: map[string]any{"ok": false, "sourceIp": ip}, IP: ip,
			})
			writeFirewallError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionFirewallConfirm, TargetType: auditTargetFirewall,
			TargetID: "-", Detail: map[string]any{"ok": true, "sourceIp": ip, "ports": st.ConfirmedPorts, "cidrs": st.ConfirmedCIDRs}, IP: ip,
		})
		writeJSON(w, http.StatusOK, toFirewallStatusDTO(st, fw.Detect(r.Context()), ip, false))
	}
}

// makeRollbackFirewallHandler 返回 POST /api/firewall/rollback(手动回退临时规则;
// 逃生通道,不设来源门)。
func makeRollbackFirewallHandler(fw firewall.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fw == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "防火墙限制未初始化")
			return
		}
		st, err := fw.Rollback(r.Context())
		if err != nil {
			recordAudit(r.Context(), aud, audit.Entry{
				Actor: auditActor, Action: auditActionFirewallRollback, TargetType: auditTargetFirewall,
				TargetID: "-", Detail: map[string]any{"ok": false}, IP: clientIP(r),
			})
			writeFirewallError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionFirewallRollback, TargetType: auditTargetFirewall,
			TargetID: "-", Detail: map[string]any{"ok": true, "phase": st.Phase}, IP: clientIP(r),
		})
		ip := clientIP(r)
		allowed, _ := fw.AllowsConfirm(r.Context(), ip)
		writeJSON(w, http.StatusOK, toFirewallStatusDTO(st, fw.Detect(r.Context()), ip, allowed))
	}
}

// makeDisableFirewallHandler 返回 POST /api/firewall/disable(拆除全部规则并复位未开启;
// 逃生通道,不设来源门)。
func makeDisableFirewallHandler(fw firewall.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fw == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "防火墙限制未初始化")
			return
		}
		st, err := fw.Disable(r.Context())
		if err != nil {
			recordAudit(r.Context(), aud, audit.Entry{
				Actor: auditActor, Action: auditActionFirewallDisable, TargetType: auditTargetFirewall,
				TargetID: "-", Detail: map[string]any{"ok": false}, IP: clientIP(r),
			})
			writeFirewallError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionFirewallDisable, TargetType: auditTargetFirewall,
			TargetID: "-", Detail: map[string]any{"ok": true}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, toFirewallStatusDTO(st, fw.Detect(r.Context()), clientIP(r), false))
	}
}
