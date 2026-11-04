package yaegi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMergePackageSourceDedupsImports 钉住：多文件重复 import 必须去重。
//
// 逐文件 Eval 时第二个文件的 `import "fmt"` 会被 yaegi 当成同一作用域
// 的重复声明，报 `fmt/_.go redeclared in this block` —— 多文件插件
// 全都调试不了，且只在多文件时出现，单文件工程看不出来。
func TestMergePackageSourceDedupsImports(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.go", `package main

import "fmt"

func A() { fmt.Println("a") }
`)
	writeFile(t, dir, "b.go", `package main

import "fmt"

func B() { fmt.Println("b") }
`)

	d := &YaegiDebugger{Dir: dir}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	src, err := d.mergePackageSource(entries)
	if err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if n := strings.Count(src, `"fmt"`); n != 1 {
		t.Fatalf(`"fmt" 出现 %d 次，应去重为 1 次`, n)
	}
	if !strings.Contains(src, "func A()") || !strings.Contains(src, "func B()") {
		t.Fatal("合并后丢失了函数声明")
	}
}

// TestMergePackageSourceKeepsSDKImport 钉住：SDK import 必须**保留**。
//
// 曾经的方案是剔除 SDK import、把 `sdk.Xxx` 重写成裸名 `Xxx`，靠
// i.Use(sdkExports) 的裸名导出来解析。实测走不通：
//   - 函数签名的类型位置会报 `undefined: PluginSDK`
//   - 插件自己的 `type Plugin struct` 与 SDK 的 Plugin 接口撞名
//
// 现在的方案是**保留 import**，由 buildSDKGopathLink 搭出 GOPATH 布局的
// 软链让 yaegi 能按 import 路径找到 SDK 源码。这样类型解析与真实编译一致。
func TestMergePackageSourceKeepsSDKImport(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.go", `package main

import sdk "github.com/JianFeeeee/hologramagentsdk/sdk"

func A(s *sdk.PluginSDK) {}
`)

	d := &YaegiDebugger{Dir: dir}
	entries, _ := os.ReadDir(dir)
	src, err := d.mergePackageSource(entries)
	if err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if !strings.Contains(src, "hologramagentsdk/sdk") {
		t.Fatal("SDK import 被剔除了 —— 类型会解析不到")
	}
	if !strings.Contains(src, "sdk.PluginSDK") {
		t.Fatal("sdk. 限定名被改写了 —— 应保持原样")
	}
}

// TestMergePackageSourceSkipsImportDecls 钉住：import 声明不能落进 body。
//
// 不跳过会生成 `package ... import ... func ... import ...`，
// Go 语法要求 import 必须在所有其它声明之前，报
// `imports must appear before other declarations`。
func TestMergePackageSourceSkipsImportDecls(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.go", `package main

import "fmt"

func A() { fmt.Println("x") }
`)
	d := &YaegiDebugger{Dir: dir}
	entries, _ := os.ReadDir(dir)
	src, err := d.mergePackageSource(entries)
	if err != nil {
		t.Fatal(err)
	}
	// import 只应出现在文件开头那一个块里
	idx := strings.Index(src, "func A()")
	if idx < 0 {
		t.Fatal("缺少 func A")
	}
	if strings.Contains(src[idx:], "import") {
		t.Fatal("函数声明之后又出现了 import —— 语法非法")
	}
}

// TestFileMatchesCurrentPlatform 钉住平台过滤。
//
// proc_unix.go / proc_windows.go 各有一个 setSysProcAttr，
// 不按平台过滤会报 redeclared。
func TestFileMatchesCurrentPlatform(t *testing.T) {
	if !fileMatchesCurrentPlatform("proc_unix.go") {
		// 在 windows 上应为 false，其它平台为 true
		if !strings.Contains(strings.ToLower(osName()), "windows") {
			t.Fatal("非 windows 平台上 proc_unix.go 应参与合并")
		}
	}
	if fileMatchesCurrentPlatform("proc_windows.go") && !strings.Contains(strings.ToLower(osName()), "windows") {
		t.Fatal("非 windows 平台上 proc_windows.go 不应参与合并")
	}
}

func osName() string {
	if v := os.Getenv("GOOS"); v != "" {
		return v
	}
	return "linux"
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
