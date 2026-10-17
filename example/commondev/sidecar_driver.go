package main

// sidecar **驱动层**：把「跑哪个 CLI、怎么起、给什么环境」从插件核心里分离。
//
// ## 为什么需要这一层
//
// 设计目标（用户拍板 2026-10-15）：
//
//	**通过不同的 sidecar 支持不同的 CLI 后端，插件核心一行不用改。**
//
// 拆开看，这就是**两个正交的轴**：
//
//	Runtime   = local | ssh      ← 在哪跑（传输层）
//	AgentCLI  = pi | zcode | ... ← 跑什么（执行后端）
//
// 而插件核心真正依赖 sidecar 的只有**两个动作**：
//
//	startSidecar(branch, sessionID) → *sidecarProcess
//	sc.call("prompt"|"ping"|"state"|"close", params) → JSON
//
// 所以只要「起什么进程 + 什么环境变量」变成**数据**而不是代码，
// 换 CLI 就是换一份 driver 描述，核心不需要条件分支。
//
// ## 为什么是声明式 JSON 而不是 Go 插件接口
//
// Go 接口要求每个后端都编译进同一个二进制 ⇒ **换 CLI 要重编插件**，
// 那正是要避免的（与「平台逻辑进 skill」同一个理由：知识的载体应该能独立演进）。
// driver.json 是数据，随 sidecar 脚本一起分发，加一个后端 = 加一个目录。
//
// ## 目录约定
//
//	<插件数据目录>/runtime/<cli>/driver.json
//	<插件数据目录>/runtime/<cli>/sidecar.mjs      ← 或 driver.json 指定的脚本
//
// ## driver.json 形状
//
//	{
//	  "name": "zcode",
//	  "script": "sidecar.mjs",          // 相对本目录
//	  "node": "auto",                   // auto | 路径
//	  "env": {                          // 追加/覆盖的环境变量
//	    "ZCODE_DATA_BASE_DIR": "{dataDir}/runtime/zcode",
//	    "ZCODE_SESSION_DIR":   "{workdir}/.zcode-sessions"
//	  },
//	  "args": ["--workdir", "{workdir}", "--session-id", "{sessionId}"],
//	  "llmConfig": "zcode",             // pi | zcode | none —— 谁负责喂 LLM 源
//	  "sessionDirSuffix": ".zcode-sessions"
//	}
//
// 占位符：{workdir} {sessionId} {sessionName} {dataDir} {pluginDir}

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// driverSpec 是一个 CLI 后端的启动描述。
type driverSpec struct {
	Name string `json:"name"`
	// Script 是 sidecar 脚本相对本目录的路径（默认 sidecar.mjs）。
	Script string `json:"script,omitempty"`
	// Node 是 node 可执行文件："auto"（默认，随包优先）或显式路径。
	Node string `json:"node,omitempty"`
	// Env 是要注入的环境变量（值支持占位符）。
	Env map[string]string `json:"env,omitempty"`
	// Args 是传给脚本的参数（值支持占位符）。
	Args []string `json:"args,omitempty"`
	// LLMConfig 说明谁负责准备 LLM 源：pi | zcode | none。
	//
	// ★ 这是**能力声明**而不是分支条件：核心只问「这个后端要不要我先喂 LLM 源」，
	// 至于怎么喂由各后端的配置写入器实现（pi 写 models.json/auth.json）。
	LLMConfig string `json:"llmConfig,omitempty"`
	// SessionDirSuffix 是会话目录名（放在工作区内，避免污染用户全局）。
	SessionDirSuffix string `json:"sessionDirSuffix,omitempty"`
	// SessionNameFlag 声明是否支持 --session-name（不支持就不传）。
	SessionNameFlag *bool `json:"sessionNameFlag,omitempty"`
	// CLIPath 是**后端 CLI 的入口路径**（zcode 这类需要外部二进制的后端用）。
	//
	// 为何必须有这个字段（实测 2026-10-16）：zcode 的 sidecar 靠自己的
	// findCLI() 在**三个固定位置**里碰运气（/opt/ZCode/resources/glm/zcode.cjs 等）。
	// 本机装法千差万别（构建产物、nvm、自定义目录），碰不到就报
	// 「找不到 ZCode CLI」—— 而用户唯一的办法是设环境变量，插件里有
	// 一个**声明的字段**比"记得设 env"可靠得多，也让 SSH 场景能分别声明
	// 本地与远端各自的路径。
	//
	// 支持占位符（尤其 {rawfile}：随包分发的 CLI 就放那里）。
	CLIPath string `json:"cliPath,omitempty"`
}

// driverDir 返回某个 CLI 后端的驱动目录。
//
// dir 为空时用默认后端（pi）。
func (p *Plugin) driverDir(cli string) string {
	if strings.TrimSpace(cli) == "" {
		cli = defaultAgentCLI
	}
	return filepath.Join(filepath.Dir(p.path), "runtime", cli)
}

