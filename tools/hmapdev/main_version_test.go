package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/JianFeeeee/hologramagentsdk/meta"
)

// 工具链必须能报出自己的版本。
//
// 为什么值得钉住：插件产物与内核是协议绑定的，「手里是哪一版工具链」直接决定
// 产物能不能建链；此前既没有 version 子命令，`-ldflags -X meta.Version` 也因为
// meta 包没被链接而**静默无效**（表现为报不出任何版本）。
func TestPrintVersionReportsInjectedVersion(t *testing.T) {
	origVersion, origCommit := meta.Version, meta.Commit
	defer func() { meta.Version, meta.Commit = origVersion, origCommit }()

	// 模拟 -ldflags 注入后的取值
	meta.Version = "9.9.9"
	meta.Commit = "deadbee"

	var buf bytes.Buffer
	printVersionTo(&buf)
	out := buf.String()

	if !strings.Contains(out, "9.9.9") {
		t.Fatalf("版本号未出现在输出里（-X 注入会失效）:\n%s", out)
	}
	if !strings.Contains(out, "hmapdev") {
		t.Fatalf("输出里没有工具名:\n%s", out)
	}
	if !strings.Contains(out, "deadbee") {
		t.Fatalf("提交号未出现在输出里:\n%s", out)
	}
	// ★ 断言必须**跟随 meta.SDKModule**，不能白名单几个历史名字面量。
	//
	// 反例（2026-10-29 实测）：这里曾写死 homeagent-sdk / homeagentsdk，
	// 而模块名推进到 hologramagentsdk 后，"hologramagentsdk" 并不包含
	// "homeagentsdk" 子串（多了 hologram 前缀）⇒ 测试红。
	// 而真正该钉的是"输出里有当前 SDK 模块名"，不是"某几个历史名之一"。
	if !strings.Contains(out, meta.SDKModule) {
		t.Fatalf("输出里没有当前 SDK 模块标识 %q:\n%s", meta.SDKModule, out)
	}
}

// 未注入时（源码默认值）也必须能报——否则开发构建和发版构建长得一样。
func TestPrintVersionWorksWithoutInjection(t *testing.T) {
	origCommit, origBuildTime := meta.Commit, meta.BuildTime
	defer func() { meta.Commit, meta.BuildTime = origCommit, origBuildTime }()
	meta.Commit, meta.BuildTime = "unknown", "unknown"

	var buf bytes.Buffer
	printVersionTo(&buf)
	out := buf.String()
	if !strings.Contains(out, meta.Version) {
		t.Fatalf("未注入时应报出源码默认版本 %q:\n%s", meta.Version, out)
	}
	if strings.Contains(out, "unknown") {
		t.Fatalf("unknown 字段不应出现在输出里（噪声）:\n%s", out)
	}
}
