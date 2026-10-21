package main

// 打包层判据的夹具助手。
//
// 为什么单独一个文件：TestPackageShipsSidecarScript 与 TestLocateSidecarScriptFindsShippedCopy
// 都要"摆一份跟踪源到临时目录/摆一份解包布局"，这几个动作与具体判据无关。
// 放这里，别的判据也能复用，不必各自再写一遍拷贝逻辑。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// mustReadFile 读一个测试输入文件；读不到直接 fatal（缺输入是判据前提被破坏，
// 不是"环境不支持"——静默 skip 会让判据变成假绿）。
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return b
}

// copyTrackedProject 把插件工程**拷进临时目录**，只带构建必需的跟踪文件。
//
// 为什么不直接原地构建：hmapdev build 会往工程里写 build//dist//plugin.json，
// 在源码目录构建会把工作区弄脏（并发跑测试时还会互相踩）。
// 为什么不拷 rawfile/：rawfile 的内容不进版本库，这里要验的正是
// "打包时由构建方另行准备的资源会不会被带上"，rawfile 必须由调用方显式摆。
//
// 跳过项：
//   - src 里不存在的（可选）
//   - rawfile/ dist/ build/（前两者是产物，rawfile 由调用方摆）
//   - *_test.go（构建不需要，且它们会引用工程根相对路径，拷过去反而误导）
//   - .gitignore 等版本控制文件（对构建无意义）
func copyTrackedProject(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("读工程目录 %s: %v", src, err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case name == "rawfile", name == "dist", name == "build",
			name == ".git", name == ".pi-sessions", name == ".zcode", name == ".zcode-sessions":
			continue
		case strings.HasSuffix(name, "_test.go"):
			continue
		case strings.HasPrefix(name, "."):
			continue
		}
		if err := copyPath(filepath.Join(src, name), filepath.Join(dst, name)); err != nil {
			t.Fatalf("拷 %s: %v", name, err)
		}
	}
}

func copyPath(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, st.Mode().Perm())
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// buildHostHmapdevForTest 用本仓当前源码构建 hmapdev，返回可执行文件路径。
//
// ★ 必须"用当前源码构建"，不能找 PATH 上的 hmapdev 或 build/ 里的旧二进制：
// 那种判据验的是**已装的那份工具链**，改了 rawfiles.go 它照样绿（实测过的坑，
// 见 package/build-examples.sh 里"工具链过期就必须重编"那段注释）。
func buildHostHmapdevForTest(t *testing.T) string {
	t.Helper()
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	srcDir := filepath.Join(repo, "tools", "hmapdev")
	if _, err := os.Stat(filepath.Join(srcDir, "go.mod")); err != nil {
		t.Fatalf("找不到 tools/hmapdev（%s）: %v", srcDir, err)
	}
	out := filepath.Join(t.TempDir(), "hmapdev")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false")
	if b, err := cmd.CombinedOutput(); err != nil {
		// 构建工具链失败多半是**环境**问题（无网 / 无模块缓存，拉不到 yaegi 之类）。
		// 但那不是"可以跳过"的理由：本判据守的是"随包资源到底进没进包"，
		// 跳过它等于把 rawfile 通道的守卫撤了（本仓曾以"skip 而非零退"丢过一次守卫）。
		// ⇒ 响亮失败，并把**怎么解决**写进文案。
		t.Fatalf("构建 hmapdev 失败: %v\n%s\n"+
			"这通常是环境问题（拉不到 tools/hmapdev 的依赖）。"+
			"解法：设 GOPROXY（如 GOPROXY=https://goproxy.cn,direct）"+
			"或先 `cd tools/hmapdev && go mod download`。", err, b)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("hmapdev 未产出: %v", err)
	}
	return out
}

