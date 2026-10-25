package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/JianFeeeee/hologramagentsdk/sdk"
)

type Plugin struct {
	name       string
	sdk        *sdk.PluginSDK
	srvMu      sync.Mutex
	server     *http.Server
	serverAddr string

	// 会话表：session_id → 上下文前缀。A2A 无状态协议下由插件侧维护
	// 多轮上下文：同 session 的后续请求会把之前的对话拼进注入文本。
	sessMu   sync.Mutex
	sessions map[string]*a2aSession

	// turnMu 保护「同一 session 的一轮对话」不被并发交错。
	//
	// 为什么必须有（2026-10-25）：handleIncomingA2A 是**并发**的
	// （net/http 每请求一个 goroutine），而 agent 侧只有一个对话队列。
	// 两个**同会话**的请求同时进来时，会各自读到同一份历史、各自注入，
	// 回复按 agent 的完成顺序写回——于是历史里出现
	// 「用户A / 用户B / 助手B / 助手A」这种与真实因果不符的交替。
	// sessMu 保不住这一点：它只保证 map/切片的**数据结构安全**，
	// 不保证「一轮对话」这个逻辑单元的原子性。
	//
	// 为什么是 per-session 而不是全局锁：全局锁会让一个跑几十秒的
	// 重任务把**其他所有会话**堵在门外，而 A2A 的并发本就是跨会话的。
	// 做法：每个 session 一个一次性信号量，用完即拆（无人引用时无内存增长）。
	turnMu  sync.Mutex
	turnSem map[string]chan struct{}
}

// a2aSession 记录一个会话的轮次历史，用于延续上下文。
type a2aSession struct {
	ID       string
	History  []string // 轮次文本 [user1, agent1, user2, agent2, ...]
	LastUsed time.Time
}

// maxSessionTurns 单会话保留的最大轮次对数（防上下文无限膨胀）。
const maxSessionTurns = 10

