<!-- 本页由 tools/apidoc/gensite 从源码生成，请勿手改；要改文档就改 sdk/*.go 的注释。 -->

# 桥接装配点（Bridge）

以下方法**不是给插件业务代码调的**——它们由 `hmapdev` 生成的运行时在启动时调用，用来把内核能力注入到 SDK 实例。列在这里是为了让「公开 API 面」完整，并说明每个注入点对应什么能力。

### `APIRegistrar`

```go
type APIRegistrar func(name string) error
```

APIRegistrar registers a plugin API for external access.

<small>`plugin.go:585`</small>

### `InputChannelRegistrar`

```go
type InputChannelRegistrar func(name string, def ChannelDef) error
```

InputChannelRegistrar registers an input channel with its memory behavior.

<small>`plugin.go:588`</small>

### `OutputChannelRegistrar`

```go
type OutputChannelRegistrar func(name string, caps int, desc string, def ChannelDef, handler ToolHandler) error
```

OutputChannelRegistrar registers an output channel that the output_send tool can use.

<small>`plugin.go:591`</small>

### `OutputChannelUnregistrar`

```go
type OutputChannelUnregistrar func(name string) error
```

OutputChannelUnregistrar 注销一个输出通道。

为什么需要它：输出通道不止有"启动时注册一次"的静态通道，还有**随外部资源生灭**的
动态通道 —— 典型是远程设备：`device/<id>` 只在设备在线期间存在，设备掉线后
必须注销，否则 output_list_channels 会一直列着它、模型会往一个死通道发消息。

<small>`plugin.go:598`</small>

### `PluginSDK.SetDocMemoryAPI`

!!! info "桥接装配点"
    桥接运行时注入点（由 hmapdev 生成的 proc_main 调用，插件业务代码不调用）

```go
func (s *PluginSDK) SetDocMemoryAPI(dm DocMemoryAPI)
```

<small>`plugin.go:980`</small>

### `PluginSDK.SetEventSubscriber`

!!! warning "仅内核内置插件可用"
    同上：无调用点。标 builtin 而非 bridge，因为连桥接运行时都不注入它——外部插件无法用它获得事件订阅能力。

```go
func (s *PluginSDK) SetEventSubscriber(es EventSubscriber)
```

<small>`plugin.go:1004`</small>

### `PluginSDK.SetIOInjector`

!!! info "桥接装配点"
    桥接运行时注入点（由 hmapdev 生成的 proc_main 调用，插件业务代码不调用）

```go
func (s *PluginSDK) SetIOInjector(io IOInjector)
```

SetIOInjector sets the IO injector (called by the core at startup).

<small>`plugin.go:930`</small>

### `PluginSDK.SetInputChannelRegistrar`

!!! info "桥接装配点"
    桥接运行时注入点（由 hmapdev 生成的 proc_main 调用，插件业务代码不调用）

```go
func (s *PluginSDK) SetInputChannelRegistrar(r InputChannelRegistrar)
```

SetInputChannelRegistrar sets the input channel registrar (called by the core at startup).

<small>`plugin.go:923`</small>

### `PluginSDK.SetKernelControlAPI`

```go
func (s *PluginSDK) SetKernelControlAPI(k KernelControlAPI)
```

SetKernelControlAPI sets the kernel control API (restart/shutdown).
Called by the bridge at startup; nil means "not available".

<small>`plugin.go:1027`</small>

### `PluginSDK.SetKnowledgeAPI`

!!! info "桥接装配点"
    桥接运行时注入点（由 hmapdev 生成的 proc_main 调用，插件业务代码不调用）

```go
func (s *PluginSDK) SetKnowledgeAPI(kn KnowledgeAPI)
```

<small>`plugin.go:986`</small>

### `PluginSDK.SetLLMAPI`

!!! info "桥接装配点"
    桥接运行时注入点（由 hmapdev 生成的 proc_main 调用，插件业务代码不调用）

```go
func (s *PluginSDK) SetLLMAPI(llm LLMAPI)
```

<small>`plugin.go:992`</small>

### `PluginSDK.SetMemoryAPI`

!!! info "桥接装配点"
    桥接运行时注入点（由 hmapdev 生成的 proc_main 调用，插件业务代码不调用）

```go
func (s *PluginSDK) SetMemoryAPI(mem MemoryAPI)
```

<small>`plugin.go:968`</small>

### `PluginSDK.SetOutputChannelRegistrar`

!!! info "桥接装配点"
    桥接运行时注入点（由 hmapdev 生成的 proc_main 调用，插件业务代码不调用）

```go
func (s *PluginSDK) SetOutputChannelRegistrar(r OutputChannelRegistrar)
```

SetOutputChannelRegistrar sets the output channel registrar (called by the core at startup).

<small>`plugin.go:909`</small>

### `PluginSDK.SetOutputChannelUnregistrar`

!!! warning "仅内核内置插件可用"
    同上，桥接模板不注入。

```go
func (s *PluginSDK) SetOutputChannelUnregistrar(r OutputChannelUnregistrar)
```

SetOutputChannelUnregistrar sets the output channel unregistrar (called by the core at startup).

<small>`plugin.go:916`</small>

### `PluginSDK.SetPluginMgrAPI`

!!! info "桥接装配点"
    桥接运行时注入点（由 hmapdev 生成的 proc_main 调用，插件业务代码不调用）

```go
func (s *PluginSDK) SetPluginMgrAPI(pm PluginMgrAPI)
```

SetPluginMgrAPI sets the plugin manager API (called by the bridge at startup).

<small>`plugin.go:1011`</small>

### `PluginSDK.SetRawFilesAPI`

```go
func (s *PluginSDK) SetRawFilesAPI(v RawFilesAPI)
```

SetRawFilesAPI 由内核装配时注入（插件不调用它）。

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`commondev`](../examples/index.md#commondev) | `example/commondev/full_pipeline_test.go:168` | `p.sdk.SetRawFilesAPI(&testRawFiles{root: filepath.Join(root, "rawfile")})` |

<small>`plugin.go:948`</small>

### `PluginSDK.SetSecretsAPI`

```go
func (s *PluginSDK) SetSecretsAPI(v SecretsAPI)
```

SetSecretsAPI 由内核装配时注入（插件不调用它）。

<small>`plugin.go:955`</small>

### `PluginSDK.SetSecurityAPI`

```go
func (s *PluginSDK) SetSecurityAPI(v SecurityAPI)
```

SetSecurityAPI 由内核注入策略声明面的实现（插件不调用）。

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`qq`](../examples/index.md#qq) | `example/qq/security_profile_test.go:53` | `s.SetSecurityAPI(cap)` |

<small>`plugin.go:962`</small>

### `PluginSDK.SetSocialAPI`

!!! info "桥接装配点"
    桥接运行时注入点（由 hmapdev 生成的 proc_main 调用，插件业务代码不调用）

```go
func (s *PluginSDK) SetSocialAPI(social SocialAPI)
```

<small>`plugin.go:998`</small>

### `PluginSDK.SetTextMemoryAPI`

!!! info "桥接装配点"
    桥接运行时注入点（由 hmapdev 生成的 proc_main 调用，插件业务代码不调用）

```go
func (s *PluginSDK) SetTextMemoryAPI(tm TextMemoryAPI)
```

<small>`plugin.go:974`</small>

### `ToolRegistrar`

```go
type ToolRegistrar func(name string, def ToolDef, handler ToolHandler) error
```

ToolRegistrar registers a tool dynamically.

<small>`plugin.go:579`</small>

