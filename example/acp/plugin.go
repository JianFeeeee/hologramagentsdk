package main

import (
	"bufio"
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

// acpPlugin 实现 Agent Client Protocol (ACP) 0.0.x 子集：
//   - 服务端：POST /api/session （JSON-RPC：session/new / session/update），
//     请求注入本 Agent，另提供 GET /api/session?id=xxx SSE 事件流。
//   - 客户端：向远程 ACP 服务端发 session/new 并读取 SSE session/reply。
type Plugin struct {
	name     string
	sdk      *sdk.PluginSDK
	srvMu    sync.Mutex
	server   *http.Server
	serverID string

	mu       sync.RWMutex
	sessions map[string]*sessionState

	// turnMu 保护「同一 session 的一轮对话」不被并发交错。
	//
	// 为什么必须有：handleSessionPost 是并发的（net/http 每请求一个 goroutine），
	// 而 agent 侧只有一个对话队列。两个**同会话**请求同时进来时会各自读到
	// 同一份历史、各自注入，回复按完成顺序写回 ⇒ 历史出现
	// 「用户A / 用户B / 助手B / 助手A」这种与真实因果不符的交替。
	// p.mu 保不住这一点：它只保证 map/切片的**数据结构安全**。
	//
	// 用 per-session 信号量而非全局锁：全局锁会让一个跑两分钟的重任务
	// 把其他会话全堵住，而 ACP 的并发本就是跨会话的。
	turnMu  sync.Mutex
	turnSem map[string]chan struct{}
}

type sessionState struct {
	ID       string
	Replying []map[string]interface{}
	History  []string // 轮次历史 [user, agent, user, agent...]，延续上下文用
	LastUsed time.Time
}

// maxSessionTurns 单会话保留的最大轮次对数。
const maxSessionTurns = 10

// maxInboundBody 入站请求体上限（见 handleSessionPost 注释）。
const maxInboundBody = 1 << 20

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s
	p.sessions = make(map[string]*sessionState)
	// 会话 GC：长期不用的会话与其 per-session 信号量一起回收。
	// 没有它，turnSem 会随会话数无限增长。
	go p.sessionGCLoop()
	// 入站通道：本插件用 p.name 通道注入输入（见 InjectInputSync 调用），
	// 输入侧必须显式登记 —— 否则"把该 inputch 划给驻留子"会报 `inputch 未注册`。
	//
	// ★ ChannelDef 里声明 NoMemory + 关召回/场面：ACP 入站是**外部 agent 的
	// 请求指令**，不是用户说的话。不声明的后果：它会进图记忆、被向量化、
	// 参与情景识别，把别人的机器指令当成用户偏好存下来（qq 的通道无此问题，
	// 因为它承载的就是真实对话）。真正的业务内容由 stage 在应答上保留。
	_ = s.RegisterInputChannel(p.name, sdk.ChannelDef{
		NoMemory:      true,
		RecallPolicy:  sdk.RecallPolicyNone,
		ContextPolicy: "none",
	})
	tp := p.name + "_"

	// 注册自身为输出通道：agent 回复 emit 到本通道时有落点。
	// （回复主要走同步注入返回，此通道用于 agent 主动 output_send__acp）
	s.RegisterOutputChannel(p.name, 1, "ACP Agent 互联通道（外部 agent 会话的回复由此返回）", sdk.ChannelDef{}, func(args map[string]interface{}) (interface{}, error) {
		payload, _ := args["payload"].(string)
		log.Printf("[%s] channel output: %s", p.name, truncateStr(payload, 120))
		return map[string]interface{}{"status": "ok"}, nil
	})

	// ★ stage 把「本轮是哪次 ACP 请求」绑到**帧**上（照 qq 的 qq_auth 做法）。
	//
	// 为什么必须绑帧而不是只用全局变量：ACP 与 QQ 面临同一个真实问题——
	// 同一轮推理可能由多条注入合并而成（中断抢占、并发到达），而全局
	// 身份/会话变量在内核 side 是**后写覆盖先写**的。只靠全局变量时，
	// stage 里读到的是“最后一条注入的会话”，不是“本帧对应的会话”，
	// 于是回复可能写错会话。
	//
	// 流程：注入时把 acp_sid 放进 InjectOptions.Payload（内核原样透传到
	// 本轮 stageCtx.Extra）⇒ onInput 把它钉进 Extra[acp_sid] ⇒ afterOutput
	// 用帧上那个值（而不是全局变量）。
	s.RegisterStage(sdk.StageOnInput, p.stageBindSession, sdk.StageScopeGlobal)
	s.RegisterStage(sdk.StageAfterOutput, p.stagePushReply, sdk.StageScopeGlobal)

	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "listen", Default: "127.0.0.1:12001",
		Type: "string", DisplayName: "监听地址",
		Description: "ACP 服务端监听地址，设为空可禁用 HTTP 服务",
		Category:    p.name,
	})

	s.RegisterTool(tp+"acp_query", sdk.ToolDef{
		Name: tp + "acp_query", Description: "向远程 ACP Agent（如 opencode http://127.0.0.1:13000、pi bridge http://127.0.0.1:12011 或回环到自身 12001）发起一个会话请求并等待回复，返回其最终回答文本，兼容 SSE 型与同步 JSON 型 ACP 服务端",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"server_url": map[string]interface{}{"type": "string", "description": "目标 ACP 服务端地址（如 http://127.0.0.1:13000）"},
				"prompt":     map[string]interface{}{"type": "string", "description": "发送给目标 Agent 的任务描述"},
				"session_id": map[string]interface{}{"type": "string", "description": "可选。上次调用返回的 session_id，传入可延续与该 agent 的多轮对话上下文"},
				"timeout":    map[string]interface{}{"type": "integer", "description": "等待回复超时（秒），默认 120"},
			},
			"required": []string{"server_url", "prompt"},
		},
		Cleaner: func(output string) string {
			var r struct {
				Reply string `json:"reply"`
			}
			if json.Unmarshal([]byte(output), &r) == nil && r.Reply != "" {
				return r.Reply
			}
			return output
		},
	}, p.handleAcpQuery)

	s.RegisterTool(tp+"acp_configure", sdk.ToolDef{
		Name: tp + "acp_configure", Description: "修改 ACP 插件的监听配置并生效（重启 HTTP 服务）",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"listen": map[string]interface{}{"type": "string", "description": "监听地址（如 0.0.0.0:12001，设为空禁用）"},
			},
		},
	}, p.handleConfigure)

	s.RegisterTool(tp+"acp_status", sdk.ToolDef{
		Name: tp + "acp_status", Description: "查看 ACP 插件运行状态与当前活跃会话数",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, p.handleStatus)

	addr, _ := s.Settings().Get("listen")
	if addrStr, ok := addr.(string); ok && addrStr != "" {
		if err := p.startServer(addrStr); err != nil {
			log.Printf("[%s] start ACP server: %v", p.name, err)
		}
	}

	log.Printf("[%s] started", p.name)
	return nil
}

