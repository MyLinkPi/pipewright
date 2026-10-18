<script setup lang="ts">
/*
  Files.vue — 服务器文件查看(目录浏览 + 文件下载 + ≤64KiB 纯文本在线查看/编辑;
  顶部导航「文件」,仅管理员)。

  服务器在 设置 → 服务器 登记;此处对单台服务器:
    - 目录浏览:面包屑逐层跳转 / 路径输入框直达 / 目录行点击进入;
    - 文件:下载(流式,浏览器直连);≤64KiB 纯文本在线查看与编辑(Monaco,
      加载失败自动降级 textarea);
    - 保存:服务端先 cp -a 同目录备份(<名称>.pipewright-bak.<UTC 时间戳>)再覆盖,
      响应带回备份路径;>64KiB 或二进制仅可下载(服务端以标记返回,不报错)。
*/
import { ref, computed, watch, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { NIcon } from 'naive-ui'
import {
  Refresh, Folder, File as FileIcon, FileText, Download, ArrowUp, X, AlertTriangle,
} from '@vicons/tabler'
import { listServers, type Server } from '../api/servers'
import {
  listServerFiles, readServerFile, saveServerFile, serverFileDownloadUrl,
  type FileListing, type ServerFile,
} from '../api/fileAdmin'
import { HttpError } from '../api/http'
import { useToast } from '../composables/useToast'
import { useConfirm } from '../composables/useConfirm'
import CodeEditor from '../components/code/CodeEditor.vue'

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()

const errMsg = (err: unknown, fallback: string): string =>
  err instanceof HttpError ? (err.apiError?.message ?? fallback) : fallback

// ─── 服务器与目录 ────────────────────────────────────────────────────────────
const servers = ref<Server[]>([])
const currentId = ref('')

const dir = ref('/')
const listing = ref<FileListing | null>(null)
const loading = ref(false)
const loadErr = ref('')
const pathInput = ref('/')
/** 正在导航的目标(防过期响应覆盖新目录)。 */
let navToken = 0

async function loadServers(): Promise<void> {
  try {
    servers.value = await listServers()
    if (!servers.value.some((s) => s.id === currentId.value)) {
      currentId.value = servers.value[0]?.id ?? ''
    }
  } catch (err) {
    toast.error(t('fileAdmin.errLoadServers'), { detail: errMsg(err, t('fileAdmin.errNetwork')) })
  }
}

async function refresh(): Promise<void> {
  if (!currentId.value) {
    listing.value = null
    return
  }
  const token = ++navToken
  loading.value = true
  loadErr.value = ''
  try {
    const res = await listServerFiles(currentId.value, dir.value)
    if (token !== navToken) return
    listing.value = res
    pathInput.value = res.path
  } catch (err) {
    if (token !== navToken) return
    listing.value = null
    loadErr.value = errMsg(err, t('fileAdmin.errList'))
  } finally {
    if (token === navToken) loading.value = false
  }
}

function openDir(path: string): void {
  dir.value = path || '/'
  void refresh()
}

function parentDir(): void {
  if (dir.value === '/') return
  const idx = dir.value.lastIndexOf('/')
  openDir(idx <= 0 ? '/' : dir.value.slice(0, idx))
}

function onPathInput(): void {
  const p = pathInput.value.trim()
  if (!p.startsWith('/')) {
    toast.error(t('fileAdmin.pathAbsolute'))
    return
  }
  openDir(p.replace(/\/+$/, '') || '/')
}

// 面包屑:根 / + 各段(可点击跳回)。
const crumbs = computed(() => {
  const out: Array<{ name: string; path: string }> = [{ name: '/', path: '/' }]
  if (dir.value === '/') return out
  let acc = ''
  for (const seg of dir.value.split('/').filter((s) => s !== '')) {
    acc += '/' + seg
    out.push({ name: seg, path: acc })
  }
  return out
})

// 条目点击:目录进入;符号链接按目录尝试,失败(目标是文件)则转预览。
async function onEntryClick(path: string, type: string): Promise<void> {
  if (type === 'dir') {
    openDir(path)
    return
  }
  if (type === 'symlink') {
    const prev = dir.value
    dir.value = path
    const token = ++navToken
    loading.value = true
    try {
      const res = await listServerFiles(currentId.value, path)
      if (token !== navToken) return
      listing.value = res
      pathInput.value = res.path
    } catch (err) {
      if (token !== navToken) return
      dir.value = prev
      if (err instanceof HttpError && err.apiError?.code === 'file_not_dir') {
        openPreview(path)
        return
      }
      toast.error(errMsg(err, t('fileAdmin.errList')))
    } finally {
      if (token === navToken) loading.value = false
    }
    return
  }
  if (type === 'file') {
    openPreview(path)
  }
}

onMounted(async () => {
  window.addEventListener('keydown', onKeydown)
  await loadServers()
  await refresh()
})

onUnmounted(() => {
  window.removeEventListener('keydown', onKeydown)
})

// 切服务器回根目录重拉。
watch(currentId, () => {
  closePreview()
  dir.value = '/'
  void refresh()
})

// ─── 格式化 ──────────────────────────────────────────────────────────────────
function formatBytes(n: number): string {
  if (!n) return '—'
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MiB`
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GiB`
}

function formatTime(epochSec: number): string {
  if (!epochSec) return '—'
  const d = new Date(epochSec * 1000)
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString()
}

const dirCount = computed(() => listing.value?.entries.filter((e) => e.type === 'dir').length ?? 0)
const fileCount = computed(() => (listing.value?.entries.length ?? 0) - dirCount.value)

// ─── 文件预览与编辑 ──────────────────────────────────────────────────────────
const previewPath = ref<string | null>(null)
const previewFile = ref<ServerFile | null>(null)
const previewLoading = ref(false)
const previewErr = ref('')
const saving = ref(false)
const editor = ref<InstanceType<typeof CodeEditor> | null>(null)

async function openPreview(path: string): Promise<void> {
  previewPath.value = path
  previewFile.value = null
  previewErr.value = ''
  previewLoading.value = true
  try {
    previewFile.value = await readServerFile(currentId.value, path)
  } catch (err) {
    previewErr.value = errMsg(err, t('fileAdmin.previewErr'))
  } finally {
    previewLoading.value = false
  }
}

function closePreview(): void {
  previewPath.value = null
  previewFile.value = null
  previewErr.value = ''
}

async function save(): Promise<void> {
  const path = previewPath.value
  const f = previewFile.value
  if (!path || !f?.editable || !editor.value || saving.value) return
  const content = editor.value.getValue()
  const ok = await confirm.open({
    title: t('fileAdmin.confirmSaveTitle'),
    body: t('fileAdmin.confirmSaveBody', { path }),
    confirmLabel: t('fileAdmin.save'),
    variant: 'danger',
  })
  if (!ok) return
  saving.value = true
  try {
    const res = await saveServerFile(currentId.value, path, content)
    toast.success(t('fileAdmin.saved'), { detail: t('fileAdmin.savedBackup', { backup: res.backup }) })
    // 原位刷新:内容基准换成已保存版本(编辑器不再显示为已修改),列表大小/时间更新。
    await openPreview(path)
    await refresh()
  } catch (err) {
    toast.error(t('fileAdmin.saveFailed'), { detail: errMsg(err, t('fileAdmin.errNetwork')) })
  } finally {
    saving.value = false
  }
}

// Esc 关闭预览。
function onKeydown(e: KeyboardEvent): void {
  if (e.key === 'Escape' && previewPath.value && !saving.value) closePreview()
}
</script>

<template>
  <div class="files-view">
    <header class="view-header">
      <div class="header-main">
        <h1 class="view-title">{{ t('fileAdmin.title') }}</h1>
        <p class="view-sub">{{ t('fileAdmin.subtitle') }}</p>
      </div>
      <div v-if="servers.length > 0" class="header-tools">
        <select v-model="currentId" class="instance-select" :aria-label="t('fileAdmin.pickServer')">
          <option v-for="s in servers" :key="s.id" :value="s.id">{{ s.name }} · {{ s.host }}</option>
        </select>
        <button class="btn-secondary" :disabled="dir === '/' || loading" @click="parentDir">
          <NIcon :size="13"><ArrowUp /></NIcon>
          {{ t('fileAdmin.parentDir') }}
        </button>
        <button class="btn-secondary" :disabled="loading" @click="refresh">
          <NIcon :size="13"><Refresh /></NIcon>
          {{ t('fileAdmin.refresh') }}
        </button>
      </div>
    </header>

    <div v-if="servers.length === 0" class="empty-panel">
      <NIcon :size="26"><Folder /></NIcon>
      <p class="empty-title">{{ t('fileAdmin.noServers') }}</p>
      <p class="empty-desc">{{ t('fileAdmin.noServersDesc') }}</p>
      <router-link to="/settings/servers" class="btn-primary">{{ t('fileAdmin.goToSettings') }}</router-link>
    </div>

    <template v-else>
      <!-- 面包屑 + 路径直达 -->
      <div class="path-bar">
        <nav class="crumbs" :aria-label="t('fileAdmin.breadcrumb')">
          <template v-for="(c, i) in crumbs" :key="`${i}-${c.path}`">
            <span v-if="i > 0" class="crumb-sep">/</span>
            <button v-if="i < crumbs.length - 1" class="crumb-link" @click="openDir(c.path)">{{ c.name }}</button>
            <span v-else class="crumb-current" :title="c.path">{{ c.name }}</span>
          </template>
        </nav>
        <input
          v-model="pathInput"
          class="path-input mono"
          spellcheck="false"
          :aria-label="t('fileAdmin.pathAria')"
          :placeholder="t('fileAdmin.pathPlaceholder')"
          @keydown.enter="onPathInput"
        />
      </div>

      <div v-if="loading && !listing" class="files-state">{{ t('fileAdmin.loading') }}</div>
      <div v-else-if="loadErr && !listing" class="files-error">
        <NIcon :size="20"><AlertTriangle /></NIcon>
        <p>{{ loadErr }}</p>
        <button class="btn-secondary" @click="refresh">{{ t('fileAdmin.retry') }}</button>
      </div>

      <div v-else-if="listing && listing.entries.length === 0" class="empty-panel empty-panel--slim">
        <NIcon :size="22"><Folder /></NIcon>
        <p class="empty-title">{{ t('fileAdmin.emptyDir') }}</p>
      </div>

      <div v-else-if="listing" class="panel">
        <div class="panel-head">
          <span class="row-count">{{ t('fileAdmin.entryCount', { dirs: dirCount, files: fileCount }) }}</span>
        </div>
        <div class="grid-scroll">
          <table class="data-table">
            <thead>
              <tr>
                <th>{{ t('fileAdmin.colName') }}</th>
                <th class="col-num">{{ t('fileAdmin.colSize') }}</th>
                <th>{{ t('fileAdmin.colMode') }}</th>
                <th>{{ t('fileAdmin.colOwner') }}</th>
                <th>{{ t('fileAdmin.colModified') }}</th>
                <th>{{ t('fileAdmin.colActions') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="e in listing.entries" :key="e.path" class="row-click" @click="onEntryClick(e.path, e.type)">
                <td class="name-cell">
                  <NIcon :size="14" :class="e.type === 'dir' ? 'icon-dir' : 'icon-file'">
                    <Folder v-if="e.type === 'dir'" />
                    <FileText v-else-if="e.type === 'file'" />
                    <FileIcon v-else />
                  </NIcon>
                  <span class="mono name-text" :title="e.path">{{ e.name }}</span>
                  <span v-if="e.type === 'symlink'" class="dim-tag">@</span>
                </td>
                <td class="col-num cell-dim">{{ e.type === 'dir' ? '—' : formatBytes(e.size) }}</td>
                <td class="cell-dim mono">{{ e.mode }}</td>
                <td class="cell-dim mono">{{ e.owner }}:{{ e.group }}</td>
                <td class="cell-dim">{{ formatTime(e.mtime) }}</td>
                <td @click.stop>
                  <a
                    v-if="e.type === 'file' || e.type === 'symlink'"
                    class="mini-btn"
                    :href="serverFileDownloadUrl(currentId, e.path)"
                    :download="e.name"
                  >{{ t('fileAdmin.download') }}</a>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </template>

    <!-- 文件预览/编辑 覆盖层 -->
    <Teleport to="body">
      <div v-if="previewPath" class="preview-backdrop" @click.self="!saving && closePreview()">
        <div class="preview-panel" role="dialog" :aria-label="previewPath">
          <header class="preview-head">
            <div class="preview-title mono" :title="previewPath">{{ previewPath }}</div>
            <div class="preview-tools">
              <a
                class="btn-secondary"
                :href="serverFileDownloadUrl(currentId, previewPath)"
                :download="previewPath.split('/').pop()"
              >
                <NIcon :size="13"><Download /></NIcon>
                {{ t('fileAdmin.download') }}
              </a>
              <button class="btn-secondary" :disabled="saving" @click="closePreview">
                <NIcon :size="13"><X /></NIcon>
                {{ t('fileAdmin.close') }}
              </button>
            </div>
          </header>

          <div v-if="previewLoading" class="preview-state">{{ t('fileAdmin.previewLoading') }}</div>
          <div v-else-if="previewErr" class="files-error preview-state-error">
            <NIcon :size="20"><AlertTriangle /></NIcon>
            <p>{{ previewErr }}</p>
          </div>
          <template v-else-if="previewFile">
            <div v-if="previewFile.tooLarge" class="preview-notice">
              <NIcon :size="20"><AlertTriangle /></NIcon>
              <p class="notice-title">{{ t('fileAdmin.tooLargeTitle') }}</p>
              <p class="notice-desc">{{ t('fileAdmin.tooLargeDesc', { size: formatBytes(previewFile.size) }) }}</p>
            </div>
            <div v-else-if="previewFile.binary" class="preview-notice">
              <NIcon :size="20"><FileIcon /></NIcon>
              <p class="notice-title">{{ t('fileAdmin.binaryTitle') }}</p>
              <p class="notice-desc">{{ t('fileAdmin.binaryDesc') }}</p>
            </div>
            <template v-else>
              <div class="preview-meta mono">
                <span>{{ formatBytes(previewFile.size) }}</span>
              </div>
              <CodeEditor ref="editor" class="preview-editor" :path="previewPath" :content="previewFile.content" />
            </template>
          </template>

          <footer v-if="previewFile?.editable && !previewLoading && !previewErr" class="preview-foot">
            <span class="foot-hint">{{ t('fileAdmin.saveHint') }}</span>
            <button class="btn-primary" :disabled="saving" @click="save">
              {{ saving ? t('fileAdmin.saving') : t('fileAdmin.save') }}
            </button>
          </footer>
        </div>
      </div>
    </Teleport>
  </div>
</template>

<style scoped>
.files-view {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.view-header {
  display: flex;
  justify-content: space-between;
  align-items: flex-end;
  gap: 16px;
  flex-wrap: wrap;
}
.view-title { font-size: 1.3rem; font-weight: 700; color: var(--color-text); margin: 0; }
.view-sub { font-size: 0.82rem; color: var(--color-faint); margin: 4px 0 0; }
.header-tools { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.instance-select {
  height: 32px;
  padding: 0 8px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  color: var(--color-text);
  font-size: 0.8rem;
}

.btn-primary {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 34px;
  padding: 0 16px;
  background: var(--color-primary);
  color: var(--color-primary-contrast, #fff);
  border: none;
  border-radius: var(--rounded);
  font-size: 0.82rem;
  font-weight: 600;
  cursor: pointer;
}
.btn-primary:disabled { opacity: 0.55; cursor: not-allowed; }
.btn-secondary {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 32px;
  padding: 0 12px;
  background: var(--color-card);
  color: var(--color-text);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded);
  font-size: 0.8rem;
  font-weight: 500;
  cursor: pointer;
  text-decoration: none;
}
.btn-secondary:disabled { opacity: 0.55; cursor: not-allowed; }

.empty-panel {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 10px;
  padding: 48px 24px;
  border: 1px dashed var(--color-border-strong);
  border-radius: var(--rounded);
  color: var(--color-dim);
}
.empty-panel--slim { padding: 28px 20px; }
.empty-title { font-size: 0.92rem; font-weight: 600; color: var(--color-text); margin: 0; }
.empty-desc { font-size: 0.82rem; color: var(--color-faint); max-width: 52ch; margin: 0; }

.files-state {
  padding: 40px 24px;
  text-align: center;
  color: var(--color-faint);
  font-size: 0.84rem;
}
.files-error {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 12px;
  padding: 36px 24px;
  text-align: center;
  border: 1px solid var(--color-red-line);
  background: var(--color-red-soft);
  border-radius: var(--rounded);
  color: var(--color-red);
  font-size: 0.82rem;
  word-break: break-all;
}

.path-bar {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}
.crumbs {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
  font-size: 0.84rem;
  min-width: 0;
}
.crumb-link {
  border: none;
  background: none;
  padding: 0;
  color: var(--color-primary);
  font-size: 0.84rem;
  font-weight: 500;
  cursor: pointer;
  max-width: 260px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.crumb-link:hover { text-decoration: underline; }
.crumb-current {
  color: var(--color-text);
  font-weight: 600;
  max-width: 260px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.crumb-sep { color: var(--color-faint); }
.path-input {
  flex: 1;
  min-width: 220px;
  height: 32px;
  padding: 0 10px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  color: var(--color-text);
  font-size: 0.78rem;
}
.path-input:focus { outline: 1px solid var(--color-primary); }

.panel {
  display: flex;
  flex-direction: column;
  gap: 10px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  background: var(--color-card);
  padding: 14px;
}
.panel-head { display: flex; align-items: center; justify-content: space-between; gap: 10px; }
.row-count { font-size: 0.76rem; color: var(--color-faint); }

.grid-scroll {
  overflow: auto;
  max-height: 560px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
}
.data-table { width: 100%; border-collapse: collapse; font-size: 0.78rem; }
.data-table th {
  text-align: left;
  padding: 8px 10px;
  font-size: 0.7rem;
  font-weight: 600;
  color: var(--color-faint);
  text-transform: uppercase;
  letter-spacing: 0.04em;
  border-bottom: 1px solid var(--color-border);
  background: var(--color-card-2);
  white-space: nowrap;
  position: sticky;
  top: 0;
  z-index: 1;
}
.data-table td {
  padding: 8px 10px;
  border-bottom: 1px solid var(--color-border);
  color: var(--color-dim);
  vertical-align: middle;
}
.data-table tbody tr:last-child td { border-bottom: none; }
.row-click { cursor: pointer; }
.row-click:hover td { background: var(--color-inset); }
.name-cell { display: flex; align-items: center; gap: 8px; min-width: 0; }
.name-text {
  font-size: 0.8rem;
  color: var(--color-text);
  max-width: 380px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.icon-dir { color: var(--color-primary); flex: none; }
.icon-file { color: var(--color-faint); flex: none; }
.cell-dim { color: var(--color-dim); }
.col-num { text-align: right; white-space: nowrap; }
.dim-tag { margin-left: 4px; font-size: 0.66rem; color: var(--color-faint); }
.mini-btn {
  display: inline-flex;
  align-items: center;
  height: 24px;
  padding: 0 10px;
  background: var(--color-card);
  color: var(--color-dim);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded);
  font-size: 0.72rem;
  cursor: pointer;
  text-decoration: none;
  white-space: nowrap;
}
.mini-btn:hover { color: var(--color-text); border-color: var(--color-primary); }

/* ─── 预览/编辑覆盖层 ─────────────────────────────────────────────────────── */
.preview-backdrop {
  position: fixed;
  inset: 0;
  z-index: 60;
  background: color-mix(in srgb, var(--color-bg, #111) 65%, transparent);
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 32px;
}
.preview-panel {
  display: flex;
  flex-direction: column;
  width: min(960px, 100%);
  max-height: calc(100vh - 64px);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded);
  background: var(--color-card);
  overflow: hidden;
}
.preview-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 10px 14px;
  border-bottom: 1px solid var(--color-border);
}
.preview-title {
  font-size: 0.8rem;
  color: var(--color-text);
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.preview-tools { display: flex; align-items: center; gap: 8px; flex: none; }
.preview-state {
  padding: 48px 24px;
  text-align: center;
  color: var(--color-faint);
  font-size: 0.84rem;
}
.preview-state-error { border: none; border-radius: 0; padding: 32px 24px; }
.preview-notice {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
  padding: 44px 24px;
  color: var(--color-dim);
  text-align: center;
}
.notice-title { font-size: 0.92rem; font-weight: 600; color: var(--color-text); margin: 0; }
.notice-desc { font-size: 0.8rem; color: var(--color-faint); margin: 0; }
.preview-meta {
  padding: 8px 14px 0;
  font-size: 0.72rem;
  color: var(--color-faint);
}
.preview-editor {
  flex: 1;
  min-height: 320px;
  display: flex;
}
.preview-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 10px 14px;
  border-top: 1px solid var(--color-border);
}
.foot-hint { font-size: 0.74rem; color: var(--color-faint); }
</style>
