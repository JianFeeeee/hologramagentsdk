// Command gensite 把 apidoc 提取出的 api.json 渲染成文档站的 Markdown 页面，
// 并额外产出一份供浏览器即时检索的索引。
//
// 为什么要「生成」而不是手写：API 面有 100+ 个符号，手抄必然与源码漂移。
// 这里的每个签名、每段说明都直接来自源码注释，因此文档只在「人写的指南」
// 部分才需要人工维护。
//
// 用法：
//
//	go run ./tools/apidoc -pkgdir ./sdk -out /tmp/api.json           # 先提取
//	go run ./tools/apidoc/gensite -api /tmp/api.json -out ./docs      # 再渲染
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 与 extract.go 的 Package 结构对应（两个 main 包不共享代码，故重复声明）。
type Symbol struct {
	Kind        string   `json:"kind"`
	Recv        string   `json:"recv"`
	Name        string   `json:"name"`
	Signature   string   `json:"signature"`
	Doc         string   `json:"doc"`
	DocBrief    string   `json:"doc_brief"`
	File        string   `json:"file"`
	Line        int      `json:"line"`
	Group       string   `json:"group"`
	Exported    bool     `json:"exported"`
	Examples    []string `json:"examples"`
	Deprecated  bool     `json:"deprecated"`
	BuiltinOnly bool     `json:"builtin_only"`
	Tier        string   `json:"tier"`
	TierReason  string   `json:"tier_reason"`
}

type Interface struct {
	Name    string   `json:"name"`
	Doc     string   `json:"doc"`
	Methods []Symbol `json:"methods"`
}

type ConstGroup struct {
	Doc    string   `json:"doc"`
	Consts []Symbol `json:"consts"`
}

type Package struct {
	ImportPath  string       `json:"import_path"`
	Doc         string       `json:"doc"`
	Symbols     []Symbol     `json:"symbols"`
	Interfaces  []Interface  `json:"interfaces"`
	ConstGroups []ConstGroup `json:"const_groups"`
}

// siteSection 是 API 参考的一个页面。
type siteSection struct {
	File  string // 输出文件名（不含 .md）
	Title string // 页面标题
	Desc  string // 页面导语
	Match func(Symbol) bool
}