// sessionGCPeriod 会话过期清理周期；超过 2 小时未用的会话回收。
const sessionGCPeriod = 30 * time.Minute

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s
	p.sessions = make(map[string]*a2aSession)
	// 入站通道：本插件用 p.name 通道注入输入（见 InjectInputSync 调用），
	// 输入侧必须显式登记 —— 否则"把该 inputch 划给驻留子"会报 `inputch 未注册`。
	//
	// ★ 声明 NoMemory + 关召回：A2A 入站是**外部 agent 发来的请求指令**，
	// 不是本机用户说的话。不声明的后果：它会被向量化进图记忆、被当成
	// 用户偏好参与情景识别——一条别人的机器指令就这样被永久存下来。
	_ = s.RegisterInputChannel(p.name, sdk.ChannelDef{
		NoMemory:      true,
		RecallPolicy:  sdk.RecallPolicyNone,
		ContextPolicy: "none",
	})
	tp := p.name + "_"

	// ★ stage：把「本轮是哪个 A2A 会话」绑到**帧**上，而不是只靠全局变量。
	//
	// 参照 qq 插件的 qq_auth 做法：同一轮推理可能由多条注入合并而成
	// （中断抢占、并发到达），而全局变量在内核侧是后写覆盖先写。
	// stage 拿到的若是全局值，回复就可能写错会话。
	s.RegisterStage(sdk.StageOnInput, p.stageBindSession, sdk.StageScopeGlobal)
	s.RegisterStage(sdk.StageAfterOutput, p.stagePushReply, sdk.StageScopeGlobal)

	// 注册自身为输出通道：agent 回复 emit 到本通道时有落点，
	// 且 output_list_channels 可见（agent 能主动向 a2a 会话推送消息）。
	if err := s.RegisterOutputChannel(p.name, 1, "A2A Agent 互联通道（外部 agent 查询的回复由此返回）", sdk.ChannelDef{}, func(args map[string]interface{}) (interface{}, error) {
		payload, _ := args["payload"].(string)
		log.Printf("[%s] channel output: %s", p.name, truncateRunes(payload, 120))
		return map[string]interface{}{"status": "ok"}, nil
	}); err != nil {
		log.Printf("[%s] register output channel: %v", p.name, err)
	}

	// 会话 GC：后台周期回收长期不用的会话
	go p.sessionGCLoop()

	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "listen", Default: "127.0.0.1:12000",
		Type: "string", DisplayName: "监听地址",
		Description: "A2A 服务端监听地址，设为空可禁用 HTTP 服务",
		Category:    p.name,
	})

	// Outbound: query + discover
	s.RegisterTool(tp+"a2a_query", sdk.ToolDef{
		Name: tp + "a2a_query", Description: "向另一个 A2A Agent 发送查询并获取回复",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"agent_url":  map[string]interface{}{"type": "string", "description": "目标 Agent 的 A2A 端点 URL"},
				"query":      map[string]interface{}{"type": "string", "description": "发送给目标 Agent 的文本查询"},
				"session_id": map[string]interface{}{"type": "string", "description": "可选。上次调用返回的 session_id，传入可延续与该 agent 的多轮对话上下文"},
				"timeout":    map[string]interface{}{"type": "integer", "description": "超时时间（秒），默认 60"},
			},
			"required": []string{"agent_url", "query"},
		},
		Cleaner: func(output string) string {
			var r struct{ Content string }
			if json.Unmarshal([]byte(output), &r) == nil && r.Content != "" {
				return r.Content
			}
			return output
		},
	}, p.handleA2AQuery)

	s.RegisterTool(tp+"a2a_discover", sdk.ToolDef{
		Name: tp + "a2a_discover", Description: "获取另一个 A2A Agent 的能力描述（Agent Card）",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"agent_url": map[string]interface{}{"type": "string", "description": "目标 Agent 的 A2A 端点 URL"},
			},
			"required": []string{"agent_url"},
		},
	}, p.handleA2ADiscover)

	// Management tools
	s.RegisterTool(tp+"a2a_configure", sdk.ToolDef{
		Name: tp + "a2a_configure", Description: "修改 A2A 插件配置并自动重启服务。支持动态更改监听地址等参数。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"listen": map[string]interface{}{"type": "string", "description": "监听地址（如 0.0.0.0:12000，设为空字符串禁用 HTTP 服务）"},
			},
		},
	}, p.handleConfigure)

	s.RegisterTool(tp+"a2a_restart", sdk.ToolDef{
		Name: tp + "a2a_restart", Description: "重启 A2A HTTP 服务端。当连接异常或配置变更后需要重新加载时使用。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, p.handleRestart)

	s.RegisterTool(tp+"a2a_status", sdk.ToolDef{
		Name: tp + "a2a_status", Description: "查看 A2A 插件的运行状态，包括监听地址和当前配置。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, p.handleStatus)

	// Inbound HTTP server
	if addr, _ := s.Settings().Get("listen"); addr != nil {
		if addrStr, ok := addr.(string); ok && addrStr != "" {
			if err := p.startServer(addrStr); err != nil {
				log.Printf("[%s] start A2A server: %v", p.name, err)
			}
		}
	}

	log.Printf("[%s] started", p.name)
	return nil
}

func (p *Plugin) Stop() error {
	p.stopServer()
	return nil
}

// sessionGCLoop 周期清理超时会话。
func (p *Plugin) sessionGCLoop() {
	ticker := time.NewTicker(sessionGCPeriod)
	defer ticker.Stop()
	for range ticker.C {
		p.sessMu.Lock()
		expired := make([]string, 0, 8)
		for id, sess := range p.sessions {
			if time.Since(sess.LastUsed) > 2*time.Hour {
				delete(p.sessions, id)
				expired = append(expired, id)
			}
		}
		p.sessMu.Unlock()
		// 会话与其一轮对话信号量同生命周期。只回收**已空闲**的（buffered
		// 通道里没有令牌 = 无人在持锁），否则删掉一个正在用的 sem 会让
		// 下一个请求新建一个 ⇒ 新锁与旧锁并存 ⇒ 互斥消失（见 lockTurn 注释）。
		if len(expired) > 0 {
			p.turnMu.Lock()
			for _, id := range expired {
				if sem := p.turnSem[id]; sem != nil && len(sem) == 0 {
					delete(p.turnSem, id)
				}
			}
			p.turnMu.Unlock()
		}
	}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// sessionMessages 返回指定会话的近 limit 条消息（时间正序），
// 会话不存在返回 nil。消息格式 [{role, text, ts}]。
func (p *Plugin) sessionMessages(sessionID string, limit int) []map[string]interface{} {
	p.sessMu.Lock()
	sess := p.sessions[sessionID]
	var hist []string
	var lastUsed time.Time
	if sess != nil {
		hist = append([]string{}, sess.History...)
		lastUsed = sess.LastUsed
	}
	p.sessMu.Unlock()
	if sess == nil {
		return nil
	}
	_ = lastUsed
	// History 交替 [user, agent, user, agent...]，取末尾 limit 条，保持时间正序
	start := 0
	if len(hist) > limit {
		start = len(hist) - limit
	}
	msgs := make([]map[string]interface{}, 0, len(hist)-start)
	for i := start; i < len(hist); i++ {
		role, text := "user", hist[i]
		if after, ok := strings.CutPrefix(text, "用户: "); ok {
			role, text = "user", after
		} else if after, ok := strings.CutPrefix(text, "助手: "); ok {
			role, text = "agent", after
		}
		msgs = append(msgs, map[string]interface{}{
			"role": role,
			"text": text,
		})
	}
	return msgs
}

func (p *Plugin) stopServer() {
	p.srvMu.Lock()
	defer p.srvMu.Unlock()
	if p.server != nil {
		p.server.Close()
		p.server = nil
		p.serverAddr = ""
	}
}

// ---- Inbound HTTP Server ----

func (p *Plugin) startServer(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/agent-card", p.handleAgentCard)
	mux.HandleFunc("/task", p.handleIncomingTask)
	mux.HandleFunc("/a2a", p.handleIncomingA2A)

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %v", addr, err)
	}

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  60 * time.Second,
		// 无界的 header / body 都是「本地进程就能吃掉插件进程内存」的入口。
		MaxHeaderBytes: 64 << 10,
	}
	addrStr := listener.Addr().String()

	p.srvMu.Lock()
	if p.server != nil {
		p.server.Close()
	}
	p.server = srv
	p.serverAddr = addrStr
	p.srvMu.Unlock()

	go func() {
		log.Printf("[%s] A2A server on %s", p.name, addrStr)
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("[%s] serve: %v", p.name, err)
		}
	}()
	return nil
}

func (p *Plugin) handleAgentCard(w http.ResponseWriter, r *http.Request) {
	card := map[string]interface{}{
		"name":        p.name,
		"description": "HologramAgent A2A Agent - 支持多工具调用与记忆管理",
		"url":         r.Host,
		"version":     "1.0.0",
		"capabilities": []map[string]string{
			{"id": "a2a_query", "name": "查询", "description": "接收并处理文本查询"},
			{"id": "a2a_stream", "name": "流式响应", "description": "支持 SSE 流式回复"},
		},
		"skills": []map[string]string{
			{"id": "chat", "name": "对话", "description": "通用对话与问题回答"},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(card)
}

// maxInboundBody 是入站请求体的字节上限。
//
// 此前是 io.ReadAll(r.Body) 无上限：A2A 端点监听在回环上，但任何能访问
// 该端口的本地进程都能用一个超大的请求体把插件进程的堆吃掉（插件是所有
// 插件共用的子进程池成员，炸掉它就是炸掉一串工具）。1MB 远超正常文本查询，
// 同时把「无界输入」这个类别直接消灭。
const maxInboundBody = 1 << 20

// canonicalMethod 把 A2A 的方法名归一化后再比对。
//
// 为什么要归一化（2026-10-25 实测）：A2A 规范写法是 "message/send"，
// 同一批实现里也广泛流传 "Tasks/Send"（Google 早期示例的写法），而本插件
// 原先只认 "tasks.send" 一种。结果是**真实调用方全部打不进来**，
// 返回一句 "unknown method"——我们的 pi-a2a 扩展发的正是 "Tasks/Send"，
// 也就是说这条互通链路其实一直是断的，只是没人从对端试过。
//
// 归一化到小写并去掉 '/'、'_'、'-'、'.' 与空白，于是 "Tasks/Send"、
// "tasks.send"、"tasks_send" 都落到 "taskssend"，同一个分支处理。
//
// ★ '.' 必须在剔除表里：首版漏了它，于是 "tasks.send"（点）归一化成
// "tasks.send" 仍带点，匹配不上 "taskssend"，反而只有大写斜杠写法能用——
// 实测就是先过了 "Tasks/Send"、再测 "tasks.send" 才暴露出来。
func canonicalMethod(method string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(method)) {
		switch r {
		case '/', '_', '-', '.', ' ', '\t':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (p *Plugin) handleIncomingA2A(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		p.handleAgentCard(w, r)
		return
	}
	if r.Method != http.MethodPost {
		writeRPCError(w, "", -32600, "只支持 POST / GET")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxInboundBody+1))
	if err != nil {
		writeRPCError(w, "", -32700, "读取请求体失败: "+err.Error())
		return
	}
	if len(body) > maxInboundBody {
		writeRPCError(w, "", -32600,
			fmt.Sprintf("请求体超过 %d 字节上限", maxInboundBody))
		return
	}

	var req struct {
		JSONRPC string `json:"jsonrpc"`
		ID      string `json:"id"`
		Method  string `json:"method"`
		Params  struct {
			Query     string `json:"query,omitempty"`
			Text      string `json:"text,omitempty"`
			SessionID string `json:"session_id,omitempty"`
			Limit     int    `json:"limit,omitempty"`
			Message   *struct {
				Role  string `json:"role"`
				Parts []struct {
					Text string `json:"text,omitempty"`
					Type string `json:"type,omitempty"`
				} `json:"parts"`
			} `json:"message,omitempty"`
		} `json:"params,omitempty"`
	}
	// 解析失败必须显式报错：忽略 err 会让一个格式错误的请求静默变成
	// 「空 method」⇒ 走到 default 分支，调用方看到的是 "unknown method"，
	// 完全指不到真正的病因（JSON 写错了）。
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCError(w, req.ID, -32700, "请求体不是合法 JSON: "+err.Error())
		return
	}

	switch canonicalMethod(req.Method) {
	case "taskssend", "messagesend": // tasks.send / Tasks/Send / message/send
		// Extract query text
		queryText := req.Params.Query
		if queryText == "" {
			queryText = req.Params.Text
		}
		if queryText == "" && req.Params.Message != nil {
			for _, part := range req.Params.Message.Parts {
				if part.Text != "" {
					queryText += part.Text + "\n"
				}
			}
		}
		queryText = strings.TrimSpace(queryText)
		if queryText == "" {
			writeRPCError(w, req.ID, -32602,
				"缺少查询文本：请给 params.query、params.text 或 params.message.parts[].text 之一")
			return
		}

		// 会话：调用方可指定 session_id 延续多轮上下文；不指定则新建。
		//
		// ★ 注意这里**不再立刻把新会话写进 map**：注入可能失败或超时，
		// 提前登记会留下一批 History 为空的空壳会话，且 handleIncomingA2A
		// 是并发执行的，两个新请求会同时往同一张表里塞各自的 session。
		requested := strings.TrimSpace(req.Params.SessionID)
		sessionID := requested

		// 一轮对话从读历史到写历史必须是一个原子单元（见 turnSem 注释）。
		// 先按会话取锁（尚未有 session_id 的新请求用空串作键——它们
		// 没有历史可交错，用同一把键只是为了避免再开一条路径）。
		unlock := p.lockTurn(requested)
		defer unlock()

		injectText := queryText
		p.sessMu.Lock()
		if sessionID != "" {
			if sess := p.sessions[sessionID]; sess != nil {
				sess.LastUsed = time.Now()
				if len(sess.History) > 0 {
					injectText = buildContextualInput(sess.History, queryText)
				}
			}
		} else {
			sessionID = fmt.Sprintf("a2a_%d", time.Now().UnixNano())
		}
		p.sessMu.Unlock()

		// 同步注入：阻塞等待 agent 处理完成拿回复（不再抢占打断、
		// 也不再回 202 让请求方永远等不到结果）。HTTP 超时由调用方控制。
		//
		// p.sdk 为 nil 只会发生在未 Start 的实例（单测/早期故障）：
		// 裸调会 nil deref，把「服务没起来」表现成插件进程崩掉。
		reply := ""
		if p.sdk != nil {
			reply = p.sdk.InjectInputSyncOpts(p.name, p.name,
				fmt.Sprintf("[来自A2A Agent的查询 session=%s]\n%s\n[注意] 请直接以文本回复本查询，不要调用 output_send__%s——你的最终文本回复会被系统自动返回给请求方。", sessionID, injectText, p.name),
				sdk.InjectOptions{
					// 外部 agent 的指令：不进图记忆、不据它召回、不参与场面识别。
					NoMemory:     true,
					RecallPolicy: sdk.RecallPolicyNone,
					ScenePolicy:  sdk.ScenePolicyNone,
					// 会话标识随 payload 走（内核原样透传到本轮 stageCtx.Extra）。
					Payload: map[string]interface{}{a2aSessionKey: sessionID},
				})
		}

		// 回复写回会话历史（下一轮作为上下文）。成功后才登记会话：
		// 注入失败时不留下半个会话。
		p.sessMu.Lock()
		sess := p.sessions[sessionID]
		if sess == nil {
			sess = &a2aSession{ID: sessionID}
			p.sessions[sessionID] = sess
		}
		sess.History = append(sess.History, "用户: "+queryText, "助手: "+reply)
		if len(sess.History) > maxSessionTurns*2 {
			sess.History = sess.History[len(sess.History)-maxSessionTurns*2:]
		}
		sess.LastUsed = time.Now()
		p.sessMu.Unlock()

		writeRPCResult(w, req.ID, map[string]interface{}{
			"id":         fmt.Sprintf("task_%d", time.Now().UnixNano()),
			"status":     "completed",
			"session_id": sessionID,
			"message": map[string]interface{}{
				"role":  "agent",
				"parts": []map[string]string{{"type": "text", "text": reply}},
			},
		})

	case "tasksget", "sessionget", "tasksstatus":
		// 按 session_id 返回会话内近 N 条消息（默认 10 条）。
		sessionID := strings.TrimSpace(req.Params.SessionID)
		if sessionID == "" {
			sessionID = strings.TrimSpace(req.Params.Query)
		}
		limit := 10
		if req.Params.Limit > 0 && req.Params.Limit <= 100 {
			limit = req.Params.Limit
		}
		msgs := p.sessionMessages(sessionID, limit)
		if msgs == nil {
			writeRPCResult(w, req.ID, map[string]interface{}{
				"session_id": sessionID,
				"status":     "not_found",
				"messages":   []interface{}{},
			})
			return
		}
		writeRPCResult(w, req.ID, map[string]interface{}{
			"session_id": sessionID,
			"status":     "completed",
			"messages":   msgs,
		})

	case "agentcard":
		p.handleAgentCard(w, r)

	default:
		// 把**收到的**方法名回显出来：只说 "unknown method" 时，
		// 调用方无法判断是自己拼错还是服务端版本旧（本次互通断开正是
		// 因为这个信息缺失而拖了很久才发现）。
		writeRPCError(w, req.ID, -32601,
			fmt.Sprintf("未知方法 %q（支持 tasks.send / message/send / tasks.get）", req.Method))
	}
}

// lockTurn 取同一会话的「一轮对话」互斥，返回解锁函数。
//
// 语义：同 sessionID 的请求串行执行（保证历史读-注入-写的因果顺序），
// 不同 sessionID 完全并行（不用一把全局锁拖死其他会话）。
//
// 实现：每个会话一个容量为 1 的 chan 作信号量。
//
// ★ 为什么解码时**不能**从这里删表：
//
//	A 持锁中；B 到达，拿到同一 sem 并阻塞在发送上；
//	A 释放 → 拆表 → B 拿到锁继续；
//	C 到达，发现表里没有 sem ⇒ 新建一个 ⇒ **B 与 C 同时持锁**。
//
// 拆表这个看似无害的清理动作会直接消掉互斥性。因此 sem 的生命周期
// 与 session 一致，统一由 sessionGCLoop 回收（会话 2 小时未用即删）。
func (p *Plugin) lockTurn(sessionID string) func() {
	p.turnMu.Lock()
	if p.turnSem == nil {
		p.turnSem = make(map[string]chan struct{})
	}
	sem := p.turnSem[sessionID]
	if sem == nil {
		sem = make(chan struct{}, 1)
		p.turnSem[sessionID] = sem
	}
	p.turnMu.Unlock()

	sem <- struct{}{}
	return func() { <-sem }
}

// buildContextualInput 把历史与本次输入拼成注入文本。
//
// 分隔与转义的理由：历史条目里直接含用户原始输入，若不做标记，
// 一个精心构造的查询（例如正文里写 "[本轮输入]"）就能伪造出"本轮"的
// 开端，把自己的后半段伪装成历史、或把真实历史挤到注入文本之外。
// 这不是密码学边界，但把「模型看到的段落归属」变成确定的。
func buildContextualInput(history []string, query string) string {
	var b strings.Builder
	b.WriteString("[对话上下文]\n")
	for _, line := range history {
		b.WriteString(escapeContextLine(line))
		b.WriteString("\n")
	}
	b.WriteString("[本轮输入]\n")
	b.WriteString(query)
	return b.String()
}

// escapeContextLine 让单条历史不可能伪装成结构标记：
// 把行首的 '[' 转成全角 '［'，段落标记因此只可能来自本函数自己写的行。
// escapeContextLine 让单条历史不可能伪装成结构标记。
//
// 首版只处理「行首 '['」，但历史条目实际形状是 "用户: [...]"——
// '[' 在冒号后面，于是逃逸完全没生效（测试当场抓住）。现在转义的是
// **所有** '['：历史只用于让人读，不需要保留方括号原字。
func escapeContextLine(s string) string {
	if !strings.Contains(s, "[") {
		return s
	}
	return strings.ReplaceAll(s, "[", "［")
}

// writeRPCResult / writeRPCError 保证响应形状一致（JSON-RPC 2.0）。
func writeRPCResult(w http.ResponseWriter, id string, result map[string]interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"jsonrpc": "2.0", "id": id, "result": result,
	})
}

