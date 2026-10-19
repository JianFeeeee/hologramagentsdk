# localuse — 本地外设插件

本机外设能力（截屏/摄像头/麦克风/剪贴板/整机操控）。按需加载
（`auto_start: false`）：agent 经 `plugin_enable("localuse")` 拉起。

源自内核内置插件（2026-10-19 外置；只用公开 SDK，见主仓判定标准）。

## 构建

    hmapdev build

产物：`dist/localuse_linux_amd64.hmap`
