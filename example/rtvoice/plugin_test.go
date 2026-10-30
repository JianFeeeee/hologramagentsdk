package main

// 插件侧策略判据（plan.md 十一.四 空白 1 的"策略归插件"那一侧）。
//
// 这些判据钉的是**攒批与当前态**的语义，与内核的 rt 契约互相独立：
//
//	内核保证：新输入能打断旧输出（rtSlot 单槽）
//	本插件保证：喂进去的东西是**当前态**，不是事件流水

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// outRecorder 是 emit 的并发安全记录器。
//
// ★ 为什么不能把 []string 直接挂在测试里（初版就这么写，-race 当场抓到）：
// emit 会被**定时器 goroutine**（armWindow 的 AfterFunc）调用，而测试主
// goroutine 同时在读 —— 两个 goroutine 访问同一 slice，一个 append 一个
// slicecopy，是真实的数据竞争。普通 go test 看不出来，-race 才暴露。
type outRecorder struct {
	mu   sync.Mutex
	list []string
}

func (r *outRecorder) emit(s string) {
	r.mu.Lock()
	r.list = append(r.list, s)
	r.mu.Unlock()
}

func (r *outRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.list...)
}

// newTestPlugin 造一个不经 SDK 的实例（emit 换成记录器）。
func newTestPlugin(t *testing.T) (*Plugin, *outRecorder) {
	t.Helper()
	p := New("voice")
	rec := &outRecorder{}
	p.emit = rec.emit
	return p, rec
}

// TestPlugin_KeyOverwritesKeepOnlyLatest 是「当前态表」的核心判据：
// 同 key 多次更新，批次里**只有最新那个值**，旧值不得出现。
//
// 若把 table 改成 append 的列表，本判据会看到旧值 —— 那正是计划明令禁止的
// "积压即过期"（§一：rt 的处理表是 key → 最新值 的覆盖表）。
func TestPlugin_KeyOverwritesKeepOnlyLatest(t *testing.T) {
	p, out := newTestPlugin(t)

	p.Push(Event{Key: KeySpeech, Payload: "第一句"})
	p.Push(Event{Key: KeySpeech, Payload: "第二句"})
	p.Push(Event{Key: KeySpeech, Payload: "第三句"})
	p.Flush()

	got := out.all()
	if len(got) != 1 {
		t.Fatalf("应只送出 1 批，得到 %d 批：%v", len(got), got)
	}
	if !strings.Contains(got[0], "第三句") {
		t.Fatalf("批次里没有最新值：%q", got[0])
	}
	for _, stale := range []string{"第一句", "第二句"} {
		if strings.Contains(got[0], stale) {
			t.Fatalf("批次里出现过期内容 %q —— 当前态表退化成了事件流水\n%s", stale, got[0])
		}
	}
	// dropped 必须记账（覆盖发生了）。
	if st := p.Stats(); st.Dropped != 2 {
		t.Fatalf("dropped = %d，期望 2（两次覆盖）", st.Dropped)
	}
}

// TestPlugin_TurnpointClosesWindowImmediately 钉住拐点强制关窗：
// 说完（speech_done）到达时**立刻**送出，不等窗口到点。
//
// 若去掉拐点判断，要等满窗口（这里设 5s）才送出 —— 本判据会超时。
func TestPlugin_TurnpointClosesWindowImmediately(t *testing.T) {
	p, out := newTestPlugin(t)
	p.SetConfig(Config{Window: 5 * time.Second}) // 故意很长的窗口

	p.Push(Event{Key: KeySpeech, Payload: "我想问个事"})
	// 窗口还没到点，此时不该有批次。
	if got := out.all(); len(got) != 0 {
		t.Fatalf("窗口未到就送出了：%v", got)
	}

	// 拐点：说完 ⇒ 必须立刻关窗（含上一句 + 这个拐点本身）。
	start := time.Now()
	p.Push(Event{Key: KeySpeechDone, Payload: "done"})
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("拐点关窗耗时 %v —— 没有立刻送出（等窗口了）", elapsed)
	}

	got := out.all()
	if len(got) != 1 {
		t.Fatalf("拐点应送出 1 批，得到 %d", len(got))
	}
	if !strings.Contains(got[0], "我想问个事") {
		t.Fatalf("拐点批次丢了同批的语音内容：%q", got[0])
	}
	if st := p.Stats(); st.TurnpointBatches != 1 {
		t.Fatalf("turnpoint_batches = %d，期望 1（拐点可观测）", st.TurnpointBatches)
	}
}

// TestPlugin_EmptyValueDoesNotOverwrite 钉住"空值不覆盖"：
// "没话说"不该抹掉"刚才说了什么"。
func TestPlugin_EmptyValueDoesNotOverwrite(t *testing.T) {
	p, out := newTestPlugin(t)

	p.Push(Event{Key: KeySpeech, Payload: "有内容"})
	p.Push(Event{Key: KeySpeech, Payload: "   "}) // 空/空白：不覆盖
	p.Flush()

	got := out.all()
	if len(got) != 1 || !strings.Contains(got[0], "有内容") {
		t.Fatalf("空值抹掉了已有内容：%v", got)
	}
}

