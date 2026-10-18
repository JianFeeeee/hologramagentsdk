---
name: rss-tools
description: RSS/Atom 订阅与更新推送。本插件**常驻自启**：它定时轮询订阅源，新内容要主动推送。
---

# RSS 订阅

## 本插件为什么常驻

RSS 插件每 **N 分钟轮询**所有订阅源。发现新文章后**主动注入**给 agent，
由 agent 决定要不要告诉用户。

改成按需加载 ⇒ 没人轮询 ⇒ **新文章永远不会被发现**，而且不报错。

## 工具

| 工具 | 用途 |
|---|---|
| `rss_subscribe` | 订阅一个源 |
| `rss_unsubscribe` | 取消订阅 |
| `rss_list` | 列出所有订阅 |
| `rss_check_now` | 立刻检查一次（不等下一次轮询） |

## 注意

- 轮询间隔可在插件配置里调（默认 30 分钟）。
- 订阅源失效会记日志但不会删订阅，需要时用 `rss_list` 看状态。

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
