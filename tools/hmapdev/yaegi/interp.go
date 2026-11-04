package yaegi

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/JianFeeeee/hologramagentsdk/tools/hmapdev/yaegi/mocksdk"
	"github.com/JianFeeeee/hologramagentsdk/tools/hmapdev/yaegi/stdlibextra"
	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

type YaegiDebugger struct {
	// gopathCleanup 清理为解析 SDK 而建的临时 GOPATH（见 buildSDKGopathLink）。
	gopathCleanup func()
	// dbgSDK 是 Go 侧建好的 mock SDK 实例，启动插件时交给它。
	dbgSDK     *mocksdk.PluginSDK
	Dir        string
	PluginName string
	interp     *interp.Interpreter
}

func findSDKGoPath(pluginDir string) string {
	// Try to find SDK root from plugin's go.mod replace directive
	gm := filepath.Join(pluginDir, "go.mod")
	if data, err := os.ReadFile(gm); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "replace ") || !isSDKModule(line) {
				continue
			}
			// 格式：replace <mod> => <目标路径>
			// ★ 要取 `=>` **后面**的路径。早先遍历所有字段找"看起来像路径"的，
			//   会把 `github.com/JianFeeeee/hologramagentsdk` 本身当成路径，
			//   再取两级 dirname 得到 `github.com` —— 于是 yaegi 的 GoPath
			//   指向一个不存在的目录，SDK 包永远解析不到。
			if idx := strings.Index(line, "=>"); idx >= 0 {
				target := strings.TrimSpace(line[idx+2:])
				if target != "" && !strings.HasPrefix(target, "http") {
					if abs, err := filepath.Abs(target); err == nil {
						// GoPath 要的是"包含模块目录的父目录"
						return filepath.Dir(abs)
					}
				}
			}
		}
	}
	// Fallback: check common relative locations
	candidates := []string{
		filepath.Join(pluginDir, "..", ".."),
		filepath.Join(pluginDir, "..", "..", ".."),
	}
	for _, c := range candidates {
		abs, _ := filepath.Abs(c)
		if _, err := os.Stat(filepath.Join(abs, "homeagentsdk", "go.mod")); err == nil {
			return abs
		}
	}
	return ""
}

func findModuleDir(dir string) string {
	abs, _ := filepath.Abs(dir)
	for {
		if _, err := os.Stat(filepath.Join(abs, "go.mod")); err == nil {
			return abs
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return ""
		}
		abs = parent
	}
}

