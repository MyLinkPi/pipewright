<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import {
  Dashboard,
  GitBranch,
  GitFork,
  ChartBar,
  Server,
  Box,
  Certificate,
  AlertTriangle,
  Bell,
  Settings,
  Stack2,
  World,
  Apps,
  Database,
  CloudUpload,
  Bolt,
  DeviceFloppy,
  Folder,
  Package,
  Users,
  ChevronRight,
  Logout,
  Route,
  Sitemap,
  Archive,
  LayoutGrid,
  Activity,
  ReportAnalytics,
  Cpu,
  SwitchHorizontal,
  Shield,
} from '@vicons/tabler'
import { NIcon } from 'naive-ui'
import ThemeToggle from '../components/ThemeToggle.vue'
import { useNavGroups, type NavGroup, type NavItem } from '../composables/useNavGroups'
import { logout } from '../api/auth'
import { useSessionStore } from '../stores/session'
import { useConfirm } from '../composables/useConfirm'
import { useToast } from '../composables/useToast'

const route = useRoute()
const router = useRouter()
const sessionStore = useSessionStore()
const confirm = useConfirm()
const toast = useToast()
const { t } = useI18n()

// 退出登录:确认 → POST /api/auth/logout(吊销当前会话)→ 清本地会话缓存 → 回登录页。
// 即便后端请求失败也照样清缓存跳转(本地一定登出),避免卡在"看似已登录但实际无效"。
const loggingOut = ref(false)
async function handleLogout(): Promise<void> {
  if (loggingOut.value) return
  const ok = await confirm.open({
    title: t('shell.logoutTitle'),
    body: t('shell.logoutBody'),
    confirmLabel: t('nav.logout'),
    variant: 'danger',
  })
  if (!ok) return
  loggingOut.value = true
  try {
    await logout()
  } catch {
    // 后端不可达也要本地登出 —— 失败不阻塞跳转。
    toast.info(t('shell.logoutLocalTitle'), { detail: t('shell.logoutLocalDetail') })
  } finally {
    sessionStore.clearSession()
    loggingOut.value = false
    void router.push('/login')
  }
}

// 顶层散项:高频入口不参与分组折叠,始终直接可见。
const adminTopItems: NavItem[] = [
  { name: 'dashboard', to: '/dashboard', icon: Dashboard, labelKey: 'nav.dashboard', ariaKey: 'nav.dashboard' },
]

// 尾部散项:贴近底部设置区的管理入口。
const adminTrailingItems: NavItem[] = [
  // 用户体系:管理员用户 / 邀请 / 项目成员管理。
  { name: 'users', to: '/users', icon: Users, labelKey: 'nav.users', ariaKey: 'nav.users' },
]

