---
name: localuse-tools
description: 本机外设：截屏/看屏、摄像头、扬声器、剪贴板、整机操控。按需工具：先 plugin_start localuse，用完可 plugin_stop。
---

# 本地外设

## 什么时候用

用户要求**看本机屏幕 / 拍一张 / 读剪贴板 / 操作本机界面**时。
纯文本任务不要拉它——外设能力会占住屏幕与麦克风。

## 先拉起插件

```
plugin_start  name=localuse
```

| 工具 | 用途 |
|---|---|
| `local_screensee` | 截屏当前画面（返回图片） |
| `local_screensue` | 主动操作屏幕（点击/键入） |
| `local_camerasue` | 摄像头拍照 |
| `local_speakeruse` | 扬声器播放 |
| `local_clipboardsee` | 读剪贴板 |
| `local_clipboardsue` | 写剪贴板 |
| `local_computeruse` | 整机操控 |

命名约定：`see` = 只读观察，`sue` = 主动执行。

## 用完可释放

```
plugin_stop  name=localuse
```

## 注意

- 摄像头 / 麦克风 / 整机操控属于敏感操作，**先向用户确认**再执行。
- 依赖系统工具（Linux 下截图用 scrot / import / gnome-screenshot 任一），
  全都缺时会明确报错，不要反复重试。

## ★ 用 plugin_stop / plugin_start，不要用 plugin_disable / plugin_enable

| 工具 | 作用 | 跨重启 |
|---|---|---|
| `plugin_stop` | **运行态**关闭，现在停掉 | **不影响**，重启照常加载 |
| `plugin_start` | **运行态**拉起，现在开回来 | 不影响 |
| `plugin_disable` | **持久**禁用 | **会记下来**，开机也不自启 |
| `plugin_enable` | 解除持久禁用 | 会记下来 |

**释放上下文用 `plugin_stop`。** 本插件声明 `auto_start: false`，本来就不自启；
但 `plugin_disable` 仍会写进持久禁用清单，让 `plugin_start` 拒绝拉起。
