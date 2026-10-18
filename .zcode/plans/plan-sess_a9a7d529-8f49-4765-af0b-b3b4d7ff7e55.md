# 左栏菜单分组折叠机制

## 目标
`web/src/layouts/AppShell.vue` 中 22 个管理端菜单项目前纯平铺渲染（`adminNavItems`，L79-122），改为「顶层散项 + 5 个可折叠分组」，折叠状态持久化，当前路由所在组自动展开。普通用户（2 项菜单）不受影响，保持平铺。

## 已确认的决策
- **分组方案（按职能 5 组）**：
  - 顶层散项：概览（dashboard）、用户管理（users）
  - **交付** delivery：项目、运行、产物、复用库、DORA 指标
  - **基础设施** infra：服务器、集群、PVE 集群、容器
  - **存储与文件** storage：Ceph 存储、对象存储、磁盘、文件
  - **应用与数据** apps：应用商店、数据库、Redis、服务注册、证书管理
  - **监控与告警** monitoring：异常检测、通知
- **默认状态**：首次访问（localStorage 无记录）全部折叠；当前路由所在组强制展开并高亮组头
- **62px 收起态**：图标手风琴 —— 组头也是图标按钮，点击纵向展开组内图标（居中排列），与展开态共用同一折叠状态

## 实现步骤

### 1. 新建 `web/src/composables/useNavGroups.ts`
折叠状态逻辑抽成 composable（便于单测，避免重挂载 AppShell）：
- `NavItem` 接口从 AppShell 移到这里导出（或放共享类型），新增 `NavGroup { name, labelKey, icon, items }`
- localStorage key `pipewright_nav_collapsed_groups`，存折叠组名 JSON 数组；读取走 try/catch 静默降级（参考 `stores/theme.ts` 模式）；**无记录或解析失败 → 默认全部折叠**（初始 set 含全部组名）
- `isOpen(group) = !collapsed.has(group.name) || group.items.some(isActive)` —— 活动组强制展开；用户手动折叠活动组时本组仍保持可见，离开该组后自动回到折叠态（collapsed set 不变）
- `toggle(group)` 更新 set 并持久化

### 2. 改造 `AppShell.vue` script
- `adminNavItems` 拆为 `adminTopItems: NavItem[]`（dashboard、users）+ `adminNavGroups: NavGroup[]`（保留原条目的中文注释，随条目迁入对应组）
- 计算属性：`topItems = isAdmin ? adminTopItems : memberNavItems`、`navGroups = isAdmin ? adminNavGroups : []`（member 无分组）
- 接入 `useNavGroups`，`isActive` 复用现有实现（L152-157）

### 3. 改造模板（L181-194）
```
<ul class="nav-list">
  顶层散项 v-for（现有 nav-item 标记不变）
  <li v-for="group in navGroups" class="nav-group">
    <button class="nav-group-header" :aria-expanded="isOpen(group)"
            :class="{ 'nav-group-header--active': !isOpen(group) && 组内含活动项 }">
      <n-icon :component="group.icon" /> <span class="nav-label">{{ t(group.labelKey) }}</span>
      <chevron（随开合旋转 90°）/>
    </button>
    <ul v-show="isOpen(group)" class="nav-group-items"> 组内 nav-item v-for </ul>
  </li>
</ul>
```
组头为 `<button>`（原生可聚焦，`aria-expanded` 表达状态）；nav-item 标记复用现有写法（文件内 settings/logout 本就有同款重复，风格一致）。

### 4. 样式（沿用 `.is-expanded` 覆盖模式）
- 收起态（默认）：`.nav-group-header` 为 40×40 居中图标按钮；`.nav-label`、chevron 隐藏（复用 `.nav-label` 的 display:none → `.is-expanded` 显示模式）；组间用 `margin-top` 留间距；`.nav-group-items` 居中纵排
- 展开态（`.is-expanded` 前缀）：组头占满宽度（icon + label + chevron 靠右）；组内条目左缩进 ~10px 表达层级（活动指示条随之缩进，视觉自然）
- `.nav-group-header--active`：文字转 primary 色 + 收起态下贴左缘指示条（复用 `.nav-item--active::before` 的 `--rail-width` 回推算法）
- `.nav-list` 的 `flex:1; overflow-y:auto` 滚动机制不变

### 5. 图标
5 个组各配一个 @vicons/tabler 图标（如 delivery→`Route2`、infra→`Server2`、storage→`Archive`/`CloudDatabase`、apps→`Category`/`LayoutGrid`、monitoring→`Activity`），实现时以包内实际存在者为准，避免与条目图标完全撞款。

### 6. i18n（8 个 locale 文件 `nav` 块各加 5 个 key）
`groupDelivery` / `groupInfra` / `groupStorage` / `groupApps` / `groupMonitoring`
- zh-CN：交付 / 基础设施 / 存储与文件 / 应用与数据 / 监控与告警
- 其余 en/de/es/fr/ja/ko/zh-HK 按各自文件现有语气补齐

### 7. 测试
- **新增单测** `web/src/composables/useNavGroups.test.ts`（Vitest + localStorage stub，`src/test/setup.ts` 已提供）：默认全折叠、toggle 持久化、活动组强制 isOpen、脏 JSON 降级、无记录重置为全折叠
- **更新 E2E** `web/e2e/shell.spec.ts`：现有断言「项目/运行/服务器可见」在默认折叠下会失败 → 改为断言 5 个组头 + 概览/设置可见，点击「交付」组头展开后再断言「项目/运行」并完成原有导航点击测试；可加一条折叠状态刷新后保持的断言

### 8. 验证
`web/` 下依次：`npm run lint`、`npm run test`（vitest）、`npm run build`（含类型检查）；环境允许时跑 `npx playwright test e2e/shell.spec.ts`。

## 不改动的部分
- 底部固定的 设置/退出登录、整栏展开/收起（`pipewright_sidebar_expanded`）、路由与守卫、member 菜单、isActive 现有逻辑（含 `overview` 特判）
