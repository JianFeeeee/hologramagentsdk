package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestPlugin() *Plugin {
	p := &Plugin{name: "acp"}
	p.sessions = make(map[string]*sessionState)
	return p
}

func doPost(t *testing.T, p *Plugin, body string) map[string]interface{} {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/session", strings.NewReader(body))
	rec := httptest.NewRecorder()
	p.handleSessionPost(rec, req)
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, rec.Body.String())
	}
	return resp
}

// 方法名归一化。变异：去掉 '.' ⇒ "session.new" 立刻变红。
func TestCanonicalACP(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"session/new", "sessionnew"},
		{"Session/New", "sessionnew"},
		{"session.new", "sessionnew"},
		{"session_new", "sessionnew"},
		{"session-get", "sessionget"},
		{"", ""},
	} {
		if got := canonicalACP(tc.in); got != tc.want {
			t.Errorf("canonicalACP(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// 各种历史写法都必须路由到 session/new。
func TestHandleSessionPost_AcceptsVariants(t *testing.T) {
	for _, method := range []string{"session/new", "session.new", "Session/New"} {
		p := newTestPlugin()
		body := `{"jsonrpc":"2.0","id":"1","method":"` + method +
			`","params":{"request":{"text":"hi"}}}`
		resp := doPost(t, p, body)
		if _, hasErr := resp["error"]; hasErr {
			t.Fatalf("方法 %q 应被接受，实际报错: %v", method, resp["error"])
		}
		result, _ := resp["result"].(map[string]interface{})
		if result == nil {
			t.Fatalf("方法 %q 应返回 result，实际: %v", method, resp)
		}
		sess, _ := result["session"].(map[string]interface{})
		if sess == nil || sess["id"] == "" {
			t.Fatalf("方法 %q 应返回 session.id，实际: %v", method, resp)
		}
	}
}

// 响应必须回显请求 id：不回显时调用方无法把响应与请求对应上（并发时致命）。
func TestHandleSessionPost_EchoesID(t *testing.T) {
	p := newTestPlugin()
	body := `{"jsonrpc":"2.0","id":"req-42","method":"session/new","params":{"request":{"text":"hi"}}}`
	resp := doPost(t, p, body)
	if resp["id"] != "req-42" {
		t.Fatalf("应回显 id=req-42，实际: %v", resp["id"])
	}
}

// 未知方法回显名字。
func TestHandleSessionPost_UnknownMethodEchoesName(t *testing.T) {
	p := newTestPlugin()
	resp := doPost(t, p, `{"jsonrpc":"2.0","id":"x","method":"session/bogus","params":{}}`)
	errObj, _ := resp["error"].(map[string]interface{})
	if errObj == nil {
		t.Fatalf("未知方法应报错，实际: %v", resp)
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "session/bogus") {
		t.Fatalf("错误信息应回显方法名，实际: %s", msg)
	}
}

// 超限请求体被拒。
func TestHandleSessionPost_BodyLimit(t *testing.T) {
	p := newTestPlugin()
	huge := strings.Repeat("x", maxInboundBody+10)
	resp := doPost(t, p, `{"jsonrpc":"2.0","id":"1","method":"session/new","params":{"request":{"text":"`+huge+`"}}}`)
	errObj, _ := resp["error"].(map[string]interface{})
	if errObj == nil {
		t.Fatal("超限请求体应被拒")
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "上限") {
		t.Fatalf("应提示超限，实际: %s", msg)
	}
}

// 会话级互斥：同 session 的并发请求必须串行（否则历史顺序会与因果不符）。
func TestLockTurn_SerializesSameSession(t *testing.T) {
	p := newTestPlugin()

	unlock1 := p.lockTurn("s1")
	acquired := make(chan struct{})
	go func() {
		unlock2 := p.lockTurn("s1")
		close(acquired)
		unlock2()
	}()

	select {
	case <-acquired:
		t.Fatal("同会话的第二个 lockTurn 不应在第一个未释放时返回")
	default:
	}
	unlock1()
	<-acquired // 释放后第二个必须拿到
}

// 不同会话必须能并行（不能一把全局锁把无关会话堵死）。
func TestLockTurn_DifferentSessionsParallel(t *testing.T) {
	p := newTestPlugin()
	unlockA := p.lockTurn("a")
	defer unlockA()

	done := make(chan struct{})
	go func() {
		unlockB := p.lockTurn("b")
		unlockB()
		close(done)
	}()
	<-done // 若实现是全局锁，这里会死等（测试超时）
}

// 解锁后不得拆表：拆了会让「已在等锁的 B」与「新建锁的 C」同时持锁。
//
// 变异：在 lockTurn 的解锁闭包里加 delete(p.turnSem, sid) ⇒ 本判据变红。
//
// 这是我在实现时真的写错过的版本（注释里留了推导），单测必须能抓住它，
// 否则「per-session 互斥」只是看起来对。
func TestLockTurn_DoesNotDropSemaphoreAfterUnlock(t *testing.T) {
	p := newTestPlugin()

	unlock1 := p.lockTurn("s")
	sem1 := p.turnSem["s"]
	unlock1()

	// 再次获取必须是**同一个**信号量。
	unlock2 := p.lockTurn("s")
	sem2 := p.turnSem["s"]
	unlock2()

	if sem1 != sem2 {
		t.Fatal("解锁后信号量被替换：已在等锁的请求会与新建锁的请求同时持锁（互斥失效）")
	}
}

// 过期清理只回收空闲的信号量（有令牌 = 有人在持锁，不得删）。
func TestSweepSessions_KeepsBusySemaphore(t *testing.T) {
	p := newTestPlugin()

	// 一个忙碌会话（持锁未释放）与一个空闲会话。
	unlockBusy := p.lockTurn("busy")
	defer unlockBusy()
	unlockIdle := p.lockTurn("idle")
	unlockIdle()

	// 让两个会话都过期。
	p.mu.Lock()
	p.sessions["busy"] = &sessionState{ID: "busy", LastUsed: time.Now().Add(-3 * time.Hour)}
	p.sessions["idle"] = &sessionState{ID: "idle", LastUsed: time.Now().Add(-3 * time.Hour)}
	p.mu.Unlock()

	p.sweepSessions()

	if _, ok := p.sessions["busy"]; ok {
		t.Fatal("过期会话应被删除")
	}
	if p.turnSem["busy"] == nil {
		t.Fatal("仍在持锁的会话，其信号量不得回收（删除会让新锁与旧锁并存）")
	}
	if p.turnSem["idle"] != nil {
		t.Fatal("空闲会话的信号量应被回收（否则表随会话数无限增长）")
	}
}

// 每个方法都必须**真的路由到自己的分支**（而不只是 canonicalACP 算得对）。
//
// 这条判据来自一次真实回归：switch 的分支值我一开始写成了 "session/get"
// （带斜杠），而归一化后的值是不带斜杠的 "sessionget"，于是该分支永远
// 走不到、落到 default 报 unknown method。只测 canonicalACP 抓不到——
// 必须把请求真发一遍。
//
// 变异：把任一 case 改回带斜杠的写法 ⇒ 对应子测试变红。
func TestHandleSessionPost_RoutesEachMethod(t *testing.T) {
	// 先建一个会话，供 get/update/cancel 使用。
	p := newTestPlugin()
	created := doPost(t, p, `{"jsonrpc":"2.0","id":"c","method":"session/new","params":{"request":{"text":"hi"}}}`)
	result, _ := created["result"].(map[string]interface{})
	if result == nil {
		t.Fatalf("session/new 未成功: %v", created)
	}
	sess, _ := result["session"].(map[string]interface{})
	sid, _ := sess["id"].(string)
	if sid == "" {
		t.Fatalf("session/new 未返回 id: %v", created)
	}

	for _, tc := range []struct {
		name   string
		method string
		params string
	}{
		{"sessionget", "session/get", `"session_id":"` + sid + `"`},
		{"sessionupdate", "session/update", `"session_id":"` + sid + `","final":true`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := doPost(t, p, `{"jsonrpc":"2.0","id":"1","method":"`+tc.method+
				`","params":{`+tc.params+`}}`)
			if _, hasErr := resp["error"]; hasErr {
				t.Fatalf("方法 %q 应被路由到自己的分支，实际报错: %v", tc.method, resp["error"])
			}
		})
	}

	// cancel 放最后：它会删掉会话。
	resp := doPost(t, p, `{"jsonrpc":"2.0","id":"1","method":"session/cancel","params":{"session_id":"`+sid+`"}}`)
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("session/cancel 应被路由，实际报错: %v", resp["error"])
	}
}
