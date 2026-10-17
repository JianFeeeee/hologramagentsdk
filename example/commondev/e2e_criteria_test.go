package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ★★ 真机端到端：**真实项目 + 真实 go test**，验证刀 4 能识破
// 「真 bug 藏在松判据后面」。
//
// 场景（与 /tmp/d4e2e 手工复现的一致）：
//
//	Clamp(v,lo,hi) 有三个分支；判据只测了 v<lo
//	代码里删掉 v>hi 分支（真 bug）⇒ 松判据**仍全绿**
//
// 复核必须给出 **pass_with_gaps** 而不是 pass，并说明"变异未被咬住"。
func TestE2E_ReviewCatchesHiddenBugBehindLooseCriteria(t *testing.T) {
	if _, err := exec.Command("go", "version").Output(); err != nil {
		t.Skip("环境无 go")
	}
	dir := t.TempDir()
	write := func(rel, c string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module d4demo\n\ngo 1.21\n")
	write("calc.go", `package d4demo

func Clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
`)
	// ★ 松判据：只测下界
	write("calc_test.go", `package d4demo

import "testing"

func TestClampLoose(t *testing.T) {
	if Clamp(-5, 0, 10) != 0 {
		t.Fatal("低于下界时错")
	}
}
`)
	if err := exec.Command("git", "init", "-q", "-b", "main", dir).Run(); err != nil {
		t.Skipf("git init: %v", err)
	}

	p := newTestPluginForCriteria(t)
	p.ledger.Projects["demo"] = &Project{Name: "demo", RemoteURL: "x"}
	p.ledger.Branches["br-1"] = &Branch{
		ID: "br-1", ProjectID: "demo", Name: "feat/x", Workdir: dir, Status: "open"}

	// 主管定义判据：正向用松判据；变异**删掉 v>hi 分支**
	p.ledger.Criteria["cr-clamp"] = &Criteria{
		ID: "cr-clamp", ProjectID: "demo", Name: "clamp 上下界", Kind: "unit",
		Script: "go test -count=1 ./...",
		Expect: "上下界都被限制",
		Mutations: []Mutation{{
			ID: "M1", File: "calc.go",
			Old: "	if v > hi {\n		return hi\n	}\n", New: "",
			Note: "删掉上界分支（真 bug）——松判据看不见它",
		}},
		DefHash: "x",
	}

	res, err := p.reviewAll("br-1")
	if err != nil {
		t.Fatal(err)
	}
	verdict := res["verdict"].(string)

	// ① 正向必须**通过**（松判据确实能过 —— 这就是问题所在）
	if !res["forward_ok"].(bool) {
		t.Fatalf("前提失效：松判据正向应通过。report=%v", res["report"])
	}
	// ② 变异必须**未被咬住**（真 bug 躲在判据盲区里）
	if res["mutate_ok"].(bool) {
		t.Fatal("★ 删掉整个上界分支都没被咬住却报 mutate_ok=true —— 反向阶段失效")
	}
	// ③ 结论绝不能被当成 "pass"
	if verdict == "pass" {
		t.Fatal("★★ 真 bug + 松判据 ⇒ 给了 pass。这正是刀 4 存在的理由")
	}
	if verdict != "pass_with_gaps" {
		t.Errorf("应为 pass_with_gaps，实际 %q", verdict)
	}
	// ④ 报告里要能看出是**哪条**变异没被咬住
	rep, _ := res["report"].([]map[string]interface{})
	if len(rep) == 0 {
		t.Fatal("报告为空")
	}
	ev := str(rep[0], "mutate_evidence")
	if !strings.Contains(ev, "未被咬住") {
		t.Errorf("mutate_evidence 应指出未被咬住，实际: %s", ev)
	}
	// ⑤ 工作目录必须**已恢复**（变异不留脏文件）
	after, _ := os.ReadFile(filepath.Join(dir, "calc.go"))
	if !strings.Contains(string(after), "if v > hi {") {
		t.Fatal("★ 变异后未恢复 calc.go —— 工作目录被污染")
	}

	t.Logf("✅ 真机识破「真 bug 藏在松判据后」: verdict=%s", verdict)
	t.Logf("   notes=%v", res["notes"])
}

// ★ 对照：**补一条严判据**后，同一个 bug 会被正向阶段直接抓到。
func TestE2E_TightCriteriaCatchesTheBugDirectly(t *testing.T) {
	if _, err := exec.Command("go", "version").Output(); err != nil {
		t.Skip("环境无 go")
	}
	dir := t.TempDir()
	write := func(rel, c string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(c), 0o644)
	}
	write("go.mod", "module d4demo2\n\ngo 1.21\n")
	// ★ 这个项目里 bug **已经存在**（上界分支缺失）
	write("calc.go", `package d4demo2

func Clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	return v
}
`)
	// 严判据：上下界都测
	write("calc_test.go", `package d4demo2

import "testing"

func TestClampTight(t *testing.T) {
	if Clamp(-5, 0, 10) != 0 {
		t.Fatal("下界错")
	}
	if Clamp(15, 0, 10) != 10 {
		t.Fatal("上界错")
	}
}
`)
	exec.Command("git", "init", "-q", "-b", "main", dir).Run()

	p := newTestPluginForCriteria(t)
	p.ledger.Projects["demo2"] = &Project{Name: "demo2", RemoteURL: "x"}
	p.ledger.Branches["br-1"] = &Branch{
		ID: "br-1", ProjectID: "demo2", Name: "feat/x", Workdir: dir, Status: "open"}
	p.ledger.Criteria["cr-1"] = &Criteria{
		ID: "cr-1", ProjectID: "demo2", Name: "clamp 上下界", Kind: "unit",
		Script: "go test -count=1 ./...",
		Mutations: []Mutation{{ID: "M1", File: "calc.go",
			Old: "return v\n}", New: "return lo\n}"}},
		DefHash: "x",
	}

	res, err := p.reviewAll("br-1")
	if err != nil {
		t.Fatal(err)
	}
	// 正向就抓到 bug ⇒ fail
	if res["verdict"] != "fail" {
		t.Fatalf("严判据应正向就抓到 bug（fail），实际 %q", res["verdict"])
	}
	t.Log("✅ 对照：严判据在正向阶段直接抓到同一个 bug → fail")
}