func writeRPCError(w http.ResponseWriter, id string, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // JSON-RPC 错误走 200，错误在 body 里
	json.NewEncoder(w).Encode(map[string]interface{}{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]interface{}{"code": code, "message": msg},
	})
}

func (p *Plugin) handleIncomingTask(w http.ResponseWriter, r *http.Request) {
	p.handleIncomingA2A(w, r)
}

// ---- A2A Protocol Types ----

type A2AAgentCard struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	URL          string          `json:"url"`
	Version      string          `json:"version,omitempty"`
	Capabilities []A2ACapability `json:"capabilities,omitempty"`
	Skills       []A2ASkill      `json:"skills,omitempty"`
}

type A2ACapability struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type A2ASkill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema string `json:"input_schema,omitempty"`
}

type A2ARequest struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      string    `json:"id"`
	Method  string    `json:"method"`
	Params  A2AParams `json:"params,omitempty"`
}

type A2AParams struct {
	Query     string      `json:"query,omitempty"`
	SessionID string      `json:"session_id,omitempty"`
	Message   *A2AMessage `json:"message,omitempty"`
	TaskID    string      `json:"id,omitempty"`
}

type A2AResponse struct {
	JSONRPC string     `json:"jsonrpc"`
	ID      string     `json:"id"`
	Result  *A2AResult `json:"result,omitempty"`
	Error   *A2AError  `json:"error,omitempty"`
}

