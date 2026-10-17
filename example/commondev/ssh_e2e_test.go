package main

// SSH 传输的**真端到端**判据。
//
// 前面的 ssh_wiring_test.go 只验证"参数被正确接线"（台账里记的是 ssh）。
// 这里验证**链路真的能跑**：真起 ssh 进程、真把脚本推到远端、真握手、
// 真投递一次 prompt。
//
// 用 localhost（本机已配免密）作为"远端"——它同样是完整的 ssh 路径：
// 走 ssh 客户端、BatchMode、scp、远端 node 执行。与真远端只差网络延迟。
//
// 默认跳过（需要 sshd + 免密），设 HA_SSH_E2E=1 运行。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JianFeeeee/homeagentsdk/sdk"
)

// mapSettings 是能**真正存取值**的 settings 替身。
//
// stubSettings 的 Get 恒返回 nil，无法表达"用户配置了 ssh_runtime_root"。
// 这里用 map 实现 Get/Set 的往返，使判据能覆盖配置驱动的那条路径。
type mapSettings struct {
	stubSettings
	vals map[string]interface{}
}

func newMapSettings(dir string) *mapSettings {
	return &mapSettings{stubSettings: stubSettings{dir: dir}, vals: map[string]interface{}{}}
}

func (m *mapSettings) Get(key string) (interface{}, error) {
	if v, ok := m.vals[key]; ok {
		return v, nil
	}
	return nil, nil
}

func (m *mapSettings) Set(key string, v interface{}) error {
	m.vals[key] = v
	return nil
}

// testSetSetting 把插件 settings 换成可存取版本并写一个键。
func (p *Plugin) testSetSetting(key string, v interface{}) {
	var ms *mapSettings
	if cur, ok := p.sdk.Settings().(*mapSettings); ok {
		ms = cur
	} else {
		ms = newMapSettings(p.sdk.Settings().DataDir())
	}
	_ = ms.Set(key, v)
	if _, ok := p.sdk.Settings().(*mapSettings); !ok {
		// 重新装配 SDK：Settings 是不可变注入的，必须整体替换
		p.sdk = sdk.New("commondev", ms, nil, nil, nil, nil)
	}
}