// 管理端分组:组头可开合(默认收起;当前路由所在组强制展开,见 useNavGroups)。
const adminNavGroups: NavGroup[] = [
  {
    name: 'delivery',
    labelKey: 'nav.groupDelivery',
    icon: Route,
    items: [
      { name: 'projects', to: '/projects', icon: GitBranch, labelKey: 'nav.projects', ariaKey: 'nav.projects' },
      { name: 'runs', to: '/runs', icon: GitFork, labelKey: 'nav.runs', ariaKey: 'nav.runs' },
      // 流水线产物管理:跨运行产物(文件/镜像)查看/下载/删除(镜像导出 tar + 内置 registry tag 联动清理)。
      { name: 'artifacts', to: '/artifacts', icon: Package, labelKey: 'nav.artifacts', ariaKey: 'nav.artifacts' },
      // FR-8-13: 复用库(流水线模板 + 变量组)。
      { name: 'library', to: '/library', icon: Stack2, labelKey: 'nav.library', ariaKey: 'nav.library' },
      // 环境一等公民:按环境聚合的部署历史 + 一键回滚(对标 GitLab environments)。
      // FR-8-15: DORA 四指标仪表盘(交付效能;只读聚合)。
      { name: 'dora', to: '/metrics/dora', icon: ChartBar, labelKey: 'nav.dora', ariaKey: 'nav.dora' },
    ],
  },
  {
    name: 'infra',
    labelKey: 'nav.groupInfra',
    icon: Sitemap,
    items: [
      // Story 6-1: 多机状态总览(服务器层指标 FR-15);登记在 设置 → 服务器。
      { name: 'server-status', to: '/server-status', icon: Server, labelKey: 'nav.serverStatus', ariaKey: 'nav.serverStatus' },
      // 集群:对等节点总览(EasyTier 链路 p2p/中继、机器与流水线状态)。
      { name: 'cluster', to: '/cluster', icon: World, labelKey: 'nav.cluster', ariaKey: 'nav.cluster' },
      // PVE 集群管理:总览/VM/电源操作/任务(集群经服务器登记自动发现,Token 在 设置 → PVE)。
      { name: 'pve', to: '/pve', icon: Server, labelKey: 'nav.pve', ariaKey: 'nav.pve' },
      // 容器管理:跨服务器聚合容器总览 + 行内生命周期操作(docker over SSH)。
      { name: 'containers', to: '/containers', icon: Box, labelKey: 'nav.containers', ariaKey: 'nav.containers' },
      // PM2 管理:跨服务器聚合 pm2 进程总览 + 行内生命周期操作(pm2 over SSH)。
      { name: 'pm2', to: '/pm2', icon: Cpu, labelKey: 'nav.pm2', ariaKey: 'nav.pm2' },
      // SSH 隧道:local -L / remote -R / dynamic -D 配置与运行态(独立 helper,重启平台不断)。
      { name: 'tunnels', to: '/tunnels', icon: SwitchHorizontal, labelKey: 'nav.tunnels', ariaKey: 'nav.tunnels' },
    ],
  },
  {
    name: 'storage',
    labelKey: 'nav.groupStorage',
    icon: Archive,
    items: [
      // Ceph 集群:cephadm 部署向导 + 存储(主机/OSD/池/RBD/CephFS)管理。
      { name: 'ceph', to: '/ceph', icon: Database, labelKey: 'nav.ceph', ariaKey: 'nav.ceph' },
      // 对象存储浏览(只读):实例在 设置 → 对象存储 注册;桶内目录树(marker 游标分页)。
      { name: 'object-storage', to: '/object-storage', icon: CloudUpload, labelKey: 'nav.objectStorage', ariaKey: 'nav.objectStorage' },
      // 服务器磁盘管理:块设备/LVM 拓扑 + 分区/格式化/挂载/扩缩容(只读默认 + 显式解锁)。
      { name: 'disks', to: '/disks', icon: DeviceFloppy, labelKey: 'nav.disks', ariaKey: 'nav.disks' },
      // 服务器文件查看:目录浏览 + 文件下载 + ≤64KiB 纯文本在线查看/编辑(保存前同目录备份)。
      { name: 'files', to: '/files', icon: Folder, labelKey: 'nav.files', ariaKey: 'nav.files' },
    ],
  },
  {
    name: 'apps',
    labelKey: 'nav.groupApps',
    icon: LayoutGrid,
    items: [
      // 应用商店:MySQL/Redis 等模板参数一键部署(DPanel 式)。
      { name: 'app-store', to: '/app-store', icon: Apps, labelKey: 'nav.appStore', ariaKey: 'nav.appStore' },
      // 数据库在线管理(phpMyAdmin 式):实例在 设置 → 数据库 注册;应用市场部署 mysql/postgres 自动登记。
      { name: 'databases', to: '/databases', icon: Database, labelKey: 'nav.databases', ariaKey: 'nav.databases' },
      // Redis 在线管理:实例在 设置 → Redis 注册;应用市场部署 redis 自动登记;SCAN 分页键浏览。
      { name: 'redis', to: '/redis', icon: Bolt, labelKey: 'nav.redis', ariaKey: 'nav.redis' },
      // 服务注册网关(nginx):服务名 + 基域 → 子域名反代(abc.efg.com),证书由「证书管理」签发/同步。
      { name: 'service-reg', to: '/service-reg', icon: World, labelKey: 'nav.serviceReg', ariaKey: 'nav.serviceReg' },
      // 证书管理:acme.sh 自动签发/续期(DNS-01)+ 手动导入,签发引擎跑在网关主机上。
      { name: 'cert-mgmt', to: '/certificates', icon: Certificate, labelKey: 'nav.certificates', ariaKey: 'nav.certificates' },
    ],
  },
  {
    name: 'monitoring',
    labelKey: 'nav.groupMonitoring',
    icon: Activity,
    items: [
      // R4 / E4.1: PR 预览环境大盘(临时环境列表 + 手动回收)。
      // Story 6-5: configurable anomaly detection & alerts (FR-23)
      { name: 'anomaly', to: '/anomaly', icon: AlertTriangle, labelKey: 'nav.anomaly', ariaKey: 'nav.anomaly' },
      // 机器监控:ssar(自维护 fork)安装分发 + 平台采样总览 + 机器侧细粒度历史。
      { name: 'ssar-monitor', to: '/ssar-monitor', icon: ReportAnalytics, labelKey: 'nav.ssarMonitor', ariaKey: 'nav.ssarMonitor' },
      // 安全中心:Trivy/Nuclei 扫描 + 定时规则 + HTML 审计报告(仅管理员)。
      { name: 'security', to: '/security', icon: Shield, labelKey: 'nav.security', ariaKey: 'nav.security' },
      { name: 'notifications', to: '/settings/notifications', icon: Bell, labelKey: 'nav.notifications', ariaKey: 'nav.notifications' },
    ],
  },
]

