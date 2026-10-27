# EasyTier 能力审计与 Secure Mode 采用方案

> 前提(2026-10-27 确认):集群内不存在任何低于 v2.6.4 的 EasyTier 部署,
> **所有向后兼容逻辑一律不考虑保留**。二进制自动下载取最新 release,当前即 v2.6.4。
>
> 背景:本仓 EasyTier 集成(2026-10-17 引入)实现时未调研当前版本能力面,
> 准入模型停留在 v2.1 时代的「网络名 + 共享密钥」,且代码里留有按 v2.1 功能下限
> 写的兼容探测层。本文档是补做的能力审计与升级方案。

## 一、现状盘点(已用能力)

平台节点与 pwclient 共用 `internal/mesh/easytier` 一套装配(`buildArgs`):

| 类别 | 用到的参数/机制 |
| --- | --- |
| 组网 | `--network-name` `--network-secret` `-p/--peers` `--hostname` `-i` `--dhcp` |
| 子网代理/路由 | `--proxy-networks` `--manual-routes` |
| 代理出口 | `--socks5` `--exit-nodes` `--enable-exit-node` |
| 网络标识 | `--network-directory` |
| 运维 | `--rpc-portal`(固定 127.0.0.1)+ `easytier-cli -o json` 状态解析;二进制自动下载;进程收养/孤儿清理 |
| 平台自有(非 EasyTier) | ed25519 节点请求签名(`internal/mesh/sign.go`)、mesh_acl 五张授权单、mesh DNS 记录同步 |

## 二、能力审计(v2.3.0 → v2.6.4 对照)

结论速览:当前版本可用而我们未用的能力约 25 项;**建议立即采用 4 项、评估 7 项、明确不用 2 项**,其余记录备查。

### 认证与准入(缺口核心)

| 能力(引入版本) | 状态 | 建议 |
| --- | --- | --- |
| Secure Mode:管理节点签发临时凭据、免分发 network_secret、可撤销、信任传播(v2.6.0) | 未用 | **采用**(方案见第四节) |
| 凭据托管增强:声明式受管凭据 + ACL 热替换(v2.7.0) | 未用 | 跟进,v2.7 稳定后 |
| 网络层 ACL:完整规则集(v2.4.0)、组规则(v2.4.3)、web 控制台管理(v2.6.1) | 未用 | 评估:给手机/访客设备做端口级隔离;与平台自有 mesh_acl(管 API 权限)是两层,不冲突 |
| `--private-mode`(v2.3.1) | **已采用**(2026-10-27 默认启用,`buildArgs` 无条件传参,不设开关) | 安全默认:不为陌生网络中继;与 `--relay-network-whitelist` 二选一,后者不再考虑 |
| `--relay-network-whitelist` / `--foreign-relay-bps-limit` | 未用 | 可选加固 |
| Noise XX 握手静态密钥、中继/web-core E2EE(v2.6.0) | 自动获益 | — |
| `--rpc-portal-whitelist`(v2.3.2) | 已固定绑 127.0.0.1 | 无需动 |

### 连接与性能

| 能力 | 状态 | 建议 |
| --- | --- | --- |
| TCP 打洞(v2.5.0)、IPv6 打洞(v2.3.0) | 自动获益 | — |
| UPnP 自动端口映射,**v2.6.2 起默认开启** | 未显式设置 | **注意行为变化**:家用路由客户端受益可留;公网 ECS 节点显式 `--disable-upnp`(安全组已挡,只留日志噪音) |
| `--latency-first` 路由模式 | 未用 | 可选:交互流量(ssh/终端)优先延迟时开 |
| KCP 代理 `--enable-kcp-proxy`(v2.5.0) | 未用 | 评估:弱网/跨国链路,需开 UDP 端口 |
| QUIC 代理 `--enable-quic-proxy`(v2.5.0) | 未用 | 评估:同上,注意额外监听端口 |
| `--compression`(v2.6.0 zstd) | 未用 | 可选:构建产物传输类流量 CPU 换带宽 |
| `--multi-thread`(v2.4.0) | 未用 | 可选:共享构建机/中继角色的高吞吐节点 |
| 自建 `--stun-servers`(v2.4.3) | 未用 | 评估:公共 STUN 不稳的国内 NAT 环境 |
| FakeTCP 隧道(v2.5.0,实验性) | 未用 | 不碰(实验性) |
| `--p2p-only` / `--lazy-p2p` / `--need-p2p`(v2.5/2.6) | 未用 | 记录:手机计费流量场景才相关 |

