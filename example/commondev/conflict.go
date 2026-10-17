package main

// 刀 3：多项目/并行下的 **conflict 预检** 与 **base_sha 记录**。
//
// ## 为什么需要这一刀
//
// 设计 §4.2：`base_sha` 是用户原描述里**缺失**的一环，但必须补：
//
//	没有它，插件无法判断「两个 PR 改了同一文件时第二个该不该 rebase、
//	能不能自动合」，出问题也无法重放。
//
// 并行开发（§4.4）让这个问题从"可能"变成"必然"：每分支一份独立 clone，
// 两棵树**互不可见**，冲突不会在开发时暴露，而是全部堆到 merge 时爆发。
// 那时才发现"两个分支改了同一个文件的同一段"就太晚了 ——
// 复审已经通过、判据已经变绿，但合不进去。
//
// ## 做法：在**提 PR 之前**算一次可合并性
//
// 用 `git merge-tree --write-tree` —— ★ 它**完全不碰工作区**（不写索引、
// 不动 HEAD、不留 MERGE_HEAD），所以可以在任何时候安全地跑，
// 不需要先 stash/checkout，也不会破坏正在进行的开发。
//
// 退出码语义（实测 git 2.53）：
//
//	0 → 可干净合并
//	1 → 有冲突
//	其它 → 命令本身失败（仓库损坏等），**不能当成"无冲突"**
//
// ## 与旧写法（`git merge-tree <base> <b1> <b2>`）的对比
//
// 旧语法输出是给人看的文本，要靠 grep `CONFLICT` 判断 —— 脆弱且没有
// 明确的退出码。新语法（git 2.38+）直接给树 OID 与退出码。
// 本仓的环境是 git 2.53，用新语法；同时保留旧语法的兜底判断。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ConflictReport 是一次冲突预检的结果。
type ConflictReport struct {
	// Mergeable 是否可干净合并。
	Mergeable bool `json:"mergeable"`
	// Conflicts 是冲突文件列表（可为空）。
	Conflicts []string `json:"conflicts,omitempty"`
	// BaseSHA / HeadSHA 是本次比较的两端（用于审计与重放）。
	BaseSHA string `json:"base_sha,omitempty"`
	HeadSHA string `json:"head_sha,omitempty"`
	// Target 是被合入的目标（通常是 main）。
	Target string `json:"target"`
	// Detail 是给人看的一行说明。
	Detail string `json:"detail"`
	// Skipped 为真表示预检被跳过（而非"检查通过了"）。
	//
	// ★ 这个字段很关键：把"没检查"和"检查通过"混为一谈，
	// 会让调用方以为安全 —— 而实际什么都没验。
	Skipped bool `json:"skipped,omitempty"`
}

// ────────────────────────── base_sha 记录 ──────────────────────────

// captureBaseSHA 记录工作目录当前 HEAD（开线时的基点）。
//
// 必须在**第一次投递任务之前**调用：agent 一旦开始改，HEAD 就不再是基点了。
// 拿不到时如实返回错误（不静默留空 —— 空 base_sha 会让后续冲突判定失据）。
func (p *Plugin) captureBaseSHA(branchID, workdir string) (string, error) {
	sha, err := runGitOut(workdir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("取 base_sha 失败: %w", err)
	}
	sha = strings.TrimSpace(sha)
	// 空值守卫：实测 `git rev-parse HEAD` 要么成功给非空 sha、要么报错，
	// 所以这条**实际不可达** —— 保留它是防御性的，不必为它写判据
	//（写了会是"永远绿"的假判据，反而掩盖覆盖率的真实情况）。
	if sha == "" {
		return "", fmt.Errorf("取 base_sha 得到空值")
	}
	p.mu.Lock()
	if b, ok := p.ledger.Branches[branchID]; ok {
		// 只记第一次：重开同一分支时基点不该被后续改动覆盖
		// （覆盖会让"这条分支从哪出发"永久丢失，冲突判定与重放都失去依据）
		if b.BaseSHA == "" {
			b.BaseSHA = sha
		} else {
			sha = b.BaseSHA
		}
	}
	p.mu.Unlock()
	return sha, nil
}

