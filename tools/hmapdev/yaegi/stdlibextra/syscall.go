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
)

// ★ 平台边界（2026-11-04 修）：`syscall.Kill` 与 SIGTERM/SIGKILL/SIGINT/SIGHUP
// 在 Windows 上**不存在**（syscall 包在 Windows 上只有 WaitStatus 那一套）。
//
// 此前本文件直接引用它们，导致 `GOOS=windows go build` 报：
//
//	yaegi/stdlibextra/syscall.go:25:38: undefined: syscall.Kill
//
// 结果是 hmapdev 的 windows 交叉编译门长期红着（实测 2026-11-04 那次
// CI：hmapdev (windows/amd64) failure，而 linux/darwin 全绿）。
//
// 拆法：跨平台符号留本文件，Unix 专属符号进 syscall_unix.go（build tag）。
// 不导出符号的**语义**不变 —— Windows 上本来就没有进程组信号，插件
// 在 Windows 上也不会用 Kill(-pid)。

// SyscallSymbols 返回 yaegi 可用的 `syscall` 导出表。
func SyscallSymbols() map[string]map[string]reflect.Value {
	// ★ 不导出 SysProcAttr 类型：它含平台特定字段，yaegi 的 ptrOf
	//   在反射它时会 nil 解引用而**整个进程 panic**（实测）。
	//   这是 yaegi 的已知弱点 —— 复合类型的字段布局无法完整还原。
	//   插件里若需要 Setpgid，应在 _unix 文件里用平台专用代码，
	//   而不是指望调试器能解释这个类型。
	// Windows 上 syscall 只有 WaitStatus 那一套，本表在那边是空的
	// （真实符号由 unixSyscallSymbols 提供，见 syscall_unix.go）。
	pkg := map[string]reflect.Value{}

	// 合并平台专属符号（Unix 有 Kill/SIG*，Windows 为空表）。
	//
	// 为什么要合并而不是让调用方判断 GOOS：插件代码不该关心宿主平台，
	// 它只问"syscall 里有没有 X"。Windows 上缺 Kill 是正确的平台事实。
	for k, v := range unixSyscallSymbols() {
		pkg[k] = v
	}

	return map[string]map[string]reflect.Value{
		"syscall/syscall": pkg,
	}
}
