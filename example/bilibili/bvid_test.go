package main

// BV/av/链接解析的判据。
//
// 这些用例的存在理由：入口形态判错的后果是**静默的错误结果**
//（把 ?p=2 当编号、把短链当 BV 号），平台只会回一个空 data，
// 使用者于是被告知"这个视频没有内容"。所以宁可这里失败得难看。

import (
	"strings"
	"testing"
)

func TestParseRefBV(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		wantA int64
	}{
		{"BV17x411w7KC", "BV17x411w7KC", 170001},
		{"  BV17x411w7KC  ", "BV17x411w7KC", 170001},
		// 前缀大小写不敏感，但编号部分大小写敏感 —— 不能被悄悄改写。
		{"bv17x411w7KC", "BV17x411w7KC", 170001},
		{"BV1xx411c7mD", "BV1xx411c7mD", 2},
		{"BV1J14y1M713", "BV1J14y1M713", 777927636},
	}
	for _, c := range cases {
		ref, err := parseRef(c.in)
		if err != nil {
			t.Fatalf("parseRef(%q) 报错: %v", c.in, err)
		}
		if ref.BVID != c.want {
			t.Errorf("parseRef(%q).BVID = %q, want %q", c.in, ref.BVID, c.want)
		}
		if ref.AID != c.wantA {
			t.Errorf("parseRef(%q).AID = %d, want %d（BV↔av 换算错位会让评论接口查到别的视频）", c.in, ref.AID, c.wantA)
		}
	}
}

func TestParseRefAV(t *testing.T) {
	ref, err := parseRef("av170001")
	if err != nil {
		t.Fatalf("parseRef(av170001): %v", err)
	}
	if ref.AID != 170001 || ref.BVID != "BV17x411w7KC" {
		t.Fatalf("av170001 解析成 %+v，期望 AID=170001 BVID=BV17x411w7KC", ref)
	}

	// 大小写混写的 AV 也要认（用户从别处复制时常这样）。
	if _, err := parseRef("AV170001"); err != nil {
		t.Errorf("parseRef(AV170001) 应接受: %v", err)
	}
}

func TestParseRefURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://www.bilibili.com/video/BV17x411w7KC", "BV17x411w7KC"},
		// 带参数：最常见的形态，也是"把参数当编号"最容易出错的地方。
		{"https://www.bilibili.com/video/BV17x411w7KC?p=2&t=10&spm_id_from=333.999", "BV17x411w7KC"},
		{"https://www.bilibili.com/video/BV17x411w7KC/?spm_id_from=333.788&vd_source=abc", "BV17x411w7KC"},
		{"https://m.bilibili.com/video/BV17x411w7KC?share_source=copy_web", "BV17x411w7KC"},
		{"www.bilibili.com/video/BV17x411w7KC", "BV17x411w7KC"},
		// 带引号/尖括号的串：模型常原样递进来。
		{"\"https://www.bilibili.com/video/BV17x411w7KC\"", "BV17x411w7KC"},
	}
	for _, c := range cases {
		ref, err := parseRef(c.in)
		if err != nil {
			t.Fatalf("parseRef(%q) 报错: %v", c.in, err)
		}
		if ref.BVID != c.want {
			t.Errorf("parseRef(%q).BVID = %q, want %q", c.in, ref.BVID, c.want)
		}
		if ref.AID != 170001 {
			t.Errorf("parseRef(%q).AID = %d, want 170001", c.in, ref.AID)
		}
	}
}

func TestParseRefURLWithAV(t *testing.T) {
	ref, err := parseRef("https://www.bilibili.com/video/av170001?p=1")
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if ref.AID != 170001 {
		t.Fatalf("AID = %d, want 170001", ref.AID)
	}
	if ref.BVID != "BV17x411w7KC" {
		t.Fatalf("BVID = %q, want BV17x411w7KC", ref.BVID)
	}
}

func TestParseRefInvalid(t *testing.T) {
	cases := []struct {
		in   string
		hint string // 期望错误里出现的关键词，确保报错**说到点子上**
	}{
		{"", "缺少视频标识"},
		{"   ", "缺少视频标识"},
		// 长度不对
		{"BV17x411w7K", "既不是合法 BV 号"},
		{"BV17x411w7KCC", "既不是合法 BV 号"},
		// 字母表外的字符（0 与 I 不在 base58 变体字母表里）
		{"BV10x411w7KC", "既不是合法 BV 号"},
		{"BV1Ix411w7KC", "既不是合法 BV 号"},
		// 短链：要明确说"不支持短链"，而不是含糊地说"格式不对"
		{"https://b23.tv/abc123", "不支持 B 站短链"},
		// 链接里没有编号
		{"https://www.bilibili.com/bangumi/play/ep123456", "找不到 BV 号或 av 号"},
		{"随便一段话", "既不是合法 BV 号"},
	}
	for _, c := range cases {
		_, err := parseRef(c.in)
		if err == nil {
			t.Fatalf("parseRef(%q) 应当报错，却成功了", c.in)
		}
		if !strings.Contains(err.Error(), c.hint) {
			t.Errorf("parseRef(%q) 的报错没说到点子上（期望含 %q）：%v", c.in, c.hint, err)
		}
	}
}

// 反向钉住：非法 BV 号里的"编号部分大小写敏感"这件事不能被悄悄 For 成
// 大小写不敏感 —— 那样会算出一个存在但属于别人的视频。
func TestBVIsCaseSensitive(t *testing.T) {
	a, ok := normalizeBV("BV17x411w7KC")
	if !ok {
		t.Fatal("BV17x411w7KC 应合法")
	}
	lower, ok := normalizeBV("BV17X411W7KC")
	if !ok {
		// 字母表包含大写字母，所以它仍是"合法形状"的 BV 号 ——
		// 关键在于它必须是**不同的编号**。
		t.Log("BV17X411W7KC 未通过字母表校验，跳过")
		return
	}
	if a == lower {
		t.Fatal("大小写不同的 BV 号被归一成同一个值：这会查到别人的视频")
	}
	if bvToAID(a) == bvToAID(lower) {
		t.Fatal("大小写不同的 BV 号换算出同一个 aid")
	}
}

func TestBVAIDRoundTrip(t *testing.T) {
	for _, aid := range []int64{1, 2, 170001, 114514, 777927636, 113324382475357} {
		bv := aidToBV(aid)
		if len(bv) != bvLen {
			t.Fatalf("aid %d → %q 长度不是 12", aid, bv)
		}
		if got := bvToAID(bv); got != aid {
			t.Errorf("aid %d → %s → %d 往返不一致", aid, bv, got)
		}
	}
}