// stageShippedRuntime 在临时目录里摆出"解包后"的随包资源布局，返回 rawfile 根。
//
// 布局与 hmapdev 解包完全一致：<root>/runtime/<cli>/{driver.json,sidecar.mjs}。
// 源取自**跟踪进版本库**的文件：
//
//	examples/custom-cli/zcode/{driver.json,sidecar.mjs}   zcode（参考实现）
//	sidecar.mjs + runtime/pi/driver.json                  pi
//
// 为什么不直接读工作区的 rawfile/：它不进版本库，干净克隆下不存在；
// 拿它当夹具会让判据"本机有资源就绿、CI 就红"，两边都不是可靠判据。
func stageShippedRuntime(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	put := func(cli, name string, b []byte) {
		d := filepath.Join(root, "runtime", cli)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("pi", "sidecar.mjs", mustReadFile(t, "sidecar.mjs"))
	put("zcode", "sidecar.mjs",
		mustReadFile(t, filepath.Join("examples", "custom-cli", "zcode", "sidecar.mjs")))
	put("zcode", "driver.json",
		mustReadFile(t, filepath.Join("examples", "custom-cli", "zcode", "driver.json")))
	if _, err := os.Stat(filepath.Join("runtime", "pi", "driver.json")); err == nil {
		put("pi", "driver.json", mustReadFile(t, filepath.Join("runtime", "pi", "driver.json")))
	}
	return root
}

// absUnderPackage 把工程内相对路径转成绝对路径，并断言它存在。
func absUnderPackage(t *testing.T, rel string) string {
	t.Helper()
	a, err := filepath.Abs(rel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a); err != nil {
		t.Fatalf("%s 不存在: %v", rel, err)
	}
	return a
}

// ===== 「本机有没有这个可执行文件」的前置条件（带可复现说明）=====
//
// ## 为什么需要这两条 helper
//
// 有两条判据要通过**产品自己的解析链**去起一个真进程：
//
//	TestPiMcpListOnFreshInstall     → p.piBin()  → 起 `pi mcp list --json`
//	TestSwappingDriverSwapsBackend  → p.nodeBin() → 起 node 跑假 sidecar
//
// 机器上缺那个二进制时，它们原来直接 **FAIL**（而不是 skip）：
//
//	★ 无配置时不该报错: map[error:exec: "pi": executable file not found in $PATH …]
//	★ 换驱动后启动失败（核心仍依赖 pi？）: 找不到 node（… 也可设 node_bin 或用 PATH）
//
// 这两条红**红在机器没装东西**，与被测代码无关 —— 即是「真红，但原因错了」。
// 它威胁 CI：examples-tests job 会跑 commondev，某天 runner 缺 node/pi
// 就会红，而红的原因跟被测代码无关。
//
// ## 处置口径（三条）
//
//  1. **有就必须真跑**：skip 只在"确实没有"时发生。判断用**产品自己的**
//     piBin()/nodeBin() —— 不用另写一套 "LookPath 一下" 的判据，
//     否则判据与产品行为会漂移（产品改了查找链，skip 判据还在看旧位置）。
//  2. **缺就 skip 并说清三件事**：缺哪个二进制、这条原本会验证什么、
//     装/配了什么之后才算真验过。不写一句"跳过"。
//  3. skip 数变化必须如实记账（脚本会把 skip 名单打进日志）。

// requirePiBinary 断言"产品能找到 pi"，否则 skip 并说明原始验什么、怎么补上。
//
// 判据用 p.piBin() 而不是 exec.LookPath("pi")：后者会忽略
// `pi_bin` 配置与 `<插件数据>/runtime/pi` 这两条产品支持的来源，
// 于是"用户配了 pi_bin 但 PATH 上没有"这种合法部署会被误判成缺失。
func requirePiBinary(t *testing.T, p *Plugin) string {
	t.Helper()
	bin := p.piBin()
	if _, err := exec.LookPath(bin); err != nil && !fileExists(bin) {
		t.Skipf("跳过：本机找不到 pi 可执行文件（p.piBin() 解析到 %q，LookPath/stat 都失败）。"+
			"本条判据原本要验证的是：**全新安装（无 mcp.json）时 pi_mcp_list 不得报错、应回空列表** —— "+
			"它要真起一次 `pi mcp list --json` 才能判断，没有 pi 就没法验（不是被测代码有问题）。"+
			"补上：装 pi（npm i -g @earendil-works/pi-coding-agent）、"+
			"或把 <插件数据>/runtime/pi 放好、或用 settings 的 pi_bin 指到真实路径。", bin)
	}
	return bin
}

// requireNodeBinary 断言"产品能找到 node"，否则 skip 并说明原始验什么。
//
// 同样用 p.nodeBin()：产品支持 node_bin / 随包 rawfile/node/bin/node / PATH 三级，
// 判据若只看 PATH 就会把"随包 node 已就位"的部署误判成缺失。
func requireNodeBinary(t *testing.T, p *Plugin) string {
	t.Helper()
	bin := p.nodeBin()
	if bin == "" {
		t.Skipf("跳过：本机找不到 node（p.nodeBin() 三级链全部落空：settings.node_bin、" +
			"<插件>/rawfile/node/bin/node、PATH 上的 node）。" +
			"本条判据原本要验证的是：**换 driver.json 就能换后端，插件核心零改动** —— " +
			"它用一个假 sidecar（node 脚本）端到端证明 startSidecar 真的按驱动声明" +
			"拼参数/环境并把进程拉起来，没有 node 就没法验（不是被测代码有问题）。" +
			"补上：装 node、或把随包 node 放到 <插件>/rawfile/node/bin/node、" +
			"或用 settings 的 node_bin 指到真实路径。")
	}
	return bin
}
