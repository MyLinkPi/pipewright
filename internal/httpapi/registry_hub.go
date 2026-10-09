package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/registryhub"
	"github.com/huangchengsir/pipewright/internal/target"
)

// 内置本地 registry(registryhub):设置读写 / 栈部署 / 状态 / 保留清理 / daemon.json 手动下发。
//
// 铁律:
//   - PUT /settings/registry **只写库**,不部署、不下发;触碰机器的动作(deploy/daemon-apply/
//     prune)全部是显式 POST,且 daemon-apply **只处理请求里显式列出的目标**(勾选即范围)。
//   - deploy/daemon-apply/prune 为写方法,过 auth + CSRF;成功后写审计(detail 无凭据)。
//   - 执行失败(机器层)一律 200 + ok:false + 人读 error,不 500(对齐 server_ops 契约)。

// registryHubService 抽象 httpapi 所需的 registryhub 能力(*registryhub.Hub 实现;测试可 fake)。
type registryHubService interface {
	Get(ctx context.Context) (*registryhub.Config, error)
	Save(ctx context.Context, in registryhub.SaveInput) (*registryhub.Config, error)
	DeployStack(ctx context.Context) (*registryhub.DeployResult, error)
	Status(ctx context.Context) (*registryhub.Status, error)
	Prune(ctx context.Context, now time.Time) (*registryhub.PruneResult, error)
	ApplyDaemon(ctx context.Context, serverIDs []string, includeLocal bool) ([]registryhub.DaemonApplyResult, error)
	InspectDaemon(ctx context.Context, serverID string) (*registryhub.DaemonInspect, error)
	// ResolveDirs 返回制品/缓存存储的生效目录(UI 占位展示;配置为空时回显服务端默认)。
	ResolveDirs(cfg *registryhub.Config) (artifact, cache string)
}

// registryHubConfigDTO 是 GET/PUT /api/settings/registry 的响应体(冻结契约;无任何凭据)。
// suggestedAddr 为控制机出网网卡 IP(仅 UI 预填建议,不回写);artifactAddr/cacheAddr 是
// 生成 daemon.json / remoteTag 用的完整 host:port 展示值;artifactDataDir/cacheDataDir 是
// 配置值(空=默认),*Effective 是服务端解析后的生效目录(供占位展示)。
type registryHubConfigDTO struct {
	Enabled                  bool    `json:"enabled"`
	ExternalAddr             string  `json:"externalAddr"`
	SuggestedAddr            string  `json:"suggestedAddr"`
	UpstreamURL              string  `json:"upstreamUrl"`
	ArtifactPort             int     `json:"artifactPort"`
	CachePort                int     `json:"cachePort"`
	ArtifactDataDir          string  `json:"artifactDataDir"`
	CacheDataDir             string  `json:"cacheDataDir"`
	EffectiveArtifactDataDir string  `json:"effectiveArtifactDataDir"`
	EffectiveCacheDataDir    string  `json:"effectiveCacheDataDir"`
	TLSCertID                string  `json:"tlsCertId"`
	KeepPerProject           int     `json:"keepPerProject"`
	MaxAgeDays               int     `json:"maxAgeDays"`
	ArtifactAddr             string  `json:"artifactAddr"`
	CacheAddr                string  `json:"cacheAddr"`
	UpdatedAt                *string `json:"updatedAt"`
}

func toRegistryHubConfigDTO(c *registryhub.Config, svc registryHubService) registryHubConfigDTO {
	artifactDir, cacheDir := svc.ResolveDirs(c)
	dto := registryHubConfigDTO{
		Enabled:                  c.Enabled,
		ExternalAddr:             c.ExternalAddr,
		SuggestedAddr:            registryhub.DetectOutboundAddr(),
		UpstreamURL:              c.UpstreamURL,
		ArtifactPort:             c.ArtifactPort,
		CachePort:                c.CachePort,
		ArtifactDataDir:          c.ArtifactDataDir,
		CacheDataDir:             c.CacheDataDir,
		EffectiveArtifactDataDir: artifactDir,
		EffectiveCacheDataDir:    cacheDir,
		TLSCertID:                c.TLSCertID,
		KeepPerProject:           c.KeepPerProject,
		MaxAgeDays:               c.MaxAgeDays,
		ArtifactAddr:             c.ArtifactAddr(),
		CacheAddr:                c.CacheAddr(),
	}
	if c.UpdatedAt != nil {
		s := c.UpdatedAt.UTC().Format(time.RFC3339)
		dto.UpdatedAt = &s
	}
	return dto
}

