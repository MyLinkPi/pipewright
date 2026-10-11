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
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/servicereg"
)

// projectServiceRegDTO 是项目卡片展示用的服务注册条目(仅展示子集;无上游/证书等敏感字段)。
// http 服务的访问地址 = fqdn;tcp 服务经网关监听端口(tcpListenPort)访问。
type projectServiceRegDTO struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	FQDN          string `json:"fqdn"`
	Protocol      string `json:"protocol"`
	TCPListenPort int    `json:"tcpListenPort"`
}

// projectRegRefReader 读取项目流水线绑定的服务注册引用(窄接口:只取 pipeline.Service 的
// 只读一面,便于测试替身;实现不触发惰性默认生成、无写库副作用)。
type projectRegRefReader interface {
	RegServiceRefs(ctx context.Context, projectID string) ([]string, error)
}

// serviceRegLister 全量列出注册服务(窄接口:servicereg.Service 的只读一面)。
type serviceRegLister interface {
	ListServices(ctx context.Context) ([]servicereg.ServiceWithDomain, error)
}

// projectDTO 是项目对外响应体(冻结契约;camelCase;无明文/无密文)。
// lastRunStatus / targetServers 本期为占位(null / []),数据由后续 story 填。
// serviceRegs 为项目已绑定的「服务注册」条目(卡片展示子集),按流水线部署节点 job.config
// 的 regServiceId(legacy gatewayService 按服务名兜底)实时解析;无绑定/无流水线 → []。
type projectDTO struct {
	ID              string                 `json:"id"`
	Name            string                 `json:"name"`
	RepoURL         string                 `json:"repoUrl"`
	DefaultBranch   string                 `json:"defaultBranch"`
	CredentialID    string                 `json:"credentialId"`
	CredentialName  string                 `json:"credentialName"`
	PacEnabled      bool                   `json:"pacEnabled"`
	PRStatusEnabled bool                   `json:"prStatusEnabled"`
	LastRunStatus   *string                `json:"lastRunStatus"`
	TargetServers   []string               `json:"targetServers"`
	ServiceRegs     []projectServiceRegDTO `json:"serviceRegs"`
	CreatedAt       string                 `json:"createdAt"`
	UpdatedAt       string                 `json:"updatedAt"`
}

