package stdlibextra

import (
	"reflect"
	"testing"
)

// TestSyscallSymbolsHasKill 钉住进程组控制所需的最小符号。
func TestSyscallSymbolsHasKill(t *testing.T) {
	ex := SyscallSymbols()
	pkg, ok := ex["syscall/syscall"]
	if !ok {
		t.Fatal("缺少 syscall/syscall 导出表")
	}
	for _, name := range []string{"Kill", "SIGTERM", "SIGKILL", "ESRCH"} {
		if _, ok := pkg[name]; !ok {
			t.Fatalf("缺少 %s", name)
		}
	}
}

// TestSyscallAvoidsSysProcAttr 钉住：不导出 SysProcAttr。
//
// 它含平台特定字段，yaegi 反射时 panic（与 exec.Cmd 同类问题）。
func TestSyscallAvoidsSysProcAttr(t *testing.T) {
	ex := SyscallSymbols()
	if _, ok := ex["syscall/syscall"]["SysProcAttr"]; ok {
		t.Fatal("SysProcAttr 会触发 yaegi panic，不能导出")
	}
	for name, v := range ex["syscall/syscall"] {
		if v.Kind() == reflect.Struct {
			t.Fatalf("%s 是结构体值，可能触发 panic", name)
		}
	}
}
