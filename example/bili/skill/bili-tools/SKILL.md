---
name: bili-tools
description: B站视频清晰度查询与下载（yt-dlp）。按需工具：先 plugin_start bili，用完可 plugin_stop。
---

# B站视频

## 什么时候用

用户要**下载 B站视频**，或想知道某个视频有哪些清晰度可选时。

## 先拉起插件

```
plugin_start  name=bili
```

| 工具 | 用途 |
|---|---|
| `bili_video` | 查清晰度列表 / 按指定格式下载 |

## 用完可释放

```
plugin_stop  name=bili
```

## 注意

- 依赖 **yt-dlp**，未安装时会明确报错。
- 下载目录由插件配置决定，不要假设是当前工作目录。
- 先查清晰度列表再下载，不要盲目选最高（可能体积很大）。

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
