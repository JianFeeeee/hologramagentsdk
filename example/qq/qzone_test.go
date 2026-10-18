package main

// Qzone 工具的判据。
//
// ## 一个必须被钉住的教训（2026-10-18）
//
// 我曾断言「生产 NapCat 不支持发说说」，依据是探测返回
// 「不支持的Api SendQzoneMsg」。**那是调用名错了** ——
// router.ts 里声明的是下划线式 `send_qzone_msg`，我用的是源码类名
// `SendQzoneMsg`。换对名字后一次成功（retcode 0, tid 返回）。
//
// 所以判据要钉的第一条就是：**action 名必须是下划线式**。
// 这一条能防止未来有人"照着源码类名"写回来。

import (
	"os"
	"strings"
	"testing"
)

// ★★★ 判据 1：NapCat action 名必须是下划线式，不是源码类名。
func TestQzoneActionNamesAreSnakeCase(t *testing.T) {
	for _, ln := range codeLines(t, "qzone.go") {
		// 抓 p.napcat("xxx" 里的字面量
		if i := strings.Index(ln, `p.napcat("`); i >= 0 {
			rest := ln[i+len(`p.napcat("`):]
			if j := strings.Index(rest, `"`); j > 0 {
				action := rest[:j]
				if action != strings.ToLower(action) {
					t.Fatalf("★ NapCat action 名 `%s` 不是下划线式 ——\n"+
						"   源码里的类名（SendQzoneMsg）不是调用名，真实名在 router.ts:\n"+
						"     SendQzoneMsg:   'send_qzone_msg',\n"+
						"     DeleteQzoneMsg: 'delete_qzone_msg'", action)
				}
			}
		}
	}
}

// ★★★ 判据 2：ugc_right 白名单必须在本地校验。
//
// 服务端也校验，但那是跨进程往返之后才拿到的错误。
// 本地先拦能给模型更直接的反馈，也省一次 NapCat 调用。
func TestQzoneUgcRightValidatedLocally(t *testing.T) {
	want := []int{1, 4, 16, 64, 128}
	if len(validUgcRights) != len(want) {
		t.Fatalf("★ ugc_right 白名单项数 %d，期望 %d", len(validUgcRights), len(want))
	}
	for _, v := range want {
		if _, ok := validUgcRights[v]; !ok {
			t.Fatalf("★ ugc_right 白名单缺少 %d（NapCat 源码 ValidUgcRights = %v）", v, want)
		}
	}
}

// ★★★ 判据 3：16/128 时必须要求 target_uins。
//
// NapCat 服务端会报 "ugc_right 为 16 或 128 时, target_uins 不能为空"，
// 本地先拦省一次往返。
func TestQzoneTargetUinsRequiredForRestrictedRights(t *testing.T) {
	p := &Plugin{name: "qq"}

	// 16 无 target_uins ⇒ 必须报错
	if _, err := p.handleSendQzoneMsg(map[string]interface{}{
		"content": "x", "ugc_right": float64(16),
	}); err == nil {
		t.Fatal("★ ugc_right=16 缺 target_uins 却没报错 —— 服务端会拒，白跑一次往返")
	}

	// content 必填
	if _, err := p.handleSendQzoneMsg(map[string]interface{}{}); err == nil {
		t.Fatal("★ 缺 content 却没报错")
	}

	// 非法 ugc_right 本地拦
	if _, err := p.handleSendQzoneMsg(map[string]interface{}{
		"content": "x", "ugc_right": float64(7),
	}); err == nil {
		t.Fatal("★ ugc_right=7（不在白名单）却没报错")
	}
}

// ★★★ 判据 4：target_uins / images 必须接受 JSON 反序列化的形态。
//
// 经 proc 协议往返后，数组一定是 []interface{}、数字一定是 float64。
// 只认 []int64 / []string 会让这些参数**永远为空且不报错** —— 静默失效。
func TestQzoneAcceptJSONDecodedTypes(t *testing.T) {
	// []interface{} + float64（真实链路形态）
	uins, err := toInt64Slice([]interface{}{float64(123456), float64(789012)})
	if err != nil || len(uins) != 2 || uins[0] != 123456 || uins[1] != 789012 {
		t.Fatalf("★ target_uins 不接受 JSON 形态（[]interface{}+float64）：%v, %v", uins, err)
	}
	imgs, err := toStringSlice([]interface{}{"http://a/b.jpg", "base64://xxx"})
	if err != nil || len(imgs) != 2 {
		t.Fatalf("★ images 不接受 JSON 形态（[]interface{}）：%v, %v", imgs, err)
	}
	// 字符串形式的 QQ 号也要能吃（模型常传 "123456"）
	uins2, err := toInt64Slice([]interface{}{"123456"})
	if err != nil || len(uins2) != 1 || uins2[0] != 123456 {
		t.Fatalf("★ target_uins 不接受字符串 QQ 号：%v, %v", uins2, err)
	}
}

// ★★★ 判据 5：两个工具都注册了（否则模型看不到）。
func TestQzoneToolsRegistered(t *testing.T) {
	b, err := os.ReadFile("plugin.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, name := range []string{"send_qzone_msg", "delete_qzone_msg"} {
		if !strings.Contains(src, `tp + "`+name+`"`) {
			t.Fatalf("★ 工具 %s 没有注册", name)
		}
	}
}
