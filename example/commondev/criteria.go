package main

// 刀 4：**判据与复核**（`criteria` / `review`）—— 设计文档 §4.5 的落地。
//
// ## 核心主张：复核必须是**两步**（正向 + 变异）
//
// 用户原话：「我定义过单元测试后，对应 agent 完成修改后你直接让插件进行复核即可」
//
// 这里的隐含假设是「插件 GREEN = 改对了」。**该假设不成立** ——
// 插件只能跑主管**预先写下**的判据；判据没覆盖那条 bug，插件照样打勾。
//
// 本仓当日实证（我在 /tmp 复刻过一遍，结论一致）：
//
//	func Max(a,b int) int { if a>b { return a }; return b }
//	松判据只测 Max(5,3) == 5
//	变异 return b → return a（真 bug）⇒ **判据仍全绿**
//	补一条 Max(3,5) == 5 ⇒ 同一变异立刻报红
//
// ⇒ **没有反向阶段，就是在给"测试通过"这个信号不断充值信用，
//   直到它骗一次大的。**
//
// ## 两个阶段
//
//	正向 forward：跑判据，期望**通过**
//	反向 mutate ：改坏被测代码，期望判据**报红**（不报红 ⇒ 判据太松）
//
// ## ★ 实现要点（每条都是踩过的坑）
//
//  1. **必须禁用测试缓存**（`-count=1`）：变异后若命中缓存，测试根本没跑，
//     "报红"就无从谈起 —— 而 Go 默认会缓存。实测复刻时就看到 `(cached)`。
//  2. **变异必须真的改到文件**：改坏→跑→**恢复**，恢复用 defer 保证
//     即使 panic 也不留脏文件（否则工作目录被污染，后续全乱）。
//  3. **恢复失败要响亮**：恢复不回去比变异本身危险得多（agent 会基于
//     坏代码继续改）。
//  4. **判据脚本的"通过"由退出码决定**，不是由输出文本里有没有 "PASS"。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------------- 台账结构 ----------------

// Criteria 是**主管定义的一条判据**。
type Criteria struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	BranchID  string `json:"branch_id,omitempty"` // 留空 = 项目级（所有分支共用）

	Name string `json:"name"`
	// Kind 是判据类型：unit|interface|stress|build。
	Kind string `json:"kind"`

	// Script 是判据本体（shell 命令）。退出码 0 = 通过。
	Script string `json:"script"`
	// Cwd 是执行目录（相对工作目录）。留空 = 工作目录根。
	Cwd string `json:"cwd,omitempty"`
	// TimeoutSec 单次执行上限。默认 300。
	TimeoutSec int `json:"timeout_sec,omitempty"`

	// Expect 描述"通过"的含义（**给人看**，不参与判定）。
	Expect string `json:"expect,omitempty"`

	// Mutations 是**变异规则**（反向阶段用）。
	//
	// 空 = 只做正向。但设计 §4.5 明确：**没有反向就是在给信号充值信用** ——
	// 所以复核结果里会把"无变异规则"标成一个**警告**，而不是静默略过。
	Mutations []Mutation `json:"mutations,omitempty"`

	// DefHash 是定义指纹：判据内容变了它就该变。
	//
	// 用途：① 幂等（重复 define 同一条不产生新记录）
	//      ② 审计（"这次复核跑的是哪一版判据"）
	DefHash string `json:"def_hash"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// Mutation 是一条变异规则：把文件里的 old 换成 new。
type Mutation struct {
	// ID 用于报告（如 "M1"）。
	ID string `json:"id"`
	// File 是相对工作目录的路径。
	File string `json:"file"`
	// Old / New 是字面量替换（**不是正则** —— 正则容易改错地方）。
	Old string `json:"old"`
	New string `json:"new"`
	// Note 说明这条变异想验证什么（"判据是否覆盖了 b>a 的分支"）。
	Note string `json:"note,omitempty"`
}

// ReviewRun 是一次复核执行的结果。
type ReviewRun struct {
	ID        string `json:"id"`
	BranchID  string `json:"branch_id"`
	Phase     string `json:"phase"` // forward | mutate
	Criteria  string `json:"criteria_id"`
	Result    string `json:"result"` // pass | fail | error | skipped
	Detail    string `json:"detail,omitempty"`
	Evidence  string `json:"evidence,omitempty"` // 输出摘要（截断）
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at,omitempty"`
	// HeadSHA 是复核时的提交 —— 判据通过的是**那个提交**，不是一个会移动的目标。
	HeadSHA string `json:"head_sha,omitempty"`
}

