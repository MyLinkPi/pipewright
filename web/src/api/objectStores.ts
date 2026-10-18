/**
 * Object-storage API client（设置 · 对象存储 + 顶级浏览页）。
 *
 * 一个 `ObjectStore` 是一份对象存储**账户**配置（阿里云 OSS / S3 兼容的 RustFS），供流水线
 * `oss_upload` 节点把构建产物推送到桶。凭据按机密性拆分：
 *  - `accessKeyId` 非机密 —— 明文存储、可回显 / 可编辑；
 *  - AccessKeySecret **只写** —— 提交一次（可轮换），经后端保险库加密存储，**绝不回显**；
 *    列表只以 `credentialConfigured` 告知「是否已绑凭据」。
 *
 * GET    /api/object-stores                      → { items: ObjectStore[] }
 * POST   /api/object-stores                      → ObjectStore           (needs CSRF)
 * PUT    /api/object-stores/{id}                 → ObjectStore           (needs CSRF)
 * DELETE /api/object-stores/{id}                 → { ok: true }          (needs CSRF)
 * POST   /api/object-stores/{id}/test            → TestObjectStoreResult (needs CSRF)
 * GET    /api/object-stores/{id}/objects         → BrowseObjectStoreResult（marker 游标单页浏览）
 *
 * 写操作走共享 `http` 封装（自动 CSRF + locale）。绝不返回 Secret。
 */

import { http } from './http'

/** 受支持的对象存储后端（阿里云 OSS / S3 兼容的 RustFS）。 */
export type ObjectStoreType = 'aliyun_oss' | 'rustfs'

/**
 * 一份对象存储账户配置。Secret 永不返回；`accessKeyId` 非机密，明文回显。
 */
export interface ObjectStore {
  id: string
  type: ObjectStoreType
  /** 人读展示名，如「生产 OSS」。 */
  name: string
  /** 例如 `oss-cn-hangzhou.aliyuncs.com`。 */
  endpoint: string
  /** 默认桶名。 */
  bucket: string
  /** 可选：所有上传对象键统一附加的路径前缀（存储级默认；已归一化，无首尾斜杠）。 */
  pathPrefix: string
  /** 非机密的凭据半段（阿里云 AccessKeyId / RustFS Access Key）。 */
  accessKeyId: string
  /** 服务端派生：保险库中是否已存 AccessKeySecret。永不返回 Secret 本身。 */
  credentialConfigured: boolean
  createdAt: string
}

/** 创建对象的请求体。`secret` 只写 —— 提交一次，绝不回显。 */
export interface CreateObjectStoreInput {
  type: ObjectStoreType
  name: string
  endpoint: string
  bucket: string
  /** 可选：对象键路径前缀（服务端归一化，去首尾斜杠）。 */
  pathPrefix: string
  accessKeyId: string
  /** AccessKeySecret（只写）。经保险库存储，永不返回。 */
  secret: string
}

/** 编辑对象的请求体。缺省字段保持不变；`secret` 非空即轮换。 */
export interface UpdateObjectStoreInput {
  name?: string
  endpoint?: string
  bucket?: string
  /** 可选：路径前缀；传空串 = 清空前缀。 */
  pathPrefix?: string
  accessKeyId?: string
  /** 可选的 AccessKeySecret 轮换（只写）。 */
  secret?: string
}

/** 「测试连接」结果：连通性 + 桶可访问性探测。 */
export interface TestObjectStoreResult {
  ok: boolean
  latencyMs: number
  /** 人读详情（成功 = 桶名等；失败 = 原因）。 */
  detail: string
  /** 失败原因（成功为 null）。绝不含 Secret。 */
  error: string | null
}

/** 列出全部对象存储配置（永不包含 Secret）。 */
export async function listObjectStores(): Promise<ObjectStore[]> {
  const res = await http.get<{ items: ObjectStore[] }>('/api/object-stores')
  return res.items ?? []
}

/** 新增一条对象存储配置（AccessKeySecret 经保险库只写存储）。返回创建的 DTO。 */
export async function createObjectStore(input: CreateObjectStoreInput): Promise<ObjectStore> {
  return http.post<ObjectStore>('/api/object-stores', input)
}

/** 编辑配置（名称 / Endpoint / Bucket / AccessKeyId；可选轮换 Secret，不重建）。 */
export async function updateObjectStore(
  id: string,
  input: UpdateObjectStoreInput,
): Promise<ObjectStore> {
  return http.put<ObjectStore>(`/api/object-stores/${encodeURIComponent(id)}`, input)
}

/** 删除配置（连带删除其保险库凭据）。已引用它的流水线节点运行时会报「找不到对象存储」。 */
export async function deleteObjectStore(id: string): Promise<{ ok: boolean }> {
  return http.delete<{ ok: boolean }>(`/api/object-stores/${encodeURIComponent(id)}`)
}

/** 测试配置的凭据能否访问目标桶。返回耗时 / 详情 / 人读错误。 */
export async function testObjectStore(id: string): Promise<TestObjectStoreResult> {
  return http.post<TestObjectStoreResult>(`/api/object-stores/${encodeURIComponent(id)}/test`, {})
}

/** 浏览结果里的一个对象条目（`key` 为桶内完整键；展示时由页面按当前目录截断）。 */
export interface ObjectStoreEntry {
  key: string
  size: number
  lastModified: string
}

/**
 * 浏览一个目录的单页结果。桶内对象可能巨多：marker 游标分页（S3/OSS V1 语义），
 * `nextMarker` 为空即最后一页；`prefixes` 为子目录（以 `/` 结尾，服务端 delimiter 折叠）。
 */
export interface BrowseObjectStoreResult {
  prefixes: string[]
  objects: ObjectStoreEntry[]
  nextMarker: string
  truncated: boolean
}

/** 浏览入参：`prefix` 相对配置级 pathPrefix（空 = 前缀根）；`marker` 为空 = 第一页。 */
export interface BrowseObjectStoreParams {
  prefix?: string
  marker?: string
  maxKeys?: number
}

/** 单页浏览指定存储的目录（只读；绝不聚合全量，永远一页）。 */
export async function browseObjectStore(
  id: string,
  params: BrowseObjectStoreParams = {},
): Promise<BrowseObjectStoreResult> {
  const qs = new URLSearchParams()
  if (params.prefix !== undefined) qs.set('prefix', params.prefix)
  if (params.marker !== undefined) qs.set('marker', params.marker)
  if (params.maxKeys !== undefined) qs.set('maxKeys', String(params.maxKeys))
  const query = qs.toString()
  return http.get<BrowseObjectStoreResult>(
    `/api/object-stores/${encodeURIComponent(id)}/objects${query ? `?${query}` : ''}`,
  )
}
