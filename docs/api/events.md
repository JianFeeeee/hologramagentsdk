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

<small>`plugin.go:526`</small>

## `EventsAPI`

EventsAPI 是插件可见的事件总线。

它**不是** *events.Bus 的投影：这是一个有边界、有审查的接口。

| 方法 | 说明 |
|---|---|
| [`AllowedPublish`](#eventsapiallowedpublish) | AllowedPublish 返回当前可发布的事件类型（白名单 + 自己的命名空间前缀）。 |
| [`AllowedSubscribe`](#eventsapiallowedsubscribe) | AllowedSubscribe 返回当前可订阅的事件类型。 |
| [`Publish`](#eventsapipublish) | Publish 发布一个事件。 |
| [`Subscribe`](#eventsapisubscribe) | Subscribe 订阅一个白名单内的事件（只读面）。 |

### `EventsAPI.AllowedPublish`

```go
AllowedPublish() []string
```

AllowedPublish 返回当前可发布的事件类型（白名单 + 自己的命名空间前缀）。

<small>`events.go:43`</small>

### `EventsAPI.AllowedSubscribe`

```go
AllowedSubscribe() []string
```

AllowedSubscribe 返回当前可订阅的事件类型。

<small>`events.go:46`</small>

### `EventsAPI.Publish`

```go
Publish(eventType string, payload map[string]interface{}) error
```

Publish 发布一个事件。

eventType 必须在白名单内（见 AllowedPublish），或形如
"plugin.<本插件名>.<后缀>"。载荷大小与键数有上限。

<small>`events.go:34`</small>

### `EventsAPI.Subscribe`

```go
Subscribe(eventType string, handler func(payload map[string]interface{})) (cancel func(), err error)
```

Subscribe 订阅一个白名单内的事件（只读面）。

返回的 cancel 在插件 Stop 时调用可解除订阅；宿主卸载插件时也会自动解除。
handler 的 panic 会被总线隔离，不影响其他订阅者。

<small>`events.go:40`</small>

### `Event`

```go
type Event struct { Type EventType `json:"type"` Source string `json:"source"` Payload map[string]interface{} `json:"payload"` T …
```

Event represents a system event published by the kernel.

<small>`plugin.go:512`</small>

### `EventHandler`

```go
type EventHandler func(evt *Event)
```

EventHandler processes a system event.

<small>`plugin.go:520`</small>

### `EventNameDeviceReady`

```go
const EventNameDeviceReady
```

EventNameDeviceReady：工具/设备数据面就绪（只读订阅）。

<small>`events.go:65`</small>

### `EventNameSecurityReady`

```go
const EventNameSecurityReady
```

EventNameSecurityReady：安全闸门装配完成（只读订阅）。

<small>`events.go:63`</small>

### `EventNameSkillDetected`

```go
const EventNameSkillDetected
```

EventNameSkillDetected：兼容层把发现的纯 SKILL 条目交给 skillmgr 接管。
（职责链交接，不是执行指令）

<small>`events.go:59`</small>

### `EventNameToolsReady`

```go
const EventNameToolsReady
```

EventNameToolsReady：全部插件加载完、工具注册齐（只读订阅）。

<small>`events.go:61`</small>

### `EventType`

```go
type EventType string
```

EventType identifies the kind of system event.

<small>`plugin.go:494`</small>

### `PluginSDK.Events`

!!! warning "仅内核内置插件可用"
    实测全仓 SetEventSubscriber 只有定义、无任何调用点（grep -rn SetEventSubscriber --include=*.go 仅命中 sdk/plugin.go 的定义与 lua_plugin.go 的一句注释）。外部插件拿到的 Events() 恒为 nil。外部插件订阅事件走的是桥接运行时的 events.subscribe RPC（proc_main.go.tmpl:1421 在插件侧分发、内核 protocol.go MethodEventsSubscribe），Lua 插件则走内部 SDK 的 Subscribe。这正是 PLUGIN_DEV.md 里没有说清的一处。

```go
func (s *PluginSDK) Events() EventSubscriber
```

Events returns the event subscriber for listening to kernel events (may be nil if not available).

<small>`plugin.go:750`</small>