// 普通用户导航(最小面:仅「我的项目 / 运行」;设置项单列在底部)。
const memberNavItems: NavItem[] = [
  { name: 'my-projects', to: '/my-projects', icon: GitBranch, labelKey: 'nav.myProjects', ariaKey: 'nav.myProjects' },
  { name: 'my-runs',     to: '/my-runs',     icon: GitFork,   labelKey: 'nav.runs',       ariaKey: 'nav.runs' },
]

const adminSettingsItem: NavItem = {
  name: 'settings',
  to: '/settings',
  icon: Settings,
  labelKey: 'nav.settings',
  ariaKey: 'nav.settings',
}

// 普通用户设置:独立页(不挂管理员 Settings 外壳)。
const memberSettingsItem: NavItem = {
  name: 'my-settings',
  to: '/my-settings',
  icon: Settings,
  labelKey: 'nav.mySettings',
  ariaKey: 'nav.mySettings',
}

// 角色感知导航:管理员见全量(散项 + 分组),普通用户仅见最小面(member,无分组)。
const isAdmin = computed(() => sessionStore.isAdmin)
const topItems = computed<NavItem[]>(() => (isAdmin.value ? adminTopItems : memberNavItems))
const trailingItems = computed<NavItem[]>(() => (isAdmin.value ? adminTrailingItems : []))
const navGroups = computed<NavGroup[]>(() => (isAdmin.value ? adminNavGroups : []))
const settingsItem = computed<NavItem>(() => (isAdmin.value ? adminSettingsItem : memberSettingsItem))

function isActive(item: NavItem): boolean {
  if (item.name === 'overview') {
    return route.name === 'overview'
  }
  return route.path.startsWith(item.to)
}

function hasActiveItem(group: NavGroup): boolean {
  return group.items.some(isActive)
}

// 分组折叠:默认全收、手动开合记忆到 localStorage;当前路由所在组强制展开(useNavGroups)。
const { isOpen, toggle: toggleGroup } = useNavGroups(
  adminNavGroups.map((group) => group.name),
  isActive,
)

// 侧栏展开/收起:固定按钮切换,状态持久化到 localStorage(刷新/重开保持)。
const STORAGE_KEY = 'pipewright_sidebar_expanded'
const expanded = ref(localStorage.getItem(STORAGE_KEY) === '1')
function toggleExpanded(): void {
  expanded.value = !expanded.value
  localStorage.setItem(STORAGE_KEY, expanded.value ? '1' : '0')
}
</script>

