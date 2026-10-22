---
name: pipewright-yaml
description: 编写、修改、排错 Pipewright 平台的流水线即代码文件 .pipewright.yml(格式规范、全部节点类型目录、引用与大小写规则、校验清单)。只要用户提到 .pipewright.yml、pipewright 流水线、流水线即代码、pipeline as code、pipeline YAML、GitOps 流水线,或要求为使用 Pipewright CI 的仓库创建/修改构建部署流水线、排查流水线配置报错(unknown field / invalid stage / 422 / 改了没生效),都应使用本技能——即使用户没有说出 .pipewright.yml 这个文件名。
---

# 编写 .pipewright.yml(Pipewright 流水线即代码)

## 心智模型:两层数组,不是 GitHub Actions

`.pipewright.yml` 放在仓库根目录,整条流水线是 **`version` + `stages[] → jobs[]`** 两层结构(风格接近 Jenkins/云效)。解析器以严格模式读取:**任何未知字段直接让整个文件报错**(不是静默忽略)。所以写每个键之前确认它在本文档或参考文件里列出过,拿不准就查,不要凭 GitHub Actions / GitLab CI 的记忆类比编造。

| 其他 CI 的概念 | .pipewright.yml 的对应物 |
|---|---|
| `on:` / `trigger:` 触发器 | **不在 YAML 里**——触发规则、定时 cron、分支/路径过滤都在平台 UI 配置 |
| `runs-on:` | stage 的 `runner` 字段(构建机选择器,见 references/advanced.md) |
| `steps:` + `uses:` | job 没有 steps;一个 job 就是一个节点,`type` 决定行为;复用自定义步骤靠 `templated` 节点 |
| 顶层 `env:` / `secrets:` | **不在 YAML 里**——变量与 secret 在平台 UI「变量与缓存」配置 |
| `${{ github.sha }}` | 仅个别字段支持 `{{branch}}`/`{{commit}}` 等占位符(见 references/advanced.md) |

## 完整示例

```yaml
version: 1
stages:
  - id: stg_src
    name: 流水线源
    kind: source
    jobs:
      - name: 拉取代码
        type: git_source
        summary: org/repo · main

  - id: stg_build
    name: 构建与测试
    kind: build
    needs: [stg_src]
    jobs:
      - name: 构建
        type: build_golang
        script:
          image: golang:1.23
          commands:
            - go vet ./...
            - CGO_ENABLED=0 go build -o bin/app ./...
          env:
            GOFLAGS: -count=1
        config:
          artifactPath: bin/app
          timeoutSeconds: "600"

  - id: stg_deploy
    name: 部署
    kind: deploy
    needs: [stg_build]
    gate: true                     # 进入本阶段前人工审批,拒绝则阶段失败
    when:
      branches: [main, release/*]  # glob;不满足则本阶段及下游记 skipped(非失败)
      events: [webhook]            # manual / webhook / schedule;两组都写是"与"关系
    jobs:
      - name: 主机部署
        type: deploy_ssh
        config:
          artifactJob: 构建         # 引用 job 的 name(不是 id!)
          healthPath: /healthz

  - id: stg_notify
    name: 通知
    kind: notify
    needs: [stg_deploy]
    allowFailure: true             # 通知失败不阻塞流水线
    jobs:
      - name: 钉钉通知
        type: notify
        config:
          statusFrom: 主机部署       # {{status}} 渲染该上游节点的状态
          titleTemplate: "{{project}} {{branch}} 部署 {{status}}"
```

## 阶段(stage)字段

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `id` | string | 自动生成 | 被其他阶段的 `needs` 引用时必须显式写 |
| `name` | string | 必填 | 人读名,可用中文 |
| `kind` | string | 必填 | `source` / `build` / `deploy` / `notify` / `custom`。**没有 `quality`**——健康检查类 job 放 `deploy` 或 `custom` 阶段 |
| `needs` | []string | 无 | 上游**阶段 id** 列表;引用必须存在、不能自指/成环。没写 needs 的阶段作为起点执行;全流水线一个 needs 都没有时按声明顺序线性执行 |
| `allowFailure` | bool | false | 阶段失败仍放行下游(自身仍记 failed) |
| `gate` | bool | false | 进入前人工审批 |
| `when` | object | 无条件 | `branches`(glob)+ `events`(`manual`/`webhook`/`schedule`) |
| `runner` | string | 项目默认 | 构建机选择器,语法见 references/advanced.md |
| `matrix` / `post` / `services` | — | 无 | 高级特性,见 references/advanced.md |
| `jobs` | [] | 空 | 同阶段多个 job 默认并行 |

