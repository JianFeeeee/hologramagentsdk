---
name: weather-tools
description: 查询实时天气与未来预报。按需工具：用到时先 plugin_start weather 拉起，用完可 plugin_stop 释放。
---

# 天气查询

## 什么时候用

用户问**天气**、气温、是否下雨、要不要带伞、未来几天怎么样时。

## 先拉起插件

本插件默认**不自启**（不在初始工具清单里）。调用前先启用：

```
plugin_start  name=weather
```

启用后可用工具：

| 工具 | 用途 |
|---|---|
| `weather_current` | 当前实时天气（温度/湿度/风力/天气现象） |
| `weather_forecast` | 未来若干天预报 |
| `weather_set_location` | 设置默认城市（避免每次都说地名） |

## 用完可释放

天气是一次性查询，问完不再需要。连续问同一类问题时保持启用即可；
换了别的话题、且较长时间不会再查天气，可以关掉以释放上下文：

```
plugin_stop  name=weather
```

## 注意

- 数据源是 wttr.in，**不需要 API key**。
- 用户没给城市时，先看有没有设置过默认位置；没有就问一句，不要瞎猜。
- 查的是**真实天气**，不要把训练数据里的气温当答案。

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
