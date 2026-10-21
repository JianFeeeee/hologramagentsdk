---
name: cmd-tools
description: 一次性 shell 命令执行（cmd_run），支持管道/heredoc/重定向。按需工具：先 plugin_start cmd，用完可 plugin_stop。
---

# 命令执行

## 什么时候用

需要跑一条**不需要交互**的 shell 命令时：查系统信息、跑脚本、批量处理文件、
`git`/`go`/`python` 之类的一次性调用。

需要**持续输入**的程序（vim、top、ssh 会话）不要用本插件，用 agentcli 的
`terminal_create` / `terminal_write` / `terminal_read`。

## 先拉起插件

```
plugin_start  name=cmd
```

| 工具 | 用途 |
|---|---|
| `cmd_run` | 执行一条命令，返回 stdout/stderr/exit_code |

参数：`command`（必填）、`timeout`（如 30s/5m，默认 30s）、`workdir`。

## 用完可释放

```
plugin_stop  name=cmd
```

## 注意

- 经完整 bash 执行：管道、`;`、`&&`、`$()`、heredoc、重定向都可用。
- 长驻进程别用 cmd_run 挂着等它退出，那会撞 timeout；用终端工具。
- **文件读写不要用 shell 代替**（`cat`/`echo`/`sed`/`tee`/重定向）：文件里有已知内容要写、
  或要读文件内容时，走 files 插件的 `files_read`/`files_write`/`files_edit`。
  只有当文件是命令的副产物（`go build -o`、`git clone`、日志重定向）才由命令自己产生。
- 危险命令（`rm -rf`、写系统目录）先向用户确认。

## ★ 用 plugin_stop / plugin_start，不要用 plugin_disable / plugin_enable

| 工具 | 作用 | 跨重启 |
|---|---|---|
| `plugin_stop` | **运行态**关闭，现在停掉 | **不影响**，重启照常加载 |
| `plugin_start` | **运行态**拉起，现在开回来 | 不影响 |
| `plugin_disable` | **持久**禁用 | **会记下来**，开机也不自启 |
| `plugin_enable` | 解除持久禁用 | 会记下来 |

**释放上下文用 `plugin_stop`。** 用 `plugin_disable` 会顺手把"开机自启"
永久关掉。本插件声明 `auto_start: false`，本来就不自启；但 `plugin_disable`
仍会写进持久禁用清单，让 `plugin_start` 拒绝拉起。
