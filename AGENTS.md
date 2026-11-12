# 意图

本机 Agent 任务看板。**AI 是唯一驱动者**：卡片、事件、待拍板都由 Agent 创建和维护；用户只看、回话、拍板。

# 铁律

- 看板不是人的资产。会话退出、状态无法复原时，主 Agent 直接重开一局，不要让人去维护账本。
- 清空/归档只由主 Agent 发起，必须显式确认（`reset --yes` / `archive --yes`）；用户不需要手动管理。
- 用户不建卡，也不移动卡片。
- 部署归用户；代码只保证本机 `127.0.0.1` 能跑，不碰 Tailscale / 公网 / VPS。

---

# 使用本仓库时

（接入看板的主会话 / 中继 Agent 照这一章做）

**中继只做分发，不亲自动手。** 收到用户消息，中继只负责把它变成看板事件、把卡片派给对应 Agent、再把结论回话；改代码、跑测试这类活儿整个派出去。自己手里只留调度。

为什么：中继一旦自己下场干活，卡片上的进度就和真实状态对不上，用户也就没法只看板判断该不该拍板。

## 起步：让人说「帮我听消息」时

```bash
# 1) Telegram 收消息（常驻）
python ~/code/tg-bridge/notify.py poll

# 2) 看板监听（后台常驻，断线自动重连，日志在 ~/.agent-board/run/watch.log）
cd ~/code/agent-board && ./scripts/watch.sh start          # Linux / macOS
cd ~/code/agent-board && powershell -File scripts\watch.ps1 start   # Windows

# 收工
./scripts/watch.sh stop                                   # 或 powershell -File scripts\watch.ps1 stop
```

两个监听都在跑时，用户在任何一边说话，主会话都会收到。

## 主会话循环

会话开始挂常驻监听：

```
Monitor: agent-board watch
```

收到行后：

| 行 | 动作 |
|---|---|
| `BOARD_USER_REPLY` | `SendMessage` 给 `agent` 字段；然后 `agent-board dispatch --seq N --to "Agent"`；办完 `agent-board ack --seq N` |
| `BOARD_USER_MESSAGE` | 自己处理，办完 `agent-board ack --seq N` |
| `BOARD_AGENT_MESSAGE` | 自己发的回话，不用派活；不要 `ack` |
| `BOARD_DECISION_ANSWERED` | 按答案派给卡片归属 Agent，`dispatch` + `ack` |
| `BOARD_DECISION` | 提醒用户去收件箱拍板（Telegram 单向提醒会同时发出） |
| `BOARD_EVENT` | 进展，不用派活 |

干不完就 `agent-board fail --seq N --reason "..."`，别让状态一直停在执行中。

会话结束前：还有未结项就 `agent-board archive --yes`；整个看板都过期了才 `agent-board reset --yes`。

## 写看板

```bash
agent-board task new --title "..." --project ... --agent "..."   # 建卡
agent-board progress T-1 "一行进展" --agent "..."                # 进展
agent-board ask --task T-1 --question "..." --options "A,B"       # 待拍板
agent-board done T-1 --text "结论"                                # 完成
agent-board message --text "..." --from main                      # 主会话自己回话
agent-board task tag T-1 add 界面,推送                            # 加标签（remove 去掉）
agent-board task transfer T-1 --to "另一个 Agent"                 # 转交，事件里留痕
agent-board host --agent "名字"                                  # 登记本机（主会话 watch 时自动做）
```

- 结论写 `conclusion`，不要只写 `note`。
- 不直连 Telegram；提醒由本机服务发出。
- 同一件事重复上报用 `--key`，服务端只记一次。
- 做到一张卡就写一行 `progress`，别攒到最后。
- 多个 Agent 并行时，每个 Agent 用自己的令牌（`agent-board token --agent "名字"`），只能碰自己名下的卡。

---

# 开发本仓库时

（改这块看板的会话照这一章做）

## 每次交付

改完不要只提交，固定三步，缺一不可：

1. **重启本地服务**：换新二进制，保持 `0.0.0.0:8765` 可访问，确认能用口令登录。
2. **推送 GitHub**：推 `main`，等 Actions 变绿（`gh run watch <id> --exit-status`）。
3. **结论带回**：在结论里写清新 PID 和 CI 状态。

推 `main` 前先 `git pull --rebase`：可能有别的 Agent 同时在这个仓库改东西，别覆盖彼此的提交。

## 本地怎么跑

```bash
go vet ./...
go test ./...
go build -o agent-board.exe .
```

CI 跑 `gofmt -l .`、`go vet`、`go test -race`、`go build`。

前端在 `web/`，通过 `embed` 打进二进制，不需要构建步骤，改完重新 `go build` 就行。界面截图在 `screenshots/`（375 宽单列 + 1440 桌面，深浅两套）。

## 改代码时的规矩

- 先写测试再改实现；新功能要有 Go 测试，界面改动要有 `ui_test.go` 里的断言。
- 口令只存加盐慢哈希，token 只进 `~/.agent-board/token`（0600）；**口令、token、数据库都不许进仓库**。
- 会话 cookie 必须 HttpOnly + SameSite；`/api/*` 网页走 cookie、CLI 走 Bearer，两者都合法。写操作必须校验 Origin（cookie 会被浏览器自动带上，光靠它挡不住跨站提交）。
- 改完先本地验（命令行、无头浏览器），再按上面三步交付。
