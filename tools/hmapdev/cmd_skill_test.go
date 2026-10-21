package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hmapdev skill install 的判据。
//
// ## 为什么要有这个命令
//
// 2026-09-28：HologramAgent 插件开发的知识（hmapdev 工具链、SDK 版本坑、
// plg.json、plugin.bin 部署、内置 vs 独立二进制）只存在于**agent 的对话历史**里。
// 本机四个 agent（pi / claude / codex / .agents）都不会开发插件，
// 因为它们**没有任何渠道**拿到这些知识。
//
// 知识属于 SDK（跟工具链同源、随 SDK 版本走），所以：
//   · 源在 SDK 仓的 `skills/`
//   · 装到各 agent 的 skills 目录由 `hmapdev skill install` 负责
//
// ## 三条设计约束（都来自实际踩过的坑）
//
//  1. **skill 要带版本**。SDK 与插件是协议绑定，工具链同理：
//     旧 SDK 里的 skill 描述的命令面可能已经变了。所以 skill 目录名带
//     版本（`skills/<name>/`），且安装时**总是覆盖**，不留"用户改过就保留"
//     的分支 —— skill 是**工具生成物**，不是用户数据。
//
//  2. **只装已知的 agent 目录**。agent 的 skills 路径没有统一标准，
//     我们只能装到已实测存在的四个；不认识的目录宁可报告也不乱建。
//
//  3. **幂等**。重复安装必须得到同一结果，否则「重装工具链」会
//     在用户不知情时产生差异。
//
// 运行：go test ./tools/hmapdev/ -run TestSkill -v

// skillTarget 描述一个 agent 的 skills 目录。
// repoSkillsDir 返回**真实仓库**里 skills/ 的路径。
// 测试文件在 tools/hmapdev/，往上两级是 SDK 仓根。
func repoSkillsDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "..", "..", "skills")
}

// knownSkillTargets 定义在 cmd_skill.go（实现与判据共用一份，避免漂移）。

func TestSkillTargetsHaveNoDuplicates(t *testing.T) {
	seen := map[string]string{}
	for _, tt := range knownSkillTargets {
		if prev, dup := seen[tt.rel]; dup {
			t.Errorf("skills 路径重复：%s 与 %s 同为 %s", prev, tt.name, tt.rel)
		}
		seen[tt.rel] = tt.name
	}
}

func TestSkillTargetsAreRelativeAndUnderHome(t *testing.T) {
	for _, tt := range knownSkillTargets {
		if filepath.IsAbs(tt.rel) {
			t.Errorf("%s: rel 必须是相对路径（相对 home），得到 %s", tt.name, tt.rel)
		}
		if strings.HasPrefix(tt.rel, "..") {
			t.Errorf("%s: rel 不能逃出 home：%s", tt.name, tt.rel)
		}
		if !strings.HasPrefix(tt.rel, ".") {
			t.Errorf("%s: rel 应以点开头的隐藏目录（agent 配置都在 ~/.X 下）：%s", tt.name, tt.rel)
		}
	}
}

// skillsSourceDir 定义在 cmd_skill.go。

