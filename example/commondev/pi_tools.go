package main

// pi 配置面工具补全（审计发现项）。
//
// 设计 §3.6.4 明确：**插件是 pi 配置系统的受控入口** —— 写的就是 pi 自己的
// 配置文件，不造中间层。§3.6.3 已定的落地方式：
//
//	<agentdir>/models.json    providers.<name>.{baseUrl,api,apiKey,models[]}
//	<agentdir>/auth.json      provider 凭据
//	<agentdir>/settings.json  defaultProvider / defaultModel
//
// 四条实现纪律（§3.6.4）：
//  1. 写的就是 pi 的配置文件，格式与 pi 自己写出来的一致
//     （否则 pi 重写文件时会把我们的字段抹掉）
//  2. **改前备份、改后校验**：json 写坏会让内层 pi 起不来；
//     校验失败则**回滚并响亮报错**
//  3. key 不进日志、不进返回值
//  4. **不改用户全局**：一律带 PI_CODING_AGENT_DIR

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// loadPiModels 读 pi 的 models.json（不存在则返回空结构）。
func loadPiModels(agentDir string) (*piModelsFile, error) {
	out := &piModelsFile{Providers: map[string]*piProvider{}}
	b, err := os.ReadFile(filepath.Join(agentDir, "models.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, out); err != nil {
		// 坏文件必须响亮报错：静默当空会**抹掉用户已有的源**
		return nil, fmt.Errorf("models.json 已损坏（拒绝覆盖，先人工修复）: %w", err)
	}
	if out.Providers == nil {
		out.Providers = map[string]*piProvider{}
	}
	return out, nil
}

// loadPiAuth 读 pi 的 auth.json（不存在则返回空）。
//
// ★ 用 map[string]interface{} 而不是 map[string]map[string]string：
// 后者把每个 provider 的 entry 当成**扁平字符串表**，于是只能写出
// `{"apiKey": "..."}` —— 而 pi 认的是 `{"type":"api_key","key":"..."}`
// （权威定义见 pi 的 dist/core/auth-storage.js:
//
//	`if (value.type === "api_key")` 与 model-runtime.js 的
//	`{ type: "api_key", key: apiKey }`）。
//
// 格式不符的后果是**完全静默**的：pi 的 auth 读取返回空 ⇒
// `Provider is not configured: <name>`，而配置看起来完全正常。
// 实测（2026-10-15）：内嵌 pi 因此谁都调不动，只改这个格式即恢复。
func loadPiAuth(agentDir string) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	b, err := os.ReadFile(filepath.Join(agentDir, "auth.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("auth.json 已损坏（拒绝覆盖）: %w", err)
	}
	return out, nil
}

// handlePiSourceAdd 添加/更新一个 LLM 源（直接写 pi 的配置文件）。
//
// 纪律 2：**先备份**，写完回读校验，失败回滚。
func (p *Plugin) handlePiSourceAdd(args map[string]interface{}) (interface{}, error) {
	name := strings.TrimSpace(str(args, "name"))
	baseURL := strings.TrimSpace(str(args, "base_url"))
	model := strings.TrimSpace(str(args, "model"))
	if name == "" || baseURL == "" || model == "" {
		return map[string]interface{}{"error": "name、base_url、model 必填"}, nil
	}
	if !validName(name) {
		return map[string]interface{}{"error": "name 只允许 [A-Za-z0-9_-]（会成为 JSON 的键名）"}, nil
	}
	adapter := strings.TrimSpace(str(args, "adapter"))
	api := adapterToAPI(adapter)
	if api == "" {
		return map[string]interface{}{
			"error": fmt.Sprintf("adapter=%q 无法映射到 pi 的 api（已知：openai/anthropic）——不猜默认值", adapter),
		}, nil
	}

	agentDir := p.piAgentDir()
	backup, err := backupPiConfig(agentDir)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}

	models, err := loadPiModels(agentDir)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	auth, err := loadPiAuth(agentDir)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}

	apiKey := str(args, "api_key")
	m := piModel{ID: model, Name: model, Input: []string{"text"}}
	if cw := int(numArg(args, "context_window")); cw > 0 {
		m.ContextWindow = cw
	}
	if mt := int(numArg(args, "max_tokens")); mt > 0 {
		m.MaxTokens = mt
	}
	if v, ok := args["vision"].(bool); ok && v {
		m.Input = []string{"text", "image"}
	}
	if rb, ok := args["reasoning"].(bool); ok && rb {
		m.Reasoning = true
	}
	models.Providers[name] = &piProvider{
		BaseURL: baseURL, API: api, APIKey: apiKey, Models: []piModel{m},
	}
	if strings.TrimSpace(apiKey) != "" {
		if auth == nil {
			auth = map[string]interface{}{}
		}
		auth[name] = newPiAuthEntry(apiKey)
	}

	if err := p.writePiConfigs(agentDir, models, auth); err != nil {
		rollbackPiConfig(agentDir, backup)
		return map[string]interface{}{"error": "写入失败，已回滚: " + err.Error()}, nil
	}
	// ★ 返回值里**没有 key**（纪律 3）
	return map[string]interface{}{
		"status": "ok", "provider": name, "base_url": baseURL,
		"api": api, "model": model,
		"key_set": strings.TrimSpace(apiKey) != "",
		"backup":  backup,
	}, nil
}

