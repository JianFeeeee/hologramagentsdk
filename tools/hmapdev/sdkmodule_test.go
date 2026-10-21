package main

import "testing"

// sdkModule 识别必须**同时认新旧两个 module 名**。
//
// # 为何这条判据重要（2026-10-08）
//
// SDK 仓从 gitcode 迁到 GitHub 后 module 路径变成
// `github.com/JianFeeeee/hologramagentsdk`。我（AI）做迁移时只替换了 import 路径，
// **漏了用旧名做逻辑判定的地方**——`cmd_build.go` 用
// `strings.Contains(mod, "homeagent-sdk")` 判断「这是不是 SDK 依赖」，
// 迁移后恒为假。
//
// 后果是**静默**的：不写 replace、认不出 SDK 仓，没有任何报错。
// 这类失效只有实测才会暴露，而当时没有任何测试覆盖这三个判定点。
//
// 存量插件的 go.mod 里写的仍是旧名（本机实测 qq / vanblog / mcquery /
// fileproc / mail-bridge 五个工作区全是 `gitcode.com/...`），
// 所以**旧名必须继续被认**，不能简单换名字。
func TestSDKModulePathAcceptsBothNames(t *testing.T) {
	cases := []struct {
		mod  string
		want bool
	}{
		// 当前名
		{"github.com/JianFeeeee/hologramagentsdk", true},
		{"github.com/JianFeeeee/hologramagentsdk/sdk", true},
		// 迁移前旧名（存量插件仍在用，必须继续认）
		{"gitcode.com/JianFeeeee/homeagent-sdk", true},
		{"gitcode.com/JianFeeeee/homeagent-sdk/sdk", true},
		{"gitcode.com/JianFeeeee/homeagent-sdk v1.3.0", true},
		// 不应误判
		{"", false},
		{"github.com/JianFeeeee/HologramAgent", false}, // 核心仓，不是 SDK
		{"github.com/other/sdk", false},
		{"  ", false},
	}
	for _, c := range cases {
		if got := isSDKModule(c.mod); got != c.want {
			t.Errorf("isSDKModule(%q) = %v, want %v\n"+
				"  旧名漏认会让存量插件静默走错分支（不写 replace / 找不到 SDK 路径）",
				c.mod, got, c.want)
		}
	}
}

func TestContainsSDKModuleAcceptsBothNames(t *testing.T) {
	if !containsSDKModule("module foo\nrequire github.com/JianFeeeee/hologramagentsdk v0.0.0\n") {
		t.Error("containsSDKModule 必须认当前名")
	}
	if !containsSDKModule("require gitcode.com/JianFeeeee/homeagent-sdk v0.0.0\n") {
		t.Error("containsSDKModule 必须认旧名（存量插件 go.mod 仍是旧名）")
	}
	if containsSDKModule("module foo\n\ngo 1.21\n") {
		t.Error("不含 SDK 依赖的 go.mod 不应被误认为 SDK 仓")
	}
}
