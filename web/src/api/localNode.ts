/**
 * Local-node roles API client.
 *
 * 本机(控制节点)角色(specs/local-node-roles.md):控制机可选启用为构建机 / 部署目标 /
 * 网关主机。GET/PUT /api/local-node(admin;PUT 需 CSRF)。
 *   - 默认全关;关闭态各消费方(构建机池 / 部署目标 / 网关主机)行为与未启用时完全一致。
 *   - buildLabels 与登记服务器同一套标签语法(逗号分隔 tag / k=v);空 = 不参与标签圈选
 *     (仍可被 `server:__local__` 钉死)。maxBuilds 0 = 调度器全局默认。
 *   - platformSupportsDeployGateway 是能力位:非 Linux 控制机为 false,deploy/gateway
 *     开关应禁用(保存侧服务端仍强校验,双保险)。
 */
import { http } from './http'

export interface LocalNodeRoles {
  buildEnabled: boolean
  buildLabels: string
  buildMaxBuilds: number
  buildPriority: number
  deployEnabled: boolean
  gatewayEnabled: boolean
  /** 非 Linux 控制机为 false:部署目标/网关主机角色依赖 POSIX 脚本(sh -c / tar / curl…)。 */
  platformSupportsDeployGateway: boolean
}

export const getLocalNode = (): Promise<LocalNodeRoles> =>
  http.get('/api/local-node')

export interface SaveLocalNodeInput {
  buildEnabled?: boolean
  buildLabels?: string
  buildMaxBuilds?: number
  buildPriority?: number
  deployEnabled?: boolean
  gatewayEnabled?: boolean
}

export const saveLocalNode = (input: SaveLocalNodeInput): Promise<LocalNodeRoles> => {
  const body: Record<string, unknown> = {}
  if (input.buildEnabled !== undefined) body.buildEnabled = input.buildEnabled
  if (input.buildLabels !== undefined) body.buildLabels = input.buildLabels
  if (input.buildMaxBuilds !== undefined) body.buildMaxBuilds = input.buildMaxBuilds
  if (input.buildPriority !== undefined) body.buildPriority = input.buildPriority
  if (input.deployEnabled !== undefined) body.deployEnabled = input.deployEnabled
  if (input.gatewayEnabled !== undefined) body.gatewayEnabled = input.gatewayEnabled
  return http.put('/api/local-node', body)
}
