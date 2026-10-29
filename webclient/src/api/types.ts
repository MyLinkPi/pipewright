// 与 Go 侧 JSON 契约一一对应:
//   internal/pwclient/runtime.go (StatusSnapshot)
//   internal/pwclient/ipc.go    (configView / ConfigSetRequest / LogChunk)
//   internal/pwclient/upgrade.go(UpgradeStatus)
//   internal/uiserver/local.go  (LocalInfo)
// 改动这里必须同步 Go 侧 golden 单测(internal/pwclient/ipc_test.go)。

export interface JoinStatus {
  networkName: string
  hasSecret: boolean
  hasCredential: boolean
  secureMode: boolean
  peers: string[]
  virtualIp: string
  hostname: string
}

export interface Socks5Status {
  exitNodes: string[]
  enabled: boolean
  port: number
}

export interface ProxyStatus {
  port: number
  exitNodes: string[]
  running: boolean
  lastErr: string
}

export interface SupervisorStatus {
  running: boolean
  lastErr: string
  restarts: number
  startedAt: string
  adopted: boolean
}

export interface EasyTierLink {
  peerId: string
  virtualIp: string
  hostname: string
  version: string
  latencyMs: number
  lossRate: string
  rxBytes: number
  txBytes: number
  tunnel: string
  relay: boolean
  relayVia: string
}

export interface ProxyRouteVia {
  ip: string
  name: string
  relay: boolean
  relayVia: string
}

export interface ProxyRoute {
  cidr: string
  viaIp: string
  viaName: string
  vias: ProxyRouteVia[]
}

export interface EasyTierStatus {
  ok: boolean
  err: string
  selfIp: string
  hostname: string
  version: string
  links: EasyTierLink[]
  proxyCidrs: string[]
  proxyRoutes: ProxyRoute[]
}

export interface BinStatus {
  corePath: string
  cliPath: string
  version: string
  downloading: boolean
  lastErr: string
}

export interface ServiceInfo {
  runningAs: string
  pid: number
}

export interface AliyunGroupConfig {
  regionId: string
  securityGroupId: string
  description: string
}

export interface SecGroupItemStatus extends AliyunGroupConfig {
  status: string
  lastErr: string
}

export interface SecGroupStatus {
  configured: boolean
  accessKeyId: string
  hasSecret: boolean
  groups: AliyunGroupConfig[]
  lastIp: string
  lastRun: string
  lastErr: string
  results: SecGroupItemStatus[]
}

export interface StatusSnapshot {
  configured: boolean
  enabled: boolean
  join: JoinStatus
  routes: string[]
  socks5: Socks5Status
  proxies: ProxyStatus[]
  supervisor: SupervisorStatus
  // 组网状态:键名与 Go 侧 json:"easytier" 逐字一致(全小写,不同于内部类型名 EasyTierStatus)。
  // 写成 easyTier 会整体读空 → 状态恒「连接中…」;契约见 api/statusContract.test.ts。
  easytier: EasyTierStatus | null
  binaries: BinStatus
  service: ServiceInfo
  clientVersion: string
  clientPath: string
  securityGroup: SecGroupStatus
}

export interface JoinView {
  networkName: string
  hasSecret: boolean
  hasCredential: boolean
  secureMode: boolean
  peers: string[]
  virtualIp: string
  hostname: string
}

export interface Socks5Config {
  enabled: boolean
  port: number
  exitNodes: string[]
}

export interface ProxyConfig {
  port: number
  exitNodes: string[]
}

export interface EasyTierConfig {
  binDir: string
  rpcAddr: string
}

export interface AliyunView {
  accessKeyId: string
  hasSecret: boolean
  groups: AliyunGroupConfig[]
}

export interface ConfigView {
  enabled: boolean
  join: JoinView
  routes: string[]
  socks5: Socks5Config
  proxies: ProxyConfig[]
  easytier: EasyTierConfig
  aliyun: AliyunView
}

// ConfigSetRequest 是部分更新载荷:只提交需要修改的字段(未设置 = 保持原值)。
export interface ConfigSetRequest {
  enabled?: boolean
  networkName?: string
  networkSecret?: string
  credential?: string
  peers?: string[]
  virtualIp?: string
  hostname?: string
  routes?: string[]
  socks5Enabled?: boolean
  socks5Port?: number
  socks5ExitNodes?: string[]
  socks5Proxies?: ProxyConfig[]
  easytierBinDir?: string
  easytierRpc?: string
  aliyunAccessKeyId?: string
  aliyunAccessKeySecret?: string
  aliyunGroups?: AliyunGroupConfig[]
}

export interface ImportRequest {
  content: string
  replaceRoutes: boolean
}

export interface LogChunk {
  lines: string[]
  nextSeq: number
  reset: boolean
}

export type UpgradePhase =
  | ''
  | 'downloading'
  | 'applying'
  | 'restarting'
  | 'done'
  | 'skipped'
  | 'error'

export interface UpgradeStatus {
  phase: UpgradePhase
  current: string
  target: string
  message: string
  error?: string
  time: string
}

export interface UpdateInfo {
  current: string
  latest: string
  updateAvailable: boolean
  releaseUrl: string
  publishedAt: string
  notes: string
  /** latest 是否为预发布(beta/rc);未开启包含预发布时恒 false。 */
  prerelease?: boolean
  /** 本次检查是否包含非稳定版本(回显查询口径)。 */
  includeUnstable?: boolean
  checkError?: string
}

/** 后端持有的界面偏好(<dataDir>/ui-prefs.json),与 uiserver.UIPrefs 对齐。 */
export interface UIPrefs {
  theme: 'dark' | 'light'
  /** 升级接受非稳定版本(预发布 beta/rc)。 */
  includeUnstable: boolean
}

export interface ServiceState {
  installed: boolean
  state: string
}

export interface LocalInfo {
  platform: string
  version: string
  dataDir: string
  configFile: string
  logFile: string
  service: ServiceState
  autoStart: boolean
  canInstallService: boolean
}
