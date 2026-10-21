package main

import (
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/JianFeeeee/hologramagentsdk/meta"
)

func main() {
	if len(os.Args) < 2 {
		help()
		return
	}
	switch os.Args[1] {
	case "init":
		cmdInit(os.Args[2:])
	case "build":
		cmdBuild(os.Args[2:])
	case "clean":
		cmdClean(os.Args[2:])
	case "debug":
		cmdDebug(os.Args[2:])
	case "sdk":
		cmdSDK(os.Args[2:])
	case "skill":
		cmdSkill(os.Args[2:])
	case "version", "-v", "--version":
		printVersion()
	default:
		help()
	}
}

// printVersion 输出工具链自身的版本身份。
//
// 为何必须有：此前工具链不报版本，而插件产物与内核是**协议绑定**的——
// 手里是哪一版工具链、能不能配当前内核，只能靠翻文件名或猜。
// 版本号来自 meta.Version（与 SDK 发布同源，由 -ldflags -X 注入）；
// lnflags 未注入时它是源码里的默认值，此时提示它可能是开发构建。
func printVersion() {
	printVersionTo(os.Stdout)
}

// printVersionTo 把版本身份写到 w（抽出来是为了能被测试钉住）。
func printVersionTo(w io.Writer) {
	fmt.Fprintf(w, "hmapdev %s\n", meta.Version)
	fmt.Fprintf(w, "  SDK 模块: %s\n", meta.SDKModule)
	if meta.Commit != "" && meta.Commit != "unknown" {
		fmt.Fprintf(w, "  构建提交: %s\n", meta.Commit)
	}
	if meta.BuildTime != "" && meta.BuildTime != "unknown" {
		fmt.Fprintf(w, "  构建时间: %s\n", meta.BuildTime)
	}
	fmt.Fprintf(w, "  构建用 Go: %s\n", runtime.Version())
	fmt.Fprintf(w, "  可执行文件: %s\n", os.Args[0])
}

func help() {
	fmt.Print(`HologramAgent Plugin Dev Tool

Usage:
  hmapdev version                Print toolchain version
  hmapdev init <name>             Scaffold a new plugin project
  hmapdev init <name> --lua       Create Lua plugin
  hmapdev init <name> --type remotedevice
                                    Create C remote device adapter
  hmapdev build [flags]           Compile and package plugin
  hmapdev clean                   Clean build/dist artifacts
  hmapdev debug [dir]             Interpret and debug plugin source
  hmapdev sdk <command>           Manage SDK versions
  hmapdev skill <command>          Install plugin-dev skills into agent skill dirs

Flags:
  --outdir    Output directory (default: dist)
  --target    Target OS/arch (e.g. linux/amd64), repeatable
  --lua       Create Lua plugin (for init)
  --type      Project type: "remotedevice" (for init)
  -t          Alias for --type
`)
}
