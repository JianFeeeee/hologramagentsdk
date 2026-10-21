package yaegi

import "strings"

// sdkModuleNames 是 HologramAgent SDK 的 module path：**当前名 + 两代迁移前旧名**。
//
// # 为何必须两个都认（2026-10-08）
//
// 历史上改过两次名（gitcode → github/homeagentsdk → github/hologramagentsdk），
// **存量插件的 go.mod 里写的仍是旧名**
// `gitcode.com/JianFeeeee/homeagent-sdk`（本机实测 qq / vanblog / mcquery /
// fileproc / mail-bridge 五个插件工作区全是旧名）。用户已建的项目不会因为
// 我们改了仓就自动更新。
//
// 本包的 findSDKGoPath 要从插件 go.mod 的 replace 里推出 SDK 源码根目录。
// 只认新名 ⇒ 旧插件查不到路径 ⇒ 退化成「找不到 SDK」的兜底分支，
// 而那是**静默**的：没有报错，只是行为不再是预期那条。
var sdkModuleNames = []string{
	"github.com/JianFeeeee/hologramagentsdk",
	"github.com/JianFeeeee/homeagentsdk", // 中间一代（存量插件仍在用）
	"gitcode.com/JianFeeeee/homeagent-sdk",
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