// 章节划分：按「插件作者想做什么」组织，而不是按 Go 的符号类别。
// 这是文档好不好用的关键——作者是来找「怎么注册工具」的，不是来找 type 的。
var sections = []siteSection{
	{
		File: "tools", Title: "工具（Tools）",
		Desc: "注册 LLM 可调用的工具。工具是插件最主要的能力形态：模型看到 `ToolDef` 的说明后决定是否调用，调用时执行你的 `ToolHandler`。",
		Match: func(s Symbol) bool {
			return s.Name == "RegisterTool" || s.Name == "ToolDef" || s.Name == "ToolHandler" || s.Name == "ToolCall" || s.Name == "ToolResult" || s.Name == "ContentBlock" || s.Name == "ToolCleaner"
		},
	},
	{
		File: "stages", Title: "阶段钩子（Stages）",
		Desc: "在消息处理管道的固定点位插入自己的逻辑。阶段比工具更底层：工具是模型主动调用的，阶段是流程经过时必然触发的。",
		Match: func(s Symbol) bool {
			return s.Group == "stages" || s.Name == "Stage" || s.Name == "StageHandler" || s.Name == "StageScope" || s.Name == "RegisterStage" || strings.HasPrefix(s.Name, "Stage")
		},
	},
	{
		File: "memory", Title: "记忆（Memory）",
		Desc: "三层记忆的读写接口：**图记忆**（三元组关系）、**文档记忆**（带元数据的文档）、**文本记忆**（事件流水）。以及知识库。",
		Match: func(s Symbol) bool {
			switch s.Name {
			case "MemoryAPI", "TextMemoryAPI", "DocMemoryAPI", "KnowledgeAPI", "Entity", "Relation", "Triple", "Doc", "TextEvent", "MediaAttachment", "Knowledge", "Memory", "TextMemory", "DocMemory", "DocQuery", "SocialAPI", "PersonProfile", "SocialRelation", "Social":
				return true
			}
			return strings.HasPrefix(s.Recv, "Memory") || strings.HasPrefix(s.Recv, "Doc") || strings.HasPrefix(s.Recv, "TextMemory") || strings.HasPrefix(s.Recv, "Knowledge")
		},
	},
	{
		File: "channels", Title: "输入 / 输出通道",
		Desc: "通道是插件与外界（设备、其他 Agent、外部系统）交换消息的入口。**输入通道**接收外部消息，**输出通道**把消息投递出去。",
		Match: func(s Symbol) bool {
			switch s.Name {
			case "RegisterInputChannel", "RegisterOutputChannel", "ChannelDef", "CapText", "CapFile", "CapImage", "CapAudio", "CapStructured", "IOInjector", "InjectText", "InjectTextNoMemory", "InjectTextOpts", "InjectInterruptText", "InjectInterruptTextOpts", "InjectInputSync", "InjectInputSyncOpts", "InjectInputMedia", "InjectInputMediaSync", "InjectInputMediaOpts", "InjectInputMediaSyncOpts", "InjectInterruptMedia", "InjectInterruptMediaOpts", "InjectOptions", "ContextPolicyNone", "ContextPolicyPrune", "RecallPolicyNone", "RecallPolicyAuto", "SetToolBlocks", "ValidContextPolicy", "ValidRecallPolicy", "PriorityL1", "PriorityL2", "PriorityL3", "PriorityL4":
				return true
			}
			return false
		},
	},
	{
		File: "settings", Title: "配置（Settings）",
		Desc:  "声明插件自己的配置项，内核会把它渲染到 WebUI 的设置页，并为每个插件维护独立的配置表。",
		Match: func(s Symbol) bool { return s.Name == "SettingsAPI" || s.Name == "ConfigDef" || s.Name == "Settings" },
	},
	{
		File: "lifecycle", Title: "生命周期（Lifecycle）",
		Desc: "插件的启动、停止与卸载回调。停止与卸载是两件事：**停止**是进程/加载状态变化，**卸载**（onRemove）是插件被删除前的清理机会。",
		Match: func(s Symbol) bool {
			switch s.Name {
			case "Plugin", "RegisterStopHandler", "RunStopHandlers", "RegisterOnRemoveHandler", "RunOnRemoveHandlers", "SetAutoRestart", "AutoRestart", "PluginName", "RegisterPluginAPI", "PluginMgr", "PluginMgrAPI", "Settings":
				return true
			}
			return false
		},
	},
	{
		File: "events", Title: "事件（Events）",
		Desc: "订阅内核事件。**注意**：外部分布式插件的事件订阅不走 `Events()`（该接口在外部插件路径上未被注入，恒为 nil），而是由 `hmapdev` 生成的运行时通过 `events.subscribe` 完成。详见下方说明。",
		Match: func(s Symbol) bool {
			return s.Name == "EventSubscriber" || s.Name == "Event" || s.Name == "EventType" || s.Name == "EventHandler" || s.Name == "Events" || strings.HasPrefix(s.Name, "Event")
		},
	},
	{
		File: "llm", Title: "LLM 调用",
		Desc:  "让插件自己调用模型（而不是只等模型来调你）。",
		Match: func(s Symbol) bool { return s.Name == "LLMAPI" || s.Name == "LLM" },
	},
	{
		File: "bridge", Title: "桥接装配点（Bridge）",
		Desc: "以下方法**不是给插件业务代码调的**——它们由 `hmapdev` 生成的运行时在启动时调用，用来把内核能力注入到 SDK 实例。列在这里是为了让「公开 API 面」完整，并说明每个注入点对应什么能力。",
		Match: func(s Symbol) bool {
			// 只匹配真正的「装配注入点」：Set*API 系列，以及三个 Register/Injector 注入点。
			// 注意不能用「Set 开头」一刀切 —— SetAutoRestart / SetToolBlocks 是
			// 插件业务代码会调的公开方法，不属于装配面，分别归入 lifecycle / channels。
			if s.Recv == "PluginSDK" && strings.HasPrefix(s.Name, "Set") && strings.HasSuffix(s.Name, "API") {
				return true
			}
			switch s.Name {
			case "APIRegistrar", "ToolRegistrar", "InputChannelRegistrar", "OutputChannelRegistrar", "OutputChannelUnregistrar",
				"SetIOInjector", "SetInputChannelRegistrar", "SetOutputChannelRegistrar", "SetOutputChannelUnregistrar", "SetEventSubscriber":
				return true
			}
			return false
		},
	},
	{
		File: "misc", Title: "其他类型",
		Desc:  "剩余的类型与方法：`PluginSDK` 本体的访问器、`StageContext` 的并发控制，以及多模态辅助类型。没有归入上面任何一个主题，但可能仍会用到。",
		Match: func(s Symbol) bool { return true },
	},
}

