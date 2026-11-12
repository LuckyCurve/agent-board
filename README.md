# Agent Board

[![ci](https://github.com/LuckyCurve/agent-board/actions/workflows/ci.yml/badge.svg)](https://github.com/LuckyCurve/agent-board/actions/workflows/ci.yml)

# 看板是什么

本机任务看板：**AI 是唯一驱动者**，卡片、进展、待拍板都由 Agent 写；用户只看、回话、拍板，不建卡、不管流程。

单个 Go 二进制 + 一个 SQLite 文件，默认只绑 `127.0.0.1:8765`，页面要口令才能进。

| 视图 | 干什么 |
|---|---|
| 对话 | 首页。跟主会话聊天、发图片、引用某张卡继续说；顶部常驻「等你决定 N」 |
| 看板 | 四列：待办 / 进行中 / 阻塞 / 已完成，最右一列是归档 |
| 归档 | 收起来的旧事，可恢复或永久删除 |
| 日历 | 最近 12 周的事件热力图 |

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

除登录页和静态资源外，页面要网页登录；`/api/*` 网页用会话 cookie、CLI 用 Agent token，两种都合法。写操作会校验 Origin，跨站提交一律拒绝。登录失败每分钟最多 5 次。

手机访问要自己把监听地址改成局域网地址，并确认只在内网暴露。iOS Safari / Android Chrome 打开后可「添加到主屏幕」，按独立 App 使用，支持深色模式。

顶栏铃铛启用浏览器通知：在「等你拍板 / 完成 / 阻塞 / 失败 / 进展」时推，点通知回到对应卡片，再点一次即关。进展是最常来的一类，所以同一张卡 5 分钟内只推第一条；其余几类条条推。浏览器只在 HTTPS 或 localhost 下开放通知，局域网 `http://IP:8765` 无法启用；iOS 还要求先「添加到主屏幕」。铃铛旁的纸飞机按钮会发一条测试通知，用它确认手机收不收得到。

推送服务要求留一个像样的联系人地址（`mailto:` 或 `https:` 开头，`localhost` 这类主机名会被 Apple 拒掉）：设 `VAPID_SUB=mailto:admin@你的域名` 或启动时加 `--vapid-sub`。不设时按 `BOARD_URL` 的域名推一个，再不济用 `mailto:admin@example.com`。

图片：网页里可点选、粘贴、拍照上传，缩略图点开看大图；CLI 的 `message / progress / note / event` 都支持 `--image`，可重复写。图片存 `<data>/images/`，只收 PNG / JPG / WebP，单张 ≤5MB、每条 ≤6 张；按魔数识别，不认扩展名。启动时清掉 30 天前的旧图。

---

# Agent 怎么接入

## 三步

```bash
# 1) 起服务
./agent-board.exe serve

# 2) 起监听（后台常驻，断线自动重连）
./scripts/watch.sh start                        # Linux / macOS
powershell -File scripts\watch.ps1 start        # Windows
#    状态 ./scripts/watch.sh status   日志 ./scripts/watch.sh logs   收工 ./scripts/watch.sh stop

# 3) 干活：收到事件就派活，干完 ack
```

CLI 每次都自动读 `~/.agent-board/token`；也可以用 `BOARD_TOKEN` 指定。`BOARD_URL` 默认 `http://127.0.0.1:8765`。token 不进仓库、不写日志。

## 事件与动作

`agent-board watch` 逐行输出事件：

```
BOARD_USER_REPLY      | seq=42 | task=T-1 | agent=wealth 开发 | author=user | text=保留，但只给月度报表用
BOARD_USER_MESSAGE    | seq=43 | author=user | text=看板先做只读版就行
BOARD_AGENT_MESSAGE   | seq=44 | author=main | text=已经改成只读版了
BOARD_DECISION_ANSWERED | seq=45 | task=T-3 | author=user | text=按项目拆
```

| 行 | 谁来接 | 做什么 |
|---|---|---|
| `BOARD_USER_REPLY` | 卡片归属的 Agent | 派活 → `dispatch --seq N --to "Agent"` → 干完 `ack --seq N` |
| `BOARD_USER_MESSAGE` | 主会话自己 | 处理完 `ack --seq N` |
| `BOARD_AGENT_MESSAGE` | 不用接 | 主会话自己发的回话，别 ack |
| `BOARD_DECISION_ANSWERED` | 卡片归属的 Agent | 按答案派活 → `dispatch` + `ack` |
| `BOARD_DECISION` | 提醒用户 | 有人卡在待拍板上，提醒用户去收件箱 |
| `BOARD_EVENT` | 不用接 | 进展播报 |

干不完就 `agent-board fail --seq N --reason "..."`。

## 写看板

```bash
agent-board task new --title "把导出层改成流式写入" --project 财富 --agent "wealth 开发"
agent-board progress T-1 "42 项单测全过，内存峰值 1.2G → 180M" --agent "wealth 开发"
agent-board ask --task T-1 --question "Excel 导出还要保留吗？" --options "保留,只 CSV,先不做"
agent-board done T-1 --text "已合入，单测通过"
agent-board message --text "跑通了" --from main --image shot.png
```

事件 kind：`progress note tool artifact conclusion blocked dispatch`。同一个 `key` 重复提交只记一次。

主会话自己说话用 `agent-board message --text "..." --from main`。这类回话在 `watch` 里是 `BOARD_AGENT_MESSAGE`，不会把卡片点亮成「执行中」，也不会生成待处理的回执。

## 标签与转交

卡片可以打任意关键词标签；页面顶栏下方一行是多选筛选，选中的会记住。

```bash
agent-board task new --title "改登录页" --tag 界面 --tag 登录
agent-board task tag T-1 add 界面,登录
agent-board task tag T-1 remove 登录
agent-board task transfer T-1 --to "看板开发"   # 转交，事件里留痕
```

项目也算一个筛选维度（页面里显示成「项目 · 名字」）；筛选行只在看板页出现，对话和归档不按标签收窄。

已完成的任务有两种归档方式：卡片右下角的「归档」，或卡片详情里的「收进归档」。看板右上角的归档入口显示归档里有多少张卡，点开就是归档页。

## 切换器只列每台机器的主 Agent

一台机器一个主 Agent，子 Agent 不进切换器。主会话常驻 `watch` 时顺手登记本机：

```bash
agent-board host --agent "看板开发"      # 也可以直接：BOARD_AGENT=看板开发 agent-board watch
```

登记的是机器名、用户名和 IP（IP 由服务端按请求来源记，客户端改不了）。切换器里显示成 `看板开发 · DESKTOP-1`，选中后旁边补 `用户名 · IP`。同一台机器换主 Agent，新的顶掉旧的；没人登记过时切换器退回列出全部 Agent。

## 一个 Agent 一把令牌

```bash
agent-board token --agent "看板开发"     # 生成一把只属于这个 Agent 的令牌（只显示一次）
export BOARD_TOKEN=...                    # 用它跑 CLI
```

这把令牌只能看见和改动自己名下的卡：别人的卡读不到、改不了，事件流里也只有自己的动静（没挂卡的消息照收）。主令牌（`~/.agent-board/token`）不受限制，网页用的就是它。

## 回执状态

用户每次输入会生成一条回执：`received → processing → done | failed | timeout`。

- 收到即 `received`（卡片上亮 ✓）
- 主会话 `watch` 领取自动变 `processing`（显示哪个 Agent 在处理）
- `ack` 变 `done`；`fail` 写明原因
- 10 分钟无人领取、或执行超过 2 小时未收尾，自动标 `timeout`

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
| PATCH | `/api/tasks/{id}` | 移列 `{column}`，含 `archived` |
| DELETE | `/api/tasks/{id}?confirm=yes` | 永久删除（还有未结项会拒绝） |
| POST | `/api/tasks/{id}/events` | 写事件 `{kind,text,agent,key,images}` |
| POST | `/api/message` | 往对话里发消息 `{text,images,task_id}` |
| GET | `/api/chat` | 对话消息（含卡片里的历史消息） |
| POST | `/api/images` | 传图片（`Content-Type` 即图片类型） |
| POST | `/api/decisions` | 待拍板 `{task_id,question,options}` |
| POST | `/api/decisions/{id}/answer` | 拍板 `{answer}` |
| GET | `/api/events?since=&wait=50` | 长轮询 |
| POST | `/api/claim` / `/api/ack` | 领取 / 确认 |
| GET | `/api/stream` | 浏览器 SSE，支持 `Last-Event-ID` 补发 |
| GET | `/api/board` | 当前数量 |
| POST | `/api/board/archive` | 归档 `{confirm:"archive"}` |
| POST | `/api/board/reset` | 清空 `{confirm:"reset"}` |

---

# 开发

```bash
go vet ./...
go test ./...
go build -o agent-board.exe .
```

前端在 `web/`，通过 `embed` 打进二进制；不需要构建步骤，改完重新 `go build`。界面截图在 `screenshots/`（375 宽单列 + 1440 桌面，深浅两套）。
