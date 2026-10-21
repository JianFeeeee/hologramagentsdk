package main

// QQ 在线状态：随内核工作阶段自动切换。
//
// ## 接口（NapCat 源码 SetOnlineStatus.ts 的权威取值表）
//
//	set_online_status      status + ext_status + battery_status
//	set_diy_online_status  face_id + face_type + wording
//
// 取值来自 NapCat 的 actionDescription（不是猜的）：
//
//	在线      status=10  ext=0
//	忙碌      status=50  ext=0
//	离开      status=30  ext=0
//	请勿打扰  status=70  ext=0
//	隐身      status=40  ext=0
//	听歌中    status=10  ext=1028
//	学习中    status=10  ext=1018
//	搬砖中    status=10  ext=2023
//	睡觉中    status=10  ext=1016
//	摸鱼中    status=10  ext=1300
//
// ★ 注意 status=11 **不是**标准值。我最初用 11 试探，NapCat 也返回 retcode=0
// （它不校验），但那是无效状态 —— 必须以源码里的取值表为准。
//
// ## 设计要点
//
// 1. **只读 stage，不当闸门**：状态切换失败**绝不**影响工具调用。
//    NapCat 掉线时若因为设状态失败而阻塞工具，那才是真故障。
// 2. **防抖**：stage 在每次工具调用都会触发，逐次发 HTTP 会把 NapCat 打满。
//    同状态只在变化时发，且最短间隔 2 秒（与 typing 的 5s 心跳同思路）。
// 3. **可关闭**：默认关闭（online_status_enabled=false）。理由与 offload 一致 ——
//    自动改 QQ 状态是"内核替用户做决定"，属于显式特权。
// 4. **失败静默**：只记一次日志（同类失败合并计数），不打爆日志。

import (
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/JianFeeeee/hologramagentsdk/sdk"
)

// QQ 在线状态（NapCat 权威取值）。
type qqStatus struct {
	Status    int    // status 字段
	ExtStatus int    // ext_status 字段
	Name      string // 人读名（日志与工具回显用）
}

// 常用状态常量。取值出自 NapCat SetOnlineStatus.ts 的 actionDescription。
var (
	qqStatusOnline = qqStatus{Status: 10, ExtStatus: 0, Name: "在线"}
	qqStatusBusy   = qqStatus{Status: 50, ExtStatus: 0, Name: "忙碌"}
	qqStatusLeave  = qqStatus{Status: 30, ExtStatus: 0, Name: "离开"}
	qqStatusDND    = qqStatus{Status: 70, ExtStatus: 0, Name: "请勿打扰"}
	qqStatusHidden = qqStatus{Status: 40, ExtStatus: 0, Name: "隐身"}
	// 工种类状态（ext_status 细分，status 恒为 10）
	qqStatusWorking = qqStatus{Status: 10, ExtStatus: 2023, Name: "搬砖中"}
	qqStatusStudy   = qqStatus{Status: 10, ExtStatus: 1018, Name: "学习中"}
	qqStatusMusic   = qqStatus{Status: 10, ExtStatus: 1028, Name: "听歌中"}
)

// qqStatusByName 供工具用名字设置状态（避免让模型记数字）。
var qqStatusByName = map[string]qqStatus{
	"online":  qqStatusOnline,
	"busy":    qqStatusBusy,
	"leave":   qqStatusLeave,
	"dnd":     qqStatusDND,
	"hidden":  qqStatusHidden,
	"working": qqStatusWorking,
	"study":   qqStatusStudy,
	"music":   qqStatusMusic,
	"在线":      qqStatusOnline,
	"忙碌":      qqStatusBusy,
	"离开":      qqStatusLeave,
	"请勿打扰":    qqStatusDND,
	"隐身":      qqStatusHidden,
	"搬砖中":     qqStatusWorking,
	"学习中":     qqStatusStudy,
	"听歌中":     qqStatusMusic,
	// 常用别名（用户/模型可能说“音乐”而不是“听歌中”）
	"音乐": qqStatusMusic,
	"工作": qqStatusWorking,
	"学习": qqStatusStudy,
	"勿扰": qqStatusDND,
	"离线": qqStatusHidden,
}

