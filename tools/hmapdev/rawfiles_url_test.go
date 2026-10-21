package main

// rawfile 外部依赖（url 来源）的判据。
//
// ## 为什么这一批判据值得写这么细
//
// 这个特性把"构建期从互联网取一段字节，然后原封不动打进发布包"引入了
// 工具链。它同时踩三条红线：
//
//	① 供应链：不校验的下载 = 把发布包内容交给传输通道决定（sha256 强制）；
//	② 可复现：断网、缓存命中、缓存被改，三种状态下同一个 plg.json 必须
//	   产出同一个包（或明确失败），不能"有时有资源有时没有"；
//	③ 路径穿越：归档条目名来自下载物，`../../plugin.bin` 能往插件目录外写。
//
// 所以每条判据都配了**变异**：把实现改坏一处，判据必须变红。判据的注释里
// 写明"变异点"，便于以后有人重构时知道这条判据在守什么。
//
// 全部判据用 httptest 起本地服务器 + t.Setenv 把缓存指到 t.TempDir()：
// 不依赖外网、不在开发者机器上留缓存、可并发跑。

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

func shaOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// newTestCache 把缓存根指到临时目录，返回缓存根。
//
// ★ 必须隔离：不隔离的话判据会读到开发者本机 ~/.homeagent 里真实下载过的
// 资源，于是"断网未命中"这类判据会因为恰好命中而变绿 —— 那是最典型的
// "本地绿、CI 红"。
func newTestCache(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(rawFileCacheEnv, dir)
	return dir
}

// countingServer 起一个计数请求的 HTTP 服务器。
type countingServer struct {
	*httptest.Server
	hits int32
}

func newCountingServer(t *testing.T, body []byte) *countingServer {
	t.Helper()
	cs := &countingServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&cs.hits, 1)
		w.Write(body)
	}))
	t.Cleanup(cs.Close)
	return cs
}

func (c *countingServer) Hits() int { return int(atomic.LoadInt32(&c.hits)) }

