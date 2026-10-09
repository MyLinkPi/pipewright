package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/appstore"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/target"
)

// 应用商店(DPanel 式一键部署)。模板 CRUD + 参数渲染部署;部署复用 Stacks 受管链路
// (/opt/pipewright/stacks/<name> + docker compose up -d),见 deployComposeToServer。
//
// 审计 action:appstore.*;detail 绝无参数值(secret 生成值仅部署响应一次性返回,不落审计)。

const (
	auditActionAppTemplateCreate = "appstore.template.create"
	auditActionAppTemplateUpdate = "appstore.template.update"
	auditActionAppTemplateDelete = "appstore.template.delete"
	auditActionAppDeploy         = "appstore.deploy"
	auditTargetAppStore          = "app_template"
)

// deployComposeToServer 把一段 compose 内容部署为受管 Stacks 项目(与 Stacks 部署同一条链路):
// 建受管目录 → Upload compose 文件(字节流)→ 探测 compose CLI → `compose -p <name> up -d`。
// 返回 (ok, 输出摘要, 人读错误);SSH 层错误经 humanServiceError 转人话。
func deployComposeToServer(ctx context.Context, svc target.Service, serverID, name, compose string) (bool, string, string) {
	dir := stacksBaseDir + "/" + name
	composePath := dir + "/" + composeFileName

	if res, err := svc.Exec(ctx, serverID, []string{"mkdir", "-p", dir}); err != nil {
		return false, "", humanServiceError(err)
	} else if res.ExitCode != 0 {
		return false, "", "创建受管目录失败:" + truncateLog(strings.TrimSpace(res.Stderr), 256)
	}
	if err := svc.Upload(ctx, serverID, strings.NewReader(compose), composePath); err != nil {
		return false, "", "写入 compose 文件失败:" + humanServiceError(err)
	}
	bin := detectComposeBin(ctx, svc, serverID)
	if bin == nil {
		return false, "", "该主机未检测到 docker compose / docker-compose,无法部署"
	}
	upCmd := append(append([]string{}, bin...), "-p", name, "-f", composePath, "up", "-d")
	res, err := svc.Exec(ctx, serverID, upCmd)
	if err != nil {
		return false, "", humanServiceError(err)
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = "docker compose up 以非零状态退出"
		}
		return false, "", truncateLog(msg, 1024)
	}
	return true, truncateLog(strings.TrimSpace(res.Stdout)+"\n"+strings.TrimSpace(res.Stderr), 2048), ""
}

// writeAppStoreError 把应用商店领域错误映射为契约错误码/状态码。
func writeAppStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, appstore.ErrNotFound):
		writeError(w, http.StatusNotFound, "app_template_not_found", "应用模板不存在")
	case errors.Is(err, appstore.ErrNameTaken):
		writeError(w, http.StatusConflict, "app_name_taken", "模板名已被占用")
	case errors.Is(err, appstore.ErrBuiltin):
		writeError(w, http.StatusBadRequest, "app_builtin_readonly", "内置模板不可修改或删除")
	case errors.Is(err, appstore.ErrInvalidName):
		writeError(w, http.StatusBadRequest, "invalid_app_template", "模板名非法:小写字母/数字(可含 -,不以 - 开头)")
	case errors.Is(err, appstore.ErrInvalidParam):
		writeError(w, http.StatusBadRequest, "invalid_app_template", "参数定义非法(名称/类型/默认值)")
	case errors.Is(err, appstore.ErrInvalidCompose):
		writeError(w, http.StatusBadRequest, "invalid_app_template", "compose 内容为空或超过 512 KiB")
	case errors.Is(err, appstore.ErrMissingParam):
		writeError(w, http.StatusBadRequest, "missing_app_param", "必填参数缺失:"+strings.TrimPrefix(err.Error(), appstore.ErrMissingParam.Error()+":"))
	case errors.Is(err, appstore.ErrUnknownParam):
		writeError(w, http.StatusBadRequest, "unknown_app_param", "提交了模板未定义的参数:"+strings.TrimPrefix(err.Error(), appstore.ErrUnknownParam.Error()+":"))
	case errors.Is(err, appstore.ErrInvalidValue):
		writeError(w, http.StatusBadRequest, "invalid_app_param", "参数值非法(类型/字符集/长度):"+strings.TrimPrefix(err.Error(), appstore.ErrInvalidValue.Error()+":"))
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// appParamSpecDTO 是模板参数定义 DTO(与领域 ParamSpec 字段一致)。
type appParamSpecDTO struct {
	Name         string `json:"name"`
	Label        string `json:"label,omitempty"`
	Type         string `json:"type"`
	Default      string `json:"default,omitempty"`
	Required     bool   `json:"required"`
	AutoGenerate bool   `json:"autoGenerate,omitempty"`
}

