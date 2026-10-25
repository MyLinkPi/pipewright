<script setup lang="ts">
/*
  Redis.vue — Redis 在线管理(只读浏览 + 删键/改 TTL;顶部导航「Redis」)。

  实例在 设置 → Redis 注册(应用市场部署 redis 模板自动登记);此处:
    - 概览:版本/uptime/内存/连接数 + 各逻辑库键数(INFO + DBSIZE);
    - 键浏览:SCAN 游标分页(键空间可能百万级,绝不整库拉取;支持 glob 模式过滤),
      cursor 历史栈实现上一页/下一页,nextCursor='0' 即遍历完成;
    - 键详情:类型/TTL/内存 + 按类型的值预览(服务端截断);
    - 写操作:删键/改 TTL——实例默认只读,解锁是显式确认动作(服务端强校验 + 审计)。
  逻辑库(0-15)可临时切换;实例配置只记默认库号。
*/
import { ref, computed, watch, onMounted } from 'vue'
import { useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { NIcon } from 'naive-ui'
import {
  Bolt, Refresh, ChevronLeft, ChevronRight, Lock, LockOpen,
  PlugConnected, Trash, Clock, Search, Key as KeyIcon,
} from '@vicons/tabler'
import {
  listRedisInstances, setRedisReadonly, testRedisInstance, getRedisInfo,
  scanRedisKeys, getRedisKeyDetail, deleteRedisKey, setRedisKeyTTL,
  type RedisInstance, type RedisServerInfo, type RedisScanResult, type RedisKeyDetail,
} from '../api/redisAdmin'
import { HttpError } from '../api/http'
import { useToast } from '../composables/useToast'
import { useConfirm } from '../composables/useConfirm'
import { listObjectStores, type ObjectStore } from '../api/objectStores'
import BackupPanel from '../components/dbbackup/BackupPanel.vue'

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()
const route = useRoute()

const errMsg = (err: unknown, fallback: string): string =>
  err instanceof HttpError ? (err.apiError?.message ?? fallback) : fallback

// ─── 实例 ──────────────────────────────────────────────────────────────────────
const instances = ref<RedisInstance[]>([])
const currentId = ref('')
const loadingInstances = ref(true)
const current = computed<RedisInstance | null>(() =>
  instances.value.find((i) => i.id === currentId.value) ?? null)

async function loadInstances(): Promise<void> {
  loadingInstances.value = true
  try {
    instances.value = await listRedisInstances()
    const wanted = String(route.query.instance ?? '')
    currentId.value = instances.value.some((i) => i.id === wanted)
      ? wanted
      : (instances.value[0]?.id ?? '')
  } catch (err) {
    toast.error(t('redisAdmin.errLoadInstances'), { detail: errMsg(err, t('redisAdmin.errNetwork')) })
  } finally {
    loadingInstances.value = false
  }
}

// ─── 逻辑库与概览 ──────────────────────────────────────────────────────────────
const DB_COUNT = 16
const dbNum = ref(0)
const info = ref<RedisServerInfo | null>(null)
const loadingInfo = ref(false)
const testing = ref(false)

async function loadInfo(): Promise<void> {
  if (!current.value) return
  loadingInfo.value = true
  try {
    info.value = await getRedisInfo(current.value.id, dbNum.value)
  } catch (err) {
    info.value = null
    toast.error(t('redisAdmin.errInfo'), { detail: errMsg(err, t('redisAdmin.errNetwork')) })
  } finally {
    loadingInfo.value = false
  }
}

async function runTest(): Promise<void> {
  if (!current.value || testing.value) return
  testing.value = true
  try {
    const res = await testRedisInstance(current.value.id)
    if (res.ok) {
      toast.success(t('redisAdmin.testOk', { latency: res.latencyMs, version: res.version || '?' }))
    } else {
      toast.error(t('redisAdmin.testFail'), { detail: res.error ?? '' })
    }
  } finally {
    testing.value = false
  }
}

// ─── 解锁/上锁写模式 ───────────────────────────────────────────────────────────
const readonlyBusy = ref(false)

// 备份折叠区(实例级;恢复门禁沿用本页纪律:只读时给出提示而非放行)。
const backupOpen = ref(false)
const objectProviders = ref<ObjectStore[]>([])

function restoreGate(action: () => void): void {
  if (current.value?.readonly) {
    toast.error(t('redisAdmin.readonlyHint'))
    return
  }
  action()
}

async function askUnlock(): Promise<void> {
  if (!current.value) return
  const ok = await confirm.open({
    title: t('redisAdmin.unlockTitle'),
    body: t('redisAdmin.unlockBody'),
    confirmLabel: t('redisAdmin.unlock'),
    variant: 'danger',
  })
  if (!ok) return
  await toggleReadonly(false)
}

async function relock(): Promise<void> {
  await toggleReadonly(true)
}

async function toggleReadonly(readonly: boolean): Promise<void> {
  if (!current.value || readonlyBusy.value) return
  readonlyBusy.value = true
  try {
    const updated = await setRedisReadonly(current.value.id, readonly)
    instances.value = instances.value.map((i) => (i.id === updated.id ? updated : i))
    toast.success(readonly ? t('redisAdmin.relockOk') : t('redisAdmin.unlockOk'))
  } catch (err) {
    toast.error(t('redisAdmin.errReadonlyToggle'), { detail: errMsg(err, t('redisAdmin.errNetwork')) })
  } finally {
    readonlyBusy.value = false
  }
}

// ─── 键浏览(SCAN 游标分页;cursor 历史栈回放上一页)─────────────────────────────
const PAGE_SIZES = [100, 200, 500]
const pageSize = ref(100)
const match = ref('')
const matchApplied = ref('')
const cursorStack = ref<string[]>(['0']) // 各页起始游标(第 0 页 = '0' 从头遍历)
const pageIdx = ref(0)
const scanResult = ref<RedisScanResult | null>(null)
const scanning = ref(false)
const scanError = ref('')

async function loadKeys(): Promise<void> {
  if (!current.value) return
  scanning.value = true
  scanError.value = ''
  try {
    scanResult.value = await scanRedisKeys(current.value.id, {
      db: dbNum.value,
      cursor: cursorStack.value[pageIdx.value] ?? '0',
      match: matchApplied.value,
      count: pageSize.value,
    })
  } catch (err) {
    scanResult.value = null
    scanError.value = errMsg(err, t('redisAdmin.errNetwork'))
  } finally {
    scanning.value = false
  }
}

/** 重新搜索:match 生效并从头遍历。 */
function applyMatch(): void {
  matchApplied.value = match.value.trim()
  cursorStack.value = ['0']
  pageIdx.value = 0
  detail.value = null
  void loadKeys()
}

function nextPage(): void {
  const nc = scanResult.value?.nextCursor ?? '0'
  if (nc === '0' || scanning.value) return
  cursorStack.value = [...cursorStack.value.slice(0, pageIdx.value + 1), nc]
  pageIdx.value++
  detail.value = null
  void loadKeys()
}

function prevPage(): void {
  if (pageIdx.value === 0 || scanning.value) return
  pageIdx.value--
  detail.value = null
  void loadKeys()
}

function onPageSizeChange(): void {
  cursorStack.value = ['0']
  pageIdx.value = 0
  detail.value = null
  void loadKeys()
}

function onDbChange(): void {
  cursorStack.value = ['0']
  pageIdx.value = 0
  detail.value = null
  void loadInfo()
  void loadKeys()
}

/** 各库键数标注(INFO keyspace;无数据为空串)。 */
function dbKeysLabel(db: number): string {
  const n = info.value?.keyspace?.[String(db)]
  return n === undefined ? '' : ` (${n})`
}

// ─── 键详情与写操作 ────────────────────────────────────────────────────────────
const detail = ref<RedisKeyDetail | null>(null)
const loadingDetail = ref(false)
const ttlInput = ref('')
const ttlBusy = ref(false)
const deleteBusy = ref(false)

async function openDetail(key: string): Promise<void> {
  if (!current.value) return
  loadingDetail.value = true
  detail.value = null
  try {
    detail.value = await getRedisKeyDetail(current.value.id, key, dbNum.value)
    ttlInput.value = detail.value.ttlSeconds > 0 ? String(detail.value.ttlSeconds) : ''
  } catch (err) {
    toast.error(t('redisAdmin.errDetail'), { detail: errMsg(err, t('redisAdmin.errNetwork')) })
  } finally {
    loadingDetail.value = false
  }
}

async function applyTTL(persist: boolean): Promise<void> {
  if (!current.value || !detail.value || ttlBusy.value) return
  let ttl = -1
  if (!persist) {
    const n = Number(ttlInput.value.trim())
    if (!Number.isInteger(n) || n < 0) {
      toast.error(t('redisAdmin.errTtlInput'))
      return
    }
    ttl = n
  }
  ttlBusy.value = true
  try {
    await setRedisKeyTTL(current.value.id, { db: dbNum.value, key: detail.value.key, ttl })
    toast.success(persist ? t('redisAdmin.ttlPersistOk') : t('redisAdmin.ttlSetOk', { ttl }))
    await openDetail(detail.value.key)
  } catch (err) {
    toast.error(t('redisAdmin.errTtlSet'), { detail: errMsg(err, t('redisAdmin.errNetwork')) })
  } finally {
    ttlBusy.value = false
  }
}

async function askDeleteKey(): Promise<void> {
  if (!current.value || !detail.value || deleteBusy.value) return
  const ok = await confirm.open({
    title: t('redisAdmin.deleteKeyTitle'),
    body: t('redisAdmin.deleteKeyBody', { key: detail.value.key }),
    confirmLabel: t('redisAdmin.deleteKey'),
    variant: 'danger',
  })
  if (!ok) return
  deleteBusy.value = true
  try {
    await deleteRedisKey(current.value.id, detail.value.key, dbNum.value)
    toast.success(t('redisAdmin.keyDeleted'))
    detail.value = null
    // 键已删:当前页重新扫描以反映变化。
    void loadKeys()
  } catch (err) {
    toast.error(t('redisAdmin.errDeleteKey'), { detail: errMsg(err, t('redisAdmin.errNetwork')) })
  } finally {
    deleteBusy.value = false
  }
}

function ttlText(seconds: number): string {
  if (seconds === -1) return t('redisAdmin.ttlPersistent')
  if (seconds <= 0) return '—'
  if (seconds < 3600) return t('redisAdmin.ttlMinutes', { n: Math.max(1, Math.round(seconds / 60)) })
  if (seconds < 86400) return t('redisAdmin.ttlHours', { h: Math.floor(seconds / 3600), m: Math.round((seconds % 3600) / 60) })
  return t('redisAdmin.ttlDays', { d: Math.floor(seconds / 86400), h: Math.round((seconds % 86400) / 3600) })
}

function formatUptime(sec: number): string {
  if (!sec) return '—'
  const d = Math.floor(sec / 86400)
  const h = Math.floor((sec % 86400) / 3600)
  const m = Math.floor((sec % 3600) / 60)
  if (d > 0) return t('redisAdmin.uptimeDays', { d, h })
  if (h > 0) return t('redisAdmin.uptimeHours', { h, m })
  return t('redisAdmin.uptimeMinutes', { m })
}

// ─── 生命周期与联动 ────────────────────────────────────────────────────────────
watch(currentId, () => {
  dbNum.value = current.value?.dbNum ?? 0
  info.value = null
  scanResult.value = null
  detail.value = null
  match.value = ''
  matchApplied.value = ''
  cursorStack.value = ['0']
  pageIdx.value = 0
  if (currentId.value) {
    void loadInfo()
    void loadKeys()
  }
})

onMounted(async () => {
  await loadInstances()
  try {
    objectProviders.value = (await listObjectStores()) ?? []
  } catch {
    objectProviders.value = []
  }
})
</script>

<template>
  <div class="redis-view">
    <header class="view-header">
      <div class="header-main">
        <h1 class="view-title">{{ t('redisAdmin.title') }}</h1>
        <p class="view-sub">{{ t('redisAdmin.subtitle') }}</p>
      </div>
      <div v-if="instances.length > 0" class="header-tools">
        <select v-model="currentId" class="instance-select" :aria-label="t('redisAdmin.pickInstance')">
          <option v-for="i in instances" :key="i.id" :value="i.id">{{ i.name }} · {{ i.endpoint }}</option>
        </select>
        <span v-if="current" class="mode-chip" :class="current.readonly ? 'mode-chip--ro' : 'mode-chip--rw'">
          <NIcon :size="13"><Lock v-if="current.readonly" /><LockOpen v-else /></NIcon>
          {{ current.readonly ? t('redisAdmin.readonlyMode') : t('redisAdmin.writeMode') }}
        </span>
        <button v-if="current?.readonly" class="btn-secondary" :disabled="readonlyBusy" @click="askUnlock">
          {{ t('redisAdmin.unlock') }}
        </button>
        <button v-else class="btn-secondary" :disabled="readonlyBusy" @click="relock">
          {{ t('redisAdmin.relock') }}
        </button>
        <button class="btn-secondary" :disabled="testing" @click="runTest">
          <NIcon :size="13"><PlugConnected /></NIcon>
          {{ t('redisAdmin.test') }}
        </button>
      </div>
    </header>

    <div v-if="loadingInstances" class="redis-state">{{ t('redisAdmin.loading') }}</div>

    <!-- 无实例:引导去设置页登记 -->
    <div v-else-if="instances.length === 0" class="empty-panel">
      <NIcon :size="26"><Bolt /></NIcon>
      <p class="empty-title">{{ t('redisAdmin.noInstances') }}</p>
      <p class="empty-desc">{{ t('redisAdmin.noInstancesDesc') }}</p>
      <router-link to="/settings/redis" class="btn-primary">{{ t('redisAdmin.goToSettings') }}</router-link>
    </div>

    <template v-else>
      <!-- 概览条 + 逻辑库切换 -->
      <div class="info-bar">
        <template v-if="info">
          <span class="info-chip mono">v{{ info.version || '?' }}</span>
          <span class="info-chip">{{ t('redisAdmin.uptime') }} {{ formatUptime(info.uptimeSeconds) }}</span>
          <span class="info-chip">{{ t('redisAdmin.memory') }} {{ info.usedMemory || '—' }}</span>
          <span class="info-chip">{{ t('redisAdmin.clients') }} {{ info.connectedClients }}</span>
          <span class="info-chip info-chip--strong">{{ t('redisAdmin.dbSize', { n: info.dbSize }) }}</span>
        </template>
        <span v-else-if="loadingInfo" class="info-chip">{{ t('redisAdmin.loading') }}</span>
        <span class="info-spacer" />
        <label class="db-picker">
          {{ t('redisAdmin.dbLabel') }}
          <select v-model="dbNum" class="db-select" @change="onDbChange">
            <option v-for="db in DB_COUNT" :key="db - 1" :value="db - 1">
              db{{ db - 1 }}}{{ dbKeysLabel(db - 1) }}
            </option>
          </select>
        </label>
      </div>

      <!-- 键浏览工具条 -->
      <div class="keys-toolbar">
        <input
          v-model="match"
          class="match-input mono"
          type="text"
          :placeholder="t('redisAdmin.matchPlaceholder')"
          @keyup.enter="applyMatch"
        />
        <button class="btn-secondary" :disabled="scanning" @click="applyMatch">
          <NIcon :size="13"><Search /></NIcon>{{ t('redisAdmin.search') }}
        </button>
        <button class="icon-btn" :title="t('common.refresh')" :disabled="scanning" @click="loadKeys">
          <NIcon :size="14"><Refresh /></NIcon>
        </button>
        <span class="row-count">{{ t('redisAdmin.keyCount', { n: scanResult?.keys.length ?? 0 }) }}</span>
        <select v-model="pageSize" class="page-size" :aria-label="t('redisAdmin.pageSize')" @change="onPageSizeChange">
          <option v-for="n in PAGE_SIZES" :key="n" :value="n">{{ n }} / {{ t('redisAdmin.pageUnit') }}</option>
        </select>
      </div>

      <div v-if="scanning" class="redis-state">{{ t('redisAdmin.loading') }}</div>
      <div v-else-if="scanError" class="redis-error">
        <p>{{ scanError }}</p>
        <button class="btn-secondary" @click="loadKeys">{{ t('redisAdmin.retry') }}</button>
      </div>
      <div v-else-if="!scanResult || scanResult.keys.length === 0" class="empty-panel empty-panel--slim">
        <NIcon :size="22"><KeyIcon /></NIcon>
        <p class="empty-title">{{ t('redisAdmin.emptyKeys') }}</p>
      </div>
      <div v-else class="keys-panel">
        <div class="keys-list grid-scroll">
          <table class="data-table">
            <thead>
              <tr><th>{{ t('redisAdmin.colKey') }}</th></tr>
            </thead>
            <tbody>
              <tr
                v-for="k in scanResult.keys"
                :key="k"
                class="row-click"
                :class="{ 'row-selected': detail?.key === k }"
                @click="openDetail(k)"
              >
                <td class="cell-strong mono name-cell">{{ k }}</td>
              </tr>
            </tbody>
          </table>
        </div>

        <!-- 键详情面板 -->
        <aside class="detail-panel">
          <div v-if="loadingDetail" class="detail-empty">{{ t('redisAdmin.loading') }}</div>
          <div v-else-if="!detail" class="detail-empty">{{ t('redisAdmin.pickKey') }}</div>
          <template v-else>
            <div class="detail-head">
              <span class="type-badge mono">{{ detail.type }}</span>
              <strong class="mono detail-key" :title="detail.key">{{ detail.key }}</strong>
              <span class="detail-spacer" />
              <button
                class="icon-btn icon-btn--danger"
                :title="t('redisAdmin.deleteKey')"
                :aria-label="t('redisAdmin.deleteKey')"
                :disabled="deleteBusy || current?.readonly"
                @click="askDeleteKey"
              >
                <NIcon :size="14"><Trash /></NIcon>
              </button>
            </div>
            <dl class="detail-meta">
              <div><dt>{{ t('redisAdmin.ttl') }}</dt><dd>{{ ttlText(detail.ttlSeconds) }}</dd></div>
              <div><dt>{{ t('redisAdmin.memoryUsage') }}</dt><dd>{{ detail.memoryBytes ? `${detail.memoryBytes} B` : '—' }}</dd></div>
              <div v-if="detail.type !== 'string'"><dt>{{ t('redisAdmin.length') }}</dt><dd>{{ detail.len }}</dd></div>
              <div v-if="detail.type === 'string'"><dt>{{ t('redisAdmin.strLen') }}</dt><dd>{{ detail.strLen }}</dd></div>
            </dl>

            <!-- TTL 编辑(须解锁) -->
            <div class="ttl-editor">
              <NIcon :size="14" class="ttl-icon"><Clock /></NIcon>
              <input
                v-model="ttlInput"
                class="ttl-input mono"
                type="number"
                min="0"
                :placeholder="t('redisAdmin.ttlPlaceholder')"
                :disabled="current?.readonly"
              />
              <span class="ttl-unit">{{ t('redisAdmin.ttlUnit') }}</span>
              <button class="btn-secondary" :disabled="ttlBusy || current?.readonly" @click="applyTTL(false)">
                {{ t('redisAdmin.ttlSet') }}
              </button>
              <button class="btn-secondary" :disabled="ttlBusy || current?.readonly" @click="applyTTL(true)">
                {{ t('redisAdmin.ttlPersist') }}
              </button>
            </div>
            <p v-if="current?.readonly" class="hint-line">{{ t('redisAdmin.readonlyHint') }}</p>

            <!-- 值预览(服务端截断) -->
            <div class="value-wrap">
              <template v-if="detail.type === 'string'">
                <pre class="value-pre mono">{{ detail.strValue || '—' }}</pre>
                <p v-if="detail.truncated" class="hint-line">{{ t('redisAdmin.valueTruncated', { n: detail.strLen }) }}</p>
              </template>
              <table v-else-if="detail.type === 'hash'" class="data-table">
                <thead><tr><th>{{ t('redisAdmin.field') }}</th><th>{{ t('redisAdmin.value') }}</th></tr></thead>
                <tbody>
                  <tr v-for="m in detail.hashMembers ?? []" :key="m.field">
                    <td class="mono cell-strong">{{ m.field }}</td>
                    <td class="mono">{{ m.value }}</td>
                  </tr>
                </tbody>
              </table>
              <template v-else-if="detail.type === 'list'">
                <ol class="value-list mono">
                  <li v-for="(item, i) in detail.listItems ?? []" :key="i">{{ item }}</li>
                </ol>
              </template>
              <template v-else-if="detail.type === 'set'">
                <ul class="value-list mono">
                  <li v-for="(item, i) in detail.setItems ?? []" :key="i">{{ item }}</li>
                </ul>
              </template>
              <table v-else-if="detail.type === 'zset'" class="data-table">
                <thead><tr><th>{{ t('redisAdmin.member') }}</th><th>{{ t('redisAdmin.score') }}</th></tr></thead>
                <tbody>
                  <tr v-for="z in detail.zsetItems ?? []" :key="z.member">
                    <td class="mono cell-strong">{{ z.member }}</td>
                    <td class="mono">{{ z.score }}</td>
                  </tr>
                </tbody>
              </table>
              <p v-else class="hint-line">{{ t('redisAdmin.typeNoPreview', { type: detail.type }) }}</p>
            </div>
          </template>
        </aside>
      </div>

      <!-- SCAN 游标分页:顺序翻页;下一页由 nextCursor 驱动,上一页回放历史栈 -->
      <div class="pager">
        <button class="icon-btn" :disabled="pageIdx === 0 || scanning" :aria-label="t('redisAdmin.prevPage')" @click="prevPage">
          <NIcon :size="13"><ChevronLeft /></NIcon>
        </button>
        <span class="pager-info">
          {{ t('redisAdmin.pageOf', { n: pageIdx + 1 }) }}
          <template v-if="scanResult && scanResult.nextCursor === '0'"> · {{ t('redisAdmin.lastPage') }}</template>
        </span>
        <button
          class="icon-btn"
          :disabled="!scanResult || scanResult.nextCursor === '0' || scanning"
          :aria-label="t('redisAdmin.nextPage')"
          @click="nextPage"
        >
          <NIcon :size="13"><ChevronRight /></NIcon>
        </button>
      </div>

      <!-- 备份恢复(实例级;默认折叠,展开即本实例的备份面板) -->
      <section v-if="current" class="backup-section" :aria-label="t('dbBackup.section')">
        <button class="backup-toggle" :aria-expanded="backupOpen" @click="backupOpen = !backupOpen">
          <NIcon :size="14" :class="{ 'backup-chevron--open': backupOpen }"><ChevronRight /></NIcon>
          {{ t('dbBackup.section') }}
        </button>
        <div v-if="backupOpen" class="backup-body">
          <BackupPanel
            :key="current.id"
            kind="redis"
            :instance-id="current.id"
            :instance-name="current.name"
            :providers="objectProviders"
            :restore-gate="restoreGate"
          />
        </div>
      </section>
    </template>
  </div>
