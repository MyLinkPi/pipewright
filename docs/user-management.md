# 用户与权限(user-management)

本文说明 Pipewright 的用户体系:角色模型、邀请与注册流程、权限矩阵、默认拒绝中间件、
数据迁移与安全注意事项。面向运维、二次开发者与需要理解权限边界的集成方。

---

## 1. 角色模型

平台只有两种角色,由会话携带(服务端 cookie),前端与后端据此分级:

| 角色 | 值 | 账号来源 | 说明 |
|---|---|---|---|
| 管理员 | `admin` | 单账号,存于 `admin_user`(`CHECK (id = 1)`) | 首启生成随机口令,或经 `PIPEWRIGHT_ADMIN_PASSWORD` 指定。拥有全部功能。 |
| 普通用户 | `member` | 多账号,存于 `users` 表 | 由管理员邀请注册,可见范围被严格限定。 |

设计要点:

- 管理员**不迁移**进 `users` 表 —— 保持既有单管理员认证模型不变,零回归风险。
- 普通用户与项目是**多对多**关系(`project_members`);管理员天然可见全部项目,不写入该表。
- 会话同时携带 `role` / `user_id` / `username`,供权限校验与界面回显;升级前签发的旧会话
  (无 `role`)按管理员兜底,保证平滑升级。

---

## 2. 邀请与注册流程

邀请采用**一次性 token + 自填用户名/口令**的模式,管理员不接触被邀请者的口令。

```
管理员                       被邀请者                     平台后端
  │                              │                          │
  ├─ 生成邀请 ───────────────────────────────────────────────▶ POST /api/invitations
  │                              │                          │   建 invitations 行(token, expires_at=+7d)
  │◀─ 完整注册链接(/register?token=<token>) ─────────────────┤
  │                              │                          │
  ├─ 通过安全渠道发送链接 ────────▶                          │
  │                              │                          │
  │                              ├─ 打开链接 ────────────────▶ GET /api/auth/invitations/{token}
  │                              │◀─ state: pending/used/expired
  │                              │                          │
  │                              ├─ 填表提交 ────────────────▶ POST /api/auth/register
  │                              │                          │   校验 token(存在/未用/未过期)
  │                              │                          │   校验用户名(3–32,字母数字_.-)与口令强度(≥8)
  │                              │                          │   建 users 行(argon2id 哈希)
  │                              │                          │   标记邀请 used_at
  │                              │◀─ 身份 + 会话 cookie(自动登录)─┤
  │                              │                          │
  ├─ 加入项目 ───────────────────────────────────────────────▶ POST /api/projects/{id}/members
  │                              │                          │   建 project_members 行
  │                              ├─ 查看「我的项目」───────────▶ GET /api/projects(仅返回成员项目)
```

关键语义:

- 邀请**一次性**:注册成功即写入 `used_at`,再次使用返回 `invitation_used`。
- 邀请**默认 7 天有效**,过期返回 `invitation_expired`;不存在/被撤销返回 `invitation_invalid`。
- 注册成功后后端直接下发会话 cookie(**自动登录**),前端随即跳转「我的项目」。
- 管理员可**撤销**未使用的邀请;已使用/已过期的邀请不可撤销。

---

## 3. 权限矩阵(端点 × 角色)

后端以**默认拒绝**为准绳:普通用户仅能访问下表白名单内的端点,且需通过项目归属校验;
其余端点一律 403。

### 3.1 公开端点(无需登录)

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/auth/login` | 登录(管理员 / 普通用户共用) |
| POST | `/api/auth/register` | 邀请注册(一次性 token) |
| GET | `/api/auth/invitations/{token}` | 查询邀请状态(注册页展示用) |
| GET | `/api/auth/session` | 当前会话(未登录返回 401) |
| POST | `/api/auth/logout` | 登出 |

### 3.2 普通用户可访问(白名单,含归属校验)

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/projects` | **仅返回**该用户加入的项目(服务端按成员过滤) |
| GET | `/api/projects/{id}/parameters` | 项目运行参数定义(触发运行表单所需;须为该项目成员) |
| POST | `/api/projects/{id}/runs` | 触发运行(须为该项目成员) |
| GET | `/api/runs` | 运行列表(服务端按成员项目过滤) |
| GET | `/api/runs/{id}` | 运行详情(须为该运行所属项目成员) |
| GET | `/api/runs/{id}/events` | 运行事件流(SSE 实时 tail) |
| GET | `/api/runs/{id}/logs` | 运行日志(历史回放) |
| GET | `/api/runs/{id}/artifacts` | 产物列表 |
| GET | `/api/runs/{id}/artifacts/{artifactId}/download` | 产物下载 |
| GET | `/api/account/sessions` | 自身活跃会话列表 |
| POST | `/api/account/password` | 修改自身口令 |
| DELETE | `/api/account/sessions/{id}` | 撤销自身会话 |

> 归属校验:项目维度直接取路径中的项目 id;运行维度先由 `run.Get` 解析出 `project_id`,
> 再校验成员关系。运行不存在或查询失败时**一律拒绝**,不泄露资源是否存在。

### 3.3 管理员专属(`requireAdmin` 纵深防御)

