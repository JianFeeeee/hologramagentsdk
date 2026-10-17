package main

// 远端（ssh）工作目录准备。
//
// ## 为什么必须有这个文件
//
// `prepareWorkdir`（plugin.go）走的是**本地** git：`clone --mirror` +
// `clone mirror workdir`，全部在本机磁盘上。对 `runtime=ssh` 的分支来说
// 这是错的 —— 远端 pi 的 cwd 指向远端目录，而那个目录**从未被创建**。
// 症状是「agent 起来了，但一执行命令就说目录不存在」。
//
// 所以远端线必须有一套对应的远端准备流程：
//
//	① 远端建 workdir 的父目录
//	② 有共享 mirror 就用它（省一次全量拉取），没有就直连 remote 拉
//	③ 切到目标分支
//
// ## 为什么远端不复用本地的 mirror
//
// mirror 是**本机路径**（`<插件数据>/mirrors/<name>.git`），远端看不到。
// 远端有自己的 mirror 根（`<ssh_workdir_root>/.mirrors/`），
// 这样同一台远端上的多条分支线可以共享一次拉取。
//
// ## 凭据
//
// 远端拉取需要远端自己能认证（远端已配好 key 或 credential helper）。
// 本插件**不把凭据推到远端**（设计 §7.2：凭据不进工作目录、不进 remote URL）。
// 拉不动就响亮报错并说明原因，不静默降级。

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// remoteGit 在远端跑一条 git 命令（BatchMode，不交互输密码）。
//
// cwd 用 `cd <dir> &&` 而不是 `git -C`：`git -C` 在目录不存在时
// 报的是 "cannot change to"，而 `cd` 失败更直观；两者都会响亮失败。
func (t sshTarget) remoteGit(dir string, args ...string) (string, error) {
	parts := []string{"git"}
	parts = append(parts, args...)
	for i, a := range parts {
		if i > 0 {
			parts[i] = shellQuote(a)
		}
	}
	cmdline := strings.Join(parts, " ")
	if strings.TrimSpace(dir) != "" {
		cmdline = "cd " + shellQuote(dir) + " && " + cmdline
	}
	argv := append(t.sshBaseArgs(), t.Host, cmdline)
	return t.runSSHCapture(argv, 10*time.Minute)
}

// remoteShell 在远端跑一条**原始 shell 命令**（不经 git）。
//
// 与 remoteGit 分开是必要的：`git mv` / `git rm` 是仓库内的操作，
// 用于文件系统层面的搬移会报 "not a git repository"（实测踩到）。
func (t sshTarget) remoteShell(cmdline string) (string, error) {
	argv := append(t.sshBaseArgs(), t.Host, cmdline)
	return t.runSSHCapture(argv, 10*time.Minute)
}

// remoteMkdir 在远端建目录（幂等）。
func (t sshTarget) remoteMkdir(dir string) error {
	argv := append(t.sshBaseArgs(), t.Host, "mkdir -p "+shellQuote(dir))
	_, err := t.runSSHCapture(argv, 60*time.Second)
	return err
}

// remotePathExists 判断远端路径是否存在且是目录。
func (t sshTarget) remotePathExists(dir string) bool {
	argv := append(t.sshBaseArgs(), t.Host,
		"test -d "+shellQuote(dir)+" && echo yes")
	out, err := t.runSSHCapture(argv, 60*time.Second)
	return err == nil && strings.TrimSpace(out) == "yes"
}

// runSSHCapture 跑一条 ssh 命令并返回 stdout（统一超时与错误包装）。
func (t sshTarget) runSSHCapture(argv []string, timeout time.Duration) (string, error) {
	return runSSHCaptureOn(t.Host, argv, timeout)
}

