package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/pushsource"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/trigger"
)

// webhookMaxBody 限制 webhook 投递体大小(防超大 body 拖垮内存)。
const webhookMaxBody = 1 << 20 // 1 MiB

// NewRunCreator 把 run.Service 适配为 trigger.RunCreator(供 main 装配 webhook 接收器)。
func NewRunCreator(svc run.Service) trigger.RunCreator {
	return runCreatorAdapter{svc: svc}
}

// runCreatorAdapter 把 run.Service 适配为 trigger.RunCreator:
// 以 webhook 触发类型创建运行,并返回新运行 id。
type runCreatorAdapter struct {
	svc run.Service
}

// CreateWebhookRun 实现 trigger.RunCreator:以 webhook 触发类型创建运行。
func (a runCreatorAdapter) CreateWebhookRun(ctx context.Context, projectID string, in trigger.RunRequest) (string, error) {
	r, err := a.svc.Create(ctx, projectID, run.Trigger{
		Type:   run.TriggerWebhook,
		Branch: in.Branch,
		Commit: in.Commit,
		Actor:  in.Actor,
	})
	if err != nil {
		return "", err
	}
	return r.ID, nil
}

// webhookAcceptedDTO 是命中创建运行的响应体(冻结契约)。
type webhookAcceptedDTO struct {
	Accepted bool   `json:"accepted"`
	RunID    string `json:"runId"`
}

// webhookIgnoredDTO 是未命中/被忽略的响应体(冻结契约;ignored 为原因枚举)。
type webhookIgnoredDTO struct {
	Accepted bool   `json:"accepted"`
	Ignored  string `json:"ignored"`
}

// makeWebhookHandler 返回 POST /api/webhooks/{token} handler(公开;靠签名校验;豁免 auth/CSRF）。
// 验签失败 → 401(不创建);命中 → 200 {accepted:true,runId};否则 200 {accepted:false,ignored}.
// 绝不回显密钥明文/密文;错误体永不含 secret。
func makeWebhookHandler(rc *trigger.Receiver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if rc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "webhook 接收服务未初始化")
			return
		}
		token := chi.URLParam(r, "token")

		// 限体积后读取原始 body(验签 + 解析所需)。
		r.Body = http.MaxBytesReader(w, r.Body, webhookMaxBody)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体过大或读取失败")
			return
		}

		res, err := rc.Handle(r.Context(), trigger.Delivery{
			Token: token,
			// 头按「Gitee → Gitea → Gogs」fallback 取第一个非空:
			// Gitea 出于兼容会同时发送 X-Gitea-* 与 X-Gogs-* 两套头,故 Gitea 优先。
			Event:         firstHeaderValue(r.Header.Get(trigger.HeaderGiteeEvent), r.Header.Get(trigger.HeaderGiteaEvent), r.Header.Get(trigger.HeaderGogsEvent)),
			TokenHdr:      r.Header.Get(trigger.HeaderGiteeToken),
			Timestamp:     r.Header.Get(trigger.HeaderGiteeTimestamp),
			DeliveryID:    firstHeaderValue(r.Header.Get(trigger.HeaderGiteeDelivery), r.Header.Get(trigger.HeaderGiteaDelivery), r.Header.Get(trigger.HeaderGogsDelivery)),
			BodySignature: firstHeaderValue(r.Header.Get(trigger.HeaderGiteaSignature), r.Header.Get(trigger.HeaderGogsSignature)),
			Actor:         webhookProvider(r.Header),
			RawBody:       body,
		})
		if err != nil {
			switch {
			case errors.Is(err, trigger.ErrUnauthorized):
				// 验签失败:401,不创建。错误体不含任何密钥信息。
				writeError(w, http.StatusUnauthorized, "unauthorized", "webhook 签名校验失败")
			case errors.Is(err, trigger.ErrTokenNotFound):
				writeError(w, http.StatusNotFound, "webhook_token_not_found", "webhook 端点不存在")
			default:
				writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			}
			return
		}

		if res.Accepted {
			writeJSON(w, http.StatusOK, webhookAcceptedDTO{Accepted: true, RunID: res.RunID})
			return
		}
		writeJSON(w, http.StatusOK, webhookIgnoredDTO{Accepted: false, Ignored: res.Ignored})
	}
}

