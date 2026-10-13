package main

import (
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
)

// 子进程运行时模板的静态检查（Part 3）。
//
// 为什么需要这些测试：模板是插件的运行时半身，它与内核 internal/plugin/proc/
// 的协议名、共享段布局、字段索引必须逐一对齐。任一处漂移都会导致
// 「插件编译通过但运行时读错字段」——比编译错误难查得多。
//
// 模板改为真实 .go 源文件（而非 raw string）的直接收益就是这类检查可行。

func loadProcTemplate(t *testing.T) string {
	t.Helper()
	data, err := procTemplates.ReadFile("templates/proc_main.go.tmpl")
	if err != nil {
		t.Fatalf("读取内嵌模板: %v", err)
	}
	return string(data)
}

// extractShmFlagsOff 取出模板里 shmFlagsOff 函数的函数体，供断言其取值来源。
func extractShmFlagsOff(t *testing.T, src string) string {
	t.Helper()
	const sig = "func shmFlagsOff() uint32 {"
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatal("模板缺少 shmFlagsOff()")
	}
	rest := src[i+len(sig):]
	if j := strings.Index(rest, "\n}"); j >= 0 {
		return rest[:j]
	}
	return rest
}

// stripComments 去掉源码中的注释（用空白填充以保持偏移），只留可执行代码。
func stripComments(t *testing.T, src string) string {
	t.Helper()
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, "proc_main.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("解析模板: %v", err)
	}
	out := []byte(src)
	for _, cg := range f.Comments {
		s := fs.Position(cg.Pos()).Offset
		e := fs.Position(cg.End()).Offset
		for i := s; i < e && i < len(out); i++ {
			if out[i] != '\n' {
				out[i] = ' '
			}
		}
	}
	return string(out)
}

// 模板必须是合法 Go 源码。
func TestProcTemplate_ParsesAsGo(t *testing.T) {
	src := loadProcTemplate(t)
	fs := token.NewFileSet()
	if _, err := parser.ParseFile(fs, "proc_main.go", src, parser.AllErrors); err != nil {
		t.Fatalf("模板不是合法 Go 源码: %v", err)
	}
}

// 模板必须提供 main()，且不得含 cgo 痕迹。
//
// 零 cgo 是迁移的核心收益之一（§3.7 锁仲裁回内核后整个架构无 cgo）；
// 一旦有人往模板里加 import "C"，交叉编译立刻退回需要目标平台 C 工具链。
func TestProcTemplate_HasMainAndNoCgo(t *testing.T) {
	src := loadProcTemplate(t)

	if !strings.Contains(src, "func main()") {
		t.Error("子进程模板必须有 main() 入口")
	}
	// 只检查代码，不检查注释——模板顶部的说明文字本身就提到了 C.CString/C.free
	code := stripComments(t, src)
	for _, forbidden := range []string{
		`import "C"`,
		"//export ",
		"C.CString",
		"C.GoString",
		"C.free",
	} {
		if strings.Contains(code, forbidden) {
			t.Errorf("模板不应含 cgo 痕迹 %q（零 cgo 是迁移的核心收益）", forbidden)
		}
	}
}

