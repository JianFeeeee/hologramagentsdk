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

// NapCat 的表情接口只有 fetch_custom_face（收藏表情 URL 列表）+ 消息段发送，
// 没有"按 id 发表情"的独立 action。这两条用例钉住：
//
//	① list 能把 fetch_custom_face 的 data[] 解析成带 index 的列表
//	② send 能按 index 取到 URL，并以 image 段发到正确端点
type stickerNapCat struct {
	mu      sync.Mutex
	actions []string
	bodies  map[string]map[string]interface{}
	srv     *httptest.Server
}

func newStickerNapCat(t *testing.T) *stickerNapCat {
	t.Helper()
	f := &stickerNapCat{bodies: map[string]map[string]interface{}{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := strings.TrimPrefix(r.URL.Path, "/")
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.actions = append(f.actions, action)
		f.bodies[action] = body
		f.mu.Unlock()

		var resp interface{}
		if action == "fetch_custom_face" {
			resp = map[string]interface{}{
				"status": "ok", "retcode": 0,
				"data": []string{
					"https://p.qpic.cn/qq_expression/1/a/0",
					"https://p.qpic.cn/qq_expression/1/b/0",
				},
			}
		} else {
			resp = map[string]interface{}{"status": "ok", "retcode": 0, "data": map[string]interface{}{"message_id": 99}}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newStickerPlugin(t *testing.T) (*Plugin, *stickerNapCat) {
	t.Helper()
	f := newStickerNapCat(t)
	inst, err := NewPluginFactory("qq", nil)
	if err != nil {
		t.Fatal(err)
	}
	p := inst.(*Plugin)
	p.httpClient = &http.Client{Timeout: 5 * time.Second}
	p.napcatURL = f.srv.URL
	return p, f
}

func TestListStickersParsesURLsWithOneBasedIndex(t *testing.T) {
	p, _ := newStickerPlugin(t)
	res, err := p.handleListStickers(map[string]interface{}{"count": float64(10)})
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]interface{})
	if m["total"].(int) != 2 {
		t.Fatalf("total = %v, want 2", m["total"])
	}
	list := m["stickers"].([]map[string]interface{})
	if list[0]["index"].(int) != 1 || list[1]["index"].(int) != 2 {
		t.Fatalf("index 必须从 1 开始: %v", list)
	}
	if !strings.Contains(list[1]["url"].(string), "/b/") {
		t.Fatalf("url 解析错位: %v", list[1])
	}
}

// 表情发送能力已并入 outputch（用户 2026-10-22 拍板收回独立 tool）：
// 原先的 qq_send_sticker(index=N) 现在是 output_send__qq + type=sticker +
// meta.sticker_index=N。本测试改为走 outputch 的真实入口，确保迁移后行为不变。
func TestSendStickerByIndexUsesImageSegment(t *testing.T) {
	p, f := newStickerPlugin(t)
	if _, err := p.handleChannelOutput(map[string]interface{}{
		"payload": "",
		"type":    "sticker",
		"meta":    `{"user_id":2198972886,"sticker_index":2}`,
	}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.bodies["send_private_msg"]
	if !ok {
		t.Fatalf("私聊表情应走 send_private_msg，实际命中 %v", f.actions)
	}
	msg, _ := body["message"].(string)
	// 表情按 image 段发送（NapCat 无独立表情发送 action）
	if !strings.HasPrefix(msg, "[CQ:image,file=") || !strings.Contains(msg, "/b/") {
		t.Fatalf("发送内容 = %q，应是把第 2 个收藏表情作为 image 段", msg)
	}
	if body["user_id"] == nil {
		t.Fatalf("缺 user_id: %v", body)
	}
}

func TestSendStickerRejectsMissingTarget(t *testing.T) {
	p, _ := newStickerPlugin(t)
	if _, err := p.handleChannelOutput(map[string]interface{}{
		"payload": "https://x/y",
		"type":    "sticker",
		"meta":    `{}`,
	}); err == nil {
		t.Fatal("没有 group_id/user_id 时应报错")
	}
}

func TestSendStickerRejectsIndexOutOfRange(t *testing.T) {
	p, _ := newStickerPlugin(t)
	if _, err := p.handleChannelOutput(map[string]interface{}{
		"payload": "",
		"type":    "sticker",
		"meta":    `{"user_id":1,"sticker_index":9}`,
	}); err == nil {
		t.Fatal("序号越界时应报错")
	}
}

// 描述缓存：模型看过表情后记录 desc/mood，下次 list 直接带出来，
// 这样它才能真正"按情绪挑表情"——NapCat 的 fetch_custom_face 只给 URL。
func TestStickerDescriptionsRoundTrip(t *testing.T) {
	p, _ := newStickerPlugin(t)
	p.stickerDescs = map[string]stickerDesc{}
	base := "https://p.qpic.cn/qq_expression/1/a/0"

	if _, err := p.handleDescribeSticker(map[string]interface{}{
		"url":  base,
		"desc": "一只猫翻白眼",
		"mood": "无语",
	}); err != nil {
		t.Fatal(err)
	}

	res, err := p.handleListStickers(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	list := res.(map[string]interface{})["stickers"].([]map[string]interface{})
	if list[0]["desc"] != "一只猫翻白眼" || list[0]["mood"] != "无语" {
		t.Fatalf("描述未带出: %v", list[0])
	}
	// 第二个表情没有描述 ⇒ 不能出现空字符串字段，且 hint 要提示去描述
	if _, ok := list[1]["desc"]; ok {
		t.Fatalf("未描述的表情不应带空 desc: %v", list[1])
	}
	if !strings.Contains(res.(map[string]interface{})["hint"].(string), "see_image") {
		t.Fatalf("hint 应提示用 see_image 补描述: %v", res.(map[string]interface{})["hint"])
	}
}

func TestDescribeStickerRequiresContent(t *testing.T) {
	p, _ := newStickerPlugin(t)
	if _, err := p.handleDescribeSticker(map[string]interface{}{"url": "https://x/y"}); err == nil {
		t.Fatal("desc/mood 都为空时应报错")
	}
}

// add_sticker：从消息里提取 sub_type=1 的表情图片，下载落地后调 add_custom_face。
func TestAddStickerFromMessage(t *testing.T) {
	var mu sync.Mutex
	var actions []string
	var addFiles []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := strings.TrimPrefix(r.URL.Path, "/")
		mu.Lock()
		actions = append(actions, action)
		mu.Unlock()
		switch action {
		case "get_msg":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "ok", "retcode": 0,
				"data": map[string]interface{}{
					"message": []interface{}{
						map[string]interface{}{"type": "image", "data": map[string]interface{}{
							"sub_type": float64(1), "url": "http://" + r.Host + "/cdn/a.gif"}},
						map[string]interface{}{"type": "text", "data": map[string]interface{}{"text": "hi"}},
						map[string]interface{}{"type": "image", "data": map[string]interface{}{
							"sub_type": float64(0), "url": "http://" + r.Host + "/cdn/plain.jpg"}},
					},
				},
			})
		case "add_custom_face":
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f, _ := body["file"].(string)
			mu.Lock()
			addFiles = append(addFiles, f)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "ok", "retcode": 0,
				"data": map[string]interface{}{"result": 0, "isExist": 0},
			})
		default:
			// 图片 CDN：返回 1x1 PNG 字节
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
		}
	}))
	defer srv.Close()

	inst, err := NewPluginFactory("qq", nil)
	if err != nil {
		t.Fatal(err)
	}
	p := inst.(*Plugin)
	p.httpClient = &http.Client{Timeout: 5 * time.Second}
	p.napcatURL = srv.URL
	p.remoteDir = t.TempDir()

	res, err := p.handleAddSticker(map[string]interface{}{"message_id": float64(123)})
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]interface{})["total"].(int) != 1 {
		t.Fatalf("只应收录 sub_type=1 的表情: %v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(addFiles) != 1 || !strings.HasPrefix(addFiles[0], "/app/files/") {
		t.Fatalf("add_custom_face 应收到 /app/files 路径: %v", addFiles)
	}
}