### 网络功能

| 能力 | 状态 | 建议 |
| --- | --- | --- |
| WireGuard 服务端 `--vpn-portal`(v2.5.0) | 未用 | **评估(重要)**:手机/第三方设备用原生 WireGuard 客户端入网的零开发路径;代价:流量经 portal 节点中继(无全网 p2p)、仅 /24 网段、密钥体系与凭据是两套。作为安卓方案备选 |
| Magic DNS `--accept-dns`(v2.3.0,`hostname.et.net`) | 未用 | 保持自建 mesh DNS(支持任意域名,更全);客户端已设 `--hostname`,et.net 短名可顺手获益 |
| `--listeners` 显式收敛 | 未用(默认开 tcp/udp/ws/wss/wg 五种监听) | **采用**:服务节点显式只留 `tcp://0.0.0.0:11010` + `udp://0.0.0.0:11010`;ws/wss/wg 从不用,白收攻击面 |
| `--no-listener`(v2.5.0) | 未用 | 客户端**不建议**:打洞依赖 listener,关掉退化全中继 |
| `--no-tun` + 子网代理访问 | 未用 | 记录:无法建 tun 的受限机器/无 VPN 权限安卓 |
| `--port-forward`(v2.3.0) | 未用 | 记录:免管理员访问单服务场景 |
| IPv6 双栈 `--ipv6`、公网 v6 自动分配(v2.6.2) | 未用 | 评估:国内家庭宽带 v6 普及,可提升直连率,先测 |
| `--proxy-networks` 带下一跳映射语法 | 部分(只传网段) | 记录:异构组网拓扑时有用 |
| `--proxy-forward-by-system` | 未用 | 记录 |
| `--mapped-listeners`(v2.4.3) | 未用 | 记录:出口端口受限场景 |
| Windows UDP 广播转发(v2.6.4) | 未用 | 无关(不做 LAN 发现) |

### 管理面

| 能力 | 状态 | 建议 |
| --- | --- | --- |
| easytier-web 控制台 + webhook 托管配置(v2.3.0/v2.6.0) | 未用 | **明确不用**:pipewright 平台自身就是管理面,凭据签发由平台直连节点 RPC 实现,不引第二套 |
| config.toml 声明式配置 | 未用 | 不需要:参数由代码装配 |
| 多实例 `--instance-name` / `--config-dir` | 未用 | 不需要:单网络架构 |
| RPC JSON 输出 | 已用 ✓ | — |

### 行为变化注意(非能力,但影响运维判断)

- v2.6.2 起 UPnP 默认开启(见上)。
- v2.6.3 起文件日志默认关闭(我们不用它的文件日志,无影响)。
- v2.6.0 起中继/web-core 链路端到端加密(自动获益)。

## 三、兼容层清理清单(前提:全量 ≥ v2.6.4)

