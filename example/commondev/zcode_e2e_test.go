//go:build zcode_e2e

// ZCode 后端的**真机**端到端判据。
//
// 与 pi 的 e2e 同一形状，但驱动的是**随包分发的** driver.json + sidecar.mjs
// 与**真实 ZCode CLI** —— 而不是我的临时 harness。要求真实改动工程文件，
// 且不破坏原有代码。
//
// 跑法（需要本机已能跑通 zcode）：
//
//	HA_ZCODE_CLI=<zcode.cjs 路径> \
//	HA_ZCODE_DATA_DIR=<zcode 数据目录> \
//	go test -tags zcode_e2e -run TestZcodeFullPipeline -v .
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func zcodeE2EReady(t *testing.T) (nodeBin, cliPath, dataDir string) {
	t.Helper()
	cliPath = os.Getenv("HA_ZCODE_CLI")
	dataDir = os.Getenv("HA_ZCODE_DATA_DIR")
	if cliPath == "" || dataDir == "" {
		t.Skip("需要 HA_ZCODE_CLI 与 HA_ZCODE_DATA_DIR（真实 zcode 环境）")
	}
	if _, err := os.Stat(cliPath); err != nil {
		t.Skipf("HA_ZCODE_CLI 不存在: %v", err)
	}
	for _, c := range []string{"node", "nodejs"} {
		if p, err := exec.LookPath(c); err == nil {
			nodeBin = p
			break
		}
	}
	if nodeBin == "" {
		t.Skip("本机无 node")
	}
	return
}

