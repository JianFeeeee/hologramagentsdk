package main

// 示例插件的 go.mod 判据（2026-10-17 CI 事故）。
//
// ## 事故
//
// 我在一个提交里用 `git add -A` 把本机 `go build` **自动改写**过的
// go.mod 一并提交了：
//
//	require github.com/JianFeeeee/homeagentsdk v1.4.0
//	replace github.com/JianFeeeee/homeagentsdk => /root/.homeagent/hmapdev/sdk/v1.4.0
//	                                              ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^ 本机绝对路径
//
// 后果：CI（干净环境）上 commondev / editdoc 构建**必然失败**
//
//	error: 项目声明 sdk=1.4.0，但本地 SDK 存储里没有这个版本
//
// 而本机能过（那个目录真的存在）⇒ 本地全绿掩盖了问题，PR 卡在 CI 才暴露。
//
// ## 判据
//
//  1. 任何示例的 go.mod 都**不得**含本机绝对路径
//  2. replace 必须是相对路径（`../../`），这是 SDK 仓内示例的约定
//  3. plg.json 不得声明 `sdk` 版本 —— 一声明就要求本地 SDK store 有该版本，
//     而示例的正确依赖表达是 go.mod 的 v0.0.0 + replace ../../
//
// 这三条合起来保证「CI 干净环境能构建全部示例」。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ★★★ 判据：示例 go.mod 不得含本机绝对路径。
func TestExampleGoModHasNoAbsoluteReplace(t *testing.T) {
	for _, gm := range findExampleGoMods(t) {
		b, err := os.ReadFile(gm)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, bad := range []string{"/root/", "/home/", "/Users/", "C:\\"} {
			if strings.Contains(src, bad) {
				rel := mustRel(t, gm)
				t.Fatalf("★ %s 含本机绝对路径 %q ——\n"+
					"  CI（干净环境）会因此构建失败，而本机因为有那个目录反而通过。\n"+
					"  示例必须用相对路径：replace ... => ../../", rel, bad)
			}
		}
	}
}

// ★★★ 判据：示例的 replace 指向 SDK 仓根（相对路径）。
func TestExampleGoModUsesRelativeSDKReplace(t *testing.T) {
	reReplace := regexp.MustCompile(`(?m)^replace\s+\S*homeagentsdk\S*\s*=>\s*(\S+)`)
	for _, gm := range findExampleGoMods(t) {
		b, _ := os.ReadFile(gm)
		m := reReplace.FindStringSubmatch(string(b))
		if m == nil {
			continue // 没有 replace 也算合法（某些示例可能不用）
		}
		tgt := m[1]
		if filepath.IsAbs(tgt) {
			t.Fatalf("★ %s 的 replace 是绝对路径 %q（必须相对）", mustRel(t, gm), tgt)
		}
		if tgt != "../../" {
			t.Errorf("%s 的 replace 是 %q，本仓约定为 ../../（与 ai_image/calendar 等一致）",
				mustRel(t, gm), tgt)
		}
	}
}

// ★★★ 判据：示例 plg.json 不得声明 sdk 版本。
//
// 为什么：一声明 `"sdk": "1.4.0"`，hmapdev 就会去**本地 SDK store** 找该版本，
// 找不到直接拒绝构建。而示例的正确依赖表达是 go.mod 的 v0.0.0 + replace ../../。
// 全仓 20+ 个示例都不声明，只有被我改过的两个声明了 ⇒ CI 只在这两个上红。
func TestExamplePlgJsonDoesNotPinSDKVersion(t *testing.T) {
	root := repoRootForTest(t)
	matches, _ := filepath.Glob(filepath.Join(root, "example", "*", "plg.json"))
	reSDK := regexp.MustCompile(`"sdk"\s*:`)
	for _, p := range matches {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if reSDK.Match(b) {
			rel := mustRel(t, p)
			t.Fatalf("★ %s 声明了 sdk 版本 ——\n"+
				"  这会让 hmapdev 去本地 SDK store 找该版本，干净 CI 上必然失败。\n"+
				"  示例请用 go.mod 的 `require ... v0.0.0` + `replace ... => ../../`", rel)
		}
	}
}

func findExampleGoMods(t *testing.T) []string {
	t.Helper()
	root := repoRootForTest(t)
	matches, err := filepath.Glob(filepath.Join(root, "example", "*", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("没找到任何示例 go.mod ⇒ 判据会假绿，路径写错了？")
	}
	return matches
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// tools/hmapdev → 上两级
	return filepath.Dir(filepath.Dir(wd))
}

func mustRel(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.Rel(repoRootForTest(t), p)
	if err != nil {
		return p
	}
	return r
}
