新增「对象存储浏览」页面（只读，强制分页）。参照「数据库」页模式：设置页注册实例、顶级页浏览，仅管理员可见。

## 分页设计（响应用户核心约束）

S3/OSS 协议无 offset 随机跳页能力（服务端跳过 N 条仍需遍历），采用 **marker 游标分页（keyset）**：
- 每次请求 `prefix + delimiter="/" + marker + maxKeys`（默认 100，钳制 1–1000），SDK 单页返回，绝不聚合全量列举
- `delimiter="/"` 让服务端把子目录折叠为 CommonPrefixes（目录树浏览）
- 前端保存 marker 历史栈实现"上一页/下一页"顺序翻页；`nextMarker` 为空即最后一页

## 后端改动

### 1. `internal/objectstore/objectstore.go` — 领域类型与接口
- 新增类型：
  - `ListPageInput{Prefix, Marker string; MaxKeys int}`（Prefix 以 `/` 结尾，空 = 根）
  - `ObjectEntry{Key string; Size int64; LastModified time.Time}`
  - `ListPageResult{Prefixes []string; Objects []ObjectEntry; NextMarker string}`（NextMarker 空 = 没有更多页）
- `ObjectClient` 接口新增 `ListPage(ctx context.Context, in ListPageInput) (ListPageResult, error)`
- `Service` 接口新增 `Browse(ctx context.Context, id string, prefix, marker string, maxKeys int) (*ListPageResult, error)`
- 新增领域错误 `ErrBrowseFailed`

### 2. `internal/objectstore/s3.go` — RustFS（minio-go）
- `core := minio.Core{Client: c.client}`，`core.ListObjects(bucket, prefix, marker, "/", maxKeys)`（V1 单页，返回 `ListBucketResult{Contents, CommonPrefixes, IsTruncated}`）
- NextMarker 计算：IsTruncated 时取 Contents ∪ CommonPrefixes 字典序最大 key（V1 + delimiter 语义的标准兜底）
- 过滤 `key == prefix` 的目录占位对象

### 3. `internal/objectstore/oss.go` — 阿里云 OSS
- `c.bucket.ListObjects(oss.Prefix(p), oss.Marker(m), oss.Delimiter("/"), oss.MaxKeys(n))`
- NextMarker 优先取 `res.NextMarker`，为空且 IsTruncated 时用最大 key 兜底；同样过滤占位对象

### 4. `internal/objectstore/service.go` — Browse 方法
- 走现有模式：store.get → vault.Reveal → dial
- 浏览根锚定 PathPrefix：`joinPrefixKey(p.PathPrefix, prefix)` 后规范化为尾部 `/` 形式（与上传节点写入位置一致）
- 失败返回 `ErrBrowseFailed` + describeClientError（绝不含密钥）
- `service_test.go`：fakeClient 补 `ListPage` stub；新增 Browse 单测（PathPrefix 拼接、marker 透传、占位过滤、vault 未配置/列举失败错误映射）

### 5. `internal/httpapi/objectstore.go` + `router.go` — HTTP 端点
- `makeBrowseObjectStoreHandler`：`GET /api/object-stores/{id}/objects?prefix=&marker=&maxKeys=`
- 只读无审计（与 list 一致）；svc==nil → 503；错误经 writeObjectStoreError 映射（补 ErrBrowseFailed → 502）
- 响应 DTO：`{prefixes: string[], objects: [{key, size, lastModified(RFC3339)}], nextMarker, truncated}`
- maxKeys 默认 100、钳 1–1000；prefix 归一化（去首 `/`、空或补尾 `/`）
- 路由注册在 router.go 现有对象存储路由组旁

## 前端改动

### 6. `web/src/api/objectStores.ts`
- `ObjectEntry` / `BrowseResult` 类型 + `browseObjectStore(id, {prefix?, marker?, maxKeys?})`

### 7. 新页面 `web/src/views/ObjectStorage.vue`
- 参照 Databases.vue 的顶级页结构风格（手写 CSS、无 NPagination）
- 顶部存储实例下拉（name · type · bucket）；支持 `?store=<id>` query 预选（从设置页跳入）
- 面包屑：bucket /（PathPrefix 起始段）/ 逐级目录，点击任意层级跳转
- 列表：目录行（文件夹图标 + 名称，点击进入并重置分页）在前，对象行（名称、大小、修改时间）在后
- 分页：marker 历史栈 + `‹ 第 N 页 ›` 按钮；nextMarker 空 → 禁用下一页；页大小选择（50/100/200）
- loading / 错误（HttpError message + 重试按钮）/ 空目录 EmptyState 三态

### 8. 路由与导航
- `router/index.ts`：`{ path: 'object-storage', name: 'object-storage', component: ObjectStorage, meta: { title: '对象存储', roles: ['admin'] } }`
- `AppShell.vue` adminNavItems 加导航项（@vicons/tabler 云存储类图标）；8 语言 `nav.objectStorage` 词条
- `SettingsObjectStorage.vue` 每行操作区加「浏览」按钮 → `/object-storage?store=<id>`

### 9. i18n
- 8 个语言目录各新建 `locales/<lang>/objectStorage.ts`（命名空间 `objectStorage.*`，keyParity 测试强制一致）

## 验证
- `go build ./...`、`go test ./internal/objectstore/... ./internal/httpapi/...`
- web：`npm run typecheck`（或 vue-tsc）+ 单测（keyParity）+ `npm run build`