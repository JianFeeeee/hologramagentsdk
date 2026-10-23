package main

// diag 判据的夹具助手。
//
// ## 为什么需要自造 config.db
//
// 原来的 diag 判据直接对**宿主机器的** /home/<user>/config.db 断言。
// 那样有两个问题：
//
//	这台机器恰好配齐 → 绿（但什么都没证明：换个机器就红）
//	这台机器缺字段   → 红（红的是"这台机器配置有问题"，不是代码缺陷）
//	CI 上文件不存在  → t.Skip（假绿）
//
// 判据要守的是"**诊断逻辑对不对**"，那就必须自己控制输入。
// 夹具刻意做出三种输入组合（全好 / 一好一坏 / 文件缺失），
// 断言每种输入下诊断给出的结构，与机器状态彻底解耦。

import (
	"os/exec"
	"sort"
	"strings"
	"testing"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// seedConfigDB 用 sqlite3 CLI 造一个内核形状的 config.db。
//
// 表结构照内核的 `CREATE TABLE config (key TEXT PRIMARY KEY, value TEXT NOT NULL)`：
// 键形如 `core.llm.sources.<name>.<field>`，值一律是 TEXT。
//
// ★ 为什么用 sqlite3 CLI 而不是 Go 的 sqlite 驱动：
// 本仓（以及全部 example）都是**零外部依赖**的 —— go.mod 里没有 sqlite 驱动，
// 引入一个只为了测试会改变示例工程的依赖面（示例是给人抄的模板）。
// 被测代码 p.dbSources() 走的也是 sqlite3 CLI，夹具用同一条路反而更贴近真实。
//
// 缺 sqlite3 时**跳过并说明为什么**（不是静默通过）：这条判据测的是
// "解析 SQL 结果对不对"，没有 sqlite3 就没法造出合法输入。
func seedConfigDB(t *testing.T, path string, sources map[string]map[string]string) {
	t.Helper()
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skipf("跳过：本机没有 sqlite3 CLI（%v）。"+
			"夹具需要用**真实 sqlite 文件**喂 diag_db 的解析路径，"+
			"没有它造不出合法输入。装法：apt-get install -y sqlite3 / brew install sqlite", err)
	}

	var stmts []string
	stmts = append(stmts, "CREATE TABLE config (key TEXT PRIMARY KEY, value TEXT NOT NULL);")
	names := make([]string, 0, len(sources))
	for n := range sources {
		names = append(names, n)
	}
	sort.Strings(names) // 确定性：同样的 map 生成同样的 SQL
	for _, n := range names {
		fields := make([]string, 0, len(sources[n]))
		for f := range sources[n] {
			fields = append(fields, f)
		}
		sort.Strings(fields)
		for _, f := range fields {
			key := "core.llm.sources." + n + "." + f
			stmts = append(stmts,
				"INSERT INTO config(key,value) VALUES("+sqlStr(key)+","+sqlStr(sources[n][f])+");")
		}
	}
	if out, err := exec.Command(bin, path, strings.Join(stmts, "\n")).CombinedOutput(); err != nil {
		t.Fatalf("造 config.db 失败: %v\n%s", err, out)
	}
}

// sqlStr 把值包成 SQL 字符串字面量（只处理单引号转义 —— 夹具里的值都是我们自己写的）。
func sqlStr(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// fakeSettings 是能**真正存放**核心配置的 Settings 替身。
//
// 为什么需要它：p.dbSources() 在"没有 sqlite3 CLI"时回退到
// sdk.Settings().ListCore/GetCore。要覆盖那条分支，必须能给一个
// 真的能列出键、读出值的 Settings —— 返回 nil 的桩会让回退分支
// 永远得到空结果，"覆盖了回退"就变成一句空话。
type fakeSettings struct {
	values map[string]interface{} // 插件自己的 config（db_check_cmd 等）
	core   map[string]interface{} // 核心配置（core.llm.sources.*）
}

func (f *fakeSettings) Get(key string) (interface{}, error) {
	if f.values != nil {
		if v, ok := f.values[key]; ok {
			return v, nil
		}
	}
	return nil, nil
}
func (f *fakeSettings) Set(string, interface{}) error                 { return nil }
func (f *fakeSettings) SetSecret(string, interface{}) error           { return nil }
func (f *fakeSettings) GetSecret(string) (string, error)              { return "", nil }
func (f *fakeSettings) List(string) ([]string, error)                 { return nil, nil }
func (f *fakeSettings) DataDir() string                               { return "" }
func (f *fakeSettings) GetPlugin(string, string) (interface{}, error) { return nil, nil }
func (f *fakeSettings) SetPlugin(string, string, interface{}) error   { return nil }
func (f *fakeSettings) ListPlugin(string, string) ([]string, error)   { return nil, nil }
func (f *fakeSettings) RegisterDef(sdk.ConfigDef)                     {}
func (f *fakeSettings) Defs(string) []*sdk.ConfigDef                  { return nil }
func (f *fakeSettings) Dump() map[string]interface{}                  { return map[string]interface{}{} }
func (f *fakeSettings) Plugins() []string                             { return nil }

func (f *fakeSettings) GetCore(key string) (interface{}, error) {
	if f.core != nil {
		if v, ok := f.core[key]; ok {
			return v, nil
		}
	}
	return nil, nil
}
func (f *fakeSettings) SetCore(string, interface{}) error { return nil }

// ListCore 必须**按前缀**返回真实键集合 —— 被测代码靠它枚举源。
func (f *fakeSettings) ListCore(prefix string) ([]string, error) {
	var out []string
	for k := range f.core {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

// newFakeSDK 造一个只带 Settings 的 PluginSDK（其余能力为 nil）。
func newFakeSDK(sett sdk.SettingsAPI) *sdk.PluginSDK {
	return sdk.New("recoverydiag-test", sett, nil, nil, nil, nil)
}
