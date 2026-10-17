package main

// commondev：主管式多仓库开发流水线的运行时外壳（刀 1）。
//
// 设计文档（核心仓）：docs/zh/commondev-design.md
//                     docs/zh/commondev-agent-runtime-design.md
// 纪律（skill）：commondev-discipline
//
// ## 本插件做什么
//
// 用户拍板的总纲：**对 pi 做的事，本质就是「把给 pi 发消息包装一下、
// 把 pi 返回包装一下」，并深度融合内核的 IO 抽象与中断系统 —— pi = 一个 Device。**
//
// 所以这里**不实现**调度器 / 会话路由 / 中断栈，全部用内核已有的：
//
//	agent → pi   output_send__commondev_<id>(payload=…)  ← 内核输出门，本插件只写通道 handler
//	pi → agent   InjectTextTo("commondev/<id>", …)       ← 内核按 inputch Owner 路由
//	中断 pi      InjectInterruptTextOpts(…, Priority)     ← 内核调度器
//
// ## 刀 1 的范围（最小可跑链路）
//
//	台账（projects/branches/deliveries）+ 每工程一对通道 + sidecar 投递
//	**不含**：判据/变异、PR、多平台凭据、远端 ssh（后续刀次）
//
// ## 关键纪律（每一条都有实测依据）
//
//  1. **先有 session，才有通道**：通道是「与某个具体 pi 会话对话的入口」，
//     没 session 注册出来是空壳 ⇒ branch_open 后必须 agent_up。
//  2. **outputch 名不能带斜杠**：它会被拼成 output_send__<name>，上游要求
//     ^[a-zA-Z0-9_-]{1,64}$；违反的后果是**整条请求 400、agent 整体失语**
//     （实测：带斜杠会产出 output_send__commondev/br-1）。
//     ⇒ inputch 用 `commondev/<id>`（可含斜杠），outputch 用 `commondev_<id>`。
//  3. **通道控制流不进记忆**：ChannelDef 一律 NoMemory，
//     避免污染召回与场面指纹。
//  4. **投递必须落同一个 session**：台账记 session_id，投递后断言它未变。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/JianFeeeee/homeagentsdk/sdk"
)

const (
	// 一次 sidecar 调用的默认超时。pi 干活可能很久，投递本身要快。
	defaultSidecarTimeout = 60 * time.Second
)

// safeNamePattern 是**非法字符**字符集（用于把任意名字派生为安全名）。
//
// ⚠️ 它匹配的是「违法字符」，因此**不能**用它 MatchString 判断合法性：
// 合法名不含违法字符 ⇒ 返回 false（本插件初版就因这个反了的用法，
// 把合法项目名 `cdtest` 判成非法 —— 由端到端跑时的 agent 发现）。
// 判断合法用 validNamePattern。
var safeNamePattern = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// validNamePattern 是**合法名**白名单（^...$ 钉住整串），用于校验。
var validNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// validName 报告名字是否可直接用作目录名 / 通道名 / 工具名。
func validName(s string) bool { return validNamePattern.MatchString(s) }

// Project 是一个被登记的仓库。
type Project struct {
	Name          string `json:"name"`
	RemoteURL     string `json:"remote_url"`
	Platform      string `json:"platform,omitempty"` // github|gitea|gitcode
	DefaultBranch string `json:"default_branch,omitempty"`
	MirrorPath    string `json:"mirror_path,omitempty"`
	CreatedAt     string `json:"created_at"`
}

// Branch 是一条工程对话（分支线）：四元组 + 状态。
//
// ★ 四元组缺一不可：workdir / session_id / runtime / channels。
type Branch struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	WorkDesc  string `json:"work_desc,omitempty"`
	AgentName string `json:"agent_name,omitempty"`
	BaseSHA   string `json:"base_sha,omitempty"`
	Status    string `json:"status"` // open|review|merged|abandoned

	// ↓ 四元组
	Workdir   string `json:"workdir,omitempty"`
	SessionID string `json:"session_id,omitempty"` // ★ 续投的唯一依据
	// pi 会话列表可读名：br-<project>-<slug>（U6）。
	SessionName string `json:"session_name,omitempty"`
	// 启动时从 sidecar ping 回显的内层 pi 版本（U8）。
	PiVersion string `json:"pi_version,omitempty"`
	Runtime   string `json:"runtime,omitempty"` // local|ssh（刀 1 只有 local）
	// AgentCLI 是**执行后端**（与 Runtime 正交）：
	//   pi（默认）| zcode | ...（实测由 runtime/<cli>/driver.json 提供）
	// ★ 两个轴分开：Runtime 决定"在哪跑"，AgentCLI 决定"跑什么"。
	AgentCLI string `json:"agent_cli,omitempty"`
	// SSHHost 是远端目标（user@host 或 ~/.ssh/config 别名），仅 runtime=ssh 用。
	SSHHost  string `json:"ssh_host,omitempty"`
	InputCh  string `json:"input_ch,omitempty"`  // commondev/<id>
	OutputCh string `json:"output_ch,omitempty"` // commondev_<id>

	CreatedAt string `json:"created_at"`
	ClosedAt  string `json:"closed_at,omitempty"`
}

// Delivery 是一次投递记录（幂等键 + 结果）。
type Delivery struct {
	Key       string `json:"key"`
	BranchID  string `json:"branch_id"`
	Text      string `json:"text"`
	SessionID string `json:"session_id"`
	Status    string `json:"status"` // sent|failed
	Result    string `json:"result,omitempty"`
	At        string `json:"at"`
}

// Job 是一个后台作业（慢操作异步化）。
//
// ★ 为何必须异步（实测教训）：内核把**每次工具调用**硬限制在 **60 秒**
// （internal/agent/core/toolcall.go:62，硬编码无配置），而 `git clone --mirror`
// 一个仓库耗时不定。同步 clone 的后果是：工具报超时，**但 mirror 其实已建好**
// —— 调用方只看到失败，状态却是“做了一半”，这是最坏的一种失败。
//
// ⇒ 工具只做快操作（登记台账）+ 发起后台作业，慢活由状态工具报告。
type Job struct {
	ID        string `json:"id"`
	BranchID  string `json:"branch_id"`
	Kind      string `json:"kind"`   // prepare_workdir|clone_mirror|start_pi|...
	Status    string `json:"status"` // pending|running|done|failed
	Detail    string `json:"detail,omitempty"`
	Err       string `json:"err,omitempty"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at,omitempty"`
}

// Ledger 是插件台账（不存凭据 —— 凭据另有 0600 文件，刀 2 落地）。
type Ledger struct {
	Projects   map[string]*Project `json:"projects"`
	Branches   map[string]*Branch  `json:"branches"`
	Jobs       map[string]*Job     `json:"jobs,omitempty"`
	Deliveries []Delivery          `json:"deliveries"`
	// ★ Criteria 是主管定义的判据（刀 4）。ReviewRuns 是复核历史。
	Criteria   map[string]*Criteria `json:"criteria,omitempty"`
	ReviewRuns []ReviewRun          `json:"review_runs,omitempty"`
	// PiVersion 是本插件随包/运行时声明的 pi 版本（U8）。
	PiVersion string `json:"pi_version,omitempty"`
	// ★ Interceptions 已移除：拦截记录由**内核**拥有并暴露
	//（内核日志 + secret_list 的命中计数），插件不再重复维护一份。
}

type Plugin struct {
	name string
	sdk  *sdk.PluginSDK

	mu sync.Mutex
	// prepMu 串行化"准备目录"这类慢操作（本地/远端共用）。
	//
	// ★ 为什么必须串行：准备流程的第一步是"mirror 不存在就建"——
	// 而那是**检查后行动**（TOCTOU）：两个 goroutine 同时判定"不存在"，
	// 然后都去 clone，第二个就会撞上
	//   fatal: destination path '.../x.git' already exists and is not an empty directory
	//
	// 触发场景是真实的：branch_open 的目录准备在后台跑，用户看 job 还没
	// done 就再点一次 workdir_prepare（那是完全自然的操作），于是两个
	// goroutine 同时准备同一个 mirror。实测 3/6 次复现。
	//
	// 用一整把锁而不是按 mirror 分锁：准备是低频重操作（一次 clone），
	// 串行化的代价远小于把这一步做错的代价。
	prepMu sync.Mutex
	ledger *Ledger
	path   string

	// ★ 凭据独立文件（0600）—— 不进 config.db（那是 644 全局可读）。
	// 见 credentials.go 的头部纪律。
	creds *credStore

	// vault 是变量库接口的**可注入副本**（仅测试用；生产为 nil ⇒ 走 sdk）。
	vault sdk.SecretsAPI

	// lastReview 缓存最近一次复核结论（供 review_result 取）。
	lastReview map[string]interface{}

	// ★ 可读名 → 内核别名 的映射（**不含值**）。
	// 值存于内核变量库（进程级单例，全部插件与 agent 共享）。
	// 见 encstore.go 的头部纪律。
	refs secretRefs

	// ★ 敏感信息拦截由内核接管（见 Start 里的说明）。
	// ★ 加密存储由内核变量库接管（见 encstore.go 的头部说明）。

	// sidecars 是每个工程对话一份的 sidecar（刀 1：本地内嵌）。
	sidecars map[string]*sidecarProcess

	// endpoints 是**远端端点表**：按 host 存一套、按 (host, cli) 覆盖一套。
	// 为什么不能只有一组全局 settings —— 见 endpoints.go 头部。
	endpoints *endpointTable
}

