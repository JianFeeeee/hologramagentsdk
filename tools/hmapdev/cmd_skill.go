package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// hmapdev skill —— 把 SDK 里的插件开发知识装到各 agent 的 skills 目录。
//
// ## 为什么需要这个命令
//
// HologramAgent 插件开发的知识（hmapdev 工具链、SDK 版本坑、plg.json、
// plugin.bin 部署、内置 vs 独立二进制）此前只存在于**某个 agent 的对话历史**里。
// 本机四个 agent 都不会开发插件，因为它们没有任何渠道拿到这些知识。
//
// 知识属于 SDK（与工具链同源、随 SDK 版本走），所以源在 SDK 仓的 `skills/`，
// 由本命令分发。
//
// ## 两条策略，与别处**故意相反**
//
//  1. **总是覆盖**。skill 是**工具生成物**，不是用户数据。
//     保留用户改过的版本会让它与工具链脱节 —— 而工具链的命令面会随版本变。
//     （对比：`hmapdev build` 写出的适配器更新会**保护**用户修改，
//     因为那是运行时代码；skill 只是说明文档。）
//  2. **只装已实测存在的目标**，不扫 glob。不认识的目录建出来也没用，
//     还会让用户以为装上了。

// skillTarget 描述一个 agent 的 skills 目录。
type skillTarget struct {
	name string
	rel  string // 相对 home
}

// knownSkillTargets 是已实测存在的 agent skills 目录。
var knownSkillTargets = []skillTarget{
	{"pi", ".pi/agent/skills"},
	{"claude", ".claude/skills"},
	{"codex", ".codex/skills"},
	{"agents", ".agents/skills"},
}

func skillHelp() {
	fmt.Println(`hmapdev skill <command>

Distribute HologramAgent plugin-development knowledge to agent skill directories.

Commands:
  list                 List skills shipped with the active SDK
  install [name...]    Install skills into agent skill directories
  path                 Show the skills source directory of the active SDK

Install targets (only these; unknown directories are not created):
  ~/.pi/agent/skills     ~/.claude/skills
  ~/.codex/skills         ~/.agents/skills

Notes:
  · Source is the **active SDK**'s skills/ directory. Pick the SDK first with
    "hmapdev sdk use <version>".
  · Installation always **overwrites**: a skill is a tool-generated artifact,
    not user data. Keeping user edits would let it drift from the toolchain.
  · Idempotent: installing twice yields the same result.`)
}

func cmdSkill(args []string) {
	if len(args) == 0 {
		skillHelp()
		return
	}
	switch args[0] {
	case "list":
		cmdSkillList()
	case "install":
		cmdSkillInstall(args[1:])
	case "path":
		cmdSkillPath()
	case "-h", "--help", "help":
		skillHelp()
	default:
		fmt.Printf("unknown skill command: %s\n\n", args[0])
		skillHelp()
		os.Exit(1)
	}
}

// skillsSourceDir 返回 SDK 内的 skills 源目录。
func skillsSourceDir(sdkRoot string) string {
	return filepath.Join(sdkRoot, "skills")
}

// resolveSkillsSource 定位 skill 源目录。
//
// 优先用**hmapdev 自己所在仓库**的 skills/，回退到 store 里的活跃 SDK。
//
// ★ 为什么需要这个回退顺序
//
// store（~/.hologram/hmapdev/sdk/<v>/）是 `hmapdev sdk install` 复制的**副本**。
// 而 skill 是纯文档、加它不需要动 SDK 的编译产物 —— 于是「改了 skill 却要
// 重装 SDK 才能生效」，对日常维护很不合理。
//
// hmapdev 在 SDK 仓里用 `go build` 构建时，它自身就在仓库内
// （tools/hmapdev → ../../skills），此时仓库是**最新的**，应当优先。
// 发布出去的二进制不在仓库里，两条路径都落空时才报错。
func resolveSkillsSource() (string, string) {
	// 1) hmapdev 自身所在仓库
	if exe, err := os.Executable(); err == nil {
		if p := repoSkillsFromExe(exe); p != "" {
			return p, "仓库（hmapdev 构建自 SDK 源）"
		}
	}
	// 2) store 里的活跃 SDK
	if root := activeSDKRoot(); root != "" {
		p := skillsSourceDir(root)
		if _, err := os.Stat(p); err == nil {
			return p, "SDK store（" + root + "）"
		}
	}
	return "", ""
}