// minStatusInterval 是两次自动状态切换的最小间隔。
//
// 取 2 秒：stage 在每次工具调用都会触发，一轮对话可能有十几次；
// 逐次发 HTTP 会把 NapCat 打满，且 QQ 侧看到状态疯狂闪烁。
//
// ★ 收尾（StageAfterOutput）**不受此限**，见 applyStatusForce。
const minStatusInterval = 2 * time.Second

// statusState 是自动状态切换的运行态。
type statusState struct {
	mu sync.Mutex
	// applied 是**最近一次成功应用**的状态（空 Name = 还没设过）。
	applied qqStatus
	// lastAt 是最近一次实际发请求的时间（限流用）。
	lastAt time.Time
	// failCount 统计连续失败次数（只在首次与每 10 次记日志，避免刷屏）。
	failCount int

	// pending 是**限流期间被推迟**的最新期望状态（空 Name = 无待发）。
	//
	// ## 为什么需要它（2026-10-18 修）
	//
	// 原先限流（2 秒）一命中就直接 `return`，**期望状态被丢弃**。
	// 而 stage 之间的间隔常常不到 2 秒：
	//
	//	pre_action(学习中) --0.5s--> before_toolcall(搬砖中)
	//	⇒ "搬砖中" 被丢；工具跑完后 after_toolcall 又要"学习中"，
	//	   若落在限流窗口内同样被丢 ⇒ QQ 上看到的状态与实际工作**长期不符**。
	//
	// 状态是「当前工作阶段」的**投影**，不是事件流 —— 投影必须最终收敛到
	// 最新值，不能因为来得早而被丢掉。所以限流只限制**发送频率**，
	// 被推迟的那一次必须在窗口结束后补发（见 flushPendingStatusLocked）。
	pending qqStatus
	// pendingTimer 是补发定时器（非 nil = 已有一次补发排队，勿重复排）。
	pendingTimer *time.Timer
}

// schedulePendingFlushLocked 排一次「窗口结束后补发最新期望状态」。
//
// 调用方必须持有 statusState.mu。
//
// ## 为什么是"补发最新"而不是"补发被拒的那个"
//
// 限流窗口内可能连续来了多个期望（学习中 → 搬砖中 → 学习中）。
// 真正的语义是"最终应该是什么状态"，中间值没有独立意义 ——
// 所以补发时取 `pending`（已被后续调用覆盖成最新值）。
//
// 已排过一次就不再排（pendingTimer != nil）：多次排队会让补发风暴，
// 每个 timer 都会发一次请求。
func (p *Plugin) schedulePendingFlushLocked(delay time.Duration) {
	if p.statusState.pendingTimer != nil {
		return // 已有补发排队，它醒来时会取最新 pending
	}
	if delay < 0 {
		delay = 0
	}
	p.statusState.pendingTimer = time.AfterFunc(delay, func() {
		p.flushPendingStatus()
	})
}

