package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/sshtunnel"
	"github.com/huangchengsir/pipewright/internal/target"
)

// tunnelDTO 是隧道对外响应体(冻结契约;camelCase;配置 + 运行态合并;无凭据明文)。
type tunnelDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// 连接方式:serverId 非空 = 绑定已登记服务器(沿用其凭据/跳板链);
	// 空 = 直连模式(host/user/credentialId 字段生效)。
	ServerID        string `json:"serverId"`
	ServerName      string `json:"serverName"`
	Host            string `json:"host"`
	Port            int    `json:"port"`
	User            string `json:"user"`
	CredentialID    string `json:"credentialId"`
	CredentialName  string `json:"credentialName"`
	Mode            string `json:"mode"` // local | remote | dynamic
	BindHost        string `json:"bindHost"`
	BindPort        int    `json:"bindPort"`
	DestHost        string `json:"destHost"`
	DestPort        int    `json:"destPort"`
	Enabled         bool   `json:"enabled"`
	CreatedAt       string `json:"createdAt"`
	UpdatedAt       string `json:"updatedAt"`
	Status          string `json:"status"` // disabled|stopped|connecting|active|retrying
	PID             int    `json:"pid"`
	LastError       string `json:"lastError"`
	Restarts        int    `json:"restarts"`
	ActualPort      int    `json:"actualPort"`
	Endpoint        string `json:"endpoint"`
	StartedAt       string `json:"startedAt"`
}

func toTunnelDTO(v sshtunnel.TunnelView) tunnelDTO {
	startedAt := ""
	if !v.StartedAt.IsZero() {
		startedAt = v.StartedAt.UTC().Format(time.RFC3339)
	}
	return tunnelDTO{
		ID: v.ID, Name: v.Name,
		ServerID: v.ServerID, ServerName: v.ServerName,
		Host: v.Host, Port: v.Port, User: v.User,
		CredentialID: v.CredentialID, CredentialName: v.CredentialName,
		Mode: v.Mode, BindHost: v.BindHost, BindPort: v.BindPort,
		DestHost: v.DestHost, DestPort: v.DestPort,
		Enabled:   v.Enabled,
		CreatedAt: v.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: v.UpdatedAt.UTC().Format(time.RFC3339),
		Status: v.Status, PID: v.PID, LastError: v.LastError,
		Restarts: v.Restarts, ActualPort: v.ActualPort, Endpoint: v.Endpoint,
		StartedAt: startedAt,
	}
}

