package stdlibextra

import (
	"reflect"
	"testing"
)

// TestExecSymbolsHasCommand 钉住最小必需符号。
//
// os/exec 在 yaegi 官方 stdlib 里完全没有，不补的话任何调用外部命令的插件
// 在 debug 时报 `unable to find source related to: "os/exec"`。
func TestExecSymbolsHasCommand(t *testing.T) {
	ex := ExecSymbols()
	pkg, ok := ex["os/exec/exec"]
	if !ok {
		t.Fatal("缺少 os/exec/exec 导出表")
	}
	for _, name := range []string{"Command", "CommandContext", "LookPath"} {
		if _, ok := pkg[name]; !ok {
			t.Fatalf("缺少 %s", name)
		}
	}
}

// TestExecSymbolsAvoidsTypes 钉住：**不能**导出 exec.Cmd 这类复合类型。
//
// yaegi v0.16.1 反射它们时会在 interp.ptrOf 里 nil 解引用直接 panic
// （实测：整个 hmapdev 进程崩溃，不是返回错误）。所以只导出函数。
func TestExecSymbolsAvoidsTypes(t *testing.T) {
	ex := ExecSymbols()
	pkg := ex["os/exec/exec"]
	for name, v := range pkg {
		if v.Kind() == reflect.Struct {
			t.Fatalf("%s 是结构体值 —— 会触发 yaegi ptrOf panic，必须移除", name)
		}
	}
}