func (p *Plugin) Stop() error {
	p.stopServer()
	return nil
}

func (p *Plugin) stopServer() {
	p.srvMu.Lock()
	defer p.srvMu.Unlock()
	if p.server != nil {
		p.server.Close()
		p.server = nil
		p.serverID = ""
	}
}

// ---- Inbound HTTP Server ----

func (p *Plugin) startServer(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", p.handleSession)

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %v", addr, err)
	}

	srv := &http.Server{
		Handler: mux,
		// 无界 header / 无超时都是「本地进程就能占用插件进程资源」的入口。
		// WriteTimeout 特意留给 SSE：该连接长时间挂起是设计（事件流），
		// 所以只在 handler 内部用 15s 心跳保活、不设全局写超时。
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	addrStr := listener.Addr().String()

	p.srvMu.Lock()
	if p.server != nil {
		p.server.Close()
	}
	p.server = srv
	p.serverID = addrStr
	p.srvMu.Unlock()

	go func() {
		log.Printf("[%s] ACP server on %s", p.name, addrStr)
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("[%s] serve: %v", p.name, err)
		}
	}()
	return nil
}

func (p *Plugin) handleSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "POST":
		p.handleSessionPost(w, r)
	case "GET":
		p.handleSessionSSE(w, r)
	default:
		http.Error(w, "", http.StatusMethodNotAllowed)
	}
}

