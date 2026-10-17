package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/identity"
)

// userDTO 是普通用户对外响应体(绝不含口令哈希)。
type userDTO struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	Status    string `json:"status"`
	CreatedAt string `json:"createdAt"`
}

func toUserDTO(u identity.User) userDTO {
	return userDTO{
		ID:        u.ID,
		Username:  u.Username,
		Role:      u.Role,
		Status:    u.Status,
		CreatedAt: u.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// invitationDTO 是邀请对外响应体。Token 仅用于管理员拼注册链接。
type invitationDTO struct {
	ID        string `json:"id"`
	Token     string `json:"token"`
	CreatedBy string `json:"createdBy"`
	ExpiresAt string `json:"expiresAt"`
	UsedAt    string `json:"usedAt,omitempty"`
	State     string `json:"state"`
	CreatedAt string `json:"createdAt"`
}

func toInvitationDTO(inv identity.Invitation) invitationDTO {
	dto := invitationDTO{
		ID:        inv.ID,
		Token:     inv.Token,
		CreatedBy: inv.CreatedBy,
		ExpiresAt: inv.ExpiresAt.UTC().Format(time.RFC3339),
		State:     inv.State,
		CreatedAt: inv.CreatedAt.UTC().Format(time.RFC3339),
	}
	if inv.UsedAt != nil {
		dto.UsedAt = inv.UsedAt.UTC().Format(time.RFC3339)
	}
	return dto
}

// writeRegisterError 把注册领域错误映射为 HTTP 状态码 + 稳定错误码。
func writeRegisterError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, identity.ErrInvitationNotFound):
		writeError(w, http.StatusNotFound, "invitation_invalid", "邀请链接无效")
	case errors.Is(err, identity.ErrInvitationUsed):
		writeError(w, http.StatusConflict, "invitation_used", "邀请链接已被使用")
	case errors.Is(err, identity.ErrInvitationExpired):
		writeError(w, http.StatusGone, "invitation_expired", "邀请链接已过期")
	case errors.Is(err, identity.ErrUsernameTaken):
		writeError(w, http.StatusConflict, "username_taken", "用户名已被占用")
	case errors.Is(err, identity.ErrInvalidUsername):
		writeError(w, http.StatusUnprocessableEntity, "invalid_username", "用户名需 3-32 位字母/数字/下划线/点/连字符")
	case errors.Is(err, identity.ErrWeakPassword):
		writeError(w, http.StatusUnprocessableEntity, "weak_password", "口令至少 8 位")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// makeRegisterHandler 返回 POST /api/auth/register handler(公开;无需会话)。
// 用邀请 token 注册普通用户,成功后自动登录并下发会话 cookie。
func makeRegisterHandler(id identity.Service, authn auth.Authenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if id == nil || authn == nil {
			writeError(w, http.StatusServiceUnavailable, "no_auth", "认证服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			Token    string `json:"token"`
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}

		u, err := id.Register(r.Context(), req.Token, req.Username, req.Password)
		if err != nil {
			writeRegisterError(w, err)
			return
		}

		// 自动登录:注册成功即建立会话,避免用户再手动登录一次。
		sess, err := authn.Login(u.Username, req.Password, clientIP(r))
		if err != nil {
			// 注册已成功(账号已建),登录异常不阻断:前端据 username 引导去登录页。
			writeJSON(w, http.StatusCreated, map[string]string{"username": u.Username, "role": u.Role})
			return
		}
		setSessionCookies(w, r, sess)
		writeJSON(w, http.StatusCreated, map[string]string{"username": sess.Username, "role": sess.Role})
	}
}

// makeInvitationInfoHandler 返回 GET /api/auth/invitations/{token} handler(公开只读)。
// 供注册页展示邀请状态(pending/used/expired)。不存在 → 404。
func makeInvitationInfoHandler(id identity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if id == nil {
			writeError(w, http.StatusServiceUnavailable, "no_auth", "认证服务未初始化")
			return
		}
		token := chi.URLParam(r, "token")
		inv, err := id.InvitationInfo(r.Context(), token)
		if err != nil {
			if errors.Is(err, identity.ErrInvitationNotFound) {
				writeError(w, http.StatusNotFound, "invitation_invalid", "邀请链接无效")
				return
			}
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"state": inv.State})
	}
}

// makeListUsersHandler 返回 GET /api/users handler(管理员)。
func makeListUsersHandler(id identity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if id == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "用户服务未初始化")
			return
		}
		users, err := id.ListUsers(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		out := make([]userDTO, 0, len(users))
		for _, u := range users {
			out = append(out, toUserDTO(u))
		}
		writeJSON(w, http.StatusOK, map[string]any{"users": out})
	}
}

