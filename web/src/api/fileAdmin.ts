/**
 * 服务器文件查看 API client(顶级「文件」页:目录浏览 + 文件下载 + ≤64KiB 纯文本
 * 在线查看/编辑)。
 *
 * 后端在保存前自动 cp -a 同目录备份(<路径>.pipewright-bak.<UTC 时间戳>),响应
 * 带回备份路径;>64KiB 或二进制文件后端以 tooLarge/binary 标记返回(不报错),
 * 前端引导走下载。
 *
 * GET  /api/fileadmin/servers/{id}/files?path=…          → FileListing        (admin)
 * GET  /api/fileadmin/servers/{id}/file?path=…           → ServerFile         (admin)
 * GET  /api/fileadmin/servers/{id}/file/download?path=…  → octet-stream 流    (admin)
 * PUT  /api/fileadmin/servers/{id}/file                  → { ok, backup }     (admin, needs CSRF)
 */

import { http } from './http'

/** 目录条目(type: dir|file|symlink|other;目录在前、名称升序)。 */
export interface FileEntry {
  name: string
  path: string
  type: 'dir' | 'file' | 'symlink' | 'other'
  size: number
  mode: string
  owner: string
  group: string
  mtime: number
}

export interface FileListing {
  path: string
  entries: FileEntry[]
}

/** 小文本读取结果:binary/tooLarge 时不带 content,editable = 可在线编辑。 */
export interface ServerFile {
  path: string
  size: number
  binary: boolean
  tooLarge: boolean
  editable: boolean
  content: string
}

export interface SaveFileResult {
  ok: boolean
  backup: string
}

const base = (id: string): string => `/api/fileadmin/servers/${encodeURIComponent(id)}`

export async function listServerFiles(serverId: string, path: string): Promise<FileListing> {
  return http.get<FileListing>(`${base(serverId)}/files?${new URLSearchParams({ path })}`)
}

export async function readServerFile(serverId: string, path: string): Promise<ServerFile> {
  return http.get<ServerFile>(`${base(serverId)}/file?${new URLSearchParams({ path })}`)
}

export async function saveServerFile(serverId: string, path: string, content: string): Promise<SaveFileResult> {
  return http.put<SaveFileResult>(`${base(serverId)}/file`, { path, content })
}

/** 下载直连 URL(同源 cookie 自动带上;配合 <a :href download> 使用)。 */
export function serverFileDownloadUrl(serverId: string, path: string): string {
  return `${base(serverId)}/file/download?path=${encodeURIComponent(path)}`
}
