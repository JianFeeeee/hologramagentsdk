<!-- 本页由 tools/apidoc/gensite 从源码生成，请勿手改；要改文档就改 sdk/*.go 的注释。 -->

# 阶段钩子（Stages）

在消息处理管道的固定点位插入自己的逻辑。阶段比工具更底层：工具是模型主动调用的，阶段是流程经过时必然触发的。

### `StageContext.IsResponded`

```go
func (c *StageContext) IsResponded() bool
```

<small>`plugin.go:243`</small>

### `StageContext.Lock`

```go
func (c *StageContext) Lock()
```

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`a2a`](../examples/index.md#a2a) | `example/a2a/plugin.go:158` | `p.sessMu.Lock()` |
| [`acp`](../examples/index.md#acp) | `example/acp/plugin.go:128` | `p.srvMu.Lock()` |
| [`bili`](../examples/index.md#bili) | `example/bili/plugin.go:37` | `p.runMu.Lock()` |
| [`browser`](../examples/index.md#browser) | `example/browser/plugin.go:450` | `p.mu.Lock()` |

<small>`plugin.go:241`</small>

### `StageContext.RLock`

```go
func (c *StageContext) RLock()
```

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`acp`](../examples/index.md#acp) | `example/acp/plugin.go:267` | `p.mu.RLock()` |
| [`calendar`](../examples/index.md#calendar) | `example/calendar/plugin.go:650` | `p.mu.RLock()` |
| [`memo`](../examples/index.md#memo) | `example/memo/plugin.go:224` | `p.mu.RLock()` |
| [`qq`](../examples/index.md#qq) | `example/qq/status.go:569` | `ctx.RLock()` |

<small>`plugin.go:239`</small>

### `StageContext.RUnlock`

```go
func (c *StageContext) RUnlock()
```

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`acp`](../examples/index.md#acp) | `example/acp/plugin.go:273` | `p.mu.RUnlock()` |
| [`calendar`](../examples/index.md#calendar) | `example/calendar/plugin.go:651` | `defer p.mu.RUnlock()` |
| [`memo`](../examples/index.md#memo) | `example/memo/plugin.go:229` | `p.mu.RUnlock()` |
| [`qq`](../examples/index.md#qq) | `example/qq/status.go:570` | `defer ctx.RUnlock()` |

<small>`plugin.go:240`</small>

### `PluginSDK.RegisterStage`

```go
func (s *PluginSDK) RegisterStage(stage Stage, handler StageHandler, scope ...StageScope)
```

RegisterStage registers a handler for a pipeline stage.

	scope: StageScopeGlobal (default) — receives all stage events.
	       StageScopeOwnTools — only before_toolcall/after_toolcall for this plugin's tools.

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`cmd`](../examples/index.md#cmd) | `example/cmd/plugin.go:221` | `s.RegisterStage(sdk.StageBeforeToolcall, func(ctx *sdk.StageContext) error {` |
| [`memo`](../examples/index.md#memo) | `example/memo/plugin.go:152` | `s.RegisterStage(sdk.StagePreAction, p.stagePreAction)` |
| [`qq`](../examples/index.md#qq) | `example/qq/plugin.go:940` | `s.RegisterStage(sdk.StageOnInput, p.onInputAuthContext, sdk.StageScopeGlobal)` |
| [`sanitizer`](../examples/index.md#sanitizer) | `example/sanitizer/plugin.go:52` | `s.RegisterStage(sdk.StageOnInput, func(ctx *sdk.StageContext) error {` |

<small>`plugin.go:660`</small>

### `Stage`

```go
type Stage string
```

Stage represents a point in the message processing pipeline.

<small>`plugin.go:26`</small>

### `StageContext`

```go
type StageContext struct { mu sync.RWMutex RawMessage string UserID string GroupID string ContextMsgs []map[string]interface{} L …
```

StageContext provides context for stage handlers.

<small>`plugin.go:219`</small>

### `StageHandler`

```go
type StageHandler func(ctx *StageContext) error
```

StageHandler is a function that handles a pipeline stage event.

<small>`plugin.go:23`</small>

### `StageRegistrar`

```go
type StageRegistrar func(stage Stage, handler StageHandler)
```

StageRegistrar registers a stage handler.

<small>`plugin.go:483`</small>

### `StageScope`

```go
type StageScope int
```

StageScope controls which events a stage handler receives.

<small>`plugin.go:469`</small>

### `StageContext.Unlock`

```go
func (c *StageContext) Unlock()
```

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`a2a`](../examples/index.md#a2a) | `example/a2a/plugin.go:164` | `p.sessMu.Unlock()` |
| [`acp`](../examples/index.md#acp) | `example/acp/plugin.go:129` | `defer p.srvMu.Unlock()` |
| [`bili`](../examples/index.md#bili) | `example/bili/plugin.go:40` | `p.runMu.Unlock()` |
| [`browser`](../examples/index.md#browser) | `example/browser/plugin.go:455` | `p.mu.Unlock()` |

<small>`plugin.go:242`</small>

