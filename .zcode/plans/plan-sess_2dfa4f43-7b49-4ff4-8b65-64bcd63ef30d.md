# 支持为 sudo 选择密码凭据

## 目标与策略

当前平台 HTTPS 在非 root 且无免密 sudo 时直接报 `ErrNoPrivilege`。改为:利用现有 vault 凭据体系新增 `sudo_password` 凭据类型,在**服务器表单**中可选绑定;执行提权命令时优先级为 **root → 免密 sudo(`sudo -n`)→ sudo 密码(`sudo -S -p ''`,密码经 SSH stdin 喂入)**。密码绝不进命令行 argv(ps 可见)、绝不进日志/错误体、绝不挂起等 TTY 输入——延续原设计纪律,只放宽"仅免密"这一条。

选择"服务器级"而非"HTTPS 设置级":sudo 密码语义上属于服务器登录用户,且与现有 `servers.credential_id` 引用模式一致,未来其他提权场景可复用。

## 后端改动

### 1. vault:`internal/vault/vault.go`
- 类型枚举新增 `TypeSudoPassword = "sudo_password"`(掩码走 default 分支全打点,无需改 masker);`validateType` switch 加一项。
- `Delete()` 的占用检查(vault.go:361 附近)追加 `SELECT COUNT(*) FROM servers WHERE sudo_credential_id = ?` → `ErrCredentialInUse`(SQLite 无法 ALTER 加外键,用显式查询防悬挂引用)。

### 2. 迁移:`0058_servers_sudo_credential.sql`(sqlite + mysql 各一份,照 0052 风格)
- `ALTER TABLE servers ADD COLUMN sudo_credential_id TEXT NOT NULL DEFAULT ''`(mysql 用 VARCHAR(64))。

### 3. target 层:`internal/target/target.go` 及实现
- `Server`/`CreateInput`/`UpdateInput` 增加 `SudoCredentialID`(Update 用 `*string`);`SudoCredentialName` 只读展示列(List/Get 第二个 LEFT JOIN credentials);`Create`/`Update`/`scanServer` SQL 同步。
- 校验:非空时经 `vault.Exists` 检查存在性 → 不存在映射 `ErrCredentialNotFound`。不强制类型(前端过滤 `sudo_password`;选错类型时 sudo 探测会以明确错误失败)。
- `Service` 接口新增方法(追加式扩展,同 ExecStream/Upload 先例;包注释"冻结契约"补一句):
  `ExecWithStdin(ctx, serverID, cmd []string, stdin io.Reader) (*ExecResult, error)` — 实现照 `service.Exec`(target.go:427-473)复制,改调 `s.dialer.RunWithStdin`(ssh.go:96 已实现并测试过)。

### 4. platformhttps:`internal/platformhttps/`
- `New(db, tg, certs, defaultPort)` 增参 `sudoSrc SudoSource`,窄接口 `type SudoSource interface { Get(id string) (string, error) }`(vault 已满足;nil = 密码路径禁用)。main.go:461 传入 `credVault`。
- `privilegePrefix` 重构为 `buildPrivilege(ctx, tg, sudoPwd) (privilege, error)`,返回:
  ```go
  type privilege struct {
      prefix  []string // nil=root;["sudo","-n"];["sudo","-S","-p",""]
      sudoPwd string   // 密码模式:每条命令新造 strings.NewReader(pwd+"\n") 经 stdin 喂
  }
  ```
  探测顺序:`id -u`==0 → 免密;`sudo -n true` → 免密;sudoPwd 非空 → `sudo -S -p '' true` 探测(错密码 → sudo 读一行后 EOF 重试失败退出非 0),成功进密码模式,失败返回新错误 `ErrSudoPassword`;都没有 → `ErrNoPrivilege`(错误文案更新为:用 root / 配免密 sudo / 在服务器设置选择 sudo 密码凭据)。