// ---------------- 判据定义 ----------------

// criteriaDefHash 计算判据定义的指纹。
//
// 为何要它：① 幂等（同定义重复登记不产生新记录）；
// ② 审计（"这次跑的是哪一版判据"）。变异规则也必须进指纹 ——
// 只改变异规则而判据本体不变时，复核的正确性已经变了。
func criteriaDefHash(c *Criteria) string {
	h := sha256.New()
	for _, part := range []string{c.Name, c.Kind, c.Script, c.Cwd, c.Expect} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	for _, m := range c.Mutations {
		h.Write([]byte(m.ID + "\x01" + m.File + "\x01" + m.Old + "\x01" + m.New))
		h.Write([]byte{0})
	}
	fmt.Fprintf(h, "%d", c.TimeoutSec)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ---------------- 执行 ----------------

// runShell 在工作目录里跑一段命令，返回 (退出码, 合并输出)。
//
// ★ 超时是必须的：判据脚本可能挂住（死循环、等输入）。内核工具调用
// 有 60s 硬限，但这里是**后台作业**（不受它限），所以自己定上限。
func runShell(dir, script string, timeoutSec int) (int, string) {
	if timeoutSec <= 0 {
		timeoutSec = 300
	}
	ctx, cancel := newTimeoutCtx(time.Duration(timeoutSec) * time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1 // 起不来（命令不存在等）
		}
	}
	return code, string(out)
}

// runCriteriaForward 执行一条判据的**正向**阶段。
//
// 返回 (是否通过, 证据摘要)。
func (p *Plugin) runCriteriaForward(c *Criteria, workdir string) (bool, string) {
	dir := workdir
	if c.Cwd != "" {
		dir = filepath.Join(workdir, c.Cwd)
	}
	if _, err := os.Stat(dir); err != nil {
		return false, fmt.Sprintf("执行目录不存在: %s", dir)
	}
	code, out := runShell(dir, c.Script, c.TimeoutSec)
	if code == 0 {
		return true, summarize(out, "通过（退出码 0）")
	}
	return false, summarize(out, fmt.Sprintf("未通过（退出码 %d）", code))
}

// runCriteriaMutate 执行**反向**阶段：逐条变异，期望判据报红。
//
// ★ 这是本刀的核心。设计 §4.5：变异**不报红 ⇒ 判据太松**。
//
// 返回 (全部变异都被咬住, 逐条说明, 是否有严重错误)。
func (p *Plugin) runCriteriaMutate(c *Criteria, workdir string) (bool, string, bool) {
	if len(c.Mutations) == 0 {
		return false, "未定义变异规则 —— **无法判断判据是否太松**。" +
			"设计 §4.5：没有反向阶段，就是在给「测试通过」这个信号充值信用。" +
			"请在 commondev_test_define 里补 mutations", false
	}

	allCaught := true
	var lines []string
	for _, m := range c.Mutations {
		caught, note, hardErr := p.runOneMutation(c, m, workdir)
		if hardErr {
			// 硬错误（改不到文件、恢复失败）—— 必须响亮，不能算"咬住"
			allCaught = false
			lines = append(lines, fmt.Sprintf("  ✗ %s 执行错误: %s", m.ID, note))
			continue
		}
		if caught {
			lines = append(lines, fmt.Sprintf("  ✓ %s 被咬住（判据报红）%s", m.ID, suffixNote(m.Note)))
		} else {
			allCaught = false
			lines = append(lines, fmt.Sprintf("  ✗ %s **未被咬住** —— 判据太松%s", m.ID, suffixNote(m.Note)))
		}
	}
	return allCaught, strings.Join(lines, "\n"), false
}

