package main

// 状态映射的判据。
//
// ## 用户定义的语义（2026-10-18）
//
//	学习中  = agent 正在**等待 LLM**
//	搬砖中  = 真正**执行 toolcall**
//	在线    = **空闲**
//
// ## 内核时序（task.go，权威）
//
//	pre_action  ──→ stepLLM() ──→ ★等 LLM★ ──→ post_action
//	                                              ↓
//	                   工具循环: before_toolcall → ★跑工具★ → after_toolcall
//	                                              ↓
//	                  再次 stepLLM（第二轮工具）或 before_output → after_output
//
// 所以正确映射是：
//
//	pre_action      → 学习中   （即将调 LLM，接下来就是等）
//	before_toolcall → 搬砖中   （即将/正在跑工具）
//	after_toolcall  → 学习中   （工具回来了，又要等 LLM 决定下一步）
//	before_output   → 不改状态 （LLM 已回、即将输出；不是等 LLM 也不是跑工具）
//	after_output    → 在线     （一轮真正结束，空闲）
//
// ## 曾经错在哪
//
// 「学习中」被挂在 `before_output` 上 —— 那个时刻 LLM **已经返回**了，
// 等的是输出而不是等模型。后果：等待 LLM 的整段时间状态是错的
// （停在上一轮的「搬砖中」或「在线」），用户看不到 agent 在想。

import (
	"strings"
	"testing"
)

// ★★★ 判据 1：pre_action 必须切「学习中」（这是唯一的"即将等 LLM"时点）。
func TestStatusStudyOnPreAction(t *testing.T) {
	lines := codeLines(t, "status.go")
	body := funcBody(t, lines, "func (p *Plugin) stagePreAction(")
	if !strings.Contains(body, "applyStatus(qqStatusStudy)") {
		t.Fatal("★ pre_action 没有切「学习中」——\n" +
			"   内核时序里 pre_action 在 stepLLM() **之前**，这是唯一能提前标记\n" +
			"   “接下来要等模型”的位置。不在这里切，等待 LLM 的整段时间状态都是错的")
	}
}

// ★★★ 判据 2：before_toolcall 必须切「搬砖中」。
func TestStatusWorkingOnBeforeToolcall(t *testing.T) {
	lines := codeLines(t, "status.go")
	body := funcBody(t, lines, "func (p *Plugin) stageBeforeToolcall(")
	if !strings.Contains(body, "applyStatus(qqStatusWorking)") {
		t.Fatal("★ before_toolcall 没有切「搬砖中」")
	}
	// 权限门必须仍在这里（合并后不能丢）
	if !strings.Contains(body, "p.beforeToolcall(ctx)") {
		t.Fatal("★ before_toolcall 里权限门丢了 —— 合并时把安全逻辑吞掉了")
	}
}

// ★★★ 判据 3：after_toolcall 必须回到「学习中」。
//
// 不回到学习中，状态会在整个工具循环里挂着「搬砖中」，看起来像工具卡住。
func TestStatusStudyOnAfterToolcall(t *testing.T) {
	lines := codeLines(t, "status.go")
	body := funcBody(t, lines, "func (p *Plugin) stageAfterToolcall(")
	if !strings.Contains(body, "applyStatus(qqStatusStudy)") {
		t.Fatal("★ after_toolcall 没回到「学习中」——\n" +
			"   工具回来了，接下来又要等 LLM 决定下一步。不切会让状态\n" +
			"   在整个工具循环里挂着「搬砖中」，看起来像卡住")
	}
}

// ★★★ 判据 4：before_output **不**改在线状态。
//
// 那个时刻 LLM 已返回、即将输出 —— 既不是等 LLM 也不是跑工具。
// 在那里写「学习中」会让状态在收尾时继续挂着，看起来像永远在想。
func TestStatusNotSetOnBeforeOutput(t *testing.T) {
	lines := codeLines(t, "status.go")
	body := funcBody(t, lines, "func (p *Plugin) stageBeforeOutput(")
	// 必须检查**真实调用**：`_ = qqStatusStudy` 这种引用不该算作"改了状态"，
	// 但 `applyStatus(qqStatusStudy)` 必须算。反过来也一样——
	// 只查常量名会让"引用了但没调用"的变异漏网（实测）。
	for _, bad := range []string{"applyStatus(qqStatusStudy)", "applyStatus(qqStatusWorking)", "applyStatusForce(qqStatusOnline)"} {
		if strings.Contains(body, bad) {
			t.Fatalf("★ before_output 改了在线状态（%s）——\n"+
				"   这个时刻 LLM 已返回、即将输出，既不是等 LLM 也不是跑工具；\n"+
				"   在这里写「学习中」会让状态在收尾时继续挂着，看起来像永远在想", bad)
		}
	}
	// 但它必须仍负责打字指示
	if !strings.Contains(body, "p.startTypingForTurn(ctx)") {
		t.Fatal("★ before_output 没有发起打字指示")
	}
}

