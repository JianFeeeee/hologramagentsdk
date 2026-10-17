package main

// rawfile 打包判据。
//
// 规则（用户拍板）：
//  ① 工程根 `rawfile/` 目录**默认全收**（无需在 plg.json 声明）
//  ② plg.json 的 `rawfiles` 数组声明**额外**的外部路径
//  ③ 解包后一律落在 `<插件目录>/rawfile/` 下

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkProject 造一个最小工程结构。
func mkProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 默认目录（含嵌套）
	write("rawfile/main/prompt.txt", "you are a dev")
	write("rawfile/main/nested/data.json", `{"a":1}`)
	// 外部路径（声明后才收）
	write("assets/theme.css", "body{}")
	write("third_party/node/bin/node", "#!/bin/sh\n")
	return root
}

// ★★ 判据：**rawfile/ 目录默认全收**，无需任何声明。
func TestRawfileDirIsCollectedByDefault(t *testing.T) {
	root := mkProject(t)
	ents, err := collectRawFiles(root, nil) // 不给声明
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range ents {
		got[e.zip] = true
	}
	for _, want := range []string{
		"rawfile/main/prompt.txt",
		"rawfile/main/nested/data.json", // ★ 嵌套结构必须保留
	} {
		if !got[want] {
			t.Errorf("默认目录漏了 %s（实际 %v）", want, keysOf(got))
		}
	}
	// 未声明的外部路径**不该**被收
	for _, shouldNot := range []string{"rawfile/theme.css", "rawfile/node"} {
		if got[shouldNot] {
			t.Errorf("未声明的外部路径被误收: %s", shouldNot)
		}
	}
}

