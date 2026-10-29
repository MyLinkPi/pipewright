SHELL := /bin/bash
BIN := pipewright
CLIENT_BIN := pwclient.exe
CLIENT_WEB_DIR := webclient
UISERVER_DIST := internal/pwclient/uiserver/dist
INSTALLER_OUT := dist-installer

# 模块代理:默认走国内镜像(proxy.golang.org 在境内常超时),与 Dockerfile 保持一致。
# 用 ?= 保证 CI / 已有 GOPROXY 环境可覆盖;export 后传入 go 子进程。
# GOSUMDB 必须保留:goproxy.cn 会代理 sumdb 校验(/sumdb/ 端点),设 off 会在
# 自动下载工具链(golang.org/toolchain)时被拒——工具链强制要求校验,不允许 GOSUMDB=off。
GOPROXY ?= https://goproxy.cn,direct
GOSUMDB ?= sum.golang.org
export GOPROXY GOSUMDB
# 显式包范围:根 + cmd + internal。不用 ./... 以免扫进 web/node_modules 里的 Go 包。
GO_PKGS := . ./cmd/... ./internal/...
GO_FMT_DIRS := cmd internal embed.go

# 版本元数据:tag 优先(git describe),源码态回退 dev。发版由 .goreleaser.yaml 注入同名变量。
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VPKG    := github.com/huangchengsir/pipewright/internal/version
# pwclient 是 GUI 程序:windowsgui 子系统免控制台窗口(文本子命令经 AttachConsole 接回终端)。
LDFLAGS := -s -w -X $(VPKG).Version=$(VERSION) -X $(VPKG).Commit=$(COMMIT) -X $(VPKG).Date=$(DATE)
CLIENT_LDFLAGS := $(LDFLAGS) -H=windowsgui

.PHONY: all build build-client build-client-all client-web client-web-stub client-e2e client-syso build-installer embed-frontend dist-stub go-build test vet fmt fmt-check mem-check dev run version clean

all: build

## embed-frontend: 构建前端静态资源到 web/dist(供 go:embed)
embed-frontend:
	cd web && npm ci && npm run build

## dist-stub: 保证 web/dist 有最小占位 index.html(go:embed 的编译目标;dist 已全部
## 不入库,纯后端流程 fresh clone 后跑 make test/vet/dev/run 前由本目标兜底创建)。
dist-stub:
	@[ -f web/dist/index.html ] || { mkdir -p web/dist; echo '<!doctype html><div id="app"></div>' > web/dist/index.html; }

go-build: dist-stub
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/pipewright

## client-web: 构建客户端前端(webclient/ → internal/pwclient/uiserver/dist,供 go:embed)
client-web:
	cd $(CLIENT_WEB_DIR) && npm ci && npm run build

## client-web-stub: 保证 uiserver/dist 有最小占位 index.html(纯后端流程 test/vet 编译 embed 用)
client-web-stub:
	@[ -f $(UISERVER_DIST)/index.html ] || { mkdir -p $(UISERVER_DIST); printf '<!doctype html><meta charset="utf-8"><div id="app"></div>\n' > $(UISERVER_DIST)/index.html; }

## build-client: 构建 Win11 集群客户端(前端嵌入 + 托盘/WebView2 + Windows 服务,单二进制)
build-client: client-web
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(CLIENT_LDFLAGS)" -o $(CLIENT_BIN) ./cmd/pwclient
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -ldflags "$(CLIENT_LDFLAGS)" -o pwclient-arm64.exe ./cmd/pwclient

## build-client-all: 三平台客户端产物(Windows 完整形态;Linux/macOS 前台 + 浏览器 UI)
build-client-all: client-web
	mkdir -p dist-client
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(CLIENT_LDFLAGS)" -o dist-client/pwclient_windows_amd64.exe ./cmd/pwclient
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -ldflags "$(CLIENT_LDFLAGS)" -o dist-client/pwclient_windows_arm64.exe ./cmd/pwclient
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist-client/pwclient_linux_amd64 ./cmd/pwclient
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist-client/pwclient_linux_arm64 ./cmd/pwclient
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist-client/pwclient_darwin_amd64 ./cmd/pwclient
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist-client/pwclient_darwin_arm64 ./cmd/pwclient

