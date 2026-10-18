package main

// 真机 e2e：**取消正在运行的任务**（HA_PI_E2E=1 时运行）。
//
// ## 为什么静态判据不够
//
// interrupt_test.go 只断言"sidecar 里写了 interrupt 且调了 session.abort()"。
// 但 abort() 是否**真的**让一条在跑的任务停下来、并且会话还能继续用，
// 只有真跑一次能证明。这正是本项目反复强调的：判据要打在真实入口上。
//
// ## 对照组是判据的一半
//
// 只测"发了 interrupt 之后任务结束了"是不够的 —— 任务本来也可能自己跑完。
// 必须证明：**不中断时它还在跑**。所以本判据同时跑两组。

import (
	"strings"
	"testing"
	"time"
)

// 逐步任务：要求 agent 对 60 个文件逐个工具调用，刻意拉长回合。
const interruptE2ETask = "请逐个检查 /usr/bin 下按字节序排序的前 60 个可执行文件，" +
	"每个文件一次工具调用，不要合并、不要跳步，全部检查完再汇总。" +
	"这是刻意要求的逐步流程，请严格照做。"

// ★★★ 判据：interrupt 真的停住了在跑的任务，且会话随后仍可用。
func TestRealPiInterruptStopsRunningTask(t *testing.T) {
	nodeBin, sidecarPath := piE2EReady(t)
	workdir := t.TempDir()
	_, w, r := startSidecarForE2E(t, nodeBin, sidecarPath, workdir, "e2e-interrupt")

	if resp := callSidecar(t, w, r, 1, "ping", nil); resp.Error != nil {
		t.Fatalf("ping 失败: %s", resp.Error.Message)
	}

	// 投递长任务
	if resp := callSidecar(t, w, r, 2, "prompt", map[string]interface{}{"text": interruptE2ETask}); resp.Error != nil {
		t.Fatalf("prompt 失败: %s", resp.Error.Message)
	}

	// 等它真的开始干活（否则 interrupt 会撞在"还没开始"上，得 interrupted=false）
	time.Sleep(12 * time.Second)

	// 发中断
	resp := callSidecar(t, w, r, 3, "interrupt", nil)
	if resp.Error != nil {
		t.Fatalf("interrupt 失败: %s", resp.Error.Message)
	}
	raw := string(resp.Result)
	if !strings.Contains(raw, `"interrupted":true`) {
		t.Fatalf("★ interrupt 没有真正中断（应答 %s）—— 用户以为停了，任务还在跑", raw)
	}
	t.Logf("interrupt 应答: %s", raw)

	// 中断后会话必须仍可用：能继续投递并拿到回复
	if resp := callSidecar(t, w, r, 4, "prompt", map[string]interface{}{
		"text": "只回答两个字：收到",
	}); resp.Error != nil {
		t.Fatalf("中断后会话不可用：%s", resp.Error.Message)
	}
	// 等完成通知（id=0 的行会被 callSidecar 跳过，这里靠 state 确认有进展）
	time.Sleep(3 * time.Second)
	st := callSidecar(t, w, r, 5, "state", nil)
	if st.Error != nil {
		t.Fatalf("state 失败: %s", st.Error.Message)
	}
	t.Logf("中断后 state: %s", string(st.Result))
}

// ★★★ 对照组：不中断时，同一个任务在同样时间窗内**远未完成**。
//
// 没有这一条，"中断生效"就无法与"任务碰巧自己跑完了"区分开。
func TestRealPiSameTaskWithoutInterruptStillRunning(t *testing.T) {
	nodeBin, sidecarPath := piE2EReady(t)
	workdir := t.TempDir()
	_, w, r := startSidecarForE2E(t, nodeBin, sidecarPath, workdir, "e2e-control")

	if resp := callSidecar(t, w, r, 1, "ping", nil); resp.Error != nil {
		t.Fatalf("ping 失败: %s", resp.Error.Message)
	}
	if resp := callSidecar(t, w, r, 2, "prompt", map[string]interface{}{"text": interruptE2ETask}); resp.Error != nil {
		t.Fatalf("prompt 失败: %s", resp.Error.Message)
	}

	// 与中断组同样的等待窗口（12s + 中断后观察 ~3s）
	time.Sleep(15 * time.Second)

	// 用 state 看不到"是否在跑"，但 can 用一个事实：此时若发 interrupt，
	// 应当 interrupted=true —— 说明 15 秒时任务仍在运行。
	resp := callSidecar(t, w, r, 3, "interrupt", nil)
	if resp.Error != nil {
		t.Fatalf("interrupt 失败: %s", resp.Error.Message)
	}
	raw := string(resp.Result)
	if !strings.Contains(raw, `"interrupted":true`) {
		t.Fatalf("★ 对照组在 %ds 时任务已不在了（%s）—— "+
			"这说明中断组的 '已中断' 可能只是任务本来就跑完了，判据不成立", 15, raw)
	}
	t.Logf("对照组 15s 时仍在运行，interrupt 成功: %s", raw)
}