// handleSessionPost 处理 JSON-RPC：session/new 与 session/update
func (p *Plugin) handleSessionPost(w http.ResponseWriter, r *http.Request) {
	// ★ 体积上限：无界的 io.ReadAll 是「任何本地进程一个请求就能把插件进程
	// 的内存吃掉」的入口。ACP 端点在回环上，但插件是所有插件共享的子进程
	// 池成员，炸掉它就炸掉一串工具。多读 1 字节用于判断是否超限。
	body, err := io.ReadAll(io.LimitReader(r.Body, maxInboundBody+1))
	if err != nil {
		writeRPCErr(w, nil, -32700, "读取请求体失败: "+err.Error())
		return
	}
	if len(body) > maxInboundBody {
		writeRPCErr(w, nil, -32600,
			fmt.Sprintf("请求体超过 %d 字节上限", maxInboundBody))
		return
	}

	var req struct {
		JSONRPC string      `json:"jsonrpc"`
		ID      interface{} `json:"id"`
		Method  string      `json:"method"`
		Params  struct {
			Request *struct {
				Text string `json:"text"`
			} `json:"request,omitempty"`
			SessionID string `json:"session_id,omitempty"`
			Limit     int    `json:"limit,omitempty"`
			Final     bool   `json:"final,omitempty"`
		} `json:"params,omitempty"`
	}
	// ★ 错误必须用 JSON-RPC 错误帧回，而不是 http.Error 的纯文本：
	// 调用方解析 JSON 失败时只能看到 “invalid character 'i'”，根本不知道
	// 是自己协议写错了还是服务端版本不对。并把 req.ID 带上，避免
	// “请求能发出、无法对应哪一条”的悬挂。
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCErr(w, req.ID, -32700, "请求体不是合法 JSON: "+err.Error())
		return
	}

	switch canonicalACP(req.Method) {
	case "sessionnew":
		text := ""
		if req.Params.Request != nil {
			text = strings.TrimSpace(req.Params.Request.Text)
		}
		if text == "" {
			writeRPCErr(w, req.ID, -32602, "缺少 request.text")
			return
		}

		// 会话：调用方可指定 session_id 延续多轮；不指定则新建。
		// ★ 先取会话级「一轮对话」互斥，再读历史 -> 注入 -> 写历史：
		// 中间整个区间必须是一个原子单元，否则同会话并发会把因果顺序弄乱
		// （详见 Plugin.turnSem 注释）。
		requested := strings.TrimSpace(req.Params.SessionID)
		sid := requested
		unlock := p.lockTurn(sid)
		defer unlock()

		p.mu.Lock()
		if sid == "" {
			sid = fmt.Sprintf("session_%d", time.Now().UnixNano())
		}
		st := p.sessions[sid]
		if st == nil {
			// 延迟登记：注入失败/超时不留下 History 为空的空壳会话。
			st = &sessionState{ID: sid}
		}
		var history []string
		if len(st.History) > 0 {
			history = append([]string{}, st.History...)
		}
		p.mu.Unlock()

		// 延续上下文（与 A2A 同一套分隔/转义约定）
		injectText := buildACPContextualInput(history, text)

		// 同步注入等待回复：不抢占打断，完整闭环返回文本。
		//
		// ★ 用 Opts 变体而不是 InjectInputSync：ACP 入站是**外部 agent 的指令**，
		// 不该进图记忆、不该据它召回别人的记忆、也不该据它裁剪上下文。
		// 声明「本轮属于哪个 ACP 会话」走 Payload——内核把它透传到
		// stageCtx.Extra，afterOutput 凭帧上的值决定推给哪个会话的 SSE。
		// 不用全局变量：并发/中断合并时全局值会互相覆盖（见 Start 里的说明）。
		reply := ""
		if p.sdk != nil {
			reply = p.sdk.InjectInputSyncOpts(p.name, p.name,
				fmt.Sprintf("[来自ACP Agent的请求 session %s]\n%s\n[注意] 请直接以文本回复本请求，不要调用 output_send__%s——你的最终文本回复会被系统自动返回给请求方。", sid, injectText, p.name),
				sdk.InjectOptions{
					NoMemory:     true,
					RecallPolicy: sdk.RecallPolicyNone,
					ScenePolicy:  sdk.ScenePolicyNone,
					Payload:      map[string]interface{}{acpSessionKey: sid},
				})
		}

		// 写回历史 + 填充 Replying 供 SSE 消费
		p.mu.Lock()
		if cur := p.sessions[sid]; cur != nil {
			st = cur
		}
		p.sessions[sid] = st
		st.History = append(st.History, "用户: "+text, "助手: "+reply)
		if len(st.History) > maxSessionTurns*2 {
			st.History = st.History[len(st.History)-maxSessionTurns*2:]
		}
		st.LastUsed = time.Now()
		if reply != "" {
			st.Replying = append(st.Replying, map[string]interface{}{
				"type": "reply", "text": reply,
			})
		}
		p.mu.Unlock()

		// ★ 直接在这里回 reply，同时**保留**进 Replying 供 SSE 消费：
		// 两条通路的消费方不同（同步调用方 / SSE 订阅方），不能只留一条。
		writeRPCResult(w, req.ID, map[string]interface{}{
			"session": map[string]interface{}{"id": sid},
			"reply":   reply,
		})

	case "sessionget":
		// 按 session_id 返回会话内近 N 条消息（默认 10 条，时间正序）
		sid := req.Params.SessionID
		p.mu.RLock()
		st := p.sessions[sid]
		var hist []string
		if st != nil {
			hist = append([]string{}, st.History...)
		}
		p.mu.RUnlock()
		if st == nil {
			writeRPCResult(w, req.ID, map[string]interface{}{
				"session_id": sid,
				"status":     "not_found",
				"messages":   []interface{}{},
			})
			return
		}
		limit := 10
		if req.Params.Limit > 0 && req.Params.Limit <= 100 {
			limit = req.Params.Limit
		}
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
		writeRPCResult(w, req.ID, map[string]interface{}{
			"session_id": sid,
			"status":     "completed",
			"messages":   msgs,
		})

	case "sessionupdate":
		sid := req.Params.SessionID
		p.mu.Lock()
		st := p.sessions[sid]
		if st != nil && req.Params.Final {
			// 客户端结束会话：标记并保留历史（后续可再 session/new 续）
			st.LastUsed = time.Now()
		}
		p.mu.Unlock()
		if st == nil {
			writeRPCErr(w, req.ID, -32001, "session not found: "+sid)
			return
		}
		writeRPCResult(w, req.ID, map[string]interface{}{"final": true})

	case "sessioncancel":
		p.mu.Lock()
		delete(p.sessions, req.Params.SessionID)
		p.mu.Unlock()
		writeRPCResult(w, req.ID, map[string]interface{}{"canceled": true})

	default:
		// 回显**收到的**方法名：只说 unknown method 时调用方无法判断是
		// 自己拼错还是服务端版本旧。
		writeRPCErr(w, req.ID, -32601,
			fmt.Sprintf("未知方法 %q（支持 session/new、session/get、session/update、session/cancel）", req.Method))
	}
}

