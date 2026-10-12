import { describe, it, expect } from 'vitest'
import { stageEdgePath } from './stageEdgePath'

/** 解析 `M.. C..` 路径为 [起点, CP1, CP2, 终点]。 */
function points(d: string): Array<{ x: number; y: number }> {
  const nums = d.match(/-?\d+(?:\.\d+)?/g)?.map(Number) ?? []
  expect(nums).toHaveLength(8)
  return [0, 2, 4, 6].map((i) => ({ x: nums[i], y: nums[i + 1] }))
}

describe('stageEdgePath', () => {
  it('相邻跳:控制点与端点同高(贴标题行的浅 S 曲线)', () => {
    const [, cp1, cp2, end] = points(stageEdgePath(248, 13, 308, 13, false))
    expect(cp1.y).toBe(13)
    expect(cp2.y).toBe(13)
    expect(end).toEqual({ x: 308, y: 13 })
  })

  it('跨级跳:控制点垂住向下,最低点落在标题带与 chips 之间的空带里', () => {
    // 与截屏相同的场景:流水源 → 构建镜像,中间隔着自检列
    const d = stageEdgePath(248, 13, 616, 13, true)
    const [, cp1, cp2] = points(d)
    for (const cp of [cp1, cp2]) {
      expect(cp.y).toBeGreaterThan(13) // 向下潜
      expect(cp.y).toBeLessThanOrEqual(13 + 26) // dip 上限
    }
    // apex ≈ y + 0.75 × dip:必须低于标题行下缘(~22)且不触到 chips 上缘(~36)
    const apex = 13 + 0.75 * (cp1.y - 13)
    expect(apex).toBeGreaterThan(22)
    expect(apex).toBeLessThan(36)
  })

  it('跨级跳:超长距离时下潜封顶,不会一路栽进卡片里', () => {
    const [, cp1] = points(stageEdgePath(248, 13, 1800, 13, true))
    expect(cp1.y).toBe(13 + 26)
  })

  it('跨级跳:反向依赖(上游在右侧)控制点朝左下,仍在两端之间', () => {
    const [start, cp1, cp2, end] = points(stageEdgePath(900, 13, 300, 13, true))
    expect(start).toEqual({ x: 900, y: 13 })
    expect(end).toEqual({ x: 300, y: 13 })
    expect(cp1.x).toBeLessThan(900)
    expect(cp1.y).toBeGreaterThan(13)
    expect(cp2.x).toBeGreaterThan(300)
    expect(cp2.y).toBeGreaterThan(13)
  })

  it('两端不同高(chips 撑高列)时终点锚在各自标题中心', () => {
    const [, cp1, cp2, end] = points(stageEdgePath(248, 13, 616, 21, true))
    expect(cp1.y).toBeGreaterThan(13)
    expect(cp2.y).toBe(21 + (cp1.y - 13)) // 同一 dip
    expect(end).toEqual({ x: 616, y: 21 })
  })
})
