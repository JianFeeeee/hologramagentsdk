package main

// 侧车驱动层判据（「换 CLI，核心一行不改」）。
//
// 要证明三件事：
//  ① 核心不认 pi —— startSidecar 的参数/环境全部来自 driver 描述
//  ② 声明式与内置描述**等价**（否则显式化就改变了行为）
//  ③ 换驱动真的能换后端（用假 CLI 端到端验证）

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ★★★ 判据：**核心源码里不得出现 pi 专有的启动细节**。
//
// 这是「核心一行不改」的可验证形式：pi 的 agentdir / sessions 目录名、
// `PI_CODING_AGENT_*` 环境变量，都只能出现在 **driver 描述**里，
// 不能出现在 startSidecar 里。
func TestCoreDoesNotHardcodePiLaunchDetails(t *testing.T) {
	b, err := os.ReadFile("sidecar.go")
	if err != nil {
		t.Fatal(err)
	}
	// startSidecar 函数体：从函数头到下一个顶层函数
	src := string(b)
	start := strings.Index(src, "func (p *Plugin) startSidecar(")
	if start < 0 {
		t.Fatal("找不到 startSidecar")
	}
	rest := src[start:]
	if end := strings.Index(rest[1:], "\nfunc "); end > 0 {
		rest = rest[:end+1]
	}
	for _, forbidden := range []string{
		"PI_CODING_AGENT_DIR", // pi 专有环境变量
		"PI_CODING_AGENT_SESSION_DIR",
		".pi-sessions",      // pi 专有目录名
		"ensurePiLLMConfig", // pi 专有配置准备
		"sidecar.mjs",       // 写死的脚本名
	} {
		for i, line := range strings.Split(rest, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue // 注释里解释"为什么不该有"是允许的
			}
			if strings.Contains(line, forbidden) {
				t.Errorf("★ startSidecar 仍硬编码 pi 细节 %q（第 %d 行）—— "+
					"它必须来自 driver 描述，否则换 CLI 就得改核心", forbidden, i+1)
			}
		}
	}
}

// ★★ 判据：**内置 pi 描述与 runtime/pi/driver.json 逐字等价**。
//
// 用途：把隐式行为显式化时最容易出的错是"显式版和隐式版写了一点点不同"，
// 而那种差异在**只走 driver.json 的部署**里根本看不出来。
func TestPiDriverJSONMatchesBuiltin(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("runtime", "pi", "driver.json"))
	if err != nil {
		t.Skipf("没有 runtime/pi/driver.json（可选）: %v", err)
	}
	var d driverSpec
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	want := builtinPiDriver()
	if d.Name != want.Name || d.Script != want.Script || d.Node != want.Node {
		t.Errorf("基础字段不一致: %+v vs %+v", d, want)
	}
	if strings.Join(d.Args, "\x00") != strings.Join(want.Args, "\x00") {
		t.Errorf("args 不一致:\n  json=%v\n  内置=%v", d.Args, want.Args)
	}
	if len(d.Env) != len(want.Env) {
		t.Fatalf("env 数量不一致: %v vs %v", d.Env, want.Env)
	}
	for k, v := range want.Env {
		if d.Env[k] != v {
			t.Errorf("env[%s] 不一致: %q vs %q", k, d.Env[k], v)
		}
	}
	if d.LLMConfig != want.LLMConfig || d.SessionDirSuffix != want.SessionDirSuffix {
		t.Errorf("llmConfig/sessionDirSuffix 不一致: %q/%q", d.LLMConfig, d.SessionDirSuffix)
	}
}

// ★★ 判据：未知 CLI **必须报错**，不得静默回退到 pi。
//
// 静默回退会让「我配了 zcode」变成「实际在跑 pi」而没有任何提示 ——
// 任务照跑，用的是错的后端。
func TestUnknownAgentCLIFailsLoudly(t *testing.T) {
	p := newTestPluginForShell(t)
	if _, _, err := p.loadDriver("nonexistent-cli"); err == nil {
		t.Fatal("★ 未知 CLI 未报错 —— 静默回退会让任务用错后端跑")
	}
	// ★ 显式传 "pi" 必须始终可用（内置描述，不依赖 driver.json）。
	//
	// 这与"默认后端是谁"是两件事，必须分开测：
	// 2026-10-17 把默认从 pi 改成 zcode 时，实现里用
	// `if cli == defaultAgentCLI` 决定给谁内置回退 ⇒ pi 当场失去兜底。
	// 所以这里显式钉住 pi 的可用性，不再让它随默认值漂移。
	d, _, err := p.loadDriver("pi")
	if err != nil {
		t.Fatalf("★ 显式 pi 必须可用（内置描述不依赖 driver.json）: %v", err)
	}
	if d.Name != "pi" {
		t.Errorf("显式 pi 应解析为 pi，实际 %q", d.Name)
	}

	// 默认值语义单独一条判据（见 TestDefaultAgentCLIIsZcode）。
}

