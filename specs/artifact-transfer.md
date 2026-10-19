# 产物跨环境传输(artifact transfer)

> 状态:已实现。相关:`shared-build-cluster.md`(集群共享构建)、mesh_push / pushed_source 节点(`internal/build/mesh_nodes.go`)。

## 语义

产物管理页(admin)可把某个运行的产物**手动传输**到 mesh 集群里的另一个环境:

- **复用 mesh_push 推送通道,接收侧零改动**:对端经既有 `POST /api/mesh/push` 接收——blob 内容寻址去重(preflight 只传缺失段)、镜像归档 `docker load` + 推对端内置 registry(同 digest 跨推送复用)、按 (origin 节点, origin 项目) 幂等定位/自动创建「被推送源」项目并落**修订**。传输过去的产物在对端经 `pushed_source` 节点 / 触发运行消费。
- **一次传输 = 对方项目的一个修订**:弹窗内可勾选同 run 的多个产物一起传输(点击的产物默认勾选),语义与 mesh_push 流水线节点一致;修订标签可选,留空由接收端生成(`rev-<originRunId>`)。
- **发送核心单一代码路径**:`build.pushArtifactsToPeer`(manifest 组装 → 镜像 EnsureLocal + docker save 导出 → preflight 去重 → multipart 直传)同时服务 mesh_push 节点与本功能。镜像先 EnsureLocal:手动传输时镜像可能已被本机 daemon 清理,内置 registry 可直拉兜底。
- **对端授权**:对端 ACL `push_allowed` 把门,未授权 → 403,UI 提示「需在对端设置 → 集群授权」。

## 可传输性判定(与下载按钮同口径 + 收紧)

| 产物形态 | 可传 | 说明 |
|---|---|---|
| image(reference 非空) | ✅ | docker save 归档直传;digest 随清单带给对端去重 |
| 文件类 `format=file` / `tar.gz`(stored=true) | ✅ | reference 即制品库句柄,流式直传 |
| `format=files`(packMode=none 多文件清单) | ❌ | 接收模型一条目一 blob,无法忠实还原,显式拒绝(mesh_push 节点对它同样只传首文件,属既有限制) |
| 未归档占位(stored≠true / 无引用) | ❌ | 与下载同口径 |

## 端点(admin 组,产物管理面)

| 端点 | 作用 |
|---|---|
| `POST /api/runs/{id}/artifacts/transfer` | body `{nodeId, artifactIds[], revisionLabel?}` → `{transferId}`;同步校验(run/产物/对端存在、可传输性、并发上限),传输异步执行 |
| `GET /api/artifacts/transfers/{transferId}` | 状态轮询:`running/success/failed/canceled` + 阶段文本 + 结果(对方项目/修订/深链) |
| `DELETE /api/artifacts/transfers/{transferId}` | 取消进行中的传输(docker save 经 ctx 终止;已终态 → 409) |

错误映射:run/产物/对端不存在 → 404;不可传输 → 400 `artifact_not_transferable`(人读原因);对端无虚拟 IP → 409;并发上限(4)→ 429;依赖未装配 → 503。

## 边界与取舍

- **任务表进程内存态**(同共享构建任务表):服务重启即丢,前端轮询 404 → 提示重新发起;对端内容寻址去重,重传只补缺,幂等安全。
- **硬超时 60 分钟**(对齐 mesh_trigger 等待上限);终态条目保留 30 分钟、上限 100 条(惰性剪枝)。
- **并发上限 4**:镜像导出占本机磁盘/带宽;终态留审计 `runs.artifact.transfer`(经 `OnSettled` 回调由装配层记录,build 不依赖 audit)。
- **手动传输不修改本端产物记录**:传输是只读消费(镜像导出/docker pull 除外),删除/保留策略不受影响。

## 实现位置

- 发送核心 + 任务服务:`internal/build/artifact_transfer.go`(`pushArtifactsToPeer` 共享核心 + `ArtifactTransferService`);mesh_push 节点委托同一核心(`mesh_nodes.go`)。
- HTTP 面:`internal/httpapi/artifact_transfer.go`(admin 组挂载,见 `router.go`)。
- 装配:`cmd/pipewright/main.go`(构造服务、接审计回调、`WithArtifactTransfer`)。
- 前端:`web/src/views/Artifacts.vue`(行操作/详情弹窗入口 + 传输弹窗:环境选择 / 产物勾选 / 进度轮询 / 取消 / 结果深链);API client 在 `web/src/api/artifactAdmin.ts`。
