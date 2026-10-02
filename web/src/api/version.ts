/**
 * Version API — 构建版本元数据 + 一键检查更新。
 *
 * GET /version                  → VersionInfo(公开,构建期注入的版本/commit/日期/部署形态)
 * GET /api/version/check        → UpdateInfo(鉴权;二进制/Docker 查升级源,源码部署查 git 上游)
 * POST /api/version/update      → UpdateResult(鉴权+CSRF;binary 自替换重启 / docker 给命令 /
 *                                 source 启动后台管线后立即返回)
 * GET /api/version/update/status → UpdateJobStatus(鉴权;source 后台管线的进度快照)
 *
 * 检查更新永不抛业务错:后端在 CheckError 字段里带失败原因(网络/限流),始终附当前版本,
 * 由 UI 优雅降级渲染。
 */

import { http } from './http'

export interface VersionInfo {
  version: string
  commit: string
  date: string
  goVersion: string
  platform: string
  /** 部署形态;缺省视为 binary(旧后端不返回该字段)。 */
  runtime?: 'binary' | 'docker' | 'source'
}

export interface UpdateInfo {
  current: string
  /** 二进制/Docker:最新 tag;源码部署:上游短 SHA。 */
  latest: string
  updateAvailable: boolean
  releaseUrl: string
  publishedAt: string
  notes: string
  /** 非空 ⇒ 本次检查失败(网络/限流);此时 updateAvailable 恒 false。 */
  checkError?: string
}

/** 一键自动更新结果。binary 成功后进程自重启;docker 返回升级命令;source 返回 building 后走后台管线。 */
export interface UpdateResult {
  mode: 'binary' | 'docker' | 'source' | ''
  status: 'restarting' | 'manual' | 'uptodate' | 'building' | 'busy' | 'error'
  from: string
  to: string
  message: string
  command?: string
}

/** 源码升级后台管线的进度快照(GET /api/version/update/status)。 */
export interface UpdateJobStatus {
  /** false = 非源码部署,无后台任务可轮询。 */
  supported: boolean
  running: boolean
  /** pull | build | install | restart */
  step: string
  message?: string
  /** 管线完成,已触发重启(服务重启 / re-exec)。 */
  done: boolean
  error?: string
  /** 最近输出(环形,新在后)。 */
  log?: string[]
}

export function getVersion(): Promise<VersionInfo> {
  return http.get<VersionInfo>('/version')
}

export function checkUpdate(): Promise<UpdateInfo> {
  return http.get<UpdateInfo>('/api/version/check')
}

/** 触发一键自动更新(写操作,带 CSRF)。 */
export function applyUpdate(): Promise<UpdateResult> {
  return http.post<UpdateResult>('/api/version/update')
}

/** 轮询源码升级后台管线的进度(鉴权只读)。 */
export function updateJobStatus(): Promise<UpdateJobStatus> {
  return http.get<UpdateJobStatus>('/api/version/update/status')
}
