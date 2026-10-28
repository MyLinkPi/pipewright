#!/usr/bin/env bash
# client-e2e.sh — 在 Linux 上启动真实 pwclient 界面(临时数据目录,不触网),再跑 Playwright。
# 用法:make client-e2e(会先构建前端与 Linux 二进制);也可手动:
#   go build -o pwclient ./cmd/pwclient && bash scripts/client-e2e.sh
set -euo pipefail
cd "$(dirname "$0")/.."

bin=${PW_CLIENT_BIN:-./pwclient}
if [ ! -x "$bin" ]; then
  echo "缺少可执行文件 $bin(先 go build -o pwclient ./cmd/pwclient)" >&2
  exit 1
fi

tmp=$(mktemp -d)
# 不固定端口(绑 127.0.0.1:0 随机端口):CI 并行/同机残留进程时固定端口会 bind 失败;
# 真实端口由 ui.json 回填。
PIPEWRIGHT_CLIENT_DATA="$tmp" "$bin" ui --no-open >"$tmp/ui.log" 2>&1 &
pid=$!
cleanup() {
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  rm -rf "$tmp"
}
trap cleanup EXIT

for _ in $(seq 1 50); do
  [ -f "$tmp/ui.json" ] && break
  sleep 0.2
done

if [ ! -f "$tmp/ui.json" ]; then
  echo "界面未启动(缺少 ui.json):" >&2
  cat "$tmp/ui.log" >&2
  exit 1
fi

# 从 ui.json 解析端口与界面令牌(与 uiserver.URL 一致:令牌在 query,读后立即清理)。
url=$(node -e 'const fs=require("fs");const j=JSON.parse(fs.readFileSync(process.argv[1],"utf8"));process.stdout.write(`http://127.0.0.1:${j.port}/?t=${j.token}`)' "$tmp/ui.json")
if [ -z "$url" ]; then
  echo "无法解析界面地址:" >&2
  cat "$tmp/ui.json" >&2
  exit 1
fi

# 先验 HTTP 可达与前端已 embed:缺产物时在此快速失败并给出可定位的报错,
# 而不是把问题推迟到 Playwright 阶段。
base=${url%%\?*}
token=$(node -e 'process.stdout.write(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")).token)' "$tmp/ui.json")
curl -fsS "$base/" >/dev/null || {
  echo "界面首页不可达(前端产物未 embed?):$base/" >&2
  cat "$tmp/ui.log" >&2
  exit 1
}
curl -fsS -H "X-PW-Token: $token" "$base/api/status" >/dev/null || {
  echo "界面 API 不可达:$base/api/status" >&2
  cat "$tmp/ui.log" >&2
  exit 1
}

(
  cd webclient
  PW_UI_URL="$url" npx playwright test
)
