/**
 * Run-data retention policy API.
 *
 * GET /api/retention/config → RetentionConfig
 * PUT /api/retention/config → RetentionConfig   (needs CSRF)
 *
 * Global policy: when enabled, a background sweeper periodically prunes terminal
 * runs (and their logs/steps/artifacts) that exceed keepPerProject or maxAgeDays.
 * In-progress runs are never pruned. 0 = unlimited for either limit.
 */

import { http } from './http'

export interface RetentionConfig {
  enabled: boolean
  /** Keep the most recent N terminal runs per project (0 = unlimited). */
  keepPerProject: number
  /** Delete terminal runs older than N days (0 = no age limit). */
  maxAgeDays: number
  /** Build-dependency cache total size cap in GB (LRU eviction; 0 = unlimited). Independent of enabled. */
  cacheMaxGb: number
  /** Local image tags kept per project (older pipewright/<slug>:<tag> rotated out; 0 = off). Independent of enabled. */
  imageKeepTags: number
}

export async function getRetentionConfig(): Promise<RetentionConfig> {
  return http.get<RetentionConfig>('/api/retention/config')
}

export async function setRetentionConfig(input: RetentionConfig): Promise<RetentionConfig> {
  return http.put<RetentionConfig>('/api/retention/config', input)
}
