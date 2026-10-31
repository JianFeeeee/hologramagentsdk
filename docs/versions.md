# 版本与兼容

## SDK 版本语义

**SDK 版本跟随内核的中版本，patch 位恒为 `.0`。**

整条内核 `1.1.x` 线（1.1.0、1.1.1、1.1.7…）共用 **SDK 1.1.0**；
只有内核进入 `1.2.0` 这种中版本跃迁时，SDK 才升到 1.2.0。

这样插件作者只需关心「我在为哪个中版本写插件」，不必跟着内核的每个 bugfix 换依赖。
当前内核声明的兼容上限是 **SDK 1.3.0**。

## 版本历史

| SDK | 内核 | 变化 | 需要重编？ |
|---|---|---|---|
| **1.4.0** | 1.4.0+ | **声明式安全**（`SecurityAPI`：`DeclareProfile`/`DeclareProfiles`、`SecurityProfile`）、**实时通道**（`ChannelDef.Realtime`）、`ScenePolicy` | 想用新 API 才需要 |
| **1.3.0** | 1.4.0+ | 驻留子 agent、`RecallPolicy` 等 | 想用新 API 才需要 |
| **1.2.0** | 1.2.0 / 1.3.x | `InjectOptions{NoMemory, ContextPolicy}`、六个 `*Opts` 变体、`ChannelDef.ContextPolicy` | 不需要 |
| **1.1.0** | 1.1.x | 多模态贯通：`Triple.SentenceText`、`Doc.Attachments`、`MediaAttachment`、`InsertWithMedia`、媒体注入方法 | 不需要 |
| **1.0.0** | 1.0.0+ | **运行模型变更**：C ABI 动态库 → 子进程 + 共享内存 | **需要** |

### 1.0.0 是唯一一次破坏性变更

- `.so` / `.dylib` / `.dll` **不再被加载**，遇到旧产物会跳过并报可操作错误（不崩溃）。
- **业务代码不用改一行** —— 公开 SDK 接口零改动，用新版 `hmapdev` 重编即可。
- 产物从 `plugin.so` 变为 `plugin.bin`；不再需要 cgo。

### 1.1.0 / 1.2.0 是纯追加

两次都是**新增方法由插件调用、内核实现**，不调就不受影响。
零值 `InjectOptions` 与旧的三参数方法完全等价，因此存量插件**不需要改、也不需要重编**；
想用新字段的重编即可。

### SDK 1.4.0 → 本轮新增（PluginMgr 运行态启停）⚠️ 有一个例外

本轮给 `PluginMgrAPI` 新增 3 个方法（`StopPlugin` / `StartPlugin` /
`IsPluginRuntimeStopped`，见 `docs/zh/kernel-modularization-design.md` §2.6.7）。

按上面的规则，这属于"**插件调用、内核实现**"的纯追加 ⇒ 无需重编。**但有一个例外**：

| 对象 | 是否需要动 | 原因 |
|---|---|---|
| **已编译的存量插件**（`plugin.bin`） | ❌ 不需要 | 不调新方法就不受影响（与 1.1/1.2 一致） |
| **存量插件的源码要重编** | ⚠️ **需要同步 hmapdev** | `proc_main.go.tmpl` 里的 `procPluginMgr` 必须实现新方法，否则**编译不过** |
| **新插件** | ✅ 自动 | 模板已同步（`hmapdev` 已更新） |

!!! warning "为什么这次和 1.1/1.2 不一样"
    1.1/1.2 只改了 `sdk/`（插件**调用**的一侧）与内核实现。
    本轮还改了 **`tools/hmapdev/templates/proc_main.go.tmpl`** ——
    那是**生成插件代码**的模板，属于"插件**被编译**的一侧"。

    `plugin.bin` 里的 `procPluginMgr` 是**编译期**由模板生成的具体类型，
    它必须满足 `PluginMgrAPI` 的**全部**方法（Go 接口是精确匹配）。
    模板不同步 ⇒ 生成物不满足接口 ⇒ **编译失败**（不是运行期降级）。

    ⇒ 判据 `internal/plugin/proc/bridge_sync_test.go` 已有 3 条自动守这个
    （SDK 方法集 ⇒ 能力表 / Handle 分派 / hmapdev 模板，三处必须同步）。



