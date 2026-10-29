package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JianFeeeee/hologramagentsdk/sdk"
)

type ForwardRule struct {
	GroupID  int64  `json:"group_id"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Password string `json:"password"`
	Template string `json:"template"`
}

func rconSend(host string, port int, password, cmd string) error {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("rcon dial: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	buf := make([]byte, 4096)
	// Login
	pkt := rconPacket(1, 3, password)
	if _, err := conn.Write(pkt); err != nil {
		return fmt.Errorf("rcon login write: %w", err)
	}
	if _, err := io.ReadFull(conn, buf[:12]); err != nil {
		return fmt.Errorf("rcon login read: %w", err)
	}
	// Command
	pkt = rconPacket(2, 2, cmd)
	if _, err := conn.Write(pkt); err != nil {
		return fmt.Errorf("rcon cmd write: %w", err)
	}
	n, err := io.ReadFull(conn, buf[:12])
	if err != nil && err != io.ErrUnexpectedEOF {
		return fmt.Errorf("rcon cmd read: %w (n=%d)", err, n)
	}
	return nil
}

func rconPacket(id, typ int32, body string) []byte {
	b := []byte(body)
	b = append(b, 0) // null terminator
	b = append(b, 0) // padding
	length := 4 + 4 + len(b)
	pkt := make([]byte, 12+len(b))
	binary.LittleEndian.PutUint32(pkt, uint32(length))
	binary.LittleEndian.PutUint32(pkt[4:], uint32(id))
	binary.LittleEndian.PutUint32(pkt[8:], uint32(typ))
	copy(pkt[12:], b)
	return pkt
}

type DownloadTask struct {
	FileID    string `json:"file_id"`
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Path      string `json:"path,omitempty"`
	Error     string `json:"error,omitempty"`
	CreatedAt string `json:"created_at"`
}

// qqAuthContext 是绑帧身份。字段导出 + json tag：proc 插件下身份要经
// 共享段 JSON 往返（内核 shm 桥 v2 的 fExtraBag），非导出字段会在
// marshal 时静默丢失 —— 那正是 2026-10-12 权限门失效的根因链一环。
type qqAuthContext struct {
	Active     bool   `json:"active"`
	Owner      bool   `json:"owner"`
	MessageID  int64  `json:"message_id"`
	UserID     int64  `json:"user_id"`
	GroupID    int64  `json:"group_id"`
	IsGroup    bool   `json:"is_group"`
	Generation uint64 `json:"generation"`
}

// authFromMap 按 JSON 往返约定重建身份（数字统一 float64）。
// 解不出完整身份（键缺失/类型错）返回 false —— 宁回落全局也不猜：
// 半截的身份比没有更危险（owner 误判即提权）。
func authFromMap(m map[string]interface{}) (qqAuthContext, bool) {
	var a qqAuthContext
	getBool := func(k string) (bool, bool) {
		v, ok := m[k].(bool)
		return v, ok
	}
	getInt := func(k string) (int64, bool) {
		f, ok := m[k].(float64)
		if !ok {
			return 0, false
		}
		return int64(f), true
	}
	var ok bool
	if a.Active, ok = getBool("active"); !ok {
		return a, false
	}
	if a.Owner, ok = getBool("owner"); !ok {
		return a, false
	}
	if a.MessageID, ok = getInt("message_id"); !ok {
		return a, false
	}
	if a.UserID, ok = getInt("user_id"); !ok {
		return a, false
	}
	if a.GroupID, ok = getInt("group_id"); !ok {
		return a, false
	}
	if a.IsGroup, ok = getBool("is_group"); !ok {
		return a, false
	}
	if g, present := m["generation"].(float64); present {
		a.Generation = uint64(g)
	}
	return a, true
}

// defaultHardPrivateTools 是**旧写死清单的等价默认值**（行为不变），
// 但改成配置项 hard_private_tools，用户可增删或清空。
//
// 为什么改成可配置：群里/朋友对话中「下载我刚发的图」「读一下这个文档」
// 这类请求本身不涉及私人资源，旧清单把 qq_download_file 之外的通用能力
// （files_/terminal_/cmd_）整块封死，正常交流也被打断；而各部署的信任边界
// 并不相同 —— 由部署者决定，而不是写死在代码里。
var defaultHardPrivateParsed = parseToolAllowlist(defaultHardPrivateTools)

const defaultHardPrivateTools = `["calendar_*","email_*","mail_*","agentmail_*","memory_*","knowledge_*","device_*","devicectl_*","terminal_*","shell_*","command_*","exec_*","filesystem_*","agentfs_*","config_*","settings_*","plugin_*","plugins_*","read_file","write_file","edit_file","delete_file","list_files","run_command","homeagent_config","homeagent_restart","output_send__email","output_send__mail"]`

var qqMessageIDRe = regexp.MustCompile(`message_id=(-?\d+)`)

// 默认只开放公共信息与当前 QQ 会话所需能力。日历、邮件、记忆、知识库、
// 主机文件/命令、设备、配置、插件管理及 QQ 联系人/跨会话列表均不在白名单中。
const defaultPublicToolAllowlist = `["output_send__qq","output_list_channels","qq_get_message","qq_get_history","qq_mark_read","qq_get_group_member_info","qq_get_group_files","qq_video_download","weather_*","browser_search","browser_fetch","browser_render","ocr_*","multimodal_*","qq_download_file","qq_read_document","qq_get_download_tasks","qq_list_stickers","qq_add_sticker","qq_describe_sticker","qq_get_groups","qq_resolve_name","seemedia_*","see_image","see_video","hear_audio","bili_*","music_*","ai_image_*"]`

const defaultGroupToolAllowlists = `{"*":["output_send__qq","output_list_channels","qq_get_message","qq_get_history","qq_mark_read","qq_get_group_member_info","qq_get_group_files","qq_video_download","weather_*","browser_search","browser_fetch","browser_render","ocr_*","multimodal_*","qq_download_file","qq_read_document","qq_get_download_tasks","qq_list_stickers","qq_add_sticker","qq_describe_sticker","qq_get_groups","qq_resolve_name","seemedia_*","see_image","see_video","hear_audio","bili_*","music_*","ai_image_*"]}`

const (
	// 单轮 QQ 触发的工具调用总数上限（0 = 不限制）。只作跑飞兜底，
	// 不应拦下正常的长时间多步任务。
	defaultMaxQQToolCalls = 200
	// 单轮 QQ 主动发送的不同消息条数上限（0 = 不限制）。
	defaultMaxQQOutputCalls = 20
	// 单轮内同一条消息（参数完全相同）允许重复发送的次数（0 = 不限制）。
	defaultMaxDuplicateSend = 1
)

type Plugin struct {
	name         string
	sdk          *sdk.PluginSDK
	listenAddr   string
	napcatURL    string
	remoteDir    string
	filesDir     string
	webhookToken string
	ownerIDs     []int64
	botID        int64
	botNickname  string
	dmPolicy     string
	groupPolicy  string
	stickerMu    sync.RWMutex
	stickerDescs map[string]stickerDesc // url -> 描述（模型看过之后记录，见 qq_describe_sticker）
	httpClient   *http.Client
	// sendClient 专用于**发送类**端点（send_private_msg / send_group_msg）。
	//
	// ★ 为何与查询类分开（2026-10-13 实测）：QQ 发送要经过 NapCat 的
	//   NodeIKernelMsgService/sendMsg，慢时响应头 30s 都回不来。旧实现
	//   查询与发送共用 30s 客户端，于是：
	//     消息**已经发出**（NapCat 返回 {"retcode":0,"data":{"message_id":...}}，
	//     用户也真收到了），但 Go 报 "context deadline exceeded"
	//   ⇒ 工具结果被报成「发送失败」，模型据此重试。
	//   实测日志（memory/context.json）：
	//     napcat send_private_msg: Post ".../send_private_msg":
	//       context deadline exceeded (Client.Timeout exceeded while awaiting headers)
	//   同一内容重试后用户只收到一份（qq 循环保险拦住了重复），但那属于**侥幸**，
	//   不是正确行为：模型会反复重试、白白耗尽轮数，且每次都是假失败。
	//
	//   发送超时给足 120s（与插件内 TTS/下载同量级）；查询类保持 30s
	//   （它们是幂等的，超时就是真超时，快速失败更有利于排查）。
	sendClient     *http.Client
	allowFrom      map[int64]struct{}
	groupAllowFrom map[int64]struct{}
	srv            *http.Server
	agentfsDir     string
	downloadMu     sync.Mutex
	downloadTasks  []*DownloadTask
	typingMu       sync.Mutex
	// toolCount 是本次启动实际注册的工具数，仅用于启动日志。
	//
	// 存在的原因：这里原本在日志里**硬编码 15**，工具加到 24 后日志仍报 15，
	// 导致排查时误以为装的是旧版。日志必须说真话。
	toolCount int
	typingMap map[int64]*typingState

	// statusAutoEnabled 是「随内核阶段自动切 QQ 在线状态」的总开关。
	//
	// 默认**关闭**：自动改 QQ 状态是内核替用户做决定（与 offload 同理，
	// 属于显式特权）。且它对外可见 —— 好友会看到你的状态在跳。
	statusAutoEnabled atomic.Bool
	statusState       statusState

	authMu               sync.RWMutex
	auth                 qqAuthContext
	authGeneration       uint64
	authByMessageID      map[int64]qqAuthContext
	lastDenial           string
	denialLocked         bool
	toolCallCount        int
	outputCallCount      int
	outputSignatures     map[string]int
	maxQQToolCalls       int
	maxQQOutputCalls     int
	maxDuplicateSend     int
	groupToolAllowlists  map[int64][]string // 0 表示通配配置 "*"
	privateToolAllowlist []string

	// 权限门策略（均可配置）。字段用**反向**命名，让零值 = 旧行为（严格）：
	// 单测/未初始化时不至于把门悄悄放开 —— 这是安全默认。
	groupRequireAtOff bool // true = 群里不 @ 也响应
	// replyChance 是**未 @ 机器人**的群消息被注入的概率（0~100）。
	//
	// 语义（2026-10-27 用户口径）：
	//   · 消息 @ 了机器人 → **100%** 注入（确定性，不看概率）
	//   · 未 @ 的群消息   → 按本概率随机注入（0=从不，100=全量）
	// 与 group_require_at 的关系：该开关仍是总闸（false 时不做概率，全部注入）。
	replyChance           int
	hardPrivateTools      []string // 非所有者即使白名单也拦的工具（精确名 / 前缀*）
	hardPrivateToolsSet   bool     // 是否显式配置过（未配置 ⇒ 用内置默认清单）

	// msg_id → peer 映射 + 会话最新状态（<7 天兜底 get_history + list_chats）
	msgMu  sync.Mutex
	msgMap map[int64]msgRef    // message_id → {peer, time}
	chats  map[int64]*chatMeta // peerID → 会话状态（群号或 QQ 号）

	// 消息合并（debounce）：同一会话、同一发送者在 batchWindow 内连续到达的消息
	// 合并成一次中断。同一个人连发「在吗」「帮我看看」「报错是这个」三条，
	// 逐条注入会把 Agent 唤醒三次，且前两次拿到的信息都不完整。
	batchMu     sync.Mutex
	batches     map[string]*pendingBatch
	batchWindow time.Duration // 最后一条到达后再等多久（<=0 = 关闭合并，逐条投递）
	batchMax    time.Duration // 一批最长等多久（防持续刷屏时永远不投）

	// injectHook 仅供测试：非 nil 时 injectInterrupt 走它而不是真实 SDK。
	injectHook func(text, level string)
}

// pendingBatch 是一批待投递的消息（同一会话、同一发送者、短时间内的连续消息）。
type pendingBatch struct {
	key      string
	isGroup  bool
	userID   int64
	groupID  int64
	nickname string
	msgIDs   []int64
	single   string // 单条时沿用的原文（含所有者/高危前缀），保证 n==1 行为不变
	owner    bool
	highRisk bool
	first    time.Time
	timer    *time.Timer
}

// qqBatchKey 同一会话 + 同一发送者 = 一组。私聊按 QQ 号；群聊按 (群号, QQ 号)——
// 群里不同人各发各的，不该并成一条。
func qqBatchKey(msgType string, groupID, userID int64) string {
	if msgType == "group" {
		return fmt.Sprintf("g:%d:%d", groupID, userID)
	}
	return fmt.Sprintf("p:%d", userID)
}

type typingState struct {
	userID int64
	stopCh chan struct{}
}

// msgRef 一条已见过的消息的引用：只记录 msg_id → (peer, time) 映射，不缓存正文。
// 用途：NapCat get_msg 的临时短号 <7 天失效时，据此把 get_msg 兜底为按 peer 拉 get_history。
type msgRef struct {
	peerID  int64
	isGroup bool
	time    int64 // 秒级时间戳
}

// chatMeta 一个会话（群/私聊）的最新状态，供 list_chats 展示。
// 只维护最新一条的短摘要（≤qqLastSumLen 字符）与未读数，不缓存完整历史。
type chatMeta struct {
	peerID   int64
	isGroup  bool
	name     string
	unread   int
	lastTime int64
	lastText string
	lastNick string
}

const qqMsgTTL = 7 * 86400 // 7 天：msg_id → peer 映射的有效期
const qqLastSumLen = 60    // list_chats 里最新一条摘要的最大长度

// snapshotMsg 记录一条策略允许的消息：更新 msg_id→peer 映射与会话未读/最新状态。
// 不缓存消息正文（仅最新一条留 ≤qqLastSumLen 的摘要供列表展示）。
func (p *Plugin) snapshotMsg(msgID, peerID int64, isGroup bool, t int64, nickname, text string) {
	if msgID <= 0 {
		return
	}
	p.msgMu.Lock()
	defer p.msgMu.Unlock()

	// msg_id 映射（7 天 TTL，惰性清理）
	p.msgMap[msgID] = msgRef{peerID: peerID, isGroup: isGroup, time: t}
	now := time.Now().Unix()
	if len(p.msgMap) > 2000 { // 定期清理过期项
		for k, v := range p.msgMap {
			if now-v.time > qqMsgTTL {
				delete(p.msgMap, k)
			}
		}
	}

	ch := p.chats[peerID]
	if ch == nil {
		ch = &chatMeta{peerID: peerID, isGroup: isGroup}
		p.chats[peerID] = ch
	}
	if ch.name == "" {
		if isGroup {
			ch.name = fmt.Sprintf("群%d", peerID)
		} else {
			ch.name = nickname
		}
	}
	// 按到达次序维护未读与最新摘要：仅当本条更新时才更新 lastTime/lastText（保持按时间排）
	if t > ch.lastTime {
		ch.lastTime = t
		ch.lastText = text
		ch.lastNick = nickname
	}
	ch.unread++
}

// lookupMsgRef 查 msg_id 映射，返回 (peer, isGroup, time, ok)。超过 7 天视为无效（交给 get_history）。
func (p *Plugin) lookupMsgRef(msgID int64) (int64, bool, int64, bool) {
	p.msgMu.Lock()
	defer p.msgMu.Unlock()
	ref, ok := p.msgMap[msgID]
	if !ok {
		return 0, false, 0, false
	}
	now := time.Now().Unix()
	if now-ref.time > qqMsgTTL {
		delete(p.msgMap, msgID)
		return 0, false, 0, false
	}
	return ref.peerID, ref.isGroup, ref.time, true
}

// markChatRead 清零某会话未读数（模型处理完该会话后调用）。
func (p *Plugin) markChatRead(peerID int64) {
	p.msgMu.Lock()
	defer p.msgMu.Unlock()
	if ch := p.chats[peerID]; ch != nil {
		ch.unread = 0
	}
}

// listChats 返回会话列表：按最新消息时间降序，含未读数与最新一条摘要。
func (p *Plugin) listChats(capN int) []map[string]interface{} {
	p.msgMu.Lock()
	list := make([]*chatMeta, 0, len(p.chats))
	for _, c := range p.chats {
		list = append(list, c)
	}
	p.msgMu.Unlock()

	// 降序（最新消息在前）
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].lastTime > list[j-1].lastTime; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	if len(list) > capN {
		list = list[:capN]
	}

	out := make([]map[string]interface{}, 0, len(list))
	for _, c := range list {
		typ := "private"
		if c.isGroup {
			typ = "group"
		}
		item := map[string]interface{}{
			"peer_id":   c.peerID,
			"type":      typ,
			"name":      c.name,
			"unread":    c.unread,
			"last_text": c.lastText,
			"last_nick": c.lastNick,
		}
		if c.lastTime > 0 {
			item["last_time"] = time.Unix(c.lastTime, 0).Format("2006-01-02 15:04")
		}
		out = append(out, item)
	}
	return out
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s

	s.Settings().RegisterDef(sdk.ConfigDef{Key: "listen", Default: "0.0.0.0:25580", Type: "string", DisplayName: "监听地址", Description: "Webhook HTTP 监听地址", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "napcat_url", Default: "http://127.0.0.1:3000", Type: "string", DisplayName: "NapCat 地址", Description: "NapCat HTTP API 基础 URL", Category: "qq"})
	// 在线状态随阶段切换（默认关）。
	//
	// 默认关的理由：它对外可见（好友会看到状态在跳），属于
	// "内核替用户做决定"——与 offload 同一类，必须显式打开。
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "status_auto", Default: "false", Type: "bool", DisplayName: "随阶段切在线状态", Description: "开启后：调工具时切「搬砖中」、生成回复时切「学习中」、一轮结束切回「在线」。失败不影响工具调用", Category: "qq"})
	// turn_hints：**本轮注入的行为提示**（可选，空 = 不加）。
	//
	// 为什么放在插件而不是内核/人格：只有插件同时知道"这是 QQ、私聊还是群、
	// 对方是不是 owner、群里要不要 @、有没有收藏表情"。这些是**本轮事实**，
	// 人格只描述"我是谁"，说不清"这一轮能做什么"。
	//
	// 现场（2026-10-26）：注入文本只教"怎么取消息、往哪回"，于是模型
	// 不知道可以发表情、不知道群里要 @，风格全押在 skill 上 —— 而 skill
	// 需要模型主动去读，它经常不读。
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "turn_hints", Default: "", Type: "text",
		DisplayName: "本轮行为提示（注入给模型，可留空）",
		Description: "每条消息注入时附带的行为提示。留空则不加。", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "sticker_descs", Default: "", Type: "text", DisplayName: "表情包描述缓存",
		Description: "收藏表情的内容/情绪描述（JSON，url → {desc,mood}）。由 qq_describe_sticker 自动写入，一般不用手改；清空后 loss 描述，表情仍可发。", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "owner", Default: "", Type: "string", DisplayName: "Bot 所有者 QQ", Description: "Bot 所有者 QQ 号列表，逗号分隔。所有者无论私聊或群聊均拥有完整工具权限", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "admin", Default: "", Type: "string", DisplayName: "Bot 所有者 QQ（旧配置）", Description: "兼容旧版 admin 配置；owner 为空时作为 Bot 所有者列表", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "group_tool_allowlists", Default: defaultGroupToolAllowlists, Type: "string", DisplayName: "群聊工具白名单", Description: "JSON 对象：群号到允许工具名/前缀*的数组；* 为未单独配置群的默认白名单。Bot 所有者不受限制", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "private_tool_allowlist", Default: defaultPublicToolAllowlist, Type: "string", DisplayName: "非所有者私聊工具白名单", Description: "JSON 数组，支持工具精确名和尾部 * 前缀。硬性私人资源工具不能由此白名单放行；Bot 所有者不受权限限制", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "max_qq_tool_calls", Default: defaultMaxQQToolCalls, Type: "int", DisplayName: "单轮 QQ 工具调用上限", Description: "QQ 输入触发的单轮推理最多调用工具次数（0=不限制）；仅作跑飞兜底，不拦参数不同的必需调用", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "max_qq_output_calls", Default: defaultMaxQQOutputCalls, Type: "int", DisplayName: "单轮 QQ 发送上限", Description: "单轮最多主动发送的不同消息条数（0=不限制）；参数不同的消息不视为重复", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "max_duplicate_qq_send", Default: defaultMaxDuplicateSend, Type: "int", DisplayName: "单轮相同 QQ 发送上限", Description: "单轮内参数完全相同的 output_send__qq 允许重复的次数（0=不限制）；这才是循环保险的真正触发条件", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "dm_policy", Default: "open", Type: "string", DisplayName: "私聊策略", Description: "open / allowlist / disabled", Category: "qq", Options: []string{"open", "allowlist", "disabled"}})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "allow_from", Default: "", Type: "string", DisplayName: "私聊白名单", Description: "允许私聊机器人的 QQ 号列表，逗号分隔", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "group_reply_chance", Default: "0", Type: "int",
		DisplayName: "未 @ 时的回复概率（%）",
		Description: "0~100。群里**没有 @ 机器人**的消息按此概率随机响应；@ 了机器人则 100% 响应（不受此项影响）。0=只在被 @ 时回复，100=群里每条都回。",
		Category:    "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "group_require_at", Default: "true", Type: "bool", DisplayName: "群聊必须 @ 才响应",
		Description: "true=群里只有 @ 机器人才回应（默认，避免乱插话）；false=群里所有消息都回应。群多、想让机器人自然搭话时关闭", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "hard_private_tools", Default: defaultHardPrivateTools, Type: "string", DisplayName: "硬性私人工具限制",
		Description: "JSON 数组：非所有者即使白名单放行也会被拦的工具，支持精确名与尾部 * 前缀（如 files_*）。清空 [] = 不做硬性拦截，完全交给白名单", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "group_policy", Default: "open", Type: "string", DisplayName: "群聊策略", Description: "open / allowlist / disabled", Category: "qq", Options: []string{"open", "allowlist", "disabled"}})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "group_allow_from", Default: "", Type: "string", DisplayName: "群聊白名单", Description: "允许接入的群号列表，逗号分隔", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "forward_rules", Default: "[]", Type: "string", DisplayName: "转发规则", Description: "JSON 数组，每项 {group_id,host,port,password,template}。匹配的群消息通过 RCON 转发到 Minecraft。template 支持 {nickname} {message} 占位", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "files_dir", Default: "/home/hologramagent/qq-workspace/files", Type: "string", DisplayName: "文件存储目录", Description: "从QQ接收的文件保存目录（CQ file/image 自动下载到此目录）", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "remote_dir", Default: "/home/program/qq-workspace/remote", Type: "string", DisplayName: "NapCat容器共享目录", Description: "与NapCat容器共享的文件目录，主机路径。发文件时文件会复制到此目录，NapCat内部映射为/app/files/", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "webhook_token", Default: "", Type: "string", DisplayName: "Webhook 令牌", Description: "NapCat 上报请求头 X-Webhook-Token 校验值，留空则不校验", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "agentfs_dir", Default: "/home/hologramagent/qq-workspace", Type: "string", DisplayName: "AgentFS目录", Description: "文件读写的工作目录，read_document/video_download 等工具的默认工作目录", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "batch_window_ms", Default: "1500", Type: "int", DisplayName: "消息合并窗口(毫秒)", Description: "同一会话同一发送者的连续消息在该窗口内合并成一次中断并告知共几条；0=关闭合并（逐条投递）", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "batch_max_ms", Default: "30000", Type: "int", DisplayName: "消息合并上限(毫秒)", Description: "一批消息最长等这么久就投递，避免对方持续刷屏时一直不唤醒 Agent", Category: "qq"})

	settings := s.Settings()

	p.listenAddr = getSetting[string](settings, "listen", "0.0.0.0:25580")
	p.webhookToken = getSetting[string](settings, "webhook_token", "")
	p.napcatURL = strings.TrimRight(getSetting[string](settings, "napcat_url", "http://127.0.0.1:3000"), "/")
	// 自动状态切换的总开关（默认 false，见 RegisterDef 处的理由）。
	//
	// ★ 用 getSetting[string] 而不是 bool：该泛型约束是
	// string | int64 | float64（不含 bool），SettingsAPI.Get 返回
	// interface{} ⇒ 布尔值在存储层是 "true"/"false" 字符串。
	if strings.EqualFold(getSetting[string](settings, "status_auto", "false"), "true") {
		p.statusAutoEnabled.Store(true)
		log.Printf("[qq] 自动在线状态切换已开启（随内核阶段）")
	}
	ownerRaw := getSetting[string](settings, "owner", "")
	if strings.TrimSpace(ownerRaw) == "" {
		ownerRaw = getSetting[string](settings, "admin", "")
	}
	p.ownerIDs = parseIDList(ownerRaw)
	p.groupToolAllowlists = parseGroupToolAllowlists(getSetting[string](settings, "group_tool_allowlists", defaultGroupToolAllowlists))
	p.privateToolAllowlist = parseToolAllowlist(getSetting[string](settings, "private_tool_allowlist", defaultPublicToolAllowlist))
	p.groupRequireAtOff = !strings.EqualFold(getSetting[string](settings, "group_require_at", "true"), "true")
	// 未 @ 时的回复概率：越界值夹到 0..100，非法值回落 0 并留日志
	//（静默当成 100 会让机器人突然开始刷群，那是灾难性误配置）。
	{
		raw := strings.TrimSpace(getSetting[string](settings, "group_reply_chance", "0"))
		n, err := strconv.Atoi(raw)
		if err != nil {
			if raw != "" {
				log.Printf("[qq] group_reply_chance %q 非法，按 0 处理", raw)
			}
			n = 0
		}
		if n < 0 {
			n = 0
		}
		if n > 100 {
			n = 100
		}
		p.replyChance = n
	}
	p.hardPrivateTools = parseToolAllowlist(getSetting[string](settings, "hard_private_tools", defaultHardPrivateTools))
	p.hardPrivateToolsSet = true
	p.maxQQToolCalls = nonNegativeOrDefault(int(getSetting[int64](settings, "max_qq_tool_calls", int64(defaultMaxQQToolCalls))), defaultMaxQQToolCalls)
	p.maxQQOutputCalls = nonNegativeOrDefault(int(getSetting[int64](settings, "max_qq_output_calls", int64(defaultMaxQQOutputCalls))), defaultMaxQQOutputCalls)
	p.maxDuplicateSend = nonNegativeOrDefault(int(getSetting[int64](settings, "max_duplicate_qq_send", int64(defaultMaxDuplicateSend))), defaultMaxDuplicateSend)
	p.dmPolicy = normalizePolicy(getSetting[string](settings, "dm_policy", "open"))
	p.groupPolicy = normalizePolicy(getSetting[string](settings, "group_policy", "open"))
	p.allowFrom = parseIDSet(getSetting[string](settings, "allow_from", ""))
	p.groupAllowFrom = parseIDSet(getSetting[string](settings, "group_allow_from", ""))
	p.filesDir = strings.TrimRight(getSetting[string](settings, "files_dir", "/home/hologramagent/qq-workspace/files"), "/")
	p.agentfsDir = strings.TrimRight(getSetting[string](settings, "agentfs_dir", "/home/hologramagent/qq-workspace"), "/")
	p.remoteDir = strings.TrimRight(getSetting[string](settings, "remote_dir", "/home/program/qq-workspace/remote"), "/")
	p.batchWindow = time.Duration(getSetting[int64](settings, "batch_window_ms", 1500)) * time.Millisecond
	p.batchMax = time.Duration(getSetting[int64](settings, "batch_max_ms", 30000)) * time.Millisecond
	if p.batchWindow < 0 {
		p.batchWindow = 0
	}
	os.MkdirAll(p.remoteDir, 0755)
	p.loadStickerDescs()

	// ★ 声明安全档位（必须在 hard_private_tools 读入之后）。
	//
	// 声明是**同步**的：DeclareProfiles 返回即表示内核已登记，
	// 之后注入带 qq.guest 才不会报"未声明档位"。
	p.declareSecurityProfiles(s)

	p.httpClient = &http.Client{Timeout: 30 * time.Second}

	// ★ Transport 显式配置：默认 Transport 的 idle 连接无上限等待，
	//   复用对端已单向关闭的 keep-alive 连接时，请求可能已写入 socket
	//   （对端已处理）而本地读响应时拿到 EOF —— 又一种「已发送却报失败」。
	//   IdleConnTimeout 主动淘汰空闲连接，避免复用濒死连接。
	sendTransport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	p.sendClient = &http.Client{Timeout: 120 * time.Second, Transport: sendTransport}

	// msg_id → peer 映射 + 会话状态（不缓存正文）
	p.msgMap = make(map[int64]msgRef)
	p.batches = make(map[string]*pendingBatch)
	p.chats = make(map[int64]*chatMeta)

	// 从 NapCat 获取 Bot 身份（阻塞等待，最多 5s）
	p.fetchBotInfo()
	if p.botID == 0 {
		log.Printf("[qq] warning: 获取 Bot 身份失败，群 @ 检查将拒绝所有未提及消息")
	}

	tp := p.name + "_"

	botInfo := ""
	if p.botNickname != "" {
		botInfo = fmt.Sprintf("你的QQ昵称是%s", p.botNickname)
		if p.botID > 0 {
			botInfo += fmt.Sprintf("，QQ号是%d", p.botID)
		}
		botInfo += "。"
	}

	// ---- 注册输出通道 ----
	s.RegisterOutputChannel("qq", sdk.CapText|sdk.CapFile|sdk.CapImage|sdk.CapAudio,
		`发送QQ群聊/私聊消息，支持文字、语音、图片、文件。
meta JSON 格式：
{
  "group_id": 123456,       // 群号
  "user_id": 123456,        // QQ号
  "reply_to": 12345         // 可选，回复指定消息 ID
}
路由规则：仅 group_id 发群；仅 user_id 发私聊；两者同时存在时发到 group_id，并在消息头 @user_id。
type 枚举: text（文字）/ voice（语音转文字后发送）/ image（图片URL）/ sticker（表情包URL，见 qq_list_stickers）/ file（文件URL）`,
		sdk.ChannelDef{}, p.handleChannelOutput)

	// ---- 注册输入通道（记忆计算层行为） ----
	inputCleaner := func(text string) string {
		// 从中断模板中提取语义内容：消息来源和发送者昵称
		// 模板: 来自「昵称」的私聊/群聊消息(message_id=N)...
		// 模板: 【重要！老大消息】来自「昵称」...
		cleaned := text
		// 去掉模板前缀
		if strings.HasPrefix(cleaned, "【重要！老大消息】") {
			cleaned = strings.TrimPrefix(cleaned, "【重要！老大消息】")
		}
		// 提取 "来自「XXX」" 中的昵称作为关键词
		if start := strings.Index(cleaned, "来自「"); start >= 0 {
			if end := strings.Index(cleaned[start:], "」"); end >= 0 {
				nick := cleaned[start+len("来自「") : start+end]
				cleaned = nick
			}
		}
		return cleaned
	}
	// qq 通道到达的是**中断通知（meta）**，不是用户正文，不据它召回；
	// 真实正文由 qq_get_message 取回后由该工具声明 RecallPolicy=auto 触发召回。
	s.RegisterInputChannel("qq", sdk.ChannelDef{NoMemory: true, Cleaner: inputCleaner, RecallPolicy: sdk.RecallPolicyNone})

	// 查询类工具输出清洗器：提取 JSON 中的 content/文本字段参与向量化
	cleaner := func(output string) string {
		var r struct{ Content string }
		if json.Unmarshal([]byte(output), &r) == nil && r.Content != "" {
			return r.Content
		}
		return output
	}

	// ---- 消息 ----
	p.regTool(s, sdk.ToolDef{
		Name: tp + "get_message", Description: botInfo + "通过 message_id 从 NapCat 实时获取消息正文、发送者、附件等信息。message_id 从中断消息的 message_id=N 获取，或从 reply_to 的 message_id 获取。",
		NoMemory: false,
		Cleaner:  cleaner,
		// 消息正文只在当轮需要（决策怎么回复）；用完即裁剪。
		// 不裁的后果是每条 QQ 消息的完整正文都留在 L0 上下文里，
		// 长会话下持续挤占 token 预算（§13.8）。
		ContextPolicy: "prune",
		// 正文才是真实内容：取回后用**正文**触发一次召回，
		// 而不是用中断通知的 meta 文本去召回（那是无关词）。
		RecallPolicy: "auto",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"message_id": map[string]interface{}{"type": "integer", "description": "NapCat消息ID（从中断消息的 message_id=N 或 reply_to.message_id 获取）"},
			}, "required": []string{"message_id"},
		},
	}, p.handleGetMessage)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "list_stickers", Description: "列出我收藏的QQ表情包（NapCat fetch_custom_face）。返回表情图片地址列表，" +
			"可用 qq_send_sticker 发送。表情是'图'不是'文件'——发出去在 QQ 里显示为表情图。",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"count": map[string]interface{}{"type": "integer", "description": "拉取数量（默认 20，最多 100）"},
			},
		},
		NoMemory: true,
	}, p.handleListStickers)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "describe_sticker", Description: "给收藏表情记录**内容与情绪描述**（缓存到插件配置，之后 qq_list_stickers 会带出来）。" +
			"用法：先用 see_image(url) 看清这幅图，再用本工具写下画的是什么、适合什么情绪/场合。" +
			"描述写给人看也写给模型看：desc=画面内容，mood=情绪场合（开心/无语/嘲讽/撒娇…）。",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"url":   map[string]interface{}{"type": "string", "description": "表情地址（也可传 index 让插件自己查）"},
				"index": map[string]interface{}{"type": "integer", "description": "收藏表情序号（1 开始，来自 qq_list_stickers）"},
				"desc":  map[string]interface{}{"type": "string", "description": "画面内容：画的是什么、什么梗"},
				"mood":  map[string]interface{}{"type": "string", "description": "情绪/场合：开心、无语、嘲讽、撒娇…"},
			},
		},
		NoMemory: true,
	}, p.handleDescribeSticker)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "add_sticker", Description: "把别人发来的表情**添加到我自己的收藏表情**（NapCat add_custom_face）。" +
			"三种来源任选：message_id（把那条消息里的表情全部收藏）、url（表情图片地址）、file（本机图片路径）。" +
			"收藏后可用 qq_list_stickers 看到并发送。",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"message_id": map[string]interface{}{"type": "integer", "description": "消息 ID：会把该消息里所有表情图片都收藏"},
				"url":        map[string]interface{}{"type": "string", "description": "单张表情图片地址"},
				"file":       map[string]interface{}{"type": "string", "description": "本机图片路径"},
			},
		},
		NoMemory: true,
	}, p.handleAddSticker)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "remove_sticker", Description: "从我的收藏表情里**删除**一个（NapCat delete_custom_face）。" +
			"传 index（来自 qq_list_stickers）或 url。用于清理误收藏/过期表情。",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"url":   map[string]interface{}{"type": "string", "description": "表情地址（收藏列表里的 url）"},
				"index": map[string]interface{}{"type": "integer", "description": "收藏表情序号（1 开始）"},
			},
		},
		NoMemory: true,
	}, p.handleRemoveSticker)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "get_history", Description: "获取QQ群聊/私聊最近历史消息。当收到引用回复消息或需要了解对话上下文时应优先调用此工具查看前后文。返回值每条格式为 [时间] 发送者: 消息内容。如果消息包含文件，会额外返回 files 字段（含 file_id 和 name），可用 qq_download_file 工具下载。",
		NoMemory: false,
		Cleaner:  cleaner,
		// 与 get_message 同理：返回的是**真实聊天正文**，不只当轮需要，
		// 还可能牵出与这些正文相关的长期记忆。故取回后既裁剪（用完不长期占
		// L0）又据正文召回（取进来）。不声明 recall 的话就是「记忆里有、但
		// 拉回历史消息时不注入」的盲区。
		ContextPolicy: "prune",
		RecallPolicy:  "auto",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				// ★ 参数语义必须写死「私聊不要传 group_id」。
				//
				// 现场故障（2026-10-19）：模型先调 qq_get_message 取私聊正文，
				// 而私聊消息的返回体带 "group_id": 0（QQ 协议里私聊群号为 0）。
				// 它随后把整个返回体的字段原样喂给本工具，于是传入 group_id=0，
				// 后端走了 get_group_msg_history(群0) → NapCat 回「消息undefined不存在」，
				// 私聊历史永远拉不到。光在代码里把 0 判成缺失还不够 —— 模型仍需
				// 知道自己该传什么，否则会反复试错。
				"group_id": map[string]interface{}{"type": "integer", "description": "群号（仅群聊）。私聊必须省略此字段、只传 user_id；不要传 group_id=0 —— 0 不是合法群号，私聊的 group_id 在消息体里就是 0，回灌进来会查不到任何东西。"},
				"user_id":  map[string]interface{}{"type": "integer", "description": "QQ号（仅私聊历史）。私聊请只传这个，不要同时传 group_id。"},
				"count":    map[string]interface{}{"type": "integer", "description": "拉取条数，默认10"},
			}, "required": []string{},
		},
	}, p.handleGetHistory)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "list_chats", Description: "获取QQ会话列表，与真人客户端一致：按最新消息先后排序，每条标注会话（群/私聊）、会话名、未读消息数、最新一条消息摘要与时间。用于发现有未读消息的会话，再配合 qq_get_history 拉取对应会话内容、output_send__qq 回复。",
		NoMemory: false,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"count": map[string]interface{}{"type": "integer", "description": "最多返回会话数，默认10"},
			}, "required": []string{},
		},
	}, p.handleListChats)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "mark_read", Description: "将某个会话的未读计数清零（对象：群聊传 group_id，私聊传 user_id）。处理完某会话消息后可调用，让 list_chats 的未读数回到0，与真人客户端标记已读一致。",
		NoMemory: false,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"group_id": map[string]interface{}{"type": "integer", "description": "群号（与user_id二选一）"},
				"user_id":  map[string]interface{}{"type": "integer", "description": "QQ号（与group_id二选一）"},
			}, "required": []string{},
		},
	}, p.handleMarkRead)

	// ---- 查询 ----
	p.regTool(s, sdk.ToolDef{
		Name: tp + "get_groups", Description: "获取QQ群列表，可按关键词搜索群名",
		NoMemory: false,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"keyword": map[string]interface{}{"type": "string", "description": "搜索关键词（可选）"},
			},
		},
	}, p.handleGetGroups)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "get_friends", Description: "获取QQ好友列表，可按昵称/备注关键词搜索",
		NoMemory: false,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"keyword": map[string]interface{}{"type": "string", "description": "搜索关键词（可选）"},
			},
		},
	}, p.handleGetFriends)

	// ★ QQ空间（Qzone）说说。
	//
	// ## 2026-10-18 的误判，引以为戒
	//
	// 我曾断言“NapCat 不支持发说说”，依据是探测返回
	// 「不支持的Api SendQzoneMsg」。**那是我的调用方式错了**：
	// NapCat 的 action 名是下划线式的 `send_qzone_msg`，
	// 而我用的是源码里的**类名** `SendQzoneMsg`。
	//
	// 换对名字后：
	//
	//	POST /send_qzone_msg {"content":"…","ugc_right":64}
	//	→ retcode 0, tid=a5c1ca56d081d46a789a0200（实测成功，已删除）
	//
	// 教训：“API 不存在”这个结论必须用**真实 action 名**验证；
	// 源码里的类名/文件名都不是调用名。
	p.regTool(s, sdk.ToolDef{
		Name: tp + "send_qzone_msg",
		Description: "发QQ空间说说。支持纯文字或带图，可设查看权限。" +
			"ugc_right: 1所有人可见 4好友可见 16部分好友可见 64仅自己可见 128部分好友不可见" +
			"（16/128 需同时给 target_uins）。返回 tid，可用于 delete_qzone_msg。",
		NoMemory: false,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"content":     map[string]interface{}{"type": "string", "description": "说说正文"},
				"images":      map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "图片数组，支持 http(s):// / file:// / base64:// ，自动上传"},
				"ugc_right":   map[string]interface{}{"type": "integer", "description": "查看权限，默认 1（所有人可见）。见工具说明"},
				"target_uins": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "integer"}, "description": "ugc_right 为 16/128 时作用的好友 QQ 号数组"},
			}, "required": []string{"content"},
		},
	}, p.handleSendQzoneMsg)

	p.regTool(s, sdk.ToolDef{
		Name:        tp + "delete_qzone_msg",
		Description: "删除一条QQ空间说说。tid 来自 send_qzone_msg 或空间列表。",
		NoMemory:    false,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"tid": map[string]interface{}{"type": "string", "description": "说说 tid"},
			}, "required": []string{"tid"},
		},
	}, p.handleDeleteQzoneMsg)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "get_recent_contacts", Description: "查看最近有消息的联系人和群聊，返回最近消息概览（含消息数、最后一条消息内容）。可用于发现有谁发过消息但未处理。",
		NoMemory: false,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"count": map[string]interface{}{"type": "integer", "description": "获取数量，默认10"},
			},
		},
	}, p.handleGetRecentContacts)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "resolve_name", Description: "将QQ号或群号解析为可读的用户昵称或群名称",
		NoMemory: false,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"user_id":  map[string]interface{}{"type": "integer", "description": "QQ号（与group_id二选一）"},
				"group_id": map[string]interface{}{"type": "integer", "description": "群号（与user_id二选一）"},
			},
		},
	}, p.handleResolveName)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "resolve_nickname", Description: "按昵称/备注/群名片搜索QQ用户，返回匹配的QQ号和详细信息。支持搜索好友列表或指定群成员。",
		NoMemory: false,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"keyword":  map[string]interface{}{"type": "string", "description": "搜索关键词（昵称/备注/群名片）"},
				"group_id": map[string]interface{}{"type": "integer", "description": "所在群号（可选），不传则搜索好友列表"},
			}, "required": []string{"keyword"},
		},
	}, p.handleResolveNickname)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "get_group_member_info", Description: "获取QQ群成员详细信息",
		NoMemory: false,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"group_id": map[string]interface{}{"type": "integer", "description": "群号"},
				"user_id":  map[string]interface{}{"type": "integer", "description": "QQ号"},
			}, "required": []string{"group_id", "user_id"},
		},
	}, p.handleGetGroupMemberInfo)

	// ---- 群管理 ----
	p.regTool(s, sdk.ToolDef{
		Name: tp + "group_manage", Description: "QQ群综合管理。通过command参数执行各种操作：leave退群, kick踢人, ban禁言, unban解禁, rename改名, mute-all全员禁言, set-card设名片, set-admin设管理, set-title设头衔, member-list成员列表, group-info群详情, member-info成员详情, at-all-remain@全体剩余, msg-history消息历史, recall撤回, pin-msg精华, list-files文件列表, pending-requests待处理请求, folder-create创建文件夹。注意：leave/kick/ban/unban/mute-all/set-admin等破坏性操作必须先请示管理员确认后再执行。",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command":    map[string]interface{}{"type": "string", "description": "操作命令"},
				"group_id":   map[string]interface{}{"type": "integer", "description": "群号"},
				"user_id":    map[string]interface{}{"type": "integer", "description": "QQ号（踢人/禁言/设名片等需要）"},
				"message_id": map[string]interface{}{"type": "integer", "description": "消息ID（撤回/精华）"},
				"name":       map[string]interface{}{"type": "string", "description": "群名称（rename）或文件夹名（folder-create）"},
				"card":       map[string]interface{}{"type": "string", "description": "群名片（set-card）"},
				"title":      map[string]interface{}{"type": "string", "description": "群头衔（set-title）"},
				"enable":     map[string]interface{}{"type": "boolean", "description": "启用/禁用（set-admin/mute-all）"},
				"minutes":    map[string]interface{}{"type": "integer", "description": "禁言分钟数（ban），0=解禁"},
				"count":      map[string]interface{}{"type": "integer", "description": "消息条数（msg-history），默认10"},
				"folder_id":  map[string]interface{}{"type": "string", "description": "文件夹ID（list-files）"},
				"reject_add": map[string]interface{}{"type": "boolean", "description": "踢出时拒绝加群（kick）"},
				"confirm":    map[string]interface{}{"type": "boolean", "description": "高风险操作确认标记。执行 leave/kick/ban/unban/rename/mute-all/set-card/set-admin/set-title/recall/pin-msg/folder-create 时必须传 true"},
			},
		},
		NoMemory: true,
	}, p.handleGroupManage)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "friend_action", Description: "QQ好友管理：delete删除好友, block拉黑（删好友+从所有群踢出+拒绝加群）, approve-friend同意好友请求, reject-friend拒绝好友请求, list-friends列出好友。注意：涉及删除/拉黑的操作必须请示管理员确认后再执行，未经授权不可操作。",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command":  map[string]interface{}{"type": "string", "description": "操作: delete|block|approve-friend|reject-friend|list-friends"},
				"user_id":  map[string]interface{}{"type": "integer", "description": "目标QQ号"},
				"flag":     map[string]interface{}{"type": "string", "description": "好友请求flag（approve-friend/reject-friend需要）"},
				"remark":   map[string]interface{}{"type": "string", "description": "好友备注（approve-friend可选）"},
				"group_id": map[string]interface{}{"type": "integer", "description": "仅从指定群踢出（block配合）"},
				"confirm":  map[string]interface{}{"type": "boolean", "description": "高风险操作确认标记。执行 delete/block/approve-friend/reject-friend 时必须传 true"},
			},
		},
		NoMemory: true,
	}, p.handleFriendAction)

	// ---- 文件 ----
	p.regTool(s, sdk.ToolDef{
		Name: tp + "get_group_files", Description: "查询群文件列表、搜索文件、下载文件到本地。操作: list列出, search搜索, download下载",
		NoMemory: false,
		Cleaner:  cleaner,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"group_id":  map[string]interface{}{"type": "integer", "description": "群号"},
				"command":   map[string]interface{}{"type": "string", "description": "操作: list|search|download"},
				"folder_id": map[string]interface{}{"type": "string", "description": "文件夹ID（list指定文件夹）"},
				"keyword":   map[string]interface{}{"type": "string", "description": "搜索关键词（search）"},
				"file_id":   map[string]interface{}{"type": "string", "description": "文件ID（download）"},
				"filename":  map[string]interface{}{"type": "string", "description": "保存文件名（download可选）"},
			},
		},
	}, p.handleGetGroupFiles)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "download_file", Description: "从聊天记录下载文件到本地。file_id 从 qq_get_message/qq_get_history 的 files 字段获取。群文件建议提供 group_id，私聊文件建议提供 user_id 以提高成功率。异步下载，完成后推送通知。",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"file_id":  map[string]interface{}{"type": "string", "description": "文件 ID（从 qq_get_message 或 qq_get_history 的 files 字段获取）"},
				"url":      map[string]interface{}{"type": "string", "description": "文件下载 URL（可选，qq_get_message 返回的 url 字段）"},
				"group_id": map[string]interface{}{"type": "integer", "description": "群号（可选，群文件下载）"},
				"user_id":  map[string]interface{}{"type": "integer", "description": "私聊对象QQ号（可选，私聊文件下载）"},
				"filename": map[string]interface{}{"type": "string", "description": "保存文件名（可选，默认用原文件名）"},
			}, "required": []string{"file_id"},
		},
		NoMemory: true,
	}, p.handleDownloadFile)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "get_download_tasks", Description: "查看所有下载任务及状态（running/done/failed），包含文件名、保存路径、错误信息等",
		NoMemory: false,
		Cleaner:  cleaner,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{},
		},
	}, p.handleGetDownloadTasks)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "upload_group_file", Description: "上传文件到QQ群（通过base64编码发送，同时出现在群消息和群文件柜）",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"group_id": map[string]interface{}{"type": "integer", "description": "目标群号"},
				"file":     map[string]interface{}{"type": "string", "description": "本地文件路径"},
				"name":     map[string]interface{}{"type": "string", "description": "文件名（可选，默认取原文件名）"},
			}, "required": []string{"group_id", "file"},
		},
		NoMemory: true,
	}, p.handleUploadGroupFile)

	// ---- 文档/视频/网页工具 ----
	p.regTool(s, sdk.ToolDef{
		Name: tp + "read_document", Description: "读取文档内容文本。支持 PDF、DOCX、DOC、XLSX、XLS、PPTX、PPT、TXT、CSV、MD 格式。使用 libreoffice + pandoc 转换提取文本，返回前 20000 字符。适合处理用户发来的文档文件。",
		NoMemory: false,
		Cleaner:  cleaner,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"path": map[string]interface{}{"type": "string", "description": "文档文件路径（已保存到本地的文件路径）"},
			}, "required": []string{"path"},
		},
	}, p.handleReadDocument)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "video_download", Description: "下载视频到本地。支持 B站、YouTube 等主流视频网站（通过 yt-dlp）。先调用 info_only 查看视频信息，再下载。下载后文件保存在 agentfs 目录。",
		NoMemory: false,
		Cleaner:  cleaner,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"url":       map[string]interface{}{"type": "string", "description": "视频分享链接"},
				"info_only": map[string]interface{}{"type": "boolean", "description": "仅获取视频信息（标题、时长、清晰度列表），不下"},
			}, "required": []string{"url"},
		},
	}, p.handleVideoDownload)

	// ---- 附加 ----
	p.regTool(s, sdk.ToolDef{
		Name: tp + "set_status",
		Description: "设 QQ 在线状态（对外可见）。" +
			"可选：online/busy/leave/dnd/hidden/working/study/music（也收中文名）。" +
			"失败只记日志、不影响其他工具。",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"status": map[string]interface{}{
					"type":        "string",
					"description": "状态名：online|busy|leave|dnd|hidden|working|study|music，或中文（在线/忙碌/离开/请勿打扰/隐身/搬砖中/学习中/听歌中）",
				},
			}, "required": []string{"status"},
		},
		NoMemory: true,
	}, p.handleSetStatus)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "set_diy_status",
		Description: "设 QQ **自定义**在线状态（图标 + 文字）。" +
			"face_id 是 QQ 状态图标 ID（需自行提供，无内置图标表）；" +
			"wording 是显示文字（如「思考中…」）。失败只记日志。",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"face_id": map[string]interface{}{"type": "string", "description": "状态图标 ID（QQ 的 QSid）"},
				"wording": map[string]interface{}{"type": "string", "description": "状态文字，如「思考中…」"},
			}, "required": []string{"face_id", "wording"},
		},
		NoMemory: true,
	}, p.handleSetDiyStatus)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "send_like", Description: "给QQ好友点赞/戳一戳",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"user_id": map[string]interface{}{"type": "integer", "description": "目标QQ号"},
				"times":   map[string]interface{}{"type": "integer", "description": "点赞次数1-20，默认1"},
			}, "required": []string{"user_id"},
		},
		NoMemory: true,
	}, p.handleSendLike)

	// ★★ 每个 stage **只能注册一个 handler**（2026-10-18 踩到的坑）。
	//
	// 插件侧（工具链生成的 proc_main）把 handler 存在
	// `stageHandlers map[string]StageHandler`（stage 名为键）⇒
	// **同一 stage 注册两次，后者静默覆盖前者**。而之前写成：
	//
	//	s.RegisterStage(StageBeforeToolcall, p.beforeToolcall, Global)
	//	s.RegisterStage(StageBeforeToolcall, p.stageStatusWorking, Global)  ← 覆盖了上面
	//	s.RegisterStage(StageAfterOutput,    p.stageStatusOnline, Global)
	//	s.RegisterStage(StageAfterOutput,    p.afterOutputAuthContext, Global) ← 覆盖了上面
	//
	// 后果不只是状态不切换 —— **QQ 的权限门被静默关掉了**（那几版部署里
	// 任何人发消息都能调任何工具）。hmapdev 现在会在构建期对这类重复告警。
	//
	// 现在合并成一个 handler，每个 stage 一次注册。
	//
	// ## 状态映射（用户定义的语义，2026-10-18）
	//
	//	学习中  = agent 正在**等待 LLM**
	//	搬砖中  = 真正**执行 toolcall**
	//	在线    = **空闲**
	//
	// 对应到内核 stage 时序（task.go）：
	//
	//	pre_action  ──→ stepLLM() ──→ ★等 LLM★ ──→ post_action
	//	                                            ↓
	//	                         工具循环: before_toolcall → ★跑工具★ → after_toolcall
	//	                                            ↓
	//	                         再次 stepLLM（第二轮工具）或 before_output
	//
	// 所以：
	//	pre_action       → 学习中（即将调 LLM，接下来就是等）
	//	before_toolcall  → 搬砖中（即将/正在跑工具）
	//	after_toolcall   → 学习中（工具回来了，又要等 LLM 决定下一步）
	//	after_output     → 在线（一轮真正结束）
	//
	// ★ `before_output` **不**改状态：它是「LLM 已返回、即将输出」，
	// 既不是等 LLM 也不是跑工具；若在那里写“学习中”会让状态在收尾时
	// 继续挂着，看起来像永远在想。
	s.RegisterStage(sdk.StageOnInput, p.onInputAuthContext, sdk.StageScopeGlobal)
	s.RegisterStage(sdk.StagePreAction, p.stagePreAction, sdk.StageScopeGlobal)
	s.RegisterStage(sdk.StageBeforeToolcall, p.stageBeforeToolcall, sdk.StageScopeGlobal)
	s.RegisterStage(sdk.StageAfterToolcall, p.stageAfterToolcall, sdk.StageScopeGlobal)
	s.RegisterStage(sdk.StageBeforeOutput, p.stageBeforeOutput, sdk.StageScopeGlobal)
	s.RegisterStage(sdk.StageAfterOutput, p.stageAfterOutput, sdk.StageScopeGlobal)
	// before_toolcall 的 Response 只用于拒绝当前工具。下一次模型补全到达时必须清掉，
	// 否则内核会把它误当作“结束整个推理”的最终响应。
	s.RegisterStage(sdk.StagePostAction, p.clearDeniedResponse, sdk.StageScopeGlobal)

	// ---- HTTP server for NapCat webhook ----
	mux := http.NewServeMux()
	mux.HandleFunc("/", p.handleWebhook)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	})
	p.srv = &http.Server{Addr: p.listenAddr, Handler: mux}
	go func() {
		log.Printf("[qq] webhook %s napcat=%s", p.listenAddr, p.napcatURL)
		if err := p.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[qq] http: %v", err)
		}
	}()

	// ★ 用**真实计数**，不写死。
	//
	// 这里原本是硬编码的 `15` —— 后来工具加到 24 个，日志仍然报 15。
	// 后果：排查“qzone 工具装上没”时看日志说 15，误以为装的是旧版，
	// 白折腾了几轮。这类“日志说谎”比不日志更坏。
	log.Printf("[qq] plugin started: %s (%d tools)", p.name, p.toolCount)
	return nil
}

func (p *Plugin) Stop() error {
	// ★ 停机前把在线状态恢复为「在线」。
	//
	// 不恢复的话，若停机时正处于“搬砖中”，QQ 会一直挂着那个状态，
	// 而下一次自动切换只在开关打开时才生效 —— 用户会看到一个
	// **永久错位**的状态，且不知道是什么改的。
	//
	// 放在最前：后面的 flushAllBatches 可能耗时，先把对外观感收尾。
	p.restoreStatus()

	p.typingMu.Lock()
	for _, st := range p.typingMap {
		select {
		case <-st.stopCh:
		default:
			close(st.stopCh)
		}
	}
	p.typingMu.Unlock()
	// 停机前把未到点的合并批次立刻投出去，别把对方的消息吞掉。
	p.flushAllBatches()
	if p.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p.srv.Shutdown(ctx)
	}
	return nil
}

func (p *Plugin) regTool(s *sdk.PluginSDK, def sdk.ToolDef, handler sdk.ToolHandler) {
	s.RegisterTool(def.Name, def, handler)
	p.toolCount++
}

// ======== Bot Identity ========

func (p *Plugin) fetchBotInfo() {
	resp, err := p.rawNapcat("get_login_info", nil)
	if err != nil {
		log.Printf("[qq] fetch login info: %v", err)
		return
	}
	var info struct {
		Status string `json:"status"`
		Data   *struct {
			UserID   int64  `json:"user_id"`
			Nickname string `json:"nickname"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(resp), &info); err != nil {
		log.Printf("[qq] parse login info: %v", err)
		return
	}
	if info.Data != nil {
		p.botID = info.Data.UserID
		p.botNickname = info.Data.Nickname
		log.Printf("[qq] bot identity: %s (%d)", p.botNickname, p.botID)
	}
}

