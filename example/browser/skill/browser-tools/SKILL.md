---
name: browser-tools
description: 统一浏览器能力（搜索/HTTP 抓取/无头渲染/CDP 交互）。本插件**常驻自启**：它有 ticker 与长命令回调，会主动注入。
---

# 浏览器

## 本插件为什么常驻

browser 插件有两处**主动注入**：

1. **30 秒 ticker** —— 检查长任务（渲染、下载）的进度
2. **长时间命令的完成回调** —— 命令跑完后把结果注入回来

改成按需加载 ⇒ 长任务还在跑、插件却被卸载 ⇒ 结果无人接收，
或者在跑的任务变成孤儿进程。

## 工具

| 组 | 工具 |
|---|---|
| 轻量 | `search` `fetch`（HTTP 抓取，快） |
| 渲染 | `render` `html`（无头浏览器，慢） |
| 交互 | `start` `navigate` `screenshot` `click` `type` `scroll` `close` |
| 环境 | `install`（装浏览器依赖） |

## 选哪个

| 需求 | 用 |
|---|---|
| 只要页面文字 | `fetch`（最快，不执行 JS） |
| 页面靠 JS 渲染 | `render` |
| 要点击/填表/截图 | `start` → `navigate` → 交互 → `close` |

## 注意

- 交互式会话**用完必须 `close`**，否则浏览器进程一直占内存。
- `install` 只在缺依赖时用，不要每次都跑。

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