<template>
  <div class="app-shell" :class="{ 'is-expanded': expanded }">
    <!-- Left rail navigation -->
    <nav class="rail" :aria-label="t('nav.ariaMain')">
      <!-- 顶部:品牌 -->
      <div class="rail-head">
        <router-link to="/" class="brand" :aria-label="t('nav.ariaBrandHome')">
          <span class="brand-mark mono">p&gt;</span>
          <span class="brand-name">Pipewright</span>
        </router-link>
      </div>

      <!-- Primary navigation items -->
      <ul class="nav-list" role="list">
        <!-- 顶层散项:不经分组折叠,始终直接可见(概览 / member 最小面)。 -->
        <li v-for="item in topItems" :key="item.name">
          <router-link
            :to="item.to"
            class="nav-item"
            :class="{ 'nav-item--active': isActive(item) }"
            :aria-label="t(item.ariaKey)"
            :aria-current="isActive(item) ? 'page' : undefined"
          >
            <n-icon class="nav-icon" :component="item.icon" :size="20" />
            <span class="nav-label">{{ t(item.labelKey) }}</span>
          </router-link>
        </li>

        <!-- 分组:组头开合(收起态为图标手风琴);当前路由所在组强制展开 -->
        <li v-for="group in navGroups" :key="group.name" class="nav-group">
          <button
            type="button"
            class="nav-group-header"
            :class="{ 'nav-group-header--active': hasActiveItem(group) }"
            :aria-expanded="isOpen(group)"
            :aria-label="t(group.labelKey)"
            @click="toggleGroup(group)"
          >
            <n-icon class="nav-icon" :component="group.icon" :size="18" />
            <span class="nav-group-title">{{ t(group.labelKey) }}</span>
            <n-icon class="nav-group-chevron" :class="{ open: isOpen(group) }" :component="ChevronRight" :size="14" />
          </button>
          <ul v-show="isOpen(group)" class="nav-group-items" role="list">
            <li v-for="item in group.items" :key="item.name">
              <router-link
                :to="item.to"
                class="nav-item"
                :class="{ 'nav-item--active': isActive(item) }"
                :aria-label="t(item.ariaKey)"
                :aria-current="isActive(item) ? 'page' : undefined"
              >
                <n-icon class="nav-icon" :component="item.icon" :size="20" />
                <span class="nav-label">{{ t(item.labelKey) }}</span>
              </router-link>
            </li>
          </ul>
        </li>

        <!-- 尾部散项:贴近底部设置区的管理入口(用户管理)。 -->
        <li v-for="item in trailingItems" :key="item.name">
          <router-link
            :to="item.to"
            class="nav-item"
            :class="{ 'nav-item--active': isActive(item) }"
            :aria-label="t(item.ariaKey)"
            :aria-current="isActive(item) ? 'page' : undefined"
          >
            <n-icon class="nav-icon" :component="item.icon" :size="20" />
            <span class="nav-label">{{ t(item.labelKey) }}</span>
          </router-link>
        </li>
      </ul>

      <!-- Spacer -->
      <div class="rail-spacer" aria-hidden="true" />

      <!-- Settings (bottom) -->
      <router-link
        :to="settingsItem.to"
        class="nav-item"
        :class="{ 'nav-item--active': isActive(settingsItem) }"
        :aria-label="t(settingsItem.ariaKey)"
        :aria-current="isActive(settingsItem) ? 'page' : undefined"
      >
        <n-icon class="nav-icon" :component="settingsItem.icon" :size="20" />
        <span class="nav-label">{{ t(settingsItem.labelKey) }}</span>
      </router-link>

      <!-- Logout (bottom-most) -->
      <button
        type="button"
        class="nav-item nav-item--logout"
        :disabled="loggingOut"
        :aria-label="t('nav.logout')"
        @click="handleLogout"
      >
        <n-icon class="nav-icon" :component="Logout" :size="20" />
        <span class="nav-label">{{ t('nav.logout') }}</span>
      </button>
    </nav>

    <!-- 边缘切换:骑在侧栏右缘、与品牌齐平的圆形按钮(随 --rail-width 平移)。 -->
    <button
      class="rail-edge-toggle"
      type="button"
      :aria-label="expanded ? t('nav.collapse') : t('nav.expand')"
      :aria-pressed="expanded"
      @click="toggleExpanded"
    >
      <n-icon class="toggle-chevron" :class="{ flipped: expanded }" :component="ChevronRight" :size="16" />
    </button>

    <!-- Main content area -->
    <main class="main-area" id="main-content">
      <!-- 全幅页面(如流水线编辑器):route.meta.fullBleed 时放开 --content-max,内容吃满主区宽度。 -->
      <div class="main-inner" :class="{ 'main-inner--full': route.meta.fullBleed === true }">
        <router-view />
      </div>
    </main>

    <!-- Theme toggle (bottom-right, always visible) -->
    <ThemeToggle />
  </div>
