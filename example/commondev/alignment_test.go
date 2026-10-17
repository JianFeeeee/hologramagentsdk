package main

// 对齐审计：**逐个调用**已注册工具，检查它们真的能工作。
//
// 为何要这个：注册了名字 ≠ 能用。上一轮补了工具，但「能注册」只证明
// 编译通过。本文件对每个工具做一次最小真实调用，把「写了但没接上」
// （空返回、nil handler、路径写死错、参数名不匹配）暴露出来。
//
// 判定原则：
//   - 返回 error ≠ 失败（很多工具对缺参数就是返回 error，那是正确的响亮行为）
//   - 真正的失败是：panic、handler 为 nil、或**参数齐全却依然拒绝**
//   - 这里的目标是发现"连正确调用都走不通"的工具

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// listRegisteredTools 从 plugin.go 里抽出注册的工具名。
// 用源码而非运行期 sdk，因为 SDK 的 mock 不暴露注册表。
func listRegisteredTools(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("plugin.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		const p = `p.sdk.RegisterTool("`
		if i := strings.Index(line, p); i >= 0 {
			rest := line[i+len(p):]
			if j := strings.Index(rest, `"`); j > 0 {
				out = append(out, rest[:j])
			}
		}
	}
	return out
}

// handlerFor 通过名字映射到实际 handler 函数。
//
// ★ 这里刻意**手写映射**而不是反射：如果新增了工具却忘了在这里登记，
// 下面的覆盖率判据会报出来 —— 这正好是"新工具没接 handler"的探测。
func handlerFor(p *Plugin, name string) func(map[string]interface{}) (interface{}, error) {
	switch name {
	case "commondev_project_add", "commondev_project_open":
		return p.handleProjectAdd
	case "commondev_project_list":
		return p.handleProjectList
	case "commondev_branch_open":
		return p.handleBranchOpen
	case "commondev_branch_status":
		return p.handleBranchStatus
	case "commondev_branch_close":
		return p.handleBranchClose
	case "commondev_job_status":
		return p.handleJobStatus
	case "commondev_agent_up":
		return p.handleAgentUp
	case "commondev_agent_state":
		return p.handleAgentState
	case "commondev_agent_down":
		return p.handleAgentDown
	case "commondev_workdir_prepare":
		return p.handleWorkdirPrepare
	case "commondev_workdir_clean":
		return p.handleWorkdirClean
	case "commondev_test_define":
		return p.handleTestDefine
	case "commondev_test_list":
		return p.handleTestList
	case "commondev_test_run":
		return p.handleTestRun
	case "commondev_test_mutate":
		return p.handleTestMutate
	case "commondev_review_submit":
		return p.handleReviewSubmit
	case "commondev_review_result":
		return p.handleReviewResult
	case "commondev_conflict_check":
		return p.handleConflictCheck
	case "commondev_parallel_status":
		return p.handleParallelStatus
	case "commondev_pi_config_get":
		return p.handlePiConfigGet
	case "commondev_pi_config_set":
		return p.handlePiConfigSet
	case "commondev_pi_source_add":
		return p.handlePiSourceAdd
	case "commondev_pi_source_list":
		return p.handlePiSourceList
	case "commondev_pi_source_remove":
		return p.handlePiSourceRemove
	case "commondev_pi_source_default":
		return p.handlePiSourceDefault
	case "commondev_pi_source_import":
		return p.handlePiSourceImport
	case "commondev_pi_package_install", "commondev_pi_plugin_install":
		return p.handlePiPackageInstall
	case "commondev_pi_plugin_list":
		return p.handlePiPluginList
	case "commondev_pi_plugin_remove":
		return p.handlePiPluginRemove
	case "commondev_pi_plugin_update":
		return p.handlePiPluginUpdate
	case "commondev_pi_mcp_list":
		return p.handlePiMCPList
	case "commondev_pi_mcp_add":
		return p.handlePiMCPAdd
	case "commondev_pi_mcp_remove":
		return p.handlePiMCPRemove
	case "enc_store":
		return p.handleEncStore
	case "enc_list":
		return p.handleEncList
	case "enc_show":
		return p.handleEncShow
	case "enc_rm":
		return p.handleEncRm
	case "enc_shell":
		return p.handleEncryptedShell
	case "commondev_endpoint_set":
		return p.handleEndpointSet
	case "commondev_endpoint_list":
		return p.handleEndpointList
	case "commondev_endpoint_remove":
		return p.handleEndpointRemove
	}
	return nil
}

