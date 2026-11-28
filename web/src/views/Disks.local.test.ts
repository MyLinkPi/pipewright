/**
 * Disks.local.test.ts — 系统设备在线扩容的按钮守卫回归测试。
 *
 * 系统设备(protected)继续拒绝格式化/删除/缩容/卸载,但末分区与系统 LV 允许在线
 * 扩容;未解锁时所有写操作入口都不可点。后端扩前备份由领域层测试覆盖,这里只验证
 * 前端按操作区分的按钮组与警示文案。
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises, type VueWrapper } from '@vue/test-utils'
import { ref } from 'vue'

const listServersMock = vi.fn()
const getTopologyMock = vi.fn()
const growMock = vi.fn()
const lvActionMock = vi.fn()
const confirmOpenMock = vi.fn()

vi.mock('vue-router', () => ({
  useRoute: () => ({ query: {} }),
  RouterLink: { props: ['to'], template: '<a><slot /></a>' },
}))

vi.mock('../api/servers', () => ({ listServers: (...a: unknown[]) => listServersMock(...a) }))

vi.mock('../api/diskAdmin', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../api/diskAdmin')>()
  return {
    ...actual,
    getDiskTopology: (...a: unknown[]) => getTopologyMock(...a),
    growDiskPartition: (...a: unknown[]) => growMock(...a),
    diskLVAction: (...a: unknown[]) => lvActionMock(...a),
  }
})

vi.mock('../composables/useConfirm', () => ({
  useConfirm: () => ({
    pending: ref(null),
    open: (opts: unknown) => {
      confirmOpenMock(opts)
      return Promise.resolve(true)
    },
  }),
}))

vi.mock('../composables/useToast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

import Disks from './Disks.vue'
import type { DiskTopology, BlockDevice, LogicalVolume } from '../api/diskAdmin'
import type { Server } from '../api/servers'

function part(over: Partial<BlockDevice>): BlockDevice {
  return {
    path: '', name: '', type: 'part', sizeBytes: 100 << 30, fstype: 'ext4',
    mountpoints: null, uuid: '', partuuid: '', serial: '', model: '', readOnly: false,
    pttype: 'gpt', parent: '', protected: false, usedBytes: 0, children: null,
    ...over,
  }
}

function lv(over: Partial<LogicalVolume>): LogicalVolume {
  return {
    path: '', vgName: 'rootvg', name: '', sizeBytes: 50 << 30, attr: '',
    fstype: 'ext4', mountpoints: null, usedBytes: 0, active: true, protected: false,
    ...over,
  }
}

function topology(unlocked = true): DiskTopology {
  return {
    devices: [
      {
        ...part({ path: '/dev/sda', name: 'sda', type: 'disk', sizeBytes: 200 << 30, fstype: '', pttype: 'gpt', protected: true }),
        children: [
          part({ path: '/dev/sda1', name: 'sda1', mountpoints: ['/boot'], protected: true }),
          part({ path: '/dev/sda2', name: 'sda2', mountpoints: ['/'], protected: true, usedBytes: 143 << 30, parent: '/dev/sda' }),
        ],
      },
      {
        ...part({ path: '/dev/sdb', name: 'sdb', type: 'disk', sizeBytes: 100 << 30, fstype: '', pttype: 'gpt' }),
        children: [part({ path: '/dev/sdb1', name: 'sdb1', parent: '/dev/sdb' })],
      },
    ],
    pvs: [],
    vgs: [{ name: 'rootvg', sizeBytes: 50 << 30, freeBytes: 20 << 30, pvCount: 1, lvCount: 1, protected: true }],
    lvs: [lv({ path: '/dev/rootvg/root', name: 'root', mountpoints: ['/'], protected: true })],
    tools: {
      parted: true, partx: true, sfdisk: true, mkfsExt4: true, mkfsXfs: true,
      mkfsBtrfs: true, e2fsck: true, resize2fs: true, xfsGrowfs: true,
      btrfs: true, lvm: true, findmnt: true,
    },
    loginUser: 'root', privileged: true, unlocked,
  }
}

async function mountView(): Promise<VueWrapper> {
  const w = mount(Disks, { global: { stubs: { 'router-link': { template: '<a><slot /></a>' } } } })
  await flushPromises()
  return w
}

/** 按首列设备路径定位表格行(块设备表与 LV 表共用)。 */
function rowByPath(w: VueWrapper, path: string) {
  return w.findAll('tbody tr').find((tr) => tr.findAll('td')[0]?.text().includes(path))
}

function buttons(row: ReturnType<typeof rowByPath>): string[] {
  return row?.findAll('button').map((b) => b.text().trim()) ?? []
}

beforeEach(() => {
  document.body.innerHTML = ''
  vi.clearAllMocks()
  listServersMock.mockResolvedValue([{ id: 's1', name: 'srv', host: '10.0.0.1' }] as unknown as Server[])
  getTopologyMock.mockResolvedValue(topology())
  growMock.mockImplementation(async () => ({ ok: true, topology: topology() }))
  lvActionMock.mockImplementation(async () => ({ ok: true, topology: topology() }))
})

describe('Disks · 系统设备在线扩容入口', () => {
  it('系统末分区只显示扩容;非末分区与其他按钮仍隐藏;普通分区按钮不受影响', async () => {
    const w = await mountView()

    expect(buttons(rowByPath(w, '/dev/sda2'))).toEqual(['扩容'])
    expect(buttons(rowByPath(w, '/dev/sda1'))).toEqual([])
    expect(buttons(rowByPath(w, '/dev/sdb1'))).toEqual(['格式化', '挂载', '扩容', '缩容', '删除'])
    w.unmount()
  })

  it('系统 LV 只显示扩容;点击后给出备份警示文案并调用扩容接口', async () => {
    const w = await mountView()

    const row = rowByPath(w, '/dev/rootvg/root')
    expect(buttons(row)).toEqual(['扩容'])
    await row?.find('button').trigger('click')
    await flushPromises()

    expect(w.find('.dlg-mask').text()).toContain('备份 VG 元数据')
    await w.find('.dlg-actions .btn-primary').trigger('click')
    await flushPromises()

    expect(lvActionMock).toHaveBeenCalledWith('s1', expect.objectContaining({ action: 'extend', path: '/dev/rootvg/root' }))
    w.unmount()
  })

  it('系统分区扩容确认框给出分区表备份警示,确认后调用 grow 接口', async () => {
    const w = await mountView()

    await rowByPath(w, '/dev/sda2')?.find('button').trigger('click')
    await flushPromises()

    expect(confirmOpenMock).toHaveBeenCalledWith(expect.objectContaining({
      body: expect.stringContaining('备份分区表'),
    }))
    expect(growMock).toHaveBeenCalledWith('s1', '/dev/sda2')
    w.unmount()
  })

  it('远端缺 sfdisk 时系统分区扩容按钮禁用并给出提示', async () => {
    const topo = topology()
    topo.tools.sfdisk = false
    getTopologyMock.mockResolvedValue(topo)
    const w = await mountView()

    const btn = rowByPath(w, '/dev/sda2')?.find('button')
    expect(btn?.attributes('disabled')).toBeDefined()
    expect(btn?.attributes('title')).toContain('sfdisk')
    w.unmount()
  })

  it('未解锁时系统分区与系统 LV 都没有写操作按钮', async () => {
    getTopologyMock.mockResolvedValue(topology(false))
    const w = await mountView()

    expect(buttons(rowByPath(w, '/dev/sda2'))).toEqual([])
    expect(buttons(rowByPath(w, '/dev/rootvg/root'))).toEqual([])
    w.unmount()
  })
})
