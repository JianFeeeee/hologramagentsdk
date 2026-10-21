package main

import (
	"encoding/json"
	"testing"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// 本测试验证 tmplLinuxBridge 中 snapshotWritable + changedFieldsOnly 的语义。
// 该语义于 2026-09 落地并经本测试锁定（历史依据见 git log --grep=hmapdev）。
// 模板字符串本身无法直接单测，这里以同一份逻辑复刻，防止回归。
// ❗ 模板与本文件须同步修改。
//
// 关键陷阱（第一版实现踩过）：stageContextWritable 返回的切片字段与 sc 共享底层数组，
// handler 原地改元素时"before 快照"会跟着变，diff 看不到变更 → 修复静默失效。
// 故 before 必须是**序列化后的字符串快照**。

func writable(sc *sdk.StageContext) map[string]interface{} {
	m := map[string]interface{}{
		"raw_message": sc.RawMessage,
		"user_id":     sc.UserID,
		"group_id":    sc.GroupID,
		"phase":       string(sc.Phase),
		"llm_text":    sc.LLMText,
		"final_text":  sc.FinalText,
		"no_memory":   sc.NoMemory,
	}
	if sc.Response != nil {
		m["response"] = *sc.Response
	}
	if len(sc.ToolCalls) > 0 {
		m["tool_calls"] = sc.ToolCalls
	}
	if len(sc.ToolResults) > 0 {
		m["tool_results"] = sc.ToolResults
	}
	return m
}

// snapshot 对应模板里的 snapshotWritable：逐字段序列化为不可变快照。
func snapshot(sc *sdk.StageContext) map[string]string {
	snap := map[string]string{}
	for k, v := range writable(sc) {
		b, err := json.Marshal(v)
		if err != nil {
			continue
		}
		snap[k] = string(b)
	}
	return snap
}

// diffOnly 对应模板里的 changedFieldsOnly。
func diffOnly(before map[string]string, after map[string]interface{}) map[string]interface{} {
	diff := map[string]interface{}{}
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	for k := range keys {
		bRaw, bHas := before[k]
		a, aHas := after[k]
		switch {
		case aHas && !bHas:
			diff[k] = a
		case aHas && bHas:
			ab, _ := json.Marshal(a)
			if bRaw != string(ab) {
				diff[k] = a
			}
		case bHas && !aHas:
			switch k {
			case "tool_calls":
				diff[k] = []sdk.ToolCall{}
			case "tool_results":
				diff[k] = []sdk.ToolResult{}
			}
		}
	}
	return diff
}

// 只读插件（如 weather 的 AfterToolcall）不改任何字段 → 零回传。
// 这是修复 lost update 的关键：旧实现会回传它收到的旧快照，覆盖 sanitizer 的清洗结果。
func TestChangedFieldsOnly_ReadOnlyPluginReturnsNothing(t *testing.T) {
	sc := &sdk.StageContext{
		RawMessage: "hello",
		LLMText:    "world",
		ToolResults: []sdk.ToolResult{
			{CallID: "c1", Name: "weather_query", Success: true, Result: "已清洗结果"},
		},
	}
	before := snapshot(sc)
	// 只读 handler：读了但没改
	_ = sc.ToolResults[0].Result
	diff := diffOnly(before, writable(sc))

	if len(diff) != 0 {
		t.Fatalf("只读插件应零回传，实际回传 %d 个字段: %v", len(diff), diff)
	}
}

// 改写插件（如 sanitizer 改 ToolResults）→ 只回传被改的字段。
// ⚠️ 这里是原地改切片元素，正是共享底层数组陷阱的触发场景。
func TestChangedFieldsOnly_WriterReturnsOnlyChanged(t *testing.T) {
	sc := &sdk.StageContext{
		RawMessage: "hello",
		LLMText:    "world",
		ToolResults: []sdk.ToolResult{
			{CallID: "c1", Name: "weather_query", Success: true, Result: "带\x1b[31mANSI\x1b[0m脏数据"},
		},
	}
	before := snapshot(sc)
	// sanitizer handler：原地清洗 ToolResults
	sc.ToolResults[0].Result = "带ANSI脏数据"
	diff := diffOnly(before, writable(sc))

	if len(diff) != 1 {
		t.Fatalf("应只回传 tool_results 一个字段，实际 %d 个: %v", len(diff), diff)
	}
	if _, ok := diff["tool_results"]; !ok {
		t.Fatalf("回传字段应为 tool_results，实际 %v", diff)
	}
	// raw_message / llm_text 未改，不应出现（否则会覆盖其他插件的改写）
	if _, ok := diff["raw_message"]; ok {
		t.Error("raw_message 未改却被回传（会覆盖其他插件的改写）")
	}
	if _, ok := diff["llm_text"]; ok {
		t.Error("llm_text 未改却被回传")
	}
}

// 改写标量字段（如 before_output 改 FinalText）→ 只回传该字段。
func TestChangedFieldsOnly_ScalarChange(t *testing.T) {
	sc := &sdk.StageContext{
		RawMessage: "hi",
		FinalText:  "  带空白的回复  ",
		LLMText:    "原始",
	}
	before := snapshot(sc)
	sc.FinalText = "带空白的回复"
	diff := diffOnly(before, writable(sc))

	if len(diff) != 1 || diff["final_text"] != "带空白的回复" {
		t.Fatalf("应只回传 final_text，实际 %v", diff)
	}
}

// 首次设置 response（短路）→ 回传。
func TestChangedFieldsOnly_NewResponseIsReturned(t *testing.T) {
	sc := &sdk.StageContext{RawMessage: "hi"}
	before := snapshot(sc)
	resp := "被插件短路"
	sc.Response = &resp
	diff := diffOnly(before, writable(sc))

	if v, ok := diff["response"]; !ok || v != "被插件短路" {
		t.Fatalf("新设置的 response 应回传，实际 %v", diff)
	}
}

// 清空切片字段 → 显式回传空值让内核跟随。
func TestChangedFieldsOnly_ClearedSliceIsReturnedAsEmpty(t *testing.T) {
	sc := &sdk.StageContext{
		ToolCalls: []sdk.ToolCall{{ID: "t1", Name: "cmd_run"}},
	}
	before := snapshot(sc)
	sc.ToolCalls = nil // 插件拒绝了全部工具调用
	diff := diffOnly(before, writable(sc))

	v, ok := diff["tool_calls"]
	if !ok {
		t.Fatalf("清空 tool_calls 应显式回传空值，实际 %v", diff)
	}
	if arr, _ := v.([]sdk.ToolCall); len(arr) != 0 {
		t.Fatalf("应回传空切片，实际 %v", v)
	}
}

// 复刻现网场景（实验 13）：sanitizer 清洗后 weather 只读回传，清洗结果不得被覆盖。
// 旧实现下 weather 会回传自己收到的旧快照（含脏数据），覆盖 sanitizer 的清洗（丢失率 1.6~4.3%）。
func TestChangedFieldsOnly_ProductionScenarioNoOverwrite(t *testing.T) {
	dirty := "天气：晴 \x1b[31m28°C\x1b[0m"
	clean := "天气：晴 28°C"

	// 内核下发的原始快照（两插件各拿到一份副本）
	kernelSnapshot := map[string]interface{}{
		"raw_message":  "查天气",
		"llm_text":     "",
		"final_text":   "",
		"user_id":      "u1",
		"group_id":     "",
		"phase":        "after_toolcall",
		"no_memory":    false,
		"tool_results": []sdk.ToolResult{{CallID: "c1", Name: "weather_query", Result: dirty}},
	}

	// sanitizer 副本：清洗
	scSan := &sdk.StageContext{
		RawMessage:  "查天气",
		UserID:      "u1",
		Phase:       sdk.StageAfterToolcall,
		ToolResults: []sdk.ToolResult{{CallID: "c1", Name: "weather_query", Result: dirty}},
	}
	beforeSan := snapshot(scSan)
	scSan.ToolResults[0].Result = clean
	diffSan := diffOnly(beforeSan, writable(scSan))

	// weather 副本：只读，不改
	scWea := &sdk.StageContext{
		RawMessage:  "查天气",
		UserID:      "u1",
		Phase:       sdk.StageAfterToolcall,
		ToolResults: []sdk.ToolResult{{CallID: "c1", Name: "weather_query", Result: dirty}},
	}
	beforeWea := snapshot(scWea)
	diffWea := diffOnly(beforeWea, writable(scWea))

	// weather 必须零回传，否则它的旧快照会覆盖 sanitizer 的清洗
	if len(diffWea) != 0 {
		t.Fatalf("weather 只读却回传 %v —— 会覆盖 sanitizer 清洗结果", diffWea)
	}
	// sanitizer 必须回传 tool_results
	if _, ok := diffSan["tool_results"]; !ok {
		t.Fatalf("sanitizer 改写了 tool_results 却未回传：%v", diffSan)
	}

	// 内核按 sanitizer → weather 顺序应用 diff（weather 后到，是最坏情形）
	kernel := map[string]interface{}{}
	for k, v := range kernelSnapshot {
		kernel[k] = v
	}
	for k, v := range diffSan {
		kernel[k] = v
	}
	for k, v := range diffWea {
		kernel[k] = v
	}

	res, _ := kernel["tool_results"].([]sdk.ToolResult)
	if len(res) == 0 || res[0].Result != clean {
		t.Fatalf("清洗结果被覆盖：期望 %q，实际 %v", clean, kernel["tool_results"])
	}
}