// toProjectDTO 把领域 Project 转为契约 DTO(serviceRegs 默认空集合;
// 列表/更新响应用 withServiceRegs 覆盖为实时解析结果)。
func toProjectDTO(p *project.Project) projectDTO {
	return projectDTO{
		ID:              p.ID,
		Name:            p.Name,
		RepoURL:         p.RepoURL,
		DefaultBranch:   p.DefaultBranch,
		CredentialID:    p.CredentialID,
		CredentialName:  p.CredentialName,
		PacEnabled:      p.PacEnabled,
		PRStatusEnabled: p.PRStatusEnabled,
		LastRunStatus:   nil,                      // 本期占位:尚无运行
		TargetServers:   []string{},               // 本期占位:空集合
		ServiceRegs:     []projectServiceRegDTO{}, // 默认空;由调用方按绑定实时覆盖
		CreatedAt:       p.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:       p.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// withServiceRegs 把实时解析的服务注册展示子集写入 DTO(nil → 空集合,JSON 恒为 [])。
func withServiceRegs(dto projectDTO, regs []projectServiceRegDTO) projectDTO {
	if regs == nil {
		regs = []projectServiceRegDTO{}
	}
	dto.ServiceRegs = regs
	return dto
}

// resolveProjectsServiceRegs 为一批项目解析卡片展示用的服务注册条目:全量服务列表只取一次
// (避免 N+1 全表查询),逐项目读取流水线绑定引用(至多一次单行查询)。服务注册属于卡片附加
// 展示:任一环节读取失败都降级为空集合,不阻断项目列表。
func resolveProjectsServiceRegs(
	ctx context.Context, pipe projectRegRefReader, sr serviceRegLister, projectIDs []string,
) map[string][]projectServiceRegDTO {
	out := make(map[string][]projectServiceRegDTO, len(projectIDs))
	var all []servicereg.ServiceWithDomain
	if sr != nil {
		if list, err := sr.ListServices(ctx); err == nil {
			all = list
		}
	}
	for _, id := range projectIDs {
		out[id] = []projectServiceRegDTO{}
		if pipe == nil || len(all) == 0 {
			continue
		}
		refs, err := pipe.RegServiceRefs(ctx, id)
		if err != nil || len(refs) == 0 {
			continue
		}
		out[id] = matchProjectServiceRegs(refs, all)
	}
	return out
}

// resolveProjectServiceRegs 是单项目版本(更新响应复用,保持与列表同一解析语义)。
func resolveProjectServiceRegs(
	ctx context.Context, pipe projectRegRefReader, sr serviceRegLister, projectID string,
) []projectServiceRegDTO {
	return resolveProjectsServiceRegs(ctx, pipe, sr, []string{projectID})[projectID]
}

// matchProjectServiceRegs 把项目绑定的引用解析为展示条目:先按服务 ID 精确匹配,再按服务名
// 兜底(legacy gatewayService 键按名绑定);悬空引用(服务已删除)静默跳过。
func matchProjectServiceRegs(refs []string, all []servicereg.ServiceWithDomain) []projectServiceRegDTO {
	out := make([]projectServiceRegDTO, 0, len(refs))
	for _, ref := range refs {
		var hit *servicereg.ServiceWithDomain
		for i := range all {
			if all[i].ID == ref {
				hit = &all[i]
				break
			}
		}
		if hit == nil {
			for i := range all {
				if all[i].Name == ref {
					hit = &all[i]
					break
				}
			}
		}
		if hit == nil {
			continue
		}
		out = append(out, projectServiceRegDTO{
			ID:            hit.ID,
			Name:          hit.Name,
			FQDN:          hit.FQDN(hit.BaseDomain),
			Protocol:      hit.Protocol,
			TCPListenPort: hit.TCPListenPort,
		})
	}
	return out
}

// writeProjectError 把领域错误映射为契约错误码/状态码;绝不回显明文/凭据/栈。
func writeProjectError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, project.ErrVaultUnconfigured):
		writeError(w, http.StatusUnprocessableEntity, "vault_unconfigured", "保险库未配置 master key,无法校验仓库连通")
	case errors.Is(err, project.ErrInvalidCredential):
		writeError(w, http.StatusUnprocessableEntity, "credential_invalid", "凭据内容无法解析:请确认私钥完整、未加密(不支持带 passphrase 的私钥),或令牌粘贴完整")
	case errors.Is(err, project.ErrCredentialError),
		errors.Is(err, project.ErrCredentialNotFound):
		writeError(w, http.StatusUnprocessableEntity, "credential_error",
			"凭据被远端拒绝:请确认令牌/密码有效且账号有该仓库权限;SSH 方式需确认私钥对应的公钥已登记到代码托管平台(SSH 用户名以仓库地址 git@ 段为准)")
	case errors.Is(err, project.ErrRepoUnreachable):
		writeError(w, http.StatusUnprocessableEntity, "repo_unreachable", "仓库地址不可达,请检查地址")
	case errors.Is(err, project.ErrNotFound):
		writeError(w, http.StatusNotFound, "project_not_found", "项目不存在")
	case errors.Is(err, project.ErrProjectHasActiveRuns):
		writeError(w, http.StatusConflict, "project_has_active_runs", "该项目有进行中的运行,无法删除")
	case errors.Is(err, project.ErrEmptyName):
		writeError(w, http.StatusBadRequest, "invalid_project", "项目名称不能为空")
	case errors.Is(err, project.ErrEmptyRepoURL):
		writeError(w, http.StatusBadRequest, "invalid_project", "仓库地址不能为空")
	case errors.Is(err, project.ErrEmptyCredentialID):
		writeError(w, http.StatusBadRequest, "invalid_project", "请选择仓库凭据")
	default:
		// 内部错误:不泄漏细节。
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// atoiDefault 解析十进制整数;空串/非法值返回 def。
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// makeListProjectsHandler 返回 GET /api/projects handler。
// pipe/sr 用于给每张卡片附带「服务注册地址」展示子集(按流水线绑定实时解析);
// 任一为 nil(未装配)时 serviceRegs 输出 []。
func makeListProjectsHandler(svc project.Service, pipe projectRegRefReader, sr serviceRegLister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "项目服务未初始化")
			return
		}
		// 可选分页参数(默认第 1 页);响应保持冻结契约:仍是 projectDTO 数组。
		page := atoiDefault(r.URL.Query().Get("page"), 1)
		pageSize := atoiDefault(r.URL.Query().Get("pageSize"), project.DefaultPageSize)
		res, err := svc.ListPaged(r.Context(), page, pageSize)
		if err != nil {
			writeProjectError(w, err)
			return
		}
		ids := make([]string, 0, len(res.Items))
		for i := range res.Items {
			ids = append(ids, res.Items[i].ID)
		}
		regsByProject := resolveProjectsServiceRegs(r.Context(), pipe, sr, ids)
		out := make([]projectDTO, 0, len(res.Items))
		for i := range res.Items {
			out = append(out, withServiceRegs(toProjectDTO(&res.Items[i]), regsByProject[res.Items[i].ID]))
		}
		// 分页元信息经响应头暴露(不破坏数组契约;前端可选读取)。
		w.Header().Set("X-Total-Count", strconv.Itoa(res.Total))
		w.Header().Set("X-Page", strconv.Itoa(res.Page))
		w.Header().Set("X-Page-Size", strconv.Itoa(res.PageSize))
		writeJSON(w, http.StatusOK, out)
	}
}