0. **已实证的 v2.6.4 旗子形态坑(2026-10-27 线上事故复盘)**:
   - EasyTier 布尔旗子是**取值形**而非裸旗子:`--private-mode <PRIVATE_MODE>`
     (possible values true/false)必须显式带值,裸旗子被 clap 拒绝、进程秒退,
     表象为 RPC portal 不起、`easytier-cli` 报 failed to connect to server。
     修复:传 `"--private-mode", "true"`(已落码,含回归测试)。
   - `--network-directory` **已在 v2.6.4 移除**(继任者 `--config-dir`:加载目录
     .toml 并保存下发配置)。当前探测层发现 help 无此旗子即静默不传 → v2.6.4
     部署上实例身份目录只剩 cwd 兜底(supervisor 已设 cmd.Dir),DHCP 虚拟 IP
     重启稳定性与多出口代理实例隔离**未验证是否仍然成立**。处置:阶段 0 切换到
     `--config-dir` 并在真实网络下验证身份落盘位置后移除旧旗子分支。
   - 其余已验证可用的传参形态:`--dhcp`(可选值,裸旗子 OK)、`--manual-routes`
     (可选多值,裸旗子 OK)、`--enable-exit-node`(可选值,显式 true 亦 OK)、
     `--socks5 <PORT>`(必带值)。清理探测层时按此形态直拼。
1. `supervisor.go`:`probeHelpFlags` / `--help` 探测缓存 / `helpFlags` 结构 /
   `buildArgs` 的 flags 分支 / 全部「需 v2.1+」错误路径(约 227-269、300-313 行)——
   删除,无条件直接拼新参数。相应单测(`supervisor_test.go`)同步简化。
2. `status.go`:`easytier-cli` 新旧调用形态(`-o json` vs `--output json`)自动探测
   (commit e8e0674 引入)——删旧形态,固定 `-o json`。
3. `download.go`:下载/启动增加版本下限校验,`InstalledVersion` 解析低于 v2.6.4 时
   拒绝启动并提示(防上游 release 回退或本地缓存旧包)。
4. 全部「v2.1+」措辞的错误信息与文档替换为 v2.6.4 下限。

## 四、Secure Mode 采用方案

### 阶段 0:清理 + 监听面收敛(零协议风险,预计半天)

- ~~评估开启 `--private-mode`~~ —— **已完成**(2026-10-27):`buildArgs` 无条件传参,
  平台节点与 pwclient 同一生效路径。存量进程因参数指纹变化过不了收养比对,首个
  对账节拍(10s)自动滚动重启一次,无需人工干预。
- 执行第三节全部清理。
- 服务节点:`--listeners tcp://0.0.0.0:11010 --listeners udp://0.0.0.0:11010`
  (收敛掉 ws/wss/wg)+ `--disable-upnp`。
- 不动任何协议行为,随时可发布。

### 阶段 1:Secure Mode 密钥基建(预计 1-2 天)

- `mesh_settings` 增加 secure mode 静态密钥对(EasyTier 自有密钥体系,与现有
  ed25519 应用层签名是两套,分开存储管理)。
- 所有节点以 `--secure-mode --local-private-key <固定>` 启动;平台节点(管理角色)
  加 `--credential-file` 持久化已签发凭据。
- 切换方式:我们控制全部节点,一个维护窗口内全量滚动重启即可(官方语义是凭据节点
  拒绝旧客户端,不存在长期混跑形态)。

### 阶段 2:凭据签发(预计 2-3 天)

- 平台新增凭据管理 API/UI:签发(TTL/一次性/可复用)、撤销、列表;审计对齐
  reveal-secret / client-config export 先例。
- `GET /mesh/client-config` 导出从 `networkSecret` 切换为 `credential` 字段
  (pipewright-client.yaml schema 同步加字段;不考虑兼容,直接切)。
- pwclient `buildArgs` 增加 `--credential` 透传;托盘导入流程、`doctor` 自检同步。
- 安卓官方 App 凭据入网的实际 UI 路径在此阶段实测确认。

### 阶段 3(可选)

- 网络层 ACL:访客设备端口级隔离。
- WireGuard portal:作为安卓/第三方设备的备选入网通道评估。

### 风险与验证

- 全网切换需一次协同重启——节点全量可控,维护窗口解决。
- 凭据丢失 = 设备无法入网——重新签发即可,这正是目标性质(对比现状:明文共享密钥
  泄露需全网换密钥)。
- secgroup「连接前放行」与凭据正交:凭据解决加密层准入,安全组解决网络层可达,
  手机入网两者都要过,升级凭据不豁免安全组。
