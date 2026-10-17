package main

// `enc_*` 工具族的判据 —— ★ 值已上交给**内核变量库**后的形态。
//
// ## 这组判据要守住的两条不变量
//
//	① **值只存一处**：插件不留副本（否则又回到"两库互不可见"的老问题）
//	② **工具返回值绝不回显明文**（用户明确提醒「存入过程也需要注意」：
//	   工具返回值会进模型上下文与会话记录）

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

// fakeVault 是内核变量库的测试替身。
//
// 它模拟真实实现的关键语义：
//   - 别名由**派生**得到（不是用户自选）——与内核一致
//   - 同明文得到同一别名
//   - 值存在这个"库里"，插件不留副本
type fakeVault struct {
	values map[string]string // alias -> plain
	notes  map[string]string
	calls  int
}

func newFakeVault() *fakeVault {
	return &fakeVault{values: map[string]string{}, notes: map[string]string{}}
}

// deriveAlias 模拟内核的 HMAC 派生（这里用简单确定性函数，只要"同明文同别名"）。
func deriveAlias(plain string) string {
	h := 0
	for _, c := range plain {
		h = h*31 + int(c)
	}
	if h < 0 {
		h = -h
	}
	return "sec_" + strings.Repeat("0", 16-len(itoa16(h))) + itoa16(h)
}

func itoa16(n int) string {
	const hexd = "0123456789abcdef"
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{hexd[n%16]}, b...)
		n /= 16
	}
	return string(b)
}

func (v *fakeVault) Available() bool { return true }

func (v *fakeVault) Store(value, note string) (string, error) {
	v.calls++
	a := deriveAlias(value)
	v.values[a] = value
	v.notes[a] = note
	return a, nil
}

func (v *fakeVault) List() []sdk.SecretInfo {
	out := make([]sdk.SecretInfo, 0, len(v.values))
	for a := range v.values {
		out = append(out, sdk.SecretInfo{Alias: a, Source: "submitted", Note: v.notes[a]})
	}
	return out
}

func (v *fakeVault) Forget(alias string) error {
	delete(v.values, alias)
	delete(v.notes, alias)
	return nil
}

func (v *fakeVault) Fill(text string) (string, int, []string) {
	n := 0
	var used []string
	for a, plain := range v.values {
		if strings.Contains(text, "@{"+a+"}") {
			text = strings.ReplaceAll(text, "@{"+a+"}", plain)
			n++
			used = append(used, a)
		}
	}
	return text, n, used
}

func (v *fakeVault) Encrypt(plain []byte) (string, error) { return "enc:" + string(plain), nil }
func (v *fakeVault) Decrypt(s string) ([]byte, error) {
	return []byte(strings.TrimPrefix(s, "enc:")), nil
}

// newTestPluginWithVault 建一个带假变量库的插件。
func newTestPluginWithVault(t *testing.T) (*Plugin, *fakeVault) {
	t.Helper()
	p := newTestPluginForShell(t)
	p.refs = secretRefs{Refs: map[string]*secretRef{}}
	v := newFakeVault()
	p.vault = v
	return p, v
}

// ★★ 判据：**存入的返回值绝不回显明文**。
//
// 依据：工具返回值会进模型上下文与会话记录。
func TestEncStoreNeverEchoesValue(t *testing.T) {
	p, _ := newTestPluginWithVault(t)
	const secret = "STORE-ECHO-CANARY-4b71"

	out, err := p.handleEncStore(map[string]interface{}{
		"alias": "canary", "value": secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), secret) {
		t.Fatalf("enc_store 返回值回显了明文: %s", b)
	}
	m := out.(map[string]interface{})
	if m["ref"] == "" || !strings.HasPrefix(m["ref"].(string), "sec_") {
		t.Errorf("应返回内核别名（ref），实际 %v", m["ref"])
	}
	if _, ok := m["warning"]; !ok {
		t.Error("直接传明文时应告警（明文已进入本次会话记录）")
	}
	if m["source"] != "literal" {
		t.Errorf("来源应标为 literal，实际 %v", m["source"])
	}
}