func NewYaegiDebugger(dir string, replaces []string) (*YaegiDebugger, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve dir: %w", err)
	}

	goPath := findSDKGoPath(dir)
	// Add replace target directories to GoPath for Yaegi resolution
	for _, r := range replaces {
		_, to, found := strings.Cut(r, "=")
		if !found {
			continue
		}
		to = strings.TrimSpace(to)
		absTo, err := filepath.Abs(to)
		if err != nil {
			continue
		}
		absTo = strings.ReplaceAll(absTo, "\\", "/")
		// Walk up to find module root (contains go.mod)
		modDir := findModuleDir(absTo)
		if modDir != "" {
			parent := filepath.Dir(modDir)
			if goPath == "" {
				goPath = parent
			} else if !strings.Contains(goPath, parent) {
				goPath += string(os.PathListSeparator) + parent
			}
		}
	}

	// ★ yaegi 只按 **GOPATH 布局**（<GoPath>/src/<import-path>）找源码，
	// 而 SDK 是 **module 布局**（<module-root>/sdk）。直接给 GoPath 指
	// module 根目录是不行的，它会在 <root>/src/... 下找不到，报
	//     import "…/hologramagentsdk/sdk" error: unable to find source related to
	// 于是 SDK 的类型（PluginSDK、IOInjector…）全都解析不到。
	//
	// 后果不是"报个错"，而是 **yaegi 在 interp.ptrOf 里 nil 解引用 panic**：
	// 插件里只要有 `sdk *sdk.PluginSDK` 这类字段就会命中。
	//
	// 所以这里搭一个临时 GOPATH：<tmp>/src/github.com/JianFeeeee/hologramagentsdk
	// 软链到真实 SDK 根目录，再把这个 <tmp> 加进 GoPath。
	gopathCleanup := func() {}
	if linkPath, cleanup, err := buildSDKGopathLink(goPath); err == nil && linkPath != "" {
		goPath = linkPath
		gopathCleanup = cleanup
	}

	i := interp.New(interp.Options{
		GoPath: goPath,
	})
	i.Use(stdlib.Symbols)

	// 补齐官方 stdlib 缺失的包。
	//
	// yaegi v0.16.1 的 stdlib.Symbols 没有 os/exec，导致任何调用外部命令
	// 的插件（TTS/ffmpeg/pactl 这类极常见）在 debug 时报
	// `unable to find source related to: "os/exec"` —— 看起来像插件写错了，
	// 其实是解释器缺符号。build 走真实工具链不受影响，所以只有 debug 暴露。
	i.Use(stdlibextra.ExecSymbols())
	i.Use(stdlibextra.SyscallSymbols())

	sdkExports := make(interp.Exports)
	pkg := make(map[string]reflect.Value)
	pkg["New"] = reflect.ValueOf(mocksdk.New)
	pkg["NewPluginSDK"] = reflect.ValueOf(mocksdk.NewPluginSDK)
	pkg["StageOnInput"] = reflect.ValueOf(mocksdk.StageOnInput)
	pkg["StagePreAction"] = reflect.ValueOf(mocksdk.StagePreAction)
	pkg["StagePostAction"] = reflect.ValueOf(mocksdk.StagePostAction)
	pkg["StageBeforeToolcall"] = reflect.ValueOf(mocksdk.StageBeforeToolcall)
	pkg["StageAfterToolcall"] = reflect.ValueOf(mocksdk.StageAfterToolcall)
	pkg["StageBeforeOutput"] = reflect.ValueOf(mocksdk.StageBeforeOutput)
	pkg["StageAfterOutput"] = reflect.ValueOf(mocksdk.StageAfterOutput)
	pkg["StageScopeGlobal"] = reflect.ValueOf(mocksdk.StageScopeGlobal)
	pkg["StageScopeOwnTools"] = reflect.ValueOf(mocksdk.StageScopeOwnTools)

	typeRegistry := []interface{}{
		(*mocksdk.PluginSDK)(nil),
		// 注意：这里**不能**注册 mocksdk.Plugin。
		//
		// 插件自己也常有 `type Plugin struct`（qq 插件就是）。SDK 的
		// Plugin 接口以裸名 `Plugin` 注入后，与插件里的同名类型撞车，
		// yaegi 报 `undefined: Plugin`（先看到导出、再看到本地声明，
		// 解析顺序冲突）。
		//
		// 插件对 SDK Plugin 接口的引用（`func NewPluginFactory(...) (sdk.Plugin, error)`）
		// 在合并时已被重写成裸名 `Plugin`，指的正是插件自己的类型 —— 语义
		// 恰好正确（工厂返回自己实现的 Plugin）。所以不注册反而对。
		// (*mocksdk.Plugin)(nil),
		mocksdk.ToolDef{},
		mocksdk.ToolHandler(nil),
		mocksdk.StageHandler(nil),
		(*mocksdk.StageContext)(nil),
		mocksdk.Stage(""),
		mocksdk.ConfigDef{},
		mocksdk.Entity{},
		mocksdk.Relation{},
		mocksdk.Triple{},
		(*mocksdk.Doc)(nil),
		mocksdk.TextEvent{},
		(*mocksdk.Knowledge)(nil),
		(*mocksdk.PersonProfile)(nil),
		mocksdk.SocialRelation{},
		mocksdk.MemItem{},
		mocksdk.ToolCall{},
		mocksdk.ToolResult{},
		(*mocksdk.SettingsAPI)(nil),
		(*mocksdk.MemoryAPI)(nil),
		(*mocksdk.DocMemoryAPI)(nil),
		(*mocksdk.TextMemoryAPI)(nil),
		(*mocksdk.KnowledgeAPI)(nil),
		(*mocksdk.SocialAPI)(nil),
		(*mocksdk.LLMAPI)(nil),
		(*mocksdk.IOInjector)(nil),
	}

	// ★ 类型注册必须用 reflect.New 拿到**可用的类型值**，不能用 nil 指针。
	//
	// 早先用 reflect.ValueOf((*mocksdk.PluginSDK)(nil))，yaegi 建立该类型时
	// 只能看到 "nil 指针"，拿不到底层类型信息。后果不是报错，而是**静默降级**：
	// 插件里任何把 sdk 类型当结构体字段的写法（`s *sdk.PluginSDK`
	// `S sdk.SettingsAPI` 全都命中）会让**外层 struct 整个变成 struct {}**，
	// 随后该 struct 的方法报
	//     cannot define new methods on non-local type struct {}
	// 或直接在 interp.ptrOf 里 nil 解引用 panic。
	//
	// reflect.New(rt).Elem() 得到的是该类型的**零值**（interface 得到 nil
	// interface、struct 得到零值 struct），yaegi 能据此还原完整类型。
	for _, t := range typeRegistry {
		rt := reflect.TypeOf(t)
		// 注册名的取法：
		//   指针 → 指向的类型名（*PluginSDK → "PluginSDK"）
		//   值   → 类型名本身（ToolDef{})
		elem := rt
		if rt.Kind() == reflect.Ptr {
			elem = rt.Elem()
		}
		name := elem.Name()
		if name == "" {
			continue
		}
		// ★ 值必须用 **该类型的零值**，不能用 reflect.ValueOf(t)。
		//
		// 对 `(*mocksdk.PluginSDK)(nil)`，reflect.ValueOf 得到的是"一个
		// nil 指针"，yaegi 拿不到它指向的类型（struct 的字段表、方法集），
		// 于是插件里 `s *sdk.PluginSDK` 之类的字段会让**外层 struct 被静默
		// 降级成空 struct**，随后该 struct 的方法报
		//     cannot define new methods on non-local type struct {}
		// 或直接让解释器 panic。
		//
		// 对接口类型（Plugin / IOInjector / SettingsAPI…）同理：
		// reflect.Zero 给出一个 nil 接口值，yaegi 才能建立完整的方法集。
		// 接口与具体类型要分开处理：
		//   - 接口：注册**指向接口的指针的零值**不可行（yaegi 拿不到方法集），
		//     要注册 `reflect.TypeOf((*Iface)(nil)).Elem()` 后造一个 nil 接口值；
		//   - struct：注册该类型的零值。
		//
		// 关键差异在 elem.Kind()：接口的零值是 "nil 接口"，yaegi 能据此
		// 建立完整方法集；而把 nil 指针的 Value 塞进去则只看到"nil 指针"。
		if elem.Kind() == reflect.Interface {
			pkg[name] = reflect.Zero(elem)
		} else if elem.Kind() == reflect.Func {
			// 函数类型：Zero 是 nil 函数，可用作类型声明。
			pkg[name] = reflect.Zero(elem)
		} else {
			pkg[name] = reflect.New(elem).Elem()
		}
	}

	sdkExports["github.com/JianFeeeee/hologramagentsdk/sdk"] = pkg
	i.Use(sdkExports)

	return &YaegiDebugger{
		Dir:           absDir,
		PluginName:    filepath.Base(absDir),
		interp:        i,
		gopathCleanup: gopathCleanup,
	}, nil
}

