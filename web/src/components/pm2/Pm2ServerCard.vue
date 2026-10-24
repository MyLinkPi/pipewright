<script setup lang="ts">
/*
  Pm2ServerCard.vue — 单台服务器的 pm2 进程管理卡片(仿 ServerCard 精简版,无 tab)。
  进程行:状态点 | 名称+脚本+pm_id·pid | 运行时长+重启次数+CPU/内存 | 生命周期操作。
  生命周期操作按**进程名**下发(pm2 restart <name> 对 cluster 同名全部实例生效)。
  写操作完成后 emit('changed') 让父刷新聚合;日志/详情交页面级抽屉与弹窗。
*/
import { ref, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { serviceAction } from '../../api/servers'
import type { ServerPm2, Pm2Process } from '../../api/pm2'
import { HttpError } from '../../api/http'
import { useToast } from '../../composables/useToast'
import { useConfirm } from '../../composables/useConfirm'
import { NIcon } from 'naive-ui'
import { Cpu, AlertTriangle } from '@vicons/tabler'
import {
  pm2Bucket,
  pm2StateMeta,
  pm2ActionsFor,
  pm2RowKey,
  pm2UptimeLabel,
  formatPm2Bytes,
  PM2_ACTION_HINTS,
  PM2_DANGER_COPY,
  type Pm2ActionSpec,
  type Pm2Bucket,
} from '../../lib/pm2State'

const props = defineProps<{
  group: ServerPm2
  name: string
  host: string
  /** 状态筛选(来自页面级筛选段);'all' = 不筛。 */
  stateFilter: 'all' | Pm2Bucket
  /** 文本搜索;按 name / script 忽略大小写包含。 */
  search: string
  /** 批量模式:进程行左侧显示复选框。 */
  bulkMode: boolean
  /** 父持有的选择集;key = `${serverId}::${name}`。 */
  selectedSet: Set<string>
}>()
const emit = defineEmits<{
  (e: 'changed'): void
  (e: 'logs', p: Pm2Process): void
  (e: 'detail', p: Pm2Process): void
  (e: 'toggle-select', name: string): void
}>()

const toast = useToast()
const confirm = useConfirm()
const { t } = useI18n()

const visibleProcesses = computed(() => {
  let list = props.group.processes
  if (props.stateFilter !== 'all') {
    list = list.filter((p) => pm2Bucket(p.status) === props.stateFilter)
  }
  const q = props.search.trim().toLowerCase()
  if (q) {
    list = list.filter((c) => c.name.toLowerCase().includes(q) || c.script.toLowerCase().includes(q))
  }
  return list
})

const emptyHint = computed(() => {
  const hasSearch = props.search.trim().length > 0
  const hasFilter = props.stateFilter !== 'all'
  const q = props.search.trim()
  const total = props.group.total
  if (hasSearch && hasFilter) return t('pm2.emptySearchFilter', { q, total })
  if (hasSearch) return t('pm2.emptySearch', { q, total })
  return t('pm2.emptyFilter', { total })
})

function isSelected(name: string): boolean {
  return props.selectedSet.has(`${props.group.serverId}::${name}`)
}

// ─── 生命周期操作(按进程名;cluster 同名全部实例一并生效) ──────────────────────
const busy = ref<Set<string>>(new Set())

function rowBusy(rowKey: string): boolean {
  for (const k of busy.value) if (k.startsWith(rowKey + ':')) return true
  return false
}
function isBusy(rowKey: string, action: string): boolean {
  return busy.value.has(`${rowKey}:${action}`)
}

async function runAction(p: Pm2Process, spec: Pm2ActionSpec): Promise<void> {
  if (spec.danger) {
    const copy = PM2_DANGER_COPY[spec.action as keyof typeof PM2_DANGER_COPY]
    const ok = await confirm.open({
      title: copy?.title(p.name) ?? t('pm2.actionConfirmTitle', { name: p.name, label: spec.label }),
      body: copy?.body ?? t('pm2.actionConfirmBody'),
      confirmLabel: copy?.confirmLabel ?? spec.label,
      variant: 'danger',
    })
    if (!ok) return
  }
  const key = `${pm2RowKey(p)}:${spec.action}`
  busy.value = new Set(busy.value).add(key)
  try {
    const res = await serviceAction(props.group.serverId, { type: 'pm2', target: p.name, action: spec.action })
    if (res.ok) toast.success(t('pm2.actionSuccess', { label: spec.label }), { detail: `${p.name} · ${props.name}` })
    else toast.error(t('pm2.actionFail', { label: spec.label }), { detail: res.error || t('pm2.errNonZeroExit') })
  } catch (err) {
    toast.error(t('pm2.actionFail', { label: spec.label }), {
      detail: err instanceof HttpError ? (err.apiError?.message ?? t('pm2.errReqFail', { status: err.status })) : t('pm2.errNetwork'),
    })
  } finally {
    const next = new Set(busy.value)
    next.delete(key)
    busy.value = next
    // 延迟一拍:后端清单缓存已被操作失效,600ms 后拿到的多半是已重采的新清单。
    setTimeout(() => emit('changed'), 600)
  }
}
</script>

<template>
  <article class="panel">
    <header class="panel__head">
      <div class="panel__id">
        <h2 class="panel__name">{{ name }}</h2>
        <span class="panel__host mono">{{ host }}</span>
      </div>
      <div class="panel__meta">
        <span
          class="rt-badge"
          :class="{
            'rt-badge--ok': group.reachable && group.runtime,
            'rt-badge--warn': group.reachable && !group.runtime,
            'rt-badge--down': !group.reachable,
          }"
        >
          <NIcon :size="15" class="rt-badge__icon">
            <Cpu v-if="group.reachable && group.runtime" />
            <AlertTriangle v-else />
          </NIcon>
          {{ !group.reachable ? t('pm2.unreachable') : group.runtime ? 'pm2' : t('pm2.noRuntime') }}
        </span>
        <span v-if="group.reachable && group.runtime" class="panel__count mono">
          {{ t('pm2.onlineCount', { online: group.online, total: group.total }) }}
        </span>
      </div>
    </header>

    <!-- 不可达 / 无 pm2:行内提示 -->
    <p v-if="!group.reachable" class="panel__hint panel__hint--down">⚠ {{ group.error }}</p>
    <p v-else-if="!group.runtime" class="panel__hint">{{ group.error }}</p>

    <template v-else>
      <p v-if="group.total === 0" class="panel__hint">{{ t('pm2.noProcesses') }}</p>
      <p v-else-if="visibleProcesses.length === 0" class="panel__hint">{{ emptyHint }}</p>
      <ul v-else class="plist" role="list">
        <li
          v-for="p in visibleProcesses"
          :key="pm2RowKey(p)"
          class="prow"
          :class="{ 'prow--busy': rowBusy(pm2RowKey(p)), 'prow--bulk': bulkMode }"
        >
          <label v-if="bulkMode" class="prow__check">
            <input
              type="checkbox"
              :checked="isSelected(p.name)"
              :aria-label="t('pm2.selectProcessAria', { name: p.name })"
              @change="emit('toggle-select', p.name)"
            />
          </label>
          <div class="prow__state">
            <span class="dot" :class="[`dot--${pm2StateMeta(p.status).tone}`, { 'dot--pulse': pm2StateMeta(p.status).pulse }]" aria-hidden="true" />
            <span class="prow__statelabel" :class="`txt--${pm2StateMeta(p.status).tone}`">{{ pm2StateMeta(p.status).label }}</span>
          </div>
          <div class="prow__main">
            <div class="prow__name" :title="p.name">{{ p.name }}</div>
            <div class="prow__sub mono" :title="`${p.script} · pm_id ${p.id}`">
              {{ p.script || '—' }} · pm_id {{ p.id }}<template v-if="p.pid"> · pid {{ p.pid }}</template>
            </div>
          </div>
          <div class="prow__status">
            <div class="prow__statustxt" :title="t('pm2.restartsTitle', { n: p.restarts })">
              {{ pm2UptimeLabel(p.uptime) || '—' }} · {{ t('pm2.restartsCount', { n: p.restarts }) }}
            </div>
            <div v-if="p.status === 'online'" class="prow__stats mono" :title="t('pm2.memTitle')">
              CPU {{ p.cpu.toFixed(1) }}% · {{ formatPm2Bytes(p.memory) }}
            </div>
          </div>
          <div class="prow__actions">
            <button
              v-for="a in pm2ActionsFor(p.status)"
              :key="a.action"
              class="op"
              :class="[`op--${a.variant}`, { 'op--busy': isBusy(pm2RowKey(p), a.action) }]"
              :disabled="rowBusy(pm2RowKey(p))"
              :title="PM2_ACTION_HINTS[a.action]"
              @click="runAction(p, a)"
            >
              {{ a.label }}
            </button>
            <button class="op op--ghost" :title="t('pm2.detailTitle')" @click="emit('detail', p)">{{ t('pm2.detail') }}</button>
            <button class="op op--ghost" :disabled="rowBusy(pm2RowKey(p))" :title="t('pm2.logsTitle')" @click="emit('logs', p)">{{ t('pm2.logs') }}</button>
          </div>
        </li>
      </ul>
    </template>
  </article>