// flushPendingStatus 补发被限流推迟的最新状态。
//
// 取 pending 后**立刻清空**再释放锁：补发过程中若又来了新的期望，
// 它们会各自走 applyStatusMaybe 重新判断，不会被这里的清理误吞。
func (p *Plugin) flushPendingStatus() {
	p.statusState.mu.Lock()
	st := p.statusState.pending
	p.statusState.pending = qqStatus{}
	p.statusState.pendingTimer = nil
	if st.Name == "" || p.statusState.applied.Name == st.Name {
		// 无待发，或补发前状态已被别的路径设成一样 ⇒ 什么都不用做
		p.statusState.mu.Unlock()
		return
	}
	p.statusState.lastAt = time.Now()
	p.statusState.mu.Unlock()

	if err := p.setSelfStatus(st); err != nil {
		p.statusState.mu.Lock()
		p.statusState.failCount++
		n := p.statusState.failCount
		p.statusState.mu.Unlock()
		if n == 1 || n%10 == 0 {
			log.Printf("[qq] 补发在线状态 %s 失败（第 %d 次，已忽略）: %v", st.Name, n, err)
		}
		return
	}
	p.statusState.mu.Lock()
	p.statusState.applied = st
	p.statusState.failCount = 0
	p.statusState.mu.Unlock()
	log.Printf("[qq] 在线状态 → %s（status=%d ext=%d，限流后补发）", st.Name, st.Status, st.ExtStatus)
}

// setSelfStatus 调 NapCat 设置在线状态。
//
// 失败返回 error，但**调用方不应把它当致命错误**（见文件头设计要点 1）。
func (p *Plugin) setSelfStatus(st qqStatus) error {
	_, err := p.napcat("set_online_status", map[string]interface{}{
		"status":         st.Status,
		"ext_status":     st.ExtStatus,
		"battery_status": 100,
	})
	return err
}

// setSelfDiyStatus 设自定义状态（图标 + 文字）。
//
// face_id 是 QQ 状态图标 ID（face_config.json 的 QSid）。这里不内置图标表：
// 由调用方给，给不出就走 set_online_status 的预置状态。
func (p *Plugin) setSelfDiyStatus(faceID, wording string) error {
	_, err := p.napcat("set_diy_online_status", map[string]interface{}{
		"face_id":   faceID,
		"face_type": "1",
		"wording":   wording,
	})
	return err
}

// applyStatus 防抖地应用一个状态。
//
// 防抖规则：
//   - 与已应用状态相同 ⇒ 直接跳过（stage 每轮都触发，不跳过会把 NapCat 打满）
//   - 距上次实际请求 < minInterval ⇒ 跳过（但记下"想切"的意图，下次生效）
//
// ★ 这里刻意**不做排队重试**：状态是"当前观感"，错过一次就在下一个 stage 补上，
// 没必要维护队列（队列会让状态滞后于阶段，反而更错）。
func (p *Plugin) applyStatus(st qqStatus) {
	p.applyStatusMaybe(st, true)
}

// applyStatusForce 跳过防抖（用于"收尾"：一轮结束时必须回在线）。
//
// ## 为什么必须有这个分支（2026-10-18 实测 bug）
//
// QQ 界面持续停「学习中」。原因：
//
//	BeforeOutput → AfterOutput 常常不足 2 秒
//	⇒ 收尾状态被防抖拦住 ⇒ **永远回不到在线**
//
// 防抖本身是对的（防刷 NapCat），但**收尾不能被防抖** ——
// 它是一轮结束时唯一的"归位"机会，错过就得等下一轮，
// 那期间好友看到的是错的状态。
func (p *Plugin) applyStatusForce(st qqStatus) {
	p.applyStatusMaybe(st, false)
}

