package main

// rawfile：把**非代码资源**打进 .hmap，解包后供插件在运行期读取。
//
// ## 为什么需要
//
// 在它之前 `hmapdev build` 只收 `plugin.bin` + `plugin.json`（+README/LICENSE），
// **任何资源都得靠部署脚本手工拷** —— 那让"一个 .hmap 就是完整插件"这个
// 前提失效：白板机器上装完直接缺文件。
//
// 典型需求：随包分发的 node 与 pi、各 CLI 后端的 sidecar 脚本与 driver.json、
// 模板、prompt 文本、字典、默认配置、wasm、字体……
//
// ## ★ 规则（用户拍板 2026-10-15）
//
//	1. 工程根下的 `rawfile/` 目录 **默认全部打包**（不需要在 plg.json 里声明）。
//	2. 其他位置的资源，用户在 plg.json 的 `rawfiles` 数组里**显式列出**，一同打包。
//	3. 解包后**一律落在 `<插件目录>/rawfile/` 下**：
//	     · `rawfile/` 目录里的内容 → `<插件目录>/rawfile/<原名>`
//	     · 外部声明的路径         → `<插件目录>/rawfile/<声明名>`
//
// 即 `rawfile/` 是**唯一入口**，插件只需认一个目录。
//
// ## plg.json 形状
//
//	{
//	  "rawfiles": [
//	    "config/defaults.json",          // 字符串：源路径，落到 rawfile/defaults.json
//	    { "from": "assets", "exclude": ["assets/tmp"] }
//	  ]
//	}
//
// ## 为什么统一落到 rawfile/ 下
//
// 若允许任意目标路径（比如把 node 直接放到 `runtime/node/bin/node`），
// 就会出现两类路径（工程路径 vs 包内路径）互相穿模，且**与插件自己写的
// 运行期文件混在一起**——升级时无法区分"包里带的"与"运行期生成的"。
// 单一入口让"包内的"与"运行期的"天然分离：包内只读，运行期在别处。

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RawFilesDir 是默认打包目录（工程根下）。
const RawFilesDir = "rawfile"

// RawFilesPrefix 是解包后这些资源的统一前缀。
const RawFilesPrefix = "rawfile"

// RawFileDecl 是一条 rawfiles 声明（支持字符串与对象两种写法）。
type RawFileDecl struct {
	From     string   `json:"from"`
	Exclude  []string `json:"exclude,omitempty"`
	Optional bool     `json:"optional,omitempty"`
}

// UnmarshalJSON 同时接受 `"path"` 与 `{"from": "path"}` 两种写法。
//
// 为何两种都收：只要一个路径时写字符串最省事（`"config/x.json"`），
// 需要 exclude/optional 时才用对象。强迫所有人写对象是没必要的仪式。
func (d *RawFileDecl) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, `"`) {
		var only string
		if err := json.Unmarshal(b, &only); err != nil {
			return err
		}
		d.From = only
		return nil
	}
	type alias RawFileDecl
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*d = RawFileDecl(a)
	return nil
}

type rawEntry struct {
	src string // 磁盘路径
	zip string // zip 内条目名（保留路径）
}

