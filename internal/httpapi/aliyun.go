package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/aliyunecs"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// 审计 action / target(阿里云 ECS 账户 + 导入写操作)。detail 绝无 Secret / 凭据明文
// (AccessKeyId 非机密,可入 detail)。
const (
	auditActionAliyunAccountCreate = "aliyun.account.create"
	auditActionAliyunAccountUpdate = "aliyun.account.update"
	auditActionAliyunAccountDelete = "aliyun.account.delete"
	auditActionAliyunAccountTest   = "aliyun.account.test"
	auditActionAliyunImport        = "aliyun.import"
	auditActionAliyunPower         = "aliyun.power"
	auditTargetAliyunAccount       = "aliyun_account"
)

// aliyunAccountDTO 是阿里云账户对外响应体(冻结契约;camelCase)。绝不外泄 Secret:
// 密文 BLOB 剥离,secret 由前端「只写」字段提交、留空即不修改;AccessKeyId 非机密可回显。
type aliyunAccountDTO struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	AccessKeyID string `json:"accessKeyId"`
	Region      string `json:"region"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

func toAliyunAccountDTO(a aliyunecs.Account) aliyunAccountDTO {
	return aliyunAccountDTO{
		ID:          a.ID,
		Name:        a.Name,
		AccessKeyID: a.AccessKeyID,
		Region:      a.Region,
		CreatedAt:   a.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   a.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// aliyunInstanceDTO 是 ECS 实例对外响应体(导入选型数据源)。
type aliyunInstanceDTO struct {
	InstanceID   string   `json:"instanceId"`
	Name         string   `json:"name"`
	HostName     string   `json:"hostname"`
	PrivateIPs   []string `json:"privateIps"`
	PublicIPs    []string `json:"publicIps"`
	OSType       string   `json:"osType"`
	Status       string   `json:"status"`
	InstanceType string   `json:"instanceType"`
	RegionID     string   `json:"regionId"`
}

func toAliyunInstanceDTO(i aliyunecs.Instance) aliyunInstanceDTO {
	priv := i.PrivateIPs
	if priv == nil {
		priv = []string{}
	}
	pub := i.PublicIPs
	if pub == nil {
		pub = []string{}
	}
	return aliyunInstanceDTO{
		InstanceID:   i.InstanceID,
		Name:         i.Name,
		HostName:     i.HostName,
		PrivateIPs:   priv,
		PublicIPs:    pub,
		OSType:       i.OSType,
		Status:       i.Status,
		InstanceType: i.InstanceType,
		RegionID:     i.RegionID,
	}
}

// writeAliyunError 把阿里云 ECS 领域错误映射为契约错误码/状态码;绝不回显明文/Secret。
func writeAliyunError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, aliyunecs.ErrNotFound):
		writeError(w, http.StatusNotFound, "aliyun_account_not_found", "阿里云账户不存在")
	case errors.Is(err, aliyunecs.ErrEmptyName):
		writeError(w, http.StatusBadRequest, "invalid_aliyun_account", "请填写账户展示名")
	case errors.Is(err, aliyunecs.ErrInvalidAccessKeyID):
		writeError(w, http.StatusBadRequest, "invalid_aliyun_account", "请填写 AccessKeyId")
	case errors.Is(err, aliyunecs.ErrEmptySecret):
		writeError(w, http.StatusBadRequest, "invalid_aliyun_account", "请填写 AccessKeySecret")
	case errors.Is(err, aliyunecs.ErrInvalidRegion):
		writeError(w, http.StatusBadRequest, "invalid_aliyun_account", "Region 非法(形如 cn-hangzhou)")
	case errors.Is(err, aliyunecs.ErrVaultUnconfigured):
		writeError(w, http.StatusServiceUnavailable, "vault_unconfigured", "保险库未配置 master key,无法封存 AccessKeySecret")
	case errors.Is(err, aliyunecs.ErrNoInstances):
		writeError(w, http.StatusBadRequest, "invalid_aliyun_import", "请先勾选要导入的实例")
	case errors.Is(err, aliyunecs.ErrTooManyInstances):
		writeError(w, http.StatusBadRequest, "invalid_aliyun_import", "单批导入不能超过 100 台")
	case errors.Is(err, aliyunecs.ErrInvalidCredential):
		writeError(w, http.StatusBadRequest, "invalid_aliyun_import", "认证方式非法:请选择已有 SSH 凭据或填写内联密码/私钥(二选一)")
	case errors.Is(err, aliyunecs.ErrInvalidIPPreference):
		writeError(w, http.StatusBadRequest, "invalid_aliyun_import", "连接 IP 偏好非法(仅内网 private / 公网 public)")
	case errors.Is(err, aliyunecs.ErrInvalidSSHUser):
		writeError(w, http.StatusBadRequest, "invalid_aliyun_import", "请填写 SSH 登录用户")
	case errors.Is(err, aliyunecs.ErrListFailed):
		writeError(w, http.StatusBadGateway, "aliyun_api_failed", "阿里云 ECS API 调用失败:凭据无效、Region 不存在、无权限或网络不可达")
	case errors.Is(err, aliyunecs.ErrVerifyFailed):
		writeError(w, http.StatusBadGateway, "aliyun_verify_failed", "阿里云账户校验失败:凭据无效、Region 不存在或无 ECS 权限")
	case errors.Is(err, aliyunecs.ErrServerNotLinked):
		writeError(w, http.StatusNotFound, "server_not_linked", "该服务器不是导入的阿里云 ECS,无法开关机")
	case errors.Is(err, aliyunecs.ErrPowerFailed):
		writeError(w, http.StatusBadGateway, "aliyun_power_failed", "阿里云 ECS 开关机失败:凭据无效、网络不可达或实例状态不允许")
	case errors.Is(err, vault.ErrVaultUnconfigured):
		writeError(w, http.StatusServiceUnavailable, "vault_unconfigured", "保险库未配置 master key,无法存取凭据")
	case errors.Is(err, vault.ErrEmptySecret), errors.Is(err, vault.ErrEmptyName):
		writeError(w, http.StatusBadRequest, "invalid_aliyun_import", "内联凭据名称与内容不能为空")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusBadGateway, "aliyun_unreachable", "连接阿里云 ECS API 超时")
	// target(SSH)层错误:复用语义映射(导入路径可能穿透)。
	case errors.Is(err, target.ErrVaultUnconfigured):
		writeError(w, http.StatusServiceUnavailable, "vault_unconfigured", "保险库未配置 master key,无法取 SSH 凭据")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// makeListAliyunAccountsHandler 返回 GET /api/aliyun/accounts → { items: [...] }(只读,无审计)。
func makeListAliyunAccountsHandler(svc aliyunecs.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "阿里云 ECS 服务未初始化")
			return
		}
		accounts, err := svc.ListAccounts(r.Context())
		if err != nil {
			writeAliyunError(w, err)
			return
		}
		out := make([]aliyunAccountDTO, 0, len(accounts))
		for _, a := range accounts {
			out = append(out, toAliyunAccountDTO(a))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

// makeCreateAliyunAccountHandler 返回 POST /api/aliyun/accounts(认证 + CSRF)→ 账户(201)。
// body: { name, accessKeyId, secret, region }。secret 是「只写」字段:提交一次经
// vault.SealSecret 密文落库即弃,绝不回库/响应/审计/日志。
func makeCreateAliyunAccountHandler(svc aliyunecs.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "阿里云 ECS 服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<14)
		var in struct {
			Name        string `json:"name"`
			AccessKeyID string `json:"accessKeyId"`
			Secret      string `json:"secret"`
			Region      string `json:"region"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		a, err := svc.CreateAccount(r.Context(), aliyunecs.AccountCreateInput{
			Name:        in.Name,
			AccessKeyID: in.AccessKeyID,
			Secret:      in.Secret,
			Region:      in.Region,
		})
		if err != nil {
			writeAliyunError(w, err)
			return
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor:      auditActor,
			Action:     auditActionAliyunAccountCreate,
			TargetType: auditTargetAliyunAccount,
			TargetID:   a.ID,
			Detail:     map[string]any{"name": a.Name, "accessKeyId": a.AccessKeyID, "region": a.Region}, // 绝无 Secret
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusCreated, toAliyunAccountDTO(*a))
	}
}

