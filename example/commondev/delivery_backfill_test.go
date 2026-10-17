package main

// 投递台账**结果回填**的判据（2026-10-18 生产实测缺陷）。
//
// ## 缺陷
//
// 异步投递后，投递那一刻只记 "accepted"；真实结果几十秒后才由完成通知
// 送回。但原先**没人把它写回台账** ⇒ 台账永远停在 accepted：
//
//	2026-10-18T07:39:09 accepted  result=None
//	2026-10-18T07:40:09 accepted  result=None
//	2026-10-18T07:41:18 accepted  result=None     ← 13 条全是 None
//
// 实测后果（同一天的日志）：
//
//	[timer] firing: 【轮询轮次 2/6】…检查是否有新 result 回填…
//
// agent 自己起了 6 轮 timer 去轮询"有没有新 result"——而那件事永远不会发生。
//
// ## 判据
//
//  1. 完成通知必须回填最新那条 accepted
//  2. 只回填该 branch 的，不串到别的工程线
//  3. 已回填的不会被第二条通知重复覆盖（幂等边界）
//  4. 无匹配时不能 panic / 不能误改别的记录

import (
	"os"
	"strings"
	"testing"
)

func newLedgerPlugin(t *testing.T) *Plugin {
	t.Helper()
	dir := t.TempDir()
	p := &Plugin{path: dir + "/plugin.bin", ledger: &Ledger{}}
	p.ledger.Branches = map[string]*Branch{}
	return p
}

// ★★★ 判据：完成通知回填最新 accepted。
func TestCompleteDeliveryBackfillsLatestAccepted(t *testing.T) {
	p := newLedgerPlugin(t)
	p.recordDelivery("br-a", "sess-1", "任务一", "accepted", "")
	p.recordDelivery("br-a", "sess-1", "任务二", "accepted", "")

	p.completeDelivery("br-a", "sess-2", "done", "改好了 multiply")

	ds := p.ledger.Deliveries
	if len(ds) != 2 {
		t.Fatalf("投递条数应不变（2），实际 %d", len(ds))
	}
	// 最新那条被回填
	if ds[1].Status != "done" || ds[1].Result != "改好了 multiply" {
		t.Fatalf("★ 最新 accepted 未回填：status=%q result=%q", ds[1].Status, ds[1].Result)
	}
	if ds[1].SessionID != "sess-2" {
		t.Errorf("session 应更新为 sess-2，实际 %q", ds[1].SessionID)
	}
	// 更早那条不能被误改
	if ds[0].Status != "accepted" || ds[0].Result != "" {
		t.Fatalf("★ 误改了更早的记录：status=%q result=%q", ds[0].Status, ds[0].Result)
	}
}

// ★★★ 判据：只回填同一 branch，不串线。
func TestCompleteDeliveryDoesNotCrossBranches(t *testing.T) {
	p := newLedgerPlugin(t)
	p.recordDelivery("br-a", "s1", "a 的任务", "accepted", "")
	p.recordDelivery("br-b", "s2", "b 的任务", "accepted", "")

	p.completeDelivery("br-a", "s1", "done", "a 完成")

	if p.ledger.Deliveries[0].Status != "done" {
		t.Fatal("br-a 的记录应被回填")
	}
	if p.ledger.Deliveries[1].Status != "accepted" {
		t.Fatalf("★ br-b 的记录被误改：status=%q", p.ledger.Deliveries[1].Status)
	}
}

// ★★★ 判据：已回填的记录不会被第二条通知覆盖（同一投递只认第一次结果）。
func TestCompleteDeliveryIsNotOverwritten(t *testing.T) {
	p := newLedgerPlugin(t)
	p.recordDelivery("br-a", "s1", "任务", "accepted", "")

	p.completeDelivery("br-a", "s1", "done", "第一次结果")
	p.completeDelivery("br-a", "s1", "failed", "第二次结果")

	d := p.ledger.Deliveries[0]
	if d.Status != "done" || d.Result != "第一次结果" {
		t.Fatalf("★ 已回填的记录被第二条通知覆盖：status=%q result=%q", d.Status, d.Result)
	}
}

