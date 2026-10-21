package main

// SSH 传输与"接入其他 CLI"的接线判据。
//
// 背景（实测发现的缺口）：`sidecar_ssh.go` 的实现是完整的，
// `startSidecar` 里的 `if b.Runtime == "ssh"` 分派也是接好的，
// 但 `branch_open` **硬编码 `Runtime: "local"`** 且从不读 `ssh_host` ——
// 于是 `runtime=ssh` 被**静默吞掉**：用户以为跑在远端，实际跑在本地，
// 且没有任何提示。`runtimeOrDefault` 当时更是**从未被调用**的死代码。
//
// 这类"参数被吞"比报错危险得多：报错会立刻发现，吞掉会一路错下去。

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// ★★★ 判据：`runtime=ssh` 被真正记进台账（不再被吞）。
func TestSSHRuntimeIsPersistedToLedger(t *testing.T) {
	p := newTestPluginForAddr(t)
	p.ledger.Projects["proj"] = &Project{Name: "proj", RemoteURL: "/tmp/x", DefaultBranch: "main"}

	r, _ := p.handleBranchOpen(map[string]interface{}{
		"project": "proj", "branch": "feat/remote",
		"runtime": "ssh", "ssh_host": "build@10.0.0.9",
	})
	m := r.(map[string]interface{})
	if _, isErr := m["error"]; isErr {
		t.Fatalf("开远端分支不应报错: %v", m)
	}
	id := m["branch_id"].(string)
	b := p.ledger.Branches[id]
	if b == nil {
		t.Fatal("台账里没有这条分支")
	}
	if b.Runtime != "ssh" {
		t.Errorf("★ runtime 未被记录: got %q, want \"ssh\"（参数被吞）", b.Runtime)
	}
	if b.SSHHost != "build@10.0.0.9" {
		t.Errorf("★ ssh_host 未被记录: got %q", b.SSHHost)
	}
}

// ★★ 判据：`runtime=ssh` 但没给 ssh_host → **响亮拒绝**。
//
// 不拒的话 startSSHSidecar 会在 parseSSHTarget 里失败，
// 但那是"拉起时"，到那时用户已经等了一轮；且错误离原因更远。
func TestSSHWithoutHostIsRejected(t *testing.T) {
	p := newTestPluginForAddr(t)
	p.ledger.Projects["proj"] = &Project{Name: "proj", RemoteURL: "/tmp/x", DefaultBranch: "main"}
	r, _ := p.handleBranchOpen(map[string]interface{}{
		"project": "proj", "branch": "feat/nohost", "runtime": "ssh",
	})
	m := r.(map[string]interface{})
	e, isErr := m["error"]
	if !isErr {
		t.Fatal("★ runtime=ssh 缺 ssh_host 应被拒绝")
	}
	if !strings.Contains(e.(string), "ssh_host") {
		t.Errorf("错误应指明缺 ssh_host: %v", e)
	}
}

// ★★★ 判据：**未知 runtime 响亮拒绝，不静默降级为 local**。
//
// 这是本仓的核心纪律：静默降级会让"我以为跑在远端"的假设
// 一路错下去，而症状（任务跑在本地、文件在本地被改）非常难归因。
func TestUnknownRuntimeFailsLoudly(t *testing.T) {
	p := newTestPluginForAddr(t)
	p.ledger.Projects["proj"] = &Project{Name: "proj", RemoteURL: "/tmp/x", DefaultBranch: "main"}
	r, _ := p.handleBranchOpen(map[string]interface{}{
		"project": "proj", "branch": "feat/bad", "runtime": "k8s",
	})
	m := r.(map[string]interface{})
	e, isErr := m["error"]
	if !isErr {
		t.Fatal("★ 未知 runtime 应被拒绝（不静默降级）")
	}
	if !strings.Contains(e.(string), "k8s") {
		t.Errorf("错误应回显收到的值: %v", e)
	}
	// 不得在台账里留下 half-open 的条目
	if len(p.ledger.Branches) != 0 {
		t.Errorf("★ 被拒的分支不应进台账，实际 %d 条", len(p.ledger.Branches))
	}
}

// ★★ 判据：默认（不给 runtime）仍是 local —— 老调用方不受影响。
func TestRuntimeDefaultsToLocal(t *testing.T) {
	p := newTestPluginForAddr(t)
	p.ledger.Projects["proj"] = &Project{Name: "proj", RemoteURL: "/tmp/x", DefaultBranch: "main"}
	r, _ := p.handleBranchOpen(map[string]interface{}{"project": "proj", "branch": "feat/d"})
	id := r.(map[string]interface{})["branch_id"].(string)
	if got := p.ledger.Branches[id].Runtime; got != "local" {
		t.Errorf("默认应为 local，实际 %q", got)
	}
}

