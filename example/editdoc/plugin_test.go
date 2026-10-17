package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/JianFeeeee/homeagentsdk/sdk"
)

// fakeSettings 是 SettingsAPI 的最小实现，只为把 resolveScript 的 DataDir 分支跑通。
// 其余方法测试用不到，给个安全默认值即可。
type fakeSettings struct {
	dataDir string
	defs    []*sdk.ConfigDef
}

func (f *fakeSettings) Get(string) (interface{}, error)               { return nil, nil }
func (f *fakeSettings) Set(string, interface{}) error                 { return nil }
func (f *fakeSettings) SetSecret(string, interface{}) error           { return nil }
func (f *fakeSettings) GetSecret(string) (string, error)              { return "", nil }
func (f *fakeSettings) List(string) ([]string, error)                 { return nil, nil }
func (f *fakeSettings) GetCore(string) (interface{}, error)           { return nil, nil }
func (f *fakeSettings) SetCore(string, interface{}) error             { return nil }
func (f *fakeSettings) ListCore(string) ([]string, error)             { return nil, nil }
func (f *fakeSettings) DataDir() string                               { return f.dataDir }
func (f *fakeSettings) GetPlugin(string, string) (interface{}, error) { return nil, nil }
func (f *fakeSettings) SetPlugin(string, string, interface{}) error   { return nil }
func (f *fakeSettings) ListPlugin(string, string) ([]string, error)   { return nil, nil }
func (f *fakeSettings) RegisterDef(def sdk.ConfigDef)                 { f.defs = append(f.defs, &def) }
func (f *fakeSettings) Defs(string) []*sdk.ConfigDef                  { return f.defs }
func (f *fakeSettings) Dump() map[string]interface{}                  { return nil }
func (f *fakeSettings) Plugins() []string                             { return nil }

// newTestPlugin 造一个带可用 DataDir 的 Plugin（sdk 走 sdk.New，只注入 SettingsAPI）。
func newTestPlugin(t *testing.T, scriptPath string) *Plugin {
	t.Helper()
	sett := &fakeSettings{dataDir: t.TempDir()}
	s := sdk.New("editdoc", sett, nil, nil, nil, nil)
	return &Plugin{
		name:       "editdoc",
		sdk:        s,
		scriptPath: scriptPath,
		venvPython: "/usr/local/bin/python3",
		timeoutSec: 60,
	}
}

// 内嵌脚本必须自带 v2 协议标记，否则防呆会把自己的脚本也挡掉。
func TestEmbeddedScriptHasProtocolMarker(t *testing.T) {
	if !containsMarker(embeddedScript) {
		t.Fatalf("内嵌脚本缺少协议标记 %q —— 防呆会把内嵌脚本自己判为旧版", scriptProtocolMarker)
	}
	// 标记必须与 embed/edit_doc.py 顶部一致（防手改两处不同步）
	t.Logf("协议标记=%q 内嵌脚本指纹=%s", scriptProtocolMarker, scriptStamp())
}

// 业务脚本只能有**一份**：embed/edit_doc.py。
//
// 为何钉这一条（2026-10-13 迁入仓库时实测）：原工作目录里同时存在
// edit_doc.py 与 embed/edit_doc.py —— 两份逐字节相同的副本，而只有
// embed/ 那份被 //go:embed 引用。同名双副本是典型的漂移源：
// 改了顶层那份（看着像“主文件”）而真正生效的是 embed/ 里的，
// 或反过来；两种都不会报错，只会让行为与源码对不上。
func TestOnlyOneBusinessScript(t *testing.T) {
	if _, err := os.Stat("edit_doc.py"); err == nil {
		t.Error("存在顶层 edit_doc.py：业务脚本只应有 embed/edit_doc.py 一份（顶部副本未被 embed，改了不生效）")
	}
	if _, err := os.Stat(filepath.Join("embed", "edit_doc.py")); err != nil {
		t.Fatalf("缺少 embed/edit_doc.py: %v", err)
	}
}