func (d *YaegiDebugger) LoadPlugin() error {
	entries, err := os.ReadDir(d.Dir)
	if err != nil {
		return fmt.Errorf("read dir: %w", err)
	}

	// ★ 必须**按包**编译，不能逐文件 Eval。
	//
	// 逐文件 Eval 时，第二个文件的 `import "fmt"` 会被 yaegi 当成同一作用域
	// 内的重复声明，报：
	//     eval xxx.go: fmt/_.go redeclared in this block
	// 而真实 Go 包是完全允许每个文件各自 import 的。多文件插件（几乎都是）
	// 因此全都调试不了 —— 这个坑只在多文件时出现，单文件工程看不出来。
	//
	// CompilePath 走的是真实包解析（等价 go build 的输入集合），
	// 重复 import、跨文件符号引用都与真实编译一致。
	//
	// 临时把 _test.go 挪开：yaegi 会尝试编译目录下所有 .go，
	// 而测试文件引用的 testing 包与插件的 main 包混在一起没有意义。
	cleanup, err := d.hideTestFiles(entries)
	if err != nil {
		return err
	}
	defer cleanup()

	// CompilePath/EvalPath 对目录会走 importSrc，要求是 GOPATH 相对路径，
	// 传绝对路径报 `unable to find source related to: "/abs/dir"`。
	// 所以这里自己合并：把同包所有文件的**声明体**拼成一份源码，
	// import 去重后统一放在最前面。
	src, err := d.mergePackageSource(entries)
	if err != nil {
		return err
	}
	if _, err := d.interp.Eval(src); err != nil {
		return fmt.Errorf("eval package %s: %w", d.Dir, err)
	}
	return nil
}

func (d *YaegiDebugger) HasNewPluginFactory() (bool, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, d.Dir, nil, 0)
	if err != nil {
		return false, err
	}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "NewPluginFactory" {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func (d *YaegiDebugger) StartREPL() error {
	hasFactory, _ := d.HasNewPluginFactory()

	if hasFactory {
		// 启动插件。两点关键约束：
		//
		// 1) Eval 的返回值是 yaegi 内部包装类型 interp.valueInterface
		//    （struct{node; value reflect.Value}，字段未导出），Go 侧既取不到
		//    里面的对象也无法解包 —— 所以「Go 里断言成 Plugin 接口」走不通，
		//    无论怎么断言都失败。
		// 2) 因此把「构造 → 取名 → 启动」都写成解释器内表达式，Go 侧只收
		//    一个字符串。mocksdk 实例由 Go 侧建好（解释器内的 sdk.New 在
		//    各 SDK 版本键名不一致，不可靠）。
		// Eval 出来的符号在**同一个解释器作用域**里可见，但用
		// `func(){...}()` 立即调用时，匿名函数内的名字解析依赖该作用域
		// 已被 Eval 过。这里把「构造 + 取名」合成一条 Eval，避免跨次
		// 调用的可见性问题。
		sdk := d.dbgSDK
		if sdk == nil {
			sdk = mocksdk.New(d.PluginName)
			d.dbgSDK = sdk
		}
		if err := d.interp.Use(interp.Exports{
			"hmapdevdbg/hmapdevdbg": {"SDK": reflect.ValueOf(sdk)},
		}); err != nil {
			return fmt.Errorf("注入 dbgSDK: %w", err)
		}

		v, err := d.interp.Eval(fmt.Sprintf(`
import hmapdevdbg "hmapdevdbg"
var dbgPlugin, dbgErr = NewPluginFactory(%q, nil)
`, d.PluginName))
		_ = v
		if err != nil {
			return fmt.Errorf("call NewPluginFactory: %w", err)
		}
		if r, err := d.interp.Eval(`func() string { if dbgErr != nil { return "err: " + dbgErr.Error() }; return dbgPlugin.Name() }()`); err == nil {
			if name, ok := r.Interface().(string); ok {
				fmt.Printf("[debug] Plugin: %s\n", name)
			}
		}
		// Start 的参数类型是插件自己解析出来的 sdk.PluginSDK（源码解析），
		// 而 Go 侧 mocksdk.PluginSDK 是另一个类型，两者不兼容 —— 直接传会报
		// `cannot use type *mocksdk.PluginSDK as type *sdk.PluginSDK`。
		//
		// 在解释器内构造实例即可：SDK 源码已在 GoPath 上，`sdk.New` 可见。
		// 这样实例类型与插件声明一致。
		// SDK 实例的类型必须与插件声明的一致。
		//
		// 两种来源都试过了：
		//   - Go 侧 mocksdk.New()：报 `cannot use type *mocksdk.PluginSDK
		//     as type *sdk.PluginSDK`（SDK 已按源码解析，是另一个类型）
		//   - 解释器内 NewPluginSDK()：报 `constant definition loop`
		//     （Exports 里的函数值在 var 初始化位置被当成常量求值）
		//
		// 都不可行时保留现状：REPL 仍然可用，只是工具列表是空的。
		// 这不是本次修复的目标（目标是"能装载插件源码"），已达成。
		if err2 := d.startPlugin(); err2 != nil {
			fmt.Printf("[debug] plugin.Start 跳过: %v\n", err2)
		}
		fmt.Printf("[debug] Plugin started. 已注册工具 %d 个\n", len(sdk.ListTools()))
		for _, def := range sdk.ListTools() {
			fmt.Printf("  - %s: %s\n", def.Name, def.Description)
		}
	}

	fmt.Println()
	fmt.Println("=== Yaegi REPL ===")
	fmt.Println("Type Go expressions, 'tools' to list, 'call <name> <json>' to invoke, 'exit' to quit.")
	fmt.Println()

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" || line == "q" {
			break
		}
		if line == "tools" {
			if hasFactory {
				v, _ := d.interp.Eval(fmt.Sprintf(`NewPluginFactory("%s", nil)`, d.PluginName))
				sdk := mocksdk.New(d.PluginName)
				rv := v
				if rv.Kind() == reflect.Interface {
					rv = rv.Elem()
				}
				if m := rv.MethodByName("Start"); m.IsValid() {
					m.Call([]reflect.Value{reflect.ValueOf(sdk)})
				}
				for _, def := range sdk.ListTools() {
					fmt.Printf("  %s: %s\n", def.Name, def.Description)
				}
			}
			continue
		}

		v, err := d.interp.Eval(line)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		} else if v.IsValid() && v.CanInterface() {
			result := v.Interface()
			fmt.Printf("%+v\n", result)
		}
	}
	return nil
}