// makeUpdateAliyunAccountHandler 返回 PUT /api/aliyun/accounts/{id}(认证 + CSRF)→ 账户。
// body: { name?, accessKeyId?, region?, secret? }(指针语义:缺省不修改;secret 非空即轮换重封)。
func makeUpdateAliyunAccountHandler(svc aliyunecs.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "阿里云 ECS 服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<14)
		var in struct {
			Name        *string `json:"name"`
			AccessKeyID *string `json:"accessKeyId"`
			Region      *string `json:"region"`
			Secret      *string `json:"secret"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		a, err := svc.UpdateAccount(r.Context(), id, aliyunecs.AccountUpdateInput{
			Name:        in.Name,
			AccessKeyID: in.AccessKeyID,
			Region:      in.Region,
			Secret:      in.Secret,
		})
		if err != nil {
			writeAliyunError(w, err)
			return
		}
		rotated := in.Secret != nil && strings.TrimSpace(*in.Secret) != ""
		recordAudit(r.Context(), rec, audit.Entry{
			Actor:      auditActor,
			Action:     auditActionAliyunAccountUpdate,
			TargetType: auditTargetAliyunAccount,
			TargetID:   a.ID,
			Detail:     map[string]any{"name": a.Name, "accessKeyId": a.AccessKeyID, "region": a.Region, "secretRotated": rotated}, // 绝无 Secret
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusOK, toAliyunAccountDTO(*a))
	}
}

// makeDeleteAliyunAccountHandler 返回 DELETE /api/aliyun/accounts/{id}(认证 + CSRF)→ { ok: true }。
// 账户密文随之消失(行删除);实例映射一并清理。
func makeDeleteAliyunAccountHandler(svc aliyunecs.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "阿里云 ECS 服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.DeleteAccount(r.Context(), id); err != nil {
			writeAliyunError(w, err)
			return
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor:      auditActor,
			Action:     auditActionAliyunAccountDelete,
			TargetType: auditTargetAliyunAccount,
			TargetID:   id,
			Detail:     map[string]any{}, // 绝无 Secret
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// makeTestAliyunAccountHandler 返回 POST /api/aliyun/accounts/{id}/test(认证 + CSRF)
// → { ok: true };失败走错误信封(凭据无效/Region 不存在/无权限 → 502 人话)。
func makeTestAliyunAccountHandler(svc aliyunecs.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "阿里云 ECS 服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		err := svc.VerifyAccount(r.Context(), id)
		recordAudit(r.Context(), rec, audit.Entry{
			Actor:      auditActor,
			Action:     auditActionAliyunAccountTest,
			TargetType: auditTargetAliyunAccount,
			TargetID:   id,
			Detail:     map[string]any{"ok": err == nil},
			IP:         clientIP(r),
		})
		if err != nil {
			writeAliyunError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// makeListAliyunInstancesHandler 返回 GET /api/aliyun/accounts/{id}/instances → { items: [...] }。
// 只读拉取该账户 Region 下全部 ECS 实例(导入选型数据源);凭据解封仅进程内即用即弃。
func makeListAliyunInstancesHandler(svc aliyunecs.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "阿里云 ECS 服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		instances, err := svc.ListInstances(r.Context(), id)
		if err != nil {
			writeAliyunError(w, err)
			return
		}
		out := make([]aliyunInstanceDTO, 0, len(instances))
		for _, i := range instances {
			out = append(out, toAliyunInstanceDTO(i))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

// makeAliyunImportHandler 返回 POST /api/aliyun/import(认证 + CSRF)→ 逐台导入结果。
// body: { accountId, instanceIds: [...], ipPreference, credentialId | inlineSecret { kind, name, secret },
// sshUser, sshPort }。逐台独立容错(一台失败 200 + ok:false,不 500);整批校验失败才 4xx。
func makeAliyunImportHandler(svc aliyunecs.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "阿里云 ECS 服务未初始化")
			return
		}
		// 64KiB:内联 SSH 私钥足够容纳,同时封顶异常超大请求体。
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var in struct {
			AccountID    string   `json:"accountId"`
			InstanceIDs  []string `json:"instanceIds"`
			IPPreference string   `json:"ipPreference"`
			CredentialID string   `json:"credentialId"`
			InlineSecret *struct {
				Kind   string `json:"kind"`
				Name   string `json:"name"`
				Secret string `json:"secret"`
			} `json:"inlineSecret"`
			SSHUser string `json:"sshUser"`
			SSHPort int    `json:"sshPort"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		input := aliyunecs.ImportInput{
			AccountID:    in.AccountID,
			InstanceIDs:  in.InstanceIDs,
			IPPreference: in.IPPreference,
			CredentialID: in.CredentialID,
			SSHUser:      in.SSHUser,
			SSHPort:      in.SSHPort,
		}
		if in.InlineSecret != nil {
			input.InlineSecret = &aliyunecs.InlineSecret{
				Kind:   in.InlineSecret.Kind,
				Name:   in.InlineSecret.Name,
				Secret: in.InlineSecret.Secret,
			}
		}
		res, err := svc.Import(r.Context(), input)
		if err != nil {
			writeAliyunError(w, err)
			return
		}
		type itemDTO struct {
			InstanceID string `json:"instanceId"`
			Name       string `json:"name"`
			ServerID   string `json:"serverId"`
			Action     string `json:"action"`
			OK         bool   `json:"ok"`
			Error      string `json:"error,omitempty"`
		}
		items := make([]itemDTO, 0, len(res.Items))
		for _, it := range res.Items {
			items = append(items, itemDTO{
				InstanceID: it.InstanceID,
				Name:       it.Name,
				ServerID:   it.ServerID,
				Action:     it.Action,
				OK:         it.OK,
				Error:      it.Err,
			})
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor:      auditActor,
			Action:     auditActionAliyunImport,
			TargetType: auditTargetAliyunAccount,
			TargetID:   in.AccountID,
			Detail: map[string]any{ // 绝无 Secret / 凭据明文
				"total": res.Total, "ok": res.OK, "failed": res.Failed,
				"ipPreference": in.IPPreference, "inlineCredential": in.InlineSecret != nil,
			},
			IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"items": items, "total": res.Total, "ok": res.OK, "failed": res.Failed,
		})
	}
}

