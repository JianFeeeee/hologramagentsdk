package main

import (
	"path/filepath"
	"testing"
)

// ★ 核心判据（用户 2026-10-14 明确要求）：
// **同一个仓库的不同分支，必然是不同的目录** —— 否则两个 agent 会共用工作树，
// 未提交改动互相污染，"独立 clone" 的前提就不成立了。
func TestSameRepoDifferentBranchesGetDistinctDirs(t *testing.T) {
	root := "/data/program"
	cases := []struct{ project, branch string }{
		{"TrueAgent", "feat/a"},
		{"TrueAgent", "feat/b"}, // ← 同仓库、不同分支：必须不同
		{"TrueAgent", "main"},
		{"TrueAgent", "feat/a"},   // ← 与第一条同：必须相同（可重算）
		{"Other", "feat/a"},       // ← 不同仓库、同名分支：必须不同
		{"TrueAgent", "feat/a/b"}, // 嵌套斜杠
		{"TrueAgent", "feat.a"},   // 点（与 feat/a 不同名，应不同目录）
	}
	seen := map[string]string{}
	for _, c := range cases {
		got := branchDir(root, c.project, c.branch)
		key := c.project + "|" + c.branch
		if prev, dup := seen[got]; dup && prev != key {
			t.Errorf("目录冲突：%s 与 %s 都得到 %s", prev, key, got)
		}
		seen[got] = key
	}

	// 明确钉住用户关心的那条
	a := branchDir(root, "TrueAgent", "feat/a")
	b := branchDir(root, "TrueAgent", "feat/b")
	if a == b {
		t.Fatalf("同仓库不同分支得到同一目录：%s", a)
	}
	// 可重算：同输入必同输出（清理孤儿目录依赖这一点）
	if branchDir(root, "TrueAgent", "feat/a") != a {
		t.Fatal("目录名不可重算")
	}
}

// 目录名形态：<hash>-<name>，且 name 可读（含分支信息）。
func TestDirNameShape(t *testing.T) {
	d := dirName("TrueAgent", "feat/cool-thing")
	hash, name, ok := parseDirName(d)
	if !ok {
		t.Fatalf("目录名 %q 不可解析", d)
	}
	if len(hash) != identityHashLen {
		t.Errorf("hash 位数不对: %q", hash)
	}
	if name != "feat_cool-thing" {
		t.Errorf("name 应可读（分支派生）: %q", name)
	}
	// 目录内的路径应落在 program/ 下
	if filepath.Dir(branchDir("/x/program", "P", "b")) != "/x/program" {
		t.Error("目录未落在 program/ 下")
	}
}

// 非法字符被派生掉（目录名必须能安全用于文件系统与通道名）。
func TestDirNameSanitized(t *testing.T) {
	for _, br := range []string{"feat/a b", "中文", "a.b", "a/b"} {
		d := dirName("P", br)
		if !validName(d) {
			t.Errorf("分支 %q 派生的目录名 %q 含非法字符", br, d)
		}
	}
}
