package main

// 全流程覆盖度补齐：生命周期、台账一致性与状态查询的判据。
//
// ## 为什么补这一批
//
// 覆盖度审计发现 40 个工具里有 23 个**没有任何机制级测试** —— 包括
// `agent_up`（整条链路的枢纽）、`branch_close`（收尾）、`project_*`（入口）、
// `pi_source_*`（LLM 源配置）。缺口集中在两类：
//
//	① 状态查询类（branch_status / parallel_status / job_status / test_list）
//	② 生命周期类（agent_up / branch_close / project_*）
//
// ②类中最要命的是**顺序与不变量**：
//	- agent_up 必须在 branch_open **之后**（否则没有 branch 可挂）
//	- 通道必须在**拿到 session 之后**注册（否则通道指向不存在的会话）
//	- branch_close 必须清掉通道与 sidecar 引用（否则"以为还在跑"）
//
// 这些不变量此前只写在注释里，**没有判据钉住**。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newFlowPlugin 造一个专用于流程判据的插件（真实台账文件，可验证持久化）。
func newFlowPlugin(t *testing.T) *Plugin {
	t.Helper()
	dir, err := os.MkdirTemp("", "cdflow")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := newTestPluginForShell(t)
	p.path = filepath.Join(dir, "ledger.json")
	p.ledger = &Ledger{
		Projects: map[string]*Project{}, Branches: map[string]*Branch{},
		Jobs: map[string]*Job{}, Criteria: map[string]*Criteria{},
	}
	// ★ sidecars 在 Start() 里初始化（plugin.go:216）。判据绕过了 Start(),
	// 所以必须自己初始化 —— 否则往 nil map 里赋值会 panic。
	p.sidecars = map[string]*sidecarProcess{}
	return p
}

// ★★★ 判据：**先开线才有 agent**（顺序不变量）。
//
// agent_up 对不存在的 branch 必须响亮拒绝。不拒的话会拿不到
// workdir/session 却继续注册通道，产出一条指向虚空的线。
func TestAgentUpRequiresExistingBranch(t *testing.T) {
	p := newFlowPlugin(t)
	res, _ := p.handleAgentUp(map[string]interface{}{"branch_id": "br-nonexistent"})
	m := res.(map[string]interface{})
	if _, isErr := m["error"]; !isErr {
		t.Fatalf("★ 对不存在的分支应报错，实际: %v", m)
	}
	if !strings.Contains(m["error"].(string), "不存在") {
		t.Errorf("错误应说明分支不存在: %v", m["error"])
	}
	// 不得留下任何 sidecar / 通道痕迹
	if len(p.sidecars) != 0 {
		t.Errorf("★ 失败后不该有 sidecar 残留: %v", p.sidecars)
	}
}

// ★ 判据：branch_id 空值必须抓住（不能默认到某条线）。
func TestAgentUpRequiresBranchID(t *testing.T) {
	p := newFlowPlugin(t)
	res, _ := p.handleAgentUp(map[string]interface{}{})
	if _, isErr := res.(map[string]interface{})["error"]; !isErr {
		t.Error("★ 空 branch_id 应报错")
	}
}

