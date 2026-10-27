# 节点类型完整目录(job.type)

与前端 `jobConfigSchema.ts` 和 `internal/ai/catalog.go` 对齐。**config 键以本文为准,不要凭记忆编造。** 未标"必填"的键都可省略;所有值一律写成字符串(数字加引号)。

## source(源)

### git_source — 拉取代码
每条流水线必有一个 source 阶段,git 项目的源 job 就是它。config 键:`repoUrl`/`branch`/`credentialId`/`depth`——项目绑定后会预填,YAML 里通常整个 config 都可省:

```yaml
- name: 拉取代码
  type: git_source
  summary: org/repo · main
```

### pushed_source — 集群推送源
集群对端推送型项目使用,系统生成,一般不手写。

## 脚本类(容器内跑命令)

适用 type:`script`、`custom`、`build_nodejs`、`build_java`、`build_golang`、`build_python`、`templated`(以及存量兼容的 `build_frontend`/`build_backend`)。用 `script:` 块写 image/commands/env/workdir,执行选项平级写在 `config:`:

| config 键 | 说明 |
|---|---|
| `artifactPath` | 产物路径,多行或 glob(如 `bin/app`、`dist/**`)。要被部署类节点引用就必须配 |
| `timeoutSeconds` | 任务超时,"0"=不限 |
| `retries` | 失败重试次数,"0"=不重试 |
| `cpu` / `memory` | 容器资源,如 `"1"` / `"512m"`(docker --cpus / --memory 取值) |
| `init` | `"true"` 时 docker run --init |
| `shmSize` | 共享内存,浏览器类负载如 `"1g"` |
| `artifactPackMode` / `tarLayout` / `artifactRename` | 产物打包:`tar`(默认)/`none`;`contents`(默认)/`top`;重命名 |
| `cachePaths` / `cacheKey` | 任务级构建缓存,如 `cachePaths: node_modules` |
| `runner` | job 级构建机选择器,语法同阶段级 |

语言模板的默认值(写 `script:` 会覆盖对应默认):
- `build_golang`:image `golang:1.22`,`CGO_ENABLED=0 go build -o bin/app ./...`,产物 `bin/app`
- `build_nodejs`:image `node:20`,`npm ci && npm run build`,产物 dist
- `build_java`:Maven 打包产 jar(Gradle 项目自己换 image 和命令)
- `build_python`:image `python:3.12`,`pip install -r requirements.txt && python -m build`
- `script`/`custom`:通用容器命令,跑测试、lint、扫描等

```yaml
- name: 前端构建
  type: build_nodejs
  script:
    image: node:20
    commands:
      - npm ci
      - npm run build
  config:
    artifactPath: dist
    cachePaths: node_modules
```

### templated — 参数化自定义节点
复用库里的用户自定义步骤。config:`image`、`params`(多行 `k=v` 或 `k:v`)、`commandTemplate`(内用 `{{参数名}}` 占位)、`workDir`、`artifactPath` + 上述执行选项。

## build(镜像类)

### build_image — 构建 Docker 镜像
有 Dockerfile 走 dockerfile 模式;没有就用工具链模式。config:`artifactType`(`image`/`jar`/`dist`)、`buildModel`(`dockerfile`/`toolchain`)、`dockerfilePath`、`context`、`buildTarget`(多阶段 Dockerfile 的目标 stage,即 `FROM … AS <名字>` 的名字,docker build `--target`;空 = 构建最后一个 stage)、`toolchainLanguage`(`node`/`java`/`go`/`python`/`custom`)、`toolchainVersion`、`buildCommand`。

一个 Dockerfile 出多个镜像:同阶段放多个 build_image 节点、各配不同 `buildTarget`(相同 `dockerfilePath`/`context`)。`buildTarget` 非空时镜像名自动并入 `-<target>` 后缀——本地 tag(`pipewright/<项目slug>-<target>:<commit>`)与 push_image 的远端默认镜像名都互不冲突,节点可直接并行,也无需逐节点手配 `imageName`(仍可显式配置覆盖)。只有多个 build_image 节点既不配 `buildTarget` 又配相同 `buildTarget` 时才会同名冲突,应避免。

### push_image — 推送镜像
config **必填 `artifactJob`**:要推送的镜像来源节点 name,**必须是同阶段的 `build_image`**。可选:`registry`(留空回退环境绑定仓库)、`imageName`、`tag`(支持 `{{runId}}`/`{{branch}}`/`{{commit}}`/`{{project}}`/`{{date}}`/`{{date:yyyyMMddHHmmss}}` 占位)、`credentialId`。通常紧随 build_image。

```yaml
- id: job_img
  name: 构建镜像
  type: build_image
  config:
    buildModel: dockerfile
    dockerfilePath: Dockerfile
    context: .
- name: 推镜像
  type: push_image
  needs: [job_img]   # 同阶段内等构建完成(job.needs 引用同阶段 job 的 id)
  config:
    artifactJob: 构建镜像
    tag: "{{branch}}-{{date:yyyyMMddHHmmss}}"
```

(两个 job 必须同阶段——push_image 的 artifactJob 只接受同阶段的 build_image;靠 job 级 `needs` 排先后。)

## deploy(部署类)

