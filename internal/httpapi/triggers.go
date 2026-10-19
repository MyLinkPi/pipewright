package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/mesh"
	"github.com/huangchengsir/pipewright/internal/trigger"
)

// webhookURLPrefix 是 webhook 接收端点路径前缀(冻结契约:/api/webhooks/<token>)。
// 注:接收端点本身 = Story 3-2,本期只在配置里返回该 URL。
const webhookURLPrefix = "/api/webhooks/"

// triggerDTO 是触发配置对外响应体(冻结契约;camelCase;密钥仅掩码,绝无明文)。
type triggerDTO struct {
	WebhookURL          string    `json:"webhookUrl"`
	WebhookSecretMasked string    `json:"webhookSecretMasked"`
	Events              eventsDTO `json:"events"`
	// BranchFilters 为分支过滤 glob 白名单;空 = 所有分支触发。
	BranchFilters []string `json:"branchFilters"`
	// PathFilters 为路径过滤 glob 列表(monorepo · P0);空 = 不启用(放行一切)。
	PathFilters []string `json:"pathFilters"`
}

type eventsDTO struct {
	Push        bool `json:"push"`
	Tag         bool `json:"tag"`
	PullRequest bool `json:"pullRequest"`
	Release     bool `json:"release"`
}

// toTriggerDTO 把领域 Config 转为契约 DTO(密钥仅掩码)。
func toTriggerDTO(c *trigger.Config) triggerDTO {
	branchFilters := c.BranchFilters
	if branchFilters == nil {
		branchFilters = []string{}
	}
	filters := c.PathFilters
	if filters == nil {
		filters = []string{}
	}
	return triggerDTO{
		WebhookURL:          webhookURLPrefix + c.WebhookToken,
		WebhookSecretMasked: c.WebhookSecretMasked,
		Events: eventsDTO{
			Push:        c.Events.Push,
			Tag:         c.Events.Tag,
			PullRequest: c.Events.PullRequest,
			Release:     c.Events.Release,
		},
		BranchFilters: branchFilters,
		PathFilters:   filters,
	}
}

// writeTriggerError 把领域错误映射为契约错误码/状态码;绝不回显密钥明文/密文/栈。
func writeTriggerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, trigger.ErrVaultUnconfigured):
		writeError(w, http.StatusUnprocessableEntity, "vault_unconfigured", "保险库未配置 master key,无法生成或读取 webhook 签名密钥")
	case errors.Is(err, trigger.ErrProjectNotFound):
		writeError(w, http.StatusNotFound, "project_not_found", "项目不存在")
	case errors.Is(err, trigger.ErrNotFound):
		writeError(w, http.StatusNotFound, "trigger_not_found", "触发配置不存在")
	case errors.Is(err, trigger.ErrInvalidBranchPattern):
		writeError(w, http.StatusUnprocessableEntity, "invalid_branch_pattern", "分支过滤不能为空且需为合法通配(如 main、release/*)")
	case errors.Is(err, trigger.ErrInvalidPathFilter):
		writeError(w, http.StatusUnprocessableEntity, "invalid_path_filter", "路径过滤 glob 不能含空白(如 backend/**、*.go)")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// makeGetTriggerHandler 返回 GET /api/projects/{id}/trigger handler。
// 首次访问无配置 → 惰性生成默认(token + 加密签名密钥)并返回;密钥仅掩码。
func makeGetTriggerHandler(svc trigger.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "触发配置服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		cfg, err := svc.Get(r.Context(), id)
		if err != nil {
			writeTriggerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, toTriggerDTO(cfg))
	}
}

