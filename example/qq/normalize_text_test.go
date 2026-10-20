package main

import "testing"

// 用户实测反馈：QQ 消息里仍有空行（模型用 \n\n 分段）。
// 通道出口做确定性收敛 —— 保留分行、去掉空行、去首尾空白。
func TestNormalizeIMText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"第一段\n\n第二段", "第一段\n第二段"},
		{"a\n\n\nb", "a\nb"},
		{"\n\n开头有空行", "开头有空行"},
		{"结尾有空行\n\n", "结尾有空行"},
		{"保留\n单换行", "保留\n单换行"},
		{"\n\n\n", ""},
		{"没有换行", "没有换行"},
	}
	for _, c := range cases {
		if got := normalizeIMText(c.in); got != c.want {
			t.Errorf("normalizeIMText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
