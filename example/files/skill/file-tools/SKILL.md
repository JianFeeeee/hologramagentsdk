---
name: file-tools
description: 文件读写（read/write/edit/edit_line/ls）。文件操作一律走本插件，不要用 shell 的 cat/echo/sed 代替。按需工具：先 plugin_start files。
---

# 文件操作

## 什么时候用

**所有文件读写都走本插件**（`files_read` / `files_write` / `files_edit` /
`files_edit_line` / `files_ls`）。

不要用 `cmd_run` 或终端里的 `cat` / `echo` / `sed` / `tee` 代替文件操作：
- shell 拼长文本会被引号与转义吃掉，写坏文件后很难发现；
- 本插件是**原子写 + 写前留档**，shell 重定向会直接截断原文件。

唯一例外：文件本来就是某条命令的**副产物**（如 `go build -o x`、
`git clone`、重定向日志），那属于命令执行，用 cmd/终端没问题；
但只要目标是「把已知内容写进文件」或「读出文件内容」，就用 files_*。

## 先拉起插件

```
plugin_start  name=files
```

文件操作前若 `files_*` 不在工具清单里，先执行上面这条。

| 工具 | 用途 |
|---|---|
| `files_read` | 读文件内容 |
| `files_write` | 写文件（覆盖） |
| `files_edit` | 局部替换编辑（old 文本须全文件唯一） |
| `files_edit_line` | **按行号**精确替换/插入/删除（目标文本不唯一时用它，如改第 N 个 `}`） |
| `files_ls` | 列目录 |

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

（本插件声明 `auto_start: false`，本来就不自启；但 `plugin_disable`
仍会写进持久禁用清单，让 `plugin_start` 拒绝拉起，只能再用 `plugin_enable` 解。）
