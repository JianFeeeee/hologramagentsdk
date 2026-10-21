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
//	    { "from": "assets", "exclude": ["assets/tmp"] },
//	    // ↓ 外部依赖（url 来源）：构建时下载 + sha256 强制校验，仓库里不留大文件
//	    { "name": "runtime", "url": "https://host/rt.tar.gz",
//	      "sha256": "…64 hex…", "os": "linux", "arch": "amd64",
//	      "format": "tar.gz", "strip_prefix": "package/" }
//	  ]
//	}
//
// url 来源的语义（缓存/平台过滤/构建期展开）全部在 rawfiles_fetch.go，
// 文件头写清了为什么这么拆（为什么下载不在本文件的 collectRawFiles 里做）。
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
//
// 两种来源，互斥：
//
//	from  工程内的本地路径（目录或单文件）——**默认来源**，旧写法逐字兼容
//	url   外部下载（可选的 format/strip_prefix 让归档在**构建期**展开）
//
// url 来源的全部 I/O 见 rawfiles_fetch.go 的文件头注释。
type RawFileDecl struct {
	From     string   `json:"from"`
	Exclude  []string `json:"exclude,omitempty"`
	Optional bool     `json:"optional,omitempty"`

	// ↓ 外部依赖（url 来源）

	// Name 是包内路径（相对 rawfile/ 根），如 "node/bin/node"、"pi"。
	// 包内条目为 rawfile/<name>[ /<归档内相对路径> ]。
	Name string `json:"name,omitempty"`
	// URL 是资源来源。与 from 互斥。
	URL string `json:"url,omitempty"`
	// SHA256 是**下载到的原始字节**的校验和（64 位 hex，大小写不敏感）。
	// 必填：不校验的下载等于信任传输通道，且出问题毫无症状。
	SHA256 string `json:"sha256,omitempty"`
	// OS/Arch 按**构建目标**过滤（空 = 不限；都有则必须同时匹配）。
	OS   string `json:"os,omitempty"`
	Arch string `json:"arch,omitempty"`
	// Format 是归档格式：zip / tar.gz / 空（按 url 后缀推断）。
	// 非归档（单文件）留空即可。
	Format string `json:"format,omitempty"`
	// StripPrefix 是展开时剥掉的前缀（如 npm 包的 "package/"）。
	StripPrefix string `json:"strip_prefix,omitempty"`

	// ↓ 运行期记账（不来自 JSON），由 ensureRawFileDownloads 填。

	// materialized 是这条声明最终贡献的包内条目（归档展开后可能很多条）。
	materialized []rawEntry
	// skipped 报告本条因平台不匹配被跳过（不下载、不进包）。
	skipped bool
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

	// mode 非 0 时覆盖写入 .hmap 的权限位。
	//
	// 为什么需要它：下载到缓存的单文件权限由**缓存目录**决定（可能是 0600），
	// 而随包资源里最典型的是 node 可执行文件 —— 权限丢了就是"包里有文件
	// 但运行期起不来"。归档来源不需要它（展开时已按归档内 mode 落盘）。
	mode os.FileMode

	// origin 记录这条条目的**来源类别**，只用于撞名时报对文案。
	//
	// 为什么需要它：撞名（两条声明落到同一包内路径）有三种组合，而
	// "下一步怎么做"对三种组合并不相同：
	//
	//	url  + url      → 没有本地目录可删，该说"改名/删一条/分平台构建"
	//	local + url     → 删本地目录、或从 plg.json 去掉那条 url
	//	local + local   → 删其中一份声明
	//
	// 不给来源标识就只能用一勺烩文案，那必然对其中一种场景是错的 ——
	// 实测 url/url 撞名时报的是"要保留打开后的本地目录请去除 url 声明"，
	// 而该场景下根本没有本地目录，照做等于原地打转。
	//
	// 取值见 origin* 常量；空值视为 originLocal（兼容旧构造点）。
	origin rawOrigin
}

// rawOrigin 是 rawEntry 的来源类别。
type rawOrigin string

