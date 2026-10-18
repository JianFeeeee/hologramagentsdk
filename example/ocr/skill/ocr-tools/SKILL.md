---
name: ocr-tools
description: 从图片中提取文字（Tesseract OCR）。按需工具：先 plugin_start ocr，用完可 plugin_stop。
---

# 图片文字识别

## 什么时候用

用户给了**图片里带文字**（截图、照片、扫描件），需要把文字读出来时。

★ 如果图片是**给模型看内容**（风景、人物、界面布局），不需要 OCR ——
直接用 see_image 看就行。只有"要文字"才用本插件。

## 先拉起插件

```
plugin_start  name=ocr
```

| 工具 | 用途 |
|---|---|
| `ocr_ocr_image` | 从 URL 或 base64 图片提取文字 |

## 用完可释放

```
plugin_stop  name=ocr
```

## 注意

- 依赖本机 **Tesseract** 引擎；未安装时工具会明确报错，不要当成"图片里没字"。
- 识别结果可能含错字（尤其手写/低分辨率）。引用原文时提示这一点。

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
