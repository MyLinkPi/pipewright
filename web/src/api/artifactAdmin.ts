/**
 * 产物管理 API client(顶级「产物」页:跨运行产物列表 + 查看/下载/删除/跨环境传输)。
 *
 * 文件类产物(jar/dist/archive 且 metadata.stored=true)走既有制品库下载端点;
 * 镜像类产物走服务端导出(docker pull + docker save 流式 .tar,大镜像耗时);
 * 删除会联动清理内置 registry tag(镜像类)与已无引用的制品库 blob(服务端处理 + 审计);
 * 跨环境传输复用 mesh_push 推送通道(异步任务,对端自动建「被推送源」项目落修订)。
 *
 * GET    /api/artifacts?page&pageSize&projectId&type          → ArtifactListResult
 * DELETE /api/runs/{id}/artifacts/{artifactId}                → ArtifactDeleteResult (needs CSRF)
 * POST   /api/runs/{id}/artifacts/transfer                    → { transferId } (needs CSRF)
 * GET    /api/artifacts/transfers/{transferId}                → ArtifactTransferStatus
 * DELETE /api/artifacts/transfers/{transferId}                → { ok } (needs CSRF)
 * (下载 URL 由 runArtifactDownloadUrl / runArtifactImageArchiveUrl 拼出,浏览器直连)
 */

import { http } from './http'

/** 产物类型(冻结枚举,与后端 run.Artifact* 一致)。 */
export type ArtifactType = 'image' | 'jar' | 'dist' | 'archive'

/** 产物列表行 = 冻结 artifactDTO 字段 + 关联运行/项目信息(管理面 additive)。 */
export interface ArtifactRowDTO {
  id: string
  type: ArtifactType
  name: string
  reference: string
  sizeBytes: number
  metadata: Record<string, unknown>
  createdAt: string
  runId: string
  projectId: string
  projectName: string
  runStatus: string
}

export interface ArtifactListResult {
  items: ArtifactRowDTO[]
  page: number
  total: number
}

export interface ArtifactDeleteResult {
  deleted: boolean
  registryTagDeleted: boolean
  blobsRemoved: number
}

export interface ListArtifactsParams {
  page?: number
  pageSize?: number
  projectId?: string
  type?: ArtifactType
}

/** 跨运行产物列表(管理面)。 */
export async function listArtifacts(params: ListArtifactsParams = {}): Promise<ArtifactListResult> {
  const qs = new URLSearchParams()
  if (params.page != null) qs.set('page', String(params.page))
  if (params.pageSize != null) qs.set('pageSize', String(params.pageSize))
  if (params.projectId) qs.set('projectId', params.projectId)
  if (params.type) qs.set('type', params.type)
  const suffix = qs.toString() ? `?${qs.toString()}` : ''
  return http.get<ArtifactListResult>(`/api/artifacts${suffix}`)
}

/** 删除一条产物记录(镜像类联动删内置 registry tag;已无引用 blob 服务端回收)。 */
export async function deleteRunArtifact(runId: string, artifactId: string): Promise<ArtifactDeleteResult> {
  return http.delete<ArtifactDeleteResult>(
    `/api/runs/${encodeURIComponent(runId)}/artifacts/${encodeURIComponent(artifactId)}`,
  )
}

/** 文件类产物的制品库下载 URL(metadata.stored=true 时可用;浏览器直连同源 cookie)。 */
export function runArtifactDownloadUrl(runId: string, artifactId: string): string {
  return `/api/runs/${encodeURIComponent(runId)}/artifacts/${encodeURIComponent(artifactId)}/download`
}

/** 镜像类产物的服务端 tar 导出 URL(docker pull + save;大镜像导出较慢)。 */
export function runArtifactImageArchiveUrl(runId: string, artifactId: string): string {
  return `/api/runs/${encodeURIComponent(runId)}/artifacts/${encodeURIComponent(artifactId)}/image-archive`
}

// ─── 跨环境传输(mesh_push 推送通道的手动触发) ─────────────────────────────

/** 传输任务状态机(running → success/failed/canceled)。 */
export type ArtifactTransferState = 'running' | 'success' | 'failed' | 'canceled'

/** 传输成功后的对端落点(projectUrl 为对端人用深链)。 */
export interface ArtifactTransferResult {
  projectId: string
  revisionId: string
  created: boolean
  projectUrl: string
}

/** 传输任务快照(轮询;phase 为服务端人读阶段文本)。 */
export interface ArtifactTransferStatus {
  id: string
  runId: string
  projectName: string
  peerNodeId: string
  peerName: string
  items: string[]
  status: ArtifactTransferState
  phase: string
  error?: string
  result?: ArtifactTransferResult
  createdAt: string
  updatedAt: string
}

/** 发起传输(异步任务):同 run 的多个产物一次传输 = 对方项目下的一个修订。 */
export function transferRunArtifacts(
  runId: string,
  body: { nodeId: string; artifactIds: string[]; revisionLabel?: string },
): Promise<{ transferId: string }> {
  return http.post(`/api/runs/${encodeURIComponent(runId)}/artifacts/transfer`, body)
}

/** 查询传输任务状态(服务重启后会话丢失 → 404,可重新发起)。 */
export function getArtifactTransfer(id: string): Promise<ArtifactTransferStatus> {
  return http.get(`/api/artifacts/transfers/${encodeURIComponent(id)}`)
}

/** 取消进行中的传输(已终态 → 409)。 */
export function cancelArtifactTransfer(id: string): Promise<{ ok: boolean }> {
  return http.delete(`/api/artifacts/transfers/${encodeURIComponent(id)}`)
}