const (
	// originLocal：工程内本地路径（rawfile/ 默认目录、from 声明）。
	originLocal rawOrigin = "local"
	// originURL：url 来源（下载到缓存、可展开归档）。
	originURL rawOrigin = "url"
	// originSkill：skill/ 目录（与 rawfile 平级，不参与 rawfile 撞名）。
	originSkill rawOrigin = "skill"
)

// label 返回给人看的来源名（用于报错文案）。
func (o rawOrigin) label() string {
	if o == originURL {
		return "url 声明"
	}
	if o == originSkill {
		return "skill 目录"
	}
	return "本地声明"
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

	// ★ 入口先把 root 规范成**绝对路径**（并据此重算 cleanRoot）。
	//
	// 为什么必须在这一行上做，而不能在各个比较点上打补丁：
	// 逃脱检查原来是 `strings.HasPrefix(cleanAbs, cleanRoot+os.PathSeparator)`，
	// 而真实调用方传的 root 就是 "."（buildTarget 里 `collectRawFiles(".", ...)`）。
	// 于是 cleanRoot="."、cleanAbs="legacy.txt"，前缀比较是
	// `HasPrefix("legacy.txt", "./")` —— **恒假** ⇒ 任何 `from` 都判成"逃出
	// 工程目录"。后果是 `"rawfiles": ["legacy.txt"]`（文档里最朴素的写法）
	// 在真实 CLI 上 100% 失败，而直接给 collectRawFiles 传 t.TempDir() 的
	// 单测全部绿 —— 这就是它对单测隐形的原因。
	//
	// 绝对 root 还有个附带好处：所有 rawEntry.src 都变成绝对路径，
	// 与 cwd 解耦 ⇒ 构建中途 chdir 也不会读错文件。zip 条目名仍由
	// filepath.Rel 派生（见下），不会把绝对路径带进包里。
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("解析工程根路径失败（root=%q）: %w", root, err)
	}
	root = absRoot

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
				src:    p,
				zip:    filepath.ToSlash(filepath.Join(RawFilesPrefix, rel)),
				origin: originLocal,
			})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("展开 %s/ 失败: %w", RawFilesDir, err)
		}
	}

	// ② 显式声明
	for _, d := range decls {
		// url 来源：全部 I/O 已在 ensureRawFileDownloads 里做完，这里只读账。
		//
		// ★ 这里**绝不联网、绝不下载**。原因见 rawfiles_fetch.go 文件头：
		// collectRawFiles 被 bundle 模式每平台调一次、单目标模式每 target 调一次，
		// 在其中下载会让同一资源被拉 N 次，且让"打包到一半才联网失败"
		// 留下半成品 .hmap。
		if strings.TrimSpace(d.URL) != "" {
			if d.skipped {
				continue // 平台不匹配：已在预取阶段报过"跳过"
			}
			if len(d.materialized) == 0 {
				// 没经过预取就调 collectRawFiles（如测试直接调用）—— 必须
				// 报错而不是静默少资源：少资源的包运行期才炸，且症状指向别处。
				return nil, fmt.Errorf(
					"rawfile %q 的 url 来源尚未预取（应先调 ensureRawFileDownloads）—— "+
						"否则包内会缺这份资源而构建照旧\"成功\"", d.Name)
			}
			out = append(out, d.materialized...)
			continue
		}

		from := strings.TrimSpace(d.From)
		if from == "" {
			return nil, fmt.Errorf("rawfiles 有条目缺 from（也没有 url）")
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
		// cleanRoot 必须基于**已绝对化的 root** 重算（不是 entry 时那个原值）。
		// 这就是上面那句 absRoot 的意义：只给 abs 加绝对化而留下相对 cleanRoot
		// 等于白改 —— 前缀比较仍会拿 "legacy.txt" 去比 "./"。
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
				src:    abs,
				zip:    filepath.ToSlash(filepath.Join(RawFilesPrefix, base)),
				origin: originLocal,
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
				src:    p,
				zip:    filepath.ToSlash(filepath.Join(RawFilesPrefix, base, rel)),
				origin: originLocal,
			})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("展开 rawfiles 目录 %s 失败: %w", from, err)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].zip < out[j].zip })
	// 撞名检测：两条来源落到同一包内路径 —— 不报的话"我改的没生效"极难查
	//
	// ★ 列表用**最终列表**而不是 map 去重：map 只能发现重复，无法从 []rawEntry
	// 里删掉一项；而"内容相同则去重"需要真的删（否则包里会出现两条同名条目，
	// 内核解包后哪个在上面取决于顺序）。
	kept := make([]rawEntry, 0, len(out))
	seen := map[string]*rawEntry{}
	for i := range out {
		e := &out[i]
		prev, dup := seen[e.zip]
		if !dup {
			seen[e.zip] = e
			kept = append(kept, *e)
			continue
		}
		// 内容**完全相同**时去重：典型场景是"工作区里还留着展开好的本地
		// rawfile/node/，同时 plg.json 又声明了 url 来源"。此时两份字节一致，
		// 报错只会逼人删目录（而那目录可能是本机跑 e2e 需要的）。
		// 内容不同则必须报错：静默取一边会让"我改的没生效"不可查。
		same, cmpErr := sameFileContent(prev.src, e.src)
		if cmpErr == nil && same {
			// ★ 日志也要分清来源：url+url 时不能说"本地与 url"。
			// 一句话说错来源，读者就会去不存在的地方找东西。
			fmt.Printf("  rawfile 去重 %s（%s 与 %s 内容一致，取其一）\n",
				e.zip, prev.origin.label(), e.origin.label())
			continue
		}
		// ★ 撞名冲突：文案必须对**当前来源组合**成立。
		// 这条是纪律，不是措辞偏好：url+url 场景下没有本地目录可删，
		// 沿用 local+url 那句"去掉本地目录"会把人引向不存在的对象。
		return nil, conflictError(e.zip, prev, e)
	}
	return kept, nil
}

