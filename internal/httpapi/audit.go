package httpapi

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/audit"
)

// auditActor 返回当前请求的操作者标识。本平台为单管理员账户,认证写操作的操作者
// 即 "admin"(与 manual run handler 一致)。未来多用户时由 session 携带身份再细化。
const auditActor = "admin"

// clientIP 从请求提取来源 IP,用于审计记录(非安全判定)。
//
// **默认只用 RemoteAddr**:Pipewright 常作单二进制直连暴露,无反代。若无条件采信
// 转发头,任意客户端可发 `X-Forwarded-For: 1.2.3.4` 把伪造来源写进 append-only
// 的审计 ip 列,削弱 AC-SEC-03「不可篡改」的取证价值。两种情形才采信转发头:
//  1. 直连对端是回环地址 —— 平台 HTTPS 的本机 nginx 反代即此形态(nginx 用
//     $remote_addr 覆写 X-Real-IP,客户端伪造不进来);
//  2. 显式配置可信反代(PIPEWRIGHT_TRUST_PROXY=1/true)。
//
// 优先 X-Real-IP(反代覆写值,不可被客户端预置污染),其次 XFF 首段,兜底 RemoteAddr。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	if !trustForwardedHeader() && !isLoopbackHost(host) {
		return host
	}
	if xrip := strings.TrimSpace(r.Header.Get("X-Real-IP")); xrip != "" {
		return xrip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	return host
}

// isLoopbackHost 报告 host 是否回环地址(IPv4 127.0.0.0/8、IPv6 ::1,含 IPv4-mapped 形式)。
func isLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// trustForwardedHeader 报告是否显式配置了可信反代。
func trustForwardedHeader() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PIPEWRIGHT_TRUST_PROXY"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// recordAudit 在业务成功后追加一行审计;rec 为 nil 时静默跳过(不阻断业务)。
// 本地写入失败**不回滚业务**(审计不阻断),但必须**记日志**——否则敏感操作的审计缺口
// 完全静默,损害 AC-SEC-03 的「完整」保证。
func recordAudit(ctx context.Context, rec audit.Recorder, e audit.Entry) {
	if rec == nil {
		return
	}
	if err := rec.Record(ctx, e); err != nil {
		log.Printf("[audit] 警告:写审计失败(action=%s target=%s/%s): %v", e.Action, e.TargetType, e.TargetID, err)
	}
}

// auditEntryDTO 是审计条目对外响应体(冻结契约;camelCase;detail 已脱敏,绝无明文)。
type auditEntryDTO struct {
	ID         string         `json:"id"`
	Timestamp  string         `json:"timestamp"` // RFC3339
	Actor      string         `json:"actor"`
	Action     string         `json:"action"`
	TargetType string         `json:"targetType"`
	TargetID   string         `json:"targetId"`
	Detail     map[string]any `json:"detail"`
	IP         string         `json:"ip"`
}

// auditListDTO 是审计列表响应体(冻结契约:entries + nextBefore)。
type auditListDTO struct {
	Entries    []auditEntryDTO `json:"entries"`
	NextBefore *string         `json:"nextBefore"` // null 表示到底
}

// toAuditEntryDTO 把领域 Record 转为契约 DTO(detail 已在写入时脱敏)。
func toAuditEntryDTO(r audit.Record) auditEntryDTO {
	detail := r.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	return auditEntryDTO{
		ID:         r.ID,
		Timestamp:  r.Timestamp.UTC().Format(time.RFC3339),
		Actor:      r.Actor,
		Action:     r.Action,
		TargetType: r.TargetType,
		TargetID:   r.TargetID,
		Detail:     detail,
		IP:         r.IP,
	}
}

// makeListAuditHandler 返回 GET /api/audit handler(认证保护 + 分页 + 过滤;只读)。
// query:limit(默认 50,上限 200)· before(游标,上一页末条 id)· action · targetType。
func makeListAuditHandler(rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if rec == nil {
			writeError(w, http.StatusServiceUnavailable, "audit_unavailable", "审计服务未初始化")
			return
		}
		q := r.URL.Query()
		f := audit.ListFilter{
			Limit:      atoiDefault(q.Get("limit"), 0),
			Before:     strings.TrimSpace(q.Get("before")),
			Action:     strings.TrimSpace(q.Get("action")),
			TargetType: strings.TrimSpace(q.Get("targetType")),
		}
		res, err := rec.List(r.Context(), f)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		entries := make([]auditEntryDTO, 0, len(res.Entries))
		for _, e := range res.Entries {
			entries = append(entries, toAuditEntryDTO(e))
		}
		var nextBefore *string
		if res.NextBefore != "" {
			nb := res.NextBefore
			nextBefore = &nb
		}
		writeJSON(w, http.StatusOK, auditListDTO{Entries: entries, NextBefore: nextBefore})
	}
}
