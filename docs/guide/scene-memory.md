# 场景记忆（Scene Memory）

> 场景式记忆是内核 v1.3 起的能力。它不新增 API 面，只影响**你的输入被怎样记住与取回**。
> 与 `NoMemory` / `ContextPolicy` / `RecallPolicy` 并列为第四项声明：`ScenePolicy`。

## 它解决什么问题

三层记忆按**字面相关性**召回：你得说出相近的词，记忆才会被取回来。
场景记忆补上另一半：按**场合**召回。

同一场合再次出现时，当时挂在这个场合上的约定、偏好、人物关系会自动回来——
与这次说了什么措辞无关。

```
你：以后在群里回消息简短点
    └─ 这条记忆挂到场面「chan:qq + peer:group_xxx」上

一周后，同一个群里有人问「上次说的格式是什么」
    └─ 场面重现（还没等你提到「格式」），那条约定已经被取回
```

## 场面是自己长出来的

场景**不需要声明**。每轮交互，内核采集一组可观察信号当这轮���「场面指纹」：

| 特征 | 来源 | 权重 | 说明 |
|---|---|---|---|
| `chan` | 输入通道名 | 1.0 | 最强的同一性信号 |
| `peer` / `peer_group` | 注入点给的 `payload` 里的 `group_id`/`user_id`/`chat_id` 等 | 1.0 | 群与私聊分开，避免互相命中 |
| `tool` | 触发这一步的工具名 | 0.8 | 行为信号 |
| `topic` | 清洗后输入的内容词 | 0.4 | 软信号，同场面的不同话题不该被拆开 |
| `part` | 时段（夜间/上午/下午/晚间） | 0.2 | 最弱，只做辅助 |

指纹反复重合时，一场场面就成形了。相似度按**加权 Jaccard** 算
（共享特征的权重和 ÷ 并集的权重和）——不加权的话，一次偶然的话题重合
会把两个不同场面并成一个。

**同类场面出现第二次才被认定。** 一次性的交互不建场面：
那不是「场面」，建了只会让图库被一次性事件撑满。

## 声明你的参与姿态

```go
sdk.ChannelDef{
    ScenePolicy: sdk.ScenePolicyNone,  // 这条通道不参与场面识别
}
```

或单次注入覆盖：

```go
sdk.InjectOptions{
    ScenePolicy: sdk.ScenePolicyNone,
}
```

| 取值 | 含义 |
|---|---|
| `""`（空）/ `ScenePolicyAuto` | **参与**（默认，保持既有行为） |
| `ScenePolicyNone` | **不参与**：不产任何场面指纹，也不派生场景键 |

**默认是参与而不是不参与**，与 `ContextPolicy` 刻意相反。原因是场景只
**附加**检索路径、不改记忆本体，默认关会让存量通道突然失去场景召回；
而「关」是少数意图（纯内部信号）。

声明 `none` 之后连时段特征都不产——一个不参与的门面不该在场面索引里
留下任何足迹。

### 谁该考虑关掉

内核自循环（`system`）、心跳（`timer`）、内部状态汇报（`kernel`）这类
纯内部信号。它们每次触发都在撑一个场面，会把不相干的交互聚到一起。

反过来说，**多标一个通道通常没有代价**：一个没人往上面写记忆的场面，
召回时返回空。关不关都不影响正确性——所以拿不准时，默认参与就好。

## 怎么给场面命名

场景键有两种来源：

**通道派生（默认）**——`evt.Source` 派生出 `chan:qq` 这类键。你不用管。

**显式声明（进阶）**——在注入时给出更有语义的键：

```go
p.sdk.InjectInterruptTextOpts("qq", "qq", text, sdk.InjectOptions{
    ScenePolicy: sdk.ScenePolicyAuto,
})
```

也可以通过 `payload["scene"]` 传层级键（支持 `string` / `[]string` /
`[]interface{}` 三种形态）：

```go
"chan:qq/peer:group_1027"
```

召回走**前缀匹配**（`chan:qq` 能覆盖 `chan:qq/peer:xxx`），用 `/` 兜底
以免 `chan:qq` 误吞 `chan:qq2` 这种同前缀但不同层的场景。

## 场面记忆不改变什么

- **不改记忆本体**：场景是记忆的**附加索引**，删掉场景不删记忆。
- **不让模型负责**：`stablemem_commit` 的 `scene` 留空即可，内核会挂到本轮
  解析出的场面上。留空是安全的一侧——猜错的场面会把无关记忆钉死。
- **不影响同步通道**：`webui` / `cli` / 终端走 `ResponseCh`，不经
  `output_send__*`，与场面无关。

## 相关 API

- `ChannelDef.ScenePolicy` —— 通道级声明（见 [输入/输出通道](../api/channels.md)）
- `InjectOptions.ScenePolicy` —— 单次注入覆盖（见 [其他类型](../api/misc.md)）
- `ScenePolicyAuto` / `ScenePolicyNone` / `ValidScenePolicy` —— 常量与校验
