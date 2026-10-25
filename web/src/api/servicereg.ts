/**
 * Service registry gateway (nginx) API client.
 *
 * 一个独立部署的 nginx 容器做「服务名 + 基域 → 子域名」反向代理(abc + efg.com →
 * abc.efg.com)。*.efg.com 泛域名由用户手动解析到网关主机;HTTPS 证书由「证书管理」
 * (acme.sh)签发/导入后经 CertSink 同步到基域,本模块不再暴露证书上传端点。
 *
 * GET    /api/servicereg/settings              → Settings
 * PUT    /api/servicereg/settings              → Settings          (CSRF)
 * GET    /api/servicereg/gateway               → Gateway
 * GET    /api/servicereg/gateway/config        → { serverId, files }  (?serverId=)
 * POST   /api/servicereg/gateway/deploy        → Gateway           (CSRF)
 * DELETE /api/servicereg/gateway               → { ok }            (CSRF)
 * GET    /api/servicereg/domains               → { items: Domain[] }
 * POST   /api/servicereg/domains               → Domain            (CSRF)
 * DELETE /api/servicereg/domains/{id}          → { ok }            (CSRF)
 * GET    /api/servicereg/services              → { items: Service[] }
 * POST   /api/servicereg/services              → { id, … }         (CSRF)
 * PUT    /api/servicereg/services/{id}         → { id, … }         (CSRF)
 * POST   /api/servicereg/services/{id}/enabled → { ok }            (CSRF)
 * DELETE /api/servicereg/services/{id}         → { ok }            (CSRF)
 * POST   /api/servicereg/apply                 → { ok }            (CSRF)
 */
import { http } from './http'

export type ServiceProtocol = 'http' | 'tcp'
export type UpstreamKind = 'container' | 'address' | 'static' | 'forward'

export interface Settings {
  /** 全部网关主机(多台部署完全一致的网关);空数组 = 未配置。 */
  serverIds: string[]
  /** 第一台网关主机(兼容保留)。 */
  serverId: string
  httpPort: number
  httpsPort: number
  image: string
  network: string
  containerName: string
  volumeName: string
  /** 静态发布根目录(各网关主机同一路径约定,只读挂进网关容器)。 */
  staticRoot: string
  lastApplyAt: string
  lastApplyError: string
  /** 逐台收敛错误(serverId → 文案,仅失败项)。 */
  lastApplyErrors: Record<string, string> | null
}

export interface Domain {
  id: string
  baseDomain: string
  hasCert: boolean
  certSubject: string
  certExpiresAt: string
  createdAt: string
  updatedAt: string
}

export interface RegisteredService {
  id: string
  domainId: string
  baseDomain: string
  name: string
  fqdn: string
  /** 子路径('' = 根路径 /);同 FQDN 下多服务靠不同 path 共存。 */
  path: string
  protocol: ServiceProtocol
  upstreamKind: UpstreamKind
  upstream: string
  upstreamPort: number
  tcpListenPort: number
  /** forward 专用:对端 mesh 节点 id + 对端服务 id;其余类型恒空。 */
  peerNodeId: string
  peerServiceId: string
  /** 反代剥离 location 前缀(默认 false = 原样透传;static/tcp 恒 false)。 */
  stripPathPrefix: boolean
  enabled: boolean
  /** 实例总数(含 detached;列表接口带回,徽标直显免逐服务加载)。 */
  instanceCount: number
  createdAt: string
  updatedAt: string
}

/** 跨环境转发:对端网关(目录下发,upstream 成员来源)。 */
export interface CatalogGateway {
  serverId: string
  serverName: string
  ip: string
}

/** 跨环境转发:对端目录里一条可转发服务。 */
export interface CatalogService {
  serviceId: string
  fqdn: string
  path: string
  protocol: ServiceProtocol
  hasCert: boolean
  tcpListenPort: number
}

/** 跨环境转发:对端服务目录(GET /api/servicereg/forward/peers 内嵌)。 */
export interface PeerCatalog {
  httpPort: number
  httpsPort: number
  gateways: CatalogGateway[]
  services: CatalogService[]
}

