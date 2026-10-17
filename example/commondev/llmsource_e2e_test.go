package main

// LLM 源端到端判据：**内核源 → 插件导入 → pi 配置 → 真实调用成功**。
//
// ## 为什么必须端到端而不能只测导入
//
// 导入逻辑正确 ≠ pi 能用。中间隔着三层容易各自出错：
//
//	① 源字段映射（内核 core.llm.sources.<n>.* → pi 的 providers.<n>.{baseUrl,api,apiKey}）
//	② adapter → pi api 名的映射（openai → openai-completions）
//	③ 默认源写进 settings.json（defaultProvider/defaultModel）
//
// 任何一层错了，现象都是「pi 起不来 / 报 No API key found」——
// 而单元判据全绿。所以本文件用**真实源 + 真实 HTTP 调用**收口。
//
// ## 判据分层
//
//	TestLLMSourceImportProducesValidPiConfig   — 纯逻辑（不需要网络）
//	TestLLMSourceRoundTripAgainstRealEndpoint  — 真机（需要 HA_E2E_LLM=1）

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/JianFeeeee/homeagentsdk/sdk"
)

// coreSettings 是只答 GetCore 的最小替身 —— 内核配置面在这一层被使用。
type coreSettings struct {
	stubSettings
	core map[string]interface{}
}

func (c *coreSettings) GetCore(key string) (interface{}, error) {
	if v, ok := c.core[key]; ok {
		return v, nil
	}
	return nil, nil
}
func (c *coreSettings) ListCore(prefix string) ([]string, error) {
	var out []string
	for k := range c.core {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, k)
		}
	}
	return out, nil
}

// newPluginWithCore 造一个能读到「内核 LLM 源」的插件。
func newPluginWithCore(t *testing.T, core map[string]interface{}) *Plugin {
	t.Helper()
	p := newTestPluginForShell(t)
	cs := &coreSettings{stubSettings: stubSettings{dir: filepath.Dir(p.path)}, core: core}
	p.sdk = sdk.New("commondev", cs, nil, nil, nil, nil)
	return p
}

