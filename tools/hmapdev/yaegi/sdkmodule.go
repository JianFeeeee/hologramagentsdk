package yaegi

import "strings"

// sdkModuleNames 是 HologramAgent SDK 的 module path：**当前名 + 迁移前旧名**。
//
// # 为何必须两个都认（2026-10-08）
//
// SDK 仓已从 gitcode 迁到 GitHub，module 路径变成
// `github.com/JianFeeeee/homeagentsdk`；但**存量插件的 go.mod 里写的仍是旧路径**
// `gitcode.com/JianFeeeee/homeagent-sdk`（本机实测 qq / vanblog / mcquery /
// fileproc / mail-bridge 五个插件工作区全是旧名）。用户已建的项目不会因为
// 我们改了仓就自动更新。
//
// 本包的 findSDKGoPath 要从插件 go.mod 的 replace 里推出 SDK 源码根目录。
// 只认新名 ⇒ 旧插件查不到路径 ⇒ 退化成「找不到 SDK」的兜底分支，
// 而那是**静默**的：没有报错，只是行为不再是预期那条。
var sdkModuleNames = []string{
	// 当前名（本仓 go.mod 已改名）
	"github.com/JianFeeeee/hologramagentsdk",
	// 改名前的 GitHub 名（存量插件的 go.mod 里写的仍是它）
	"github.com/JianFeeeee/homeagentsdk",
	// 迁移前的 gitcode 名
	"gitcode.com/JianFeeeee/homeagent-sdk",
	// gitcode 已同步改名后的路径
	"gitcode.com/JianFeeeee/hologramagentsdk",
}

// isSDKModule 报告 s 是否含 SDK 模块名。
func isSDKModule(s string) bool {
	for _, n := range sdkModuleNames {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}