</template>

<style scoped>
/* 备份折叠区 */
.backup-section { border: 1px solid var(--color-border); border-radius: 10px; margin-top: 18px; overflow: hidden; }
.backup-toggle { display: flex; align-items: center; gap: 8px; width: 100%; padding: 11px 14px; background: transparent; border: none; color: inherit; font-size: 13px; font-weight: 600; cursor: pointer; }
.backup-toggle :deep(svg) { transition: transform .15s ease; }
.backup-chevron--open { transform: rotate(90deg); }
.backup-body { padding: 14px; border-top: 1px solid var(--color-border); }
.redis-view {
  display: flex;
  flex-direction: column;
  gap: 18px;
}

.view-header {
  display: flex;
  align-items: flex-start;
  gap: 16px;
  padding-bottom: 18px;
  border-bottom: 1px solid var(--color-border);
}
.header-main { flex: 1; }
.view-title {
  font-size: var(--text-display);
  font-weight: 700;
  letter-spacing: -0.02em;
  color: var(--color-text);
}
.view-sub {
  font-size: var(--text-body);
  color: var(--color-faint);
  margin-top: 2px;
}
.header-tools {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}
.instance-select {
  height: 34px;
  padding: 0 10px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  color: var(--color-text);
  font-family: var(--font-sans);
  font-size: 0.84rem;
  max-width: 380px;
}
.mode-chip {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 0.74rem;
  font-weight: 600;
  padding: 4px 10px;
  border-radius: var(--rounded-full);
  border: 1px solid var(--color-border-strong);
}
.mode-chip--ro { color: var(--color-faint); }
.mode-chip--rw { color: var(--color-green); border-color: var(--color-green); }

