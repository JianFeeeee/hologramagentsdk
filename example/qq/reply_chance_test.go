package main

// 未 @ 时的回复概率（2026-10-27 用户口径）：
//   · @ 了机器人 → 100% 处理（确定性，不看概率）
//   · 未 @ 的群消息 → 按 group_reply_chance 随机注入
//   · group_require_at=false 是总闸：关掉即全量，概率不起作用

import (
	"math/rand"
	"testing"
)

// shouldInject 与 plugin.go 的判定保持同一形状，便于对概率分布做统计断言。
func shouldInject(isAt, requireAtOff bool, chance int, r *rand.Rand) bool {
	if isAt {
		return true // @ 机器人恒定 100%
	}
	if requireAtOff {
		return true // 总闸关掉 = 全量
	}
	return chance > 0 && r.Intn(100) < chance
}

func TestReplyChanceAlwaysInjectsMention(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 500; i++ {
		if !shouldInject(true, false, 0, r) {
			t.Fatal("被 @ 的消息必须 100% 注入，与概率无关")
		}
	}
}

func TestReplyChanceZeroDropsUnmentioned(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 500; i++ {
		if shouldInject(false, false, 0, r) {
			t.Fatal("chance=0 时未 @ 的消息不得注入")
		}
	}
}

func TestReplyChanceHundredInjectsAll(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 500; i++ {
		if !shouldInject(false, false, 100, r) {
			t.Fatal("chance=100 时未 @ 的消息必须全部注入")
		}
	}
}

func TestReplyChanceDistribution(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	const n = 20000
	got := 0
	for i := 0; i < n; i++ {
		if shouldInject(false, false, 30, r) {
			got++
		}
	}
	rate := float64(got) / n
	// 30% ± 2 个百分点（n=20000 时标准差约 0.32%，2pp 是极宽松的界）
	if rate < 0.28 || rate > 0.32 {
		t.Fatalf("chance=30 实测注入率 %.3f，超出 [0.28,0.32]", rate)
	}
}

func TestReplyChanceMasterSwitchOverridesProbability(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		if !shouldInject(false, true, 0, r) {
			t.Fatal("group_require_at=false 时不看概率，全部注入")
		}
	}
}
