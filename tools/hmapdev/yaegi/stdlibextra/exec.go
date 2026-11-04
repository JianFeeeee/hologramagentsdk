// os/exec 包的补丁符号（最小集）。
//
// # 为什么只导出函数，不导出类型
//
// yaegi v0.16.1 在反射 `exec.Cmd` 这类复合类型时会 **nil 解引用直接 panic**
// （实测：interp.ptrOf → type.go:235）。原因是 Cmd 含 func 字段、
// chan、平台相关结构，yaegi 的类型还原做不了。
//
// 但**函数指针**没问题：`exec.Command` 返回值在解释器里当作不透明值传递，
// 只要调用方能接着调 `.Output()` / `.Run()`，yaegi 就能通过反射执行到
// 真实方法 —— 这部分走的不是类型还原，是 method 调用。
//
// 所以：声明函数、不声明类型，既能满足 `import "os/exec"`，
// 又不会触发那段崩溃路径。副作用是解释器内不能写 `&exec.Cmd{}` 字面量，
// 但插件里没有人这么写（都是 exec.Command 构造）。
package stdlibextra

import (
	"os/exec"
	"reflect"
)

// ExecSymbols 返回 yaegi 可用的 `os/exec` 导出表（仅函数与哨兵错误）。
func ExecSymbols() map[string]map[string]reflect.Value {
	pkg := map[string]reflect.Value{
		"Command":        reflect.ValueOf(exec.Command),
		"CommandContext": reflect.ValueOf(exec.CommandContext),
		"LookPath":       reflect.ValueOf(exec.LookPath),
		"ErrNotFound":    reflect.ValueOf(&exec.ErrNotFound).Elem(),
	}

	return map[string]map[string]reflect.Value{
		"os/exec/exec": pkg,
	}
}
