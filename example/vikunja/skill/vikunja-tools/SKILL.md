---
name: vikunja-tools
description: 管理 Vikunja 任务/项目/看板（28 个工具）。按需工具：先 plugin_start vikunja，用完可 plugin_stop。
---

# Vikunja 任务管理

## 什么时候用

用户要**建/改任务、管项目看板、打标签、看通知**他们的 Vikunja 时。

## 先拉起插件

```
plugin_start  name=vikunja
```

工具按用途分组（都带 `vikunja_` 前缀）：

| 组 | 工具 |
|---|---|
| 任务 | `tasks` `task_get` `task_create` `task_update` `task_done` `task_delete` `task_bulk` |
| 任务细节 | `task_assignees` `task_labels` `task_comments` `task_relations` `task_attachments` |
| 项目/视图 | `projects` `project_views` |
| 组织 | `labels` `filters` `teams` `sharing` |
| 协作 | `notifications` `subscriptions` `webhooks` `reactions` |
| 其他 | `time_entries` `migrate` `user` `admin` `api` `status` |

## 用完可释放

28 个工具占约 365 tokens。做完任务管理就关掉：

```
plugin_stop  name=vikunja
```

## 关键纪律

- **`task_delete` 与 `admin` 不可逆**。执行前向用户确认具体对象。
- `task_bulk` 会一次改多条，先说明影响范围。
- 需要 API token 与实例地址；没配时明确报错，不要猜。

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
