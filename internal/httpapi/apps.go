package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/appstore"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/composeops"
	"github.com/huangchengsir/pipewright/internal/dbadmin"
	"github.com/huangchengsir/pipewright/internal/objectstore"
	"github.com/huangchengsir/pipewright/internal/redisadmin"
	"github.com/huangchengsir/pipewright/internal/secscan"
	"github.com/huangchengsir/pipewright/internal/target"
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
	auditActionAppUpgrade        = "appstore.upgrade"
	auditActionAppUninstall      = "appstore.uninstall"
	auditActionObjectStoreReg    = "objectstore.register"
	auditTargetAppStore          = "app_template"
)

// appAutoRegisterTemplates 是「部署成功后联动自动登记」的内置模板名集合:
// rustfs → 登记为对象存储配置;mysql / postgres → 登记为数据库管理实例;redis → 登记为
// Redis 管理实例;trivy-server → 登记为安全中心扫描器;nuclei-runner → 登记为安全中心
// 常驻执行容器(Nuclei 扫描经它执行,目标机无需 docker)。
const appAutoRegisterObjectStoreTemplate = "rustfs"
const appAutoRegisterRedisTemplate = "redis"
const appAutoRegisterScannerTemplate = "trivy-server"
const appAutoRegisterNucleiRunnerTemplate = "nuclei-runner"
const appAutoRegisterSafelineTemplate = "safeline"

var appAutoRegisterDBTemplates = map[string]bool{
	"mysql":    true,
	"postgres": true,
}

// appRegisterTimeout 是部署后联动登记对象存储的独立超时(含建桶短重试),不占 compose up 的窗口。
const appRegisterTimeout = 20 * time.Second

// registerRustFSObjectStore 是应用商店部署 RustFS 成功后的联动登记(尽力而为,绝不影响部署结果):
// 从渲染后的生效参数取接入信息(endpoint = 服务器地址 + API 端口),交给 objectstore.Register
// 完成「确保默认桶 + 凭据入 vault + 同 type+endpoint 幂等轮换」。返回 (是否已登记, 人话备注)。
// 备注会进入部署响应与审计 detail;绝不含 Secret(审计只记接入参数,与 objectstore.create 同纪律)。
// instance 非空(多副本部署)时登记名带实例后缀,区分同机多个 RustFS。
func registerRustFSObjectStore(ctx context.Context, objStores objectstore.Service, aud audit.Recorder, r *http.Request, srv target.Server, used map[string]string, instance string) (bool, string) {
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
	if instance != "" {
		name += " · " + instance
	}
	endpoint := net.JoinHostPort(srv.Host, apiPort)
	res, err := objStores.Register(ctx, objectstore.RegisterInput{
		Type:        objectstore.TypeRustFS,
		Name:        name,
		Endpoint:    endpoint,
		UseSSL:      false, // 部署形态是明文 host:port;若手动改过已有配置,Register 幂等路径不动该开关
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

// registerDatabaseInstance 是应用商店部署 mysql/postgres 成功后的联动登记(尽力而为,
// 绝不影响部署结果):从渲染后的生效参数取 root/postgres 密码与端口,交给 dbadmin.Register
// 完成幂等登记(同引擎同地址原地轮换密码)。密码经 vault 密封,审计只记接入参数,
// 绝不含密码。不在部署链路里做连接测试——数据库容器首启初始化可达分钟级,
// 连通性由「数据库」管理页的连接测试兜底。返回 (是否已登记, 人话备注)。
func registerDatabaseInstance(ctx context.Context, dbAdmins dbadmin.Service, aud audit.Recorder, r *http.Request, srv target.Server, tplName, stackName string, used map[string]string, instance string) (bool, string) {
	if dbAdmins == nil {
		return false, "" // 服务未注入(如裁剪装配)→ 静默跳过,部署不受影响
	}
	username, password, portParam, display := "", "", "", ""
	switch tplName {
	case "mysql":
		username, password, portParam, display = "root", used["root_password"], used["port"], "MySQL"
	case "postgres":
		username, password, portParam, display = "postgres", used["password"], used["port"], "PostgreSQL"
	default:
		return false, ""
	}
	if password == "" || portParam == "" {
		return false, "生效参数缺少密码/端口,未自动登记数据库实例"
	}
	port, err := strconv.Atoi(strings.TrimSpace(portParam))
	if err != nil || port < 1 || port > 65535 {
		return false, "生效端口参数非法,未自动登记数据库实例"
	}
	name := display + " · " + srv.Name
	if strings.TrimSpace(name) == display+" ·" {
		name = display + " · " + srv.Host
	}
	if instance != "" {
		name += " · " + instance
	}
	res, err := dbAdmins.Register(ctx, dbadmin.RegisterInput{
		Engine: tplName, Name: name, Host: srv.Host, Port: port,
		Username: username, Password: password, ServerID: srv.ID, StackName: stackName,
	})
	if err != nil {
		msg := "自动登记数据库实例失败"
		if errors.Is(err, dbadmin.ErrVaultUnconfigured) {
			msg += ":保险库未配置 master key"
		} else {
			msg += ":" + err.Error()
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionDBInstanceRegister, TargetType: auditTargetAppStore,
			Detail: map[string]any{"engine": tplName, "host": srv.Host, "port": port, "source": "appstore", "ok": false, "error": msg}, IP: clientIP(r),
		})
		return false, msg
	}
	note := "已自动登记到「设置 → 数据库」,可在「数据库」页在线管理"
	if res.Updated {
		note = "已更新既有数据库实例的密码(同地址原地轮换)"
	}
	recordAudit(r.Context(), aud, audit.Entry{
		Actor: auditActor, Action: auditActionDBInstanceRegister, TargetType: auditTargetAppStore,
		TargetID: res.Instance.ID,
		Detail:   map[string]any{"engine": tplName, "host": srv.Host, "port": port, "source": "appstore", "ok": true, "updated": res.Updated}, IP: clientIP(r),
	})
	return true, note
}

// registerRedisInstance 是应用商店部署 redis 成功后的联动登记(尽力而为,绝不影响部署
// 结果):从渲染后的生效参数取 requirepass 密码与端口,交给 redisadmin.Register 完成
// 幂等登记(同地址原地轮换密码)。密码经 vault 密封,审计只记接入参数,绝不含密码。
// 不在部署链路里做连接测试——容器首启初始化可达秒级,连通性由「Redis」管理页的
// 连接测试兜底。返回 (是否已登记, 人话备注)。
func registerRedisInstance(ctx context.Context, redisAdmins redisadmin.Service, aud audit.Recorder, r *http.Request, srv target.Server, stackName string, used map[string]string, instance string) (bool, string) {
	if redisAdmins == nil {
		return false, "" // 服务未注入(如裁剪装配)→ 静默跳过,部署不受影响
	}
	password, portParam := used["password"], used["port"]
	if password == "" || portParam == "" {
		return false, "生效参数缺少密码/端口,未自动登记 Redis 实例"
	}
	port, err := strconv.Atoi(strings.TrimSpace(portParam))
	if err != nil || port < 1 || port > 65535 {
		return false, "生效端口参数非法,未自动登记 Redis 实例"
	}
	name := "Redis · " + srv.Name
	if strings.TrimSpace(name) == "Redis ·" {
		name = "Redis · " + srv.Host
	}
	if instance != "" {
		name += " · " + instance
	}
	res, err := redisAdmins.Register(ctx, redisadmin.RegisterInput{
		Name: name, Host: srv.Host, Port: port, Password: password,
		ServerID: srv.ID, StackName: stackName,
	})
	if err != nil {
		msg := "自动登记 Redis 实例失败"
		if errors.Is(err, redisadmin.ErrVaultUnconfigured) {
			msg += ":保险库未配置 master key"
		} else {
			msg += ":" + err.Error()
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionRedisInstanceRegister, TargetType: auditTargetAppStore,
			Detail: map[string]any{"host": srv.Host, "port": port, "source": "appstore", "ok": false, "error": msg}, IP: clientIP(r),
		})
		return false, msg
	}
	note := "已自动登记到「设置 → Redis」,可在「Redis」页浏览键空间"
	if res.Updated {
		note = "已更新既有 Redis 实例的密码(同地址原地轮换)"
	}
	recordAudit(r.Context(), aud, audit.Entry{
		Actor: auditActor, Action: auditActionRedisInstanceRegister, TargetType: auditTargetAppStore,
		TargetID: res.Instance.ID,
		Detail:   map[string]any{"host": srv.Host, "port": port, "source": "appstore", "ok": true, "updated": res.Updated}, IP: clientIP(r),
	})
	return true, note
}

// registerScannerInstance 是应用商店部署 trivy-server 成功后的联动登记(尽力而为,
// 绝不影响部署结果):从渲染后的生效参数取端口与可选令牌,交给 secscan.Register 完成
// 幂等登记(同地址原地轮换令牌)。令牌经 vault 密封,审计只记接入参数,绝不含令牌。
// 不做连接测试——trivy-server 首启要拉漏洞库(可达分钟级),连通性由「安全中心」
// 扫描器的测试按钮兜底。返回 (是否已登记, 人话备注)。
func registerScannerInstance(ctx context.Context, secScans secscan.Service, aud audit.Recorder, r *http.Request, srv target.Server, stackName string, used map[string]string, instance string) (bool, string) {
	if secScans == nil {
		return false, "" // 服务未注入(如裁剪装配)→ 静默跳过,部署不受影响
	}
	portParam := used["port"]
	if portParam == "" {
		return false, "生效参数缺少 port,未自动登记安全中心扫描器"
	}
	port, err := strconv.Atoi(strings.TrimSpace(portParam))
	if err != nil || port < 1 || port > 65535 {
		return false, "生效端口参数非法,未自动登记安全中心扫描器"
	}
	name := "Trivy Server · " + srv.Name
	if strings.TrimSpace(name) == "Trivy Server ·" {
		name = "Trivy Server · " + srv.Host
	}
	if instance != "" {
		name += " · " + instance
	}
	res, err := secScans.Register(ctx, secscan.RegisterInput{
		Kind: secscan.KindTrivyServer, Name: name, Host: srv.Host, Port: port, Token: used["token"],
		ServerID: srv.ID, StackName: stackName,
	})
	if err != nil {
		msg := "自动登记安全中心扫描器失败"
		if errors.Is(err, secscan.ErrVaultUnconfigured) {
			msg += ":保险库未配置 master key"
		} else {
			msg += ":" + err.Error()
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSecScannerReg, TargetType: auditTargetSecurity,
			Detail: map[string]any{"host": srv.Host, "port": port, "source": "appstore", "ok": false, "error": msg}, IP: clientIP(r),
		})
		return false, msg
	}
	note := "已自动登记到「安全中心」,可作为 Trivy 扫描的中心漏洞库服务"
	if res.Updated {
		note = "已更新既有扫描器登记的令牌(同地址原地轮换)"
	}
	targetID := ""
	if res.Instance != nil {
		targetID = res.Instance.ID
	}
	recordAudit(r.Context(), aud, audit.Entry{
		Actor: auditActor, Action: auditActionSecScannerReg, TargetType: auditTargetSecurity,
		TargetID: targetID,
		Detail:   map[string]any{"host": srv.Host, "port": port, "source": "appstore", "ok": true, "updated": res.Updated}, IP: clientIP(r),
	})
	return true, note
}