type A2AResult struct {
	TaskID    string        `json:"id,omitempty"`
	Status    string        `json:"status,omitempty"`
	SessionID string        `json:"session_id,omitempty"`
	Message   *A2AMessage   `json:"message,omitempty"`
	AgentCard *A2AAgentCard `json:"agent_card,omitempty"`
}

type A2AMessage struct {
	Role  string    `json:"role"`
	Parts []A2APart `json:"parts"`
}

type A2APart struct {
	Text string `json:"text,omitempty"`
	Data string `json:"data,omitempty"`
	Type string `json:"type,omitempty"`
}

type A2AError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ---- Outbound Handlers ----

func (p *Plugin) handleA2ADiscover(args map[string]interface{}) (interface{}, error) {
	agentURL, _ := args["agent_url"].(string)
	agentURL = strings.TrimRight(agentURL, "/")
	if !strings.HasPrefix(agentURL, "http://") && !strings.HasPrefix(agentURL, "https://") {
		agentURL = "http://" + agentURL
	}

	cardURL := agentURL
	if !strings.HasSuffix(cardURL, "/agent-card") {
		cardURL = agentURL + "/agent-card"
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(cardURL)
	if err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("连接失败: %v", err)}, nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return map[string]interface{}{"error": fmt.Sprintf("状态码 %d", resp.StatusCode), "raw_body": string(body)}, nil
	}

	var card A2AAgentCard
	if err := json.Unmarshal(body, &card); err != nil {
		var fallback map[string]interface{}
		if err2 := json.Unmarshal(body, &fallback); err2 == nil {
			return map[string]interface{}{"agent_info": fallback, "format": "非标准格式"}, nil
		}
		return map[string]interface{}{"error": fmt.Sprintf("解析失败: %v", err), "raw_body": string(body)}, nil
	}

	return map[string]interface{}{
		"name": card.Name, "description": card.Description,
		"version": card.Version, "url": card.URL,
		"capabilities": card.Capabilities, "skills": card.Skills,
	}, nil
}

