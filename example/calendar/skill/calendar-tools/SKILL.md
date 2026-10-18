---
name: calendar-tools
description: 日历事件与提醒。本插件**常驻自启**：它定时检查提醒并主动通知。
---

# 日历

## 本插件为什么常驻

日历插件有一个 **30 秒的提醒 ticker**。到点的事件会主动注入给 agent，
让 agent 提醒用户。

改成按需加载 ⇒ 没有 ticker ⇒ **提醒永远不会响**。而"提醒不响"比"没有提醒功能"
更糟：用户以为设了闹钟，实际上什么都没发生。

## 工具

| 工具 | 用途 |
|---|---|
| `calendar_event_add` | 新建事件（可带提醒） |
| `calendar_event_list` | 列出事件 |
| `calendar_event_update` | 修改事件 |
| `calendar_event_delete` | 删除事件 |
| `calendar_today` / `week` / `month` | 按范围查看 |
| `calendar_search` | 搜索事件 |

## 注意

- 时间必须确认**时区**。用户说"明天三点"时，先确认是本地时间。
- 重复事件要明确是每天/每周/每月。

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
