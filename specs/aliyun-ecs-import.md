# 阿里云 ECS 批量导入

> 状态:已实现。相关:`internal/aliyunecs/`(领域包)、`internal/httpapi/aliyun.go`(HTTP 面)、`web/src/views/settings/SettingsAliyunEcs.vue`(前端)、迁移 `0089_aliyun_ecs.sql`。

## 语义

「设置 → 阿里云 ECS」页(admin)把阿里云账户下的 ECS 实例批量登记为平台服务器(`servers` 行),并可反复执行以**更新**既有机器行:

- **账户配置(多套)**:每套 = 名称 + AccessKeyId + AccessKeySecret + Region(下拉,取自公共地域清单)。凭据按机密性拆开存储(同 dns_providers/object_stores 先例):AK 非机密明文列可回显;Secret 经 `vault.SealSecret` 密文 BLOB(同 pve_clusters 先例)长期保存,**绝不回显**——编辑弹窗里留空 = 不修改,非空 = 轮换。多 Region 以多条账户并存表达。
- **ECS OpenAPI 客户端**:包内手写(`net/http`,无 SDK,同 alidns 先例),采用官方推荐的 **V3 请求结构 + ACS3-HMAC-SHA256 签名**(新服务已只认 V3;旧 RPC V1 逐步下线)。端点 `https://ecs.<region>.aliyuncs.com`;仅消费 `DescribeInstances`(分页 PageSize=100,页数护栏 50 页 = 5000 台)。测试经 RoundTripper 拦截 + nowFn/nonceFn 钉死签名,不触真网络。
- **导入向导**:选账户 + IP 偏好(**内网 IP 默认**,可切公网)→ 拉取实例 → 手选机器范围(≤100 台/批;用 `GET /api/servers` 的 host 集合预标「将更新」)→ 连接设置(SSH 用户默认 root、端口默认 22)→ **每批统一的认证方式**(下拉选已有 vault 凭据 ssh_key/ssh_password,或内联粘贴密码/私钥由后端代建一条新凭据)→ 提交 → 逐台结果(created/updated/failed)。
- **更新匹配**:导入时后端**重新拉取**实例列表解析当前 IP(不信前端选型数据)。匹配顺序:`aliyun_ecs_instance_links` 实例 ID 映射(首选;**IP 变化后仍命中**,命中即刷新 host/name/port/user/credential,labels 等不动)→ 连接 IP 兜底(兼容导入前手工登记过的行;跳过 PVE 物理机与控制节点伪机)→ 都无则新建(default 池、standard)。成功即写/刷新映射。
- **逐台独立容错**:一台失败(实例已释放 / 无该类 IP / 凭据无效)只落该台 `error`,整批不 500。

## 端点(/api/aliyun*,memberGate 默认拒绝 → 实际仅 admin)

| 端点 | 作用 |
|---|---|
| `GET /api/aliyun/accounts` | 账户列表(无 Secret)。 |
| `POST /api/aliyun/accounts` | 新建 `{name, accessKeyId, secret, region}`。审计 `aliyun.account.create` |
| `PUT /api/aliyun/accounts/{id}` | 编辑(指针语义;secret 非空即轮换重封)。审计 `aliyun.account.update` |
| `DELETE /api/aliyun/accounts/{id}` | 删除账户 + 连带清其实例映射(已导入服务器不受影响)。审计 `aliyun.account.delete` |
| `POST /api/aliyun/accounts/{id}/test` | 最小 DescribeInstances(PageSize=1)测连。审计 `aliyun.account.test` |
| `GET /api/aliyun/accounts/{id}/instances` | 拉取该 Region 全部实例(选型数据源;私网/公网 IP 已合并 VPC/经典/EIP 来源) |
| `POST /api/aliyun/import` | `{accountId, instanceIds[], ipPreference, credentialId \| inlineSecret{kind,name,secret}, sshUser, sshPort}` → 逐台结果 + 汇总。审计 `aliyun.import` |

错误映射:参数非法 → 400;账户不存在 → 404;vault 未配置 → 503;阿里云 API 失败(凭据无效/Region 不存在/无权限/不可达)→ 502。错误体绝无 ak/sk 片段。

## 边界与取舍

- **「AccessToken」即 AccessKeySecret**:ECS OpenAPI 长期凭据只有 AK ID + Secret 对;STS Token 是临时凭据,不提供长期保存。
- **不做定时自动同步**:导入/更新均为手动触发,不监听实例变更事件。
- **内网 IP 默认**:要求平台服务器与 ECS 同 VPC / 专线互通;否则向导切公网 IP(未分配且未绑 EIP 的实例该台报错)。
- **不引入阿里云 SDK**:仅一个只读 API,手写签名客户端与 alidns 先例一致,零新增依赖。
- **不改 `servers` 表结构**:实例 ID ↔ 服务器行的映射独立成表(`aliyun_ecs_instance_links`,server 外键 CASCADE),servers 域零改动;导入/更新复用 `target.Service.Create/Update`,校验语义与手工登记完全一致(凭据存在性、类型、端口区间等)。
- **Region 清单为前端静态常量**(~31 个公共地域,2026-10 按官方文档核对;含本地地域武汉,不含关停中的南京/福州):下拉可搜索;后端只做格式校验(`^[a-z0-9]+(-[a-z0-9]+)*$`),非清单内 Region 显示 ⚠ 但不阻断(合作地域/新地域可用)。
- **导入的机器不自动打标签、不自动装 ssar/pwclient**:与手工登记的机器行为一致,导入后按需在机器管理页配置。

## 实现位置

- 领域包 `internal/aliyunecs/`:`client.go`(V3 签名 + DescribeInstances 分页 + 错误映射)、`accounts.go`(账户 CRUD + 测连 + 实例列表)、`import.go`(导入编排:凭据解析 / 映射匹配 / 逐台容错)、`aliyunecs.go`(类型 + Service 接口 + 装配)。
- HTTP 面:`internal/httpapi/aliyun.go`(DTO camelCase、审计、错误信封);路由注册紧邻 DNS 提供商(`router.go`);装配 `main.go`(`aliyunecs.New(st.DB, credVault, targetSvc)`)。
- 前端:`web/src/api/aliyunEcs.ts`(API 客户端 + `ALIYUN_REGIONS` 常量)、`web/src/views/settings/SettingsAliyunEcs.vue`(账户区 + 三步向导 + 逐台结果);机器管理页工具栏有「从阿里云导入」快捷跳转。
