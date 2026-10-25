// Package httpapi is the single outward-facing surface of the platform.
// Domain packages never touch HTTP directly; they are wired in here.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/huangchengsir/pipewright/internal/ai"
	"github.com/huangchengsir/pipewright/internal/aliyunecs"
	"github.com/huangchengsir/pipewright/internal/anomaly"
	"github.com/huangchengsir/pipewright/internal/approval"
	"github.com/huangchengsir/pipewright/internal/appstore"
	"github.com/huangchengsir/pipewright/internal/artifactstore"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/backup"
	"github.com/huangchengsir/pipewright/internal/ceph"
	"github.com/huangchengsir/pipewright/internal/certdeploy"
	"github.com/huangchengsir/pipewright/internal/certmgmt"
	"github.com/huangchengsir/pipewright/internal/chain"
	"github.com/huangchengsir/pipewright/internal/cron"
	"github.com/huangchengsir/pipewright/internal/dagrun"
	"github.com/huangchengsir/pipewright/internal/dbadmin"
	"github.com/huangchengsir/pipewright/internal/deploy"
	"github.com/huangchengsir/pipewright/internal/diskadmin"
	"github.com/huangchengsir/pipewright/internal/dnsprovider"
	"github.com/huangchengsir/pipewright/internal/fileadmin"
	"github.com/huangchengsir/pipewright/internal/firewall"
	"github.com/huangchengsir/pipewright/internal/i18n"
	"github.com/huangchengsir/pipewright/internal/identity"
	"github.com/huangchengsir/pipewright/internal/labels"
	"github.com/huangchengsir/pipewright/internal/library"
	"github.com/huangchengsir/pipewright/internal/maintenance"
	"github.com/huangchengsir/pipewright/internal/metrics"
	"github.com/huangchengsir/pipewright/internal/mfa"
	"github.com/huangchengsir/pipewright/internal/notify"
	"github.com/huangchengsir/pipewright/internal/oauth"
	"github.com/huangchengsir/pipewright/internal/objectstore"
	"github.com/huangchengsir/pipewright/internal/opschat"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/platformhttps"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/pveadmin"
	"github.com/huangchengsir/pipewright/internal/redisadmin"
	"github.com/huangchengsir/pipewright/internal/retention"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/runner"
	"github.com/huangchengsir/pipewright/internal/secscan"
	"github.com/huangchengsir/pipewright/internal/servercmd"
	"github.com/huangchengsir/pipewright/internal/servicereg"
	"github.com/huangchengsir/pipewright/internal/srcupdate"
	"github.com/huangchengsir/pipewright/internal/ssar"
	"github.com/huangchengsir/pipewright/internal/sshtunnel"
	"github.com/huangchengsir/pipewright/internal/systemcfg"
	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/trigger"
	"github.com/huangchengsir/pipewright/internal/vault"
	"github.com/huangchengsir/pipewright/internal/version"
	"github.com/huangchengsir/pipewright/internal/waitnode"
)

const (
	cookieSession = "pipewright_session"
	cookieCsrf    = "pipewright_csrf"
	// cookieLoginScheme 标记「该浏览器最近一次经 HTTPS 建立会话」(值固定 "https")。
	cookieLoginScheme = "pipewright_login_scheme"
	headerCsrf        = "X-CSRF-Token"
)

// contextKey 用于 context 值键,避免冲突。
type contextKey int

const contextKeySession contextKey = 1

// Option 配置 New 构建的路由(如注入凭据保险库)。
type Option func(*options)

type options struct {
	opsChat              *opschat.Service
	vault                vault.Vault
	projects             project.Service
	triggers             trigger.Service
	cron                 cron.Service
	chain                chain.Service
	concurrency          run.ConcurrencyService
	parameters           run.ParameterService
	pipelines            pipeline.Service
	pipelineSettings     pipeline.SettingsService
	runs                 run.Service
	runSub               runSubscriber
	specLoader           dagrun.SpecLoader
	feedback             run.FeedbackService
	secretSource         *RunSecretSource
	receiver             *trigger.Receiver
	webhookForwarder     *WebhookForwarder
	audit                audit.Recorder
	account              accountService
	identity             identity.Service
	aiSettings           ai.Service
	aiAnalyzer           ai.RepoAnalyzer
	runDiffer            ai.RunDiffer
	sourceReader         SourceReader
	refsLister           RefsLister
	runnerConfig         runner.Service
	servers              target.Service
	labels               labels.Service
	notifications        notify.Service
	deployer             deploy.Service
	anomaly              anomaly.Service
	anomalyInterval      int // 后台定时检测间隔(秒);0=已关闭。供前端显示节奏。
	anomalyCooldown      int // 同条件告警去重冷却窗口(秒)。
	metricsHistory       metrics.Service
	ssar                 ssar.Service
	oauth                oauth.Service
	approvalCoord        *approval.Coordinator
	approvalStore        *approval.Store
	approvalSigner       *approval.Signer
	waitCoord            *waitnode.Coordinator
	doraMetrics          run.MetricsService
	templates            library.TemplateService
	varGroups            library.VarGroupService
	customNodes          library.CustomNodeService
	artifactStore        *artifactstore.Store
	retention            *retention.Service
	backupSvc            *backup.Service
	maintenance          *maintenance.Service
	projectMirrorCleaner func(ctx context.Context, repoURL string)
	certMgmt             certmgmt.Service
	certDeploys          *certdeploy.Service
	dnsProviders         dnsprovider.Service
	aliyunEcs            aliyunecs.Service
	objectStores         objectstore.Service
	serviceReg           servicereg.Service
	platformHTTPS        platformhttps.Service
	firewallSvc          firewall.Service
	systemConfig         systemcfg.Service
	appStore             appstore.Service
	dbAdmin              dbadmin.Service
	redisAdmin           redisadmin.Service
	secScan              secscan.Service
	pveAdmin             pveadmin.Service
	diskAdmin            diskadmin.Service
	fileAdmin            fileadmin.Service
	serverCmds           *servercmd.Service
	registryHub          registryHubService
	reexecGuard          ReexecGuard
	mesh                 *meshAdminDeps
	ceph                 ceph.Service
	mfa                  *mfa.Service
	artifactExporter     artifactImageExporter
	artifactTransfers    artifactTransferService
	sshTunnels           *sshtunnel.Service
}

// WithCeph 注入 Ceph 集群部署与管理服务(internal/ceph),在 admin 组挂载 /api/ceph/* 路由
// (集群登记/部署管线/状态 + 主机/OSD/存储池/RBD/CephFS 管理;GET 过 auth,写方法过
// auth + CSRF + 审计)。不传则 Ceph 功能整体不可用(端点 503)。
func WithCeph(s ceph.Service) Option {
	return func(o *options) { o.ceph = s }
}

// WithSSHTunnels 注入 SSH 隧道服务(internal/sshtunnel;0099),挂载 /api/tunnels* 路由
// (local -L / remote -R / dynamic -D 隧道的配置 CRUD、启停/重启/测试与 helper 日志;
// GET 过 auth,写方法过 auth + CSRF + 审计)。隧道以独立 helper 子进程运行,宿主重启
// 不杀(心跳收养)。不传则隧道功能整体不可用(端点 503)。
func WithSSHTunnels(s *sshtunnel.Service) Option {
	return func(o *options) { o.sshTunnels = s }
}

// WithDiskAdmin 注入「服务器磁盘管理」服务(internal/diskadmin),在 admin 组挂载
// /api/diskadmin/servers/{id}/* 路由(块设备/LVM 拓扑浏览 + 分区/格式化/挂载/扩缩容/
// LVM 变更;只读默认 + 显式解锁,全部变更留审计)。不传则磁盘管理功能整体不可用
// (端点 503)。
func WithDiskAdmin(s diskadmin.Service) Option {
	return func(o *options) { o.diskAdmin = s }
}

// WithFileAdmin 注入「服务器文件查看」服务(internal/fileadmin),在 admin 组挂载
// /api/fileadmin/servers/{id}/* 路由(目录列表 + 小文本查看/编辑 + 流式下载;
// GET 过 auth,PUT 过 auth + CSRF + 审计,编辑前服务端先同目录备份)。不传则
// 文件查看功能整体不可用(端点 503)。
func WithFileAdmin(s fileadmin.Service) Option {
	return func(o *options) { o.fileAdmin = s }
}

// WithPVEAdmin 注入 PVE 集群纳管与管理服务(internal/pveadmin):
//   - admin 组挂载 /api/pve/*(集群视图/Token 配置/测连/只读开关/删除/总览/VM 列表/
//     电源操作/任务;Token 密封入 vault,响应绝无密文);
//   - 服务器登记钩子:POST /api/servers 成功后自动探测 PVE 并建池(svc 为 nil 时跳过);
//   - 受保护组只读 GET /api/server-pools(池列表,成员可见)。
//
// 不传则 PVE 管理面不可用(端点 503)、登记不做探测。
func WithPVEAdmin(s pveadmin.Service) Option {
	return func(o *options) { o.pveAdmin = s }
}

// WithReexecGuard 注入自更新 re-exec 门槛(活跃任务判定 + 托管子进程清理钩子;
// 不传 = 无门槛,更新后直接重启)。
func WithReexecGuard(g ReexecGuard) Option {
	return func(o *options) { o.reexecGuard = g }
}

// WithMesh 注入集群(对等节点 + EasyTier)依赖,挂载两组路由:
//   - 根级 /api/mesh/*(与 webhook 同级,豁免 session/CSRF;节点签名 + ACL 鉴权):
//     hello 发现握手 / servers-metrics / projects / runs / push / SSE 事件回流;
//   - 受保护组内 /api/mesh/{settings,acl,peers,overview,...}(admin session + CSRF):
//     本节点配置、授权单、节点注册表、集群页聚合与对端代理查询。
//
// 不传则集群功能整体不可用(端点不挂载)。nil 子依赖(如 status/supervisor)由各
// handler 优雅降级(503/空态),不阻断其余功能。
func WithMesh(d meshAdminDeps) Option {
	return func(o *options) { o.mesh = &d }
}

// WithArtifactStore 注入制品库(Story 8-16):挂载产物下载端点
// GET /api/runs/{id}/artifacts/{artifactId}/download(按 reference 句柄取真字节)。nil → 端点 503。
func WithArtifactStore(s *artifactstore.Store) Option {
	return func(o *options) { o.artifactStore = s }
}

// WithArtifactImageTransfer 注入镜像导出器(*build.ImageTransfer):供产物管理面
// GET /api/runs/{id}/artifacts/{artifactId}/image-archive 做服务端 pull + docker save
// 流式导出。nil → 该端点 503(文件类产物下载不受影响)。
func WithArtifactImageTransfer(e artifactImageExporter) Option {
	return func(o *options) { o.artifactExporter = e }
}

// WithArtifactTransfer 注入产物跨环境传输服务(*build.ArtifactTransferService 原样满足):
// 产物管理面挂载 POST /api/runs/{id}/artifacts/transfer(发起异步传输,复用 mesh_push
// 推送通道,对端自动建「被推送源」项目落修订)+ GET/DELETE /api/artifacts/transfers/{id}
// (状态轮询/取消)。不传则端点不挂载。
func WithArtifactTransfer(s artifactTransferService) Option {
	return func(o *options) { o.artifactTransfers = s }
}

// WithApprovals 注入审批门协调器 + 持久层(Story 8-4),挂载 /api/runs/{id}/{approve,reject,approvals} 路由。
// 不传则审批端点返回 503。
func WithApprovals(coord *approval.Coordinator, store *approval.Store) Option {
	return func(o *options) {
		o.approvalCoord = coord
		o.approvalStore = store
	}
}

// WithSpecLoader 注入「流水线即代码」感知的 spec 加载器(与执行器同一实例,main 装配):
// 供失败运行按节点恢复(POST /api/runs/{id}/resume)做配置一致性校验。nil → 恢复端点 503。
func WithSpecLoader(l dagrun.SpecLoader) Option {
	return func(o *options) { o.specLoader = l }
}

// WithApprovalLinks 注入审批链接签名器(「从通知直接审批」),挂载**公开**端点(豁免 requireAuth +
// CSRF,token 即认证):GET /approvals(确认页,无副作用)+ POST /approvals/act(解析门)。
// signer 为 nil / 禁用(无 master key)时,公开页提示「链接不可用」、不解析任何门。
func WithApprovalLinks(signer *approval.Signer) Option {
	return func(o *options) { o.approvalSigner = signer }
}

// WithWaitNode 注入「等待」节点(wait)的手动跳过协调器,挂载 /api/runs/{id}/{waits,skip-wait}
// 路由。不传则等待节点照常执行、只是不可查询/跳过(端点 503)。
func WithWaitNode(coord *waitnode.Coordinator) Option {
	return func(o *options) { o.waitCoord = coord }
}

// WithDoraMetrics 注入 DORA 指标只读聚合服务(FR-8-15),挂载 GET /api/metrics/dora 路由
// (认证 + 只读;projectId 可选过滤 + window 时间窗口)。对既有运行数据做纯聚合,无新表、无副作用。
// 不传则该端点返回 503(服务未初始化)。
func WithDoraMetrics(s run.MetricsService) Option {
	return func(o *options) { o.doraMetrics = s }
}

// WithTemplates 注入流水线模板服务(FR-8-13 复用基座),挂载 /api/templates* 与
// POST /api/projects/{id}/pipeline/apply-template 路由。GET 过 auth;写方法过 auth + CSRF + 审计。
// 不传则相关端点返回 503(服务未初始化)。
func WithTemplates(s library.TemplateService) Option {
	return func(o *options) { o.templates = s }
}

// WithVariableGroups 注入变量组服务(FR-8-13 复用基座),挂载 /api/variable-groups* 路由。
// GET 过 auth;POST/PUT/DELETE 过 auth + CSRF + 审计。secret 变量只存/回 credentialId + 掩码,绝无明文。
// 不传则相关端点返回 503(服务未初始化)。
func WithVariableGroups(s library.VarGroupService) Option {
	return func(o *options) { o.varGroups = s }
}

// WithCustomNodes 注入自定义节点服务(复用库 Tier 2),挂载 /api/custom-nodes* 路由。
// GET 过 auth;POST/PUT/DELETE 过 auth + CSRF + 审计。config 为自由 KV 快照,不含明文 secret。
// 不传则相关端点返回 503(服务未初始化)。
func WithCustomNodes(s library.CustomNodeService) Option {
	return func(o *options) { o.customNodes = s }
}

// WithVault 注入凭据保险库,挂载 /api/credentials* 路由。
// 不传则保险库相关端点不可用(返回 vault_unconfigured)。
func WithVault(v vault.Vault) Option {
	return func(o *options) { o.vault = v }
}