// handleSessionSSE 提供 SSE 事件流订阅
func (p *Plugin) handleSessionSSE(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("id")
	if sid == "" {
		http.Error(w, "id query param required", http.StatusBadRequest)
		return
	}

	p.mu.RLock()
	st := p.sessions[sid]
	p.mu.RUnlock()
	if st == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// 让反向代理不要缓冲 SSE（否则客户端看到的是「一次性到达」而不是流）。
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		p.mu.RLock()
		// 重新按 sid 取：会话可能在等待期间被 cancel（cancel 会 delete）。
		// 持有 st 指针不重取的话，这里会往一个已被删除的会话里写——
		// 内存不泄漏，但订阅方会永远收不到后续回复（与 HTTP 连接状态不一致）。
		cur := p.sessions[sid]
		var replies []map[string]interface{}
		if cur == st {
			replies = append(replies, st.Replying...)
		}
		p.mu.RUnlock()

		if cur != st {
			// 会话已结束：告知订阅方并关闭，而不是静默挂着。
			fmt.Fprintf(w, "event: session/closed\ndata: %s\n\n",
				`{"reason":"session closed or expired"}`)
			fl.Flush()
			return
		}

		if len(replies) > 0 {
			p.mu.Lock()
			if p.sessions[sid] == st {
				st.Replying = nil
			}
			p.mu.Unlock()
			for _, rep := range replies {
				data, _ := json.Marshal(rep)
				fmt.Fprintf(w, "event: session/reply\ndata: %s\n\n", data)
			}
			fl.Flush()
		}

		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			// 心跳注释帧：无它时中间代理/内核会在空闲 60~90s 后静默断连，
			// 而 SSE 消费者只能看到一个「没有新事件」的假象。
			fmt.Fprint(w, ": keep-alive\n\n")
			fl.Flush()
		}
	}
}