func main() {
	apiPath := flag.String("api", "/tmp/api.json", "apidoc 提取的 JSON")
	outDir := flag.String("out", "./docs", "文档站目录")
	exDir := flag.String("examples", "./example", "示例插件目录（用于抽取真实用法）")
	flag.Parse()

	raw, err := os.ReadFile(*apiPath)
	if err != nil {
		log.Fatalf("读取 %s: %v", *apiPath, err)
	}
	var pkg Package
	if err := json.Unmarshal(raw, &pkg); err != nil {
		log.Fatalf("解析 %s: %v", *apiPath, err)
	}

	apiDir := filepath.Join(*outDir, "api")
	if err := os.MkdirAll(apiDir, 0o755); err != nil {
		log.Fatal(err)
	}

	// 收集所有待渲染的符号名，再扫 example/ 取真实用法。
	want := map[string]bool{}
	for _, s := range pkg.Symbols {
		if s.Exported {
			want[s.Name] = true
		}
	}
	for _, it := range pkg.Interfaces {
		want[it.Name] = true
	}
	usages := scanUsages(*exDir, want)

	// 中文检索关键词（弥补英文注释搜不到中文的问题）。
	kw, err := loadKeywords()
	if err != nil {
		log.Printf("警告：未加载中文检索关键词（%v），中文检索将只能命中中文注释", err)
	}

	used := map[string]bool{}
	var index []indexEntry

	for _, sec := range sections {
		// 本节的接口（先算出来，下面要用它排除重复的 type 符号）。
		var ifaces []Interface
		ifaceNames := map[string]bool{}
		for _, it := range pkg.Interfaces {
			if sec.Match(Symbol{Kind: "type", Name: it.Name}) {
				ifaces = append(ifaces, it)
				ifaceNames[it.Name] = true
			}
		}

		var picked []Symbol
		for _, s := range pkg.Symbols {
			if !s.Exported || !sec.Match(s) || used[key(s)] {
				continue
			}
			// 接口已作为 ifaces 单独渲染（带方法表），这里跳过它的 `type`
			// 符号，否则同一个接口会既出现在符号区、又出现在接口区，
			// 检索索引里也会出现两条。
			if s.Kind == "type" && ifaceNames[s.Name] {
				used[key(s)] = true
				continue
			}
			picked = append(picked, s)
			used[key(s)] = true
		}
		if len(picked) == 0 && len(ifaces) == 0 {
			continue
		}
		sort.Slice(picked, func(i, j int) bool { return picked[i].Name < picked[j].Name })
		md := renderSection(sec, picked, ifaces, usages)
		if err := writeFile(filepath.Join(apiDir, sec.File+".md"), md); err != nil {
			log.Fatal(err)
		}
		for _, s := range picked {
			index = append(index, indexEntry{
				N: s.Name, S: s.Signature, D: s.DocBrief,
				K: s.Kind, R: s.Recv, P: sec.File,
				B: s.BuiltinOnly, F: s.File, L: s.Line,
				G: kw.lookup(s.Name, s.DocBrief),
			})
		}
		// 接口本身与其方法也要进索引。此前只加了顶层符号，导致
		// `MemoryAPI.Recall` 这类接口方法搜不到（只能靠页面浏览）。
		for _, it := range ifaces {
			index = append(index, indexEntry{
				N: it.Name, S: "type " + it.Name + " interface",
				D: briefOf(it.Doc), K: "interface", P: sec.File,
			})
			for _, m := range it.Methods {
				index = append(index, indexEntry{
					N: m.Name, S: m.Signature, D: m.DocBrief,
					K: "method", R: it.Name, P: sec.File,
					B: m.BuiltinOnly, F: m.File, L: m.Line,
					G: kw.lookup(m.Name, m.DocBrief),
				})
			}
		}
	}

	// 常量单独成页（它们是取值枚举，不是「怎么做」）。
	if md := renderConstants(pkg.ConstGroups); md != "" {
		if err := writeFile(filepath.Join(apiDir, "constants.md"), md); err != nil {
			log.Fatal(err)
		}
		for _, g := range pkg.ConstGroups {
			for _, c := range g.Consts {
				index = append(index, indexEntry{
					N: c.Name, S: "const " + c.Name, D: c.DocBrief,
					K: "const", P: "constants", F: c.File, L: c.Line,
					G: kw.lookup(c.Name, c.DocBrief),
				})
			}
		}
	}

	// 仅内置 API 汇总页——这是「真实的私密边界」的显式落点。
	if md := renderBuiltinOnly(pkg); md != "" {
		if err := writeFile(filepath.Join(apiDir, "builtin-only.md"), md); err != nil {
			log.Fatal(err)
		}
	}

	// 客户端即时检索索引。
	//
	// 除了「按名称」与「按签名」，还带 examples/ 里的真实调用点，
	// 因为用户的核心诉求是「按描述搜到 API」——描述文本与用法片段
	// 一起进索引，搜「发消息」能找到 InjectText，搜「注册工具」能找到 RegisterTool。
	//
	// 去重：同一符号可能同时作为「顶层方法」与「接口方法」被扫到
	// （如 PluginSDK 方法与接口方法共享名字）。按 接收者.名称 去掉重复，
	// 否则搜索结果里同一 API 会出现两次。
	index = dedupeIndex(index)
	sort.Slice(index, func(i, j int) bool { return index[i].N < index[j].N })
	if err := writeJSON(filepath.Join(*outDir, "assets", "api-index.json"), index); err != nil {
		log.Fatal(err)
	}

	// 示例插件总览页（真实调用点也在这里汇总）。
	if err := writeFile(filepath.Join(*outDir, "examples", "index.md"), renderExamples(usages)); err != nil {
		log.Fatal(err)
	}

	// 给 agent 的入口：llms.txt / llms-full.txt（Markdown 正文汇总）。
	// 每个页面的 .md 副本由 build.sh 在 docs/ 定稿后统一复制进站点产物。
	pages, err := writeAgentEntrypoints(*outDir, "")
	if err != nil {
		log.Fatalf("生成 agent 入口失败: %v", err)
	}
	fmt.Fprintf(os.Stderr, "agent 入口：llms.txt + llms-full.txt（覆盖 %d 个页面）\n", pages)

	sort.Slice(pkg.Symbols, func(i, j int) bool { return pkg.Symbols[i].Name < pkg.Symbols[j].Name })
	fmt.Fprintf(os.Stderr, "生成 %d 个章节 + 检索索引 %d 条 → %s\n",
		len(sections), len(index), apiDir)
}

