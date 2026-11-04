//go:build !windows

package stdlibextra

// Unix 专属的 syscall 符号（Windows 上不存在这些标识符）。
//
// 单独成文件的理由见 syscall.go 文件头：不拆开的话
// `GOOS=windows go build` 会报 `undefined: syscall.Kill`。
//
// 这些符号是**进程组控制**的基础：插件 Stop 时要一次带走所有子进程
// （Qt/QQ 会 fork 一大堆），靠的就是 Kill(-pid, SIGTERM/SIGKILL)。

import (
	"reflect"
	"syscall"
)

// unixSyscallSymbols 返回仅在 Unix 系（linux/darwin/bsd）存在的符号。
func unixSyscallSymbols() map[string]reflect.Value {
	return map[string]reflect.Value{
		// 信号：SIGTERM 优雅退出、SIGKILL 强杀。
		"Kill":    reflect.ValueOf(syscall.Kill),
		"SIGTERM": reflect.ValueOf(syscall.SIGTERM),
		"SIGKILL": reflect.ValueOf(syscall.SIGKILL),
		"SIGINT":  reflect.ValueOf(syscall.SIGINT),
		"SIGHUP":  reflect.ValueOf(syscall.SIGHUP),

		// 常用错误值（判断子进程是否被信号杀死时会用到）
		"ECHILD": reflect.ValueOf(syscall.ECHILD),
		"ESRCH":  reflect.ValueOf(syscall.ESRCH),
		"EPERM":  reflect.ValueOf(syscall.EPERM),
		"EINTR":  reflect.ValueOf(syscall.EINTR),
	}
}