// ---------------- 生命周期 ----------------

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	p.sdk = s
	s.SetAutoRestart(true)
	p.sidecars = map[string]*sidecarProcess{}

	dataDir := s.Settings().DataDir()
	p.path = filepath.Join(dataDir, "ledger.json")
	if err := p.loadLedger(); err != nil {
		return fmt.Errorf("加载台账失败: %w", err)
	}
	// 凭据存储：独立文件 + 启动时校验权限（不对则拒绝启动）。
	cs, err := newCredStore(p.credentialsPath())
	if err != nil {
		return fmt.Errorf("凭据存储不可用: %w", err)
	}
	cs.keyProvider = p.masterKey
	p.creds = cs
	// 加密信息库（通用 KV，独立文件）
	if err := p.loadSecretRefs(); err != nil {
		return fmt.Errorf("凭据名映射不可用: %w", err)
	}
	// 远端端点表（按 host / (host,cli) 存 CLI 路径等）。
	// 损坏必须响亮 —— 静默用空表会让所有远端配置凭空消失。
	if err := p.loadEndpoints(); err != nil {
		return fmt.Errorf("端点表不可用: %w", err)
	}
	// ★ 敏感信息拦截已由**内核**接管（docs/zh/kernel-secret-firewall-design.md）。
	//
	// 原先插件自带一套拦截（StageOnInput + StageAfterToolcall），但它的覆盖面
	// **严格小于内核版**：
	//   · 插件只能看到 stageCtx.RawMessage / ToolResults
	//   · 内核还能覆盖 Input / Response / Blocks / memory_candidate（记忆侧路）
	//   · 且内核拦在 provider 层，插件拦不住"已进 f.Msgs 但未出网"的窗口
	//
	// 两套并存还有个真实害处：插件版会在内核版**之后**再扫一遍，
	// 而那时明文已是别名（内核先换了）⇒ 纯属空转，还会把别名二次改写。
	// 升级路径：老版本存的明文凭据，启动时一次性加密落盘。
	// （失败不阻断启动：某个 ${ENV} 暂时不可解析不该让插件起不来；
	// 它会在能解析时被下一次写盘带上）
	p.creds.mu.Lock()
	if err := p.creds.sealAllLocked(); err == nil {
		_ = p.creds.saveLocked()
	}
	p.creds.mu.Unlock()

	p.registerConfig()
	p.registerTools()

	log.Printf("[commondev] 就绪：台账=%s，工程=%d", p.path, len(p.ledger.Projects))
	return nil
}

func (p *Plugin) Stop() error {
	// ★ 孤儿清理是插件的责任（内核对自己的驻留子有硬约束，我们对进程同样硬）。
	// 幂等全清：sidecar / pi 进程一个不留。
	p.mu.Lock()
	sc := p.sidecars
	p.sidecars = map[string]*sidecarProcess{}
	p.mu.Unlock()
	for id, s := range sc {
		s.Close()
		log.Printf("[commondev] 已清理 sidecar: %s", id)
	}
	return nil
}

func (p *Plugin) registerConfig() {
	p.sdk.Settings().RegisterDef(sdk.ConfigDef{
		Key: "pi_bin", Type: "string", DisplayName: "pi 可执行文件",
		Description: "内置 pi 的路径。留空则用随包分发的 runtime/pi，其次 PATH。",
		Category:    p.name,
	})
	// ★ 原先的 intercept_enabled 开关已移除：拦截现由**内核**统一负责，
	// 全局开关是 core.security.enabled（在 WebUI 的设置页里，不在插件里）。
	// 插件保留一个同名开关会让人以为它还能控制内核行为 —— 那是误导。
	p.sdk.Settings().RegisterDef(sdk.ConfigDef{
		Key: "sidecar_timeout_sec", Default: 60, Type: "int", DisplayName: "sidecar 超时（秒）",
		Description: "单次 sidecar 调用的超时上限",
		Category:    p.name,
	})

	// —— 远端（runtime=ssh）配置 ——
	//
	// ★ 这三个原先**只被 Get 读、从未注册**，于是设置页里看不到它们，
	// 用户只能去改 config.yaml 猜键名。缺 ssh_workdir_root 时
	// 远端线的目录准备会直接失败（那是唯一能让它跑起来的入口）。
	// ★ 多台远端机器请用 commondev_endpoint_set/list/remove（按 host 存一套、
	// 按 (host,cli) 再覆盖一套）。下面这三个是**全局兜底**，只在端点表
	// 没有对应值时生效 —— 单台机器时最省事，多台时会被端点表压过。
	p.sdk.Settings().RegisterDef(sdk.ConfigDef{
		Key: "ssh_workdir_root", Type: "string",
		DisplayName: "远端工程目录根",
		Description: "远端放工程工作目录的根，例如 /home/user/.commondev/program。" +
			"runtime=ssh 时**必填**（缺了无法在远端准备目录）。同一远端上的多条分支线在此共享 .mirrors。",
		Category: p.name,
	})
	p.sdk.Settings().RegisterDef(sdk.ConfigDef{
		Key: "ssh_runtime_root", Type: "string",
		DisplayName: "远端 sidecar 目录",
		Description: "远端放 sidecar 脚本的目录（每次拉起会 scp 推过去）。runtime=ssh 时**必填**。",
		Category:    p.name,
	})
	p.sdk.Settings().RegisterDef(sdk.ConfigDef{
		Key: "ssh_node_path", Default: "node", Type: "string",
		DisplayName: "远端 node 路径",
		Description: "远端 node 的路径或命令名（默认走 PATH）。远端 node 不在 PATH 时填绝对路径。" +
			"★ 探测与拉起都用这个值 —— 配了它就该生效。",
		Category: p.name,
	})
}

// ---------------- 台账 ----------------

func (p *Plugin) loadLedger() error {
	p.ledger = &Ledger{
		Projects: map[string]*Project{},
		Branches: map[string]*Branch{},
		Jobs:     map[string]*Job{},
	}
	b, err := os.ReadFile(p.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 首次启动
		}
		return err
	}
	if err := json.Unmarshal(b, p.ledger); err != nil {
		return fmt.Errorf("台账损坏（拒绝静默重建）: %w", err)
	}
	if p.ledger.Projects == nil {
		p.ledger.Projects = map[string]*Project{}
	}
	if p.ledger.Branches == nil {
		p.ledger.Branches = map[string]*Branch{}
	}
	if p.ledger.Jobs == nil {
		p.ledger.Jobs = map[string]*Job{}
	}
	return nil
}

// saveLocked 调用方必须已持 p.mu。
func (p *Plugin) saveLocked() error {
	b, err := json.MarshalIndent(p.ledger, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, p.path)
}

// ---------------- 通道名派生（★ 纪律 2） ----------------

// inputChannelName：inputch 可含斜杠（内核自己就用 child/<id>）。
func inputChannelName(id string) string { return "commondev/" + id }

// outputChannelName：★ 必须只有 [A-Za-z0-9_-]。
//
// 它会被拼成 output_send__<name>，而上游要求函数名满足 ^[a-zA-Z0-9_-]{1,64}$。
// 违反的后果不是「这个工具不可用」，而是**整条请求被 400、agent 整体失语**。
func outputChannelName(id string) string {
	return "commondev_" + safeNamePattern.ReplaceAllString(id, "_")
}

// ---------------- 工具面（刀 1） ----------------