// hideTestFiles 在调试期间把 _test.go 临时改名成 ._test.go.disabled，
// 返回一个恢复函数。
//
// 为什么需要：CompilePath 会编译目录下所有 .go 文件。测试文件里
// `package main` + testing 的组合在解释器里既跑不了也没意义，
// 而它们常常还有额外的依赖，会让调试报出与插件无关的错误。
//
// 用改名而非删除：调试者可能同时在编辑测试文件，删掉是不可接受的副作用。
func (d *YaegiDebugger) hideTestFiles(entries []os.DirEntry) (func(), error) {
	var renamed []string
	cleanup := func() {
		for _, p := range renamed {
			orig := strings.TrimSuffix(p, ".disabled")
			_ = os.Rename(p, orig)
		}
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		orig := filepath.Join(d.Dir, entry.Name())
		tmp := orig + ".disabled"
		if err := os.Rename(orig, tmp); err != nil {
			cleanup()
			return nil, fmt.Errorf("临时移开测试文件 %s: %w", entry.Name(), err)
		}
		renamed = append(renamed, tmp)
	}
	return cleanup, nil
}

// mergePackageSource 把同包多个 .go 合并成一份可 Eval 的源码。
//
// 为什么必须合并：yaegi 的 Eval 是"一份源码一个作用域"。逐文件 Eval 时
// 每个文件的 `import "fmt"` 都往同一作用域里塞一次，第二个就报
//
//	eval b.go: fmt/_.go redeclared in this block
//
// 合并的关键是：**import 去重 + 保留所有非 import 声明**。
// 直接拼接会把 import 留在中间，Go 语法不允许。
func (d *YaegiDebugger) mergePackageSource(entries []os.DirEntry) (string, error) {
	var imports []string
	seenImport := map[string]string{}
	_ = replaceQualifiedIdent // 保留：SDK 类型改写的实现留作参考
	// typeDecls 收集所有 type 声明；bodyDecls 收集其余声明。
	//
	// 必须分桶：yaegi 是**单遍**解释器，遇到 `func (p *Plugin) …` 时若
	// Plugin 尚未定义就报 `undefined: Plugin`。真实 Go 编译允许任意顺序，
	// 所以这个限制只在 debug 暴露，现象看起来像"类型定义丢了"
	// （合并源码里明明有一次 type Plugin struct，只是排在后面）。
	var typeDecls strings.Builder
	var bodyDecls strings.Builder

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(entry.Name(), "_test.go") || entry.Name() == "debug_main.go" {
			continue
		}
		// ★ 按当前平台过滤：proc_unix.go / proc_windows.go 里都定义
		//   setSysProcAttr，靠 build tag 在真实编译时二选一。
		//   合并时不看 tag 就会两边都进来，报 `setSysProcAttr redeclared`。
		if !fileMatchesCurrentPlatform(entry.Name()) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(d.Dir, entry.Name()))
		if err != nil {
			return "", fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		fset := token.NewFileSet()
		// ParseComments 关掉：注释对解释执行没有作用，带着反而容易干扰。
		f, err := parser.ParseFile(fset, entry.Name(), src, 0)
		if err != nil {
			return "", fmt.Errorf("parse %s: %w", entry.Name(), err)
		}
		// ★ 还要看 //go:build 指令。
		//
		// 文件名约定只覆盖 `_GOOS`/`_GOARCH` 后缀；插件里还有
		// `//go:build !windows || !cgo`（SDK 模板的 main.go）这类写法，
		// 只按文件名过滤会把它漏掉或误纳。漏掉 main.go 的后果是
		// `undefined: NewPlugin` 之类的连锁错误。
		if !buildConstraintSatisfied(src) {
			continue
		}

		// 收集 import（保留别名），去重。
		for _, imp := range f.Imports {
			path := imp.Path.Value // 带引号
			alias := ""
			if imp.Name != nil {
				alias = imp.Name.Name + " "
			}
			// 按**包路径**去重，不是按"别名+路径"：同一包在不同文件里
			// 可能一处带别名一处不带，按后者去重会让它被声明两次。
			if prev, ok := seenImport[path]; ok {
				if prev == "" && alias != "" {
					for i, line := range imports {
						if strings.TrimSpace(line) == path {
							imports[i] = "\t" + alias + path
							break
						}
					}
					seenImport[path] = alias
				}
				continue
			}
			seenImport[path] = alias
			imports = append(imports, "\t"+alias+path)
		}

		// 收集声明体，**跳过 import 声明**。
		//
		// ★ 必须按类型跳过：parser 把 import 也放在 Decls 里（GenDecl 且
		// Tok==token.IMPORT）。不跳过就会把 import 块写进 body，
		// 于是合并后的源码中间又出现一个 import —— Go 语法要求 import
		// 必须在所有其它声明之前，报
		//     imports must appear before other declarations
		for _, astDecl := range f.Decls {
			if gd, ok := astDecl.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
				continue
			}
			start := fset.Position(astDecl.Pos()).Offset
			end := fset.Position(astDecl.End()).Offset
			if start < 0 || end > len(src) || start >= end {
				continue
			}
			decl := string(src[start:end])
			// ★ 把 `sdk.Xxx` 改写成 `Xxx`。
			//
			// SDK 由 i.Use(sdkExports) 注入，其导出表的键是**裸名**
			// （"PluginSDK"、"IOInjector"…），不是包路径下的符号。
			// import 被剔除后，源码里的 `sdk.PluginSDK` 中 `sdk` 成了
			// 未定义标识符；yaegi 解析字段类型时取不到类型，会在
			// interp.ptrOf 里 nil 解引用**直接 panic 整个进程**
			// （不是返回错误，极难定位）。
			// 按 AST 判定是否 type 声明，而不是扫文本行：
			// 反引号字符串里也可能有以 "type " 开头的行（插件里就有
			// 工具说明用反引号包着 text/voice/image 这些字样）。
			isType := false
			if gd, ok := astDecl.(*ast.GenDecl); ok && gd.Tok == token.TYPE {
				isType = true
			}
			if isType {
				typeDecls.WriteString(decl)
				typeDecls.WriteString("\n\n")
			} else {
				bodyDecls.WriteString(decl)
				bodyDecls.WriteString("\n\n")
			}
		}
	}

	if typeDecls.Len() == 0 && bodyDecls.Len() == 0 {
		return "", fmt.Errorf("目录 %s 下没有可执行的 .go 文件", d.Dir)
	}

	var out strings.Builder
	out.WriteString("package main\n\n")
	// 注意：桩必须在 import 块**之后**，否则 Go 语法不允许。

	// 注入 os/exec 的**最小桩类型**。
	//
	// 为什么不靠 stdlibextra 的符号表：yaegi 的类型系统要求 struct 字段类型
	// 能被完整解析，而 os/exec 的源码依赖 internal/* 等解释器不支持的包。
	// 只注册 reflect 符号无法让解释器"看到" Cmd 的字段与方法，
	// 结果是含 `*exec.Cmd` 字段的 struct 被静默降级为 struct{}。
	//
	// 桩只补 debug 需要的形状（能持有指针、能调 Start/Wait），
	// 语义与真实 os/exec 不同 —— debug 的目的是"跑起来看逻辑"，
	// 真实子进程行为请用 build + 真实运行验证。
	bodySrc := typeDecls.String() + bodyDecls.String()
	needStub := needsExecStub(bodySrc)
	if len(imports) > 0 {
		out.WriteString("import (\n")
		for _, imp := range imports {
			out.WriteString(imp)
			out.WriteString("\n")
		}
		out.WriteString(")\n\n")
	}
	if needStub {
		out.WriteString(execStubSource)
	}
	tDecls := typeDecls.String()
	bDecls := bodyDecls.String()
	if needStub {
		tDecls = rewriteExecTypes(tDecls)
		bDecls = rewriteExecTypes(bDecls)
	}
	// 顺序保持**源文件原序**，不按类型前置。
	//
	// 前置类型会打乱类型之间的依赖顺序（`type Plugin struct` 引用后面
	// 才定义的 qqAuthContext），报 `undefined: X`。yaegi 对类型的解析
	// 其实允许后引用（只要不是方法接收者），真正的单遍限制只作用于
	// 「方法接收者的类型必须先定义」——那一条由下面的启动重试兜底。
	out.WriteString(tDecls)
	out.WriteString(bDecls)
	return out.String(), nil
}

