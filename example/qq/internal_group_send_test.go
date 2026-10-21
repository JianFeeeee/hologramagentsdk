package main

// 判据：内部委派的工具调用**不得向群聊发送消息**。
//
// ## 现场事故（2026-10-18）
//
// offload 子 agent 的「分诊汇报」把本该私聊给用户的**进度播报发到了群里**：
//
//	17:15:00 interrupt from offload-1792313969/child: 分诊汇报
//	17:15:41 executing tool: output_send__qq
//	17:15:41 handleChannelOutput meta={"group_id": 979911915}
//
// 根因：子 agent 被授予全部输出通道（AllowedOutputs=nil，用户明确要求
// "持有全部输出通道"），于是它能自己选目标 —— 而它只从上下文里看到一个群号，
// 就选了群。
//
// ## 为什么必须由此处硬拦（提示词不够）
//
// 提示词是"软"的；而群的发言**公开可见**。子 agent 看不到群里的上下文、
// 分不清每句话是回给谁的 —— 它只有一句任务描述。让它自由向群发言，
// 等于把私聊级别的判断拿到公开场合执行。
//
// 真需要群里回时，由**父**（有完整上下文与用户身份）来发 ——
// 父的 comefrom 是 user，不受这条约束。

import (
	"strings"
	"testing"

	"github.com/JianFeeeee/hologramagentsdk/sdk"
)

func mkOutputCall(name, meta string) *sdk.StageContext {
	return &sdk.StageContext{
		Extra: map[string]interface{}{"comefrom": "internal"},
		ToolCalls: []sdk.ToolCall{{
			Name:      name,
			Arguments: map[string]interface{}{"payload": "x", "type": "text", "meta": meta},
		}},
	}
}

// ★★★ 群目标必须被识别出来。
func TestOutputSendGroupTargetDetectsGroup(t *testing.T) {
	ctx := mkOutputCall("output_send__qq", `{"group_id": 979911915}`)
	if got := outputSendGroupTarget(ctx, "qq"); got != "979911915" {
		t.Fatalf("★ 没识别出群目标，got=%q —— 进度播报会照旧发到群里", got)
	}
}

// 私聊目标不算（那是允许的）。
func TestOutputSendGroupTargetIgnoresPrivate(t *testing.T) {
	ctx := mkOutputCall("output_send__qq", `{"user_id": 1456128421}`)
	if got := outputSendGroupTarget(ctx, "qq"); got != "" {
		t.Fatalf("★ 私聊目标被误判成群目标：%q", got)
	}
}

// 别的通道的 output_send 不管（本插件只约束自己）。
func TestOutputSendGroupTargetIgnoresOtherChannel(t *testing.T) {
	ctx := mkOutputCall("output_send__webui", `{"group_id": 1}`)
	if got := outputSendGroupTarget(ctx, "qq"); got != "" {
		t.Fatalf("★ 别的通道被误拦：%q", got)
	}
}

// 非 output_send 工具不管。
func TestOutputSendGroupTargetIgnoresOtherTools(t *testing.T) {
	ctx := &sdk.StageContext{
		ToolCalls: []sdk.ToolCall{{
			Name:      "qq_send_private_msg",
			Arguments: map[string]interface{}{"meta": `{"group_id": 1}`},
		}},
	}
	if got := outputSendGroupTarget(ctx, "qq"); got != "" {
		t.Fatalf("★ 非 output_send 工具被误拦：%q", got)
	}
}

// ★★★ 接线判据：beforeToolcall 必须**真的**在 internal 放行前调用它。
//
// 这一类“函数写了但没接上”编译器不报、运行时不报错 ——
// 事故还会照旧发生。必须按代码行验证调用存在且位置正确。
func TestInternalGroupSendBlockIsWired(t *testing.T) {
	lines := codeLines(t, "plugin.go")
	body := funcBody(t, lines, "func (p *Plugin) beforeToolcall(")
	if !strings.Contains(body, "outputSendGroupTarget(ctx, p.name)") {
		t.Fatal("★ beforeToolcall 没调用 outputSendGroupTarget —— " +
			"内部子 agent 仍可把消息发到群里（事故会复发）")
	}
	// 必须在 isInternalToolCall 的分支里（内部委派专用约束）
	iInt := strings.Index(body, "if isInternalToolCall(ctx) {")
	iCall := strings.Index(body, "outputSendGroupTarget(ctx, p.name)")
	if iInt < 0 || iCall < 0 || iCall < iInt {
		t.Fatal("★ outputSendGroupTarget 不在 isInternalToolCall 分支内 —— " +
			"约束会错误地作用到用户请求上")
	}
	// 拦截必须落到 setDenial（写拒绝指令），而不是 return error（会让整轮失败）
	if !strings.Contains(body, "p.setDenial(ctx, msg)") {
		t.Fatal("★ 拦截没写 denial —— 模型不会知道自己被拒，可能反复重试")
	}
}
