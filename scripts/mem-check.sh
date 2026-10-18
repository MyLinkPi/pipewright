#!/usr/bin/env bash
# 体积门(上游 NFR-4 / SM-C1 演进):构建静态二进制,空载运行,断言常驻内存 RSS ≤ 150MB。
# 超标则以非零退出,使 CI 失败。
#
# 预算演进:上游初始设 100MB(2026-05 项目仅触发/运行竖切时)。本 fork 功能持续叠加
# (PVE/Ceph/dbadmin/redisadmin/MFA/opschat 等)后空载 RSS 约 103MB,其中 ~60MB 为
# modernc/sqlite 纯 Go 引擎的固有基线,无单一回归点可修;2026-10 经维护者决策放宽至
# 150MB(约 45% 余量),保持对真回归(如 RSS 翻倍)的拦截能力。
set -euo pipefail

LIMIT_KB=153600 # 150 MB
PORT="${MEMCHECK_PORT:-18099}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

BIN="$(mktemp -t pipewright.XXXXXX)"
DB="$(mktemp -u -t dtmem.XXXXXX).db" # -u: 只取名不创建,确保实际 DB 路径与名字一致(不泄漏临时文件)
PID=""
cleanup() {
  [ -n "$PID" ] && kill "$PID" 2>/dev/null || true
  rm -f "$BIN" "$DB" "$DB"-wal "$DB"-shm
}
trap cleanup EXIT

echo "==> building static binary"
CGO_ENABLED=0 go build -o "$BIN" ./cmd/pipewright

echo "==> starting on :$PORT"
PIPEWRIGHT_ADDR=":$PORT" PIPEWRIGHT_DB="$DB" "$BIN" >/dev/null 2>&1 &
PID=$!

healthy=0
for _ in $(seq 1 40); do
  if curl -fsS -o /dev/null "http://localhost:$PORT/healthz" 2>/dev/null; then
    healthy=1
    break
  fi
  sleep 0.25
done
if [ "$healthy" -ne 1 ]; then
  echo "FAIL: 服务未在预期时间内就绪(/healthz);可能端口被占或启动失败"
  exit 1
fi

RSS="$(ps -o rss= -p "$PID" | tr -d ' ')"
echo "resident memory: ${RSS} KB ($((RSS / 1024)) MB) · limit: ${LIMIT_KB} KB (150 MB)"

if [ -z "$RSS" ]; then
  echo "FAIL: could not measure RSS (process not running?)"
  exit 1
fi
if [ "$RSS" -gt "$LIMIT_KB" ]; then
  echo "FAIL: resident memory exceeds 150 MB budget"
  exit 1
fi
echo "OK: within 150 MB budget"
