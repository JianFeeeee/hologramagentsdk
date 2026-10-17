package main

// 加密信息工具族（`enc_*`）—— ★ 值已**上交给内核变量库**。
//
// ## 去重背景（这次重构的原因）
//
// 本文件原先自带一整套加密存储：`secrets.json` + AES-256-GCM +
// 别名表 + 指纹，与内核的 `internal/security.Store` **完全重复**。
//
// 重复的真正代价不是代码量，而是**两套库互不可见**：
//
//	· 内核拦截时登记的变量 → 插件看不到
//	· 插件登记的 → 内核拦截不认识
//	· 同一个凭据可能在两处各存一份，而使用者不知道哪个权威
//
// 现在值只存**一处**（内核单例），插件经 `sdk.Secrets()` 访问它。
// 两份判据钉住这个不变量：`TestE2E_PluginAndKernelShareOneVault`。
//
// ## ★ 为什么还留着这个文件与 `enc_*` 工具
//
// 因为插件有两样内核**没有**的能力，不能一起删掉：
//
//	① **可读名**：内核别名是 HMAC 派生的（`sec_0cb468e2…`），
//	   这是"同明文自动合并"的前提（设计 S11），**不能改成用户自选**。
//	   但人需要 `@{gitea-local}` 这种能读懂的名字 ⇒ 插件存一层
//	   **名字 → 内核别名**的映射。
//	   ★ 映射里**只有名字，没有值** —— 所以这个文件泄了也没用。
//
//	② **`${ENV}` / `@file:` 来源**：让明文**从不进会话**
//	   （工具参数里只有 `${GITEA_TOKEN}` 这样的引用）。
//	   内核 `secret_submit` 目前只收直传明文（T10 待办）。
//
// ## 存储分工（一句话）
//
//	内核  —— 存值（加密）、派生别名、在调用插件前回填
//	插件  —— 存「可读名 → 别名」映射、解析来源、执行命令时的 argv 保护

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	sdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

