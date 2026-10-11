# /runs 页面增加项目筛选

## 概要(Summary)

在 `/runs` 运行列表页的状态筛选之外,新增**项目筛选**:一排横向标签展示项目列表的前
N 个(默认 6 个,作为高频快捷入口)+ 一个下拉选择器兜底(列出全部项目)。二者与既有状态
筛选**组合生效**(AND),筛选结果由后端 `GET /api/runs?projectId=&status=` 返回。

后端**零改动**:`projectId` 筛选项在 [service.go](file:///c:/Users/xufan/Trae/pipewright/internal/run/service.go#L583-L592)
(`WHERE pr.project_id = ?`)与 [runs.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/runs.go#L227-L231)
已实现并有测试([runs\_test.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/runs_test.go#L127));
前端 [runs.ts](file:///c:/Users/xufan/Trae/pipewright/web/src/api/runs.ts#L194-L211) 的 `ListRunsParams`
也已有 `projectId`,尚无 UI 调用。

## 现状分析(Current State)

* [Runs.vue](file:///c:/Users/xufan/Trae/pipewright/web/src/views/Runs.vue) 只有状态筛选:
  `statusFilter` ref + `.filter-bar` 里的 `.filter-tab` 按钮组(第 335-345 行);
  [loadRuns()](file:///c:/Users/xufan/Trae/pipewright/web/src/views/Runs.vue#L55-L75) 仅按 `statusFilter`
  构造 `ListRunsParams`,**未传** `projectId`。

* 项目列表已在同文件中加载(供手动触发弹窗):`projects` ref +
  [loadProjectsForTrigger()](file:///c:/Users/xufan/Trae/pipewright/web/src/views/Runs.vue#L100-L110),懒加载(打开弹窗才请求)。

* 已有现成的「项目下拉」范式可复用:[DoraDashboard.vue](file:///c:/Users/xufan/Trae/pipewright/web/src/views/DoraDashboard.vue#L144-L158)
  用 [AppSelect.vue](file:///c:/Users/xufan/Trae/pipewright/web/src/components/ui/AppSelect.vue)(options 首项为
  `{ value: '', label: 全部项目 }`)。

* `listProjects()`([projects.ts](file:///c:/Users/xufan/Trae/pipewright/web/src/api/projects.ts#L101-L103))
  返回 `Project[]`,后端按 `created_at DESC` 排序、默认最多 50 条 —— **无运行频次数据**,
  故按用户确认取「列表前 N 个」。

* i18n 为 8 语言严格键对齐([keyParity.test.ts](file:///c:/Users/xufan/Trae/pipewright/web/src/i18n/keyParity.test.ts)),
  且组件静态 `t()` key 必须在 zh-CN 存在([componentKeys.test.ts](file:///c:/Users/xufan/Trae/pipewright/web/src/i18n/componentKeys.test.ts));
  `runs` 命名空间在 `web/src/i18n/locales/<lang>/runs.ts`(共 8 份)。

* e2e 在 [runs.spec.ts](file:///c:/Users/xufan/Trae/pipewright/web/e2e/runs.spec.ts),用 `page.route` 打桩
  (`stubLoggedIn` / `stubEnvelopeDefaults` 已打桩 `/api/projects`、`/api/runs`;后注册的路由优先)。

## 修改方案(Proposed Changes)

### 1. `web/src/views/Runs.vue`(核心改动)

**a. 新增项目筛选状态与派生数据**(放在 `// ─── filters ───` 区):

```ts
const projectFilter = ref<string>('')          // '' = 全部项目
const TOP_PROJECT_TAGS = 6                     // 横向标签最多展示的项目数

const topProjects = computed(() => projects.value.slice(0, TOP_PROJECT_TAGS))
const projectFilterOptions = computed(() => [
  { value: '', label: t('runs.allProjects') },
  ...projects.value.map((p) => ({ value: p.id, label: p.name })),
])
// 是否有任一筛选生效(供空状态文案/清除按钮判断)
const hasFilter = computed(() => statusFilter.value !== 'all' || projectFilter.value !== '')

function selectProject(id: string): void {
  projectFilter.value = id
  onFilterChange()
}
function clearFilters(): void {
  statusFilter.value = 'all'
  projectFilter.value = ''
  onFilterChange()
}
```

**b.** **`loadRuns()`** **带上** **`projectId`**(在 status 之后追加一行):

```ts
if (projectFilter.value) params.projectId = projectFilter.value
```

**c. 项目列表改为页面加载时即取**:把现有 `loadProjectsForTrigger` 的取数逻辑抽成
`loadProjects()`(带「已加载则不重复请求」的早退),`onMounted` 时调用;`loadProjectsForTrigger`
改为直接调用 `loadProjects()`(保持触发弹窗行为不变):

```ts
async function loadProjects(): Promise<void> {
  if (projects.value.length > 0) return          // 已加载
  projectsLoading.value = true
  try { projects.value = await listProjects() } catch { /* 非致命:筛选区隐藏 */ }
  finally { projectsLoading.value = false }
}

onMounted(() => { void loadProjects(); void loadRuns() })
```

**d. 模板:在状态** **`.filter-bar`(第 335-345 行)之前插入项目筛选区**:

```html
<div v-if="projects.length" class="project-filter">
  <div class="filter-bar" role="group" :aria-label="t('runs.projectFilterAria')">
    <button type="button" class="filter-tab project-tag"
      :class="{ 'filter-tab--active': projectFilter === '' }"
      @click="selectProject('')">{{ t('runs.allProjects') }}</button>
    <button v-for="p in topProjects" :key="p.id" type="button" class="filter-tab project-tag"
      :class="{ 'filter-tab--active': projectFilter === p.id }"
      @click="selectProject(p.id)">{{ p.name }}</button>
  </div>
  <span class="project-filter__select">
    <AppSelect
      :model-value="projectFilter"
      :options="projectFilterOptions"
      :placeholder="t('runs.allProjects')"
      :aria-label="t('runs.projectFilterAria')"
      min-width="180px"
      height="30px"
      @update:model-value="selectProject"
    />
  </span>
</div>
```

* 新增 `import AppSelect from '../components/ui/AppSelect.vue'`。

**e. 空状态与清除按钮关联修改**(第 382-401 行):把 `v-if="statusFilter !== 'all'"` 两处
改为 `v-if="hasFilter"`,清除按钮 `@click` 改为 `clearFilters()`(同时重置两个筛选)。
`emptyFiltered` 文案沿用。

**f. 样式**(scoped `<style>` 内新增):

```css
.project-filter { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.project-tag { max-width: 140px; overflow: hidden; text-overflow: ellipsis; }
.project-filter__select { display: inline-flex; width: 200px; flex: none; }
```

(用外层 `span` 控制宽度,不依赖 AppSelect 的 class 透传。)

### 2. i18n:8 个 `runs.ts` 各新增 2 个键

`web/src/i18n/locales/{zh-CN,zh-HK,en,ja,ko,es,fr,de}/runs.ts`,新增:

| key                 | zh-CN | zh-HK | en                | ja          | ko        | es                   | fr                 | de                   |
| ------------------- | ----- | ----- | ----------------- | ----------- | --------- | -------------------- | ------------------ | -------------------- |
| `allProjects`       | 全部项目  | 全部專案  | All projects      | すべてのプロジェクト  | 모든 프로젝트   | Todos los proyectos  | Tous les projets   | Alle Projekte        |
| `projectFilterAria` | 按项目筛选 | 依專案篩選 | Filter by project | プロジェクトで絞り込み | 프로젝트로 필터링 | Filtrar por proyecto | Filtrer par projet | Nach Projekt filtern |

(后 6 种语言的译文对齐既有 `doraDashboard.ts` 中同名文案,保证一致。)

* `allProjects` 同时用作标签首项文案与下拉 placeholder。

* 8 份键集必须一致,否则 `keyParity.test.ts` 判红;组件静态 `t('runs.allProjects')` /
  `t('runs.projectFilterAria')` 必须在 zh-CN 存在,否则 `componentKeys.test.ts` 判红。

### 3. 测试:`web/e2e/runs.spec.ts` 新增用例

在 `describe('Runs list')` 内新增一条(与既有「status filter re-queries」用例并列):

```ts
test('project filter re-queries the API with the chosen projectId', async ({ page }) => {
  await stubLoggedIn(page)
  await stubEnvelopeDefaults(page)
  let lastProjectId = ''
  await page.route((u) => u.pathname === '/api/projects',
    (route) => route.fulfill(fulfillJson([
      projectFixture({ id: 'p-a', name: 'acme-web' }),
      projectFixture({ id: 'p-b', name: 'billing-svc' }),
    ])))
  await page.route((u) => u.pathname === '/api/runs', (route) => {
    lastProjectId = new URL(route.request().url()).searchParams.get('projectId') ?? ''
    return route.fulfill(fulfillJson({ items: [], page: 1, total: 0 }))
  })
  await page.goto('/runs')

  await page.getByRole('group', { name: '按项目筛选' })
    .getByRole('button', { name: 'acme-web' }).click()
  await expect.poll(() => lastProjectId).toBe('p-a')
})
```

需从 `_stubs` 追加 import `projectFixture`(已存在该 fixture)。

#### 组件级测试(沙箱内可运行):`web/src/views/Runs.test.ts`(新增)

> 说明:Playwright e2e 在本机沙箱中无法运行(Chromium 被限制写入
> `%LOCALAPPDATA%\ms-playwright\...\debug.log`,`npm run e2e` 全部用例失败,
> 含改动前既有用例,属环境限制而非代码问题)。故补一份**组件级**回归,
> 沿用 [ProjectPipeline.test.ts](file:///c:/Users/xufan/Trae/pipewright/web/src/views/ProjectPipeline.test.ts)
> 范式(`vi.mock` API + `@vue/test-utils`;i18n 由 `src/test/setup.ts` 全局安装 zh-CN)。

覆盖 5 条:
1. 初次加载 `listRuns` 不带 `projectId`;点击项目标签后带上 `{ projectId: 'p-a' }`。
2. 项目数 > 6 时横向标签只渲染前 6 个 + 下拉兜底(`AppSelect` options 含全部项目 + 首项 `''`)。
3. 下拉选择不在标签中的项目 → `listRuns` 带该 `projectId`。
4. 无匹配结果时「清除筛选」同时重置项目筛选(`listRuns` 回到 `{}`,「全部项目」标签重新高亮)。
5. 无项目时项目筛选区不渲染。

### 4. 文档

本文件即为该功能的设计/实施说明(与 `pipeline-log-command-echo.md`、`deploy-max-targets-limit.md` 同格式)。

## 假设与决策(Assumptions & Decisions)

* **后端不改**:`projectId` 筛选已实现并有测试,本次纯前端。

* **「高频项目」= 项目列表前 6 个**(用户确认取最简方案);`listProjects()` 按
  `created_at DESC`,故实际是「最近创建的前 6 个」。不做运行次数统计。

* **筛选不同步 URL**(用户确认):与现有 `statusFilter` 行为一致,刷新回到「全部」。

* **单选**:后端仅支持单个 `projectId`,不做多选。

* **组合关系**:项目筛选与状态筛选 AND。

* **降级**:项目列表加载失败 → `projects` 为空 → 项目筛选区 `v-if` 隐藏,仅状态筛选可用;
  手动触发弹窗行为不变(失败时下拉为空)。

* **N = 6** 定义为常量 `TOP_PROJECT_TAGS`,便于后续调整。

* 下拉选择不在前 6 个标签中的项目时,标签无高亮、下拉显示该项目名(状态一致,不额外处理)。

## 验证步骤(Verification)

1. 类型检查:`cd web; npm run typecheck` ✅ 通过
2. 单测(i18n key parity / component keys / 组件回归):`cd web; npm run test` ✅ 58 文件 / 546 用例全绿
   - 仅跑本功能回归:`cd web; npx vitest run src/views/Runs.test.ts` ✅ 5/5
3. 生产构建:`cd web; npm run build` ✅ 通过
4. e2e(含新增用例):`cd web; npm run e2e -- runs.spec.ts` —— ⚠️ 本机沙箱限制 Chromium,
   需在非沙箱环境执行(用例已按计划补齐,可在本地/CI 验证)
5. 手动核对:

   * 进入 `/runs`,项目标签与下拉出现;点标签 → 请求带 `projectId`;

   * 下拉选一个不在标签里的项目 → 列表按该项目过滤;

   * 同时选状态 + 项目 → 两个参数都带上;

   * 无匹配结果 → 显示「没有符合当前筛选条件的运行。」;点「清除筛选」→ 两项一起重置;

   * 项目数为 0 时项目筛选区不显示。

## 完成清单(Checklist)

* [x] `Runs.vue`:新增 `projectFilter`/`topProjects`/`projectFilterOptions`/`hasFilter`、
  `loadProjects`、`selectProject`/`clearFilters`;`loadRuns` 带 `projectId`;
  `onMounted` 加载项目;模板新增项目筛选区;空状态/清除按钮改 `hasFilter`;
  新增样式;`import AppSelect`

* [x] 8 个 `runs.ts` 新增 `allProjects` / `projectFilterAria`

* [x] `runs.spec.ts` 新增项目筛选 e2e 用例 + import `projectFixture`

* [x] `Runs.test.ts` 新增组件级回归(5 条,沙箱内可运行)

* [x] 本设计文档