// tunnelInputDTO 是创建/更新请求体(全量替换;无凭据明文,只持引用)。
type tunnelInputDTO struct {
	Name         string `json:"name"`
	ServerID     string `json:"serverId"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	User         string `json:"user"`
	CredentialID string `json:"credentialId"`
	Mode         string `json:"mode"`
	BindHost     string `json:"bindHost"`
	BindPort     int    `json:"bindPort"`
	DestHost     string `json:"destHost"`
	DestPort     int    `json:"destPort"`
	Enabled      bool   `json:"enabled"`
}

func toTunnelInput(req tunnelInputDTO) sshtunnel.Input {
	return sshtunnel.Input{
		Name: req.Name, ServerID: req.ServerID,
		Host: req.Host, Port: req.Port, User: req.User, CredentialID: req.CredentialID,
		Mode: req.Mode, BindHost: req.BindHost, BindPort: req.BindPort,
		DestHost: req.DestHost, DestPort: req.DestPort, Enabled: req.Enabled,
	}
}

// writeTunnelError 把领域错误映射为契约错误码/状态码;绝不回显明文/私钥/口令/栈。
func writeTunnelError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sshtunnel.ErrUninitialized):
		writeError(w, http.StatusServiceUnavailable, "internal", "隧道服务未初始化")
	case errors.Is(err, sshtunnel.ErrNotFound):
		writeError(w, http.StatusNotFound, "tunnel_not_found", "隧道不存在")
	case errors.Is(err, sshtunnel.ErrBindConflict):
		writeError(w, http.StatusConflict, "bind_conflict", "监听地址冲突:另一条已启用的本机隧道占用了相同的 host:port")
	case errors.Is(err, target.ErrNotFound):
		writeError(w, http.StatusUnprocessableEntity, "invalid_tunnel", "绑定的服务器不存在或已删除")
	case errors.Is(err, target.ErrCredentialNotFound):
		writeError(w, http.StatusUnprocessableEntity, "credential_error", "引用的 SSH 凭据不存在")
	case errors.Is(err, target.ErrCredentialTypeMismatch):
		writeError(w, http.StatusUnprocessableEntity, "credential_error", "凭据类型须为「SSH 密钥」或「SSH 密码」")
	case errors.Is(err, target.ErrVaultUnconfigured):
		writeError(w, http.StatusServiceUnavailable, "vault_unconfigured", "保险库未配置 master key,无法取 SSH 凭据")
	case errors.Is(err, sshtunnel.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_tunnel", "隧道配置非法:请检查名称、模式、连接方式(绑定服务器或直连主机二选一)、监听与目标端口")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// auditTunnelOp 写隧道操作审计(detail 只记动作与隧道名,不记配置细节)。
func auditTunnelOp(r *http.Request, aud audit.Recorder, action, id, name string, ok bool) {
	recordAudit(r.Context(), aud, audit.Entry{
		Actor:      auditActor,
		Action:     action,
		TargetType: audit.TargetTunnel,
		TargetID:   id,
		Detail:     map[string]any{"action": action, "name": name, "ok": ok},
		IP:         clientIP(r),
	})
}

// makeListTunnelsHandler 返回 GET /api/tunnels handler → { items: [...] }。
func makeListTunnelsHandler(svc *sshtunnel.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "隧道服务未初始化")
			return
		}
		tunnels, err := svc.List(r.Context())
		if err != nil {
			writeTunnelError(w, err)
			return
		}
		out := make([]tunnelDTO, 0, len(tunnels))
		for _, t := range tunnels {
			out = append(out, toTunnelDTO(t))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

// decodeTunnelInput 解码并校验请求体基础形态(领域校验在 Service 内)。
func decodeTunnelInput(w http.ResponseWriter, r *http.Request) (tunnelInputDTO, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	var req tunnelInputDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
		return req, false
	}
	return req, true
}

// makeCreateTunnelHandler 返回 POST /api/tunnels handler(写:auth + CSRF + 审计)。
func makeCreateTunnelHandler(svc *sshtunnel.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "隧道服务未初始化")
			return
		}
		req, ok := decodeTunnelInput(w, r)
		if !ok {
			return
		}
		t, err := svc.Create(r.Context(), toTunnelInput(req))
		if err != nil {
			writeTunnelError(w, err)
			return
		}
		auditTunnelOp(r, aud, audit.ActionTunnelCreate, t.ID, t.Name, true)
		writeJSON(w, http.StatusCreated, toTunnelDTO(*t))
	}
}

// makeUpdateTunnelHandler 返回 PUT /api/tunnels/{id} handler(写:auth + CSRF + 审计;
// 运行中且配置变化自动重启 helper)。
func makeUpdateTunnelHandler(svc *sshtunnel.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "隧道服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		req, ok := decodeTunnelInput(w, r)
		if !ok {
			return
		}
		t, err := svc.Update(r.Context(), id, toTunnelInput(req))
		if err != nil {
			writeTunnelError(w, err)
			return
		}
		auditTunnelOp(r, aud, audit.ActionTunnelUpdate, id, t.Name, true)
		writeJSON(w, http.StatusOK, toTunnelDTO(*t))
	}
}

// makeDeleteTunnelHandler 返回 DELETE /api/tunnels/{id} handler(写:auth + CSRF + 审计;
// 先停 helper 再删配置与运行时文件)。
func makeDeleteTunnelHandler(svc *sshtunnel.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "隧道服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		tunnels, err := svc.List(r.Context())
		if err != nil {
			writeTunnelError(w, err)
			return
		}
		name := id
		for _, t := range tunnels {
			if t.ID == id {
				name = t.Name
				break
			}
		}
		if err := svc.Delete(r.Context(), id); err != nil {
			writeTunnelError(w, err)
			return
		}
		auditTunnelOp(r, aud, audit.ActionTunnelDelete, id, name, true)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// makeTunnelStartHandler 返回 POST /api/tunnels/{id}/start handler(写:auth + CSRF + 审计)。
func makeTunnelStartHandler(svc *sshtunnel.Service, aud audit.Recorder) http.HandlerFunc {
	return tunnelActionHandler(svc, aud, "start", func(s *sshtunnel.Service, ctx context.Context, id string) (*sshtunnel.TunnelView, error) {
		return s.Start(ctx, id)
	})
}

// makeTunnelStopHandler 返回 POST /api/tunnels/{id}/stop handler(写:auth + CSRF + 审计)。
func makeTunnelStopHandler(svc *sshtunnel.Service, aud audit.Recorder) http.HandlerFunc {
	return tunnelActionHandler(svc, aud, "stop", func(s *sshtunnel.Service, ctx context.Context, id string) (*sshtunnel.TunnelView, error) {
		return s.Stop(ctx, id)
	})
}

// makeTunnelRestartHandler 返回 POST /api/tunnels/{id}/restart handler(写:auth + CSRF + 审计)。
func makeTunnelRestartHandler(svc *sshtunnel.Service, aud audit.Recorder) http.HandlerFunc {
	return tunnelActionHandler(svc, aud, "restart", func(s *sshtunnel.Service, ctx context.Context, id string) (*sshtunnel.TunnelView, error) {
		return s.Restart(ctx, id)
	})
}

// tunnelActionHandler 抽出 start/stop/restart 共用骨架:执行 → 审计 → 200 视图。
func tunnelActionHandler(svc *sshtunnel.Service, aud audit.Recorder, action string,
	run func(*sshtunnel.Service, context.Context, string) (*sshtunnel.TunnelView, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "隧道服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		t, err := run(svc, r.Context(), id)
		if err != nil {
			writeTunnelError(w, err)
			return
		}
		auditTunnelOp(r, aud, audit.ActionTunnelOp, id, t.Name, true)
		writeJSON(w, http.StatusOK, toTunnelDTO(*t))
	}
}

// makeTunnelTestHandler 返回 POST /api/tunnels/{id}/test handler(一次性连通性验证,
// 不驻留进程;写方法过 auth + CSRF)。
func makeTunnelTestHandler(svc *sshtunnel.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "隧道服务未初始化")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		res, err := svc.Test(ctx, chi.URLParam(r, "id"))
		if err != nil {
			writeTunnelError(w, err)
			return
		}
		out := struct {
			OK        bool    `json:"ok"`
			LatencyMs int64   `json:"latencyMs"`
			Error     *string `json:"error"`
		}{OK: res.OK, LatencyMs: res.LatencyMs}
		if !res.OK && res.Err != "" {
			out.Error = &res.Err
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// makeTunnelLogsHandler 返回 GET /api/tunnels/{id}/logs handler(只读:过 auth;
// helper 日志尾部,默认 200 行、上限 1000)。
func makeTunnelLogsHandler(svc *sshtunnel.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "隧道服务未初始化")
			return
		}
		lines := 200
		if v := r.URL.Query().Get("lines"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				lines = n
			}
		}
		logs, err := svc.Logs(chi.URLParam(r, "id"), lines)
		if err != nil {
			writeTunnelError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"lines": logs})
	}
}