// ---- stage：把 ACP 会话绑到帧上（照 qq 的 qq_auth 做法）----
//
// 为什么不只用全局变量：同一轮推理可能由多条注入合并而成（中断抢占、
// 并发到达），全局变量在内核侧是后写覆盖先写。stage 拿到的若是全局值，
// 回复就可能推错会话。绑到 Extra 上后，「本帧对应哪个 ACP 会话」是确定的。

// acpSessionKey 是 Payload / Extra 里的会话键。
//
// 加插件名前缀（不用裸 "session_id"）：后者太通用，将来内核若加同名字段会撞车。
const acpSessionKey = "acp_sid"

// stageBindSession 在 on_input 把 Payload 里的会话钉到本帧 Extra 上。
func (p *Plugin) stageBindSession(ctx *sdk.StageContext) error {
	ctx.RLock()
	sid, _ := ctx.Extra[acpSessionKey].(string)
	ctx.RUnlock()
	if sid == "" {
		// 非 ACP 发起的输入（用户直接对话、定时器等）：不绑定，afterOutput 跳过。
		return nil
	}
	ctx.Lock()
	if ctx.Extra == nil {
		ctx.Extra = make(map[string]interface{})
	}
	ctx.Extra[acpSessionKey] = sid
	ctx.Unlock()
	return nil
}