// appTemplateDTO 是应用模板对外响应体(冻结契约)。
type appTemplateDTO struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	DisplayName string            `json:"displayName"`
	Description string            `json:"description"`
	Icon        string            `json:"icon"`
	ComposeYAML string            `json:"composeYaml"`
	Params      []appParamSpecDTO `json:"params"`
	Builtin     bool              `json:"builtin"`
	CreatedAt   string            `json:"createdAt"`
	UpdatedAt   string            `json:"updatedAt"`
}

func toAppTemplateDTO(t appstore.Template) appTemplateDTO {
	params := make([]appParamSpecDTO, 0, len(t.Params))
	for _, p := range t.Params {
		params = append(params, appParamSpecDTO{
			Name: p.Name, Label: p.Label, Type: p.Type, Default: p.Default,
			Required: p.Required, AutoGenerate: p.AutoGenerate,
		})
	}
	return appTemplateDTO{
		ID: t.ID, Name: t.Name, DisplayName: t.DisplayName, Description: t.Description,
		Icon: t.Icon, ComposeYAML: t.ComposeYAML, Params: params, Builtin: t.Builtin,
		CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: t.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// makeListAppTemplatesHandler 返回 GET /api/ops/apps → { items: [...] }(内置在前)。
func makeListAppTemplatesHandler(svc appstore.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "应用商店未初始化")
			return
		}
		list, err := svc.List(r.Context())
		if err != nil {
			writeAppStoreError(w, err)
			return
		}
		items := make([]appTemplateDTO, 0, len(list))
		for _, t := range list {
			items = append(items, toAppTemplateDTO(t))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

// makeGetAppTemplateHandler 返回 GET /api/ops/apps/{id}。
func makeGetAppTemplateHandler(svc appstore.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "应用商店未初始化")
			return
		}
		t, err := svc.Get(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeAppStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, toAppTemplateDTO(*t))
	}
}

// makeCreateAppTemplateHandler 返回 POST /api/ops/apps(自定义模板)。
func makeCreateAppTemplateHandler(svc appstore.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		Name        string            `json:"name"`
		DisplayName string            `json:"displayName"`
		Description string            `json:"description"`
		Icon        string            `json:"icon"`
		ComposeYAML string            `json:"composeYaml"`
		Params      []appParamSpecDTO `json:"params"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "应用商店未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 600<<10)
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		t, err := svc.Create(r.Context(), appstore.CreateInput{
			Name: req.Name, DisplayName: req.DisplayName, Description: req.Description,
			Icon: req.Icon, ComposeYAML: req.ComposeYAML, Params: toParamSpecs(req.Params),
		})
		if err != nil {
			writeAppStoreError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionAppTemplateCreate, TargetType: auditTargetAppStore,
			TargetID: t.ID, Detail: map[string]any{"name": t.Name}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusCreated, toAppTemplateDTO(*t))
	}
}

// makeUpdateAppTemplateHandler 返回 PUT /api/ops/apps/{id}(自定义模板)。
func makeUpdateAppTemplateHandler(svc appstore.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		DisplayName *string           `json:"displayName"`
		Description *string           `json:"description"`
		Icon        *string           `json:"icon"`
		ComposeYAML *string           `json:"composeYaml"`
		Params      []appParamSpecDTO `json:"params"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "应用商店未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 600<<10)
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		in := appstore.UpdateInput{
			DisplayName: req.DisplayName, Description: req.Description, Icon: req.Icon,
			ComposeYAML: req.ComposeYAML,
		}
		if req.Params != nil {
			in.Params = toParamSpecs(req.Params)
		}
		t, err := svc.Update(r.Context(), chi.URLParam(r, "id"), in)
		if err != nil {
			writeAppStoreError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionAppTemplateUpdate, TargetType: auditTargetAppStore,
			TargetID: t.ID, Detail: map[string]any{"name": t.Name, "ok": true}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, toAppTemplateDTO(*t))
	}
}

