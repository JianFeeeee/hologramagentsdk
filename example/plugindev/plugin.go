package main

// plugindev：把 SDK 的 hmapdev 工具链封装成 Agent 可调用的插件。
//
// 为什么需要它：hmapdev 是"给人和 CI 用"的命令行工具。做成插件后，Agent 能自己：
//   plugindev_init（脚手架）→ plugindev_build（构建出 .hmap）→ plugin_install（安装）→ plgreload
// 也就是"让 Agent 自己写/改/装插件"这条链不需要人来敲命令。
//
// 安全边界（都在实现里落实，不只写在描述里）：
//   - 只有 **hmapdev 一个可执行文件**会被 exec（不接受任意命令/参数拼接）；
//   - `plugindev_build` 只接受"看起来是插件工程"的目录（含 plg.json），
//     避免把一个 `hmapdev build` 变成对任意目录的操作；
//   - `plugindev_init` 生成的工程名必须满足 `[a-zA-Z0-9_-]{1,64}`（与 LLM 函数名
//     同一套约束 —— 插件名会进 `output_send__<通道>` 之类的工具名）；
//   - 所有子进程都有超时，输出截断后再返回（防止把几十 MB 构建日志灌进模型上下文）。

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/JianFeeeee/hologramagentsdk/sdk"
)

const (
	defaultBuildTimeout = 10 * time.Minute
	maxOutputChars      = 6000
)

// namePattern 与内核/上游对"会进工具名的标识符"的约束一致。
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type Plugin struct {
	name string
	sdk  *sdk.PluginSDK

	hmapdev   string        // 解析到的 hmapdev 可执行文件路径
	workspace string        // 默认工作区（生成的工程落在这里）
	timeout   time.Duration // 单次 hmapdev 调用的超时
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	p.sdk = s
	s.SetAutoRestart(true)

	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "hmapdev_path", Type: "string", DisplayName: "hmapdev 路径",
		Description: "插件开发工具链可执行文件路径。留空则按 PATH → /usr/local/bin/hmapdev → /root/go/bin/hmapdev 查找",
		Category:    p.name,
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "workspace_dir", Type: "string", DisplayName: "工程工作区",
		Description: "plugindev_init 生成工程的默认目录。留空则用 <data_dir>/plugindev",
		Category:    p.name,
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "build_timeout_sec", Default: 600, Type: "int", DisplayName: "构建超时（秒）",
		Description: "单次 hmapdev 调用的超时上限",
		Category:    p.name,
	})

	p.hmapdev = p.resolveHmapdev()
	p.workspace = p.resolveWorkspace()
	p.timeout = defaultBuildTimeout
	if v, _ := s.Settings().Get("build_timeout_sec"); v != nil {
		if n, ok := toInt(v); ok && n > 0 {
			p.timeout = time.Duration(n) * time.Second
		}
	}

	s.RegisterTool("plugindev_status", sdk.ToolDef{
		Name:        "plugindev_status",
		Description: "查看插件开发工具链状态：hmapdev 是否可用、版本、当前 SDK 版本与路径、工程工作区目录。排查\"为什么不能构建插件\"时先用它。",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	}, p.handleStatus)

	s.RegisterTool("plugindev_init", sdk.ToolDef{
		Name:        "plugindev_init",
		Description: "生成一个新的插件工程骨架（等价于 `hmapdev init <name> [--lua]`）。生成后在返回的目录里改 plugin.go，再用 plugindev_build 构建。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "插件名（也是工程目录名）：只允许字母数字下划线短横，长度 1-64。例：my_plugin",
				},
				"lang": map[string]interface{}{
					"type": "string", "description": "go（默认）或 lua",
				},
				"dir": map[string]interface{}{
					"type": "string", "description": "在哪个目录下生成（默认工作区）。必须是已存在的目录",
				},
			},
			"required": []string{"name"},
		},
	}, p.handleInit)

	s.RegisterTool("plugindev_build", sdk.ToolDef{
		Name:        "plugindev_build",
		Description: "构建并打包一个插件工程（等价于在该工程目录里执行 `hmapdev build [target]`），产物是 dist/*.hmap。构建成功后用 plugin_install 安装（本地路径）。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"dir": map[string]interface{}{
					"type": "string", "description": "插件工程目录（必须含 plg.json）",
				},
				"target": map[string]interface{}{
					"type": "string", "description": "构建目标，留空 = native（当前平台）。例：linux/amd64",
				},
			},
			"required": []string{"dir"},
		},
	}, p.handleBuild)

	s.RegisterTool("plugindev_sdk", sdk.ToolDef{
		Name:        "plugindev_sdk",
		Description: "管理插件 SDK 版本（hmapdev sdk 子命令）：list 列出已安装、current 当前版本、path 当前路径、latest 远端最新、install 安装某版本（可用 from 指定本地源码目录）、use 切换版本。构建插件报\"SDK 缺少某能力\"时用它升级 SDK。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"action": map[string]interface{}{
					"type": "string", "description": "list | current | path | latest | install | use",
				},
				"version": map[string]interface{}{
					"type": "string", "description": "install/use 的版本号，如 v1.3.0",
				},
				"from": map[string]interface{}{
					"type": "string", "description": "install 时用本地 SDK 源码目录（开发中的 SDK 用这个）",
				},
			},
			"required": []string{"action"},
		},
	}, p.handleSDK)

	s.RegisterTool("plugindev_projects", sdk.ToolDef{
		Name:        "plugindev_projects",
		Description: "列出工作区里已有的插件工程（名字、版本、是否已构建出 dist 产物），用于接续之前的开发。",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	}, p.handleProjects)

	log.Printf("[plugindev] 就绪：hmapdev=%s 工作区=%s", fallback(p.hmapdev, "(未找到)"), p.workspace)
	return nil
}

