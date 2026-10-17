package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 用**真实 git 输出**验证冲突解析（从 git 2.53 实测抓取）。
func TestParseConflictFilesRealOutput(t *testing.T) {
	// 这是 git merge-tree --write-tree 冲突时的**原样输出**
	const real = `82d5993c292e11020e08a83074413e7cc6e2d5d1
100644 df967b96a579e45a18b8251732d16804b2e56a55 1	f.txt
100644 f70f10e4db19068f79bc43844b49f3eece45c4e8 2	f.txt
100644 223b7836fb19fdf64ba2d3cd6173c6a283141f78 3	f.txt

Auto-merging f.txt
CONFLICT (content): Merge conflict in f.txt
`
	got := parseConflictFiles(real)
	if len(got) != 1 || got[0] != "f.txt" {
		t.Fatalf("应从真实输出解出 [f.txt]，实际 %v", got)
	}
	if !isMergeTreeConflict(real) {
		t.Fatal("真实冲突输出应被识别为冲突")
	}
}

// 无冲突时不该误判。
func TestNoConflictOutputNotMisjudged(t *testing.T) {
	const clean = `3d501af9404b81d5e9d7fe6254d082ff59108312
`
	if isMergeTreeConflict(clean) {
		t.Fatal("干净合并的输出不该被判为冲突")
	}
	if got := parseConflictFiles(clean); len(got) != 0 {
		t.Fatalf("无冲突时不该解出文件: %v", got)
	}
}

// 多文件冲突要全部解出且稳定排序。
func TestParseMultipleConflictsStable(t *testing.T) {
	const multi = `abc123
100644 aaa 1	z.txt
100644 bbb 2	z.txt
100644 ccc 3	z.txt
100644 ddd 1	a.txt
100644 eee 2	a.txt
100644 fff 3	a.txt
CONFLICT (content): Merge conflict in z.txt
CONFLICT (content): Merge conflict in a.txt
`
	got := parseConflictFiles(multi)
	if len(got) != 2 || got[0] != "a.txt" || got[1] != "z.txt" {
		t.Fatalf("应解出 [a.txt z.txt] 且已排序，实际 %v", got)
	}
	// 重复出现（阶段行 + CONFLICT 行）不该重复计数
	if len(got) != len(uniqueStrings(got)) {
		t.Error("同一文件出现多次时应去重")
	}
}

