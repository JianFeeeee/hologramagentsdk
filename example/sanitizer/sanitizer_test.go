package main

import "testing"

func TestCleanText(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"empty", "", ""},
		{"clean", "你好世界 hello", "你好世界 hello"},
		{"invalid_utf8", "a\xff\xfe b", "a b"},
		{"ufffd", "有乱码\ufffd字符", "有乱码字符"},
		{"multiple_ufffd", "a\ufffd\ufffdb\ufffdc", "abc"},
		{"ansi_color", "\x1b[31m红色\x1b[0m结束", "红色结束"},
		{"ansi_cursor", "a\x1b[2K\r\nb", "a\r\nb"},
		{"ansi_osc", "\x1b]0;title\x07文本", "文本"},
		{"an_and_ufffd", "\x1b[31m\ufffd中文\x1b[0m", "中文"},
		{"emoji_kept", "颜文字(・ω・´)和🍎", "颜文字(・ω・´)和🍎"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cleanText(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCleanToolCallLeakage(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"empty", "", ""},
		{"clean", "你好", "你好"},
		{"tool_call", "a<tool_call>x</tool_call>b", "ab"},
		{"invoke", "a<invoke>x</invoke>b", "ab"},
		{"function", "a<function>x</function>b", "ab"},
		{"xml_block", "a\n```xml\n<tool_call>x</tool_call>\n```\nb", "a\n\nb"},
		{"json_block", "a\n```json\n<invoke>x</invoke>\n```\nb", "a\n\nb"},
		{"bare_code", "```python\nprint(1)\n```", "```python\nprint(1)\n```"},
		{"chinese_marker", "a【tool_call】x【/tool_call】b", "ab"},
		{"tool_line", "cmd_run(\"ls\")\nok", "ok"},
		{"prose_kept", "cmd_run 是一个工具", "cmd_run 是一个工具"},
		{"multiline", "a\n<tool_call>\nx\n</tool_call>\nb", "a\n\nb"},
		{"whitespace", "a\n\n\n\nb", "a\n\nb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cleanToolCallLeakage(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
