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

// session_scope_enforce=false：非所有者也能按 ID 读其它会话。
func TestSessionScopeSwitch(t *testing.T) {
	auth := qqAuthContext{Active: true, UserID: 100, MessageID: 1}
	args := map[string]interface{}{"user_id": float64(200)}

	p := &Plugin{name: "qq"}
	if ok, _ := p.sessionToolArgsAllowed("qq_get_history", args, auth); ok {
		t.Error("开启时非所有者读别的会话应被拒")
	}
	p.sessionScopeUnlimited = true
	if ok, _ := p.sessionToolArgsAllowed("qq_get_history", args, auth); !ok {
		t.Error("关闭后应放行")
	}
}

// 群 @ 门：代码里必须有 groupRequireAt 分支，且 require_at 关闭时不 return。
func TestGroupRequireAtIsConfigurable(t *testing.T) {
	joined := strings.Join(codeLines(t, "plugin.go"), "\n")
	i := strings.Index(joined, "if !p.isAtBot(evt.Message) {")
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
