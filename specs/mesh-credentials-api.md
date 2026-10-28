# Mesh 临时凭据 API

> 状态:已实现。相关:`internal/httpapi/mesh_credentials_admin.go`(HTTP 面)、`internal/mesh/credentials.go`(登记簿 + `CredentialRecord`/`ApiToken` 类型)、`internal/mesh`(EasyTier `CredentialProvider` 签发/撤销/活跃表)。

## 语义

为 EasyTier 集群(**Secure Mode** 模式下)签发**临时入网凭据**:设备凭 `credentialSecret` 入网,凭据带 TTL 自动过期,可提前撤销(全网 1~3 秒踢下线)。三个端点同时服务两类调用方,经 `meshCredentialAuth` **双通道认证**。

- **明文纪律**:`credentialSecret` 只出现在签发响应一次(绝不落库/落日志);API 令牌明文只出现在创建响应一次(库内只有 SHA-256)。
- **签发链路**:Secure Mode 前置校验 → 经本节点 `easytier-core` 生成(自预生成 `credentialId` UUID)→ 登记簿落库 → 明文 secret 仅本次返回。落库失败即回滚撤销已签出的 easytier 凭据(尽力而为;残留会以 `external` 幽灵行出现在列表且可撤销,有 TTL 自然过期兜底)。
- **列表合并**:登记簿行 × easytier 活跃表合并——撤销以登记簿为准,活跃/过期以 easytier 为准;活跃表里有而登记簿无的(其它管理节点 CLI 直签)以 `source: "external"` 补列。

## 认证(双通道)

由 `meshCredentialAuth` 统一网关,两条通道二选一:

1. **外部系统(API 令牌)**:请求头 `Authorization: Bearer pwmc_xxx`。令牌经 `mesh_api_tokens` 表 SHA-256 哈希查表校验;审计 actor 记为 `token:<名称>`。令牌由 `POST /api/mesh/api-tokens` 创建(见关联端点)。
2. **浏览器(管理控制台)**:管理员登录会话 Cookie + `X-CSRF-Token` 头(与 `/api` 组同款 `requireAuth → requireCSRF → requireAdmin` 链;三条路径挂根路由,若挂进 `/api/*` 子组会被根路由静态路由遮蔽)。

未走 Bearer 头时即视为浏览器路径。认证失败:`401`(`mesh_token_invalid`);已登录但 POST 缺 CSRF:`403`;非管理员:管理面权限拒绝。

错误体统一信封:

```json
{ "error": { "code": "mesh_token_invalid", "message": "凭据 API 令牌无效或已撤销" } }
```

## 端点

| 端点 | 作用 |
|---|---|
| `POST /api/mesh/credentials` | 签发临时凭据。审计 `mesh_credential_issue` |
| `GET /api/mesh/credentials` | 列出凭据(登记簿 × 活跃表合并) |
| `POST /api/mesh/credentials/{id}/revoke` | 撤销凭据(先撤 easytier,再标记登记簿)。审计 `mesh_credential_revoke` |

### 1. 签发 `POST /api/mesh/credentials`

请求体(JSON,`MaxBytesReader` ≤ 16KB):

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `ttlSeconds` | int64 | 否 | 有效期(秒)。默认 **86400(24h)**;范围 **60 ~ 7776000(60s ~ 90 天)**,越界 422 |
| `remark` | string | 否 | 备注,最长 **200 字符**(`rune` 计),超限 422 |

成功响应 `200`:

| 字段 | 类型 | 说明 |
|---|---|---|
| `credentialId` | string | UUID,凭据 ID(平台预生成) |
| `credentialSecret` | string | **明文仅此一次返回**,调用方负责安全送达设备 |
| `expiresAt` | string | RFC3339(UTC),过期时刻 |
| `remark` | string | 回落备注 |

```json
{
  "credentialId": "550e8400-e29b-41d4-a716-446655440000",
  "credentialSecret": "pwmc_…",
  "expiresAt": "2026-11-04T08:00:00Z",
  "remark": "ci 测试"
}
```

### 2. 列表 `GET /api/mesh/credentials`

无查询参数。成功响应 `200`:`{ "items": [ … ] }`,`items` 元素 = `CredentialRecord` 内嵌 + `status`:

| 字段 | 类型 | 说明 |
|---|---|---|
| `credentialId` | string | 凭据 ID |
| `remark` | string | 备注(外部签发为空) |
| `ttlSeconds` | int64 | 签发时的 TTL |
| `createdAt` | string | RFC3339,创建时刻 |
| `expiresAt` | string | RFC3339,过期时刻 |
| `revokedAt` | string | RFC3339,撤销时刻;**空 = 未撤销** |
| `createdBy` | string | 签发者(`admin` / `token:<名>`;外部签发为空) |
| `source` | string | `admin` \| `token` \| `external` |
| `status` | string | `active` \| `revoked` \| `expired` \| `missing`(见下) |

`status` 判定(以登记簿 + 活跃表合并):

| 值 | 含义 |
|---|---|
| `active` | 未撤销且在 easytier 活跃表中 |
| `revoked` | 登记簿已标记撤销(`revokedAt` 非空,优先级最高) |
| `expired` | 已过期(不在活跃表且已过 `expiresAt`) |
| `missing` | 未到期但不在活跃表(凭据文件丢失或管理节点侧被清理) |

```json
{
  "items": [
    {
      "credentialId": "…",
      "remark": "ci 测试",
      "ttlSeconds": 3600,
      "createdAt": "2026-10-28T08:00:00Z",
      "expiresAt": "2026-10-28T09:00:00Z",
      "revokedAt": "",
      "createdBy": "token:ci",
      "source": "token",
      "status": "active"
    }
  ]
}
```

