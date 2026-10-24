package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/ai"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/secscan"
)

// 安全中心(Trivy/Nuclei 漏洞与暴露面扫描)。挂载 /api/security/*(admin 组:
// auth + CSRF + 审计)。扫描经 SSH 在目标机 docker run 扫描器容器,run 异步执行;
// HTML 审计报告 GET 下载(cookie 鉴权,免 CSRF,与构建产物下载同姿态)。
//
// 审计 action:security.*;detail 绝无 token(token 经 vault 密封,仅记接入参数)。

const (
	auditActionSecScannerCreate = "security.scanner.create"
	auditActionSecScannerUpdate = "security.scanner.update"
	auditActionSecScannerDelete = "security.scanner.delete"
	auditActionSecScannerTest   = "security.scanner.test"
	auditActionSecScannerReg    = "security.scanner.register" // 应用市场部署联动(apps.go 调)
	auditActionSecScanRun       = "security.scan.run"
	auditActionSecScanFleet     = "security.scan.fleet"
	auditActionSecScheduleSave  = "security.schedule.update"
	auditActionSecReportExport  = "security.report.export"
	auditActionSecExplain       = "security.finding.explain"
	auditTargetSecurity         = "security"
)

// writeSecurityError 把安全中心领域错误映射为契约错误码/状态码。
func writeSecurityError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, secscan.ErrNotFound):
		writeError(w, http.StatusNotFound, "security_not_found", "扫描器或扫描记录不存在")
	case errors.Is(err, secscan.ErrEmptyName), errors.Is(err, secscan.ErrEmptyHost),
		errors.Is(err, secscan.ErrInvalidPort), errors.Is(err, secscan.ErrInvalidInput),
		errors.Is(err, secscan.ErrInvalidKind):
		writeError(w, http.StatusBadRequest, "invalid_security_input", "参数非法:"+err.Error())
	case errors.Is(err, secscan.ErrEndpointTaken):
		writeError(w, http.StatusConflict, "security_endpoint_taken", "同类型同地址的扫描器已登记")
	case errors.Is(err, secscan.ErrVaultUnconfigured):
		writeError(w, http.StatusServiceUnavailable, "vault_unconfigured", "保险库未配置 master key,无法保存令牌")
	case errors.Is(err, secscan.ErrRunnerUnavailable):
		writeError(w, http.StatusServiceUnavailable, "security_runner_unavailable", "扫描执行器未装配(裁剪部署)")
	case errors.Is(err, secscan.ErrServerNotFound):
		writeError(w, http.StatusNotFound, "server_not_found", "目标服务器不存在")
	case errors.Is(err, secscan.ErrServerNotTarget):
		writeError(w, http.StatusUnprocessableEntity, "pve_node_not_target", "PVE 集群物理机不是扫描目标")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// secScannerDTO 是扫描器实例对外响应体(冻结契约;绝无 token)。
type secScannerDTO struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	Name            string `json:"name"`
	Host            string `json:"host"`
	Port            int    `json:"port"`
	TokenConfigured bool   `json:"tokenConfigured"`
	Source          string `json:"source"`
	ServerID        string `json:"serverId,omitempty"`
	StackName       string `json:"stackName,omitempty"`
	CreatedAt       string `json:"createdAt"`
	UpdatedAt       string `json:"updatedAt"`
}

