<div align="center">

# Pipewright

**一个轻量、自托管的 DevOps 与基础设施一体化平台。**
单个 Go 静态二进制(内嵌前端,运行时零依赖),
从 CI/CD、多机部署、容器运维,到镜像仓库、网关与证书、
应用市场、Redis / 对象存储 / PVE / Ceph 纳管,装进同一个二进制。

[![Release](https://img.shields.io/github/v/release/MyLinkPi/pipewright)](https://github.com/MyLinkPi/pipewright/releases)
[![CI](https://github.com/MyLinkPi/pipewright/actions/workflows/ci.yml/badge.svg)](https://github.com/MyLinkPi/pipewright/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[English](README.md) | 简体中文

</div>

> [!IMPORTANT]
> 本项目是 [Pipewright](https://github.com/MyLinkPi/pipewright) 的一个深度定制分支,已在原版基础上大幅扩展与重构,
> **与上游版本不兼容**:数据、配置与流水线不可直接互换或互相升级。请勿混用两个版本的二进制与数据目录,
> 也不要向上游提交本分支的问题或 PR。使用前请评估本分支自身的维护状态。

---

## 为什么是 Pipewright

Pipewright 把「持续集成、多服务器部署、服务器/容器运维」装进**一个静态二进制**:下载、启动、打开浏览器,就是全部安装过程。无需拼装多套系统,无需插件迷宫。

如今它已远不止 CI/CD:内置镜像仓库、HTTPS 域名网关与证书、应用市场一键部署、数据库 / Redis / 对象存储 / PVE / Ceph 纳管 —— 它想成为自托管基础设施的控制平面。

开箱即用:

- 单二进制部署,零运行时依赖,页面一键自更新
- 可视化流水线编排(DAG 画布),流水线即代码按分支演进
- 容器内隔离构建、免 Agent 多机部署、零停机切换 + 失败回滚
- 自动 HTTPS + 域名反向代理,内置镜像仓库(制品推送 + 基础镜像缓存)
- 服务器 / 容器 / 数据库 / Redis / 对象存储 / PVE / Ceph 一站纳管
- 应用市场模板一键参数化部署
- DORA 指标开箱即用,AI 失败诊断(可选、完全可降级)

## 界面预览

**全局概览** —— 项目、运行成功率、服务器健康、DORA 指标,一屏全局:

![Dashboard](docs/screenshots/dashboard.png)

**可视化流水线编排** —— 阶段/任务两级 DAG 画布:横向连线串行、纵向并排真并行,支持矩阵构建、人工审批门、旁挂服务、阶段后置步骤;画布与 YAML 双向往返:

![流水线编排画布](docs/screenshots/pipeline-canvas.png)

**运行详情** —— 阶段流转、实时日志(SSE 推送 + 历史回放)、构建产物与镜像引用、逐步骤状态:

![运行详情](docs/screenshots/run-detail.png)

**容器管理** —— 跨主机容器/镜像/Stacks/卷/网络一站管理,生命周期操作、实时 stats、日志、交互终端:

![容器管理](docs/screenshots/containers.png)

## 能力总览

- **🔐 安全地基** —— 单管理员认证(argon2id + CSRF)· 凭据加密保险库(NaCl secretbox,掩码呈现,绝无明文)· OAuth 应用接入 Gitee / GitHub / GitLab / 自建实例(拿到的 access token 直接存成可复用的保险库凭据)· append-only 审计(SQLite trigger 硬拦 UPDATE/DELETE)+ 可选远端 sink · 按 run 解析真实凭据后对日志 / 诊断 / 通知全链路脱敏。
- **🧩 项目与流水线** —— 可视化编排画布(阶段 DAG + 阶段内任务级 DAG)· 矩阵构建 · 人工审批门(可直接在通知里点签名链接审批)· 旁挂服务(测试挂 DB/Redis)· 阶段 `when` 条件 + 阶段后置步骤 · 类型化运行参数(枚举/布尔/数字,触发时即校验)· 触发方式:webhook(Gitee / Gitea / Gogs;事件开关 + 分支过滤 glob 白名单 + 路径过滤;集群多环境时任一环境收到投递自动转发到所属环境,接收端点可切换其他环境地址复制)、5 字段 cron 定时、上游→下游流水线串联(深度门 + 路径门防环)· 项目级并发上限 + 超限 FIFO 排队 · 复用库:流水线模板 / 变量组 / 自定义节点 · 服务端权威合法性校验。
- **📝 流水线即代码** —— 把流水线结构写进 `.pipewright.yml`、按分支各自演进,画布配置始终作为兜底回退([详见下文](#流水线即代码gitops))。
- **🏗 隔离构建与产物** —— 版本钉死的容器内隔离构建(docker/nerdctl/podman)· 代码管理区:本地 bare 镜像 + 增量 fetch,秒级出工作区 · 构建依赖缓存(按分支 + lockfile hash 寻址)· 内容寻址制品库,jar/dist 存**真字节**供部署(而非占位 reference)· 镜像构建 + 推送私有仓库 + 镜像 GC · **内置镜像仓库**:控制机自管 registry:2 双服务(制品推送 + 基础镜像 pull-through 缓存),各机 daemon.json 一键下发;可选挂载「证书管理」的公共可信证书以 **HTTPS** 服务(客户端机器零配置,明文模式自动回退 insecure-registries)· 多节点构建机池:给服务器打标签即成构建机,项目/阶段用标签选择器调度(优先级 → 流水线亲和 → 负载,全忙自动排队,每机默认并发 1;构建经 SSH 下沉到远程,token 只留控制机;控制机自身无容器 CLI 也可纯靠远程机池构建)· JUnit + Cobertura 测试报告喂质量门禁,不过则阶段失败、阻断下游部署 · 实时终端日志(SSE)+ 历史回放 · 只读代码浏览(Monaco)· **产物管理页**:跨运行集中查看/下载/删除全部产物(文件走制品库字节流,镜像服务端 pull + save 导出 tar;删除联动清理内置 registry tag 与无引用 blob,全程审计)。
- **🚀 多服务器部署** —— 经 SSH 免 Agent 部署 · 健康门控 · 零停机切换 + 失败回滚 · 多机并行扇出 + 部分失败可见 · 命令型部署(无产物,直接重启服务)。
- **🌐 服务注册网关(集群反代)+ 托管证书** —— 在你选定的(一台或多台)网关主机上,经 SSH 各编排一个配置完全一致的 nginx 容器(渲染 nginx.conf → `nginx -t` → 优雅 reload):注册基域后「服务名 + 基域」即成为子域名(`abc.efg.com`),服务还可注册**子路径**(`/api` → `abc.efg.com/api`;同域名不同路径的服务自动合并共存,路径冲突保存时报 409;反代默认原样透传完整路径,可按服务选择剥离前缀)。**集群模型**:实例 = (服务器, 宿主端口),各网关主机渲染同一份配置、按 `服务器地址:端口` 反代**集群所有实例** —— 本机没有部署的服务同样代理;容器实例由部署自动注册,宿主端口**范围探测自动分配**(默认 20000-30000,防火墙放行该段即可);部署节点内选基域 + 填服务名**一键生成**注册绑定,实例与端口随部署自动维护、自动清理失联实例。容器升级走「扩容起新→预热健康→原子切换→排空→缩容停旧」零停机轮转;非容器服务填端口注册,升级走「摘除→升级→健康→挂回」,集群可用性由其余机器保证。健康检查只需填探测路径,端口自动推导。**前后端分离一体化发布**:前端服务可注册为**静态托管**形态(无端口/实例),配合 `deploy_static`「网关静态发布」节点 —— 无需选机器,dist 产物自动发布到全部网关主机(发布目录 + current 软链原子切换 + SPA 回退,失败自动回滚),由网关 nginx 直接托管,与 `/api` 后端服务共用同一子域名。证书为一等公民:自托管 acme.sh(二进制内嵌、控制机本地执行)经 **DNS-01**(Cloudflare / DNSPod / 阿里云 DNS,含通配符)签发并自动续期,支持导入既有证书,PEM 密文存保险库,签发后自动下发到匹配基域 —— 证书管理页全程可见。
- **🔒 平台 HTTPS(宿主 nginx)** —— 当运行 Pipewright 的主机装有 nginx 时,平台可把自己的 Web 页面发布为 **HTTPS**:选访问域名 + 复用「证书管理」的证书,平台在自身所在主机自动下发证书并写入 `/etc/nginx/conf.d` vhost(443 ssl 反代平台端口 + 80→443 跳转),`nginx -t` 校验(失败自动回滚)、`nginx -T` 验证 include 生效后热加载;需要 root 或免密 sudo,与本机既有 nginx 配置共存。证书续期后自动重新下发;被平台 HTTPS 占用的证书禁止误删。
- **📣 通知** —— 企业微信 / 钉钉 / 飞书 / Slack / 邮件 / 自定义 webhook · 事件→渠道细粒度路由 · 模板 + 变量自定义 · 飞书富卡片(审批/详情行动按钮 + 发版汇总)· 流水线内通知节点。
- **🖥 服务器与容器运维** —— 多机状态总览(CPU/内存/磁盘)+ 指标时序趋势图 · 容器/镜像/Stacks/卷/网络管理 · 容器创建/inspect/prune · 实时 + 历史服务日志 · 实时 stats · 容器交互终端 · Web 运维终端(主机 shell,完整复制粘贴/信号支持)· 可配置异常检测:定时自动跑、冷却去重、命中走通知渠道。
- **📊 机器监控(ssar)** —— 基于 [ssar](https://github.com/MyLinkPi/ssar)(Anolis ssar 自维护 fork)的深度服务器指标:安装包(rpm/deb)从其 GitHub Release 自动下载(镜像感知、sha256 校验),经 SSH 分发到 Linux 目标机(root 或免密 sudo;包内 sresar 采集器以 systemd 服务常驻,机器本地保留 7 天 60s/5s 粒度历史)。平台侧周期采样入库做总览图表(CPU 分项、内存、5s 级负载、TCP 重传),细查面板按需直查机器本地高粒度历史(白名单视图:cpu/mem/tcp/retran/softirq/load5s…);不改动既有状态页/异常检测体系。
- **🛒 应用市场与中间件纳管** —— 应用商店模板一键参数化部署(内置 MySQL / Redis / Postgres / Nginx / RustFS / RabbitMQ / Gitea / Redpanda,亦可自建模板;参数白名单校验防注入,secret 自动生成且仅部署响应一次性返回)· 数据库管理(MySQL / PostgreSQL 实例注册 + 在线库表数据管理,默认只读、写操作需显式解锁并留审计)· Redis 管理(实例注册 + SCAN 游标分页键浏览/删改/改 TTL,值按类型截断预览;应用市场部署 Redis 自动登记为实例)· 对象存储集成(阿里云 OSS / S3 兼容 RustFS 多账户接入,桶浏览、新建桶自动登记为配置,流水线 `oss_upload` 节点直传)。
- **🧊 PVE 与 Ceph 纳管** —— PVE 物理机登记即经 SSH 自动发现集群并入服务器池(API Token 密文入保险库;物理机禁止作为构建/部署目标,电源操作默认只读、解锁留审计)· Ceph 集群 cephadm 引导部署(后台任务 + 步骤/日志轮询)+ 主机/OSD/存储池/RBD/CephFS 一站管理(dashboard 密码只入保险库)。
- **🤖 AI 辅助(可选,完全可降级)** —— 自带 Claude / OpenAI / Ollama 端点(apiKey 密文入保险库)。构建/部署失败自动根因诊断 + 👍/👎 反馈飞轮与准确率统计 · 仓库分析 → 生成流水线草案 · 成功/失败提交差异 · 脚本风险标注 · 运维终端的自然语言→shell 助手与容器诊断。核心 CI/CD 路径完全不依赖它(NFR-10)。
- **📈 度量** —— DORA 四指标(部署频率/变更前置时长/变更失败率/平均恢复时长)开箱即用,并给 Elite/High/Medium/Low 绩效分档。
- **🧹 数据与平台** —— 运行数据保留清理器(默认关;绝不动在跑的运行)· SQLite(纯 Go)或 MySQL · 8 种界面语言(简中 / 繁中 / 英 / 日 / 韩 / 德 / 法 / 西),API 错误信息亦服务端本地化。
- **🔄 检查 + 一键自更新** —— 设置→系统 一键查 GitHub 最新发布并语义比对;二进制部署可页面**一键自动更新**(下载 + 校验和核验 + 原子替换 + 自重启),Docker 部署给出精确升级命令。

> 安全不可妥协:凭据仅以密文存储、命令 array 化防注入、出网 SSRF 收口、日志脱敏。

## 用户与权限

平台内置两种角色,管理员为单账号,普通用户为多账号,权限严格分级:

- **管理员(admin)** —— 平台唯一的超级账号,首启生成(或经 `PIPEWRIGHT_ADMIN_PASSWORD` 指定)。拥有全部功能:项目、流水线、服务器、容器、通知、AI、设置等一切。
- **普通用户(member)** —— 由管理员邀请注册。**默认拒绝**:除被显式授予的范围外,一切功能不可见。可见范围仅三类:
  1. **我加入的项目**(只读列表);
  2. 在这些项目上**触发运行**;
  3. **查看运行详情与实时/历史日志**(含产物下载)。

  管理员设置外壳、用户管理、服务器、容器、应用商店、通知、AI 等页面与接口,对普通用户一律不出现、请求返回 403。

**邀请 → 注册流程**:管理员进入「用户管理」页生成一次性邀请链接(默认有效期 7 天)→ 通过安全渠道把链接发给被邀请者 → 被邀请者打开链接、自填用户名与口令完成注册(自动登录)→ 管理员将该用户加入指定项目 → 该用户即可在「我的项目」中看到并运行该项目。

**管理员操作路径**:左侧导航「用户管理」→ 生成邀请 / 复制链接 / 撤销未用邀请;选择项目 → 添加或移出成员。普通用户的入口为「我的项目 / 运行 / 我的设置」。

> 细节(角色模型、权限矩阵、默认拒绝中间件、迁移与安全说明)见 [docs/user-management.md](docs/user-management.md)。

## 安装 / 部署

四种形态任选,平台本体是单静态二进制、**运行时零依赖**(无需 Go/Node)。

> **Docker 前置**:平台本体不依赖 Docker,但**「隔离构建 / 容器部署」需要 Docker**(没有则降级到桩 runner、不做真实构建)。控制台 / SSH 部署 / 通知不需要。一键脚本会**检测 Docker** 并在缺失时提示;Linux 下可 `INSTALL_DOCKER=1` 自动安装(经官方 get.docker.com),macOS 请装 Docker Desktop。

### ① 一键下载安装(Linux / macOS,推荐)

本地无构建产物时,`install.sh` 自动从 GitHub Release 下载当前平台二进制(sha256 校验)并安装 —— 无需 Go/Node,小内存机器也无需本地编译:

```bash
curl -fsSL https://raw.githubusercontent.com/MyLinkPi/pipewright/master/install.sh | sh

# 直接装为 systemd 服务(开机自启 + 一键自更新;Linux,需 root):
curl -fsSL https://raw.githubusercontent.com/MyLinkPi/pipewright/master/install.sh | SETUP_SERVICE=1 sh

# 从自己的 fork 发版仓库下载(fork 推 v* tag,GitHub Actions 自动出包),或用 PIPEWRIGHT_VERSION=v1.2.3 钉版本:
curl -fsSL https://raw.githubusercontent.com/<owner>/pipewright/master/install.sh | \
  PIPEWRIGHT_RELEASE_REPO=<owner>/pipewright sh
```

GitHub 不可达的网络(如境内)可经前缀式代理安装,升级源同步走代理:

```bash
curl -fsSL https://gh-proxy.com/https://raw.githubusercontent.com/MyLinkPi/pipewright/master/install.sh | \
  SETUP_SERVICE=1 PIPEWRIGHT_RELEASE_REPO=MyLinkPi/pipewright \
  PIPEWRIGHT_RELEASE_MIRROR=https://gh-proxy.com/https://github.com sh
```

升级即**重跑同一命令**:自动进入升级模式,替换二进制并重启服务。GitHub 不可达时可用 `PIPEWRIGHT_RELEASE_MIRROR` 指向镜像(GitHub 路径兼容反代,或 `https://gh-proxy.com/https://github.com` 这类前缀式代理);`PIPEWRIGHT_NO_DOWNLOAD=1` 恢复「本地无产物即报错」的旧行为。

> Windows 用户:从 [Releases](https://github.com/MyLinkPi/pipewright/releases) 下载 `pipewright_<版本>_windows_<架构>.zip` 解压后直接运行 `pipewright.exe`(无 systemd,install.sh 不适用)。

### ② 本地编译安装(Linux / macOS)

`install.sh` 安装**本地 `make build` 产出的二进制**到 `/usr/local/bin`(含 Docker 检测;仓库根无产物时自动转入①的下载安装),需先装 Go 与 Node:

```bash
git clone https://github.com/MyLinkPi/pipewright && cd pipewright
make build
sh install.sh

# 二进制在别处 / 自定义安装目录 / Linux 顺带自动装 Docker:
BIN_PATH=/path/to/pipewright INSTALL_DIR=$HOME/.local/bin INSTALL_DOCKER=1 sh install.sh

# 运行(未设 PIPEWRIGHT_ADMIN_PASSWORD 时,首启自动生成随机口令,落 <数据目录>/admin_password.txt)
PIPEWRIGHT_MASTER_KEY=$(openssl rand -base64 32) \
  pipewright          # 打开 http://localhost:8080;口令见 admin_password.txt,登录后改掉并删文件
```

**推荐:装为 systemd 服务**(开机自启 + 崩溃重启 + 一键自更新可用;Linux,需 root)。脚本会自动持久化 master key 到 `/etc/pipewright/master.key`、数据落 `/var/lib/pipewright`、配置写 `/etc/pipewright/pipewright.env`:

```bash
SETUP_SERVICE=1 sh install.sh
# 状态 / 日志:systemctl status pipewright  ·  journalctl -u pipewright -f
# 改端口等:编辑 /etc/pipewright/pipewright.env 后 systemctl restart pipewright

# 用 MySQL 而非默认 SQLite(DSN 为 go-sql-driver 格式,parseTime=true 必带):
SETUP_SERVICE=1 PIPEWRIGHT_DB_DRIVER=mysql \
  PIPEWRIGHT_DB_DSN='user:pw@tcp(host:3306)/pipewright?parseTime=true&charset=utf8mb4' \
  sh install.sh
```

> Windows 用户:在 Git Bash 里 `make build` 后直接运行 `./pipewright.exe`,或从 [Releases](https://github.com/MyLinkPi/pipewright/releases) 下载 zip 解压运行(无 systemd,install.sh 不适用)。

### ③ docker compose(推荐自托管)

```bash
curl -fsSLO https://raw.githubusercontent.com/MyLinkPi/pipewright/master/docker-compose.yml
curl -fsSLO https://raw.githubusercontent.com/MyLinkPi/pipewright/master/.env.example
cp .env.example .env       # 生成 MASTER_KEY:openssl rand -base64 32;管理员口令可不设(首启随机生成)
docker compose up -d       # 数据持久化在具名卷 pipewright-data;切 MySQL 见 .env 注释
```

### ④ docker run(最快试用)

```bash
docker run -d -p 8080:8080 -v pipewright-data:/data \
  -e PIPEWRIGHT_MASTER_KEY=$(openssl rand -base64 32) \
  ghcr.io/mylinkpi/pipewright:latest
# 管理员初始口令(随机生成,0600):docker exec <容器> cat /data/admin_password.txt
```

### 从源码构建

```bash
make build          # 前端构建 → go:embed → 单个静态二进制 ./pipewright(纯 Go,无 CGO)
./pipewright --version
sh install.sh       # 亦可把构建产物装到 /usr/local/bin(见「本地编译安装」)
```

### 更新

打开 **设置 → 系统**,点「检查更新」查最新发布;有新版时:

- **二进制部署**:点「立即更新」即自动下载新版 + 校验和核验 + 替换 + 重启(需对二进制文件有写权限;装在 `$HOME/.local/bin` 免 sudo,或用 `SETUP_SERVICE=1` 装的 root systemd 服务亦满足)。
- **Docker 部署**:容器不替换自身镜像,按提示在宿主执行 `docker compose pull && docker compose up -d`(数据卷保留)。
- **源码部署**(git clone → `make build` → `sh install.sh`):检查更新改为比对 git 上游(`git fetch` 后比**运行中构建所基于的提交**,未注入构建信息时回退工作树 HEAD —— 这样即便代码已 pull 到位而二进制尚未重建/重启,也能正确报出有更新);点「立即更新」后台自动执行 `git pull --ff-only` → `make build` → `install.sh`,页面实时展示步骤与日志,装完自动重启服务(裸跑则自重启)。仓库根由 install.sh 自动记录(env `PIPEWRIGHT_SOURCE_DIR` / 安装目录 `.pipewright-source` 标记),也可手动指定。git 拉取自动以仓库属主身份执行(root 服务 + 普通用户克隆的场景会复用其 `~/.ssh` 密钥与主机指纹),失败时页面给出针对性修复指引。
  想改走**二进制一键更新**(如小内存机器跑不动 `make build`):重跑①的一键下载命令即可 —— install.sh 会清除源码部署标记,部署形态自动转为二进制部署。

升级源不强制依赖 GitHub:内网或 GitHub 不可达的环境,可在 **设置 → 系统 → 升级源** 里把检查与下载指向镜像,修改即时生效;也可用环境变量 `PIPEWRIGHT_RELEASE_MIRROR` 兜底。镜像支持两种形态:**GitHub 路径兼容反代**(须按同样路径布局转发,如 nginx 反代 `api.github.com` 与 `github.com`)或**前缀式公共代理** —— 直接填 `https://gh-proxy.com/https://github.com` 这类地址,检查(api.github.com)与下载(github.com)地址会自动派生。

### 配置(环境变量)

正常安装只需前两个,其余都有合理默认值。平台对外访问基址(通知里的签名审批链接、PR 状态跳转链接用)改为运行时在 **设置 → HTTPS 访问 → 外部访问地址** 里配置,修改即时生效、无需重启。

**核心**

| 变量 | 说明 | 默认 |
|---|---|---|
| `PIPEWRIGHT_ADMIN_PASSWORD` | 管理员初始口令(可选,仅首启建账时生效)。不设则自动生成随机强口令,明文写入数据目录 `admin_password.txt`(0600),日志只提示路径 | 空(自动随机) |
| `PIPEWRIGHT_ADMIN_PASSWORD_FILE` | 随机初始口令落盘路径(输出)。默认:sqlite 为库同级 `admin_password.txt`,mysql 为工作目录下同名文件 | 按数据目录推导 |
| `PIPEWRIGHT_MASTER_KEY` | 凭据保险库主密钥(base64 的 32 字节);或用 `PIPEWRIGHT_MASTER_KEY_FILE` 指文件 | 未配则保险库禁用 |
| `PIPEWRIGHT_ADDR` | HTTP 监听地址 | `:8080` |
| `PIPEWRIGHT_ADMIN_USERNAME` | 首次启动管理员用户名 | `admin` |
| `PIPEWRIGHT_TRUST_PROXY` | 采信转发头(`X-Real-IP` 优先,其次 `X-Forwarded-For` 首段)作为审计来源 IP(`1`/`true`/`yes`/`on`)。本机反代(对端为 127.0.0.1/::1,如平台 HTTPS 内置 nginx)无需此变量即自动采信;它只对**非回环**的外部反代生效 —— 除非前面确有可信反代,否则别开,否则任意客户端都能伪造审计来源 IP | 关 |
| `PIPEWRIGHT_RELEASE_REPO` | 检查更新所查的仓库(fork 可改) | `mylinkpi/pipewright`(发版构建注入,跟随构建仓库;本地 `make build` 回落 `huangchengsir/pipewright`) |
| `PIPEWRIGHT_RELEASE_MIRROR` | 自升级镜像源 base URL(部署级兜底;运行时改用 **设置 → 系统 → 升级源**,库内配置优先) | 空(GitHub 官方源) |
| `PIPEWRIGHT_RUNTIME` | 设 `docker`/`source` 显式声明部署形态(影响自更新方式);否则自动探测(`/.dockerenv` / 源码仓库标记) | 自动探测 |
| `PIPEWRIGHT_SOURCE_DIR` | 源码部署的仓库根目录(install.sh 装服务时自动写入;手动源码部署可自设) | 自动探测 |
| `PIPEWRIGHT_AUDIT_SINK` | 远端审计 sink:`http(s)://` 端点,或填其它值作为第二份本地 JSON Lines 文件路径。本地库被删后审计仍完整 | 无 |

**数据库**

| 变量 | 说明 | 默认 |
|---|---|---|
| `PIPEWRIGHT_DB_DRIVER` | 数据库驱动:`sqlite` 或 `mysql` | `sqlite` |
| `PIPEWRIGHT_DB` | SQLite 数据库路径(driver=sqlite 时) | `pipewright.db` |
| `PIPEWRIGHT_DB_DSN` | MySQL DSN(driver=mysql 时必填) | 无 |

**运行与构建**

| 变量 | 说明 | 默认 |
|---|---|---|
| `PIPEWRIGHT_RUNNER` | 运行执行器:默认 DAG(按画布 stages/script/deploy_ssh/notify 编排执行);设 `legacy` 回退旧版固定流程 | `dag` |
| `PIPEWRIGHT_BUILDER` | `auto` 探测到 docker/nerdctl/podman 用真实构建、否则回退桩;`real` 无容器 CLI 直接启动失败;`stub` 完全不碰容器 | `auto` |
| `PIPEWRIGHT_MAX_CONCURRENT` | 全局同时运行上限(超出保持 queued 排队,FIFO)。项目级上限在界面里配 | worker 数(4) |
| `PIPEWRIGHT_RUNNER_SLOTS` | 每台构建机的默认并发构建槽位(单机可在「设置 → 服务器」用槽位字段覆盖;默认 1 = 单机串行) | 1 |
| `PIPEWRIGHT_ARTIFACT_DIR` | 制品库目录(jar/dist 真字节) | `<DB 同级>/artifacts` |
| `PIPEWRIGHT_REPO_CACHE_DIR` | 代码管理区(本地 bare 镜像)目录 | `<DB 同级>/repos` |
| `PIPEWRIGHT_NO_REPO_CACHE` | `1` 关闭代码管理区(每次构建直连网络克隆) | 关 |
| `PIPEWRIGHT_CACHE_DIR` | 构建依赖缓存目录 | `<DB 同级>/cache` |
| `PIPEWRIGHT_NO_BUILD_CACHE` | `1` 关闭构建依赖缓存(每次冷构建) | 关 |
| `PIPEWRIGHT_NO_IMAGE_GC` | `1` 保留构建出的镜像,不做垃圾回收 | 关 |
| `PIPEWRIGHT_PAC_RUNTIME` | `1` 对**所有项目**强开流水线即代码,无视各项目开关 | 关 |
| `PIPEWRIGHT_CHAIN_MAX_DEPTH` | 流水线串联深度上限(防无限链的硬兜底) | `5` |

**集成**

| 变量 | 说明 | 默认 |
|---|---|---|
| `PIPEWRIGHT_PR_STATUS` | `1` 对**所有项目**强开 PR 状态回写,无视各项目开关 | 关 |
| `PIPEWRIGHT_PR_STATUS_GITHUB_BASE` | GitHub API 基址(GitHub Enterprise 用) | 公有 GitHub |
| `PIPEWRIGHT_PR_STATUS_GITEE_BASE` | Gitee API 基址(自建 Gitee 用) | 公有 Gitee |
| `PIPEWRIGHT_CADDY_IMAGE` | 反代镜像。默认是自构建的 Caddy(含 DNS-01 / ratelimit / layer4 插件);用原版 `caddy:2` 也能跑,但会失去 DNS-01/通配符/TCP 能力 | `ghcr.io/huangchengsir/pipewright-caddy:latest` |

**运维监控**

| 变量 | 说明 | 默认 |
|---|---|---|
| `PIPEWRIGHT_ANOMALY_INTERVAL` | 异常检测间隔(秒);`0` 关闭定时(仍可手动「立即检测」) | `60` |
| `PIPEWRIGHT_ANOMALY_COOLDOWN` | 同「服务器×规则」重复告警的最小间隔(秒) | `600` |
| `PIPEWRIGHT_METRICS_SAMPLE_INTERVAL` | 服务器指标采样间隔(秒,趋势图数据源);`0` 关闭采样 | `60` |
| `PIPEWRIGHT_METRICS_RETENTION_DAYS` | 指标样本保留天数 | `7` |
| `PIPEWRIGHT_SSAR_SAMPLE_INTERVAL` | ssar 机器监控采样间隔(秒);`0` 关闭采样 | `60` |
| `PIPEWRIGHT_SSAR_RETENTION_DAYS` | ssar 采样样本保留天数 | `7` |
| `PIPEWRIGHT_SSAR_VERSION` | 钉死要安装的 ssar release tag(留空 = 最新) | — |

## 流水线即代码(GitOps)

把流水线结构写进仓库的 `.pipewright.yml`,**跟代码同源、走 PR 评审、按分支演进**——不再依赖画布配置的隐式漂移。

- **开启**:在项目的流水线页打开「流水线即代码 / Pipeline as code」开关(按项目维度)。
- **生效方式**:开启后,每次运行都从**本次构建分支**的**仓库根**读取 `.pipewright.yml`(分支为空时回退项目默认分支),用其中的流水线 spec 驱动本次运行;不同分支可携带各自的 `.pipewright.yml`。文件用项目绑定的仓库凭据临时拉取,无新增暴露面。
- **永不卡住运行的回退**:文件**缺失** → 回退到画布(UI)里已配置的流水线;文件存在但 **YAML 非法** → 同样回退到已存的画布配置。
- **作用范围**:YAML 只管**流水线结构**(阶段 / 任务 / `needs` / DAG 编排);**变量与缓存、触发规则**仍来自画布(UI)设置,**不写在 YAML 里**。
- **schema** 与平台「从 YAML 导入」用的是同一套(`version` + `stages` → `jobs`,job 用嵌套 `script:` 块写 `image`/`commands`/`env`/`workdir`)。
- **节点类型**(画布与 YAML 通用):`git_source`、`script`、`build_backend`、`build_frontend`、`build_image`、`push_image`(推送镜像:用 `artifactJob` 绑定**同阶段**的来源 `build_image` 节点——推送在该节点内完成;`registry`/`imageName`/`tag`/`credentialId` 可选覆盖目标,留空回退内置镜像仓库)、`oss_upload`、`deploy_ssh`、`deploy_frontend`、`health_check`、`notify`、`templated`、`custom`。

```yaml
version: 1
stages:
  - id: stg_src              # needs 按阶段 id 引用,故跨阶段依赖须显式写 id
    name: 流水线源
    kind: source
    jobs:
      - name: 流水线源
        type: git_source
  - id: stg_build
    name: 构建
    kind: build
    needs: [stg_src]
    jobs:
      - name: 运行测试
        type: script
        script:
          image: golang:1.23
          commands:
            - go vet ./...
            - go test ./...
          env:
            CGO_ENABLED: "0"
          workdir: src/app
  - id: stg_deploy
    name: 部署
    kind: deploy
    needs: [stg_build]
    gate: true               # 人工审批门
    when:
      branches: [main, release/*]
    jobs:
      - name: SSH 部署
        type: deploy_ssh
        config:
          targetEnv: prod
```

> 也可用全局环境变量 `PIPEWRIGHT_PAC_RUNTIME=1` 对**所有项目**强制开启流水线即代码(无视各项目开关),供向后兼容 / 高级用户使用。

## 技术栈

- **后端**:Go · Chi(路由)· modernc/sqlite(纯 Go,无 CGO)+ go-sql-driver/mysql · go-git(不依赖宿主 git)· NaCl secretbox(保险库)· argon2id + bcrypt · golang.org/x/crypto/ssh(免 Agent 部署)· coder/websocket(终端)· aliyun-oss-go-sdk(阿里云 OSS 上传)· Caddy(在目标主机上编排,提供自动 HTTPS)
- **前端**:Vue 3 `<script setup>` · Vite · naive-ui · OKLCH 双主题 · Monaco(只读代码浏览)· Vitest + Playwright · 经 `go:embed` 内嵌进二进制

依赖刻意保持精简:cron 解析器、DAG 引擎、DNS 厂商客户端、制品库/构建缓存、Caddyfile 渲染器全部自己实现;唯一引入 SDK 的是对象存储上传(`aliyun-oss-go-sdk`,阿里云 OSS 官方 SDK)。

## 架构

一个进程,约 45 个领域包。`internal/httpapi` 是唯一碰 HTTP 的包,所有领域包注入其中;`cmd/pipewright/main.go` 是唯一的装配层(包内互引经这里的适配器晚绑,保持包间单向依赖)。

```
单静态二进制 (cmd/pipewright)
│
├─ 地基
│  ├── internal/config        env 配置 + master key 加载
│  ├── internal/store         开库 + 迁移(sqlite / mysql 双方言)
│  ├── internal/auth          认证 + 会话 + CSRF
│  ├── internal/vault         凭据加密保险库(secretbox)
│  ├── internal/oauth         OAuth 应用接入 → token 存成保险库凭据
│  ├── internal/audit         append-only 审计 + 可选远端 sink
│  ├── internal/mask          secret 脱敏(日志 / 诊断 / 通知)
│  ├── internal/i18n          用户可见信息的服务端本地化
│  └── internal/version       版本检查 + 一键自更新
│
├─ 流水线
│  ├── internal/project       项目接入 + 仓库探测
│  ├── internal/pipeline      流水线 spec + 构建/部署配置 + 校验
│  ├── internal/pipelineyaml  YAML ↔ spec 双向往返(导入 / 导出)
│  ├── internal/pacloader     流水线即代码加载(.pipewright.yml,按分支)
│  ├── internal/library       可复用模板 + 变量组 + 自定义节点
│  ├── internal/trigger       webhook + 分支过滤 + 路径过滤
│  ├── internal/cron          5 字段 cron 解析器 + 分钟粒度调度器
│  └── internal/chain         上游→下游串联(防环)
│
├─ 执行
│  ├── internal/run           运行模型 + worker pool + 日志 + 产物 + 参数
│  ├── internal/dag           纯 DAG 调度内核(零 I/O)
│  ├── internal/dagrun        DAG 运行编排(阶段级 + 任务级,矩阵展开)
│  ├── internal/build         容器内隔离构建 + 镜像 + 阶段执行器
│  ├── internal/runner        构建机池:项目选择器配置 + 标签/优先级/亲和调度
│  ├── internal/repocache     代码管理区(本地 bare 镜像 + 增量 fetch)
│  ├── internal/buildcache    构建依赖缓存(分支 + lockfile 寻址)
│  ├── internal/artifactstore 内容寻址的产物字节存储
│  ├── internal/testreport    JUnit + Cobertura 解析
│  ├── internal/qualitygate   纯质量门禁评估
│  ├── internal/approval      人工审批门(协调器 + 持久化)
│  └── internal/retention     运行数据保留清理
│
├─ 交付
│  ├── internal/target        通用 SSH exec/session 层(部署 + 运维共享)
│  ├── internal/deploy        SSH 部署执行 + 健康门控 + 回滚
│  ├── internal/prstatus      PR / 提交状态回写(GitHub / Gitee)
│  ├── internal/proxy         自动 HTTPS + 域名反向代理(Caddy 编排)
│  ├── internal/dnsprovider   Cloudflare / DNSPod / 阿里云 DNS(DNS-01 + 子域名)
│  ├── internal/objectstore   对象存储接入(阿里云 OSS;流水线产物上传)
│
├─ 可观测
│  ├── internal/notify        多渠道通知 + 事件路由 + 模板
│  ├── internal/anomaly       可配置阈值检测 + 告警
│  ├── internal/metrics       服务器指标时序(趋势图)
│  ├── internal/dora          DORA 指标 + 绩效分档
│  └── internal/ai            可选 AI:诊断 / 仓库分析 / 差异 / 风险标注
│
├── internal/httpapi          唯一对外 HTTP 面(约 188 条路由)
└── web/                      Vue3 前端(go:embed 内嵌)
```

## 开发状态

✅ **已正式发布**,持续迭代中 —— 最新版本见 [Releases](https://github.com/MyLinkPi/pipewright/releases)(tag 驱动发版:6 平台二进制 + ghcr 多架构镜像)。已在真实生产环境承载多项目的构建、部署与日常运维。

## 贡献 / Contributing

欢迎 PR 与 Issue!动手前请读 [CONTRIBUTING.md](CONTRIBUTING.md)(搭环境 / 测试 / 提交规范),并遵守[行为准则](CODE_OF_CONDUCT.md)。安全漏洞请走私密渠道,见 [SECURITY.md](SECURITY.md)。

## License

MIT — 见 [LICENSE](LICENSE)。

---

<div align="center">
<sub>Pipewright —— 把 CI、部署与运维装进一个二进制。</sub>
</div>
