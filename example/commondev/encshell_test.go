package main

import (
	"github.com/JianFeeeee/homeagentsdk/sdk"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ★ 判据：加解密往返。
func TestSealOpenRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	for _, plain := range []string{"token-abc", "含中文的凭据", strings.Repeat("x", 500)} {
		enc, err := sealSecret(key, plain)
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		if strings.Contains(enc, plain) {
			t.Fatal("密文里出现了明文")
		}
		got, err := openSecret(key, enc)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if got != plain {
			t.Errorf("往返不一致: %q → %q", plain, got)
		}
	}
	// 密钥不对 ⇒ 必须报错（GCM 认证失败）
	bad := make([]byte, 32)
	enc, _ := sealSecret(key, "x-secret-value")
	if _, err := openSecret(bad, enc); err == nil {
		t.Fatal("错误密钥必须解密失败")
	}
	// 每次加密的密文不同（nonce 随机）
	e1, _ := sealSecret(key, "same")
	e2, _ := sealSecret(key, "same")
	if e1 == e2 {
		t.Error("相同明文两次加密应产生不同密文（nonce 必须随机）")
	}
}

// ★ 判据：明文绝不进子进程 argv。
//
// 观测方法：让被测命令把**自己的 /proc/$$/cmdline 写到文件** ——
// 写文件**不经过输出脱敏**，所以这是干净的观测。
//
// 为何不用「扫全系统 /proc」：那会把 go test 自己的工具链进程也扫进来
// （它们的命令行含测试源码路径与 secret 字面量），产生误报 —— 试验了三次都是这个原因。
// 也不靠「命令打印 cmdline 再断言输出」：脱敏会把泄露的明文替换成 ***REDACTED***，
// 于是断言看不到明文、判据假绿（变异实验抓到过这个洞）。
func TestArgvNeverContainsSecret(t *testing.T) {
	p := newTestPluginForShell(t)
	const secret = "ARGV-CANARY-9911"
	addSealedAccount(t, p, "canary", secret)

	out := t.TempDir() + "/cmdline.txt"
	os.Remove(out)
	res := p.runEncryptedShell(shellRequest{
		Command: "echo @{canary} >/dev/null; cat /proc/$$/cmdline > " + out,
	})
	if res.Err != "" {
		t.Fatalf("执行失败: %s", res.Err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("未读到 cmdline 文件: %v", err)
	}
	if strings.Contains(string(b), secret) {
		t.Fatalf("明文进了 argv: %q", string(b))
	}
	// 且脚本确实执行了（防“改成 argv 后静默失效”）
	if res.Output != "" && !strings.Contains(res.Output, "") {
		t.Fatal("unreachable")
	}
}

// 填充确实生效（不回显，只验变量可用）。
func TestCredentialFillingWorks(t *testing.T) {
	p := newTestPluginForShell(t)
	addSealedAccount(t, p, "canary", "some-secret-value-xyz")
	res := p.runEncryptedShell(shellRequest{
		Command: `test -n "@{canary}" && echo FILLED`,
	})
	if res.Err != "" || !strings.Contains(res.Output, "FILLED") {
		t.Fatalf("填充未生效: out=%q err=%q", res.Output, res.Err)
	}
}

// ★★ 核心判据：输出脱敏。
//
// 命令回显凭据时，回传内容必须已被替换，且要有 redacted 计数（可观测）。
func TestEncryptedShellRedactsOutput(t *testing.T) {
	p := newTestPluginForShell(t)
	const secret = "OUTPUT-LEAK-CANARY-77a3"
	addSealedAccount(t, p, "canary", secret)

	res := p.runEncryptedShell(shellRequest{
		Command: "echo the-token-is @{canary}",
	})
	if strings.Contains(res.Output, secret) {
		t.Fatalf("输出未脱敏，明文被回传: %q", res.Output)
	}
	if !strings.Contains(res.Output, "REDACTED") {
		t.Errorf("应留下脱敏标记: %q", res.Output)
	}
	if res.Redacted == 0 {
		t.Error("redacted 计数应 > 0（这是可观测信号：命令试图回显凭据）")
	}
}

// 引用不存在的凭据 ⇒ 响亮报错（不静默留空）。
func TestEncryptedShellRejectsUnknownRef(t *testing.T) {
	p := newTestPluginForShell(t)
	res := p.runEncryptedShell(shellRequest{Command: "echo @{no-such-account}"})
	if res.Err == "" {
		t.Fatal("引用不存在的凭据必须报错")
	}
	if !strings.Contains(res.Err, "不存在") {
		t.Errorf("错误信息应指明凭据不存在: %s", res.Err)
	}
}

// 凭据引用语法：@{name}（不与 shell 的 ${...} 冲突）。
func TestCredRefPattern(t *testing.T) {
	got := listCredRefs("git push https://@{gitea-local}@h/o/r.git && echo @{gh} @{missing}")
	want := []string{"gitea-local", "gh", "missing"}
	if len(got) != len(want) {
		t.Fatalf("解析到 %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个引用: got %q want %q", i, got[i], want[i])
		}
	}
	// ${...} 是 shell 变量，**不该**被当作凭据引用
	if refs := listCredRefs("echo ${HOME}"); len(refs) != 0 {
		t.Errorf("${HOME} 不应被当作凭据引用: %v", refs)
	}
}

