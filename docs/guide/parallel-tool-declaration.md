# 工具并发声明：`ParallelSafe` / `Serial`

> 对应 `sdk.ToolDef` 的两个字段。内核在**同一轮**收到多个 `tool_call` 时，
> 依据它们决定并发还是整批串行。
>
> 状态：已随 2026-09 的并行内核落地并在生产启用。

## 1. 为什么是"保守 opt-in"

**默认整批串行。** 只有当**批内每一个**工具都显式声明 `ParallelSafe: true`
时，那一批才并发；**只要有一个不声明，整批退回串行**。

这不是"漏了声明导致退化"的将就，而是刻意的设计：

- 存量插件**不改一行**就得到保守行为（整批串行），不会被升级意外并发
- 声明是**责任**而非特权 —— 声明者必须自己确认线程安全
- 宁可慢，不可错：一次错误的并发可能让两个工具抢同一个 SQLite 写、
  同一台设备、或同一个输出通道

```go
// 同批全是安全工具 → 并发
tools: [a(ParallelSafe), b(ParallelSafe)]  ⇒ 并发

// 只要有一个没声明 → 整批串行
tools: [a(ParallelSafe), b(默认)]          ⇒ 串行
```

## 2. 三个条件都满足才可以声明 `ParallelSafe`

1. **handler 自身线程安全** —— 不持有跨调用的可变状态
2. **不与同批其它工具争抢同一资源** —— SQLite 写、设备、同一输出通道
3. **执行顺序无关** —— 顺序敏感的工具应留 `false`，由内核保序

第 3 条常被忽略：内核能保证**用户可见的消息**按声明顺序落盘，但**工具
之间的实际执行先后**在并发模式下不确定。有顺序依赖就留 `false`。

## 3. `Serial`：显式的反向标记

```go
Serial bool `json:"serial,omitempty"`
```

`ParallelSafe` 的零值 `false` 已经表达"串行"，插件**无法区分**：

- "我没想过"
- "我确认过**必须**串行，且有原因"

一旦工具作者需要把"这里**故意**串行，是有原因的"写进代码（而不只是没填），
这个区分就是必需的 —— 否则只能靠命名约定传递意图。

适用场景：读操作但有隐含顺序约束（终端 `read`/`resize` 这类共享会话
状态）；写操作虽已加锁但需要串行以获得可预测的交错顺序。

**优先级：`Serial` 胜出。** 即使同时写了 `ParallelSafe: true`，`Serial`
仍然生效 —— 显式声明"必须串行"不允许被 `ParallelSafe` 或任何默认值覆盖。

## 4. 写法

声明字段放在 `ToolDef` 结构体的**末尾**，遵循既有 `NoMemory` 的风格：

```go
sdk.RegisterTool(sdk.ToolDef{
    Name:        "my_readonly_query",
    Description: "……",
    Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
    Handler:     h.query,
    ParallelSafe: true,   // 声明在末尾
}, s)
```

需要"故意串行"时：

```go
sdk.RegisterTool(sdk.ToolDef{
    Name:    "my_terminal_input",
    Handler: h.input,
    Serial:  true,        // 胜出，忽略 ParallelSafe
}, s)
```

## 5. 内置工具

核心仓的内置工具用 `toolDefOptions` / `parallelOpts()` 声明
（`internal/agent/core/tooldefs.go`），最终由 `buildToolDefs` 汇总成
`ToolDef.ParallelSafe`。

内核只提供并行调度基础设施，**不硬编码任何工具名的安全状态表** ——
状态由每个工具在自己的声明结构里给出，查询时走聚合表。

## 6. 效果（实测）

同批 N 个各约 250ms 的工具，新版内核"内部并发"对"强制串行"：

| N | 加速比 |
| --- | --- |
| 2 | ~1.41× |
| 4 | ~2.22× |
| 8 | ~3.88× |

批耗时几乎不随 N 增长，串行批严格线性。

> 压测时**必须同时记录实际执行的工具数**，不能只看耗时。
> 某次对照中旧版本耗时更短、但因适配器缺 `stream_index` 导致
> **实际处理 0 个工具** —— 那不是性能提升，是全失败。

## 相关

设计背景与踩坑见**核心仓**文档（不在本仓）：

- `docs/zh/toolcall-contract-and-sequence-design.md` —— 契约与序列设计
- `docs/zh/toolcall-contract-and-sequence-design.md` —— 契约 + **附录 A 实测压测数据**
- `docs/zh/deploy-runbook.md` —— 生产部署（含适配器 `stream_index` 相关）
