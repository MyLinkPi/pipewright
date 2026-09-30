/**
 * Build runner pool API — 构建机池选择器(FR-8-14 续 / FR-8-19 池化).
 *
 * GET /api/projects/{id}/runner → { runnerServerId, selector }
 * PUT /api/projects/{id}/runner → 同上(需 CSRF;空 selector = 清,回本地构建)。
 *
 * selector 为标签选择器(`linux,arch=arm64`,AND 语义)或钉死单机 `server:<id>`;
 * runnerServerId 为钉死形式的派生字段(旧客户端只传 runnerServerId 仍被服务端接受)。
 * 配置后该项目构建从打了标签的服务器池按「优先级 → 流水线亲和 → 负载」选机远程执行。
 */
import { http } from './http'

export interface RunnerConfig {
  runnerServerId: string
  selector: string
}

export async function getRunner(projectId: string): Promise<RunnerConfig> {
  return http.get<RunnerConfig>(`/api/projects/${projectId}/runner`)
}

export async function saveRunnerSelector(projectId: string, selector: string): Promise<RunnerConfig> {
  return http.put<RunnerConfig>(`/api/projects/${projectId}/runner`, { selector })
}
