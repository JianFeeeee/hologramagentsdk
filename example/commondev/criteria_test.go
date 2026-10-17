package main

// 刀 4 的判据：判据与复核（正向 + 变异）。
//
// ## 这组判据要证明的核心命题
//
// 设计 §4.5：「插件 GREEN ≠ 改对了」——
//
//	插件只能跑主管**预先写下**的判据。判据没覆盖那条 bug，插件照样打勾。
//	自动化**不会**发现假绿，只会让假绿跑得更快、更可信。
//
// ⇒ 复核必须是两步：正向（判据应过）+ 反向（变异应被咬住）。
//
// 所以本文件最重要的判据是 **TestReviewCatchesLooseCriteria**：
// 造一个「真 bug 但判据太松」的场景，断言复核**不给出 pass**。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newTestPluginForCriteria 建一个带台账的插件。
func newTestPluginForCriteria(t *testing.T) *Plugin {
	t.Helper()
	p := newTestPluginForShell(t)
	p.ledger = &Ledger{
		Projects: map[string]*Project{},
		Branches: map[string]*Branch{},
		Jobs:     map[string]*Job{},
		Criteria: map[string]*Criteria{},
	}
	return p
}

// writeRepo 在工作目录里放一个可被变异的最小 Go 项目。
func writeRepo(t *testing.T, loose bool) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite := func(rel, content string) {
		path := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("go.mod", "module probe\n\ngo 1.21\n")
	mustWrite("calc.go", `package calc

func Max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
`)
	if loose {
		// ★ 松判据：只测 a>b 的分支
		mustWrite("calc_test.go", `package calc

import "testing"

func TestMax(t *testing.T) {
	if Max(5, 3) != 5 {
		t.Fatal("错")
	}
}
`)
	} else {
		// 严判据：两个方向都测
		mustWrite("calc_test.go", `package calc

import "testing"

func TestMax(t *testing.T) {
	if Max(5, 3) != 5 {
		t.Fatal("a>b 错")
	}
	if Max(3, 5) != 5 {
		t.Fatal("b>a 错")
	}
}
`)
	}
	return dir
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// ★★★ 本刀最重要的判据：**复核必须识破「真 bug + 松判据」**。
//
// 场景：
//
//	代码 Max 正确；判据只测 Max(5,3)（漏了 b>a）
//	变异：return b → return a（真 bug）
//	松判据**仍通过** ⇒ 该变异"未被咬住"
//
// 期望：复核结论**不是 pass**（而是 pass_with_gaps），并明确指出
// "变异未被咬住 ⇒ 判据太松"。
//
// 若这条报红，说明复核机制回到了"只看正向"——那就失去了本刀的全部意义。
func TestReviewCatchesLooseCriteria(t *testing.T) {
	p := newTestPluginForCriteria(t)
	dir := writeRepo(t, true) // ★ 松判据

	p.ledger.Projects["probe"] = &Project{Name: "probe", RemoteURL: "x"}
	p.ledger.Branches["br-1"] = &Branch{ID: "br-1", ProjectID: "probe", Name: "feat/x", Workdir: dir, Status: "open"}
	p.ledger.Criteria["cr-1"] = &Criteria{
		ID: "cr-1", ProjectID: "probe", Name: "max 单元判据", Kind: "unit",
		Script: "go test ./...",
		Expect: "两方向都正确",
		Mutations: []Mutation{{
			ID: "M1", File: "calc.go",
			Old: "\treturn b\n}", New: "\treturn a\n}",
			Note: "验证判据是否覆盖 b>a 分支",
		}},
		DefHash: "x",
	}

	res, err := p.reviewAll("br-1")
	if err != nil {
		t.Fatal(err)
	}
	verdict := res["verdict"].(string)

	// ① 正向应通过（松判据确实能过）
	if !res["forward_ok"].(bool) {
		t.Fatalf("前提失效：松判据的正向应通过，实际 evidence=%v", res["report"])
	}
	// ② 反向必须**没被咬住**
	if res["mutate_ok"].(bool) {
		t.Fatal("★ 变异未被咬住却报 mutate_ok=true —— 反向阶段没在起作用")
	}
	// ③ 结论**绝不能是 pass**
	if verdict == "pass" {
		t.Fatal("★★ 真 bug + 松判据 却给出 pass —— " +
			"这正是设计 §4.5 说的『给测试通过充值信用，直到它骗一次大的』")
	}
	if verdict != "pass_with_gaps" {
		t.Errorf("结论应为 pass_with_gaps，实际 %q", verdict)
	}
	// ④ 必须明确说明原因（不能只给个模糊的黄色）
	notes, _ := res["notes"].([]string)
	joined := strings.Join(notes, " ")
	if !strings.Contains(joined, "太松") && !strings.Contains(joined, "未被咬住") {
		t.Errorf("notes 应指出『判据太松』，实际: %v", notes)
	}
	t.Logf("✅ 松判据被识破：verdict=%s notes=%v", verdict, notes)
}

// ★★ 判据：**严判据**时复核给出 pass（机制不能把一切都说成有问题）。
func TestReviewPassesWithTightCriteria(t *testing.T) {
	p := newTestPluginForCriteria(t)
	dir := writeRepo(t, false) // 严判据

	p.ledger.Projects["probe"] = &Project{Name: "probe", RemoteURL: "x"}
	p.ledger.Branches["br-1"] = &Branch{ID: "br-1", ProjectID: "probe", Name: "feat/x", Workdir: dir, Status: "open"}
	p.ledger.Criteria["cr-1"] = &Criteria{
		ID: "cr-1", ProjectID: "probe", Name: "max 单元判据", Kind: "unit",
		Script: "go test ./...",
		Mutations: []Mutation{{
			ID: "M1", File: "calc.go",
			Old: "\treturn b\n}", New: "\treturn a\n}",
			Note: "b>a 分支",
		}},
		DefHash: "x",
	}

	res, err := p.reviewAll("br-1")
	if err != nil {
		t.Fatal(err)
	}
	if res["verdict"] != "pass" {
		t.Fatalf("严判据应给 pass，实际 %q（notes=%v）", res["verdict"], res["notes"])
	}
	if !res["forward_ok"].(bool) || !res["mutate_ok"].(bool) {
		t.Fatalf("正向与反向都应通过: forward=%v mutate=%v",
			res["forward_ok"], res["mutate_ok"])
	}
	t.Log("✅ 严判据 → pass（机制不会把一切都判成有问题）")
}

// ★★ 判据：**判据实现有 bug** 时正向就报红 ⇒ fail。
func TestReviewFailsOnForwardFailure(t *testing.T) {
	p := newTestPluginForCriteria(t)
	dir := writeRepo(t, false)

	p.ledger.Projects["probe"] = &Project{Name: "probe", RemoteURL: "x"}
	p.ledger.Branches["br-1"] = &Branch{ID: "br-1", ProjectID: "probe", Workdir: dir, Status: "open"}
	p.ledger.Criteria["cr-1"] = &Criteria{
		ID: "cr-1", ProjectID: "probe", Name: "故意失败", Kind: "unit",
		Script:    "false", // 永远失败
		Mutations: []Mutation{{ID: "M1", File: "calc.go", Old: "a > b", New: "a >= b"}},
		DefHash:   "x",
	}

	res, err := p.reviewAll("br-1")
	if err != nil {
		t.Fatal(err)
	}
	if res["verdict"] != "fail" {
		t.Fatalf("正向失败应给 fail，实际 %q", res["verdict"])
	}
	if res["forward_ok"].(bool) {
		t.Fatal("forward_ok 应为 false")
	}
}

// ★★ 判据：**未定义变异规则**时不能给 pass（那正是"信号充值"）。
func TestReviewFlagsMissingMutations(t *testing.T) {
	p := newTestPluginForCriteria(t)
	dir := writeRepo(t, false)

	p.ledger.Projects["probe"] = &Project{Name: "probe", RemoteURL: "x"}
	p.ledger.Branches["br-1"] = &Branch{ID: "br-1", ProjectID: "probe", Name: "feat/x", Workdir: dir, Status: "open"}
	p.ledger.Criteria["cr-1"] = &Criteria{
		ID: "cr-1", ProjectID: "probe", Name: "无变异", Kind: "unit",
		Script: "go test ./...",
		// ★ 刻意不给 Mutations
		DefHash: "x",
	}

	res, err := p.reviewAll("br-1")
	if err != nil {
		t.Fatal(err)
	}
	if res["verdict"] == "pass" {
		t.Fatal("★ 无变异规则却给 pass —— 等于把『没检查』当『检查通过』")
	}
	if res["verdict"] != "pass_with_gaps" {
		t.Errorf("应为 pass_with_gaps，实际 %q", res["verdict"])
	}
	notes, _ := res["notes"].([]string)
	if !strings.Contains(strings.Join(notes, " "), "变异规则") {
		t.Errorf("notes 应指出缺变异规则: %v", notes)
	}
}

// ---------------- 变异执行的安全性 ----------------

// ★★★ 判据：变异**必须恢复原文件**（否则工作目录被污染，后续全乱）。
func TestMutationAlwaysRestoresFile(t *testing.T) {
	p := newTestPluginForCriteria(t)
	dir := writeRepo(t, false)

	orig, _ := os.ReadFile(filepath.Join(dir, "calc.go"))
	c := &Criteria{
		ID: "c", ProjectID: "probe", Name: "n", Kind: "unit",
		Script: "go test ./...",
		Mutations: []Mutation{
			{ID: "M1", File: "calc.go", Old: "\treturn b\n}", New: "\treturn a\n}"},
		},
	}

	_, _, hardErr := p.runCriteriaMutate(c, dir)
	if hardErr {
		t.Fatal("不该有硬错误")
	}
	after, err := os.ReadFile(filepath.Join(dir, "calc.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(orig) {
		t.Fatalf("★ 变异后**未恢复**原文件 —— 工作目录被污染\n前:\n%s\n后:\n%s", orig, after)
	}
}

// ★★ 判据：`old` 出现 **0 次或多次**都必须报硬错误（不能算"咬住"）。
//
// 为何重要：
//
//	· 0 次 ⇒ 变异无效，"判据没报红"是**假警报**（其实判据没问题）
//	· 多次 ⇒ 可能改错地方，"判据报红"是**假通过**（改的不是想改的那处）
func TestMutationRequiresUniqueMatch(t *testing.T) {
	p := newTestPluginForCriteria(t)
	dir := writeRepo(t, false)
	c := &Criteria{ID: "c", ProjectID: "probe", Name: "n", Kind: "unit", Script: "true"}

	// ① old 不存在
	c.Mutations = []Mutation{{ID: "M1", File: "calc.go", Old: "NOT_PRESENT_ANYWHERE", New: "x"}}
	caught, note, hardErr := p.runOneMutation(c, c.Mutations[0], dir)
	if !hardErr {
		t.Fatal("★ old 不存在时报硬错误 —— 否则『变异无效』会被误判成『判据太松』（假警报）")
	}
	if caught {
		t.Error("硬错误时不该算咬住")
	}
	if !strings.Contains(note, "找不到") {
		t.Errorf("说明应指出找不到片段: %s", note)
	}

	// ② old 出现多次
	multi := filepath.Join(dir, "multi.go")
	os.WriteFile(multi, []byte("package calc\n\nvar a = 1\nvar b = 1\n"), 0o644)
	c.Mutations = []Mutation{{ID: "M2", File: "multi.go", Old: "= 1", New: "= 2"}}
	_, note2, hardErr2 := p.runOneMutation(c, c.Mutations[0], dir)
	if !hardErr2 {
		t.Fatal("★ old 出现多次时报硬错误 —— 否则可能改错地方却报『咬住』（假通过）")
	}
	if !strings.Contains(note2, "出现 2 次") {
		t.Errorf("说明应指出出现次数: %s", note2)
	}
}

// ★ 判据：恢复失败/内容不一致 ⇒ 硬错误（**响亮**，不静默继续）。
func TestMutationDetectsRestoreMismatch(t *testing.T) {
	p := newTestPluginForCriteria(t)
	dir := writeRepo(t, false)
	// 只读文件 ⇒ 写入变异会失败 ⇒ 硬错误
	ro := filepath.Join(dir, "ro.go")
	os.WriteFile(ro, []byte("package calc\n\nvar x = 1\n"), 0o444)
	if os.Geteuid() == 0 {
		t.Skip("root 下只读文件仍可写，跳过")
	}
	c := &Criteria{ID: "c", ProjectID: "probe", Name: "n", Kind: "unit", Script: "true"}
	m := Mutation{ID: "M1", File: "ro.go", Old: "= 1", New: "= 2"}
	if _, _, hardErr := p.runOneMutation(c, m, dir); !hardErr {
		t.Fatal("写不进去时报硬错误")
	}
}

// ---------------- 真机端到端（真实 go test） ----------------

// ★★ 判据：**真实 go test** 下，正反两阶段的结果符合预期。
//
// 上面几条用真实 git/go 项目；这一条额外确认「Go 的**测试缓存**不会
// 让变异失效」—— 实测过：变异后若命中缓存，测试根本没跑，
// "报红"就无从谈起。实现里用 `-count=1` 规避。
func TestReviewDoesNotHitGoTestCache(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("环境无 go")
	}
	p := newTestPluginForCriteria(t)
	dir := writeRepo(t, false)

	// 先跑一次让缓存热起来
	code, _ := runShell(dir, "go test -count=1 ./...", 120)
	if code != 0 {
		t.Skipf("前置 go test 未通过（环境问题），跳过: %d", code)
	}

	p.ledger.Projects["probe"] = &Project{Name: "probe", RemoteURL: "x"}
	p.ledger.Branches["br-1"] = &Branch{ID: "br-1", ProjectID: "probe", Name: "feat/x", Workdir: dir, Status: "open"}
	p.ledger.Criteria["cr-1"] = &Criteria{
		ID: "cr-1", ProjectID: "probe", Name: "严判据", Kind: "unit",
		Script: "go test -count=1 ./...",
		Mutations: []Mutation{{
			ID: "M1", File: "calc.go",
			Old: "\treturn b\n}", New: "\treturn a\n}",
			Note: "b>a 分支",
		}},
		DefHash: "x",
	}

	res, err := p.reviewAll("br-1")
	if err != nil {
		t.Fatal(err)
	}
	if !res["mutate_ok"].(bool) {
		ev := ""
		if rep, ok := res["report"].([]map[string]interface{}); ok && len(rep) > 0 {
			ev = str(rep[0], "mutate_evidence")
		}
		t.Fatalf("★ 变异未被咬住 —— 可能是 Go 测试缓存导致变异后测试没真跑\n%s", ev)
	}
	t.Log("✅ 真实 go test 下变异被正确咬住（未受缓存影响）")
}