</template>

<style scoped>
.app-shell {
  display: grid;
  /* minmax(0,1fr) 而非 1fr:1fr 的最小尺寸是 min-content,会被内部超宽元素(如终端里不换行的
     超长日志行)撑到比窗口还宽 → 整页横向溢出、卡片顶出右边。minmax(0,…) 允许主区收缩到窗口宽度,
     超长内容只在各自容器内(终端横向滚动)处理,布局自适应窗口。 */
  grid-template-columns: var(--rail-width) minmax(0, 1fr);
  min-height: 100vh;
  transition: grid-template-columns var(--duration-normal) var(--ease-out-expo, ease);
}
/* 展开态:覆盖 --rail-width,grid 主区与 active 指示条偏移随之自适应。 */
.app-shell.is-expanded {
  --rail-width: 212px;
}

/* ——— Rail ——— */
.rail {
  position: sticky;
  top: 0;
  height: 100vh;
  display: flex;
  flex-direction: column;
  align-items: center;
  padding: 20px 0;
  gap: 6px;
  border-right: 1px solid var(--color-border);
  background-color: var(--color-bg);
  z-index: 10;
  overflow: hidden;
}
.is-expanded .rail {
  align-items: stretch;
  padding: 20px 12px;
}

/* 顶部头区:品牌 + 切换。收起态纵向叠放(logo 上、箭头下);展开态横向一行(品牌左、箭头右)。 */
.rail-head {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
  margin-bottom: 18px;
  flex-shrink: 0;
}
.is-expanded .rail-head {
  flex-direction: row;
  align-items: center;
  gap: 8px;
  padding-left: 4px;
}

/* Brand */
.brand {
  display: flex;
  align-items: center;
  gap: 10px;
  text-decoration: none;
  flex-shrink: 0;
}
.brand-mark {
  width: 30px;
  height: 30px;
  border-radius: var(--rounded);
  background: var(--color-primary);
  color: #fff;
  display: grid;
  place-items: center;
  font-family: var(--font-mono);
  font-weight: 700;
  font-size: 0.78rem;
  box-shadow: 0 4px 14px var(--color-primary-soft);
  transition: box-shadow var(--duration-fast);
  flex-shrink: 0;
}
.brand:hover .brand-mark {
  box-shadow: 0 6px 20px var(--color-primary-soft);
}
.brand-name {
  font-size: var(--text-body);
  font-weight: 700;
  letter-spacing: -0.01em;
  color: var(--color-text);
  white-space: nowrap;
  overflow: hidden;
  /* 收起时隐藏文字(无障碍名仍由 brand 的 aria-label 提供)。 */
  display: none;
}
.is-expanded .brand-name {
  display: inline;
}

