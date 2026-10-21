package main

// rawfile 的**外部依赖**：url 下载 + sha256 强制校验 + 构建期展开。
//
// ## 为什么需要这一段
//
// `rawfile/` 里的东西**不进版本库**（见 rawfile/.gitignore 的理由：上百兆
// 二进制 + 每次升级一个巨型 diff）。代价是干净克隆拿不到资源，打出的包
// 缺 sidecar / 缺内嵌 node，而构建日志照旧写着"成功"。
//
// 解法不是把二进制塞进 git，而是让 plg.json **声明**它们从哪来：
//
//	"rawfiles": [
//	  { "name": "runtime/pi", "url": "https://host/pi-1.2.3.tar.gz",
//	    "sha256": "…64 hex…", "os": "linux", "arch": "amd64",
//	    "format": "tar.gz", "strip_prefix": "package/" }
//	]
//
// 构建时：下载 → 校验 sha256 → （归档则）展开到暂存目录 → 逐条目打进 .hmap。
// 包内路径仍是 `rawfile/<name>`（归档展开后是 `rawfile/<name>/<归档内相对路径>`），
// 所以**运行期读取代码零改动**，`{rawfile}` 的语义也一字不变。
//
// ## 为什么拆成"预取"与"收集"两段
//
// `collectRawFiles` 原来的契约是**纯本地遍历**：它只 `os.Stat` 磁盘，不联网。
// 保留这条契约很重要 —— 打包循环里（bundle 模式每平台一次、单目标模式每个
// target 一次）重跑它不会产生网络副作用，且它的单测不需要任何服务器。
//
// 于是新增 `ensureRawFileDownloads`（**只在 cmdBuild 开头调一次**）负责全部
// I/O：校验声明、按平台过滤、下载、验 sha、展开，把结果记账在 decl 上
// （`resolved` / `materialized` / `skipped`）。`collectRawFiles` 随后只读账。
//
// 反过来做（在 collectRawFiles 里下载）会带来三个具体问题：同一资源被下载
// N 次、单测被迫起 HTTP 服务、以及"打包到一半才联网失败"导致已写出的
// .hmap 是半成品。
//
// ## 缓存
//
// `${HOMEAGENT_RAWFILE_CACHE}` 或 `~/.homeagent/hmapdev/cache/rawfiles/`
// （与 `hmapdev sdk install` 的 store 同级，不新开概念）。
//
//   <cache>/<sha256>            下载到的原始字节，**文件名就是校验和**
//   <cache>/<sha256>-<key8>/    归档展开结果 + .ready 标记（key8 = format+strip 的摘要）
//
// 内容寻址带来三条性质，都是判据要的：
//
//  1. 命中判据是"文件存在**且**实算 sha256 等于文件名" —— 缓存被改/截断
//     不会被当成命中（只判 Stat 会让改过的缓存静默进包）。
//  2. 命中即返回 ⇒ 全程零网络 ⇒ 断网可重跑（口径 5）。
//  3. 展开结果也按 (sha, format, strip_prefix) 记账 ⇒ 重复构建不重复解压。
//
// ## 校验对象
//
// sha256 校验的是**下载到的原始归档字节**，不是展开后的内容。
// 展开是不可逆的（mode/symlink/顺序都可能丢），校验展开结果等于换了个
// 更弱且更难对上上游发布的判据。归档字节可被上游直接发布与核对。

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// rawFileCacheEnv 覆盖缓存根目录（测试用它把缓存指到 t.TempDir()）。
const rawFileCacheEnv = "HOMEAGENT_RAWFILE_CACHE"

// rawFileDownloadTimeout 是单个资源的下载上限。
//
// 5 分钟不是拍的：内嵌 node 单文件 122MB，慢链路（500KB/s）也要 4 分钟。
// 曾经用 30s（沿用 sdk install 的值）—— 那对 SDK 源码包够，对 122MB 二进制
// 必然超时，表现为"断网"这种完全错误的归因。
const rawFileDownloadTimeout = 5 * time.Minute