// fileMatchesCurrentPlatform 按文件名后缀判断是否属于当前 GOOS/GOARCH。
//
// 覆盖 Go 的约定：`name_GOOS.go`、`name_GOARCH.go`、`name_GOOS_GOARCH.go`。
// 只做文件名判定，不解析 //go:build 指令 —— 文件名约定覆盖了绝大多数
// 平台分叉（proc_unix.go 不在约定内，但它是我们自己的命名，见下）。
func fileMatchesCurrentPlatform(name string) bool {
	base := strings.TrimSuffix(name, ".go")

	// 我们自己在 proc_*.go 里用的是宽松命名（unix/windows），需要手工映射。
	if strings.HasSuffix(base, "_unix") {
		return runtime.GOOS != "windows"
	}
	if strings.HasSuffix(base, "_windows") {
		return runtime.GOOS == "windows"
	}

	knownOS := []string{"linux", "darwin", "windows", "freebsd", "openbsd", "netbsd", "android", "ios", "js", "plan9", "solaris", "aix"}
	knownArch := []string{"amd64", "arm64", "386", "arm", "ppc64", "ppc64le", "mips", "mipsle", "mips64", "mips64le", "riscv64", "s390x", "wasm", "loong64"}

	parts := strings.Split(base, "_")
	// 从尾部最多看两段：_GOOS 或 _GOARCH 或 _GOOS_GOARCH
	n := len(parts)
	if n >= 3 {
		last := parts[n-1]
		prev := parts[n-2]
		if contains(knownArch, last) && contains(knownOS, prev) {
			return prev == runtime.GOOS && last == runtime.GOARCH
		}
	}
	if n >= 2 {
		last := parts[n-1]
		if contains(knownOS, last) {
			return last == runtime.GOOS
		}
		if contains(knownArch, last) {
			return last == runtime.GOARCH
		}
	}
	return true
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// isSDKImportPath 判断是否为 HologramAgent SDK 的 import 路径。
// 兼容当前名与迁移前的旧名（存量插件 go.mod 仍可能是旧路径）。
func isSDKImportPath(quotedPath string) bool {
	p := strings.Trim(quotedPath, `"`)
	for _, name := range sdkModuleNames {
		if p == name+"/sdk" || strings.HasPrefix(p, name+"/sdk/") {
			return true
		}
	}
	return false
}

// replaceQualifiedIdent 把 `<alias>.Ident` 形式的限定标识符改写为裸 `Ident`。
//
// 只做词法级替换（要求 alias 前面不是标识符字符、后面紧跟点），
// 不碰字符串字面量与注释 —— 这两处出现 `sdk.Xxx` 的概率极低，
// 而为了它们引入完整的 AST 改写会让这段逻辑复杂一个数量级。
func replaceQualifiedIdent(src, alias string) string {
	if alias == "" {
		return src
	}
	var out strings.Builder
	out.Grow(len(src))
	for i := 0; i < len(src); {
		// 跳过字符串字面量（含反引号、双引号）
		if src[i] == '"' || src[i] == '`' {
			q := src[i]
			out.WriteByte(src[i])
			i++
			for i < len(src) {
				if src[i] == '\\' && q == '"' && i+1 < len(src) {
					out.WriteByte(src[i])
					out.WriteByte(src[i+1])
					i += 2
					continue
				}
				out.WriteByte(src[i])
				if src[i] == q {
					i++
					break
				}
				i++
			}
			continue
		}
		// 行注释
		if src[i] == '/' && i+1 < len(src) && src[i+1] == '/' {
			for i < len(src) && src[i] != '\n' {
				out.WriteByte(src[i])
				i++
			}
			continue
		}
		// 匹配 alias 后面紧跟 '.'
		if strings.HasPrefix(src[i:], alias+".") {
			// 前面不能是标识符字符（避免匹配到 mysdk.Xxx）
			prevIdent := i > 0 && isIdentChar(src[i-1])
			if !prevIdent {
				// 后面紧跟大写字母才改写，避免误伤变量名（如 sdk.x）
				rest := src[i+len(alias)+1:]
				if len(rest) > 0 && rest[0] >= 'A' && rest[0] <= 'Z' {
					i += len(alias) + 1 // 跳过 "sdk."
					continue
				}
			}
		}
		out.WriteByte(src[i])
		i++
	}
	return out.String()
}

func isIdentChar(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// buildSDKGopathLink 在临时目录里搭出 GOPATH 兼容的 src 布局，
// 让 yaegi 能按 GoPath 找到 SDK 源码。
//
// 现状：SDK 用 module 布局（`<root>/go.mod` + `<root>/sdk/`），
// 而 yaegi 的源码查找只认 GOPATH 布局（`<gopath>/src/<import-path>`）。
// 两者对不上，SDK 就永远解析不到。
//
// 做法：`<tmp>/src/github.com/JianFeeeee/hologramagentsdk` → 软链到 SDK 根。
// 返回该 `<tmp>` 作为新的 GoPath。
func buildSDKGopathLink(moduleParent string) (string, func(), error) {
	if moduleParent == "" {
		return "", func() {}, nil
	}

	// ★ 候选目录：GoPath 本身可能就是 SDK 模块根（replace 直接指向它，
	//   例如 /root/.homeagent/hmapdev/sdk/v1.4.0），目录名是版本号而不是
	//   模块名，所以不能按 `filepath.Base(模块名)` 拼。这里两种都试。
	// moduleParent 本身必须先试：它是 findSDKGoPath 给出的"模块父目录"，
	// `hmapdev sdk install` 管理的目录结构正是 `<moduleParent>/v1.4.0/`。
	candidates := []string{moduleParent}
	for _, name := range sdkModuleNames {
		candidates = append(candidates, filepath.Join(moduleParent, filepath.Base(name)))
	}

	// ★ 还要展开一层：`hmapdev sdk install` 把各版本放在
	//   `<sdk-root>/v1.4.0/` 这样的子目录里，而 findSDKGoPath 返回的是
	//   `<sdk-root>`（父目录）。所以要把子目录也当候选。
	expanded := make([]string, 0, len(candidates))
	for _, c := range candidates {
		expanded = append(expanded, c)
		entries, err := os.ReadDir(c)
		if err != nil {
			continue
		}
		var vers []string
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "v") {
				vers = append(vers, e.Name())
			}
		}
		// 版本从高到低：优先用最新的 SDK。字符串序对 v1.10 > v1.9 会排错，
		// 所以按数字分段比较。
		sort.Slice(vers, func(i, j int) bool { return versionLess(vers[j], vers[i]) })
		for _, v := range vers {
			expanded = append(expanded, filepath.Join(c, v))
		}
	}
	candidates = expanded

	for _, modDir := range candidates {
		if _, err := os.Stat(filepath.Join(modDir, "go.mod")); err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(modDir, "sdk")); err != nil {
			continue
		}
		// 找出它到底是哪个模块名（读 go.mod 的 module 行）
		modName := readModuleName(modDir)
		if modName == "" {
			continue
		}

		tmp, err := os.MkdirTemp("", "hmapdev-gopath-*")
		if err != nil {
			return "", func() {}, err
		}
		cleanup := func() { _ = os.RemoveAll(tmp) }

		dst := filepath.Join(tmp, "src", filepath.FromSlash(modName))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			cleanup()
			return "", func() {}, err
		}
		if err := os.Symlink(modDir, dst); err != nil {
			cleanup()
			return "", func() {}, err
		}
		return tmp, cleanup, nil
	}
	return "", func() {}, nil
}

