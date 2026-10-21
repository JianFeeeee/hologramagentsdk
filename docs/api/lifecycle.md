<!-- 本页由 tools/apidoc/gensite 从源码生成，请勿手改；要改文档就改 sdk/*.go 的注释。 -->

# 生命周期（Lifecycle）

插件的启动、停止与卸载回调。停止与卸载是两件事：**停止**是进程/加载状态变化，**卸载**（onRemove）是插件被删除前的清理机会。

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

### `PluginSDK.AutoRestart`

```go
func (s *PluginSDK) AutoRestart() bool
```

AutoRestart 返回插件是否允许自动重启。

<small>`plugin.go:1027`</small>

### `PluginSDK.PluginMgr`

```go
func (s *PluginSDK) PluginMgr() PluginMgrAPI
```

PluginMgr returns the plugin manager API (ReloadOne / ReloadPlugins / list).
May be nil if the host did not wire it.

<small>`plugin.go:870`</small>

### `PluginSDK.PluginName`

```go
func (s *PluginSDK) PluginName() string
```

PluginName returns the name of the plugin.

<small>`plugin.go:580`</small>

### `PluginSDK.RegisterOnRemoveHandler`

```go
func (s *PluginSDK) RegisterOnRemoveHandler(fn func())
```

RegisterOnRemoveHandler 注册插件被删除（卸载）时的清理回调。
注册的 handler 会在插件目录被移除前按"后注册先执行"的顺序调用，
适用于清理外部资源、删除配置表、下线状态等删除后处理。
可注册多个；执行后清空（一次删除只执行一次）。

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`calendar`](../examples/index.md#calendar) | `example/calendar/plugin.go:297` | `s.RegisterOnRemoveHandler(p.cleanupData)` |
| [`memo`](../examples/index.md#memo) | `example/memo/plugin.go:69` | `s.RegisterOnRemoveHandler(p.cleanupData)` |
| [`rss`](../examples/index.md#rss) | `example/rss/plugin.go:128` | `s.RegisterOnRemoveHandler(p.cleanupData)` |

<small>`plugin.go:1062`</small>

### `PluginSDK.RegisterPluginAPI`

```go
func (s *PluginSDK) RegisterPluginAPI(name string) error
```

RegisterPluginAPI registers this plugin's API for access by other plugins.

<small>`plugin.go:695`</small>

### `PluginSDK.RegisterStopHandler`

```go
func (s *PluginSDK) RegisterStopHandler(fn func())
```

RegisterStopHandler 注册插件停止阶段的清理回调。
注册的 handler 会在插件 Stop() 之前按"后注册先执行"的顺序调用，
适用于释放资源、落盘状态、关闭子进程等停止时清理操作。
可注册多个；执行后清空（进程停止前只执行一次）。

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`calendar`](../examples/index.md#calendar) | `example/calendar/plugin.go:295` | `s.RegisterStopHandler(p.saveEvents)` |
| [`deepsearch`](../examples/index.md#deepsearch) | `example/deepsearch/plugin.go:695` | `s.RegisterStopHandler(func() { p.shutdownSearxng() })` |

<small>`plugin.go:1037`</small>

### `PluginSDK.RunOnRemoveHandlers`

```go
func (s *PluginSDK) RunOnRemoveHandlers()
```

RunOnRemoveHandlers 执行全部已注册的 onRemove handler（后注册先执行，执行后清空，幂等）。
由内核在卸载插件（registry.RemovePlugin）时、插件 Stop() 之后执行。

<small>`plugin.go:1073`</small>

### `PluginSDK.RunStopHandlers`

```go
func (s *PluginSDK) RunStopHandlers()
```

RunStopHandlers 执行全部已注册的 stop handler（后注册先执行，执行后清空，幂等）。
由内核（内置插件）或插件桥接层（外部插件 z_bridge 的 StopPlugin）在调用插件 Stop() 前执行。

<small>`plugin.go:1048`</small>

### `PluginSDK.SetAutoRestart`

```go
func (s *PluginSDK) SetAutoRestart(enabled bool)
```

SetAutoRestart 设置插件崩溃后内核是否自动重启它。
默认 true。如果插件有无法恢复的状态（如外部连接），应设为 false。

重启是有限度的：线性退避（第 n 次等 n×1s，即 1s→2s→3s），
且同一 5 分钟窗口内第 4 次崩溃就停下不再拉起（详见 README）。
注意这与「重载」（换 plugin.bin 后重新加载）是两回事。

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`a2a`](../examples/index.md#a2a) | `example/a2a/plugin.go:47` | `s.SetAutoRestart(true)` |
| [`acp`](../examples/index.md#acp) | `example/acp/plugin.go:47` | `s.SetAutoRestart(true)` |
| [`ai_image`](../examples/index.md#ai_image) | `example/ai_image/plugin.go:110` | `s.SetAutoRestart(true)` |
| [`bili`](../examples/index.md#bili) | `example/bili/plugin.go:63` | `s.SetAutoRestart(true)` |

<small>`plugin.go:1020`</small>

