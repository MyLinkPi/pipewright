# PVE 集群升级（日常 apt + 仓库管理 + 大版本 8→9）

> 状态:已实现。相关:`internal/pveadmin/`(领域包:upgrade/aptrepo/upgrade_major + client/probe 复用)、`internal/httpapi/pveadmin_upgrade.go`(HTTP 面)、`web/src/views/PVEUpgrade.vue` + `web/src/components/pve/{UpgradeProgress,MajorUpgradeWizard,MajorUpgradeProgress,AptRepoPanel}.vue`(前端)、迁移 `0117_pve_upgrade_jobs.sql`。

## 语义

「PVE」页(admin)新增**升级管理**入口(`/pve/upgrade`),三条风险分级的功能线:

- **A 日常 apt 升级(读/刷新纯 API,升级步 SSH)**:更新矩阵逐节点聚合版本/运行内核/待更新包/订阅/需重启标记(`GET /nodes/{n}/version|status|apt/update|apt/versions|subscription`,并发 4 扇出、错误隔离)。刷新索引 `POST apt/update` 收 UPID(前端经既有 `GET /task` 轮询)。升级任务照 `createJob`:每集群一个内存任务、readonly 闸门、与建机/大版本互斥(`ErrBusy`)、`go runUpgrade`(`context.Background`)、**严格串行**(corosync/pve-cluster 包更新会重启服务,并行有丢 quorum 风险)、**逐节点独立容错**(一台失败不阻断后续)。每节点:刷新索引(waitTask 10min)→ 重取待更新数(0 则跳过)→ **SSH `apt-get dist-upgrade`**(`env DEBIAN_FRONTEND=noninteractive` + confdef/confold 保留旧配置,60min;**PVE API 无 apt/upgrade 自动化端点**——GUI 的「升级」是交互式 xterm 控制台,POST 返回 501,实测确立,与 ceph/大版本同款 SSH 语义)→ 内核需重启检测(`apt/versions` 的 Running vs Version,边界安全前缀比较)。**dist-upgrade 前仓库体检**(SSH 读清单):suite 与运行代号不符(含 stable 系滚动别名漂移)或未启用任何 PVE 源 → 拒绝该节点升级并给出修正指引(清单读不到时放行,由 apt-get 自身兜底)——实测教训:pvetools 风格 `stable-security` 在 2026-11 已指向 trixie,混入 bookworm PVE 8 会让解析器卸载 proxmox-ve,被 pve-apt-hook 拦截。重启是**单独显式动作**(`RebootNode`,confirm 须原样等于节点名),绝不自动重启。
- **B apt 仓库管理(SSH)**:固定 sh 脚本遍历 `/etc/apt/sources.list` + `sources.list.d/*.list|*.sources`,`##PWFILE/##PWEND` 标记分隔后 Go 解析(双格式:one-line 与 deb822);条目按 URI/路径/组件分类 debian|pve|ceph|other。三种写动作 toggle(启停)/mirror(镜像预设)/preset(企业↔无订阅),全部「重读最新 → Go 变换 → `cp -a` 备份 → `Runner.WriteFile` 写 tmp → `mv -f` 原子替换 → `apt-get update` 验证」(`fstabCommit` 同形),中途失败尽力回滚。镜像预设复用 `cloudinit.go mirrorHosts`:USTC/清华的 proxmox 镜像带 `/proxmox` 路径前缀;**Debian 源按 suite 语义规范化路径**(参考 pvetools 行为语义,净室重写:`*-security`/`stable-security`/`*/updates` → `/debian-security`,其余 → `/debian`,ubuntu → `/ubuntu`,ports → `/ubuntu-ports`),原始行路径缺失/非标准(如裸主机 `http://security.debian.org`)时换源后仍产出可用规范 URL,绝不把损坏路径带到镜像站;**stable 系滚动 suite(stable-security 等)钉到运行代号**(它随 Debian 稳定版漂移,2026-11 起指向 trixie,与 bookworm PVE 8 混用会触发卸载 proxmox-ve,被 pve-apt-hook 拦截——实测;testing 等非稳定别名不动);**非官方预设切源后补齐缺失条目**(pvetools 完整配置语义):PVE 非订阅源禁用则启用、缺失则按镜像基址新建 `pve-no-subscription.list`(阿里云无 proxmox 镜像 → 官方 download.proxmox.com),Debian backports 完全缺失才补建(组件沿用受管理主源,已有含禁用不动,尊重显式配置);**阿里云无 proxmox 镜像,PVE 源保持官方**;**企业源不被镜像改写**(镜像站不承载订阅源);**Ceph 源只读**。restore 从备份恢复(备份名白名单)。批量 `apply` 仅 mirror/preset(节点无关动作),并发闸 4、逐节点结果隔离。
- **C 大版本 8→9(SSH 滚动 + 持久化)**:忠实还原官方流程(pve.proxmox.com/wiki/Upgrade_from_8_to_9)。**预检**(同步,`WriteTimeout=0` 可行):quorate/节点在线、版本 8.4+(**9.x 节点提前跳过全部检查、不参与升级目标**,混合集群/续跑可用)、**待更新数 0(先做日常升级)**、代号 bookworm、根分区 ≥5GB(`df -B1 --output=avail /`)、**Ceph 检测(`test -f /etc/pve/ceph.conf`)命中即阻断**、企业源无订阅阻断、**无任何启用的 PVE 源阻断**(否则 dist-upgrade 只升 Debian 包,pveversion 仍 8.x,节点必失败——仓库管理「应用镜像」可自动补建)、运行中任务阻断、`pve8to9 --full` FAIL 阻断/WARN 须 ack;两节点无 qdevice、自托管、HA 在用为须 ack 警告。报告缓存 30min。**启动**:confirm 须等于集群名 + 新鲜预检无 FAIL + 警告全 ack + 快速复查 quorate/Ceph + **任一节点离线即拒绝**(避免静默漏升级)。**节点状态机**(严格串行、每步落库):guests(shutdown 逐台优雅关机 waitTask 5min,超时即节点失败**不强停**;warn 仅日志列出将被硬停的 guest)→ sources(复用 B 的解析/备份/写,debian+pve 条目 bookworm→trixie)→ apt update(15min)→ upgrading(`env DEBIAN_FRONTEND=noninteractive apt-get -y -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold dist-upgrade`,60min)→ pve8to9 复检 + pveversion 须 9.x → rebooting(API command=reboot,不 waitTask;先记重启前 uptime)→ waiting(两阶段:先等下线 10min,再等回线 + quorate + SSH 可达 15min,**并校验 uptime 已重置,防 SSH 瞬断误判「已重启」**)→ verifying(9.x + online + quorate)→ done → 下一节点;全部完成 → 终验全节点 9.x。**fail-fast**:任一节点失败立即暂停,绝不带病推进。**中止只在步骤边界生效,绝不中断 dist-upgrade**(防 dpkg 半配置)。retry 重跑失败节点(幂等);resume 从首个未完成节点续跑。

