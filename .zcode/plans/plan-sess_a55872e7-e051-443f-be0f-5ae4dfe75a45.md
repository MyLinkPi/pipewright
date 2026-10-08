# 内置镜像仓库支持 TLS(挂载 certmgmt 证书,去 insecure-registries)

## 背景与目标
内置 registry 栈(registry:2 双服务,:5000 制品 / :5001 缓存)目前是明文 HTTP,靠各机 daemon.json 的 `insecure-registries` 工作。目标:支持在设置页选择「证书管理」(certmgmt)里的证书,部署时挂载进两个容器以 HTTPS 服务;daemon.json 随之改为 https mirror 并**去掉 insecure-registries**。客户端信任按**公共可信 CA** 最小实现(不做 certs.d CA 下发)。

## 设计决策
- 配置模型:单字段 `TLSCertID`(空 = 明文 HTTP 现状;非空 = 整栈双服务 TLS,一张证书覆盖同域名两个端口)。仿 platformhttps 模式按证书 ID 软引用 certmgmt,PEM 仅进程内解密、绝不走路径/HTTP。
- 域名校验:Save 时经注入的 `CertSource` 校验证书存在 + 域名覆盖 ExternalAddr(精确或通配符),失败 → 新错误 `ErrInvalidTLSCert`(httpapi 映射 422 `invalid_registry_tls`)。
- 部署语义不变:Save 只写库;点「部署」→ renderCompose 变化 → `compose up -d` 漂移重建容器。证书续期联动:Hub 实现 `UsesCert`/`RedeployCert`(= 重新 DeployStack,重新落盘新 PEM)。
- 平滑升级:旧 daemon.json 的 insecure 条目下 docker 本就先试 HTTPS,先部署栈、后重推 daemon.json 的窗口期不炸;清空证书选择+重新部署即回退明文。

## 改动清单

**1. DB 迁移** — `internal/store/migrations/{sqlite,mysql}/0066_registry_hub_tls.sql`(仿 0062):`registry_hub_config` 加列 `tls_cert_id TEXT NOT NULL DEFAULT ''`。

**2. 领域层 registryhub**
- `config.go`:Config/SaveInput 加 `TLSCertID`;NormalizeConfig 校验(trim、长度≤128、非空白);`configService` 注入 `CertSource`(registryhub 本包自定义接口 `GetCert`/`OpenCertPEM`,防包环),Save 校验证书存在+域名覆盖(通配符匹配 helper);新增 `ErrInvalidTLSCert`;Get/Save SQL 带新列。
- `stack.go`:
  - `renderCompose`(118-146):TLS on 时双服务各加卷 `- "<certsDir>:/certs:ro"`(certsDir=`<BaseDir>/certs`)与 env `REGISTRY_HTTP_TLS_CERTIFICATE: /certs/fullchain.pem`、`REGISTRY_HTTP_TLS_KEY: /certs/privkey.pem`;**保持 `- "<dir>:/var/lib/registry"` 行形态**(parseComposeDataDirs 依赖)。
  - `DeployStack`(153-200):写 compose 前落盘 PEM(fullchain 0644 / privkey 0600);证书获取失败诚实报错;TLS off 时 best-effort 清 certs 目录(私钥不留盘)。
  - `pingV2`(296-312)与 `retention.go:23-28` registryClient:TLS on 时走 `https://127.0.0.1:<port>` + InsecureSkipVerify 本机探活/清理客户端。
- `daemon.go`:`mirrorURL`(30)TLS on → `https://` 前缀;`daemonJSONContent`(34-44)TLS on → 只写 `registry-mirrors`,不再写 `insecure-registries`。
- `hub.go`:Options 加 `Certs CertSource`;Hub 加 `UsesCert`/`RedeployCert`。

**3. httpapi** — `internal/httpapi/registry_hub.go`:DTO + PUT 请求体加 `TLSCertID`;`writeRegistryHubError` 加 `ErrInvalidTLSCert → 422 invalid_registry_tls`;`registry_hub_test.go` 补用例。

**4. 装配 main.go** — `registryCertSource` 适配器(registryhub.CertSource ← certmgmt.Service,仿 platformCertSource main.go:998-1016);`registryHubAdapter`(UsesCert/RedeployCert)与 platformHTTPSAdapter 组合后注入 certmgmt(具体组合方式按 certmgmt.PlatformHTTPS 装配点实现时确认)。

**5. 前端 web/**
- `web/src/api/registryHub.ts`:两个 interface 加 `tlsCertId: string`。
- `web/src/views/settings/SettingsRegistry.vue`:reg-grid 加「TLS 证书」下拉(复用证书列表 API,首项「不启用(明文 HTTP)」);提示:保存后需重新「部署」并重新「下发 daemon.json」(mirror 变 https、去 insecure-registries),证书须公共可信;daemon 下发确认弹窗文案同步 https mirror。
- i18n:`web/src/i18n/locales/<8 语言>/settingsRegistry.ts` 各加新 key(zh-CN 为源,keyParity 测试约束)。

**6. 文档** — README.zh-CN.md / README.md:内置镜像仓库章节补 TLS 说明。

## 测试
- registryhub 单测:Save 三态(证书不存在/域名不覆盖/通过,fake CertSource);renderCompose 两态(断言卷行形态、env);DeployStack PEM 落盘权限与 TLS off 清目录;daemonJSONContent/mirrorURL 两态;pingV2/registryClient https;UsesCert。
- httpapi:PUT 带 tlsCertId、422 错误映射。
- 前端:keyParity 自动覆盖 8 语言。

## 明确不做(范围外)
自签/内部 CA 的 certs.d 下发;nerdctl/podman(containerd)证书目录;构建日志/AI 诊断的该错误文案增强;构建/部署侧 docker CLI 链路改动(push/pull 走 daemon 信任,证书可信即零改动)。