// tarGzOf 构造一个 tar.gz。
func tarGzOf(t *testing.T, files map[string]struct {
	Body []byte
	Mode int64
}) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	// 固定顺序，保证同一输入同一字节（判据要比较 sha）
	sortStrings(names)
	for _, n := range names {
		f := files[n]
		hdr := &tar.Header{
			Name: n, Mode: f.Mode, Size: int64(len(f.Body)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.Body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// zipOf 构造一个 zip（可含重复条目与穿越条目，用于反向判据）。
func zipOf(t *testing.T, entries []struct {
	Name string
	Body []byte
	Mode os.FileMode
}) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.Name, Method: zip.Deflate}
		if e.Mode != 0 {
			hdr.SetMode(e.Mode)
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.Body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// packsToHmap 跑一遍 collectRawFiles + createHmapWithRaw，返回 .hmap 条目表。
func packsToHmap(t *testing.T, root string, decls []RawFileDecl) (*zip.ReadCloser, string) {
	t.Helper()
	ents, err := collectRawFiles(root, decls)
	if err != nil {
		t.Fatalf("collectRawFiles: %v", err)
	}
	out := filepath.Join(t.TempDir(), "x.hmap")
	createHmapWithRaw(out, nil, ents)
	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatalf("打开产出的 .hmap 失败: %v", err)
	}
	t.Cleanup(func() { zr.Close() })
	return zr, out
}

func hmapEntryNames(zr *zip.ReadCloser) []string {
	var out []string
	for _, f := range zr.File {
		out = append(out, f.Name)
	}
	sortStrings(out)
	return out
}

// ---------------------------------------------------------------------------
// 判据 1：单文件 url → rawfile/<name>，路径语义不变（口径 6）
// ---------------------------------------------------------------------------

func TestURLRawFilePacksUnderName(t *testing.T) {
	newTestCache(t)
	body := []byte("#!/bin/sh\necho node\n")
	srv := newCountingServer(t, body)
	root := mkProject(t)

	decls := []RawFileDecl{{
		Name: "node/bin/node", URL: srv.URL + "/node", SHA256: shaOf(body),
	}}
	if err := validateRawFileDecls(decls); err != nil {
		t.Fatal(err)
	}
	// 平台过滤：不声明 os/arch = 不限，任何目标都匹配
	if err := ensureRawFileDownloads(decls, []string{"linux/amd64"}); err != nil {
		t.Fatal(err)
	}
	zr, _ := packsToHmap(t, root, decls)
	names := hmapEntryNames(zr)
	want := "rawfile/node/bin/node"
	found := false
	for _, n := range names {
		if n == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("★ 包内没有 %s（实际 %v）—— {rawfile}/node/bin/node 语义会断", want, names)
	}
	// 内容逐字节相符
	for _, f := range zr.File {
		if f.Name != want {
			continue
		}
		rc, _ := f.Open()
		var buf bytes.Buffer
		buf.ReadFrom(rc)
		rc.Close()
		if !bytes.Equal(buf.Bytes(), body) {
			t.Errorf("包内内容与下载内容不一致: %q", buf.String())
		}
		// 单文件必须是 0755（随包 node 要能执行）
		if f.Mode().Perm()&0o111 == 0 {
			t.Errorf("★ 单文件条目丢了可执行位（mode=%v）—— 随包 node 会不可执行", f.Mode())
		}
	}
	// 变异点：把 zip 名写成 filepath.Base(d.Name) 则上面 want 找不到 → 红。
}

// ---------------------------------------------------------------------------
// 判据 2：缺 sha256 必须报错，且**不发请求**
// ---------------------------------------------------------------------------

func TestURLRawFileRequiresSha256(t *testing.T) {
	cacheDir := newTestCache(t)
	srv := newCountingServer(t, []byte("x"))

	decls := []RawFileDecl{{Name: "a.txt", URL: srv.URL + "/a"}}
	err := validateRawFileDecls(decls)
	if err == nil {
		t.Fatal("★ 缺 sha256 未被拒绝（口径 2：必须报错退出，不许 warning 后继续）")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("报错信息应点名 sha256，实际: %v", err)
	}
	// 校验必须发生在下载**之前**
	if srv.Hits() != 0 {
		t.Errorf("★ 缺 sha256 时报错前不该发请求（实际 %d 次）", srv.Hits())
	}
	// 缓存目录应仍是空的
	if ents, _ := os.ReadDir(cacheDir); len(ents) != 0 {
		t.Errorf("校验失败不该在缓存里留东西: %v", ents)
	}
	// 变异点：把缺 sha256 从 return err 改成 fmt.Printf("warn") + continue → 红。
}

// ---------------------------------------------------------------------------
// 判据 3：sha256 不符必须报错，且不落缓存
// ---------------------------------------------------------------------------

func TestURLRawFileShaMismatchFails(t *testing.T) {
	cacheDir := newTestCache(t)
	body := []byte("actual content")
	srv := newCountingServer(t, body)
	root := mkProject(t)

	wrong := strings.Repeat("a", 64)
	decls := []RawFileDecl{{Name: "a.bin", URL: srv.URL + "/a", SHA256: wrong}}
	if err := validateRawFileDecls(decls); err != nil {
		t.Fatalf("静态校验不该拒（sha 格式合法）: %v", err)
	}
	err := ensureRawFileDownloads(decls, []string{"linux/amd64"})
	if err == nil {
		t.Fatal("★ sha256 不符未被拒绝（口径 2：校验不符必须报错退出）")
	}
	if !strings.Contains(err.Error(), "sha256") || !strings.Contains(err.Error(), shortSHA(wrong)) {
		t.Errorf("报错应同时给出期望值与实际值，实际: %v", err)
	}
	// 缓存的命名就是 sha ⇒ 不符时不能有文件入位
	if _, statErr := os.Stat(filepath.Join(cacheDir, wrong)); statErr == nil {
		t.Error("★ 校验失败的文件被写进了缓存（它的名字就是错的 sha）")
	}
	// 也不该把失败的那条打进包
	if _, cerr := collectRawFiles(root, decls); cerr == nil {
		t.Error("★ 校验失败后 collectRawFiles 仍返回成功 —— 会把空/旧内容打进包")
	}
	// 变异点：去掉 ensureCachedFile 里的 got != wantSHA 判断 → 红。
}

// ---------------------------------------------------------------------------
// 判据 4：缓存命中 → 零网络；断网仍可构建（口径 4、5）
// ---------------------------------------------------------------------------

func TestURLRawFileCacheHitIsZeroNetwork(t *testing.T) {
	newTestCache(t)
	body := []byte("cached bytes")
	srv := newCountingServer(t, body)
	root := mkProject(t)

	mk := func() []RawFileDecl {
		return []RawFileDecl{{Name: "x/y.bin", URL: srv.URL + "/f", SHA256: shaOf(body)}}
	}
	d1 := mk()
	if err := ensureRawFileDownloads(d1, []string{"linux/amd64"}); err != nil {
		t.Fatal(err)
	}
	if srv.Hits() != 1 {
		t.Fatalf("首次应下载 1 次，实际 %d", srv.Hits())
	}
	// 第二次：必须零网络
	d2 := mk()
	if err := ensureRawFileDownloads(d2, []string{"linux/amd64"}); err != nil {
		t.Fatal(err)
	}
	if srv.Hits() != 1 {
		t.Fatalf("★ 缓存命中仍发起了请求（hits=%d）—— 口径 4 要求重复构建不重复下载", srv.Hits())
	}
	// 断网（服务器关掉）再来一次：必须成功
	d3 := mk()
	srv.Close()
	if err := ensureRawFileDownloads(d3, []string{"linux/amd64"}); err != nil {
		t.Fatalf("★ 断网 + 缓存命中时构建失败（口径 5 要求离线可重跑）: %v", err)
	}
	zr, _ := packsToHmap(t, root, d3)
	found := false
	for _, n := range hmapEntryNames(zr) {
		if n == "rawfile/x/y.bin" {
			found = true
		}
	}
	if !found {
		t.Errorf("离线构建的包里没有资源: %v", hmapEntryNames(zr))
	}
	// 变异点：命中判断里只看 os.Stat 不看实算 sha → 判据 5 会红（不是这条）。
	// 这条的变异：把命中分支改成"文件名存在就重新下载" → 上面 hits 断言红。
}

// ---------------------------------------------------------------------------
// 判据 5：缓存被改 → 联网时修好；断网时明确失败（不许把坏文件打进包）
// ---------------------------------------------------------------------------

func TestURLRawFileCacheCorruption(t *testing.T) {
	cacheDir := newTestCache(t)
	body := []byte("good bytes")
	srv := newCountingServer(t, body)
	root := mkProject(t)
	sha := shaOf(body)

	mk := func() []RawFileDecl {
		return []RawFileDecl{{Name: "c.bin", URL: srv.URL + "/c", SHA256: sha}}
	}
	d1 := mk()
	if err := ensureRawFileDownloads(d1, []string{"linux/amd64"}); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(cacheDir, sha)
	if err := os.WriteFile(cachePath, []byte("TAMPERED"), 0o644); err != nil {
		t.Fatal(err)
	}

	// (a) 联网：必须重新下载并恢复
	d2 := mk()
	if err := ensureRawFileDownloads(d2, []string{"linux/amd64"}); err != nil {
		t.Fatalf("缓存被改后应能重新下载: %v", err)
	}
	if srv.Hits() != 2 {
		t.Errorf("★ 缓存被改未被发现（hits=%d，期望 2）—— 变异：命中只判 Stat", srv.Hits())
	}
	got, _ := os.ReadFile(cachePath)
	if !bytes.Equal(got, body) {
		t.Errorf("缓存未被修复: %q", got)
	}

	// (b) 断网 + 缓存被改：必须**报错**，不能把坏内容打进包
	if err := os.WriteFile(cachePath, []byte("TAMPERED AGAIN"), 0o644); err != nil {
		t.Fatal(err)
	}
	d3 := mk()
	srv.Close()
	err := ensureRawFileDownloads(d3, []string{"linux/amd64"})
	if err == nil {
		t.Fatal("★ 缓存损坏 + 断网时构建成功了 —— 会把坏内容打进发布包")
	}
	// 报错必须说清是"缓存被改"而不是"网断了"，否则排查方向完全错
	if !strings.Contains(err.Error(), "缓存") {
		t.Errorf("报错应点名缓存损坏（否则用户会去查网络）: %v", err)
	}
	// 坏缓存已被删除，且不会进包
	if _, statErr := os.Stat(cachePath); statErr == nil {
		t.Error("★ 损坏的缓存文件没有被删除，下次仍会被误用")
	}
	if _, cerr := collectRawFiles(root, d3); cerr == nil {
		t.Error("★ 下载失败后 collectRawFiles 仍成功")
	}
	// 变异点：命中判断去掉实算 sha 比较 → (a) hits 仍为 1 → 红。
}

// ---------------------------------------------------------------------------
// 判据 6：平台不匹配 → 跳过且**不下载**（口径 3）
// ---------------------------------------------------------------------------

func TestURLRawFilePlatformMismatchSkipsDownload(t *testing.T) {
	newTestCache(t)
	srv := newCountingServer(t, []byte("darwin only"))
	root := mkProject(t)

	decls := []RawFileDecl{{
		Name: "node/bin/node", URL: srv.URL + "/node",
		SHA256: shaOf([]byte("darwin only")),
		OS:     "darwin", Arch: "arm64",
	}}
	if err := validateRawFileDecls(decls); err != nil {
		t.Fatal(err)
	}
	if err := ensureRawFileDownloads(decls, []string{"linux/amd64"}); err != nil {
		t.Fatalf("平台不匹配应跳过而不是报错: %v", err)
	}
	if srv.Hits() != 0 {
		t.Errorf("★ 平台不匹配仍发起了下载（hits=%d）—— 口径 3 要求跳过不下载", srv.Hits())
	}
	if !decls[0].skipped {
		t.Error("声明应标记为 skipped")
	}
	if len(decls[0].materialized) != 0 {
		t.Errorf("跳过的声明不该有包内条目: %v", decls[0].materialized)
	}
	// 不匹配的条目也不能让构建失败
	if _, err := collectRawFiles(root, decls); err != nil {
		t.Errorf("跳过的条目不该让收集失败: %v", err)
	}
	// 反之：声明 linux/amd64 时必须真的下载
	decls2 := []RawFileDecl{{
		Name: "n.bin", URL: srv.URL + "/n2",
		SHA256: shaOf([]byte("darwin only")), OS: "linux", Arch: "amd64",
	}}
	if err := ensureRawFileDownloads(decls2, []string{"linux/amd64"}); err != nil {
		t.Fatal(err)
	}
	if srv.Hits() != 1 {
		t.Errorf("匹配的平台必须下载（hits=%d）", srv.Hits())
	}
	// 变异点：删掉 ensureRawFileDownloads 里的 declMatchesTargets 分支 → hits=1 → 红。
}

// ---------------------------------------------------------------------------
// 判据 7：断网且未命中 → 明确失败（不许静默产出空资源包）
// ---------------------------------------------------------------------------

func TestURLRawFileOfflineWithoutCacheIsFatal(t *testing.T) {
	newTestCache(t)
	srv := newCountingServer(t, []byte("x"))
	url := srv.URL + "/gone"
	srv.Close() // 连不上
	root := mkProject(t)

	decls := []RawFileDecl{{Name: "a.bin", URL: url, SHA256: strings.Repeat("b", 64)}}
	err := ensureRawFileDownloads(decls, []string{"linux/amd64"})
	if err == nil {
		t.Fatal("★ 断网且未命中缓存时构建成功了 —— 会产出缺资源的包而日志写\"成功\"")
	}
	if !strings.Contains(err.Error(), url) {
		t.Errorf("报错应给出失败的 url: %v", err)
	}
	// collectRawFiles 也不能把"没预取"当没事
	if _, cerr := collectRawFiles(root, decls); cerr == nil {
		t.Error("★ 未预取的 url 声明被静默忽略")
	}
	// 变异点：把下载失败当作 optional 跳过 → 红（判据会拿到 err==nil）。
}

// ---------------------------------------------------------------------------
// 判据 8：归档在**构建期**展开，包内路径 rawfile/<name>/<rel>，权限沿用归档
// ---------------------------------------------------------------------------

func TestArchiveExpandsIntoHmap(t *testing.T) {
	newTestCache(t)
	arch := tarGzOf(t, map[string]struct {
		Body []byte
		Mode int64
	}{
		"package/runtime/pi/sidecar.mjs":    {Body: []byte("case \"interrupt\": {}"), Mode: 0o644},
		"package/runtime/zcode/sidecar.mjs": {Body: []byte("zcode"), Mode: 0o644},
		"package/node/bin/node":             {Body: []byte("#!/bin/sh\n"), Mode: 0o755},
	})
	srv := newCountingServer(t, arch)
	root := mkProject(t)

	decls := []RawFileDecl{{
		Name: "bundle", URL: srv.URL + "/b.tar.gz", SHA256: shaOf(arch),
		StripPrefix: "package/",
	}}
	if err := validateRawFileDecls(decls); err != nil {
		t.Fatal(err)
	}
	if err := ensureRawFileDownloads(decls, []string{"linux/amd64"}); err != nil {
		t.Fatal(err)
	}
	zr, _ := packsToHmap(t, root, decls)
	names := hmapEntryNames(zr)
	wantSet := map[string]bool{
		"rawfile/bundle/runtime/pi/sidecar.mjs":    true,
		"rawfile/bundle/runtime/zcode/sidecar.mjs": true,
		"rawfile/bundle/node/bin/node":             true,
	}
	for w := range wantSet {
		found := false
		for _, n := range names {
			if n == w {
				found = true
			}
		}
		if !found {
			t.Errorf("★ 包内缺 %s（实际 %v）—— {rawfile}/<name>/... 语义会断", w, names)
		}
	}
	// strip_prefix 之外的路径不该以原始形式出现
	for _, n := range names {
		if strings.Contains(n, "package/") {
			t.Errorf("★ strip_prefix 未生效，包内出现 %s", n)
		}
	}
	// 归档内 0755 的可执行位必须保留（node）
	for _, f := range zr.File {
		if f.Name == "rawfile/bundle/node/bin/node" {
			if f.Mode().Perm()&0o111 == 0 {
				t.Errorf("★ 归档内 0755 的可执行位丢了（mode=%v）", f.Mode())
			}
		}
		if f.Name == "rawfile/bundle/runtime/pi/sidecar.mjs" {
			if f.Mode().Perm()&0o111 != 0 {
				t.Errorf("归档内 0644 被抬高成可执行（mode=%v）", f.Mode())
			}
		}
	}
	// 包内不应出现归档本身（必须是逐条目展开）
	for _, n := range names {
		if strings.HasSuffix(n, ".tar.gz") {
			t.Errorf("★ 归档被原样打进包（%s）—— 运行期就得解压了", n)
		}
	}
	// 变异点：把 materializeArchive 换成"单个文件落地" → 上面 wantSet 全红。
}

// ---------------------------------------------------------------------------
// 判据 9：strip_prefix 剥完出现 .. 或绝对路径 → 报错（本次追加）
// ---------------------------------------------------------------------------

func TestArchiveStripPrefixTraversalRejected(t *testing.T) {
	newTestCache(t)
	cases := []struct {
		name  string
		entry string
		strip string
	}{
		{"剥前缀后是 ..", "package/../evil.txt", "package/"},
		{"剥前缀后含 ..", "package/a/../../evil.txt", "package/"},
		{"strip_prefix 为 ..", "../evil.txt", ".."},
		{"strip_prefix 为绝对路径", "/etc/passwd", "/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newTestCache(t)
			arch := tarGzOf(t, map[string]struct {
				Body []byte
				Mode int64
			}{tc.entry: {Body: []byte("evil"), Mode: 0o644}})
			srv := newCountingServer(t, arch)
			decls := []RawFileDecl{{
				Name: "x", URL: srv.URL + "/a.tar.gz", SHA256: shaOf(arch),
				StripPrefix: tc.strip,
			}}
			err := ensureRawFileDownloads(decls, []string{"linux/amd64"})
			if err == nil {
				t.Fatalf("★ 穿越路径 %q（strip=%q）未被拒绝 —— 能往插件目录外写文件", tc.entry, tc.strip)
			}
			// 报错理由必须是穿越/前缀，不能是"文件不存在"这类无关原因
			msg := err.Error()
			if !strings.Contains(msg, "..") && !strings.Contains(msg, "前缀") &&
				!strings.Contains(msg, "绝对路径") {
				t.Errorf("拒绝理由不对（应为穿越/前缀/绝对路径）: %v", err)
			}
			// 缓存里不能留下展开结果
			cacheDir, _ := rawFileCacheDir()
			ents, _ := os.ReadDir(cacheDir)
			for _, e := range ents {
				if e.IsDir() {
					t.Errorf("★ 拒绝后仍留下展开目录: %s", e.Name())
				}
			}
		})
	}
	// 变异点：去掉 checkArchiveRelPath 里的 ".." 判断（或 s.strip 里的校验调用）
	//         → 第一个子判据会拿到 err==nil → 红。
}

// ---------------------------------------------------------------------------
// 判据 10：归档内重复条目 → 报错（不许静默取一边）（本次追加）
// ---------------------------------------------------------------------------

func TestArchiveDuplicateEntriesRejected(t *testing.T) {
	newTestCache(t)
	// 同路径、不同内容：静默后写覆盖会让"哪个版本进包"取决于条目顺序，
	// 而条目顺序由上游客打包器决定 —— 那是不可复现的。
	arch := zipOf(t, []struct {
		Name string
		Body []byte
		Mode os.FileMode
	}{
		{"pkg/a.txt", []byte("FIRST"), 0o644},
		{"pkg/a.txt", []byte("SECOND"), 0o644},
	})
	srv := newCountingServer(t, arch)
	decls := []RawFileDecl{{
		Name: "p", URL: srv.URL + "/p.zip", SHA256: shaOf(arch), StripPrefix: "pkg/",
	}}
	err := ensureRawFileDownloads(decls, []string{"linux/amd64"})
	if err == nil {
		t.Fatal("★ 归档内重复条目未被拒绝 —— 后写覆盖会让包内容取决于条目顺序")
	}
	if !strings.Contains(err.Error(), "重复") {
		t.Errorf("报错应点名重复条目: %v", err)
	}
	// 也不能留下半截展开结果（否则下次构建可能把它当展开好的）
	cacheDir, _ := rawFileCacheDir()
	ents, _ := os.ReadDir(cacheDir)
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			t.Errorf("★ 失败后留下展开目录（会被下次当成已完成）: %s", e.Name())
		}
	}
	// 换一个"同路径同内容"也不能静默通过：那仍是诡异的归档
	arch2 := zipOf(t, []struct {
		Name string
		Body []byte
		Mode os.FileMode
	}{
		{"pkg/b.txt", []byte("SAME"), 0o644},
		{"pkg/b.txt", []byte("SAME"), 0o644},
	})
	srv2 := newCountingServer(t, arch2)
	d2 := []RawFileDecl{{
		Name: "q", URL: srv2.URL + "/q.zip", SHA256: shaOf(arch2), StripPrefix: "pkg/",
	}}
	if err := ensureRawFileDownloads(d2, []string{"linux/amd64"}); err == nil {
		t.Error("★ 同路径同内容的重复条目也应报错（归档本身异常）")
	}
	// 变异点：把 archSink.emit 里的 seen 检查改成"直接覆盖写" → err==nil → 红。
}

