package main

// sidecar：每个工程对话一份的 pi 运行时外壳。
//
// 协议照抄 internal/plugins/clawhubadapter/sidecar.go（本仓已验证的成熟做法）：
// JSON-RPC over stdin/stdout + pending-id 配对 + readLoop + 优雅 Close。
// 不自创协议 —— 那边踩过的坑（长行截断、pending 表泄漏、退出时唤醒等待者）不必再踩。
//
// ## 为什么需要 sidecar
//
// pi 是 Node 程序。我们**不把 pi 的 RPC 循环搬进 Go**（那是重复实现），
// 只用一个薄薄的 Node 脚本把 pi 包成一个「可反复 prompt 的会话」，Go 侧用
// JSON-RPC 跟它说话。这样：
//
//	Go 插件  ──JSON-RPC/stdio──►  sidecar(node)  ──►  pi 会话（内嵌 createAgentSession）
//
// ## 刀 1 的形态
//
// 本地内嵌。远端 ssh 是后续刀次（同一份 sidecar，只换传输层）。
// sidecar 脚本路径：优先随包分发的 runtime/sidecar.mjs，其次插件目录下的 sidecar.mjs。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	sdk "github.com/JianFeeeee/homeagentsdk/sdk"
	"sync"
	"time"
)

type sidecarRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type sidecarResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      int              `json:"id"`
	Result  *json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type sidecarProcess struct {
	branchID string
	workdir  string

	cmd    *exec.Cmd
	stdin  *bufio.Writer
	stdout io.Reader

	mu      sync.Mutex
	nextID  int
	pending map[int]chan []byte
	// onNotify 处理 sidecar **主动推送**的帧（resp.ID == 0）。
	//
	// ★ 为什么需要它（2026-10-17 实测缺陷）：
	//   deliverToPi 原来 `await s.prompt(text)` **同步等 pi 把任务跑完**，
	//   而 pi 开工程线干活要几十秒到几分钟 ⇒ 内核 60s 工具超时先到，
	//   报「执行超时（60秒）」。
	//   但通道语义是"**投递**任务"（见 outputch 的描述），不是"等完成"。
	//   ⇒ 改成：prompt 立即返回"已受理"，pi 跑完后用主动通知回投结果。
	//
	// 回调在 readLoop 里调用（已持锁外），实现方不得再进 readLoop。
	onNotify func(branchID string, payload map[string]interface{})
	closed   bool
	stopped  bool
	exited   chan struct{}
	waitErr  error

	timeout time.Duration

	// handshakeTimeout 用于 ping 握手（比普通调用短得多）。
	// 为何单列：握手撞上工具调用的 60s 上限会退化成“超时但进程已起”。
	handshakeTimeout time.Duration
}

func newSidecarProcess(branchID, workdir string, cmd *exec.Cmd,
	stdin *bufio.Writer, stdout io.Reader, timeout time.Duration) *sidecarProcess {
	sp := &sidecarProcess{
		branchID: branchID,
		workdir:  workdir,
		cmd:      cmd,
		stdin:    stdin,
		stdout:   stdout,
		pending:  map[int]chan []byte{},
		exited:   make(chan struct{}),
		timeout:  timeout,
	}
	go sp.readLoop(stdout)
	go func() {
		sp.waitErr = cmd.Wait()
		close(sp.exited)
	}()
	return sp
}