// writeRegistryHubError 把领域错误映射为契约错误码/状态码。
func writeRegistryHubError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, registryhub.ErrInvalidAddr):
		writeError(w, http.StatusUnprocessableEntity, "invalid_registry_addr", "外部地址非法:须为纯 host/IP(不含协议/端口),启用时必填")
	case errors.Is(err, registryhub.ErrInvalidUpstream):
		writeError(w, http.StatusUnprocessableEntity, "invalid_registry_upstream", "上游地址非法:须为 http/https 且含主机名")
	case errors.Is(err, registryhub.ErrInvalidPort):
		writeError(w, http.StatusUnprocessableEntity, "invalid_registry_port", "端口非法:须为 1-65535,且制品/缓存端口不得相同")
	case errors.Is(err, registryhub.ErrInvalidDataDir):
		writeError(w, http.StatusUnprocessableEntity, "invalid_registry_data_dir", "存储目录非法:须为控制机上的绝对路径,制品/缓存目录不得相同或互为嵌套,也不得罩住栈基目录")
	case errors.Is(err, registryhub.ErrInvalidTLSCert):
		writeError(w, http.StatusUnprocessableEntity, "invalid_registry_tls", "TLS 证书非法:证书不存在,或其域名未覆盖外部地址(须公共可信,客户端才能零配置直连)")
	case errors.Is(err, registryhub.ErrDisabled):
		writeError(w, http.StatusUnprocessableEntity, "registry_disabled", "内置 registry 未启用:先在设置中开启并保存")
	case errors.Is(err, registryhub.ErrNoLocalDocker):
		writeError(w, http.StatusUnprocessableEntity, "no_local_docker", "控制机本机未检测到 docker / docker compose")
	case errors.Is(err, target.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "目标服务器不存在")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// makeGetRegistryHubHandler 返回 GET /api/settings/registry handler(只读 + 认证)。
// 无行 → 惰性默认(关 + 默认上游/端口),suggestedAddr 供 UI 预填。
func makeGetRegistryHubHandler(svc registryHubService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "内置 registry 服务未初始化")
			return
		}
		cfg, err := svc.Get(r.Context())
		if err != nil {
			writeRegistryHubError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, toRegistryHubConfigDTO(cfg, svc))
	}
}

