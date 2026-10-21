<!-- 本页由 tools/apidoc/gensite 从源码生成，请勿手改；要改文档就改 sdk/*.go 的注释。 -->

# 输入 / 输出通道

通道是插件与外界（设备、其他 Agent、外部系统）交换消息的入口。**输入通道**接收外部消息，**输出通道**把消息投递出去。

## `IOInjector`

IOInjector provides methods for injecting input and interrupts into the agent pipeline.
All methods accept (source, channel) where channel is the target output channel
for routing the agent's response.

| 方法 | 说明 |
|---|---|
| [`InjectInputMedia`](#ioinjectorinjectinputmedia) |  |
| [`InjectInputMediaOpts`](#ioinjectorinjectinputmediaopts) |  |
| [`InjectInputMediaSync`](#ioinjectorinjectinputmediasync) |  |
| [`InjectInputMediaSyncOpts`](#ioinjectorinjectinputmediasyncopts) |  |
| [`InjectInputSync`](#ioinjectorinjectinputsync) | InjectInputSync 注入输入事件并同步等待 agent 回复，返回回复文本（无回复时返回空串）。 |
| [`InjectInputSyncOpts`](#ioinjectorinjectinputsyncopts) |  |
| [`InjectInterruptMedia`](#ioinjectorinjectinterruptmedia) |  |
| [`InjectInterruptMediaOpts`](#ioinjectorinjectinterruptmediaopts) |  |
| [`InjectInterruptText`](#ioinjectorinjectinterrupttext) |  |
| [`InjectInterruptTextOpts`](#ioinjectorinjectinterrupttextopts) |  |
| [`InjectText`](#ioinjectorinjecttext) |  |
| [`InjectTextNoMemory`](#ioinjectorinjecttextnomemory) |  |
| [`InjectTextOpts`](#ioinjectorinjecttextopts) | 以下 Opts 变体让调用点在**这一次注入**上声明记忆与裁剪行为。 |
| [`SetToolBlocks`](#ioinjectorsettoolblocks) | SetToolBlocks 插件工具注入多模态内容块（image_url/audio_url），内核在下一条 |

### `IOInjector.InjectInputMedia`

```go
InjectInputMedia(source, channel, text string, blocks []ContentBlock)
```

<small>`plugin.go:378`</small>

### `IOInjector.InjectInputMediaOpts`

```go
InjectInputMediaOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions)
```

<small>`plugin.go:389`</small>

### `IOInjector.InjectInputMediaSync`

```go
InjectInputMediaSync(source, channel, text string, blocks []ContentBlock) string
```

<small>`plugin.go:379`</small>

### `IOInjector.InjectInputMediaSyncOpts`

```go
InjectInputMediaSyncOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions) string
```

<small>`plugin.go:390`</small>

### `IOInjector.InjectInputSync`

```go
InjectInputSync(source, channel, text string) string
```

InjectInputSync 注入输入事件并同步等待 agent 回复，返回回复文本（无回复时返回空串）。
用于通道消息的完整闭环：收到入站 → agent 处理 → 回复取回 → 送回通道。

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`a2a`](../examples/index.md#a2a) | `example/a2a/plugin.go:345` | `reply := p.sdk.InjectInputSync(p.name, p.name,` |
| [`acp`](../examples/index.md#acp) | `example/acp/plugin.go:237` | `reply = p.sdk.InjectInputSync(p.name, p.name,` |

<small>`plugin.go:374`</small>

### `IOInjector.InjectInputSyncOpts`

```go
InjectInputSyncOpts(source, channel, text string, opts InjectOptions) string
```

<small>`plugin.go:388`</small>

### `IOInjector.InjectInterruptMedia`

```go
InjectInterruptMedia(source, channel, text string, blocks []ContentBlock)
```

<small>`plugin.go:380`</small>

### `IOInjector.InjectInterruptMediaOpts`

```go
InjectInterruptMediaOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions)
```

<small>`plugin.go:391`</small>

### `IOInjector.InjectInterruptText`

```go
InjectInterruptText(source, channel, text string)
```

<small>`plugin.go:369`</small>

### `IOInjector.InjectInterruptTextOpts`

```go
InjectInterruptTextOpts(source, channel, text string, opts InjectOptions)
```

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`browser`](../examples/index.md#browser) | `example/browser/plugin.go:1426` | `p.sdk.InjectInterruptTextOpts(p.name, p.name,` |
| [`calendar`](../examples/index.md#calendar) | `example/calendar/plugin.go:528` | `p.sdk.InjectInterruptTextOpts("calendar", "calendar", msg, sdk.InjectOptions{NoMemory: true})` |
| [`memo`](../examples/index.md#memo) | `example/memo/plugin.go:299` | `p.sdk.InjectInterruptTextOpts(p.name, p.name,` |
| [`qq`](../examples/index.md#qq) | `example/qq/plugin.go:1894` | `p.sdk.InjectInterruptTextOpts(p.name, p.name, text, sdk.InjectOptions{` |

<small>`plugin.go:387`</small>

### `IOInjector.InjectText`

```go
InjectText(source, channel, text string)
```

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`commondev`](../examples/index.md#commondev) | `example/commondev/sidecar.go:673` | `p.sdk.InjectText("commondev", inCh, text)` |

<small>`plugin.go:370`</small>

### `IOInjector.InjectTextNoMemory`

```go
InjectTextNoMemory(source, channel, text string)
```

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`browser`](../examples/index.md#browser) | `example/browser/plugin.go:1142` | `p.sdk.InjectTextNoMemory(p.name, p.name, fmt.Sprintf("[浏览器 %s 已导航到 %s]", id, rawURL))` |

<small>`plugin.go:371`</small>

### `IOInjector.InjectTextOpts`

```go
InjectTextOpts(source, channel, text string, opts InjectOptions)
```

以下 Opts 变体让调用点在**这一次注入**上声明记忆与裁剪行为。

上面那些不带 opts 的方法等价于传零值 InjectOptions（记入记忆 + 不裁剪），
保留它们是为了不破坏已有插件；新代码应当用 Opts 变体把意图写清楚。

<small>`plugin.go:386`</small>

### `IOInjector.SetToolBlocks`

```go
SetToolBlocks(blocks []ContentBlock)
```

SetToolBlocks 插件工具注入多模态内容块（image_url/audio_url），内核在下一条
tool message 的 content 数组里带上这些块，让模型在后续轮次看到图/听到音频。

<small>`plugin.go:377`</small>

### `CapAudio`

```go
const CapAudio
```

Output capability flags

<small>`plugin.go:506`</small>

### `CapFile`

```go
const CapFile
```

Output capability flags

<small>`plugin.go:504`</small>

### `CapImage`

```go
const CapImage
```

Output capability flags

<small>`plugin.go:505`</small>

### `CapStructured`

```go
const CapStructured
```

Output capability flags

<small>`plugin.go:507`</small>

### `CapText`

```go
const CapText
```

Output capability flags

<small>`plugin.go:503`</small>

### `ChannelDef`

```go
type ChannelDef struct { NoMemory bool `json:"no_memory,omitempty"` Cleaner func(string) string `json:"-"` ContextPolicy string `json: …
```

ChannelDef 描述通道在记忆计算层的行为，与 ToolDef.NoMemory/Cleaner 语义一致。
NoMemory: 此通道输入/输出不参与记忆计算（向量化/关键词提取/蒸馏），但原文保留在上下文中
Cleaner:  计算层过滤函数，不改原文；仅在向量化/jieba/蒸馏/存档提取关键词时调用
ContextPolicy: 此通道的输入到达后是否据此裁剪上下文，默认 none（不裁剪）
RecallPolicy:  此通道的输入到达后是否据此召回相关记忆，默认 auto（召回）
ScenePolicy:   此通道的输入到达后是否参与场面识别（场景式记忆），默认 auto（参与）

JSON tag 是必需的：通道定义要跨进程传给内核，而 Cleaner 是函数（必须忽略）。
没有 tag 时既无法整体 marshal（func 不支持），又会诱使调用方手写字段白名单——
那样新增字段会被静默丢掉。

<small>`plugin.go:208`</small>

### `ContextPolicyNone`

```go
const ContextPolicyNone
```

上下文策略：决定一次工具调用/输入/注入是否依据其内容裁剪上下文。

默认（空串或 ContextPolicyNone）**不裁剪**：裁剪会归档丢弃低相关事件，
必须由工具/通道/注入点显式声明才发生——否则一个只想往上下文里塞内容的
插件会在背后把别人的内容挤掉，且看不出是谁干的。

<small>`plugin.go:44`</small>

### `ContextPolicyPrune`

```go
const ContextPolicyPrune
```

上下文策略：决定一次工具调用/输入/注入是否依据其内容裁剪上下文。

默认（空串或 ContextPolicyNone）**不裁剪**：裁剪会归档丢弃低相关事件，
必须由工具/通道/注入点显式声明才发生——否则一个只想往上下文里塞内容的
插件会在背后把别人的内容挤掉，且看不出是谁干的。

<small>`plugin.go:45`</small>

### `PluginSDK.InjectInputMedia`

```go
func (s *PluginSDK) InjectInputMedia(source, channel, text string, blocks []ContentBlock)
```

InjectInputMedia 注入带媒体内容块（image_url/audio_url）的输入。
blocks 会落进媒体存储被记忆引用捕获，同时作为当前轮 content 数组
发给 LLM，让模型在「本轮」就看到图/听到音频——区别于 SetToolBlocks
的「下一轮 tool message」语义。
等价于 InjectInputMediaOpts(..., InjectOptions{})。

<small>`plugin.go:937`</small>

### `PluginSDK.InjectInputMediaOpts`

```go
func (s *PluginSDK) InjectInputMediaOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions)
```

InjectInputMediaOpts 注入带媒体块的输入，并声明记忆/裁剪行为。

<small>`plugin.go:976`</small>

### `PluginSDK.InjectInputMediaSync`

```go
func (s *PluginSDK) InjectInputMediaSync(source, channel, text string, blocks []ContentBlock) string
```

InjectInputMediaSync 注入带媒体内容块的输入并同步等待 agent 回复。
等价于 InjectInputMediaSyncOpts(..., InjectOptions{})。

<small>`plugin.go:943`</small>

### `PluginSDK.InjectInputMediaSyncOpts`

```go
func (s *PluginSDK) InjectInputMediaSyncOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions) string
```

InjectInputMediaSyncOpts 注入带媒体块的输入并同步等待回复，同时声明记忆/裁剪行为。

<small>`plugin.go:983`</small>

### `PluginSDK.InjectInputSync`

```go
func (s *PluginSDK) InjectInputSync(source, channel, text string) string
```

InjectInputSync injects a text message and synchronously waits for the agent reply,
returning the reply text (empty string if none). Replies must be dispatched back
to the source channel by the caller.

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`a2a`](../examples/index.md#a2a) | `example/a2a/plugin.go:345` | `reply := p.sdk.InjectInputSync(p.name, p.name,` |
| [`acp`](../examples/index.md#acp) | `example/acp/plugin.go:237` | `reply = p.sdk.InjectInputSync(p.name, p.name,` |

<small>`plugin.go:928`</small>

### `PluginSDK.InjectInputSyncOpts`

```go
func (s *PluginSDK) InjectInputSyncOpts(source, channel, text string, opts InjectOptions) string
```

InjectInputSyncOpts 注入输入并同步等待回复，同时在这次注入上声明记忆/裁剪行为。

<small>`plugin.go:967`</small>

### `PluginSDK.InjectInterruptMedia`

```go
func (s *PluginSDK) InjectInterruptMedia(source, channel, text string, blocks []ContentBlock)
```

InjectInterruptMedia 注入带媒体内容块的中断，可抢占当前 LLM 处理。
blocks 随中断消息一起发给模型。

<small>`plugin.go:1000`</small>

### `PluginSDK.InjectInterruptMediaOpts`

```go
func (s *PluginSDK) InjectInterruptMediaOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions)
```

InjectInterruptMediaOpts 注入带媒体块的中断，并声明记忆/裁剪行为。

<small>`plugin.go:992`</small>

### `PluginSDK.InjectInterruptText`

```go
func (s *PluginSDK) InjectInterruptText(source, channel, text string)
```

InjectInterruptText injects a text interrupt that can preempt current LLM processing.
等价于 InjectInterruptTextOpts(..., InjectOptions{})：记入记忆、不裁剪。

<small>`plugin.go:909`</small>

### `PluginSDK.InjectInterruptTextOpts`

```go
func (s *PluginSDK) InjectInterruptTextOpts(source, channel, text string, opts InjectOptions)
```

InjectInterruptTextOpts 注入可抢占当前处理的中断文本。

中断也允许声明 ContextPolicyPrune：中断同样携带内容进入上下文，
是否需要据此裁剪由调用方决定（默认不裁剪）。

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`browser`](../examples/index.md#browser) | `example/browser/plugin.go:1426` | `p.sdk.InjectInterruptTextOpts(p.name, p.name,` |
| [`calendar`](../examples/index.md#calendar) | `example/calendar/plugin.go:528` | `p.sdk.InjectInterruptTextOpts("calendar", "calendar", msg, sdk.InjectOptions{NoMemory: true})` |
| [`memo`](../examples/index.md#memo) | `example/memo/plugin.go:299` | `p.sdk.InjectInterruptTextOpts(p.name, p.name,` |
| [`qq`](../examples/index.md#qq) | `example/qq/plugin.go:1894` | `p.sdk.InjectInterruptTextOpts(p.name, p.name, text, sdk.InjectOptions{` |

<small>`plugin.go:960`</small>

### `InjectOptions`

```go
type InjectOptions struct { NoMemory bool ContextPolicy string // RecallPolicy 声明此次注入是否据其内容召回相关记忆。 // 空串 = 默认（输入/注�� …
```

InjectOptions 声明一次注入行为在记忆层与上下文层的表现。

零值 = 记入记忆 + 不裁剪上下文，与历史行为（三参数注入方法）完全一致，
因此调用方只有在确实需要改变行为时才需要填它。

为什么注入也要这两个标志：注入的内容来源千差万别——轮询到的频道消息
属于真实对话（该记），而“任务还在跑”“连接已重连”这类提醒不该污染记忆，
也不该把上下文按它的内容裁一遍。按调用点声明比按通道一刀切准确。

NoMemory:       此次注入不参与记忆计算（向量化/关键词提取/蒸馏），原文仍留在上下文
ContextPolicy:  此次注入后是否依据（清洗后的）内容裁剪上下文；默认不裁剪。
RecallPolicy:   此次注入是否依据（清洗后的）内容召回相关记忆；默认 auto（召回）。

	中断注入也允许声明 prune——它同样会携带内容进入上下文。

CleanerName:    此次注入的内容用哪个**已注册的通道 cleaner** 清洗。

	空串 = 按注入的 source 查通道定义（既有行为）。
	为什么要能显式指定：注入的 source 未必是注册过的输入通道名，
	而注入内容往往带 ANSI/JSON 包装，需要清洗后才是有效内容；
	不指定就只能退到「按 source 查不到就不清洗」。

<small>`plugin.go:132`</small>

### `PluginSDK.InjectText`

```go
func (s *PluginSDK) InjectText(source, channel, text string)
```

InjectText injects a text message into the agent pipeline.
等价于 InjectTextOpts(..., InjectOptions{})：记入记忆、不裁剪。

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`commondev`](../examples/index.md#commondev) | `example/commondev/sidecar.go:673` | `p.sdk.InjectText("commondev", inCh, text)` |

<small>`plugin.go:915`</small>

### `PluginSDK.InjectTextNoMemory`

```go
func (s *PluginSDK) InjectTextNoMemory(source, channel, text string)
```

InjectTextNoMemory injects a text message without generating memory.
等价于 InjectTextOpts(..., InjectOptions{NoMemory: true})。

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`browser`](../examples/index.md#browser) | `example/browser/plugin.go:1142` | `p.sdk.InjectTextNoMemory(p.name, p.name, fmt.Sprintf("[浏览器 %s 已导航到 %s]", id, rawURL))` |

<small>`plugin.go:921`</small>

### `PluginSDK.InjectTextOpts`

```go
func (s *PluginSDK) InjectTextOpts(source, channel, text string, opts InjectOptions)
```

InjectTextOpts 注入文本到 agent，并在这一次注入上声明记忆与裁剪行为。

<small>`plugin.go:950`</small>

### `PriorityL1`

```go
const PriorityL1
```

中断优先级取值。

L1..L3 任何插件都可声明；**L4 只有内核级插件**（编译期内置插件，
如 cli/webui/timer）才能声明——它用于实现真正的“立即打断”能力，
例如 WebUI 的终止按钮。外部插件（走 proc 桥）声明 L4 会被内核夹到 L3。

<small>`plugin.go:191`</small>

### `PriorityL2`

```go
const PriorityL2
```

中断优先级取值。

L1..L3 任何插件都可声明；**L4 只有内核级插件**（编译期内置插件，
如 cli/webui/timer）才能声明——它用于实现真正的“立即打断”能力，
例如 WebUI 的终止按钮。外部插件（走 proc 桥）声明 L4 会被内核夹到 L3。

<small>`plugin.go:192`</small>

### `PriorityL3`

```go
const PriorityL3
```

中断优先级取值。

L1..L3 任何插件都可声明；**L4 只有内核级插件**（编译期内置插件，
如 cli/webui/timer）才能声明——它用于实现真正的“立即打断”能力，
例如 WebUI 的终止按钮。外部插件（走 proc 桥）声明 L4 会被内核夹到 L3。

<small>`plugin.go:193`</small>

### `PriorityL4`

!!! warning "仅内核内置插件可用"
    sdk/plugin.go:119-126 明确：L1..L3 任何插件可声明，L4 只有内核级插件可用，外部插件声明会被内核夹到 L3（内核侧有 priority_clamp_test.go 守着）。外部插件文档应说明「声明 L4 无效」。

```go
const PriorityL4
```

PriorityL4 仅内核级（内置）插件可用；外部插件声明会被夹到 L3。

<small>`plugin.go:195`</small>

### `RecallPolicyAuto`

```go
const RecallPolicyAuto
```

召回策略：决定一次工具调用/输入/注入是否据其内容**召回**（注入）相关记忆。

与 ContextPolicy **正交**：ContextPolicy 管「裁剪」（把低相关 L0 事件归档），
RecallPolicy 管「召回」（把 L2/L3 的相关记忆注入本轮）。两者默认值刻意相反——
裁剪是破坏性的，默认关（必须显式声明）；召回是只读增量、日常对话本就需要，
默认 auto（输入/注入），仅**工具**默认 none（工具输出多为噪声，按需声明）。

<small>`plugin.go:65`</small>

### `RecallPolicyNone`

```go
const RecallPolicyNone
```

召回策略：决定一次工具调用/输入/注入是否据其内容**召回**（注入）相关记忆。

与 ContextPolicy **正交**：ContextPolicy 管「裁剪」（把低相关 L0 事件归档），
RecallPolicy 管「召回」（把 L2/L3 的相关记忆注入本轮）。两者默认值刻意相反——
裁剪是破坏性的，默认关（必须显式声明）；召回是只读增量、日常对话本就需要，
默认 auto（输入/注入），仅**工具**默认 none（工具输出多为噪声，按需声明）。

<small>`plugin.go:64`</small>

### `PluginSDK.RegisterInputChannel`

```go
func (s *PluginSDK) RegisterInputChannel(name string, def ChannelDef) error
```

RegisterInputChannel registers an input channel with its memory behavior.

契约：**凡是用 InjectText*/InjectInput*/InjectInterrupt*(source, "<name>", ...)
注入的通道名，都应当在这里登记**。inputch 是内核里最基本的**输入路由单位**：
只有登记过的通道才能在 inputch 登记表里出现，父 agent 才能"把某个 inputch 划给驻留子"；
没登记就划分会直接失败（`inputch 未注册`）。