## 任务(job)字段

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `id` | string | 自动生成 | 仅被**同阶段**其他 job 的 `needs` 引用时需要 |
| `name` | string | 必填 | **流水线内保持唯一**——`artifactJob`/`statusFrom`/`deployJob` 等跨 job 引用按 name |
| `type` | string | 必填 | 节点类型。唯一权威清单在 **references/node-types.md**,选型时必读 |
| `summary` | string | 空 | 卡片副标题展示文本 |
| `needs` | []string | 无 | 同阶段其他 job 的 **id**;不写=并行,写了=等它完成 |
| `script` | object | — | 脚本类 job 专用,见下 |
| `config` | map[string]string | — | 自由 KV 透传。**值必须是字符串**——数字也要加引号(如 `timeoutSeconds: "600"`) |

### script 块(脚本类 job)

```yaml
script:
  image: golang:1.23     # 必填
  commands:              # 至少一条非空,保序执行
    - go test ./...
  env:                   # 明文环境变量;secret 走平台 UI,不能写在这里
    CGO_ENABLED: "0"
  workdir: src/app       # 注意:script 块内是小写 workdir
```

`script` 与 `config` 可并存(同名键 script 优先)。脚本类 job 的执行选项(`artifactPath`/`timeoutSeconds`/`retries`/缓存等)平级写在 `config:` 里,完整清单见 references/node-types.md。

## 硬规则清单(写完逐条自查)

1. **恰好一个** `kind: source` 阶段(通常放第一个);git 项目的源 job 必须是 `git_source`,集群推送项目是 `pushed_source`。
2. `config` 的值全部是字符串,数字加引号:`waitSeconds: "300"`、`seconds: "60"`。
3. 大小写:`allowFailure`、post 的 `workDir`、config 键 `workDir` 是驼峰;**script 块内的 `workdir` 是小写**。写错 = 整个文件报错。
4. 引用三态:`stage.needs` → 阶段 id;`job.needs` → 同阶段 job id;`artifactJob`/`statusFrom`/`deployJob` → job **name**。
5. `push_image` 的 `artifactJob` 必须指向**同阶段**的 `build_image` 节点;`ai_test` 的 `deployJob` 必须指向**同阶段**的部署类节点(用 job 级 `needs` 排先后);`deploy_container` 的 `artifactJob` 只能指向 `build_image`;`oss_upload`/`deploy_ssh` 的 `artifactJob` 必须是上游节点 name。
6. 不写任何本文档及参考文件未列出的键。
7. 触发器、定时、变量、secret、全局缓存不写进 YAML。

## 编写流程

1. **摸底仓库**:看 `package.json`/`pom.xml`/`go.mod`/`requirements.txt`/`Dockerfile` 决定构建方式(有 Dockerfile 时优先考虑 `build_image`)。
2. **画阶段图**:source → build(测试、构建可拆成并行 job)→ [gate] deploy → notify(配 `allowFailure: true`)。
3. **逐个选 job type**:打开 references/node-types.md 对照,config 键以它为准,不要凭记忆编。
4. 需要矩阵构建、旁挂服务(postgres 等测试依赖)、阶段后置步骤、条件执行、指定构建机时,读 references/advanced.md。
5. 按「完整示例」的形态产出 YAML,对照硬规则清单自查。

## 校验与排错

- **没有 CLI 校验命令**。能访问平台时:
  - `POST /api/projects/{id}/pipeline/import`,body `{"yaml": "<全文>"}`——解析失败返回 422 和人读错误定位;
  - `GET /api/projects/{id}/pac/preview?ref=<分支>`——校验仓库里实际的 `.pipewright.yml`。
- **运行时陷阱**:仓库里的 YAML 无效时平台会**静默回退到画布配置**继续跑。用户反馈"改了 YAML 但没生效",第一反应应是文件里有错被忽略了,先用上面的接口校验。
- Parse 与序列化无损往返;平台「查看源码」导出的 YAML 就是规范形态,可作参照。

## 参考文件

- **references/node-types.md**——全部 job type 及各自 config 键(选型与写 config 时必读)。
- **references/advanced.md**——matrix / post / services / when / runner / gate 细节、占位符、环境变量注入顺序、完整校验规则。
