# DNS 提供商模型重构:一账户多根区 + 凭据 ID/Secret 分离

## 目标模型

- **Provider = 一个厂商账户**:`type / name / apiId(明文列,非机密,可回显可编辑)/ credentialId(vault 只存 Secret)`
  - dnspod:apiId=SecretId,Secret=Token;alidns:apiId=AccessKeyId,Secret=AccessKeySecret;cloudflare:apiId 为空,Secret=API Token
- **Zone = 根区**:新表 `dns_provider_zones`,一个 Provider 下多个 base_domain
- **引用关系**(按用户决策):`proxy_routes.dnsProviderId` 保持提供商级(DNS-01 只需要凭据),创建路由时校验通配符域落在该 provider 某 zone 下;预览配置结构不变(provider + 自己的 baseDomain);瞬时子域名分配改按 zone 入参
- **存量数据**:破坏式切换,不做逗号凭据兼容拆分(旧凭据需重填)

## 1. 数据库迁移 `0054_dns_provider_zones.sql`(sqlite + mysql 各一份)

- `CREATE TABLE dns_provider_zones(id, provider_id, base_domain, created_at, UNIQUE(provider_id, base_domain))` + provider_id 索引
- `INSERT INTO dns_provider_zones ... SELECT`(sqlite 用 `lower(hex(randomblob(16)))`,mysql 用 `UUID()`)把现有每行 provider 的 base_domain 搬成 zone
- `ALTER TABLE dns_providers ADD COLUMN api_id`(sqlite `TEXT NOT NULL DEFAULT ''`;mysql `VARCHAR(256) NOT NULL DEFAULT ''`)
- `ALTER TABLE dns_providers DROP COLUMN base_domain`

## 2. internal/dnsprovider(核心重写)

**dnsprovider.go**
- `Provider{ID,Type,Name,APIID,CredentialID,Zones []Zone,CreatedAt,UpdatedAt}`;`Zone{ID,ProviderID,BaseDomain,CreatedAt}`
- `CreateInput{Type,Name,APIID,CredentialID,BaseDomains []string}`(≥1 个合法根域);`UpdateInput{Name,APIID *string}`
- 校验:dnspod/alidns 的 apiId 必填、cloudflare 必须为空;根域校验复用 `ValidBaseDomain`
- 错误集调整:新增 `ErrZoneNotFound`;`ErrInvalidCredential` 语义改为"缺少 API ID / Secret";更新包文档与错误注释(移除 "id,token" 逗号格式描述)
- `Service` 接口改造:
  - `List`(装配 zones)/`Get`/`Create`/`Update(name,apiId)`/`Delete`
  - `AddZone(providerID, baseDomain)` / `RemoveZone(providerID, zoneID)`
  - `Verify(providerID) ([]ZoneVerifyResult, error)`——逐 zone 校验,返回 `{ZoneID,BaseDomain,Err}`
  - `DeleteSubdomainRecord(providerID, fqdn)`——按最长后缀匹配找 zone 再删
  - `AllocateSubdomain` 入参 `ProviderID`→`ZoneID`(zone→provider 供建 DNS-01 路由);`AllocateFQDN(ProviderID, Subdomain)`——按最长后缀匹配找 zone,不在任何 zone 下 → `ErrAllocate`
  - `ResolveToken`→`ResolveCredential(ctx,pid) (type, apiID, secret string, ok bool, err error)`
  - 新增 `Zones(ctx, pid) ([]Zone, bool, error)`(供 proxy 覆盖校验与 previewenv 校验)

**store.go**:provider CRUD 去 base_domain、增 api_id,加 update;zone CRUD(insertZones/listByProvider/get/delete);List 时一次性查 zones 按 provider 分组装配;`zoneForFQDN` 最长后缀匹配(`fqdn==base` 或 `HasSuffix(fqdn, "."+base)`)

