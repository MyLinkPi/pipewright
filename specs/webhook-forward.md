# Webhook 集群转发(跨环境接收)

> 状态:已实现。相关:`internal/httpapi/webhook_forward.go`(入口侧转发器)、`internal/httpapi/webhooks.go`(公网入口 + 共享头解析)、`internal/httpapi/mesh_inbound.go`(对端接收)、`internal/mesh/client.go`(`ForwardWebhook`)、`web/src/components/TriggersPanel.vue`(端点环境选择)。

## 语义

多环境集群(mesh)下,Git 平台(Gitee/Gitea/Gogs)常常只够得到其中某一台机器。任一节点收到 `POST /api/webhooks/{token}` 且 token **不属于本环境**时,把原始投递(头白名单 + body)经 mesh 签名通道转发给各对端;token 所属环境(唯一持有签名密钥)完成验签/防重放/去重/触发,响应原样透传回 Git 平台:

```
Git 平台 ──POST /api/webhooks/{token}──▶ 入口节点 A(本地无此 token)
   A ──POST /api/mesh/webhooks/{token}(ed25519 节点签名)──▶ 对端环境 B…N(并发)
   B(归属)── 验签+去重+创建运行,响应 ◀── 200 {accepted:true,runId}
Git 平台 ◀── A 透传 B 的 status+body(附 X-PW-Webhook-Forwarded-For: B 环境名)
```

- **归属判定完全在持有触发配置的对端本地完成**:转发节点不需要(也拿不到)任何密钥;非归属对端一律 404。
- **授权**:对端按 mesh ACL 的 `trigger_allowed` 授权单决定是否接受转发(默认全拒绝——需在归属环境的「集群 → 授权」中把入口节点加入「允许触发」)。转发即触发,语义与 `mesh_trigger` 节点/跨环境运行触发一致。
- **UI**:项目「触发设置 → Webhook 接入」的接收端点支持下拉切换环境地址复制(`GET /api/projects/{id}/trigger/webhook-endpoints`,仅列出**登记了 publicUrl** 的对端;选择记忆在 localStorage `pw.webhookEnv.<projectId>`)。

## 入口侧行为(`WebhookForwarder`)

- 候选对端 = `mesh_peers` 中 VirtualIP 非空且非本节点;mesh 未启用/无对端 → 不转发(维持原 404)。
- 首次投递**并发 fan-out**(整体超时 8s):对端 404(无此 token)/ 403(未获授权,**与 token 无关**,无法与「非归属」区分,只能忽略)/ 传输错误一律忽略;第一个确定性响应(200 或 401 等)采纳并取消其余在途请求。多个 `accepted:true` 竞态时优先采纳(实际不发生:token 全局唯一由随机生成保证)。
- `accepted:true` 后缓存 token→归属环境(TTL 10 分钟,容量 1024 防御性清空),后续投递直达归属环境;直达失联/404 则失效缓存并回退 fan-out。
- 结果分类:命中 → 透传对端 status+body;全部对端不可达 → 502 `webhook_forward_unreachable`;仍找不到归属 → 维持本地 404。
- 头透传白名单:`X-Gitee-*` / `X-Gitea-*` / `X-Gogs-*` + `Content-Type`(与节点签名头 `X-PW-Mesh-*` 无前缀冲突;body 完整性由签名串中的 bodyHash 绑定)。请求体上限沿用 1 MiB。

## 对端接收(`POST /api/mesh/webhooks/{token}`)

- 挂载在根路由(与公网 webhook 同级,豁免 session/CSRF),`meshAuth`(节点 ed25519 验签 + ±5min 防重放)+ `meshRequire(permTrigger)`。
- 与公网入口**共用同一套** `webhookDelivery` 头解析(Gitee→Gitea→Gogs fallback)与 `trigger.Receiver.Handle`:验签、`webhook_deliveries` 幂等去重、事件/分支/路径过滤、创建运行全部在对端完成,响应契约同公网(`{accepted,runId}` / `{accepted:false,ignored}` / 401 / 404)。
- **防环**:该端点直达 receiver,不经过公网 handler,不会再转发;不做 A→B→C 链式转发(节点注册表经发现循环全集群一致,入口节点总能直达归属环境)。

## 安全边界

- 转发节点成为「开放中继」的风险由归属侧最终把关:入口节点不知密钥,伪造投递在归属环境验签处失败(401 透传);防重放 5 分钟窗 + 归属侧 `webhook_deliveries` 唯一约束去重兜底跨节点重放。
- 体积放大有限(1 MiB 上限 + 8s 整体超时 + fan-out 仅对注册表内对端)。
- 无数据库迁移:token 归属在对端本地 DB 判定,归属缓存为进程内存。

## 实现位置

- `internal/httpapi/webhook_forward.go`:`WebhookForwarder`(fan-out/缓存/结果分类)+ `relayWebhookToMesh`(响应透传)。
- `internal/httpapi/webhooks.go`:`webhookDelivery`(共享头解析)、`writeWebhookResult`(共享响应写出)、公网 handler 的 `ErrTokenNotFound` 分支接入转发。
- `internal/httpapi/mesh_inbound.go`:`POST /api/mesh/webhooks/{token}`(trigger gate;`meshDeps.webhookReceiver` 经 `NewMeshDeps` 注入)。
- `internal/mesh/client.go`:`ForwardWebhook`(签名转发,返回原始 status+body 不做 2xx 映射)。
- `internal/httpapi/triggers.go`:`GET /api/projects/{id}/trigger/webhook-endpoints`(项目成员可用,只回 nodeId/name/base,不暴露 admin 面完整字段)。
- 装配:`cmd/pipewright/main.go`(`WithWebhookForwarder`;mesh 未启用时转发器自动降级为纯本地)。
- 前端:`web/src/components/TriggersPanel.vue`(环境下拉/记忆/提示)、`web/src/api/triggers.ts`(`getWebhookEndpoints`)、i18n 8 语言 `projectPanels.ts`(`envLocal`/`envSelectAria`/`forwardHint`)。

## 测试

- Go(`internal/httpapi/webhook_forward_test.go`,双节点真实 HTTP 端到端):入口无 token → 转发 → 透传 accepted+runId+归属注记头;归属验签失败 → 401 透传;全对端无此 token → 404;ACL 未授权(403 不可区分)→ 404;对端全不可达 → 502;归属缓存命中;mesh 入站端点未签名 401 / 默认拒绝 403 / 授权后头透传处理;webhook-endpoints 列表(publicUrl 过滤)。
- 前端:`vue-tsc` typecheck + 生产构建。
