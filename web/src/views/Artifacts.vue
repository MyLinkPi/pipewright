<script setup lang="ts">
/*
  Artifacts.vue — 流水线产物管理(跨运行产物列表 + 查看/下载/删除;顶部导航「产物」)。

  数据源是全部运行的产物(run_artifacts JOIN 运行/项目):
    - 查看:列表 + 详情弹窗(metadata 全量键值:digest/filename/format/files 清单/来源节点);
    - 下载:文件类(jar/dist/archive,metadata.stored=true)走制品库字节流;
      镜像类走服务端导出(docker pull + docker save 流式 .tar;大镜像耗时,外置私有
      仓库可能因无凭据失败——服务端诚实报错);
    - 删除:仅终态运行的产物可删(服务端护栏);镜像类联动删内置 registry tag
      (digest 共租时保留别名),已无引用的制品库 blob 由服务端回收;全程审计。
*/
import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { Refresh, Eye, Download, Trash } from '@vicons/tabler'
import AppSelect from '../components/ui/AppSelect.vue'
import { listProjects, type Project } from '../api/projects'
import {
  listArtifacts, deleteRunArtifact, runArtifactDownloadUrl, runArtifactImageArchiveUrl,
  type ArtifactRowDTO, type ArtifactType,
} from '../api/artifactAdmin'
import { HttpError } from '../api/http'
import { useToast } from '../composables/useToast'
import { useConfirm } from '../composables/useConfirm'

const { t } = useI18n()
const router = useRouter()
const toast = useToast()
const confirm = useConfirm()

const errMsg = (err: unknown, fallback: string): string =>
  err instanceof HttpError ? (err.apiError?.message ?? fallback) : fallback

// ─── 列表状态 ────────────────────────────────────────────────────────────────

type LoadState = 'idle' | 'loading' | 'error'

const PAGE_SIZE = 20
const loadState = ref<LoadState>('idle')
const loadError = ref('')
const rows = ref<ArtifactRowDTO[]>([])
const total = ref(0)
const page = ref(1)

const totalPages = computed(() => Math.max(1, Math.ceil(total.value / PAGE_SIZE)))

// ─── 筛选 ────────────────────────────────────────────────────────────────────

const typeFilter = ref<ArtifactType | 'all'>('all')
const projectFilter = ref('')
const projects = ref<Project[]>([])

const TYPE_FILTER_OPTIONS: Array<ArtifactType | 'all'> = ['all', 'image', 'jar', 'dist', 'archive']

function typeFilterLabel(v: ArtifactType | 'all'): string {
  return v === 'all' ? t('artifactAdmin.filterAllTypes') : t(`artifactAdmin.type_${v}`)
}

const projectFilterOptions = computed(() => [
  { value: '', label: t('artifactAdmin.allProjects') },
  ...projects.value.map((p) => ({ value: p.id, label: p.name })),
])

const hasFilter = computed(() => typeFilter.value !== 'all' || projectFilter.value !== '')

function clearFilters(): void {
  typeFilter.value = 'all'
  projectFilter.value = ''
  page.value = 1
  void load()
}

async function loadProjects(): Promise<void> {
  try {
    projects.value = await listProjects()
  } catch {
    // 非致命:项目下拉退化为只有「全部项目」。
  }
}

// ─── 加载 ────────────────────────────────────────────────────────────────────

async function load(): Promise<void> {
  loadState.value = 'loading'
  loadError.value = ''
  try {
    const res = await listArtifacts({
      page: page.value,
      pageSize: PAGE_SIZE,
      projectId: projectFilter.value || undefined,
      type: typeFilter.value === 'all' ? undefined : typeFilter.value,
    })
    rows.value = res.items
    total.value = res.total
    loadState.value = 'idle'
  } catch (err) {
    if (err instanceof HttpError) {
      loadError.value = err.status === 0
        ? t('artifactAdmin.errNoServer')
        : (err.apiError?.message ?? t('artifactAdmin.errLoadFailedCode', { n: err.status }))
    } else {
      loadError.value = t('artifactAdmin.errLoadFailedRetry')
    }
    loadState.value = 'error'
  }
}

onMounted(() => {
  void loadProjects()
  void load()
})

function onFilterChange(): void {
  page.value = 1
  void load()
}

