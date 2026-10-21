<!-- 本页由 tools/apidoc/gensite 从源码生成，请勿手改；要改文档就改 sdk/*.go 的注释。 -->

# 事件（Events）

订阅内核事件。**注意**：外部分布式插件的事件订阅不走 `Events()`（该接口在外部插件路径上未被注入，恒为 nil），而是由 `hmapdev` 生成的运行时通过 `events.subscribe` 完成。详见下方说明。

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

### `Event`

```go
type Event struct { Type EventType `json:"type"` Source string `json:"source"` Payload map[string]interface{} `json:"payload"` T …
```

Event represents a system event published by the kernel.

<small>`plugin.go:413`</small>

### `EventHandler`

```go
type EventHandler func(evt *Event)
```

EventHandler processes a system event.

<small>`plugin.go:421`</small>

### `EventType`

```go
type EventType string
```

EventType identifies the kind of system event.

<small>`plugin.go:395`</small>

### `PluginSDK.Events`

!!! warning "仅内核内置插件可用"
    实测全仓 SetEventSubscriber 只有定义、无任何调用点（grep -rn SetEventSubscriber --include=*.go 仅命中 sdk/plugin.go 的定义与 lua_plugin.go 的一句注释）。外部插件拿到的 Events() 恒为 nil。外部插件订阅事件走的是桥接运行时的 events.subscribe RPC（proc_main.go.tmpl:1421 在插件侧分发、内核 protocol.go MethodEventsSubscribe），Lua 插件则走内部 SDK 的 Subscribe。这正是 PLUGIN_DEV.md 里没有说清的一处。

```go
func (s *PluginSDK) Events() EventSubscriber
```

Events returns the event subscriber for listening to kernel events (may be nil if not available).

<small>`plugin.go:639`</small>