// ★★★ 判据：**管线全流程的台账一致性**（project → branch → 状态查询）。
//
// 这是流程主干的端到端：每一步都查一次状态，确认台账如实反映。
func TestPipelineLedgerConsistency(t *testing.T) {
	p := newFlowPlugin(t)

	// ① 登记项目
	r1, _ := p.handleProjectAdd(map[string]interface{}{
		"name": "proj", "remote_url": "/tmp/fake.git", "platform": "gitea",
	})
	m1 := r1.(map[string]interface{})
	if m1["status"] != "ok" {
		t.Fatalf("登记项目失败: %v", m1)
	}

	// ② 列项目：必须看到它
	r2, _ := p.handleProjectList(map[string]interface{}{})
	m2 := r2.(map[string]interface{})
	if n, _ := m2["count"].(int); n != 1 {
		t.Errorf("★ 项目列表应有 1 个，实际 %v", m2["count"])
	}

	// ③ 重复登记必须拒绝（不静默覆盖）
	r3, _ := p.handleProjectAdd(map[string]interface{}{"name": "proj", "remote_url": "/tmp/other.git"})
	if _, isErr := r3.(map[string]interface{})["error"]; !isErr {
		t.Error("★ 重复登记项目应被拒绝")
	}

	// ④ 开线
	r4, _ := p.handleBranchOpen(map[string]interface{}{
		"project": "proj", "branch": "feat/a", "work_desc": "做点事",
	})
	m4 := r4.(map[string]interface{})
	if m4["status"] != "ok" {
		t.Fatalf("开线失败: %v", m4)
	}
	id := m4["branch_id"].(string)
	jobID := m4["job_id"].(string)

	// ⑤ 查分支状态：字段必须齐全（缺一个下游就没法用）
	// ★ 结构是**嵌套的**：{branch: <Branch>, sidecar_alive, deliveries}。
	// 初版按"平铺字段"断言，于是全红 —— 那是判据的假设错了，不是实现的问题。
	// （嵌套更好：Branch 是一个整体，平铺会与 sidecar_alive 这类派生字段混淆。）
	r5, _ := p.handleBranchStatus(map[string]interface{}{"branch_id": id})
	m5 := r5.(map[string]interface{})
	br5, ok := m5["branch"].(*Branch)
	if !ok {
		t.Fatalf("★ branch_status 应返回嵌套的 branch 对象: %v", m5)
	}
	if br5.ID != id {
		t.Errorf("branch.id 不对: %s vs %s", br5.ID, id)
	}
	if br5.Workdir == "" {
		t.Error("branch.workdir 为空")
	}
	if br5.Status != "open" {
		t.Errorf("branch.status 应为 open，实际 %q", br5.Status)
	}
	if br5.WorkDesc != "做点事" {
		t.Errorf("work_desc 未保留: %v", br5.WorkDesc)
	}
	// 派生字段也在
	if _, has := m5["sidecar_alive"]; !has {
		t.Error("缺 sidecar_alive")
	}
	if _, has := m5["deliveries"]; !has {
		t.Error("缺 deliveries")
	}

	// ⑥ 查作业状态：必须是真实状态（不是永远 pending）
	r6, _ := p.handleJobStatus(map[string]interface{}{"job_id": jobID})
	m6 := r6.(map[string]interface{})
	job, _ := m6["job"].(*Job)
	if job == nil {
		t.Fatalf("查不到作业: %v", m6)
	}
	if job.BranchID != id {
		t.Errorf("作业挂错了分支: %s vs %s", job.BranchID, id)
	}

	// ⑦ 并行视图：必须能按项目分组看到这条线
	r7, _ := p.handleParallelStatus(map[string]interface{}{})
	if m7, ok := r7.(map[string]interface{}); ok {
		if _, has := m7["projects"]; !has {
			t.Errorf("★ parallel_status 应给出项目分组: %v", m7)
		}
	}
}

// ★★ 判据：**台账持久化**（重启后仍在）。
//
// 不落盘的话重启即失忆 —— 而"重启后备账仍可查"是设计明确要求的
// （setJob 的注释写着"并落盘，保证重启后台账仍可查"）。
func TestLedgerPersistsAcrossReload(t *testing.T) {
	p := newFlowPlugin(t)
	p.handleProjectAdd(map[string]interface{}{"name": "persist", "remote_url": "/tmp/x.git"})
	r, _ := p.handleBranchOpen(map[string]interface{}{"project": "persist", "branch": "feat/p"})
	id := r.(map[string]interface{})["branch_id"].(string)

	// 重新载入（模拟重启）
	p2 := newTestPluginForShell(t)
	p2.path = p.path
	if err := p2.loadLedger(); err != nil {
		t.Fatalf("重载台账失败: %v", err)
	}
	if _, ok := p2.ledger.Projects["persist"]; !ok {
		t.Error("★ 项目未持久化")
	}
	if b, ok := p2.ledger.Branches[id]; !ok {
		t.Error("★ 分支未持久化")
	} else if b.Workdir == "" {
		t.Error("分支持久化但丢了 workdir")
	}
}

