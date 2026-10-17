# 自定义 CLI 后端（示例：ZCode）

> ## ★ 边界：**内嵌只做 pi**
>
> 插件**内嵌支持的后端只有 pi**（`runtime/pi/`，不装 driver.json 也照常工作）。
> 本文及同目录示例是给**想让插件驱动别的 CLI** 的用户用的。
>
> 这不是"官方内嵌支持"——它们是**参考实现**：插件不承诺跟着上游 CLI 变更同步更新，
> 你要根据自己的 CLI 版本调整（尤其参数名与输出格式）。
>
> 沿用本仓的一条纪律：**内核/插件只保证机器必须保证的事，策略与平台知识交给能
> 独立演进的地方**（见 skill `commondev-discipline` 里"平台知识进 skill"的同一条理由）。

## 怎么加一个后端

插件按 **数据**（不是代码）选后端。加一个 CLI 只要放两个文件：

```
<插件数据目录>/runtime/<cli>/driver.json     ← 声明：起什么命令、什么环境
<插件数据目录>/runtime/<cli>/sidecar.mjs     ← 实现：把 CLI 包成 JSON-RPC
```

**插件核心一行都不用改**（这是可验证的：`TestCoreDoesNotHardcodePiLaunchDetails`
会断言 `startSidecar` 里不出现任何 pi 专有细节）。

开分支时指定后端：

```
commondev_branch_open(project="X", branch="feat/a", agent_cli="zcode", workdir=...)
```

### 两种来源：随包分发 vs 本机覆盖

`driver.json` 与 sidecar 脚本都按同一条优先级链找：

| 顺序 | 位置 | 用途 |
|---|---|---|
| ① | `<插件数据>/runtime/<cli>/` | **本机覆盖**（最高优先）—— 指到本机真实的 CLI 路径 |
| ② | `<插件目录>/rawfile/runtime/<cli>/` | **随包分发** —— 升级插件即带上 |
| ③ | `<可执行文件目录>/` | 开发期 |

**为什么要 ②**：`cliPath` 的随包默认值是 `{rawfile}/zcode/zcode.cjs`，
而 CLI 有 28MB、按需分发 —— 所以**多数机器上它不存在**，此时必须由 ① 覆盖：

```bash
# 本机覆盖：只改这两个文件，不影响随包内容（升级插件不会被覆盖）
mkdir -p <插件数据>/runtime/zcode
cp <随包>/rawfile/runtime/zcode/sidecar.mjs <插件数据>/runtime/zcode/
# 然后编辑 <插件数据>/runtime/zcode/driver.json，把 cliPath 指向本机 zcode.cjs
```

若两处都没有，错误会**列出找过的每个位置**并告诉你改哪里（不要让它静默失败）。

## sidecar 必须实现的契约

JSON-RPC over stdin/stdout，方法与 pi 的 sidecar **完全相同**：

| 方法 | 入参 | 必须返回 |
|---|---|---|
| `ping` | — | `{ok, session_id, cli_version?}`（握手，10s 内） |
| `prompt` | `{text, session_id?}` | `{ok, session_id, reply}` ← **`session_id` 必须回** |
| `state` | — | `{ok, session_id, messages}` |
| `close` | — | `{ok}` |

**为什么 `prompt` 必须回 `session_id`**：插件在投递后**断言会话未漂移**
（设计 §3.3）。不回或回错值，插件会响亮失败——那是防"悄悄换了个会话"的机制。

### ★ 会话 id 有两个**不同**的东西，别混

这是 ZCode 后端实测踩到的真坑（2026-10-16，真机复现）：

| 名字 | 长什么样 | 谁产生 | 能当 `--resume` 目标吗 |
|---|---|---|---|
| **插件标签** | `br-<project>-<branch>` | 插件（`--session-id` 传进来） | ❌ **绝不能** |
| **后端真实 id** | `sess_xxxxxxxx-...` | 后端首轮返回 | ✅ 只有它能 |

初版把 `args.sessionId`（标签）直接当 `--resume` 目标 ⇒ 首轮就执行
`--resume br-xxx`，ZCode 报 `Session not found: br-xxx`——
**表现为"第一次投递必定失败"**，第二次才可能好，非常像"偶发网络问题"。

