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
	// 判据跟着改名走：用 meta.SDKModule 而不是写死字面量，
	// 否则下一次改名这里会变成「永远为真的坏断言」或误报。
	if !strings.Contains(out, meta.SDKModule) {
		t.Fatalf("输出里没有 SDK 模块标识:\n%s", out)
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