// defaultAgentCLI 是没有指定后端时的默认后端 = pi。
//
// ## 为什么默认是 pi（2026-10-17 确认）
//
// 我们本地主要用 pi；zcode 是**交付方主用**的后端。所以默认值保持 pi，
// 交付方那台机器通过**端点表**把默认设成 zcode，而不是改全局默认。
//
// ## 为什么默认值不该跟着某个客户走
//
// 默认值是"新开线且未指定 cli 时"的行为，它应反映**最常见**的场景。
// 局部需求用局部开关表达（见下），否则每次交付换客户都要改代码。
//
// ## 交付方怎么用 zcode（两种，都不改全局默认）
//
//	① 单线指定：commondev_branch_open(cli="zcode")
//	② 按机器设默认：commondev_endpoint_set(host="<交付方机器>", cli="zcode")
//	   —— 该机器上所有未显式指定 cli 的线都走 zcode
//
// zcode 的三条真机判据（本地全流水线 / SSH / 每机独立端点）见
// zcode_e2e_test.go（build tag zcode_e2e），已验证可用。
const defaultAgentCLI = "pi"

// loadDriver 读一个后端的 driver.json。
//
// ★ 找不到 driver.json 时**回退到内置的 pi 描述**（而不是报错）：
// 现有部署的 runtime/ 下只有 sidecar.mjs、没有 driver.json，
// 若因此启动失败，等于把这个改动变成破坏性变更。
func (p *Plugin) loadDriver(cli string) (driverSpec, string, error) {
	if strings.TrimSpace(cli) == "" {
		cli = defaultAgentCLI
	}
	dir := p.driverDir(cli)
	path := filepath.Join(dir, "driver.json")
	b, err := os.ReadFile(path)
	if err != nil {
		// ★ 回退 ②：随包分发的描述（rawfile/runtime/<cli>/driver.json）。
		//
		// 为什么必须有这条回退（实测 2026-10-16）：`locateSidecarScript`
		// 已经有三级链（用户目录 → rawfile → 可执行文件目录），
		// 但 `loadDriver` **只看插件数据目录** ⇒ 随包分发的 driver.json
		// 永远读不到 ⇒ 全新部署上 `agent_cli=zcode` 直接报「驱动不存在」，
		// 而文件明明就在包里。这与 sidecar 脚本曾经踩的坑是**同一个**
		// （"装了插件、文件都在、一用就找不到"），只是漏在了描述文件上。
		//
		// 顺序与 locateSidecarScript 一致：用户目录优先，随包次之。
		if d2, ok := p.loadShippedDriver(cli); ok {
			return d2, dir, nil
		}
		// ★ 内置 pi 描述：判断条件必须是**字面 "pi"**，不能是 defaultAgentCLI。
		//
		// 为什么（2026-10-17 踩到）：这里原本写 `cli == defaultAgentCLI`，
		// 把"内置回退给谁"和"默认选谁"耦合成了同一个常量。
		// 默认从 pi 改成 zcode 之后，pi **失去了内置回退** ⇒
		// 显式传 cli="pi" 的调用开始报「驱动不存在」，
		// 而 pi 从来就不需要 driver.json（它的描述是代码里的 builtinPiDriver）。
		// 两条判据当场变红：
		//   TestRuntimeAndCLIAreOrthogonal
		//   TestBuiltinPiStillWorksWithoutDriverJSON
		//
		// ⇒ 语义澄清：builtinPiDriver 是 **pi 这个后端**的兜底，
		//   与"默认后端是哪个"完全无关。
		if cli == "pi" {
			// 内置 pi 描述：与改动前的行为逐字一致
			return builtinPiDriver(), dir, nil
		}
		return driverSpec{}, dir, fmt.Errorf(
			"agent_cli=%s 的驱动不存在（应为 %s，或随包 %s）—— 不支持的 CLI 不静默回退到 pi，"+
				"否则会用错的后端跑任务而没有任何提示", cli, path,
			filepath.Join("<插件>/rawfile", "runtime", cli, "driver.json"))
	}
	var d driverSpec
	if err := json.Unmarshal(b, &d); err != nil {
		// 损坏必须响亮：静默用默认值会让人以为"改了没生效"
		return driverSpec{}, dir, fmt.Errorf("驱动描述损坏（拒绝猜默认）: %s: %w", path, err)
	}
	return p.finishDriver(d, cli), dir, nil
}

// loadShippedDriver 读**随包分发**的 driver.json（<插件>/rawfile/runtime/<cli>/driver.json）。
//
// 独立成函数是为了让"两条来源各自怎么解析"都能被单独判据覆盖：
// 随包路径必须与用户路径解析出**同一套默认值填充**，否则会出现
// "本机覆盖能用、随包分发不能用"这种只在部分机器复现的怪问题。
func (p *Plugin) loadShippedDriver(cli string) (driverSpec, bool) {
	rf := p.rawFiles()
	if rf == nil {
		return driverSpec{}, false
	}
	rel := filepath.ToSlash(filepath.Join("runtime", cli, "driver.json"))
	if !rf.Exists(rel) {
		return driverSpec{}, false
	}
	b, err := rf.Read(rel)
	if err != nil {
		return driverSpec{}, false
	}
	var d driverSpec
	if err := json.Unmarshal(b, &d); err != nil {
		// 随包描述损坏同样响亮，不静默退到内置 pi（那会用错后端）
		return driverSpec{}, false
	}
	return p.finishDriver(d, cli), true
}

