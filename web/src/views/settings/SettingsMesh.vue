<!--
  SettingsMesh.vue — 集群设置(本节点身份 / EasyTier 组网 / 三张授权单 / 节点注册表)。

  对等无中心:这里配置的是**本节点**——
    · 身份:节点名(展示)与 node_id/公钥(只读;签名鉴权的锚点);
    · EasyTier:托管/外部模式、网络名 + 密钥(密文存储,回显只报已配置)、虚拟 IP、初始 peers;
    · 授权单(默认全拒绝):可见(服务器/项目/运行)、允许推送、允许触发 —— 均**对哪些节点开放**;
    · 节点注册表:发现(auto)登记 + 手工补录(manual;发现兜底,可补对外访问地址)。
-->
<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  revealMeshNetworkSecret,
  downloadMeshClientConfig,
  fetchMeshClientConfigText,
  getMeshOverview,
  triggerEasyTierDownload,
  type EasyTierBinaries,
  type MeshOverview,
  getMeshSettings,
  saveMeshSettings,
  getMeshACL,
  saveMeshACL,
  listMeshPeers,
  saveMeshPeer,
  deleteMeshPeer,
  type MeshSettings,
  type MeshPeer,
} from '../../api/mesh'
import AppButton from '../../components/ui/AppButton.vue'

const { t } = useI18n()

type LoadState = 'loading' | 'ready' | 'error'
const state = ref<LoadState>('loading')
const errorMsg = ref('')
// actionError 是保存/授权/补录等**动作**失败的可视横幅(与初始加载失败分开:
// 此前动作失败只写 errorMsg,而它只在加载态渲染 → 保存失败毫无提示,像存上了)。
const actionError = ref('')
const saved = ref(false)
const saving = ref(false)
let savedTimer: number | undefined

/** markSaved 显示「已保存 ✓」并在 3s 后自动消失;同时清掉动作错误横幅。 */
function markSaved(): void {
  actionError.value = ''
  saved.value = true
  if (savedTimer !== undefined) window.clearTimeout(savedTimer)
  savedTimer = window.setTimeout(() => { saved.value = false }, 3000)
}

const settings = ref<MeshSettings | null>(null)
const peers = ref<MeshPeer[]>([])

// 表单(密钥类只在新填时提交)
const form = ref({
  name: '',
  enabled: false,
  easytierMode: 'managed' as 'managed' | 'external',
  easytierRpc: '127.0.0.1:15888',
  networkName: '',
  networkSecret: '',
  virtualIp: '',
  peers: '',
  proxyNetworks: '',
  acceptNetworks: '',
  enableExitNode: false,
})
const acl = ref({ visibleTo: [] as string[], pushAllowed: [] as string[], triggerAllowed: [] as string[], forwardAllowed: [] as string[], buildAllowed: [] as string[] })
const manualPeer = ref({ nodeId: '', name: '', virtualIp: '', apiPort: 8080, publicUrl: '' })
// EasyTier 二进制状态(就位版本/自动下载进度;来自 overview)。
const binaries = ref<EasyTierBinaries | null>(null)
// 组网密钥回显:点击「显示」拉取已存明文(带审计);「生成」本地造 24 位随机强密钥。
const secretRevealed = ref(false)

async function toggleSecret(): Promise<void> {
  if (!secretRevealed.value) {
    try {
      const res = await revealMeshNetworkSecret()
      if (res.secret) form.value.networkSecret = res.secret
    } catch (err) {
      actionError.value = err instanceof Error ? err.message : String(err)
      return
    }
  }
  secretRevealed.value = !secretRevealed.value
}

function generateSecret(): void {
  // 24 位 base62 随机(crypto.getRandomValues);保存后到各对端「设置→集群」粘贴同一密钥。
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789'
  const bytes = new Uint8Array(24)
  crypto.getRandomValues(bytes)
  form.value.networkSecret = Array.from(bytes, (b) => alphabet[b % alphabet.length]).join('')
  secretRevealed.value = true
}

