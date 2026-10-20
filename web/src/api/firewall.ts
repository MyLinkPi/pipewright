/**
 * Firewall guard API — mesh 网段防火墙限制(两步生效)。
 *
 *   GET  /api/firewall/status  → FirewallStatus(草稿 + 阶段 + 快照 + 探测 + 确认门预判)
 *   PUT  /api/firewall/settings → { ports, extraCidrs }            (CSRF, 仅存草稿)
 *   POST /api/firewall/apply    → FirewallStatus(CSRF, 草稿→运行时规则,临时生效)
 *   POST /api/firewall/confirm  → FirewallStatus(CSRF, 真实来源门校验后固化)
 *   POST /api/firewall/rollback → FirewallStatus(CSRF, 立即回退临时规则)
 *   POST /api/firewall/disable  → FirewallStatus(CSRF, 拆除全部并复位未开启)
 */

import { http } from "./http";

export type FirewallPhase = "off" | "pending" | "active";

export interface FirewallSnapshot {
  ports: number[];
  cidrs: string[];
  appliedAt: string;
}

export interface FirewallDetect {
  supported: boolean;
  reason: string;
  linux: boolean;
  inContainer: boolean;
  iptables: boolean;
  ip6tables: boolean;
  dockerUser: boolean;
  isRoot: boolean;
  sudoOk: boolean;
  notes: string[];
}

export interface FirewallStatus {
  /** Draft ports for the next apply (comma-edited in UI, stored as list). */
  ports: number[];
  /** Draft extra allow CIDRs (empty = local + auto mesh /24 only). */
  extraCidrs: string[];
  phase: FirewallPhase;
  pending: FirewallSnapshot;
  confirmed: FirewallSnapshot;
  lastResult: "" | "ok" | "failed" | "reverted";
  statusDetail: string;
  lastAppliedAt: string;
  /** Seconds left in the pending confirmation window (0 outside pending). */
  pendingExpiresInSec: number;
  detect: FirewallDetect;
  /** Real source IP of *this* request (proxy-trust resolved server-side). */
  sourceIp: string;
  /** Whether sourceIp can pass the confirm gate right now (pending only). */
  confirmAllowed: boolean;
}

export function getFirewallStatus(): Promise<FirewallStatus> {
  return http.get("/api/firewall/status");
}

/** Save the draft (ports / extra CIDRs). Never touches the machine. */
export function saveFirewallDraft(body: {
  ports?: number[];
  extraCidrs?: string[];
}): Promise<{ ports: number[]; extraCidrs: string[] }> {
  return http.put("/api/firewall/settings", body);
}

/** Apply the draft to runtime iptables (temporary until confirmed; reboots revert). */
export function applyFirewall(): Promise<FirewallStatus> {
  return http.post("/api/firewall/apply");
}

/**
 * Solidify the pending rules. Server-side gate: the request's real source IP
 * (proxy-trust resolved) must be local or inside the mesh allow-set.
 */
export function confirmFirewall(): Promise<FirewallStatus> {
  return http.post("/api/firewall/confirm");
}

/** Roll back the pending rules immediately (escape hatch; always allowed). */
export function rollbackFirewall(): Promise<FirewallStatus> {
  return http.post("/api/firewall/rollback");
}

/** Tear down all guard rules and reset to off (escape hatch; always allowed). */
export function disableFirewall(): Promise<FirewallStatus> {
  return http.post("/api/firewall/disable");
}
