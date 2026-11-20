<script setup lang="ts">
/*
  AptRepoPanel.vue — APT 仓库管理面板(按节点;非模态,嵌在升级管理页 Tab 内)。

  清单双格式(.list / .sources)逐条目启停;镜像预设 / 订阅预设 / 批量应用;
  每次写入自动备份 → 可撤销(restore);ceph 源只读。写操作须集群已解锁。
  写入后 apt update 失败仅警告(文件已写入,可撤销),不阻断面板。
*/
import { ref, computed, watch, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { NIcon } from 'naive-ui'
import { Download, RotateClockwise } from '@vicons/tabler'
import AppSelect from '../ui/AppSelect.vue'
import {
  getPVEAptRepos, togglePVEAptRepo, setPVEAptRepoMirror, setPVEAptRepoPreset,
  restorePVEAptRepo, applyPVEAptRepos,
  type PVEAptRepoInventory, type PVEAptRepoFile, type PVEAptRepoEntry,
  type PVEAptRepoWriteResult, type PVEAptRepoApplyItem, type PVEAptRepoBackup,
} from '../../api/pveUpgrade'
import { HttpError } from '../../api/http'
import { useToast } from '../../composables/useToast'
import { useConfirm } from '../../composables/useConfirm'

const props = defineProps<{ clusterId: string; nodes: string[]; readonly: boolean }>()

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()
const errMsg = (err: unknown, fallback: string): string =>
  err instanceof HttpError ? (err.apiError?.message ?? fallback) : fallback

// ─── 节点选择与清单加载 ────────────────────────────────────────────────────────
const node = ref('')
const inv = ref<PVEAptRepoInventory | null>(null)
const loading = ref(false)
const writing = ref(false)
const mirrorPreset = ref('official')
const lastBackups = ref<PVEAptRepoBackup[]>([])
const batchResult = ref<PVEAptRepoApplyItem[] | null>(null)

const nodeOptions = computed(() => props.nodes.map((n) => ({ value: n, label: n })))
const mirrorOptions = computed(() => [
  { value: 'official', label: t('pveUpgrade.mirrorOfficial') },
  { value: 'ustc', label: t('pveUpgrade.mirrorUstc') },
  { value: 'tsinghua', label: t('pveUpgrade.mirrorTsinghua') },
  { value: 'aliyun', label: t('pveUpgrade.mirrorAliyun') },
])
const mirrorLabel = computed(() =>
  mirrorOptions.value.find((o) => o.value === mirrorPreset.value)?.label ?? mirrorPreset.value)

let loadSeq = 0
let loadController: AbortController | null = null

async function loadInventory(): Promise<void> {
  const seq = ++loadSeq
  loadController?.abort()
  const controller = new AbortController()
  loadController = controller
  const target = node.value
  inv.value = null
  if (!props.clusterId || !target) return
  loading.value = true
  try {
    const res = await getPVEAptRepos(props.clusterId, target, controller.signal)
    if (seq !== loadSeq || target !== node.value) return
    inv.value = res
  } catch (err) {
    if (seq !== loadSeq || (err instanceof Error && err.name === 'AbortError')) return
    toast.error(t('pveUpgrade.errLoad'), { detail: errMsg(err, t('pveUpgrade.errNetwork')) })
  } finally {
    if (seq === loadSeq) {
      loading.value = false
      loadController = null
    }
  }
}

watch(() => props.nodes, (list) => {
  if (!list.includes(node.value)) node.value = list[0] ?? ''
})
watch(node, () => {
  lastBackups.value = []
  batchResult.value = null
  void loadInventory()
})
watch(() => props.clusterId, () => {
  lastBackups.value = []
  batchResult.value = null
  void loadInventory()
})

onMounted(() => {
  const first = props.nodes[0] ?? ''
  if (node.value !== first) node.value = first // watch(node) 触发加载
  else void loadInventory()
})
onUnmounted(() => {
  loadSeq += 1
  loadController?.abort()
  loadController = null
})

// ─── 展示辅助 ──────────────────────────────────────────────────────────────────
function subLabel(s?: string): string {
  if (s === 'active') return t('pveUpgrade.subActive')
  if (s === 'inactive') return t('pveUpgrade.subInactive')
  return t('pveUpgrade.subUnknown')
}
function kindLabel(kind: PVEAptRepoEntry['kind']): string {
  if (kind === 'debian') return t('pveUpgrade.kindDebian')
  if (kind === 'pve') return t('pveUpgrade.kindPve')
  if (kind === 'ceph') return t('pveUpgrade.kindCeph')
  return t('pveUpgrade.kindOther')
}

/** 写入结果统一处理:apt update 失败仅警告;备份留存供撤销。 */
function handleWriteResult(res: PVEAptRepoWriteResult): void {
  lastBackups.value = res.backups ?? []
  if (!res.updateOk) toast.warn(t('pveUpgrade.writeUpdateFailed'), { detail: res.updateTail })
  else toast.success(res.note || t('pveUpgrade.writeOk'))
}

// ─── 写操作 ────────────────────────────────────────────────────────────────────
async function applyMirror(): Promise<void> {
  if (props.readonly || writing.value || !node.value) return
  writing.value = true
  try {
    const res = await setPVEAptRepoMirror(props.clusterId, node.value, mirrorPreset.value)
    handleWriteResult(res)
    await loadInventory()
  } catch (err) {
    toast.error(errMsg(err, t('pveUpgrade.errNetwork')))
  } finally {
    writing.value = false
  }
}

async function applyPreset(preset: 'no-subscription' | 'enterprise'): Promise<void> {
  if (props.readonly || writing.value || !node.value) return
  writing.value = true
  try {
    const res = await setPVEAptRepoPreset(props.clusterId, node.value, preset)
    handleWriteResult(res)
    await loadInventory()
  } catch (err) {
    toast.error(errMsg(err, t('pveUpgrade.errNetwork')))
  } finally {
    writing.value = false
  }
}

async function onToggle(file: PVEAptRepoFile, entry: PVEAptRepoEntry, ev: Event): Promise<void> {
  const enabled = (ev.target as HTMLInputElement).checked
  if (props.readonly || writing.value || entry.kind === 'ceph') return
  writing.value = true
  try {
    const res = await togglePVEAptRepo(props.clusterId, node.value, { path: file.path, key: entry.key, enabled })
    handleWriteResult(res)
    await loadInventory()
  } catch (err) {
    toast.error(errMsg(err, t('pveUpgrade.errNetwork')))
    await loadInventory() // 失败回滚展示态
  } finally {
    writing.value = false
  }
}

async function applyToAll(): Promise<void> {
  if (props.readonly || writing.value || !node.value) return
  const ok = await confirm.open({
    title: t('pveUpgrade.applyAllConfirmTitle'),
    body: t('pveUpgrade.applyAllConfirmBody', { action: `${t('pveUpgrade.mirrorPreset')} · ${mirrorLabel.value}` }),
    confirmLabel: t('pveUpgrade.applyToAll'),
    variant: 'primary',
  })
  if (!ok) return
  writing.value = true
  try {
    const items = await applyPVEAptRepos(props.clusterId, { action: 'mirror', preset: mirrorPreset.value })
    await loadInventory()
    batchResult.value = items
  } catch (err) {
    toast.error(errMsg(err, t('pveUpgrade.errNetwork')))
  } finally {
    writing.value = false
  }
}

async function restoreBackup(b: PVEAptRepoBackup): Promise<void> {
  if (props.readonly || writing.value || !node.value) return
  const ok = await confirm.open({
    title: t('pveUpgrade.restoreConfirmTitle'),
    body: t('pveUpgrade.restoreConfirmBody', { path: b.path }),
    confirmLabel: t('pveUpgrade.restore'),
    variant: 'primary',
  })
  if (!ok) return
  writing.value = true
  try {
    const res = await restorePVEAptRepo(props.clusterId, node.value, b.path, b.backupPath)
    handleWriteResult(res)
    await loadInventory()
  } catch (err) {
    toast.error(errMsg(err, t('pveUpgrade.errNetwork')))
  } finally {
    writing.value = false
  }
}
</script>

<template>
  <section class="repo-panel">
    <div class="panel-head">
      <div>
        <h3 class="panel-title">{{ t('pveUpgrade.reposTitle') }}</h3>
        <p class="panel-sub">{{ t('pveUpgrade.reposSub') }}</p>
      </div>
      <div class="node-pick">
        <AppSelect
          v-model="node"
          :options="nodeOptions"
          :placeholder="t('pveUpgrade.selectNode')"
          :aria-label="t('pveUpgrade.selectNode')"
          min-width="160px"
          height="36px"
        />
      </div>
    </div>

    <div v-if="loading" class="load-state">{{ t('pveUpgrade.loading') }}</div>
    <template v-else-if="inv">
      <!-- 代号 / 订阅 -->
      <div class="meta-row">
        <span class="meta-item">
          <span class="meta-label">{{ t('pveUpgrade.codename') }}</span>
          <strong class="mono">{{ inv.codename || '—' }}</strong>
        </span>
        <span class="meta-item">
          <span class="meta-label">{{ t('pveUpgrade.subscription') }}</span>
          <strong>{{ subLabel(inv.subscription) }}</strong>
        </span>
      </div>

      <!-- 清单警告 -->
      <p v-for="w in inv.warnings" :key="w.key" class="banner" :class="w.level === 'warn' ? 'banner--warn' : 'banner--info'">
        {{ w.message }}
      </p>

      <!-- 预设控制(只读集群禁用) -->
      <div class="controls">
        <label class="field">
          <span class="field-label">{{ t('pveUpgrade.mirrorPreset') }}</span>
          <AppSelect v-model="mirrorPreset" :options="mirrorOptions" :disabled="readonly || writing" min-width="150px" height="36px" />
        </label>
        <button type="button" class="btn-secondary" :disabled="readonly || writing || !node" @click="applyMirror">
          <NIcon :size="13"><Download /></NIcon>{{ t('pveUpgrade.applyMirror') }}
        </button>
        <div class="control-group">
          <span class="field-label">{{ t('pveUpgrade.subPreset') }}</span>
          <div class="control-btns">
            <button type="button" class="btn-secondary" :disabled="readonly || writing || !node" @click="applyPreset('no-subscription')">
              {{ t('pveUpgrade.presetNoSub') }}
            </button>
            <button type="button" class="btn-secondary" :disabled="readonly || writing || !node" @click="applyPreset('enterprise')">
              {{ t('pveUpgrade.presetEnterprise') }}
            </button>
          </div>
        </div>
        <button type="button" class="btn-secondary" :disabled="readonly || writing || !node" @click="applyToAll">
          {{ t('pveUpgrade.applyToAll') }}
        </button>
      </div>

      <!-- 撤销最近一次写入 -->
      <div v-if="lastBackups.length > 0 && !readonly" class="restore-row">
        <button
          v-for="b in lastBackups"
          :key="b.backupPath"
          type="button"
          class="link-btn"
          :disabled="writing"
          :title="b.backupPath"
          @click="restoreBackup(b)"
        >
          <NIcon :size="12"><RotateClockwise /></NIcon>{{ t('pveUpgrade.restore') }} · {{ b.path }}
        </button>
      </div>

      <!-- 批量结果 -->
      <template v-if="batchResult">
        <span class="sec-title">{{ t('pveUpgrade.batchResult') }}</span>
        <div class="row head batch-cols" aria-hidden="true">
          <span>{{ t('pveUpgrade.node') }}</span>
          <span>{{ t('pveUpgrade.colResult') }}</span>
          <span>{{ t('pveUpgrade.refreshIndex') }}</span>
        </div>
        <div v-for="it in batchResult" :key="it.node" class="row batch-cols">
          <strong class="cell-name">{{ it.node }}</strong>
          <span>
            <span class="tag" :class="it.ok ? 'ok' : 'err'" :title="it.err">{{ it.ok ? t('pveUpgrade.ok') : t('pveUpgrade.failed') }}</span>
          </span>
          <span>
            <span class="tag" :class="it.updateOk ? 'ok' : 'warn'" :title="it.note">{{ it.updateOk ? t('pveUpgrade.ok') : t('pveUpgrade.failed') }}</span>
          </span>
        </div>
      </template>

      <!-- 逐文件条目 -->
      <p v-if="inv.files.length === 0" class="load-state">{{ t('pveUpgrade.emptyRepos') }}</p>
      <div v-for="file in inv.files" :key="file.path" class="file-block">
        <div class="file-head">
          <span class="file-label">{{ t('pveUpgrade.repoFile') }}</span>
          <span class="mono file-path" :title="file.path">{{ file.path }}</span>
          <span class="tag">{{ file.format }}</span>
        </div>
        <div class="row head repo-cols" aria-hidden="true">
          <span>{{ t('pveUpgrade.repoEnabled') }}</span>
          <span>{{ t('pveUpgrade.repoKind') }}</span>
          <span>{{ t('pveUpgrade.repoUri') }}</span>
          <span>{{ t('pveUpgrade.repoSuite') }}</span>
          <span>{{ t('pveUpgrade.repoComponents') }}</span>
        </div>
        <div v-for="entry in file.entries" :key="entry.key" class="row repo-cols">
          <span>
            <input
              type="checkbox"
              :checked="entry.enabled"
              :disabled="readonly || writing || entry.kind === 'ceph'"
              :title="entry.kind === 'ceph' ? t('pveUpgrade.cephReadonlyHint') : (entry.enabled ? t('pveUpgrade.toggleOn') : t('pveUpgrade.toggleOff'))"
              @change="onToggle(file, entry, $event)"
            />
          </span>
          <span><span class="tag" :class="{ dim: entry.kind === 'ceph' }">{{ kindLabel(entry.kind) }}</span></span>
          <span class="mono cell-list" :title="entry.uris.join('\n')">{{ entry.uris.join(', ') || '—' }}</span>
          <span class="mono cell-list" :title="entry.suites.join('\n')">{{ entry.suites.join(', ') || '—' }}</span>
          <span class="mono cell-list" :title="entry.components.join('\n')">{{ entry.components.join(', ') || '—' }}</span>
        </div>
      </div>
    </template>
    <p v-else class="load-state">{{ t('pveUpgrade.noNodes') }}</p>
  </section>
</template>

<style scoped>
.repo-panel {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 16px;
  background: var(--color-card);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-card, var(--rounded-lg));
}
.panel-head { display: flex; align-items: flex-start; gap: 12px; }
.panel-title { font-size: 0.9rem; font-weight: 600; color: var(--color-text); }
.panel-sub { font-size: 0.74rem; color: var(--color-faint); margin-top: 3px; line-height: 1.5; max-width: 64ch; }
.node-pick { margin-left: auto; width: 180px; flex-shrink: 0; }
.load-state { padding: 28px 16px; text-align: center; color: var(--color-faint); font-size: 0.82rem; }
.meta-row { display: flex; gap: 20px; flex-wrap: wrap; }
.meta-item { display: inline-flex; align-items: baseline; gap: 6px; font-size: 0.8rem; }
.meta-label { font-size: 0.72rem; color: var(--color-faint); }
.meta-item strong { color: var(--color-text); }
.banner {
  padding: 9px 12px;
  border-radius: var(--rounded);
  background: var(--color-red-soft, oklch(95% 0.03 25));
  color: var(--color-red);
  font-size: 0.78rem;
  line-height: 1.5;
  overflow-wrap: anywhere;
}
.banner--warn { background: var(--color-amber-soft, oklch(95% 0.06 80)); color: var(--color-amber, oklch(75% 0.15 80)); }
.banner--info { background: var(--color-inset); color: var(--color-dim); }
.controls { display: flex; align-items: flex-end; gap: 10px; flex-wrap: wrap; }
.field { display: flex; flex-direction: column; gap: 6px; min-width: 0; }
.field-label { font-size: 0.72rem; font-weight: 500; color: var(--color-dim); }
.control-group { display: flex; flex-direction: column; gap: 6px; }
.control-btns { display: flex; gap: 6px; }
.restore-row { display: flex; gap: 12px; flex-wrap: wrap; }
.sec-title { font-size: 0.74rem; font-weight: 600; color: var(--color-dim); }
.file-block { display: flex; flex-direction: column; gap: 6px; }
.file-head { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.file-label { font-size: 0.72rem; color: var(--color-faint); }
.file-path { font-size: 0.76rem; color: var(--color-text); overflow-wrap: anywhere; }
.row {
  display: grid;
  gap: 10px;
  align-items: center;
  padding: 8px 12px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  background: var(--color-card);
  font-size: 0.8rem;
}
.row.head {
  background: none;
  border: none;
  padding-bottom: 0;
  font-size: 0.68rem;
  font-weight: 600;
  color: var(--color-faint);
  text-transform: uppercase;
  letter-spacing: 0.04em;
}
.repo-cols { grid-template-columns: 0.5fr 0.9fr 1.6fr 1fr 1fr; }
.batch-cols { grid-template-columns: 1fr 1fr 1fr; }
.cell-name { color: var(--color-text); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.cell-list { font-size: 0.72rem; color: var(--color-dim); overflow-wrap: anywhere; }
.tag {
  display: inline-flex;
  align-items: center;
  padding: 1px 8px;
  border-radius: var(--rounded-full);
  font-size: 0.68rem;
  font-weight: 600;
  background: var(--color-inset);
  color: var(--color-dim);
  white-space: nowrap;
}
.tag.ok { background: var(--color-green-soft, oklch(95% 0.04 150)); color: var(--color-green); }
.tag.warn { background: var(--color-amber-soft, oklch(95% 0.06 80)); color: var(--color-amber, oklch(75% 0.15 80)); }
.tag.err { background: var(--color-red-soft, oklch(95% 0.03 25)); color: var(--color-red); }
.tag.dim { color: var(--color-faint); }
.mono { font-family: var(--font-mono); }
.link-btn {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  background: none;
  border: none;
  padding: 0;
  color: var(--color-primary);
  font-size: 0.74rem;
  cursor: pointer;
}
.link-btn:disabled { opacity: 0.5; cursor: not-allowed; }
.btn-secondary {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 36px;
  padding: 0 12px;
  background: var(--color-card);
  color: var(--color-text);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded);
  font-size: 0.8rem;
  font-weight: 500;
  cursor: pointer;
}
.btn-secondary:disabled { opacity: 0.55; cursor: not-allowed; }
@media (max-width: 720px) {
  .repo-cols { grid-template-columns: 0.5fr 0.9fr 1.6fr; }
  .repo-cols > :nth-child(4), .repo-cols > :nth-child(5) { display: none; }
  .node-pick { width: 140px; }
}
</style>
