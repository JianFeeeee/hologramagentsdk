// syscall 包的补丁符号。
//
// 与 os/exec 同类问题：yaegi v0.16.1 的 stdlib 不含 syscall。
// 而插件经常需要它做进程组控制（Setpgid / Kill(-pid)）——
// 这是"Stop 时能一次带走所有子进程"的基础，Qt/QQ 会 fork 一大堆。
//
// 只导出**平台无关且常见**的少数符号，不追求覆盖整个 syscall：
// 全量导出既庞大又容易在不同 GOOS 间冲突，而插件实际只用到这几个。
package stdlibextra

import (
	"reflect"
	"syscall"
)

// SyscallSymbols 返回 yaegi 可用的 `syscall` 导出表。
func SyscallSymbols() map[string]map[string]reflect.Value {
	// ★ 不导出 SysProcAttr 类型：它含平台特定字段，yaegi 的 ptrOf
	//   在反射它时会 nil 解引用而**整个进程 panic**（实测）。
	//   这是 yaegi 的已知弱点 —— 复合类型的字段布局无法完整还原。
	//   插件里若需要 Setpgid，应在 _unix 文件里用平台专用代码，
	//   而不是指望调试器能解释这个类型。
	pkg := map[string]reflect.Value{
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

	return map[string]map[string]reflect.Value{
		"syscall/syscall": pkg,
	}
}