// WithProjects 注入项目服务,挂载 /api/projects* 路由。
// 不传则项目相关端点返回 503(服务未初始化)。
func WithProjects(s project.Service) Option {
	return func(o *options) { o.projects = s }
}

// WithCron 注入定时触发配置服务,挂载 /api/projects/{id}/cron 路由(Story 8-6)。
func WithCron(s cron.Service) Option {
	return func(o *options) { o.cron = s }
}

// WithChain 注入流水线串联配置服务,挂载 /api/projects/{id}/chain 路由(FR-8-11)。
// 不传则该端点返回 503(服务未初始化)。GET 过 auth;PUT 过 auth + CSRF。
func WithChain(s chain.Service) Option {
	return func(o *options) { o.chain = s }
}

// WithConcurrency 注入项目级并发上限配置服务,挂载 /api/projects/{id}/concurrency 路由(FR-8-10)。
// 不传则该端点返回 503(服务未初始化)。GET 过 auth;PUT 过 auth + CSRF。
func WithConcurrency(s run.ConcurrencyService) Option {
	return func(o *options) { o.concurrency = s }
}

// WithParameters 注入项目级类型化运行参数定义服务,挂载 /api/projects/{id}/parameters 路由(P0 typed params)。
// 不传则该端点返回 503;手动触发亦据此校验提供的参数(无定义则透传,向后兼容)。
func WithParameters(s run.ParameterService) Option {
	return func(o *options) { o.parameters = s }
}

// WithTriggers 注入触发配置服务,挂载 /api/projects/{id}/trigger* 路由。
// 不传则触发相关端点返回 503(服务未初始化)。
func WithTriggers(s trigger.Service) Option {
	return func(o *options) { o.triggers = s }
}

// WithPipelines 注入流水线配置服务,挂载 /api/projects/{id}/pipeline 路由(Story 2.2)。
// 不传则流水线相关端点返回 503(服务未初始化)。
func WithPipelines(s pipeline.Service) Option {
	return func(o *options) { o.pipelines = s }
}

// WithPipelineSettings 注入构建/部署配置服务,挂载 /api/projects/{id}/pipeline/settings 路由(Story 2.4)。
// 独立于 2-2 的 pipeline spec;不传则该端点返回 503(服务未初始化)。
func WithPipelineSettings(s pipeline.SettingsService) Option {
	return func(o *options) { o.pipelineSettings = s }
}

// WithRuns 注入运行服务与事件订阅源(worker pool),挂载 /api/runs* 路由(含 SSE)。
// sub 用于 SSE 订阅运行状态/步骤事件(不轮询 DB);为 nil 时 events 端点返回 503。
// 不传则运行相关端点返回 503(服务未初始化)。
func WithRuns(s run.Service, sub runSubscriber) Option {
	return func(o *options) {
		o.runs = s
		o.runSub = sub
	}
}

// WithDiagnosisFeedback 注入诊断反馈服务(Story 7.5;FR-26),挂载
// POST /api/runs/{id}/diagnosis/feedback(认证 + CSRF)+ GET /api/settings/diagnosis-stats(认证)。
// 对 ready 诊断 👍/👎(👎 可附正确根因,脱敏 + 长度上限);反馈 upsert by runID、汇入统计(知识库种子)。
// run 无诊断 → 422 no_diagnosis;不传则相关端点返回 503(服务未初始化)。
func WithDiagnosisFeedback(s run.FeedbackService) Option {
	return func(o *options) { o.feedback = s }
}

// WithSecretSource 注入 run 凭据解析器(红线修):诊断/反馈端点据此构建登记了该 run 真实凭据的
// Masker,出网/落库前脱敏(项目/registry/secret 变量明文 → [MASKED])。不传则降级为只登记桩 secret。
func WithSecretSource(s *RunSecretSource) Option {
	return func(o *options) { o.secretSource = s }
}

// WithWebhooks 注入 webhook 接收器(Story 3.2),挂载**公开**端点 POST /api/webhooks/{token}
// (在 requireAuth/CSRF 白名单外;靠签名校验)。不传则该端点返回 503。
func WithWebhooks(rc *trigger.Receiver) Option {
	return func(o *options) { o.receiver = rc }
}

// WithWebhookForwarder 注入 webhook 集群转发器:公网收到本地不存在的 token 时,把投递
// 经 mesh 转发给 token 所属的对端环境并透传响应(见 webhook_forward.go)。不传则该端点
// 维持纯本地行为(token 不存在一律 404)。
func WithWebhookForwarder(f *WebhookForwarder) Option {
	return func(o *options) { o.webhookForwarder = f }
}

// WithAudit 注入审计 Recorder(Story 1.4),挂载 GET /api/audit 查询端点,并令既有
// 敏感操作 handler 在业务成功后追加审计行。不传则 /api/audit 返回 503,且写操作不审计。
func WithAudit(rec audit.Recorder) Option {
	return func(o *options) { o.audit = rec }
}

// WithAccount 注入账户设置服务(Story 1.7),挂载 /api/account/* 路由
// (改口令 / 会话列表 / 撤销会话)。不传则相关端点返回 503。
func WithAccount(s accountService) Option {
	return func(o *options) { o.account = s }
}

// WithIdentity 注入普通用户体系服务,挂载:
//   - 公开:POST /api/auth/register、GET /api/auth/invitations/{token};
//   - 管理员:GET /api/users、/api/invitations*、/api/projects/{id}/members*;
//   - 并为受保护 /api 组启用普通用户默认拒绝门(memberGate)。
//
// 不传则相关端点返回 503;普通用户会话(memberGate 依赖 identity)一律拒绝。
func WithIdentity(s identity.Service) Option {
	return func(o *options) { o.identity = s }
}

// WithAISettings 注入可配置 AI 提供商服务(Story 7.1),挂载 /api/settings/ai* 路由
// (GET auth;PUT/test auth + CSRF)。不传则相关端点返回 503;AI 未配置/未启用时
// 下游优雅禁用,核心 CI/CD 完全不依赖此服务(NFR-10)。
func WithAISettings(s ai.Service) Option {
	return func(o *options) { o.aiSettings = s }
}

// WithRegistryHub 注入内置本地 registry 服务(registryhub),挂载 /api/settings/registry*
// 与 /api/registry/* 路由(读写/部署/状态/清理/daemon.json 手动下发)。不传则相关端点 503。
// PUT 只写库;触碰机器的 deploy/daemon-apply/prune 均为显式 POST(认证 + CSRF + 审计)。
func WithRegistryHub(s registryHubService) Option {
	return func(o *options) { o.registryHub = s }
}

// WithAIGenerate 注入仓库分析器(go-git 浅克隆),挂载 /api/projects/{id}/pipeline/ai-generate
// 与 ai-apply 路由(Story 2.5)。generate 复用已注入的 ai.Service + projects + vault;
// apply 复用 pipelines + pipelineSettings + triggers。analyzer 为 nil 时 generate 返回 503。
func WithAIGenerate(a ai.RepoAnalyzer) Option {
	return func(o *options) { o.aiAnalyzer = a }
}

// WithRunDiff 注入成功/失败差异计算器(go-git 克隆 + tree diff),挂载 GET /api/runs/{id}/diff
// 路由(Story 7.3;FR-25)。复用已注入的 runs(取本次 run + LastSuccessfulRun)+ projects + vault
// (取 repoURL/凭据)。不传则 diff 端点返回 503(服务未初始化)。
func WithRunDiff(d ai.RunDiffer) Option {
	return func(o *options) { o.runDiffer = d }
}

// WithSource 注入只读源码读取器(go-git 浅克隆),挂载 /api/projects/{id}/source/{tree,blob}
// 路由(Story 3.6;FR-4 预埋,7-4 前端消费)。复用已注入的 projects + vault 取仓库凭据。
// 不传则 source 端点返回 503(服务未初始化)。
func WithSource(reader SourceReader) Option {
	return func(o *options) { o.sourceReader = reader }
}

// WithRefs 注入「列仓库分支/tag」能力(代码管理区 repocache;Story 8-18)。nil → 端点 503。
func WithRefs(lister RefsLister) Option {
	return func(o *options) { o.refsLister = lister }
}

// WithRunnerConfig 注入项目「远程构建 runner」配置服务(FR-8-14 续)。nil → 端点 503。
func WithRunnerConfig(svc runner.Service) Option {
	return func(o *options) { o.runnerConfig = svc }
}

// WithServers 注入目标服务器服务(Story 4.1;internal/target 通用 SSH 层),挂载
// /api/servers* 路由(GET auth;写方法 + test auth + CSRF)。SSH 私钥/口令仅经 vault
// 密文,响应/错误绝无明文。不传则相关端点返回 503(服务未初始化)。
func WithServers(s target.Service) Option {
	return func(o *options) { o.servers = s }
}

// WithLabels 注入机器标签登记处(internal/labels),挂载 /api/labels* 路由
// (GET 过 auth;POST/DELETE 写方法过 auth + CSRF)。标签可先建后挂(悬置标签),
// 各选择器下拉的候选集 = 登记处 ∪ 机器实际标签。不传则相关端点返回 503。
func WithLabels(s labels.Service) Option {
	return func(o *options) { o.labels = s }
}

// WithServerCommands 注入批量命令服务(internal/servercmd),挂载 POST /api/servers/commands/batch
// (写:auth + CSRF + 审计)与 GET /api/servers/commands/runs[/{runId}](历史回看,只读)。
// 复用 WithServers 注入的 target.Service 做 SSH 执行 + 本地库存历史(保最近 200 次)。
// 不传则相关端点返回 503(服务未初始化)。
func WithServerCommands(s *servercmd.Service) Option {
	return func(o *options) { o.serverCmds = s }
}

// WithDeploy 注入部署执行服务(Story 4.2;internal/deploy,FR-10),挂载
// POST /api/runs/{id}/deploy 路由(认证 + CSRF;本期同步执行返回最终 targets)。
// 经 target.Exec 把产物按 type 部署到目标机(命令 array 化,AC-SEC-02);输出/message 绝无明文密钥。
// 不传则该端点返回 503(服务未初始化)。
func WithDeploy(s deploy.Service) Option {
	return func(o *options) { o.deployer = s }
}

// WithNotifications 注入通知渠道服务(Story 5.1;FR-19),挂载 /api/notifications/channels* 路由
// (GET auth;POST/PUT/DELETE/test auth + CSRF)。敏感字段(SMTP 密码)经 vault 加密入库、响应仅
// hasPassword。不传则相关端点返回 503(服务未初始化)。
func WithNotifications(s notify.Service) Option {
	return func(o *options) { o.notifications = s }
}

// WithRetention 注入运行数据保留服务,挂载 /api/retention/config 路由(GET auth;PUT auth + CSRF)。
// 不传则相关端点返回 503。
func WithRetention(s *retention.Service) Option {
	return func(o *options) { o.retention = s }
}

// WithBackup 注入平台备份恢复服务(internal/backup),挂载 /api/backup/* 路由
// (策略/立即备份/列表/下载/删除/对象存储上传/在线恢复;写方法过 auth + CSRF + 审计)。
// 不传则端点 503。
func WithBackup(s *backup.Service) Option {
	return func(o *options) { o.backupSvc = s }
}

// WithMaintenance 注入磁盘维护服务,挂载 POST /api/maintenance/cleanup(auth + CSRF + 审计;
// target 枚举白名单)。与后台定时任务共用同一套清理实现。不传则端点返回 503。
func WithMaintenance(s *maintenance.Service) Option {
	return func(o *options) { o.maintenance = s }
}

// WithProjectMirrorCleaner 注入「项目删除后回收仓库镜像」回调(磁盘治理):项目行删除成功
// 且无其它项目共用同一 repo_url 时,删除本地 git 镜像(best-effort,残余由定时任务兜底)。
// 不传则项目删除保持旧行为(镜像留待定时兜底回收)。
func WithProjectMirrorCleaner(fn func(ctx context.Context, repoURL string)) Option {
	return func(o *options) { o.projectMirrorCleaner = fn }
}

// WithCertMgmt 注入证书管理服务(acme.sh 自动签发/续期 + 手动导入),挂载 /api/certmgmt/* 路由
// (GET auth;写方法 auth + CSRF + 审计;create 为异步签发,201 立即返回 pending 行)。
// 签发引擎(acme.sh 宿主机脚本)运行在服务注册网关主机上,经已注入的 target.Service 编排。
// 不传则相关端点返回 503(服务未初始化)。
func WithCertMgmt(s certmgmt.Service) Option {
	return func(o *options) { o.certMgmt = s }
}

// WithCertDeploys 注入证书部署目标服务(internal/certdeploy;0100),挂载 /api/cert-deploys*
// 路由(GET auth;写方法 auth + CSRF + 审计;deploy 为同步执行,总预算 2 分钟)。
// 证书 PEM 进程内解密直传 SSH,绝不回显;不传则相关端点返回 503。
func WithCertDeploys(s *certdeploy.Service) Option {
	return func(o *options) { o.certDeploys = s }
}

// WithDNSProviders 注入 DNS 提供商集成层服务(R3 E3.1–E3.4),挂载 /api/dns/providers* 路由
// (GET auth;POST/DELETE/verify auth + CSRF + 审计)。
// DNS token 经已注入的 vault 加密入库、apply 时即用即弃,响应/审计/日志绝无明文 token。
// 不传则相关端点返回 503(服务未初始化)。
func WithDNSProviders(s dnsprovider.Service) Option {
	return func(o *options) { o.dnsProviders = s }
}

// WithAliyunEcs 注入阿里云 ECS 批量导入服务(设置→阿里云 ECS),挂载 /api/aliyun/* 路由
// (GET auth;POST/PUT/DELETE/test/import auth + CSRF + 审计)。
// AccessKeySecret 经 vault.SealSecret 密文入库、拉实例/导入时即用即弃,响应/审计/日志
// 绝无明文;AccessKeyId 非机密可回显。不传则相关端点返回 503(服务未初始化)。
func WithAliyunEcs(s aliyunecs.Service) Option {
	return func(o *options) { o.aliyunEcs = s }
}

// WithServiceReg 注入「服务注册网关」(nginx)服务,挂载 /api/servicereg/* 路由
// (GET auth;写方法 auth + CSRF + 审计)。
// 不传则相关端点返回 503(服务未初始化)。
func WithServiceReg(s servicereg.Service) Option {
	return func(o *options) { o.serviceReg = s }
}

// WithPlatformHTTPS 注入「平台 HTTPS 访问」(宿主 nginx 自动配置)服务,挂载
// /api/platform-https/* 路由(GET auth;写方法 auth + CSRF + 审计)。
// 平台自身 Web 页面经宿主 nginx 发布为 HTTPS:下发证书 + conf.d vhost(443 ssl +
// 80→443 跳转)+ nginx -t + reload;证书复用证书管理模块。不传则相关端点返回 503。
func WithPlatformHTTPS(s platformhttps.Service) Option {
	return func(o *options) { o.platformHTTPS = s }
}