正确形状（示例就是这么写的）：

```js
let desiredSessionId = args.sessionId || "";  // 插件标签，只用于回报
let zcodeSessionId   = "";                    // 真实 id，只有它能 resume
// 首轮：不传 --resume；从 {type:"result"} 行取回 sess_...
if (zcodeSessionId) argv.push("--resume", zcodeSessionId);
// 回报时：有真实 id 报它，否则退回标签
session_id: zcodeSessionId || desiredSessionId || null,
```

`params.session_id`（插件在 prompt 里传的）**同样是标签**，不要拿它赋值给
"真实 id"变量——那等于把上面的修法又改回去了。

### ★ 声明的 CLI 路径必须**验存**

`driver.json` 的 `cliPath` 可能指向一个**不存在的**文件（例如随包默认值
`{rawfile}/zcode/zcode.cjs`，而包里并没带那个 28MB 的 CLI）。
若 `if (args.cliPath) return args.cliPath` 直接返回，Node 会在 spawn 时抛
`MODULE_NOT_FOUND`，用户看到的是一坨 `Module._resolveFilename` 堆栈，
**完全看不出"CLI 不存在"**，也不知道去哪儿配。

正确做法：把"声明的"和"探测到的"一起验存，找不到时**列出找过的每个位置
及其来源**，并给出修复动作（改哪个文件 / 设哪个环境变量）。

## driver.json 字段

```jsonc
{
  "name": "zcode",                  // 后端名（与目录名一致）
  "script": "sidecar.mjs",          // 相对本目录
  "node": "auto",                   // auto = 随包优先；ssh 场景会走远端 PATH
  "sessionNameFlag": false,         // 声明是否支持 --session-name（不支持就不传）
  "sessionDirSuffix": ".zcode-sessions",
  "llmConfig": "none",              // pi | zcode | none —— 谁准备 LLM 源
  "env":  { "ZCODE_DATA_BASE_DIR": "{dataDir}/runtime/zcode" },
  "args": ["--workdir", "{workdir}", "--session-id", "{sessionId}", "--mode", "guarded"]
}
```

占位符：`{workdir}` `{sessionId}` `{sessionName}` `{dataDir}` `{pluginDir}`

- **`sessionNameFlag: false` 是真实的坑**：ZCode CLI 没有 `--session-name`，
  传了会 `Unknown option` 直接退到 usage——**一行参数毁掉整条命令**。
- **`llmConfig`**：`pi` = 插件先写 `models.json`/`auth.json`（复用内核 LLM 源）；
  `none` = 后端自己管。**没有 `zcode` 这个实现**——我们只知道
  `ZCODE_BUILTIN_PROVIDER_CONFIG_FILE` 这个环境变量，**没实测过它的文件格式**，
  凭猜测写是"静默写坏"（ZCode 起不来会表现为"模型不回复"，排查方向完全错）。
  要用内核源，请先实测格式再实现写入器。

## ZCode 示例的实测契约（真机 `zcode 0.16.9`，非猜测）

```
zcode.cjs --prompt <文本> --output-format stream-json --cwd <目录> --mode <值> [--resume <sess_id>]
```

| 点 | 实况（真机核实） |
|---|---|
| `--mode` | 合法值 **`build\|edit\|plan\|yolo\|guarded`**；**不传默认 `yolo`**（`run.ts: DEFAULT_HEADLESS_PROMPT_MODE`）= 绕过全部授权询问。示例用 `guarded`。 |
| `--output-format` | **存在**（`--help` 里**不显示**，是隐藏参数 —— 别因为 help 没有就以为不存在）。`stream-json` 才有逐事件 NDJSON + **末行** `{type:"result", sessionId, response}`，sidecar 就靠这一行。 |
| `--resume` | 会话延续。**只接受真实 `sess_...` id**，首轮不能传（见上文会话 id 一节） |
| `--allowed-tools` | ⚠ **不存在**——只能用 `--disallowed-tools` |
| `--max-turns` | ⚠ **不存在** |
| `--session-name` | ⚠ **不存在**（`sessionNameFlag: false`） |

