# 能力边界：哪些 API 外部插件能用

HologramAgent 有两类插件：

| 类型 | 说明 | 分发 |
|---|---|---|
| **外部插件** | 第三方开发，编译成 `.hmap` 后安装 | 独立分发，**可闭源** |
| **内置插件** | 编译进内核，`init()` 自注册 | 随内核发行，需合入主仓 |

SDK 包是**同一个** `github.com/JianFeeeee/hologramagentsdk/sdk`，但两类插件拿到的
**能力不同**：外部插件跑在独立进程里，由内核通过桥接注入能力（IPC，不是共享内存里的直接调用）。

本页说明边界在哪、为什么，以及**怎么在写代码前就知道某个 API 是否可用**。

## 一句话规则

> **公开 SDK 包里声明的符号，不等于外部插件拿得到。**

原因是：有些能力只有进程内的内置插件才可能拥有（比如直接读事件发布通道、
直接注入到内核 IO 层）。外部插件通过桥接运行时拿到的是一份**受注入的能力集合**。

## 外部插件**不可用**的 API

这些 API 在公开包里存在，但在外部插件路径上拿不到。文档里每条都带
<span class="api-badge api-badge-builtin">仅内置</span> 标记，
完整清单见 [仅内置插件可用](../api/builtin-only.md)。

| API | 外部插件的实际情况 | 该用什么 |
|---|---|---|
| `sdk.PluginSDK.Events()` | **恒为 nil**。桥接运行时不注入 event subscriber（`SetEventSubscriber` 全仓无调用点） | 桥接运行时已按你的声明完成 `events.subscribe`；Lua 插件用 `sdk.events.subscribe` |
| `sdk.PluginSDK.SetEventSubscriber` | 无人调用 | 同上 |
| `UnregisterOutputChannel` | 桥接只注入 registrar、**不注入 unregistrar**，调用是**静默无效**（返回 nil，不报错也不注销） | `RegisterOutputChannel` 可用；注销需重载插件 |
| `SocialAPI` 的写操作 | 公开接口只有 6 个**只读**方法 | 读用 `s.GetPerson` 等；写需内置插件 |
| `EventSubscriber.Publish` | 公开接口**刻意只有 Subscribe**，没有 Publish | 只订阅 |
| `PriorityL4` | 声明会被内核**夹到 L3** | 用 L1–L3 |
| `RegisterChannel` / `ListChannels` / `OutputChan` / `InjectInput` / `InjectInterrupt` | 只存在于内核内部 SDK | `RegisterInputChannel` / `RegisterOutputChannel` / `InjectText` 等公开方法 |
| `PluginMgr()` 的完整能力 | 公开 `PluginMgrAPI` **只有 3 个方法**（`ReloadOne` / `ListLoadedPlugins` / `IsPluginDisabled`） | 就这 3 个；`ReloadPlugins`/`Disable`/`Remove` 属内部接口 |

!!! warning "两处常见的文档错误（本站已更正）"
    1. **`PluginMgr()` 不是「仅内置可用」**。桥接模板显式注入了它
       （`base.SetPluginMgrAPI(procPluginMgr{})`），公开 `PluginMgrAPI` 也注明
       「外部插件可调用」。真正的区别是**方法数量**：公开面 3 个，内部面 9 个。
       容易混淆是因为两个包里有**同名但不同**的接口：
       `sdk.PluginMgrAPI`（3 方法）与 `internal/sdk.PluginManager`（9 方法）。
    2. **`Events()` 恒为 nil 这件事以前没写清**。旧文档把 `Events()` 当作
       可用的订阅入口，但桥接运行时不注入 subscriber。外部插件的事件订阅
       实际由生成的运行时通过 `events.subscribe` 完成。

## 判定依据来自哪里

本站的「仅内置」标记不是猜的，逐条来自：

1. **`tools/hmapdev/templates/proc_main.go.tmpl`** —— 外部插件运行时**实际注入**
   哪些能力，看 `buildPluginSDK()` 里的 `base.Set*` 调用。
2. **`internal/sdk`** —— 内置插件用的完整接口，与公开包对照。
3. **内核 RPC 协议表**（`internal/plugin/proc/protocol.go`）—— 外部插件**能发哪些请求**。

每条裁定的具体依据写在该 API 的告警框里，可以直接核对。

## 怎么快速确认

- 用 [API 搜索](../api/index.md) 搜 API 名或功能描述，带
  <span class="api-badge api-badge-builtin">仅内置</span> 的就是外部不可用
- 直接看 [仅内置插件可用](../api/builtin-only.md) 汇总页
- 拿不准时，**读 `example/` 下的示例插件** —— 它们全是外部插件，
  能被它们编译通过的写法，外部就一定可用

## 为什么这样设计

不是为了限制，而是**IPC 边界决定了能力边界**：外部插件跑在独立进程里，
内核只能通过显式的注入点把能力交过去。凡是需要「持有内核内部数据结构」
的能力（事件发布通道、IO 通道、插件注册表全量操作），进程外都无法安全暴露。

这套边界同时带来好处：插件崩溃不会带崩内核（进程隔离），
以及**插件可以闭源**（SDK 是 MIT，见[首页](../index.md#_3)）。