// WithFirewall 注入「集群防火墙访问限制」服务,挂载 /api/firewall/* 路由
// (GET auth;写方法 auth + CSRF + 审计)。两步生效:apply 临时写运行时 iptables,
// confirm 经真实来源门(clientIP 信任模型,防经 nginx 伪装本机)固化,rollback/disable
// 为逃生通道。不传则相关端点返回 503。
func WithFirewall(s firewall.Service) Option {
	return func(o *options) { o.firewallSvc = s }
}

// WithSystemConfig 注入「系统级运行时配置」服务,挂载 /api/system/config 路由
// (GET auth;PUT auth + CSRF + 审计)。public_url 供通知审批链接 / PR 回写 target_url
// 使用,运行时修改即时生效(取代环境变量 PIPEWRIGHT_PUBLIC_URL)。不传则端点返回 503。
func WithSystemConfig(s systemcfg.Service) Option {
	return func(o *options) { o.systemConfig = s }
}

// WithMFA 注入两步验证服务(internal/mfa),挂载:
//   - /api/auth/login/mfa、/api/auth/passkey/login/*、/api/auth/options(匿名);
//   - /api/account/mfa/*(认证 + CSRF + memberGate 白名单,管理员与成员各自管理);
//   - /api/users/{id}/mfa/*(管理员重置成员 TOTP/Passkey)。
//
// 并为登录流程提供 TOTP 启用检查(口令通过 → pending 会话 → 验证码升级)。
// 不传则两步验证端点 503、登录保持单步。
func WithMFA(s *mfa.Service) Option {
	return func(o *options) { o.mfa = s }
}

// WithObjectStores 注入「对象存储」服务,挂载 /api/object-stores* 路由
// (GET auth;写方法 auth + CSRF + 审计;test 为写方法)。AccessKeySecret 经 vault 密文,
// 响应/审计/日志绝无明文;AccessKeyId 非机密,可回显。不传则相关端点返回 503。
func WithObjectStores(s objectstore.Service) Option {
	return func(o *options) { o.objectStores = s }
}

// WithAppStore 注入应用商店模板服务,挂载 /api/ops/apps* 与 POST /api/servers/{id}/apps/deploy
// 路由(GET auth;写方法 auth + CSRF + 审计;部署复用 Stacks 受管链路)。
// 不传则相关端点返回 503(服务未初始化)。
func WithAppStore(s appstore.Service) Option {
	return func(o *options) { o.appStore = s }
}

// WithDBAdmin 注入「数据库管理」服务,挂载 /api/db-instances* 路由(admin 子组:
// auth + CSRF + 审计;MySQL/PostgreSQL 实例注册与 phpMyAdmin 式在线管理)。
// 连接密码经 vault 密文,响应/审计/日志绝无明文;实例默认只读,解锁为显式审计动作。
// 应用市场部署 mysql/postgres 模板成功后经此服务联动自动登记实例。不传则端点 503。
func WithDBAdmin(s dbadmin.Service) Option {
	return func(o *options) { o.dbAdmin = s }
}

// WithRedisAdmin 注入「Redis 管理」服务,挂载 /api/redis-instances* 路由(admin 子组:
// auth + CSRF + 审计;实例注册 + SCAN 游标分页键浏览 + 删键/改 TTL)。
// 连接密码经 vault 密文,响应/审计/日志绝无明文;实例默认只读,解锁为显式审计动作。
// 应用市场部署 redis 模板成功后经此服务联动自动登记实例。不传则端点 503。
func WithRedisAdmin(s redisadmin.Service) Option {
	return func(o *options) { o.redisAdmin = s }
}

// WithSecScan 注入「安全中心」服务(internal/secscan),挂载 /api/security/* 路由
// (admin 组:auth + CSRF + 审计;trivy-server 扫描器登记 + 单机/全量扫描 + 定时规则 +
// HTML 审计报告)。扫描令牌经 vault 密文;应用市场部署 trivy-server 模板成功后经此服务
// 联动自动登记。不传则安全中心端点不挂载(503 语义由前端提示)。
func WithSecScan(s secscan.Service) Option {
	return func(o *options) { o.secScan = s }
}

// WithAnomaly 注入可配置异常检测服务(Story 6.5;FR-23),挂载 /api/anomaly/* 路由
// (GET rules/alerts auth;POST rules/check + DELETE rules/{id} auth + CSRF)。检测复用 6-1
// 指标采集(metricsCollector 适配 collectServerMetrics);不可达/指标 null 的服务器跳过(不误报)。
// 不传则相关端点返回 503(服务未初始化)。
func WithAnomaly(s anomaly.Service) Option {
	return func(o *options) { o.anomaly = s }
}

// WithAnomalyConfig 注入后台定时检测的节奏参数(秒),经 GET /api/anomaly/config 暴露给前端显示
// 「每 N 秒自动检测一次」。intervalSec=0 表示定时检测已关闭(仅手动)。
func WithAnomalyConfig(intervalSec, cooldownSec int) Option {
	return func(o *options) {
		o.anomalyInterval = intervalSec
		o.anomalyCooldown = cooldownSec
	}
}

// WithMetricsHistory 注入服务器指标时序历史服务,挂载 GET /api/metrics/history(auth):
// 供异常检测页画 CPU/内存/磁盘随时间走势折线。不传则该端点返回 503。
func WithMetricsHistory(s metrics.Service) Option {
	return func(o *options) { o.metricsHistory = s }
}

// WithSsar 注入机器监控服务(internal/ssar),挂载 /api/ssar/* 路由:
// GET overview/query/history 过 auth(只读);POST download/install 过 auth + CSRF + 审计。
// 不传则相关端点返回 503(服务未初始化)。
func WithSsar(s ssar.Service) Option {
	return func(o *options) { o.ssar = s }
}

// WithOAuth 注入多 provider OAuth 凭据接入服务,挂载 /api/oauth/* 路由:
//   - GET  /api/oauth/apps(auth):列已配 OAuth 应用(secret 掩码)。
//   - PUT  /api/oauth/apps/{provider}(auth+CSRF):upsert 应用配置(clientSecret 只写)。
//   - GET  /api/oauth/{provider}/authorize(auth):生成 state → 302 跳 provider 授权页。
//   - GET  /api/oauth/{provider}/callback(auth;豁免 CSRF——GET 回跳,靠 state 校验):
//     验 state → Exchange → 把 access_token 经 vault.Create 存成 git_token 凭据 → 302 跳回。
//
// 复用已注入的 vault(o.vault)存 token 凭据。client_secret/access_token 经 vault 密文,
// 响应/错误/日志绝无明文。不传则相关端点返回 503。
func WithOAuth(s oauth.Service) Option {
	return func(o *options) { o.oauth = s }
}

