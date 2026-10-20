import type { Page, Route } from '@playwright/test'

/**
 * Shared API stubs for smoke e2e — keeps flows offline & deterministic.
 * Each helper installs page.route handlers that fulfill the frozen API shapes.
 */

function json(route: Route, status: number, body: unknown) {
  return route.fulfill({
    status,
    contentType: 'application/json',
    body: JSON.stringify(body),
  })
}

/**
 * Only intercept real backend calls (URL path begins with `/api/`).
 * A broad recursive glob over any `/api` segment would wrongly match Vite source
 * modules such as `/src/api/http.ts` and break loading — so we match the parsed path.
 */
function isApiPath(url: URL, suffix?: string): boolean {
  if (!url.pathname.startsWith('/api/')) return false
  return suffix ? url.pathname === suffix : true
}

/** Treat the user as NOT logged in: session → 401, so guards bounce to /login. */
export async function stubLoggedOut(page: Page): Promise<void> {
  await page.route(
    (u) => isApiPath(u, '/api/auth/session'),
    (route) => json(route, 401, { error: { code: 'unauthorized', message: '未登录' } }),
  )
}

/** Treat the user as logged in, with an empty-but-valid backend. */
export async function stubLoggedIn(page: Page, username = 'admin'): Promise<void> {
  // Most-recently-registered route wins → broad catch-all FIRST, specifics AFTER.
  await page.route(
    (u) => isApiPath(u),
    (route) => {
      if (route.request().method() === 'GET') return json(route, 200, [])
      return json(route, 204, {})
    },
  )
  await page.route(
    (u) => isApiPath(u, '/api/projects'),
    (route) => json(route, 200, []),
  )
  await page.route(
    (u) => isApiPath(u, '/api/auth/session'),
    (route) => json(route, 200, { username }),
  )
  await page.route(
    (u) => isApiPath(u, '/api/auth/login'),
    (route) => json(route, 200, { username }),
  )
}

// ─── Shared DTO fixtures (frozen-shape, used by feature specs) ────────────────

export function projectFixture(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: 'p-acme',
    name: 'acme-web',
    repoUrl: 'https://gitee.com/acme/acme-web.git',
    defaultBranch: 'main',
    credentialId: 'c-git',
    credentialName: 'gitee-token · ghp_••••a91f',
    lastRunStatus: 'success',
    // No registered services by default; specs override with ProjectServiceReg[] entries.
    serviceRegs: [],
    createdAt: '2026-05-01T08:00:00Z',
    updatedAt: '2026-05-28T08:00:00Z',
    ...over,
  }
}

export function runListItemFixture(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: 'r-00000001',
    projectId: 'p-acme',
    projectName: 'acme-web',
    status: 'success',
    trigger: { type: 'manual', branch: 'main', commit: 'a3f1c2dd', actor: 'admin' },
    createdAt: new Date().toISOString(),
    durationMs: 42000,
    ...over,
  }
}

export function runDetailFixture(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: 'r-00000001',
    projectId: 'p-acme',
    projectName: 'acme-web',
    status: 'success',
    trigger: { type: 'manual', branch: 'main', commit: 'a3f1c2dd', actor: 'admin' },
    steps: [
      { id: 's1', name: '拉取代码', status: 'success', startedAt: '2026-05-29T08:00:00Z', finishedAt: '2026-05-29T08:00:05Z', durationMs: 5000 },
      { id: 's2', name: '构建', status: 'success', startedAt: '2026-05-29T08:00:05Z', finishedAt: '2026-05-29T08:00:40Z', durationMs: 35000 },
    ],
    createdAt: '2026-05-29T08:00:00Z',
    startedAt: '2026-05-29T08:00:00Z',
    finishedAt: '2026-05-29T08:00:42Z',
    durationMs: 42000,
    artifacts: [],
    targets: null,
    diagnosis: null,
    ...over,
  }
}

