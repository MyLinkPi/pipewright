/**
 * pipelineLabels.test.ts — 后端默认阶段/任务名的展示期本地化。
 *
 * 回归:源阶段与其 git_source 任务统一叫「流水线源」,不再用某个具体托管平台名;
 * 用户自定义名一律原样透传。vitest 全局把 locale 钉在 zh-CN(src/test/setup.ts)。
 */
import { describe, it, expect } from 'vitest'
import { localizeName } from './pipelineLabels'

describe('localizeName', () => {
  it('本地化「流水线源」(源阶段与源任务同名共用)', () => {
    expect(localizeName('流水线源')).toBe('流水线源')
  })

  it('本地化其余后端默认阶段名', () => {
    expect(localizeName('构建')).toBe('构建')
    expect(localizeName('部署')).toBe('部署')
    expect(localizeName('通知')).toBe('通知')
  })

  it('用户自定义名原样透传', () => {
    expect(localizeName('我的构建')).toBe('我的构建')
    expect(localizeName('打镜像')).toBe('打镜像')
  })

  it('空/空白名原样返回', () => {
    expect(localizeName('')).toBe('')
    expect(localizeName('   ')).toBe('   ')
  })
})