// ★★ 判据：声明的外部路径也收，且**统一落到 rawfile/ 下**。
func TestDeclaredExternalPathsAlsoCollected(t *testing.T) {
	root := mkProject(t)
	ents, err := collectRawFiles(root, []RawFileDecl{
		{From: "assets/theme.css"},
		{From: "third_party/node/bin/node"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range ents {
		got[e.zip] = true
	}
	// ★ 规则③：一律在 rawfile/ 下
	if !got["rawfile/theme.css"] {
		t.Errorf("声明的文件未落到 rawfile/ 下: %v", keysOf(got))
	}
	if !got["rawfile/node"] {
		t.Errorf("声明的文件未落到 rawfile/ 下: %v", keysOf(got))
	}
	// 不得出现 rawfile/ 之外的条目
	for z := range got {
		if !strings.HasPrefix(z, "rawfile/") {
			t.Errorf("★ 有资源落在 rawfile/ 之外: %s", z)
		}
	}
}

// ★★ 判据：目录递归展开时**保留结构**（不是拍平）。
//
// 这是 rawfile 与既有 createHmap 的关键区别：后者用 filepath.Base()，
// 目录进去会全部拍平成一层，`main/nested/data.json` 与另一处的 `data.json` 撞名。
func TestDirectoryStructureIsPreserved(t *testing.T) {
	root := mkProject(t)
	ents, err := collectRawFiles(root, []RawFileDecl{{From: "assets"}})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range ents {
		if e.zip == "rawfile/assets/theme.css" {
			found = true
		}
		if e.zip == "rawfile/theme.css" {
			t.Error("★ 目录被拍平了（丢了 assets/ 这一层）")
		}
	}
	if !found {
		t.Errorf("目录展开结果不对: %v", zipNames(ents))
	}
}

// ★ 判据：exclude 生效。
func TestExcludeWorks(t *testing.T) {
	root := mkProject(t)
	if err := os.WriteFile(filepath.Join(root, "assets/skip.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ents, err := collectRawFiles(root, []RawFileDecl{{From: "assets", Exclude: []string{"skip.tmp"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.Contains(e.zip, "skip.tmp") {
			t.Errorf("exclude 未生效: %s", e.zip)
		}
	}
}

// ★★ 判据：声明路径**逃出工程目录**必须报错。
//
// 不挡的话既让构建不可复现，也可能无意把凭据/密钥打进发布包。
func TestPathEscapeIsRejected(t *testing.T) {
	root := mkProject(t)
	if _, err := collectRawFiles(root, []RawFileDecl{{From: "../../etc/passwd"}}); err == nil {
		t.Fatal("★ 逃出工程目录的声明未被拒绝")
	}
}

// ★★ 判据：两条来源落到同一包内路径 ⇒ **报错**（不静默覆盖）。
//
// 不报的话"我改的没生效"极难查（后写的覆盖前者，且无任何提示）。
func TestConflictingTargetsAreRejected(t *testing.T) {
	root := mkProject(t)
	// ★ 真冲突：两条外部声明落到同一个 base 名（都变成 rawfile/<base>）
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(root, d, "same.txt"), []byte(d), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := collectRawFiles(root, []RawFileDecl{
		{From: "a/same.txt"}, {From: "b/same.txt"},
	}); err == nil {
		t.Fatal("★ 目标冲突未被检出（会静默覆盖）")
	}
}

// ★ 判据：optional 缺失只警告不报错；非 optional 缺失报错。
func TestOptionalMissingSource(t *testing.T) {
	root := mkProject(t)
	if _, err := collectRawFiles(root, []RawFileDecl{{From: "nope.txt"}}); err == nil {
		t.Error("非 optional 的缺失源应报错")
	}
	if _, err := collectRawFiles(root, []RawFileDecl{{From: "nope.txt", Optional: true}}); err != nil {
		t.Errorf("optional 缺失不应报错: %v", err)
	}
}

// ★★ 判据：输出**确定有序**（同样输入产出同样字节）。
//
// 不稳定的话每次构建的 .hmap 都不同，校验和与 diff 全无意义。
func TestCollectionIsDeterministic(t *testing.T) {
	root := mkProject(t)
	a, err := collectRawFiles(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := collectRawFiles(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("两次收集数量不同: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].zip != b[i].zip {
			t.Fatalf("顺序不稳定（第 %d 项）: %s vs %s", i, a[i].zip, b[i].zip)
		}
	}
}

// ★★ 判据：**plg.json 同时接受字符串与对象两种写法**。
func TestRawFileDeclAcceptsBothForms(t *testing.T) {
	var decls []RawFileDecl
	raw := `["plain/path.txt", {"from": "obj/path", "exclude": ["x"]}, {"from": "opt", "optional": true}]`
	if err := json.Unmarshal([]byte(raw), &decls); err != nil {
		t.Fatal(err)
	}
	if len(decls) != 3 {
		t.Fatalf("应解析出 3 条，实际 %d", len(decls))
	}
	if decls[0].From != "plain/path.txt" {
		t.Errorf("字符串写法解析错: %+v", decls[0])
	}
	if decls[1].From != "obj/path" || len(decls[1].Exclude) != 1 {
		t.Errorf("对象写法解析错: %+v", decls[1])
	}
	if !decls[2].Optional {
		t.Errorf("optional 未解析: %+v", decls[2])
	}
}

// ★★ 判据：**真正写进 zip 后，条目名保留完整路径**。
//
// 这条是端到端的：前面的判据只看收集结果，这里看 zip 里的真实条目。
func TestRawEntriesWrittenWithFullPath(t *testing.T) {
	root := mkProject(t)
	ents, err := collectRawFiles(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "t.hmap")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	if err := writeRawEntries(w, ents); err != nil {
		t.Fatal(err)
	}
	w.Close()
	f.Close()

	r, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	names := map[string]bool{}
	for _, zf := range r.File {
		names[zf.Name] = true
	}
	if !names["rawfile/main/nested/data.json"] {
		t.Fatalf("zip 里没有保留路径的条目（实际 %v）", keysOf(names))
	}
	// 内容也要对
	for _, zf := range r.File {
		if zf.Name == "rawfile/main/prompt.txt" {
			rc, _ := zf.Open()
			buf := make([]byte, 64)
			n, _ := rc.Read(buf)
			rc.Close()
			if !strings.Contains(string(buf[:n]), "you are a dev") {
				t.Errorf("内容不对: %q", string(buf[:n]))
			}
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func zipNames(ents []rawEntry) []string {
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.zip)
	}
	return out
}

// ★★★ 判据：**绝对路径也必须挡在工程外**（实测发现的真 bug）。
//
// 初版只在相对路径分支里检查 `../`，于是 `{"from": "/etc/passwd"}`
// 被原样打进发布包 —— 那是把宿主机凭据/密钥带进发行的真实通道。
// `filepath.Join(root, "/etc/passwd")` 也不会逃出（Join 会把绝对路径
// 当相对处理），所以这条走的是"根 + 绝对路径"这条更隐蔽的入口。
func TestAbsolutePathOutsideProjectIsRejected(t *testing.T) {
	// ★ 关键：必须**造出真实存在的逃逸目标**。
	//
	// 初版用 `/etc/passwd` 之类的固定路径 —— 那是错的：在没有该文件的
	// 环境里（如临时目录的父级），实现会因"文件不存在"报错，
	// 于是**判据通过但那不是因为拒绝了逃逸**。实测变异（去掉前缀检查）
	// 没被咬住，正是这个原因：报错来自 Stat 失败，不是来自安全检查。
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("REDACTED"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := mkProject(t)
	// 构造一个**从工程根出发、真实指向工程外**的相对路径
	rel, err := filepath.Rel(root, filepath.Join(outside, "secret.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, from := range []string{rel, "/etc/passwd", "a/../../../etc/passwd"} {
		ents, cerr := collectRawFiles(root, []RawFileDecl{{From: from}})
		if cerr == nil {
			t.Errorf("★ 逃出工程的声明 %q 未被拒绝（收集到 %d 项）", from, len(ents))
			continue
		}
		// 报错原因必须是**逃逸**，不能是"文件不存在"
		if !strings.Contains(cerr.Error(), "逃出") && !strings.Contains(cerr.Error(), "绝对路径") {
			t.Errorf("拒绝 %q 的理由不对（应为逃逸/绝对路径，实际: %v）", from, cerr)
		}
	}
	// 相对路径（相对工程根）是唯一接受的形态
	ents, err := collectRawFiles(root, []RawFileDecl{{From: "assets/theme.css"}})
	if err != nil {
		t.Errorf("工程内的相对路径应被接受: %v", err)
	} else {
		found := false
		for _, e := range ents {
			if e.zip == "rawfile/theme.css" {
				found = true
			}
		}
		if !found {
			t.Errorf("声明的相对路径未落到 rawfile/ 下: %v", zipNames(ents))
		}
	}
}

// ★★ 判据：**外部声明的目录也要保留结构**（不只是默认目录）。
//
// 最初只对默认 rawfile/ 目录验了结构保留，于是"外部目录被拍平"
// 这个变异没有咬住 —— 判据必须覆盖**每一条**会产生 zip 路径的分支。
func TestDeclaredDirectoryKeepsStructure(t *testing.T) {
	root := mkProject(t)
	if err := os.MkdirAll(filepath.Join(root, "assets/sub/deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets/sub/deep/x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ents, err := collectRawFiles(root, []RawFileDecl{{From: "assets"}})
	if err != nil {
		t.Fatal(err)
	}
	var want string
	for _, e := range ents {
		if strings.HasSuffix(e.zip, "x.txt") {
			want = e.zip
		}
	}
	if want != "rawfile/assets/sub/deep/x.txt" {
		t.Fatalf("★ 外部目录未保留结构（期望 rawfile/assets/sub/deep/x.txt，实际 %q）", want)
	}
}

// ★★ 判据：脚手架文件（.gitkeep/.gitignore/.DS_Store）**不进包**。
//
// 实测发现：init 出的空工程第一次 build 就报"已打包 2 个 rawfile"，
// 里面只有 .gitkeep 与 .gitignore。既是噪声（误导开发者以为有资源），
// 也是把工程内部约定发给了运行期。
func TestScaffoldFilesAreExcluded(t *testing.T) {
	root := mkProject(t)
	for _, f := range []string{".gitkeep", ".gitignore", ".DS_Store", "main/.gitkeep"} {
		p := filepath.Join(root, "rawfile", f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ents, err := collectRawFiles(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if IsRawFileScaffold(e.zip) {
			t.Errorf("★ 脚手架文件被打进包: %s", e.zip)
		}
	}
	// 真实资源仍要在
	found := false
	for _, e := range ents {
		if strings.HasSuffix(e.zip, "prompt.txt") {
			found = true
		}
	}
	if !found {
		t.Error("真实资源被误排")
	}
}

// ★ 判据：空工程（只有脚手架）→ 收集结果为空，不该报"打包了 N 个"。
func TestEmptyProjectCollectsNothing(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "rawfile"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "rawfile/.gitkeep"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(root, "rawfile/.gitignore"), []byte("*\n"), 0o644)
	ents, err := collectRawFiles(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Errorf("★ 空工程应收 0 项，实际 %d: %v", len(ents), zipNames(ents))
	}
}