/** A failed run carrying a ready AI diagnosis (root-cause + evidence + confidence). */
export function diagnosisReadyFixture(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    status: 'ready',
    reason: '',
    hypothesis: '部署步骤缺少环境变量 DB_PASSWORD，导致服务启动时连接数据库失败。',
    confidence: 'high',
    alternateCauses: [],
    fixSuggestions: ['在凭据保险库注入 DB_PASSWORD', '重新触发该分支的部署'],
    evidence: [
      { line: 41, text: 'connecting to db…', highlight: false },
      { line: 42, text: 'FATAL: password authentication failed for user "app"', highlight: true },
    ],
    generatedAt: '2026-05-29T08:01:00Z',
    ...over,
  }
}

/**
 * Control-node (local) container inventory — `GET /api/servers/__local__/containers`.
 * Deliberately reachable-with-runtime but with **no** containers: the local card then
 * renders its "no containers" hint and cannot steal `.first()` action buttons from the
 * first registered server card in layout specs.
 */
export function localHostContainersFixture(
  over: Record<string, unknown> = {},
): Record<string, unknown> {
  return {
    serverId: '__local__',
    reachable: true,
    runtime: 'docker',
    error: '',
    containers: [],
    running: 0,
    total: 0,
    collectedAt: '2026-05-29T08:00:00Z',
    ...over,
  }
}

/** Control-node container stats — `GET /api/servers/__local__/containers/stats` (empty). */
export function localHostContainerStatsFixture(
  over: Record<string, unknown> = {},
): Record<string, unknown> {
  return {
    serverId: '__local__',
    reachable: true,
    runtime: 'docker',
    error: '',
    stats: [],
    collectedAt: '2026-05-29T08:00:00Z',
    ...over,
  }
}

/**
 * Control-node (local) metrics — a bare ServerMetrics object, matching
 * GET /api/system/metrics (the host status card on the server-status page).
 */
export function localHostMetricsFixture(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    serverId: '__local__',
    reachable: true,
    error: '',
    cpu: { loadavg1: 0.42, cores: 8 },
    memory: {
      usedBytes: 4123456789,
      usedWithCacheBytes: 6173456789,
      totalBytes: 17179869184,
      physicalTotalBytes: 0,
      swapUsedBytes: 0,
      swapTotalBytes: 0,
    },
    disk: { path: '/', usedBytes: 123456789012, totalBytes: 494384795648 },
    gpu: null,
    collectedAt: '2026-05-29T08:00:00Z',
    ...over,
  }
}

/**
 * Install the logged-in catch-all PLUS object-shaped stubs for the endpoints
 * that return an envelope ({ items }) rather than a bare array — the bare-[]
 * catch-all would otherwise break listChannels/listRuns/listServers etc.
 * Specs add more-specific routes AFTER calling this (last route wins).
 */
export async function stubEnvelopeDefaults(page: Page): Promise<void> {
  const envelopes: Array<[string, unknown]> = [
    ['/api/runs', { items: [], page: 1, total: 0 }],
    ['/api/servers', { items: [] }],
    ['/api/servers/metrics', { items: [] }],
    ['/api/notifications/channels', { items: [] }],
    ['/api/notifications/routes', { items: [] }],
    ['/api/notifications/templates', { items: [] }],
  ]
  for (const [path, body] of envelopes) {
    await page.route(
      (u) => isApiPath(u, path),
      (route) => {
        if (route.request().method() === 'GET') return json(route, 200, body)
        return json(route, 204, {})
      },
    )
  }
  // 控制节点「本机」指标是**裸 ServerMetrics 对象**(非 { items } 信封)——落到 bare-[]
  // 兜底会让状态页渲染出一张错卡,这里补一份可达样例。
  await page.route(
    (u) => isApiPath(u, '/api/system/metrics'),
    (route) => {
      if (route.request().method() === 'GET') return json(route, 200, localHostMetricsFixture())
      return json(route, 204, {})
    },
  )
  // 控制节点「本机」容器(serverID __local__):容器页会单独拉这一份并置首成卡。
  // containers/stats 也要给(卡片挂载即采样),否则会落到 catch-all 的空数组上。
  await page.route(
    (u) => isApiPath(u, '/api/servers/__local__/containers/stats'),
    (route) => json(route, 200, localHostContainerStatsFixture()),
  )
  await page.route(
    (u) => isApiPath(u, '/api/servers/__local__/containers'),
    (route) => json(route, 200, localHostContainersFixture()),
  )
}
