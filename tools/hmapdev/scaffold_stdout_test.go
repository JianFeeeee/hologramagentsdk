package main

// 脚手架模板的**协议通道纪律**判据（2026-10-16）。
//
// ## 为什么必须有这条
//
// 子进程插件的 **stdout 是 JSON-RPC 协议通道**，**stderr 才是日志通道**
//（SDK 侧 `log.SetOutput(os.Stderr)`，见 proc_main.go.tmpl）。
//
// 而 `hmapdev init` 生成的 `plugin.go` 原先用 `fmt.Printf` 打日志
// ⇒ 写进 **stdout** ⇒ 内核 readLoop 收到非 JSON 文本：
//
//     [proc] final_probe 收到非法 JSON 帧（21 字节）: invalid character 'i' in literal false
//
// 两个后果叠加，都很难定位：
//   ① 那句日志**丢了**（内核把它当坏帧丢弃）；
//   ② 多了一条看起来像**协议故障**的报错 —— 而真因是"插件自己 printf"。
//
// 实测发现路径：用新版 hmapdev 脚手架了一个插件装进真机内核，
// 启动日志里出现上面那行（其余 20 个插件都没有）。
//
// ## 判据为什么是"源码扫描"而不是"跑一次插件看日志"
//
// 该模板是**字符串常量**（`tmplPluginGo`），不是可直接执行的程序；
// 真正的行为验证在真机 e2e（已做：final_probe 启动日志干净）。
// 这里断言的是**生成物里不存在会写 stdout 的调用** —— 属于
// "某个模式是否出现"那一类，源码扫描在此是**恰当**的。
//（对比：断言"接线/调用顺序"时源码扫描会骗人，见
//  docs/zh/kernel-modularization-design.md §8.6。）

import (
	"strings"
	"testing"
)

// ★★★ 判据：脚手架生成的 plugin.go **不得**往 stdout 打印。
//
// ★ 实现要点：**必须先剥掉注释行**。
// 模板里的注释**故意**提到 `fmt.Printf`（讲这个坑），若直接扫描字符串
// 就会**自己被自己写的注释判红**（实测第一版就是这样，假红）。
// 这就是"源码扫描判据"的固有脆弱性 —— 本仓的规范要求把坑写进注释，
// 而注释里必然出现那些符号名。
//
// ⇒ 剥注释后只扫**代码行**，语义才准确。
func TestScaffoldTemplateDoesNotWriteStdout(t *testing.T) {
	code := stripTemplateComments(tmplPluginGo)
	for _, pat := range []string{"fmt.Printf(", "fmt.Println(", "fmt.Print(", "os.Stdout"} {
		if strings.Contains(code, pat) {
			t.Errorf("★ 脚手架模板的**代码**含 `%s` —— 它写 **stdout**，"+
				"而 stdout 是子进程插件的 **JSON-RPC 协议通道**"+
				"（内核会报「收到非法 JSON 帧」并丢弃该行日志）。"+
				"日志必须用 log.Printf（默认 stderr）。", pat)
		}
	}
}

// stripTemplateComments 剥掉 Go 源码里的 // 行注释（模板只有行注释）。
//
// 只处理 `//`：模板里没有块注释，也不需要处理字符串内的 `//`
// （模板代码里没有含 // 的字符串字面量；若有，这里会误剥 —— 但那会让
//
//	判据**更严**（漏扫），不会假绿，可接受）。
func stripTemplateComments(src string) string {
	var out []string
	for _, ln := range strings.Split(src, "\n") {
		l := strings.TrimSpace(ln)
		if strings.HasPrefix(l, "//") {
			continue
		}
		// 行尾注释也剥掉（`x := 1 // fmt.Printf` 这种）
		if i := strings.Index(ln, "//"); i >= 0 {
			// 只在该 // 之前没有引号时剥（避免剥掉 URL 之类）
			if !strings.Contains(ln[:i], `"`) {
				ln = ln[:i]
			}
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

// ★★★ 判据：模板必须**有**日志（否则上面的判据可以靠"删掉所有日志"通过）。
//
// ★ 这条是"反向钉住"：光禁 stdout 不够 —— 把日志全删也能过。
//
//	模板的价值在于示范**正确的**日志方式，所以必须真的用 log.Printf。
func TestScaffoldTemplateUsesStderrLogging(t *testing.T) {
	if !strings.Contains(tmplPluginGo, "log.Printf") {
		t.Fatal("★ 脚手架模板没有 log.Printf —— 删光日志能通过上一条判据，" +
			"但那就失去了「示范正确做法」的意义")
	}
	if !strings.Contains(tmplPluginGo, `"log"`) {
		t.Fatal("★ 用了 log.Printf 但没 import \"log\" —— 生成物编译不过")
	}
	// 关键位置都要走 log（启动/停止/停止钩子）
	for _, want := range []string{
		`log.Printf("[%s] started", p.name)`,
		`log.Printf("[%s] stopped", p.name)`,
		"stop handler running",
	} {
		if !strings.Contains(tmplPluginGo, want) {
			t.Errorf("★ 模板缺少预期日志: %s", want)
		}
	}
}

// ★★★ 判据：**别的脚手架模板**（Lua）也不得写 stdout。
//
// Lua 插件同理：它走的是同一套子进程协议（main.lua 的 print 会污染帧）。
func TestScaffoldLuaTemplateDoesNotWriteStdout(t *testing.T) {
	src, err := readEmbeddedLuaTemplate()
	if err != nil {
		t.Skipf("读不到 Lua 模板（可能已改名）: %v", err)
	}
	// 正常路径：print(...) 在 Lua 里就是 stdout
	for _, ln := range strings.Split(src, "\n") {
		l := strings.TrimSpace(ln)
		if strings.HasPrefix(l, "--") {
			continue // 注释
		}
		if strings.Contains(l, "print(") && !strings.Contains(l, "io.stderr") {
			t.Errorf("★ Lua 模板含 `print(`（→ stdout，会污染协议帧）: %s", l)
		}
	}
}