// New 构建 HTTP 处理器:健康端点 + Auth API + (可选)凭据 API + 内嵌 SPA 静态托管。
// webFS 是已嵌入的前端构建产物(根为 web/dist)。
// authn 可选;为 nil 时认证中间件一律返回 401(仅便于测试静态服务)。
// 额外配置(如凭据保险库)经 Option 注入,见 WithVault。
func New(webFS fs.FS, authn auth.Authenticator, opts ...Option) http.Handler {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	// 更新检查器:进程级单例,内含 TTL 缓存(跨请求复用,避免反复点击打爆升级源限流)。
	// 升级源:系统配置的 release_mirror(设置→系统 可改,即时生效)优先,空则回落
	// env PIPEWRIGHT_RELEASE_MIRROR / GitHub 官方源;配置库不可用时优雅回退,不阻断检查。
	updateChecker := version.NewChecker()
	if sc := o.systemConfig; sc != nil {
		updateChecker.SetSourceProvider(func(ctx context.Context) version.Source {
			return version.ResolveSource(systemcfg.ResolveReleaseMirror(ctx, sc))
		})
	}
	// 自更新串行闸:同一时刻仅允许一个更新进行。
	updateInflight := newUpdateGate()
	// 源码部署(git clone + make build + install.sh):升级走 git 检查 + 本地重建管线;
	// 非源码部署为 nil,check/update 走升级源(GitHub / 镜像)。
	var srcUpdater *srcupdate.Service
	if dir := version.SourceDir(); dir != "" {
		srcUpdater = srcupdate.New(dir)
	}

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	// 安全响应头(CSP/防点击劫持/引用策略)对全部响应生效,须在任何写 body 的
	// handler 之前注册。
	r.Use(securityHeaders)
	// 解析请求 locale 并包装 writer,使 writeError 能按 UI 语言本地化错误消息
	// (包装器转发 Flush/Hijack/Unwrap,不影响 SSE 日志流与 WS 终端)。
	r.Use(localeMiddleware)
	// /api/* 访问日志(一行:method path status duration remote;>1s 标 SLOW)。
	// 排障第一证据链:出现在日志 = 请求已到达本进程;故障时缺失 = 卡在反代/隧道/浏览器层。
	r.Use(accessLog)

	r.Get("/healthz", handleHealthz)
	// /version 已移入受保护 /api 组(版本元数据会帮助攻击者匹配已知漏洞版本;
	// 前端仅在登录后的「设置→系统」页使用,运维探针用 /healthz)。

	svc := authn

	// Auth 端点:登录/登出/会话探测(见冻结契约)。
	r.Route("/api/auth", func(ar chi.Router) {
		ar.Post("/login", makeLoginHandler(svc))
		ar.Get("/session", makeSessionHandler(svc))
		// 普通用户注册(公开):邀请 token + 自填用户名/口令;成功后自动登录。
		ar.Post("/register", makeRegisterHandler(o.identity, svc))
		// 两步验证匿名端点:登录第二步(验证码)、Passkey discoverable 登录、
		// 登录页能力探测(passkey_login)。o.mfa 为 nil 时各端点 503。
		mountMFAAuth(ar, o.mfa, svc)
		// 邀请状态查询(公开只读):供注册页展示 pending/used/expired。
		ar.Get("/invitations/{token}", makeInvitationInfoHandler(o.identity))
		// 登出属写操作:先认证后 CSRF。
		ar.Post("/logout", func(w http.ResponseWriter, r *http.Request) {
			requireAuth(svc, requireCSRF(makeLogoutHandler(svc))).ServeHTTP(w, r)
		})
	})

	// Webhook 接收(公开入口,Story 3.2):豁免 requireAuth + CSRF,靠签名校验。
	// 必须在受保护 /api 组之外注册;限请求体大小在 handler 内(MaxBytesReader)。
	// 本地无此 token 时经集群转发给归属环境(webhookForwarder;mesh 未启用则纯本地)。
	r.Post("/api/webhooks/{token}", makeWebhookHandler(o.receiver, o.webhookForwarder))

	// 集群对等入站(公开入口=网络层 EasyTier 把关):豁免 session/CSRF,靠节点 ed25519
	// 签名头 + 接收方 ACL(默认全拒绝)鉴权。与 webhook 同级注册在受保护 /api 组之外。
	if o.mesh != nil {
		mountMeshInbound(r, o.mesh.meshDeps)
	}

	// 从通知直接审批(公开入口,SECURITY-SENSITIVE):豁免 requireAuth + CSRF——token(HMAC+过期+
	// 常量时间比较)即认证。GET 仅渲染确认页(无副作用,防 IM 预取自动放行);POST 才解析门。
	// signer 为 nil/禁用时页面提示不可用、不解析任何门。
	r.Get("/approvals", makeApprovalPageHandler(o.approvalSigner, o.approvalCoord))
	r.Post("/approvals/act", makeApprovalActHandler(o.approvalSigner, o.approvalCoord, o.audit))

	// 受保护 API 端点(除 /api/auth 与 /api/webhooks 外):requireAuth + 写方法 requireCSRF。
	r.Route("/api", func(ar chi.Router) {
		ar.Use(func(next http.Handler) http.Handler {
			return requireAuth(svc, next)
		})
		ar.Use(func(next http.Handler) http.Handler {
			return requireCSRF(next)
		})
		// 普通用户默认拒绝门:成员仅可访问白名单端点(项目/运行/日志/自身账户);
		// 管理员不受影响。必须在 requireAuth 之后(依赖 context 中的 Session)。
		ar.Use(func(next http.Handler) http.Handler {
			return memberGate(o.identity, o.runs, next)
		})
		mountOpsChat(ar, o.opsChat, svc)

		// 审计 Recorder(Story 1.4):传给既有敏感操作 handler,业务成功后追加审计行。
		// 为 nil 时 handler 跳过审计(不阻断业务)。
		aud := o.audit

		// 检查更新:鉴权只读,查升级源最新发布(源码部署改查 git 上游)并与当前版本比对(GET 免 CSRF)。
		ar.Get("/version", handleVersion)
		ar.Get("/version/check", makeCheckUpdateHandler(updateChecker, srcUpdater))
		// 一键自动更新:鉴权 + CSRF(写操作);source 后台重建管线,binary 自替换+重启,docker 返回升级命令。
		ar.Post("/version/update", makeSelfUpdateHandler(updateChecker, srcUpdater, updateInflight, o.reexecGuard))
		// 升级进度轮询(鉴权只读):source 后台管线步骤/日志/完成态;binary 更新进行中报告 running,
		// 供页面刷新后恢复升级进度区。
		ar.Get("/version/update/status", makeUpdateStatusHandler(srcUpdater, updateInflight))

		// 审计查询(Story 1.4):只读 + 认证保护 + 分页 + 过滤。
		ar.Get("/audit", makeListAuditHandler(aud))

		// 凭据保险库(Story 1.3)。v 为 nil 时 handler 返回 vault_unconfigured。
		v := o.vault
		ar.Get("/credentials", makeListCredentialsHandler(v))
		ar.Post("/credentials", makeCreateCredentialHandler(v, aud))
		// 生成 SSH 密钥对(可选算法):无状态加密运算,不落库/不留审计;私钥仅在响应中一次性返回。
		ar.Post("/credentials/ssh-keypair", makeGenerateSSHKeyPairHandler())
		ar.Patch("/credentials/{id}", makeUpdateCredentialHandler(v, aud))
		ar.Delete("/credentials/{id}", makeDeleteCredentialHandler(v, aud))
		// 查看明文(POST + auth + CSRF;每次留 credential_reveal 审计)。
		ar.Post("/credentials/{id}/reveal", makeRevealCredentialHandler(v, aud))

		// 项目接入与列表(Story 2.1)。p 为 nil 时 handler 返回 503。
		// test-clone 须在 {id} 路由之前注册,否则被 /projects/{id} 吞掉。
		// 列表/更新响应附带 serviceRegs(卡片展示的服务注册地址):由 o.pipelines(流水线绑定引用)
		// 与 o.serviceReg(注册服务清单)实时解析;任一未装配时该字段输出 []。
		// o.runs 回填卡片「上次运行」状态(最近一次运行;未装配时输出 null)。
		p := o.projects
		ar.Get("/projects", makeListProjectsHandler(p, o.identity, o.pipelines, o.serviceReg, o.runs))
		ar.Post("/projects", makeCreateProjectHandler(p, aud))
		ar.Post("/projects/test-clone", makeTestCloneHandler(p))
		ar.Patch("/projects/{id}", makeUpdateProjectHandler(p, aud, o.pipelines, o.serviceReg, o.runs))
		ar.Delete("/projects/{id}", makeDeleteProjectHandler(p, aud, o.projectMirrorCleaner))

		// 用户体系(管理员):普通用户列表、邀请生成/列表/撤销、项目成员增删查。
		// 普通用户由 memberGate 默认拒绝;此组再叠加 requireAdmin 做纵深防御。
		ar.Group(func(ar chi.Router) {
			ar.Use(requireAdmin)
			ar.Get("/users", makeListUsersHandler(o.identity, o.mfa))
			// 成员两步验证重置(锁死恢复通道):清除 TOTP/全部通行密钥。
			mountAdminMFA(ar, o.mfa, aud)
			ar.Post("/invitations", makeCreateInvitationHandler(o.identity, aud))
			ar.Get("/invitations", makeListInvitationsHandler(o.identity))
			ar.Delete("/invitations/{id}", makeRevokeInvitationHandler(o.identity, aud))
			ar.Get("/projects/{id}/members", makeListProjectMembersHandler(o.identity))
			ar.Post("/projects/{id}/members", makeAddProjectMemberHandler(o.identity, aud))
			ar.Delete("/projects/{id}/members/{userId}", makeRemoveProjectMemberHandler(o.identity, aud))
			// 集群管理面(管理员):本节点配置/授权单/节点注册表/集群页聚合 + 对端代理查询。
			if o.mesh != nil {
				mountMeshAdmin(ar, *o.mesh)
				// 集群级共享构建机管理面(管理员):跨环境任务列表/强制取消(运行时缺席不挂)。
				mountSharedBuildAdmin(ar, o.mesh.meshDeps)
			}
			// 数据库管理面(管理员):MySQL/PostgreSQL 实例注册 + phpMyAdmin 式在线管理。
			if o.dbAdmin != nil {
				mountDBAdmin(ar, o.dbAdmin, aud)
			}
			// Redis 管理面(管理员):实例注册 + SCAN 分页键浏览 + 删键/改 TTL。
			if o.redisAdmin != nil {
				mountRedisAdmin(ar, o.redisAdmin, aud)
			}
			// 安全中心(管理员):trivy-server 扫描器登记 + Trivy/Nuclei 扫描 +
			// 定时规则 + HTML 审计报告 + 漏洞 AI 解读(窄接口断言注入,未配置 → 端点 503)。
			if o.secScan != nil {
				var secExplainer ai.FindingExplainer
				if o.aiSettings != nil {
					if e, ok := o.aiSettings.(ai.FindingExplainer); ok {
						secExplainer = e
					}
				}
				mountSecurity(ar, o.secScan, aud, secExplainer)
			}
			// Ceph 管理面(管理员):集群登记/cephadm 部署管线/状态 + 主机/OSD/池/RBD/CephFS 管理。
			if o.pveAdmin != nil {
				mountPVEAdmin(ar, o.pveAdmin, aud)
			}
			if o.ceph != nil {
				mountCeph(ar, o.ceph, aud)
			}
			// 磁盘管理面(管理员):块设备/LVM 拓扑 + 分区/格式化/挂载/扩缩容/LVM 变更
			// (只读默认 + 显式解锁 + 危险操作逐字确认)。
			if o.diskAdmin != nil {
				mountDiskAdmin(ar, o.diskAdmin, aud)
			}
			// 文件查看面(管理员):目录浏览 + 文件下载 + ≤64KiB 纯文本在线查看/编辑
			// (编辑前同目录备份;PUT 留审计)。
			if o.fileAdmin != nil {
				mountFileAdmin(ar, o.fileAdmin, aud)
			}
			// 产物管理面(管理员):跨运行产物列表 + 删除(镜像类联动删内置 registry tag、
			// 无引用 blob 回收)+ 镜像 tar 导出下载。文件类按运行查看/下载在受保护组(成员可用)。
			if o.runs != nil {
				ar.Get("/artifacts", makeArtifactsListHandler(o.runs))
				ar.Delete("/runs/{id}/artifacts/{artifactId}", makeArtifactDeleteHandler(o.runs, o.artifactStore, o.registryHub, aud))
				ar.Get("/runs/{id}/artifacts/{artifactId}/image-archive", makeArtifactImageArchiveHandler(o.runs, o.artifactExporter))
			}
			// 产物跨环境传输(管理员):发起(异步任务)/状态轮询/取消;发送核心与
			// mesh_push 流水线节点同路径,对端经 /api/mesh/push 接收(接收侧零改动)。
			if o.artifactTransfers != nil {
				ar.Post("/runs/{id}/artifacts/transfer", makeArtifactTransferStartHandler(o.artifactTransfers))
				ar.Get("/artifacts/transfers/{transferId}", makeArtifactTransferStatusHandler(o.artifactTransfers))
				ar.Delete("/artifacts/transfers/{transferId}", makeArtifactTransferCancelHandler(o.artifactTransfers))
			}
		})

		// 触发设置与分支映射(Story 2.3)。t 为 nil 时 handler 返回 503。
		// secret/reset 须在通用 trigger 路由之外单独注册;均过 auth + 写方法 CSRF。
		t := o.triggers
		ar.Get("/projects/{id}/trigger", makeGetTriggerHandler(t))
		ar.Put("/projects/{id}/trigger", makeSaveTriggerHandler(t))
		ar.Post("/projects/{id}/trigger/secret/reset", makeResetTriggerSecretHandler(t, aud))
		// webhook 接收端点可选环境地址(集群转发;只读,项目成员可用):mesh 启用时返回
		// 已配置对外地址的对端环境,供 UI 把端点切到其他环境的地址复制。
		ar.Get("/projects/{id}/trigger/webhook-endpoints", makeWebhookEndpointsHandler(o.meshServices()))

		// 定时(cron)触发配置(Story 8-6)。o.cron 为 nil 时 handler 返回 503。GET 过 auth;PUT 过 auth + CSRF。
		ar.Get("/projects/{id}/cron", makeGetCronHandler(o.cron))
		ar.Put("/projects/{id}/cron", makeSaveCronHandler(o.cron))

		// 项目级并发上限(FR-8-10)。o.concurrency 为 nil 时 handler 返回 503。GET 过 auth;PUT 过 auth + CSRF。
		ar.Get("/projects/{id}/concurrency", makeGetConcurrencyHandler(o.concurrency))
		ar.Put("/projects/{id}/concurrency", makeSaveConcurrencyHandler(o.concurrency))

		// 项目级类型化运行参数定义(P0 typed params)。o.parameters 为 nil → handler 返回 503。GET 过 auth;PUT 过 auth + CSRF。
		ar.Get("/projects/{id}/parameters", makeGetParametersHandler(o.parameters))
		ar.Put("/projects/{id}/parameters", makeSaveParametersHandler(o.parameters))

		// 流水线串联(FR-8-11):上游成功后触发下游流水线。o.chain 为 nil → handler 返回 503。
		// GET 过 auth;PUT 为写方法,过 auth + CSRF。
		ar.Get("/projects/{id}/chain", makeGetChainHandler(o.chain))
		ar.Put("/projects/{id}/chain", makeSaveChainHandler(o.chain))

		// 流水线编排配置(Story 2.2)。pl 为 nil 时 handler 返回 503。
		// 与 /projects/{id}/trigger 同在 {id} 路由组内并存(不会被 /projects/{id} 吞)。
		// GET 过 auth;PUT 为写方法,过 auth + CSRF。
		pl := o.pipelines
		ar.Get("/projects/{id}/pipeline", makeGetPipelineHandler(pl))
		ar.Put("/projects/{id}/pipeline", makeSavePipelineHandler(pl))

		// 流水线即代码导入/预览(FR-8-12):POST 收 .pipewright.yml 文档,解析+校验为同一套 spec。
		// /pipeline/import 比 /pipeline 多一段,不会被吞;写方法,过 auth + CSRF。
		ar.Post("/projects/{id}/pipeline/import", makeImportPipelineHandler(pl))

		// 构建/部署配置(Story 2.4):独立表/端点,与 /pipeline(2-2 spec)各自路由;
		// /pipeline/settings 比 /pipeline 多一段,不会被吞。GET 过 auth;PUT 写方法过 auth + CSRF。
		ps := o.pipelineSettings
		ar.Get("/projects/{id}/pipeline/settings", makeGetPipelineSettingsHandler(ps))
		ar.Put("/projects/{id}/pipeline/settings", makeSavePipelineSettingsHandler(ps))

		// 流水线配置合法性校验(Story 2.6):只读聚合视图 + 认证。
		// 聚合 spec(2-2)+ settings(2-4)+ trigger(2-3)+ 项目凭据,跑跨 tab 校验。
		// 复用已注入的 o.pipelines/o.pipelineSettings/o.triggers/o.projects/o.vault,无需新 Option。
		// /pipeline/validation 比 /pipeline 多一段,不会被吞;GET 过 auth。
		ar.Get("/projects/{id}/pipeline/validation",
			makeGetPipelineValidationHandler(pl, ps, t, p, v))

		// 运行模型 + 列表 + 详情 + 取消 + SSE 状态流(Story 3.1)。rs 为 nil 时返回 503。
		// SSE events 为 GET,豁免 CSRF;cancel 为写方法,过 CSRF。均过 requireAuth。
		rs := o.runs
		ar.Get("/runs", makeListRunsHandler(rs, o.identity))
		ar.Get("/runs/{id}", makeGetRunHandler(rs))
		ar.Get("/runs/{id}/events", makeRunEventsHandler(rs, o.runSub))
		// 历史日志拉取 / 分页(Story 3.6):只读 + 认证;sinceSeq 分页;complete=终态。
		ar.Get("/runs/{id}/logs", makeRunLogsHandler(rs))
		// 构建产物契约(Story 3.4 / FR-6):只读 + 认证;按 (type, reference) 供 Epic 4 消费。
		ar.Get("/runs/{id}/artifacts", makeRunArtifactsHandler(rs))
		// 产物下载(Story 8-16 续):按 reference 句柄从制品库取真字节流下载。o.artifactStore 为 nil → 503。
		ar.Get("/runs/{id}/artifacts/{artifactId}/download", makeArtifactDownloadHandler(rs, o.artifactStore))
		// 测试报告 + 质量门禁(Story 8-6 / FR-8-6):只读 + 认证;通过/失败/跳过 + 覆盖率 + 门禁裁决。
		ar.Get("/runs/{id}/test-report", makeRunTestReportHandler(rs))
		ar.Post("/runs/{id}/cancel", makeCancelRunHandler(rs))
		// 失败运行按节点恢复(FR:失败重试/跳过):校验配置一致性 → 创建派生运行(继承成功
		// 节点、重试/跳过失败节点、部署节点只重试失败机器)。写方法,过 auth + CSRF;
		// specLoader 为 nil(legacy runner 模式)→ 503。返回 201 + 子运行详情。
		ar.Post("/runs/{id}/resume", makeResumeRunHandler(rs, o.specLoader, o.approvalStore, aud))
		// 人工审批门(Story 8-4):批准/拒绝某运行的审批门阶段 + 列审批记录。
		// approve/reject 为写方法,过 auth + CSRF;coord/store 为 nil → 503。
		ar.Post("/runs/{id}/approve", makeApprovalDecisionHandler(o.approvalCoord, aud, true))
		ar.Post("/runs/{id}/reject", makeApprovalDecisionHandler(o.approvalCoord, aud, false))
		ar.Get("/runs/{id}/approvals", makeListApprovalsHandler(o.approvalStore))
		// 等待节点(wait)运行中查询 + 手动跳过:GET 为只读;skip-wait 为写方法,过 auth + CSRF。
		// coord 为 nil → 503(等待节点照常执行,只是不可查询/跳过)。
		ar.Get("/runs/{id}/waits", makeListRunWaitsHandler(o.waitCoord))
		ar.Post("/runs/{id}/skip-wait", makeSkipWaitHandler(o.waitCoord, aud))
		// SSH 部署执行(Story 4.2 / FR-10):认证 + CSRF(写方法)。dep 为 nil → 503。
		// 取 run + 产物 + 服务器 → deploy.Deploy(逐机经 SSH 执行部署命令,array 不拼 shell)
		// → 据结果更新 run 终态 → 返回填好的 targets。部署执行失败不 500(每机 status=failed)。
		ar.Post("/runs/{id}/deploy", makeDeployRunHandler(o.deployer, rs))
		// 仅重试失败目标(Story 4.5 / FR-13):认证 + CSRF(写方法)。dep 为 nil → 503。
		// 取 run 当前 failed/rolled_back 目标 → 复用产物 + 配置并行重跑 → 逐目标 upsert(成功机不动)
		// → 重算 run 终态 → 返回全量最新 targets。run 非失败 / 无失败目标 → 422;不存在 → 404。
		// 重试执行失败不 500(被重试目标 status=failed)。比 /deploy 多一段,不会被吞。
		ar.Post("/runs/{id}/deploy/retry", makeRetryDeployHandler(o.deployer, rs))
		// (原 /deploy/continue 与 /deploy/abort 已随交互式分批策略移除:统一滚动分批自动推进。)

		// AI 失败诊断(Story 7.2):显式(重)诊断。认证 + CSRF(写方法)。
		// 取 run 失败日志 → 脱敏 → ai.Diagnose → 持久化 → 返回 diagnosis 子 DTO。
		// 非失败 → 422 run_not_failed;不存在 → 404;任何 LLM 失败 → 200 + status=unavailable
		// (绝不 500、绝无密钥)。复用已注入 rs + aiSettings(o.aiSettings),无新顶层依赖。
		ar.Post("/runs/{id}/diagnose", makeDiagnoseRunHandler(rs, o.aiSettings, o.secretSource))

		// 成功/失败差异对比(Story 7.3;FR-25):只读 + 认证。取本次 run(commit)+ LastSuccessfulRun
		// (同项目+同分支+更早+最近成功)+ 项目 repoURL/凭据 → RunDiffer(go-git 克隆 + tree diff)→ DTO。
		// 无 baseline / 无 commit / 克隆失败 / commit 不可达 → 200 available:false degraded(绝不 500);
		// run 不存在 → 404。复用已注入 rs + p + v + 注入的 RunDiffer。differ 为 nil → 503。
		ar.Get("/runs/{id}/diff", makeRunDiffHandler(runDiffDeps{
			runs:     rs,
			projects: p,
			vault:    v,
			differ:   o.runDiffer,
		}))

		// 诊断反馈闭环(Story 7.5;FR-26):对 ready 诊断 👍/👎(👎 可附正确根因)。认证 + CSRF(写方法)。
		// run 不存在 → 404;无诊断 → 422 no_diagnosis;同 run 重复提交 = upsert 覆盖最新。
		// correctRootCause 脱敏(过 mask)+ 长度上限。复用已注入 rs + o.feedback(WithDiagnosisFeedback)。
		// fb 为 nil → 503。GET diagnosis-stats 在 /settings 组内(只读 + 认证)。
		ar.Post("/runs/{id}/diagnosis/feedback", makeSubmitDiagnosisFeedbackHandler(rs, o.feedback, o.secretSource))

		// 手动触发创建运行(Story 3.2):认证 + CSRF;actor=admin;返 201 run-detail DTO。
		// o.parameters 非 nil 时,据项目参数定义校验 + 强转 + 填默认(typed params,P0);无定义则透传。
		ar.Post("/projects/{id}/runs", makeManualRunHandler(rs, o.parameters, aud))

		// 被推送源项目的修订查询(集群推送接收侧):运行对话框的修订/条目选择器用。
		// 比手动触发多一段,不会被吞;集群未装配时不挂载(前端降级为「最新修订/全部条目」)。
		if o.mesh != nil {
			mountPushedSource(ar, o.mesh.meshDeps)
		}

		// 账户设置(Story 1.7):改口令 / 会话列表 / 撤销会话。ac 为 nil 时 handler 返回 503。
		// 改口令、撤销会话为写方法,过 auth + CSRF;列表为 GET,过 auth。
		// 改口令 / 撤销会话为敏感操作,业务成功后接 audit(detail 绝无明文口令)。
		ac := o.account
		ar.Post("/account/password", makeChangePasswordHandler(ac, aud))
		ar.Get("/account/sessions", makeListSessionsHandler(ac))
		ar.Delete("/account/sessions/{id}", makeRevokeSessionHandler(ac, aud))

		// 两步验证(本人管理):TOTP 开启/关闭/备份码 + Passkey 注册/删除。
		// 已过组级 auth + CSRF;memberGate 白名单放行成员管理自己的 MFA。
		mountAccountMFA(ar, o.mfa, aud)

		// 可配置 AI 提供商(Story 7.1)。aiSvc 为 nil 时 handler 返回 503。
		// GET 过 auth;PUT/test 为写方法,过 auth + CSRF。apiKey 加密入库、响应仅掩码。
		aiSvc := o.aiSettings
		ar.Get("/settings/ai", makeGetAISettingsHandler(aiSvc))
		ar.Put("/settings/ai", makeSaveAISettingsHandler(aiSvc))
		ar.Post("/settings/ai/test", makeTestAISettingsHandler(aiSvc))
		ar.Post("/settings/ai/web/test", makeWebTestAISettingsHandler(aiSvc))
		ar.Post("/settings/ai/models", makeAIModelsHandler(aiSvc))

		// 内置本地 registry(registryhub)。rh 为 nil 时各端点 503。GET 过 auth;
		// PUT(只写库,不触碰机器)与 deploy/prune/daemon-apply(显式触碰机器)过 auth + CSRF;
		// daemon-apply **只处理请求里显式勾选的目标**;机器层失败一律 200 + ok:false,不 500。
		rh := o.registryHub
		ar.Get("/settings/registry", makeGetRegistryHubHandler(rh))
		ar.Put("/settings/registry", makeSaveRegistryHubHandler(rh))
		ar.Post("/settings/registry/deploy", makeDeployRegistryHandler(rh, aud))
		ar.Post("/settings/registry/upgrade-image", makeUpgradeRegistryImageHandler(rh, aud))
		ar.Get("/settings/registry/status", makeRegistryStatusHandler(rh))
		ar.Post("/settings/registry/prune", makeRegistryPruneHandler(rh, aud))
		ar.Post("/registry/daemon-apply", makeDaemonApplyHandler(rh, aud))
		ar.Get("/registry/inspect", makeDaemonInspectHandler(rh))

		// 诊断反馈闭环统计(Story 7.5;FR-26):准确率/计数/最近趋势/最近 corrections(知识库种子可视化)。
		// 只读 + 认证;无反馈 → 全 0 / 空数组 / accuracy:null(不报错)。fb 为 nil → 503。
		ar.Get("/settings/diagnosis-stats", makeDiagnosisStatsHandler(o.feedback))

		// AI 生成流水线配置(Story 2.5):浅克隆分析 + LLM 生成提案 + 全/部分接受应用。
		// generate 复用 aiSvc + projects(p)+ vault(v);apply 复用 pipelines(pl)+ settings(ps)+ triggers(t)。
		// /pipeline/ai-generate、/pipeline/ai-apply 比 /pipeline 多一段,不会被吞;均为写方法,过 auth + CSRF。
		// 两路优雅降级(AI 未配 / clone 失败 / LLM 失败)均 HTTP 200,绝不 500、绝无明文密钥。
		aiGenDeps := aiGenerateDeps{
			analyzer:    o.aiAnalyzer,
			aiSvc:       aiSvc,
			projects:    p,
			pipes:       pl,
			settings:    ps,
			vault:       v,
			customNodes: o.customNodes,
		}
		ar.Post("/projects/{id}/pipeline/ai-generate", makeAIGenerateHandler(aiGenDeps))
		ar.Post("/projects/{id}/pipeline/ai-apply", makeAIApplyHandler(aiGenDeps))

		// AI 脚本风险标注(护城河 · AI moat):对流水线脚本步骤(Epic 8 script step)做风险标注 —
		// 确定性正则预扫(rm -rf /、curl|sh、chmod 777、明文密钥、latest 镜像)+ 可选 LLM 语义增强。
		// 复用 aiSvc + projects(p)+ settings(ps)+ secretSource(出网前脱敏)。写方法,过 auth + CSRF。
		// AI 未配/失败均 200 + 确定性结果 + aiEnhanced=false,绝不 500、绝无明文密钥。
		aiRisk := aiRiskDeps{aiSvc: aiSvc, projects: p, settings: ps, secretSource: o.secretSource}
		ar.Post("/projects/{id}/pipeline/analyze-risks", makeAnalyzeRisksHandler(aiRisk))

		// AI 运维终端助手(运维终端 P1 · 护城河):中文 → 命令卡 / 命令解释。复用 aiSvc。
		// 写方法,过 auth + CSRF。未配 AI → 200 + available=false(前端提示去设置配);命令最终
		// risk 由领域层确定性复核(rm -rf/mkfs/dd/关机等无条件 danger),前端据此拦截直接执行。
		ar.Post("/ai/command", makeAICommandHandler(aiSvc))
		ar.Post("/ai/explain", makeAIExplainHandler(aiSvc))
		ar.Post("/ai/complete", makeAICompleteHandler(aiSvc)) // P2 智能补全(前缀→完整命令)
		ar.Post("/ai/compose", makeAIComposeHandler(aiSvc))   // 中文 → docker-compose.yml

		// 只读源码读取(Story 3.6;FR-4 预埋,7-4 消费):go-git 浅克隆读 tree/blob。
		// 复用已注入 projects(p)+ vault(v)取凭据 + 注入的 SourceReader。reader 为 nil → 503。
		// /source/tree、/source/blob 比 /projects/{id} 多两段,不会被吞;GET 过 auth。
		srcDeps := sourceDeps{projects: p, vault: v, reader: o.sourceReader}
		ar.Get("/projects/{id}/source/tree", makeSourceTreeHandler(srcDeps))
		ar.Get("/projects/{id}/source/blob", makeSourceBlobHandler(srcDeps))

		// 「流水线即代码」预览/校验(GitOps Slice 3):按 ref 拉取仓库 .pipewright.yml 并校验,
		// 让用户在依赖它驱动运行之前看清运行时会用到的配置摘要、显式捕获 YAML 错误(运行时静默回退)。
		// 复用 srcDeps(projects + vault + SourceReader);/pac/preview 比 /projects/{id} 多两段,不会被吞;GET 过 auth。
		ar.Get("/projects/{id}/pac/preview", makePacPreviewHandler(srcDeps))

		// 列仓库分支/tag(代码管理区 · Story 8-18):供前端触发时分支/commit 下拉。o.refsLister 为 nil → 503。
		ar.Get("/projects/{id}/refs", makeListRefsHandler(p, v, o.refsLister))
		ar.Get("/projects/{id}/commits", makeListCommitsHandler(p, v, o.refsLister))
		ar.Get("/projects/{id}/runner", makeGetRunnerHandler(o.runnerConfig))
		ar.Put("/projects/{id}/runner", makeSaveRunnerHandler(o.runnerConfig, aud))

		// 目标服务器登记 + 通用 SSH 执行层(Story 4.1;internal/target,FR-14)。
		// sv 为 nil 时 handler 返回 503。GET/List/Get 过 auth;create/update/delete/test 为写方法,
		// 过 auth + CSRF。/servers/{id}/test 比 /servers/{id} 多一段,不会被吞。
		// SSH 私钥/口令经 vault 即用即弃,响应/错误绝无明文(AC-SEC-01/02)。
		sv := o.servers
		ar.Get("/servers", makeListServersHandler(sv))
		ar.Post("/servers", makeCreateServerHandler(sv, o.pveAdmin))
		// 服务器池(0084,系统管理:default + PVE 探测自动建):只读列表,成员可见,
		// 供服务器列表分组展示与目标选择器过滤 pve_node。
		ar.Get("/server-pools", makeServerPoolListHandler(sv))
		ar.Get("/servers/{id}", makeGetServerHandler(sv))
		ar.Put("/servers/{id}", makeUpdateServerHandler(sv))
		ar.Delete("/servers/{id}", makeDeleteServerHandler(sv))
		ar.Post("/servers/{id}/test", makeTestServerHandler(sv))

		// 容器域(容器/镜像/栈/卷/网络/日志/交互终端/服务操作)额外支持「控制节点本机」:
		// csv 对 serverID == target.LocalHostID("__local__")改为在**本机执行**(os/exec,不经 SSH),
		// 对真实 serverID 逐字透传(零行为变化)。命令仍是 []string array、不经 shell(AC-SEC-02);
		// 本机无 SSH 凭据参与,故无凭据泄漏面。**登记类端点(CRUD/test)刻意不套 csv** —— 本机不是
		// 可登记实体(Update/Delete 返回 target.ErrLocalHostImmutable),它也不进 List(批量端点不含它)。
		csv := target.WithLocalHost(sv)

		// 机器标签登记处(标签字典,可先建后挂):GET 过 auth;POST/DELETE 过 auth + CSRF。
		// 字面段 /labels 与 /servers/{id} 互不干扰。
		ar.Get("/labels", makeListLabelsHandler(o.labels))
		ar.Post("/labels", makeCreateLabelHandler(o.labels))
		ar.Delete("/labels/{name}", makeDeleteLabelHandler(o.labels))
		// 服务日志查看(Story 6.2;FR-16,经 SSH 取目标服务器日志)。复用 sv(4-1 装配,无新服务)。
		// 均为 GET 只读 → 过 auth、豁免 CSRF。source/target 严格白名单校验(AC-SEC-02);
		// SSH/命令失败人读不 500;实时 /logs/stream 为 SSE,客户端断开即关 SSH session。
		// 比 /servers/{id} 多一段,不会被吞。
		ar.Get("/servers/{id}/logs", makeServerLogsHandler(csv))
		ar.Get("/servers/{id}/logs/stream", makeServerLogsStreamHandler(csv))
		// 多机状态总览 —— 服务器层指标(Story 6.1;FR-15,经 SSH 跑固定白名单只读命令采集
		// CPU/内存/磁盘)。复用 sv(4-1 装配,无新服务)。均为 GET 只读 → 过 auth、豁免 CSRF。
		// 采集命令纯静态 array、不接受任何用户输入(AC-SEC-02);某台不可达/某指标缺失 →
		// reachable:false / 该指标 null,不 500。批量端点逐台并行有界、各自独立。
		// chi 字面段优先于 {id},/servers/metrics 不会被 /servers/{id} 吞。
		ar.Get("/servers/metrics", makeAllServerMetricsHandler(sv))
		ar.Get("/servers/{id}/metrics", makeServerMetricsHandler(sv))
		// 容器管理列表/聚合(Portainer 式总览)——经 SSH 跑 `docker ps -a --format {{json .}}`
		// 采集容器清单。复用 sv(4-1 装配,无新服务)。GET 只读 → 过 auth、豁免 CSRF。命令纯静态
		// array、不接受任何用户输入(AC-SEC-02);某台不可达/无运行时 → reachable:false / runtime:""
		// ,不 500。批量端点逐台并行有界、各自独立。容器**生命周期**写操作复用上面的 /service/action
		// (type=docker)。chi 字面段 /servers/containers 优先于 {id},不会被吞。
		ar.Get("/servers/containers", makeAllServerContainersHandler(sv))
		ar.Get("/servers/{id}/containers", makeServerContainersHandler(csv))
		// PM2 管理 —— 进程清单/聚合(server_pm2.go,`pm2 jlist` over SSH,零侵入)。
		// 复用 sv/csv,无新服务。GET 只读 → 过 auth、豁免 CSRF;命令纯静态 array
		// (AC-SEC-02);某台不可达/未装 pm2 → reachable:false / runtime:"",不 500。
		// 进程**生命周期**写操作复用上面的 /service/action(type=pm2)。chi 字面段
		// /servers/pm2 优先于 {id},不会被吞;批量用 sv(不含本机),本机由前端单独拉
		// getServerPm2(__local__)(csv 单台端点支持)。
		ar.Get("/servers/pm2", makeAllServerPM2Handler(sv))
		ar.Get("/servers/{id}/pm2", makeServerPM2Handler(csv))
		// 容器实时资源 stats(docker stats --no-stream)。字面段 stats 不与 {id}/containers 冲突。
		ar.Get("/servers/{id}/containers/stats", makeServerContainerStatsHandler(csv))
		// 容器详情 inspect(docker inspect → 精选字段)。
		ar.Get("/servers/{id}/containers/{containerId}/inspect", makeContainerInspectHandler(csv))
		// 一键清理:磁盘占用(docker system df)+ prune(写,过 CSRF + 审计)。
		ar.Get("/servers/{id}/system/df", makeSystemDfHandler(csv))
		ar.Post("/servers/{id}/system/prune", makeSystemPruneHandler(csv, aud))
		// 新增容器(docker run) —— 写操作,过 auth + CSRF。参数严格白名单 + array 化,
		// 非法 400 invalid_create_spec;SSH/命令失败人读不 500;成功后写审计(detail 仅 image/name)。
		ar.Post("/servers/{id}/containers", makeCreateContainerHandler(csv, aud))
		// 镜像管理 —— 列表(只读)+ 拉取/删除(写,过 CSRF + 审计)。镜像引用严格白名单 + array 化。
		ar.Get("/servers/{id}/images", makeServerImagesHandler(csv))
		ar.Post("/servers/{id}/images/pull", makeImagePullHandler(csv, aud))
		ar.Post("/servers/{id}/images/remove", makeImageRemoveHandler(csv, aud))
		// Compose/Stacks —— 列表(只读)+ 部署(贴 yaml → up)+ start/stop/restart/down。
		// 项目名严格白名单 + array 化;compose 内容经 Upload 落文件(非 shell 注入面)。写过 CSRF + 审计。
		ar.Get("/servers/{id}/stacks", makeServerStacksHandler(csv))
		ar.Get("/servers/{id}/stacks/{name}", makeServerStackDetailHandler(csv))
		ar.Post("/servers/{id}/stacks/deploy", makeStackDeployHandler(csv, aud))
		ar.Post("/servers/{id}/stacks/action", makeStackActionHandler(csv, aud))
		ar.Post("/servers/{id}/stacks/save", makeStackSaveHandler(csv, aud))
		// 应用商店一键部署(DPanel 式):模板参数渲染 → 复用 Stacks 链路 up -d。
		// 生成的 secret 仅部署响应一次性返回;审计只记模板名,不记参数值。
		// RustFS 模板部署成功后联动自动登记对象存储;mysql/postgres 模板联动自动登记
		// 数据库实例(均尽力而为,不影响部署结果)。
		ar.Post("/servers/{id}/apps/deploy", makeDeployAppHandler(o.appStore, csv, aud, o.objectStores, o.dbAdmin, o.redisAdmin, o.secScan))
		// 应用市场升级:读参数边车原位保留参数(空值键 = 保持不变,secret 不重新生成)→
		// 重渲染 → 复用 Stacks 链路 up -d;upgrade-context 供升级表单预填(secret 只回有值标记)。
		ar.Get("/servers/{id}/apps/upgrade-context", makeAppUpgradeContextHandler(o.appStore, csv))
		ar.Post("/servers/{id}/apps/upgrade", makeUpgradeAppHandler(o.appStore, csv, aud))
		// 应用市场卸载:按项目标签删容器(可选连删具名卷)→ 清受管目录(实例从列表消失)。
		ar.Post("/servers/{id}/apps/uninstall", makeAppUninstallHandler(csv, aud))
		// 已部署实例聚合枚举(「升级」直达入口的数据底座):全部机器上带参数边车的栈。
		// GET 只读 → 过 auth、豁免 CSRF;find 命令纯静态 array(AC-SEC-02);某台不可达仅该台
		// reachable:false,不 500。chi 字面段 /servers/apps 优先于 {id},不会被吞。
		ar.Get("/servers/apps/instances", makeAllAppInstancesHandler(o.appStore, csv))
		// 容器 AI 诊断 / 看日志(AI moat):取容器日志 → ai.Diagnose 出根因+修复。复用 csv +
		// o.aiSettings。出网前脱敏;AI 未配/失败 → 200 unavailable,绝不 500。比 {id} 多两段不被吞。
		ar.Post("/servers/{id}/containers/{containerId}/diagnose", makeContainerDiagnoseHandler(csv, o.aiSettings))
		// 数据卷 + 网络 —— 列表(只读)+ 创建/删除(写,过 CSRF + 审计)。名称白名单 + array 化。
		ar.Get("/servers/{id}/volumes", makeServerVolumesHandler(csv))
		ar.Post("/servers/{id}/volumes/create", makeVolumeCreateHandler(csv, aud))
		ar.Post("/servers/{id}/volumes/remove", makeVolumeRemoveHandler(csv, aud))
		ar.Get("/servers/{id}/networks", makeServerNetworksHandler(csv))
		ar.Post("/servers/{id}/networks/create", makeNetworkCreateHandler(csv, aud))
		ar.Post("/servers/{id}/networks/remove", makeNetworkRemoveHandler(csv, aud))
		// 网络详情:某网络上连着哪些容器 + 连接/断开。network 段白名单 + array 化。
		ar.Get("/servers/{id}/networks/{network}/containers", makeNetworkContainersHandler(csv))
		ar.Post("/servers/{id}/networks/{network}/connect", makeNetworkConnectHandler(csv, aud))
		ar.Post("/servers/{id}/networks/{network}/disconnect", makeNetworkDisconnectHandler(csv, aud))
		// 服务操作 —— 重启/停止/启动(Story 6.3;FR-17,经 SSH 跑 systemctl/docker)。
		// 复用 sv(4-1 装配)+ aud(1-4 装配),无需新服务。写操作 → 过 auth + CSRF。
		// type/target/action 严格白名单(AC-SEC-02:首字符非 `-` 防 flag 注入、无 shell 元字符
		// 防命令注入、action 枚举);命令 array 化经 target.Exec 不拼 shell。非法 → 400
		// invalid_service_target;SSH/命令失败人读不 500;成功投递后写审计(detail 脱敏)。
		// 比 /servers/{id} 多两段,不会被吞。
		ar.Post("/servers/{id}/service/action", makeServiceActionHandler(csv, aud))
		// 容器内交互终端(Story 6.4;FR-18,WS ↔ SSH → docker exec -it)。复用 sv(4-1 装配)
		// + aud(1-4 装配),无需新服务。**平台唯一 WS 升级点**(其余实时流走 SSE)。GET 升级
		// → 过 auth(未登录 401 不升级);CSRF 对 GET 豁免,改以同源(Origin)校验防跨站劫持。
		// containerId 严格白名单(首字符非 `-` 防 flag 注入、无 shell 元字符)+ shell 枚举白名单;
		// 命令 array 化经 target.ExecInteractive 不拼 shell。握手成功(PTY 建立)后写审计(detail 脱敏)。
		// 比 /servers/{id} 多两段,不会被吞。
		ar.Get("/servers/{id}/containers/{containerId}/terminal", makeContainerTerminalHandler(csv, aud))
		// 主机 shell 终端(「连服务器终端」的目标:SSH 直起交互 shell)。进容器留给用户在 shell 里
		// 自己 docker exec 自由探索(不绑死容器;很多服务器没 docker)。WS 升级,同源校验 + shell
		// 白名单;审计 server_terminal。比 /servers/{id} 多一段,不会被吞。
		ar.Get("/servers/{id}/terminal", makeServerTerminalHandler(csv, aud))
		// 批量执行命令(服务器状态页 → 勾选多机 → 同步执行 + 历史回看)。任意 shell 命令是功能
		// 本质:单管理员平台、既有 WS 终端单机本就任意命令,批量入口不提升权限面。护栏:命令 ≤8KiB、
		// 机器数 ≤100、单机超时 5-300s、并发 6、输出截 64KiB;逐机独立容错(200 + ok:false,不 500);
		// 每次尝试写审计(命令摘要 + 计数)。执行经 sh -c(target.Upload 同一先例)。字面段 commands
		// 优先于 {id},/servers/commands 不会被 /servers/{id} 吞;runs/{runId} 深一层同理。
		scmd := o.serverCmds
		ar.Post("/servers/commands/batch", makeBatchCommandHandler(scmd, aud))
		ar.Get("/servers/commands/runs", makeListCommandRunsHandler(scmd))
		ar.Get("/servers/commands/runs/{runId}", makeGetCommandRunHandler(scmd))
		// SSH 隧道(0099;local -L / remote -R / dynamic -D)。隧道配置 CRUD + 启停/
		// 重启/测试 + helper 日志尾部;运行态(心跳文件)合并在 GET 列表里。隧道以独立
		// helper 子进程运行(同二进制 --tunnel-helper),宿主重启不杀、心跳新鲜即收养。
		// GET(列表/日志)过 auth;POST/PUT/DELETE 为写方法,过 auth + CSRF + 审计
		//(启停改本机监听端口与转发面,敏感)。ts 为 nil 时端点 503。
		ts := o.sshTunnels
		ar.Get("/tunnels", makeListTunnelsHandler(ts))
		ar.Post("/tunnels", makeCreateTunnelHandler(ts, aud))
		ar.Put("/tunnels/{id}", makeUpdateTunnelHandler(ts, aud))
		ar.Delete("/tunnels/{id}", makeDeleteTunnelHandler(ts, aud))
		ar.Post("/tunnels/{id}/start", makeTunnelStartHandler(ts, aud))
		ar.Post("/tunnels/{id}/stop", makeTunnelStopHandler(ts, aud))
		ar.Post("/tunnels/{id}/restart", makeTunnelRestartHandler(ts, aud))
		ar.Post("/tunnels/{id}/test", makeTunnelTestHandler(ts))
		ar.Get("/tunnels/{id}/logs", makeTunnelLogsHandler(ts))
		// 通知渠道(Story 5.1;FR-19)。nf 为 nil 时 handler 返回 503。
		// GET(列表/详情)过 auth;POST/PUT/DELETE/test 为写方法,过 auth + CSRF。
		// 敏感字段(SMTP 密码)加密入库、响应仅 hasPassword。test 须在 {id} 路由内单独注册。
		nf := o.notifications
		// 通知全局配置(外发通知默认文案语言)。
		ar.Get("/notifications/config", makeGetNotifyConfigHandler(nf))
		ar.Put("/notifications/config", makeSetNotifyConfigHandler(nf))

		// 运行数据保留策略(全局)。GET 过 auth;PUT 写方法过 auth + CSRF。
		ar.Get("/retention/config", makeGetRetentionConfigHandler(o.retention))
		ar.Put("/retention/config", makeSetRetentionConfigHandler(o.retention))

		// 平台自身备份恢复(管理员):策略/立即备份/列表/下载/删除/传对象存储/在线恢复。
		// GET 过 auth;写方法过 auth + CSRF(写方法均留 backup.* 审计)。
		// /restore 与 /restore/pending 须在 /{id} 通配路由前注册,避免被吞。
		ar.Get("/backup/config", makeGetBackupConfigHandler(o.backupSvc))
		ar.Put("/backup/config", makeSetBackupConfigHandler(o.backupSvc))
		ar.Post("/backup/run", makeRunBackupHandler(o.backupSvc, aud))
		ar.Get("/backup/list", makeListBackupsHandler(o.backupSvc))
		ar.Post("/backup/restore", makeBackupRestoreHandler(o.backupSvc, o.objectStores, o.reexecGuard, aud))
		ar.Get("/backup/restore/pending", makeBackupRestorePendingHandler(o.backupSvc))
		ar.Delete("/backup/restore/pending", makeBackupRestoreCancelHandler(o.backupSvc, aud))
		ar.Get("/backup/{id}/download", makeBackupDownloadHandler(o.backupSvc))
		ar.Post("/backup/{id}/upload-objectstore", makeBackupUploadObjectStoreHandler(o.backupSvc, aud))
		ar.Delete("/backup/{id}", makeDeleteBackupHandler(o.backupSvc, aud))

		// 磁盘清理手动触发(设置页「立即清理」;target 枚举白名单)。写方法过 auth + CSRF + 审计。
		ar.Post("/maintenance/cleanup", makeMaintenanceCleanupHandler(o.maintenance, aud))

		// DNS 提供商集成层(R3 E3.1–E3.4):Cloudflare / DNSPod / 阿里云 DNS 接入(凭据走 vault)。
		// dp 为 nil → handler 返回 503。GET(列表)过 auth;POST/PUT/DELETE 为写方法,过 auth + CSRF + 审计。
		// 一个提供商可托管多个根区(zone 增删端点);Secret 经 vault 密文、apply 时即用即弃,
		// 响应/审计/日志绝无明文;API ID 非机密,可回显。
		dp := o.dnsProviders
		ar.Get("/dns/providers", makeListDNSProvidersHandler(dp))
		ar.Post("/dns/providers", makeCreateDNSProviderHandler(dp, o.vault, aud))
		ar.Put("/dns/providers/{id}", makeUpdateDNSProviderHandler(dp, o.vault, aud))
		ar.Post("/dns/providers/{id}/verify", makeVerifyDNSProviderHandler(dp, aud))
		ar.Post("/dns/providers/{id}/zones", makeAddDNSZoneHandler(dp, aud))
		ar.Delete("/dns/providers/{id}/zones/{zoneId}", makeRemoveDNSZoneHandler(dp, aud))
		ar.Delete("/dns/providers/{id}", makeDeleteDNSProviderHandler(dp, o.vault, aud))

		// 阿里云 ECS 批量导入(设置→阿里云 ECS):多套账户配置(AK 明文列 + Secret 密文 BLOB)
		// + 拉实例 + 手选范围批量导入/更新 servers 行(每批统一认证)。ae 为 nil → handler 返回
		// 503。GET(列表/实例)过 auth;POST/PUT/DELETE/test/import 为写方法,过 auth + CSRF + 审计。
		// AccessKeySecret 绝不回显(只写字段,留空=不修改);逐台导入结果独立容错(200 + ok:false)。
		ae := o.aliyunEcs
		ar.Get("/aliyun/accounts", makeListAliyunAccountsHandler(ae))
		ar.Post("/aliyun/accounts", makeCreateAliyunAccountHandler(ae, aud))
		ar.Put("/aliyun/accounts/{id}", makeUpdateAliyunAccountHandler(ae, aud))
		ar.Delete("/aliyun/accounts/{id}", makeDeleteAliyunAccountHandler(ae, aud))
		ar.Post("/aliyun/accounts/{id}/test", makeTestAliyunAccountHandler(ae, aud))
		ar.Get("/aliyun/accounts/{id}/instances", makeListAliyunInstancesHandler(ae))
		ar.Post("/aliyun/import", makeAliyunImportHandler(ae, aud))
		// ECS 开关机(服务器状态页电源按钮):status 只读过 auth;power 为写方法,
		// 过 auth + CSRF + 审计(aliyun.power)。发起即返回不等待;逐台独立容错。
		ar.Get("/aliyun/power/status", makeAliyunPowerStatusHandler(ae))
		ar.Post("/aliyun/power", makeAliyunPowerHandler(ae, aud))

		// 对象存储集成层(流水线 OSS 上传节点配套):本期仅阿里云 OSS(凭据走 vault)。
		// obs 为 nil → handler 返回 503。GET(列表)过 auth;POST/PUT/DELETE/test 为写方法,
		// 过 auth + CSRF + 审计。AccessKeySecret 经 vault 密文、上传/测试时即用即弃,
		// 响应/审计/日志绝无明文;AccessKeyId 非机密,可回显。
		obs := o.objectStores
		ar.Get("/object-stores", makeListObjectStoresHandler(obs))
		// 浏览桶内目录(只读;marker 游标单页,桶可能对象巨多,绝不聚合全量)。
		ar.Get("/object-stores/{id}/objects", makeBrowseObjectStoreHandler(obs))
		// 列举账户下的桶(只读;设置页 Bucket 字段的候选下拉)。
		ar.Get("/object-stores/{id}/buckets", makeListBucketsHandler(obs))
		// 未落库直连探测列桶(添加弹窗配置尚未创建;明文凭据仅内存态,不审计/不落库)。
		ar.Post("/object-stores/buckets/probe", makeProbeBucketsHandler(obs))
		// 新建桶(浏览页「新建桶」):在既有配置的账户下建桶并自动登记新配置(写操作,过审计)。
		ar.Post("/object-stores/{id}/buckets", makeCreateBucketHandler(obs, aud))
		ar.Post("/object-stores", makeCreateObjectStoreHandler(obs, o.vault, aud))
		ar.Put("/object-stores/{id}", makeUpdateObjectStoreHandler(obs, o.vault, aud))
		ar.Post("/object-stores/{id}/test", makeTestObjectStoreHandler(obs, aud))
		ar.Delete("/object-stores/{id}", makeDeleteObjectStoreHandler(obs, o.vault, aud))

		// 服务注册网关(独立部署的 nginx 容器):基域/泛域名证书/子域名服务注册/网关编排。
		// sr 为 nil → handler 返回 503。GET 过 auth;写方法过 auth + CSRF + 审计。
		sr := o.serviceReg
		ar.Get("/servicereg/settings", makeGetServiceRegSettingsHandler(sr))
		ar.Put("/servicereg/settings", makeUpdateServiceRegSettingsHandler(sr, aud))
		ar.Get("/servicereg/gateway", makeGetServiceRegGatewayHandler(sr))
		// 查看某台网关主机容器内实际生效的 nginx.conf 及其引用文件(证书等)。
		ar.Get("/servicereg/gateway/config", makeGetServiceRegGatewayConfigHandler(sr))
		ar.Post("/servicereg/gateway/deploy", makeDeployServiceRegGatewayHandler(sr, aud))
		ar.Post("/servicereg/gateway/upgrade-image", makeUpgradeServiceRegGatewayImageHandler(sr, aud))
		ar.Delete("/servicereg/gateway", makeRemoveServiceRegGatewayHandler(sr, aud))
		ar.Get("/servicereg/domains", makeListServiceRegDomainsHandler(sr))
		ar.Post("/servicereg/domains", makeCreateServiceRegDomainHandler(sr, aud))
		ar.Delete("/servicereg/domains/{id}", makeDeleteServiceRegDomainHandler(sr, aud))
		ar.Get("/servicereg/services", makeListServiceRegServicesHandler(sr))
		// 跨环境转发:可用对端 + 服务目录(服务表单「转发其他环境」级联下拉)。
		ar.Get("/servicereg/forward/peers", makeListServiceRegForwardPeersHandler(sr))
		ar.Post("/servicereg/services", makeCreateServiceRegServiceHandler(sr, aud))
		ar.Post("/servicereg/services/ensure", makeEnsureServiceRegServiceHandler(sr, aud))
		ar.Put("/servicereg/services/{id}", makeUpdateServiceRegServiceHandler(sr, aud))
		ar.Post("/servicereg/services/{id}/enabled", makeSetServiceRegServiceEnabledHandler(sr, aud))
		ar.Delete("/servicereg/services/{id}", makeDeleteServiceRegServiceHandler(sr, aud))
		// 服务实例(多实例 upstream 成员):列表/添加/删除/摘挂。/services/{id}/instances 比
		// /services/{id} 多一段、/instances/{id}/* 首段不同,均不会被吞。
		ar.Get("/servicereg/services/{id}/instances", makeListServiceRegInstancesHandler(sr))
		ar.Post("/servicereg/services/{id}/instances", makeAddServiceRegInstanceHandler(sr, aud))
		ar.Delete("/servicereg/instances/{id}", makeDeleteServiceRegInstanceHandler(sr, aud))
		ar.Post("/servicereg/instances/{id}/attached", makeSetServiceRegInstanceAttachedHandler(sr, aud))
		ar.Post("/servicereg/apply", makeApplyServiceRegHandler(sr, aud))

		// 证书管理(certmgmt):acme.sh 自动签发/续期(DNS-01)+ 手动导入;签发引擎(宿主机脚本)
		// 运行在网关主机上。cm 为 nil → handler 返回 503。GET 过 auth;写方法过 auth + CSRF + 审计。
		// create 201 立即返回 pending 行(异步签发,前端轮询);renew/{id} 多一段,不会被吞。
		cm := o.certMgmt
		ar.Get("/certmgmt/certs", makeListCertsHandler(cm))
		ar.Post("/certmgmt/certs", makeCreateCertHandler(cm, aud))
		ar.Post("/certmgmt/certs/import", makeImportCertHandler(cm, aud))
		ar.Get("/certmgmt/certs/{id}", makeGetCertHandler(cm))
		ar.Post("/certmgmt/certs/{id}/renew", makeRenewCertHandler(cm, aud))
		ar.Post("/certmgmt/certs/{id}/auto-renew", makeSetCertAutoRenewHandler(cm, aud))
		ar.Delete("/certmgmt/certs/{id}", makeDeleteCertHandler(cm, aud))
		// 签发引擎(acme.sh 宿主机脚本):GET 探测状态(auth);POST 显式部署/更新(auth + CSRF + 审计)。
		ar.Get("/certmgmt/engine", makeGetCertEngineHandler(cm))
		ar.Post("/certmgmt/engine/deploy", makeDeployCertEngineHandler(cm, aud))

		// 证书部署目标(0100):证书/私钥下发到远程机器指定路径 + 部署后命令;证书签发/
		// 导入/续期后自动联动。cd 为 nil → handler 返回 503。GET 过 auth;写方法过 auth + CSRF + 审计。
		cd := o.certDeploys
		ar.Get("/cert-deploys", makeListCertDeploysHandler(cd))
		ar.Post("/cert-deploys", makeCreateCertDeployHandler(cd, aud))
		ar.Put("/cert-deploys/{id}", makeUpdateCertDeployHandler(cd, aud))
		ar.Delete("/cert-deploys/{id}", makeDeleteCertDeployHandler(cd, aud))
		ar.Post("/cert-deploys/{id}/deploy", makeDeployCertDeployHandler(cd, aud))
		ar.Get("/cert-deploys/{id}/runs", makeListCertDeployRunsHandler(cd))

		// 平台 HTTPS 访问(宿主 nginx):平台自身 Web 页面发布为 HTTPS(证书复用证书管理)。
		// ph 为 nil → handler 返回 503。GET 过 auth;写方法过 auth + CSRF + 审计。
		ph := o.platformHTTPS
		ar.Get("/platform-https/settings", makeGetPlatformHTTPSSettingsHandler(ph))
		ar.Put("/platform-https/settings", makeUpdatePlatformHTTPSSettingsHandler(ph, aud))
		ar.Get("/platform-https/detect", makeDetectPlatformHTTPSHandler(ph))
		ar.Post("/platform-https/apply", makeApplyPlatformHTTPSHandler(ph, aud))
		ar.Post("/platform-https/disable", makeDisablePlatformHTTPSHandler(ph, aud))

		// 集群防火墙访问限制(两步生效):apply 临时写运行时 iptables → 从本机/mesh
		// 网络访问后 confirm 固化;rollback/disable 逃生通道。fw 为 nil → 503。
		// GET 过 auth;写方法过 auth + CSRF + 审计。
		fw := o.firewallSvc
		ar.Get("/firewall/status", makeGetFirewallStatusHandler(fw))
		ar.Put("/firewall/settings", makeUpdateFirewallSettingsHandler(fw, aud))
		ar.Post("/firewall/apply", makeApplyFirewallHandler(fw, aud))
		ar.Post("/firewall/confirm", makeConfirmFirewallHandler(fw, aud))
		ar.Post("/firewall/rollback", makeRollbackFirewallHandler(fw, aud))
		ar.Post("/firewall/disable", makeDisableFirewallHandler(fw, aud))

		// 系统级运行时配置(public_url = 平台对外访问地址;通知审批链接 / PR 回写用)。
		sc := o.systemConfig
		ar.Get("/system/config", makeGetSystemConfigHandler(sc))
		ar.Put("/system/config", makeSetSystemConfigHandler(sc, aud))

		// 本机(控制节点)指标 —— 运行 Pipewright 的这台机器自身的 CPU/内存/磁盘。与远程服务器
		// 共用同款纯静态只读脚本,但**在本机执行**(os/exec,不走 SSH、不选服务器、无需 svc)。
		// GET 只读 → 过 auth、豁免 CSRF。命令纯静态、无用户输入(AC-SEC-02);采集失败 →
		// 200 + reachable:false + 人读错误,不 500。字面段 /system/metrics 与 /system/config 互不干扰。
		ar.Get("/system/metrics", makeLocalMetricsHandler())

		// 应用商店模板(DPanel 式):内置 seed + 自定义 CRUD。GET 过 auth;写方法过 auth + CSRF + 审计。
		// apps 为 nil → handler 返回 503。/ops/apps/{id} 比 /ops/apps 多一段,不会被吞。
		apps := o.appStore
		ar.Get("/ops/apps", makeListAppTemplatesHandler(apps))
		ar.Post("/ops/apps", makeCreateAppTemplateHandler(apps, aud))
		ar.Get("/ops/apps/{id}", makeGetAppTemplateHandler(apps))
		ar.Put("/ops/apps/{id}", makeUpdateAppTemplateHandler(apps, aud))
		ar.Delete("/ops/apps/{id}", makeDeleteAppTemplateHandler(apps, aud))

		ar.Get("/notifications/channels", makeListChannelsHandler(nf))
		ar.Post("/notifications/channels", makeCreateChannelHandler(nf))
		ar.Get("/notifications/channels/{id}", makeGetChannelHandler(nf))
		ar.Put("/notifications/channels/{id}", makeUpdateChannelHandler(nf))
		ar.Delete("/notifications/channels/{id}", makeDeleteChannelHandler(nf))
		ar.Post("/notifications/channels/{id}/test", makeTestChannelHandler(nf))

		// 通知事件路由(Story 5.2;FR-20)。复用同一 notify.Service。「事件 → 渠道」映射:
		// GET(列表)过 auth;POST/DELETE 为写方法,过 auth + CSRF。未配置路由的事件不发送。
		ar.Get("/notifications/routes", makeListRoutesHandler(nf))
		ar.Post("/notifications/routes", makeCreateRouteHandler(nf))
		ar.Delete("/notifications/routes/{id}", makeDeleteRouteHandler(nf))

		// 通知模板(Story 5.3;FR-21)。复用同一 notify.Service。「事件(可选按渠道)→ 标题/正文
		// 模板」:渲染纯文本替换占位(无 RCE);未配模板的事件 → 平台默认文案(5-2 行为不变)。
		// GET(列表)过 auth;POST/PUT/DELETE 为写方法,过 auth + CSRF。
		ar.Get("/notifications/templates", makeListTemplatesHandler(nf))
		ar.Post("/notifications/templates", makeCreateTemplateHandler(nf))
		ar.Put("/notifications/templates/{id}", makeUpdateTemplateHandler(nf))
		ar.Delete("/notifications/templates/{id}", makeDeleteTemplateHandler(nf))

		// 可配置异常检测与告警(Story 6.5;FR-23)。an 为 nil 时 handler 返回 503。
		// 规则 CRUD + 立即检测 + 告警列表;检测复用 6-1 指标采集(metricsCollector 适配
		// collectServerMetrics)逐服务器逐 enabled 规则求值,命中产告警入库(指标 null /
		// 不可达跳过,不误报)。GET(rules/alerts)过 auth;POST(rules/check)+ DELETE 为
		// 写方法,过 auth + CSRF。metric/operator 枚举校验;无规则 → 空检测不报错。
		// /anomaly/rules 字面段优先于 {id},/anomaly/check、/anomaly/alerts 不会被吞。
		an := o.anomaly
		ar.Get("/anomaly/config", makeAnomalyConfigHandler(o.anomalyInterval, o.anomalyCooldown))
		ar.Get("/anomaly/rules", makeListAnomalyRulesHandler(an))
		ar.Post("/anomaly/rules", makeCreateAnomalyRuleHandler(an))
		ar.Patch("/anomaly/rules/{id}", makeUpdateAnomalyRuleHandler(an))
		ar.Delete("/anomaly/rules/{id}", makeDeleteAnomalyRuleHandler(an))
		ar.Post("/anomaly/check", makeAnomalyCheckHandler(an))
		ar.Get("/anomaly/alerts", makeListAnomalyAlertsHandler(an))
		ar.Get("/metrics/history", makeMetricsHistoryHandler(o.metricsHistory))

		// 机器监控(自维护 fork MyLinkPi/ssar):安装分发 + 平台采样总览 + 机器侧细粒度
		// 历史按需查询。o.ssar 为 nil 时各 handler 返回 503。GET 过 auth;POST 为写
		// 方法,过 auth + CSRF(审计在 handler 内)。query 的 view 白名单 / 区间校验
		// 在领域层(绝无用户输入拼进远端命令行,AC-SEC-02)。
		ar.Get("/ssar/overview", makeSsarOverviewHandler(o.ssar, o.servers))
		ar.Post("/ssar/download", makeSsarDownloadHandler(o.ssar, aud))
		ar.Post("/ssar/install", makeSsarInstallHandler(o.ssar, aud))
		ar.Get("/ssar/query", makeSsarQueryHandler(o.ssar))
		ar.Get("/ssar/history", makeSsarHistoryHandler(o.ssar))

		// 多 provider OAuth 凭据接入(连接 Gitee/GitHub/GitLab/自建)。oa 为 nil → handler 503。
		// apps 列表/upsert:GET 过 auth;PUT 写方法过 auth + CSRF(clientSecret 只写、加密入库、响应仅掩码)。
		// authorize/callback 均为 GET(过 auth、豁免 CSRF):authorize 生成绑会话 state → 302 跳授权页;
		// callback 验 state(CSRF 防护)→ Exchange → 经已注入 vault(v)把 access_token 存成 git_token
		// 凭据 → 302 跳回 /settings/vault。token/client_secret 经 vault 密文,响应/错误绝无明文。
		// 字面段 apps 优先于 {provider},/oauth/{provider}/authorize 不会被 /oauth/apps 吞。
		oa := o.oauth
		ar.Get("/oauth/apps", makeListOAuthAppsHandler(oa))
		ar.Put("/oauth/apps/{provider}", makeSaveOAuthAppHandler(oa))
		ar.Get("/oauth/{provider}/authorize", makeOAuthAuthorizeHandler(oa))
		ar.Get("/oauth/{provider}/callback", makeOAuthCallbackHandler(oa, v))

		// DORA 四指标只读聚合(FR-8-15):部署频率 / 变更前置时长 / 变更失败率 / MTTR。
		// 对既有运行数据做纯聚合(无新表、无副作用)。GET 只读 → 过 auth、豁免 CSRF。
		// projectId 可选过滤;window 形如 30d(默认)。o.doraMetrics 为 nil → handler 返回 503。
		ar.Get("/metrics/dora", makeDoraMetricsHandler(o.doraMetrics))

		// 流水线模板(FR-8-13 复用基座):命名、可复用的流水线定义,跨项目共享。
		// tpl 为 nil → handler 返回 503。GET(列表/详情)过 auth;POST/DELETE/apply 为写方法,
		// 过 auth + CSRF + 审计。模板 spec 不含明文 secret。apply-template 比 /pipeline 多一段,不会被吞。
		tpl := o.templates
		ar.Get("/templates", makeListPipelineTemplatesHandler(tpl))
		ar.Post("/templates", makeCreatePipelineTemplateHandler(tpl, aud))
		ar.Get("/templates/{id}", makeGetPipelineTemplateHandler(tpl))
		ar.Delete("/templates/{id}", makeDeletePipelineTemplateHandler(tpl, aud))
		ar.Post("/projects/{id}/pipeline/apply-template", makeApplyTemplateHandler(tpl, aud))

		// 变量组(FR-8-13 复用基座):命名、可复用的变量集合(key=value + secret 引用)。
		// vg 为 nil → handler 返回 503。GET 过 auth;POST/PUT/DELETE 为写方法,过 auth + CSRF + 审计。
		// secret 变量只存/回 credentialId + 掩码,绝无明文。
		vg := o.varGroups
		ar.Get("/variable-groups", makeListVarGroupsHandler(vg))
		ar.Post("/variable-groups", makeCreateVarGroupHandler(vg, aud))
		ar.Get("/variable-groups/{id}", makeGetVarGroupHandler(vg))
		ar.Put("/variable-groups/{id}", makeUpdateVarGroupHandler(vg, aud))
		ar.Delete("/variable-groups/{id}", makeDeleteVarGroupHandler(vg, aud))

		// 自定义节点(复用库 Tier 2):命名、可复用的「单节点」(nodeType + config 快照)。
		// cn 为 nil → handler 返回 503。GET 过 auth;POST/PUT/DELETE 为写方法,过 auth + CSRF + 审计。
		// config 为自由 KV 快照,不含明文 secret。
		cn := o.customNodes
		ar.Get("/custom-nodes", makeListCustomNodesHandler(cn))
		ar.Post("/custom-nodes", makeCreateCustomNodeHandler(cn, aud))
		ar.Get("/custom-nodes/{id}", makeGetCustomNodeHandler(cn))
		ar.Put("/custom-nodes/{id}", makeUpdateCustomNodeHandler(cn, aud))
		ar.Delete("/custom-nodes/{id}", makeDeleteCustomNodeHandler(cn, aud))

		// 后续 story 在此挂载其他 API 路由。
	})

	// 其余路径交给 SPA:存在的静态文件直接返回,否则回退 index.html。
	r.Handle("/*", spaHandler(webFS))

	return r
}