// handlePiConfigGet 读取 pi 的配置（**key 一律脱敏**，纪律 3）。
//
// scope: plugin（默认，读 <agentdir>）| project（读 <workdir>/.pi）。
func (p *Plugin) handlePiConfigGet(args map[string]interface{}) (interface{}, error) {
	agentDir, err := p.piScopeDir(args)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	models, err := loadPiModels(agentDir)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	providers := []map[string]interface{}{}
	names := make([]string, 0, len(models.Providers))
	for n := range models.Providers {
		names = append(names, n)
	}
	sort.Strings(names) // 固定顺序，便于对照
	for _, n := range names {
		pv := models.Providers[n]
		modelsOut := make([]string, 0, len(pv.Models))
		for _, m := range pv.Models {
			modelsOut = append(modelsOut, m.ID)
		}
		providers = append(providers, map[string]interface{}{
			"name": n, "base_url": pv.BaseURL, "api": pv.API,
			"models": modelsOut,
			// ★ 只报"是否配了 key"，不返回值
			"key_set": strings.TrimSpace(pv.APIKey) != "",
		})
	}
	var settings map[string]interface{}
	if b, err := os.ReadFile(filepath.Join(agentDir, "settings.json")); err == nil {
		_ = json.Unmarshal(b, &settings)
	}
	out := map[string]interface{}{
		"scope_dir":      agentDir,
		"providers":      providers,
		"count":          len(providers),
		"settings":       settings,
		"key_discipline": "api_key 绝不返回（与 commondev_account_list 同一条纪律）",
	}
	return out, nil
}

// handlePiPackageInstall 为内置 pi 装一个扩展包（pi install）。
//
// 设计 §3.5.2 的两种粒度：plugin（全局，装进 agentdir）/ project（装进工作目录）。
// ★ 不擅自安装：命令与来源都显式要求，且安装失败要响亮报错。
func (p *Plugin) handlePiPackageInstall(args map[string]interface{}) (interface{}, error) {
	source := strings.TrimSpace(str(args, "source"))
	if source == "" {
		return map[string]interface{}{"error": "source 必填（如 npm:@foo/bar、git:github.com/u/r）"}, nil
	}
	scope := strings.TrimSpace(str(args, "scope"))
	if scope == "" {
		scope = "plugin"
	}
	agentDir, err := p.piScopeDir(map[string]interface{}{
		"scope": scope, "workdir": str(args, "workdir"),
	})
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	piBin := p.piBin()

	argv := []string{"install", source}
	dir := agentDir
	if scope == "project" {
		argv = append(argv, "-l") // pi 的 project-local 开关
		dir = strings.TrimSpace(str(args, "workdir"))
		if dir == "" {
			return map[string]interface{}{"error": "scope=project 时必须给 workdir"}, nil
		}
	}

	cmd := exec.Command(piBin, argv...)
	cmd.Dir = dir
	// ★ 纪律 4：绝不改用户全局（一律指向插件自己的 agentdir）
	cmd.Env = append(os.Environ(), "PI_CODING_AGENT_DIR="+agentDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// 不吞输出：pi 的报错（来源不存在、网络不通）是唯一线索
		return map[string]interface{}{
			"status": "failed", "error": err.Error(),
			"output": truncate(string(out), 800),
		}, nil
	}
	return map[string]interface{}{
		"status": "ok", "source": source, "scope": scope,
		"dir": dir, "output": truncate(string(out), 800),
	}, nil
}

