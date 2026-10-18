---
name: sanitizer-tools
description: 清洗 LLM 输出里的工具调用残留（思维泄漏）。本插件**常驻自启**：它注册的是 stage 钩子，不解包任何工具。
---

# 输出清洗

## 本插件为什么常驻

sanitizer **没有工具**，它注册三个 stage 钩子：

| 钩子 | 时机 | 作用 |
|---|---|---|
| `StageOnInput` | 输入时 | 清理输入里的协议残留 |
| `StageAfterToolcall` | 工具调用后 | 去掉工具结果的格式噪声 |
| `StagePostAction` | 动作后 | 清洗即将发出去的回复 |

stage 钩子**必须在每一轮**都生效。停掉它不影响"能不能用"，
但会让脏输出漏给用户（模型把内部工具调用语法当正文吐出来）。

所以它属于"基础设施型"插件：既不占工具槽位，也不能按需加载。

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
