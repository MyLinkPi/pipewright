# 让 GitHub 免费打包 + install.sh 一键下载安装

## 背景结论
- GitHub Actions 发版流水线已存在（推 `v*` tag → goreleaser 出 6 平台二进制 + checksums + Release + ghcr 镜像），公开仓库免费。但 `.goreleaser.yaml` 硬编码上游 `huangchengsir/pipewright`，tag 推到自己 fork 会因 GITHUB_TOKEN 无权限发版失败 → 改为**仓库自适应**。
- Web 端「二进制部署」的一键升级（下载 → sha256 校验 → 原子替换 → 自动重启）已存在，**前后端零改动**。缺的是让部署进入二进制模式的入口：install.sh 联网下载安装。
- install.sh 下载安装时必须**跳过/清理源码部署标记**（`.pipewright-source` + env 文件 `PIPEWRIGHT_SOURCE_DIR`），否则仍被识别为源码部署、Web 升级又走 `make build`（小内存机器死路）。

## 改动清单

### 1. `.goreleaser.yaml` — fork 自适应
- 删除 `release.github.owner/name` 硬编码（tag 推到哪个仓库，Release 就发到哪个仓库，由 git remote 推断）；draft/prerelease/name_template 保留。
- `dockers`/`docker_manifests` 的镜像名与 `source` label 改模板：`ghcr.io/{{ .Env.IMAGE_NAMESPACE }}/pipewright`、`https://github.com/{{ .Env.RELEASE_REPO }}`（env 由 workflow 注入）。
- `ldflags` 增加 `-X .../internal/version.DefaultRepo={{ .Env.RELEASE_REPO }}`：二进制自报发布渠道，Web 检查更新默认跟随构建仓库。
- release footer 增加一行式安装命令（`raw.githubusercontent.com/<repo>/<tag>/install.sh | sh`）。

### 2. `.github/workflows/release.yml`
- goreleaser 步骤前加一步导出 `IMAGE_NAMESPACE=${GITHUB_REPOSITORY_OWNER}`、`RELEASE_REPO=${GITHUB_REPOSITORY}` 到 `GITHUB_ENV`。其余不动。

### 3. `internal/version/source.go` — 渠道可注入
- `defaultRepo` const 改为 `DefaultRepo` var（初值不变），`releaseRepo()` 优先级 env > DefaultRepo > 硬编码兜底（防空串）。

### 4. `install.sh` — 下载模式（核心新增，约 +90 行）
- 新环境变量：`PIPEWRIGHT_VERSION`（钉版本，缺省 latest）、`PIPEWRIGHT_RELEASE_REPO`（缺省 `huangchengsir/pipewright`，与后端语义一致）、`PIPEWRIGHT_RELEASE_MIRROR`（GitHub 路径兼容镜像）、`PIPEWRIGHT_NO_DOWNLOAD=1`（恢复纯本地行为）。
- 触发：本地 `BIN_PATH` 不存在且未禁用下载 → 自动进入下载模式（`curl | sh` 一键安装成立）；存在 make build 产物时行为完全不变。
- 下载流程：`uname` 探测 os/arch（仅 linux/darwin × amd64/arm64）→ 经 `/releases/latest` 302 重定向解析最新 tag（与 Go 端同法，免 API 限流）→ 下载 `pipewright_<v>_<os>_<arch>.tar.gz` + `checksums.txt` 到 mktemp（trap 清理）→ sha256 校验（sha256sum/shasum 兼容）→ 解包得二进制作为 BIN_PATH，接续既有安装/升级/服务流程。
- 部署形态修正：下载模式下不写 `.pipewright-source` 标记；升级时清理旧标记与 env 文件中残留的 `PIPEWRIGHT_SOURCE_DIR` 行（源码→二进制部署转换，Web 升级界面随之变为一键下载）；并把实际下载用的 `PIPEWRIGHT_RELEASE_REPO` 幂等写入 env 文件，保证 Web 自升级与安装同渠道。
- 更新脚本头注释（"不联网下载二进制"的旧描述）。

### 5. `README.md` / `README.zh-CN.md`
- 安装章节新增「一行命令安装（推荐）」：`curl -fsSL .../install.sh | sh`（含 `SETUP_SERVICE=1` 变体、fork 用 `PIPEWRIGHT_RELEASE_REPO` 指向自己仓库的示例、Windows 手动下载 zip 说明）。
- 升级说明补充：二进制部署 = 设置页一键更新；源码部署重装为二进制后自动切换。

## 验证
- `sh -n install.sh` 语法检查；`go build ./...` + `go test ./internal/version/... ./internal/httpapi/...`；有 goreleaser 则跑 `goreleaser check`。
- 端到端（需你操作）：合并后推 `v*` tag 到 origin（MyLinkPi）→ 确认 Release 资产与 ghcr 镜像落到自己账号 → jump2 上 `curl -fsSL .../install.sh | sh` 或 scp install.sh 执行 → Web 设置页应显示二进制模式一键升级。

## 范围外
- Web 前后端代码零改动；不加源码模式在线切换入口（按选择，重装即切换）。
- 不代推 tag / 不动上游仓库。