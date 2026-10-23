package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ===== 宿主环境判据（默认 skip，须显式开）=====
//
// 这两条判据**把宿主机器的状态当成断言对象**：它读这台机器真实的
// config.db / log 目录，然后断言"所有 LLM 源都配全了"。
//
// ## 为什么默认关掉（这是 2026-10-21 修的一条真实假绿+假红）
//
// 原来写死 `realCfg = "/home/newqqagent/config.db"`：
//
//	本机            → 文件在 → 断言 source_failed == 0 → 而它实际是 1 → **红**
//	CI / 别人的机器  → 文件不在 → t.Skip → **假绿**
//
// 两种环境下的结果都不是判据：红的时候它证明的是"这台机器的配置有问题"
// （那是**运维信号**，不是代码缺陷），绿的时候它什么都没证明。
//
// ## 现在怎么用
//
//	HA_E2E_REAL_DB=<路径>      诊断真实内核 config.db
//	HA_E2E_REAL_LOG_DIR=<路径> 扫描真实日志目录
//
// 不开就 skip，且**把为什么 skip 打印出来**（不是一句 "skipping"）：
// 下一个人看到 skip 必须能自己判断"要不要开、开了会断言什么"。
//
// 与逻辑判据（下面 TestDiagDB* / TestDiagLogScan* 的夹具版）分开：
// 夹具版验"诊断逻辑对不对"，这两条验"这台机器的配置/日志现在什么样"。
var realCfg = os.Getenv("HA_E2E_REAL_DB")
var realLog = os.Getenv("HA_E2E_REAL_LOG_DIR")

func TestDiagTriage(t *testing.T) {
	p := &Plugin{name: "recoverydiag"}

	cases := []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"signal", map[string]interface{}{"exit_code": 0, "signal": "SIGSEGV"}, "process_death"},
		{"oom", map[string]interface{}{"exit_code": 0, "signal": "SIGKILL", "crash_reason": "oom-kill"}, "process_starvation"},
		{"nonzero", map[string]interface{}{"exit_code": 1}, "process_death"},
		{"healthy", map[string]interface{}{"exit_code": 0}, "normal_stop"},
		{"alive", map[string]interface{}{"still_alive": true, "signal": "SIGKILL"}, "config_unreachable"},
	}
	for _, c := range cases {
		r, _ := p.handleTriage(c.args)
		m, ok := r.(map[string]interface{})
		if !ok {
			t.Fatalf("%s: not a map", c.name)
		}
		if got, _ := m["class"].(string); got != c.want {
			t.Errorf("%s: class = %q, want %q", c.name, got, c.want)
		}
	}
}

// ===== config.db 诊断：自造夹具（默认跑，带真断言）=====

