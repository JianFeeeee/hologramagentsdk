package main

// BV 号/av 号/视频链接的解析与校验。
//
// ## 为什么单独一个文件、并且做得这么挑剔
//
// B 站内容侧的所有工具都以「视频」为主键入口，而用户与模型递进来的东西
// 有五种形态：BV 号、av 号、完整链接（带 p=2&t=10 之类的参数）、手机端
// 链接、短链（b23.tv）。**把参数当编号**或者**把短链当 BV 号**去请求，
// 平台只会回一个语焉不详的空结果 —— 使用者会以为是「这个视频没有评论」，
// 而实际入口就已经错了。所以入口处就把形态判清楚，认不出就**响亮报错**。
//
// 反过来，一旦进了工具内部，我们只认一种内部表示（videoRef），
// 后面的接口调用就不必再猜。

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// bvAlphabet 是 BV 号所用的 base58 变体字母表（去掉了易混的 0/I/O/l）。
//
// 顺序有意义：BV↔av 的换算按**位置**取字符，改动顺序会让所有换算结果错位。
const bvAlphabet = "FcwAPNKTMug3GV5Lj7EJnHpWsx4tb8haYeviqBz6rkCy12mUSDQX9RdoZf"

const (
	// bvXor / bvMask 是 BV↔av 换算的固定盐与位宽（51 bit 承载 aid）。
	bvXor  int64 = 23442827791579
	bvMask int64 = (1 << 51) - 1

	bvLen = 12 // "BV" + 10 位
)

var (
	reBV = regexp.MustCompile(`[Bb][Vv][0-9A-Za-z]{10}`)
	// \b 包住 av 号，避免把 "av1700012" 这类长串截成 "av170001"。
	reAV = regexp.MustCompile(`(?i)\bav(\d{1,18})\b`)
)

// videoRef 是插件内部的统一视频标识。
//
// 两个字段都保留：接口层有的端点只吃 bvid（view/player），有的端点只吃
// aid（评论 list/add），提前换算好可以少一次请求，也不必在每个工具里重算。
type videoRef struct {
	BVID string
	AID  int64
}

// String 供日志与错误信息使用：两种编号一起显示，便于用户核对。
func (r videoRef) String() string {
	switch {
	case r.BVID != "" && r.AID > 0:
		return fmt.Sprintf("%s (av%d)", r.BVID, r.AID)
	case r.BVID != "":
		return r.BVID
	default:
		return fmt.Sprintf("av%d", r.AID)
	}
}

// parseRef 把任意形态的输入归一成 videoRef。
func parseRef(raw string) (videoRef, error) {
	s := strings.TrimSpace(raw)
	// 模型常常把带引号/尖括号的串原样递进来。
	s = strings.Trim(s, "\"'`<>")
	if s == "" {
		return videoRef{}, fmt.Errorf("缺少视频标识：需要 BV 号（如 BV17x411w7KC）、av 号（如 av170001）或视频链接")
	}

	lower := strings.ToLower(s)
	// 短链必须先展开才可能拿到编号，而展开要多发一次请求、还要跟随 302。
	// 本插件刻意不做（见 README「明确没做」一节）：与其悄悄拿一个跳转后的
	// 页面地址去猜，不如直接让调用方贴上完整链接。
	for _, host := range []string{"b23.tv", "bili2233.cn", "bili22.cn"} {
		if strings.Contains(lower, host) {
			return videoRef{}, fmt.Errorf("不支持 B 站短链 %q：短链要先跟随跳转才能拿到 BV 号，本插件不发这类隐式请求；请粘贴完整视频链接（含 /video/BV…）或直接给 BV 号", s)
		}
	}

	// 裸号优先：合法的 BV/av 号里不会出现 "/" 或 "?"，能直接命中就不必走 URL 解析。
	if bv, ok := normalizeBV(s); ok {
		return videoRef{BVID: bv, AID: bvToAID(bv)}, nil
	}
	if aid, ok, err := normalizeAV(s); err != nil {
		return videoRef{}, err
	} else if ok {
		return videoRef{AID: aid, BVID: aidToBV(aid)}, nil
	}

	// 走到这里说明不是裸号：按链接再试一次，从路径/参数里抠编号。
	if strings.Contains(s, "/") || strings.Contains(s, "?") || strings.Contains(lower, "bilibili.com") {
		if m := reBV.FindString(s); m != "" {
			if bv, ok := normalizeBV(m); ok {
				return videoRef{BVID: bv, AID: bvToAID(bv)}, nil
			}
			return videoRef{}, fmt.Errorf("链接里的 BV 号 %q 不合法（大小写敏感，10 位字符均取自 base58 变体字母表，不含 0/I/O/l）", m)
		}
		if m := reAV.FindStringSubmatch(s); m != nil {
			aid, err := strconv.ParseInt(m[1], 10, 64)
			if err != nil || aid <= 0 || aid > bvMask {
				return videoRef{}, fmt.Errorf("链接里的 av 号 %q 不合法（应为 1 ~ %d）", m[0], bvMask)
			}
			return videoRef{AID: aid, BVID: aidToBV(aid)}, nil
		}
		if u, err := url.Parse(s); err == nil && u.Host != "" {
			return videoRef{}, fmt.Errorf("链接 %q 里找不到 BV 号或 av 号：请用形如 https://www.bilibili.com/video/BV17x411w7KC 的视频页链接（动态/番剧/直播链接不是本插件的输入）", s)
		}
	}

	return videoRef{}, fmt.Errorf("%q 既不是合法 BV 号（BV+10 位）、av 号（av+数字），也不是 B 站视频链接", s)
}