func (p *Plugin) registerTools() {
	p.sdk.RegisterTool("commondev_project_add", sdk.ToolDef{
		Name:        "commondev_project_add",
		Description: "登记一个仓库（项目）。之后用 commondev_branch_open 开工程对话。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":           map[string]interface{}{"type": "string", "description": "项目名（英文，用作 id）"},
				"remote_url":     map[string]interface{}{"type": "string", "description": "git 远程地址"},
				"platform":       map[string]interface{}{"type": "string", "description": "github|gitea|gitcode"},
				"default_branch": map[string]interface{}{"type": "string", "description": "默认分支（默认 main）"},
			},
			"required": []string{"name", "remote_url"},
		},
		NoMemory: true,
		Serial:   true, // 写台账，顺序敏感
	}, p.handleProjectAdd)

	p.sdk.RegisterTool("commondev_project_list", sdk.ToolDef{
		Name:         "commondev_project_list",
		Description:  "列出已登记的仓库与各自的工程对话。",
		Parameters:   map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		NoMemory:     true,
		ParallelSafe: true,
	}, p.handleProjectList)

	p.sdk.RegisterTool("commondev_branch_open", sdk.ToolDef{
		Name: "commondev_branch_open",
		Description: "开一条工程对话（分支线）：建独立工作目录（由 mirror 本地 clone 派生）。" +
			"★ 之后必须调 commondev_agent_up 拿到 session —— 通道依赖 session 才注册。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"project":   map[string]interface{}{"type": "string", "description": "项目名"},
				"branch":    map[string]interface{}{"type": "string", "description": "分支名（会做合法性派生）"},
				"work_desc": map[string]interface{}{"type": "string", "description": "这条线要做什么"},
				"agent_cli": map[string]interface{}{"type": "string",
					"description": "执行后端（默认 pi）。可选 zcode 等 —— 由 " +
						"runtime/<cli>/driver.json 提供，插件核心不感知具体 CLI。"},
				"runtime": map[string]interface{}{"type": "string",
					"description": "在哪跑：local（默认）| ssh。两个轴正交，可与 agent_cli 任意组合。"},
				"ssh_host": map[string]interface{}{"type": "string",
					"description": "runtime=ssh 时的目标（user@host 或 ssh 别名）；需先配好免密。"},
			},
			"required": []string{"project", "branch"},
		},
		NoMemory: true,
		Serial:   true, // 写台账 + 建目录
	}, p.handleBranchOpen)

	p.sdk.RegisterTool("commondev_job_status", sdk.ToolDef{
		Name: "commondev_job_status",
		Description: "查后台作业进度（如 branch_open 的目录准备）：pending|running|done|failed。" +
			"★ 因为内核把工具调用限制在 60 秒，clone 等慢操作都转后台，用本工具追进度。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"job_id":    map[string]interface{}{"type": "string", "description": "作业 id（branch_open 返回）"},
				"branch_id": map[string]interface{}{"type": "string", "description": "或按工程对话查其全部作业"},
			},
		},
		NoMemory:     true,
		ParallelSafe: true,
	}, p.handleJobStatus)

	// —— 平台凭据（用户要求：主流系统的 ackey 保存与变量填充引用）——
	// ★ 工具参数里只出现 account **名**，绝不出现 secret；值支持 ${ENV} 引用。

	// —— 刀 4：判据与复核（正向 + 变异）——
	// ★ 设计 §4.5：插件 GREEN ≠ 改对了。没有反向阶段，就是在给
	//   「测试通过」这个信号不断充值信用，直到它骗一次大的。
	p.sdk.RegisterTool("commondev_test_define", sdk.ToolDef{
		Name: "commondev_test_define",
		Description: "**主管**定义一条判据（可迭代幂等：同定义重复登记不改动）。" +
			"★ 必须同时给 mutations（变异规则）—— 没有它，复核时无法判断判据是否太松。" +
			"script 退出码 0 = 通过。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"project": map[string]interface{}{"type": "string", "description": "项目名"},
				"name":    map[string]interface{}{"type": "string", "description": "判据名（人读）"},
				"kind": map[string]interface{}{"type": "string",
					"description": "unit | interface | stress | build"},
				"script":      map[string]interface{}{"type": "string", "description": "命令（退出码 0 = 通过）"},
				"cwd":         map[string]interface{}{"type": "string", "description": "可选：执行目录（相对工作目录）"},
				"expect":      map[string]interface{}{"type": "string", "description": "可选：说明『通过』的含义（给人看）"},
				"timeout_sec": map[string]interface{}{"type": "number", "description": "可选：单次执行上限（默认 300）"},
				"branch_id":   map[string]interface{}{"type": "string", "description": "可选：限定某条分支线（留空=项目级）"},
				"mutations": map[string]interface{}{
					"type": "array",
					"description": "变异规则：把文件里的 old 换成 new，期望判据**报红**。" +
						"不报红 ⇒ 判据太松。old 必须在文件里**恰好出现一次**。",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"id":   map[string]interface{}{"type": "string", "description": "如 M1"},
							"file": map[string]interface{}{"type": "string", "description": "相对工作目录的路径"},
							"old":  map[string]interface{}{"type": "string", "description": "要替换的字面量（恰好出现一次）"},
							"new":  map[string]interface{}{"type": "string", "description": "替换成什么"},
							"note": map[string]interface{}{"type": "string", "description": "可选：这条变异想验证什么"},
						},
						"required": []string{"file", "old", "new"},
					},
				},
			},
			"required": []string{"project", "name", "script"},
		},
		NoMemory: true,
		Serial:   true,
	}, p.handleTestDefine)

	p.sdk.RegisterTool("commondev_test_list", sdk.ToolDef{
		Name:         "commondev_test_list",
		Description:  "列出已定义的判据（含变异规则条数 —— 为 0 的判据是复核缺口）。",
		Parameters:   map[string]interface{}{"type": "object", "properties": map[string]interface{}{"project": map[string]interface{}{"type": "string", "description": "可选：只看某项目"}}},
		NoMemory:     true,
		ParallelSafe: true,
	}, p.handleTestList)

	p.sdk.RegisterTool("commondev_review_submit", sdk.ToolDef{
		Name: "commondev_review_submit",
		Description: "对一条分支线跑**复核**：正向（判据应通过）+ 反向（变异应被咬住）。" +
			"结论有三档：pass / pass_with_gaps（正向绿但反向有缺口）/ fail。" +
			"★ pass_with_gaps 不是「通过」——它是「判据没覆盖到」。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"branch_id": map[string]interface{}{"type": "string", "description": "工程对话 id（br-...）"},
			},
			"required": []string{"branch_id"},
		},
		NoMemory: true,
		Serial:   true,
	}, p.handleReviewSubmit)

	p.sdk.RegisterTool("commondev_review_result", sdk.ToolDef{
		Name:         "commondev_review_result",
		Description:  "取最近一次复核结论与历史（含每条判据的正向/反向证据）。",
		Parameters:   map[string]interface{}{"type": "object", "properties": map[string]interface{}{"branch_id": map[string]interface{}{"type": "string", "description": "可选：只看某分支"}}},
		NoMemory:     true,
		ParallelSafe: true,
	}, p.handleReviewResult)

	// —— 刀 3：冲突预检与并行盘点 ——
	p.sdk.RegisterTool("commondev_conflict_check", sdk.ToolDef{
		Name: "commondev_conflict_check",
		Description: "在**提 PR 之前**检查本分支能否干净合进目标分支（默认 main）。" +
			"用 git merge-tree 判定，**完全不碰工作区**（不动 HEAD、不留 MERGE_HEAD），" +
			"所以可以随时安全地跑。并行开发时用它**提前**发现冲突 —— " +
			"别等到 PR 开好了、判据绿了，才发现合不进去。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"branch_id": map[string]interface{}{"type": "string", "description": "工程对话 id（br-...）"},
				"target":    map[string]interface{}{"type": "string", "description": "目标分支（默认 main）"},
			},
			"required": []string{"branch_id"},
		},
		NoMemory:     true,
		ParallelSafe: true, // 只读判定，不写任何东西
	}, p.handleConflictCheck)

	p.sdk.RegisterTool("commondev_parallel_status", sdk.ToolDef{
		Name: "commondev_parallel_status",
		Description: "盘点当前**并行推进**的分支线（按项目分组）。" +
			"并行只适合互不相干的模块 —— 派活前先看这个，确认没有两条线在改同一批文件" +
			"（否则冲突会堆到 merge 时才爆发）。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		NoMemory:     true,
		ParallelSafe: true,
	}, p.handleParallelStatus)

	// —— 补全：pi 配置面（设计 §3.6.4 的权威细化）——
	p.sdk.RegisterTool("commondev_pi_config_get", sdk.ToolDef{
		Name: "commondev_pi_config_get",
		Description: "读 pi 的配置（providers / settings / 默认源）。" +
			"★ api_key **绝不返回**（只报 key_set）——与 commondev_account_list 同一条纪律。" +
			"scope: plugin=插件自己的 agentdir（默认）；project=工作目录下的 .pi。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"scope":   map[string]interface{}{"type": "string", "description": "plugin（默认）| project"},
				"workdir": map[string]interface{}{"type": "string", "description": "scope=project 时的工程工作目录"},
			},
		},
		NoMemory:     true,
		ParallelSafe: true,
	}, p.handlePiConfigGet)

	p.sdk.RegisterTool("commondev_pi_source_add", sdk.ToolDef{
		Name: "commondev_pi_source_add",
		Description: "给内置 pi 添加/更新一个 LLM 源（**直接写 pi 的 models.json/auth.json**，" +
			"不造中间层）。★ 改前备份、写后回读校验，失败自动回滚；api_key 不回显。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":           map[string]interface{}{"type": "string", "description": "源名（[A-Za-z0-9_-]）"},
				"base_url":       map[string]interface{}{"type": "string", "description": "API base url"},
				"api_key":        map[string]interface{}{"type": "string", "description": "可选：密钥（写入 auth.json，不回显）"},
				"model":          map[string]interface{}{"type": "string", "description": "模型 id"},
				"adapter":        map[string]interface{}{"type": "string", "description": "openai | anthropic（未知会报错，不猜）"},
				"context_window": map[string]interface{}{"type": "number", "description": "可选：上下文窗口"},
				"max_tokens":     map[string]interface{}{"type": "number", "description": "可选：最大输出"},
				"vision":         map[string]interface{}{"type": "boolean", "description": "可选：是否支持图像输入"},
				"reasoning":      map[string]interface{}{"type": "boolean", "description": "可选：是否推理模型"},
			},
			"required": []string{"name", "base_url", "model", "adapter"},
		},
		NoMemory: true,
		Serial:   true,
	}, p.handlePiSourceAdd)

	p.sdk.RegisterTool("commondev_pi_package_install", sdk.ToolDef{
		Name: "commondev_pi_package_install",
		Description: "为内置 pi 装扩展包（等价 pi install）。" +
			"scope: plugin=装进插件自己的 agentdir（默认）；project=装进工作目录（要 workdir）。" +
			"★ 绝不碰用户全局 ~/.pi/agent；失败时把 pi 的原话带回来。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"source":  map[string]interface{}{"type": "string", "description": "如 npm:@foo/bar、git:github.com/u/r"},
				"scope":   map[string]interface{}{"type": "string", "description": "plugin（默认）| project"},
				"workdir": map[string]interface{}{"type": "string", "description": "scope=project 时的工程工作目录"},
			},
			"required": []string{"source"},
		},
		NoMemory: true,
		Serial:   true,
	}, p.handlePiPackageInstall)

	// —— 补全：仓库搜索（设计 §4.1）——

	// —— 项目打开兼容别名（续篇早期文档使用 project_open）——
	p.sdk.RegisterTool("commondev_project_open", sdk.ToolDef{
		Name: "commondev_project_open", Description: "commondev_project_add 的兼容别名：登记项目。",
		Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{
			"name": map[string]interface{}{"type": "string"}, "remote_url": map[string]interface{}{"type": "string"}, "platform": map[string]interface{}{"type": "string"}, "default_branch": map[string]interface{}{"type": "string"},
		}, "required": []string{"name", "remote_url"}}, NoMemory: true, Serial: true,
	}, p.handleProjectOpen)

	// —— pi 包更新（续篇 §3.5.2）——
	p.sdk.RegisterTool("commondev_pi_plugin_update", sdk.ToolDef{
		Name: "commondev_pi_plugin_update", Description: "更新内置 pi 扩展（可指定 source；不指定则更新全部）。scope=plugin|project。",
		Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"source": map[string]interface{}{"type": "string"}, "scope": map[string]interface{}{"type": "string"}, "workdir": map[string]interface{}{"type": "string"}}}, NoMemory: true, Serial: true,
	}, p.handlePiPluginUpdate)

	// —— pi 配置/源完整入口（设计 §3.6.4）——
	p.sdk.RegisterTool("commondev_pi_config_set", sdk.ToolDef{
		Name: "commondev_pi_config_set", Description: "设置 pi settings.json 的一个键（改前备份、写后校验，失败回滚；scope=plugin|project）。",
		Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{
			"key": map[string]interface{}{"type": "string"}, "value": map[string]interface{}{"type": "string", "description": "JSON 值或普通字符串"}, "scope": map[string]interface{}{"type": "string"}, "workdir": map[string]interface{}{"type": "string"},
		}, "required": []string{"key", "value"}}, NoMemory: true, Serial: true,
	}, p.handlePiConfigSet)
	p.sdk.RegisterTool("commondev_pi_source_remove", sdk.ToolDef{
		Name: "commondev_pi_source_remove", Description: "删除 pi 的一个 LLM 源（models.json + auth.json），改前备份、失败回滚。",
		Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}}, "required": []string{"name"}}, NoMemory: true, Serial: true,
	}, p.handlePiSourceRemove)
	p.sdk.RegisterTool("commondev_pi_mcp_list", sdk.ToolDef{
		Name: "commondev_pi_mcp_list", Description: "列出 pi MCP 配置（scope=plugin|project）。",
		Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"scope": map[string]interface{}{"type": "string"}, "workdir": map[string]interface{}{"type": "string"}}}, NoMemory: true, ParallelSafe: true,
	}, p.handlePiMCPList)
	p.sdk.RegisterTool("commondev_pi_mcp_add", sdk.ToolDef{
		Name: "commondev_pi_mcp_add", Description: "添加 pi MCP；stdio 的 source 必须是 JSON 字符串数组，避免 shell 注入；也可给 url。",
		Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}, "source": map[string]interface{}{"type": "string"}, "url": map[string]interface{}{"type": "string"}, "scope": map[string]interface{}{"type": "string"}, "workdir": map[string]interface{}{"type": "string"}}, "required": []string{"name"}}, NoMemory: true, Serial: true,
	}, p.handlePiMCPAdd)
	p.sdk.RegisterTool("commondev_pi_mcp_remove", sdk.ToolDef{
		Name: "commondev_pi_mcp_remove", Description: "删除 pi MCP（scope=plugin|project）。",
		Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}, "scope": map[string]interface{}{"type": "string"}, "workdir": map[string]interface{}{"type": "string"}}, "required": []string{"name"}}, NoMemory: true, Serial: true,
	}, p.handlePiMCPRemove)

	// —— pi 插件命名兼容别名（早期设计用 plugin，细化稿用 package）——
	p.sdk.RegisterTool("commondev_pi_plugin_install", sdk.ToolDef{
		Name: "commondev_pi_plugin_install", Description: "commondev_pi_package_install 的兼容别名。",
		Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{
			"source": map[string]interface{}{"type": "string"}, "scope": map[string]interface{}{"type": "string"}, "workdir": map[string]interface{}{"type": "string"},
		}, "required": []string{"source"}}, NoMemory: true, Serial: true,
	}, p.handlePiPackageInstall)
	p.sdk.RegisterTool("commondev_pi_plugin_list", sdk.ToolDef{
		Name: "commondev_pi_plugin_list", Description: "列出内置 pi 已安装扩展（分 scope）。",
		Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"scope": map[string]interface{}{"type": "string"}, "workdir": map[string]interface{}{"type": "string"}}}, NoMemory: true, ParallelSafe: true,
	}, p.handlePiPluginList)
	p.sdk.RegisterTool("commondev_pi_plugin_remove", sdk.ToolDef{
		Name: "commondev_pi_plugin_remove", Description: "卸载内置 pi 扩展（分 scope）。",
		Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"source": map[string]interface{}{"type": "string"}, "scope": map[string]interface{}{"type": "string"}, "workdir": map[string]interface{}{"type": "string"}}, "required": []string{"source"}}, NoMemory: true, Serial: true,
	}, p.handlePiPluginRemove)

	// —— 判据单阶段工具（review_submit 仍是推荐的一键正反复核）——
	p.sdk.RegisterTool("commondev_test_run", sdk.ToolDef{
		Name: "commondev_test_run", Description: "只跑判据正向阶段；正向通过不代表改对了，还要跑 test_mutate。",
		Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"branch_id": map[string]interface{}{"type": "string"}}, "required": []string{"branch_id"}}, NoMemory: true, Serial: true,
	}, p.handleTestRun)
	p.sdk.RegisterTool("commondev_test_mutate", sdk.ToolDef{
		Name: "commondev_test_mutate", Description: "只跑判据反向变异阶段；变异未被咬住 ⇒ 判据太松。",
		Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"branch_id": map[string]interface{}{"type": "string"}}, "required": []string{"branch_id"}}, NoMemory: true, Serial: true,
	}, p.handleTestMutate)

	// —— 工作目录重新准备（已有 branch 的幂等修复入口）——
	p.sdk.RegisterTool("commondev_workdir_prepare", sdk.ToolDef{
		Name: "commondev_workdir_prepare", Description: "为已有工程对话重新准备/复用工作目录（后台 clone，幂等）。",
		Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"branch_id": map[string]interface{}{"type": "string"}}, "required": []string{"branch_id"}}, NoMemory: true, Serial: true,
	}, p.handleWorkdirPrepare)

	// —— 补全：PR 工作流（审计发现：设计 §4.1 有 pr_open/pr_status，实现里缺）——

	// —— 补全：agent 生命周期（agent_state / agent_down）——
	p.sdk.RegisterTool("commondev_agent_state", sdk.ToolDef{
		Name:         "commondev_agent_state",
		Description:  "查某条工程对话的 agent 状态：session / runtime / 通道 / **进程是否还活着**。",
		Parameters:   map[string]interface{}{"type": "object", "properties": map[string]interface{}{"branch_id": map[string]interface{}{"type": "string", "description": "工程对话 id"}}, "required": []string{"branch_id"}},
		NoMemory:     true,
		ParallelSafe: true,
	}, p.handleAgentState)

	p.sdk.RegisterTool("commondev_agent_down", sdk.ToolDef{
		Name: "commondev_agent_down",
		Description: "停掉一条工程对话的 agent 进程（**保留工作目录与分支状态**）。" +
			"与 commondev_branch_close 的区别：这是「暂停」，那是「收尾」（会删目录）。幂等。",
		Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"branch_id": map[string]interface{}{"type": "string", "description": "工程对话 id"}}, "required": []string{"branch_id"}},
		NoMemory:   true,
		Serial:     true,
	}, p.handleAgentDown)

	// —— 补全：孤儿目录清理（审计发现：listOrphanDirs 曾是死代码）——
	p.sdk.RegisterTool("commondev_workdir_clean", sdk.ToolDef{
		Name: "commondev_workdir_clean",
		Description: "清理 program/ 下的**孤儿目录**（台账里没有记录的），并做**凭据残留检查**" +
			"（扫 .git/config 的 remote URL 是否内嵌 token，设计 §7.2）。" +
			"★ dry_run 默认 true —— 删除不可逆，先看清要删什么。只碰本插件生成的目录。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"dry_run": map[string]interface{}{"type": "boolean", "description": "true（默认）=只列出；false=实际删除"},
			},
		},
		NoMemory: true,
		Serial:   true,
	}, p.handleWorkdirClean)

	// —— 刀 2：主管侧的审批与合并（需 supervisor 角色凭据）——

	// —— 远端端点表：**按 host 存一套、按 (host,cli) 覆盖一套** ——
	//
	// 为什么必须是工具而不是只改 config.yaml：多台机器时用户要频繁
	// 增删改这些值，而 config.yaml 改完还要重启（且键名靠猜）。
	p.sdk.RegisterTool("commondev_endpoint_set", sdk.ToolDef{
		Name: "commondev_endpoint_set",
		Description: "配置**某台远端机器**的 CLI 路径 / node / 目录（多台机器各配各的，互不干扰）。" +
			"也可只针对**某个后端**（cli 参数）设置，优先于该机器的默认值。" +
			"host 与 branch_open 的 ssh_host 逐字一致（含 user@host:port 或 ssh alias）。" +
			"若不传 verify，会先 ssh 到该机器确认 cli_path 真实存在 —— 避免配一个不存在的路径。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"host":         map[string]interface{}{"type": "string", "description": "ssh_host（与 branch_open 的 ssh_host 一致）"},
				"cli":          map[string]interface{}{"type": "string", "description": "后端名（如 zcode/pi）。给了就是**该后端专属**设置，优先于机器默认"},
				"cli_path":     map[string]interface{}{"type": "string", "description": "该机器上的 CLI 入口（必须**远端**路径，不能是本机路径或 {rawfile}）"},
				"node_path":    map[string]interface{}{"type": "string", "description": "该机器上的 node（默认 node，走远端 PATH）"},
				"runtime_root": map[string]interface{}{"type": "string", "description": "该机器上放 sidecar 脚本的目录"},
				"workdir_root": map[string]interface{}{"type": "string", "description": "该机器上放工程目录的根（仅 host 级）"},
				"note":         map[string]interface{}{"type": "string", "description": "备注（给人看）"},
				"verify":       map[string]interface{}{"type": "boolean", "description": "true（默认）=先 ssh 确认 cli_path 存在；false=跳过（离线配置）"},
			},
			"required": []string{"host"},
		},
		NoMemory: true,
		Serial:   true,
	}, p.handleEndpointSet)

	p.sdk.RegisterTool("commondev_endpoint_list", sdk.ToolDef{
		Name:        "commondev_endpoint_list",
		Description: "列出所有远端机器的端点配置（按 host，含各自的后端专属覆盖）。host 留空则列出全部。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"host": map[string]interface{}{"type": "string", "description": "只看某台机器（留空 = 全部）"},
			},
		},
		NoMemory:     true,
		ParallelSafe: true,
	}, p.handleEndpointList)

	p.sdk.RegisterTool("commondev_endpoint_remove", sdk.ToolDef{
		Name: "commondev_endpoint_remove",
		Description: "删掉某台机器的端点配置（或只删某个后端的专属覆盖）。" +
			"host 自带配置与后端覆盖**分开删**：只给 host 时，若它有后端覆盖会拒绝并提示 —— " +
			"避免“以为删了机器，实际子项还在，下次又把老路径拿出来用”。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"host":  map[string]interface{}{"type": "string", "description": "机器（与 branch_open 的 ssh_host 一致）"},
				"cli":   map[string]interface{}{"type": "string", "description": "只删该后端的专属覆盖"},
				"force": map[string]interface{}{"type": "boolean", "description": "true = 连同该机器的后端覆盖一起删掉"},
			},
			"required": []string{"host"},
		},
		NoMemory: true,
		Serial:   true,
	}, p.handleEndpointRemove)

	// —— 刀 2：分支保护（让「必须有主管复审」成为机制而非礼貌）——

	p.sdk.RegisterTool("enc_shell", sdk.ToolDef{
		Name: "enc_shell",
		Description: "执行命令，并用 @{别名} 引用自动填充**解密后**的加密信息。" +
			"例：git push https://x-access-token:@{gh-token}@github.com/o/r.git feat/x，" +
			"或 curl -H \"Authorization: token @{gitea-local}\" http://host/api/v1/user。" +
			"★ 明文不进 argv（脚本走 stdin）、输出会脱敏。" +
			"⚠ 它不是沙箱：能防意外回显，不防恶意导出（如 base64）。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string",
					"description": "要执行的命令；用 @{别名} 引用加密信息或平台凭据"},
				"workdir": map[string]interface{}{"type": "string",
					"description": "工作目录（必须在 program/ 工程目录下；留空用当前目录）"},
				"timeout_sec": map[string]interface{}{"type": "number",
					"description": "超时秒数（上限 55，因为内核工具调用硬限 60s）"},
			},
			"required": []string{"command"},
		},
		NoMemory: true, // ★ 命令输出可能含敏感信息，不进记忆
		Serial:   true, // 顺序敏感 + 可能与 git 争抢工作树
	}, p.handleEncryptedShell)

	// ———— 加密信息工具族（独立命名空间 enc_*）————
	// 用户要求：「独立为一个加密 shell 工具族，含存入加密信息（提供别名）、使用加密信息。
	// 注意存入过程也需要注意」
	//
	// ★ 存入过程的泄露面（tool 描述里明写）：工具参数会进会话记录/日志，
	// 所以优先用 ${ENV_NAME} 或 @file:/path；直传明文会在返回值里告警。
	p.sdk.RegisterTool("enc_store", sdk.ToolDef{
		Name: "enc_store",
		Description: "存入一条加密信息并给它一个**别名**（后续用 @{别名} 引用）。" +
			"★ 值的抗泄露优先级：`${ENV_NAME}`（环境变量，明文从不经工具参数）> " +
			"`@file:/path`（从文件读）> 直接明文（**会进本会话记录/日志**，插件会告警）。" +
			"存盘为 AES-256-GCM 密文，返回值只给别名+指纹，绝不回显值。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"alias": map[string]interface{}{"type": "string", "description": "别名（[A-Za-z0-9_-]），后续用 @{别名} 引用"},
				"value": map[string]interface{}{"type": "string",
					"description": "值：${ENV_NAME} | @file:/path | 明文（推荐前两种）"},
				"note": map[string]interface{}{"type": "string", "description": "可选：这是什么（便于日后辨认）"},
			},
			"required": []string{"alias", "value"},
		},
		NoMemory: true, // ★ 参数/结果不进记忆
		Serial:   true,
	}, p.handleEncStore)

	p.sdk.RegisterTool("enc_list", sdk.ToolDef{
		Name:         "enc_list",
		Description:  "列出已存的加密信息（别名/来源/指纹/备注）——**绝不返回值本体**。",
		Parameters:   map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		NoMemory:     true,
		ParallelSafe: true,
	}, p.handleEncList)

	p.sdk.RegisterTool("enc_show", sdk.ToolDef{
		Name:        "enc_show",
		Description: "查看某个别名的元信息（来源/指纹/备注）——**不返回值本体**；要看值就在 enc_shell 里用 @{别名}。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"alias": map[string]interface{}{"type": "string", "description": "别名"},
			},
			"required": []string{"alias"},
		},
		NoMemory:     true,
		ParallelSafe: true,
	}, p.handleEncShow)

	p.sdk.RegisterTool("enc_rm", sdk.ToolDef{
		Name:        "enc_rm",
		Description: "删除一条加密信息。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"alias": map[string]interface{}{"type": "string", "description": "别名"},
			},
			"required": []string{"alias"},
		},
		NoMemory: true,
		Serial:   true,
	}, p.handleEncRm)

	// ———— 内置 pi 的 LLM 源配置（用户要求：插件要提供对应 tool 做对应配置）————
	// 这些工具直接写 pi 自己的配置文件（models.json / auth.json / settings.json），
	// 不另建存储 —— pi 本身就有完整的配置系统。
	p.sdk.RegisterTool("commondev_pi_source_import", sdk.ToolDef{
		Name: "commondev_pi_source_import",
		Description: "把 HomeAgent 内核已配的 LLM 源导入内置 pi（写它的 models.json/auth.json，" +
			"默认源写 settings.json）。★ 不导入的话内置 pi 会报 'No API key found'。",
		Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		NoMemory:   true,
		Serial:     true,
	}, p.handlePiSourceImport)

	p.sdk.RegisterTool("commondev_pi_source_list", sdk.ToolDef{
		Name:         "commondev_pi_source_list",
		Description:  "列出内置 pi 已配的 LLM 源与当前默认源（**不返回 api key**）。",
		Parameters:   map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		NoMemory:     true,
		ParallelSafe: true,
	}, p.handlePiSourceList)

	p.sdk.RegisterTool("commondev_pi_source_default", sdk.ToolDef{
		Name: "commondev_pi_source_default",
		Description: "指定内置 pi 默认用哪个 LLM 源（写 settings.json 的 defaultProvider/defaultModel）。" +
			"源必须已存在于 models.json。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":  map[string]interface{}{"type": "string", "description": "源名（models.json 里的 provider 名）"},
				"model": map[string]interface{}{"type": "string", "description": "可选：该源的默认模型"},
			},
			"required": []string{"name"},
		},
		NoMemory: true,
		Serial:   true,
	}, p.handlePiSourceDefault)

	p.sdk.RegisterTool("commondev_agent_up", sdk.ToolDef{
		Name: "commondev_agent_up",
		Description: "为一条工程对话拉起内置 pi 并拿到 session，随后注册该工程的一对通道。" +
			"★ 顺序：先 up（拿 session）→ 插件才注册通道 → 再建驻留子并划入。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"branch_id":  map[string]interface{}{"type": "string", "description": "工程对话 id"},
				"session_id": map[string]interface{}{"type": "string", "description": "可选：恢复指定会话；留空则新建"},
			},
			"required": []string{"branch_id"},
		},
		NoMemory: true,
		Serial:   true, // 拉起进程 + 注册通道
	}, p.handleAgentUp)

	p.sdk.RegisterTool("commondev_branch_status", sdk.ToolDef{
		Name:        "commondev_branch_status",
		Description: "查一条工程对话的详细状态（四元组 + 投递记录）。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"branch_id": map[string]interface{}{"type": "string", "description": "工程对话 id"},
			},
			"required": []string{"branch_id"},
		},
		NoMemory:     true,
		ParallelSafe: true,
	}, p.handleBranchStatus)

	p.sdk.RegisterTool("commondev_branch_close", sdk.ToolDef{
		Name:        "commondev_branch_close",
		Description: "收尾一条工程对话：杀 sidecar、删工作目录（合入后即删，不留残留）。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"branch_id": map[string]interface{}{"type": "string", "description": "工程对话 id"},
			},
			"required": []string{"branch_id"},
		},
		NoMemory: true,
		Serial:   true,
	}, p.handleBranchClose)
}

