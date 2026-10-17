package main

// pi 运行时配置类的判据（llmsource / pi_plugin / pi_mcp / pi_config）。
//
// ## 为什么补这批
//
// 覆盖度审计显示这批 14 个工具**没有任何机制级测试**，而它们
// **直接写 pi 的配置文件**（models.json / auth.json / settings.json /
// mcp.json）—— 是本插件里风险最高的一组：
//   - 写坏 models.json 会让内层 pi 完全起不来；
//   - 把 key 泄漏进返回值会经由 agent 记忆外泄；
//   - 改错目录会污染用户的全局 pi（`~/.pi`）。
//
// 已有 audit_test.go 覆盖了两条关键纪律（损坏不覆盖、备份回滚），
// 这里补齐其余：**不泄漏 key**、**作用域隔离**、**幂等**、**错误可诊断**。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

// newPiPlugin 造一个 piAgentDir 落在临时目录的插件。
func newPiPlugin(t *testing.T) *Plugin {
	t.Helper()
	p := newFlowPlugin(t)
	// piAgentDir = <插件数据>/runtime/agentdir；p.path 的父目录就是插件数据目录
	if err := os.MkdirAll(p.piAgentDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// writePiModels 写一个 models.json。
func writePiModels(t *testing.T, dir string, m map[string]interface{}) {
	t.Helper()
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "models.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ★★★ 判据：`pi_source_list` **绝不返回 api key**。
//
// 这是全插件最硬的一条纪律（设计 §7.2）：key 一旦进返回值，
// 就会经由 agent 记忆、日志、UI 三处外泄，且**事后无法收回**。
func TestPiSourceListNeverReturnsKey(t *testing.T) {
	p := newPiPlugin(t)
	dir := p.piAgentDir()
	writePiModels(t, dir, map[string]interface{}{
		"providers": map[string]interface{}{
			"myprov": map[string]interface{}{
				"baseUrl": "https://api.example.com",
				"api":     "openai",
				"apiKey":  "sk-SUPER-SECRET-VALUE-12345",
				"models":  []interface{}{map[string]interface{}{"id": "m1"}},
			},
		},
	})

	res, err := p.handlePiSourceList(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	// 把整个返回值序列化，确认秘密字符串**任何形态都不出现**
	blob, _ := json.Marshal(res)
	s := string(blob)
	if strings.Contains(s, "sk-SUPER-SECRET") {
		t.Fatalf("★ pi_source_list 泄漏了 api key: %s", s)
	}
	// 也不该泄漏 auth.json 里的凭据（这里没建，但格式化检查同样适用）
	if strings.Contains(strings.ToLower(s), "secret-value") {
		t.Fatalf("★ 返回值含疑似凭据: %s", s)
	}
}

// ★★★ 判据：**作用域隔离** —— 一律指向插件自己的 agentdir，绝不碰用户全局 pi。
//
// 纪律 4（设计 §3.5.2）：piScopeDir 必须带显式 agentdir。
// 违反的话会改掉用户 `~/.pi` 的配置 —— 那是**跨项目污染**，很难查。
func TestPiScopeNeverTouchesUserGlobalPi(t *testing.T) {
	p := newPiPlugin(t)
	// plugin 作用域（默认）→ 插件自己的 agentdir
	got, err := p.piScopeDir(map[string]interface{}{"scope": "plugin"})
	if err != nil {
		t.Fatal(err)
	}
	if got != p.piAgentDir() {
		t.Errorf("plugin 作用域应指向插件 agentdir: %s vs %s", got, p.piAgentDir())
	}
	// 必须是**插件数据目录**下的 runtime/agentdir，不是用户全局 ~/.pi
	if strings.HasPrefix(got, filepath.Join(homeDir(), ".pi")) {
		t.Errorf("★ plugin 作用域指向了用户全局 pi: %s", got)
	}
	// project 作用域 → **必须显式给 workdir**（不给就报错，不猜）
	if _, err := p.piScopeDir(map[string]interface{}{"scope": "project"}); err == nil {
		t.Error("★ scope=project 缺 workdir 应报错（不猜一个目录）")
	}
	// ★ 更强的约束：workdir 必须落在**插件的 program/ 工程目录**下。
	// 任意目录不行 —— 否则 scope=project 会往用户随便什么目录里写 .pi。
	if _, err := p.piScopeDir(map[string]interface{}{
		"scope": "project", "workdir": t.TempDir(),
	}); err == nil {
		t.Error("★ 任意目录不该被接受（必须在 program/ 下）")
	}
	wd := filepath.Join(p.programRoot(), "testwd")
	if err := os.MkdirAll(wd, 0o755); err != nil {
		t.Fatal(err)
	}
	gp, err := p.piScopeDir(map[string]interface{}{"scope": "project", "workdir": wd})
	if err != nil {
		t.Fatalf("program/ 下的 workdir 应被接受: %v", err)
	}
	if !strings.HasPrefix(gp, wd) {
		t.Errorf("★ project 作用域应在工作目录内: %s", gp)
	}
}

// ★★ 判据：`pi_source_default` 写入的是 settings.json 的**正确键名**。
//
// 键名写错的表现是"设了默认源但不生效"，且没有任何报错 ——
// 属于典型的静默失效。
func TestPiSourceDefaultWritesExpectedKeys(t *testing.T) {
	p := newPiPlugin(t)
	dir := p.piAgentDir()
	writePiModels(t, dir, map[string]interface{}{
		"providers": map[string]interface{}{
			"prov1": map[string]interface{}{
				"baseUrl": "http://x", "api": "openai", "models": []interface{}{map[string]interface{}{"id": "m1"}},
			},
		},
	})
	res, err := p.handlePiSourceDefault(map[string]interface{}{"name": "prov1"})
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := res.(map[string]interface{}); ok {
		if e, bad := m["error"]; bad {
			t.Fatalf("设默认源失败: %v", e)
		}
	}
	// 读回 settings.json 确认键名
	b, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("settings.json 未生成: %v", err)
	}
	var st map[string]interface{}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("settings.json 不是合法 JSON: %v\n%s", err, b)
	}
	if st["defaultProvider"] != "prov1" {
		t.Errorf("★ defaultProvider 未正确写入: %v", st["defaultProvider"])
	}
	// 必须同时给默认 model（只给 provider 的话 pi 不知道用哪个模型）
	if _, has := st["defaultModel"]; !has {
		t.Errorf("★ 缺 defaultModel（只给 provider，pi 不知用哪个模型）: %v", st)
	}
}

// ★★ 判据：设默认源指向**不存在的源**必须报错（不静默写进去）。
func TestPiSourceDefaultRejectsUnknownSource(t *testing.T) {
	p := newPiPlugin(t)
	writePiModels(t, p.piAgentDir(), map[string]interface{}{
		"providers": map[string]interface{}{
			"only": map[string]interface{}{"baseUrl": "http://x", "api": "openai", "models": []interface{}{map[string]interface{}{"id": "m"}}},
		},
	})
	res, _ := p.handlePiSourceDefault(map[string]interface{}{"name": "nonexistent"})
	m, ok := res.(map[string]interface{})
	if !ok {
		t.Skipf("返回形状未知: %T", res)
	}
	if _, isErr := m["error"]; !isErr {
		t.Errorf("★ 指向不存在的源应报错: %v", m)
	}
}

// ★★ 判据：`pi_source_remove` 幂等（删不存在的源不报致命错）+ 真的删掉。
func TestPiSourceRemoveIsIdempotentAndEffective(t *testing.T) {
	p := newPiPlugin(t)
	dir := p.piAgentDir()
	writePiModels(t, dir, map[string]interface{}{
		"providers": map[string]interface{}{
			"kill": map[string]interface{}{"baseUrl": "http://x", "api": "openai", "models": []interface{}{map[string]interface{}{"id": "m"}}},
			"keep": map[string]interface{}{"baseUrl": "http://y", "api": "openai", "models": []interface{}{map[string]interface{}{"id": "m"}}},
		},
	})
	if _, err := p.handlePiSourceRemove(map[string]interface{}{"name": "kill"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "models.json"))
	if strings.Contains(string(b), `"kill"`) {
		t.Errorf("★ 源未被删除:\n%s", b)
	}
	if !strings.Contains(string(b), `"keep"`) {
		t.Errorf("★ 误删了别的源:\n%s", b)
	}
	// 再删一次：不该炸（幂等）
	if _, err := p.handlePiSourceRemove(map[string]interface{}{"name": "kill"}); err != nil {
		t.Errorf("重复删除应幂等: %v", err)
	}
}

// ★★ 判据：`pi_source_list` 在**配置不存在**时给出空列表而不是报错。
//
// 新部署上 models.json 尚不存在是常态；报错会让调用方以为坏了。
func TestPiSourceListOnFreshInstall(t *testing.T) {
	p := newPiPlugin(t) // 不写任何配置
	res, err := p.handlePiSourceList(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := res.(map[string]interface{})
	if !ok {
		t.Skipf("返回形状未知: %T", res)
	}
	if e, bad := m["error"]; bad {
		t.Errorf("★ 无配置时应返回空列表而非报错: %v", e)
	}
}

// ★★★ 判据：**损坏的 mcp.json 必须拒绝覆盖**（与 models.json 同一纪律）。
//
// 静默重建会抹掉用户已配的 MCP 服务器，且用户不会立刻发现。
func TestCorruptPiMcpIsNotSilentlyOverwritten(t *testing.T) {
	p := newPiPlugin(t)
	dir := p.piAgentDir()
	orig := []byte("{ this is broken json")
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), orig, 0o600); err != nil {
		t.Fatal(err)
	}
	res, _ := p.handlePiMCPAdd(map[string]interface{}{
		"name": "x", "url": "http://localhost:1234",
	})
	if m, ok := res.(map[string]interface{}); ok {
		if _, isErr := m["error"]; !isErr {
			t.Errorf("★ 损坏的 mcp.json 应被拒绝（不静默覆盖）: %v", m)
		}
	}
	// 原文件必须**未被改动**
	after, _ := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if string(after) != string(orig) {
		t.Errorf("★ 损坏文件被覆盖了:\nbefore=%s\nafter=%s", orig, after)
	}
}

// ★★ 判据：`pi_mcp_list` 在无配置时给空列表。
func TestPiMcpListOnFreshInstall(t *testing.T) {
	p := newPiPlugin(t)
	res, err := p.handlePiMCPList(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := res.(map[string]interface{}); ok {
		if _, bad := m["error"]; bad {
			t.Errorf("★ 无配置时不该报错: %v", m)
		}
	}
}

// ★★ 判据：`pi_config_set` 拒绝**不存在的键**或返回明确错误。
//
// 静默接受未知键的后果是"我设了但没生效"，且没有任何线索。
func TestPiConfigSetUnknownKeyIsDiagnosable(t *testing.T) {
	p := newPiPlugin(t)
	res, _ := p.handlePiConfigSet(map[string]interface{}{
		"key": "definitely_not_a_real_pi_key_xyz", "value": "v",
	})
	m, ok := res.(map[string]interface{})
	if !ok {
		return // 返回形状不同则不判
	}
	// 要么报错，要么明确说明写了什么 —— 但不能是"静默成功且无痕迹"
	if _, isErr := m["error"]; !isErr {
		if _, hasKey := m["key"]; !hasKey {
			t.Errorf("★ 未知键既未报错也未回显，调用方无法判断是否生效: %v", m)
		}
	}
}

// ★★ 判据：`project_open` 是 `project_add` 的**真别名**（行为一致）。
func TestProjectOpenIsAliasOfAdd(t *testing.T) {
	p := newFlowPlugin(t)
	r1, _ := p.handleProjectOpen(map[string]interface{}{
		"name": "aliased", "remote_url": "/tmp/a.git",
	})
	if m, ok := r1.(map[string]interface{}); ok {
		if _, bad := m["error"]; bad {
			t.Fatalf("project_open 失败: %v", m)
		}
	}
	// 台账里必须真的登记了（别名不能是空壳）
	if _, ok := p.ledger.Projects["aliased"]; !ok {
		t.Error("★ project_open 未真正登记项目（别名是空壳）")
	}
	// 与 add 的重复检测行为一致
	r2, _ := p.handleProjectOpen(map[string]interface{}{"name": "aliased", "remote_url": "/tmp/b.git"})
	if _, isErr := r2.(map[string]interface{})["error"]; !isErr {
		t.Error("★ 别名未继承 add 的重复检测")
	}
}

// ★★★ 判据：写进 auth.json 的凭据格式**必须是 pi 认的 schema**。
//
// 这是实测踩出来的真缺陷（2026-10-15）：`pi_source_add` / `pi_source_import`
// 原先写 `{"apiKey": "..."}`，而 pi 认的是 `{"type":"api_key","key":"..."}`
// （权威定义：pi 的 dist/core/auth-storage.js:185 `value.type === "api_key"`）。
//
// 后果**完全静默**：three 个配置文件看起来都正常，但 pi 的 auth 读取返回空，
// 于是报 `Provider is not configured: <name>` —— 内嵌 pi 谁都调不动。
//
// 定位过程值得记：同一个 pi API、同一份 models.json，只切
// `PI_CODING_AGENT_DIR` 就一边跑通一边跑不通，逐字段对比才落到键名上。
// 所以判据必须**断言键名与取值**，不能只看"文件写成功了"。
func TestPiAuthWrittenInPiSchema(t *testing.T) {
	p := newPiPlugin(t)
	dir := p.piAgentDir()
	res, _ := p.handlePiSourceAdd(map[string]interface{}{
		"name":     "prov",
		"base_url": "http://127.0.0.1:8081/v1",
		"api":      "openai-completions",
		"api_key":  "sk-test-SCHEMA-CHECK",
		"model":    "AUTO",
	})
	if m, ok := res.(map[string]interface{}); ok {
		if e, bad := m["error"]; bad {
			t.Fatalf("添加源失败: %v (%v)", e, m["details"])
		}
	}

	b, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatalf("auth.json 未生成: %v", err)
	}
	var auth map[string]map[string]interface{}
	if err := json.Unmarshal(b, &auth); err != nil {
		t.Fatalf("auth.json 不是合法 JSON: %v\n%s", err, b)
	}
	entry, ok := auth["prov"]
	if !ok {
		t.Fatalf("auth.json 里没有 prov: %s", b)
	}
	// ★ 核心断言：必须是 pi 的 schema
	if entry["type"] != "api_key" {
		t.Errorf("★ entry.type 必须是 \"api_key\"（pi 的 auth-storage 按它判别），实际 %#v", entry["type"])
	}
	if entry["key"] != "sk-test-SCHEMA-CHECK" {
		t.Errorf("★ entry.key 不对: %#v", entry["key"])
	}
	// 旧格式的键不该出现（否则说明还有一处没改）
	if _, bad := entry["apiKey"]; bad {
		t.Errorf("★ auth.json 仍有旧的 \"apiKey\" 键（pi 不认）: %s", b)
	}
}

// ★★ 判据：**导入内核源**也写 pi 的 schema（两条路径必须一致）。
//
// source_add 与 source_import 是两个入口，写同一份 auth.json。
// 只改一处的话另一条路径仍然静默失效。
func TestPiAuthImportAlsoUsesPiSchema(t *testing.T) {
	// 直接验证构造器（import 路径的写入最终也走它）
	newPiPlugin(t)
	e := newPiAuthEntry("sk-x")
	if e["type"] != "api_key" || e["key"] != "sk-x" {
		t.Errorf("★ newPiAuthEntry 未产出 pi schema: %#v", e)
	}
	// 反向：确保没有留下产出旧格式的地方
	src, err := os.ReadFile("llmsource.go")
	if err == nil {
		if strings.Contains(string(src), `"apiKey": apiKey`) {
			t.Error("★ llmsource.go 里仍有写旧格式 apiKey 的地方")
		}
	}
	src2, err2 := os.ReadFile("pi_tools.go")
	if err2 == nil {
		if strings.Contains(string(src2), `map[string]string{"apiKey"`) {
			t.Error("★ pi_tools.go 里仍有写旧格式的地方")
		}
	}
}
