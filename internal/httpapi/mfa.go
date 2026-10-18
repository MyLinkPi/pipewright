package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/mfa"
)

// cookieWebAuthnChallenge 是 WebAuthn 挑战 id 的 cookie(值仅是随机 id,
// 真正的挑战数据在服务端 webauthn_challenges 表;Path 限 /api 覆盖登录与账户路径)。
const cookieWebAuthnChallenge = "pipewright_webauthn"

// setChallengeCookie 下发挑战 id cookie(TTL 与挑战行一致)。
func setChallengeCookie(w http.ResponseWriter, r *http.Request, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieWebAuthnChallenge,
		Value:    id,
		Path:     "/api",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsSecure(r),
		MaxAge:   int(mfa.ChallengeTTL.Seconds()),
	})
}

// clearChallengeCookie 清除挑战 cookie(结束/失败)。
func clearChallengeCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieWebAuthnChallenge, Value: "", Path: "/api", MaxAge: -1, Expires: time.Unix(0, 0),
	})
}

// challengeIDFromRequest 从 cookie 读挑战 id;缺失返回空串。
func challengeIDFromRequest(r *http.Request) string {
	if c, err := r.Cookie(cookieWebAuthnChallenge); err == nil {
		return c.Value
	}
	return ""
}

// writeMFAError 把 mfa 领域错误映射为 HTTP 错误响应;已处理返回 true。
func writeMFAError(w http.ResponseWriter, err error) bool {
	var locked *mfa.ErrLockedOut
	switch {
	case err == nil:
		return false
	case errors.As(err, &locked):
		mins := int(locked.RetryAfter.Minutes()) + 1
		writeError(w, http.StatusTooManyRequests, "locked_out",
			"尝试次数过多,请 "+strconv.Itoa(mins)+" 分钟后重试")
	case errors.Is(err, mfa.ErrInvalidCode):
		writeError(w, http.StatusUnauthorized, "invalid_code", "验证码或备份码错误")
	case errors.Is(err, mfa.ErrTOTPNotSetup):
		writeError(w, http.StatusConflict, "totp_not_setup", "请先发起两步验证配置")
	case errors.Is(err, mfa.ErrTOTPAlreadyOn):
		writeError(w, http.StatusConflict, "totp_already_enabled", "两步验证已开启")
	case errors.Is(err, mfa.ErrTOTPNotEnabled):
		writeError(w, http.StatusConflict, "totp_not_enabled", "两步验证未开启")
	case errors.Is(err, mfa.ErrPasskeyNotFound):
		writeError(w, http.StatusNotFound, "passkey_not_found", "通行密钥不存在")
	case errors.Is(err, mfa.ErrChallengeNotFound):
		writeError(w, http.StatusBadRequest, "challenge_expired", "验证会话已过期,请重试")
	case errors.Is(err, mfa.ErrPasskeyDisabled):
		writeError(w, http.StatusForbidden, "passkey_disabled", "通行密钥登录未启用")
	case errors.Is(err, mfa.ErrInsecureContext):
		writeError(w, http.StatusBadRequest, "insecure_context",
			"通行密钥需要 HTTPS 访问(localhost 除外),请配置 HTTPS 或使用应用内地址")
	case errors.Is(err, mfa.ErrSecretUnreadable):
		writeError(w, http.StatusInternalServerError, "vault_key_missing",
			"验证密钥无法解密(加密密钥不可用),请联系管理员")
	case errors.Is(err, mfa.ErrOwnerNotFound):
		writeError(w, http.StatusNotFound, "user_not_found", "用户不存在")
	default:
		return false
	}
	return true
}

// ---- 公开路由(/api/auth)--------------------------------------------------------

// mountMFAAuth 挂载匿名 MFA 端点:登录第二步、Passkey 登录、登录页能力探测。
// svc 为 nil 时(mfa 未装配)各端点返回 503。
func mountMFAAuth(r chi.Router, svc *mfa.Service, authn auth.Authenticator) {
	r.Post("/login/mfa", makeMFALoginHandler(svc, authn))
	r.Get("/options", makeAuthOptionsHandler(svc))
	r.Post("/passkey/login/begin", makePasskeyLoginBeginHandler(svc))
	r.Post("/passkey/login/finish", makePasskeyLoginFinishHandler(svc, authn))
}

