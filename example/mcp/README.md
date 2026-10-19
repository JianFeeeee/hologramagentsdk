# mcp — MCP 服务器接入插件

接入外部 MCP (Model Context Protocol) 服务器（stdio / SSE），透传其工具。
按需加载（`auto_start: false`）。

源自内核内置插件（2026-10-19 外置；internal/safeclose 已内联为本目录的
safeCloseChan —— 外部插件不能引 internal 包）。

## 构建

    hmapdev build

产物：`dist/mcp_linux_amd64.hmap`