// ★★ 判据：**每个注册的工具都必须有对应 handler**。
//
// 这条能抓出「加了 RegisterTool 却指向不存在的 handler」或
// 「加了工具但这里没同步登记」（后者是需要人补映射，属于审计信号）。
func TestEveryRegisteredToolHasHandler(t *testing.T) {
	p := newAuditPlugin(t)
	tools := listRegisteredTools(t)
	if len(tools) < 40 {
		t.Fatalf("注册工具数异常偏少（%d），插件文件可能没被正确解析", len(tools))
	}
	var missing []string
	for _, name := range tools {
		if handlerFor(p, name) == nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("★ 这些工具没有可用 handler（注册了但没接上）: %v", missing)
	}
	t.Logf("已注册 %d 个工具，全部有 handler", len(tools))
}

// ★★ 判据：**无参数调用不得 panic**。
//
// 工具会遇到模型乱传参数（缺字段、类型错）。正确行为是返回 error，
// 不是崩掉整个插件进程。
func TestNoToolPanicsOnEmptyArgs(t *testing.T) {
	p := newAuditPlugin(t)
	for _, name := range listRegisteredTools(t) {
		h := handlerFor(p, name)
		if h == nil {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("★ %s 空参数 panic: %v", name, r)
				}
			}()
			// 空 map + nil map 都要试：模型可能传 null
			h(map[string]interface{}{})
			h(nil)
		}()
	}
}