// makeCreateProjectHandler 返回 POST /api/projects handler。
// 创建成功后追加 project_create 审计(detail 仅项目元数据)。
func makeCreateProjectHandler(svc project.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "项目服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			Name          string `json:"name"`
			RepoURL       string `json:"repoUrl"`
			CredentialID  string `json:"credentialId"`
			DefaultBranch string `json:"defaultBranch"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		p, err := svc.Create(r.Context(), project.CreateInput{
			Name:          req.Name,
			RepoURL:       req.RepoURL,
			CredentialID:  req.CredentialID,
			DefaultBranch: req.DefaultBranch,
		})
		if err != nil {
			writeProjectError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor:      auditActor,
			Action:     audit.ActionProjectCreate,
			TargetType: audit.TargetProject,
			TargetID:   p.ID,
			Detail:     map[string]any{"name": p.Name, "repoUrl": p.RepoURL, "defaultBranch": p.DefaultBranch},
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusCreated, toProjectDTO(p))
	}
}

// makeUpdateProjectHandler 返回 PATCH /api/projects/{id} handler。
// 更新成功后追加 project_update 审计。pipe/sr 用于回填 serviceRegs(与列表同一解析语义;
// 前端以更新响应替换卡片,缺省会让卡片上的服务注册地址消失)。
func makeUpdateProjectHandler(svc project.Service, aud audit.Recorder, pipe projectRegRefReader, sr serviceRegLister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "项目服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			Name            *string `json:"name"`
			DefaultBranch   *string `json:"defaultBranch"`
			CredentialID    *string `json:"credentialId"`
			PacEnabled      *bool   `json:"pacEnabled"`
			PRStatusEnabled *bool   `json:"prStatusEnabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		p, err := svc.Update(r.Context(), id, project.UpdateInput{
			Name:            req.Name,
			DefaultBranch:   req.DefaultBranch,
			CredentialID:    req.CredentialID,
			PacEnabled:      req.PacEnabled,
			PRStatusEnabled: req.PRStatusEnabled,
		})
		if err != nil {
			writeProjectError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor:      auditActor,
			Action:     audit.ActionProjectUpdate,
			TargetType: audit.TargetProject,
			TargetID:   p.ID,
			Detail:     map[string]any{"name": p.Name, "repoUrl": p.RepoURL, "defaultBranch": p.DefaultBranch},
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusOK, withServiceRegs(toProjectDTO(p), resolveProjectServiceRegs(r.Context(), pipe, sr, p.ID)))
	}
}

// makeDeleteProjectHandler 返回 DELETE /api/projects/{id} handler。
// 删除成功后追加 project_delete 审计。
func makeDeleteProjectHandler(svc project.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "项目服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.Delete(r.Context(), id); err != nil {
			writeProjectError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor:      auditActor,
			Action:     audit.ActionProjectDelete,
			TargetType: audit.TargetProject,
			TargetID:   id,
			IP:         clientIP(r),
		})
		w.WriteHeader(http.StatusNoContent)
	}
}

// makeTestCloneHandler 返回 POST /api/projects/test-clone handler(不落库)。
func makeTestCloneHandler(svc project.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "项目服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			RepoURL      string `json:"repoUrl"`
			CredentialID string `json:"credentialId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		res, err := svc.TestClone(r.Context(), req.RepoURL, req.CredentialID)
		if err != nil {
			writeProjectError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            true,
			"defaultBranch": res.DefaultBranch,
		})
	}
}
