package main

// 群消息的 @ 门判据。
//
// ## 用户的核心关切（2026-10-18）
//
// 「非 @agent 的群消息会不会产生中断」
//
// 答复：**不会**。代码路径上 `isAtBot` 为 false 就直接 return，
// 根本走不到 `enqueueInterrupt`。但这条链路必须被钉住 ——
// 一旦有人改了 return 或加个 fallthrough，bot 就会在群里乱说话。
//
// ## 同时修正的缺陷：@全体成员被当成 @bot
//
// 原实现：`if v == botIDStr || v == "all" { return true }`
// 后果：任何人 @全体成员 都会拉起 bot。那不是"在叫 bot"，是"在叫所有人"
// —— 一个是针对性请求，一个是广播，语义完全不同。
//
// 现场佐证：群友直接质问过「又没@你，你就这么确定是找你的？」。

import (
	"strings"
	"testing"
)

// ifBlockAt 从 s[start:] 的 `if ... {` 起，用**括号计数**截出整个 if 块。
//
// 为什么不能用字符串找 `}`：源码缩进/换行/嵌套块都会让固定串匹配失败，
// 而失败后通常不会报错，只会悄悄把范围放大到文件末尾 ——
// 判据于是变得永远为真（false green）。这个坑在本项目已踩过多次。
func ifBlockAt(s string, start int) string {
	depth := 0
	seen := false
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
			seen = true
		case '}':
			depth--
			if seen && depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// atEvent 造一段 OneBot 的 message 段数组（type=at）。
func atEvent(qq string) []interface{} {
	return []interface{}{
		map[string]interface{}{
			"type": "at",
			"data": map[string]interface{}{"qq": qq},
		},
	}
}

// ★★★ 判据 1：@全体成员**不算** @bot。
func TestIsAtBotRejectsAtAll(t *testing.T) {
	p := &Plugin{botID: 1456128421}
	if p.isAtBot(atEvent("all")) {
		t.Fatal("★ @全体成员 被当成了 @bot ——\n" +
			"   任何人 @全体都会拉起 bot。那不是“在叫 bot”，是“在叫所有人”。\n" +
			"   群里误响应的代价是在未被邀请的场合发言")
	}
}

// ★★★ 判据 2：@bot 本人才算。
func TestIsAtBotAcceptsSelf(t *testing.T) {
	p := &Plugin{botID: 1456128421}
	if !p.isAtBot(atEvent("1456128421")) {
		t.Fatal("★ @bot 本人没被识别 —— 群里的正常呼叫会漏响应")
	}
	// 数字形式（部分客户端会传数字）
	numEvt := []interface{}{
		map[string]interface{}{
			"type": "at",
			"data": map[string]interface{}{"qq": float64(1456128421)},
		},
	}
	if !p.isAtBot(numEvt) {
		t.Fatal("★ 数字形式的 @bot 没被识别")
	}
}

// ★★★ 判据 3：@别人 不算。
func TestIsAtBotRejectsOthers(t *testing.T) {
	p := &Plugin{botID: 1456128421}
	if p.isAtBot(atEvent("2198972886")) {
		t.Fatal("★ @别人 被当成了 @bot")
	}
	// 纯文本也不算
	textOnly := []interface{}{
		map[string]interface{}{"type": "text", "data": map[string]interface{}{"text": "1456128421"}},
	}
	if p.isAtBot(textOnly) {
		t.Fatal("★ 纯文本里出现 bot 号码被当成了 @ —— 那是可伪造的")
	}
}

// ★★★ 判据 4：**接线** —— 非 @ 的群消息必须 return，不能走到注入。
//
// 这一类“函数对、接线断”编译器不报、运行时不报错，
// 后果是 bot 在群里对每条消息都回应。
func TestNonAtGroupMessageReturnsBeforeInject(t *testing.T) {
	lines := codeLines(t, "plugin.go")
	joined := strings.Join(lines, "\n")

	// 找到 @bot 判断与 enqueueInterrupt 调用的相对位置
	iAt := strings.Index(joined, "if !p.isAtBot(evt.Message) {")
	if iAt < 0 {
		t.Fatal("找不到 @bot 判断 —— 判据需同步")
	}
	iInject := strings.Index(joined, "p.enqueueInterrupt(")
	if iInject < 0 {
		t.Fatal("找不到 enqueueInterrupt 调用")
	}
	if iAt > iInject {
		t.Fatal("★ @bot 判断在 enqueueInterrupt **之后** —— " +
			"非 @ 的群消息也会产生中断（bot 会在群里对每条消息回应）")
	}

	// 该分支内必须有 return（不能只是 log 完继续往下走）。
	//
	// ★ 必须用**括号计数**截出分支体，不能用字符串找结束位置：
	// 判据第一版用 `strings.Index(branch, "}\n\t}")`，而 joined 里的缩进
	// 已被 strip ⇒ 找不到（返回 -1）⇒ branch 一路延伸到文件末尾，
	// 于是"文件里任何别处的 return"都让它通过 —— 删掉这条 return 也不咬（实测）。
	branch := ifBlockAt(joined, iAt)
	if branch == "" {
		t.Fatal("无法截出 if 分支体 —— 判据需同步")
	}
	if !strings.Contains(branch, "return") {
		t.Fatal("★ `if !isAtBot` 分支里没有 return —— " +
			"非 @ 的群消息会继续走到注入")
	}
}