type indexEntry struct {
	N string `json:"n"` // 名称
	S string `json:"s"` // 签名
	D string `json:"d"` // 描述摘要
	K string `json:"k"` // 类别
	R string `json:"r"` // 接收者
	P string `json:"p"` // 所属页面
	B bool   `json:"b"` // 仅内置
	F string `json:"f"` // 源文件
	L int    `json:"l"` // 行号
	// G 是中文检索关键词（同义词/功能词）。
	//
	// 为什么需要：SDK 里 100 个有摘要的符号中 **66 个是英文注释**
	// （`RegisterTool registers a tool that the LLM can call.`），
	// 于是搜「注册工具」根本找不到 RegisterTool —— 而中文是主要受众。
	// 这些词由 tools/apidoc/keywords.json 维护，不改源码注释，
	// 也不依赖机器翻译。
	G []string `json:"g,omitempty"`
}

func key(s Symbol) string {
	if s.Recv != "" {
		return s.Recv + "." + s.Name
	}
	return s.Kind + "." + s.Name
}

func briefOf(doc string) string {
	for _, line := range strings.Split(doc, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// genBanner 是生成页的首页标记。
//
// 必须有：docs/api/*.md 会被提交进仓，而它们下次构建就被覆盖。
// 没有这行提示，别人手改一处再发现改动消失，会以为是自己弄错了。
const genBanner = "<!-- 本页由 tools/apidoc/gensite 从源码生成，请勿手改；要改文档就改 sdk/*.go 的注释。 -->\n\n"

// renderSection 渲染一个 API 章节。
func renderSection(sec siteSection, syms []Symbol, ifaces []Interface, usages map[string][]Usage) string {
	var b strings.Builder
	b.WriteString(genBanner)
	fmt.Fprintf(&b, "# %s\n\n%s\n\n", sec.Title, sec.Desc)

	// 接口优先展示（它们是「能力清单」），再列独立符号。
	for _, it := range ifaces {
		fmt.Fprintf(&b, "## `%s`\n\n", it.Name)
		if it.Doc != "" {
			fmt.Fprintf(&b, "%s\n\n", it.Doc)
		}
		if len(it.Methods) > 0 {
			b.WriteString("| 方法 | 说明 |\n|---|---|\n")
			for _, m := range it.Methods {
				// 锚点必须与标题逐字对应：标题是 `接口.方法`，slug 会把点号丢掉。
				fmt.Fprintf(&b, "| [`%s`](#%s) | %s |\n",
					m.Name, anchor(it.Name+"."+m.Name), escapePipe(m.DocBrief))
			}
			b.WriteString("\n")
			for _, m := range it.Methods {
				b.WriteString(renderSymbol(m, usages[m.Name]))
			}
		}
	}

	for _, s := range syms {
		b.WriteString(renderSymbol(s, usages[s.Name]))
	}
	return b.String()
}

// renderSymbol 渲染单个符号。签名放进代码块便于复制，说明保留原文 markdown。
func renderSymbol(s Symbol, uses []Usage) string {
	var b strings.Builder

	name := s.Name
	if s.Recv != "" {
		name = s.Recv + "." + s.Name
	}
	fmt.Fprintf(&b, "### `%s`\n\n", name)

	if s.BuiltinOnly {
		b.WriteString("!!! warning \"仅内核内置插件可用\"\n")
		if s.TierReason != "" {
			b.WriteString(indent(s.TierReason, "    ") + "\n")
		}
		b.WriteString("\n")
	} else if s.Tier == "bridge" {
		b.WriteString("!!! info \"桥接装配点\"\n")
		b.WriteString(indent(s.TierReason, "    ") + "\n\n")
	}

	b.WriteString("```go\n")
	b.WriteString(strings.TrimSpace(s.Signature))
	b.WriteString("\n```\n\n")

	if s.Doc != "" {
		fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(s.Doc))
	}
	if s.Deprecated {
		b.WriteString("!!! danger \"已废弃\"\n    不要在新代码里使用。\n\n")
	}
	for _, ex := range s.Examples {
		b.WriteString("**示例**\n\n```go\n" + ex + "\n```\n\n")
	}
	if len(uses) > 0 {
		b.WriteString("**示例插件里的真实用法**\n\n")
		b.WriteString("| 插件 | 位置 | 代码 |\n|---|---|---|\n")
		for _, u := range uses {
			fmt.Fprintf(&b, "| [`%s`](../examples/index.md#%s) | `%s:%d` | `%s` |\n",
				u.Plugin, u.Plugin, u.File, u.Line, escapePipe(u.Code))
		}
		b.WriteString("\n")
	}
	if s.File != "" {
		fmt.Fprintf(&b, "<small>`%s:%d`</small>\n\n", s.File, s.Line)
	}
	return b.String()
}

func renderConstants(groups []ConstGroup) string {
	if len(groups) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(genBanner)
	b.WriteString("# 常量与枚举\n\n")
	b.WriteString("SDK 里的取值枚举。其中带「仅内置」标注的取值在内核侧会被夹到较低级别。\n\n")
	for _, g := range groups {
		title := "相关取值"
		if len(g.Consts) > 0 {
			title = g.Consts[0].Name
			if len(g.Consts) > 1 {
				title += " 等"
			}
		}
		fmt.Fprintf(&b, "## %s\n\n", title)
		if g.Doc != "" {
			fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(g.Doc))
		}
		b.WriteString("| 名称 | 说明 |\n|---|---|\n")
		for _, c := range g.Consts {
			fmt.Fprintf(&b, "| `%s` | %s |\n", c.Name, escapePipe(c.DocBrief))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// renderBuiltinOnly 汇总仅内置符号——把「真正的私密边界」集中在一页，
// 外部插件作者一眼能看出哪些不属于自己。
func renderBuiltinOnly(pkg Package) string {
	var hits []Symbol
	var ifaceHits []struct {
		iface string
		m     Symbol
	}
	for _, s := range pkg.Symbols {
		if s.BuiltinOnly {
			hits = append(hits, s)
		}
	}
	for _, it := range pkg.Interfaces {
		for _, m := range it.Methods {
			if m.BuiltinOnly {
				ifaceHits = append(ifaceHits, struct {
					iface string
					m     Symbol
				}{it.Name, m})
			}
		}
	}
	if len(hits) == 0 && len(ifaceHits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(genBanner)
	b.WriteString("# 仅内置插件可用的 API\n\n")
	b.WriteString("这些 API **存在于公开 SDK 包里**，但在外部（第三方）插件的运行路径上" +
		"不可用：要么桥接运行时根本不注入它（拿到 nil），要么内核会拒绝/降级。" +
		"列在这里是为了让边界显式——而不是让你在运行时才发现拿不到。\n\n")
	b.WriteString("判断依据全部来自源码与 `hmapdev` 桥接模板，逐条记在每条说明里。\n\n")
	for _, s := range hits {
		b.WriteString(renderSymbol(s, nil))
	}
	for _, h := range ifaceHits {
		fmt.Fprintf(&b, "### `%s.%s`\n\n!!! warning \"仅内核内置插件可用\"\n", h.iface, h.m.Name)
		if h.m.TierReason != "" {
			b.WriteString(indent(h.m.TierReason, "    ") + "\n")
		}
		fmt.Fprintf(&b, "\n```go\n%s\n```\n\n", strings.TrimSpace(h.m.Signature))
		if h.m.Doc != "" {
			fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(h.m.Doc))
		}
	}
	return b.String()
}

// renderExamples 汇总示例插件，并把每个插件用到的 API 列出。
// 这是「想找一个能跑的参考实现」的入口。
func renderExamples(usages map[string][]Usage) string {
	byPlugin := map[string]map[string]bool{}
	for api, list := range usages {
		for _, u := range list {
			if byPlugin[u.Plugin] == nil {
				byPlugin[u.Plugin] = map[string]bool{}
			}
			byPlugin[u.Plugin][api] = true
		}
	}
	names := make([]string, 0, len(byPlugin))
	for n := range byPlugin {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString(genBanner)
	b.WriteString("# 示例插件\n\n")
	// ★ 2026-10-31：26 个插件已迁到 GitHub 组织 HologramAgent（一插件一仓），
	// 本仓 example/ 只剩未迁移的少数。旧文案写"下有多个示例，覆盖工具注册、
	// 通道、记忆读写、LLM 调用、生命周期"—— 在只剩两个示例时是**误导**。
	b.WriteString("SDK 仓 `example/` 里保留的是**未迁移到组织仓**的示例插件，" +
		"都能用 `hmapdev build` 打成 `.hmap` 装进内核直接跑。\n\n")
	b.WriteString("★ **其余 26 个插件已迁到 GitHub 组织** " +
		"[`HologramAgent`](https://github.com/HologramAgent) —— 一插件一仓，" +
		"覆盖工具注册、通道、记忆读写、LLM 调用、生命周期等全部常见形态。\n" +
		"想看完整实现时去那里 clone，不要以为本仓就是全部。\n\n")
	for _, n := range names {
		apis := make([]string, 0, len(byPlugin[n]))
		for a := range byPlugin[n] {
			apis = append(apis, a)
		}
		sort.Strings(apis)
		fmt.Fprintf(&b, "## `%s`\n\n", n)
		fmt.Fprintf(&b, "用到的 API：%s\n\n", codeList(apis))
	}
	if len(names) == 0 {
		b.WriteString("（未找到示例插件调用点）\n")
	}
	return b.String()
}

// dedupeIndex 按「接收者.名称.类别」去重。
//
// 为什么带类别：接口会被两条路径加进索引——一次作为 `type` 符号（来自
// pkg.Symbols），一次作为接口本身（来自 pkg.Interfaces）。两者名字相同、
// 接收者都为空，只用「名称」做键漏不掉；带上类别才能区分（并且保留两条
// 也不算错，但会让结果重复，所以统一按 kind 去重）。
//
// 同名但不同接收者的（如 MemoryAPI.Recall 与 IOInjector.InjectText）都保留。
func dedupeIndex(in []indexEntry) []indexEntry {
	seen := map[string]bool{}
	out := in[:0]
	for _, e := range in {
		k := e.K + "|" + e.R + "." + e.N
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, e)
	}
	return out
}

func codeList(items []string) string {
	parts := make([]string, 0, len(items))
	for _, s := range items {
		parts = append(parts, "`"+s+"`")
	}
	return strings.Join(parts, " · ")
}

// anchor 复现 python-markdown 的 toc slugify：小写，去掉非字母数字与下划线
// **以外**的字符（点号、反引号、括号都在此列），空格与下划线**保留为原形**。
//
// 实测确认：`### \`SettingsAPI.DataDir\“ → id="settingsapidatadir"；
// `## \`ai_image\“ → id="ai_image"（下划线保留，不转连字符）。
// 所以调用方必须传**完整标题文本**（如 "SettingsAPI.DataDir"），不是裸方法名。
func anchor(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}

func escapePipe(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

func indent(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		if strings.TrimSpace(lines[i]) != "" {
			lines[i] = pad + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, string(append(data, '\n')))
}
