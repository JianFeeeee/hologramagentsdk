package main

// ★★★ 端到端：**真 pi 真的能接受工程并改代码**。
//
// ## 这条判据为什么最重要
//
// 覆盖度审计发现：此前**没有任何判据**验证过这条链路。所有测试都停在
// "工具存在" / "参数被记账" / "驱动被加载" 这一层，而**最根本的假设从未被验证**：
//
//	投一句话 → sidecar 转给 pi → pi 用工具**真的改了工作目录的文件**
//
// 如果这个假设不成立，整个插件就是空壳 —— 台账、通道、判据复核全都在
// 正确运转，但 agent 什么都没做。
//
// ## 为什么必须走 sidecar（不走 pi 的 API 直调）
//
// 直调 pi 的 `createAgentSession` 只能证明"pi 可用"，证明不了
// **本插件的 sidecar 契约是对的**（stdin JSON-RPC、session_id 回报、
// 裸名模块解析）。实测中这三处每一处都可能单独失败：
//   - 方法名不是 send 而是 prompt（我第一版就猜错了）
//   - 裸名 `@earendil-works/pi-coding-agent` 解析不到会**静默回退到宿主机的 pi**
//
// ## 环境要求
//
// 需要插件目录下有内嵌运行时（node + pi）：
//
//	HA_PI_E2E=1 go test -run TestRealPiEditsFiles ./...
//
// 默认跳过（内嵌运行时是构建产物，不进版本库；且真跑一次要几十秒到几分钟）。

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// piE2EReady 检查内嵌运行时是否就绪。
func piE2EReady(t *testing.T) (nodeBin, sidecarPath string) {
	t.Helper()
	if os.Getenv("HA_PI_E2E") != "1" {
		t.Skip("设 HA_PI_E2E=1 以运行（需要 rawfile/ 下有内嵌 node+pi，且会真调 LLM）")
	}
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	nodeBin = filepath.Join(root, "rawfile", "node", "bin", "node")
	if _, err := os.Stat(nodeBin); err != nil {
		t.Skipf("内嵌 node 不存在: %v", err)
	}
	piMod := filepath.Join(root, "rawfile", "pi", "node_modules", "@earendil-works", "pi-coding-agent")
	if _, err := os.Stat(piMod); err != nil {
		t.Skipf("内嵌 pi 不存在: %v", err)
	}
	sidecarPath = filepath.Join(root, "sidecar.mjs")
	if _, err := os.Stat(sidecarPath); err != nil {
		t.Fatalf("sidecar.mjs 不存在: %v", err)
	}
	return nodeBin, sidecarPath
}