> **教训**：`--output-format` 这一条曾被基于 `--help` 误判为"不存在"，
> 差点把 sidecar 改成解析非结构化文本。**判定参数存在性必须用真机试跑**
> （`--prompt hi --output-format stream-json` 直接看输出），`--help` 只覆盖公开参数。

> ⚠️ **超时必须杀进程树**：ZCode 会派生工具子进程（bash 等），
> 只杀父进程会留下孤儿继续跑，且仍占着工作目录。示例里用 `detached: true` +
> `process.kill(-pid)` 整组杀。

## 远端（SSH）场景的额外注意

`runtime=ssh` 时用的是**同一份** driver 声明与 sidecar 契约，但：

1. **环境变量必须显式前置**（`ssh host env K=V … node …`）——ssh 不继承本地环境。
2. **node 用远端的**（裸 `node` 走远端 PATH），不是本地随包分发的绝对路径。
3. **脚本要先推到远端**（插件会自动 `scp` 一次，见 `pushSidecar`）。
4. **不交互**：`BatchMode=yes`。没配免密必须立刻失败，否则会挂在密码提示上
   直到工具调用超时——那表现为"拉起 agent 卡住"而不是"没配免密"。

## 验证

```bash
cd <插件源码>
# 结构性判据（不需要真 CLI）
go test -count=1 -run 'TestCoreDoesNotHardcode|TestSwappingDriverSwapsBackend|TestSSH|TestZcode|TestShippedDriver' ./...

# ★ 真机端到端（需要真实 ZCode CLI + 可用 provider 配置）
HA_ZCODE_CLI=<...>/zcode.cjs \
HA_ZCODE_DATA_DIR=<插件数据>/runtime/zcode \
  go test -tags zcode_e2e -run TestZcodeFullPipeline -v .

# ★ SSH 远端（需要 sshd + localhost 免密）
HA_SSH_E2E=1 HA_ZCODE_CLI=... HA_ZCODE_DATA_DIR=... \
  go test -tags zcode_e2e -run TestZcodeOverSSH -v .
```

`TestSwappingDriverSwapsBackend` 用一个假 CLI 端到端证明：
换 driver 就能换后端，插件核心零改动。

`TestZcodeSidecarArgvBehavior` 用**假 CLI 记录真实 argv** 来判"首轮不得 `--resume`"——
为什么不用源码字符串匹配：初版就是匹配字面串，把变量改个名就绕过了（判据假绿）。

## 给 ZCode 配 LLM 源（实测可用的最小配置）

ZCode 用自己的 provider 配置，**插件不碰它**（`llmConfig: "none"`）。
数据目录由 `ZCODE_DATA_BASE_DIR` 决定，配置落在`<该目录>/.zcode/v2/provider_config.json`：

```json
{
  "schemaVersion": 1,
  "config": {
    "providerConfigRules": { "providerRules": [ {
      "providerId": "llmsproxy", "providerName": "llmsproxy", "enabled": true,
      "config": {
        "group": "standard-personal",
        "personalModelIds": ["AUTO"], "modelOrder": ["AUTO"],
        "access": { "type": "api-key", "apiKey": "sk-..." },
        "api": { "type": "openai-chat-completions", "baseUrl": "http://127.0.0.1:8081/v1" }
      }
    } ] },
    "defaultModelSelection": { "providerId": "llmsproxy", "modelId": "AUTO" }
  }
}
```

实测踩过的三个坑（全部真机复现）：

1. **`schemaVersion` 必须有**，且 `providerRules` 嵌在 `config.providerConfigRules` 下
   —— 放顶层会被静默"恢复"成默认值（文件被改写但你没察觉）。
2. **`group: "standard-personal"` 必须有**。少了它，配置**被读到但不被准入**，
   报 `Error: Model creation failed`；加 `--verbose` 才能看到真因
   `Select a model before continuing`。
3. `api.type` 合法值：`anthropic-messages` / `openai-chat-completions` / `openai-responses`。
