# RustFS:应用商店模板 + 对象存储新类型 + 部署自动登记

三件事:①应用商店新增 RustFS 内置模板;②对象存储设置新增 `rustfs` 类型(S3 兼容,新增 minio-go 客户端);③从应用商店部署 RustFS 成功后,自动把它登记为一条对象存储配置(建桶 + 凭据入 vault + 落库)。

两处 `type` 列均为应用层校验的自由文本,**无需数据库迁移**。RustFS 部署细节按官方文档:镜像 `rustfs/rustfs`,S3 API 端口 9000、控制台 9001,凭据环境变量 `RUSTFS_ACCESS_KEY`/`RUSTFS_SECRET_KEY`,数据目录 `/data`。

## 一、后端:应用商店新增 RustFS 模板

**`internal/appstore/builtin.go`** — `builtins` 切片追加(仿 minio 条目,启动时 `EnsureBuiltins` 幂等 seed,老安装重启即出现):

```go
{
    Name:        "rustfs",
    DisplayName: "RustFS",
    Description: "Rust 高性能 S3 兼容对象存储(含控制台);部署成功后自动登记到「设置 → 对象存储」。",
    Icon:        "🦀",
    ComposeYAML: `services:
  rustfs:
    image: rustfs/rustfs:{{version}}
    restart: unless-stopped
    command: ["/data"]
    environment:
      RUSTFS_ACCESS_KEY: {{access_key}}
      RUSTFS_SECRET_KEY: {{secret_key}}
      RUSTFS_ADDRESS: ":9000"
      RUSTFS_CONSOLE_ADDRESS: ":9001"
      RUSTFS_CONSOLE_ENABLE: "true"
    ports:
      - "{{api_port}}:9000"
      - "{{console_port}}:9001"
    volumes:
      - rustfs-data:/data
volumes:
  rustfs-data:
`,
    Params: []ParamSpec{
        {Name: "version",      Label: "镜像版本",     Type: ParamTypeString, Default: "latest"},
        {Name: "access_key",   Label: "Access Key",   Type: ParamTypeString, Default: "rustfsadmin"},
        {Name: "secret_key",   Label: "Secret Key",   Type: ParamTypeSecret, Required: true, AutoGenerate: true},
        {Name: "api_port",     Label: "API 端口",     Type: ParamTypeInt, Default: "9000"},
        {Name: "console_port", Label: "控制台端口",   Type: ParamTypeInt, Default: "9001"},
        {Name: "bucket",       Label: "自动登记的桶名", Type: ParamTypeString, Default: "pipewright"},
    },
    Builtin: true,
},
```

`bucket` 参数不进 compose,仅供自动登记读取(`Render` 会把所有生效参数放进 `rendered.Used`,含未引用项——已确认)。同步更新 `appstore.go` 头注释里的内置应用清单。

## 二、后端:对象存储支持 rustfs 类型

- **`internal/objectstore/objectstore.go`**:加 `TypeRustFS = "rustfs"` 常量 + `ValidType` 分支;更新包注释("本期仅阿里云 OSS" → 双类型)。
- **`internal/objectstore/s3.go`(新文件)**:基于 `github.com/minio/minio-go/v7`(go.mod 新增依赖,RustFS 是 S3 API/SigV4,阿里云 OSS SDK 不适用)实现 `ObjectClient`:
  - `newRustFSClient(endpoint, ak, sk, bucket)`:`minio.New` + `credentials.NewStaticV4`,`Secure: false`,`BucketLookup: minio.BucketLookupPath`(自建存储按 IP/主机名访问必须 path-style)。
  - `VerifyBucket` = `BucketExists`;`PutObject` = `cli.PutObject(..., -1, ...)`(流式未知长度)。
  - 额外实现 `EnsureBucket(ctx)`(`BucketExists` → `MakeBucket`),经可选能力接口 `bucketEnsurer` 暴露,阿里云实现不动。
- **`internal/objectstore/oss.go`**:`newObjectClient` 加 `TypeRustFS` 分支;`describeClientError` 加 `minio.ErrorResponse`(Code/Message)人话分支。
- **`internal/httpapi/objectstore.go:62`**:错误文案改为"支持 aliyun_oss / rustfs"。
- 已知取舍:Endpoint 沿用现有归一化约定(去 scheme,HTTP),自建场景即 `host:port`,本期不支持 https 标记。
- 顺带更新 `internal/ai/catalog.go` 中 oss_upload 节点"上传到已配置的阿里云 OSS"的措辞为通用说法。

