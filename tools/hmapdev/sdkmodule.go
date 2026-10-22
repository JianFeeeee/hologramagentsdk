package main

import "strings"

// sdkModulePath 报告 mod 是否为 HologramAgent SDK 的 module path，
// 是则返回规范化的模块名。
//
// # 为何要同时认两个名字（2026-10-08）
//
// SDK 仓已从 gitcode 迁到 GitHub，module 路径随之变成
// `github.com/JianFeeeee/homeagentsdk`。但**存量插件的 go.mod 里写的仍是旧路径**
// `gitcode.com/JianFeeeee/homeagent-sdk` —— 本机实测 qq / vanblog / mcquery /
// fileproc / homeagent-mail-bridge 五个插件工作区全部如此（用户已创建的项目
// 不可能因为我们改了仓就自动更新）。
//
// 所以任何「识别 SDK 模块」的判定都必须**两种都接受**：
// 只认新名 ⇒ 旧插件被当成「这不是 SDK 依赖」⇒ 静默走错分支
// （不写 replace、认不出 SDK 仓、yaegi 找不到 SDK 源码路径）。
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
	for _, name := range []string{
		"github.com/JianFeeeee/homeagentsdk",
		"gitcode.com/JianFeeeee/homeagent-sdk",
	} {
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
	for _, name := range []string{
		"github.com/JianFeeeee/homeagentsdk",
		"gitcode.com/JianFeeeee/homeagent-sdk",
	} {
		if strings.Contains(text, name) {
			return true
		}
	}
	return false
}