// TestDiagDBFlagsSourceMissingRequiredFields 用**自造**的 config.db 验证诊断逻辑。
//
// 夹具刻意做成"一个源正常 + 一个源故意缺字段"：
//
//	good     base_url / model / adapter 齐全          → 不参与 source_failed
//	broken   三个必备字段全缺                          → 必须被点出来
//
// 断言的是诊断**结构**（谁失败、缺哪些字段、verdict 是什么），
// 与这台机器上的真实配置无关 —— 于是同样的输入在任何机器上都得同样的结论。
func TestDiagDBFlagsSourceMissingRequiredFields(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "config.db")
	seedConfigDB(t, db, map[string]map[string]string{
		"good": {
			"base_url": "http://127.0.0.1:8081/v1",
			"model":    "AUTO",
			"adapter":  "openai",
			"api_key":  "enc:v1:not-a-real-secret",
		},
		"broken": {
			// 三个必备字段一个都没有；priority 之类的非必备字段有值也不能救它
			"base_url": "",
			"priority": "0",
		},
	})

	p := &Plugin{name: "recoverydiag", dataDir: dir, cfgPath: db}
	r, err := p.handleDB(map[string]interface{}{"db_path": db})
	if err != nil {
		t.Fatalf("handleDB: %v", err)
	}
	m := r.(map[string]interface{})
	t.Logf("integrity=%v sources=%v failed=%v missing=%v verdict=%v",
		m["integrity"], m["source_count"], m["source_failed"], m["missing_fields"], m["verdict"])

	if m["integrity"] != "ok" {
		t.Errorf("integrity = %v, want ok（夹具是完整的 sqlite 文件）", m["integrity"])
	}
	if got, _ := m["source_count"].(int); got != 2 {
		t.Errorf("source_count = %v, want 2（好源 + 坏源都要被枚举出来）", m["source_count"])
	}
	if got, _ := m["source_failed"].(int); got != 1 {
		t.Fatalf("★ source_failed = %v, want 1 —— 缺必备字段的源没有被判失败", got)
	}
	// 必须点明**是哪个源**、**缺哪几个字段**，否则用户还得自己比对
	var missing []string
	switch v := m["missing_fields"].(type) {
	case []string:
		missing = v
	case []interface{}:
		for _, x := range v {
			missing = append(missing, x.(string))
		}
	}
	joined := strings.Join(missing, ";")
	if !strings.Contains(joined, "broken:") {
		t.Errorf("★ missing_fields 没点出 broken 源：%v", missing)
	}
	for _, field := range []string{"base_url", "model", "adapter"} {
		if !strings.Contains(joined, field) {
			t.Errorf("★ missing_fields 没列出缺的字段 %q：%v", field, missing)
		}
	}
	if m["verdict"] != "degraded" {
		t.Errorf("verdict = %v, want degraded（库完整但有源缺字段）", m["verdict"])
	}
	if s, _ := m["summary"].(string); !strings.Contains(s, "broken") {
		t.Errorf("summary 应向人指出是哪个源：%q", s)
	}
}

// TestDiagDBReportsZeroFailedWhenAllSourcesComplete 是全好的那一侧：
// 没有任何源缺字段时必须报 0 / ok（防止"永远 degraded"的假实现蒙过去）。
func TestDiagDBReportsZeroFailedWhenAllSourcesComplete(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "config.db")
	seedConfigDB(t, db, map[string]map[string]string{
		"good": {"base_url": "http://127.0.0.1:8081/v1", "model": "AUTO", "adapter": "openai"},
		"aux":  {"base_url": "http://127.0.0.1:8082/v1", "model": "aux", "adapter": "openai-messages"},
	})

	p := &Plugin{name: "recoverydiag", dataDir: dir, cfgPath: db}
	r, err := p.handleDB(map[string]interface{}{"db_path": db})
	if err != nil {
		t.Fatalf("handleDB: %v", err)
	}
	m := r.(map[string]interface{})
	if got, _ := m["source_count"].(int); got != 2 {
		t.Errorf("source_count = %v, want 2", m["source_count"])
	}
	if got, _ := m["source_failed"].(int); got != 0 {
		t.Fatalf("★ source_failed = %v, want 0（两个源都齐全）: %v", got, m["missing_fields"])
	}
	if m["verdict"] != "ok" {
		t.Errorf("verdict = %v, want ok", m["verdict"])
	}
}

// TestDiagDBAbsentFileIsHighRisk 是第三种输入：文件不存在。
// 诊断必须把它当"配置损坏类高危信号"，而不是一个空报告。
func TestDiagDBAbsentFileIsHighRisk(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.db")
	p := &Plugin{name: "recoverydiag", dataDir: dir, cfgPath: missing}
	r, err := p.handleDB(map[string]interface{}{"db_path": missing})
	if err != nil {
		t.Fatalf("handleDB: %v", err)
	}
	m := r.(map[string]interface{})
	if m["exists"] != false {
		t.Errorf("exists = %v, want false", m["exists"])
	}
	if m["integrity"] != "absent" {
		t.Errorf("integrity = %v, want absent", m["integrity"])
	}
	if s, _ := m["summary"].(string); !strings.Contains(s, "缺失") {
		t.Errorf("summary 应指出文件缺失：%q", s)
	}
}

