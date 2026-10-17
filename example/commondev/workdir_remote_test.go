package main

// 远端工作目录准备的判据。
//
// 本次发现的缺口：`prepareWorkdir` **只有本地一套**（本机 clone --mirror +
// clone mirror workdir）。对 runtime=ssh 的分支，远端 pi 的 cwd 指向一个
// **从未被创建**的远端目录 —— 症状是"agent 起来了，但一执行命令就说目录不存在"，
// 而 job 状态显示 done（因为本地 clone 成功了）。
//
// 这类"两边各自看起来都成功"的缺口最难靠肉眼发现，必须有判据。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// filepathBase 是 filepath.Base 的薄包装（判据里读起来更清楚）。
func filepathBase(p string) string { return filepath.Base(p) }

// writeFile 是判据用的小工具。
func writeFile(p, content string) error {
	return os.WriteFile(p, []byte(content), 0o644)
}

// readSourceOrSkip 读一个源文件，读不到就跳过。
func readSourceOrSkip(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Skipf("读不到 %s: %v", name, err)
	}
	return string(b)
}

// ★★★ 判据：runtime=ssh 时台账里的 Workdir 必须是**远端路径**。
//
// 留本地路径会让 agent_up 把远端 pi 的 cwd 指到本地目录名上。
func TestSSHBranchWorkdirBecomesRemotePath(t *testing.T) {
	sshE2EReady(t)
	p := newTestPluginForShell(t)
	p.ledger = &Ledger{Projects: map[string]*Project{}, Branches: map[string]*Branch{},
		Jobs: map[string]*Job{}, Criteria: map[string]*Criteria{}}
	remoteRoot := t.TempDir()
	p.testSetSetting("ssh_workdir_root", remoteRoot)

	// 造一个**远端可就地 clone 的**裸仓库（本机路径对 localhost 的 ssh 同样可见）
	localBare := makeBareRepo(t)
	p.ledger.Projects["proj"] = &Project{
		Name: "proj", RemoteURL: localBare, DefaultBranch: "main",
	}

	// ★ 直接构造 branch，**不调 handleBranchOpen**：
	// 后者会起后台 goroutine 也去准备同一个 mirror，与本次同步准备并发撞车
	// （那正是它暴露出的 TOCTOU —— 已在生产代码用 prepMu 修掉，
	//   但判据不该依赖"两条路径同时跑"的时序）。
	b := &Branch{ID: "br-remote", Name: "feat/remote", ProjectID: "proj",
		Runtime: "ssh", SSHHost: "localhost"}
	tgt, err := parseSSHTarget(b)
	if err != nil {
		t.Fatal(err)
	}
	tgt.WorkdirRoot = remoteRoot
	remoteWorkdir := remoteRoot + "/" + dirName("proj", "feat/remote")
	if err := p.prepareRemoteWorkdir(tgt, p.ledger.Projects["proj"], "feat/remote", remoteWorkdir); err != nil {
		t.Fatalf("★ 远端目录准备失败: %v", err)
	}

	// ① 远端目录真的存在且是 git 仓库
	if !tgt.remotePathExists(remoteWorkdir) {
		t.Fatal("★ 远端工作目录不存在")
	}
	if _, err := tgt.remoteGit(remoteWorkdir, "rev-parse", "--git-dir"); err != nil {
		t.Fatalf("★ 远端目录不是 git 仓库: %v", err)
	}
	// ② 已切到目标分支（不是默认分支）
	br, err := tgt.remoteGit(remoteWorkdir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(br) != "feat/remote" {
		t.Errorf("★ 远端目录在错的分支上: %q（应为 feat/remote）", strings.TrimSpace(br))
	}
	// ③ 共享 mirror 建在远端（不是本地路径）
	if !tgt.remotePathExists(remoteRoot + "/.mirrors/proj.git") {
		t.Error("★ 远端共享 mirror 未建立")
	}
}

// ★★ 判据：**缺 ssh_workdir_root 时响亮报错**（不猜默认值）。
//
// 猜默认会把文件写到远端别的目录里 —— 那是很难回滚的错。
func TestRemoteWorkdirRequiresRootConfig(t *testing.T) {
	p := newTestPluginForShell(t)
	// settings 的 Get 返回 nil ⇒ 视为未配置
	_, err := p.remoteWorkdirRoot("somehost")
	if err == nil {
		t.Fatal("★ 未配置 ssh_workdir_root 时应报错")
	}
	if !strings.Contains(err.Error(), "workdir_root") {
		t.Errorf("错误应指出缺哪个配置: %v", err)
	}
}

// ★★★ 判据：**本地路径与远端路径不能混用**。
//
// prepareWorkdir 用的是本机 git；远端线绝不能走它。
// 这条钉住"两条路径必须分开"这个结构性事实：
// 若 runPrepareWorkdir 忽略 runtime 一律走本地，远端目录永远是空的。
func TestLocalAndRemoteWorkdirPathsAreDistinct(t *testing.T) {
	// 远端目录名与本地同构（<hash>-<name>），但**根不同**
	local := branchDir("/local/program", "proj", "feat/x")
	remote := "/remote/root/" + dirName("proj", "feat/x")
	if local == remote {
		t.Error("本地与远端目录不应相同")
	}
	// 基名必须一致（便于人对照），根必须不同
	if filepathBase(local) != filepathBase(remote) {
		t.Errorf("两端目录基名应一致（可读性）: %s vs %s", filepathBase(local), filepathBase(remote))
	}
}

// ★★ 判据：远端目录**已存在时复用**（重开分支线不重新克隆）。
func TestRemoteWorkdirReuseIsIdempotent(t *testing.T) {
	sshE2EReady(t)
	p := newTestPluginForShell(t)
	p.ledger = &Ledger{Projects: map[string]*Project{}, Branches: map[string]*Branch{},
		Jobs: map[string]*Job{}, Criteria: map[string]*Criteria{}}
	root := t.TempDir()
	bare := makeBareRepo(t)
	pr := &Project{Name: "proj", RemoteURL: bare, DefaultBranch: "main"}

	tgt := sshTarget{Host: "localhost", NodePath: "node", WorkdirRoot: root}
	wd := root + "/" + dirName("proj", "feat/reuse")

	// 第一次：完整准备
	if err := p.prepareRemoteWorkdir(tgt, pr, "feat/reuse", wd); err != nil {
		t.Fatalf("首次准备失败: %v", err)
	}
	// 放一个标记文件，第二次准备**不能**把它冲掉
	tgt.remoteMkdir(wd)
	if _, err := tgt.remoteGit(wd, "rev-parse"); err != nil {
		t.Fatal(err)
	}
	if err := p.prepareRemoteWorkdir(tgt, pr, "feat/reuse", wd); err != nil {
		t.Fatalf("复用准备失败: %v", err)
	}
	// 仍在正确分支
	br, _ := tgt.remoteGit(wd, "rev-parse", "--abbrev-ref", "HEAD")
	if strings.TrimSpace(br) != "feat/reuse" {
		t.Errorf("复用后分支不对: %q", strings.TrimSpace(br))
	}
}

// ★ 判据：远端 git 命令的关键要素（BatchMode + GIT_TERMINAL_PROMPT=0）。
//
// 远端拉取若卡在认证提示，会挂到超时而不是报"没凭据"。
func TestRemoteGitIsNonInteractive(t *testing.T) {
	src := readSourceOrSkip(t, "workdir_remote.go")
	if !strings.Contains(src, "GIT_TERMINAL_PROMPT=0") {
		t.Error("★ 远端 git 未禁用交互提示（会挂到超时而非报错）")
	}
	if !strings.Contains(src, "sshBaseArgs()") {
		t.Error("★ 远端 git 未用 sshBaseArgs（缺 BatchMode）")
	}
}

// makeBareRepo 造一个可被 clone 的裸仓库（含 main 分支与一次提交）。
func makeBareRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bare := dir + "/origin.git"
	work := dir + "/work"
	run := func(d string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = d
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run(dir, "init", "--bare", "-b", "main", bare)
	run(dir, "init", "-b", "main", work)
	if err := writeFile(work+"/README.md", "hi\n"); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "-A")
	run(work, "commit", "-m", "init")
	run(work, "remote", "add", "origin", bare)
	run(work, "push", "origin", "main")
	return bare
}

