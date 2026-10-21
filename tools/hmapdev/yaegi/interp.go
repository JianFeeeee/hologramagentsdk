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
	"strings"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
	"github.com/JianFeeeee/hologramagentsdk/tools/hmapdev/yaegi/mocksdk"
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
			if strings.HasPrefix(line, "replace ") && isSDKModule(line) {
				parts := strings.Fields(line)
				for _, p := range parts {
					if isSDKModule(p) && strings.Contains(p, string(filepath.Separator)) {
						return filepath.Dir(filepath.Dir(p))
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

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		if entry.Name() == "debug_main.go" {
			continue
		}

		src, err := os.ReadFile(filepath.Join(d.Dir, entry.Name()))
		if err != nil {
			return fmt.Errorf("read %s: %w", entry.Name(), err)
		}

		_, err = d.interp.Eval(string(src))
		if err != nil {
			return fmt.Errorf("eval %s: %w", entry.Name(), err)
		}
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