function goPage(delta: number): void {
  const next = page.value + delta
  if (next < 1 || next > totalPages.value) return
  page.value = next
  void load()
}

// ─── 类型徽标(与运行详情 ArtifactList 同一套语义色) ─────────────────────────

interface TypeConfig {
  label: string
  fg: string
  bg: string
}

const TYPE_CONFIG: Record<ArtifactType, TypeConfig> = {
  image:   { label: t('artifactAdmin.type_image'),   fg: 'var(--color-primary)', bg: 'var(--color-primary-soft)' },
  jar:     { label: 'JAR',                           fg: 'var(--color-amber)',   bg: 'var(--color-amber-soft)' },
  dist:    { label: 'dist',                          fg: 'var(--color-cyan)',    bg: 'var(--color-cyan-soft)' },
  archive: { label: 'archive',                       fg: 'var(--color-green)',   bg: 'var(--color-green-soft)' },
}

function typeConfig(type: string): TypeConfig {
  return TYPE_CONFIG[type as ArtifactType] ?? {
    label: type, fg: 'var(--color-dim)', bg: 'var(--color-card-2)',
  }
}

// ─── 展示辅助 ────────────────────────────────────────────────────────────────

function humanSize(bytes: number): string {
  if (!bytes || bytes <= 0) return t('artifactAdmin.sizeUnknown')
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = bytes
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  const fixed = i === 0 ? String(Math.round(v)) : v.toFixed(v < 10 ? 1 : 0)
  return `${fixed} ${units[i]}`
}

function shortId(id: string): string {
  return id.slice(0, 8)
}

function formatTime(iso: string): string {
  const d = new Date(iso)
  return d.toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}

function runStatusLabel(status: string): string {
  // 全局 runStatus.* 文案(缺失时回退原值)。
  const key = `runStatus.${status}`
  const label = t(key)
  return label === key ? status : label
}

function goToRun(runId: string): void {
  void router.push({ name: 'run-detail', params: { id: runId } })
}

// ─── 下载 ────────────────────────────────────────────────────────────────────

function isFileDownloadable(a: ArtifactRowDTO): boolean {
  return a.metadata?.stored === true && !!a.reference
}

function isImage(a: ArtifactRowDTO): boolean {
  return a.type === 'image' && !!a.reference
}

function downloadUrl(a: ArtifactRowDTO): string {
  return a.type === 'image'
    ? runArtifactImageArchiveUrl(a.runId, a.id)
    : runArtifactDownloadUrl(a.runId, a.id)
}

// ─── 查看详情弹窗 ────────────────────────────────────────────────────────────

interface MetaEntry {
  key: string
  value: string
}

const detailOpen = ref(false)
const detail = ref<ArtifactRowDTO | null>(null)
const detailMeta = computed<MetaEntry[]>(() => {
  const meta = detail.value?.metadata ?? {}
  return Object.keys(meta)
    .sort()
    .map((key) => {
      const raw = meta[key]
      let value: string
      if (raw === null || raw === undefined) {
        value = '—'
      } else if (typeof raw === 'object') {
        try {
          value = JSON.stringify(raw, null, 2)
        } catch {
          value = String(raw)
        }
      } else {
        value = String(raw)
      }
      return { key, value }
    })
})

function openDetail(a: ArtifactRowDTO): void {
  detail.value = a
  detailOpen.value = true
}

function closeDetail(): void {
  detailOpen.value = false
}

// ─── 删除 ────────────────────────────────────────────────────────────────────

const deleting = ref<string | null>(null)

async function removeArtifact(a: ArtifactRowDTO): Promise<void> {
  const isImageRow = a.type === 'image'
  const body = isImageRow
    ? t('artifactAdmin.deleteBodyImage', { name: a.name, ref: a.reference })
    : t('artifactAdmin.deleteBody', { name: a.name })
  const ok = await confirm.open({
    title: t('artifactAdmin.deleteTitle'),
    body,
    confirmLabel: t('artifactAdmin.deleteConfirm'),
    variant: 'danger',
  })
  if (!ok) return
  deleting.value = a.id
  try {
    const res = await deleteRunArtifact(a.runId, a.id)
    if (isImageRow && res.registryTagDeleted) {
      toast.success(t('artifactAdmin.deleteOkWithTag'))
    } else {
      toast.success(t('artifactAdmin.deleteOk'))
    }
    // 当前页删空且不在第 1 页 → 回退一页;否则原地刷新。
    if (rows.value.length === 1 && page.value > 1) {
      page.value -= 1
    }
    await load()
  } catch (err) {
    toast.error(t('artifactAdmin.deleteFailed'), {
      detail: errMsg(err, t('artifactAdmin.errNetwork')),
    })
  } finally {
    deleting.value = null
  }
}
</script>