// readLoop 持续读 stdout。
//
// ⚠️ 必须持续读：不读会因管道缓冲填满而**堵住 pi**（这是 RPC 类协议最常见的死法）。
func (s *sidecarProcess) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var resp sidecarResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			// 不是协议记录：当诊断信息，不打断
			log.Printf("[commondev] sidecar %s 非协议输出: %s", s.branchID, truncate(string(line), 200))
			continue
		}
		if resp.ID == 0 {
			// 主动通知：sidecar 不等请求就推过来的帧。
			//
			// 当前用途：异步投递的**完成回投**（见 deliverToPi）。
			// 在独立 goroutine 里跑回调 —— 回调可能调 InjectText，那会
			// 走到内核（可能阻塞）；在 readLoop 里同步跑会堵住 stdout，
			// 而 stdout 一堵，pi 就写不进来（RPC 类协议最常见的死法）。
			if s.onNotify != nil {
				var payload map[string]interface{}
				if resp.Result != nil && len(*resp.Result) > 0 {
					_ = json.Unmarshal(*resp.Result, &payload)
				}
				branch := s.branchID
				fn := s.onNotify
				go fn(branch, payload)
			}
			continue
		}
		s.mu.Lock()
		ch := s.pending[resp.ID]
		delete(s.pending, resp.ID)
		s.mu.Unlock()
		if ch != nil {
			cp := make([]byte, len(line))
			copy(cp, line)
			ch <- cp // 不 close：避免 close/send 竞态
		}
	}
	// stdout 结束 ⇒ 进程已亡，唤醒所有等待者
	s.mu.Lock()
	for id, ch := range s.pending {
		delete(s.pending, id)
		ch <- nil
	}
	s.mu.Unlock()
}

func (s *sidecarProcess) call(method string, params interface{}) (interface{}, error) {
	if s.handshakeTimeout > 0 && method == "ping" {
		return s.callWithTimeout(method, params, s.handshakeTimeout)
	}
	return s.callWithTimeout(method, params, s.timeout)
}

func (s *sidecarProcess) callWithTimeout(method string, params interface{}, timeout time.Duration) (interface{}, error) {
	raw, err := s.callRawTimeout(method, params, timeout)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	var resp sidecarResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("sidecar %s 应答解析失败: %w", s.branchID, err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("sidecar %s: %s", s.branchID, resp.Error.Message)
	}
	if resp.Result == nil {
		return nil, nil
	}
	var out interface{}
	if err := json.Unmarshal(*resp.Result, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *sidecarProcess) callRaw(method string, params interface{}) ([]byte, error) {
	return s.callRawTimeout(method, params, s.timeout)
}

func (s *sidecarProcess) callRawTimeout(method string, params interface{}, timeout time.Duration) ([]byte, error) {
	ch := make(chan []byte, 1)

	s.mu.Lock()
	if s.closed || s.stopped {
		s.mu.Unlock()
		return nil, fmt.Errorf("sidecar %s 已关闭", s.branchID)
	}
	s.nextID++
	id := s.nextID
	s.pending[id] = ch

	req := sidecarRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}
	if _, err := s.stdin.Write(data); err != nil {
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}
	if _, err := s.stdin.Write([]byte("\n")); err != nil {
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}
	if err := s.stdin.Flush(); err != nil {
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()

	select {
	case raw := <-ch:
		if raw == nil {
			return nil, fmt.Errorf("sidecar %s 进程已退出", s.branchID)
		}
		return raw, nil
	case <-time.After(timeout):
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, fmt.Errorf("sidecar %s 调用 %s 超时", s.branchID, method)
	case <-s.exited:
		return nil, fmt.Errorf("sidecar %s 进程已退出", s.branchID)
	}
}

// Alive 报告进程是否还活着。
func (s *sidecarProcess) Alive() bool {
	if s == nil {
		return false
	}
	select {
	case <-s.exited:
		return false
	default:
		return true
	}
}

// Close 优雅收尾：先关 stdin 请求有序退出，超时则强杀。
//
// ★ 孤儿清理是插件的责任：绝不允许留下 pi 进程。
func (s *sidecarProcess) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	// ★ 关 stdin = 请求有序退出（与 clawhubadapter 同纪律）。
	//
	// 这里的 nil 判定不只是防御式编程：Close() 的签名承诺了 nil-safe
	// （上面 `if s == nil`），但**未完整初始化的对象**（stdin 为 nil
	// interface）同样会在这里 panic —— 而调用方看不到差别
	// （两者都是 *sidecarProcess）。
	//
	// 实测来源：`deliverToPi` / `handleBranchClose` 都可能在 startSidecar
	// 中途失败时拿到半初始化对象并调 Close；空指针 panic 会盖住真正的错误。
	if s.stdin != nil {
		_ = s.stdin.Flush()
	}

	// ★ 若从未真正启动（cmd/exited 为空），没有可等、可杀的东西 —— 直接收尾。
	//
	// 为何必须显式判：Close() 在 `if s == nil` 之后就不再检查内部字段，
	// 而 nil-safe 的承诺对**半初始化对象**同样适用（调用方只看到
	// *sidecarProcess，分不出"从未启动"与"已退出"）。
	// 实测：startSidecar 中途失败时，handleBranchClose 会拿到这种对象；
	// 不判则 `s.cmd.Process` 解引用 nil 指针 —— panic 会盖住真正的错误。
	if s.cmd == nil || s.exited == nil {
		s.markStopped()
		return
	}

	select {
	case <-s.exited:
		// 已退
	case <-time.After(3 * time.Second):
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		select {
		case <-s.exited:
		case <-time.After(2 * time.Second):
			log.Printf("[commondev] sidecar %s 强杀后仍未退出", s.branchID)
		}
	}

	s.markStopped()
}