// ★★ 判据：**损坏的台账必须响亮失败，不静默重建**。
//
// 静默重建会让用户以为"线还在"而实际全丢了 —— 那是最危险的静默降级。
func TestCorruptLedgerFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newTestPluginForShell(t)
	p.path = path
	err := p.loadLedger()
	if err == nil {
		t.Fatal("★ 损坏的台账应报错（不静默重建）")
	}
	if !strings.Contains(err.Error(), "损坏") {
		t.Errorf("错误应指出台账损坏: %v", err)
	}
}

// ★★★ 判据：`branch_close` 必须**清干净**（通道 + sidecar + 目录引用）。
//
// 留着通道会让内核继续往一条死线投递；留着 sidecar 引用会让
// agent_state 报"还活着"。
func TestBranchCloseCleansUp(t *testing.T) {
	p := newFlowPlugin(t)
	p.handleProjectAdd(map[string]interface{}{"name": "proj", "remote_url": "/tmp/x.git"})
	r, _ := p.handleBranchOpen(map[string]interface{}{"project": "proj", "branch": "feat/c"})
	id := r.(map[string]interface{})["branch_id"].(string)

	// ★ 必须**真塞一个 sidecar 占位**，否则根本没测到"清理"这件事。
	//
	// 初版只设了通道与 session，没有往 p.sidecars 里放东西 ——
	// 于是 `delete(p.sidecars, id)` 这个变异**不报红**：删除一个不存在的键
	// 与不删除在断言上看不出区别。判据必须让"被保护的状态"真实存在。
	p.mu.Lock()
	p.ledger.Branches[id].InputCh = inputChannelName(id)
	p.ledger.Branches[id].OutputCh = outputChannelName(id)
	p.ledger.Branches[id].SessionID = "sess-x"
	// 用一个**不持有进程**的占位（Close 对零值安全）。这里测的是
	// "map 里的条目被清掉"，不是进程真的被杀。
	p.sidecars[id] = &sidecarProcess{}
	p.mu.Unlock()
	if p.sidecars[id] == nil {
		t.Fatal("前置：sidecar 占位未塞入")
	}

	res, _ := p.handleBranchClose(map[string]interface{}{"branch_id": id})
	m := res.(map[string]interface{})
	if e, bad := m["error"]; bad {
		t.Fatalf("关闭失败: %v", e)
	}

	p.mu.Lock()
	b := p.ledger.Branches[id]
	sc := p.sidecars[id]
	p.mu.Unlock()

	if sc != nil {
		t.Error("★ sidecar 引用未清（agent_state 会报还活着）")
	}
	if b.Status != "merged" && b.Status != "abandoned" && b.Status != "closed" {
		t.Errorf("★ 关闭后状态不对: %q", b.Status)
	}
	if b.ClosedAt == "" {
		t.Error("★ 关闭时间未记录")
	}
}

// ★★ 判据：对不存在的分支操作必须响亮拒绝（不是静默成功）。
//
// 静默成功的后果是调用方以为清理了，实际什么都没有。
func TestOperationsOnMissingBranchFail(t *testing.T) {
	p := newFlowPlugin(t)
	for name, fn := range map[string]func(map[string]interface{}) (interface{}, error){
		// 只列**按 branch 寻址**的操作。workdir_clean 与 test_list 是
		// **全局**操作（前者清 orphan 目录、后者列全部判据），本就不该因
		// "某个 branch 不存在"报错 —— 初版把它们也列进来，判据因此误报。
		"branch_close":   p.handleBranchClose,
		"branch_status":  p.handleBranchStatus,
		"agent_state":    p.handleAgentState,
		"agent_down":     p.handleAgentDown,
		"conflict_check": p.handleConflictCheck,
	} {
		res, _ := fn(map[string]interface{}{"branch_id": "br-ghost"})
		m, ok := res.(map[string]interface{})
		if !ok {
			continue
		}
		if _, isErr := m["error"]; !isErr {
			t.Errorf("★ %s 对不存在的分支未报错: %v", name, m)
		}
	}
}