// makeDeleteAppTemplateHandler 返回 DELETE /api/ops/apps/{id}(自定义模板)。
func makeDeleteAppTemplateHandler(svc appstore.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "应用商店未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.Delete(r.Context(), id); err != nil {
			writeAppStoreError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionAppTemplateDelete, TargetType: auditTargetAppStore,
			TargetID: id, Detail: map[string]any{"ok": true}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// makeDeployAppHandler 返回 POST /api/servers/{id}/apps/deploy {templateId, params}。
// 渲染参数(自动生成 secret)→ 复用 Stacks 链路部署到该主机;生成的 secret 随响应一次性返回
// (部署后以明文存在于目标机 compose 文件,与 Stacks 语义一致;审计只记模板名,不记参数值)。
func makeDeployAppHandler(apps appstore.Service, servers target.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		TemplateID string            `json:"templateId"`
		Params     map[string]string `json:"params"`
	}
	type response struct {
		ServerID string            `json:"serverId"`
		Name     string            `json:"name"`
		OK       bool              `json:"ok"`
		Output   string            `json:"output"`
		Error    string            `json:"error"`
		Params   map[string]string `json:"params,omitempty"` // 生效参数(含生成的 secret,仅此一次)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if apps == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "应用商店未初始化")
			return
		}
		if servers == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务器服务未初始化")
			return
		}
		serverID := chi.URLParam(r, "id")
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		if req.TemplateID == "" {
			writeError(w, http.StatusBadRequest, "invalid_app_template", "templateId 必填")
			return
		}
		tpl, err := apps.Get(r.Context(), req.TemplateID)
		if err != nil {
			writeAppStoreError(w, err)
			return
		}
		if _, err := servers.Get(r.Context(), serverID); err != nil {
			writeServerError(w, err)
			return
		}
		rendered, err := apps.Render(r.Context(), req.TemplateID, req.Params)
		if err != nil {
			writeAppStoreError(w, err)
			return
		}

		out := response{ServerID: serverID, Name: tpl.Name, Params: rendered.Used}
		cctx, cancel := context.WithTimeout(r.Context(), stacksUpTimeout)
		defer cancel()
		out.OK, out.Output, out.Error = deployComposeToServer(cctx, servers, serverID, tpl.Name, rendered.Compose)

		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionAppDeploy, TargetType: audit.TargetServer, TargetID: serverID,
			Detail: map[string]any{"template": tpl.Name, "ok": out.OK}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, out)
	}
}

// toParamSpecs 把 DTO 参数定义转领域对象。
func toParamSpecs(ps []appParamSpecDTO) []appstore.ParamSpec {
	out := make([]appstore.ParamSpec, 0, len(ps))
	for _, p := range ps {
		out = append(out, appstore.ParamSpec{
			Name: p.Name, Label: p.Label, Type: p.Type, Default: p.Default,
			Required: p.Required, AutoGenerate: p.AutoGenerate,
		})
	}
	return out
}