// markStopped 置停止标记并唤醒所有挂起的调用（幂等）。
func (s *sidecarProcess) markStopped() {
	s.mu.Lock()
	s.stopped = true
	for id, ch := range s.pending {
		delete(s.pending, id)
		ch <- nil
	}
	s.mu.Unlock()
}

// ---------------- 启动 ----------------

// startSidecar 拉起某个工程对话的运行时（**按 agent_cli 选驱动**）。
//
// ★ 本函数**不知道 pi 的存在**：它只读 driver.json（或内置 pi 描述），
// 按声明起进程、注环境、传参数。换 CLI = 换一份 driver.json，这里一行不改。
//
// 契约（sidecar 脚本必须实现，driver 只负责把进程拉起来）：
//
//	ping   → {ok, session_id, ...}
//	prompt → {ok, session_id, reply}   ← session_id 必须回，Go 侧据此断言会话未漂移
//	state  → {ok, session_id, messages}
//	close  → {ok}
func (p *Plugin) startSidecar(b *Branch, sessionID string) (*sidecarProcess, error) {
	p.mu.Lock()
	existing := p.sidecars[b.ID]
	p.mu.Unlock()
	if existing != nil && existing.Alive() {
		return existing, nil // 复用
	}

	cli := strings.TrimSpace(b.AgentCLI)
	if cli == "" {
		cli = defaultAgentCLI
	}
	drv, dir, err := p.loadDriver(cli)
	if err != nil {
		return nil, err
	}

	// 脚本查找链（按优先级）：
	//
	//	① <插件数据>/runtime/<cli>/<script>   用户自备/覆盖（最高优先）
	//	② <插件目录>/rawfile/runtime/<cli>/<script>   ★ 随包分发（见下）
	//	③ <可执行文件同目录>/<script>          开发期（hmapdev debug）
	//
	// ★ ② 是必需的，不是锦上添花：sidecar 脚本此前**没有随包分发**
	// （打包只收 rawfile/ 与 skill/），而 ① 在部署时没人创建 ⇒
	// `agent_up` 在全新部署上**必然失败**（报「找不到 sidecar 脚本」），
	// 表现为"插件装了、工具在、但一拉 agent 就错"。
	//
	// 放在 rawfile/ 而不是新建约定目录：rawfile 已是"随包任意资源"的通道，
	// 且 nodeBin 已经用同一机制定位内嵌 node —— 复用同一条路，不新增概念。
	script, scriptErr := p.locateSidecarScript(drv, dir, cli)
	if scriptErr != nil {
		return nil, scriptErr
	}
	dataDir := filepath.Dir(p.path)
	rawfileDir := ""
	if rf := p.rawFiles(); rf != nil {
		rawfileDir = rf.Dir()
	}
	ctx := driverCtx{
		Workdir: b.Workdir, SessionID: sessionID,
		SessionName: "br-" + b.ID, DataDir: dataDir, PluginDir: dir,
		RawfileDir: rawfileDir,
	}

	// ★ 传输层分派：local（exec）| ssh（远端同一份 sidecar）。
	// 驱动声明（跑什么）与传输（在哪跑）是正交的 —— 这里只分两路，
	// 不随 CLI/后端数量增长。
	if b.Runtime == "ssh" {
		return p.startSSHSidecar(b, drv, ctx, cli)
	}

	node := p.resolveNode(drv)
	if node == "" {
		return nil, fmt.Errorf("找不到 node（随包分发应为 <插件>/rawfile/node/bin/node；也可设 node_bin 或用 PATH）")
	}

	// 会话目录放工作区内，避免污染用户全局会话库。
	if drv.SessionDirSuffix != "" {
		if err := os.MkdirAll(filepath.Join(b.Workdir, drv.SessionDirSuffix), 0o700); err != nil {
			return nil, err
		}
	}
	// LLM 源由**声明**决定谁准备（不是 if cli == "pi" 这种分支）。
	if err := p.prepareLLMConfig(drv, ctx); err != nil {
		return nil, err
	}

	args := append([]string{script}, drv.buildArgs(ctx)...)
	cmd := exec.Command(node, args...)
	cmd.Dir = b.Workdir
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), drv.buildEnv(ctx)...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 sidecar 失败: %w", err)
	}

	timeout := defaultSidecarTimeout
	if v, _ := p.sdk.Settings().Get("sidecar_timeout_sec"); v != nil {
		if n, err := strconv.Atoi(fmt.Sprint(v)); err == nil && n > 0 {
			timeout = time.Duration(n) * time.Second
		}
	}

	sp := newSidecarProcess(b.ID, b.Workdir, cmd, bufio.NewWriter(stdin), stdout, timeout)

	// ★ 接上"完成回投"：pi 跑完任务后主动推通知，我们把它注入回 agent。
	//
	// 这是**异步投递**的另一半（见 deliverToPi 与 sidecar.mjs 的 prompt）。
	// 没有它，任务虽然完成了但模型永远不知道结果 —— 比同步超时更糟。
	sp.onNotify = p.makePromptNotifier()

	// ★ 握手必须短且确定 —— 绝不能用默认 timeout（那是 60s，正好撞上内核的
	// 工具调用 60s 硬上限 ⇒ 工具超时但进程已起，状态"做了一半"）。
	sp.handshakeTimeout = 10 * time.Second
	if ping, err := sp.call("ping", nil); err != nil {
		sp.Close()
		return nil, fmt.Errorf("%s sidecar 握手失败（10s）: %w", cli, err)
	} else if m, ok := ping.(map[string]interface{}); ok {
		// 版本字段名按后端而异，两者都认（不要求后端改名）
		for _, k := range []string{"pi_version", "cli_version", "version"} {
			if v, ok := m[k].(string); ok && v != "" {
				b.PiVersion = v
				break
			}
		}
	}
	return sp, nil
}