// stagePushReply 在 after_output 把本轮最终文本推给**本帧**对应的 ACP 会话。
//
// 为什么需要它：同步注入（InjectInputSync）只覆盖「HTTP 请求在等回复」这一条
// 路径。如果这一轮是 agent 自循环/定时器/工具链续轮产生的，注入早已返回，
// 回复就永远到不了 SSE 订阅方。这里把 after_output 的 FinalText 补进会话，
// 让 SSE 成为一个**完整的**事件流（而不只是同步回应的镜像）。
func (p *Plugin) stagePushReply(ctx *sdk.StageContext) error {
	ctx.RLock()
	sid, _ := ctx.Extra[acpSessionKey].(string)
	final := ctx.FinalText
	ctx.RUnlock()
	if sid == "" || final == "" {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.sessions[sid]
	if st == nil {
		return nil // 会话已 cancel 或过期：丢弃而不是创建一个幽灵会话
	}
	// 去重：同步路径已把同一段文本写进 Replying，这里不重复推。
	for _, rep := range st.Replying {
		if t, _ := rep["text"].(string); t == final {
			return nil
		}
	}
	st.Replying = append(st.Replying, map[string]interface{}{
		"type": "reply", "text": final,
	})
	st.LastUsed = time.Now()
	return nil
}

// canonicalACP 归一化方法名（session/new、session.new、session_new 等价）。
//
// 与 A2A 同一个理由：协议名在真实客户端里写法不一，只认一种就会
// 把整条链路做成 400 + "unknown method"，且难以从客户端侧看出原因。
// '.' 必须一起剔除（同 canonicalMethod 里的教训）。
func canonicalACP(method string) string {
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

// lockTurn 取同一会话的「一轮对话」互斥，返回解锁函数。
//
// ★ 为什么不在解锁时删表：
//
//	A 持锁中；B 到达，拿到同一 sem 并阻塞在发送上；
//	A 释放 → 拆表 → B 拿到锁继续；
//	C 到达，发现表里没 sem ⇒ 新建一个 ⇒ **B 与 C 同时持锁**。
//
// 拆表这个看似无害的清理动作会直接消掉互斥性。sem 生命周期与 session 一致，
// 由 cancel / 过期清理（见 sweepSessions）。
func (p *Plugin) lockTurn(sid string) func() {
	p.turnMu.Lock()
	if p.turnSem == nil {
		p.turnSem = make(map[string]chan struct{})
	}
	sem := p.turnSem[sid]
	if sem == nil {
		sem = make(chan struct{}, 1)
		p.turnSem[sid] = sem
	}
	p.turnMu.Unlock()

	sem <- struct{}{}
	return func() { <-sem }
}

// sweepSessions 回收长期未用的会话与其信号量。
func (p *Plugin) sweepSessions() {
	const idle = 2 * time.Hour
	p.mu.Lock()
	expired := make([]string, 0, 8)
	for id, st := range p.sessions {
		if time.Since(st.LastUsed) > idle {
			delete(p.sessions, id)
			expired = append(expired, id)
		}
	}
	p.mu.Unlock()
	if len(expired) == 0 {
		return
	}
	p.turnMu.Lock()
	for _, id := range expired {
		// 只回收已空闲的（通道里无令牌 = 无人在持锁）。删掉正在用的 sem
		// 会让下一个请求新建一个 ⇒ 新旧两把锁并存 ⇒ 互斥消失（见 lockTurn）。
		if sem := p.turnSem[id]; sem != nil && len(sem) == 0 {
			delete(p.turnSem, id)
		}
	}
	p.turnMu.Unlock()
}

func (p *Plugin) sessionGCLoop() {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		p.sweepSessions()
	}
}

// buildACPContextualInput 把历史与本次输入拼成注入文本。
//
// 分隔 + 转义的理由：历史条目里直接含对端原始输入，不标记的话，
// 一个构造过的 prompt（正文写 "[本轮输入]"）就能伪造出“本轮”的开端，
// 把自己的后半段伪装成历史。这不是密码学边界，但把段落归属变成确定的。
func buildACPContextualInput(history []string, text string) string {
	if len(history) == 0 {
		return text
	}
	var b strings.Builder
	b.WriteString("[对话上下文]\n")
	for _, line := range history {
		if strings.HasPrefix(line, "[") {
			line = "［" + line[1:]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("[本轮输入]\n")
	b.WriteString(text)
	return b.String()
}

// writeRPCResult / writeRPCErr 保证响应形状一致（JSON-RPC 2.0）。
func writeRPCResult(w http.ResponseWriter, id interface{}, result map[string]interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"jsonrpc": "2.0", "id": id, "result": result,
	})
}

func writeRPCErr(w http.ResponseWriter, id interface{}, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]interface{}{"code": code, "message": msg},
	})
}