// registerNucleiRunnerInstance 是应用商店部署 nuclei-runner 成功后的联动登记(尽力而为,
// 绝不影响部署结果):登记 kind=nuclei-runner(无服务端口,按部署宿主 + 栈名定位执行
// 容器),幂等语义同 registerScannerInstance。
func registerNucleiRunnerInstance(ctx context.Context, secScans secscan.Service, aud audit.Recorder, r *http.Request, srv target.Server, stackName, instance string) (bool, string) {
	if secScans == nil {
		return false, ""
	}
	name := "Nuclei Runner · " + srv.Name
	if strings.TrimSpace(name) == "Nuclei Runner ·" {
		name = "Nuclei Runner · " + srv.Host
	}
	if instance != "" {
		name += " · " + instance
	}
	res, err := secScans.Register(ctx, secscan.RegisterInput{
		Kind: secscan.KindNucleiRunner, Name: name, Host: srv.Host,
		ServerID: srv.ID, StackName: stackName,
	})
	if err != nil {
		msg := "自动登记 Nuclei Runner 失败:" + err.Error()
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSecScannerReg, TargetType: auditTargetSecurity,
			Detail: map[string]any{"kind": secscan.KindNucleiRunner, "host": srv.Host, "source": "appstore", "ok": false, "error": msg}, IP: clientIP(r),
		})
		return false, msg
	}
	note := "已自动登记到「安全中心」,Nuclei 扫描将经此执行(目标机无需 docker)"
	if res.Updated {
		note = "已更新既有 Nuclei Runner 登记(同主机原地刷新)"
	}
	targetID := ""
	if res.Instance != nil {
		targetID = res.Instance.ID
	}
	recordAudit(r.Context(), aud, audit.Entry{
		Actor: auditActor, Action: auditActionSecScannerReg, TargetType: auditTargetSecurity,
		TargetID: targetID,
		Detail:   map[string]any{"kind": secscan.KindNucleiRunner, "host": srv.Host, "source": "appstore", "ok": true, "updated": res.Updated}, IP: clientIP(r),
	})
	return true, note
}

// registerSafelineInstance 是应用商店部署 safeline 成功后的联动登记(尽力而为,
// 绝不影响部署结果):登记 kind=safeline 的 WAF 实例(管理台地址 = 宿主:管理端口;
// API Token 留空,note 指引到雷池控制台生成后补填)。幂等语义同 registerScannerInstance。
func registerSafelineInstance(ctx context.Context, secScans secscan.Service, aud audit.Recorder, r *http.Request, srv target.Server, stackName string, used map[string]string, instance string) (bool, string) {
	if secScans == nil {
		return false, ""
	}
	portParam := used["port"]
	if portParam == "" {
		return false, "生效参数缺少 port,未自动登记 WAF 实例"
	}
	port, err := strconv.Atoi(strings.TrimSpace(portParam))
	if err != nil || port < 1 || port > 65535 {
		return false, "生效端口参数非法,未自动登记 WAF 实例"
	}
	name := "SafeLine WAF · " + srv.Name
	if strings.TrimSpace(name) == "SafeLine WAF ·" {
		name = "SafeLine WAF · " + srv.Host
	}
	if instance != "" {
		name += " · " + instance
	}
	res, err := secScans.Register(ctx, secscan.RegisterInput{
		Kind: secscan.KindSafeline, Name: name, Host: srv.Host, Port: port,
		ServerID: srv.ID, StackName: stackName,
	})
	if err != nil {
		msg := "自动登记 WAF 实例失败:" + err.Error()
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSecScannerReg, TargetType: auditTargetSecurity,
			Detail: map[string]any{"kind": secscan.KindSafeline, "host": srv.Host, "port": port, "source": "appstore", "ok": false, "error": msg}, IP: clientIP(r),
		})
		return false, msg
	}
	note := "已自动登记到「安全中心」;下一步:在雷池控制台生成 API Token,到安全中心扫描器实例中补填,即可看到拦截态势"
	if res.Updated {
		note = "已更新既有 WAF 实例登记(同地址原地刷新)"
	}
	targetID := ""
	if res.Instance != nil {
		targetID = res.Instance.ID
	}
	recordAudit(r.Context(), aud, audit.Entry{
		Actor: auditActor, Action: auditActionSecScannerReg, TargetType: auditTargetSecurity,
		TargetID: targetID,
		Detail:   map[string]any{"kind": secscan.KindSafeline, "host": srv.Host, "port": port, "source": "appstore", "ok": true, "updated": res.Updated}, IP: clientIP(r),
	})
	return true, note
}

