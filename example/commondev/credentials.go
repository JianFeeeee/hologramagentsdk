package main

// 凭据存储：**保留的通用部分**。
//
// ## 2026-10-15 架构调整（用户拍板）
//
// **git 托管平台专有的部分已移出插件**（`platformSpec` / `account_*` 工具 /
// 角色语义 / 按平台构造认证头）—— 那些是**平台知识**，进 skill 由 agent
// 用 `safeshell` + `curl` 操作。理由：每接一个托管平台都要改 Go 代码、
// 重编译、重启服务，而平台差异只是「API 前缀 + 认证头」两个维度的不同。
//
// **本文件保留的是与平台无关的部分**：
//   · 凭据的加密落盘（AES-256-GCM，自持密钥）
//   · `${ENV_VAR}` 变量引用解析
//   · 「不泄漏值」的展示形态
//
// 平台的用法见 skill `commondev-discipline`（含 Gitea/GitHub/GitLab/Gitee
// 的认证头对照表、PR 与分支保护的 curl 命令、以及三个实测踩到的 API 坑）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Account 是一条凭据。
//
// ★ 已移除 `Platform` / `Role` / `BaseURL` 等**平台语义**字段：
// 本插件不再按平台构造请求，也不再有「执行侧/主管侧」的角色区分 ——
// 那是平台侧的（分支保护）与 skill 侧的知识。
type Account struct {
	Name string `json:"name"`
	// Token 是**旧路径**：明文或 `${ENV_VAR}` 引用。
	// ★ 新登记会立即加密进 Sealed，本字段随即清空（见 sealAllLocked）。
	// 本字段**绝不出现在任何工具返回值里**（见 redacted）。
	Token string `json:"token,omitempty"`
	// Sealed 是 AES-256-GCM 加密后的凭据（base64，nonce 前置）。
	//
	// 为何要加密：文件虽 0600，但会被备份、打包、随日志外带；
	// 加密后即使文件外泄，没有密钥也不可用。
	Sealed string `json:"sealed,omitempty"`
	// Username 仅用于 git 提交身份（可选）。
	Username  string `json:"username,omitempty"`
	Email     string `json:"email,omitempty"`
	Note      string `json:"note,omitempty"`
	CreatedAt string `json:"created_at"`
}

// credentialsFile 是磁盘上的凭据集合（独立文件，0600）。
type credentialsFile struct {
	Accounts map[string]*Account `json:"accounts"`
}

// varRefPattern 匹配 ${VAR} 或 $VAR 形式的变量引用。
var varRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// credStore 是凭据管理器（与 Plugin 同生命周期）。
type credStore struct {
	mu   sync.Mutex
	path string
	data credentialsFile

	// keyProvider 返回加密密钥（由 Plugin 注入）。
	keyProvider func() ([]byte, error)
}

func newCredStore(path string) (*credStore, error) {
	cs := &credStore{path: path, data: credentialsFile{Accounts: map[string]*Account{}}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cs, nil
		}
		return nil, err
	}
	// ★ 启动时校验权限：凭据文件必须 0600（本仓纪律：响亮失败，不静默降级）
	if fi, err := os.Stat(path); err == nil {
		if perm := fi.Mode().Perm(); perm != 0o600 {
			return nil, fmt.Errorf(
				"凭据文件权限不安全：%s 是 %04o，必须是 0600（它是能 push 用户仓库的凭据）",
				path, perm)
		}
	}
	if err := json.Unmarshal(b, &cs.data); err != nil {
		return nil, fmt.Errorf("凭据文件损坏（拒绝静默重建）: %w", err)
	}
	if cs.data.Accounts == nil {
		cs.data.Accounts = map[string]*Account{}
	}
	return cs, nil
}

// saveLocked 写入并确权（调用方持锁）。
func (cs *credStore) saveLocked() error {
	b, err := json.MarshalIndent(cs.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := cs.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, cs.path)
}

// resolveSecret 把凭据里的变量引用解析成实际值。
//
// 返回错误的情形（都不静默降级）：
//   - 引用了未设置的环境变量
//   - 解析结果为空
func resolveSecret(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("凭据为空")
	}
	var missing []string
	out := varRefPattern.ReplaceAllStringFunc(raw, func(m string) string {
		name := m
		if strings.HasPrefix(m, "${") {
			name = m[2 : len(m)-1]
		} else {
			name = m[1:]
		}
		v, ok := os.LookupEnv(name)
		if !ok || v == "" {
			missing = append(missing, name)
			return ""
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf(
			"凭据引用的环境变量未设置: %s —— 请在插件进程的环境里提供它（不静默当空串，"+
				"否则会退化成匿名访问、表现成 401 而非“凭据没配”）",
			strings.Join(missing, ", "))
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("凭据解析后为空")
	}
	return out, nil
}

// redacted 返回凭据的可安全展示形态：**绝不暴露 secret**。
func (a *Account) redacted() map[string]interface{} {
	out := map[string]interface{}{
		"name":      a.Name,
		"username":  a.Username,
		"token_ref": tokenRefLabel(a.Token),
		"token_set": a.Sealed != "" || strings.TrimSpace(a.Token) != "",
	}
	if a.Note != "" {
		out["note"] = a.Note
	}
	if a.Sealed != "" {
		out["storage"] = "encrypted(AES-256-GCM)"
	} else {
		out["storage"] = "plaintext-or-envref"
	}
	return out
}

// tokenRefLabel 描述凭据来源但**不泄露值**。
func tokenRefLabel(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if varRefPattern.MatchString(raw) {
		return raw // ${VAR} 是引用不是秘密，展示它对排查有用
	}
	return "（明文已设置）"
}

// credentialsPath 是凭据文件路径（与台账同目录）。
func (p *Plugin) credentialsPath() string {
	return filepath.Join(filepath.Dir(p.path), "credentials.json")
}
