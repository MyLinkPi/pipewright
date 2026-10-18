package httpapi

import (
	"net/http"
	"strings"

	"github.com/huangchengsir/pipewright/internal/identity"
	"github.com/huangchengsir/pipewright/internal/run"
)

// memberGate 是普通用户(member)的**默认拒绝**授权门:
//   - 管理员(admin)直接放行;
//   - 普通用户仅放行白名单内的 (method, 路径模式),其余一律 403;
//   - 白名单内若涉及项目/运行归属,则校验其为该项目成员(否则 403)。
//
// 默认拒绝保证:未来新增的 API 端点自动对普通用户关闭(无需逐点维护黑名单)。
// 必须套在 requireAuth 之后(依赖 context 中的 Session)。
func memberGate(id identity.Service, runs run.Service, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, ok := sessionFromContext(r.Context())
		if !ok || sess == nil || sess.Role != roleMember {
			// 非普通用户(管理员/异常):不干预,交由后续 handler。
			next.ServeHTTP(w, r)
			return
		}

		segments := pathSegments(r.URL.Path)
		if !memberAllowed(r.Method, segments) {
			writeError(w, http.StatusForbidden, "forbidden", "无权限访问该资源")
			return
		}

		// 归属校验:项目维度直接取路径 id;运行维度先解析 run → project。
		if !memberProjectAllowed(r, id, runs, sess.UserID, segments) {
			writeError(w, http.StatusForbidden, "forbidden", "无权限访问该资源")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// roleMember 与 auth.RoleMember 字面量一致(避免 httpapi 反向依赖细节)。
const roleMember = "member"

// isMemberSession 判断当前请求是否为普通用户(member)会话。
func isMemberSession(r *http.Request) bool {
	sess, ok := sessionFromContext(r.Context())
	return ok && sess != nil && sess.Role == roleMember
}

// containsString 判断切片是否包含指定字符串。
func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// pathSegments 把 URL 路径切为段(去掉首尾斜杠)。"/api/runs/x/" → ["api","runs","x"]。
func pathSegments(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// memberAllowedPattern 是白名单条目:method + 段模式("*" 匹配单段;段数须完全一致)。
type memberAllowedPattern struct {
	method  string
	pattern []string
}

// memberAllowlist 是普通用户可访问的端点白名单(默认拒绝之外的全部例外)。
//
// 可见范围:项目条目、项目运行参数(触发运行表单所需)、手动触发运行、运行/日志/产物、
// 以及自身的账户设置(改口令/会话列表/撤销)。
var memberAllowlist = []memberAllowedPattern{
	{http.MethodGet, []string{"api", "account", "sessions"}},
	{http.MethodPost, []string{"api", "account", "password"}},
	{http.MethodDelete, []string{"api", "account", "sessions", "*"}},

	// 自身两步验证管理:状态查询、TOTP 开启/关闭/备份码、Passkey 注册/删除。
	{http.MethodGet, []string{"api", "account", "mfa"}},
	{http.MethodPost, []string{"api", "account", "mfa", "totp", "*"}},
	{http.MethodPost, []string{"api", "account", "mfa", "passkeys", "register", "*"}},
	{http.MethodDelete, []string{"api", "account", "mfa", "passkeys", "*"}},

	{http.MethodGet, []string{"api", "projects"}},
	{http.MethodGet, []string{"api", "projects", "*", "parameters"}},
	{http.MethodPost, []string{"api", "projects", "*", "runs"}},

	{http.MethodGet, []string{"api", "runs"}},
	{http.MethodGet, []string{"api", "runs", "*"}},
	{http.MethodGet, []string{"api", "runs", "*", "events"}},
	{http.MethodGet, []string{"api", "runs", "*", "logs"}},
	{http.MethodGet, []string{"api", "runs", "*", "artifacts"}},
	{http.MethodGet, []string{"api", "runs", "*", "artifacts", "*", "download"}},
}

// memberAllowed 判断 (method, 路径段) 是否命中白名单(段数完全一致,不做前缀匹配)。
func memberAllowed(method string, segments []string) bool {
	for _, p := range memberAllowlist {
		if p.method != method || len(p.pattern) != len(segments) {
			continue
		}
		match := true
		for i, seg := range p.pattern {
			if seg == "*" {
				continue
			}
			if seg != segments[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// memberProjectAllowed 校验普通用户对目标项目/运行的归属。
// 非项目/运行维度(如账户端点)无需归属校验 → true。
func memberProjectAllowed(r *http.Request, id identity.Service, runs run.Service, userID string, segments []string) bool {
	projectID := ""
	switch {
	case len(segments) >= 3 && segments[0] == "api" && segments[1] == "projects":
		projectID = segments[2]
	case len(segments) >= 3 && segments[0] == "api" && segments[1] == "runs":
		if runs == nil {
			return false
		}
		rn, err := runs.Get(r.Context(), segments[2])
		if err != nil {
			// 运行不存在/查询失败:不泄露存在性,一律拒绝。
			return false
		}
		projectID = rn.ProjectID
	default:
		return true
	}
	if projectID == "" {
		return false
	}
	if id == nil {
		return false
	}
	ok, err := id.IsMember(r.Context(), userID, projectID)
	return err == nil && ok
}

// requireAdmin 中间件:仅允许管理员访问(普通用户 403)。用于管理端点的纵深防御。
// 必须套在 requireAuth 之后。
func requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, ok := sessionFromContext(r.Context())
		if !ok || sess == nil || sess.Role != "admin" {
			writeError(w, http.StatusForbidden, "forbidden", "无权限访问该资源")
			return
		}
		next.ServeHTTP(w, r)
	})
}
