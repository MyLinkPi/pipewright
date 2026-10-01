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
  serverId: string
  httpPort: number
  httpsPort: number
  image: string
  network: string
  containerName: string
  volumeName: string
  lastApplyAt: string
  lastApplyError: string
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

export interface Gateway {
  configured: boolean
  serverId: string
  serverName: string
  installed: boolean
  running: boolean
  image: string
  ports: string
  lastApplyAt: string
  lastApplyError: string
}

/** 服务实例:多实例 upstream 成员(实例级轮转的单元)。 */
export interface ServiceInstance {
  id: string
  serviceId: string
  container: string
  /** 0 = 继承服务默认端口。 */
  port: number
  /** false = 已摘除(不渲染进 upstream)。 */
  attached: boolean
  createdAt: string
  updatedAt: string
}

export interface SettingsUpdate {
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

export const deleteService = (id: string): Promise<{ ok: boolean }> =>
  http.delete(`/api/servicereg/services/${encodeURIComponent(id)}`)

export const setServiceEnabled = (id: string, enabled: boolean): Promise<{ ok: boolean }> =>
  http.post(`/api/servicereg/services/${encodeURIComponent(id)}/enabled`, { enabled })

export const applyServiceReg = (): Promise<{ ok: boolean }> =>
  http.post('/api/servicereg/apply')

export const listInstances = (serviceId: string): Promise<{ items: ServiceInstance[] }> =>
  http.get(`/api/servicereg/services/${encodeURIComponent(serviceId)}/instances`)

export const addInstance = (serviceId: string, container: string, port: number): Promise<ServiceInstance> =>
  http.post(`/api/servicereg/services/${encodeURIComponent(serviceId)}/instances`, { container, port })

export const removeInstance = (id: string): Promise<{ ok: boolean }> =>
  http.delete(`/api/servicereg/instances/${encodeURIComponent(id)}`)

export const setInstanceAttached = (id: string, attached: boolean): Promise<{ ok: boolean }> =>
  http.post(`/api/servicereg/instances/${encodeURIComponent(id)}/attached`, { attached })
