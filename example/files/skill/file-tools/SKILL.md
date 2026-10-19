---
name: file-tools
description: 沙箱内文件读写（read/write/edit/ls）。按需工具：先 plugin_start files，用完可 plugin_stop。
---

# 文件操作

## 什么时候用

需要在**插件配置的工作目录**内读写文件时。

★ 如果你已经有 run_shell / bash 之类的能力，优先用那个 —— 它更直接。
本插件适合**没有 shell 权限**或需要走沙箱路径限制的场景。

## 先拉起插件

```
plugin_start  name=files
```

| 工具 | 用途 |
|---|---|
| `files_read` | 读文件内容 |
| `files_write` | 写文件（覆盖） |
| `files_edit` | 局部替换编辑（old 文本须全文件唯一） |
| `files_edit_line` | **按行号**精确替换/插入/删除（目标文本不唯一时用它，如改第 N 个 `}`） |
| `files_ls` | 列目录 |

## 用完可释放

```
plugin_stop  name=files
```

## 注意

- 只能访问**配置的工作目录**内。走出沙箱会明确报错，不要试图绕。
- 写/改之前先读一遍，确认不是要保留的内容。

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
