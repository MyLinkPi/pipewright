<script setup lang="ts">
/*
  Disks.vue — 服务器磁盘管理(块设备/LVM 拓扑 + 分区/格式化/挂载/扩缩容;顶部导航「磁盘」)。

  服务器在 设置 → 服务器 登记;此处对单台服务器:
    - 拓扑:lsblk 设备树(整盘/分区/LVM 层级缩进,含 fstype/挂载点/用量/系统盘标记)
      + LVM 视图(VG → LV,PV 归属与剩余空间),单次 SSH 合并采集;
    - 写操作:分区(空盘初始化)/格式化/挂载(fstab 持久化)/卸载/扩容/缩容/LVM 变更
      ——默认只读,解锁是显式确认动作(30 分钟 TTL;服务端强校验 + 审计);
    - 危险操作(格式化/删除/缩容/移除)须逐字输入目标路径确认(服务端强校验);
    - 文件系统能力矩阵:ext4 在线扩/离线缩(已挂载须先自行卸载);XFS 在线扩/不可缩;
      Btrfs 在线扩/在线缩。工具缺失时对应按钮禁用(topology.tools 探测)。
*/
import { ref, computed, watch, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { NIcon } from 'naive-ui'
import {
  Refresh, Lock, LockOpen, DeviceFloppy, Database, CirclePlus, Trash,
  ArrowUp, ArrowDown, PlugConnected, AlertTriangle,
} from '@vicons/tabler'
import { listServers, type Server } from '../api/servers'
import {
  getDiskTopology, unlockDiskServer, lockDiskServer,
  createDiskPartition, deleteDiskPartition, growDiskPartition, shrinkDiskTarget,
  formatDiskTarget, mountDiskTarget, umountDiskTarget,
  diskPVAction, diskVGAction, diskLVAction,
  type DiskTopology, type BlockDevice, type VolumeGroup, type LogicalVolume, type PhysicalVolume,
  type DiskOpResult,
} from '../api/diskAdmin'
import { HttpError } from '../api/http'
import { useToast } from '../composables/useToast'
import { useConfirm } from '../composables/useConfirm'

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()

const errMsg = (err: unknown, fallback: string): string =>
  err instanceof HttpError ? (err.apiError?.message ?? fallback) : fallback

// ─── 服务器与拓扑 ────────────────────────────────────────────────────────────
const servers = ref<Server[]>([])
const currentId = ref('')
const current = computed<Server | null>(() =>
  servers.value.find((s) => s.id === currentId.value) ?? null)
const topo = ref<DiskTopology | null>(null)
const loading = ref(false)
const loadErr = ref('')

const canWrite = computed(() => !!topo.value?.unlocked && !!topo.value?.privileged)
const tools = computed(() => topo.value?.tools)

async function loadServers(): Promise<void> {
  try {
    servers.value = await listServers()
    if (!servers.value.some((s) => s.id === currentId.value)) {
      currentId.value = servers.value[0]?.id ?? ''
    }
  } catch (err) {
    toast.error(t('diskAdmin.errLoadServers'), { detail: errMsg(err, t('diskAdmin.errNetwork')) })
  }
}

async function refresh(): Promise<void> {
  if (!currentId.value) {
    topo.value = null
    return
  }
  loading.value = true
  loadErr.value = ''
  try {
    topo.value = await getDiskTopology(currentId.value)
  } catch (err) {
    topo.value = null
    loadErr.value = errMsg(err, t('diskAdmin.errTopology'))
  } finally {
    loading.value = false
  }
}

/** 应用写操作响应(带回拓扑则直接用,否则重拉)。 */
function applyResult(res: DiskOpResult | void): void {
  if (res && res.topology) topo.value = res.topology
  else void refresh()
}

onMounted(async () => {
  await loadServers()
  await refresh()
})

// ─── 解锁 / 上锁 ────────────────────────────────────────────────────────────
const lockBusy = ref(false)

async function askUnlock(): Promise<void> {
  if (!current.value) return
  const ok = await confirm.open({
    title: t('diskAdmin.unlockTitle'),
    body: t('diskAdmin.unlockBody'),
    confirmLabel: t('diskAdmin.unlock'),
    variant: 'danger',
  })
  if (!ok) return
  lockBusy.value = true
  try {
    await unlockDiskServer(current.value.id)
    toast.success(t('diskAdmin.unlockOk'))
    await refresh()
  } catch (err) {
    toast.error(t('diskAdmin.opFailed'), { detail: errMsg(err, t('diskAdmin.errNetwork')) })
  } finally {
    lockBusy.value = false
  }
}

async function relock(): Promise<void> {
  if (!current.value) return
  lockBusy.value = true
  try {
    await lockDiskServer(current.value.id)
    toast.success(t('diskAdmin.relockOk'))
    await refresh()
  } catch (err) {
    toast.error(t('diskAdmin.opFailed'), { detail: errMsg(err, t('diskAdmin.errNetwork')) })
  } finally {
    lockBusy.value = false
  }
}

// ─── 展示辅助 ───────────────────────────────────────────────────────────────
function fmtBytes(n: number): string {
  if (!n || n <= 0) return '—'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB']
  let v = n
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v >= 100 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`
}

const typeLabel = (ty: string): string => {
  const map: Record<string, string> = {
    disk: t('diskAdmin.typeDisk'), part: t('diskAdmin.typePart'), lvm: t('diskAdmin.typeLvm'),
    crypt: t('diskAdmin.typeCrypt'), rom: t('diskAdmin.typeRom'), loop: t('diskAdmin.typeLoop'),
  }
  return map[ty] ?? (ty || '—')
}

interface DeviceRow {
  dev: BlockDevice
  depth: number
  isLastPart: boolean
}

const rows = computed<DeviceRow[]>(() => {
  const out: DeviceRow[] = []
  const walk = (devs: BlockDevice[] | null | undefined, depth: number, disk: BlockDevice | null): void => {
    for (const d of devs ?? []) {
      const sibs = disk?.children ?? []
      out.push({
        dev: d,
        depth,
        isLastPart: d.type === 'part' && sibs.length > 0 && sibs[sibs.length - 1].path === d.path,
      })
      walk(d.children, depth + 1, d.type === 'disk' ? d : disk)
    }
  }
  walk(topo.value?.devices, 0, null)
  return out
})

/** 空整盘:可初始化(无分区、无 fs、非 PV、非系统盘)。 */
function isBlankDisk(d: BlockDevice): boolean {
  return d.type === 'disk' && !d.protected && (d.children ?? []).length === 0
    && !d.fstype && !isPV(d.path)
}

const freePVs = computed<PhysicalVolume[]>(() => (topo.value?.pvs ?? []).filter((p) => !p.vgName))
const isPV = (path: string): boolean => (topo.value?.pvs ?? []).some((p) => p.path === path)

const lvMounted = (lv: LogicalVolume): boolean => (lv.mountpoints ?? []).length > 0
const devMounted = (d: BlockDevice): boolean => (d.mountpoints ?? []).length > 0

const shrinkSupported = (fstype: string): boolean => fstype === 'ext4' || fstype === 'btrfs'
const growSupported = (fstype: string): boolean =>
  fstype === 'ext4' || fstype === 'ext3' || fstype === 'ext2' || fstype === 'xfs' || fstype === 'btrfs'

/** 空盘初始化后的首个分区路径预测(sdX→sdX1;nvme0n1→nvme0n1p1)。 */
function predictPartitionPath(disk: BlockDevice): string {
  const suffix = /\d$/.test(disk.name) ? `${disk.name}p1` : `${disk.name}1`
  const dir = disk.path.slice(0, disk.path.length - disk.name.length)
  return `${dir}${suffix}`
}

// ─── 通用操作弹窗 ───────────────────────────────────────────────────────────
type FieldVal = string | number | boolean
interface OpField {
  key: string
  label: string
  type: 'text' | 'number' | 'select' | 'check'
  value: FieldVal
  placeholder?: string
  hint?: string
  options?: { value: string; label: string }[]
  showIf?: (vals: Record<string, FieldVal>) => boolean
}
interface OpDialog {
  title: string
  body?: string
  confirmText?: string
  submitLabel: string
  busy: boolean
  fields: OpField[]
  run: (vals: Record<string, FieldVal>) => Promise<void>
}
const dlg = ref<OpDialog | null>(null)
const confirmInput = ref('')

function openOp(d: Omit<OpDialog, 'busy'>): void {
  dlg.value = { ...d, busy: false }
  confirmInput.value = ''
}

function closeDlg(): void {
  if (dlg.value?.busy) return
  dlg.value = null
}

const visibleFields = computed<OpField[]>(() => {
  if (!dlg.value) return []
  const vals = fieldVals(dlg.value.fields)
  return dlg.value.fields.filter((f) => !f.showIf || f.showIf(vals))
})

function fieldVals(fields: OpField[]): Record<string, FieldVal> {
  const out: Record<string, FieldVal> = {}
  for (const f of fields) out[f.key] = f.value
  return out
}

const canSubmitDlg = computed<boolean>(() => {
  if (!dlg.value) return false
  if (dlg.value.busy) return false
  if (dlg.value.confirmText && confirmInput.value !== dlg.value.confirmText) return false
  return true
})

async function submitDlg(): Promise<void> {
  const d = dlg.value
  if (!d || d.busy || !canSubmitDlg.value) return
  d.busy = true
  try {
    await d.run(fieldVals(d.fields))
    dlg.value = null
  } catch (err) {
    toast.error(t('diskAdmin.opFailed'), { detail: errMsg(err, t('diskAdmin.errNetwork')) })
    await refresh()
    if (dlg.value) dlg.value.busy = false
  }
}

/** 简单确认类操作(服务端无逐字确认要求的)。 */
async function simpleOp(title: string, body: string, label: string, run: () => Promise<DiskOpResult>): Promise<void> {
  const ok = await confirm.open({ title, body, confirmLabel: label, variant: 'danger' })
  if (!ok) return
  try {
    applyResult(await run())
    toast.success(t('diskAdmin.opDone'))
  } catch (err) {
    toast.error(t('diskAdmin.opFailed'), { detail: errMsg(err, t('diskAdmin.errNetwork')) })
    await refresh()
  }
}

const sid = (): string => currentId.value

// ─── 空盘初始化 / 分区 ──────────────────────────────────────────────────────
function openInit(disk: BlockDevice): void {
  const part = predictPartitionPath(disk)
  const needsLabel = !disk.pttype
  openOp({
    title: t('diskAdmin.initTitle', { dev: disk.path }),
    body: needsLabel
      ? `${t('diskAdmin.initBody')}\n${t('diskAdmin.initMakeLabel')}`
      : t('diskAdmin.initBody'),
    confirmText: part,
    submitLabel: t('diskAdmin.actInit'),
    fields: [
      {
        key: 'sizeSpec', label: t('diskAdmin.initSize'), type: 'select', value: '100%',
        options: [
          { value: '100%', label: t('diskAdmin.initSizeAll') },
          { value: '10GiB', label: '10 GiB' }, { value: '20GiB', label: '20 GiB' },
          { value: '50GiB', label: '50 GiB' }, { value: '100GiB', label: '100 GiB' },
          { value: '200GiB', label: '200 GiB' }, { value: '500GiB', label: '500 GiB' },
          { value: '1000GiB', label: '1000 GiB' },
        ],
      },
      {
        key: 'fstype', label: t('diskAdmin.fstype'), type: 'select', value: 'ext4',
        options: [
          { value: 'ext4', label: 'ext4' }, { value: 'xfs', label: 'xfs' }, { value: 'btrfs', label: 'btrfs' },
        ],
      },
      { key: 'label', label: t('diskAdmin.fsLabel'), type: 'text', value: '', placeholder: 'data' },
      { key: 'mountpoint', label: t('diskAdmin.mountPoint'), type: 'text', value: '', placeholder: '/mnt/data' },
    ],
    run: async (v) => {
      toast.info(t('diskAdmin.initStepPart'))
      await createDiskPartition(sid(), {
        device: disk.path, sizeSpec: String(v.sizeSpec), makeLabel: needsLabel,
      })
      toast.info(t('diskAdmin.initStepFormat'))
      await formatDiskTarget(sid(), {
        target: part, fstype: String(v.fstype) as 'ext4' | 'xfs' | 'btrfs',
        label: String(v.label ?? ''), confirm: part,
      })
      const mnt = String(v.mountpoint ?? '').trim()
      if (mnt) {
        toast.info(t('diskAdmin.initStepMount'))
        await mountDiskTarget(sid(), { target: part, mountpoint: mnt })
      }
      toast.success(t('diskAdmin.initDone', { dev: part }))
      await refresh()
    },
  })
}

function askDeletePartition(part: BlockDevice): void {
  void simpleOp(
    t('diskAdmin.delTitle', { dev: part.path }),
    t('diskAdmin.delBody'), t('diskAdmin.actDelete'),
    () => deleteDiskPartition(sid(), { partition: part.path, confirm: part.path }),
  )
}

// ─── 格式化 / 挂载 / 卸载 / 扩缩容 ──────────────────────────────────────────
function openFormat(target: string, suggested?: string): void {
  openOp({
    title: t('diskAdmin.fmtTitle', { dev: target }),
    body: t('diskAdmin.fmtBody'),
    confirmText: target,
    submitLabel: t('diskAdmin.actFormat'),
    fields: [
      {
        key: 'fstype', label: t('diskAdmin.fstype'), type: 'select', value: suggested ?? 'ext4',
        options: [
          { value: 'ext4', label: 'ext4' }, { value: 'xfs', label: 'xfs' }, { value: 'btrfs', label: 'btrfs' },
        ],
      },
      { key: 'label', label: t('diskAdmin.fsLabel'), type: 'text', value: '', placeholder: 'data' },
    ],
    run: async (v) => {
      await formatDiskTarget(sid(), {
        target, fstype: String(v.fstype) as 'ext4' | 'xfs' | 'btrfs',
        label: String(v.label ?? ''), confirm: target,
      })
      toast.success(t('diskAdmin.opDone'))
      await refresh()
    },
  })
}

function openMount(target: string): void {
  openOp({
    title: t('diskAdmin.mountTitle', { dev: target }),
    body: t('diskAdmin.mountHint'),
    submitLabel: t('diskAdmin.actMount'),
    fields: [
      { key: 'mountpoint', label: t('diskAdmin.mountPoint'), type: 'text', value: '', placeholder: '/mnt/data' },
      { key: 'options', label: t('diskAdmin.mountOptions'), type: 'text', value: 'defaults', hint: 'defaults / noatime' },
    ],
    run: async (v) => {
      await mountDiskTarget(sid(), {
        target, mountpoint: String(v.mountpoint).trim(),
        options: String(v.options ?? 'defaults') || 'defaults',
      })
      toast.success(t('diskAdmin.opDone'))
      await refresh()
    },
  })
}

function askUmount(mountpoint: string): void {
  void simpleOp(
    t('diskAdmin.umountTitle', { mnt: mountpoint }),
    t('diskAdmin.umountBody'), t('diskAdmin.actUmount'),
    () => umountDiskTarget(sid(), { mountpoint, removeDir: false }),
  )
}

function askGrow(target: string, fstype: string): void {
  void simpleOp(
    t('diskAdmin.growTitle', { dev: target }),
    fstype === 'xfs' || fstype === 'btrfs'
      ? t('diskAdmin.growBodyOnline', { fs: fstype })
      : t('diskAdmin.growBody'),
    t('diskAdmin.actGrow'),
    () => growDiskPartition(sid(), target),
  )
}

function openShrink(target: string, currentGiB: number, fstype: string): void {
  openOp({
    title: t('diskAdmin.shrinkTitle', { dev: target }),
    body: fstype === 'ext4'
      ? t('diskAdmin.ext4OfflineShrink')
      : t('diskAdmin.btrfsOnlineShrink'),
    confirmText: target,
    submitLabel: t('diskAdmin.actShrink'),
    fields: [
      {
        key: 'targetGiB', label: t('diskAdmin.shrinkTo'), type: 'number', value: Math.max(1, currentGiB - 1),
        hint: t('diskAdmin.shrinkCurrent', { n: currentGiB }),
      },
    ],
    run: async (v) => {
      await shrinkDiskTarget(sid(), { target, targetGiB: Number(v.targetGiB), confirm: target })
      toast.success(t('diskAdmin.opDone'))
      await refresh()
    },
  })
}

// ─── LVM ────────────────────────────────────────────────────────────────────
function askPVCreate(device: string): void {
  void simpleOp(
    t('diskAdmin.pvCreateTitle', { dev: device }),
    t('diskAdmin.pvCreateBody'), t('diskAdmin.actPvCreate'),
    () => diskPVAction(sid(), { action: 'create', device }),
  )
}

function askPVRemove(device: string): void {
  void simpleOp(
    t('diskAdmin.pvRemoveTitle', { dev: device }),
    t('diskAdmin.pvRemoveBody'), t('diskAdmin.actPvRemove'),
    () => diskPVAction(sid(), { action: 'remove', device, confirm: device }),
  )
}

function openVGCreate(): void {
  const opts = freePVs.value.map((p) => ({ value: p.path, label: `${p.path}(${fmtBytes(p.sizeBytes)})` }))
  if (opts.length === 0) {
    toast.error(t('diskAdmin.noFreePvs'))
    return
  }
  openOp({
    title: t('diskAdmin.vgCreateTitle'),
    body: t('diskAdmin.vgCreateBody'),
    submitLabel: t('diskAdmin.actVgCreate'),
    fields: [
      { key: 'name', label: t('diskAdmin.vgName'), type: 'text', value: 'datavg', placeholder: 'datavg' },
      ...opts.map((o): OpField => ({ key: `pv:${o.value}`, label: o.label, type: 'check', value: false })),
    ],
    run: async (v) => {
      const devices = Object.entries(v).filter(([k, val]) => k.startsWith('pv:') && val === true).map(([k]) => k.slice(3))
      await diskVGAction(sid(), { action: 'create', name: String(v.name).trim(), devices })
      toast.success(t('diskAdmin.opDone'))
      await refresh()
    },
  })
}

function openVGExtend(vg: VolumeGroup): void {
  const opts = freePVs.value.map((p) => ({ value: p.path, label: `${p.path}(${fmtBytes(p.sizeBytes)})` }))
  if (opts.length === 0) {
    toast.error(t('diskAdmin.noFreePvs'))
    return
  }
  openOp({
    title: t('diskAdmin.vgExtendTitle', { vg: vg.name }),
    submitLabel: t('diskAdmin.actVgExtend'),
    fields: [
      { key: 'device', label: t('diskAdmin.vgDevice'), type: 'select', value: opts[0].value, options: opts },
    ],
    run: async (v) => {
      await diskVGAction(sid(), { action: 'extend', name: vg.name, device: String(v.device) })
      toast.success(t('diskAdmin.opDone'))
      await refresh()
    },
  })
}

function askVGReduce(vg: string, device: string): void {
  void simpleOp(
    t('diskAdmin.vgReduceTitle', { vg, dev: device }),
    t('diskAdmin.vgReduceBody'), t('diskAdmin.actVgReduce'),
    () => diskVGAction(sid(), { action: 'reduce', name: vg, device, confirm: device }),
  )
}

function askVGRemove(vg: VolumeGroup): void {
  const okConfirm = async (): Promise<void> => {
    const ok = await confirm.open({
      title: t('diskAdmin.vgRemoveTitle', { vg: vg.name }),
      body: t('diskAdmin.vgRemoveBody'),
      confirmText: vg.name,
      confirmLabel: t('diskAdmin.actVgRemove'),
      variant: 'danger',
    })
    if (!ok) return
    try {
      applyResult(await diskVGAction(sid(), { action: 'remove', name: vg.name, confirm: vg.name }))
      toast.success(t('diskAdmin.opDone'))
    } catch (err) {
      toast.error(t('diskAdmin.opFailed'), { detail: errMsg(err, t('diskAdmin.errNetwork')) })
      await refresh()
    }
  }
  void okConfirm()
}

function lvsOf(vg: string): LogicalVolume[] {
  return (topo.value?.lvs ?? []).filter((lv) => lv.vgName === vg)
}

function pvsOf(vg: string): PhysicalVolume[] {
  return (topo.value?.pvs ?? []).filter((p) => p.vgName === vg)
}

function openLVCreate(vg: VolumeGroup): void {
  openOp({
    title: t('diskAdmin.lvCreateTitle', { vg: vg.name }),
    submitLabel: t('diskAdmin.actLvCreate'),
    fields: [
      { key: 'name', label: t('diskAdmin.lvName'), type: 'text', value: 'data', placeholder: 'data' },
      {
        key: 'mode', label: t('diskAdmin.lvSizeMode'), type: 'select', value: 'all',
        options: [
          { value: 'all', label: t('diskAdmin.lvAll', { free: fmtBytes(vg.freeBytes) }) },
          { value: 'add', label: t('diskAdmin.lvAdd') },
        ],
      },
      { key: 'sizeGiB', label: 'GiB', type: 'number', value: 10, showIf: (v) => v.mode === 'add' },
      {
        key: 'fstype', label: t('diskAdmin.lvFstype'), type: 'select', value: 'ext4',
        options: [
          { value: 'none', label: '—' }, { value: 'ext4', label: 'ext4' },
          { value: 'xfs', label: 'xfs' }, { value: 'btrfs', label: 'btrfs' },
        ],
      },
      { key: 'mountpoint', label: t('diskAdmin.mountPoint'), type: 'text', value: '', placeholder: '/mnt/data', showIf: (v) => v.fstype !== 'none' },
    ],
    run: async (v) => {
      await diskLVAction(sid(), {
        action: 'create', vg: vg.name, name: String(v.name).trim(),
        useAllFree: v.mode === 'all', sizeGiB: v.mode === 'all' ? 0 : Number(v.sizeGiB),
        fstype: v.fstype === 'none' ? '' : String(v.fstype),
        mountpoint: v.fstype === 'none' ? '' : String(v.mountpoint ?? '').trim(),
      })
      toast.success(t('diskAdmin.opDone'))
      await refresh()
    },
  })
}

function openLVGrow(lv: LogicalVolume): void {
  openOp({
    title: t('diskAdmin.lvGrowTitle', { lv: lv.path }),
    submitLabel: t('diskAdmin.actLvGrow'),
    fields: [
      {
        key: 'mode', label: t('diskAdmin.lvSizeMode'), type: 'select', value: 'all',
        options: [
          { value: 'all', label: t('diskAdmin.lvAll', { free: fmtBytes(vgFree(lv.vgName)) }) },
          { value: 'add', label: t('diskAdmin.lvAdd') },
        ],
      },
      { key: 'sizeGiB', label: 'GiB', type: 'number', value: 10, showIf: (v) => v.mode === 'add' },
    ],
    run: async (v) => {
      await diskLVAction(sid(), {
        action: 'extend', path: lv.path,
        useAllFree: v.mode === 'all', sizeGiB: v.mode === 'all' ? 0 : Number(v.sizeGiB),
      })
      toast.success(t('diskAdmin.opDone'))
      await refresh()
    },
  })
}

function vgFree(vg: string): number {
  return topo.value?.vgs?.find((g) => g.name === vg)?.freeBytes ?? 0
}

function openLVShrink(lv: LogicalVolume): void {
  openOp({
    title: t('diskAdmin.lvShrinkTitle', { lv: lv.path }),
    body: lv.fstype === 'ext4'
      ? t('diskAdmin.ext4OfflineShrink')
      : t('diskAdmin.btrfsOnlineShrink'),
    confirmText: lv.path,
    submitLabel: t('diskAdmin.actLvShrink'),
    fields: [
      {
        key: 'targetGiB', label: t('diskAdmin.shrinkTo'), type: 'number', value: Math.max(1, (lv.sizeBytes >> 30) - 1),
        hint: t('diskAdmin.shrinkCurrent', { n: lv.sizeBytes >> 30 }),
      },
    ],
    run: async (v) => {
      await diskLVAction(sid(), { action: 'reduce', path: lv.path, targetGiB: Number(v.targetGiB), confirm: lv.path })
      toast.success(t('diskAdmin.opDone'))
      await refresh()
    },
  })
}

function askLVRemove(lv: LogicalVolume): void {
  void simpleOp(
    t('diskAdmin.lvRemoveTitle', { lv: lv.path }),
    t('diskAdmin.lvRemoveBody'), t('diskAdmin.actLvRemove'),
    () => diskLVAction(sid(), { action: 'remove', path: lv.path, confirm: lv.path }),
  )
}

watch(currentId, () => {
  void refresh()
})
</script>

<template>
  <div class="disks-view">
    <header class="view-header">
      <div class="header-main">
        <h1 class="view-title">{{ t('diskAdmin.title') }}</h1>
        <p class="view-sub">{{ t('diskAdmin.subtitle') }}</p>
      </div>
      <div v-if="servers.length > 0" class="header-tools">
        <select v-model="currentId" class="instance-select" :aria-label="t('diskAdmin.pickServer')">
          <option v-for="s in servers" :key="s.id" :value="s.id">{{ s.name }} · {{ s.host }}</option>
        </select>
        <span v-if="topo" class="mode-chip" :class="canWrite ? 'mode-chip--rw' : 'mode-chip--ro'">
          <NIcon :size="13"><LockOpen v-if="canWrite" /><Lock v-else /></NIcon>
          {{ canWrite ? t('diskAdmin.writeMode') : t('diskAdmin.readonlyMode') }}
        </span>
        <button v-if="topo && !topo.unlocked" class="btn-secondary" :disabled="lockBusy" @click="askUnlock">
          {{ t('diskAdmin.unlock') }}
        </button>
        <button v-else-if="topo?.unlocked" class="btn-secondary" :disabled="lockBusy" @click="relock">
          {{ t('diskAdmin.relock') }}
        </button>
        <button class="btn-secondary" :disabled="loading" @click="refresh">
          <NIcon :size="13"><Refresh /></NIcon>
          {{ t('diskAdmin.refresh') }}
        </button>
      </div>
    </header>

    <div v-if="servers.length === 0" class="empty-panel">
      <NIcon :size="26"><DeviceFloppy /></NIcon>
      <p class="empty-title">{{ t('diskAdmin.noServers') }}</p>
      <p class="empty-desc">{{ t('diskAdmin.noServersDesc') }}</p>
      <router-link to="/settings/servers" class="btn-primary">{{ t('diskAdmin.goToSettings') }}</router-link>
    </div>

    <template v-else>
      <div v-if="loading && !topo" class="disks-state">{{ t('diskAdmin.loading') }}</div>
      <div v-else-if="loadErr && !topo" class="disks-error">
        <NIcon :size="20"><AlertTriangle /></NIcon>
        <p>{{ loadErr }}</p>
        <button class="btn-secondary" @click="refresh">{{ t('diskAdmin.retry') }}</button>
      </div>

      <template v-else-if="topo">
        <!-- 信息条:登录用户 / 提权 / 工具缺失提示 -->
        <div class="info-bar">
          <span class="info-chip mono">{{ t('diskAdmin.loginAs') }} {{ topo.loginUser }}</span>
          <span v-if="!topo.privileged" class="info-chip info-chip--warn">{{ t('diskAdmin.noPrivilege') }}</span>
          <span v-if="!topo.tools.parted" class="info-chip info-chip--warn">parted ✗</span>
          <span v-if="!topo.tools.lvm" class="info-chip info-chip--warn">{{ t('diskAdmin.lvmMissing') }}</span>
          <span v-if="!topo.tools.mkfsExt4 && !topo.tools.mkfsXfs" class="info-chip info-chip--warn">mkfs ✗</span>
        </div>

        <!-- 块设备树 -->
        <section class="panel">
          <div class="panel-head">
            <h2 class="panel-title"><NIcon :size="15"><DeviceFloppy /></NIcon> {{ t('diskAdmin.devicesTitle') }}</h2>
          </div>
          <div class="grid-scroll">
            <table class="data-table">
              <thead>
                <tr>
                  <th>{{ t('diskAdmin.colDevice') }}</th>
                  <th>{{ t('diskAdmin.colType') }}</th>
                  <th>{{ t('diskAdmin.colSize') }}</th>
                  <th>{{ t('diskAdmin.colFstype') }}</th>
                  <th>{{ t('diskAdmin.colMount') }}</th>
                  <th>{{ t('diskAdmin.colUsed') }}</th>
                  <th>{{ t('diskAdmin.colActions') }}</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="row in rows" :key="row.dev.path">
                  <td>
                    <span class="dev-path mono" :style="{ paddingLeft: `${row.depth * 16}px` }">{{ row.dev.path }}</span>
                    <span v-if="row.dev.model || row.dev.serial" class="dev-meta">{{ row.dev.model }} {{ row.dev.serial }}</span>
                  </td>
                  <td>
                    <span class="type-badge" :class="`type-${row.dev.type}`">{{ typeLabel(row.dev.type) }}</span>
                    <span v-if="row.dev.protected" class="prot-badge" :title="t('diskAdmin.protectedTitle')">
                      <NIcon :size="11"><Lock /></NIcon>{{ t('diskAdmin.protectedBadge') }}
                    </span>
                  </td>
                  <td class="mono">{{ fmtBytes(row.dev.sizeBytes) }}</td>
                  <td><span v-if="row.dev.fstype" class="fs-badge mono">{{ row.dev.fstype }}</span><span v-else>—</span></td>
                  <td>
                    <span v-if="devMounted(row.dev)" class="mono">{{ (row.dev.mountpoints ?? []).join(', ') }}</span>
                    <span v-else>—</span>
                  </td>
                  <td>
                    <div v-if="row.dev.usedBytes > 0 && row.dev.sizeBytes > 0" class="used-cell">
                      <div class="used-bar">
                        <div class="used-fill" :style="{ width: `${Math.min(100, (row.dev.usedBytes / row.dev.sizeBytes) * 100)}%` }"></div>
                      </div>
                      <span class="used-text mono">{{ fmtBytes(row.dev.usedBytes) }}</span>
                    </div>
                    <span v-else>—</span>
                  </td>
                  <td>
                    <div class="row-actions" v-if="!row.dev.protected">
                      <!-- 空整盘:初始化(分区+格式化+挂载) -->
                      <template v-if="isBlankDisk(row.dev) && canWrite && topo.tools.parted">
                        <button class="mini-btn" @click="openInit(row.dev)">
                          <NIcon :size="12"><CirclePlus /></NIcon>{{ t('diskAdmin.actInit') }}
                        </button>
                        <button v-if="topo.tools.lvm" class="mini-btn" @click="askPVCreate(row.dev.path)">{{ t('diskAdmin.actPvCreate') }}</button>
                      </template>
                      <!-- 分区 -->
                      <template v-else-if="row.dev.type === 'part' && canWrite">
                        <button v-if="!devMounted(row.dev)" class="mini-btn mini-btn--danger" @click="openFormat(row.dev.path)">
                          <NIcon :size="12"><AlertTriangle /></NIcon>{{ t('diskAdmin.actFormat') }}
                        </button>
                        <button v-if="row.dev.fstype && !devMounted(row.dev)" class="mini-btn" @click="openMount(row.dev.path)">
                          <NIcon :size="12"><PlugConnected /></NIcon>{{ t('diskAdmin.actMount') }}
                        </button>
                        <button v-if="devMounted(row.dev)" class="mini-btn" @click="askUmount((row.dev.mountpoints ?? [])[0])">
                          {{ t('diskAdmin.actUmount') }}
                        </button>
                        <button v-if="row.isLastPart && row.dev.fstype && growSupported(row.dev.fstype)" class="mini-btn" @click="askGrow(row.dev.path, row.dev.fstype)">
                          <NIcon :size="12"><ArrowUp /></NIcon>{{ t('diskAdmin.actGrow') }}
                        </button>
                        <button
                          v-if="row.dev.fstype && shrinkSupported(row.dev.fstype)"
                          class="mini-btn mini-btn--danger"
                          :title="row.dev.fstype === 'xfs' ? t('diskAdmin.xfsNoShrink') : ''"
                          @click="openShrink(row.dev.path, row.dev.sizeBytes >> 30, row.dev.fstype)"
                        >
                          <NIcon :size="12"><ArrowDown /></NIcon>{{ t('diskAdmin.actShrink') }}
                        </button>
                        <button v-if="!row.dev.fstype && !isPV(row.dev.path) && topo.tools.lvm" class="mini-btn" @click="askPVCreate(row.dev.path)">{{ t('diskAdmin.actPvCreate') }}</button>
                        <button v-if="!devMounted(row.dev) && !isPV(row.dev.path)" class="mini-btn mini-btn--danger" @click="askDeletePartition(row.dev)">
                          <NIcon :size="12"><Trash /></NIcon>{{ t('diskAdmin.actDelete') }}
                        </button>
                      </template>
                      <span v-if="!canWrite" class="lock-hint">{{ t('diskAdmin.readonlyMode') }}</span>
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </section>

        <!-- LVM 面板 -->
        <section class="panel">
          <div class="panel-head">
            <h2 class="panel-title"><NIcon :size="15"><Database /></NIcon> {{ t('diskAdmin.lvmTitle') }}</h2>
            <button v-if="canWrite && topo.tools.lvm" class="btn-secondary" @click="openVGCreate">
              <NIcon :size="13"><CirclePlus /></NIcon>{{ t('diskAdmin.actVgCreate') }}
            </button>
          </div>

          <div v-if="!topo.tools.lvm" class="disks-state">{{ t('diskAdmin.lvmMissing') }}</div>
          <div v-else-if="(topo.vgs ?? []).length === 0" class="disks-state">
            {{ freePVs.length > 0 ? t('diskAdmin.vgHasFreePvs', { n: freePVs.length }) : t('diskAdmin.lvmNoVgs') }}
          </div>

          <div v-else class="vg-list">
            <div v-for="vg in topo.vgs" :key="vg.name" class="vg-card">
              <div class="vg-head">
                <span class="vg-name mono">{{ vg.name }}</span>
                <span v-if="vg.protected" class="prot-badge"><NIcon :size="11"><Lock /></NIcon>{{ t('diskAdmin.protectedBadge') }}</span>
                <span class="info-chip">{{ t('diskAdmin.vgSize') }} {{ fmtBytes(vg.sizeBytes) }}</span>
                <span class="info-chip">{{ t('diskAdmin.vgFree') }} {{ fmtBytes(vg.freeBytes) }}</span>
                <span class="info-chip">{{ vg.pvCount }} PV · {{ vg.lvCount }} LV</span>
                <span class="vg-actions" v-if="canWrite && !vg.protected">
                  <button class="mini-btn" @click="openLVCreate(vg)"><NIcon :size="12"><CirclePlus /></NIcon>{{ t('diskAdmin.actLvCreate') }}</button>
                  <button class="mini-btn" @click="openVGExtend(vg)">{{ t('diskAdmin.actVgExtend') }}</button>
                  <button class="mini-btn mini-btn--danger" @click="askVGRemove(vg)"><NIcon :size="12"><Trash /></NIcon>{{ t('diskAdmin.actVgRemove') }}</button>
                </span>
              </div>

              <table v-if="lvsOf(vg.name).length > 0" class="data-table">
                <thead>
                  <tr>
                    <th>{{ t('diskAdmin.lvPath') }}</th>
                    <th>{{ t('diskAdmin.colSize') }}</th>
                    <th>{{ t('diskAdmin.colFstype') }}</th>
                    <th>{{ t('diskAdmin.colMount') }}</th>
                    <th>{{ t('diskAdmin.colUsed') }}</th>
                    <th>{{ t('diskAdmin.colActions') }}</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="lv in lvsOf(vg.name)" :key="lv.path">
                    <td><span class="mono">{{ lv.path }}</span><span v-if="!lv.active" class="dim-tag">inactive</span></td>
                    <td class="mono">{{ fmtBytes(lv.sizeBytes) }}</td>
                    <td><span v-if="lv.fstype" class="fs-badge mono">{{ lv.fstype }}</span><span v-else>—</span></td>
                    <td><span v-if="lvMounted(lv)" class="mono">{{ (lv.mountpoints ?? []).join(', ') }}</span><span v-else>—</span></td>
                    <td class="mono">{{ lv.usedBytes > 0 ? fmtBytes(lv.usedBytes) : '—' }}</td>
                    <td>
                      <div class="row-actions" v-if="canWrite && !lv.protected">
                        <button v-if="!lvMounted(lv) && lv.fstype" class="mini-btn" @click="openMount(lv.path)">
                          <NIcon :size="12"><PlugConnected /></NIcon>{{ t('diskAdmin.actMount') }}
                        </button>
                        <button v-if="lvMounted(lv)" class="mini-btn" @click="askUmount((lv.mountpoints ?? [])[0])">{{ t('diskAdmin.actUmount') }}</button>
                        <button v-if="!lvMounted(lv) && lv.fstype" class="mini-btn mini-btn--danger" @click="openFormat(lv.path, lv.fstype)">{{ t('diskAdmin.actFormat') }}</button>
                        <button class="mini-btn" @click="openLVGrow(lv)"><NIcon :size="12"><ArrowUp /></NIcon>{{ t('diskAdmin.actLvGrow') }}</button>
                        <button
                          v-if="!lv.fstype || shrinkSupported(lv.fstype)"
                          class="mini-btn mini-btn--danger"
                          @click="openLVShrink(lv)"
                        ><NIcon :size="12"><ArrowDown /></NIcon>{{ t('diskAdmin.actLvShrink') }}</button>
                        <button v-if="!lvMounted(lv)" class="mini-btn mini-btn--danger" @click="askLVRemove(lv)">
                          <NIcon :size="12"><Trash /></NIcon>{{ t('diskAdmin.actLvRemove') }}
                        </button>
                      </div>
                      <span v-else-if="lv.protected" class="prot-badge"><NIcon :size="11"><Lock /></NIcon></span>
                    </td>
                  </tr>
                </tbody>
              </table>

              <div class="vg-pvs">
                <span class="dim-label">{{ t('diskAdmin.colPv') }}:</span>
                <template v-for="pv in pvsOf(vg.name)" :key="pv.path">
                  <span class="info-chip mono">{{ pv.path }} · {{ fmtBytes(pv.freeBytes) }}/{{ fmtBytes(pv.sizeBytes) }}</span>
                  <button
                    v-if="canWrite && !vg.protected && pv.freeBytes + 1048576 >= pv.sizeBytes"
                    class="mini-btn mini-btn--danger"
                    @click="askVGReduce(vg.name, pv.path)"
                  >{{ t('diskAdmin.actVgReduce') }}</button>
                </template>
              </div>
            </div>
          </div>

          <!-- 未入组 PV -->
          <div v-if="freePVs.length > 0" class="vg-pvs free-pvs">
            <span class="dim-label">{{ t('diskAdmin.freePvsTitle') }}:</span>
            <template v-for="pv in freePVs" :key="pv.path">
              <span class="info-chip mono">{{ pv.path }} · {{ fmtBytes(pv.sizeBytes) }}</span>
              <button v-if="canWrite" class="mini-btn mini-btn--danger" @click="askPVRemove(pv.path)">
                <NIcon :size="12"><Trash /></NIcon>{{ t('diskAdmin.actPvRemove') }}
              </button>
            </template>
          </div>
        </section>
      </template>
    </template>

    <!-- 通用操作弹窗 -->
    <div v-if="dlg" class="dlg-mask" @click.self="closeDlg">
      <div class="dlg" role="dialog" :aria-label="dlg.title">
        <h3 class="dlg-title">{{ dlg.title }}</h3>
        <p v-if="dlg.body" class="dlg-body">{{ dlg.body }}</p>
        <div class="dlg-fields">
          <template v-for="f in visibleFields" :key="f.key">
            <template v-if="f.type === 'check'">
              <label class="dlg-check">
                <input type="checkbox" :checked="!!f.value" @change="f.value = ($event.target as HTMLInputElement).checked">
                {{ f.label }}
              </label>
            </template>
            <template v-else>
              <label class="dlg-label" :for="`op-${f.key}`">{{ f.label }}</label>
              <select
                v-if="f.type === 'select'"
                :id="`op-${f.key}`" class="dlg-input"
                :value="String(f.value)"
                @change="f.value = ($event.target as HTMLSelectElement).value"
              >
                <option v-for="o in f.options" :key="o.value" :value="o.value">{{ o.label }}</option>
              </select>
              <input
                v-else-if="f.type === 'number'"
                :id="`op-${f.key}`" class="dlg-input" type="number" min="1"
                :value="Number(f.value)"
                @input="f.value = Number(($event.target as HTMLInputElement).value)"
              >
              <input
                v-else
                :id="`op-${f.key}`" class="dlg-input" type="text"
                v-model="f.value as string"
                :placeholder="f.placeholder"
              >
            </template>
            <p v-if="f.hint" class="dlg-hint">{{ f.hint }}</p>
          </template>
        </div>
        <div v-if="dlg.confirmText" class="dlg-confirm">
          <label class="dlg-label" for="op-confirm">{{ t('diskAdmin.confirmPrompt', { text: dlg.confirmText }) }}</label>
          <input id="op-confirm" v-model="confirmInput" class="dlg-input mono" type="text" autocomplete="off">
        </div>
        <div class="dlg-actions">
          <button class="btn-secondary" :disabled="dlg.busy" @click="closeDlg">{{ t('diskAdmin.cancel') }}</button>
          <button class="btn-primary" :disabled="!canSubmitDlg" @click="submitDlg">
            {{ dlg.busy ? t('diskAdmin.loading') : dlg.submitLabel }}
          </button>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.disks-view {
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
.mode-chip {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 0.74rem;
  padding: 4px 10px;
  border-radius: var(--rounded-full);
  border: 1px solid var(--color-border);
}
.mode-chip--ro { color: var(--color-dim); background: var(--color-inset); }
.mode-chip--rw { color: var(--color-primary); border-color: var(--color-primary); font-weight: 600; }

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
.empty-title { font-size: 0.92rem; font-weight: 600; color: var(--color-text); margin: 0; }
.empty-desc { font-size: 0.82rem; color: var(--color-faint); max-width: 52ch; margin: 0; }

.disks-state {
  padding: 40px 24px;
  text-align: center;
  color: var(--color-faint);
  font-size: 0.84rem;
}
.disks-error {
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

.info-bar { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.info-chip {
  font-size: 0.74rem;
  color: var(--color-dim);
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  padding: 4px 10px;
  border-radius: var(--rounded-full);
}
.info-chip--warn {
  color: var(--color-red);
  border-color: var(--color-red-line);
  background: var(--color-red-soft);
}

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
.panel-title {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 0.95rem;
  font-weight: 650;
  color: var(--color-text);
  margin: 0;
}

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

.dev-path { font-size: 0.8rem; color: var(--color-text); }
.dev-meta { display: block; font-size: 0.68rem; color: var(--color-faint); margin-top: 2px; }
.type-badge {
  display: inline-block;
  font-size: 0.68rem;
  padding: 2px 8px;
  border-radius: var(--rounded-full);
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  color: var(--color-dim);
}
.type-disk { color: var(--color-primary); border-color: var(--color-primary); }
.fs-badge {
  font-size: 0.7rem;
  padding: 2px 8px;
  border-radius: var(--rounded-full);
  background: var(--color-inset);
}
.prot-badge {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  margin-left: 6px;
  font-size: 0.66rem;
  color: var(--color-red);
  border: 1px solid var(--color-red-line);
  background: var(--color-red-soft);
  padding: 2px 8px;
  border-radius: var(--rounded-full);
}
.dim-tag { margin-left: 6px; font-size: 0.66rem; color: var(--color-faint); }

.used-cell { display: flex; align-items: center; gap: 8px; min-width: 140px; }
.used-bar {
  flex: 1;
  height: 6px;
  border-radius: var(--rounded-full);
  background: var(--color-inset);
  overflow: hidden;
}
.used-fill { height: 100%; background: var(--color-primary); }
.used-text { font-size: 0.7rem; color: var(--color-faint); white-space: nowrap; }

.row-actions { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
.mini-btn {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  height: 24px;
  padding: 0 8px;
  font-size: 0.7rem;
  background: var(--color-card);
  color: var(--color-dim);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded);
  cursor: pointer;
  white-space: nowrap;
}
.mini-btn:hover { color: var(--color-text); border-color: var(--color-primary); }
.mini-btn--danger { color: var(--color-red); }
.mini-btn--danger:hover { border-color: var(--color-red); color: var(--color-red); }
.lock-hint { font-size: 0.68rem; color: var(--color-faint); }

.vg-list { display: flex; flex-direction: column; gap: 12px; }
.vg-card {
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  padding: 10px 12px;
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.vg-head { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.vg-name { font-size: 0.86rem; font-weight: 650; color: var(--color-text); }
.vg-actions { display: inline-flex; gap: 6px; margin-left: auto; }
.vg-pvs { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.dim-label { font-size: 0.72rem; color: var(--color-faint); }
.free-pvs { border-top: 1px dashed var(--color-border); padding-top: 10px; }

/* ─── 操作弹窗 ─────────────────────────────────────────────────────────────── */
.dlg-mask {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.45);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 60;
}
.dlg {
  width: min(520px, calc(100vw - 32px));
  max-height: min(80vh, 720px);
  overflow: auto;
  background: var(--color-card);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded-lg, 10px);
  padding: 18px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.dlg-title { font-size: 1rem; font-weight: 650; color: var(--color-text); margin: 0; }
.dlg-body { font-size: 0.8rem; color: var(--color-dim); white-space: pre-line; margin: 0; }
.dlg-fields { display: flex; flex-direction: column; gap: 8px; }
.dlg-label { font-size: 0.74rem; color: var(--color-faint); margin: 0; }
.dlg-input {
  height: 32px;
  padding: 0 10px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  color: var(--color-text);
  font-size: 0.8rem;
  width: 100%;
  box-sizing: border-box;
}
.dlg-input:focus { outline: none; border-color: var(--color-primary); }
.dlg-check { display: flex; align-items: center; gap: 8px; font-size: 0.8rem; color: var(--color-dim); }
.dlg-hint { font-size: 0.7rem; color: var(--color-faint); margin: 0; }
.dlg-confirm {
  border-top: 1px dashed var(--color-border);
  padding-top: 10px;
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.dlg-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 4px; }
</style>
