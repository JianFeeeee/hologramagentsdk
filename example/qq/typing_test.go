package main

// 「对方正在输入中」的语义判据。
//
// ## 修的是什么（2026-10-18）
//
// 旧实现两处都错位：
//
//	HTTP 入口            → startTyping(evt.UserID)     ← 消息刚到就挂指示
//	output_send__* 里    → if userID>0 { stopTyping }  ← 任何发送都停
//
// 具体害处：
//
//  1. **起点太早**：agent 还没接受消息、可能在批量合并窗口里等、甚至可能
//     被权限门拒掉。被拒的那条消息会让用户一直看着"正在输入"却没有回复。
//     而且 typingLoop 有 30 秒超时 —— agent 跑得慢就自己灭了，回复还在路上。
//
//  2. **终点太宽**：stopTyping 挂在"发送消息"上，而发送可能是群聊、可能是
//     发给别人。正在跟 A 私聊时 agent 发了条群消息，A 的指示被莫名关掉。
//
// 正确边界：`before_output` 开始、`after_output` 结束（内核里这两个阶段
// **每轮只触发一次**），且只处理"自己引起的、有身份的"那一轮。
//
// ## 为什么需要内核透传 inject_meta
//
// `before_output` 的 stageCtx 是内核新建的（emitResponse），与 `on_input`
// 的 stageCtx 不共享 Extra（后者是局部变量）。所以身份必须由注入方声明、
// 内核原样透传 —— 否则插件只能读全局 p.auth 猜，并发私聊会串号
// （A 的输入指示打到 B 身上）。

import (
	"os"
	"strings"
	"testing"

	"github.com/JianFeeeee/hologramagentsdk/sdk"
)