// conflictError 为撞名冲突生成**对当前来源组合成立**的报错。
//
// 三种组合的"下一步"确实不同，所以不能共用一句：
//
//	url  + url（sha 不同）→ url/url 专用：改名/删一条/按平台分构建
//	local + url           → 现有语义（删本地目录或去掉那条 url 声明）
//	local + local         → 删其中一份声明
func conflictError(zipName string, a, b *rawEntry) error {
	sameOriginURL := a.origin == originURL && b.origin == originURL
	if sameOriginURL {
		return fmt.Errorf(
			"rawfile 目标冲突: 两条 url 声明指向同一个包内名 %q（sha256 不同，来自 %s 与 %s）。"+
				"请**改名**（让两条声明的 name 不同）或**删掉其中一条**；"+
				"若确实要按平台分发不同内容，请用 os/arch 区分声明并用 --target 分平台构建"+
				"（bundle 包要求所有目标平台共用同一组条目，装不下两份同名不同内容的资源）",
			zipName, a.origin.label(), b.origin.label())
	}
	return fmt.Errorf(
		"rawfile 目标冲突: %q 同时来自 %s 与 %s 且内容不同 —— "+
			"删掉其中一份（要保留打开后的本地目录请从 plg.json 去掉对应 url 声明）",
		zipName, a.src, b.src)
}

// sameFileContent 比较两个文件内容是否逐字节相同（长度不同直接返回 false）。
func sameFileContent(a, b string) (bool, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fb.Close()
	sa, err := fa.Stat()
	if err != nil {
		return false, err
	}
	sb, err := fb.Stat()
	if err != nil {
		return false, err
	}
	if sa.Size() != sb.Size() {
		return false, nil
	}
	bufA := make([]byte, 64*1024)
	bufB := make([]byte, 64*1024)
	for {
		na, errA := io.ReadFull(fa, bufA)
		nb, errB := io.ReadFull(fb, bufB)
		if na != nb || string(bufA[:na]) != string(bufB[:nb]) {
			return false, nil
		}
		if errA == io.EOF || errA == io.ErrUnexpectedEOF {
			return true, nil
		}
		if errA != nil {
			return false, errA
		}
		if errB != nil && errB != io.EOF && errB != io.ErrUnexpectedEOF {
			return false, errB
		}
	}
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
		if e.mode != 0 {
			// 覆盖权限位：下载来的单文件统一 0755（暂不提供 mode 字段），
			// 否则缓存目录的 0600 会让随包 node 变成不可执行。
			hdr.SetMode(e.mode)
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
