# 机器监控(ssar)

> 状态:已实现。监控工具为自维护 fork [MyLinkPi/ssar](https://github.com/MyLinkPi/ssar)(上游 Anolis ssar 1.0.5)。相关:`internal/ssar/`(领域包)、`internal/httpapi/ssar.go`(HTTP 面)、`web/src/views/SsarMonitor.vue`(前端)。

## 语义

「机器监控」页(admin)把 ssar 安装分发到登记的 Linux 服务器,提供两层监控数据:

- **安装分发**:平台从 GitHub Release(MyLinkPi/ssar)拉取**安装包**(`ssar-<ver>-1.x86_64.rpm` / `ssar_<ver>-1_amd64.deb`,sha256 digest 硬校验,镜像感知:设置库 release_mirror > env `PIPEWRIGHT_RELEASE_MIRROR` > GitHub;env `PIPEWRIGHT_SSAR_VERSION` 可钉死 tag)。安装 = 上传包到目标机 `/tmp` 后提权执行 `rpm -Uvh --replacepkgs` / `dpkg -i`——包自带 `/usr/bin/{ssar,sresar}`、`/etc/ssar` 配置与 systemd 服务(%post 自动 enable+start;无 systemd 的容器环境回落 cron)。幂等重装即升级。需要 root 登录或免密 sudo(与磁盘管理同款探测)。
- **机器侧采集(数据面在目标机)**:sresar 守护本地 60s 采全量 /proc 快照 + 5s 级负载,默认保留 7 天(`duration_threshold=168h`),磁盘自保护(>90% 或 <1GB 停采)。平台不代理存储该数据。
- **平台侧周期采样(总览数据源)**:ticker(env `PIPEWRIGHT_SSAR_SAMPLE_INTERVAL`,默认 60s;0 关闭)对每台跑**一条**合并 SSH 脚本(`##PW:` 分段:cpu/mem/tcp/retran/load5s,各段 `ssar --<view> --api -r 3 -P` 取**尾行**)解析入 `ssar_samples` 时序表;保留 `PIPEWRIGHT_SSAR_RETENTION_DAYS`(默认 7)。
- **按需细查**:页面选机器 + 白名单视图 + 时长/步长 → SSH 直查机器本地 ssar 历史(`ssar --<view> --api -r <分钟> -i <步长>m -P`;load5s 子命令无 -i/-P),细粒度(60s/5s)到 7 天。

## 解析契约(ssar `--api`)

单行 JSON 数组,元素为扁平对象:键 = 列别名 + `collect_datetime`,**值一律字符串**;`-`(无数据)/`*`(重启边界)等占位符 → 该值 null。时间口径:平台入库时间用 pipewright 采样时刻(**不采信**行内时间——机器本地时区);细查响应原样回传机器本地时间字符串,前端标注。列单位由静态口径表标注(CPU jiffies/s、内存 kB、重传 /s 等)。

## 端点(/api/ssar*,admin 组)

| 端点 | 作用 |
|---|---|
| `GET /api/ssar/overview` | 每台 {serverId, name, installed, version, collectorActive, lastSampleAt, error} + 平台包缓存状态(binaries)。状态探测走 TTL 缓存(30s) |
| `POST /api/ssar/download` | 异步触发安装包下载(202;单飞;进度经 overview.binaries.downloading 轮询)。审计 `ssar_download` |
| `POST /api/ssar/install` | body `{serverIds[]}`(≤100)→ 逐台部署(并发 3),同步返回逐台 `{serverId, ok, version, error}`。审计 `ssar_install` |
| `GET /api/ssar/query?serverId=&view=&minutes=&step=` | 机器侧细查。view 白名单:cpu/mem/tcp/udp/vm/softirq/retran/tcpofo/tcpdrop/tcperr/load5s;minutes 1..10080、step ≥ 1 分钟、行数 ≤ 1440 |
| `GET /api/ssar/history?serverId=&hours=` | 平台采样时序(1..168h,升序,≤11000 点) |

错误映射:参数越界 → 400 `bad_request`;机器不可达 / ssar 不可用 → 502 `ssar_query_failed`;服务未装配 → 503。单台探测/安装失败只落该台 `error` 字段,不连累其它台、不 500。

## 边界与取舍

- **仅 Linux 目标**:探测 `uname -s`,非 Linux → 明确人读错误;aarch64 暂无发行资产 → 「暂无匹配安装包」(Release 补充 arm64 包后自动纳管,资产按后缀匹配)。
- **未装 ssar 的机器自然跳过采样**(合并脚本各段为空 → 全字段 nil → 不入库),不报错、不产生告警噪声。
- **所有远端命令均为纯静态 argv / 静态 sh -c 文本**(AC-SEC-02 同源):view/minutes/step 经白名单与数值区间校验后才进固定模板,绝无用户输入拼接。指标无敏感信息;错误体不含凭据明文。
- **与 6-1 服务器指标(ServerStatus)互不替代**:现有 loadavg/free/df 口径继续服务异常检测与多机总览;ssar 提供更丰富的细分(jiffies 分项 / TCP 重传家族 / 5s 负载)。后续可选把 ssar 采样接入 anomaly 数据源。
- **安装包缓存换版本即清旧**(`<data>/bin/ssar/` 只保留当前 tag 一份);下载失败不留半截文件(digest 校验 + 临时文件原子改名)。

## 实现位置

- 领域包 `internal/ssar/`:`download.go`(Release 下载器,仿 easytier)、`install.go`(部署 + 状态探测 + TTL 缓存)、`runner.go`(SSH 提权/免提权执行抽象,仿 diskadmin)、`collect.go`(采样脚本 + `--api` 解析)、`query.go`(白名单视图查询)、`store.go`(ssar_samples 存取)、`ssar.go`(Service 接口与装配)。
- 迁移:`internal/store/migrations/{sqlite,mysql}/0088_ssar_samples.sql`。
- HTTP 面:`internal/httpapi/ssar.go`(`router.go` 挂载,`WithSsar` 注入);审计动作 `internal/audit`(ssar_download / ssar_install)。
- 装配:`cmd/pipewright/main.go`(下载器 + 服务构造、采样 ticker 与保留清理)。
- 前端(列表 + 独立详情两页):`web/src/views/SsarMonitor.vue`(列表:包状态卡 / 服务器表 / 安装动作;点机器跳详情)、`web/src/views/SsarMonitorDetail.vue`(单机详情:总览图表 60s 自动刷新 + 细查面板)、`web/src/components/metrics/MultiLineChart.vue`(多量纲 SVG 折线)、`web/src/api/ssar.ts`;路由 `/ssar-monitor` 与 `/ssar-monitor/:id`(admin)。

## 测试

- Go(`internal/ssar/ssar_test.go` / `download_test.go`):`--api` 解析(尾行取值 / 占位符 / 噪声包裹 JSON)、argv 白名单与固定形态、参数区间校验、store 往返(升序 / null 保持 / 最近采样 / 清理)、install 完整链路与命令序列(fake runner)、非 Linux 拒绝、未缓存提示、资产挑选与架构映射。
- 前端:vitest 既有套件(含 i18n 8 语言 key parity)自动覆盖新词条;`vue-tsc` typecheck。
- 手动验收:真机安装( rpm / deb 各一台)→ overview 状态转「采集中」→ 总览图表出数 → 细查 load5s 出 5s 粒度行 → 卸载重装幂等。
