/**
 * jobConfigSchema — declarative, per-type parameter forms for pipeline canvas nodes.
 *
 * The 2-2 pipeline contract freezes the node shape as `config: Record<string,string>`
 * (a flat string map). Rather than widen that contract, every known job *type* declares
 * a typed field schema here; JobDrawer renders those fields and reads/writes specific
 * keys in the same flat map. Anything not covered by a type's schema stays editable via
 * the drawer's "raw parameters" fallback, so custom/unknown types and power users lose
 * nothing. This is the Jenkins/云效-style "each step type has its own form" behaviour,
 * implemented without a backend DTO change.
 *
 * All values are strings (config is Record<string,string>). number/toggle fields are
 * serialized to/from strings at the field layer; multiline commands are stored as a
 * single newline-joined string.
 */

import type { CredentialType } from '../../api/credentials'
import { t } from '../../i18n'

// ─── Field model ──────────────────────────────────────────────────────────────

export type FieldKind =
  | 'text'
  | 'textarea'
  | 'select'
  | 'number'
  | 'toggle'
  | 'credential'
  | 'server'
  | 'channel'
  | 'labelSelector'
  | 'producer'
  | 'runnerPicker'

export interface SelectOption {
  value: string
  label: string
}

export interface JobField {
  /** Key written into job.config */
  key: string
  /** Human label (zh) */
  label: string
  kind: FieldKind
  placeholder?: string
  /** Helper text shown under the control */
  hint?: string
  /** Options for `select` */
  options?: SelectOption[]
  /** Restrict the credential picker to one credential type (or a set of them) */
  credentialType?: CredentialType | CredentialType[]
  /** Render with monospace font (paths, commands, image refs) */
  monospace?: boolean
  /** Conditional visibility based on the current config values */
  when?: (config: Record<string, string>) => boolean
  /** labelSelector / producer 控件的候选来源约束(如只列产镜像节点的类型集合) */
  producerKinds?: string[]
}

/** Accent palette keys — map to --color-{accent} / --color-{accent}-soft tokens. */
export type AccentName = 'cyan' | 'primary' | 'green' | 'amber' | 'red' | 'neutral'

/** Picker category id for grouping task types (Jenkins/云效-style gallery). */
export type CategoryId = 'source' | 'build' | 'deploy' | 'quality' | 'notify' | 'custom'

export interface JobTypeSpec {
  type: string
  /** Friendly zh label shown in the picker, drawer, and node card */
  label: string
  /** One-line description of what this node does (shown in the picker card) */
  description: string
  /** Accent colour for the type icon */
  accent: AccentName
  /** Category the type belongs to (picker grouping) */
  category: CategoryId
  fields: JobField[]
  /** 模板节点的预填配置(选中即带上,用户只改参数);普通节点省略。 */
  defaultConfig?: Record<string, string>
}

// ─── Shared option sets ─────────────────────────────────────────────────────────

const ARTIFACT_OPTIONS: SelectOption[] = [
  { value: 'image', get label() { return t('pipelineJob.artifactImage') } },
  { value: 'jar', get label() { return t('pipelineJob.artifactJar') } },
  { value: 'dist', get label() { return t('pipelineJob.artifactDist') } },
]

const BUILD_MODEL_OPTIONS: SelectOption[] = [
  { value: 'dockerfile', get label() { return t('pipelineJob.buildModelDockerfile') } },
  { value: 'toolchain', get label() { return t('pipelineJob.buildModelToolchain') } },
]

const TOOLCHAIN_OPTIONS: SelectOption[] = [
  { value: 'node', label: 'Node.js' },
  { value: 'java', label: 'Java / Maven' },
  { value: 'go', label: 'Go' },
  { value: 'python', label: 'Python' },
  { value: 'custom', get label() { return t('pipelineJob.toolchainCustom') } },
]

// 部署节点产物来源模式(deployMode):部署产物(默认)/ 仅执行重启命令(配置类流水线)。
const DEPLOY_MODE_OPTIONS: SelectOption[] = [
  { value: 'artifact', get label() { return t('pipelineJob.deployModeArtifact') } },
  { value: 'command', get label() { return t('pipelineJob.deployModeCommand') } },
]

// 标签匹配方式(selectorMode):满足全部条件(且,默认)/ 满足任一条件(或)。
const SELECTOR_MODE_OPTIONS: SelectOption[] = [
  { value: 'all', get label() { return t('pipelineJob.selectorModeAll') } },
  { value: 'any', get label() { return t('pipelineJob.selectorModeAny') } },
]

// 产物打包方式(artifactPackMode):目录打包 tar.gz(默认)| 不打包按文件清单。
const ARTIFACT_PACK_OPTIONS: SelectOption[] = [
  { value: 'tar', get label() { return t('pipelineJob.artifactPackTar') } },
  { value: 'none', get label() { return t('pipelineJob.artifactPackNone') } },
]