## 三、后端:部署联动自动登记

**`internal/objectstore/service.go`** — Service 接口新增 `Register`(服务层已持有 vault,密文密封收在领域层;httpapi 测试只需 fake 整个 Service,可测性最好):

```go
type RegisterInput struct { Type, Name, Endpoint, Bucket, AccessKeyID, Secret string }
type RegisterResult struct { Provider *Provider; Updated bool; Note string }
Register(ctx, in) (*RegisterResult, error)
```

逻辑:校验入参 → 尽力而为建桶(`dialFactory` 拿客户端,实现 `bucketEnsurer` 才做;带短重试应对容器刚起,包内可覆盖的重试间隔变量便于测试;失败记入 `Note` 不阻断)→ 按 `type+endpoint` 匹配现有配置:**命中则先 `vault.Update` 轮换密钥再 `Update` 字段**(重新部署会重新生成 Secret,旧凭据必须跟着换),**未命中则 `vault.Create` → 复用 `svc.Create` → 失败回滚 `vault.Delete`**。

**`internal/httpapi/apps.go`** — `makeDeployAppHandler` 增加 `objStores objectstore.Service` 参数:
- 捕获 `servers.Get` 返回的 `srv`(需要 `Host`/`Name`,现为 `_` 丢弃)。
- 部署成功且 `tpl.Name == "rustfs"` 且服务非 nil 时:从 `rendered.Used` 取 `access_key`/`secret_key`/`api_port`/`bucket`,endpoint 用 `net.JoinHostPort(srv.Host, api_port)`,名 `RustFS · <服务器名>`,带独立超时(约 20s)调 `Register`。尽力而为:登记失败不影响部署结果,只写入响应备注。
- 响应结构新增 `objectStoreRegistered bool` + `objectStoreNote string`(omitempty);新审计 action `objectstore.register`,detail 含 type/name/endpoint/bucket/来源,绝无密钥。
- **`internal/httpapi/router.go:892`**:传 `o.objectStores`(已在 options 里,nil 则跳过登记)。

## 四、前端

- **`web/src/api/objectStores.ts`**:`ObjectStoreType` 联合加 `'rustfs'`(TS 会强制补齐 label)。
- **`web/src/views/settings/SettingsObjectStorage.vue`**:`STORE_TYPES` 加 `'rustfs'`、`typeLabels` 加项;Endpoint 占位符/提示改按 `form.type` 计算切换(rustfs 显示 `192.168.1.10:9000` 风格);更新文件头注释。
- **`web/src/api/apps.ts`**:`AppDeployResult` 加 `objectStoreRegistered?` / `objectStoreNote?`。
- **`web/src/views/AppStore.vue`**:部署结果面板在成功文案下显示登记结果行(已登记 → 成功样式 + 备注;登记失败 → 警告样式显示备注,部署本身仍成功)。
- **i18n(8 个语言目录)**:`settingsObjectStorage.ts` 加 `typeRustfs`、`endpointPlaceholderRustfs`、`endpointHintRustfs`,并把 `desc`/`emptyHint`/`addSub` 里"仅阿里云 OSS"措辞改为双类型;`appStore.ts` 加 `deploy.autoRegistered` 等登记提示键。

## 五、测试与验证

- `internal/objectstore/objectstore_test.go`:`TestValidType` 加 rustfs 用例。
- `internal/objectstore/service_test.go`:Register 的创建/轮换/建桶失败仍登记/vault 未配置等用例(fake dialFactory 注入 bucketEnsurer 假客户端;重试间隔变量置零)。
- `internal/httpapi/apps_register_test.go`(新):fake appstore/target/objectstore(接口嵌入法)验证——rustfs 部署成功触发 Register 且入参正确(endpoint=host:port、bucket 取参数);非 rustfs 模板不触发;登记失败部署仍 ok;部署失败不登记。
- 全量:`go build ./... && go test ./...`;前端 `cd web && npx vue-tsc --noEmit && npm run build`(具体以 Makefile 为准)。
- 依赖:`go get github.com/minio/minio-go/v7`(仓库已配 GOPROXY,应无碍)。

默认值说明:自动登记的桶名默认 `pipewright`(部署表单里可改);重复部署同一服务器的 RustFS 会更新既有配置的凭据而不是产生重复条目。