// piScopeDir 解析 scope 到具体目录（纪律 4：带显式 agentdir）。
func (p *Plugin) piScopeDir(args map[string]interface{}) (string, error) {
	scope := strings.TrimSpace(str(args, "scope"))
	if scope == "" {
		scope = "plugin"
	}
	switch scope {
	case "plugin":
		return p.piAgentDir(), nil
	case "project":
		wd := strings.TrimSpace(str(args, "workdir"))
		if wd == "" {
			return "", fmt.Errorf("scope=project 时必须给 workdir")
		}
		abs, err := filepath.Abs(wd)
		if err != nil {
			return "", err
		}
		if !p.isInsideProgramRoot(abs) {
			return "", fmt.Errorf("project scope 的 workdir 必须在 program/ 工程目录下")
		}
		return filepath.Join(abs, ".pi"), nil
	default:
		return "", fmt.Errorf("scope 只能是 plugin | project")
	}
}

// piBin 定位内置 pi 可执行文件。
func (p *Plugin) piBin() string {
	if v, _ := p.sdk.Settings().Get("pi_bin"); v != nil {
		if s, _ := v.(string); strings.TrimSpace(s) != "" {
			return s
		}
	}
	if cand := filepath.Join(filepath.Dir(p.path), "runtime", "pi"); fileExists(cand) {
		return cand
	}
	if cand, err := exec.LookPath("pi"); err == nil {
		return cand
	}
	return "pi" // 交给 exec 报"找不到"
}

// ---------------- 改前备份 / 失败回滚（纪律 2） ----------------

// backupPiConfig 把 agentDir 下的 pi 配置文件备份成一个时间戳快照。
func backupPiConfig(agentDir string) (string, error) {
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	dir := filepath.Join(agentDir, ".backup-"+stamp)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	n := 0
	for _, f := range []string{"models.json", "auth.json", "settings.json"} {
		src := filepath.Join(agentDir, f)
		b, err := os.ReadFile(src)
		if err != nil {
			continue // 不存在就不用备份
		}
		if err := writeFile0600(filepath.Join(dir, f), b); err != nil {
			return "", err
		}
		n++
	}
	if n == 0 {
		// 没什么可备份：建一个空目录标记（回滚时删除新文件即可）
		return dir, nil
	}
	return dir, nil
}

// rollbackPiConfig 用备份覆盖回去（写坏时的**响亮**兜底）。
func rollbackPiConfig(agentDir, backupDir string) {
	if backupDir == "" {
		return
	}
	for _, f := range []string{"models.json", "auth.json", "settings.json"} {
		src := filepath.Join(backupDir, f)
		b, err := os.ReadFile(src)
		if err != nil {
			// 备份里没有 ⇒ 该文件是本次新建的，删掉即回到原状
			_ = os.Remove(filepath.Join(agentDir, f))
			continue
		}
		_ = writeFile0600(filepath.Join(agentDir, f), b)
	}
}

