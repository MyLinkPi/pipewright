package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/appstore"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/objectstore"
	"github.com/huangchengsir/pipewright/internal/target"
	"gopkg.in/yaml.v3"
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
	auditActionObjectStoreReg    = "objectstore.register"
	auditTargetAppStore          = "app_template"
)

// appAutoRegisterTemplate 是「部署后自动登记为对象存储配置」的内置模板名(应用商店 RustFS 联动)。
const appAutoRegisterTemplate = "rustfs"

// appRegisterTimeout 是部署后联动登记对象存储的独立超时(含建桶短重试),不占 compose up 的窗口。
const appRegisterTimeout = 20 * time.Second

// registerRustFSObjectStore 是应用商店部署 RustFS 成功后的联动登记(尽力而为,绝不影响部署结果):
// 从渲染后的生效参数取接入信息(endpoint = 服务器地址 + API 端口),交给 objectstore.Register
// 完成「确保默认桶 + 凭据入 vault + 同 type+endpoint 幂等轮换」。返回 (是否已登记, 人话备注)。
// 备注会进入部署响应与审计 detail;绝不含 Secret(审计只记接入参数,与 objectstore.create 同纪律)。
func registerRustFSObjectStore(ctx context.Context, objStores objectstore.Service, aud audit.Recorder, r *http.Request, srv target.Server, used map[string]string) (bool, string) {
	if objStores == nil {
		return false, "" // 服务未注入(如裁剪装配)→ 静默跳过,部署不受影响
	}
	accessKey, secret, apiPort := used["access_key"], used["secret_key"], used["api_port"]
	if accessKey == "" || secret == "" || apiPort == "" {
		return false, "生效参数缺少 access_key / secret_key / api_port,未自动登记对象存储"
	}
	bucket := used["bucket"]
	if bucket == "" {
		bucket = "pipewright"
	}
	name := "RustFS · " + srv.Name
	if strings.TrimSpace(name) == "RustFS ·" {
		name = "RustFS · " + srv.Host
	}
	endpoint := net.JoinHostPort(srv.Host, apiPort)
	res, err := objStores.Register(ctx, objectstore.RegisterInput{
		Type:        objectstore.TypeRustFS,
		Name:        name,
		Endpoint:    endpoint,
		Bucket:      bucket,
		AccessKeyID: accessKey,
		Secret:      secret,
	})
	if err != nil {
		msg := "自动登记对象存储失败"
		if errors.Is(err, objectstore.ErrVaultUnconfigured) {
			msg += ":保险库未配置 master key"
		} else {
			msg += ":" + err.Error()
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionObjectStoreReg, TargetType: auditTargetObjectStore,
			Detail: map[string]any{"type": objectstore.TypeRustFS, "endpoint": endpoint, "bucket": bucket, "source": "appstore", "ok": false, "error": msg}, IP: clientIP(r),
		})
		return false, msg
	}
	note := "已自动登记到「设置 → 对象存储」"
	if res.Updated {
		note = "已更新既有对象存储配置的凭据(同 Endpoint 原地轮换)"
	}
	if res.Note != "" {
		note += ";" + res.Note
	}
	targetID := ""
	if res.Provider != nil {
		targetID = res.Provider.ID
	}
	recordAudit(r.Context(), aud, audit.Entry{
		Actor: auditActor, Action: auditActionObjectStoreReg, TargetType: auditTargetObjectStore,
		TargetID: targetID,
		Detail:   map[string]any{"type": objectstore.TypeRustFS, "endpoint": endpoint, "bucket": bucket, "source": "appstore", "ok": true, "updated": res.Updated}, IP: clientIP(r),
	})
	return true, note
}

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