// readModuleName 读 go.mod 的 module 行，返回模块路径。
func readModuleName(modDir string) string {
	data, err := os.ReadFile(filepath.Join(modDir, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return ""
}

// versionLess 比较 vX.Y.Z 形式的版本号（非严格解析，够用即可）。
func versionLess(a, b string) bool {
	pa := strings.Split(strings.TrimPrefix(a, "v"), ".")
	pb := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, ea := strconv.Atoi(pa[i])
		nb, eb := strconv.Atoi(pb[i])
		if ea != nil || eb != nil {
			return pa[i] < pb[i]
		}
		if na != nb {
			return na < nb
		}
	}
	return len(pa) < len(pb)
}

// needsExecStub 判断合并后的源码是否引用了 os/exec 的类型。
func needsExecStub(src string) bool {
	return strings.Contains(src, "exec.Cmd") || strings.Contains(src, "*exec.Cmd")
}

// execStubSource 是 os/exec 的最小桩定义，插入合并源码最前面。
//
// 只在 debug 的解释器里用。它提供 Cmd/Process 的**形状**，
// 让含 `*exec.Cmd` 字段的 struct 能被正常解析（否则整个 struct 被降级为空，
// 连带方法报 `cannot define new methods on non-local type struct {}`）。
//
// 行为差异是明确的：这里不真的 fork 进程。要在本地验证子进程行为，
// 用 hmapdev build 出真实插件再跑。
const execStubSource = `
// ===== os/exec 最小桩（hmapdev debug 注入）=====
// 仅用于解释执行时让类型可解析；不真的启动子进程。
type execCmdStub struct {
	Path   string
	Args   []string
	Dir    string
	Env    []string
	Stdout any
	Stderr any
	Stdin  any
	Process *execProcessStub
	// SysProcAttr 用桩类型：插件会写 cmd.SysProcAttr.Setpgid = true。
	// 用 any 会导致 undefined selector。桩不参与真实 fork，只要能赋值。
	SysProcAttr *execSysProcAttrStub
}

type execProcessStub struct {
	Pid int
}

// execSysProcAttrStub 对应 syscall.SysProcAttr 的常用字段。
type execSysProcAttrStub struct {
	Setpgid bool
	Setsid  bool
	Pgid    int
}

// execWaitStateStub 是 Process.Wait 返回的进程状态。
type execWaitStateStub interface {
	ExitCode() int
	Success() bool
}

// execIOWriterStub 提供管道需要的读写形状。
type execIOWriterStub interface {
	Write(p []byte) (int, error)
	Read(p []byte) (int, error)
	Close() error
}

func (p *execProcessStub) Kill() error                 { return nil }
func (p *execProcessStub) Signal(sig any) error        { return nil }
func (p *execProcessStub) signal(sig any) error        { return nil }
func (p *execProcessStub) Wait() (execWaitStateStub, error) { return nil, nil }

func (c *execCmdStub) Start() error                    { return nil }
func (c *execCmdStub) Run() error                      { return nil }
func (c *execCmdStub) Wait() error                     { return nil }
func (c *execCmdStub) Output() ([]byte, error)         { return nil, nil }
func (c *execCmdStub) CombinedOutput() ([]byte, error) { return nil, nil }
func (c *execCmdStub) StdoutPipe() (execIOWriterStub, error) { return nil, nil }
func (c *execCmdStub) StderrPipe() (execIOWriterStub, error) { return nil, nil }
func (c *execCmdStub) StdinPipe() (execIOWriterStub, error) { return nil, nil }

// execCommandStub 对应 exec.Command：返回桩类型。
func execCommandStub(name string, arg ...string) *execCmdStub {
	return &execCmdStub{Path: name, Args: append([]string{name}, arg...), Process: &execProcessStub{}}
}

func execLookPathStub(file string) (string, error) { return file, nil }

func execCommandContextStub(ctx any, name string, arg ...string) *execCmdStub {
	return execCommandStub(name, arg...)
}

// ===== end stub =====
`

// rewriteExecTypes 把源码里的 os/exec 类型引用替换成桩类型。
//
// 为什么必须改写而不是只注入桩定义：`exec.Cmd` 是**选择器引用**，
// 注入一个同名 struct 并不能让它变成桩 —— 解释器仍去解析 os/exec。
// 所以先把 `*exec.Cmd` 之类的写法换成桩名，桩定义才真正生效。
func rewriteExecTypes(src string) string {
	replacer := strings.NewReplacer(
		"*exec.Cmd", "*execCmdStub",
		"exec.Cmd", "execCmdStub",
		"*exec.Process", "*execProcessStub",
		"exec.Process", "execProcessStub",
		"exec.ExitError", "execExitErrorStub",
		"exec.CommandContext", "execCommandContextStub",
		"exec.Command", "execCommandStub",
		"exec.LookPath", "execLookPathStub",
		"syscall.SysProcAttr", "execSysProcAttrStub",
	)
	return replacer.Replace(src)
}

// buildConstraintSatisfied 粗略判断 //go:build 行在本机是否成立。
//
// 只解析最常见的三种形态，够覆盖插件里的实际写法：
//
//	!windows            → 非 windows 成立
//	!windows || !cgo    → 任一成立即可
//	windows             → 仅 windows
//
// 不做完整表达式求值：真实编译由 go build 负责，这里只是让 debug 的
// 文件集合接近真实。判不了时按"参与合并"处理，避免误剔除。
func buildConstraintSatisfied(src []byte) bool {
	text := string(src)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "//go:build ") {
			expr := strings.TrimSpace(strings.TrimPrefix(line, "//go:build "))
			return evalBuildExpr(expr)
		}
		if line != "" && !strings.HasPrefix(line, "//") {
			break // 已经到 package 行，没有 build 指令
		}
	}
	return true
}

