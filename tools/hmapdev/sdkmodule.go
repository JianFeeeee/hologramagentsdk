package main

import "strings"

// sdkModuleNames 是「什么算 SDK 模块」的唯一事实源：**三个名字都认**。
//
//  1. `gitcode.com/JianFeeeee/homeagent-sdk`   —— 最初（gitcode 时代）
//  2. `github.com/JianFeeeee/homeagentsdk`     —— 2026-10-08 迁到 GitHub
//  3. `github.com/JianFeeeee/hologramagentsdk` —— 2026-10 产品改名
//
// ❗旧名必须继续被认出：存量插件的 go.mod 里写的仍是 1 或 2（本机实测仍有
// 工作区在用第 2 个）。只认新名 ⇒ 旧插件被当成「这不是 SDK 依赖」⇒
// **静默**走错分支（不写 replace、认不出 SDK 仓、yaegi 找不到 SDK 源码路径）。
var sdkModuleNames = []string{
	"github.com/JianFeeeee/hologramagentsdk",
	"github.com/JianFeeeee/homeagentsdk",
	"gitcode.com/JianFeeeee/homeagent-sdk",
}

// sdkModulePath 报告 mod 是否为 HologramAgent SDK 的 module path，
// 是则返回命中的那个规范名。
//
// # 为何要同时认三个名字
//
// 见 sdkModuleNames 的注释。要点：每次改名都留下一批「go.mod 里写着旧名」的
// 存量插件工作区（用户已创建的项目不可能因为我们改了仓就自动更新）。
//
// 这类失效**不报错**，正是最危险的一种：迁移代码路径改完了，
// 逻辑判定漏改，功能悄悄坏掉而测试仍然绿。
//
// 为何用 Contains 而不是 ==：module 行可能带版本号后缀
// （`gitcode.com/.../v1.3.0`）或是 require 块内的裸写法。
func sdkModulePath(mod string) (string, bool) {
	mod = strings.TrimSpace(mod)
	if mod == "" {
		return "", false
	}
	for _, name := range sdkModuleNames {
		if strings.Contains(mod, name) {
			return name, true
		}
	}
	return "", false
}

// isSDKModule 是 sdkModulePath 的布尔形式（调用方不关心规范名时用）。
func isSDKModule(mod string) bool {
	_, ok := sdkModulePath(mod)
	return ok
}

// containsSDKModule 报告一段文本（如 go.mod 全文）里是否出现 SDK module 名。
//
// 与 isSDKModule 分开：那个判「单个 module path 是不是 SDK」，
// 这个判「整份文件里有没有引用 SDK」——用途不同，别合并。
func containsSDKModule(text string) bool {
	for _, name := range sdkModuleNames {
		if strings.Contains(text, name) {
			return true
		}
	}
	return false
}