// applyStatusMaybe 是 applyStatus / applyStatusForce 的共同实现。
func (p *Plugin) applyStatusMaybe(st qqStatus, debounce bool) {
	if !p.statusAutoEnabled.Load() {
		return
	}
	p.statusState.mu.Lock()
	if p.statusState.applied.Name == st.Name && p.statusState.pending.Name == "" {
		p.statusState.mu.Unlock()
		return
	}
	if debounce && time.Since(p.statusState.lastAt) < minStatusInterval {
		// ★ 限流命中：**记住**这个期望，等窗口结束补发，不能丢。
		//
		// 原先这里直接 return ⇒ "搬砖中"/"学习中" 在快速切换时被丢掉，
		// QQ 上看到的状态与实际工作长期不符。
		p.statusState.pending = st
		p.schedulePendingFlushLocked(minStatusInterval - time.Since(p.statusState.lastAt))
		p.statusState.mu.Unlock()
		return
	}
	p.statusState.pending = qqStatus{}
	p.statusState.lastAt = time.Now()
	p.statusState.mu.Unlock()

	if err := p.setSelfStatus(st); err != nil {
		p.statusState.mu.Lock()
		p.statusState.failCount++
		n := p.statusState.failCount
		p.statusState.mu.Unlock()
		// 首次失败 + 每 10 次记一条：NapCat 掉线时不能刷屏
		if n == 1 || n%10 == 0 {
			log.Printf("[qq] 设置在线状态 %s 失败（第 %d 次，已忽略；不影响工具调用）: %v",
				st.Name, n, err)
		}
		return
	}
	p.statusState.mu.Lock()
	p.statusState.applied = st
	p.statusState.failCount = 0
	p.statusState.mu.Unlock()
	log.Printf("[qq] 在线状态 → %s（status=%d ext=%d）", st.Name, st.Status, st.ExtStatus)
}

// restoreStatus 把状态恢复到"在线"（停机时用）。
//
// ★ 判据不能用 statusAutoEnabled：手动工具（qq_set_status）也能改状态，
// 而那时自动开关可能是关的。若以开关为准，一次手动设的“搬砖中”
// 会永久挂着。
//
// 正确判据是“**实际应用过什么**”：applied 有名字且不是在线，才需要恢复。
// 注意这里**不走 applyStatus**（它会检查开关、还会防抖）—— 停机时
// 必须无条件发一次，否则状态会错位。
func (p *Plugin) restoreStatus() {
	p.statusState.mu.Lock()
	// ★ 先取消待补发：否则停机后定时器还会醒来发一次 set_online_status，
	// 而此时插件进程/连接已在拆除 —— 要么报错刷日志，要么把刚恢复的
	// “在线”又改成别的状态。
	if p.statusState.pendingTimer != nil {
		p.statusState.pendingTimer.Stop()
		p.statusState.pendingTimer = nil
	}
	p.statusState.pending = qqStatus{}
	cur := p.statusState.applied
	p.statusState.mu.Unlock()
	if cur.Name == "" || cur.Name == qqStatusOnline.Name {
		return // 从未设过，或已经是在线
	}
	if err := p.setSelfStatus(qqStatusOnline); err != nil {
		log.Printf("[qq] 停机恢复在线状态失败（忽略）: %v", err)
		return
	}
	p.statusState.mu.Lock()
	p.statusState.applied = qqStatusOnline
	p.statusState.mu.Unlock()
	log.Printf("[qq] 停机：在线状态已从 %s 恢复为在线", cur.Name)
}

// statusForPhase 把内核阶段映射到 QQ 状态。
//
// 映射原则：**让对话者看出你在干什么**，而不是精确反映内部管线。
//
//	before_toolcall   → 搬砖中（正在调工具干活）
//	before_output     → 学习中（正在生成文字，QQ 没有"思考中"这个预置状态）
//	after_output      → 在线（一轮结束）
//	after_toolcall    → 不切（工具刚结束还要继续，频繁切换只会闪）
//
// ★ "思考中" 用 set_diy_online_status 的自定义文字更贴切，但 face_id 需要
// 图标表。这里先用预置的"学习中"，把 diy 留给显式工具调用。
func statusForPhase(phase string) (qqStatus, bool) {
	switch phase {
	case string(sdk.StageBeforeToolcall):
		return qqStatusWorking, true
	case string(sdk.StageBeforeOutput):
		return qqStatusStudy, true
	case string(sdk.StageAfterOutput):
		return qqStatusOnline, true
	default:
		return qqStatus{}, false
	}
}

// statusPhaseName 把 phase 常量转成人读名（日志/工具回显用）。
func statusPhaseName(phase string) string {
	switch phase {
	case string(sdk.StageBeforeToolcall):
		return "工具调用中"
	case string(sdk.StageBeforeOutput):
		return "生成回复中"
	case string(sdk.StageAfterOutput):
		return "一轮结束"
	default:
		return phase
	}
}

