package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ★ 回归：qq_get_history 的端点选择不能把 group_id=0 当成"查群 0"。
//
// 现场故障（2026-10-19，内核日志 13 次调用全灭）：
//
//	模型先 qq_get_message 拿到私聊正文，返回体当时无条件带 "group_id": 0
//	（QQ 协议里私聊的群号就是 0）。它把字段原样抄进 qq_get_history，
//	于是后端走 get_group_msg_history(group_id=0)，NapCat 回
//	「retcode=200 消息undefined不存在」；真正的 get_friend_msg_history
//	一次都没被调用过。用户看到的现象是"私聊历史拉不到"。
//
// 本测试钉住两条：① 0 视为未提供，私聊走私聊端点；② 私聊返回体不再吐 group_id。

// fakeNapCat 记录被命中的 action，并对历史端点返回一条固定消息。
type fakeNapCat struct {
	mu      sync.Mutex
	actions []string
	srv     *httptest.Server
}

func newFakeNapCat(t *testing.T) *fakeNapCat {
	t.Helper()
	f := &fakeNapCat{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := strings.TrimPrefix(r.URL.Path, "/")
		f.mu.Lock()
		f.actions = append(f.actions, action)
		f.mu.Unlock()

		// 一条私聊消息：group_id 缺省/为 0，与真实 NapCat 的返回一致。
		body := map[string]interface{}{
			"status":  "ok",
			"retcode": 0,
			"data": map[string]interface{}{
				"messages": []interface{}{
					map[string]interface{}{
						"message_id":   123456,
						"user_id":      2198972886,
						"group_id":     0,
						"message_type": "private",
						"time":         1792407859,
						"raw_message":  "hello",
						"sender":       map[string]interface{}{"nickname": "—/"},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNapCat) hits() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.actions...)
}

func newHistoryPlugin(t *testing.T) (*Plugin, *fakeNapCat) {
	t.Helper()
	f := newFakeNapCat(t)
	instance, err := NewPluginFactory("qq", nil)
	if err != nil {
		t.Fatal(err)
	}
	p := instance.(*Plugin)
	// httpClient 在 Start() 里才建；不经 Start 的单测得自己给，否则 napcat() 空指针。
	p.httpClient = &http.Client{Timeout: 5 * time.Second}
	p.napcatURL = f.srv.URL
	return p, f
}

// group_id=0（私聊消息体里抄来的）必须走私聊端点，而不是群端点。
func TestGetHistoryTreatsZeroGroupIDAsAbsent(t *testing.T) {
	p, f := newHistoryPlugin(t)

	// 模型最容易犯的错：把 get_message 返回体里的 group_id:0 连同 user_id 一起传。
	if _, err := p.handleGetHistory(map[string]interface{}{
		"group_id": float64(0),
		"user_id":  float64(2198972886),
	}); err != nil {
		t.Fatalf("私聊历史不该报错: %v", err)
	}

	hits := f.hits()
	if len(hits) != 1 {
		t.Fatalf("应只调用 1 次 NapCat，实际 %d 次: %v", len(hits), hits)
	}
	if hits[0] != "get_friend_msg_history" {
		t.Fatalf("私聊必须走 get_friend_msg_history，实际走了 %s", hits[0])
	}
}

// 只传 group_id=0（没有任何有效会话）必须明确报错，而不是去查"群 0"。
func TestGetHistoryRejectsOnlyZeroGroupID(t *testing.T) {
	p, f := newHistoryPlugin(t)

	_, err := p.handleGetHistory(map[string]interface{}{"group_id": float64(0)})
	if err == nil {
		t.Fatal("只传 group_id=0 应报错（0 不是合法会话）")
	}
	if !strings.Contains(err.Error(), "user_id") {
		t.Fatalf("错误信息应引导用 user_id，实际: %v", err)
	}
	if hits := f.hits(); len(hits) != 0 {
		t.Fatalf("参数非法时不该请求 NapCat，实际: %v", hits)
	}
}

// 群聊传 group_id 仍走群端点（别把正常路径改坏）。
func TestGetHistoryGroupStillUsesGroupEndpoint(t *testing.T) {
	p, f := newHistoryPlugin(t)

	if _, err := p.handleGetHistory(map[string]interface{}{"group_id": float64(123456)}); err != nil {
		t.Fatalf("群聊历史不该报错: %v", err)
	}
	hits := f.hits()
	if len(hits) != 1 || hits[0] != "get_group_msg_history" {
		t.Fatalf("群聊应走 get_group_msg_history，实际: %v", hits)
	}
}

// 两个都给 ⇒ 语义不明，必须报错而不是静默选一个分支。
func TestGetHistoryRejectsBothPeers(t *testing.T) {
	p, f := newHistoryPlugin(t)

	_, err := p.handleGetHistory(map[string]interface{}{
		"group_id": float64(123456),
		"user_id":  float64(2198972886),
	})
	if err == nil {
		t.Fatal("同时传 group_id 与 user_id 应报错")
	}
	if !strings.Contains(err.Error(), "二选一") {
		t.Fatalf("错误信息应说明二选一，实际: %v", err)
	}
	if hits := f.hits(); len(hits) != 0 {
		t.Fatalf("参数非法时不该请求 NapCat，实际: %v", hits)
	}
}

// msgToGetMsgResult：私聊不吐 group_id，群聊才吐。防止模型再从返回体里抄 0。
func TestMsgToGetMsgResultOmitsGroupIDForPrivate(t *testing.T) {
	privateMsg := map[string]interface{}{
		"message_id":   float64(1),
		"user_id":      float64(2198972886),
		"group_id":     float64(0),
		"message_type": "private",
		"time":         float64(1792407859),
		"raw_message":  "hi",
	}
	got := msgToGetMsgResult(privateMsg)
	if _, exists := got["group_id"]; exists {
		t.Fatalf("私聊返回体不该包含 group_id（会诱导模型回灌 0），实际: %v", got)
	}

	groupMsg := map[string]interface{}{
		"message_id":   float64(2),
		"user_id":      float64(2198972886),
		"group_id":     float64(123456),
		"message_type": "group",
		"time":         float64(1792407859),
		"raw_message":  "hi all",
	}
	gotGroup := msgToGetMsgResult(groupMsg)
	if gid, ok := gotGroup["group_id"]; !ok || gid != int64(123456) {
		t.Fatalf("群聊返回体应包含真实 group_id，实际: %v", gotGroup)
	}
}