// ---- 认证中间件 --------------------------------------------------------

// requireAuth 中间件:校验会话 cookie → 注入 Session 到 context;未过 → 401 JSON。
func requireAuth(svc auth.Authenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "请先登录")
			return
		}
		cookie, err := r.Cookie(cookieSession)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "请先登录")
			return
		}
		sess, err := svc.Verify(cookie.Value)
		if err != nil {
			if errors.Is(err, auth.ErrSessionNotFound) {
				writeError(w, http.StatusUnauthorized, "unauthorized", "会话已过期,请重新登录")
				return
			}
			// 其它错误(如 DB 故障)不应被当作「会话过期」掩盖。
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		// 待二步验证的中间态会话只能访问登录第二步端点(它自行校验),其余一律
		// 401 mfa_required:前端据此停留在验证码步骤,不误判为已登录。
		if sess.MFAPending {
			writeError(w, http.StatusUnauthorized, "mfa_required", "请完成两步验证")
			return
		}
		// 滑动续期刚写库时同步重发会话 cookie:长开不刷新的 SPA 标签页里,
		// 库内会话在滑动而 cookie Expires 固定,会在固定时刻被动登出。
		if sess.Refreshed {
			setSessionCookies(w, r, sess)
		}
		ctx := context.WithValue(r.Context(), contextKeySession, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// sessionFromContext 从已认证的 context 取 Session。
func sessionFromContext(ctx context.Context) (*auth.Session, bool) {
	sess, ok := ctx.Value(contextKeySession).(*auth.Session)
	return sess, ok
}

// requireCSRF 中间件:将 X-CSRF-Token header 与服务端权威的 session.CSRFToken 比对。
// GET/HEAD/OPTIONS 豁免;写方法需 header == session.CSRFToken。
// 必须套在 requireAuth 之后(依赖 context 中的 Session)。
//
// 相较旧的「header == cookie」双提交方案,以会话存储的 csrf_token 为权威值,
// 子域/MITM 写入伪造 cookie 无法绕过(攻击者读不到 HttpOnly 会话对应的服务端 token)。
// 前端契约不变:它仍发送 header = pipewright_csrf cookie 值,而该 cookie 下发的正是
// session.CSRFToken,故 header 仍等于 session.CSRFToken。
func requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		sess, ok := sessionFromContext(r.Context())
		if !ok || sess == nil {
			// 未经 requireAuth 注入 Session:无法校验 CSRF。
			writeError(w, http.StatusForbidden, "csrf_invalid", "CSRF token 缺失或不匹配")
			return
		}
		headerVal := r.Header.Get(headerCsrf)
		if headerVal == "" || subtle.ConstantTimeCompare([]byte(headerVal), []byte(sess.CSRFToken)) != 1 {
			writeError(w, http.StatusForbidden, "csrf_invalid", "CSRF token 缺失或不匹配")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- 认证 Handler --------------------------------------------------------

// makeLoginHandler 返回 POST /api/auth/login handler。
// 登录端点豁免 CSRF(此时尚无会话);但受锁定限制。
func makeLoginHandler(svc auth.Authenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "no_auth", "认证服务未初始化")
			return
		}
		// 限制匿名请求体大小,防超大 body。
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}

		sess, err := svc.Login(req.Username, req.Password, clientIP(r))
		if err != nil {
			var lockedErr *auth.ErrLockedOut
			if errors.As(err, &lockedErr) {
				mins := int(lockedErr.RetryAfter.Minutes()) + 1
				msg := fmt.Sprintf("登录失败次数过多,请 %d 分钟后重试", mins)
				writeError(w, http.StatusTooManyRequests, "locked_out", msg)
				return
			}
			if errors.Is(err, auth.ErrInvalidCredentials) {
				writeError(w, http.StatusUnauthorized, "invalid_credentials", "用户名或口令错误")
				return
			}
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}

		// 设置会话 cookie 与可读 CSRF cookie,并回显身份(用户名 + 角色)。
		setSessionCookies(w, r, sess)
		// 已启用 TOTP 的账户:口令虽过,仍处于「待二步验证」中间态——cookie 已下发
		// (验证码端点复用该会话 + CSRF),但响应标记 mfa_required,由前端进入第二步。
		if sess.MFAPending {
			writeJSON(w, http.StatusOK, map[string]any{"mfa_required": true})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"username": sess.Username, "role": sess.Role})
	}
}

