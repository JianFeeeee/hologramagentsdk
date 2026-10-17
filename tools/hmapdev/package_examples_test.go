package main

// 发版打包脚本（package/build-examples.sh）的判据（2026-10-17）。
//
// ## 守什么
//
//  1. **产物架构必须与目标平台一致** —— 打 arm64 包不能装进 x86-64 二进制
//  2. **Lua 插件不得被静默跳过** —— 它们没有 plugin.go，但有 main.lua
//
// ## ★ 为什么必须是行为判据（而不是扫脚本源码）
//
// 这两个缺陷的共性是「**脚本逻辑错了但看起来成功**」：
//
//   · 架构错：`ls dist/*.hmap | head -1` 在 `--target` 追加语义下
//     按字典序拿到 amd64 那份 ⇒ 文件名写 `linux_arm64_...` 而内容是 x86-64。
//     脚本**退出码 0、校验和也对**（校验和只证明文件没坏，不证明架构对）。
//   · Lua 跳过：入口判据只认 plugin.go ⇒ luademo 静默不产包，
//     同样退出码 0、同样"成功 21/21"。
//
// ⇒ 如果只扫脚本里有没有 `_"${GOOS}_${GOARCH}"` 字样，把匹配写错、
//   或 `head -1` 又溜回来，判据仍会绿。**必须真的构建并检查 ELF 头。**

