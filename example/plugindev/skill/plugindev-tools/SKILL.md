---
name: plugindev-tools
description: 插件开发工具链（脚手架/构建 .hmap/SDK 管理）。按需工具：先 plugin_start plugindev。
---

# 插件开发

## 什么时候用

用户要**写一个新插件**、构建 `.hmap`、或管理 SDK 版本时。

## 先拉起插件

```
plugin_start  name=plugindev
```

| 工具 | 用途 |
|---|---|
| `plugindev_status` | 查工具链与 SDK 状态 |
| `plugindev_init` | 生成插件工程脚手架 |
| `plugindev_build` | 构建打包 `.hmap` |
| `plugindev_sdk` | SDK 版本安装/切换 |
| `plugindev_projects` | 列已有插件工程 |

## 开发完可释放

```
plugin_stop  name=plugindev
```

## 关键纪律

- 构建产物（`.hmap`）**必须用 `plugin_install` 安装**，
  不要手工解包替换 `plugin.bin` —— 那会绕过整包校验、备份回滚、
  rawfile 全量解包。
- 声明 `auto_start=false`（按需加载）的插件**必须**配 `skill/`，
  否则 agent 不知道它存在，等价于没装。打包时会被拦下。

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