// deployComposeToServer 把一段 compose 内容部署为受管 Stacks 项目(与 Stacks 部署同一条链路):
// 建受管目录 → Upload compose 文件(字节流)→ 探测 compose CLI → `compose -p <name> up -d`。
// 实现在 internal/composeops(与 DBaaS 共享);此处仅叠加本端容器清单缓存失效。
// 返回 (ok, 输出摘要, 人读错误)。
func deployComposeToServer(ctx context.Context, svc target.Service, serverID, name, compose string) (bool, string, string) {
	res := composeops.Deploy(ctx, svc, serverID, name, compose)
	if res.Dispatched {
		// compose up 已投递(容器集合大概率已变)→ 丢弃该台容器清单缓存。
		serverContainersCache.invalidate(serverID)
	}
	return res.OK, res.Out, res.Err
}

// hostParamNames 是「部署宿主地址」约定参数:模板声明了这些参数且用户未显式提供
// (键缺省或留空)时,部署 handler 注入目标机 Host,使客户端通告/克隆地址自动指向
// 部署宿主(advertise_host 如 Redpanda Kafka advertise,ssh_domain 如 Gitea SSH
// 克隆地址主机——缺省会回落 localhost)。
var hostParamNames = map[string]bool{
	"advertise_host": true,
	"ssh_domain":     true,
}

// injectHostParams 对模板声明的宿主地址约定参数注入 host(仅键缺省或留空时,显式填写优先)。
// 返回更新后的参数表(入参 nil 时新建)。
func injectHostParams(params map[string]string, tpl *appstore.Template, host string) map[string]string {
	for _, p := range tpl.Params {
		if !hostParamNames[p.Name] {
			continue
		}
		if params == nil {
			params = map[string]string{}
		}
		if strings.TrimSpace(params[p.Name]) == "" {
			params[p.Name] = host
		}
	}
	return params
}

