---
name: a2a-tools
description: Agent-to-Agent 协议通信（TCP 服务端 + 客户端）。本插件**常驻自启**：它在监听外部 Agent 请求。
---

# A2A 通信

## 本插件为什么常驻

a2a 插件在 **TCP 端口上监听**其他 Agent 发来的 Tasks/Send 请求，收到后注入给本 agent 处理。

改成按需加载 ⇒ 端口不监听 ⇒ **外部 Agent 的请求无人应答**，
对方只会看到连接失败或超时，而本机**毫无记录**。

## 工具

| 工具 | 用途 |
|---|---|
| `a2a_query` | 向其他 Agent 发请求 |
| `a2a_discover` | 发现可用的对端 |
| `a2a_configure` | 配置对端地址 |
| `a2a_restart` | 重启监听 |
| `a2a_status` | 查监听状态与已知对端 |

## 注意

- 监听地址/端口在插件配置里。端口冲突时 `a2a_status` 会明确报错。
- 出站 `a2a_query` 需要能连上对端，网络不通时明确报错，不要重试到超时。

## ★ 用 plugin_stop / plugin_start，不要用 plugin_disable / plugin_enable

四个工具看着像同义词，语义差别很大：

| 工具 | 作用 | 跨重启 |
|---|---|---|
| `plugin_stop` | **运行态**关闭，现在停掉 | **不影响**，重启照常加载 |
| `plugin_start` | **运行态**拉起，现在开回来 | 不影响 |
| `plugin_disable` | **持久**禁用 | **会记下来**，开机也不自启 |
| `plugin_enable` | 解除持久禁用 | 会记下来 |

**释放上下文用 `plugin_stop`。** 用 `plugin_disable` 会顺手把"开机自启"
永久关掉 —— 用户下次开机发现插件没了，得再手动开回来。

（本插件声明 `auto_start: false`，所以它本来就不自启；但 `plugin_disable`
仍会写进持久禁用清单，让 `plugin_start` 拒绝拉起，只能再用 `plugin_enable` 解。）
