---
name: acp-tools
description: Agent Client Protocol 通信（服务端 + 客户端）。本插件**常驻自启**：它在监听外部 ACP 请求。
---

# ACP 通信

## 本插件为什么常驻

acp 插件在 **TCP 端口上监听** ACP 客户端的 session/new 等请求。

改成按需加载 ⇒ 端口不监听 ⇒ 外部调用方（如编辑器、其他 agent 框架）
连不上，而本机无任何记录。

## 工具

| 工具 | 用途 |
|---|---|
| `acp_query` | 以 ACP 客户端身份向对端发起请求 |
| `acp_configure` | 配置对端 |
| `acp_status` | 查监听与对端状态 |

## 注意

与 a2a 的区别：A2A 是 Agent 对 Agent 的任务协议，ACP 是
"客户端驱动 agent 会话"的协议（更像 IDE 集成）。两者可能同时在跑。

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
