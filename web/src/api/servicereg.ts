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
export type UpstreamKind = 'container' | 'address'

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
  protocol: ServiceProtocol
  upstreamKind: UpstreamKind
  upstream: string
  upstreamPort: number
  tcpListenPort: number
  enabled: boolean
  createdAt: string
  updatedAt: string
}

export interface GatewayServer {
  serverId: string
  serverName: string
  installed: boolean
  running: boolean
  image: string
  ports: string
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
  servers: GatewayServer[]
  lastApplyAt: string
  lastApplyError: string
  lastApplyErrors: Record<string, string> | null
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
}

export interface ServiceCreateInput {
  domainId: string
  name: string
  protocol: ServiceProtocol
  upstreamKind: UpstreamKind
  /** http+container 部署托管服务可空(实例行携带容器名)。 */
  upstream: string
  upstreamPort: number
  tcpListenPort?: number
}

export const getServiceRegSettings = (): Promise<Settings> =>
  http.get('/api/servicereg/settings')

export const updateServiceRegSettings = (u: SettingsUpdate): Promise<Settings> =>
  http.put('/api/servicereg/settings', u)

export const getGateway = (): Promise<Gateway> =>
  http.get('/api/servicereg/gateway')

export const deployGateway = (): Promise<Gateway> =>
  http.post('/api/servicereg/gateway/deploy')

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

/** 一键生成(幂等):按(基域, 服务名)注册/刷新服务;实例由部署按目标机自动注册。 */
export const ensureService = (in_: ServiceCreateInput): Promise<{ id: string; name: string; enabled: boolean }> =>
  http.post('/api/servicereg/services/ensure', in_)

export const deleteService = (id: string): Promise<{ ok: boolean }> =>
  http.delete(`/api/servicereg/services/${encodeURIComponent(id)}`)

export const setServiceEnabled = (id: string, enabled: boolean): Promise<{ ok: boolean }> =>
  http.post(`/api/servicereg/services/${encodeURIComponent(id)}/enabled`, { enabled })

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
