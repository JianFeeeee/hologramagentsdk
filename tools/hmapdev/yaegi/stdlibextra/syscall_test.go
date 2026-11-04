package stdlibextra

import (
	"reflect"
	"runtime"
	"testing"
)

// TestSyscallSymbolsHasKill 钉住进程组控制所需的最小符号。
//
// ★ 只在 Unix 上断言：Windows 没有 Kill/SIG*（那不是遗漏，是平台事实），
//
//	详见 syscall.go 文件头。此前这条判据不分平台，导致
//	`GOOS=windows go test` 会红 —— 而实际上 Windows 就不该有这些符号。
//
//	跨平台真正该保证的是：**导出表本身存在**，且 Unix 上含进程组控制符号。
func TestSyscallSymbolsHasKill(t *testing.T) {
	ex := SyscallSymbols()
	pkg, ok := ex["syscall/syscall"]
	if !ok {
		t.Fatal("缺少 syscall/syscall 导出表")
	}
	if runtime.GOOS == "windows" {
		// Windows：表可以为空，但不能为 nil（nil 会让 yaegi 解引用 panic）。
		if pkg == nil {
			t.Fatal("★ 导出表为 nil —— yaegi 反射时会 panic")
		}
		return
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
