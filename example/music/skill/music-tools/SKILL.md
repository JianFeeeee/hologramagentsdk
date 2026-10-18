---
name: music-tools
description: 搜索歌曲与查看歌词（网易云音乐）。按需工具：先 plugin_start music，用完可 plugin_stop。
---

# 音乐搜索

## 什么时候用

用户问某首歌、某个歌手、想找歌词时。

## 先拉起插件

```
plugin_start  name=music
```

| 工具 | 用途 |
|---|---|
| `music_search` | 按歌名/歌手搜索 |
| `music_lyrics` | 查指定歌曲的歌词 |

## 用完可释放

```
plugin_stop  name=music
```

## 注意

只返回**曲目信息与歌词文本**，不下载音频文件。

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