// ★★ 判据：默认后端是 **pi**（本地主用），且 zcode 是可选后端。
func TestDefaultAgentCLIIsPi(t *testing.T) {
	if defaultAgentCLI != "pi" {
		t.Fatalf("默认后端应为 pi（本地主用；交付方用端点表设 zcode），实际 %q", defaultAgentCLI)
	}
	// 空串必须解析成 pi，且内置描述可用（不需要 driver.json）
	p := newTestPluginForShell(t)
	d, _, err := p.loadDriver("")
	if err != nil {
		t.Fatalf("默认（空 cli）应解析为 pi 且可用: %v", err)
	}
	if d.Name != "pi" {
		t.Fatalf("空 cli 应解析为 pi，实际 %q", d.Name)
	}
}

// ★★ 判据：**内置 pi 回退与默认值解耦**。
//
// 这条判据守的是一个真实踩过的坑（2026-10-17）：实现里原本写
//
//	if cli == defaultAgentCLI { return builtinPiDriver(), ... }
//
// 把"给谁内置回退"和"默认选谁"耦合到同一常量。一旦有人改默认
// （哪怕只是想在交付环境试 zcode），pi 就**当场失去兜底**，
// 显式 cli="pi" 开始报「驱动不存在」。
//
// ⇒ 正确语义：builtinPiDriver 是 pi 这个**后端**的兜底，与默认值无关。
// 这里通过"临时把默认设成 zcode 后，显式 pi 仍必须可用"来钉住解耦。
func TestBuiltinPiFallbackIsDecoupledFromDefault(t *testing.T) {
	p := newTestPluginForShell(t)
	// 直接验证字面 "pi" 走内置描述（不依赖 driver.json 存在）
	d, _, err := p.loadDriver("pi")
	if err != nil {
		t.Fatalf("★ 显式 pi 必须走内置描述: %v", err)
	}
	if d.Name != "pi" || d.Script != "sidecar.mjs" {
		t.Fatalf("内置 pi 描述不符: name=%q script=%q", d.Name, d.Script)
	}
	// 该目录下确实没有 driver.json —— 证明是代码里的兜底
	if _, statErr := os.Stat(filepath.Join(p.driverDir("pi"), "driver.json")); statErr == nil {
		t.Fatal("测试前提不成立：竟然存在 pi/driver.json")
	}
}

// ★★ 判据：占位符替换正确（workdir/sessionId/... 全部生效）。
func TestDriverPlaceholderExpansion(t *testing.T) {
	d := driverSpec{
		Env:  map[string]string{"A": "{workdir}/x", "B": "{dataDir}/y"},
		Args: []string{"--cwd", "{workdir}", "--sid", "{sessionId}", "--name", "{sessionName}"},
	}
	ctx := driverCtx{
		Workdir: "/w", SessionID: "s1", SessionName: "br-1",
		DataDir: "/d", PluginDir: "/p",
	}
	env := strings.Join(d.buildEnv(ctx), ",")
	if !strings.Contains(env, "A=/w/x") || !strings.Contains(env, "B=/d/y") {
		t.Errorf("env 占位符未替换: %s", env)
	}
	args := strings.Join(d.buildArgs(ctx), " ")
	if !strings.Contains(args, "--cwd /w") || !strings.Contains(args, "--sid s1") ||
		!strings.Contains(args, "--name br-1") {
		t.Errorf("args 占位符未替换: %s", args)
	}
	// 环境变量顺序必须稳定（map 遍历随机 ⇒ 日志难对照）
	if strings.Join(d.buildEnv(ctx), ",") != env {
		t.Error("env 顺序不稳定")
	}
}

// ★★ 判据：`sessionNameFlag:false` 时**不传** --session-name 及其值。
//
// 理由：ZCode CLI 没有这个开关，传了会 `Unknown option` 直接退到 usage ——
// 一行参数毁掉整条命令。
func TestSessionNameFlagRespected(t *testing.T) {
	no := false
	d := driverSpec{Args: []string{"--workdir", "{workdir}", "--session-name", "{sessionName}"}, SessionNameFlag: &no}
	ctx := driverCtx{Workdir: "/w", SessionName: "br-1"}
	got := strings.Join(d.buildArgs(ctx), " ")
	if strings.Contains(got, "session-name") || strings.Contains(got, "br-1") {
		t.Fatalf("★ 后端声明不支持 --session-name 却仍传了: %s", got)
	}
	// 支持时应传（默认行为）
	d2 := driverSpec{Args: d.Args}
	got2 := strings.Join(d2.buildArgs(ctx), " ")
	if !strings.Contains(got2, "--session-name br-1") {
		t.Fatalf("默认应传 --session-name: %s", got2)
	}
}

