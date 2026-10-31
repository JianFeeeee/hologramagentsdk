<!-- 本页由 tools/apidoc/gensite 从源码生成，请勿手改；要改文档就改 sdk/*.go 的注释。 -->

# 示例插件

SDK 仓 `example/` 下有多个**真实可编译**的示例插件，覆盖工具注册、通道、记忆读写、LLM 调用、生命周期等常见形态。

每个示例都能用 `hmapdev build` 打成 `.hmap` 装进内核直接跑。

## `a2a`

用到的 API：`Error` · `InjectInputSyncOpts` · `Lock` · `RLock` · `RUnlock` · `RegisterInputChannel` · `RegisterOutputChannel` · `RegisterStage` · `RegisterTool` · `SetAutoRestart` · `Settings` · `Unlock`

## `acp`

用到的 API：`Error` · `InjectInputSyncOpts` · `Lock` · `RLock` · `RUnlock` · `RegisterInputChannel` · `RegisterOutputChannel` · `RegisterStage` · `RegisterTool` · `SetAutoRestart` · `Settings` · `Unlock`

## `ai_image`

用到的 API：`Error` · `RegisterTool` · `SetAutoRestart` · `Settings`

## `bili`

用到的 API：`Lock` · `RegisterTool` · `SetAutoRestart` · `Settings` · `Unlock`

## `bilibili`

用到的 API：`Error` · `Lock` · `RLock` · `RUnlock` · `RegisterOnRemoveHandler` · `Unlock`

## `browser`

用到的 API：`InjectInterruptTextOpts` · `InjectTextNoMemory` · `RegisterInputChannel`

## `calendar`

用到的 API：`InjectInterruptTextOpts` · `RLock` · `RUnlock` · `RegisterInputChannel` · `RegisterOnRemoveHandler` · `RegisterStopHandler`

## `cmd`

用到的 API：`RegisterStage`

## `commondev`

用到的 API：`InjectText` · `RawFiles` · `RegisterOutputChannel` · `Secrets` · `SetRawFilesAPI`

## `deepsearch`

用到的 API：`RegisterStopHandler`

## `memo`

用到的 API：`InjectInterruptTextOpts` · `RegisterOnRemoveHandler` · `RegisterStage`

## `qq`

用到的 API：`InjectInterruptTextOpts` · `RegisterOutputChannel` · `Security` · `SetSecurityAPI`

## `recoverydiag`

用到的 API：`Knowledge`

## `rss`

用到的 API：`RegisterOnRemoveHandler`