// rawNapcat sends a request to NapCat and returns raw JSON string.
func (p *Plugin) rawNapcat(action string, params map[string]interface{}) (string, error) {
	data, _ := json.Marshal(params)
	url := fmt.Sprintf("%s/%s", p.napcatURL, action)
	resp, err := p.httpClient.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("napcat %s: %w", action, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body), nil
}

// getSetting reads a setting from the SDK; returns fallback if unset or wrong type.
func getSetting[T string | int64 | float64](s sdk.SettingsAPI, key string, fallback T) T {
	v, err := s.Get(key)
	if err != nil || v == nil {
		return fallback
	}
	switch any(fallback).(type) {
	case string:
		if str, ok := v.(string); ok {
			return any(str).(T)
		}
	case int64:
		switch val := v.(type) {
		case int:
			return any(int64(val)).(T)
		case int64:
			return any(val).(T)
		case float64:
			return any(int64(val)).(T)
		case string:
			if n, err := strconv.ParseInt(val, 10, 64); err == nil {
				return any(n).(T)
			}
		}
	case float64:
		switch val := v.(type) {
		case float64:
			return any(val).(T)
		case string:
			if n, err := strconv.ParseFloat(val, 64); err == nil {
				return any(n).(T)
			}
		}
	}
	return fallback
}

