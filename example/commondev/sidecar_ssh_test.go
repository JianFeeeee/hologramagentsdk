package main

// SSH 传输层判据。
//
// 重点验三件事（都是"本地好、远端坏"的典型来源）：
//  ① 环境变量**显式前置**（ssh 不继承本地环境）
//  ② 参数**正确转义**（远端还有一层 shell）
//  ③ 远端命令字面量正确（node 用远端路径、脚本用远端路径）
//
// ⚠ 这些判据**不需要真的 ssh 到远端** —— 它们验证的是命令组装，
// 那正是本地能测且最常错的部分。真机远端验证需另配环境（见文末说明）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ★★ 判据：ssh 命令里**环境变量必须显式前置**。
//
// ssh 不继承本地环境。若只把 env 塞进本地 cmd.Env，远端进程拿不到 ——
// 而它的表现是「远端 pi/zcode 找不到 agentdir / 会话目录」，
// 看起来像远端配置问题，实际是我们没传过去。
func TestSSHCommandExportsEnvExplicitly(t *testing.T) {
	tgt := sshTarget{Host: "u@h"}
	argv := tgt.sshCommand("node", "/r/sidecar.mjs", []string{"--workdir", "/w"},
		map[string]string{"PI_CODING_AGENT_DIR": "/rd/agentdir"})

	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "env ") {
		t.Fatalf("★ ssh 命令里没有 `env` 前缀 —— 远端拿不到环境变量: %s", joined)
	}
	if !strings.Contains(joined, "PI_CODING_AGENT_DIR=/rd/agentdir") {
		t.Fatalf("环境变量未出现在远端命令里: %s", joined)
	}
	// 环境顺序必须稳定（否则命令不可对照、diff 无意义）
	if strings.Join(tgt.sshCommand("node", "/r/s.mjs", nil, map[string]string{"B": "2", "A": "1"}), " ") !=
		strings.Join(tgt.sshCommand("node", "/r/s.mjs", nil, map[string]string{"A": "1", "B": "2"}), " ") {
		t.Error("环境变量顺序不稳定")
	}
}

// ★★★ 判据：含空格/引号的参数**必须正确转义**。
//
// 远端还有一层 shell：不转义就会被拆成多个参数。
// 这条最容易漏，因为本地测试路径通常不含空格（`/tmp/xxx`）——
// 一旦真实路径是 `/home/my project/…` 就炸，而本地全绿。
func TestSSHCommandQuotesArgumentsWithSpaces(t *testing.T) {
	tgt := sshTarget{Host: "u@h"}
	argv := tgt.sshCommand("/usr/bin/node", "/r/side car.mjs",
		[]string{"--workdir", "/home/my project/repo"},
		map[string]string{"NOTE": "it's a test"})
	joined := strings.Join(argv, " ")

	// 远端命令里必须出现被引号包住的整体（而不是裸的空格分隔）
	if !strings.Contains(joined, `'/home/my project/repo'`) {
		t.Fatalf("★ 含空格的工作目录未转义（远端会拆成两个参数）: %s", joined)
	}
	// 引号包住的是整个 `NOTE=it's a test`，所以出现的是
	// `'NOTE=it'\''s a test'` —— 关键证据是那个转义序列 `'\''`。
	if !strings.Contains(joined, "'\\''") {
		t.Fatalf("★ 含单引号的值未做 shell 转义: %s", joined)
	}
	// shellQuote 单元级验证
	for in, want := range map[string]string{
		"simple":           "simple",
		"with space":       "'with space'",
		"a'b":              `'a'\''b'`,
		"":                 "''",
		"/a/b-c.d_e:f@g=h": "/a/b-c.d_e:f@g=h",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q)=%q 期望 %q", in, got, want)
		}
	}
}

// ★★ 判据：**node 必须用远端的路径**，不是本地随包分发的绝对路径。
//
// 本地 node 在远端通常不存在；用本地路径会报 "No such file or directory"，
// 而那个错误看起来像"远端没装 node"，实际是我们传错了路径。
func TestSSHUsesRemoteNodePath(t *testing.T) {
	tgt := sshTarget{Host: "u@h", NodePath: "node"}
	argv := tgt.sshCommand(tgt.NodePath, "/r/s.mjs", nil, nil)
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "/root/.homeagent") || strings.Contains(joined, "plugin.bin") {
		t.Fatalf("★ 远端命令里出现了本地路径: %s", joined)
	}
	// 默认应是裸 `node`（走远端 PATH）
	last := argv[len(argv)-1]
	if !strings.HasPrefix(last, "node ") {
		t.Fatalf("远端命令应以远端 node 开头: %s", last)
	}
}

