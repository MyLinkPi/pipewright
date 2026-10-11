# 用户体系：管理员邀请用户 + 普通用户项目成员权限

## 1. 摘要

在现有**单管理员**认证内核之上引入**普通用户（member）**角色，实现：

1. 管理员生成一次性**邀请链接**；被邀请者打开链接**自填用户名 + 口令**完成注册（注册后自动登录）。
2. 管理员把普通用户**加入项目**（项目成员）。
3. 普通用户仅可见：**自己加入的项目**、**触发这些项目的流水线运行**、**查看运行/日志**。其余功能对普通用户不可见（导航隐藏 + 后端 403）。

交付范围：后端 API + 完整前端界面（含 8 语言词条对齐）。

---

## 2. 现状分析（关键约束）

| 事实 | 位置 | 影响 |
|---|---|---|
| 认证为单管理员：`admin_user` 单行 `CHECK(id=1)` | [0002_auth.sql](file:///c:/Users/xufan/Trae/pipewright/internal/store/migrations/sqlite/0002_auth.sql) | 需新增 `users` 表承载普通用户，且不能破坏既有管理员路径 |
| `sessions` 无用户关联（token/csrf/时间） | 同上 | 需加列 `role` / `user_id` / `username` |
| `auth.Service.Login` 只查 `admin_user WHERE id=1` | [service.go](file:///c:/Users/xufan/Trae/pipewright/internal/auth/service.go#L146-L196) | 需扩展为「管理员 + 普通用户」双查，保持单次 argon2 校验（时序安全） |
| `ChangePassword` 硬编码 `id=1` | [service.go](file:///c:/Users/xufan/Trae/pipewright/internal/auth/service.go#L240-L285) | 需按会话身份定位用户 |
| 所有受保护 API 在同一个 `/api` chi 组，仅 `requireAuth`+`requireCSRF` | [router.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/router.go#L538-L546) | 需插入「成员门（默认拒绝）」中间件，避免逐个 handler 改签名 |
| `auditActor = "admin"` 硬编码 | [audit.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/audit.go#L17) | 手工运行时 actor 应取真实用户名 |
| 前端 8 语言 key 必须完全对齐（有测试强制） | [keyParity.test.ts](file:///c:/Users/xufan/Trae/pipewright/web/src/i18n/keyParity.test.ts) | 所有新增词条必须落到 8 个 locale |
| `Runs.vue` 仅含「列表 + 手动触发 + 参数」，无 cancel/approve 等管理动作 | [Runs.vue](file:///c:/Users/xufan/Trae/pipewright/web/src/views/Runs.vue) | **可安全复用**给普通用户 |
| `RunDetail.vue` / `Projects.vue` 含大量管理动作 | [RunDetail.vue](file:///c:/Users/xufan/Trae/pipewright/web/src/views/RunDetail.vue) | 不复用，另建精简只读成员页 |
| 迁移已到 `0072` | [migrations](file:///c:/Users/xufan/Trae/pipewright/internal/store/migrations) | 新迁移编号 `0073` |
| MySQL 主键需显式长度；`projects.id VARCHAR(64)`、`sessions.token VARCHAR(191)` | [0004_projects.sql](file:///c:/Users/xufan/Trae/pipewright/internal/store/migrations/mysql/0004_projects.sql) | 新表/FK 类型必须与之一致 |

---

## 3. 设计决策

- **角色模型**：两角色 `admin`（沿用 `admin_user`，保持既有行为与测试不变）/ `member`（新增 `users` 表）。不引入第三种角色。
- **普通用户可见范围**（已确认）：项目条目 + 运行 + 日志（最小面）。只额外放行「读取项目运行参数定义」——因为 `Runs.vue` 触发运行前会 `GET /api/projects/{id}/parameters`。流水线配置/代码/设置等一律不可见。
- **邀请方式**（已确认）：管理员生成**一次性**邀请链接（含有效期，默认 7 天，可配），被邀请者自填用户名 + 口令。链接用后失效；可手动撤销。
- **权限落地方式**：在受保护 `/api` 组内新增 **`memberGate` 中间件**——对 `member` 采取**默认拒绝**：仅放行白名单（method+路径模式），其余 403；并对项目/运行做**归属校验**。`admin` 直接放行。默认拒绝保证「未来新增端点自动对成员关闭」。
- **管理员账号不进入 `users` 表**，避免迁移既有 `admin_user` 带来的口令/引导/测试连锁改动（低风险、最小改动）。
- **普通用户前端独立精简页面**，不改动 94KB 的 `Projects.vue` 与 96KB 的 `RunDetail.vue`；复用 `Runs.vue`（本就不含管理动作）。

---

## 4. 数据层

新增迁移（两份，SQL 方言分叉）：

- [0073_identity.sql (sqlite)](file:///c:/Users/xufan/Trae/pipewright/internal/store/migrations/sqlite/0073_identity.sql)
- [0073_identity.sql (mysql)](file:///c:/Users/xufan/Trae/pipewright/internal/store/migrations/mysql/0073_identity.sql)

SQLite：

```sql
CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'member',
    status        TEXT NOT NULL DEFAULT 'active',
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS invitations (
    id         TEXT PRIMARY KEY,
    token      TEXT NOT NULL UNIQUE,
    created_by TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    used_at    TEXT,
    used_by    TEXT,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS project_members (
    project_id TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (project_id, user_id),
    FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE,
    FOREIGN KEY (user_id)    REFERENCES users (id)    ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_project_members_user ON project_members (user_id);

ALTER TABLE sessions ADD COLUMN role     TEXT NOT NULL DEFAULT 'admin';
ALTER TABLE sessions ADD COLUMN user_id  TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN username TEXT NOT NULL DEFAULT '';
```

MySQL：同结构，`VARCHAR(64)` 主键/外键、`VARCHAR(191)` token、`VARCHAR(32)` 时间、`VARCHAR(16)` role/status，`ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`；`ALTER TABLE sessions ADD COLUMN role VARCHAR(16) NOT NULL DEFAULT 'admin'` 等。

> 迁移执行器：SQLite 单事务；MySQL 逐句执行（`splitStatements` 按 `;` 切分）。本迁移无字符串内分号，兼容。
> 旧会话行在 `ALTER ... DEFAULT` 后会得到 `role='admin'`，天然向后兼容。

---

## 5. 后端实现

### 5.1 `internal/auth` 改造

**`session.go`**
- `Session` 新增字段：`Role string`、`UserID string`、`Username string`。
- `SessionStore.Create(role, userID, username string) (*Session, error)`：签名扩展，INSERT 时写入三列。
- `SessionStore.Get`：SELECT 增加 `role, user_id, username`，Scan 到新字段（用 `COALESCE` 兜底历史 NULL）。
- 其余方法（Delete/List/PurgeExpired/DeleteByIDPrefix/DeleteOthers）不变。

**`service.go`**
- `Login(username, password)` 改为双表查询、仍只做**一次** argon2 校验：
  1. 查 `admin_user WHERE id=1` 得 `(adminUsername, adminHash)`。
  2. 查 `users WHERE username=? AND status='active'` 得 `(memberID, memberUsername, memberHash)`（无则空）。
  3. 选定候选：用户名匹配管理员 → `(role='admin', userID='', hash=adminHash)`；否则匹配普通用户 → `(role='member', userID=memberID, hash=memberHash)`；否则 → `dummyHash`（保持时序一致）。
  4. 复用现有 `lockout.Guard` 单次校验；成功后 `sessions.Create(role, userID, username)`。
  - 新增：普通用户 `status != 'active'` 时不允许登录（视同凭据错误，避免枚举）。
- 新增哨兵错误：`ErrUsernameTaken`（供 identity 复用，可选）、不改动既有错误。
- `ChangePassword(current, new, currentToken)` → 改签名为 **`ChangePassword(role, userID, current, new, currentToken string) error`**：
  - `role='admin'` → 更新 `admin_user WHERE id=1`（现行为不变）。
  - `role='member'` → 更新 `users WHERE id=?`。
  - `DeleteOthers(currentToken)` 逻辑不变。
- `AdminUsername()` 保留（管理端回显仍用），新增能力由 `Session.Username/Role` 提供。
- `Authenticator` 接口**不变**（`Verify` 返回的 `*Session` 已带角色）。

### 5.2 新领域包 `internal/identity`

新增文件：`identity.go`（类型 + 接口 + 服务实现）、`store.go`（SQL）、`identity_test.go`。

类型：

```go
type User struct { ID, Username, Role, Status string; CreatedAt, UpdatedAt time.Time }
type Invitation struct { ID, Token, CreatedBy string; ExpiresAt time.Time; UsedAt *time.Time; UsedBy string; CreatedAt time.Time; State string } // pending|used|expired
```

接口方法（`Service`）：

- `CreateInvitation(ctx, createdBy string, ttl time.Duration) (*Invitation, error)`：`crypto/rand` 生成 32B hex token + uuid id；`ttl<=0` 用默认 `InvitationTTL = 7*24h`。
- `ListInvitations(ctx) ([]Invitation, error)`、`RevokeInvitation(ctx, id string) error`（删除；无行 → `ErrInvitationNotFound`）。
- `InvitationInfo(ctx, token string) (*Invitation, error)`：公开校验（不存在 → `ErrInvitationNotFound`；据 `used_at`/`expires_at` 计算 `State`）。
- `Register(ctx, token, username, password string) (*User, error)`：校验邀请有效（存在/未用/未过期）→ 校验用户名（3–32 位 `[A-Za-z0-9_.-]`，且不与 `admin_user.username` 及 `users.username` 冲突）→ 强度（复用 `auth.HashPassword`；长度 <8 → `ErrWeakPassword`）→ 事务内 INSERT user + 标记邀请 used → 返回 User。
- `ListUsers(ctx) ([]User, error)`、`GetUser(ctx, id string) (*User, error)`。
- `AddMember(ctx, projectID, userID string) error`（校验 user 存在；`DoNothing` 幂等 upsert）、`RemoveMember(ctx, projectID, userID string) error`（无行 → `ErrMemberNotFound`）。
- `ListProjectMembers(ctx, projectID string) ([]User, error)`。
- `ListUserProjectIDs(ctx, userID string) ([]string, error)`。
- `IsMember(ctx, userID, projectID string) (bool, error)`。

哨兵错误：`ErrInvitationNotFound`、`ErrInvitationUsed`、`ErrInvitationExpired`、`ErrUsernameTaken`、`ErrInvalidUsername`、`ErrWeakPassword`（复用 auth 的同名语义或本地定义并映射）、`ErrUserNotFound`、`ErrMemberNotFound`。

构造：`New(db *sql.DB) Service`。使用 `store.Dialect`/`store.Excluded` 等方言 helper 保证 MySQL 幂等（`ON DUPLICATE KEY`）。密码哈希直接调用 `auth.HashPassword`。

### 5.3 `internal/httpapi`

**新增 `options.identity identity.Service` + `WithIdentity(s identity.Service) Option`**（[router.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/router.go#L72-L120)）。

**新增文件 `internal/httpapi/identity.go`**（DTO + handlers）：

公开（注册进现有 `/api/auth` 组，见 [router.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/router.go#L519-L526)）：
- `POST /api/auth/register`：body `{token, username, password}` → 调 `identity.Register` → 成功后**自动登录**（`auth.Service.Login(username, password)`）并下发会话/CSRF cookie（抽公共 helper `setSessionCookies(w, r, sess)`，供 login 与 register 复用）→ 201 `{username, role:"member"}`。
  - 错误映射：邀请无效 404 `invitation_invalid`；已用 409 `invitation_used`；过期 410 `invitation_expired`；用户名占用 409 `username_taken`；用户名非法 422 `invalid_username`；弱口令 422 `weak_password`。
- `GET /api/auth/invitations/{token}`：公开返回 `{state}`（`pending|used|expired`），供注册页展示状态；不存在 404。

管理员（注册在受保护 `/api` 组；`memberGate` 已默认拒绝成员，额外用 `requireAdmin` 做纵深防御）：
- `GET /api/users` → `{users:[{id,username,role,status,createdAt}]}`
- `POST /api/invitations` → 201 `{id, token, expiresAt, state}`（前端据 token 拼邀请链接 `location.origin + '/register?token=' + token`）
- `GET /api/invitations` → 列表（含 state）
- `DELETE /api/invitations/{id}` → 204
- `GET /api/projects/{id}/members` → 成员用户列表
- `POST /api/projects/{id}/members` body `{userId}` 或 `{username}` → 204/201
- `DELETE /api/projects/{id}/members/{userId}` → 204

所有写操作成功后 `recordAudit`（新增审计动作常量，见 5.5）。

**新增权限中间件**（`internal/httpapi/membergate.go` + `membergate_test.go`）：

```go
func memberGate(id identity.Service, runs run.Service, next http.Handler) http.Handler
```

逻辑：
1. 取 `sessionFromContext`；`Role != "member"` → 直接 `next`。
2. 若 `(method, path)` 不在**白名单** → 403 `{error:{code:"forbidden"}}`。
3. 白名单内若含项目维度（`/api/projects/{id}/...`）→ 取 `{id}` 校验 `IsMember`；不通过 403。
4. 若为运行维度（`/api/runs/{id}...`，GET 单条/events/logs/artifacts/download）→ `runs.Get(id)` 取 `ProjectID` → 校验 `IsMember`；run 不存在 → 404 语义（交给后续 handler 亦可，这里 403 覆盖）。
5. 通过 → `next`。

白名单（method + 段模式；`*` 匹配单段）：

```
GET    /api/account/sessions
POST   /api/account/password
DELETE /api/account/sessions/*
GET    /api/projects
GET    /api/projects/*/parameters
POST   /api/projects/*/runs
GET    /api/runs
GET    /api/runs/*
GET    /api/runs/*/events
GET    /api/runs/*/logs
GET    /api/runs/*/artifacts
GET    /api/runs/*/artifacts/*/download
```

> 说明：`GET /api/runs/*` 覆盖 run 详情；`/api/runs/*` 与 `/api/runs/*/events` 的匹配需**最长模式优先**，实现用「按段数精确匹配 + 精确尾段」而非前缀，避免 `/api/runs/{id}/diff` 被 `/api/runs/*` 误放行。因此 `/api/runs/*` 需额外判定「路径恰为 3 段」，即 `/api/runs/{id}`。实现与单测需覆盖此点。

在 `router.go` 受保护组内、`requireCSRF` 之后插入：

```go
ar.Use(func(next http.Handler) http.Handler { return memberGate(o.identity, o.runs, next) })
```

**既有 handler 适配**：
- [makeListProjectsHandler](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/projects.go#L199-L228)：当 `session.Role=='member'` 时，改用 `identity.ListUserProjectIDs` 得到 id 集合，调用新增的 `project.Service.ListPagedByIDs(ctx, ids, page, pageSize)` 仅返回加入的项目（分页/总数按过滤后计）。
- [makeListRunsHandler](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/runs.go#L207-L242)：当为成员时，将 `identity.ListUserProjectIDs` 结果填入 `run.ListFilter.AllowedProjectIDs`（见 5.4）；`projectId` 查询参数若非成员所属则视为空集。
- [makeManualRunHandler](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/webhooks.go#L110-L168)：`Actor` 与审计 `Actor` 取 `sess.Username`（不再硬编码 `"admin"`）；非成员仍为管理员用户名。
- `makeLoginHandler` / `makeSessionHandler`（[router.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/router.go#L1177-L1281)）：响应体加入 `role`（`map[string]string{"username":..,"role":..}`）；`makeSessionHandler` 用 `sess.Username`/`sess.Role` 回显，不再调 `AdminUsername()`。
- `makeChangePasswordHandler`（[account.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/account.go#L58-L100)）：`accountService.ChangePassword` 接口签名同步为 `(role, userID, current, new, currentToken string) error`，handler 从 `sessionFromContext` 取 `Role/UserID`。
- 新增 `requireAdmin(next)` 中间件（读 session，`role!="admin"` → 403），仅用于 5.3 的管理员端点（纵深防御）。

### 5.4 `internal/run` 与 `internal/project` 小幅扩展

- `run.ListFilter` 增加 `AllowedProjectIDs []string`（[run.go](file:///c:/Users/xufan/Trae/pipewright/internal/run/run.go#L273-L278)）；`service.List`（[service.go](file:///c:/Users/xufan/Trae/pipewright/internal/run/service.go#L563-L618)）中若非空则追加 `pr.project_id IN (?,...)`；空切片且已设该字段 → 直接返回空结果（不生成非法 `IN ()`）。
- `project.Service` 增加 `ListPagedByIDs(ctx, ids []string, page, pageSize int) (*ListResult, error)`；实现 `WHERE p.id IN (...)` 分页 + count。空 `ids` → 空结果。同步更新 `project.Service` 接口与实现；检查是否存在测试替身需补实现（当前 httpapi 测试均用真实 service，无 stub）。

### 5.5 `internal/audit`

`audit.go` 新增动作常量（只增不改）：`ActionInvitationCreate`、`ActionInvitationRevoke`、`ActionUserRegister`、`ActionProjectMemberAdd`、`ActionProjectMemberRemove`；`TargetUser = "user"`、`TargetInvitation = "invitation"`。

### 5.6 装配 `cmd/pipewright/main.go`

- 在 `authSvc` 之后新增：`identitySvc := identity.New(st.DB)`。
- `httpapi.New(...)` 追加 `httpapi.WithIdentity(identitySvc)`。

---

## 6. 前端实现

### 6.1 session store 与 API 层

- [stores/session.ts](file:///c:/Users/xufan/Trae/pipewright/web/src/stores/session.ts)：`SessionUser` 增加 `role: 'admin' | 'member'`；新增 getter `isAdmin`（`role === 'admin'`）。
- [api/auth.ts](file:///c:/Users/xufan/Trae/pipewright/web/src/api/auth.ts)：`SessionUser` 同步；新增 `register(token, username, password)`、`fetchInvitation(token)`。
- 新增 `web/src/api/users.ts`：`listUsers`、`createInvitation`、`listInvitations`、`revokeInvitation`、`listProjectMembers`、`addProjectMember`、`removeProjectMember`。

### 6.2 路由 [router/index.ts](file:///c:/Users/xufan/Trae/pipewright/web/src/router/index.ts)

- 新增公开路由 `{ path: '/register', name: 'register', component: Register, meta: { public: true } }`。
- 新增成员路由（`meta: { requiresAuth: true, roles: ['member'] }`）：
  - `/my-projects` → `MyProjects`
  - `/my-runs` → 复用 `Runs.vue`
  - `/my-runs/:id` → `MemberRunDetail`
  - `/my-settings` → `MemberSettings`（普通用户专属设置界面，**不挂**管理员 `Settings.vue` 外壳）
- 新增管理员路由（`meta: { roles: ['admin'] }`）：`/users` → `Users`。
- 路由守卫扩展：会话结果带 `role`；若 `to.meta.roles` 存在且不含当前角色 → 重定向到各自首页（admin → `/dashboard`，member → `/my-projects`）。管理员访问 `/my-*`、成员访问管理路由均被拦。

### 6.3 导航 [AppShell.vue](file:///c:/Users/xufan/Trae/pipewright/web/src/layouts/AppShell.vue)

- 计算 `isAdmin = sessionStore.user?.role === 'admin'`。
- `navItems` 拆分为 `adminNavItems`（现有全部）+ `memberNavItems`：
  - `nav.myProjects → /my-projects`
  - `nav.runs → /my-runs`
  - `nav.mySettings → /my-settings`（普通用户设置入口）
- 模板据角色渲染对应列表；`settingsItem`（管理员设置外壳）仅管理员可见。

### 6.4 新增页面

- `web/src/views/Register.vue`：读取 `route.query.token`；调 `fetchInvitation` 展示状态（pending/used/expired/不存在）；表单（用户名/口令/确认口令）→ `register()` → `sessionStore.setUser` → 跳 `/`。样式复用 `Login.vue` 的沉浸式深色外壳（同套 CSS 变量，避免新增视觉体系）。
- `web/src/views/MyProjects.vue`：`listProjects()`（后端已按成员过滤）只读表格（名称/仓库/默认分支/最近运行状态），行内「运行」按钮 → 跳 `/my-runs?projectId=<id>`。无创建/编辑/删除入口。
- `web/src/views/MemberRunDetail.vue`：`getRun(id)` + `subscribeRunEvents` + `getRunLogs` + `getRunArtifacts`；复用 [RunStepList.vue](file:///c:/Users/xufan/Trae/pipewright/web/src/components/run/RunStepList.vue) 与 [RunTerminal.vue](file:///c:/Users/xufan/Trae/pipewright/web/src/components/run/RunTerminal.vue)（只读），顶部返回按钮指向 `/my-runs`。不调用 deploy/approve/diagnose/diff/promotions/servers。
- `web/src/views/Users.vue`（管理员）：三个区块
  1. 用户列表（`listUsers`）；
  2. 邀请管理：生成（`createInvitation` → 展示完整链接 `origin + '/register?token='` + 复制）、列表（含状态/过期时间）、撤销（`revokeInvitation`，二次确认）；
  3. 项目成员管理：选择项目（`listProjects`）→ 展示成员（`listProjectMembers`）→ 添加（下拉选择用户）/移除。
- `web/src/views/MemberSettings.vue`（**普通用户设置界面**）：独立页面，**不挂**管理员 `Settings.vue` 外壳（避免暴露 12 项管理员子导航）。包含四个区块：
  1. **个人资料**：用户名（只读，来自 `sessionStore.user.username`）+ 角色徽章（普通用户）；说明文案「账号由管理员邀请创建」。
  2. **修改密码**：当前口令 / 新口令 / 确认口令 → `changePassword`；错误码 `invalid_current_password`（401）/ `weak_password`（422）内联映射；成功后提示并刷新会话列表（服务端已吊销其它会话）。
  3. **我的活跃会话**：`listSessions` 列表（当前会话标记）+ 单条撤销 `revokeSession`（`ConfirmDialog` 二次确认）。
  4. **退出登录**：与 AppShell 一致（确认 → `logout` → 清缓存 → 跳 `/login`）。
  仅调用 `GET /api/account/sessions`、`POST /api/account/password`、`DELETE /api/account/sessions/{id}`（均在 `memberGate` 白名单内，作用于**当前用户自身**，无越权风险）。

**共享组件抽取（管理员与普通用户复用同一套账户逻辑）**：

现有 [SettingsAccount.vue](file:///c:/Users/xufan/Trae/pipewright/web/src/views/settings/SettingsAccount.vue) 已自包含「身份展示 + 改密 + 会话列表」，但含管理员语义（`username` 默认 `'admin'`、顶部注释写 admin、含「重新引导」按钮）。为避免逻辑分叉：

- 抽取两个无状态子组件到 `web/src/components/settings/`：
  - `ChangePasswordPanel.vue`：改密表单 + 错误映射 + 成功回调（props：无；emit：`updated`）。
  - `ActiveSessionsPanel.vue`：会话列表 + 撤销（props：无；emit：`revoked`）。
- [SettingsAccount.vue](file:///c:/Users/xufan/Trae/pipewright/web/src/views/settings/SettingsAccount.vue) 改为组合这两个子组件（**行为保持不变**，仅内部结构改变，继续保留其「重新引导」按钮）；`username` 由 `sessionStore.user?.username` 提供（不再默认 `'admin'`）。
- `MemberSettings.vue` 同样组合这两个子组件，另加个人资料卡与退出登录。
- 两个子组件的文案**复用现有 `settingsAccount.*` 词条**（8 语言已齐备），无需重复翻译。

### 6.5 既有页面适配

- [Runs.vue](file:///c:/Users/xufan/Trae/pipewright/web/src/views/Runs.vue)：详情跳转目标按角色选择 `run-detail`（管理员）或 `my-run-detail`（成员）；成员隐藏「项目筛选」中未加入项（后端已过滤，前端无需额外处理）。
- [SettingsAccount.vue](file:///c:/Users/xufan/Trae/pipewright/web/src/views/settings/SettingsAccount.vue)：重构为组合抽取出的两个面板（见 6.4），外部行为与对外接口不变。

### 6.6 i18n（8 语言对齐）

- 基础文件 `locales/<lang>.ts`（8 份）：`nav` 命名空间新增 `myProjects`、`mySettings`、`users`。
- 新增命名空间目录文件（每命名空间 8 份）：
  - `locales/<lang>/register.ts`：注册页词条 + 邀请状态文案。
  - `locales/<lang>/users.ts`：用户/邀请/项目成员管理词条。
  - `locales/<lang>/myProjects.ts`：成员项目页词条。
  - `locales/<lang>/myRunDetail.ts`：成员运行详情页词条。
  - `locales/<lang>/mySettings.ts`：普通用户设置页**仅新增**个人资料/角色徽章/退出登录/页面标题等词条；改密与会话区**复用 `settingsAccount.*`**，不新增 key。
- zh-CN 与 en 提供真实译文；其余 6 语言提供对应译文（可先英文回退值，保证 key 集合一致，`keyParity.test.ts` 通过）。

---

## 7. 测试

后端（Go，表驱动 + 现有 `storetest` 夹具，兼容 SQLite/MySQL 双方言）：

- `internal/auth`：会话含 role/user/username；管理员登录行为不回归；普通用户登录成功/失败/被禁用；`ChangePassword` 对 admin 与 member 各自生效且其它会话失效。
- `internal/identity/identity_test.go`：邀请创建/列表/撤销/过期/已用；注册成功、用户名占用、弱口令、用户名非法、邀请不存在/已用/过期；成员增删查幂等与 `IsMember`/`ListUserProjectIDs`。
- `internal/httpapi/identity_test.go`：`/api/auth/register` 全错误分支 + 自动登录 cookie；`/api/auth/invitations/{token}` 状态；管理员端点鉴权（成员访问 403）；成员访问白名单内/外端点（含 `/api/runs/{id}/diff` 被拒）；项目归属越权 403。
- `internal/httpapi/membergate_test.go`：白名单匹配（尤其 `/api/runs/{id}` vs `/api/runs/{id}/diff` 的段精确性）、项目/运行归属校验。
- `internal/httpapi/runs_test.go` / `projects_test.go`：成员列表按成员项目过滤、`AllowedProjectIDs` 为空返回空结果。
- `internal/run`、`internal/project`：`AllowedProjectIDs` 过滤与 `ListPagedByIDs` 分页单测。

前端（vitest）：
- `stores/session` 角色派生 `isAdmin`。
- `Register.vue`：邀请状态与提交分支。
- `Users.vue`：生成链接/复制/撤销。
- `MemberRunDetail.vue`：只读渲染与返回。
- `MemberSettings.vue`：个人资料渲染、改密成功/错误映射、会话撤销。
- `ChangePasswordPanel.vue` / `ActiveSessionsPanel.vue`：抽取后行为与既有 `SettingsAccount` 测试保持一致（同步迁移/保留原测试）。
- 新增/修改 API 层单测（`web/src/api/users.test.ts`）。
- `i18n keyParity.test.ts` 必须通过（新词条 8 语言齐全）。

---

## 8. 文档

- 更新 `README.md` / `README.zh-CN.md`：新增「用户与权限」小节（角色说明、邀请流程、成员可见范围、管理员操作路径）。
- 在 `docs/` 下新增 `user-management.md`：角色模型、邀请/注册时序、权限矩阵（端点 × 角色）、默认拒绝中间件说明、迁移说明（0073）、安全注意（token 一次性、口令哈希复用 argon2id、成员默认拒绝）。
- 迁移文件顶部注释说明版本与目的（与既有迁移风格一致）。

---

## 9. 验证步骤

1. 后端编译：`go build ./cmd/pipewright`
2. 后端静态检查：`go vet ./cmd/... ./internal/...`
3. 后端测试：`go test ./internal/auth/... ./internal/identity/... ./internal/httpapi/... ./internal/run/... ./internal/project/... ./internal/store/...`
   - 若配置了 `PIPEWRIGHT_TEST_MYSQL_DSN`，重跑以覆盖 MySQL 方言迁移与新 SQL。
4. 前端类型检查：`cd web && npm run typecheck`
5. 前端单测：`cd web && npm run test`（含 `keyParity`）
6. 前端构建：`cd web && npm run build`
7. 端到端手工验证：
   - 管理员登录 → `/users` 生成邀请链接。
   - 无痕窗口打开链接 → 注册 → 自动登录 → 仅见「我的项目 / 运行 / 设置」。
   - 成员进入 `/my-settings` → 个人资料/改密/会话列表/退出登录均可用；管理员 `/settings` 外壳与其 12 项子导航对成员不可见。
   - 管理员把该用户加入项目 A → 成员可见项目 A 并触发运行、查看日志。
   - 成员直接请求 `/api/users`、`/api/runs/{other}/diff` → 403。
   - 撤销未用邀请后访问注册链接 → 失效提示。

---

## 10. 假设与决策

- 仅两种角色；管理员仍是单账号（`admin_user`），不迁移进 `users` 表。
- 邀请链接**一次性**、默认有效期 7 天；被邀请者自填用户名 + 口令。
- 成员可见范围严格为「项目 + 运行 + 日志」，例外仅放行读取项目运行参数定义（触发运行表单所需）。
- 成员前端使用独立精简页面；不改动 `Projects.vue`、`RunDetail.vue`。
- 普通用户设置使用独立页面 `/my-settings`（复用抽取出的改密/会话面板），不暴露管理员设置外壳。
- 6 个非 zh-CN/en 语言的新词条允许先以英文占位，以满足 key 对齐测试。
- 审计 `actor` 对成员使用其用户名；历史审计记录的 actor 语义不变。

---

## 11. 风险与缓解

| 风险 | 缓解 |
|---|---|
| 改 `SessionStore.Create` / `ChangePassword` 签名破坏既有测试 | 同步更新 `auth` 与 `httpapi`（account）相关测试；先跑 `go test ./internal/auth/...` |
| `memberGate` 路径匹配漏洞（前缀误放行） | 采用**段精确**匹配 + 单测覆盖边界（`/api/runs/{id}` vs `/api/runs/{id}/diff`）；默认拒绝 |
| MySQL 迁移 FK 类型不匹配 | FK 列类型与 `projects.id VARCHAR(64)`、`users.id VARCHAR(64)` 一致；有 `PIPEWRIGHT_TEST_MYSQL_DSN` 时跑双方言迁移测试 |
| 成员误触管理端点导致页面报错 | 前端按角色渲染导航与路由守卫；后端 403 兜底 |
| 8 语言词条遗漏 | 依赖 `keyParity.test.ts` 强制校验，必须全绿才能合并 |