## 端点(/api/pve/clusters/{id}/*,admin 组)

| 端点 | 作用 |
|---|---|
| `GET .../updates` | 更新矩阵(只读) |
| `POST .../updates/refresh` | `{nodes?}` → 202 `{items:[{node,upid}]}`。审计 `pveadmin.upgrade.refresh` |
| `POST .../upgrade` | `{nodes?}` → 202(异步串行任务)。审计 `pveadmin.upgrade.start` |
| `GET .../upgrade-status` | 日常升级快照 |
| `POST .../nodes/{node}/reboot` | `{confirm}` → 202 `{node,upid}`。审计 `pveadmin.node.reboot` |
| `GET .../nodes/{node}/apt-repos` | 仓库清单 + 警告(只读) |
| `POST .../nodes/{node}/apt-repos/toggle` | `{path,key,enabled}` → 写入结果。审计 `pveadmin.apt.toggle` |
| `POST .../nodes/{node}/apt-repos/mirror` | `{preset}` → 写入结果。审计 `pveadmin.apt.mirror` |
| `POST .../nodes/{node}/apt-repos/preset` | `{preset}` → 写入结果。审计 `pveadmin.apt.preset` |
| `POST .../nodes/{node}/apt-repos/restore` | `{path,backupPath}` → 写入结果。审计 `pveadmin.apt.restore` |
| `POST .../apt-repos/apply` | `{action,preset,nodes?}` → `{items}`(批量)。审计 `pveadmin.apt.apply` |
| `GET .../major-upgrade/preflight` | 同步预检报告(只读) |
| `POST .../major-upgrade` | `{confirm,acks,guestPolicy,nodes?}` → 202。审计 `pveadmin.upgrade.major.start` |
| `GET .../major-upgrade-status` | 大版本快照(DB-backed) |
| `POST .../major-upgrade/abort` | 中止(步骤边界生效)。审计 `pveadmin.upgrade.major.abort` |
| `POST .../major-upgrade/retry` | `{node,confirm}` → 202。审计 `pveadmin.upgrade.major.retry` |
| `POST .../major-upgrade/resume` | `{confirm}` → 202。审计 `pveadmin.upgrade.major.resume` |

错误映射:入参非法 → 422 `invalid_pve_input`;输入确认不匹配 → 422 `pve_confirm_mismatch`;只读 → 409 `pve_readonly`(**拒绝也留审计**);任务占用 → 409 `pve_busy`;预检未满足 → 409 `pve_preflight_required`;Ceph → 409 `pve_ceph_manual`;无提权 → 409 `pve_no_privilege`(带 sudoers 指引);SSH 命令失败 → 502 `pve_ssh_failed`;PVE API 失败 → 502 `pve_connect_failed`;未配 Token → 409 `pve_cluster_not_ready`。审计 detail 只含 node/preset/action/confirm 结果等非密字段,**绝不含 Token/凭据**。

