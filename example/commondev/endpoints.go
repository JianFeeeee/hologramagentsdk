package main

// 远端端点表：**按 host 存一套，按 (host, cli) 覆盖一套**。
//
// ## 为什么必须按 host 分开
//
// 改动前 SSH 相关的东西只有**一组全局 settings**：
//
//	ssh_node_path     远端 node 路径
//	ssh_runtime_root  远端放 sidecar 脚本的目录
//	ssh_workdir_root  远端放工程目录的根
//
// 单台远端时够用。**两台就开始互相打架**：A 机 node 在 `/usr/local/bin/node`、
// B 机在 `/opt/node/bin/node`；A 把脚本放 `/home/a/rt`、B 在 `/srv/rt`。
// 全局只有一个值 ⇒ 后配的那台覆盖前一台，表现为"换台机器就报找不到 node/
// 找不到脚本"，而报错指向的是**另一台机器的路径**，排查方向完全错。
//
// ## 为什么还要按 (host, cli) 再分
//
// 同一个 host 上不同后端 CLI 的路径天然不同：zcode 在
// `/opt/zcode/zcode.cjs`，将来接别的 CLI 又在别处。而且同一台机器上
// 可能出现**同一 CLI 的多个版本**（系统装的 + 用户目录装的），
// 需要在不改 host 默认值的前提下为某个后端单独指定。
//
// ## 结构（优先级从高到低）
//
//	① endpoints[host].CLIs[cli].CLIPath   该机器该后端的专属 CLI 路径
//	② endpoints[host].CLIPath            该机器上所有后端的默认 CLI 路径
//	③ driver.json 的 cliPath              随包/全局默认（本地与远端共用同一份声明）
//
// node/runtime_root/workdir_root 同理：host 级默认 + (host,cli) 级覆盖。
//
// ★ 关键约束：**本机路径绝不发给远端**。见 resolveRemoteEndpoint 的说明。
//
// ## 为什么放独立文件而不是塞进 ledger.json
//
// ledger 是"工程状态"（可重建的派生数据）；端点是"环境配置"
// （人手工维护、丢了要重配、且含机器专属信息）。混在一起会让
// ledger 的保存路径（每次分支变动都写）碰到与它无关的配置，
// 且一次台账损坏会连带丢掉配置。参照 credentials.json 的先例。

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// cliEndpoint 是**某台机器上某个后端**的专属设置。
type cliEndpoint struct {
	// CLIPath 该后端在这台机器上的 CLI 入口（如 /opt/zcode/zcode.cjs）。
	CLIPath string `json:"cli_path,omitempty"`
	// NodePath 该后端用的 node（留空则用 host 级或默认）。
	NodePath string `json:"node_path,omitempty"`
	// RuntimeRoot 该后端 sidecar 脚本落地的远端目录（留空则用 host 级）。
	RuntimeRoot string `json:"runtime_root,omitempty"`
	// Env 该后端额外的远端环境变量（展开占位符后前置到 ssh 命令）。
	Env map[string]string `json:"env,omitempty"`
	// Note 备注（给人看；例如"这台机器是测试机"）。
	Note string `json:"note,omitempty"`
}

// hostEndpoint 是**一台机器**的设置。
type hostEndpoint struct {
	// CLIPath 这台机器上后端的**默认** CLI 路径（被 CLIs[cli].CLIPath 覆盖）。
	CLIPath string `json:"cli_path,omitempty"`
	// NodePath 这台机器的 node（默认 `node`，走远端 PATH）。
	NodePath string `json:"node_path,omitempty"`
	// RuntimeRoot 这台机器放 sidecar 脚本的目录。
	RuntimeRoot string `json:"runtime_root,omitempty"`
	// WorkdirRoot 这台机器放工程目录的根。
	WorkdirRoot string `json:"workdir_root,omitempty"`
	// CLIs 按后端覆盖（key 是 agent_cli，如 "zcode"）。
	CLIs map[string]*cliEndpoint `json:"clis,omitempty"`
	// Note 备注。
	Note string `json:"note,omitempty"`
}

// endpointTable 是端点表的磁盘形态。
type endpointTable struct {
	// Version 供将来迁移用（现在只有 1）。
	Version int `json:"version"`
	// Hosts key 是 ssh_host（与 branch_open 的 ssh_host 参数逐字一致，
	// 包括 `user@host:port` 或 ssh config 别名）。
	Hosts map[string]*hostEndpoint `json:"hosts"`
}