// ★ 判据：大小写与空白被规范化（'SSH' / ' ssh ' 都认）。
func TestRuntimeIsNormalized(t *testing.T) {
	for _, in := range []string{"SSH", " ssh ", "Ssh"} {
		p := newTestPluginForAddr(t)
		p.ledger.Projects["proj"] = &Project{Name: "proj", RemoteURL: "/tmp/x", DefaultBranch: "main"}
		r, _ := p.handleBranchOpen(map[string]interface{}{
			"project": "proj", "branch": "feat/n", "runtime": in, "ssh_host": "u@h",
		})
		m := r.(map[string]interface{})
		if _, isErr := m["error"]; isErr {
			t.Errorf("runtime=%q 应被识别为 ssh: %v", in, m)
			continue
		}
		id := m["branch_id"].(string)
		if got := p.ledger.Branches[id].Runtime; got != "ssh" {
			t.Errorf("runtime=%q 规范化后应为 ssh，实际 %q", in, got)
		}
	}
}

// ★★★ 判据：**接入新 CLI 不需要改插件核心**。
//
// 这是 sidecar driver 层的立身之本：用户放一个
// `<插件数据>/runtime/<cli>/driver.json` + 脚本即可新增后端。
// 判据必须在**不碰任何 Go 源码**的前提下把一个假 CLI 跑起来。
func TestAddingNewCLIRequiresNoCoreChange(t *testing.T) {
	p := newTestPluginForShell(t)
	// 造一个假 CLI 的 driver 目录
	cliDir := p.driverDir("fakecli")
	if err := os.MkdirAll(cliDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 假 CLI 的 sidecar：实现最小契约（ping/prompt/state）
	script := `#!/usr/bin/env node
// 极简 sidecar：只支持 ping / prompt
const rl=require('readline').createInterface({input:process.stdin});
rl.on('line',l=>{let q;try{q=JSON.parse(l)}catch(e){return}
 if(q.method==='ping'){process.stdout.write(JSON.stringify({id:q.id,result:{ok:true,session_id:null,cli_version:'fake-9.9'}})+'\n')}
 else if(q.method==='prompt'){process.stdout.write(JSON.stringify({id:q.id,result:{ok:true,session_id:q.params.session_id,reply:'fake'}})+'\n')}
 else {process.stdout.write(JSON.stringify({id:q.id,result:{ok:true}})+'\n')}});
`
	if err := os.WriteFile(filepath.Join(cliDir, "sidecar.mjs"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	drv := `{"name":"fakecli","script":"sidecar.mjs","node":"auto","llmConfig":"none",
  "sessionDirSuffix":".fake-sessions","args":["--session-id","{sessionId}"]}`
	if err := os.WriteFile(filepath.Join(cliDir, "driver.json"), []byte(drv), 0o644); err != nil {
		t.Fatal(err)
	}

	// ① loadDriver 必须能读到它（不报"驱动不存在"）
	d, dir, err := p.loadDriver("fakecli")
	if err != nil {
		t.Fatalf("★ 新增 CLI 的 driver 未被识别: %v", err)
	}
	if d.Name != "fakecli" {
		t.Errorf("驱动名不对: %q", d.Name)
	}
	if !strings.HasSuffix(dir, filepath.Join("runtime", "fakecli")) {
		t.Errorf("驱动目录不对: %s", dir)
	}
	// ② LLMConfig=none 必须透传（fake CLI 自管凭据，插件不去写 pi 配置）
	if d.LLMConfig != "none" {
		t.Errorf("★ llmConfig 未透传: %q（会导致去改 pi 的配置）", d.LLMConfig)
	}
	// ③ 占位符按该 driver 的声明展开
	ctx := driverCtx{SessionID: "sess-1", Workdir: "/w", SessionName: "n", DataDir: "/d", PluginDir: "/p"}
	args := d.buildArgs(ctx)
	if len(args) != 2 || args[1] != "sess-1" {
		t.Errorf("占位符未按该 driver 展开: %v", args)
	}
}

// ★★ 判据：**内置 pi 在没有 driver.json 时仍可用**（老部署不受影响）。
//
// 这条是"新增 driver 层不破坏既有行为"的保证。
func TestBuiltinPiStillWorksWithoutDriverJSON(t *testing.T) {
	p := newTestPluginForShell(t)
	d, _, err := p.loadDriver("pi")
	if err != nil {
		t.Fatalf("内置 pi 不应需要 driver.json: %v", err)
	}
	if d.LLMConfig != "pi" {
		t.Errorf("内置 pi 的 llmConfig 应为 pi，实际 %q", d.LLMConfig)
	}
	if len(d.Args) == 0 {
		t.Error("内置 pi 应带默认 args")
	}
}

// ★★ 判据：未知 CLI **不静默回退到 pi**。
func TestUnknownCLIDoesNotFallBackToPi(t *testing.T) {
	p := newTestPluginForShell(t)
	_, _, err := p.loadDriver("nosuchcli")
	if err == nil {
		t.Fatal("★ 未知 CLI 应报错（静默回退会用错后端跑任务）")
	}
	if !strings.Contains(err.Error(), "不静默回退") {
		t.Errorf("错误应说明不回退: %v", err)
	}
}

// ★★ 判据：`agent_state` 必须回显 ssh_host（远端线要能看出打到哪台机）。
//
// 不回显的话远端线与本地线状态一模一样，排查时分不清任务跑在哪。
func TestAgentStateShowsSSHHost(t *testing.T) {
	p := newTestPluginForAddr(t)
	p.ledger.Projects["proj"] = &Project{Name: "proj", RemoteURL: "/tmp/x", DefaultBranch: "main"}
	r, _ := p.handleBranchOpen(map[string]interface{}{
		"project": "proj", "branch": "feat/r", "runtime": "ssh", "ssh_host": "build@10.0.0.9",
	})
	id := r.(map[string]interface{})["branch_id"].(string)

	st, _ := p.handleAgentState(map[string]interface{}{"branch_id": id})
	m := st.(map[string]interface{})
	if m["runtime"] != "ssh" {
		t.Errorf("runtime 未回显: %v", m["runtime"])
	}
	if m["ssh_host"] != "build@10.0.0.9" {
		t.Errorf("★ ssh_host 未回显（远端线看不出打到哪台机）: %v", m["ssh_host"])
	}
	// 本地线不应出现 ssh_host
	r2, _ := p.handleBranchOpen(map[string]interface{}{"project": "proj", "branch": "feat/l"})
	id2 := r2.(map[string]interface{})["branch_id"].(string)
	st2, _ := p.handleAgentState(map[string]interface{}{"branch_id": id2})
	if _, has := st2.(map[string]interface{})["ssh_host"]; has {
		t.Error("本地线不应回显 ssh_host")
	}
}

// testRawFiles 是 RawFilesAPI 的最小替身（判据用）。
//
// 直接实现接口而不是复用内核实现：内核在 internal/，插件（外部）不能 import；
// 且这里只需 Exists/Dir 两个方法。
type testRawFiles struct{ root string }

func (t *testRawFiles) Read(rel string) ([]byte, error) {
	return os.ReadFile(filepath.Join(t.root, rel))
}
func (t *testRawFiles) List(rel string) ([]string, error) {
	es, err := os.ReadDir(filepath.Join(t.root, rel))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(es))
	for _, e := range es {
		n := e.Name()
		if e.IsDir() {
			n += "/"
		}
		out = append(out, n)
	}
	return out, nil
}
func (t *testRawFiles) Exists(rel string) bool {
	st, err := os.Stat(filepath.Join(t.root, rel))
	return err == nil && !st.IsDir()
}
func (t *testRawFiles) Dir() string { return t.root }
func (t *testRawFiles) WorkDir() string {
	d := filepath.Join(filepath.Dir(t.root), "work")
	os.MkdirAll(d, 0o755)
	return d
}

// ★★★ 判据：**sidecar 脚本必须随包分发**，且查找链能找到它。
//
// 实测发现的真缺陷（2026-10-15）：打包只收 `rawfile/` 与 `skill/`，
// 而 `sidecar.mjs` 在**工程根** ⇒ 没进包。同时 `runtime/pi/` 在部署时
// 没人创建 ⇒ 插件的数据目录里也没有脚本。
//
// 后果：插件装得上、工具都在、但 `agent_up` **必然失败**，报
// 「找不到 pi 的 sidecar 脚本: /home/xxx/plugin_data/commondev/runtime/pi/sidecar.mjs」
// —— 一个用户从没听说过的路径。属于"能装不能跑"。
func TestSidecarScriptShipsWithPackage(t *testing.T) {
	// ① 脚本必须在 rawfile/ 下（随包分发通道）
	rel := filepath.Join("rawfile", "runtime", "pi", "sidecar.mjs")
	if _, err := os.Stat(rel); err != nil {
		t.Fatalf("★ sidecar 脚本未随包分发（应放在 %s）: %v", rel, err)
	}
	// ② 内容必须与工程根的权威副本一致（防止改了根那份忘了同步）
	root, err := os.ReadFile("sidecar.mjs")
	if err != nil {
		t.Skipf("读不到根 sidecar.mjs: %v", err)
	}
	shipped, err := os.ReadFile(rel)
	if err != nil {
		t.Fatal(err)
	}
	if string(root) != string(shipped) {
		t.Error("★ rawfile/ 里的 sidecar.mjs 与工程根不一致（改了根那份没同步？）")
	}
}

// ★★★ 判据：**查找链能找到随包脚本**（这是 agent_up 能跑的前提）。
//
// 直接调 locateSidecarScript —— 判据必须测真实入口，不能只检查文件存在。
func TestLocateSidecarScriptFindsShippedCopy(t *testing.T) {
	p := newFlowPlugin(t)
	// 让 rawFiles() 指向工程目录（模拟解包后的插件目录）
	root, _ := os.Getwd()
	p.sdk.SetRawFilesAPI(&testRawFiles{root: filepath.Join(root, "rawfile")})

	drv := builtinPiDriver()
	// cliDir 指向一个**不存在**的目录（模拟全新部署没人建 runtime/pi）
	cliDir := filepath.Join(t.TempDir(), "runtime", "pi")

	got, err := p.locateSidecarScript(drv, cliDir, "pi")
	if err != nil {
		t.Fatalf("★ 找不到随包的 sidecar 脚本（agent_up 会失败）: %v", err)
	}
	if !strings.HasSuffix(got, filepath.Join("runtime", "pi", "sidecar.mjs")) {
		t.Errorf("找到的路径不对: %s", got)
	}
	if _, err := os.Stat(got); err != nil {
		t.Errorf("★ 返回的路径不可读: %v", err)
	}
}

// ★★ 判据：**用户自备的脚本优先**于随包副本（允许覆盖）。
func TestUserProvidedSidecarWins(t *testing.T) {
	p := newFlowPlugin(t)
	root, _ := os.Getwd()
	p.sdk.SetRawFilesAPI(&testRawFiles{root: filepath.Join(root, "rawfile")})

	// 造一个用户自己的 runtime/pi/sidecar.mjs
	cliDir := filepath.Join(t.TempDir(), "runtime", "pi")
	if err := os.MkdirAll(cliDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(cliDir, "sidecar.mjs")
	if err := os.WriteFile(mine, []byte("// 我的版本"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := p.locateSidecarScript(builtinPiDriver(), cliDir, "pi")
	if err != nil {
		t.Fatal(err)
	}
	if got != mine {
		t.Errorf("★ 用户自备脚本应优先，实际用了 %s", got)
	}
}

// ★★ 判据：全部落空时，错误必须**列出找过哪里**（否则用户只能猜）。
func TestMissingSidecarErrorIsActionable(t *testing.T) {
	p := newFlowPlugin(t) // 无 rawFiles
	cliDir := filepath.Join(t.TempDir(), "runtime", "nope")
	_, err := p.locateSidecarScript(driverSpec{Script: "sidecar.mjs"}, cliDir, "nope")
	if err == nil {
		t.Fatal("找不到脚本应报错")
	}
	msg := err.Error()
	for _, want := range []string{"rawfile", "runtime", "可执行文件"} {
		if !strings.Contains(msg, want) {
			t.Errorf("★ 错误信息应说明找过 %q: %s", want, msg)
		}
	}
	// 必须告诉用户**怎么办**
	if !strings.Contains(msg, "放到") {
		t.Errorf("★ 错误信息应给出解决办法: %s", msg)
	}
}

// ★★★ 判据：zcode sidecar 的 **CLI 路径必须可声明**（本地与远端各一套）。
//
// 实测缺口（2026-10-16）：zcode 的 sidecar 只在三个固定位置碰运气找 CLI，
// 且 driver.json 里**没有**声明路径的字段 ⇒ 本机装法不同就必然
// 「找不到 ZCode CLI」，用户只能记得设环境变量。SSH 场景更糟：
// 本地与远端的路径本来就不同，而当时只有一个全局 env 可用。
func TestZcodeCLIPathIsDeclarable(t *testing.T) {
	// ① driver.json 必须声明 cliPath
	b, err := os.ReadFile("rawfile/runtime/zcode/driver.json")
	if err != nil {
		t.Fatalf("zcode driver.json 不存在: %v", err)
	}
	var d driverSpec
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("driver.json 解析失败: %v", err)
	}
	if strings.TrimSpace(d.CLIPath) == "" {
		t.Fatal("★ driver.json 未声明 cliPath（用户只能靠环境变量，且 SSH 无法分别指定）")
	}
	if !strings.Contains(d.CLIPath, "{rawfile}") {
		t.Errorf("cliPath 应支持随包分发（含 {rawfile} 占位符）: %s", d.CLIPath)
	}

	// ② {rawfile} 占位符必须能被展开
	ctx := driverCtx{
		Workdir: "/w", SessionID: "s", SessionName: "n",
		DataDir: "/d", PluginDir: "/p", RawfileDir: "/rf",
	}
	args := d.buildArgs(ctx)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--cli /rf/zcode/zcode.cjs") {
		t.Errorf("★ cliPath 未展开成 --cli 参数: %v", args)
	}
}

// ★★★ 判据：**首轮不得带 --resume**（zcode 真实会话 id 首轮才产生）。
//
// 实测真缺陷（2026-10-16）：sidecar 把插件传来的 `--session-id`
// （形如 `br-<project>-<branch>`，是**插件自己的标签**）直接当作
// `--resume` 的目标，于是首轮就执行 `--resume br-xxx`，zcode 报
// `Session not found: br-xxx` ⇒ **第一次投递必定失败**。
//
// 契约（参考实现与本文件注释都写明）：首轮不传 --resume，
// 从 `{type:"result"}` 行取回 `sess_...`，之后才用它续谈。
func TestZcodeSidecarDoesNotResumeOnFirstTurn(t *testing.T) {
	src, err := os.ReadFile("rawfile/runtime/zcode/sidecar.mjs")
	if err != nil {
		t.Skipf("读不到 zcode sidecar: %v", err)
	}
	s := string(src)
	// 必须存在区分「插件标签」与「真实会话 id」的两个变量。
	if !strings.Contains(s, "desiredSessionId") || !strings.Contains(s, "zcodeSessionId") {
		t.Fatal("★ sidecar 未区分插件标签与 zcode 真实会话 id（首轮会错误 resume）")
	}
}

// ★★★ **行为判据**：用假 CLI 真跑 sidecar，检查它实际拼出的 argv。
//
// 为什么不能只看源码字符串（本会话第 7 次同类教训）：最初的判据 ④ 匹配的是
// `if (params?.session_id) sessionId = String(params.session_id)` 这个**字面串**，
// 把变量改名为 zcodeSessionId 就轻松绕过 —— 判据全绿而缺陷照样存在。
//
// 实测真缺陷（2026-10-16）：sidecar 把插件的标签 `br-xxx` 直接当 --resume 目标，
// 首轮即 `Session not found: br-xxx`，第一次投递必定失败。
//
// 这里用一个**假 CLI**记录收到的 argv 并返回合法的 result 行，从而断言真实行为：
//
//	· 首轮 argv **不含** --resume，且 --cli 指向真 CLI
//	· 第二轮（把首轮返回的 sess_... 传回）**必须**含 --resume sess_...
func TestZcodeSidecarArgvBehavior(t *testing.T) {
	node := findNodeForTest(t)
	dir := t.TempDir()
	rec := filepath.Join(dir, "argv.txt")

	// 假 CLI：记录 argv，输出 sidecar 期望的 result 行。
	fake := filepath.Join(dir, "fake-zcode.cjs")
	if err := os.WriteFile(fake, []byte(`const fs=require("fs");
const out=process.env.FAKE_ARGV_FILE;
fs.appendFileSync(out, JSON.stringify(process.argv.slice(2))+"\n");
const i=process.argv.indexOf("--resume");
const sid = i>=0 ? process.argv[i+1] : "sess_FIRSTTURN";
process.stdout.write(JSON.stringify({type:"result",sessionId:sid,response:"ok"})+"\n");
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// ★ 必须绝对路径：下面把 cmd.Dir 设成 work，相对路径会解析到 work 下。
	sidecar, err := filepath.Abs(filepath.Join("rawfile", "runtime", "zcode", "sidecar.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sidecar); err != nil {
		t.Skipf("sidecar 不存在: %v", err)
	}

	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(node, sidecar,
		"--workdir", work,
		"--session-id", "br-demo-label",
		"--mode", "guarded",
		"--cli", fake)
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "FAKE_ARGV_FILE="+rec)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	mustWrite := func(v any) {
		b, _ := json.Marshal(v)
		if _, err := stdin.Write(append(b, '\n')); err != nil {
			t.Fatalf("写 stdin 失败: %v", err)
		}
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	readReply := func(id int) map[string]any {
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			if int(m["id"].(float64)) == id {
				return m
			}
		}
		t.Fatalf("等 id=%d 的回复超时", id)
		return nil
	}

	// readNotification 读一条**主动通知**（id=0）。
	//
	// 异步投递后，任务完成不是通过请求应答返回，而是 sidecar 主动推一帧
	// id=0 的通知（见 sidecar.go readLoop 的分派）。
	readNotification := func(t *testing.T) map[string]any {
		t.Helper()
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			if id, _ := m["id"].(float64); id == 0 {
				res, _ := m["result"].(map[string]any)
				return res
			}
		}
		t.Fatal("等完成通知超时")
		return nil
	}

	// 首轮
	mustWrite(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "prompt",
		"params": map[string]any{"session_id": "br-demo-label", "text": "hi"}})
	r1 := readReply(1)
	if r1["error"] != nil {
		t.Fatalf("首轮失败: %v", r1["error"])
	}
	res1, _ := r1["result"].(map[string]any)
	// ★ 异步投递（2026-10-17）：prompt 立即回 "accepted"，此时**还拿不到**
	// 真实 sess_ id（它要等 zcode 真正建好会话）。真实 id 随完成通知推回。
	// 首轮这里只要求"受理成功且回报了一个 session_id 字段"。
	if st, _ := res1["status"].(string); st != "accepted" {
		t.Fatalf("首轮应回 status=accepted（异步投递），实际 %q", st)
	}
	sid, _ := res1["session_id"].(string)
	if sid == "" {
		t.Fatal("★ 首轮未回报 session_id 字段（Go 侧据此断言会话未漂移）")
	}

	// 等完成通知（id=0），从里面取真实 sess_ id
	notif := readNotification(t)
	realSid, _ := notif["session_id"].(string)
	if !strings.HasPrefix(realSid, "sess_") {
		t.Fatalf("★ 完成通知里未含真实 sess_ id: %q（通知=%v）", realSid, notif)
	}
	sid = realSid

	// 第二轮：把真实 id 传回 → 必须 --resume
	mustWrite(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "prompt",
		"params": map[string]any{"session_id": sid, "text": "again"}})
	if r2 := readReply(2); r2["error"] != nil {
		t.Fatalf("第二轮失败: %v", r2["error"])
	}
	// 第二轮同样是异步：应答只代表"已受理"，假 CLI 的第二次执行发生在
	// 完成通知之前。必须等通知，否则读 argv 时第二次还没被调起（实测）。
	_ = readNotification(t)

	raw, err := os.ReadFile(rec)
	if err != nil {
		t.Fatalf("假 CLI 未记录 argv: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 2 {
		t.Fatalf("假 CLI 应被调用 2 次，实际 %d: %v", len(lines), lines)
	}

	var a1, a2 []string
	_ = json.Unmarshal([]byte(lines[0]), &a1)
	_ = json.Unmarshal([]byte(lines[1]), &a2)

	// ★ 首轮绝不能 resume
	if i := indexOf(a1, "--resume"); i >= 0 {
		t.Errorf("★ 首轮带了 --resume %q —— zcode 会报 Session not found，第一次投递必定失败", a1[i+1])
	}
	// ★ 第二轮必须 resume 真实 id
	i := indexOf(a2, "--resume")
	if i < 0 || a2[i+1] != sid {
		t.Errorf("★ 第二轮应 --resume %q，实际 argv: %v", sid, a2)
	}
	// --cli 是 sidecar 自己的参数（被它消费，不转发给 CLI）。
	// 「假 CLI 真的被调起来」这件事本身就是 --cli 生效的证明：
	// 上面的 rec 文件有两行 argv，只有 sidecar 找到并执行了 fake 才可能。
	// 这里额外确认 argv 里没有残留的 --cli（若被转发，真 CLI 会不认识它）。
	if j := indexOf(a1, "--cli"); j >= 0 {
		t.Errorf("--cli 不应转发给后端 CLI: %v", a1)
	}
	// 且 --output-format stream-json 必须在（sidecar 依赖末行 result）
	if j := indexOf(a1, "--output-format"); j < 0 || a1[j+1] != "stream-json" {
		t.Errorf("★ 缺少 --output-format stream-json（拿不到 result 行）: %v", a1)
	}
}

func indexOf(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}

func findNodeForTest(t *testing.T) string {
	t.Helper()
	for _, c := range []string{"node", "nodejs"} {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	for _, p := range []string{"/usr/local/bin/node", "/usr/bin/node"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("本机无 node，跳过行为判据")
	return ""
}

// ★★★ 判据：**随包分发的 driver.json 必须能被读到**。
//
// 实测真缺陷（2026-10-16）：`locateSidecarScript` 早就有三级链
// （用户目录 → rawfile → 可执行文件目录），但 `loadDriver` **只看插件数据目录**。
// 于是随包分发的 `rawfile/runtime/zcode/driver.json` 永远读不到 ⇒
// 全新部署上 `agent_cli=zcode` 报「驱动不存在」，而文件就在包里 ——
// 与 sidecar 脚本当年踩的坑是**同一个**，只是漏在了描述文件上。
//
// 判据必须覆盖**两条来源**且断言二者解析结果一致（同一套默认值填充），
// 否则会出现"本机覆盖能用、随包分发不能用"这种只在部分机器复现的问题。
func TestShippedDriverIsLoadable(t *testing.T) {
	// ① 随包确实带了 zcode 的 driver.json
	rfDir, err := filepath.Abs(filepath.Join("rawfile", "runtime", "zcode"))
	if err != nil {
		t.Fatal(err)
	}
	shipped := filepath.Join(rfDir, "driver.json")
	if _, err := os.Stat(shipped); err != nil {
		t.Fatalf("★ 随包缺少 zcode driver.json（新鲜部署上 zcode 不可用）: %v", err)
	}

	// ② 通过真实 SDK 的 RawFiles 接口读到它
	//    （loadShippedDriver 走的就是 p.sdk.RawFiles()，路径拼接错了这里会红）
	// rfDir = <commondev>/rawfile/runtime/zcode
	//   三次 Dir：zcode → runtime → rawfile → <commondev>
	pluginDir := filepath.Dir(filepath.Dir(filepath.Dir(rfDir))) // .../commondev
	p := newTestPluginWithRawfiles(t, filepath.Join(pluginDir, "rawfile"))
	rel := filepath.ToSlash(filepath.Join("runtime", "zcode", "driver.json"))
	if !p.rawFiles().Exists(rel) {
		t.Fatalf("★ rawFiles 读不到随包 driver.json（rel=%s）", rel)
	}
	d, ok := p.loadShippedDriver("zcode")
	if !ok {
		t.Fatal("★ loadShippedDriver 未识别随包 driver.json")
	}

	// ③ 默认值填充必须与"用户目录"来源一致
	//    造一个最小描述，比较两条路径的填充结果
	dir := t.TempDir()
	userPath := filepath.Join(dir, "runtime", "demo", "driver.json")
	if err := os.MkdirAll(filepath.Dir(userPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPath, []byte(`{"name":"demo","args":["--x"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(userPath)
	var dUser driverSpec
	if err := json.Unmarshal(raw, &dUser); err != nil {
		t.Fatal(err)
	}
	a := p.finishDriver(dUser, "demo")
	b := p.finishDriver(driverSpec{Name: "demo", Args: []string{"--x"}}, "demo")
	// driverSpec 含 map，不能直接比较；逐字段比关键项
	if a.Name != b.Name || a.Script != b.Script || a.LLMConfig != b.LLMConfig ||
		a.SessionDirSuffix != b.SessionDirSuffix || a.CLIPath != b.CLIPath {
		t.Errorf("★ 两条来源的默认值填充不一致:\n user=%+v\n shipped=%+v", a, b)
	}
	// 默认后缀必须按 cli 派生（不能写死某个后端）
	if a.SessionDirSuffix != ".demo-sessions" {
		t.Errorf("★ 默认 sessionDirSuffix 必须按 cli 派生，实际 %q", a.SessionDirSuffix)
	}
	// ④ ★ **必须走真实入口 loadDriver**（这是本会话第 8 次同类教训：
	//    只测辅助函数会漏掉"入口没接上回退"这种缺陷 —— 变异验证时，
	//    删掉 loadDriver 里的 loadShippedDriver 调用，辅助函数判据依然全绿）。
	//
	//    这里把插件数据目录指向一个**空的**临时目录，逼 loadDriver
	//    只能从随包 rawfile 取到描述。
	rawRoot := filepath.Join(pluginDir, "rawfile")
	tmp := t.TempDir()
	// driverDir = <插件数据>/runtime/<cli>；把 p.path 指到 tmp 下
	p2 := newTestPluginWithRawfiles(t, rawRoot)
	p2.path = filepath.Join(tmp, "commondev")
	got, _, err := p2.loadDriver("zcode")
	if err != nil {
		t.Fatalf("★ loadDriver('zcode') 走真实入口失败 —— 随包 driver.json 未被回退读取: %v", err)
	}
	if got.Name != "zcode" || got.CLIPath == "" {
		t.Errorf("★ 从随包取到的描述不完整: %+v", got)
	}
	// 且证明用户目录确实是空的（否则上一步可能只是在读用户目录）
	if _, statErr := os.Stat(filepath.Join(p2.driverDir("zcode"), "driver.json")); statErr == nil {
		t.Fatal("判据前提被破坏：用户目录不该有 driver.json")
	}

	// ⑤ 随包 zcode 描述解析出的关键字段
	if d.Script != "sidecar.mjs" {
		t.Errorf("zcode script=%q", d.Script)
	}
	if d.LLMConfig != "none" {
		t.Errorf("zcode 不得注入 LLM 配置（它用自己的 provider 配置），实际 %q", d.LLMConfig)
	}
	if d.CLIPath == "" {
		t.Error("zcode 必须声明 cliPath")
	}
}

// newTestPluginWithRawfiles 造一个注入了真实 RawFiles 接口的插件实例。
//
// 为什么不用裸 &Plugin{}：loadShippedDriver 走的是 p.sdk.RawFiles()，
// 裸实例 sdk==nil 会让判据永远走"没有随包资源"的分支而**假绿**。
func newTestPluginWithRawfiles(t *testing.T, dir string) *Plugin {
	t.Helper()
	sdkp := sdk.New("commondev-test", nil, nil, nil, nil, nil)
	sdkp.SetRawFilesAPI(&fsRawFiles{root: dir})
	return &Plugin{sdk: sdkp}
}

// fsRawFiles 是最小的 RawFilesAPI 实现（只为判据服务）。
//
// ★ 用**真文件系统**而不是内存桩：被测的是"路径怎么拼、读哪个文件"，
// 内存桩会把拼接错误一起吞掉（例如 rel 拼错但桩照样返回内容）。
type fsRawFiles struct{ root string }

func (f *fsRawFiles) resolve(rel string) string {
	return filepath.Join(f.root, filepath.FromSlash(rel))
}
func (f *fsRawFiles) Read(rel string) ([]byte, error) { return os.ReadFile(f.resolve(rel)) }
func (f *fsRawFiles) Exists(rel string) bool          { _, err := os.Stat(f.resolve(rel)); return err == nil }
func (f *fsRawFiles) Dir() string                     { return f.root }
func (f *fsRawFiles) WorkDir() string                 { return f.root }
func (f *fsRawFiles) List(rel string) ([]string, error) {
	ents, err := os.ReadDir(f.resolve(rel))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() {
			n += "/"
		}
		out = append(out, n)
	}
	return out, nil
}

// ★★★ **行为判据**：声明的 CLI 路径不存在时，错误必须**可执行**。
//
// 实测真缺陷（2026-10-16）：随包 driver.json 声明 `{rawfile}/zcode/zcode.cjs`，
// 但包里没有这个文件（28MB，按需分发）。旧实现把声明值**直接返回**，
// 于是 Node 在 spawn 时抛 `Module._resolveFilename` 堆栈 ——
// 用户完全看不出"CLI 不存在"，也不知道去哪儿配。
//
// 判据跑真实 sidecar（假 node 环境不必要：本判据只要求它**快速失败并给出信息**），
// 断言错误文本里同时出现：① "找不到 ZCode CLI" ② 声明过的那个路径 ③ 修复指引。
func TestZcodeMissingCLIErrorIsActionable(t *testing.T) {
	node := findNodeForTest(t)
	sidecar, err := filepath.Abs(filepath.Join("rawfile", "runtime", "zcode", "sidecar.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	missing := filepath.Join(t.TempDir(), "definitely-not-here", "zcode.cjs")

	cmd := exec.Command(node, sidecar,
		"--workdir", work,
		"--session-id", "br-x",
		"--mode", "guarded",
		"--cli", missing)
	cmd.Dir = work
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
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	w := bufio.NewWriter(stdin)
	rd := bufio.NewReader(stdout)
	mustJSON := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = w.Write(append(b, '\n'))
		_ = w.Flush()
	}
	mustJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "prompt",
		"params": map[string]any{"session_id": "br-x", "text": "hi"}})

	var got string
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("没等到错误回复: %v", err)
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if e, ok := m["error"].(map[string]any); ok {
			got, _ = e["message"].(string)
			break
		}
	}
	// ① 说清是什么问题
	if !strings.Contains(got, "找不到 ZCode CLI") {
		t.Errorf("★ 错误没点明'找不到 CLI': %q", got)
	}
	// ② 列出**声明过的那个路径**（用户据此知道 driver.json 生效了）
	if !strings.Contains(got, missing) {
		t.Errorf("★ 错误未列出声明过的路径 %s: %q", missing, got)
	}
	// ③ 给出修复动作
	if !strings.Contains(got, "driver.json") || !strings.Contains(got, "ZCODE_CLI") {
		t.Errorf("★ 错误未给出修复指引: %q", got)
	}
	// ④ 绝不能是 Node 的模块解析堆栈（那正是被修掉的表现）
	if strings.Contains(got, "Module._resolveFilename") {
		t.Errorf("★ 仍在暴露 Node 内部堆栈（用户无法定位）: %q", got)
	}
}