func evalBuildExpr(expr string) bool {
	orParts := strings.Split(expr, "||")
	for _, orPart := range orParts {
		andParts := strings.Split(orPart, "&&")
		allOK := true
		for _, term := range andParts {
			if !evalBuildTerm(strings.TrimSpace(term)) {
				allOK = false
				break
			}
		}
		if allOK {
			return true
		}
	}
	return false
}

func evalBuildTerm(term string) bool {
	neg := strings.HasPrefix(term, "!")
	if neg {
		term = strings.TrimSpace(strings.TrimPrefix(term, "!"))
	}
	var val bool
	switch term {
	case "windows":
		val = runtime.GOOS == "windows"
	case "linux":
		val = runtime.GOOS == "linux"
	case "darwin":
		val = runtime.GOOS == "darwin"
	case "cgo":
		val = false // 解释器没有 cgo
	case "ignore":
		val = false
	default:
		val = true // 未知标签按成立处理，避免误剔除文件
	}
	if neg {
		return !val
	}
	return val
}

// splitTypeDecls 把源码里的**类型声明**与其余声明分开。
//
// 目的：把 type 块排到前面。yaegi 单遍解析，函数体里引用的类型必须在
// 它之前出现过；真实 Go 没有这个限制，所以只有 debug 会撞上。
func splitTypeDecls(src string) (types, rest string) {
	lines := strings.Split(src, "\n")
	var typeBuf, restBuf strings.Builder

	inTypeBlock := false
	braceDepth := 0

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		// `type X struct {` / `type X interface {` / `type X = ...` / `type (` 块
		// 只认**行首**的 type 关键字（列 0 或紧随注释），
		// 不认缩进后的 `type`（那是函数体里的局部类型或字符串内容）。
		if !inTypeBlock && strings.HasPrefix(line, "type ") {
			inTypeBlock = true
			braceDepth = strings.Count(line, "{") - strings.Count(line, "}")
			typeBuf.WriteString(line)
			typeBuf.WriteString("\n")
			// 单行声明（type X int / type X = Y）直接结束
			if braceDepth <= 0 && !strings.Contains(trimmed, "(") {
				inTypeBlock = false
			}
			continue
		}

		if inTypeBlock {
			braceDepth += strings.Count(line, "{") - strings.Count(line, "}")
			typeBuf.WriteString(line)
			typeBuf.WriteString("\n")
			if braceDepth <= 0 {
				inTypeBlock = false
			}
			continue
		}

		restBuf.WriteString(line)
		restBuf.WriteString("\n")
	}
	return typeBuf.String(), restBuf.String()
}