// makeManualRunHandler 返回 POST /api/projects/{id}/runs handler(认证 + CSRF;手动触发）。
// body {branch, commit?, params?};被推送源项目额外支持 {revision?, items?}(部署时选择
// 修订与条目,经保留参数键随运行携带;items 为条目名数组)。
// actor=admin;成功 → 201 冻结 run-detail DTO。成功后追加 run_trigger_manual 审计。
func makeManualRunHandler(svc run.Service, params run.ParameterService, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "运行服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			Branch   string            `json:"branch"`
			Commit   string            `json:"commit"`
			Params   map[string]string `json:"params"`
			Revision string            `json:"revision"`
			Items    []string          `json:"items"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		// 分支可选:留空时由 run.Service.Create 取项目默认分支(项目已知默认分支,系统自动解析)。
		branch := strings.TrimSpace(req.Branch)
		commit := strings.TrimSpace(req.Commit)
		// 参数化运行(Story 8-11):trim key、剔空 key;明文构建参数(注入 script 步骤容器环境)。
		runParams := normalizeRunParams(req.Params)
		// 被推送源的部署时选择:修订标签 + 条目名列表(保留参数键;空 = 最新修订/全部条目)。
		if rev := strings.TrimSpace(req.Revision); rev != "" {
			if runParams == nil {
				runParams = map[string]string{}
			}
			runParams[pushsource.ParamRevision] = rev
		}
		if len(req.Items) > 0 {
			names := make([]string, 0, len(req.Items))
			for _, n := range req.Items {
				if n = strings.TrimSpace(n); n != "" {
					names = append(names, n)
				}
			}
			if len(names) > 0 {
				if runParams == nil {
					runParams = map[string]string{}
				}
				runParams[pushsource.ParamItems] = strings.Join(names, ",")
			}
		}
		// 类型化参数(P0):有项目参数定义时校验 + 强转 + 填默认;无定义则透传(向后兼容)。
		// 非法值(必填缺失 / 枚举越界 / 类型不符)→ 422,不创建运行。保留键不参与类型化过滤
		// (params.Resolve 只保留已定义键,先把选择键存回)。
		var pushRevision, pushItems string
		if runParams != nil {
			pushRevision, pushItems = runParams[pushsource.ParamRevision], runParams[pushsource.ParamItems]
		}
		if params != nil {
			resolved, perr := params.Resolve(r.Context(), id, runParams)
			if perr != nil {
				if errors.Is(perr, run.ErrInvalidParamValue) {
					writeError(w, http.StatusUnprocessableEntity, "invalid_parameter", perr.Error())
					return
				}
				writeError(w, http.StatusInternalServerError, "internal", "参数校验失败")
				return
			}
			runParams = resolved
		}
		if pushRevision != "" || pushItems != "" {
			if runParams == nil {
				runParams = map[string]string{}
			}
			if pushRevision != "" {
				runParams[pushsource.ParamRevision] = pushRevision
			}
			if pushItems != "" {
				runParams[pushsource.ParamItems] = pushItems
			}
		}
		// 操作人取当前会话用户名(普通用户为其用户名;管理员为其用户名;回退 "admin")。
		actor := sessionUsernameOrAdmin(r)
		rn, err := svc.Create(r.Context(), id, run.Trigger{
			Type:   run.TriggerManual,
			Branch: branch,
			Commit: commit,
			Actor:  actor,
			Params: runParams,
		})
		if err != nil {
			writeRunError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor:      actor,
			Action:     audit.ActionRunTriggerManual,
			TargetType: audit.TargetRun,
			TargetID:   rn.ID,
			Detail:     map[string]any{"projectId": id, "branch": branch, "commit": commit, "paramCount": len(runParams)},
			IP:         clientIP(r),
		})
		// 刚创建(queued)的运行尚无产物/无部署;nil → []/null。产物在成功路径由 worker emit、部署经 deploy 端点。
		writeJSON(w, http.StatusCreated, toRunDetailDTO(rn, nil, nil))
	}
}

// normalizeRunParams 规范化运行参数:trim key、剔除空 key(value 原样保留)。nil/空 → nil。
func normalizeRunParams(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		k = strings.TrimSpace(k)
		if k != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// firstHeaderValue 返回第一个 trim 后非空的值(用于多 provider 头 fallback)。
func firstHeaderValue(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// webhookProvider 据请求头识别投递来源(写入运行触发信息的操作人标签):
// 任一 X-Gitea-* 头存在 → gitea;否则任一 X-Gogs-* 头存在 → gogs;否则按 Gitee。
func webhookProvider(h http.Header) string {
	for _, name := range []string{trigger.HeaderGiteaEvent, trigger.HeaderGiteaSignature, trigger.HeaderGiteaDelivery} {
		if strings.TrimSpace(h.Get(name)) != "" {
			return "gitea"
		}
	}
	for _, name := range []string{trigger.HeaderGogsEvent, trigger.HeaderGogsSignature, trigger.HeaderGogsDelivery} {
		if strings.TrimSpace(h.Get(name)) != "" {
			return "gogs"
		}
	}
	return "gitee"
}