// ────────────────────────── 冲突预检 ──────────────────────────

// checkConflict 在**不动工作区**的前提下判断 branch 能否干净地合进 target。
//
// 两种实现路径，自动选择：
//
//	① 本地有该分支的 workdir ⇒ 直接在工作目录里跑 merge-tree（最快，无需网络）
//	② 否则 ⇒ 在 mirror 里跑（mirror 是 --mirror clone，含全部分支）
//
// ★ 为什么不在 PR 之后靠平台判定：那时已经推了分支、开了 PR，
//
//	冲突才暴露 → 白跑一轮。预检的意义就是**在浪费工作之前发现**。
func (p *Plugin) checkConflict(workdir, target string) ConflictReport {
	rep := ConflictReport{Target: target}
	if target == "" {
		target = "main"
		rep.Target = target
	}

	// 本地要有 target 的最新视角：mirror 的 fetch 由调用方保证
	// （这里只做只读判定，不引入网络依赖 —— 预检失败不该阻断流程）

	// 取 HEAD（本次要合入的提交）
	head, err := runGitOut(workdir, "rev-parse", "HEAD")
	if err != nil {
		rep.Skipped = true
		rep.Detail = "取 HEAD 失败，预检跳过（不等于无冲突）: " + err.Error()
		return rep
	}
	rep.HeadSHA = strings.TrimSpace(head)

	// 目标分支在本地存在吗？不存在就没法预检（如实标记 Skipped）
	if _, err := runGitOut(workdir, "rev-parse", "--verify", target); err != nil {
		rep.Skipped = true
		rep.Detail = fmt.Sprintf(
			"本地没有 %s 引用，预检跳过（不等于无冲突）。"+
				"先 fetch 目标分支再预检，或直接在平台上开 PR 让平台判定", target)
		return rep
	}

	base, _ := runGitOut(workdir, "merge-base", target, "HEAD")
	rep.BaseSHA = strings.TrimSpace(base)

	// ★ 核心：merge-tree --write-tree（不碰工作区）
	out, err := runGitOut(workdir, "merge-tree", "--write-tree", target, "HEAD")
	if err == nil {
		rep.Mergeable = true
		rep.Detail = fmt.Sprintf("可干净合并进 %s（基点 %s）", target, shortSHA(rep.BaseSHA))
		return rep
	}

	// 非零退出：区分「冲突」与「命令失败」。
	//
	// ★ 这个区分极其重要：把命令失败当成"有冲突"会误报（让人去做无谓 rebase），
	//   把它当成"无冲突"则更危险（带着冲突去合）。两者都不行 ⇒ 分别处理。
	if isMergeTreeConflict(out) {
		rep.Mergeable = false
		rep.Conflicts = parseConflictFiles(out)
		rep.Detail = fmt.Sprintf("与 %s 有冲突（%d 个文件）：%s",
			target, len(rep.Conflicts), strings.Join(rep.Conflicts, ", "))
		return rep
	}

	// 命令失败（如 git 版本不支持 --write-tree）⇒ 退回旧语法再判一次
	if rep2, ok := p.checkConflictLegacy(workdir, target); ok {
		rep2.BaseSHA, rep2.HeadSHA = rep.BaseSHA, rep.HeadSHA
		return rep2
	}

	rep.Skipped = true
	rep.Detail = "merge-tree 不可用（git 过旧？），预检跳过（不等于无冲突）: " +
		truncate(strings.TrimSpace(out), 200)
	return rep
}