func uniqueStrings(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// ★★ 决定性判据：`checkConflict` 在**真实 git 仓库**上正确判定。
//
// 上面几条测的是 parser（纯函数）。这条跑真实 git —— 因为 parser 对
// 不代表「预检对」：命令拼错、退出码语义搞反、目标分支取错，
// 都只有真跑才暴露。
//
// 场景（与并行开发的真实情形一致）：
//
//	main ← base
//	├─ feat/a 改 f.txt  →  合进 main（main 前进）
//	└─ feat/b 改 f.txt  →  此时预检必冲突
//
// 以及对照组：feat/c 改的是**另一个文件** → 预检应通过。
func TestCheckConflictOnRealGitRepo(t *testing.T) {
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		if _, err2 := exec.LookPath("git"); err2 != nil {
			t.Skip("环境无 git")
		}
	}

	base := t.TempDir()
	repo := filepath.Join(base, "w")

	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v 失败: %v\n%s", args, err, out)
		}
		return string(out)
	}

	// 建仓 + 基线
	// ★ 显式指定初始分支为 main：git 默认分支名随版本/配置而变
	//（本机是 master），不指定的话下面的 "main" 会找不到。
	run(base, "init", "-q", "-b", "main", repo)
	run(repo, "config", "user.email", "t@t")
	run(repo, "config", "user.name", "t")
	os.WriteFile(filepath.Join(repo, "f.txt"), []byte("base\n"), 0o600)
	run(repo, "add", "-A")
	run(repo, "commit", "-qm", "base")

	// feat/a 改 f.txt
	run(repo, "checkout", "-q", "-b", "feat/a")
	os.WriteFile(filepath.Join(repo, "f.txt"), []byte("A\n"), 0o600)
	run(repo, "commit", "-qam", "A")

	// feat/b 也改 f.txt（与 a 同一个文件 → 冲突）
	run(repo, "checkout", "-q", "main")
	run(repo, "checkout", "-q", "-b", "feat/b")
	os.WriteFile(filepath.Join(repo, "f.txt"), []byte("B\n"), 0o600)
	run(repo, "commit", "-qam", "B")

	// feat/c 改另一个文件（不冲突）
	run(repo, "checkout", "-q", "main")
	run(repo, "checkout", "-q", "-b", "feat/c")
	os.WriteFile(filepath.Join(repo, "g.txt"), []byte("C\n"), 0o600)
	run(repo, "add", "-A")
	run(repo, "commit", "-qm", "C")

	p := &Plugin{ledger: &Ledger{}}

	// ① main 还没前进 ⇒ feat/b 可干净合并
	run(repo, "checkout", "-q", "feat/b")
	rep := p.checkConflict(repo, "main")
	if rep.Skipped {
		t.Fatalf("不该跳过: %s", rep.Detail)
	}
	if !rep.Mergeable {
		t.Fatalf("main 未前进时 feat/b 应可合并，实际: %s", rep.Detail)
	}
	if rep.HeadSHA == "" || rep.BaseSHA == "" {
		t.Error("应记录 head_sha 与 base_sha（审计与重放要用）")
	}

	// ② 把 feat/a 合进 main（main 前进）⇒ feat/b 必冲突
	run(repo, "checkout", "-q", "main")
	run(repo, "merge", "-q", "--no-ff", "feat/a", "-m", "merge a")
	run(repo, "checkout", "-q", "feat/b")
	rep = p.checkConflict(repo, "main")
	if rep.Skipped {
		t.Fatalf("不该跳过: %s", rep.Detail)
	}
	if rep.Mergeable {
		t.Fatal("★ main 已含 feat/a 的改动、feat/b 改了同一文件 ⇒ 必须判为冲突")
	}
	if len(rep.Conflicts) == 0 || rep.Conflicts[0] != "f.txt" {
		t.Fatalf("应指出冲突文件 f.txt，实际 %v（detail=%s）", rep.Conflicts, rep.Detail)
	}

	// ③ 对照：feat/c 改的是另一文件 ⇒ 仍可合并
	run(repo, "checkout", "-q", "feat/c")
	rep = p.checkConflict(repo, "main")
	if rep.Skipped {
		t.Fatalf("不该跳过: %s", rep.Detail)
	}
	if !rep.Mergeable {
		t.Fatalf("feat/c 改了无关文件，应可合并，实际: %s", rep.Detail)
	}

	// ④ ★ 关键：预检**不能改动工作区**
	//（否则它就成了"会破坏开发状态"的副作用操作）
	cur := strings.TrimSpace(run(repo, "rev-parse", "--abbrev-ref", "HEAD"))
	if cur != "feat/c" {
		t.Fatalf("预检把 HEAD 改到了 %s —— 它绝不该动工作区", cur)
	}
	if _, err := os.Stat(filepath.Join(repo, "MERGE_HEAD")); err == nil {
		t.Fatal("预检留下了 MERGE_HEAD —— 会污染后续提交")
	}
}

// ★★ 判据：**每一条 Skipped 路径都不得声称可合并**。
//
// 把"没检查"当成"检查通过"是最危险的静默降级 —— 调用方会以为安全。
//
// 为何要**逐路径**覆盖：checkConflict 有 3 条 Skipped 出口
// （取 HEAD 失败 / 目标分支不存在 / merge-tree 不可用），
// 初版只测了第 2 条，于是把第 1 条的 `rep.Mergeable` 改成 true 的变异
// **不报红** —— 判据覆盖不全，等于给其余两条留了洞。
func TestEverySkippedPathRefusesMergeable(t *testing.T) {
	mkRepo := func(t *testing.T, withBranch bool) string {
		repo := t.TempDir()
		git := func(args ...string) {
			cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
				"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
			cmd.Run()
		}
		if out, err := exec.Command("git", "init", "-q", "-b", "main", repo).CombinedOutput(); err != nil {
			t.Skipf("git init 失败: %v %s", err, out)
		}
		git("config", "user.email", "t@t")
		git("config", "user.name", "t")
		os.WriteFile(filepath.Join(repo, "a.txt"), []byte("x\n"), 0o600)
		git("add", "-A")
		git("commit", "-qm", "init")
		if withBranch {
			git("checkout", "-q", "-b", "other")
		}
		return repo
	}

	p := &Plugin{ledger: &Ledger{}}

	t.Run("目标分支不存在", func(t *testing.T) {
		rep := p.checkConflict(mkRepo(t, false), "no-such-branch")
		assertSkippedNotMergeable(t, rep)
	})

	t.Run("不是 git 仓库", func(t *testing.T) {
		rep := p.checkConflict(t.TempDir(), "main")
		assertSkippedNotMergeable(t, rep)
	})

	t.Run("工作目录不存在", func(t *testing.T) {
		rep := p.checkConflict(filepath.Join(t.TempDir(), "nope"), "main")
		assertSkippedNotMergeable(t, rep)
	})
}

