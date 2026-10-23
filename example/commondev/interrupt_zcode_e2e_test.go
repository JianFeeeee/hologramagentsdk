package main

// 真机 e2e：zcode 的取消路径（HA_ZCODE_E2E=1 时运行）。
//
// ## 为什么单独测
//
// pi 与 zcode 的取消是**两种完全不同的机制**：
//
//	pi    → session.abort()，协作式中断，agent 自己走到下一个可中断点
//	zcode → 整组杀掉当前回合的子进程（zcode 没有 abort API）
//
// 所以 pi 的判据证明不了 zcode 能用。这里用**假 CLI**（一个会持续输出的
// node 脚本）验证杀进程这条路径 —— 不需要真 zcode CLI 就能测：
// 判据要证的是"interrupt 真的把子进程杀了、且不误杀后续回合"，
// 这与 CLI 是不是真的 zcode 无关。

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ★★★ 判据：zcode 的 interrupt 真的杀掉子进程，且句柄正确清空。
func TestRealZcodeInterruptKillsChildAndClearsHandle(t *testing.T) {
	if os.Getenv("HA_ZCODE_E2E") != "1" {
		t.Skip("设 HA_ZCODE_E2E=1 以运行（用假 CLI，不需要真 zcode）")
	}
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	nodeBin := filepath.Join(root, "rawfile", "node", "bin", "node")
	if _, err := os.Stat(nodeBin); err != nil {
		t.Skipf("内嵌 node 不存在: %v", err)
	}
	// 读**跟踪的参考实现**：rawfile/ 不进版本库，干净克隆下不存在。
	// 原来读 rawfile 部署路径 ⇒ 打开开关的人会得到"文件不存在"的假红，
	// 而它想守的是"取消真的杀了子进程"，与资源有没有准备好无关。
	sidecarPath := filepath.Join(root, "examples", "custom-cli", "zcode", "sidecar.mjs")
	if _, err := os.Stat(sidecarPath); err != nil {
		t.Fatalf("跟踪的 zcode 参考实现不存在: %v", err)
	}

	dir := t.TempDir()
	workdir := filepath.Join(dir, "work")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 假 CLI：node 脚本，持续输出 120 秒（足够被中断）
	fake := filepath.Join(dir, "fakezcode.js")
	script := `console.log(JSON.stringify({type:"start"}));
let i = 0;
const t = setInterval(() => {
  i++;
  console.log("tick-" + i);
  if (i >= 120) {
    clearInterval(t);
    console.log(JSON.stringify({type:"result", sessionId:"sess_fake", response:"done"}));
  }
}, 1000);
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	_, w, r := startSidecarWithExtraEnv(t, nodeBin, sidecarPath, workdir, "e2e-zcode-int", "ZCODE_CLI="+fake)

	if resp := callSidecar(t, w, r, 1, "ping", nil); resp.Error != nil {
		t.Fatalf("ping 失败: %s", resp.Error.Message)
	}
	if resp := callSidecar(t, w, r, 2, "prompt", map[string]interface{}{"text": "跑吧"}); resp.Error != nil {
		t.Fatalf("prompt 失败: %s", resp.Error.Message)
	}
	// 让它真的跑起来
	time.Sleep(6 * time.Second)

	// 第一次 interrupt：必须真的杀
	resp := callSidecar(t, w, r, 3, "interrupt", nil)
	if resp.Error != nil {
		t.Fatalf("interrupt 失败: %s", resp.Error.Message)
	}
	raw := string(resp.Result)
	if !strings.Contains(raw, `"interrupted":true`) {
		t.Fatalf("★ zcode interrupt 没有真的中断（%s）—— 用户以为停了，任务还在跑", raw)
	}
	if !strings.Contains(raw, "killed_pid") {
		t.Fatalf("★ interrupt 应答没有 killed_pid（%s）—— 无法确认真的杀了进程", raw)
	}
	t.Logf("第一次 interrupt: %s", raw)

	// 第二次 interrupt：句柄必须已清空，不能误杀后续回合
	time.Sleep(3 * time.Second)
	resp2 := callSidecar(t, w, r, 4, "interrupt", nil)
	raw2 := string(resp2.Result)
	if !strings.Contains(raw2, `"interrupted":false`) {
		t.Fatalf("★ 第二次 interrupt 仍报 interrupted=true（%s）—— "+
			"说明 currentChild 没清空，会误杀下一轮", raw2)
	}
	if !strings.Contains(raw2, "no_running_turn") {
		t.Fatalf("★ 第二次 interrupt 的 reason 不是 no_running_turn（%s）", raw2)
	}
	t.Logf("第二次 interrupt（应无回合）: %s", raw2)
}

// startSidecarWithExtraEnv 在既有 helper 的基础上追加一条环境变量。
//
// 为什么需要额外 env：zcode 的 CLI 路径由 ZCODE_CLI 指定（driver.json 里
// 没有 cliPath 时）。判据用假 CLI，所以必须能注入这条变量。
func startSidecarWithExtraEnv(t *testing.T, nodeBin, sidecarPath, workdir, sessionID, extraEnv string) (*exec.Cmd, *bufio.Writer, *bufio.Reader) {
	t.Helper()
	cmd := exec.Command(nodeBin, sidecarPath, "--workdir", workdir, "--session-id", sessionID)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(),
		"PI_CODING_AGENT_SESSION_DIR="+filepath.Join(workdir, ".pi-sessions"),
		extraEnv,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 sidecar 失败: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd, bufio.NewWriter(stdin), bufio.NewReader(stdout)
}
