# 高级特性与规则细节

## when — 阶段条件执行

```yaml
when:
  branches: [main, release/*]   # glob,`*` 可跨 `/` 匹配;省略=任意分支
  events: [webhook]             # manual / webhook / schedule;省略=任意触发
```

两组都写时是**与**关系。不满足 → 该阶段及全部下游记 `skipped`(不是失败)。触发类型补充:集群远程触发(mesh)按 manual 语义命中 `when`。

## gate / allowFailure

- `gate: true`:进入阶段前人工审批;批准→继续,拒绝→该阶段失败(状态 `waiting_approval`/`rejected`)。生产部署建议加。
- `allowFailure: true`:阶段失败仍放行下游,自身状态记 failed。通知、非关键检查建议加。

## runner — 构建机选择器(stage 级或脚本类 job 的 config 级)

| 写法 | 含义 |
|---|---|
| `gpu` | 带该标签的构建机 |
| `arch=arm64` | 标签键值匹配 |
| `gpu,arch=arm64` | 逗号多项(与) |
| `server:<id>` | 钉死单机 |
| `peer:<name>` | 集群共享机 |

约束:总长 ≤255,去重后 ≤16 项。空 = 项目默认构建机。

## matrix — 矩阵构建(stage 级)

```yaml
- id: stg_test
  name: 多版本测试
  kind: build
  needs: [stg_src]
  matrix:
    go: ["1.21", "1.22"]
    os: [linux]
  jobs:
    - name: 测试
      type: script
      script:
        image: golang:1.22
        commands:
          - echo "running on go $MATRIX_GO"   # 轴值只能作为容器环境变量在命令里引用
          - go test ./...
```

规则:轴名必须是标识符(字母/下划线开头);最多 8 轴;笛卡尔积 ≤50 个组合;每轴至少一个非空值。每个组合并行跑该阶段 jobs 的副本,轴值以 `$MATRIX_<大写轴名>` 注入容器环境(如 `MATRIX_GO`)。

**注意**:`MATRIX_*` 只出现在容器环境变量里,供 `commands` 中的 shell 引用;`image` 等 config 字段**不做** `${MATRIX_GO}` 展开——不要在镜像名里写占位符。

## post — 阶段后置步骤(stage 级,对标 Jenkins post)

```yaml
post:
  - condition: always        # always(默认)/ on_success / on_failure
    image: alpine:3.20
    commands:
      - echo collect logs
    workDir: app             # 驼峰 workDir(与 script 块的小写 workdir 不同!)
```

`image` 必填、`commands` 至少一条;单阶段最多 32 条。适合收集日志、失败时上报。

## services — 旁挂服务容器(stage 级,对标 GitLab services)

```yaml
services:
  - name: testdb               # 必填;即容器网络别名,阶段内唯一
    image: postgres:16         # 必填
    env:
      - POSTGRES_PASSWORD=test
    ports: ["5432:5432"]       # host:container
```

与脚本容器在同一 docker 网络,脚本里直接用服务名当主机名:`psql -h testdb`。单阶段最多 16 个。集成测试依赖数据库/Redis 时用它,不要在 commands 里自己起守护进程。

## 环境变量机制

注入到脚本类容器的变量,同名时**后者覆盖前者**:

1. 运行参数(手动触发时填的 params)
2. 上游 job 输出(见 `$PIPEWRIGHT_ENV`)
3. 流水线级「变量与缓存」(平台 UI 配置,含 secret——执行时即取即注入并全程脱敏)
4. job 自己的节点级环境变量(`script.env` 块,或 `config.envVars` 每行一条 `KEY=value`;画布表单与 YAML 等价)

`build_image` 节点同样支持节点级环境变量(`config.envVars`):dockerfile 模式注入为 docker build `--build-arg`(Dockerfile 里先 `ARG` 声明才能用),工具链模式注入为构建容器环境变量;注入优先级同上(节点级覆盖流水线级)。secret 不写在节点 env 里——用流水线级变量或 `${NAME}` 引用平台保险库。

`$PIPEWRIGHT_ENV`(对标 GITHUB_ENV):容器内脚本向该环境变量指向的文件追加 `KEY=VALUE` 行,执行器捕获后注入**同阶段后续 job** 的环境:

```yaml
commands:
  - echo "VERSION=$(cat version.txt)" >> $PIPEWRIGHT_ENV
```

容器内引用变量用普通 shell 语法 `$VAR`;**声明的流水线变量**例外——顶层 `variables:` 声明、平台「变量与缓存」填值的变量,在**仓库文件驱动的运行**里对全部 job config 字符串值做 `${NAME}` 替换(含 `script.env` 的值、`commands` 行、部署节点配置;需要环境变量时在 `env` 里写 `KEY: ${NAME}` 自行声明)。替换只发生在执行瞬间,落库/回显恒为占位符;secret 值替换后全程脱敏;`required: true` 未填值则运行被拦截并列出变量名。未声明的 `${X}` 原样保留,交给容器内 shell。git sha、分支等运行上下文**不是**环境变量,只能通过下述占位符用在支持的字段里。

## 占位符 {{...}}

| 支持的字段 | 可用占位符 |
|---|---|
| `push_image` 的 `tag`、`oss_upload` 的 `prefix`、`mesh_push` 的 `revisionLabel` | `{{runId}}` `{{branch}}` `{{commit}}` `{{project}}` `{{date}}` `{{date:yyyyMMddHHmmss}}`(UTC,默认格式 `yyyy-MM-dd-HH-mm`) |
| `notify` 的 `titleTemplate`/`bodyTemplate` | `{{project}}` `{{branch}}` `{{commit}}` `{{status}}` `{{runId}}` |
| `templated` 的 `commandTemplate`/`artifactPath` | `{{参数名}}`(取自 `params` 与 config) |

不支持占位符的字段写了 `{{...}}` 会原样保留,不会展开。

## 完整校验清单(normalizeSpec 全量规则)

写复杂流水线后逐条对照:

- 文档非空;`version` 只能是 1(可省略);至少一个 stage。
- 未知字段 → 直接报错(KnownFields 严格模式)。
- stage:`name` 非空;`kind` ∈ {source, build, deploy, notify, custom};id 在 stage+job 全局唯一。
- **恰好一个 source 阶段**;git 项目源 job 是 `git_source`,推送项目是 `pushed_source`;mesh 相关节点与项目源类型有配套约束。
- stage `needs`:引用存在、不自指、不成环(Kahn 检测)。
- job:`name`/`type` 非空;job `needs` 引用同阶段存在的 job id,不自指不成环。
- 脚本类 job:执行期要求有 `image`(缺了运行时才报"缺少运行镜像")。
- `runner` 选择器语法合法(长度 ≤255、项数 ≤16)。
- matrix:轴名标识符、≤8 轴、积 ≤50、每轴有非空值。
- post:`image` 必填、commands 非空、≤32 条;services:name 合法(`^[A-Za-z_][A-Za-z0-9_-]*$`)且阶段内唯一、≤16 个。
- `notify.statusFrom` 必须引用上游节点 name。
- `oss_upload.artifactJob`、`deploy_ssh.artifactJob` 必须是上游节点 name;`push_image.artifactJob` 必填且是同阶段 `build_image`;`deploy_container.artifactJob` 只能指向 `build_image`。
- `ecs_power`:action/selector 形态合法。
- `ai_test`:`onAiUnavailable` 枚举、`timeoutSeconds` 范围、`deployJob` 必须是**同阶段**的部署类节点 name。

## 校验途径与运行时行为

- 无 CLI 校验子命令。HTTP API:
  - `POST /api/projects/{id}/pipeline/import`,body `{"yaml": "<全文>", "save": false}`(错误码 `invalid_yaml`/`invalid_stage` 等);UI「从 YAML 导入」即此接口。
  - `GET /api/projects/{id}/pac/preview?ref=<ref>`:校验仓库里该分支的 `.pipewright.yml`,返回 `{found, valid, ref, error, stageCount, ...}`。
- 运行时加载(pacloader):按运行分支取仓库根的 `.pipewright.yml`;解析失败**静默回退画布配置**,绝不中断运行——所以"YAML 改了没生效"先怀疑文件非法被忽略。
- Parse → Marshal → Parse 无损往返;「查看源码」导出即规范 YAML。
