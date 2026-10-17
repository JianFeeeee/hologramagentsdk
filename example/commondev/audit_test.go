package main

// 审计补全项的判据。
//
// 这些判据对应审计发现的**真问题**（不是补全功能的装饰）：
//   · push 的凭据纪律（设计 §7.2）—— 曾经 pushURLWithToken 是死代码，
//     意味着「凭据不进 remote URL」这条纪律**从未被真正执行过**
//   · 孤儿清理（listOrphanDirs 曾是死代码）
//   · pi 配置写入的备份/回滚（设计 §3.6.4 纪律 2）

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newAuditPlugin(t *testing.T) *Plugin {
	t.Helper()
	p := newTestPluginForCriteria(t)
	return p
}

// ★★ 判据：凭据残留检查能识别 `scheme://user:pass@host` 形态。
func TestGitConfigCredentialResidueDetection(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "config"),
		[]byte("[remote \"origin\"]\n\turl = https://user:tok3n@example.com/o/r.git\n"), 0o644)
	if got := gitConfigCredentialResidue(dir); got == "" {
		t.Fatal("★ 未检出内嵌凭据（设计 §7.2 要求 workdir_clean 内置此检查）")
	}
	// 干净的不应误报
	dir2 := t.TempDir()
	os.MkdirAll(filepath.Join(dir2, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir2, ".git", "config"),
		[]byte("[remote \"origin\"]\n\turl = https://example.com/o/r.git\n"), 0o644)
	if got := gitConfigCredentialResidue(dir2); got != "" {
		t.Errorf("干净 remote 被误报: %s", got)
	}
	// scp 形态的 git@host 不是凭据（用户提示有误伤风险）
	dir3 := t.TempDir()
	os.MkdirAll(filepath.Join(dir3, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir3, ".git", "config"),
		[]byte("[remote \"origin\"]\n\turl = git@github.com:o/r.git\n"), 0o644)
	if got := gitConfigCredentialResidue(dir3); got != "" {
		t.Errorf("scp 形态 git@host 被误报成凭据: %s", got)
	}
}

// ★★ 判据（设计 §7.2 + 审计发现）：workdir_clean
//   - **默认 dry_run**（删除不可逆）
//   - 只删**本插件生成的孤儿目录**，不碰台账里的目录、不碰陌生目录
func TestWorkdirCleanOnlyRemovesOrphans(t *testing.T) {
	p := newAuditPlugin(t)
	root := p.programRoot()
	os.MkdirAll(root, 0o700)

	known := "aabbccdd-feat_known"
	orphan := "11223344-feat_orphan"
	stranger := "not-ours-just-a-dir"
	for _, d := range []string{known, orphan, stranger} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	p.ledger.Branches["br-1"] = &Branch{
		ID: "br-1", ProjectID: "proj", Name: "feat/known",
		Workdir: filepath.Join(root, known), Status: "open"}

	// ① dry_run 默认：什么都不删
	res, err := p.handleWorkdirClean(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]interface{})["status"] != "dry_run" {
		t.Fatalf("默认应为 dry_run，实际 %v", res.(map[string]interface{})["status"])
	}
	for _, d := range []string{known, orphan, stranger} {
		if _, err := os.Stat(filepath.Join(root, d)); err != nil {
			t.Fatalf("★ dry_run 却删掉了 %s", d)
		}
	}

	// ② 实际清理：只删孤儿
	res2, err := p.handleWorkdirClean(map[string]interface{}{"dry_run": false})
	if err != nil {
		t.Fatal(err)
	}
	removed, _ := res2.(map[string]interface{})["removed"].([]string)
	if len(removed) != 1 || removed[0] != orphan {
		t.Fatalf("应只删 %s，实际 %v", orphan, removed)
	}
	if _, err := os.Stat(filepath.Join(root, known)); err != nil {
		t.Fatal("★ 删掉了台账里在用的目录")
	}
	if _, err := os.Stat(filepath.Join(root, stranger)); err != nil {
		t.Fatal("★ 删掉了不是本插件生成的目录（越界）")
	}
}