// codeLines 返回某个 .go 文件里**非注释**的代码行。
//
// 这个项目已多次栽在「注释里有、代码里没有」：判据直接 Contains 全文，
// 会把解释性的注释当成实现在，变异删掉代码也不咬。
func codeLines(t *testing.T, file string) []string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	inBlock := false
	for _, ln := range strings.Split(string(b), "\n") {
		s := strings.TrimSpace(ln)
		if inBlock {
			if i := strings.Index(s, "*/"); i >= 0 {
				s = strings.TrimSpace(s[i+2:])
				inBlock = false
			} else {
				continue
			}
		}
		if strings.HasPrefix(s, "//") || strings.HasPrefix(s, "*") {
			continue
		}
		if strings.HasPrefix(s, "/*") {
			if i := strings.Index(s, "*/"); i >= 0 {
				s = strings.TrimSpace(s[i+2:])
			} else {
				inBlock = true
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

// funcBody 用**括号计数**提取某个顶层函数/方法的函数体（含签名行）。
//
// 为什么不用正则：调用点里常有嵌套括号（`p.injectInterrupt(a, b, qqInjectPayload(x, y, z))`），
// `[^)]*` 会在第一个 `)` 提前截断 —— 这个判据第一版就因此误报"没带身份"。
// 也不用 `\n}\n`：函数体内有块语句，第一个行首 `}` 未必是函数结尾。
func funcBody(t *testing.T, lines []string, sigPrefix string) string {
	t.Helper()
	start := -1
	for i, ln := range lines {
		if strings.HasPrefix(ln, sigPrefix) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("找不到 %s —— 判据需同步", sigPrefix)
	}
	depth := 0
	seenBrace := false
	for j := start; j < len(lines); j++ {
		for _, r := range lines[j] {
			switch r {
			case '{':
				depth++
				seenBrace = true
			case '}':
				depth--
			}
		}
		if seenBrace && depth == 0 {
			return strings.Join(lines[start:j+1], "\n")
		}
	}
	t.Fatalf("%s 函数体没闭合", sigPrefix)
	return ""
}

// callArgs 提取 `name(` 之后到**匹配的** `)` 之间的实参文本（处理嵌套括号）。
func callArgs(s, name string) []string {
	var out []string
	for i := 0; ; {
		k := strings.Index(s[i:], name+"(")
		if k < 0 {
			return out
		}
		k += i + len(name) + 1
		depth := 1
		for j := k; j < len(s); j++ {
			switch s[j] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					out = append(out, s[k:j])
					i = j + 1
					goto next
				}
			}
		}
		return out
	next:
	}
}

// ★★★ 判据 1：HTTP 入口**不再**发起输入指示。
//
// 变异方式：把 `p.startTyping(evt.UserID)` 加回入口 —— 必咬。
func TestTypingNotStartedAtHTTPEntry(t *testing.T) {
	for _, ln := range codeLines(t, "plugin.go") {
		if strings.Contains(ln, "startTyping(") && !strings.HasPrefix(ln, "func ") {
			// 允许出现在 startTypingForTurn 里（那是 before_output 的调用）
			t.Fatalf("★ HTTP 入口仍在发起输入指示：%q\n"+
				"   应在 before_output 发起（消息刚到就挂指示会让被拒/排队的消息"+
				"一直显示\"正在输入\"）", ln)
		}
	}
}

// ★★★ 判据 2：发送消息路径**不再**停输入指示。
//
// 变异方式：把 `p.stopTyping(userID)` 加回 sendMessage —— 必咬。
func TestTypingNotStoppedBySending(t *testing.T) {
	lines := codeLines(t, "plugin.go")
	// 只看发送消息那条路径的函数体（handleChannelOutput 是 output_send__* 的落点）。
	//
	// ★ 不能看全文件：`typingLoop` 超时自停是合法的
	// （`p.stopTyping(ts.userID)`），它必须保留 —— 否则 agent 卡死时
	// 指示器永远不会消失。这个判据第一版就误报了这一处。
	body := funcBody(t, lines, "func (p *Plugin) handleChannelOutput(")
	if strings.Contains(body, "stopTyping(") {
		t.Fatal("★ 发送消息仍在停输入指示 ——\n" +
			"   发送可能是群聊/发给别人；正在跟 A 私聊时发条群消息就会关掉 A 的指示")
	}
}

// ★★★ 判据 3：before_output 发起、after_output 结束（且是真调用，不是注释）。
func TestTypingWiredToOutputStages(t *testing.T) {
	lines := codeLines(t, "status.go")

	// 定位 stageBeforeOutput / stageAfterOutput 的函数体
	bodyOf := func(name string) string {
		// ★ 用 funcBody（括号计数），不能用"找行首 }"：
		// 函数体内有 if/for 块，第一个行首 `}` 往往是**内层块**的结尾，
		// 于是判据看到的是被截断的前半段 —— 后半段里的调用"看不见"，
		// 删掉也不咬（实测：stageAfterOutput 新增 if 分支后本判据假绿）。
		return funcBody(t, lines, "func (p *Plugin) "+name+"(")
	}

	before := bodyOf("stageBeforeOutput")
	if !strings.Contains(before, "p.startTypingForTurn(ctx)") {
		t.Fatal("★ before_output 没有发起输入指示（startTypingForTurn）")
	}
	// after_output 现在有**两条分支**（2026-10-18）：
	//
	//	单次 output_send  → stopTypingAfterOutput（配对的收尾）
	//	turn 边界         → stopTypingForTurn + auth 清理 + 状态归位
	//
	// 两条都必须存在：只留一条会让另一种路径的指示器收不了尾。
	after := bodyOf("stageAfterOutput")
	if !strings.Contains(after, "p.stopTypingAfterOutput(ctx)") {
		t.Fatal("★ after_output 没有为单次发送收尾（stopTypingAfterOutput）——" +
			"agent 用 output_send__qq 直接发消息时，打字指示器会一直挂着")
	}
	if !strings.Contains(after, "p.stopTypingForTurn(ctx)") {
		t.Fatal("★ after_output 的 turn 边界分支没有结束输入指示（stopTypingForTurn）")
	}

	// turn 边界分支必须先停指示再清理 auth —— auth 清理若失败，
	// 指示不能永久留在"正在输入"。
	iStop := strings.Index(after, "stopTypingForTurn")
	iAuth := strings.Index(after, "afterOutputAuthContext")
	if iStop < 0 || iAuth < 0 || iStop > iAuth {
		t.Fatal("★ turn 边界分支里应先停输入指示、再清理 auth")
	}
}

// ★★★ 判据 4：只认**注入方声明**的身份（inject_meta），不读全局 p.auth。
//
// 读 p.auth 会在并发私聊时串号：A 的消息进来后内核可能先跑 B 那一轮，
// before_output 读 p.auth 拿到 B ⇒ A 的输入指示打到 B 身上。
func TestTypingUsesInjectMetaNotGlobalAuth(t *testing.T) {
	lines := codeLines(t, "status.go")
	body := funcBody(t, lines, "func (p *Plugin) turnTypingUser(")

	if !strings.Contains(body, `ctx.Extra["qq_uid"]`) {
		t.Fatal("★ turnTypingUser 没读 payload 透传的 qq_uid —— 身份必须由注入方声明，" +
			"否则并发私聊会串号")
	}
	if strings.Contains(body, "p.auth") {
		t.Fatal("★ turnTypingUser 读了全局 p.auth —— 并发私聊时 A 的输入指示会打到 B 身上")
	}
	// 只处理自己引起的轮次
	if !strings.Contains(body, `ctx.Extra["input_source"]`) {
		t.Fatal("★ turnTypingUser 没校验 input_source —— 别人的输出不该动我们的指示器")
	}
}

// ★★★ 判据 5：注入时声明了身份（否则 inject_meta 是空的，前面全白搭）。
func TestInjectDeclaresTypingIdentity(t *testing.T) {
	lines := codeLines(t, "plugin.go")
	joined := strings.Join(lines, "\n")

	// qqInjectPayload 群聊返回 nil（QQ 的"正在输入"只在私聊标题栏有意义）
	body := funcBody(t, lines, "func qqInjectPayload(")
	if !strings.Contains(body, "isGroup") {
		t.Fatal("★ qqInjectPayload 没排除群聊 —— 群的标题栏没有\"正在输入\"语义")
	}

	// 所有 injectInterrupt 调用点都必须把 meta 传进去。
	//
	// ★ 用括号计数取实参，不用正则：实参里有嵌套调用
	// `qqInjectPayload(userID, groupID, ...)`，`[^)]*` 会提前截断导致误报。
	calls := callArgs(joined, "p.injectInterrupt")
	if len(calls) == 0 {
		t.Fatal("找不到 injectInterrupt 调用")
	}
	for _, c := range calls {
		if !strings.Contains(c, "qqInjectPayload") {
			t.Fatalf("★ injectInterrupt 调用没带上身份（payload）：(%s)\n"+
				"   不带 meta ⇒ before_output 拿不到 inject_meta ⇒ 指示器永远不出现", c)
		}
	}
}

// ★★★ 判据 6：inject_meta 经 JSON 往返后 user_id 可能是 float64 / json.Number。
//
// 只处理 int64 会让真实链路（proc 协议是 JSON）静默失效 ——
// 单测里传 int64 是绿的，生产里永远不发指示。这是最阴的一类。
func TestTurnTypingUserAcceptsJSONNumbers(t *testing.T) {
	p := &Plugin{name: "qq"}
	mk := func(v interface{}) *sdk.StageContext {
		ctx := &sdk.StageContext{Extra: map[string]interface{}{
			"input_source": "qq",
			"qq_uid":       v,
		}}
		return ctx
	}
	cases := []struct {
		name string
		v    interface{}
		want int64
	}{
		{"float64（JSON 解码默认）", float64(1456128421), 1456128421},
		{"int64", int64(1456128421), 1456128421},
		{"int", 1456128421, 1456128421},
		{"nil", nil, 0},
		{"字符串不算", "1456128421", 0},
	}
	for _, c := range cases {
		if got := p.turnTypingUser(mk(c.v)); got != c.want {
			t.Errorf("★ %s: user_id=%v 解析出 %d，期望 %d", c.name, c.v, got, c.want)
		}
	}

	// 非本插件来源：不该动我们的指示器
	ctx := &sdk.StageContext{Extra: map[string]interface{}{
		"input_source": "other",
		"qq_uid":       float64(1),
	}}
	if got := p.turnTypingUser(ctx); got != 0 {
		t.Errorf("★ 别人的轮次也被识别成本插件：%d", got)
	}
	// 无 payload 字段：不发
	if got := p.turnTypingUser(&sdk.StageContext{Extra: map[string]interface{}{}}); got != 0 {
		t.Errorf("★ 无 inject_meta 时不该发指示：%d", got)
	}
}

// ★★★ 判据 7：set_input_status 的 user_id 必须是**字符串**。
//
// NapCat 4.8.x 接受数字，但 4.18.37 的 payloadSchema 声明成 Type.String。
// 传数字会报 `uid is empty`（不是 schema 错误 —— 更容易误判成"用户不存在"），
// 功能**静默失效**：编译不报错、内核不告警、用户只是永远看不到"正在输入"。
//
// 实测：
//
//	{"user_id":1,"event_type":1}            → uid is empty
//	{"user_id":"1456128421","event_type":1} → retcode 0, errMsg success
func TestSetInputStatusSendsStringUserID(t *testing.T) {
	lines := codeLines(t, "plugin.go")
	body := funcBody(t, lines, "func (p *Plugin) setInputStatus(")
	if !strings.Contains(body, `strconv.FormatInt(userID, 10)`) {
		t.Fatal("★ set_input_status 的 user_id 没转成字符串 ——\n" +
			"   NapCat 4.18.37 的 payloadSchema 是 Type.String，传数字返回 `uid is empty`，\n" +
			"   「对方正在输入中」会静默失效（编译不报错、内核不告警）")
	}
}