### deploy_ssh — 主机部署(文件产物)
经 SSH 滚动部署 jar/dist 等:`releases/<runId>/` + 原子 `current` 软链 + 重启 + 健康门控 + 失败自动回滚。config **必填 `artifactJob`**(产物来源节点 name)。常用:`selector`(构建机/目标机选择器)、`selectorMode`(`all`/`any`)、`deployMode`(`artifact`/`command`)、`artifactName`(glob 消歧)、`deployPath`、`restartCommand`、`firstBatchSize`/`batchSize`(分批滚动)、`healthPath`/`healthPort`/`healthUrl`/`healthCommand`/`healthRetries`/`healthIntervalSeconds`、服务注册绑定 `regDomainId`/`regServiceName`/`regServiceAliases`(逗号分隔额外子域名,与主域名同一服务)/`regPath`/`regPort`/`regStripPath`/`regServiceId`(一键生成回填,部署期真正消费的绑定)。

### deploy_container — 容器部署(镜像产物)
在目标机 pull + 重建容器。config **必填 `artifactJob`**(只能指向 `build_image`)、`containerName`。常用:`selector`/`selectorMode`、`ports`(如 `"8080, auto:9000, 8080:80"`)、`envVars`(每行 `KEY=value`,`#` 开头为注释行)、`volumes`(每行 `/data` 或 `宿主:容器`)、`cpuLimit`/`memoryLimit`、`restartPolicy`(`no`/`always`/`unless-stopped`/`on-failure`)、`runArgs`(自由参数)、`autoPortRange`、`registryUrl`/`registryCredentialId`(目标机登录私有仓库)、`firstBatchSize`/`batchSize`/`maxTargets`、`health*` 系列、服务注册 `reg*` 系列。

### oss_upload — 对象存储上传
config **必填 `storageId`**(平台设置页的对象存储配置)、**`artifactJob`**(上游产物节点 name)。可选:`prefix`(支持 `{{runId}}`/`{{branch}}`/`{{commit}}`/`{{project}}`/`{{date}}`/`{{date:格式}}` 占位)、`artifactName`(glob)。

### ecs_power — 阿里云 ECS 开关机
config **必填 `accountId`**、`action`(`start`/`stop`)。机器选择二选一:`selector: instances` + `instanceIds`(逗号分隔,配置时快照)或 `selector: tags` + `tagSource`(`aliyun`/`platform`)+ `tags`(逗号分隔 `k=v`,AND)。可选 `forceStop`、`waitSeconds`(默认 `"300"`,`"0"` 不等待)、`waitReachable`(`"true"` 仅 start:到达 Running 后继续等 TCP 网络可达,地址优先导入机器的 Host:Port、兜底公网/私网 IP;须配 `waitSeconds: "1".."3600"`)、`reachablePort`(探活端口 1–65535,默认 `"22"`)。适合定时启停测试环境或部署前预热。

### mesh_push — 推送到集群对等节点
把本 run 的产物/镜像推给集群(EasyTier)里的对端,首推自动在对端建对应项目。config **必填 `node`**(目标节点 id)。可选:`items`(逗号分隔产物名,空=全部)、`revisionLabel`(支持占位符)、`timeoutMinutes`。

### mesh_trigger — 启动对端节点的工作流
config **必填 `node`**。寻址二选一:`targetMode: origin`(本项目在对端的对应项目,常配在 mesh_push 之后)或 `targetMode: explicit` + `project`。可选:`params`、`revision`、`waitForCompletion`(默认 `"true"`,日志回流本节点)、`timeoutMinutes`。

### wait — 等待
解决部署时序(如等服务就绪再做健康检查)。config:`seconds`("1"–"86400",默认 `"60"`)。运行中可手动跳过。通常插在部署与健康检查之间。

### deploy_static / deploy_frontend — 网关静态发布(存量兼容)
`deploy_static`:config `artifactJob`/`artifactName` + `reg*` 服务注册绑定。`deploy_frontend` 为旧键,仍执行但不推荐新写。

## 质量类(注意:stage 的 kind 没有 quality,放 deploy/custom 阶段)

### health_check — 健康检查
config:`selector`/`selectorMode`/`serverId`(圈选目标机)、`probeMode`(`http`/`command`)、`url`、`command`、`retries`、`intervalSeconds`。失败令阶段失败,建议接在部署节点之后。

### ai_test — AI 测试
config:`target`、`deployJob`(**必须引用同阶段的部署类节点 name**——ai_test 要和部署 job 放在同一个 stage,用 job 级 `needs` 排先后,规则同 push_image 的 artifactJob)、`filter`(需求文件 glob,如 `.pipewright/tests/requirements/**/*.md`)、`onAiUnavailable`(`fail`/`skip`)、`timeoutSeconds`、`maxSteps`、`model`、`modelContextTokens`、`browserImage`。

## notify(通知)

### notify
config:`channel`(平台设置里配置的通知渠道)、`statusFrom`(上游节点 name,让 `{{status}}` 渲染该节点状态而非运行状态)、`titleTemplate`/`bodyTemplate`(占位符:`{{project}}`/`{{branch}}`/`{{commit}}`/`{{status}}`/`{{runId}}`)。通知阶段建议配 `allowFailure: true`。
