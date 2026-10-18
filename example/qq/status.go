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
	"log"
	"strings"
	"sync"
	"time"

	"github.com/JianFeeeee/homeagentsdk/sdk"
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

// statusState 是自动状态切换的运行态。
type statusState struct {
	mu sync.Mutex
	// applied 是**最近一次成功应用**的状态（空 Name = 还没设过）。
	applied qqStatus
	// lastAt 是最近一次实际发请求的时间（防抖用）。
	lastAt time.Time
	// failCount 统计连续失败次数（只在首次与每 10 次记日志，避免刷屏）。
	failCount int
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
	if !p.statusAutoEnabled.Load() {
		return
	}
	p.statusState.mu.Lock()
	if p.statusState.applied.Name == st.Name {
		p.statusState.mu.Unlock()
		return
	}
	if time.Since(p.statusState.lastAt) < 2*time.Second {
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

func (p *Plugin) stageStatusWorking(ctx *sdk.StageContext) error {
	p.applyStatus(qqStatusWorking)
	return nil
}

func (p *Plugin) stageStatusThinking(ctx *sdk.StageContext) error {
	// "生成回复中"用「学习中」的预置状态：QQ 没有"思考中"这个预置项。
	// 想要精确文字可调 qq_set_diy_status 工具（自定义 face_id + wording）。
	p.applyStatus(qqStatusStudy)
	return nil
}

func (p *Plugin) stageStatusOnline(ctx *sdk.StageContext) error {
	p.applyStatus(qqStatusOnline)
	return nil
}

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
