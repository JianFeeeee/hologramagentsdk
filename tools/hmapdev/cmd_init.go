package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"
)

func (p *PlgConfig) ReplacesToSlice() []string {
	var s []string
	for from, to := range p.Replaces {
		s = append(s, from+"="+to)
	}
	sort.Strings(s) // deterministic order
	return s
}

// validateProxies 在打包前校验反代声明，让插件作者**本地**就发现写错，
// 而不是装到 HologramAgent 上才看到「声明被拒」。
//
// 校验规则与 SDK 的 sdk.ValidateProxyDef 保持一致（同一套 DNS label / auth /
// target 规则）；工具链不 import SDK 是为了保持"打包机只需工具链"的独立性，
// 两侧一致性由 SDK 仓与主仓的同名测试分别钉住。
func validateProxies(list []ProxyConfig) error {
	seen := map[string]bool{}
	for i, p := range list {
		if strings.TrimSpace(p.Target) == "" {
			return fmt.Errorf("proxies[%d] (%s): target 不能为空", i, p.Name)
		}
		// "homeagent" 是**协议值**，不是品牌文案：存量插件的 plg.json 里写的就是它，
		// 核心仓以 pubsdk.ProxyAuthHomeAgent 别名引用同一常量。改它 = 存量插件静默失效。
		switch p.Auth {
		case "", "homeagent", "none":
		default:
			return fmt.Errorf("proxies[%d] (%s): auth 只允许 \"\"/\"homeagent\"/\"none\"，得到 %q", i, p.Name, p.Auth)
		}
		if p.Host != "" {
			if !validHostLabel(p.Host) {
				return fmt.Errorf("proxies[%d] (%s): host %q 不是合法子域名标签（小写字母/数字/连字符，不以连字符开头结尾，≤63）", i, p.Name, p.Host)
			}
			if seen[p.Host] {
				return fmt.Errorf("proxies[%d]: host %q 在同一声明里重复", i, p.Host)
			}
			seen[p.Host] = true
		}
		// 端口必须是数字：SplitHostPort 不校验数字，"host:abc" 会溜过去
		raw := p.Target
		if j := strings.Index(raw, "://"); j >= 0 {
			raw = raw[j+3:]
		}
		if j := strings.IndexByte(raw, '/'); j >= 0 {
			raw = raw[:j]
		}
		if h, port, err := net.SplitHostPort(raw); err == nil {
			if h == "" {
				return fmt.Errorf("proxies[%d] (%s): target %q 缺少主机", i, p.Name, p.Target)
			}
			if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
				return fmt.Errorf("proxies[%d] (%s): target 端口非法（应为 1-65535）: %q", i, p.Name, p.Target)
			}
		}
	}
	return nil
}

