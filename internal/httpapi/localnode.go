package httpapi

// localnode.go 暴露「本机(控制节点)角色」配置端点(specs/local-node-roles.md):
//
//	GET /api/local-node → { buildEnabled, buildLabels, buildMaxBuilds, buildPriority,
//	                        deployEnabled, gatewayEnabled, platformSupportsDeployGateway }
//	PUT /api/local-node → 同上(需 CSRF;指针字段 nil = 不修改)。
//
// 仅 admin 可达(路由组叠加 requireAdmin)。平台不支持的角色(非 Linux 开 deploy/gateway)
// → 422 local_node_platform_unsupported;labels/槽位/优先级越界 → 422 invalid_local_node。
// 变更落审计(启用部署/网关角色 = 流程获得控制机执行权,审计必须可追溯)。

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/localnode"
)

const (
	auditActionLocalNodeSet = "localnode.set"
	auditTargetLocalNode    = "local_node"
)

// localNodeDTO 是本机角色视图。platformSupportsDeployGateway 是能力位:前端据此禁用
// 非 Linux 控制机上的 deploy/gateway 开关(保存侧仍有领域层强校验,双保险)。
type localNodeDTO struct {
	BuildEnabled                  bool   `json:"buildEnabled"`
	BuildLabels                   string `json:"buildLabels"`
	BuildMaxBuilds                int    `json:"buildMaxBuilds"`
	BuildPriority                 int    `json:"buildPriority"`
	DeployEnabled                 bool   `json:"deployEnabled"`
	GatewayEnabled                bool   `json:"gatewayEnabled"`
	PlatformSupportsDeployGateway bool   `json:"platformSupportsDeployGateway"`
}

func toLocalNodeDTO(r localnode.Roles) localNodeDTO {
	return localNodeDTO{
		BuildEnabled:                  r.BuildEnabled,
		BuildLabels:                   r.BuildLabels,
		BuildMaxBuilds:                r.BuildMaxBuilds,
		BuildPriority:                 r.BuildPriority,
		DeployEnabled:                 r.DeployEnabled,
		GatewayEnabled:                r.GatewayEnabled,
		PlatformSupportsDeployGateway: localnode.PlatformSupportsDeployGateway(),
	}
}

func writeLocalNodeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, localnode.ErrInvalidInput):
		writeError(w, http.StatusUnprocessableEntity, "invalid_local_node",
			"本机角色参数非法:labels 应为逗号分隔标签项(如 linux,arch=arm64),槽位 0..64,优先级 0..100")
	case errors.Is(err, localnode.ErrPlatformUnsupported):
		writeError(w, http.StatusUnprocessableEntity, "local_node_platform_unsupported",
			"当前控制机平台不支持部署目标/网关主机角色(要求 Linux;POSIX 脚本依赖)")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

func makeGetLocalNodeHandler(svc localnode.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "本机角色服务未初始化")
			return
		}
		roles, err := svc.Get(r.Context())
		if err != nil {
			writeLocalNodeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, toLocalNodeDTO(roles))
	}
}

func makeSaveLocalNodeHandler(svc localnode.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		BuildEnabled   *bool   `json:"buildEnabled"`
		BuildLabels    *string `json:"buildLabels"`
		BuildMaxBuilds *int    `json:"buildMaxBuilds"`
		BuildPriority  *int    `json:"buildPriority"`
		DeployEnabled  *bool   `json:"deployEnabled"`
		GatewayEnabled *bool   `json:"gatewayEnabled"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "本机角色服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		roles, err := svc.Save(r.Context(), localnode.SaveInput{
			BuildEnabled:   req.BuildEnabled,
			BuildLabels:    req.BuildLabels,
			BuildMaxBuilds: req.BuildMaxBuilds,
			BuildPriority:  req.BuildPriority,
			DeployEnabled:  req.DeployEnabled,
			GatewayEnabled: req.GatewayEnabled,
		})
		if err != nil {
			writeLocalNodeError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionLocalNodeSet, TargetType: auditTargetLocalNode,
			TargetID: "local",
			Detail: map[string]any{
				"ok":           true,
				"buildEnabled": roles.BuildEnabled, "buildLabels": roles.BuildLabels,
				"buildMaxBuilds": roles.BuildMaxBuilds, "buildPriority": roles.BuildPriority,
				"deployEnabled": roles.DeployEnabled, "gatewayEnabled": roles.GatewayEnabled,
			},
		})
		writeJSON(w, http.StatusOK, toLocalNodeDTO(roles))
	}
}