// rawFileCacheDir 返回缓存根目录。
func rawFileCacheDir() (string, error) {
	if v := strings.TrimSpace(os.Getenv(rawFileCacheEnv)); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// 没有 HOME 的极端环境（某些 CI 容器）：退到临时目录而不是失败。
		// 缓存本就可丢弃，失败在这里只会让"能构建的东西不能构建"。
		return filepath.Join(os.TempDir(), "homeagent-rawfile-cache"), nil
	}
	return filepath.Join(home, ".homeagent", "hmapdev", "cache", "rawfiles"), nil
}

// ---------------------------------------------------------------------------
// 声明校验
// ---------------------------------------------------------------------------

// validateRawFileDecls 做**静态**校验（不联网、不碰缓存）。
//
// 为什么要在下载之前单独做：配置错误（缺 sha256、字段混用）是最常见的一类，
// 而它们的正确报错时机是"编译之前、下载之前" —— 否则用户先等一次 122MB
// 下载或一次交叉编译，才看到"你少写了个字段"。
func validateRawFileDecls(decls []RawFileDecl) error {
	for i, d := range decls {
		where := fmt.Sprintf("rawfiles[%d]", i)
		hasFrom := strings.TrimSpace(d.From) != ""
		hasURL := strings.TrimSpace(d.URL) != ""
		switch {
		case hasFrom && hasURL:
			return fmt.Errorf("%s: from 与 url 互斥（一条件只能有一个来源）", where)
		case !hasFrom && !hasURL:
			return fmt.Errorf("%s: 既没有 from 也没有 url（不知道资源从哪来）", where)
		}
		if hasFrom {
			// 本地来源不允许挂"外部依赖"的字段：挂了也不会生效，
			// 而"我设了 os 却没生效"是最难查的那类缺陷。
			for k, v := range map[string]string{
				"name":         d.Name,
				"sha256":       d.SHA256,
				"os":           d.OS,
				"arch":         d.Arch,
				"format":       d.Format,
				"strip_prefix": d.StripPrefix,
			} {
				if strings.TrimSpace(v) != "" {
					return fmt.Errorf("%s: from 来源不支持 %s 字段（那是 url 来源的）", where, k)
				}
			}
			continue
		}
		// url 来源
		if err := checkRawFileName(d.Name); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if strings.TrimSpace(d.SHA256) == "" {
			// ★ 口径 2：缺 sha256 直接失败，不允许 warning 后继续。
			//
			// 为什么不能"没写就不校验"：不校验的下载是**信任传输通道**，
			// 一次中间人/一次上游覆盖就换掉了进包的可执行文件；而 SHA 缺失
			// 不会在运行期暴露成任何症状 —— 它只会让"包内容"悄悄变成别的东西。
			return fmt.Errorf(
				"%s（name=%q）: url 来源**必须**声明 sha256（64 位 hex）—— "+
					"不校验的下载等于信任传输通道，且出问题不会有任何症状", where, d.Name)
		}
		if _, err := normalizeSHA256(d.SHA256); err != nil {
			return fmt.Errorf("%s（name=%q）: %w", where, d.Name, err)
		}
		if d.Optional {
			// optional 的语义是"缺失就跳过"，而 url 声明的是**必需的随包资源**。
			// 两者一起写 = 配错了也不报错 ⇒ 打出一个缺资源的包。宁可直接拒。
			return fmt.Errorf(
				"%s（name=%q）: url 来源不支持 optional —— "+
					"外部依赖是必需资源，缺失必须报错而不是静默跳过", where, d.Name)
		}
		f, err := effectiveFormat(d)
		if err != nil {
			return fmt.Errorf("%s（name=%q）: %w", where, d.Name, err)
		}
		if strings.TrimSpace(d.StripPrefix) != "" {
			if err := checkStripPrefix(d.StripPrefix); err != nil {
				return fmt.Errorf("%s（name=%q）: %w", where, d.Name, err)
			}
			if f == "" {
				return fmt.Errorf(
					"%s（name=%q）: strip_prefix 只对归档有效，但 format 为空且 url 也无法推断"+
						"（请在 url 用 .tar.gz/.tgz/.zip 结尾，或显式写 format）", where, d.Name)
			}
		}
	}
	return nil
}