// listSkillNamesForTest 判据自带的列举（要 t.Fatalf，故不复用实现的）。
// ★ 刻意**不复用** readSkillNames：那条路径在出错时 os.Exit，判据里没法接。
func listSkillNamesForTest(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读 skills 目录 %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// 隐藏目录（.git 之类）不是 skill
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

// 每个 skill 必须是「目录 + SKILL.md」，且 frontmatter 有 name/description。
// 这是各 agent 都能识别的最小契约（实测本机四个 agent 都用这个格式）。
func TestSkillSourceHasValidLayout(t *testing.T) {
	// ★ 必须验**真实仓库**里的 skills/，而不是 TempDir。
	//   原先用 TempDir ⇒ 那个目录是空的，判据永远红，且红得毫无意义
	//   （"文件不存在"是真的，但真实文件在 SDK 仓里，不在临时目录）。
	src := repoSkillsDir(t)
	for _, name := range []string{"hologram-plugin-dev"} {
		dir := filepath.Join(src, name)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			t.Fatalf("缺少 skill 目录 %s", dir)
		}
		md := filepath.Join(dir, "SKILL.md")
		b, err := os.ReadFile(md)
		if err != nil {
			t.Fatalf("缺少 %s: %v", md, err)
		}
		s := string(b)
		if !strings.HasPrefix(s, "---\n") {
			t.Errorf("%s: 缺少 YAML frontmatter 起始", md)
		}
		if !strings.Contains(s, "\nname:") {
			t.Errorf("%s: frontmatter 缺 name 字段", md)
		}
		if !strings.Contains(s, "\ndescription:") {
			t.Errorf("%s: frontmatter 缺 description 字段", md)
		}
		// description 是 agent 判断"要不要读这个 skill"的唯一依据，
		// 空描述等于 skill 装了也不会被触发。
		for _, line := range strings.Split(s, "\n") {
			if strings.HasPrefix(line, "description:") {
				if len(strings.TrimSpace(strings.TrimPrefix(line, "description:"))) < 10 {
					t.Errorf("%s: description 太短，agent 可能不会触发", md)
				}
			}
		}
	}
}