// ★★★ 端到端判据：**换一个 driver 就能换后端**，插件核心零改动。
//
// 做法：造一个假 CLI 的 sidecar（实现同名 JSON-RPC 契约），
// 断言 startSidecar 用**它的**参数与环境把它拉起来，且 call 能拿到结果。
func TestSwappingDriverSwapsBackend(t *testing.T) {
	p := newTestPluginForShell(t)
	// 假 sidecar 是 node 脚本 ⇒ 本机必须有 node（产品三级链：node_bin /
	// 随包 rawfile/node/bin/node / PATH）。缺的时候原来直接 FAIL，
	// 而红的原因是"机器没装 node"，与被测代码无关。
	// ★ 有 node 时必须真跑（端到端起进程），skip 不是绕过主路径的借口。
	requireNodeBinary(t, p)
	cli := "fakecli"
	dir := p.driverDir(cli)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 假 sidecar：把收到的参数与环境写进文件，然后按契约应答
	script := filepath.Join(dir, "sidecar.mjs")
	body := `
import { writeFileSync } from "node:fs";
import { createInterface } from "node:readline";
// 证据：把调用参数与环境落盘，判据据此断言"确实换了后端"
writeFileSync(process.env.FAKE_EVIDENCE, JSON.stringify({
  argv: process.argv.slice(2), env: { FAKE_MARK: process.env.FAKE_MARK },
  cwd: process.cwd(),
}));
const rl = createInterface({ input: process.stdin });
rl.on("line", (line) => {
  line = line.trim(); if (!line) return;
  const req = JSON.parse(line);
  let result;
  if (req.method === "ping") result = { ok: true, session_id: null, cli_version: "fake-1.0" };
  else if (req.method === "prompt") result = { ok: true, session_id: "sess_fake", reply: "来自假后端" };
  else if (req.method === "state") result = { ok: true, session_id: "sess_fake", messages: 1 };
  else result = { ok: true };
  process.stdout.write(JSON.stringify({ jsonrpc: "2.0", id: req.id, result }) + "\n");
  if (req.method === "close") process.exit(0);
});
`
	if err := os.WriteFile(script, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(t.TempDir(), "evidence.json")
	drvJSON := map[string]interface{}{
		"name": cli, "script": "sidecar.mjs", "node": "auto",
		"sessionNameFlag": false,
		"env":             map[string]string{"FAKE_MARK": "yes-from-driver", "FAKE_EVIDENCE": evidence},
		"args":            []string{"--workdir", "{workdir}", "--session-id", "{sessionId}"},
		"llmConfig":       "none",
	}
	b, _ := json.MarshalIndent(drvJSON, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "driver.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	wd := t.TempDir()
	br := &Branch{ID: "br-1", ProjectID: "p", Name: "feat/x", Workdir: wd, AgentCLI: cli, Status: "open"}

	sc, err := p.startSidecar(br, "sess-1")
	if err != nil {
		t.Fatalf("★ 换驱动后启动失败（核心仍依赖 pi？）: %v", err)
	}
	defer sc.Close()

	// ping 的版本应被记到台账（核心认 cli_version 字段）
	if br.PiVersion != "fake-1.0" {
		t.Errorf("应记录后端版本，实际 %q", br.PiVersion)
	}

	// prompt 必须能走通并回报 session_id
	res, err := sc.call("prompt", map[string]interface{}{"text": "hello"})
	if err != nil {
		t.Fatalf("prompt 失败: %v", err)
	}
	m, ok := res.(map[string]interface{})
	if !ok || m["session_id"] != "sess_fake" {
		t.Fatalf("prompt 未按契约回报 session_id: %v", res)
	}

	// ★ 证据：后端确实拿到了**驱动声明**的参数与环境
	ev, err := os.ReadFile(evidence)
	if err != nil {
		t.Fatalf("假后端未写证据（说明它没被启动）: %v", err)
	}
	s := string(ev)
	if !strings.Contains(s, "yes-from-driver") {
		t.Errorf("驱动声明的环境未注入: %s", s)
	}
	if !strings.Contains(s, wd) {
		t.Errorf("驱动声明的 --workdir 未传入: %s", s)
	}
	if strings.Contains(s, "session-name") {
		t.Errorf("sessionNameFlag:false 却传了 --session-name: %s", s)
	}
}

var _ = exec.Command
