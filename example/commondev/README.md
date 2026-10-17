# commondev

commondev plugin

## Build

```bash
hmapdev build
```

## Channels

入站与出站是分开登记的两件事：

| 方向 | API | 用途 |
|---|---|---|
| 入站 inputch | RegisterInputChannel(name, def) | 声明「谁会往这个通道注入输入」。**凡是用 InjectText*/InjectInput*/InjectInterrupt*(source, "<name>", ...) 注入的通道名都要登记** |
| 出站 output | RegisterOutputChannel(name, caps, desc, def, handler) | 声明 output_send__<name> 的回复发给谁；handler 收到 {payload,type,meta} |

def（ChannelDef）描述该通道在记忆计算层的行为：NoMemory: true = 该通道输入不进记忆；
Cleaner = 计算层清洗后再向量化/提关键词（原文不改）。

> 只登记出站通道、却用同名通道注入输入时，内核会兜底登记同名 inputch 并在日志里告警。
> 兜底只为兼容老插件 —— 请显式登记，让「这是入站通道」成为插件的明确意图。

## Install

Upload the .hmap file through the Plugin Manager API.
