#!/usr/bin/env bash
# 启动独立的企业版演示。不使用生产身份源，不清理其他数据库。
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PG="/opt/homebrew/opt/postgresql@16/bin"
DEMO="$ROOT/.demo"
mkdir -p "$DEMO"
fail() {
  echo "错误：$1" >&2
  exit 1
}

if [[ ! -x "$PG/initdb" || ! -x "$PG/pg_ctl" || ! -x "$PG/psql" ]]; then
  fail "没有找到 PostgreSQL 16：$PG。数据库未就绪，演示没有启动。"
fi

stop_recorded() {
  local file="$1"
  [[ -f "$file" ]] || return 0
  local pid
  pid="$(cat "$file" 2>/dev/null || true)"
  if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
    pkill -P "$pid" 2>/dev/null || true
    kill "$pid" 2>/dev/null || true
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.3
    done
  fi
  rm -f "$file"
}

if [[ ! -f "$DEMO/pg/PG_VERSION" ]]; then
  "$PG/initdb" -D "$DEMO/pg" --username=artex --auth-local=trust --auth-host=trust --encoding=UTF8 --locale=C \
    || fail "初始化演示数据库失败。日志还没有生成。"
fi
if ! "$PG/pg_ctl" -D "$DEMO/pg" status >/dev/null 2>&1; then
  "$PG/pg_ctl" -D "$DEMO/pg" -o "-p 55432 -k $DEMO/pg" -l "$DEMO/pg.log" start \
    || fail "PostgreSQL 没有启动。请查看 $DEMO/pg.log"
fi
if ! "$PG/pg_isready" -h 127.0.0.1 -p 55432 -U artex >/dev/null 2>&1; then
  fail "PostgreSQL 没有在 127.0.0.1:55432 接受连接。请查看 $DEMO/pg.log"
fi
if ! "$PG/psql" -h 127.0.0.1 -p 55432 -U artex -d artex_enterprise_demo -c 'SELECT 1' >/dev/null 2>&1; then
  "$PG/createdb" -h 127.0.0.1 -p 55432 -U artex artex_enterprise_demo \
    || fail "创建演示库 artex_enterprise_demo 失败。"
fi

stop_demo_port() {
  local port="$1" pids pid cmd ppid pcmd
  pids="$(lsof -nP -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null || true)"
  [[ -z "$pids" ]] && return 0
  for pid in $pids; do
    cmd="$(ps -p "$pid" -o command= || true)"
    cwd="$(lsof -a -p "$pid" -d cwd -Fn 2>/dev/null | awk '/^n/ {print substr($0,2); exit}')"
    if [[ "$cmd" != *"$ROOT"* && "$cmd" != *"artex"* && "$cwd" != "$ROOT"* ]]; then
      fail "端口 $port 已被其他程序占用：$cmd"
    fi
    ppid="$(ps -p "$pid" -o ppid= | tr -d ' ' || true)"
    kill "$pid" 2>/dev/null || true
    if [[ -n "$ppid" && "$ppid" != "1" ]]; then
      pcmd="$(ps -p "$ppid" -o command= || true)"
      if [[ "$pcmd" == *"$ROOT"* || "$pcmd" == *"artex"* || "$pcmd" == *"next dev -p $port"* ]]; then
        kill "$ppid" 2>/dev/null || true
      fi
    fi
  done
  local i
  for ((i = 1; i <= 20; i++)); do
    if ! lsof -nP -tiTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.3
  done
  fail "端口 $port 上的旧演示进程没有退出。"
}

stop_recorded "$DEMO/backend.pid"
stop_recorded "$DEMO/frontend.pid"
stop_demo_port 8797
stop_demo_port 5174

export ARTEX_ENTERPRISE=1
export ARTEX_DEMO_IDP=1
export ARTEX_PUBLIC_ORIGIN=http://127.0.0.1:5174
export ARTEX_PG_DSN="postgres://artex@127.0.0.1:55432/artex_enterprise_demo?sslmode=disable"
export AUTOPENTEST_API=http://127.0.0.1:8797
cd "$ROOT"
set -m
go run ./cmd/artex -addr 127.0.0.1:8797 -data "$DEMO/data" >"$DEMO/backend.log" 2>&1 &
back=$!
echo "$back" >"$DEMO/backend.pid"
( cd "$ROOT/web" && npx next dev -p 5174 -H 127.0.0.1 ) >"$DEMO/frontend.log" 2>&1 &
front=$!
echo "$front" >"$DEMO/frontend.pid"
cleanup() {
  pkill -P "$back" 2>/dev/null || true
  pkill -P "$front" 2>/dev/null || true
  kill "$back" "$front" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

wait_http() {
  local url="$1" name="$2" tries="$3"
  local i
  for ((i = 1; i <= tries; i++)); do
    if curl -fsS --max-time 2 "$url" >/dev/null 2>&1; then
      return 0
    fi
    if ! kill -0 "$back" 2>/dev/null; then
      fail "$name 进程已退出。请查看 $DEMO/backend.log"
    fi
    sleep 0.5
  done
  return 1
}

if ! wait_http "http://127.0.0.1:8797/api/health" "后端" 60; then
  fail "后端健康检查没有通过。请查看 $DEMO/backend.log"
fi
status="$(curl -fsS --max-time 3 http://127.0.0.1:8797/api/enterprise/status || true)"
if [[ "$status" != *'"enterprise":true'* ]]; then
  fail "企业模式没有打开。后端返回：${status:-空}。请查看 $DEMO/backend.log"
fi
if [[ "$status" != *'"idp_available":true'* ]]; then
  fail "演示身份源没有就绪。后端返回：${status:-空}。请查看 $DEMO/backend.log"
fi
if ! wait_http "http://127.0.0.1:5174/login" "前端" 160; then
  if ! kill -0 "$front" 2>/dev/null; then
    fail "前端进程已退出。请查看 $DEMO/frontend.log"
  fi
  fail "前端登录页没有打开。请查看 $DEMO/frontend.log"
fi

cat <<'EOF'
演示已就绪。下面的地址只能在这台电脑的浏览器里打开。

登录页：http://127.0.0.1:5174/login
员工：周宁（demo-analyst），登录后进入原来的任务工作台
领导：顾清（demo-biz），登录后进入报告与结论
复核：陈溯（demo-reviewer）
项目 B 员工：苏晚（demo-member-b）
登录：打开登录页，点「使用企业账号登录」，选择对应的人，再点「继续并返回 ARTEX」。口令使用登录页上已经填好的虚构口令。当前身份源是本机演示，不是企业真实 IdP。

健康状态：
  数据库：正常（127.0.0.1:55432 / artex_enterprise_demo）
  后端：正常（127.0.0.1:8797）
  前端：正常（127.0.0.1:5174）
  演示身份源：正常
EOF
while kill -0 "$back" 2>/dev/null && kill -0 "$front" 2>/dev/null; do
  sleep 2
done
fail "有服务中途退出。请查看 $DEMO/backend.log 和 $DEMO/frontend.log"