/* Nav list */
.nav-list {
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: 4px;
  width: 100%;
  align-items: center;
  /* 主菜单占据 head 与底部固定项之间的弹性空间;菜单项过多时自身滚动,
     绝不挤压(或被 .rail overflow:hidden 裁掉)底部的「设置 / 退出登录」。
     min-height:0 让 flex 子项能收缩到内容高度以下,overflow 才会生效。 */
  flex: 1 1 auto;
  min-height: 0;
  overflow-y: auto;
  overflow-x: hidden;
  scrollbar-width: thin;
}
.is-expanded .nav-list {
  align-items: stretch;
}

/* Nav item */
.nav-item {
  position: relative;
  width: 40px;
  height: 40px;
  border-radius: var(--rounded);
  display: grid;
  place-items: center;
  color: var(--color-faint);
  text-decoration: none;
  background: none;
  border: none;
  cursor: pointer;
  transition:
    color var(--duration-fast),
    background-color var(--duration-fast);
}
.is-expanded .nav-item {
  width: 100%;
  display: flex;
  align-items: center;
  justify-content: flex-start;
  gap: 12px;
  padding: 0 12px;
}
.nav-icon {
  flex-shrink: 0;
}
.nav-item:hover {
  color: var(--color-text);
  background-color: var(--color-border);
}
.nav-item:focus-visible {
  outline: 2px solid var(--color-primary);
  outline-offset: 2px;
}

/* 退出登录:button 而非 link,补 font 继承;hover 转危险色与设置项区分。 */
.nav-item--logout {
  font: inherit;
}
.nav-item--logout:hover {
  color: var(--color-red);
  background-color: var(--color-red-soft);
}
.nav-item--logout:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}

.nav-label {
  font-size: var(--text-label);
  font-weight: 500;
  white-space: nowrap;
  overflow: hidden;
  display: none;
}
.is-expanded .nav-label {
  display: inline;
}

/* Active state */
.nav-item--active {
  color: var(--color-primary);
}
.is-expanded .nav-item--active {
  background: var(--color-primary-soft);
  font-weight: 600;
}
.nav-item--active::before {
  content: "";
  position: absolute;
  /* 收起态:指示条贴在 rail 左缘(图标水平居中,据 rail 宽度回推偏移)。 */
  left: calc(-1 * (var(--rail-width) / 2 - 20px + 1px));
  top: 9px;
  bottom: 9px;
  width: 2.5px;
  border-radius: 2px;
  background: var(--color-primary);
}
/* 展开态:指示条贴在条目自身左缘(条目左对齐、占满宽度)。 */
.is-expanded .nav-item--active::before {
  left: 0;
  top: 6px;
  bottom: 6px;
}

/* ——— Nav groups(分组折叠)——— */
.nav-group {
  width: 100%;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 4px;
  /* 组间留白:与上一区块拉开距离,收起态的图标堆里也能分辨组界。 */
  margin-top: 6px;
}
.is-expanded .nav-group {
  align-items: stretch;
}

/* 组头:收起态与 nav-item 同为 40×40 图标钮(手风琴);展开态一行(icon + 标题 + chevron)。 */
.nav-group-header {
  position: relative;
  width: 40px;
  height: 40px;
  border-radius: var(--rounded);
  display: grid;
  place-items: center;
  color: var(--color-faint);
  background: none;
  border: none;
  padding: 0;
  font: inherit;
  cursor: pointer;
  transition:
    color var(--duration-fast),
    background-color var(--duration-fast);
}
.is-expanded .nav-group-header {
  width: 100%;
  display: flex;
  align-items: center;
  justify-content: flex-start;
  gap: 12px;
  padding: 0 12px;
}
.nav-group-header:hover {
  color: var(--color-text);
  background-color: var(--color-border);
}
.nav-group-header:focus-visible {
  outline: 2px solid var(--color-primary);
  outline-offset: 2px;
}

/* 组头与条目的层级区分:标题加粗(图标在模板中已小一号 18 vs 20)。 */
.nav-group-title {
  font-size: var(--text-label);
  font-weight: 600;
  letter-spacing: 0.01em;
  white-space: nowrap;
  overflow: hidden;
  display: none;
}
.is-expanded .nav-group-title {
  display: inline;
}

