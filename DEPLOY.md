# Agent Board 部署备忘

**地址**：https://43-165-128-180.sslip.io
**服务器**：43.165.128.180（Ubuntu 26.04）

结构：Caddy 在 80/443 终止 HTTPS 并反代 → `agent-board` 只监听 `127.0.0.1:8765`（公网不可直连）。

| 项 | 路径 |
|---|---|
| 二进制 | `/opt/agent-board/agent-board` |
| 上一版（回滚用） | `/opt/agent-board/agent-board.prev` |
| 数据 / DB | `/var/lib/agent-board/`（口令哈希、会话、token、VAPID、图片都在此） |
| Agent token | `/var/lib/agent-board/token` |
| systemd 单元 | `/etc/systemd/system/agent-board.service` |
| Caddy 配置 | `/etc/caddy/Caddyfile` |
| 源码 checkout | `/opt/agent-board-src`（服务器自己 clone 公开仓库） |
| 部署脚本 | `/usr/local/bin/agent-board-autodeploy`（入口）、`deploy/deploy.sh`（仓库里） |
| 定时器 | `agent-board-autodeploy.timer`（每 90 秒） |
| 已部署版本 | `/opt/agent-board/DEPLOYED_COMMIT` |
| 日志 | `journalctl -u agent-board -f` / `-u caddy -f` / `-u agent-board-autodeploy -f` |

## 自动更新

服务器自己在盯仓库，**不需要 GitHub Actions，也不依赖任何一台本机**。

流程：定时器每 90 秒看一次 `main` → 有新提交就从公开仓库拉下来 → 在服务器上用 Go 编译 →
只换 `/opt/agent-board/agent-board` → `systemctl restart agent-board` → 健康检查。

- 已是最新版本时什么都不做（幂等）。
- 编译失败保留旧二进制、不重启，网站照常可用。
- 换完健康检查不过会自动回滚到上一版。
- **只换二进制，绝不碰 `/var/lib/agent-board`**：登录口令、登录会话、Agent token、VAPID 都在那儿，
  换了版本用户不会被登出，口令也不会失效。

手动刷新（立刻拉一次，不等定时器）：

```bash
systemctl start agent-board-autodeploy.service
```

面板右上角还有「立即更新」按钮，等同于上面这条命令（走 `POST /api/deploy`，
只认 Agent token；没启用自动更新的机器上不显示）。

看部署日志：

```bash
journalctl -u agent-board-autodeploy -n 100 --no-pager
systemctl list-timers agent-board-autodeploy.timer
```

## 启停

```bash
systemctl start agent-board      # 启动（Caddy 也要在跑：systemctl start caddy）
systemctl stop agent-board       # 停止
systemctl restart agent-board    # 重启（改口令后）
systemctl status agent-board     # 状态
```

## 改口令

在服务器上执行（明文不落盘，立即生效并使旧登录失效）：

```bash
/opt/agent-board/agent-board passwd --password '新口令' --data /var/lib/agent-board
systemctl restart agent-board
```

> 改口令会清掉所有登录会话，浏览器要重新登录；自动更新不会。

## 换 Agent token

```bash
/opt/agent-board/agent-board token --new --data /var/lib/agent-board
```

新 token 写进 `/var/lib/agent-board/token`，每次请求实时读取，**无需重启**，旧 token 立刻作废。

## 云端凭据放在哪

云端看板的登录口令和 Agent token **不进仓库**，存在使用者的本机：

```
~/.agent-board/cloud-board.env
  BOARD_URL=https://43-165-128-180.sslip.io
  BOARD_TOKEN=...
  BOARD_PASSWORD=...
```

权限只给本人（Windows 上可用 `icacls` 收紧）。本机 CLI 用时 source 这个文件即可。

轮换后覆盖写这个文件——口令用上面的 `passwd`，token 用 `token --new`，
两者都不写进 git、不贴进聊天。

## 看日志

```bash
journalctl -u agent-board -n 100 --no-pager
journalctl -u agent-board -f
journalctl -u caddy -f            # 证书签发 / 反代报错
```

## 排查：手机上收不到通知

先看 `POST /api/push/test` 的返回和日志：

```bash
journalctl -u agent-board -n 50 --no-pager | grep -i push
```

日志会写 `push: <推送服务主机> 拒绝了这条通知: status=... body=...`。

- **`403 BadJwtToken`**：VAPID JWT 的联系人地址（`sub`）不合法。Apple 不认 `localhost` 这类主机名。
  用 `VAPID_SUB`（或 `--vapid-sub`）配成 `mailto:` 或 `https:` 地址；不配时按 `BOARD_URL` 推一个，
  最后退回 `mailto:admin@example.com`。
- **`404` / `410`**：订阅真失效了，程序会自动删掉那条；手机上重开一次铃铛重新订阅。
- **`devices=1 / sent=0 / failed=1` 且日志里没有推送行**：老版本不在非 404/410 失败时写日志，
  先确认部署的是带日志的版本。
- 换数据目录或清库会导致 VAPID 密钥变化、旧订阅全部失效；只换二进制不受影响。

## 应急：手工换二进制

自动更新坏了才用：

```bash
# 本机：从当前 main 构建
git archive origin/main | tar -x -C /tmp/ab-build
cd /tmp/ab-build
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o agent-board-linux-amd64 .
scp -i ~/.ssh/agent_board_deploy agent-board-linux-amd64 root@43.165.128.180:/root/agent-board-deploy
```

```bash
# 服务器
install -m 0755 /root/agent-board-deploy /opt/agent-board/agent-board && rm -f /root/agent-board-deploy
systemctl restart agent-board && systemctl is-active agent-board
```

## 防火墙 / HTTPS

- 只放行 22/80/443；`ufw status` 可查。8765 不对外。
- 证书由 Caddy 自动向 Let's Encrypt 申请并续期。
- 会话 Cookie 带 `Secure`；Web Push 需 HTTPS + 浏览器授权，登录后点「开启通知」。
