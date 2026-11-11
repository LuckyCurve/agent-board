#!/usr/bin/env bash
# 看板常驻监听：后台跑，断线自动重连。
#
#   scripts/watch.sh start    # 后台起一个监听
#   scripts/watch.sh stop     # 停掉
#   scripts/watch.sh status   # 看在不在
#   scripts/watch.sh logs     # 跟着日志看
#   scripts/watch.sh run      # 前台跑（调试用）
#
# 认这些环境变量：
#   BOARD_BIN      agent-board 可执行文件位置（默认找仓库里那份，再找 PATH）
#   BOARD_URL      看板地址（默认 http://127.0.0.1:8765）
#   BOARD_DATA     数据目录（默认 ~/.agent-board，token 也在这）
#   BOARD_TOKEN    Agent token（默认读 <data>/token）
#   BOARD_CONSUMER 消费者名字（默认 main）
#   BOARD_WAIT     长轮询秒数（默认 15，短一点好断线重连）

set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DATA="${BOARD_DATA:-$HOME/.agent-board}"
RUN_DIR="${BOARD_RUN_DIR:-$DATA/run}"
LOG_FILE="${BOARD_LOG:-$RUN_DIR/watch.log}"
PID_FILE="$RUN_DIR/watch.pid"
CONSUMER="${BOARD_CONSUMER:-main}"
WAIT="${BOARD_WAIT:-15}"

die() { echo "watch.sh: $*" >&2; exit 1; }

find_bin() {
  if [ -n "${BOARD_BIN:-}" ]; then echo "$BOARD_BIN"; return; fi
  for c in "$REPO/agent-board" "$REPO/agent-board.exe" "$DATA/agent-board.exe"; do
    if [ -x "$c" ]; then echo "$c"; return; fi
  done
  if command -v agent-board >/dev/null 2>&1; then command -v agent-board; return; fi
  echo ""
}

bin="$(find_bin)"
[ -n "$bin" ] || die "找不到 agent-board：先 go build -o agent-board . ，或设 BOARD_BIN"

alive() {
  [ -f "$PID_FILE" ] || return 1
  local pid; pid="$(cat "$PID_FILE" 2>/dev/null)"
  [ -n "$pid" ] || return 1
  kill -0 "$pid" 2>/dev/null
}

run_loop() {
  # watch 自己会重连；这层兜住进程整个挂掉的情况。输出交给调用方重定向。
  while true; do
    "$bin" watch --consumer "$CONSUMER" --wait "$WAIT"
    echo "[watch.sh] 监听退出，3 秒后重连"
    sleep 3
  done
}

case "${1:-status}" in
  start)
    if alive; then
      echo "已在运行（pid $(cat "$PID_FILE")）"
      exit 0
    fi
    mkdir -p "$RUN_DIR"
    : >"$LOG_FILE"
    export BOARD_DATA="$DATA"
    if command -v setsid >/dev/null 2>&1; then
      setsid bash "$0" run >>"$LOG_FILE" 2>&1 &
    else
      nohup bash "$0" run >>"$LOG_FILE" 2>&1 &
    fi
    echo $! >"$PID_FILE"
    sleep 1
    if alive; then
      echo "已启动（pid $(cat "$PID_FILE")），日志 $LOG_FILE"
    else
      rm -f "$PID_FILE"
      die "启动失败，看日志 $LOG_FILE"
    fi
    ;;
  stop)
    if ! alive; then
      echo "没有在跑"
      rm -f "$PID_FILE"
      exit 0
    fi
    pid="$(cat "$PID_FILE")"
    kill -- "-$pid" 2>/dev/null || kill "$pid" 2>/dev/null
    sleep 1
    kill -9 "$pid" 2>/dev/null
    rm -f "$PID_FILE"
    echo "已停"
    ;;
  status)
    if alive; then
      echo "运行中（pid $(cat "$PID_FILE")）"
    else
      echo "没在跑"
      exit 1
    fi
    ;;
  logs)
    [ -f "$LOG_FILE" ] || die "还没有日志：$LOG_FILE"
    tail -n 50 -f "$LOG_FILE"
    ;;
  run)
    mkdir -p "$RUN_DIR"
    export BOARD_DATA="$DATA"
    run_loop
    ;;
  *)
    die "用法：$0 start|stop|status|logs|run"
    ;;
esac