func (p *Plugin) handleA2AQuery(args map[string]interface{}) (interface{}, error) {
	agentURL, _ := args["agent_url"].(string)
	query, _ := args["query"].(string)
	sessionID, _ := args["session_id"].(string) // 可选：延续对方会话
	timeoutSec := 60
	if v, ok := args["timeout"].(float64); ok && v > 0 {
		timeoutSec = int(v)
	}

	agentURL = strings.TrimRight(agentURL, "/")
	if !strings.HasPrefix(agentURL, "http://") && !strings.HasPrefix(agentURL, "https://") {
		agentURL = "http://" + agentURL
	}

	taskURL := agentURL
	if strings.HasSuffix(agentURL, "/agent-card") {
		taskURL = strings.TrimSuffix(agentURL, "/agent-card")
	}
	taskURL = strings.TrimRight(taskURL, "/") + "/task"

	reqBody := A2ARequest{
		JSONRPC: "2.0",
		ID:      fmt.Sprintf("a2a_%d", time.Now().UnixNano()),
		Method:  "tasks.send",
		Params: A2AParams{
			SessionID: sessionID,
			Message:   &A2AMessage{Role: "user", Parts: []A2APart{{Text: query, Type: "text"}}},
		},
	}

	bodyData, _ := json.Marshal(reqBody)
	client := &http.Client{Timeout: time.Duration(timeoutSec) * time.Second}
	resp, err := client.Post(taskURL, "application/json", bytes.NewReader(bodyData))
	if err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("请求失败(超时%d秒): %v", timeoutSec, err)}, nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return map[string]interface{}{"error": fmt.Sprintf("状态码 %d", resp.StatusCode), "raw_body": string(body)}, nil
	}

	var a2aResp A2AResponse
	if err := json.Unmarshal(body, &a2aResp); err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("解析响应失败: %v", err), "raw_body": string(body)}, nil
	}

	if a2aResp.Error != nil {
		return map[string]interface{}{"error": fmt.Sprintf("Agent错误 [%d]: %s", a2aResp.Error.Code, a2aResp.Error.Message)}, nil
	}
	if a2aResp.Result == nil {
		return map[string]interface{}{"error": "空结果", "raw_body": string(body)}, nil
	}

	var replyText string
	if a2aResp.Result.Message != nil {
		for _, part := range a2aResp.Result.Message.Parts {
			if part.Text != "" {
				replyText += part.Text + "\n"
			}
		}
		replyText = strings.TrimSpace(replyText)
	}

	result := map[string]interface{}{
		"task_id": a2aResp.Result.TaskID, "status": a2aResp.Result.Status,
		"response": replyText,
	}
	if a2aResp.Result.SessionID != "" || sessionID != "" {
		result["session_id"] = a2aResp.Result.SessionID
		if result["session_id"] == "" {
			result["session_id"] = sessionID
		}
		result["note"] = "延续会话：下次调用传此 session_id 可保持上下文"
	}
	return result, nil
}