// runOneMutation 改坏 → 跑判据 → 恢复。返回 (是否被咬住, 说明, 硬错误)。
func (p *Plugin) runOneMutation(c *Criteria, m Mutation, workdir string) (bool, string, bool) {
	if m.File == "" || m.Old == "" {
		return false, "变异规则缺 file 或 old", true
	}
	path := filepath.Join(workdir, m.File)
	orig, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Sprintf("读不到 %s: %v", m.File, err), true
	}
	text := string(orig)

	// ★ Old 必须**恰好出现一次**：出现 0 次 = 变异无效（改不到）；
	// 出现多次 = 可能改错地方（第一个不一定是我们想改的那个）。
	// 两者都必须报错 —— 否则"变异没生效"会被误判成"判据太松"（假警报），
	// 或"改了别处"被误判成"咬住了"（假通过）。
	n := strings.Count(text, m.Old)
	if n == 0 {
		return false, fmt.Sprintf("%s 里找不到要替换的片段（变异无效）: %q", m.File, truncate(m.Old, 60)), true
	}
	if n > 1 {
		return false, fmt.Sprintf("%s 里该片段出现 %d 次（可能改错地方，请给出更长的上下文）: %q",
			m.File, n, truncate(m.Old, 60)), true
	}

	mutated := strings.Replace(text, m.Old, m.New, 1)
	if mutated == text {
		return false, "替换后内容未变化（Old 与 New 相同？）", true
	}

	// ★ 写坏 → 跑 → **无论如何都恢复**（defer 保证 panic 也恢复）
	restore := func() error { return os.WriteFile(path, orig, 0o644) }
	if err := os.WriteFile(path, []byte(mutated), 0o644); err != nil {
		return false, fmt.Sprintf("写变异失败: %v", err), true
	}
	defer func() { _ = restore() }()

	passed, evidence := p.runCriteriaForward(c, workdir)

	// 恢复并**校验**恢复成功
	if err := restore(); err != nil {
		return false, fmt.Sprintf("★ 恢复 %s 失败（工作目录已被污染）: %v", m.File, err), true
	}
	back, err := os.ReadFile(path)
	if err != nil || string(back) != text {
		return false, fmt.Sprintf("★ 恢复 %s 后内容不一致（工作目录已被污染）", m.File), true
	}

	if !passed {
		return true, summarize(evidence, "判据报红（期望行为）"), false
	}
	// 变异了但判据仍通过 ⇒ 这条变异没被咬住 ⇒ 判据在该维度太松
	return false, summarize(evidence, "变异后判据**仍通过**"), false
}

func suffixNote(note string) string {
	if note == "" {
		return ""
	}
	return "（" + note + "）"
}

// summarize 把输出压成一条可读证据（**截断**：判据输出可能很长，
// 全塞进台账会让文件爆炸，而证据的价值在"能看出怎么失败的"）。
func summarize(out, headline string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return headline
	}
	return headline + "\n" + truncate(out, 1500)
}

// ---------------- 工具实现 ----------------

