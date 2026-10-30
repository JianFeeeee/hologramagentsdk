// Package rtvoice 是 **rtchild 接入的参考实现**（插件侧策略层）。
//
// ## 它做什么，不做什么
//
// 按 rtchild 计划 v0.4 §一 的分层，内核只负责「把打断送进 rt agent、
// 把连续输出接出来」；而**攒批、拐点判定、当前态表、VAD、采集**全部是
// **策略**，随业务变，归插件。本包就是那层策略的一个可跑参考实现。
//
//	内核（plan.md 十一）           本包（插件策略）
//	─────────────────────────────  ────────────────────────────────
//	rtSlot 单槽覆盖（最新者胜）      攒批窗口 250ms、按 key 覆盖的当前态表
//	ChatStream 流式输出             收 outputch 文本，交给采集层（TTS 等）
//	打断 / 截断                     拐点判定（开口/说完/画面突变）强制提前关窗
//	历史全文保留 + contextfull 交接   ——（内核的事，插件不碰）
//
// ## 为什么策略一定在插件侧
//
// 窗口多长、什么算拐点、静音怎么丢 —— 这些随设备与业务变（手机通话 vs
// 车载 vs 会议）。内核一旦写死，所有接入方被迫共用一套节奏。内核该保证的
// 只有一条：**新输入能打断旧输出，且打断后立刻接新的**。
//
// ## 一份最小可用的接线
//
//	p := rtvoice.New("voice")
//	p.Start(sdk)                       // 注册 inputch/rt + outputch/rt + 参数
//	p.Push(Event{Key: KeySpeech, Payload: "用户说的话"})   // 采集层喂进来
//	p.Flush()                          // 或由攒批窗口自动 flush
//
// 采集（麦克风/摄像头）不在本包：那是设备相关代码，本包只吃"已经削减过的
// 事件"（音频已过 VAD、画面已摘要），与计划 §3.3 的口径一致。
package rtvoice

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// Key 是当前态表的键。第一版写死（计划 §3.2），跑通再放开。
type Key string

const (
	// KeySpeech 是当前正在说的话（同 key 新值覆盖旧值）。
	KeySpeech Key = "speech"
	// KeySpeechDone 标记"这一句说完了"（拐点：强制关窗）。
	KeySpeechDone Key = "speech_done"
	// KeySpeaker 是当前说话人。
	KeySpeaker Key = "speaker"
	// KeyScene 是当前画面摘要。
	KeyScene Key = "scene"
	// KeySceneDirty 标记画面有突变（拐点：强制关窗）。
	KeySceneDirty Key = "scene_dirty"
	// KeyControl 是上游显式控制事件（拐点：强制关窗）。
	KeyControl Key = "control"
)

// turnpointKeys 是**语义拐点**：它们一到就立刻关窗送出，不等窗口到点。
//
// 为什么需要：真按每条事件都打断，一秒几十条流进来，模型连一轮都跑不完；
// 但只在窗口到点时才送，用户"开口/说完"的关键时刻会被延迟最多一个窗口。
// 拐点强制关窗是两者的折中（计划 §3.1）。
var turnpointKeys = map[Key]bool{
	KeySpeechDone: true,
	KeySceneDirty: true,
	KeyControl:    true,
}

// Event 是一次进入当前态表的事件。
type Event struct {
	Key     Key
	Payload string
	// At 是事件时间（零值 = 现在）。供延迟分段统计用。
	At time.Time
}

// entry 是当前态表的一格：**只有最新值**（旧值被覆盖即作废）。
type entry struct {
	Payload string
	Seq     uint64 // 单调递增，用于机械校验过期
	At      time.Time
}

// Batch 是一次攒批的产出，即将投给 rtchild 的内容。
type Batch struct {
	ID       uint64
	OpenedAt time.Time
	ClosedAt time.Time
	// Reason 是关窗原因："window"（窗口到点）/ 拐点 key 名。
	Reason string
	// Events 已按 key 去重（只留最新），按 key 名排序以保证可复现。
	Events []Event
}