// tar 包内布局(tarLayout):内容铺包根(默认 = 旧行为)| 保留顶层目录。
const TAR_LAYOUT_OPTIONS: SelectOption[] = [
  { value: 'contents', get label() { return t('pipelineJob.tarLayoutContents') } },
  { value: 'top', get label() { return t('pipelineJob.tarLayoutTop') } },
]

const PROBE_MODE_OPTIONS: SelectOption[] = [
  { value: 'http', get label() { return t('pipelineJob.probeModeHttp') } },
  { value: 'command', get label() { return t('pipelineJob.probeModeCommand') } },
]

// `when` helpers
const modelIs = (v: string) => (c: Record<string, string>) =>
  (c.buildModel || 'dockerfile') === v
const probeIs = (v: string) => (c: Record<string, string>) =>
  (c.probeMode || 'http') === v

// ─── Per-type specs ──────────────────────────────────────────────────────────────

// 任务级执行选项(P0 引擎能力):超时 / 重试 / 资源规格。脚本类节点共用。
// 全部可选;留空 = 旧行为(不限超时、不重试、不限资源)。timeout/retry 为非负整数。
const EXEC_OPTION_FIELDS: JobField[] = [
  {
    key: 'timeoutSeconds',
    get label() { return t('pipelineJob.fieldTimeoutLabel') },
    kind: 'number',
    placeholder: '0',
    get hint() { return t('pipelineJob.fieldTimeoutHint') },
  },
  {
    key: 'retries',
    get label() { return t('pipelineJob.fieldRetriesLabel') },
    kind: 'number',
    placeholder: '0',
    get hint() { return t('pipelineJob.fieldRetriesHint') },
  },
  {
    key: 'cpu',
    get label() { return t('pipelineJob.fieldCpuLabel') },
    kind: 'text',
    monospace: true,
    placeholder: '1',
    get hint() { return t('pipelineJob.fieldCpuHint') },
  },
  {
    key: 'memory',
    get label() { return t('pipelineJob.fieldMemoryLabel') },
    kind: 'text',
    monospace: true,
    placeholder: '512m',
    get hint() { return t('pipelineJob.fieldMemoryHint') },
  },
]

const SCRIPT_FIELDS: JobField[] = [
  {
    key: 'image',
    get label() { return t('pipelineJob.fieldImageLabel') },
    kind: 'text',
    monospace: true,
    placeholder: 'node:20',
    get hint() { return t('pipelineJob.fieldImageHint') },
  },
  {
    key: 'commands',
    get label() { return t('pipelineJob.fieldCommandsLabel') },
    kind: 'textarea',
    monospace: true,
    placeholder: 'npm ci\nnpm run build',
    get hint() { return t('pipelineJob.fieldCommandsHint') },
  },
  {
    key: 'workDir',
    get label() { return t('pipelineJob.fieldWorkDirLabel') },
    kind: 'text',
    monospace: true,
    placeholder: '.',
    get hint() { return t('pipelineJob.fieldWorkDirHint') },
  },
  {
    key: 'artifactPath',
    get label() { return t('pipelineJob.fieldArtifactPathLabel') },
    kind: 'textarea',
    monospace: true,
    placeholder: 'frontend/dist\nbackend/target/app.jar',
    get hint() { return t('pipelineJob.fieldArtifactPathHint') },
  },
  {
    key: 'artifactPackMode',
    get label() { return t('pipelineJob.fieldArtifactPackModeLabel') },
    kind: 'select',
    options: ARTIFACT_PACK_OPTIONS,
    get hint() { return t('pipelineJob.fieldArtifactPackModeHint') },
  },
  {
    key: 'tarLayout',
    get label() { return t('pipelineJob.fieldTarLayoutLabel') },
    kind: 'select',
    options: TAR_LAYOUT_OPTIONS,
    get hint() { return t('pipelineJob.fieldTarLayoutHint') },
    when: (c) => (c.artifactPackMode || 'tar') === 'tar',
  },
  {
    key: 'artifactRename',
    get label() { return t('pipelineJob.fieldArtifactRenameLabel') },
    kind: 'text',
    monospace: true,
    placeholder: 'app.jar',
    get hint() { return t('pipelineJob.fieldArtifactRenameHint') },
  },
  ...EXEC_OPTION_FIELDS,
  {
    key: 'cachePaths',
    get label() { return t('pipelineJob.fieldCachePathsLabel') },
    kind: 'textarea',
    monospace: true,
    placeholder: 'node_modules\n.m2/repository',
    get hint() { return t('pipelineJob.fieldCachePathsHint') },
  },
  {
    key: 'cacheKey',
    get label() { return t('pipelineJob.fieldCacheKeyLabel') },
    kind: 'text',
    monospace: true,
    get placeholder() { return t('pipelineJob.fieldCacheKeyPlaceholder') },
    get hint() { return t('pipelineJob.fieldCacheKeyHint') },
  },
]

