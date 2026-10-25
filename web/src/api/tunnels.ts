/**
 * SSH tunnel management API client.
 *
 * GET    /api/tunnels               → { items: SshTunnel[] }   (config + runtime merged)
 * POST   /api/tunnels               → SshTunnel                (create; enabled ⇒ helper spawned)
 * PUT    /api/tunnels/:id           → SshTunnel                (full replace; config change restarts helper)
 * DELETE /api/tunnels/:id           → { ok: true }             (stop helper + delete row + cleanup files)
 * POST   /api/tunnels/:id/start     → SshTunnel                (enabled = true + spawn)
 * POST   /api/tunnels/:id/stop      → SshTunnel                (stop helper + enabled = false)
 * POST   /api/tunnels/:id/restart   → SshTunnel                (stop + spawn, enabled unchanged)
 * POST   /api/tunnels/:id/test      → { ok, latencyMs, error } (one-shot connectivity probe)
 * GET    /api/tunnels/:id/logs      → { lines: string[] }      (helper log tail; ?lines=N)
 *
 * Tunnels run in detached helper processes (same binary, `--tunnel-helper`):
 * restarting pipewright never kills them — the reconcile loop re-adopts live
 * helpers by heartbeat freshness. Runtime fields (status/pid/lastError/…) are
 * merged into the list response; no credential material ever leaves the vault.
 */

import { http } from './http'

/** Tunnel mode — the ssh flag it mirrors. `vpn` is ssh -w (see the not-recommended hint in the UI). */
export type TunnelMode = 'local' | 'remote' | 'dynamic' | 'vpn'

/** Merged runtime status (config.enabled + helper heartbeat). */
export type TunnelStatus = 'disabled' | 'stopped' | 'connecting' | 'active' | 'retrying'

/** One SSH tunnel: configuration + runtime snapshot (no secrets, ids only). */
export interface SshTunnel {
  id: string
  name: string
  /** Bound registered server (uses its credentials + jump chain); empty = direct mode. */
  serverId: string
  serverName: string
  /** Direct-mode fields (target host not in the servers table; no jump chain). */
  host: string
  port: number
  user: string
  credentialId: string
  credentialName: string
  mode: TunnelMode
  /** Listen address: local/dynamic = this machine; remote = the sshd side. */
  bindHost: string
  /** 0 = auto-assign; the actual port comes back in `actualPort`/`endpoint`. */
  bindPort: number
  /** Forward target (local = reachable from the remote host; remote = local side); empty for dynamic. */
  destHost: string
  destPort: number
  /** Doubles as autostart and desired state: false = stopped + disabled. */
  enabled: boolean
  createdAt: string
  updatedAt: string
  status: TunnelStatus
  pid: number
  lastError: string
  restarts: number
  actualPort: number
  endpoint: string
  startedAt: string
}

/** Create/update payload (full replace). */
export interface TunnelInput {
  name: string
  serverId: string
  host: string
  port: number
  user: string
  credentialId: string
  mode: TunnelMode
  bindHost: string
  bindPort: number
  destHost: string
  destPort: number
  enabled: boolean
}

/** One-shot connectivity test result. */
export interface TunnelTestResult {
  ok: boolean
  latencyMs: number
  /** Human-readable failure reason (never a secret); null on success. */
  error: string | null
}

/** List all tunnels with merged runtime status. */
export async function listTunnels(): Promise<SshTunnel[]> {
  const res = await http.get<{ items: SshTunnel[] }>('/api/tunnels')
  return res.items ?? []
}

/** Create a tunnel (enabled ⇒ the helper process is spawned immediately). */
export async function createTunnel(input: TunnelInput): Promise<SshTunnel> {
  return http.post<SshTunnel>('/api/tunnels', input)
}

/** Update a tunnel (full replace; a running helper restarts when config changed). */
export async function updateTunnel(id: string, input: TunnelInput): Promise<SshTunnel> {
  return http.put<SshTunnel>(`/api/tunnels/${id}`, input)
}

/** Delete a tunnel: stops the helper, removes the row and runtime files. */
export async function deleteTunnel(id: string): Promise<void> {
  await http.delete(`/api/tunnels/${id}`)
}

/** Enable + start a tunnel. */
export async function startTunnel(id: string): Promise<SshTunnel> {
  return http.post<SshTunnel>(`/api/tunnels/${id}/start`)
}

/** Stop the helper and disable the tunnel. */
export async function stopTunnel(id: string): Promise<SshTunnel> {
  return http.post<SshTunnel>(`/api/tunnels/${id}/stop`)
}

/** Restart the helper (config re-read on start). */
export async function restartTunnel(id: string): Promise<SshTunnel> {
  return http.post<SshTunnel>(`/api/tunnels/${id}/restart`)
}

/** One-shot connectivity test (dial; remote additionally probes the sshd listen). */
export async function testTunnel(id: string): Promise<TunnelTestResult> {
  return http.post<TunnelTestResult>(`/api/tunnels/${id}/test`)
}

/** Tail the helper log (last `lines` lines; 200 default, 1000 max server-side). */
export async function getTunnelLogs(id: string, lines = 200): Promise<string[]> {
  const res = await http.get<{ lines: string[] }>(`/api/tunnels/${id}/logs?lines=${lines}`)
  return res.lines ?? []
}