// ---- Management Handlers ----

func (p *Plugin) handleConfigure(args map[string]interface{}) (interface{}, error) {
	listen, _ := args["listen"].(string)
	listen = strings.TrimSpace(listen)

	if err := p.sdk.Settings().Set("listen", listen); err != nil {
		return fmt.Sprintf("保存配置失败: %v", err), nil
	}

	if listen == "" || listen == "off" || listen == "disabled" {
		p.stopServer()
		return "A2A HTTP 服务已禁用（listen 设为空）", nil
	}

	if err := p.startServer(listen); err != nil {
		return fmt.Sprintf("A2A 配置已保存，但服务启动失败: %v", err), nil
	}
	return fmt.Sprintf("A2A 配置已更新。监听地址: %s (已启动)", listen), nil
}

func (p *Plugin) handleRestart(args map[string]interface{}) (interface{}, error) {
	p.stopServer()

	addr, _ := p.sdk.Settings().Get("listen")
	addrStr, _ := addr.(string)
	if addrStr == "" || addrStr == "off" || addrStr == "disabled" {
		return "A2A 服务未配置监听地址（listen 为空），无法启动", nil
	}

	if err := p.startServer(addrStr); err != nil {
		return fmt.Sprintf("A2A 服务启动失败: %v", err), nil
	}

	p.srvMu.Lock()
	listening := p.serverAddr
	p.srvMu.Unlock()
	return fmt.Sprintf("A2A 服务已重启，监听: %s", listening), nil
}