// handlePiPluginList 列出已装的 pi 扩展包（分 scope）。
func (p *Plugin) handlePiPluginList(args map[string]interface{}) (interface{}, error) {
	scope := strings.TrimSpace(str(args, "scope"))
	agentDir := p.piAgentDir()
	argv := []string{"list"}
	dir := agentDir
	env := os.Environ()
	if scope == "project" {
		wd := strings.TrimSpace(str(args, "workdir"))
		if wd == "" {
			return map[string]interface{}{"error": "scope=project 时必须给 workdir"}, nil
		}
		if !p.isInsideProgramRoot(wd) {
			return map[string]interface{}{"error": "workdir 必须在 program/ 工程目录下"}, nil
		}
		dir = wd
		argv = append(argv, "-l")
	}
	cmd := exec.Command(p.piBin(), argv...)
	cmd.Dir = dir
	cmd.Env = append(env, "PI_CODING_AGENT_DIR="+agentDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return map[string]interface{}{
			"status": "failed", "error": err.Error(),
			"output": truncate(stripPiNoise(string(out)), 800),
		}, nil
	}
	text := strings.TrimSpace(stripPiNoise(string(out)))
	return map[string]interface{}{
		"status": "ok", "scope": scopeOrDefault(scope), "dir": dir,
		"packages": text, "count": countPiListLines(text),
	}, nil
}

// handlePiPluginRemove 卸载一个 pi 扩展包。
func (p *Plugin) handlePiPluginRemove(args map[string]interface{}) (interface{}, error) {
	source := strings.TrimSpace(str(args, "source"))
	if source == "" {
		return map[string]interface{}{"error": "source 必填"}, nil
	}
	agentDir := p.piAgentDir()
	argv := []string{"remove", source}
	dir := agentDir
	if strings.EqualFold(strings.TrimSpace(str(args, "scope")), "project") {
		wd := strings.TrimSpace(str(args, "workdir"))
		if wd == "" {
			return map[string]interface{}{"error": "scope=project 时必须给 workdir"}, nil
		}
		if !p.isInsideProgramRoot(wd) {
			return map[string]interface{}{"error": "workdir 必须在 program/ 工程目录下"}, nil
		}
		dir = wd
		argv = append(argv, "-l")
	}
	cmd := exec.Command(p.piBin(), argv...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PI_CODING_AGENT_DIR="+agentDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return map[string]interface{}{
			"status": "failed", "error": err.Error(),
			"output": truncate(stripPiNoise(string(out)), 800),
		}, nil
	}
	return map[string]interface{}{
		"status": "ok", "source": source, "dir": dir,
		"output": truncate(stripPiNoise(string(out)), 800),
	}, nil
}

// stripPiNoise 去掉 pi 启动时那两行 A2A/ACP 端口告警。
//
// 为何要滤：那两行是**环境噪声**（内层 pi 尝试监听端口被占），
// 混在结果里会让人以为安装/列出失败了。真实的错误信息不受影响。
func stripPiNoise(s string) string {
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "[pi-a2a]") ||
			strings.HasPrefix(strings.TrimSpace(line), "[pi-acp]") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

func scopeOrDefault(s string) string {
	if strings.TrimSpace(s) == "" {
		return "plugin"
	}
	return s
}

