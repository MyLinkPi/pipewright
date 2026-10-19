# 集群级共享构建机(cluster shared build)

> 状态:已实现(0086)。相关:`runner-pool-test-plan.md`(本端构建机池)、`deploy-target-selector.md`(部署目标选择器)。

## 语义

同一 mesh 集群里,环境 A(所属方/owner)可把某台构建机标记为「集群共享构建机」;环境 B(发起方)用 `peer:<A 的环境名>` 选择器把**构建**节点派到 A 的这台机上执行:

- **排队权威在 A**:B 提交的任务进入 A 的构建机调度器(`runner.Scheduler`)同一 FIFO,与 A 自己的任务统一按 priority/亲和/负载调度——跨环境任务排队由此保证。B 端不占本地槽位。
- **凭据不出 A**:SSH 凭据只由 A 解密使用;B 的 registry 凭据只留在 B。A 对 B 的任务只出「调度 + SSH 执行 + 字节中转」。
- **源码从 B 上传**:B 本地克隆(含恢复上游产物/被推送源物化)后打包,经 mesh(multipart 流式 + sha256 校验)传给 A;A 落 OS 临时文件、上机成功即删。
- **产物回 B、不进 A**:script 产物按 B 渲染的 artifactPath 在 A 的机器上打 tar 回拉给 B,由 B 分类(jar/dist/archive)归档进**B 的制品库**;build_image 在 A 的机器 build + `docker save`,归档回传 B,B `docker load` 后用自己的凭据按自己的推送计划 push。A 全程不写自己的制品库。
- **部署不受影响**:shared 标记只影响"谁能用这台机构建"。A 的构建/部署行为零变化;B 不能部署到 A 的机器(部署目标选择器含 `peer:` 项 → 显式 422)。

## 配置面

- **机器标记**:`servers.shared_build`(服务器页开关;要求 kind=standard 且 labels 非空——对端靠标签选中该机。PVE 放置会自动清除该标记)。
- **对端授权**:mesh ACL 第 5 张授权单 `build_allowed`(集群页授权矩阵,默认全拒绝)。
- **发起方选择器**:`peer:<环境名|nodeID>`(≤1 个,可与标签项、`server:<id>` 钉死组合),如 `peer:prod,arch=arm64` / `peer:prod,server:<id>`。不含 `peer:` 的选择器永远只在本端选机,零行为变化。仅支持 script / build_image 节点(build_image 远程仅 image 产物,与自有池远程构建同边界)。

## 协议(wire 契约:`internal/mesh/shared_build.go`)

| 端点(A 端,签名 + build ACL) | 作用 |
|---|---|
| `GET /api/mesh/build/catalog` | 共享机目录(调度元数据,脱敏;供钉死校验/预览) |
| `POST /api/mesh/build/jobs` | multipart:spec 段(≤1MB)+ workspace 段(流式,sha256 校验)→ `{jobId}` |
| `GET /api/mesh/build/jobs/{id}/events` | SSE:历史日志回放 + 实时日志/状态,终态即收流(15s 心跳) |
| `GET /api/mesh/build/jobs/{id}` | 状态轮询兜底(404 = 过期/对端重启) |
| `GET /api/mesh/build/jobs/{id}/result` | 产物归档流式回拉(头带 sha256/大小) |
| `DELETE /api/mesh/build/jobs/{id}` | 取消(排队撤销/运行中断,幂等) |

owner 管理面:`GET/DELETE /api/build/shared/jobs/{id}`(admin)——跨环境任务列表与强制取消(设置 → 共享构建机页)。

## 实现位置

- A 端运行时:`internal/build/mesh_shared_server.go`(内存任务表;`AcquireShared` 取机;script 直跑 driver;build_image 复用 `Builder.build` 换远程 driver 后 `docker save`)。
- B 端派发:`internal/build/mesh_shared_client.go`(执行层分流缝在 remote_stage_exec.go 的 dispatch / dispatchRemote 两处);产物回收复用 `collectScriptArtifacts` 与 `pushImageArtifact`。
- 取机约束:`runner.Scheduler.AcquireShared`(候选只含共享机;钉死未共享机 → `ErrServerNotShared`)。
- 新 SSH 原语:`target.Service.Download`(远程 `cat` stdout 流回,读尽后收割退出码)。

## 边界(文档/UI 已明示)

- 共享机会执行发起环境的命令并接收其注入的环境变量(含 secret);传输经节点签名 + EasyTier(WireGuard)。A 不落盘 spec、日志只驻内存 ring。
- 工作区/镜像归档跨环境传输受 EasyTier 链路带宽限制(流式,无全量内存驻留)。
- 任务表/暂存为进程内存+临时文件态:A 重启即丢(发起方轮询 404 判失败),与本平台"调度态进程内存"的既有语义一致;终态产物保留 15 分钟供回拉。
- 队列上限 32 个在册任务(防宿主临时盘打爆;满则受理拒绝 429)。
- 瘦控制机(无本地容器 CLI)的发起方不支持共享 build_image(load 需要);script 类不受影响。
