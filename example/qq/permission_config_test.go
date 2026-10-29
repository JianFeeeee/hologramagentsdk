package main

import (
	"strings"
	"testing"
)

// 权限门必须可配置（用户反馈：写死导致正常交流被打断）。
// 这三条钉住"配置真的生效"，而不是只多了三个配置项。

// 硬性私人工具清单：默认等价旧行为；清空后不再拦；可自定义。
func TestHardPrivateToolsConfigurable(t *testing.T) {
	p := &Plugin{hardPrivateTools: parseToolAllowlist(defaultHardPrivateTools), hardPrivateToolsSet: true}
	// 旧行为等价性：这些默认被拦
	for _, name := range []string{"memory_recall", "terminal_create", "settings_set", "run_command"} {
		if !p.isHardPrivateTool(name) {
			t.Errorf("默认应拦 %s", name)
		}
	}
	// 不在默认硬清单里的工具，是否可用完全交给白名单（这正是"可配置"的意义）
	for _, name := range []string{"qq_download_file", "files_read", "cmd_run"} {
		if p.isHardPrivateTool(name) {
			t.Errorf("%s 不该在默认硬私有清单里（否则部署者加了白名单也放不开）", name)
		}
	}

	// 清空 = 完全不拦（交给 allowlist 决定）
	p.hardPrivateTools = nil
	if p.isHardPrivateTool("memory_recall") {
		t.Error("清空 hard_private_tools 后不应再硬拦")
	}

	// 自定义：只拦 files_*
	p.hardPrivateTools = parseToolAllowlist(`["files_*"]`)
	if !p.isHardPrivateTool("files_write") || p.isHardPrivateTool("memory_recall") {
		t.Error("自定义清单未生效")
	}
}

// 「只能读当前会话」的参数级限制已**移除**（2026-10-29，用户裁定：
// 允许访问所有会话）。本判据改为钉住"它真的没了"：
//
//  1. 代码里不得再有 sessionToolArgsAllowed 或 session_scope_enforce
//     —— 否则等于限制以另一种形式回来了，而运维会以为配置还能开
//  2. 仍保留的安全边界必须还在（档位声明 + 工具白名单），
//     避免"删限制时顺手把人删了"
//
// 为何用"源码扫描"而不是行为判据：被删的函数已不存在，
// 没有可调用的入口；而"它是否复活"恰恰是源码级事实。
func TestSessionScopeRestrictionRemoved(t *testing.T) {
	src := strings.Join(codeLines(t, "plugin.go"), "\n")

	for _, gone := range []string{
		"sessionToolArgsAllowed", // 参数级会话范围检查
		"session_scope_enforce",  // 它的配置项
		"sessionScopeUnlimited",  // 它的开关字段
	} {
		if strings.Contains(src, gone) {
			t.Errorf("★ %q 仍存在于 plugin.go —— 会话范围限制应已移除", gone)
		}
	}

	// 仍然必须保留的安全边界（删限制≠删防护）
	for _, kept := range []string{
		"declareSecurityProfiles", // 档位声明（私人工具限制交给内核）
		"currentToolAllowed",      // 群/私聊工具白名单
		"maxDuplicateSend",        // 重复发送保险
	} {
		if !strings.Contains(src, kept) {
			t.Errorf("★ %q 不应被删 —— 它是与会话范围无关的独立防护", kept)
		}
	}
}

// 群 @ 门：代码里必须有 groupRequireAt 分支，且 require_at 关闭时不 return。
func TestGroupRequireAtIsConfigurable(t *testing.T) {
	joined := strings.Join(codeLines(t, "plugin.go"), "\n")
	i := strings.Index(joined, "if !isAt {")
	if i < 0 {
		t.Fatal("找不到 @bot 判断")
	}
	branch := ifBlockAt(joined, i)
	if !strings.Contains(branch, "groupRequireAtOff") {
		t.Fatal("★ @ 门没有读 group_require_at 配置 —— 仍然是写死的")
	}
	if !strings.Contains(branch, "return") {
		t.Fatal("★ 默认路径（require_at=true）必须 return，否则群里每条消息都会响应")
	}
}
