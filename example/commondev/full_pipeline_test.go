package main

// ★★★ 全流程判据：用**插件自己的工具**跑完整生命周期。
//
// 手写脚本证明的是"pi 能改代码"；这条证明的是**插件的代码路径**能把它串起来：
//   project_add → branch_open → (目录准备) → agent_up → 投递 → 收尾
//
// 与 pi_e2e_test.go 的分工：那条测 sidecar 契约，这条测**插件编排**。

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitQ(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// ★★★ 判据：插件工具能把一条工程线从登记走到"pi 真的改了工程文件"。
//
// 需要内嵌 node+pi 与可用 LLM 源：
//
//	HA_PI_E2E=1 HA_PI_AGENTDIR=<配好的 agentdir> go test -run TestFullPipeline ./...
func TestFullPipeline(t *testing.T) {
	nodeBin, sidecarPath := piE2EReady(t)
	agentDir := os.Getenv("HA_PI_AGENTDIR")
	if agentDir == "" {
		t.Skip("需要 HA_PI_AGENTDIR（含可用 LLM 源的 pi agentdir）")
	}

	base := t.TempDir()
	origin := filepath.Join(base, "origin")
	gitQ(t, base, "init", "-q", "-b", "main", origin)
	if err := os.MkdirAll(filepath.Join(origin, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(origin, "src/calc.py"),
		[]byte("def add(a, b):\n    return a + b\n"), 0o644)
	gitQ(t, origin, "add", "-A")
	gitQ(t, origin, "commit", "-qm", "initial")

	// ① 登记项目（走插件工具）
	p := newFlowPlugin(t)
	r1, _ := p.handleProjectAdd(map[string]interface{}{
		"name": "demo", "remote_url": origin, "default_branch": "main",
	})
	if m := r1.(map[string]interface{}); m["status"] != "ok" {
		t.Fatalf("登记项目失败: %v", m)
	}

	// ② 开线（走插件工具）—— 它会把 clone 转后台
	r2, _ := p.handleBranchOpen(map[string]interface{}{
		"project": "demo", "branch": "feat/mul", "work_desc": "加 multiply",
	})
	m2 := r2.(map[string]interface{})
	if m2["status"] != "ok" {
		t.Fatalf("开线失败: %v", m2)
	}
	branchID := m2["branch_id"].(string)
	workdir := m2["workdir"].(string)

	// ③ 同步准备目录（不走后台 goroutine，避免与判据的时序竞争）
	b := p.ledger.Branches[branchID]
	if err := p.prepareWorkdir(p.ledger.Projects["demo"], "feat/mul", workdir); err != nil {
		t.Fatalf("目录准备失败: %v", err)
	}
	// ★ 目录必须在目标分支上（否则 agent 会在 main 上改）
	br, err := exec.Command("git", "-C", workdir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(br)) != "feat/mul" {
		t.Fatalf("★ 目录不在目标分支: %q", strings.TrimSpace(string(br)))
	}

	// ④ 走 sidecar 契约投递任务（agent_up 的内核部分）
	b.Workdir = workdir
	_, w, rd := startSidecarForE2E(t, nodeBin, sidecarPath, workdir, branchID)
	ping := callSidecar(t, w, rd, 1, "ping", nil)
	if ping.Error != nil {
		t.Fatalf("握手失败: %s", ping.Error.Message)
	}
	resp := callSidecar(t, w, rd, 2, "prompt", map[string]interface{}{
		"session_id": branchID,
		"text":       "在 src/calc.py 里新增 multiply(a, b) 返回 a*b，保留 add。只回一句完成。",
	})
	if resp.Error != nil {
		t.Fatalf("投递失败: %s", resp.Error.Message)
	}
	var pr map[string]interface{}
	if err := json.Unmarshal(resp.Result, &pr); err != nil {
		t.Fatal(err)
	}
	// ★ 会话不得漂移
	if got, _ := pr["session_id"].(string); got != "" && got != branchID {
		t.Fatalf("★ 会话漂移: %s vs %s", got, branchID)
	}

	// ⑤ **关键断言**：pi 真的改了**这个工程**的文件
	b2, err := os.ReadFile(filepath.Join(workdir, "src", "calc.py"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b2), "multiply") {
		t.Fatalf("★ pi 没有在工程里加 multiply:\n%s", b2)
	}
	if !strings.Contains(string(b2), "def add") {
		t.Errorf("★ 原有代码被破坏:\n%s", b2)
	}

	// ⑥ 提交后做冲突预检（走插件工具）
	gitQ(t, workdir, "add", "-A")
	gitQ(t, workdir, "commit", "-qm", "feat: add multiply")
	r6, _ := p.handleConflictCheck(map[string]interface{}{
		"branch_id": branchID, "target": "main",
	})
	if m, ok := r6.(map[string]interface{}); ok {
		if e, bad := m["error"]; bad {
			t.Fatalf("冲突预检失败: %v", e)
		}
		t.Logf("冲突预检结果: %v", m)
	}

	// ⑦ 收尾（走插件工具）：台账标记 + 目录删除
	r7, _ := p.handleBranchClose(map[string]interface{}{"branch_id": branchID})
	if m, ok := r7.(map[string]interface{}); ok {
		if e, bad := m["error"]; bad {
			t.Fatalf("收尾失败: %v", e)
		}
	}
	if _, err := os.Stat(workdir); !os.IsNotExist(err) {
		t.Errorf("★ 收尾后工作目录应被删除: %v", err)
	}
	t.Logf("✅ 全流程闭环：登记 → 开线(分支正确) → 投递 → pi 真改工程 → 冲突预检 → 收尾")
}

// ★★★ 判据：**agent_up 真能拉起 sidecar**（走随包分发的脚本）。
//
// 这条覆盖的缺口（2026-10-15 实测发现）：sidecar 脚本此前**没有随包分发**，
// 而插件数据目录的 runtime/<cli>/ 在部署时没人创建 ⇒ agent_up 必然失败，
// 报「找不到 pi 的 sidecar 脚本」并指向一个用户没听说过的路径。
//
// 本判据用**解包后的插件目录布局**（rawfile/runtime/pi/）验证查找链，
// 并真的拉起进程完成握手 —— 证明"脚本随包 + 查找链"这条组合可用。
func TestAgentUpFindsShippedSidecar(t *testing.T) {
	nodeBin, _ := piE2EReady(t)
	root, _ := os.Getwd()

	// 模拟解包后的布局：脚本在 rawfile/runtime/pi/
	shipped := filepath.Join(root, "rawfile", "runtime", "pi", "sidecar.mjs")
	if _, err := os.Stat(shipped); err != nil {
		t.Fatalf("★ 随包脚本不存在: %v", err)
	}

	p := newFlowPlugin(t)
	p.sdk.SetRawFilesAPI(&testRawFiles{root: filepath.Join(root, "rawfile")})
	// cliDir 指向**不存在的**目录（全新部署的真实状态）
	cliDir := filepath.Join(t.TempDir(), "runtime", "pi")

	got, err := p.locateSidecarScript(builtinPiDriver(), cliDir, "pi")
	if err != nil {
		t.Fatalf("★ 查找链找不到随包脚本（agent_up 会失败）: %v", err)
	}

	// 用找到的脚本真的拉起 sidecar 并握手
	workdir := t.TempDir()
	_, w, r := startSidecarForE2E(t, nodeBin, got, workdir, "agentup-e2e")
	ping := callSidecar(t, w, r, 1, "ping", nil)
	if ping.Error != nil {
		t.Fatalf("★ 用随包脚本拉起的 sidecar 握手失败: %s", ping.Error.Message)
	}
	var pr map[string]interface{}
	json.Unmarshal(ping.Result, &pr)
	if ok, _ := pr["ok"].(bool); !ok {
		t.Fatalf("ping 未 ok: %v", pr)
	}
	t.Logf("✅ 随包 sidecar 脚本可用，pi 版本: %v", pr["pi_version"])
}