// ---------------------------------------------------------------------------
// 判据 11：name 路径穿越/非法形状 → 报错
// ---------------------------------------------------------------------------

func TestURLRawFileNameRejected(t *testing.T) {
	newTestCache(t)
	bad := []string{"../x", "/abs", "rawfile/x", "a//b", "a/./b", "a/", "x\\y", ""}
	for _, n := range bad {
		decls := []RawFileDecl{{
			Name: n, URL: "https://example.invalid/x", SHA256: strings.Repeat("c", 64),
		}}
		if err := validateRawFileDecls(decls); err == nil {
			t.Errorf("★ 非法 name %q 未被拒绝 —— 会往插件目录外/错误位置写", n)
		}
	}
	// 合法形状必须通过
	for _, n := range []string{"node/bin/node", "a.txt", "pi"} {
		decls := []RawFileDecl{{
			Name: n, URL: "https://example.invalid/x", SHA256: strings.Repeat("c", 64),
		}}
		if err := validateRawFileDecls(decls); err != nil {
			t.Errorf("合法 name %q 被拒: %v", n, err)
		}
	}
}

// ---------------------------------------------------------------------------
// 判据 12：字段混用/optional/format 的静态校验
// ---------------------------------------------------------------------------

func TestRawFileDeclStaticValidation(t *testing.T) {
	sha := strings.Repeat("d", 64)
	cases := []struct {
		name string
		d    RawFileDecl
		want string // 报错必须包含的子串
	}{
		{"from 与 url 互斥", RawFileDecl{From: "a", URL: "https://x/y", SHA256: sha}, "互斥"},
		{"两者都没", RawFileDecl{}, "既没有"},
		{"from 挂 sha256", RawFileDecl{From: "a", SHA256: sha}, "不支持 sha256"},
		{"from 挂 os", RawFileDecl{From: "a", OS: "linux"}, "不支持 os"},
		{"url 缺 name", RawFileDecl{URL: "https://x/y", SHA256: sha}, "name"},
		{"url 缺 sha256", RawFileDecl{Name: "a", URL: "https://x/y"}, "sha256"},
		{"sha256 不是 64 位", RawFileDecl{Name: "a", URL: "https://x/y", SHA256: "abc"}, "64 位"},
		{"sha256 非 hex", RawFileDecl{Name: "a", URL: "https://x/y", SHA256: strings.Repeat("z", 64)}, "hex"},
		{"optional 与 url 混用", RawFileDecl{Name: "a", URL: "https://x/y", SHA256: sha, Optional: true}, "optional"},
		{"format 不支持", RawFileDecl{Name: "a", URL: "https://x/y.tar.gz", SHA256: sha, Format: "rar"}, "format"},
		{"strip_prefix 但非归档", RawFileDecl{Name: "a", URL: "https://x/y", SHA256: sha, StripPrefix: "p"}, "strip_prefix"},
	}
	for _, tc := range cases {
		err := validateRawFileDecls([]RawFileDecl{tc.d})
		if err == nil {
			t.Errorf("★ %s 未被拒绝", tc.name)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.want)) {
			t.Errorf("%s 的报错应含 %q，实际: %v", tc.name, tc.want, err)
		}
	}
	// 旧写法（字符串）必须逐字兼容 —— 这是口径 1 的硬要求
	var decls []RawFileDecl
	if err := json.Unmarshal([]byte(`["assets/theme.css", {"from":"assets","exclude":["assets/tmp"]}]`), &decls); err != nil {
		t.Fatalf("旧写法解析失败: %v", err)
	}
	if decls[0].From != "assets/theme.css" || decls[0].URL != "" {
		t.Errorf("字符串写法解析错: %+v", decls[0])
	}
	if err := validateRawFileDecls(decls); err != nil {
		t.Errorf("旧写法不该被新校验拒绝: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 判据 13：url 来源与本地同名条目
// ---------------------------------------------------------------------------

func TestURLRawFileCollidesWithLocalEntry(t *testing.T) {
	newTestCache(t)
	root := mkProject(t)
	// mkProject 已有 rawfile/main/prompt.txt；这里造一个与 url name 相同的本地文件
	localPath := filepath.Join(root, "rawfile", "dup.txt")
	if err := os.WriteFile(localPath, []byte("LOCAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	remote := []byte("REMOTE")
	srv := newCountingServer(t, remote)

	// (a) 内容不同 → 必须报错
	d := []RawFileDecl{{Name: "dup.txt", URL: srv.URL + "/d", SHA256: shaOf(remote)}}
	if err := ensureRawFileDownloads(d, []string{"linux/amd64"}); err != nil {
		t.Fatal(err)
	}
	if _, err := collectRawFiles(root, d); err == nil {
		t.Error("★ 本地与 url 来源同名但内容不同，未被拒绝（会静默取一边）")
	}

	// (b) 内容相同 → 去重成功，条目只出现一次
	if err := os.WriteFile(localPath, remote, 0o644); err != nil {
		t.Fatal(err)
	}
	ents, err := collectRawFiles(root, d)
	if err != nil {
		t.Fatalf("内容相同时应去重而不是报错: %v", err)
	}
	n := 0
	for _, e := range ents {
		if e.zip == "rawfile/dup.txt" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("★ 去重后同名条目应只有 1 条，实际 %d", n)
	}
	// 变异点：去掉 sameFileContent 的去重分支（一律报错）→ (b) 红；
	//         去掉冲突报错（一律后者覆盖）→ (a) 红。
}

// ---------------------------------------------------------------------------
// 判据 14：源码级 —— 三处 collectRawFiles 失败必须置 buildFailed；
//           Lua 分支必须走 exitIfBuildFailed（我勘察出来的退出码洞）
// ---------------------------------------------------------------------------

func TestRawfileFailuresMarkBuildFailed(t *testing.T) {
	src, err := os.ReadFile("cmd_build.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)

	// (1) 所有 `collectRawFiles(".", plg.RawFiles)` 的 if 块里都要有 buildFailed = true
	//     且块内不能只有 Printf + return。
	needles := strings.Split(s, `collectRawFiles(".", plg.RawFiles)`)
	if len(needles) < 4 {
		t.Fatalf("未找到三处调用点（实际切出 %d 段）—— 判据的前提变了", len(needles))
	}
	for i, tail := range needles[1:] {
		// 只看紧邻的 12 行
		lines := strings.Split(tail, "\n")
		n := 12
		if len(lines) < n {
			n = len(lines)
		}
		window := strings.Join(lines[:n], "\n")
		if !strings.Contains(window, "buildFailed = true") {
			t.Errorf("★ 第 %d 处 collectRawFiles 失败分支没有置 buildFailed —— "+
				"退出码会是 0，调用方把失败当成功（口径 2 不成立）", i+1)
		}
	}

	// (2) Lua 分支的 return 之前必须有 exitIfBuildFailed()
	luaIdx := strings.Index(s, `buildTarget(plg, "lua", outDir, "")`)
	if luaIdx < 0 {
		t.Fatal("未找到 Lua 打包调用点")
	}
	luaTail := s[luaIdx:]
	// 只在**语句行**里找：注释里出现 "return" 这个词是正常的
	// （实测被这一点误伤过一次 —— 判据自己读源码时也得区分注释与代码）。
	luaLines := strings.Split(luaTail, "\n")
	sawExit := false
	sawReturn := false
	for _, ln := range luaLines {
		code := strings.TrimSpace(ln)
		if strings.HasPrefix(code, "//") {
			continue
		}
		if strings.HasPrefix(code, "exitIfBuildFailed()") {
			sawExit = true
			continue
		}
		if code == "return" {
			sawReturn = true
			break
		}
	}
	if !sawReturn {
		t.Fatal("Lua 分支后没有 return 语句？")
	}
	if !sawExit {
		t.Error("★ Lua 分支直接 return，跳过了 exitIfBuildFailed —— Lua 打包失败时退出码为 0")
	}

	// (3) 唯一失败退出点必须存在且真的 Exit(1)
	if !strings.Contains(s, "func exitIfBuildFailed()") {
		t.Fatal("★ 缺少 exitIfBuildFailed —— 失败路径会散落各处")
	}
	fnIdx := strings.Index(s, "func exitIfBuildFailed()")
	fnTail := s[fnIdx:]
	fnEnd := strings.Index(fnTail, "\n}")
	if fnEnd < 0 || !strings.Contains(fnTail[:fnEnd], "os.Exit(1)") {
		t.Error("★ exitIfBuildFailed 里没有 os.Exit(1)")
	}
	// 变异点：删掉任一处的 `buildFailed = true` → (1) 红；
	//         删掉 Lua 分支的 exitIfBuildFailed() → (2) 红。
}

// ---------------------------------------------------------------------------
// 判据 15：端到端 —— 真编译 hmapdev，真跑 build，退出码必须非零
// ---------------------------------------------------------------------------

// TestBuildExitsNonZeroOnRawfileFailure 是这个特性唯一的**端到端**判据。
//
// 为什么源码级判据不够：`buildFailed = true` 只是置位，真正让 shell 拿到
// 非零退出码的是 `os.Exit(1)`。而 cmdBuild 里有多条 return 路径（Lua 分支
// 甚至跳过末尾的收尾统计），"置位了但没走到 Exit"是完全可能的 ——
// 我勘察时就在现有代码里发现了 3 处这样的洞。只有真跑二进制才能咬住它。
//
// 用 `--no-bundle --target linux/amd64`：不依赖交叉编译工具链的完整性，
// 且 rawfile 校验在**编译之前**，所以判据不会变慢。
func TestBuildExitsNonZeroOnRawfileFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("需要编译 hmapdev 二进制，-short 下跳过")
	}
	bin := filepath.Join(t.TempDir(), "hmapdev")
	buildCmd := exec.Command("go", "build", "-o", bin, ".")
	buildCmd.Dir = "."
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("编译 hmapdev 失败: %v\n%s", err, out)
	}

	// 造一个最小插件工程：plg.json 声明的 url 条目**缺 sha256**
	proj := t.TempDir()
	writeFile := func(rel, content string) {
		p := filepath.Join(proj, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("plg.json", `{
  "name": "probe", "name_en": "Probe", "name_zh": "探针",
  "version": "0.0.1", "entry": "plugin.go", "targets": "linux/amd64",
  "rawfiles": [ { "name": "node/bin/node", "url": "https://example.invalid/node" } ]
}`)
	writeFile("plugin.go", "package main\n")

	cmd := exec.Command(bin, "build", "--no-bundle", "--target", "linux/amd64")
	cmd.Dir = proj
	t.Setenv(rawFileCacheEnv, t.TempDir())
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("★ 缺 sha256 的构建**退出码为 0** —— 口径 2 不成立（构建日志:\n%s）", out)
	}
	if !strings.Contains(string(out), "sha256") {
		t.Errorf("报错应点名 sha256，实际输出:\n%s", out)
	}
	// 不应产出 .hmap
	if m, _ := filepath.Glob(filepath.Join(proj, "*", "*.hmap")); len(m) > 0 {
		t.Errorf("★ 校验失败却产出了 .hmap: %v", m)
	}
	// 变异点：删掉 cmdBuild 里 validateRawFileDecls 失败分支的 exitIfBuildFailed()
	//         → 上面 err == nil → 红（退出码 0）。
}
