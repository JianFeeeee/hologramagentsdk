# 流式多 `tool_call`：适配器必须透传 `index`

> 面向在 Lua 里写适配器（`transform_stream_chunk`）的插件作者。
>
> 状态：已随 2026-09 的并行内核落地；生产 `openai.lua` 等适配器已修复。

## 1. 问题

OpenAI 兼容的流式响应里，同一轮的多个 `tool_call` 以**分片**形式到达，
靠 `index` 字段区分归属：

```
data: {"choices":[{"delta":{"tool_calls":[
        {"index":0,"id":"call_a","function":{"name":"alpha","arguments":""}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[
        {"index":1,"id":"call_b","function":{"name":"beta","arguments":""}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[
        {"index":0,"function":{"arguments":"{\"x\":1}"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[
        {"index":1,"function":{"arguments":"{\"y\":2}"}}]}}]}
```

**每个 SSE chunk 通常只含一个 `tool_call` 元素。** 内核按 `index` 分桶累积
`id` / `name` / `arguments`。

## 2. 适配器必须做的事

`transform_stream_chunk` 的输出 JSON 里，每个 tool call 分片都要带
**`stream_index`**（值取上游的 `index`）：

```lua
table.insert(tcs, {
    id = tc.id or "",
    type = tc.type or "function",
    name = name,
    raw_arguments = raw_args,
    -- ★ 必须透传上游 index（键名是 stream_index，不是 index）。
    --   内核按 stream_index 分桶累积同一轮多个 tool_call 的分片。
    stream_index = tc.index or 0
})
```

### ★ 键名是 `stream_index`，不是 `index`

内核的 `ToolCall.StreamIndex` 标签是 `json:"stream_index"`：

```go
StreamIndex int `json:"stream_index,omitempty"`
```

写成 `index` 会被 Go 的解码器**静默丢弃**（无匹配字段），
`StreamIndex` 恒为 0 ⇒ 全部落进 `accs[0]`。

## 3. 不透传的实际后果

不是"少个字段"，而是**多工具并行调用整体失效**：

| 现象 | 原因 |
| --- | --- |
| `name` 相互覆盖 | 全进 `accs[0]`，后写的赢 |
| `arguments` 碎片混拼 | 两个工具的 JSON 片段交错拼接 |
| 报"参数不是合法 JSON" | 上面拼接的产物解析失败 |
| 工具被当成**空参数**调用 | 同上 |

2026-09-27 的对照压测里，旧适配器耗时**更短**但**实际处理 0 个工具** ——
每个工具都因参数非法失败。⇒ 压测**必须同时统计实际执行数**，不能只看耗时。

## 4. 还有两个容易踩的点

**① 不能按 `name` 过滤分片**

```lua
-- ✗ 错：后续块的 name 为空但携带 arguments
if tc.function and tc.function.name then ... end

-- ✓ 对：无 name 但有 arguments 的分片也要收，累积时再校验 name
```

**② 扁平结构 + `stream_index`**

部分协议族（`server` / `kimicode` / `anthropic` / `ollama`）的流式 tool call
是**扁平**结构（`name`/`arguments` 直接在 `tc` 上，不在 `tc.function` 里），
同样要带 `stream_index`。

## 5. 自检

```bash
# 1) 适配器是否透传
grep -n "stream_index" /home/newqqagent/adapters/<你的>.lua

# 2) 实测：发一个同轮多工具的请求，看是否两个都真被执行
#    内核日志里 executing tool 应出现两次（可能并发）
journalctl -u homeagent.service --since "-2 min" | grep "executing tool"

# 3) 有没有参数解析失败
journalctl -u homeagent.service --since "-2 min" | grep -E "参数|合法 JSON"
```

> `gemini.lua` 目前**没有**流式 tool call 实现，因此不涉及本条。
> Gemini 协议是 `functionCall` 而非 `tool_calls`，不能照搬 OpenAI 的做法。

## 相关

- `docs/guide/parallel-tool-declaration.md` —— 并发声明 `ParallelSafe`/`Serial`
- 核心仓 `docs/zh/toolcall-contract-and-sequence-design.md` —— 契约、协议族清单、**附录 A 压测数据**
