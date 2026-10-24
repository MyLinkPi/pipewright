/**
 * PM2 management API client.
 *
 * GET /api/servers/pm2       → { items: ServerPm2[] }  (batch aggregate; read-only)
 * GET /api/servers/:id/pm2   → ServerPm2               (single host; read-only)
 *
 * Processes are collected over SSH by running `pm2 jlist` on each registered
 * host — the same agentless design as container management (no daemon socket
 * exposed). Per-host fault isolation: an unreachable host or a host without
 * pm2 never 500s and never affects the others.
 *
 * 控制节点(本机):同一批端点也服务 serverId = `LOCAL_HOST_ID`(见 `api/servers.ts`)。
 * 后端把命令在**本机**执行(os/exec,不经 SSH),前端照常调用
 * `getServerPm2(LOCAL_HOST_ID)` 再把它置首成卡即可。
 *
 * Process LIFECYCLE (start/stop/restart/reload/delete) reuses the existing
 * `serviceAction(id, { type: 'pm2', ... })` endpoint in `servers.ts`. Process
 * logs reuse the server logs endpoint with `source: 'pm2'`.
 */

import { http } from './http'

/** pm2 process status as reported by `pm2 jlist` (normalised server-side). */
export type Pm2Status = 'online' | 'stopping' | 'stopped' | 'launching' | 'errored'

/** One pm2 process instance as reported by `pm2 jlist`. */
export interface Pm2Process {
  /** pm2 process id (cluster mode: same name, one id per instance). */
  id: number
  /** Process name (`pm2 start --name` / script basename). */
  name: string
  /** System PID; 0 when stopped. */
  pid: number
  status: Pm2Status
  /** Cumulative restart count. */
  restarts: number
  /** Epoch ms of the last start (pm_uptime); 0 when stopped. */
  uptime: number
  /** Sampled CPU percent (monit.cpu). */
  cpu: number
  /** Resident memory in bytes (monit.memory). */
  memory: number
  /** fork_mode / cluster_mode. */
  execMode: string
  /** Instance count (number, or `max`). */
  instances: string
  /** Interpreter, usually `node`. */
  interpreter: string
  /** Entry script as passed to pm2 start. */
  script: string
  /** Working directory (pm_cwd). */
  cwd: string
  /** Epoch ms when pm2 first took over the app. */
  createdAt: number
  /** stdout log file path. */
  outLog: string
  /** stderr log file path. */
  errLog: string
}

/** pm2 processes for one server, plus reachability + runtime detection. */
export interface ServerPm2 {
  serverId: string
  /** False when the host is unreachable / auth failed. */
  reachable: boolean
  /** Detected runtime (`pm2`) or empty string when none was found. */
  runtime: string
  /** Human-readable error when reachable is false or no runtime was found. Never a secret. */
  error: string
  processes: Pm2Process[]
  /** Count of online (+ launching) processes. */
  online: number
  /** Total processes (all statuses). */
  total: number
  collectedAt: string
}

/** Fetch the pm2 inventory for every registered server (parallel, fault-isolated). */
export async function getAllPm2(): Promise<ServerPm2[]> {
  const res = await http.get<{ items: ServerPm2[] }>('/api/servers/pm2')
  return res.items ?? []
}

/** Fetch the pm2 inventory for a single server (supports `LOCAL_HOST_ID`). */
export async function getServerPm2(id: string): Promise<ServerPm2> {
  return http.get<ServerPm2>(`/api/servers/${id}/pm2`)
}
