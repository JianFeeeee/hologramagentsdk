package main

// 传输层：local（exec）| ssh（远端同一份 sidecar）。
//
// ## 设计依据（续篇 §3.1）
//
//	「默认本地内嵌，配置切到远程 SSH。两者**共用同一份 sidecar**，
//	  差别只在**传输层**（本地 exec vs 远程 ssh）」
//
// ⇒ 所以这不改变 sidecar 契约、不改变驱动声明 —— 只是把「怎么起进程」
// 从 `exec.Command(node, script, args…)` 换成 `ssh host <同样的命令>`。
//
// ## 两个轴是正交的（关键）
//
//	Runtime  = local | ssh       ← 在哪跑（本文件）
//	AgentCLI = pi | zcode | ...  ← 跑什么（sidecar_driver.go）
//
// 四种组合都成立：本地 pi / 本地 zcode / 远端 pi / 远端 zcode。
// 组合数不该让代码分支数量相乘 —— 所以传输层只关心「命令行 + 环境」，
// 完全不知道跑的是哪个 CLI。
//
// ## ★ SSH 的硬边界（必须诚实，不能假装等价）
//
//  1. **环境变量不能像本地那样注**：`ssh host cmd` 不继承本地环境。
//     必须把驱动要求的 env **显式写进远端命令**（`env K=V … node script …`）。
//  2. **node 可能是远端的**：本地随包分发的 node 在远端不存在。
//     ⇒ node 路径必须可被驱动声明为**远端路径**（`Node` 字段），
//     且默认值应是 `node`（走远端 PATH），而不是本地绝对路径。
//  3. **脚本要先在远端存在**：本地改的 sidecar.mjs 不会自动出现在远端。
//     ⇒ 提供 `syncSidecar` 把一个目录下的脚本推过去（首次拉起时）。
//  4. **退出码/信号语义不同**：ssh 会把远端退出码透传，但进程树杀不掉
//     （本地 kill 杀不到远端），必须用远端 `timeout`/`pkill` 兜底。
//  5. **stdin 经 ssh 传递是可行的**（ssh 转发 stdin），所以脚本仍走 stdin ——
//     这条安全纪律（明文不进 argv）在远端同样成立。
//
// 第 4 条决定了：远端场景下**超时要在远端执行**，不能只靠本地 kill。

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// sshTarget 描述一个远端。
type sshTarget struct {
	// Host 是 ssh 的目标，如 `user@host` 或 ~/.ssh/config 里的别名。
	Host string
	// Port 可选。
	Port int
	// WorkdirRoot 是远端放工程目录的根（镜像本地 program/ 的角色）。
	WorkdirRoot string
	// NodePath 是**远端**的 node（默认 `node`，走远端 PATH）。
	NodePath string
	// RemoteRuntimeRoot 是远端放 sidecar 脚本的目录。
	RemoteRuntimeRoot string
}

// parseSSHTarget 从分支的远端声明里解析出目标。
//
// 分支上只存一个字符串（`user@host` 或 `user@host:port`），其余用默认值 ——
// 让台账保持简单，高级配置走 settings。
func parseSSHTarget(b *Branch) (sshTarget, error) {
	raw := strings.TrimSpace(b.SSHHost)
	if raw == "" {
		return sshTarget{}, fmt.Errorf(
			"分支 %s 声明 runtime=ssh 但没给 ssh_host（形如 user@host 或 alias）", b.ID)
	}
	t := sshTarget{Host: raw, NodePath: "node"}
	if i := strings.LastIndex(raw, ":"); i > 0 && !strings.Contains(raw[i:], "@") {
		// user@host:2222 → 拆出端口
		var port int
		if _, err := fmt.Sscanf(raw[i+1:], "%d", &port); err == nil && port > 0 {
			t.Host = raw[:i]
			t.Port = port
		}
	}
	return t, nil
}

// sshBaseArgs 是 ssh 的公共参数。
//
// BatchMode=yes：**不交互**。若没配好免密，必须立刻失败 ——
// 否则 ssh 会停在密码提示上等输入，而工具调用会一直挂着直到 60s 超时，
// 表现为「拉起 agent 卡住」而不是「没有免密」（排查方向完全不同）。
func (t sshTarget) sshBaseArgs() []string {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=10",
	}
	if t.Port > 0 {
		args = append(args, "-p", fmt.Sprint(t.Port))
	}
	return args
}

