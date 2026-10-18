package main

// 「取消正在运行的任务」判据（2026-10-18 用户实测反馈）。
//
// ## 缺陷
//
// prompt 改成异步投递后，Go 侧只能"把任务发出去"，**无法收回**。于是用户
// 发现派错任务、或某条 session 卡住时，除了等它自己跑完以外没有任何办法 ——
// agent 在日志里也只能如实回答"无法取消，只能等它跑完"。
//
// 根因不是"没实现取消"，而是取消能力**从来没有被暴露**：pi 的 AgentSession
// 本身就有 abort()，但 commondev 的 30 个工具里一个 cancel/stop 都没有，
// sidecar 也只有 ping/prompt/state/close 四个 method。
//
// ## 判据设计
//
// 关键是**不谎报成功**：sidecar 没在跑、或该 CLI 没实现 interrupt 时，
// 必须把"没有真正中断"如实回给调用方。否则用户以为停了、实际还在跑。

import (
	"os"
	"strings"
	"testing"
)

// ★★★ sidecar 必须实现 interrupt，且 pi 用的是 session.abort()
func TestPiSidecarImplementsInterrupt(t *testing.T) {
	for _, path := range []string{
		"sidecar.mjs",                    // 单测/SSH e2e 用
		"rawfile/runtime/pi/sidecar.mjs", // 部署用（hmapdev 打进 rawfile）
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读 %s: %v", path, err)
		}
		src := string(b)
		if !strings.Contains(src, `case "interrupt":`) {
			t.Fatalf("★ %s 没有 interrupt method —— 取消能力无从触达", path)
		}
		// pi 必须用自带的 abort()，而不是自己造机制
		if !strings.Contains(src, "session.abort()") {
			t.Fatalf("★ %s 的 interrupt 没有调用 pi 的 session.abort()", path)
		}
		// 必须 await（abort 返回 Promise；不 await 会立刻回"已中断"但实际还在跑）
		if !strings.Contains(src, "await session.abort()") {
			t.Fatalf("★ %s 的 abort() 没有 await —— 会谎报已中断", path)
		}
		// ★ 判定"是否真的中断"必须用 pi 的 isIdle，不能用包装层的 inflight。
		//
		// 我第一版写 `interrupted: hadInflight`（中断前 >0 就算成功），
		// 于是"删掉 abort()"这个变异**完全测不出来**：任务还在跑，
		// hadInflight 仍为 true，照样回报"已中断" —— 那正是最坏的情形。
		//
		// 第二版改用 inflight 的变化，又**反向**错了：inflight 只在后台
		// promise 的 finally 里递减，abort() 返回时还没减 ⇒ 实测
		// before=1/inflight=1 误报"未中断"，而任务其实已经停了。
		//
		// isIdle 是 pi 的权威视图，正确。
		if !strings.Contains(src, "session.isIdle") {
			t.Fatalf("★ %s 判定中断成功没有用 session.isIdle —— "+
				"用 inflight 之类的包装计数会误报（两个方向都试过了）", path)
		}
		// ★ 只查**代码行**，不查注释：这个反例本身就该写在注释里当说明，
		// 直接全文 Contains 会把解释性注释当成缺陷（实测误报过一次）。
		for _, ln := range strings.Split(src, "\n") {
			code := strings.TrimSpace(ln)
			if strings.HasPrefix(code, "//") || strings.HasPrefix(code, "*") {
				continue
			}
			if strings.Contains(code, "interrupted: hadInflight") {
				t.Fatalf("★ %s 又回到了 `interrupted: hadInflight` —— "+
					"只看中断前 >0 会让「删掉 abort」的变异测不出", path)
			}
		}
	}
}

// ★★★ zcode 是子进程模式，取消 = 整组杀掉当前回合
func TestZcodeSidecarInterruptKillsChild(t *testing.T) {
	b, err := os.ReadFile("rawfile/runtime/zcode/sidecar.mjs")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if !strings.Contains(src, `case "interrupt":`) {
		t.Fatal("★ zcode sidecar 没有 interrupt method")
	}
	// 必须真的杀进程，而不是只返回 ok
	seg := src[strings.Index(src, `case "interrupt":`):]
	if end := strings.Index(seg, `case "close":`); end > 0 {
		seg = seg[:end]
	}
	if !strings.Contains(seg, "killTree(") {
		t.Fatalf("★ zcode 的 interrupt 没有 killTree —— 只回 ok 不真杀")
	}
	if !strings.Contains(seg, "currentChild") {
		t.Fatalf("★ zcode 的 interrupt 没有用 currentChild（杀不到真实进程）")
	}
}

// ★★★ 三个 sidecar 必须语法合法（node --check 之外的静态兜底：
// 至少不能有重复声明这类必炸错误）
func TestSidecarsHaveNoDuplicateDecl(t *testing.T) {
	for _, path := range []string{
		"sidecar.mjs",
		"rawfile/runtime/pi/sidecar.mjs",
		"rawfile/runtime/zcode/sidecar.mjs",
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读 %s: %v", path, err)
		}
		n := strings.Count(string(b), "let currentChild = null;")
		if n > 1 {
			t.Fatalf("★ %s 里 currentChild 声明了 %d 次（重复声明 = 启动即 SyntaxError）", path, n)
		}
	}
}

// ★★★ 核心纪律判据：**不谎报成功**。
//
// sidecar 没在跑、或 CLI 没实现 interrupt 时，必须如实回 interrupted=false。
// 谎报的代价：用户以为停了，实际任务还在跑并继续改代码/推分支。
func TestAgentInterruptNeverLiesAboutSuccess(t *testing.T) {
	p := newTestPluginForShell(t)
	p.ledger = &Ledger{
		Projects: map[string]*Project{},
		Branches: map[string]*Branch{},
		Jobs:     map[string]*Job{},
		Criteria: map[string]*Criteria{},
	}

	// ① 不存在的 branch → 报错，不能回 interrupted=true
	out, err := p.handleAgentInterrupt(map[string]interface{}{"branch_id": "br-nope"})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	m := out.(map[string]interface{})
	if v, _ := m["interrupted"].(bool); v {
		t.Fatal("★ 对不存在的 branch 回了 interrupted=true")
	}
	if m["error"] == nil {
		t.Fatal("对不存在的 branch 应给出 error")
	}

	// ② branch 存在但 sidecar 没跑 → interrupted=false + note
	// 直接塞进 ledger，不调 handleBranchOpen（它会触发真实 git clone）
	id := "br-iproj-ifeat"
	p.ledger.Branches[id] = &Branch{
		ID: id, ProjectID: "iproj", Name: "feat/ifeat",
		Status: "open", Runtime: "local", AgentCLI: "pi",
	}

	out2, err := p.handleAgentInterrupt(map[string]interface{}{"branch_id": id})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	m2 := out2.(map[string]interface{})
	if v, _ := m2["interrupted"].(bool); v {
		t.Fatal("★ sidecar 没在跑却回了 interrupted=true —— 谎报成功")
	}
	if m2["note"] == nil {
		t.Fatal("sidecar 没跑时应给出 note 说明")
	}
}