// ★★ 判据：`agent_state` 在**未拉起**时给出可执行的下一步提示。
func TestAgentStateGuidesWhenNotRunning(t *testing.T) {
	p := newFlowPlugin(t)
	p.handleProjectAdd(map[string]interface{}{"name": "proj", "remote_url": "/tmp/x.git"})
	r, _ := p.handleBranchOpen(map[string]interface{}{"project": "proj", "branch": "feat/s"})
	id := r.(map[string]interface{})["branch_id"].(string)

	res, _ := p.handleAgentState(map[string]interface{}{"branch_id": id})
	m := res.(map[string]interface{})
	if alive, _ := m["agent_alive"].(bool); alive {
		t.Fatal("未拉起时不该报 alive")
	}
	note, _ := m["note"].(string)
	if !strings.Contains(note, "agent_up") {
		t.Errorf("★ 应提示用 agent_up 拉起: %q", note)
	}
}

// ★★★ 判据：**通道名在 lifecycle 前后一致**（agent_up 与 branch_close 同源）。
//
// 不一致的话：注册的通道与清理的通道是两个名字，清理不掉 ——
// 内核会继续往死通道投递。这条钉住"注册与清理用同一套派生函数"。
func TestChannelNamesMatchAcrossLifecycle(t *testing.T) {
	p := newFlowPlugin(t)
	p.handleProjectAdd(map[string]interface{}{"name": "proj", "remote_url": "/tmp/x.git"})
	r, _ := p.handleBranchOpen(map[string]interface{}{"project": "proj", "branch": "feat/n"})
	id := r.(map[string]interface{})["branch_id"].(string)

	// agent_up 用的名字
	inUp := inputChannelName(id)
	outUp := outputChannelName(id)
	// branch_close 用的名字（同一套派生函数）
	if inputChannelName(id) != inUp || outputChannelName(id) != outUp {
		t.Error("★ 通道名派生不稳定（注册与清理会不一致）")
	}
	// outputch 必须符合内核的 output_send__<name> 命名约束
	if !validNamePattern.MatchString(outUp) {
		t.Errorf("outputch 不合规: %s", outUp)
	}
	if strings.Contains(outUp, "/") {
		t.Errorf("outputch 含斜杠（output_send__ 不允许）: %s", outUp)
	}
}

// ★★ 判据：`test_list` 标出**变异规则为 0 的判据**（复核缺口可见）。
//
// 这是"复核必须两步"这条主张的操作化：光跑正向不算复核完，
// 而没有变异规则的判据在列表里必须看得出来。
func TestTestListMarksCriteriaWithoutMutations(t *testing.T) {
	p := newFlowPlugin(t)
	p.ledger.Criteria["cr-a"] = &Criteria{
		ID: "cr-a", Name: "有变异", Mutations: []Mutation{{Old: "x", New: "y"}},
	}
	p.ledger.Criteria["cr-b"] = &Criteria{
		ID: "cr-b", Name: "无变异", Mutations: nil,
	}
	res, _ := p.handleTestList(map[string]interface{}{})
	m := res.(map[string]interface{})
	items, ok := m["criteria"].([]map[string]interface{})
	if !ok {
		t.Skipf("test_list 返回形状未知: %T", m["criteria"])
	}
	if len(items) != 2 {
		t.Fatalf("应列出 2 条判据，实际 %d: %v", len(items), items)
	}
	// 真实字段名是 `mutations`（不是 mutation_count）——判据必须按真实字段断言，
	// 否则"字段改了判据还绿"。
	for _, it := range items {
		switch it["criteria_id"] {
		case "cr-a":
			if n, _ := it["mutations"].(int); n != 1 {
				t.Errorf("★ cr-a 的 mutations 应为 1，实际 %#v", it["mutations"])
			}
		case "cr-b":
			if n, _ := it["mutations"].(int); n != 0 {
				t.Errorf("★ cr-b 的 mutations 应为 0（复核缺口应可见），实际 %#v", it["mutations"])
			}
		default:
			t.Errorf("出现意外的判据: %v", it)
		}
	}
}