**clients**:`newDNSClient(type, apiID, secret)`;删除 `parseDNSPodToken`/`parseAliCred`/`splitDNSCred`;dialFactory 签名同步改;`clientFor` 组装 (apiID, secret)

## 3. internal/proxy

- `DNSResolver` 接口:`ResolveToken`→`ResolveCredential`;新增 `ProviderZones(ctx,pid) ([]string, bool, error)`
- proxy.go:`dnsCred{Type,Token}`→`dnsCred{Type,APIID,Secret}`;`validateDNSProviderRef` 扩展——路由含通配符域时须落在该 provider 某 zone 下,否则 `ErrInvalidDNSProvider`(人话提示)
- caddy.go:删 `splitDNSCred`,alidns/tencentcloud 块直接写 `cred.APIID`/`cred.Secret`

## 4. cmd/pipewright/main.go

`dnsResolverAdapter` 适配 `ResolveCredential`/`ProviderZones`;装配链路不变

## 5. internal/httpapi(dnsprovider.go + router.go)

- DTO:`{id,type,name,apiId,zones:[{id,baseDomain}],credentialConfigured,createdAt}`
- `POST /api/dns/providers` body `{type,name,apiId,secret,baseDomains[]}`——vault.Create 只存 Secret(dnspod/alidns 校验 apiId 非空),失败回滚凭据(保留现状)
- `PUT /api/dns/providers/{id}`(新)`{name?,apiId?,secret?}`——secret 走 `vault.Update`(同 credentialId 原地轮换),name/apiId 走 svc.Update
- `POST /api/dns/providers/{id}/zones`(新)、`DELETE /api/dns/providers/{id}/zones/{zoneId}`(新)
- verify 响应改 `{ok, zones:[{id,baseDomain,ok,error}]}`
- `POST /api/proxy/subdomains` body `providerId`→`zoneId`
- 新审计 action(update / zone.add / zone.remove);detail 只含 type/name/zones,绝无 secret;`writeDNSProviderError` 文案更新(删除逗号格式提示)

## 6. internal/previewenv

`SetConfig` 启用时校验 baseDomain 落在所选 provider 某 zone 下(注入小查询接口,模式同 recordDeleter);provision/Reclaim 调用点不变

## 7. 前端 web/src

- `api/dnsProviders.ts`:类型(apiId/zones)、create 入参、`updateDnsProvider`/`addDnsZone`/`removeDnsZone`、verify 逐 zone 结果;子域名分配 API 改传 zoneId
- `views/settings/SettingsDnsProviders.vue`:创建表单拆 apiId(条件显示,cloudflare 隐藏)+ secret(password)+ 根区动态多值输入;列表展示 apiId 与 zones 标签;编辑弹窗(名称/apiId/可选轮换 secret);zone 增删操作;verify 逐 zone 结果展示
- `components/ops/InstantSubdomain.vue`:下拉改为 zone 选项(label `provider.name · baseDomain`)
- `components/ops/RouteAdvancedSettings.vue`:仍 provider 级,选项 label 附 zones 概览;`ProjectPreviewConfig.vue` 字段不变
- i18n:8 个 locale 的 `dnsProviders.ts` 全量更新(字段拆分、多 zone、编辑/zone 操作文案)

## 8. 测试与验证

- `dnsprovider_test.go`:zone CRUD/装配、后缀匹配、apiId 必填、多 zone Verify、AllocateSubdomain(zone 入参)
- `dnspod_test.go`/`alidns_test.go`:删 parse* 测试,改 (apiID,secret) 构造用例
- `proxy_r3_test.go`:dnsCred 新结构渲染、通配符 zone 覆盖校验通过/拒绝
- `previewenv_test.go`:baseDomain 覆盖校验
- 验证:`go build ./... && go test ./...`;web 目录 `npm run build`(含 vue-tsc 类型检查)

**已知行为**(与现状一致,不额外加守卫):删除 provider/zone 不级联检查路由与预览配置引用,悬挂引用在下次操作时报错提示重建。