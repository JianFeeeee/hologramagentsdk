package main

// 随包 skill 的打包（`skill/` 目录）。
//
// ## 为什么与 `rawfile/` 分开，而不是放进 rawfile/
//
// 两者**意图不同**：
//
//	rawfile/  任意随包资源（node 二进制、pi 依赖、配置模板…）——内核只是解开
//	skill/    要**登记给 skillmgr** 的纯 SKILL 条目 —— 内核安装后会移交注册
//
// 若把 skill 放在 rawfile/ 下（如 `rawfile/skills/x/SKILL.md`），内核就必须
// 靠**子路径名**去猜哪些是要注册的 —— 那是隐式约定，正是本仓反复吃过亏的
// 模式（例：config.db 因"默认 0644"这个隐式假设而长期明文）。
// 显式目录让意图在**结构**里可见。
//
// ## 解包形态
//
// 与 rawfile 一样**保留路径**（不拍平）：
//
//	<插件目录>/skill/<name>/SKILL.md
//
// 内核侧（pluginmgr）扫 `<插件目录>/skill/*` 并把每个目录发
// skill_detected 事件，由 skillmgr 接管注册（见
// internal/plugins/pluginmgr/skill_handout.go）。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SkillDir 是工程根下存放随包 skill 的目录（与 rawfile/ 并列）。
const SkillDir = "skill"

// SkillZipPrefix 是解包后这些条目的统一前缀。
//
// 与 SkillDir 同值 —— 保留这个独立常量是为了让"打包目录名"与
// "包内前缀"可以分别演进（rawfile 那边是两个同名常量，同样的理由）。
const SkillZipPrefix = "skill"

// collectSkillFiles 收集 `skill/` 目录下的条目。
//
// 与 collectRawFiles 的差别：
//   - **只收一层**：skill 是「每个子目录一个 skill」，嵌套不再是 skill。
//     递归收会让 `skill/a/.git/SKILL.md` 这种被当成 skill 发出去。
//   - **只收目录**：单文件 skill 不在本约定内（skillmgr 支持，
//     但随包分发统一用目录形态，避免两种形态都要判）。
//   - 仍然排除脚手架（.gitkeep/.gitignore）。
func collectSkillFiles(root string) ([]rawEntry, error) {
	dir := filepath.Join(root, SkillDir)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		// 没有 skill/ 是**常态**（绝大多数插件不带），不是错误
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读取 %s/ 失败: %w", SkillDir, err)
	}

	var out []rawEntry
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue // 脚手架/隐藏项
		}
		if !e.IsDir() {
			// 明确拒绝而不是静默跳过：放在 skill/ 下的文件几乎肯定是放错了
			//（要么该是 skill/<name>/SKILL.md，要么该放 rawfile/）。
			return nil, fmt.Errorf(
				"%s/%s 是文件；%s/ 下只接受**目录**（每个目录一个 skill，形如 %s/<name>/SKILL.md）"+
					"；普通资源请放 rawfile/",
				SkillDir, name, SkillDir, SkillDir)
		}
		sub := filepath.Join(dir, name)
		// 每个 skill 目录下的文件**递归保留结构**
		// （有的 skill 带 references/、assets/ 等子目录）。
		err := filepath.Walk(sub, func(p string, fi os.FileInfo, werr error) error {
			if werr != nil {
				return werr
			}
			if fi.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			if IsRawFileScaffold(rel) {
				return nil
			}
			out = append(out, rawEntry{
				src:    p,
				zip:    filepath.ToSlash(filepath.Join(SkillZipPrefix, rel)),
				origin: originSkill,
			})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("展开 %s/%s 失败: %w", SkillDir, name, err)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].zip < out[j].zip })
	return out, nil
}

// skillFilesSummary 给构建输出一行摘要（0 个时不打印）。
func skillFilesSummary(ents []rawEntry) string {
	if len(ents) == 0 {
		return ""
	}
	// 数 skill 个数（一层目录名去重）而不只是文件数 —— 后者对用户无意义
	names := map[string]bool{}
	for _, e := range ents {
		rest := strings.TrimPrefix(e.zip, SkillZipPrefix+"/")
		if i := strings.Index(rest, "/"); i > 0 {
			names[rest[:i]] = true
		}
	}
	var list []string
	for n := range names {
		list = append(list, n)
	}
	sort.Strings(list)
	return fmt.Sprintf("%d 个 skill（%s，共 %d 个文件）",
		len(list), strings.Join(list, ", "), len(ents))
}

// hasSkillDir 判断工程根是否有**至少一个真实 skill**（供打包期校验用）。
//
// 为什么不用简单的 os.Stat(SkillDir)：空目录、只有 .gitkeep 的脚手架目录
// 都会让它返回 true，而那种情况下 agent 一样拿不到任何说明 ⇒ 校验形同虚设。
// 这里复用 collectSkillFiles 的实际收集结果，与打包时的判定**同源**，
// 避免"校验说有、打包说没有"的分叉。
func hasSkillDir(root string) bool {
	files, err := collectSkillFiles(root)
	if err != nil {
		return false
	}
	// 收集到的条目里必须真的有 SKILL.md 才算一个可用 skill：
	// 只有目录没有说明文件的条目，注册到 skillmgr 也是空壳。
	for _, f := range files {
		if strings.HasSuffix(f.zip, "SKILL.md") {
			return true
		}
	}
	return false
}