// injectComposeLimits 把 CPU/内存上限注入 compose 每个 service 的 deploy.resources.limits
//(docker compose v2 非 swarm 模式同样生效;模板已写同名键时被本次部署选择覆盖)。
// 两者皆空 → 原样返回(零开销);模板产出非法 YAML → 错误,不部署。
func injectComposeLimits(compose, cpus, memory string) (string, error) {
	if cpus == "" && memory == "" {
		return compose, nil
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(compose), &doc); err != nil {
		return "", fmt.Errorf("解析 compose YAML: %w", err)
	}
	services, _ := doc["services"].(map[string]any)
	for _, raw := range services {
		svcMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		deploy, _ := svcMap["deploy"].(map[string]any)
		if deploy == nil {
			deploy = map[string]any{}
		}
		resources, _ := deploy["resources"].(map[string]any)
		if resources == nil {
			resources = map[string]any{}
		}
		limits, _ := resources["limits"].(map[string]any)
		if limits == nil {
			limits = map[string]any{}
		}
		if cpus != "" {
			limits["cpus"] = cpus
		}
		if memory != "" {
			limits["memory"] = memory
		}
		resources["limits"] = limits
		deploy["resources"] = resources
		svcMap["deploy"] = deploy
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("重编码 compose YAML: %w", err)
	}
	return string(out), nil
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

// makeDeployAppHandler 返回 POST /api/servers/{id}/apps/deploy {templateId, params, cpus?, memory?}。
// 渲染参数(自动生成 secret)→ 可选注入资源限制(deploy.resources.limits)→ 复用 Stacks 链路
// 部署到该主机;生成的 secret 随响应一次性返回(部署后以明文存在于目标机 compose 文件,与
// Stacks 语义一致;审计只记模板名,不记参数值)。RustFS 模板部署成功后联动自动登记对象存储
// 配置(尽力而为,结果见响应 objectStoreRegistered / objectStoreNote)。
func makeDeployAppHandler(apps appstore.Service, servers target.Service, aud audit.Recorder, objStores objectstore.Service) http.HandlerFunc {
	type request struct {
		TemplateID string            `json:"templateId"`
		Params     map[string]string `json:"params"`
		CPUs       string            `json:"cpus,omitempty"`   // 可选,全服务 CPU 核数上限(如 "1.0")
		Memory     string            `json:"memory,omitempty"` // 可选,全服务内存硬上限(如 "512m")
	}
	type response struct {
		ServerID string            `json:"serverId"`
		Name     string            `json:"name"`
		OK       bool              `json:"ok"`
		Output   string            `json:"output"`
		Error    string            `json:"error"`
		Params   map[string]string `json:"params,omitempty"` // 生效参数(含生成的 secret,仅此一次)
		// RustFS 联动登记结果(仅 rustfs 模板且部署成功时出现;尽力而为,失败不影响 OK)。
		ObjectStoreRegistered bool   `json:"objectStoreRegistered,omitempty"`
		ObjectStoreNote       string `json:"objectStoreNote,omitempty"`
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
		// 资源限制复用容器创建的同一白名单校验(reCPUs/reMemory,防非法值进 compose)。
		if req.CPUs != "" && !reCPUs.MatchString(req.CPUs) {
			writeError(w, http.StatusBadRequest, "invalid_app_deploy", "非法 CPU 上限(正整数/小数,如 0.5、1、2.5)")
			return
		}
		if req.Memory != "" && !reMemory.MatchString(req.Memory) {
			writeError(w, http.StatusBadRequest, "invalid_app_deploy", "非法内存上限(正整数 + 单位 b/k/m/g,如 512m、1g)")
			return
		}
		tpl, err := apps.Get(r.Context(), req.TemplateID)
		if err != nil {
			writeAppStoreError(w, err)
			return
		}
		srv, err := servers.Get(r.Context(), serverID)
		if err != nil {
			writeServerError(w, err)
			return
		}
		// 约定参数 advertise_host(如 Redpanda 模板):未显式提供时注入目标机 Host,
		// 让 Kafka advertise 等客户端通告地址自动指向部署宿主。
		if _, ok := req.Params["advertise_host"]; !ok {
			for _, p := range tpl.Params {
				if p.Name == "advertise_host" {
					if req.Params == nil {
						req.Params = map[string]string{}
					}
					req.Params["advertise_host"] = srv.Host
					break
				}
			}
		}
		rendered, err := apps.Render(r.Context(), req.TemplateID, req.Params)
		if err != nil {
			writeAppStoreError(w, err)
			return
		}
		compose := rendered.Compose
		if compose, err = injectComposeLimits(compose, req.CPUs, req.Memory); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_app_deploy", "资源限制注入失败:"+err.Error())
			return
		}

		out := response{ServerID: serverID, Name: tpl.Name, Params: rendered.Used}
		cctx, cancel := context.WithTimeout(r.Context(), stacksUpTimeout)
		defer cancel()
		out.OK, out.Output, out.Error = deployComposeToServer(cctx, servers, serverID, tpl.Name, compose)

		// RustFS 联动:部署成功后自动登记为对象存储配置(尽力而为,独立超时,不影响部署结果)。
		if out.OK && tpl.Name == appAutoRegisterTemplate {
			rctx, rcancel := context.WithTimeout(r.Context(), appRegisterTimeout)
			out.ObjectStoreRegistered, out.ObjectStoreNote = registerRustFSObjectStore(rctx, objStores, aud, r, *srv, rendered.Used)
			rcancel()
		}

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