import (
	"archive/zip"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ★★★ 判据：**打包脚本真的按目标平台挑产物**（跑真实脚本，检查 ELF 架构）。
//
// ## 为什么必须跑**脚本本身**（而不是只测 hmapdev）
//
// 缺陷在脚本的"从产物里挑哪一份"这一步：
//
//	hmap="$(ls "$dir"/dist/*.hmap | head -1)"   ← 按字典序拿到 amd64
//
// 而 `--target` 是**追加**语义（docs/guide/multi-platform.md 写明），
// 示例 plg.json 又普遍声明 `targets: "linux/amd64"`
// ⇒ `--target linux/arm64` 会**同时**产出 amd64 与 arm64 两份
// ⇒ `head -1` 拿到 amd64 ⇒ **打 arm64 包却装进 x86-64 二进制**。
//
// ★ 我第一版判据直接调 `hmapdev` 并自己挑目标平台那份 ⇒ **把变异改回
//
//	`head -1` 后判据照样绿**（它根本没跑脚本，测的是 hmapdev 而非脚本）。
//	⇒ 判据必须驱动**真实的 package/build-examples.sh**。
//
// ## 做法
//
// 造一个**最小 SDK 镜像**（临时目录）：
//
//	<tmp>/package/build-examples.sh   ← 复制真实脚本
//	<tmp>/example/memo/               ← symlink 到真实示例（含 plg.json targets）
//	<tmp>/build/hmapdev_<host>        ← 宿主工具链（脚本按此名查找）
//
// 然后以 arm64 为目标跑脚本，读**它产出的**文件里的 ELF machine。
func TestPackagingScriptPicksTargetArchArtifact(t *testing.T) {
	if testing.Short() {
		t.Skip("需要真实交叉构建")
	}
	host := hostArch()
	var target, wantMachine, wantSuffix string
	switch host {
	case "amd64":
		target, wantMachine, wantSuffix = "linux/arm64", "aarch64", "linux_arm64"
	case "arm64":
		target, wantMachine, wantSuffix = "linux/amd64", "x86-64", "linux_amd64"
	default:
		t.Skipf("未知宿主架构 %s", host)
	}

	sdkRoot := filepath.Join("..", "..")
	scriptSrc := filepath.Join(sdkRoot, "package", "build-examples.sh")
	if _, err := os.Stat(scriptSrc); err != nil {
		t.Skipf("找不到打包脚本: %v", err)
	}
	exampleSrc := filepath.Join(sdkRoot, "example", "memo")
	plg, err := os.ReadFile(filepath.Join(exampleSrc, "plg.json"))
	if err != nil || !strings.Contains(string(plg), `"targets"`) {
		t.Skip("示例未声明 targets，复现不了「追加」前提")
	}

	// 最小镜像
	mirror := t.TempDir()
	mustMkdir(t, filepath.Join(mirror, "package"))
	mustMkdir(t, filepath.Join(mirror, "example"))
	mustMkdir(t, filepath.Join(mirror, "build"))
	copyFile(t, scriptSrc, filepath.Join(mirror, "package", "build-examples.sh"))
	// ★ symlink 目标必须是**绝对路径**：脚本与插件内部有 `cd`，
	// 相对目标会相对**链接所在目录**解析（实测 `../../example/memo` 从
	// mirror/example/ 解析成了 mirror 的父目录 ⇒ 循环匹配不到任何插件，
	// 表现为「成功 0 / 失败 0」而不报错）。
	if err := os.Symlink(absOf(t, exampleSrc), filepath.Join(mirror, "example", "memo")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// 脚本按 hmapdev_<host> 名查找工具链
	dev := buildHostHmapdev(t)
	if err := os.Symlink(dev, filepath.Join(mirror, "build", "hmapdev_"+host)); err != nil {
		t.Fatalf("symlink hmapdev: %v", err)
	}
	// 插件的 go.mod 里有 `replace github.com/JianFeeeee/homeagentsdk => ../../`，
	// 它解析到**镜像根** ⇒ 镜像根必须有真实的 SDK Go 源码（go.mod + sdk/ + meta/…）。
	// 不接上的话，交叉构建会以
	//   `reading <mirror>/go.mod: no such file or directory` 失败 ——
	// 看起来像工具链坏了，其实只是测试夹具不全。
	sdkAbs := absOf(t, sdkRoot)
	for _, name := range []string{"go.mod", "go.sum", "sdk", "meta", "lua"} {
		src := filepath.Join(sdkAbs, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.Symlink(src, filepath.Join(mirror, name)); err != nil {
			t.Logf("symlink %s（非致命）: %v", name, err)
		}
	}
	// 脚本在找不到工具链时会尝试 `cd <sdk>/tools/hmapdev && go build`。
	// 这里已提供工具链，但保险起见仍把该目录接上（脚本的 fallback 分支）。
	mustMkdir(t, filepath.Join(mirror, "tools"))
	if err := os.Symlink(filepath.Join(absOf(t, sdkRoot), "tools", "hmapdev"),
		filepath.Join(mirror, "tools", "hmapdev")); err != nil {
		t.Logf("symlink tools/hmapdev（非致命）: %v", err)
	}

	// ★ OUT_DIR 必须传**绝对路径**：脚本内部有 `cd "$dir"`（进插件目录构建），
	// 若 OUT_DIR 是相对的，后续 `mkdir -p`/`cp` 会解析到插件目录下 ——
	// 实测那样会得到「脚本报成功、但 outDir 里只有 MANIFEST/SHA256SUMS」，
	// 看起来像"没有插件被构建"，其实产物落到了别处。
	outDir := filepath.Join(mirror, "out")
	cmd := exec.Command("bash", filepath.Join(mirror, "package", "build-examples.sh"), target, outDir)
	cmd.Dir = mirror
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	scriptOut, err := cmd.CombinedOutput()
	t.Logf("脚本输出:\n%s", scriptOut)
	if err != nil {
		t.Fatalf("脚本失败: %v\n%s", err, scriptOut)
	}

	// 脚本自己命名产物为 <goos>_<goarch>_<name>.hmap
	var picked string
	entries, _ := os.ReadDir(outDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), wantSuffix+"_") && strings.HasSuffix(e.Name(), ".hmap") {
			picked = filepath.Join(outDir, e.Name())
			break
		}
	}
	if picked == "" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("脚本未产出 %s 前缀的包。实际: %v", wantSuffix, names)
	}

	if got := elfMachineOfHmap(t, picked); got != wantMachine {
		t.Fatalf("★ 脚本产出的 %s 里 plugin.bin 架构是 %s，期望 %s\n"+
			"   ⇒ 脚本挑错了产物（文件名写 %s，内容却是别的平台）。\n"+
			"   在目标机器上只会以 Exec format error 失败，且排查方向被文件名带偏。",
			filepath.Base(picked), got, wantMachine, target)
	}
}