// TestDiagDBRejectsPathOutsideDataDir 钉住安全校验：
// db_path 只允许数据目录内的 sqlite 文件（防止被用作任意文件探测）。
func TestDiagDBRejectsPathOutsideDataDir(t *testing.T) {
	dataDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "elsewhere.db")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &Plugin{name: "recoverydiag", dataDir: dataDir}
	r, err := p.handleDB(map[string]interface{}{"db_path": outside})
	if err != nil {
		t.Fatalf("handleDB: %v", err)
	}
	m := r.(map[string]interface{})
	// 拒绝时返回 content(...) —— 只有 "content" 一个键，没有任何探测结果
	if _, ok := m["exists"]; ok {
		t.Fatalf("★ 数据目录外的路径被当成合法 db 探测了：%v", m)
	}
	if s, _ := m["content"].(string); !strings.Contains(s, "数据目录内") {
		t.Errorf("拒绝文案应说明路径限制：%q", s)
	}
}

// ===== config.db 诊断：Settings 回退路径（无可用的 sqlite3 CLI 时）=====

// TestDiagDBCorruptFileIsNotReportedHealthy 钉住"损坏的库不能被报成健康"。
//
// 输入是一个**不是 sqlite 的文件**（文本）。sqlite3 对它做 PRAGMA 会
// 报 "file is not a database" 并非零退出 ⇒ 诊断必须落到 fail/error，
// 不能因为"查不出东西"就回 ok。
func TestDiagDBCorruptFileIsNotReportedHealthy(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "config.db")
	if err := os.WriteFile(db, []byte("this is definitely not a sqlite file, just text"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &Plugin{name: "recoverydiag", dataDir: dir, cfgPath: db}
	r, err := p.handleDB(map[string]interface{}{"db_path": db})
	if err != nil {
		t.Fatalf("handleDB: %v", err)
	}
	m := r.(map[string]interface{})
	t.Logf("非 sqlite：integrity=%v verdict=%v summary=%v",
		m["integrity"], m["verdict"], m["summary"])
	if m["integrity"] == "ok" {
		t.Errorf("★ 非 sqlite 文件被报成 integrity=ok —— 配置损坏会被漏诊：%v", m)
	}
	if m["verdict"] == "ok" {
		t.Errorf("★ 非 sqlite 文件被报成 verdict=ok：%v", m)
	}
}

// TestDiagDBSettingsFallbackIsDistinguishable 让 Settings 回退分支**必然**被走到，
// 并证明"走的是回退"而不是"走了 CLI 但碰巧没数据"。
//
// ## 为什么需要专门一条（本机有 sqlite3 时那条分支默认走不到）
//
// p.dbSources() 先试 sqlite3 CLI，不可用才回退 Settings。
// 本机装了 sqlite3 ⇒ 回退分支**永远不执行** ⇒ 只靠"本机跑一遍"是假覆盖。
//
// ## 怎么做到"必然走到回退"，且与设备状态无关
//
// 关键是 p.sqliteBin() 的取值链：Settings 的 db_check_cmd 若指向一个
// **LookPath 找不到**的命令，sqliteBin() 返回 ""（它不会退而求其次，
// 见实现），于是 dbSources 直接走 Settings 分支。
//
// 这里把 db_check_cmd 设成 `definitely-no-such-sqlite-binary-xyz`：
//
//	· 若实现走 CLI 分支（错误地忽略 LookPath 失败）→ kv 空 → 源数 0
//	· 实现正确回退 Settings                      → 拿到 2 个源
//
// 而夹具里**故意在真库里放一个诱饵源 decoy**：万一实现偷偷用了真 sqlite3，
// 拿到的会是 decoy ⇒ 断言立刻指出"夹具没把 CLI 分支堵死"。
// 三种可能的结果彼此可区分 —— 这才叫判据，而不是"本机碰巧走哪条"。
//
// ★ 这条判据同时钉住"回退分支用的是 Settings 而不是别的来源"：
// Settings 里两条源名叫 settings1/settings2，与库里的 decoy 完全不同。
func TestDiagDBSettingsFallbackIsDistinguishable(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "config.db")
	seedConfigDB(t, db, map[string]map[string]string{
		// 诱饵：只有"错误地走了真 sqlite3"才会读到它
		"decoy": {"base_url": "http://decoy/v1", "model": "d", "adapter": "openai"},
	})

	fake := &fakeSettings{
		values: map[string]interface{}{"db_check_cmd": "definitely-no-such-sqlite-binary-xyz"},
		core: map[string]interface{}{
			"core.llm.sources.settings1.base_url": "http://127.0.0.1:9001/v1",
			"core.llm.sources.settings1.model":    "m1",
			"core.llm.sources.settings1.adapter":  "openai",
			"core.llm.sources.settings2.base_url": "http://127.0.0.1:9002/v1",
			"core.llm.sources.settings2.model":    "m2",
			// settings2 缺 adapter → 必须被判失败
		},
	}

	// 直接测 dbSources：这是"源从哪来"的唯一入口，比走 handleDB 更好定位
	p := &Plugin{name: "recoverydiag", dataDir: dir, cfgPath: db, sdk: newFakeSDK(fake)}
	sources, errTxt := p.dbSources(db)
	if errTxt != "" {
		t.Logf("dbSources 附带诊断：%s", errTxt)
	}
	if len(sources) != 2 {
		t.Fatalf("★ 源数量 = %d, want 2。得到 1 且名叫 decoy ⇒ 夹具没堵死 CLI 分支（判据是假的）；"+
			"得到 0 ⇒ 两条分支都没成。实际：%+v", len(sources), sources)
	}
	var names []string
	for _, s := range sources {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "settings1,settings2" {
		t.Fatalf("★ 源名 = %v, want [settings1 settings2]", names)
	}
	byName := map[string]llmSourceData{}
	for _, s := range sources {
		byName[s.Name] = s
	}
	if !byName["settings1"].OK {
		t.Errorf("settings1 字段齐全，不该判失败：%+v", byName["settings1"])
	}
	if byName["settings2"].OK {
		t.Errorf("★ settings2 缺 adapter 却判成 OK：%+v", byName["settings2"])
	}
	if !strings.Contains(strings.Join(byName["settings2"].Missing, ","), "adapter") {
		t.Errorf("settings2 的 missing 应含 adapter：%v", byName["settings2"].Missing)
	}
	for _, s := range sources {
		if s.Name == "decoy" {
			t.Fatalf("★ 出现了诱饵源 decoy —— 说明走的是 sqlite3 分支，Settings 回退未被覆盖")
		}
	}
	t.Logf("✅ Settings 回退分支被真实覆盖：%+v", sources)
}

// ===== config.db 诊断：真实宿主（默认 skip）=====

func TestDiagDBOnRealHost(t *testing.T) {
	if realCfg == "" {
		t.Skip("跳过：未设 HA_E2E_REAL_DB。" +
			"本判据对**这台机器真实的** config.db 断言（要读内核数据目录，取决于本机装了什么），" +
			"所以默认关。开了它会断言：库完整性=ok、能枚举出 LLM 源、" +
			"且**所有源都配齐了**（source_failed=0）。" +
			"用法：HA_E2E_REAL_DB=/path/to/config.db go test -run TestDiagDBOnRealHost -v")
	}
	if _, err := os.Stat(realCfg); err != nil {
		t.Fatalf("HA_E2E_REAL_DB=%s 不存在: %v", realCfg, err)
	}
	p := &Plugin{name: "recoverydiag", dataDir: filepath.Dir(realCfg), cfgPath: realCfg}
	r, err := p.handleDB(map[string]interface{}{"db_path": realCfg})
	if err != nil {
		t.Fatalf("handleDB: %v", err)
	}
	m := r.(map[string]interface{})
	t.Logf("integrity=%v sources=%v verdict=%v summary=%v",
		m["integrity"], m["source_count"], m["verdict"], m["summary"])
	if m["integrity"] != "ok" {
		t.Errorf("integrity = %v, want ok", m["integrity"])
	}
	if m["source_count"] == 0 {
		t.Errorf("source_count == 0, expected LLM sources")
	}
	if got, _ := m["source_failed"].(int); got != 0 {
		t.Errorf("source_failed = %d, want 0（本机所有源都该配齐）: %v",
			got, m["missing_fields"])
	}
}

// ===== 日志扫描：自造夹具 + 真实宿主 =====

// TestDiagLogScanCountsSignatures 用自造日志验签名统计与主导结论。
func TestDiagLogScanCountsSignatures(t *testing.T) {
	dir := t.TempDir()
	// 文件名要符合 handleLogScan 的筛选：<任意>.log
	content := strings.Join([]string{
		"2026/10/21 10:00:01 panic: runtime error: invalid memory address",
		"2026/10/21 10:00:02 [commondev] 台账回填：br-a → done",
		"2026/10/21 10:00:03 panic: nil pointer dereference",
		"2026/10/21 10:00:04 dial tcp: connection refused",
		"2026/10/21 10:00:05 everything is fine here",
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "homed_2026-10-21_10-00-00.log"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// 非 .log 文件必须被忽略（归档/其它文件不该参与统计）
	if err := os.WriteFile(filepath.Join(dir, "homed_2026-10-20.log.gz"), []byte("panic"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Plugin{name: "recoverydiag", logDir: dir}
	r, err := p.handleLogScan(map[string]interface{}{"log_dir": dir})
	if err != nil {
		t.Fatalf("handleLogScan: %v", err)
	}
	m := r.(map[string]interface{})
	t.Logf("files=%v scanned=%v matched=%v counts=%v dominant=%v",
		m["files_read"], m["lines_scanned"], m["lines_matched"], m["counts"], m["dominant"])

	if got, _ := m["files_read"].(int); got != 1 {
		t.Errorf("files_read = %v, want 1（.log.gz 不该被读）", got)
	}
	counts, _ := m["counts"].(map[string]int)
	if counts == nil {
		if mm, ok := m["counts"].(map[string]interface{}); ok {
			counts = map[string]int{}
			for k, v := range mm {
				if n, ok := v.(int); ok {
					counts[k] = n
				}
			}
		}
	}
	if counts["panic"] != 2 {
		t.Errorf("panic 计数 = %d, want 2（两行 panic）: %v", counts["panic"], counts)
	}
	if counts["network"] != 1 {
		t.Errorf("network 计数 = %d, want 1（connection refused）: %v", counts["network"], counts)
	}
	if m["dominant"] != "panic" {
		t.Errorf("dominant = %v, want panic（计数最多）", m["dominant"])
	}
	if s, _ := m["conclusion"].(string); !strings.Contains(s, "panic") {
		t.Errorf("conclusion 应点出主导类别：%q", s)
	}
}

// TestDiagLogScanUnreadableDirIsReportedAsContent 钉住"目录不可读"的表达方式：
// 返回结论（不 panic、不静默成功）。
func TestDiagLogScanUnreadableDirIsReportedAsContent(t *testing.T) {
	p := &Plugin{name: "recoverydiag"}
	nope := filepath.Join(t.TempDir(), "no-such-dir")
	r, err := p.handleLogScan(map[string]interface{}{"log_dir": nope})
	if err != nil {
		t.Fatalf("handleLogScan 不该返回 error: %v", err)
	}
	m := r.(map[string]interface{})
	s, _ := m["content"].(string)
	if !strings.Contains(s, "不可读") {
		t.Errorf("应回报目录不可读：%v", m)
	}
}

func TestDiagLogScanOnRealHost(t *testing.T) {
	if realLog == "" {
		t.Skip("跳过：未设 HA_E2E_REAL_LOG_DIR。" +
			"本判据扫**这台机器真实的**日志目录（结果取决于本机最近发生过什么），默认关。" +
			"用法：HA_E2E_REAL_LOG_DIR=/path/to/log go test -run TestDiagLogScanOnRealHost -v")
	}
	if _, err := os.Stat(realLog); err != nil {
		t.Fatalf("HA_E2E_REAL_LOG_DIR=%s 不存在: %v", realLog, err)
	}
	p := &Plugin{name: "recoverydiag"}
	r, err := p.handleLogScan(map[string]interface{}{
		"log_dir":       realLog,
		"since_minutes": 60 * 24 * 3,
	})
	if err != nil {
		t.Fatalf("handleLogScan: %v", err)
	}
	m := r.(map[string]interface{})
	t.Logf("matched=%v counts=%v dominant=%v conclusion=%v",
		m["lines_matched"], m["counts"], m["dominant"], m["conclusion"])
}

// ===== 其余判据（保持原样）=====

func TestDiagDelta(t *testing.T) {
	base := t.TempDir()
	cur := t.TempDir()
	sub := filepath.Join(base, "sub")
	os.MkdirAll(sub, 0755)

	// modified: same path, different content
	os.WriteFile(filepath.Join(base, "a.txt"), []byte("hello"), 0644)
	os.WriteFile(filepath.Join(cur, "a.txt"), []byte("world!"), 0644)
	// created
	os.WriteFile(filepath.Join(cur, "b.txt"), []byte("new"), 0644)
	// deleted
	os.WriteFile(filepath.Join(base, "gone.txt"), []byte("bye"), 0644)
	// unchanged
	os.WriteFile(filepath.Join(base, "same.txt"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(cur, "same.txt"), []byte("x"), 0644)

	p := &Plugin{name: "recoverydiag"}
	r, err := p.handleDelta(map[string]interface{}{"baseline_dir": base, "current_dir": cur})
	if err != nil {
		t.Fatalf("handleDelta: %v", err)
	}
	m := r.(map[string]interface{})
	sum := m["summary"].(map[string]int)
	t.Logf("summary=%v total=%v", sum, m["total_diff"])
	if sum["created"] != 1 || sum["deleted"] != 1 || sum["modified"] != 1 {
		t.Errorf("summary = %v, want modified=1 created=1 deleted=1", sum)
	}
}

func TestDiagLoc(t *testing.T) {
	p := &Plugin{name: "recoverydiag"}
	r, _ := p.handleLoc(map[string]interface{}{
		"triage":   map[string]interface{}{"class": "process_death", "verdict": "down"},
		"db":       map[string]interface{}{"verdict": "ok"},
		"log_scan": map[string]interface{}{"dominant": "panic"},
		"delta":    map[string]interface{}{"summary": map[string]interface{}{"created": 0, "modified": 0, "deleted": 0}},
	})
	m := r.(map[string]interface{})
	// 经 JSON 往返，模拟内核把子结论以 JSON 传给 diag_loc 的真实路径
	raw, _ := json.Marshal(m)
	var dec map[string]interface{}
	json.Unmarshal(raw, &dec)
	hs := dec["ranked_hypotheses"].([]interface{})
	if len(hs) == 0 {
		t.Fatal("no hypotheses")
	}
	top := hs[0].(map[string]interface{})
	t.Logf("top cause=%v conf=%v rec=%v", top["cause"], top["confidence"], top["recommendation"])
	if top["cause"] != "code_panic_loop" {
		t.Errorf("expected code_panic_loop, got %v", top["cause"])
	}
}

func TestDiagLocPersist(t *testing.T) {
	kb := filepath.Join(t.TempDir(), "recovery_kb")
	p := &Plugin{name: "recoverydiag", dataDir: filepath.Dir(kb)}
	args := map[string]interface{}{
		"persist":  true,
		"triage":   map[string]interface{}{"class": "process_death", "verdict": "down"},
		"db":       map[string]interface{}{"verdict": "ok"},
		"log_scan": map[string]interface{}{"dominant": "panic"},
		"delta":    map[string]interface{}{"summary": map[string]interface{}{"created": 0, "modified": 0, "deleted": 0}},
	}
	if _, err := p.handleLoc(args); err != nil {
		t.Fatalf("handleLoc: %v", err)
	}
	entries, err := os.ReadDir(kb)
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected persisted diag json, got err=%v entries=%v", err, entries)
	}
	data, _ := os.ReadFile(filepath.Join(kb, entries[0].Name()))
	if !strings.Contains(string(data), `"cause"`) {
		t.Errorf("persisted file missing cause field: %s", data)
	}
}
