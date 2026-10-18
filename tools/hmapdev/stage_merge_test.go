package main

// 模板判据：同一 stage 多次注册必须**合并**而不是静默覆盖。
//
// ## 修的是什么（真实事故，2026-10-18）
//
// 模板里原来是：
//
//	stageHandlers[string(stage)] = handler   // stage 名为键，后者覆盖前者
//
// 有人写了两次同 stage 注册：
//
//	RegisterStage(BeforeToolcall, 权限门)   ← 被下一条覆盖
//	RegisterStage(BeforeToolcall, 切状态)   ← 实际生效
//
// 后果不只是状态不切换 —— **权限门被静默关掉了**（那几版 QQ 插件里
// 任何人发消息都能调任何工具）。而内核侧日志显示 `2 handlers [qq qq]`，
// 看起来完全正常 —— 这是最难查的一类 bug。
//
// ## 为什么判据在工具链而不是 SDK
//
// 用户明确要求：「是工具链对同stage多handler注册给出告警」。
// 工具链是生成 proc_main 的地方，也就是 map 覆盖实际发生的地方；
// 在源头拦，告警直接出现在**插件启动日志**里，开发者本地就能看到。
//
// ## 判据验什么
//
//  · 存在"重复注册"分支（不是无条件赋值）
//  · 合并是**链式**的（prev 先跑，返回 error 中止）
//  · 有告警（log.Printf + 重复注册字样）
//  · 重复注册**不**再次上报 stage.register（否则内核会记两条假 handler）

import (
	"strings"
	"testing"
)

func TestTemplateMergesDuplicateStageRegistrations(t *testing.T) {
	data, err := procTemplates.ReadFile("templates/proc_main.go.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)

	// 定位 stage 注册闭包：从 `func(stage sdk.Stage, handler sdk.StageHandler) {` 起
	i := strings.Index(src, "func(stage sdk.Stage, handler sdk.StageHandler) {")
	if i < 0 {
		t.Fatal("找不到 stage 注册闭包 —— 模板结构变了，判据需同步")
	}
	body := src[i:]
	if j := strings.Index(body[1:], "\n\t\tfunc("); j > 0 {
		body = body[:j+1]
	}

	// ① 必须有"已存在"的判断（不是无条件赋值）
	if !strings.Contains(body, "prev, dup :=") && !strings.Contains(body, "dup := false") {
		t.Fatalf("★ stage 注册没有检测重复 —— 仍在静默覆盖。\n片段：\n%s", body)
	}

	// ② 必须是**链式**合并：prev 先跑
	if !strings.Contains(body, "prev(ctx)") {
		t.Fatal("★ 重复注册没有链式调用 prev —— 合并后旧 handler 会丢失")
	}

	// ③ 第一个 handler 返回 error 必须中止（与内核 RunStage 语义一致）
	if !strings.Contains(body, "if err := prev(ctx); err != nil {") {
		t.Fatal("★ 链式合并没有在 prev 出错时中止 —— 权限门拒绝了还会继续执行后续 handler")
	}

	// ④ 必须告警，且必须是**代码行**（不能靠注释里的字眼蒙混）。
	//
	// 第一版写成裸 `strings.Contains(body, "重复注册")` —— 把注释里的解释
	// 也算进去了，于是删掉 log.Printf 后判据照样通过（变异不咬，实测）。
	// 必须跳过注释行再找。
	var warnCode string
	for _, ln := range strings.Split(body, "\n") {
		code := strings.TrimSpace(ln)
		if strings.HasPrefix(code, "//") || strings.HasPrefix(code, "*") {
			continue
		}
		if strings.Contains(code, "log.Printf") && strings.Contains(code, "stage") {
			warnCode = code
			break
		}
	}
	if warnCode == "" {
		t.Fatal("★ 重复注册没有告警代码行 —— 开发者无从知道该优化写法")
	}
	// 告警文案要说明“建议合并写法”（带提示，而不只是“重复了”）
	if !strings.Contains(body, "建议把多个功能合并进同一个 handler") {
		t.Fatal("★ 告警没有给出行动建议（应提示合并写法）")
	}

	// ⑤ 重复注册不能再上报一次 stage.register
	//    （内核会记两条 handler，但实际只调度一次 ⇒ 制造 `2 handlers [qq qq]` 假象）
	dupBlock := body
	if k := strings.Index(body, "if dup {"); k >= 0 {
		dupBlock = body[k:]
		if m := strings.Index(dupBlock, "\n\t\t\t}"); m > 0 {
			dupBlock = dupBlock[:m]
		}
		if strings.Contains(dupBlock, "stage.register") {
			t.Fatal("★ 重复注册仍然上报了 stage.register —— " +
				"内核会记录两条 handler 而实际只派发一次，制造假象")
		}
	} else {
		t.Fatal("★ 找不到 `if dup {` 分支")
	}
}