// ★★ 判据：导入后生成的 models.json / auth.json / settings.json 必须自洽。
func TestLLMSourceImportProducesValidPiConfig(t *testing.T) {
	core := map[string]interface{}{
		"core.llm.provider":                     "mysrc",
		"core.llm.model":                        "my-model",
		"core.llm.sources.mysrc.base_url":       "http://127.0.0.1:9999/v1",
		"core.llm.sources.mysrc.api_key":        "sk-test-123",
		"core.llm.sources.mysrc.model":          "my-model",
		"core.llm.sources.mysrc.adapter":        "openai",
		"core.llm.sources.mysrc.context_window": float64(128000),
		"core.llm.sources.mysrc.vision":         "true",
	}
	p := newPluginWithCore(t, core)
	agentDir := p.piAgentDir()

	n, def, err := p.importKernelSources(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || def != "mysrc" {
		t.Fatalf("导入数量/默认源不对: n=%d def=%q", n, def)
	}

	// models.json 必须是 pi 认识的形状
	var models piModelsFile
	b := mustRead(t, filepath.Join(agentDir, "models.json"))
	if err := json.Unmarshal(b, &models); err != nil {
		t.Fatalf("models.json 不是合法 JSON: %v", err)
	}
	pv, ok := models.Providers["mysrc"]
	if !ok {
		t.Fatal("models.json 缺少源")
	}
	if pv.BaseURL != "http://127.0.0.1:9999/v1" {
		t.Errorf("baseUrl 错: %q", pv.BaseURL)
	}
	// ★ adapter 映射：openai → openai-completions（pi 的取值）
	if pv.API != "openai-completions" {
		t.Errorf("api 映射错（pi 需要 openai-completions）: %q", pv.API)
	}
	if len(pv.Models) != 1 || pv.Models[0].ID != "my-model" {
		t.Errorf("models 错: %+v", pv.Models)
	}
	if pv.Models[0].ContextWindow != 128000 {
		t.Errorf("contextWindow 未透传: %d", pv.Models[0].ContextWindow)
	}
	// 权限：含明文 key ⇒ 必须 0600
	if fi, err := os.Stat(filepath.Join(agentDir, "models.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("models.json 权限应为 0600")
	}
	// settings.json 的默认源
	var settings map[string]interface{}
	if err := json.Unmarshal(mustRead(t, filepath.Join(agentDir, "settings.json")), &settings); err != nil {
		t.Fatal(err)
	}
	if settings["defaultProvider"] != "mysrc" {
		t.Errorf("defaultProvider 错: %v", settings["defaultProvider"])
	}
}

// ★★ 判据：**未知 adapter 必须报错**，不得猜一个默认值。
//
// 猜错会让 pi 用错的协议说话，现象是「模型不回复」—— 那是极难定位的失败。
func TestLLMSourceImportRejectsUnknownAdapter(t *testing.T) {
	core := map[string]interface{}{
		"core.llm.sources.weird.base_url": "http://x/v1",
		"core.llm.sources.weird.model":    "m",
		"core.llm.sources.weird.adapter":  "some-unknown-protocol",
	}
	p := newPluginWithCore(t, core)
	if _, _, err := p.importKernelSources(p.piAgentDir()); err == nil {
		t.Fatal("★ 未知 adapter 未报错 —— 猜默认值会让 pi 用错协议（现象是「模型不回复」）")
	}
}

// ★★ 判据：内核**没有任何源**时必须报错（不静默造一个空的）。
func TestLLMSourceImportFailsWithoutKernelSources(t *testing.T) {
	p := newPluginWithCore(t, map[string]interface{}{})
	if _, _, err := p.importKernelSources(p.piAgentDir()); err == nil {
		t.Fatal("内核无源时应报错，而不是生成一个空配置（pi 会起不来且原因不明）")
	}
}

// ★★★ 真机判据：**导入的真实源必须真的能调通**。
//
// 这是端到端的最后一环：前面的判据只证明「配置写对了」，
// 这条证明「配置里的源可用」。
//
// 用 HA_E2E_LLM=1 打开（默认跳过：它依赖本机 llmsproxy）。
// 环境变量 HA_E2E_BASE_URL / HA_E2E_API_KEY / HA_E2E_MODEL 可覆盖。
func TestLLMSourceRoundTripAgainstRealEndpoint(t *testing.T) {
	if os.Getenv("HA_E2E_LLM") != "1" {
		t.Skip("跳过真机 LLM 判据（设 HA_E2E_LLM=1 打开）")
	}
	baseURL := envOr("HA_E2E_BASE_URL", "http://127.0.0.1:8081/v1")
	apiKey := os.Getenv("HA_E2E_API_KEY")
	model := envOr("HA_E2E_MODEL", "AUTO")
	if apiKey == "" {
		t.Fatal("需要 HA_E2E_API_KEY")
	}

	// ① 像真实内核那样配一个源，走**插件的导入路径**
	core := map[string]interface{}{
		"core.llm.provider":                   "e2e",
		"core.llm.model":                      model,
		"core.llm.sources.e2e.base_url":       baseURL,
		"core.llm.sources.e2e.api_key":        apiKey,
		"core.llm.sources.e2e.model":          model,
		"core.llm.sources.e2e.adapter":        "openai",
		"core.llm.sources.e2e.context_window": float64(1048576),
	}
	p := newPluginWithCore(t, core)
	agentDir := p.piAgentDir()
	n, def, err := p.importKernelSources(agentDir)
	if err != nil || n != 1 || def != "e2e" {
		t.Fatalf("导入失败: n=%d def=%q err=%v", n, def, err)
	}

	// ② 从**导入产物**（而不是从测试变量）读回连接信息 —— 这是端到端的要点：
	// 若导入写错了字段，这里就会用错地址/错 key，调用会失败。
	var models piModelsFile
	b := mustRead(t, filepath.Join(agentDir, "models.json"))
	if err := json.Unmarshal(b, &models); err != nil {
		t.Fatal(err)
	}
	pv := models.Providers["e2e"]
	if pv == nil {
		t.Fatal("导入产物里没有 e2e 源")
	}
	var auth map[string]map[string]string
	if err := json.Unmarshal(mustRead(t, filepath.Join(agentDir, "auth.json")), &auth); err != nil {
		t.Fatal(err)
	}
	keyFromDisk := auth["e2e"]["apiKey"]
	if keyFromDisk == "" {
		t.Fatal("auth.json 里没有 key —— pi 会报 No API key found")
	}

	// ③ 用**磁盘上的**配置真调一次
	body, _ := json.Marshal(map[string]interface{}{
		"model":      pv.Models[0].ID,
		"max_tokens": 32,
		"messages":   []map[string]string{{"role": "user", "content": "只回答两个字：可用"}},
	})
	url := pv.BaseURL + "/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+keyFromDisk)
	req.Header.Set("Content-Type", "application/json")
	cli := &http.Client{Timeout: 90 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatalf("调用导入配置里的源失败: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
		Error interface{} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d（用导入产物里的地址与 key）: %v", resp.StatusCode, out.Error)
	}
	if len(out.Choices) == 0 {
		t.Fatalf("无 choices: %+v", out)
	}
	t.Logf("✅ 端到端通过：内核源 → 插件导入 → 用磁盘配置真实调用成功（%s）", pv.Models[0].ID)
}

// ---------------- 对照真机内核配置 ----------------

// ★★★ 判据：**用真机生产 config.db 里的真实源**跑一遍导入。
//
// 与上一条的区别：上一条的源是测试构造的；这条读**生产配置**，
// 因此能发现「真实内核的字段形态与我们的假设不符」这类问题
// （例如字段名、类型、有没有 sources.* 子表）。
//
// 用 HA_E2E_REAL_DB=/home/newqqagent/config.db 打开。
func TestLLMSourceImportFromRealKernelConfig(t *testing.T) {
	dbPath := os.Getenv("HA_E2E_REAL_DB")
	if dbPath == "" {
		t.Skip("跳过真机内核配置判据（设 HA_E2E_REAL_DB=<config.db> 打开）")
	}
	rows := readCoreLLMRows(t, dbPath)
	if len(rows) == 0 {
		t.Fatalf("%s 里没有任何 core.llm.* 配置", dbPath)
	}
	// 直接喂真实的字段形态给导入逻辑
	core := map[string]interface{}{}
	for k, v := range rows {
		core[k] = v
	}
	p := newPluginWithCore(t, core)
	agentDir := p.piAgentDir()
	n, def, err := p.importKernelSources(agentDir)
	if err != nil {
		t.Fatalf("★ 用**真实内核配置**导入失败（说明字段形态与假设不符）: %v", err)
	}
	if n == 0 {
		t.Fatal("导入 0 个源")
	}
	var models piModelsFile
	if err := json.Unmarshal(mustRead(t, filepath.Join(agentDir, "models.json")), &models); err != nil {
		t.Fatal(err)
	}
	for name, pv := range models.Providers {
		if pv.BaseURL == "" || pv.API == "" || len(pv.Models) == 0 {
			t.Errorf("源 %s 导入不完整: %+v", name, pv)
		}
		// ★ api 必须是 pi 认识的值（否则 pi 起不来）
		if pv.API != "openai-completions" && pv.API != "anthropic-messages" {
			t.Errorf("源 %s 的 api=%q 不是 pi 的合法取值", name, pv.API)
		}
	}
	t.Logf("✅ 真实内核配置导入成功：%d 个源，默认=%s", n, def)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// readCoreLLMRows 读 config.db 的 core.llm.* 行（用 go-sqlite3）。
func readCoreLLMRows(t *testing.T, dbPath string) map[string]string {
	t.Helper()
	// 用 sqlite3 CLI（内核侧判据不引 cgo 依赖）
	out, err := runCmd("sqlite3", dbPath, "select key||'\t'||value from config where key like 'core.llm.%';")
	if err != nil {
		t.Skipf("读 config.db 失败（缺 sqlite3？）: %v", err)
	}
	rows := map[string]string{}
	for _, line := range splitLines(out) {
		if i := indexByte(line, '\t'); i > 0 {
			rows[line[:i]] = line[i+1:]
		}
	}
	return rows
}

func runCmd(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	b, err := cmd.Output()
	return string(b), err
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if c == '\n' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// mustRead 读文件（失败即 Fatal）。
func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	return b
}
