---
name: qq-tools
description: QQ 消息收发（NapCat 协议桥接）+ 发收藏表情包。本插件**常驻自启**：它在监听外部消息，不能按需加载。
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
| `output_send__qq` | **发消息、发图、发文件、发表情都走它**（type 决定形态，群/私聊由 meta 决定；payload 传数组可一次发多条） |
| `qq_list_stickers` | 列出收藏表情包（index + url + 已缓存的 desc/mood） |
| `qq_describe_sticker` | 给表情记录内容/情绪描述（先用 see_image 看，再记） |
| `qq_add_sticker` | 把别人发的表情收藏到自己账号（message_id / url / file） |
| `qq_remove_sticker` | 从收藏里删表情（index / url），清理误收 |

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

## 表情包

### 该主动发，不要等用户要

表情是**情绪的表达方式**，不是"用户点名才发的附件"。在 QQ 聊天里遇到这些场合，
**主动**挑一张合适的发过去（发完可以再补一句文字，或只发表情）：

- 用户吐槽、抱怨、被气到 → 发一张「无语 / 翻白眼 / 扶额 / 摊手」类的
- 用户自嘲、搞砸了、认错 → 发一张「心虚 / 委屈 / 求放过」类的
- 用户分享好消息、得意 → 发一张「比心 / 开心 / 鼓掌」类的
- 你在卖萌、撒娇、被夸 → 发一张「害羞 / 得意」类的
- 对话冷场/收尾，可以用表情代替一句干巴巴的「好的」

**分寸**（避免刷屏）：

- 一轮对话最多 1～2 张，别每句都贴。
- 表情要和当下情绪**真的对得上**；没有匹配的就只说话，别硬发。
- 别在同一轮里连发表情；连续对话里也别一直用同一张。
- 群里发要更克制：只在明确对某人说话、或气氛合适时发。

挑选方式：先 `qq_list_stickers` 看 `mood`/`desc`，选最贴的一张 `index`；
列表里没有匹配的（或都没有描述）就先按下面的流程补描述，别乱发一张。

NapCat 的表情接口是 `fetch_custom_face`（收藏表情，返回的是**图片 URL**，
不是 mface 的 emoji_id/key —— 后者只有收到别人发表情时才带）。所以发收藏表情
走 image 段：QQ 会渲染成表情图。

```
qq_list_stickers count=20
  → [{index:1, url:"https://p.qpic.cn/...", desc:"猫翻白眼", mood:"无语"}, ...]
```

发表情同样走 `output_send__qq`，两种写法都行：

```
# 按收藏序号（推荐：不用记 URL，表情库变了也不会错）
output_send__qq type=sticker payload="" meta={"user_id":2198972886,"sticker_index":1}

# 直接给 URL
output_send__qq type=sticker payload="https://p.qpic.cn/..." meta={"user_id":2198972886}
```

### 别人发的表情，可以收进自己收藏

用户（或群里）发来表情时，如果想留着以后自己用：

```
qq_add_sticker message_id=12345     # 那条消息里的表情全部收藏
qq_add_sticker url="https://..."    # 单张
qq_add_sticker file="/path/a.gif"   # 本机图片
```

收藏后 `qq_list_stickers` 就能看到；新收藏的还没有描述，第一次发之前先 `see_image`
看一眼再用 `qq_describe_sticker` 记下来（否则列表里只有 URL，挑不准）。

收错了用 `qq_remove_sticker index=N` 删掉（内部走 NapCat delete_custom_face）。

★ 如果用户说「你没有添加表情的接口」：先确认它是不是在**群/非 owner 私聊**里提的——
`qq_add_sticker` 默认不在给外部用户的工具白名单里，会被权限门挡下。那种情况要
如实说「需要管理员开放权限」，不要说工具不存在。

### 第一次用某张表情：先看再记，之后按情绪挑

NapCat 只给图片 URL，没有名称/情绪。所以列表里 `desc`/`mood` **是模型自己
看过后缓存的**（存在插件配置 `sticker_descs`，重启不丢）：

1. `qq_list_stickers` 看到某张没有 desc ⇒ `see_image(url)` 亲自看；
2. `qq_describe_sticker(url="…", desc="画面内容", mood="无语/开心/嘲讽…")` 记下来；
3. 之后每次 list 都自带描述，直接按 mood 挑 `index` 发送即可。

描述要写**能帮你在别的场合挑图**的信息：画的是什么、什么梗、什么情绪下合适。

## 权限门（可配置，不是写死的）

非所有者（群里的人、非 owner 好友）能用的工具由插件配置决定，三个开关：

| 配置项 | 默认 | 说明 |
|---|---|---|
| `private_tool_allowlist` | 公共工具 + 当前会话 QQ 工具 | 非 owner 私聊白名单（精确名 / 尾部 `*`） |
| `group_tool_allowlists` | 同上（`*` 为默认群） | 群聊白名单，可按群号单独配 |
| `hard_private_tools` | 日历/邮件/记忆/知识库/终端/文件/配置等 | **硬性**拦（白名单也放不开）；清空 `[]` = 不硬拦，全交给白名单 |
| `group_require_at` | `true` | 群里是否必须 @ 才响应；`false` = 群里所有消息都回 |
| `session_scope_enforce` | `true` | 非 owner 是否只能读本会话；`false` = 可按 ID 读别的会话 |

★ 如果用户抱怨"机器人没权限/不能看图/不能下载"，先查这几项，而不是让它道歉：
它自己的工具清单里就有 `plugin_*` 之外的管理工具不一定开着 —— 权限是部署者配置，
可以在 WebUI 插件设置里改。owner 不受这些限制。

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
