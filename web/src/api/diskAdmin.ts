/**
 * 磁盘管理 API client(顶级「磁盘」页:块设备/LVM 拓扑 + 分区/格式化/挂载/扩缩容)。
 *
 * 服务器默认只读(拓扑可浏览),写操作被服务端拒绝;解锁是显式确认动作(30 分钟
 * TTL,进程内状态)。危险操作(格式化/删除/缩容/移除)要求 confirm 字段逐字等于
 * 目标路径(服务端强校验 + 审计)。fstab 持久化由服务端「备份-校验-原子替换」完成。
 *
 * GET  /api/diskadmin/servers/{id}/topology        → DiskTopology
 * POST /api/diskadmin/servers/{id}/unlock          → { unlocked, expiresAt }  (needs CSRF)
 * POST /api/diskadmin/servers/{id}/lock            → { unlocked: false }      (needs CSRF)
 * POST /api/diskadmin/servers/{id}/partitions      → DiskOpResult             (needs CSRF)
 * POST /api/diskadmin/servers/{id}/partitions/delete  → DiskOpResult          (needs CSRF)
 * POST /api/diskadmin/servers/{id}/partitions/grow    → DiskOpResult          (needs CSRF)
 * POST /api/diskadmin/servers/{id}/partitions/shrink  → DiskOpResult          (needs CSRF)
 * POST /api/diskadmin/servers/{id}/format          → DiskOpResult             (needs CSRF)
 * POST /api/diskadmin/servers/{id}/mount           → DiskOpResult             (needs CSRF)
 * POST /api/diskadmin/servers/{id}/umount          → DiskOpResult             (needs CSRF)
 * POST /api/diskadmin/servers/{id}/lvm/pv          → DiskOpResult             (needs CSRF)
 * POST /api/diskadmin/servers/{id}/lvm/vg          → DiskOpResult             (needs CSRF)
 * POST /api/diskadmin/servers/{id}/lvm/lv          → DiskOpResult             (needs CSRF)
 */

import { http } from './http'

/** lsblk 树节点(宽容解析:采不到的字段为零值)。 */
export interface BlockDevice {
  path: string
  name: string
  type: string // disk|part|lvm|crypt|rom|loop
  sizeBytes: number
  fstype: string
  mountpoints: string[] | null
  uuid: string
  partuuid: string
  serial: string
  model: string
  readOnly: boolean
  pttype: string
  parent: string
  /** 系统设备(关键挂载/swap/根 VG):服务端拒绝一切写操作。 */
  protected: boolean
  usedBytes: number
  children: BlockDevice[] | null
}

export interface PhysicalVolume {
  path: string
  vgName: string
  sizeBytes: number
  freeBytes: number
  protected: boolean
}

export interface VolumeGroup {
  name: string
  sizeBytes: number
  freeBytes: number
  pvCount: number
  lvCount: number
  protected: boolean
}

export interface LogicalVolume {
  path: string
  vgName: string
  name: string
  sizeBytes: number
  attr: string
  fstype: string
  mountpoints: string[] | null
  usedBytes: number
  active: boolean
  protected: boolean
}

/** 远端工具可用性(command -v 探测;缺失时前端禁用对应操作并提示)。 */
export interface DiskTools {
  parted: boolean
  partx: boolean
  mkfsExt4: boolean
  mkfsXfs: boolean
  mkfsBtrfs: boolean
  e2fsck: boolean
  resize2fs: boolean
  xfsGrowfs: boolean
  btrfs: boolean
  lvm: boolean
  findmnt: boolean
}

export interface DiskTopology {
  devices: BlockDevice[] | null
  pvs: PhysicalVolume[] | null
  vgs: VolumeGroup[] | null
  lvs: LogicalVolume[] | null
  tools: DiskTools
  loginUser: string
  privileged: boolean
  unlocked: boolean
  unlockExpiresAt?: string
}

/** 写操作响应:ok + 尽力带回的最新拓扑(失败为 null,前端自行重拉)。 */
export interface DiskOpResult {
  ok: boolean
  topology: DiskTopology | null
}

export interface CreatePartitionInput {
  device: string
  sizeSpec: string // "100%" | "<N>GiB"
  makeLabel: boolean
}

export interface DeletePartitionInput {
  partition: string
  confirm: string
}

export interface ShrinkInput {
  target: string
  targetGiB: number
  confirm: string
}

export interface FormatInput {
  target: string
  fstype: 'ext4' | 'xfs' | 'btrfs'
  label?: string
  confirm: string
}

export interface MountInput {
  target: string
  mountpoint: string
  options?: string
}

export interface UmountInput {
  mountpoint: string
  removeDir?: boolean
}

export interface PVActionInput {
  action: 'create' | 'remove'
  device: string
  confirm?: string
}

export interface VGActionInput {
  action: 'create' | 'extend' | 'reduce' | 'remove'
  name: string
  devices?: string[]
  device?: string
  confirm?: string
}

export interface LVActionInput {
  action: 'create' | 'extend' | 'reduce' | 'remove'
  vg?: string
  name?: string
  path?: string
  sizeGiB?: number
  targetGiB?: number
  useAllFree?: boolean
  fstype?: string
  mountpoint?: string
  confirm?: string
}

const base = (id: string): string => `/api/diskadmin/servers/${encodeURIComponent(id)}`

export async function getDiskTopology(serverId: string): Promise<DiskTopology> {
  return http.get<DiskTopology>(`${base(serverId)}/topology`)
}

export async function unlockDiskServer(serverId: string): Promise<{ unlocked: boolean; expiresAt: string }> {
  return http.post(`${base(serverId)}/unlock`)
}

export async function lockDiskServer(serverId: string): Promise<{ unlocked: boolean }> {
  return http.post(`${base(serverId)}/lock`)
}

export async function createDiskPartition(serverId: string, input: CreatePartitionInput): Promise<DiskOpResult> {
  return http.post(`${base(serverId)}/partitions`, input)
}

export async function deleteDiskPartition(serverId: string, input: DeletePartitionInput): Promise<DiskOpResult> {
  return http.post(`${base(serverId)}/partitions/delete`, input)
}

export async function growDiskPartition(serverId: string, target: string): Promise<DiskOpResult> {
  return http.post(`${base(serverId)}/partitions/grow`, { target })
}

export async function shrinkDiskTarget(serverId: string, input: ShrinkInput): Promise<DiskOpResult> {
  return http.post(`${base(serverId)}/partitions/shrink`, input)
}

export async function formatDiskTarget(serverId: string, input: FormatInput): Promise<DiskOpResult> {
  return http.post(`${base(serverId)}/format`, input)
}

export async function mountDiskTarget(serverId: string, input: MountInput): Promise<DiskOpResult> {
  return http.post(`${base(serverId)}/mount`, input)
}

export async function umountDiskTarget(serverId: string, input: UmountInput): Promise<DiskOpResult> {
  return http.post(`${base(serverId)}/umount`, input)
}

export async function diskPVAction(serverId: string, input: PVActionInput): Promise<DiskOpResult> {
  return http.post(`${base(serverId)}/lvm/pv`, input)
}

export async function diskVGAction(serverId: string, input: VGActionInput): Promise<DiskOpResult> {
  return http.post(`${base(serverId)}/lvm/vg`, input)
}

export async function diskLVAction(serverId: string, input: LVActionInput): Promise<DiskOpResult> {
  return http.post(`${base(serverId)}/lvm/lv`, input)
}