// checkConflictLegacy 用旧版 merge-tree 语法兜底（git < 2.38）。
//
// 旧语法输出是文本，靠 `CONFLICT` / 冲突标记判断 —— 脆弱，故只作兜底。
func (p *Plugin) checkConflictLegacy(workdir, target string) (ConflictReport, bool) {
	rep := ConflictReport{Target: target}
	base, err := runGitOut(workdir, "merge-base", target, "HEAD")
	if err != nil {
		return rep, false
	}
	out, _ := runGitOut(workdir, "merge-tree", strings.TrimSpace(base), target, "HEAD")
	if !strings.Contains(out, "CONFLICT") && !strings.Contains(out, "<<<<<<<") {
		rep.Mergeable = true
		rep.Detail = fmt.Sprintf("可干净合并进 %s（旧语法判定）", target)
		return rep, true
	}
	rep.Mergeable = false
	rep.Conflicts = parseConflictFilesLegacy(out)
	rep.Detail = fmt.Sprintf("与 %s 有冲突（旧语法判定，%d 个文件）",
		target, len(rep.Conflicts))
	return rep, true
}

// isMergeTreeConflict 判断 merge-tree 的失败是"有冲突"还是"命令本身失败"。
//
// 依据：git 在冲突时退出码为 1；命令错误（如未知参数）通常是 129 等。
// `runGitOut` 把 stderr 一起并进 out，所以同时看退出码语义与输出特征。
func isMergeTreeConflict(out string) bool {
	if strings.Contains(out, "CONFLICT") {
		return true
	}
	// 冲突时 merge-tree 会打印 "Auto-merging ... CONFLICT (content)"，
	// 或至少列出冲突阶段号（1/2/3 的 ls-tree 形态）
	return strings.Contains(out, "Auto-merging") ||
		strings.Contains(out, "Merge conflict")
}

// parseConflictFiles 从 `merge-tree --write-tree` 输出里提文件名。
//
// 输出形态（实测 git 2.53）：
//
//	<tree-oid>
//	100644 <oid> 1\tpath/to/file      ← 阶段 1/2/3 即为冲突条目
//	100644 <oid> 2\tpath/to/file
//	100644 <oid> 3\tpath/to/file
//	...
//	Auto-merging path
//	CONFLICT (content): Merge conflict in path
func parseConflictFiles(out string) []string {
	set := map[string]bool{}

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		// 形态 ①：明确的 "CONFLICT ... in <path>"
		if i := strings.Index(line, "CONFLICT"); i >= 0 {
			if j := strings.Index(line, " in "); j > 0 {
				set[strings.TrimSpace(line[j+4:])] = true
			}
			continue
		}
		// 形态 ②：ls-tree 阶段行 "100644 <oid> <stage>\t<path>"
		if rest, ok := strings.CutPrefix(line, "100"); ok {
			_ = rest
			parts := strings.SplitN(line, "\t", 2)
			if len(parts) == 2 {
				fields := strings.Fields(parts[0])
				// mode, oid, stage
				if len(fields) == 3 && (fields[2] == "1" || fields[2] == "2" || fields[2] == "3") {
					set[strings.TrimSpace(parts[1])] = true
				}
			}
		}
	}

	out2 := make([]string, 0, len(set))
	for f := range set {
		out2 = append(out2, f)
	}
	sort.Strings(out2) // 稳定输出（map 顺序随机，回执要可对照）
	return out2
}

// parseConflictFilesLegacy 从旧语法输出里提文件名（形态 "CONFLICT ... in <path>"）。
func parseConflictFilesLegacy(out string) []string {
	set := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, "CONFLICT"); i >= 0 {
			if j := strings.Index(line, " in "); j > 0 {
				set[strings.TrimSpace(line[j+4:])] = true
			}
		}
	}
	r := make([]string, 0, len(set))
	for f := range set {
		r = append(r, f)
	}
	sort.Strings(r)
	return r
}

// shortSHA 截短 sha 供人阅读（空值原样返回）。
func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// ────────────────────────── 多项目/并行盘点 ──────────────────────────

