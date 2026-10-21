<!-- 本页由 tools/apidoc/gensite 从源码生成，请勿手改；要改文档就改 sdk/*.go 的注释。 -->

# 仅内置插件可用的 API

这些 API **存在于公开 SDK 包里**，但在外部（第三方）插件的运行路径上不可用：要么桥接运行时根本不注入它（拿到 nil），要么内核会拒绝/降级。列在这里是为了让边界显式——而不是让你在运行时才发现拿不到。

判断依据全部来自源码与 `hmapdev` 桥接模板，逐条记在每条说明里。

### `PriorityL4`

!!! warning "仅内核内置插件可用"
    sdk/plugin.go:119-126 明确：L1..L3 任何插件可声明，L4 只有内核级插件可用，外部插件声明会被内核夹到 L3（内核侧有 priority_clamp_test.go 守着）。外部插件文档应说明「声明 L4 无效」。

```go
const PriorityL4
```

PriorityL4 仅内核级（内置）插件可用；外部插件声明会被夹到 L3。

<small>`plugin.go:195`</small>

### `PluginSDK.Events`

!!! warning "仅内核内置插件可用"
    实测全仓 SetEventSubscriber 只有定义、无任何调用点（grep -rn SetEventSubscriber --include=*.go 仅命中 sdk/plugin.go 的定义与 lua_plugin.go 的一句注释）。外部插件拿到的 Events() 恒为 nil。外部插件订阅事件走的是桥接运行时的 events.subscribe RPC（proc_main.go.tmpl:1421 在插件侧分发、内核 protocol.go MethodEventsSubscribe），Lua 插件则走内部 SDK 的 Subscribe。这正是 PLUGIN_DEV.md 里没有说清的一处。

```go
func (s *PluginSDK) Events() EventSubscriber
```

Events returns the event subscriber for listening to kernel events (may be nil if not available).

<small>`plugin.go:639`</small>

### `PluginSDK.SetEventSubscriber`

!!! warning "仅内核内置插件可用"
    同上：无调用点。标 builtin 而非 bridge，因为连桥接运行时都不注入它——外部插件无法用它获得事件订阅能力。

```go
func (s *PluginSDK) SetEventSubscriber(es EventSubscriber)
```

<small>`plugin.go:855`</small>

### `PluginSDK.SetOutputChannelUnregistrar`

!!! warning "仅内核内置插件可用"
    同上，桥接模板不注入。

```go
func (s *PluginSDK) SetOutputChannelUnregistrar(r OutputChannelUnregistrar)
```

SetOutputChannelUnregistrar sets the output channel unregistrar (called by the core at startup).

<small>`plugin.go:774`</small>

### `PluginSDK.UnregisterOutputChannel`

!!! warning "仅内核内置插件可用"
    外部插件的桥接模板只注入 SetOutputChannelRegistrar（proc_main.go.tmpl:693-705），**未**注入 SetOutputChannelUnregistrar（grep Unregistrar 在 templates/ 下无命中）。因此外部插件的 regOutputUnreg 为 nil，UnregisterOutputChannel 会命中 `if reg != nil` 的 else 分支**直接返回 nil**（sdk/plugin.go:539-549）——即**静默无效**：不报错、通道也没注销。内置插件由 internal/sdk/plugin.go:330 注入 cfg.RegOutputUnreg，真正生效。外部插件要让通道下线，只能重载插件。

```go
func (s *PluginSDK) UnregisterOutputChannel(name string) error
```

UnregisterOutputChannel 注销一个输出通道（动态通道随资源生灭时必须调用）。

<small>`plugin.go:732`</small>

### `EventSubscriber.Subscribe`

!!! warning "仅内核内置插件可用"

```go
Subscribe(eventType EventType, handler EventHandler) func()
```