// ---------------- 工具实现 ----------------

func (p *Plugin) handleProjectAdd(args map[string]interface{}) (interface{}, error) {
	name := strings.TrimSpace(str(args, "name"))
	url := strings.TrimSpace(str(args, "remote_url"))
	if name == "" || url == "" {
		return map[string]interface{}{"error": "name 与 remote_url 必填"}, nil
	}
	if !validName(name) {
		return map[string]interface{}{"error": "项目名只允许 [A-Za-z0-9_-]（会进目录名与通道名）"}, nil
	}
	def := strings.TrimSpace(str(args, "default_branch"))
	if def == "" {
		def = "main"
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if _, dup := p.ledger.Projects[name]; dup {
		return map[string]interface{}{"error": fmt.Sprintf("项目 %s 已存在", name)}, nil
	}
	p.ledger.Projects[name] = &Project{
		Name: name, RemoteURL: url,
		Platform:      strings.TrimSpace(str(args, "platform")),
		DefaultBranch: def,
		CreatedAt:     time.Now().Format(time.RFC3339),
	}
	if err := p.saveLocked(); err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	return map[string]interface{}{"status": "ok", "project": name}, nil
}

func (p *Plugin) handleProjectOpen(args map[string]interface{}) (interface{}, error) {
	return p.handleProjectAdd(args)
}

func (p *Plugin) handleProjectList(args map[string]interface{}) (interface{}, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]map[string]interface{}, 0, len(p.ledger.Projects))
	for _, pr := range p.ledger.Projects {
		branches := []map[string]interface{}{}
		for _, b := range p.ledger.Branches {
			if b.ProjectID == pr.Name {
				branches = append(branches, map[string]interface{}{
					"id": b.ID, "name": b.Name, "status": b.Status,
					"session_id": b.SessionID,
				})
			}
		}
		out = append(out, map[string]interface{}{
			"name": pr.Name, "remote_url": pr.RemoteURL,
			"platform": pr.Platform, "default_branch": pr.DefaultBranch,
			"branches": branches,
		})
	}
	return map[string]interface{}{"projects": out, "count": len(out)}, nil
}