// ★★ 判据：agent_down 清掉 session/通道绑定，但**保留工作目录与状态**。
//
// 它与 branch_close 的区别正是"暂停"vs"收尾"；若 agent_down 也删目录，
// 二者就没有区别了（而我实现时差点把两者写成一个）。
func TestAgentDownKeepsWorkdir(t *testing.T) {
	p := newAuditPlugin(t)
	dir := t.TempDir()
	p.ledger.Branches["br-1"] = &Branch{
		ID: "br-1", ProjectID: "proj", Name: "feat/x", Workdir: dir,
		Status: "open", SessionID: "s1", InputCh: "commondev/br-1", OutputCh: "commondev_br-1"}

	res, err := p.handleAgentDown(map[string]interface{}{"branch_id": "br-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]interface{})["status"] != "ok" {
		t.Fatal("应成功")
	}
	b := p.ledger.Branches["br-1"]
	if b.SessionID != "" || b.InputCh != "" || b.OutputCh != "" {
		t.Errorf("★ session/通道绑定应被清掉（留着会让人以为进程还在）: %+v", b)
	}
	if b.Status != "open" {
		t.Errorf("★ 状态不该变（这是暂停不是收尾），实际 %s", b.Status)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("★ agent_down 不该删工作目录（那是 branch_close 的事）")
	}
}

// ---------------- pi 配置写入纪律（设计 §3.6.4） ----------------

// ★★★ 判据：**损坏的 models.json 必须拒绝覆盖**（纪律 2）。
//
// 静默当空会**抹掉用户已有的全部源** —— 那是最坏的一种"静默降级"。
func TestCorruptPiModelsIsNotSilentlyOverwritten(t *testing.T) {
	dir := t.TempDir()
	bad := "{ this is not json"
	os.WriteFile(filepath.Join(dir, "models.json"), []byte(bad), 0o600)

	if _, err := loadPiModels(dir); err == nil {
		t.Fatal("★ 损坏的 models.json 未被拒绝 —— 静默当空会抹掉用户已有的源")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "models.json"))
	if string(after) != bad {
		t.Fatal("损坏的文件被改动了")
	}
}

// ★★ 判据：写坏时**回滚**（纪律 2：失败则回滚并响亮报错）。
func TestPiConfigRollbackRestoresPrevious(t *testing.T) {
	dir := t.TempDir()
	orig := `{"providers":{"keep":{"baseUrl":"http://keep","api":"openai-completions","models":[{"id":"m"}]}}}`
	os.WriteFile(filepath.Join(dir, "models.json"), []byte(orig), 0o600)

	backup, err := backupPiConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟"写坏了"
	os.WriteFile(filepath.Join(dir, "models.json"), []byte("{broken"), 0o600)
	rollbackPiConfig(dir, backup)

	back, _ := os.ReadFile(filepath.Join(dir, "models.json"))
	if string(back) != orig {
		t.Fatalf("★ 回滚未恢复原内容\n前: %s\n后: %s", orig, back)
	}
}

// ★★ 判据：**api_key 绝不进任何返回结构**（纪律 3）。
func TestPiConfigGetNeverReturnsKey(t *testing.T) {
	p := newAuditPlugin(t)
	dir := p.piAgentDir()
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "models.json"), []byte(
		`{"providers":{"p1":{"baseUrl":"http://x","api":"openai-completions","apiKey":"SUPER_SECRET","models":[{"id":"m"}]}}}`), 0o600)

	res, err := p.handlePiConfigGet(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	s := jsonDump(t, res)
	if strings.Contains(s, "SUPER_SECRET") {
		t.Fatalf("★★ api_key 泄漏到返回值里：%s", s)
	}
	if !strings.Contains(s, "key_set") {
		t.Errorf("应报 key_set 而不是值: %s", s)
	}
}

// jsonDump 把任意返回值转成 JSON 串（用于断言"某串不得出现"）。
func jsonDump(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ★ 判据：pi 插件工具的**两套命名都能到达同一实现**（早期稿用 plugin，细化稿用 package）。
func TestPiPluginToolAliasesRegistered(t *testing.T) {
	src, err := os.ReadFile("plugin.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, name := range []string{
		`"commondev_pi_plugin_install"`, `"commondev_pi_package_install"`,
		`"commondev_pi_plugin_list"`, `"commondev_pi_plugin_remove"`,
	} {
		if !strings.Contains(s, name) {
			t.Errorf("工具 %s 未注册（两套命名应都可用）", name)
		}
	}
}