// Text 把批次渲染成投给 rtchild 的一段文本。
//
// 渲染口径：**只给当前态**，不陈述历史。rt 的语义是"现在是什么样"，
// 给模型一堆"然后…然后…"会把实时对话变成流水账。
func (b Batch) Text() string {
	if len(b.Events) == 0 {
		return ""
	}
	var parts []string
	for _, e := range b.Events {
		if strings.TrimSpace(e.Payload) == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %s", e.Key, e.Payload))
	}
	return strings.Join(parts, "\n")
}

// Config 是攒批策略的参考取值（计划 §3.1 的默认值）。
type Config struct {
	// Window 是攒批窗口。计划参考值 200～300ms，默认 250ms。
	//
	// 它不是"延迟优化"，而是**压缩事件密度**：延迟预算由拐点打断兜底。
	Window time.Duration
	// MaxEvents 是单批事件数上限（防某个 key 高频抖动把批次撑大）。
	MaxEvents int
	// DegradedAfter 是连续多少次超时失败后标记 degraded（计划 §3.4）。
	DegradedAfter int
}

// DefaultConfig 返回参考配置。
func DefaultConfig() Config {
	return Config{Window: 250 * time.Millisecond, MaxEvents: 32, DegradedAfter: 3}
}

// Plugin 是 rtvoice 的插件实例。
type Plugin struct {
	name string
	cfg  Config
	sdk  *sdk.PluginSDK

	mu      sync.Mutex
	table   map[Key]entry // 当前态表：key → 最新值
	seq     uint64
	opened  time.Time
	batchID uint64
	reason  string // 本批已触发的拐点（空 = 还没触发）

	// timer 是窗口定时器；拐点到达时会被提前停掉并立刻 flush。
	timer *time.Timer

	// dropped 统计被覆盖掉的 key 更新次数（"当前态"的直接证据）。
	dropped int
	// flushes / turnpointFlushes 供观测（延迟分段统计的基础）。
	flushes          int
	turnpointFlushes int
	// degraded 是连续失败计数（计划 §3.4）。
	degraded int

	// emit 是往 rtchild 投批次的出口；可在测试里替换。
	emit func(text string)
	// outputHook 是下游（TTS/播放）的挂钩：内核写进 outputch(rt) 的文本经它落地。
	outputHook func(string)
}

// New 创建一个 rtvoice 插件实例。
func New(name string) *Plugin {
	return &Plugin{
		name:  name,
		cfg:   DefaultConfig(),
		table: map[Key]entry{},
	}
}

// Name 实现 sdk.Plugin。
func (p *Plugin) Name() string { return p.name }

// Start 注册通道与参数，并接上投递出口。
//
// ★ 两个注册口都带 Realtime 标记 —— 这是「本通道能承载 rt 流」的声明，
// 内核据此允许把它划给 rtchild（plan.md 十一.三 F）。
func (p *Plugin) Start(s *sdk.PluginSDK) error {
	p.sdk = s

	// ① inputch(rt)：采集层往里灌"已经削减过的事件"。
	if err := s.RegisterInputChannel(p.name+"-in", sdk.ChannelDef{
		Realtime: true,
		// rt 流不进记忆：通话的每一句都进记忆会把图谱灌满噪声。
		NoMemory: true,
	}); err != nil {
		return fmt.Errorf("注册 inputch(rt): %w", err)
	}

	// ② outputch(rt)：内核把 rtchild 的**文本**流写进来。
	// ★ 只收文本；怎么展示（TTS / 字幕 / 播放）是本插件的事（用户裁定 R6）。
	//
	// caps 取 1（CapText）—— rt 出口**不声明音频能力**：那不是内核要产出音频，
	// 而是本插件内部把文本转语音。CapRT 是**通道侧**"能承载 rt 流"的声明，
	// 由 ChannelDef.Realtime 表达（内核注册时会映射到能力位）。
	if err := s.RegisterOutputChannel(p.name, 1, "实时通话输出（文本流）",
		sdk.ChannelDef{Realtime: true},
		func(args map[string]interface{}) (interface{}, error) {
			text, _ := args["text"].(string)
			p.EmitOutput(text)
			return map[string]interface{}{"ok": true}, nil
		}); err != nil {
		return fmt.Errorf("注册 outputch(rt): %w", err)
	}

	// ③ 参数：窗口长度可调（部署方按设备调优）。
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "window_ms", Default: "250", Type: "int",
		DisplayName: "攒批窗口（毫秒）",
		Description: "实时流的事件合并窗口。200～300 是参考值；" +
			"窗口越短越跟手、但轮次越多越贵。语义拐点会强制提前关窗。",
	})

	p.emit = func(text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		// 投给 rtchild：走**中断**语义（rt 通道的单槽覆盖），不是排队。
		s.InjectInterruptTextOpts(p.name, p.name+"-in", text,
			sdk.InjectOptions{NoMemory: true})
	}
	return nil
}