// makeSaveTriggerHandler 返回 PUT /api/projects/{id}/trigger handler。
// 保存 events / 分支过滤 / 路径过滤;不触碰密钥。校验失败 → 422 定位到项。
func makeSaveTriggerHandler(svc trigger.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "触发配置服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			Events struct {
				Push        bool `json:"push"`
				Tag         bool `json:"tag"`
				PullRequest bool `json:"pullRequest"`
				Release     bool `json:"release"`
			} `json:"events"`
			BranchFilters []string `json:"branchFilters"`
			PathFilters   []string `json:"pathFilters"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		cfg, err := svc.Save(r.Context(), id, trigger.SaveInput{
			Events: trigger.Events{
				Push:        req.Events.Push,
				Tag:         req.Events.Tag,
				PullRequest: req.Events.PullRequest,
				Release:     req.Events.Release,
			},
			BranchFilters: req.BranchFilters,
			PathFilters:   req.PathFilters,
		})
		if err != nil {
			writeTriggerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, toTriggerDTO(cfg))
	}
}

// makeResetTriggerSecretHandler 返回 POST /api/projects/{id}/trigger/secret/reset handler。
// 重新生成并加密签名密钥(旧值失效),完整明文仅在此响应里一次性回显(供粘贴到 Gitee/Gitea)。
// 成功后追加 trigger_secret_reset 审计;detail **绝不含密钥明文/密文**,仅记掩码。
func makeResetTriggerSecretHandler(svc trigger.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "触发配置服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		res, err := svc.ResetSecret(r.Context(), id)
		if err != nil {
			writeTriggerError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor:      auditActor,
			Action:     audit.ActionTriggerSecretReset,
			TargetType: audit.TargetTrigger,
			TargetID:   id,
			// detail 仅记掩码(已脱敏值);即便 Masker 也会兜底,绝不写明文密钥。
			Detail: map[string]any{"webhookSecretMasked": res.Masked},
			IP:     clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]string{
			"webhookSecret":       res.Secret, // 完整明文,仅此一次
			"webhookSecretMasked": res.Masked,
		})
	}
}

// webhookEndpointDTO 是「接收端点可选环境地址」列表项(集群转发)。
type webhookEndpointDTO struct {
	// NodeID 是对端环境节点 id(前端选择器的稳定 key)。
	NodeID string `json:"nodeId"`
	// Name 是对端环境名(mesh_peers.name;空则回退虚拟 IP)。
	Name string `json:"name"`
	// Base 是该环境的 Web 基址(仅登记了对外地址 publicUrl 的对端;末尾不带 /)。
	Base string `json:"base"`
}

// webhookEndpointsDTO 是 GET /api/projects/{id}/trigger/webhook-endpoints 响应体。
type webhookEndpointsDTO struct {
	Items []webhookEndpointDTO `json:"items"`
}

// makeWebhookEndpointsHandler 返回 GET /api/projects/{id}/trigger/webhook-endpoints handler。
// 集群启用时返回**已配置对外地址(publicUrl)**的对端环境列表,供 UI 把接收端点切换到
// 其他环境的地址复制(Git 平台只需够到任一环境,集群会把投递转发到 token 所属环境;
// 归属环境需在集群 ACL 的「允许触发」中授权入口节点)。只读端点,不触碰项目触发配置;
// 项目成员可用(不暴露 admin 面 /api/mesh/peers 的公钥/版本等完整字段)。
func makeWebhookEndpointsHandler(svc mesh.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		empty := webhookEndpointsDTO{Items: []webhookEndpointDTO{}}
		if svc == nil {
			writeJSON(w, http.StatusOK, empty)
			return
		}
		st, err := svc.Settings(r.Context())
		if err != nil || !st.Enabled {
			writeJSON(w, http.StatusOK, empty)
			return
		}
		peers, err := svc.ListPeers(r.Context())
		if err != nil {
			writeServerError(w, err)
			return
		}
		items := make([]webhookEndpointDTO, 0, len(peers))
		for _, p := range peers {
			base := strings.TrimSpace(p.PublicURL)
			if base == "" || p.NodeID == st.NodeID {
				continue
			}
			name := strings.TrimSpace(p.Name)
			if name == "" {
				name = strings.TrimSpace(p.VirtualIP)
			}
			items = append(items, webhookEndpointDTO{
				NodeID: p.NodeID,
				Name:   name,
				Base:   strings.TrimSuffix(base, "/"),
			})
		}
		writeJSON(w, http.StatusOK, webhookEndpointsDTO{Items: items})
	}
}