// 节点级构建机选择器(FR-8-19):存 config.runner,语法与阶段级一致(空 = 跟随阶段/项目默认;
// `server:<id>` 钉死;标签项)。不进常规字段循环 —— JobDrawer 为脚本类节点单独渲染「构建机」区块。
export const RUNNER_FIELD_KEY = 'runner'
const RUNNER_FIELD: JobField = {
  key: RUNNER_FIELD_KEY,
  get label() { return t('pipelineCanvas.runnerSectionLabel') },
  kind: 'runnerPicker',
  get hint() { return t('pipelineJob.jobRunnerHint') },
}
SCRIPT_FIELDS.push(RUNNER_FIELD)

// 部署节点共用字段(deploy_ssh 主机部署 / deploy_container 容器部署;旧 deploy_frontend 模板沿用)。
// 语义:部署目标 → 产物来源(精确绑定,无「自动」)→ 滚动批次 → 健康检查 → 重启/容器参数。
const DEPLOY_SSH_FIELDS: JobField[] = [
  {
    key: 'selector',
    get label() { return t('pipelineJob.fieldDeploySelectorLabel') },
    kind: 'labelSelector',
    get hint() { return t('pipelineJob.fieldDeploySelectorHint') },
  },
  {
    key: 'selectorMode',
    get label() { return t('pipelineJob.fieldSelectorModeLabel') },
    kind: 'select',
    options: SELECTOR_MODE_OPTIONS,
    get hint() { return t('pipelineJob.fieldSelectorModeHint') },
    when: (c) => (c.selector || '').split(',').filter((s) => s.trim()).length > 1,
  },
  {
    key: 'serverId',
    get label() { return t('pipelineJob.fieldServerIdLabel') },
    kind: 'server',
    get hint() { return t('pipelineJob.fieldServerIdHint') },
  },
  {
    key: 'deployMode',
    get label() { return t('pipelineJob.fieldDeployModeLabel') },
    kind: 'select',
    options: DEPLOY_MODE_OPTIONS,
    get hint() { return t('pipelineJob.fieldDeployModeHint') },
  },
  {
    key: 'artifactJob',
    get label() { return t('pipelineJob.fieldArtifactJobLabel') },
    kind: 'producer',
    get hint() { return t('pipelineJob.fieldArtifactJobHint') },
    when: (c) => (c.deployMode || 'artifact') !== 'command',
  },
  {
    key: 'artifactName',
    get label() { return t('pipelineJob.fieldArtifactNameLabel') },
    kind: 'text',
    monospace: true,
    placeholder: 'app-*.jar',
    get hint() { return t('pipelineJob.fieldArtifactNameHint') },
    when: (c) => (c.deployMode || 'artifact') !== 'command' && !!(c.artifactJob || '').trim(),
  },
  {
    key: 'firstBatchSize',
    get label() { return t('pipelineJob.fieldFirstBatchSizeLabel') },
    kind: 'number',
    placeholder: '1',
    get hint() { return t('pipelineJob.fieldFirstBatchSizeHint') },
  },
  {
    key: 'batchSize',
    get label() { return t('pipelineJob.fieldBatchSizeLabel') },
    kind: 'number',
    placeholder: '0',
    get hint() { return t('pipelineJob.fieldBatchSizeHint') },
  },
  {
    key: 'healthPort',
    get label() { return t('pipelineJob.fieldHealthPortLabel') },
    kind: 'number',
    placeholder: '8080',
    get hint() { return t('pipelineJob.fieldHealthPortHint') },
  },
  {
    key: 'healthPath',
    get label() { return t('pipelineJob.fieldHealthPathLabel') },
    kind: 'text',
    monospace: true,
    placeholder: '/healthz',
    get hint() { return t('pipelineJob.fieldHealthPathHint') },
    when: (c) => !!(c.healthPort || '').trim(),
  },
  {
    // 存量完整 URL 写法:只在已配置时出现(新配置用端口+路径,后端拼成 127.0.0.1 URL)。
    key: 'healthUrl',
    get label() { return t('pipelineJob.fieldHealthUrlLabel') },
    kind: 'text',
    monospace: true,
    placeholder: 'http://127.0.0.1:8080/healthz',
    get hint() { return t('pipelineJob.fieldHealthUrlHint') },
    when: (c) => !!(c.healthUrl || '').trim(),
  },
  {
    key: 'healthCommand',
    get label() { return t('pipelineJob.fieldHealthCommandLabel') },
    kind: 'text',
    monospace: true,
    placeholder: 'curl -fsS 127.0.0.1:8080/healthz',
    get hint() { return t('pipelineJob.fieldHealthCommandHint') },
    when: (c) => !(c.healthUrl || '').trim() && !(c.healthPort || '').trim(),
  },
  {
    key: 'healthRetries',
    get label() { return t('pipelineJob.fieldHealthRetriesLabel') },
    kind: 'number',
    placeholder: '3',
    get hint() { return t('pipelineJob.fieldHealthRetriesHint') },
    when: (c) => !!(c.healthUrl || '').trim() || !!(c.healthPort || '').trim() || !!(c.healthCommand || '').trim(),
  },
  {
    key: 'healthIntervalSeconds',
    get label() { return t('pipelineJob.fieldHealthIntervalLabel') },
    kind: 'number',
    placeholder: '3',
    get hint() { return t('pipelineJob.fieldHealthIntervalHint') },
    when: (c) => !!(c.healthUrl || '').trim() || !!(c.healthCommand || '').trim(),
  },
  {
    key: 'deployPath',
    get label() { return t('pipelineJob.fieldDeployPathLabel') },
    kind: 'text',
    monospace: true,
    placeholder: '/opt/app',
    get hint() { return t('pipelineJob.fieldDeployPathHint') },
  },
  {
    key: 'restartCommand',
    get label() { return t('pipelineJob.fieldRestartCommandLabel') },
    kind: 'textarea',
    monospace: true,
    placeholder: 'systemctl restart app\nnginx -s reload',
    get hint() { return t('pipelineJob.fieldRestartCommandHint') },
  },
]

