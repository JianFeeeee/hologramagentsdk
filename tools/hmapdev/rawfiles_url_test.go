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
// ## ★ 缺 sha256 的两道守卫与各自变异（复核前必读）
//
// 缺 sha256 由**两道守卫**把守（语义文案 + 格式校验，见 rawfiles_fetch.go
// 里那两段 if 上方的注释）。它们不能合并，否则要么对没写字段的人说"格式错"，
// 要么让 `sha256: "abc"` 静默通过。实测三种变异：
//
//	A. 只短路空串守卫   → TestURLRawFileRequiresSha256 **红**（文案断言命中）
//	B. 只短路格式守卫   → TestRawFileDeclStaticValidation **红**（长度/hex 用例）
//	A+B 同时短路       → 两条都红
//
// 所以两个守卫都有判据。但**只看 TestURLRawFileRequiresSha256 施加单点变异 B
// 会看到绿** —— 那不是判据漏了，而是该判据造的是空串，走不到格式分支。
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
	"io"
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
	// ★ 必须钉住**语义文案**，不能只要求"含 sha256"。
	//
	// 为什么：缺 sha256 有**两道守卫**（空串判定给语义文案；normalizeSHA256
	// 给格式校验），两道都含 "sha256"。只断言 `Contains(err, "sha256")` 的
	// 时候，短路掉空串守卫，归一点器的"必须是 64 位 hex（实际 0 位）"也能
	// 满足它 —— 复核时就会误以为判据没咬住（实测发生）。
	//
	// 这条断言守的是：对着一个**根本没写字段**的人，绝对不能回一句
	// "你的格式不对"。那句话会把人引到去数 hex 位数，而不是去补字段。
	if !strings.Contains(err.Error(), "必须") || !strings.Contains(err.Error(), "声明") {
		t.Errorf("★ 缺 sha256 的报错必须点明\"必须声明\"（而不是归一点器的长度错），实际: %v", err)
	}
	// 校验必须发生在下载**之前**
	if srv.Hits() != 0 {
		t.Errorf("★ 缺 sha256 时报错前不该发请求（实际 %d 次）", srv.Hits())
	}
	// 缓存目录应仍是空的
	if ents, _ := os.ReadDir(cacheDir); len(ents) != 0 {
		t.Errorf("校验失败不该在缓存里留东西: %v", ents)
	}
	// 显式空串（写了字段但值是 ""）与全角空格走同一条守卫，避免"我写了呀"
	// 这种歧义：`"sha256": ""` 与不写字段必须得到**同一句**报错。
	for _, blank := range []string{"", "   "} {
		err := validateRawFileDecls([]RawFileDecl{{Name: "a.txt", URL: srv.URL + "/a", SHA256: blank}})
		if err == nil {
			t.Errorf("★ sha256=%q 未被拒绝", blank)
			continue
		}
		if !strings.Contains(err.Error(), "必须") || !strings.Contains(err.Error(), "声明") {
			t.Errorf("sha256=%q 应走\"必须声明\"文案（与未写字段一致），实际: %v", blank, err)
		}
	}
	if srv.Hits() != 0 {
		t.Errorf("★ 空 sha256 也不该发请求（实际 %d 次）", srv.Hits())
	}

	// ## 变异说明（给下一个复核的人）
	//
	// 缺 sha256 由**两道守卫**把守（语义文案 + 格式校验，见 rawfiles_fetch.go
	// 里那两段 if 上方的注释）。实测三种变异的真实结果：
	//
	//	A. 只短路 `if TrimSpace(d.SHA256) == ""`   → **红**（文案断言命中：
	//	   "必须是 64 位 hex（实际 0 位）"—— 对没写字段的人说格式错）
	//	B. 只短路 `normalizeSHA256` 的报错            → **绿**（空串守卫已挡住，
	//	   本条不感知；但 `TestRawFileDeclStaticValidation` 会红，见下面的分注）
	//	A+B. 两道同时短路                           → **红**（err == nil，t.Fatal）
	//
	// 即：单点变异 A 已被咬住，单点变异 B 由另一条判据咬住。两条一起看，
	// 每个守卫都有判据，不存在无人把守的分支。
	//
	// ★ B 为何不在本条里红：本条造的是**空串**，空串先被语义守卫挡下，
	// 根本走不到格式校验。要观察 B，需要造一个"写了但格式错"的 sha256
	// （那正是 TestRawFileDeclStaticValidation 的 "sha256 不是 64 位" /
	// "sha256 非 hex" 两个子用例）。
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
	// ★ 本判据是「格式守卫」的唯一把守人：`sha256: "abc"` / 64 位非 hex
	// 只有到这里才会被拒（空串先被 rawfiles_fetch.go 的语义守卫挡下，
	// 那一条在 TestURLRawFileRequiresSha256 里验）。
	//
	// 变异：删掉 validateRawFileDecls 里的 normalizeSHA256 调用
	//   → 下面「sha256 不是 64 位」与「sha256 非 hex」两个子用例红。
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