.mono { font-family: var(--font-mono); }

.empty-panel {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 10px;
  padding: 72px 32px;
  text-align: center;
  border: 1.5px dashed var(--color-border-strong);
  border-radius: var(--rounded-card);
  color: var(--color-dim);
}
.empty-panel--slim { padding: 40px 32px; }
.empty-title {
  font-size: 1rem;
  font-weight: 600;
  color: var(--color-text);
}
.empty-desc {
  font-size: 0.82rem;
  color: var(--color-faint);
  max-width: 52ch;
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
  text-decoration: none;
}
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
}
.btn-secondary:disabled { opacity: 0.55; cursor: not-allowed; }

.info-bar {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.info-chip {
  font-size: 0.74rem;
  color: var(--color-dim);
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  padding: 4px 10px;
  border-radius: var(--rounded-full);
}
.info-chip--strong {
  color: var(--color-primary);
  border-color: var(--color-primary);
  font-weight: 600;
}
.info-spacer { flex: 1; }
.db-picker {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  font-size: 0.78rem;
  color: var(--color-dim);
}
.db-select {
  height: 30px;
  padding: 0 8px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  color: var(--color-text);
  font-size: 0.78rem;
}

.keys-toolbar {
  display: flex;
  align-items: center;
  gap: 10px;
}
.match-input {
  height: 32px;
  flex: 1;
  max-width: 420px;
  padding: 0 12px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  color: var(--color-text);
  font-size: 0.8rem;
}
.match-input:focus { outline: none; border-color: var(--color-primary); }
.row-count { font-size: 0.74rem; color: var(--color-faint); }
.page-size {
  height: 32px;
  padding: 0 8px;
  margin-left: auto;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  color: var(--color-text);
  font-size: 0.78rem;
}

.redis-state {
  padding: 48px 24px;
  text-align: center;
  color: var(--color-faint);
  font-size: 0.84rem;
}
.redis-error {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 12px;
  padding: 40px 24px;
  text-align: center;
  border: 1px solid var(--color-red-line);
  background: var(--color-red-soft);
  border-radius: var(--rounded);
  color: var(--color-red);
  font-size: 0.82rem;
  word-break: break-all;
}

.keys-panel {
  display: grid;
  grid-template-columns: minmax(0, 1fr) 380px;
  gap: 14px;
  align-items: start;
}
.grid-scroll {
  overflow: auto;
  max-height: 560px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
}
.data-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 0.78rem;
}
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
}
.data-table td {
  padding: 8px 10px;
  border-bottom: 1px solid var(--color-border);
  color: var(--color-dim);
  vertical-align: top;
}
.data-table tbody tr:last-child td { border-bottom: none; }
.row-click { cursor: pointer; transition: background-color var(--duration-fast); }
.row-click:hover { background: var(--color-inset); }
.row-selected { background: var(--color-primary-soft); }
.cell-strong { color: var(--color-text); font-weight: 500; }
.name-cell {
  max-width: 480px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.detail-panel {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 14px;
  background: var(--color-card);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
}
.detail-empty {
  padding: 32px 12px;
  text-align: center;
  color: var(--color-faint);
  font-size: 0.8rem;
}
.detail-head {
  display: flex;
  align-items: center;
  gap: 8px;
}
.type-badge {
  font-size: 0.7rem;
  font-weight: 600;
  padding: 3px 8px;
  border-radius: var(--rounded-full);
  background: var(--color-primary-soft);
  color: var(--color-primary);
  flex-shrink: 0;
}
.detail-key {
  font-size: 0.84rem;
  color: var(--color-text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.detail-spacer { flex: 1; }
.detail-meta {
  margin: 0;
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 8px;
}
.detail-meta > div {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.detail-meta dt {
  font-size: 0.68rem;
  text-transform: uppercase;
  letter-spacing: 0.04em;
  color: var(--color-faint);
}
.detail-meta dd {
  margin: 0;
  font-size: 0.82rem;
  color: var(--color-text);
}

.ttl-editor {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.ttl-icon { color: var(--color-faint); }
.ttl-input {
  width: 110px;
  height: 30px;
  padding: 0 8px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  color: var(--color-text);
  font-size: 0.78rem;
}
.ttl-input:focus { outline: none; border-color: var(--color-primary); }
.ttl-unit { font-size: 0.72rem; color: var(--color-faint); }
.hint-line {
  font-size: 0.72rem;
  color: var(--color-faint);
  margin: 0;
}

.value-wrap {
  display: flex;
  flex-direction: column;
  gap: 6px;
  min-height: 0;
}
.value-pre {
  margin: 0;
  padding: 10px 12px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  font-size: 0.76rem;
  line-height: 1.5;
  white-space: pre-wrap;
  word-break: break-all;
  max-height: 320px;
  overflow: auto;
}
.value-list {
  margin: 0;
  padding: 10px 12px 10px 30px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  font-size: 0.76rem;
  line-height: 1.7;
  word-break: break-all;
  max-height: 320px;
  overflow: auto;
}

.pager {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 10px;
}
.pager-info { font-size: 0.78rem; color: var(--color-dim); }
.icon-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 30px;
  height: 30px;
  background: var(--color-card);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded);
  color: var(--color-dim);
  cursor: pointer;
}
.icon-btn:disabled { opacity: 0.45; cursor: not-allowed; }
.icon-btn:hover:not(:disabled) { color: var(--color-text); }
.icon-btn--danger { color: var(--color-red); }
.icon-btn--danger:hover:not(:disabled) { color: var(--color-red); }

@media (max-width: 1100px) {
  .keys-panel { grid-template-columns: 1fr; }
}
</style>
