# 容器部署支持 volumes(宿主存储自动生成、升级稳定)

## 语义设计(用户已拍板:Docker 命名卷)

`cfg["volumes"]` 每行一条,镜像 `docker -v` 语义:

| 写法 | 语义 |
|---|---|
| `/var/lib/mysql` | 只写容器路径 → **自动命名卷**,卷名确定性推导 |
| `/data/app:/app/data` | source 以 `/` 开头 → 显式绑定宿主目录(原样传 docker) |
| `appdata:/app/data` | source 不以 `/` 开头 → 显式命名卷(原样传 docker) |
| 任意形式尾部 `:ro` | 只读挂载 |

自动命名卷名:`pw-<sanitizeName(容器名)>__<容器路径 / 换 ->`,如容器 `shopapp` + `/var/lib/mysql` → `pw-shopapp__var-lib-mysql`。docker 首次挂载自动创建,`docker rm -f` 不删卷。

**升级稳定的关键**:卷名从**基础容器名**(`imageContainerName(a, cfg)`,即 `cfg["containerName"]` 或产物名净化)推导,绝不掺运行时容器名/runId —— 蓝绿切换、单机滚动、实例轮转(新容器名 `<name>-r<hex>`)、健康失败回滚,全部挂同一个卷。

## 后端改动

1. **internal/deploy/image_release.go**
   - 新增 `imageVolumeArgs(cfg map[string]string, containerName string) ([]string, error)`:解析 cfg["volumes"] → 产 `-v src:dst[:ro]` 独立数组元素(不拼 shell,守 AC-SEC-02)。校验:dst 必须以 `/` 开头、段数 ≤3、模式仅 `:ro`,非法给人读错误;空行跳过。
   - `imageBaseRunArgs(cfg)` 改签名 `imageBaseRunArgs(cfg, containerName) ([]string, error)`,顺序:cpu/memory/restart/env → volumes(`-v`)→ runArgs(逃生舱仍最后,同名 flag 后写生效)。
   - 两个调用点适配(均有现成失败路径):`stageImageOne`(119 行)、`deployInstanceRollingOne`(instance_rolling.go:108)。两处都传**基础容器名**。
   - 解析成功后经 cmdLog 输出一行「卷映射 → ...」(与端口映射日志同风格),自动推导的卷名在部署日志可核对。
   - 回滚 `rollbackImage` 复用 `st.baseArgs` → `-v` 自动保持,零改动。

2. **internal/build/dag_stage_exec.go:687-698** 透传白名单加 `"volumes"`(同步 679-685 注释)。

## 前端 + 文案

3. **web/src/components/pipeline/jobConfigSchema.ts** DEPLOY_CONTAINER_FIELDS:envVars 之后、runArgs 之前加 `{ key: 'volumes', kind: 'textarea', monospace: true, placeholder: '/var/lib/mysql\n/data/app:/app/data\nappdata:/app/data' }`。

4. **i18n 8 个 locale**(zh-CN/zh-HK/en/ja/ko/es/fr/de 的 pipelineJob.ts)加 `fieldVolumesLabel`/`fieldVolumesHint`,一次加齐(keyParity.test.ts 强制)。

5. **AI 生成侧**:internal/ai/catalog.go deploy_container Description、internal/ai/generate.go:412 prompt 行补 volumes 说明。

## 测试

6. Go(stubTarget 捕获命令 + 逐元素比对,沿用现有模式):
   - `imageVolumeArgs` 表驱动:自动卷名推导 / 显式绑定 / 显式命名卷 / `:ro` / 非法(非 `/` 开头 dst、段数超)。
   - `TestImageBaseRunArgs` 适配新签名 + volumes 在 runArgs 之前。
   - `TestStageImageStructuredContainerParams` 模式:docker run 全参含 `-v pw-...`。
   - 实例轮转:新旧容器两次 run 的 `-v` 相同(升级稳定的核心断言);回滚路径 `-v` 保持。
7. 前端 jobConfigSchema.test.ts:volumes 字段存在、kind textarea、排在 runArgs 前、`splitConfig` extras=0 归属断言。

## 不做(边界)

- 卷 GC/孤儿卷清理(后续独立需求);appstore compose 路径已支持 volumes 不动;不为挂载写审计(与 ports/env 待遇一致)。
- 用户改容器名/产物名会导致卷名变化(旧卷成为孤儿),文案里提示即可。