// secretRef 是「可读名 → 内核别名」的一条映射。
//
// ★ 刻意**不含值**：值在内核变量库里，这里只有名字。
type secretRef struct {
	Name      string `json:"name"`
	Alias     string `json:"alias"` // 内核别名（sec_xxx）
	Source    string `json:"source"`
	Note      string `json:"note,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// secretRefs 是映射集合（落盘，小文件，不含任何值）。
type secretRefs struct {
	Refs map[string]*secretRef `json:"refs"`
}

// refsPath 是映射文件路径。
//
// 与旧的 secrets.json 分开命名：**旧文件里是密文值**，新文件里只有名字。
// 不沿用同名可以避免"读旧文件时把密文当别名"这类混淆。
func (p *Plugin) refsPath() string {
	return filepath.Join(filepath.Dir(p.path), "secret_names.json")
}

func (p *Plugin) loadSecretRefs() error {
	p.refs = secretRefs{Refs: map[string]*secretRef{}}
	b, err := os.ReadFile(p.refsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 首次
		}
		return err
	}
	if fi, err := os.Stat(p.refsPath()); err == nil {
		if perm := fi.Mode().Perm(); perm != 0o600 {
			return fmt.Errorf("映射文件权限不安全：%s 是 %04o，必须 0600", p.refsPath(), perm)
		}
	}
	if err := json.Unmarshal(b, &p.refs); err != nil {
		return fmt.Errorf("映射文件损坏（不静默重建）: %w", err)
	}
	if p.refs.Refs == nil {
		p.refs.Refs = map[string]*secretRef{}
	}
	return nil
}

// saveSecretRefsLocked 落盘（调用方持锁）。
//
// ★ 0600：虽然不含值，但仍保持最紧权限 ——
// 名字本身可能透露"这台机器在管哪些凭据"，对攻击者有情报价值。
func (p *Plugin) saveSecretRefsLocked() error {
	b, err := json.MarshalIndent(p.refs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.refsPath()), 0o700); err != nil {
		return err
	}
	tmp := p.refsPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.refsPath())
}

// resolveSecretSource 解析 `value` 参数的三类来源。
//
//	${ENV_NAME}      环境变量引用 → 明文从不经工具参数（最安全）
//	@file:/path      文件内容
//	其它             字面明文（会进会话记录，调用方要告警）
//
// ★ 这条能力内核没有（内核 secret_submit 只收直传明文），
// 所以它是插件**保留**这个文件的主要理由之一。
func resolveSecretSource(spec string) (plain, source string, err error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return "", "", fmt.Errorf("值不能为空")
	}
	if varRefPattern.MatchString(s) {
		v, err := resolveSecret(s)
		if err != nil {
			return "", "", err
		}
		return v, "env", nil
	}
	if rest, ok := strings.CutPrefix(s, "@file:"); ok {
		path := strings.TrimSpace(rest)
		if path == "" {
			return "", "", fmt.Errorf("@file: 后面要给路径")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", "", fmt.Errorf("读文件失败（%s）: %w", path, err)
		}
		v := strings.TrimSpace(string(b)) // token 文件常带换行
		if v == "" {
			return "", "", fmt.Errorf("文件 %s 内容为空", path)
		}
		return v, "file", nil
	}
	return s, "literal", nil
}

// ---------------- 工具实现 ----------------

func (p *Plugin) handleEncStore(args map[string]interface{}) (interface{}, error) {
	name := strings.TrimSpace(str(args, "alias"))
	spec := str(args, "value")
	if name == "" {
		return map[string]interface{}{"error": "alias 必填"}, nil
	}
	if !validSecretName(name) {
		return map[string]interface{}{"error": "别名只允许 [A-Za-z0-9_.-]（它会进 @{别名} 引用）"}, nil
	}
	plain, source, err := resolveSecretSource(spec)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}

	sec := p.secretsAPI()
	if sec == nil || !sec.Available() {
		// ★ 响亮失败：不静默降级成"存到别处"。插件以为存好了而实际没存，
		// 比直接报错危险得多。
		return map[string]interface{}{"error": "内核变量库不可用（core.security.enabled=false？）—— 拒绝以明文或私有文件降级存储"}, nil
	}

	// ★ 值交给内核（它负责加密落盘 + 派生别名）
	alias, err := sec.Store(plain, strings.TrimSpace(str(args, "note")))
	if err != nil {
		return map[string]interface{}{"error": "登记进内核变量库失败: " + err.Error()}, nil
	}

	now := nowRFC3339()
	p.mu.Lock()
	prev, existed := p.refs.Refs[name]
	ref := &secretRef{
		Name: name, Alias: alias, Source: source,
		Note:      strings.TrimSpace(str(args, "note")),
		CreatedAt: now,
	}
	if existed {
		ref.CreatedAt = prev.CreatedAt
		ref.UpdatedAt = now
	}
	p.refs.Refs[name] = ref
	saveErr := p.saveSecretRefsLocked()
	p.mu.Unlock()
	if saveErr != nil {
		return map[string]interface{}{"error": saveErr.Error()}, nil
	}

	out := map[string]interface{}{
		"status": "ok",
		"alias":  name,  // 人用的可读名
		"ref":    alias, // 内核别名（自动合并的钥匙）
		"source": source,
		"usage":  fmt.Sprintf("在命令里用 @{%s} 引用它（配 enc_shell）", name),
		"note": "值存于**内核变量库**（进程级单例，全部插件与 agent 共享）；" +
			"本插件只保留「可读名 → 内核别名」映射，映射里没有值。",
	}
	if source == "literal" {
		out["warning"] = "你直接传了明文：它会进入本次会话记录（工具调用是会话的一部分）。" +
			"下次请优先用 ${ENV_NAME} 或 @file:/path —— 那两条路径明文不进会话。"
	}
	return out, nil
}

func (p *Plugin) handleEncList(args map[string]interface{}) (interface{}, error) {
	sec := p.secretsAPI()

	// 内核侧的元信息（别名 → 备注/命中）
	kernelMeta := map[string]secretInfoView{}
	if sec != nil && sec.Available() {
		for _, in := range sec.List() {
			kernelMeta[in.Alias] = secretInfoView{
				Source: in.Source, Note: in.Note, Hits: in.Hits, At: in.CreatedAt,
			}
		}
	}

	p.mu.Lock()
	type view struct {
		Alias   string `json:"alias"`
		Ref     string `json:"ref"`
		Source  string `json:"source"`
		Note    string `json:"note,omitempty"`
		At      string `json:"at"`
		Present bool   `json:"present_in_vault"`
	}
	out := []view{}
	for _, r := range p.refs.Refs {
		_, present := kernelMeta[r.Alias]
		out = append(out, view{
			Alias: r.Name, Ref: r.Alias, Source: r.Source, Note: r.Note,
			At: r.UpdatedAt + r.CreatedAt, Present: present,
		})
	}
	p.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })

	res := map[string]interface{}{
		"secrets": out, "count": len(out),
		"vault_available": sec != nil && sec.Available(),
		"note": "**不含值**（值在内核变量库里，且没有读取接口）。" +
			"用 @{别名} 在 enc_shell 命令里引用。" +
			"present_in_vault=false 表示该名字指向的内核变量已不存在（被删了？）。",
	}
	return res, nil
}

// secretInfoView 是内核元信息的精简视图。
type secretInfoView struct {
	Source string
	Note   string
	Hits   int
	At     string
}

// handleEncShow 查看元信息（**不返回值本体**）。
//
// 为何不提供"取回明文"的工具：那等于给 agent 一个取 secret 的入口，
// 使前面所有"明文不进会话"的努力归零。要看值，就在命令里用 @{别名}。
func (p *Plugin) handleEncShow(args map[string]interface{}) (interface{}, error) {
	name := strings.TrimSpace(str(args, "alias"))
	p.mu.Lock()
	r, ok := p.refs.Refs[name]
	var ref secretRef
	if ok {
		ref = *r
	}
	p.mu.Unlock()
	if !ok {
		return map[string]interface{}{"error": fmt.Sprintf("别名 %s 不存在", name)}, nil
	}

	out := map[string]interface{}{
		"alias": ref.Name, "ref": ref.Alias, "source": ref.Source,
		"note": ref.Note, "created_at": ref.CreatedAt, "updated_at": ref.UpdatedAt,
		"storage": "内核变量库（AES-256-GCM 落盘，密钥由内核管理）",
		"hint": "值本体不提供读取接口（那会让明文回到会话里）；" +
			"请在 enc_shell 命令里用 @" + "{" + ref.Name + "} 引用。",
	}
	// 顺带报内核侧的命中计数（可观测：这东西被用了多少次）
	if sec := p.secretsAPI(); sec != nil && sec.Available() {
		for _, in := range sec.List() {
			if in.Alias == ref.Alias {
				out["hits"] = in.Hits
				break
			}
		}
	}
	return out, nil
}

func (p *Plugin) handleEncRm(args map[string]interface{}) (interface{}, error) {
	name := strings.TrimSpace(str(args, "alias"))
	p.mu.Lock()
	ref, ok := p.refs.Refs[name]
	p.mu.Unlock()
	if !ok {
		return map[string]interface{}{"error": fmt.Sprintf("别名 %s 不存在", name)}, nil
	}

	// ★ 先删内核变量（值），再删映射 —— 反过来会留下无人引用的密文
	warnings := []string{}
	if sec := p.secretsAPI(); sec != nil && sec.Available() {
		if err := sec.Forget(ref.Alias); err != nil {
			// 映射仍要删（否则名单里留个死名字），但要如实告警
			warnings = append(warnings,
				fmt.Sprintf("内核变量 %s 删除失败（该密文可能仍在内核库里）: %v", ref.Alias, err))
		}
	} else {
		warnings = append(warnings, "内核变量库不可用 —— 只删了映射，值可能仍在")
	}

	p.mu.Lock()
	delete(p.refs.Refs, name)
	saveErr := p.saveSecretRefsLocked()
	p.mu.Unlock()
	if saveErr != nil {
		return map[string]interface{}{"error": saveErr.Error()}, nil
	}

	out := map[string]interface{}{"status": "ok", "removed": name, "ref": ref.Alias}
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	return out, nil
}

// lookupAnySecret 供 enc_shell 使用：把可读名解析成明文。
//
// 解析顺序：
//
//	① 映射表里的名字 → 拿到内核别名 → 请内核 Fill 出明文
//	② 回退到**平台凭据**（credentials.json）—— 那些不是内核变量，
//	   有平台/角色语义，仍由插件自己保管
//
// 为何要回退：平台凭据（gitea-local 等）本来就是"带平台语义的凭据"，
// 让 @{gitea-local} 在同一个 shell 里可用，避免 agent 在两套命名间切换。
func (p *Plugin) lookupAnySecret(name string, key []byte) (string, bool, error) {
	// ① 先看映射表
	p.mu.Lock()
	ref, ok := p.refs.Refs[name]
	p.mu.Unlock()
	if ok {
		sec := p.secretsAPI()
		if sec == nil || !sec.Available() {
			return "", false, fmt.Errorf(
				"%s 指向内核变量 %s，但变量库不可用（core.security.enabled=false？）", name, ref.Alias)
		}
		// ★ 用 Fill 而不是任何"按名取明文"的接口 —— Fill 要求先在文本里
		// 写出别名，而别名本身不泄漏明文（HMAC 派生）。
		out, n, _ := sec.Fill("@" + "{" + ref.Alias + "}")
		if n == 0 {
			return "", false, fmt.Errorf(
				"%s 指向的内核变量 %s 已不存在（被 secret_forget 删了？）", name, ref.Alias)
		}
		return out, true, nil
	}

	// ② 回退平台凭据
	p.creds.mu.Lock()
	a, ok := p.creds.data.Accounts[name]
	p.creds.mu.Unlock()
	if !ok {
		return "", false, nil
	}
	v, err := p.decryptAccount(a, key)
	return v, true, err
}

// secretsAPI 取内核变量库接口（可能为 nil —— 内核版本过旧或未装配）。
//
// 优先取可注入副本（测试用），生产为 nil 时走 SDK。
func (p *Plugin) secretsAPI() sdk.SecretsAPI {
	if p.vault != nil {
		return p.vault
	}
	if p.sdk == nil {
		return nil
	}
	return p.sdk.Secrets()
}

// validSecretName 校验 secret 别名。
//
// ★ 为什么不用 validName：那是**项目/通道名**的校验器（不含 `.`），
// 而 secret 引用的字符集含 `.`（见 encshell.go 的 credRefPattern）。
// 两者是两个命名空间，混用会造成「能被引用却存不进去」的自相矛盾 ——
// 而报错只说"字符不合法"，很难联想到是两处校验器不一致。
var secretNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func validSecretName(s string) bool { return secretNamePattern.MatchString(s) }