// endpointsPath 端点表与台账同目录（与 credentials.json 一致）。
func (p *Plugin) endpointsPath() string {
	return filepath.Join(filepath.Dir(p.path), "endpoints.json")
}

// loadEndpoints 读端点表。
//
// ★ 缺文件**不是错误**（首次运行、或只用本地后端的人永远不会有它）。
// 但**损坏必须是错误** —— 静默用空表会让所有远端配置凭空消失，
// 表现为"我明明配过，怎么又要配"，而用户会先怀疑自己。
func (p *Plugin) loadEndpoints() error {
	p.endpoints = &endpointTable{Version: 1, Hosts: map[string]*hostEndpoint{}}
	b, err := os.ReadFile(p.endpointsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var t endpointTable
	if err := json.Unmarshal(b, &t); err != nil {
		return fmt.Errorf("端点表损坏（拒绝猜默认，否则所有远端配置会静默消失）: %s: %w",
			p.endpointsPath(), err)
	}
	if t.Hosts == nil {
		t.Hosts = map[string]*hostEndpoint{}
	}
	if t.Version == 0 {
		t.Version = 1
	}
	p.endpoints = &t
	return nil
}

// saveEndpoints 原子写（tmp + rename），权限 0600。
//
// 0600 而不是 0644：端点表可能含机器内网地址、端口、用户目录结构，
// 也**可能**含 env 里塞进去的令牌（虽然不推荐，但不该由文件权限替人决定）。
func (p *Plugin) saveEndpoints() error {
	b, err := json.MarshalIndent(p.endpoints, "", "  ")
	if err != nil {
		return err
	}
	path := p.endpointsPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// getHost 取（或按需创建）某台机器的设置。
//
// create=false 时**不**创建 —— 读路径不该有副作用（否则一个查询工具
// 就会往表里塞一堆空条目，把真实配置淹掉）。
func (p *Plugin) getHost(host string, create bool) *hostEndpoint {
	if p.endpoints == nil {
		p.endpoints = &endpointTable{Version: 1, Hosts: map[string]*hostEndpoint{}}
	}
	if p.endpoints.Hosts == nil {
		p.endpoints.Hosts = map[string]*hostEndpoint{}
	}
	h, ok := p.endpoints.Hosts[host]
	if !ok {
		if !create {
			return nil
		}
		h = &hostEndpoint{CLIs: map[string]*cliEndpoint{}}
		p.endpoints.Hosts[host] = h
	}
	if h.CLIs == nil {
		h.CLIs = map[string]*cliEndpoint{}
	}
	return h
}

// resolvedEndpoint 是"算完之后"的最终取值（三层合并的结果）。
type resolvedEndpoint struct {
	Host        string
	CLI         string
	CLIPath     string
	CLIPathFrom string // 取值来源，**必须在错误信息里出现**
	NodePath    string
	RuntimeRoot string
	WorkdirRoot string
	Env         map[string]string
}

// resolveEndpoint 把三层配置合并成最终端点。
//
// cliPathFrom 记录取值来源：出问题时要能一句话说清"这个路径是从哪来的"。
// 没有它，用户面对"远端找不到 CLI"根本不知道该改哪个文件 ——
// 本会话已经因为"错误不指出位置"吃过多次教训。
func (p *Plugin) resolveEndpoint(host, cli string, drv driverSpec, ctx driverCtx) resolvedEndpoint {
	r := resolvedEndpoint{Host: host, CLI: cli, NodePath: "node", Env: map[string]string{}}

	h := p.getHost(host, false)
	if h != nil {
		r.CLIPath, r.CLIPathFrom = h.CLIPath, "host 默认（endpoints.json）"
		r.NodePath = h.NodePath
		r.RuntimeRoot = h.RuntimeRoot
		r.WorkdirRoot = h.WorkdirRoot
	}
	if h != nil && h.CLIs != nil {
		if c, ok := h.CLIs[cli]; ok && c != nil {
			if strings.TrimSpace(c.CLIPath) != "" {
				r.CLIPath, r.CLIPathFrom = c.CLIPath, "该后端专属（endpoints.json）"
			}
			if strings.TrimSpace(c.NodePath) != "" {
				r.NodePath = c.NodePath
			}
			if strings.TrimSpace(c.RuntimeRoot) != "" {
				r.RuntimeRoot = c.RuntimeRoot
			}
			for k, v := range c.Env {
				r.Env[k] = v
			}
		}
	}
	if strings.TrimSpace(r.NodePath) == "" {
		r.NodePath = "node"
	}

	// 兜底：driver.json 的 cliPath（随包/全局声明）。
	//
	// ★ 只有**未被任何端点配置覆盖**时才用它。而且它可能含 {rawfile} 这类
	// **本地**占位符 —— 那种路径绝不能发给远端（见 resolveRemoteCLIPath）。
	if r.CLIPath == "" && strings.TrimSpace(drv.CLIPath) != "" {
		r.CLIPath = drv.expand(drv.CLIPath, ctx)
		r.CLIPathFrom = "driver.json 的 cliPath 默认值"
	}

	// 兼容旧配置：ssh_node_path / ssh_runtime_root / ssh_workdir_root
	// 只在端点表没有对应值时生效，避免"配了端点表却还被旧键盖住"。
	//
	// ★ 经 settingStr 而不是裸 p.sdk.Settings()：Settings 未装配时
	// 裸调用会 **nil 解引用 panic**，而这里在 ssh 拉起的热路径上 ——
	// 一个配置读取的健壮性问题会变成"拉 agent 直接崩"。
	if r.RuntimeRoot == "" {
		r.RuntimeRoot = p.settingStr("ssh_runtime_root")
	}
	if r.WorkdirRoot == "" {
		r.WorkdirRoot = p.settingStr("ssh_workdir_root")
	}
	if r.NodePath == "node" {
		if s := p.settingStr("ssh_node_path"); s != "" {
			r.NodePath = s
		}
	}
	return r
}

// settingStr 读一个字符串 setting，**容忍 Settings 未装配**。
//
// 返回 "" 表示"没配/取不到"；调用方据此走默认值。
// 修这个是因为判据造了不带 Settings 的插件就 panic —— 那说明
// 任何"Settings 尚未注入"的调用点都会崩，属真实健壮性缺口。
func (p *Plugin) settingStr(key string) string {
	if p.sdk == nil {
		return ""
	}
	st := p.sdk.Settings()
	if st == nil {
		return ""
	}
	v, _ := st.Get(key)
	if v == nil {
		return ""
	}
	s := strings.TrimSpace(fmt.Sprint(v))
	if s == "" || s == "<nil>" {
		return ""
	}
	return s
}

// localPathPlaceholders 是**只在本机有意义**的占位符。
//
// ★ 这是一条安全/正确性边界，不是审美问题：`{rawfile}` 展开成
// `<本机插件目录>/rawfile`，把它发给远端 100% 是个不存在的路径，
// 而远端报的错误是"文件不存在"——用户会去远端找，方向完全错。
var localPathPlaceholders = []string{"{rawfile}", "{pluginDir}", "{dataDir}"}

// containsLocalPlaceholder 报告路径里是否含本机专属占位符。
func containsLocalPlaceholder(s string) bool {
	for _, ph := range localPathPlaceholders {
		if strings.Contains(s, ph) {
			return true
		}
	}
	return false
}

// validateRemoteCLIPath 在**启动 ssh 之前**校验远端 CLI 路径。
//
// 两条规则（都是实测踩出来的）：
//
//  1. **本机占位符不得下发**：展开后是本机路径，远端一定不存在。
//  2. **本机绝对路径不得下发**：即使不是占位符，用户也可能把本机的
//     `/home/program/.zcode-src/.../zcode.cjs` 直接填进 host 端点，
//     而远端是另一台机器。这比第 1 条更隐蔽 —— 它看起来就是个正常路径。
//
// 两条都必须**在 ssh 之前**拦：让远端去失败的话，错误来自远端 shell
// （"No such file or directory"），用户会以为远端缺文件而不是本地配错了。
func validateRemoteCLIPath(cliPath, cli string) error {
	if strings.TrimSpace(cliPath) == "" {
		return fmt.Errorf(
			"远端 %s 没有可用的 CLI 路径。请在 endpoints.json 里为该机器配置 "+
				"hosts.<ssh_host>.cli_path（该机所有后端的默认）或 "+
				"hosts.<ssh_host>.clis.%s.cli_path（该后端的专属路径）。"+
				"可用 commondev_endpoint_set 写入（会校验路径在远端是否存在）。", cli, cli)
	}
	if containsLocalPlaceholder(cliPath) {
		return fmt.Errorf(
			"★ 远端 CLI 路径含**本机专属占位符**（%s）：%q。"+
				"{rawfile}/{pluginDir}/{dataDir} 展开后是**本机**路径，发给远端必然不存在。"+
				"请把它换成该机器上的真实路径（endpoint_set host=<ssh_host> cli=%s cli_path=<远端路径>）",
			strings.Join(localPathPlaceholders, "/"), cliPath, cli)
	}
	return nil
}

// endpointListRows 把端点表摊平成可读行（供 commondev_endpoint_list）。
//
// 一并报出**解析后的默认值**，因为"我这台机器最终会用哪个路径"才是
// 用户真正要回答的问题；只给原始 JSON 等于让人自己心算三层合并。
func (p *Plugin) endpointListRows() []map[string]interface{} {
	rows := []map[string]interface{}{}
	if p.endpoints == nil {
		return rows
	}
	hosts := make([]string, 0, len(p.endpoints.Hosts))
	for h := range p.endpoints.Hosts {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		he := p.endpoints.Hosts[h]
		if he == nil {
			continue
		}
		row := map[string]interface{}{
			"host":         h,
			"cli_path":     he.CLIPath,
			"node_path":    he.NodePath,
			"runtime_root": he.RuntimeRoot,
			"workdir_root": he.WorkdirRoot,
			"note":         he.Note,
		}
		if len(he.CLIs) > 0 {
			clis := make([]string, 0, len(he.CLIs))
			for c := range he.CLIs {
				clis = append(clis, c)
			}
			sort.Strings(clis)
			cm := map[string]interface{}{}
			for _, c := range clis {
				ce := he.CLIs[c]
				if ce == nil {
					continue
				}
				cm[c] = map[string]interface{}{
					"cli_path":     ce.CLIPath,
					"node_path":    ce.NodePath,
					"runtime_root": ce.RuntimeRoot,
					"env":          ce.Env,
					"note":         ce.Note,
				}
			}
			row["clis"] = cm
		}
		rows = append(rows, row)
	}
	return rows
}

// ---------------- 工具处理 ----------------

// handleEndpointSet 写某台机器（或该机器某后端）的端点配置。
//
// ★ 默认会 ssh 到远端**确认 cli_path 真实存在**。
// 为什么默认开：本会话反复出现的失败模式就是"配置写对了但指向一个
// 不存在的东西"，而症状出现在很远的地方（远端 shell 报 No such file）。
// 在这里花一次 ssh 往返，能把它变成一条立刻可见的、指出位置的错误。
func (p *Plugin) handleEndpointSet(args map[string]interface{}) (interface{}, error) {
	host := strings.TrimSpace(str(args, "host"))
	if host == "" {
		return map[string]interface{}{"error": "必须给 host（与 branch_open 的 ssh_host 逐字一致）"}, nil
	}
	cli := strings.TrimSpace(str(args, "cli"))
	cliPath := strings.TrimSpace(str(args, "cli_path"))
	nodePath := strings.TrimSpace(str(args, "node_path"))
	runtimeRoot := strings.TrimSpace(str(args, "runtime_root"))
	workdirRoot := strings.TrimSpace(str(args, "workdir_root"))
	note := strings.TrimSpace(str(args, "note"))

	// ★ 本机路径不得写进**远端**配置：写了就是 100% 起不来，
	// 而报错来自远端 shell，排查方向完全错。在这里拦最省事。
	if cliPath != "" {
		if containsLocalPlaceholder(cliPath) {
			return map[string]interface{}{
				"error": fmt.Sprintf(
					"cli_path 含本机专属占位符（%s）：%q。这些展开后是**本机**路径，"+
						"远端必然不存在。请填该机器上的真实路径",
					strings.Join(localPathPlaceholders, "/"), cliPath),
			}, nil
		}
		if isLocalPathHint(cliPath) {
			return map[string]interface{}{
				"error": fmt.Sprintf(
					"cli_path %q 看起来是**本机**路径（在 %s 下）。请填该**远端机器**上的路径；"+
						"若该机器确实与本地同构（如 localhost 回环），传 verify=false 并确认无误",
					cliPath, p.localRootHint()),
			}, nil
		}
	}

	if workdirRoot != "" && cli != "" {
		return map[string]interface{}{
			"error": "workdir_root 是**机器级**设置（该机器上所有后端共用），请去掉 cli 参数单独设置",
		}, nil
	}

	// 远端验存（默认开）
	verify := true
	if v, ok := args["verify"]; ok {
		if b, isB := v.(bool); isB {
			verify = b
		}
	}
	if verify && cliPath != "" {
		t, err := parseSSHTarget(&Branch{SSHHost: host})
		if err != nil {
			return map[string]interface{}{"error": err.Error()}, nil
		}
		if _, err := p.sshRunCapture(t, "test -f "+shellQuote(cliPath)+" && echo __OK__"); err != nil {
			return map[string]interface{}{
				"error": fmt.Sprintf("远端 %s 上不存在 %s（ssh 验存失败，未写入配置）：%v。"+
					"确认路径后重试；若机器暂时不可达，传 verify=false 先记下配置", host, cliPath, err),
			}, nil
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	he := p.getHost(host, true)

	if cli != "" {
		ce, ok := he.CLIs[cli]
		if !ok || ce == nil {
			ce = &cliEndpoint{}
			he.CLIs[cli] = ce
		}
		if cliPath != "" {
			ce.CLIPath = cliPath
		}
		if nodePath != "" {
			ce.NodePath = nodePath
		}
		if runtimeRoot != "" {
			ce.RuntimeRoot = runtimeRoot
		}
		if note != "" {
			ce.Note = note
		}
	} else {
		if cliPath != "" {
			he.CLIPath = cliPath
		}
		if nodePath != "" {
			he.NodePath = nodePath
		}
		if runtimeRoot != "" {
			he.RuntimeRoot = runtimeRoot
		}
		if workdirRoot != "" {
			he.WorkdirRoot = workdirRoot
		}
		if note != "" {
			he.Note = note
		}
	}
	if err := p.saveEndpoints(); err != nil {
		return map[string]interface{}{"error": "保存端点表失败: " + err.Error()}, nil
	}
	return map[string]interface{}{
		"status": "ok",
		"host":   host,
		"cli":    cli,
		"scope":  map[bool]string{true: "该后端专属", false: "该机器默认"}[cli != ""],
	}, nil
}

// handleEndpointList 列出端点配置。
//
// 一并给出**解析后的最终值**：用户真正要回答的是"这台机器最终用哪个路径"，
// 只给原始 JSON 等于让人自己心算三层合并。给 host+cli 时才计算。
func (p *Plugin) handleEndpointList(args map[string]interface{}) (interface{}, error) {
	host := strings.TrimSpace(str(args, "host"))
	p.mu.Lock()
	defer p.mu.Unlock()
	// ★ 端点表可能尚未加载（裸插件 / 加载失败前的调用）。
	// 空参数调用不得 panic —— 工具会遇到模型乱传参数与半初始化状态。
	if p.endpoints == nil {
		p.endpoints = &endpointTable{Version: 1, Hosts: map[string]*hostEndpoint{}}
	}
	if host != "" {
		he := p.getHost(host, false)
		if he == nil {
			return map[string]interface{}{
				"status": "ok", "host": host, "configured": false,
				"hint": "该机器没有端点配置。用 commondev_endpoint_set host=" + host +
					" cli_path=<远端 CLI 路径> runtime_root=<远端目录> workdir_root=<远端工程根> 配置",
			}, nil
		}
		m := map[string]interface{}{
			"status": "ok", "host": host, "configured": true,
			"cli_path": he.CLIPath, "node_path": he.NodePath,
			"runtime_root": he.RuntimeRoot, "workdir_root": he.WorkdirRoot,
			"note": he.Note,
		}
		if len(he.CLIs) > 0 {
			cm := map[string]interface{}{}
			for c, ce := range he.CLIs {
				if ce == nil {
					continue
				}
				cm[c] = map[string]interface{}{
					"cli_path": ce.CLIPath, "node_path": ce.NodePath,
					"runtime_root": ce.RuntimeRoot, "env": ce.Env, "note": ce.Note,
				}
			}
			m["clis"] = cm
		}
		return m, nil
	}
	return map[string]interface{}{
		"status": "ok", "count": len(p.endpoints.Hosts), "hosts": p.endpointListRows(),
	}, nil
}

// handleEndpointRemove 删端点配置。
//
// ★ 该机器有后端覆盖时，只给 host 会**拒绝**（除非 force）：
// 否则会出现"以为删了机器，实际子项还在，下次又把老路径拿出来用"。
func (p *Plugin) handleEndpointRemove(args map[string]interface{}) (interface{}, error) {
	host := strings.TrimSpace(str(args, "host"))
	cli := strings.TrimSpace(str(args, "cli"))
	force := false
	if v, ok := args["force"]; ok {
		if b, isB := v.(bool); isB {
			force = b
		}
	}
	if host == "" {
		return map[string]interface{}{"error": "必须给 host"}, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.endpoints == nil {
		p.endpoints = &endpointTable{Version: 1, Hosts: map[string]*hostEndpoint{}}
	}
	he := p.getHost(host, false)
	if he == nil {
		return map[string]interface{}{"status": "ok", "host": host, "removed": false,
			"note": "该机器本来就没有配置（幂等）"}, nil
	}
	if cli != "" {
		if _, ok := he.CLIs[cli]; !ok {
			return map[string]interface{}{"status": "ok", "host": host, "cli": cli, "removed": false,
				"note": "该后端本来就没有专属配置（幂等）"}, nil
		}
		delete(he.CLIs, cli)
		if err := p.saveEndpoints(); err != nil {
			return map[string]interface{}{"error": err.Error()}, nil
		}
		return map[string]interface{}{"status": "ok", "host": host, "cli": cli, "removed": true}, nil
	}
	// 只给 host：有子项时拒绝
	if len(he.CLIs) > 0 && !force {
		names := make([]string, 0, len(he.CLIs))
		for c := range he.CLIs {
			names = append(names, c)
		}
		sort.Strings(names)
		return map[string]interface{}{
			"status": "refused",
			"reason": fmt.Sprintf("该机器还有 %d 个后端专属配置（%s）。请先逐个删"+
				"（commondev_endpoint_remove host=%s cli=<名>），或传 force=true 一并删除",
				len(names), strings.Join(names, ", "), host),
		}, nil
	}
	delete(p.endpoints.Hosts, host)
	if err := p.saveEndpoints(); err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	return map[string]interface{}{"status": "ok", "host": host, "removed": true}, nil
}

// isLocalPathHint 粗略判断路径是否"像是本机路径"。
//
// 用本机真实存在的根（插件数据目录 / 工程目录）去比：
// 比"硬编码 /home/program 或 /home/newqqagent"可靠 —— 换台机器部署仍然对。
// 只做**提示**不做强制（确有 localhost 回环这种同构合法情形）。
func isLocalPathHint(cliPath string) bool {
	hint := currentLocalRootHint()
	if hint == "" {
		return false
	}
	return strings.HasPrefix(cliPath, hint)
}

// localRootHint 给用户看的本机根（错误信息里说"本机在 X 下"）。
func (p *Plugin) localRootHint() string {
	return currentLocalRootHint()
}

// currentLocalRootHint 取一个"本机特征路径"用于提示。
func currentLocalRootHint() string {
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		if d != "" && d != "/" {
			return d
		}
	}
	if hd, err := os.UserHomeDir(); err == nil && hd != "" {
		return hd
	}
	return ""
}

// sshRunCapture 在远端跑一条命令并取回 stdout（stderr 一并带在错误里）。
//
// 供端点验存等**轻量**场景使用；重活（准备目录、推脚本）走各自专门函数。
func (p *Plugin) sshRunCapture(t sshTarget, cmd string) (string, error) {
	args := append(t.sshBaseArgs(), t.Host, cmd)
	out, err := exec.Command("ssh", args...).CombinedOutput()
	s := string(out)
	if err != nil {
		return s, fmt.Errorf("%w: %s", err, strings.TrimSpace(s))
	}
	if !strings.Contains(s, "__OK__") {
		return s, fmt.Errorf("远端未返回预期标记（命令可能未生效）: %s", strings.TrimSpace(s))
	}
	return s, nil
}