// powerItemDTO 是单台实例开关机结果的对外响应体(与领域 PowerItemResult 对齐)。
type powerItemDTO struct {
	InstanceID string `json:"instanceId"`
	ServerID   string `json:"serverId,omitempty"`
	Skipped    bool   `json:"skipped"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
}

// makeAliyunPowerStatusHandler 返回 GET /api/aliyun/power/status → { items: [...] }(只读,无审计)。
// 服务器状态页电源徽标数据源:全部「已导入 ECS ↔ servers」映射 + 实例实时电源状态
// (Running/Stopped/...;逐账户独立容错,失败条目带 error)。ae 为 nil → 503。
func makeAliyunPowerStatusHandler(svc aliyunecs.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "阿里云 ECS 服务未初始化")
			return
		}
		items, err := svc.LinkedPowerStatus(r.Context())
		if err != nil {
			writeAliyunError(w, err)
			return
		}
		type itemDTO struct {
			ServerID   string `json:"serverId"`
			AccountID  string `json:"accountId"`
			InstanceID string `json:"instanceId"`
			Status     string `json:"status"`
			Error      string `json:"error,omitempty"`
		}
		out := make([]itemDTO, 0, len(items))
		for _, it := range items {
			out = append(out, itemDTO{
				ServerID:   it.ServerID,
				AccountID:  it.AccountID,
				InstanceID: it.InstanceID,
				Status:     it.Status,
				Error:      it.Err,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

// makeAliyunPowerHandler 返回 POST /api/aliyun/power(认证 + CSRF + 审计)→ 逐台结果。
// body: { serverId, action: "start" | "stop" }。对某登记服务器关联的 ECS 实例发起
// 开机/关机(发起即返回,不等待到达目标态;幂等:已在目标态跳过)。逐台独立容错
// (200 + ok:false,不 500);结构错误(动作非法/未关联)才 4xx/5xx。
func makeAliyunPowerHandler(svc aliyunecs.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "阿里云 ECS 服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
		var in struct {
			ServerID string `json:"serverId"`
			Action   string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		if in.ServerID == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "请提供 serverId")
			return
		}
		if in.Action != aliyunecs.PowerActionStart && in.Action != aliyunecs.PowerActionStop {
			writeError(w, http.StatusBadRequest, "bad_request", "动作非法(仅 start|stop)")
			return
		}
		res, err := svc.PowerServer(r.Context(), in.ServerID, in.Action)
		// 开关机是高危操作:任何尝试(含失败)都留审计;detail 绝无凭据。
		auditDetail := map[string]any{"action": in.Action, "serverId": in.ServerID}
		if res != nil {
			instances := make([]string, 0, len(res.Items))
			for _, it := range res.Items {
				instances = append(instances, it.InstanceID)
			}
			auditDetail["total"] = res.Total
			auditDetail["ok"] = res.OK
			auditDetail["failed"] = res.Failed
			auditDetail["instances"] = instances
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor:      auditActor,
			Action:     auditActionAliyunPower,
			TargetType: audit.TargetServer,
			TargetID:   in.ServerID,
			Detail:     auditDetail,
			IP:         clientIP(r),
		})
		if err != nil {
			writeAliyunError(w, err)
			return
		}
		items := make([]powerItemDTO, 0, len(res.Items))
		for _, it := range res.Items {
			items = append(items, powerItemDTO{
				InstanceID: it.InstanceID,
				ServerID:   it.ServerID,
				Skipped:    it.Skipped,
				OK:         it.OK,
				Error:      it.Err,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items": items, "total": res.Total, "ok": res.OK, "failed": res.Failed,
		})
	}
}
