# 第一个 Lua 插件

Lua 插件适合**轻量、快速原型**：不需要 Go 编译环境，改完重启内核即可生效。
但它有一个必须理解的限制 —— 执行模型是**被动回调**。

## 执行模型（先读这段）

Lua 插件跑在内核进程内的 gopher-lua 解释器里（单 Lua 状态 + 互斥锁）：

- **被动回调**：`main.lua` 只在加载时执行一次。此后工具、阶段钩子、
  输入输出通道全部由内核事件驱动回调你的 Lua 函数。**插件不能自己启动后台任务。**
- **没有并发**：Lua 侧没有 goroutine、协程调度，也没有 `os` / `io` 库和 socket 监听。
  唯一主动出站通道是 `sdk.http.get/post`（同步请求）。
- **任何阻塞循环都会持锁卡死该插件的全部调用。**

!!! warning "要常驻服务就用 Go 插件"
    需要监听端口、后台轮询、定时任务的，请用 [Go 插件](first-plugin.md)
    （可自行启动 goroutine）。Lua 侧的等价做法是**事件驱动**：把逻辑挂在
    工具、阶段钩子或通道回调上。

## 生成工程

```bash
hmapdev init myluaplugin --lua
cd myluaplugin
```

结构：

```
myluaplugin/
├── plg.json    — entry: "main.lua", targets: "lua"
├── main.lua    — 插件实现
├── sdk.lua     — SDK 模拟层（支持独立测试）
└── README.md
```

## 一个完整的插件

```lua
-- main.lua
local plugin = {
  name = "myluaplugin"
}

function plugin.start(sdk)
  sdk.log("info", "myluaplugin starting...")

  sdk.register_tool("myluaplugin_hello", {
    description = "向指定的人打招呼",
    parameters = {
      type = "object",
      properties = {
        who = { type = "string", description = "要打招呼的对象" }
      },
      required = { "who" }
    }
  }, function(args)
    return { content = "hello, " .. (args.who or "world") .. "!" }
  end)

  sdk.log("info", "myluaplugin started")
end

function plugin.stop()
  sdk.log("info", "myluaplugin stopped")
end

return plugin
```

## 本地测试

`sdk.lua` 是纯 Lua 的 SDK 模拟实现，可以直接用解释器跑：

```bash
lua main.lua
# [lua-plugin] info: myluaplugin starting...
# [lua-plugin] register_tool: myluaplugin_hello
# [lua-plugin] info: myluaplugin started
```

在内核里运行时，`sdk.*` 由 Go 层注入，`sdk.lua` 里所有 `-- !impl` 标记的函数
会被替换成真实实现。

## API 约定的两点

- **注册类函数调用即时报错**（抛 Lua error）—— 注册失败不会静默。
- **数据类函数统一返回 `(result, err)`**，`err` 为 nil 表示成功。
  核心未装配的子系统（如 SocialAPI）返回空值而非报错。

Lua 侧的 `sdk.*` 能力与外部 Go 插件对齐至 SDK 1.3.0（需内核 1.4.0+）。

!!! note "历史提醒"
    1.1–1.3 期间，媒体 / 注入标志位 / 优先级能力只在 Go 侧有，Lua 侧静默缺失。
    现已全量对齐，并由 `internal/plugin/lua_surface_test.go` 的契约测试守住
    「`sdk.lua` 承诺的每个函数都有运行时绑定」。

## 构建

```bash
hmapdev build          # → dist/myluaplugin_lua.hmap
```

Lua 插件直接打包源码，不经过编译。

## 下一步

- [能力边界](capability-boundary.md)：Lua 与 Go 外部插件的能力面一致
- [打包与发布](packaging.md)
- [示例](../examples/index.md) — 本仓只留未迁移的示例
- [luademo](https://github.com/HologramAgent/luademo) — Lua 版完整参考实现（已迁到组织仓）
