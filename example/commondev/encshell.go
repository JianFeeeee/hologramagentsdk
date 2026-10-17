package main

// 加密 shell：agent 用特殊格式让插件为它**填充解密后的凭据**，再执行命令。
//
// ## 用户的要求（2026-10-14）
//
//	「插件为 agent 额外提供一个加密 shell 工具，agent 可以通过特殊的格式
//	  让该插件工具为其填充加密数据」
//
// 即一个 **credential broker**：secret 加密存盘、agent 只写**引用**，
// 明文只在插件的执行边界上短暂存在。
//
// ## 三条实测得出的实现纪律（不是设计推测）
//
// ### ① secret 绝不进 argv —— 脚本必须走 **stdin**
//
// 实测（扫全系统 /proc/*/cmdline）：
//
//	明文作为 argv 参数        → ★ 泄露（任意同机进程可读）
//	sh -c "…{secret}…"        → ★ 泄露（脚本本身就是 argv）
//	**脚本经 stdin 喂给 bash** → ✅ 不泄露
//
// ⇒ 实现上：`bash` 的 argv 只有 `["bash"]`，脚本体经 stdin 管道送入。
// 这条不能妥协：`ps -eo args` 是任何本机进程都能跑的。
//
// ### ② 输出必须脱敏 —— 否则明文经 stdout 进模型上下文
//
// 实测：`echo "token is $T"` 的输出会原样回传。而工具返回值**会进模型上下文**，
// 这是最危险的泄露面。⇒ 输出回传前必须把已知 secret 值替换掉。
//
// ### 这两条只防**意外**，不防**恶意**
//
// 必须诚实：agent 可以 `echo $TOKEN | base64` 或 `xxd` 绕过字符串脱敏。
// 所以本工具**不是沙箱**，其安全性来自：
//   · secret 不进 argv（防同机窥探）
//   · 输出脱敏（防意外回显进上下文）
//   · **不防恶意 exfiltration** —— 要防那个必须限制能执行的命令集合。
// 这条边界写进工具描述，不假装安全。
//
// ## 特殊格式
//
// 命令里用 `@{name}` 引用一条凭据（如 `@{gitea-local}`），插件在冻结的脚本里
// 把它替换为解密后的明文。为什么用 `@{...}` 而不是 `${...}`：
//   · `${...}` 与 shell 自己的变量语法冲突，agent 很容易误用；
//   · `@{...}` 不会与任何常见 shell 语法撞车，拼错时行为可预期。

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// credRefPattern 匹配命令里的凭据引用 @{name}。
var credRefPattern = regexp.MustCompile(`@\{([A-Za-z0-9_.-]{1,64})\}`)

// ---------------- 加密存储 ----------------
//
// 为何要"加密"：凭据文件虽是 0600，但它在插件的**数据目录**里，会被备份、
// 会被 `tar` 打包、可能随日志外带。加密后即使文件外泄，没有密钥也不可用。
//
// 密钥从哪来（**不落盘**）：
//   1. `${COMMONDEV_CRED_KEY}` 环境变量（推荐：systemd 注入）
//   2. `<data>/master.key`（0600，自动生成）—— 便利与安全之间取后者弱一档，
//      但它至少与凭据文件**不同位置**，且本插件把整个数据目录权限视为 0700。
//
// 铁律：**没有密钥 ⇒ 响亮报错**，绝不静默退回明文存盘。

const masterKeyEnv = "COMMONDEV_CRED_KEY"

// masterKey 返回 32 字节 AES-256 密钥。
func (p *Plugin) masterKey() ([]byte, error) {
	if v := strings.TrimSpace(os.Getenv(masterKeyEnv)); v != "" {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(b) != 32 {
			// 也接受原始 32 字节字符串（方便人手工设）
			if len(v) == 32 {
				return []byte(v), nil
			}
			return nil, fmt.Errorf(
				"%s 必须是 base64 编码的 32 字节密钥（或正好 32 字符）", masterKeyEnv)
		}
		return b, nil
	}
	// 回退：数据目录下的 master.key
	path := filepath.Join(filepath.Dir(p.path), "master.key")
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b[:32], nil
	}
	// 首次：生成并落盘 0600
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return key, nil
}

// sealSecret 用 AES-256-GCM 加密明文，返回 base64（nonce 前置）。
func sealSecret(key []byte, plain string) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

