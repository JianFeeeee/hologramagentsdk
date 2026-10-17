package main

// 工作目录映射：工程分支 ↔ 目录 的**单一事实源**。
//
// 用户要求（2026-10-14）：
//
//	「即使是同一个仓库，只要是不同的分支都拉到 hash-name 下使用 hash 区分，
//	  插件通过哈希表映射工程分支与文件夹」
//
// 所以目录形态是 program/<hash>-<name>，其中：
//
//	hash = H(项目 + 分支)  → ★ 唯一性来源，保证同仓库不同分支**必然**是不同目录
//	name = 分支名派生      → ★ 可读性来源，让人一眼看出这是哪条线
//
// ## 为什么 hash 必须含分支，且必须是唯一性来源
//
// 一个仓库可以并行开多条分支（这正是本插件的目标）。若只按仓库名建目录，
// 两条分支就会**共用同一个工作树** —— 那是灾难性的：agent A 的未提交改动会出现在
// agent B 的 `git status` 里，提交互相污染，且"独立 clone"的前提（设计 §4.3）不成立。
//
// 所以：目录唯一性由 hash 承担，**不依赖 name**。
// name 只做可读性，冲突了也无所谓（hash 已经不同）。
//
// ## 为什么不能只留 hash
//
// 清理孤儿目录、人工排查、审计时，纯 hash 目录名毫无信息量。所以保留 name 后缀。
//
// ## 映射是**双向**的
//
//   branchKey(project, branch) → dirName(project, branch) → 目录路径
//   目录路径 → 可从目录名解析回 (hash, name)，再经台账 branch.Workdir 反查工程分支
//
// ★ 绝对不要在这之外另算目录名：那会出现两处真相，改一处就漂移。

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// identityHashLen 是目录名里 hash 的长度（8 位 16 进制，碰撞概率对本场景足够低）。
const identityHashLen = 8

// branchKey 是「工程 + 分支」的规范化键 —— 映射的输入。
//
// 用 "\x00" 分隔，避免 (a|b) 与 (ab|"") 之类的拼接歧义。
func branchKey(project, branch string) string {
	return strings.TrimSpace(project) + "\x00" + strings.TrimSpace(branch)
}

// shortHash 是 FNV-1a 32 位哈希的 16 进制串（可重算 ⇒ 可清理孤儿）。
func shortHash(s string) string {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return fmt.Sprintf("%0*x", identityHashLen, h)
}

// dirName 返回某个工程分支对应的目录名（不含父目录）。
//
// 形态：<hash>-<name>。
func dirName(project, branch string) string {
	h := shortHash(branchKey(project, branch))
	name := safeNamePattern.ReplaceAllString(strings.TrimSpace(branch), "_")
	name = strings.Trim(name, "_")
	if name == "" {
		name = "branch"
	}
	// 目录名总长有限制；分支名过长时截断（hash 已保证唯一，截断不损正确性）。
	if len(name) > 48 {
		name = name[:48]
	}
	return h + "-" + name
}

// branchDir 返回某个工程分支的工作目录绝对路径。
//
// programRoot 是 program/ 的父目录（插件数据目录）。
func branchDir(programRoot, project, branch string) string {
	return filepath.Join(programRoot, dirName(project, branch))
}

// programRoot 返回插件的数据目录下的 program/ 根。
func (p *Plugin) programRoot() string {
	return filepath.Join(filepath.Dir(p.path), "program")
}

// dirNamePattern 用于从目录名解析回 (hash, name)。
var dirNamePattern = regexp.MustCompile(`^([0-9a-f]{8})-(.+)$`)

// parseDirName 解析目录名。ok=false 表示不像本插件生成的目录。
func parseDirName(dir string) (hash, name string, ok bool) {
	m := dirNamePattern.FindStringSubmatch(dir)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// resolveBranchDir 是**反查**：给定目录名，在台账里找到它属于哪个工程分支。
//
// 用途：清理孤儿目录、审计"这个目录是谁的"。
func (p *Plugin) resolveBranchDir(dirName string) (*Branch, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range p.ledger.Branches {
		if filepath.Base(b.Workdir) == dirName {
			return b, true
		}
	}
	return nil, false
}

// listOrphanDirs 列出 program/ 下**台账里没有记录**的目录（孤儿）。
//
// 为什么需要它：目录会因崩溃/手工操作残留，而"合入即删"是本插件的纪律；
// 没有孤儿检测，残留会永久堆积、且下次同名分支可能与旧残留混淆。
func (p *Plugin) listOrphanDirs() ([]string, error) {
	root := p.programRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	known := map[string]bool{}
	p.mu.Lock()
	for _, b := range p.ledger.Branches {
		if b.Workdir != "" {
			known[filepath.Base(b.Workdir)] = true
		}
	}
	p.mu.Unlock()

	var orphans []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if _, _, ok := parseDirName(name); !ok {
			continue // 不是本插件生成的目录，不碰
		}
		if !known[name] {
			orphans = append(orphans, name)
		}
	}
	return orphans, nil
}