// ---- Outbound：ACP 客户端 ----

// parseRPCBody 兼容 JSON 与 SSE 两种响应体
func parseRPCBody(ct string, body []byte) (*json.RawMessage, error) {
	if strings.Contains(ct, "text/event-stream") {
		sc := bufio.NewScanner(bytes.NewReader(body))
		var last string
		for sc.Scan() {
			line := strings.TrimRight(sc.Text(), "\r")
			if strings.HasPrefix(line, "data:") {
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data != "" && data != "[DONE]" {
					last = data
				}
			}
		}
		if last == "" {
			return nil, fmt.Errorf("SSE body 中无 data 帧: %s", truncateStr(string(body), 200))
		}
		body = []byte(last)
	}
	var raw json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析响应失败: %v: %s", err, truncateStr(string(body), 300))
	}
	return &raw, nil
}

func truncateStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func (p *Plugin) handleAcpQuery(args map[string]interface{}) (interface{}, error) {
	serverURL, _ := args["server_url"].(string)
	serverURL = strings.TrimRight(strings.TrimSpace(serverURL), "/")
	if serverURL == "" {
		return map[string]interface{}{"error": "server_url 不能为空"}, nil
	}
	if !strings.HasPrefix(serverURL, "http://") && !strings.HasPrefix(serverURL, "https://") {
		serverURL = "http://" + serverURL
	}
	prompt, _ := args["prompt"].(string)
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return map[string]interface{}{"error": "prompt 不能为空"}, nil
	}
	sessionID, _ := args["session_id"].(string) // 可选：延续对方会话
	timeoutSec := 120
	if v, ok := args["timeout"].(float64); ok && v > 0 {
		timeoutSec = int(v)
	}

	endpoint := serverURL + "/api/session"
	client := &http.Client{Timeout: time.Duration(timeoutSec) * time.Second}

	params := map[string]interface{}{
		"request": map[string]interface{}{"text": prompt},
	}
	if sessionID != "" {
		params["session_id"] = sessionID
	}
	newBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": "acp-" + fmt.Sprintf("%d", time.Now().UnixNano()),
		"method": "session/new",
		"params": params,
	})

	req, _ := http.NewRequest("POST", endpoint, bytes.NewReader(newBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := client.Do(req)
	if err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("请求失败(超时%d秒): %v", timeoutSec, err)}, nil
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 原写法 `code < 200 || code >= 300 && code != 202` 依靠 Go 的
		// && 优先于 || 才勉强正确，且把 202 当成成功——但 202 意味着
		// “已接受、异步处理”，而本函数必需同步拿到 reply，拿不到只会在
		// 下游变成一句含糊的「未收到回复」。
		return map[string]interface{}{"error": fmt.Sprintf("状态码 %d", resp.StatusCode), "raw_body": truncateStr(string(body), 300)}, nil
	}

	raw, err := parseRPCBody(resp.Header.Get("Content-Type"), body)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	var rpcResp struct {
		Result *struct {
			Session *struct {
				ID string `json:"id"`
			} `json:"session,omitempty"`
			SessionID string `json:"sessionId,omitempty"`
			Reply     string `json:"reply,omitempty"`
		} `json:"result,omitempty"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(*raw, &rpcResp); err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("JSON-RPC 解析失败: %v", err), "raw_body": truncateStr(string(*raw), 300)}, nil
	}
	if rpcResp.Error != nil {
		return map[string]interface{}{"error": fmt.Sprintf("ACP 错误 [%d]: %s", rpcResp.Error.Code, rpcResp.Error.Message)}, nil
	}
	if rpcResp.Result == nil {
		return map[string]interface{}{"error": "响应中没有 result", "raw_body": truncateStr(string(*raw), 300)}, nil
	}

	// 兼容两种协议：
	//   A) 标准/SSE 型（opencode、本插件服务端）：result.session.id，回复经 SSE 事件流
	//   B) 同步 JSON 型（pi bridge）：result.sessionId + result.reply
	if rpcResp.Result.Reply != "" {
		return map[string]interface{}{
			"session_id": rpcResp.Result.SessionID,
			"status":     "completed",
			"reply":      rpcResp.Result.Reply,
		}, nil
	}
	if rpcResp.Result.Session == nil || rpcResp.Result.Session.ID == "" {
		return map[string]interface{}{"error": "响应中没有 session.id", "raw_body": truncateStr(string(*raw), 300)}, nil
	}
	sid := rpcResp.Result.Session.ID

	replyText := p.readSSEReply(endpoint, sid, client, timeoutSec)

	return map[string]interface{}{
		"session_id": sid,
		"status":     "completed",
		"reply":      replyText,
		"note":       "延续会话：下次调用传此 session_id 可保持上下文",
	}, nil
}

// readSSEReply 通过 SSE 读取 session/reply 事件并拼接回复文本
func (p *Plugin) readSSEReply(endpoint, sid string, client *http.Client, timeoutSec int) string {
	sseURL := fmt.Sprintf("%s?id=%s", endpoint, sid)
	req, _ := http.NewRequest("GET", sseURL, nil)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Sprintf("(SSE 读取失败: %v)", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bb, _ := io.ReadAll(resp.Body)
		return fmt.Sprintf("(SSE 状态码 %d: %s)", resp.StatusCode, truncateStr(string(bb), 200))
	}

	var sb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	for sc.Scan() {
		if time.Now().After(deadline) {
			break
		}
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.HasPrefix(line, "event: ") && strings.TrimSpace(strings.TrimPrefix(line, "event: ")) == "session/error" {
			break
		}
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}
			var evt struct {
				SessionID string `json:"session_id,omitempty"`
				Type      string `json:"type,omitempty"`
				Text      string `json:"text,omitempty"`
				Message   *struct {
					Text string `json:"text"`
				} `json:"message,omitempty"`
			}
			if json.Unmarshal([]byte(data), &evt) == nil {
				text := evt.Text
				if evt.Message != nil && evt.Message.Text != "" {
					text = evt.Message.Text
				}
				if text != "" {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(text)
				}
			}
		}
	}
	if sb.Len() == 0 {
		return "(未收到回复)"
	}
	return sb.String()
}

// ---- Management ----

func (p *Plugin) handleConfigure(args map[string]interface{}) (interface{}, error) {
	listen, _ := args["listen"].(string)
	listen = strings.TrimSpace(listen)

	if err := p.sdk.Settings().Set("listen", listen); err != nil {
		return fmt.Sprintf("保存配置失败: %v", err), nil
	}

	if listen == "" || listen == "off" || listen == "disabled" {
		p.stopServer()
		return "ACP HTTP 服务已禁用", nil
	}

	if err := p.startServer(listen); err != nil {
		return fmt.Sprintf("ACP 配置已保存，但服务启动失败: %v", err), nil
	}
	return fmt.Sprintf("ACP 配置已更新，监听: %s", listen), nil
}

func (p *Plugin) handleStatus(args map[string]interface{}) (interface{}, error) {
	addr, _ := p.sdk.Settings().Get("listen")
	addrStr, _ := addr.(string)

	p.srvMu.Lock()
	serverRunning := p.server != nil
	listening := p.serverID
	p.srvMu.Unlock()

	p.mu.RLock()
	n := len(p.sessions)
	p.mu.RUnlock()

	if !serverRunning {
		listening = "未运行"
	}
	return fmt.Sprintf("配置监听地址: %s\n当前监听: %s\n服务状态: %s\n活跃会话: %d",
		addrStr, listening, map[bool]string{true: "运行中", false: "已停止"}[serverRunning], n), nil
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}