// checkStripPrefix 校验 strip_prefix 自身。
//
// ★ 必须单独校验：`strip_prefix: ".."` 配条目 `../evil.txt` 时，
// 剥完得到 `evil.txt` —— 看似合法，实际已经把穿越段"消化"掉了，
// 而人读配置只会以为写错了目录。实测就是这个组合让穿越检查失效过一次。
func checkStripPrefix(p string) error {
	v := strings.TrimSpace(p)
	if strings.Contains(v, "\\") {
		return fmt.Errorf("strip_prefix 不接受反斜杠: %q", p)
	}
	if path.IsAbs(v) || strings.HasPrefix(v, "/") {
		return fmt.Errorf("strip_prefix 必须是相对路径，不能是绝对路径: %q", p)
	}
	for _, seg := range strings.Split(v, "/") {
		if seg == ".." {
			return fmt.Errorf("strip_prefix 不能含 ..: %q", p)
		}
	}
	return nil
}

// checkRawFileName 校验声明的包内名（相对 rawfile/ 根）。
//
// 为什么这么严：它会被拼成 `rawfile/<name>` 写进 .hmap，而 .hmap 由内核
// 解包到插件目录。`../` 或绝对路径在这里就等于**往插件目录外写文件**
// （与 sdk/rawfiles.go 里"必须拒绝路径穿越"是同一个面）。
func checkRawFileName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("url 来源必须声明 name（包内路径，如 \"node/bin/node\"）")
	}
	if strings.Contains(name, "\\") {
		return fmt.Errorf("name 不接受反斜杠（包内路径统一用 /）: %q", name)
	}
	if path.IsAbs(name) || strings.HasPrefix(name, "/") {
		return fmt.Errorf("name 必须是相对 rawfile/ 的路径，不能是绝对路径: %q", name)
	}
	if path.Clean(name) != name {
		return fmt.Errorf("name 必须是已规范化的相对路径（不要 ./, //, 结尾 /）: %q", name)
	}
	if name == RawFilesPrefix || strings.HasPrefix(name, RawFilesPrefix+"/") {
		return fmt.Errorf("name 不要带 %s/ 前缀（它本来就在 rawfile/ 下）: %q", RawFilesPrefix, name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("name 含非法路径段 %q: %q", seg, name)
		}
	}
	return nil
}

// normalizeSHA256 接受大小写 hex，返回小写 64 位形式。
func normalizeSHA256(s string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	if len(v) != 64 {
		return "", fmt.Errorf("sha256 必须是 64 位 hex（实际 %d 位）", len(v))
	}
	if _, err := hex.DecodeString(v); err != nil {
		return "", fmt.Errorf("sha256 不是合法 hex: %v", err)
	}
	return v, nil
}

// effectiveFormat 返回规范化后的归档格式（"" = 单文件）。
func effectiveFormat(d RawFileDecl) (string, error) {
	f := strings.ToLower(strings.TrimSpace(d.Format))
	if f == "" {
		f = inferFormatFromURL(d.URL)
	}
	switch f {
	case "":
		return "", nil
	case "zip":
		return "zip", nil
	case "tar.gz", "tgz", "targz":
		return "tar.gz", nil
	}
	return "", fmt.Errorf("不支持的 format %q（可选 zip / tar.gz，或留空按 url 后缀推断）", d.Format)
}

// inferFormatFromURL 按 url 路径后缀推断格式（忽略 query/fragment）。
func inferFormatFromURL(rawurl string) string {
	u := rawurl
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	lower := strings.ToLower(u)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return "tar.gz"
	case strings.HasSuffix(lower, ".zip"):
		return "zip"
	}
	return ""
}

// ---------------------------------------------------------------------------
// 平台过滤
// ---------------------------------------------------------------------------