func (p *Plugin) handleBranchOpen(args map[string]interface{}) (interface{}, error) {
	proj := strings.TrimSpace(str(args, "project"))
	branch := strings.TrimSpace(str(args, "branch"))
	if proj == "" || branch == "" {
		return map[string]interface{}{"error": "project 与 branch 必填"}, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	pr, ok := p.ledger.Projects[proj]
	if !ok {
		return map[string]interface{}{"error": fmt.Sprintf("项目 %s 未登记（先 commondev_project_add）", proj)}, nil
	}

	// ★ 唯一性闸：独立 clone 没有 worktree 的「同分支不可双挂」保护，
	// 必须在这里补（设计 §4.3）。同名且未结的分支 → 响亮拒绝，不静默复用。
	for _, b := range p.ledger.Branches {
		if b.ProjectID == proj && b.Name == branch && (b.Status == "open" || b.Status == "review") {
			return map[string]interface{}{
				"error": fmt.Sprintf("分支 %s 已有未结的工程对话 %s（状态 %s）——拒绝重复开（唯一性闸）",
					branch, b.ID, b.Status),
			}, nil
		}
	}

	// ★ 目录映射走单一事实源（workdir.go）：
	// 同仓库不同分支必然得到不同目录（hash 含分支）。
	workdir := branchDir(p.programRoot(), proj, branch)
	id := fmt.Sprintf("br-%s-%s", pr.Name, safeNamePattern.ReplaceAllString(branch, "_"))

	// ★ 两个正交轴的取值在这里定下来（此前硬编码 "local"，于是
	// runtime=ssh 的参数被静默忽略 —— 用户传了 ssh 却跑在本地，
	// 且没有任何提示。这是典型的"参数被吞"）。
	//
	//   Runtime  = 在哪跑（local|ssh）
	//   AgentCLI = 跑什么（pi|zcode|...，由 runtime/<cli>/driver.json 描述）
	runtimeVal := runtimeOrDefault(str(args, "runtime"))
	sshHost := strings.TrimSpace(str(args, "ssh_host"))
	if runtimeVal == "ssh" && sshHost == "" {
		return map[string]interface{}{
			"error": "runtime=ssh 时必须给 ssh_host（user@host 或 ~/.ssh/config 里的别名）",
		}, nil
	}
	if runtimeVal != "local" && runtimeVal != "ssh" {
		// 不认识的 runtime → 响亮拒绝，不静默降级为 local
		// （静默降级会让"我以为跑在远端"的假设一路错下去）
		return map[string]interface{}{
			"error": fmt.Sprintf("未知 runtime %q（只支持 local|ssh）", runtimeVal),
		}, nil
	}

	// ★ 快操作：只登记台账 + 建目录，**不在这里做慢的 clone**。
	// 内核把工具调用限制在 60s（toolcall.go:62），同步 clone 必超时。
	if err := os.MkdirAll(p.programRoot(), 0o700); err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}

	jobID := fmt.Sprintf("job-%s-%d", id, time.Now().UnixNano()%100000)
	b := &Branch{
		ID: id, ProjectID: proj, Name: branch,
		WorkDesc: strings.TrimSpace(str(args, "work_desc")),
		Status:   "open",
		Workdir:  workdir,
		// 两个正交轴（见上）：Runtime 决定在哪跑，AgentCLI 决定跑什么。
		Runtime:   runtimeVal,
		SSHHost:   sshHost,
		AgentCLI:  strings.TrimSpace(str(args, "agent_cli")),
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	p.ledger.Branches[id] = b
	p.ledger.Jobs[jobID] = &Job{
		ID: jobID, BranchID: id, Kind: "prepare_workdir",
		Status: "pending", StartedAt: time.Now().Format(time.RFC3339),
	}
	if err := p.saveLocked(); err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}

	// 慢操作转后台（clone mirror + 派生工作目录）
	// ★ 远端线的目录准备也在**远端**做（见 workdir_remote.go）：
	// 本地 clone 出来的目录远端看不到，`prepareWorkdir` 那套是纯本地的。
	go p.runPrepareWorkdir(jobID, id, pr, branch, workdir, b.Runtime, b.SSHHost)

	return map[string]interface{}{
		"status":    "ok",
		"branch_id": id,
		"workdir":   workdir,
		"job_id":    jobID,
		"next":      "目录准备在后台进行（clone 可能超过 60s 工具上限）——用 commondev_job_status 查进度；ready 后再调 commondev_agent_up",
	}, nil
}

