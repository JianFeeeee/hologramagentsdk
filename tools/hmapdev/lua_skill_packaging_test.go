package main

// Lua 插件的打包路径也必须收集 skill。
//
// ## 缺陷（2026-10-18 实测）
//
// `buildTarget` 里 Go 路径会调 collectRawFiles + collectSkillFiles，
// 而 **Lua 路径是另一段代码**，直接 `createHmap(out, pkgFiles)` ——
// 完全不走 rawfile/skill 收集。
//
// 实测：给 luademo 加了 skill/luademo-tools/SKILL.md 后，
// 打出的 `linux_amd64_lua_demo_lua.hmap` 里 skill 条目数为 **0**。
//
// 这对 Lua 插件伤害更大：Go 插件至少有 RegisterTool 的 Description
// 作为文档位，而 Lua 插件向 agent 说明自己的**唯一**途径就是 skill。
// 缺了它，一个功能完整的 Lua 插件对模型来说是隐形的。
//
// ## 判据
//
// 断言 Lua 分支里出现 collectRawFiles + collectSkillFiles，
// 并且最终走 createHmapWithRaw（而不是只包 pkgFiles 的 createHmap）。

import (
	"os"
	"strings"
	"testing"
)

// ★★★ 判据：Lua 打包分支必须收集 rawfile 与 skill。
func TestLuaPackagingCollectsSkillAndRawfiles(t *testing.T) {
	b, err := os.ReadFile("cmd_build.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)

	// 定位 Lua 分支：`if target == "lua" {` 之后到 `// Resolve build config`
	i := strings.Index(src, `if target == "lua" {`)
	if i < 0 {
		t.Fatal("找不到 Lua 打包分支")
	}
	// ★ 结束边界必须是 Lua 分支自己的 `return` + `}`，不能用
	// "// Resolve build config" —— 实测那个注释在文件里**出现多次**
	// （Go 路径里也有），用它截取会把 Go 分支一起包进来，
	// 于是"Lua 分支没传 skills"这种变异测不出来。
	endMark := "\t\tfmt.Printf(\"  packaged %s\\n\", hmapName)"
	j := strings.Index(src[i:], "\t\treturn\n\t}")
	if j < 0 {
		t.Fatal("找不到 Lua 分支的结束位置")
	}
	// 更稳妥：截到该分支的第一个 `\n\t}\n\n\t// Resolve` 之前。
	lua := src[i : i+j]
	_ = endMark

	// ★★ 只查符号存在是不够的（实测：把 createHmapWithRaw 换回 createHmap，
	// 判据依然通过 —— 因为那行 `createHmapWithRaw` 的字符串还在注释里）。
	// 必须断言：收集到的 raws/skills 确实**传给了** createHmapWithRaw。
	for _, want := range []struct{ sym, why string }{
		{"collectRawFiles", "Lua 插件带不上 rawfile（随包资源全部丢失）"},
		{"collectSkillFiles", "Lua 插件带不上 skill（它对模型来说是隐形的）"},
	} {
		if !strings.Contains(lua, want.sym) {
			t.Fatalf("★ Lua 打包分支缺少 %s —— %s", want.sym, want.why)
		}
	}

	// 必须是**代码行**在调用 createHmapWithRaw，且参数里带上了 skills。
	// 逐行查（跳过注释），避免"注释里有、代码里没有"。
	callOK := false
	for _, ln := range strings.Split(lua, "\n") {
		code := strings.TrimSpace(ln)
		if strings.HasPrefix(code, "//") {
			continue
		}
		if strings.Contains(code, "createHmapWithRaw(") && strings.Contains(code, "skills") {
			callOK = true
			break
		}
	}
	if !callOK {
		t.Fatal("★ Lua 打包没有把 skill 传给 createHmapWithRaw —— " +
			"收集到了却没写进包，Lua 插件对模型来说仍是隐形的")
	}
}
