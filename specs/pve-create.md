# PVE 建机三件套（纯 API）

> 状态:已实现。相关:`internal/pveadmin/`(领域包:client/probe/provision/cloudinit/create/create_guest/create_validate/register/upload)、`internal/httpapi/pveadmin.go` + `pveadmin_create.go`(HTTP 面)、`web/src/components/pve/CreateWizard.vue` + `CreateProgress.vue` + `web/src/views/PVE.vue`(前端)、迁移 `0116_pve_vm_links.sql`。

## 语义

「PVE」页(admin)在既有纳管/总览/电源操作之上新增**建机能力**:模板克隆建 VM、云镜像 Cloud-Init 建 VM、创建 LXC,全部经 PVE REST API(官方 apidoc 逐端点验证,`allowtoken=1`,与既有 API Token 认证兼容),批量 1–50 台,建机后可选自动登记为平台服务器。

- **后台任务模型**(照 `ceph.deployJob`):建机是多步编排(克隆/导入/下载单步可达分钟级),`StartCreate` 校验 + readonly 闸门 + busy 检查后 `go runCreate`(`context.Background`,不随请求取消);每集群同时一个任务(占用 → `ErrBusy`);`CreateStatus` 返回步骤/消息/600 行环形日志/逐台结果快照,前端 2s 轮询至 done。UPID 等待各步独立超时(克隆/导入 15min、下载 30min、启动 3min、改配 2min)。
- **管线 A 模板克隆**:`POST /nodes/{n}/qemu/{src}/clone`(full=1)→ waitTask → `POST .../config`(同步变体:cores/memory/net0/agent/onboot/bios/efidisk0)→ `PUT .../resize`(绝对尺寸;缩盘被拒非致命)→ 模板带 cloud-init 盘则注入 ipconfig0+cicustom → 启动。**.vma 恢复分支**:`POST /nodes/{n}/qemu` + `archive`(卷 ID 或绝对路径)→ 同收尾。克隆节点自动取模板所在节点。
- **管线 B 云镜像**:镜像准备(URL → 9.x `POST .../storage/{s}/download-url` / 8.x `POST .../content`+url,content=import,按扩展名自动解压;同 URL 批量只下载一次;或选已有 import/images 卷)→ **snippet 落盘**(Go 生成 #cloud-config,经 `Runner.WriteFile` SSH 0600 写到节点 snippets 存储,文件名含任务级随机 ID 防覆盖;PVE API 无 snippet 上传端点,这是唯一 SSH 必需点)→ `POST /nodes/{n}/qemu`(ostype/scsihw/net0/ovmf/efidisk0/ide2 cloudinit/ipconfig0/cicustom/boot)→ **磁盘导入** `PUT .../config` `scsi0:"STORAGE:0,import-from=<volid>"`(仅明确拒绝 import-from 参数才经 SSH `qm importdisk` 兜底,解析输出卷 ID 再挂盘)→ 按 `diskGb` 扩容并等待 → `PUT .../cloudinit` 再生(失败非致命,开机自动再生)→ 启动。
- **管线 C LXC**:(可选任务内 aplinfo 在线下载,按 location 基名定位卷)→ `POST /nodes/{n}/lxc`(ostemplate/rootfs 目标存储与系统盘/hostname/password/net0 静态 IP 或 DHCP/unprivileged/features nesting+keyctl/mp0 数据盘/nameserver/timezone/ssh-public-keys)→ waitTask → 启动。
- **cloud-init 生成**(`cloudinit.go`,净室实现):恒定 hostname + preserve_hostname:false + hostnamectl 兜底(保证 PVE 名 == guest 主机名);users/chpasswd/ssh_authorized_keys;runcmd 顺序敏感:DNS 健全性 → 换源(ustc/tsinghua/aliyun,apt/dnf/yum 三路,**先于装 agent**)→ hostnamectl → ssh 重启 → 装并启动 qemu-guest-agent(static 单元 start+enable)→ 关 SELinux/防火墙(可选)→ 扩根(OpenWrt 专用分支 + growpart/pvresize/lvextend/resize2fs/xfs_growfs,幂等)→ DHCP 固化静态(netplan/nmcli 分派,哨兵文件仅一次)。密码/密钥只进 YAML 与 SSH stdin,**绝不进日志/审计/错误体**。
- **批量**:vmid=起始+i、name 多台加 `-序号`、静态 IP `netip.Addr.Next()` 递增;**逐台独立容错**(一台失败不阻断后续,参照 aliyunecs.Import),结果表创建与登记分开计错。
- **自动登记**(`register.go`,照 aliyunecs.Import):IP 获取(静态直用 + TCP 探活非致命;DHCP 轮询 guest agent `network-get-interfaces` / lxc `interfaces`,过滤回环/链路本地)→ 凭据 XOR(已有 vault 凭据或内联代建)→ `pve_vm_links`(cluster_id+node+vmid 唯一)幂等匹配:命中更新原机器行,未命中 `servers.Create`(**Kind 留空 = standard**,避开 pve_node 目标拦截,可进 deploy_ssh)→ 回写映射。登记失败不否定建机结果。
- **.vma 上传**(`upload.go`):路由在读取流之前判定(避免流消费后无法兜底)——9.x API 上传 enum 无 backup → SSH 流式写文件型存储 dump 目录的随机临时文件(0640),成功后以硬链接原子发布且不覆盖同名文件,失败清理临时文件,返回绝对路径;8.x 存储启用 backup 内容 → API multipart,返回卷 ID;非文件型存储 → 可操作错误。全程流式不缓冲,整个 multipart 请求上限 2 TiB。
- **版本自适应**:`apiMajor`(GET /version)区分 8.x/9.x 上传/下载端点;import-from 运行时被拒即 SSH 兜底(不依赖精确版本判断)。

## 端点(/api/pve/clusters/{id}/*,admin 组)

| 端点 | 作用 |
|---|---|
| `GET .../templates` | 集群 qemu 模板列表(克隆源下拉;LXC 克隆尚未支持) |
| `GET .../storages?node=` | 节点存储状态(内容类型/用量) |
| `GET .../bridges?node=` | 节点网桥 |
| `GET .../nextid` | 集群下一个空闲 VMID |
| `GET .../storage-content?node=&storage=&content=` | 存储内容(vztmpl/import/images/backup) |
| `GET .../aplinfo?node=` | 可下载 LXC 模板目录 |
| `POST .../aplinfo/download` | `{node,storage,template}` → 202 {node,upid}。审计 `pveadmin.apl.download` |
| `POST .../create` | CreateInput → 202 {ok:true}(异步任务)。审计 `pveadmin.vm.create` |
| `GET .../create-status` | 任务快照(步骤/日志/逐台结果) |
| `POST .../upload-vma?node=&storage=` | multipart 文件字段 filename,流式透传 → {volid}。审计 `pveadmin.backup.upload` |

错误映射:入参非法 → 422 `invalid_pve_input`;只读 → 409 `pve_readonly`(**拒绝也留审计**);任务占用 → 409 `pve_busy`;未配 Token → 409 `pve_cluster_not_ready`;PVE API 失败 → 502 `pve_connect_failed`;上传超限 → 413 `pve_upload_too_large`。审计 detail 只含 mode/node/name/count/vmid/filename 等非密字段,**绝不含密码/密钥/Token**。上传路由绕过 64KiB body 限制(流式);全局 `http.Server.ReadTimeout` 保持 30s 防匿名慢请求,上传 handler 经 `ResponseController.SetReadDeadline` 单独放宽到 2h。

## 边界与取舍

- **不做流水线 pve_create 节点**(后续另做):领域层 StartCreate/CreateStatus 已为其预留;届时照 ecs_power 六层清单实施。
- **不做 VNC 控制台、快照/备份/迁移管理、分片断点续传、云镜像本地文件上传**(仅 URL 下载 + 已有卷)、PVE 防火墙/用户管理、PBS。
- **snippet 必须 SSH**:PVE API 无 snippets 上传端点(官方 schema 验证);节点纳管时本就要求 SSH 可达,依赖现成。无 snippets 存储/非文件型 → 明确报错。
- **9.x .vma 上传走 SSH**:官方 9.x 上传 enum 仅 iso|vztmpl|import(无 backup);仅文件型存储(dir/nfs)可兜底。
- **克隆静态 IP 依赖模板 cloud-init 盘**:无盘则记提示不注入(与 pvewebtools 行为一致)。
- **GPL 纪律**:pvewebtools 为 GPL-3.0、本平台 MIT——cloud-init/扩根/换源等仅参考其行为语义,Go 净室重写,不复制任何外部代码。
- **每集群一个建机任务**:简化并发与存储/VMID 竞争;批量在任务内循环。

## 实现位置

- 领域包 `internal/pveadmin/`:`client.go`(Put/Upload multipart 流式 + 独立无超时上传客户端 + apiMajor + 存储/网络/aplinfo/content 响应模型)、`probe.go`(Runner.WriteFile 提权 stdin 写 + nodeServerID 三级匹配)、`provision.go`(只读下拉 + snippetsStorage 定位)、`cloudinit.go`(#cloud-config 生成)、`create.go`(任务模型/StartCreate/CreateStatus/批量/waitTask)、`create_guest.go`(三管线 + qemu/lxc 助手 + importDiskSSH 兜底)、`create_validate.go`(白名单校验)、`register.go`(IP 探测 + 凭据 XOR + 幂等登记)、`upload.go`(.vma 版本自适应)。
- HTTP 面:`internal/httpapi/pveadmin.go`(mount + ErrBusy 映射)、`pveadmin_create.go`(10 个新 handler + 审计);装配零改动(`pveadmin.New(db, vault, targetSvc)` 依赖已齐)。
- 迁移:`0116_pve_vm_links.sql`(sqlite/mysql 成对;server 外键 CASCADE;集群删除领域层连带清理)。
- 前端:`web/src/api/pveAdmin.ts`(建机面 client + 类型)、`web/src/api/http.ts`(postForm)、`web/src/components/pve/CreateWizard.vue`(三步向导)、`CreateProgress.vue`(轮询进度)、`web/src/views/PVE.vue`(新建入口)、`web/src/i18n/locales/*/pveCreate.ts`(8 语言,keyParity 强制)。
- 平台微调:`cmd/pipewright/main.go` 保持 ReadTimeout=30s;大文件上传在 handler 单独设置有限读取期限。