// runPrepareWorkdir 是 branch_open 的后台部分：建 mirror + 派生独立 clone。
//
// 单独跑在 goroutine 里，不进工具调用路径 —— 因此不受 60s 限制。
func (p *Plugin) runPrepareWorkdir(jobID, branchID string, pr *Project, branch, workdir, runtime, sshHost string) {
	// ★ 两条路径必须在**同一台机器**上准备目录：
	//   local → 本机 prepareWorkdir
	//   ssh   → 远端 prepareRemoteWorkdir
	//
	// 走错的话症状极具迷惑性：本地 clone 成功、job 显示 done，
	// 但远端 pi 的 cwd 指向一个不存在的目录 ⇒ "agent 起来了但一执行就说目录不存在"。
	if runtime == "ssh" {
		b := &Branch{ID: branchID, Name: branch, ProjectID: pr.Name, Runtime: runtime, SSHHost: sshHost}
		t, err := parseSSHTarget(b)
		if err != nil {
			p.setJob(jobID, "failed", "", err.Error())
			return
		}
		root, err := p.remoteWorkdirRoot(t.Host)
		if err != nil {
			p.setJob(jobID, "failed", "", err.Error())
			return
		}
		t.WorkdirRoot = root
		// 远端目录名与本地同构：<root>/<hash>-<name>
		remoteWorkdir := root + "/" + dirName(pr.Name, branch)
		p.setJob(jobID, "running", "在远端 "+t.Host+" 准备目录", "")
		if err := p.prepareRemoteWorkdir(t, pr, branch, remoteWorkdir); err != nil {
			p.setJob(jobID, "failed", "", err.Error())
			return
		}
		// ★ 台账里的 Workdir 必须改成**远端路径** ——
		// 留着本地路径会让 agent_up 把远端 pi 的 cwd 指到本地目录名上。
		p.mu.Lock()
		if bb := p.ledger.Branches[branchID]; bb != nil {
			bb.Workdir = remoteWorkdir
		}
		p.mu.Unlock()
		if err := p.saveLocked(); err != nil {
			p.setJob(jobID, "done", "目录已就绪但台账保存失败: "+err.Error(), "")
			return
		}
		if sha, err := p.remoteCaptureBaseSHA(t, remoteWorkdir); err != nil {
			p.setJob(jobID, "done",
				"远端目录已就绪；⚠ base_sha 未记下: "+err.Error(), "")
			return
		} else {
			p.mu.Lock()
			if bb := p.ledger.Branches[branchID]; bb != nil {
				bb.BaseSHA = sha
			}
			p.mu.Unlock()
			_ = p.saveLocked()
			p.setJob(jobID, "done",
				fmt.Sprintf("远端目录已就绪（%s）；base_sha=%s", t.Host, shortSHA(sha)), "")
			return
		}
	}

	p.setJob(jobID, "running", "开始准备目录", "")
	if err := p.prepareWorkdir(pr, branch, workdir); err != nil {
		p.setJob(jobID, "failed", "", err.Error())
		return
	}
	// ★ 刀 3：目录就绪后**立刻**记 base_sha —— 必须在 agent 开始改之前。
	//
	// 为什么必须在这里：agent 一旦提交，HEAD 就不再是"这条线从哪出发"。
	// 没有 base_sha，两件事做不了：① merge 时判断该不该 rebase；
	// ② 事后重放这次改动（设计 §4.2 明确要求）。
	//
	// 失败不阻断：拿不到 base_sha 只影响后续冲突判定的精度，
	// 而目录本身是好的 —— 但要**如实记进作业详情**，不静默吞掉。
	sha, err := p.captureBaseSHA(branchID, workdir)
	if err != nil {
		p.setJob(jobID, "done",
			"目录已就绪（已切到目标分支）；⚠ base_sha 未记下: "+err.Error(), "")
		return
	}
	p.setJob(jobID, "done",
		fmt.Sprintf("目录已就绪（已切到目标分支）；base_sha=%s", shortSHA(sha)), "")
}