// sshE2EReady 检查真 e2e 的前置条件。
func sshE2EReady(t *testing.T) {
	t.Helper()
	if os.Getenv("HA_SSH_E2E") != "1" {
		t.Skip("设 HA_SSH_E2E=1 以运行（需要 sshd + localhost 免密）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5",
		"localhost", "node --version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("localhost 免密或 node 不可用: %v: %s", err, out)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(out)), "v") {
		t.Skipf("远端 node 输出异常: %q", out)
	}
}

// setupSSHPlugin 造一个能跑 ssh 路径的 Plugin，并把真实 sidecar 放进 driver 目录。
func setupSSHPlugin(t *testing.T, remoteRoot string) *Plugin {
	t.Helper()
	p := newTestPluginForShell(t)
	// 把真实的 sidecar.mjs 放进 runtime/pi/
	cliDir := p.driverDir("pi")
	if err := os.MkdirAll(cliDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("sidecar.mjs")
	if err != nil {
		t.Skipf("读不到 sidecar.mjs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cliDir, "sidecar.mjs"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	p.ledger = &Ledger{
		Projects: map[string]*Project{}, Branches: map[string]*Branch{},
		Jobs: map[string]*Job{}, Criteria: map[string]*Criteria{},
	}
	return p
}

// ★★★ 判据：SSH 链路真的能握手（probe + push + ping）。
//
// 这条覆盖的是"ssh 路径能不能跑通"，而不是"参数有没有记对"。
func TestSSHEndToEndHandshake(t *testing.T) {
	sshE2EReady(t)
	remoteRoot := t.TempDir()
	p := setupSSHPlugin(t, remoteRoot)
	// 配置远端运行时目录与 node 路径
	p.testSetSetting("ssh_runtime_root", remoteRoot)
	p.testSetSetting("ssh_node_path", "node")

	b := &Branch{
		ID: "br-e2e-ssh", Name: "feat/e2e", ProjectID: "proj",
		Workdir: t.TempDir(), Runtime: "ssh", SSHHost: "localhost",
	}
	ctx := driverCtx{
		Workdir: b.Workdir, SessionID: "sess-e2e", SessionName: "br-e2e",
		DataDir: filepath.Dir(p.path), PluginDir: p.driverDir("pi"),
	}
	drv, _, err := p.loadDriver("pi")
	if err != nil {
		t.Fatal(err)
	}

	sc, err := p.startSSHSidecar(b, drv, ctx, "pi")
	if err != nil {
		t.Fatalf("★ SSH sidecar 启动/握手失败: %v", err)
	}
	defer sc.Close()

	// 握手成功即证明了：probeSSH（免密）→ pushSidecar（scp）→ ssh 起进程 → ping
	if !sc.Alive() {
		t.Fatal("★ 握手后进程不存活")
	}

	// 真投一次 prompt（覆盖"投到指定 session"在远端同样成立）
	res, err := sc.call("prompt", map[string]interface{}{
		"session_id": "sess-e2e", "text": "只回一个词：ok",
	})
	if err != nil {
		t.Fatalf("★ 远端投递失败: %v", err)
	}
	m, _ := res.(map[string]interface{})
	// 断言 session 未漂移（deliverToPi 的核心不变量）
	if got, _ := m["session_id"].(string); got != "" && got != "sess-e2e" {
		t.Errorf("★ 远端会话漂移: %q", got)
	}
	t.Logf("SSH e2e 握手与投递成功: %v", m)
}

// ★★ 判据：**远端脚本被真的推过去了**（否则跑的是上一次的旧版）。
func TestSSHPushesScriptToRemote(t *testing.T) {
	sshE2EReady(t)
	remoteRoot := t.TempDir()
	defer os.RemoveAll(remoteRoot)
	p := setupSSHPlugin(t, remoteRoot)
	p.testSetSetting("ssh_runtime_root", remoteRoot)
	p.testSetSetting("ssh_node_path", "node")

	b := &Branch{ID: "br-push", Name: "f", ProjectID: "proj",
		Workdir: t.TempDir(), Runtime: "ssh", SSHHost: "localhost"}
	ctx := driverCtx{Workdir: b.Workdir, SessionID: "s", SessionName: "n",
		DataDir: filepath.Dir(p.path), PluginDir: p.driverDir("pi")}
	drv, dir, err := p.loadDriver("pi")
	if err != nil {
		t.Fatal(err)
	}

	tgt, err := parseSSHTarget(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.probeSSH(tgt); err != nil {
		t.Skipf("probe 失败: %v", err)
	}
	if err := p.pushSidecar(tgt, drv, dir, remoteRoot); err != nil {
		t.Fatalf("★ 推送脚本失败: %v", err)
	}
	// 本地与远端内容必须一致（否则"改了没生效"）
	local, _ := os.ReadFile(filepath.Join(dir, drv.Script))
	remote, err := os.ReadFile(filepath.Join(remoteRoot, drv.Script))
	if err != nil {
		t.Fatalf("★ 远端没有脚本: %v", err)
	}
	if string(local) != string(remote) {
		t.Errorf("★ 远端脚本与本地不一致（本地 %d 字节，远端 %d 字节）",
			len(local), len(remote))
	}
	_ = ctx
}

// ★★ 判据：远端缺 node → **响亮报错**（不是 10s 后含糊的"握手失败"）。
func TestSSHProbeFailsLoudlyWhenNodeMissing(t *testing.T) {
	sshE2EReady(t)
	p := setupSSHPlugin(t, t.TempDir())
	b := &Branch{ID: "br-nonode", Name: "f", Runtime: "ssh", SSHHost: "localhost"}
	tgt, err := parseSSHTarget(b)
	if err != nil {
		t.Fatal(err)
	}
	// 用一个必然不存在的命令名做 node → probe 必须报"输出异常/不可达"
	tgt.NodePath = "/nonexistent/node-binary-xyz"
	err = p.probeSSH(tgt)
	if err == nil {
		t.Fatal("★ 远端缺 node 时应报错")
	}
	if !strings.Contains(err.Error(), "远端") {
		t.Errorf("错误应指明是远端问题: %v", err)
	}
}

// ★★ 判据：**没有免密时响亮失败**，且提示"不交互输密码"。
func TestSSHWithoutPasswordlessFailsLoudly(t *testing.T) {
	sshE2EReady(t)
	p := setupSSHPlugin(t, t.TempDir())
	b := &Branch{ID: "br-nopw", Name: "f", Runtime: "ssh",
		SSHHost: "nonexistent-host-xyz.invalid"}
	tgt, err := parseSSHTarget(b)
	if err != nil {
		t.Fatal(err)
	}
	err = p.probeSSH(tgt)
	if err == nil {
		t.Fatal("★ 不可达主机应报错")
	}
	if !strings.Contains(err.Error(), "免密") {
		t.Errorf("错误应提示配免密: %v", err)
	}
}

// ★ 判据：BatchMode 必须开启（否则会卡在交互式密码提示上）。
func TestSSHUsesBatchMode(t *testing.T) {
	b := &Branch{ID: "b", Name: "f", Runtime: "ssh", SSHHost: "u@h"}
	tgt, err := parseSSHTarget(b)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(tgt.sshBaseArgs(), " ")
	if !strings.Contains(args, "BatchMode=yes") {
		t.Errorf("★ 缺少 BatchMode=yes（会卡在密码提示）: %v", args)
	}
	if !strings.Contains(args, "ConnectTimeout") {
		t.Errorf("★ 缺少 ConnectTimeout: %v", args)
	}
}
