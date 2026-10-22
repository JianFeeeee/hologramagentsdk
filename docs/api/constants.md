<!-- 本页由 tools/apidoc/gensite 从源码生成，请勿手改；要改文档就改 sdk/*.go 的注释。 -->

# 常量与枚举

SDK 里的取值枚举。其中带「仅内置」标注的取值在内核侧会被夹到较低级别。

## AutoStartOnKernel 等

| 名称 | 说明 |
|---|---|
| `AutoStartOnKernel` | AutoStartOnKernel 表示伴随内核启动（缺少该字段时的默认形态）。 |
| `AutoStartOnDemand` | AutoStartOnDemand 表示启用但不自启：待 agent 显式拉起。 |

## OnDemandNeedsSkill

| 名称 | 说明 |
|---|---|
| `OnDemandNeedsSkill` |  |

## FacilityConfig 等

| 名称 | 说明 |
|---|---|
| `FacilityConfig` | FacilityConfig 是配置快照（cfg）就绪。 |
| `FacilityKernelIO` | FacilityKernelIO 是 IOManager + EventBus 就绪。 |
| `FacilityMemory` | FacilityMemory 是图记忆（含索引/社交图/蒸馏）就绪。 |
| `FacilityLua` | FacilityLua 是 Lua 协议适配 VM 就绪。 |
| `FacilitySupervisor` | FacilitySupervisor 是代理生命周期守护就绪。 |
| `FacilityTracker` | FacilityTracker 是变更追踪（overlayfs）就绪。 |
| `FacilityTextMemory` | FacilityTextMemory 是文本记忆就绪。 |
| `FacilityLLM` | FacilityLLM 是多源 Provider 管理就绪。 |
| `FacilityDocStore` | FacilityDocStore 是文档记忆就绪。 |
| `FacilityKnowledge` | FacilityKnowledge 是知识库就绪。 |
| `FacilityMultimodal` | FacilityMultimodal 是多模态向量空间就绪。 |
| `FacilityONNX` | FacilityONNX 是依存句法分析器就绪。 |
| `FacilityStageHost` | FacilityStageHost 是阶段管道 + 插件注册表就绪。 |
| `FacilityAgent` | FacilityAgent 是主 Agent 就绪（**最晚**的一个，通常在插件加载前）。 |

## ProxyAuthAgent 等

| 名称 | 说明 |
|---|---|
| `ProxyAuthAgent` | 表示由内核统一保护：浏览器走门户会话，非浏览器客户端走 X-API-Key |
| `ProxyAuthNone` | 表示不经内核鉴权，直接把请求转发给上游 |
| `ProxyAuthAgentLegacy` | **仅用于兼容读取**的旧值 `"homeagent"`，存量 plugin.json 里写的仍是它；不要在新声明里用 |
| `ProxyAuthHomeAgent` | 旧常量名，等价于 `ProxyAuthAgent`（Deprecated） |

## StageOnInput 等

| 名称 | 说明 |
|---|---|
| `StageOnInput` |  |
| `StagePreAction` |  |
| `StagePostAction` |  |
| `StageBeforeToolcall` |  |
| `StageAfterToolcall` |  |
| `StageBeforeOutput` |  |
| `StageAfterOutput` |  |

## ContextPolicyNone 等

| 名称 | 说明 |
|---|---|
| `ContextPolicyNone` |  |
| `ContextPolicyPrune` |  |

## RecallPolicyNone 等

| 名称 | 说明 |
|---|---|
| `RecallPolicyNone` |  |
| `RecallPolicyAuto` |  |

## ScenePolicyAuto 等

| 名称 | 说明 |
|---|---|
| `ScenePolicyAuto` |  |
| `ScenePolicyNone` |  |

## PriorityL1 等

| 名称 | 说明 |
|---|---|
| `PriorityL1` |  |
| `PriorityL2` |  |
| `PriorityL3` |  |
| `PriorityL4` | PriorityL4 仅内核级（内置）插件可用；外部插件声明会被夹到 L3。 |

## EventRawInput 等

| 名称 | 说明 |
|---|---|
| `EventRawInput` |  |
| `EventAgentOutput` |  |
| `EventAgentLLMChain` |  |
| `EventToolCall` |  |
| `EventReasoning` |  |
| `EventStage` |  |
| `EventSystem` |  |
| `EventReasoningDelta` | 流式增量事件（token 级）：核心 process() 流式化后每收到一个增量块发布。 |
| `EventContentDelta` |  |

## StageScopeGlobal 等

| 名称 | 说明 |
|---|---|
| `StageScopeGlobal` | StageScopeGlobal receives all stage events (default). |
| `StageScopeOwnTools` | StageScopeOwnTools only receives events for this plugin's own tool calls |

## CapText 等

| 名称 | 说明 |
|---|---|
| `CapText` |  |
| `CapFile` |  |
| `CapImage` |  |
| `CapAudio` |  |
| `CapStructured` |  |

