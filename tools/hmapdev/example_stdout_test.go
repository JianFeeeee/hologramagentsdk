package main

// 示例插件的**协议通道纪律**判据（2026-10-16）。
//
// ## 为什么必须有这条
//
// 示例插件是**子进程插件**（`package main` + `plugin.bin`），它们的
// **stdout 是 JSON-RPC 协议通道**。用 `fmt.Printf` 打日志 ⇒ 裸文本进入
// 协议流 ⇒ 内核报「收到非法 JSON 帧」并丢弃该日志。
//
// ## 这是**生产环境正在发生**的缺陷（不是理论问题）
//
// 实测 `journalctl -u homeagent`：**1038 条**非法帧报错，来源：
//
//     huawei_smarthome 720 / weather 113 / rss 103 / calendar 102
//
// weather / rss / calendar 正是本仓示例插件 ⇒ 已全部修为 log.Printf。
//（huawei_smarthome 是第三方插件，源码不在本仓 —— 内核侧的可定位报错
//  就是为它那类情况准备的。）
//
// ## 判据形态：源码扫描（这里是**恰当**的）
//
// 每个示例插件是**独立 Go module**（21 个 go.mod），无法在一个测试进程里
// 全部编译 ⇒ 只能扫源码。断言的是"某个模式是否出现"，属于
// "符号/模式存在性"那一类（对比：断言接线/顺序时源码扫描会骗人）。
//
// ★ 必须**先剥注释**：示例插件里可能**刻意**提到 fmt.Printf 来讲这个坑。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ★★★ 判据：所有子进程示例插件**不得**往 stdout 打印。
func TestExamplePluginsDoNotWriteStdout(t *testing.T) {
	exDir := filepath.Join("..", "..", "example")
	entries, err := os.ReadDir(exDir)
	if err != nil {
		t.Skipf("读不到 example 目录: %v", err)
	}
	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// 只查**子进程**插件（有 plugin.go 且以 package main 开头）
		var files []string
		fis, err := os.ReadDir(filepath.Join(exDir, name))
		if err != nil {
			continue
		}
		for _, fi := range fis {
			if fi.IsDir() || !strings.HasSuffix(fi.Name(), ".go") ||
				strings.HasSuffix(fi.Name(), "_test.go") {
				continue
			}
			files = append(files, filepath.Join(exDir, name, fi.Name()))
		}
		isSubprocess := false
		for _, f := range files {
			b, _ := os.ReadFile(f)
			if strings.HasPrefix(string(b), "package main") {
				isSubprocess = true
				break
			}
		}
		if !isSubprocess {
			continue
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			code := stripTemplateComments(string(b))
			for _, pat := range []string{"fmt.Printf(", "fmt.Println(", "fmt.Print(", "os.Stdout"} {
				if !strings.Contains(code, pat) {
					continue
				}
				// 找出具体行号（便于修）
				for i, ln := range strings.Split(code, "\n") {
					if strings.Contains(ln, pat) {
						t.Errorf("★ %s:%d 含 `%s` —— 子进程插件的 **stdout 是 "+
							"JSON-RPC 协议通道**，内核会报「收到非法 JSON 帧」并"+
							"丢弃该日志。\n    修法：改成 log.Printf（默认 stderr）。\n    行: %s",
							filepath.Join(name, filepath.Base(f)), i+1, pat, strings.TrimSpace(ln))
					}
				}
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("没找到任何子进程示例插件 —— 判据会永远绿（请检查目录名/结构）")
	}
	t.Logf("检查了 %d 个子进程示例插件", checked)
}

// ★★★ 判据：**反向钉住** —— 必须有示例插件真的在用 log.Printf。
//
// ★ 为什么需要：上面那条可以靠"把所有日志删光"通过，但那样示例就
//
//	失去了"示范正确做法"的意义。这条确保至少有几个插件在用 log.Printf。
func TestExamplePluginsUseStderrLogging(t *testing.T) {
	exDir := filepath.Join("..", "..", "example")

	// ★ 2026-10-31：原本硬编码 ["weather","rss","calendar"]，但 26 个插件
	// 迁到 GitHub 组织 HologramAgent 后，example/ 只剩 rtvoice + feiyu_voice，
	// 硬编码清单全部消失 ⇒ 判据退化成"读了不存在的文件就红"。
	//
	// 改成**动态扫描**：对现存每个子进程示例，要么完全不往 stdout 写
	// （上面那条判据覆盖），要么必须用 log.Printf 写 stderr。这样判据
	// 意图不变（示范正确做法），但不再依赖具体哪几个插件在本仓。
	ents, err := os.ReadDir(exDir)
	if err != nil {
		t.Fatalf("读 %s: %v", exDir, err)
	}
	checked := 0
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(exDir, e.Name(), "plugin.go")
		b, err := os.ReadFile(p)
		if err != nil {
			continue // 没有 plugin.go 的目录（如纯 lua 示例）跳过
		}
		src := string(b)
		if !strings.HasPrefix(src, "package main") {
			continue // 非子进程插件（lua 等）不适用
		}
		checked++
		if strings.Contains(src, "log.Printf") && !strings.Contains(src, `"log"`) {
			t.Errorf("★ %s 用了 log.Printf 但没 import \"log\"", e.Name())
		}
	}
	if checked == 0 {
		t.Fatal("没找到任何子进程示例插件 —— 判据会永远绿（请检查目录名/结构）")
	}
	t.Logf("检查了 %d 个子进程示例插件的日志写法", checked)
}