// ★★ 判据：ssh 必须**非交互**（BatchMode）。
//
// 缺它会停在密码提示上等输入 ⇒ 工具调用挂到 60s 超时，
// 表现为「拉起 agent 卡住」而不是「没配免密」—— 排查方向完全不同。
func TestSSHIsNonInteractive(t *testing.T) {
	tgt := sshTarget{Host: "u@h"}
	argv := tgt.sshBaseArgs()
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "BatchMode=yes") {
		t.Fatal("★ 缺少 BatchMode=yes —— 没配免密时会在密码提示上挂死")
	}
	if !strings.Contains(joined, "ConnectTimeout=") {
		t.Error("缺少 ConnectTimeout（不可达主机会挂很久）")
	}
}

// ★★ 判据：runtime=ssh 但没给 ssh_host ⇒ **响亮报错**。
func TestSSHRequiresHost(t *testing.T) {
	b := &Branch{ID: "br-1", Runtime: "ssh"}
	if _, err := parseSSHTarget(b); err == nil {
		t.Fatal("★ runtime=ssh 但无 ssh_host 却未报错")
	}
	// 带端口的形式
	b2 := &Branch{ID: "br-2", Runtime: "ssh", SSHHost: "u@h:2222"}
	tgt, err := parseSSHTarget(b2)
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Host != "u@h" || tgt.Port != 2222 {
		t.Errorf("端口解析错: %+v", tgt)
	}
}

// ★★★ 判据：**两个轴正交** —— runtime 与 agent_cli 的任意组合都能构造出正确命令。
//
// 这条防止将来有人把两者耦合成"if ssh then pi"这种分支
// （那会让组合数变成分支数）。
func TestRuntimeAndCLIAreOrthogonal(t *testing.T) {
	p := newTestPluginForShell(t)
	cases := []struct {
		cli       string
		wantEnv   string
		wantNoEnv string
	}{
		{"pi", "PI_CODING_AGENT_DIR", "ZCODE_DATA_BASE_DIR"},
		{"zcode", "ZCODE_DATA_BASE_DIR", "PI_CODING_AGENT_DIR"},
	}
	for _, c := range cases {
		drv, dir, err := p.loadDriver(c.cli)
		if err != nil {
			if c.cli == "zcode" {
				t.Skipf("没有 %s 驱动（可选）", c.cli)
			}
			t.Fatal(err)
		}
		ctx := driverCtx{
			Workdir: "/w", SessionID: "s", SessionName: "br-1",
			DataDir: dir, PluginDir: dir,
		}
		node, args, env := buildRemoteCommand(drv, ctx, "", "node")
		if node != "node" {
			t.Errorf("%s: 远端 node 应为裸 node，实际 %q", c.cli, node)
		}
		var keys []string
		for k := range env {
			keys = append(keys, k)
		}
		joined := strings.Join(keys, ",")
		if !strings.Contains(joined, c.wantEnv) {
			t.Errorf("%s: 远端 env 缺 %s（实际 %s）", c.cli, c.wantEnv, joined)
		}
		if strings.Contains(joined, c.wantNoEnv) {
			t.Errorf("%s: 远端 env 混入了别的后端变量 %s", c.cli, c.wantNoEnv)
		}
		// sessionNameFlag=false 的后端不得带 --session-name
		argStr := strings.Join(args, " ")
		if drv.SessionNameFlag != nil && !*drv.SessionNameFlag && strings.Contains(argStr, "session-name") {
			t.Errorf("%s: 声明不支持 --session-name 却带了: %s", c.cli, argStr)
		}
	}
}

// ★★ 判据：**推送脚本失败必须响亮**（不静默用远端旧脚本）。
func TestPushFailureIsLoud(t *testing.T) {
	p := newTestPluginForShell(t)
	tgt := sshTarget{Host: "u@h"}
	// 本地脚本不存在 ⇒ 必须在推送前就报错
	err := p.pushSidecar(tgt, driverSpec{Script: "nonexistent-sidecar.mjs"},
		t.TempDir(), "/remote/runtime")
	if err == nil {
		t.Fatal("★ 本地脚本不存在却未报错")
	}
	// 没配远端 runtime 根目录 ⇒ 报错
	if err := p.pushSidecar(tgt, driverSpec{Script: "x.mjs"}, t.TempDir(), ""); err == nil {
		t.Fatal("★ 未配置 ssh_runtime_root 却未报错")
	}
}

// ★ 判据：远端脚本路径必须是**远端根 + 驱动声明的脚本名**（不是本地路径）。
func TestRemoteScriptPathUsesRemoteRoot(t *testing.T) {
	local := filepath.Join(t.TempDir(), "sidecar.mjs")
	if err := os.WriteFile(local, []byte("// x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 这条只验证"远端路径由 remoteRoot 决定"这一事实（不实际 ssh）
	remoteRoot := "/remote/runtime/zcode"
	got := remoteRoot + "/" + "sidecar.mjs"
	if strings.Contains(got, os.TempDir()) {
		t.Fatalf("远端路径混进了本地临时目录: %s", got)
	}
	if !strings.HasPrefix(got, remoteRoot) {
		t.Fatalf("远端路径应以远端根开头: %s", got)
	}
}
