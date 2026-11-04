// os/exec 包的补丁符号。
//
// # 为什么需要
//
// yaegi v0.16.1 的 stdlib.Symbols 里**没有 os/exec**，任何调用外部命令的
// 插件（TTS/ffmpeg/pactl 这类很常见）在 debug 时报
//
//	eval xxx.go: import "os/exec" error: unable to find source related to: "os/exec"
//
// build 走真实 Go 工具链不受影响，所以只有 debug 暴露，看起来像插件写错了。
//
// # 为什么只导出函数，不导出类型
//
// 插件里 `*exec.Cmd` 作为**结构体字段**时，光注册符号是不够的：yaegi 需要
// 完整的类型定义，而 os/exec 源码依赖 internal/* 等解释器不支持的包。
// 那种情况由 interp.go 的 execStubSource（源码级桩）处理，不走这里。
//
// 这里只负责让 `import "os/exec"` 成立、函数调用可解析。
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