// prepareLLMConfig 按驱动的 LLMConfig 声明准备 LLM 源。
//
// ★ 用**声明**而不是分支：核心只说「这个后端需要我先喂源」，
// 至于怎么喂（写哪些文件、什么格式）由各后端的实现决定。
// none = 后端自己管（如 ZCode 用内置 provider 配置）。
func (p *Plugin) prepareLLMConfig(drv driverSpec, ctx driverCtx) error {
	switch drv.LLMConfig {
	case "", "none":
		return nil
	case "pi":
		agentDir := filepath.Join(ctx.DataDir, "runtime", "agentdir")
		if err := os.MkdirAll(agentDir, 0o700); err != nil {
			return err
		}
		// ★ 导入内核的 LLM 源（用户要求：默认用 homeagent 内部配置的源）。
		// 不导入的话 pi 会报 `No API key found for the selected model`
		//（实测：agentdir/auth.json 只有 2 字节）。
		if err := p.ensurePiLLMConfig(agentDir); err != nil {
			return fmt.Errorf("导入内核 LLM 源失败（不静默回退到宿主凭据）: %w", err)
		}
		return nil
	default:
		// ★ 内嵌只实现 pi 的 LLM 喂法。别的后端要么自己管（none），
		// 要么由用户在自己的 sidecar 里处理 —— 插件不替未知 CLI 猜格式。
		return fmt.Errorf(
			"未知的 llmConfig 声明 %q（内嵌只实现 pi | none）。"+
				"自定义后端请用 none 并在自己的 sidecar 里准备配置 —— "+
				"插件不替未知 CLI 猜配置文件格式（猜错会表现为「模型不回复」）",
			drv.LLMConfig)
	}
}

