# qq · QQ 消息桥接

通过 [NapCat](https://github.com/NapNeko/NapCatQQ) 把 QQ 接成 HologramAgent 的一个 IO 通道：
让 agent 收发 QQ 消息、读群/好友信息、传文件。

> ⚠️ 这是**安全敏感**插件：它让外部 QQ 用户能触达 agent 的工具。
> 本文档的「权限模型」一节请务必读完。

## 通道与钩子

| 类型 | 名称 | 说明 |
|---|---|---|
| 出站 | `qq` | `CapText` + `CapFile` + `CapImage` + `CapAudio`；发消息/文件给 QQ |
| 入站 | `qq` | `NoMemory: true` + `Cleaner` + `RecallPolicy: None` |

四个阶段钩子（全部 `StageScopeGlobal`）：

| 钩子 | 作用 |
|---|---|
| `on_input` | 把本轮 QQ 身份**绑到帧上** |
| `before_toolcall` | 权限门：逐个工具判断是否放行 |
| `post_action` | 清掉被拒绝时模型已经吐出的废话 |
| `after_output` | 收尾时清理插件全局身份 |

## 工具（20 个）

| 工具 | 说明 |
|---|---|
| `qq_get_message` | 按 `message_id` 取消息正文、发送者、附件 |
| `qq_get_history` | 取群/私聊最近历史消息 |
| `qq_list_chats` | 会话列表（按最新消息排序，带未读数与摘要） |
| `qq_mark_read` | 把某会话未读数清零 |
| `qq_get_groups` | 群列表，可按关键词搜 |
| `qq_get_friends` | 好友列表，可按昵称/备注搜 |
| `qq_get_recent_contacts` | 最近有消息的联系人与群 |
| `qq_resolve_name` / `qq_resolve_nickname` | 名字 ↔ QQ 号互查 |
| `qq_get_group_member_info` | 群成员信息 |
| `qq_group_manage` | 群综合管理（见下） |
| `qq_friend_action` | 好友操作 |
| `qq_get_group_files` | 群文件列表 |
| `qq_download_file` / `qq_upload_group_file` / `qq_get_download_tasks` | 文件传输与任务 |
| `qq_read_document` | 读 QQ 传来的文档 |
| `qq_video_download` | 下载视频 |
| `qq_send_like` | 点赞 |

`qq_group_manage` 一个工具承载多种操作（`command` 参数）：
`leave` 退群、`kick` 踢人、`ban`/`unban` 禁言解禁、`rename` 改名、`mute-all` 全员禁言、
`set-card` 设名片、`set-admin` 设管理、`set-title` 设头衔、`member-list`、`group-info`、
`msg-history`、`recall` 撤回、`pin-msg` 精华、`list-files`、`pending-requests`、`folder-create` 等。

**破坏性操作**（`leave`/`kick`/`ban`/`unban`/`rename`/`mute-all`/`set-card`/`set-admin`/
`set-title`/`recall`/`pin-msg`/`folder-create`）**必须显式传 `confirm: true`**。

## 权限模型

这是本插件最重要的部分。

### 身份分级

| 身份 | 权限 |
|---|---|
| **owner**（Bot 所有者） | 私聊或群聊均**完整放行** |
| **普通 QQ 用户** | 只放行白名单内的工具 |

### 身份必须「绑帧」，不能只存插件全局

源码注释记录了两个真实故障，这就是绑帧的原因：

1. **中断抢占后身份丢失**：中断会抢占当前轮、把现场压栈。中断轮收尾时
   `after_output` 会清空插件**全局**身份；随后外层被恢复（`resumeTask` 复用同一帧、
   **不重跑 `on_input`**）。若身份只存全局，恢复后的外层就是"无身份"，
   `before_toolcall` 在 `!auth.active` 处直接返回 —— **整个权限门失效**。
2. **运行中到达的消息改写身份**：新消息会调 `activateAuthContext` 改写全局身份，
   把**正在跑的那一轮**换成另一方的身份（换高=越权，换低=误拒）。

帧上的 `Extra` 随帧一起压栈/恢复，正好是"这一轮的身份"。

### 合并取最小权限

多来源被内核合并到同一推理时，权限**取交集**而非并集：

```go
p.auth.owner = p.auth.owner && next.owner
```

防的是"非所有者请求 + 随后所有者消息"意外把前一个请求提权。

### 硬私有工具

非所有者**一律拒绝**（不看白名单），按前缀拦截：
`calendar_`、`email_`、`mail_`、`agentmail_`、`memory_`、`knowledge_`、`device_`、
`devicectl_`、`terminal_`、`shell_`、`command_`、`exec_`、`filesystem_`、`agentfs_`、
`config_`、`settings_`、`plugin_`、`plugins_`，
外加 `read_file`、`write_file`、`edit_file`、`delete_file`、`list_files`、`run_command`、
`homeagent_config`、`homeagent_restart`、`output_send__email`、`output_send__mail`。

> ⚠️ `homeagent_config` / `homeagent_restart` 是**已注册的工具名**（见 `plugin.go` 的
> 工具清单），属于对外协议面 —— 改名会让存量调用方失效，所以刻意保留旧名。

### 参数与会话一致性校验

光看工具名不够，还要检查**参数指向的会话与当前身份一致**，否则可以拿别人的
`message_id` 去读别处内容：

- 带 `message_id` 的工具：该 ID 必须属于当前 QQ 会话（`lookupMsgRef` 校验 peer 与群/私聊类型）。
- `get_group_member_info` / `get_group_files`：`group_id` 必须是**当前群**。

### 频率与重复控制

| 键 | 作用 |
|---|---|
| `max_qq_tool_calls` | 单轮工具调用上限 |
| `max_qq_output_calls` | 单轮输出调用上限 |
| `max_duplicate_qq_send` | 重复发送上限，防刷屏 |
| `batch_window_ms` / `batch_max_ms` | 消息合批窗口 |

被拒时只允许**发一次权鉴说明**，之后锁止本轮剩余工具调用
（`clearDeniedResponse` 再把模型已写出的内容清掉，避免输出里带一堆"我不能…"）。

## 配置项

### 连接

| 键 | 默认 | 说明 |
|---|---|---|
| `napcat_url` | — | NapCat 服务地址 |
| `listen` | — | 本插件 HTTP 监听地址 |
| `webhook_token` | — | webhook 校验令牌 |

### 身份与准入

| 键 | 默认 | 说明 |
|---|---|---|
| `owner` | 空 | Bot 所有者 QQ 列表（逗号分隔），拥有完整权限 |
| `admin` | 空 | **旧配置名**，`owner` 为空时作为所有者列表（兼容用） |
| `dm_policy` | `open` | 私聊策略：`open` / `allowlist` / `disabled` |
| `allow_from` | 空 | 私聊白名单（QQ 号，逗号分隔） |
| `group_policy` | `open` | 群聊策略：`open` / `allowlist` / `disabled` |
| `group_allow_from` | 空 | 群白名单 |
| `private_tool_allowlist` | 空 | 私聊下非所有者可用的工具 |
| `group_tool_allowlists` | 空 | 按群配置的工具白名单 |

### 文件与转发

| 键 | 说明 |
|---|---|
| `files_dir` | 本地文件目录 |
| `remote_dir` | 供 NapCat 容器访问的目录（发文件前先复制到这里） |
| `agentfs_dir` | agent 文件系统目录 |
| `forward_rules` | JSON 数组，每项 `{group_id,host,port,password,template}`：匹配的群消息经 **RCON** 转发到 Minecraft；`template` 支持 `{nickname}` / `{message}` 占位 |

## 部署前提

需要**自行部署 NapCat**（本插件不含 QQ 协议实现，只是 NapCat 的客户端）。
发文件前会先把文件复制到 `remote_dir`，因为 NapCat 通常在容器里，看不到宿主任意路径。

## 测试

```bash
go test -count=1 -race ./...
```

含权限门与绑帧的回归测试。改动权限相关代码后务必跑 `-race`。

## 构建

```bash
hmapdev build
```
