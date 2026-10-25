// Command pipewright 是平台入口:加载配置 → 打开存储(应用迁移)→ 装配 HTTP → 启动。
// 整个平台为单静态二进制(前端经 go:embed 内嵌),无前后端分离部署。
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	root "github.com/huangchengsir/pipewright"
	"github.com/huangchengsir/pipewright/internal/ai"
	"github.com/huangchengsir/pipewright/internal/aliyunecs"
	"github.com/huangchengsir/pipewright/internal/anomaly"
	"github.com/huangchengsir/pipewright/internal/approval"
	"github.com/huangchengsir/pipewright/internal/appstore"
	"github.com/huangchengsir/pipewright/internal/artifactstore"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/backup"
	"github.com/huangchengsir/pipewright/internal/build"
	"github.com/huangchengsir/pipewright/internal/buildcache"
	"github.com/huangchengsir/pipewright/internal/ceph"
	"github.com/huangchengsir/pipewright/internal/certdeploy"
	"github.com/huangchengsir/pipewright/internal/certmgmt"
	"github.com/huangchengsir/pipewright/internal/chain"
	"github.com/huangchengsir/pipewright/internal/config"
	"github.com/huangchengsir/pipewright/internal/cron"
	"github.com/huangchengsir/pipewright/internal/dagrun"
	"github.com/huangchengsir/pipewright/internal/dbadmin"
	"github.com/huangchengsir/pipewright/internal/dbbackup"
	"github.com/huangchengsir/pipewright/internal/deploy"
	"github.com/huangchengsir/pipewright/internal/diskadmin"
	"github.com/huangchengsir/pipewright/internal/dnsprovider"
	"github.com/huangchengsir/pipewright/internal/fileadmin"
	"github.com/huangchengsir/pipewright/internal/firewall"
	"github.com/huangchengsir/pipewright/internal/httpapi"
	"github.com/huangchengsir/pipewright/internal/identity"
	"github.com/huangchengsir/pipewright/internal/labels"
	"github.com/huangchengsir/pipewright/internal/library"
	"github.com/huangchengsir/pipewright/internal/maintenance"
	"github.com/huangchengsir/pipewright/internal/mask"
	"github.com/huangchengsir/pipewright/internal/mesh"
	"github.com/huangchengsir/pipewright/internal/mesh/easytier"
	"github.com/huangchengsir/pipewright/internal/meshdns"
	"github.com/huangchengsir/pipewright/internal/metrics"
	"github.com/huangchengsir/pipewright/internal/mfa"
	"github.com/huangchengsir/pipewright/internal/notify"
	"github.com/huangchengsir/pipewright/internal/oauth"
	"github.com/huangchengsir/pipewright/internal/objectstore"
	"github.com/huangchengsir/pipewright/internal/opschat"
	"github.com/huangchengsir/pipewright/internal/pacloader"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/platformhttps"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/prstatus"
	"github.com/huangchengsir/pipewright/internal/pushsource"
	"github.com/huangchengsir/pipewright/internal/pveadmin"
	"github.com/huangchengsir/pipewright/internal/redisadmin"
	"github.com/huangchengsir/pipewright/internal/registryhub"
	"github.com/huangchengsir/pipewright/internal/repocache"
	"github.com/huangchengsir/pipewright/internal/retention"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/runner"
	"github.com/huangchengsir/pipewright/internal/secscan"
	"github.com/huangchengsir/pipewright/internal/servercmd"
	"github.com/huangchengsir/pipewright/internal/servicereg"
	"github.com/huangchengsir/pipewright/internal/ssar"
	"github.com/huangchengsir/pipewright/internal/sshtunnel"
	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/systemcfg"
	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/trigger"
	"github.com/huangchengsir/pipewright/internal/vault"
	"github.com/huangchengsir/pipewright/internal/version"
	"github.com/huangchengsir/pipewright/internal/waitnode"
)