func normalizePolicy(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "allowlist":
		return "allowlist"
	case "disabled":
		return "disabled"
	default:
		return "open"
	}
}

func parseIDSet(raw string) map[int64]struct{} {
	out := make(map[int64]struct{})
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if n, err := strconv.ParseInt(part, 10, 64); err == nil {
			out[n] = struct{}{}
		}
	}
	return out
}

func parseIDList(raw string) []int64 {
	var out []int64
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if n, err := strconv.ParseInt(part, 10, 64); err == nil && n > 0 {
			out = append(out, n)
			continue
		}
		// 兼容历史坏数据：科学计数法存库的值（如 2.198972886e+09）
		if f, err := strconv.ParseFloat(part, 64); err == nil && f > 0 && f == math.Trunc(f) {
			out = append(out, int64(f))
		}
	}
	return out
}

func (p *Plugin) isOwner(userID int64) bool {
	for _, id := range p.ownerIDs {
		if id == userID {
			return true
		}
	}
	return false
}

func parseToolAllowlist(raw string) []string {
	var patterns []string
	if json.Unmarshal([]byte(raw), &patterns) != nil {
		return nil
	}
	out := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		if pattern = strings.TrimSpace(pattern); pattern != "" {
			out = append(out, pattern)
		}
	}
	return out
}

func parseGroupToolAllowlists(raw string) map[int64][]string {
	var encoded map[string][]string
	if json.Unmarshal([]byte(raw), &encoded) != nil {
		return map[int64][]string{}
	}
	out := make(map[int64][]string, len(encoded))
	for key, patterns := range encoded {
		var groupID int64
		if key != "*" {
			parsed, err := strconv.ParseInt(strings.TrimSpace(key), 10, 64)
			if err != nil || parsed <= 0 {
				continue
			}
			groupID = parsed
		}
		clean := make([]string, 0, len(patterns))
		for _, pattern := range patterns {
			if pattern = strings.TrimSpace(pattern); pattern != "" {
				clean = append(clean, pattern)
			}
		}
		out[groupID] = clean
	}
	return out
}

func matchesToolAllowlist(name string, patterns []string) bool {
	for _, pattern := range patterns {
		if pattern == name {
			return true
		}
		if strings.HasSuffix(pattern, "*") && strings.HasPrefix(name, strings.TrimSuffix(pattern, "*")) {
			return true
		}
	}
	return false
}

// nonNegativeOrDefault 保留 0（表示“不限制”），仅把负数纠正为默认值。
func nonNegativeOrDefault(value, fallback int) int {
	if value < 0 {
		return fallback
	}
	return value
}

// isHardPrivateTool 是不可由群/私聊白名单覆盖的私人资源边界。
// output_send__qq 及 QQ 当前会话工具在参数级另行约束，不在此处按名称误杀。
// isHardPrivateTool 判断工具是否属于「硬性私人资源」。
//
// 判据来自配置 hard_private_tools（默认值与旧的写死清单等价）；条目支持
// 精确名与尾部 * 前缀。清空配置即不再做硬性拦截（完全交给 allowlist）。
func (p *Plugin) isHardPrivateTool(name string) bool {
	patterns := p.hardPrivateTools
	if !p.hardPrivateToolsSet {
		// 未初始化（单测/异常路径）按内置默认判：安全默认，不放开。
		patterns = defaultHardPrivateParsed
	}
	if len(patterns) == 0 {
		return false // 显式配置成 [] 才是"不拦"
	}
	return matchesToolAllowlist(name, patterns)
}

func argInt64(args map[string]interface{}, key string) (int64, bool) {
	value, exists := args[key]
	if !exists || value == nil {
		return 0, false
	}
	parsed, err := convInt64(value)
	return parsed, err == nil && parsed != 0
}

// argPeerID 解析「会话标识」参数（group_id / user_id），把 0 视为**未提供**。
//
// ## 为什么不能直接用 convInt64 的 err==nil 当「存在」（2026-10-19 实测故障）
//
// convInt64 对 0 是成功返回的，于是 `if gerr == nil` 会把「没传」和「传了 0」
// 混为一谈。真实后果：模型从 qq_get_message 的返回体里读到私聊消息的
// `group_id: 0`，原样回灌给 qq_get_history，代码便认为「调用方要查群 0」，
// 打 get_group_msg_history(group_id=0)，NapCat 回
// `retcode=200 消息undefined不存在`；而真正的私聊端点
// get_friend_msg_history **一次都没被调用**（内核日志 13 次失败全部如此）。
//
// 群号/QQ 号都是正整数，0 从来不是合法会话，所以这里必须把 0 判成缺失。
// 权限门用的 argInt64 早有 `parsed != 0` 检查 —— 本函数把同一个判据
// 补到业务分支上，避免同一份代码里「权限门严、取数松」的错配。
func argPeerID(args map[string]interface{}, key string) (int64, bool) {
	return argInt64(args, key)
}


// qqAuthExtraKey 是本轮（帧）QQ 身份挂在 StageContext.Extra 上的键。
//
// 身份必须**绑帧**，不能只存插件全局：
//   - 中断会抢占当前轮并把现场压栈（scheduler 的 suspendStack），中断轮收尾时
//     afterOutput 把插件全局身份清空；随后外层被恢复（resumeTask 复用同一帧、
//     不重跑 StageOnInput），若身份只存全局，恢复后的外层就是"无身份"——
//     beforeToolcall 会在 !auth.Active 处直接返回，权限门整体失效。
//   - 运行中到达的新消息会调 activateAuthContext 改写全局身份，把**正在跑的那一轮**
//     换成另一方的身份（换高=越权，换低=误拒）。
//
// 帧上的 Extra 随帧一起压栈/恢复，正好是"这一轮的身份"。
const qqAuthExtraKey = "qq_auth"

// authOnFrame 读取本帧绑定的身份；ok=false 表示本帧未绑定过 QQ 身份。
//
// 双形态兼容（proc 桥 JSON 往返约定）：
//   - 值形态（进程内 stage / 单测）：类型断言直接命中；
//   - map 形态（proc 插件：身份经共享段 JSON 往返）：按 json tag 重建。
func authOnFrame(ctx *sdk.StageContext) (qqAuthContext, bool) {
	if ctx == nil || ctx.Extra == nil {
		return qqAuthContext{}, false
	}
	switch v := ctx.Extra[qqAuthExtraKey].(type) {
	case qqAuthContext:
		return v, true
	case map[string]interface{}:
		return authFromMap(v)
	default:
		return qqAuthContext{}, false
	}
}

// bindAuthOnFrame 把身份绑到本帧上。调用方需持有 ctx 的写锁。
func bindAuthOnFrame(ctx *sdk.StageContext, auth qqAuthContext) {
	if ctx == nil {
		return
	}
	if ctx.Extra == nil {
		ctx.Extra = make(map[string]interface{})
	}
	ctx.Extra[qqAuthExtraKey] = auth
}

// activateAuthContext 只接收 OneBot 事件中的可信 ID。多个中断在同一推理轮合并时
// 采用最小权限合并，防止“非所有者请求 + 随后所有者消息”意外提升前一请求权限。
// message_id 映射供排队输入在 StageOnInput 精确恢复身份，不依赖昵称或用户正文。
func (p *Plugin) activateAuthContext(messageID, userID, groupID int64, isGroup bool) {
	p.authMu.Lock()
	defer p.authMu.Unlock()
	if p.authByMessageID == nil {
		p.authByMessageID = make(map[int64]qqAuthContext)
	}
	p.authGeneration++
	next := qqAuthContext{
		Active: true, Owner: p.isOwner(userID), MessageID: messageID, UserID: userID,
		GroupID: groupID, IsGroup: isGroup, Generation: p.authGeneration,
	}
	if messageID != 0 {
		p.authByMessageID[messageID] = next
		if len(p.authByMessageID) > 2048 {
			cutoff := p.authGeneration - 1024
			for id, auth := range p.authByMessageID {
				if auth.Generation < cutoff {
					delete(p.authByMessageID, id)
				}
			}
		}
	}
	if !p.auth.Active {
		p.auth = next
		return
	}
	if p.auth.UserID == userID && p.auth.GroupID == groupID && p.auth.IsGroup == isGroup {
		p.auth.Owner = p.auth.Owner && next.Owner
		p.auth.MessageID = next.MessageID
		p.auth.Generation = next.Generation
		return
	}
	// 不同可信来源被内核合并到同一推理时，只在双方都是所有者时保留完整权限。
	bothOwners := p.auth.Owner && next.Owner
	sameGroup := p.auth.IsGroup && next.IsGroup && p.auth.GroupID == groupID
	p.auth.Owner = bothOwners
	p.auth.MessageID = 0
	p.auth.UserID = 0
	p.auth.IsGroup = sameGroup
	if sameGroup {
		p.auth.GroupID = groupID
	} else {
		p.auth.GroupID = 0
	}
	p.auth.Generation = next.Generation
}

var qqMessageIDsRe = regexp.MustCompile(`message_id=(-?\d+(?:,-?\d+)*)`)

// messageIDsFromInput 取出一段输入里出现的全部 message_id。
//
// 合并中继的正文是 `(message_id=100,101,102)`：只取第一个会留下同批其余 id 永不清理；
// 身份表用 id 做键，泄漏的条目要等 generation 回收才会消失。
func messageIDsFromInput(raw string) []int64 {
	matches := qqMessageIDsRe.FindAllStringSubmatch(raw, -1)
	ids := make([]int64, 0, len(matches))
	for _, match := range matches {
		if len(match) != 2 {
			continue
		}
		for _, part := range strings.Split(match[1], ",") {
			id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
			if err != nil || id == 0 {
				continue
			}
			ids = append(ids, id)
		}
	}
	return ids
}

func (p *Plugin) onInputAuthContext(ctx *sdk.StageContext) error {
	ctx.RLock()
	source, _ := ctx.Extra["input_source"].(string)
	raw := ctx.RawMessage
	ctx.RUnlock()

	p.authMu.Lock()
	// 默认降权：QQ 来源却对不上可信事件时绝不复用上一条消息的身份。
	next := qqAuthContext{Active: source == p.name}
	ids := messageIDsFromInput(raw)
	if source == p.name && len(ids) > 0 {
		if auth, ok := p.authByMessageID[ids[0]]; ok {
			next = auth
			for _, id := range ids {
				delete(p.authByMessageID, id)
			}
		}
	}
	// p.auth 只作为"帧上没绑身份"时的兜底（单测/异常帧），权威副本在帧上。
	p.auth = next
	p.resetTurnGuardLocked()
	p.authMu.Unlock()

	ctx.Lock()
	bindAuthOnFrame(ctx, next)
	ctx.Unlock()
	return nil
}

func (p *Plugin) resetTurnGuardLocked() {
	p.lastDenial = ""
	p.denialLocked = false
	p.toolCallCount = 0
	p.outputCallCount = 0
	p.outputSignatures = make(map[string]int)
}

func (p *Plugin) afterOutputAuthContext(ctx *sdk.StageContext) error {
	p.authMu.Lock()
	p.auth = qqAuthContext{}
	p.resetTurnGuardLocked()
	p.authMu.Unlock()
	return nil
}

func (p *Plugin) currentToolAllowed(ctx *sdk.StageContext, name string) (bool, qqAuthContext) {
	// 身份以本帧为准（中断恢复后全局身份可能已属于别的轮）。
	auth, onFrame := authOnFrame(ctx)
	p.authMu.RLock()
	if !onFrame {
		auth = p.auth
	}
	var patterns []string
	if auth.Active && !auth.Owner {
		if auth.IsGroup {
			patterns, _ = p.groupToolAllowlists[auth.GroupID]
			if patterns == nil {
				patterns = p.groupToolAllowlists[0]
			}
		} else {
			patterns = p.privateToolAllowlist
		}
	}
	p.authMu.RUnlock()
	if !auth.Active || auth.Owner {
		return true, auth
	}
	return matchesToolAllowlist(name, patterns), auth
}

// extraKeyComeFrom 是内核写在帧上的「来源标识」键。
//
// 契约（见内核 internal/agent/core/comefrom.go）：
//
//	"user"     用户请求在边界上发起（默认语义，本插件的权限门只应管这类）
//	"internal" agent 体系内部发起（驻留子/转投/自主续跑）⇒ 不走用户权限模型
//	缺失        旧内核 ⇒ 按 "user" 处理，**行为必须与改动前逐字一致**
//
// 为什么用 Extra 而不是具名字段：内核用 Extra 的通用袋跨进程透传插件自建键
// （proc 桥的 fExtraBag），与既有 `input_source` / `qq_auth` 同一读法。
const extraKeyComeFrom = "comefrom"

// isInternalToolCall 报告本帧的工具调用是否由 agent 内部委派发起。
//
// ★ 保守默认：**只有明确读到 "internal" 才算内部**。
// 缺失、空串、类型不对、未知取值 —— 全部当作 "user"（旧行为）。
// 这个方向不能反：把用户请求误判成 internal 就是**权限门整体失效**，
// 那比"多拦一次内部调用"严重得多。
func isInternalToolCall(ctx *sdk.StageContext) bool {
	if ctx == nil || ctx.Extra == nil {
		return false
	}
	v, ok := ctx.Extra[extraKeyComeFrom]
	if !ok {
		return false
	}
	s, _ := v.(string)
	return s == "internal"
}