// rpcResponse 是 sidecar 的应答。
type rpcResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// startSidecarForE2E 拉起真实 sidecar 并完成握手。
//
// 走**与 Go 侧完全一样的契约**：stdin 写 JSON-RPC 行，stdout 读应答行。
func startSidecarForE2E(t *testing.T, nodeBin, sidecarPath, workdir, sessionID string) (*exec.Cmd, *bufio.Writer, *bufio.Reader) {
	t.Helper()
	pluginDir := filepath.Dir(sidecarPath)
	cmd := exec.Command(nodeBin, sidecarPath,
		"--workdir", workdir,
		"--session-id", sessionID,
		"--session-name", "e2e-"+sessionID,
	)
	cmd.Dir = workdir
	// ★ 与 driver.json 里声明的 env 一致（PI_CODING_AGENT_DIR/SESSION_DIR）。
	// ★ PI_CODING_AGENT_DIR 指向**已配好 LLM 源**的 agentdir。
	// 内嵌 pi 是干净的（无凭据），必须先把源导进去 —— 这正是
	// `commondev_pi_source_import` 的职责。判据用现成的 agentdir，
	// 顺带验证"导入后的配置能被内嵌 pi 直接使用"。
	agentDir := os.Getenv("HA_PI_AGENTDIR")
	if agentDir == "" {
		agentDir = filepath.Join(pluginDir, "e2e-agentdir")
	}
	cmd.Env = append(os.Environ(),
		"PI_CODING_AGENT_DIR="+agentDir,
		"PI_CODING_AGENT_SESSION_DIR="+filepath.Join(workdir, ".pi-sessions"),
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = nil // 忽略 pi 的 a2a/acp 端口告警
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 sidecar 失败: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd, bufio.NewWriter(stdin), bufio.NewReader(stdout)
}

// callSidecar 发一条 JSON-RPC 并读回对应 id 的应答。
func callSidecar(t *testing.T, w *bufio.Writer, r *bufio.Reader, id int, method string, params map[string]interface{}) *rpcResponse {
	t.Helper()
	req := map[string]interface{}{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	if _, err := w.WriteString(string(b) + "\n"); err != nil {
		t.Fatalf("写 sidecar 失败: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush 失败: %v", err)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("读 sidecar 失败（method=%s）: %v", method, err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var resp rpcResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			continue // 非 JSON 行（pi 的日志）跳过
		}
		if resp.ID == id {
			return &resp
		}
	}
	t.Fatalf("等 sidecar 应答超时（method=%s）", method)
	return nil
}

// ★★★ 判据：**真 pi 真的按指令改了工作目录里的文件**。
//
// 这是「agent 能接受工程并更新」的地基。整条链：
//
//	真 sidecar 进程 → 真 pi 会话 → pi 调自己的写文件工具 → 工作目录出现文件
func TestRealPiEditsFiles(t *testing.T) {
	nodeBin, sidecarPath := piE2EReady(t)
	workdir := t.TempDir()
	sessionID := "e2e-edit"

	_, w, r := startSidecarForE2E(t, nodeBin, sidecarPath, workdir, sessionID)

	// ① 先握手（sidecar 契约的第一环）
	ping := callSidecar(t, w, r, 1, "ping", nil)
	if ping.Error != nil {
		t.Fatalf("ping 失败: %s", ping.Error.Message)
	}
	var pingRes map[string]interface{}
	if err := json.Unmarshal(ping.Result, &pingRes); err != nil {
		t.Fatalf("ping 应答不是对象: %s", ping.Result)
	}
	if ok, _ := pingRes["ok"].(bool); !ok {
		t.Fatalf("ping 未返回 ok: %v", pingRes)
	}
	// ★ 必须回报 pi 版本（U8 要求可锁定内层 pi）——取不到要如实为 null，
	// 不许编一个。这里是断言"字段存在"，至于是否为空由实现诚实性决定。
	if _, has := pingRes["pi_version"]; !has {
		t.Error("ping 应回报 pi_version（用于锁定内层 pi 版本）")
	} else {
		t.Logf("内层 pi 版本: %v", pingRes["pi_version"])
	}

	// ② 给一个必须动文件的指令
	const wantFile = "hello.txt"
	const wantText = "hello from pi"
	resp := callSidecar(t, w, r, 2, "prompt", map[string]interface{}{
		"session_id": sessionID,
		"text": "在当前工作目录创建文件 " + wantFile +
			"，内容恰好一行：" + wantText + "。只做这一件事，不要创建别的文件。",
	})
	if resp.Error != nil {
		t.Fatalf("prompt 失败: %s", resp.Error.Message)
	}
	var pr map[string]interface{}
	if err := json.Unmarshal(resp.Result, &pr); err != nil {
		t.Fatalf("prompt 应答不是对象: %s", resp.Result)
	}
	// ★ 会话不得漂移（Go 侧 deliverToPi 会据此拒绝继续）
	if got, _ := pr["session_id"].(string); got != "" && got != sessionID {
		t.Fatalf("★ 会话漂移: 期望 %s，实际 %s", sessionID, got)
	}

	// ③ **关键断言**：文件真的出现在工作目录里，且内容正确
	path := filepath.Join(workdir, wantFile)
	b, err := os.ReadFile(path)
	if err != nil {
		// 列出目录帮助定位（是没创建，还是创建到了别处）
		entries, _ := os.ReadDir(workdir)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("★ pi 没有创建工作目录下的 %s —— 说明「接受工程并更新」不成立。目录内容: %v",
			wantFile, names)
	}
	if !strings.Contains(string(b), wantText) {
		t.Errorf("★ 文件内容不对: 期望含 %q，实际 %q", wantText, string(b))
	}
	t.Logf("✅ 真 pi 改文件成功: %s = %q", wantFile, strings.TrimSpace(string(b)))
}

// ★★★ 判据：**同一 session 的第二次投递续在同一会话**（不是新会话）。
//
// 「续投」是这个插件的核心能力（设计：session_id 是续投的唯一依据）。
// 若第二次投递另起会话，agent 就失去了上下文，工程对话不再连续。
func TestRealPiKeepsSessionAcrossPrompts(t *testing.T) {
	nodeBin, sidecarPath := piE2EReady(t)
	workdir := t.TempDir()
	sessionID := "e2e-cont"

	_, w, r := startSidecarForE2E(t, nodeBin, sidecarPath, workdir, sessionID)
	callSidecar(t, w, r, 1, "ping", nil)

	// 第一句：建立上下文（记住一个数）
	resp1 := callSidecar(t, w, r, 2, "prompt", map[string]interface{}{
		"session_id": sessionID,
		"text":       "请记住这个数字：7391。只回复「记住了」。",
	})
	if resp1.Error != nil {
		t.Fatalf("第一次投递失败: %s", resp1.Error.Message)
	}
	var p1 map[string]interface{}
	json.Unmarshal(resp1.Result, &p1)
	got1, _ := p1["session_id"].(string)
	if got1 != sessionID {
		t.Fatalf("★ 首次投递会话不对: %s", got1)
	}

	// 第二句：问它那个数 —— 只有**同一会话**才答得出
	resp2 := callSidecar(t, w, r, 3, "prompt", map[string]interface{}{
		"session_id": sessionID,
		"text":       "我刚才让你记的数字是多少？只回复那个数字。",
	})
	if resp2.Error != nil {
		t.Fatalf("第二次投递失败: %s", resp2.Error.Message)
	}
	var p2 map[string]interface{}
	json.Unmarshal(resp2.Result, &p2)
	got2, _ := p2["session_id"].(string)
	if got2 != sessionID {
		t.Fatalf("★ 第二次投递会话漂移: 期望 %s，实际 %s（续投失效）", sessionID, got2)
	}
	reply, _ := p2["reply"].(string)
	if !strings.Contains(reply, "7391") {
		t.Errorf("★ 第二次回复未体现会话连续性（期望含 7391）: %q", reply)
	}
	t.Logf("✅ 会话连续性成立，第二次回复: %q", strings.TrimSpace(reply))
}