func validHostLabel(s string) bool {
	if s == "" || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

type PlgConfig struct {
	Name        string            `json:"name"`
	NameZh      string            `json:"name_zh"`
	NameEn      string            `json:"name_en"`
	Version     string            `json:"version"`
	Description string            `json:"description"`
	Author      string            `json:"author"`
	Entry       string            `json:"entry"`
	Tags        []string          `json:"tags"`
	Targets     string            `json:"targets"`
	OutDir      string            `json:"outdir,omitempty"`
	Bundle      *bool             `json:"bundle,omitempty"`
	SDKPath     string            `json:"sdk_path,omitempty"`
	GoVersion   string            `json:"go_version,omitempty"`
	Replaces    map[string]string `json:"replaces,omitempty"`
	SourceDirs  []string          `json:"source_dirs,omitempty"`

	// SDK 声明本插件针对的 SDK **接口版本**（中版本或完整版本，如 "1.2" / "1.2.1"）。
	//
	// 为何需要：工具链存储里可能装有多个 SDK 版本，而插件产物与内核是协议绑定的——
	// 不给声明就只能猜（旧行为是直接用 current：谁改过 current 就拿谁的版本编，
	// 出错时表现为莫名其妙的编译错误）。写中版本表示「只要 1.2 这条接口线，
	// 补丁由工具链挑最新」（patch 只含工具链/打包修复，接口不变，见 README 版本语义）。
	SDK string `json:"sdk,omitempty"`

	// Proxies 声明本插件需要 HologramAgent 反代出去的服务（自带 Web UI / HTTP API）。
	//
	// 为什么声明在 plugin.json 而不是运行期注册：静态可发现（插件没起来时
	// 也能报「声明了 ui 但目标不可达」，而不是静默 404）、可版本化、旧内核无害。
	// 字段语义见 SDK 的 sdk.ProxyDef（工具链与内核共用同一套校验规则）。
	Proxies []ProxyConfig `json:"proxies,omitempty"`

	// RawFiles 声明要一并打包的**非代码资源**（见 rawfiles.go）。
	//
	// 工程根的 `rawfile/` 目录**默认全收**，不需要在这里声明；
	// 本数组用于**额外**的来源。解包后统一落在 <插件目录>/rawfile/ 下。
	//
	// 两种来源（字段细节见 rawfiles.go 的 RawFileDecl）：
	//
	//	"assets/theme.css"                          工程内的本地路径（旧写法）
	//	{ "from": "assets", "exclude": ["assets/tmp"] }
	//	{ "name": "node/bin/node",                   包内路径（相对 rawfile/）
	//	  "url": "https://host/node-linux-x64",      外部下载（与 from 互斥）
	//	  "sha256": "…64 hex…",                      ★ 必填，不符即构建失败
	//	  "os": "linux", "arch": "amd64" }           可选：按构建目标过滤
	//	{ "name": "runtime", "url": "https://host/rt.tgz", "sha256": "…",
	//	  "format": "tar.gz", "strip_prefix": "package/" }  归档在**构建期**展开
	//
	// 为什么需要 url 来源：`rawfile/` 里的东西不进版本库（上百兆二进制进 git
	// 会拖慢克隆且每次升级产生巨型 diff），于是干净克隆打出的包缺 sidecar/
	// 缺内嵌 node。声明式外部依赖让「克隆就能构建」与「不把二进制塞进 git」
	// 同时成立。下载产物按 sha256 缓存在
	// ${HOMEAGENT_RAWFILE_CACHE} 或 ~/.homeagent/hmapdev/cache/rawfiles/。
	RawFiles []RawFileDecl `json:"rawfiles,omitempty"`

	// ResolvedSDK 是本次构建实际选中的 SDK 版本（build 按 SDK 声明解析后回填），
	// 只写进产物里的 plugin.json，便于事后追溯「这个 .hmap 是哪版 SDK 编的」。
	ResolvedSDK string `json:"-"`

	// RequiresFacilities 声明本插件需要**内核哪些设施就绪**后才能启动。
	//
	// 取值见 sdk.KnownFacilities()（如 FacilityConfig / FacilityLLM / FacilityStageHost）。
	// **不声明** ⇒ 等内核完全初始化（默认；与旧行为一致）。
	//
	// ★ 为什么必须在 PlgConfig 里声明、且必须写进产物 plugin.json：
	// 内核靠读 plugin.json 的该字段决定**何时拉起插件**。工具链若不透传，
	// 插件作者在 plg.json 里写了也**不会进包** —— 而且是**静默丢失**
	// （插件照常装上、照常运行，只是启动时机与作者预期不符，无任何提示）。
	// 同理见 proxies 字段的注释（同一类"漏透传"坑）。
	//
	// ★ 打包期校验：值必须是 sdk.KnownFacilities() 里的名字。
	// 拼错在**打包时就报错**，比运行期发现早得多（运行期发现意味着用户
	// 已经装上了那个**永不启动**的插件）。
	RequiresFacilities []string `json:"requires_facilities,omitempty"`

	// AutoStart 声明是否**伴随内核启动**（缺省 true）还是等显式启用（false）。
	//
	// 用 *bool 而非 bool：必须区分"未声明"与"显式 false"。
	//   缺省(nil) ⇒ true（伴随启动，与旧行为一致）
	//   显式 false ⇒ 不自动拉起，等 agent 经 plugin_enable 启用
	// 若用普通 bool，零值是 false ⇒ "没写这个字段"会被当成"不自动启动"
	// ⇒ 存量插件全部不启动（灾难性）。
	AutoStart *bool `json:"auto_start,omitempty"`

	// Repo 是本插件**源码仓库**地址（如 https://github.com/HologramAgentPLG/qq）。
	//
	// 用途：内核的 plugin_update 靠它「从仓库拉最新源码 → 现场编译 .hmap → 安装」。
	// 没有它，plugin_update 只能说"该插件未声明仓库"，无法升级。
	//
	// ★ 必须透传进产物 plugin.json：内核读的是**装好的 plugin.json**，
	// 而不是仓库里的 plg.json（安装后源码已不在机器上）。漏透传的话，
	// 插件作者写了 repo、plugin_update 却永远查不到 —— 静默失效。
	//
	// 留空表示"仓库未知/不提供自动更新"（如私有插件、内置插件）。
	Repo string `json:"repo,omitempty"`
}

// TargetList parses the Targets string into a slice.
func (p *PlgConfig) TargetList() []string { return parseTargets(p.Targets) }

// BundleDefault returns true if bundle mode is not explicitly disabled.
func (p *PlgConfig) BundleDefault() bool { return p.Bundle == nil || *p.Bundle }

// OutDirDefault returns the output directory, defaulting to "dist".
func (p *PlgConfig) OutDirDefault() string {
	if p.OutDir != "" {
		return p.OutDir
	}
	return "dist"
}

type TemplateData struct {
	Plg   PlgConfig
	IsLua bool

	// Go module info (for go.mod)
	ModulePath string
	GoVersion  string
	SDKModule  string
	SDKVersion string

	// SDKLocalPath 是本机 SDK 源码绝对路径，写入生成的 go.mod 作为 replace 目标。
	//
	// 为何必须写：gitcode 的模块不在 proxy.golang.org 上，只 require 一个
	// 版本号的 go.mod 配上缺失的 go.sum，新用户第一次 `hmapdev build`
	// 必定死在 "missing go.sum entry"，而 `go mod tidy` 又会去公共 proxy 拉
	// 一个不存在的条目。有了本地 replace，go 完全不需要 go.sum 条目。
	SDKLocalPath string
}

func cmdInit(args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: hmapdev init <name> [--lua] [--type remotedevice]")
		os.Exit(1)
	}

	name := args[0]
	isLua := false
	isRemoteDevice := false
	for _, a := range args[1:] {
		switch a {
		case "--lua":
			isLua = true
		case "--type", "-t":
			// handled in next iteration
		}
	}
	// also check --type remotedevice as a single arg
	for i, a := range args[1:] {
		if a == "--type" || a == "-t" {
			if i+1 < len(args[1:]) {
				if args[1:][i+1] == "remotedevice" {
					isRemoteDevice = true
				}
			}
		}
		if a == "--type=remotedevice" || a == "-t=remotedevice" {
			isRemoteDevice = true
		}
	}

	if isRemoteDevice && isLua {
		fmt.Println("error: --type remotedevice and --lua are mutually exclusive")
		os.Exit(1)
	}

	// Remote device projects use different scaffold
	if isRemoteDevice {
		scaffoldRemoteDevice(name)
		return
	}

	dir := name
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		fmt.Printf("error: directory %q already exists\n", dir)
		os.Exit(1)
	}

	// Go 插件统一产出 plugin.bin（v1.0.0 子进程模式）。
	//
	// 此前这里写 "plugin.so"，scaffold 出来的 plg.json 就带着一个已退场的
	// entry 值，新手跟着模板走会误以为自己在做 C ABI 插件。
	// build 实际不看这个值（只用它区分 Lua），但模板不应误导。
	entry := "plugin.bin"
	var targets string
	if isLua {
		entry = "main.lua"
		targets = "lua"
	} else {
		targets = "linux/amd64,windows/amd64"
	}

	nameEn := strings.ReplaceAll(name, "-", " ")
	nameEn = strings.Title(nameEn)

	data := TemplateData{
		Plg: PlgConfig{
			Name:        name,
			NameZh:      "中文名",
			NameEn:      nameEn,
			Version:     "0.1.0",
			Description: name + " plugin",
			Author:      "HologramAgent",
			Entry:       entry,
			Tags:        []string{name},
			Targets:     targets,
		},
		IsLua: isLua,
	}

	// Detect SDK info for Go plugin go.mod.
	// 生成的 go.mod 除 require 外还写一条指向本机 SDK 的 replace：
	// 否则 scaffold 出来的项目第一次 build 必定失败（详见 SDKLocalPath 注释）。
	if isLua {
		// Lua 插件也要记录它按哪版 SDK 语义编写：Lua `sdk.*` 是公开契约，
		// 与内核能力版本挂钩；不写版本就只能靠“调用时才发现是 nil”。
		if root := tryActiveSDKRoot(); root != "" {
			data.Plg.SDK = normalizeSDKVersion(readMetaVersion(root))
		}
	} else {
		sdkMod, goVer, sdkRoot, sdkVer := detectSDKInfo()
		data.ModulePath = name
		data.GoVersion = goVer
		data.SDKModule = sdkMod
		data.SDKVersion = "v" + sdkVer
		data.SDKLocalPath = strings.ReplaceAll(sdkRoot, "\\", "/")
		// 声明**完整版本号**：SDK 版本跟随内核中版本、patch 位恒为 .0，
		// 一条内核线只对应一个 SDK 版本（build 时按此解析，见 ResolveSDKForProject）。
		data.Plg.SDK = normalizeSDKVersion(sdkVer)
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Printf("error: create dir: %v\n", err)
		os.Exit(1)
	}

	// write plg.json
	writeTemplate(filepath.Join(dir, "plg.json"), tmplPlgJSON, data)

	// Lua plugins get main.lua + sdk.lua; Go plugins get plugin.go only
	if isLua {
		writeTemplate(filepath.Join(dir, "main.lua"), tmplMainLua, data)
		// sdk.lua 是给 `lua main.lua` 离线测试用的 mock，单一事实源在 SDK 仓的
		// sdk/lua/sdk.lua；优先从当前激活的 SDK 拷，拷不到才回退内嵌模板。
		if !copyCanonicalLuaSDK(dir) {
			if err := os.WriteFile(filepath.Join(dir, "sdk.lua"), []byte(fallbackLuaSDK), 0644); err != nil {
				fmt.Printf("error: write sdk.lua: %v\n", err)
				os.Exit(1)
			}
		}
	} else {
		writeTemplate(filepath.Join(dir, "plugin.go"), tmplPluginGo, data)
	}

	// write README.md
	writeTemplate(filepath.Join(dir, "README.md"), tmplReadme, data)

	// write go.mod for Go plugins
	if !isLua {
		writeTemplate(filepath.Join(dir, "go.mod"), tmplGoMod, data)
	}

	// create thirdpart directory for external library sources
	os.MkdirAll(filepath.Join(dir, "thirdpart"), 0755)

	// create rawfile directory：**默认打包目录**。
	// 放这里的东西会被 hmapdev build 自动打进 .hmap，
	// 解包后落在 <插件目录>/rawfile/ 下（见 tools/hmapdev/rawfiles.go）。
	// 留一个 .gitkeep 让空目录也能进版本库。
	rawDir := filepath.Join(dir, "rawfile")
	os.MkdirAll(rawDir, 0755)
	os.WriteFile(filepath.Join(rawDir, ".gitkeep"), []byte(""), 0644)
	// ★ 自带的 .gitignore：rawfile/ 里放的是随包资源（可能内嵌 node/pi
	// 等上百兆二进制），由构建方自行准备，不能进版本库 ——
	// 一个克隆带上 300MB 二进制、每次升级产生巨型 diff。
	// 规则放在目录内部（自包含），不依赖上层 .gitignore 的否定式语义。
	os.WriteFile(filepath.Join(rawDir, ".gitignore"), []byte(
		"# rawfile/ 里的是随包资源，由构建方自行准备（下载/拷贝），不进版本库。\n"+
			"*\n!.gitignore\n!.gitkeep\n"), 0644)

	fmt.Printf("Created plugin project %q (%s)\n", dir, entry)
	if isLua {
		fmt.Printf("  cd %s && lua main.lua  (standalone test)\n", dir)
	}
	fmt.Printf("  cd %s && hmapdev build\n", dir)
}