// ensurePiLLMConfig 确保内置 pi 有可用的 LLM 配置。
//
// 已配置就**不覆盖**（用户可能手工调过、或用了 commondev_pi_source_add 加的源）。
func (p *Plugin) ensurePiLLMConfig(agentDir string) error {
	modelsPath := filepath.Join(agentDir, "models.json")
	if b, err := os.ReadFile(modelsPath); err == nil {
		var f piModelsFile
		if json.Unmarshal(b, &f) == nil && len(f.Providers) > 0 {
			return nil // 已有配置
		}
	}
	n, def, err := p.importKernelSources(agentDir)
	if err != nil {
		return err
	}
	log.Printf("[commondev] 已导入内核 LLM 源：%d 个，默认=%s", n, def)
	return nil
}

// sidecarScript 定位 sidecar 脚本。
func (p *Plugin) sidecarScript() string {
	cands := []string{
		filepath.Join(filepath.Dir(p.path), "runtime", "sidecar.mjs"),
		filepath.Join(filepath.Dir(p.path), "sidecar.mjs"),
	}
	// 开发期：与插件源码同目录（hmapdev debug 场景）
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), "sidecar.mjs"))
	}
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// nodeBin 定位 node：随包分发优先，其次 PATH。
// nodeBin 定位 node，**随包内嵌优先**。
//
// 优先级（用户要求：白板机器也能用）：
//
//	① setting node_bin（显式覆盖，排查用）
//	② <插件目录>/rawfile/node/bin/node   ← 随包内嵌（白板机器靠这条）
//	③ PATH 里的 node                      ← 回退（老部署/开发机）
//
// ★ ② 的路径由 rawfile 机制提供：hmapdev 把工程根 `rawfile/` 打进包，
// 内核解包保留结构 ⇒ 落在插件目录下。所以插件"内嵌 node"只需把
// node 放进工程的 rawfile/ 并从 plg.json 声明（或直接放 rawfile/ 下）。
func (p *Plugin) nodeBin() string {
	if v, _ := p.sdk.Settings().Get("node_bin"); v != nil {
		if s, _ := v.(string); strings.TrimSpace(s) != "" {
			return s
		}
	}
	// ② 随包内嵌（rawfile 机制）
	if rf := p.rawFiles(); rf != nil && rf.Exists("node/bin/node") {
		if cand := filepath.Join(rf.Dir(), "node", "bin", "node"); fileExists(cand) {
			return cand
		}
	}
	// 兼容旧路径（rawfile 之前的约定）
	if cand := filepath.Join(filepath.Dir(p.path), "runtime", "node", "bin", "node"); fileExists(cand) {
		return cand
	}
	if cand, err := exec.LookPath("node"); err == nil {
		return cand
	}
	return ""
}