// 容器部署节点字段(deploy_container):固定部署 image 产物。
const DEPLOY_CONTAINER_FIELDS: JobField[] = [
  {
    key: 'selector',
    get label() { return t('pipelineJob.fieldDeploySelectorLabel') },
    kind: 'labelSelector',
    get hint() { return t('pipelineJob.fieldDeploySelectorHint') },
  },
  {
    key: 'selectorMode',
    get label() { return t('pipelineJob.fieldSelectorModeLabel') },
    kind: 'select',
    options: SELECTOR_MODE_OPTIONS,
    get hint() { return t('pipelineJob.fieldSelectorModeHint') },
    when: (c) => (c.selector || '').split(',').filter((s) => s.trim()).length > 1,
  },
  {
    key: 'serverId',
    get label() { return t('pipelineJob.fieldServerIdLabel') },
    kind: 'server',
    get hint() { return t('pipelineJob.fieldServerIdHint') },
  },
  {
    key: 'artifactJob',
    get label() { return t('pipelineJob.fieldArtifactJobLabel') },
    kind: 'producer',
    producerKinds: ['build_image'],
    get hint() { return t('pipelineJob.fieldArtifactJobImageHint') },
  },
  {
    key: 'firstBatchSize',
    get label() { return t('pipelineJob.fieldFirstBatchSizeLabel') },
    kind: 'number',
    placeholder: '1',
    get hint() { return t('pipelineJob.fieldFirstBatchSizeHint') },
  },
  {
    key: 'batchSize',
    get label() { return t('pipelineJob.fieldBatchSizeLabel') },
    kind: 'number',
    placeholder: '0',
    get hint() { return t('pipelineJob.fieldBatchSizeHint') },
  },
  {
    key: 'healthPort',
    get label() { return t('pipelineJob.fieldHealthPortLabel') },
    kind: 'number',
    placeholder: '8080',
    get hint() { return t('pipelineJob.fieldHealthPortHint') },
  },
  {
    key: 'healthPath',
    get label() { return t('pipelineJob.fieldHealthPathLabel') },
    kind: 'text',
    monospace: true,
    placeholder: '/healthz',
    get hint() { return t('pipelineJob.fieldHealthPathHint') },
    when: (c) => !!(c.healthPort || '').trim(),
  },
  {
    // 存量完整 URL 写法:只在已配置时出现(新配置用端口+路径,后端拼成 127.0.0.1 URL)。
    key: 'healthUrl',
    get label() { return t('pipelineJob.fieldHealthUrlLabel') },
    kind: 'text',
    monospace: true,
    placeholder: 'http://127.0.0.1:8080/healthz',
    get hint() { return t('pipelineJob.fieldHealthUrlHint') },
    when: (c) => !!(c.healthUrl || '').trim(),
  },
  {
    key: 'healthExec',
    get label() { return t('pipelineJob.fieldHealthExecLabel') },
    kind: 'textarea',
    monospace: true,
    placeholder: 'pg_isready -U postgres',
    get hint() { return t('pipelineJob.fieldHealthExecHint') },
    when: (c) => !(c.healthPort || '').trim() && !(c.healthUrl || '').trim() && !(c.healthCommand || '').trim(),
  },
  {
    // 存量主机命令写法:只在已配置时出现(容器部署常用端口探测或容器内命令)。
    key: 'healthCommand',
    get label() { return t('pipelineJob.fieldHealthCommandLabel') },
    kind: 'text',
    monospace: true,
    placeholder: 'curl -fsS 127.0.0.1:8080/healthz',
    get hint() { return t('pipelineJob.fieldHealthCommandHint') },
    when: (c) => !!(c.healthCommand || '').trim(),
  },
  {
    key: 'healthRetries',
    get label() { return t('pipelineJob.fieldHealthRetriesLabel') },
    kind: 'number',
    placeholder: '3',
    when: (c) => !!(c.healthUrl || '').trim() || !!(c.healthPort || '').trim() || !!(c.healthCommand || '').trim() || !!(c.healthExec || '').trim(),
  },
  {
    key: 'healthIntervalSeconds',
    get label() { return t('pipelineJob.fieldHealthIntervalLabel') },
    kind: 'number',
    placeholder: '3',
    when: (c) => !!(c.healthUrl || '').trim() || !!(c.healthPort || '').trim() || !!(c.healthCommand || '').trim() || !!(c.healthExec || '').trim(),
  },
  {
    key: 'containerName',
    get label() { return t('pipelineJob.fieldContainerNameLabel') },
    kind: 'text',
    monospace: true,
    placeholder: 'app',
    get hint() { return t('pipelineJob.fieldContainerNameHint') },
  },
  {
    key: 'ports',
    get label() { return t('pipelineJob.fieldPortsLabel') },
    kind: 'text',
    monospace: true,
    placeholder: '8080:80, 9000:9000',
    get hint() { return t('pipelineJob.fieldPortsHint') },
  },
  {
    key: 'runArgs',
    get label() { return t('pipelineJob.fieldRunArgsLabel') },
    kind: 'text',
    monospace: true,
    placeholder: '-e KEY=value --restart always',
    get hint() { return t('pipelineJob.fieldRunArgsHint') },
  },
  {
    key: 'registryUrl',
    get label() { return t('pipelineJob.fieldRegistryUrlLabel') },
    kind: 'text',
    monospace: true,
    placeholder: 'registry.example.com:5000',
    get hint() { return t('pipelineJob.fieldRegistryUrlHint') },
  },
  {
    key: 'registryCredentialId',
    get label() { return t('pipelineJob.fieldRegistryCredentialLabel') },
    kind: 'credential',
    credentialType: 'registry',
    get hint() { return t('pipelineJob.fieldRegistryCredentialHint') },
  },
]

