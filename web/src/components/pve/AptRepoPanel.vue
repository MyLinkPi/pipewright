<script setup lang="ts">
/*
  AptRepoPanel.vue — APT 仓库管理面板(非模态,嵌在升级管理页 Tab 内)。

  总览优先:默认展示集群全节点仓库摘要(代号/订阅/PVE 源/镜像/Ceph/警告),一眼看全;
  点击节点下钻到单机清单编辑(双格式逐条目启停/镜像与订阅预设/备份撤销)。
  总览层直接提供「应用到所有节点」批量动作(镜像/订阅预设)。写操作须集群已解锁;
  写入后 apt update 失败仅警告(文件已写入,可撤销),不阻断面板。ceph 源只读。
*/
import { ref, computed, watch, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { NIcon } from 'naive-ui'
import { Download, RotateClockwise, ArrowLeft, Refresh } from '@vicons/tabler'
import AppSelect from '../ui/AppSelect.vue'
import {
  getPVEAptRepos, getPVEAptRepoOverview, togglePVEAptRepo, setPVEAptRepoMirror, setPVEAptRepoPreset,
  restorePVEAptRepo, applyPVEAptRepos,
  type PVEAptRepoInventory, type PVEAptRepoFile, type PVEAptRepoEntry,
  type PVEAptRepoWriteResult, type PVEAptRepoApplyItem, type PVEAptRepoBackup,
  type PVEAptRepoOverview, type PVEAptRepoNodeSummary,
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

// ─── 视图:总览 / 单机下钻 ──────────────────────────────────────────────────────
const view = ref<'overview' | 'detail'>('overview')
const node = ref('')

// ─── 总览 ──────────────────────────────────────────────────────────────────────
const overview = ref<PVEAptRepoOverview | null>(null)
const loadingOverview = ref(false)
let ovSeq = 0
let ovController: AbortController | null = null

async function loadOverview(): Promise<void> {
  const seq = ++ovSeq
  ovController?.abort()
  const controller = new AbortController()
  ovController = controller
  overview.value = null
  if (!props.clusterId) return
  loadingOverview.value = true
  try {
    const res = await getPVEAptRepoOverview(props.clusterId, controller.signal)
    if (seq !== ovSeq) return
    overview.value = res
  } catch (err) {
    if (seq !== ovSeq || (err instanceof Error && err.name === 'AbortError')) return
    toast.error(t('pveUpgrade.errLoad'), { detail: errMsg(err, t('pveUpgrade.errNetwork')) })
  } finally {
    if (seq === ovSeq) {
      loadingOverview.value = false
      ovController = null
    }
  }
}

const nodesWithWarnings = computed(() =>
  (overview.value?.items ?? []).filter((n) => (n.warnings ?? []).some((w) => w.level === 'warn')).length)

function openDetail(n: string): void {
  view.value = 'detail'
  lastBackups.value = []
  batchResult.value = null
  if (node.value !== n) {
    node.value = n // watch(node) 触发加载(避免与显式调用重复读)
  } else {
    void loadInventory() // 重复进入同一节点:watch 不触发,显式加载
  }
}
function backToOverview(): void {
  view.value = 'overview'
  void loadOverview()
}

// ─── 单机清单加载 ──────────────────────────────────────────────────────────────
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

watch(node, () => {
  if (view.value !== 'detail') return
  lastBackups.value = []
  batchResult.value = null
  void loadInventory()
})
watch(() => props.clusterId, () => {
  lastBackups.value = []
  batchResult.value = null
  view.value = 'overview'
  void loadOverview()
})

onMounted(() => { void loadOverview() })
onUnmounted(() => {
  loadSeq += 1
  ovSeq += 1
  loadController?.abort()
  ovController?.abort()
  loadController = null
  ovController = null
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
function pveRepoLabel(v: string): string {
  if (v === 'enterprise') return t('pveUpgrade.pveRepoEnterprise')
  if (v === 'no-subscription') return t('pveUpgrade.pveRepoNoSub')
  if (v === 'mixed') return t('pveUpgrade.pveRepoMixed')
  return t('pveUpgrade.pveRepoNone')
}
function pveRepoClass(v: string): string {
  if (v === 'no-subscription') return 'ok'
  if (v === 'none') return 'err'
  return 'warn' // enterprise / mixed 需订阅
}
function mirrorLabelOf(v: string): string {
  if (v === 'ustc') return t('pveUpgrade.mirrorUstc')
  if (v === 'tsinghua') return t('pveUpgrade.mirrorTsinghua')
  if (v === 'aliyun') return t('pveUpgrade.mirrorAliyun')
  if (v === 'mixed') return t('pveUpgrade.mirrorMixed')
  return t('pveUpgrade.mirrorOfficial')
}
function warnCount(n: PVEAptRepoNodeSummary): number {
  return (n.warnings ?? []).filter((w) => w.level === 'warn').length
}
function warnTitle(n: PVEAptRepoNodeSummary): string {
  return (n.warnings ?? []).map((w) => w.message).join('\n')
}

/** 写入结果统一处理:apt update 失败仅警告;备份留存供撤销。 */
function handleWriteResult(res: PVEAptRepoWriteResult): void {
  lastBackups.value = res.backups ?? []
  if (!res.updateOk) toast.warn(t('pveUpgrade.writeUpdateFailed'), { detail: res.updateTail })
  else toast.success(res.note || t('pveUpgrade.writeOk'))
}

// ─── 写操作(单机) ──────────────────────────────────────────────────────────────
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

// ─── 批量应用(总览层 / 单机层共用) ─────────────────────────────────────────────
async function applyToAll(action: 'mirror' | 'preset', preset: string, actionLabel: string): Promise<void> {
  if (props.readonly || writing.value) return
  const ok = await confirm.open({
    title: t('pveUpgrade.applyAllConfirmTitle'),
    body: t('pveUpgrade.applyAllConfirmBody', { action: actionLabel }),
    confirmLabel: t('pveUpgrade.applyToAll'),
    variant: 'primary',
  })
  if (!ok) return
  writing.value = true
  try {
    const items = await applyPVEAptRepos(props.clusterId, { action, preset })
    batchResult.value = items
    if (view.value === 'detail') await loadInventory()
    else await loadOverview()
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
        <p class="panel-sub">{{ view === 'overview' ? t('pveUpgrade.overviewSub') : t('pveUpgrade.reposSub') }}</p>
      </div>
      <div class="head-ops">
        <template v-if="view === 'overview'">
          <button type="button" class="btn-secondary" :disabled="loadingOverview" @click="loadOverview">
            <NIcon :size="13"><Refresh :class="{ spin: loadingOverview }" /></NIcon>{{ t('pveUpgrade.refresh') }}
          </button>
        </template>
        <template v-else>
          <button type="button" class="btn-secondary" @click="backToOverview">
            <NIcon :size="13"><ArrowLeft /></NIcon>{{ t('pveUpgrade.backOverview') }}
          </button>
          <AppSelect
            v-model="node"
            :options="nodeOptions"
            :placeholder="t('pveUpgrade.selectNode')"
            :aria-label="t('pveUpgrade.selectNode')"
            min-width="150px"
            height="36px"
          />
        </template>
      </div>
    </div>

    <!-- ─── 总览视图 ─── -->
    <template v-if="view === 'overview'">
      <div v-if="loadingOverview" class="load-state">{{ t('pveUpgrade.loading') }}</div>
      <template v-else-if="overview && overview.items.length > 0">
        <p v-if="nodesWithWarnings > 0" class="banner banner--warn">
          {{ t('pveUpgrade.nodesWithWarnings', { n: nodesWithWarnings }) }}
        </p>

        <!-- 批量控制 -->
        <div class="controls">
          <label class="field">
            <span class="field-label">{{ t('pveUpgrade.mirrorPreset') }}</span>
            <AppSelect v-model="mirrorPreset" :options="mirrorOptions" :disabled="readonly || writing" min-width="150px" height="36px" />
          </label>
          <button type="button" class="btn-secondary" :disabled="readonly || writing" @click="applyToAll('mirror', mirrorPreset, `${t('pveUpgrade.mirrorPreset')} · ${mirrorLabel}`)">
            <NIcon :size="13"><Download /></NIcon>{{ t('pveUpgrade.applyToAll') }}
          </button>
          <div class="control-group">
            <span class="field-label">{{ t('pveUpgrade.subPreset') }}</span>
            <div class="control-btns">
              <button type="button" class="btn-secondary" :disabled="readonly || writing" @click="applyToAll('preset', 'no-subscription', t('pveUpgrade.presetNoSub'))">
                {{ t('pveUpgrade.applyAllNoSub') }}
              </button>
              <button type="button" class="btn-secondary" :disabled="readonly || writing" @click="applyToAll('preset', 'enterprise', t('pveUpgrade.presetEnterprise'))">
                {{ t('pveUpgrade.applyAllEnterprise') }}
              </button>
            </div>
          </div>
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
            <span><span class="tag" :class="it.ok ? 'ok' : 'err'" :title="it.err">{{ it.ok ? t('pveUpgrade.ok') : t('pveUpgrade.failed') }}</span></span>
            <span><span class="tag" :class="it.updateOk ? 'ok' : 'warn'" :title="it.note">{{ it.updateOk ? t('pveUpgrade.ok') : t('pveUpgrade.failed') }}</span></span>
          </div>
        </template>

        <!-- 总览表 -->
        <div class="row head ov-cols" aria-hidden="true">
          <span>{{ t('pveUpgrade.node') }}</span>
          <span>{{ t('pveUpgrade.codename') }}</span>
          <span>{{ t('pveUpgrade.subscription') }}</span>
          <span>{{ t('pveUpgrade.colPveRepo') }}</span>
          <span>{{ t('pveUpgrade.colMirror') }}</span>
          <span>{{ t('pveUpgrade.colCeph') }}</span>
          <span>{{ t('pveUpgrade.colWarnings') }}</span>
          <span />
        </div>
        <div v-for="n in overview.items" :key="n.node" class="row ov-cols">
          <span class="node-cell">
            <span class="node-dot" :class="n.online ? 'up' : 'down'" />
            <strong class="cell-name">{{ n.node }}</strong>
          </span>
          <template v-if="n.error">
            <span class="tag err ov-error" :title="n.error">{{ t('pveUpgrade.failed') }}</span>
          </template>
          <template v-else>
            <span class="mono dim">{{ n.codename || '—' }}</span>
            <span><span class="tag" :class="{ ok: n.subscription === 'active' }">{{ subLabel(n.subscription) }}</span></span>
            <span><span class="tag" :class="pveRepoClass(n.pveRepo)">{{ pveRepoLabel(n.pveRepo) }}</span></span>
            <span><span class="tag dim">{{ mirrorLabelOf(n.mirror) }}</span></span>
            <span><span class="tag" :class="n.ceph ? 'warn' : 'dim'">{{ n.ceph ? t('pveUpgrade.cephYes') : t('pveUpgrade.cephNo') }}</span></span>
            <span>
              <span v-if="warnCount(n) > 0" class="tag warn" :title="warnTitle(n)">{{ t('pveUpgrade.warningsN', { n: warnCount(n) }) }}</span>
              <span v-else class="dim">—</span>
            </span>
          </template>
          <span class="row-ops">
            <button type="button" class="link-btn" :disabled="!n.online" @click="openDetail(n.node)">
              {{ t('pveUpgrade.viewEdit') }}
            </button>
          </span>
        </div>
      </template>
      <p v-else class="load-state">{{ t('pveUpgrade.noNodes') }}</p>
    </template>

    <!-- ─── 单机下钻视图 ─── -->
    <template v-else>
      <div v-if="loading" class="load-state">{{ t('pveUpgrade.loading') }}</div>
      <template v-else-if="inv">
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

        <p v-for="w in inv.warnings" :key="w.key" class="banner" :class="w.level === 'warn' ? 'banner--warn' : 'banner--info'">
          {{ w.message }}
        </p>

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
          <button type="button" class="btn-secondary" :disabled="readonly || writing || !node" @click="applyToAll('mirror', mirrorPreset, `${t('pveUpgrade.mirrorPreset')} · ${mirrorLabel}`)">
            {{ t('pveUpgrade.applyToAll') }}
          </button>
        </div>

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

        <template v-if="batchResult">
          <span class="sec-title">{{ t('pveUpgrade.batchResult') }}</span>
          <div class="row head batch-cols" aria-hidden="true">
            <span>{{ t('pveUpgrade.node') }}</span>
            <span>{{ t('pveUpgrade.colResult') }}</span>
            <span>{{ t('pveUpgrade.refreshIndex') }}</span>
          </div>
          <div v-for="it in batchResult" :key="it.node" class="row batch-cols">
            <strong class="cell-name">{{ it.node }}</strong>
            <span><span class="tag" :class="it.ok ? 'ok' : 'err'" :title="it.err">{{ it.ok ? t('pveUpgrade.ok') : t('pveUpgrade.failed') }}</span></span>
            <span><span class="tag" :class="it.updateOk ? 'ok' : 'warn'" :title="it.note">{{ it.updateOk ? t('pveUpgrade.ok') : t('pveUpgrade.failed') }}</span></span>
          </div>
        </template>

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
    </template>
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
.head-ops { margin-left: auto; display: flex; gap: 8px; align-items: center; flex-shrink: 0; }
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
.ov-cols { grid-template-columns: 1.1fr 0.8fr 0.8fr 1fr 0.9fr 0.6fr 0.7fr auto; }
.ov-error { grid-column: 2 / 8; justify-self: start; }
.node-cell { display: inline-flex; align-items: center; gap: 6px; min-width: 0; }
.node-dot { width: 8px; height: 8px; border-radius: 50%; flex-shrink: 0; }
.node-dot.up { background: var(--color-green); }
.node-dot.down { background: var(--color-red); }
.row-ops { display: flex; align-items: center; justify-content: flex-end; }
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
.dim { color: var(--color-dim); }
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
.spin { animation: spin 1s linear infinite; }
@keyframes spin { to { transform: rotate(360deg); } }
@media (max-width: 960px) {
  .ov-cols { grid-template-columns: 1.1fr 0.8fr 1fr 0.7fr auto; }
  .ov-cols > :nth-child(3), .ov-cols > :nth-child(5), .ov-cols > :nth-child(6) { display: none; }
  .ov-error { grid-column: 2 / 5; }
  .repo-cols { grid-template-columns: 0.5fr 0.9fr 1.6fr; }
  .repo-cols > :nth-child(4), .repo-cols > :nth-child(5) { display: none; }
  .panel-head { flex-direction: column; }
  .head-ops { margin-left: 0; }
}
</style>