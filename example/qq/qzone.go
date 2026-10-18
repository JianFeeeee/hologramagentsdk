package main

// QQ 空间（Qzone）说说：发送与删除。
//
// ## 接口来源与一次真实的误判（2026-10-18）
//
// NapCat 源码里 Qzone 的实现是：
//
//	packages/napcat-onebot/action/extends/SendQzoneMsg.ts
//	packages/napcat-onebot/action/extends/DeleteQzoneMsg.ts
//
// 但 **action 名不是类名**，`router.ts` 里声明的是：
//
//	SendQzoneMsg:   'send_qzone_msg',
//	DeleteQzoneMsg: 'delete_qzone_msg',
//
// 我曾用类名 `SendQzoneMsg` 探测，得到「不支持的Api SendQzoneMsg」，
// 于是错误地断定"生产版本太旧、不支持 Qzone"。换成下划线名后：
//
//	POST /send_qzone_msg {"content":"…","ugc_right":64}
//	→ {"status":"ok","retcode":0,"data":{"tid":"a5c1ca56d081d46a789a0200"}}
//
// **API 一直是可用的**，是我的调用名错了。
//
// 教训：「不支持」这个结论必须用真实 action 名验证 —— 源码里的
// 类名、文件名、方法名都不是调用名。
//
// ## 为什么不需要 Shell / NTQQ 注入
//
// Qzone 是纯 HTTP：NapCat 的 WebApi.publishQzoneMsg 直连
// `up.qzone.qq.com` / `user.qzone.qq.com`，与 NTQQ 客户端进程无关。
// （对比：shell/native loader 那套才是绑 NTQQ 的。）

import (
	"fmt"
	"strconv"
)

// validUgcRights 是 NapCat 服务端硬校验的白名单（源码 ValidUgcRights）。
//
// 本地也校验一次：服务端拒绝时报的是 "ugc_right 参数不合法, 允许的值为: …"，
// 但那是**跨进程往返之后**才拿到的错误；本地先拦能给模型更直接的反馈，
// 也避免为一次参数错误消耗一次 NapCat 调用。
var validUgcRights = map[int]string{
	1:   "所有人可见",
	4:   "好友可见",
	16:  "部分好友可见",
	64:  "仅自己可见",
	128: "部分好友不可见",
}

// handleSendQzoneMsg 发一条说说。
func (p *Plugin) handleSendQzoneMsg(args map[string]interface{}) (interface{}, error) {
	content, _ := args["content"].(string)
	if content == "" {
		return nil, fmt.Errorf("content 不能为空（说说正文是必填项）")
	}

	params := map[string]interface{}{"content": content}

	// ugc_right：缺省 1（所有人可见）。这里**不**擅自降成"仅自己可见" ——
	// 用户明确说了要发说说，就按他要的默认发；权限由调用方显式声明。
	ugc := 1
	if v, ok := args["ugc_right"]; ok && v != nil {
		switch n := v.(type) {
		case float64:
			ugc = int(n)
		case int:
			ugc = n
		case int64:
			ugc = int(n)
		case string:
			if parsed, err := strconv.Atoi(n); err == nil {
				ugc = parsed
			} else {
				return nil, fmt.Errorf("ugc_right 需为整数，收到 %q", n)
			}
		}
	}
	if desc, ok := validUgcRights[ugc]; !ok {
		return nil, fmt.Errorf("ugc_right=%d 不合法，只能取 1/4/16/64/128（依次为：所有人可见/好友可见/部分好友可见/仅自己可见/部分好友不可见）", ugc)
	} else {
		_ = desc
	}
	params["ugc_right"] = ugc

	// target_uins：16/128 时必填（NapCat 服务端也会校验，本地先拦）。
	if raw, ok := args["target_uins"]; ok && raw != nil {
		uins, err := toInt64Slice(raw)
		if err != nil {
			return nil, fmt.Errorf("target_uins: %w", err)
		}
		if len(uins) > 0 {
			params["target_uins"] = uins
		}
	}
	if ugc == 16 || ugc == 128 {
		if _, ok := params["target_uins"]; !ok {
			return nil, fmt.Errorf("ugc_right=%d（%s）时必须提供 target_uins", ugc, validUgcRights[ugc])
		}
	}

	// images：原样透传字符串数组（支持 http(s):// / file:// / base64://，
	// NapCat 自己下载并上传到 Qzone）。
	if raw, ok := args["images"]; ok && raw != nil {
		imgs, err := toStringSlice(raw)
		if err != nil {
			return nil, fmt.Errorf("images: %w", err)
		}
		if len(imgs) > 0 {
			params["images"] = imgs
		}
	}

	resp, err := p.napcat("send_qzone_msg", params)
	if err != nil {
		return nil, fmt.Errorf("发送说说失败: %w", err)
	}
	return resp, nil
}

// handleDeleteQzoneMsg 删一条说说。
func (p *Plugin) handleDeleteQzoneMsg(args map[string]interface{}) (interface{}, error) {
	tid, _ := args["tid"].(string)
	if tid == "" {
		return nil, fmt.Errorf("tid 不能为空（来自 send_qzone_msg 的返回值或空间列表）")
	}
	resp, err := p.napcat("delete_qzone_msg", map[string]interface{}{"tid": tid})
	if err != nil {
		return nil, fmt.Errorf("删除说说失败: %w", err)
	}
	return resp, nil
}

// toStringSlice 把 JSON-RPC 反序列化出来的数组转成 []string。
//
// 为什么不能直接断言 []string：经过 proc 协议的 JSON 往返后，
// []interface{} 是常态（模型传来的参数一定是这个形态）。
// 只认 []string 会让这个参数**永远为空**，且不报错 —— 静默失效。
func toStringSlice(v interface{}) ([]string, error) {
	switch arr := v.(type) {
	case []string:
		return arr, nil
	case []interface{}:
		out := make([]string, 0, len(arr))
		for _, e := range arr {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("期望字符串数组，遇到 %T", e)
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("期望数组，收到 %T", v)
}

// toInt64Slice 同上，用于 QQ 号数组。
//
// ★ 必须接受 float64：JSON 数字解码成 float64，模型传 123456 到这里就是
// float64(123456)。只认 int 会让 target_uins 永远被当成空数组。
func toInt64Slice(v interface{}) ([]int64, error) {
	switch arr := v.(type) {
	case []int64:
		return arr, nil
	case []interface{}:
		out := make([]int64, 0, len(arr))
		for _, e := range arr {
			switch n := e.(type) {
			case float64:
				out = append(out, int64(n))
			case int:
				out = append(out, int64(n))
			case int64:
				out = append(out, n)
			case string:
				parsed, err := strconv.ParseInt(n, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("QQ 号需为数字，收到 %q", n)
				}
				out = append(out, parsed)
			default:
				return nil, fmt.Errorf("期望 QQ 号数组，遇到 %T", e)
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("期望数组，收到 %T", v)
}
