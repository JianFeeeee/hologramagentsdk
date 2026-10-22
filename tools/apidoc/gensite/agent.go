package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 给 agent 用的入口。
//
// 为什么需要：文档站是给**人**看的（HTML + 主题 + JS 搜索），但越来越多读者是
// agent —— 它们要的是「一次拿到结构化事实」，而不是渲染后的页面。让 agent 去
// 爬 HTML 既浪费 token（主题样板占大头）又容易漏内容。
//
// 因此额外产出三样东西：
//
//	/llms.txt             站点的**目录**：每个页面一行，带 URL 与一句话说明
//	/llms-full.txt        全部文档**正文**（Markdown）拼成一份，可一次读完
//	/<page>.md            每个页面的 Markdown 原文（含生成的 API 页）
//	/<page>.json          机器可读版（API 页有结构化符号）
//
// 约定沿用 llms.txt 社区规范（Jeremy Howard 提出）：llms.txt 是给「读目录」
// 用的精简索引，llms-full.txt 是给「一次读全」用的大文件。
const llmsHeader = `# HologramAgent 插件 SDK

> 用 Go 或 Lua 为 HologramAgent 编写插件。插件跑在独立进程里，通过公开 SDK 与内核通信：
> 注册工具供模型调用、挂阶段钩子干预流程、读写三层记忆、注册输入输出通道、订阅事件。
>
> SDK 以 MIT 发布（插件可闭源、可商用，无需回馈）。内核本身是 AGPL-3.0-only。
>
> 本文件是给 agent 的入口。下列每个链接都是**纯 Markdown 正文**，可直接读，
> 不含 HTML 样板；也可以直接取 %s 一次读完全部文档。

`

// writeAgentEntrypoints 产出 llms.txt 与 llms-full.txt，并把每个页面同时写成
// `.md`（Markdown 原文）。返回写出的页面数。
//
// 注意：mkdocs 只会把 `.md` 渲染成 HTML，不会把它们复制到站点产物里。
// 所以这里除了写 docs/，构建后还要把 Markdown 副本搬进 site_build/
// （见 build.sh 的 copy_agent_files）。
func writeAgentEntrypoints(docsDir, siteDir string) (int, error) {
	type entry struct {
		rel   string // 相对 docs/ 的路径，如 api/tools.md
		title string
		desc  string
	}
	var entries []entry

	err := filepath.Walk(docsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		if filepath.Base(path) == "README.md" {
			return nil
		}
		rel, _ := filepath.Rel(docsDir, path)
		rel = filepath.ToSlash(rel)

		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		title, desc := firstHeadingAndDesc(string(body))
		entries = append(entries, entry{rel: rel, title: title, desc: desc})
		return nil
	})
	if err != nil {
		return 0, err
	}

	// 排序：首页最前，其余按路径。
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].rel == "index.md" {
			return true
		}
		if entries[j].rel == "index.md" {
			return false
		}
		return entries[i].rel < entries[j].rel
	})

	// 站点基址：默认是自托管域名（106 上的部署）。
	// GitHub Pages 构建时由 SITE_BASE_URL 覆盖成 https://<owner>.github.io/<repo> ——
	// 否则 llms.txt 里的绝对链接会把读者从 Pages 带去自托管站，跨站且版本可能不一致。
	base := strings.TrimRight(os.Getenv("SITE_BASE_URL"), "/")
	if base == "" {
		base = "https://sdk.hologramagent.jianfgit.xyz"
	}
	var idx strings.Builder
	fmt.Fprintf(&idx, llmsHeader, base+"/llms-full.txt")
	for _, e := range entries {
		// URL 就是 .md 的落地路径（构建后把 docs/**/*.md 复制进站点产物）。
		// 不要把 index.md 改成 index/ —— 那样指向的是 HTML 页而不是 Markdown 源。
		mdURL := base + "/" + e.rel
		fmt.Fprintf(&idx, "- [%s](%s)", e.title, mdURL)
		if e.desc != "" {
			fmt.Fprintf(&idx, ": %s", e.desc)
		}
		idx.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(docsDir, "llms.txt"), []byte(idx.String()), 0o644); err != nil {
		return 0, err
	}

	var full strings.Builder
	fmt.Fprintf(&full, "# HologramAgent 插件 SDK — 完整文档\n\n")
	full.WriteString("（本文件由 tools/apidoc/gensite 从 docs/ 汇总生成，供 agent 一次读取。）\n\n")
	full.WriteString("---\n\n")
	for _, e := range entries {
		body, err := os.ReadFile(filepath.Join(docsDir, e.rel))
		if err != nil {
			continue
		}
		fmt.Fprintf(&full, "\n\n## <%s>\n\n", e.rel)
		full.Write(body)
		full.WriteString("\n")
	}
	fullPath := filepath.Join(docsDir, "llms-full.txt")
	if err := os.WriteFile(fullPath, []byte(full.String()), 0o644); err != nil {
		return 0, err
	}

	// 把 llms.txt / llms-full.txt 也复制进站点产物（mkdocs 不搬运非 md 页面）。
	// 各页面的 .md 副本由 build.sh 统一复制——那时 docs/ 已经定稿。
	if siteDir != "" {
		if err := os.MkdirAll(siteDir, 0o755); err == nil {
			_ = copyFile(filepath.Join(docsDir, "llms.txt"), filepath.Join(siteDir, "llms.txt"))
			_ = copyFile(fullPath, filepath.Join(siteDir, "llms-full.txt"))
		}
	}
	return len(entries), nil
}

// firstHeadingAndDesc 取首个 `# 标题` 与紧随其后的第一段（作一句话说明）。
func firstHeadingAndDesc(body string) (title, desc string) {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "# ") && title == "" {
			title = strings.TrimSpace(strings.TrimPrefix(l, "# "))
			// 往下找第一段非空、非标题、非注释、非命令的文本。
			for j := i + 1; j < len(lines); j++ {
				t := strings.TrimSpace(lines[j])
				if t == "" || strings.HasPrefix(t, "#") ||
					strings.HasPrefix(t, "<!--") || strings.HasPrefix(t, "```") ||
					strings.HasPrefix(t, "!!!") || strings.HasPrefix(t, "|") {
					continue
				}
				// 去掉行内标记，截断到一句话。
				d := strings.NewReplacer("**", "", "`", "", "\\", "").Replace(t)
				if idx := strings.IndexAny(d, "。."); idx > 0 {
					d = d[:idx]
				}
				return title, d
			}
		}
	}
	return title, ""
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
