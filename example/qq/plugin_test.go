package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JianFeeeee/homeagentsdk/sdk"
)

func newPermissionTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	instance, err := NewPluginFactory("qq", nil)
	if err != nil {
		t.Fatal(err)
	}
	return instance.(*Plugin)
}

func toolCallContext(name string, args map[string]interface{}) *sdk.StageContext {
	return &sdk.StageContext{ToolCalls: []sdk.ToolCall{{Name: name, Arguments: args}}}
}

func TestOwnerBypassesQQPermissionBoundary(t *testing.T) {
	p := newPermissionTestPlugin(t)
	p.auth = qqAuthContext{Active: true, Owner: true, UserID: 2198972886}
	ctx := toolCallContext("calendar_list", nil)
	if err := p.beforeToolcall(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Response != nil {
		t.Fatalf("owner call rejected: %s", *ctx.Response)
	}
}

func TestPrivateResourceCannotBeAllowlisted(t *testing.T) {
	p := newPermissionTestPlugin(t)
	p.privateToolAllowlist = append(p.privateToolAllowlist, "calendar_*")
	p.auth = qqAuthContext{Active: true, UserID: 10001}
	ctx := toolCallContext("calendar_list", nil)
	if err := p.beforeToolcall(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Response == nil || !strings.Contains(*ctx.Response, "私人资源工具") {
		t.Fatalf("expected private-resource denial, got %#v", ctx.Response)
	}
}

func TestNonOwnerQQHistoryIsScopedToCurrentGroup(t *testing.T) {
	p := newPermissionTestPlugin(t)
	p.auth = qqAuthContext{Active: true, MessageID: 88, UserID: 10001, GroupID: 20002, IsGroup: true}

	ctx := toolCallContext("qq_get_history", map[string]interface{}{"group_id": int64(20003)})
	if err := p.beforeToolcall(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Response == nil || !strings.Contains(*ctx.Response, "当前 QQ 会话") {
		t.Fatalf("cross-group history not rejected: %#v", ctx.Response)
	}

	ctx = toolCallContext("qq_get_history", map[string]interface{}{"group_id": int64(20002)})
	if err := p.beforeToolcall(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Response != nil {
		t.Fatalf("current-group history rejected: %s", *ctx.Response)
	}
}

func TestUnmatchedQQInputIsDowngraded(t *testing.T) {
	p := newPermissionTestPlugin(t)
	p.auth = qqAuthContext{Active: true, Owner: true, UserID: 2198972886}
	ctx := &sdk.StageContext{
		RawMessage: "来自未知事件(message_id=404)",
		Extra:      map[string]interface{}{"input_source": "qq"},
	}
	if err := p.onInputAuthContext(ctx); err != nil {
		t.Fatal(err)
	}
	if !p.auth.Active || p.auth.Owner || p.auth.UserID != 0 {
		t.Fatalf("unmatched input reused prior privilege: %+v", p.auth)
	}
}

func TestDuplicateQQOutputIsStopped(t *testing.T) {
	p := newPermissionTestPlugin(t)
	p.maxDuplicateSend = 1
	p.auth = qqAuthContext{Active: true, Owner: true, UserID: 2198972886}
	args := map[string]interface{}{"payload": "same", "type": "text", "meta": `{"user_id":123}`}

	ctx := toolCallContext("output_send__qq", args)
	if err := p.beforeToolcall(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Response != nil {
		t.Fatalf("first send rejected: %s", *ctx.Response)
	}

	ctx = toolCallContext("output_send__qq", args)
	if err := p.beforeToolcall(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Response == nil || !strings.Contains(*ctx.Response, "循环保险") {
		t.Fatalf("duplicate send not stopped: %#v", ctx.Response)
	}
}

func TestGroupAndUserRouteAddsLeadingMention(t *testing.T) {
	var path string
	var request map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","retcode":0,"data":{"message_id":1}}`))
	}))
	defer server.Close()

	p := newPermissionTestPlugin(t)
	p.napcatURL = server.URL
	p.httpClient = server.Client()
	_, err := p.handleChannelOutput(map[string]interface{}{
		"payload": "hello",
		"type":    "text",
		"meta":    `{"group_id":20002,"user_id":10001}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/send_group_msg" {
		t.Fatalf("path=%q, want /send_group_msg", path)
	}
	segments, ok := request["message"].([]interface{})
	if !ok || len(segments) < 2 {
		t.Fatalf("message is not a segment array: %#v", request["message"])
	}
	mention, _ := segments[0].(map[string]interface{})
	data, _ := mention["data"].(map[string]interface{})
	if mention["type"] != "at" || data["qq"] != "10001" {
		t.Fatalf("leading mention=%#v", mention)
	}
}

// 回归：循环保险曾按“总数”拦截，导致参数不同且必需的调用被误杀。
// 现在只拦参数完全相同的重复调用。
func TestDistinctQQOutputsAreNotTreatedAsDuplicates(t *testing.T) {
	p := newPermissionTestPlugin(t)
	p.auth = qqAuthContext{Active: true, Owner: true, UserID: 2198972886}
	// maxDuplicateSend 默认 1：同一条消息重复才会被拦，不同消息必须全部放行。
	for i := 0; i < 5; i++ {
		ctx := toolCallContext("output_send__qq", map[string]interface{}{
			"payload": fmt.Sprintf("message-%d", i),
			"type":    "text",
			"meta":    `{"user_id":123}`,
		})
		if err := p.beforeToolcall(ctx); err != nil {
			t.Fatal(err)
		}
		if ctx.Response != nil {
			t.Fatalf("distinct message %d was blocked: %s", i, *ctx.Response)
		}
	}
}

func TestDistinctNecessaryToolCallsAreNotBlocked(t *testing.T) {
	p := newPermissionTestPlugin(t)
	p.auth = qqAuthContext{Active: true, Owner: true, UserID: 2198972886}
	// 旧实现 maxQQToolCalls=32 会在第 33 个不同参数的必需调用处误拦。
	for i := 0; i < 50; i++ {
		ctx := toolCallContext("cmd_run", map[string]interface{}{"command": fmt.Sprintf("cmd-%d", i)})
		if err := p.beforeToolcall(ctx); err != nil {
			t.Fatal(err)
		}
		if ctx.Response != nil {
			t.Fatalf("necessary tool call %d was blocked: %s", i, *ctx.Response)
		}
	}
}

func TestZeroLimitsMeanUnlimited(t *testing.T) {
	p := newPermissionTestPlugin(t)
	p.maxQQOutputCalls = 0
	p.maxDuplicateSend = 0
	p.maxQQToolCalls = 0
	p.auth = qqAuthContext{Active: true, Owner: true, UserID: 2198972886}
	for i := 0; i < 30; i++ {
		ctx := toolCallContext("output_send__qq", map[string]interface{}{
			"payload": "same-content",
			"type":    "text",
			"meta":    `{"user_id":123}`,
		})
		if err := p.beforeToolcall(ctx); err != nil {
			t.Fatal(err)
		}
		if ctx.Response != nil {
			t.Fatalf("0 should mean unlimited, blocked at %d: %s", i, *ctx.Response)
		}
	}
}

// 降权（本轮无法精确匹配可信 OneBot 事件 ⇒ auth={Active:true}、无 peer、非 owner）时，
// **输出仍必须放行**：发到哪个会话由 agent 自己给的 meta 决定，
// 不该被「当前会话身份」挡住。现场：被子的中断唤醒的一轮里，父带齐 meta 也发不出去
// （报「可信 QQ 会话身份不完整」）。
//
// 反之，**读取类**工具在降权时仍受当前会话限制 —— 那才是真的不能跨会话读。
func TestDowngradedAuthStillAllowsQQOutput(t *testing.T) {
	p := newPermissionTestPlugin(t)
	p.auth = qqAuthContext{Active: true}
	p.privateToolAllowlist = []string{"output_send__qq", "qq_get_history"}
	p.groupToolAllowlists = map[int64][]string{0: {"output_send__qq", "qq_get_history"}}

	ctx := toolCallContext("output_send__qq", map[string]interface{}{
		"payload": "带齐 meta 的主动发送",
		"type":    "text",
		"meta":    `{"user_id":2198972886}`,
	})
	if err := p.beforeToolcall(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Response != nil {
		t.Fatalf("降权时输出被拒: %s", *ctx.Response)
	}

	ctx2 := toolCallContext("qq_get_history", map[string]interface{}{"group_id": 1027993713})
	if err := p.beforeToolcall(ctx2); err != nil {
		t.Fatal(err)
	}
	if ctx2.Response == nil || !strings.Contains(*ctx2.Response, "可信 QQ 会话身份不完整") {
		t.Fatalf("读取类工具在降权时应被当前会话限制挡住: %#v", ctx2.Response)
	}
}

// ---- 消息合并（debounce）----

// collectInterrupts 用注入钩子收集中断文本（避免测试依赖真实 SDK）。
func collectInterrupts(p *Plugin) *[]string {
	got := []string{}
	p.injectHook = func(s, _ string) { got = append(got, s) }
	return &got
}

func TestConsecutiveMessagesFromSameSenderAreBatched(t *testing.T) {
	p := newPermissionTestPlugin(t)
	got := collectInterrupts(p)
	p.batchWindow = 20 * time.Millisecond
	p.batchMax = time.Second

	for i := 0; i < 3; i++ {
		p.enqueueInterrupt("private", 10001, 0, int64(100+i), "小明", "单条", false, false)
	}
	time.Sleep(120 * time.Millisecond)

	if len(*got) != 1 {
		t.Fatalf("同一发送者连发 3 条应合并成 1 次中断，实际 %d 次: %#v", len(*got), *got)
	}
	if !strings.Contains((*got)[0], "3 条消息") {
		t.Fatalf("合并中断应说明一共几条，实际: %s", (*got)[0])
	}
	// 三个 message_id 都要带上，模型才能取全
	for _, id := range []string{"100", "101", "102"} {
		if !strings.Contains((*got)[0], id) {
			t.Fatalf("合并中断漏了 message_id=%s: %s", id, (*got)[0])
		}
	}
}

func TestDifferentSendersAreNotBatchedTogether(t *testing.T) {
	p := newPermissionTestPlugin(t)
	got := collectInterrupts(p)
	p.batchWindow = 20 * time.Millisecond
	p.batchMax = time.Second

	p.enqueueInterrupt("private", 10001, 0, 1, "小明", "a", false, false)
	p.enqueueInterrupt("private", 10002, 0, 2, "小红", "b", false, false)
	time.Sleep(120 * time.Millisecond)

	if len(*got) != 2 {
		t.Fatalf("不同发送者不该合并，应有 2 次中断，实际 %d: %#v", len(*got), *got)
	}
}

func TestBatchWindowZeroFallsBackToPerMessage(t *testing.T) {
	p := newPermissionTestPlugin(t)
	got := collectInterrupts(p)
	p.batchWindow = 0

	for i := 0; i < 3; i++ {
		p.enqueueInterrupt("private", 10001, 0, int64(i), "小明", "原文", false, false)
	}
	if len(*got) != 3 {
		t.Fatalf("关闭合并时应逐条投递（3 次），实际 %d: %#v", len(*got), *got)
	}
}

func TestSingleMessageKeepsOriginalText(t *testing.T) {
	p := newPermissionTestPlugin(t)
	got := collectInterrupts(p)
	p.batchWindow = 20 * time.Millisecond
	p.batchMax = time.Second

	p.enqueueInterrupt("group", 10001, 20002, 7, "小明", "单条原文", true, false)
	time.Sleep(120 * time.Millisecond)

	if len(*got) != 1 || (*got)[0] != "单条原文" {
		t.Fatalf("单条消息应沿用原文（含所有者前缀），实际 %#v", *got)
	}
}

// Bot 所有者/管理员的消息给 L2，普通人的给 L1 —— 否则所有者的话会被路人
// 的 L1 闲聊抢占/挤到队尾。
func TestOwnerMessagesGetHigherInterruptLevel(t *testing.T) {
	p := newPermissionTestPlugin(t)
	got := []string{}
	p.injectHook = func(text, level string) { got = append(got, text+"|"+level) }
	p.batchWindow = 20 * time.Millisecond
	p.batchMax = time.Second

	p.enqueueInterrupt("private", 1, 0, 1, "owner", "owner-msg", true, false)
	p.enqueueInterrupt("private", 2, 0, 2, "someone", "other-msg", false, false)
	time.Sleep(120 * time.Millisecond)

	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "owner-msg|L2") {
		t.Fatalf("所有者消息应为 L2，实际 %q", joined)
	}
	if !strings.Contains(joined, "other-msg|L1") {
		t.Fatalf("普通人消息应为 L1，实际 %q", joined)
	}
}

// 身份必须绑在帧上：中断抢占当前轮、中断轮收尾清空插件全局身份之后，
// 外层轮被恢复（resumeTask 复用同一帧、不重跑 onInput）时权限门不能整体失效。
func TestAuthSurvivesInterruptPreemptionOfAnotherTurn(t *testing.T) {
	p := newPermissionTestPlugin(t)

	// 中断轮（Bot 所有者）跑完：afterOutput 会清掉插件全局身份。
	inner := &sdk.StageContext{Extra: map[string]interface{}{
		qqAuthExtraKey: qqAuthContext{Active: true, Owner: true, UserID: 2198972886},
	}}
	if err := p.afterOutputAuthContext(inner); err != nil {
		t.Fatal(err)
	}
	if p.auth.Active {
		t.Fatal("收尾后插件全局身份应为空（复现恢复前状态）")
	}

	// 外层轮（非所有者群成员）恢复后继续调工具：仍须按非所有者拦下私人资源工具。
	frame := &sdk.StageContext{
		Extra:     map[string]interface{}{qqAuthExtraKey: qqAuthContext{Active: true, UserID: 10001, GroupID: 20002, IsGroup: true}},
		ToolCalls: []sdk.ToolCall{{Name: "calendar_list"}},
	}
	if err := p.beforeToolcall(frame); err != nil {
		t.Fatal(err)
	}
	if frame.Response == nil || !strings.Contains(*frame.Response, "私人资源工具") {
		t.Fatalf("中断恢复后权限门失效（整体放行）: %#v", frame.Response)
	}
}

// 运行中到达的新消息会改写插件全局身份；正在跑的那一轮必须不受影响。
func TestMidTurnMessageDoesNotChangeRunningTurnAuth(t *testing.T) {
	p := newPermissionTestPlugin(t)

	frame := &sdk.StageContext{
		Extra:     map[string]interface{}{qqAuthExtraKey: qqAuthContext{Active: true, Owner: true, UserID: 2198972886}},
		ToolCalls: []sdk.ToolCall{{Name: "calendar_list"}},
	}
	// 路人的群消息在所有者轮运行中到达。
	p.activateAuthContext(4242, 10001, 20002, true)
	if p.auth.Owner {
		t.Fatal("到达事件应改写全局身份（复现场景）")
	}

	if err := p.beforeToolcall(frame); err != nil {
		t.Fatal(err)
	}
	if frame.Response != nil {
		t.Fatalf("在跑的所有者轮被到达消息篡改: %s", *frame.Response)
	}
}

// 合并中断正文里的整批 message_id 都要消费掉，并在帧上绑定身份。
func TestBatchInterruptConsumesAllMessageIDs(t *testing.T) {
	p := newPermissionTestPlugin(t)
	p.authByMessageID = map[int64]qqAuthContext{
		100: {Active: true, Owner: true, UserID: 2198972886},
		101: {Active: true, Owner: true, UserID: 2198972886},
	}
	ctx := &sdk.StageContext{
		RawMessage: "来自「老板」的私聊短时间内连续发来 2 条消息(message_id=100,101, user_id=2198972886)。",
		Extra:      map[string]interface{}{"input_source": "qq"},
	}
	if err := p.onInputAuthContext(ctx); err != nil {
		t.Fatal(err)
	}
	if !p.auth.Owner {
		t.Fatalf("合并中断未恢复所有者身份: %+v", p.auth)
	}
	if len(p.authByMessageID) != 0 {
		t.Fatalf("同批 message_id 未全部清理: %v", p.authByMessageID)
	}
	if auth, ok := authOnFrame(ctx); !ok || !auth.Owner {
		t.Fatalf("身份未绑定到帧上: %+v ok=%v", auth, ok)
	}
}

// 非 QQ 来源（webui/timer 等）的帧上绑空身份：权限门对这些轮整体关闭。
func TestNonQQFrameBindsInactiveAuth(t *testing.T) {
	p := newPermissionTestPlugin(t)
	p.auth = qqAuthContext{Active: true, Owner: true, UserID: 2198972886}

	ctx := &sdk.StageContext{
		RawMessage: "webui 里的提问",
		Extra:      map[string]interface{}{"input_source": "webui"},
		ToolCalls:  []sdk.ToolCall{{Name: "calendar_list"}},
	}
	if err := p.onInputAuthContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.beforeToolcall(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Response != nil {
		t.Fatalf("非 QQ 轮不应被 QQ 权限门拦: %s", *ctx.Response)
	}
}

// ===== 绑帧跨 proc 桥的判据（2026-10-12 shm v2 fExtraBag）=====
//
// 背景：内核 shm 桥 v2 之前只透传 4 个具名 Extra 键，插件自建键（qq_auth）
// 在 stage 之间静默丢失 → before_toolcall 永远回落可变全局身份 → 绑帧失效。
// v2 起自建键装 fExtraBag 以 JSON 往返；本组判据锁定插件侧解码契约：
// 数字统一 float64、解不出完整身份按未绑定处理（宁回落全局也不猜）。

// 判据 1：身份 struct → JSON → map（模拟 proc 桥往返）→ authOnFrame 重建无损。
func TestAuthOnFrameMapRoundTripFullFidelity(t *testing.T) {
	src := qqAuthContext{
		Active: true, Owner: true, MessageID: 1125391573, UserID: 2198972886,
		GroupID: 0, IsGroup: false, Generation: 42,
	}
	raw, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ctx := &sdk.StageContext{Extra: map[string]interface{}{qqAuthExtraKey: m}}
	got, ok := authOnFrame(ctx)
	if !ok {
		t.Fatal("map 形态身份应能重建（proc 桥语义），got !ok")
	}
	if got != src {
		t.Fatalf("往返失真:\n got %+v\nwant %+v", got, src)
	}
}

// 判据 2：降权身份往返后必须保持降权（防“往返即提权”）。
func TestAuthOnFrameMapRoundTripPreservesDemotion(t *testing.T) {
	src := qqAuthContext{Active: true, Owner: false, UserID: 3162251442, IsGroup: true, GroupID: 258962824}
	raw, _ := json.Marshal(src)
	var m map[string]interface{}
	_ = json.Unmarshal(raw, &m)
	ctx := &sdk.StageContext{Extra: map[string]interface{}{qqAuthExtraKey: m}}
	got, ok := authOnFrame(ctx)
	if !ok {
		t.Fatal("降权身份应能重建")
	}
	if got.Owner {
		t.Fatal("owner=false 经往返绝不能变 true（提权=安全漏洞）")
	}
	if !got.IsGroup || got.GroupID != 258962824 {
		t.Fatalf("群身份字段失真: %+v", got)
	}
}

// 判据 3：半截身份（缺键）按未绑定处理 —— 宁回落全局也不猜。
// 每个必填键都单独缺失一次：任何一个被放过，降权/身份锚定就有一条缝。
func TestAuthOnFramePartialMapIsRejected(t *testing.T) {
	full := map[string]interface{}{
		"active": true, "owner": true, "message_id": float64(1),
		"user_id": float64(1), "group_id": float64(0), "is_group": false,
	}
	for _, missing := range []string{"active", "owner", "message_id", "user_id", "group_id", "is_group"} {
		m := map[string]interface{}{}
		for k, v := range full {
			if k != missing {
				m[k] = v
			}
		}
		ctx := &sdk.StageContext{Extra: map[string]interface{}{qqAuthExtraKey: m}}
		if _, ok := authOnFrame(ctx); ok {
			t.Fatalf("缺 %s: 半截身份应按未绑定处理（回落全局），却解析成功", missing)
		}
	}
	// 类型错同样拒绝（float64 约定被破坏 = 旧桥/脏数据）。
	bad := map[string]interface{}{
		"active": true, "owner": true, "message_id": float64(1),
		"user_id": "10001", "group_id": float64(0), "is_group": false,
	}
	ctx := &sdk.StageContext{Extra: map[string]interface{}{qqAuthExtraKey: bad}}
	if _, ok := authOnFrame(ctx); ok {
		t.Fatal("user_id 类型错应拒绝")
	}
}

// 判据 4：帧上身份优先于全局身份 —— 绑帧存在时全局只作兜底。
func TestFrameIdentityBeatsGlobalIdentity(t *testing.T) {
	p := newPermissionTestPlugin(t)
	p.auth = qqAuthContext{Active: true, Owner: true, UserID: 2198972886}
	ctx := &sdk.StageContext{
		Extra: map[string]interface{}{
			qqAuthExtraKey: qqAuthContext{Active: true, Owner: false, UserID: 3162251442, IsGroup: true, GroupID: 258962824},
		},
		ToolCalls: []sdk.ToolCall{{Name: "memory_commit"}},
	}
	p.beforeToolcall(ctx)
	if ctx.Response == nil || !strings.Contains(*ctx.Response, "拒绝") {
		t.Fatalf("帧上非 owner 调硬私工具应被拒（全局 owner 不得放行）: %v", ctx.Response)
	}

	// 反向：全局非 owner，帧上 owner → 放行（owner 被误拒的原始故障）。
	p2 := newPermissionTestPlugin(t)
	p2.auth = qqAuthContext{Active: true, Owner: false, UserID: 3162251442}
	ctx2 := &sdk.StageContext{
		Extra: map[string]interface{}{
			qqAuthExtraKey: qqAuthContext{Active: true, Owner: true, UserID: 2198972886},
		},
		ToolCalls: []sdk.ToolCall{{Name: "memory_commit"}},
	}
	p2.beforeToolcall(ctx2)
	if ctx2.Response != nil {
		t.Fatalf("帧上 owner 调硬私工具应放行（2026-10-12 owner 被误拒的故障场景）: %s", *ctx2.Response)
	}
}

// ── napcat 响应判定与超时语义（2026-10-13 真实故障的判据）────────────

// 真实故障：QQ 发送经 NapCat 的 NodeIKernelMsgService 慢时响应头 30s 回不来，
// 旧实现共用 30s 客户端 ⇒ 消息**已发出**（NapCat 已返回 message_id、用户真收到）
// 却报 "context deadline exceeded"，模型据此重试。
//
// 判据（不变量，不是"函数被调过"）：
//  1. OneBot 失败响应（retcode!=0 / status=failed）必须报错
//  2. OneBot 成功响应（retcode=0,status=ok）必须放行
//     旧实现不校验 retcode ⇒ NapCat 明确报失败（data:null + sendMsg Timeout）
//     也会被当成发送成功
//  3. 发送类端点必须走 sendClient（更长超时），查询类走 httpClient
//  4. 超时错误必须带"可能已送达，不要盲目重发"的提示
//     （只说"失败"会诱发模型重发 → 重复消息）

func TestOneBotError_FailedResponses(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool // 期望判为失败
	}{
		{"retcode 非0", `{"status":"failed","retcode":100,"message":"bad","wording":""}`, true},
		{"retcode 非0 带 wording", `{"status":"failed","retcode":1400,"wording":"群号不存在","data":null}`, true},
		{"status failed 无 retcode", `{"status":"failed","message":"oops"}`, true},
		{"status error", `{"status":"error","message":"boom"}`, true},
		{"成功 retcode0", `{"status":"ok","retcode":0,"data":{"message_id":713138214},"wording":""}`, false},
		{"非对象（数组）", `[{"a":1}]`, false},
		{"无成败字段", `{"data":{"message_id":1}}`, false},
	}
	for _, c := range cases {
		err := oneBotError(json.RawMessage(c.body))
		if c.want && err == nil {
			t.Errorf("%s：应判失败却放行 → NapCat 报失败会被当成发送成功", c.name)
		}
		if !c.want && err != nil {
			t.Errorf("%s：应放行却报错（%v）→ 会把正常结果误报失败，诱发重复发送", c.name, err)
		}
	}
}

// 发送类端点必须真的走 sendClient（更长超时），而不是查询类的 httpClient。
//
// ★ 为何要真的验证路由（变异测试实测发现）：只断言两个客户端的 Timeout
//
//	字段值，把 napcat() 里的路由分支删掉判据照样全绿——判据必须观测
//	"哪个客户端实际发了请求"，否则"接线断了"测不出来。
func TestNapcatSendUsesSendClient(t *testing.T) {
	var sendHits, otherHits atomic.Int64
	sendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sendHits.Add(1)
		_, _ = w.Write([]byte(`{"status":"ok","retcode":0,"data":{"message_id":1}}`))
	}))
	defer sendSrv.Close()
	otherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		_, _ = w.Write([]byte(`{"status":"ok","retcode":0}`))
	}))
	defer otherSrv.Close()

	p := newPermissionTestPlugin(t)
	p.napcatURL = otherSrv.URL      // 查询类客户端指向这里
	p.sendClient = sendSrv.Client() // 发送类客户端指向 sendSrv
	p.sendClient.Timeout = 120 * time.Second
	// 让 sendClient 的请求打到 sendSrv：直接改 napcatURL 不可行（两者共用），
	// 所以用 Transport 层重写 host —— 只有 sendClient 带这个 Transport。
	p.sendClient.Transport = rewriteHostTransport(sendSrv.URL)
	p.httpClient = otherSrv.Client()
	p.httpClient.Timeout = 30 * time.Second

	if _, err := p.napcat("send_private_msg", map[string]interface{}{"user_id": 1, "message": "x"}); err != nil {
		t.Fatalf("send_private_msg 应成功: %v", err)
	}
	if sendHits.Load() != 1 {
		t.Errorf("发送类端点未走 sendClient（sendHits=%d）—— 30s 客户端会把已发出的消息报成超时失败", sendHits.Load())
	}
	if otherHits.Load() != 0 {
		t.Errorf("发送类端点误用了 httpClient（otherHits=%d）", otherHits.Load())
	}
}

// rewriteHostTransport 返回一个把所有请求重定向到 baseURL 的 Transport。
// 仅测试用：让"哪个 client 发的"可观测。
func rewriteHostTransport(baseURL string) http.RoundTripper {
	target, _ := url.Parse(baseURL)
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		return http.DefaultTransport.RoundTrip(req)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// 超时错误必须写明「可能已送达，不要盲目重发」。
func TestNapcatTimeoutErrorIsActionable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond) // 故意超过客户端超时
		_, _ = w.Write([]byte(`{"status":"ok","retcode":0}`))
	}))
	defer srv.Close()

	p := newPermissionTestPlugin(t)
	p.napcatURL = srv.URL
	p.httpClient = &http.Client{Timeout: 50 * time.Millisecond}
	p.sendClient = &http.Client{Timeout: 50 * time.Millisecond}

	_, err := p.napcat("send_private_msg", map[string]interface{}{"user_id": 1, "message": "x"})
	if err == nil {
		t.Fatal("超时应报错")
	}
	if !errors.Is(err, errNapcatTimeout) {
		t.Errorf("超时应包装 errNapcatTimeout，实为 %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "不要盲目重发") {
		t.Errorf("超时错误必须提示不要盲目重发（否则模型会重发→重复消息），实为: %s", msg)
	}
	if !strings.Contains(msg, "可能已送达") {
		t.Errorf("超时错误必须说明可能已送达，实为: %s", msg)
	}
}

// napcat() 端到端必须把 OneBot 失败响应转成 error。
//
// ★ 为何单测 oneBotError 不够（变异测试实测发现）：把 napcat() 里的
//
//	oneBotError 调用删掉，只测 oneBotError 的判据照样全绿——判据必须
//	穿过真实调用链，否则"接线断了"测不出来（正是本仓踩过的同款坑：
//	单测全绿而生产链路断开）。
func TestNapcatConvertsOneBotFailureToError(t *testing.T) {
	for _, body := range []string{
		`{"status":"failed","retcode":100,"wording":"群号不存在"}`,
		`{"status":"failed","retcode":1400,"message":"send failed"}`,
		`{"data":null,"echo":null,"message":"Timeout: NTEvent serviceAndMethod:NodeIKernelMsgService/sendMsg timed out","retcode":-1,"status":"failed"}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		p := newPermissionTestPlugin(t)
		p.napcatURL = srv.URL
		p.httpClient = srv.Client()
		p.sendClient = srv.Client()

		_, err := p.napcat("send_private_msg", map[string]interface{}{"user_id": 1, "message": "x"})
		srv.Close()
		if err == nil {
			t.Errorf("OneBot 失败响应必须转成 error，否则「发送失败」会被当成成功: %s", body)
		}
	}

	// 对照：成功响应必须放行（否则正常发送被误报失败 → 模型重发）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","retcode":0,"data":{"message_id":713138214},"wording":""}`))
	}))
	defer srv.Close()
	p := newPermissionTestPlugin(t)
	p.napcatURL = srv.URL
	p.httpClient = srv.Client()
	p.sendClient = srv.Client()
	if _, err := p.napcat("send_private_msg", map[string]interface{}{"user_id": 1, "message": "x"}); err != nil {
		t.Errorf("正常成功响应被误报失败（会诱发重复发送）: %v", err)
	}
}