export const JOB_TYPE_SPECS: Record<string, JobTypeSpec> = {
  git_source: {
    type: 'git_source',
    get label() { return t('pipelineJob.typeGitSourceLabel') },
    get description() { return t('pipelineJob.typeGitSourceDesc') },
    accent: 'cyan',
    category: 'source',
    fields: [
      {
        key: 'repoUrl',
        get label() { return t('pipelineJob.fieldRepoUrlLabel') },
        kind: 'text',
        monospace: true,
        placeholder: 'https://gitee.com/org/repo.git',
        get hint() { return t('pipelineJob.fieldRepoUrlHint') },
      },
      {
        key: 'branch',
        get label() { return t('pipelineJob.fieldBranchLabel') },
        kind: 'text',
        monospace: true,
        placeholder: 'master',
        get hint() { return t('pipelineJob.fieldBranchHint') },
      },
      {
        key: 'credentialId',
        get label() { return t('pipelineJob.fieldCredentialIdLabel') },
        kind: 'credential',
        credentialType: ['git_token', 'git_http', 'ssh_key', 'ssh_password'],
        get hint() { return t('pipelineJob.fieldCredentialIdHint') },
      },
      {
        key: 'depth',
        get label() { return t('pipelineJob.fieldDepthLabel') },
        kind: 'number',
        placeholder: '1',
        get hint() { return t('pipelineJob.fieldDepthHint') },
      },
    ],
  },

  build_image: {
    type: 'build_image',
    get label() { return t('pipelineJob.typeBuildImageLabel') },
    get description() { return t('pipelineJob.typeBuildImageDesc') },
    accent: 'primary',
    category: 'build',
    fields: [
      { key: 'artifactType', get label() { return t('pipelineJob.fieldArtifactTypeBuildLabel') }, kind: 'select', options: ARTIFACT_OPTIONS },
      {
        key: 'buildModel',
        get label() { return t('pipelineJob.fieldBuildModelLabel') },
        kind: 'select',
        options: BUILD_MODEL_OPTIONS,
        get hint() { return t('pipelineJob.fieldBuildModelHint') },
      },
      {
        key: 'dockerfilePath',
        get label() { return t('pipelineJob.fieldDockerfilePathLabel') },
        kind: 'text',
        monospace: true,
        placeholder: 'Dockerfile',
        when: modelIs('dockerfile'),
      },
      {
        key: 'context',
        get label() { return t('pipelineJob.fieldContextLabel') },
        kind: 'text',
        monospace: true,
        placeholder: '.',
        get hint() { return t('pipelineJob.fieldContextHint') },
        when: modelIs('dockerfile'),
      },
      {
        key: 'toolchainLanguage',
        get label() { return t('pipelineJob.fieldToolchainLanguageLabel') },
        kind: 'select',
        options: TOOLCHAIN_OPTIONS,
        when: modelIs('toolchain'),
      },
      {
        key: 'toolchainVersion',
        get label() { return t('pipelineJob.fieldToolchainVersionLabel') },
        kind: 'text',
        monospace: true,
        placeholder: '20',
        when: modelIs('toolchain'),
      },
      {
        key: 'buildCommand',
        get label() { return t('pipelineJob.fieldBuildCommandLabel') },
        kind: 'text',
        monospace: true,
        placeholder: 'npm run build',
        get hint() { return t('pipelineJob.fieldBuildCommandHint') },
        when: modelIs('toolchain'),
      },
    ],
  },

  push_image: {
    type: 'push_image',
    get label() { return t('pipelineJob.typePushImageLabel') },
    get description() { return t('pipelineJob.typePushImageDesc') },
    accent: 'amber',
    category: 'build',
    fields: [
      {
        key: 'registry',
        get label() { return t('pipelineJob.fieldRegistryLabel') },
        kind: 'text',
        monospace: true,
        placeholder: 'registry.cn-hangzhou.aliyuncs.com',
      },
      {
        key: 'imageName',
        get label() { return t('pipelineJob.fieldImageNameLabel') },
        kind: 'text',
        monospace: true,
        placeholder: 'org/app',
      },
      {
        key: 'tag',
        get label() { return t('pipelineJob.fieldTagLabel') },
        kind: 'text',
        monospace: true,
        placeholder: '${COMMIT_SHA}',
        get hint() { return t('pipelineJob.fieldTagHint') },
      },
      {
        key: 'credentialId',
        get label() { return t('pipelineJob.fieldRegistryCredentialLabel') },
        kind: 'credential',
        credentialType: 'registry',
        get hint() { return t('pipelineJob.fieldRegistryCredentialHint') },
      },
    ],
  },

  deploy_ssh: {
    type: 'deploy_ssh',
    get label() { return t('pipelineJob.typeDeploySshLabel') },
    get description() { return t('pipelineJob.typeDeploySshDesc') },
    accent: 'green',
    category: 'deploy',
    fields: DEPLOY_SSH_FIELDS,
  },

  deploy_container: {
    type: 'deploy_container',
    get label() { return t('pipelineJob.typeDeployContainerLabel') },
    get description() { return t('pipelineJob.typeDeployContainerDesc') },
    accent: 'primary',
    category: 'deploy',
    fields: DEPLOY_CONTAINER_FIELDS,
  },

  health_check: {
    type: 'health_check',
    get label() { return t('pipelineJob.typeHealthCheckLabel') },
    get description() { return t('pipelineJob.typeHealthCheckDesc') },
    accent: 'green',
    category: 'quality',
    fields: [
      {
        key: 'selector',
        get label() { return t('pipelineJob.fieldDeploySelectorLabel') },
        kind: 'labelSelector',
        get hint() { return t('pipelineJob.fieldHealthSelectorHint') },
      },
      {
        key: 'selectorMode',
        get label() { return t('pipelineJob.fieldSelectorModeLabel') },
        kind: 'select',
        options: SELECTOR_MODE_OPTIONS,
        get hint() { return t('pipelineJob.fieldSelectorModeHint') },
        when: (c) => (c.selector || '').split(',').filter((s) => s.trim()).length > 1,
      },
      {
        key: 'serverId',
        get label() { return t('pipelineJob.fieldServerIdLabel') },
        kind: 'server',
        get hint() { return t('pipelineJob.fieldServerIdHint') },
      },
      { key: 'probeMode', get label() { return t('pipelineJob.fieldProbeModeLabel') }, kind: 'select', options: PROBE_MODE_OPTIONS },
      {
        key: 'url',
        get label() { return t('pipelineJob.fieldUrlLabel') },
        kind: 'text',
        monospace: true,
        placeholder: 'http://localhost:8080/healthz',
        when: probeIs('http'),
      },
      {
        key: 'command',
        get label() { return t('pipelineJob.fieldCommandLabel') },
        kind: 'text',
        monospace: true,
        placeholder: 'curl -fsS localhost:8080/healthz',
        when: probeIs('command'),
      },
      { key: 'retries', get label() { return t('pipelineJob.fieldHealthRetriesLabel') }, kind: 'number', placeholder: '3' },
      { key: 'intervalSeconds', get label() { return t('pipelineJob.fieldIntervalSecondsLabel') }, kind: 'number', placeholder: '5' },
    ],
  },

  notify: {
    type: 'notify',
    get label() { return t('pipelineJob.typeNotifyLabel') },
    get description() { return t('pipelineJob.typeNotifyDesc') },
    accent: 'cyan',
    category: 'notify',
    fields: [
      {
        key: 'channel',
        get label() { return t('pipelineJob.fieldChannelLabel') },
        kind: 'channel',
        get hint() { return t('pipelineJob.fieldChannelHint') },
      },
      {
        key: 'titleTemplate',
        get label() { return t('pipelineJob.fieldTitleTemplateLabel') },
        kind: 'text',
        get placeholder() { return t('pipelineJob.fieldTitleTemplatePlaceholder') },
        get hint() { return t('pipelineJob.fieldTitleTemplateHint') },
      },
      {
        key: 'bodyTemplate',
        get label() { return t('pipelineJob.fieldBodyTemplateLabel') },
        kind: 'textarea',
        get placeholder() { return t('pipelineJob.fieldBodyTemplatePlaceholder') },
        get hint() { return t('pipelineJob.fieldBodyTemplateHint') },
      },
    ],
  },

  // ── 模板节点:按语言区分的构建模板(预填好镜像+命令,改改就能用)+ 旧模板键(存量兼容,不在目录)──
  build_nodejs: {
    type: 'build_nodejs',
    get label() { return t('pipelineJob.typeBuildNodejsLabel') },
    get description() { return t('pipelineJob.typeBuildNodejsDesc') },
    accent: 'green',
    category: 'build',
    fields: SCRIPT_FIELDS,
    defaultConfig: {
      image: 'node:20',
      commands: 'npm ci --no-audit --no-fund\nnpm run build',
      artifactPath: 'dist',
    },
  },

  build_java: {
    type: 'build_java',
    get label() { return t('pipelineJob.typeBuildJavaLabel') },
    get description() { return t('pipelineJob.typeBuildJavaDesc') },
    accent: 'amber',
    category: 'build',
    fields: SCRIPT_FIELDS,
    defaultConfig: {
      image: 'maven:3.9-eclipse-temurin-21',
      commands: 'mvn -B -DskipTests package',
      artifactPath: 'target/*.jar',
    },
  },

  build_golang: {
    type: 'build_golang',
    get label() { return t('pipelineJob.typeBuildGolangLabel') },
    get description() { return t('pipelineJob.typeBuildGolangDesc') },
    accent: 'cyan',
    category: 'build',
    fields: SCRIPT_FIELDS,
    defaultConfig: {
      image: 'golang:1.22',
      commands: 'CGO_ENABLED=0 go build -o bin/app ./...',
      artifactPath: 'bin/app',
    },
  },

  build_python: {
    type: 'build_python',
    get label() { return t('pipelineJob.typeBuildPythonLabel') },
    get description() { return t('pipelineJob.typeBuildPythonDesc') },
    accent: 'primary',
    category: 'build',
    fields: SCRIPT_FIELDS,
    defaultConfig: {
      image: 'python:3.12',
      commands: 'pip install -r requirements.txt\npython -m build',
      artifactPath: 'dist/*',
    },
  },

  build_frontend: {
    type: 'build_frontend',
    get label() { return t('pipelineJob.typeBuildFrontendLabel') },
    get description() { return t('pipelineJob.typeBuildFrontendDesc') },
    accent: 'primary',
    category: 'build',
    fields: SCRIPT_FIELDS,
    defaultConfig: {
      image: 'node:20',
      commands: 'cd frontend\nnpm install --no-audit --no-fund\nnpm run build',
      artifactPath: 'frontend/dist',
    },
  },

  build_backend: {
    type: 'build_backend',
    get label() { return t('pipelineJob.typeBuildBackendLabel') },
    get description() { return t('pipelineJob.typeBuildBackendDesc') },
    accent: 'amber',
    category: 'build',
    fields: SCRIPT_FIELDS,
    defaultConfig: {
      image: 'maven:3.9-eclipse-temurin-21',
      commands: 'cd backend\nmvn -B -DskipTests package',
      artifactPath: 'backend/target/*.jar',
    },
  },

  deploy_frontend: {
    type: 'deploy_frontend',
    get label() { return t('pipelineJob.typeDeployFrontendLabel') },
    get description() { return t('pipelineJob.typeDeployFrontendDesc') },
    accent: 'green',
    category: 'deploy',
    fields: DEPLOY_SSH_FIELDS,
    defaultConfig: { strategy: 'rolling', restartCommand: 'nginx -s reload' },
  },

  templated: {
    type: 'templated',
    get label() { return t('pipelineJob.typeTemplatedLabel') },
    get description() { return t('pipelineJob.typeTemplatedDesc') },
    accent: 'cyan',
    category: 'custom',
    fields: [
      {
        key: 'image',
        get label() { return t('pipelineJob.fieldImageLabel') },
        kind: 'text',
        monospace: true,
        placeholder: 'node:20',
      },
      {
        key: 'params',
        get label() { return t('pipelineJob.fieldParamsLabel') },
        kind: 'textarea',
        monospace: true,
        placeholder: 'dir=frontend\nbuildCmd=npm run build',
        get hint() { return t('pipelineJob.fieldParamsHint') },
      },
      {
        key: 'commandTemplate',
        get label() { return t('pipelineJob.fieldCommandTemplateLabel') },
        kind: 'textarea',
        monospace: true,
        placeholder: 'cd {{dir}}\nnpm install\n{{buildCmd}}',
        get hint() { return t('pipelineJob.fieldCommandTemplateHint') },
      },
      {
        key: 'artifactPath',
        get label() { return t('pipelineJob.fieldArtifactPathLabel') },
        kind: 'textarea',
        monospace: true,
        placeholder: '{{dir}}/dist',
        get hint() { return t('pipelineJob.fieldTemplatedArtifactPathHint') },
      },
      {
        key: 'workDir',
        get label() { return t('pipelineJob.fieldWorkDirLabel') },
        kind: 'text',
        monospace: true,
        placeholder: '.',
        get hint() { return t('pipelineJob.fieldTemplatedWorkDirHint') },
      },
      ...EXEC_OPTION_FIELDS,
      {
        key: 'cachePaths',
        get label() { return t('pipelineJob.fieldCachePathsLabel') },
        kind: 'textarea',
        monospace: true,
        placeholder: '{{dir}}/node_modules',
        get hint() { return t('pipelineJob.fieldTemplatedCachePathsHint') },
      },
      {
        key: 'cacheKey',
        get label() { return t('pipelineJob.fieldCacheKeyLabel') },
        kind: 'text',
        monospace: true,
        get placeholder() { return t('pipelineJob.fieldCacheKeyPlaceholder') },
        get hint() { return t('pipelineJob.fieldTemplatedCacheKeyHint') },
      },
    ],
    defaultConfig: {
      image: 'node:20',
      params: 'dir=frontend\nbuildCmd=npm run build',
      commandTemplate: 'cd {{dir}}\nnpm install --no-audit --no-fund\n{{buildCmd}}',
      artifactPath: '{{dir}}/dist',
    },
  },

  script: {
    type: 'script',
    get label() { return t('pipelineJob.typeScriptLabel') },
    get description() { return t('pipelineJob.typeScriptDesc') },
    accent: 'primary',
    category: 'custom',
    fields: SCRIPT_FIELDS,
  },

  custom: {
    type: 'custom',
    get label() { return t('pipelineJob.typeScriptLabel') },
    get description() { return t('pipelineJob.typeScriptDesc') },
    accent: 'primary',
    category: 'custom',
    fields: SCRIPT_FIELDS,
  },
}

