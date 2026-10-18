# HomeAgent SDK

HomeAgent 插件开发 SDK，用于构建与 HomeAgent 平台交互的智能插件。

> 📖 **完整文档**：<https://sdk.homeagent.jianfgit.xyz/>
>
> 快速开始 / API 参考 / 指南 / 示例插件都在那里，**本 README 只是速览**，
> 接口细节以文档站为准。直接进入：[快速开始](https://sdk.homeagent.jianfgit.xyz/guide/getting-started/)
> · [API 参考](https://sdk.homeagent.jianfgit.xyz/api/) · [能力边界](https://sdk.homeagent.jianfgit.xyz/guide/capability-boundary/)
>
> 面向 agent 的纯文本入口：[`llms.txt`](https://sdk.homeagent.jianfgit.xyz/llms.txt)
> （含全部 API 的 [`llms-full.txt`](https://sdk.homeagent.jianfgit.xyz/llms-full.txt)）。

## 版本与兼容性

当前：**SDK 1.3.0**（需内核 **1.3.0+**）。

**版本号跟随内核的中版本，patch 位恒为 `.0`**：

| 内核版本 | 对应 SDK |
|---|---|
| 1.0.0 / 1.0.1 / … / 1.0.4 | 1.0.0 |
| 1.1.0 / 1.1.1 / … / 1.1.N | **1.1.0** |
| 1.2.0 / 1.2.1 / … / 1.2.N | **1.2.0** |
| 1.3.0 起 | **1.3.0** |

内核的 patch 位专用于 bugfix 与漏洞修复，不碰公开接口，所以 SDK 版本号不跟着动——
否则你要么被迫跟版、要么怀疑自己版本过时，而接口其实一个字都没变。

因此 **SDK 仓在一个中版本里只发一次**（`vX.Y.0`），核心的 `v1.3.1`/`v1.3.2`/… 不伴随 SDK 发版。
（2026-09-13 曾误发过 `v1.3.1`，已撤回 —— patch 位带非零数字的 SDK tag 都是错误的。）

**1.0.x 插件升到 1.1.x：不需要改代码，也不需要重编。** 1.1.0 的新增全部是
「插件调用、内核实现」方向，不调就不受影响（已用 SDK 0.9.2 编的旧 `plugin.bin`
实测验证：在新内核上直接建链通过，因为握手校验的是主版本号、不是 SDK 全等版本）。
想用新字段时重编即可。

**1.1.x 插件升到 1.2.x：接口纯追加；若内核共享区布局改了则需重编。** 公开接口没有签名变更
（新增 `InjectOptions` 与六个 `*Opts` 变体、`ChannelDef.ContextPolicy`），不调新能力就不受影响。
1.2.0 同时把 fd3 换成了统一共享区（SuperBlock + StageContext + EvtRing），
这类**布局**变化由握手时的结构判据拦下（统一区魔数、`field_count` 描述符个数），
而不是靠某个版本号——它们都是二进制里的派生事实，不会「忘同步」。

## 1.3.0 新增：注入优先级与动态输出通道

### 注入优先级（`InjectOptions.Priority`）

插件可以声明**自己这次注入的中断级别**，内核按四级阶梯调度：

| 级别 | 常量 | 谁用 |
|---|---|---|
| L1–L3 | `PriorityL1` / `PriorityL2` / `PriorityL3` | 插件按紧急程度自选（L1 最低） |
| L4 | `PriorityL4` | **保留给内核与内核级插件**（内核自身事件、内核级通道） |

- 零值（不声明）与旧的注入调用**完全等价**：按排队处理，不抢占任何正在执行的回合
  ⇒ 存量插件不需要改一行、也不需要重编。
- 高优先级中断可以**抢占**低优先级正在跑的回合；被抢占的回合挂起、之后恢复继续
  （现场保存/恢复对插件透明）。
- 排队输入**没有级别**：排队就是排队，任何中断都能插到它前面。

### 动态输出通道（`UnregisterOutputChannel`）

`RegisterOutputChannel` 注册的通道此前只增不减。对**随资源生灭**的通道（典型：远程设备
一台设备一个输出通道），设备掉线后通道还在，模型会继续对一个死通道发消息并以为发成功了。

1.3.0 起成对提供：

| API | 用途 |
|---|---|
| `UnregisterOutputChannel(name)` | 注销输出通道（含能力表与工具） |
| `OutputChannelUnregistrar` / `SetOutputChannelUnregistrar` | 插件侧拿到注销句柄（内核注入） |

⚠️ 通道名要**由插件派生得又合法又唯一**（外部 id 不能直接当通道名）——
设备 id 这类外部输入可能带 `/` 等字符，而通道名会拼进 LLM 函数名 `output_send__<name>`，
违规会让**整条 LLM 请求**被上游拒绝（2026-09-13 生产事故：`device/<id>` 导致全量对话 403）。
派生规则与约束见下方「输出通道」一节。

## 业务字段透传（`InjectOptions.Payload`）

插件可以在**一次注入**上携带自己的业务字段，内核**不解释、只透传**：

```go
type InjectOptions struct {
	// ... 上面的 NoMemory / ContextPolicy / CleanerName / Priority ...

	// Payload 是本次注入要额外并入事件 payload 的字段（不透明）。
	// 合并时**内核保留键不可覆盖**（见下）。
	Payload map[string]interface{}
}
```

### 它解决什么

`InputEvent.Payload` 本就是通道送入内核的请求体，业务字段本该在那里。但
`IOInjector` 的注入方法全是 `(source, channel, text, opts)` —— 插件无法直接
给 payload 加字段。`Payload` 就是这个扩展口。

典型用途：**通道插件需要知道"这条消息是发给谁的"**。

```go
// 注入时声明身份（键名加插件前缀，避免与内核字段撞车）
sdk.InjectInterruptTextOpts("qq", "qq", text, sdk.InjectOptions{
	NoMemory: true,
	Payload:  map[string]interface{}{"qq_uid": userID},
})
```

### 内核怎么传递

```
注入点 opts.Payload
  → 与真 payload 合并（保留键跳过）
  → emitResponse / output_send 把**非保留键**原样写进 stageCtx.Extra
```

插件在 stage 里直接读：

```go
uid, ok := ctx.Extra["qq_uid"]
```

### 保留键不可覆盖

`no_memory` / `context_policy` / `recall_policy` / `scene_policy` /
`cleaner_name` / `priority` / `type` / `content` / `interrupt` / `stop` /
`command` / `scene` / `source` / `channel` / `media_blocks` / `media_type`
由内核语义决定，插件写了会被**跳过并记日志**（不是拒绝整次注入）。

原因：允许覆盖等于允许插件伪造「这条不记记忆」或「这条要立即打断」——
那是权限提升，不是便利。

## 注入行为与上下文裁剪（1.2.0）

「记不记入记忆」与「要不要据此裁剪上下文」这两件事，原先只有 `ToolDef` 能声明；
1.2.0 起**注入侧也能声明**，并且二者共用同一套语义与取值。

```go
type InjectOptions struct {
	NoMemory      bool   // true = 不参与记忆计算（向量化/关键词提取/蒸馏），原文仍留在上下文
	ContextPolicy string // ""/none = 不裁剪（默认）；prune = 据此裁剪上下文
	CleanerName   string // 计算层过滤函数名：先经 Cleaner 得到实际有效内容，再计算/裁剪
}

const (
	ContextPolicyNone  = "none"
	ContextPolicyPrune = "prune"
)

// 六个变体，与旧的三参数方法一一对应，只多一个 opts
InjectTextOpts(source, channel, text string, opts InjectOptions)
InjectInterruptTextOpts(source, channel, text string, opts InjectOptions)
InjectInputSyncOpts(source, channel, text string, opts InjectOptions) string
InjectInputMediaOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions)
InjectInputMediaSyncOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions) string
InjectInterruptMediaOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions)
```

要点：

- **零值 `InjectOptions{}` 与旧的三参数方法逐键等价**（记入记忆 + 不裁剪）。旧方法保留为
  零值糖（`InjectText` / `InjectInterruptText` / `InjectTextNoMemory` …），存量插件不改一行、
  不需重编即可继续调用。
- **裁剪（`prune`）必须显式声明**：它会归档丢弃低相关事件，是有副作用的行为，故默认关闭。
  内核只放行 `""` / `none` / `prune`（`ValidContextPolicy`），未声明的取值会被拒。
- 裁剪前先经该插件注册的 **`Cleaner`**（由 `CleanerName` 指定）拿到实际有效内容，
  避开「按原文裁剪、按清洗后计算」这种不一致。
- `ChannelDef` 也有同名 `context_policy`（并且 1.2.0 给它补上了 JSON tag——通道定义要跨进程
  传给内核，而 `Cleaner` 是函数必须忽略；无 tag 时新增字段会被静默丢掉）。

## 阶段钩子：触发时机与陷阱

七个阶段，按一轮任务的真实时序：

```
on_input → pre_action → [ stepLLM 调模型 ] → post_action
                ↓
        工具循环: before_toolcall → [ 执行工具 ] → after_toolcall
                ↓                                      ↓
        （还有工具则回到 stepLLM）                  （否则）
                ↓
        before_output → [ 发送 ] → after_output
```

| 阶段 | 时机 | 典型用途 |
|---|---|---|
| `on_input` | 输入事件刚进内核 | 权限上下文绑定、改写用户输入 |
| `pre_action` | **即将调 LLM** | 标记"接下来要等模型" |
| `post_action` | LLM 已返回、工具未执行 | 审阅/改写模型的 tool_calls |
| `before_toolcall` | 即将执行工具 | 权限门、参数校验、拦截 |
| `after_toolcall` | 工具已返回 | 清洗结果、度量 |
| `before_output` | 即将发送/输出 | 改写文本、发"正在输入"指示 |
| `after_output` | 已发送/一轮结束 | 收尾、状态归位 |

### ★★ 每个阶段只能注册一个 handler

插件侧把 handler 存在 `map[string]StageHandler`（**阶段名为键**）⇒
**同一阶段注册两次，后者静默覆盖前者**。

```go
s.RegisterStage(sdk.StageBeforeToolcall, p.permissionGate)  // ← 被下一条覆盖
s.RegisterStage(sdk.StageBeforeToolcall, p.updateStatus)    // ← 实际生效的
```

后果不只是功能不生效 —— 如果被覆盖的是**权限门**，等于权限被静默关掉。
而内核日志会显示 `2 handlers [qq qq]`，看起来完全正常。

**正确做法：把多个功能合并进同一个 handler。** 每次阶段往返都有跨进程开销，
合并写法本身也是性能要求。hmapdev 在**构建期**对重复注册告警：

```
⚠️  警告: stage StageBeforeToolcall 注册了 2 次
    插件侧 stageHandlers 是 map（阶段名为键）⇒ 只有最后一次注册生效
```

### ★★ `before_output` / `after_output` 有**两条触发路径**

它们的触发源不止"一轮结束"，还有"单次 output_send__调用"：

| 来源 | Extra 内容 | 含义 |
|---|---|---|
| `emitResponse` | `input_source`、本轮 payload 透传字段 | **一轮的回复**（turn 边界） |
| `output_send__<ch>` | `output_channel` / `output_meta` / `output_type` | **单次发送** |

插件必须区分，否则会错：

```go
// 判断是否"单次发送"
_, isSingleSend := ctx.Extra["output_meta"]
```

- **"正在输入"类指示器**：应挂在单次发送上（消息发出前开始、发出后结束）。
  挂在轮次边界会漏 —— 一轮可能发多条消息。
- **状态归位（回到"空闲"）**：**只能**由轮次边界触发。每条发送都归位的话，
  状态会在干活期间反复跳回空闲。

## SDK API 接口

### Plugin 接口

插件需实现 `Plugin` 接口：

```go
type Plugin interface {
    Name() string
    Start(sdk *PluginSDK) error
    Stop() error
}
```

### PluginSDK 方法

通过 `Start(sdk *PluginSDK)` 注入的 SDK 实例提供以下方法：

> **通道的方向契约**：入站与出站是分开登记的两件事。凡是用 `InjectText*/InjectInput*/InjectInterrupt*`
> 注入的通道名都要 `RegisterInputChannel` —— inputch 是内核最基本的**输入路由单位**，
> 只有登记过的通道才能被"划给驻留子"；只登记出站通道时内核会兜底登记同名 inputch 并告警（兼容老插件）。

| 分类 | 方法 | 说明 |
|------|------|------|
| 阶段钩子 | `RegisterStage(stage, handler, scope...)` | 注册阶段回调，scope 可选：`StageScopeGlobal`（全局，默认）或 `StageScopeOwnTools`（仅自己工具） |
| 输入通道 | `RegisterInputChannel(name, def)` | 注册输入通道（**入站**：谁会往这个通道注入输入），def 为 `ChannelDef`（NoMemory/Cleaner） |
| 输出通道 | `RegisterOutputChannel(name, caps, desc, def, handler)` | 注册输出通道（**出站**：`output_send__<name>` 的回复发给谁），def 为 `ChannelDef`，caps 为能力位掩码。⚠️ 通道名只能用 `[A-Za-z0-9_-]`（见下方"输出通道"一节的命名约束） |
| 工具注册 | `RegisterTool(name, def, handler)` | 注册工具供 LLM 调用 |
| 插件 API | `RegisterPluginAPI(name)` | 注册插件 API 供其他插件访问 |
| 图记忆 | `Memory()` | 访问图记忆 API（实体-关系存储） |
| 文本记忆 | `TextMemory()` | 访问文本记忆 API（时序事件） |
| 文档记忆 | `DocMemory()` | 访问文档记忆 API（向量存储） |
| 社交图谱 | `Social()` | 访问社交图谱 API（外部插件只读） |
| 知识库 | `Knowledge()` | 访问知识库 API |
| LLM | `LLM()` | 访问 LLM 提供商管理 API |
| 设置 | `Settings()` | 访问设置 API |
| 事件 | `Events()` | 访问事件订阅器（外部插件仅订阅） |
| 注入 | `InjectText(source, channel, text)` / `InjectInterruptText(source, channel, text)` / `InjectTextNoMemory(source, channel, text)` | 向管道注入文本 |
| 多模态注入 | `InjectInputMedia(source, channel, text, blocks)` / `InjectInputMediaSync(...)` / `InjectInterruptMedia(...)` | 注入带图片/音频的输入（1.1.0 新增） |
| 自动重启 | `SetAutoRestart(enabled)` / `AutoRestart()` | 控制崩溃自动重启 |

### 阶段钩子

```go
// 全局监听所有插件的阶段事件
sdk.RegisterStage(StagePreAction, func(ctx *StageContext) error { return nil })

// 仅监听自己注册的工具的 before_toolcall / after_toolcall
sdk.RegisterStage(StageBeforeToolcall, myHandler, StageScopeOwnTools)
```

### ChannelDef

```go
type ChannelDef struct {
    NoMemory bool              // 通道输入/输出不参与记忆计算（向量/关键词/蒸馏），原文保留
    Cleaner  func(string) string // 可选：计算层过滤函数（不改原文）
}
```

`ChannelDef` 控制通道在记忆计算层的行为，与 `ToolDef` 的 `NoMemory`/`Cleaner` 语义一致。

### 输入通道

```go
sdk.RegisterInputChannel("qq", ChannelDef{
    NoMemory: true,
    Cleaner:  func(text string) string { return strings.TrimSpace(text) },
})
```

### 输出通道

> ⚠️ **命名约束（会进 LLM 函数名）**：内核按 `output_send__<name>` 生成工具，
> 而上游对函数名的规范是 `^[a-zA-Z0-9_-]{1,64}$`。名字违规的后果不是
> "这个工具不可用"，而是**整条请求被 400 拒绝**（`Invalid 'tools[N].function.name'`），
> 网关 auto tier 全链条失败，表现成**整个 agent 不回应**。
> 所以 `name` 只能用 `[A-Za-z0-9_-]`，并留出 `output_send__`（13 字符）的余量。
> 名字若来自外部输入（设备自报 id 之类），请在插件侧派生一个合规且唯一的名字 ——
> 内核**不会**替你净化。

```go
sdk.RegisterOutputChannel("my-channel", CapText|CapFile, "通道描述", ChannelDef{}, handler)
```

handler 接收三个参数：
- `payload` (string) — 消息载荷。`type=text` 时直接填文字，`type=file/image` 时填 URL
- `meta` (string) — 可选的 JSON 路由元数据（如 `{"group_id":123,"user_id":456}`）
- `type` (string) — 载荷类型，枚举值见下

能力标志位：

| 标志 | 值 | 说明 |
|------|----|------|
| `CapText` | 1 | 纯文本输出 |
| `CapFile` | 2 | 文件输出 |
| `CapImage` | 4 | 图片输出 |
| `CapAudio` | 8 | 音频输出 |
| `CapStructured` | 16 | 结构化数据输出 |

type 枚举值：

| 值 | 说明 |
|----|------|
| `text` | 纯文本 |
| `voice` / `audio` | 语音 |
| `image` | 图片 |
| `file` | 文件 |

### IOInjector 通道路由

| 方法 | 说明 |
|------|------|
| `InjectText(source, channel, text)` | 注入文本，记入内存，路由到指定通道 |
| `InjectInterruptText(source, channel, text)` | 注入中断文本，打断当前处理，路由到指定通道 |
| `InjectTextNoMemory(source, channel, text)` | 注入文本，不记入内存，路由到指定通道 |

### 多模态注入（1.1.0 新增）

| 方法 | 说明 |
|------|------|
| `InjectInputMedia(source, channel, text, blocks)` | 注入带媒体的输入，异步 |
| `InjectInputMediaSync(source, channel, text, blocks)` | 注入带媒体的输入并同步等待回复文本 |
| `InjectInterruptMedia(source, channel, text, blocks)` | 注入带媒体的中断，可抢占当前处理 |

`blocks` 是 `[]sdk.ContentBlock`，与 `SetToolBlocks` 用同一类型：

```go
s.InjectInputMedia("myplugin", "webui", "帮我看看这张图", []sdk.ContentBlock{{
    Type:     "image_url",
    ImageURL: &sdk.ImageURL{URL: "data:image/png;base64," + b64, Detail: "auto"},
}})
```

与 `SetToolBlocks` 的区别：`SetToolBlocks` 只能在工具处理函数内部调用，媒体要等到
下一条 tool message 才到模型手上；这三个方法是插件**主动发起一轮带媒体的对话**，
媒体在本轮就随消息发给模型，并自动落进媒体存储、挂上媒体记忆引用。

媒体块里的 `data:` URL 会被内核落盘去重；`http(s)` URL 只透传给模型，不入库
（入库需要内核发起网络请求，涉及超时、鉴权与 SSRF）。

`source` 标识来源，`channel` 指定目标输出通道。

### Triple 扩展字段

Triple 数据结构新增字段：

- `Confidence` — 置信度（0.0~1.0）
- `SubjectType` — 主体类型
- `ObjectType` — 客体类型
- `SentenceText` — 原始句子文本（1.1.0 新增），写入 `sentences` 表；媒体引用挂在句子上
- `MediaDigests` — 关联的媒体 digest 列表（1.1.0 新增）

### 记忆里的媒体（1.1.0 新增）

媒体在纯文本记忆里以**标记**形式存在，格式 `[<mime> <短digest>] <描述>`：

```
[image/png a1b2c3d4e5f6] 一张紫蓝红三色带图
```

描述文本是持久的语义记忆（检索靠它），digest 是回到字节的钥匙（反查靠它）。
标记由内核生成，插件不必自己拼——**填 digest 就够**。

#### 图记忆

```go
s.Memory().Commit([]sdk.Triple{{
    Subject: "配色图", Relation: "包含", Object: "三色带",
    MediaDigests: []string{"a1b2c3d4e5f6"}, // 短 digest 即可，内核补全
}})
```

没给 `SentenceText` 时内核会用标记本身充当句子——媒体必须有句子落点，
否则引用无从挂起。

#### 知识库

```go
s.DocMemory().InsertWithMedia(&sdk.Doc{
    Title:   "带图笔记",
    Content: "正文",
}, []sdk.MediaAttachment{
    {MIME: "image/png", Data: pngBytes, Name: "chart.png"}, // 新内容，落盘去重
    {Digest: "a1b2c3d4e5f6"},                               // 引用已有内容
})
```

`Insert` 保持原签名不变，正文里已有的标记同样会被挂成文档级引用。
`Query` 返回的 `Doc` 带 `MediaDigests` 与 `Attachments`（mime + 描述，
**不含字节**——一次检索可能命中几十份媒体）。删除文档时引用自动释放。

#### 文本记忆

```go
s.TextMemory().Append(sdk.TextEvent{
    Role: "user", Content: "看这张图",
    Attachments: []sdk.MediaAttachment{{MIME: "image/png", Data: pngBytes}},
})
```

`RecentEvents` 读回时正文里的标记会被反解成 `Attachments`。

媒体存储可在内核侧关闭（`core.memory.media.enabled=false`），此时以上接口
全部退化为纯文本行为：不报错、不 panic，与本特性上线前一致。

### ToolDef 字段说明

`RegisterTool` 的 `def` 参数类型为 `sdk.ToolDef`，包含以下字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `Name` | `string` | 工具名，建议插件名前缀避免冲突 |
| `Description` | `string` | 工具描述，LLM 据此选择调用 |
| `Parameters` | `map[string]interface{}` | JSON Schema 格式参数定义 |
| `NoMemory` | `bool` | 默认为 `false`；设为 `true` 时输出不参与向量/jieba/蒸馏计算（原文保留） |
| `Cleaner` | `func(string) string` | 可选，输出进入计算层前的清洗函数（如 JSON 提取 `.content`） |

`NoMemory` 和 `Cleaner` 的详细设计意图参见核心仓 `docs/zh/PLUGIN_DEV.md`。

### New 构造函数

`New()` 由内核在加载插件时调用，插件开发者无需手动构造 PluginSDK：

```go
func New(name string, sett SettingsAPI, regTool ToolRegistrar, regStage StageRegistrar, regAPI APIRegistrar, regOutput OutputChannelRegistrar) *PluginSDK
```

插件开发者只需实现 `Plugin` 接口并导出 `NewPluginFactory()` 入口函数。

## hmapdev 工具链

`hmapdev` 提供插件开发全流程支持，最终产出 `.hmap` 插件包（工具名即来自该包格式）。
预编译二进制作为 **release 附件**分发（linux/darwin/windows × amd64/arm64），
下载后加入 PATH 即可：

> ⚠️ **二进制的实际分发地址目前是 gitcode**（两个仓的 release 附件不同步）：
> `hmapdev_linux_amd64` 等 5 个平台二进制 + `SHA256SUMS` 在
> <https://github.com/JianFeeeee/homeagentsdk/releases>。
> GitHub 侧（<https://github.com/JianFeeeee/homeagentsdk/releases>）从
> 下一个 SDK 版本（v1.4.0）起才会同步发布 —— 因为 SDK 仓**移仓后还没发过版**。
> 源码与文档一律以 GitHub 为准，**只有二进制暂时还得到 gitcode 取**。

> 改名说明：工具链原名 `plugindev`，自 1.2.0 起更名 `hmapdev`。
> SDK 存储目录同时由 `~/.homeagent/plugindev/sdk` 迁到 `~/.homeagent/hmapdev/sdk`
> （旧目录会被自动沿用，不会丢已装版本）。

```bash
# 从 release 附件下载（以 linux amd64 为例，<版本> 如 v1.3.0）
# 当前实际分发地址是 gitcode（见下方说明）：
curl -Lo hmapdev https://github.com/JianFeeeee/homeagentsdk/releases/download/<版本>/hmapdev_linux_amd64
chmod +x hmapdev

# 或从源码自己编
cd tools/hmapdev && go build -o hmapdev .
```

> 二进制不再随仓库分发（旧的 `bin/` 目录已停用）：5 个平台各 26-28MB，
> 每次重编都在 git 历史里再叠一份，而它们本质是可从源码复现的产物。

| 命令 | 说明 |
|------|------|
| `hmapdev init <name> [--lua]` | 初始化插件项目（生成 plg.json、plugin.go 或 main.lua、go.mod、README.md） |
| `hmapdev build [flags]` | 编译并打包为 `.hmap` 包（支持跨平台编译和 bundle 模式） |
| `hmapdev clean` | 清理 `build/`、`dist/` 目录及生成文件（plugin.json、z_bridge_gen.go） |
| `hmapdev debug [dir]` | 通过 Yaegi Go 解释器加载插件源码，启动交互式 REPL 调试 |
| `hmapdev sdk <command>` | SDK 版本管理（子命令：list/install/use/path/current/latest） |

支持 **Go** 和 **Lua** 两种插件语言。

### build 命令 flags

| Flag | 说明 |
|------|------|
| `--outdir <dir>` | 输出目录（默认 `dist`，可覆盖 plg.json 中的 `outdir`） |
| `--target <os/arch>` | 构建目标（如 `linux/amd64`），可重复指定（追加到 plg.json 中的 targets） |
| `--bundle` | 强制 bundle 模式（同时编译 linux/amd64, darwin/amd64, windows/amd64） |
| `--no-bundle` | 关闭 bundle 模式，仅按 targets 逐个编译 |
| `--sdk-path <path>` | 指定 SDK 源码路径（覆盖 plg.json 中的 `sdk_path`） |
| `--replace <from=to>` / `-R` | Go 模块替换（追加到 plg.json 中的 replaces），`from` 为模块路径，`to` 为本地路径 |

### plg.json 清单格式

```json
{
  "name": "weather",
  "name_zh": "天气查询",
  "name_en": "Weather",
  "version": "1.0.0",
  "description": "天气查询插件",
  "author": "HomeAgent",
  "entry": "plugin.bin",
  "tags": ["weather", "forecast"],
  "targets": "linux/amd64,windows/amd64",
  "outdir": "dist",
  "bundle": true,
  "replaces": {
    "github.com/example/pkg": "../local/pkg"
  },
  "source_dirs": [
    "../shared-lib"
  ]
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | 插件标识名 |
| `name_zh` | string | 中文名 |
| `name_en` | string | 英文名 |
| `version` | string | 版本号 |
| `description` | string | 插件描述 |
| `author` | string | 作者 |
| `entry` | string | 入口文件（`plugin.bin` / `main.lua`）。v1.0.0 起 Go 插件统一为 `plugin.bin`，不再区分平台后缀 |
| `tags` | string[] | 标签 |
| `targets` | string | 构建目标，逗号分隔（如 `linux/amd64,windows/amd64`，Lua 插件为 `lua`） |
| `outdir` | string | 输出目录（默认 `dist`） |
| `bundle` | bool | 是否 bundle 模式（同时编译多平台，默认 `true`） |
| `sdk_path` | string | SDK 源码路径（覆盖自动检测的 SDK 路径） |
| `go_version` | string | Go 版本（如 `1.21`，默认从 SDK 的 go.mod 读取） |
| `replaces` | object | Go 模块替换，key=模块路径，value=本地路径 |
| `source_dirs` | string[] | 额外源码搜索路径（编译时自动导入，用于引入 `thirdpart/` 外部的共享代码） |

### .hmap 包格式

`.hmap` 为 ZIP 归档，包含：

- `plugin.json` — 插件元数据
- `plugin.bin` — Go 编译产物（单平台构建）
- `plugin.bin.<goos>.<goarch>` — 多平台 bundle 模式下每平台一份，
  安装时 pluginmgr 挑当前平台那份重命名为 `plugin.bin`
- `main.lua` — Lua 插件入口（Lua 插件时）

> v1.0.0 起不再使用 `plugin.so`/`plugin.dll`/`plugin.dylib`——进程边界即 ABI 边界，
> 不存在平台特定的动态库区分。旧产物新内核不会加载，会给出明确的重编提示。

## 插件生命周期

### 入口函数

插件必须导出 `NewPluginFactory` 入口函数（Go）或 `start()` 函数（Lua）：

**Go 插件** — 实现 `Plugin` 接口并导出工厂函数：

```go
func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
    return &Plugin{name: name}, nil
}
```

该函数由内核在加载插件时调用，`name` 为插件名，`config` 为 `skill.json` 中的配置（如有）。

**Lua 插件** — 返回包含 `start(sdk)` 和 `stop()` 方法的 table：

```lua
local plugin = { name = "my-plugin" }
function plugin.start(sdk) -- 注册工具等 end
function plugin.stop() end
return plugin
```

### 启动与停止

- `Start(sdk *PluginSDK) error` — 插件启动，接收 SDK 实例
- `Stop() error` — 插件停止，释放资源
- `sdk.RegisterStopHandler(fn func())` — 注册停止清理回调。内核（内置插件）或 z_bridge（外部插件）会在调用插件 `Stop()` **之前**统一执行已注册的 handler（后注册先执行，执行后清空、幂等）。适合做持久化落盘、取消后台任务等清理：此时插件内存状态仍然新鲜，避免在 `Stop()` 阶段以陈旧状态写回导致数据复活。

### 删除清理（onRemove）

`Stop`/`RegisterStopHandler` 在插件**停止**（含重载、禁用）时执行；`RegisterOnRemoveHandler` 仅在插件被**卸载（删除）**时执行一次，重载/禁用不触发：

- `sdk.RegisterOnRemoveHandler(fn func())` — 注册删除清理回调。内核在 `RemovePlugin` 流程中、插件 `Stop()` **之后**执行（后注册先执行，执行后清空、幂等）。用于删除插件自身创建的持久化文件（数据/缓存/状态文件）。
- 内核卸载时一并清理：工具注册、`disabled_plugins` 记录、插件配置项定义（`plugin.<name>.*`）与插件配置表（`config_<name>`），卸载后插件配置区完全消失。
- 示例：`example/calendar`（删 events.json）、`example/memo`（删 memos.json）、`example/rss`（删订阅数据目录）、`example/weather`（删缓存目录）；`hmapdev` 模板含 onRemove 演示。

```go
sdk.RegisterOnRemoveHandler(func() {
    os.Remove(filepath.Join(dataDir, "events.json"))
})
```

### 自动重启

```go
sdk.SetAutoRestart(true)
// 查询状态
enabled := sdk.AutoRestart()
```

插件崩溃时平台自动拉起，保障服务可用性。

重启是**有节制的**，默认参数（内核 `internal/plugin/registry.go`）：

| 参数 | 值 | 含义 |
|---|---|---|
| `procRestartBackoff` | `1s` | 第 n 次重启前等 `n × 1s`（线性退避，非立即拉起） |
| `procMaxRestarts` | `3` | 窗口内允许的重启次数上限 |
| `procCrashWindow` | `5min` | 窗口内无新崩溃则计数归零 |

即崩溃后的实际序列是 **1s → 2s → 3s**；同一 5 分钟窗口内第 **4** 次崩溃
（`n > 3`）**不再自动拉起**，交人工介入。这不是「立即无感恢复」——
如果插件需要秒级就位，请自己在 `OnStart` 里做好重连与重建。

> ⚠️ `SetAutoRestart` 的典型用法是「外部连接建好后再判定能否自动重启」，而连接建立
> 通常在后台 goroutine 里，内核又在另一个 goroutine 读它——这对读写天然并发。
> **SDK 1.1.0 已给这个标志与全部 API 字段加锁**（`-race` 实测 11 处竞态，
> 生产表现是插件重载瞬间偶发 nil 解引用崩溃）。早于 1.1.0 的版本建议升级。

## 插件开发者的并发约定

`PluginSDK` 是**被多个 goroutine 同时使用的共享对象**：你在 `Start()` 里起的轮询、
监听、定时器都拿着同一份 `*PluginSDK` 往里注消息，而内核会在加载/重载时写它的
 API 字段。因此：

- **SDK 侧已保证的**：全部 API 访问器（`Memory()`/`DocMemory()`/…）、全部注入方法、
  `SetAutoRestart`/`AutoRestart`、`RegisterTool`/`RegisterStage`、
  `RunStopHandlers`/`RunOnRemoveHandlers`（幂等，并发调也只执行一次）。
- **你需要自己保证的**：`StageContext` 的字段全部导出，并发读写必须自己持
  `ctx.Lock()`/`ctx.RLock()`。尤其是 `ctx.Extra`——**map 的并发写在 Go 里是直接 fatal，
  `recover` 接不住**。

```go
ctx.Lock()
ctx.Extra["mykey"] = value
ctx.FinalText += "补充说明"
ctx.Unlock()
```

## 受限 SDK vs 完整 SDK

外部插件（第三方分发）使用**受限 SDK**，仅暴露安全子集：

| 受限 API | 允许操作 |
|----------|----------|
| `SocialAPI` | 只读：`GetPerson`、`GetTrait`、`GetRelations`、`GetNetwork`、`ListPersons` |
| `EventSubscriber` | 仅订阅：`Subscribe`（无 `Publish`） |

内部插件（平台内置）拥有完整 SDK 访问权限，包括 SocialAPI 写操作和 EventPublisher。

## 项目声明 SDK 版本（plg.json 的 `sdk` 字段）

`hmapdev init` 生成的工程里，`plg.json` 会带一个 `sdk` 字段：

```json
{
  "name": "MyPlugin",
  "version": "0.1.0",
  "entry": "plugin.bin",
  "sdk": "1.2.0"
}
```

它的语义是**本插件针对的 SDK 版本**，工具链据此在本地 SDK 存储里选择版本：
命中就用它，并把 `go.mod` 的 `require`/`replace` 同步到该版本；未命中则**明确报错**
（列出已装版本 + `hmapdev sdk install vX.Y.Z`），**绝不静默退化成 `current`**。

```bash
$ hmapdev build
[hmapdev] SDK 1.2.0（项目声明 sdk=1.2.0）
```

为什么要这个字段：以前项目里没有任何「我要哪版 SDK」的声明，工具链只能用存储里的
`current`——谁改过 `current` 就拿谁的版本编，出错时表现为一堆看不懂的编译错误
（例如存储里只有陈旧的 `v0.8.0` 时，模板项目首次构建会报 `undefined: sdk.InjectOptions`）。

**写法必须是完整版本号（`1.2.0`），不接受区间写法（`1.2`）。** 原因见上文的版本纪律：
SDK 版本跟随内核中版本、patch 位恒为 `.0`，一条内核线只对应一个 SDK 版本；
写区间会让人误以为同一条线里还能挑不同 SDK（工具链会直接拒绝并说明这条规矩）。

- 显式 `--sdk-path` 或 `plg.json` 的 `sdk_path` 优先（本机改 SDK 联调时用）；
- 存量工程（`plg.json` 没有 `sdk` 字段）行为不变，仍按 `current` 构建；
- 产物 `.hmap` 里的 `plugin.json` 会记录**实际选中的 SDK 版本**，便于事后追溯。

## IDE 支持：VSCode 扩展（`tools/vscode-hmapdev`）

调试插件的实操回路是「构建 → 运行 → 看内核日志」，这三步都在 IDE 之外很别扭，
所以仓库里带了一个 VSCode 扩展（[tools/vscode-hmapdev](tools/vscode-hmapdev)）：

- **plg.json 诊断**：必需字段、`sdk` 是否是完整版本号、声明的 SDK 是否已安装（直接给安装命令）；
- **状态栏**：`插件 · SDK <声明> · hmapdev <版本>`，工具链缺失或工程有错时变色；
- **命令 / 任务**：build / build（全部目标）/ clean / debug（解释执行），编译错误进 Problems；
- **跟随内核日志**：读 `<dataDir>/log` 下最新的 `homed_*.log` 并按插件名过滤。

```bash
cd tools/vscode-hmapdev && npm install && npm run compile   # 然后在 VSCode 里按 F5
```

它不是源码级调试器（没有断点/单步）：插件要么编译成产物在内核里跑、要么用
`hmapdev debug` 解释执行，两条路都没有 DAP 会话；扩展做的是构建、运行、看日志与清单校验。

## 示例插件

| 插件 | 类型 | 说明 |
|------|------|------|
| [weather](example/weather) | Go | 天气查询（wttr.in），演示 NoMemory/Cleaner/阶段钩子/通道/文本记忆 |
| [luademo](example/luademo) | Lua | Lua 全功能示例，覆盖 v0.8.0 Lua SDK 全部 API 面 |
| [qq](example/qq) | Go | QQ 消息集成（NapCat），17 个工具，输入/输出通道完整对接 |
| [a2a](example/a2a) | Go | Agent-to-Agent 协议通信 |
| [ai_image](example/ai_image) | Go | AI 图片生成 |
| [bili](example/bili) | Go | Bilibili 视频下载 |
| [browser](example/browser) | Go | 网络搜索、网页抓取、浏览器渲染 |
| [calendar](example/calendar) | Go | 日历管理 |
| [editdoc](example/editdoc) | Go | 文档编辑 |
| [files](example/files) | Go | 文件管理 |
| [memo](example/memo) | Go | 备忘录（PreAction 注入 + 定时提醒） |
| [music](example/music) | Go | 音乐播放 |
| [ocr](example/ocr) | Go | 光学字符识别 |
| [rss](example/rss) | Go | RSS 订阅 |
| [sanitizer](example/sanitizer) | Go | 内容清洗/安全过滤 |

**发版时附带预编译示例产物**：SDK 的 release 除 5 平台 `hmapdev` 外，还包含各示例插件的
`.hmap` 与 `SHA256SUMS`/`MANIFEST.txt`。原因是插件二进制与内核在**布局上绑定**（主版本号
+ 统一共享区魔数 + `field_count`），只发工具链不发示例产物，很容易拿旧产物去装而握手失败
——那看起来像「插件坏了」而不是「版本不配套」。

## Remote Device SDK

用于开发**远程设备接入适配器**的 C 语言 SDK，零外部依赖，兼容嵌入式平台。

### 架构

```
┌─────────────────────────────────────────────────┐
│            ha_remotedevice (C SDK)              │
│  协议引擎  │  WS 帧  │  JSON  │  状态机  │ 传输抽象  │
└──────────┬──────────────────────────────────────┘
           │  同一份 C 代码，设备端和 App 端共用
    ┌──────┴──────────────────┐
    ▼                         ▼
┌──────────────┐    ┌──────────────────────────┐
│  ESP32 裸机   │    │  Linux 设备上的 App        │
│  纯 C 直调     │    │  (Python ctypes / Go CGo / │
│  简单命令处理   │    │   Node addon / C# P/Invoke) │
└──────────────┘    └──────────────────────────┘
```

### 声明式 API 设计

设备在代码中声明**自己是什么**、**能做什么**、**支持哪些命令**，每个命令对应独立处理函数，SDK 自动分发并回执结果：

```c
#include "ha_remotedevice.h"

/* 声明能力 */
const char *caps[] = {"camera", "status", NULL};

/* 声明式命令处理表：每个命令绑定独立处理函数 */
static ha_status_t handle_camerasue(const char *req_id, const char *args,
                                    ha_cmd_result_t *result, void *userdata) {
    (void)req_id; (void)userdata;
    int duration = args[0] ? atoi(args) : 0;
    // 拍照/录像...
    result->status = 0;
    result->output = "data:image/jpeg;base64,...";  // SDK 自动回执
    return HA_OK;
}

ha_cmd_handler_def_t handlers[] = {
    {.command = "shell",      .handler = handle_shell},
    {.command = "camerasue",  .handler = handle_camerasue},
    {.command = "screensee",  .handler = handle_screensee},
    {.command = "speakeruse", .handler = handle_speakeruse},
    {.command = NULL},  /* 标记结束 */
};

ha_config_t config = {
    .transport = my_transport,     // 用户实现 4 个函数
    .server    = "192.168.1.100:9890",
    .token     = "my-token",
    .device = {
        .device_id = "esp32-cam-1",
        .name      = "门口摄像头",
        .kind      = "camera",
        .caps      = caps,
    },
    .handlers  = handlers,   // 声明式命令处理表
    .on_state  = my_state_handler,
};

ha_client_t *client = ha_client_new(&config);
ha_client_start(client);
while (1) {
    ha_client_process(client);     // 主循环处理
}
```

### 传输层抽象

用户只需实现 4 个函数，适配不同平台：

```c
ha_transport_t my_transport = {
    .connect = my_tcp_connect,   // 建立 TCP 连接
    .send    = my_tcp_send,      // 发送数据
    .recv    = my_tcp_recv,      // 接收数据（阻塞）
    .close   = my_tcp_close,     // 关闭连接
    .ctx     = &my_platform_ctx,
};
```

### 支持的协议

| 功能 | API |
|------|-----|
| WS 连接 + 握手 | `ha_client_start` 自动完成 |
| 设备注册 (hello/bind) | 启动时自动发送 |
| 命令接收 (shell/homeagent) | `handlers` 表声明式注册，SDK 自动分发 |
| 命令回执 | `ha_client_send_result` |
| 二进制分块（录像等） | `ha_client_send_data_chunked` |
| TTS 音频接收 | `on_binary` 回调 |
| 事件上报 | `ha_client_send_event` |
| 状态上报 | `ha_client_send_status` |
| 心跳保持 | 自动 ping/pong |

### 使用方式

通过 `hmapdev` 工具链初始化项目：

```bash
hmapdev init my-adapter --type remotedevice
```

生成 `main.c` + `CMakeLists.txt`，可直接编译或作为三方库引入：

```cmake
add_subdirectory(path/to/ha_remotedevice)
target_link_libraries(my_app ha_remotedevice)
target_include_directories(my_app PRIVATE ${HA_REMOTEDEVICE_INCLUDE_DIR})
```

### 快速接入指南

以下是从零到设备成功接入 HomeAgent 的完整步骤。

#### 1. 准备工作

在 HomeAgent 平台上创建接入令牌：

```bash
# 在 HomeAgent 服务端创建一个设备接入令牌
curl -X POST http://<homeagent-server>:8080/api/v1/device/token \
  -H "Content-Type: application/json" \
  -d '{"device_id":"esp32-cam-1","name":"门口摄像头","kind":"camera"}'
# 返回: {"token":"ha-dev-token-xxxxx"}
```

记录下返回的 `token`，设备端配置时使用。

#### 2. 实现传输层（4 个函数）

根据你的平台实现 `ha_transport_t` 的 4 个函数指针。以下是几种常见场景：

**场景 A：带 TCP/IP 栈的嵌入式设备（如 ESP32 + lwIP）**

```c
#include "ha_remotedevice.h"
#include "lwip/sockets.h"

static int esp_connect(void *ctx, const char *host, uint16_t port) {
    struct sockaddr_in addr;
    int sock = socket(AF_INET, SOCK_STREAM, 0);
    if (sock < 0) return -1;
    addr.sin_family = AF_INET;
    addr.sin_port = htons(port);
    inet_pton(AF_INET, host, &addr.sin_addr);
    int ret = connect(sock, (struct sockaddr *)&addr, sizeof(addr));
    if (ret < 0) { closesocket(sock); return -1; }
    *(int *)ctx = sock;
    return 0;
}

static int esp_send(void *ctx, const uint8_t *data, int len) {
    int sock = *(int *)ctx;
    return send(sock, (const char *)data, len, 0);
}

static int esp_recv(void *ctx, uint8_t *buf, int len) {
    int sock = *(int *)ctx;
    return recv(sock, (char *)buf, len, 0);
}

static void esp_close(void *ctx) {
    int sock = *(int *)ctx;
    closesocket(sock);
}

int esp_ctx = -1;
ha_transport_t transport = {
    .connect = esp_connect,
    .send    = esp_send,
    .recv    = esp_recv,
    .close   = esp_close,
    .ctx     = &esp_ctx,
};
```

**场景 B：通过串口（UART）连接透传模块**

```c
static int uart_connect(void *ctx, const char *host, uint16_t port) {
    (void)host; (void)port;
    // 初始化 UART，波特率 115200
    return uart_init((uart_ctx_t *)ctx, 115200);
}

static int uart_send(void *ctx, const uint8_t *data, int len) {
    return uart_write((uart_ctx_t *)ctx, data, len);
}

static int uart_recv(void *ctx, uint8_t *buf, int len) {
    return uart_read((uart_ctx_t *)ctx, buf, len);
}

static void uart_close(void *ctx) {
    uart_deinit((uart_ctx_t *)ctx);
}
```

> 注意：UART 透传时，另一端需运行一个 TCP 桥接程序，将串口数据转发到 HomeAgent 的 WebSocket 端口。

#### 3. 声明设备能力和命令处理

```c
#include "ha_remotedevice.h"

/* 声明设备能力 */
const char *caps[] = {"camera", "speaker", "status", NULL};

/* 处理 camerasue 命令（拍照） */
static ha_status_t handle_camera(const char *req_id, const char *args,
                                 ha_cmd_result_t *result, void *userdata) {
    (void)req_id; (void)userdata;
    int duration = args[0] ? atoi(args) : 0;  // 参数：录像时长

    // 拍照或录像，将结果填入 result
    result->status = 0;
    result->output = "data:image/jpeg;base64,/9j/4AAQ...";  // base64 图像数据
    return HA_OK;
}

/* 处理 shell 命令 */
static ha_status_t handle_shell(const char *req_id, const char *args,
                                ha_cmd_result_t *result, void *userdata) {
    (void)req_id; (void)userdata;
    // 执行 shell 命令，args 为完整命令字符串
    result->status = 0;
    result->output = "command executed";
    return HA_OK;
}

/* 声明式命令处理表 */
ha_cmd_handler_def_t handlers[] = {
    {.command = "shell",      .handler = handle_shell},
    {.command = "camerasue",  .handler = handle_camera},
    {.command = "screensee",  .handler = handle_camera},
    {.command = "speakeruse", .handler = handle_speaker},
    {.command = NULL},  /* 标记结束 */
};
```

#### 4. 配置并启动客户端

```c
ha_config_t config = {
    .transport = transport,                 // 传输层实现
    .server    = "192.168.1.100:9890",      // HomeAgent 服务端地址
    .token     = "ha-dev-token-xxxxx",      // 第 1 步获取的令牌
    .device = {
        .device_id = "esp32-cam-1",
        .name      = "门口摄像头",
        .kind      = "camera",
        .caps      = caps,
        .info_json = "{\"chip\":\"ESP32-S3\",\"firmware\":\"v1.0\"}",
    },
    .handlers  = handlers,                  // 命令处理表
    .on_binary = on_binary_data,            // 接收 TTS 音频等二进制数据
    .on_state  = on_state_change,           // 连接状态变化回调
    .ping_interval = 30,                    // 心跳间隔秒数
};

ha_client_t *client = ha_client_new(&config);
ha_status_t ret = ha_client_start(client);
if (ret != HA_OK) {
    printf("设备接入失败: %d\n", ret);
    return;
}

/* 主循环 */
while (1) {
    ha_client_process(client);  // 处理协议帧、心跳、命令分发

    /* 可选：设备主动上报事件 */
    ha_client_send_event(client, "motion_detected",
                         "{\"zone\":\"front_door\",\"confidence\":0.95}");

    /* 可选：上报设备状态 */
    ha_client_send_status(client, "online");

    vTaskDelay(100 / portTICK_PERIOD_MS);  // 嵌入式 RTOS 风格延时
}
```

#### 5. 验证连接

在 HomeAgent 服务端检查设备是否在线：

```bash
# 查看已注册设备列表
curl http://<homeagent-server>:8080/api/v1/device/list
# 预期输出包含: {"device_id":"esp32-cam-1","status":"online",...}

# 向设备发送命令（测试 camerasue）
curl -X POST http://<homeagent-server>:8080/api/v1/device/esp32-cam-1/cmd \
  -H "Content-Type: application/json" \
  -d '{"cmd":"camerasue","args":"3"}'
# 预期返回: {"status":"ok","result":"data:image/jpeg;base64,..."}
```

#### 6. 调试技巧

| 问题 | 检查点 |
|------|--------|
| 连接失败 | 确认 `server` 地址和端口可通；检查 `token` 是否正确 |
| WS 握手失败 | 确认 HomeAgent 服务端已开启 WebSocket 支持 |
| 命令无响应 | 确认 `handlers` 表中注册了对应命令名；检查 `on_binary` 是否配置 |
| 断线重连 | `max_reconnect` 控制重连次数，-1 为无限重连 |
| 内存不足（嵌入式） | 定义 `HA_NO_ALLOC` 宏禁用动态内存分配 |

### 位置

- **SDK 源码**: `remotedevice/`
- **hmapdev 模板**: `hmapdev init --type remotedevice`

## 构建与安装

### 构建

```bash
hmapdev build
```

输出 `.hmap` 包到 `dist/` 目录（默认 bundle 多平台合集；单平台构建使用 `hmapdev build --no-bundle`）。

### 安装

通过 pluginmgr HTTP API 安装（端口默认 9876，仅监听 127.0.0.1，无鉴权）：

```bash
# 本地路径
curl -X POST http://127.0.0.1:9876/plugins \
  -H "Content-Type: application/json" \
  -d '{"path": "/path/to/my-plugin.hmap"}'

# 直接上传二进制
curl -X POST http://127.0.0.1:9876/plugins \
  --data-binary @dist/my-plugin.hmap
```

或通过 WebUI 插件管理页面上传，也可手动将 `.hmap` 放入插件目录后重启平台。

## 文档

- 文档站：**<https://sdk.homeagent.jianfgit.xyz/>**
- 快速开始：[环境与工具链](https://sdk.homeagent.jianfgit.xyz/guide/getting-started/) · [第一个 Go 插件](https://sdk.homeagent.jianfgit.xyz/guide/first-plugin/) · [第一个 Lua 插件](https://sdk.homeagent.jianfgit.xyz/guide/first-lua-plugin/)
- API 参考：[总览](https://sdk.homeagent.jianfgit.xyz/api/) · [工具](https://sdk.homeagent.jianfgit.xyz/api/tools/) · [阶段钩子](https://sdk.homeagent.jianfgit.xyz/api/stages/) · [记忆](https://sdk.homeagent.jianfgit.xyz/api/memory/) · [输入/输出通道](https://sdk.homeagent.jianfgit.xyz/api/channels/)
- 指南：[能力边界（哪些 API 外部可用）](https://sdk.homeagent.jianfgit.xyz/guide/capability-boundary/) · [工具并发声明](https://sdk.homeagent.jianfgit.xyz/guide/parallel-tool-declaration/) · [流式多 tool_call](https://sdk.homeagent.jianfgit.xyz/guide/stream-tool-call-index/) · [场景记忆](https://sdk.homeagent.jianfgit.xyz/guide/scene-memory/) · [打包与发布](https://sdk.homeagent.jianfgit.xyz/guide/packaging/)
- 版本与兼容：<https://sdk.homeagent.jianfgit.xyz/versions/>
- 核心仓（内核 / 记忆 / 调度）：<https://github.com/JianFeeeee/HomeAgent>
- 介绍站：<https://introduce.homeagent.jianfgit.xyz/>

> 文档站为自托管（国内直连，不必挂代理）。构建与部署见核心仓的
> `deploy-sdk-site.sh`（`--check` 只比对、`--rollback` 回滚）。

## 许可

SDK 以 **MIT** 发布，全文见 [LICENSE](LICENSE)。

**这是刻意的宽松**：SDK 会随插件一起**静态链接**（其源码进入插件二进制），
若用 AGPL 之类的传染许可，插件作者就会被强制以其对外开源。选 MIT 就是为了
让插件作者**自由选择自己的许可**——闭源、商业、私有均可，无需向本项目回馈，
也无需取得任何例外或商业授权。第三方插件生态的安全与活跃正建立在这条之上。

前提是 SDK 本身**完全自包含**：`go.mod` 零外部依赖，`sdk/` 只依赖 Go 标准库
（`sync`），不引用核心仓的任何代码，因此 MIT 授权不与其他许可冲突。

第三方组件（Go 依赖：go-sqlite3、gojieba、bubbletea 等，均为 MIT / BSD-3 / Apache-2.0）
保持各自原有许可。平台侧的模型与推理运行时（Chinese-CLIP Apache-2.0、ONNX Runtime MIT）
不属于本 SDK，其许可全文随发行包放在 `/usr/share/doc/homeagent/licenses/`。
