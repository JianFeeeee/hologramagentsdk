---
name: qq-tools
description: QQ 消息收发（NapCat 协议桥接）。本插件**常驻自启**：它在监听外部消息，不能按需加载。
---

# QQ 消息

## 本插件为什么常驻

QQ 插件在 **HTTP 端口上监听 NapCat 推送**。用户发来的 QQ 消息**随时可能到达**，
到达后它立刻注入给 agent。

如果把本插件改成按需加载：

- 内核启动时它不会被拉起
- **没有人会去拉起它** —— 因为没有人知道有消息在等着
- 结果：QQ 消息**静默丢失**，用户以为 agent 不理他

所以 `auto_start` 必须保持默认（true）。这不是性能取舍，是**功能正确性**。

## 工具

| 工具 | 用途 |
|---|---|
| `qq_get_message` | 按 message_id 取单条正文（消息刚到时先用它） |
| `qq_get_history` | 按会话拉最近历史（**群聊传 group_id，私聊传 user_id**） |
| `qq_list_chats` | 会话列表，按最新消息排序，带未读数与摘要 |
| `qq_mark_read` | 清零某会话未读数 |
| `output_send__qq` | 主动发消息（群/私聊由 meta 决定） |

## ★★ 拉私聊历史：只传 user_id，**不要**传 group_id

这是本插件最容易踩的坑，已经真实咬过人（2026-10-19 连续 13 次调用全失败）：

```
qq_get_history(user_id=2198972886)            ✅ 拉私聊
qq_get_history(group_id=123456)               ✅ 拉群聊
qq_get_history(group_id=0, user_id=2198972886) ❌ 报「消息undefined不存在」
```

**为什么容易错**：`qq_get_message` 取回一条**私聊**消息时，QQ 协议里群号
就是 `0`。如果返回体带了这个字段，把它原样抄进 `qq_get_history` 就会去查
"群 0" —— 而群 0 不存在，NapCat 只会回一句含糊的
`retcode=200 消息undefined不存在`，看起来像"历史拉不到"，其实是参数用错。

现在的行为（已修）：

- 私聊消息的返回体**不再包含 `group_id`** —— 没这个字段就别去抄它
- 后端把 `group_id=0` 判为"未提供"，走私聊端点
- 两个都传会直接报错并说明"只能二选一"，而不是静默选错分支

**判断该传哪个**：看消息来源。私聊注入会带 `user_id=...`；群聊注入带
`group_id=...`。拿不准就先 `qq_list_chats`，它会标明每个会话是群还是私聊。

## 常见报错怎么读

| 现象 | 含义 | 怎么办 |
|---|---|---|
| `消息undefined不存在` | 通常是把私聊当群聊查了（group_id=0 或群号错） | 私聊改传 `user_id`，别传 `group_id` |
| `message_id 可能已过期` | NapCat 侧短号有时效（约 7 天） | 改用 `qq_get_history` 按会话拉 |
| `只能访问当前 QQ 会话` | 权限门：非 owner 只能读本轮会话 | 不是 bug；需要跨会话请让 owner 操作 |

## 注意

- 依赖 **NapCat** 服务在运行且已登录。
- 消息可能过期（NapCat 侧有时效），发送失败时会明确报错。
- **不要在群里 @全体成员**、不要连续刷屏。

## ★ 用 plugin_stop / plugin_start，不要用 plugin_disable / plugin_enable

四个工具看着像同义词，语义差别很大：

| 工具 | 作用 | 跨重启 |
|---|---|---|
| `plugin_stop` | **运行态**关闭，现在停掉 | **不影响**，重启照常加载 |
| `plugin_start` | **运行态**拉起，现在开回来 | 不影响 |
| `plugin_disable` | **持久**禁用 | **会记下来**，开机也不自启 |
| `plugin_enable` | 解除持久禁用 | 会记下来 |

**释放上下文用 `plugin_stop`。** 用 `plugin_disable` 会顺手把"开机自启"
永久关掉 —— 用户下次开机发现插件没了，得再手动开回来。

（本插件**没有**声明 `auto_start: false`，即按契约默认**自启** —— 这是对的，
它必须常驻。所以这里更要小心：`plugin_disable` 会写进持久禁用清单，
让重启后的自启也失效，只能再用 `plugin_enable` 解。）