func toSecScannerDTO(i secscan.Instance) secScannerDTO {
	return secScannerDTO{
		ID: i.ID, Kind: i.Kind, Name: i.Name, Host: i.Host, Port: i.Port,
		TokenConfigured: i.TokenConfigured, Source: i.Source,
		ServerID: i.ServerID, StackName: i.StackName,
		CreatedAt: i.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: i.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// secRunDTO 是扫描运行的对外响应体(冻结契约)。
type secRunDTO struct {
	ID          string          `json:"id"`
	ServerID    string          `json:"serverId"`
	ServerName  string          `json:"serverName"`
	ScanType    string          `json:"scanType"`
	Status      string          `json:"status"`
	Trigger     string          `json:"trigger,omitempty"`
	Summary     secscan.Summary `json:"summary"`
	Error       string          `json:"error,omitempty"`
	StartedAt   string          `json:"startedAt"`
	FinishedAt  string          `json:"finishedAt,omitempty"`
	FindingsURL string          `json:"findingsUrl,omitempty"` // 明细抽屉数据源(GET .../findings)
}

func toSecRunDTO(r secscan.Run) secRunDTO {
	dto := secRunDTO{
		ID: r.ID, ServerID: r.ServerID, ServerName: r.ServerName,
		ScanType: r.ScanType, Status: r.Status, Trigger: r.Trigger,
		Summary: r.Summary, Error: r.Error,
		StartedAt: r.StartedAt.UTC().Format(time.RFC3339),
	}
	if !r.FinishedAt.IsZero() {
		dto.FinishedAt = r.FinishedAt.UTC().Format(time.RFC3339)
		dto.FindingsURL = "/api/security/scans/" + r.ID + "/findings"
	}
	return dto
}

// secOverviewDTO 是安全中心概览响应体。
type secOverviewDTO struct {
	Servers []secOverviewServerDTO `json:"servers"`
	Totals  secscan.Summary        `json:"totals"`
	Scanned int                    `json:"scanned"`
	// ServerTotal 与 LastScanAtRFC 系列字段前端可自行取;保持小写驼峰冻结契约。
	ServerTotal int    `json:"serverTotal"`
	LastScanAt  string `json:"lastScanAt,omitempty"`
}

type secOverviewServerDTO struct {
	ServerID   string            `json:"serverId"`
	ServerName string            `json:"serverName"`
	Host       string            `json:"host"`
	Trivy      *secRunLiteDTO    `json:"trivy,omitempty"`
	Nuclei     *secRunLiteDTO    `json:"nuclei,omitempty"`
	Totals     *secscan.Summary  `json:"totals,omitempty"` // 该机最新 done 结果合计(徽标用)
}

type secRunLiteDTO struct {
	RunID      string          `json:"runId"`
	ScanType   string          `json:"scanType"`
	Status     string          `json:"status"`
	Summary    secscan.Summary `json:"summary"`
	Error      string          `json:"error,omitempty"`
	FinishedAt string          `json:"finishedAt,omitempty"`
}

func toSecRunLiteDTO(r *secscan.RunLite) *secRunLiteDTO {
	if r == nil {
		return nil
	}
	dto := &secRunLiteDTO{
		RunID: r.RunID, ScanType: r.ScanType, Status: r.Status, Summary: r.Summary, Error: r.Error,
	}
	if !r.FinishedAt.IsZero() {
		dto.FinishedAt = r.FinishedAt.UTC().Format(time.RFC3339)
	}
	return dto
}

// secScheduleDTO 是定时规则响应体。
type secScheduleDTO struct {
	Enabled   bool     `json:"enabled"`
	Expression string  `json:"expression"`
	ScanTypes []string `json:"scanTypes"`
	LastRunAt string   `json:"lastRunAt,omitempty"`
	UpdatedAt string   `json:"updatedAt"`
}

// mountSecurity 挂载 /api/security/*(admin 组)。
// securityExplainer 是漏洞 AI 解读的窄依赖(ai.FindingExplainer;未装配 → 端点 503 引导配置)。
type securityExplainer = ai.FindingExplainer

// mountSecurity 挂载 /api/security/*(admin 组)。explainer 可为 nil(AI 未注入 → 解读端点 503)。
func mountSecurity(ar chi.Router, svc secscan.Service, aud audit.Recorder, explainer securityExplainer) {
	ar.Get("/security/overview", makeSecurityOverviewHandler(svc))
	ar.Get("/security/scanners", makeListSecScannersHandler(svc))
	ar.Post("/security/scanners", makeCreateSecScannerHandler(svc, aud))
	ar.Post("/security/scanners/test", makeTestSecScannerDirectHandler(svc, aud))
	ar.Put("/security/scanners/{id}", makeUpdateSecScannerHandler(svc, aud))
	ar.Delete("/security/scanners/{id}", makeDeleteSecScannerHandler(svc, aud))
	ar.Post("/security/scanners/{id}/test", makeTestSecScannerHandler(svc, aud))
	ar.Post("/security/scans", makeStartScanHandler(svc, aud))
	ar.Post("/security/scans/fleet", makeStartFleetScanHandler(svc, aud))
	ar.Get("/security/scans", makeListSecRunsHandler(svc))
	ar.Get("/security/scans/{id}/findings", makeRunFindingsHandler(svc))
	ar.Get("/security/schedule", makeGetScheduleHandler(svc))
	ar.Put("/security/schedule", makeSaveScheduleHandler(svc, aud))
	ar.Get("/security/report", makeSecurityReportHandler(svc, aud))
	ar.Post("/security/findings/explain", makeExplainFindingHandler(explainer, aud))
	ar.Get("/security/waf", makeWafOverviewHandler(svc))
}

// makeSecurityOverviewHandler 返回 GET /api/security/overview。
func makeSecurityOverviewHandler(svc secscan.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ov, err := svc.Overview(r.Context())
		if err != nil {
			writeSecurityError(w, err)
			return
		}
		out := secOverviewDTO{Totals: ov.Totals, Scanned: ov.Scanned, ServerTotal: len(ov.Servers)}
		if !ov.LastScanAt.IsZero() {
			out.LastScanAt = ov.LastScanAt.UTC().Format(time.RFC3339)
		}
		out.Servers = make([]secOverviewServerDTO, 0, len(ov.Servers))
		for _, srv := range ov.Servers {
			item := secOverviewServerDTO{
				ServerID: srv.ServerID, ServerName: srv.ServerName, Host: srv.Host,
				Trivy: toSecRunLiteDTO(srv.Trivy), Nuclei: toSecRunLiteDTO(srv.Nuclei),
			}
			// 徽标合计:该机最新 done 结果的 trivy+nuclei 合计(running/failed 不计入)。
			if (srv.Trivy != nil && srv.Trivy.Status == secscan.StatusDone) || (srv.Nuclei != nil && srv.Nuclei.Status == secscan.StatusDone) {
				total := secscan.Summary{}
				if srv.Trivy != nil && srv.Trivy.Status == secscan.StatusDone {
					total.Add(srv.Trivy.Summary)
				}
				if srv.Nuclei != nil && srv.Nuclei.Status == secscan.StatusDone {
					total.Add(srv.Nuclei.Summary)
				}
				item.Totals = &total
			}
			out.Servers = append(out.Servers, item)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// makeListSecScannersHandler 返回 GET /api/security/scanners → { items: [...] }。
func makeListSecScannersHandler(svc secscan.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := svc.ListScanners(r.Context())
		if err != nil {
			writeSecurityError(w, err)
			return
		}
		items := make([]secScannerDTO, 0, len(list))
		for _, i := range list {
			items = append(items, toSecScannerDTO(i))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

// makeCreateSecScannerHandler 返回 POST /api/security/scanners(手动登记外部服务)。
func makeCreateSecScannerHandler(svc secscan.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		Name  string `json:"name"`
		Host  string `json:"host"`
		Port  int    `json:"port"`
		Token string `json:"token,omitempty"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var req request
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		inst, err := svc.CreateScanner(r.Context(), secscan.CreateInput{Name: req.Name, Host: req.Host, Port: req.Port, Token: req.Token})
		if err != nil {
			writeSecurityError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSecScannerCreate, TargetType: auditTargetSecurity,
			TargetID: inst.ID, Detail: map[string]any{"kind": inst.Kind, "host": inst.Host, "port": inst.Port}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusCreated, toSecScannerDTO(*inst))
	}
}

// makeUpdateSecScannerHandler 返回 PUT /api/security/scanners/{id}。
func makeUpdateSecScannerHandler(svc secscan.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		Name  *string `json:"name"`
		Host  *string `json:"host"`
		Port  *int    `json:"port"`
		Token *string `json:"token"` // 非 nil 即轮换;空串 = 清除
	}
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var req request
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		inst, err := svc.UpdateScanner(r.Context(), chi.URLParam(r, "id"), secscan.UpdateInput{
			Name: req.Name, Host: req.Host, Port: req.Port, Token: req.Token,
		})
		if err != nil {
			writeSecurityError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSecScannerUpdate, TargetType: auditTargetSecurity,
			TargetID: inst.ID, Detail: map[string]any{"host": inst.Host, "port": inst.Port}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, toSecScannerDTO(*inst))
	}
}

// makeDeleteSecScannerHandler 返回 DELETE /api/security/scanners/{id}。
func makeDeleteSecScannerHandler(svc secscan.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if err := svc.DeleteScanner(r.Context(), id); err != nil {
			writeSecurityError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSecScannerDelete, TargetType: auditTargetSecurity,
			TargetID: id, Detail: map[string]any{"ok": true}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// makeTestSecScannerHandler 返回 POST /api/security/scanners/{id}/test。
func makeTestSecScannerHandler(svc secscan.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		res, err := svc.TestScanner(r.Context(), id)
		if err != nil {
			writeSecurityError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSecScannerTest, TargetType: auditTargetSecurity,
			TargetID: id, Detail: map[string]any{"ok": res.OK}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": res.OK, "latencyMs": res.LatencyMs, "detail": res.Detail})
	}
}

// makeTestSecScannerDirectHandler 返回 POST /api/security/scanners/test(保存前直连测试;
// token 用完即弃,不落审计参数)。
func makeTestSecScannerDirectHandler(svc secscan.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		Host  string `json:"host"`
		Port  int    `json:"port"`
		Token string `json:"token,omitempty"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var req request
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		res, err := svc.TestScannerDirect(r.Context(), secscan.ConnectInput{Host: req.Host, Port: req.Port, Token: req.Token})
		if err != nil {
			writeSecurityError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSecScannerTest, TargetType: auditTargetSecurity,
			Detail: map[string]any{"ok": res.OK, "direct": true, "host": req.Host, "port": req.Port}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": res.OK, "latencyMs": res.LatencyMs, "detail": res.Detail})
	}
}

// makeStartScanHandler 返回 POST /api/security/scans {serverId, scanType, targets?}。
// 异步:立即返回 running run;结果经 overview/history/findings 轮询。
func makeStartScanHandler(svc secscan.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		ServerID string   `json:"serverId"`
		ScanType string   `json:"scanType"`
		Targets  []string `json:"targets,omitempty"` // 仅 nuclei:探测目标 URL
	}
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var req request
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		run, err := svc.StartScan(r.Context(), secscan.ScanInput{
			ServerID: req.ServerID, ScanType: req.ScanType, Targets: req.Targets, Trigger: secscan.TriggerManual,
		})
		if err != nil {
			writeSecurityError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSecScanRun, TargetType: audit.TargetServer,
			TargetID: req.ServerID, Detail: map[string]any{"scanType": req.ScanType, "runId": run.ID, "ok": true}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusAccepted, toSecRunDTO(*run))
	}
}

// makeStartFleetScanHandler 返回 POST /api/security/scans/fleet {scanTypes: []}。
func makeStartFleetScanHandler(svc secscan.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		ScanTypes []string `json:"scanTypes"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var req request
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		runs, err := svc.StartFleetScan(r.Context(), req.ScanTypes, secscan.TriggerFleet)
		if err != nil {
			writeSecurityError(w, err)
			return
		}
		items := make([]secRunDTO, 0, len(runs))
		for i := range runs {
			items = append(items, toSecRunDTO(runs[i]))
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSecScanFleet, TargetType: auditTargetSecurity,
			Detail: map[string]any{"scanTypes": req.ScanTypes, "runs": len(items), "ok": true}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusAccepted, map[string]any{"items": items})
	}
}

// makeListSecRunsHandler 返回 GET /api/security/scans?serverId=&scanType=&limit=。
func makeListSecRunsHandler(svc secscan.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		runs, err := svc.ListRuns(r.Context(), secscan.RunFilter{
			ServerID: r.URL.Query().Get("serverId"),
			ScanType: r.URL.Query().Get("scanType"),
			Limit:    limit,
		})
		if err != nil {
			writeSecurityError(w, err)
			return
		}
		items := make([]secRunDTO, 0, len(runs))
		for i := range runs {
			items = append(items, toSecRunDTO(runs[i]))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

// makeRunFindingsHandler 返回 GET /api/security/scans/{id}/findings
// ?severity=&source=&q=&offset=&limit=(内存过滤分页)。
func makeRunFindingsHandler(svc secscan.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		offset, _ := strconv.Atoi(q.Get("offset"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		page, err := svc.RunFindings(r.Context(), chi.URLParam(r, "id"), secscan.FindingFilter{
			Severity: q.Get("severity"), Source: q.Get("source"), Query: q.Get("q"),
			Offset: offset, Limit: limit,
		})
		if err != nil {
			writeSecurityError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, page)
	}
}

// makeGetScheduleHandler 返回 GET /api/security/schedule。
func makeGetScheduleHandler(svc secscan.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sch, err := svc.GetSchedule(r.Context())
		if err != nil {
			writeSecurityError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, toSecScheduleDTO(*sch))
	}
}

// toSecScheduleDTO 把领域 Schedule 转 DTO。
func toSecScheduleDTO(s secscan.Schedule) secScheduleDTO {
	dto := secScheduleDTO{
		Enabled: s.Enabled, Expression: s.Expression, ScanTypes: s.ScanTypes,
		UpdatedAt: s.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if !s.LastRunAt.IsZero() {
		dto.LastRunAt = s.LastRunAt.UTC().Format(time.RFC3339)
	}
	return dto
}

// makeSaveScheduleHandler 返回 PUT /api/security/schedule {enabled, expression, scanTypes}。
func makeSaveScheduleHandler(svc secscan.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		Enabled    bool     `json:"enabled"`
		Expression string   `json:"expression"`
		ScanTypes  []string `json:"scanTypes"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var req request
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		sch, err := svc.SaveSchedule(r.Context(), secscan.ScheduleInput{
			Enabled: req.Enabled, Expression: req.Expression, ScanTypes: req.ScanTypes,
		})
		if err != nil {
			writeSecurityError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSecScheduleSave, TargetType: auditTargetSecurity,
			Detail: map[string]any{"enabled": sch.Enabled, "expression": sch.Expression, "scanTypes": sch.ScanTypes}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, toSecScheduleDTO(*sch))
	}
}

// decodeJSON 是安全中心 handler 共用的请求体解码(Body 已由调用方 MaxBytesReader 封顶)。
func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// ---------- 报告(HTML;独立文件 security_report.go 承载模板与渲染) ----------

// makeSecurityReportHandler 返回 GET /api/security/report?serverIds=a,b,c&format=html
// (serverIds 空 = 全部纳管服务器;兼容旧单机参数 serverId)。GET 免 CSRF(cookie 鉴权)。
func makeSecurityReportHandler(svc secscan.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if f := strings.ToLower(r.URL.Query().Get("format")); f != "" && f != "html" {
			writeError(w, http.StatusBadRequest, "invalid_report_format", "暂仅支持 HTML 报告")
			return
		}
		// 机器范围:serverIds 逗号分隔多选;单值 serverId 兼容;都空 = 全员。
		// 去重去空、上限 100(防超长 query;机器再多也该分批导出)。
		seen := map[string]bool{}
		var serverIDs []string
		addIDs := func(raw string) {
			for _, id := range strings.Split(raw, ",") {
				id = strings.TrimSpace(id)
				if id == "" || seen[id] || len(serverIDs) >= 100 {
					continue
				}
				seen[id] = true
				serverIDs = append(serverIDs, id)
			}
		}
		addIDs(r.URL.Query().Get("serverIds"))
		addIDs(r.URL.Query().Get("serverId"))
		reports, err := svc.CollectReport(r.Context(), serverIDs)
		if err != nil {
			writeSecurityError(w, err)
			return
		}
		scope := "fleet"
		switch {
		case len(serverIDs) == 1:
			scope = "server"
		case len(serverIDs) > 1:
			scope = "custom"
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSecReportExport, TargetType: auditTargetSecurity,
			Detail: map[string]any{"scope": scope, "servers": len(reports)}, IP: clientIP(r),
		})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		name := fmt.Sprintf("pipewright-security-report-%s.html", time.Now().UTC().Format("20060102-150405"))
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
		if err := renderSecurityReport(w, reports, time.Now().UTC(), scope); err != nil {
			// 头已写,无法再改状态码;记录到响应尾即可(报告为流式渲染,极小概率发生)。
			_, _ = w.Write([]byte("\n<!-- render error: " + err.Error() + " -->"))
		}
	}
}

// makeExplainFindingHandler 返回 POST /api/security/findings/explain:把一条扫描发现交
// AI 生成人读解读(Markdown)。入参就是前端明细行持有的公开语义字段(不回查 run,避免
// 后端再解析 artifact);主机地址绝不入 prompt(领域层出网纪律)。AI 未配置/未注入 →
// 503 引导到「设置 → AI」;审计只记漏洞编号。
func makeExplainFindingHandler(explainer ai.FindingExplainer, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		ScanType         string `json:"scanType"`
		Source           string `json:"source"`
		Severity         string `json:"severity"`
		ID               string `json:"id"`
		Title            string `json:"title"`
		Target           string `json:"target"`
		InstalledVersion string `json:"installedVersion"`
		FixedVersion     string `json:"fixedVersion"`
		URL              string `json:"url"`
		ServerName       string `json:"serverName"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if explainer == nil {
			writeError(w, http.StatusServiceUnavailable, "ai_explain_unavailable", "AI 服务未装配,解读不可用")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var req request
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		// 字段封顶(防异常长字段进 prompt);控制字符清洗为空格。
		clamp := func(v string, n int) string {
			v = strings.Map(func(rr rune) rune {
				if rr == '\n' || rr == '\r' || rr == '\t' {
					return ' '
				}
				return rr
			}, v)
			if len([]rune(v)) > n {
				return string([]rune(v)[:n])
			}
			return v
		}
		if strings.TrimSpace(req.ID) == "" {
			writeError(w, http.StatusBadRequest, "invalid_security_input", "缺少漏洞编号")
			return
		}
		in := ai.ExplainFindingInput{
			ScanType: clamp(req.ScanType, 16), Source: clamp(req.Source, 256), Severity: clamp(req.Severity, 16),
			VulnID: clamp(req.ID, 128), Title: clamp(req.Title, 512), Target: clamp(req.Target, 256),
			InstalledVersion: clamp(req.InstalledVersion, 128), FixedVersion: clamp(req.FixedVersion, 128),
			URL: clamp(req.URL, 512), ServerName: clamp(req.ServerName, 128),
		}
		text, err := explainer.ExplainFinding(r.Context(), in)
		if err != nil {
			if errors.Is(err, ai.ErrAINotConfigured) {
				writeError(w, http.StatusServiceUnavailable, "ai_not_configured", "AI 未配置或未启用:请到「设置 → AI」完成配置后使用解读")
				return
			}
			writeError(w, http.StatusBadGateway, "ai_explain_failed", err.Error())
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSecExplain, TargetType: auditTargetSecurity,
			Detail: map[string]any{"findingId": in.VulnID, "scanType": in.ScanType}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]string{"text": text})
	}
}

// makeWafOverviewHandler 返回 GET /api/security/waf:SafeLine WAF 实例的防护态势
// (在线/延迟/今日与近7日拦截;未配置 Token → 明确提示)。只读,不审计。
func makeWafOverviewHandler(svc secscan.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := svc.WafOverview(r.Context())
		if err != nil {
			writeSecurityError(w, err)
			return
		}
		if items == nil {
			items = []secscan.WafInstanceStatus{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}
