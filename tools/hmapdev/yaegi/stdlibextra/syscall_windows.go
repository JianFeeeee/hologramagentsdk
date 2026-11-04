//go:build windows

package stdlibextra

// Windows 上的补充符号表（当前为空，保留形态以便将来按需扩展）。
//
// 为什么需要这个文件：Go 要求同一函数在**所有** GOOS 下都有定义，
// 而 syscall_unix.go 带 `//go:build !windows`。没有这个空实现的话，
// syscall.go 里对 unixSyscallSymbols() 的调用在 Windows 上编译不过。
//
// Windows 的进程控制不走信号（TerminateProcess / Job Object），
// 所以这里不需要 Kill/SIG* —— 缺它们是**正确的平台行为**，不是遗漏。

import "reflect"

// unixSyscallSymbols 在 Windows 上返回空表。
func unixSyscallSymbols() map[string]reflect.Value {
	return map[string]reflect.Value{}
}
