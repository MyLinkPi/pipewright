# 服务注册网关支持多台机器(配置完全一致,兼容存量升级)

## 兼容策略(核心)

- `service_reg_settings` 新增 `server_ids` 列(JSON 数组,`TEXT/LONGTEXT NOT NULL DEFAULT '[]'`)+ `last_apply_errors` 列(JSON 对象 serverId→error)。
- **读**:旧行 `server_ids` 为空 → 回退 `[server_id]`(存量部署升级后零配置直接可用)。
- **写**:`server_id` 始终同步为列表第一台(旧二进制读新库仍工作,可安全降级),`server_ids` 写全量 JSON。
- httpapi DTO 保留旧字段(`serverId`/`installed`/`running` 等映射到第一台),新增 `serverIds`、`servers[]`;前端同仓库一起升级。

## 1. 存储层(internal/servicereg/store.go + 新迁移 0064)

- 新迁移 `0064_service_reg_multi_gateway.sql`(sqlite + mysql 两份,沿 0007 的 JSON 列先例:mysql 用 `LONGTEXT NOT NULL DEFAULT ('[]')` / `('{}')`)。
- `scanSettings` 读 `server_ids`/`last_apply_errors` 并做空回退;`updateSettings`/`getOrCreateSettings` 写两列;`setApplyResult` 扩展为可带每台错误 JSON。

## 2. 领域层(internal/servicereg)

- `Settings.ServerID` → `ServerIDs []string`;`SettingsUpdate` 新增 `ServerIDs *[]string`(保留 `ServerID *string` 作旧入口,等价于单元素列表;空列表 = 清空转配置态);`validateSettings` 增加去重/非空校验。
- `applyWith` 重构:加载 domains/services/instances 与 enabled/tcpPorts 一次,**逐台循环** `applyWithServer(ctx, st, serverID, ...)`(现函数体参数化:探活剔除、connectUpstream、deployCerts、按本机存活实例渲染、nginx -t → cp → reload)。单台失败记录并继续,最后聚合成 `serverId: err` 列表返回;每台结果写入 `last_apply_errors`,`last_apply_error` 存聚合文案,`last_apply_at` 存整轮结束时间。
- `GetGateway` 逐台 `inspectNginx` + `tg.Get` 取名字 → `GatewayStatus` 新增 `Servers []GatewayServerStatus{ServerID, ServerName, Installed, Running, Image, Ports}`,旧平铺字段填第一台。
- `ResolveDeployInstances`:`st.ServerID != serverID` 改为 `!slices.Contains(st.ServerIDs, serverID)`(部署联动:任一网关机上的应用部署/滚动交换照常工作;各机按本机存活实例收敛,与现行为一致)。
- `RemoveGateway` 逐台移除容器(聚合一台失败不阻断其它台)。
- 已知边界(不改动,保持现模型):实例表 `UNIQUE(service_id, container)` 假设服务内容器名全局唯一——同一服务以相同容器名部署到多机的场景不在本次范围。

## 3. HTTP API(internal/httpapi/servicereg.go)

- settings DTO 加 `serverIds []string`;更新请求加 `ServerIDs *[]string`(兼容旧 `serverId`)。
- gateway DTO 加 `servers[]`(每台 installed/running/image/ports/serverName),旧字段保留填第一台。路由与 handler 签名不变。

## 4. 前端(web/)

- `api/servicereg.ts`:`Settings`/`SettingsUpdate`/`Gateway` 类型同步。
- `ServiceRegistry.vue`:主机单选 AppSelect 改为**复选列表**(照抄 SettingsRegistry.vue 的 checked-Set 模式,listServers 数据源);网关状态区改为逐台列表(每台:名字 + running 徽标 + 端口 + 最近收敛错误);部署按钮 `serverIds.length > 0` 才可用。
- i18n:`serviceReg.*` 新增/调整文案,zh-CN 与 en 全量,其余 6 语言补关键 key(fallback 到 zh-CN)。

## 5. 测试

- 迁移兼容:手写旧行(`server_ids=''`)→ GetSettings 回退 `[server_id]`;storetest 双方言。
- 领域层:双网关 apply 逐台执行且单台失败不阻断(扩展 fakeTarget 记录 serverID,按机器注入失败);ResolveDeployInstances 对第二台网关匹配;多台聚合错误文案。
- 既有用例 `SettingsUpdate{ServerID: strPtr("srv1")}` 经旧入口继续通过(兼容回归)。
- 全量 `go test ./...` + 前端 `vue-tsc` 类型检查。

## 交付顺序

迁移/store → 领域层 → httpapi → 前端 → 测试补齐 → 全量验证。不 commit(除非你要求)。