## 边界与取舍

- **只做 8→9**:7→8 不做(7 已 EOL);检测到非 8.x/9.x 版本预检阻断。
- **Ceph 集群人工处理**:检测到 `/etc/pve/ceph.conf` 即阻断大版本升级(官方要求先升 Ceph 到 Squid 19.2);仓库管理里 Ceph 源只读。不做 noout 自动化。
- **guest 策略仅 warn/shutdown**:不做自动迁移;shutdown 超时即节点失败,绝不强停。
- **日常升级串行**:不做并行模式(quorum 风险);逐节点容错继续(区别大版本 fail-fast)。
- **仓库管理不做自定义源增删改**:仅启停 + 镜像预设 + 订阅预设;路径白名单(拒穿越)、单文件 64KiB 上限、条目 key 为文件内序号(写前重读防 TOCTOU)。
- **大版本任务持久化,日常任务内存态**:大版本横跨 1-2h 与多次重启,控制端重启后 DB running 无内存对应 → interrupted,**只允许人工 resume/retry,绝不自动续跑**;日常升级照 createJob 内存态。
- **apt API allowtoken**:实现时按官方 apidoc 逐端点验证(建机 spec 同款纪律);若 `apt/upgrade` 拒绝 API Token,日常升级回退 SSH `apt-get dist-upgrade`(ceph 先例),接口形状不变。
- **pve8to9 输出解析**:容错正则(PASS/FAIL/WARN/SKIP 前缀 + TOTAL 汇总),fixture 单测;真机输出校准留待 E2E。
- **GPL 纪律**:pve8to9 只在节点上调用,不复制其代码;Go 净室实现。
- **不做**:PBS 协同升级、opschat AI 暴露、强制 deb822 modernize-sources、日常 job 持久化。

## 实现位置

- 领域包 `internal/pveadmin/`:`upgrade.go`(更新矩阵扇出/刷新/串行 job/重启/内核需重启启发式/resolveUpgradeNodes/fanOutNodes)、`aptrepo.go`(双格式解析/分类/镜像/订阅/suite 变换/备份原子写/restore/批量/osReleaseCodename)、`upgrade_major.go`(预检/pve8to9 解析/Ceph 检测/持久化 job/滚动状态机/waitNodeBack/fail-fast/retry/resume/abort)、`client.go`(apt/订阅/节点状态响应模型 + apiResource.HAState)、`pveadmin.go`(Service +17 方法、job maps、anyJobRunningLocked 跨类型互斥、DeleteCluster 守卫与连带清理、新错误哨兵)、`create.go`(busy 检查改 anyJobRunningLocked)。
- HTTP 面:`internal/httpapi/pveadmin_upgrade.go`(17 handler + mountPVEUpgrade + 审计)、`pveadmin.go`(mount 调用 + writePVEAdminError 扩展)。
- 迁移:`0117_pve_upgrade_jobs.sql`(sqlite/mysql 成对;pve_upgrade_jobs + pve_upgrade_job_nodes,job 外键 CASCADE;集群删除领域层连带清理)。
- 前端:`web/src/api/pveUpgrade.ts`(client + 类型)、`web/src/views/PVEUpgrade.vue`(集群选择 + 三 tab)、`web/src/components/pve/{UpgradeProgress,MajorUpgradeWizard,MajorUpgradeProgress,AptRepoPanel}.vue`、`web/src/views/PVE.vue`(升级管理入口)、`web/src/router/index.ts`(/pve/upgrade)、`web/src/i18n/locales/*/pveUpgrade.ts`(8 语言,keyParity 强制)。

## 验证

- 单测:`internal/pveadmin/{aptrepo,upgrade,upgrade_major}_test.go`(双格式解析 fixture、镜像/订阅/suite 变换、备份→原子写序列、路径白名单、Ceph 只读、日常管线序列/readonly/busy/重启确认、pve8to9 解析、预检阻断、启动闸门、有状态 fake 滚动序列、fail-fast+retry、持久化 interrupted+resume、中止步骤边界)、`internal/httpapi/pveadmin_upgrade_test.go`(端点契约/错误码/审计含拒绝)。
- `go build ./...` + `go vet` + `go test ./internal/pveadmin/... ./internal/httpapi/... ./internal/store/...`;web:`npm run build`(vue-tsc)+ vitest keyParity。
- 真机 E2E(一次性 8.4 测试集群):登记→解锁→矩阵→刷新索引→日常升级→重启→仓库启停/换源/撤销/批量→预检→双节点大版本滚动→终验 9.x;Ceph 集群验证阻断路径。