func (p *Plugin) handleTestDefine(args map[string]interface{}) (interface{}, error) {
	proj := strings.TrimSpace(str(args, "project"))
	if proj == "" {
		return map[string]interface{}{"error": "project 必填"}, nil
	}
	name := strings.TrimSpace(str(args, "name"))
	script := str(args, "script")
	if name == "" || strings.TrimSpace(script) == "" {
		return map[string]interface{}{"error": "name 与 script 必填"}, nil
	}
	kind := strings.TrimSpace(str(args, "kind"))
	if kind == "" {
		kind = "unit"
	}
	switch kind {
	case "unit", "interface", "stress", "build":
	default:
		return map[string]interface{}{"error": "kind 只能是 unit|interface|stress|build"}, nil
	}

	c := &Criteria{
		ID:         fmt.Sprintf("cr-%s-%s", safeNamePattern.ReplaceAllString(proj, "_"), shortHash(proj+name+script)),
		ProjectID:  proj,
		BranchID:   strings.TrimSpace(str(args, "branch_id")),
		Name:       name,
		Kind:       kind,
		Script:     script,
		Cwd:        strings.TrimSpace(str(args, "cwd")),
		Expect:     strings.TrimSpace(str(args, "expect")),
		TimeoutSec: int(numArg(args, "timeout_sec")),
		CreatedAt:  nowRFC3339(),
	}
	// 变异规则
	if raw, ok := args["mutations"].([]interface{}); ok {
		for i, item := range raw {
			mm, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			m := Mutation{
				ID:   str(mm, "id"),
				File: str(mm, "file"),
				Old:  str(mm, "old"),
				New:  str(mm, "new"),
				Note: str(mm, "note"),
			}
			if m.ID == "" {
				m.ID = fmt.Sprintf("M%d", i+1)
			}
			if m.File == "" || m.Old == "" || m.New == "" {
				return map[string]interface{}{"error": fmt.Sprintf("变异 %s 缺 file/old/new", m.ID)}, nil
			}
			c.Mutations = append(c.Mutations, m)
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ledger.Criteria == nil {
		p.ledger.Criteria = map[string]*Criteria{}
	}
	// 幂等：同 ID 且定义指纹相同 ⇒ 不动（"可迭代幂等"）
	if prev, exists := p.ledger.Criteria[c.ID]; exists {
		if prev.DefHash == criteriaDefHash(c) {
			return map[string]interface{}{
				"status": "unchanged", "criteria_id": c.ID,
				"note": "同定义已存在，未改动（幂等）",
			}, nil
		}
		c.CreatedAt = prev.CreatedAt
		c.UpdatedAt = nowRFC3339()
	}
	c.DefHash = criteriaDefHash(c)
	p.ledger.Criteria[c.ID] = c
	if err := p.saveLocked(); err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}

	out := map[string]interface{}{
		"status": "ok", "criteria_id": c.ID, "kind": c.Kind,
		"def_hash": c.DefHash, "mutations": len(c.Mutations),
	}
	if len(c.Mutations) == 0 {
		out["warning"] = "未定义变异规则：复核时**无法判断判据是否太松**（设计 §4.5）"
	}
	return out, nil
}

func (p *Plugin) handleTestList(args map[string]interface{}) (interface{}, error) {
	proj := strings.TrimSpace(str(args, "project"))
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []map[string]interface{}{}
	for _, c := range p.ledger.Criteria {
		if proj != "" && c.ProjectID != proj {
			continue
		}
		out = append(out, map[string]interface{}{
			"criteria_id": c.ID, "name": c.Name, "kind": c.Kind,
			"project": c.ProjectID, "branch_id": c.BranchID,
			"mutations": len(c.Mutations), "def_hash": c.DefHash,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["name"].(string) < out[j]["name"].(string)
	})
	return map[string]interface{}{"criteria": out, "count": len(out)}, nil
}

// newTimeoutCtx 是 context.WithTimeout 的薄包装。
func newTimeoutCtx(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// ---------------- 复核执行（后台作业） ----------------

// reviewAll 对一个分支跑**全部**判据的复核，返回汇总。
//
// ★ 这是「主管按一下、插件跑完正反两阶段」的入口。
func (p *Plugin) reviewAll(branchID string) (map[string]interface{}, error) {
	p.mu.Lock()
	b, ok := p.ledger.Branches[branchID]
	var workdir, proj string
	if ok {
		workdir, proj = b.Workdir, b.ProjectID
	}
	// 收集适用判据：分支级优先，项目级兜底
	var list []*Criteria
	for _, c := range p.ledger.Criteria {
		if c.ProjectID != proj {
			continue
		}
		if c.BranchID != "" && c.BranchID != branchID {
			continue
		}
		list = append(list, c)
	}
	p.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("工程对话 %s 不存在", branchID)
	}
	if workdir == "" {
		return nil, fmt.Errorf("该分支没有工作目录")
	}
	if len(list) == 0 {
		return map[string]interface{}{
			"error": fmt.Sprintf("项目 %s 没有定义任何判据 —— 先 commondev_test_define", proj),
		}, nil
	}
	// ★ 固定顺序（map 遍历随机）—— 复核报告要可对照
	sort.Slice(list, func(i, j int) bool {
		if list[i].Kind != list[j].Kind {
			return list[i].Kind < list[j].Kind
		}
		return list[i].Name < list[j].Name
	})

	head, _ := runGitOut(workdir, "rev-parse", "HEAD")
	head = strings.TrimSpace(head)

	forwardOK := true
	mutateOK := true
	anyWithoutMutations := false
	var report []map[string]interface{}

	for _, c := range list {
		fwdPass, fwdEv := p.runCriteriaForward(c, workdir)
		if !fwdPass {
			forwardOK = false
		}
		mutCaught, mutEv, _ := p.runCriteriaMutate(c, workdir)
		if len(c.Mutations) == 0 {
			anyWithoutMutations = true
		} else if !mutCaught {
			mutateOK = false
		}

		item := map[string]interface{}{
			"criteria_id": c.ID, "name": c.Name, "kind": c.Kind,
			"forward_pass":     fwdPass,
			"forward_evidence": truncate(fwdEv, 800),
			"mutations_total":  len(c.Mutations),
			"mutations_caught": mutCaught,
		}
		if len(c.Mutations) > 0 {
			item["mutate_evidence"] = truncate(mutEv, 1200)
		} else {
			item["mutate_evidence"] = mutEv
		}
		report = append(report, item)

		// 写复核历史（可审计：哪次复核、跑的是哪一版判据、在哪个提交上）
		p.appendReviewRun(branchID, c.ID, "forward", fwdPass, fwdEv, head)
		if len(c.Mutations) > 0 {
			p.appendReviewRun(branchID, c.ID, "mutate", mutCaught, mutEv, head)
		}
	}

	// ★ 结论三条并列，**不合并成一个绿色勾**：
	//   正向绿 + 反向没跑 ≠ 复核通过（那是"信号充值"）
	verdict := "pass"
	notes := []string{}
	if !forwardOK {
		verdict = "fail"
		notes = append(notes, "有判据未通过（实现有 bug）")
	}
	if anyWithoutMutations {
		notes = append(notes, "★ 有判据未定义变异规则 ⇒ 无法判断它是否太松（设计 §4.5）")
		if verdict == "pass" {
			verdict = "pass_with_gaps"
		}
	}
	if !mutateOK {
		notes = append(notes, "★ 有变异**未被咬住** ⇒ 对应判据太松，实现可能有问题而判据看不见")
		if verdict == "pass" {
			verdict = "pass_with_gaps"
		}
	}

	out := map[string]interface{}{
		"verdict":      verdict, // pass | pass_with_gaps | fail
		"forward_ok":   forwardOK,
		"mutate_ok":    mutateOK,
		"head_sha":     shortSHA(head),
		"criteria_run": len(report),
		"report":       report,
	}
	if len(notes) > 0 {
		out["notes"] = notes
	}
	if verdict == "pass_with_gaps" {
		out["warning"] = "正向全绿，但**反向阶段有缺口** —— " +
			"这不是「通过」，是「判据没覆盖到」。插件 GREEN ≠ 改对了。"
	}
	return out, nil
}

// appendReviewRun 记一条复核历史。
func (p *Plugin) appendReviewRun(branchID, criteriaID, phase string, pass bool, evidence, head string) {
	res := "fail"
	if pass {
		res = "pass"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ledger.ReviewRuns = append(p.ledger.ReviewRuns, ReviewRun{
		ID:       fmt.Sprintf("rv-%d", time.Now().UnixNano()%1000000000),
		BranchID: branchID, Criteria: criteriaID, Phase: phase,
		Result: res, Evidence: truncate(evidence, 500),
		StartedAt: nowRFC3339(), EndedAt: nowRFC3339(),
		HeadSHA: head,
	})
	// 只保留最近 500 条（防台账无限膨胀；复核历史是审计用，不需要全量）
	if n := len(p.ledger.ReviewRuns); n > 500 {
		p.ledger.ReviewRuns = p.ledger.ReviewRuns[n-500:]
	}
	_ = p.saveLocked()
}

// handleReviewSubmit 是 commondev_review_submit 的实现。
//
// ★ 复核可能跑很久（判据里可能有编译、压测）⇒ 走**后台作业**，
// 与 branch_open 的 clone 同理（内核工具调用有 60s 硬限）。
func (p *Plugin) handleReviewSubmit(args map[string]interface{}) (interface{}, error) {
	branchID := strings.TrimSpace(str(args, "branch_id"))
	if branchID == "" {
		return map[string]interface{}{"error": "branch_id 必填"}, nil
	}
	p.mu.Lock()
	if _, ok := p.ledger.Branches[branchID]; !ok {
		p.mu.Unlock()
		return map[string]interface{}{"error": fmt.Sprintf("工程对话 %s 不存在", branchID)}, nil
	}
	jobID := fmt.Sprintf("review-%s-%d", branchID, time.Now().UnixNano()%100000)
	p.ledger.Jobs[jobID] = &Job{
		ID: jobID, BranchID: branchID, Kind: "review",
		Status: "pending", StartedAt: nowRFC3339(),
	}
	p.saveLocked()
	p.mu.Unlock()

	go func() {
		p.setJob(jobID, "running", "正在复核（正向 + 变异）", "")
		res, err := p.reviewAll(branchID)
		if err != nil {
			p.setJob(jobID, "failed", "", err.Error())
			return
		}
		if e, isErr := res["error"]; isErr {
			p.setJob(jobID, "failed", "", fmt.Sprint(e))
			return
		}
		verdict := fmt.Sprint(res["verdict"])
		detail := fmt.Sprintf("复核结论: %s", verdict)
		if notes, ok := res["notes"].([]string); ok && len(notes) > 0 {
			detail += " | " + strings.Join(notes, "；")
		}
		p.setJob(jobID, "done", detail, "")
		p.lastReview = res
	}()

	return map[string]interface{}{
		"status": "ok", "job_id": jobID, "branch_id": branchID,
		"next": "复核在后台进行（判据可能含编译/压测，超过 60s 工具上限）——" +
			"用 commondev_job_status 查进度；用 commondev_review_result 取结论",
	}, nil
}

// handleReviewResult 取最近一次复核结论。
func (p *Plugin) handleReviewResult(args map[string]interface{}) (interface{}, error) {
	branchID := strings.TrimSpace(str(args, "branch_id"))
	p.mu.Lock()
	res := p.lastReview
	// 也把历史摘要带上（审计）
	var hist []map[string]interface{}
	for _, r := range p.ledger.ReviewRuns {
		if branchID != "" && r.BranchID != branchID {
			continue
		}
		hist = append(hist, map[string]interface{}{
			"id": r.ID, "phase": r.Phase, "criteria": r.Criteria,
			"result": r.Result, "head_sha": shortSHA(r.HeadSHA), "at": r.StartedAt,
		})
	}
	p.mu.Unlock()

	if res == nil && len(hist) == 0 {
		return map[string]interface{}{"error": "还没有复核记录（先 commondev_review_submit）"}, nil
	}
	// 只给最近 40 条（防刷屏）
	if len(hist) > 40 {
		hist = hist[len(hist)-40:]
	}
	out := map[string]interface{}{"history": hist, "history_count": len(hist)}
	if res != nil {
		out["last"] = res
	}
	return out, nil
}

// ---------------- 单阶段复核工具（设计 §4.1） ----------------
//
// 设计 §4.1 把复核拆成两个工具：
//
//	commondev_test_run     执行判据集合（正向）
//	commondev_test_mutate  执行变异验证（反向）★ 必须有
//
// 而 commondev_review_submit 是**两者一起跑**（主管按一下就跑完）。
// 三种粒度都保留，因为排查时需要单跑某一阶段：
// "正向都过不了" 与 "正过但变异咬不住" 是**完全不同**的问题。

// criteriaFor 收集适用于某分支的判据（分支级优先，项目级兜底）。
func (p *Plugin) criteriaFor(branchID, proj string) []*Criteria {
	p.mu.Lock()
	var list []*Criteria
	for _, c := range p.ledger.Criteria {
		if c.ProjectID != proj {
			continue
		}
		if c.BranchID != "" && c.BranchID != branchID {
			continue
		}
		list = append(list, c)
	}
	p.mu.Unlock()
	sort.Slice(list, func(i, j int) bool {
		if list[i].Kind != list[j].Kind {
			return list[i].Kind < list[j].Kind
		}
		return list[i].Name < list[j].Name
	})
	return list
}

// lookupBranchWorkdir 取分支与其项目（找不到时给明确错误）。
func (p *Plugin) lookupBranchWorkdir(branchID string) (*Branch, *Project, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, ok := p.ledger.Branches[branchID]
	if !ok {
		return nil, nil, fmt.Errorf("工程对话 %s 不存在", branchID)
	}
	proj := p.ledger.Projects[b.ProjectID]
	if proj == nil {
		return nil, nil, fmt.Errorf("分支 %s 的项目 %s 不存在", branchID, b.ProjectID)
	}
	if strings.TrimSpace(b.Workdir) == "" {
		return nil, nil, fmt.Errorf("该分支没有工作目录（先 commondev_branch_open）")
	}
	return b, proj, nil
}

// handleTestRun 只跑**正向**阶段。
func (p *Plugin) handleTestRun(args map[string]interface{}) (interface{}, error) {
	branchID := strings.TrimSpace(str(args, "branch_id"))
	if branchID == "" {
		return map[string]interface{}{"error": "branch_id 必填"}, nil
	}
	b, proj, err := p.lookupBranchWorkdir(branchID)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	list := p.criteriaFor(branchID, proj.Name)
	if len(list) == 0 {
		return map[string]interface{}{"error": fmt.Sprintf("项目 %s 没有定义判据（先 commondev_test_define）", proj.Name)}, nil
	}
	allPass, report := true, []map[string]interface{}{}
	for _, c := range list {
		pass, ev := p.runCriteriaForward(c, b.Workdir)
		if !pass {
			allPass = false
		}
		report = append(report, map[string]interface{}{
			"criteria_id": c.ID, "name": c.Name, "kind": c.Kind,
			"pass": pass, "evidence": truncate(ev, 800),
		})
	}
	out := map[string]interface{}{
		"phase": "forward", "all_pass": allPass,
		"criteria_run": len(report), "report": report,
	}
	if !allPass {
		out["error"] = "有判据未通过（实现有 bug）"
	}
	// ★ 提醒：正向过了也不代表改对了
	out["warning"] = "正向通过 ≠ 改对了 —— 还要跑 commondev_test_mutate" +
		"（变异不报红 ⇒ 判据太松，见设计 §4.5）"
	return out, nil
}

// handleTestMutate 只跑**反向**阶段。
func (p *Plugin) handleTestMutate(args map[string]interface{}) (interface{}, error) {
	branchID := strings.TrimSpace(str(args, "branch_id"))
	if branchID == "" {
		return map[string]interface{}{"error": "branch_id 必填"}, nil
	}
	b, proj, err := p.lookupBranchWorkdir(branchID)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	list := p.criteriaFor(branchID, proj.Name)
	if len(list) == 0 {
		return map[string]interface{}{"error": fmt.Sprintf("项目 %s 没有定义判据", proj.Name)}, nil
	}
	allCaught, withGaps, report := true, []string{}, []map[string]interface{}{}
	for _, c := range list {
		if len(c.Mutations) == 0 {
			allCaught = false
			withGaps = append(withGaps, c.Name)
			report = append(report, map[string]interface{}{
				"criteria_id": c.ID, "name": c.Name,
				"mutations_total": 0, "all_caught": false,
				"evidence": "★ 未定义变异规则 ⇒ 无法判断判据是否太松",
			})
			continue
		}
		caught, ev, _ := p.runCriteriaMutate(c, b.Workdir)
		if !caught {
			allCaught = false
		}
		report = append(report, map[string]interface{}{
			"criteria_id": c.ID, "name": c.Name,
			"mutations_total": len(c.Mutations), "all_caught": caught,
			"evidence": truncate(ev, 1200),
		})
	}
	out := map[string]interface{}{
		"phase": "mutate", "all_caught": allCaught,
		"criteria_run": len(report), "report": report,
	}
	if len(withGaps) > 0 {
		out["criteria_without_mutations"] = withGaps
	}
	if !allCaught {
		out["warning"] = "★ 有变异**未被咬住** ⇒ 判据太松，实现可能有问题而判据看不见"
	}
	return out, nil
}
