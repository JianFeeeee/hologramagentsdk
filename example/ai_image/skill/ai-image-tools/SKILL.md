---
name: ai-image-tools
description: AI 图像生成（DALL·E / Stable Diffusion）。按需工具：先 plugin_start ai_image，用完可 plugin_stop。
---

# AI 画图

## 什么时候用

用户要求**生成图片**（画一张、生成插画、做个配图）时。

## 先拉起插件

```
plugin_start  name=ai_image
```

| 工具 | 用途 |
|---|---|
| `ai_image_generate` | 按提示词生成图片 |

## 用完可释放

```
plugin_stop  name=ai_image
```

## 注意

- 需要配置**上游服务**（DALL·E 或 SD）与各自的密钥；没配时会明确报错，
  不要重试或猜一个 URL。
- 生图有成本（按张计费）。一次不满意就调整提示词，不要连续盲试。
- 提示词用**画面描述**（主体/风格/构图/光线），不是命令式句子。

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
