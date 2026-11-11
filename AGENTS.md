# 意图

本机 Agent 任务看板。**AI 是唯一驱动者**：卡片、事件、待拍板都由 Agent 创建和维护；用户只看、回话、拍板。

## 铁律

- 看板不是人的资产。会话退出、状态无法复原时，主 Agent 直接重开一局，不要让人去维护账本。
- 清空/归档只由主 Agent 发起，必须显式确认（`reset --yes` / `archive --yes`）；用户不需要手动管理。
- 用户不建卡，也不移动卡片。
- 部署归用户；代码只保证本机 `127.0.0.1` 能跑，不碰 Tailscale / 公网 / VPS。

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

## 其他 Agent

```bash
agent-board task new --title "..." --project ... --agent "..."   # 建卡
agent-board progress T-1 "一行进展" --agent "..."                # 进展
agent-board ask --task T-1 --question "..." --options "A,B"       # 待拍板
agent-board done T-1 --text "结论"                                # 完成
agent-board message --text "..." --from main                      # 主会话自己回话
```

- 结论写 `conclusion`，不要只写 `note`。
- 不直连 Telegram；提醒由本机服务发出。
- 同一件事重复上报用 `--key`，服务端只记一次。
