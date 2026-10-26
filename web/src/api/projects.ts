/**
 * Projects API — aligns to frozen 2.1 contract.
 *
 * GET    /api/projects                  → Project[]
 * POST   /api/projects                  → Project   (needs CSRF)
 * POST   /api/projects/test-clone       → TestCloneResult (needs CSRF)
 * PATCH  /api/projects/{id}             → Project   (needs CSRF)
 * DELETE /api/projects/{id}             → 204       (needs CSRF)
 *
 * credentialName is read-only display; credentialId is the reference.
 * lastRunStatus is the project's most recent run status (same vocabulary as
 * /api/runs) or null when the project has no runs yet.
 * serviceRegs lists the project's registered services (card display subset), resolved
 * server-side from the pipeline's deploy-node bindings (regServiceId / legacy
 * gatewayService by name); [] when nothing is bound.
 * The API never returns credential plaintext — only credentialName (masked display).
 */

import { http } from './http'

export type RunStatus =
  | 'queued'
  | 'running'
  | 'waiting_approval'
  | 'success'
  | 'failed'
  | 'rejected'
  | 'partial_failed'
  | 'rolled_back'

/** A service the project has registered in the service-registry gateway (card display subset). */
export interface ProjectServiceReg {
  id: string
  name: string
  /** HTTP service access domain (e.g. shop.example.com); meaningless for tcp. */
  fqdn: string
  protocol: 'http' | 'tcp'
  /** Gateway listen port for tcp services (always 0 for http). */
  tcpListenPort: number
}

export interface Project {
  id: string
  name: string
  repoUrl: string
  /** Source kind: 'git' (default) | 'pushed' (cluster-pushed artifacts/images; auto-created on first push, no repo). Optional: older fixtures/tests may omit. */
  sourceType?: 'git' | 'pushed'
  /** Pushed-source provenance: originating cluster node id + its project id (empty for git projects). */
  originNodeId?: string
  originProjectRef?: string
  defaultBranch: string
  credentialId: string
  /** Masked display name from the vault — never plaintext. */
  credentialName: string
  /**
   * Pipeline-as-code (GitOps) toggle. When true, each run reads `.pipewright.yml`
   * from the run's branch in the repo and uses it to drive the run (falling back
   * to the stored UI pipeline if the file is missing or invalid). FR-8-12.
   */
  pacEnabled: boolean
  /**
   * Pipeline-as-code file path (empty = default `.pipewright.yml`). Runtime,
   * preview and AI-ops all read this repo-relative path instead of the hardcoded
   * default. Validated server-side (relative path, no `..` segments).
   */
  pacFile?: string
  /**
   * PR status checks toggle (Story 8-9 / FR-8-9). When true, on a run reaching a
   * terminal status Pipewright detects the repo platform (GitHub/Gitee) and writes
   * back the commit status (PR check) using the project credential. Best-effort.
   */
  prStatusEnabled: boolean
  /** null until the project has a run. */
  lastRunStatus: RunStatus | null
  /** RFC3339 created_at of the most recent run; null until the project has a run. */
  lastRunAt: string | null
  /**
   * Services registered in the service-registry gateway, resolved from the pipeline's
   * deploy-node bindings (regServiceId; legacy gatewayService by service name).
   * Empty until a deploy node binds a service — the project card lists every entry.
   */
  serviceRegs: ProjectServiceReg[]
  createdAt: string
  updatedAt: string
}

export interface CreateProjectInput {
  name: string
  repoUrl: string
  credentialId: string
  /** Optional — server auto-detects from ls-remote HEAD if omitted. */
  defaultBranch?: string
}

export interface UpdateProjectInput {
  name?: string
  defaultBranch?: string
  credentialId?: string
  /** Toggle pipeline-as-code (GitOps) for this project. */
  pacEnabled?: boolean
  /** Set the pipeline-as-code file path (empty string resets to the default file). */
  pacFile?: string
  /** Toggle PR status checks (commit status writeback) for this project. */
  prStatusEnabled?: boolean
}

export interface TestCloneResult {
  ok: true
  defaultBranch: string
}

export interface TestCloneInput {
  repoUrl: string
  credentialId: string
}

export async function listProjects(): Promise<Project[]> {
  return http.get<Project[]>('/api/projects')
}

export async function createProject(input: CreateProjectInput): Promise<Project> {
  return http.post<Project>('/api/projects', input)
}

export async function testClone(input: TestCloneInput): Promise<TestCloneResult> {
  return http.post<TestCloneResult>('/api/projects/test-clone', input)
}

export async function updateProject(id: string, input: UpdateProjectInput): Promise<Project> {
  return http.patch<Project>(`/api/projects/${id}`, input)
}

export async function deleteProject(id: string): Promise<void> {
  return http.delete<void>(`/api/projects/${id}`)
}

/** One stage's summary from a previewed `.pipewright.yml` (no secrets). */
export interface PacStageSummary {
  name: string
  kind: string
  jobCount: number
}

/** One variable declared in the repo YAML top-level `variables:` block (metadata only, never a value). */
export interface PacVarDefSummary {
  name: string
  description: string
  required: boolean
  secret: boolean
  default: string
}

/**
 * Result of previewing/validating the repo's pipeline file at a chosen ref.
 * - found=false: the file does not exist at that ref (or repo unreadable) — runs fall back to the UI pipeline.
 * - found=true, valid=false: the file exists but failed to parse/validate; `error` carries the
 *   server's human-readable, secret-free message; `stages` is empty.
 * - found=true, valid=true: `stages`/`stageCount` summarize what the runtime would use, and
 *   `variables` lists the YAML's variable declarations (also synced into pipeline settings).
 * - `file` is the effective file: the `file` query param override, else the project's pacFile, else the default.
 */
export interface PacPreviewResult {
  found: boolean
  valid: boolean
  ref: string
  file: string
  error: string
  stageCount: number
  stages: PacStageSummary[]
  variables: PacVarDefSummary[]
}

/**
 * Fetch & validate the repo's pipeline file at `ref` (defaults to the project's default
 * branch when omitted) and `file` (defaults to the project's pacFile, then `.pipewright.yml`).
 * Read-only; never returns secrets or the repo URL credentials. On success the declared
 * variables are synced into the project's pipeline settings (「变量与缓存」).
 */
export async function previewPacConfig(id: string, ref?: string, file?: string): Promise<PacPreviewResult> {
  const params: string[] = []
  if (ref?.trim()) params.push(`ref=${encodeURIComponent(ref.trim())}`)
  if (file?.trim()) params.push(`file=${encodeURIComponent(file.trim())}`)
  const qs = params.length ? `?${params.join('&')}` : ''
  return http.get<PacPreviewResult>(`/api/projects/${id}/pac/preview${qs}`)
}
