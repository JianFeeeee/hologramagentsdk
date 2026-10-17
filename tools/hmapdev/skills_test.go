package main

// 随包 skill 打包的判据。
//
// 约定（与 rawfile/ 平行但**意图不同**）：
//
//	skill/<name>/SKILL.md   每个子目录一个 skill，登记给 skillmgr
//	skill/<name>/skill.json
//
// 为什么与 rawfile 分开：内核需要知道**哪些是要注册的**。
// 靠子路径名猜（rawfile/skills/）是隐式约定，本仓为此吃过亏。

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkSkillProject 造一个带 skill/ 的工程。
func mkSkillProject(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// ★★ 判据：skill/ 下的每个目录被打包，**保留路径**。
func TestCollectSkillFiles(t *testing.T) {
	root := mkSkillProject(t, map[string]string{
		"skill/alpha/SKILL.md":          "# alpha",
		"skill/alpha/skill.json":        `{"name":"alpha"}`,
		"skill/beta/SKILL.md":           "# beta",
		"skill/beta/references/note.md": "# ref", // 子目录也要保留
		"rawfile/some/data.bin":         "x",     // 不该被 skill 收集器收
	})
	ents, err := collectSkillFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range ents {
		got[e.zip] = true
	}
	for _, want := range []string{
		"skill/alpha/SKILL.md",
		"skill/alpha/skill.json",
		"skill/beta/SKILL.md",
		"skill/beta/references/note.md",
	} {
		if !got[want] {
			t.Errorf("缺条目 %s（实际 %v）", want, keys(got))
		}
	}
	// 不得收 rawfile 下的东西
	for z := range got {
		if !strings.HasPrefix(z, "skill/") {
			t.Errorf("★ 收到了 skill/ 之外的条目: %s", z)
		}
	}
}

// ★ 判据：**没有 skill/ 目录是常态**，不报错、返回空。
func TestNoSkillDirIsFine(t *testing.T) {
	root := mkSkillProject(t, map[string]string{"rawfile/x.txt": "x"})
	ents, err := collectSkillFiles(root)
	if err != nil {
		t.Fatalf("无 skill/ 不应报错: %v", err)
	}
	if len(ents) != 0 {
		t.Errorf("应返回空，实际 %d", len(ents))
	}
}

// ★★ 判据：skill/ 下放**文件**要报错（几乎肯定是放错了）。
//
// 静默跳过会让"我加的 skill 没生效"变成无提示的谜题。
func TestFileInSkillDirIsRejected(t *testing.T) {
	root := mkSkillProject(t, map[string]string{
		"skill/stray.md":    "# 我放错了",
		"skill/ok/SKILL.md": "# ok",
	})
	_, err := collectSkillFiles(root)
	if err == nil {
		t.Fatal("★ skill/ 下的文件应报错（不静默跳过）")
	}
	if !strings.Contains(err.Error(), "rawfile/") {
		t.Errorf("错误应指出正确去处（rawfile/）: %v", err)
	}
}

// ★ 判据：脚手架不被打包。
func TestSkillScaffoldExcluded(t *testing.T) {
	root := mkSkillProject(t, map[string]string{
		"skill/.gitignore":     "x",
		"skill/alpha/.gitkeep": "",
		"skill/alpha/SKILL.md": "# a",
	})
	ents, err := collectSkillFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if IsRawFileScaffold(e.zip) {
			t.Errorf("★ 脚手架被打包: %s", e.zip)
		}
	}
}

// ★★ 判据：真正的 zip 里条目名保留完整路径。
func TestSkillEntriesInZip(t *testing.T) {
	root := mkSkillProject(t, map[string]string{
		"skill/commondev-discipline/SKILL.md": "# 纪律",
	})
	ents, err := collectSkillFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "t.hmap")
	f, _ := os.Create(out)
	w := zip.NewWriter(f)
	if err := writeRawEntries(w, ents); err != nil {
		t.Fatal(err)
	}
	w.Close()
	f.Close()

	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	found := false
	for _, zf := range zr.File {
		if zf.Name == "skill/commondev-discipline/SKILL.md" {
			found = true
		}
	}
	if !found {
		t.Error("★ zip 里未保留 skill 的目录结构")
	}
}

// ★ 判据：摘要报的是 **skill 个数**（不是文件数）。
func TestSkillSummaryCountsSkillsNotFiles(t *testing.T) {
	root := mkSkillProject(t, map[string]string{
		"skill/a/SKILL.md":        "x",
		"skill/a/references/r.md": "x",
		"skill/b/SKILL.md":        "x",
	})
	ents, _ := collectSkillFiles(root)
	sum := skillFilesSummary(ents)
	if !strings.Contains(sum, "2 个 skill") {
		t.Errorf("摘要应按 skill 数计（2 个），实际: %s", sum)
	}
	if !strings.Contains(sum, "3 个文件") {
		t.Errorf("摘要应同时给出文件数: %s", sum)
	}
}

func keys(m map[string]bool) []string {
	var o []string
	for k := range m {
		o = append(o, k)
	}
	return o
}