<template>
  <div class="art-root">
    <!-- ─── 页头 ─────────────────────────────────────────────────────────── -->
    <header class="page-header">
      <div class="page-header-text">
        <h1 class="page-title">{{ t('artifactAdmin.title') }}</h1>
        <p class="page-sub">{{ t('artifactAdmin.subtitle') }}</p>
      </div>
      <button
        class="btn-secondary"
        :disabled="loadState === 'loading'"
        @click="load()"
      >
        <Refresh :size="13" aria-hidden="true" />
        {{ t('artifactAdmin.refresh') }}
      </button>
    </header>

    <!-- ─── 筛选:类型标签 + 项目下拉 ────────────────────────────────────── -->
    <div class="filter-row">
      <div class="filter-bar" role="group" :aria-label="t('artifactAdmin.typeFilterAria')">
        <button
          v-for="opt in TYPE_FILTER_OPTIONS"
          :key="opt"
          type="button"
          class="filter-tab"
          :class="{ 'filter-tab--active': typeFilter === opt }"
          @click="typeFilter = opt; onFilterChange()"
        >{{ typeFilterLabel(opt) }}</button>
      </div>
      <span v-if="projects.length" class="project-select">
        <AppSelect
          :model-value="projectFilter"
          :options="projectFilterOptions"
          :placeholder="t('artifactAdmin.allProjects')"
          :aria-label="t('artifactAdmin.projectFilterAria')"
          min-width="200px"
          height="30px"
          @update:model-value="(v: string) => { projectFilter = v; onFilterChange() }"
        />
      </span>
    </div>

    <!-- ─── 错误横幅 ──────────────────────────────────────────────────────── -->
    <div v-if="loadState === 'error'" class="banner banner--error" role="alert">
      <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true">
        <circle cx="12" cy="12" r="9"/><path d="M12 8v4M12 16h.01"/>
      </svg>
      <span>{{ loadError }}</span>
      <button class="banner-retry" @click="load">↻ {{ t('artifactAdmin.retry') }}</button>
    </div>

    <!-- ─── 加载骨架 ──────────────────────────────────────────────────────── -->
    <template v-if="loadState === 'loading'">
      <div class="table-wrap" aria-busy="true" :aria-label="t('artifactAdmin.loading')">
        <div v-for="i in 6" :key="i" class="skel-row" aria-hidden="true">
          <div class="skel skel--type" />
          <div class="skel skel--name" />
          <div class="skel skel--ref" />
          <div class="skel skel--proj" />
          <div class="skel skel--size" />
          <div class="skel skel--time" />
        </div>
      </div>
    </template>

    <!-- ─── 空状态 ────────────────────────────────────────────────────────── -->
    <template v-else-if="loadState === 'idle' && rows.length === 0">
      <div class="empty-state" role="status">
        <div class="empty-icon" aria-hidden="true">
          <svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6">
            <path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z"/>
            <path d="M3.27 6.96 12 12.01l8.73-5.05M12 22.08V12"/>
          </svg>
        </div>
        <p class="empty-label">{{ t('artifactAdmin.empty') }}</p>
        <p class="empty-hint">
          <template v-if="hasFilter">{{ t('artifactAdmin.emptyFiltered') }}</template>
          <template v-else>{{ t('artifactAdmin.emptyHint') }}</template>
        </p>
        <button v-if="hasFilter" class="btn-secondary" @click="clearFilters()">
          {{ t('artifactAdmin.clearFilter') }}
        </button>
      </div>
    </template>

    <!-- ─── 产物表格 ──────────────────────────────────────────────────────── -->
    <template v-else-if="loadState === 'idle'">
      <p class="result-count" aria-live="polite">
        {{ t('artifactAdmin.resultCount', { n: total }) }}
      </p>

      <div class="table-wrap">
        <div class="table-head" role="row">
          <span class="th">{{ t('artifactAdmin.colType') }}</span>
          <span class="th">{{ t('artifactAdmin.colName') }}</span>
          <span class="th">{{ t('artifactAdmin.colProject') }}</span>
          <span class="th">{{ t('artifactAdmin.colSize') }}</span>
          <span class="th">{{ t('artifactAdmin.colTime') }}</span>
          <span class="th th--action">{{ t('artifactAdmin.colActions') }}</span>
        </div>

        <div
          v-for="a in rows"
          :key="a.id"
          class="art-row"
          role="row"
        >
          <!-- 类型徽标 -->
          <div class="col-type">
            <span class="type-badge" :style="{ color: typeConfig(a.type).fg, background: typeConfig(a.type).bg }">
              {{ typeConfig(a.type).label }}
            </span>
          </div>

          <!-- 名称 + reference -->
          <div class="col-name">
            <span class="art-name" :title="a.name">{{ a.name }}</span>
            <code class="art-ref mono" :title="a.reference">{{ a.reference }}</code>
          </div>

          <!-- 项目 + 运行 -->
          <div class="col-project">
            <span class="project-name">{{ a.projectName }}</span>
            <button
              type="button"
              class="run-link mono"
              :title="t('artifactAdmin.goToRunTitle')"
              @click.stop="goToRun(a.runId)"
            >#{{ shortId(a.runId) }} · {{ runStatusLabel(a.runStatus) }}</button>
          </div>

          <!-- 大小 -->
          <div class="col-size">
            <span class="size-value mono">{{ humanSize(a.sizeBytes) }}</span>
          </div>

          <!-- 时间 -->
          <div class="col-time">
            <span class="time-value">{{ formatTime(a.createdAt) }}</span>
          </div>

          <!-- 操作 -->
          <div class="col-actions">
            <button
              type="button"
              class="mini-btn"
              :title="t('artifactAdmin.viewTitle')"
              @click.stop="openDetail(a)"
            >
              <Eye :size="13" aria-hidden="true" />
            </button>
            <a
              v-if="isFileDownloadable(a) || isImage(a)"
              class="mini-btn mini-btn--link"
              :href="downloadUrl(a)"
              download
              :title="isImage(a) ? t('artifactAdmin.downloadImageTitle') : t('artifactAdmin.downloadTitle')"
            >
              <Download :size="13" aria-hidden="true" />
            </a>
            <span v-else class="mini-btn mini-btn--disabled" :title="t('artifactAdmin.downloadUnavailable')">
              <Download :size="13" aria-hidden="true" />
            </span>
            <button
              type="button"
              class="mini-btn mini-btn--danger"
              :disabled="deleting === a.id"
              :title="t('artifactAdmin.deleteTitle')"
              @click.stop="removeArtifact(a)"
            >
              <Trash :size="13" aria-hidden="true" />
            </button>
          </div>
        </div>
      </div>

      <!-- ─── 分页 ─────────────────────────────────────────────────────────── -->
      <div class="pagination" :aria-label="t('artifactAdmin.paginationAria')">
        <button
          type="button"
          class="page-btn"
          :disabled="page <= 1"
          @click="goPage(-1)"
        >{{ t('artifactAdmin.prevPage') }}</button>
        <span class="page-indicator mono">{{ t('artifactAdmin.pageOf', { page, total: totalPages }) }}</span>
        <button
          type="button"
          class="page-btn"
          :disabled="page >= totalPages"
          @click="goPage(1)"
        >{{ t('artifactAdmin.nextPage') }}</button>
      </div>
    </template>

    <!-- ─── 查看详情弹窗 ──────────────────────────────────────────────────── -->
    <Teleport to="body">
      <div
        v-if="detailOpen && detail"
        class="dlg-mask"
        role="dialog"
        :aria-label="t('artifactAdmin.detailTitle')"
        aria-modal="true"
        @keydown.esc="closeDetail"
        @click.self="closeDetail"
      >
        <div class="dlg">
          <div class="dlg-head">
            <div class="dlg-head-text">
              <h3 class="dlg-title">
                <span class="type-badge" :style="{ color: typeConfig(detail.type).fg, background: typeConfig(detail.type).bg }">
                  {{ typeConfig(detail.type).label }}
                </span>
                {{ detail.name }}
              </h3>
              <code class="dlg-ref mono">{{ detail.reference }}</code>
            </div>
            <button class="dlg-close" :aria-label="t('artifactAdmin.closeDialog')" @click="closeDetail">
              <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                <path d="M18 6 6 18M6 6l12 12"/>
              </svg>
            </button>
          </div>

          <div class="dlg-body">
            <dl class="meta-grid">
              <div class="meta-item">
                <dt>{{ t('artifactAdmin.detailSize') }}</dt>
                <dd class="mono">{{ humanSize(detail.sizeBytes) }}</dd>
              </div>
              <div class="meta-item">
                <dt>{{ t('artifactAdmin.detailCreatedAt') }}</dt>
                <dd>{{ formatTime(detail.createdAt) }}</dd>
              </div>
              <div class="meta-item">
                <dt>{{ t('artifactAdmin.detailProject') }}</dt>
                <dd>{{ detail.projectName }}</dd>
              </div>
              <div class="meta-item">
                <dt>{{ t('artifactAdmin.detailRun') }}</dt>
                <dd>
                  <button type="button" class="run-link mono" @click="closeDetail(); goToRun(detail.runId)">
                    #{{ shortId(detail.runId) }} · {{ runStatusLabel(detail.runStatus) }}
                  </button>
                </dd>
              </div>
            </dl>

            <h4 class="meta-title">{{ t('artifactAdmin.detailMetadata') }}</h4>
            <p v-if="detailMeta.length === 0" class="meta-empty">{{ t('artifactAdmin.detailNoMetadata') }}</p>
            <dl v-else class="meta-kv">
              <div v-for="e in detailMeta" :key="e.key" class="meta-kv-item">
                <dt class="mono">{{ e.key }}</dt>
                <dd><pre class="meta-val mono">{{ e.value }}</pre></dd>
              </div>
            </dl>
          </div>

          <div class="dlg-foot">
            <a
              v-if="isFileDownloadable(detail) || isImage(detail)"
              class="btn-primary"
              :href="downloadUrl(detail)"
              download
            >
              <Download :size="12" aria-hidden="true" />
              {{ t('artifactAdmin.download') }}
            </a>
            <button class="btn-secondary" @click="closeDetail">{{ t('artifactAdmin.closeDialog') }}</button>
          </div>
        </div>
      </div>
    </Teleport>
  </div>