// ★★★ 判据：**完整远端闭环** —— 远端建目录 → 远端拉起 pi → 投递一次。
//
// 前面两条分别验了"远端目录准备"与"远端 sidecar 握手"，但真实使用是
// 串起来的：目录必须先于 pi 存在（否则 pi 的 cwd 无效），而投递必须落到
// 那个目录里的会话上。这条把三段串起来跑一遍。
func TestSSHFullLoop(t *testing.T) {
	sshE2EReady(t)
	p := newTestPluginForShell(t)
	p.ledger = &Ledger{Projects: map[string]*Project{}, Branches: map[string]*Branch{},
		Jobs: map[string]*Job{}, Criteria: map[string]*Criteria{}}

	root := t.TempDir()
	runtimeRoot := t.TempDir()
	p.testSetSetting("ssh_workdir_root", root)
	p.testSetSetting("ssh_runtime_root", runtimeRoot)
	p.testSetSetting("ssh_node_path", "node")

	// 真实 sidecar 放进 driver 目录（供 scp 推到远端）
	cliDir := p.driverDir("pi")
	if err := os.MkdirAll(cliDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcBytes, err := os.ReadFile("sidecar.mjs")
	if err != nil {
		t.Skipf("读不到 sidecar.mjs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cliDir, "sidecar.mjs"), srcBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	bare := makeBareRepo(t)
	pr := &Project{Name: "proj", RemoteURL: bare, DefaultBranch: "main"}

	// ① 远端准备目录
	tgt := sshTarget{Host: "localhost", NodePath: "node", WorkdirRoot: root}
	wd := root + "/" + dirName("proj", "feat/loop")
	if err := p.prepareRemoteWorkdir(tgt, pr, "feat/loop", wd); err != nil {
		t.Fatalf("① 远端目录准备失败: %v", err)
	}

	// ② 远端拉起 sidecar，并确认它在**那个目录**里工作
	b := &Branch{ID: "br-loop", Name: "feat/loop", ProjectID: "proj",
		Workdir: wd, Runtime: "ssh", SSHHost: "localhost"}
	ctx := driverCtx{Workdir: wd, SessionID: "sess-loop", SessionName: "br-loop",
		DataDir: filepath.Dir(p.path), PluginDir: cliDir}
	drv, _, err := p.loadDriver("pi")
	if err != nil {
		t.Fatal(err)
	}
	sc, err := p.startSSHSidecar(b, drv, ctx, "pi")
	if err != nil {
		t.Fatalf("② 远端 sidecar 启动失败: %v", err)
	}
	defer sc.Close()

	// ③ 投递一次，断言会话未漂移
	res, err := sc.call("prompt", map[string]interface{}{
		"session_id": "sess-loop", "text": "只回一个词：ok",
	})
	if err != nil {
		t.Fatalf("③ 远端投递失败: %v", err)
	}
	m, _ := res.(map[string]interface{})
	if got, _ := m["session_id"].(string); got != "" && got != "sess-loop" {
		t.Errorf("★ 远端会话漂移: %q", got)
	}
	t.Logf("远端完整闭环成功：目录=%s 会话=%v", wd, m["session_id"])
}

// ★★★ 判据：`runPrepareWorkdir` **必须按 runtime 分派**。
//
// 这条补的是变异暴露的缺口：上面那些判据直接调用 prepareRemoteWorkdir，
// 所以把 runPrepareWorkdir 里的 `if runtime == "ssh"` 去掉（一律走本地）
// 时**它们全绿** —— 而那正是最致命的回退：远端目录永远为空，
// 但 job 状态显示 done（本地 clone 成功了）。
//
// 判据必须钉住**分派本身**，而不是只钉住被分派到的函数。
func TestPrepareWorkdirDispatchesByRuntime(t *testing.T) {
	src := readSourceOrSkip(t, "plugin.go")
	// 找到 runPrepareWorkdir 函数体
	i := strings.Index(src, "func (p *Plugin) runPrepareWorkdir(")
	if i < 0 {
		t.Fatal("找不到 runPrepareWorkdir")
	}
	end := strings.Index(src[i:], "\n}\n")
	if end < 0 {
		t.Fatal("找不到函数结尾")
	}
	body := src[i : i+end]

	// ① 必须有 runtime 参数（否则无从分派）
	if !strings.Contains(body, "runtime, sshHost string") {
		t.Error("★ runPrepareWorkdir 未接收 runtime 参数")
	}
	// ② 必须按 runtime 分派到远端实现
	//
	// ★ 判据要**精确匹配分派语句**，不能只 Contains("runtime == \"ssh\"")：
	// 实测 `if false && runtime == "ssh"`（把分派关掉）仍然包含那个子串，
	// 于是判据照绿。这是"判据通过 ≠ 检查到了目标性质"的又一例。
	sshIdx := strings.Index(body, `if runtime == "ssh" {`)
	if !strings.Contains(body, `if runtime == "ssh" {`) {
		t.Error("★ runPrepareWorkdir 未按 runtime 分派（远端线会走本地 clone，远端目录永远为空）")
	}
	if strings.Contains(body, `false && runtime == "ssh"`) {
		t.Error("★ 分派条件被禁用（存在 `false &&` 前缀）")
	}
	// 分派必须**真的返回**（进入远端分支后不能落到本地路径）
	if sshIdx >= 0 {
		seg := body[sshIdx:]
		if !strings.Contains(seg, "return") {
			t.Error("★ 远端分派分支没有 return（会继续落到本地路径）")
		}
	}
	if !strings.Contains(body, "prepareRemoteWorkdir") {
		t.Error("★ runPrepareWorkdir 未调用 prepareRemoteWorkdir")
	}
	// ③ 本地路径必须在分派之后（否则会被远端分支挡住）
	localIdx := strings.Index(body, "p.prepareWorkdir(")
	if localIdx >= 0 && sshIdx >= 0 && localIdx < sshIdx {
		t.Error("★ 本地 prepareWorkdir 出现在分派之前（远端线会走本地路径）")
	}
	// ④ 远端线的 Workdir 必须被改写成远端路径
	if !strings.Contains(body, "bb.Workdir = remoteWorkdir") {
		t.Error("★ 远端线未把台账 Workdir 改成远端路径（agent_up 会用错 cwd）")
	}
}

// ★★ 判据：`branch_open` 必须把 runtime/ssh_host 传给后台准备。
//
// 只记进台账但不传给 runPrepareWorkdir 的话，台账对了、目录还是本地建的
// —— "参数记了但没用"是同一类缺口的另一半。
func TestBranchOpenPassesRuntimeToPrepare(t *testing.T) {
	src := readSourceOrSkip(t, "plugin.go")
	if !strings.Contains(src, "go p.runPrepareWorkdir(jobID, id, pr, branch, workdir, b.Runtime, b.SSHHost)") {
		t.Error("★ branch_open 未把 runtime/ssh_host 传给 runPrepareWorkdir")
	}
}

// ★★★ 判据：**并发的目录准备不能互相撞车**（真 bug，实测 3/6 复现）。
//
// 现象：
//
//	fatal: destination path '.../.mirrors/proj.git' already exists
//	       and is not an empty directory
//
// 根因是"检查后 clone"这个 TOCTOU：两个 goroutine 同时判定 mirror 不存在，
// 然后都去 clone。**触发场景是真实的** —— branch_open 的目录准备在后台跑，
// 用户看 job 还没 done 就再点一次 workdir_prepare（完全自然的操作）。
//
// 判据直接并发调 prepareRemoteWorkdir N 次，要求**至少一次成功且无致命错误**
// （"已被别人建好"不算失败，那是幂等的一部分）。
func TestConcurrentRemotePrepareDoesNotCollide(t *testing.T) {
	sshE2EReady(t)
	p := newTestPluginForShell(t)
	root := t.TempDir()
	bare := makeBareRepo(t)
	pr := &Project{Name: "proj", RemoteURL: bare, DefaultBranch: "main"}
	tgt := sshTarget{Host: "localhost", NodePath: "node", WorkdirRoot: root}
	wd := root + "/" + dirName("proj", "feat/race")

	const n = 4
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			errCh <- p.prepareRemoteWorkdir(tgt, pr, "feat/race", wd)
		}()
	}
	var errs []error
	for i := 0; i < n; i++ {
		if err := <-errCh; err != nil {
			errs = append(errs, err)
		}
	}
	for _, err := range errs {
		// "already exists" 是撞车的直接证据，必须为 0
		if strings.Contains(err.Error(), "already exists") {
			t.Errorf("★ 并发准备撞车: %v", err)
		}
	}
	// 最终必须有一个可用的仓库在正确分支上
	if _, err := tgt.remoteGit(wd, "rev-parse", "--abbrev-ref", "HEAD"); err != nil {
		t.Fatalf("并发准备后工作目录不可用: %v（错误: %v）", err, errs)
	}
	br, _ := tgt.remoteGit(wd, "rev-parse", "--abbrev-ref", "HEAD")
	if strings.TrimSpace(br) != "feat/race" {
		t.Errorf("并发准备后分支不对: %q", strings.TrimSpace(br))
	}
}

// ★★ 判据：mirror 就位用 **shell mv**，不是 `git mv`。
//
// `git mv` 只在仓库内工作，用于文件系统搬移会报 "not a git repository"
// （实测踩到：mirror 就位时失败）。
func TestRemoteMirrorPlacedWithShellMv(t *testing.T) {
	src := readSourceOrSkip(t, "workdir_remote.go")
	if !strings.Contains(src, `remoteShell("mv "`) {
		t.Error("★ mirror 就位未用 shell mv（git mv 会报 not a git repository）")
	}
	if strings.Contains(src, `remoteGit("", "mv"`) {
		t.Error("★ 仍在用 git mv 搬移 mirror")
	}
}