/** 跨环境转发:可用转发来源对端(available = 对端已授权且目录可达)。 */
export interface ForwardPeer {
  nodeId: string
  name: string
  virtualIp: string
  available: boolean
  /** forbidden(对端未授权)| unreachable(不可达)| disabled(集群未装配)| ''。 */
  reason: string
  catalog?: PeerCatalog
}

/** 网关机上一条跨环境 /32 路由的对账结果。 */
export interface GatewayRoute {
  serverId: string
  dest: string
  state: 'present' | 'applied' | 'removed' | 'missing'
  error: string
}

/** 跨环境转发数据面状态(网关状态内嵌)。 */
export interface ForwardStatus {
  ipForwardOk: boolean
  ipForwardDetail: string
  routes: GatewayRoute[]
  conflicts: string[]
}

export interface GatewayServer {
  serverId: string
  serverName: string
  installed: boolean
  running: boolean
  image: string
  ports: string
  /** 容器内 nginx 二进制版本(如 "1.27.4";探测失败为空)。 */
  version: string
}

export interface Gateway {
  configured: boolean
  /** 第一台网关(兼容保留);逐台状态见 servers。 */
  serverId: string
  serverName: string
  installed: boolean
  running: boolean
  image: string
  ports: string
  version: string
  servers: GatewayServer[]
  lastApplyAt: string
  lastApplyError: string
  lastApplyErrors: Record<string, string> | null
  /** 跨环境转发数据面状态(路由对账 / ip_forward / IP 冲突);未装配时缺省。 */
  forward?: ForwardStatus
}

/** 服务实例:集群 upstream 成员 = (服务器, 宿主端口)。容器实例 container 非空,
 * 由部署自动注册(宿主端口自动分配);非容器实例 container 为空(部署注册或人工添加)。 */
export interface ServiceInstance {
  id: string
  serviceId: string
  /** 归属服务器;'' = 遗留数据(待下次部署认领)。 */
  serverId: string
  /** 容器名;非容器实例为空。 */
  container: string
  /** 服务端口(0 = 继承服务默认端口)。 */
  port: number
  /** 网关反代宿主端口(0 = 继承)。 */
  hostPort: number
  /** false = 已摘除(不渲染进 upstream)。 */
  attached: boolean
  createdAt: string
  updatedAt: string
}

export interface SettingsUpdate {
  /** 全部网关主机(优先于 serverId;空数组 = 清空转配置态)。 */
  serverIds?: string[]
  /** 旧版单机入口,兼容保留。 */
  serverId?: string
  httpPort?: number
  httpsPort?: number
  image?: string
  network?: string
  containerName?: string
  volumeName?: string
  staticRoot?: string
}

export interface ServiceCreateInput {
  domainId: string
  name: string
  /** 子路径('' / 缺省 = 根路径 /;tcp 服务不支持)。 */
  path?: string
  protocol: ServiceProtocol
  upstreamKind: UpstreamKind
  /** http+container 部署托管服务可空(实例行携带容器名);static/forward 恒空。 */
  upstream: string
  /** static/forward 形态可填 0(无本地端口)。 */
  upstreamPort: number
  tcpListenPort?: number
  /** forward 专用:对端 mesh 节点 id + 对端服务 id。 */
  peerNodeId?: string
  peerServiceId?: string
  /** 反代剥离 location 前缀(默认 false = 保留前缀原样透传)。 */
  stripPathPrefix?: boolean
}

/** 更新可改字段(基域不可换;更新后后端自动 apply 收敛)。缺省字段保持原值。 */
export interface ServiceUpdateInput {
  name?: string
  path?: string
  protocol?: ServiceProtocol
  upstreamKind?: UpstreamKind
  upstream?: string
  upstreamPort?: number
  tcpListenPort?: number
  peerNodeId?: string
  peerServiceId?: string
  stripPathPrefix?: boolean
}

export const getServiceRegSettings = (): Promise<Settings> =>
  http.get('/api/servicereg/settings')

export const updateServiceRegSettings = (u: SettingsUpdate): Promise<Settings> =>
  http.put('/api/servicereg/settings', u)

export const getGateway = (): Promise<Gateway> =>
  http.get('/api/servicereg/gateway')