// repoSkillsFromExe 从 hmapdev 可执行文件位置反推 SDK 仓根，再取 skills/。
// 形如 <sdk>/tools/hmapdev/hmapdev ⇒ <sdk>/skills
func repoSkillsFromExe(exe string) string {
	dir := filepath.Dir(exe) // <sdk>/tools/hmapdev
	// go.mod 声明 go1.21 ⇒ 不能用 range-over-int（需 1.22）
	for i := 0; i < 3; i++ {
		cand := filepath.Join(dir, "skills")
		if st, err := os.Stat(cand); err == nil && st.IsDir() {
			// 确认这确实像 SDK 仓（有 go.mod 且 module 是 homeagent-sdk）
			if gm, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil &&
				containsSDKModule(string(gm)) {
				return cand
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func cmdSkillPath() {
	dir, origin := resolveSkillsSource()
	if dir == "" {
		fmt.Println("error: 找不到 skills 目录")
		fmt.Println("  装一个带 skills 的 SDK：hmapdev sdk install latest")
		skillHelp()
		os.Exit(1)
	}
	fmt.Printf("%s\n  来源：%s\n", dir, origin)
}

func cmdSkillList() {
	src, origin := resolveSkillsSource()
	if src == "" {
		fmt.Println("error: 找不到 skills 目录")
		fmt.Println("  装一个带 skills 的 SDK：hmapdev sdk install latest")
		os.Exit(1)
	}
	names, err := readSkillNames(src)
	if err != nil {
		fmt.Printf("error: read %s: %v\n", src, err)
		fmt.Println("  If this SDK predates skills/, upgrade: hmapdev sdk install latest")
		os.Exit(1)
	}
	if len(names) == 0 {
		fmt.Printf("no skills in %s\n", src)
		return
	}
	fmt.Printf("Skills in %s（%s）:\n", src, origin)
	for _, n := range names {
		fmt.Printf("  %s\n", n)
	}
	fmt.Println()
	fmt.Println("Install with: hmapdev skill install")
}

func cmdSkillInstall(names []string) {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Printf("error: cannot determine home directory: %v\n", err)
		os.Exit(1)
	}
	src, origin := resolveSkillsSource()
	if src == "" {
		fmt.Println("error: 找不到 skills 目录")
		fmt.Println("  装一个带 skills 的 SDK：hmapdev sdk install latest")
		os.Exit(1)
	}
	available, err := readSkillNames(src)
	if err != nil {
		fmt.Printf("error: read %s: %v\n", src, err)
		fmt.Println("  If this SDK predates skills/, upgrade: hmapdev sdk install latest")
		os.Exit(1)
	}
	if len(available) == 0 {
		fmt.Printf("no skills to install from %s\n", src)
		return
	}

	// 不给名字就全装
	want := names
	if len(want) == 0 {
		want = available
	}
	// 显式拒绝不存在的 skill，而不是静默跳过 —— 静默跳过会让用户以为装上了
	for _, w := range want {
		found := false
		for _, a := range available {
			if a == w {
				found = true
				break
			}
		}
		if !found {
			fmt.Printf("error: no such skill: %s\n", w)
			fmt.Printf("  available: %s\n", strings.Join(available, ", "))
			os.Exit(1)
		}
	}

	// 只装到**已存在**的目标目录。
	//
	// 这里与判据里的"不建未知目录"配套：对不存在的目录直接报告，
	// 让用户自己确认路径，而不是静默创建一个可能没人读的空目录。
	installed, skipped := 0, 0
	for _, w := range want {
		from := filepath.Join(src, w)
		for _, tt := range knownSkillTargets {
			base := filepath.Join(home, tt.rel)
			if _, err := os.Stat(base); os.IsNotExist(err) {
				skipped++
				continue
			}
			to := filepath.Join(base, w)
			if err := copyDir(from, to); err != nil {
				fmt.Printf("error: install %s -> %s: %v\n", w, tt.name, err)
				os.Exit(1)
			}
			fmt.Printf("  %-8s %s\n", tt.name, to)
			installed++
		}
	}
	fmt.Printf("\nsource: %s（%s）\n", src, origin)
	fmt.Printf("installed %d skill(s) into %d target(s)", len(want), installed)
	if skipped > 0 {
		fmt.Printf("; skipped %d target(s) whose directory does not exist", skipped)
	}
	fmt.Println()
	if installed == 0 {
		fmt.Println("  No agent skills directory found. Create one of:")
		for _, tt := range knownSkillTargets {
			fmt.Printf("    %s\n", filepath.Join(home, tt.rel))
		}
		os.Exit(1)
	}
}

// readSkillNames 列出 skills/ 下的 skill 名（跳过隐藏目录与非目录）。
func readSkillNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}
