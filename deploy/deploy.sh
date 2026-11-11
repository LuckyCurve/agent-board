#!/usr/bin/env bash
# Agent Board 云端部署：拉 main → 构建 → 只换二进制 → 重启 → 健康检查。
# 幂等：重复执行安全。
#
# 硬约束：绝不改动 /var/lib/agent-board。
# 登录口令、登录会话、Agent token、VAPID 密钥都在那个目录里，动了用户会被登出、口令失效。
# 部署只做两件事：换 /opt/agent-board/agent-board、重启 systemd 服务。
set -euo pipefail

SRC=${AGENT_BOARD_SRC:-/opt/agent-board-src}
BIN=${AGENT_BOARD_BIN:-/opt/agent-board/agent-board}
DATA=${AGENT_BOARD_DATA:-/var/lib/agent-board}
MARK=${AGENT_BOARD_MARK:-/opt/agent-board/DEPLOYED_COMMIT}
SERVICE=${AGENT_BOARD_SERVICE:-agent-board}
HEALTH_URL=${AGENT_BOARD_HEALTH:-http://127.0.0.1:8765/api/health}

cd "$SRC"

echo ">> 拉取 main"
git fetch --prune origin
git checkout -q main
git reset --hard origin/main
SHA=$(git rev-parse HEAD)

echo ">> 构建"
export CGO_ENABLED=0 GOOS=linux GOARCH=amd64
go build -trimpath -ldflags="-s -w" -o "$BIN.new" .
head -c 4 "$BIN.new" | grep -q ELF || { echo "构建产物不可执行" >&2; exit 1; }

echo ">> 换二进制（留一份上一版，回滚用）"
if [ -f "$BIN" ]; then cp -f "$BIN" "$BIN.prev"; fi
install -m 0755 "$BIN.new" "$BIN"
rm -f "$BIN.new"

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
  echo "健康检查失败，回滚到上一版二进制" >&2
  if [ -f "$BIN.prev" ]; then
    install -m 0755 "$BIN.prev" "$BIN"
    systemctl restart "$SERVICE" || true
  fi
  systemctl status "$SERVICE" --no-pager >&2 || true
  exit 1
fi

echo "$SHA" > "$MARK"
echo "已部署 $SHA"
