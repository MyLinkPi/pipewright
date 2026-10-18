在对象存储浏览页增加「新建桶」功能：在当前存储实例的账户下创建新桶，并自动登记为一份新的对象存储配置（这样它才会出现在浏览页下拉与设置页中）。

## 关键设计决策

- **建桶+登记一体**：Provider 配置绑定单桶，新建桶后自动落一条新配置（同 Type/Endpoint/AccessKeyID，新桶名，PathPrefix 为空，展示名缺省=桶名）。
- **凭据独立重封**：从源配置 vault 取出 Secret，为 New配置 Create 一条**新的** vault 凭据（绝不共享 credentialId——删除配置会级联删凭据，共享会误伤同账户其他配置）。
- **孤儿桶恢复**：服务端报“桶已存在且归己有”（BucketAlreadyOwnedByYou）→ 视为成功；报“已存在”但归属不明 → 用新桶名 dial 一个客户端 VerifyBucket，校验通过视为成功（覆盖半程失败遗留），否则报错（名被他人占用）。
- **支持两类存储**：RustFS（minio MakeBucket）与阿里云 OSS（client.CreateBucket，默认私有 ACL）。此前 EnsureBucket 不含 OSS 是“自动建桶”的保守选择；用户显式点击的建桶动作对云账户同样合理。
- **校验先于触网**：桶名校验 + 同账户重复配置检查在 dial 之前（httpapi 层可无网测试）。

## 后端改动

### 1. `internal/objectstore/objectstore.go`
- 新增错误：`ErrInvalidBucket`（桶名非法）、`ErrDuplicateBucket`（同账户同桶已登记）、`ErrBucketCreateFailed`（服务端建桶失败）
- 新增桶名规则 `bucketNameRe = ^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`（S3/OSS 交集；不含点号——点会破坏 OSS 泛域名证书校验，也规避 IP 形态桶名）
- 新增可选能力接口 `bucketCreator { CreateBucket(ctx, bucketName) error }`（与 bucketEnsurer 同模式）
- `Service` 接口新增：`CreateBucket(ctx, sourceProviderID, bucket, name string) (*Provider, error)`

### 2. `internal/objectstore/s3.go` + `oss.go`
- 两客户端实现 `CreateBucket`（RustFS: minio MakeBucket 默认参数；OSS: client.CreateBucket 私有 ACL）
- oss.go 新增 `isBucketAlreadyOwned(err)`：识别 oss.ServiceError / minio.ErrorResponse 的 `BucketAlreadyOwnedByYou` 码

### 3. `internal/objectstore/service.go` — CreateBucket 实现
流程：get 源配置 → 桶名校验（小写归一化）→ `findByTypeEndpointBucket`（type+endpoint+bucket）重复检查 → vault reveal → dial → CreateBucket（含孤儿桶恢复逻辑）→ vaultW.Create 重封凭据 → s.Create 落库（失败回滚删凭据）→ 返回新配置。新增辅助方法 `findByTypeEndpointBucket`。

### 4. `internal/httpapi/objectstore.go` + `router.go`
- 新审计动作 `objectstore.bucket_create`
- 错误映射：ErrInvalidBucket→400（提示命名规则）、ErrDuplicateBucket→409 `object_store_duplicate`、ErrBucketCreateFailed→502
- Handler：`POST /api/object-stores/{id}/buckets`，body `{bucket, name?}`（认证+CSRF+审计），成功 201 返回新配置 DTO（绝无 Secret）
- 路由注册在现有对象存储路由组

### 5. 后端测试
- `service_test.go`：fakeClient 补 `CreateBucket` stub（记录调用+错误可控）；新增用例：成功路径（建桶+登记+凭据独立）、桶名校验（非法名）、重复桶→ErrDuplicateBucket、建桶失败→ErrBucketCreateFailed、孤儿桶恢复（AlreadyExists+Verify 通过→成功）、AlreadyOwnedByYou→成功、vault 未配置
- `objectstore_test.go`（httpapi）：无网路径——重复桶→409、非法桶名→400

## 前端改动

### 6. `web/src/api/objectStores.ts`
- `CreateBucketInput {bucket, name?}` 类型 + `createObjectStoreBucket(id, input)` 函数

### 7. `web/src/views/ObjectStorage.vue`
- header 工具区加「新建桶」按钮（Plus 图标）→ 弹窗（克隆设置页 modal 样式体系：modal-scrim/modal/field/field-input）
- 弹窗字段：桶名（必填，前端同规则正则实时校验+提示）、展示名（可选，缺省桶名）；提交中/错误提示
- 成功后：toast + 重拉存储列表 + 切换下拉到新配置（触发现有 watch 自动回到根目录加载）

### 8. i18n（8 语言 objectStorage 命名空间各加 12 个词条）
createBucket / createBucketTitle / createBucketSub / bucketNameField / bucketNamePlaceholder / bucketNameHint / displayNameField / displayNamePlaceholder / createBucketConfirm / creating / cancel / createBucketOk

## 验证
- `go build ./...` + `go test ./internal/objectstore/... ./internal/httpapi/`
- `vue-tsc --noEmit` + `vitest run`（含 keyParity）+ `vite build`