// makeCreateInvitationHandler 返回 POST /api/invitations handler(管理员 + CSRF)。
func makeCreateInvitationHandler(id identity.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if id == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "用户服务未初始化")
			return
		}
		createdBy := auditActor
		if sess, ok := sessionFromContext(r.Context()); ok && sess != nil && sess.Username != "" {
			createdBy = sess.Username
		}
		inv, err := id.CreateInvitation(r.Context(), createdBy, 0)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor:      createdBy,
			Action:     audit.ActionInvitationCreate,
			TargetType: audit.TargetInvitation,
			TargetID:   inv.ID,
			Detail:     map[string]any{"expiresAt": inv.ExpiresAt.UTC().Format(time.RFC3339)},
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusCreated, toInvitationDTO(*inv))
	}
}

// makeListInvitationsHandler 返回 GET /api/invitations handler(管理员;只读)。
func makeListInvitationsHandler(id identity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if id == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "用户服务未初始化")
			return
		}
		invs, err := id.ListInvitations(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		out := make([]invitationDTO, 0, len(invs))
		for _, inv := range invs {
			out = append(out, toInvitationDTO(inv))
		}
		writeJSON(w, http.StatusOK, map[string]any{"invitations": out})
	}
}

// makeRevokeInvitationHandler 返回 DELETE /api/invitations/{id} handler(管理员 + CSRF)。
func makeRevokeInvitationHandler(id identity.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if id == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "用户服务未初始化")
			return
		}
		invID := chi.URLParam(r, "id")
		if err := id.RevokeInvitation(r.Context(), invID); err != nil {
			if errors.Is(err, identity.ErrInvitationNotFound) {
				writeError(w, http.StatusNotFound, "invitation_not_found", "邀请不存在")
				return
			}
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor:      sessionUsernameOrAdmin(r),
			Action:     audit.ActionInvitationRevoke,
			TargetType: audit.TargetInvitation,
			TargetID:   invID,
			Detail:     map[string]any{},
			IP:         clientIP(r),
		})
		w.WriteHeader(http.StatusNoContent)
	}
}

// makeListProjectMembersHandler 返回 GET /api/projects/{id}/members handler(管理员;只读)。
func makeListProjectMembersHandler(id identity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if id == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "用户服务未初始化")
			return
		}
		projectID := chi.URLParam(r, "id")
		members, err := id.ListProjectMembers(r.Context(), projectID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		out := make([]userDTO, 0, len(members))
		for _, u := range members {
			out = append(out, toUserDTO(u))
		}
		writeJSON(w, http.StatusOK, map[string]any{"members": out})
	}
}

// makeAddProjectMemberHandler 返回 POST /api/projects/{id}/members handler(管理员 + CSRF)。
// body {userId} 或 {username};两者皆空 → 400。
func makeAddProjectMemberHandler(id identity.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if id == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "用户服务未初始化")
			return
		}
		projectID := chi.URLParam(r, "id")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			UserID   string `json:"userId"`
			Username string `json:"username"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		userID := req.UserID
		if userID == "" && req.Username != "" {
			u, err := id.GetUserByUsername(r.Context(), req.Username)
			if err != nil {
				if errors.Is(err, identity.ErrUserNotFound) {
					writeError(w, http.StatusNotFound, "user_not_found", "用户不存在")
					return
				}
				writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
				return
			}
			userID = u.ID
		}
		if userID == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "缺少用户标识")
			return
		}
		if err := id.AddMember(r.Context(), projectID, userID); err != nil {
			switch {
			case errors.Is(err, identity.ErrUserNotFound):
				writeError(w, http.StatusNotFound, "user_not_found", "用户不存在")
			case errors.Is(err, identity.ErrProjectNotFound):
				writeError(w, http.StatusNotFound, "project_not_found", "项目不存在")
			default:
				writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			}
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor:      sessionUsernameOrAdmin(r),
			Action:     audit.ActionProjectMemberAdd,
			TargetType: audit.TargetProject,
			TargetID:   projectID,
			Detail:     map[string]any{"userId": userID},
			IP:         clientIP(r),
		})
		w.WriteHeader(http.StatusNoContent)
	}
}

// makeRemoveProjectMemberHandler 返回 DELETE /api/projects/{id}/members/{userId}(管理员 + CSRF)。
func makeRemoveProjectMemberHandler(id identity.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if id == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "用户服务未初始化")
			return
		}
		projectID := chi.URLParam(r, "id")
		userID := chi.URLParam(r, "userId")
		if err := id.RemoveMember(r.Context(), projectID, userID); err != nil {
			if errors.Is(err, identity.ErrMemberNotFound) {
				writeError(w, http.StatusNotFound, "member_not_found", "成员关系不存在")
				return
			}
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor:      sessionUsernameOrAdmin(r),
			Action:     audit.ActionProjectMemberRemove,
			TargetType: audit.TargetProject,
			TargetID:   projectID,
			Detail:     map[string]any{"userId": userID},
			IP:         clientIP(r),
		})
		w.WriteHeader(http.StatusNoContent)
	}
}

// sessionUsernameOrAdmin 取当前会话用户名;无则回退审计默认 actor。
func sessionUsernameOrAdmin(r *http.Request) string {
	if sess, ok := sessionFromContext(r.Context()); ok && sess != nil && sess.Username != "" {
		return sess.Username
	}
	return auditActor
}
