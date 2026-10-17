## 应用市场新增 NFS 应用(nfs-server-alpine)

应用市场是纯数据驱动的:内置应用全部定义在 `internal/appstore/builtin.go` 的 `builtins` 切片中,启动时 `EnsureBuiltins()` 幂等 seed 到数据库,前端自动渲染卡片与部署表单。**只需改动这一个文件**,零前端/零 i18n 改动。

### 改动:`internal/appstore/builtin.go`

在 `builtins` 切片末尾(redpanda 之后)追加 NFS 模板:

```go
{
    Name:        "nfs",
    DisplayName: "NFS Server",
    Description: "NFSv4 文件共享服务(仅需 2049 端口);客户端 mount -t nfs4 <宿主IP>:/ 挂载;数据落具名卷 nfs-data;容器需特权模式,宿主机内核需支持 nfsd。",
    Icon:        "📁",
    // itsthenetwork/nfs-server-alpine 仅支持 NFSv4(免 rpcbind,只暴露 2049/TCP),
    // 容器内要写 /etc/exports 并挂载 nfsd 内核线程,必须 privileged;
    // PERMITTED 为客户端白名单(* 或 IP/CIDR,空格分隔),READ_ONLY 控制只读。
    ComposeYAML: `services:
  nfs:
    image: itsthenetwork/nfs-server-alpine:{{version}}
    restart: unless-stopped
    privileged: true
    environment:
      SHARED_DIRECTORY: /data/shared
      PERMITTED: "{{permitted}}"
      READ_ONLY: "{{read_only}}"
      SYNC: "true"
    ports:
      - "{{port}}:2049"
    volumes:
      - nfs-data:/data/shared
volumes:
  nfs-data:
`,
    Params: []ParamSpec{
        {Name: "version", Label: "镜像版本", Type: ParamTypeString, Default: "latest"},
        {Name: "permitted", Label: "允许的客户端(* 或 IP/CIDR,空格分隔)", Type: ParamTypeString, Default: "*"},
        {Name: "read_only", Label: "只读共享(true/false)", Type: ParamTypeString, Default: "false"},
        {Name: "port", Label: "宿主端口", Type: ParamTypeInt, Default: "2049"},
    },
    Builtin: true,
},
```

### 已确认的兼容点(无需额外改动)

- **参数校验**:`valueRe` 白名单允许 `*`、`/`、空格,`PERMITTED` 取值(如 `10.0.0.0/16`)可正常通过防注入校验。
- **资源限制注入**:`injectComposeLimits` 经泛型 `map[string]any` 只追加 `deploy.resources.limits`,compose 中的 `privileged: true` 会被完整保留。
- **前端**:部署表单按 params schema 动态渲染;端口参数名沿用 `port`,已在 `AppStore.vue` 的 `portParamPriority` 首位,网关注册端口预填正常。
- **存量环境升级**:`EnsureBuiltins` 对同名内置行做差异刷新,老库重启后自动出现新模板。
- **宿主地址注入**:`hostParamNames` 只含 `advertise_host`/`ssh_domain`,NFS 无需接入。

### 验证

运行 `go test ./internal/appstore/... ./internal/httpapi/...`(测试对内置应用数用 `len(builtins)` 动态断言,新增条目不破坏现有测试)。