// openSecret 解密 sealSecret 的产物。
func openSecret(key []byte, enc string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", fmt.Errorf("密文不是合法 base64: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("密文太短")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("解密失败（密钥不对或数据损坏）: %w", err)
	}
	return string(plain), nil
}

// sealAccounts 把所有明文凭据加密后落盘。
//
// 存储形态（credentials.json）：
//
//	{ "accounts": { "<name>": { "sealed": "<base64>", ... } } }
//
// 兼容：旧版明文 `token` 字段仍能读（并在下次 save 时自动转为 sealed）。
func (cs *credStore) sealAllLocked() error {
	key, err := cs.keyProvider()
	if err != nil {
		return err
	}
	for _, a := range cs.data.Accounts {
		if a.Sealed != "" {
			continue // 已加密
		}
		plain, err := resolveSecret(a.Token) // 可能是 ${ENV}
		if err != nil {
			// 引用解析不了：**保留原样不加密**，等使用者修好环境再加密
			// （这里不报错是刻意的：否则一个未设的环境变量会让整份凭据无法保存）
			continue
		}
		enc, err := sealSecret(key, plain)
		if err != nil {
			return err
		}
		a.Sealed = enc
		a.Token = "" // ★ 明文立即从内存结构里清掉
	}
	return nil
}

// ---------------- shell 执行 ----------------

// shellRequest 是一次加密 shell 调用的参数。
type shellRequest struct {
	Command string
	Workdir string
	Timeout time.Duration
	Env     map[string]string
}

// shellResult 是执行结果。
type shellResult struct {
	Output   string
	ExitCode int
	Redacted int // 脱敏替换发生次数（可观测：非 0 说明命令试图回显凭据）
	Err      string
}

// runEncryptedShell 执行一条带凭据填充的命令。
//
// 流程（每一步都对应上面的一条纪律）：
//
//	① 扫命令里的 @{name} 引用 → 解密取值
//	② 把明文填进**脚本体**（不是 argv）
//	③ 脚本经 **stdin** 喂给 bash（argv 只有 "bash"）
//	④ 收集输出 → **脱敏**（把已知 secret 换成 ***）
//	⑤ 擦除内存里的明文副本
func (p *Plugin) runEncryptedShell(req shellRequest) shellResult {
	res := shellResult{}

	script := req.Command
	// ① 收集引用
	refs := credRefPattern.FindAllStringSubmatch(script, -1)
	secrets := map[string]string{} // 明文值 → 用于脱敏
	var missing []string

	p.creds.mu.Lock()
	key, kerr := p.creds.keyProvider()
	p.creds.mu.Unlock()
	if kerr != nil {
		res.Err = "无法取得凭据密钥: " + kerr.Error()
		return res
	}

	// 收集本次要用到的明文（执行完必须擦）
	var plaintexts []string
	defer func() {
		// ⑤ 尽力擦除（Go 里字符串不可变，这里擦可写的副本）
		for i := range plaintexts {
			plaintexts[i] = strings.Repeat("\x00", len(plaintexts[i]))
		}
		for k := range secrets {
			delete(secrets, k)
		}
	}()

	// 解密由 lookupAnySecret 负责（它会自己取锁）；这里不再直接持有 accounts。

	for _, m := range refs {
		name := m[1]
		// ★ 统一查找：先在加密信息库（enc_store 存的），再回退到平台凭据。
		// 这样 @{gitea-local}（平台）与 @{gh-token}（通用 KV）在同一个 shell 里都能用。
		plain, found, err := p.lookupAnySecret(name, key)
		if err != nil {
			res.Err = fmt.Sprintf("加密信息 %s 解密失败: %v", name, err)
			return res
		}
		if !found {
			missing = append(missing, name)
			continue
		}
		if plain == "" {
			res.Err = fmt.Sprintf("加密信息 %s 解析为空", name)
			return res
		}
		script = strings.ReplaceAll(script, m[0], plain)
		secrets[plain] = name
		plaintexts = append(plaintexts, plain)
	}
	if len(missing) > 0 {
		res.Err = fmt.Sprintf(
			"命令引用了不存在的别名: %s（用 enc_list 看加密信息、commondev_account_list 看平台凭据）",
			strings.Join(missing, ", "))
		return res
	}

	// 超时保护
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 50 * time.Second // ★ 留在内核 60s 工具上限之内
	} else if timeout > 55*time.Second {
		timeout = 55 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// ③ argv 只有 bash —— 脚本走 stdin
	cmd := exec.CommandContext(ctx, "bash")
	cmd.Dir = req.Workdir
	// ★ 不把 secret 放进 env（env 可被 /proc/PID/environ 读）
	cmd.Env = append(os.Environ(), "COMMONDEV_ENCRYPTED_SHELL=1")
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdin = strings.NewReader(script)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	// ④ 脱敏
	out, n := redactSecrets(buf.String(), secrets)
	res.Output = out
	res.Redacted = n

	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
		} else if ctx.Err() == context.DeadlineExceeded {
			res.Err = fmt.Sprintf("执行超时（%s）", timeout)
		} else {
			res.Err = err.Error()
		}
	}
	return res
}