// activeSDKRoot 返回当前激活 SDK 的根目录，未安装/未激活则报错退出。
//
// 与 activeSDKRoot（fatal 版）区别：这里只探测，不退出。
// Lua 插件的 mock 是“锦上添花”，没装 SDK 不应该阻断 init。
func tryActiveSDKRoot() string {
	store := sdkStore()
	current := resolveCurrentVersion(store)
	if current == "" {
		return ""
	}
	root := sdkVersionDir(current)
	if _, err := os.Stat(root); err != nil {
		return ""
	}
	return root
}

// copyCanonicalLuaSDK 把激活 SDK 的 sdk/lua/sdk.lua 拷进新项目。
// 三份 sdk.lua（内核内嵌 / 工具链模板 / 项目副本）各自漂移是本工具链的历史债，
// 单一事实源在 SDK 仓，工具链只负责搬运。返回是否成功。
func copyCanonicalLuaSDK(dir string) bool {
	root := tryActiveSDKRoot()
	if root == "" {
		return false
	}
	src := filepath.Join(root, "sdk", "lua", "sdk.lua")
	data, err := os.ReadFile(src)
	if err != nil {
		return false
	}
	if err := os.WriteFile(filepath.Join(dir, "sdk.lua"), data, 0644); err != nil {
		return false
	}
	fmt.Printf("  sdk.lua <- %s\n", src)
	return true
}