// effectiveTargets 返回"本次构建真正要产出的平台"。
//
// ★ 关键：过滤器按**构建目标**判，不是按宿主。
// 交叉编译 linux/amd64 插件时，配一台 darwin 宿主不该决定打进哪个 node。
func effectiveTargets(plg *PlgConfig, bundle bool, targets []string) []string {
	if plg != nil && plg.IsLua() {
		// Lua 插件不交叉编译，产物就是本机可跑的解释执行包。
		return []string{runtime.GOOS + "/" + runtime.GOARCH}
	}
	if bundle || len(targets) == 0 {
		out := make([]string, 0, len(allBundleTargets))
		for _, bt := range allBundleTargets {
			out = append(out, bt.target)
		}
		return out
	}
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, normalizeTarget(t))
	}
	return out
}

// normalizeTarget 把 "" / "native" 展开成宿主平台。
func normalizeTarget(t string) string {
	t = strings.TrimSpace(t)
	if t == "" || t == "native" {
		return runtime.GOOS + "/" + runtime.GOARCH
	}
	return t
}

// declMatchesTargets 报告某条声明是否适用于本次构建的任一平台。
//
// 空 os/arch = 不限。都声明则必须**同时**匹配（AND），与 Go 的 build tag 同义。
func declMatchesTargets(d RawFileDecl, targets []string) bool {
	wantOS := strings.ToLower(strings.TrimSpace(d.OS))
	wantArch := strings.ToLower(strings.TrimSpace(d.Arch))
	if wantOS == "" && wantArch == "" {
		return true
	}
	if len(targets) == 0 {
		return false
	}
	for _, t := range targets {
		goos, goarch, _ := strings.Cut(normalizeTarget(t), "/")
		if wantOS != "" && !strings.EqualFold(wantOS, goos) {
			continue
		}
		if wantArch != "" && !strings.EqualFold(wantArch, goarch) {
			continue
		}
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 缓存 + 下载
// ---------------------------------------------------------------------------

// ensureCachedFile 保证 dst 处有一份**校验通过**的文件，返回是否走了网络。
//
// corrupted 报告"缓存存在但校验不符" —— 这个事实必须传到错误信息里：
// 断网 + 缓存损坏时，用户需要知道是"缓存被改过"，而不是"网断了"。
func ensureCachedFile(dst, url, wantSHA string) (downloaded, corrupted bool, err error) {
	if st, statErr := os.Stat(dst); statErr == nil && st.Mode().IsRegular() {
		got, hashErr := fileSHA256(dst)
		if hashErr == nil && got == wantSHA {
			return false, false, nil // 命中：零网络
		}
		// 不命中就删掉：留着一个内容与方法名（= sha）不符的文件，
		// 下次仍不命中，且占着空间让人以为缓存是好的。
		_ = os.Remove(dst)
		corrupted = true
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, corrupted, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".dl-*")
	if err != nil {
		return false, corrupted, err
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		_ = os.Remove(tmpPath) // rename 成功后这里是 no-op
	}()

	if err := downloadTo(url, tmp); err != nil {
		if corrupted {
			return false, true, fmt.Errorf(
				"本地缓存 %s 校验不符（sha256 不等于 %s，已删除），且重新下载失败: %w",
				dst, shortSHA(wantSHA), err)
		}
		return false, false, err
	}
	got, err := fileSHA256(tmpPath)
	if err != nil {
		return false, corrupted, err
	}
	if got != wantSHA {
		// ★ 口径 2：校验不符直接失败。绝不落进缓存、绝不进包。
		return false, corrupted, fmt.Errorf(
			"sha256 校验失败：期望 %s，实际 %s（%s）—— "+
				"要么上游替换了文件，要么 plg.json 的 sha256 写错了", wantSHA, got, url)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return false, corrupted, err
	}
	return true, corrupted, nil
}

// downloadTo 把 url 的字节写进已打开的 f。
func downloadTo(url string, f *os.File) error {
	client := &http.Client{Timeout: rawFileDownloadTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("下载 %s 失败: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载 %s 失败: HTTP %d", url, resp.StatusCode)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("下载 %s 中断: %w", url, err)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return nil
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// ---------------------------------------------------------------------------
// 预取：cmdBuild 开头调一次
// ---------------------------------------------------------------------------

// ensureRawFileDownloads 处理全部 url 声明：平台过滤 → 下载/校验/缓存 →
// （归档则）展开 → 记账到 decls[i].materialized。
//
// 幂等：第二次调用不会重复下载（缓存命中）也不会重复展开（.ready 标记）。
func ensureRawFileDownloads(decls []RawFileDecl, targets []string) error {
	if len(decls) == 0 {
		return nil
	}
	cacheRoot, err := rawFileCacheDir()
	if err != nil {
		return err
	}
	for i := range decls {
		d := &decls[i]
		if strings.TrimSpace(d.URL) == "" {
			continue
		}
		if !declMatchesTargets(*d, targets) {
			// ★ 口径 3：不匹配就**跳过且不下载**。
			// 这里必须真的不发请求 —— "先下再判断"会让 linux 构建也去拉
			// 一个 122MB 的 darwin node，而结果还被丢掉。
			d.skipped = true
			fmt.Printf("  rawfile 跳过 %s（os=%s arch=%s 不匹配本次目标 %s）\n",
				d.Name, orAny(d.OS), orAny(d.Arch), strings.Join(targets, ","))
			continue
		}
		sha, err := normalizeSHA256(d.SHA256)
		if err != nil {
			return fmt.Errorf("rawfile %s: %w", d.Name, err)
		}
		archivePath := filepath.Join(cacheRoot, sha)
		downloaded, corrupted, err := ensureCachedFile(archivePath, d.URL, sha)
		if err != nil {
			return fmt.Errorf("rawfile %s: %w", d.Name, err)
		}
		switch {
		case downloaded:
			fmt.Printf("  rawfile 下载 %s → 缓存 %s（%s）\n", d.URL, shortSHA(sha), d.Name)
		case corrupted:
			fmt.Printf("  rawfile 缓存曾损坏、已重新下载 %s\n", shortSHA(sha))
		default:
			fmt.Printf("  rawfile 缓存命中 %s（零网络）: %s\n", shortSHA(sha), d.Name)
		}

		format, err := effectiveFormat(*d)
		if err != nil {
			return fmt.Errorf("rawfile %s: %w", d.Name, err)
		}
		if format == "" {
			// 非归档：单文件按现状直接落地。
			//
			// 权限统一 0755：没有 mode 字段可声明（本版口径），而随包资源里
			// 最典型的就是 node 可执行文件 —— 用缓存目录的默认权限（0600）
			// 会让包里出现一个"存在但不可执行"的 node，症状是 agent_up 报
			// “找不到 node”而文件明明就在那里。
			d.materialized = []rawEntry{{
				src:  archivePath,
				zip:  filepath.ToSlash(path.Join(RawFilesPrefix, d.Name)),
				mode: 0o755,
			}}
			continue
		}
		ents, err := materializeArchive(archivePath, sha, format, d.StripPrefix, d.Name, cacheRoot)
		if err != nil {
			return fmt.Errorf("rawfile %s: %w", d.Name, err)
		}
		d.materialized = ents
	}
	return nil
}

func orAny(s string) string {
	if strings.TrimSpace(s) == "" {
		return "*"
	}
	return s
}

// materializeArchive 把归档展开到缓存里的暂存目录，返回逐条目清单。
//
// 暂存目录名带上 (format, strip_prefix) 的摘要：同一份归档用不同
// strip_prefix 声明两次（不同 name）不会互相踩。
func materializeArchive(archivePath, sha, format, stripPrefix, name, cacheRoot string) ([]rawEntry, error) {
	key := shortSHA(sha256Hex(format + "\x00" + stripPrefix))
	dst := filepath.Join(cacheRoot, sha+"-"+key[:8])
	ready := filepath.Join(dst, ".ready")
	want := format + "\n" + strings.TrimSpace(stripPrefix) + "\n"
	if b, err := os.ReadFile(ready); err == nil && string(b) == want {
		return archiveEntries(dst, name)
	}
	// 展开到临时目录再整体 rename：中途失败不会留下"看起来可用"的半截目录，
	// 也不会让下一次构建把它当成已展开（幂等的前提）。
	if err := os.RemoveAll(dst); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(cacheRoot, ".x-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if err := extractArchiveTo(archivePath, format, stripPrefix, tmp); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(tmp, ".ready"), []byte(want), 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return nil, err
	}
	return archiveEntries(dst, name)
}

// archiveEntries 把展开目录摊成 rawEntry（zip 名为 rawfile/<name>/<rel>）。
func archiveEntries(dir, name string) ([]rawEntry, error) {
	var out []rawEntry
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, werr error) error {
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
		relSlash := filepath.ToSlash(rel)
		if relSlash == ".ready" {
			return nil
		}
		out = append(out, rawEntry{
			src: p,
			zip: filepath.ToSlash(path.Join(RawFilesPrefix, name, relSlash)),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].zip < out[j].zip })
	return out, nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// 归档展开（构建期；运行期仍是零解压）
// ---------------------------------------------------------------------------

// checkArchiveRelPath 校验归档条目/剥前缀后的相对路径。
//
// ★ 这是安全边界：条目名来自**下载来的归档**（可能被替换过），
// `../../plugin.bin` 这种条目能往插件目录外写任意文件。
func checkArchiveRelPath(rel string) error {
	if rel == "" {
		return fmt.Errorf("归档条目名为空")
	}
	if strings.Contains(rel, "\\") {
		return fmt.Errorf("归档条目名含反斜杠（不支持的 Windows 风格路径）: %q", rel)
	}
	if path.IsAbs(rel) || strings.HasPrefix(rel, "/") {
		return fmt.Errorf("归档条目是绝对路径（拒绝解到任意位置）: %q", rel)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return fmt.Errorf("归档条目含 ..（路径穿越，拒绝展开）: %q", rel)
		}
		if seg == "" || seg == "." {
			return fmt.Errorf("归档条目含非法路径段 %q: %q", seg, rel)
		}
	}
	return nil
}

// extractArchiveTo 展开归档到 dst。
//
// 规则：
//   - strip_prefix 剥完必须仍在 dst 内（含 `..` / 绝对路径直接报错）；
//   - 同一路径出现两次 → 报错（不静默取一边：后写覆盖会让"哪个版本进包"
//     取决于条目顺序，而条目顺序由上游打包器决定）；
//   - 目录/文件权限沿用归档内记录；symlink 校验目标在 dst 内后建为符号链接
//     （后续 os.Stat 会跟随它，最终打进 .hmap 的是目标内容 —— 与
//     "本地 rawfile/ 里有 symlink 时 build 的行为"一致）。
func extractArchiveTo(archivePath, format, stripPrefix, dst string) error {
	// ★ 防御性重校（静态校验也会校一次，但展开路径上再校一次成本为零）：
	// `strip_prefix: ".."` 配条目 `../evil.txt` 时剥完得到 `evil.txt`，
	// 穿越段被"消化"掉 —— 只看剥完的结果是查不出来的。
	if strings.TrimSpace(stripPrefix) != "" {
		if err := checkStripPrefix(stripPrefix); err != nil {
			return err
		}
	}
	sink := &archSink{dst: dst, seen: map[string]bool{}, prefix: normalizePrefix(stripPrefix)}
	switch format {
	case "zip":
		return walkZip(archivePath, sink)
	case "tar.gz":
		return walkTarGz(archivePath, sink)
	}
	return fmt.Errorf("不支持的归档格式 %q", format)
}

func normalizePrefix(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "./")
	p = strings.Trim(p, "/")
	return p
}

type archSink struct {
	dst    string
	prefix string
	seen   map[string]bool
	links  []linkEntry
}

type linkEntry struct {
	rel    string
	target string
}

// emit 处理一个归档条目。
func (s *archSink) emit(rawName string, mode os.FileMode, dir bool, linkTarget string, r io.Reader) error {
	rel, err := s.strip(rawName)
	if err != nil {
		return err
	}
	if rel == "" {
		return nil // 前缀目录本身
	}
	if s.seen[rel] {
		return fmt.Errorf("归档内重复条目: %q（同路径出现多次，不允许静默取一边）", rel)
	}
	s.seen[rel] = true

	if dir {
		perm := mode.Perm()
		if perm == 0 {
			perm = 0o755
		}
		return os.MkdirAll(filepath.Join(s.dst, filepath.FromSlash(rel)), perm)
	}
	if linkTarget != "" || mode&os.ModeSymlink != 0 {
		s.links = append(s.links, linkEntry{rel: rel, target: linkTarget})
		return nil
	}
	dstPath := filepath.Join(s.dst, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return err
	}
	perm := mode.Perm()
	if perm == 0 {
		perm = 0o644
	}
	f, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// 归档里的 mode 只有 perm 位有意义（umask 也会影响 OpenFile 结果），
	// 显式 Chmod 一次，保证 0755 的可执行位不被 umask 抹掉。
	return os.Chmod(dstPath, perm)
}

// strip 应用 strip_prefix 并校验结果路径。
func (s *archSink) strip(rawName string) (string, error) {
	rel := strings.TrimPrefix(rawName, "./")
	if s.prefix != "" {
		if rel == s.prefix {
			return "", nil
		}
		if !strings.HasPrefix(rel, s.prefix+"/") {
			// 前缀之外的文件：报错而不是跳过。
			// 跳过会让"展开成功但少了文件"变成静默事实，而 strip_prefix
			// 写错正是最可能的原因（如多写了一个目录层级）。
			return "", fmt.Errorf(
				"归档条目 %q 不在 strip_prefix %q 之下 —— "+
					"要么 strip_prefix 写错，要么归档结构变了", rawName, s.prefix)
		}
		rel = rel[len(s.prefix)+1:]
	}
	rel = path.Clean(rel)
	if rel == "." {
		return "", nil
	}
	if err := checkArchiveRelPath(rel); err != nil {
		return "", err
	}
	return rel, nil
}

// finish 建 symlink（目标必须在 dst 内）。
func (s *archSink) finish() error {
	for _, l := range s.links {
		target := l.target
		if target == "" {
			return fmt.Errorf("归档内 symlink %q 没有目标", l.rel)
		}
		if path.IsAbs(target) || strings.HasPrefix(target, "/") {
			return fmt.Errorf("归档内 symlink %q 指向绝对路径 %q（拒绝）", l.rel, target)
		}
		resolved := path.Clean(path.Join(path.Dir(l.rel), target))
		if err := checkArchiveRelPath(resolved); err != nil {
			return fmt.Errorf("归档内 symlink %q → %q 逃出展开目录: %w", l.rel, target, err)
		}
		dstPath := filepath.Join(s.dst, filepath.FromSlash(l.rel))
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			return err
		}
		if _, err := os.Lstat(dstPath); err == nil {
			return fmt.Errorf("归档内 symlink %q 与已有条目同路径", l.rel)
		}
		if err := os.Symlink(target, dstPath); err != nil {
			return err
		}
	}
	return nil
}

func walkZip(archivePath string, s *archSink) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("打开 zip 失败（format 写错或文件损坏）: %w", err)
	}
	defer zr.Close()
	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		mode := zf.Mode()
		linkTarget := ""
		var body io.Reader = rc
		if mode&os.ModeSymlink != 0 {
			b, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return err
			}
			linkTarget = string(b)
			body = nil
		}
		err = s.emit(zf.Name, mode, zf.FileInfo().IsDir(), linkTarget, body)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return s.finish()
}

func walkTarGz(archivePath string, s *archSink) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gzr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("不是合法的 tar.gz（format 写错或文件损坏）: %w", err)
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		mode := hdr.FileInfo().Mode()
		var linkTarget string
		var body io.Reader = tr
		switch hdr.Typeflag {
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			continue
		case tar.TypeSymlink, tar.TypeLink:
			linkTarget = hdr.Linkname
			body = nil
		case tar.TypeDir:
			body = nil
		}
		if err := s.emit(hdr.Name, mode, hdr.Typeflag == tar.TypeDir, linkTarget, body); err != nil {
			return err
		}
	}
	return s.finish()
}