func main() {
	// --version / -v:在任何配置加载、存储打开等副作用之前处理,纯查询即打印退出。
	for _, a := range os.Args[1:] {
		if a == "--version" || a == "-version" || a == "-v" {
			fmt.Println(version.String()) // 走 stdout(惯例),便于脚本捕获
			return
		}
	}

	// --tunnel-helper:SSH 隧道 helper 子进程模式(0099)。主进程为每条启用的隧道以
	// 本二进制 re-exec 自己拉起一个独立 helper(与主进程解耦:宿主停机/自升级不杀它,
	// 心跳新鲜即收养 → 重启 pipewright 隧道零中断)。argv 只有隧道 id,凭据由 helper
	// 经继承的 env 自行解析,明文仅在 helper 进程内即用即弃。在任何正常装配前分派。
	if len(os.Args) > 1 && os.Args[1] == sshtunnel.HelperFlag {
		if err := sshtunnel.RunHelperMain(os.Args[2:]); err != nil {
			log.Fatalf("[sshtunnel] helper 退出:%v", err)
		}
		return
	}

	cfg := config.Load()

	dbDriver, dbDSN, err := cfg.StoreConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// restore 子命令:CLI 离线恢复(在打开存储之前分派——此时不持有任何数据库
	// 句柄,可安全替换 SQLite 文件/回放 MySQL dump;详见 restore_cli.go)。
	if len(os.Args) > 1 && os.Args[1] == "restore" {
		os.Exit(runRestoreCLI(cfg, os.Args[2:]))
	}

	// UI 在线恢复的启动期应用:上一进程退出前上传并暂存了备份(pending-restore.json),
	// 此刻(存储打开之前)应用它并清除标记。失败属致命(半恢复状态不可静默带病启动)。
	if rep, berr := backup.ApplyPendingRestore(backupRestoreOptions(dbDriver, dbDSN)); berr != nil {
		log.Fatalf("[backup] 应用待恢复备份失败:%v(现库已转存 restore-safety/,请人工介入)", berr)
	} else if rep != nil {
		log.Printf("[backup] 已应用在线恢复:数据库=%v acme=%v artifacts=%v masterKey=%s",
			rep.AppliedDB, rep.AppliedAcme, rep.AppliedArtifacts, rep.MasterKeyAction)
		for _, w := range rep.Warnings {
			log.Printf("[backup] 恢复告警:%s", w)
		}
	}

	st, err := store.OpenWithConfig(store.OpenConfig{Driver: dbDriver, DSN: dbDSN})
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer func() { _ = st.Close() }()

	// 启动期 fail-fast:内嵌前端必须含 index.html,否则运行期白屏(空/错误 web/dist 也能编过)。
	webFS := root.WebFS()
	if _, err := fs.Stat(webFS, "index.html"); err != nil {
		log.Fatalf("embedded web/dist 缺少 index.html(前端未正确构建?): %v", err)
	}

	if dbDriver == "mysql" {
		log.Printf("data db: mysql (%s)", st.Dialect)
	} else if abs, err := filepath.Abs(dbDSN); err == nil {
		log.Printf("data db: %s", abs)
	}

	// 装配认证服务(nil clock → RealClock)并执行管理员引导。
	// 随机初始口令的落盘路径:显式 env 优先;缺省落数据目录(sqlite 为库同级,mysql 为
	// 工作目录 —— systemd/compose 的 WorkingDirectory 均已设为数据目录)。
	authPwdFile := cfg.AdminPasswordFile
	if authPwdFile == "" {
		if dbDriver == "mysql" {
			authPwdFile = "admin_password.txt"
		} else {
			authPwdFile = filepath.Join(filepath.Dir(dbDSN), "admin_password.txt")
		}
	}
	authSvc := auth.NewService(st.DB, nil)
	if err := authSvc.Bootstrap(cfg.AdminUsername, cfg.AdminPassword, authPwdFile); err != nil {
		log.Fatalf("auth bootstrap: %v", err)
	}
	// 启动时清理 DB 中遗留的过期会话(惰性删除可能遗漏)。
	if n, err := authSvc.PurgeExpiredSessions(); err != nil {
		log.Printf("[auth] 警告:启动清理过期会话失败:%v", err)
	} else if n > 0 {
		log.Printf("[auth] 启动清理过期会话:%d 条", n)
	}

	// 装配普通用户体系(邀请/注册/项目成员)。管理员账号仍在 admin_user,不迁移。
	identitySvc := identity.New(st.DB)

	// 装配凭据保险库(Story 1.3)。master key 缺失 → 未配置态(不 panic,平台仍启动);
	// 解码/长度错误 → 视为配置错误,提示但仍以未配置态继续(/healthz 保持可用)。
	var masterKey *[config.MasterKeyLen]byte
	switch key, err := config.LoadMasterKey(); {
	case err == nil:
		masterKey = key
		log.Printf("[vault] master key 已加载,凭据保险库可用")
	case errors.Is(err, config.ErrNoMasterKey):
		log.Printf("[vault] 警告:未配置 master key(PIPEWRIGHT_MASTER_KEY 或 _FILE),凭据保险库不可用。")
	default:
		// 不打印 err 之外的任何内容;err 本身不含 key 值。
		log.Printf("[vault] 警告:master key 配置无效(%v),凭据保险库不可用。", err)
	}
	credVault := vault.New(st.DB, masterKey)

	// 审批链接签名器(「从通知直接审批」):用 master key 做 HMAC 签发/校验审批 token。
	// master key 缺失 → 禁用态(不签链接、公开页不可用),功能优雅关闭。
	var approvalSignerKey []byte
	if masterKey != nil {
		approvalSignerKey = masterKey[:]
	}
	approvalSigner := approval.NewSigner(approvalSignerKey)

	// 装配项目服务(Story 2.1):经 store 触库、经 vault 取凭据做 ls-remote 校验(进程内,用完即弃)。
	// prober 传 nil → 使用默认 go-git ListRemote 实现(纯 Go,无宿主 git 依赖,不驻留)。
	projectSvc := project.New(st.DB, credVault, nil)

	// 装配触发配置服务(Story 2.3):复用 vault secretbox(master key)加密 webhook 签名密钥。
	// master key 未配置时,涉及密钥的读取/重置降级为明确错误(不 panic)。
	triggerSvc := trigger.New(st.DB, credVault)

	// 装配流水线配置服务(Story 2.2):经 store 触库,惰性默认 + spec 校验 + YAML 渲染。
	pipelineSvc := pipeline.New(st.DB)

	// 装配构建/部署配置服务(Story 2.4):独立于 2-2 spec;经 vault 校验 secret 引用存在性 +
	// 回算掩码。master key 未配置时,涉及 secret 引用的保存降级为明确错误(不 panic)。
	pipelineSettingsSvc := pipeline.NewSettingsService(st.DB, credVault)

	// 装配定时(cron)触发配置服务(Story 8-6):每项目一份 cron 表达式 + 分支 + 启用开关。
	// 调度器(下方,pool 后)按启用配置分钟粒度扫描、到点经 run.Service 创建「定时」触发的运行。
	cronSvc := cron.NewService(st.DB)

	// 装配流水线模板 + 变量组复用基座(FR-8-13 · 对标 Jenkins Shared Library / 云效模板与变量组)。
	// 模板:命名、可复用的流水线定义(与项目流水线同一套 stages 模型),跨项目共享;应用模板经
	// pipelineSvc.Save 把 stages 落到项目流水线(复用同一套规范化/校验/渲染)。变量组:命名、可复用的
	// 变量集合,镜像每流水线变量模型;secret 经 credVault 校验存在性、只存引用,明文绝不入库。
	templateSvc := library.NewTemplateService(st.DB, pipelineSvc)
	varGroupSvc := library.NewVarGroupService(st.DB, credVault)
	// 自定义节点(复用库 Tier 2):命名、可复用的「单节点」(nodeType + config 快照),供选择器复用。
	customNodeSvc := library.NewCustomNodeService(st.DB)

	// 装配项目级并发上限配置服务(FR-8-10 并发/队列控制):每项目一份 max_concurrent。
	// 注入 worker pool 做准入(下方);经 /api/projects/{id}/concurrency 读写。
	concurrencySvc := run.NewConcurrencyService(st.DB)
	parameterSvc := run.NewParameterService(st.DB)

	// 装配流水线串联配置服务(FR-8-11):每项目一份「成功后触发的下游目标」列表。
	// 串联终态钩子(下方,与通知钩子同槽组合)在 run 落 success 时据此创建下游运行;
	// 经 /api/projects/{id}/chain 读写。环路安全在 chain.NewHook(深度门 + 路径门)。
	chainSvc := chain.NewService(st.DB)

	// 装配运行服务(Story 3.1):本期桩 runner 跑占位步骤打通状态机;真实构建/部署=3-3/4-x。
	// worker pool 在 aiSvc 装配后创建(以便注入 best-effort 自动诊断钩子,Story 7.2)。
	runSvc := run.New(st.DB)
	// DORA 指标只读聚合(FR-8-15):同一 *service 实现暴露为 MetricsService,无新连接/新表。
	doraMetricsSvc := run.NewMetricsService(runSvc)

	// 装配人工审批门协调器 + 持久层(Story 8-4):DAG 调度到 Gate 阶段时阻塞等待批准/拒绝。
	// 协调器为进程内(零持久状态);记录落 run_approvals 供 UI/审计/重启清理。
	approvalCoord := approval.New()
	approvalStore := approval.NewStore(st.DB)

	// 「等待」节点(wait)手动跳过协调器:等待中的节点可经 /api/runs/{id}/skip-wait 跳过。
	// 进程内零持久状态;等待者随运行结束/重启清理(重启孤儿由 FailOrphanedRuns 兜底)。
	waitCoord := waitnode.New()

	// 重启清理:进程重启会丢失内存等待者,把残留 waiting_approval 运行清为 failed(孤儿不可恢复)。
	if n, err := runSvc.FailOrphanedRuns(context.Background()); err != nil {
		log.Printf("[run] 警告:清理孤儿运行失败:%v", err)
	} else if n > 0 {
		log.Printf("[run] 启动清理孤儿运行(waiting_approval/running):%d 条 → failed", n)
	}

	// 磁盘维护服务(磁盘治理,0094):构建缓存 GC / 仓库镜像回收 / 构建机与本机孤儿临时文件清扫。
	// 先带 db 构造(此处即用),缓存/镜像/服务器层在装配好后链式注入;自动定时与设置页手动入口
	// 共用同一套实现。启动先清一次本机孤儿——上次进程被杀时 defer 未跑的构建工作区/暂存。
	maintSvc := maintenance.New(st.DB)
	if deleted, reclaimed, err := build.SweepLocalOrphans(os.TempDir(), 48*time.Hour); err != nil {
		log.Printf("[maintenance] 警告:启动清扫本机孤儿临时文件失败:%v", err)
	} else if len(deleted) > 0 {
		log.Printf("[maintenance] 启动清扫:删除 %d 个构建孤儿临时文件/目录(48h 前的),回收 %d 字节", len(deleted), reclaimed)
		for _, p := range deleted {
			log.Printf("[maintenance] 启动清扫:删除 %s", p)
		}
	}

	// 装配诊断反馈服务(Story 7.5;FR-26):诊断 👍/👎(👎 可附正确根因)持久化 + 准确率统计(知识库种子)。
	// 复用同一 *sql.DB(参数化 SQL);correctRootCause 脱敏在 httpapi 层(过 mask)+ 领域层长度上限兜底。
	// upsert by run_id(同 run 改判覆盖)。无 init 副作用、不抬高空载内存。
	feedbackSvc := run.NewFeedbackService(st.DB)

	// 装配 webhook 接收器(Story 3.2):按 token 定位项目触发配置、解密密钥验签、
	// 解析 Gitee 投递、按 events+分支映射匹配、去重后经 RunService 创建运行(入 pool 调度)。
	webhookReceiver := trigger.NewReceiver(st.DB, credVault, httpapi.NewRunCreator(runSvc))

	// 装配审计地基(Story 1.4):敏感操作成功后写 append-only 审计表(SQLite trigger 硬拦
	// UPDATE/DELETE)。Masker 在 detail 落库前脱敏(绝不写明文 secret);可选远端 sink
	// (PIPEWRIGHT_AUDIT_SINK)使本地被删后远端仍完整。sink 失败降级不阻断主操作。
	auditMasker := mask.NewMasker()
	var auditSink audit.Sink
	if sink, err := audit.SinkFromEnv(); err != nil {
		log.Printf("[audit] 警告:远端 sink 配置无效(%v),仅启用本地审计。", err)
	} else if sink != nil {
		auditSink = sink
		log.Printf("[audit] 远端审计 sink 已启用(PIPEWRIGHT_AUDIT_SINK)")
	}
	auditRec := audit.New(st.DB, auditMasker, auditSink)

	// 装配可配置 AI 提供商服务(Story 7.1):复用 vault secretbox 加密 apiKey(密文入库)。
	// Test 探测用带超时的 HTTP 客户端(~8s);master key 未配置时涉及密钥的操作降级为明确错误。
	// 未配置/未启用 → 下游优雅禁用,核心 CI/CD 不依赖此服务(NFR-10)。
	aiSvc := ai.New(st.DB, credVault, &http.Client{Timeout: 8 * time.Second})

	// 装配仓库分析器(Story 2.5):go-git 浅克隆(内存 storer + memfs,Depth:1)读 manifest
	// 检测技术栈;严格 SSRF 收口;克隆失败优雅降级(不致命)。无 init 副作用/不驻留。
	repoAnalyzer := ai.NewRepoAnalyzer()

	// 装配成功/失败差异计算器(Story 7.3;FR-25):go-git 克隆 + 两 commit tree diff;严格 SSRF 收口;
	// 克隆失败/commit 不可达优雅降级(不致命)。无 init 副作用/不驻留。
	runDiffer := ai.NewRunDiffer()

	// 装配进程内 worker pool(Story 3.1)+ best-effort 自动诊断钩子(Story 7.2)。
	// 钩子在 run→failed 后「取失败日志→脱敏→ai.Diagnose→保存」,经 Option 注入使 run 包不 import ai。
	// AI 未配/失败仅降级(status=unavailable),绝不阻断 run 终态(NFR-10)。pool 调度多 run 并行、结果独立。
	// 日志/诊断/反馈/通知出网脱敏器(Story 3.6/7-2/7-5/5-3;AC-SEC-04)——**红线修**:
	// 不再只登记桩假 secret,而是 per-run 解析该 run 真实用到的全部凭据(项目主凭据 + 流水线
	// settings 的 secret 构建/环境变量 + 镜像仓库凭据),从 vault Reveal 明文登记进 Masker。
	// 这样 3-3 真实构建落地后,真实日志/诊断/反馈/通知里的真凭据也一律 [MASKED],绝不明文外发。
	secretSrc := httpapi.NewRunSecretSource(runSvc, projectSvc, pipelineSettingsSvc, credVault)

	// 装配通知渠道服务(Story 5.1;FR-19):复用 vault secretbox 加密敏感字段(如 SMTP 密码,密文入库)。
	// webhook 投递用带超时的注入 HTTP 客户端(~8s);webhook URL 经 SSRF 收口(拒云元数据/链路本地)。
	// master key 未配置时,涉及敏感字段的保存/读取降级为明确错误(不 panic)。未启用渠道发送时优雅跳过。
	// 提前装配(先于 pool)以便把通知钩子注入 pool(Story 5.2;FR-20)。
	notifySvc := notify.New(st.DB, credVault, &http.Client{Timeout: 8 * time.Second})

	// 选择运行执行器(Story 3-3/3-5):按 PIPEWRIGHT_BUILDER 开关装配真实隔离构建器或桩。
	//   - real :强制真实 Builder(探测不到容器 CLI 即启动失败,诚实不假装)。
	//   - stub :强制桩 runner(合成日志,不碰容器;dev/CI 无 Docker 时用)。
	//   - 缺省 auto:探测到 docker/nerdctl/podman 用 real,否则回退桩(优雅降级,NFR-10)。
	// 真实 Builder 经构造函数注入 projectSvc/pipelineSettingsSvc/credVault;真实日志/产物/失败日志
	// 经同一 run.StepSink 喂出,复用既有持久化/SSE/脱敏(下方 WithLogMaskerFunc 对真实日志同样生效)。
	// 运行执行器默认 DAG 调度执行器(Epic 8 · 8-1↔8-3):按阶段 needs 构 DAG 调度
	// (无依赖阶段并行、失败阻断下游),真按 UI 可视化流水线 stages/script job/deploy_ssh/notify
	// 执行,阶段编排经既有 StepSink 持久化。**默认启用**(下方据 PIPEWRIGHT_RUNNER 选择):
	// 仅 PIPEWRIGHT_RUNNER=legacy 才回退 PIPEWRIGHT_BUILDER 选出的旧版固定流程 Builder / 桩 runner。
	// 目标服务器(SSH 执行层)+ 构建机池(FR-8-14 续 / FR-8-19 池化):项目/stage 配标签选择器,
	// 从打了标签的服务器池按「优先级 → 流水线亲和 → 负载」选机远程构建(控制机本地克隆 → 经 SSH
	// 传工作区 → 远程容器跑;token 只在控制机)。每机并发槽位默认 1,PIPEWRIGHT_RUNNER_SLOTS
	// 调全局默认,单机可用 servers.max_builds 覆盖(设置界面)。
	targetSvc := target.New(st.DB, credVault, nil)
	// 内置本地 registry(registryhub):控制机本机双服务 registry 栈(制品 + pull-through 缓存)、
	// daemon.json 手动勾选下发、镜像 tag 保留清理。复用 targetSvc(远程机 SSH 通道);未启用时
	// 构建/部署完全保持旧行为(本地 tag 不推送)。构建接入经 builtinRegistryOpt 注入 Builder。
	// TLS 证书经 CertSource 软引用 certmgmt(晚绑:certSvc 在其后构造,见下方 set 接线)。
	registryCertSrc := &registryCertSource{}
	registryHubSvc := registryhub.New(st.DB, targetSvc, registryhub.Options{Certs: registryCertSrc})
	builtinRegistryOpt := build.WithBuiltinRegistry(registryHubSvc.ResolveBuiltin)
	// 批量执行命令(服务器状态页 → 勾选多机 → 同步执行 + 历史回看):复用 targetSvc 的
	// SSH 执行层逐机并发跑 sh -c,结果落本地库保最近 200 次;每次尝试写审计。
	serverCmdSvc := servercmd.New(st.DB, targetSvc)
	// SSH 隧道(0099;local -L / remote -R / dynamic -D):配置 CRUD + 独立 helper 子进程
	// 运行时。helper 与主进程解耦(重启互不影响),对账循环按心跳收养/重拉/重启。
	sshtunnelSvc := sshtunnel.New(st.DB, targetSvc, filepath.Dir(cfg.DBPath))
	// Assistant sessions use a separate, local-only recorder and masker.
	opsMasker := mask.NewMasker()
	if masterKey != nil {
		opsMasker.RegisterSecret(fmt.Sprintf("%x", masterKey[:]))
		opsMasker.RegisterSecret(base64.StdEncoding.EncodeToString(masterKey[:]))
	}
	var opsChatSvc *opschat.Service
	// opschat 运行参数:PIPEWRIGHT_OPS_SSH_SLOTS 调全局受限 SSH 槽位(默认 4),
	// PIPEWRIGHT_OPS_APPROVAL_TTL_MINUTES 调审批有效期(默认 15 分钟)。
	opsRuntime := opschat.RuntimeLimits{}
	if v := strings.TrimSpace(os.Getenv("PIPEWRIGHT_OPS_SSH_SLOTS")); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			opsRuntime.SSHSlots = n
		} else {
			log.Printf("[opschat] 警告:PIPEWRIGHT_OPS_SSH_SLOTS=%q 非法(须为正整数),用默认 4", v)
		}
	}
	if v := strings.TrimSpace(os.Getenv("PIPEWRIGHT_OPS_APPROVAL_TTL_MINUTES")); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			opsRuntime.ApprovalTTL = time.Duration(n) * time.Minute
		} else {
			log.Printf("[opschat] 警告:PIPEWRIGHT_OPS_APPROVAL_TTL_MINUTES=%q 非法(须为正整数分钟),用默认 15", v)
		}
	}
	if limited, ok := targetSvc.(target.LimitedExecutor); ok {
		// 项目维度后端:opschat 项目工具落到 project/pipeline/run 服务 + 源码读取器
		//(SourceReader 无状态,这里独立构造;main.go:964 的 web 端实例互不影响)。
		opsProjects := httpapi.NewOpsProjectBackend(projectSvc, pipelineSvc, runSvc, credVault, httpapi.NewSourceReader())
		chat, err := opschat.New(opschat.Options{DB: st.DB, Vault: credVault, Executor: limited,
			Model: ai.NewOpsModel(aiSvc, opsMasker), Projects: opsProjects, Web: ai.NewWebTools(aiSvc, opsMasker), Masker: opsMasker, LocalRecorder: audit.New(st.DB, opsMasker, nil), Runtime: opsRuntime,
			Context: func() opschat.ContextPolicy { return aiContextPolicy(aiSvc) }})
		if err == nil {
			err = chat.Start(context.Background())
		}
		if err == nil {
			opsChatSvc = chat
		} else {
			log.Printf("[opschat] unavailable: local key, storage or executor configuration")
		}
	}
	runnerSvc := runner.New(st.DB, targetExister{targetSvc})
	// 机器标签登记处(标签字典,可先建后挂):设置页建/删管理,各选择器下拉候选 = 登记处 ∪ 机器实际标签。
	labelsSvc := labels.New(st.DB, targetSvc)
	runnerSlots := 1
	if v := strings.TrimSpace(os.Getenv("PIPEWRIGHT_RUNNER_SLOTS")); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			runnerSlots = n
		} else {
			log.Printf("[runner] 警告:PIPEWRIGHT_RUNNER_SLOTS=%q 非法(须为正整数),用默认 1", v)
		}
	}
	runnerSched := runner.NewScheduler(st.DB, targetProber{targetSvc}, runnerSlots)

	// 制品库(Story 8-16 / FR-8-16):构建后把 jar/dist 真字节归档,部署时取真字节上传目标机
	// (否则只有占位 reference,jar/dist 无法真正部署)。默认落 DB 同级 artifacts/,可经
	// PIPEWRIGHT_ARTIFACT_DIR 改。构造失败 → 降级 nil(保持旧占位行为,不中断启动)。
	var artStore *artifactstore.Store
	{
		artDir := strings.TrimSpace(os.Getenv("PIPEWRIGHT_ARTIFACT_DIR"))
		if artDir == "" {
			artDir = filepath.Join(filepath.Dir(cfg.DBPath), "artifacts")
		}
		if as, aerr := artifactstore.New(artDir); aerr == nil {
			artStore = as
			log.Printf("[artifact] 制品库已启用(归档 jar/dist 真字节供部署):%s", artDir)
		} else {
			log.Printf("[artifact] 警告:制品库不可用(%v),jar/dist 部署退回占位引用", aerr)
		}
	}

	// 代码管理区(Story 8-18 / FR-8-18):中控机维护持久 bare 镜像,每次构建增量 fetch 后从本地镜像
	// 秒级出工作区(大仓库/高频构建省时省网),并给「列分支/commit」提供数据(下方 refs 端点)。
	// 默认 DB 同级 repos/(PIPEWRIGHT_REPO_CACHE_DIR 可改);PIPEWRIGHT_NO_REPO_CACHE=1 关。
	// 任何缓存问题都回退直连网络克隆(永不让构建挂)。
	var repoCache *repocache.Cache
	if os.Getenv("PIPEWRIGHT_NO_REPO_CACHE") != "1" {
		repoDir := strings.TrimSpace(os.Getenv("PIPEWRIGHT_REPO_CACHE_DIR"))
		if repoDir == "" {
			repoDir = filepath.Join(filepath.Dir(cfg.DBPath), "repos")
		}
		if rc, rerr := repocache.New(repoDir, build.NewCloner()); rerr == nil {
			repoCache = rc
			log.Printf("[repocache] 代码管理区已启用(本地镜像+增量 fetch):%s", repoDir)
		} else {
			log.Printf("[repocache] 警告:代码管理区不可用(%v),构建退回每次直连克隆", rerr)
		}
	}

	// 构建依赖缓存(build cache · P0):script 类节点配了 cachePaths 时,执行前恢复依赖目录
	// (node_modules/.m2/.gradle 等)到工作区、执行后保存回缓存库,按 key(分支 + lockfile hash)
	// 寻址,跨 run 持久化提速。默认 DB 同级 cache/(PIPEWRIGHT_CACHE_DIR 可改);
	// PIPEWRIGHT_NO_BUILD_CACHE=1 关。缓存问题绝不让构建失败(恢复失败=冷构建,保存失败=记日志)。
	var cacheStore *buildcache.Store
	if os.Getenv("PIPEWRIGHT_NO_BUILD_CACHE") != "1" {
		cacheDir := strings.TrimSpace(os.Getenv("PIPEWRIGHT_CACHE_DIR"))
		if cacheDir == "" {
			cacheDir = filepath.Join(filepath.Dir(cfg.DBPath), "cache")
		}
		if cs, cerr := buildcache.New(cacheDir); cerr == nil {
			cacheStore = cs
			log.Printf("[buildcache] 构建依赖缓存已启用(配 cachePaths 的脚本节点跨 run 复用依赖):%s", cacheDir)
		} else {
			log.Printf("[buildcache] 警告:构建依赖缓存不可用(%v),脚本节点每次冷构建", cerr)
		}
	}

	// 把代码缓存(若启用)做成 Builder 选项;未启用 → 无操作选项(保持默认直连克隆器)。
	clonerOpt := func(*build.Builder) {}
	var refsLister httpapi.RefsLister
	if repoCache != nil {
		clonerOpt = build.WithCloner(repoCache)
		refsLister = repoCache
	}

	// 构建依赖缓存(若启用)做成 Builder 选项;未启用 → 无操作(避免 typed-nil 接口陷阱)。
	buildCacheOpt := func(*build.Builder) {}
	if cacheStore != nil {
		buildCacheOpt = build.WithBuildCache(cacheStore)
	}

	// 服务注册网关(nginx 版;提前到 deploy 构造前:instance_rolling 默认策略经适配器晚绑)。
	// 复用 targetSvc(SSH + docker)与 credVault(证书 SealSecret 密文);vault 未配 master key
	// 时证书上传不可用,其余功能照常(优雅降级)。集群反代渲染需要 serverID → 可达地址,
	// 一并注入解析器(target 查 SSH Host,主机名经控制端 DNS 解析为 IP)。
	// target 经 deploy.ObservingTarget 包装:网关编排的 SSH 命令(nginx 容器渲染/热加载等)在
	// 部署链路内回流到运行日志(「流水线日志可见所有执行的命令」;无日志 ctx = 纯透传)。
	serviceRegSvc := servicereg.New(st.DB, deploy.ObservingTarget(targetSvc), credVault)
	serviceRegSvc.SetServerAddrResolver(serverAddrResolver{tg: targetSvc}.resolve)

	// 系统级运行时配置(public_url = 平台对外访问地址):取代环境变量 PIPEWRIGHT_PUBLIC_URL,
	// 设置页在线修改、即时生效(通知审批链接 / PR 回写 target_url 每次触发时按需读取)。
	sysCfgSvc := systemcfg.New(st.DB)
	publicURLResolver := func(ctx context.Context) string { return systemcfg.Resolve(ctx, sysCfgSvc) }

	// 两步验证服务(TOTP + Passkey):TOTP 密钥 secretbox 加密落库——master key 已配置
	// 时用 vault 封存(与库隔离),否则自动生成本地兜底密钥(system_config.mfa_secret_key),
	// 无需任何环境变量即可启用。Passkey RP 配置读 systemcfg(留空自动推导)。
	var mfaSealer mfa.Sealer
	if masterKey != nil {
		mfaSealer = credVault
	}
	mfaSvc := mfa.NewService(st.DB, nil, mfaSealer, sysCfgSvc)
	mfaSvc.PurgeExpiredChallenges()
	authSvc.SetMFAChecker(mfaSvc)

	// 部署服务(提前到 dag 装配前构造,供 deploy_ssh 流水线节点注入)。Story 4.6 诊断钩子复用 7-2。
	// instance_rolling(默认策略)的网关摘/挂经 instanceRollGateway 适配器晚绑(deploy 不 import
	// servicereg,包间保持单向依赖);未装配时该策略自动回退既有滚动。
	deploySvc := deploy.New(targetSvc, runSvc, deploy.WithDiagnoseHook(httpapi.NewDiagnoseHook(runSvc, aiSvc, secretSrc)), deploy.WithArtifactStore(artStore), deploy.WithInstanceGateway(&instanceRollGateway{sr: serviceRegSvc}))

	// Ceph 集群部署与管理(cephadm 引导 + 池/RBD/CephFS 管理;命令经 target SSH 层)。
	cephSvc := ceph.New(st.DB, targetSvc, credVault)

	// ── 集群(对等节点 + EasyTier 自组网)装配 ─────────────────────────────────────
	// 全节点同构:mesh 服务、推送接收器、出站签名客户端、修订存储与镜像传输器一律构造
	// (端点是否可达/节点是否启用由运行时设置决定,不装配则集群功能整体缺席)。
	meshSvc := mesh.New(st.DB, credVault)
	pushedStore := pushsource.New(st.DB)
	meshClient := mesh.NewClient(meshSvc)
	// 跨环境服务转发:注入 mesh 依赖(peer 注册表 + 签名目录拉取)。对端未启用集群时
	// 目录拉取快速失败,转发服务渲染为 503 维护态。
	serviceRegSvc.SetMeshForward(
		func(ctx context.Context) ([]servicereg.ForwardPeer, error) {
			peers, perr := meshSvc.ListPeers(ctx)
			if perr != nil {
				return nil, perr
			}
			out := make([]servicereg.ForwardPeer, 0, len(peers))
			for _, p := range peers {
				out = append(out, servicereg.ForwardPeer{NodeID: p.NodeID, Name: p.Name, VirtualIP: p.VirtualIP})
			}
			return out, nil
		},
		func(ctx context.Context, nodeID string) (*mesh.ServiceCatalog, error) {
			if mst, merr := meshSvc.Settings(ctx); merr != nil || !mst.Enabled {
				return nil, fmt.Errorf("集群未启用")
			}
			p, perr := meshSvc.GetPeer(ctx, nodeID)
			if perr != nil {
				return nil, perr
			}
			return meshClient.FetchServiceCatalog(ctx, p)
		},
	)
	meshImageBinary := "docker"
	if drv, derr := build.DetectDriver(nil); derr == nil {
		meshImageBinary = drv.Binary()
	}
	meshImages := &build.ImageTransfer{Binary: meshImageBinary, BuiltinRegistry: registryHubSvc.ResolveBuiltin}
	meshReceiver := mesh.NewReceiver(projectSvc, artStore, meshImages, pushedStore)
	var meshOpts []build.BuilderOption
	meshOpts = append(meshOpts, build.WithMeshStage(meshSvc, meshClient, meshImages), build.WithPushedSource(pushedStore),
		// 集群共享构建发起网关(0086):含 peer: 选择器的构建节点经 mesh 派发到对端共享机。
		build.WithSharedBuildGateway(meshSvc, meshClient))

	// sharedBuildSrv 是集群共享构建的提供方运行时(DAG 构建器就绪后装配;legacy 运行器路径
	// 不构造 → 对端提交共享构建时本端 mesh 端点缺席,按目录不可达降级)。
	var sharedBuildSrv *build.SharedBuildServer

	// 对象存储集成层(流水线 OSS 上传节点配套):本期仅阿里云 OSS(AccessKeySecret 走 vault)。
	// 提前到 dag 装配前构造,供 oss_upload 流水线节点注入;同时供设置页 /api/object-stores 消费。
	objectStoreSvc := objectstore.New(st.DB, credVault)

	// 平台自身备份恢复(internal/backup):数据目录锚定 DB 同级(sqlite)/工作目录(mysql);
	// 对象存储上传复用 objectStoreSvc(定时备份配了 provider 才会自动上云)。CLI/在线恢复的
	// 启动期分派见 main 开头。定时备份 sweeper 在 ctx 就绪后启动(见下方)。
	backupOpts := backupRestoreOptions(dbDriver, dbDSN)
	backupSvc := backup.New(st.DB, backup.Options{
		Driver:   backupOpts.Driver,
		DataDir:  backupOpts.DataDir,
		DBPath:   backupOpts.DBPath,
		MySQLDSN: backupOpts.MySQLDSN,
		Uploader: objectStoreSvc,
	})

	// 阿里云 ECS 批量导入 + 开关机(设置→阿里云 ECS / 流水线 ecs_power 节点):多套账户配置
	// (AK/SK)+ 手选范围批量导入/更新 servers 行 + 流水线开关机(关机固定节省停机模式)。
	// ECS OpenAPI 客户端为包内手写(net/http + ACS3-HMAC-SHA256,无 SDK);落库/更新复用
	// targetSvc(校验语义与手工登记一致),Secret 封存复用 credVault。提前到 dag 装配前构造,
	// 供 ecs_power 流水线节点注入;同时供设置页 /api/aliyun/* 消费。
	aliyunSvc := aliyunecs.New(st.DB, credVault, targetSvc)

	// 数据库管理(MySQL/PostgreSQL 实例注册 + phpMyAdmin 式在线管理):连接密码走 vault 密文,
	// 实例默认只读;应用市场部署 mysql/postgres 模板成功后经此服务联动自动登记。
	dbAdminSvc := dbadmin.New(st.DB, credVault)

	// Redis 管理(实例注册 + SCAN 游标分页键浏览 + 删键/改 TTL):连接密码走 vault 密文,
	// 实例默认只读;应用市场部署 redis 模板成功后经此服务联动自动登记。
	redisAdminSvc := redisadmin.New(st.DB, credVault)

	// 已登记实例备份恢复(internal/dbbackup;功能长在 /databases 与 /redis 页的备份面板):
	// MySQL/PG 走协议(MySQL 全库 / PG 纳管实例 SSH 全保真、外部实例协议子集)、
	// Redis 走 SSH+docker exec;恢复前强制安全备份。定时备份 sweeper 在 ctx 就绪后启动。
	dbBackupSvc := dbbackup.New(st.DB, dbAdminSvc, redisAdminSvc, targetSvc, dbbackup.Options{
		DataDir:  backupOpts.DataDir,
		Uploader: objectStoreSvc,
	})

	// 安全中心(trivy-server 扫描器登记 + Trivy/Nuclei 扫描 + 定时规则 + HTML 审计报告):
	// 扫描经 target 层 SSH 在目标机 docker run 扫描器容器(无主动超时;重启后由
	// MarkStaleRuns 诚实标记中断);令牌走 vault 密文;完整报告 JSON 落制品库。应用市场
	// 部署 trivy-server 模板成功后经此服务联动自动登记。
	secScanOpts := []secscan.Option{secscan.WithVault(credVault), secscan.WithRunner(secscan.SSHRunner{Svc: targetSvc})}
	if artStore != nil {
		secScanOpts = append(secScanOpts, secscan.WithArtifacts(artStore))
	}
	// 目标机到不了 ghcr.io 时(典型:国内网络,本地缓存卷模式下载漏洞库失败),给全部
	// trivy 客户端注入镜像仓库;值如 ghcr.nju.edu.cn/aquasecurity/trivy-db(逗号分隔回退)。
	if dbRepo := os.Getenv("PIPEWRIGHT_TRIVY_DB_REPOSITORY"); dbRepo != "" {
		secScanOpts = append(secScanOpts, secscan.WithTrivyDBRepository(dbRepo))
		log.Printf("[security] Trivy 漏洞库仓库(客户端本地模式):%s", dbRepo)
	}
	// Java DB(镜像内 JAR 的检测索引)始终由客户端下载(即使走 --server),默认仓库
	// mirror.gcr.io 不可达时镜像扫到 JAR 即 FATAL;缺省按漏洞库仓库自动推导
	// (…/trivy-db → …/trivy-java-db),镜像源命名不标准时用此变量显式指定。
	if javaRepo := os.Getenv("PIPEWRIGHT_TRIVY_JAVA_DB_REPOSITORY"); javaRepo != "" {
		secScanOpts = append(secScanOpts, secscan.WithTrivyJavaDBRepository(javaRepo))
		log.Printf("[security] Trivy Java DB 仓库(客户端):%s", javaRepo)
	}
	secScanSvc := secscan.New(st.DB, secScanOpts...)
	if err := secScanSvc.MarkStaleRuns(context.Background()); err != nil {
		log.Printf("[security] 遗留扫描标记失败:%v", err)
	}

	// PVE 集群纳管与管理(服务器池 0084):PVE 物理机登记时探测自动建池;API Token 走
	// vault 密文;电源操作默认只读。需要 target 层(SSH 探测)与 vault。
	pveAdminSvc := pveadmin.New(st.DB, credVault, targetSvc)

	// 磁盘管理(块设备/LVM 拓扑 + 分区/格式化/挂载/扩缩容/LVM 变更):全部经 target 层
	// SSH argv 执行(root/免密 sudo 提权);操作无状态,解锁闸门为进程内 TTL,无需落库。
	diskAdminSvc := diskadmin.New(targetSvc)

	// 文件查看(目录浏览 + 文件下载 + ≤64KiB 纯文本在线查看/编辑):全部经 target 层
	// SSH argv 执行(提权可用则用,否则回退登录用户);编辑保存前服务端先同目录备份。
	fileAdminSvc := fileadmin.New(targetSvc)

	// 运行执行器选择(Epic 8):**默认走 DAG 调度执行器**——它是唯一真正按 UI 配置的
	// stages/script job/deploy_ssh/notify 编排执行的运行器。只有显式 PIPEWRIGHT_RUNNER=legacy
	// 才回退旧版固定流程(clone→对仓库根 docker build→deploy,无视可视化流水线)。
	// PIPEWRIGHT_RUNNER=dag 仍受支持(向后兼容);未设或任意非 legacy 值一律 = dag。
	// DAG 模式探测不到容器 CLI(docker)时优雅回退 stub(现有逻辑,NFR-10)。
	var runnerOpts []run.PoolOption
	// resumeSpecLoader 供 httpapi「失败运行按节点恢复」端点做配置一致性校验(与执行器同一 spec
	// 来源)。legacy 模式保持 nil → 恢复端点 503,与其不支持按节点恢复的语义一致。
	var resumeSpecLoader dagrun.SpecLoader
	// dagBuilder 提升 到分支外:磁盘维护的「构建产物镜像」手动清理需要 Builder 的本机
	// driver(枚举/删除镜像);legacy 分支的构建器在 buildRunnerOption 内部,不暴露(旧路径
	// 已弃用,手动镜像清理对 legacy 不生效,自动路径不受影响)。
	var dagBuilder *build.Builder
	// 产物镜像轮转依赖(磁盘治理,0094):每项目保留最近 N 个本地 tag(pipewright/<slug>:<commit>
	// 更旧的 rmi)。保留个数每轮构建现读 retention 配置(设置页可调,免重启);读不到配置时取 0
	// (宁可保留也不误删)。最近提交集来自 run 库的 trigger_commit。
	imageRotationOpt := build.WithImageRotation(build.ImageRotationConfig{
		KeepN: func(ctx context.Context) int {
			cfg, err := retention.NewService(st.DB).GetConfig(ctx)
			if err != nil {
				return 0
			}
			return cfg.ImageKeepTags
		},
		RecentCommits: runSvc.RecentCommitTags,
	})
	// 推送成功后回收构建机本机镜像(磁盘治理,0095):镜像已在 registry,构建机无需留存。
	// 默认回收;设置页可关(留副本复用构建层缓存)。每轮推送现读配置,免重启。
	imagePushCleanupOpt := build.WithImagePushCleanup(func(ctx context.Context) bool {
		cfg, err := retention.NewService(st.DB).GetConfig(ctx)
		if err != nil {
			return true // 读不到配置:回收(默认行为)
		}
		return cfg.ImageRemoveAfterPush
	})
	if strings.EqualFold(strings.TrimSpace(os.Getenv("PIPEWRIGHT_RUNNER")), "legacy") {
		runnerOpts = buildRunnerOption(projectSvc, pipelineSettingsSvc, credVault, artStore, repoCache, builtinRegistryOpt, imageRotationOpt, imagePushCleanupOpt)
		log.Printf("[run] PIPEWRIGHT_RUNNER=legacy:旧版固定流程运行器已启用(clone→对仓库根 docker build→deploy,⚠ 不执行 UI 可视化流水线 stages;如需真按流水线跑请去掉该 env)")
	} else {
		// 阶段执行体(Story 8-2):探测到容器 CLI → 注入真实阶段执行器(script 类型 job 在隔离
		// 容器内真实跑命令,复用 Builder 的 cloner+driver);否则回退 stub(优雅降级,NFR-10)。
		var dagOpts []dagrun.Option
		// 审批门 hook(Story 8-4):Gate 阶段阻塞等待人工批准/拒绝。进入等待态后 best-effort 发
		// 「需要审批」通知 + 签名审批链接(链接前缀 = 系统配置里的对外访问地址,未配则跳过通知,
		// 门行为不变;运行时可改)。
		approvalNotifier := httpapi.NewApprovalNotifier(notifySvc, approvalSigner, publicURLResolver, runSvc)
		dagOpts = append(dagOpts, dagrun.WithGate(httpapi.NewApprovalGate(runSvc, approvalCoord, approvalStore, approvalNotifier)))
		builderOpts := []build.BuilderOption{build.WithArtifactStore(artStore), build.WithArtifactLister(runSvc.ListArtifacts), build.WithImageGC(os.Getenv("PIPEWRIGHT_NO_IMAGE_GC") != "1"), build.WithCommitRecorder(func(ctx context.Context, runID, commit string, commitAt time.Time) {
			_ = runSvc.SetCommit(ctx, runID, commit, commitAt)
		}), build.WithStageDeployer(deploySvc), build.WithStageNotifier(notifySvc), build.WithStageObjectUploader(objectStoreSvc), build.WithEcsPowerController(aliyunSvc), build.WithAITester(aiSvc), build.WithAITestInfra(&aiTestInfraAdapter{svc: targetSvc}), build.WithWaitCoordinator(waitCoord), clonerOpt, buildCacheOpt, builtinRegistryOpt, imageRotationOpt, imagePushCleanupOpt}
		// 浏览器动作集(P2):控制机有容器 CLI 才注入浏览器桥编排;瘦控制机降级为纯 HTTP 验收。
		if aiCLI, aiCLIErr := build.DetectAITestContainerCLI(); aiCLIErr == nil {
			builderOpts = append(builderOpts, build.WithAITestContainer(aiCLI))
		}
		builderOpts = append(builderOpts, meshOpts...)
		b, berr := build.NewBuilder(projectSvc, pipelineSettingsSvc, credVault, builderOpts...)
		if berr == nil {
			dagBuilder = b // 供磁盘维护手动清理(控制机镜像轮转)使用
		}
		if berr != nil && errors.Is(berr, build.ErrNoContainerCLI) {
			// 瘦控制机(FR-8-19):本机无容器 CLI 不再整体回退 stub——改用 errDriver 构造 Builder,
			// 远程构建机池照常接线(克隆/打包/上传都在控制机,不依赖本机 CLI);未配选择器而回落
			// 本地执行的阶段将诚实报「无容器 CLI」,不假装成功。
			b, berr = build.NewBuilder(projectSvc, pipelineSettingsSvc, credVault, append(builderOpts, build.WithRemoteOnlyDriver())...)
		}
		if berr == nil {
			// 集群共享构建提供方运行时(0086):对端提交的共享构建任务进入本端构建机调度器
			// 同一 FIFO(排队权威在本端);工作区/产物只经 OS 临时文件中转,不进本端制品库。
			// 审计:受理/终态各留一条(接收端留痕,actor=mesh:<对端名>,同 run_trigger_mesh 先例)。
			sharedBuildSrv = build.NewSharedBuildServer(b, runnerSched, targetSvc, targetSvc)
			sharedBuildSrv.OnEvent = func(action string, st mesh.SharedJobStatus) {
				actor := "mesh:peer"
				if st.Peer != "" {
					actor = "mesh:" + st.Peer
				}
				detail := map[string]any{"jobId": st.JobID, "peerRun": st.RunID, "state": st.State}
				if st.ServerName != "" {
					detail["machine"] = st.ServerName
				}
				if st.Error != "" {
					detail["error"] = st.Error
				}
				_ = auditRec.Record(context.Background(), audit.Entry{
					Actor:      actor,
					Action:     "shared_build_" + action,
					TargetType: audit.TargetServer,
					TargetID:   st.ServerID,
					Detail:     detail,
				})
			}
			// runSvc 作测试报告持久层注入(Story 8-6 / FR-8-6):script 步骤产报告 → 解析 →
			// 落库 → 质量门禁裁决(不过则阶段失败,阻断下游部署)。
			dagOpts = append(dagOpts, dagrun.WithStageExecutor(build.NewStageExecutorWithRunner(b, runSvc, runnerPool{cfg: runnerSvc, sched: runnerSched}, targetSvc)))
			if b.DriverBinary() == "none" {
				log.Printf("[run] DAG 调度执行器已启用(默认;阶段按 needs 编排,真按 UI 可视化流水线 stages 执行;⚠ 本机未探测到容器 CLI:瘦控制机模式,仅配了构建机选择器的项目/stage 可远程执行,本地执行将报「无容器 CLI」;PIPEWRIGHT_RUNNER=legacy 可回退旧版固定流程)")
			} else {
				log.Printf("[run] DAG 调度执行器已启用(默认;阶段按 needs 编排,真按 UI 可视化流水线 stages 执行;script 类型 job 在隔离容器真实执行,CLI=%s;PIPEWRIGHT_RUNNER=legacy 可回退旧版固定流程)", b.DriverBinary())
			}
		} else {
			log.Printf("[run] DAG 调度执行器已启用(默认;阶段按 needs 编排,真按 UI 可视化流水线 stages 执行;⚠ 构建器初始化失败,阶段执行体回退 stub:%v;PIPEWRIGHT_RUNNER=legacy 可回退旧版固定流程)", berr)
		}
		// 「流水线即代码」运行时覆盖(FR-8-12):装饰器包住库内 loader——运行时若项目**开启了
		// 流水线即代码**(每项目开关 projects.pac_enabled)且仓库根含合法 `.pipewright.yml`,即用它
		// 驱动本次运行,否则一律回退库内配置(任何 YAML/拉取问题都不中断运行)。SpecLoader 现已带运行
		// 分支,故按**本次运行分支**拉取(空分支退化默认分支);库内 pipeline.Service(分支无关)经
		// dagrun.SpecLoaderFunc 适配进 branch 感知契约,其 2 参公有签名不变(其它调用方不受影响)。
		//
		// 装饰器始终挂载(每项目开关默认关 → 老项目行为不变,无需服务端 env)。历史全局开关
		// PIPEWRIGHT_PAC_RUNTIME=1 保留为**全局强开**:无视每项目开关,对所有项目尝试覆盖。
		pacGlobalOverride := strings.EqualFold(strings.TrimSpace(os.Getenv("PIPEWRIGHT_PAC_RUNTIME")), "1")
		var specLoader dagrun.SpecLoader = pacSpecLoader{pacloader.New(
			pipelineSvc,
			pacProjectLookup{projectSvc},
			pacGitAuthResolver{vault: credVault},
			pacBlobFetcher{httpapi.NewSourceReader()},
			pacGlobalOverride,
		)}
		if pacGlobalOverride {
			log.Printf("[pac] 全局强开:.pipewright.yml 覆盖对所有项目生效(PIPEWRIGHT_PAC_RUNTIME=1;无视每项目开关)")
		}
		resumeSpecLoader = specLoader
		runnerOpts = []run.PoolOption{run.WithRunner(dagrun.New(specLoader, dagOpts...))}
	}

	// DNS 提供商集成层(R3 E3.1–E3.4):Cloudflare / DNSPod / 阿里云 DNS 接入(凭据走 vault)。
	// 复用已装配的 targetSvc(SSH 执行/上传)+ st.DB(参数化 SQL),无新顶层依赖、无 init 副作用。
	// 与证书管理互引经适配器在装配后晚绑,无 init 副作用、无新顶层依赖。
	dnsSvc := dnsprovider.New(st.DB, credVault)

	// 证书管理(acme.sh 自动签发/续期,DNS-01):凭据/根区复用 dnsSvc;签发引擎以内嵌脚本
	// 跑在控制机本地(DB 同级 acme/,不依赖网关配置);签发结果读回入库后经 CertSink 同步
	// 基域(内部 apply + reload),并联动平台 HTTPS。
	certSvc := certmgmt.New(st.DB, targetSvc, credVault)
	if cfg, ok := certSvc.(interface {
		SetCredentialsResolver(certmgmt.CredentialsResolver)
	}); ok {
		cfg.SetCredentialsResolver(&dnsResolverAdapter{dns: dnsSvc})
	}
	if h, ok := certSvc.(interface{ SetHomeDir(string) }); ok {
		h.SetHomeDir(filepath.Join(filepath.Dir(cfg.DBPath), "acme"))
	}
	if cfg, ok := certSvc.(interface{ SetCertSink(certmgmt.CertSink) }); ok {
		cfg.SetCertSink(&certSinkAdapter{sr: serviceRegSvc})
	}
	// 新建基域 → 自动拉取既有覆盖证书:证书推送只发生在证书事件时,后建的基域拿不到
	// 已有证书(否则要手动刷新一次)。钩子在 CreateDomain 后 best-effort 反向拉取。
	{
		sr := serviceRegSvc
		if cs, ok2 := certSvc.(interface {
			SyncAllToSink(context.Context) error
		}); ok2 {
			sr.SetCertSyncHook(cs.SyncAllToSink)
		}
	}
	// servicereg 存量孤儿证书一次性回填进证书管理页(已被库内证书覆盖的基域跳过;
	// 历史版本误搬的冻结副本顺带清理。幂等;失败仅记日志)。
	if n, berr := certSvc.BackfillFromServiceReg(context.Background()); berr != nil {
		log.Printf("[certmgmt] 存量证书回填失败(不影响启动):%v", berr)
	} else if n > 0 {
		log.Printf("[certmgmt] 已回填 %d 张服务注册基域存量证书", n)
	}

	// 平台 HTTPS 访问(宿主 nginx):当平台自身所在主机装有宿主 nginx 时,自动把平台 Web 页面
	// 发布为 HTTPS —— 下发证书管理模块的证书 + 写 conf.d vhost(443 ssl 反代平台 Web 端口 +
	// 80→443 跳转)+ nginx -t + reload,全部在本机执行(nil → os/exec 默认 Host)。
	// 证书经 CertSource 软引用(PEM 仅进程内解密传递);续期/重下发后经 PlatformHTTPS 联动
	// 重新落盘,删除占用证书被拦截。
	platformHTTPSSvc := platformhttps.New(st.DB, nil, &platformCertSource{cm: certSvc}, defaultPortFromAddr(cfg.Addr))
	registryCertSrc.set(certSvc) // 内置 registry 的 TLS 证书来源此时才可用(晚绑接上)
	if cfg2, ok := certSvc.(interface{ SetPlatformHTTPS(certmgmt.PlatformHTTPS) }); ok {
		cfg2.SetPlatformHTTPS(&multiPlatformHTTPS{parts: []certmgmt.PlatformHTTPS{
			&platformHTTPSAdapter{ph: platformHTTPSSvc},
			&registryHubAdapter{h: registryHubSvc}, // registry 栈引用的证书续期后重部署栈
		}})
	}

	// 证书部署目标(远程机器指定路径 + 部署后命令):证书签发/导入/续期后自动联动重新
	// 部署,删除被引用证书时占用拦截。执行面复用 target.Service(SSH Exec/Upload),
	// 证书 PEM 经 platformCertSource 进程内解密(同一适配器,晚绑 certSvc)。
	// safeline 类型目标经安全中心的 WAF 实例解析接入面(见 secscan.safeline.go)。
	certDeploySvc := certdeploy.New(st.DB, targetSvc, &platformCertSource{cm: certSvc}).WithLogger(log.Printf).WithWafGateway(secScanSvc)
	if cfg3, ok := certSvc.(interface{ SetCertDeploys(certmgmt.PlatformHTTPS) }); ok {
		cfg3.SetCertDeploys(certDeploySvc)
	}

	// 应用商店(DPanel 式一键部署):模板 CRUD + 内置 seed(幂等;失败仅记日志,不阻断启动)。
	appStoreSvc := appstore.New(st.DB)
	if err := appStoreSvc.EnsureBuiltins(context.Background()); err != nil {
		log.Printf("[appstore] 警告:内置模板 seed 失败(不影响启动,可重启重试): %v", err)
	}

	// 终态钩子:通知(Story 5.2)+「PR 状态回写」(Story 8-9 / FR-8-9):run 终态 → 据项目仓库
	// 识别 GitHub/Gitee → 经项目凭据回写该 commit 的提交状态(PR 检查)。
	//
	// PR 回写钩子**始终挂载**,但仅对**开启了 PR 状态检查**的项目(每项目开关 projects.pr_status_enabled)
	// 生效——老项目默认关 → 行为不变(不会意外往真实仓库回写),且无需服务端 env 即可逐项目启用。
	// 历史全局开关 PIPEWRIGHT_PR_STATUS=1 保留为**全局强开**:无视每项目开关,对所有项目回写(向后兼容)。
	// API base 可经 env 覆盖(GitHub/Gitee 企业版自托管;空则用公有云默认),无论是否全局强开均生效。
	// best-effort:失败仅记日志,不阻断 run 终态。
	terminalHook := httpapi.NewNotifyHook(runSvc, notifySvc, secretSrc)
	prStatusGlobalOverride := strings.EqualFold(strings.TrimSpace(os.Getenv("PIPEWRIGHT_PR_STATUS")), "1")
	reporter := prstatus.NewReporter(&http.Client{Timeout: 10 * time.Second}).WithBaseURLs(
		strings.TrimSpace(os.Getenv("PIPEWRIGHT_PR_STATUS_GITHUB_BASE")),
		strings.TrimSpace(os.Getenv("PIPEWRIGHT_PR_STATUS_GITEE_BASE")))
	prHook := httpapi.NewPRStatusHook(runSvc, projectSvc, credVault, reporter,
		publicURLResolver, prStatusGlobalOverride)
	notify := terminalHook
	terminalHook = func(ctx context.Context, runID, finalStatus string) {
		notify(ctx, runID, finalStatus)
		prHook(ctx, runID, finalStatus)
	}
	if prStatusGlobalOverride {
		log.Printf("[prstatus] 全局强开:PR 状态回写对所有项目生效(PIPEWRIGHT_PR_STATUS=1;无视每项目开关)")
	}

	// 流水线串联终态钩子(FR-8-11):复用同一终态槽,叠加到 terminalHook 之上。
	// run 落 success → 据上游项目串联配置创建下游运行(TriggerChain);环路安全(深度门 + 路径门)
	// 在 chain.NewHook 内。深度上限经 PIPEWRIGHT_CHAIN_MAX_DEPTH 覆盖(默认 5);best-effort,
	// 失败仅记日志,绝不阻断 run 终态。
	chainMaxDepth := chain.DefaultMaxDepth
	if v := strings.TrimSpace(os.Getenv("PIPEWRIGHT_CHAIN_MAX_DEPTH")); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			chainMaxDepth = n
		} else {
			log.Printf("[chain] 警告:PIPEWRIGHT_CHAIN_MAX_DEPTH=%q 非法(须为正整数),用默认 %d", v, chain.DefaultMaxDepth)
		}
	}
	chainHook := chain.NewHook(chainSvc, runSvc, chainMaxDepth)
	priorTerminal := terminalHook
	terminalHook = func(ctx context.Context, runID, finalStatus string) {
		priorTerminal(ctx, runID, finalStatus)
		chainHook(ctx, runID, finalStatus)
	}

	// 并发/队列控制(FR-8-10):全局上限经 PIPEWRIGHT_MAX_CONCURRENT 设置(默认 = worker 数);
	// 项目级上限经 concurrencySvc(/api/projects/{id}/concurrency 配置)。突发超限的运行保持 queued
	// 排队,待槽位释放再调度(FIFO)。非法/未设 env → 0,pool 回落到默认行为(仅受全局上限)。
	maxConcurrent := 0
	if v := strings.TrimSpace(os.Getenv("PIPEWRIGHT_MAX_CONCURRENT")); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			maxConcurrent = n
		} else {
			log.Printf("[run] 警告:PIPEWRIGHT_MAX_CONCURRENT=%q 非法(须为正整数),忽略", v)
		}
	}

	pool := run.NewWorkerPool(runSvc,
		append(runnerOpts,
			run.WithDiagnoseHook(httpapi.NewDiagnoseHook(runSvc, aiSvc, secretSrc)),
			// per-run 日志脱敏:每个 run 构建登记了该 run 真实凭据的 Masker(红线修)。
			run.WithLogMaskerFunc(secretSrc.MaskerForRun),
			// best-effort 终态钩子(通知 + 可选 PR 状态回写)。未配路由的事件不发送;绝不阻断 run 终态。
			run.WithNotifyHook(terminalHook),
			// 并发/队列控制(FR-8-10):全局上限 + 项目级上限准入。
			run.WithMaxConcurrent(maxConcurrent),
			run.WithConcurrencyLimits(concurrencySvc),
		)...,
	)
	pool.Start()
	log.Printf("[run] worker pool started")

	// 集群运行时与 HTTP 依赖(在 pool 之后:meshDeps 需要 runSub/pool 做 SSE 事件回流)。
	// 运行时循环的启动在 ctx(signal)就绪后(见下方 meshRT.start 调用)。
	meshDataDir := filepath.Dir(cfg.DBPath)
	meshCorePath := easytier.Locate(os.Getenv("PIPEWRIGHT_EASYTIER_BIN"), meshDataDir, easytier.CoreBinary())
	meshCLIPath := easytier.Locate(os.Getenv("PIPEWRIGHT_EASYTIER_CLI"), meshDataDir, easytier.CliBinary())
	// EasyTier 二进制自动下载器:升级源复用平台镜像机制(设置库 release_mirror >
	// env PIPEWRIGHT_RELEASE_MIRROR > GitHub 官方),仅换 Repo 为 EasyTier/EasyTier;
	// 资产 sha256 digest 硬校验。托管模式启用而二进制缺失时由运行时自动触发。
	meshDownloader := &easytier.Downloader{
		DataDir: meshDataDir,
		Source: func(ctx context.Context) version.Source {
			base := version.ResolveSource(systemcfg.ResolveReleaseMirror(ctx, sysCfgSvc))
			return version.Source{Repo: "EasyTier/EasyTier", APIBase: base.APIBase, WebBase: base.WebBase, DLBase: base.DLBase}
		},
	}
	meshAdvertisePort := defaultPortFromAddr(cfg.Addr)
	if v := strings.TrimSpace(os.Getenv("PIPEWRIGHT_MESH_API_PORT")); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			meshAdvertisePort = n
		}
	}
	meshRT := newMeshRuntime(meshSvc, meshClient, meshCorePath, meshCLIPath, meshDataDir, meshDownloader)
	// 跨环境转发数据面:①本端网关 IP 以 /32 通告进 mesh(存在可转发服务时;变化经设置
	// 指纹重启 easytier);②发现循环节拍收尾对账网关机 /32 路由 + ip_forward 保活。
	meshRT.SetProxyNetworks(serviceRegSvc.GatewayProxyNetworks)
	meshRT.SetAfterDiscover(serviceRegSvc.ReconcileForwardRoutes)
	// 集群 DNS 映射:指定域名 A 记录自动指向本机虚拟 IP(同步循环随 ctx 启停;手动
	// 「立即同步」经 /api/mesh/dns-records/sync)。虚拟 IP 运行态 SelfIP 优先(兼容 DHCP;
	// 托管/外部模式皆可),EasyTier 未就绪时回退集群设置的手填值。
	meshDNSSvc := meshdns.New(st.DB, dnsSvc)
	meshDNSSyncer := meshdns.NewSyncer(meshDNSSvc, dnsSvc,
		func(ctx context.Context) string {
			if snap := meshRT.EasyTierStatus(ctx); snap != nil && snap.SelfIP != "" {
				return snap.SelfIP
			}
			if mst, err := meshSvc.Settings(ctx); err == nil {
				return mst.VirtualIP
			}
			return ""
		},
		func(ctx context.Context) bool {
			mst, err := meshSvc.Settings(ctx)
			return err == nil && mst.Enabled
		},
	)
	meshAdminDeps := httpapi.NewMeshDeps(meshSvc, meshReceiver, meshClient, projectSvc, runSvc, pool, parameterSvc, targetSvc, pipelineSvc, auditRec, pushedStore, webhookReceiver, meshAdvertisePort).
		WithRuntime(meshRT).WithServiceCatalog(serviceRegSvc).WithSharedBuild(sharedBuildSrv).WithMeshDNS(meshDNSSvc, meshDNSSyncer)

	// 集群防火墙访问限制(两步生效):apply 把平台本机端口(默认 80/443/8080/5000/5001,
	// 8080 取实际监听口)临时限制为仅本机 + mesh 网段可访问;从本机/mesh 访问后 confirm
	// 固化,此后 KeepAlive 跨重启幂等断言。SelfIP 从 mesh 运行时取(拿不到且无补充网段时
	// apply 拒绝,宁可不应用不瞎猜网段)。
	firewallSvc := firewall.New(st.DB, firewall.Options{
		DefaultPorts: []int{80, 443, 5000, 5001, defaultPortFromAddr(cfg.Addr)},
		SelfIP: func() string {
			if snap := meshRT.EasyTierStatus(context.Background()); snap != nil {
				return snap.SelfIP
			}
			return ""
		},
	})

	// webhook 集群转发:公网收到本地不存在的 token 时,投递经 mesh 转发给归属环境
	// (对端按 trigger_allowed 授权;mesh 未启用时转发器自动降级为纯本地)。
	webhookForwarder := httpapi.NewWebhookForwarder(meshSvc, meshClient)

	// 产物跨环境传输(产物管理页手动触发):复用 mesh_push 推送通道,对端经 /api/mesh/push
	// 接收并自动建「被推送源」项目落修订(接收侧零改动);终态留审计。
	artifactTransfers := build.NewArtifactTransferService(meshSvc, meshClient, meshImages, artStore, runSvc)
	artifactTransfers.OnSettled(func(st *build.TransferStatus) {
		detail := map[string]any{
			"runId": st.RunID, "peer": st.PeerName, "peerNode": st.PeerNodeID,
			"items": st.Items, "status": st.Status,
		}
		if st.Result != nil {
			detail["peerProjectId"] = st.Result.ProjectID
			detail["revisionId"] = st.Result.RevisionID
		}
		if st.Error != "" {
			detail["error"] = st.Error
		}
		_ = auditRec.Record(context.Background(), audit.Entry{
			Actor:      "admin",
			Action:     "runs.artifact.transfer",
			TargetType: "run_artifact",
			TargetID:   st.RunID,
			Detail:     detail,
		})
	})

	// 装配并启动定时(cron)调度器(Story 8-6):分钟粒度扫描启用的项目 cron,到点经 run.Service
	// 创建「定时」触发的运行(入 pool 调度)。同一项目同一分钟去重;表达式非法的项目跳过。
	// 无启用配置时空转无副作用。优雅停机随 worker pool 停。
	cronScheduler := cron.NewScheduler(cronSvc, httpapi.NewScheduledRunCreator(runSvc))
	cronScheduler.Start(context.Background())
	log.Printf("[cron] 定时调度器已启动")

	// 运行数据保留清理器:按 retention_config 策略定期裁剪过期终态运行(连带其日志/步骤/产物),
	// 防止无限增长撑爆磁盘。默认策略 enabled=0(不删);用户在设置里开启并配条数/天数后生效。
	retentionSvc := retention.NewService(st.DB)
	// 制品孤儿 GC:run 过期删除后,artifacts/ 磁盘 blob 会残留成孤儿(内容寻址去重防不了累积);
	// 挂到保留清理器上同周期回收(无引用且超过 24h 宽限期的 blob 才删,防误删进行中构建)。
	if artStore != nil {
		retentionSvc.WithBlobStore(retention.NewBlobStore(artStore.Root()))
	}
	// 磁盘治理任务随保留清理器同周期(每小时,各任务独立成败):构建缓存 LRU GC(独立于 enabled
	// 开关——缓存不是审计数据;上限来自 retention_config.cache_max_gb,默认 10GB)+ 无引用仓库
	// 镜像回收(兜底项目删除遗漏与历史遗留)。
	maintSvc.WithBuildCache(cacheStore).WithRepoCache(repoCache).WithTargets(targetSvc)
	if dagBuilder != nil {
		maintSvc.WithLocalImageRotation(dagBuilder.RotateImagesWithKeep)
		maintSvc.WithLocalExtraClean(dagBuilder.ExtraImageClean)
	}
	retentionSweeper := retention.NewSweeper(retentionSvc, time.Hour).WithGC(retentionSvc).
		WithTask("buildcache-gc", func(ctx context.Context) error {
			maintenance.LogResult(maintSvc.CleanBuildcache(ctx))
			return nil
		}).
		WithTask("repocache-prune", func(ctx context.Context) error {
			maintenance.LogResult(maintSvc.CleanRepocache(ctx))
			return nil
		}).
		WithTask("security-scan-retention", func(ctx context.Context) error {
			// 安全扫描历史保留 30 天(明细 artifact 文件不回收,属可接受占用)。
			if n, err := secScanSvc.Sweep(ctx, time.Now().Add(-30*24*time.Hour)); err == nil && n > 0 {
				log.Printf("[security] 已清理 %d 条过期扫描记录", n)
			}
			return nil
		})
	retentionSweeper.Start(context.Background())
	log.Printf("[retention] 保留清理器已启动(每小时一扫;策略默认关,需在设置开启;磁盘治理任务除外——缓存/镜像回收默认生效)")

	// 项目删除后的仓库镜像即时回收(磁盘治理):项目行删除后若无其它项目共用同一 repo_url,
	// 即删本地镜像(多项目同 URL 只删最后一个删的)。best-effort 只记日志;残余由每小时
	// repocache-prune 兜底。
	projectMirrorCleaner := func(ctx context.Context, repoURL string) {
		if repoCache == nil || strings.TrimSpace(repoURL) == "" {
			return
		}
		var n int
		if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM projects WHERE repo_url = ?`, repoURL).Scan(&n); err != nil || n > 0 {
			return
		}
		if err := repoCache.Remove(repoURL); err != nil {
			log.Printf("[maintenance] 删除项目仓库镜像失败(best-effort,待定时兜底):%v", err)
		}
	}

	// 内置 registry 镜像保留清理器:按 registry_hub_config 策略(每仓库保留最近 N tag / 超龄删除)
	// 每日裁剪制品 registry 并触发 gc。默认策略不限(不删);需在「设置 → 镜像仓库」配置后生效。
	registryHubSweeper := registryhub.NewSweeper(registryHubSvc, 24*time.Hour)
	registryHubSweeper.Start(context.Background())
	log.Printf("[registryhub] 镜像保留清理器已启动(每日一扫;策略默认不限,需在设置配置)")

	// 证书自动续期调度器:按固定间隔扫描证书,对「ACME + 自动续期 + 到期 <30 天 + 非进行中 +
	// 距上次尝试 >24h」的证书触发续期(acme.sh --renew --force,在网关主机上执行;每次续期在
	// 证书管理页可见)。间隔经 PIPEWRIGHT_CERT_SWEEP_INTERVAL 覆盖(Go duration;默认 1h;0 关闭)。
	certSweepInterval := time.Hour
	if v := strings.TrimSpace(os.Getenv("PIPEWRIGHT_CERT_SWEEP_INTERVAL")); v != "" {
		if d, perr := time.ParseDuration(v); perr == nil && d > 0 {
			certSweepInterval = d
		} else if perr == nil && d == 0 {
			log.Printf("[certmgmt] 证书自动续期调度已关闭(PIPEWRIGHT_CERT_SWEEP_INTERVAL=0)")
			certSweepInterval = 0
		} else {
			log.Printf("[certmgmt] 警告:PIPEWRIGHT_CERT_SWEEP_INTERVAL=%q 非法(须为 Go duration),用默认 %s", v, certSweepInterval)
		}
	}
	if certSweepInterval > 0 {
		certSweeper := certmgmt.NewSweeper(certSvc, certSweepInterval)
		certSweeper.Start(context.Background())
		log.Printf("[certmgmt] 证书自动续期调度器已启动(每 %s 一扫)", certSweepInterval)
	}

	// Per-PR 预览环境自动回收器(R4 E4.1 收尾):按固定间隔遍历 active 预览环境,查其 PR 是否
	// 装配只读源码读取器(Story 3.6;FR-4 预埋):go-git 浅克隆读 tree/blob;严格 SSRF 收口;
	// 克隆失败优雅降级(不致命)。无 init 副作用/不驻留。
	sourceReader := httpapi.NewSourceReader()

	// 装配目标服务器服务(Story 4.1;internal/target,FR-14):通用 SSH exec/session 基座,
	// 供 Epic 4 部署 + Epic 6 运维共享。SSH 私钥/口令经 vault 即用即弃,绝不入库/日志/响应/错误。
	// dialer 传 nil → 使用默认 x/crypto/ssh 实现(无宿主依赖,不驻留)。master key 未配置时
	// Exec/Test 降级为明确错误(不 panic)。

	// 装配部署执行服务(Story 4.2;internal/deploy,FR-10):把成功运行的产物经 SSH 部署到目标机。
	// 部署命令 array 化经 targetSvc.Exec 执行(AC-SEC-02,不拼 shell);每机结果落 deploy_targets,
	// 据结果更新 run 终态(success/partial_failed/failed)。复用已装配的 targetSvc + runSvc,无新顶层依赖。
	// (notifySvc 已在 pool 装配前声明〔Story 5.2 注入 NotifyHook〕,此处不重复。)
	// Story 4.6(FR-22 种子):注入诊断钩子 → 部署失败(failed/partial_failed)合成失败日志 +
	// best-effort 触发 7-2 AI 诊断,让诊断飞轮覆盖部署失败(而非只覆盖构建失败)。复用 7-2 NewDiagnoseHook。
	// (deploySvc 已提前在 dag 装配前构造,供 deploy_ssh 流水线节点注入;此处不重复。)

	// 装配可配置异常检测服务(Story 6.5;FR-23):按阈值规则对服务器指标做检测,命中产告警入库。
	// 检测复用 6-1 指标采集(NewAnomalyCollector 适配 collectServerMetrics);不可达/指标 null 的
	// 服务器跳过(不误报)。复用已装配的 targetSvc(采集),无新顶层依赖;无 init 副作用/不起轮询。
	// 告警去重窗口(同「服务器×规则条件」最小间隔):env PIPEWRIGHT_ANOMALY_COOLDOWN(秒)覆盖,默认 600s。
	anomalyCooldown := envDurationSeconds("PIPEWRIGHT_ANOMALY_COOLDOWN", 600*time.Second)
	// 定时检测间隔:env PIPEWRIGHT_ANOMALY_INTERVAL(秒)覆盖,默认 60s;置 0 关闭定时(仅手动)。
	// 在此提前算出(httpapi 经 WithAnomalyConfig 暴露给前端显示节奏 + 下方定时器复用)。
	anomalyInterval := envDurationSeconds("PIPEWRIGHT_ANOMALY_INTERVAL", 60*time.Second)
	anomalySvc := anomaly.New(st.DB, httpapi.NewAnomalyCollector(targetSvc),
		anomaly.WithNotifier(httpapi.NewAnomalyNotifier(notifySvc)),
		anomaly.WithCooldown(anomalyCooldown),
	)

	// 服务器指标时序历史(异常检测「看趋势」折线图数据源):后台采样器复用 6-1 采集周期性写样本,
	// 按保留窗口清理。采样间隔 env PIPEWRIGHT_METRICS_SAMPLE_INTERVAL(秒,默认 60;0 关闭采样),
	// 保留天数 env PIPEWRIGHT_METRICS_RETENTION_DAYS(默认 7)。
	metricsHist := metrics.New(st.DB)
	metricsSampleInterval := envDurationSeconds("PIPEWRIGHT_METRICS_SAMPLE_INTERVAL", 60*time.Second)
	metricsRetentionDays := 7
	if v := strings.TrimSpace(os.Getenv("PIPEWRIGHT_METRICS_RETENTION_DAYS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			metricsRetentionDays = n
		}
	}
	metricsSampleCollector := httpapi.NewAnomalyCollector(targetSvc)

	// 机器监控(自维护 fork MyLinkPi/ssar):安装包下载器 + 领域服务。升级源复用平台
	// 镜像机制(同 EasyTier 下载器),仅换 Repo;env PIPEWRIGHT_SSAR_VERSION 可钉死
	// 具体 release tag(默认跟随最新)。采样节奏在下方 ticker(独立于 6-1 指标采样)。
	ssarDownloader := ssar.NewDownloader(func(ctx context.Context) version.Source {
		base := version.ResolveSource(systemcfg.ResolveReleaseMirror(ctx, sysCfgSvc))
		return version.Source{Repo: ssar.Repo, APIBase: base.APIBase, WebBase: base.WebBase, DLBase: base.DLBase}
	}, meshDataDir, os.Getenv("PIPEWRIGHT_SSAR_VERSION"))
	ssarSvc := ssar.New(st.DB, targetSvc, ssarDownloader)
	// 采样间隔 env PIPEWRIGHT_SSAR_SAMPLE_INTERVAL(秒,默认 60,与机器侧 sresar 快照
	// 同拍;0 关闭),保留天数 env PIPEWRIGHT_SSAR_RETENTION_DAYS(默认 7,与 ssar 本地
	// 保留窗口对齐)。
	ssarSampleInterval := envDurationSeconds("PIPEWRIGHT_SSAR_SAMPLE_INTERVAL", 60*time.Second)
	ssarRetentionDays := 7
	if v := strings.TrimSpace(os.Getenv("PIPEWRIGHT_SSAR_RETENTION_DAYS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			ssarRetentionDays = n
		}
	}

	// 装配多 provider OAuth 凭据接入服务(连接 Gitee/GitHub/GitLab/自建):复用 vault secretbox 加密
	// OAuth 应用 client_secret(密文入库);OAuth 拿到的 access_token 经 vault.Create 存成 git_token 凭据,
	// 之后克隆/构建直接复用(无需改克隆逻辑)。Exchange 兑换用带超时的注入 HTTP 客户端(~10s);
	// state CSRF 短期内存绑会话。master key 未配置时涉及密钥的操作降级为明确错误(不 panic)。
	oauthSvc := oauth.New(st.DB, credVault, &http.Client{Timeout: 10 * time.Second})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpapi.New(webFS, authSvc, httpapi.WithVault(credVault), httpapi.WithProjects(projectSvc), httpapi.WithTriggers(triggerSvc), httpapi.WithPipelines(pipelineSvc), httpapi.WithPipelineSettings(pipelineSettingsSvc), httpapi.WithRuns(runSvc, pool), httpapi.WithWebhooks(webhookReceiver), httpapi.WithWebhookForwarder(webhookForwarder), httpapi.WithAudit(auditRec), httpapi.WithAccount(authSvc), httpapi.WithIdentity(identitySvc), httpapi.WithAISettings(aiSvc), httpapi.WithAIGenerate(repoAnalyzer), httpapi.WithRunDiff(runDiffer), httpapi.WithSource(sourceReader), httpapi.WithRefs(refsLister), httpapi.WithArtifactStore(artStore), httpapi.WithArtifactImageTransfer(meshImages), httpapi.WithArtifactTransfer(artifactTransfers), httpapi.WithServers(targetSvc), httpapi.WithLabels(labelsSvc), httpapi.WithServerCommands(serverCmdSvc), httpapi.WithSSHTunnels(sshtunnelSvc), httpapi.WithRegistryHub(registryHubSvc), httpapi.WithRunnerConfig(runnerSvc), httpapi.WithDeploy(deploySvc), httpapi.WithSpecLoader(resumeSpecLoader), httpapi.WithNotifications(notifySvc), httpapi.WithRetention(retentionSvc), httpapi.WithDNSProviders(dnsSvc), httpapi.WithAliyunEcs(aliyunSvc), httpapi.WithObjectStores(objectStoreSvc), httpapi.WithBackup(backupSvc), httpapi.WithCertMgmt(certSvc), httpapi.WithCertDeploys(certDeploySvc), httpapi.WithPlatformHTTPS(platformHTTPSSvc), httpapi.WithFirewall(firewallSvc), httpapi.WithSystemConfig(sysCfgSvc), httpapi.WithMFA(mfaSvc), httpapi.WithServiceReg(serviceRegSvc), httpapi.WithAppStore(appStoreSvc), httpapi.WithDBAdmin(dbAdminSvc), httpapi.WithRedisAdmin(redisAdminSvc), httpapi.WithDBBackup(dbBackupSvc), httpapi.WithSecScan(secScanSvc), httpapi.WithPVEAdmin(pveAdminSvc), httpapi.WithDiagnosisFeedback(feedbackSvc), httpapi.WithAnomaly(anomalySvc), httpapi.WithAnomalyConfig(int(anomalyInterval.Seconds()), int(anomalyCooldown.Seconds())), httpapi.WithMetricsHistory(metricsHist), httpapi.WithSsar(ssarSvc), httpapi.WithSecretSource(secretSrc), httpapi.WithOAuth(oauthSvc), httpapi.WithCron(cronSvc), httpapi.WithChain(chainSvc), httpapi.WithApprovals(approvalCoord, approvalStore), httpapi.WithApprovalLinks(approvalSigner), httpapi.WithWaitNode(waitCoord), httpapi.WithConcurrency(concurrencySvc), httpapi.WithParameters(parameterSvc), httpapi.WithDoraMetrics(doraMetricsSvc), httpapi.WithTemplates(templateSvc), httpapi.WithVariableGroups(varGroupSvc), httpapi.WithCustomNodes(customNodeSvc), httpapi.WithOpsChat(opsChatSvc), httpapi.WithMesh(meshAdminDeps), httpapi.WithCeph(cephSvc), httpapi.WithDiskAdmin(diskAdminSvc), httpapi.WithFileAdmin(fileAdminSvc), httpapi.WithMaintenance(maintSvc), httpapi.WithProjectMirrorCleaner(projectMirrorCleaner), httpapi.WithReexecGuard(reexecGuard{runs: runSvc, shared: sharedBuildSrv})),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// WriteTimeout 置 0:SSE 长连接(/api/runs/{id}/events)不可被写超时切断;
		// 各非流式 handler 自身在合理时间内完成,慢响应风险由 ReadTimeout/IdleTimeout 兜底。
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	// 优雅停机:SIGINT/SIGTERM 后 Shutdown,放行在途请求并让 defer st.Close() 生效。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 防火墙保活:启动即收敛(未确认的临时规则拆掉重放固化版;固化版断言在位),
	// 此后每分钟检查漂移与确认窗口超时。
	go firewallSvc.StartKeepAlive(ctx)

	// 集群运行时(设置监督 + 对等发现)随 ctx 启停;托管 easytier-core 时二进制定位结果
	// 打一行日志便于排障(找不到时集群页会给出放置指引)。
	meshRT.start(ctx)

	// SSH 隧道对账循环(0099):启用的隧道拉起 helper、停用的停进程、配置变更重启;
	// 与 mesh 同款停机语义 —— ctx 取消直接返回,**不杀**任何 helper(孤儿继续服务,
	// 下次启动心跳新鲜即零中断收养)。
	go sshtunnelSvc.StartLoop(ctx)

	// 平台定时备份调度器:按备份策略(设置→备份)间隔触发 scheduled 备份。随 ctx 停机退出。
	go backup.NewSweeper(backupSvc, 10*time.Minute).Start(ctx)
	log.Printf("[backup] 定时备份调度器已启动(每 10 分钟检查到点与否;策略默认关,需在设置开启)")

	// 实例定时备份调度器:按各实例备份计划(db_instances/redis_instances.backup_interval_hours)
	// 触发 scheduled 备份。随 ctx 停机退出。
	go dbbackup.NewSweeper(dbBackupSvc, 10*time.Minute).Start(ctx)
	log.Printf("[dbbackup] 实例定时备份调度器已启动(每 10 分钟扫描实例计划)")

	// 集群 DNS 映射同步循环(虚拟 IP 变化自动跟随 + 周期兜底)随 ctx 启停。
	go meshDNSSyncer.Run(ctx)
	if meshCorePath != "" {
		log.Printf("[mesh] 集群运行时已启动(easytier-core: %s)", meshCorePath)
	} else {
		log.Printf("[mesh] 集群运行时已启动(未定位到 easytier-core:启用托管模式后将自动下载;亦可在 设置→集群 手动触发,或放置 %s 于 %s/bin)", easytier.CoreBinary(), meshDataDir)
	}

	// 异常检测定时器(Story 6.5 增强):周期性 Check → 命中产告警 + best-effort 通知(去重已在服务内)。
	// 间隔 env PIPEWRIGHT_ANOMALY_INTERVAL(秒)覆盖,默认 60s;置 "0" 关闭定时(仍可手动「立即检测」)。
	// 无 enabled 规则时 Check 提前返回(不采集),故空载零开销。随 ctx 取消(停机)退出。
	if anomalyInterval > 0 {
		go func() {
			ticker := time.NewTicker(anomalyInterval)
			defer ticker.Stop()
			log.Printf("[anomaly] 定时检测已启用,间隔 %s", anomalyInterval)
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if _, err := anomalySvc.Check(ctx); err != nil && ctx.Err() == nil {
						log.Printf("[anomaly] 定时检测失败(已跳过本轮):%v", err)
					}
				}
			}
		}()
	}

	// 构建机/本机孤儿清扫(磁盘治理,0094):每 24h 对全部登记构建机清 /tmp/pipewright-remote|shared
	// 下 48h 前的残留工作区(控制机崩溃/SSH 清理失败时远端无人回收),并顺带再清一次本机临时目录
	// (覆盖长驻进程;启动时已清过一次)。年龄门保证进行中构建(现场 mtime 新)绝不受影响。
	go func() {
		first := time.NewTimer(5 * time.Minute) // 避开启动高峰
		defer first.Stop()
		select {
		case <-ctx.Done():
			return
		case <-first.C:
			maintenance.LogResult(maintSvc.SweepRemoteWorkspaces(ctx))
			maintenance.LogResult(maintSvc.SweepLocalTemp(ctx))
		}
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				maintenance.LogResult(maintSvc.SweepRemoteWorkspaces(ctx))
				maintenance.LogResult(maintSvc.SweepLocalTemp(ctx))
			}
		}
	}()
	log.Printf("[maintenance] 孤儿清扫已启动(每 24h:构建机 /tmp 残留 + 本机临时文件,仅清 48h 前的)")

	// 指标时序采样器(异常检测「看趋势」数据源):每 metricsSampleInterval 采集一次写时序历史,
	// 并按保留窗口清理旧样本。独立于异常检测(无规则也采样),随 ctx 停机退出。置 0 关闭采样。
	if metricsSampleInterval > 0 {
		go func() {
			ticker := time.NewTicker(metricsSampleInterval)
			defer ticker.Stop()
			log.Printf("[metrics] 指标采样已启用,间隔 %s,保留 %d 天", metricsSampleInterval, metricsRetentionDays)
			for {
				select {
				case <-ctx.Done():
					return
				case t := <-ticker.C:
					snaps, err := metricsSampleCollector.Collect(ctx, nil)
					if err != nil {
						if ctx.Err() == nil {
							log.Printf("[metrics] 采样失败(已跳过本轮):%v", err)
						}
						continue
					}
					if err := metricsHist.Record(ctx, httpapi.SamplesFromSnapshots(snaps, t.UTC())); err != nil && ctx.Err() == nil {
						log.Printf("[metrics] 写采样失败:%v", err)
					}
					// 顺手清理过期样本(保留窗口外)。
					if n, err := metricsHist.Sweep(ctx, t.UTC().AddDate(0, 0, -metricsRetentionDays)); err != nil && ctx.Err() == nil {
						log.Printf("[metrics] 清理过期样本失败:%v", err)
					} else if n > 0 {
						log.Printf("[metrics] 清理过期样本 %d 行", n)
					}
				}
			}
		}()
	}

	// ssar 机器监控采样器:每 ssarSampleInterval 对每台已装 ssar 的服务器跑一条合并
	// SSH 脚本(机器侧 sresar 供数),解析入 ssar_samples 时序表并按保留窗口清理。
	// 未装 / 不可达 / 无数据的机器自然跳过;置 0 关闭。随 ctx 停机退出。
	if ssarSampleInterval > 0 {
		go func() {
			ticker := time.NewTicker(ssarSampleInterval)
			defer ticker.Stop()
			log.Printf("[ssar] 机器监控采样已启用,间隔 %s,保留 %d 天", ssarSampleInterval, ssarRetentionDays)
			for {
				select {
				case <-ctx.Done():
					return
				case t := <-ticker.C:
					if _, err := ssarSvc.SampleAll(ctx); err != nil {
						if ctx.Err() == nil {
							log.Printf("[ssar] 采样失败(已跳过本轮):%v", err)
						}
						continue
					}
					// 顺手清理过期样本(保留窗口外)。
					if dn, err := ssarSvc.Sweep(ctx, t.UTC().AddDate(0, 0, -ssarRetentionDays)); err != nil && ctx.Err() == nil {
						log.Printf("[ssar] 清理过期样本失败:%v", err)
					} else if dn > 0 {
						log.Printf("[ssar] 清理过期样本 %d 行", dn)
					}
				}
			}
		}()
	}

	// 安全中心定时扫描器:分钟对齐(与项目 cron 调度器同纪律),规则在前端配置
	// (security_schedule 单行:enabled + 5 段 cron + 扫描类型)。到点对全部纳管服务器
	// (排除 PVE 节点)发起所选类型扫描(trigger=scheduled),同分钟去重;未启用空转。
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		lastMinute := ""
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-ticker.C:
				sch, err := secScanSvc.GetSchedule(ctx)
				if err != nil || !sch.Enabled {
					continue
				}
				sched, err := cron.Parse(sch.Expression)
				if err != nil {
					continue // 表达式非法(保存端已校验,防御旧脏数据)
				}
				minute := t.UTC().Truncate(time.Minute).Format(time.RFC3339)
				if minute == lastMinute || !sched.Matches(t) {
					continue
				}
				lastMinute = minute
				runs, err := secScanSvc.StartFleetScan(ctx, sch.ScanTypes, secscan.TriggerScheduled)
				if err != nil {
					if ctx.Err() == nil {
						log.Printf("[security] 定时扫描触发失败:%v", err)
					}
					continue
				}
				_ = secScanSvc.TouchScheduleLastRun(ctx, t)
				log.Printf("[security] 定时扫描已触发:%d 个扫描任务(%v)", len(runs), sch.ScanTypes)
			}
		}
	}()

	go func() {
		log.Printf("pipewright listening on %s", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	stop()
	log.Printf("shutting down…")
	if opsChatSvc != nil {
		opsCtx, opsCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := opsChatSvc.Close(opsCtx); err != nil {
			log.Printf("[opschat] worker shutdown did not converge before deadline")
		}
		opsCancel()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
	// HTTP 停机后停 worker pool:取消在途运行执行,等 worker 退出(随 shutdownCtx 超时)。
	cronScheduler.Stop()
	retentionSweeper.Stop()
	registryHubSweeper.Stop()
	pool.Stop(shutdownCtx)
	log.Printf("[run] worker pool stopped")
}

// envDurationSeconds 读取 name 环境变量(整数秒)为 time.Duration;未设/非法 → def。
// 显式 "0"(或负)→ 0(调用方据此关闭对应定时任务)。
func envDurationSeconds(name string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("[config] %s=%q 非法(应为整数秒),用默认 %s", name, v, def)
		return def
	}
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// ── 「流水线即代码」运行时覆盖适配器(FR-8-12)──────────────────────────────────
// 把既有领域服务桥接到 pacloader 的小接口,使 pacloader 不反向 import project/vault/httpapi。

// pacProjectLookup 把 project.Service 适配为 pacloader.ProjectLookup(取仓库/凭据/默认分支)。
type pacProjectLookup struct{ svc project.Service }

func (p pacProjectLookup) Lookup(ctx context.Context, projectID string) (pacloader.ProjectInfo, error) {
	proj, err := p.svc.Get(ctx, projectID)
	if err != nil {
		return pacloader.ProjectInfo{}, err
	}
	return pacloader.ProjectInfo{
		RepoURL:       proj.RepoURL,
		CredentialID:  proj.CredentialID,
		DefaultBranch: proj.DefaultBranch,
		PacEnabled:    proj.PacEnabled,
	}, nil
}

type pacGitAuthResolver struct{ vault vault.Vault }

func (r pacGitAuthResolver) GetGitAuth(credentialID string) (vault.GitAuth, error) {
	return r.vault.GetGitAuth(credentialID)
}

// pacBlobFetcher adapts SourceReader for pacloader without exposing credentials.
type pacBlobFetcher struct{ reader httpapi.SourceReader }

func (b pacBlobFetcher) FetchBlob(ctx context.Context, repoURL string, cred vault.GitAuth, ref, file string) (string, bool, error) {
	blob, err := b.reader.Blob(ctx, repoURL, cred, ref, file)
	if err != nil {
		return "", false, err
	}
	return blob.Content, blob.Degraded, nil
}

// pacSpecLoader 把 *pacloader.Loader 适配为 dagrun.SpecLoader:转换其同形的 SpecSource
// (配置来源可见性 · Slice 2)。pacloader 不反向 import dagrun(避免环),故来源结构在 main 处转换。
type pacSpecLoader struct{ inner *pacloader.Loader }

func (l pacSpecLoader) Get(ctx context.Context, projectID, branch string) (*pipeline.Config, dagrun.SpecSource, error) {
	cfg, src, err := l.inner.Get(ctx, projectID, branch)
	return cfg, dagrun.SpecSource{
		FromRepo:       src.FromRepo,
		Ref:            src.Ref,
		File:           src.File,
		FallbackReason: src.FallbackReason,
	}, err
}

// buildRunnerOption 按 PIPEWRIGHT_BUILDER 开关返回注入 pool 的运行执行器选项(Story 3-3/3-5)。
//
//   - real :强制真实 Builder;构造失败(无容器 CLI)直接 fail-fast(用户显式要真实却没环境,应明确报错)。
//   - stub :不注入 WithRunner(pool 默认 StubRunner),返回空选项。
//   - 其它/缺省 auto:尝试构造真实 Builder,探测不到容器 CLI 时回退桩(优雅降级,NFR-10)。
//
// 返回 []run.PoolOption(可能为空):空 ⇒ 用 pool 默认 StubRunner。
func buildRunnerOption(projectSvc project.Service, settingsSvc pipeline.SettingsService, v vault.Vault, artStore *artifactstore.Store, repoCache *repocache.Cache, extraOpts ...build.BuilderOption) []run.PoolOption {
	clonerOpt := func(*build.Builder) {}
	if repoCache != nil {
		clonerOpt = build.WithCloner(repoCache)
	}
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("PIPEWRIGHT_BUILDER")))
	switch mode {
	case "stub":
		log.Printf("[build] PIPEWRIGHT_BUILDER=stub:使用桩 runner(合成日志,不碰容器)")
		return nil
	case "real":
		b, err := build.NewBuilder(projectSvc, settingsSvc, v, append([]build.BuilderOption{build.WithArtifactStore(artStore), build.WithImageGC(os.Getenv("PIPEWRIGHT_NO_IMAGE_GC") != "1"), clonerOpt}, extraOpts...)...)
		if err != nil {
			log.Fatalf("[build] PIPEWRIGHT_BUILDER=real 但构建器不可用:%v", err)
		}
		log.Printf("[build] 真实隔离构建器已启用(容器 CLI=%s)", b.DriverBinary())
		return []run.PoolOption{run.WithRunner(b)}
	default:
		b, err := build.NewBuilder(projectSvc, settingsSvc, v, append([]build.BuilderOption{build.WithArtifactStore(artStore), build.WithImageGC(os.Getenv("PIPEWRIGHT_NO_IMAGE_GC") != "1"), clonerOpt}, extraOpts...)...)
		if err != nil {
			log.Printf("[build] 未探测到容器 CLI(docker/nerdctl/podman),回退桩 runner(PIPEWRIGHT_BUILDER=real 可强制要求真实):%v", err)
			return nil
		}
		log.Printf("[build] auto:真实隔离构建器已启用(容器 CLI=%s)", b.DriverBinary())
		return []run.PoolOption{run.WithRunner(b)}
	}
}

// targetExister 把 target.Service 适配为 runner.ServerExister(Get 无错即存在),供 runner 配置校验。
type targetExister struct{ svc target.Service }

func (t targetExister) Exists(ctx context.Context, id string) bool {
	_, err := t.svc.Get(ctx, id)
	return err == nil
}

// targetProber 把 target.Service.Test 适配为 runner.HealthProber(SSH 实连探测),供调度器健康过滤。
type targetProber struct{ svc target.Service }

func (t targetProber) Probe(ctx context.Context, serverID string) bool {
	res, err := t.svc.Test(ctx, serverID)
	return err == nil && res != nil && res.OK
}

// runnerPool 把「runner 配置(Service)」与「池调度器(Scheduler)」组合适配为 build.RunnerResolver:
// SelectorFor 走配置,Acquire 走调度(选机 + 占槽 + FIFO 等待)。装配层桥接,build 不直依赖 runner。
type runnerPool struct {
	cfg   runner.Service
	sched *runner.Scheduler
}

func (p runnerPool) SelectorFor(ctx context.Context, projectID string) (string, bool) {
	return p.cfg.SelectorFor(ctx, projectID)
}

func (p runnerPool) Acquire(ctx context.Context, pipelineID, selector string, log func(string)) (string, string, func(), error) {
	return p.sched.Acquire(ctx, pipelineID, selector, log)
}

// dnsResolverAdapter 适配 certmgmt.CredentialsResolver:把「按提供商 id 取 (类型, API ID, Secret)/
// 取类型 / 取根区清单」下沉到 dnsprovider.Service。Secret 仅在生成一次性 acme.sh 签发脚本时
// 进程内使用,即用即弃;API ID 非机密。
type dnsResolverAdapter struct{ dns dnsprovider.Service }

func (a *dnsResolverAdapter) Resolve(ctx context.Context, providerID string) (string, string, string, bool, error) {
	return a.dns.ResolveCredential(ctx, providerID)
}

func (a *dnsResolverAdapter) ProviderType(ctx context.Context, providerID string) (string, bool, error) {
	return a.dns.ProviderType(ctx, providerID)
}

func (a *dnsResolverAdapter) ProviderZones(ctx context.Context, providerID string) ([]string, bool, error) {
	zones, ok, err := a.dns.Zones(ctx, providerID)
	if err != nil || !ok {
		return nil, ok, err
	}
	out := make([]string, 0, len(zones))
	for _, z := range zones {
		out = append(out, z.BaseDomain)
	}
	return out, true, nil
}

// instanceRollGateway 把 servicereg 适配为 deploy.InstanceGateway(统一滚动的网关联动消费;
// 晚绑防 import 环:deploy 不 import servicereg,servicereg 也不 import deploy)。
type instanceRollGateway struct{ sr servicereg.Service }

func (g *instanceRollGateway) ResolveInstances(ctx context.Context, serverID, serviceRef, container string) ([]deploy.InstanceRef, error) {
	refs, err := g.sr.ResolveDeployInstances(ctx, serverID, serviceRef, container)
	if err != nil {
		return nil, err
	}
	out := make([]deploy.InstanceRef, 0, len(refs))
	for _, r := range refs {
		out = append(out, deploy.InstanceRef{
			ServiceID: r.ServiceID, ServiceName: r.ServiceName,
			InstanceID: r.InstanceID, ServerID: r.ServerID, Container: r.Container,
			Port: r.Port, HostPort: r.HostPort,
		})
	}
	return out, nil
}

func (g *instanceRollGateway) SwapInstance(ctx context.Context, serviceID, serverID, oldContainer, newContainer string, hostPort int) error {
	return g.sr.SwapInstance(ctx, serviceID, serverID, oldContainer, newContainer, hostPort)
}

func (g *instanceRollGateway) DetachInstance(ctx context.Context, instanceID string) error {
	return g.sr.SetInstanceAttached(ctx, instanceID, false)
}

func (g *instanceRollGateway) AttachInstance(ctx context.Context, instanceID string) error {
	return g.sr.SetInstanceAttached(ctx, instanceID, true)
}

func (g *instanceRollGateway) EnsureInstance(ctx context.Context, serviceID, serverID, container string, port, hostPort int) error {
	_, err := g.sr.EnsureInstance(ctx, serviceID, serverID, container, port, hostPort)
	return err
}

func (g *instanceRollGateway) EnsureInstanceDetached(ctx context.Context, serviceID, serverID, container string, port, hostPort int) (deploy.InstanceRef, error) {
	inst, err := g.sr.EnsureInstanceDetached(ctx, serviceID, serverID, container, port, hostPort)
	if err != nil {
		return deploy.InstanceRef{}, err
	}
	return deploy.InstanceRef{
		ServiceID: inst.ServiceID, InstanceID: inst.ID, ServerID: inst.ServerID,
		Container: inst.Container, Port: inst.Port, HostPort: inst.HostPort,
	}, nil
}

func (g *instanceRollGateway) PruneInstances(ctx context.Context, serviceID string, serverIDs []string) error {
	_, err := g.sr.PruneInstancesNotIn(ctx, serviceID, serverIDs)
	return err
}

// ResolveStaticDeploy 把静态发布目标解析适配为 deploy.StaticDeploySpec(deploy_static 节点
// 的目标/发布目录来源;实现 deploy.StaticDeployResolver 可选扩展)。
func (g *instanceRollGateway) ResolveStaticDeploy(ctx context.Context, serviceRef string) (deploy.StaticDeploySpec, error) {
	spec, err := g.sr.ResolveStaticDeploy(ctx, serviceRef)
	if err != nil {
		return deploy.StaticDeploySpec{}, err
	}
	return deploy.StaticDeploySpec{
		ServiceID: spec.ServiceID, FQDN: spec.FQDN, Path: spec.Path,
		ServerIDs: spec.ServerIDs, BaseDir: spec.BaseDir,
	}, nil
}

// serverAddrResolver 把 serverID 解析为网关可达地址(target 查 SSH Host;主机名经控制端
// DNS 解析为 IP —— nginx upstream 静态成员必须可解析,否则 reload 卡死)。注入 servicereg
// 供集群反代渲染。
type serverAddrResolver struct{ tg target.Service }

func (r serverAddrResolver) resolve(ctx context.Context, serverID string) (string, error) {
	srv, err := r.tg.Get(ctx, serverID)
	if err != nil {
		return "", err
	}
	host := strings.TrimSpace(srv.Host)
	if host == "" || net.ParseIP(host) != nil {
		return host, nil
	}
	addrs, lerr := net.DefaultResolver.LookupHost(ctx, host)
	if lerr != nil || len(addrs) == 0 {
		if host != "" {
			return host, nil // 解析失败时回落主机名(渲染后由 nginx -t 报错暴露),不静默丢实例
		}
		return "", fmt.Errorf("服务器 %s 无可达地址", serverID)
	}
	return addrs[0], nil
}

// platformCertSource 适配 platformhttps.CertSource:证书元数据/PEM 解密下沉 certmgmt
// (PEM 仅进程内传递,绝不过 HTTP;晚绑防 platformhttps 直接依赖 certmgmt)。
type platformCertSource struct{ cm certmgmt.Service }

func (a *platformCertSource) GetCert(ctx context.Context, id string) (*platformhttps.CertInfo, error) {
	c, err := a.cm.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &platformhttps.CertInfo{
		ID: c.ID, PrimaryDomain: c.PrimaryDomain, Domains: c.Domains,
		Status: c.Status, NotAfter: c.NotAfter,
	}, nil
}

func (a *platformCertSource) OpenCertPEM(ctx context.Context, id string) (string, string, error) {
	return a.cm.OpenCertPEM(ctx, id)
}

// platformHTTPSAdapter 适配 certmgmt.PlatformHTTPS:证书续期/重下发后联动平台宿主 nginx
// 重新落盘证书并 reload;删除证书前检查占用。
type platformHTTPSAdapter struct{ ph platformhttps.Service }

func (a *platformHTTPSAdapter) UsesCert(ctx context.Context, certID string) (bool, error) {
	return a.ph.UsesCert(ctx, certID)
}

func (a *platformHTTPSAdapter) RedeployCert(ctx context.Context, certID string) error {
	return a.ph.RedeployCert(ctx, certID)
}

// registryCertSource 适配 registryhub.CertSource:证书元数据/PEM 解密下沉 certmgmt(PEM 仅
// 进程内传递,绝不过 HTTP;晚绑防 registryhub 直接依赖 certmgmt)。certSvc 在 registry 栈
// 之后构造,经 set 接上;HTTP 服务启动前必然已装配(set 前调用只会得到诚实报错)。
type registryCertSource struct {
	mu sync.Mutex
	cm certmgmt.Service
}

func (a *registryCertSource) set(cm certmgmt.Service) {
	a.mu.Lock()
	a.cm = cm
	a.mu.Unlock()
}

func (a *registryCertSource) svc() certmgmt.Service {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cm
}

func (a *registryCertSource) GetCert(ctx context.Context, id string) (*registryhub.CertInfo, error) {
	cm := a.svc()
	if cm == nil {
		return nil, errors.New("证书能力未装配")
	}
	c, err := cm.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &registryhub.CertInfo{
		ID: c.ID, PrimaryDomain: c.PrimaryDomain, Domains: c.Domains,
		Status: c.Status, NotAfter: c.NotAfter,
	}, nil
}

func (a *registryCertSource) OpenCertPEM(ctx context.Context, id string) (string, string, error) {
	cm := a.svc()
	if cm == nil {
		return "", "", errors.New("证书能力未装配")
	}
	return cm.OpenCertPEM(ctx, id)
}

// registryHubAdapter 适配 certmgmt.PlatformHTTPS:内置 registry 栈正引用的证书续期/重下发后
// 重新部署栈(部署时重新解密落盘 PEM 并按配置漂移重建容器);删除证书前占用拦截。
type registryHubAdapter struct{ h *registryhub.Hub }

func (a *registryHubAdapter) UsesCert(ctx context.Context, certID string) (bool, error) {
	return a.h.UsesCert(ctx, certID)
}

func (a *registryHubAdapter) RedeployCert(ctx context.Context, certID string) error {
	return a.h.RedeployCert(ctx, certID)
}

// multiPlatformHTTPS 把多个证书联动消费方组合为一个 PlatformHTTPS(UsesCert 任一 true;
// RedeployCert 依次执行,单方失败立即返回,由 certmgmt 落到证书 detail)。
type multiPlatformHTTPS struct{ parts []certmgmt.PlatformHTTPS }

func (m *multiPlatformHTTPS) UsesCert(ctx context.Context, certID string) (bool, error) {
	for _, p := range m.parts {
		ok, err := p.UsesCert(ctx, certID)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func (m *multiPlatformHTTPS) RedeployCert(ctx context.Context, certID string) error {
	for _, p := range m.parts {
		if err := p.RedeployCert(ctx, certID); err != nil {
			return err
		}
	}
	return nil
}

// defaultPortFromAddr 从监听地址(如 ":8080"/"0.0.0.0:8080")提取端口作为平台 HTTPS 的
// 默认反代上游端口;解析失败回退 8080。
func defaultPortFromAddr(addr string) int {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		if p, err := strconv.Atoi(addr[i+1:]); err == nil && p > 0 && p < 65536 {
			return p
		}
	}
	return 8080
}

// certSinkAdapter 适配 certmgmt.CertSink:把证书同步进 servicereg 基域(UploadCert 内部落库 +
// 全量收敛 nginx);CurrentCertPEM 供删除证书时比对归属;ClearCert 清空基域回退 HTTP-only。
type certSinkAdapter struct{ sr servicereg.Service }

func (a *certSinkAdapter) ListBaseDomains(ctx context.Context) ([]certmgmt.BaseDomain, error) {
	domains, err := a.sr.ListDomains(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]certmgmt.BaseDomain, 0, len(domains))
	for _, d := range domains {
		out = append(out, certmgmt.BaseDomain{ID: d.ID, BaseDomain: d.BaseDomain})
	}
	return out, nil
}

func (a *certSinkAdapter) UploadCert(ctx context.Context, domainID, certPEM, keyPEM string) error {
	_, err := a.sr.UploadCert(ctx, domainID, certPEM, keyPEM)
	return err
}

func (a *certSinkAdapter) CurrentCertPEM(ctx context.Context, domainID string) (string, bool, error) {
	return a.sr.ExportCert(ctx, domainID)
}

func (a *certSinkAdapter) ClearCert(ctx context.Context, domainID string) error {
	return a.sr.ClearCert(ctx, domainID)
}

// reexecGuard 实现自更新 re-exec 门槛:有活跃任务(本地非终态运行 + 其他环境
// 提交的共享构建任务)时推迟重启。托管 easytier-core 不再随 re-exec 停止:
// 新进程按参数指纹收养遗留进程(一致零中断,不一致杀旧拉新,见
// cmd/pipewright/mesh_runtime.go 与 internal/mesh/easytier/adopt.go)。
type reexecGuard struct {
	runs   run.Service
	shared *build.SharedBuildServer
}

func (g reexecGuard) ActiveTasks() (int, int) {
	n, err := g.runs.ActiveRuns(context.Background())
	if err != nil {
		log.Printf("[update] 统计活跃运行失败(按 0 计,可强制重启): %v", err)
		n = 0
	}
	return n, g.shared.ActiveJobs() // nil 接收者安全(legacy 装配)
}

// GracefulShutdown 是 re-exec 前的托管子进程清理钩子;mesh 自 v1.6.4 的
// 「先停再 exec」已改为收养机制,当前无需要同步停止的托管子进程,保留空实现
// 供未来扩展。
func (g reexecGuard) GracefulShutdown() {}

// aiContextPolicy 把 AI 设置里的上下文声明映射为 opschat 策略(每轮读取,
// 设置保存后下轮生效;读取失败回退全默认,绝不阻塞对话)。
func aiContextPolicy(svc ai.Service) opschat.ContextPolicy {
	cfg, err := svc.Get(context.Background())
	if err != nil || cfg == nil {
		return opschat.ContextPolicy{}
	}
	c := cfg.Context.Resolved()
	auto := c.AutoCompact == nil || *c.AutoCompact
	return opschat.ContextPolicy{
		WindowTokens:     c.WindowTokens,
		MaxOutputTokens:  c.MaxOutputTokens,
		HistoryMessages:  c.HistoryMessages,
		HistoryBytes:     c.HistoryBytes,
		AutoCompact:      &auto,
		KeepRecentTokens: c.KeepRecentTokens,
		BufferTokens:     c.BufferTokens,
	}
}

// ─── AI 测试(ai_test)装配适配器 ─────────────────────────────────────────────

// aiTestInfraAdapter 把 targetSvc 适配为 ai_test 的部署目标解析 + 被测机执行能力
// (build.aiTestInfra):deployJob 的目标选择器(server:<id> 钉死形式)解析出被测机;
// 内网抓取(vantage)经该机 SSH 执行 curl(argv 数组,不拼 shell)。
type aiTestInfraAdapter struct{ svc target.Service }

func aiTestCfgStr(cfg map[string]any, key string) string {
	s, _ := cfg[key].(string)
	return strings.TrimSpace(s)
}

func (a *aiTestInfraAdapter) AITestDeployTarget(ctx context.Context, deployJob pipeline.Job) (string, string, error) {
	selector := aiTestCfgStr(deployJob.Config, "selector")
	if selector == "" {
		if sid := aiTestCfgStr(deployJob.Config, "serverId"); sid != "" {
			selector = "server:" + sid
		}
	}
	sid, ok := strings.CutPrefix(selector, "server:")
	if !ok || sid == "" {
		return "", "", fmt.Errorf("部署节点目标为标签选择器,无唯一目标机(%q)", selector)
	}
	srv, err := a.svc.Get(ctx, sid)
	if err != nil {
		return "", "", err
	}
	host := srv.Host
	if srv.Port != 0 && srv.Port != 22 {
		host = fmt.Sprintf("%s:%d", srv.Host, srv.Port)
	}
	return sid, host, nil
}

func (a *aiTestInfraAdapter) AITestRemoteExec(ctx context.Context, serverID string, argv []string) (string, error) {
	out, err := a.svc.Exec(ctx, serverID, argv)
	if err != nil {
		return "", err
	}
	if out == nil || out.ExitCode != 0 {
		code := -1
		if out != nil {
			code = out.ExitCode
		}
		return "", fmt.Errorf("远程执行退出码 %d", code)
	}
	return out.Stdout, nil
}
