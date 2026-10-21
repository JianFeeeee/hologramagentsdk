<!-- 本页由 tools/apidoc/gensite 从源码生成，请勿手改；要改文档就改 sdk/*.go 的注释。 -->

# LLM 调用

让插件自己调用模型（而不是只等模型来调你）。

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

### `PluginSDK.LLM`

```go
func (s *PluginSDK) LLM() LLMAPI
```

LLM returns the LLM provider API (may be nil if not available).

<small>`plugin.go:615`</small>