// injectComposeLimits 把 CPU/内存上限注入 compose 每个 service 的 deploy.resources.limits
// (实现在 internal/composeops,与 DBaaS 共享)。
func injectComposeLimits(compose, cpus, memory string) (string, error) {
	return composeops.InjectLimits(compose, cpus, memory)
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

// makeDeployAppHandler 返回 POST /api/servers/{id}/apps/deploy {templateId, params, instance?, cpus?, memory?}。
// 渲染参数(自动生成 secret)→ 可选注入资源限制(deploy.resources.limits)→ 复用 Stacks 链路
// 部署到该主机;生成的 secret 随响应一次性返回(部署后以明文存在于目标机 compose 文件,与
// Stacks 语义一致;审计只记模板名,不记参数值)。联动登记(均尽力而为,结果见响应字段):
// RustFS 部署成功 → 自动登记对象存储配置;mysql/postgres → 自动登记数据库实例;
// redis → 自动登记 Redis 管理实例。
// 可选 instance 支持同机多副本:栈目录与 compose 项目名用 <模板名>-<实例名>(留空即模板名,
// 默认单实例);数据卷/网络按 compose 项目名前缀自动隔离。
// 部署成功后在栈目录落参数边车(.appstore-params.json,含生效参数与资源限制)——「升级」
// (POST .../apps/upgrade)靠它原位保留参数,避免 secret 重新生成改掉既有实例口令。
func makeDeployAppHandler(apps appstore.Service, servers target.Service, aud audit.Recorder, objStores objectstore.Service, dbAdmins dbadmin.Service, redisAdmins redisadmin.Service, secScans secscan.Service) http.HandlerFunc {
	type request struct {
		TemplateID string            `json:"templateId"`
		Params     map[string]string `json:"params"`
		Instance   string            `json:"instance,omitempty"` // 可选实例名:栈名后缀,同机多副本
		CPUs       string            `json:"cpus,omitempty"`     // 可选,全服务 CPU 核数上限(如 "1.0")
		Memory     string            `json:"memory,omitempty"`   // 可选,全服务内存硬上限(如 "512m")
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
		// 实例名与 Stacks 部署的项目名同规则(reDockerTgt,防 shell 元字符/路径注入);留空 = 默认单实例。
		req.Instance = strings.TrimSpace(req.Instance)
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
		// 资源限制复用容器创建的同一白名单校验(reCPUs/reMemory,防非法值进 compose);
		// PVE 集群物理机不允许作为应用部署目标(0084:虚拟化宿主机不跑平台部署的栈)。
		if msg := validateAppDeployTarget(req.Instance, req.CPUs, req.Memory, srv); msg != "" {
			if srv.IsPVENode() {
				writeError(w, http.StatusUnprocessableEntity, "pve_node_not_target", msg)
				return
			}
			writeError(w, http.StatusBadRequest, "invalid_app_deploy", msg)
			return
		}
		// 宿主地址约定参数(advertise_host/ssh_domain,见 hostParamNames):未显式提供或
		// 留空时注入目标机 Host,让 Kafka advertise、SSH 克隆地址等自动指向部署宿主。
		req.Params = injectHostParams(req.Params, tpl, srv.Host)
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
		// 生效栈名:实例名留空 = 模板名(兼容既有单实例部署);组合长度与 Stacks 项目名上限一致。
		stackName := appStackName(tpl.Name, req.Instance)
		if len(stackName) > 128 {
			writeError(w, http.StatusBadRequest, "invalid_app_deploy", "实例名过长:组合栈名超过 128 字符")
			return
		}

		out := appDeployResponse{ServerID: serverID, Name: stackName, Params: rendered.Used}
		// compose up 含镜像拉取,不设主动超时:大镜像(数 GB)时长取决于带宽,人为截断只会杀掉
		// 进行中的分层下载;拉取失败 docker 自身会报错。客户端断开经 r.Context() 取消兜底。
		out.OK, out.Output, out.Error = deployComposeToServer(r.Context(), servers, serverID, stackName, compose)
		// 部署成功 → 落参数边车(后续「升级」原位保留参数的凭据;失败只警示不翻转结果)。
		if out.OK {
			if werr := writeAppSidecar(r.Context(), servers, serverID, stackName, appDeploySidecar{
				TemplateID: req.TemplateID, Params: rendered.Used, CPUs: req.CPUs, Memory: req.Memory,
				DeployedAt: time.Now().UTC(),
			}); werr != nil {
				out.Output = strings.TrimSpace(out.Output + "\n⚠ 参数记录写入失败(后续无法走「升级」原位保留参数):" + werr.Error())
			}
		}

		// RustFS 联动:部署成功后自动登记为对象存储配置(尽力而为,独立超时,不影响部署结果)。
		if out.OK && tpl.Name == appAutoRegisterObjectStoreTemplate {
			rctx, rcancel := context.WithTimeout(r.Context(), appRegisterTimeout)
			out.ObjectStoreRegistered, out.ObjectStoreNote = registerRustFSObjectStore(rctx, objStores, aud, r, *srv, rendered.Used, req.Instance)
			rcancel()
		}
		// mysql/postgres 联动:部署成功后自动登记为数据库管理实例(尽力而为,独立超时;
		// 不做连接测试,容器首启初始化由管理页的连接测试兜底)。
		if out.OK && appAutoRegisterDBTemplates[tpl.Name] {
			rctx, rcancel := context.WithTimeout(r.Context(), appRegisterTimeout)
			out.DBRegistered, out.DBNote = registerDatabaseInstance(rctx, dbAdmins, aud, r, *srv, tpl.Name, stackName, rendered.Used, req.Instance)
			rcancel()
		}
		// redis 联动:部署成功后自动登记为 Redis 管理实例(尽力而为,独立超时;连通性由
		// 「Redis」管理页的连接测试兜底)。
		if out.OK && tpl.Name == appAutoRegisterRedisTemplate {
			rctx, rcancel := context.WithTimeout(r.Context(), appRegisterTimeout)
			out.RedisRegistered, out.RedisNote = registerRedisInstance(rctx, redisAdmins, aud, r, *srv, stackName, rendered.Used, req.Instance)
			rcancel()
		}
		// trivy-server 联动:部署成功后自动登记为安全中心扫描器(尽力而为,独立超时;
		// 连通性由「安全中心」扫描器的测试按钮兜底)。
		if out.OK && tpl.Name == appAutoRegisterScannerTemplate {
			rctx, rcancel := context.WithTimeout(r.Context(), appRegisterTimeout)
			out.ScannerRegistered, out.ScannerNote = registerScannerInstance(rctx, secScans, aud, r, *srv, stackName, rendered.Used, req.Instance)
			rcancel()
		}
		// nuclei-runner 联动:部署成功后自动登记为安全中心常驻执行容器(尽力而为,
		// 独立超时;容器在跑由「安全中心」的测试按钮兜底)。
		if out.OK && tpl.Name == appAutoRegisterNucleiRunnerTemplate {
			rctx, rcancel := context.WithTimeout(r.Context(), appRegisterTimeout)
			out.ScannerRegistered, out.ScannerNote = registerNucleiRunnerInstance(rctx, secScans, aud, r, *srv, stackName, req.Instance)
			rcancel()
		}
		// safeline 联动:部署成功后自动登记为安全中心 WAF 实例(尽力而为;API Token
		// 留空,由用户在雷池控制台生成后到安全中心补填)。
		if out.OK && tpl.Name == appAutoRegisterSafelineTemplate {
			rctx, rcancel := context.WithTimeout(r.Context(), appRegisterTimeout)
			out.ScannerRegistered, out.ScannerNote = registerSafelineInstance(rctx, secScans, aud, r, *srv, stackName, rendered.Used, req.Instance)
			rcancel()
		}

		detail := map[string]any{"template": tpl.Name, "ok": out.OK}
		if req.Instance != "" {
			detail["instance"] = req.Instance
			detail["stack"] = stackName
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionAppDeploy, TargetType: audit.TargetServer, TargetID: serverID,
			Detail: detail, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, out)
	}
}

// ---------- 参数边车(升级的凭据) ----------

// appParamsFileName 是应用市场部署的「生效参数边车文件」:部署/升级成功后写入受管栈目录
// (/opt/pipewright/stacks/<name>/),记录模板 ID、生效参数(含 secret)与资源限制。
// 升级靠它原位保留参数 —— 否则重新渲染时 secret 会重新生成,把既有实例的口令改掉。
// 安全姿态与 compose 文件一致(同目录明文;Stacks 详情本就能看 compose 全文)。
const appParamsFileName = ".appstore-params.json"

// appDeploySidecar 是边车文件内容。
type appDeploySidecar struct {
	TemplateID string            `json:"templateId"`
	Params     map[string]string `json:"params"`
	CPUs       string            `json:"cpus,omitempty"`
	Memory     string            `json:"memory,omitempty"`
	// DeployedAt 是本次部署/升级时间(「模板有更新」标记的比较基准)。旧边车无此字段 →
	// 零值早于一切模板更新时间 → 一律标「有更新」:升一次级即建立基准,之后判定准确。
	DeployedAt time.Time `json:"deployedAt,omitempty"`
}

// appSidecarMaxBytes 是边车文件尺寸上限(参数 ≤32 个、值 ≤256B,64KiB 绰绰有余)。
const appSidecarMaxBytes = 64 << 10

// cappedWriter 截断写入(超过 max 的字节静默丢弃;供边车读取限尺寸,防异常大文件撑爆内存)。
type cappedWriter struct {
	buf *bytes.Buffer
	max int
}

func (w cappedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if remain := w.max - w.buf.Len(); remain < len(p) {
		p = p[:max(remain, 0)]
	}
	_, err := w.buf.Write(p)
	return n, err
}

// writeAppSidecar 把生效参数写入目标机栈目录的边车文件(部署/升级成功后调用)。
func writeAppSidecar(ctx context.Context, svc target.Service, serverID, stackName string, sc appDeploySidecar) error {
	raw, err := json.Marshal(sc)
	if err != nil {
		return err
	}
	return svc.Upload(ctx, serverID, bytes.NewReader(raw), stacksBaseDir+"/"+stackName+"/"+appParamsFileName)
}

// readAppSidecar 读目标机栈目录的参数边车;文件不存在/读不到/损坏均返回 error(人读文案)。
func readAppSidecar(ctx context.Context, svc target.Service, serverID, stackName string) (*appDeploySidecar, error) {
	var buf bytes.Buffer
	if _, err := svc.Download(ctx, serverID, stacksBaseDir+"/"+stackName+"/"+appParamsFileName, cappedWriter{buf: &buf, max: appSidecarMaxBytes}); err != nil {
		return nil, fmt.Errorf("读取参数记录失败:%w", err)
	}
	var sc appDeploySidecar
	if err := json.Unmarshal(buf.Bytes(), &sc); err != nil || sc.TemplateID == "" {
		return nil, errors.New("参数记录损坏或为空")
	}
	return &sc, nil
}

// appDeployResponse 是部署/升级共用响应体(冻结契约;Params 含生成的 secret,仅此一次返回)。
type appDeployResponse struct {
	ServerID string            `json:"serverId"`
	Name     string            `json:"name"`
	OK       bool              `json:"ok"`
	Output   string            `json:"output"`
	Error    string            `json:"error"`
	Params   map[string]string `json:"params,omitempty"` // 生效参数(含生成的 secret,仅此一次)
	// RustFS 联动登记结果(仅 rustfs 模板且部署成功时出现;尽力而为,失败不影响 OK)。
	ObjectStoreRegistered bool   `json:"objectStoreRegistered,omitempty"`
	ObjectStoreNote       string `json:"objectStoreNote,omitempty"`
	// 数据库联动登记结果(仅 mysql/postgres 模板且部署成功时出现;尽力而为,失败不影响 OK)。
	DBRegistered bool   `json:"dbRegistered,omitempty"`
	DBNote       string `json:"dbNote,omitempty"`
	// Redis 联动登记结果(仅 redis 模板且部署成功时出现;尽力而为,失败不影响 OK)。
	RedisRegistered bool   `json:"redisRegistered,omitempty"`
	RedisNote       string `json:"redisNote,omitempty"`
	// 扫描器联动登记结果(仅 trivy-server 模板且部署成功时出现;尽力而为,失败不影响 OK)。
	ScannerRegistered bool   `json:"scannerRegistered,omitempty"`
	ScannerNote       string `json:"scannerNote,omitempty"`
}

// appStackName 推导生效栈名:实例名留空 = 模板名(兼容既有单实例);组合上限与 Stacks 一致。
func appStackName(tplName, instance string) string {
	if instance == "" {
		return tplName
	}
	return tplName + "-" + instance
}

// validateAppDeployTarget 是部署/升级共用的入参校验(instance/cpus/memory 白名单 + PVE 宿主机拦截)。
// 返回人读错误串("" = 通过)。
func validateAppDeployTarget(instance, cpus, memory string, srv *target.Server) string {
	if cpus != "" && !reCPUs.MatchString(cpus) {
		return "非法 CPU 上限(正整数/小数,如 0.5、1、2.5)"
	}
	if memory != "" && !reMemory.MatchString(memory) {
		return "非法内存上限(正整数 + 单位 b/k/m/g,如 512m、1g)"
	}
	if instance != "" && !reDockerTgt.MatchString(instance) {
		return "非法实例名(仅字母数字与 . _ -,不以 - 开头)"
	}
	if srv.IsPVENode() {
		return "PVE 集群物理机不允许作为应用部署目标"
	}
	return ""
}

// makeAppUpgradeContextHandler 返回 GET /api/servers/{id}/apps/upgrade-context?templateId=&instance=:
// 读既有实例的参数边车供升级表单预填。secret 现值一并回显(secrets: 名→现值)——安全姿态
// 与边车/compose 同级(同目录明文,Stacks 详情本就能看 compose 全文);升级表单预填后,
// 不动 = 保持、修改 = 覆盖,语义显式,不再是黑盒。无边车(旧版部署)→ found:false,但尝试
// 读栈目录旧 compose 反推非 secret 现值(legacy:true)供「重新部署」预填;反推不出 → 裸回。
func makeAppUpgradeContextHandler(apps appstore.Service, servers target.Service) http.HandlerFunc {
	type response struct {
		Found     bool              `json:"found"`
		Legacy    bool              `json:"legacy,omitempty"` // 无边车但反推出旧配置(仅「重新部署」预填用,不可原位升级)
		StackName string            `json:"stackName"`
		Params    map[string]string `json:"params"`  // 非 secret 参数现值 / legacy 反推值
		Secrets   map[string]string `json:"secrets"` // secret 参数现值(名→值;与 compose 明文同级姿态)
		CPUs      string            `json:"cpus"`    // 原部署的资源限制(升级沿用)
		Memory    string            `json:"memory"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if apps == nil || servers == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "应用商店/服务器服务未初始化")
			return
		}
		serverID := chi.URLParam(r, "id")
		templateID := r.URL.Query().Get("templateId")
		instance := strings.TrimSpace(r.URL.Query().Get("instance"))
		if templateID == "" {
			writeError(w, http.StatusBadRequest, "invalid_app_template", "templateId 必填")
			return
		}
		if instance != "" && !reDockerTgt.MatchString(instance) {
			writeError(w, http.StatusBadRequest, "invalid_app_deploy", "非法实例名(仅字母数字与 . _ -,不以 - 开头)")
			return
		}
		tpl, err := apps.Get(r.Context(), templateID)
		if err != nil {
			writeAppStoreError(w, err)
			return
		}
		if _, err := servers.Get(r.Context(), serverID); err != nil {
			writeServerError(w, err)
			return
		}
		stackName := appStackName(tpl.Name, instance)
		out := response{StackName: stackName, Params: map[string]string{}, Secrets: map[string]string{}}
		sc, err := readAppSidecar(r.Context(), servers, serverID, stackName)
		if err != nil {
			// 无边车(旧版部署)→ 读栈目录旧 compose 尽力反推非 secret 现值。「重新部署」
			// 预填用;found 仍 false(诚实:不可原位升级)。读 compose 独立超时,SSH 挂起不卡请求。
			lctx, lcancel := context.WithTimeout(r.Context(), appsInstancesTimeout)
			old := readStackCompose(lctx, servers, serverID, stackName)
			lcancel()
			if old != "" {
				if got := extractLegacyParams(tpl.ComposeYAML, old, tpl.Params); len(got) > 0 {
					out.Legacy = true
					out.Params = got
				}
			}
			writeJSON(w, http.StatusOK, out)
			return
		}
		out.Found = true
		out.CPUs, out.Memory = sc.CPUs, sc.Memory
		secretNames := map[string]bool{}
		for _, p := range tpl.Params {
			if p.Type == appstore.ParamTypeSecret {
				secretNames[p.Name] = true
			}
		}
		for k, v := range sc.Params {
			if secretNames[k] {
				out.Secrets[k] = v
			} else {
				out.Params[k] = v
			}
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// readStackCompose 读受管栈目录里的 compose 文件(「重新部署」的旧配置反推源);读不到/为空 → ""。
func readStackCompose(ctx context.Context, svc target.Service, serverID, stackName string) string {
	var buf bytes.Buffer
	if _, err := svc.Download(ctx, serverID, stacksBaseDir+"/"+stackName+"/"+composeFileName, cappedWriter{buf: &buf, max: composeMaxBytes}); err != nil {
		return ""
	}
	return buf.String()
}

// legacyAnchorMax 是反推锚文本的最大字节数:锚只取占位符紧邻字面量(不跨占位符/换行)。
const legacyAnchorMax = 32

// legacyValueMax 是反推参数值的上限(与 appstore 参数值校验上限一致)。
const legacyValueMax = 256

// extractLegacyParams 从旧 compose(渲染产物)尽力反推参数现值(含 secret:旧 compose
// 本就明文,反推预填后重新部署可保持原口令,不必重新生成):模板 compose 里每处 {{name}}
// 取紧邻锚文本(前后各 ≤32B,遇 {{ }} 或换行截断),在旧 compose 里按前锚(+后锚/行尾)
// 定位提取中间值;全部出现处一致才采纳,任何可疑(前锚缺失/定位不到/值空、含换行、超长)
// 一律跳过,该参数回落表单默认值(secret 空缺由部署端自动生成)。
func extractLegacyParams(tplCompose, oldCompose string, specs []appstore.ParamSpec) map[string]string {
	out := map[string]string{}
	for _, p := range specs {
		ph := "{{" + p.Name + "}}"
		var vals []string
		start := 0
		for {
			i := strings.Index(tplCompose[start:], ph)
			if i < 0 {
				break
			}
			i += start
			if prefix := anchorBefore(tplCompose, i); prefix != "" {
				if v, ok := extractAnchored(oldCompose, prefix, anchorAfter(tplCompose, i+len(ph))); ok {
					vals = append(vals, v)
				}
			}
			start = i + len(ph)
		}
		if len(vals) == 0 {
			continue
		}
		same := true
		for _, v := range vals[1:] {
			if v != vals[0] {
				same = false
				break
			}
		}
		if same {
			out[p.Name] = vals[0]
		}
	}
	return out
}

// anchorBefore 取 s[:i] 紧邻 i 的锚文本(≤32B,遇 {{ / }} / 换行停)。
func anchorBefore(s string, i int) string {
	j := i
	for i-j < legacyAnchorMax && j > 0 {
		if s[j-1] == '\n' || (j >= 2 && (s[j-2:j] == "{{" || s[j-2:j] == "}}")) {
			break
		}
		j--
	}
	return s[j:i]
}

// anchorAfter 取 s[i:] 紧邻 i 的锚文本(≤32B,遇 {{ / }} / 换行停)。
func anchorAfter(s string, i int) string {
	j := i
	for j-i < legacyAnchorMax && j < len(s) {
		if s[j] == '\n' || (j+2 <= len(s) && (s[j:j+2] == "{{" || s[j:j+2] == "}}")) {
			break
		}
		j++
	}
	return s[i:j]
}

// extractAnchored 在旧文本里按「前锚(+后锚或行尾)」提取参数值;可疑 → ok=false。
func extractAnchored(old, prefix, suffix string) (string, bool) {
	idx := strings.Index(old, prefix)
	if idx < 0 {
		return "", false
	}
	start := idx + len(prefix)
	var end int
	if suffix != "" {
		j := strings.Index(old[start:], suffix)
		if j < 0 || j > legacyValueMax {
			return "", false
		}
		end = start + j
	} else {
		j := strings.IndexByte(old[start:], '\n')
		if j < 0 {
			j = len(old[start:])
		}
		if j > legacyValueMax {
			return "", false
		}
		end = start + j
	}
	v := strings.TrimSpace(old[start:end])
	if v == "" || strings.ContainsAny(v, "\n\r") || len(v) > legacyValueMax {
		return "", false
	}
	return v, true
}

// makeUpgradeAppHandler 返回 POST /api/servers/{id}/apps/upgrade {templateId, instance?, params, cpus?, memory?}。
// 升级 = 读参数边车 → 本次显式给的参数覆盖边车现值(键缺省/留空 = 保持不变,secret 绝不重新生成)
// → 重新渲染 → 重走 Stacks 部署链路(compose up -d 只重建有变化的服务;数据卷按项目名保持挂载)。
// 请求带非空 cpus/memory 时覆盖原资源限制;成功后边车重写为最新生效值。
// 无边车(旧版部署/非应用市场栈)→ 200 + ok:false + 人读指引(诚实,不假装能升)。
// 升级不重跑部署联动登记(对象存储/数据库/Redis 首次部署已登记,参数未变时原地轮换无意义)。
func makeUpgradeAppHandler(apps appstore.Service, servers target.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		TemplateID string            `json:"templateId"`
		Params     map[string]string `json:"params"`
		Instance   string            `json:"instance,omitempty"`
		CPUs       string            `json:"cpus,omitempty"`
		Memory     string            `json:"memory,omitempty"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if apps == nil || servers == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "应用商店/服务器服务未初始化")
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
		req.Instance = strings.TrimSpace(req.Instance)
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
		if msg := validateAppDeployTarget(req.Instance, req.CPUs, req.Memory, srv); msg != "" {
			code := "invalid_app_deploy"
			if srv.IsPVENode() {
				writeError(w, http.StatusUnprocessableEntity, "pve_node_not_target", msg)
				return
			}
			writeError(w, http.StatusBadRequest, code, msg)
			return
		}
		stackName := appStackName(tpl.Name, req.Instance)
		if len(stackName) > 128 {
			writeError(w, http.StatusBadRequest, "invalid_app_deploy", "实例名过长:组合栈名超过 128 字符")
			return
		}
		out := appDeployResponse{ServerID: serverID, Name: stackName}
		auditOp := func() {
			recordAudit(r.Context(), aud, audit.Entry{
				Actor: auditActor, Action: auditActionAppUpgrade, TargetType: audit.TargetServer, TargetID: serverID,
				Detail: map[string]any{"template": tpl.Name, "stack": stackName, "ok": out.OK}, IP: clientIP(r),
			})
		}
		sc, err := readAppSidecar(r.Context(), servers, serverID, stackName)
		if err != nil {
			out.Error = "该实例没有部署参数记录(旧版部署或非应用市场栈),无法原位升级:可到 Stacks 用「更新」拉取同名可变镜像,或重新部署(会重新生成口令)"
			auditOp()
			writeJSON(w, http.StatusOK, out)
			return
		}
		// 合并参数:仅模板 schema 声明的键(模板新版删掉的参数静默丢弃,防 ErrUnknownParam);
		// 显式非空值覆盖边车现值,其余原位保留。
		merged := map[string]string{}
		for _, p := range tpl.Params {
			if v, ok := req.Params[p.Name]; ok && strings.TrimSpace(v) != "" {
				merged[p.Name] = strings.TrimSpace(v)
			} else if old, ok := sc.Params[p.Name]; ok {
				merged[p.Name] = old
			}
		}
		rendered, err := apps.Render(r.Context(), req.TemplateID, merged)
		if err != nil {
			writeAppStoreError(w, err)
			return
		}
		cpus, memory := sc.CPUs, sc.Memory
		if req.CPUs != "" {
			cpus = req.CPUs
		}
		if req.Memory != "" {
			memory = req.Memory
		}
		compose := rendered.Compose
		if compose, err = injectComposeLimits(compose, cpus, memory); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_app_deploy", "资源限制注入失败:"+err.Error())
			return
		}
		out.Params = rendered.Used
		// 同部署:up 含镜像拉取,不设主动超时(拉取失败 docker 自报错,客户端断开兜底取消)。
		out.OK, out.Output, out.Error = deployComposeToServer(r.Context(), servers, serverID, stackName, compose)
		// 升级成功 → 边车重写为最新生效参数/资源限制(下一轮升级的凭据;失败只警示不翻转结果)。
		if out.OK {
			if werr := writeAppSidecar(r.Context(), servers, serverID, stackName, appDeploySidecar{
				TemplateID: req.TemplateID, Params: rendered.Used, CPUs: cpus, Memory: memory,
				DeployedAt: time.Now().UTC(),
			}); werr != nil {
				out.Output = strings.TrimSpace(out.Output + "\n⚠ 参数记录写入失败(后续升级无法原位保留参数):" + werr.Error())
			}
		}
		auditOp()
		writeJSON(w, http.StatusOK, out)
	}
}

