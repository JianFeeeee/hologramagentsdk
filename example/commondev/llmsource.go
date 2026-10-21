package main

// LLM 源导入：把 HologramAgent 内核已配的 LLM 源写进内置 pi 的配置文件。
//
// 用户要求（2026-10-14）：
//
//	「插件内置 pi 默认使用 homeagent 内部配置的 llms 源，支持添加其他源，
//	  且支持在插件中配置默认使用哪个源」
//	「pi 配置的源直接写入配置文件」← 不另建存储（pi 本身就有完整配置系统）
//
// ## 落点（都是 pi 自己的配置文件）
//
//	<agentdir>/models.json   providers.<name>.{baseUrl,api,apiKey,models[]}
//	<agentdir>/auth.json     provider 凭据
//	<agentdir>/settings.json defaultProvider / defaultModel
//
// ## 内核侧源的真实位置 ［已核］
//
// 存在 **config 表**（不是 config_<plugin>），键前缀 core.llm.：
//
//	core.llm.provider                        ← ★ 默认源就是这一项
//	core.llm.sources.<n>.base_url
//	core.llm.sources.<n>.api_key
//	core.llm.sources.<n>.model
//	core.llm.sources.<n>.adapter             (“openai” → pi 的 "openai-completions")
//	core.llm.sources.<n>.context_window      (可选)
//	core.llm.sources.<n>.vision / .audio     (可选)
//
// ## 两条安全红线（设计 §3.6.5）
//
//  1. models.json / auth.json 含**明文** api key ⇒ 目录 0700、文件 0600。
//     内核那套是明文存 config.db（该库 644 全局可读）—— **不要把这套惯例带进来**。
//  2. 导入失败要**报错**，不得静默回退到宿主凭据（~/.pi/agent/auth.json）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// piModelsFile 是 pi 的 models.json 结构（只取我们用到的字段）。
type piModelsFile struct {
	Providers map[string]*piProvider `json:"providers"`
}

type piProvider struct {
	BaseURL string    `json:"baseUrl"`
	API     string    `json:"api"`
	APIKey  string    `json:"apiKey,omitempty"`
	Models  []piModel `json:"models"`
}

type piModel struct {
	ID            string   `json:"id"`
	Name          string   `json:"name,omitempty"`
	Reasoning     bool     `json:"reasoning"`
	Input         []string `json:"input,omitempty"`
	ContextWindow int      `json:"contextWindow,omitempty"`
	MaxTokens     int      `json:"maxTokens,omitempty"`
}

// adapterToAPI 把内核的 adapter 名映射成 pi 的 api 取值。
//
// 只映射确定已知的；未知 adapter 返回空串 ⇒ 调用方必须报错，
// 不能猜一个默认值（猜错会让 pi 用错的协议说话，表现成"模型不回复"）。
func adapterToAPI(adapter string) string {
	switch strings.ToLower(strings.TrimSpace(adapter)) {
	case "", "openai", "openai-completions", "openai_completions":
		return "openai-completions"
	case "anthropic", "anthropic-messages", "claude":
		return "anthropic-messages"
	default:
		return ""
	}
}

// importKernelSources 把内核已配的 LLM 源导入内置 pi 的配置文件。
//
// 返回：导入了几个源、默认源名、错误。
func (p *Plugin) importKernelSources(agentDir string) (int, string, error) {
	sett := p.sdk.Settings()

	// ① 源清单：内核的配置是散键（core.llm.sources.<n>.<field>），
	//    所以要先扫出所有 <n>。
	keys, err := sett.ListCore("core.llm.sources.")
	if err != nil {
		return 0, "", fmt.Errorf("读取内核 LLM 源清单失败: %w", err)
	}
	names := map[string]bool{}
	for _, k := range keys {
		rest := strings.TrimPrefix(k, "core.llm.sources.")
		if i := strings.IndexByte(rest, '.'); i > 0 {
			names[rest[:i]] = true
		}
	}
	if len(names) == 0 {
		// 没有任何 sources.* —— 退回单源形态（core.llm.base_url/api_key/model）
		if n, def, err := p.importSingleKernelSource(agentDir); err == nil && n > 0 {
			return n, def, nil
		}
		return 0, "", fmt.Errorf("内核没有任何已配置的 LLM 源（core.llm.sources.* 为空）")
	}

	ordered := make([]string, 0, len(names))
	for n := range names {
		ordered = append(ordered, n)
	}
	sort.Strings(ordered)

	models := &piModelsFile{Providers: map[string]*piProvider{}}
	auth := map[string]interface{}{}
	imported := 0

	for _, n := range ordered {
		prefix := "core.llm.sources." + n + "."
		baseURL := coreStr(sett, prefix+"base_url")
		apiKey := coreStr(sett, prefix+"api_key")
		model := coreStr(sett, prefix+"model")
		adapter := coreStr(sett, prefix+"adapter")
		if baseURL == "" {
			continue // 源不完整：跳过（不静默造一个坏的）
		}
		api := adapterToAPI(adapter)
		if api == "" {
			return imported, "", fmt.Errorf(
				"源 %s 的 adapter=%q 无法映射到 pi 的 api（已知：openai/anthropic）——"+
					"不猜默认值，请显式指定", n, adapter)
		}
		if model == "" {
			model = "AUTO"
		}
		m := piModel{
			ID:    model,
			Name:  model,
			Input: []string{"text"},
		}
		if cw := coreInt(sett, prefix+"context_window"); cw > 0 {
			m.ContextWindow = cw
		}
		if coreStr(sett, prefix+"vision") == "true" {
			m.Input = []string{"text", "image"}
		}
		models.Providers[n] = &piProvider{
			BaseURL: baseURL, API: api, APIKey: apiKey,
			Models: []piModel{m},
		}
		if apiKey != "" {
			auth[n] = newPiAuthEntry(apiKey)
		}
		imported++
	}
	if imported == 0 {
		return 0, "", fmt.Errorf("内核的 LLM 源都缺 base_url，无可导入")
	}

	if err := p.writePiConfigs(agentDir, models, auth); err != nil {
		return imported, "", err
	}

	// ② 默认源 = 内核的 core.llm.provider（用户要求：默认用内核的）
	def := coreStr(sett, "core.llm.provider")
	if def == "" || models.Providers[def] == nil {
		def = ordered[0] // 内核没指定或指到了不存在的源：用第一个（并如实返回）
	}
	if err := p.writePiDefaults(agentDir, def, coreStr(sett, "core.llm.model")); err != nil {
		return imported, def, err
	}
	return imported, def, nil
}