// setJob 更新后台作业状态（并落盘，保证重启后台账仍可查）。
func (p *Plugin) setJob(id, status, detail, errMsg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	j, ok := p.ledger.Jobs[id]
	if !ok {
		return
	}
	j.Status = status
	if detail != "" {
		j.Detail = detail
	}
	if errMsg != "" {
		j.Err = errMsg
	}
	if status == "done" || status == "failed" {
		j.EndedAt = time.Now().Format(time.RFC3339)
	}
	_ = p.saveLocked()
}

func (p *Plugin) handleJobStatus(args map[string]interface{}) (interface{}, error) {
	jobID := strings.TrimSpace(str(args, "job_id"))
	branchID := strings.TrimSpace(str(args, "branch_id"))

	p.mu.Lock()
	defer p.mu.Unlock()

	if jobID != "" {
		j, ok := p.ledger.Jobs[jobID]
		if !ok {
			return map[string]interface{}{"error": fmt.Sprintf("作业 %s 不存在", jobID)}, nil
		}
		return map[string]interface{}{"job": j}, nil
	}
	if branchID != "" {
		jobs := []*Job{}
		for _, j := range p.ledger.Jobs {
			if j.BranchID == branchID {
				jobs = append(jobs, j)
			}
		}
		return map[string]interface{}{"jobs": jobs, "count": len(jobs)}, nil
	}
	// 都不给：列出最近的作业（便于排查）
	jobs := []*Job{}
	for _, j := range p.ledger.Jobs {
		jobs = append(jobs, j)
	}
	return map[string]interface{}{"jobs": jobs, "count": len(jobs)}, nil
}

func (p *Plugin) handleAgentUp(args map[string]interface{}) (interface{}, error) {
	id := strings.TrimSpace(str(args, "branch_id"))
	if id == "" {
		return map[string]interface{}{"error": "branch_id 必填"}, nil
	}

	p.mu.Lock()
	b, ok := p.ledger.Branches[id]
	p.mu.Unlock()
	if !ok {
		return map[string]interface{}{"error": fmt.Sprintf("工程对话 %s 不存在", id)}, nil
	}

	// ① 拉起 sidecar（内含 pi 会话），拿到 session_id
	sessionID := strings.TrimSpace(str(args, "session_id"))
	if sessionID == "" {
		sessionID = id // 用工程 id 作会话 id：可复算、可恢复
	}
	sc, err := p.startSidecar(b, sessionID)
	if err != nil {
		// ★ 响亮失败：不静默降级、不冒充成功
		return map[string]interface{}{"error": fmt.Sprintf("拉起 pi 失败: %v", err)}, nil
	}

	// ② ★ 此刻才注册通道（先有 session，才有通道）
	inCh := inputChannelName(id)
	outCh := outputChannelName(id)

	// inputch：pi → agent 的回投入口（控制流不进记忆）
	if err := p.sdk.RegisterInputChannel(inCh, sdk.ChannelDef{NoMemory: true}); err != nil {
		sc.Close()
		return map[string]interface{}{"error": fmt.Sprintf("注册 inputch 失败: %v", err)}, nil
	}

	// outputch：agent → pi 的入口。★ 名已派生为合法字符集。
	if err := p.sdk.RegisterOutputChannel(outCh, sdk.CapText,
		fmt.Sprintf("工程对话 %s：给内置 pi 投递任务", id),
		sdk.ChannelDef{NoMemory: true},
		func(cargs map[string]interface{}) (interface{}, error) {
			return p.deliverToPi(id, cargs)
		}); err != nil {
		sc.Close()
		return map[string]interface{}{"error": fmt.Sprintf("注册 outputch 失败: %v", err)}, nil
	}

	p.mu.Lock()
	b.SessionID = sessionID
	b.SessionName = "br-" + id // U6：可读名，便于在 pi 会话列表里认出
	b.InputCh = inCh
	b.OutputCh = outCh
	p.sidecars[id] = sc
	err = p.saveLocked()
	p.mu.Unlock()
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}

	res := map[string]interface{}{
		"status":       "ok",
		"branch_id":    id,
		"session_id":   sessionID,
		"session_name": b.SessionName,
		"agent_cli":    cliOrPi(b.AgentCLI),
		"input_ch":     inCh,
		"output_ch":    outCh,
		"next": fmt.Sprintf("建驻留子并划入：input_chs=%s allowed_outputs=%s；"+
			"之后给它下任务就走 output_send__%s", inCh, outCh, outCh),
	}
	if b.PiVersion != "" {
		res["pi_version"] = b.PiVersion
	} else {
		res["pi_version_warning"] = "sidecar 未能回显内层 pi 版本（U8 要求可锁定）"
	}
	return res, nil
}