// makeMFALoginHandler 返回 POST /api/auth/login/mfa {code}。
// 前提:口令已通过(pending 会话 cookie 已下发)。手动做会话校验(不走 requireAuth,
// 因为 pending 会话会被它拒绝)+ CSRF(header 与 pending 会话的服务端 token 比对,
// 与 requireCSRF 同算法)。成功把会话升级为 30 天正式会话并重发 cookie。
func makeMFALoginHandler(svc *mfa.Service, authn auth.Authenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil || authn == nil {
			writeError(w, http.StatusServiceUnavailable, "no_mfa", "两步验证未初始化")
			return
		}
		cookie, err := r.Cookie(cookieSession)
		if err != nil || cookie.Value == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "请先登录")
			return
		}
		sess, err := authn.Verify(cookie.Value)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "mfa_required", "会话已过期,请重新登录")
			return
		}
		if !sess.MFAPending {
			// 非 pending 会话不应出现在此端点(已完成或从未进入两步流程)。
			writeError(w, http.StatusUnauthorized, "mfa_required", "无需两步验证,请直接登录")
			return
		}
		headerVal := r.Header.Get(headerCsrf)
		if headerVal == "" || subtle.ConstantTimeCompare([]byte(headerVal), []byte(sess.CSRFToken)) != 1 {
			writeError(w, http.StatusForbidden, "csrf_invalid", "CSRF token 缺失或不匹配")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<12)
		var req struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		if err := svc.VerifyLoginCode(sess.Role, sess.UserID, req.Code); err != nil {
			if !writeMFAError(w, err) {
				writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			}
			return
		}
		upgraded, err := authn.CompleteMFA(cookie.Value)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "mfa_required", "会话已过期,请重新登录")
			return
		}
		setSessionCookies(w, r, upgraded)
		writeJSON(w, http.StatusOK, map[string]string{"username": upgraded.Username, "role": upgraded.Role})
	}
}

// makeAuthOptionsHandler 返回 GET /api/auth/options:登录页能力探测(公开只读)。
// passkey_login=实例内有已注册通行密钥且总开关开启;不泄露任何账户信息。
func makeAuthOptionsHandler(svc *mfa.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		usable := svc != nil && svc.PasskeyLoginUsable(r.Context())
		writeJSON(w, http.StatusOK, map[string]bool{"passkey_login": usable})
	}
}

// makePasskeyLoginBeginHandler 返回 POST /api/auth/passkey/login/begin(匿名)。
// 返回浏览器原生 PublicKeyCredentialRequestOptions 并种下挑战 cookie。
func makePasskeyLoginBeginHandler(svc *mfa.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "no_mfa", "两步验证未初始化")
			return
		}
		res, err := svc.BeginPasskeyLogin(r.Context(), requestIsSecure(r), r.Host)
		if err != nil {
			if !writeMFAError(w, err) {
				writeError(w, http.StatusBadRequest, "passkey_begin_failed", "无法发起通行密钥登录")
			}
			return
		}
		setChallengeCookie(w, r, res.ChallengeID)
		writeJSON(w, http.StatusOK, map[string]any{"options": res.Options})
	}
}

// makePasskeyLoginFinishHandler 返回 POST /api/auth/passkey/login/finish(匿名)。
// 请求体是浏览器 PublicKeyCredential 原生 JSON(库直接解析 r)。
// 成功直接签发 30 天已验证会话(Passkey 即双因素,不再叠加 TOTP)。
func makePasskeyLoginFinishHandler(svc *mfa.Service, authn auth.Authenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil || authn == nil {
			writeError(w, http.StatusServiceUnavailable, "no_mfa", "两步验证未初始化")
			return
		}
		cid := challengeIDFromRequest(r)
		if cid == "" {
			writeError(w, http.StatusBadRequest, "challenge_expired", "验证会话已过期,请重试")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		ident, err := svc.FinishPasskeyLogin(r.Context(), cid, requestIsSecure(r), r.Host, r)
		clearChallengeCookie(w)
		if err != nil {
			if !writeMFAError(w, err) {
				writeError(w, http.StatusUnauthorized, "invalid_assertion", "通行密钥验证失败")
			}
			return
		}
		sess, err := authn.CreatePasskeySession(ident.Role, ident.UserID, ident.Username)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		setSessionCookies(w, r, sess)
		writeJSON(w, http.StatusOK, map[string]string{"username": sess.Username, "role": sess.Role})
	}
}

// ---- 账户路由(/api/account/mfa,认证 + CSRF + memberGate 白名单)-------------------

