# cmd — 命令执行插件

沙箱工作目录里执行 shell 命令。按需加载（`auto_start: false`）：
由 agent 经 `plugin_enable("cmd")` 拉起，用完 `plugin_stop` 释放。

源自内核内置插件（2026-10-19 外置，判定标准见主仓 ARCHITECTURE
「内置插件的判定标准」：它只用公开 SDK，无必须内置的理由）。

## 构建

    hmapdev build

产物：`dist/cmd_linux_amd64.hmap`
