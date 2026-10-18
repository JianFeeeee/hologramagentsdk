package main

// 打包期校验：`auto_start=false` 的插件**必须**带 skill。
//
// ## 为什么这条是必须的，而不是"建议"
//
// 逻辑链是闭合的：
//
//	auto_start=false ⇒ 内核启动时不拉起 ⇒ 工具不在初始清单里
//	⇒ 模型不知道它存在 ⇒ 永远不会被调用、也永远不会被拉起
//
// 也就是说：缺 skill 的按需插件**等价于没装**，但比没装更糟 ——
// 它占着磁盘和安装记录，让人以为它可用（静默降级）。
//
// 所以校验放在**打包期**：比用户装完再困惑早得多。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ★★★ 判据：有真实 SKILL.md 才算"带 skill"。
func TestHasSkillDirRequiresRealSkillMd(t *testing.T) {
	// ① 完全没有 skill/ 目录
	root1 := t.TempDir()
	if hasSkillDir(root1) {
		t.Fatal("★ 没有 skill/ 目录却判定为有 skill")
	}

	// ② 有空 skill/ 目录
	root2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root2, "skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if hasSkillDir(root2) {
		t.Fatal("★ 空 skill/ 目录被判为有 skill —— 校验形同虚设")
	}

	// ③ 只有 .gitkeep 的脚手架目录
	root3 := t.TempDir()
	sk3 := filepath.Join(root3, "skill")
	if err := os.MkdirAll(sk3, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sk3, ".gitkeep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if hasSkillDir(root3) {
		t.Fatal("★ 只有 .gitkeep 的脚手架目录被判为有 skill")
	}

	// ④ 有目录但没有 SKILL.md（空壳）
	root4 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root4, "skill", "myskill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root4, "skill", "myskill", "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hasSkillDir(root4) {
		t.Fatal("★ 只有目录没有 SKILL.md 却被判为有 skill —— 注册到 skillmgr 也是空壳")
	}

	// ⑤ 真有一个 skill
	root5 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root5, "skill", "myskill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root5, "skill", "myskill", "SKILL.md"), []byte("# x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasSkillDir(root5) {
		t.Fatal("★ 有真实 SKILL.md 却判定为没有 skill")
	}
}

// ★★★ 判据：cmd_build 里必须真的拦住 auto_start=false 且无 skill 的情况。
func TestBuildRejectsOnDemandWithoutSkill(t *testing.T) {
	b, err := os.ReadFile("cmd_build.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)

	i := strings.Index(src, `if plg.AutoStart != nil && !*plg.AutoStart {`)
	if i < 0 {
		t.Fatal("找不到 auto_start 的写入分支")
	}
	body := src[i:]
	if j := strings.Index(body[1:], "\n\t}\n"); j > 0 {
		body = body[:j+3]
	}
	if !strings.Contains(body, "hasSkillDir") {
		t.Fatal("★ auto_start=false 的分支没有校验 skill —— " +
			"按需插件缺 skill 等价于没装（agent 不知道它存在）")
	}
	if !strings.Contains(body, "os.Exit(1)") {
		t.Fatal("★ 校验没有中断打包（os.Exit(1)）—— 报错了还继续产出不可用包")
	}
}
