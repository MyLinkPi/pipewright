package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/huangchengsir/pipewright/internal/aptcacherng"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/target"
)

// 内置 apt 包缓存(aptcacherng):设置读写 / 栈部署 / 状态 / 缓存清理 / 镜像升级 /
// apt 代理片段手动下发。
//
// 铁律(与 registry_hub 一致):
//   - PUT /settings/apt-cache **只写库**,不部署、不下发;触碰机器的动作(deploy/upgrade-image/
//     clear-cache/proxy-apply)全部是显式 POST,且 proxy-apply **只处理请求里显式列出的目标**
//     (勾选即范围)。
//   - 写方法过 auth + CSRF;成功后写审计(detail 无凭据)。
//   - 执行失败(机器层)一律 200 + ok:false + 人读 error,不 500。

// aptCacherService 抽象 httpapi 所需的 aptcacherng 能力(*aptcacherng.Hub 实现;测试可 fake)。
type aptCacherService interface {
	Get(ctx context.Context) (*aptcacherng.Config, error)
	Save(ctx context.Context, in aptcacherng.SaveInput) (*aptcacherng.Config, error)
	DeployStack(ctx context.Context) (*aptcacherng.DeployResult, error)
	UpgradeImage(ctx context.Context) (*aptcacherng.ImageUpgradeResult, error)
	ClearCache(ctx context.Context) (*aptcacherng.DeployResult, error)
	Status(ctx context.Context) (*aptcacherng.Status, error)
	ApplyProxy(ctx context.Context, serverIDs []string, includeLocal bool) ([]aptcacherng.ProxyApplyResult, error)
	InspectProxy(ctx context.Context, serverID string) (*aptcacherng.ProxyInspect, error)
	// ResolveDir 返回缓存存储的生效目录(UI 占位展示;配置为空时回显服务端默认)。
	ResolveDir(cfg *aptcacherng.Config) string
}

// aptCacheConfigDTO 是 GET/PUT /api/settings/apt-cache 的响应体(冻结契约;无任何凭据)。
// suggestedAddr 为控制机出网网卡 IP(仅 UI 预填建议,不回写);addr 是生成代理片段用的
// 完整 host:port 展示值;dataDir 是配置值(空=默认),effectiveDataDir 是服务端解析后的
// 生效目录(供占位展示);tlsEnabled 是 tlsCertId 非空的派生便捷位。
type aptCacheConfigDTO struct {
	Enabled          bool    `json:"enabled"`
	ExternalAddr     string  `json:"externalAddr"`
	SuggestedAddr    string  `json:"suggestedAddr"`
	Port             int     `json:"port"`
	DataDir          string  `json:"dataDir"`
	EffectiveDataDir string  `json:"effectiveDataDir"`
	TLSCertID        string  `json:"tlsCertId"`
	TLSEnabled       bool    `json:"tlsEnabled"`
	Addr             string  `json:"addr"`
	UpdatedAt        *string `json:"updatedAt"`
}

func toAptCacheConfigDTO(c *aptcacherng.Config, svc aptCacherService) aptCacheConfigDTO {
	dto := aptCacheConfigDTO{
		Enabled:          c.Enabled,
		ExternalAddr:     c.ExternalAddr,
		SuggestedAddr:    aptcacherng.DetectOutboundAddr(),
		Port:             c.Port,
		DataDir:          c.DataDir,
		EffectiveDataDir: svc.ResolveDir(c),
		TLSCertID:        c.TLSCertID,
		TLSEnabled:       c.TLSEnabled(),
		Addr:             c.Addr(),
	}
	if c.UpdatedAt != nil {
		s := c.UpdatedAt.UTC().Format(time.RFC3339)
		dto.UpdatedAt = &s
	}
	return dto
}