## 脚手架模板的协议通道纪律（`hmapdev init` 生成物）

子进程插件的 **stdout 是 JSON-RPC 协议通道**，**stderr 才是日志通道**
（SDK 侧 `log.SetOutput(os.Stderr)`）。这条纪律**必须写进脚手架**，
否则每个新插件作者都会踩。

### 曾踩的坑（2026-10-16 实测）

`hmapdev init` 生成的 `plugin.go` 原先用 `fmt.Printf` 打日志 ⇒ 写进 stdout
⇒ 内核 readLoop 收到非 JSON 文本：

```
[proc] final_probe 收到非法 JSON 帧（21 字节）: invalid character 'i' in literal false
```

两个后果叠加、都很难定位：

1. 那句日志**丢了**（内核把它当坏帧丢弃）；
2. 多了一条看起来像**协议故障**的报错 —— 而真因是"插件自己 printf"。

**A/B 实证**（同一内核，两个插件）：

| 插件 | 脚手架 | 非法 JSON 帧 | 日志可见 |
|---|---|---|---|
| `final_probe` | 旧（`fmt.Printf`） | **1 次** | ❌ 丢失 |
| `fixed_probe` | 新（`log.Printf`） | **0 次** | ✅ `[fixed_probe] started` |

### 修法

模板改成 `log.Printf`，并把这条纪律写进生成物的**头部注释**
（让每个插件作者第一眼就看到）。

### 判据（`tools/hmapdev/scaffold_stdout_test.go`，3 条）

1. `TestScaffoldTemplateDoesNotWriteStdout` —— **代码行**里不得有
   `fmt.Printf/Println/Print/os.Stdout`
2. `TestScaffoldTemplateUsesStderrLogging` —— 反向钉住：光禁 stdout 不够，
   把日志全删也能过第一条 ⇒ 必须**真的**用 `log.Printf`（并 import "log"）
3. `TestScaffoldLuaTemplateDoesNotWriteStdout` —— Lua 脚手架同理

**变异 2/2 咬住**：改回 `fmt.Printf` ⇒ 红；删光日志 ⇒ 红。

!!! warning "判据自身踩的坑（值得记住）"
    第一版判据直接扫描模板字符串 ⇒ **被模板里的注释判红**（注释**故意**
    提到 `fmt.Printf` 来讲这个坑）。必须**先剥注释**再扫代码行。

    这是"源码扫描判据"的固有脆弱性：本仓规范要求把坑写进注释，
    而注释里必然出现那些符号名 ⇒ **任何时候用字符串扫描判据，
    都要先想"注释里会不会也有这个串"**（同类事故见
    `docs/zh/kernel-modularization-design.md` §8.6，那里连续骗过三次）。


## 示例插件的协议通道纪律（已修）

上面的脚手架纪律同样适用于**示例插件** —— 它们是**子进程插件**
（`package main` + `plugin.bin`），stdout 即协议通道。

### 生产环境实测的缺陷（不是理论问题）

`journalctl -u hologramagent | grep -c "非法 JSON 帧"` ⇒ **1038 条**，来源：

| 插件 | 次数 | 本仓？ |
|---|---|---|
| `huawei_smarthome` | 720 | 第三方（内核侧报错就是为它准备的） |
| **`weather`** | 113 | ✅ 示例 |
| **`rss`** | 103 | ✅ 示例 |
| **`calendar`** | 102 | ✅ 示例 |

**根因**：这三个示例插件（以及 `ai_image`）用了 `fmt.Printf` 打日志 ⇒
裸文本进入 stdout 协议流 ⇒ 内核报非法帧并**丢弃该日志**。

### 修法（已完成）

4 个插件共 **14 处** `fmt.Printf` → `log.Printf`，并补 `"log"` import
（`calendar`/`rss`/`weather` 原先没 import log；`ai_image` 已有）。