// deliverToPi 是 outputch 的 handler —— 即「把给 pi 发消息包装一下」。
//
// 内核的输出门 output_send__<outCh> 打到这里，我们查台账拿 session_id 后投给 pi。
func (p *Plugin) deliverToPi(branchID string, cargs map[string]interface{}) (interface{}, error) {
	payload, _ := cargs["payload"].(string)
	if strings.TrimSpace(payload) == "" {
		return map[string]interface{}{"status": "unconfirmed", "note": "payload 为空"}, nil
	}

	p.mu.Lock()
	b := p.ledger.Branches[branchID]
	sc := p.sidecars[branchID]
	if b == nil {
		p.mu.Unlock()
		return nil, fmt.Errorf("工程对话 %s 不存在", branchID)
	}
	wantSession := b.SessionID
	p.mu.Unlock()

	if sc == nil {
		return nil, fmt.Errorf("工程对话 %s 的 pi 未拉起（先 commondev_agent_up）", branchID)
	}

	res, err := sc.call("prompt", map[string]interface{}{
		"session_id": wantSession,
		"text":       payload,
	})
	if err != nil {
		p.recordDelivery(branchID, wantSession, payload, "failed", err.Error())
		return nil, fmt.Errorf("投递给 pi 失败: %w", err)
	}

	// ★ 现在是**异步投递**（2026-10-17 修）：sidecar 立即回 "accepted"，
	// 真正跑完时用主动通知（id=0）回投，见 sidecar.go 的 onNotify。
	//
	// 原实现同步等 pi 跑完 ⇒ 内核 60s 工具超时先到，报「执行超时（60秒）」，
	// 而任务其实还在正常跑（生产实测两次）。
	// 通道的语义是"投递任务"，不是"等任务完成"。

	// ★ 纪律 4：断言 session 未变 —— 变了即响亮失败（不静默改投新会话）
	got := wantSession
	if m, ok := res.(map[string]interface{}); ok {
		if s, _ := m["session_id"].(string); s != "" {
			got = s
		}
	}
	if got != wantSession {
		msg := fmt.Sprintf("会话漂移：期望 %s，实际 %s —— 拒绝继续（不静默改投新会话）", wantSession, got)
		p.recordDelivery(branchID, got, payload, "failed", msg)
		return nil, fmt.Errorf("%s", msg)
	}

	p.recordDelivery(branchID, got, payload, "accepted", "")
	// ★ status 用 "accepted" 而不是 "sent"：
	//   sent 会让人以为"pi 已经干完了"，而实际只是**排队受理**。
	//   内核的 executeOutputSendTool 会把 "queued"/"unconfirmed" 当成
	//   "结果未知"回报给模型（见 output.go），accepted 走正常成功路径
	//   但语义诚实：任务已送达、结果稍后回投。
	return map[string]interface{}{
		"status":  "accepted",
		"note":    "任务已投递给 pi，完成结果稍后经 inputch 回投",
		"session": got,
	}, nil
}

func (p *Plugin) recordDelivery(branchID, sessionID, text, status, note string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ledger.Deliveries = append(p.ledger.Deliveries, Delivery{
		Key:      fmt.Sprintf("%s-%d", branchID, time.Now().UnixNano()),
		BranchID: branchID, Text: truncate(text, 200),
		SessionID: sessionID, Status: status, Result: note,
		At: time.Now().Format(time.RFC3339),
	})
	// 只留最近 200 条，防止台账无限增长
	if n := len(p.ledger.Deliveries); n > 200 {
		p.ledger.Deliveries = p.ledger.Deliveries[n-200:]
	}
	_ = p.saveLocked()
}

func (p *Plugin) handleBranchStatus(args map[string]interface{}) (interface{}, error) {
	id := strings.TrimSpace(str(args, "branch_id"))
	p.mu.Lock()
	defer p.mu.Unlock()
	b, ok := p.ledger.Branches[id]
	if !ok {
		return map[string]interface{}{"error": fmt.Sprintf("工程对话 %s 不存在", id)}, nil
	}
	deliveries := []Delivery{}
	for _, d := range p.ledger.Deliveries {
		if d.BranchID == id {
			deliveries = append(deliveries, d)
		}
	}
	alive := false
	if sc := p.sidecars[id]; sc != nil {
		alive = sc.Alive()
	}
	return map[string]interface{}{
		"branch": b, "sidecar_alive": alive,
		"deliveries": deliveries,
	}, nil
}

func (p *Plugin) handleBranchClose(args map[string]interface{}) (interface{}, error) {
	id := strings.TrimSpace(str(args, "branch_id"))
	if id == "" {
		return map[string]interface{}{"error": "branch_id 必填"}, nil
	}

	p.mu.Lock()
	b, ok := p.ledger.Branches[id]
	if !ok {
		p.mu.Unlock()
		return map[string]interface{}{"error": fmt.Sprintf("工程对话 %s 不存在", id)}, nil
	}
	sc := p.sidecars[id]
	delete(p.sidecars, id)
	workdir := b.Workdir
	b.Status = "merged"
	b.ClosedAt = time.Now().Format(time.RFC3339)
	err := p.saveLocked()
	p.mu.Unlock()

	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	// ① 杀进程（幂等）
	if sc != nil {
		sc.Close()
	}
	// ② ★ 合入后即删目录（用户明确要求：不留残留）
	var rmErr string
	if workdir != "" && workdir != "/" && strings.Contains(workdir, "program") {
		if err := os.RemoveAll(workdir); err != nil {
			rmErr = err.Error()
		}
	}
	res := map[string]interface{}{"status": "ok", "branch_id": id, "removed": workdir}
	if rmErr != "" {
		res["remove_error"] = rmErr
	}
	return res, nil
}

// ---------------- 工作目录准备 ----------------

// prepareWorkdir 由 mirror 本地 clone 派生工作目录。
//
// 设计 §4.3（实测）：必须是**本地 clone（硬链接）**，不能是 worktree
// （rm -rf 后 .git/worktrees/ 残留）也不能是 --reference
// （alternates 外部依赖，mirror 被清即损坏）。
func (p *Plugin) prepareWorkdir(pr *Project, branch, workdir string) error {
	// ★ 与远端同一条理由串行化（mirror 的"检查后 clone"是 TOCTOU）：
	// 后台 job 与用户重试会并发撞同一个 mirror。
	p.prepMu.Lock()
	defer p.prepMu.Unlock()

	if st, err := os.Stat(workdir); err == nil && st.IsDir() {
		return p.ensureOnBranch(branch, workdir) // 复用（分支重开时）
	}
	if err := os.MkdirAll(filepath.Dir(workdir), 0o700); err != nil {
		return fmt.Errorf("建目录失败: %w", err)
	}

	mirror := pr.MirrorPath
	if mirror == "" {
		mirror = filepath.Join(filepath.Dir(p.path), "mirrors", pr.Name+".git")
	}
	if _, err := os.Stat(mirror); err != nil {
		// 还没有 mirror：先建（刀 1 允许失败 —— 无凭据时拉不了私有仓）
		if err := runGit("", "clone", "--mirror", pr.RemoteURL, mirror); err != nil {
			return fmt.Errorf("建 mirror 失败（无凭据或网络不通）: %w", err)
		}
		p.mu.Lock()
		pr.MirrorPath = mirror
		p.mu.Unlock()
	}
	if err := runGit("", "clone", mirror, workdir); err != nil {
		return fmt.Errorf("由 mirror 派生工作目录失败: %w", err)
	}
	// ★ 必须切到目标分支 —— “每个目录下是独立仓库的**独立分支**”（用户原话）。
	// 少了这一步，目录里会是默认分支（main），agent 会在错的分支上改。
	return p.ensureOnBranch(branch, workdir)
}

// ensureOnBranch 把工作目录切到指定分支（已在该分支则不动）。
//
// 分支已存在就 checkout；不存在就基于当前 HEAD 新建（首次开线就是这样）。
func (p *Plugin) ensureOnBranch(branch, workdir string) error {
	if strings.TrimSpace(branch) == "" {
		return nil
	}
	cur, err := runGitOut(workdir, "rev-parse", "--abbrev-ref", "HEAD")
	if err == nil && strings.TrimSpace(cur) == branch {
		return nil // 已在目标分支
	}
	// 分支存在 ⇔ 能 checkout；否则新建
	if err := runGit(workdir, "checkout", branch); err != nil {
		if err2 := runGit(workdir, "checkout", "-b", branch); err2 != nil {
			return fmt.Errorf("切到分支 %s 失败: %v", branch, err2)
		}
	}
	return nil
}

func runGit(dir string, args ...string) error {
	_, err := runGitOut(dir, args...)
	return err
}

// runGitOut 同 runGit，但返回 stdout（需要读结果的场景）。
func runGitOut(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// ★ 凭据不进 remote URL（设计 §7.2）：push 时临时注入，此处只做只读操作。
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, truncate(string(out), 300))
	}
	return string(out), nil
}

// ---------------- 辅助 ----------------

// configBool 读一个布尔配置（缺省用 def）。
func (p *Plugin) configBool(key string, def bool) bool {
	if p.sdk == nil {
		return def
	}
	v, err := p.sdk.Settings().Get(key)
	if err != nil || v == nil {
		return def
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(strings.TrimSpace(t), "true") || t == "1"
	case float64:
		return t != 0
	}
	return def
}

func str(args map[string]interface{}, k string) string {
	s, _ := args[k].(string)
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// shortHash 已移到 workdir.go（目录映射的单一事实源）。

var _ = bufio.NewReader // sidecar.go 使用

// cliOrPi 把空值规范化成默认后端（用于回显）。
func cliOrPi(s string) string {
	if strings.TrimSpace(s) == "" {
		return defaultAgentCLI
	}
	return s
}

// runtimeOrDefault 规范化 runtime（默认 local）。
func runtimeOrDefault(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ssh":
		return "ssh"
	case "", "local":
		return "local"
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}