// writeAptCacheError 把领域错误映射为契约错误码/状态码。
func writeAptCacheError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, aptcacherng.ErrInvalidAddr):
		writeError(w, http.StatusUnprocessableEntity, "invalid_apt_addr", "外部地址非法:须为纯 host/IP(不含协议/端口),启用时必填")
	case errors.Is(err, aptcacherng.ErrInvalidPort):
		writeError(w, http.StatusUnprocessableEntity, "invalid_apt_port", "端口非法:须为 1-65535")
	case errors.Is(err, aptcacherng.ErrInvalidDataDir):
		writeError(w, http.StatusUnprocessableEntity, "invalid_apt_data_dir", "缓存目录非法:须为控制机上的绝对路径,且不得罩住栈基目录")
	case errors.Is(err, aptcacherng.ErrInvalidTLSCert):
		writeError(w, http.StatusUnprocessableEntity, "invalid_apt_tls", "TLS 证书非法:证书不存在,或其域名未覆盖外部地址(须公共可信,apt 客户端才能零配置直连)")
	case errors.Is(err, aptcacherng.ErrDisabled):
		writeError(w, http.StatusUnprocessableEntity, "apt_cache_disabled", "内置 apt 缓存未启用:先在设置中开启并保存")
	case errors.Is(err, aptcacherng.ErrNoLocalDocker):
		writeError(w, http.StatusUnprocessableEntity, "no_local_docker", "控制机本机未检测到 docker / docker compose")
	case errors.Is(err, target.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "目标服务器不存在")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// makeGetAptCacheHandler 返回 GET /api/settings/apt-cache handler(只读 + 认证)。
// 无行 → 惰性默认(关 + 默认端口),suggestedAddr 供 UI 预填。
func makeGetAptCacheHandler(svc aptCacherService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "内置 apt 缓存服务未初始化")
			return
		}
		cfg, err := svc.Get(r.Context())
		if err != nil {
			writeAptCacheError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, toAptCacheConfigDTO(cfg, svc))
	}
}

