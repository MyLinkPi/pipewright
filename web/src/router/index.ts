import { createRouter, createWebHistory } from 'vue-router'
import { useSessionStore, type UserRole } from '../stores/session'

// Lazy-loaded views
const Login       = () => import('../views/Login.vue')
const AppShell    = () => import('../layouts/AppShell.vue')
// 用户体系:邀请注册(公开)+ 普通用户精简页 + 管理员用户管理页
const Register    = () => import('../views/Register.vue')
const MyProjects  = () => import('../views/MyProjects.vue')
const MemberRunDetail = () => import('../views/MemberRunDetail.vue')
const MemberSettings  = () => import('../views/MemberSettings.vue')
const Users       = () => import('../views/Users.vue')
// Story 1-7: first-run onboarding guide
const Onboarding  = () => import('../views/Onboarding.vue')
const Dashboard   = () => import('../views/Dashboard.vue')
const Projects    = () => import('../views/Projects.vue')
const Runs        = () => import('../views/Runs.vue')
// FR-8-13: 复用库(流水线模板 + 变量组)
const Library     = () => import('../views/Library.vue')
// FR-8-15: DORA 四指标仪表盘(部署频率 / 前置时长 / 变更失败率 / MTTR;只读聚合)
const DoraDashboard = () => import('../views/DoraDashboard.vue')
// Story 6-1: multi-host status overview (server-layer CPU/memory/disk metrics, FR-15)
const ServerStatus = () => import('../views/ServerStatus.vue')
// 容器管理:跨所有已登记服务器聚合容器(docker ps over SSH)+ 行内生命周期操作
const Containers = () => import('../views/Containers.vue')
// PM2 管理:跨所有已登记服务器聚合 pm2 进程(pm2 jlist over SSH)+ 行内生命周期操作
const Pm2 = () => import('../views/Pm2.vue')
// R2 / E2.4: 证书总览大盘(跨主机跨域名一张表 + 到期高亮;只读聚合)
const Certificates = () => import('../views/Certificates.vue')
// 服务注册网关(nginx):基域/泛域名证书/子域名服务(DPanel 式容器面板的配套入口)。
const ServiceRegistry = () => import('../views/ServiceRegistry.vue')
// 应用商店:模板参数一键部署(复用 Stacks 链路)。
const AppStore = () => import('../views/AppStore.vue')
// 数据库在线管理(phpMyAdmin 式):实例在 设置 → 数据库 注册,此处浏览/查询/管理。
const Databases = () => import('../views/Databases.vue')
// 对象存储浏览(只读):实例在 设置 → 对象存储 注册,此处浏览桶内目录(marker 游标分页)。
const ObjectStorage = () => import('../views/ObjectStorage.vue')
// Redis 在线管理:实例在 设置 → Redis 注册(应用市场部署 redis 自动登记),此处 SCAN 分页浏览键 + 删键/改 TTL。
const Redis = () => import('../views/Redis.vue')
// 服务器磁盘管理:块设备/LVM 拓扑 + 分区/格式化/挂载/扩缩容(只读默认 + 显式解锁)。
const Disks = () => import('../views/Disks.vue')
const Files = () => import('../views/Files.vue')
// 流水线产物管理:跨运行产物列表(文件/镜像)+ 查看详情 / 下载 / 删除(仅管理员)。
const Artifacts = () => import('../views/Artifacts.vue')
// Story 6-5: configurable anomaly detection & alerts (FR-23)
const AnomalyDetection = () => import('../views/AnomalyDetection.vue')
const SsarMonitor = () => import('../views/SsarMonitor.vue')
const SsarMonitorDetail = () => import('../views/SsarMonitorDetail.vue')
// 集群(对等节点 + EasyTier 自组网):节点状态/链路(p2p|中继)总览 + 授权管理入口
const Cluster = () => import('../views/Cluster.vue')
// Ceph 集群部署与管理:cephadm 引导向导 + 状态/主机/OSD/存储池/RBD/CephFS 管理
const Ceph = () => import('../views/Ceph.vue')
const CephClusterDetail = () => import('../views/CephClusterDetail.vue')
const SettingsMesh = () => import('../views/settings/SettingsMesh.vue')
const Settings    = () => import('../views/Settings.vue')
const SettingsAI  = () => import('../views/settings/SettingsAI.vue')
// OAuth app config: per-provider Client ID/Secret for one-click "连接" in the vault
const SettingsOAuth = () => import('../views/settings/SettingsOAuth.vue')
const SettingsVault = () => import('../views/settings/SettingsVault.vue')
// R3 / E3.1: DNS providers (Cloudflare / DNSPod / 阿里云 DNS) — unlocks DNS-01 wildcard + instant subdomains
const SettingsDnsProviders = () => import('../views/settings/SettingsDnsProviders.vue')
// 对象存储:流水线 oss_upload 节点上传产物到阿里云 OSS(本期仅阿里云)
const SettingsObjectStorage = () => import('../views/settings/SettingsObjectStorage.vue')
// 数据库实例注册(MySQL/PostgreSQL;管理操作在 /databases 顶级页)
const SettingsDatabases = () => import('../views/settings/SettingsDatabases.vue')
// Redis 实例注册(应用市场部署 redis 后自动登记;管理在 /redis 顶级页)
const SettingsRedis = () => import('../views/settings/SettingsRedis.vue')
// PVE 集群管理:集群经服务器登记自动发现(设置 → PVE 配 Token),此处总览/VM/电源/任务。
const PVE = () => import('../views/PVE.vue')
// PVE 集群 Token 配置(集群记录由服务器登记探测自动生成)
const SettingsPVE = () => import('../views/settings/SettingsPVE.vue')
// 阿里云 ECS 批量导入:多账户 AK/SK 配置 + 拉实例手选范围批量登记/更新服务器
const SettingsAliyunEcs = () => import('../views/settings/SettingsAliyunEcs.vue')
// 平台 HTTPS 访问:宿主 nginx 自动配置(证书 + 80→443 跳转),证书复用证书管理
const SettingsHttps = () => import('../views/settings/SettingsHttps.vue')
const SettingsAccount = () => import('../views/settings/SettingsAccount.vue')
// Story 4-1: target server registry + shared SSH layer (FR-14)
const SettingsServers = () => import('../views/settings/SettingsServers.vue')
// 集群级共享构建机(0086):owner 视角任务列表/强制取消 + 共享机总览
const SettingsSharedBuild = () => import('../views/settings/SettingsSharedBuild.vue')
// Story 5-1: notification channels (FR-19)
const SettingsNotifications = () => import('../views/settings/SettingsNotifications.vue')
// Story 7-5: diagnosis feedback-loop stats (FR-26)
const SettingsDiagnosisStats = () => import('../views/settings/SettingsDiagnosisStats.vue')
// 系统信息 + 一键检查更新
const SettingsSystem = () => import('../views/settings/SettingsSystem.vue')
// 内置本地 Docker registry(制品 + pull-through 缓存 + daemon.json 下发)
const SettingsRegistry = () => import('../views/settings/SettingsRegistry.vue')
// Story 2-2: new pipeline editor
const ProjectPipeline = () => import('../views/ProjectPipeline.vue')
// Story 2-3: triggers (kept for backward compat; now a thin wrapper around TriggersPanel)
const ProjectTriggers = () => import('../views/ProjectTriggers.vue')
// Story 7-4: read-only code browsing (FR-4) — Monaco dynamic-imported, off the main bundle
const ProjectCode = () => import('../views/ProjectCode.vue')
const RunDetail = () => import('../views/RunDetail.vue')
// Story 1-6: living styleguide (public — no auth required for dev browsing)
const StatesShowcase = () => import('../views/StatesShowcase.vue')
// 自定义节点工作室:路由级聚焦低代码编辑页(shell 外全屏,进出经路由)
const CustomNodeStudioPage = () => import('../views/CustomNodeStudioPage.vue')
// AI 运维终端:独立全屏页(左终端 / 右 AI 助手),shell 外但需鉴权(对标阿里云 Cloud Shell)
const ServerTerminal = () => import('../views/ServerTerminal.vue')

