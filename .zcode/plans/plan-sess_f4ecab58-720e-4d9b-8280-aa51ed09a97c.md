# 服务器磁盘管理功能（diskadmin）实现方案

## 一、调研结论（现状与可复用地基）

- 项目所有目标机操作走 **SSH argv 数组执行**（`target.Service.Exec`，AC-SEC-02 纪律：绝不拼 shell 字符串），无 Agent 概念，磁盘功能零存量、无冲突。
- 可直接复用的现成模式：
  - **提权执行**：`internal/ceph/exec.go` 的 `Runner`（root 直跑；否则 `sudo -n true` 探测免密 sudo 后加前缀，按服务器缓存 10 分钟，失败带 sudoers 指引）——照抄为 diskadmin 私有实现。
  - **lsblk 解析**：`internal/ceph/bootstrap.go` 的 `checkDevices`（`lsblk -J` + 宽容 JSON walk）与 `ceph/parse.go` 的 `parseJSON/asObj/asArr/pickStr` 工具集。
  - **只读闸门**：`internal/redisadmin` 的 `readonly` 默认 + `ErrReadonly` 哨兵 + 独立解锁端点 + 审计。
  - **配置持久化**：`internal/registryhub/daemon.go` 的「备份 → 校验 → 原子替换 → 失败回滚」范式（fstab 写入照此）。
  - **单次合并采集脚本**：`internal/httpapi/server_metrics.go` 的 `##PW:` 分段标记模式（发现接口一次 SSH 拉全拓扑）。
  - 前端：`useConfirm`（confirmText 逐字输入确认）、自研 data-table、api client 模块 + 8 语言 i18n namespace、顶级页面惯例（Redis/PVE/Ceph 先例）。
- 权限：`/api/servers/*` 不在 memberGate 白名单，member 默认 403；磁盘管理挂 admin 子组（requireAdmin），与现有基础设施管理页一致。

## 二、范围与文件系统能力矩阵

| 能力 | ext4 | XFS | Btrfs |
|---|---|---|---|
| 格式化 | ✅ `mkfs.ext4 -F` | ✅ `mkfs.xfs -f` | ✅ `mkfs.btrfs -f` |
| 扩容 | ✅ `resize2fs`（在线/离线均可） | ✅ `xfs_growfs`（**须已挂载**） | ✅ `btrfs filesystem resize max`（须挂载） |
| 缩容 | ✅ **仅离线**：须未挂载（用户自行卸载后操作），链路 `e2fsck -f` → `resize2fs` → `parted resizepart` → `resize2fs` 回填 | ❌ 不支持（前后端都禁用并标注） | ✅ **在线**：`btrfs filesystem resize` → `parted resizepart` |

LVM 同理套用该矩阵（LV 的扩缩在 fs 层之下做 `lvextend`/`lvreduce`，fs 层复用同一套扩缩例程）。

## 三、架构与新增文件

新领域包 `internal/diskadmin`（对齐 redisadmin/ceph 三层惯例），**无 DB 迁移**（操作无状态；解锁状态为进程内 TTL 安全联锁，重启自动回锁，比落库更安全）。

后端新增：
- `internal/diskadmin/diskadmin.go` — 领域模型（BlockDevice 树 / PV / VG / LV / Topology / UnlockState）、Service 接口、哨兵错误（ErrReadonly / ErrNoPrivilege / ErrInvalidInput / ErrDeviceNotFound / ErrDeviceBusy / ErrProtectedDevice / ErrFSTypeUnsupported / ErrNotLastPartition / ErrLvmMissing / ErrExec…）
- `internal/diskadmin/exec.go` — 提权 Runner（抄 ceph/exec.go，含 privTTL 缓存与 tailOutput）
- `internal/diskadmin/parse.go` — 宽容 JSON 工具（按 ceph/mesh 先例包内私有）
- `internal/diskadmin/probe.go` — 拓扑发现：**单次 SSH** 跑 `sh -c` 合并脚本（`##PW:` 分段）：`lsblk -J -b -o PATH,NAME,TYPE,SIZE,FSTYPE,MOUNTPOINTS,UUID,PARTUUID,SERIAL,MODEL,RO`、`pvs/vgs/lvs --reportformat json --units b`（缺 lvm2 则 LVM 段返回 null+提示）、`findmnt` 关键信息、`command -v` 探测 parted/各 mkfs/resize 工具可用性（前端据此禁用按钮）
- `internal/diskadmin/ops.go` — 分区/格式化/挂载/扩缩容操作
- `internal/diskadmin/lvm.go` — PV/VG/LV 操作
- `internal/diskadmin/fstab.go` — fstab 管理（读取 → 本地追加/移除 UUID= 行 → Upload 写临时文件 → `cp` 备份 → `mv` 原子替换 → `findmnt --verify` 校验 → 失败恢复备份）