// ─── Lookups ──────────────────────────────────────────────────────────────────

/** Canonical, ordered list of pickable types (旧键 build_frontend/build_backend/deploy_frontend 保留 spec 供存量渲染,但不再进目录)。 */
export const PICKABLE_TYPES: readonly string[] = [
  'git_source',
  'build_nodejs',
  'build_java',
  'build_golang',
  'build_python',
  'build_image',
  'push_image',
  'deploy_container',
  'deploy_ssh',
  'health_check',
  'notify',
  'script',
  'templated',
]

/** Dropdown options for the job type selector (friendly label + token). */
export const JOB_TYPE_OPTIONS: SelectOption[] = PICKABLE_TYPES.map((type) => ({
  value: type,
  get label() {
    return `${JOB_TYPE_SPECS[type].label} · ${type}`
  },
}))

/** Picker categories in display order. */
export const JOB_CATEGORIES: ReadonlyArray<{ id: CategoryId; label: string }> = [
  { id: 'source', get label() { return t('pipelineJob.categorySource') } },
  { id: 'build', get label() { return t('pipelineJob.categoryBuild') } },
  { id: 'deploy', get label() { return t('pipelineJob.categoryDeploy') } },
  { id: 'quality', get label() { return t('pipelineJob.categoryQuality') } },
  { id: 'notify', get label() { return t('pipelineJob.categoryNotify') } },
  { id: 'custom', get label() { return t('pipelineJob.categoryCustom') } },
]

