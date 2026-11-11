#!/usr/bin/env bash
# Agent Board 服务器端自动部署。
# 由 systemd 定时器调用，也可以手动跑：systemctl start agent-board-autodeploy.service
#
# 流程：拉 main → 比对已部署版本 → 有新提交才构建 → 只换二进制 → 重启 → 健康检查 → 失败回滚。
# 幂等：已经是最新版本时什么都不做；构建失败保留旧二进制、不重启。
#
# 硬约束：绝不改动 /var/lib/agent-board。
# 登录口令、登录会话、Agent token、VAPID 密钥都在那个目录里，动了用户会被登出、口令会失效。
set -euo pipefail

SRC=${AGENT_BOARD_SRC:-/opt/agent-board-src}
BIN=${AGENT_BOARD_BIN:-/opt/agent-board/agent-board}
DATA=${AGENT_BOARD_DATA:-/var/lib/agent-board}
MARK=${AGENT_BOARD_MARK:-/opt/agent-board/DEPLOYED_COMMIT}
SERVICE=${AGENT_BOARD_SERVICE:-agent-board}
HEALTH_URL=${AGENT_BOARD_HEALTH:-http://127.0.0.1:8765/api/health}

# 并发保护：定时器、手动命令、面板按钮可能同时触发。
exec 9>/run/lock/agent-board-autodeploy.lock
if ! flock -n 9; then
  echo "已有部署在进行，跳过"
  exit 0
fi

cd "$SRC"

echo ">> 拉取 main"
git fetch --prune origin

SHA=$(git rev-parse origin/main)
CUR=""
if [ -f "$MARK" ]; then
  CUR=$(cat "$MARK")
fi
if [ "$SHA" = "$CUR" ] && [ -x "$BIN" ]; then
  echo "已是最新版本 $SHA，无需部署"
  exit 0
fi

echo ">> 构建 $SHA"
BUILD_DIR=$(mktemp -d)
trap 'rm -rf "$BUILD_DIR" "$BIN.new"' EXIT
# 从 origin/main 直接构建，不动工作树，避免构建途中被 reset 影响。
git archive origin/main | tar -x -C "$BUILD_DIR"
cd "$BUILD_DIR"
export CGO_ENABLED=0 GOOS=linux GOARCH=amd64
go build -trimpath -ldflags="-s -w" -o "$BIN.new" .
head -c 4 "$BIN.new" | grep -q ELF || { echo "构建产物不可执行" >&2; exit 1; }

echo ">> 换二进制（保留上一版以便回滚）"
if [ -f "$BIN" ]; then
  cp -f "$BIN" "$BIN.prev"
fi
install -m 0755 "$BIN.new" "$BIN"

echo ">> 重启服务"
systemctl restart "$SERVICE"

echo ">> 健康检查"
ok=0
for _ in $(seq 1 30); do
  if curl -fsS -m 2 -H "Authorization: Bearer $(cat "$DATA/token")" "$HEALTH_URL" >/dev/null 2>&1; then
    ok=1
    break
  fi
  sleep 1
done
if [ "$ok" != 1 ]; then
  echo "健康检查失败，回滚到上一版" >&2
  if [ -f "$BIN.prev" ]; then
    install -m 0755 "$BIN.prev" "$BIN"
    systemctl restart "$SERVICE" || true
  fi
  systemctl status "$SERVICE" --no-pager >&2 || true
  exit 1
fi

echo "$SHA" > "$MARK"
echo "已部署 $SHA"
