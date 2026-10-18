package main

// 构建期检查的判据：同 stage 重复注册必须被**构建期**发现。
//
// ## 为什么是构建期
//
// 用户原话：「本身工具链这个编译器警告就是给开发者看的」。
// 运行期日志给运维看，开发者看不到；构建是开发者必经的一步。
//
// ## 这类错误为什么只能静态查
//
// 插件侧 stageHandlers 是 `map[string]StageHandler`（stage 名为键）⇒
// 同 stage 注册两次，**后者静默覆盖前者**。编译器不报、运行时不报错、
// 内核日志还显示 `2 handlers [qq qq]`（看着正常，实际只派发一次）。
// 2026-10-18 因此把 QQ 的**权限门**覆盖掉了。
//
// ## 判据覆盖的点
//
//  · 同 stage 两次 ⇒ 报出来
//  · 不同 stage 各一次 ⇒ 不误报
//  · **注释里的示例注册不计入**（这个检查器第一版就栽在这，
//    把"当初写错了、留在注释里当教训"的代码算进去，修好之后照样告警）

import (
	"os"
	"path/filepath"
	"testing"
)

func writeGo(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDuplicateStageRegsDetects(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "plugin.go", `
package main
func reg(s *sdk.PluginSDK, p *P) {
	s.RegisterStage(sdk.StageBeforeToolcall, p.auth, sdk.StageScopeGlobal)
	s.RegisterStage(sdk.StageBeforeToolcall, p.status, sdk.StageScopeGlobal)
}
`)
	dups, err := duplicateStageRegs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if dups["StageBeforeToolcall"] != 2 {
		t.Fatalf("★ 没检出 StageBeforeToolcall 重复，实际 %v", dups)
	}
}

func TestDuplicateStageRegsNoFalsePositive(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "plugin.go", `
package main
func reg(s *sdk.PluginSDK, p *P) {
	s.RegisterStage(sdk.StageOnInput, p.auth, sdk.StageScopeGlobal)
	s.RegisterStage(sdk.StageBeforeToolcall, p.before, sdk.StageScopeGlobal)
	s.RegisterStage(sdk.StageAfterOutput, p.after, sdk.StageScopeGlobal)
}
`)
	dups, err := duplicateStageRegs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(dups) != 0 {
		t.Fatalf("★ 不同 stage 各一次不该报，实际 %v", dups)
	}
}

// ★★★ 判据：注释里的示例注册**不计入**。
//
// 这个检查器第一版直接对全文跑正则，于是把插件里
// 「当初写错了、现在留在注释里当教训」的那几行也统计进去，
// 修好之后照样报"注册了 3 次"（实测）。
func TestDuplicateStageRegsIgnoresComments(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "plugin.go", `
package main
func reg(s *sdk.PluginSDK, p *P) {
	//	s.RegisterStage(StageBeforeToolcall, p.beforeToolcall, Global)
	//	s.RegisterStage(StageBeforeToolcall, p.stageStatusWorking, Global)  ← 覆盖了上面
	/*
		s.RegisterStage(StageAfterOutput, p.stageStatusOnline, Global)
	*/
	s.RegisterStage(sdk.StageBeforeToolcall, p.merged, sdk.StageScopeGlobal)
	s.RegisterStage(sdk.StageAfterOutput, p.mergedAfter, sdk.StageScopeGlobal)
}
`)
	dups, err := duplicateStageRegs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(dups) != 0 {
		t.Fatalf("★ 注释里的示例注册被当成真注册，实际 %v —— "+
			"这会让\"已修好\"的插件照样告警（实测踩过）", dups)
	}
}

// ★★★ 判据：测试文件与工具链生成文件不计入。
func TestDuplicateStageRegsSkipsTestAndGenerated(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "plugin.go", `
package main
func reg(s *sdk.PluginSDK, p *P) {
	s.RegisterStage(sdk.StageBeforeToolcall, p.a, sdk.StageScopeGlobal)
}
`)
	// 测试文件里故意造重复（是判据的需要，不是真缺陷）
	writeGo(t, dir, "plugin_test.go", `
package main
func t1(s *sdk.PluginSDK) {
	s.RegisterStage(sdk.StageBeforeToolcall, nil)
	s.RegisterStage(sdk.StageBeforeToolcall, nil)
}
`)
	// 生成文件里的注册是模板展开，不是作者写的
	writeGo(t, dir, "z_proc_gen.go", `
package main
func g(s *sdk.PluginSDK) {
	s.RegisterStage(sdk.StageBeforeToolcall, nil)
	s.RegisterStage(sdk.StageBeforeToolcall, nil)
}
`)
	dups, err := duplicateStageRegs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(dups) != 0 {
		t.Fatalf("★ 测试/生成文件被计入，实际 %v", dups)
	}
}