// sshCommand 把「远端要跑的命令」包成一条 ssh 调用。
//
// ★ 环境变量显式前置（`env K=V …`）—— ssh 不继承本地环境。
// 这是 SSH 与本地最大的行为差异，也是本地能跑、远端跑不起来的头号原因。
func (t sshTarget) sshCommand(node string, script string, args []string, env map[string]string) []string {
	remote := []string{}
	if len(env) > 0 {
		remote = append(remote, "env")
		// 固定顺序（map 随机 ⇒ 命令不可对照）
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, k := range keys {
			remote = append(remote, shellQuote(k+"="+env[k]))
		}
	}
	remote = append(remote, shellQuote(node), shellQuote(script))
	for _, a := range args {
		remote = append(remote, shellQuote(a))
	}
	argv := t.sshBaseArgs()
	argv = append(argv, t.Host, strings.Join(remote, " "))
	return argv
}

// shellQuote 单引号转义（远端命令要过一层 shell）。
//
// ★ 必须转义：工作目录、session id、驱动声明的 env 值都可能含空格/引号，
// 不转义就会被远端 shell 当作多个参数 —— 那是最典型的"本地好、远端坏"。
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	// 安全字符集直接返回（可读性）
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.' || r == '/' || r == ':' || r == '@' || r == '=') {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildRemoteCommand 组装远端启动命令（供 ssh 与本地共用同一套语义）。
//
// 返回 (node, script, args, env)，其中 env 已按驱动展开。
func buildRemoteCommand(drv driverSpec, ctx driverCtx, script string, node string) (string, []string, map[string]string) {
	env := map[string]string{}
	for k, v := range drv.Env {
		env[k] = drv.expand(v, ctx)
	}
	return node, drv.buildArgs(ctx), env
}

// buildRemoteCommandFor 按**端点表里该机器该后端**的实际配置拼远端命令。
//
// ★ 这是相对 buildRemoteCommand 的关键修正：后者用 `drv.buildArgs(ctx)`，
// 其中 `drv.CLIPath` 会被展开成 `--cli <**本机**路径>`（如 `{rawfile}` → 本机插件目录）
// 然后原样发到远端 —— 远端上那个路径百分之百不存在。
//
// 两个真实后果（单台远端时都被掩盖，多台时必现）：
//
//  1. 远端 zcode 起不来，报的却是**远端 shell 的** “No such file”，
//     用户会去远端找原因，而真正的问题是本地把路径发错了。
//  2. 每台机器的 CLI 安装位置不同（这正是在端点表里按 host 分开的原因），
//     单一全局值必然至少对一台是错的。
//
// 所以：远端命令里的 `--cli` **只能**用端点表解析出来的值。
func buildRemoteCommandFor(drv driverSpec, ctx driverCtx, node string, ep resolvedEndpoint) (string, []string, map[string]string) {
	env := map[string]string{}
	for k, v := range drv.Env {
		env[k] = drv.expand(v, ctx)
	}
	// 端点表声明的额外 env（值同样支持占位符）
	for k, v := range ep.Env {
		env[k] = drv.expand(v, ctx)
	}

	args := make([]string, 0, len(drv.Args)+2)
	for _, a := range drv.Args {
		args = append(args, drv.expand(a, ctx))
	}
	// ★ 只加远端自己那条路径。这里**刻意不复用** buildArgs，
	// 因为 buildArgs 会带上 drv.CLIPath（本地值）。
	if strings.TrimSpace(ep.CLIPath) != "" {
		args = append(args, "--cli", ep.CLIPath)
	}
	return node, args, env
}

// pushSidecar 把本地 driver 目录下的脚本推到远端（幂等）。
//
// 为何需要：本地改的 sidecar.mjs 不会自动出现在远端。不推的话，
// 远端跑的是**上次推的旧版** —— 而症状是"改了没生效"，极难归因。
//
// 用 `scp -q`；失败**响亮报错**（不静默继续用远端旧脚本）。
func (p *Plugin) pushSidecar(t sshTarget, drv driverSpec, localDir string, remoteRoot string) error {
	if remoteRoot == "" {
		return fmt.Errorf("远端 runtime 目录未配置（ssh_runtime_root）")
	}
	// 远端建目录
	mk := append(t.sshBaseArgs(), t.Host, "mkdir -p "+shellQuote(remoteRoot))
	if out, err := exec.Command("ssh", mk...).CombinedOutput(); err != nil {
		return fmt.Errorf("远端建目录失败: %s: %w", strings.TrimSpace(string(out)), err)
	}
	local := filepath.Join(localDir, drv.Script)
	if _, err := os.Stat(local); err != nil {
		return fmt.Errorf("本地找不到 sidecar 脚本: %s", local)
	}
	// scp（端口与 ssh 一致）
	args := []string{"-q", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new"}
	if t.Port > 0 {
		args = append(args, "-P", fmt.Sprint(t.Port))
	}
	args = append(args, local, t.Host+":"+remoteRoot+"/")
	if out, err := exec.Command("scp", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("推送 sidecar 脚本失败（远程可能仍是旧脚本）: %s: %w",
			strings.TrimSpace(string(out)), err)
	}
	return nil
}

// probeSSH 探测远端可用性（免密 + node）。
//
// 拉起 agent 之前先探一次：**早失败**。直接拉起的话，失败会混在
// sidecar 握手超时里（10s 后报"握手失败"），而真因可能是没免密。
// probeSSH 探测远端可用性（免密 + node）。
//
// ★ 必须用 t.NodePath 而不是硬编码 "node"：
// ssh_node_path 的存在意义就是"远端 node 不在 PATH 里"
// （例如 /opt/node/bin/node）。初版探的是 `node`，于是**用户按提示配好了
// ssh_node_path，probe 却仍然失败**并报"远端不可达或未配免密" ——
// 把一个配置问题伪装成连通性问题，方向完全错。
func (p *Plugin) probeSSH(t sshTarget) error {
	node := strings.TrimSpace(t.NodePath)
	if node == "" {
		node = "node"
	}
	argv := append(t.sshBaseArgs(), t.Host, shellQuote(node)+" --version")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", argv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf(
			"远端不可达或未配免密（ssh %s）: %s —— 本插件不交互输密码，请先配好免密",
			t.Host, strings.TrimSpace(string(out)))
	}
	if !strings.HasPrefix(strings.TrimSpace(string(out)), "v") {
		return fmt.Errorf("远端 `node --version` 输出异常: %q", strings.TrimSpace(string(out)))
	}
	return nil
}