// setSessionCookies 下发 HttpOnly 会话 cookie 与可读 CSRF cookie。
// Secure 标志:直连 TLS 或「可信反代 + X-Forwarded-Proto: https」时置位
// (见 requestIsSecure;平台自身的 platformHTTPS/nginx 反代形态即靠后者)。
// 供登录与注册自动登录复用。
func setSessionCookies(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	secure := requestIsSecure(r)
	http.SetCookie(w, &http.Cookie{
		Name:     cookieSession,
		Value:    sess.Token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		Expires:  sess.ExpiresAt,
	})
	// 下发可读 CSRF cookie(非 HttpOnly,前端 JS 读取)。
	http.SetCookie(w, &http.Cookie{
		Name:     cookieCsrf,
		Value:    sess.CSRFToken,
		Path:     "/",
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		Expires:  sess.ExpiresAt,
	})

	// 仅在安全(HTTPS)请求时下发「HTTPS 登录过」标记 cookie,供前端登录页提醒:
	// 曾用 HTTPS 访问的浏览器里,残留的 Secure 会话 cookie 会让浏览器静默丢弃后续
	// HTTP 登录下发的非 Secure cookie(登录 200 但后续接口 401),只能手动清理。
	// 两个关键属性:
	//   - 自身绝不带 Secure:Secure cookie 对纯 HTTP 页面不可见,带上标记就读不到了;
	//   - 明文 HTTP 请求不写、不覆盖:HTTP 登录即使服务端成功,cookie 也可能已被浏览器
	//     丢弃,此时若把标记改成 http,警告恰好在「登录循环」场景里消失。
	//     前端在确认 HTTP 登录真正生效后自行清除该标记。
	// Expires 随会话:残留 Secure cookie 过期被清的同时标记同步失效,提醒自动停止。
	if secure {
		http.SetCookie(w, &http.Cookie{
			Name:     cookieLoginScheme,
			Value:    "https",
			Path:     "/",
			HttpOnly: false,
			SameSite: http.SameSiteLaxMode,
			Expires:  sess.ExpiresAt,
		})
	}
}