// ---------- 卸载(实例行的反向动作) ----------

// makeAppUninstallHandler 返回 POST /api/servers/{id}/apps/uninstall {stackName, deleteVolumes?}。
// 卸载 = 按项目标签删容器(可选连删该项目具名卷)→ 删受管目录(compose + 参数边车)→
// 「已部署实例」列表随之消失。与 Stacks「销毁」同纪律的纯 docker 标签操作,不依赖 compose
// CLI:容器/卷均按 com.docker.compose.project 标签定位,名字过 reDockerTgt 白名单(无 / 与
// 前导 -,rm 受管目录的路径段同受约束,无穿越),命令 array 化不拼 shell。任一步失败即停并
// 保留受管目录(实例仍列出,可重试;各步幂等:无容器/无卷/无目录都算该步成功)。
// 审计 action:appstore.uninstall;detail 只记栈名与是否删卷,不含 compose 内容。
func makeAppUninstallHandler(servers target.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		StackName     string `json:"stackName"`
		DeleteVolumes bool   `json:"deleteVolumes,omitempty"` // true = 连删该项目具名卷(数据不可恢复)
	}
	type response struct {
		ServerID string `json:"serverId"`
		Name     string `json:"name"`
		OK       bool   `json:"ok"`
		Output   string `json:"output"`
		Error    string `json:"error"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if servers == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务器服务未初始化")
			return
		}
		serverID := chi.URLParam(r, "id")
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		name := strings.TrimSpace(req.StackName)
		if !reDockerTgt.MatchString(name) || len(name) > 128 {
			writeError(w, http.StatusBadRequest, "invalid_app_deploy", "栈名非法(仅字母数字与 . _ -,不以 - 开头)")
			return
		}
		if _, err := servers.Get(r.Context(), serverID); err != nil {
			writeServerError(w, err)
			return
		}
		out := response{ServerID: serverID, Name: name}
		auditOp := func() {
			recordAudit(r.Context(), aud, audit.Entry{
				Actor: auditActor, Action: auditActionAppUninstall, TargetType: audit.TargetServer, TargetID: serverID,
				Detail: map[string]any{"stack": name, "volumes": req.DeleteVolumes, "ok": out.OK}, IP: clientIP(r),
			})
		}
		// 卸载各步通常秒级,与 Stacks 动作同款窗口;失败重试幂等(已删的容器/卷/目录跳过)。
		cctx, cancel := context.WithTimeout(r.Context(), stacksCmdTimeout)
		defer cancel()

		// 1) 删该项目全部容器(标签定位,无容器 = 幂等通过)。
		ids, err := projectContainerIDs(cctx, servers, serverID, name)
		if err != nil {
			if errors.Is(err, target.ErrNotFound) {
				writeServerError(w, err)
				return
			}
			if errors.Is(err, target.ErrCredentialNotFound) || errors.Is(err, target.ErrVaultUnconfigured) {
				auditOp()
				writeServerError(w, err)
				return
			}
			out.Error = humanServiceError(err)
			auditOp()
			writeJSON(w, http.StatusOK, out)
			return
		}
		notes := []string{}
		if len(ids) > 0 {
			if msg, ok := uninstallStep(cctx, servers, serverID, append([]string{"docker", "rm", "-f"}, ids...)); !ok {
				out.Error = "删除容器失败:" + msg
				auditOp()
				writeJSON(w, http.StatusOK, out)
				return
			}
			serverContainersCache.invalidate(serverID) // 容器集合已变 → 丢弃该台清单缓存
			notes = append(notes, fmt.Sprintf("已删除容器 %d 个", len(ids)))
		}
		// 2) 可选删该项目具名卷(compose 建的卷带项目标签;无卷 = 幂等通过)。
		if req.DeleteVolumes {
			vols, verr := projectVolumeNames(cctx, servers, serverID, name)
			if verr != nil {
				out.Error = "枚举数据卷失败:" + humanServiceError(verr)
				auditOp()
				writeJSON(w, http.StatusOK, out)
				return
			}
			if len(vols) > 0 {
				if msg, ok := uninstallStep(cctx, servers, serverID, append([]string{"docker", "volume", "rm"}, vols...)); !ok {
					out.Error = "删除数据卷失败:" + msg + "(容器已删;可重试卸载,或到「容器 → 数据卷」手动清理)"
					auditOp()
					writeJSON(w, http.StatusOK, out)
					return
				}
				notes = append(notes, "已删除数据卷:"+strings.Join(vols, ", "))
			}
		}
		// 3) 删受管目录(compose + 参数边车;目录不在 = 幂等通过)。目录删除放最后:前面失败时
		// 保留目录 = 实例仍列出,重试卸载有抓手。
		if msg, ok := uninstallStep(cctx, servers, serverID, []string{"rm", "-rf", stacksBaseDir + "/" + name}); !ok {
			out.Error = "清理受管目录失败:" + msg + "(容器已停删;可重试卸载)"
			auditOp()
			writeJSON(w, http.StatusOK, out)
			return
		}
		notes = append(notes, "已清理受管目录")
		out.OK, out.Output = true, strings.Join(notes, ";")
		auditOp()
		writeJSON(w, http.StatusOK, out)
	}
}

// uninstallStep 执行卸载的一步命令,返回 (人读错误摘要, 是否成功)。
func uninstallStep(ctx context.Context, svc target.Service, serverID string, cmd []string) (string, bool) {
	res, err := svc.Exec(ctx, serverID, cmd)
	if err != nil {
		return humanServiceError(err), false
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = "命令以非零状态退出"
		}
		return truncateLog(msg, 1024), false
	}
	return "", true
}

// projectVolumeNames 取某 compose 项目(按标签)的具名卷名(实现在 internal/composeops)。
func projectVolumeNames(ctx context.Context, svc target.Service, serverID, project string) ([]string, error) {
	return composeops.ProjectVolumeNames(ctx, svc, serverID, project)
}

// ---------- 已部署实例枚举(「升级」直达入口的数据底座) ----------

// cmdFindAppStacks 是定值 find 命令(array 化,无 shell、无用户输入;AC-SEC-02):列受管栈
// 目录下的 compose 文件与参数边车。有 compose = 受管栈;边车在则可原位升级,不在则按
// 栈名反推模板作「旧版部署」列出。目录不存在时 find 非零退出 + stderr 噪声,但 stdout
// 为空 → 调用方忽略 ExitCode,只解析 stdout(与 cmdDiscoverComposeFiles 同纪律)。
var cmdFindAppStacks = []string{"find", stacksBaseDir, "-maxdepth", "2",
	"(", "-name", composeFileName, "-o", "-name", appParamsFileName, ")"}

// cmdImagesByProject 列出该机全部运行容器的「compose 项目名|||镜像引用」(一次往返,
// 供实例版本展示;`docker ps --format` 的 Label 取法与 cmdComposeLabels 同纪律)。
var cmdImagesByProject = []string{"docker", "ps", "--format",
	`{{.Label "com.docker.compose.project"}}|||{{.Config.Image}}`}

// imagesByProject 解析 docker ps 输出为 项目名 → 镜像引用(去重,多容器栈取首见)。
func imagesByProject(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|||", 2)
		if len(parts) != 2 || parts[0] == "" {
			continue
		}
		if _, dup := m[parts[0]]; !dup {
			m[parts[0]] = strings.TrimSpace(parts[1])
		}
	}
	return m
}

// imageVersionTag 从镜像引用提取 tag("repo/name:tag" → tag;无 tag → "")。
func imageVersionTag(imageRef string) string {
	if i := strings.LastIndex(imageRef, ":"); i >= 0 && !strings.Contains(imageRef[i:], "/") {
		return imageRef[i+1:]
	}
	return ""
}

// appsInstancesConcurrency / appsInstancesTimeout 是逐台枚举的有界并发与单机窗口
// (find + 逐边车 cat,量级远小于 compose up)。
const (
	appsInstancesConcurrency = 6
	appsInstancesTimeout     = 30 * time.Second
)

// appInstanceDTO 是「已部署实例」的单条 DTO(冻结契约):应用商店部署在某台机器上的一个栈,
// 供前端渲染直达的「升级」入口。机器不可达也单独成条(reachable:false)——诚实告知,不静默吞机器。
type appInstanceDTO struct {
	ServerID   string `json:"serverId"`
	ServerName string `json:"serverName"`
	Reachable  bool   `json:"reachable"`
	Error      string `json:"error,omitempty"`
	StackName  string `json:"stackName,omitempty"`    // 生效栈名(= 受管目录名)
	TemplateID string `json:"templateId,omitempty"`   // 边车记录的模板 ID(未删模板时可靠)
	TemplateName string `json:"templateName,omitempty"`
	DisplayName  string `json:"displayName,omitempty"`
	Icon         string `json:"icon,omitempty"`
	Instance     string `json:"instance,omitempty"`    // 实例名后缀(stackName 去掉「模板名-」前缀)
	Version      string `json:"version,omitempty"`     // 运行镜像 tag(best-effort;如 "8.4"、"latest")
	CPUs         string `json:"cpus,omitempty"`
	Memory       string `json:"memory,omitempty"`
	DeployedAt   string `json:"deployedAt,omitempty"`  // RFC3339;旧边车无部署时间 → 空(未知)
	TemplateGone bool   `json:"templateGone,omitempty"`          // 边车模板 ID 已不在模板库
	UpgradeAvailable bool `json:"upgradeAvailable,omitempty"`    // 模板 UpdatedAt 晚于边车部署时间(旧边车零值时间 → 恒真,升级一次后判定准确)
	// Legacy = 旧版部署无边车:不可原位升级(无参数现值,重渲染会重生成口令),前端引导
	// 「重新部署」纳入(落边车,数据卷按项目名保留)。
	Legacy bool `json:"legacy,omitempty"`
}

// makeAllAppInstancesHandler 返回 GET /api/servers/apps/instances(认证,只读;批量聚合):
// 全部机器(排除 PVE 节点)上带参数边车的栈 = 应用商店部署的实例。逐台并行有界、各自独立
// (仿 makeAllServerContainersHandler);join 模板库补展示名/图标并判定「模板有更新」。
func makeAllAppInstancesHandler(apps appstore.Service, servers target.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if apps == nil || servers == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "应用商店/服务器服务未初始化")
			return
		}
		all, err := servers.List(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		tpls, err := apps.List(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		byID := make(map[string]appstore.Template, len(tpls))
		for _, t := range tpls {
			byID[t.ID] = t
		}

		targets := make([]*target.Server, 0, len(all))
		for _, srv := range all {
			if srv.IsPVENode() {
				continue // PVE 节点不是部署目标(见 validateAppDeployTarget),必无边车
			}
			targets = append(targets, srv)
		}
		rows := make([][]appInstanceDTO, len(targets))
		sem := make(chan struct{}, appsInstancesConcurrency)
		var wg sync.WaitGroup
		for i, srv := range targets {
			wg.Add(1)
			go func(i int, srv *target.Server) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				rows[i] = collectAppInstances(r.Context(), servers, srv, byID)
			}(i, srv)
		}
		wg.Wait()
		items := make([]appInstanceDTO, 0, len(rows))
		for _, rs := range rows {
			items = append(items, rs...)
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

// collectAppInstances 枚举一台机器上的应用商店实例:find 受管栈目录 → 按栈目录聚合
// (有 compose = 受管栈;栈名受 reDockerTgt 白名单约束)→ 有边车读边车(join 模板补
// 展示名/更新标记),无/坏边车按栈名反推模板作「旧版部署」条目(反推不到 → 大概率是
// Stacks 页手动部署的栈,不列)。机器不可达 → 单条 reachable:false。
func collectAppInstances(ctx context.Context, svc target.Service, srv *target.Server, tpls map[string]appstore.Template) []appInstanceDTO {
	cctx, cancel := context.WithTimeout(ctx, appsInstancesTimeout)
	defer cancel()
	res, err := svc.Exec(cctx, srv.ID, cmdFindAppStacks)
	if err != nil { // 传输层失败(SSH 不通)= 机器不可达;find 非零退出仍可能有 stdout,不在此列
		return []appInstanceDTO{{ServerID: srv.ID, ServerName: srv.Name, Error: err.Error()}}
	}
	type stackDir struct{ hasCompose, hasSidecar bool }
	dirs := make(map[string]*stackDir, 8)
	for _, line := range strings.Split(res.Stdout, "\n") {
		p := strings.TrimSpace(line)
		if p == "" || !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\r\x00") {
			continue
		}
		name := path.Base(path.Dir(p))
		if name == "" || name == "." || name == "/" || !reDockerTgt.MatchString(name) {
			continue
		}
		d := dirs[name]
		if d == nil {
			d = &stackDir{}
			dirs[name] = d
		}
		if path.Base(p) == appParamsFileName {
			d.hasSidecar = true
		} else {
			d.hasCompose = true
		}
	}
	// 运行镜像映射(版本展示):一次 docker ps;失败/为空不影响实例列表(版本留空)。
	imgs := map[string]string{}
	if res2, err := svc.Exec(cctx, srv.ID, cmdImagesByProject); err == nil && res2.ExitCode == 0 {
		imgs = imagesByProject(res2.Stdout)
	}
	names := make([]string, 0, len(dirs))
	for name, d := range dirs {
		if d.hasCompose {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]appInstanceDTO, 0, len(names))
	for _, name := range names {
		if dirs[name].hasSidecar {
			sc, err := readAppSidecar(cctx, svc, srv.ID, name)
			if err == nil {
				dto := appInstanceDTO{
					ServerID: srv.ID, ServerName: srv.Name, Reachable: true,
					StackName: name, TemplateID: sc.TemplateID, CPUs: sc.CPUs, Memory: sc.Memory,
					Version: imageVersionTag(imgs[name]),
				}
				if !sc.DeployedAt.IsZero() {
					dto.DeployedAt = sc.DeployedAt.UTC().Format(time.RFC3339)
				}
				if tpl, ok := tpls[sc.TemplateID]; ok {
					dto.TemplateName = tpl.Name
					dto.DisplayName = tpl.DisplayName
					dto.Icon = tpl.Icon
					if instance, found := strings.CutPrefix(name, tpl.Name+"-"); found {
						dto.Instance = instance
					}
					// 模板在部署之后又改过 = 有可升级内容。旧边车无部署时间 → 零值早于一切模板
					// 更新时间,自然标「有更新」:升一次级即建立基准,之后判定准确。
					if tpl.UpdatedAt.After(sc.DeployedAt) {
						dto.UpgradeAvailable = true
					}
				} else {
					dto.TemplateGone = true
				}
				out = append(out, dto)
				continue
			}
			// 边车读不出(损坏)→ 与无边车等价,降级为 legacy 条目
		}
		if tpl := matchLegacyTemplate(name, tpls); tpl != nil {
			dto := appInstanceDTO{
				ServerID: srv.ID, ServerName: srv.Name, Reachable: true, Legacy: true,
				StackName: name, TemplateID: tpl.ID, TemplateName: tpl.Name,
				DisplayName: tpl.DisplayName, Icon: tpl.Icon,
				Version: imageVersionTag(imgs[name]),
			}
			if instance, found := strings.CutPrefix(name, tpl.Name+"-"); found {
				dto.Instance = instance
			}
			out = append(out, dto)
		}
	}
	return out
}

// matchLegacyTemplate 按栈名反推模板:栈名 == 模板名,或以「模板名-」开头(应用商店
// 的栈名规则)。多模板名互为前缀时取最长者(更精确)。无匹配 → nil。
func matchLegacyTemplate(stackName string, tpls map[string]appstore.Template) *appstore.Template {
	var best *appstore.Template
	for _, tpl := range tpls {
		if stackName == tpl.Name || strings.HasPrefix(stackName, tpl.Name+"-") {
			t := tpl
			if best == nil || len(t.Name) > len(best.Name) {
				best = &t
			}
		}
	}
	return best
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
