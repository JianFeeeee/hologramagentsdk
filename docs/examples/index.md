<!-- 本页由 tools/apidoc/gensite 从源码生成，请勿手改；要改文档就改 sdk/*.go 的注释。 -->

# 示例插件

SDK 仓 `example/` 里保留的是**未迁移到组织仓**的示例插件，都能用 `hmapdev build` 打成 `.hmap` 装进内核直接跑。

★ **其余 26 个插件已迁到 GitHub 组织** [`HologramAgent`](https://github.com/HologramAgent) —— 一插件一仓，覆盖工具注册、通道、记忆读写、LLM 调用、生命周期等全部常见形态。
想看完整实现时去那里 clone，不要以为本仓就是全部。

## `feiyu_voice`

用到的 API：`RegisterTool` · `SetAutoRestart` · `Settings`

## `rtvoice`

用到的 API：`InjectInterruptTextOpts` · `Lock` · `RegisterInputChannel` · `RegisterOutputChannel` · `RegisterTool` · `Settings` · `Unlock`