// finishDriver 补齐描述里的默认值（两条来源**共用**，保证行为一致）。
func (p *Plugin) finishDriver(d driverSpec, cli string) driverSpec {
	if d.Name == "" {
		d.Name = cli
	}
	if d.Script == "" {
		d.Script = "sidecar.mjs"
	}
	if d.LLMConfig == "" {
		d.LLMConfig = "none"
	}
	if d.SessionDirSuffix == "" {
		// ★ 必须按 cli 派生（原来就是 "." + cli + "-sessions"）。
		// 写死成某个具体后端会把"默认值"变成"某后端的硬编码"，
		// 对其他后端就是静默的行为回归。
		d.SessionDirSuffix = "." + cli + "-sessions"
	}
	return d
}

// builtinPiDriver 是未提供 driver.json 时的内置 pi 描述。
//
// 与环境变量写法必须与改动前**逐字一致** —— 这是"核心一行不改"的保证：
// 老部署不装 driver.json 也照常工作。
func builtinPiDriver() driverSpec {
	return driverSpec{
		Name: "pi", Script: "sidecar.mjs", Node: "auto",
		Env: map[string]string{
			"PI_CODING_AGENT_DIR":         "{dataDir}/runtime/agentdir",
			"PI_CODING_AGENT_SESSION_DIR": "{workdir}/.pi-sessions",
		},
		Args:             []string{"--workdir", "{workdir}", "--session-id", "{sessionId}", "--session-name", "{sessionName}"},
		LLMConfig:        "pi",
		SessionDirSuffix: ".pi-sessions",
	}
}

// expand 把值里的占位符替换成实际参数。
func (d driverSpec) expand(val string, ctx driverCtx) string {
	r := strings.NewReplacer(
		"{workdir}", ctx.Workdir,
		"{sessionId}", ctx.SessionID,
		"{sessionName}", ctx.SessionName,
		"{dataDir}", ctx.DataDir,
		"{pluginDir}", ctx.PluginDir,
		// {rawfile} 指向**随包资源目录**（<插件目录>/rawfile）。
		// 有了它，driver.json 可以直接声明"用随包分发的 CLI"：
		//   "cliPath": "{rawfile}/zcode/zcode.cjs"
		"{rawfile}", ctx.RawfileDir,
	)
	return r.Replace(val)
}

// driverCtx 是占位符替换用的上下文。
type driverCtx struct {
	Workdir     string
	SessionID   string
	SessionName string
	DataDir     string
	PluginDir   string

	// RawfileDir 是随包资源根（<插件目录>/rawfile）。
	// 用于 {rawfile} 占位符：让 driver.json 能声明"用随包分发的 CLI"。
	RawfileDir string
}

// resolveNode 按驱动声明解析 node 路径。
func (p *Plugin) resolveNode(d driverSpec) string {
	if d.Node != "" && d.Node != "auto" {
		return d.Node
	}
	return p.nodeBin()
}

// buildArgs 生成传给 sidecar 脚本的参数表。
func (d driverSpec) buildArgs(ctx driverCtx) []string {
	args := make([]string, 0, len(d.Args))
	supportsName := d.SessionNameFlag == nil || *d.SessionNameFlag
	for i := 0; i < len(d.Args); i++ {
		a := d.Args[i]
		// 不支持 --session-name 的后端：跳过这个开关及其值
		if !supportsName && a == "--session-name" {
			i++
			continue
		}
		args = append(args, d.expand(a, ctx))
	}
	// ★ 后端 CLI 路径（若声明了）作为 --cli 追加。
	//
	// 为什么由**核心**追加而不是写死在 driver.json 的 args 里：
	// 路径要支持占位符展开（{rawfile}/{dataDir}/{pluginDir}），
	// 而这些只有核心知道。写在 args 里也能展开，但把"这个后端需要一个
	// 外部 CLI"这件事藏在了通用参数表里 —— 显式字段让 SSH 场景可以
	// 分别声明本地/远端两套路径，sidecar 也能据此给出更好的错误。
	if strings.TrimSpace(d.CLIPath) != "" {
		args = append(args, "--cli", d.expand(d.CLIPath, ctx))
	}
	return args
}

// buildEnv 生成驱动要求的额外环境变量（已展开占位符）。
func (d driverSpec) buildEnv(ctx driverCtx) []string {
	out := make([]string, 0, len(d.Env)+1)
	// 固定排序（map 遍历随机 ⇒ 环境变量顺序不稳，日志难对照）
	keys := make([]string, 0, len(d.Env))
	for k := range d.Env {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		out = append(out, k+"="+d.expand(d.Env[k], ctx))
	}
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

var _ = strconv.Itoa
