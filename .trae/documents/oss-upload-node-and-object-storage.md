# 流水线 OSS 上传节点 + 对象存储设置

## Summary

新增两件相互配套的能力：

1. **流水线节点 `oss_upload`**：把一个上游节点产出的构建产物上传到阿里云 OSS，失败则令阶段失败并阻断下游（强失败语义，对齐 `deploy_ssh`）。
2. **设置界面的「对象存储」配置页**：多账户列表形态（对齐 `dnsprovider`），每条配置带类型（本期只支持「阿里云 OSS」）、Endpoint、Bucket、AccessKeyId（明文非机密）、AccessKeySecret（只写，走 vault 加密）。

对象存储客户端使用官方 SDK `github.com/aliyun/aliyun-oss-go-sdk/oss`（用户已确认）。

## Current State Analysis

- **流水线 job type 是自由字符串**：后端 `internal/pipeline/pipeline.go` 的 `normalizeSpec` 只校验 `job.Type` 非空，无类型白名单；新增节点无需改 job type 白名单，只需在执行器登记分支。
- **前端节点定义单一事实来源**是 [jobConfigSchema.ts](file:///c:/Users/xufan/Trae/pipewright/web/src/components/pipeline/jobConfigSchema.ts)：`JOB_TYPE_SPECS`（每类型一张声明式字段表）+ `PICKABLE_TYPES`（画廊可见清单）+ `FieldKind`（控件种类）。新增节点 = 加一个 spec + 加进 PICKABLE_TYPES + （如需要）加 `FieldKind`。
- **执行分发中枢**是 [dag_stage_exec.go](file:///c:/Users/xufan/Trae/pipewright/internal/build/dag_stage_exec.go)：`NewStageExecutor`（非 DAG 分桶）与 `runStageJobsDAG` 的 `runJob` switch（DAG 路径）两处都要登记分支；远程路径 [remote_stage_exec.go](file:///c:/Users/xufan/Trae/pipewright/internal/build/remote_stage_exec.go) 的 `runMixedNode` switch 同步。
- **最贴近的参照实现**：`runDeployJob`（独立执行器 + 通过 `b.deployer` 落地 + 任一失败→`ErrBuildFailed`）与 `runNotifyJob`（独立执行器 + 读 config）。OSS 节点采用「独立执行器 + 强失败」的组合。
- **产物读取范式**（可复用）：`deploy.Service` 用 `s.runs.ListArtifacts(ctx, runID)` 取产物，按 `metadata["sourceJob"] == cfg["artifactJob"]` 精确挑选（`pickStageArtifactExplicit`），再 `s.artStore.Open(reference)` 取真字节流上传。产物 metadata 形状：`stored`(bool)、`format`(`file`/`tar.gz`/`files`)、`filename`、`files`(清单 `[{path,key,size}]`)、`sourceJob`、`workspacePath`。`format=files` 需按清单逐文件上传。
- **Builder 依赖注入模式**：[builder.go](file:///c:/Users/xufan/Trae/pipewright/internal/build/builder.go) 结构体持有 `artStore *artifactstore.Store`、`artifactLister func(...)`、`deployer`、`notifier`，经 `WithXxx` Option 注入，`main.go` 装配。
- **设置项两套成熟范式**：`ai`（单例）与 `dnsprovider`（多 provider + 阿里云 + 密钥走 vault）。本需求选 `dnsprovider` 范式。
- **vault**：[vault.go](file:///c:/Users/xufan/Trae/pipewright/internal/vault/vault.go) 有类型枚举（`TypeDNSToken` 等）+ `validateType` + `Reveal`/`Exists`/`Update`/`Delete`；`Delete` 有「在用守卫」（检查 `dns_providers.credential_id` 等防悬挂引用）。
- **DB 迁移**：`internal/store/migrations/{sqlite,mysql}/` 成对，`go:embed` 自动加载；当前最大编号 **0071**，新迁移为 **0072**，无需改代码。
- **前端设置页**：`web/src/views/Settings.vue` 二级导航 + `web/src/router/index.ts` 子路由，每项一个 `views/settings/Settings*.vue`。
- **前端 i18n**：`web/src/i18n/locales/{zh-CN,zh-HK,en,ja,ko,es,fr,de}/<namespace>.ts`，由 `keyParity.test.ts` 强制 8 语言键对齐。
- **API 路由**：[router.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/router.go) 的 `options` + `WithXxx` + `New(webFS, authn, opts...)`；`ar` 分组 = auth，写方法额外 CSRF；审计走 `recordAudit`。
- 全仓库**当前无任何 OSS/对象存储实现或依赖**。

## Proposed Changes

### A. 数据迁移（sqlite + mysql 各一份）

新建 `internal/store/migrations/sqlite/0072_object_stores.sql` 与 `internal/store/migrations/mysql/0072_object_stores.sql`，表 `object_stores`：

| 列 | 类型(sqlite/mysql) | 说明 |
|---|---|---|
| id | TEXT/VARCHAR(64) PK | uuid |
| type | TEXT/VARCHAR(32) NOT NULL | 提供方类型；本期仅 `aliyun_oss` |
| name | TEXT/VARCHAR(255) NOT NULL | 展示名 |
| endpoint | TEXT/VARCHAR(255) NOT NULL | 如 `oss-cn-hangzhou.aliyuncs.com` |
| bucket | TEXT/VARCHAR(255) NOT NULL | 默认桶 |
| access_key_id | TEXT/VARCHAR(255) NOT NULL | 非机密，明文 |
| credential_id | TEXT/VARCHAR(64) NOT NULL | 指向 vault（AccessKeySecret） |
| created_at / updated_at | TEXT/VARCHAR(32) | RFC3339 |

参照 `0046_dns_providers.sql`（`credential_id` 不设外键，删除由 vault 在用守卫拦）。

### B. vault 扩展

文件：`internal/vault/vault.go`

1. 类型枚举新增 `TypeObjectStoreSecret = "object_store_secret"`（附注释：对象存储 AccessKeySecret，单字串）。
2. `validateType` 加入该常量。
3. `Delete` 的「在用守卫」新增一段：`SELECT COUNT(*) FROM object_stores WHERE credential_id = ?`，命中则返回 `ErrCredentialInUse`（与 dns_providers 同形）。
4. `internal/vault/masker.go` 无需改（unknown 类型走 default 全打点，安全）。

### C. 后端领域层：新建包 `internal/objectstore/`

**`objectstore.go`**（领域模型 + 接口 + 校验）
- 常量 `TypeAliyunOSS = "aliyun_oss"`；`ValidType(t)`（switch，未知 → false）。
- 领域错误：`ErrNotFound`、`ErrInvalidType`、`ErrEmptyName`、`ErrEmptyEndpoint`、`ErrEmptyBucket`、`ErrEmptyAccessKeyID`、`ErrEmptyCredentialID`、`ErrCredentialNotFound`、`ErrVaultUnconfigured`、`ErrTestFailed`、`ErrUploadFailed`。
- `Provider{ID, Type, Name, Endpoint, Bucket, AccessKeyID, CredentialID, CreatedAt, UpdatedAt}`（**绝不含 Secret**）。
- `CreateInput{Type, Name, Endpoint, Bucket, AccessKeyID, CredentialID}` / `UpdateInput{Name, Endpoint, Bucket, AccessKeyID *string}`。
- `TestResult{OK bool; LatencyMs int64; Detail string; Err error}`。
- `ObjectClient` 抽象（注入便于单测，不触网）：
  - `VerifyBucket(ctx) error`（连通性/桶存在）
  - `PutObject(ctx, objectKey string, body io.Reader) error`
- `Service` 接口：`List/Get/Create/Update/Delete/Test` + `Upload(ctx, providerID, objectKey string, body io.Reader) error`（供 build 节点调用）。
- `validateCreate/validateUpdate`（归一化 + 字段非空 + `ValidType`）。

**`store.go`**：参数化 SQL 持久层（`NewStore(db)`，`list/get/create/update/del`，`scanProvider` 永不读密文列），照抄 `dnsprovider/store.go` 结构。

**`service.go`**：
- `vaultReader{ Reveal(id) (string,error); Exists(id) (bool,error) }` 最小接口（照 dnsprovider）。
- `New(db *sql.DB, v vault.Vault) Service`。
- `dialFactory func(providerType, endpoint, accessKeyID, accessKeySecret string) ObjectClient`（生产工厂 `prodDialFactory`；测试注入 fake）。
- `Create` 先 `vault.Exists(credentialID)` 校验（不存在 → `ErrCredentialNotFound`）。
- `Test`：取 Secret → 造 client → `VerifyBucket` → 返回耗时/结果（错误人话，绝不含密钥）。
- `Upload`：取 Provider + Reveal Secret → 造 client → `PutObject`。Secret 用完即弃。

**`oss.go`**：`aliyunObjectClient` 实现，用官方 SDK：
```go
import "github.com/aliyun/aliyun-oss-go-sdk/oss"
client, _ := oss.New(endpoint, accessKeyID, accessKeySecret)
bucket, _ := client.Bucket(bucketName)
bucket.PutObject(objectKey, body)      // PutObject(ctx, key, io.Reader)
bucket.IsObjectExist("") / GetBucketInfo() // VerifyBucket 触网探测
```
错误映射到 `ErrTestFailed`/`ErrUploadFailed`（人话，剥离可能含 AK 的 SDK 原始串——仅保留 `oss.Err` code/message）。

**测试**：`objectstore_test.go`（ValidType/validate）、`store_test.go`、`service_test.go`（fake vault + fake ObjectClient）、`oss_test.go`（仅签名/键构造与错误映射，不触真网）。

**go.mod**：`go get github.com/aliyun/aliyun-oss-go-sdk/oss`（写入 require）。

### D. 后端 HTTP 层：新建 `internal/httpapi/objectstore.go`

照 `dnsprovider.go` / `ai_settings.go`：
- 审计常量：`objectstore.create` / `.update` / `.delete` / `.test`，target `object_store`。
- `objectStoreDTO{ID, Type, Name, Endpoint, Bucket, AccessKeyID, CredentialConfigured bool, CreatedAt}`（**不返回 credentialId / Secret**）。
- `writeObjectStoreError`（领域错误 → 契约错误码；`ErrVaultUnconfigured` → 503，`ErrCredentialNotFound` → 422 等）。
- handlers：
  - `makeListObjectStoresHandler` — `GET /api/object-stores` → `{items:[...]}`
  - `makeCreateObjectStoreHandler` — `POST /api/object-stores`，body 含只写 `secret`：先 `v.Create(vault.TypeObjectStoreSecret)`，再 `svc.Create`，失败回滚凭据（照 `makeCreateDNSProviderHandler`）
  - `makeUpdateObjectStoreHandler` — `PUT /api/object-stores/{id}`，可选 `secret` 轮换（vault 原地重封，credentialId 不变）
  - `makeDeleteObjectStoreHandler` — `DELETE /api/object-stores/{id}`，级联删凭据（尽力而为）
  - `makeTestObjectStoreHandler` — `POST /api/object-stores/{id}/test` → `{ok, latencyMs, detail, error}`

**测试**：`internal/httpapi/objectstore_test.go`（CRUD + secret 只写不回显 + 错误映射）。

### E. 路由与装配

1. `internal/httpapi/router.go`：`options` 加 `objectStores objectstore.Service`；新增 `WithObjectStores(s)`；在 `ar` 分组注册上述 5 条路由（GET 仅 auth，其余 auth+CSRF+审计）。
2. `cmd/pipewright/main.go`：
   - 构造 `objectStoreSvc := objectstore.New(st.DB, credVault)`。
   - Builder 装配处追加 `build.WithStageObjectUploader(objectStoreSvc)`。
   - `httpapi.New(...)` 追加 `httpapi.WithObjectStores(objectStoreSvc)`。

### F. 节点执行：`internal/build/`

1. **`builder.go`**：
   - 新增字段 `objectUploader stageObjectUploader`（`stageObjectUploader` 是 build 包内定义的最小接口：`Upload(ctx, providerID, objectKey string, body io.Reader) error`，由 `objectstore.Service` 满足）。
   - 新增 `WithStageObjectUploader(u stageObjectUploader) BuilderOption`（照 `WithStageDeployer`）。nil → 节点按「未注入，跳过」处理（向后兼容）。
2. **`dag_stage_exec.go`**：
   - 新增 `isObjectUploadJob(t) bool`（`t == "oss_upload"`）。
   - `NewStageExecutor` 分桶 switch 增 `ossUploadJobs` 桶；`needsBuild` 判定与「诚实占位」条件把该桶计入（非空即不占位）。
   - 非 DAG 执行循环新增分支 → `runOssUploadJob`。
   - `runStageJobsDAG` 的 `runJob` switch 新增 `case isObjectUploadJob(jb.Type): return b.runOssUploadJob(ctx, jrep, jb, r)`。
   - 新增 `runOssUploadJob(ctx, rep, jb pipeline.Job, r *run.Run) error`：
     - `b.objectUploader == nil` → 记日志「对象存储服务未注入，跳过」并放行（与 deployer nil 一致）。
     - 读 config：`storageId`（必填，空 → 记 stderr + 返回 `ErrBuildFailed`）、`artifactJob`（必填）、`artifactName`（可选 glob）、`prefix`（可选）。
     - 前缀模板渲染：把 `prefix` 中 `{{runId}}/{{branch}}/{{commit}}/{{project}}` 用 `r.ID / r.Trigger.Branch / r.Trigger.Commit / r.ProjectName` 替换（新写一个包内小函数，避免依赖 notify 模板）。
     - 经 `b.artifactLister(ctx, r.ID)` 取产物，按 `metadata["sourceJob"]==artifactJob` 挑候选；`artifactName` 非空按 `path.Match` 对产物名/`workspacePath` 基名消歧；0 件 → 记 stderr + `ErrBuildFailed`；多件且无 glob → 取第一件并告警（build 包不引入 deploy 的 `ErrArtifactAmbiguous`，保持包边界）。
     - 只处理**非 image 产物**（`jar`/`dist`/`archive`；image 无字节可传 → 记 stderr + `ErrBuildFailed`，提示改用镜像推送节点）。
     - 取字节：`b.artStore.Open(a.Reference)`；按 `format`：
       - `files`：解 `metadata["files"]` 清单，逐 `artStore.Open(key)` 上传到 `objectKey = joinPrefix(prefix, entry.path)`。
       - 其它（`file`/`tar.gz`）：单流上传到 `objectKey = joinPrefix(prefix, filename)`，`filename` 取 `metadata["filename"]`，缺省用 `a.Name`。
     - 每个对象成功打一行 `→ 已上传 oss://<bucket>/<key>` 日志；任一失败 → 记 stderr + 返回 `ErrBuildFailed`（强失败）。
     - 注：`objectstore.Service.Upload` 内部按 providerID 解析 bucket/endpoint/AK，build 侧只需 `storageId + objectKey + reader`。
3. **`remote_stage_exec.go`**：`runMixedNode` switch 增 `case isObjectUploadJob(...)` 分支（非 script 类型由本地执行器跑，此处登记以保证拓扑/依赖解析正确）。

**测试**：`internal/build/dag_stage_exec_test.go` 增 `oss_upload` 用例（fake uploader + fake artifactLister + fake driver/artStore），覆盖：正常单文件上传、`format=files` 多文件、storageId 为空失败、artifactJob 无产物失败。

### G. 领域校验（保存期）

1. `internal/pipeline/`：新建 `object_upload.go`，导出一个 `validateObjectUpload(spec, ...)`（照 `notify_status.go` 的 `validateNotifyStatusFrom`）：校验 `oss_upload` 节点的 `artifactJob` 非空且引用的节点是**上游节点**（同 stage 前置或前序 stage）。在 `normalizeSpec`（`pipeline.go` 的 `validateNotifyStatusFrom` 挂接处附近）挂上。
   - `storageId` 不做保存期必填（用户可能先画节点后配存储），改由运行期强失败兜底。
2. **测试**：`internal/pipeline/object_upload_test.go`（合法/非法引用）。

### H. AI 节点目录

1. `internal/ai/catalog.go`：`BuiltinNodeCatalog()` 增 `NodeKind{Type:"oss_upload", Label:"OSS 上传", Category:"deploy", Description: "把上游节点的产物上传到阿里云 OSS。config 必填 storageId(对象存储配置)、artifactJob(产物来源节点);可选 prefix/artifactName。"}`。
2. `internal/ai/generate.go` prompt（可选）：在节点用法段落补一句 oss_upload 说明。
3. **测试**：`internal/ai/catalog_test.go` 补断言。

### I. 前端：节点定义与控件

1. **`web/src/components/pipeline/jobConfigSchema.ts`**：
   - `FieldKind` 新增 `'objectStore'`。
   - `JOB_TYPE_SPECS` 新增：
     ```ts
     oss_upload: {
       type: 'oss_upload', label: t('pipelineJob.typeOssUploadLabel'),
       description: t('pipelineJob.typeOssUploadDesc'), accent: 'primary', category: 'deploy',
       fields: [
         { key:'storageId', label: t('pipelineJob.fieldObjectStoreLabel'), kind:'objectStore',
           hint: t('pipelineJob.fieldObjectStoreHint') },
         { key:'artifactJob', label: t('pipelineJob.fieldArtifactJobLabel'), kind:'producer',
           hint: t('pipelineJob.fieldArtifactJobHint') },
         { key:'artifactName', label: t('pipelineJob.fieldArtifactNameLabel'), kind:'text',
           monospace:true, placeholder:'app-*.jar', hint: t('pipelineJob.fieldArtifactNameHint'),
           when: (c) => !!(c.artifactJob||'').trim() },
         { key:'prefix', label: t('pipelineJob.fieldOssPrefixLabel'), kind:'text', monospace:true,
           placeholder:'releases/{{runId}}', hint: t('pipelineJob.fieldOssPrefixHint') },
       ],
     },
     ```
   - `PICKABLE_TYPES` 在 `push_image` 之后插入 `'oss_upload'`。
2. **`web/src/components/pipeline/JobDrawer.vue`**：
   - props 新增 `objectStores?: ObjectStore[]`（import `ObjectStore` 类型）。
   - 模板新增 `v-else-if="field.kind === 'objectStore'"` 的 `<select>`（选项 = 对象存储列表；未选提示 `pipelineJob.objectStoreUnselected`）。
3. **`web/src/components/pipeline/PipelineCanvas.vue`**：props 新增 `objectStores?: ObjectStore[]`，透传给 `JobDrawer`。
4. **`web/src/views/ProjectPipeline.vue`**：`Promise.all` 加载处追加 `listObjectStores().catch(() => [])`，`objectStores.value` 传给 `<PipelineCanvas :object-stores="objectStores">`。
5. **`web/src/components/pipeline/JobTypeIcon.vue`**：`ICONS` 新增 `oss_upload` 的 SVG 路径（云/上传箭头）。

### J. 前端：设置页

1. **`web/src/api/objectStores.ts`**（照 `dnsProviders.ts`）：类型 `ObjectStore`、`CreateObjectStoreInput`（含只写 `secret`）、`UpdateObjectStoreInput`（可选 `secret`）、`TestObjectStoreResult`；函数 `listObjectStores/createObjectStore/updateObjectStore/deleteObjectStore/testObjectStore`。
2. **`web/src/views/settings/SettingsObjectStorage.vue`**（照 `SettingsDnsProviders.vue`）：列表 + 添加/编辑弹窗；字段：类型（下拉，本期仅「阿里云 OSS」）、名称、Endpoint、Bucket、AccessKeyId、AccessKeySecret（只写，编辑时留空=保留/轮换）、「测试连接」按钮；dirty guard。
3. **`web/src/router/index.ts`**：懒加载声明 `SettingsObjectStorage` + 子路由 `/settings/object-storage`。
4. **`web/src/views/Settings.vue`**：导航加 `<router-link to="/settings/object-storage">{{ t('settingsHub.navObjectStorage') }}</router-link>`。
5. **`web/src/api/credentials.ts`**：`CredentialType` 增 `'object_store_secret'`；`SYSTEM_MANAGED_CREDENTIAL_TYPES` 增 `'object_store_secret'`（防止在通用凭据页误列/误删）。

### K. 前端 i18n（8 语言齐备）

- 新建 `web/src/i18n/locales/{zh-CN,zh-HK,en,ja,ko,es,fr,de}/settingsObjectStorage.ts`（命名空间 `settingsObjectStorage`）。
- 8 语言 `settingsHub.ts` 增 `navObjectStorage`。
- 8 语言 `pipelineJob.ts` 增 `typeOssUploadLabel/Desc`、`fieldObjectStoreLabel/Hint`、`fieldOssPrefixLabel/Hint`、`objectStoreUnselected`。
- 自检：`web/src/i18n/keyParity.test.ts` 会强制键对齐。

### L. 前端测试

- `web/src/components/pipeline/jobConfigSchema.test.ts`：补 `oss_upload` spec/keys/字段断言。
- `web/src/api/objectStores.test.ts`：补 CRUD 封装的单测（照现有 `*.test.ts` 模式）。

## Assumptions & Decisions

- **配置形态**：多账户列表（用户确认）。节点通过 `storageId` 引用某条配置。
- **SDK**：使用官方 `aliyun-oss-go-sdk/oss`（用户确认）。
- **失败语义**：强失败，阻断下游（用户确认）；仅当对象存储服务未注入（测试/未装配）时才放行。
- **节点 type 命名** `oss_upload`；分类归 `deploy`（产物交付环节）；设置页路由 `/settings/object-storage`。
- **AccessKeyId 明文存储可回显**（非机密，与 dnsprovider 的 api_id 一致）；**AccessKeySecret 落 vault 加密、只写不回显**。
- **引用完整性**：保存期只校验 `artifactJob`（须为上游节点）；`storageId` 缺失留待运行期报错。对象存储被删除时不额外做流水线引用守卫（与 dnsprovider 现状一致，悬空引用在运行期报「找不到对象存储」）。
- **prefix 模板**：仅支持 `{{runId}}/{{branch}}/{{commit}}/{{project}}` 四个占位（build 包内实现，不引入 notify 模板依赖）。
- **不处理镜像产物**：`oss_upload` 只上传文件类产物（jar/dist/archive）；命中 image 产物直接报错提示。
- **后端用户可见错误文案**：硬编码中文（与 `dnsprovider.go`/`ai_settings.go` 一致），不走 `internal/i18n` 包。
- **不改动的部分**：`internal/pipelineyaml/pipelineyaml.go`（非脚本类 config 自动经 `config:` 块往返）、`internal/pipeline/pipeline.go` 的 `isValidKind`（只管 stage kind）、`internal/pipeline/settings.go`。

## Verification

1. **编译**：`go build ./...`；`go vet ./...`。
2. **后端测试**：`go test ./internal/objectstore/... ./internal/build/... ./internal/pipeline/... ./internal/httpapi/... ./internal/ai/... ./internal/vault/...`。
3. **前端测试**：在 `web/` 下 `npm run test:unit`（或脚本名以 package.json 为准），重点 `keyParity`、`componentKeys`、`jobConfigSchema`。
4. **前端类型/构建**：`npm run build`（或 `vue-tsc`）。
5. **端到端手测**：
   - 设置页新增一条阿里云 OSS 配置 → 「测试连接」返回 ok；编辑时 Secret 留空不轮换、填新值轮换；删除后对应 vault 凭据消失。
   - 画布新增 `oss_upload` 节点，选对象存储 + 选上游构建节点产物 + 填 prefix → 保存（无校验错误）→ 运行 → 运行日志出现逐文件「已上传 oss://...」；未配 storageId 时运行失败并给出人读提示。
6. **回归**：确认既有 `deploy_ssh`/`notify`/`push_image` 节点行为不变；`keyParity.test.ts` 通过。

## 改动文件清单（速查）

后端新增：`internal/objectstore/{objectstore.go,store.go,service.go,oss.go,*_test.go}`、`internal/httpapi/objectstore.go(+_test)`、`internal/pipeline/object_upload.go(+_test)`、`internal/store/migrations/{sqlite,mysql}/0072_object_stores.sql`。

后端修改：`internal/vault/vault.go`、`internal/build/builder.go`、`internal/build/dag_stage_exec.go`、`internal/build/remote_stage_exec.go`、`internal/httpapi/router.go`、`internal/ai/catalog.go(、generate.go)`、`internal/pipeline/pipeline.go`、`cmd/pipewright/main.go`、`go.mod`/`go.sum`。

前端新增：`web/src/api/objectStores.ts(+_test)`、`web/src/views/settings/SettingsObjectStorage.vue`、`web/src/i18n/locales/*/settingsObjectStorage.ts`（8 份）。

前端修改：`web/src/components/pipeline/jobConfigSchema.ts(|_test)`、`JobDrawer.vue`、`PipelineCanvas.vue`、`JobTypeIcon.vue`、`web/src/views/ProjectPipeline.vue`、`web/src/router/index.ts`、`web/src/views/Settings.vue`、`web/src/api/credentials.ts`、`web/src/i18n/locales/*/settingsHub.ts`（8 份）、`web/src/i18n/locales/*/pipelineJob.ts`（8 份）。