// countPiListLines 数出已装包的行数（"No packages installed." 记 0）。
func countPiListLines(text string) int {
	if strings.TrimSpace(text) == "" || strings.Contains(text, "No packages installed") {
		return 0
	}
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// ---------------- workdir 预准备（设计 §4.1） ----------------

// handleWorkdirPrepare 重新准备/修复某条分支的工作目录（幂等）。
//
// 与 branch_open 的区别：branch_open 是**登记一条新线**（含唯一性闸）；
// 本工具是对**已有的线**重做目录准备 —— 目录被误删、clone 中断、需要
// 重新从 mirror 派生时用。走后台作业（clone 可能超过 60s）。
func (p *Plugin) handleWorkdirPrepare(args map[string]interface{}) (interface{}, error) {
	id := strings.TrimSpace(str(args, "branch_id"))
	if id == "" {
		return map[string]interface{}{"error": "branch_id 必填"}, nil
	}
	p.mu.Lock()
	b, ok := p.ledger.Branches[id]
	var proj *Project
	if ok {
		proj = p.ledger.Projects[b.ProjectID]
	}
	p.mu.Unlock()
	if !ok || proj == nil {
		return map[string]interface{}{"error": fmt.Sprintf("工程对话 %s 或其项目不存在", id)}, nil
	}
	jobID := fmt.Sprintf("prepare-%s-%d", id, time.Now().UnixNano()%100000)
	p.mu.Lock()
	p.ledger.Jobs[jobID] = &Job{
		ID: jobID, BranchID: id, Kind: "prepare_workdir",
		Status: "pending", StartedAt: nowRFC3339(),
	}
	_ = p.saveLocked()
	p.mu.Unlock()

	go p.runPrepareWorkdir(jobID, id, proj, b.Name, b.Workdir, b.Runtime, b.SSHHost)
	return map[string]interface{}{
		"status": "ok", "job_id": jobID, "workdir": b.Workdir,
		"next": "用 commondev_job_status 查进度",
	}, nil
}

// handlePiConfigSet 设置 pi settings.json 的一个顶层键（直接落盘，带备份/校验）。
func (p *Plugin) handlePiConfigSet(args map[string]interface{}) (interface{}, error) {
	key := strings.TrimSpace(str(args, "key"))
	if key == "" {
		return map[string]interface{}{"error": "key 必填"}, nil
	}
	dir, err := p.piScopeDir(args)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	backup, err := backupPiConfig(dir)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	path := filepath.Join(dir, "settings.json")
	cur := map[string]interface{}{}
	if b, readErr := os.ReadFile(path); readErr == nil {
		if err := json.Unmarshal(b, &cur); err != nil {
			return map[string]interface{}{"error": fmt.Sprintf("settings.json 已损坏（拒绝覆盖）: %v", err)}, nil
		}
	} else if !os.IsNotExist(readErr) {
		return map[string]interface{}{"error": readErr.Error()}, nil
	}
	raw := strings.TrimSpace(str(args, "value"))
	if raw == "" {
		return map[string]interface{}{"error": "value 必填（JSON 值或普通字符串）"}, nil
	}
	var value interface{}
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		value = raw
	}
	cur[key] = value
	b, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	if err := writeFile0600(path, b); err != nil {
		rollbackPiConfig(dir, backup)
		return map[string]interface{}{"error": "写入失败，已回滚: " + err.Error()}, nil
	}
	var verify map[string]interface{}
	if b2, err := os.ReadFile(path); err != nil || json.Unmarshal(b2, &verify) != nil {
		rollbackPiConfig(dir, backup)
		return map[string]interface{}{"error": "写后校验失败，已回滚"}, nil
	}
	return map[string]interface{}{"status": "ok", "scope": scopeOrDefault(str(args, "scope")), "key": key}, nil
}

// runPiMCP 是 pi mcp 命令的受控包装（所有写入仍落 pi 自己的 mcp.json）。
func (p *Plugin) runPiMCP(args []string, workdir string) (interface{}, error) {
	agentDir := p.piAgentDir()
	cmd := exec.Command(p.piBin(), append([]string{"mcp"}, args...)...)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), "PI_CODING_AGENT_DIR="+agentDir)
	out, err := cmd.CombinedOutput()
	clean := truncate(stripPiNoise(string(out)), 1200)
	if err != nil {
		return map[string]interface{}{"status": "failed", "error": err.Error(), "output": clean}, nil
	}
	return map[string]interface{}{"status": "ok", "output": clean}, nil
}

func (p *Plugin) mcpWorkdir(args map[string]interface{}) (string, error) {
	scope := scopeOrDefault(str(args, "scope"))
	if scope == "plugin" {
		return p.piAgentDir(), nil
	}
	wd := strings.TrimSpace(str(args, "workdir"))
	if wd == "" || !p.isInsideProgramRoot(wd) {
		return "", fmt.Errorf("scope=project 时 workdir 必须在 program/ 工程目录下")
	}
	return wd, nil
}

