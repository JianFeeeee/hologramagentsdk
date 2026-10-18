---
name: deepsearch-tools
description: 真正的联网检索（本地 SearXNG 聚合多引擎）。按需工具：先 plugin_start deepsearch，用完可 plugin_stop。
---

# 联网检索

## 什么时候用

需要**当前**信息（新闻、价格、版本号、事件进展），
而你的训练数据可能过期时。

★ 判断标准：答案是"昨天的"就会出错的问题，就用它。
常识性、不变的知识不用搜。

## 先拉起插件

```
plugin_start  name=deepsearch
```

| 工具 | 用途 |
|---|---|
| `deepsearch_search` | 通用检索（标题/URL/摘要/时间） |
| `deepsearch_news` | 新闻检索 |
| `deepsearch_fetch` | 抓取指定 URL 正文 |
| `deepsearch_deep` | 深度检索（多轮、更全） |
| `deepsearch_status` | 查后端服务是否可用 |

## 用完可释放

```
plugin_stop  name=deepsearch
```

## 注意

- 后端是**本地 SearXNG**。服务没起时 `deepsearch_status` 会明确报错，
  不要编造检索结果。
- 引用检索结果时**带上 URL 和日期**，让用户能自己核对。
- 摘要可能不准，关键事实要 `deepsearch_fetch` 打开原文确认。

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
