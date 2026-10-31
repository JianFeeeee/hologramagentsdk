<!-- 本页由 tools/apidoc/gensite 从源码生成，请勿手改；要改文档就改 sdk/*.go 的注释。 -->

# 阶段钩子（Stages）

在消息处理管道的固定点位插入自己的逻辑。阶段比工具更底层：工具是模型主动调用的，阶段是流程经过时必然触发的。

### `StageContext.IsResponded`

```go
func (c *StageContext) IsResponded() bool
```

<small>`plugin.go:342`</small>

### `StageContext.Lock`

```go
func (c *StageContext) Lock()
```

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`rtvoice`](../examples/index.md#rtvoice) | `example/rtvoice/plugin_test.go:29` | `r.mu.Lock()` |

<small>`plugin.go:340`</small>

### `StageContext.RLock`

```go
func (c *StageContext) RLock()
```

<small>`plugin.go:338`</small>

### `StageContext.RUnlock`

```go
func (c *StageContext) RUnlock()
```

<small>`plugin.go:339`</small>

### `PluginSDK.RegisterStage`

```go
func (s *PluginSDK) RegisterStage(stage Stage, handler StageHandler, scope ...StageScope)
```

RegisterStage registers a handler for a pipeline stage.

	scope: StageScopeGlobal (default) — receives all stage events.
	       StageScopeOwnTools — only before_toolcall/after_toolcall for this plugin's tools.

<small>`plugin.go:802`</small>

### `Stage`

```go
type Stage string
```

Stage represents a point in the message processing pipeline.

<small>`plugin.go:60`</small>

### `StageContext`

```go
type StageContext struct { mu sync.RWMutex RawMessage string UserID string GroupID string ContextMsgs []map[string]interface{} L …
```

StageContext provides context for stage handlers.

<small>`plugin.go:318`</small>

### `StageHandler`

```go
type StageHandler func(ctx *StageContext) error
```

StageHandler is a function that handles a pipeline stage event.

<small>`plugin.go:57`</small>

### `StageRegistrar`

```go
type StageRegistrar func(stage Stage, handler StageHandler)
```

StageRegistrar registers a stage handler.

<small>`plugin.go:582`</small>

### `StageScope`

```go
type StageScope int
```

StageScope controls which events a stage handler receives.

<small>`plugin.go:568`</small>

### `StageContext.Unlock`

```go
func (c *StageContext) Unlock()
```

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`rtvoice`](../examples/index.md#rtvoice) | `example/rtvoice/plugin_test.go:31` | `r.mu.Unlock()` |

<small>`plugin.go:341`</small>

