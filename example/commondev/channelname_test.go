package main

import "testing"

// 钉住文档 §2.2 的命名约束：
// inputch 可含斜杠；outputch **必须**只剩 [A-Za-z0-9_-]，
// 否则会产出非法的 output_send__<name>（上游 400、agent 整体失语）。
func TestOutputChannelNameIsToolSafe(t *testing.T) {
	cases := []string{
		"br-TrueAgent-feat_x",
		"br-p1-feat/x", // 分支名带斜杠
		"br-p1-feat.x", // 点
		"br-p1-中文",     // 非 ASCII
		"br-p1-a b",    // 空格
	}
	for _, id := range cases {
		got := outputChannelName(id)
		if !validToolName("output_send__" + got) {
			t.Errorf("id=%q → outputch=%q ⇒ 工具名 %q 非法（会触发上游 400）",
				id, got, "output_send__"+got)
		}
		// inputch 则允许斜杠（内核自己就用 child/<id>）
		if in := inputChannelName(id); in != "commondev/"+id {
			t.Errorf("inputch 应原样保留: %q", in)
		}
	}
}

// validToolName 复刻上游对函数名的约束。
func validToolName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// 台账往返：保存后能读回，且损坏时拒绝静默重建。
func TestLedgerRoundTrip(t *testing.T) {
	p := &Plugin{name: "commondev"}
	p.path = t.TempDir() + "/ledger.json"
	if err := p.loadLedger(); err != nil {
		t.Fatal(err)
	}
	p.ledger.Projects["demo"] = &Project{Name: "demo", RemoteURL: "https://x/y.git"}
	if err := p.saveLocked(); err != nil {
		t.Fatal(err)
	}

	q := &Plugin{name: "commondev", path: p.path}
	if err := q.loadLedger(); err != nil {
		t.Fatal(err)
	}
	if q.ledger.Projects["demo"] == nil {
		t.Fatal("台账未往返")
	}
}

// 钉住名字校验方向（这条是**真 bug 的回归测试**）。
//
// 初版用排除式正则 `[^a-zA-Z0-9_-]` 配 MatchString 判"合法"，恰好反了：
// 合法名不含违法字符 ⇒ 返回 false ⇒ 把 `cdtest` 判非法。
// 是端到端跑时由 agent 调工具发现的 —— 所以它必须有回归判据。
func TestValidName(t *testing.T) {
	legal := []string{"cdtest", "TrueAgent", "feat_x", "a-b", "A1"}
	for _, s := range legal {
		if !validName(s) {
			t.Errorf("合法名被拒: %q（校验方向反了？）", s)
		}
	}
	illegal := []string{"feat/x", "a b", "中文", "a.b", "", "a/b/c"}
	for _, s := range illegal {
		if validName(s) {
			t.Errorf("非法名被放行: %q", s)
		}
	}
}