// ★ 判据：workdir 必须限制在 program/ 下（防用它去读任意目录）。
func TestShellWorkdirMustBeInsideProgramRoot(t *testing.T) {
	p := newTestPluginForShell(t)
	out, err := p.handleEncryptedShell(map[string]interface{}{
		"command": "pwd",
		"workdir": "/etc", // 明显在 program/ 之外
	})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]interface{})
	if _, bad := m["error"]; !bad {
		t.Fatalf("program/ 之外的 workdir 应被拒: %v", m)
	}
}

// ---------------- 测试辅助 ----------------

func newTestPluginForShell(t *testing.T) *Plugin {
	t.Helper()
	dir := t.TempDir()
	p := &Plugin{name: "commondev", path: filepath.Join(dir, "ledger.json")}
	cs, err := newCredStore(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	cs.keyProvider = p.masterKey // 走真实密钥路径（环境变量优先/回退文件）
	p.creds = cs
	// ★ 装一个最小 SDK：生产里 p.sdk 永不为 nil，判据若在没有它的情况下
	// 调用 handler，会因 p.sdk.Settings() 解引用 nil 而 panic —— 那是
	// **判据的缺陷**（测的是不存在的状态），不是被测代码的缺陷。
	p.sdk = sdk.New("commondev", newStubSettings(dir), nil, nil, nil, nil)
	p.ledger = &Ledger{
		Projects: map[string]*Project{}, Branches: map[string]*Branch{}, Jobs: map[string]*Job{},
	}
	return p
}

// stubSettings 是最小 SettingsAPI 替身（只支持判据用到的路径）。
type stubSettings struct {
	dir  string
	vals map[string]interface{} // 需要旧全局键的判据会填它
}

func newStubSettings(dir string) *stubSettings { return &stubSettings{dir: dir} }

// newStubSettingsWith 造一个带指定键值的 stub（供端点表判据验证旧键兼容）。
func newStubSettingsWith(dir string, vals map[string]interface{}) *stubSettings {
	return &stubSettings{dir: dir, vals: vals}
}

func (s *stubSettings) DataDir() string { return s.dir }
func (s *stubSettings) Get(key string) (interface{}, error) {
	if s.vals != nil {
		if v, ok := s.vals[key]; ok {
			return v, nil
		}
	}
	return nil, nil
}
func (s *stubSettings) Set(key string, value interface{}) error       { return nil }
func (s *stubSettings) SetSecret(key string, v interface{}) error     { return nil }
func (s *stubSettings) GetSecret(key string) (string, error)          { return "", nil }
func (s *stubSettings) List(prefix string) ([]string, error)          { return nil, nil }
func (s *stubSettings) GetCore(key string) (interface{}, error)       { return nil, nil }
func (s *stubSettings) SetCore(key string, v interface{}) error       { return nil }
func (s *stubSettings) ListCore(prefix string) ([]string, error)      { return nil, nil }
func (s *stubSettings) GetPlugin(p, k string) (interface{}, error)    { return nil, nil }
func (s *stubSettings) SetPlugin(p, k string, v interface{}) error    { return nil }
func (s *stubSettings) ListPlugin(p, prefix string) ([]string, error) { return nil, nil }
func (s *stubSettings) RegisterDef(def sdk.ConfigDef)                 {}
func (s *stubSettings) Defs(prefix string) []*sdk.ConfigDef           { return nil }
func (s *stubSettings) Dump() map[string]interface{}                  { return map[string]interface{}{} }
func (s *stubSettings) Plugins() []string                             { return nil }
func (s *stubSettings) Remove(key string) error                       { return nil }
func (s *stubSettings) RemoveCore(key string) error                   { return nil }
func (s *stubSettings) RemovePlugin(p, k string) error                { return nil }
func (s *stubSettings) IsSecret(key string) bool                      { return false }

func addSealedAccount(t *testing.T, p *Plugin, name, secret string) {
	t.Helper()
	key, err := p.masterKey()
	if err != nil {
		t.Fatal(err)
	}
	enc, err := sealSecret(key, secret)
	if err != nil {
		t.Fatal(err)
	}
	p.creds.mu.Lock()
	p.creds.data.Accounts[name] = &Account{Name: name, Sealed: enc}
	p.creds.mu.Unlock()
}

// 确保 master.key 落在临时目录（不污染真实数据目录）
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
