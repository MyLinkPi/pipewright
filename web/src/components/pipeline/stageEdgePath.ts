/**
 * stageEdgePath — 阶段间依赖连线的 SVG 路径(纯几何,便于单测)。
 *
 * 相邻跳(中间无其他阶段列):浅 S 曲线,贴标题行水平连过列间空隙。
 * 跨级跳(中间隔着其他阶段列):下潜绕行 —— 控制点垂住向下,曲线在出发/到达的
 * 列间空隙里下潜、贴着「标题行下缘 → 依赖/条件 chips 上缘」之间的空带横穿中间列,
 * 再回升进入目标,避免连线从中间阶段的标题文字/按钮上碾过去(虚线在半透明按钮
 * 底下时隐时现,观感即「线不正常」)。
 */

/** 连线锚点在阶段列内的纵向偏移(标题行垂直中心)。 */
export const HEADER_Y = 13

/**
 * 跨级跳下潜深度上下限。曲线最低点 ≈ 锚点 y + 0.75 × dip:
 * 标题行内容下缘约 22px、chips 上缘约 36px,故 dip ∈ [18, 26] 使 apex 落在 (25, 33)。
 */
const SKIP_DIP_MIN = 18
const SKIP_DIP_MAX = 26

export function stageEdgePath(
  x1: number,
  y1: number,
  x2: number,
  y2: number,
  /** 两端横向区间内是否隔着其他阶段列(跨级跳)。 */
  hopsOver: boolean,
): string {
  const dx = x2 - x1
  if (!hopsOver) {
    const cdx = Math.max(26, Math.abs(dx) * 0.5)
    return `M${x1},${y1} C${x1 + cdx},${y1} ${x2 - cdx},${y2} ${x2},${y2}`
  }
  const dip = Math.min(SKIP_DIP_MAX, Math.max(SKIP_DIP_MIN, Math.abs(dx) * 0.07))
  const cdx = Math.min(Math.abs(dx) * 0.18, 44)
  const sx = dx < 0 ? -1 : 1
  return `M${x1},${y1} C${x1 + sx * cdx},${y1 + dip} ${x2 - sx * cdx},${y2 + dip} ${x2},${y2}`
}