// 模板引用的 method 名必须与内核 internal/plugin/proc/protocol.go 一致。
//
// 这里硬编码一份清单做对照：内核侧改了 method 名而模板没跟上时，
// 表现是插件调用返回「未知 method」，测试能提前拦住。
func TestProcTemplate_CoversAllCoreMethods(t *testing.T) {
	src := loadProcTemplate(t)

	// 51 个 C ABI method id 平移后的名字（§3.2），加 stage 锁仲裁 2 个
	required := []string{
		// 注册面
		"tool.register", "stage.register", "output.register", "api.register", "input.register",
		// IO 注入（io.injectTextNoMem 见下方 deprecated：内核保留为兼容旧二进制，
		// 当前模板改走 io.injectText + NoMemory 标志位，不再发那个 id）
		"io.injectText", "io.injectInterrupt", "io.injectInputSync",
		"io.setToolBlocks",
		// 多模态注入（1.1.0 新增）。漏接线的后果是插件调 InjectInputMedia 静默无效果：
		// 模板不发这个 RPC，内核也就永远收不到，而两边都不报错。
		"io.injectMedia", "io.injectMediaSync", "io.injectInterruptMedia",
		// 生命周期
		"lifecycle.autoRestart",
		// 图记忆
		"memory.recall", "memory.commit", "memory.introspect", "memory.merge", "memory.purge",
		// 文档记忆
		"doc.query", "doc.insert", "doc.remove", "doc.stats",
		// 文档媒体（1.1.0 新增）
		"doc.insertWithMedia",
		// 知识库
		"knowledge.search", "knowledge.add", "knowledge.list",
		// 文本记忆
		"textmemory.append",
		// 设置
		"settings.get", "settings.set", "settings.registerDef",
		"settings.getCore", "settings.setCore", "settings.listCore",
		"settings.getPlugin", "settings.setPlugin", "settings.listPlugin",
		"settings.list", "settings.defs", "settings.dump", "settings.plugins",
		"settings.dataDir",
		// LLM
		"llm.listSources", "llm.setSource", "llm.currentSource",
		// 社交图
		"social.getPerson", "social.getNetwork", "social.getTrait",
		"social.getRelations", "social.listPersons",
		// 插件管理
		"plugin.reloadOne", "plugin.listLoaded", "plugin.isDisabled",
		// 共享段锁仲裁（新增，C ABI 下不存在此概念）
		"stage.lock", "stage.unlock",
	}
	for _, m := range required {
		if !strings.Contains(src, `"`+m+`"`) {
			t.Errorf("模板缺少 core method %q（内核已提供，插件侧未接线）", m)
		}
	}

	// 内核保留、但**当前模板不再发送**的 method id。
	//
	// 它们不是「公开接口新增却忘记接线」，而是刻意的向后兼容面：
	// 内核必须继续接受用旧模板编出的插件二进制发来的 id，而当前模板没有理由再发。
	//
	// 可复查的判据：ba49dfd 之前的模板里 InjectTextNoMemory 发的就是
	// "io.injectTextNoMem"；注入标志位落地后它改走 "io.injectText" + NoMemory。
	//
	// 为何要单独列而不是直接从 required 删掉：这条守卫的价值在于「新接口必须接线」，
	// 而把「内核有、模板就必须发」当不变量，会让它常驻误报——常驻误报的守卫迟早
	// 被人习惯性忽略，那时真漏接线也就没人看见了。
	deprecated := map[string]string{
		"io.injectTextNoMem": "旧模板经此表达「不进记忆」；现由 io.injectText + NoMemory 表达",
	}
	// 反向保护：allowlist 条目一旦又出现在模板里，说明它已过期，必须删掉，
	// 否则这里会悄悄变成「永久豁免」的垃圾抽屉。
	for m, why := range deprecated {
		if strings.Contains(src, `"`+m+`"`) {
			t.Errorf("deprecated 里的 %q 又出现在模板里（%s）——条目已过期，请从 deprecated 移除", m, why)
		}
	}
}

// 模板必须处理内核发来的全部调用（含无法 JSON 序列化的 Cleaner 回调）。
func TestProcTemplate_HandlesAllKernelCalls(t *testing.T) {
	src := loadProcTemplate(t)
	for _, m := range []string{
		"handshake",
		"plugin.init", "plugin.start", "plugin.stop",
		"tool.invoke", "cleaner.invoke", "stage.invoke", "output.invoke",
	} {
		if !strings.Contains(src, `case "`+m+`"`) {
			t.Errorf("模板未处理内核调用 %q", m)
		}
	}
}

// 工具调用的 payload 必须走内核标定的**调用帧**（funccall 模型）。
//
// 共享内存是内核内部实现（插件作者只看到普通 map），但模板必须在传输层
// 正确读写 frame / args_len / result_ref。漏接线的后果很隐蔽：参数被静默
// 丢弃、结果只走内联，性能退化而不报错。
func TestProcTemplate_ToolInvokeUsesSharedRef(t *testing.T) {
	src := loadProcTemplate(t)
	for _, want := range []string{"frame", "args_len", "result_ref"} {
		if !strings.Contains(src, want) {
			t.Errorf("模板的 tool.invoke 必须处理 %q（payload 走内核标定的调用帧）", want)
		}
	}
}

