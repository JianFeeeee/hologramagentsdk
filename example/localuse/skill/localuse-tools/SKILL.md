---
name: localuse-tools
description: 本机外设能力：截屏/看屏、摄像头/麦克风、剪贴板读写、整机操控。按需加载：先 plugin_enable("localuse") 再调用。
---

# 本地外设

## 本插件是按需加载（auto_start: false）

外设能力只在被要求时才启动。用 `plugin_enable("localuse")` 拉起，
工具才会出现在清单里。

## 工具（see 前缀 = 只读观察；sue 前缀 = 主动操作）

| 工具 | 用途 |
|---|---|
| `local_screensee` | 截屏当前画面（返回图片） |
| `local_screensue` | 主动操作屏幕（点击/键入） |
| `local_camerasue` | 摄像头拍照 |
| `local_speakeruse` | 扬声器播放 |
| `local_clipboardsee` / `local_clipboardsue` | 剪贴板读 / 写 |
| `local_computeruse` | 整机操控 |

## 注意

- 涉及摄像头/麦克风/整机操控时，先向用户确认再执行。
- 依赖系统工具（Linux: scrot/import、gnome-screenshot 任一）。