- `execOK` 旁新增 `execPriv`:无密码走 `tg.Exec`,密码模式走 `tg.ExecWithStdin`。`applyPlatformConf`/`removePlatformConf`/`rollbackConf`/`cleanupTmp` 的 `prefix []string` 参数全部换成 `p privilege`,改用 `execPriv`。
- `Apply`/`Disable`/`Detect`:`tg.Get(serverID)` 取 `SudoCredentialID` → 非空时 `sudoSrc.Get` 取密码(取出错:ErrVaultUnconfigured 原样透传,其余包成 ErrNoPrivilege 人话)。
- `NginxDetect` 增加 `SudoPwdConfigured`/`SudoPwdOk`;`detectNginx` 探测链补密码探测。
- 包注释(platformhttps.go:14-15)"绝不交互输密"改写为:免密优先;可选 sudo 密码凭据经 `sudo -S` stdin 非交互喂入。
- 已知特性(接受):每条 SSH exec 是独立会话,sudo 时间戳缓存不复用,一次 Apply 约 8-10 条命令各喂一次密码。

### 5. httpapi:`internal/httpapi/servers.go` + `platformhttps.go`
- servers:DTO/创建/更新请求体加 `sudoCredentialId`(响应另加 `sudoCredentialName`),照 credentialId 现有处理(含 ErrCredentialNotFound 映射)。
- platformhttps:detect DTO 加 `sudoPwdConfigured`/`sudoPwdOk`;`writePlatformHTTPSError` 更新 `no_privilege` 文案,新增 `ErrSudoPassword` → 400 `sudo_password_failed`("sudo 密码验证失败:请检查服务器设置中所选 sudo 密码凭据")。

### 6. Go 测试
- `internal/platformhttps/platformhttps_test.go`:`fakeTarget` 补 `ExecWithStdin`(捕获 stdin);新增用例:root / 免密 / 密码正确 / 密码错误 / 未配置 五分支;密码模式下 apply 全链路断言命令带 `sudo -S -p ''` 且每条命令都有新 stdin。
- 其他实现 target.Service 的测试桩补 3 行 stub:`internal/httpapi/server_ops_e2e_test.go`、`servicereg_test.go`、`internal/build/remote_test.go`。
- vault:validateType 接受新类型;Delete 占用检查覆盖 sudo_credential_id。
- target:ExecWithStdin 透传(capturingDialer 已实现 RunWithStdin);服务器 CRUD 回传 sudo_credential_id。
- httpapi:servers DTO 往返、detect DTO 新字段。

## 前端改动

- `web/src/api/credentials.ts`:`CredentialType` 加 `'sudo_password'`。
- `web/src/api/servers.ts`:`Server`/创建/更新入参加 `sudoCredentialId`/`sudoCredentialName`。
- `web/src/api/platformHttps.ts`:`PlatformHttpsDetect` 加 `sudoPwdConfigured`/`sudoPwdOk`。
- `web/src/views/settings/SettingsVault.vue`:新建凭据类型下拉加 sudo_password 选项;`settingsVault.ts` i18n ×8 语言(zh-CN/en/de/es/fr/ja/ko/zh-TW)加类型名/说明/placeholder。
- `web/src/views/settings/SettingsServers.vue`:SSH 凭据下方加可选「sudo 提权凭据」原生 select(照 495-500 行现有凭据 select 模式,过滤 `sudo_password`,默认"不使用"+ hint);`settingsServers.ts` i18n ×8。
- `web/src/views/settings/SettingsHttps.vue` 提权徽章链(381-383 行)扩展:`isRoot` → root;`sudoOk` → 免密 sudo;`sudoPwdOk` → 新键"sudo 密码可用";`sudoPwdConfigured && !sudoPwdOk` → 红色"sudo 密码验证失败";否则"无提权权限";`platformHttps.ts` i18n ×8 加 `detectSudoPwd`/`detectSudoPwdFailed`。
- README.md / README.zh-CN.md 平台 HTTPS 段落:"支持 root / 免密 sudo" 补 "sudo 密码凭据"。

## 验证

1. `go build ./... && go test ./internal/...`
2. `cd web && npm run build`(类型检查随构建)
3. 手动路径自检:创建 sudo_password 凭据 → 服务器绑定 → HTTPS 页探测徽章显示"sudo 密码可用" → 应用成功;配错密码时探测/应用报 `sudo_password_failed`。

实施顺序:vault → 迁移 → target → platformhttps → httpapi → 前端 → 文档。