// makeSaveRegistryHubHandler 返回 PUT /api/settings/registry handler(认证 + CSRF)。
// **只写库**:不部署栈、不下发 daemon.json、不触碰任何机器。校验失败 → 422 定位。
func makeSaveRegistryHubHandler(svc registryHubService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "内置 registry 服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			Enabled         bool   `json:"enabled"`
			ExternalAddr    string `json:"externalAddr"`
			UpstreamURL     string `json:"upstreamUrl"`
			ArtifactPort    int    `json:"artifactPort"`
			CachePort       int    `json:"cachePort"`
			ArtifactDataDir string `json:"artifactDataDir"`
			CacheDataDir    string `json:"cacheDataDir"`
			TLSCertID       string `json:"tlsCertId"`
			KeepPerProject  int    `json:"keepPerProject"`
			MaxAgeDays      int    `json:"maxAgeDays"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		cfg, err := svc.Save(r.Context(), registryhub.SaveInput{
			Enabled:         req.Enabled,
			ExternalAddr:    req.ExternalAddr,
			UpstreamURL:     req.UpstreamURL,
			ArtifactPort:    req.ArtifactPort,
			CachePort:       req.CachePort,
			ArtifactDataDir: req.ArtifactDataDir,
			CacheDataDir:    req.CacheDataDir,
			TLSCertID:       req.TLSCertID,
			KeepPerProject:  req.KeepPerProject,
			MaxAgeDays:      req.MaxAgeDays,
		})
		if err != nil {
			writeRegistryHubError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, toRegistryHubConfigDTO(cfg, svc))
	}
}

// makeDeployRegistryHandler 返回 POST /api/settings/registry/deploy handler(认证 + CSRF)。
// 在控制机本机部署/更新 registry 栈(compose up -d,声明式收敛)。机器层失败 → 200 + ok:false
// + 人读 error,不 500;成功/失败均写审计。
func makeDeployRegistryHandler(svc registryHubService, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "内置 registry 服务未初始化")
			return
		}
		res, err := svc.DeployStack(r.Context())
		if err != nil {
			writeRegistryHubError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor:      auditActor,
			Action:     audit.ActionRegistryDeploy,
			TargetType: audit.TargetRegistryHub,
			TargetID:   "stack",
			Detail:     map[string]any{"ok": res.OK, "error": truncateLog(res.Error, 512)},
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusOK, res)
	}
}

// makeRegistryStatusHandler 返回 GET /api/settings/registry/status handler(只读 + 认证)。
// 返回栈部署/容器/探活/存储占用现状(全 best-effort,绝不 500)。
func makeRegistryStatusHandler(svc registryHubService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "内置 registry 服务未初始化")
			return
		}
		st, err := svc.Status(r.Context())
		if err != nil {
			writeRegistryHubError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

// makeRegistryPruneHandler 返回 POST /api/settings/registry/prune handler(认证 + CSRF)。
// 立即按保留策略清理制品 registry(DELETE manifest + gc);失败 → 200 + ok:false + 人读 error。
func makeRegistryPruneHandler(svc registryHubService, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "内置 registry 服务未初始化")
			return
		}
		res, err := svc.Prune(r.Context(), time.Now())
		if err != nil {
			writeRegistryHubError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor:      auditActor,
			Action:     audit.ActionRegistryPrune,
			TargetType: audit.TargetRegistryHub,
			TargetID:   "prune",
			Detail:     map[string]any{"ok": res.OK, "deletedTags": res.DeletedTags, "repos": res.Repos, "error": truncateLog(res.Error, 512)},
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusOK, res)
	}
}

// daemonApplyResponse 是 POST /api/registry/daemon-apply 的响应体(冻结契约)。
// results 与请求目标一一对应(仅显式勾选的机器;每台独立成败)。
type daemonApplyResponse struct {
	Results []registryhub.DaemonApplyResult `json:"results"`
}

// makeDaemonApplyHandler 返回 POST /api/registry/daemon-apply handler(认证 + CSRF)。
// 请求 {serverIds:[], includeLocal};**只处理显式列出的目标**(手动勾选即全部范围,绝不扩大)。
// 任何机器层失败都落在对应 result 里(200),不 500;整体写一条审计(含逐台成败摘要)。
func makeDaemonApplyHandler(svc registryHubService, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "内置 registry 服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			ServerIDs    []string `json:"serverIds"`
			IncludeLocal bool     `json:"includeLocal"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		if len(req.ServerIDs) > 100 {
			writeError(w, http.StatusBadRequest, "bad_request", "单次下发目标过多(上限 100)")
			return
		}
		if len(req.ServerIDs) == 0 && !req.IncludeLocal {
			writeError(w, http.StatusBadRequest, "bad_request", "未选择任何目标(至少勾选一台服务器或控制机本机)")
			return
		}
		results, err := svc.ApplyDaemon(r.Context(), req.ServerIDs, req.IncludeLocal)
		if err != nil {
			writeRegistryHubError(w, err)
			return
		}
		okCount := 0
		for _, res := range results {
			if res.OK {
				okCount++
			}
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor:      auditActor,
			Action:     audit.ActionRegistryDaemonApply,
			TargetType: audit.TargetRegistryHub,
			TargetID:   "daemon",
			Detail: map[string]any{
				"serverIds": req.ServerIDs, "includeLocal": req.IncludeLocal,
				"total": len(results), "ok": okCount,
			},
			IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, daemonApplyResponse{Results: results})
	}
}

// makeDaemonInspectHandler 返回 GET /api/registry/inspect handler(只读 + 认证)。
// serverId=local 为控制机本机;返回该机当前生效镜像源与 daemon.json 原文(截断)。
func makeDaemonInspectHandler(svc registryHubService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "内置 registry 服务未初始化")
			return
		}
		serverID := r.URL.Query().Get("serverId")
		if serverID == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "缺少 serverId 参数(local=控制机本机)")
			return
		}
		ins, err := svc.InspectDaemon(r.Context(), serverID)
		if err != nil {
			writeRegistryHubError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, ins)
	}
}