// copied:复制成功后按钮短暂显示「已复制」。
const copied = ref(false)
let copiedTimer: number | undefined

/** copyText 写剪贴板;非安全上下文(http 局域网)时 navigator.clipboard 不可用,回退 execCommand。 */
async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    // 落入下方回退。
  }
  const ta = document.createElement('textarea')
  ta.value = text
  ta.style.position = 'fixed'
  ta.style.opacity = '0'
  document.body.appendChild(ta)
  ta.select()
  let ok = false
  try {
    ok = document.execCommand('copy')
  } catch {
    ok = false
  }
  document.body.removeChild(ta)
  return ok
}

/** 一键复制:输入框有内容复制输入框(所见即所复制);为空则直接取服务端已存密钥 —— 不需要先「显示」。 */
async function copySecret(): Promise<void> {
  let text = form.value.networkSecret.trim()
  if (!text) {
    try {
      const res = await revealMeshNetworkSecret()
      text = res.secret
    } catch (err) {
      actionError.value = err instanceof Error ? err.message : String(err)
      return
    }
  }
  if (!text) return
  if (await copyText(text)) {
    actionError.value = ''
    copied.value = true
    if (copiedTimer !== undefined) window.clearTimeout(copiedTimer)
    copiedTimer = window.setTimeout(() => { copied.value = false }, 2000)
  } else {
    actionError.value = t('cluster.settings.secretCopyFailed')
  }
}

// 运行状态(设置页顶部状态条 + EasyTier 区二进制行共用):轮询 overview。
const overview = ref<MeshOverview | null>(null)
let statusTimer: number | undefined

async function loadStatus(): Promise<void> {
  try {
    const ov = await getMeshOverview()
    overview.value = ov
    binaries.value = ov.binaries ?? null
  } catch {
    // 状态拉取失败不弹横幅(轮询噪音):保留上一次快照,状态条自会显示其旧值。
  }
}

/** 状态条数据(计算属性;overview 为空时各格显示 —)。 */
const stSelfIp = computed(() => overview.value?.easytier.selfIp || '—')
const stNetwork = computed(() => overview.value?.settings.networkName || '—')
const stOnline = computed(() => {
  const peers = overview.value?.peers ?? []
  return `${peers.filter((x) => x.online).length}/${peers.length}`
})
const stProcess = computed(() => {
  const ov = overview.value
  if (!ov) return { text: '…', ok: false }
  if (ov.externalMode) return { text: t('cluster.et.readonly'), ok: true }
  if (ov.settings.enabled && ov.supervisor?.running) return { text: t('cluster.et.running'), ok: true }
  return { text: t('cluster.et.stopped'), ok: false }
})
/** 需要人看见的最近异常(EasyTier 状态错误 > 托管进程错误),空 = 无。 */
const stError = computed(() => {
  const ov = overview.value
  if (!ov) return ''
  return ov.easytier.err || ov.supervisor?.lastErr || ''
})

async function downloadBinaries(): Promise<void> {
  try {
    await triggerEasyTierDownload()
    if (binaries.value) binaries.value = { ...binaries.value, downloading: true, lastErr: '' }
    // 异步下载:3s 后探一次进度,仍在下载则继续轮询。
    const poll = window.setInterval(async () => {
      await loadStatus()
      if (binaries.value && !binaries.value.downloading) {
        window.clearInterval(poll)
        await load()
      }
    }, 3000)
  } catch (err) {
    actionError.value = err instanceof Error ? err.message : String(err)
  }
}

// 导出 Win11 客户端(pwclient)加入配置:首选「复制文本」弹窗(pwclient 粘贴即用),
// 弹窗内保留文件下载作为兜底。
const clientYaml = ref('')
const clientYamlOpen = ref(false)
const yamlCopied = ref(false)
let yamlCopiedTimer: number | undefined

async function exportClientConfig(): Promise<void> {
  try {
    clientYaml.value = await fetchMeshClientConfigText()
    actionError.value = ''
    yamlCopied.value = false
    clientYamlOpen.value = true
  } catch (err) {
    actionError.value = err instanceof Error ? err.message : String(err)
  }
}

