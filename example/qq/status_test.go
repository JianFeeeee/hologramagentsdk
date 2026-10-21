package main

// QQ 在线状态切换的判据。
//
// ## 核心纪律（用户明确要求）
//
//	「失败不阻塞不重试，只写日志，因为是完全非致命错误」
//
// 这条比"状态设对了"更重要：NapCat 掉线是常态（重启、网络抖动、升级），
// 如果状态设置失败能让工具调用失败或让一轮回复中断，那才是真故障 ——
// 而它本来只是个装饰性功能。
//
// 所以判据的重点是**失败路径**，不是成功路径。

import (
	"net/http"
	"testing"
	"time"

	"github.com/JianFeeeee/hologramagentsdk/sdk"
)

// ★★★ 判据：NapCat 不可达时，三个 stage 钩子**仍返回 nil**（不阻塞）。
//
// 用 127.0.0.1:1（必拒绝）当 NapCat 地址，确保请求必然失败。
func TestStatusStageNeverBlocksOnFailure(t *testing.T) {
	p := &Plugin{
		name:      "qq",
		napcatURL: "http://127.0.0.1:1",
		// ★ 必须给 httpClient：产品里它由 Start() 赋值（plugin.go:470）。
		// 不给的话 napcat() 会在 nil client 上 panic —— 而真机不会，
		// 所以那只是测试构造漏项，不是产品缺陷。
		httpClient: &http.Client{Timeout: 2 * time.Second},
	}
	p.statusAutoEnabled.Store(true) // 强制开启，确保真的会尝试发请求
	ctx := &sdk.StageContext{}

	for _, c := range []struct {
		name string
		fn   func(*sdk.StageContext) error
	}{
		{"before_toolcall", p.stageBeforeToolcall},
		{"before_output", p.stageBeforeOutput},
		{"after_output", p.stageAfterOutput},
	} {
		if err := c.fn(ctx); err != nil {
			t.Fatalf("★ %s 返回了 error（%v）—— 状态设置是非致命错误，"+
				"不能让它阻塞工具调用或中断回复", c.name, err)
		}
	}
}

// ★★★ 判据：开关关闭时**完全不发请求**（默认关闭是常态）。
func TestStatusStageNoopWhenDisabled(t *testing.T) {
	p := &Plugin{name: "qq", napcatURL: "http://127.0.0.1:1"}
	// statusAutoEnabled 默认零值 false
	ctx := &sdk.StageContext{}
	if err := p.stageBeforeToolcall(ctx); err != nil {
		t.Fatalf("开关关闭时不应出错: %v", err)
	}
	p.statusState.mu.Lock()
	applied := p.statusState.applied
	p.statusState.mu.Unlock()
	if applied.Name != "" {
		t.Fatalf("开关关闭却应用了状态 %q —— 不该发任何请求", applied.Name)
	}
}

// ★★★ 判据：状态名归一化（工具参数）。
func TestNormalizeStatusName(t *testing.T) {
	ok := map[string]qqStatus{
		"online": qqStatusOnline, "busy": qqStatusBusy,
		"working": qqStatusWorking, "study": qqStatusStudy,
		"音乐": qqStatusMusic, "在线": qqStatusOnline, "忙碌": qqStatusBusy,
	}
	for in, want := range ok {
		got, found := normalizeStatusName(in)
		if !found {
			t.Fatalf("应识别 %q", in)
		}
		if got.Name != want.Name {
			t.Errorf("%q → %q，应为 %q", in, got.Name, want.Name)
		}
	}
	if _, found := normalizeStatusName("乱写的状态"); found {
		t.Error("未知状态名不该被识别（否则会发一个无效值给 NapCat）")
	}
}

// ★★★ 判据：阶段映射用**源码里的权威取值**，不是猜的。
//
// NapCat 的 SetOnlineStatus.ts 里 status=50 是「忙碌」、10+2023 是「搬砖中」、
// 10+1018 是「学习中」。我最初拿 status=11 试探，NapCat 也返回 retcode=0
// （它不校验），但那是**无效状态** —— 必须以取值表为准。
func TestStatusPhaseMappingUsesAuthoritativeValues(t *testing.T) {
	// 工具调用中 ⇒ 搬砖中（status=10, ext=2023）
	if st, ok := statusForPhase(string(sdk.StageBeforeToolcall)); !ok ||
		st.Status != 10 || st.ExtStatus != 2023 {
		t.Errorf("before_toolcall 应映射到 status=10/ext=2023（搬砖中），实际 %+v", st)
	}
	// 生成回复中 ⇒ 学习中（status=10, ext=1018）
	if st, ok := statusForPhase(string(sdk.StageBeforeOutput)); !ok ||
		st.Status != 10 || st.ExtStatus != 1018 {
		t.Errorf("before_output 应映射到 status=10/ext=1018（学习中），实际 %+v", st)
	}
	// 一轮结束 ⇒ 在线（status=10, ext=0）
	if st, ok := statusForPhase(string(sdk.StageAfterOutput)); !ok ||
		st.Status != 10 || st.ExtStatus != 0 {
		t.Errorf("after_output 应映射到 status=10/ext=0（在线），实际 %+v", st)
	}
	// 忙碌必须是 status=50（不是 11 或别的）
	if qqStatusBusy.Status != 50 || qqStatusBusy.ExtStatus != 0 {
		t.Errorf("忙碌应为 status=50/ext=0，实际 %+v", qqStatusBusy)
	}
}

// ★★★ 判据：手动工具遇到未知状态名时**不发请求**，只回错误。
func TestHandleSetStatusRejectsUnknown(t *testing.T) {
	p := &Plugin{name: "qq", napcatURL: "http://127.0.0.1:1"}
	out, err := p.handleSetStatus(map[string]interface{}{"status": "不存在的状态"})
	if err != nil {
		t.Fatalf("不该返回 error（应为结果里的 error 字段）: %v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok || m["error"] == nil {
		t.Fatalf("未知状态应给 error 字段，实际 %#v", out)
	}
}