// normalizeStatusName 归一化工具传入的状态名。
func normalizeStatusName(s string) (qqStatus, bool) {
	k := strings.ToLower(strings.TrimSpace(s))
	st, ok := qqStatusByName[k]
	return st, ok
}

// ======== stage 钩子（失败永不阻塞）========
//
// 三个钩子的形状完全一样，只做一件事：防抖地应用一个状态。
//
// ★ 都 **return nil**，且**不重试**：在线状态是完全非致命的错误。
//   · 不返回 error ⇒ 内核不会因状态设置失败而中断工具调用或改写回复
//   · 不重试 ⇒ NapCat 掉线时不会每轮都打一次 HTTP + 打一条日志
//   · 失败只在 applyStatus 里记日志（首次 + 每 10 次），不刷屏
//
// 这与 typing（set_input_status）的处置一致：那套也从不因失败中断对话。

// ======== 工具 handler ========

// handleSetStatus 处理 qq_set_status：手动设预置在线状态。
//
// ★ 与自动切换共用 setSelfStatus，但不走防抖（手动操作应立即生效）。
func (p *Plugin) handleSetStatus(args map[string]interface{}) (interface{}, error) {
	name, _ := args["status"].(string)
	st, ok := normalizeStatusName(name)
	if !ok {
		return map[string]interface{}{
			"error": "未知状态名: " + name,
			"可选":    "online / busy / leave / dnd / hidden / working / study / music（或中文名）",
		}, nil
	}
	if err := p.setSelfStatus(st); err != nil {
		// 明确报错但不算致命：调用方拿到 error 字段可自行决定
		return map[string]interface{}{
			"error":  "设置失败: " + err.Error(),
			"status": st.Name,
			"note":   "NapCat 可能未运行；这不影响其他 QQ 工具",
		}, nil
	}
	// 手动设置要同步运行态，否则自动切换会以为"还是旧状态"而跳过
	p.statusState.mu.Lock()
	p.statusState.applied = st
	p.statusState.lastAt = time.Now()
	p.statusState.mu.Unlock()
	return map[string]interface{}{
		"status":     st.Name,
		"status_num": st.Status,
		"ext_status": st.ExtStatus,
	}, nil
}

// handleSetDiyStatus 处理 qq_set_diy_status：自定义状态（图标 + 文字）。
func (p *Plugin) handleSetDiyStatus(args map[string]interface{}) (interface{}, error) {
	faceID, _ := args["face_id"].(string)
	wording, _ := args["wording"].(string)
	if strings.TrimSpace(faceID) == "" || strings.TrimSpace(wording) == "" {
		return map[string]interface{}{"error": "face_id 与 wording 都必须非空"}, nil
	}
	if err := p.setSelfDiyStatus(faceID, wording); err != nil {
		return map[string]interface{}{
			"error": "设置自定义状态失败: " + err.Error(),
			"note":  "NapCat 可能未运行；这不影响其他 QQ 工具",
		}, nil
	}
	return map[string]interface{}{
		"ok":      true,
		"face_id": faceID,
		"wording": wording,
	}, nil
}

// ======== 合并后的 stage 入口 ========
//
// ★★ 为什么必须合并（2026-10-18 踩到的真坑）
//
// 工具链生成的 proc_main 把 stage handler 存在
// `stageHandlers map[string]sdk.StageHandler`（**stage 名为键**）：
//
//	func(stage sdk.Stage, handler sdk.StageHandler) {
//		stageHandlers[string(stage)] = handler   // ← 后者覆盖前者
//	}
//
// 所以一个插件**每个 stage 只能有一个 handler**。原先写成两行注册：
//
//	RegisterStage(StageBeforeToolcall, p.beforeToolcall, Global)   ← 权限门
//	RegisterStage(StageBeforeToolcall, p.stageStatusWorking, Global) ← 覆盖了它
//
// 后果不只是"状态不切换"——**QQ 的权限门被静默关掉了**。
// hmapdev 现在会在**构建期**对这类重复告警（开发者在构建输出里必然看见）。