只登记输出通道（RegisterOutputChannel）而没登记输入通道时，内核会兜底登记同名
inputch 并打告警日志 —— 兜底只为兼容老插件，新插件请显式登记。

def.NoMemory: 此通道输入不参与记忆计算
def.Cleaner:  计算层对输入文本清洗后（不改原文）再向量化/提关键词

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`a2a`](../examples/index.md#a2a) | `example/a2a/plugin.go:52` | `_ = s.RegisterInputChannel(p.name, sdk.ChannelDef{})` |
| [`acp`](../examples/index.md#acp) | `example/acp/plugin.go:52` | `_ = s.RegisterInputChannel(p.name, sdk.ChannelDef{})` |
| [`browser`](../examples/index.md#browser) | `example/browser/plugin.go:227` | `_ = s.RegisterInputChannel(p.name, sdk.ChannelDef{})` |
| [`calendar`](../examples/index.md#calendar) | `example/calendar/plugin.go:282` | `_ = s.RegisterInputChannel("calendar", sdk.ChannelDef{NoMemory: true})` |

<small>`plugin.go:754`</small>

### `PluginSDK.RegisterOutputChannel`

```go
func (s *PluginSDK) RegisterOutputChannel(name string, caps int, desc string, def ChannelDef, handler ToolHandler) error
```

RegisterOutputChannel registers an output channel that the output_send tool can route to.

与 RegisterInputChannel 的分工：本函数声明**出站**（output_send__<name> 的回复发给谁）；
入站（谁会往 <name> 注入输入）是另一件事，用 RegisterInputChannel 声明。
若该通道同时也是你的注入入口，两个都要登记。

name: channel name (e.g. "qq", "webui")。

❗**命名约束**：内核会把通道名拼进 LLM 的函数名（`output_send__<name>`），
而上游对函数名的规范是 `^[a-zA-Z0-9_-]{1,64}$`。违反的后果不是"这个工具不可用"，
而是**整条请求被上游 400 拒绝**（`Invalid 'tools[N].function.name'`），
网关的 auto tier 会全链条失败 —— 表现成"整个 agent 不说话了"。
所以通道名只能用 `[A-Za-z0-9_-]`，且总长要留出 `output_send__`（13 字符）的余量。
若通道名来自外部输入（设备自报 id 之类），请**在插件侧派生一个合规且唯一的名字**，
而不是把原始值直接当通道名。
caps: bitmask of supported output capabilities (CapText, CapFile, etc.)
desc: description of the channel, expected meta format, and type enum
def:  通道在记忆计算层的行为（NoMemory/Cleaner）
handler: receives args map with keys: payload (string), type (string), meta (string|optional)

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`a2a`](../examples/index.md#a2a) | `example/a2a/plugin.go:57` | `if err := s.RegisterOutputChannel(p.name, 1, "A2A Agent 互联通道（外部 agent 查询的回复由此返回）", sdk.ChannelDef{}, func(args…` |
| [`acp`](../examples/index.md#acp) | `example/acp/plugin.go:57` | `s.RegisterOutputChannel(p.name, 1, "ACP Agent 互联通道（外部 agent 会话的回复由此返回）", sdk.ChannelDef{}, func(args map[strin…` |
| [`commondev`](../examples/index.md#commondev) | `example/commondev/plugin.go:1291` | `if err := p.sdk.RegisterOutputChannel(outCh, sdk.CapText,` |
| [`qq`](../examples/index.md#qq) | `example/qq/plugin.go:513` | `s.RegisterOutputChannel("qq", sdk.CapText\|sdk.CapFile\|sdk.CapImage\|sdk.CapAudio,` |

<small>`plugin.go:721`</small>

### `PluginSDK.SetToolBlocks`

```go
func (s *PluginSDK) SetToolBlocks(blocks []ContentBlock)
```

SetToolBlocks 在工具处理函数内注入多模态内容块，内核在下一条 tool message
的 content 数组里带上它们。需要「本轮就让模型看到」时用 InjectInputMedia。

<small>`plugin.go:1008`</small>

### `ValidContextPolicy`

```go
func ValidContextPolicy(policy string) bool
```

ValidContextPolicy 校验策略取值；空串等价于 ContextPolicyNone。

<small>`plugin.go:49`</small>

### `ValidRecallPolicy`

```go
func ValidRecallPolicy(policy string) bool
```

ValidRecallPolicy 校验召回策略取值；空串按调用面取默认值。

<small>`plugin.go:69`</small>