// mountAccountMFA 挂载本人两步验证管理端点。
func mountAccountMFA(r chi.Router, svc *mfa.Service, aud audit.Recorder) {
	r.Get("/account/mfa", makeMFAStatusHandler(svc))
	r.Post("/account/mfa/totp/setup", makeTOTPSetupHandler(svc))
	r.Post("/account/mfa/totp/enable", makeTOTPEnableHandler(svc, aud))
	r.Post("/account/mfa/totp/disable", makeTOTPDisableHandler(svc, aud))
	r.Post("/account/mfa/totp/backup-codes", makeBackupCodesHandler(svc, aud))
	r.Post("/account/mfa/passkeys/register/begin", makePasskeyRegisterBeginHandler(svc))
	r.Post("/account/mfa/passkeys/register/finish", makePasskeyRegisterFinishHandler(svc, aud))
	r.Delete("/account/mfa/passkeys/{id}", makePasskeyDeleteHandler(svc, aud))
}

// mfaSession 从 context 取会话并归一 owner 身份(role/userID/username)。
func mfaSession(r *http.Request) (role, userID, username string, ok bool) {
	sess, found := sessionFromContext(r.Context())
	if !found || sess == nil {
		return "", "", "", false
	}
	return sess.Role, sess.UserID, sess.Username, true
}

func makeMFAStatusHandler(svc *mfa.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "no_mfa", "两步验证未初始化")
			return
		}
		role, userID, _, ok := mfaSession(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "请先登录")
			return
		}
		info, err := svc.Status(role, userID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		writeJSON(w, http.StatusOK, info)
	}
}

// makeTOTPSetupHandler 返回 POST /api/account/mfa/totp/setup:
// 生成新密钥与备份码(明文仅本次返回)。已启用者须先关闭。
func makeTOTPSetupHandler(svc *mfa.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "no_mfa", "两步验证未初始化")
			return
		}
		role, userID, username, ok := mfaSession(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "请先登录")
			return
		}
		setup, err := svc.SetupTOTP(role, userID, username)
		if err != nil {
			if !writeMFAError(w, err) {
				writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"secret":      setup.Secret,
			"otpauthUrl":  setup.OTPAuthURL,
			"backupCodes": setup.BackupCodes,
		})
	}
}

func makeTOTPEnableHandler(svc *mfa.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "no_mfa", "两步验证未初始化")
			return
		}
		role, userID, username, ok := mfaSession(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "请先登录")
			return
		}
		var req struct {
			Code string `json:"code"`
		}
		if !decodeJSONBody(w, r, &req) {
			return
		}
		if err := svc.EnableTOTP(role, userID, req.Code); err != nil {
			if !writeMFAError(w, err) {
				writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			}
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActorOf(r, username), Action: audit.ActionTOTPEnabled,
			TargetType: audit.TargetAccount, TargetID: auditActorOf(r, username),
			Detail: map[string]any{"method": "totp"}, IP: clientIP(r),
		})
		w.WriteHeader(http.StatusNoContent)
	}
}

func makeTOTPDisableHandler(svc *mfa.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "no_mfa", "两步验证未初始化")
			return
		}
		role, userID, username, ok := mfaSession(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "请先登录")
			return
		}
		var req struct {
			Code string `json:"code"`
		}
		if !decodeJSONBody(w, r, &req) {
			return
		}
		if err := svc.DisableTOTP(role, userID, req.Code); err != nil {
			if !writeMFAError(w, err) {
				writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			}
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActorOf(r, username), Action: audit.ActionTOTPDisabled,
			TargetType: audit.TargetAccount, TargetID: auditActorOf(r, username),
			Detail: map[string]any{"method": "totp"}, IP: clientIP(r),
		})
		w.WriteHeader(http.StatusNoContent)
	}
}

func makeBackupCodesHandler(svc *mfa.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "no_mfa", "两步验证未初始化")
			return
		}
		role, userID, username, ok := mfaSession(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "请先登录")
			return
		}
		var req struct {
			Code string `json:"code"`
		}
		if !decodeJSONBody(w, r, &req) {
			return
		}
		codes, err := svc.RegenerateBackupCodes(role, userID, req.Code)
		if err != nil {
			if !writeMFAError(w, err) {
				writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			}
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActorOf(r, username), Action: audit.ActionBackupCodesRegenerated,
			TargetType: audit.TargetAccount, TargetID: auditActorOf(r, username),
			Detail: map[string]any{"count": len(codes)}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"backupCodes": codes})
	}
}