以下仅为代表,凡不在 3.1/3.2 中的已认证端点对普通用户均返回 403:

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/users` | 普通用户列表 |
| POST / GET | `/api/invitations` | 生成 / 列出邀请 |
| DELETE | `/api/invitations/{id}` | 撤销邀请 |
| GET / POST | `/api/projects/{id}/members` | 项目成员列表 / 添加成员 |
| DELETE | `/api/projects/{id}/members/{userId}` | 移出成员 |
| GET | `/api/runs/{id}/diff` | 成功/失败提交差异(成员不可见) |
| — | 项目/流水线/服务器/容器/应用商店/通知/AI/设置等其余端点 | 全部对成员关闭 |

---

## 4. 默认拒绝中间件 `memberGate`

实现见 [`internal/httpapi/membergate.go`](../internal/httpapi/membergate.go)。中间件套在
`requireAuth` 之后,依赖 context 中的会话:

1. **非普通用户**(管理员或异常)**不干预**,原样放行给后续 handler。
2. **普通用户**仅放行 `memberAllowlist` 命中的 `(method, 路径段)`;否则 403。
3. 白名单命中且路径含项目/运行 id 时,再校验**成员归属**;不合格 403。

匹配规则的两个关键点:

- **段精确**:路径按 `/` 切段,**段数必须完全一致**,不做前缀匹配。这样
  `/api/runs/{id}` 放行的同时,`/api/runs/{id}/diff` 不会被误放行——两者段数不同。
- **默认拒绝**:未来新增的任何端点,除非显式加入白名单,否则自动对普通用户关闭,
  无需维护黑名单,避免「忘记收紧」导致越权。

此外,管理端点另套 `requireAdmin`(仅 `role=admin` 通过),作为纵深防御的第二道闸。

---

## 5. 前端可见范围

前端镜像后端的默认拒绝语义,避免成员看到无权页面:

- **路由守卫**(`web/src/router/index.ts`):带 `meta.roles` 的路由须匹配当前角色;
  **没有** `meta.roles` 的路由对普通用户一律拒绝,重定向到其首页(成员为 `/my-projects`)。
  → 未来新增的管理页自动对成员不可见。
- **导航**(`AppShell.vue`):管理员与普通用户使用两套导航项;普通用户仅见
  「我的项目 / 运行」与底部「我的设置」。
- **页面**:
  - 成员:`MyProjects.vue`(只读项目列表)、复用 `Runs.vue` 作为运行列表、
    `MemberRunDetail.vue`(只读运行详情,复用 `RunStepList` / `RunTerminal` / `ArtifactList`,
    不含任何管理动作)、`MemberSettings.vue`(个人资料 / 改密 / 活跃会话 / 退出登录)。
  - 管理员:`Users.vue`(用户列表 + 邀请管理 + 项目成员管理)。
- **共享面板**:管理员与成员的账户设置复用 `ChangePasswordPanel.vue` /
  `ActiveSessionsPanel.vue`,职责单一、逻辑不分叉。
- **邀请注册页**:`Register.vue`(`/register?token=...`,公开路由),按邀请状态渲染
  表单或提示(无效 / 已用 / 过期)。

---

## 6. 数据模型与迁移(0073)

迁移文件:

- [`internal/store/migrations/sqlite/0073_identity.sql`](../internal/store/migrations/sqlite/0073_identity.sql)
- [`internal/store/migrations/mysql/0073_identity.sql`](../internal/store/migrations/mysql/0073_identity.sql)

新增/变更:

| 对象 | 说明 |
|---|---|
| `users` | 普通用户账号:`id` / `username`(唯一)/ `password_hash` / `role`(默认 `member`)/ `status` / 时间戳 |
| `invitations` | 一次性邀请:`id` / `token`(唯一)/ `created_by` / `expires_at` / `used_at` / 时间戳 |
| `project_members` | 项目×用户多对多成员关系(管理员不入此表) |
| `sessions` 加列 | `role` / `user_id` / `username` —— 会话携带身份,供权限校验与回显 |

兼容性:`sessions` 的既有行在 `ALTER ... DEFAULT` 后得到 `role='admin'`,升级平滑,
不打断在登录中的管理员会话。SQLite 与 MySQL 8.0 双方言均已覆盖。

> 若配置了 `PIPEWRIGHT_TEST_MYSQL_DSN`,测试会额外覆盖 MySQL 方言的迁移与新 SQL。

---

## 7. 安全注意事项

- **邀请 token 一次性**:注册成功即置 `used_at`,并默认 7 天过期;管理员可撤销未用邀请。
  过期/已用/不存在分别返回明确错误码,前端据此展示对应提示。
- **口令哈希复用 argon2id**:与管理员口令同一套实现(见 `internal/auth/password.go`),
  PHC 格式存储,绝不明文。
- **登录时序安全**:管理员与普通用户**分表查询、仅执行一次 argon2 校验**;失败路径
  使用 dummy hash 对齐耗时,消除「用户名是否存在」的侧信道。
- **默认拒绝**:`memberGate` 白名单 + 段精确匹配 + 归属校验,新端点自动对成员关闭。
- **不泄露存在性**:运行/项目不存在或查询失败时统一返回 403,不区分「无此资源」与「无权限」。
- **越权防护**:成员对非成员项目的运行列表、详情、日志、产物请求均被归属校验拦截;
  直接请求管理员端点(`/api/users`、`/api/runs/{other}/diff` 等)返回 403。
- **审计**:邀请创建、注册、成员增删等关键动作写入 append-only 审计;成员动作的 `actor`
  使用其用户名,历史审计记录语义不变。

---

## 8. 常见操作路径

- **邀请新用户**:左侧导航「用户管理」→「生成邀请」→ 复制注册链接 → 安全渠道发送。
- **撤销邀请**:「用户管理」→ 邀请列表 → 对「待使用」项点「撤销」。
- **把用户加入项目**:「用户管理」→ 选择项目 → 从下拉选择用户 →「添加成员」;
  「移出」可撤销访问。
- **成员自助**:登录后进入「我的项目」→ 点「运行」触发流水线 → 在「运行」中查看详情与日志;
  在「我的设置」中改口令、查看/撤销自己的会话、退出登录。