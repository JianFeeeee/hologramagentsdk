package main

// agent 生命周期与孤儿目录清理。
//
// 这两件事**与 git 平台无关**，所以留在插件里（2026-10-15 架构调整时确认）：
//   · agent 状态/停止 —— 属 pi 运行时会话管理
//   · 工作目录孤儿清理 —— 属本插件的目录映射纪律，且含凭据残留检查

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// handleAgentState 是 commondev_agent_state 的实现。
func (p *Plugin) handleAgentState(args map[string]interface{}) (interface{}, error) {
	id := strings.TrimSpace(str(args, "branch_id"))
	if id == "" {
		return map[string]interface{}{"error": "branch_id 必填"}, nil
	}
	p.mu.Lock()
	b, ok := p.ledger.Branches[id]
	sc := p.sidecars[id]
	p.mu.Unlock()
	if !ok {
		return map[string]interface{}{"error": fmt.Sprintf("工程对话 %s 不存在", id)}, nil
	}
	alive := sc != nil && sc.Alive()
	out := map[string]interface{}{
		"branch_id": id, "status": b.Status, "runtime": b.Runtime,
		"agent_cli":  cliOrPi(b.AgentCLI),
		"session_id": b.SessionID, "input_ch": b.InputCh, "output_ch": b.OutputCh,
		"agent_alive": alive,
	}
	// ★ runtime=ssh 时必须回显**打到哪台机器**。
	//
	// 不回显的话，一条远端线的状态看着和本地线一模一样，
	// 排查时无法区分"任务跑在本机"还是"跑到 10.0.0.9 上了" ——
	// 而这两者的现象（哪些文件被改、日志在哪）完全不同。
	if b.Runtime == "ssh" {
		out["ssh_host"] = b.SSHHost
	}
	if !alive {
		out["note"] = "sidecar 未在运行（进程已退出或从未拉起）—— 用 commondev_agent_up 拉起"
	}
	return out, nil
}

// handleAgentDown 是 commondev_agent_down 的实现（幂等收尾）。
//
// 与 branch_close 的区别：**只杀进程、不动工作目录、不改分支状态**。
// 用途是「暂停这条线但保留现场」，而 branch_close 是「这条线完事了」。
func (p *Plugin) handleAgentDown(args map[string]interface{}) (interface{}, error) {
	id := strings.TrimSpace(str(args, "branch_id"))
	if id == "" {
		return map[string]interface{}{"error": "branch_id 必填"}, nil
	}
	p.mu.Lock()
	b, ok := p.ledger.Branches[id]
	sc := p.sidecars[id]
	delete(p.sidecars, id)
	if ok {
		// ★ 清掉通道与 session 绑定：留着会让人以为进程还在。
		// 工作目录与 status **不动** —— 那是「暂停」，不是「收尾」。
		b.SessionID, b.InputCh, b.OutputCh = "", "", ""
		_ = p.saveLocked()
	}
	p.mu.Unlock()
	if !ok {
		return map[string]interface{}{"error": fmt.Sprintf("工程对话 %s 不存在", id)}, nil
	}
	stopped := sc != nil
	if sc != nil {
		sc.Close()
	}
	return map[string]interface{}{
		"status": "ok", "branch_id": id, "stopped": stopped,
		"note": "已停进程；工作目录与分支状态保留（要彻底收尾用 commondev_branch_close）",
	}, nil
}

// handleWorkdirClean 是 commondev_workdir_clean 的实现。
//
// 设计 §7.2：内置一道**凭据残留检查** —— 任务跑完删目录后，clone 里可能
// 仍留着 token（remote URL 带 token 是常见做法，`git remote -v` 就能读出来）。
//
// dry_run 默认为真：清理不可逆，默认先让人看清楚要删什么。
func (p *Plugin) handleWorkdirClean(args map[string]interface{}) (interface{}, error) {
	orphans, err := p.listOrphanDirs()
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	dryRun := true
	if v, ok := args["dry_run"].(bool); ok {
		dryRun = v
	}

	// 凭据残留检查：扫每个孤儿目录的 git config
	type residue struct {
		Dir   string `json:"dir"`
		Issue string `json:"issue"`
	}
	var residues []residue
	for _, name := range orphans {
		dir := filepath.Join(p.programRoot(), name)
		if issue := gitConfigCredentialResidue(dir); issue != "" {
			residues = append(residues, residue{Dir: name, Issue: issue})
		}
	}

	if dryRun {
		return map[string]interface{}{
			"status": "dry_run", "orphans": orphans, "count": len(orphans),
			"credential_residue": residues,
			"hint":               "确认无误后用 dry_run=false 实际删除",
		}, nil
	}

	removed := []string{}
	for _, name := range orphans {
		dir := filepath.Join(p.programRoot(), name)
		if err := os.RemoveAll(dir); err != nil {
			return map[string]interface{}{"error": fmt.Sprintf("删除 %s 失败: %v", name, err)}, nil
		}
		removed = append(removed, name)
	}
	return map[string]interface{}{
		"status": "ok", "removed": removed, "count": len(removed),
		"credential_residue": residues,
	}, nil
}

// gitConfigCredentialResidue 检查 .git/config 里是否嵌了凭据（设计 §7.2）。
//
// 判据：remote URL 里出现 `://user:pass@` 或 `://<token>@` 形态。
// 为何直接读文件而不是 `git remote -v`：不依赖 git 可执行、且不会因
// 目录已损坏而失败。
func gitConfigCredentialResidue(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "url = ") {
			continue
		}
		u := strings.TrimPrefix(line, "url = ")
		if hasInlineCredential(u) {
			return "remote URL 内嵌凭据（设计 §7.2 禁止）：" + stripTokenFromURL(u)
		}
	}
	return ""
}

// hasInlineCredential 报告 URL 是否含 `scheme://userinfo@host` 形态的凭据。
//
// ⚠ scp 形态（`git@github.com:o/r.git`）的 user 不是凭据，不能误报 ——
// 那是 SSH 用户名，把用户正常的 ssh remote 报成"凭据残留"会让人不敢用清理功能。
func hasInlineCredential(u string) bool {
	i := strings.Index(u, "://")
	if i < 0 {
		return false // scp 形态：git@host:path
	}
	rest := u[i+3:]
	at := strings.Index(rest, "@")
	if at < 0 {
		return false
	}
	return rest[:at] != ""
}
