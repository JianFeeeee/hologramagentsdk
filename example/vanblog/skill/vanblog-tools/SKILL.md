---
name: vanblog-tools
description: 管理 VanBlog 博客（文章/分类/标签/草稿/备份等，28 个工具）。按需工具：先 plugin_start vanblog，用完可 plugin_stop。
---

# VanBlog 博客管理

## 什么时候用

用户要**写/改文章、管分类标签、发布草稿、备份**他们的 VanBlog 站点时。

## 先拉起插件

```
plugin_start  name=vanblog
```

工具按用途分组（都带 `vanblog_` 前缀）：

| 组 | 工具 |
|---|---|
| 文章 | `list_articles` `get_article` `create_article` `update_article` `delete_article` `search_articles` |
| 草稿 | `manage_drafts` |
| 分类/标签 | `manage_categories` `manage_tags` |
| 媒体/链接 | `manage_images` `manage_links` |
| 站点配置 | `manage_settings` `manage_site` `manage_menu` `manage_about` `manage_social` `manage_rewards` |
| 页面 | `manage_pages` |
| 运维 | `get_analysis` `get_logs` `manage_backup` `manage_caddy` `manage_isr` `manage_pipelines` |
| 协作 | `manage_collaborators` `manage_tokens` `auth` `get_meta` |

## 用完可释放

28 个工具占约 454 tokens，是本组里第二大的。写完博客就关掉：

```
plugin_stop  name=vanblog
```

## 关键纪律

- **删除类操作**（`delete_article` 等）不可逆。执行前必须向用户确认是哪一篇。
- 改文章前先 `get_article` 读原文，不要用新内容整篇覆盖。
- 需要 API key 与站点地址；没配时工具会明确报错，不要猜。

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
