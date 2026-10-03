# test-pipe 流水线功能演示仓库计划

目标:在 `C:\Users\xufan\Trae\test-pipe`(已推到 Codeup 的空仓库)里建一组**自包含、零依赖优先**的演示应用 + 一套 Pipewright 平台配置文档,尽量覆盖流水线全部功能。**不碰任何实例/凭据/真实部署**,实例与机器配置由你在目标环境自行完成。

## 一、仓库布局(演示应用矩阵)

```
test-pipe/
├── README.md                  # 总览:演示矩阵 → 功能覆盖对照 + 快速开始
├── .pipewright.yml            # 流水线即代码演示(覆盖最全的一条:源→构建→审批→镜像→部署→健康→通知)
├── apps/
│   ├── hello-node/            # Node.js 构建:零依赖 package.json,build 脚本产出 dist/
│   │                          #   演示 build_nodejs + artifactPackMode=none(文件清单)+ tarLayout
│   ├── hello-go/              # Golang 构建:零依赖 go.mod+main.go,产出单文件二进制
│   │                          #   演示 build_golang + artifactRename(部署落盘名)
│   ├── hello-java/            # Maven 最小工程(单类 + 单测),产出 target/*.jar
│   │                          #   演示 build_java + jar 类主机部署(java -jar 探活)
│   ├── hello-python/          # Python 工程(pyproject+setuptools),产出 dist/*.whl
│   │                          #   演示 build_python
│   └── hello-image/           # 静态服务镜像:Dockerfile(nginx:alpine)+ index.html + docker-compose.yml
│                              #   演示 build_image(dockerfile/toolchain 两模式)+ push_image + deploy_container
├── tools/
│   └── ci-scripts/            # script 节点用的小脚本(lint/自检,不依赖构建)
│                              #   演示 script / custom 节点、templated 自定义节点、矩阵构建
└── docs/
    ├── pipelines.md           # 每个演示项目的逐节点配置(可直接照抄的 config 键值)
    ├── features.md            # 功能覆盖对照表 + 每项验证步骤(含故意失败触发回滚/停止铺开的演示法)
    ├── triggers.md            # 触发器演示:Codeup webhook(path filter)/ cron / 流水线链 chain
    └── advanced.md            # runner 构建机、网关联动(servicereg,可选)、复用库、分支差异化 .pipewright.yml
```

## 二、功能覆盖对照(写入 docs/features.md)

| 功能 | 覆盖方式 |
|---|---|
| git_source | 各项目源阶段(Codeup SSH remote) |
| build_nodejs / java / golang / python | 四个 hello-* 应用一一对应 |
| script / custom / templated | tools/ci-scripts + 「存为自定义节点」演示 |
| build_image(dockerfile / toolchain) | hello-image;toolchain 模式在文档给对照配置 |
| push_image | hello-image + registry 配置示例(自建/harbor/dockerhub 三选) |
| deploy_ssh 文件部署 | hello-node(dist)与 hello-go/jar;releases+current 软链机制说明 |
| deployMode=command | 文档示例:仅执行重启命令的配置类部署 |
| deploy_container | hello-image:ports/runArgs/registryCredentialId/健康门控/回滚上一镜像 |
| 产物精确绑定 artifactJob(+artifactName) | 所有部署节点配置示例(强调无「自动」) |
| artifactPackMode / tarLayout / artifactRename | hello-node(none/清单)、hello-go(rename);tar+top 给对照配置 |
| health_check 节点(http / command) | 部署后接健康门控节点;平台本机兜底探测说明 |
| 阶段审批门 gate | .pipewright.yml 里 gate: true + 运行页批准/拒绝演示 |
| when 条件 / matrix / needs(阶段级+任务级) | .pipewright.yml 示例 + 文档 |
| 标签选择器 + selectorMode(且/或) | 服务器打标签示例;≥2 台才能演示「任一」,文档注明 |
| 滚动批次 firstBatchSize/batchSize | 多机部署演示法 + 故意失败停止铺开的验证 |
| 预检故障机优先 / 失败自动回滚 | docs/features.md 的「故意失败」演示(健康检查指向未启动端口) |
| 重试(含 pending 继续铺开) | 停止铺开后的重试按钮演示 |
| runner 构建机 | stage runner 选择器配置(标签圈选构建机) |
| 流水线即代码 | 根 .pipewright.yml(导入/驱动两种用法)+ 分支差异说明 |
| 触发器 | docs/triggers.md:webhook(Codeup 配置步骤)/ cron / chain |
| 复用库自定义节点 | 存为自定义节点 → 跨项目复用 |
| 网关联动(摘挂/maxSurge,可选) | docs/advanced.md:前置(网关主机+服务注册)与验证步骤 |

## 三、实现要点

- 所有应用**零第三方依赖优先**:Node 用纯 `node build.js` 生成 dist(无需 npm ci,文档注明 commands 调整);Go 零依赖秒编译;Java/Python 首次构建需拉包,文档注明时长预期。
- 根 `.pipewright.yml` 严格对齐平台 `internal/pipelineyaml` 的 schema(执行时先读该包确认字段:version/stages[].id/name/kind/jobs[].type/config/script 块),确保「从 YAML 导入」可直接成功。
- 每个应用的构建命令与平台预填模板保持一致(node:20 / maven:3.9-eclipse-temurin-21 / golang:1.22 / python:3.12),改动能最小。
- 文档中所有 config 键值与新表单一一对应(artifactJob/selectorMode/firstBatchSize/batchSize/artifactPackMode/tarLayout/artifactRename/healthUrl/gatewayService/registryCredentialId)。
- 文档强调:凭据(Codeup ssh_key)、服务器与构建机登记、master key 均由你在目标环境配置;文档给出「应该长什么样」的对照。

## 四、收尾

- 完成后在 test-pipe **本地提交**(git add + commit,**不 push**,推送由你执行);提交前用 `git -C` 自检目录结构完整。
- 不启动任何 Pipewright 实例,不写任何凭据,不做任何真实部署。