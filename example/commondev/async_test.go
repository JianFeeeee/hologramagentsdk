package main

import (
	"os"
	"strings"
	"testing"
)

// ★ 判据：慢操作（clone/fetch/测试）**绝不能**出现在工具 handler 的同步路径里。
//
// 依据（实测踩到）：内核把每次工具调用硬限制在 **60 秒**
// （internal/agent/core/toolcall.go:62，硬编码无配置项）。同步 clone 的后果是
// 工具报超时、但 mirror 其实已建好 —— 调用方只看到失败，状态却是"做了一半"。
//
// 这条判据用**源码文本**做检查（同本仓既有做法：判据类检查必须先剔除注释）。
func TestNoSyncCloneInToolHandlers(t *testing.T) {
	src, err := os.ReadFile("plugin.go")
	if err != nil {
		t.Fatal(err)
	}
	// 剔除注释，避免注释里提到 runGit 也会命中
	body := stripComments(string(src))

	// 工具 handler 的区间：从 handleProjectAdd 到 handleJobStatus 之前
	start := strings.Index(body, "func (p *Plugin) handleProjectAdd")
	end := strings.Index(body, "func (p *Plugin) handleJobStatus")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("找不到 handler 区间（函数名可能已改）")
	}
	handlers := body[start:end]

	// handleBranchOpen 的**后台函数**是允许慢操作的，单独排除
	if i := strings.Index(handlers, "func (p *Plugin) runPrepareWorkdir"); i >= 0 {
		handlers = handlers[:i]
	}

	for _, bad := range []string{"runGit(", "prepareWorkdir("} {
		if strings.Contains(handlers, bad) {
			t.Errorf("工具 handler 里出现慢操作 %s —— 会撞 60s 工具上限；应转后台（见 runPrepareWorkdir）", bad)
		}
	}
	// 但后台函数自己必须用到它（否则重构把功能删了）
	if !strings.Contains(body, "func (p *Plugin) runPrepareWorkdir") ||
		!strings.Contains(body, "prepareWorkdir(pr, branch, workdir)") {
		t.Error("runPrepareWorkdir 未实现或未调用 prepareWorkdir —— 目录准备功能丢了")
	}
	// ★ 且必须切到目标分支（“每个目录下是独立仓库的独立分支”——用户原话）
	//
	// 判据必须检查**两条路径都切**：新建路径（clone 后）与复用路径（目录已存在）。
	// 为何要查两处：初版只查字符串存在，而 ensureOnBranch 的**定义**本身
	// （func … ensureOnBranch(branch, workdir string)）就包含该子串 —— 于是
	// 只删调用点、定义还在，判据仍然全绿（变异实验当场发现了这个漏洞）。
	// ⇒ 改为数**调用点**个数（排除定义行）。
	calls := 0
	for _, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "func ") {
			continue // 定义行不算
		}
		if strings.Contains(t, "p.ensureOnBranch(branch, workdir)") {
			calls++
		}
	}
	if calls < 2 {
		t.Errorf("prepareWorkdir 切分支的调用点只有 %d 处（=new建路径+复用路径）；"+
			"缺一处就会让目录停在默认分支（main）", calls)
	}
}

// stripComments 去掉 // 行注释与 /* */ 块注释。
//
// 为何必须先剔除：本仓既有教训 —— 注释里模仿真实签名会污染一切按文本匹配的判据。
func stripComments(s string) string {
	var out strings.Builder
	lines := strings.Split(s, "\n")
	inBlock := false
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if inBlock {
			if idx := strings.Index(ln, "*/"); idx >= 0 {
				inBlock = false
				ln = ln[idx+2:]
			} else {
				out.WriteString("\n")
				continue
			}
		}
		if strings.HasPrefix(trimmed, "/*") {
			if idx := strings.Index(ln, "*/"); idx >= 0 {
				ln = ln[:strings.Index(ln, "/*")] + ln[idx+2:]
			} else {
				inBlock = true
				out.WriteString("\n")
				continue
			}
		}
		if i := strings.Index(ln, "//"); i >= 0 {
			ln = ln[:i]
		}
		out.WriteString(ln + "\n")
	}
	return out.String()
}