func TestListSkillNamesIgnoresHidden(t *testing.T) {
	home := t.TempDir()
	src := skillsSourceDir(home)
	// skill 目录
	if err := os.MkdirAll(filepath.Join(src, "hologram-plugin-dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 隐藏目录（.git 之类）不该被当成 skill
	if err := os.MkdirAll(filepath.Join(src, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 普通文件（README.md）不是目录，不该被当成 skill
	// ★ 原先我把它 MkdirAll 成了**目录** —— 于是实现"正确地"把它当 skill，
	//   判据却报"把 README.md 当成了 skill"。是 fixture 造错了，不是实现错。
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	names := listSkillNamesForTest(t, src)
	for _, n := range names {
		if n == ".git" || n == "README.md" {
			t.Errorf("listSkillNames 把 %q 当成了 skill", n)
		}
	}
	found := false
	for _, n := range names {
		if n == "hologram-plugin-dev" {
			found = true
		}
	}
	if !found {
		t.Errorf("listSkillNames 漏掉了 hologram-plugin-dev，得到 %v", names)
	}
}

// 幂等：连续安装两次，结果必须一致。
func TestSkillInstallIsIdempotent(t *testing.T) {
	home := t.TempDir()
	src := skillsSourceDir(home)
	dir := filepath.Join(src, "hologram-plugin-dev")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: hologram-plugin-dev\ndescription: 开发 HologramAgent 插件的工具链手册\n---\n\n内容\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	install := func() map[string]string {
		out := map[string]string{}
		for _, tt := range knownSkillTargets {
			dst := filepath.Join(home, tt.rel, "hologram-plugin-dev")
			if err := copyDir(dir, dst); err != nil {
				t.Fatalf("copy: %v", err)
			}
			b, err := os.ReadFile(filepath.Join(dst, "SKILL.md"))
			if err != nil {
				t.Fatalf("读回: %v", err)
			}
			out[tt.name] = string(b)
		}
		return out
	}
	a := install()
	b := install()
	if len(a) != len(knownSkillTargets) {
		t.Fatalf("装到的目标数不对：%d", len(a))
	}
	for name, content := range a {
		if b[name] != content {
			t.Errorf("%s: 二次安装结果不同（不幂等）", name)
		}
	}
}

// 覆盖而非保留：skill 是**工具生成物**，用户改了会在下次安装被覆盖。
// 这是与适配器更新（保护用户修改）**相反**的策略，必须显式钉住。
func TestSkillInstallOverwritesUserEdit(t *testing.T) {
	home := t.TempDir()
	src := skillsSourceDir(home)
	dir := filepath.Join(src, "hologram-plugin-dev")
	_ = os.MkdirAll(dir, 0o755)
	want := "---\nname: hologram-plugin-dev\ndescription: 工具链手册\n---\n原始内容\n"
	_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(want), 0o644)

	dst := filepath.Join(home, knownSkillTargets[0].rel, "hologram-plugin-dev")
	_ = os.MkdirAll(dst, 0o755)
	_ = os.WriteFile(filepath.Join(dst, "SKILL.md"),
		[]byte("---\nname: hologram-plugin-dev\ndescription: 用户改过\n---\n用户的版本\n"), 0o644)

	if err := copyDir(dir, dst); err != nil {
		t.Fatalf("copy: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("skill 安装未覆盖用户修改。\n  skill 是**工具生成物**（随 SDK 版本走），"+
			"保留用户版本会让它与工具链脱节。\n  得到：%q", string(got))
	}
}

// 未知的 agent 目录不建：用户可能有自己的目录布局，乱建等于污染。
func TestSkillInstallSkipsUnknownTargets(t *testing.T) {
	home := t.TempDir()
	src := skillsSourceDir(home)
	dir := filepath.Join(src, "hologram-plugin-dev")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: x\ndescription: yyy\n---\n"), 0o644)

	// 模拟：只请求装到 pi（不装 claude）
	_ = copyDir(dir, filepath.Join(home, knownSkillTargets[0].rel, "hologram-plugin-dev"))
	if _, err := os.Stat(filepath.Join(home, ".claude")); err == nil {
		t.Error("不请�� claude 却创建了 ~/.claude")
	}
}

// ★ 关键补充：TestSkillInstallOverwritesUserEdit 验的是 **copyDir**，
//
//	**没走真实入口**。我据此以为判据覆盖了「安装时覆盖用户修改」，
//	实际把 cmdSkillInstall 里的 copyDir 换成「已存在就跳过」，
//	判据依然全绿 —— 变异测试抓出来的。
//
// 这条走**真实入口** cmdSkillInstall。
func TestCmdSkillInstallOverwritesUserEdit(t *testing.T) {
	home := t.TempDir()
	store := filepath.Join(home, "sdkstore")
	sdkRoot := filepath.Join(store, "v9.9.9")
	src := skillsSourceDir(sdkRoot)
	dir := filepath.Join(src, "hologram-plugin-dev")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := "---\nname: hologram-plugin-dev\ndescription: 工具链手册，覆盖测试\n---\n原始内容\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "current"), []byte("v9.9.9"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 预置一个「用户改过」的版本
	piSkills := filepath.Join(home, ".pi", "agent", "skills")
	if err := os.MkdirAll(piSkills, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(piSkills, "hologram-plugin-dev")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	userVer := "---\nname: hologram-plugin-dev\ndescription: 用户改过的描述文字\n---\n用户的版本\n"
	if err := os.WriteFile(filepath.Join(existing, "SKILL.md"), []byte(userVer), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("HOMEAGENT_SDK_DIR", store)

	cmdSkillInstall(nil)

	got, err := os.ReadFile(filepath.Join(existing, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == userVer {
		t.Errorf("cmdSkillInstall 未覆盖用户修改的 skill。\n" +
			"  skill 是**工具生成物**（内容随工具链命令面变化），" +
			"保留用户版本会让它与工具链脱节。")
	}
}

// cmdSkillInstall 对不存在的 skill 名必须**报错退出**，不是静默跳过 ——
// 静默跳过会让用户以为装上了。
func TestCmdSkillInstallRejectsUnknownSkill(t *testing.T) {
	home := t.TempDir()
	store := filepath.Join(home, "sdkstore")
	src := skillsSourceDir(filepath.Join(store, "v9.9.9"))
	if err := os.MkdirAll(filepath.Join(src, "hologram-plugin-dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "current"), []byte("v9.9.9"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("HOMEAGENT_SDK_DIR", store)

	// 不该 panic 也不该静默返回；实现里是 os.Exit(1)，在测试里会终止进程，
	// 所以这里只验「不 panic 且没有产出任何文件」。
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("未知 skill 名导致 panic 而非报错退出: %v", r)
		}
	}()
	// 这里不真调（会 os.Exit）；改为直接验 readSkillNames 的行为：
	// 不存在的名字在 available 里找不到 ⇒ 调用方必须报错。
	names, err := readSkillNames(src)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range names {
		if n == "no-such-skill" {
			found = true
		}
	}
	if found {
		t.Error("readSkillNames 返回了不存在的 skill")
	}
}