/** Pickable specs grouped by category, in display order; empty groups omitted. */
export function groupedJobTypes(): Array<{ id: CategoryId; label: string; specs: JobTypeSpec[] }> {
  return JOB_CATEGORIES.map((cat) => ({
    id: cat.id,
    label: cat.label,
    specs: PICKABLE_TYPES.map((t) => JOB_TYPE_SPECS[t]).filter((s) => s.category === cat.id),
  })).filter((g) => g.specs.length > 0)
}

/** Accent colour for a type, falling back to neutral for unknown types. */
export function jobTypeAccent(type: string): AccentName {
  return JOB_TYPE_SPECS[type]?.accent ?? 'neutral'
}

/** Spec for a job type, or null when the type has no typed schema. */
export function getJobTypeSpec(type: string): JobTypeSpec | null {
  return JOB_TYPE_SPECS[type] ?? null
}

/** Friendly zh label for a type, falling back to the raw token. */
export function jobTypeLabel(type: string): string {
  return JOB_TYPE_SPECS[type]?.label ?? type
}

/** The set of config keys owned by a type's schema (used to split out raw extras). */
export function schemaKeys(type: string): Set<string> {
  const spec = JOB_TYPE_SPECS[type]
  return new Set(spec ? spec.fields.map((f) => f.key) : [])
}

/**
 * Split a config map into the keys owned by the type's schema vs. the rest
 * (rendered in the "raw parameters" advanced section). Order of extras preserved.
 */
export function splitConfig(
  type: string,
  config: Record<string, string>,
): { extras: Array<[string, string]> } {
  const owned = schemaKeys(type)
  const extras = Object.entries(config).filter(([k]) => !owned.has(k))
  return { extras }
}

/**
 * 「脚本类」节点:后端 `isScriptJob`(internal/build/dag_stage_exec.go)按 script 路径执行
 * (容器跑 + 收 artifactPath 产物)的那批 type。可视化步骤构建器只对这些 type 出现,
 * 因为它编译/反解析的就是这套 commands/artifactPath 键。与后端保持一致。
 */
const SCRIPT_CLASS_TYPES = new Set<string>([
  'script',
  'custom',
  'build_nodejs',
  'build_java',
  'build_golang',
  'build_python',
  'build_frontend',
  'build_backend',
  'templated',
])

export function isScriptClassType(type: string): boolean {
  return SCRIPT_CLASS_TYPES.has(type.trim())
}