</template>

<style scoped>
/* ─── root / header ────────────────────────────────────────────────────────── */
.art-root {
  display: flex;
  flex-direction: column;
  gap: var(--card-gap);
}

.page-header {
  display: flex;
  align-items: flex-start;
  gap: 16px;
}

.page-header-text { flex: 1; }

.page-title {
  font-size: 1.5rem;
  font-weight: 700;
  letter-spacing: -0.02em;
  color: var(--color-text);
  line-height: 1.2;
}

.page-sub {
  font-size: 0.82rem;
  color: var(--color-faint);
  margin-top: 5px;
  line-height: 1.5;
}

/* ─── filters ──────────────────────────────────────────────────────────────── */
.filter-row {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}

.project-select {
  display: inline-flex;
  width: 220px;
  flex: none;
}

.filter-bar {
  display: flex;
  gap: 4px;
  background: var(--color-inset);
  border-radius: var(--rounded);
  padding: 3px;
  width: fit-content;
  flex-wrap: wrap;
}

.filter-tab {
  height: 30px;
  padding: 0 12px;
  border: none;
  background: transparent;
  color: var(--color-dim);
  font-family: var(--font-sans);
  font-size: 0.78rem;
  font-weight: 500;
  border-radius: var(--rounded-md);
  cursor: pointer;
  white-space: nowrap;
  transition: color var(--duration-fast), background-color var(--duration-fast), box-shadow var(--duration-fast);
}

