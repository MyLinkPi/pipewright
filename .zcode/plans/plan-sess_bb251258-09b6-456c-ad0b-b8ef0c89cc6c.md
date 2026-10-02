## 目标

将项目中繁体中文的 locale 代码从 `zh-TW` 全量改为 `zh-HK`,标签保持「繁體中文」不变,繁体译文用词同步从台湾用语调整为香港用语。

## 用词调整对照表(已逐词核查上下文,其余词条不动)

| 台湾用语 | 港式用语 | 前端 | 后端 |
|---|---|---|---|
| 建置 | 構建 | 83 处 | 5 处 |
| 網路 | 網絡 | 37 处 | 1 处 |
| 使用者 | 用戶 | 24 处 | 2 处 |
| 網域(子網域/根網域) | 域名(子域名/根域名) | 27 处 | 0 |
| 智慧(智慧群助手为钉钉产品名) | 智能 | 3 处 | 0 |

不改动:程式碼、原始碼、記憶體、帳號、佇列、套件、映像、憑證、伺服器、快取、預設、部署等(港式通用或与简体原文对应);「僅當機器人」两处为误报不碰;「行動按鈕」(来自简体「行动按钮」CTA)港式通用不碰。

## 一、后端 Go

1. `internal/i18n/i18n.go`:`Supported` 切片与 `Normalize()` 返回值 `"zh-TW"` → `"zh-HK"`。**保留** `hant|tw|hk|mo` 匹配逻辑 —— 这样 DB/配置里已存的旧值 `zh-TW` 会自动归一化到 `zh-HK`,通知语言等旧配置不失效。
2. `internal/i18n/i18n_test.go`:断言改为 `"zh-HK"→"zh-HK"`、`"zh-Hant"→"zh-HK"`,并新增 `"zh-TW"→"zh-HK"`(旧值兼容)与 `"zh-MO"→"zh-HK"` 用例。
3. 8 个 `messages_*.go`(part1–5、notify、prefix、terminal):全部 map key `"zh-TW":` → `"zh-HK":`(228 处,批量替换)。
4. `internal/notify/router.go`:3 处 `lang == "zh-TW"` → `"zh-HK"`。
5. 后端繁体用词按对照表调整(共 8 处)。

## 二、前端

1. **重命名**(git mv,内容随后改):`web/src/i18n/locales/zh-TW.ts` → `zh-HK.ts`;目录 `web/src/i18n/locales/zh-TW/`(42 个文件)→ `zh-HK/`。glob 按路径归语言,目录名即生效。
2. `web/src/i18n/index.ts`:
   - import 路径与变量名 `zhTW` → `zhHK`
   - `LocaleCode` 类型、`SUPPORTED_LOCALES`(label 保持「繁體中文」)、messages bag key 改 `'zh-HK'`
   - `matchLocale` 返回 `'zh-HK'`(正则 `hant|tw|hk|mo` 保留,浏览器 zh-TW 标签仍命中)
   - `detectLocale` 增加一行迁移:存储值 `'zh-TW'` 视作 `'zh-HK'`,老用户语言不丢
3. `web/src/composables/useNaiveLocale.ts`:map key 改 `'zh-HK'`;naive-ui 无 zhHK locale,**保留** `zhTW/dateZhTW` 导入对象并加注释说明。
4. 测试同步:`keyParity.test.ts`(LocaleCode 列表)、`messageCompile.test.ts`(import 路径 + LOCALES key)。
5. `zh-HK.ts` 头注释「繁體中文(臺灣用語)」→「繁體中文(香港用語)」,内部 const 名同步。
6. 前端 43 个繁体文件按对照表批量替换(174 处)。

## 三、文档

- `README.md:72`:语言列表 `zh-TW` → `zh-HK`。

## 四、验证

1. 后端:`go test ./internal/i18n/... ./internal/notify/...`
2. 前端:`cd web && npx vitest run src/i18n`(keyParity + messageCompile)+ 类型检查/构建
3. 全仓库残留检查:`grep -rE "zh[-_]TW"`(排除 web/dist 构建产物,dist 重新构建自动再生)
4. 不做 git commit,改动留在工作区供检查(工作区已有 3 个未提交的 httpapi 修改,不会触碰)。