// script_path 指向 v1 脚本（无标记）→ 必须忽略它、回退内嵌脚本，
// 并记录 ignoredScriptPath。这是防呆的核心契约。
func TestResolveScriptIgnoresV1Script(t *testing.T) {
	dir := t.TempDir()
	v1 := filepath.Join(dir, "edit_doc_v1.py")
	// v1 脚本的真实特征：argv 约定 <file> <op> <json>，且没有 v2 标记
	if err := os.WriteFile(v1, []byte(
		"import sys\npath = sys.argv[1]\noperation = sys.argv[2]\n"), 0644); err != nil {
		t.Fatal(err)
	}

	p := newTestPlugin(t, v1)
	got, err := p.resolveScript()
	if err != nil {
		t.Fatalf("应回退到内嵌脚本，却报错: %v", err)
	}
	if got == v1 {
		t.Fatal("v1 脚本被采用了 —— 防呆失效，6 个工具会被打挂")
	}
	if filepath.Base(got) != "edit_doc.py" {
		t.Fatalf("应回退到 runtime/edit_doc.py，实际: %s", got)
	}
	if p.ignoredScriptPath != v1 {
		t.Fatalf("ignoredScriptPath 未记录，实际: %q", p.ignoredScriptPath)
	}
	// 回退出来的脚本必须是内嵌那份（字节一致）
	released, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("内嵌脚本未落盘: %v", err)
	}
	if string(released) != string(embeddedScript) {
		t.Fatal("落盘的脚本与内嵌脚本字节不一致")
	}
	t.Logf("v1 被正确忽略，回退到 %s", got)
}

// script_path 指向含标记的 v2 脚本 → 必须采用它，且不记 ignoredScriptPath。
func TestResolveScriptAcceptsV2Script(t *testing.T) {
	dir := t.TempDir()
	v2 := filepath.Join(dir, "edit_doc_v2.py")
	if err := os.WriteFile(v2, []byte(
		"#!/usr/bin/env python3\n"+scriptProtocolMarker+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	p := newTestPlugin(t, v2)
	got, err := p.resolveScript()
	if err != nil {
		t.Fatalf("应直接采用 v2 脚本，却报错: %v", err)
	}
	if got != v2 {
		t.Fatalf("v2 脚本未被采用，实际用了: %s", got)
	}
	if p.ignoredScriptPath != "" {
		t.Fatalf("v2 脚本不应被标记忽略: %q", p.ignoredScriptPath)
	}
}

// script_path 为空 → 走内嵌脚本释放，且指纹命中第二次不再重复写。
func TestResolveScriptFallsBackToEmbedded(t *testing.T) {
	p := newTestPlugin(t, "")
	got, err := p.resolveScript()
	if err != nil {
		t.Fatalf("空 script_path 应回退内嵌脚本: %v", err)
	}
	if p.ignoredScriptPath != "" {
		t.Fatalf("空配置不该有 ignoredScriptPath: %q", p.ignoredScriptPath)
	}
	// 第二次调用应复用（指纹 + 文件都在），路径一致
	got2, err := p.resolveScript()
	if err != nil || got2 != got {
		t.Fatalf("第二次解析不稳定: %s vs %s (%v)", got, got2, err)
	}
	// .stamp 指纹应有内容
	stamp, err := os.ReadFile(filepath.Join(filepath.Dir(got), ".stamp"))
	if err != nil || string(stamp) != scriptStamp() {
		t.Fatalf("指纹文件异常: %q %v", stamp, err)
	}
}

// 指向不存在的路径 → 不该报错，应静默回退内嵌脚本。
func TestResolveScriptMissingPathFallsBack(t *testing.T) {
	p := newTestPlugin(t, filepath.Join(t.TempDir(), "not-there.py"))
	got, err := p.resolveScript()
	if err != nil {
		t.Fatalf("路径不存在应回退内嵌脚本: %v", err)
	}
	if p.ignoredScriptPath != "" {
		t.Fatalf("不存在的路径不该记为 ignored: %q", p.ignoredScriptPath)
	}
	if filepath.Base(got) != "edit_doc.py" {
		t.Fatalf("应回退到 runtime/edit_doc.py，实际: %s", got)
	}
}

// containsMarker 与 resolveScript 内部用的是同一判据，供测试复用。
func containsMarker(b []byte) bool {
	m := []byte(scriptProtocolMarker)
	for i := 0; i+len(m) <= len(b); i++ {
		if string(b[i:i+len(m)]) == string(m) {
			return true
		}
	}
	return false
}