// importSingleKernelSource 处理「内核只有单源、无 sources.* 子表」的形态。
func (p *Plugin) importSingleKernelSource(agentDir string) (int, string, error) {
	sett := p.sdk.Settings()
	baseURL := coreStr(sett, "core.llm.base_url")
	apiKey := coreStr(sett, "core.llm.api_key")
	model := coreStr(sett, "core.llm.model")
	adapter := coreStr(sett, "core.llm.adapter")
	if baseURL == "" {
		return 0, "", fmt.Errorf("内核未配 core.llm.base_url")
	}
	api := adapterToAPI(adapter)
	if api == "" {
		return 0, "", fmt.Errorf("adapter=%q 无法映射", adapter)
	}
	if model == "" {
		model = "AUTO"
	}
	name := "kernel"
	if n := coreStr(sett, "core.llm.provider"); n != "" {
		name = n
	}
	models := &piModelsFile{Providers: map[string]*piProvider{
		name: {BaseURL: baseURL, API: api, APIKey: apiKey,
			Models: []piModel{{ID: model, Name: model, Input: []string{"text"}}}},
	}}
	auth := map[string]interface{}{}
	if apiKey != "" {
		auth[name] = newPiAuthEntry(apiKey)
	}
	if err := p.writePiConfigs(agentDir, models, auth); err != nil {
		return 0, "", err
	}
	if err := p.writePiDefaults(agentDir, name, model); err != nil {
		return 0, name, err
	}
	return 1, name, nil
}

// writePiConfigs 写 models.json 与 auth.json（★ 0600），并回读校验。
//
// 为何要回读校验：json 写坏会让内层 pi 直接起不来，而那种失败看起来
// 像是"pi 有问题"，排查成本很高。写完必须能解析回来。
func (p *Plugin) writePiConfigs(agentDir string, models *piModelsFile,
	auth map[string]interface{}) error {
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return err
	}
	mb, err := json.MarshalIndent(models, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile0600(filepath.Join(agentDir, "models.json"), mb); err != nil {
		return err
	}
	var back piModelsFile
	if err := json.Unmarshal(mb, &back); err != nil {
		return fmt.Errorf("models.json 回读校验失败: %w", err)
	}
	if len(back.Providers) != len(models.Providers) {
		return fmt.Errorf("models.json 回读不一致: 写入 %d 源、读回 %d 源",
			len(models.Providers), len(back.Providers))
	}
	if len(auth) > 0 {
		ab, err := json.MarshalIndent(auth, "", "  ")
		if err != nil {
			return err
		}
		if err := writeFile0600(filepath.Join(agentDir, "auth.json"), ab); err != nil {
			return err
		}
	}
	return nil
}

// writePiDefaults 把默认源写进 settings.json（pi 原生字段）。
func (p *Plugin) writePiDefaults(agentDir, provider, model string) error {
	path := filepath.Join(agentDir, "settings.json")
	cur := map[string]interface{}{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &cur) // 读坏就当空（下面会整体重写）
	}
	if provider != "" {
		cur["defaultProvider"] = provider
	}
	if model != "" {
		cur["defaultModel"] = model
	}
	b, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return err
	}
	return writeFile0600(path, b)
}