async function copyClientYaml(): Promise<void> {
  try {
    await navigator.clipboard.writeText(clientYaml.value)
  } catch {
    // 非安全上下文(http)无 Clipboard API:textarea 全选兜底,提示手动 Ctrl+C。
    const el = document.getElementById('client-yaml-text') as HTMLTextAreaElement | null
    el?.focus()
    el?.select()
    return
  }
  yamlCopied.value = true
  if (yamlCopiedTimer !== undefined) window.clearTimeout(yamlCopiedTimer)
  yamlCopiedTimer = window.setTimeout(() => { yamlCopied.value = false }, 3000)
}

function downloadClientYaml(): void {
  const blob = new Blob([clientYaml.value], { type: 'application/yaml' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = 'pipewright-client.yaml'
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
  URL.revokeObjectURL(url)
}

async function load(): Promise<void> {
  state.value = 'loading'
  try {
    const [st, aclRes, peerRes] = await Promise.all([getMeshSettings(), getMeshACL(), listMeshPeers()])
    settings.value = st
    form.value = {
      name: st.name,
      enabled: st.enabled,
      easytierMode: st.easytierMode,
      easytierRpc: st.easytierRpc || '127.0.0.1:15888',
      networkName: st.networkName,
      networkSecret: '',
      virtualIp: st.virtualIp,
      peers: st.peers.join('\n'),
      proxyNetworks: (st.proxyNetworksCustom ?? []).join('\n'),
      acceptNetworks: (st.acceptNetworksCustom ?? []).join('\n'),
      enableExitNode: st.enableExitNode ?? false,
    }
    acl.value = {
      visibleTo: aclRes.visibleTo ?? [],
      pushAllowed: aclRes.pushAllowed ?? [],
      triggerAllowed: aclRes.triggerAllowed ?? [],
      forwardAllowed: aclRes.forwardAllowed ?? [],
      buildAllowed: aclRes.buildAllowed ?? [],
    }
    peers.value = peerRes.items ?? []
    state.value = 'ready'
  } catch (err) {
    errorMsg.value = err instanceof Error ? err.message : String(err)
    state.value = 'error'
  }
}

async function persistSettings(): Promise<void> {
  if (!settings.value) return
  saving.value = true
  saved.value = false
  try {
    const body: Record<string, unknown> = {
      name: form.value.name,
      enabled: form.value.enabled,
      easytierMode: form.value.easytierMode,
      easytierRpc: form.value.easytierRpc,
      networkName: form.value.networkName,
      virtualIp: form.value.virtualIp,
      peers: form.value.peers.split('\n').map((s) => s.trim()).filter(Boolean),
      proxyNetworksCustom: form.value.proxyNetworks.split('\n').map((s) => s.trim()).filter(Boolean),
      acceptNetworksCustom: form.value.acceptNetworks.split('\n').map((s) => s.trim()).filter(Boolean),
      enableExitNode: form.value.enableExitNode,
    }
    if (form.value.networkSecret.trim() !== '') body.networkSecret = form.value.networkSecret.trim()
    const st = await saveMeshSettings(body as never)
    settings.value = st
    // 用服务端权威值回填表单(密钥只写不回显,置空):所见即已存,消除「存没存上」疑惑。
    form.value = {
      name: st.name,
      enabled: st.enabled,
      easytierMode: st.easytierMode,
      easytierRpc: st.easytierRpc || '127.0.0.1:15888',
      networkName: st.networkName,
      networkSecret: '',
      virtualIp: st.virtualIp,
      peers: st.peers.join('\n'),
      proxyNetworks: (st.proxyNetworksCustom ?? []).join('\n'),
      acceptNetworks: (st.acceptNetworksCustom ?? []).join('\n'),
      enableExitNode: st.enableExitNode ?? false,
    }
    secretRevealed.value = false
    markSaved()
    void loadStatus()
  } catch (err) {
    actionError.value = err instanceof Error ? err.message : String(err)
  } finally {
    saving.value = false
  }
}

async function persistACL(): Promise<void> {
  saving.value = true
  saved.value = false
  try {
    await saveMeshACL(acl.value)
    markSaved()
  } catch (err) {
    actionError.value = err instanceof Error ? err.message : String(err)
  } finally {
    saving.value = false
  }
}

async function addManualPeer(): Promise<void> {
  if (!manualPeer.value.nodeId.trim()) return
  try {
    await saveMeshPeer({
      nodeId: manualPeer.value.nodeId.trim(),
      name: manualPeer.value.name.trim(),
      virtualIp: manualPeer.value.virtualIp.trim(),
      apiPort: Number(manualPeer.value.apiPort) || 8080,
      publicUrl: manualPeer.value.publicUrl.trim(),
    })
    manualPeer.value = { nodeId: '', name: '', virtualIp: '', apiPort: 8080, publicUrl: '' }
    await load()
  } catch (err) {
    actionError.value = err instanceof Error ? err.message : String(err)
  }
}

async function removePeer(nodeId: string): Promise<void> {
  try {
    await deleteMeshPeer(nodeId)
    // 同步从授权单里剔除,避免悬挂引用。
    acl.value.visibleTo = acl.value.visibleTo.filter((x) => x !== nodeId)
    acl.value.pushAllowed = acl.value.pushAllowed.filter((x) => x !== nodeId)
    acl.value.triggerAllowed = acl.value.triggerAllowed.filter((x) => x !== nodeId)
    acl.value.forwardAllowed = acl.value.forwardAllowed.filter((x) => x !== nodeId)
    acl.value.buildAllowed = acl.value.buildAllowed.filter((x) => x !== nodeId)
    await Promise.all([saveMeshACL(acl.value), load()])
  } catch (err) {
    actionError.value = err instanceof Error ? err.message : String(err)
  }
}

/** 授权单编辑:点击行内徽标切换某节点的授权。 */
function toggleACL(list: 'visibleTo' | 'pushAllowed' | 'triggerAllowed' | 'forwardAllowed' | 'buildAllowed', nodeId: string): void {
  const cur = acl.value[list]
  const idx = cur.indexOf(nodeId)
  if (idx >= 0) cur.splice(idx, 1)
  else cur.push(nodeId)
}

const knownPeers = computed(() => peers.value)

onMounted(() => {
  void load()
  void loadStatus()
  statusTimer = window.setInterval(() => void loadStatus(), 10_000)
})

onUnmounted(() => {
  if (statusTimer !== undefined) window.clearInterval(statusTimer)
})
</script>

<template>
  <div class="mesh-settings">
    <p v-if="state === 'loading'">…</p>
    <p v-else-if="state === 'error'" class="err">{{ errorMsg }}</p>
    <div v-if="actionError" class="action-banner" role="alert">
      {{ t('cluster.settings.actionFailed') }}:{{ actionError }}
      <button class="link-btn" type="button" @click="actionError = ''">✕</button>
    </div>

    <template v-if="state === 'ready' && settings">
      <!-- 运行状态条:配置完立即能看到「进程起没起、拿到虚拟 IP 没有、几个人在线」。 -->
      <section class="block">
        <div class="status-head">
          <h2>{{ t('cluster.settings.statusTitle') }}</h2>
          <router-link to="/cluster" class="link-btn">{{ t('cluster.settings.viewCluster') }} →</router-link>
        </div>
        <div class="status-grid">
          <div class="st-item">
            <span class="et-label">{{ t('cluster.et.process') }}</span>
            <span class="st-value" :class="stProcess.ok ? 'ok-text' : 'warn-text'">{{ stProcess.text }}</span>
          </div>
          <div class="st-item">
            <span class="et-label">{{ t('cluster.et.selfIp') }}</span>
            <span class="st-value mono">{{ stSelfIp }}</span>
          </div>
          <div class="st-item">
            <span class="et-label">{{ t('cluster.et.network') }}</span>
            <span class="st-value mono">{{ stNetwork }}</span>
          </div>
          <div class="st-item">
            <span class="et-label">{{ t('cluster.et.peers') }}</span>
            <span class="st-value">{{ stOnline }}</span>
          </div>
        </div>
        <p v-if="stError" class="et-hint warn-text">{{ stError }}</p>
        <p v-else-if="!settings.enabled" class="et-hint">{{ t('cluster.et.disabledHint') }}</p>
      </section>

      <!-- 身份与组网 -->
      <section class="block">
        <h2>{{ t('cluster.settings.identity') }}</h2>
        <div class="grid2">
          <label class="fld"><span>{{ t('cluster.settings.nodeName') }}</span><input v-model="form.name" class="inp" type="text" /></label>
          <label class="fld"><span>{{ t('cluster.settings.nodeId') }}</span><input :value="settings.nodeId" class="inp mono" type="text" readonly /></label>
        </div>
        <div class="grid2">
          <label class="fld"><span>{{ t('cluster.settings.publicKey') }}</span><input :value="settings.publicKey" class="inp mono" type="text" readonly /></label>
          <div class="fld">
            <span>{{ t('cluster.settings.enabled') }}</span>
            <label class="switch"><input v-model="form.enabled" type="checkbox" /> {{ t('cluster.settings.enabledHint') }}</label>
          </div>
        </div>
        <div class="row-end">
          <span v-if="saved" class="ok-text">{{ t('cluster.settings.saved') }}</span>
          <AppButton :loading="saving" @click="persistSettings">{{ t('cluster.settings.save') }}</AppButton>
        </div>
        <p class="hint">{{ t('cluster.settings.identitySaveHint') }}</p>
      </section>

      <section class="block">
        <h2>{{ t('cluster.settings.easytier') }}</h2>
        <div class="grid2">
          <label class="fld"><span>{{ t('cluster.settings.mode') }}</span><select v-model="form.easytierMode" class="inp">
              <option value="managed">{{ t('cluster.settings.modeManaged') }}</option>
              <option value="external">{{ t('cluster.settings.modeExternal') }}</option>
            </select></label>
          <label class="fld"><span>{{ t('cluster.settings.rpc') }}</span><input v-model="form.easytierRpc" class="inp mono" type="text" /></label>
        </div>
        <div class="grid2">
          <label class="fld"><span>{{ t('cluster.settings.networkName') }}</span><input v-model="form.networkName" class="inp" type="text" placeholder="pipewright-net" /></label>
          <div class="fld">
            <span>{{ t('cluster.settings.networkSecret') + (settings.hasSecret ? ' ✓' : '') }}</span>
            <div class="secret-row">
              <input
                v-model="form.networkSecret"
                class="inp mono"
                :type="secretRevealed ? 'text' : 'password'"
                autocomplete="new-password"
                :placeholder="settings.hasSecret ? t('cluster.settings.secretKept') : t('cluster.settings.secretPlaceholder')"
              />
              <button class="mini-btn" type="button" @click="toggleSecret">{{ secretRevealed ? t('cluster.settings.secretHide') : t('cluster.settings.secretShow') }}</button>
              <button class="mini-btn" type="button" @click="generateSecret">{{ t('cluster.settings.secretGenerate') }}</button>
              <button class="mini-btn" type="button" @click="copySecret">{{ copied ? t('cluster.settings.secretCopied') : t('cluster.settings.secretCopy') }}</button>
            </div>
            <span class="hint">{{ t('cluster.settings.secretHint') }}</span>
          </div>
        </div>
        <div class="grid2">
          <label class="fld"><span>{{ t('cluster.settings.virtualIp') }}</span><input v-model="form.virtualIp" class="inp mono" type="text" :placeholder="t('cluster.settings.dhcp')" /></label>
          <label class="fld"><span>{{ t('cluster.settings.peers') }}</span><textarea v-model="form.peers" class="inp mono" rows="3" placeholder="tcp://1.2.3.4:11010"></textarea></label>
        </div>
        <label class="fld"><span>{{ t('cluster.settings.proxyNetworks') }}</span><textarea v-model="form.proxyNetworks" class="inp mono" rows="3" placeholder="192.168.30.0/24"></textarea></label>
        <label class="fld"><span>{{ t('cluster.settings.acceptNetworks') }}</span><textarea v-model="form.acceptNetworks" class="inp mono" rows="2" placeholder="172.20.0.0/16"></textarea></label>
        <label class="switch"><input v-model="form.enableExitNode" type="checkbox" /> {{ t('cluster.settings.enableExitNode') }}</label>
        <div class="bin-row">
          <span class="bin-label">{{ t('cluster.settings.binaries') }}</span>
          <span class="bin-status" :class="{ 'ok-text': binaries?.corePath, 'warn-text': !binaries?.corePath }">
            <template v-if="binaries?.downloading">{{ t('cluster.settings.binDownloading') }}</template>
            <template v-else-if="binaries?.corePath">
              {{ binaries.version ? `v${binaries.version.replace(/^v/, '')} · ` : '' }}{{ t('cluster.settings.binReady') }}
            </template>
            <template v-else-if="binaries?.lastErr">{{ t('cluster.settings.binFailed') }}:{{ binaries.lastErr }}</template>
            <template v-else>{{ t('cluster.settings.binMissing') }}</template>
          </span>
          <span v-if="binaries?.cliPath || binaries?.corePath" class="bin-paths mono">
            {{ [binaries?.cliPath, binaries?.corePath].filter(Boolean).join(' · ') }}
          </span>
          <AppButton variant="default" :disabled="!!binaries?.downloading" @click="downloadBinaries">
            {{ binaries?.corePath ? t('cluster.settings.binRedownload') : t('cluster.settings.binDownload') }}
          </AppButton>
        </div>
        <div class="row-end">
          <span v-if="saved" class="ok-text">{{ t('cluster.settings.saved') }}</span>
          <AppButton :loading="saving" @click="persistSettings">{{ t('cluster.settings.save') }}</AppButton>
        </div>
      </section>

      <!-- 集群客户端(Win11 pwclient):导出加入配置 —— 首选复制文本(pwclient 粘贴即用)。 -->
      <section class="block">
        <h2>{{ t('cluster.settings.clientTitle') }}</h2>
        <p class="hint">{{ t('cluster.settings.clientHint') }}</p>
        <div class="row-end">
          <AppButton variant="default" @click="exportClientConfig">{{ t('cluster.settings.exportClientConfig') }}</AppButton>
        </div>
      </section>

      <!-- 导出配置弹窗:只读 YAML + 复制(主路径)+ 下载文件(兜底)。 -->
      <div v-if="clientYamlOpen" class="yaml-scrim" @click.self="clientYamlOpen = false">
        <div class="yaml-modal" role="dialog" aria-modal="true" :aria-label="t('cluster.settings.clientYamlTitle')">
          <div class="yaml-head">
            <div>
              <div class="yaml-title">{{ t('cluster.settings.clientYamlTitle') }}</div>
              <div class="yaml-sub">{{ t('cluster.settings.clientYamlHint') }}</div>
            </div>
            <button class="yaml-close" type="button" :aria-label="t('cluster.settings.yamlClose')" @click="clientYamlOpen = false">✕</button>
          </div>
          <div class="yaml-body">
            <textarea id="client-yaml-text" class="yaml-text" readonly :value="clientYaml" rows="14" spellcheck="false" />
          </div>
          <div class="yaml-foot">
            <span v-if="yamlCopied" class="ok-text">{{ t('cluster.settings.yamlCopied') }}</span>
            <AppButton variant="default" @click="downloadClientYaml">{{ t('cluster.settings.downloadYaml') }}</AppButton>
            <AppButton variant="primary" @click="copyClientYaml">{{ t('cluster.settings.copyYaml') }}</AppButton>
          </div>
        </div>
      </div>

      <!-- 授权单 -->
      <section class="block">
        <h2>{{ t('cluster.settings.acl') }}</h2>
        <p class="hint">{{ t('cluster.settings.aclHint') }}</p>
        <div v-for="row in (['visibleTo','pushAllowed','triggerAllowed','forwardAllowed','buildAllowed'] as const)" :key="row" class="acl-row">
          <span class="acl-name">{{ t(`cluster.settings.${row}`) }}</span>
          <span class="acl-badges">
            <span v-if="knownPeers.length === 0" class="muted">{{ t('cluster.settings.noPeers') }}</span>
            <button
              v-for="p in knownPeers"
              :key="p.nodeId"
              class="badge"
              :class="{ on: acl[row].includes(p.nodeId) }"
              type="button"
              @click="toggleACL(row, p.nodeId)"
            >
              {{ p.name || p.nodeId.slice(0, 8) }}
            </button>
          </span>
        </div>
        <div class="row-end">
          <span v-if="saved" class="ok-text">{{ t('cluster.settings.saved') }}</span>
          <AppButton :loading="saving" @click="persistACL">{{ t('cluster.settings.saveAcl') }}</AppButton>
        </div>
      </section>

      <!-- 节点注册表 -->
      <section class="block">
        <h2>{{ t('cluster.settings.peersTitle') }}</h2>
        <table class="peer-list">
          <thead>
            <tr>
              <th>{{ t('cluster.peers.name') }}</th>
              <th>{{ t('cluster.peers.virtualIp') }}</th>
              <th>{{ t('cluster.settings.publicUrl') }}</th>
              <th>src</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="p in peers" :key="p.nodeId">
              <td>{{ p.name || p.nodeId.slice(0, 8) }}</td>
              <td class="mono">{{ p.virtualIp || '—' }}</td>
              <td class="mono">{{ p.publicUrl || '—' }}</td>
              <td>{{ p.source }}</td>
              <td><button class="link-btn" type="button" @click="removePeer(p.nodeId)">{{ t('cluster.settings.removePeer') }}</button></td>
            </tr>
          </tbody>
        </table>
        <div class="grid5">
          <label class="fld"><span>{{ t('cluster.settings.mNodeId') }}</span><input v-model="manualPeer.nodeId" class="inp mono" type="text" /></label>
          <label class="fld"><span>{{ t('cluster.settings.mName') }}</span><input v-model="manualPeer.name" class="inp" type="text" /></label>
          <label class="fld"><span>{{ t('cluster.peers.virtualIp') }}</span><input v-model="manualPeer.virtualIp" class="inp mono" type="text" /></label>
          <label class="fld"><span>{{ t('cluster.settings.mPort') }}</span><input v-model.number="manualPeer.apiPort" class="inp" type="number" /></label>
          <label class="fld"><span>{{ t('cluster.settings.publicUrl') }}</span><input v-model="manualPeer.publicUrl" class="inp" type="text" placeholder="https://…" /></label>
        </div>
        <div class="row-end">
          <AppButton variant="default" @click="addManualPeer">{{ t('cluster.settings.addPeer') }}</AppButton>
        </div>
      </section>
    </template>
  </div>
</template>

<style scoped>
.mesh-settings { display: flex; flex-direction: column; gap: 20px; }
.block { border: 1px solid var(--color-border); border-radius: 10px; padding: 16px; display: flex; flex-direction: column; gap: 12px; }
.block h2 { margin: 0; font-size: 15px; }
.grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
.grid5 { display: grid; grid-template-columns: 1.4fr 1fr 1fr 0.5fr 1.2fr; gap: 8px; }
.inp { width: 100%; box-sizing: border-box; background: var(--color-bg-soft); border: 1px solid var(--color-border); border-radius: 6px; padding: 7px 10px; color: var(--color-text); font-size: 13px; }
.mono { font-family: var(--font-mono, monospace); }
.switch { display: flex; align-items: center; gap: 8px; font-size: 13px; }
.fld { display: flex; flex-direction: column; gap: 4px; font-size: 12px; color: var(--color-text-muted); }
.secret-row { display: flex; gap: 6px; align-items: stretch; }
.secret-row .inp { flex: 1; min-width: 0; }
.mini-btn { border: 1px solid var(--color-border); background: var(--color-bg-soft); color: var(--color-text); border-radius: 6px; padding: 0 10px; font-size: 12px; cursor: pointer; white-space: nowrap; }
.mini-btn:disabled { opacity: 0.5; cursor: default; }
.status-head { display: flex; align-items: baseline; justify-content: space-between; }
.status-head h2 { margin: 0; }
.status-grid { display: flex; flex-wrap: wrap; gap: 24px; }
.st-item { display: flex; flex-direction: column; gap: 2px; }
.st-value { font-size: 14px; font-weight: 600; }
.action-banner { display: flex; align-items: center; gap: 8px; border: 1px solid var(--color-amber); border-radius: 8px; padding: 8px 12px; color: var(--color-amber); font-size: 13px; }
.bin-row { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; }
.bin-label { font-size: 13px; font-weight: 600; }
.bin-status { font-size: 13px; flex: 1; min-width: 200px; }
.bin-paths { width: 100%; font-size: 11px; color: var(--color-text-muted); word-break: break-all; }
.hint { margin: 0; color: var(--color-text-muted); font-size: 12px; }
.row-end { display: flex; justify-content: flex-end; align-items: center; gap: 12px; }
.acl-row { display: flex; align-items: flex-start; gap: 12px; }
.acl-name { min-width: 130px; font-size: 13px; font-weight: 600; padding-top: 3px; }
.acl-badges { display: flex; flex-wrap: wrap; gap: 6px; }
.badge { border: 1px solid var(--color-border); background: none; color: var(--color-text-muted); border-radius: 999px; padding: 2px 10px; font-size: 12px; cursor: pointer; }
.badge.on { border-color: var(--color-primary); color: var(--color-primary); background: var(--color-primary-soft, rgba(76,110,245,0.12)); }
.peer-list { width: 100%; border-collapse: collapse; font-size: 13px; }
.peer-list th { text-align: left; color: var(--color-text-muted); font-weight: 500; padding: 4px 8px; border-bottom: 1px solid var(--color-border); }
.peer-list td { padding: 6px 8px; border-bottom: 1px solid var(--color-border); }
.link-btn { background: none; border: none; color: var(--color-primary); cursor: pointer; font-size: 13px; padding: 0; }
.ok-text { color: var(--color-green); font-size: 13px; }
.err { color: var(--color-amber); }
.muted { color: var(--color-text-muted); font-size: 12px; }

/* ─── 导出配置弹窗(复制文本为主路径) ─────────── */
.yaml-scrim { position: fixed; inset: 0; background: oklch(0% 0 0 / 0.62); display: grid; place-items: center; z-index: 100; padding: 24px; }
.yaml-modal { width: 100%; max-width: 640px; background: var(--color-card); border: 1px solid var(--color-border-strong); border-radius: var(--rounded-xl); box-shadow: var(--shadow-modal); overflow: hidden; }
.yaml-head { display: flex; align-items: flex-start; gap: 12px; padding: 18px 20px 14px; border-bottom: 1px solid var(--color-border); }
.yaml-title { font-size: 1rem; font-weight: 600; color: var(--color-text); letter-spacing: -0.01em; }
.yaml-sub { font-size: 0.78rem; color: var(--color-faint); margin-top: 3px; line-height: 1.5; }
.yaml-close { margin-left: auto; flex-shrink: 0; width: 30px; height: 30px; border-radius: var(--rounded-md); border: none; background: transparent; color: var(--color-faint); cursor: pointer; display: grid; place-items: center; }
.yaml-close:hover { color: var(--color-text); background: var(--color-inset); }
.yaml-body { padding: 16px 20px; }
.yaml-text { width: 100%; min-height: 240px; resize: vertical; font-family: var(--font-mono, monospace); font-size: 0.8rem; line-height: 1.5; padding: 10px 12px; border-radius: var(--rounded-md); border: 1px solid var(--color-border); background: var(--color-inset); color: var(--color-text); }
.yaml-foot { display: flex; justify-content: flex-end; align-items: center; gap: 8px; padding: 0 20px 18px; }
</style>