// startSSHSidecar 起一个远端 sidecar 进程（本地 ssh 客户端持有 stdin/stdout）。
//
// ★ 与本地路径**共用同一份 sidecar 契约**：ssh 转发 stdin/stdout，
// 所以脚本仍走 stdin（明文不进 argv 的纪律在远端同样成立）。
func (p *Plugin) startSSHSidecar(b *Branch, drv driverSpec, ctx driverCtx, cli string) (*sidecarProcess, error) {
	t, err := parseSSHTarget(b)
	if err != nil {
		return nil, err
	}

	// ★ 端点解析：host 默认 → (host,cli) 覆盖 → driver.json 兜底。
	// 合并逻辑在 endpoints.go（含旧 settings 的兼容回退）。
	ep := p.resolveEndpoint(t.Host, cli, drv, ctx)
	t.NodePath = ep.NodePath
	remoteRoot := ep.RuntimeRoot
	if remoteRoot == "" {
		return nil, fmt.Errorf(
			"未配置远端 runtime 目录（放 sidecar 脚本的地方）。"+
				"请用 commondev_endpoint_set host=%s runtime_root=<远端目录>，"+
				"或配置全局 settings 的 ssh_runtime_root", t.Host)
	}

	// ★ 本机路径絕不下发（含本机占位符 / 本机绝对路径两种情况）。
	// 必须在 ssh 之前拦，否则错误来自远端 shell，排查方向全错。
	if err := validateRemoteCLIPath(ep.CLIPath, cli); err != nil {
		return nil, err
	}

	if err := p.probeSSH(t); err != nil {
		return nil, err
	}
	// 推脚本（幂等）。不推的话远端跑的是旧版而症状是"改了没生效"。
	if err := p.pushSidecar(t, drv, p.driverDir(cli), remoteRoot); err != nil {
		return nil, err
	}

	node, args, env := buildRemoteCommandFor(drv, ctx, t.NodePath, ep)
	remoteScript := remoteRoot + "/" + drv.Script
	sshArgv := t.sshCommand(node, remoteScript, args, env)

	cmd := exec.Command("ssh", sshArgv...)
	cmd.Dir = ctx.Workdir // 本地 cwd 无关紧要，但保持一致
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 ssh 失败: %w", err)
	}

	timeout := defaultSidecarTimeout
	if v, _ := p.sdk.Settings().Get("sidecar_timeout_sec"); v != nil {
		if n, err := parseInt(v); err == nil && n > 0 {
			timeout = time.Duration(n) * time.Second
		}
	}
	sp := newSidecarProcess(b.ID, ctx.Workdir, cmd, bufio.NewWriter(stdin), stdout, timeout)
	sp.handshakeTimeout = 20 * time.Second // ssh 建链比本地慢
	if ping, err := sp.call("ping", nil); err != nil {
		sp.Close()
		return nil, fmt.Errorf("远端 %s sidecar 握手失败: %w", cli, err)
	} else if m, ok := ping.(map[string]interface{}); ok {
		for _, k := range []string{"pi_version", "cli_version", "version"} {
			if v, ok := m[k].(string); ok && v != "" {
				b.PiVersion = v
				break
			}
		}
	}
	return sp, nil
}

func parseInt(v interface{}) (int, error) {
	var n int
	_, err := fmt.Sscanf(fmt.Sprint(v), "%d", &n)
	return n, err
}
