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
	"strings"

	"github.com/JianFeeeee/hologramagentsdk/tools/hmapdev/yaegi/mocksdk"
	"github.com/JianFeeeee/hologramagentsdk/tools/hmapdev/yaegi/stdlibextra"
	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

type YaegiDebugger struct {
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
		(*mocksdk.Plugin)(nil),
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

	for _, t := range typeRegistry {
		rt := reflect.TypeOf(t)
		name := rt.Name()
		if name == "" {
			name = rt.Elem().Name()
		}
		pkg[name] = reflect.ValueOf(t)
	}

	sdkExports["github.com/JianFeeeee/hologramagentsdk/sdk"] = pkg
	i.Use(sdkExports)

	return &YaegiDebugger{
		Dir:        absDir,
		PluginName: filepath.Base(absDir),
		interp:     i,
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
		v, err := d.interp.Eval(fmt.Sprintf(`NewPluginFactory("%s", nil)`, d.PluginName))
		if err != nil {
			return fmt.Errorf("call NewPluginFactory: %w", err)
		}

		plugin := v.Interface().(mocksdk.Plugin)
		fmt.Printf("[debug] Plugin: %s\n", plugin.Name())

		sdk := mocksdk.New(d.PluginName)
		if err := plugin.Start(sdk); err != nil {
			return fmt.Errorf("plugin.Start: %w", err)
		}
		fmt.Printf("[debug] Plugin started. Registered %d tools.\n", len(sdk.ListTools()))
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
				plugin := v.Interface().(mocksdk.Plugin)
				sdk := mocksdk.New(d.PluginName)
				plugin.Start(sdk)
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
	var body strings.Builder

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

		// 收集 import（保留别名），去重。
		for _, imp := range f.Imports {
			path := imp.Path.Value // 带引号
			alias := ""
			if imp.Name != nil {
				alias = imp.Name.Name + " "
			}
			// ★ SDK 包已通过 i.Use(sdkExports) 注入，不再当源码解析。
			//   合并源码里若保留它，yaegi 会去 GoPath 找
			//   `…/hologramagentsdk/sdk` 目录 —— 而 SDK 是 module 布局
			//   （<root>/sdk，没有 src/ 前缀），永远找不到，报
			//   `unable to find source related to: ".../sdk"`。
			//   Exports 里的键与 import 路径一致，删除 import 后
			//   标识符 `sdk.Xxx` 仍能解析。
			if isSDKImportPath(path) {
				continue
			}

			// ★ 按**包路径**去重，不是按"别名+路径"。
			//   同一包在 A 文件写成 `sdk "…/sdk"`、在 B 文件写成 `"…/sdk"`
			//   会被当成两个不同 import，合并后同一个包被声明两次 ——
			//   报 `sdk redeclared`。
			//   写法不一致时保留**带别名**的那条（更明确，且不影响使用）。
			if prev, ok := seenImport[path]; ok {
				if prev == "" && alias != "" {
					// 之前是无别名，现在有别名 → 升级为带别名
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
		for _, decl := range f.Decls {
			if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
				continue
			}
			start := fset.Position(decl.Pos()).Offset
			end := fset.Position(decl.End()).Offset
			if start < 0 || end > len(src) || start >= end {
				continue
			}
			body.Write(src[start:end])
			body.WriteString("\n\n")
		}
	}

	if body.Len() == 0 {
		return "", fmt.Errorf("目录 %s 下没有可执行的 .go 文件", d.Dir)
	}

	var out strings.Builder
	out.WriteString("package main\n\n")
	if len(imports) > 0 {
		out.WriteString("import (\n")
		for _, imp := range imports {
			out.WriteString(imp)
			out.WriteString("\n")
		}
		out.WriteString(")\n\n")
	}
	out.WriteString(body.String())
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
