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
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ★★★ sidecar 必须实现 interrupt，且 pi 用的是 session.abort()
//
// ## 为什么只看**跟踪的源**，不看 rawfile/ 下的部署路径
//
// 这条判据原本还查 `rawfile/runtime/pi/sidecar.mjs`（部署路径）。那是错的：
// `rawfile/.gitignore` 写的是 `*`，rawfile/ 的内容**完全不进版本库**，
// 干净克隆下那两条路径必然不存在 ⇒ 判据在 CI 上永远红，
// 而它红的理由（文件不存在）与它想守的东西（interrupt 有没有实现）**毫无关系**。
//
// 静态契约要盯的是**进版本库的那份源**；"部署路径里到底有没有它"
// 是**打包层**的事，由 ssh_wiring_test.go 的 TestPackageShipsSidecarScript
// 真打一个 .hmap 验 zip 条目来守。两件事分开，各自盯自己那层。
func TestPiSidecarImplementsInterrupt(t *testing.T) {
	for _, path := range []string{
		"sidecar.mjs", // 权威源：单测/SSH e2e 与 hmapdev 打包都以它为源
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
	b, err := os.ReadFile("examples/custom-cli/zcode/sidecar.mjs")
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

// ★★★ 两个 sidecar 必须语法合法（node --check 之外的静态兜底：
// 至少不能有重复声明这类必炸错误）
func TestSidecarsHaveNoDuplicateDecl(t *testing.T) {
	for _, path := range []string{
		"sidecar.mjs",
		"examples/custom-cli/zcode/sidecar.mjs",
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

// ★★★ 行为判据：zcode 的 interrupt 在**没有在跑的回合**时必须如实回 false。
//
// ## 为什么必须补这一条（变异测出来的缺口）
//
// 把 `if (!currentChild) return {ok:true, interrupted:false, ...}` 改成
// `interrupted: true` 之后，**整套默认测试全绿**：那条语义只在
// `HA_ZCODE_E2E=1` 的 e2e 里被断言，而 e2e 默认 skip。
// 也就是说"不谎报"这条最关键的纪律，在 CI 的默认路径上是**零覆盖**。
//
// ## 为什么用行为而不是 grep 源码
//
// grep `interrupted: false` 只证明那个字面串在文件里，
// 把它挪到一个永不执行的分支、或加个 `!` 反转，grep 都咬不住
// （本仓已有三次同类教训：字面串判据把变量改个名就绕过）。
// 这里**真起 sidecar 进程**发一条 interrupt，读它**实际回的 JSON**。
//
// 不需要真 zcode CLI：interrupt 在没有 currentChild 时直接返回，
// 根本走不到 spawn CLI 那一步。
func TestZcodeInterruptWithoutRunningTurnDoesNotLie(t *testing.T) {
	node := findNodeForTest(t)
	sidecar := absUnderPackage(t, filepath.Join("examples", "custom-cli", "zcode", "sidecar.mjs"))

	work := t.TempDir()
	cmd := exec.Command(node, sidecar, "--workdir", work, "--session-id", "br-lie", "--mode", "guarded")
	cmd.Dir = work
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	// 不发 prompt —— 直接问 interrupt（此刻没有任何在跑的回合）
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "interrupt"})
	if _, err := stdin.Write(append(req, '\n')); err != nil {
		t.Fatalf("写 stdin: %v", err)
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var res map[string]any
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if id, _ := m["id"].(float64); id != 1 {
			continue
		}
		if e, ok := m["error"].(map[string]any); ok {
			t.Fatalf("interrupt 返回错误（不应）：%v", e)
		}
		res, _ = m["result"].(map[string]any)
		break
	}
	if res == nil {
		t.Fatal("没等到 interrupt 的应答")
	}
	if v, _ := res["interrupted"].(bool); v {
		t.Fatalf("★ 没有在跑的回合却回了 interrupted=true —— 谎报成功：%v", res)
	}
	if reason, _ := res["reason"].(string); reason != "no_running_turn" {
		t.Errorf("reason 应为 no_running_turn（让调用方能分辨「没有任务」与「杀失败」），实际 %q: %v",
			reason, res)
	}
	t.Logf("✅ 无回合时如实回报: %v", res)
}