func (p *Plugin) handleStatus(args map[string]interface{}) (interface{}, error) {
	addr, _ := p.sdk.Settings().Get("listen")
	addrStr, _ := addr.(string)

	p.srvMu.Lock()
	serverRunning := p.server != nil
	listening := p.serverAddr
	p.srvMu.Unlock()
	if !serverRunning {
		listening = "未运行"
	}

	return fmt.Sprintf("配置监听地址: %s\n当前监听: %s\n服务状态: %s",
		addrStr, listening, map[bool]string{true: "运行中", false: "已停止"}[serverRunning]), nil
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}

// ---- stage：把 A2A 会话绑到帧上（照 qq 的 qq_auth 做法）----

// a2aSessionKey 是 Payload / Extra 里的会话键（带插件名前缀，避免与内核同名字段撞车）。
const a2aSessionKey = "a2a_sid"

// stageBindSession 在 on_input 把 Payload 带来的会话钉到本帧 Extra 上。
func (p *Plugin) stageBindSession(ctx *sdk.StageContext) error {
	ctx.RLock()
	sid, _ := ctx.Extra[a2aSessionKey].(string)
	ctx.RUnlock()
	if sid == "" {
		return nil // 非 A2A 发起的输入：不绑定
	}
	ctx.Lock()
	if ctx.Extra == nil {
		ctx.Extra = make(map[string]interface{})
	}
	ctx.Extra[a2aSessionKey] = sid
	ctx.Unlock()
	return nil
}