// ★★ 判据：**值只存内核库，插件不留副本**（去重的核心不变量）。
//
// 这是本次重构的目的。若有人"顺手"把值也存进插件侧，
// 两套库互不可见的老问题就会回来 —— 而它不会报错，只会让使用者困惑。
func TestEncStoreLeavesNoCopyInPlugin(t *testing.T) {
	p, v := newTestPluginWithVault(t)
	const secret = "NO-COPIES-CANARY-77aa"

	if _, err := p.handleEncStore(map[string]interface{}{
		"alias": "x", "value": secret,
	}); err != nil {
		t.Fatal(err)
	}

	// ① 内核库里**有**值
	if len(v.values) != 1 {
		t.Fatalf("内核库应有 1 条，实际 %d", len(v.values))
	}
	// ② 插件落盘文件里**没有**值
	b, err := os.ReadFile(p.refsPath())
	if err == nil {
		if strings.Contains(string(b), secret) {
			t.Fatalf("插件侧文件出现了明文（应只存名字映射）: %s", b)
		}
	}
	// ③ 插件的映射里**只有名字与别名**，没有值字段
	p.mu.Lock()
	for _, r := range p.refs.Refs {
		rb, _ := json.Marshal(r)
		if strings.Contains(string(rb), secret) {
			t.Fatalf("映射里出现了值: %s", rb)
		}
	}
	p.mu.Unlock()
	// ④ 映射文件权限 0600（名字本身有情报价值）
	if fi, err := os.Stat(p.refsPath()); err == nil {
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("映射文件权限应为 0600，实际 %04o", fi.Mode().Perm())
		}
	}
}

// ★ 来源分级：${ENV} 与 @file: 都应被识别，且**不告警**（明文未进会话）。
func TestEncStoreSourceClasses(t *testing.T) {
	p, _ := newTestPluginWithVault(t)
	os.Setenv("ENC_TEST_VAR", "from-env-secret")
	defer os.Unsetenv("ENC_TEST_VAR")

	out, _ := p.handleEncStore(map[string]interface{}{
		"alias": "from-env", "value": "${ENC_TEST_VAR}"})
	m := out.(map[string]interface{})
	if m["source"] != "env" {
		t.Errorf("来源应为 env，实际 %v", m["source"])
	}
	if _, bad := m["warning"]; bad {
		t.Error("env 来源不该告警（明文没进会话）")
	}

	f := filepath.Join(t.TempDir(), "tok.txt")
	os.WriteFile(f, []byte("from-file-secret\n"), 0o600) // 带换行（token 文件常见）
	out2, _ := p.handleEncStore(map[string]interface{}{
		"alias": "from-file", "value": "@file:" + f})
	m2 := out2.(map[string]interface{})
	if m2["source"] != "file" {
		t.Errorf("来源应为 file，实际 %v", m2["source"])
	}
	if _, bad := m2["warning"]; bad {
		t.Error("file 来源不该告警")
	}

	// ★ 换行必须被去掉（否则填充进命令会带换行 → 命令坏掉）
	// 通过 enc_shell 的解析路径验证（值已在内核库，只能经它读回）
	plain, found, err := p.lookupAnySecret("from-file", nil)
	if err != nil || !found {
		t.Fatalf("应能解析 from-file: found=%v err=%v", found, err)
	}
	if plain != "from-file-secret" {
		t.Errorf("文件内容应去掉尾部换行，实际 %q", plain)
	}
}

// ★ 判据：enc_list / enc_show **绝不返回值本体**。
func TestEncListShowNeverRevealValue(t *testing.T) {
	p, _ := newTestPluginWithVault(t)
	const secret = "LIST-SHOW-CANARY-2e08"
	p.handleEncStore(map[string]interface{}{"alias": "a", "value": secret})

	l, _ := p.handleEncList(nil)
	lb, _ := json.Marshal(l)
	if strings.Contains(string(lb), secret) {
		t.Fatalf("enc_list 泄露了值: %s", lb)
	}

	s, _ := p.handleEncShow(map[string]interface{}{"alias": "a"})
	sb, _ := json.Marshal(s)
	if strings.Contains(string(sb), secret) {
		t.Fatalf("enc_show 泄露了值: %s", sb)
	}
	mm := s.(map[string]interface{})
	if !strings.Contains(mm["hint"].(string), "enc_shell") {
		t.Errorf("应指引用 enc_shell 引用: %v", mm["hint"])
	}
	if !strings.Contains(mm["storage"].(string), "内核变量库") {
		t.Errorf("应说清值存于内核变量库: %v", mm["storage"])
	}
}

