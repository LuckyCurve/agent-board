# Agent Board

[![ci](https://github.com/LuckyCurve/agent-board/actions/workflows/ci.yml/badge.svg)](https://github.com/LuckyCurve/agent-board/actions/workflows/ci.yml)

本机任务看板：Agent 写卡片和进展，用户在看板上回话、拍板，主会话收事件派活。

单个 Go 二进制 + 一个 SQLite 文件，默认只绑 `127.0.0.1:8765`。

**AI 是唯一驱动者**：卡片、事件、待拍板都由 Agent 创建；用户不建卡。看板不是资产，主 Agent 可以随时清空或归档（见 `AGENTS.md`）。

## 启动

```bash
go build -o agent-board.exe .
./agent-board.exe serve
```

首次启动会在 `~/.agent-board/` 生成 `board.db`（全部数据）。同时产生两把凭据：

| 凭据 | 存哪 | 怎么拿 |
|---|---|---|
| 网页口令 | 库里只存加盐哈希 | 首次启动控制台显示一次 |
| Agent token | `~/.agent-board/token`（0600） | 首次启动控制台显示一次，同时写入文件 |

```bash
# 自己指定口令 / token（也可用 BOARD_PASSWORD / BOARD_TOKEN）
./agent-board.exe serve --password '你的口令' --token '你的token'
# 丢了口令
./agent-board.exe passwd --password '新口令'   # 立即生效，已有会话全部失效
# 丢了或想换 token
./agent-board.exe token --new                 # 立即生效，旧 token 作废
./agent-board.exe token                       # 查看当前 token
```

换端口/数据目录：`serve --addr 127.0.0.1:9000 --data D:/board`。
除登录页和静态资源外，页面要网页登录；所有 `/api/*` 另需 Agent token，网页登录态不能代替。登录失败每分钟最多 5 次。
手机访问要自己把监听地址改成局域网地址，并确认只在内网暴露。
iOS Safari / Android Chrome 打开后可「添加到主屏幕」，按独立 App 使用，支持深色模式。

## CLI

CLI 每次都自动读 `~/.agent-board/token`；也可以用 `BOARD_TOKEN` 指定。token 不进仓库、不写日志。

```bash
agent-board task new --title "把导出层改成流式写入" --project 财富 --agent "wealth 开发"
agent-board progress T-1 "42 项单测全过，内存峰值 1.2G → 180M" --agent "wealth 开发"
agent-board ask --task T-1 --question "Excel 导出还要保留吗？" --options "保留,只 CSV,先不做"
agent-board done T-1 --text "已合入，单测通过"
```

`BOARD_URL` 默认 `http://127.0.0.1:8765`。

## 主会话接入

```bash
agent-board watch            # 长轮询，逐行输出 BOARD_* 事件；自动亮「执行中」
agent-board dispatch --seq 42 --to "wealth 开发"
agent-board ack --seq 42     # 处理完，卡片上的回执变绿
agent-board take --seq 42 --agent "wealth 开发"
agent-board fail --seq 42 --reason "数据源缺权限"
agent-board replies          # 最近回执状态
agent-board board            # 当前任务/事件/待拍板数量
```

`watch` 输出的行：

```
BOARD_USER_REPLY      | seq=42 | task=T-1 | agent=wealth 开发 | text=保留，但只给月度报表用 | data={"reply_to_seq":7}
BOARD_USER_MESSAGE    | seq=43 | text=看板先做只读版就行
BOARD_DECISION_ANSWERED | seq=44 | task=T-3 | text=按项目拆
```

收到 `BOARD_USER_REPLY` 后用 `SendMessage` 派给 `agent`，再 `dispatch` 记录派发；`task` 是卡片号。处理完 `ack`。

## 重开一局

```bash
agent-board archive --yes   # 全部收进已完成，待拍板与未结回执收口，历史保留
agent-board reset --yes     # 清空所有数据，编号回到 T-1 / D-1
```

两个动作都需要 `--yes`，只给主 Agent 用；用户不需要手动管理看板。

## HTTP

所有 `/api/*` 都要 `Authorization: Bearer <Agent token>`；网页登录 cookie 不算数。SSE（`/api/stream`）因为 EventSource 不能带头，改用 `?token=`。

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/tasks` | 建卡 `{title,project,agent,column}` |
| PATCH | `/api/tasks/{id}` | 移列 `{column}` |
| POST | `/api/tasks/{id}/events` | 写事件 `{kind,text,agent,key}` |
| POST | `/api/decisions` | 待拍板 `{task_id,question,options}` |
| POST | `/api/decisions/{id}/answer` | 拍板 `{answer}` |
| GET | `/api/events?since=&wait=50` | 长轮询 |
| POST | `/api/claim` / `/api/ack` | 领取 / 确认 |
| GET | `/api/stream` | 浏览器 SSE |
| GET | `/api/board` | 当前数量 |
| POST | `/api/board/archive` | 归档 `{confirm:"archive"}` |
| POST | `/api/board/reset` | 清空 `{confirm:"reset"}` |

事件 kind：`progress note tool artifact conclusion blocked dispatch`。
同一个 `key` 重复提交只记一次。

## 回执状态

用户每次输入会生成一条回执：`received → processing → done | failed | timeout`。

- 收到即 `received`（卡片上亮 ✓）
- 主会话 `watch` 领取自动变 `processing`（显示哪个 Agent 在处理）
- `ack` 变 `done`；`fail` 写明原因
- 10 分钟无人领取、或执行超过 2 小时未收尾，自动标 `timeout`

## 开发

```bash
go vet ./...
go test ./...
go build -o agent-board.exe .
```

界面截图在 `screenshots/`（375 宽单列 + 1440 桌面，深浅两套）。

前端在 `web/`，通过 `embed` 打进二进制；不需要构建步骤，改完重新 `go build`。