.filter-tab:hover {
  color: var(--color-text);
  background: oklch(100% 0 0 / 0.04);
}

.filter-tab--active {
  background: var(--color-card);
  color: var(--color-text);
  box-shadow: var(--shadow);
}

.filter-tab:focus-visible {
  outline: 2px solid var(--color-primary);
  outline-offset: 1px;
}

/* ─── result count / table ─────────────────────────────────────────────────── */
.result-count {
  font-size: 0.78rem;
  color: var(--color-faint);
  margin-top: -4px;
}

.table-wrap {
  background: var(--color-card);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-card);
  overflow: hidden;
}

.table-head {
  display: grid;
  grid-template-columns: 92px minmax(220px, 1.6fr) minmax(150px, 1fr) 90px 110px 128px;
  padding: 0 16px;
  height: 38px;
  align-items: center;
  background: var(--color-card-2);
  border-bottom: 1px solid var(--color-border);
  gap: 12px;
}

.th {
  font-size: 0.71rem;
  font-weight: 600;
  letter-spacing: 0.04em;
  text-transform: uppercase;
  color: var(--color-faint);
}

.th--action { text-align: right; }

.art-row {
  display: grid;
  grid-template-columns: 92px minmax(220px, 1.6fr) minmax(150px, 1fr) 90px 110px 128px;
  padding: 10px 16px;
  min-height: 56px;
  align-items: center;
  gap: 12px;
  border-bottom: 1px solid var(--color-border);
  transition: background-color var(--duration-fast);
}