// ---------------------------------------------------------------------------
// 判据 16：真 CLI 级端到端 —— 本地 from 声明在真实 build 路径上必须可用
// ---------------------------------------------------------------------------

// buildProbePlugin 造一个**能真正编译通过**的最小插件工程，返回工程目录。
//
// 为什么不能像上面那条判据一样只写 `package main`：那条只走到 rawfile
// 校验就退出了（校验在编译之前），而本判据要走到**打包**，所以插件必须
// 真的能编过。用 example/memo 的 plugin.go 作源（只读复制，不改仓库）。
func buildProbePlugin(t *testing.T, plgJSON string) string {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	memoSrc := filepath.Join(repoRoot, "example", "memo", "plugin.go")
	src, err := os.ReadFile(memoSrc)
	if err != nil {
		t.Fatalf("读 %s 失败（判据的前提变了？）: %v", memoSrc, err)
	}
	proj := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(proj, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("plugin.go", string(src))
	write("go.mod", "module probe\n\ngo 1.25.0\n\n"+
		"require github.com/JianFeeeee/homeagentsdk v0.0.0\n\n"+
		"replace github.com/JianFeeeee/homeagentsdk => "+repoRoot+"\n")
	write("plg.json", plgJSON)
	return proj
}

// hmapNames 读一个 .hmap，返回条目名 → 内容。
func hmapNames(t *testing.T, p string) map[string][]byte {
	t.Helper()
	zr, err := zip.OpenReader(p)
	if err != nil {
		t.Fatalf("打开 .hmap 失败: %v", err)
	}
	defer zr.Close()
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = b
	}
	return out
}