func absOf(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// mustMkdir / copyFile：测试辅助
func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// ★★★ 判据：**Lua 插件不被打包脚本跳过**。
//
// luademo 没有 plugin.go（它是 main.lua），原脚本的
// `[ -f "$dir/plugin.go" ] || continue` 会**静默跳过它**
// ⇒ 发版产物少一个插件，而日志显示"成功 N/N"、退出码 0。
//
// ★ 判据形态：扫脚本源码里"入口判据"那一行 —— 因为要断言的是
//
//	"脚本对 Lua 插件的**接纳条件**是否放宽"，这是一个**模式存在性**断言
//	（不是接线/顺序断言），源码扫描在此恰当。
func TestPackagingScriptAcceptsLuaPlugins(t *testing.T) {
	p := filepath.Join("..", "..", "package", "build-examples.sh")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Skipf("读不到打包脚本: %v", err)
	}
	src := string(b)

	// 找"入口判据"：必须同时认 plugin.go 与 main.lua
	hasGoEntry := strings.Contains(src, "plugin.go")
	hasLuaEntry := strings.Contains(src, "main.lua")
	if !hasGoEntry {
		t.Fatal("脚本不再认 plugin.go 入口？请检查是否改坏了")
	}
	if !hasLuaEntry {
		t.Fatal("★ 打包脚本只认 plugin.go 入口 ⇒ **Lua 插件被静默跳过**\n" +
			"   （luademo 是 main.lua，不产 .hmap，脚本仍报成功、退出码 0）")
	}
	// 反向钉住：不能只把 main.lua 写在注释里就算数 ——
	// 判据行（含 continue 的那段）必须在 main.lua 附近出现
	idx := strings.Index(src, "main.lua")
	if idx < 0 {
		t.Fatal("main.lua 未出现在脚本中")
	}
	// 该处周边 300 字符内应有 continue 或 -f 判断（即真的是入口判据）
	lo := idx - 300
	if lo < 0 {
		lo = 0
	}
	hi := idx + 300
	if hi > len(src) {
		hi = len(src)
	}
	around := src[lo:hi]
	if !strings.Contains(around, "continue") && !strings.Contains(around, "-f") {
		t.Error("★ main.lua 只出现在注释里，入口判据并未真的接纳 Lua 插件")
	}
}

// ---- helpers ----

func hostArch() string {
	out, err := exec.Command("go", "env", "GOARCH").Output()
	if err != nil {
		return ""
	}
	a := strings.TrimSpace(string(out))
	switch a {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	}
	return a
}

func buildHostHmapdev(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "hmapdev")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("构建宿主 hmapdev 失败（跳过）: %v\n%s", err, b)
	}
	return out
}

// elfMachineOfHmap 读出 .hmap 里 plugin.bin 的架构名。
func elfMachineOfHmap(t *testing.T, hmapPath string) string {
	t.Helper()
	zr, err := zip.OpenReader(hmapPath)
	if err != nil {
		t.Fatalf("打开 %s: %v", hmapPath, err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "plugin.bin") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		hdr := make([]byte, 20)
		n, _ := rc.Read(hdr)
		rc.Close()
		if n < 20 || hdr[0] != 0x7f || hdr[1] != 'E' || hdr[2] != 'L' || hdr[3] != 'F' {
			t.Fatalf("%s 里的 %s 不是 ELF", hmapPath, f.Name)
		}
		switch binary.LittleEndian.Uint16(hdr[18:20]) {
		case 0x3e:
			return "x86-64"
		case 0xb7:
			return "aarch64"
		}
		return "unknown"
	}
	t.Fatalf("%s 里没有 plugin.bin", hmapPath)
	return ""
}