// ★★★ 判据 5：after_output 必须是「在线」（force，绕防抖）。
//
// 一轮结束时是唯一的"归位"机会；被防抖拦住就会永远停在错误状态。
func TestStatusOnlineOnAfterOutput(t *testing.T) {
	lines := codeLines(t, "status.go")
	body := funcBody(t, lines, "func (p *Plugin) stageAfterOutput(")
	if !strings.Contains(body, "applyStatusForce(qqStatusOnline)") {
		t.Fatal("★ after_output 没有 force 归位「在线」——\n" +
			"   一轮结束是唯一的归位机会；用 applyStatus（带防抖）会被\n" +
			"   2 秒间隔拦住，进而永远停在错误状态")
	}
}

// ★★★ 判据 6：pre_action / after_toolcall 必须真的注册了。
//
// 光有函数不够 —— 不注册就等于没写。这类“函数在、注册忘”的漏洞
// 编译器不报、运行时不报错。
func TestStatusStagesRegistered(t *testing.T) {
	lines := codeLines(t, "plugin.go")
	joined := strings.Join(lines, "\n")
	for _, s := range []string{
		"sdk.StagePreAction, p.stagePreAction",
		"sdk.StageAfterToolcall, p.stageAfterToolcall",
	} {
		if !strings.Contains(joined, s) {
			t.Fatalf("★ 没有注册 stage：%s（函数写了但没注册 = 没写）", s)
		}
	}
}

// ★★★ 判据 7：启动日志里的工具数必须是**真实计数**，不能写死。
//
// 这里原本是 `log.Printf("... (%d tools)", p.name, 15)` —— 硬编码。
// 后来工具从 15 加到 24，日志仍报 15。
//
// 后果不是"日志难看"，而是**排查时被误导**：我查"qzone 工具装上没"，
// 看到日志说 15 tools，以为装的是旧版，白折腾了好几轮。
// 日志说谎比不日志更坏 —— 因为它看起来像证据。
func TestToolCountLoggedIsNotHardcoded(t *testing.T) {
	for _, ln := range codeLines(t, "plugin.go") {
		if !strings.Contains(ln, "[qq] plugin started:") {
			continue
		}
		if !strings.Contains(ln, "p.toolCount") {
			t.Fatalf("★ 启动日志没报真实工具数：%q\n"+
				"   硬编码的数字会随工具增减而失真，进而误导排查", ln)
		}
	}
	// 计数必须真的在注册时累加（否则 p.toolCount 恒为 0，比硬编码更糟）
	body := funcBody(t, codeLines(t, "plugin.go"), "func (p *Plugin) regTool(")
	if !strings.Contains(body, "p.toolCount++") {
		t.Fatal("★ regTool 没有累加 p.toolCount —— 计数恒为 0，日志会报 `0 tools`")
	}
}

// ★★★ 判据 8：状态归位（在线）**只能由 turn 边界触发**，不能由单次发送触发。
//
// after_output 被两条路径共用（2026-10-18）：
//
//	output.go   单次 output_send__*（Extra 有 output_meta）
//	eventloop   一轮的回复（turn 边界）
//
// 一轮可能发多条消息。若每条发完都归位「在线」，状态会在干活期间
// 反复跳回空闲 —— "通过状态展示工作进度"就失效了。
func TestOnlineResetOnlyOnTurnBoundary(t *testing.T) {
	lines := codeLines(t, "status.go")
	body := funcBody(t, lines, "func (p *Plugin) stageAfterOutput(")

	// 必须有区分两条路径的分支
	if !strings.Contains(body, "isSingleSendStage(ctx)") {
		t.Fatal("★ stageAfterOutput 没有区分「单次发送」与「turn 边界」——\n" +
			"   两条路径共用同一个 stage，不区分会让单次发送也归位「在线」")
	}

	// 单次发送分支必须在归位之前 return（不能继续往下走到 applyStatusForce）
	iBranch := strings.Index(body, "if isSingleSendStage(ctx) {")
	iOnline := strings.Index(body, "applyStatusForce(qqStatusOnline)")
	if iBranch < 0 || iOnline < 0 {
		t.Fatal("找不到分支或状态归位调用")
	}
	branch := ifBlockAt(body, iBranch)
	if !strings.Contains(branch, "return") {
		t.Fatal("★ 单次发送分支没有 return —— 会继续走到状态归位")
	}
	if strings.Contains(branch, "applyStatusForce") {
		t.Fatal("★ 单次发送分支里归位了「在线」—— 一轮发多条消息时状态会反复跳回空闲")
	}

	// isSingleSendStage 的判据键必须是 output_meta（内核 output.go 放的）
	helper := funcBody(t, lines, "func isSingleSendStage(")
	if !strings.Contains(helper, `ctx.Extra["output_meta"]`) {
		t.Fatal("★ isSingleSendStage 没认 output_meta —— 与内核 output.go 的契约不符")
	}
}
