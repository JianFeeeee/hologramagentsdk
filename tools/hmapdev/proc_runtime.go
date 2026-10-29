package main

import (
	"embed"
	"fmt"
	"os"
	"strings"
)

// 子进程插件运行时（外部插件多进程化）。
//
// 模板为何是**真实 .go 源文件** + //go:embed，而不是 raw string：
// 1100+ 行代码塞在字符串里写错只能等生成插件时才炸；作为源文件可被
// gofmt / go vet / go/parser 直接检查（proc_runtime_test.go 的 16 项
// 静态检查就以此为前提）。
//
// 构建从 `-buildmode=c-shared` + CGO_ENABLED=1 变成普通 `go build` +
// CGO_ENABLED=0，交叉编译不再需要目标平台的 C 工具链（§3.1 连带消失项）。
//
// 设计依据：docs/zh/架构迁移评估.md §3、docs/zh/plugin-migration-plan.md Part 3/6

//go:embed templates/proc_main.go.tmpl
//go:embed templates/proc_shm_unix.go.tmpl
//go:embed templates/proc_shm_windows.go.tmpl
var procTemplates embed.FS

// procRuntimeFiles 列出生成到插件目录的运行时文件。
//
// 共享段与事件通知的**传递机制**按平台不同（Unix 继承 fd，
// Windows 命名内核对象），故拆成带 build tag 的两个文件；
// 共享段**布局**与 RPC 逻辑完全平台无关，全在 proc_main 里。
//
// 这正是三套独立 ABI 实现收敛为单一 RPC 实现的效果：
// 平台差异从「整套 stage 下发/写回逻辑各写一份」缩到「三个挂载函数」。
var procRuntimeFiles = []struct {
	tmpl string // 内嵌模板路径
	out  string // 生成到插件目录的文件名
}{
	{"templates/proc_main.go.tmpl", "z_proc_gen.go"},
	{"templates/proc_shm_unix.go.tmpl", "z_proc_shm_unix.go"},
	{"templates/proc_shm_windows.go.tmpl", "z_proc_shm_windows.go"},
}

// procEntryFile 是子进程插件的入口二进制名（与内核 internal/plugin/dynamic.go 的 binEntry 一致）。
//
// 全平台同名：进程边界本身就是 ABI 边界，不存在平台特有的动态库扩展名
// （对比 C ABI 时代的 .so/.dylib/.dll 三套产物 + 三套 ABI 实现）。
const procEntryFile = "plugin.bin"

// luaEntryFile 是 Lua 插件的入口。Lua 走解释器，不经过 Go 编译。
const luaEntryFile = "main.lua"

// procGenFile 是生成的主运行时文件名（兼容旧注释引用）。
// 前缀 z_ 使其在目录列表中排在业务代码之后。
const procGenFile = "z_proc_gen.go"

// sdkImportFallback 是运行时模板里写着的默认 SDK import 路径。
//
// ★ 为什么需要替换（2026-10-29 实测缺陷）：
//
// 运行时模板是**原样拷贝**进插件工程的（不经 text/template 渲染），
// 所以里面的 import 是硬编码的。而 SDK 的 module path 在历史上改过名：
//
//	github.com/JianFeeeee/homeagentsdk       （旧名，本机 store 里的 v1.4.0 仍是它）
//	github.com/JianFeeeee/hologramagentsdk   （新名，当前仓库）
//
// 于是「本机装的 SDK 是旧名 + 模板硬编码新名」时，生成的 z_proc_gen.go
// 引用一个工程 go.mod 里不存在的 module ⇒ 编译报
// "no required module provides package .../sdk"。
//
// 用户按提示 `go get` 只会拉到另一个 module，仍然是错的（两份 SDK 并存）。
//
// 修法：生成运行时文件时，把 import 路径改写为**该工程实际依赖的**
// SDK module 名（从 go.mod 读），模板只作兜底。
const sdkImportFallback = "github.com/JianFeeeee/hologramagentsdk"

// sdkModuleForProject 读工程 go.mod，取它 require/replace 里认得的 SDK module 名。
//
// 读不到就返回兜底常量（保持旧行为，不因识别失败而中断构建）。
func sdkModuleForProject() string {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		return sdkImportFallback
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "hologramagentsdk") && !strings.Contains(line, "homeagentsdk") {
			continue
		}
		// require <mod> <ver> / replace <mod> => <path> / 裸行
		for _, f := range strings.Fields(line) {
			if name, ok := sdkModulePath(f); ok {
				return name
			}
		}
	}
	return sdkImportFallback
}

// generateProcRuntime 把子进程运行时（平台无关主体 + 两个平台挂载实现）
// 写入插件目录，返回清理函数。
func generateProcRuntime() (func(), error) {
	// 清理历史 C ABI 产物：旧版 hmapdev（原名 plugindev）生成过这两个文件，残留下来会与
	// 本模板的 main 冲突。无需人工清理就能从旧版升级。
	for _, stale := range []string{"z_bridge_gen.go", "z_entry.c"} {
		os.Remove(stale)
	}

	var written []string
	cleanup := func() {
		for _, f := range written {
			os.Remove(f)
		}
	}

	for _, rf := range procRuntimeFiles {
		data, err := procTemplates.ReadFile(rf.tmpl)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("读取内嵌模板 %s: %w", rf.tmpl, err)
		}
		// 按本工程实际依赖的 SDK module 改写 import（模板里的路径只作兜底）。
		if mod := sdkModuleForProject(); mod != "" && mod != sdkImportFallback {
			data = []byte(strings.ReplaceAll(string(data), sdkImportFallback+"/sdk", mod+"/sdk"))
		}
		if err := os.WriteFile(rf.out, data, 0644); err != nil {
			cleanup()
			return nil, fmt.Errorf("写入 %s: %w", rf.out, err)
		}
		written = append(written, rf.out)
	}
	return cleanup, nil
}

// isProcEntry 已删除：Go 插件一律产出 plugin.bin，不再看 plg.json 的 entry 值。
//
// 为何忽略 entry：17 个存量插件的 plg.json 都写着 "plugin.so"。若把 entry 当作
// 通道开关，迁移就得改 17 个文件——而「外部插件零改动」是本次迁移的硬约束。
// entry 现在只用于区分 Lua（main.lua）与 Go 插件。
