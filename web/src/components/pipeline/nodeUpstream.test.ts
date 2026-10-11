import { describe, it, expect } from 'vitest'
import type { PipelineJob, PipelineStage } from '../../api/pipeline'
import { upstreamNodeOptions } from './nodeUpstream'

function job(id: string, needs: string[] = [], type = 'script'): PipelineJob {
  return { id, name: id.toUpperCase(), type, summary: '', config: {}, needs }
}

function stage(id: string, jobs: PipelineJob[] = [], needs: string[] = []): PipelineStage {
  return { id, name: id.toUpperCase(), kind: 'custom', needs, jobs }
}

describe('nodeUpstream.upstreamNodeOptions', () => {
  it('linear fallback (no stage needs): earlier stages are upstream', () => {
    const stages = [
      stage('src', [job('pull', [], 'git_source')]),
      stage('build', [job('compile')]),
      stage('notify', [job('notify-job', [], 'notify')]),
    ]
    const names = upstreamNodeOptions(stages, 'notify-job').map((o) => o.name)
    // 声明序在前的两个阶段的全部节点,按声明序;不含自己。
    expect(names).toEqual(['PULL', 'COMPILE'])
  })

  it('explicit stage needs: only the needs closure is upstream (deep chains included)', () => {
    const stages = [
      stage('src', [job('pull', [], 'git_source')]),
      stage('build', [job('compile')], ['src']),
      stage('notify', [job('notify-job', [], 'notify')], ['build']),
    ]
    const names = upstreamNodeOptions(stages, 'notify-job').map((o) => o.name)
    expect(names).toEqual(['PULL', 'COMPILE']) // build + 传递到 src
  })

  it('explicit stage needs: parallel stage is not upstream', () => {
    const stages = [
      stage('src', [job('pull', [], 'git_source')]),
      stage('build', [job('compile')], ['src']),
      stage('audit', [job('scan')]), // 无 needs → 与 notify 并行
      stage('notify', [job('notify-job', [], 'notify')], ['src']),
    ]
    const names = upstreamNodeOptions(stages, 'notify-job').map((o) => o.name)
    expect(names).toEqual(['PULL']) // scan 不在上游
  })

  it('same stage: job needs closure only (flat siblings run in parallel)', () => {
    const stages = [
      stage('src', [job('pull', [], 'git_source')]),
      stage('build', [
        job('compile'),
        job('test'),
        job('pkg', ['compile'], 'build_image'),
        job('notify-job', ['pkg'], 'notify'),
      ], ['src']),
    ]
    const names = upstreamNodeOptions(stages, 'notify-job').map((o) => o.name)
    // 同阶段只含 job needs 闭包(pkg → compile);test 是并行邻居,不在上游。
    expect(names).toEqual(['PULL', 'COMPILE', 'PKG'])
  })

  it('returns [] for unknown job, and reports stageName for display', () => {
    const stages = [stage('src', [job('pull', [], 'git_source')]), stage('notify', [job('n', [], 'notify')])]
    expect(upstreamNodeOptions(stages, 'ghost')).toEqual([])
    const opt = upstreamNodeOptions(stages, 'n')[0]
    expect(opt).toEqual({ name: 'PULL', type: 'git_source', stageName: 'SRC' })
  })

  it('cyclic needs cannot loop forever (defensive guard)', () => {
    // 保存期不会出现环;防御性用例:环里也不能死循环。
    const stages = [
      stage('a', [job('x', [], 'git_source')]),
      stage('b', [job('y', ['z'])], ['c']),
      stage('c', [job('z', ['y'])], ['b']),
    ]
    expect(() => upstreamNodeOptions(stages, 'y')).not.toThrow()
  })
})