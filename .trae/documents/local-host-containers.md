# 本机(控制节点)容器:/containers 页面的控制节点一等目标

## 概要(Summary)

[/containers](file:///c:/Users/xufan/Trae/pipewright/web/src/views/Containers.vue) 的卡片列表**置首**
新增一张**「本机(控制节点)」**卡,展示**运行 Pipewright 的这台主机**上的 Docker 容器,并与
远程服务器卡**能力等同**:容器列表 / 实时 stats / 详情 inspect / 生命周期(起停重启暂停 kill 删除) /
日志(含 SSE 实时) / 容器内交互终端 / 镜像 / Compose Stacks / 卷 / 网络 / 一键清理 / AI 诊断 /
新增容器 —— 只是命令**在控制节点本地执行**(`os/exec`),不经 SSH。

## 架构(Architecture)

核心是**一个装饰器**,而不是给每个 handler 加分支:

```
target.WithLocalHost(base Service) Service
   ├─ serverID == "__local__" → 本机执行(os/exec;unix 上 PTY 走 creack/pty)
   └─ 其它 serverID            → 逐字透传给 base(SSH,零行为变化)
```

容器域的既有 handler 全部只通过 `svc.Get/Exec/ExecStream/ExecInteractive/Upload/ExecWithStdin`
触达目标机([server_containers.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/server_containers.go)、
[server_container_stats.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/server_container_stats.go)、
[container_inspect.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/container_inspect.go)、
[container_terminal.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/container_terminal.go)、
[container_diagnose.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/container_diagnose.go)、
[container_create.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/container_create.go)、
[server_stacks.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/server_stacks.go)、
[server_ops.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/server_ops.go)、
[server_logs.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/server_logs.go)),因此
**无需改动任何 handler**:[router.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/router.go)
把容器域路由的 `sv` 换成 `csv := target.WithLocalHost(sv)` 即可。

## 关键设计点

1. **id 三处同源**:`target.LocalHostID`(后端) == `api/servers.ts` 的 `LOCAL_HOST_ID`(前端) ==
   本机状态卡牌 DTO 的 `serverId`(上一功能)。任何一处改字面量都会失配,故后端以常量互相引用。
2. **「命令不存在」不是「不可达」**:本机不可能连接失败,因此命令无法启动时返回**伪退出码**
   127(不存在)/126(不可执行)+ stderr、**不返回 error**。这样容器运行时探测会走既有的
   「连得上但没有容器运行时」降级(`runtime:""` + 人读提示),而不是误报「服务器不可达」。
3. **本机不是可登记实体**:`Get` 返回合成 `Server`(ID/名称/localhost/当前用户,无任何凭据字段),
   `Update`/`Delete` 返回 `ErrLocalHostImmutable`,`List` **不含**本机(批量端点、构建机池、
   部署目标选择都不受影响)。登记域路由(`/api/servers`、`/servers/{id}`、`/test`)刻意**不套**
   装饰器 —— 用 `__local__` 访问仍是 404。
4. **PTY 平台分支**:容器内交互终端需要真 TTY(`docker exec -it`),故 Unix 平台用
   `github.com/creack/pty` 分配伪终端(与 SSH PTY 行为一致:stdout/stderr 合流、支持 resize);
   非 Unix(Windows)平台返回 `ErrLocalInteractiveUnsupported`(**仅**终端不可用,
   `Exec`/`ExecStream`/`Upload` 不受影响)。
5. **Upload 落本地文件**:Stacks 的「保存 compose」经 `Upload` 写盘,本机实现为流式写
   `remotePath`(自动建父目录,0644),与 SSH 版语义一致。
6. **安全(AC-SEC-02 不变量)**:命令仍是 `[]string`(程序 + 参数)直接交给 `exec.Command`,
   不经 shell → 无注入面;本机执行不涉及 SSH 凭据 → 无凭据泄漏面。写操作仍走既有的
   CSRF + 审计;容器名/镜像/网络名等参数白名单仍在 handler 层生效(未改动)。

## 修改清单

后端:

- 新增 [local.go](file:///c:/Users/xufan/Trae/pipewright/internal/target/local.go):`LocalHostID`、
  `WithLocalHost` 装饰器、合成 Server、本地 `Exec`/`ExecWithStdin`/`ExecStream`/`Upload`/`Test`、
  流式 `localStream`。
- 新增 `local_pty_unix.go` / `local_pty_other.go`:PTY 会话的平台分支。
- [router.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/router.go):容器域路由改用 `csv`。
- [server_metrics.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/server_metrics.go):
  `localHostServerID` 改为引用 `target.LocalHostID`。
- 依赖:`github.com/creack/pty v1.1.24`(纯 Go,仅 Unix)。

前端:

- [Containers.vue](file:///c:/Users/xufan/Trae/pipewright/web/src/views/Containers.vue):并行拉取本机容器
  (失败只隐藏本机卡)、本机分组置首、卡片标题/主机文案走 i18n。
- [ServerTerminal.vue](file:///c:/Users/xufan/Trae/pipewright/web/src/views/ServerTerminal.vue):
  本机终端顶栏显示「本机(控制节点)」/「localhost · 控制节点」,不把 `__local__` 露给用户。
- 8 语言 `containers.ts` 新增 `localName` / `localHost`。

## 测试

- Go [local_test.go](file:///c:/Users/xufan/Trae/pipewright/internal/target/local_test.go):合成本机视图、
  不可改删、本机执行/透传、命令缺失 → 127、非零退出、空命令、ctx 取消、stdin、流式、Upload、Test。
- Go `local_pty_unix_test.go`:PTY 输出可读、resize、双向读写(真实执行结果与回显区分)、Close 幂等。
- Go [local_containers_test.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/local_containers_test.go):
  `/servers/__local__/containers` 与 `/images` 返回 200 且 `reachable:true`(不依赖测试机是否有 docker);
  未知 id 仍 404;登记域端点不认本机;未登录 401。
- 前端 [Containers.local.test.ts](file:///c:/Users/xufan/Trae/pipewright/web/src/views/Containers.local.test.ts):
  本机卡置首且与服务器卡并存、本机请求失败只隐藏本机卡、只有本机数据时不显示空态、不可达时降级提示。
- e2e:共享 stub 增加 `__local__` 容器与 stats 样例(刻意「有运行时但无容器」,避免本机卡遮挡
  首张服务器卡的 `.first()` 动作按钮);`ops-chat.spec.ts` 的容器页 fixture 同步补齐。

## 已知限制

- **非类 Unix 控制节点**:本机容器交互终端不可用(PTY 缺失);其余容器能力正常。
- **私有镜像**:本机 `DockerLogin` 未实现(装饰器拿不到 vault),拉取私有镜像需先在控制节点手工
  `docker login`;公开镜像拉取不受影响。
- **AI 助手(OpsChat)与应用商店的目标选择**仍只列**登记服务器**,不含控制节点(后端
  `/servers/{id}/apps/deploy` 已支持本机,但前端入口未暴露)。
- **服务器状态页的批量命令**仍只针对登记服务器(本机状态卡牌保持只读)。
- 本机容器操作以**控制节点进程身份**(通常 root 或 docker 组成员)执行,权限等同该用户。