// TestPlugin_EmptyBatchIsNotEmitted 钉住：没有内容时**不投空消息**。
//
// 空批次会对 rtchild 产生一次无意义的打断（它要求"有输入就打断"），
// 那会让用户在静默期也看到模型空跑一轮。
func TestPlugin_EmptyBatchIsNotEmitted(t *testing.T) {
	p, out := newTestPlugin(t)
	if b := p.Flush(); b != nil {
		t.Fatalf("空表 Flush 返回了批次：%+v", b)
	}
	if got := out.all(); len(got) != 0 {
		t.Fatalf("空表也投了内容：%v", got)
	}
}

// TestPlugin_BatchIsClearedAfterEmit 钉住"送出后清表"：
// 否则下一批会把上一批内容再送一遍（重复打断）。
func TestPlugin_BatchIsClearedAfterEmit(t *testing.T) {
	p, out := newTestPlugin(t)

	p.Push(Event{Key: KeySpeech, Payload: "第一次"})
	p.Flush()
	p.Push(Event{Key: KeySpeaker, Payload: "张三"})
	p.Flush()

	got := out.all()
	if len(got) != 2 {
		t.Fatalf("应有 2 批，得到 %d：%v", len(got), got)
	}
	if strings.Contains(got[1], "第一次") {
		t.Fatalf("第二批带着上一批的内容（重复打断）：%q", got[1])
	}
	if !strings.Contains(got[1], "张三") {
		t.Fatalf("第二批丢了本次内容：%q", got[1])
	}
}

// TestPlugin_WindowFlushesAutomatically 钉住窗口到点自动送出。
func TestPlugin_WindowFlushesAutomatically(t *testing.T) {
	p, out := newTestPlugin(t)
	p.SetConfig(Config{Window: 40 * time.Millisecond})

	p.Push(Event{Key: KeySpeech, Payload: "窗口内的话"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(out.all()) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("窗口到点后没有自动送出")
}

// TestPlugin_BatchRenderingIsCurrentStateOnly 钉住渲染口径：
// 批次文本是"现在是什么样"，不是"然后…然后…"的流水账。
func TestPlugin_BatchRenderingIsCurrentStateOnly(t *testing.T) {
	b := Batch{Events: []Event{
		{Key: KeySpeaker, Payload: "张三"},
		{Key: KeySpeech, Payload: "帮我订明天的会议室"},
	}}
	text := b.Text()
	for _, want := range []string{"speaker: 张三", "speech: 帮我订明天的会议室"} {
		if !strings.Contains(text, want) {
			t.Fatalf("批次文本缺少 %q：\n%s", want, text)
		}
	}
	if strings.Contains(text, "[") || strings.Contains(text, "然后") {
		t.Fatalf("批次文本看起来像事件流水而不是当前态：\n%s", text)
	}
}

// TestPlugin_OutputIsTextOnly 钉住用户裁定 R6：
// 内核写进 outputch(rt) 的是**文本**，插件负责怎么展示。
func TestPlugin_OutputIsTextOnly(t *testing.T) {
	p, _ := newTestPlugin(t)

	var got []string
	p.SetOutputHook(func(s string) { got = append(got, s) })

	p.EmitOutput("你好")
	p.EmitOutput("")    // 空串：不触发下游
	p.EmitOutput("   ") // 空白：不触发下游
	p.EmitOutput("再见")

	if len(got) != 2 || got[0] != "你好" || got[1] != "再见" {
		t.Fatalf("下游收到的文本流不对：%v", got)
	}
}

// TestPlugin_DegradedCounting 钉住异常降级的记账（计划 §3.4）。
func TestPlugin_DegradedCounting(t *testing.T) {
	p, _ := newTestPlugin(t)
	p.SetConfig(Config{DegradedAfter: 3})

	p.MarkDegraded()
	p.MarkDegraded()
	if st := p.Stats(); st.Degraded != 2 {
		t.Fatalf("degraded = %d，期望 2", st.Degraded)
	}
	p.MarkHealthy()
	if st := p.Stats(); st.Degraded != 0 {
		t.Fatalf("MarkHealthy 后 degraded = %d，期望 0", st.Degraded)
	}
	// 降级不改变"只认当前态"的语义：降级后推同 key 仍然覆盖。
	p.Push(Event{Key: KeySpeech, Payload: "旧"})
	p.Push(Event{Key: KeySpeech, Payload: "新"})
	if st := p.Stats(); st.Dropped == 0 {
		t.Fatal("降级状态下覆盖语义丢失（当前态表不再生效）")
	}
}