// stagePreAction：即将调 LLM ⇒ 「学习中」。
//
// ## 为什么这里才是“等 LLM”（2026-10-18 用户定义语义）
//
//	学习中  = agent 正在**等待 LLM**
//	搬砖中  = 真正**执行 toolcall**
//	在线    = **空闲**
//
// 内核时序（task.go）：
//
//	pre_action ──→ stepLLM() ──→ ★等 LLM★ ──→ post_action
//
// `pre_action` 就在 `stepLLM()` **之前** —— 这是唯一能提前标记
// “接下来要等模型”的位置。之前把「学习中」挂在 `before_output` 上是错的：
// 那个时刻 LLM **已经返回**了，等的是输出而不是等模型。
func (p *Plugin) stagePreAction(ctx *sdk.StageContext) error {
	p.applyStatus(qqStatusStudy)
	return nil
}

// stageBeforeToolcall 把「权限门」与「状态切换」合成一次注册。
//
// 顺序：先跑权限门（它可能拒绝工具），再切状态。
// 即使权限门拒绝，状态也该显示"在干活"——拒绝也是 agent 在工作。
func (p *Plugin) stageBeforeToolcall(ctx *sdk.StageContext) error {
	if err := p.beforeToolcall(ctx); err != nil {
		return err
	}
	p.applyStatus(qqStatusWorking)
	return nil
}

// stageAfterToolcall：工具跑完了，接下来又要等 LLM 决定下一步 ⇒ 「学习中」。
//
// 不回到「学习中」的话，状态会在整个工具循环期间一直挂着「搬砖中」——
// 看起来像工具卡住了。
func (p *Plugin) stageAfterToolcall(ctx *sdk.StageContext) error {
	p.applyStatus(qqStatusStudy)
	return nil
}

// stageBeforeOutput：**不改在线状态**，只负责「对方正在输入中」的起点。
//
// ## 为什么这里不写状态
//
// `before_output` 是「LLM 已返回、即将输出」——既不是等 LLM 也不是跑工具。
// 在那里写「学习中」会让状态在收尾时继续挂着，看起来像永远在想。
func (p *Plugin) stageBeforeOutput(ctx *sdk.StageContext) error {
	// ★ 「对方正在输入中」的起点：**就在消息真正发出前**。
	//
	// ## 为什么是这里（而不是轮次边界）
	//
	// `before_output` 被两个语义共用：
	//
	//	emitResponse（eventloop）  = 一轮的回复
	//	本期 output_send__*（output.go）= 单次发送
	//
	// 「对方正在输入中」表达的是"我在给你打字"，它的正确时点就是
	// **这条消息发出之前** —— 也就是 output.go 那条路径。
	//
	// 这一期能拿到 `output_meta`（本次发送的 meta 原文），从中取 user_id
	// 就知道这条发给谁。群聊（有 group_id）不发指示：QQ 的「正在输入」
	// 只在私聊标题栏有意义。
	p.startTypingForOutput(ctx)
	// ★ 「对方正在输入中」的**起点**。
	//
	// ## 为什么是 before_output（而不是消息到达时）
	//
	// 旧写法在 HTTP 入口 startTyping —— 那时 agent 还没接受消息、可能在
	// 批量合并窗口里等、甚至可能被权限门拒掉；typingLoop 还有 30 秒超时，
	// agent 跑得慢就自己灭了，回复还在路上。
	//
	// `before_output` 在内核里**每轮只触发一次**（emitResponse），
	// 正好是“这一轮的天花板要出了”的时刻 —— 与打字指示器的语义完全对上。
	//
	// ## 为什么读 payload 透传的字段而不是 p.auth
	//
	// p.auth 是“最近一次输入”的全局状态，并发多个私聊会**串号**
	// （A 的输入指示可能打到 B 身上）。注入 payload 是**注入方声明**的、
	// 随这一轮走到 stageCtx.Extra 的不透明数据，不会串。
	p.startTypingForTurn(ctx)
	return nil
}