// ★ 判据：**删除时同步清内核库**（否则留下无人引用的密文）。
func TestEncRmForgetsKernelVariable(t *testing.T) {
	p, v := newTestPluginWithVault(t)
	p.handleEncStore(map[string]interface{}{"alias": "gone", "value": "to-be-removed"})
	if len(v.values) != 1 {
		t.Fatal("前提：内核库应有 1 条")
	}

	out, err := p.handleEncRm(map[string]interface{}{"alias": "gone"})
	if err != nil {
		t.Fatal(err)
	}
	if m := out.(map[string]interface{}); m["status"] != "ok" {
		t.Fatalf("删除应成功: %v", m)
	}
	// ★ 内核库里的值也必须没了
	if len(v.values) != 0 {
		t.Fatalf("内核库应被清空（否则留下无人引用的密文），实际 %d 条", len(v.values))
	}
	// 映射也没了
	p.mu.Lock()
	_, still := p.refs.Refs["gone"]
	p.mu.Unlock()
	if still {
		t.Error("映射未删除")
	}
	// 再删应报不存在（不是静默成功）
	out2, _ := p.handleEncRm(map[string]interface{}{"alias": "gone"})
	if _, isErr := out2.(map[string]interface{})["error"]; !isErr {
		t.Error("重复删除应明确报错")
	}
}

// ★ 判据：**变量库不可用时拒绝存储**（不静默降级到明文/私有文件）。
func TestEncStoreRefusesWhenVaultUnavailable(t *testing.T) {
	p := newTestPluginForShell(t)
	p.refs = secretRefs{Refs: map[string]*secretRef{}}
	p.vault = unavailableVault{}

	out, err := p.handleEncStore(map[string]interface{}{
		"alias": "x", "value": "must-not-be-stored"})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]interface{})
	if _, isErr := m["error"]; !isErr {
		t.Fatalf("变量库不可用时应报错，实际 %v", m)
	}
	// 且不该在插件侧留下任何东西
	p.mu.Lock()
	n := len(p.refs.Refs)
	p.mu.Unlock()
	if n != 0 {
		t.Error("拒绝后不该留下映射")
	}
}

type unavailableVault struct{}

func (unavailableVault) Available() bool                       { return false }
func (unavailableVault) Store(string, string) (string, error)  { return "", errUnavailable }
func (unavailableVault) List() []sdk.SecretInfo                { return nil }
func (unavailableVault) Forget(string) error                   { return errUnavailable }
func (unavailableVault) Fill(t string) (string, int, []string) { return t, 0, nil }
func (unavailableVault) Encrypt([]byte) (string, error)        { return "", errUnavailable }
func (unavailableVault) Decrypt(string) ([]byte, error)        { return nil, errUnavailable }

var errUnavailable = errNotAvailable{}

type errNotAvailable struct{}

func (errNotAvailable) Error() string { return "变量库不可用" }

// ★ 判据：别名合法性 —— 它会进 `@{别名}` 引用，字符集必须受限。
func TestEncStoreRejectsBadAlias(t *testing.T) {
	p, _ := newTestPluginWithVault(t)
	for _, bad := range []string{"has space", "has/slash", "has@at", ""} {
		out, _ := p.handleEncStore(map[string]interface{}{"alias": bad, "value": "v"})
		if _, isErr := out.(map[string]interface{})["error"]; !isErr {
			t.Errorf("别名 %q 应被拒", bad)
		}
	}
	// 合法字符集
	for _, ok := range []string{"good-name", "good_name", "good.name", "n123"} {
		out, _ := p.handleEncStore(map[string]interface{}{"alias": ok, "value": "v"})
		if _, isErr := out.(map[string]interface{})["error"]; isErr {
			t.Errorf("别名 %q 应被接受，实际 %v", ok, out)
		}
	}
}