func (p *Plugin) Stop() error { return nil }

// ---------------- 工具实现 ----------------

func (p *Plugin) handleStatus(args map[string]interface{}) (interface{}, error) {
	out := map[string]interface{}{
		"hmapdev":   fallback(p.hmapdev, ""),
		"workspace": p.workspace,
	}
	if p.hmapdev == "" {
		out["available"] = false
		out["hint"] = "未找到 hmapdev。请安装：go build -o /usr/local/bin/hmapdev <sdk>/tools/hmapdev"
		return out, nil
	}
	out["available"] = true
	if txt, err := p.run(nil, ""); err == nil {
		out["version"] = strings.TrimSpace(txt)
	} else {
		out["error"] = err.Error()
	}
	if txt, err := p.run([]string{"sdk", "current"}, ""); err == nil {
		out["sdk_current"] = strings.TrimSpace(txt)
	}
	if txt, err := p.run([]string{"sdk", "path"}, ""); err == nil {
		out["sdk_path"] = strings.TrimSpace(txt)
	}
	return out, nil
}

func (p *Plugin) handleInit(args map[string]interface{}) (interface{}, error) {
	name, _ := args["name"].(string)
	name = strings.TrimSpace(name)
	if !namePattern.MatchString(name) {
		return map[string]interface{}{
			"error": "插件名只允许 [a-zA-Z0-9_-]，长度 1-64（它会进 LLM 工具名，违规会让整条请求被上游拒绝）",
		}, nil
	}
	dir, _ := args["dir"].(string)
	if dir == "" {
		dir = p.workspace
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return map[string]interface{}{"error": fmt.Sprintf("目录不存在: %s", dir)}, nil
	}
	cmd := []string{"init", name}
	if lang, _ := args["lang"].(string); strings.EqualFold(lang, "lua") {
		cmd = append(cmd, "--lua")
	}
	txt, err := p.run(cmd, dir)
	res := map[string]interface{}{"output": txt, "project_dir": filepath.Join(dir, name)}
	if err != nil {
		res["error"] = err.Error()
	}
	return res, nil
}

