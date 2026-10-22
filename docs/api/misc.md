<!-- 本页由 tools/apidoc/gensite 从源码生成，请勿手改；要改文档就改 sdk/*.go 的注释。 -->

# 其他类型

剩余的类型与方法：`PluginSDK` 本体的访问器、`StageContext` 的并发控制，以及多模态辅助类型。没有归入上面任何一个主题，但可能仍会用到。

## `DocMemoryAPI`

DocMemoryAPI provides access to the document vector store.

| 方法 | 说明 |
|---|---|
| [`Insert`](#docmemoryapiinsert) |  |
| [`InsertWithMedia`](#docmemoryapiinsertwithmedia) | InsertWithMedia 写入文档并关联媒体。attachments 里带 Data 的会落进 |
| [`Query`](#docmemoryapiquery) |  |
| [`Remove`](#docmemoryapiremove) |  |
| [`Stats`](#docmemoryapistats) |  |

### `DocMemoryAPI.Insert`

```go
Insert(doc *Doc) error
```

<small>`memory.go:76`</small>

### `DocMemoryAPI.InsertWithMedia`

```go
InsertWithMedia(doc *Doc, attachments []MediaAttachment) error
```

InsertWithMedia 写入文档并关联媒体。attachments 里带 Data 的会落进
内容寻址存储（相同字节只存一份），只带 Digest 的直接引用已有内容。
媒体成为文档直接持有的一等记忆块：文档向量会融合它们的原生向量，
因此图片按自己的向量被召回，不依赖任何生成的描述文本。

<small>`memory.go:81`</small>

### `DocMemoryAPI.Query`

```go
Query(text string, topK int) []*Doc
```

<small>`memory.go:75`</small>

### `DocMemoryAPI.Remove`

```go
Remove(id string)
```

<small>`memory.go:82`</small>

### `DocMemoryAPI.Stats`

```go
Stats() map[string]interface{}
```

<small>`memory.go:83`</small>

## `EventSubscriber`

EventSubscriber allows plugins to subscribe to kernel events.
This is a restricted interface: plugins can subscribe but the kernel
controls which events are delivered.

| 方法 | 说明 |
|---|---|
| [`Subscribe`](#eventsubscribersubscribe) |  |

### `EventSubscriber.Subscribe`

!!! warning "仅内核内置插件可用"

```go
Subscribe(eventType EventType, handler EventHandler) func()
```

<small>`plugin.go:427`</small>

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
| [`qq`](../examples/index.md#qq) | `example/qq/plugin.go:2002` | `p.sdk.InjectInterruptTextOpts(p.name, p.name, text, sdk.InjectOptions{` |

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

## `KernelControlAPI`

KernelControlAPI：插件请求**内核本身**重启或关机。

## 为什么需要它

有些状态变更只能靠重启内核生效（典型：改了 LLM 源、换了主密钥、
升级了要重新装配的配置）。没有这条通道时，用户只能自己去 ssh 上敲
systemctl —— 而 WebUI 是大多数人唯一会用的界面。

## 语义边界（重要，别指望它做更多）

这两个方法都是**异步**的：它们只把请求投递给内核，立刻返回。
调用方（WebUI）应当先给用户一个明确回应（"正在重启，页面稍后自动重连"），
而不是等待返回 —— 内核真的会在这几秒内消失。

进程会怎么走：

	RequestKernelRestart ⇒ 内核清理（停插件、flush、CloseAll）→ exit 42
	                       → guard 用**当前二进制**重拉，不经过 systemd、
	                         不计失败轮次（这就是"热更新"那条路）
	RequestKernelShutdown ⇒ 内核清理 → 通知 guard 主动停机 → systemd 不再拉起

★ 关机为什么不能用"guard 退出"实现：systemd 是 Restart=always，
  guard 退出会被它无条件重新拉起。必须显式 `systemctl stop`）

## 失败语义

返回错误只表示**请求没被接受**（例如内核未装配控制通道、或该功能被禁用），
不表示"没重启成功"——成功投递后本进程就会退出，返回值本身没有后续。

| 方法 | 说明 |
|---|---|
| [`RequestKernelRestart`](#kernelcontrolapirequestkernelrestart) | RequestKernelRestart 请求重启内核（热重启，guard 重拉）。 |
| [`RequestKernelShutdown`](#kernelcontrolapirequestkernelshutdown) | RequestKernelShutdown 请求关闭内核（systemd 不再拉起）。 |

### `KernelControlAPI.RequestKernelRestart`

```go
RequestKernelRestart(reason string) error
```

RequestKernelRestart 请求重启内核（热重启，guard 重拉）。

<small>`kernel.go:33`</small>

### `KernelControlAPI.RequestKernelShutdown`

```go
RequestKernelShutdown(reason string) error
```

RequestKernelShutdown 请求关闭内核（systemd 不再拉起）。

<small>`kernel.go:35`</small>

## `KnowledgeAPI`

KnowledgeAPI provides access to the knowledge store.

| 方法 | 说明 |
|---|---|
| [`Add`](#knowledgeapiadd) |  |
| [`List`](#knowledgeapilist) |  |
| [`Search`](#knowledgeapisearch) |  |

### `KnowledgeAPI.Add`

```go
Add(name, content string) error
```

<small>`knowledge.go:6`</small>

### `KnowledgeAPI.List`

```go
List() ([]string, error)
```

<small>`knowledge.go:7`</small>

### `KnowledgeAPI.Search`

```go
Search(query string, topK int) ([]*Knowledge, error)
```

<small>`knowledge.go:5`</small>

## `LLMAPI`

LLMAPI provides access to the LLM provider manager.

| 方法 | 说明 |
|---|---|
| [`CurrentSource`](#llmapicurrentsource) |  |
| [`ListSources`](#llmapilistsources) |  |
| [`SetSource`](#llmapisetsource) |  |

### `LLMAPI.CurrentSource`

```go
CurrentSource() string
```

<small>`llm.go:7`</small>

### `LLMAPI.ListSources`

```go
ListSources() []string
```

<small>`llm.go:5`</small>

### `LLMAPI.SetSource`

```go
SetSource(name string) error
```

<small>`llm.go:6`</small>

## `MemoryAPI`

MemoryAPI provides access to the graph memory (entity-relation store).

| 方法 | 说明 |
|---|---|
| [`Commit`](#memoryapicommit) |  |
| [`Introspect`](#memoryapiintrospect) |  |
| [`MergeEntities`](#memoryapimergeentities) |  |
| [`Purge`](#memoryapipurge) |  |
| [`Recall`](#memoryapirecall) |  |

### `MemoryAPI.Commit`

```go
Commit(triples []Triple) error
```

<small>`memory.go:6`</small>

### `MemoryAPI.Introspect`

```go
Introspect() (map[string]interface{}, error)
```

<small>`memory.go:7`</small>

### `MemoryAPI.MergeEntities`

```go
MergeEntities(source, target string) (int, error)
```

<small>`memory.go:8`</small>

### `MemoryAPI.Purge`

```go
Purge(criteria map[string]string, mode string) (int, error)
```

<small>`memory.go:9`</small>

### `MemoryAPI.Recall`

```go
Recall(query []string, depth int) ([]Entity, []Relation, error)
```

<small>`memory.go:5`</small>

## `Plugin`

Plugin is the interface every plugin must implement.

| 方法 | 说明 |
|---|---|
| [`Name`](#pluginname) |  |
| [`Start`](#pluginstart) |  |
| [`Stop`](#pluginstop) |  |

### `Plugin.Name`

```go
Name() string
```

<small>`plugin.go:14`</small>

### `Plugin.Start`

```go
Start(sdk *PluginSDK) error
```

<small>`plugin.go:15`</small>

### `Plugin.Stop`

```go
Stop() error
```

<small>`plugin.go:16`</small>

## `PluginMgrAPI`

PluginMgrAPI 提供插件管理能力（外部插件可调用）。
由 bridge 注入 dispatch 实现，走 C ABI CORE_PLUGIN_RELOAD_ONE 等。

| 方法 | 说明 |
|---|---|
| [`IsPluginDisabled`](#pluginmgrapiisplugindisabled) | IsPluginDisabled 查询插件是否被**持久**禁用。 |
| [`IsPluginRuntimeStopped`](#pluginmgrapiispluginruntimestopped) | IsPluginRuntimeStopped 查询插件是否处于运行态关闭。 |
| [`ListLoadedPlugins`](#pluginmgrapilistloadedplugins) | ListLoadedPlugins 列出已加载插件。 |
| [`ReloadOne`](#pluginmgrapireloadone) | ReloadOne 重载单个插件（停止后重新加载）。 |
| [`StartPlugin`](#pluginmgrapistartplugin) | StartPlugin 运行态启动：现在拉起，**不解除持久禁用**。 |
| [`StopPlugin`](#pluginmgrapistopplugin) | StopPlugin 运行态关闭：现在停掉，**不改持久状态**。重启后按 |

### `PluginMgrAPI.IsPluginDisabled`

```go
IsPluginDisabled(name string) bool
```

IsPluginDisabled 查询插件是否被**持久**禁用。

<small>`plugin.go:438`</small>

### `PluginMgrAPI.IsPluginRuntimeStopped`

```go
IsPluginRuntimeStopped(name string) bool
```

IsPluginRuntimeStopped 查询插件是否处于运行态关闭。

<small>`plugin.go:465`</small>

### `PluginMgrAPI.ListLoadedPlugins`

```go
ListLoadedPlugins() []string
```

ListLoadedPlugins 列出已加载插件。

<small>`plugin.go:436`</small>

### `PluginMgrAPI.ReloadOne`

```go
ReloadOne(name string) error
```

ReloadOne 重载单个插件（停止后重新加载）。

<small>`plugin.go:434`</small>

### `PluginMgrAPI.StartPlugin`

```go
StartPlugin(name string) error
```

StartPlugin 运行态启动：现在拉起，**不解除持久禁用**。

被持久禁用的插件会被拒绝（提示先用 EnablePlugin）——
持久禁用是用户的明确决定，运行态操作不该绕过它。

<small>`plugin.go:463`</small>

### `PluginMgrAPI.StopPlugin`

```go
StopPlugin(name string) error
```

StopPlugin 运行态关闭：现在停掉，**不改持久状态**。重启后按
auto_start + disabled_plugins 正常决定。

拒绝作用于 pluginmgr 自身与 webui（自锁/失联，内核层面拦死）。

<small>`plugin.go:458`</small>

## `RawFilesAPI`

RawFilesAPI 让插件读写**随包分发的资源**（rawfile）。

## 是什么

`hmapdev build` 会把工程根 `rawfile/` 下的全部内容（以及 plg.json
`rawfiles` 声明的外部路径）打进 .hmap；内核解包时保留路径，落到：

	<插件目录>/rawfile/<原相对路径>

本接口就是插件读这些资源的唯一入口。

## 为什么不让插件自己拼路径

插件**不知道自己的安装目录**：内核的 `PluginDir()` 是给插件管理器用的，
不暴露给插件自身。而且"包内资源"与"运行期生成的文件"必须分开 ——
前者只读、随升级替换，后者可写、跨升级保留。混在一个目录里，
升级时会误删运行期数据，或把旧资源留在原地。

所以：

	RawFiles().Read("main/prompt.txt")   → 读**包内**资源（只读）
	RawFiles().WorkDir()                 → 拿**可写**工作目录（运行期数据放这）

## 安全边界

所有路径都相对 rawfile/ 根，**必须拒绝路径穿越**（`../`）。
资源文件来自包，包可能被替换 —— 让插件能借它读到 `/etc/shadow`
就是一个真实的提权面。

| 方法 | 说明 |
|---|---|
| [`Dir`](#rawfilesapidir) | Dir 返回包内资源的根目录（只读）。 |
| [`Exists`](#rawfilesapiexists) | Exists 报告某个资源是否存在。 |
| [`List`](#rawfilesapilist) | List 列出某个目录下的条目（相对 rawfile/ 根；"" 表示根）。 |
| [`Read`](#rawfilesapiread) | Read 读一个包内资源（路径相对 rawfile/ 根）。 |
| [`WorkDir`](#rawfilesapiworkdir) | WorkDir 返回一个**可写**目录，供插件放运行期生成的文件。 |

### `RawFilesAPI.Dir`

```go
Dir() string
```

Dir 返回包内资源的根目录（只读）。

给需要把整个目录交给外部工具的插件用（如把 node 目录的路径
传给 exec.Command）。**不要往这里写东西** —— 它在包里，随升级替换。

<small>`rawfiles.go:49`</small>

### `RawFilesAPI.Exists`

```go
Exists(rel string) bool
```

Exists 报告某个资源是否存在。

<small>`rawfiles.go:43`</small>

### `RawFilesAPI.List`

```go
List(rel string) ([]string, error)
```

List 列出某个目录下的条目（相对 rawfile/ 根；"" 表示根）。

返回的名字是**相对 path 的单层名**（不是全路径），目录名带 `/` 后缀。

<small>`rawfiles.go:40`</small>

### `RawFilesAPI.Read`

```go
Read(rel string) ([]byte, error)
```

Read 读一个包内资源（路径相对 rawfile/ 根）。

例：Read("main/prompt.txt") 读 <插件目录>/rawfile/main/prompt.txt。

<small>`rawfiles.go:35`</small>

### `RawFilesAPI.WorkDir`

```go
WorkDir() string
```

WorkDir 返回一个**可写**目录，供插件放运行期生成的文件。

与 Dir() 分开的原因：Dir() 里的东西随升级被整体替换，
运行期数据放那里会在下次升级时丢失。

<small>`rawfiles.go:55`</small>

## `SecretsAPI`

SecretsAPI 是插件访问**内核变量库**的接口。

## 为什么要有它（去重）

在此之前，commondev 插件自带了一整套加密存储（`secrets.json` +
AES-256-GCM + 别名表 + `${ENV}` 解析），与内核的 `internal/security`
**完全重复**：

	             内核 security.Store        插件 unifiedSecretFile
	  落盘        secrets.json               secrets.json
	  加密        AES-256-GCM                AES-256-GCM
	  别名        HMAC 自动派生              用户指定
	  引用语法     @{sec_xxx}                 @{name}

重复的代价不只是代码量：**两套变量库互不可见** —— 内核拦截时登记的
变量，插件看不到；插件登记的，内核拦截不认识。结果是同一个凭据可能
在两处各存一份，而使用者不知道哪个才是权威。

## ★ 安全设计：明文的边界

内核的硬约束 S1 是「不提供取回明文的接口」—— 理由是一旦 LLM 能取回，
它就能把明文写回上下文，整层防护作废。

那为什么插件可以拿到明文？因为**边界不在插件侧**：

	① 内核在**插件 handler 之前**就把工具参数里的 @{别名} 填好了
	   （`StageHost.ExecuteTool` → `security.FillMap`）—— 插件本来
	   就能从 args 里读到明文，这是设计使然（否则插件无法用凭据）。
	② 明文若被插件**返回**给内核，会被两处拦回别名：
	   · `ContextEvent` 的 scrub（记忆/历史侧路）
	   · provider 装饰器（出向，LLM 永不看到）

两条都有判据钉住（`leakback_test.go`）。所以：

	**暴露 Fill 不新增泄漏面** —— 它不是「开了一个口子」，
	而是把「本来就存在的能力」从各插件自己实现改为统一接口。

⚠️ 但绝不能暴露 `Reveal(alias) -> 明文` 这样的**按名字直取**接口：
那会让插件（以及通过插件操纵它的 LLM）枚举别名并逐个取值。
本接口刻意**没有**这样的方法 —— 见下面 Fill 与 List 的签名。

## 与 Encrypt/Decrypt 的区别

插件常把凭据存进自己的配置文件（如 pi 的 `auth.json`）。那些文件权限
0600 但**明文落盘**。`Encrypt`/`Decrypt` 提供 at-rest 加密，
密钥由内核统一管理，插件不必自持密钥（自持会让密钥散落、无法轮换）。

| 方法 | 说明 |
|---|---|
| [`Available`](#secretsapiavailable) | Available 报告变量库是否可用（内核未启用安全模块时为 false）。 |
| [`Decrypt`](#secretsapidecrypt) | Decrypt 解密由 Encrypt 产生的串。 |
| [`Encrypt`](#secretsapiencrypt) | Encrypt 加密一段字节用于插件自己落盘（密钥由内核管理）。 |
| [`Fill`](#secretsapifill) | Fill 把一段文本里的 @{别名} 换成明文。 |
| [`Forget`](#secretsapiforget) | Forget 按别名删除一个变量。 |
| [`List`](#secretsapilist) | List 列出变量库的元信息（别名/来源/备注/命中次数）—— **不含明文**。 |
| [`Store`](#secretsapistore) | Store 把一个凭据登记进内核变量库，返回其**别名**。 |

### `SecretsAPI.Available`

```go
Available() bool
```

Available 报告变量库是否可用（内核未启用安全模块时为 false）。

★ 插件应据此**响亮失败**而不是静默降级 —— 例如
「密钥不可用 ⇒ 拒绝明文存储」。

<small>`secrets.go:100`</small>

### `SecretsAPI.Decrypt`

```go
Decrypt(stored string) ([]byte, error)
```

Decrypt 解密由 Encrypt 产生的串。

<small>`secrets.go:94`</small>

### `SecretsAPI.Encrypt`

```go
Encrypt(plain []byte) (string, error)
```

Encrypt 加密一段字节用于插件自己落盘（密钥由内核管理）。
返回的串带版本前缀，可直接存文件。

<small>`secrets.go:91`</small>

### `SecretsAPI.Fill`

```go
Fill(text string) (out string, replaced int, aliases []string)
```

Fill 把一段文本里的 @{别名} 换成明文。

用途：插件需要把明文喂给**不经过内核参数回填**的地方 ——
例如 commondev 的 enc_shell 要把命令经 **stdin** 送给 bash
（不是 argv，避免本机其他进程从 /proc/<pid>/cmdline 读到明文）。

★ 为什么这个方法是安全的（而不是"开了个取回明文的口子"）：
插件**本来就**能从 `args` 里拿到内核填好的明文 ——
内核在 handler 之前就填了。Fill 只是让插件能对**自己构造的文本**
做同样的事（如读文件内容、拼一段脚本）。

它**不是**按名字直取：要填谁必须先在文本里写出那个别名，
而别名本身不泄漏明文（HMAC 派生）。想枚举就得先 List，
而 List 只给元信息 —— 拿到别名后仍只能用，不能读回。

返回替换处数与用到的别名（便于插件做输出脱敏时的比对）。

<small>`secrets.go:87`</small>

### `SecretsAPI.Forget`

```go
Forget(alias string) error
```

Forget 按别名删除一个变量。

★ 变量库**不自动淘汰**（用户明确）：泄漏控制靠主动删除，不靠超时。
所以凭据用完后应主动删。

<small>`secrets.go:69`</small>

### `SecretsAPI.List`

```go
List() []SecretInfo
```

List 列出变量库的元信息（别名/来源/备注/命中次数）—— **不含明文**。

★ 刻意返回元信息而非值：调用方拿不到明文，只能引用。

<small>`secrets.go:63`</small>

### `SecretsAPI.Store`

```go
Store(value, note string) (alias string, err error)
```

Store 把一个凭据登记进内核变量库，返回其**别名**。

之后在工具参数里写 @{别名}，内核会在调用插件前换回真实值。

★ 只返回别名，不回显明文 —— 避免插件（或它转述给 LLM 的内容）
顺带把明文带出去。

note 是给人看的用途说明（如「Gitea 推送令牌」）。

<small>`secrets.go:58`</small>

## `SettingsAPI`

| 方法 | 说明 |
|---|---|
| [`DataDir`](#settingsapidatadir) | DataDir returns the plugin-specific data directory (guaranteed to exist): |
| [`Defs`](#settingsapidefs) | Defs returns config definitions matching the prefix. |
| [`Dump`](#settingsapidump) | Dump returns all config values. |
| [`Get`](#settingsapiget) | Get reads the plugin's own config value (config_<name> table). |
| [`GetCore`](#settingsapigetcore) | GetCore reads the core config table. |
| [`GetPlugin`](#settingsapigetplugin) | GetPlugin reads another plugin's config table. |
| [`GetSecret`](#settingsapigetsecret) | GetSecret reads a value written by SetSecret (transparently decrypted). |
| [`List`](#settingsapilist) | List returns all keys matching the given prefix. |
| [`ListCore`](#settingsapilistcore) | ListCore lists core config keys matching the prefix. |
| [`ListPlugin`](#settingsapilistplugin) | ListPlugin lists another plugin's config keys matching the prefix. |
| [`Plugins`](#settingsapiplugins) | Plugins returns a list of all plugin config namespaces. |
| [`RegisterDef`](#settingsapiregisterdef) | RegisterDef registers a config definition for UI display. |
| [`Set`](#settingsapiset) | Set writes a config value to the plugin's own config table. |
| [`SetCore`](#settingsapisetcore) | SetCore writes to the core config table. |
| [`SetPlugin`](#settingsapisetplugin) | SetPlugin writes to another plugin's config table. |
| [`SetSecret`](#settingsapisetsecret) | SetSecret writes a value that is ALWAYS encrypted at rest, regardless of |

### `SettingsAPI.DataDir`

```go
DataDir() string
```

DataDir returns the plugin-specific data directory (guaranteed to exist):
<daemon data>/plugin_data/<plugin_name>. Plugins should persist any
runtime files (generated images, caches, downloads) here.

<small>`settings.go:40`</small>

### `SettingsAPI.Defs`

```go
Defs(prefix string) []*ConfigDef
```

Defs returns config definitions matching the prefix.

<small>`settings.go:55`</small>

### `SettingsAPI.Dump`

```go
Dump() map[string]interface{}
```

Dump returns all config values.

<small>`settings.go:58`</small>

### `SettingsAPI.Get`

```go
Get(key string) (interface{}, error)
```

Get reads the plugin's own config value (config_<name> table).

<small>`settings.go:5`</small>

### `SettingsAPI.GetCore`

```go
GetCore(key string) (interface{}, error)
```

GetCore reads the core config table.

<small>`settings.go:29`</small>

### `SettingsAPI.GetPlugin`

```go
GetPlugin(plugin, key string) (interface{}, error)
```

GetPlugin reads another plugin's config table.

<small>`settings.go:43`</small>

### `SettingsAPI.GetSecret`

```go
GetSecret(key string) (string, error)
```

GetSecret reads a value written by SetSecret (transparently decrypted).

<small>`settings.go:23`</small>

### `SettingsAPI.List`

```go
List(prefix string) ([]string, error)
```

List returns all keys matching the given prefix.

<small>`settings.go:26`</small>

### `SettingsAPI.ListCore`

```go
ListCore(prefix string) ([]string, error)
```

ListCore lists core config keys matching the prefix.

<small>`settings.go:35`</small>

### `SettingsAPI.ListPlugin`

```go
ListPlugin(plugin, prefix string) ([]string, error)
```

ListPlugin lists another plugin's config keys matching the prefix.

<small>`settings.go:49`</small>

### `SettingsAPI.Plugins`

```go
Plugins() []string
```

Plugins returns a list of all plugin config namespaces.

<small>`settings.go:61`</small>

### `SettingsAPI.RegisterDef`

```go
RegisterDef(def ConfigDef)
```

RegisterDef registers a config definition for UI display.

<small>`settings.go:52`</small>

### `SettingsAPI.Set`

```go
Set(key string, value interface{}) error
```

Set writes a config value to the plugin's own config table.

<small>`settings.go:8`</small>

### `SettingsAPI.SetCore`

```go
SetCore(key string, value interface{}) error
```

SetCore writes to the core config table.

<small>`settings.go:32`</small>

### `SettingsAPI.SetPlugin`

```go
SetPlugin(plugin, key string, value interface{}) error
```

SetPlugin writes to another plugin's config table.

<small>`settings.go:46`</small>

### `SettingsAPI.SetSecret`

```go
SetSecret(key string, value interface{}) error
```

SetSecret writes a value that is ALWAYS encrypted at rest, regardless of
the key name. Use it when the key name is neutral (e.g. "blob", "conn")
and so cannot be detected as sensitive automatically.

Returns an error (not a silent plaintext write) when no master key is
installed — a plugin that believes it stored a secret must not end up
with plaintext on disk.

The companion read is GetSecret. ConfigDef.Secret/Type=="password" and
sensitive key names are encrypted automatically and do NOT need this.

<small>`settings.go:20`</small>

## `SocialAPI`

SocialAPI provides read-only access to the social graph (person profiles and relationships).
External plugins can query person traits and social networks but cannot modify them.

| 方法 | 说明 |
|---|---|
| [`GetNetwork`](#socialapigetnetwork) |  |
| [`GetPerson`](#socialapigetperson) |  |
| [`GetRelations`](#socialapigetrelations) |  |
| [`GetTrait`](#socialapigettrait) |  |
| [`ListPersons`](#socialapilistpersons) |  |

### `SocialAPI.GetNetwork`

```go
GetNetwork(name string, depth int) ([]*PersonProfile, error)
```

<small>`memory.go:104`</small>

### `SocialAPI.GetPerson`

```go
GetPerson(name string) (*PersonProfile, error)
```

<small>`memory.go:101`</small>

### `SocialAPI.GetRelations`

```go
GetRelations(name string) ([]SocialRelation, error)
```

<small>`memory.go:103`</small>

### `SocialAPI.GetTrait`

```go
GetTrait(name, trait string) (string, bool)
```

<small>`memory.go:102`</small>

### `SocialAPI.ListPersons`

```go
ListPersons() ([]string, error)
```

<small>`memory.go:105`</small>

## `TextMemoryAPI`

TextMemoryAPI provides access to chronological text event storage.

| 方法 | 说明 |
|---|---|
| [`Append`](#textmemoryapiappend) |  |

### `TextMemoryAPI.Append`

```go
Append(evt TextEvent) error
```

<small>`memory.go:44`</small>

### `AudioURL`

```go
type AudioURL struct { URL string `json:"url"` }
```

<small>`plugin.go:1098`</small>

### `AutoStartOnDemand`

```go
const AutoStartOnDemand
```

AutoStartOnDemand 表示启用但不自启：待 agent 显式拉起。

★ 用它的插件必须配 skill，否则 agent 无从得知要先拉起。

<small>`autostart.go:68`</small>

### `AutoStartOnKernel`

```go
const AutoStartOnKernel
```

AutoStartOnKernel 表示伴随内核启动（缺少该字段时的默认形态）。

<small>`autostart.go:64`</small>

### `EffectiveProxyAuth`

```go
func EffectiveProxyAuth(auth string) string
```

EffectiveProxyAuth 返回生效的鉴权模式（空串归一化为 ProxyAuthHomeAgent）。

<small>`proxy.go:169`</small>

### `ToolError.Error`

```go
func (e *ToolError) Error() string
```

Error 实现 error，便于工具同时走 (ToolError, error) 通道。

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`a2a`](../examples/index.md#a2a) | `example/a2a/plugin.go:317` | `http.Error(w, "query/message.text required", http.StatusBadRequest)` |
| [`acp`](../examples/index.md#acp) | `example/acp/plugin.go:175` | `http.Error(w, "", http.StatusMethodNotAllowed)` |
| [`ai_image`](../examples/index.md#ai_image) | `example/ai_image/plugin.go:259` | `return map[string]interface{}{"isError": true, "content": "Request failed: " + err.Error()}, nil` |
| [`browser`](../examples/index.md#browser) | `example/browser/plugin_test.go:15` | `if err == nil \|\| !strings.Contains(err.Error(), "timeout is required") {` |

<small>`plugin.go:294`</small>

### `FacilityAgent`

```go
const FacilityAgent
```

FacilityAgent 是主 Agent 就绪（**最晚**的一个，通常在插件加载前）。

<small>`facilities.go:63`</small>

### `FacilityConfig`

```go
const FacilityConfig
```

FacilityConfig 是配置快照（cfg）就绪。

<small>`facilities.go:37`</small>

### `FacilityDocStore`

```go
const FacilityDocStore
```

FacilityDocStore 是文档记忆就绪。

<small>`facilities.go:53`</small>

### `FacilityKernelIO`

```go
const FacilityKernelIO
```

FacilityKernelIO 是 IOManager + EventBus 就绪。

<small>`facilities.go:39`</small>

### `FacilityKnowledge`

```go
const FacilityKnowledge
```

FacilityKnowledge 是知识库就绪。

<small>`facilities.go:55`</small>

### `FacilityLLM`

```go
const FacilityLLM
```

FacilityLLM 是多源 Provider 管理就绪。

<small>`facilities.go:51`</small>

### `FacilityLua`

```go
const FacilityLua
```

FacilityLua 是 Lua 协议适配 VM 就绪。

<small>`facilities.go:43`</small>

### `FacilityMemory`

```go
const FacilityMemory
```

FacilityMemory 是图记忆（含索引/社交图/蒸馏）就绪。

<small>`facilities.go:41`</small>

### `FacilityMultimodal`

```go
const FacilityMultimodal
```

FacilityMultimodal 是多模态向量空间就绪。

<small>`facilities.go:57`</small>

### `FacilityONNX`

```go
const FacilityONNX
```

FacilityONNX 是依存句法分析器就绪。

<small>`facilities.go:59`</small>

### `FacilityStageHost`

```go
const FacilityStageHost
```

FacilityStageHost 是阶段管道 + 插件注册表就绪。

<small>`facilities.go:61`</small>

### `FacilitySupervisor`

```go
const FacilitySupervisor
```

FacilitySupervisor 是代理生命周期守护就绪。

<small>`facilities.go:45`</small>

### `FacilityTextMemory`

```go
const FacilityTextMemory
```

FacilityTextMemory 是文本记忆就绪。

<small>`facilities.go:49`</small>

### `FacilityTracker`

```go
const FacilityTracker
```

FacilityTracker 是变更追踪（overlayfs）就绪。

<small>`facilities.go:47`</small>

### `ImageURL`

```go
type ImageURL struct { URL string `json:"url"` Detail string `json:"detail,omitempty"` }
```

<small>`plugin.go:1093`</small>

### `PluginSDK.KernelControl`

```go
func (s *PluginSDK) KernelControl() KernelControlAPI
```

KernelControl returns the kernel control API (restart / shutdown).

可能为 nil（宿主没接这条通道，或插件没有 kernel-control 能力组）。
调用方必须判 nil —— 拿到 nil 时应当如实告知用户"当前环境不支持"，
而不是假装点了没反应的按钮还能用。

<small>`plugin.go:889`</small>

### `KnownFacilities`

```go
func KnownFacilities() []string
```

KnownFacilities 返回本 SDK 已知的全部设施名（供工具链校验插件声明）。

★ 它让**打包期**就能发现"插件声明了不存在的设施名" —— 比运行期发现早得多
（运行期发现意味着用户已经装上了那个永不启动的插件）。

<small>`facilities.go:70`</small>

### `MemItem`

```go
type MemItem struct { Role string `json:"role"` Content string `json:"content"` Score float64 `json:"score"` }
```

MemItem represents a memory item in stage context.

<small>`plugin.go:250`</small>

### `NormalizeProxyHost`

```go
func NormalizeProxyHost(pluginName string) string
```

NormalizeProxyHost 由插件名派生默认 Host 标签。

下划线转连字符：插件名允许下划线（huawei_smarthome），但 DNS label 不允许，
直接用会导致该子域名无法解析——这里统一转换，避免每个插件各自碰运气。

<small>`proxy.go:202`</small>

### `OnDemandNeedsSkill`

```go
const OnDemandNeedsSkill
```

OnDemandNeedsSkill 说明 auto_start=false 的插件为什么必须有 skill。

这条不是"建议"而是**功能完整性的一部分**：

	插件写了 auto_start=false 但没配 skill
	  ⇒ 该插件的工具不出现在工具的初始清单里
	  ⇒ 模型不知道它存在
	  ⇒ 它永远不会被调用，也永远不会被拉起

也就是说，缺少 skill 的按需插件等价于**没装**，但比没装更糟 ——
它占着磁盘和安装记录，让人以为它可用。

<small>`autostart.go:82`</small>

### `PluginSDK`

```go
type PluginSDK struct { name string regTool ToolRegistrar regStage StageRegistrar regAPI APIRegistrar regOutput OutputChannelRegistrar …
```

PluginSDK is the main API surface provided to plugins at runtime.
It wraps tool registration, settings, memory, knowledge, LLM, and IO injection.

<small>`plugin.go:512`</small>

### `ProxyAuthHomeAgent`

```go
const ProxyAuthHomeAgent
```

ProxyAuthHomeAgent 表示由 HologramAgent 统一保护：浏览器走门户会话
（homeagent_session cookie），非浏览器客户端走 X-API-Key。
两者都没有时返回 401，而不是把请求透传给上游。

<small>`proxy.go:149`</small>

### `ProxyAuthNone`

```go
const ProxyAuthNone
```

ProxyAuthNone 表示不经 HologramAgent 鉴权，直接把请求转发给上游。

适用场景：上游自己有鉴权且调用方不是浏览器（设备/嵌入式客户端），
或上游是刻意公开的服务。选用它意味着**信任上游自身的鉴权**，
且该服务在网络层可达范围内对所有人开放。

<small>`proxy.go:156`</small>

### `ProxyDef`

```go
type ProxyDef struct { // Name 是同一插件内多条声明的唯一标识（如 "ui"、"api"）。 // 运行期由 RegisterProxy 的第一个参数填入；声明式由 …
```

反向代理声明：插件告诉 HologramAgent「我起了个 HTTP 服务，请把它反代出去」。

为什么需要：插件自带 Web UI / HTTP API 时，监听地址在插件自己的配置里
（如 127.0.0.1:12100），外部无从得知；而 webui 的对外端口通常只有一个
（默认 :8080，且常经 frp 单端口隧道穿透）。没有声明机制时，用户只能
「知道端口 + 自己配转发」，插件换端口就失效。

设计取舍——**声明式而非注册式**：声明写在 plugin.json 里，由 HologramAgent
在加载插件时读取聚合，而不是让插件在运行期调 API 注册。理由：
 1. 静态可发现：未启动/已崩溃的插件，其服务声明依然可见（可给出准确报错
    「插件 X 声明了 ui 但目标 127.0.0.1:12100 不可达」，而不是静默 404）；
 2. 可版本化：声明随插件包一起分发、可 diff、可审计；
 3. 旧内核无害：manifest 解析忽略未知字段，未支持该能力的 HologramAgent 读旧
    插件、或旧 HologramAgent 读新插件都不会报错。

与 ToolDef / ChannelDef / ConfigDef 同族：SDK 定义声明契约，内核实现行为。
声明方式与其它能力一致 —— 在 Start() 里调 RegisterProxy(name, def)，
或写进 plugin.json 的 proxies 字段（外部插件两种都支持）。

安全性：**不声明 = 不被反代**。声明本身就是能力声明，因此不需要在
capabilities 里另外开一个开关——最小权限默认生效。

# 单一入口原则（强制要求）

**一个声明 = 一个入口**。被反代的插件必须让它的全部资源与接口都能从
该入口的一个基准路径出发访问到，不得依赖「入口之外的根路径」。

为什么强制：反代有两种挂载形态，而它们对「根路径」的处理截然不同——

	Host 形态（host）：插件独占 <标签>.<基域名>，根路径就是插件的根。
	                  根绝对路径（fetch('/api/x')）**天然正确**。
	Path 形态（path）：插件挂在门户自身 host 的某个前缀下，根路径属于**门户**。
	                  此时插件里的 fetch('/api/x') 会打到门户自己的 /api/x
	                  —— 静默错路由，页面能开但功能全坏。

于是「同一个插件必须同时支持两种形态」这条要求，等价于：

	**插件内部一律使用相对路径**（或基于 <base>/location 推导的路径），
	绝不硬编码以 / 开头的绝对路径。

这样同一份前端在两种形态下都正确，插件作者也不必知道自己被挂在哪。
反代层据此可以：外部子域可用时给 Host 形态，子域不可用（证书/放行限制）
时给 Path 形态，**无需插件配合改动**。

自检（插件作者在本地就该做）：把页面挂到 <门户>/<任意前缀>/ 下访问，
所有请求都必须仍然打到插件自己。

本项目实测案例：某插件前端写死 fetch('/api/status')，配在
/p/huawei/ 下会打到门户的 /api/status（404 或返回门户数据）；
改成相对路径后两种形态同时可用。
ProxyDef 是一个服务的**反代声明体**。

与 ToolDef 同构：Name 同时出现在字段与 RegisterProxy 的第一个参数里
（ToolDef 也是这么做的 —— 字段供 plugin.json 序列化，参数供运行期调用）。
Name 只用于展示、日志与冲突提示，**不参与路由**（路由键是 Host 与 Path）。

<small>`proxy.go:64`</small>

### `ProxyRegistrar`

```go
type ProxyRegistrar func(name string, def ProxyDef)
```

ProxyRegistrar 由内核注入（与 ToolRegistrar / InputChannelRegistrar 同族）。
插件不直接调它，用 RegisterProxy。

为什么需要运行期注册（明明有 plugin.json 自动发现）：**内置插件**
（编译进内核、没有独立插件目录与 plugin.json，如 remotedevice）扫不到；
而它们恰恰最需要被反代出去（设备网关就是内置的）。两种来源互补：
  - 外部插件 → plugin.json 的 proxies（静态，未启动也可见）
  - 内置插件 → RegisterProxy（运行期，随 Start 注册）

<small>`proxy.go:306`</small>

### `PluginSDK.RawFiles`

```go
func (s *PluginSDK) RawFiles() RawFilesAPI
```

SetMemoryAPI sets the memory API (called by the core at startup).
RawFiles 返回随包分发资源的读取接口（内核装配时注入）。

未注入时返回 nil —— 那是旧内核或测试环境。插件应判空后**响亮失败**，
而不是拿一个空实现继续（会表现为"资源读出来是空的"，与"资源不存在"难区分）。

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`commondev`](../examples/index.md#commondev) | `example/commondev/sidecar.go:573` | `return p.sdk.RawFiles()` |

<small>`plugin.go:799`</small>

### `PluginSDK.RegisterProxy`

```go
func (s *PluginSDK) RegisterProxy(name string, def ProxyDef)
```

RegisterProxy 声明一个需要 HologramAgent 反代出去的服务。

与 RegisterTool / RegisterInputChannel / RegisterOutputChannel 同一风格：
显式给名字 + 声明体。名字用于展示、日志与冲突提示（不参与路由 —— 路由键是
def.Host / def.Path）。

用法（通常在 Start 里调用）：

	s.RegisterProxy("ui", sdk.ProxyDef{
	    Host: "myapp", Target: "127.0.0.1:12100",
	})

声明立即生效（反代表在下一次请求时重建）。**不做去重**：同一 Host/Path
被两条声明占用时由反代层判定冲突并明确报错，而不是在这里静默吞掉 ——
插件作者需要看见冲突。

与 plugin.json 的 proxies 字段等价：写哪个都行，两者会合并（同名以本调用为准）。

<small>`proxy.go:335`</small>

### `SDKVersion`

```go
var SDKVersion
```

SDKVersion 是对外暴露的 SDK 版本号。

<small>`plugin.go:10`</small>

### `ScenePolicyAuto`

```go
const ScenePolicyAuto
```

场面策略：决定一次输入是否参与**场面识别**（场景式记忆）。

与前两项再正交一轴：NoMemory 管「进不进记忆计算」、ContextPolicy 管
「裁不裁上下文」、RecallPolicy 管「召不召回记忆」，本项管的是
「这条输入算不算一场戏的一部分」——它决定输入会不会产出现场指纹
（通道/对话对象/工具/话题/时段），进而决定会不会长出、命中、写入场景。

默认（空串或 ScenePolicyAuto）**参与**，保持既有行为：场景式记忆自
v1.3 落地起就对所有通道无条件生效，没有开关。不默认关有两个原因：
 1. 场景只**附加**现有记忆的检索路，不改记忆本体，默认关会让存量
    通道突然失去场景召回；
 2. 「关」是少数意图（内部信噪通道），少数意图不该是默认——
    与 ContextPolicy 刻意相反（同为破坏性操作，那里是默认关）。

该关的典型是纯内部通道：system（内核自循环）、kernel、timer、healthcheck。
但**现网不标任何一个**（2026-09-26 裁定）：实测这些 0-refs 通道合计 70
strength、0 条记忆，场景召回返回空；而 declared 场景不进相似度空间
（loadEmergentScenesLocked 只取 origin='emergent'），多写对聚类零影响。
「多写无影响、少写会缺场景」——默认 auto 保持开，声明项只作为插件
将来确实需要时的闸门。

<small>`plugin.go:98`</small>

### `ScenePolicyNone`

```go
const ScenePolicyNone
```

场面策略：决定一次输入是否参与**场面识别**（场景式记忆）。

与前两项再正交一轴：NoMemory 管「进不进记忆计算」、ContextPolicy 管
「裁不裁上下文」、RecallPolicy 管「召不召回记忆」，本项管的是
「这条输入算不算一场戏的一部分」——它决定输入会不会产出现场指纹
（通道/对话对象/工具/话题/时段），进而决定会不会长出、命中、写入场景。

默认（空串或 ScenePolicyAuto）**参与**，保持既有行为：场景式记忆自
v1.3 落地起就对所有通道无条件生效，没有开关。不默认关有两个原因：
 1. 场景只**附加**现有记忆的检索路，不改记忆本体，默认关会让存量
    通道突然失去场景召回；
 2. 「关」是少数意图（内部信噪通道），少数意图不该是默认——
    与 ContextPolicy 刻意相反（同为破坏性操作，那里是默认关）。

该关的典型是纯内部通道：system（内核自循环）、kernel、timer、healthcheck。
但**现网不标任何一个**（2026-09-26 裁定）：实测这些 0-refs 通道合计 70
strength、0 条记忆，场景召回返回空；而 declared 场景不进相似度空间
（loadEmergentScenesLocked 只取 origin='emergent'），多写对聚类零影响。
「多写无影响、少写会缺场景」——默认 auto 保持开，声明项只作为插件
将来确实需要时的闸门。

<small>`plugin.go:99`</small>

### `SecretInfo`

```go
type SecretInfo struct { Alias string `json:"alias"` Source string `json:"source"` // intercepted | submitted Note string `json:"note,omitempty"` …
```

SecretInfo 是一条变量的元信息（**不含明文**）。

<small>`secrets.go:104`</small>

### `PluginSDK.Secrets`

```go
func (s *PluginSDK) Secrets() SecretsAPI
```

Social returns the social graph API (may be nil if not available).
Secrets returns the kernel secret vault API.

★ 内核未启用安全模块时它**不是 nil**，而是 Available() 返回 false 的
实现 —— 这样插件可以统一地"先问 Available 再决定"，不必到处判空。

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`commondev`](../examples/index.md#commondev) | `example/commondev/encstore.go:397` | `return p.sdk.Secrets()` |

<small>`plugin.go:626`</small>

### `PluginSDK.SetProxyRegistrar`

```go
func (s *PluginSDK) SetProxyRegistrar(r ProxyRegistrar)
```

SetProxyRegistrar 由内核注入。插件不直接调它（与 SetInputChannelRegistrar 同族）。

<small>`proxy.go:309`</small>

### `ToolError`

```go
type ToolError struct { // Field 是出错的参数字段名（参数校验失败时填）。 Field string `json:"field,omitempty"` // Reason 是机器可读的原因码： …
```

ToolError 描述一次工具调用的失败原因。

存在的理由：失败若只表达为文本，模型无法定位到字段，只能原样重试
（实测 cmd_run 失败率 34%~48%，全部源于同一个成因：参数被截断或
JSON 写坏，工具却只回报 "command is required" 这类与真因无关的错）。

⚠️ 零值语义：插件**不必**改用本类型。内核的失败识别同时兼容既有三种约定
（{"error":…}、{"isError":true,…}、显式 error 返回），见 core.isToolError。
本类型是给**新写**的工具用的可选项，不是迁移要求。

<small>`plugin.go:282`</small>

### `PluginSDK.UnregisterOutputChannel`

!!! warning "仅内核内置插件可用"
    外部插件的桥接模板只注入 SetOutputChannelRegistrar（proc_main.go.tmpl:693-705），**未**注入 SetOutputChannelUnregistrar（grep Unregistrar 在 templates/ 下无命中）。因此外部插件的 regOutputUnreg 为 nil，UnregisterOutputChannel 会命中 `if reg != nil` 的 else 分支**直接返回 nil**（sdk/plugin.go:539-549）——即**静默无效**：不报错、通道也没注销。内置插件由 internal/sdk/plugin.go:330 注入 cfg.RegOutputUnreg，真正生效。外部插件要让通道下线，只能重载插件。

```go
func (s *PluginSDK) UnregisterOutputChannel(name string) error
```

UnregisterOutputChannel 注销一个输出通道（动态通道随资源生灭时必须调用）。

<small>`plugin.go:732`</small>

### `ValidProxyAuth`

```go
func ValidProxyAuth(auth string) bool
```

ValidProxyAuth 校验 Auth 取值；空串合法（等价 ProxyAuthHomeAgent）。

<small>`proxy.go:160`</small>

### `ValidProxyHostLabel`

```go
func ValidProxyHostLabel(label string) bool
```

ValidProxyHostLabel 校验子域名标签是否合法（DNS label 规则）。

独立成导出函数：插件作者在写声明时、HologramAgent 在加载时、工具链在打包时
都要用同一套规则判定，避免三处各写一份而互相不一致。

<small>`proxy.go:180`</small>

### `ValidScenePolicy`

```go
func ValidScenePolicy(policy string) bool
```

ValidScenePolicy 校验场面策略取值；空串等价于 ScenePolicyAuto。

<small>`plugin.go:103`</small>

### `ValidateProxyDef`

```go
func ValidateProxyDef(d ProxyDef) string
```

ValidateProxyDef 校验一条反代声明，返回人类可读的错误说明（合法时为空）。

为什么要在 SDK 里做校验：HologramAgent 加载插件时必须能明确拒绝坏声明并说明
原因（而不是静默忽略导致用户以为配好了）；插件作者也需要在本地就能查出
拼错的 Target/Host。同一套规则两端共用。

<small>`proxy.go:229`</small>