// assertSkippedNotMergeable 是上面三条路径共用的断言。
func assertSkippedNotMergeable(t *testing.T, rep ConflictReport) {
	t.Helper()
	if !rep.Skipped {
		t.Fatalf("应标记 Skipped（detail=%s）", rep.Detail)
	}
	if rep.Mergeable {
		t.Fatal("★ Skipped 时绝不能声称 Mergeable —— 那是把「没检查」当「检查通过」")
	}
	if !strings.Contains(rep.Detail, "不等于") {
		t.Errorf("说明应明确「跳过≠无冲突」: %s", rep.Detail)
	}
}

// ★★ 判据：`captureBaseSHA` 真的把 sha 写进台账，且**只写第一次**。
//
// "只写第一次"很重要：重开同一分支时若覆盖基点，"这条线从哪出发"
// 就永久丢失 —— 冲突判定与事后重放都失去依据。
func TestCaptureBaseSHARecordsOnceOnly(t *testing.T) {
	repo := t.TempDir()
	gitIn := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		cmd.Run()
	}
	if out, err := exec.Command("git", "init", "-q", "-b", "main", repo).CombinedOutput(); err != nil {
		t.Skipf("git init 失败: %v %s", err, out)
	}
	gitIn("config", "user.email", "t@t")
	gitIn("config", "user.name", "t")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("1\n"), 0o600)
	gitIn("add", "-A")
	gitIn("commit", "-qm", "c1")

	p := &Plugin{ledger: &Ledger{Branches: map[string]*Branch{
		"br-x": {ID: "br-x", Name: "feat/x", Status: "open"},
	}}}

	sha1, err := p.captureBaseSHA("br-x", repo)
	if err != nil {
		t.Fatal(err)
	}
	if sha1 == "" || len(sha1) < 7 {
		t.Fatalf("应拿到 sha，实际 %q", sha1)
	}
	if p.ledger.Branches["br-x"].BaseSHA != sha1 {
		t.Fatal("base_sha 未写进台账")
	}

	// 再提交一次（HEAD 前进），再捕获 → 仍应返回**第一次**的 sha
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("2\n"), 0o600)
	gitIn("add", "-A")
	gitIn("commit", "-qm", "c2")

	sha2, err := p.captureBaseSHA("br-x", repo)
	if err != nil {
		t.Fatal(err)
	}
	if sha2 != sha1 {
		t.Fatalf("基点不该被后续提交覆盖：第一次 %s，第二次 %s", shortSHA(sha1), shortSHA(sha2))
	}
}

// ★ 判据：拿不到 sha 时**如实报错**，不静默返回空串。
//
// 空 base_sha 会让后续冲突判定失去依据，而"没记上"与"记了个空的"
// 在台账里长得一样 —— 所以必须报错。
func TestCaptureBaseSHAFailsLoudlyOnBadRepo(t *testing.T) {
	p := &Plugin{ledger: &Ledger{Branches: map[string]*Branch{"br-y": {ID: "br-y"}}}}
	if _, err := p.captureBaseSHA("br-y", filepath.Join(t.TempDir(), "not-a-repo")); err == nil {
		t.Fatal("非 git 目录应报错，而不是返回空 sha")
	}
}

// ★ 判据：**仅 ls-tree 阶段行**（无 "CONFLICT" 行）也要能解出文件名。
//
// 某些冲突类型（如 rename/rename）不打印 "CONFLICT ... in <path>"，
// 只有 ls-tree 的三个阶段行。少了这条解析路径就会漏报冲突文件 ——
// 而"漏报"比"多报"危险：调用方以为没有冲突。
func TestParseConflictFromLsTreeOnly(t *testing.T) {
	// 只有阶段行、没有 "CONFLICT ... in" 行（某些冲突类型如此）
	const only = `abc123
100644 aaa 1	dir/x.go
100644 bbb 2	dir/x.go
100644 ccc 3	dir/x.go
`
	got := parseConflictFiles(only)
	if len(got) != 1 || got[0] != "dir/x.go" {
		t.Fatalf("仅 ls-tree 形态也应解出 [dir/x.go]，实际 %v", got)
	}
}