// delete_custom_face 只认**路径段**作 res_id；裸 HASH / 完整 URL 会返回成功但
// 收藏不变（2026-10-20 实测的"假成功"）。这条钉住解析。
func TestStickerResIDFromURL(t *testing.T) {
	url := "https://p.qpic.cn/qq_expression/1456128421/1456128421_0_0_0_715B05B6C89FCE0DCFF28DD17B7FDE07_0_0/0"
	want := "1456128421_0_0_0_715B05B6C89FCE0DCFF28DD17B7FDE07_0_0"
	if got := stickerResID(url); got != want {
		t.Fatalf("res_id = %q, want %q", got, want)
	}
	if got := stickerResID("https://example.com/a.jpg"); got != "" {
		t.Fatalf("非收藏 URL 应返回空: %q", got)
	}
}

func TestRemoveStickerByIndex(t *testing.T) {
	var mu sync.Mutex
	var deleted []map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := strings.TrimPrefix(r.URL.Path, "/")
		if action == "fetch_custom_face" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "ok", "retcode": 0,
				"data": []string{"https://p.qpic.cn/qq_expression/1/1_0_0_0_ABCDEF0123456789ABCDEF0123456789_0_0/0"},
			})
			return
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		deleted = append(deleted, body)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "ok", "retcode": 0,
			"data": map[string]interface{}{"result": 0, "errMsg": "操作成功"},
		})
	}))
	defer srv.Close()

	inst, err := NewPluginFactory("qq", nil)
	if err != nil {
		t.Fatal(err)
	}
	p := inst.(*Plugin)
	p.httpClient = &http.Client{Timeout: 5 * time.Second}
	p.napcatURL = srv.URL
	p.remoteDir = t.TempDir()

	if _, err := p.handleRemoveSticker(map[string]interface{}{"index": float64(1)}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deleted) != 1 {
		t.Fatalf("delete_custom_face 应被调用一次: %v", deleted)
	}
	if deleted[0]["res_id"] != "1_0_0_0_ABCDEF0123456789ABCDEF0123456789_0_0" {
		t.Fatalf("res_id 传错: %v", deleted[0])
	}
}