// stagePushReply 在 after_output 把本轮最终文本补进**本帧**对应的会话。
//
// 为什么需要：同步注入只覆盖「HTTP 请求正在等回复」。若这一轮是 agent
// 自循环 / 工具链续轮产生的，注入早已返回，回复就永远到不了下游。
// 这里把它补进 History，使会话记录与 agent 实际输出一致。
func (p *Plugin) stagePushReply(ctx *sdk.StageContext) error {
	ctx.RLock()
	sid, _ := ctx.Extra[a2aSessionKey].(string)
	final := ctx.FinalText
	ctx.RUnlock()
	if sid == "" || final == "" {
		return nil
	}

	p.sessMu.Lock()
	defer p.sessMu.Unlock()
	sess := p.sessions[sid]
	if sess == nil {
		return nil // 会话已过期/被清理：丢弃而不是创建一个幽灵会话
	}
	// 去重：同步路径已把同一段文本写进历史时不重复追加。
	if n := len(sess.History); n > 0 && sess.History[n-1] == "助手: "+final {
		return nil
	}
	sess.History = append(sess.History, "助手: "+final)
	if len(sess.History) > maxSessionTurns*2 {
		sess.History = sess.History[len(sess.History)-maxSessionTurns*2:]
	}
	sess.LastUsed = time.Now()
	return nil
}
