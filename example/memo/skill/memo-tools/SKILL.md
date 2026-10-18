---
name: memo-tools
description: 待办与备忘录（到期主动提醒）。本插件**常驻自启**：它定时检查待办到期。
---

# 待办与备忘录

## 本插件为什么常驻

memo 插件有一个 **5 分钟的 ticker**，检查有没有到期的待办，到点主动提醒。

改成按需加载 ⇒ 没有 ticker ⇒ **待办到期不提醒**，
而用户设待办就是为了被提醒。

## 工具

| 工具 | 用途 |
|---|---|
| `memo_todo_add` | 加待办（可带截止时间） |
| `memo_todo_complete` | 完成待办 |
| `memo_todo_list` | 列出待办 |
| `memo_todo_delete` | 删除待办 |
| `memo_create` / `memo_list` / `memo_delete` | 备忘录增删查 |

## 注意

待办 vs 备忘录的区别：**待办**有完成状态、会提醒；**备忘录**只是记着。
用户说"提醒我"时用待办，说"记一下"时用备忘录。

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
