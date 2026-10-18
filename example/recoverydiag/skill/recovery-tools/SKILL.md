---
name: recovery-tools
description: 崩溃取证与自检（退出码/信号/config.db 完整性/日志扫描）。按需工具：先 plugin_start recoverydiag。
---

# 故障取证

## 什么时候用

服务**起不来、反复重启、行为异常**，需要查清原因时。
正常运行时不需要它。

## 先拉起插件

```
plugin_start  name=recoverydiag
```

| 工具 | 用途 |
|---|---|
| `diag_triage` | 退出码/信号/存活状态粗分 |
| `diag_db` | config.db 完整性检查 |
| `diag_log_scan` | 扫描日志找错误模式 |
| `diag_delta` | 对比两次快照的差异 |
| `diag_loc` | 定位日志/配置位置 |

## 排查完可释放

```
plugin_stop  name=recoverydiag
```

## 注意

这些工具**只读**诊断，不修改任何状态。
要修复请明确告知用户具体动作，不要自行修改系统配置。

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