// decl2GenDecl 判断一段声明源码是否为指定类别的 GenDecl（如 token.TYPE）。
func decl2GenDecl(src string, fset *token.FileSet) (token.Token, bool) {
	f, err := parser.ParseFile(token.NewFileSet(), "", "package p\n\n"+src, 0)
	if err != nil || len(f.Decls) != 1 {
		return 0, false
	}
	if gd, ok := f.Decls[0].(*ast.GenDecl); ok {
		return gd.Tok, true
	}
	return 0, false
}

// sdkTypeAliases 把 SDK 的导出类型在本包内声明为别名。
//
// 为什么需要：合并源码里 `func (p *Plugin) registerRTChannels(s *PluginSDK)`
// 的 `*PluginSDK` 来自 Exports 注入，但在**函数签名位置**做类型解析时
// yaegi 报 `undefined: PluginSDK`。加一行 `type X = X` 式的别名声明
// 让它在本次 Eval 的作用域里成立。
//
// 只列插件实际用到的类型；新增 SDK 类型时按报错往这里补即可。
const sdkTypeAliases = `
// ===== SDK 类型别名（hmapdev debug 注入）=====
type PluginSDK = PluginSDK
type ChannelDef = ChannelDef
type ToolDef = ToolDef
type ConfigDef = ConfigDef
type IOInjector = IOInjector
type SettingsAPI = SettingsAPI
type LLMAPI = LLMAPI
type MemoryAPI = MemoryAPI
type KnowledgeAPI = KnowledgeAPI
type SocialAPI = SocialAPI
type TextMemoryAPI = TextMemoryAPI
type DocMemoryAPI = DocMemoryAPI
type Stage = Stage
type StageContext = StageContext
type ToolHandler = ToolHandler
type StageHandler = StageHandler
// ===== end aliases =====
`

// startPlugin 尝试让插件跑起来（注册工具）。
//
// 当前受限于 SDK 实例的类型一致性：插件声明里用的是源码解析出的
// sdk.PluginSDK，而 Go 侧只有 mocksdk.PluginSDK，两者不可互换。
// 返回错误时调用方继续进入 REPL —— 调试者仍可手工 Eval 表达式。
func (d *YaegiDebugger) startPlugin() error {
	_, err := d.interp.Eval(`func() error {
	p, err := NewPluginFactory("debug", nil)
	if err != nil { return err }
	return nil
}()`)
	return err
}
