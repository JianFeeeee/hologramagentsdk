<!-- 本页由 tools/apidoc/gensite 从源码生成，请勿手改；要改文档就改 sdk/*.go 的注释。 -->

# 工具（Tools）

注册 LLM 可调用的工具。工具是插件最主要的能力形态：模型看到 `ToolDef` 的说明后决定是否调用，调用时执行你的 `ToolHandler`。

### `ContentBlock`

```go
type ContentBlock struct { Type string `json:"type"` Text string `json:"text,omitempty"` ImageURL *ImageURL `json:"image_url,omitempty"` AudioURL *AudioURL `jso …
```

ContentBlock 是多模态内容块（OpenAI 格式：text/image_url/audio_url）。
插件工具返回结果时可用 PluginSDK.SetToolBlocks 注入，让下一轮 LLM
请求在 tool message 的 content 数组里带上图片/音频，实现"模型看图/听音频"。

<small>`plugin.go:1086`</small>

### `PluginSDK.RegisterTool`

```go
func (s *PluginSDK) RegisterTool(name string, def ToolDef, handler ToolHandler) error
```

RegisterTool registers a tool that the LLM can call.

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`a2a`](../examples/index.md#a2a) | `example/a2a/plugin.go:76` | `s.RegisterTool(tp+"a2a_query", sdk.ToolDef{` |
| [`acp`](../examples/index.md#acp) | `example/acp/plugin.go:70` | `s.RegisterTool(tp+"acp_query", sdk.ToolDef{` |
| [`ai_image`](../examples/index.md#ai_image) | `example/ai_image/plugin.go:159` | `s.RegisterTool(tp+"generate", sdk.ToolDef{` |
| [`bili`](../examples/index.md#bili) | `example/bili/plugin.go:85` | `s.RegisterTool(tp+"video", sdk.ToolDef{` |

<small>`plugin.go:646`</small>

### `ToolCall`

```go
type ToolCall struct { ID string `json:"id"` Name string `json:"name"` Plugin string `json:"plugin,omitempty …
```

ToolCall represents a model's request to call a tool.

<small>`plugin.go:257`</small>

### `ToolDef`

```go
type ToolDef struct { Name string `json:"name"` Plugin string `json:"plugin,omitempty"` Description string …
```

ToolDef describes a tool that the plugin exposes.

<small>`plugin.go:309`</small>

### `ToolHandler`

```go
type ToolHandler func(args map[string]interface{}) (interface{}, error)
```

ToolHandler is a function that handles a tool call.

<small>`plugin.go:20`</small>

### `ToolResult`

```go
type ToolResult struct { CallID string `json:"call_id"` Name string `json:"name"` Plugin string `json:"plugin,omitempty"` Success bool `json:"suc …
```

ToolResult represents the result of a tool call.

<small>`plugin.go:265`</small>