.art-row:last-child { border-bottom: none; }

.art-row:hover { background: var(--color-inset); }

/* ─── columns ──────────────────────────────────────────────────────────────── */
.col-type { display: flex; align-items: center; }

.type-badge {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  padding: 4px 9px;
  border-radius: var(--rounded-md);
  font-size: 0.74rem;
  font-weight: 600;
  white-space: nowrap;
}

.col-name {
  display: flex;
  flex-direction: column;
  gap: 3px;
  min-width: 0;
}

.art-name {
  font-size: 0.86rem;
  font-weight: 600;
  color: var(--color-text);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.art-ref {
  font-size: 0.74rem;
  color: var(--color-faint);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.col-project {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}

.project-name {
  font-size: 0.83rem;
  font-weight: 500;
  color: var(--color-text);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.run-link {
  border: none;
  background: none;
  padding: 0;
  font-size: 0.72rem;
  color: var(--color-primary);
  cursor: pointer;
  text-align: left;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.run-link:hover { text-decoration: underline; text-underline-offset: 2px; }

.run-link:focus-visible {
  outline: 2px solid var(--color-primary);
  outline-offset: 1px;
  border-radius: 2px;
}

.col-size { display: flex; align-items: center; justify-content: flex-end; }

.size-value {
  font-size: 0.78rem;
  color: var(--color-dim);
}

.col-time { display: flex; align-items: center; }

.time-value {
  font-size: 0.78rem;
  color: var(--color-faint);
  white-space: nowrap;
}

/* ─── actions ──────────────────────────────────────────────────────────────── */
.col-actions {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 6px;
}

.mini-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 28px;
  height: 28px;
  border-radius: var(--rounded-md);
  border: 1px solid var(--color-border-strong);
  background: var(--color-card-2);
  color: var(--color-dim);
  cursor: pointer;
  transition: border-color var(--duration-fast), color var(--duration-fast), background-color var(--duration-fast);
}

.mini-btn:hover {
  border-color: var(--color-primary);
  color: var(--color-primary);
  background: var(--color-primary-soft);
}

.mini-btn:focus-visible {
  outline: 2px solid var(--color-primary);
  outline-offset: 1px;
}

.mini-btn--link { text-decoration: none; }

.mini-btn--danger:hover {
  border-color: var(--color-red);
  color: var(--color-red);
  background: var(--color-red-soft);
}

.mini-btn--disabled {
  opacity: 0.35;
  cursor: not-allowed;
}

.mini-btn--disabled:hover {
  border-color: var(--color-border-strong);
  color: var(--color-dim);
  background: var(--color-card-2);
}

.mini-btn:disabled {
  opacity: 0.35;
  cursor: not-allowed;
}

/* ─── pagination ───────────────────────────────────────────────────────────── */
.pagination {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 12px;
  padding: 14px;
}

.page-btn {
  height: 30px;
  padding: 0 14px;
  border-radius: var(--rounded);
  border: 1px solid var(--color-border-strong);
  background: var(--color-card-2);
  color: var(--color-text);
  font-family: var(--font-sans);
  font-size: 0.8rem;
  font-weight: 500;
  cursor: pointer;
  transition: border-color var(--duration-fast);
}

.page-btn:hover:not(:disabled) { border-color: var(--color-primary); }

.page-btn:disabled {
  opacity: 0.4;
  cursor: not-allowed;
}

.page-btn:focus-visible {
  outline: 2px solid var(--color-primary);
  outline-offset: 2px;
}

.page-indicator {
  font-size: 0.78rem;
  color: var(--color-faint);
}

/* ─── skeleton ─────────────────────────────────────────────────────────────── */
.skel-row {
  display: grid;
  grid-template-columns: 92px minmax(220px, 1.6fr) minmax(150px, 1fr) 90px 110px 128px;
  gap: 12px;
  padding: 14px 16px;
  border-bottom: 1px solid var(--color-border);
  align-items: center;
}

.skel {
  display: block;
  background: linear-gradient(
    90deg,
    var(--color-inset) 0%,
    oklch(100% 0 0 / 0.06) 50%,
    var(--color-inset) 100%
  );
  background-size: 200% 100%;
  border-radius: var(--rounded-md);
  animation: shimmer 1.4s ease-in-out infinite;
}

@keyframes shimmer {
  0%   { background-position: 200% center; }
  100% { background-position: -200% center; }
}

@media (prefers-reduced-motion: reduce) {
  .skel { animation: none; background: var(--color-inset); }
}

.skel--type { height: 22px; width: 70px; }
.skel--name { height: 26px; width: 60%; }
.skel--ref  { height: 26px; width: 30%; }
.skel--proj { height: 26px; width: 80%; }
.skel--size { height: 14px; width: 50px; }
.skel--time { height: 14px; width: 80px; }

/* ─── banner / empty ───────────────────────────────────────────────────────── */
.banner {
  display: flex;
  align-items: flex-start;
  gap: 9px;
  padding: 11px 14px;
  border-radius: var(--rounded);
  font-size: 0.83rem;
  line-height: 1.5;
}

.banner--error {
  background: var(--color-red-soft);
  border: 1px solid var(--color-red-line);
  color: var(--color-red);
}

.banner-retry {
  margin-left: auto;
  flex-shrink: 0;
  background: none;
  border: none;
  color: var(--color-red);
  font-size: 0.83rem;
  font-weight: 600;
  cursor: pointer;
  padding: 0;
  text-decoration: underline;
  text-underline-offset: 2px;
}

.banner-retry:hover { opacity: 0.8; }

.empty-state {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 10px;
  padding: 72px 32px;
  text-align: center;
  background: var(--color-card);
  border: 1.5px dashed var(--color-border-strong);
  border-radius: var(--rounded-card);
}

.empty-icon {
  width: 56px;
  height: 56px;
  border-radius: var(--rounded-xl);
  background: var(--color-inset);
  border: 1.5px dashed var(--color-border-strong);
  display: grid;
  place-items: center;
  color: var(--color-dim);
  margin-bottom: 4px;
}

.empty-label {
  font-size: 1rem;
  font-weight: 600;
  color: var(--color-text);
}

.empty-hint {
  font-size: 0.82rem;
  color: var(--color-faint);
  max-width: 44ch;
  line-height: 1.6;
  margin-bottom: 6px;
}

/* ─── buttons ──────────────────────────────────────────────────────────────── */
.btn-secondary {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 34px;
  padding: 0 15px;
  background: var(--color-card-2);
  color: var(--color-text);
  border: 1px solid var(--color-border-strong);
  font-family: var(--font-sans);
  font-size: 0.83rem;
  font-weight: 500;
  border-radius: var(--rounded);
  cursor: pointer;
  transition: border-color var(--duration-fast), background-color var(--duration-fast);
}

.btn-secondary:hover { border-color: var(--color-faint); }

.btn-secondary:focus-visible {
  outline: 2px solid var(--color-primary);
  outline-offset: 2px;
}

.btn-primary {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 34px;
  padding: 0 15px;
  border: none;
  background: var(--color-primary);
  color: #fff;
  font-family: var(--font-sans);
  font-size: 0.83rem;
  font-weight: 600;
  border-radius: var(--rounded);
  cursor: pointer;
  text-decoration: none;
  transition: background-color var(--duration-fast);
}

.btn-primary:hover { background: var(--color-primary-press); }

.btn-primary:focus-visible {
  outline: 2px solid var(--color-primary);
  outline-offset: 2px;
}

/* ─── 详情弹窗 ─────────────────────────────────────────────────────────────── */
.dlg-mask {
  position: fixed;
  inset: 0;
  background: oklch(0% 0 0 / 0.62);
  display: grid;
  place-items: center;
  z-index: 100;
  padding: 24px;
  animation: scrim-in var(--duration-fast) ease both;
}

@keyframes scrim-in {
  from { opacity: 0; }
  to   { opacity: 1; }
}

.dlg {
  width: 100%;
  max-width: 620px;
  max-height: min(80vh, 720px);
  display: flex;
  flex-direction: column;
  background: var(--color-card);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded-xl);
  box-shadow: var(--shadow-modal);
  overflow: hidden;
  animation: modal-in 0.3s var(--ease-out-expo) both;
}

@keyframes modal-in {
  from { opacity: 0; transform: translateY(12px) scale(0.98); }
  to   { opacity: 1; transform: none; }
}

@media (prefers-reduced-motion: reduce) {
  .dlg-mask { animation: none; }
  .dlg      { animation: none; }
}

.dlg-head {
  display: flex;
  align-items: flex-start;
  gap: 12px;
  padding: 18px 20px 14px;
  border-bottom: 1px solid var(--color-border);
}

.dlg-head-text {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.dlg-title {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 1rem;
  font-weight: 600;
  color: var(--color-text);
  letter-spacing: -0.01em;
  word-break: break-all;
}

.dlg-ref {
  font-size: 0.76rem;
  color: var(--color-dim);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.dlg-close {
  flex-shrink: 0;
  width: 30px;
  height: 30px;
  border-radius: var(--rounded-md);
  border: none;
  background: transparent;
  color: var(--color-faint);
  cursor: pointer;
  display: grid;
  place-items: center;
  transition: color var(--duration-fast), background-color var(--duration-fast);
}

.dlg-close:hover {
  color: var(--color-text);
  background: var(--color-inset);
}

.dlg-close:focus-visible {
  outline: 2px solid var(--color-primary);
  outline-offset: 2px;
}

.dlg-body {
  padding: 16px 20px;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.meta-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 10px 16px;
  margin: 0;
}

.meta-item {
  display: flex;
  flex-direction: column;
  gap: 3px;
}

.meta-item dt {
  font-size: 0.72rem;
  font-weight: 600;
  color: var(--color-faint);
  text-transform: uppercase;
  letter-spacing: 0.04em;
}

.meta-item dd {
  margin: 0;
  font-size: 0.84rem;
  color: var(--color-text);
}

.meta-title {
  font-size: 0.78rem;
  font-weight: 600;
  color: var(--color-dim);
  margin: 4px 0 -4px;
}

.meta-empty {
  font-size: 0.82rem;
  color: var(--color-faint);
}

.meta-kv {
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin: 0;
}

.meta-kv-item {
  display: grid;
  grid-template-columns: 130px 1fr;
  gap: 10px;
  align-items: start;
  padding: 8px 10px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-md);
}

.meta-kv-item dt {
  font-size: 0.76rem;
  font-weight: 600;
  color: var(--color-dim);
  word-break: break-all;
}

.meta-kv-item dd { margin: 0; min-width: 0; }

.meta-val {
  margin: 0;
  font-size: 0.76rem;
  color: var(--color-text);
  white-space: pre-wrap;
  word-break: break-all;
  max-height: 180px;
  overflow-y: auto;
}

.dlg-foot {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
  padding: 14px 20px;
  border-top: 1px solid var(--color-border);
}

.mono { font-family: var(--font-mono); }

/* ─── responsive ───────────────────────────────────────────────────────────── */
@media (max-width: 900px) {
  .table-head, .art-row {
    grid-template-columns: 80px 1fr 100px 96px;
  }
  .col-project, .col-time {
    display: none;
  }
  .skel-row {
    grid-template-columns: 80px 1fr 100px 96px;
  }
}
</style>