</template>

<style scoped>
.panel {
  background: var(--color-card);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-lg, 12px);
  box-shadow: var(--shadow);
  overflow: hidden;
}
.panel__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  padding: 14px 18px;
  border-bottom: 1px solid var(--color-border);
  background: var(--color-card-2);
}
.panel__id {
  display: flex;
  align-items: baseline;
  gap: 10px;
  min-width: 0;
}
.panel__name {
  margin: 0;
  font-size: var(--text-section);
  font-weight: 700;
  color: var(--color-text);
}
.panel__host {
  font-size: var(--text-micro);
  color: var(--color-faint);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.panel__meta {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-shrink: 0;
}
.panel__count {
  font-size: var(--text-micro);
  color: var(--color-dim);
  font-variant-numeric: tabular-nums;
}
.rt-badge {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: var(--text-label);
  font-weight: 700;
  padding: 4px 11px 4px 9px;
  border-radius: 999px;
  border: 1px solid var(--color-border-strong);
  color: var(--color-dim);
  white-space: nowrap;
}
.rt-badge__icon {
  display: inline-flex;
}
.rt-badge--ok {
  color: var(--color-green);
  background: var(--color-green-soft);
  border-color: var(--color-green-line);
}
.rt-badge--warn {
  color: var(--color-amber);
  background: var(--color-amber-soft);
  border-color: var(--color-amber-line);
}
.rt-badge--down {
  color: var(--color-red);
  background: var(--color-red-soft);
  border-color: var(--color-red-line);
}
.panel__hint {
  margin: 0;
  padding: 16px 18px;
  font-size: var(--text-label);
  color: var(--color-faint);
}
.panel__hint--down {
  color: var(--color-red);
}

/* 进程行 */
.plist {
  list-style: none;
  margin: 0;
  padding: 0;
}
.prow {
  display: grid;
  grid-template-columns: 92px minmax(0, 1.4fr) minmax(0, 1.3fr) auto;
  align-items: center;
  gap: 14px;
  padding: 12px 18px;
  border-bottom: 1px solid var(--color-border);
  transition: background var(--duration-fast) var(--ease-out-expo);
}
.prow:last-child {
  border-bottom: none;
}
.prow:hover {
  background: var(--color-card-2);
}
.prow--busy {
  opacity: 0.55;
}
.prow--bulk {
  grid-template-columns: 26px 92px minmax(0, 1.4fr) minmax(0, 1.3fr) auto;
}
.prow__check {
  display: inline-flex;
  align-items: center;
  cursor: pointer;
}
.prow__check input {
  width: 16px;
  height: 16px;
  cursor: pointer;
  accent-color: var(--color-primary);
}
.prow__state {
  display: flex;
  align-items: center;
  gap: 7px;
}
.prow__statelabel {
  font-size: var(--text-micro);
  font-weight: 600;
}
.dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  flex-shrink: 0;
  background: var(--color-faint);
}
.dot--green { background: var(--color-green); }
.dot--amber { background: var(--color-amber); }
.dot--red { background: var(--color-red); }
.dot--faint { background: var(--color-faint); }
.dot--pulse { animation: dotPulse 1.8s var(--ease-out-expo) infinite; }
@keyframes dotPulse {
  0%, 100% { opacity: 1; transform: scale(1); }
  50% { opacity: 0.45; transform: scale(0.8); }
}
.txt--green { color: var(--color-green); }
.txt--amber { color: var(--color-amber); }
.txt--red { color: var(--color-red); }
.txt--faint { color: var(--color-faint); }
.prow__main { min-width: 0; }
.prow__name {
  font-size: var(--text-body);
  font-weight: 600;
  color: var(--color-text);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.prow__sub {
  font-size: var(--text-micro);
  color: var(--color-faint);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.prow__status { min-width: 0; }
.prow__statustxt {
  font-size: var(--text-label);
  color: var(--color-dim);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.prow__stats {
  margin-top: 2px;
  font-size: var(--text-micro);
  color: var(--color-accent);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.prow__actions {
  display: flex;
  align-items: center;
  gap: 6px;
  justify-content: flex-end;
  flex-wrap: wrap;
}
.op {
  font-size: var(--text-micro);
  font-weight: 600;
  padding: 4px 10px;
  border-radius: var(--rounded-sm);
  border: 1px solid var(--color-border-strong);
  background: transparent;
  color: var(--color-dim);
  cursor: pointer;
  transition: all var(--duration-fast) var(--ease-out-expo);
}
.op:hover:not(:disabled) { color: var(--color-text); border-color: var(--color-text); }
.op--default { color: var(--color-primary); border-color: var(--color-primary-soft); }
.op--default:hover:not(:disabled) { color: #fff; background: var(--color-primary); border-color: var(--color-primary); }
.op:disabled { opacity: 0.45; cursor: not-allowed; }
.op--busy { opacity: 0.6; pointer-events: none; }

@media (max-width: 720px) {
  .prow, .prow--bulk { grid-template-columns: 1fr; gap: 8px; }
  .prow__actions { justify-content: flex-start; }
}
</style>
