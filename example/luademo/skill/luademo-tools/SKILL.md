---
name: luademo-tools
description: Lua 插件全功能示例（工具 + 阶段钩子 + 通道 + 数据 API）。本插件**常驻自启**：它注册 stage 钩子做示范。
---

# Lua 插件示例（luademo）

## 这个插件是什么

**示例工程**，不是实用工具。它演示 Lua 插件能做的四件事：

1. 注册工具（`no_memory` / `cleaner` 两种形态）
2. 注册阶段钩子
3. 注册通道
4. 使用数据类 API

## 本插件为什么常驻

它注册了 stage 钩子（与 sanitizer 同理，钩子要每轮生效）。
若只是想看代码示例，可以关掉；若在**验证 Lua 适配层**，必须让它常驻跑着。

```
plugin_stop  name=luademo    # 仅看代码时
plugin_start   name=luademo    # 验证 Lua VM / 钩子时
```

## 开发者看这里

读 `example/luademo/` 的源码 + 本文件即可上手写自己的 Lua 插件。
Lua 侧能力边界由内核 `FacilityLua`（Lua 协议适配 VM）决定。

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