// ★★★ 判据：**本轮补的工具必须真的能完成一次正常调用**。
//
// 与前两条不同：这里给足参数，检查业务是否真走通。
// 「注册 + 不 panic」还不够 —— 一条写死的错误路径也能满足那两条。
func TestNewlyAddedToolsActuallyWork(t *testing.T) {
	p := newAuditPlugin(t)
	p.ledger.Projects["proj"] = &Project{
		Name: "proj", RemoteURL: "https://example.invalid/o/r.git", DefaultBranch: "main"}
	dir := t.TempDir()
	p.ledger.Branches["br-1"] = &Branch{
		ID: "br-1", ProjectID: "proj", Name: "feat/x", Workdir: dir, Status: "open"}

	// ① agent_state 应报出真实状态（此前完全不存在）
	res, err := p.handleAgentState(map[string]interface{}{"branch_id": "br-1"})
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]interface{})
	if m["branch_id"] != "br-1" || m["agent_alive"] != false {
		t.Errorf("agent_state 返回不对: %v", m)
	}

	// ② agent_down 应成功停掉（无进程时也幂等成功）
	res2, err := p.handleAgentDown(map[string]interface{}{"branch_id": "br-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res2.(map[string]interface{})["status"] != "ok" {
		t.Errorf("agent_down 未成功: %v", res2)
	}

	// ③ workdir_clean 默认 dry_run，且能识别孤儿
	root := p.programRoot()
	os.MkdirAll(filepath.Join(root, "deadbeef-feat_x"), 0o755)
	res3, err := p.handleWorkdirClean(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	m3 := res3.(map[string]interface{})
	if m3["status"] != "dry_run" {
		t.Errorf("workdir_clean 默认应为 dry_run: %v", m3)
	}
	if n, _ := m3["count"].(int); n != 1 {
		t.Errorf("应识别 1 个孤儿目录，实际 %v", m3["orphans"])
	}

	// ④ pi_config_get 必须能读到结构（即使没有任何 provider）
	res4, err := p.handlePiConfigGet(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res4.(map[string]interface{})["providers"]; !ok {
		t.Errorf("pi_config_get 缺少 providers 字段: %v", res4)
	}
}

// ★★ 判据：**设计文档点名的工具必须真的存在**。
//
// 这条把「文档说做了」与「代码里有」绑起来 —— 上一轮就是靠它
// 发现 pr_open / agent_state 等工具根本不存在。
//
// ⚠ 2026-10-15 起：**git 托管平台专有的工具已不在清单里**
// （account_* / repo_search / pr_* / protect_branch）—— 那些能力进 skill，
// 由 agent 用 safeshell + curl 完成。
func TestDesignNamedToolsExist(t *testing.T) {
	tools := listRegisteredTools(t)
	have := map[string]bool{}
	for _, n := range tools {
		have[n] = true
	}
	want := []string{
		"commondev_project_add", "commondev_project_list",
		"commondev_branch_open", "commondev_branch_status", "commondev_branch_close",
		"commondev_workdir_prepare", "commondev_workdir_clean",
		"commondev_agent_up", "commondev_agent_state", "commondev_agent_down",
		"commondev_pi_plugin_install", "commondev_pi_plugin_remove", "commondev_pi_plugin_list",
		"commondev_pi_source_import", "commondev_pi_source_add", "commondev_pi_source_list",
		"commondev_pi_source_default", "commondev_pi_config_get",
		"commondev_test_define", "commondev_test_run", "commondev_test_mutate",
		"commondev_review_submit",
	}
	var missing []string
	for _, w := range want {
		if !have[w] {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("设计点名的工具缺失: %v", missing)
	}
}

// ★ 判据：**git 平台专有的工具已全部移出插件**。
//
// 这条钉住 2026-10-15 的架构决定：平台知识进 skill。若有人又把
// account_* / pr_* / protect_branch 加回插件，这里会报红。
func TestPlatformToolsAreNotInPlugin(t *testing.T) {
	tools := listRegisteredTools(t)
	have := map[string]bool{}
	for _, n := range tools {
		have[n] = true
	}
	for _, forbidden := range []string{
		"commondev_account_add", "commondev_account_list", "commondev_account_test",
		"commondev_account_remove", "commondev_repo_search",
		"commondev_pr_open", "commondev_pr_status", "commondev_pr_approve",
		"commondev_pr_merge", "commondev_protect_branch",
	} {
		if have[forbidden] {
			t.Errorf("★ %s 属 git 平台专有能力，不应再注册在插件里（应进 skill + safeshell）", forbidden)
		}
	}
}

// ★★ 判据：**平台专有代码不得回到插件里**。
//
// 钉住 2026-10-15 的架构决定（用户拍板）：git 托管平台的知识进 skill，
// 插件只保留与平台无关的机制。若有人把 platformSpec / 按平台构造认证头
// 又加回来，这里会报红。
//
// 为什么要用判据而不只是文档：文档说了还会有人（包括将来的我）图省事加回去，
// 而那种回退在**功能上看起来完全正常** —— 只是把可移植性换成了硬编码。
func TestNoPlatformSpecificCodeInPlugin(t *testing.T) {
	// 逐文件扫源码（排除测试与注释性提及由人工判断）
	files := []string{"plugin.go", "credentials.go", "pi_tools.go", "criteria.go",
		"encstore.go", "encshell.go", "conflict.go", "workdir.go", "workdir_security.go",
		"util.go", "llmsource.go", "sidecar.go"}
	forbidden := map[string]string{
		"platformSpec(":       "按平台构造请求（认证头/端点差异）应进 skill",
		"plaftformSpec":       "平台适配结构体",
		"DefaultAPI":          "硬编码平台 API 前缀",
		"AuthHeader:":         "硬编码平台认证头",
		"RoleExecutor":        "平台角色语义",
		"handleAccountAdd":    "平台凭据工具",
		"handleRepoSearch":    "平台搜索工具",
		"handlePROpen":        "平台 PR 工具",
		"handleProtectBranch": "平台分支保护工具",
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		src := string(b)
		for sym, why := range forbidden {
			// 只认代码出现，不认注释里的解释性提及
			for i, line := range strings.Split(src, "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "//") {
					continue // 注释里说明"已移出"是允许的
				}
				if strings.Contains(line, sym) {
					t.Errorf("★ %s:%d 含平台专有代码 %q（%s）", f, i+1, sym, why)
				}
			}
		}
	}
}