func (p *Plugin) handlePiMCPList(args map[string]interface{}) (interface{}, error) {
	wd, err := p.mcpWorkdir(args)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	return p.runPiMCP([]string{"list", "--json"}, wd)
}

func (p *Plugin) handlePiMCPAdd(args map[string]interface{}) (interface{}, error) {
	name, source := strings.TrimSpace(str(args, "name")), strings.TrimSpace(str(args, "source"))
	if name == "" || source == "" {
		return map[string]interface{}{"error": "name 与 source 必填"}, nil
	}
	wd, err := p.mcpWorkdir(args)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	argv := []string{"add", name}
	if scopeOrDefault(str(args, "scope")) == "project" {
		argv = append(argv, "--local")
	}
	if url := strings.TrimSpace(str(args, "url")); url != "" {
		argv = append(argv, "--url", url)
	} else {
		// source 是命令及其参数的 JSON 数组，避免 shell 重新解析/注入。
		var command []string
		if err := json.Unmarshal([]byte(source), &command); err != nil || len(command) == 0 {
			return map[string]interface{}{"error": "source 必须是 JSON 字符串数组（stdio），或另填 url"}, nil
		}
		argv = append(argv, "--")
		argv = append(argv, command...)
	}
	return p.runPiMCP(argv, wd)
}

func (p *Plugin) handlePiMCPRemove(args map[string]interface{}) (interface{}, error) {
	name := strings.TrimSpace(str(args, "name"))
	if name == "" {
		return map[string]interface{}{"error": "name 必填"}, nil
	}
	wd, err := p.mcpWorkdir(args)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	argv := []string{"remove", name}
	if scopeOrDefault(str(args, "scope")) == "project" {
		argv = append(argv, "--local")
	}
	return p.runPiMCP(argv, wd)
}

// handlePiSourceRemove 删除一个 provider 及其 auth 记录。
func (p *Plugin) handlePiSourceRemove(args map[string]interface{}) (interface{}, error) {
	name := strings.TrimSpace(str(args, "name"))
	if name == "" {
		return map[string]interface{}{"error": "name 必填"}, nil
	}
	dir := p.piAgentDir()
	backup, err := backupPiConfig(dir)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	models, err := loadPiModels(dir)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	if _, ok := models.Providers[name]; !ok {
		return map[string]interface{}{"error": "源不存在: " + name}, nil
	}
	delete(models.Providers, name)
	auth, err := loadPiAuth(dir)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	delete(auth, name)
	if err := p.writePiConfigs(dir, models, auth); err != nil {
		rollbackPiConfig(dir, backup)
		return map[string]interface{}{"error": "删除失败，已回滚: " + err.Error()}, nil
	}
	return map[string]interface{}{"status": "ok", "removed": name}, nil
}

// handlePiPluginUpdate 更新一个已安装的 pi 扩展；未指定 source 时更新全部。
func (p *Plugin) handlePiPluginUpdate(args map[string]interface{}) (interface{}, error) {
	agentDir := p.piAgentDir()
	dir := agentDir
	argv := []string{"update"}
	if source := strings.TrimSpace(str(args, "source")); source != "" {
		argv = append(argv, source)
	}
	if scopeOrDefault(str(args, "scope")) == "project" {
		wd := strings.TrimSpace(str(args, "workdir"))
		if wd == "" || !p.isInsideProgramRoot(wd) {
			return map[string]interface{}{"error": "scope=project 时 workdir 必须在 program/ 工程目录下"}, nil
		}
		dir = wd
		argv = append(argv, "-l")
	}
	cmd := exec.Command(p.piBin(), argv...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PI_CODING_AGENT_DIR="+agentDir)
	out, err := cmd.CombinedOutput()
	result := map[string]interface{}{"scope": scopeOrDefault(str(args, "scope")), "dir": dir, "output": truncate(stripPiNoise(string(out)), 800)}
	if err != nil {
		result["status"] = "failed"
		result["error"] = err.Error()
		return result, nil
	}
	result["status"] = "ok"
	return result, nil
}