// makeSaveAptCacheHandler 返回 PUT /api/settings/apt-cache handler(认证 + CSRF)。
// **只写库**:不部署栈、不下发代理片段、不触碰任何机器。校验失败 → 422 定位。
func makeSaveAptCacheHandler(svc aptCacherService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "内置 apt 缓存服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			Enabled      bool   `json:"enabled"`
			ExternalAddr string `json:"externalAddr"`
			Port         int    `json:"port"`
			DataDir      string `json:"dataDir"`
			TLSCertID    string `json:"tlsCertId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		cfg, err := svc.Save(r.Context(), aptcacherng.SaveInput{
			Enabled:      req.Enabled,
			ExternalAddr: req.ExternalAddr,
			Port:         req.Port,
			DataDir:      req.DataDir,
			TLSCertID:    req.TLSCertID,
		})
		if err != nil {
			writeAptCacheError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, toAptCacheConfigDTO(cfg, svc))
	}
}

// makeDeployAptCacheHandler 返回 POST /api/settings/apt-cache/deploy handler(认证 + CSRF)。
// 在控制机本机部署/更新 apt 缓存栈(compose up -d,声明式收敛)。机器层失败 → 200 + ok:false
// + 人读 error,不 500;成功/失败均写审计。
func makeDeployAptCacheHandler(svc aptCacherService, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "内置 apt 缓存服务未初始化")
			return
		}
		res, err := svc.DeployStack(r.Context())
		if err != nil {
			writeAptCacheError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor:      auditActor,
			Action:     audit.ActionAptCacheDeploy,
			TargetType: audit.TargetAptCacher,
			TargetID:   "stack",
			Detail:     map[string]any{"ok": res.OK, "error": truncateLog(res.Error, 512)},
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusOK, res)
	}
}

// makeUpgradeAptCacheImageHandler 返回 POST /api/settings/apt-cache/upgrade-image handler(认证 + CSRF)。
// 升级栈镜像(apt-cacher-ng;TLS 态含 nginx 前端):pull → 比对本地镜像 ID,变了才 DeployStack
// 重建容器(数据在宿主卷,重建不丢);未变 → ok:true changed:false(不动容器)。机器层失败 →
// 200 + ok:false + 人读 error,不 500;成功/失败均写审计。
func makeUpgradeAptCacheImageHandler(svc aptCacherService, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "内置 apt 缓存服务未初始化")
			return
		}
		res, err := svc.UpgradeImage(r.Context())
		if err != nil {
			writeAptCacheError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor:      auditActor,
			Action:     audit.ActionAptCacheUpgradeImage,
			TargetType: audit.TargetAptCacher,
			TargetID:   "image",
			Detail:     map[string]any{"ok": res.OK, "changed": res.Changed, "image": res.Image, "error": truncateLog(res.Error, 512)},
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusOK, res)
	}
}

// makeAptCacheStatusHandler 返回 GET /api/settings/apt-cache/status handler(只读 + 认证)。
// 返回栈部署/容器/探活/缓存占用现状(全 best-effort,绝不 500)。
func makeAptCacheStatusHandler(svc aptCacherService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "内置 apt 缓存服务未初始化")
			return
		}
		st, err := svc.Status(r.Context())
		if err != nil {
			writeAptCacheError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

// makeClearAptCacheHandler 返回 POST /api/settings/apt-cache/clear-cache handler(认证 + CSRF)。
// 手动清空 apt 缓存(停容器 → 删缓存目录 → 重建;缓存可再生)。失败 → 200 + ok:false + 人读 error。
func makeClearAptCacheHandler(svc aptCacherService, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "内置 apt 缓存服务未初始化")
			return
		}
		res, err := svc.ClearCache(r.Context())
		if err != nil {
			writeAptCacheError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor:      auditActor,
			Action:     audit.ActionAptCacheClear,
			TargetType: audit.TargetAptCacher,
			TargetID:   "cache",
			Detail:     map[string]any{"ok": res.OK, "error": truncateLog(res.Error, 512)},
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusOK, res)
	}
}

// aptProxyApplyResponse 是 POST /api/apt-cache/proxy-apply 的响应体(冻结契约)。
// results 与请求目标一一对应(仅显式勾选的机器;每台独立成败)。
type aptProxyApplyResponse struct {
	Results []aptcacherng.ProxyApplyResult `json:"results"`
}

// makeAptProxyApplyHandler 返回 POST /api/apt-cache/proxy-apply handler(认证 + CSRF)。
// 请求 {serverIds:[], includeLocal};**只处理显式列出的目标**(手动勾选即全部范围,绝不扩大)。
// 任何机器层失败都落在对应 result 里(200),不 500;整体写一条审计(含逐台成败摘要)。
func makeAptProxyApplyHandler(svc aptCacherService, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "内置 apt 缓存服务未初始化")
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
		results, err := svc.ApplyProxy(r.Context(), req.ServerIDs, req.IncludeLocal)
		if err != nil {
			writeAptCacheError(w, err)
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
			Action:     audit.ActionAptCacheProxyApply,
			TargetType: audit.TargetAptCacher,
			TargetID:   "proxy",
			Detail: map[string]any{
				"serverIds": req.ServerIDs, "includeLocal": req.IncludeLocal,
				"total": len(results), "ok": okCount,
			},
			IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, aptProxyApplyResponse{Results: results})
	}
}

// makeAptProxyInspectHandler 返回 GET /api/apt-cache/inspect handler(只读 + 认证)。
// serverId=local 为控制机本机;返回该机当前生效代理值与片段文件原文(截断)。
func makeAptProxyInspectHandler(svc aptCacherService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "内置 apt 缓存服务未初始化")
			return
		}
		serverID := r.URL.Query().Get("serverId")
		if serverID == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "缺少 serverId 参数(local=控制机本机)")
			return
		}
		ins, err := svc.InspectProxy(r.Context(), serverID)
		if err != nil {
			writeAptCacheError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, ins)
	}
}