// ★★★ 判据：没有待回填记录时不 panic、不改任何东西。
func TestCompleteDeliveryNoMatchIsSafe(t *testing.T) {
	p := newLedgerPlugin(t)
	p.recordDelivery("br-a", "s1", "任务", "failed", "同步就失败了")

	// 这条是 failed，不是 accepted ⇒ 不该被回填
	p.completeDelivery("br-a", "s1", "done", "迟到的通知")

	if p.ledger.Deliveries[0].Status != "failed" {
		t.Fatalf("★ 非 accepted 的记录被回填了：%q", p.ledger.Deliveries[0].Status)
	}
	// 完全无记录也不该 panic
	p.completeDelivery("no-such-branch", "", "done", "x")
}

// ★★★ 判据：recordDelivery 必须返回 key（回填/追溯的前提）。
func TestRecordDeliveryReturnsKey(t *testing.T) {
	p := newLedgerPlugin(t)
	k := p.recordDelivery("br-a", "s1", "任务", "accepted", "")
	if k == "" {
		t.Fatal("recordDelivery 应返回非空 key（否则无法追溯某次投递）")
	}
	if p.ledger.Deliveries[0].Key != k {
		t.Fatalf("返回的 key 与台账记录不一致：%q vs %q", k, p.ledger.Deliveries[0].Key)
	}
}

// ★★★ 判据：**完成通知必须真的调用回填**（接线，不只是函数存在）。
//
// 为什么需要这条（2026-10-18 实测判据漏洞）：
// 上面四条测的是 `completeDelivery` 自身行为。但我把 sidecar.go 里
// **那一行调用删掉**之后，它们依然全绿 —— 因为没人再调它了。
// 而这正是原始缺陷的形态：函数写得对，但没人用。
//
// ⇒ 判据必须覆盖接线。这里读 sidecar.go 的 `makePromptNotifier` 块，
//
//	要求 prompt_done / prompt_error 两条分支里都出现 completeDelivery 调用。
//
// ★ 用**行扫描界定块边界**而不是正则：正则遇到嵌套大括号容易提前结束
//
//	（本仓在别处踩过这个坑）。
func TestPromptNotifierWiresCompleteDelivery(t *testing.T) {
	b, err := os.ReadFile("sidecar.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)

	i := strings.Index(src, "func (p *Plugin) makePromptNotifier")
	if i < 0 {
		t.Fatal("找不到 makePromptNotifier")
	}
	block := src[i:]
	if j := strings.Index(block[1:], "\nfunc "); j > 0 {
		block = block[:j+1]
	}

	for _, ev := range []string{"prompt_done", "prompt_error"} {
		k := strings.Index(block, `case "`+ev+`":`)
		if k < 0 {
			t.Fatalf("★ makePromptNotifier 里没有 %s 分支", ev)
		}
		// 该分支到**下一个** case 之间必须出现回填调用。
		//
		// ★ 偏移必须跳过当前 "case " 本身：从 k 开始找 "case " 会命中
		// k 自己（n=0），于是 seg 不被截断、包含整个函数剩余部分 ——
		// 那样 prompt_done 分支即便**删掉**回填调用也照样能"找到"
		// prompt_error 分支里的那一行 ⇒ 判据假绿。
		// 实测踩过：变异删掉 done 分支的回填，判据仍然 PASS。
		seg := block[k:]
		lead := len(`case "` + ev + `":`)
		if n := strings.Index(seg[lead:], "case "); n >= 0 {
			seg = seg[:lead+n]
		}
		if !strings.Contains(seg, "p.completeDelivery(") {
			t.Fatalf("★ %s 分支**没有调用 completeDelivery** —— "+
				"结果是：台账永远停在 accepted，看板与 agent 都查不到任何结果\n"+
				"（实测后果：agent 起了 6 轮 timer 空等一个永远不会出现的结果）", ev)
		}
	}
}