func (p *Plugin) handleBuild(args map[string]interface{}) (interface{}, error) {
	dir, _ := args["dir"].(string)
	if dir == "" {
		return map[string]interface{}{"error": "dir 不能为空"}, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	// 只在"插件工程"里构建：必须存在 plg.json。这样这个工具不会变成对任意目录跑构建。
	manifest := filepath.Join(abs, "plg.json")
	if _, err := os.Stat(manifest); err != nil {
		return map[string]interface{}{
			"error": fmt.Sprintf("%s 不是插件工程（缺 plg.json）；用 plugindev_init 先建一个", abs),
		}, nil
	}
	cmd := []string{"build"}
	if target, _ := args["target"].(string); strings.TrimSpace(target) != "" {
		cmd = append(cmd, strings.TrimSpace(target))
	}
	txt, runErr := p.run(cmd, abs)
	res := map[string]interface{}{"output": txt, "project_dir": abs}
	if pkgs := listHmap(filepath.Join(abs, "dist")); len(pkgs) > 0 {
		res["artifacts"] = pkgs
		res["next"] = "用 plugin_install 安装本地产物（path 指向上面 artifacts 里的 .hmap），然后 plgreload"
	}
	if runErr != nil {
		res["error"] = runErr.Error()
	}
	return res, nil
}

func (p *Plugin) handleSDK(args map[string]interface{}) (interface{}, error) {
	action, _ := args["action"].(string)
	action = strings.TrimSpace(action)
	switch action {
	case "list", "current", "path", "latest":
		txt, err := p.run([]string{"sdk", action}, "")
		res := map[string]interface{}{"output": txt}
		if err != nil {
			res["error"] = err.Error()
		}
		return res, nil
	case "install":
		version, _ := args["version"].(string)
		from, _ := args["from"].(string)
		cmd := []string{"sdk", "install"}
		if strings.TrimSpace(from) != "" {
			cmd = append(cmd, "--from", strings.TrimSpace(from))
		}
		if strings.TrimSpace(version) != "" {
			cmd = append(cmd, strings.TrimSpace(version))
		} else if strings.TrimSpace(from) == "" {
			cmd = append(cmd, "latest")
		}
		txt, err := p.run(cmd, "")
		res := map[string]interface{}{"output": txt}
		if err != nil {
			res["error"] = err.Error()
		}
		return res, nil
	case "use":
		version, _ := args["version"].(string)
		if strings.TrimSpace(version) == "" {
			return map[string]interface{}{"error": "use 需要 version"}, nil
		}
		txt, err := p.run([]string{"sdk", "use", strings.TrimSpace(version)}, "")
		res := map[string]interface{}{"output": txt}
		if err != nil {
			res["error"] = err.Error()
		}
		return res, nil
	default:
		return map[string]interface{}{"error": "action 只能是 list/current/path/latest/install/use"}, nil
	}
}

func (p *Plugin) handleProjects(args map[string]interface{}) (interface{}, error) {
	entries, err := os.ReadDir(p.workspace)
	if err != nil {
		return map[string]interface{}{"error": err.Error(), "workspace": p.workspace}, nil
	}
	var out []map[string]interface{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(p.workspace, e.Name())
		projects := []string{dir}
		// 有些工程会被生成到子目录里（hmapdev init 支持指定目录），这里只看一层
		for _, sub := range projects {
			if _, err := os.Stat(filepath.Join(sub, "plg.json")); err != nil {
				continue
			}
			item := map[string]interface{}{"name": e.Name(), "dir": sub}
			if v := readPlgVersion(filepath.Join(sub, "plg.json")); v != "" {
				item["version"] = v
			}
			if pkgs := listHmap(filepath.Join(sub, "dist")); len(pkgs) > 0 {
				item["artifacts"] = pkgs
			}
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	return map[string]interface{}{"workspace": p.workspace, "projects": out}, nil
}

// ---------------- 基础设施 ----------------

// run 执行一次 hmapdev。args 为空时执行 `hmapdev version`（用于探活）。
func (p *Plugin) run(args []string, dir string) (string, error) {
	if p.hmapdev == "" {
		return "", fmt.Errorf("未找到 hmapdev 可执行文件")
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.hmapdev, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// 继承环境（Go 工具链需要 GOCACHE/GOPATH/PATH 等）。
	out, err := cmd.CombinedOutput()
	txt := truncateOutput(string(out))
	if ctx.Err() == context.DeadlineExceeded {
		return txt, fmt.Errorf("hmapdev %s 超时（%s）", strings.Join(args, " "), p.timeout)
	}
	if err != nil {
		return txt, fmt.Errorf("hmapdev %s 失败: %v", strings.Join(args, " "), err)
	}
	return txt, nil
}

// resolveHmapdev 依次尝试：配置项 → PATH → 常见安装位置。
func (p *Plugin) resolveHmapdev() string {
	if v, _ := p.sdk.Settings().Get("hmapdev_path"); v != nil {
		if s, _ := v.(string); strings.TrimSpace(s) != "" {
			if _, err := os.Stat(strings.TrimSpace(s)); err == nil {
				return strings.TrimSpace(s)
			}
		}
	}
	if path, err := exec.LookPath("hmapdev"); err == nil {
		return path
	}
	for _, cand := range []string{"/usr/local/bin/hmapdev", "/root/go/bin/hmapdev"} {
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return ""
}

// resolveWorkspace：配置项 → <data_dir>/plugindev → ./plugindev。
func (p *Plugin) resolveWorkspace() string {
	if v, _ := p.sdk.Settings().Get("workspace_dir"); v != nil {
		if s, _ := v.(string); strings.TrimSpace(s) != "" {
			ws := strings.TrimSpace(s)
			_ = os.MkdirAll(ws, 0o755)
			return ws
		}
	}
	if v, err := p.sdk.Settings().GetCore("core.daemon.data_dir"); err == nil {
		if dd, _ := v.(string); dd != "" {
			ws := filepath.Join(dd, "plugindev")
			_ = os.MkdirAll(ws, 0o755)
			return ws
		}
	}
	ws := "plugindev"
	_ = os.MkdirAll(ws, 0o755)
	return ws
}

// truncateOutput 截断长输出：构建日志动辄几百 KB，直接返回会灌爆模型上下文。
func truncateOutput(s string) string {
	if len(s) <= maxOutputChars {
		return s
	}
	head := s[:maxOutputChars/2]
	tail := s[len(s)-maxOutputChars/2:]
	return fmt.Sprintf("%s\n…（输出被截断，共 %d 字节）…\n%s", head, len(s), tail)
}

// listHmap 列出目录下的 .hmap 产物（按名字排序，稳定输出）。
func listHmap(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".hmap") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out
}

func readPlgVersion(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := string(b)
	i := strings.Index(s, `"version"`)
	if i < 0 {
		return ""
	}
	rest := s[i:]
	j := strings.Index(rest, ":")
	if j < 0 {
		return ""
	}
	rest = strings.TrimSpace(rest[j+1:])
	rest = strings.TrimPrefix(rest, `"`)
	if k := strings.Index(rest, `"`); k > 0 {
		return rest[:k]
	}
	return ""
}

func toInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

func fallback(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