// makePasskeyRegisterBeginHandler 返回 POST /api/account/mfa/passkeys/register/begin
// {name?}:返回注册选项 + 挑战 cookie。
func makePasskeyRegisterBeginHandler(svc *mfa.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "no_mfa", "两步验证未初始化")
			return
		}
		role, userID, username, ok := mfaSession(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "请先登录")
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<12)
			_ = json.NewDecoder(r.Body).Decode(&req) // body 可空;解析失败用默认名
		}
		res, err := svc.BeginPasskeyRegistration(r.Context(), role, userID, username, req.Name,
			requestIsSecure(r), r.Host)
		if err != nil {
			if !writeMFAError(w, err) {
				writeError(w, http.StatusBadRequest, "passkey_begin_failed", "无法发起通行密钥注册")
			}
			return
		}
		setChallengeCookie(w, r, res.ChallengeID)
		writeJSON(w, http.StatusOK, map[string]any{"options": res.Options})
	}
}

// makePasskeyRegisterFinishHandler 返回 POST /api/account/mfa/passkeys/register/finish:
// 请求体为浏览器 PublicKeyCredential 原生 JSON。
func makePasskeyRegisterFinishHandler(svc *mfa.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "no_mfa", "两步验证未初始化")
			return
		}
		role, userID, username, ok := mfaSession(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "请先登录")
			return
		}
		cid := challengeIDFromRequest(r)
		if cid == "" {
			writeError(w, http.StatusBadRequest, "challenge_expired", "注册会话已过期,请重试")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		info, err := svc.FinishPasskeyRegistration(r.Context(), role, userID, username, cid,
			requestIsSecure(r), r.Host, r)
		clearChallengeCookie(w)
		if err != nil {
			if !writeMFAError(w, err) {
				writeError(w, http.StatusBadRequest, "passkey_register_failed", "通行密钥注册失败")
			}
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActorOf(r, username), Action: audit.ActionPasskeyAdded,
			TargetType: audit.TargetAccount, TargetID: auditActorOf(r, username),
			Detail: map[string]any{"passkeyId": info.ID, "name": info.Name}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, info)
	}
}

func makePasskeyDeleteHandler(svc *mfa.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "no_mfa", "两步验证未初始化")
			return
		}
		role, userID, username, ok := mfaSession(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "请先登录")
			return
		}
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "无效的通行密钥 id")
			return
		}
		if err := svc.DeletePasskey(role, userID, id); err != nil {
			if !writeMFAError(w, err) {
				writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			}
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActorOf(r, username), Action: audit.ActionPasskeyRemoved,
			TargetType: audit.TargetAccount, TargetID: auditActorOf(r, username),
			Detail: map[string]any{"passkeyId": id}, IP: clientIP(r),
		})
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---- 管理员路由(/api/users/{id}/mfa/*)------------------------------------------

// mountAdminMFA 挂载成员 MFA 重置端点(成员被锁死时的恢复通道)。
func mountAdminMFA(r chi.Router, svc *mfa.Service, aud audit.Recorder) {
	r.Delete("/users/{id}/mfa/totp", makeAdminResetTOTPHandler(svc, aud))
	r.Delete("/users/{id}/mfa/passkeys", makeAdminResetPasskeysHandler(svc, aud))
}

func makeAdminResetTOTPHandler(svc *mfa.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "no_mfa", "两步验证未初始化")
			return
		}
		userID := chi.URLParam(r, "id")
		if err := svc.AdminResetTOTP(userID); err != nil {
			if !writeMFAError(w, err) {
				writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			}
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActorOf(r, ""), Action: audit.ActionMemberMFAReset,
			TargetType: audit.TargetUser, TargetID: userID,
			Detail: map[string]any{"what": "totp"}, IP: clientIP(r),
		})
		w.WriteHeader(http.StatusNoContent)
	}
}

func makeAdminResetPasskeysHandler(svc *mfa.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "no_mfa", "两步验证未初始化")
			return
		}
		userID := chi.URLParam(r, "id")
		n, err := svc.AdminResetPasskeys(userID)
		if err != nil {
			if !writeMFAError(w, err) {
				writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			}
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActorOf(r, ""), Action: audit.ActionMemberMFAReset,
			TargetType: audit.TargetUser, TargetID: userID,
			Detail: map[string]any{"what": "passkeys", "count": n}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"removed": n})
	}
}

// ---- 小工具 ---------------------------------------------------------------------

// auditActorOf 取审计 actor:会话用户名优先,回退包级默认。
func auditActorOf(r *http.Request, username string) string {
	if username != "" {
		return username
	}
	if sess, ok := sessionFromContext(r.Context()); ok && sess != nil && sess.Username != "" {
		return sess.Username
	}
	return auditActor
}