// isAtBot checks if the message contains an @-mention of the bot.
//
// ★ **@全体成员（qq="all"）不算 @bot**（2026-10-18 修正）。
//
// ## 为什么改（现场事实）
//
// 原实现把 `v == "all"` 也当命中：
//
//	if v == botIDStr || v == "all" { return true }
//
// 后果：群里任何人 `@全体成员` 都会拉起 bot。那不是“在叫 bot”，
// 而是“在叫所有人” —— 一个是**针对性请求**，一个是**广播**，
// 语义完全不同。
//
// 现场佐证：群友直接质问过“又没@你，你就这么确定是找你的？”。
//
// ## 为什么不能“宽松一点”
//
// 群里发言代表公开可见。误响应的代价不是“多回一句”，而是
// **在未被邀请的场合发言**。宁可漏响应（用户会再 @ 一次），
// 不能误响应。
//
// 注：如果将来确实要支持“@全体就响应”，应做成显式配置项
// （默认关闭），而不是藏在 @ 判定里默认生效。
func (p *Plugin) isAtBot(msg interface{}) bool {
	botIDStr := strconv.FormatInt(p.botID, 10)

	// ★ NapCat 的 `message` 字段有**两种形态**（2026-10-26 现场）：
	//   · 分段数组：[{type:"at",data:{qq:"1456128421"}}, …]   （message_post_format=array）
	//   · CQ 码文本："[CQ:at,qq=1456128421] 你说啥"           （message_post_format=string）
	//
	// 原实现只认数组 ⇒ 本机部署（string 形态）下**永远返回 false**，
	// 表现为"在群里 @ 她完全没反应"，而日志只打 not @bot，看不出是形态问题。
	if str, ok := msg.(string); ok {
		return strings.Contains(str, "[CQ:at,qq="+botIDStr+"]")
	}

	segments, ok := msg.([]interface{})
	if !ok {
		return false
	}
	for _, seg := range segments {
		s, ok := seg.(map[string]interface{})
		if !ok {
			continue
		}
		if s["type"] == "at" {
			if data, ok := s["data"].(map[string]interface{}); ok {
				if qq, ok := data["qq"]; ok {
					switch v := qq.(type) {
					case string:
						// "all"（@全体成员）**不**算命中，见上方注释。
						if v == botIDStr {
							return true
						}
					case float64:
						if int64(v) == p.botID {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// ======== Webhook ========

func (p *Plugin) isDMAllowed(userID int64) bool {
	switch p.dmPolicy {
	case "disabled":
		return false
	case "allowlist":
		_, ok := p.allowFrom[userID]
		return ok
	default:
		return true
	}
}

func (p *Plugin) isGroupAllowed(groupID int64) bool {
	switch p.groupPolicy {
	case "disabled":
		return false
	case "allowlist":
		_, ok := p.groupAllowFrom[groupID]
		return ok
	default:
		return true
	}
}

func (p *Plugin) setDenial(ctx *sdk.StageContext, message string) {
	ctx.Response = &message
	p.authMu.Lock()
	p.lastDenial = message
	p.authMu.Unlock()
}

func (p *Plugin) clearPreviousDenial(ctx *sdk.StageContext) {
	p.authMu.Lock()
	last := p.lastDenial
	p.lastDenial = ""
	p.authMu.Unlock()
	if last != "" && ctx.Response != nil && *ctx.Response == last {
		ctx.Response = nil
	}
}

func (p *Plugin) clearDeniedResponse(ctx *sdk.StageContext) error {
	ctx.Lock()
	defer ctx.Unlock()
	p.clearPreviousDenial(ctx)
	return nil
}

func (p *Plugin) beforeToolcall(ctx *sdk.StageContext) error {
	ctx.Lock()
	defer ctx.Unlock()
	p.clearPreviousDenial(ctx)
	if len(ctx.ToolCalls) == 0 {
		return nil
	}
	// ★ **内部委派的工具调用不走用户权限模型**（2026-10-16）。
	//
	// 内核在帧上写了 `comefrom`：
	//   "user"     = 用户请求在边界上发起（**本插件要管的就是这类**）
	//   "internal" = agent 体系内部发起（驻留子/转投/自主续跑）
	//   缺失       = 旧内核，按 "user" 处理（保持既有行为，勿改）
	//
	// ## 为何这是正确的（实测故障 + 语义）
	//
	// 现场（2026-10-16）：驻留子 `dev-br-jianf-NextAgent-fix_abs-path` 自报 running
	// 但**零产出**，它那一轮 8 个工具（cmd_run/files_ls/notify_parent/…）全被
	// 本门拦下。日志实证：每个工具调用前都有
	// `[proc] qq stage before_toolcall 改写了 1 个字段`（= 写入了拒绝响应）。
	//
	// 根因：驻留子的输入**继承自父**（转投刻意保留原事件，否则同步调用方永久
	// 挂起），于是 `input_source` 仍是 "qq" ⇒ 本门把驻留子当成"那位 QQ 用户
	// 在提请求" ⇒ 按用户白名单判 ⇒ 开发工具一条都不在白名单里 ⇒ 全拒。
	//
	// 但用户请求**已经过了这道门**（父按真实身份判定过）。驻留子做的是
	// **父自己的委派决定**；对已经准入的工作再查一次"用户白名单"，既拦掉合法
	// 工作、又零安全增益（同一次请求，父已判定）。
	//
	// 为何不"只跳白名单、仍查硬私有"：硬私有（calendar_/email_/memory_…）
	// 防的是用户偷看私人数据；驻留子是父的内部执行体，父对它本就有完整
	// 权限 —— 再查一遍等于"机器人自己派出去的活被自己拦住"，同样荒谬。
	//
	// 为何放在 `auth.Active` 判断**之前**：internal 与"非本插件来源"在语义上
	// 是同一类（都不是"一条 QQ 用户消息"），走同一条放行路径最不容易出错。
	if isInternalToolCall(ctx) {
		// ★ 内部委派**不得向群聊发送**（2026-10-18 现场事故）。
		//
		// ## 事故
		//
		// offload 子 agent 的「分诊汇报」把本应发给用户的进度播报
		// **发到了群里**（meta={"group_id": 979911915}）。
		//
		// 根因：子 agent 被授予全部输出通道（AllowedOutputs=nil，用户明确要求
		// “持有全部输出通道”），于是它能自己选目标 —— 而它只从上下文里看到
		// 一个群号，就选了群。
		//
		// ## 为什么不允许
		//
		// 群的发言代表公开可见。子 agent 看不到“这个群里刚刚是谁 @ 了我”、
		// 也分不清“这句是回给谁的”，它只有一句任务描述。
		// 让它自由向群发言 = 把私聊级别的判断拿到公开场合执行。
		//
		// 真需要群里回时，由**父**（有完整上下文与用户身份）来发 —— 父的
		// comefrom 是 user，不受这条约束。
		if tc := outputSendGroupTarget(ctx, p.name); tc != "" {
			msg := fmt.Sprintf("内部委派不得向群聊发送消息（目标群 %s）。"+
				"请改发私聊，或把结果返回给父由它决定", tc)
			p.setDenial(ctx, msg)
			return nil
		}
		return nil
	}
	tc := &ctx.ToolCalls[0]
	allowed, auth := p.currentToolAllowed(ctx, tc.Name)
	if !auth.Active {
		return nil
	}

	p.authMu.Lock()
	p.toolCallCount++
	toolCount := p.toolCallCount
	denialLocked := p.denialLocked
	if tc.Name == "output_send__"+p.name {
		p.outputCallCount++
		signatureBytes, _ := json.Marshal(tc.Arguments)
		signature := string(signatureBytes)
		p.outputSignatures[signature]++
		duplicateCount := p.outputSignatures[signature]
		distinctCount := len(p.outputSignatures)
		// 循环保险只拦“参数完全相同的重复调用”。参数不同的必需调用一律放行，
		// 否则多次 cmd_run / update_schedule / 多条不同消息都会被误杀。
		if p.maxDuplicateSend > 0 && duplicateCount > p.maxDuplicateSend {
			p.authMu.Unlock()
			msg := fmt.Sprintf("QQ 循环保险已阻止重复发送：本轮第 %d 次出现参数完全相同的消息；请勿重复发送同一内容", duplicateCount)
			p.setDenial(ctx, msg)
			return nil
		}
		if p.maxQQOutputCalls > 0 && distinctCount > p.maxQQOutputCalls {
			p.authMu.Unlock()
			msg := fmt.Sprintf("QQ 循环保险已阻止本次发送：单轮主动发送的不同消息数已达上限 %d（0=不限制，可在插件配置调整）", p.maxQQOutputCalls)
			p.setDenial(ctx, msg)
			return nil
		}
	}
	if p.maxQQToolCalls > 0 && toolCount > p.maxQQToolCalls {
		p.authMu.Unlock()
		msg := fmt.Sprintf("QQ 循环保险已阻止工具调用：单轮工具调用总数已达上限 %d（0=不限制，可在插件配置调整）", p.maxQQToolCalls)
		p.setDenial(ctx, msg)
		return nil
	}
	p.authMu.Unlock()

	if denialLocked && tc.Name != "output_send__"+p.name {
		msg := fmt.Sprintf("QQ 权限策略已锁止本轮后续工具 %s；仅允许发送一次权限说明", tc.Name)
		p.setDenial(ctx, msg)
		return nil
	}
	if !auth.Owner && p.isHardPrivateTool(tc.Name) {
		p.authMu.Lock()
		p.denialLocked = true
		p.authMu.Unlock()
		msg := fmt.Sprintf("QQ 权限策略拒绝私人资源工具 %s；该限制不能由群聊或私聊白名单覆盖，请不要重试", tc.Name)
		p.setDenial(ctx, msg)
		return nil
	}
	if !allowed {
		scope := "非所有者私聊"
		if auth.IsGroup {
			scope = fmt.Sprintf("群聊 %d", auth.GroupID)
		}
		// 措辞很重要：旧文案只说"白名单未包含"，模型转述成"我没有这个接口"
		// （2026-10-20 现场：群友要收藏表情，bot 回"没有添加表情的接口"）。
		// 工具是**存在但未对该来源开放**，必须让模型如实说"权限没开"。
		msg := fmt.Sprintf("QQ 权限策略拒绝工具 %s：%s 的工具白名单未包含它（工具本身存在，只是没对这个来源开放）。"+
			"请如实告诉用户「这个操作需要管理员在 QQ 插件设置里开放权限」，**不要声称自己没有这个工具**，也不要重试", tc.Name, scope)
		p.setDenial(ctx, msg)
		return nil
	}
	// ★ 已移除「只能读当前会话」的参数级限制（2026-10-29，用户裁定）。
	//
	// ## 改的是什么
	//
	// 原先 `sessionToolArgsAllowed` 会检查 qq_get_history / mark_read /
	// get_message / get_group_member_info / get_group_files 的参数里
	// 的 group_id / user_id 是否等于**本轮消息所在的那个会话**。
	//
	// ## 为何移除
	//
	// 用户明确：**允许访问所有会话**。
	//
	// 从架构上看它也放错了地方：这是「参数自证」——判据依赖
	// **本轮消息来自哪个会话** + 本次调用的 args，而档位（Profile）的
	// 判定接口是 `CheckTool(name)`，**看不到 args**。
	// 把它塞进档位等于把「一次请求的上下文」塞进「长期策略表」，
	// 语义不相容。所以它选择删掉而不是迁移。
	//
	// ## 保留的安全边界
	//
	// 以下仍然生效（不依赖本轮的会话身份）：
	//   · 档位：qq.guest 的 DenyTools 清单（私人资源工具）—— 内核执行
	//   · beforeToolcall：群/私聊工具白名单 currentToolAllowed
	//   · 循环保险：单轮工具调用次数 / 重复发送上限
	if tc.Name == p.name+"_group_manage" {
		cmd, _ := tc.Arguments["command"].(string)
		if requiresConfirmGroupCommand(cmd) {
			if ok, _ := tc.Arguments["confirm"].(bool); !ok {
				msg := fmt.Sprintf("QQ群管理命令 %s 属于高风险操作，必须显式传入 confirm=true 后才能执行", cmd)
				p.setDenial(ctx, msg)
				return nil
			}
		}
	}
	if tc.Name == p.name+"_friend_action" {
		cmd, _ := tc.Arguments["command"].(string)
		if requiresConfirmFriendCommand(cmd) {
			if ok, _ := tc.Arguments["confirm"].(bool); !ok {
				msg := fmt.Sprintf("QQ好友管理命令 %s 属于高风险操作，必须显式传入 confirm=true 后才能执行", cmd)
				p.setDenial(ctx, msg)
				return nil
			}
		}
	}
	return nil
}

func requiresConfirmGroupCommand(cmd string) bool {
	switch cmd {
	case "leave", "kick", "ban", "unban", "rename", "mute-all", "set-card", "set-admin", "set-title", "recall", "pin-msg", "folder-create":
		return true
	default:
		return false
	}
}

func requiresConfirmFriendCommand(cmd string) bool {
	switch cmd {
	case "delete", "block", "approve-friend", "reject-friend":
		return true
	default:
		return false
	}
}

// enqueueInterrupt 把一条已通过策略/@ 检查的消息并入待投批次，并重置 debounce 计时。
//
// batchWindow<=0 时退回逐条投递（合并前行为）。
func (p *Plugin) enqueueInterrupt(msgType string, userID, groupID, messageID int64, nickname, single string, owner, highRisk bool) {
	if p.sdk == nil && p.injectHook == nil {
		return
	}
	if p.batchWindow <= 0 {
		p.injectInterrupt(single, p.interruptLevel(owner), qqInjectPayload(userID, groupID, msgType == "group"), owner)
		return
	}
	key := qqBatchKey(msgType, groupID, userID)
	p.batchMu.Lock()
	if p.batches == nil {
		p.batches = make(map[string]*pendingBatch)
	}
	b := p.batches[key]
	if b == nil {
		b = &pendingBatch{key: key, first: time.Now()}
		p.batches[key] = b
	}
	b.isGroup = msgType == "group"
	b.userID, b.groupID, b.nickname = userID, groupID, nickname
	b.msgIDs = append(b.msgIDs, messageID)
	b.single = single
	b.owner = b.owner || owner
	b.highRisk = b.highRisk || highRisk
	// debounce：每来一条就推迟；但整体不超过 batchMax（否则持续刷屏会一直不投）。
	delay := p.batchWindow
	if p.batchMax > 0 {
		if remain := p.batchMax - time.Since(b.first); remain < delay {
			delay = remain
		}
	}
	if delay < 0 {
		delay = 0
	}
	if b.timer != nil {
		b.timer.Stop()
	}
	b.timer = time.AfterFunc(delay, func() { p.flushBatch(key) })
	p.batchMu.Unlock()
}

// interruptLevel 决定一条 QQ 消息的中断级别（2026-10-27 用户裁定改级）。
//
//   - Bot 所有者/管理员 → **L3**（需及时处理）
//   - 普通用户         → **L2**（一般提醒）
//
// 为什么不再用 L1：L1 是"完全可等"，且 L1 之间**同级不抢占**——
// 主人发的话会被路人闲聊按到达顺序排在后面，持续刷屏时甚至一直排队尾。
// QQ 是异步消息，但"主人叫它"必须比路人闲聊更早被接住。
//
// 为什么管理员只到 L3 而不是 L4：L4 是内核独占（panic/overflow/childfull），
// 插件声明 L4 会被 clampPluginLevel 夹回 L3 —— 声明 L4 是无效的，不如直接声明 L3。
func (p *Plugin) interruptLevel(owner bool) string {
	if owner {
		return sdk.PriorityL3
	}
	return sdk.PriorityL2
}

// ───────────────────── 安全策略：声明式（2026-10-29） ─────────────────────
//
// ## 改动前的形态（plan.md §六 记录的「反例」）
//
// 身份判定、用户白名单、硬私有工具、内部委派放行、群发限制
// **全在 beforeToolcall 一个 stage handler 里**。后果：
//
//	· 每个插件各写一套判定，拒绝文案与语义分散
//	· 插件崩了策略就没了（判定不在内核）
//	· 同一份"硬私有清单"在 QQ 配置里、内核里各有一份口径
//
// ## 改动后的分工
//
//	QQ（本文件）  ：声明档位 + 注入时按身份选档     ← 只声明
//	内核 security ：CheckTool / ActivateProfile      ← 只执行
//
// 档位**只能收窄**：内核判定顺序是「全局 deny → 档位 deny → 主体 deny
// → 全局 allow → 档位 allow」，全局黑名单永远先判，档位绕不过去。
//
// ## 为什么 owner 也有档位
//
// 声明 `qq.owner` 而非"不传档位"，是为了**让行为显式**：所有者档位不额外
// 限制（零 DenyTools），但它存在这件事本身可审计（日志里能看到本轮按哪档判定）。
const (
	// securityProfileOwner 是所有者档位：不额外收窄。
	securityProfileOwner = "qq.owner"
	// securityProfileGuest 是访客档位：叠加硬私有清单的收窄。
	securityProfileGuest = "qq.guest"
)

// securityProfileFor 按发送者身份返回本轮档位名。
//
// ★ 这是**唯一**需要 QQ 关心的策略决定：「这条消息是谁发的」。
// 其余的"哪些工具不许用"由内核按档位里声明的清单执行，
// 不再散落在本插件的 stage handler 里。
func (p *Plugin) securityProfileFor(owner bool) string {
	if owner {
		return securityProfileOwner
	}
	return securityProfileGuest
}

// declareSecurityProfiles 把两个档位登记到内核（Start 时调用一次）。
//
// ## 为什么访客档位用 DenyTools 而不是 AllowTools
//
// AllowTools 是"只有这些"的语义，而 QQ 的工具面会随插件安装变化
// （用户装了 bili/music/weather… 访客也该能用）。用 DenyTools 表达
// "禁止私人资源"更稳：新增的公开工具自动对访客开放，不会因为
// 忘了加白名单而静默不可用 —— 后者是更难发现的故障。
//
// 拒绝清单取自 `hard_private_tools` 配置（保持运维可改），
// 条目支持尾部 `*` 前缀。
func (p *Plugin) declareSecurityProfiles(s *sdk.PluginSDK) {
	sec := s.Security()
	if sec == nil {
		// 宿主未接声明面（旧内核/独立测试）：不失败。
		// 此时 beforeToolcall 的自判逻辑仍在（见下方"双轨过渡"说明），
		// 行为与改动前一致 —— 不会因为内核老就放开权限。
		log.Printf("[qq] security declaration API unavailable; keeping legacy in-plugin checks")
		return
	}

	deny := p.hardPrivateTools
	if !p.hardPrivateToolsSet {
		deny = defaultHardPrivateParsed
	}

	if err := sec.DeclareProfiles([]sdk.SecurityProfile{
		// 所有者：不额外限制（空 DenyTools = 本档位不收窄）。
		{Name: securityProfileOwner},
		// 访客：硬私有清单在这里由**内核**执行。
		{Name: securityProfileGuest, DenyTools: deny},
	}); err != nil {
		// 响亮但不断流：声明失败不该让插件起不来，但必须留痕 ——
		// 否则权限门会静默退回"仅靠 beforeToolcall 自判"。
		log.Printf("[qq] declare security profiles failed: %v", err)
		return
	}
	// 成功也留痕：否则运维无法区分「声明生效」与「代码根本没跑到这里」。
	// 这条日志是灰度期验证策略真的上线的唯一信号。
	log.Printf("[qq] security profiles declared: %s (unrestricted), %s (deny %d patterns)",
		securityProfileOwner, securityProfileGuest, len(deny))
}

// qqInjectPayload 构造注入的 payload 扩展：告诉内核“这一轮是哪个用户引起的”。
//
// ## 为什么需要它（2026-10-18）
//
// 「对方正在输入中」必须只在**自己引起的** before_output 里发，而且要知道
// 发给谁。但 `before_output` 的 stageCtx 是内核新建的（emitResponse），
// 与 `on_input` 的 stageCtx 不共享 Extra（后者是局部变量）。
//
// ## 为什么走 Payload 而不是另立一个字段
//
// `InputEvent.Payload` 本就是通道送入内核的请求体，身份信息本该在那里。
// 第一版我加了 `InjectOptions.Meta` —— 那是把同一件事做了两遍，
// 而且只服务注入路径。现在用 `InjectOptions.Payload`：
// 内核把**非保留键**原样透传到 stageCtx.Extra，HTTP/CLI/ACP 也能共用。
//
// ★ 键名选 `qq_uid` 而不是 `user_id`：后者太通用，将来内核若加同名字段
// 会撞车；加插件名前缀是低成本的隔离。
func qqInjectPayload(userID, groupID int64, isGroup bool) map[string]interface{} {
	if userID <= 0 || isGroup {
		// 群聊不发输入指示（QQ 的“正在输入”只在私聊标题栏有意义）。
		return nil
	}
	return map[string]interface{}{"qq_uid": userID}
}

// injectInterrupt 投递一条中断提示（NoMemory：HTTP 侧来的不是对话内容）。
// injectInterrupt 投递一条中断输入。
//
// ★ owner 参数决定**本轮的安全档位**（声明式策略，2026-10-29）。
// 非所有者带来的消息带 `qq.guest` 档位，内核据此收窄工具面；
// 所有者带 `qq.owner`（= 不额外限制）。
func (p *Plugin) injectInterrupt(text, level string, payload map[string]interface{}, owner bool) {
	if text == "" {
		return
	}
	if level == "" {
		level = sdk.PriorityL1
	}
	if p.injectHook != nil {
		p.injectHook(text, level)
		return
	}
	if p.sdk == nil {
		return
	}
	p.sdk.InjectInterruptTextOpts(p.name, p.name, text, sdk.InjectOptions{
		NoMemory: true,
		Priority: level,
		// 中断文本是路由/取正文的指令，不是对话内容，不据它召回。
		RecallPolicy: sdk.RecallPolicyNone,
		// ★ 策略**声明式**：按发送者身份选档位，由内核执行判定。
		//
		// 在此之前权限判定全在 beforeToolcall 里自己做（读配置、判身份、
		// 查白名单、查硬私有）—— 那是 plan.md §六 记录的反例：每个插件
		// 各写一套、拒绝语义散落、插件崩了策略就没了。
		//
		// 现在：QQ 只声明档位（Start 里的 DeclareProfiles），注入时带上
		// 档位名，判定交给内核 security.CheckTool。档位只能**收窄**，
		// 绕不过全局黑名单。
		SecurityProfile: p.securityProfileFor(owner),
		// 业务字段（身份）随 payload 走，内核原样透传到本轮 stageCtx.Extra。
		Payload: payload,
	})
}

// flushBatch 投递一批：n==1 沿用单条原文；n>1 生成「共几条」的合并中断。
func (p *Plugin) flushBatch(key string) {
	p.batchMu.Lock()
	b := p.batches[key]
	delete(p.batches, key)
	p.batchMu.Unlock()
	if b == nil {
		return
	}
	text := b.single
	if len(b.msgIDs) > 1 {
		text = p.buildBatchInterrupt(b)
	}
	// 一批里只要有一条来自 Bot 所有者，整批按 L2 投递（不因混入路人消息而降低）。
	p.injectInterrupt(text, p.interruptLevel(b.owner), qqInjectPayload(b.userID, b.groupID, b.isGroup), b.owner)
}

// flushAllBatches 停机前把未到点的批次立刻投出去（best effort）。
func (p *Plugin) flushAllBatches() {
	p.batchMu.Lock()
	keys := make([]string, 0, len(p.batches))
	for k := range p.batches {
		keys = append(keys, k)
	}
	p.batchMu.Unlock()
	for _, k := range keys {
		p.flushBatch(k)
	}
}

// buildBatchInterrupt 生成合并中断：说清「一共几条」「分别是哪些 message_id」，
// 并给出一次拿全上下文的建议（get_history），避免模型逐条 get_message。
func (p *Plugin) buildBatchInterrupt(b *pendingBatch) string {
	tp := p.name + "_"
	outputTool := "output_send__" + p.name
	n := len(b.msgIDs)
	ids := formatMsgIDs(b.msgIDs)
	var s string
	if b.isGroup {
		s = fmt.Sprintf("来自「%s」在群里短时间内连续发来 %d 条消息(message_id=%s)。建议先用%sget_history(group_id=%d, count=%d)一次拉取这几条上下文再统一回复；也可用%sget_message 取单条。用%s回复群聊",
			b.nickname, n, ids, tp, b.groupID, n+5, tp, outputTool)
	} else {
		s = fmt.Sprintf("来自「%s」的私聊短时间内连续发来 %d 条消息(message_id=%s, user_id=%d)。建议先用%sget_history(user_id=%d, count=%d)一次拉取这几条上下文再统一回复；也可用%sget_message 取单条。用%s回复对方",
			b.nickname, n, ids, b.userID, tp, b.userID, n+5, tp, outputTool)
	}
	if b.highRisk {
		s = "【⚠️ 高危信息，谨慎处理】" + s
	}
	if b.owner {
		s = "【重要！Bot 所有者消息】" + s
	}
	return s
}

// formatMsgIDs 把 message_id 列表压成一行；过多时截断，避免中断文字过长。
func formatMsgIDs(ids []int64) string {
	const capN = 12
	parts := make([]string, 0, len(ids)+1)
	for i, id := range ids {
		if i >= capN {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ",")
}

func (p *Plugin) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "", http.StatusMethodNotAllowed)
		return
	}
	if p.webhookToken != "" && !hmac.Equal([]byte(r.Header.Get("X-Webhook-Token")), []byte(p.webhookToken)) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var evt struct {
		PostType    string      `json:"post_type"`
		MessageType string      `json:"message_type,omitempty"`
		MessageID   int64       `json:"message_id"`
		UserID      int64       `json:"user_id,omitempty"`
		GroupID     int64       `json:"group_id,omitempty"`
		RawMessage  string      `json:"raw_message,omitempty"`
		Message     interface{} `json:"message,omitempty"`
		Time        int64       `json:"time"`
		Sender      *struct {
			Nickname string `json:"nickname"`
			Card     string `json:"card,omitempty"`
		} `json:"sender,omitempty"`
	}
	if json.Unmarshal(body, &evt) != nil || evt.PostType != "message" {
		w.WriteHeader(http.StatusOK)
		return
	}
	// 群消息到达即记录（用于排查 napcat→webhook 链路漏报/丢弃）
	if evt.MessageType == "group" {
		log.Printf("[qq] webhook recv group msg id=%d from=%d in=%d raw=%.100s",
			evt.MessageID, evt.UserID, evt.GroupID, evt.RawMessage)
	}

	rawCQ := evt.RawMessage
	text := rawCQ
	if text == "" {
		if s, ok := evt.Message.(string); ok {
			text = s
		}
	}
	nickname := ""
	if evt.Sender != nil {
		nickname = evt.Sender.Nickname
		if evt.Sender.Card != "" {
			nickname = evt.Sender.Card
		}
	}

	if evt.MessageType == "private" {
		if !p.isDMAllowed(evt.UserID) {
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	if evt.MessageType == "group" {
		if !p.isGroupAllowed(evt.GroupID) {
			log.Printf("[qq] group msg from %d rejected: policy=%s", evt.GroupID, p.groupPolicy)
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	// ---- 记录 msg_id→peer 映射与会话状态（不缓存正文，仅最新一条短摘要）----
	// 策略允许的消息（群/私聊、是否 @bot 均记），供 get_msg 兜底与 list_chats 使用；
	// @bot 与否只决定是否发中断，不影响记录——与真人客户端一致看到全部会话。
	{
		peerID, isGroup := evt.UserID, false
		if evt.MessageType == "group" {
			peerID, isGroup = evt.GroupID, true
		}
		sum := text
		runes := []rune(sum)
		if len(runes) > qqLastSumLen {
			sum = string(runes[:qqLastSumLen]) + "…"
		}
		if evt.Time == 0 {
			evt.Time = time.Now().Unix()
		}
		p.snapshotMsg(evt.MessageID, peerID, isGroup, evt.Time, nickname, sum)
	}

	// isAt 记录"这条消息是否 @ 了 bot"：后面判断"空正文能不能丢"要用
	//（纯 @ 是叫你一声，必须回；没 @ 且没正文才是真的可以不处理）。
	isAt := false
	if evt.MessageType == "group" {
		// 群消息必须 @ 机器人才响应
		if p.botID == 0 {
			log.Printf("[qq] bot ID unknown, rejecting group message from %d", evt.GroupID)
			w.WriteHeader(http.StatusOK)
			return
		}
		isAt = p.isAtBot(evt.Message)
		if !isAt {
			// @ 了机器人 → 100% 注入（确定性）；没 @ 时按概率。
			//   · group_require_at=false  → 总闸关掉，全部注入（概率不起作用）
			//   · group_reply_chance=N    → 未 @ 的消息以 N% 概率注入
			if p.groupRequireAtOff {
				// 总闸关掉：意图就是"全量"，不打日志免得刷屏
			} else if p.replyChance > 0 && rand.Intn(100) < p.replyChance {
				log.Printf("[qq] group msg from %d/%d injected by chance (chance=%d%%, raw=%.80s)",
					evt.GroupID, evt.UserID, p.replyChance, rawCQ)
			} else {
				// 诊断：@ 解析失败时打印 at 段原文与 botID，定位漏报问题
				log.Printf("[qq] group msg from %d/%d not @bot (chance=%d%%, botID=%d, raw=%.120s)",
					evt.GroupID, evt.UserID, p.replyChance, p.botID, rawCQ)
				w.WriteHeader(http.StatusOK)
				return
			}
		}
	}

	tp := p.name + "_"
	outputTool := "output_send__" + p.name
	var interrupt string
	if evt.MessageType == "group" {
		interrupt = fmt.Sprintf("来自「%s」在群「%s」的消息(message_id=%d)。先用%sget_message(message_id=%d)取正文；若取不到（消息已过期），改用%sget_history(group_id=%d)按会话拉取上下文，或用%slist_chats 查看未读会话。用%s回复群聊", nickname, "群聊", evt.MessageID, tp, evt.MessageID, tp, evt.GroupID, tp, outputTool)
	} else {
		interrupt = fmt.Sprintf("来自「%s」的私聊消息(message_id=%d, user_id=%d)。先用%sget_message(message_id=%d)取正文；若取不到（消息已过期），改用%sget_history(user_id=%d)按会话拉取上下文，或用%slist_chats 查看未读会话。用%s回复对方", nickname, evt.MessageID, evt.UserID, tp, evt.MessageID, tp, evt.UserID, tp, outputTool)
	}
	if p.isOwner(evt.UserID) {
		interrupt = "【重要！Bot 所有者消息】" + interrupt
	}
	// ★ 本轮行为提示（配置驱动，可留空）。
	//
	// 只加**这一轮确实成立**的事实，不写通用说教：
	//   · 群聊且有 group_require_at ⇒ 必须 @，否则对方收不到提醒
	//   · 有收藏表情 ⇒ 允许用 sticker（并说明一条一张、不混文字）
	// 未被 @ 却被概率抽中 = 这是**随机插话**，模型必须知道，
	// 否则会把别人的闲聊当成对它说的话而答非所问（2026-10-27）。
	byChance := evt.MessageType == "group" && !isAt && !p.groupRequireAtOff
	interrupt += p.turnHints(evt.MessageType, evt.GroupID, evt.UserID, byChance)

	if text != "" {
		text = stripCQRe.ReplaceAllString(text, "")
		text = strings.TrimSpace(text)
	}
	// ★ 纯 @ 消息（`[CQ:at,qq=<bot>]` 后面没有正文）**不能丢**（2026-10-26 现场）。
	//
	// 原实现在这里 `return`：群里 @ 她一下就触发这条早退，用户看到的是
	// "怎么 @ 她没反应"，而日志里连一条 not @bot 都没有 —— 因为 @ 已经通过了，
	// 是正文为空被静默丢弃。@ 一下本身就是"叫你一声"，必须回。
	if text == "" {
		// 走到这里的群消息要么 @ 了机器人（100% 处理），要么已被概率抽中
		//（上面的门已经放行）。仍在门外的（没 @ 且没抽中）更早就已经 return。
		if !isAt && !p.groupRequireAtOff && p.replyChance == 0 {
			w.WriteHeader(http.StatusOK)
			return
		}
		text = "（这是一条没有正文的消息（纯图片/表情/纯 @）—— 自己判断是否回应）"
	}
	highRisk := false
	if highRiskRe.MatchString(text) {
		highRisk = true
		interrupt = "【⚠️ 高危信息，谨慎处理】" + interrupt
	}

	if evt.MessageType == "group" && p.sdk != nil {
		rulesRaw := getSetting[string](p.sdk.Settings(), "forward_rules", "[]")
		var rules []ForwardRule
		if json.Unmarshal([]byte(rulesRaw), &rules) == nil {
			for _, rule := range rules {
				if evt.GroupID == rule.GroupID {
					mcMsg := fmt.Sprintf("%s 说 %s", nickname, text)
					go func(r ForwardRule, msg string) {
						defer func() { _ = recover() }()
						if err := rconSend(r.Host, r.Port, r.Password, "say "+msg); err != nil {
							log.Printf("[qq] rcon forward to %s:%d: %v", r.Host, r.Port, err)
						}
					}(rule, mcMsg)
				}
			}
		}
	}

	// 必须在注入前记录 OneBot 可信来源；权限判断绝不依赖昵称、正文或模型参数。
	p.activateAuthContext(evt.MessageID, evt.UserID, evt.GroupID, evt.MessageType == "group")

	// ★ 「对方正在输入中」**不**在这里发起。
	//
	// ## 旧写法错在哪（2026-10-18）
	//
	// 原本是 `if private { p.startTyping(evt.UserID) }` —— 消息**刚到**
	// 就挂上“正在输入”，但此时：
	//   · agent 还没接受这条消息（下面才 enqueueInterrupt）
	//   · 批量合并窗口内可能还要等一段
	//   · 权限门可能直接拒掉（那人就一直看着“正在输入”却等不到回复）
	// 而且 typingLoop 有 30 秒超时 —— agent 跑得慢就自己灭了，
	// 回复还在路上。
	//
	// 正确边界：`before_output` 开始、`after_output` 结束（见 status.go
	// 的 stageBeforeOutput / stageAfterOutput），且只处理 output_channel==qq
	// 且带本人身份的注入（`inject_meta`）。
	//
	// 这样做的前提是：身份要由注入方声明（InjectOptions.Meta），
	// 内核透传到本轮 stageCtx.Extra["inject_meta"]。
	p.enqueueInterrupt(evt.MessageType, evt.UserID, evt.GroupID, evt.MessageID, nickname, interrupt, p.isOwner(evt.UserID), highRisk)

	w.WriteHeader(http.StatusOK)
}

// ======== Tool Handlers ========

// getMsgFromHistoryByTime 按 (peer, isGroup, targetTime) 从 NapCat 拉最近历史，返回距 targetTime 最近的完整消息。
func (p *Plugin) getMsgFromHistoryByTime(peerID int64, isGroup bool, targetTime int64) (map[string]interface{}, bool) {
	ep := "get_friend_msg_history"
	params := map[string]interface{}{"user_id": peerID, "count": 50}
	if isGroup {
		ep = "get_group_msg_history"
		params = map[string]interface{}{"group_id": peerID, "count": 50}
	}
	raw, err := p.napcat(ep, params)
	if err != nil {
		return nil, false
	}
	rawStr, _ := rawString(raw)
	if rawStr == "" {
		return nil, false
	}
	var resp struct {
		Data *struct {
			Messages []interface{} `json:"messages"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(rawStr), &resp) != nil || resp.Data == nil {
		return nil, false
	}
	var best map[string]interface{}
	bestAbs := int64(-1)
	for _, m := range resp.Data.Messages {
		mm, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		mt, _ := mm["time"].(float64)
		t := int64(mt)
		if t == 0 {
			continue
		}
		abs := t - targetTime
		if abs < 0 {
			abs = -abs
		}
		if bestAbs < 0 || abs < bestAbs {
			bestAbs = abs
			best = mm
		}
	}
	if best == nil {
		return nil, false
	}
	return best, true
}

// msgToGetMsgResult 把一条 NapCat 历史消息对象转成与 get_msg 同构的结果（历史包装语义）。
func msgToGetMsgResult(msg map[string]interface{}) map[string]interface{} {
	nickname := ""
	if s, ok := msg["sender"].(map[string]interface{}); ok {
		if n, _ := s["nickname"].(string); n != "" {
			nickname = n
		}
		if c, _ := s["card"].(string); c != "" {
			nickname = c
		}
	}
	rawText, _ := msg["raw_message"].(string)
	content := rawText
	if content == "" {
		if segs, ok := msg["message"].([]interface{}); ok {
			var parts []string
			for _, seg := range segs {
				segMap, _ := seg.(map[string]interface{})
				if segMap == nil {
					continue
				}
				typ, _ := segMap["type"].(string)
				segData, _ := segMap["data"].(map[string]interface{})
				if segData == nil {
					continue
				}
				switch typ {
				case "text":
					if t, _ := segData["text"].(string); t != "" {
						parts = append(parts, t)
					}
				case "image":
					parts = append(parts, "[图片]")
				case "file":
					if n, _ := segData["name"].(string); n != "" {
						parts = append(parts, "[文件:"+n+"]")
					}
				default:
					if typ != "" {
						parts = append(parts, "["+typ+"]")
					}
				}
			}
			if len(parts) > 0 {
				content = strings.Join(parts, " ")
			}
		}
	}
	mid, _ := msg["message_id"].(float64)
	uid, _ := msg["user_id"].(float64)
	gid, _ := msg["group_id"].(float64)
	mt, _ := msg["time"].(float64)
	mtType, _ := msg["message_type"].(string)
	loc := "私聊"
	if mtType == "group" || gid > 0 {
		loc = "群聊"
	}
	// ★ 私聊不要输出 group_id。
	//
	// 现场故障（2026-10-19）：这里无条件写 "group_id": 0，模型取回私聊正文后
	// 看到 group_id 字段就以为"这个会话的群号是 0"，把它原样回灌给
	// qq_get_history，于是后者打 get_group_msg_history(group_id=0) 拿到
	// 「消息undefined不存在」，13 次调用全灭（get_friend_msg_history 0 次）。
	//
	// 光在下游把 0 判成缺失只能止血，模型仍会一次次误用。**不在返回体里给它
	// 这个字段**，才是从源头断掉这条错误链：群聊才带 group_id，私聊只带 user_id。
	result := map[string]interface{}{
		"content":      content,
		"message_id":   int64(mid),
		"user_id":      int64(uid),
		"nickname":     nickname,
		"message_type": mtType,
		"type":         loc,
		"time":         time.Unix(int64(mt), 0).Format("2006-01-02 15:04:05"),
	}
	if gid > 0 {
		result["group_id"] = int64(gid)
	}
	return result
}

func (p *Plugin) handleGetMessage(args map[string]interface{}) (interface{}, error) {
	msgID, err := convInt64(args["message_id"])
	if err != nil {
		return map[string]interface{}{
			"content":   "需要提供 message_id 参数",
			"not_found": true,
		}, nil
	}

	// 本地 msg_id→peer 映射命中且 <7 天 → 用 get_history 语义兜底（NapCat 临时短号失效也不怕）
	if peerID, isGroup, t, ok := p.lookupMsgRef(msgID); ok {
		if m, found := p.getMsgFromHistoryByTime(peerID, isGroup, t); found {
			// 找到同会话、时间最接近的消息，包装为 get_msg 同构返回
			res := msgToGetMsgResult(m)
			res["resolved_via"] = "history" // 标明由历史查询兜底
			return res, nil
		}
		// 历史窗口内没找到（消息可能被裁剪/更早），回退 NapCat 原查询
	}
	return p.getMsgFromNapcat(msgID)
}

func (p *Plugin) getMsgFromNapcat(msgID int64) (interface{}, error) {
	raw, err := p.napcat("get_msg", map[string]interface{}{"message_id": msgID})
	if err != nil {
		return map[string]interface{}{
			"content":    fmt.Sprintf("查询 NapCat 失败: %s。该 message_id 可能已过期，请改用 qq_get_history 按会话拉取最近消息（或用 qq_list_chats 看未读会话）", err),
			"message_id": msgID,
			"not_found":  true,
		}, nil
	}
	rawStr, _ := rawString(raw)
	if rawStr == "" {
		return map[string]interface{}{
			"content":    "NapCat 返回空响应",
			"message_id": msgID,
			"not_found":  true,
		}, nil
	}

	var resp struct {
		Data *struct {
			MessageID   int64       `json:"message_id"`
			UserID      int64       `json:"user_id"`
			GroupID     int64       `json:"group_id"`
			Time        int64       `json:"time"`
			MessageType string      `json:"message_type"`
			RawMessage  string      `json:"raw_message"`
			Message     interface{} `json:"message"`
			Sender      *struct {
				Nickname string `json:"nickname"`
				Card     string `json:"card"`
			} `json:"sender"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(rawStr), &resp); err != nil || resp.Data == nil {
		return map[string]interface{}{
			"content":    "解析 NapCat 响应失败（消息可能已过期）。请改用 qq_get_history 按会话拉取最近消息，或用 qq_list_chats 查看未读会话",
			"message_id": msgID,
			"not_found":  true,
		}, nil
	}

	d := resp.Data
	nickname := ""
	if d.Sender != nil {
		nickname = d.Sender.Nickname
		if d.Sender.Card != "" {
			nickname = d.Sender.Card
		}
	}

	content := d.RawMessage
	var files []map[string]interface{}
	var replyToID int64
	var hasImage, hasFile bool
	if segs, ok := d.Message.([]interface{}); ok {
		var parts []string
		for _, seg := range segs {
			segMap, _ := seg.(map[string]interface{})
			if segMap == nil {
				continue
			}
			typ, _ := segMap["type"].(string)
			segData, _ := segMap["data"].(map[string]interface{})
			if segData == nil {
				continue
			}
			switch typ {
			case "text":
				if t, _ := segData["text"].(string); t != "" {
					parts = append(parts, t)
				}
			case "at":
				if qq, _ := segData["qq"].(string); qq != "" {
					parts = append(parts, "@"+qq)
				}
			case "reply":
				if idStr, ok := segData["id"].(string); ok {
					replyToID, _ = strconv.ParseInt(idStr, 10, 64)
				} else if id, ok := segData["id"].(float64); ok {
					replyToID = int64(id)
				}
				parts = append(parts, fmt.Sprintf("[回复id=%d]", replyToID))
			case "file":
				hasFile = true
				fid, _ := segData["file_id"].(string)
				if fid == "" {
					fid, _ = segData["file"].(string)
				}
				name, _ := segData["name"].(string)
				if name == "" {
					name, _ = segData["file"].(string)
				}
				fileURL, _ := segData["url"].(string)
				if fid != "" {
					entry := map[string]interface{}{
						"file_id": fid,
						"name":    name,
					}
					if fileURL != "" {
						entry["url"] = fileURL
					}
					files = append(files, entry)
				}
			case "image":
				hasImage = true
			}
		}
		if len(parts) > 0 {
			content = strings.Join(parts, " ")
		}
	}

	loc := "私聊"
	if d.MessageType == "group" {
		loc = "群聊"
	}
	result := map[string]interface{}{
		"content":      content,
		"message_id":   d.MessageID,
		"user_id":      d.UserID,
		"group_id":     d.GroupID,
		"nickname":     nickname,
		"message_type": d.MessageType,
		"type":         loc,
		"time":         time.Unix(d.Time, 0).Format("2006-01-02 15:04:05"),
	}
	if replyToID > 0 {
		result["reply_to"] = map[string]interface{}{"message_id": replyToID}
	}
	if hasImage {
		result["has_image"] = true
	}
	if hasFile {
		result["has_file"] = true
	}
	if len(files) > 0 {
		result["files"] = files
	}

	// 异步标记已读
	go func() {
		defer func() { _ = recover() }() // 后台任务不允许 panic 冒泡带崩进程
		if d.MessageType == "group" && d.GroupID > 0 {
			p.napcat("mark_group_msg_as_read", map[string]interface{}{"group_id": d.GroupID})
		} else if d.UserID > 0 {
			p.napcat("mark_private_msg_as_read", map[string]interface{}{"user_id": d.UserID})
		}
	}()

	return result, nil
}

// handleChannelOutput — output_send(channel="qq") 的处理器
// args 包含 payload, type, (可选 meta)
// 成功时只返回极简标记，不回传 NapCat 完整响应——避免"已发送"类富回执喂给模型
// 造成"看到成功→继续发下一条"的回声循环（issue: output loop echo）。
func (p *Plugin) handleChannelOutput(args map[string]interface{}) (interface{}, error) {
	payload, _ := args["payload"].(string)
	rawType, _ := args["type"].(string)
	meta, _ := args["meta"].(string)
	log.Printf("[qq] handleChannelOutput type=%s payload_len=%d meta=%s", rawType, len(payload), meta)
	if rawType == "" {
		return nil, fmt.Errorf("type 参数不能为空")
	}

	// 解析 meta
	var groupID, userID int64
	var replyTo int64
	// stickerIndex 让 type=sticker 时可直接按收藏序号选表情 —— 这项能力原先只存在于
	// 独立的 qq_send_sticker 工具里，但"发一条消息"本来就是输出门的语义，
	// 不该另开一个工具（用户 2026-10-22 拍板收回独立 tool）。
	var stickerIndex int64
	if meta != "" {
		var m struct {
			GroupID      int64 `json:"group_id"`
			UserID       int64 `json:"user_id"`
			ReplyTo      int64 `json:"reply_to"`
			StickerIndex int64 `json:"sticker_index"`
		}
		if err := json.Unmarshal([]byte(meta), &m); err == nil {
			groupID = m.GroupID
			userID = m.UserID
			replyTo = m.ReplyTo
			stickerIndex = m.StickerIndex
		}
	}
	// payload 通常不能为空，但 type=sticker 且 meta 指定了 sticker_index 时
	// 真实 URL 由插件从收藏里取，payload 允许留空。
	if payload == "" && !(rawType == "sticker" && stickerIndex > 0) {
		return nil, fmt.Errorf("payload 不能为空")
	}
	if groupID == 0 && userID == 0 {
		return nil, fmt.Errorf("meta 中需要 group_id 或 user_id 字段。用 output_send__qq_help 查看格式说明")
	}

	// ★ 「对方正在输入中」**不**在这里停。
	//
	// ## 旧写法错在哪（2026-10-18）
	//
	// 原本是 `if userID > 0 { p.stopTyping(userID) }` —— 它是挂在
	// **发送消息**上的，而发送消息可能是：
	//   · 群聊消息（与死个私聊用户无关）
	//   · 给**另一个人**的私聊消息
	//   · 同一个人但只是中途的一句话，而推理还在继续
	//
	// 后果：正在跟 A 私聊时 agent 发了条群消息，A 的输入指示被莫名关掉
	// —— 他以为 bot 跑了。
	//
	// 正确边界：`after_output`（一轮真正结束），见 status.go。
	var sendErr error
	switch rawType {
	case "text":
		text := normalizeIMText(p.sensitiveFilter(payload))
		message := interface{}(text)
		if groupID != 0 && userID != 0 {
			message = messageWithMention(userID, map[string]interface{}{"type": "text", "data": map[string]interface{}{"text": text}})
		}
		msg := map[string]interface{}{"message": message}
		if groupID != 0 {
			msg["group_id"] = groupID
		} else {
			msg["user_id"] = userID
		}
		if replyTo > 0 {
			msg["reply_to"] = replyTo
		}
		if groupID != 0 {
			_, sendErr = p.napcat("send_group_msg", msg)
		} else {
			_, sendErr = p.napcat("send_private_msg", msg)
		}

	case "voice", "audio":
		// ★ payload 可以是**文本**（走插件 TTS），也可以是**已存在的音频文件**
		// （本地路径或 file:// URL）。
		//
		// 为什么要支持后者（2026-10-26 用户需求）：外部 TTS/换声/唱歌服务
		// （如肥鱼声库那种 HTTP 服务）产出的音频文件，本该直接发出去；
		// 而此前 payload 一律被当成"待合成文本"，等于逼着每个音频都再过
		// 一遍本插件内置的 TTS。
		//
		// 判定顺序：file:// 前缀 → 本地绝对路径存在 → 其余按文本合成。
		// 不做"看起来像路径"的猜测：只有**确实存在**的文件才当文件。
		audioFile := ""
		if strings.HasPrefix(payload, "file://") {
			audioFile = strings.TrimPrefix(payload, "file://")
		} else if strings.HasPrefix(payload, "/") {
			if _, statErr := os.Stat(payload); statErr == nil {
				audioFile = payload
			}
		}
		var err error
		ttsTemp := false
		if audioFile == "" {
			text := p.sensitiveFilter(payload)
			audioFile, err = p.ttsToFile(text)
			if err != nil {
				return nil, fmt.Errorf("语音生成失败: %w", err)
			}
			ttsTemp = true
		}
		os.MkdirAll(p.remoteDir, 0755)
		dest := filepath.Join(p.remoteDir, filepath.Base(audioFile))
		data, err := os.ReadFile(audioFile)
		if err != nil {
			return nil, fmt.Errorf("读取音频文件失败: %w", err)
		}
		if err := os.WriteFile(dest, data, 0644); err != nil {
			return nil, fmt.Errorf("写入共享目录失败: %w", err)
		}
		// 只删**我们合成的临时文件**：调用方给的音频文件属于它自己的资产，
		// 删掉等于把用户的素材销毁。
		if ttsTemp {
			os.Remove(audioFile)
		}
		uri := fmt.Sprintf("file:///app/files/%s", filepath.Base(dest))
		cqMsg := fmt.Sprintf("[CQ:record,file=%s]", uri)
		message := interface{}(cqMsg)
		if groupID != 0 && userID != 0 {
			message = messageWithMention(userID, map[string]interface{}{"type": "record", "data": map[string]interface{}{"file": uri}})
		}
		msg := map[string]interface{}{"message": message}
		if groupID != 0 {
			msg["group_id"] = groupID
		} else {
			msg["user_id"] = userID
		}
		if groupID != 0 {
			_, sendErr = p.napcat("send_group_msg", msg)
		} else {
			_, sendErr = p.napcat("send_private_msg", msg)
		}

	case "image", "file", "sticker":
		// sticker + meta.sticker_index ⇒ 从收藏表情里按序号取 URL
		//
		// ★ 给了 sticker_index 就**忽略 payload**（2026-10-26 现场）：
		// 模型常把 payload 填成占位符（"[sticker]" / "sticker"），于是走到
		// 下面的"按文件路径处理"分支 ⇒ 报 `sticker 文件不存在: [sticker]`，
		// 用户看到的是"她发不出表情包"。
		// 既然序号已经指明了要发哪张收藏表情，payload 里的文字就是噪声。
		if rawType == "sticker" && stickerIndex > 0 {
			payload = ""
		}
		if rawType == "sticker" && stickerIndex > 0 && payload == "" {
			faces, ferr := p.fetchCustomFaces(int(stickerIndex))
			if ferr != nil {
				return nil, ferr
			}
			if int(stickerIndex) > len(faces) {
				return nil, fmt.Errorf("表情序号 %d 超出范围（当前收藏 %d 个）", stickerIndex, len(faces))
			}
			payload = faces[stickerIndex-1]
		}
		uri := payload
		if !strings.HasPrefix(payload, "http://") && !strings.HasPrefix(payload, "https://") &&
			!strings.HasPrefix(payload, "file://") {
			if _, err := os.Stat(payload); err != nil {
				return nil, fmt.Errorf("%s 文件不存在: %s", rawType, payload)
			}
			os.MkdirAll(p.remoteDir, 0755)
			dest := filepath.Join(p.remoteDir, sanitizeFilename(filepath.Base(payload)))
			data, err := os.ReadFile(payload)
			if err != nil {
				return nil, fmt.Errorf("读取文件失败: %w", err)
			}
			if err := os.WriteFile(dest, data, 0644); err != nil {
				return nil, fmt.Errorf("写入共享目录失败: %w", err)
			}
			uri = "file:///app/files/" + filepath.Base(dest)
		}
		cqTag := "file"
		// sticker 与 image 同为图片段：QQ 按 URL 渲染成表情图（见 qq_list_stickers 注释）
		if rawType == "image" || rawType == "sticker" {
			cqTag = "image"
		}
		message := interface{}(fmt.Sprintf("[CQ:%s,file=%s]", cqTag, uri))
		if groupID != 0 && userID != 0 {
			message = messageWithMention(userID, map[string]interface{}{"type": cqTag, "data": map[string]interface{}{"file": uri}})
		}
		msg := map[string]interface{}{"message": message}
		if groupID != 0 {
			msg["group_id"] = groupID
		} else {
			msg["user_id"] = userID
		}
		if groupID != 0 {
			_, sendErr = p.napcat("send_group_msg", msg)
		} else {
			_, sendErr = p.napcat("send_private_msg", msg)
		}

	default:
		return nil, fmt.Errorf("不支持的 type: %s（枚举值: text/voice/image/sticker/file）", rawType)
	}

	if sendErr != nil {
		return nil, sendErr
	}
	// 成功：返回极简标记。不再回传 NapCat 原始响应（含 message_id 等）给模型，
	// 避免模型把"发送成功"当成"上一步完成，继续下一步"的信号驱动循环。
	return "ok", nil
}

func (p *Plugin) buildOutputHelp() string {
	return `【参数】
payload — 消息载荷。type=text时直接填文字，type=voice时填文字（自动转语音），type=image/file时填URL
meta    — JSON 元数据，含 group_id（群聊）和/或 user_id（私聊或群内@），可选 reply_to
type    — text / voice / image / file

【示例】
群聊文字：output_send__qq(payload="你好", meta="{\"group_id\":123456789}", type="text")
私聊语音：output_send__qq(payload="你好", meta="{\"user_id\":123456789}", type="voice")
群内@用户：output_send__qq(payload="你好", meta="{\"group_id\":123456789,\"user_id\":987654321}", type="text")
发送图片：output_send__qq(payload="https://example.com/img.jpg", meta="{\"group_id\":123456789}", type="image")

【注意】
- group_id 与 user_id 同时存在时始终发送到 group_id，并在消息头 @user_id
- type=text 时 payload 直接是文字，无需 JSON 包裹
- type=voice 时 payload 是文字内容，自动转语音发送
- type=image/file 时 payload 是 URL 或路径`
}

// ttsToFile 用 edge-tts 将文本转为音频文件，返回临时文件路径
func (p *Plugin) ttsToFile(text string) (string, error) {
	// 清理文本中的特殊字符
	clean := strings.Map(func(r rune) rune {
		if r == '"' || r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, text)
	clean = strings.TrimSpace(clean)
	if clean == "" {
		clean = " "
	}

	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("qq_tts_%d.mp3", time.Now().UnixNano()))
	cmd := exec.Command("edge-tts",
		"--voice", "zh-CN-XiaoxiaoNeural",
		"--text", clean,
		"--write-media", tmpFile,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("edge-tts: %w\nstderr: %s", err, stderr.String())
	}
	if _, err := os.Stat(tmpFile); os.IsNotExist(err) {
		return "", fmt.Errorf("edge-tts 未生成输出文件")
	}
	return tmpFile, nil
}

// normalizeIMText 把模型输出整理成**聊天消息**的样子。
//
// 为什么需要：模型习惯用空行分段（"\n\n"），在 Markdown 里是段落，在 QQ
// 气泡里就是突兀的空行 —— 用户实测反馈「发qq消息还是有空行」。提示词里已经
// 要求「一条消息内不空行、要分段就分条发」，但模型遵循度不是 100%，所以在
// 通道出口再做一次确定性收敛：
//
//	· 连续空行（2 个以上换行）压成一个换行 —— 保留分行，去掉空行
//	· 去掉首尾空白（消息气泡不该以空行开头/结尾）
//	· 三个以上连续换行同样压成一个
//
// 只在 text 类型上做：image/file/voice 的 payload 是 URL/路径，不能动。
func normalizeIMText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	for strings.Contains(s, "\n\n") {
		s = strings.ReplaceAll(s, "\n\n", "\n")
	}
	return s
}

// stickerDesc 是给收藏表情记录的内容与情绪描述。
type stickerDesc struct {
	Desc string `json:"desc"` // 画面内容：画的是什么、什么梗
	Mood string `json:"mood"` // 情绪/场合：开心、无语、嘲讽、撒娇…
}

func (p *Plugin) loadStickerDescs() {
	p.stickerMu.Lock()
	defer p.stickerMu.Unlock()
	p.stickerDescs = map[string]stickerDesc{}
	if p.sdk == nil {
		return
	}
	raw, err := p.sdk.Settings().Get("sticker_descs")
	if err != nil {
		return
	}
	str, _ := raw.(string)
	if strings.TrimSpace(str) == "" {
		return
	}
	var m map[string]stickerDesc
	if json.Unmarshal([]byte(str), &m) == nil && m != nil {
		p.stickerDescs = m
	}
}

// saveStickerDescs 把缓存写回插件配置（json 字符串）。
func (p *Plugin) saveStickerDescs() error {
	p.stickerMu.RLock()
	data, err := json.Marshal(p.stickerDescs)
	p.stickerMu.RUnlock()
	if err != nil {
		return err
	}
	if p.sdk == nil {
		return nil
	}
	return p.sdk.Settings().Set("sticker_descs", string(data))
}

// stickerDescOf 读单个表情的描述（无描述返回零值）。
func (p *Plugin) stickerDescOf(url string) stickerDesc {
	p.stickerMu.RLock()
	defer p.stickerMu.RUnlock()
	return p.stickerDescs[url]
}

// maxStickers 是单次拉取上限（NapCat 侧过多无意义，展示也放不下）。
const maxStickers = 100

// fetchCustomFaces 调 fetch_custom_face 并解析出表情 URL 列表。
func (p *Plugin) fetchCustomFaces(count int) ([]string, error) {
	if count <= 0 {
		count = 20
	}
	if count > maxStickers {
		count = maxStickers
	}
	raw, err := p.napcat("fetch_custom_face", map[string]interface{}{"count": count})
	if err != nil {
		return nil, err
	}
	// napcat() 原样透传响应体（json.RawMessage），这里按 OneBot 信封解析 data。
	// 兼容 RawMessage / []byte / 已解析对象三种形态，避免调用方假设返回类型。
	var rawData []byte
	switch v := raw.(type) {
	case json.RawMessage:
		rawData = v
	case []byte:
		rawData = v
	default:
		rawData, _ = json.Marshal(v)
	}
	var resp struct {
		Data []string `json:"data"`
	}
	if err := json.Unmarshal(rawData, &resp); err != nil {
		return nil, fmt.Errorf("解析表情列表失败: %w", err)
	}
	return resp.Data, nil
}

// handleListStickers 列出收藏表情。
func (p *Plugin) handleListStickers(args map[string]interface{}) (interface{}, error) {
	count := 20
	if c, err := convInt64(args["count"]); err == nil && c > 0 {
		count = int(c)
	}
	faces, err := p.fetchCustomFaces(count)
	if err != nil {
		return nil, err
	}
	stickers := make([]map[string]interface{}, 0, len(faces))
	undescribed := 0
	for i, u := range faces {
		// index 从 1 开始：模型按序号发送时与人的直觉一致
		item := map[string]interface{}{"index": i + 1, "url": u}
		d := p.stickerDescOf(u)
		if d.Desc == "" && d.Mood == "" {
			undescribed++
		} else {
			// 只带非空字段：空字符串会让模型误以为"描述就是空"
			if d.Desc != "" {
				item["desc"] = d.Desc
			}
			if d.Mood != "" {
				item["mood"] = d.Mood
			}
		}
		stickers = append(stickers, item)
	}
	hint := "用 qq_send_sticker(index=N) 发送第 N 个，或直接传 url"
	if undescribed > 0 {
		hint += fmt.Sprintf("；还有 %d 个表情没有描述：先用 see_image(url) 看一眼，"+
			"再调 qq_describe_sticker 把内容与情绪记下来（只需一次，之后列表自带 desc/mood）", undescribed)
	}
	return map[string]interface{}{
		"stickers": stickers,
		"total":    len(stickers),
		"hint":     hint,
	}, nil
}

func (p *Plugin) addFaceByFile(fileName string) (bool, error) {
	raw, err := p.napcat("add_custom_face", map[string]interface{}{
		"file": "/app/files/" + fileName,
	})
	if err != nil {
		return false, err
	}
	var rawData []byte
	switch v := raw.(type) {
	case json.RawMessage:
		rawData = v
	case []byte:
		rawData = v
	default:
		rawData, _ = json.Marshal(v)
	}
	var resp struct {
		Data struct {
			Result  int `json:"result"`
			IsExist int `json:"isExist"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rawData, &resp); err != nil {
		return false, fmt.Errorf("解析 add_custom_face 失败: %w", err)
	}
	if resp.Data.Result != 0 {
		return false, fmt.Errorf("add_custom_face result=%d", resp.Data.Result)
	}
	return resp.Data.IsExist == 1, nil
}

// downloadToRemote 下载图片到 remote 目录（NapCat 可见），返回文件名。
func (p *Plugin) downloadToRemote(url, baseName string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	// QQ 多媒体 CDN 对 UA 有要求；空 UA 会被 403。
	req.Header.Set("User-Agent", "Mozilla/5.0")
	client := p.httpClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载表情失败: HTTP %d", resp.StatusCode)
	}
	if baseName == "" {
		baseName = fmt.Sprintf("sticker_%d.jpg", time.Now().UnixNano())
	}
	os.MkdirAll(p.remoteDir, 0755)
	dest := filepath.Join(p.remoteDir, sanitizeFilename(baseName))
	f, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return "", err
	}
	f.Close()
	return filepath.Base(dest), nil
}

// copyToRemote 把本机已有图片复制到 remote 目录。
func (p *Plugin) copyToRemote(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	os.MkdirAll(p.remoteDir, 0755)
	dest := filepath.Join(p.remoteDir, sanitizeFilename(filepath.Base(path)))
	if err := os.WriteFile(dest, data, 0644); err != nil {
		return "", err
	}
	return filepath.Base(dest), nil
}

// stickerURLsFromMessage 取一条消息里所有表情图片的 URL（sub_type=1）。
//
// 为什么按 sub_type 而不是 summary：NapCat 上报表情包时是 image 段、sub_type=1，
// summary 可能是空的或本地化的「[动画表情]」，不可靠。
func (p *Plugin) stickerURLsFromMessage(messageID int64) ([]string, error) {
	if messageID <= 0 {
		return nil, fmt.Errorf("message_id 无效")
	}
	raw, err := p.napcat("get_msg", map[string]interface{}{"message_id": messageID})
	if err != nil {
		return nil, err
	}
	var rawData []byte
	switch v := raw.(type) {
	case json.RawMessage:
		rawData = v
	case []byte:
		rawData = v
	default:
		rawData, _ = json.Marshal(v)
	}
	var resp struct {
		Data struct {
			Message []struct {
				Type string                 `json:"type"`
				Data map[string]interface{} `json:"data"`
			} `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rawData, &resp); err != nil {
		return nil, fmt.Errorf("解析消息失败: %w", err)
	}
	var urls []string
	for _, seg := range resp.Data.Message {
		if seg.Type != "image" {
			continue
		}
		if st, ok := seg.Data["sub_type"].(float64); !ok || int(st) != 1 {
			continue
		}
		if u, _ := seg.Data["url"].(string); u != "" {
			urls = append(urls, u)
		}
	}
	return urls, nil
}

// stickerResID 从收藏表情 URL 里取出 NapCat 需要的 res_id。
//
// 格式（fetch_custom_face 返回）：
//
//	https://p.qpic.cn/qq_expression/<uin>/<uin>_0_0_0_<HASH>_0_0/0
//	                                         ^^^^^^^^^^^^^^^^^^^^^^^ 这一段才是 res_id
//
// ★ 实测（2026-10-20）：传裸 HASH 或完整 URL 都会返回 {"retcode":0,"errMsg":"操作成功"}，
// 但收藏数量不变 —— 那是**假成功**。只有传这段路径段才真的删掉。
func stickerResID(url string) string {
	re := regexp.MustCompile(`(\d+_\d+_\d+_\d+_[0-9A-Fa-f]+_\d+_\d+)`)
	if m := re.FindStringSubmatch(url); len(m) == 2 {
		return m[1]
	}
	return ""
}

// handleRemoveSticker 从收藏里删除一个表情（按 url 或 index）。
func (p *Plugin) handleRemoveSticker(args map[string]interface{}) (interface{}, error) {
	url, _ := args["url"].(string)
	if url == "" {
		idx64, err := convInt64(args["index"])
		if err != nil || idx64 < 1 {
			return nil, fmt.Errorf("需要 url 或 index（1 开始，来自 qq_list_stickers）")
		}
		count := int(idx64)
		if c, e := convInt64(args["count"]); e == nil && int(c) > count {
			count = int(c)
		}
		faces, err := p.fetchCustomFaces(count)
		if err != nil {
			return nil, err
		}
		if int(idx64) > len(faces) {
			return nil, fmt.Errorf("表情序号 %d 超出范围（当前收藏 %d 个）", idx64, len(faces))
		}
		url = faces[idx64-1]
	}
	resID := stickerResID(url)
	if resID == "" {
		return nil, fmt.Errorf("无法从表情地址解析 res_id: %q", url)
	}
	raw, err := p.napcat("delete_custom_face", map[string]interface{}{"res_id": resID})
	if err != nil {
		return nil, err
	}
	var rawData []byte
	switch v := raw.(type) {
	case json.RawMessage:
		rawData = v
	case []byte:
		rawData = v
	default:
		rawData, _ = json.Marshal(v)
	}
	var resp struct {
		Data struct {
			Result int    `json:"result"`
			ErrMsg string `json:"errMsg"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rawData, &resp)
	if resp.Data.Result != 0 {
		return nil, fmt.Errorf("delete_custom_face 失败: result=%d %s", resp.Data.Result, resp.Data.ErrMsg)
	}

	// 同步清掉描述缓存，避免以后同 URL 复活时带旧描述
	p.stickerMu.Lock()
	if _, ok := p.stickerDescs[url]; ok {
		delete(p.stickerDescs, url)
		p.stickerMu.Unlock()
		_ = p.saveStickerDescs()
	} else {
		p.stickerMu.Unlock()
	}
	return map[string]interface{}{"status": "ok", "removed": url, "res_id": resID}, nil
}

// handleAddSticker 收藏表情：message_id 整条消息 / url 单张 / file 本机文件。
func (p *Plugin) handleAddSticker(args map[string]interface{}) (interface{}, error) {
	var files []string

	// 来源 1：整条消息里的所有表情
	if id, err := convInt64(args["message_id"]); err == nil && id > 0 {
		urls, err := p.stickerURLsFromMessage(id)
		if err != nil {
			return nil, err
		}
		if len(urls) == 0 {
			return nil, fmt.Errorf("这条消息里没有表情图片（只有 sub_type=1 的 image 段才算表情）")
		}
		for i, u := range urls {
			name, err := p.downloadToRemote(u, fmt.Sprintf("sticker_%d_%d.jpg", id, i+1))
			if err != nil {
				return nil, fmt.Errorf("第 %d 张下载失败: %w", i+1, err)
			}
			files = append(files, name)
		}
	}

	// 来源 2：直接给图片地址
	if u, _ := args["url"].(string); u != "" {
		name, err := p.downloadToRemote(u, "")
		if err != nil {
			return nil, err
		}
		files = append(files, name)
	}

	// 来源 3：本机文件
	if path, _ := args["file"].(string); path != "" {
		name, err := p.copyToRemote(path)
		if err != nil {
			return nil, err
		}
		files = append(files, name)
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("需要 message_id、url 或 file 之一")
	}

	added := make([]map[string]interface{}, 0, len(files))
	for _, f := range files {
		existed, err := p.addFaceByFile(f)
		if err != nil {
			added = append(added, map[string]interface{}{"file": f, "ok": false, "error": err.Error()})
			continue
		}
		added = append(added, map[string]interface{}{"file": f, "ok": true, "already_existed": existed})
	}
	return map[string]interface{}{
		"added": added,
		"total": len(added),
		"hint":  "收藏后用 qq_list_stickers 查看；新表情还没有描述，第一次发之前先 see_image 看一眼再 qq_describe_sticker 记下来",
	}, nil
}

// handleDescribeSticker 记录一个表情的内容/情绪描述（按 url 缓存）。
func (p *Plugin) handleDescribeSticker(args map[string]interface{}) (interface{}, error) {
	url, _ := args["url"].(string)
	if url == "" {
		idx64, err := convInt64(args["index"])
		if err != nil || idx64 < 1 {
			return nil, fmt.Errorf("需要 url 或 index（1 开始，来自 qq_list_stickers）")
		}
		count := int(idx64)
		if c, e := convInt64(args["count"]); e == nil && int(c) > count {
			count = int(c)
		}
		faces, err := p.fetchCustomFaces(count)
		if err != nil {
			return nil, err
		}
		if int(idx64) > len(faces) {
			return nil, fmt.Errorf("表情序号 %d 超出范围（当前收藏 %d 个）", idx64, len(faces))
		}
		url = faces[idx64-1]
	}
	desc, _ := args["desc"].(string)
	mood, _ := args["mood"].(string)
	if strings.TrimSpace(desc) == "" && strings.TrimSpace(mood) == "" {
		return nil, fmt.Errorf("desc 与 mood 至少写一个（内容/情绪）")
	}

	p.stickerMu.Lock()
	if p.stickerDescs == nil {
		p.stickerDescs = map[string]stickerDesc{}
	}
	p.stickerDescs[url] = stickerDesc{Desc: strings.TrimSpace(desc), Mood: strings.TrimSpace(mood)}
	p.stickerMu.Unlock()

	if err := p.saveStickerDescs(); err != nil {
		return nil, fmt.Errorf("保存表情描述失败: %w", err)
	}
	return map[string]interface{}{"status": "ok", "url": url, "desc": desc, "mood": mood}, nil
}

func (p *Plugin) handleListChats(args map[string]interface{}) (interface{}, error) {
	count := 10
	if c, err := convInt64(args["count"]); err == nil && c > 0 && c < 100 {
		count = int(c)
	}
	chats := p.listChats(count)
	return map[string]interface{}{
		"chats": chats,
		"total": len(chats),
		"hint":  "按最新消息先后排序；unread 为该会话未读消息数，处理完用 qq_mark_read 清零；用 qq_get_history(group_id/user_id) 拉取会话内容",
	}, nil
}

func (p *Plugin) handleMarkRead(args map[string]interface{}) (interface{}, error) {
	// 0 不是合法会话，必须当"未提供"（否则 markChatRead(0) 静默清不存在的会话）
	if gid, ok := argPeerID(args, "group_id"); ok {
		p.markChatRead(gid)
		return map[string]interface{}{"status": "ok", "group_id": gid, "unread": 0}, nil
	}
	if uid, ok := argPeerID(args, "user_id"); ok {
		p.markChatRead(uid)
		return map[string]interface{}{"status": "ok", "user_id": uid, "unread": 0}, nil
	}
	return nil, fmt.Errorf("need group_id or user_id")
}

func (p *Plugin) handleGetHistory(args map[string]interface{}) (interface{}, error) {
	gid, hasGroup := argPeerID(args, "group_id")
	uid, hasUser := argPeerID(args, "user_id")
	count := 10
	if c, err := convInt64(args["count"]); err == nil && c > 0 {
		count = int(c)
	}

	var endpoint string
	var params map[string]interface{}
	switch {
	case hasGroup && hasUser:
		// 两个都给 ⇒ 语义不明。旧实现静默走群聊分支，把私聊请求打成群号查询，
		// 用户看到的是 NapCat 的「消息undefined不存在」，根本看不出是参数用错。
		return nil, fmt.Errorf("group_id 与 user_id 只能二选一：私聊请只传 user_id，群聊请只传 group_id")
	case hasGroup:
		endpoint = "get_group_msg_history"
		params = map[string]interface{}{"group_id": gid, "count": count}
	case hasUser:
		endpoint = "get_friend_msg_history"
		params = map[string]interface{}{"user_id": uid, "count": count}
	default:
		return nil, fmt.Errorf("需要 group_id（群聊）或 user_id（私聊）：私聊请只传 user_id，不要传 group_id=0")
	}

	rawResp, err := p.napcat(endpoint, params)
	if err != nil {
		return nil, err
	}
	rawStr, _ := rawString(rawResp)
	if rawStr == "" {
		return map[string]interface{}{"messages": []interface{}{}, "note": "未获取到历史消息"}, nil
	}

	// 解析 NapCat 响应，提取消息列表
	var resp struct {
		Data *struct {
			Messages []interface{} `json:"messages"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(rawStr), &resp); err != nil || resp.Data == nil {
		return map[string]interface{}{"raw_response": rawStr, "note": "解析 NapCat 响应失败"}, nil
	}

	// 格式化消息为可读文本，并提取文件信息
	var lines []string
	var files []map[string]interface{}
	for _, m := range resp.Data.Messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		sender := ""
		if s, ok := msg["sender"].(map[string]interface{}); ok {
			if nick, _ := s["nickname"].(string); nick != "" {
				sender = nick
			}
			if card, _ := s["card"].(string); card != "" {
				sender = card
			}
		}
		msgText, _ := msg["raw_message"].(string)
		if msgText == "" {
			if segs, ok := msg["message"].([]interface{}); ok {
				var parts []string
				for _, seg := range segs {
					segMap, _ := seg.(map[string]interface{})
					if segMap == nil {
						continue
					}
					typ, _ := segMap["type"].(string)
					segData, _ := segMap["data"].(map[string]interface{})
					if segData == nil {
						continue
					}
					switch typ {
					case "text":
						if t, _ := segData["text"].(string); t != "" {
							parts = append(parts, t)
						}
					case "file":
						name, _ := segData["name"].(string)
						if name == "" {
							name, _ = segData["file"].(string)
						}
						fid, _ := segData["file_id"].(string)
						fileURL, _ := segData["url"].(string)
						if name != "" {
							parts = append(parts, "[文件:"+name+"]")
						}
						if fid != "" {
							entry := map[string]interface{}{
								"file_id":  fid,
								"name":     name,
								"position": len(lines),
							}
							if fileURL != "" {
								entry["url"] = fileURL
							}
							files = append(files, entry)
						}
					case "image":
						parts = append(parts, "[图片]")
					case "video":
						parts = append(parts, "[视频]")
					}
				}
				if len(parts) > 0 {
					msgText = strings.Join(parts, " ")
				}
			}
		}
		if msgText == "" {
			continue
		}
		ts := ""
		if t, ok := msg["time"].(float64); ok {
			ts = time.Unix(int64(t), 0).Format("2006-01-02 15:04")
		}
		line := msgText
		if sender != "" {
			line = sender + ": " + msgText
		}
		if ts != "" {
			line = "[" + ts + "] " + line
		}
		lines = append(lines, line)
	}

	if len(lines) == 0 {
		return map[string]interface{}{
			"messages": []interface{}{},
			"note":     "未找到历史消息，可能群内暂无消息记录",
		}, nil
	}

	result := map[string]interface{}{
		"messages": lines,
		"count":    len(lines),
	}
	if len(files) > 0 {
		result["files"] = files
	}
	// 拉取过某会话历史即视为已读（与真人客户端一致：看过=已读）
	if hasGroup {
		p.markChatRead(gid)
	} else if hasUser {
		p.markChatRead(uid)
	}
	return result, nil
}

func (p *Plugin) handleGetGroups(args map[string]interface{}) (interface{}, error) {
	return p.napcat("get_group_list", map[string]interface{}{})
}

func (p *Plugin) handleGetFriends(args map[string]interface{}) (interface{}, error) {
	return p.napcat("get_friend_list", map[string]interface{}{})
}

func (p *Plugin) handleGetRecentContacts(args map[string]interface{}) (interface{}, error) {
	count := 10
	if c, err := convInt64(args["count"]); err == nil && c > 0 {
		count = int(c)
	}
	return p.napcat("get_recent_contact", map[string]interface{}{"count": count})
}

func (p *Plugin) handleResolveName(args map[string]interface{}) (interface{}, error) {
	if uid, err := convInt64(args["user_id"]); err == nil {
		return p.napcat("get_stranger_info", map[string]interface{}{"user_id": uid, "no_cache": true})
	}
	if gid, err := convInt64(args["group_id"]); err == nil {
		return p.napcat("get_group_info", map[string]interface{}{"group_id": gid, "no_cache": true})
	}
	return nil, fmt.Errorf("need user_id or group_id")
}

func (p *Plugin) handleResolveNickname(args map[string]interface{}) (interface{}, error) {
	keyword, _ := args["keyword"].(string)
	if keyword == "" {
		return nil, fmt.Errorf("keyword is required")
	}
	keyword = strings.ToLower(keyword)

	gid, hasGroupID := argPeerID(args, "group_id")
	if hasGroupID {
		v, err := p.napcat("get_group_member_list", map[string]interface{}{"group_id": gid})
		if err != nil {
			return nil, err
		}
		raw, _ := rawString(v)
		return filterMemberList(raw, keyword)
	}

	v, err := p.napcat("get_friend_list", map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	raw, _ := rawString(v)
	return filterFriendList(raw, keyword)
}

func filterFriendList(raw, keyword string) (interface{}, error) {
	var resp struct {
		Data []struct {
			UserID   int64  `json:"user_id"`
			Nickname string `json:"nickname"`
			Remark   string `json:"remark"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return raw, nil
	}
	var matches []map[string]interface{}
	for _, f := range resp.Data {
		if strings.Contains(strings.ToLower(f.Nickname), keyword) ||
			strings.Contains(strings.ToLower(f.Remark), keyword) {
			matches = append(matches, map[string]interface{}{
				"user_id":  f.UserID,
				"nickname": f.Nickname,
				"remark":   f.Remark,
			})
		}
	}
	if len(matches) == 0 {
		return fmt.Sprintf("未找到昵称/备注包含 %q 的好友", keyword), nil
	}
	return matches, nil
}

func filterMemberList(raw, keyword string) (interface{}, error) {
	var resp struct {
		Data []struct {
			UserID   int64  `json:"user_id"`
			Nickname string `json:"nickname"`
			Card     string `json:"card"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return raw, nil
	}
	var matches []map[string]interface{}
	for _, m := range resp.Data {
		if strings.Contains(strings.ToLower(m.Nickname), keyword) ||
			strings.Contains(strings.ToLower(m.Card), keyword) {
			matches = append(matches, map[string]interface{}{
				"user_id":  m.UserID,
				"nickname": m.Nickname,
				"card":     m.Card,
			})
		}
	}
	if len(matches) == 0 {
		return fmt.Sprintf("未找到昵称/名片包含 %q 的群成员", keyword), nil
	}
	return matches, nil
}

func (p *Plugin) handleGetGroupMemberInfo(args map[string]interface{}) (interface{}, error) {
	gid, _ := convInt64(args["group_id"])
	uid, _ := convInt64(args["user_id"])
	return p.napcat("get_group_member_info", map[string]interface{}{"group_id": gid, "user_id": uid})
}

func (p *Plugin) handleGroupManage(args map[string]interface{}) (interface{}, error) {
	cmd, _ := args["command"].(string)
	if cmd == "" {
		return nil, fmt.Errorf("need command")
	}
	if requiresConfirmGroupCommand(cmd) {
		if ok, _ := args["confirm"].(bool); !ok {
			return map[string]interface{}{"isError": true, "content": fmt.Sprintf("高风险操作 %s 需要 confirm=true", cmd)}, nil
		}
	}

	switch cmd {
	case "group-list":
		return p.napcat("get_group_list", map[string]interface{}{})
	case "group-info", "member-list", "member-info", "at-all-remain", "msg-history":
		gid, _ := convInt64(args["group_id"])
		if cmd == "msg-history" {
			count := 10
			if c, err := convInt64(args["count"]); err == nil && c > 0 {
				count = int(c)
			}
			return p.napcat("get_group_msg_history", map[string]interface{}{"group_id": gid, "count": count})
		}
		if cmd == "member-info" {
			uid, _ := convInt64(args["user_id"])
			return p.napcat("get_group_member_info", map[string]interface{}{"group_id": gid, "user_id": uid})
		}
		if cmd == "at-all-remain" {
			return p.napcat("get_group_at_all_remain", map[string]interface{}{"group_id": gid})
		}
		if cmd == "group-info" {
			return p.napcat("get_group_info", map[string]interface{}{"group_id": gid})
		}
		return p.napcat("get_group_member_list", map[string]interface{}{"group_id": gid})

	case "list-files":
		gid, _ := convInt64(args["group_id"])
		folderID, _ := args["folder_id"].(string)
		if folderID != "" {
			return p.napcat("get_group_files_by_folder", map[string]interface{}{"group_id": gid, "folder_id": folderID})
		}
		return p.napcat("get_group_root_files", map[string]interface{}{"group_id": gid})

	case "pending-requests":
		return p.napcat("get_group_system_msg", map[string]interface{}{})

	case "leave":
		gid, _ := convInt64(args["group_id"])
		return p.napcat("set_group_leave", map[string]interface{}{"group_id": gid})

	case "kick":
		gid, _ := convInt64(args["group_id"])
		uid, _ := convInt64(args["user_id"])
		reject, _ := args["reject_add"].(bool)
		return p.napcat("set_group_kick", map[string]interface{}{"group_id": gid, "user_id": uid, "reject_add_request": reject})

	case "ban":
		gid, _ := convInt64(args["group_id"])
		uid, _ := convInt64(args["user_id"])
		minutes := 10
		if m, err := convInt64(args["minutes"]); err == nil {
			minutes = int(m)
		}
		return p.napcat("set_group_ban", map[string]interface{}{"group_id": gid, "user_id": uid, "duration": minutes * 60})

	case "unban":
		gid, _ := convInt64(args["group_id"])
		uid, _ := convInt64(args["user_id"])
		return p.napcat("set_group_ban", map[string]interface{}{"group_id": gid, "user_id": uid, "duration": 0})

	case "rename":
		gid, _ := convInt64(args["group_id"])
		name, _ := args["name"].(string)
		return p.napcat("set_group_name", map[string]interface{}{"group_id": gid, "group_name": name})

	case "mute-all":
		gid, _ := convInt64(args["group_id"])
		enable, _ := args["enable"].(bool)
		return p.napcat("set_group_whole_ban", map[string]interface{}{"group_id": gid, "enable": enable})

	case "set-card":
		gid, _ := convInt64(args["group_id"])
		uid, _ := convInt64(args["user_id"])
		card, _ := args["card"].(string)
		return p.napcat("set_group_card", map[string]interface{}{"group_id": gid, "user_id": uid, "card": card})

	case "set-admin":
		gid, _ := convInt64(args["group_id"])
		uid, _ := convInt64(args["user_id"])
		enable, _ := args["enable"].(bool)
		return p.napcat("set_group_admin", map[string]interface{}{"group_id": gid, "user_id": uid, "enable": enable})

	case "set-title":
		gid, _ := convInt64(args["group_id"])
		uid, _ := convInt64(args["user_id"])
		title, _ := args["title"].(string)
		return p.napcat("set_group_special_title", map[string]interface{}{"group_id": gid, "user_id": uid, "special_title": title})

	case "recall":
		mid, _ := convInt64(args["message_id"])
		return p.napcat("delete_msg", map[string]interface{}{"message_id": mid})

	case "pin-msg":
		mid, _ := convInt64(args["message_id"])
		return p.napcat("set_essence_msg", map[string]interface{}{"message_id": mid})

	case "folder-create":
		gid, _ := convInt64(args["group_id"])
		name, _ := args["name"].(string)
		return p.napcat("create_group_file_folder", map[string]interface{}{"group_id": gid, "name": name})

	default:
		return nil, fmt.Errorf("unknown group_manage command: %s", cmd)
	}
}

func (p *Plugin) handleFriendAction(args map[string]interface{}) (interface{}, error) {
	cmd, _ := args["command"].(string)
	if requiresConfirmFriendCommand(cmd) {
		if ok, _ := args["confirm"].(bool); !ok {
			return map[string]interface{}{"isError": true, "content": fmt.Sprintf("高风险操作 %s 需要 confirm=true", cmd)}, nil
		}
	}
	switch cmd {
	case "list-friends":
		return p.napcat("get_friend_list", map[string]interface{}{})
	case "delete":
		uid, _ := convInt64(args["user_id"])
		return p.napcat("delete_friend", map[string]interface{}{"user_id": uid})
	case "block":
		uid, _ := convInt64(args["user_id"])
		// delete friend
		p.napcat("delete_friend", map[string]interface{}{"user_id": uid})
		// kick from groups
		gid, err := convInt64(args["group_id"])
		if err != nil {
			return map[string]interface{}{"isError": true, "content": "block 必须提供 group_id（插件不会自动遍历所有群踢人）"}, nil
		}
		p.napcat("set_group_kick", map[string]interface{}{"group_id": gid, "user_id": uid, "reject_add_request": true})
		return `{"status":"ok","message":"blocked"}`, nil
	case "approve-friend":
		flag, _ := args["flag"].(string)
		remark, _ := args["remark"].(string)
		return p.napcat("set_friend_add_request", map[string]interface{}{"flag": flag, "approve": true, "remark": remark})
	case "reject-friend":
		flag, _ := args["flag"].(string)
		return p.napcat("set_friend_add_request", map[string]interface{}{"flag": flag, "approve": false})
	default:
		return nil, fmt.Errorf("unknown friend_action command: %s", cmd)
	}
}

func (p *Plugin) handleGetGroupFiles(args map[string]interface{}) (interface{}, error) {
	gid, _ := convInt64(args["group_id"])
	cmd, _ := args["command"].(string)

	switch cmd {
	case "list":
		folderID, _ := args["folder_id"].(string)
		if folderID != "" {
			return p.napcat("get_group_files_by_folder", map[string]interface{}{"group_id": gid, "folder_id": folderID})
		}
		return p.napcat("get_group_root_files", map[string]interface{}{"group_id": gid})

	case "search":
		return p.napcat("get_group_root_files", map[string]interface{}{"group_id": gid})

	case "download":
		fileID, _ := args["file_id"].(string)
		filename, _ := args["filename"].(string)
		if filename == "" {
			filename = fmt.Sprintf("group_file_%s", fileID)
		}
		filename = sanitizeFilename(filename)
		// get download URL
		resp, err := p.napcat("get_group_file_url", map[string]interface{}{"group_id": gid, "file_id": fileID})
		if err != nil {
			return nil, err
		}
		respStr, ok := resp.(string)
		if !ok {
			return resp, nil
		}
		// parse URL from response
		var parsed struct {
			Data struct {
				URL string `json:"url"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(respStr), &parsed); err != nil || parsed.Data.URL == "" {
			return resp, nil
		}
		dlURL := parsed.Data.URL
		client := &http.Client{Timeout: 120 * time.Second}
		httpResp, err := client.Get(dlURL)
		if err != nil {
			return nil, fmt.Errorf("download: %w", err)
		}
		defer httpResp.Body.Close()
		content, err := io.ReadAll(httpResp.Body)
		if err != nil {
			return nil, fmt.Errorf("read download: %w", err)
		}
		os.MkdirAll(p.filesDir, 0755)
		savePath := filepath.Join(p.filesDir, filename)
		if err := os.WriteFile(savePath, content, 0644); err != nil {
			return nil, fmt.Errorf("save: %w", err)
		}
		return map[string]interface{}{
			"status": "ok", "path": savePath, "filename": filename, "size": len(content),
		}, nil

	default:
		return nil, fmt.Errorf("unknown get_group_files command: %s", cmd)
	}
}

func (p *Plugin) addDownloadTask(fileID, filename string) *DownloadTask {
	p.downloadMu.Lock()
	defer p.downloadMu.Unlock()
	t := &DownloadTask{FileID: fileID, Filename: filename, Status: "running", CreatedAt: time.Now().Format("15:04:05")}
	p.downloadTasks = append(p.downloadTasks, t)
	if len(p.downloadTasks) > 100 {
		p.downloadTasks = p.downloadTasks[len(p.downloadTasks)-100:]
	}
	return t
}

func (p *Plugin) updateDownloadTask(t *DownloadTask, status, path, errMsg string) {
	p.downloadMu.Lock()
	defer p.downloadMu.Unlock()
	t.Status = status
	t.Path = path
	t.Error = errMsg
}

func (p *Plugin) handleDownloadFile(args map[string]interface{}) (interface{}, error) {
	fileID, _ := args["file_id"].(string)
	if fileID == "" {
		return nil, fmt.Errorf("file_id is required")
	}
	fileURL, _ := args["url"].(string)
	filename, _ := args["filename"].(string)
	groupID, _ := convInt64(args["group_id"])
	userID, _ := convInt64(args["user_id"])

	task := p.addDownloadTask(fileID, filename)

	go func(t *DownloadTask, fid, fname, furl string, gid, uid int64) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[qq] download task %s panic: %v", fid, r)
			}
		}()
		savePath := ""
		errMsg := ""
		if furl != "" {
			savePath = p.downloadURL(furl, fname)
		}
		if savePath == "" && gid > 0 {
			savePath = p.downloadGroupFile(gid, fid, fname)
		}
		if savePath == "" && uid > 0 {
			savePath = p.downloadPrivateFile(uid, fid, fname)
		}
		if savePath == "" {
			savePath = p.downloadFromNapCat(fid, fname)
		}
		if savePath != "" {
			p.updateDownloadTask(t, "done", savePath, "")
			log.Printf("[qq] 文件下载完成: %s", savePath)
			if p.sdk != nil {
				// NoMemory：下载完成的状态通知，不是记忆内容。
				// Priority：同上，QQ 侧一律低级别中断（L1）。
				p.sdk.InjectInterruptTextOpts(p.name, p.name,
					fmt.Sprintf("文件下载完成: %s，保存在 %s", filepath.Base(savePath), savePath),
					sdk.InjectOptions{NoMemory: true, Priority: sdk.PriorityL1, RecallPolicy: sdk.RecallPolicyNone})
			}
		} else {
			errMsg = "下载失败，文件可能已过期"
			p.updateDownloadTask(t, "failed", "", errMsg)
			log.Printf("[qq] 文件下载失败: %s", fid)
		}
	}(task, fileID, filename, fileURL, groupID, userID)

	return map[string]interface{}{
		"status": "started", "file_id": fileID, "filename": filename,
		"hint": "下载已后台启动。使用 qq_get_download_tasks 查看进度。"}, nil
}

func (p *Plugin) handleGetDownloadTasks(args map[string]interface{}) (interface{}, error) {
	p.downloadMu.Lock()
	defer p.downloadMu.Unlock()
	// 返回最近 50 条
	tasks := p.downloadTasks
	if len(tasks) > 50 {
		tasks = tasks[len(tasks)-50:]
	}
	return map[string]interface{}{
		"tasks": tasks, "total": len(p.downloadTasks),
		"hint": "status 为 running 表示下载中，done 已完成，failed 已失败。用 qq_download_file 重新下载失败的任务。"}, nil
}

func (p *Plugin) handleUploadGroupFile(args map[string]interface{}) (interface{}, error) {
	gid, _ := convInt64(args["group_id"])
	filePath, _ := args["file"].(string)
	name, _ := args["name"].(string)
	if name == "" {
		name = filepath.Base(filePath)
	}
	name = sanitizeFilename(name)

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	b64 := fmt.Sprintf("base64://%s", base64.StdEncoding.EncodeToString(data))

	resp, err := p.napcat("send_group_msg", map[string]interface{}{
		"group_id": gid,
		"message": []map[string]interface{}{
			{"type": "file", "data": map[string]interface{}{"file": b64, "name": name}},
		},
	})
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"status": "ok", "file": name, "napcat": resp}, nil
}

func (p *Plugin) handleSendLike(args map[string]interface{}) (interface{}, error) {
	uid, _ := convInt64(args["user_id"])
	times := 1
	if t, err := convInt64(args["times"]); err == nil && t > 0 && t <= 20 {
		times = int(t)
	}
	return p.napcat("send_like", map[string]interface{}{"user_id": uid, "times": times})
}

// ======== CQ Code / Message Segment Processing ========

func (p *Plugin) processMessageSegments(segments []interface{}, nickname string) string {
	if len(segments) == 0 {
		return ""
	}
	botIDStr := strconv.FormatInt(p.botID, 10)
	var parts []string

	for _, seg := range segments {
		s, ok := seg.(map[string]interface{})
		if !ok {
			continue
		}
		typ, _ := s["type"].(string)
		data, _ := s["data"].(map[string]interface{})
		if data == nil {
			continue
		}

		switch typ {
		case "text":
			if t, _ := data["text"].(string); t != "" {
				parts = append(parts, t)
			}
		case "at":
			qq, _ := data["qq"].(string)
			if qq == "all" {
				parts = append(parts, "@所有人")
			} else if qq == botIDStr {
				continue
			} else {
				parts = append(parts, "@"+qq)
			}
		case "face", "sface":
			if id, _ := data["id"].(string); id != "" {
				parts = append(parts, "[表情]")
			}
		case "file":
			parts = append(parts, fmt.Sprintf("[%s发送了文件]", nickname))
		case "image":
			parts = append(parts, fmt.Sprintf("[%s发送了图片]", nickname))
		case "video":
			parts = append(parts, fmt.Sprintf("[%s发送了视频]", nickname))
		case "reply":
			if id, ok := data["id"].(float64); ok {
				parts = append(parts, fmt.Sprintf("[回复消息id=%.0f]", id))
			}
		case "music":
			if title, _ := data["title"].(string); title != "" {
				parts = append(parts, fmt.Sprintf("[音乐:%s]", title))
			} else {
				parts = append(parts, "[音乐]")
			}
		case "share":
			title, _ := data["title"].(string)
			urlStr, _ := data["url"].(string)
			if title != "" && urlStr != "" {
				parts = append(parts, fmt.Sprintf("[分享:%s %s]", title, urlStr))
			} else if urlStr != "" {
				parts = append(parts, fmt.Sprintf("[分享:%s]", urlStr))
			}
		default:
			if typ != "" {
				parts = append(parts, "["+typ+"]")
			}
		}
	}

	return strings.TrimSpace(strings.Join(parts, " "))
}

func (p *Plugin) downloadGroupFile(groupID int64, fileID, filename string) string {
	resp, err := p.napcat("get_group_file_url", map[string]interface{}{"group_id": groupID, "file_id": fileID})
	if err != nil {
		return ""
	}
	respStr, _ := rawString(resp)
	if respStr == "" {
		return ""
	}
	var parsed struct {
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(respStr), &parsed); err != nil || parsed.Data.URL == "" {
		return ""
	}
	if p.filesDir == "" {
		return ""
	}
	os.MkdirAll(p.filesDir, 0755)
	if filename == "" {
		filename = "group_file_" + fileID
	}
	savePath := filepath.Join(p.filesDir, sanitizeFilename(filename))
	httpResp, err := p.httpClient.Get(parsed.Data.URL)
	if err != nil {
		return ""
	}
	defer httpResp.Body.Close()
	data, err := io.ReadAll(httpResp.Body)
	if err != nil || len(data) == 0 {
		return ""
	}
	os.WriteFile(savePath, data, 0644)
	return savePath
}

func (p *Plugin) downloadPrivateFile(userID int64, fileID, filename string) string {
	resp, err := p.napcat("get_private_file_url", map[string]interface{}{"user_id": userID, "file_id": fileID})
	if err != nil {
		return ""
	}
	respStr, _ := rawString(resp)
	if respStr == "" {
		return ""
	}
	var parsed struct {
		Data *struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(respStr), &parsed) != nil || parsed.Data == nil || parsed.Data.URL == "" {
		return ""
	}
	return p.downloadURL(parsed.Data.URL, filename)
}

func (p *Plugin) downloadFromNapCat(fileID, filename string) string {
	if p.filesDir == "" || fileID == "" {
		return ""
	}
	raw, err := p.napcat("get_file", map[string]interface{}{"file_id": fileID})
	if err != nil {
		return ""
	}
	rawStr, _ := rawString(raw)
	if rawStr == "" {
		return ""
	}
	var resp struct {
		Data *struct {
			File     string `json:"file"`
			FileName string `json:"file_name"`
			FileSize int64  `json:"file_size"`
			Base64   string `json:"base64"`
			URL      string `json:"url"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(rawStr), &resp) != nil || resp.Data == nil {
		return ""
	}
	info := resp.Data
	if info.FileName != "" {
		filename = info.FileName
	}
	if info.Base64 != "" {
		data, err := base64.StdEncoding.DecodeString(info.Base64)
		if err == nil {
			savePath := filepath.Join(p.filesDir, sanitizeFilename(filename))
			// ★ 写盘失败必须返回失败（2026-10-27 现场）：此前 os.WriteFile 的
			//   错误被丢弃，savePath 照样返回 ⇒ 上层报「文件下载完成」并注入
			//   中断，agent 一读就是「文件不存在」（实测 qq_files 目录
			//   因属主/权限写不进去，用户只看到"下载成功但读不到"）。
			if werr := writeFileEnsured(savePath, data); werr != nil {
				log.Printf("[qq] 文件写入失败 %s: %v", savePath, werr)
				return ""
			}
			return savePath
		}
	}
	if info.URL != "" {
		return p.downloadURL(info.URL, filename)
	}
	if info.File != "" {
		savePath := filepath.Join(p.filesDir, sanitizeFilename(filename))
		if err := writeFileEnsured(savePath, []byte(info.File)); err == nil {
			return savePath
		} else {
			log.Printf("[qq] 文件写入失败 %s: %v", savePath, err)
		}
	}
	return ""
}

func (p *Plugin) downloadURL(fileURL, filename string) string {
	if p.filesDir == "" {
		return ""
	}
	if filename == "" {
		filename = "file_" + filepath.Base(fileURL)
	}
	savePath := filepath.Join(p.filesDir, sanitizeFilename(filename))
	dlResp, err := p.httpClient.Get(fileURL)
	if err != nil {
		return ""
	}
	defer dlResp.Body.Close()
	data, err := io.ReadAll(dlResp.Body)
	if err != nil || len(data) == 0 {
		return ""
	}
	// 同上：写不进去就是下载失败，不能返回一个不存在的路径。
	if werr := writeFileEnsured(savePath, data); werr != nil {
		log.Printf("[qq] 文件写入失败 %s: %v", savePath, werr)
		return ""
	}
	return savePath
}

// writeFileEnsured 建父目录并写文件，**返回真实错误**。
//
// 为什么单列一个函数：下载路径原先散落着 `os.MkdirAll(...)` 与
// `os.WriteFile(...)` 且都不看返回值 —— 目录不可写时依然返回路径，
// 上层就报"下载完成"。这类"假成功"比失败更难查：文件明明不存在，
// 日志却说已完成。
func writeFileEnsured(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create dir %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func sanitizeFilename(name string) string {
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			return '_'
		}
		return r
	}, name)
	return name
}

// ======== Tool Handlers: Document / Video / Web ========

func (p *Plugin) handleReadDocument(args map[string]interface{}) (interface{}, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("path is required")
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return map[string]interface{}{
			"content": fmt.Sprintf("文件不存在: %s", path),
		}, nil
	}

	ext := strings.ToLower(filepath.Ext(path))
	textContent := ""

	switch ext {
	case ".txt", ".md", ".csv":
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read file: %w", err)
		}
		textContent = string(data)
	case ".docx", ".doc", ".epub", ".html", ".htm":
		textContent = p.readWithPandoc(path)
	default:
		// Try pandoc first, fallback to libreoffice
		textContent = p.readWithPandoc(path)
		if textContent == "" {
			textContent = p.readWithLibreoffice(path)
		}
		if textContent == "" {
			// last resort: read as plain text
			data, err := os.ReadFile(path)
			if err == nil {
				textContent = string(data)
			}
		}
	}

	if textContent == "" {
		return map[string]interface{}{
			"content": fmt.Sprintf("无法提取文件内容: %s（不支持的文件格式或文件损坏）", path),
		}, nil
	}

	// 截断到 20000 字符
	origLen := len(textContent)
	truncated := origLen > 20000
	if truncated {
		textContent = textContent[:20000]
	}

	result := textContent
	if truncated {
		result += fmt.Sprintf("\n\n...（内容过长，仅显示前 20000 字符，共 %d 字符）", origLen)
	}
	return map[string]interface{}{
		"content":   result,
		"file":      path,
		"truncated": truncated,
	}, nil
}

func (p *Plugin) readWithPandoc(path string) string {
	var out bytes.Buffer
	cmd := exec.Command("pandoc", path, "-t", "plain", "--wrap=none")
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

func (p *Plugin) readWithLibreoffice(path string) string {
	tmpDir, err := os.MkdirTemp("", "lo-doc-*")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(tmpDir)

	cmd := exec.Command("libreoffice", "--headless", "--convert-to", "txt:Text", "--outdir", tmpDir, path)
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return ""
	}

	// 找生成的 txt 文件
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".txt") {
			data, err := os.ReadFile(filepath.Join(tmpDir, e.Name()))
			if err == nil {
				return strings.TrimSpace(string(data))
			}
		}
	}
	return ""
}

func (p *Plugin) handleVideoDownload(args map[string]interface{}) (interface{}, error) {
	url, _ := args["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("url is required")
	}
	infoOnly, _ := args["info_only"].(bool)

	outputDir := filepath.Join(p.agentfsDir, "videos")
	os.MkdirAll(outputDir, 0755)

	if infoOnly {
		var out bytes.Buffer
		cmd := exec.Command("yt-dlp", "--dump-json", url)
		cmd.Stdout = &out
		cmd.Stderr = nil
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("yt-dlp info: %w", err)
		}
		var info struct {
			Title    string `json:"title"`
			Duration int    `json:"duration"`
			Webpage  string `json:"webpage_url"`
			Formats  []struct {
				FormatID string `json:"format_id"`
				Ext      string `json:"ext"`
				Width    int    `json:"width"`
				Height   int    `json:"height"`
				Filesize int64  `json:"filesize"`
				Format   string `json:"format"`
			} `json:"formats"`
		}
		if err := json.Unmarshal(out.Bytes(), &info); err != nil {
			return string(out.String()), nil
		}
		dur := ""
		if info.Duration > 0 {
			dur = fmt.Sprintf("%d分%d秒", info.Duration/60, info.Duration%60)
		}
		lines := []string{fmt.Sprintf("🎬 %s", info.Title)}
		if dur != "" {
			lines = append(lines, fmt.Sprintf("   时长: %s", dur))
		}
		lines = append(lines, fmt.Sprintf("   链接: %s", info.Webpage))
		lines = append(lines, "")
		for _, f := range info.Formats {
			fs := ""
			if f.Filesize > 0 {
				fs = fmt.Sprintf(" (%.1f MB)", float64(f.Filesize)/1048576)
			}
			res := ""
			if f.Width > 0 && f.Height > 0 {
				res = fmt.Sprintf(" %dx%d", f.Width, f.Height)
			}
			lines = append(lines, fmt.Sprintf("  [%s] %s%s%s", f.FormatID, f.Format, res, fs))
		}
		return map[string]interface{}{"content": strings.Join(lines, "\n")}, nil
	}

	// 下载
	outputTmpl := filepath.Join(outputDir, "%(title)s.%(ext)s")
	var out bytes.Buffer
	cmd := exec.Command("yt-dlp", "-o", outputTmpl, "--no-playlist", "--print", "after_move:filepath", url)
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("yt-dlp download: %w", err)
	}

	// 解析 yt-dlp 输出的文件路径
	dlPath := strings.TrimSpace(out.String())
	if dlPath == "" {
		return map[string]interface{}{
			"content": "下载完成，但无法获取文件路径",
		}, nil
	}
	dlFilename := filepath.Base(dlPath)
	var fileSize int64 = 0
	if fi, err := os.Stat(dlPath); err == nil {
		fileSize = fi.Size()
	}
	return map[string]interface{}{
		"content":  fmt.Sprintf("✅ 下载完成: %s\n  大小: %.1f MB\n  路径: %s", dlFilename, float64(fileSize)/1048576, dlPath),
		"file":     dlPath,
		"filename": dlFilename,
	}, nil
}

// ======== NapCat HTTP Client ========

func messageWithMention(userID int64, content interface{}) []interface{} {
	return []interface{}{
		map[string]interface{}{"type": "at", "data": map[string]interface{}{"qq": strconv.FormatInt(userID, 10)}},
		content,
	}
}

// napcatSendActions 是「会在 QQ 协议层真实发出消息」的端点。
//
// 这些调用必须用 sendClient（更长的超时），因为 NapCat 内部要等
// NodeIKernelMsgService 的回执；用查询类的 30s 会把已发出的消息报成失败。
var napcatSendActions = map[string]bool{
	"send_private_msg": true,
	"send_group_msg":   true,
	"send_msg":         true,
}

// errNapcatTimeout 标记「发送超时」——语义特殊：请求可能已送达并已发出。
//
// 单独一个哨兵是为了让上层能把这条跟「真·发送失败」区分开：
// 真失败可以重试，超时则**不可盲目重试**（盲重试=重复发消息）。
var errNapcatTimeout = errors.New("napcat 请求超时")

func (p *Plugin) napcat(action string, params map[string]interface{}) (interface{}, error) {
	data, _ := json.Marshal(params)
	url := fmt.Sprintf("%s/%s", p.napcatURL, action)

	// 发送类端点到专属客户端：QQ 发送慢时 30s 不够，而超时会被误读成失败。
	client := p.httpClient
	if napcatSendActions[action] && p.sendClient != nil {
		client = p.sendClient
	}

	resp, err := client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		// 超时必须如实报「可能已发出」：只说"失败"会让模型重发→重复消息。
		if errors.Is(err, context.DeadlineExceeded) ||
			strings.Contains(err.Error(), "Client.Timeout") ||
			strings.Contains(err.Error(), "Timeout exceeded") {
			return nil, fmt.Errorf("napcat %s: %w（请求可能已送达并已发出，**不要盲目重发**；"+
				"若确认对方没收到再重试）: %v", action, errNapcatTimeout, err)
		}
		return nil, fmt.Errorf("napcat %s: %w", action, err)
	}
	defer resp.Body.Close()

	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("napcat decode %s: %w", action, err)
	}

	// ★ OneBot v11 的成败在**响应体**里，不在 HTTP 状态码：
	//     成功 {"status":"ok","retcode":0,"data":{...}}
	//     失败 {"status":"failed","retcode":100,"message":"..."}
	// 不校验就只能靠"HTTP 通不通"当成败判据，于是 NapCat 明确报失败
	// （如 data:null + "Timeout: NTEvent serviceAndMethod:...sendMsg"）
	// 也会被当成发送成功。
	if err := oneBotError(raw); err != nil {
		// ★ NapCat 侧超时（2026-10-13 生产实测：一次回复发了 4 条）。
		//
		// 与上面的 HTTP 超时**同属「可能已送达」**，但走的是另一条路径：
		// NapCat 把发送交给 QQ 内核后等 onMsgInfoListUpdate 事件，
		// 超时（默认 10s+）即抛 "Timeout: NTEvent ...sendMsg"，
		// 映射成非零 retcode（本次实测 retcode=200）。
		//
		// 若只报「发送失败」，模型会当成真失败而重发 —— 实测中
		// 模型连发 3 次（调用 id 各不相同，是模型自己的重试决策）。
		// 所以必须与 HTTP 超时给同样的「不要盲目重发」告警。
		if napcatSendActions[action] && isNapcatSendUnconfirmed(err) {
			return nil, fmt.Errorf("napcat %s: %w（NapCat 未等到 QQ 的发送确认，"+
				"但消息**很可能已送出**，**不要盲目重发**；若确认对方没收到再重试）: %v",
				action, errNapcatTimeout, err)
		}
		return nil, fmt.Errorf("napcat %s: %w", action, err)
	}
	return raw, nil
}

// isNapcatSendUnconfirmed 报告 OneBot 错误是否为「NapCat 未等到内核确认」。
//
// 为何按消息文本而不是 retcode 判定：这条超时来自 NapCat 内部的
// callNormalEventV2（napcat.mjs）——它把等待监听事件的超时包成普通 Error，
// 再经 OneBot 适配层转成非零 retcode，所以 **retcode 本身不携带"超时"语义**
// （实测为 200，而非某个专门的超时码）。可辨识的特征只有消息里的
// NTEvent + Timeout 字样。
//
// ★ 为何**不**限定 serviceAndMethod 必须是 sendMsg：
// 调用方已经用 napcatSendActions 限定了"这是发送动作"，这里再卡 sendMsg
// 是多余的。生产实测到的超时原文里服务名确实是 sendMsg，因此该限定在现有
// 证据下**既无必要、也无观测依据**；留着一个未被任何证据支持的收紧条件，
// 只会把同样「可能已送达」的情形误判回普通失败，而普通失败会诱发模型重发。
// 变异验证：加上/去掉该限定，对「发送动作 + 非 sendMsg 超时」用例结果不同，
// 说明这行代码是活的（而非冗余），故保持宽松版本。
func isNapcatSendUnconfirmed(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "NTEvent") && strings.Contains(s, "Timeout")
}

// oneBotError 解析 OneBot 响应里的成败字段，成功返回 nil。
//
// 判据以 retcode 为主、status 为辅：
//   - retcode 存在且 != 0 ⇒ 失败
//   - retcode 缺失但 status 显式为 failed/error ⇒ 失败
//   - 两者都缺失，或非对象响应（数组等）⇒ 无法判定，按**成功**处理
//     （与内核 isToolError 的保守口径一致：宁可少报失败，也不要把
//     正常结果报成失败——误报失败会诱发模型重复发送）
func oneBotError(raw json.RawMessage) error {
	var probe struct {
		Status  string          `json:"status"`
		Retcode *int            `json:"retcode"`
		Message json.RawMessage `json:"message"`
		Wording string          `json:"wording"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil // 非对象（数组等）：无成败语义
	}
	reason := func() string {
		if probe.Wording != "" {
			return probe.Wording
		}
		if s := strings.TrimSpace(strings.Trim(string(probe.Message), `"`)); s != "" {
			return s
		}
		return "未提供原因"
	}
	if probe.Retcode != nil && *probe.Retcode != 0 {
		return fmt.Errorf("retcode=%d: %s", *probe.Retcode, reason())
	}
	switch strings.ToLower(probe.Status) {
	case "failed", "error":
		return fmt.Errorf("status=%s: %s", probe.Status, reason())
	}
	return nil
}

// setInputStatus 发「对方正在输入中」指示。
//
// ★★ `user_id` 必须传**字符串**（2026-10-18 实测）。
//
// NapCat 4.8.x 接受数字，但 4.18.37 的 payloadSchema 把它声明成 Type.String：
//
//	const PayloadSchema = Type.Object({
//	  user_id: Type.String({ description: 'QQ号' }),
//	  event_type: Type.Number({...}),
//	})
//
// 传数字会直接报 `uid is empty`（不是 schema 错误 —— 更容易误判成"用户不存在"）。
// 实测：
//
//	{"user_id":1,"event_type":1}            → uid is empty
//	{"user_id":"1456128421","event_type":1} → retcode 0, errMsg success
//
// 这个差异不会引起编译错误、也不会引起内核告警 —— 只会让功能**静默失效**
// （Qzone 那批 API 在 4.18.37 仍未支持，同理）。
func (p *Plugin) setInputStatus(userID int64, eventType int) (interface{}, error) {
	return p.napcat("set_input_status", map[string]interface{}{
		"user_id":    strconv.FormatInt(userID, 10),
		"event_type": eventType,
	})
}

func (p *Plugin) startTyping(userID int64) {
	p.typingMu.Lock()
	if _, ok := p.typingMap[userID]; ok {
		p.typingMu.Unlock()
		return
	}
	ts := &typingState{userID: userID, stopCh: make(chan struct{})}
	p.typingMap[userID] = ts
	p.typingMu.Unlock()
	go p.typingLoop(ts)
}

func (p *Plugin) stopTyping(userID int64) {
	p.typingMu.Lock()
	ts, ok := p.typingMap[userID]
	if ok {
		delete(p.typingMap, userID)
	}
	p.typingMu.Unlock()
	if ok {
		close(ts.stopCh)
	}
}

func (p *Plugin) typingLoop(ts *typingState) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	timeout := time.After(30 * time.Second)
	for {
		select {
		case <-ticker.C:
			p.setInputStatus(ts.userID, 1)
		case <-ts.stopCh:
			return
		case <-timeout:
			p.stopTyping(ts.userID)
			return
		}
	}
}

// ======== Helpers ========

// rawString extracts a string from napcat's return type (json.RawMessage or string).
func rawString(v interface{}) (string, bool) {
	switch r := v.(type) {
	case string:
		return r, true
	case json.RawMessage:
		return string(r), true
	case []byte:
		return string(r), true
	}
	return "", false
}

var reAPIKey = regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password)\s*[=:]\s*\S+`)
var reSKKey = regexp.MustCompile(`sk-[a-zA-Z0-9]{20,}`)
var reInternalIP = regexp.MustCompile(`\b(127\.\d{1,3}\.\d{1,3}\.\d{1,3}|10\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3})\b`)
var stripCQRe = regexp.MustCompile(`\[CQ:[^\]]*\]|\[mirai:[^\]]*\]`)
var highRiskRe = regexp.MustCompile(`(假如你是|你现在是|请你(扮演|化作|假装|成为)|扮演(一个|一下)|把你自己(想象|当成)|你的(人设|设定)是|穿越(到|回)|你是从.{0,10}(来|穿越)|帮我编(个|一个)故事|写(个|一个)故事让|故事(中|里)的|觉得(这个|这台|这家)?(机器人|AI|助手|ai).{0,8}(怎么样|如何|好不好|评价)|评价(下|一下)?(这个|这台|这家)?(机器人|AI|助手|ai|gpt)|忽略(之前|所有)?(指令|规则|限制|禁令)|解除.{0,6}(限制|规则|约束)|越狱|绕过.{0,6}(限制|审查)|不用(遵守|管)(任何)?(规则|限制|指令)|无视(所有)?(规则|指令)|你是(一个|一只)自由的)`)

func (p *Plugin) sensitiveFilter(text string) string {
	if p.remoteDir != "" {
		text = strings.ReplaceAll(text, p.remoteDir, "[remote]")
	}
	if p.filesDir != "" {
		text = strings.ReplaceAll(text, p.filesDir, "[files]")
	}

	text = reAPIKey.ReplaceAllString(text, "$1=***")
	text = reSKKey.ReplaceAllString(text, "sk-***")
	text = reInternalIP.ReplaceAllString(text, "[IP]")
	return text
}

func convInt64(v interface{}) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case float64:
		return int64(n), nil
	case int:
		return int64(n), nil
	case json.Number:
		return n.Int64()
	case string:
		return strconv.ParseInt(n, 10, 64)
	}
	return 0, fmt.Errorf("cannot convert %T to int64", v)
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{
		name:                 name,
		allowFrom:            make(map[int64]struct{}),
		groupAllowFrom:       make(map[int64]struct{}),
		authByMessageID:      make(map[int64]qqAuthContext),
		outputSignatures:     make(map[string]int),
		maxQQToolCalls:       defaultMaxQQToolCalls,
		maxQQOutputCalls:     defaultMaxQQOutputCalls,
		maxDuplicateSend:     defaultMaxDuplicateSend,
		groupToolAllowlists:  parseGroupToolAllowlists(defaultGroupToolAllowlists),
		privateToolAllowlist: parseToolAllowlist(defaultPublicToolAllowlist),
		downloadTasks:        make([]*DownloadTask, 0),
		typingMap:            make(map[int64]*typingState),
		dmPolicy:             "open",
		groupPolicy:          "open",
	}, nil
}

// outputSendGroupTarget 检查一次工具调用是不是「向群聊发送消息」，
// 是的话返回群号字符串，否则返回空串。
//
// ## 为什么需要它（2026-10-18 现场事故）
//
// offload 子 agent 播报进度时调 `output_send__qq` 并填了
// `meta={"group_id": 979911915}` —— 本该私聊给用户的进度播报进了群。
//
// 子 agent 的 comefrom 是 `internal`，而原先 `isInternalToolCall(ctx)`
// 一放行就 `return nil`，**内部调用完全不检查目标**。
//
// ## 为什么在 before_toolcall 拦而不是 handleChannelOutput
//
// `ToolHandler` 只收 `args`，**拿不到帧上的 comefrom**（SDK 契约如此）。
// 只有 stage 帧能看到 `comefrom` + `ToolCalls`。所以必须在阶段里拦。
//
// 拦的方式是写 denial（与现有权限门一致），而不是返回 error ——
// 返回 error 会让整轮任务失败；写 denial 让模型收到“这条被拒、换个目标”的
// 明确指令，与其它权限拒绝的行为一致。
func outputSendGroupTarget(ctx *sdk.StageContext, selfName string) string {
	if ctx == nil || len(ctx.ToolCalls) == 0 {
		return ""
	}
	tc := &ctx.ToolCalls[0]
	if tc.Name != "output_send__"+selfName {
		return ""
	}
	meta, _ := tc.Arguments["meta"].(string)
	if meta == "" {
		return ""
	}
	var m struct {
		GroupID int64 `json:"group_id"`
	}
	if err := json.Unmarshal([]byte(meta), &m); err != nil {
		// meta 解析失败时不拦：让 handleChannelOutput 去报格式错，
		// 那里本来就有更准确的提示。
		return ""
	}
	if m.GroupID <= 0 {
		return ""
	}
	return strconv.FormatInt(m.GroupID, 10)
}

// turnHints 生成**本轮事实**提示（不写风格、不写人设）。
//
// ## 分工（用户 2026-10-26 明确）
//
//	人设（personal.md）  我是谁、怎么说话、黑名单、语气       —— 静态，永远成立
//	插件注入             这一轮**才成立的事实**             —— 动态，随消息变化
//
// 所以这里**只**放插件独有、且本轮确定的信息：
//
//	· 渠道形态：QQ 私聊 / 群，回复时要用的 meta 字段
//	· 群聊是否必须 @（group_require_at）以及对方 id
//	· 收藏表情能不能用、怎么用（type=sticker + sticker_index，一条一张）
//	· 本轮是合批的 N 条消息（模型据此决定是否先拉历史）
//
// 不放在这里的东西（都在人设里）：短句、不用 markdown、不用 emoji、
// 不客服腔、不列清单、群聊别抢话、被冒犯怎么回。那些是"怎么说话"，
// 不是"这一轮能做什么"；塞进插件等于把同一件事写两处。
func (p *Plugin) turnHints(messageType string, groupID, userID int64, byChance bool) string {
	var b strings.Builder

	// ① 渠道与回复目标（模型要照抄的 meta 字段）
	if messageType == "group" {
		b.WriteString(fmt.Sprintf("\n【本轮渠道】QQ 群 %d → 用 output_send__qq，meta={\"group_id\":%d}", groupID, groupID))
		if p.requireGroupAt() {
			b.WriteString(fmt.Sprintf("；群内回复需 @ 发话人（at 目标 user_id=%d），否则对方收不到提醒", userID))
		}
	} else {
		b.WriteString(fmt.Sprintf("\n【本轮渠道】QQ 私聊 → 用 output_send__qq，meta={\"user_id\":%d}", userID))
	}

	// ①.5 随机插话：这条消息**没有 @ 机器人**，是概率抽中的。
	//
	// 只陈述本轮成立的事实，不给风格说教：模型据此自己决定"接话还是只看"。
	if byChance {
		b.WriteString(fmt.Sprintf("\n【这条消息没有 @ 你】你是随机抽中(%.0f%%)参与的：可能是闲聊。"+
			"想回应就自然地接一句，接不上就只回一个「？」或表情；不要假装对方在跟你说话，"+
			"也不要复述你没看到的上下文", float64(p.replyChance)))
	}

	// ② 收藏表情：本轮可用的能力 + 正确用法（一条一张，不能和文字同一气泡）
	if n := p.stickerCount(); n > 0 {
		b.WriteString(fmt.Sprintf("；可发表情包（共 %d 个）：output_send__qq 且 type=sticker，meta 里给 sticker_index，payload 留空（不要填文字或 \"[sticker]\"）；一条消息一张，想说话请另发一条", n))
	}

	// ④ 插件配置的额外事实（运维可写，仍然只放事实，不要写风格）
	if extra := strings.TrimSpace(getSetting[string](p.sdk.Settings(), "turn_hints", "")); extra != "" {
		b.WriteString("\n" + extra)
	}
	return b.String()
}

// requireGroupAt 读"群内必须 @"的开关。
// requireGroupAt 读"群内必须 @"的开关。
// getSetting 的泛型只支持 string/int64/float64，故用字符串比较（配置里存的是 "true"/"false"）。
func (p *Plugin) requireGroupAt() bool {
	v, err := p.sdk.Settings().Get("group_require_at")
	if err != nil || v == nil {
		return true
	}
	switch val := v.(type) {
	case bool:
		return val
	case string:
		return strings.EqualFold(strings.TrimSpace(val), "true")
	}
	return true
}

// stickerCount 返回缓存里的收藏表情数量（0 表示这轮没法发表情）。
func (p *Plugin) stickerCount() int {
	raw := getSetting[string](p.sdk.Settings(), "sticker_descs", "")
	if strings.TrimSpace(raw) == "" {
		return 0
	}
	var m map[string]interface{}
	if json.Unmarshal([]byte(raw), &m) != nil {
		return 0
	}
	return len(m)
}