// makeSessionHandler 返回 GET /api/auth/session handler。
// 此端点本身不要求登录(用于前端探测会话状态)。
func makeSessionHandler(svc auth.Authenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "请先登录")
			return
		}
		cookie, err := r.Cookie(cookieSession)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "请先登录")
			return
		}
		sess, err := svc.Verify(cookie.Value)
		if err != nil {
			if errors.Is(err, auth.ErrSessionNotFound) {
				writeError(w, http.StatusUnauthorized, "unauthorized", "会话已过期,请重新登录")
				return
			}
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		// 待二步验证的中间态会话不算「已登录」:前端 ensureSession 据此回到验证码步骤。
		if sess.MFAPending {
			writeError(w, http.StatusUnauthorized, "mfa_required", "请完成两步验证")
			return
		}
		// 续期 CSRF cookie(会话 cookie 已存在,无需重设;setSessionCookies 两者皆发,幂等)。
		setSessionCookies(w, r, sess)
		// 回显会话身份(用户名 + 角色)。旧升级前的会话 username 为空,回退到管理员用户名。
		username := sess.Username
		if username == "" {
			if u, err := svc.AdminUsername(); err == nil {
				username = u
			}
		}
		role := sess.Role
		if role == "" {
			role = auth.RoleAdmin
		}
		writeJSON(w, http.StatusOK, map[string]string{"username": username, "role": role})
	}
}

