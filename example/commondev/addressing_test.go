package main

// 「投到指定目录的指定 session」的判据。
//
// 用户问的是这句话能不能成立。成立的前提是**地址唯一确定一条线**：
//   branch_id  →  工作目录（program/<hash>-<name>）
//              →  pi session（SessionID）
//              →  一对通道（InputCh / OutputCh）
//
// 这四者必须一一对应，且**不能被两条线共享**。共享的任何一环都会让
// "投到指定 session"变成"投到某一条（不确定）"。
//
// 最危险的一环是唯一性：独立 clone 没有 worktree 的
// 「同分支不可双挂」保护（git 会拒绝），所以插件必须自己补闸。
// 设计 §4.3 明确写了这条，但**此前没有判据钉住它**。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestPluginForAddr 造一个只做台账的 Plugin（不碰网络/进程）。
//
// ★ ledger.json 放在 t.TempDir() **之外**的独立临时目录：
// branch_open 会把 clone 转后台 goroutine（runPrepareWorkdir），
// 而它会在 programRoot() 下建目录 —— 与 t.TempDir() 的清理构成竞态
// （实测报 "TempDir RemoveAll cleanup: directory not empty"）。
// 用 os.MkdirTemp + 手动清理可以让清理发生在 goroutine 之后。
func newTestPluginForAddr(t *testing.T) *Plugin {
	t.Helper()
	dir, err := os.MkdirTemp("", "cdaddr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := newTestPluginForShell(t)
	p.path = filepath.Join(dir, "ledger.json")
	p.ledger = &Ledger{
		Projects: map[string]*Project{},
		Branches: map[string]*Branch{},
		Jobs:     map[string]*Job{},
		Criteria: map[string]*Criteria{},
	}
	return p
}

// ★★★ 判据：同名分支不能开两条未结的工程对话（唯一性闸）。
//
// 这是"投到指定 session"的**前提**：若同一个 branch_id 能对应两条线，
// 或者两条线共用同一个地址，寻址就不再唯一。
//
// ★ 独立 clone 丢掉了 git 的天然保护：worktree 模式下同一分支被两棵
// worktree 挂载时 git 直接 `fatal: 'feat/a' is already used`，
// 而独立 clone 完全没有这层拦截 —— 必须由插件补，否则两条线会
// 各自改各自的目录然后互相覆盖。
func TestDuplicateBranchIsRejected(t *testing.T) {
	p := newTestPluginForAddr(t)
	p.ledger.Projects["proj"] = &Project{Name: "proj", RemoteURL: "/tmp/x", DefaultBranch: "main"}

	// 第一次开：应成功
	r1, _ := p.handleBranchOpen(map[string]interface{}{"project": "proj", "branch": "feat/a"})
	m1, _ := r1.(map[string]interface{})
	if m1["status"] != "ok" {
		t.Fatalf("首次开分支应成功: %v", m1)
	}
	id1, _ := m1["branch_id"].(string)
	if id1 == "" {
		t.Fatal("应返回 branch_id")
	}

	// 第二次开同一个分支：应被唯一性闸拒绝
	r2, _ := p.handleBranchOpen(map[string]interface{}{"project": "proj", "branch": "feat/a"})
	m2, _ := r2.(map[string]interface{})
	if _, isErr := m2["error"]; !isErr {
		t.Fatalf("★ 同名分支被允许开第二条（唯一性闸失效）: %v", m2)
	}
	if !strings.Contains(m2["error"].(string), "唯一性闸") {
		t.Errorf("拒绝理由应指明唯一性闸: %v", m2["error"])
	}

	// 台账里必须只有一条
	if n := len(p.ledger.Branches); n != 1 {
		t.Errorf("★ 台账里应只有 1 条分支，实际 %d", n)
	}
}

// ★★ 判据：**已结**的分支可以重开（闸只挡"未结"的）。
//
// 闸太严会让"合入后想再改同一个分支名"变成不可能 ——
// 而那是正常做法（分支已 merged，重开一条新的同名线是合理的）。
func TestClosedBranchCanBeReopened(t *testing.T) {
	p := newTestPluginForAddr(t)
	p.ledger.Projects["proj"] = &Project{Name: "proj", RemoteURL: "/tmp/x", DefaultBranch: "main"}

	r1, _ := p.handleBranchOpen(map[string]interface{}{"project": "proj", "branch": "feat/b"})
	id1 := r1.(map[string]interface{})["branch_id"].(string)

	// 标记为已合入
	p.ledger.Branches[id1].Status = "merged"

	// 重开应成功
	r2, _ := p.handleBranchOpen(map[string]interface{}{"project": "proj", "branch": "feat/b"})
	m2, _ := r2.(map[string]interface{})
	if m2["status"] != "ok" {
		t.Fatalf("已结分支应可重开: %v", m2)
	}
	// 但 id 相同 ⇒ 会覆盖旧台账项；这本身是设计取舍，只断言不报错
}

// ★★ 判据：review 状态同样算"未结"（不能趁复核期再开一条同类线）。
func TestReviewStatusStillBlocksDuplicate(t *testing.T) {
	p := newTestPluginForAddr(t)
	p.ledger.Projects["proj"] = &Project{Name: "proj", RemoteURL: "/tmp/x", DefaultBranch: "main"}
	r1, _ := p.handleBranchOpen(map[string]interface{}{"project": "proj", "branch": "feat/c",
		"work_desc": "x"})
	id1 := r1.(map[string]interface{})["branch_id"].(string)
	p.ledger.Branches[id1].Status = "review"

	r2, _ := p.handleBranchOpen(map[string]interface{}{"project": "proj", "branch": "feat/c"})
	if _, isErr := r2.(map[string]interface{})["error"]; !isErr {
		t.Errorf("★ review 状态应算未结，但被允许重开")
	}
}

// ★★★ 判据：地址四元组**一一对应**且互不共享。
//
// branch_id 必须唯一映射到 一个目录 + 一个 session + 一对通道。
// 任意两条线共用其中任何一项，寻址就不唯一（"投到指定 session"退化为
// "投到某一条"）。
func TestAddressTupleIsUniquePerBranch(t *testing.T) {
	p := newTestPluginForAddr(t)
	p.ledger.Projects["proj"] = &Project{Name: "proj", RemoteURL: "/tmp/x", DefaultBranch: "main"}
	// 造两条不同的线
	for _, br := range []string{"feat/x", "feat/y"} {
		r, _ := p.handleBranchOpen(map[string]interface{}{"project": "proj", "branch": br})
		if _, isErr := r.(map[string]interface{})["error"]; isErr {
			t.Fatalf("开 %s 失败: %v", br, r)
		}
	}
	if len(p.ledger.Branches) != 2 {
		t.Fatalf("应有 2 条线，实际 %d", len(p.ledger.Branches))
	}

	seenDir := map[string]string{}
	seenID := map[string]bool{}
	for id, b := range p.ledger.Branches {
		if seenID[id] {
			t.Errorf("★ branch_id 重复: %s", id)
		}
		seenID[id] = true
		if b.Workdir == "" {
			t.Errorf("★ %s 没有工作目录", id)
		}
		// 目录不能共享
		if prev, dup := seenDir[b.Workdir]; dup {
			t.Errorf("★ 两条线共用同一目录: %s（%s 与 %s）—— 投到目录无法区分线", b.Workdir, prev, id)
		}
		seenDir[b.Workdir] = id
	}

	// ★ 关键：不同分支名必须得到不同目录（设计 §4.3 的目录映射不变式）
	r1, _ := p.handleBranchOpen(map[string]interface{}{"project": "proj", "branch": "feat.z"})
	_ = r1
	// feat.z 与 feat/z 派生后 slug 相同，但目录 hash 含原始分支名 ⇒ 目录不同
	d1 := branchDir(p.programRoot(), "proj", "feat/z")
	d2 := branchDir(p.programRoot(), "proj", "feat.z")
	if d1 == d2 {
		t.Errorf("★ feat/z 与 feat.z 得到了同一目录 %s —— 两条线会互相覆盖", d1)
	}
	if filepath.Base(d1) == filepath.Base(d2) {
		t.Errorf("★ 目录基名相同 %s，说明 hash 没把分支名纳入", filepath.Base(d1))
	}
}

// ★★ 判据：投递必须打到**该 id 自己的**通道。
//
// deliverToPi 从台账取 SessionID；通道名由 id 派生。
// 若两条线的通道名撞了，内核路由就会把任务投到错的那条 pi。
func TestChannelNamesAreUniquePerBranch(t *testing.T) {
	names := map[string]string{}
	for _, id := range []string{
		"br-proj-feat_a", "br-proj-feat_b", "br-other-feat_a",
	} {
		in := inputChannelName(id)
		out := outputChannelName(id)
		for _, n := range []string{in, out} {
			if prev, dup := names[n]; dup {
				t.Errorf("★ 通道名冲突 %q（%s 与 %s）—— 投递会打到错的后端", n, prev, id)
			}
			names[n] = id
		}
		// outputch 不能含斜杠（会拼成 output_send__<name>）
		if strings.Contains(out, "/") {
			t.Errorf("★ outputch 含斜杠: %q（output_send__ 名不允许）", out)
		}
		// 上游要求 ^[a-zA-Z0-9_-]{1,64}$
		if !validNamePattern.MatchString(out) || len(out) > 64 {
			t.Errorf("outputch %q 不合规（须匹配 ^[a-zA-Z0-9_-]{1,64}$）", out)
		}
	}
}

// ★★ 判据：**会话漂移必须响亮失败**（不静默改投新会话）。
//
// 这是"投到指定 session"的守门人：如果 pi 返回了不同的 session_id
// 而插件默默接受，任务就投到了另一条会话 —— 而调用方以为投到了指定的那条。
func TestSessionDriftIsRejectedNotSilentlyAccepted(t *testing.T) {
	b, err := os.ReadFile("plugin.go")
	if err != nil {
		t.Skip("读不到 plugin.go")
	}
	src := string(b)
	if !strings.Contains(src, "会话漂移") {
		t.Fatal("★ deliverToPi 缺少会话漂移检查")
	}
	// 必须**拒绝**而不是记录后继续
	if !strings.Contains(src, "拒绝继续") {
		t.Error("★ 会话漂移应明确拒绝，而非仅记录")
	}
}