// writeFile0600 写入并确权 —— 这些文件含明文 api key。
func writeFile0600(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// coreStr / coreInt 读内核配置（类型不匹配一律当空，不 panic）。
func coreStr(sett interface {
	GetCore(string) (interface{}, error)
}, key string) string {
	v, err := sett.GetCore(key)
	if err != nil || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return fmt.Sprintf("%v", t)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func coreInt(sett interface {
	GetCore(string) (interface{}, error)
}, key string) int {
	v, err := sett.GetCore(key)
	if err != nil || v == nil {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case string:
		var n int
		_, _ = fmt.Sscanf(t, "%d", &n)
		return n
	default:
		return 0
	}
}

// ---------------- 工具实现 ----------------

func (p *Plugin) piAgentDir() string {
	return filepath.Join(filepath.Dir(p.path), "runtime", "agentdir")
}

// handlePiSourceImport 把内核的 LLM 源导入内置 pi 的配置文件。
func (p *Plugin) handlePiSourceImport(args map[string]interface{}) (interface{}, error) {
	n, def, err := p.importKernelSources(p.piAgentDir())
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	return map[string]interface{}{
		"status":       "ok",
		"imported":     n,
		"default":      def,
		"agentdir":     p.piAgentDir(),
		"config_files": []string{"models.json", "auth.json", "settings.json"},
	}, nil
}

// handlePiSourceList 列出已配的源（★ 不返回 key）。
func (p *Plugin) handlePiSourceList(args map[string]interface{}) (interface{}, error) {
	dir := p.piAgentDir()
	b, err := os.ReadFile(filepath.Join(dir, "models.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]interface{}{
				"providers": []string{}, "default": "",
				"hint": "内置 pi 尚无 LLM 配置；调 commondev_pi_source_import 导入内核的源",
			}, nil
		}
		return map[string]interface{}{"error": err.Error()}, nil
	}
	var f piModelsFile
	if err := json.Unmarshal(b, &f); err != nil {
		return map[string]interface{}{"error": "models.json 损坏: " + err.Error()}, nil
	}

	// 默认源从 settings.json 读
	def := ""
	if sb, err := os.ReadFile(filepath.Join(dir, "settings.json")); err == nil {
		var s map[string]interface{}
		if json.Unmarshal(sb, &s) == nil {
			def, _ = s["defaultProvider"].(string)
		}
	}
	// ★ 只暴露名字与端点，绝不返回 apiKey
	type view struct {
		Name    string `json:"name"`
		BaseURL string `json:"base_url"`
		API     string `json:"api"`
		Model   string `json:"model,omitempty"`
		Default bool   `json:"default"`
	}
	out := []view{}
	for name, pr := range f.Providers {
		m := ""
		if len(pr.Models) > 0 {
			m = pr.Models[0].ID
		}
		out = append(out, view{Name: name, BaseURL: pr.BaseURL, API: pr.API,
			Model: m, Default: name == def})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return map[string]interface{}{"providers": out, "default": def, "count": len(out)}, nil
}

// handlePiSourceDefault 切换内置 pi 的默认源。
func (p *Plugin) handlePiSourceDefault(args map[string]interface{}) (interface{}, error) {
	name := strings.TrimSpace(str(args, "name"))
	if name == "" {
		return map[string]interface{}{"error": "name 必填"}, nil
	}
	dir := p.piAgentDir()
	b, err := os.ReadFile(filepath.Join(dir, "models.json"))
	if err != nil {
		return map[string]interface{}{"error": "读 models.json 失败: " + err.Error()}, nil
	}
	var f piModelsFile
	if err := json.Unmarshal(b, &f); err != nil {
		return map[string]interface{}{"error": "models.json 损坏: " + err.Error()}, nil
	}
	pr, ok := f.Providers[name]
	if !ok {
		names := make([]string, 0, len(f.Providers))
		for n := range f.Providers {
			names = append(names, n)
		}
		sort.Strings(names)
		return map[string]interface{}{
			"error": fmt.Sprintf("源 %q 不存在（可用：%s）", name, strings.Join(names, ", ")),
		}, nil
	}
	model := strings.TrimSpace(str(args, "model"))
	if model == "" && len(pr.Models) > 0 {
		model = pr.Models[0].ID
	}
	if err := p.writePiDefaults(dir, name, model); err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	return map[string]interface{}{"status": "ok", "default": name, "model": model}, nil
}

// newPiAuthEntry 构造 pi 认的凭据 entry。
//
// ★ schema 是 `{"type":"api_key","key":"..."}`，不是 `{"apiKey":"..."}`。
//
// 这是**实测踩出来的**（2026-10-15）：内嵌 pi 用旧格式时，
// auth 读取静默返回空，报 `Provider is not configured: llmsproxy` ——
// 而 models.json / auth.json / settings.json 三个文件看起来都完全正常。
// 定位过程：同一个 pi API，只切 PI_CODING_AGENT_DIR 就一边跑得通
// 一边跑不通，逐字段对比才定位到这个键名。
//
// 权威定义（pi 源码）：
//
//	dist/core/auth-storage.js:185   if (value.type === "api_key") { ... }
//	dist/core/model-runtime.js:423  { type: "api_key", key: apiKey }
func newPiAuthEntry(apiKey string) map[string]interface{} {
	return map[string]interface{}{
		"type": "api_key",
		"key":  apiKey,
	}
}
