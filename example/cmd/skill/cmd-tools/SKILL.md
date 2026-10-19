---
name: cmd-tools
description: 在沙箱工作目录里执行 shell 命令（cmd_run）。按需加载：先 plugin_enable 再调用，用完可 plugin_stop 释放上下文。
---

# 命令执行

## 本插件是按需加载（auto_start: false）

它只在你真的要跑命令时才需要。要先用 `plugin_enable("cmd")` 拉起，
否则 `cmd_run` 不在工具清单里（不是坏了，是还没装上）。

## 工具

| 工具 | 用途 |
|---|---|
| `cmd_run` | 执行 shell 命令，返回 stdout/stderr/exit_code |

## 注意

- 超时可用参数 `timeout`（如 "30s" / "5m"）指定，超过后命令被终止。
- 长驻进程请用 `agentcli` 的 `terminal_*`（PTY 会话），不要用 cmd_run 挂着。
- 危险命令（rm -rf、写系统目录）先向用户确认。