★ 注意：`internal/plugins/cli` 里也有一处 `fmt.Println`，但它是
**进程内插件**（`plugin.RegisterFactory`，编译进内核），
写的是内核的真实终端 ⇒ **不是缺陷**。判据只针对"子进程插件"
（`package main`）。

### 判据（`tools/hmapdev/example_stdout_test.go`，2 条）

1. `TestExamplePluginsDoNotWriteStdout` —— 扫描全部**子进程**示例插件
   （实测 20 个），剥注释后不得含 `fmt.Printf/Println/Print/os.Stdout`；
   报错时给出**文件:行号**
2. `TestExamplePluginsUseStderrLogging` —— 反向钉住：至少 weather/rss/calendar
   真的在用 `log.Printf`（否则"删光日志"也能过第一条）

### 内核侧：让非法帧报错**可定位**（这是对第三方插件的通用修复）

原报错只有 `收到非法 JSON 帧（N 字节）: invalid character 'x'`：
不显示内容（不知是哪句话）、不提原因（看起来像内核故障）。
1038 条报错的定位花了一整轮排查 —— 就因为缺这两样。

现在报错含**三要素**：

```
[proc] bad_probe 收到非法 JSON 帧（19 字节）: invalid character 'b' looking for beginning of value
        ↑ 内容摘要: [bad_probe] started
        可能原因：插件往 **stdout** 打印了日志，而 stdout 是JSON-RPC **协议通道**（stderr 才是日志通道）。
        修法：把插件里的 fmt.Printf/fmt.Println 改成 log.Printf（脚手架模板 1.4.0 起已修正）。
```

（`summarizeFrame` 做控制字符替换 + 截断，保证单行且不刷屏。）

判据在 `internal/plugin/proc/process_test.go`：
`TestSummarizeFrameIsReadable`、`TestIllegalFrameErrorIsActionable`。
**变异 2/2 咬住**（去掉 summarizeFrame 调用 ⇒ 红；报错不提原因 ⇒ 红）。

## 升级已装的 SDK store 与 hmapdev 二进制（操作步骤）

!!! danger "顺序不能反 —— 否则会装出「二进制新、store 旧」的组合"
    **模板是 `//go:embed` 进 hmapdev 二进制的**（`tools/hmapdev/proc_runtime.go`），
    而 **SDK 接口（`sdk/`）来自 store**。二者必须**同代**：

    | 组合 | 结果 |
    |---|---|
    | 旧二进制 + 旧 store | 自洽（但拿不到新能力） |
    | 新二进制 + 新 store | ✅ 自洽 |
    | **新二进制 + 旧 store** | ⚠️ 模板的 `procPluginMgr` 有 6 个方法，旧接口只有 3 个 ⇒ 生成物**多出 3 个方法**（Go 允许，但新方法调用的 `plugin.mgrStop` 内核可能不认） |
    | **旧二进制 + 新 store** | ❌ 模板 3 个方法 vs 接口 6 个 ⇒ 生成物**不满足接口** ⇒ **编译失败** |

    ⇒ **先升级 store，再升级二进制**（或两者同一批做完）。

### 步骤（本机，用源码）

```bash
# 1) 用**当前**hmapdev 把本机源码装成 store（--from 在旧版也支持）
hmapdev sdk install --from <SDK 源码目录>     # 例：third_party/homeagent-sdk

# 2) 重编 hmapdev 二进制（模板 embed 进去了，必须重编）
cd <SDK 源码>/tools/hmapdev
CGO_ENABLED=1 go build -tags onnxruntime -o /tmp/hmapdev-new .   # TAG_ARGS 视内核而定

# 3) 原子替换（先备份！）
cp /usr/local/bin/hmapdev /usr/local/bin/hmapdev.bak.$(date +%Y%m%d-%H%M%S)
install -m 0755 /tmp/hmapdev-new /usr/local/bin/hmapdev
```

### 验证（三步都要过）

```bash
hmapdev --version                                  # 版本号
strings /usr/local/bin/hmapdev | grep -c mgrStop   # ≥1 ⇒ 新模板已 embed
diff <源码>/tools/hmapdev/templates/proc_main.go.tmpl \
     ~/.hologram/hmapdev/sdk/<ver>/tools/hmapdev/templates/proc_main.go.tmpl   # 必须无差异
```

