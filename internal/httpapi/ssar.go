package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/ssar"
	"github.com/huangchengsir/pipewright/internal/target"
)

// 机器监控(基于自维护 fork MyLinkPi/ssar):安装分发 + 平台采样总览 + 机器侧细粒度
// 历史按需查询。领域逻辑在 internal/ssar(下载器/部署器/采集/查询/存储),此处仅
// HTTP 适配;服务器列表经 target.Service(与 servers.go 同源)。
//
// 路由(router.go 仅 append;GET 过 auth,POST 过 auth + CSRF):
//   - GET  /api/ssar/overview                                每台安装/采集器状态 + 平台包缓存
//   - POST /api/ssar/download                                触发安装包下载(202 异步,审计)
//   - POST /api/ssar/install                                 部署到 {serverIds}(逐台成败,审计)
//   - GET  /api/ssar/query?serverId=&view=&minutes=&step=    机器侧细粒度历史(白名单视图)
//   - GET  /api/ssar/history?serverId=&hours=                平台采样时序(总览图表)
//
// 容错:单台探测/安装失败只落在该台 DTO 的 error 字段,不连累其它台、不 500。

// ssarStatusDTO 是 overview 中单台的状态(冻结契约)。
type ssarStatusDTO struct {
	ServerID        string `json:"serverId"`
	Name            string `json:"name"`
	Installed       bool   `json:"installed"`
	Version         string `json:"version"` // 已装 ssar 版本(如 "1.0.5";空 = 未知)
	CollectorActive bool   `json:"collectorActive"`
	// LastSampleAt 是平台侧最近一次入库采样(RFC3339;空 = 从未采到)。
	LastSampleAt string `json:"lastSampleAt"`
	// Error 为该台探测失败的人读原因(成功时为空)。
	Error string `json:"error"`
}

// ssarOverviewDTO 是 GET /api/ssar/overview 响应体(冻结契约)。
type ssarOverviewDTO struct {
	Binaries ssar.BinStatus  `json:"binaries"`
	Items    []ssarStatusDTO `json:"items"`
}

// makeSsarOverviewHandler 返回 GET /api/ssar/overview:逐台状态(经 TTL 缓存探测,
// 单台失败不连累)+ 平台安装包缓存状态 + 各台最近采样时间。
func makeSsarOverviewHandler(svc ssar.Service, servers target.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil || servers == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "机器监控服务未初始化")
			return
		}
		list, err := servers.List(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		items := make([]ssarStatusDTO, len(list))
		for i, srv := range list {
			dto := ssarStatusDTO{ServerID: srv.ID, Name: srv.Name}
			if st, serr := svc.Status(r.Context(), srv.ID); serr == nil {
				dto.Installed = st.Installed
				dto.Version = st.Version
				dto.CollectorActive = st.CollectorActive
				dto.Error = st.Error
			} else {
				dto.Error = "状态探测失败"
			}
			if at, lerr := svc.LastSampleAt(r.Context(), srv.ID); lerr == nil && !at.IsZero() {
				dto.LastSampleAt = at.UTC().Format(time.RFC3339)
			}
			items[i] = dto
		}
		writeJSON(w, http.StatusOK, ssarOverviewDTO{Binaries: svc.BinStatus(), Items: items})
	}
}

// makeSsarDownloadHandler 返回 POST /api/ssar/download:异步触发安装包下载(单飞;
// 202;进度经 overview 的 binaries.downloading 轮询)。审计动作 ssar_download。
func makeSsarDownloadHandler(svc ssar.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "机器监控服务未初始化")
			return
		}
		svc.TriggerDownload(r.Context())
		recordAudit(r.Context(), aud, audit.Entry{
			Actor:      auditActor,
			Action:     audit.ActionSsarDownload,
			TargetType: audit.TargetSsar,
		})
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
	}
}

// makeSsarInstallHandler 返回 POST /api/ssar/install:body {serverIds:[...]},
// 逐台部署(有界并发,单台失败不连累),同步返回逐台结果。审计动作 ssar_install。
func makeSsarInstallHandler(svc ssar.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "机器监控服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			ServerIDs []string `json:"serverIds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		if len(req.ServerIDs) == 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "请选择要安装的服务器")
			return
		}
		if len(req.ServerIDs) > 100 {
			writeError(w, http.StatusBadRequest, "bad_request", "单次最多安装 100 台")
			return
		}
		results := svc.Install(r.Context(), req.ServerIDs)
		recordAudit(r.Context(), aud, audit.Entry{
			Actor:      auditActor,
			Action:     audit.ActionSsarInstall,
			TargetType: audit.TargetSsar,
			Detail:     map[string]any{"servers": req.ServerIDs, "ok": countSsarOK(results)},
		})
		writeJSON(w, http.StatusOK, map[string]any{"items": results})
	}
}

// countSsarOK 统计安装成功台数(审计 detail 用)。
func countSsarOK(results []ssar.InstallResult) int {
	n := 0
	for _, r := range results {
		if r.OK {
			n++
		}
	}
	return n
}

// makeSsarQueryHandler 返回 GET /api/ssar/query?serverId=&view=&minutes=&step=
// (认证,只读)。view 白名单 / 区间校验在领域层(ErrInvalidQuery → 400);
// 机器不可达 / ssar 不可用 → 502 人读;无数据 → 空 rows(不报错)。
func makeSsarQueryHandler(svc ssar.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "机器监控服务未初始化")
			return
		}
		serverID := r.URL.Query().Get("serverId")
		if serverID == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "缺少 serverId 参数")
			return
		}
		view := r.URL.Query().Get("view")
		minutes, err := strconv.Atoi(r.URL.Query().Get("minutes"))
		if err != nil || minutes < 1 {
			minutes = 60
		}
		step, err := strconv.Atoi(r.URL.Query().Get("step"))
		if err != nil || step < 1 {
			step = 1
		}
		out, qerr := svc.Query(r.Context(), serverID, view, minutes, step)
		if qerr != nil {
			if errors.Is(qerr, ssar.ErrInvalidQuery) {
				writeError(w, http.StatusBadRequest, "bad_request", qerr.Error())
				return
			}
			writeError(w, http.StatusBadGateway, "ssar_query_failed", "查询失败:机器不可达或 ssar 不可用")
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// makeSsarHistoryHandler 返回 GET /api/ssar/history?serverId=&hours=(认证,只读):
// 平台采样时序(升序;供总览图表)。hours 缺省 24,上限 7 天(平台采样保留窗)。
func makeSsarHistoryHandler(svc ssar.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "机器监控服务未初始化")
			return
		}
		serverID := r.URL.Query().Get("serverId")
		if serverID == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "缺少 serverId 参数")
			return
		}
		hours := 24
		if v := r.URL.Query().Get("hours"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 24*7 {
				hours = n
			}
		}
		points, err := svc.History(r.Context(), serverID, time.Now().UTC().Add(-time.Duration(hours)*time.Hour))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"points": points})
	}
}