// Stop 清理定时器（插件卸载/禁用时）。
func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	return nil
}

// SetConfig 替换攒批配置（测试与部署方调优用）。
func (p *Plugin) SetConfig(c Config) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c.Window > 0 {
		p.cfg.Window = c.Window
	}
	if c.MaxEvents > 0 {
		p.cfg.MaxEvents = c.MaxEvents
	}
	if c.DegradedAfter > 0 {
		p.cfg.DegradedAfter = c.DegradedAfter
	}
}

// Push 把一条事件放进当前态表。
//
// 语义（计划 §3.2）：
//   - **同 key 新值直接覆盖**（旧值作废，dropped 计数 +1）
//   - 空值**不覆盖**（"没说话"不该抹掉"刚才说了什么"）
//   - 拐点 key 到达 ⇒ **立刻关窗送出**，不等窗口到点
func (p *Plugin) Push(ev Event) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	p.mu.Lock()
	if p.opened.IsZero() {
		p.opened = ev.At
	}
	payload := strings.TrimSpace(ev.Payload)
	if payload != "" {
		if old, ok := p.table[ev.Key]; ok && old.Payload != payload {
			p.dropped++
		}
		p.seq++
		p.table[ev.Key] = entry{Payload: payload, Seq: p.seq, At: ev.At}
	}
	isTurnpoint := turnpointKeys[ev.Key]
	tooMany := len(p.table) >= p.cfg.MaxEvents
	if isTurnpoint {
		p.reason = string(ev.Key)
	}
	p.mu.Unlock()

	// 拐点立刻关窗（同步送出，不排队等定时器）。
	if isTurnpoint {
		p.Flush()
		return
	}
	// 批次撑到上限也关窗（防抖动把批撑大）。
	if tooMany {
		p.Flush()
		return
	}
	p.armWindow()
}

// armWindow 武装/重置窗口定时器。已有定时器时不重复创建。
func (p *Plugin) armWindow() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.timer != nil || p.cfg.Window <= 0 {
		return
	}
	p.timer = time.AfterFunc(p.cfg.Window, func() {
		p.mu.Lock()
		p.timer = nil
		p.mu.Unlock()
		p.Flush()
	})
}