★ **`diff` 陷阱**：本机 PATH 上的 `diff` 是 HarmonyOS 工具链里的一个
**坏二进制**（`/opt/huawei/harmonyos/ohos-sdk/linux/toolchains/diff`），
对**不同**的文件也返回 **exit 0 且无输出** ⇒ 会给出"已同步"的**假结论**。
用 `cmp` 或 Python 比对，或显式用 `/usr/bin/diff`。

### 判据（自动守"三处同步"）

`internal/plugin/proc/bridge_sync_test.go` 有 3 条，任一不同步即红：

1. `TestBridgeCoversEveryPublishedPluginMgrMethod` —— SDK 方法集 ↔ 能力表
2. `TestBridgeDispatchesEveryPluginMethod` —— 内核 Handle 分派
3. `TestHmapdevTemplateImplementsAllPluginMgrMethods` —— **hmapdev 模板**

第 3 条直接读 `third_party/homeagent-sdk/tools/hmapdev/templates/proc_main.go.tmpl`，
所以**源码模板**始终被守；store 里的副本靠上面的 `diff` 验证。

!!! tip "什么时候必须重编"
    只有一种情况：**内核跨了主版本**（如 1.x → 2.x）。
    主版本跃迁是破坏性变更的定义，此后的帧格式与共享段布局都不兼容，
    必须用配套 `hmapdev` 重编。

    **中版本跃迁（如 1.1 → 1.2）不需要重编**：1.x 内的新增全部是
    「插件调用、内核实现」，不调新面就不受影响。

## 协议兼容性：只认主版本号

**没有独立的协议版本号。** 兼容性由双方版本号的**主版本段**表态：
内核的 `meta.Version` 与插件的 `sdk_version`（插件包里的 `sdk_version` 字段，
即编译时链接的 SDK 版本）。

| 内核 | 插件 SDK | 结果 |
|---|---|---|
| 1.4.0 | 1.0.0 / 1.1.0 / 1.3.0 | ✅ 建链（同主版本） |
| 1.4.0 | 2.0.0 | ❌ 拒绝（跨主版本） |

**为什么不设独立的协议号**：语义版本变动频繁（修 bug、加字段），如果每个
这类改动都要人去升一个额外的号，那个号必然会被漏升而腐烂（历史实证：
曾有一个 `ProtocolVersion` 常量，多次布局变更里只升过一次，而对应的守卫
测试长期处于失败状态、无人察觉）。让兼容性只挂在**始终会被发的那个号**上，
就没有「忘同步」这个失效模式。

跨主版本错配**不会静默失效**——握手时会显式拦下，并提示用配套 `hmapdev` 重编。
此外握手还会比对 `field_count`（StageContext 描述符个数）：它决定标志位基址，
不一致会让插件读到错位字段，因此也在握手上显式拒绝。

## 怎么确认自己在用什么

装的 SDK 版本：

```bash
hmapdev sdk current
hmapdev sdk list
```

插件声明的目标版本在 `plg.json` 的 `sdk` 字段。若该版本不在本地存储里，
`hmapdev` 会**明确报错**，不静默降级 —— 静默降级会产出与内核协议不匹配的包，
那种失败要到运行时才暴露。

## 文档站对应的版本

本页与 [API 参考](api/index.md) 由 `tools/apidoc` 从源码生成，
内容随源码一起演进。发现文档与代码不一致时，**改的是源码注释**，
`go run ./tools/apidoc` 重新生成即可（见下方「维护」）。

## 维护（给 SDK 维护者）

```bash
cd hologramagent-sdk
go run ./tools/apidoc -pkgdir ./sdk -out /tmp/api.json
go run ./tools/apidoc/gensite -api /tmp/api.json -out ./docs -examples ./example
mkdocs serve     # 本地预览
mkdocs build     # 产出 site_build/
```

API 面的**能力分层**（哪些 API 仅内置可用）记在 `tools/apidoc/tiers.json`，
每条裁定都附源码依据 —— 改这里而不是改生成物。