// redactSecrets 把输出里的已知 secret 值替换掉。
//
// 返回替换次数：非 0 说明命令**试图回显凭据** —— 这是可观测信号，
// 应让使用者知道（而不是静默替换了事）。
func redactSecrets(s string, secrets map[string]string) (string, int) {
	n := 0
	// 长值优先替换：避免短 secret 是长 secret 子串时替换不干净
	vals := make([]string, 0, len(secrets))
	for v := range secrets {
		if len(v) >= 4 { // 太短的值不做替换（会把正常文本打烂）
			vals = append(vals, v)
		}
	}
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	for _, v := range vals {
		c := strings.Count(s, v)
		if c > 0 {
			s = strings.ReplaceAll(s, v, "***REDACTED***")
			n += c
		}
	}
	return s, n
}

// decryptAccount 取一条凭据的明文（sealed 优先，兼容旧明文/变量引用）。
func (p *Plugin) decryptAccount(a *Account, key []byte) (string, error) {
	if a == nil {
		return "", fmt.Errorf("凭据不存在")
	}
	if a.Sealed != "" {
		return openSecret(key, a.Sealed)
	}
	// 兼容路径：尚为明文或 ${ENV} 引用
	return resolveSecret(a.Token)
}

// ---------------- 工具实现 ----------------

func (p *Plugin) handleEncryptedShell(args map[string]interface{}) (interface{}, error) {
	command := str(args, "command")
	if strings.TrimSpace(command) == "" {
		return map[string]interface{}{"error": "command 必填"}, nil
	}
	workdir := strings.TrimSpace(str(args, "workdir"))
	timeoutSec := 0
	if v, ok := args["timeout_sec"].(float64); ok {
		timeoutSec = int(v)
	}

	// workdir 必须在某个工程对话的工作目录里（防止用它去读任意目录）
	if workdir != "" {
		abs, err := filepath.Abs(workdir)
		if err != nil {
			return map[string]interface{}{"error": err.Error()}, nil
		}
		if !p.isInsideProgramRoot(abs) {
			return map[string]interface{}{
				"error": "workdir 必须在工程工作目录下（program/）；" +
					"给相对路径或省略以使用当前工作目录",
			}, nil
		}
		workdir = abs
	}

	res := p.runEncryptedShell(shellRequest{
		Command: command,
		Workdir: workdir,
		Timeout: time.Duration(timeoutSec) * time.Second,
	})

	out := map[string]interface{}{
		"output":    truncate(res.Output, 6000),
		"exit_code": res.ExitCode,
	}
	if res.Err != "" {
		out["error"] = res.Err
	}
	if res.Redacted > 0 {
		out["redacted_count"] = res.Redacted
		out["warning"] = fmt.Sprintf(
			"输出里有 %d 处凭据明文，已替换为 ***REDACTED***。命令不应回显凭据。",
			res.Redacted)
	}
	return out, nil
}

// isInsideProgramRoot 判断路径是否落在 program/ 下。
func (p *Plugin) isInsideProgramRoot(abs string) bool {
	root, err := filepath.Abs(p.programRoot())
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

// listCredRefs 返回命令里引用到的凭据名（供工具描述/调试）。
func listCredRefs(cmd string) []string {
	var out []string
	for _, m := range credRefPattern.FindAllStringSubmatch(cmd, -1) {
		out = append(out, m[1])
	}
	return out
}

// ensureJSON 小工具（保证 json 包被使用，避免不同构建标签下的未用导入）
var _ = json.Marshal