后端修改：
- `internal/httpapi/router.go` — 新增 `WithDiskAdmin(diskadmin.Service)` Option，admin 子组挂载（对齐 mountRedisAdmin 位置）
- `cmd/pipewright/main.go` — 构造 `diskadmin.New(targetService)` 并注入

前端新增：
- `web/src/api/diskAdmin.ts` — API client（文件头列全端点 + DTO）
- `web/src/views/Disks.vue` — 顶级页面（见下）
- `web/src/i18n/locales/{zh-CN,en,…}/diskAdmin.ts` × 8 语言 + 各 index 注册

前端修改：`web/src/router/index.ts`（顶级路由，roles: ['admin']）、`web/src/layouts/AppShell.vue`（导航项）。

## 四、HTTP API（前缀 /api/diskadmin，全部 admin 组）

```
GET  /servers/{id}/topology     设备树 + LVM 拓扑 + 工具可用性 + 解锁状态 + 登录用户/是否可提权
POST /servers/{id}/unlock       解锁（30 分钟 TTL，续期式；审计 diskadmin.server.unlock）
POST /servers/{id}/lock         立即回锁（审计）
POST /servers/{id}/partitions         建分区 {device, sizeSpec("100%"|"NG"), label?默认gpt}
POST /servers/{id}/partitions/delete  {device, partNum, confirm==设备路径}
POST /servers/{id}/partitions/resize  {device, partNum, targetGiB|"max", mode:grow|shrink, confirm?}
POST /servers/{id}/format             {target, fstype, label?, confirm==target}
POST /servers/{id}/mount              {target, mountpoint, options?默认defaults}
POST /servers/{id}/umount             {mountpoint, removeDir?}
POST /servers/{id}/grow               {target}   扩分区(仅末分区)+扩fs 一条龙
POST /servers/{id}/shrink             {target, targetGiB, confirm==target}
POST /servers/{id}/lvm/pv             {action:create|remove, device, confirm?}
POST /servers/{id}/lvm/vg             {action:create|extend|reduce|remove, name, devices[]}
POST /servers/{id}/lvm/lv             {action:create|extend|reduce|remove, vg, name, sizeGiB|percentFree, fstype?, confirm?}
```

写操作统一流程：readonly 闸门 → 输入校验 → **执行前实时重查拓扑做守卫**（防 TOCTOU）→ argv 执行 → 审计 → 响应携带最新拓扑（前端一次刷新）。错误映射 switch（对齐 writeRedisAdminError：哨兵 → 状态码 + 中文人话，错误体不含凭据）。

## 五、操作命令与守卫矩阵

所有命令均为 argv 数组、经提权 Runner 执行；`parted` 一律 `-s`；分区变更后 `partx -u <dev>` + 轮询 lsblk 确认新节点出现（重试 ~10s）。

| 操作 | 命令 | 关键守卫 |
|---|---|---|
| 建分区 | `parted -s <dev> -- mklabel gpt`（仅当无分区表且用户确认）/ `mkpart primary 1MiB <end>` | 盘上无已挂载分区、无 PV |
| 删分区 | `parted -s <dev> -- rm <n>` | 未挂载、非 swap、非 PV、confirm=设备路径、非系统盘分区 |
| 格式化 | `mkfs.<fstype> -F/-f <target> [-L label]` | 未挂载、非 PV、非 swap、confirm=target |
| 挂载 | `mkdir -p` + `mount` + fstab 持久化 | 挂载点绝对路径且不在关键目录（/、/usr、/boot、/etc、/dev、/proc、/sys 等）、target 有 fstype 未挂载 |
| 卸载 | `umount <mnt>`（不做 lazy）+ 移除 fstab 行 | EBUSY 时返回错误+排查提示 |
| 扩容 | 末分区 `parted resizepart <n> 100%` → `partx -u` → 按 fstype grow | 仅末分区；xfs/btrfs 须已挂载 |
| ext4 缩容 | 守卫未挂载 → `e2fsck -f` → `resize2fs <比目标略小>` → `parted resizepart` → `resize2fs` 回填 | 已挂载直接拒绝（提示用户先自行卸载）；target < 当前已用空间校验 |
| Btrfs 缩容 | `btrfs filesystem resize <target> <mnt>`（在线）→ `parted resizepart` → `resize resize max` 回填 | 须已挂载 |
| PV | `pvcreate` / `pvremove` | create 须空设备（无 fstype/未挂载，复用 checkDevices 逻辑）；remove 须不属于任何 VG |
| VG | `vgcreate` / `vgextend` / `vgreduce` / `vgremove` | reduce 要求 PV 无已分配 extent（**不做 pvmove**，UI 标注）；remove 要求无 LV |
| LV | `lvcreate -n -L|-l` / `lvextend -L +` / `lvreduce -L` / `lvremove` | create 后可选直接格式化+挂载（复用上面对应例程）；extend/reduce 的 fs 层套用能力矩阵；remove 须未挂载 |