/* 开合箭头:仅展开态显示(收起态没空间),展开组旋转 90° 朝下。 */
.nav-group-chevron {
  display: none;
  margin-left: auto;
  flex-shrink: 0;
  transition: transform var(--duration-fast) var(--ease-out-expo, ease);
}
.is-expanded .nav-group-chevron {
  display: inline-flex;
}
.nav-group-chevron.open {
  transform: rotate(90deg);
}

/* 组内条目:收起态居中纵排;展开态整体左缘缩进 10px 表达层级。
   width:auto 让条目在 stretch 布局下按可用宽计算(否则 100% + margin 会溢出右缘)。 */
.nav-group-items {
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: 4px;
  width: 100%;
  align-items: center;
}
.is-expanded .nav-group-items {
  align-items: stretch;
}
.is-expanded .nav-group-items .nav-item {
  width: auto;
  margin-left: 10px;
}

/* 组内含当前页:组头转主色提示方位(强指示仍留给活动条目本身)。 */
.nav-group-header--active {
  color: var(--color-primary);
}

/* 边缘切换:骑在侧栏右缘、与品牌行齐平的圆形按钮。随 --rail-width 平移(展开/收起都跟手)。
   fixed 而非 absolute:定位上下文是 .app-shell(整页高)时按钮会随窗口滚动跑掉,
   而侧栏本体是 sticky 的;fixed 相对视口,才能始终与品牌行齐平。--rail-width 仍从
   .app-shell 继承,展开/收起的平移动画不受影响。 */
.rail-edge-toggle {
  position: fixed;
  top: 23px; /* 与顶部品牌 mark 垂直居中对齐 */
  left: calc(var(--rail-width) - 13px); /* 圆心落在 rail 右缘分割线上 */
  width: 26px;
  height: 26px;
  border-radius: 50%;
  display: grid;
  place-items: center;
  background: var(--color-card);
  border: 1px solid var(--color-border);
  color: var(--color-dim);
  cursor: pointer;
  z-index: 30;
  box-shadow: 0 2px 8px -2px rgba(0, 0, 0, 0.14);
  transition:
    left var(--duration-normal) var(--ease-out-expo, ease),
    color var(--duration-fast),
    border-color var(--duration-fast),
    box-shadow var(--duration-fast);
}
.rail-edge-toggle:hover {
  color: var(--color-primary);
  border-color: var(--color-primary);
  box-shadow: 0 4px 12px -2px var(--color-primary-soft);
}
.rail-edge-toggle:focus-visible {
  outline: 2px solid var(--color-primary);
  outline-offset: 2px;
}
.toggle-chevron {
  transition: transform var(--duration-normal) var(--ease-out-expo, ease);
}
.toggle-chevron.flipped {
  transform: rotate(180deg);
}

.rail-spacer {
  /* nav-list 现已 flex:1 占满中间并把底部项推到底;spacer 不再伸展
     (否则会与 nav-list 平分空间、把菜单区压到一半)。保留为零高占位。 */
  flex: 0 0 0;
}

/* ——— Main area ——— */
.main-area {
  min-height: 100vh;
  /* 用 clip 而非 hidden:hidden 会让 overflow-y 被规范强制计算成 auto,使 .main-area 变成一个
     「装不下却又无法滚动」的隐性滚动容器,劫持滚轮/scrollIntoView,导致长页面(如运行详情的
     日志终端)滑不到底。clip 只裁横向、不改纵向(保持 visible),整页交给 window 单一滚动。 */
  overflow-x: clip;
}

.main-inner {
  max-width: var(--content-max);
  margin: 0 auto;
  padding: var(--main-pad-top) var(--main-pad) var(--main-pad-bottom);
  min-height: 100vh;
}

/* 全幅工作面板(流水线编辑器等,route.meta.fullBleed):取消阅读型页面的宽度上限,
   让 DAG 画布等横向空间敏感的区域吃满宽屏;保留左右内边距作为与侧栏的呼吸位。 */
.main-inner--full {
  max-width: none;
}
</style>
