package main

// 构建期检查：同一 stage 被重复注册。
//
// ## 为什么在构建期（而不是运行期）
//
// 工具链生成的 proc_main 把 stage handler 存在 map 里（stage 名为键）：
//
//	stageHandlers[string(stage)] = handler   // 后者静默覆盖前者
//
// 于是写成两行注册就会丢功能 —— 更糟的是，如果其中一个是**权限门**，
// 覆盖就意味着**权限被静默关掉**（2026-10-18 真实事故：那几版 QQ 插件里
// 任何人发消息都能调任何工具）。
//
// 这类错误编译器不报、运行时不报错、内核日志看着正常（`2 handlers [qq qq]`），
// 只有**源码静态检查**能发现。
//
// ## 为什么告警在构建期而非运行期
//
// 用户原话：「本身工具链这个编译器警告就是给开发者看的」。
// 运行期日志是给**运维/用户**看的，开发者看不到；而构建是开发者必经的一步，
// 告警打在构建输出里他必然看见。
//
// ## 为什么是告警而不是错误
//
// 存量插件里重复注册可能是本意（权限门 + 埋点分两处写）。
// 直接 fail 会让它们升 SDK 后无法构建。告警足够达到"开发者能发现"的目的，
// 想消除告警就把多个功能合并进同一个 handler。

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// stageRegRe 抓 `<任意>.RegisterStage(sdk.StageXxx`,  也容忍不带 sdk. 前缀的写法。
var stageRegRe = regexp.MustCompile(`\.RegisterStage\(\s*(?:sdk\.)?(Stage[A-Za-z]+)`)

// duplicateStageRegs 扫描目录下的 .go 源码（排除测试与生成文件），
// 返回 "Stage名 -> 出现次数" 中次数 > 1 的项。
//
// 只看 RegisterStage 的第一个参数是否为**同一个 stage 常量**。
// 不做语义分析（不需要）：同一 stage 注册两次就是这个 bug 的充要形态。
func duplicateStageRegs(dir string) (map[string]int, error) {
	counts := map[string]int{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		// 排除测试（它们会故意造重复来验证判据）与工具链生成文件
		// （生成文件里的注册是模板展开，不是作者写的）。
		if strings.HasSuffix(e.Name(), "_test.go") || strings.HasPrefix(e.Name(), "z_proc") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		// ★ 必须**逐行跳注释**：注释掉的示例注册不计入。
		//
		// 这个检查器第一版直接对全文跑正则，把插件里“当初写错了、现在
		// 保留在注释里当教训”的那几行也统计进去，于是修好之后照样报
		// “注册了 3 次”（实测）。而“注释里有、代码里没有”这个陷阱
		// 在本项目已经绊倒过好几次 —— 这次轮到检查器自己。
		inBlock := false
		for _, ln := range strings.Split(string(b), "\n") {
			code := strings.TrimSpace(ln)
			// 块注释（/* ... */）可能跨行，逐行维护状态
			if inBlock {
				if i := strings.Index(code, "*/"); i >= 0 {
					code = strings.TrimSpace(code[i+2:])
					inBlock = false
				} else {
					continue
				}
			}
			if strings.HasPrefix(code, "//") || strings.HasPrefix(code, "*") {
				continue
			}
			if strings.HasPrefix(code, "/*") {
				if i := strings.Index(code, "*/"); i >= 0 {
					code = strings.TrimSpace(code[i+2:])
				} else {
					inBlock = true
					continue
				}
			}
			// 行尾注释：只取 // 之前的部分（字符串里的 // 罕见，且
			// 这里关心的只是 RegisterStage 调用，不会出现在字符串里）。
			if i := strings.Index(code, "//"); i >= 0 {
				code = strings.TrimSpace(code[:i])
			}
			for _, m := range stageRegRe.FindAllStringSubmatch(code, -1) {
				counts[m[1]]++
			}
		}
	}
	dups := map[string]int{}
	for k, v := range counts {
		if v > 1 {
			dups[k] = v
		}
	}
	return dups, nil
}

// warnDuplicateStageRegs 打印构建期告警（不中断构建）。
//
// 返回是否有重复（供调用方决定是否在汇总里体现）。
func warnDuplicateStageRegs(dir string) bool {
	dups, err := duplicateStageRegs(dir)
	if err != nil || len(dups) == 0 {
		return false
	}
	names := make([]string, 0, len(dups))
	for k := range dups {
		names = append(names, k)
	}
	sort.Strings(names)

	fmt.Printf("\n")
	for _, n := range names {
		fmt.Printf("  ⚠️  警告: stage %s 注册了 %d 次\n", n, dups[n])
	}
	fmt.Printf("      插件侧 stageHandlers 是 map（stage 名为键）⇒ 只有**最后**一次注册生效，\n")
	fmt.Printf("      前面的会被静默覆盖。若其中一个是权限门，覆盖 = 权限被关掉。\n")
	fmt.Printf("      建议：把多个功能合并进同一个 handler（减少跨进程 stage 往返）。\n\n")
	return true
}