// rawFiles 取随包资源接口（可能为 nil —— 旧内核未注入）。
func (p *Plugin) rawFiles() sdk.RawFilesAPI {
	if p.sdk == nil {
		return nil
	}
	return p.sdk.RawFiles()
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// locateSidecarScript 按优先级链定位 sidecar 脚本。
//
// 查找链（按优先级）：
//
//	① <插件数据>/runtime/<cli>/<script>            用户自备/覆盖（最高优先）
//	② <插件目录>/rawfile/runtime/<cli>/<script>    ★ 随包分发
//	③ <可执行文件同目录>/<script>                   开发期（hmapdev debug）
//
// ★ ② 是必需的，不是锦上添花。实测（2026-10-15）发现：sidecar 脚本此前
// **没有随包分发** —— 打包只收 rawfile/ 与 skill/，而 ① 在部署时没人创建。
// 于是全新部署上 `agent_up` **必然失败**（「找不到 pi 的 sidecar 脚本」），
// 表现为"插件装了、工具都在、但一拉 agent 就错"，且错误信息指向一个
// 用户从没听说过的目录。
//
// 为什么放在 rawfile/ 而不是新开一个约定目录：rawfile 已经是
// "随包任意资源"的通道，且 nodeBin 已经用**同一机制**定位内嵌 node ——
// 复用同一条路，不新增概念、不新增内核理解负担。
func (p *Plugin) locateSidecarScript(drv driverSpec, cliDir, cli string) (string, error) {
	// ① 用户自备（插件数据目录）——允许覆盖随包脚本
	if cand := filepath.Join(cliDir, drv.Script); fileExists(cand) {
		return cand, nil
	}
	// ② 随包分发（rawfile/runtime/<cli>/）
	if rf := p.rawFiles(); rf != nil {
		rel := filepath.ToSlash(filepath.Join("runtime", cli, drv.Script))
		if rf.Exists(rel) {
			return filepath.Join(rf.Dir(), "runtime", cli, drv.Script), nil
		}
	}
	// ③ 开发期：与可执行文件同目录
	if exe, e := os.Executable(); e == nil {
		if alt := filepath.Join(filepath.Dir(exe), drv.Script); fileExists(alt) {
			return alt, nil
		}
	}
	// 全部落空：错误必须**列出找过哪里**，否则用户只能猜
	return "", fmt.Errorf(
		"找不到 %s 的 sidecar 脚本 %s；已找过：① %s ② <插件>/rawfile/runtime/%s/%s ③ <可执行文件目录>/%s。"+
			"若要自带 CLI，请把 driver.json 与脚本放到 <插件数据>/runtime/%s/",
		cli, drv.Script, filepath.Join(cliDir, drv.Script), cli, drv.Script, drv.Script, cli)
}

// makePromptNotifier 构造"任务完成回投"的回调。
//
// ## 它解决什么问题
//
// 异步投递后，pi 在后台跑完任务，结果必须回到 agent 的上下文里 ——
// 否则模型只知道"投递成功了"，永远不知道 pi 干了什么、成功还是失败。
//
// ## 为什么走 inputch（而不是 return）
//
// 投递那一刻 tool 调用早已返回（这是异步的意义）。完成事件到时，
// 已经没有"谁在等这个返回值"了。唯一能把信息送进 agent 的通道是
// **注入输入**（InjectTextTo），而 commondev 已经为每条工程线注册了
// inputch（`commondev/<id>`，NoMemory=true —— 控制流不进记忆）。
//
// ## 失败不静默
//
// 注入失败要记日志：否则表现是"任务跑完了、模型毫无反应"，
// 排查时看不出任何线索。
func (p *Plugin) makePromptNotifier() func(branchID string, payload map[string]interface{}) {
	return func(branchID string, payload map[string]interface{}) {
		if payload == nil {
			return
		}
		event, _ := payload["event"].(string)
		sess, _ := payload["session_id"].(string)

		var text string
		switch event {
		case "prompt_done":
			reply, _ := payload["reply"].(string)
			if strings.TrimSpace(reply) == "" {
				reply = "（pi 未返回文本回复）"
			}
			text = fmt.Sprintf("[工程线 %s 任务完成 · session=%s]\n%s", branchID, sess, reply)
			// ★ 回填台账：否则台账永远停在 accepted，谁都查不到结果
			// （实测：agent 为此起了 6 轮 timer 空等）。
			p.completeDelivery(branchID, sess, "done", reply)
		case "prompt_error":
			e, _ := payload["error"].(string)
			text = fmt.Sprintf("[工程线 %s 任务失败 · session=%s]\n错误：%s", branchID, sess, e)
			p.completeDelivery(branchID, sess, "failed", e)
		default:
			log.Printf("[commondev] 未知的通知事件 %q（branch=%s），忽略", event, branchID)
			return
		}

		// source 用插件名（内核据此识别来源），channel 是该线的 inputch。
		inCh := inputChannelName(branchID)
		log.Printf("[commondev] 回投任务结果到 %s（event=%s）", inCh, event)
		// InjectText 无返回值（内部已做错误日志），调用即注入。
		p.sdk.InjectText("commondev", inCh, text)
	}
}
