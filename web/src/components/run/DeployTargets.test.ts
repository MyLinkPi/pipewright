/**
 * DeployTargets.test.ts — 部署目标结果卡渲染 +「重试失败目标」按钮可见性回归。
 *
 * 守住一处修复:统一滚动部署在某批失败后会停止铺开,未轮到的机器被后端写成 pending
 * (未部署,仍跑旧版本),重试时会一并推进。若按钮只统计 failed/rolled_back,
 * 「全 pending」的现场就不出按钮,用户无法继续铺开 → 这里以组件级断言钉住该行为。
 *
 * 同时守住两点渲染语义:header 统计里 pending 单独成项(不并入「失败」),
 * 且 pending 目标的状态徽标有本地化文案(不会漏成裸 token / 空白)。
 *
 * i18n 插件与 zh-CN locale 由 src/test/setup.ts 全局安装。
 */
import { describe, it, expect } from 'vitest'
import { mount } from '@vue/test-utils'
import DeployTargets from './DeployTargets.vue'
import { t } from '../../i18n'
import type { DeployTarget, TargetStatus } from '../../api/runs'

function target(serverId: string, status: TargetStatus, message = ''): DeployTarget {
  return {
    serverId,
    serverName: `web-${serverId}`,
    status,
    message,
    startedAt: '2026-01-01T00:00:00Z',
    // pending = 还没轮到,自然没有结束时间;其余状态给一个已完成时刻。
    finishedAt: status === 'pending' ? null : '2026-01-01T00:01:30Z',
  }
}

function mountTargets(targets: DeployTarget[]) {
  return mount(DeployTargets, { props: { targets } })
}

describe('DeployTargets · retry button covers pending targets', () => {
  it('shows the retry button when nothing failed but targets are still pending', () => {
    const wrapper = mountTargets([
      target('1', 'success'),
      target('2', 'pending'),
      target('3', 'pending'),
    ])

    expect(wrapper.find('.dt-foot').exists()).toBe(true)
    expect(wrapper.get('.dt-retry-btn').text()).toContain(t('run.retryFailedTargets', { n: 2 }))
  })

  it('counts failed + rolled_back + pending as retryable', () => {
    const wrapper = mountTargets([
      target('1', 'success'),
      target('2', 'failed', '健康检查失败'),
      target('3', 'rolled_back', '已回滚到上一镜像'),
      target('4', 'pending'),
      target('5', 'pending'),
    ])

    expect(wrapper.get('.dt-retry-btn').text()).toContain(t('run.retryFailedTargets', { n: 4 }))
  })

  it('hides the retry button when every target succeeded', () => {
    const wrapper = mountTargets([target('1', 'success'), target('2', 'success')])

    expect(wrapper.find('.dt-foot').exists()).toBe(false)
  })

  it('emits retry when the button is clicked', async () => {
    const wrapper = mountTargets([target('1', 'pending')])

    await wrapper.get('.dt-retry-btn').trigger('click')

    expect(wrapper.emitted('retry')).toHaveLength(1)
  })
})

describe('DeployTargets · header stats keep pending apart from failed', () => {
  it('renders its own pending stat instead of folding it into the failed count', () => {
    const wrapper = mountTargets([
      target('1', 'failed', 'boom'),
      target('2', 'failed', 'boom'),
      target('3', 'pending'),
      target('4', 'pending'),
      target('5', 'pending'),
    ])

    expect(wrapper.get('.dt-stat--bad').text()).toBe(t('run.countBad', { n: 2 }))
    expect(wrapper.get('.dt-stat--pending').text()).toBe(t('run.countPending', { n: 3 }))
  })

  it('omits the pending stat when no target is pending', () => {
    const wrapper = mountTargets([target('1', 'success'), target('2', 'failed', 'boom')])

    expect(wrapper.find('.dt-stat--pending').exists()).toBe(false)
  })
})

describe('DeployTargets · pending badge rendering', () => {
  it('localizes the pending badge instead of leaking the raw status token', () => {
    const wrapper = mountTargets([target('1', 'pending')])

    const badge = wrapper.get('.dt-badge').text().trim()
    expect(badge).toBe(t('run.targetStatusPending'))
    expect(badge).not.toBe('pending')
  })
})