// 模板必须通过 arena.alloc / arena.free 向内核申请与归还共享内存。
//
// 共享内存是内核独占管理的**内部实现**：插件不能自己维护分配游标。
// 历史上两版跨进程分配器（bump 游标 / 模板内位图 CAS）都因为把可变
// 分配状态放在共享内存里而出竞态，所以这里做回归保护。
func TestProcTemplate_UsesKernelArenaRPC(t *testing.T) {
	src := loadProcTemplate(t)

	for _, m := range []string{`"arena.alloc"`, `"arena.free"`} {
		if !strings.Contains(src, m) {
			t.Errorf("模板缺少内核共享内存 RPC %s（插件必须向内核申请/归还）", m)
		}
	}

	// 禁止插件侧再出现本地分配器符号。
	//
	// 只查代码不查注释：注释里会解释“为什么不再这么做”。
	code := stripComments(t, src)
	for _, forbidden := range []string{"arenaUsed", "arenaWrite"} {
		if strings.Contains(code, forbidden) {
			t.Errorf("模板不应再出现插件侧分配器 %q（共享内存由内核独占管理）", forbidden)
		}
	}
}

// 共享段布局常量必须与内核 internal/plugin/proc/shm.go 一致。
//
// 字段索引错位是最危险的漂移：插件会读到相邻字段的数据，
// 而两边都不报错（同为 []byte）。
func TestProcTemplate_ShmLayoutMatchesKernel(t *testing.T) {
	src := loadProcTemplate(t)

	// 与内核 shm.go 的 offXxx 常量对齐（值比较，不依赖 gofmt 的对齐空白）
	layout := map[string]string{
		"shmOffMagic":      "0",
		"shmOffFieldCount": "4",
		"shmOffArenaBase":  "8",
		"shmOffArenaCap":   "12",
		"shmOffArenaUsed":  "16",
		"shmOffCtxBase":    "20",
		"shmOffSeq":        "24",
		// 与内核 stageFieldCount / sliceSize 对齐。
		// 注意：这里不校验人工版本号 —— 它已被删除，布局一致性
		// 改由描述符个数这种派生事实校验（见下方全局规约断言）。
		"shmStageFieldCount": "19",
		"shmSliceSize":       "8",
	}
	constRe := func(name, want string) bool {
		// gofmt 会对齐常量块，故容许 name 与 = 之间有任意空白
		re := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\s*=\s*` + regexp.QuoteMeta(want) + `\b`)
		return re.MatchString(src)
	}
	for name, want := range layout {
		if !constRe(name, want) {
			t.Errorf("共享段常量 %s 应为 %s（须与内核 internal/plugin/proc/shm.go 一致）", name, want)
		}
	}

	// 布局必须自描述：标志位基址由内核下发的描述符个数推出，
	// 不能用本插件的编译期常量。否则内核加字段时会静默错位。
	if !strings.Contains(src, "func shmFieldCount()") {
		t.Error("模板缺少 shmFieldCount()：标志位基址必须从 header 读取，不能自行推算")
	}
	flagsOff := extractShmFlagsOff(t, src)
	if strings.Contains(flagsOff, "shmStageFieldCount") {
		t.Errorf("shmFlagsOff 不得使用编译期常量 shmStageFieldCount（应经 shmFieldCount() 从 header 读取）:\n%s", flagsOff)
	}
	if !strings.Contains(flagsOff, "shmFieldCount()") {
		t.Errorf("shmFlagsOff 应基于 shmFieldCount():\n%s", flagsOff)
	}

	// 不得再出现人工版本号（内核与模板都不应有）。
	for _, banned := range []string{"shmVersion", "shmOffVersion"} {
		if strings.Contains(src, banned) {
			t.Errorf("模板不得再有 %s：布局一致性由描述符个数校验，不靠人工版本号", banned)
		}
	}

	// 字段枚举顺序：内核 stageField 的前若干项
	fieldOrder := []string{
		"fRawMessage = iota", "fUserID", "fGroupID", "fLLMText",
		"fReasoningContent", "fFinalText", "fResponse", "fPhase",
		"fContextMsgs", "fToolCalls", "fToolResults", "fMemory",
		"fTokenUsage", "fErrors",
		"fExtraMediaBlocks", "fExtraMediaType", "fExtraInputSource", "fExtraOutputChannel",
	}
	idx := -1
	for _, f := range fieldOrder {
		at := strings.Index(src, f)
		if at < 0 {
			t.Fatalf("模板缺少字段常量 %s", f)
		}
		if at <= idx {
			t.Errorf("字段常量 %s 的声明顺序与内核 stageField 枚举不一致", f)
		}
		idx = at
	}
}

// stage 处理必须「拿锁 → 读 → handler → 只写脏字段 → 放锁」。
//
// 只写脏字段是消除 lost update 的核心：只读插件零写入，
// 不可能覆盖其他插件的改写（对照 C ABI 副本模型实测 35.8~36.8% 丢失）。
func TestProcTemplate_StageFlowUsesLockAndDirtyWrite(t *testing.T) {
	src := loadProcTemplate(t)

	for _, want := range []string{
		"func handleStageInvoke(",
		"stage.lock",
		"readStageContext()",
		"takeStageSnapshot(",
		"writeStageDirty(",
		"stage.unlock",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("stage 处理链路缺少 %q", want)
		}
	}

	// 顺序检查：加锁必须在读取之前，写回必须在解锁之前
	iLock := strings.Index(src, `callCoreVoid("stage.lock"`)
	iRead := strings.Index(src, "readStageContext()")
	iWrite := strings.Index(src, "writeStageDirty(sc, snap)")
	if iLock < 0 || iRead < 0 || iWrite < 0 {
		t.Fatal("stage 链路关键调用缺失")
	}
	// readStageContext 的定义在前，调用在后；取 handleStageInvoke 内的位置
	stageFn := src[strings.Index(src, "func handleStageInvoke("):]
	iLockFn := strings.Index(stageFn, `callCoreVoid("stage.lock"`)
	iReadFn := strings.Index(stageFn, "readStageContext()")
	iWriteFn := strings.Index(stageFn, "writeStageDirty(sc, snap)")
	if !(iLockFn < iReadFn && iReadFn < iWriteFn) {
		t.Error("stage 链路顺序应为 加锁 → 读取 → 写回")
	}
}

// 快照必须存序列化字符串而非 Go 值。
//
// ❗ 这是修 C ABI 侧 11.3 时踩过的坑：StageContext 的切片字段与读出的值
// 共享底层内容，handler 原地改元素（sc.ToolResults[0].Result = x）时，
// 直接持有 Go 值的快照会跟着变，脏字段计算失效、修复静默失效。
func TestProcTemplate_SnapshotStoresSerializedStrings(t *testing.T) {
	src := loadProcTemplate(t)

	if !strings.Contains(src, "strs        map[int]string") ||
		!strings.Contains(src, "jsons       map[int]string") {
		t.Error("stageSnapshot 必须存序列化字符串（切片共享底层数组，存 Go 值会让脏字段计算失效）")
	}
	if !strings.Contains(src, "json.Marshal(v)") {
		t.Error("takeStageSnapshot 应对容器字段做 json.Marshal")
	}
}

// arena 用尽必须显式报错，不得静默截断（§4.4 风险登记）。
func TestProcTemplate_ArenaExhaustionErrors(t *testing.T) {
	src := loadProcTemplate(t)
	if !strings.Contains(src, "arena 空间不足") {
		t.Error("shmWrite 在 arena 不足时必须报错，不得静默截断")
	}
}

// 日志必须走 stderr：stdout 是 RPC 通道，写日志会破坏 NDJSON 帧。
func TestProcTemplate_LogsToStderr(t *testing.T) {
	src := loadProcTemplate(t)
	if !strings.Contains(src, "log.SetOutput(os.Stderr)") {
		t.Error("日志必须走 stderr，否则会破坏 stdout 的 RPC 帧")
	}
}

// 请求必须在独立 goroutine 里处理。
//
// handler 内会反向调用内核并等应答；若在读循环里同步处理，
// 就没人读应答帧 → 死锁。
func TestProcTemplate_DispatchesRequestsConcurrently(t *testing.T) {
	src := loadProcTemplate(t)
	if !strings.Contains(src, "go handleKernelRequest(&req)") {
		t.Error("请求须在独立 goroutine 处理（handler 内反向调用内核，同步处理会死锁）")
	}
}

// 主版本与共享内存区域结构不匹配必须拒绝，不得半兼容运行。
//
// 注意：这里不再检查任何「协议版本」/「共享段版本」自定号 ——
// 它们已废除。兼容性只认双方版本号的**主版本段**（跨主版本即破坏性变更），
// 布局一致性由描述符个数把关。
func TestProcTemplate_RejectsIncompatiblePeer(t *testing.T) {
	src := loadProcTemplate(t)
	for _, want := range []string{"主版本不匹配", "字段数不匹配", "统一区域魔数不匹配"} {
		if !strings.Contains(src, want) {
			t.Errorf("握手应校验并拒绝 %q", want)
		}
	}
	// 自定版本号必须已彻底移除，否则会出现「谁负责升这个号」的腐烂面。
	for _, banned := range []string{"procProtocolVersion", "shmVersion", "unifiedVersion"} {
		if strings.Contains(src, banned) {
			t.Errorf("模板不得再有自定版本号 %s", banned)
		}
	}
}

// 全平台统一产出 plugin.bin。
//
// 这是三套独立 ABI 实现（.so/.dylib/.dll）收敛为单一 RPC 实现的直接后果：
// 进程边界本身就是 ABI 边界，不存在平台特有的动态库扩展名。
// §9.2 记录的「Windows DLL 路径只下发 3 字段、无写回」随之消失——
// Windows 走的是与 Linux 完全相同的 RPC 实现。
func TestResolveBuild_AllPlatformsProduceBin(t *testing.T) {
	for _, target := range []string{
		"linux/amd64", "linux/arm64",
		"darwin/amd64", "darwin/arm64",
		"windows/amd64",
		"freebsd/amd64",
	} {
		cfg, errMsg := resolveBuild(target)
		if cfg == nil {
			t.Fatalf("resolveBuild(%q) 失败: %s", target, errMsg)
		}
		if cfg.entryFile != procEntryFile {
			t.Errorf("%s: 产物应为 %s，实际 %s", target, procEntryFile, cfg.entryFile)
		}
	}
}

// lua 目标仍走解释器路径（entry 字段唯一仍在使用的用途）。
func TestResolveBuild_LuaIsSeparatePath(t *testing.T) {
	for _, target := range []string{"lua", ""} {
		cfg, kind := resolveBuild(target)
		if cfg != nil {
			t.Errorf("%q 应返回 nil cfg（Lua 不经 Go 编译）", target)
		}
		if kind != "lua" {
			t.Errorf("%q 应识别为 lua，实际 %q", target, kind)
		}
	}
}

// 不支持的平台明确报错，不静默产出错误产物。
func TestResolveBuild_UnsupportedOSErrors(t *testing.T) {
	cfg, errMsg := resolveBuild("plan9/amd64")
	if cfg != nil {
		t.Error("不支持的平台应返回 nil cfg")
	}
	if !strings.Contains(errMsg, "unsupported") {
		t.Errorf("应给出 unsupported 提示，实际 %q", errMsg)
	}
}

// bundle 产物在 zip 内按平台加后缀（全平台同名 plugin.bin 会相互覆盖）。
func TestBundleTargets_HavePlatformSuffixedEntries(t *testing.T) {
	seen := map[string]bool{}
	for _, bt := range allBundleTargets {
		if seen[bt.entry] {
			t.Errorf("zip 条目名重复: %s（会相互覆盖）", bt.entry)
		}
		seen[bt.entry] = true
		if !strings.HasPrefix(bt.entry, procEntryFile+".") {
			t.Errorf("bundle 条目 %q 应以 %s. 为前缀", bt.entry, procEntryFile)
		}
	}
	if len(allBundleTargets) == 0 {
		t.Error("bundle 目标表不应为空")
	}
}

// C ABI 工具链残留必须彻底清除：不得再有 .so/.dylib/.dll 产物路径，
// 也不得再引用 c-shared 构建模式或 MinGW 探测。
func TestToolchain_NoCABIResiduals(t *testing.T) {
	for _, f := range []string{"cmd_build.go", "templates.go", "cmd_init.go", "proc_runtime.go"} {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s: %v", f, err)
		}
		src := stripComments(t, string(data))
		for _, forbidden := range []string{
			"c-shared",
			"CGO_ENABLED=1",
			"detectWindowsCC",
			"generateBridge",
			"tmplLinuxBridge",
			"tmplPluginInitC",
		} {
			if strings.Contains(src, forbidden) {
				t.Errorf("%s 仍含 C ABI 残留 %q", f, forbidden)
			}
		}
	}
}

// Go 插件的构建不再读 plg.json 的 entry 值。
//
// 这是「外部插件零改动」的关键：17 个存量插件的 plg.json 都写着 "plugin.so"，
// 若把 entry 当通道开关，迁移就得改 17 个文件。
func TestToolchain_IgnoresEntryForGoPlugins(t *testing.T) {
	data, err := os.ReadFile("cmd_build.go")
	if err != nil {
		t.Fatalf("读 cmd_build.go: %v", err)
	}
	src := stripComments(t, string(data))
	if strings.Contains(src, "isProcEntry") {
		t.Error("isProcEntry 应已删除——Go 插件一律产出 plugin.bin，不看 entry 值")
	}
	// entry 仅剩 Lua 判定这一处用途
	if !strings.Contains(src, "luaEntryFile") {
		t.Error("IsLua 应改用 luaEntryFile 常量")
	}
}