// detectSDKInfo reads the HologramAgent SDK's go.mod and meta to get module path, go version, and SDK version.
func detectSDKInfo() (modulePath, goVersion, sdkPath, sdkVersion string) {
	root := activeSDKRoot()
	gomodPath := filepath.Join(root, "go.mod")
	data, err := os.ReadFile(gomodPath)
	if err != nil {
		fmt.Printf("error: cannot read SDK go.mod at %s: %v\n", gomodPath, err)
		os.Exit(1)
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			modulePath = strings.TrimSpace(line[7:])
		}
		if strings.HasPrefix(line, "go ") {
			goVersion = strings.TrimSpace(line[3:])
		}
	}
	if modulePath == "" {
		fmt.Printf("error: no module directive in %s\n", gomodPath)
		os.Exit(1)
	}
	if goVersion == "" {
		goVersion = "1.21"
	}
	sdkVersion = readMetaVersion(root)
	return modulePath, goVersion, root, sdkVersion
}

// scaffoldRemoteDevice 创建远程设备适配器项目脚手架
func scaffoldRemoteDevice(name string) {
	dir := name
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		fmt.Printf("error: directory %q already exists\n", dir)
		os.Exit(1)
	}

	nameEn := strings.Title(strings.ReplaceAll(name, "-", " "))

	data := TemplateData{
		Plg: PlgConfig{
			Name:        name,
			NameZh:      "中文名",
			NameEn:      nameEn,
			Version:     "0.1.0",
			Description: name + " remote device adapter",
			Author:      "HologramAgent",
			Entry:       name,
			Tags:        []string{name, "remotedevice"},
		},
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Printf("error: create dir: %v\n", err)
		os.Exit(1)
	}

	// 写入 main.c
	writeTemplate(filepath.Join(dir, "main.c"), tmplRemoteDeviceMain, data)

	// 写入 CMakeLists.txt
	writeTemplate(filepath.Join(dir, "CMakeLists.txt"), tmplRemoteDeviceCMake, data)

	// 创建 SDK 目录（symlink/copy）
	sdkSrc := filepath.Join("..", "remotedevice")
	sdkDst := filepath.Join(dir, "ha_remotedevice")
	if _, err := os.Stat(sdkDst); os.IsNotExist(err) {
		// 尝试创建符号链接，失败则提示
		if err := os.Symlink(sdkSrc, sdkDst); err != nil {
			fmt.Printf("  note: could not create symlink to SDK, copy manually:\n")
			fmt.Printf("    cp -r %s %s\n", sdkSrc, sdkDst)
		}
	}

	fmt.Printf("Created remote device adapter project %q\n", dir)
	fmt.Printf("  cd %s && mkdir build && cd build && cmake .. && make\n", dir)
	fmt.Printf("  Or include as subdirectory in your project:\n")
	fmt.Printf("    add_subdirectory(%s)\n", dir)
}

func writeTemplate(path, content string, data TemplateData) {
	tmpl, err := template.New("").Parse(content)
	if err != nil {
		fmt.Printf("error: parse template: %v\n", err)
		os.Exit(1)
	}
	f, err := os.Create(path)
	if err != nil {
		fmt.Printf("error: create %s: %v\n", path, err)
		os.Exit(1)
	}
	defer f.Close()
	if err := tmpl.Execute(f, data); err != nil {
		fmt.Printf("error: execute template: %v\n", err)
		os.Exit(1)
	}
}