// stageAfterOutput 把「清理 auth 上下文」与「状态归位」合成一次注册。
//
// 顺序：先清理（原有语义，必须执行），再切状态（**force**，不受防抖）。
// 清理不能因状态失败而跳过 —— 它关系到下一轮的权限判定。
func (p *Plugin) stageAfterOutput(ctx *sdk.StageContext) error {
	// ★ after_output 被**两条路径**共用，必须区分（2026-10-18）：
	//
	//	output.go    单次 output_send__*：Extra 里有 output_meta
	//	eventloop    一轮的回复（turn 边界）：Extra 里是 input_source 等
	//
	// 区别决定两件事：
	//
	//  · 打字指示器：只有"单次发送"这条路径才有配对的 start；
	//    turn 边界那次是"这一轮说完了"，没有正在打的字要收。
	//  · 状态归位（在线）：**只能由 turn 边界触发**。一轮可能发多条消息，
	//    若每条发完都归位「在线」，状态会在干活期间反复跳回空闲 ——
	//    那正是"通过状态展示工作进度"失效的原因之一。
	if isSingleSendStage(ctx) {
		p.stopTypingAfterOutput(ctx)
		return nil
	}

	// —— 以下是 turn 边界 ——
	p.stopTypingForTurn(ctx)
	if err := p.afterOutputAuthContext(ctx); err != nil {
		return err
	}
	p.applyStatusForce(qqStatusOnline)
	return nil
}

// isSingleSendStage 报告这个 after_output 是不是"单次 output_send"那一期。
//
// 判据：内核在 output.go 的 Extra 里放了 output_meta（发送目标），
// 而 emitResponse 的 turn 边界没有这个键。
func isSingleSendStage(ctx *sdk.StageContext) bool {
	if ctx == nil {
		return false
	}
	ctx.RLock()
	defer ctx.RUnlock()
	_, has := ctx.Extra["output_meta"]
	return has
}

// ======== 「对方正在输入中」的按轮驱动 ========

// turnTypingUser 从这一轮的 stage 上下文里取出发输入指示的目标用户。
//
// 只认**注入方声明**的 payload 字段：它随这一轮走到 stageCtx.Extra，不会串号。
//
// 返回 0 表示这一轮不该发输入指示（群聊 / 非 QQ 来源 / 无身份声明）。
//
// ## 为什么不用 p.auth（2026-10-18）
//
// p.auth 是"最近一次输入"的全局状态。并发两个私聊时，A 的消息进来后
// 内核可能先跑 B 那一轮 —— before_output 读 p.auth 就拿到 B，
// A 的输入指示会**打到 B 身上**（用户看到的是"你在跟谁说话"的错乱）。
func (p *Plugin) turnTypingUser(ctx *sdk.StageContext) int64 {
	if ctx == nil {
		return 0
	}
	ctx.RLock()
	defer ctx.RUnlock()
	// 只处理自己引起的轮次：别的插件/通道的输出不该动我们的输入指示器。
	if src, _ := ctx.Extra["input_source"].(string); src != "" && src != p.name {
		return 0
	}
	// `qq_uid` 是 qq 自己在 InjectOptions.Payload 里声明的，内核原样透传。
	// 加 qq_ 前缀是为了避免将来与内核字段撞车。
	uid, ok := ctx.Extra["qq_uid"]
	if !ok {
		return 0
	}
	// 经 proc 协议（JSON）往返，数字可能变成 float64 或 json.Number。
	switch v := uid.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, err := v.Int64()
		if err == nil {
			return n
		}
	}
	return 0
}

