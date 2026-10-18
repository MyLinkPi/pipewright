新增「Redis 管理」域：实例注册（设置页）+ 键浏览管理（顶级页）+ 应用商店部署 Redis 自动登记。完整复刻「数据库管理（dbadmin）」的架构模式。

## 范围（已确认）

INFO 概览 + SCAN 游标分页浏览键（模式匹配）+ 键详情（类型/TTL/值预览/内存）+ 删除键/改 TTL（默认只读锁保护，解锁后可写，过审计）。不做命令控制台与键值编辑器。

## 后端

### 0. 依赖
引入 `github.com/redis/go-redis/v9`（go.mod 现无任何 redis 客户端）。

### 1. 新包 `internal/redisadmin/`
- **`redisadmin.go`**（领域+存储+服务，照 dbadmin.go 单文件模式）：
  - `Instance{ID,Name,Host,Port,DBNum,ReadOnly,Source(manual|appstore),ServerID,StackName,CreatedAt,UpdatedAt}`；密码经 `vault.SealSecret/OpenSecret` 密文 BLOB 落库（支持无密码实例，password_sealed 可空）
  - 错误集：ErrNotFound/ErrInvalidInput/ErrVaultUnconfigured/ErrReadonly/ErrUnreachable/ErrKeyNotFound
  - `Service`：`List/Get/Create/Update/Delete/Test/TestDirect/SetReadOnly/Register` + `Info/ScanKeys/KeyDetail/DeleteKey/SetTTL`
  - `Register` 幂等：同 host+port 命中 → 原地轮换密码（Updated=true）；未命中 INSERT source=appstore readonly=1；并发窗口撞唯一索引按更新语义重试（照抄 dbadmin.Register，含 isUniqueViolation）
- **`conn.go`**：go-redis 客户端即建即弃（拨号 3s/读写 5s 超时）；describeRedisError 人话映射
- **`scan.go`**：
  - `Info(db)`：INFO server,memory,clients,keyspace + DBSIZE → {version, uptimeSeconds, usedMemory, connectedClients, keyspace{db→keys}}
  - `ScanKeys(cursor,match,count)`：**循环 SCAN 补齐至 count 或游标归零**（键空间可能百万级，绝不 KEYS *；COUNT 钳 50–500 默认 100）
  - `KeyDetail(key)`：TYPE→TTL→MEMORY USAGE（尽力而为）→ 按 type 值预览：string GETRANGE 0..2047+STRLEN（截断标记）、hash HLEN+HSCAN 100、list LLEN+LRANGE 0..99、set SCARD+SSCAN 100、zset ZCARD+ZRANGE 0..99 WITHSCORES、其他类型仅元信息
  - `DeleteKey`/`SetTTL`（ttl<0 = PERSIST）：ensureWritable 只读守卫（默认锁定，ErrReadonly）
- **迁移** `0083_redis_instances.sql`（sqlite+mysql 双方言）：表 redis_instances + 唯一索引 (host,port)
- **领域测试**：client 经接口注入 fake（不引 miniredis，与 dbadmin 测试纪律一致）——覆盖 CRUD/幂等 Register/只读守卫/SCAN 补齐分页/键详情各类型/错误映射

### 2. HTTP 层 `internal/httpapi/redisadmin.go` + 装配
- `mountRedisAdmin(ar, svc, aud)`，挂 requireAdmin 子组（同 dbadmin）
- 端点：GET/POST `/redis-instances`；POST `/redis-instances/test`（直连）；PUT/DELETE `/redis-instances/{id}`；POST `/{id}/test`；PUT `/{id}/readonly`；GET `/{id}/info?db=`；GET `/{id}/keys?db=&cursor=&match=&count=`；GET `/{id}/keys/detail?key=&db=`；DELETE `/{id}/keys?key=&db=`；POST `/{id}/keys/ttl`（写操作过审计：redisadmin.key.delete / redisadmin.key.ttl；键名入审计目标，密码绝不入）
- `WithRedisAdmin` Option + router.go 挂载 + main.go 装配（redisadmin.New(st.DB, credVault)）
- httpapi 测试：实例 CRUD 无网路径

### 3. 应用商店联动（apps.go）
- `appAutoRegisterRedisTemplate = "redis"` + `registerRedisInstance(...)`：取 `used["password"]`/`used["port"]`（无 username；redis 模板已存在：requirepass + port 参数，AutoGenerate 密码）；登记名 `Redis · 服务器名 [· 实例]`；vault 未配置 → 备注不阻断
- `makeDeployAppHandler` 签名加 redisAdmins（router.go:909 同步）；响应加 `redisRegistered`/`redisNote`；审计 `redisadmin.instance.register`

## 前端

### 4. `web/src/api/redisAdmin.ts`
类型（RedisInstance/RedisTestResult/RedisServerInfo/ScanResult/KeyDetail）+ 全端点函数（照 dbAdmin.ts 模式）

### 5. 顶级页 `web/src/views/Redis.vue`（/redis，admin，照 Databases.vue 风格）
- 实例下拉 + 只读/写模式 chip + 解锁/上锁（确认弹窗）+ 连接测试
- INFO 概览条（版本/uptime/内存/连接数/当前库键数）+ DB 切换下拉 0-15（有键数的库标注键数）
- 键浏览：match 输入（回车/按钮重扫）+ **SCAN 游标分页**（cursor 历史栈，上一页/下一页，页大小 100/200/500；游标归零即末页）
- 键详情面板：类型徽章/TTL/内存 + 值预览（string 等宽文本、hash/list/set/zset 列表渲染，截断提示）+ 操作：删除（确认）、改 TTL（秒数输入，-1=持久化）
- loading/错误重试/空态

### 6. 设置页 `web/src/views/settings/SettingsRedis.vue`（/settings/redis）
新增表单（name/host/port 默认 6379/password 可空/db 0-15）+ 直连测试 + dirty guard；列表 + 编辑（密码留空不改）+ 删除确认 + 逐行测试；Settings.vue 加子导航

### 7. 路由/导航/联动展示/i18n
- router：`/redis`（roles: admin）+ `/settings/redis`
- AppShell adminNavItems 加 nav.redis（图标用 @vicons/tabler 的 BrandRedis，不存在则 Bolt）；8 语言 nav + settingsHub.navRedis 词条
- 8 语言新建 `locales/<lang>/redisAdmin.ts`
- apps.ts DeployResult 加 redisRegistered/redisNote；AppStore.vue 部署结果跟随 dbRegistered 模式展示 + appStore 词条

## 验证
`go mod tidy` + `go build ./...` + `go test ./internal/redisadmin/... ./internal/httpapi/`；`vue-tsc --noEmit` + `vitest run`（keyParity）+ `vite build`