系统盘保护：分区/LV 任一挂载点命中 /、/boot、/boot/efi、/usr、/var、/etc 或 fstype=swap，或其所属 VG 含根 LV → 标记 protected，全部写操作拒绝（含其所在整盘的 mklabel）。

超时：常规操作 5 分钟；e2fsck/resize2fs 缩容链路 60 分钟（同步执行 + 前端 spinner；文档注明中断风险）。同步模型对齐 ceph cmdTimeout 惯例，不引入异步任务框架。

## 六、前端页面（Disks.vue）

- 布局：顶部服务器选择器（复用现有 servers API）+ 只读/已解锁 chip（倒计时）+ 解锁按钮（confirm 弹窗）+ 刷新；下方左右两栏：
  - **块设备树**：data-table 按 lsblk 层级缩进展示（路径/类型/大小/fstype/挂载点/型号序列号/protected 标记），行内操作按钮（分区/格式化/挂载/扩容/缩容/删除），工具缺失时禁用并提示。
  - **LVM 面板**：VG 卡片（大小/剩余/PV 数）内嵌 LV 表与 PV 列表，操作按钮（建 VG/LV、扩缩、移除）。
- **初始化数据盘向导**：三步弹窗（分区 → 格式化 → 挂载点）顺序调三个 API，逐步展示结果，任一步失败即停。
- 危险操作用 `useConfirm` confirmText=设备路径逐字输入；XFS 缩容按钮禁用 + tooltip「XFS 不支持缩容」。
- 操作成功后用响应内嵌的新拓扑直接更新视图（无需二次拉取）。

## 七、实施步骤

1. 领域层骨架：diskadmin.go（模型/接口/哨兵）+ exec.go + parse.go + 单测（fake Runner 离线测守卫与命令构造）
2. probe.go 拓扑发现 + 解析 + 单测（内置 lsblk/lvs JSON 样本）
3. ops.go 分区/格式化/挂载 + fstab.go + 单测
4. lvm.go + 扩缩容（含能力矩阵守卫）+ 单测
5. httpapi/diskadmin.go handlers + 错误映射 + 审计接线；router.go/main.go 装配
6. 前端 api client + Disks.vue + 路由/导航/i18n ×8
7. `go build ./...` + `go test ./internal/diskadmin/... ./internal/httpapi/...` + 前端 `npm run build`（type-check）

## 八、明确不做（首期边界）

- XFS 缩容（文件系统本身不支持）；swap 分区管理；软 RAID（mdadm）、多路径（multipath）、LUKS 加密盘操作（拓扑中只读展示）；
- `pvmove`（PV 数据迁移）、thin pool / cache LV 等高级 LVM 特性；
- 分区起始位置移动、非末分区扩容；
- 挂载点目录删除策略默认保留（卸载时可选 removeDir）。

## 九、风险与缓解

- **TOCTOU**：执行前实时重查拓扑再校验（守卫不依赖请求时快照）。
- **fstab 损坏导致无法开机**：备份 + findmnt --verify 通过才提交 + 失败自动恢复备份；条目一律 UUID=。
- **SSH 中断破坏 resize2fs**：长操作给足 60 分钟超时；错误信息提示「若中断须 e2fsck 修复」。
- **免密 sudo 缺失**：复用 ErrNoPrivilege 人话指引（sudoers 配置示例）。