## client-e2e: Linux 起真实客户端(临时数据目录)→ Playwright 跑界面回归
## 需要先装浏览器:cd webclient && npx playwright install chromium
client-e2e: client-web
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o pwclient ./cmd/pwclient
	bash scripts/client-e2e.sh

## client-syso: 由 cmd/pwclient/winres 重新生成 Windows 资源(manifest/版本信息;.syso 已入库,
## 仅 winres.json 变更后需要重跑)
client-syso:
	go run github.com/tc-hib/go-winres@latest make --in cmd/pwclient/winres/winres.json --out cmd/pwclient/rsrc --arch amd64,arm64

## build-installer: 本地打 Windows 安装包(NSIS)。前置:本机装 makensis
## (Debian/Ubuntu: sudo apt-get install -y nsis)。取 build-client 产物 + third_party/easytier
## 随包二进制组装暂存目录,amd64/arm64 各出一个安装器到 $(INSTALLER_OUT)/。
## 发版正式路径是 release.yml 的 windows-installer 作业;本目标仅供本地验证产物。
build-installer: build-client
	bash scripts/fetch-easytier.sh
	@for arch in amd64 arm64; do \
	  set -e; \
	  stage="$(INSTALLER_OUT)/stage-$${arch}"; rm -rf "$$stage"; mkdir -p "$$stage/bin" "$$stage/licenses"; \
	  if [ "$${arch}" = amd64 ]; then cp $(CLIENT_BIN) "$$stage/pwclient.exe"; else cp pwclient-arm64.exe "$$stage/pwclient.exe"; fi; \
	  cp third_party/easytier/windows-$${arch}/bin/. "$$stage/bin/"; \
	  cp -r third_party/easytier/licenses/. "$$stage/licenses/"; \
	  cp LICENSE "$$stage/"; \
	  bash scripts/build-installer.sh --src "$$stage" --version "$(VERSION)" --arch "$${arch}" --out "$(INSTALLER_OUT)"; \
	done

## build: 前端构建 → go:embed → 静态单二进制(双运行模式之原生形态)
build: embed-frontend go-build

test: dist-stub client-web-stub
	go test $(GO_PKGS)

vet: dist-stub client-web-stub
	go vet $(GO_PKGS)

## fmt: 用内置 gofmt 格式化(无需配置文件)
fmt:
	gofmt -w $(GO_FMT_DIRS)

## fmt-check: CI 用;有未格式化文件则失败
fmt-check:
	@out="$$(gofmt -l $(GO_FMT_DIRS))"; if [ -n "$$out" ]; then echo "需要 gofmt 的文件:"; echo "$$out"; exit 1; fi

## mem-check: 断言平台常驻内存 ≤100MB(NFR-4)
mem-check:
	bash scripts/mem-check.sh

## dev: 本地开发(两个终端:Go API + Vite 热更代理)
dev: dist-stub
	@echo "终端1: go run ./cmd/pipewright"
	@echo "终端2: cd web && npm run dev   # 代理 /api /healthz 到 :8080"

## version: 打印将注入二进制的版本元数据(调试发版用)
version:
	@echo "VERSION=$(VERSION)"; echo "COMMIT=$(COMMIT)"; echo "DATE=$(DATE)"

run: go-build
	./$(BIN)

clean:
	rm -f $(BIN) $(CLIENT_BIN) pwclient-arm64.exe pwclient *.db
	rm -rf web/dist web/node_modules $(CLIENT_WEB_DIR)/node_modules $(UISERVER_DIST) dist-client $(INSTALLER_OUT)