const router = createRouter({
  history: createWebHistory(),
  routes: [
    // ——— Shell-outside: Login (no rail) ———
    {
      path: '/login',
      name: 'login',
      component: Login,
      meta: { public: true, title: '登录' },
    },
    // ——— 邀请注册(shell 外,公开):?token=<invitation token> ———
    {
      path: '/register',
      name: 'register',
      component: Register,
      meta: { public: true, title: '注册' },
    },
    // ——— Story 1-6: Component library living styleguide (shell-free, public) ———
    {
      path: '/states',
      name: 'states-showcase',
      component: StatesShowcase,
      meta: { public: true },
    },
    // ——— 自定义节点工作室:聚焦全屏编辑器(shell 外,但需鉴权)———
    {
      path: '/library/studio',
      name: 'studio-create',
      component: CustomNodeStudioPage,
      meta: { requiresAuth: true, title: '节点工作室' },
    },
    {
      path: '/library/studio/:id',
      name: 'studio-edit',
      component: CustomNodeStudioPage,
      meta: { requiresAuth: true, title: '节点工作室' },
    },
    // ——— AI 运维终端:独立全屏页(query: ?container=&shell=);shell 外,需鉴权 ———
    {
      path: '/servers/:id/terminal',
      name: 'server-terminal',
      component: ServerTerminal,
      meta: { requiresAuth: true, title: '运维终端' },
    },
    // ——— Shell-inside: authenticated routes ———
    {
      path: '/',
      component: AppShell,
      meta: { requiresAuth: true },
      children: [
        // 首页:概览仪表盘尚未建,暂重定向到项目页(避免落到占位页)。
        { path: '', name: 'overview', redirect: { name: 'dashboard' } },
        { path: 'dashboard', name: 'dashboard', component: Dashboard, meta: { title: '概览' } },
        // Story 1-7: first-run onboarding guide (inside shell, auth-required)
        { path: 'onboarding', name: 'onboarding', component: Onboarding, meta: { title: '快速上手' } },
        { path: 'projects', name: 'projects', component: Projects, meta: { title: '项目' } },
        // Story 2-2: 4-tab pipeline editor (primary config entry point)
        // fullBleed: 编辑器是全幅工作面板(AppShell 据此放开 --content-max 宽度上限,画布吃满宽屏)。
        { path: 'projects/:id/pipeline', name: 'project-pipeline', component: ProjectPipeline, meta: { title: '流水线', fullBleed: true } },
        // Story 2-3: backward-compat standalone triggers page
        { path: 'projects/:id/triggers', name: 'project-triggers', component: ProjectTriggers, meta: { title: '触发器' } },
        // Story 7-4: read-only code browsing (FR-4)
        { path: 'projects/:id/code', name: 'project-code', component: ProjectCode, meta: { title: '代码' } },
        { path: 'runs', name: 'runs', component: Runs, meta: { title: '运行' } },
        // ——— 普通用户(member)精简页:仅项目 / 运行 / 日志 / 自身设置 ———
        // 用 meta.roles 显式声明;守卫对 member 采用「默认拒绝」——未声明 roles 的路由一律不可见。
        { path: 'my-projects', name: 'my-projects', component: MyProjects, meta: { title: '我的项目', roles: ['member'] } },
        // 复用 Runs.vue(本就不含管理动作;详情跳转按角色分流)。
        { path: 'my-runs', name: 'my-runs', component: Runs, meta: { title: '运行', roles: ['member'] } },
        { path: 'my-runs/:id', name: 'my-run-detail', component: MemberRunDetail, meta: { title: '运行详情', roles: ['member'] } },
        // 普通用户设置:独立页,不挂管理员 Settings 外壳(不暴露 12 项管理员子导航)。
        { path: 'my-settings', name: 'my-settings', component: MemberSettings, meta: { title: '设置', roles: ['member'] } },
        // ——— 管理员:用户 / 邀请 / 项目成员管理 ———
        { path: 'users', name: 'users', component: Users, meta: { title: '用户', roles: ['admin'] } },
        // FR-8-13: 复用库(流水线模板 + 变量组)
        { path: 'library', name: 'library', component: Library, meta: { title: '复用库' } },
        { path: 'runs/:id', name: 'run-detail', component: RunDetail, meta: { title: '运行详情' } },
        // FR-8-15: DORA 指标仪表盘(只读聚合;projectId / window 经 query 即状态)
        { path: 'metrics/dora', name: 'dora', component: DoraDashboard, meta: { title: 'DORA 指标' } },

        // 顶层「服务器」占位页 → 重定向到真实的多机状态页(登记在 /settings/servers)。
        { path: 'servers', name: 'servers', redirect: { name: 'server-status' } },
        // Story 6-1: multi-host status overview (server-layer metrics, FR-15)
        { path: 'server-status', name: 'server-status', component: ServerStatus, meta: { title: '服务器状态' } },
        // 集群:对等节点总览(EasyTier 链路 p2p/中继、机器状态、流水线状态;FR-集群)
        { path: 'cluster', name: 'cluster', component: Cluster, meta: { title: '集群', roles: ['admin'] } },
        // Ceph 集群:部署向导(cephadm)+ 管理(状态/主机/OSD/池/RBD/CephFS)
        { path: 'ceph', name: 'ceph', component: Ceph, meta: { title: 'Ceph', roles: ['admin'] } },
        { path: 'ceph/:id', name: 'ceph-cluster-detail', component: CephClusterDetail, meta: { title: 'Ceph 集群', roles: ['admin'] } },

        { path: 'containers', name: 'containers', component: Containers, meta: { title: '容器' } },
        // PM2 管理:跨服务器聚合 pm2 进程 + 行内生命周期操作(pm2 over SSH)。
        { path: 'pm2', name: 'pm2', component: Pm2, meta: { title: 'PM2' } },
        // 服务注册网关(nginx):abc.efg.com 子域名反代 + TCP 透传 + 泛域名证书上传。
        { path: 'service-reg', name: 'service-registry', component: ServiceRegistry, meta: { title: '服务注册' } },
        // 应用商店:MySQL/Redis 等模板参数一键部署。
        { path: 'app-store', name: 'app-store', component: AppStore, meta: { title: '应用商店' } },
        // 数据库在线管理(phpMyAdmin 式):库/表浏览、SQL 控制台、导入导出、账号管理(仅管理员)。
        { path: 'databases', name: 'databases', component: Databases, meta: { title: '数据库', roles: ['admin'] } },
        // Redis 在线管理:SCAN 游标分页键浏览 + 键详情 + 删键/改 TTL(仅管理员)。
        { path: 'redis', name: 'redis', component: Redis, meta: { title: 'Redis', roles: ['admin'] } },
        // 服务器磁盘管理:lsblk 设备树 + LVM 面板 + 分区/格式化/挂载/扩缩容(仅管理员)。
        { path: 'disks', name: 'disks', component: Disks, meta: { title: 'Disks', roles: ['admin'] } },
        // 服务器文件查看:目录浏览 + 文件下载 + ≤64KiB 纯文本在线查看/编辑(仅管理员)。
        { path: 'files', name: 'files', component: Files, meta: { title: 'Files', roles: ['admin'] } },
        // 流水线产物管理:跨运行产物(文件/镜像)列表 + 查看/下载/删除(仅管理员)。
        { path: 'artifacts', name: 'artifacts', component: Artifacts, meta: { title: 'Artifacts', roles: ['admin'] } },
        // PVE 集群管理:总览/VM 列表/电源操作/任务(仅管理员;集群经服务器登记自动发现)。
        { path: 'pve', name: 'pve', component: PVE, meta: { title: 'PVE', roles: ['admin'] } },
        // 对象存储浏览(只读;仅管理员):实例在 设置 → 对象存储 注册,此处浏览桶内目录(marker 游标分页)。
        { path: 'object-storage', name: 'object-storage', component: ObjectStorage, meta: { title: '对象存储', roles: ['admin'] } },
        // R2 / E2.4: 证书总览大盘(跨主机跨域名 + 到期高亮)
        { path: 'certificates', name: 'cert-mgmt', component: Certificates, meta: { title: '证书管理' } },
        // 旧「证书总览」(/proxy)书签兼容:重定向到证书管理。
        { path: 'proxy', redirect: { name: 'cert-mgmt' } },
        // Story 6-5: configurable anomaly detection & alerts (FR-23)
        { path: 'anomaly', name: 'anomaly', component: AnomalyDetection, meta: { title: '异常检测' } },
        // 机器监控:ssar(自维护 fork)安装分发 + 平台采样总览 + 机器侧细粒度历史(仅管理员)。
        { path: 'ssar-monitor', name: 'ssar-monitor', component: SsarMonitor, meta: { title: '机器监控', roles: ['admin'] } },
        // 机器监控·单机详情:总览图表 + 细粒度按需查询(从列表页点机器进入)。
        { path: 'ssar-monitor/:id', name: 'ssar-monitor-detail', component: SsarMonitorDetail, meta: { title: '机器监控详情', roles: ['admin'] } },
        // 顶层「通知」占位页 → 重定向到真实的通知配置页。
        { path: 'notifications', name: 'notifications', redirect: { name: 'settings-notifications' } },
        {
          path: 'settings',
          name: 'settings',
          component: Settings,
          meta: { title: '设置' },
          children: [
            { path: '', redirect: { name: 'settings-ai' } },
            { path: 'ai', name: 'settings-ai', component: SettingsAI, meta: { title: 'AI 设置' } },
            { path: 'oauth', name: 'settings-oauth', component: SettingsOAuth, meta: { title: 'OAuth 设置' } },
            { path: 'notifications', name: 'settings-notifications', component: SettingsNotifications, meta: { title: '通知设置' } },
            { path: 'vault', name: 'settings-vault', component: SettingsVault, meta: { title: '凭据保险库' } },
            // R3 / E3.1: DNS 提供商(DNS-01 通配符 + 一键分配子域名)
            { path: 'dns-providers', name: 'settings-dns-providers', component: SettingsDnsProviders, meta: { title: 'DNS 提供商' } },
            // 对象存储(流水线 OSS 上传节点的目标存储)
            { path: 'object-storage', name: 'settings-object-storage', component: SettingsObjectStorage, meta: { title: '对象存储' } },
            // 数据库实例注册(MySQL/PostgreSQL;应用市场部署后自动登记;管理在 /databases)
            { path: 'databases', name: 'settings-databases', component: SettingsDatabases, meta: { title: '数据库' } },
            // Redis 实例注册(应用市场部署 redis 后自动登记;管理在 /redis)
            { path: 'redis', name: 'settings-redis', component: SettingsRedis, meta: { title: 'Redis' } },
            // PVE 集群 Token 配置(集群经服务器登记自动发现;管理在 /pve)
            { path: 'pve', name: 'settings-pve', component: SettingsPVE, meta: { title: 'PVE' } },
            { path: 'https', name: 'settings-https', component: SettingsHttps, meta: { title: 'HTTPS 访问' } },
            { path: 'account', name: 'settings-account', component: SettingsAccount, meta: { title: '账户设置' } },
            // 系统信息 + 一键检查更新
            { path: 'system', name: 'settings-system', component: SettingsSystem, meta: { title: '系统信息' } },
            // Story 4-1: target servers + shared SSH layer (FR-14)
            { path: 'servers', name: 'settings-servers', component: SettingsServers, meta: { title: '服务器' } },
            // 阿里云 ECS 批量导入(账户配置 + 导入向导;导入的机器进「服务器」)
            { path: 'aliyun-ecs', name: 'settings-aliyun-ecs', component: SettingsAliyunEcs, meta: { title: '阿里云 ECS' } },
            // 集群:本节点身份/EasyTier 组网/授权单(默认全拒绝)/节点注册表
            { path: 'mesh', name: 'settings-mesh', component: SettingsMesh, meta: { title: '集群' } },
            // 集群级共享构建机:跨环境共享构建任务(owner 视角)+ 共享机总览
            { path: 'shared-build', name: 'settings-shared-build', component: SettingsSharedBuild, meta: { title: '共享构建机' } },
            // 内置本地 Docker registry:构建产物统一推送 + 上游可切换 + daemon.json 下发
            { path: 'registry', name: 'settings-registry', component: SettingsRegistry, meta: { title: '镜像仓库' } },
            // Story 7-5: diagnosis feedback-loop stats (FR-26)
            { path: 'diagnosis-stats', name: 'settings-diagnosis-stats', component: SettingsDiagnosisStats, meta: { title: '诊断统计' } },
          ],
        },
      ],
    },
    // Fallback
    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
})

// ——— Route guard: session check with Pinia cache + role enforcement ———
//
// Error semantics:
//   kind:'ok'            → proceed (after role check)
//   kind:'unauthenticated' → redirect to /login (confirmed 401)
//   kind:'error'          → backend unreachable / 5xx:
//       • if user already had a cached session → stay on current page
//         (do NOT kick them to /login; the page should show a fault state)
//       • if session was never established → redirect to /login
//         (we can't display authenticated content anyway)
//
// Role enforcement mirrors the backend's default-deny memberGate:
//   - routes with meta.roles: the current role MUST be listed → else bounce home
//   - routes WITHOUT meta.roles: admins pass; members are denied (bounced home)
//     so any future admin-only page is automatically invisible to members.

/** 各角色首页:管理员 → 概览;普通用户 → 我的项目。 */
function homeFor(role: UserRole): string {
  return role === 'member' ? '/my-projects' : '/dashboard'
}

router.beforeEach(async (to) => {
  if (to.meta.public) return true

  // useSessionStore() must be called inside the guard (after pinia is installed)
  const sessionStore = useSessionStore()
  const result = await sessionStore.ensureSession()

  if (result.kind === 'unauthenticated') {
    return {
      name: 'login',
      query: { redirect: to.fullPath },
    }
  }

  if (result.kind === 'error') {
    // kind === 'error' (5xx / network)
    if (sessionStore.user !== undefined && sessionStore.user !== null) {
      // Had a previously confirmed session — let the navigation proceed;
      // the view can show a degraded/retry state via isNetworkError.
      return true
    }
    // Session was never established — can't show authenticated content
    return {
      name: 'login',
      query: { redirect: to.fullPath },
    }
  }

  // kind === 'ok' — enforce role-scoped visibility.
  const role: UserRole = result.user.role
  const roles = to.meta.roles as UserRole[] | undefined
  if (roles) {
    if (!roles.includes(role)) return { path: homeFor(role) }
  } else if (role === 'member') {
    // 默认拒绝:未显式声明 roles 的路由对普通用户不可见。
    return { path: homeFor(role) }
  }
  return true
})

// ——— 标签页标题:每个路由独立 document.title,多标签(尤其终端在新标签开)可区分 ———
// 静态标题来自 route.meta.title;数据相关页面(终端带服务器名、运行详情带 ID 等)由各
// 组件加载到数据后自行覆写 document.title,这里只兜底,避免出现一堆同名「Pipewright」。
export const APP_TITLE = 'Pipewright'

export function setDocumentTitle(part?: string): void {
  document.title = part ? `${part} · ${APP_TITLE}` : APP_TITLE
}

router.afterEach((to) => {
  const title = to.meta.title
  setDocumentTitle(typeof title === 'string' ? title : undefined)
})

export default router