// collectRawFiles 收集要打包的资源。
//
// 两部分合起来：
//
//	① 工程根的 `rawfile/` 目录（默认全收）
//	② plg.json 里 rawfiles 声明的外部路径
//
// 输出按 zip 条目名排序（**确定性**：同样输入产出同样字节，
// 否则每次构建的 .hmap 都不同，校验和与 diff 全无意义）。
func collectRawFiles(root string, decls []RawFileDecl) ([]rawEntry, error) {
	var out []rawEntry

	// ① 默认目录
	defDir := filepath.Join(root, RawFilesDir)
	if fi, err := os.Stat(defDir); err == nil && fi.IsDir() {
		err := filepath.Walk(defDir, func(p string, fi os.FileInfo, werr error) error {
			if werr != nil {
				return werr
			}
			if fi.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(defDir, p)
			if err != nil {
				return err
			}
			// ★ 排除脚手架文件：`.gitkeep`/`.gitignore` 是**目录占位与版本控制**
			// 用的，不是资源。不打进包的原因有两个：
			//   ① 它们是工程内部约定，对插件运行期毫无意义；
			//   ② 它们是**噪声** —— 一个空工程会打出"已打包 2 个 rawfile"，
			//      让人误以为里面有资源。
			// （实测发现：init 出的新工程第一次 build 就把这两个文件打进去了。）
			if IsRawFileScaffold(rel) {
				return nil
			}
			out = append(out, rawEntry{
				src: p,
				zip: filepath.ToSlash(filepath.Join(RawFilesPrefix, rel)),
			})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("展开 %s/ 失败: %w", RawFilesDir, err)
		}
	}

	// ② 显式声明
	for _, d := range decls {
		from := strings.TrimSpace(d.From)
		if from == "" {
			return nil, fmt.Errorf("rawfiles 有条目缺 from")
		}
		// ★ from 的语义：**相对 plg.json 所在目录（工程根）**。
		//
		// 绝对路径**不接受** —— 不是因为技术上做不到，而是它会引入一个
		// 无法判定合法性的输入：`/etc/passwd` 与 `/home/me/proj/x` 在语法上
		// 没有区别，只能靠"是否在工程内"事后判断，而那个判断依赖 cwd
		// （构建时 cwd 不保证是工程根）⇒ 同一个 plg.json 在不同目录下构建
		// 结果不同，**构建不可复现**。
		//
		// 所以：拒绝绝对路径，只接受相对路径。这样 Clean 后前缀比较就是完备的
		// （实测发现：初版允许绝对路径，`"a/../../../etc/passwd"` 能绕出去）。
		if filepath.IsAbs(from) {
			return nil, fmt.Errorf(
				"rawfiles 不接受绝对路径: %q（请写成相对工程根的路径）", from)
		}
		abs := filepath.Join(root, from)
		cleanRoot := filepath.Clean(root)
		cleanAbs := filepath.Clean(abs)
		if cleanAbs != cleanRoot && !strings.HasPrefix(cleanAbs, cleanRoot+string(os.PathSeparator)) {
			return nil, fmt.Errorf(
				"rawfiles 路径逃出工程目录: %q（资源必须放在工程内）", from)
		}
		info, err := os.Stat(abs)
		if err != nil {
			if d.Optional {
				fmt.Printf("  warn: rawfile 可选源缺失，跳过: %s\n", from)
				continue
			}
			return nil, fmt.Errorf("rawfiles 源不存在: %s（要允许缺失请加 optional）", from)
		}
		base := filepath.Base(filepath.Clean(from))
		if !info.IsDir() {
			// 显式声明也排除脚手架（同一套规则，避免"两条来源行为不一致"）
			if IsRawFileScaffold(from) {
				continue
			}
			out = append(out, rawEntry{
				src: abs,
				zip: filepath.ToSlash(filepath.Join(RawFilesPrefix, base)),
			})
			continue
		}
		err = filepath.Walk(abs, func(p string, fi os.FileInfo, werr error) error {
			if werr != nil {
				return werr
			}
			if fi.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(abs, p)
			if err != nil {
				return err
			}
			relSlash := filepath.ToSlash(rel)
			if IsRawFileScaffold(relSlash) {
				return nil
			}
			for _, ex := range d.Exclude {
				ex = filepath.ToSlash(strings.TrimSpace(ex))
				if ex != "" && strings.HasPrefix(relSlash, ex) {
					return nil
				}
			}
			out = append(out, rawEntry{
				src: p,
				zip: filepath.ToSlash(filepath.Join(RawFilesPrefix, base, rel)),
			})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("展开 rawfiles 目录 %s 失败: %w", from, err)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].zip < out[j].zip })
	// 撞名检测：两条来源落到同一包内路径 —— 不报的话"我改的没生效"极难查
	seen := map[string]string{}
	for _, e := range out {
		if prev, dup := seen[e.zip]; dup {
			return nil, fmt.Errorf("rawfile 目标冲突: %q 同时来自 %s 与 %s", e.zip, prev, e.src)
		}
		seen[e.zip] = e.src
	}
	return out, nil
}

// writeRawEntries 把资源写进 zip（**保留路径**）。
//
// ★ 与既有 createHmap 的区别：那个用 `filepath.Base(path)` 当条目名
// （只适合单文件），目录进去会全部拍平撞名。rawfile 必须保留结构，
// 否则 `rawfile/main/foo.json` 会变成 `foo.json` 落在插件目录根上。
func writeRawEntries(w *zip.Writer, entries []rawEntry) error {
	for _, e := range entries {
		info, err := os.Stat(e.src)
		if err != nil {
			return fmt.Errorf("rawfile 源消失: %s: %w", e.src, err)
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Method = zip.Deflate
		hdr.Name = e.zip // ← 完整路径，不是 Base
		writer, err := w.CreateHeader(hdr)
		if err != nil {
			return err
		}
		src, err := os.Open(e.src)
		if err != nil {
			return err
		}
		if _, err := io.Copy(writer, src); err != nil {
			src.Close()
			return err
		}
		src.Close()
	}
	return nil
}

// rawFilesSummary 给构建输出一行摘要。
func rawFilesSummary(entries []rawEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var total int64
	for _, e := range entries {
		if fi, err := os.Stat(e.src); err == nil {
			total += fi.Size()
		}
	}
	return fmt.Sprintf("%d 个 rawfile（%.1f MiB）", len(entries), float64(total)/1048576)
}

// IsRawFileScaffold 判断某个**相对路径**是否是版本控制/目录占位用的脚手架文件。
//
// 这类文件随工程存在，但不是随包资源：
//   - `.gitkeep`  空目录占位
//   - `.gitignore` rawfile/ 自带的忽略规则
//   - `.DS_Store`  macOS Finder 的目录元数据（常被误提交）
//
// 排除它们既是"别把工程内部约定发给运行期"，也是**去噪**：
// 不排的话一个空工程会报"已打包 2 个 rawfile"，误导开发者以为包里有东西。
func IsRawFileScaffold(rel string) bool {
	base := filepath.Base(filepath.ToSlash(rel))
	switch base {
	case ".gitkeep", ".gitignore", ".DS_Store":
		return true
	}
	// 任意层级下的这些名字都不算资源
	return false
}
