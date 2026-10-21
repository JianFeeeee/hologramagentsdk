# HologramAgent 插件 SDK

用 **Go** 或 **Lua** 为 HologramAgent 编写插件。插件跑在独立进程里，通过公开 SDK 与内核通信：
注册工具供模型调用、挂阶段钩子干预流程、读写三层记忆、注册输入输出通道、订阅事件。

<div id="api-search"></div>

## 从这里开始

<div class="grid cards" markdown>

-   :material-rocket-launch: **第一次写插件**

    装工具链、生成工程、写一个工具、打包成 `.hmap` 装进内核跑起来。

    [:octicons-arrow-right-24: 快速开始](guide/getting-started.md)

-   :material-book-open-variant: **API 参考**

    逐个符号的签名与说明，直接取自源码注释。附示例插件里的真实调用点。

    [:octicons-arrow-right-24: 工具（Tools）](api/tools.md)

-   :material-shield-lock: **能力边界**

    哪些 API 外部插件能用、哪些仅内置插件可用，以及为什么。**先看这个能省很多时间。**

    [:octicons-arrow-right-24: 能力边界](guide/capability-boundary.md)

-   :material-code-braces: **示例插件**

    `example/` 下有多个真实可编译的插件，覆盖常见形态。

    [:octicons-arrow-right-24: 示例总览](examples/index.md)

</div>

## 许可

SDK 以 **MIT** 发布 —— 插件作者可**自由选择自己的许可**（闭源、商业、私有均可），
不必同许可、也不必回馈。原因：SDK 会随插件一起静态链接（源码进入插件二进制），
若用传染性许可，插件作者就被强制开源；MIT 让第三方插件生态不必承担这个代价。

内核本身是 **AGPL-3.0-only**，但那是内核的许可，与外部插件无关 ——
SDK 完全自包含（`go.mod` 零外部依赖，只依赖 Go 标准库），不引用内核任何代码。

## 版本

本文档站的 API 参考从源码生成，对应 SDK 版本见 [版本与兼容](versions.md)。
