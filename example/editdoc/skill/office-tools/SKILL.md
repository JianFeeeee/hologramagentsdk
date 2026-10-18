---
name: office-tools
description: 办公文档读写转换（docx/xlsx/pptx/md/csv/txt，基于 python-docx 等）。按需工具：先 plugin_start editdoc，用完可 plugin_stop。
---

# 办公文档

## 什么时候用

用户要**新建/读取/编辑/转换** Word、Excel、PPT、Markdown、CSV 文件时。

## 先拉起插件

```
plugin_start  name=editdoc
```

| 工具 | 用途 |
|---|---|
| `read_document` | 读文档内容与结构 |
| `create_document` | 新建文档 |
| `create_from_template` | 用模板新建 |
| `edit_document` | 编辑现有文档 |
| `convert_document` | 格式转换（如 docx→md） |
| `office_check` | 检查依赖是否齐全 |

## 用完可释放

```
plugin_stop  name=editdoc
```

## 注意

- 依赖 **Python 库**（python-docx / openpyxl / python-pptx）。
  先跑 `office_check` 确认，缺库时工具会明确报错。
- 改文档前先 `read_document` 看清结构，避免覆盖掉原有格式与内容。

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