// parallelSummary 汇总「当前有哪些分支线在并行推进」。
//
// 为何要它：并行是有前提的（设计 §8.4）—— 并行的必须是**互不相干的模块**。
// 若两条线改了同一文件，冲突会堆到 merge 时才爆发。这个汇总让主管
// **在派活之前**就能看到"眼下有哪几条线、各自在哪"，从而判断该不该并行。
func (p *Plugin) parallelSummary() map[string]interface{} {
	p.mu.Lock()
	defer p.mu.Unlock()

	byProject := map[string][]map[string]interface{}{}
	for _, b := range p.ledger.Branches {
		if b.Status != "open" && b.Status != "review" {
			continue
		}
		byProject[b.ProjectID] = append(byProject[b.ProjectID], map[string]interface{}{
			"branch_id": b.ID,
			"branch":    b.Name,
			"status":    b.Status,
			"agent":     b.AgentName,
			"workdir":   b.Workdir,
			"base_sha":  shortSHA(b.BaseSHA),
		})
	}

	projects := make([]string, 0, len(byProject))
	for k := range byProject {
		projects = append(projects, k)
	}
	sort.Strings(projects)

	out := make([]map[string]interface{}, 0, len(projects))
	total := 0
	for _, proj := range projects {
		lines := byProject[proj]
		total += len(lines)
		out = append(out, map[string]interface{}{
			"project":  proj,
			"count":    len(lines),
			"branches": lines,
		})
	}
	return map[string]interface{}{
		"projects":       out,
		"total_branches": total,
		"warning": "并行只适合**互不相干的模块**。若多条线改了同一批文件，" +
			"冲突会堆到 merge 时才爆发 —— 派活前先确认边界（设计 §8.4）。",
	}
}

// distinctWorkdirs 返回所有活跃分支的工作目录（供批量操作/清理）。
func (p *Plugin) distinctWorkdirs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, b := range p.ledger.Branches {
		if b.Workdir == "" || seen[b.Workdir] {
			continue
		}
		if _, err := os.Stat(filepath.Join(b.Workdir, ".git")); err != nil {
			continue
		}
		seen[b.Workdir] = true
		out = append(out, b.Workdir)
	}
	sort.Strings(out)
	return out
}

// handleConflictCheck 是 commondev_conflict_check 的实现。
func (p *Plugin) handleConflictCheck(args map[string]interface{}) (interface{}, error) {
	id := strings.TrimSpace(str(args, "branch_id"))
	p.mu.Lock()
	b, ok := p.ledger.Branches[id]
	var workdir, base string
	if ok {
		workdir, base = b.Workdir, b.BaseSHA
	}
	proj := ""
	if ok {
		if pr, ok2 := p.ledger.Projects[b.ProjectID]; ok2 {
			proj = pr.DefaultBranch
		}
	}
	p.mu.Unlock()
	if !ok {
		return map[string]interface{}{"error": fmt.Sprintf("工程对话 %s 不存在", id)}, nil
	}
	target := strings.TrimSpace(str(args, "target"))
	if target == "" {
		target = proj
	}
	if target == "" {
		target = "main"
	}
	if workdir == "" {
		return map[string]interface{}{"error": "该分支没有工作目录（目录准备未完成？）"}, nil
	}

	rep := p.checkConflict(workdir, target)
	out := map[string]interface{}{
		"mergeable": rep.Mergeable,
		"target":    rep.Target,
		"detail":    rep.Detail,
		"base_sha":  shortSHA(base), // 开线时记下的基点（审计用）
	}
	if len(rep.Conflicts) > 0 {
		out["conflicts"] = rep.Conflicts
		out["advice"] = "先在本分支 rebase/合并目标分支并解决冲突，再提 PR。" +
			"★ 不要带冲突去开 PR —— 平台会拒绝，且白跑一轮判据。"
	}
	if rep.Skipped {
		out["skipped"] = true
		out["warning"] = "预检被跳过 —— 这**不等于**没有冲突。"
	}
	return out, nil
}

// handleParallelStatus 是 commondev_parallel_status 的实现。
func (p *Plugin) handleParallelStatus(args map[string]interface{}) (interface{}, error) {
	return p.parallelSummary(), nil
}