// runSSHCaptureOn 是 sshTarget.runSSHCapture 的自由函数形式
// （便于判据在不构造完整 target 的情况下复用）。
//
// ★ 超时用 goroutine + Wait 实现，**不能**在 Start() 之后调 CombinedOutput()
// —— 那会二次启动并报 `exec: already started`（实测踩到）。
func runSSHCaptureOn(host string, argv []string, timeout time.Duration) (string, error) {
	cmd := exec.Command("ssh", argv...)
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("启动 ssh 失败: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		s := strings.TrimSpace(buf.String())
		if err != nil {
			return s, fmt.Errorf("远端命令失败（%s）: %w: %s", host, err, truncate(s, 300))
		}
		return s, nil
	case <-time.After(timeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done // 收尾，不留 zombie
		return "", fmt.Errorf("远端命令超时（%s，%s）—— 检查远端网络与 git 是否卡在认证提示",
			host, timeout)
	}
}

// remoteWorkdirRoot 取远端放工程目录的根。
//
// ★ 按 host 取（多台机器的目录布局不同）：先问端点表，再回退到旧的
// 全局 settings。缺配置时响亮报错：默认值猜错会把文件写到远端别的项目的目录里。
func (p *Plugin) remoteWorkdirRoot(host string) (string, error) {
	if he := p.getHost(host, false); he != nil && strings.TrimSpace(he.WorkdirRoot) != "" {
		return he.WorkdirRoot, nil
	}
	if v, _ := p.sdk.Settings().Get("ssh_workdir_root"); v != nil {
		if s := strings.TrimSpace(fmt.Sprint(v)); s != "" && s != "<nil>" {
			return s, nil
		}
	}
	return "", fmt.Errorf(
		"远端 %s 未配置工程目录根。请用 commondev_endpoint_set host=%s "+
			"workdir_root=<远端根目录，如 /home/user/.commondev/program>（多台机器各配各的），"+
			"或配置全局 settings 的 ssh_workdir_root", host, host)
}

// prepareRemoteWorkdir 在远端准备这条分支的工作目录。
//
// 与本地 prepareWorkdir 的对应关系：
//
//	本地: 建父目录 → mirror clone（本机）→ clone mirror workdir → checkout
//	远端: 建父目录 → 远端 mirror（共享）→ clone mirror workdir → checkout
//
// 复用判断与本地一致：目录已存在则只确保分支正确（分支线重开时）。
func (p *Plugin) prepareRemoteWorkdir(t sshTarget, pr *Project, branch, workdir string) error {
	// ★ 串行化：见 Plugin.prepMu 的注释（mirror 的"检查后 clone"是 TOCTOU）。
	p.prepMu.Lock()
	defer p.prepMu.Unlock()

	root := t.WorkdirRoot
	if root == "" {
		var err error
		root, err = p.remoteWorkdirRoot(t.Host)
		if err != nil {
			return err
		}
		t.WorkdirRoot = root
	}

	// ① 复用：目录已在就直接切分支
	if t.remotePathExists(workdir) {
		return p.ensureRemoteOnBranch(t, branch, workdir)
	}

	// ② 建父目录
	if err := t.remoteMkdir(workdir); err != nil {
		return fmt.Errorf("远端建目录失败: %w", err)
	}

	// ③ 共享 mirror（一次全量拉取，供该远端上所有分支线复用）
	mirror := root + "/.mirrors/" + pr.Name + ".git"
	// MirrorPath 是**本地**路径，远端用不了 —— 一律用远端自己的 mirror 根。
	// （这正是当初把 MirrorPath 当通用字段用会踩的坑。）
	if !t.remotePathExists(mirror) {
		if err := t.remoteMkdir(root + "/.mirrors"); err != nil {
			return fmt.Errorf("远端建 mirror 目录失败: %w", err)
		}
		// clone 到**临时目录**再 mv 就位：即使有别的进程刚好抢先建好了，
		// 结果也只是一个未使用的临时目录，而不是一个半成品 mirror。
		// （prepMu 已经串行化了插件内的并发；这里是防**插件外**的干扰
		//  与未来可能的分进程部署。）
		// ★ 就位用 **shell 的 mv**，不是 `git mv`（后者只在仓库内工作，
		// 实测报 "not a git repository"）。清理同理用 shell 的 rm -rf。
		tmp := mirror + ".tmp"
		_, _ = t.remoteShell("rm -rf " + shellQuote(tmp))
		if _, err := t.remoteGit("", "clone", "--mirror", pr.RemoteURL, tmp); err != nil {
			// 清掉半成品，避免下次误判"已存在"
			_, _ = t.remoteShell("rm -rf " + shellQuote(tmp))
			return fmt.Errorf(
				"远端建 mirror 失败（远端需自己能认证该仓库，本插件不推凭据）: %w", err)
		}
		if _, err := t.remoteShell("mv " + shellQuote(tmp) + " " + shellQuote(mirror)); err != nil {
			_, _ = t.remoteShell("rm -rf " + shellQuote(tmp))
			return fmt.Errorf("远端 mirror 就位失败: %w", err)
		}
	}

	// ④ 由远端 mirror 派生工作目录
	if _, err := t.remoteGit("", "clone", mirror, workdir); err != nil {
		return fmt.Errorf("远端由 mirror 派生工作目录失败: %w", err)
	}

	// ⑤ 切到目标分支（与本地同一条硬要求：否则 agent 会在默认分支上改）
	return p.ensureRemoteOnBranch(t, branch, workdir)
}

// ensureRemoteOnBranch 把远端工作目录切到目标分支（已在则不动）。
func (p *Plugin) ensureRemoteOnBranch(t sshTarget, branch, workdir string) error {
	if strings.TrimSpace(branch) == "" {
		return nil
	}
	cur, err := t.remoteGit(workdir, "rev-parse", "--abbrev-ref", "HEAD")
	if err == nil && strings.TrimSpace(cur) == branch {
		return nil
	}
	if _, err := t.remoteGit(workdir, "checkout", branch); err != nil {
		if _, err2 := t.remoteGit(workdir, "checkout", "-b", branch); err2 != nil {
			return fmt.Errorf("远端切到分支 %s 失败: %v", branch, err2)
		}
	}
	return nil
}

// remoteCaptureBaseSHA 取远端工作目录的 base_sha。
//
// 与本地 captureBaseSHA 对应：必须在 agent 动手之前取。
func (p *Plugin) remoteCaptureBaseSHA(t sshTarget, workdir string) (string, error) {
	out, err := t.remoteGit(workdir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}