/** 网关容器内的一份配置文件(主配置或其引用文件;error 非空表示该文件读取失败)。 */
export interface NginxConfFile {
  path: string
  content: string
  error?: string
}

/** 查看某台网关主机容器内实际生效的 nginx.conf 及其引用文件(证书等)。 */
export const getGatewayConfig = (serverId: string): Promise<{ serverId: string; files: NginxConfFile[] }> =>
  http.get(`/api/servicereg/gateway/config?serverId=${encodeURIComponent(serverId)}`)

export const deployGateway = (): Promise<Gateway> =>
  http.post('/api/servicereg/gateway/deploy')

/** 单台网关主机的镜像升级结果(changed=false = 已是最新,未动容器)。 */
export interface GatewayImageUpgrade {
  serverId: string
  serverName: string
  ok: boolean
  changed: boolean
  error?: string
}

/** 网关镜像升级聚合结果(逐台独立成败;有变化的台重建容器,配置/证书在具名卷不丢)。 */
export interface GatewayImageUpgradeResult {
  /** 当前期望镜像引用。 */
  image: string
  results: GatewayImageUpgrade[]
}

/** 升级全部网关主机的 nginx 镜像:逐台 pull + 比对,有变化的台重建并全量收敛。 */
export const upgradeGatewayImage = (): Promise<GatewayImageUpgradeResult> =>
  http.post('/api/servicereg/gateway/upgrade-image')

export const removeGateway = (): Promise<{ ok: boolean }> =>
  http.delete('/api/servicereg/gateway')

export const listDomains = (): Promise<{ items: Domain[] }> =>
  http.get('/api/servicereg/domains')

export const createDomain = (baseDomain: string): Promise<Domain> =>
  http.post('/api/servicereg/domains', { baseDomain })

export const deleteDomain = (id: string): Promise<{ ok: boolean }> =>
  http.delete(`/api/servicereg/domains/${encodeURIComponent(id)}`)

export const listServices = (): Promise<{ items: RegisteredService[] }> =>
  http.get('/api/servicereg/services')

export const createService = (in_: ServiceCreateInput): Promise<{ id: string; name: string; enabled: boolean }> =>
  http.post('/api/servicereg/services', in_)

export const updateService = (id: string, in_: ServiceUpdateInput): Promise<{ id: string; name: string }> =>
  http.put(`/api/servicereg/services/${encodeURIComponent(id)}`, in_)

/** 一键生成(幂等):按(基域, 服务名, path)注册/刷新服务;实例由部署按目标机自动注册。 */
export const ensureService = (in_: ServiceCreateInput): Promise<{ id: string; name: string; enabled: boolean }> =>
  http.post('/api/servicereg/services/ensure', in_)

export const deleteService = (id: string): Promise<{ ok: boolean }> =>
  http.delete(`/api/servicereg/services/${encodeURIComponent(id)}`)

export const setServiceEnabled = (id: string, enabled: boolean): Promise<{ ok: boolean }> =>
  http.post(`/api/servicereg/services/${encodeURIComponent(id)}/enabled`, { enabled })

/** 跨环境转发:可用对端 + 各自服务目录(服务表单「转发其他环境」级联下拉)。 */
export const listForwardPeers = (): Promise<{ items: ForwardPeer[] }> =>
  http.get('/api/servicereg/forward/peers')

export const applyServiceReg = (): Promise<{ ok: boolean }> =>
  http.post('/api/servicereg/apply')

export const listInstances = (serviceId: string): Promise<{ items: ServiceInstance[] }> =>
  http.get(`/api/servicereg/services/${encodeURIComponent(serviceId)}/instances`)

export const addInstance = (
  serviceId: string,
  serverId: string,
  container: string,
  port: number,
  hostPort: number,
): Promise<ServiceInstance> =>
  http.post(`/api/servicereg/services/${encodeURIComponent(serviceId)}/instances`, {
    serverId, container, port, hostPort,
  })

export const removeInstance = (id: string): Promise<{ ok: boolean }> =>
  http.delete(`/api/servicereg/instances/${encodeURIComponent(id)}`)

export const setInstanceAttached = (id: string, attached: boolean): Promise<{ ok: boolean }> =>
  http.post(`/api/servicereg/instances/${encodeURIComponent(id)}/attached`, { attached })