// Flush 立刻关窗：把**当前态**（不是事件历史）组装成一批送出去。
//
// 返回送出的批次；空批次返回 nil（不投空消息，避免对 rt 的空打断）。
func (p *Plugin) Flush() *Batch {
	p.mu.Lock()
	if len(p.table) == 0 {
		if p.timer != nil {
			p.timer.Stop()
			p.timer = nil
		}
		p.mu.Unlock()
		return nil
	}
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}

	b := &Batch{
		ID:       p.batchID + 1,
		OpenedAt: p.opened,
		ClosedAt: time.Now(),
		Reason:   p.reason,
	}
	if b.Reason == "" {
		b.Reason = "window"
	}
	// 按 key 名排序：批次内容可复现（判据依赖它，人工复盘也需要）。
	keys := make([]Key, 0, len(p.table))
	for k := range p.table {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		b.Events = append(b.Events, Event{Key: k, Payload: p.table[k].Payload, At: p.table[k].At})
	}

	p.batchID++
	p.flushes++
	if p.reason != "" {
		p.turnpointFlushes++
	}
	// ★ 送出后清表：**当前态是"这一批"的状态**，不是累积。
	//   留着会让下一批把上一批的内容再送一遍（重复打断）。
	p.table = map[Key]entry{}
	p.opened = time.Time{}
	p.reason = ""
	emit := p.emit
	p.mu.Unlock()

	if emit != nil {
		emit(b.Text())
	}
	return b
}

// EmitOutput 接收内核写进 outputch(rt) 的**文本**流。
//
// ★ 内核只给文本（用户裁定 R6）。这里做什么（TTS、字幕、转发到下位机）
// 完全是本插件的事 —— 本参考实现只做**延迟观测**并转给下游回调。
func (p *Plugin) EmitOutput(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	p.mu.Lock()
	hook := p.outputHook
	p.mu.Unlock()
	if hook != nil {
		hook(text)
	}
}

// outputHook 是下游（真正的 TTS/播放）的挂钩；测试用它断言文本确实流出来了。
func (p *Plugin) SetOutputHook(fn func(string)) {
	p.mu.Lock()
	p.outputHook = fn
	p.mu.Unlock()
}

// Stats 是观测面（延迟分段与降级状态）。
type Stats struct {
	Batches          int `json:"batches"`
	TurnpointBatches int `json:"turnpoint_batches"`
	Dropped          int `json:"dropped"`
	TableKeys        int `json:"table_keys"`
	Degraded         int `json:"degraded"`
}

// Stats 返回观测快照。
func (p *Plugin) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{
		Batches: p.flushes, TurnpointBatches: p.turnpointFlushes,
		Dropped: p.dropped, TableKeys: len(p.table), Degraded: p.degraded,
	}
}

// MarkDegraded / MarkHealthy 是异常降级的记账（计划 §3.4）。
//
// 超时/断流时调用；连续 DegradedAfter 次即标 degraded。
// 注意：降级**不改变**"只认当前态"的语义 —— 它只是让运维看得见。
func (p *Plugin) MarkDegraded() {
	p.mu.Lock()
	p.degraded++
	n := p.degraded
	threshold := p.cfg.DegradedAfter
	p.mu.Unlock()
	if n >= threshold {
		log.Printf("[rtvoice] degraded: %d consecutive failures (threshold %d)", n, threshold)
	}
}

// MarkHealthy 清零降级计数（一次成功即恢复）。
func (p *Plugin) MarkHealthy() {
	p.mu.Lock()
	p.degraded = 0
	p.mu.Unlock()
}

// ── 内置插件注册 ──
//
// rtvoice 作为**参考实现**随内核发布（auto_start=false：它需要设备侧采集
// 才能真正工作，默认不跑）。接入方要用的正确姿势是：
//
//  1. 通过 sdk 的 RegisterInputChannel(name+"-in", ChannelDef{Realtime:true})
//     声明 rt 输入通道（本包 Start 里已做）
//  2. 在真机上把采集层（麦克风/VAD/画面摘要）接到 p.Push(Event{...})
//  3. 把 rtchild 的 outputch(rt) 指到本插件，接收内核写回的**文本**流
//
// NewPluginFactory 是**外部插件**的约定入口（模板 proc_main.go.tmpl 直接调用它）。
//
// ★ 为什么不是 init() + RegisterFactory：
// 那是**内置插件**的注册方式（编译进内核，plugin 包在进程内可见）。
// 外部插件是独立进程，由模板生成的 main 包按名字调用本函数拿实例，
// 没有任何东西会在 init() 里读注册表 —— 写了也是死代码。
func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return New(name), nil
}