// startTypingForOutput 在**一条消息真正发出前**发起「对方正在输入中」。
//
// ## 数据来源（2026-10-18 实测确定）
//
// 内核在 `output.go` 的 stageCtx.Extra 里放了：
//
//	output_channel  目标通道（"qq"）
//	output_meta     本次发送的 meta 原文（含 user_id / group_id）
//	output_type     text / image / file …
//
// 之前这里读的是 `qq_uid`（注入 payload 透传），但那条路径只在
// `emitResponse`（轮次边界）生效，而 agent 实际常用 `output_send__qq`
// 直接发消息 —— 那条路径原先 **Extra 是 nil**（实测 `extra_keys=0`），
// 所以打字指示器一次都没触发。
//
// ## 为什么只处理私聊
//
// QQ 的「正在输入中」显示在**私聊窗口标题栏**。群里没有这个语义
// （群聊显示"xx 正在输入"是干扰），所以有 group_id 就不发。
func (p *Plugin) startTypingForOutput(ctx *sdk.StageContext) {
	if ctx == nil {
		return
	}
	ctx.RLock()
	ch, _ := ctx.Extra["output_channel"].(string)
	rawMeta, _ := ctx.Extra["output_meta"].(string)
	ctx.RUnlock()

	if ch != p.name {
		return // 不是发往本通道
	}
	if rawMeta == "" {
		return
	}
	var m struct {
		GroupID int64 `json:"group_id"`
		UserID  int64 `json:"user_id"`
	}
	if err := json.Unmarshal([]byte(rawMeta), &m); err != nil {
		return
	}
	if m.GroupID > 0 {
		return // 群聊不发打字指示
	}
	if m.UserID <= 0 {
		return
	}
	p.startTyping(m.UserID)
	log.Printf("[qq] 对方正在输入中 → 开始（uid=%d，消息发出前）", m.UserID)
}

// stopTypingAfterOutput 在一条消息**发出后**结束「对方正在输入中」。
//
// 与 startTypingForOutput 同源（同一份 output_meta），保证成对。
func (p *Plugin) stopTypingAfterOutput(ctx *sdk.StageContext) {
	if ctx == nil {
		return
	}
	ctx.RLock()
	ch, _ := ctx.Extra["output_channel"].(string)
	rawMeta, _ := ctx.Extra["output_meta"].(string)
	ctx.RUnlock()

	if ch != p.name || rawMeta == "" {
		return
	}
	var m struct {
		GroupID int64 `json:"group_id"`
		UserID  int64 `json:"user_id"`
	}
	if err := json.Unmarshal([]byte(rawMeta), &m); err != nil {
		return
	}
	if m.GroupID > 0 || m.UserID <= 0 {
		return
	}
	p.stopTyping(m.UserID)
	log.Printf("[qq] 对方正在输入中 → 结束（uid=%d，消息已发出）", m.UserID)
}

// startTypingForTurn 在 before_output 发起「对方正在输入中」。
func (p *Plugin) startTypingForTurn(ctx *sdk.StageContext) {
	uid := p.turnTypingUser(ctx)
	if uid <= 0 {
		return
	}
	p.startTyping(uid)
	log.Printf("[qq] 对方正在输入中 → 开始（uid=%d，before_output）", uid)
}

// stopTypingForTurn 在 after_output 结束「对方正在输入中」。
//
// ★ 即使这一轮没发过指示（turnTypingUser 返回 0）也要兜一次：
// 上一轮若因超时/异常没停干净，这里能收尾。stopTyping 对不存在的条目是空操作。
func (p *Plugin) stopTypingForTurn(ctx *sdk.StageContext) {
	uid := p.turnTypingUser(ctx)
	if uid <= 0 {
		return
	}
	p.stopTyping(uid)
	log.Printf("[qq] 对方正在输入中 → 结束（uid=%d, after_output）", uid)
}