// TestZcodeFullPipeline：登记 → 开线 → 准备目录 → 用**随包 driver**起 zcode
// → 投递 → 断言真改了工程文件 → 收尾。
func TestZcodeFullPipeline(t *testing.T) {
	nodeBin, cliPath, dataDir := zcodeE2EReady(t)

	base := t.TempDir()
	origin := filepath.Join(base, "origin")
	gitQ(t, base, "init", "-q", "-b", "main", origin)
	if err := os.MkdirAll(filepath.Join(origin, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, "src/calc.py"),
		[]byte("def add(a, b):\n    return a + b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitQ(t, origin, "add", "-A")
	gitQ(t, origin, "commit", "-qm", "initial")

	p := newFlowPlugin(t)
	r1, _ := p.handleProjectAdd(map[string]interface{}{
		"name": "zdemo", "remote_url": origin, "default_branch": "main",
	})
	if m := r1.(map[string]interface{}); m["status"] != "ok" {
		t.Fatalf("登记项目失败: %v", m)
	}

	r2, _ := p.handleBranchOpen(map[string]interface{}{
		"project": "zdemo", "branch": "feat/z", "work_desc": "加乘",
	})
	m2 := r2.(map[string]interface{})
	if m2["status"] != "ok" {
		t.Fatalf("开线失败: %v", m2)
	}
	branchID := m2["branch_id"].(string)
	workdir := m2["workdir"].(string)
	if err := p.prepareWorkdir(p.ledger.Projects["zdemo"], "feat/z", workdir); err != nil {
		t.Fatalf("目录准备失败: %v", err)
	}

	// ★ 用**随包分发**的 driver.json 解析描述（不是测试里手拼参数）
	rawRoot := filepath.Join(mustAbs(t, "."), "rawfile")
	pz := newTestPluginWithRawfiles(t, rawRoot)
	pz.path = filepath.Join(base, "commondev") // 用户目录空 → 强制走随包
	drv, cliDir, err := pz.loadDriver("zcode")
	if err != nil {
		t.Fatalf("★ 随包 zcode 驱动解析失败: %v", err)
	}
	// cliPath 指向随包位置（不存在），本判据用 HA_ZCODE_CLI 覆盖成本机真实路径
	script, err := pz.locateSidecarScript(drv, cliDir, "zcode")
	if err != nil {
		t.Fatalf("★ 随包 sidecar 脚本定位失败: %v", err)
	}
	t.Logf("随包解析：script=%s cliPath=%s", script, drv.CLIPath)

	ctx := driverCtx{
		Workdir: workdir, SessionID: "br-" + branchID,
		SessionName: "br-" + branchID, DataDir: dataDir,
		PluginDir: filepath.Dir(rawRoot), RawfileDir: rawRoot,
	}
	argv := drv.buildArgs(ctx)
	argv = append(argv, "--cli", cliPath)

	cmd := exec.Command(nodeBin, append([]string{script}, argv...)...)
	cmd.Dir = workdir
	env := append(os.Environ(), drv.buildEnv(ctx)...)
	env = append(env, "ZCODE_DATA_BASE_DIR="+dataDir)
	cmd.Env = env

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
		t.Fatalf("启动 sidecar 失败: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	w := bufio.NewWriter(stdin)
	rd := bufio.NewReader(stdout)

	if ping := callSidecar(t, w, rd, 1, "ping", nil); ping.Error != nil {
		t.Fatalf("握手失败: %s", ping.Error.Message)
	}
	resp := callSidecar(t, w, rd, 2, "prompt", map[string]interface{}{
		"session_id": "br-" + branchID,
		"text":       "在 src/calc.py 里新增 multiply(a, b) 返回 a*b，保留 add。只回一句完成。",
	})
	if resp.Error != nil {
		t.Fatalf("投递失败: %s", resp.Error.Message)
	}
	var pr map[string]interface{}
	if err := json.Unmarshal(resp.Result, &pr); err != nil {
		t.Fatal(err)
	}
	sid, _ := pr["session_id"].(string)
	if !strings.HasPrefix(sid, "sess_") {
		t.Fatalf("★ 未回报真实 ZCode 会话 id: %q", sid)
	}
	t.Logf("zcode 会话: %s", sid)

	// ★ 关键断言：真的改了**这个工程**的文件，且没破坏原代码
	b2, err := os.ReadFile(filepath.Join(workdir, "src", "calc.py"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b2), "multiply") {
		t.Fatalf("★ zcode 没有在工程里加 multiply:\n%s", b2)
	}
	if !strings.Contains(string(b2), "def add") {
		t.Errorf("★ 原有代码被破坏:\n%s", b2)
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestZcodeOverSSH：**同一套随包 driver/script 走 SSH 远端**。
//
// 用户的要求是"本地 + 通过 ssh 支持的远程 zcodecli"。传输层早就与后端正交
// （Runtime×AgentCLI），但当时**没有任何判据证明二者能组合**：
// `pushSidecar` 只推脚本、`--cli` 也没法分别声明本地/远端路径。
//
// 这条判据用 localhost 做真实 ssh 传输（真 ssh 客户端、BatchMode、scp），
// 把随包的 driver.json + sidecar.mjs 推到远端目录，再在**远端**用真实
// ZCode CLI 跑一轮并要求它改动工程文件。
func TestZcodeOverSSH(t *testing.T) {
	if os.Getenv("HA_SSH_E2E") != "1" {
		t.Skip("设 HA_SSH_E2E=1 以运行（需要 sshd + localhost 免密）")
	}
	nodeBin, cliPath, dataDir := zcodeE2EReady(t)
	_ = nodeBin

	base := t.TempDir()
	origin := filepath.Join(base, "origin")
	gitQ(t, base, "init", "-q", "-b", "main", origin)
	if err := os.MkdirAll(filepath.Join(origin, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, "src/calc.py"),
		[]byte("def add(a, b):\n    return a + b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitQ(t, origin, "add", "-A")
	gitQ(t, origin, "commit", "-qm", "initial")

	p := newFlowPlugin(t)
	if r, _ := p.handleProjectAdd(map[string]interface{}{
		"name": "sshz", "remote_url": origin, "default_branch": "main",
	}); r.(map[string]interface{})["status"] != "ok" {
		t.Fatalf("登记失败: %v", r)
	}
	r2, _ := p.handleBranchOpen(map[string]interface{}{
		"project": "sshz", "branch": "feat/ssh", "work_desc": "远程",
		"runtime": "ssh", "ssh_host": "localhost",
	})
	m2 := r2.(map[string]interface{})
	if m2["status"] != "ok" {
		t.Fatalf("开 SSH 线失败: %v", m2)
	}
	branchID := m2["branch_id"].(string)

	// 远端目录：既是运行时根，也是脚本落地处
	remoteRoot := filepath.Join(base, "remote-runtime")
	remoteWork := filepath.Join(base, "remote-work")
	if err := os.MkdirAll(remoteWork, 0o755); err != nil {
		t.Fatal(err)
	}

	// ★ 走**真实** loadDriver + locateSidecarScript（随包来源）
	rawRoot := filepath.Join(mustAbs(t, "."), "rawfile")
	pz := newTestPluginWithRawfiles(t, rawRoot)
	pz.path = filepath.Join(base, "commondev")
	drv, cliDir, err := pz.loadDriver("zcode")
	if err != nil {
		t.Fatalf("随包 zcode 驱动解析失败: %v", err)
	}
	target := sshTarget{Host: "localhost", Port: 0, NodePath: nodeBin}
	if err := pz.probeSSH(target); err != nil {
		t.Skipf("ssh 不可用: %v", err)
	}
	// pushSidecar 推的是 drv.Script —— 先把随包脚本放进 cwd 让它可以被推
	stage := t.TempDir()
	localScript, err := pz.locateSidecarScript(drv, cliDir, "zcode")
	if err != nil {
		t.Fatalf("随包脚本定位失败: %v", err)
	}
	src, err := os.ReadFile(localScript)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, drv.Script), src, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := pz.pushSidecar(target, drv, stage, remoteRoot); err != nil {
		t.Fatalf("★ 推送随包脚本失败: %v", err)
	}
	// 远端脚本必须真的存在
	if out, err := exec.Command("ssh", "-o", "BatchMode=yes", "localhost",
		"test -f "+remoteRoot+"/"+drv.Script+" && echo OK").Output(); err != nil ||
		!strings.Contains(string(out), "OK") {
		t.Fatalf("★ 远端没有 sidecar 脚本: %v %s", err, out)
	}

	// ★ 在**远端**跑：node <remote script> --workdir <remote work> ... --cli <远端 cli>
	// 本机与 localhost 是同一文件系统，故 cliPath/dataDir 直接复用。
	ctx := driverCtx{
		Workdir: remoteWork, SessionID: "br-" + branchID,
		SessionName: "br-" + branchID, DataDir: dataDir,
		PluginDir: filepath.Dir(rawRoot), RawfileDir: rawRoot,
	}
	argv := drv.buildArgs(ctx)
	argv = append(argv, "--cli", cliPath)
	remoteCmd := "ZCODE_DATA_BASE_DIR=" + dataDir + " " + nodeBin + " " +
		remoteRoot + "/" + drv.Script + " " + shellJoin(argv)
	full := append(target.sshBaseArgs(), "localhost", remoteCmd)

	cmd := exec.Command("ssh", full...)
	cmd.Dir = remoteWork
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
		t.Fatalf("远端启动失败: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	w := bufio.NewWriter(stdin)
	rd := bufio.NewReader(stdout)
	if ping := callSidecar(t, w, rd, 1, "ping", nil); ping.Error != nil {
		t.Fatalf("远端握手失败: %s", ping.Error.Message)
	}
	resp := callSidecar(t, w, rd, 2, "prompt", map[string]interface{}{
		"session_id": "br-" + branchID,
		"text":       "在 src/calc.py 里新增 multiply(a, b) 返回 a*b，保留 add。只回一句完成。",
	})
	if resp.Error != nil {
		t.Fatalf("远端投递失败: %s", resp.Error.Message)
	}
	b2, err := os.ReadFile(filepath.Join(remoteWork, "src", "calc.py"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b2), "multiply") {
		t.Fatalf("★ 远端 zcode 没有改文件:\n%s", b2)
	}
	if !strings.Contains(string(b2), "def add") {
		t.Errorf("★ 远端原有代码被破坏:\n%s", b2)
	}
}

func shellJoin(args []string) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// TestZcodeOverSSHWithPerHostEndpoints：**两台"机器"各配各的 CLI 路径**，
// 走端点表而不是全局 settings —— 这是用户提的需求本体。
//
// 用两个 host 别名指向同一个 sshd（`localhost` 与 `127.0.0.1`），
// 但配置**不同的** CLI 路径与 runtime 目录，从而证明：
//
//	· 每台机器的路径确实分别生效（不是被后配的覆盖）
//	· 发布到远端的 --cli 是该机器自己那条
//
// 为什么用别名而不是真两台机器：判据要能在 CI/本机跑。别名走的是
// **同一条真实 ssh 代码路径**（解析 host → 端点表 → ssh → scp → 远端 node），
// 与真两台机器的差别只在网络延迟，不足以掩盖端点选错这类缺陷。
func TestZcodeOverSSHWithPerHostEndpoints(t *testing.T) {
	if os.Getenv("HA_SSH_E2E") != "1" {
		t.Skip("设 HA_SSH_E2E=1 以运行（需要 sshd + localhost 免密）")
	}
	nodeBin, cliPath, dataDir := zcodeE2EReady(t)

	base := t.TempDir()
	// 两个"机器"各自的 runtime 目录（互不相干）
	rtA := filepath.Join(base, "rtA")
	rtB := filepath.Join(base, "rtB")
	workA := filepath.Join(base, "workA")
	workB := filepath.Join(base, "workB")
	for _, d := range []string{rtA, rtB, workA, workB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 机器 B 上放一个**不同的** CLI 入口（软链到真 CLI，路径不同即为不同）
	cliB := filepath.Join(base, "zcode-on-B.cjs")
	if err := os.Symlink(cliPath, cliB); err != nil {
		t.Fatal(err)
	}

	// ① 端点表：两台机器各自的路径
	p := newTestPluginForShell(t)
	if err := p.loadEndpoints(); err != nil {
		t.Fatal(err)
	}
	// pushSidecar 从 <插件数据>/runtime/<cli>/<script> 取本地脚本 ——
	// 放真实随包脚本进去（推的必须是真脚本，否则远端跑的是空文件）。
	{
		src, err := os.ReadFile(filepath.Join(mustAbs(t, "."), "rawfile", "runtime", "zcode", "sidecar.mjs"))
		if err != nil {
			t.Fatalf("读随包 sidecar 失败: %v", err)
		}
		d := p.driverDir("zcode")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "sidecar.mjs"), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		host, cliPath, rt string
	}{
		{"localhost", cliPath, rtA},
		{"127.0.0.1", cliB, rtB},
	} {
		r, _ := p.handleEndpointSet(map[string]interface{}{
			"host": c.host, "cli": "zcode", "cli_path": c.cliPath,
			"runtime_root": c.rt, "node_path": nodeBin, "verify": false,
		})
		if e := r.(map[string]interface{})["error"]; e != nil {
			t.Fatalf("配 %s 失败: %v", c.host, e)
		}
	}

	// ② 解析必须各归各（这是"多机不互相干扰"的核心断言）
	drv := driverSpec{Script: "sidecar.mjs", Args: []string{"--workdir", "{workdir}"}}
	ctx := driverCtx{Workdir: workA, RawfileDir: "/rf", PluginDir: "/p", DataDir: "/d"}
	ea := p.resolveEndpoint("localhost", "zcode", drv, ctx)
	eb := p.resolveEndpoint("127.0.0.1", "zcode", drv, ctx)
	if ea.CLIPath != cliPath || ea.RuntimeRoot != rtA {
		t.Errorf("★ localhost 端点串了: %+v", ea)
	}
	if eb.CLIPath != cliB || eb.RuntimeRoot != rtB {
		t.Errorf("★ 127.0.0.1 端点串了: %+v", eb)
	}
	if ea.CLIPath == eb.CLIPath {
		t.Error("★ 两台机器的 CLI 路径不该相同（配置未分别生效）")
	}

	// ③ 真跑：两台机器各自用**自己的** CLI 路径完成任务
	for _, tc := range []struct {
		host, cliPath, rt, wd, proj string
	}{
		{"localhost", cliPath, rtA, workA, "hA"},
		{"127.0.0.1", cliB, rtB, workB, "hB"},
	} {
		origin := filepath.Join(base, "origin-"+tc.proj)
		gitQ(t, base, "init", "-q", "-b", "main", origin)
		if err := os.MkdirAll(filepath.Join(origin, "src"), 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(origin, "src/calc.py"),
			[]byte("def add(a, b):\n    return a + b\n"), 0o644)
		gitQ(t, origin, "add", "-A")
		gitQ(t, origin, "commit", "-qm", "initial")

		p.handleProjectAdd(map[string]interface{}{
			"name": tc.proj, "remote_url": origin, "default_branch": "main",
		})

		b := &Branch{ID: "br-" + tc.proj, Name: "feat/x", ProjectID: tc.proj,
			Runtime: "ssh", SSHHost: tc.host, Workdir: tc.wd}
		tgt, err := parseSSHTarget(b)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.probeSSH(tgt); err != nil {
			t.Skipf("%s 不可用: %v", tc.host, err)
		}
		// 准备该机器的工作目录（本判据聚焦端点解析与远端执行，
		// 目录准备本身已由 workdir_remote_test.go 覆盖）：
		// 直接 clone origin 到该机器的 workdir，并切到目标分支。
		if err := os.RemoveAll(tc.wd); err != nil {
			t.Fatal(err)
		}
		gitQ(t, base, "clone", "-q", origin, tc.wd)
		gitQ(t, tc.wd, "checkout", "-qb", "feat/x")

		// 推脚本 + 拼远端命令（走真实代码路径）
		if err := p.pushSidecar(tgt, drv, p.driverDir("zcode"), tc.rt); err != nil {
			t.Fatalf("%s 推脚本失败: %v", tc.host, err)
		}
		// ★ ctx.Workdir 必须**按该机器**重置：它是 {workdir} 占位符的来源，
		// 共用一个会让后一台机器把文件改到前一台的目录里
		// （本判据第一版就踩了：两台都跑在 workA）。
		ctx.Workdir = tc.wd
		ep := p.resolveEndpoint(tc.host, "zcode", drv, ctx)
		_, args, env := buildRemoteCommandFor(drv, ctx, ep.NodePath, ep)
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, tc.cliPath) {
			t.Errorf("★ %s 的远端命令未使用本机专属 CLI 路径: %v", tc.host, args)
		}
		if tc.host == "127.0.0.1" && strings.Contains(joined, rtA) {
			t.Errorf("★ 127.0.0.1 的命令里混入了 localhost 的路径: %v", args)
		}

		// ★★ 真跑：在**该机器**上执行解析出来的命令，并断言它真的改了文件。
		//
		// 只比字符串的话，"端点选对了但路径在目标机器上不存在"这类缺陷
		// 照样全绿 —— 而那正是本需求要防的东西（每台机器路径不同）。
		remoteCmd := "ZCODE_DATA_BASE_DIR=" + dataDir + " " + ep.NodePath + " " +
			tc.rt + "/" + drv.Script + " " + shellJoin(args)
		sshArgv := append(tgt.sshBaseArgs(), tgt.Host, remoteCmd)
		rc := exec.Command("ssh", sshArgv...)
		rc.Dir = tc.wd
		rstdin, err := rc.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		rstdout, err := rc.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		rc.Stderr = os.Stderr
		if err := rc.Start(); err != nil {
			t.Fatalf("%s 远端启动失败: %v", tc.host, err)
		}
		rw := bufio.NewWriter(rstdin)
		rrd := bufio.NewReader(rstdout)
		if ping := callSidecar(t, rw, rrd, 1, "ping", nil); ping.Error != nil {
			_ = rc.Process.Kill()
			t.Fatalf("%s 远端握手失败: %s", tc.host, ping.Error.Message)
		}
		resp := callSidecar(t, rw, rrd, 2, "prompt", map[string]interface{}{
			"session_id": "br-" + tc.proj,
			"text":       "在 src/calc.py 里新增 multiply(a, b) 返回 a*b，保留 add。只回一句完成。",
		})
		_ = rc.Process.Kill()
		_, _ = rc.Process.Wait()
		if resp.Error != nil {
			t.Fatalf("★ %s 投递失败（端点表里的 CLI 路径可能在该机器上不存在）: %s",
				tc.host, resp.Error.Message)
		}
		b2, err := os.ReadFile(filepath.Join(tc.wd, "src/calc.py"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b2), "multiply") {
			t.Errorf("★ %s 上 zcode 没有改文件:\n%s", tc.host, b2)
		}
		if !strings.Contains(string(b2), "def add") {
			t.Errorf("★ %s 上原有代码被破坏:\n%s", tc.host, b2)
		}
		_ = env
	}
}