### 3. 撤销 `POST /api/mesh/credentials/{id}/revoke`

- 路径参数 `id`:`credentialId`。无请求体。
- 先撤 easytier 侧(全网 1~3 秒内设备被踢下线),再标记登记簿;`{id}` 不在登记簿亦容忍(`ErrNotFound` 吞掉)——可撤外部签发的凭据。

成功响应 `200`:`{ "ok": true }`

## 错误码枚举

| HTTP | code | 出现端点 | 含义 |
|---|---|---|---|
| 401 | `mesh_token_invalid` | 全部 | Bearer 令牌无效/已撤销,或非 `pwmc_` 前缀 |
| 403 | (CSRF) | 全部 | 浏览器路径:已登录但写操作缺 `X-CSRF-Token` |
| 400 | `invalid_body` | 签发 | 请求体不是合法 JSON |
| 422 | `invalid_ttl` | 签发 | 有效期不在 60 秒 ~ 90 天之间 |
| 422 | `invalid_remark` | 签发 | 备注超过 200 字 |
| 409 | `mesh_secure_mode_off` | 签发 | 集群未启用或未开 Secure Mode,请先发起全集群统一开启 |
| 502 | `mesh_credential_generate` | 签发 | easytier-core 签发失败(错误首行摘要) |
| 502 | `mesh_credential_revoke` | 撤销 | easytier 侧撤销失败(错误首行摘要) |
| 503 | `internal` | 全部 | 集群服务 / 运行时 / 凭据签发器未装配(EasyTier 未运行或外部模式) |

## curl 示例

```bash
# 0) 管理员创建 API 令牌(明文仅返回一次)
curl -X POST https://pw.example.com/api/mesh/api-tokens \
  -b cookies.txt -H "X-CSRF-Token: $CSRF" -H "Content-Type: application/json" \
  -d '{"name":"ci"}'
# → {"name":"ci","token":"pwmc_…","createdAt":"…"}

TOKEN=pwmc_…

# 1) 签发凭据(外部系统:Bearer 令牌通道)
curl -X POST https://pw.example.com/api/mesh/credentials \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"ttlSeconds":3600,"remark":"ci 测试"}'
# → {"credentialId":"…","credentialSecret":"…","expiresAt":"…","remark":"ci 测试"}

# 2) 列出凭据
curl -s https://pw.example.com/api/mesh/credentials \
  -H "Authorization: Bearer $TOKEN"
# → {"items":[…]}

# 3) 撤销凭据
curl -X POST https://pw.example.com/api/mesh/credentials/<credentialId>/revoke \
  -H "Authorization: Bearer $TOKEN"
# → {"ok":true}
```

## 关联端点(配套,仅管理员会话通道)

三个 api-tokens 端点挂在管理面 `/api` 组(`requireAuth → requireCSRF → requireAdmin`),**不接** Bearer 令牌通道:

| 端点 | 作用 |
|---|---|
| `POST /api/mesh/api-tokens` | 创建 API 令牌 `{name}`;重名 422 `invalid_token_name`;响应 `{name, token, createdAt}`,`token` 明文仅此一次。审计 `mesh_api_token_create` |
| `GET /api/mesh/api-tokens` | 列出令牌(含 `lastUsedAt`/`revokedAt`,哈希绝不外显) |
| `DELETE /api/mesh/api-tokens/{name}` | 撤销令牌;不存在 → 404 `not_found`。审计 `mesh_api_token_revoke` |

Secure Mode 全集群统一开关(仅管理员会话):

| 端点 | 作用 |
|---|---|
| `POST /api/mesh/secure-mode/switch` | 发起统一开启/关闭 `{action, effectiveInMinutes}`;有节点未确认 → 409 `mesh_switch_aborted` 并回滚。审计 `mesh_secure_mode_switch` |
| `POST /api/mesh/secure-mode/abort` | 撤销进行中的切换 `{changeId}`。审计 `mesh_secure_mode_abort` |
| `GET /api/mesh/secure-mode/status` | 当前安全模式与节点状态(时刻字段空串 = 无) |

## 边界与取舍

- **TTL 护栏为平台自定**:官方 CLI 无默认无上限;平台收敛为默认 24h、最长 90 天、下限 60s(下限给联调留空间,非鼓励超短凭据)。
- **凭据签发依赖本节点 EasyTier**:外部模式 / EasyTier 未运行时 `CredentialProvider()` 为 nil → 503,不在无签发器时伪造凭据。
- **撤销外部凭据**:活跃表里有但登记簿无的凭据(其它节点 CLI 直签)可撤,`MarkCredentialRevoked` 对未知 ID 容忍。
- **落库失败补偿**:easytier 已签出但登记失败时尽力回滚撤销;回滚也失败则残留 `external` 幽灵行至 TTL 过期,列表可见且可手动撤销。

## 实现位置

- HTTP 面:`internal/httpapi/mesh_credentials_admin.go`(`meshCredentialAuth` 双通道网关、`mountMeshCredentialAPI` 挂根路由、issue/list/revoke 三 handler、`meshCredActor` context 注入、api-tokens 与 secure-mode handler、`firstLineOf` 错误摘要)。
- 领域层:`internal/mesh/credentials.go`(`CredentialRecord`/`ApiToken` 类型、登记簿 `IssueCredentialRecord`/`ListCredentialRecords`/`MarkCredentialRevoked`、`Service` 接口、`ErrNotFound`/`ErrInvalidInput`/`ErrDuplicate`)。
- 测试:`internal/httpapi/mesh_credentials_test.go`(无认证/伪令牌/有效令牌签发/撤销后失效,浏览器路径 CSRF 链路,api-tokens 创建·重名·列表·删除·重复删除)。