// makeLogoutHandler 返回 POST /api/auth/logout handler。
// 已由调用方套了 requireAuth + requireCSRF。
func makeLogoutHandler(svc auth.Authenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie(cookieSession); err == nil && svc != nil {
			_ = svc.Logout(cookie.Value)
		}
		// 清除两个 cookie(Max-Age=-1)。
		http.SetCookie(w, &http.Cookie{
			Name:    cookieSession,
			Value:   "",
			Path:    "/",
			MaxAge:  -1,
			Expires: time.Unix(0, 0),
		})
		http.SetCookie(w, &http.Cookie{
			Name:    cookieCsrf,
			Value:   "",
			Path:    "/",
			MaxAge:  -1,
			Expires: time.Unix(0, 0),
		})
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---- Static / SPA --------------------------------------------------------

// securityHeaders 为全部响应设置安全响应头:
//   - CSP:前端构建产物无内联脚本、无外部源(字体 @fontsource 自托管、WS/SSE/
//     Monaco worker 均同源)。script-src 严格同源,即使出现 XSS 注入,内联脚本
//     也被拒。样式必须放行 'unsafe-inline':naive-ui 组件样式经 css-render 在
//     运行时注入内联 <style>,Vue 动态 :style 依赖内联 style 属性 —— 收紧到
//     'self' 会让浏览器拒绝创建样式表,图标(n-icon)等组件样式全部失效。
//   - X-Frame-Options + frame-ancestors:管理面板禁止被 iframe 嵌套(点击劫持)。
//   - Referrer-Policy: no-referrer:URL(含审批 token 等 query)不随 Referer 外泄。
//   - X-Content-Type-Options: nosniff:禁 MIME 嗅探。
//
// 局部 handler 用 Header().Set 覆盖同值头(如审批页的 Referrer-Policy)不受影响。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; "+
				"font-src 'self' data:; connect-src 'self'; worker-src 'self'; "+
				"object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleVersion 返回构建期注入的版本元数据(version/commit/date/goVersion/platform)。
func handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, version.Get())
}

// spaHandler 服务嵌入文件系统中的静态资源;无扩展名的路径(前端路由)回退 index.html,
// 但带扩展名却不存在的请求返回 404,避免把 SPA HTML 当成脚本/资源返回 200。
// 缓存策略:默认 no-cache 全量重拉(index.html、favicon 等非哈希文件,升级后立即生效);
// 唯 assets/ 是构建期内容哈希文件,内容变则 URL 变,immutable 一年且永不重验。
// 注意 embed.FS 文件无 ModTime,FileServer 发不出 Last-Modified,304 协商本就不可用,
// 故不设 ETag,失效完全依赖 no-cache 重拉与哈希 URL 更替。
func spaHandler(webFS fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(webFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-cache")

		if info, err := fs.Stat(webFS, name); err == nil && !info.IsDir() {
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			fileServer.ServeHTTP(w, r)
			return
		}

		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}

		serveIndex(w, webFS)
	})
}

// serveIndex 输出 SPA 入口页;Cache-Control 由 spaHandler 统一设为 no-cache。
func serveIndex(w http.ResponseWriter, webFS fs.FS) {
	index, err := fs.ReadFile(webFS, "index.html")
	if err != nil {
		http.Error(w, "index.html not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(index)
}

// ---- JSON helpers --------------------------------------------------------

// writeJSON 统一以 camelCase JSON 输出。
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type errBody struct {
	Error errDetail `json:"error"`
}

type errDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError 输出统一错误响应:{ "error": { "code": "...", "message": "..." } }
// msg 以 zh-CN 源串写在调用点;此处按请求 locale(localeMiddleware 解析)本地化。
func writeError(w http.ResponseWriter, code int, errCode, msg string) {
	msg = i18n.T(localeOf(w), msg)
	writeJSON(w, code, errBody{Error: errDetail{Code: errCode, Message: msg}})
}
