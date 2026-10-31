# 实时通道（rt 流）

> 用于**实时通话**场景：输入不断覆盖，输出连续流式。参考实现见
> [`example/rtvoice`](../examples/index.md)。

## 它解决什么问题

普通 inputch 的语义是**排队**：每条输入都会被处理、不丢。但通话不是这样 ——
用户说话时，前一句还没处理完就在说下一句。此时：

```
普通 inputch：排队 ⇒ 攒一批旧话，agent 回答的是过期的内容
rt 通道    ：覆盖 ⇒ 只有最新一句有效，旧话作废
```

所以 rt 流的核心契约是 **「新值作废旧值」**，而不是「更快地排队」。

## 三层声明，缺一不可

| 层 | 谁声明 | 形态 |
|---|---|---|
| **通道能承载 rt 流** | 插件 | `sdk.ChannelDef{Realtime: true}` |
| **目标 agent 是 rtchild** | 调用方（内核侧） | `AgentConfig.IsRealtime` |
| **两侧成对划拨** | 调用方（内核侧） | `SpawnRTChild` 同时校验 input 与 output |

插件侧只需要做第一件。后两件由内核在创建 rtchild 时校验。

### 通道声明

```go
// inputch：采集层往里灌"已经削减过的事件"
if err := s.RegisterInputChannel("rtvoice-in", sdk.ChannelDef{
    Realtime: true,
    // rt 流不进记忆：通话的每一句都进记忆会把图谱灌满噪声
    NoMemory: true,
}); err != nil {
    return err
}

// outputch：内核把 rt 文本写进它
if err := s.RegisterOutputChannel("rtvoice", 1, "实时通话输出（文本流）",
    sdk.ChannelDef{Realtime: true},
    func(args map[string]interface{}) (interface{}, error) {
        // args["text"] 是本轮产出的文本片段
        return nil, nil
    },
); err != nil {
    return err
}
```

## 三条容易误解的地方

### 1. `outputch(rt)` 只产出**文本**，不是音频

`CapRT` 是**通道侧**「我能承载 rt 流」的声明，**不表示内核要产出音频**。
怎么展示（含 TTS、播放）是插件自己的事。

所以 `RegisterOutputChannel` 的第二个参数（能力位）取 `1`（`CapText`）即可 ——
不需要声明音频能力。

### 2. `Realtime` 标记**不是准入门槛**

不带本标记的普通通道**同样可以**划给 rtchild，只是那条通道不能承载 rt 流。
这是刻意的：rtchild 也可能需要普通通道（比如接收一个文件）。

### 3. 必须**成对**给出 input 与 output

`SpawnRTChild` 要求两者都给，且两者都必须是 `Realtime: true` 的通道。原因：

| 只给一半 | 后果 |
|---|---|
| 只给 output | rtchild 能说话，但**永远听不到用户** ⇒ 表现为"它自言自语，然后静默待机"，且不报错 |
| 只给 input | rtchild 能听，但**输出无处可去** ⇒ 用户听到长时间静默 |

两种都是**坏掉一半**，而不是"简化"。

## 内核侧的对应工具

插件把通道准备好之后，主 agent 可以用 `rt_children` 工具创建通话：

```
rt_children(action="create", id="call-1", inputch="rtvoice-in", outputch="rtvoice")
rt_children(action="send", id="call-1", text="用户说的话")   # 会立即打断当前轮
rt_children(action="stop", id="call-1")                      # 回收并交接上下文
```

`send` 是**打断式**的：它取消当前生成轮，用新输入重开。这与"排队等它说完"
是相反语义 —— 详见下文。

## rt 与中断阶梯的关系（重要）

内核有四级中断优先级（L1–L4）与抢占冷却，那是为**任务型**输入设计的，
目标函数是「公平、不饥饿」。

rt 流**不走**这套阶梯，而是用**单槽覆盖**。原因：

```
四级中断：连续两条同级别输入 ⇒ 第二条排队、等冷却（2s）
rt 流   ：同一时间内只有最新一句有效，其余作废
```

若 rt 复用中断阶梯，量化后果是：攒批 250ms ⇒ 每秒约 4 次拐点 ⇒
2s 冷却会让 **~75% 的打断静默降级为入队**。而失败方式是隐蔽的 ——
日志只有一行 `interrupt queued ... preempt cooldown`。

两者目标相反，**不该共用机制**。

## 能力边界

rt 通道的划拨是**内核侧动作**，插件只能声明能力，不能自己决定划给谁。
这与其它通道一致：插件注册、内核分配。

如果你的通道被划给了 rtchild，插件能观察到的只有：

- `output` handler 被调用（每轮产出文本时）
- 注入的输入被消费或被覆盖（覆盖是正常行为，不是丢包）

## 相关文档

- [`api/channels.md`](../api/channels.md) — 通道 API 参考（含 `ChannelDef` 全字段）
- [`api/lifecycle.md`](../api/lifecycle.md) — 注册时机
- [`example/rtvoice`](../examples/index.md) — 完整参考实现（含采集侧对接）