// TestCLILocalRawfileFromIsPackaged 是真 **CLI 级** 端到端判据。
//
// ## 它守的是什么（这是一个真实存在过的 100% 失效 bug）
//
// `rawfiles: ["legacy.txt"]` —— 文档里最朴素、`hmapdev init` 模板引导人写的
// 形式 —— 在真实 build 路径上**必然失败**：
//
//	error: rawfiles: rawfiles 路径逃出工程目录: "legacy.txt"
//
// 根因：逃脱检查用 `HasPrefix(cleanAbs, cleanRoot+"/")`，而真实调用方传的
// root 就是 "."。于是 cleanRoot="."、cleanAbs="legacy.txt"，
// `HasPrefix("legacy.txt", "./")` 恒假 ⇒ 任何 from 都被判成逃逸。
//
// ★ 为什么既有单测全绿：它们给 collectRawFiles 传的是 t.TempDir()（绝对路径），
// 永远踩不到 root="." 这条真实路径。**只有真跑二进制**才能暴露 —— 这正是
// 本判据存在的理由，也是"直接调函数"一类判据的系统性盲区。
func TestCLILocalRawfileFromIsPackaged(t *testing.T) {
	if testing.Short() {
		t.Skip("需要编译 hmapdev 二进制并真编插件，-short 下跳过")
	}
	bin := filepath.Join(t.TempDir(), "hmapdev")
	bc := exec.Command("go", "build", "-o", bin, ".")
	bc.Dir = "."
	if out, err := bc.CombinedOutput(); err != nil {
		t.Fatalf("编译 hmapdev 失败: %v\n%s", err, out)
	}

	proj := buildProbePlugin(t, `{
  "name": "probe", "name_en": "Probe", "name_zh": "探针",
  "version": "0.0.1", "description": "d", "author": "a",
  "entry": "plugin.so", "targets": "linux/amd64",
  "rawfiles": ["legacy.txt"]
}`)
	// 三种本地 from 形态一次覆盖：单文件字符串、目录对象、工程根下 rawfile/ 默认全收
	if err := os.WriteFile(filepath.Join(proj, "legacy.txt"), []byte("hello-legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(proj, "assets", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "assets", "sub", "x.txt"), []byte("from-assets"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(proj, "rawfile"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "rawfile", "default.txt"), []byte("default-dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "build", "--no-bundle", "--target", "linux/amd64")
	cmd.Dir = proj
	t.Setenv(rawFileCacheEnv, t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("★ 本地 from 声明的构建失败（退出码非 0）—— 文档里的写法在真实 CLI 上不可用。\n"+
			"构建输出:\n%s", out)
	}
	// 报错里不能出现"逃出工程目录"（修根因前的症状）
	if strings.Contains(string(out), "逃出工程目录") {
		t.Fatalf("★ 本地声明的合法路径被误判为逃逸。构建输出:\n%s", out)
	}

	hp := filepath.Join(proj, "dist", "probe_linux_amd64.hmap")
	ents := hmapNames(t, hp)

	// (1) 字符串声明 → rawfile/<basename>
	if b, ok := ents["rawfile/legacy.txt"]; !ok {
		t.Errorf("★ 包内没有 rawfile/legacy.txt（实际条目 %v）—— 字符串声明未生效", keysOfBytes(ents))
	} else if string(b) != "hello-legacy" {
		t.Errorf("rawfile/legacy.txt 内容不对: %q", b)
	}

	// (2) 工程根 rawfile/ 默认全收（这条同时验证 root 绝对化后默认目录遍历仍正确）
	if b, ok := ents["rawfile/default.txt"]; !ok {
		t.Errorf("★ 包内没有 rawfile/default.txt（rawfile/ 默认目录未收）: %v", keysOfBytes(ents))
	} else if string(b) != "default-dir" {
		t.Errorf("rawfile/default.txt 内容不对: %q", b)
	}

	// (3) ★ 包内条目名不得被绝对路径污染（root 绝化后被带进 zip 名是这次
	//     修改最可能引入的副作用，必须显式钉住）
	for name := range ents {
		if !strings.HasPrefix(name, "rawfile/") && name != "plugin.json" && name != "plugin.bin" &&
			name != "README.md" && name != "LICENSE" {
			t.Errorf("★ 意外的包内条目 %q", name)
		}
		if strings.HasPrefix(name, "rawfile/") && strings.Contains(name, proj) {
			t.Errorf("★ 包内条目名被工程绝对路径污染: %q", name)
		}
		if strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
			t.Errorf("★ 包内条目名是绝对路径或含 ..: %q", name)
		}
	}

	// (4) 目录对象声明（{from: assets}）也要能落成 rawfile/assets/sub/x.txt。
	//     与 (1)(2) 分开走一次 build，避免同一个 plg.json 里既有单文件又有目录时
	//     判断不出是谁生效。
	proj2 := buildProbePlugin(t, `{
  "name": "probe2", "name_en": "Probe2", "name_zh": "探针2",
  "version": "0.0.1", "description": "d", "author": "a",
  "entry": "plugin.so", "targets": "linux/amd64",
  "rawfiles": [ { "from": "assets" } ]
}`)
	if err := os.MkdirAll(filepath.Join(proj2, "assets", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj2, "assets", "sub", "x.txt"), []byte("from-assets"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd2 := exec.Command(bin, "build", "--no-bundle", "--target", "linux/amd64")
	cmd2.Dir = proj2
	out2, err := cmd2.CombinedOutput()
	if err != nil {
		t.Fatalf("★ {from: assets} 的构建失败:\n%s", out2)
	}
	ents2 := hmapNames(t, filepath.Join(proj2, "dist", "probe2_linux_amd64.hmap"))
	if b, ok := ents2["rawfile/assets/sub/x.txt"]; !ok {
		t.Errorf("★ 目录声明未保留结构（期望 rawfile/assets/sub/x.txt，实际 %v）", keysOfBytes(ents2))
	} else if string(b) != "from-assets" {
		t.Errorf("rawfile/assets/sub/x.txt 内容不对: %q", b)
	}

	// 变异点：把 collectRawFiles 入口的 filepath.Abs(root) 去掉
	//         → 第一个 build 就报"逃出工程目录"、退出码非 0 → 上面 t.Fatalf 红。
}

func keysOfBytes(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// ---------------------------------------------------------------------------
// 判据 17：local 与 url 撞名的**端到端**版（原来只直接调函数）
// ---------------------------------------------------------------------------

// TestCLILocalAndURLNameCollision 把「本地条目与 url 条目落到同一包内路径」
// 这件事放到真 CLI 上验一次。
//
// 为什么需要端到端版：直接调 collectRawFiles 的判据把 preflight
// （ensureRawFileDownloads）绕过去了 —— 它手动填 decls[i].materialized。
// 也就是说"真实 build 里 materialized 到底有没有被填上、撞名在真实路径上
// 会不会被检出"这整条链，此前没有任何判据覆盖。而这条链上有一个隐性的
// 顺序依赖：必须先预取、后收集（collectRawFiles 对未预取的 url 条目会报错）。
//
// 两种结局都必须在真 CLI 上成立：
//
//	· 内容不同 → 非零退出 + 报错点名冲突（不静默取一边）
//	· 内容相同 → 成功，且包内该路径只有一条
func TestCLILocalAndURLNameCollision(t *testing.T) {
	if testing.Short() {
		t.Skip("需要编译 hmapdev 二进制并起 HTTP 服务，-short 下跳过")
	}
	bin := filepath.Join(t.TempDir(), "hmapdev")
	bc := exec.Command("go", "build", "-o", bin, ".")
	bc.Dir = "."
	if out, err := bc.CombinedOutput(); err != nil {
		t.Fatalf("编译 hmapdev 失败: %v\n%s", err, out)
	}

	remoteBytes := []byte("REMOTE-CONTENT")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(remoteBytes)
	}))
	defer srv.Close()
	sum := shaOf(remoteBytes)

	plgFor := func() string {
		return `{
  "name": "probe", "name_en": "Probe", "name_zh": "探针",
  "version": "0.0.1", "description": "d", "author": "a",
  "entry": "plugin.so", "targets": "linux/amd64",
  "rawfiles": [
    "dup.txt",
    { "name": "dup.txt", "url": "` + srv.URL + `/dup", "sha256": "` + sum + `" }
  ]
}`
	}

	// (a) 内容不同 → 必须非零退出
	proj := buildProbePlugin(t, plgFor())
	if err := os.WriteFile(filepath.Join(proj, "dup.txt"), []byte("LOCAL-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "build", "--no-bundle", "--target", "linux/amd64")
	cmd.Dir = proj
	t.Setenv(rawFileCacheEnv, t.TempDir())
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("★ local 与 url 撞名且内容不同，构建**成功**了 —— 会静默取一边\n输出:\n%s", out)
	}
	if !strings.Contains(string(out), "冲突") {
		t.Errorf("报错应点名冲突，实际输出:\n%s", out)
	}

	// (b) 内容相同 → 必须成功，且包内该路径只有一条
	proj2 := buildProbePlugin(t, plgFor())
	if err := os.WriteFile(filepath.Join(proj2, "dup.txt"), remoteBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd2 := exec.Command(bin, "build", "--no-bundle", "--target", "linux/amd64")
	cmd2.Dir = proj2
	t.Setenv(rawFileCacheEnv, t.TempDir())
	out2, err := cmd2.CombinedOutput()
	if err != nil {
		t.Fatalf("★ 内容相同时应去重成功，实际失败:\n%s", out2)
	}
	hp := filepath.Join(proj2, "dist", "probe_linux_amd64.hmap")
	zr, err := zip.OpenReader(hp)
	if err != nil {
		t.Fatalf("打开 .hmap 失败: %v", err)
	}
	defer zr.Close()
	n := 0
	for _, f := range zr.File {
		if f.Name == "rawfile/dup.txt" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("★ 去重后 rawfile/dup.txt 应只有 1 条，实际 %d 条（内核解包后取哪份取决于顺序）", n)
	}

	// 变异点：把 collectRawFiles 里"内容相同则去重"改成一律报错 → (b) 红；
	//         把冲突报错改成静默覆盖 → (a) 红。
	//         把 cmdBuild 里的 ensureRawFileDownloads 删掉 → (a)(b) 都红
	//         （collectRawFiles 会报"url 来源尚未预取"）。
}
