<script setup lang="ts">
/*
  CreateWizard.vue — PVE 建机向导(三步:模式与源 → 资源与网络 → 登记与确认)。

  三模式:模板克隆(含 .vma 备份恢复)/ 云镜像 cloud-init / LXC(含 aplinfo 在线下载)。
  批量 1-50 台(静态 IP 自动递增);建机后可选自动登记为平台服务器(凭据二选一)。
  提交后由父组件打开 CreateProgress 轮询任务快照;本组件只负责收集入参。
  样式沿用 SettingsPVE 模态框体系(Teleport + modal-scrim + field);校验逐字段 + 横幅。
*/
import { ref, computed, watch, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { NIcon } from 'naive-ui'
import { Server, Plus, Upload } from '@vicons/tabler'
import AppSelect from '../ui/AppSelect.vue'
import {
  listPVETemplates, listPVEStorages, listPVEBridges, getPVENextID,
  listPVEStorageContent, listPVEAplInfo, uploadPVEBackup, startPVECreate,
  type PVETemplate, type PVEStorage, type PVEBridge, type PVEContentItem,
  type PVEAplTemplate, type PVECreateInput, type PVENodeStatus,
} from '../../api/pveAdmin'
import { listCredentials, type Credential } from '../../api/credentials'
import { HttpError } from '../../api/http'
import { useToast } from '../../composables/useToast'

const props = defineProps<{
  clusterId: string
  clusterName: string
  nodes: PVENodeStatus[]
}>()
const emit = defineEmits<{ close: []; started: [] }>()

const { t } = useI18n()
const toast = useToast()
const errMsg = (err: unknown, fallback: string): string =>
  err instanceof HttpError ? (err.apiError?.message ?? fallback) : fallback

// ─── 步骤与模式 ────────────────────────────────────────────────────────────────
const step = ref(1)
const mode = ref<'clone' | 'image' | 'lxc'>('clone')
const cloneSource = ref<'template' | 'backup'>('template')
const imageSource = ref<'url' | 'existing'>('url')
const lxcSource = ref<'existing' | 'download'>('existing')

// ─── 表单 ──────────────────────────────────────────────────────────────────────
const form = ref({
  node: '',
  name: '',
  count: 1,
  vmid: 0 as number | '',
  templateVmid: '',
  backupVolid: '',
  storage: '',
  imageUrl: '',
  imageVolid: '',
  ostemplate: '',
  aplTemplate: '',
  cores: 2,
  memory: 2048,
  diskGb: 0,
  dataGb: 0,
  dataStorage: '',
  bridge: '',
  ipMode: 'dhcp' as 'dhcp' | 'static',
  ip: '',
  gateway: '',
  prefix: 24,
  dns: '',
  ciUser: '',
  ciPassword: '',
  sshKeys: '',
  sshPasswordAuth: false,
  timezone: '',
  mirror: 'none',
  installAgent: true,
  disableSelinux: false,
  disableFirewall: false,
  fixStatic: false,
  onboot: false,
  start: true,
  agent: true,
  bios: '',
  lxcPassword: '',
  unprivileged: true,
  nesting: true,
  registerEnabled: false,
  sshUser: 'root',
  sshPort: 22,
  credentialMode: 'existing' as 'existing' | 'inline',
  credentialId: '',
  inlineKind: 'password' as 'password' | 'key',
  inlineSecret: '',
  inlineName: '',
  waitSeconds: 180 as number | '',
})
const formErrors = ref<Record<string, string>>({})
const formBanner = ref('')
const submitting = ref(false)

// ─── 候选数据 ──────────────────────────────────────────────────────────────────
const templates = ref<PVETemplate[]>([])
const storages = ref<PVEStorage[]>([])
const bridges = ref<PVEBridge[]>([])
const backups = ref<PVEContentItem[]>([])
const uploadedBackup = ref<{ volid: string; name: string } | null>(null)
const images = ref<PVEContentItem[]>([])
const vztmpls = ref<PVEContentItem[]>([])
const apls = ref<PVEAplTemplate[]>([])
const credentials = ref<Credential[]>([])
const nextid = ref(0)
const loadingCandidates = ref(false)
const uploading = ref(false)
const fileInput = ref<HTMLInputElement | null>(null)

/** 云镜像 URL 预设(官方 cloud 镜像稳定地址;可自定义)。 */
const imagePresets = [
  { value: 'https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img', label: 'Ubuntu 24.04 LTS' },
  { value: 'https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img', label: 'Ubuntu 22.04 LTS' },
  { value: 'https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-generic-amd64.qcow2', label: 'Debian 12' },
  { value: 'https://download.rockylinux.org/pub/rocky/9/images/x86_64/Rocky-9-GenericCloud-Base.latest.x86_64.qcow2', label: 'Rocky Linux 9' },
  { value: 'https://repo.almalinux.org/almalinux/9/cloud/x86_64/images/AlmaLinux-9-GenericCloud-latest.x86_64.qcow2', label: 'AlmaLinux 9' },
]

const nodeOptions = computed(() => props.nodes.map((n) => ({ value: n.name, label: n.name, hint: n.ip })))
const qemuTemplates = computed(() => templates.value.filter((tpl) => tpl.type === 'qemu'))
const templateOptions = computed(() => qemuTemplates.value.map((tpl) => ({
  value: String(tpl.vmid), label: tpl.name || String(tpl.vmid), hint: `VMID ${tpl.vmid} · ${tpl.node}`,
})))
const activeStorages = computed(() => storages.value.filter((s) => s.active && s.enabled))
const storageOptions = computed(() => activeStorages.value.map((s) => ({
  value: s.storage, label: s.storage, hint: `${s.type} · ${s.content.join(',')}`,
})))
const bridgeOptions = computed(() => bridges.value.map((b) => ({ value: b.iface, label: b.iface, hint: b.address || b.comments })))
const volidOptions = (items: PVEContentItem[]) => items.map((i) => ({ value: i.volid, label: i.volid.split('/').pop() ?? i.volid, hint: i.volid }))
const backupOptions = computed(() => {
  const options = volidOptions(backups.value)
  const uploaded = uploadedBackup.value
  // PVE 9.x returns an absolute path, which never occurs in storage-content volids.
  if (uploaded && !options.some((o) => o.value === uploaded.volid)) {
    options.unshift({ value: uploaded.volid, label: uploaded.name, hint: uploaded.volid })
  }
  return options
})
const imageOptions = computed(() => volidOptions(images.value))
const vztmplOptions = computed(() => volidOptions(vztmpls.value))
const aplOptions = computed(() => apls.value.map((a) => ({ value: a.template, label: a.template, hint: a.description })))
const sshCredentials = computed(() => credentials.value.filter((c) => c.type === 'ssh_key' || c.type === 'ssh_password'))
const credentialOptions = computed(() => sshCredentials.value.map((c) => ({ value: c.id, label: c.name, hint: c.maskedValue })))
const presetOptions = computed(() => imagePresets.map((p) => ({ value: p.value, label: p.label, hint: p.value })))

/** 克隆选模板后节点自动取模板所在节点。 */
const effectiveNode = computed(() => {
  if (mode.value === 'clone' && cloneSource.value === 'template' && form.value.templateVmid) {
    return qemuTemplates.value.find((tpl) => String(tpl.vmid) === form.value.templateVmid)?.node ?? form.value.node
  }
  return form.value.node
})

async function loadCandidates(): Promise<void> {
  loadingCandidates.value = true
  try {
    const [tpls, nid, creds] = await Promise.all([
      listPVETemplates(props.clusterId),
      getPVENextID(props.clusterId).catch(() => 0),
      listCredentials().catch(() => [] as Credential[]),
    ])
    templates.value = tpls
    nextid.value = nid
    credentials.value = creds
  } catch (err) {
    formBanner.value = errMsg(err, t('pveCreate.errNetwork'))
  } finally {
    loadingCandidates.value = false
  }
}

let nodeLoadSeq = 0
let storageLoadSeq = 0
let aplLoadSeq = 0

async function loadNodeCandidates(): Promise<void> {
  const seq = ++nodeLoadSeq
  const node = effectiveNode.value
  storages.value = []
  bridges.value = []
  if (!node) return
  try {
    const [sts, brs] = await Promise.all([
      listPVEStorages(props.clusterId, node),
      listPVEBridges(props.clusterId, node),
    ])
    if (seq !== nodeLoadSeq || node !== effectiveNode.value) return
    storages.value = sts
    bridges.value = brs
    if (!form.value.storage && activeStorages.value.length > 0) {
      form.value.storage = activeStorages.value.find((s) => s.storage === 'local')?.storage ?? activeStorages.value[0].storage
    }
    if (!form.value.bridge && brs.length > 0) {
      form.value.bridge = brs.find((b) => b.iface === 'vmbr0')?.iface ?? brs[0].iface
    }
  } catch (err) {
    if (seq === nodeLoadSeq && node === effectiveNode.value) {
      formBanner.value = errMsg(err, t('pveCreate.errNetwork'))
    }
  }
}

async function loadStorageContent(): Promise<void> {
  const seq = ++storageLoadSeq
  const node = effectiveNode.value
  const storage = form.value.storage
  backups.value = []
  images.value = []
  vztmpls.value = []
  if (!node || !storage) return
  try {
    const [bk, imp, img, vzt] = await Promise.all([
      listPVEStorageContent(props.clusterId, node, storage, 'backup').catch(() => [] as PVEContentItem[]),
      listPVEStorageContent(props.clusterId, node, storage, 'import').catch(() => [] as PVEContentItem[]),
      listPVEStorageContent(props.clusterId, node, storage, 'images').catch(() => [] as PVEContentItem[]),
      listPVEStorageContent(props.clusterId, node, storage, 'vztmpl').catch(() => [] as PVEContentItem[]),
    ])
    if (seq !== storageLoadSeq || node !== effectiveNode.value || storage !== form.value.storage) return
    backups.value = bk
    images.value = [...imp, ...img]
    vztmpls.value = vzt
  } catch {
    // 内容列表失败不阻断(可手填 URL/卷)。
  }
}

async function loadAplInfo(): Promise<void> {
  const seq = ++aplLoadSeq
  const node = effectiveNode.value
  apls.value = []
  if (!node) return
  try {
    const items = await listPVEAplInfo(props.clusterId, node)
    if (seq !== aplLoadSeq || node !== effectiveNode.value) return
    apls.value = items
  } catch {
    if (seq === aplLoadSeq && node === effectiveNode.value) apls.value = []
  }
}

function clearStorageSources(): void {
  form.value.backupVolid = ''
  form.value.imageVolid = ''
  form.value.ostemplate = ''
  uploadedBackup.value = null
}

watch(effectiveNode, (n, old) => {
  if (n === old) return
  cancelUpload()
  clearStorageSources()
  form.value.aplTemplate = ''
  form.value.storage = ''
  form.value.bridge = ''
  void loadNodeCandidates()
  void loadAplInfo()
})
watch(() => form.value.storage, () => {
  cancelUpload()
  clearStorageSources()
  void loadStorageContent()
})
watch(mode, () => {
  cancelUpload()
  form.value.templateVmid = ''
  form.value.aplTemplate = ''
  clearStorageSources()
  void loadStorageContent()
})
watch(cloneSource, (source) => {
  if (source !== 'backup') cancelUpload()
})

onMounted(async () => {
  if (props.nodes.length > 0) form.value.node = props.nodes[0].name
  await loadCandidates()
  await loadNodeCandidates()
  await loadStorageContent()
  await loadAplInfo()
})

// ─── .vma 上传 ─────────────────────────────────────────────────────────────────
let uploadSeq = 0
let uploadController: AbortController | null = null

function cancelUpload(): void {
  uploadSeq += 1
  uploadController?.abort()
  uploadController = null
  uploading.value = false
}

onUnmounted(() => { cancelUpload() })

async function onUploadFile(ev: Event): Promise<void> {
  const input = ev.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (!file) return
  const node = effectiveNode.value
  const storage = form.value.storage
  if (!node || !storage) {
    toast.error(t('pveCreate.errUploadTarget'))
    return
  }
  cancelUpload()
  const seq = uploadSeq
  const controller = new AbortController()
  uploadController = controller
  uploading.value = true
  try {
    const volid = await uploadPVEBackup(props.clusterId, node, storage, file, controller.signal)
    if (seq !== uploadSeq || node !== effectiveNode.value || storage !== form.value.storage ||
        mode.value !== 'clone' || cloneSource.value !== 'backup') return
    uploadedBackup.value = { volid, name: file.name }
    form.value.backupVolid = volid
    cloneSource.value = 'backup'
    toast.success(t('pveCreate.uploadDone', { name: file.name }))
    await loadStorageContent()
  } catch (err) {
    if (seq !== uploadSeq || (err instanceof Error && err.name === 'AbortError')) return
    toast.error(t('pveCreate.errUpload'), { detail: errMsg(err, t('pveCreate.errNetwork')) })
  } finally {
    if (seq === uploadSeq) {
      uploading.value = false
      uploadController = null
    }
  }
}

// ─── 校验 ──────────────────────────────────────────────────────────────────────
const nameRe = /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$/
const ipv4Re = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/
function isIPv4(s: string): boolean {
  const m = ipv4Re.exec(s)
  return !!m && m.slice(1).every((p) => Number(p) <= 255)
}

function validateStep(n: number): boolean {
  const e: Record<string, string> = {}
  const f = form.value
  if (n === 1) {
    if (!nameRe.test(f.name.trim())) e.name = t('pveCreate.errName')
    if (!effectiveNode.value) e.node = t('pveCreate.errNode')
    if (f.count < 1 || f.count > 50) e.count = t('pveCreate.errCount')
    if (f.vmid !== '' && f.vmid !== 0 && (f.vmid < 100 || f.vmid > 999999999)) e.vmid = t('pveCreate.errVmid')
    if (mode.value === 'clone') {
      if (cloneSource.value === 'template' && !f.templateVmid) e.source = t('pveCreate.errTemplate')
      if (cloneSource.value === 'backup' && !f.backupVolid) e.source = t('pveCreate.errBackup')
    } else if (mode.value === 'image') {
      if (!f.storage) e.storage = t('pveCreate.errStorage')
      if (imageSource.value === 'url' && !/^https?:\/\/.+/.test(f.imageUrl.trim())) e.source = t('pveCreate.errImageUrl')
      if (imageSource.value === 'existing' && !f.imageVolid) e.source = t('pveCreate.errImageVolid')
    } else {
      if (!f.storage) e.storage = t('pveCreate.errStorage')
      if (lxcSource.value === 'existing' && !f.ostemplate) e.source = t('pveCreate.errOstemplate')
      if (lxcSource.value === 'download' && !f.aplTemplate) e.source = t('pveCreate.errApl')
    }
  }
  if (n === 2) {
    if (f.cores < 1 || f.cores > 256) e.cores = t('pveCreate.errCores')
    if (f.memory < 128 || f.memory > 1048576) e.memory = t('pveCreate.errMemory')
    if (f.diskGb < 0 || f.diskGb > 65536) e.diskGb = t('pveCreate.errDisk')
    if (f.ipMode === 'static') {
      if (!isIPv4(f.ip.trim())) e.ip = t('pveCreate.errIp')
      if (f.prefix < 1 || f.prefix > 32) e.prefix = t('pveCreate.errPrefix')
      if (f.gateway && !isIPv4(f.gateway.trim())) e.gateway = t('pveCreate.errGateway')
    }
    if (f.dns && f.dns.split(/[,;\s]+/).filter(Boolean).some((d) => !isIPv4(d))) e.dns = t('pveCreate.errDns')
  }
  if (n === 3 && f.registerEnabled) {
    if (!nameRe.test(f.sshUser.trim())) e.sshUser = t('pveCreate.errSshUser')
    if (f.sshPort < 1 || f.sshPort > 65535) e.sshPort = t('pveCreate.errSshPort')
    if (f.waitSeconds !== '' && (f.waitSeconds < 1 || f.waitSeconds > 600)) e.waitSeconds = t('pveCreate.errWaitSeconds')
    if (f.credentialMode === 'existing' && !f.credentialId) e.credential = t('pveCreate.errCredential')
    if (f.credentialMode === 'inline' && !f.inlineSecret.trim()) e.credential = t('pveCreate.errInlineSecret')
  }
  formErrors.value = e
  return Object.keys(e).length === 0
}

function nextStep(): void {
  if (uploading.value) return
  if (validateStep(step.value)) step.value += 1
}

const summary = computed(() => {
  const f = form.value
  const src = mode.value === 'clone'
    ? (cloneSource.value === 'template' ? `${t('pveCreate.modeClone')} · VMID ${f.templateVmid}` : `${t('pveCreate.sourceBackup')} · ${f.backupVolid}`)
    : mode.value === 'image'
      ? (imageSource.value === 'url' ? f.imageUrl : f.imageVolid)
      : (lxcSource.value === 'existing' ? f.ostemplate : `${t('pveCreate.sourceDownload')} · ${f.aplTemplate}`)
  return { mode: t(`pveCreate.mode_${mode.value}`), src, node: effectiveNode.value, name: f.name, count: f.count }
})

// ─── 提交 ──────────────────────────────────────────────────────────────────────
async function submit(): Promise<void> {
  for (const n of [1, 2, 3]) {
    if (!validateStep(n)) {
      step.value = n
      return
    }
  }
  const f = form.value
  const input: PVECreateInput = {
    mode: mode.value,
    node: effectiveNode.value,
    name: f.name.trim(),
    count: f.count,
    cores: f.cores,
    memory: f.memory,
    bridge: f.bridge || undefined,
    ipMode: f.ipMode,
    dns: f.dns || undefined,
    timezone: f.timezone || undefined,
    onboot: f.onboot,
    start: f.start,
  }
  if (f.vmid !== '' && f.vmid > 0) input.vmid = f.vmid
  if (f.diskGb > 0) input.diskGb = f.diskGb
  if (f.ipMode === 'static') {
    input.ip = f.ip.trim()
    input.prefix = f.prefix
    if (f.gateway) input.gateway = f.gateway.trim()
  }
  if (mode.value === 'clone') {
    if (cloneSource.value === 'template') input.templateVmid = Number(f.templateVmid)
    else input.backupVolid = f.backupVolid
    if (f.storage) input.storage = f.storage
    input.ciUser = f.ciUser || undefined
    input.ciPassword = f.ciPassword || undefined
    input.sshKeys = f.sshKeys || undefined
    input.sshPasswordAuth = f.sshPasswordAuth
    input.mirror = f.mirror
    input.installAgent = f.installAgent
    input.disableSelinux = f.disableSelinux
    input.disableFirewall = f.disableFirewall
    input.fixStatic = f.fixStatic
    if (f.bios) input.bios = f.bios
    input.agent = f.agent
  } else if (mode.value === 'image') {
    input.storage = f.storage
    if (imageSource.value === 'url') input.imageUrl = f.imageUrl.trim()
    else input.imageVolid = f.imageVolid
    input.ciUser = f.ciUser || undefined
    input.ciPassword = f.ciPassword || undefined
    input.sshKeys = f.sshKeys || undefined
    input.sshPasswordAuth = f.sshPasswordAuth
    input.mirror = f.mirror
    input.installAgent = f.installAgent
    input.disableSelinux = f.disableSelinux
    input.disableFirewall = f.disableFirewall
    input.fixStatic = f.fixStatic
    input.agent = f.agent
    if (f.bios) input.bios = f.bios
  } else {
    input.storage = f.storage
    if (lxcSource.value === 'existing') input.ostemplate = f.ostemplate
    else input.aplDownload = { storage: f.storage, template: f.aplTemplate }
    input.lxcPassword = f.lxcPassword || undefined
    input.sshKeys = f.sshKeys || undefined
    input.unprivileged = f.unprivileged
    input.nesting = f.nesting
    if (f.dataGb > 0) {
      input.dataGb = f.dataGb
      input.dataStorage = f.dataStorage || f.storage || undefined
    }
  }
  if (f.registerEnabled) {
    input.register = {
      sshUser: f.sshUser.trim(),
      sshPort: f.sshPort,
      waitSeconds: f.waitSeconds === '' ? 180 : f.waitSeconds,
      ...(f.credentialMode === 'existing'
        ? { credentialId: f.credentialId }
        : { inlineSecret: { kind: f.inlineKind, secret: f.inlineSecret, name: f.inlineName || undefined } }),
    }
  }
  submitting.value = true
  formBanner.value = ''
  try {
    await startPVECreate(props.clusterId, input)
    emit('started')
  } catch (err) {
    formBanner.value = errMsg(err, t('pveCreate.errNetwork'))
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <Teleport to="body">
    <div
      class="modal-scrim"
      role="dialog"
      :aria-label="t('pveCreate.title')"
      aria-modal="true"
      @click.self="emit('close')"
      @keydown.esc="emit('close')"
    >
      <form class="modal modal--wide" @submit.prevent="step === 3 ? submit() : nextStep()">
        <div class="modal-head">
          <span class="modal-icon"><NIcon :size="17"><Server /></NIcon></span>
          <div>
            <h2 class="modal-title">{{ t('pveCreate.title') }} · {{ clusterName }}</h2>
            <p class="modal-sub">{{ t('pveCreate.sub') }}</p>
          </div>
          <ol class="steps">
            <li v-for="n in 3" :key="n" :class="{ active: step === n, done: step > n }">
              {{ t(`pveCreate.step${n}`) }}
            </li>
          </ol>
        </div>

        <div class="modal-body">
          <p v-if="formBanner" class="banner">{{ formBanner }}</p>

          <!-- 步 1:模式与源 -->
          <template v-if="step === 1">
            <div class="mode-cards">
              <button v-for="m in (['clone', 'image', 'lxc'] as const)" :key="m" type="button"
                class="mode-card" :class="{ 'mode-card--on': mode === m }" @click="mode = m">
                <strong>{{ t(`pveCreate.mode_${m}`) }}</strong>
                <span>{{ t(`pveCreate.modeDesc_${m}`) }}</span>
              </button>
            </div>

            <div class="field-row">
              <label class="field">
                <span class="field-label">{{ t('pveCreate.fieldNode') }}</span>
                <AppSelect v-model="form.node" :options="nodeOptions" :disabled="mode === 'clone' && cloneSource === 'template' && !!form.templateVmid" searchable />
                <span v-if="formErrors.node" class="field-error">{{ formErrors.node }}</span>
                <span v-if="mode === 'clone' && cloneSource === 'template'" class="hint">{{ t('pveCreate.nodeFromTemplate') }}</span>
              </label>
              <label class="field">
                <span class="field-label">{{ t('pveCreate.fieldName') }}</span>
                <input v-model="form.name" class="field-input mono" type="text" placeholder="web-prod" />
                <span v-if="formErrors.name" class="field-error">{{ formErrors.name }}</span>
              </label>
            </div>

            <div class="field-row">
              <label class="field">
                <span class="field-label">{{ t('pveCreate.fieldCount') }}</span>
                <input v-model.number="form.count" class="field-input" type="number" min="1" max="50" />
                <span v-if="formErrors.count" class="field-error">{{ formErrors.count }}</span>
                <span class="hint">{{ t('pveCreate.countHint') }}</span>
              </label>
              <label class="field">
                <span class="field-label">{{ t('pveCreate.fieldVmid') }}</span>
                <input v-model.number="form.vmid" class="field-input" type="number" min="0" :placeholder="nextid ? String(nextid) : 'auto'" />
                <span v-if="formErrors.vmid" class="field-error">{{ formErrors.vmid }}</span>
                <span class="hint">{{ t('pveCreate.vmidHint') }}</span>
              </label>
            </div>

            <!-- 克隆源 -->
            <template v-if="mode === 'clone'">
              <div class="sub-tabs">
                <button type="button" :class="{ on: cloneSource === 'template' }" @click="cloneSource = 'template'">{{ t('pveCreate.sourceTemplate') }}</button>
                <button type="button" :class="{ on: cloneSource === 'backup' }" @click="cloneSource = 'backup'">{{ t('pveCreate.sourceBackup') }}</button>
              </div>
              <label v-if="cloneSource === 'template'" class="field">
                <span class="field-label">{{ t('pveCreate.fieldTemplate') }}</span>
                <AppSelect v-model="form.templateVmid" :options="templateOptions" searchable :placeholder="loadingCandidates ? t('pveCreate.loading') : t('pveCreate.pickTemplate')" />
                <span v-if="formErrors.source" class="field-error">{{ formErrors.source }}</span>
              </label>
              <template v-else>
                <label class="field">
                  <span class="field-label">{{ t('pveCreate.fieldBackup') }}</span>
                  <AppSelect v-model="form.backupVolid" :options="backupOptions" searchable :placeholder="t('pveCreate.pickBackup')" />
                  <span v-if="formErrors.source" class="field-error">{{ formErrors.source }}</span>
                </label>
                <div class="upload-row">
                  <button type="button" class="btn-secondary" :disabled="uploading" @click="fileInput?.click()">
                    <NIcon :size="13"><Upload /></NIcon>
                    {{ uploading ? t('pveCreate.uploading') : t('pveCreate.uploadVma') }}
                  </button>
                  <span class="hint">{{ t('pveCreate.uploadHint') }}</span>
                  <input ref="fileInput" type="file" accept=".vma,.vma.gz,.vma.zst,.vma.lzo,.vma.bz2,.vma.xz" hidden @change="onUploadFile" />
                </div>
              </template>
              <label class="field">
                <span class="field-label">{{ t('pveCreate.fieldTargetStorage') }}</span>
                <AppSelect v-model="form.storage" :options="storageOptions" searchable />
              </label>
            </template>

            <!-- 云镜像源 -->
            <template v-if="mode === 'image'">
              <div class="sub-tabs">
                <button type="button" :class="{ on: imageSource === 'url' }" @click="imageSource = 'url'">{{ t('pveCreate.sourceUrl') }}</button>
                <button type="button" :class="{ on: imageSource === 'existing' }" @click="imageSource = 'existing'">{{ t('pveCreate.sourceExisting') }}</button>
              </div>
              <label class="field">
                <span class="field-label">{{ t('pveCreate.fieldStorage') }}</span>
                <AppSelect v-model="form.storage" :options="storageOptions" searchable />
                <span v-if="formErrors.storage" class="field-error">{{ formErrors.storage }}</span>
              </label>
              <template v-if="imageSource === 'url'">
                <label class="field">
                  <span class="field-label">{{ t('pveCreate.fieldImagePreset') }}</span>
                  <AppSelect v-model="form.imageUrl" :options="presetOptions" searchable :placeholder="t('pveCreate.pickPreset')" />
                </label>
                <label class="field">
                  <span class="field-label">{{ t('pveCreate.fieldImageUrl') }}</span>
                  <input v-model="form.imageUrl" class="field-input mono" type="text" placeholder="https://…/image.qcow2" />
                  <span v-if="formErrors.source" class="field-error">{{ formErrors.source }}</span>
                </label>
              </template>
              <label v-else class="field">
                <span class="field-label">{{ t('pveCreate.fieldImageVolid') }}</span>
                <AppSelect v-model="form.imageVolid" :options="imageOptions" searchable :placeholder="t('pveCreate.pickImage')" />
                <span v-if="formErrors.source" class="field-error">{{ formErrors.source }}</span>
              </label>
            </template>

            <!-- LXC 源 -->
            <template v-if="mode === 'lxc'">
              <div class="sub-tabs">
                <button type="button" :class="{ on: lxcSource === 'existing' }" @click="lxcSource = 'existing'">{{ t('pveCreate.sourceExisting') }}</button>
                <button type="button" :class="{ on: lxcSource === 'download' }" @click="lxcSource = 'download'">{{ t('pveCreate.sourceDownload') }}</button>
              </div>
              <label class="field">
                <span class="field-label">{{ t('pveCreate.fieldStorage') }}</span>
                <AppSelect v-model="form.storage" :options="storageOptions" searchable />
                <span v-if="formErrors.storage" class="field-error">{{ formErrors.storage }}</span>
              </label>
              <label v-if="lxcSource === 'existing'" class="field">
                <span class="field-label">{{ t('pveCreate.fieldOstemplate') }}</span>
                <AppSelect v-model="form.ostemplate" :options="vztmplOptions" searchable :placeholder="t('pveCreate.pickVztmpl')" />
                <span v-if="formErrors.source" class="field-error">{{ formErrors.source }}</span>
              </label>
              <label v-else class="field">
                <span class="field-label">{{ t('pveCreate.fieldApl') }}</span>
                <AppSelect v-model="form.aplTemplate" :options="aplOptions" searchable :placeholder="t('pveCreate.pickApl')" />
                <span v-if="formErrors.source" class="field-error">{{ formErrors.source }}</span>
                <span class="hint">{{ t('pveCreate.aplHint') }}</span>
              </label>
            </template>
          </template>

          <!-- 步 2:资源与网络 -->
          <template v-if="step === 2">
            <div class="field-row field-row--3">
              <label class="field">
                <span class="field-label">{{ t('pveCreate.fieldCores') }}</span>
                <input v-model.number="form.cores" class="field-input" type="number" min="1" max="256" />
                <span v-if="formErrors.cores" class="field-error">{{ formErrors.cores }}</span>
              </label>
              <label class="field">
                <span class="field-label">{{ t('pveCreate.fieldMemory') }}</span>
                <input v-model.number="form.memory" class="field-input" type="number" min="128" step="128" />
                <span v-if="formErrors.memory" class="field-error">{{ formErrors.memory }}</span>
              </label>
              <label class="field">
                <span class="field-label">{{ t('pveCreate.fieldDisk') }}</span>
                <input v-model.number="form.diskGb" class="field-input" type="number" min="0" />
                <span v-if="formErrors.diskGb" class="field-error">{{ formErrors.diskGb }}</span>
                <span class="hint">{{ t('pveCreate.diskHint') }}</span>
              </label>
            </div>

            <div class="field-row">
              <label class="field">
                <span class="field-label">{{ t('pveCreate.fieldBridge') }}</span>
                <AppSelect v-model="form.bridge" :options="bridgeOptions" searchable />
              </label>
              <label class="field">
                <span class="field-label">{{ t('pveCreate.fieldIpMode') }}</span>
                <select v-model="form.ipMode" class="field-input">
                  <option value="dhcp">DHCP</option>
                  <option value="static">{{ t('pveCreate.ipStatic') }}</option>
                </select>
              </label>
            </div>
            <div v-if="form.ipMode === 'static'" class="field-row field-row--3">
              <label class="field">
                <span class="field-label">{{ t('pveCreate.fieldIp') }}</span>
                <input v-model="form.ip" class="field-input mono" type="text" placeholder="10.1.0.60" />
                <span v-if="formErrors.ip" class="field-error">{{ formErrors.ip }}</span>
                <span class="hint">{{ t('pveCreate.ipHint') }}</span>
              </label>
              <label class="field">
                <span class="field-label">{{ t('pveCreate.fieldPrefix') }}</span>
                <input v-model.number="form.prefix" class="field-input" type="number" min="1" max="32" />
                <span v-if="formErrors.prefix" class="field-error">{{ formErrors.prefix }}</span>
              </label>
              <label class="field">
                <span class="field-label">{{ t('pveCreate.fieldGateway') }}</span>
                <input v-model="form.gateway" class="field-input mono" type="text" placeholder="10.1.0.1" />
                <span v-if="formErrors.gateway" class="field-error">{{ formErrors.gateway }}</span>
              </label>
            </div>
            <label class="field">
              <span class="field-label">{{ t('pveCreate.fieldDns') }}</span>
              <input v-model="form.dns" class="field-input mono" type="text" placeholder="223.5.5.5,119.29.29.29" />
              <span v-if="formErrors.dns" class="field-error">{{ formErrors.dns }}</span>
            </label>

            <!-- 凭据与 cloud-init(qemu) -->
            <template v-if="mode !== 'lxc'">
              <div class="field-row">
                <label class="field">
                  <span class="field-label">{{ t('pveCreate.fieldCiUser') }}</span>
                  <input v-model="form.ciUser" class="field-input mono" type="text" placeholder="root" />
                </label>
                <label class="field">
                  <span class="field-label">{{ t('pveCreate.fieldCiPassword') }}</span>
                  <input v-model="form.ciPassword" class="field-input mono" type="password" autocomplete="new-password" />
                </label>
              </div>
              <label class="field">
                <span class="field-label">{{ t('pveCreate.fieldSshKeys') }}</span>
                <textarea v-model="form.sshKeys" class="field-input field-textarea mono" rows="2" placeholder="ssh-ed25519 AAAA… user@host" />
              </label>
              <div class="checks">
                <label class="check"><input v-model="form.sshPasswordAuth" type="checkbox" />{{ t('pveCreate.optSshPwauth') }}</label>
                <label class="check"><input v-model="form.installAgent" type="checkbox" />{{ t('pveCreate.optAgentInstall') }}</label>
                <label class="check"><input v-model="form.agent" type="checkbox" />{{ t('pveCreate.optAgent') }}</label>
                <label class="check"><input v-model="form.onboot" type="checkbox" />{{ t('pveCreate.optOnboot') }}</label>
                <label class="check"><input v-model="form.start" type="checkbox" />{{ t('pveCreate.optStart') }}</label>
                <label class="check"><input v-model="form.disableSelinux" type="checkbox" />{{ t('pveCreate.optSelinux') }}</label>
                <label class="check"><input v-model="form.disableFirewall" type="checkbox" />{{ t('pveCreate.optFirewall') }}</label>
                <label v-if="form.ipMode === 'dhcp'" class="check"><input v-model="form.fixStatic" type="checkbox" />{{ t('pveCreate.optFixStatic') }}</label>
              </div>
              <div class="field-row field-row--3">
                <label class="field">
                  <span class="field-label">{{ t('pveCreate.fieldTimezone') }}</span>
                  <input v-model="form.timezone" class="field-input mono" type="text" placeholder="Asia/Shanghai" />
                </label>
                <label class="field">
                  <span class="field-label">{{ t('pveCreate.fieldMirror') }}</span>
                  <select v-model="form.mirror" class="field-input">
                    <option value="none">{{ t('pveCreate.mirrorNone') }}</option>
                    <option value="ustc">USTC</option>
                    <option value="tsinghua">{{ t('pveCreate.mirrorTsinghua') }}</option>
                    <option value="aliyun">{{ t('pveCreate.mirrorAliyun') }}</option>
                  </select>
                </label>
                <label class="field">
                  <span class="field-label">{{ t('pveCreate.fieldBios') }}</span>
                  <select v-model="form.bios" class="field-input">
                    <option value="">{{ t('pveCreate.biosDefault') }}</option>
                    <option value="seabios">SeaBIOS</option>
                    <option value="ovmf">OVMF (UEFI)</option>
                  </select>
                </label>
              </div>
              <p v-if="mode === 'clone'" class="hint">{{ t('pveCreate.cloneCiHint') }}</p>
            </template>

            <!-- LXC 专属 -->
            <template v-else>
              <div class="field-row">
                <label class="field">
                  <span class="field-label">{{ t('pveCreate.fieldLxcPassword') }}</span>
                  <input v-model="form.lxcPassword" class="field-input mono" type="password" autocomplete="new-password" />
                </label>
                <label class="field">
                  <span class="field-label">{{ t('pveCreate.fieldTimezone') }}</span>
                  <input v-model="form.timezone" class="field-input mono" type="text" placeholder="Asia/Shanghai" />
                </label>
              </div>
              <label class="field">
                <span class="field-label">{{ t('pveCreate.fieldSshKeys') }}</span>
                <textarea v-model="form.sshKeys" class="field-input field-textarea mono" rows="2" placeholder="ssh-ed25519 AAAA… user@host" />
              </label>
              <div class="field-row">
                <label class="field">
                  <span class="field-label">{{ t('pveCreate.fieldDataGb') }}</span>
                  <input v-model.number="form.dataGb" class="field-input" type="number" min="0" />
                  <span class="hint">{{ t('pveCreate.dataHint') }}</span>
                </label>
                <label class="field">
                  <span class="field-label">{{ t('pveCreate.fieldDataStorage') }}</span>
                  <AppSelect v-model="form.dataStorage" :options="storageOptions" searchable :placeholder="form.storage || 'local'" />
                </label>
              </div>
              <div class="checks">
                <label class="check"><input v-model="form.unprivileged" type="checkbox" />{{ t('pveCreate.optUnprivileged') }}</label>
                <label class="check"><input v-model="form.nesting" type="checkbox" />{{ t('pveCreate.optNesting') }}</label>
                <label class="check"><input v-model="form.onboot" type="checkbox" />{{ t('pveCreate.optOnboot') }}</label>
                <label class="check"><input v-model="form.start" type="checkbox" />{{ t('pveCreate.optStart') }}</label>
              </div>
            </template>
          </template>

          <!-- 步 3:登记与确认 -->
          <template v-if="step === 3">
            <label class="check check--big"><input v-model="form.registerEnabled" type="checkbox" />{{ t('pveCreate.registerToggle') }}</label>
            <p class="hint">{{ t('pveCreate.registerHint') }}</p>
            <template v-if="form.registerEnabled">
              <div class="field-row">
                <label class="field">
                  <span class="field-label">{{ t('pveCreate.fieldSshUser') }}</span>
                  <input v-model="form.sshUser" class="field-input mono" type="text" />
                  <span v-if="formErrors.sshUser" class="field-error">{{ formErrors.sshUser }}</span>
                </label>
                <label class="field">
                  <span class="field-label">{{ t('pveCreate.fieldSshPort') }}</span>
                  <input v-model.number="form.sshPort" class="field-input" type="number" min="1" max="65535" />
                  <span v-if="formErrors.sshPort" class="field-error">{{ formErrors.sshPort }}</span>
                </label>
              </div>
              <div class="sub-tabs">
                <button type="button" :class="{ on: form.credentialMode === 'existing' }" @click="form.credentialMode = 'existing'">{{ t('pveCreate.credExisting') }}</button>
                <button type="button" :class="{ on: form.credentialMode === 'inline' }" @click="form.credentialMode = 'inline'">{{ t('pveCreate.credInline') }}</button>
              </div>
              <label v-if="form.credentialMode === 'existing'" class="field">
                <span class="field-label">{{ t('pveCreate.fieldCredential') }}</span>
                <AppSelect v-model="form.credentialId" :options="credentialOptions" searchable />
                <span v-if="formErrors.credential" class="field-error">{{ formErrors.credential }}</span>
              </label>
              <template v-else>
                <div class="field-row">
                  <label class="field">
                    <span class="field-label">{{ t('pveCreate.fieldInlineKind') }}</span>
                    <select v-model="form.inlineKind" class="field-input">
                      <option value="password">{{ t('pveCreate.inlinePassword') }}</option>
                      <option value="key">{{ t('pveCreate.inlineKey') }}</option>
                    </select>
                  </label>
                  <label class="field">
                    <span class="field-label">{{ t('pveCreate.fieldInlineName') }}</span>
                    <input v-model="form.inlineName" class="field-input" type="text" :placeholder="`pve-${form.name || 'vm'}`" />
                  </label>
                </div>
                <label class="field">
                  <span class="field-label">{{ t('pveCreate.fieldInlineSecret') }}</span>
                  <textarea v-if="form.inlineKind === 'key'" v-model="form.inlineSecret" class="field-input field-textarea mono" rows="3" placeholder="-----BEGIN OPENSSH PRIVATE KEY-----" />
                  <input v-else v-model="form.inlineSecret" class="field-input mono" type="password" autocomplete="new-password" />
                  <span v-if="formErrors.credential" class="field-error">{{ formErrors.credential }}</span>
                </label>
              </template>
              <label class="field">
                <span class="field-label">{{ t('pveCreate.fieldWaitSeconds') }}</span>
                <input v-model.number="form.waitSeconds" class="field-input" type="number" min="1" max="600" />
                <span v-if="formErrors.waitSeconds" class="field-error">{{ formErrors.waitSeconds }}</span>
                <span class="hint">{{ t('pveCreate.waitHint') }}</span>
              </label>
            </template>

            <div class="summary">
              <div class="summary-row"><span>{{ t('pveCreate.sumMode') }}</span><strong>{{ summary.mode }}</strong></div>
              <div class="summary-row"><span>{{ t('pveCreate.sumSource') }}</span><strong class="mono">{{ summary.src }}</strong></div>
              <div class="summary-row"><span>{{ t('pveCreate.sumNode') }}</span><strong>{{ summary.node }}</strong></div>
              <div class="summary-row"><span>{{ t('pveCreate.sumName') }}</span><strong>{{ summary.name }} × {{ summary.count }}</strong></div>
            </div>
          </template>
        </div>

        <div class="modal-foot">
          <button type="button" class="btn-secondary" :disabled="submitting" @click="emit('close')">
            {{ t('pveCreate.cancel') }}
          </button>
          <button v-if="step > 1" type="button" class="btn-secondary" :disabled="submitting" @click="step -= 1">
            {{ t('pveCreate.back') }}
          </button>
          <button type="submit" class="btn-primary" :disabled="submitting || uploading" :aria-busy="submitting || uploading">
            <NIcon v-if="step === 3" :size="13"><Plus /></NIcon>
            {{ step === 3 ? (submitting ? t('pveCreate.submitting') : t('pveCreate.submit')) : t('pveCreate.next') }}
          </button>
        </div>
      </form>
    </div>
  </Teleport>
</template>

<style scoped>
.modal-scrim {
  position: fixed;
  inset: 0;
  background: oklch(0% 0 0 / 0.62);
  display: grid;
  place-items: center;
  z-index: 100;
  padding: 24px;
}
.modal {
  width: 100%;
  max-width: 540px;
  max-height: min(90vh, calc(100dvh - 48px));
  display: flex;
  flex-direction: column;
  background: var(--color-card);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded-xl);
  box-shadow: var(--shadow-modal);
  overflow: hidden;
}
.modal--wide { max-width: 680px; }
.modal-head {
  display: flex;
  align-items: flex-start;
  gap: 12px;
  padding: 20px 20px 16px;
  border-bottom: 1px solid var(--color-border);
}
.modal-icon {
  width: 36px;
  height: 36px;
  border-radius: var(--rounded-lg);
  background: var(--color-primary-soft);
  color: var(--color-primary);
  display: grid;
  place-items: center;
  flex-shrink: 0;
}
.modal-title { font-size: 1rem; font-weight: 600; color: var(--color-text); margin-top: 2px; }
.modal-sub { font-size: 0.78rem; color: var(--color-faint); margin-top: 3px; }
.steps { margin-left: auto; display: flex; gap: 6px; list-style: none; padding: 0; }
.steps li {
  font-size: 0.7rem;
  padding: 4px 9px;
  border-radius: var(--rounded-full);
  border: 1px solid var(--color-border);
  color: var(--color-faint);
  white-space: nowrap;
}
.steps li.active { background: var(--color-primary-soft); color: var(--color-primary); border-color: var(--color-primary); font-weight: 600; }
.steps li.done { color: var(--color-green); border-color: var(--color-green); }
.modal-body { padding: 18px 20px; display: flex; flex-direction: column; gap: 14px; min-height: 0; overflow-y: auto; }
.modal-foot {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
  padding: 14px 20px 18px;
  border-top: 1px solid var(--color-border);
}
.banner {
  padding: 9px 12px;
  border-radius: var(--rounded);
  background: var(--color-red-soft, oklch(95% 0.03 25));
  color: var(--color-red);
  font-size: 0.78rem;
  line-height: 1.5;
}
.field { display: flex; flex-direction: column; gap: 6px; min-width: 0; }
.field-row { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
.field-row--3 { grid-template-columns: 1fr 1fr 1fr; }
.field-label { font-size: 0.76rem; font-weight: 500; color: var(--color-dim); }
.field-input {
  width: 100%;
  height: 36px;
  box-sizing: border-box;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  padding: 0 10px;
  color: var(--color-text);
  font-family: var(--font-sans);
  font-size: 0.84rem;
}
.field-input.mono { font-family: var(--font-mono); font-size: 0.8rem; }
.field-input:focus {
  outline: none;
  border-color: var(--color-primary);
  box-shadow: 0 0 0 3px var(--color-primary-soft);
}
.field-textarea { height: auto; padding: 8px 10px; resize: vertical; }
.field-error { font-size: 0.72rem; color: var(--color-red); }
.hint { font-size: 0.74rem; color: var(--color-faint); line-height: 1.5; }
.mode-cards { display: grid; grid-template-columns: 1fr 1fr 1fr; gap: 10px; }
.mode-card {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 10px 12px;
  text-align: left;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  cursor: pointer;
  color: var(--color-dim);
}
.mode-card strong { font-size: 0.82rem; color: var(--color-text); }
.mode-card span { font-size: 0.7rem; line-height: 1.4; }
.mode-card--on { border-color: var(--color-primary); background: var(--color-primary-soft); }
.mode-card--on strong { color: var(--color-primary); }
.sub-tabs { display: flex; gap: 6px; }
.sub-tabs button {
  padding: 5px 12px;
  font-size: 0.76rem;
  border-radius: var(--rounded-full);
  border: 1px solid var(--color-border);
  background: var(--color-card);
  color: var(--color-dim);
  cursor: pointer;
}
.sub-tabs button.on { background: var(--color-primary-soft); color: var(--color-primary); border-color: var(--color-primary); font-weight: 600; }
.checks { display: flex; flex-wrap: wrap; gap: 8px 16px; }
.check { display: inline-flex; align-items: center; gap: 6px; font-size: 0.78rem; color: var(--color-dim); cursor: pointer; }
.check--big { font-size: 0.86rem; font-weight: 600; color: var(--color-text); }
.upload-row { display: flex; align-items: center; gap: 10px; }
.summary {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 12px 14px;
  background: var(--color-inset);
  border-radius: var(--rounded);
}
.summary-row { display: flex; justify-content: space-between; gap: 12px; font-size: 0.78rem; }
.summary-row span { color: var(--color-faint); }
.summary-row strong { color: var(--color-text); font-weight: 600; overflow-wrap: anywhere; text-align: right; }
.btn-primary {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 36px;
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
.mono { font-family: var(--font-mono); }
@media (max-width: 720px) {
  .mode-cards, .field-row--3 { grid-template-columns: 1fr; }
  .steps { display: none; }
}
</style>