// normalizeBV 校验并返回规范化（保留原大小写）的 BV 号。
//
// 不做大小写转换是故意的：BV 号**大小写敏感**，"BV1XX411C7MD" 与
// "BV1xx411c7mD" 是两个不同的编号。悄悄 ToUpper/ToLower 会算出一个
// 存在的、但是别人的视频 —— 这类错误比直接报错危险得多。
func normalizeBV(s string) (string, bool) {
	if len(s) != bvLen {
		return "", false
	}
	if s[0] != 'B' && s[0] != 'b' {
		return "", false
	}
	if s[1] != 'V' && s[1] != 'v' {
		return "", false
	}
	// 统一前缀大小写，编号部分原样保留。
	bv := "BV" + s[2:]
	body := bv[2:]
	if len(body) != bvLen-2 {
		return "", false
	}
	for i := 0; i < len(body); i++ {
		if !strings.ContainsRune(bvAlphabet, rune(body[i])) {
			return "", false
		}
	}
	return bv, true
}

// normalizeAV 校验裸 av 号。第三个返回值用于区分"不是 av 号"与"是 av 号但越界"。
func normalizeAV(s string) (int64, bool, error) {
	lower := strings.ToLower(s)
	if !strings.HasPrefix(lower, "av") {
		return 0, false, nil
	}
	digits := s[2:]
	aid, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("av 号 %q 的编号部分不是数字", s)
	}
	if aid <= 0 || aid > bvMask {
		return 0, false, fmt.Errorf("av 号 %q 越界（应为 1 ~ %d）", s, bvMask)
	}
	return aid, true, nil
}

// bvToAID / aidToBV 是平台公开的换位算法：51 bit 的 aid 异或固定盐后按
// base58 写成 10 位，再与固定位置交换两次。保留它是为了「一次换算省一次
// 请求」——评论接口只吃 aid，而用户只会给 BV 号。
func bvToAID(bv string) int64 {
	b := []byte(bv)
	if len(b) != bvLen {
		return 0
	}
	b[3], b[9] = b[9], b[3]
	b[4], b[7] = b[7], b[4]
	var tmp int64
	for i := 3; i < bvLen; i++ {
		idx := strings.IndexByte(bvAlphabet, b[i])
		if idx < 0 {
			return 0
		}
		tmp = tmp*int64(len(bvAlphabet)) + int64(idx)
	}
	return (tmp & bvMask) ^ bvXor
}

func aidToBV(aid int64) string {
	b := []byte("BV1000000000")
	tmp := ((int64(1) << 51) | aid) ^ bvXor
	for i := len(b) - 1; i >= 0 && tmp > 0; i-- {
		b[i] = bvAlphabet[tmp%int64(len(bvAlphabet))]
		tmp /= int64(len(bvAlphabet))
	}
	b[3], b[9] = b[9], b[3]
	b[4], b[7] = b[7], b[4]
	return string(b)
}
