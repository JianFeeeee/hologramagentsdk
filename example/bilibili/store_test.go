package main

// 本地查重台账的判据。
//
// 查重是**纯本地**能力，也是这个示例里唯一会持久化写盘的东西，
// 所以它的行为必须钉死：命中/未命中要分清，损坏要报错而不是静默清空。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreMissThenHit(t *testing.T) {
	dir := t.TempDir()
	s := newStore(dir)
	ref, err := parseRef("BV17x411w7KC")
	if err != nil {
		t.Fatal(err)
	}

	// 1) 未命中：必须返回 nil（调用方据此输出"未命中"）。
	got, err := s.lookup(ref)
	if err != nil {
		t.Fatalf("lookup 报错: %v", err)
	}
	if got != nil {
		t.Fatalf("空台账应未命中，却拿到了 %+v", got)
	}

	// 2) 登记后命中。
	existed, prev, err := s.upsert(videoRecord{
		BVID: ref.BVID, AID: ref.AID, Title: "测试视频", UP: "某UP", Duration: 61, Pages: 2,
	})
	if err != nil {
		t.Fatalf("upsert 报错: %v", err)
	}
	if existed || prev != nil {
		t.Fatalf("首次登记不应算命中（existed=%v prev=%+v）", existed, prev)
	}

	got, err = s.lookup(ref)
	if err != nil {
		t.Fatalf("lookup 报错: %v", err)
	}
	if got == nil {
		t.Fatal("登记后应命中，却未命中")
	}
	if got.Title != "测试视频" || got.SeenCount != 1 {
		t.Fatalf("命中记录不对: %+v", got)
	}
	if got.FirstSeen == "" || got.LastSeen == "" {
		t.Fatalf("时间戳未写入: %+v", got)
	}

	// 3) 用 av 号查同一条记录（两种编号要落到一起）。
	byAV, err := parseRef("av170001")
	if err != nil {
		t.Fatal(err)
	}
	if hit, _ := s.lookup(byAV); hit == nil {
		t.Fatal("用 av 号查不到同一条记录：两种编号没落到一起")
	}

	// 4) 再登记一次 → 命中且计数+1。
	existed, prev, err = s.upsert(videoRecord{BVID: ref.BVID, AID: ref.AID, Title: "测试视频v2"})
	if err != nil {
		t.Fatalf("第二次 upsert 报错: %v", err)
	}
	if !existed {
		t.Fatal("第二次登记应算命中（existed=false）")
	}
	if prev == nil || prev.SeenCount != 1 {
		t.Fatalf("prev 快照不对: %+v", prev)
	}
	if n := s.count(); n != 1 {
		t.Fatalf("count = %d, want 1（BV 与 av 两个键不能算两条）", n)
	}
	got, _ = s.lookup(ref)
	if got.SeenCount != 2 {
		t.Fatalf("SeenCount = %d, want 2", got.SeenCount)
	}
	if got.Title != "测试视频v2" {
		t.Fatalf("标题未按最新覆盖: %q", got.Title)
	}
}

func TestStorePersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	s1 := newStore(dir)
	if _, _, err := s1.upsert(videoRecord{BVID: "BV17x411w7KC", AID: 170001, Title: "持久"}); err != nil {
		t.Fatal(err)
	}
	// 新实例 = 插件重启：必须能从磁盘读回。
	s2 := newStore(dir)
	ref, _ := parseRef("BV17x411w7KC")
	got, err := s2.lookup(ref)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Title != "持久" {
		t.Fatalf("重启后读不回记录: %+v", got)
	}
}

func TestStoreCorruptFileIsLoud(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "records.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newStore(dir)
	ref, _ := parseRef("BV17x411w7KC")
	_, err := s.lookup(ref)
	if err == nil {
		t.Fatal("台账损坏时必须报错（静默清空会让用户以为数据正常丢失）")
	}
	if !strings.Contains(err.Error(), "损坏") {
		t.Fatalf("报错没说清是文件损坏: %v", err)
	}
}

func TestStoreRemove(t *testing.T) {
	dir := t.TempDir()
	s := newStore(dir)
	s.upsert(videoRecord{BVID: "BV17x411w7KC", AID: 170001})
	ref, _ := parseRef("av170001")

	ok, err := s.remove(ref)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("删除应成功")
	}
	if n := s.count(); n != 0 {
		t.Fatalf("删除后 count = %d, want 0（BV/av 两个键必须一起删）", n)
	}
	// 再删一次应返回"没删到"而不是报错。
	if ok, err := s.remove(ref); err != nil || ok {
		t.Fatalf("重复删除应返回 (false, nil)，得到 (%v, %v)", ok, err)
	}
}

func TestStoreListOrderAndLimit(t *testing.T) {
	dir := t.TempDir()
	s := newStore(dir)
	for i, bv := range []struct {
		bv  string
		aid int64
	}{{"BV17x411w7KC", 170001}, {"BV1xx411c7mD", 2}, {"BV1J14y1M713", 777927636}} {
		// FirstSeen 由 upsert 用当前时间生成；同一毫秒内三条顺序不可靠，
		// 所以这里只断言"条数与截断"，顺序另测。
		if _, _, err := s.upsert(videoRecord{BVID: bv.bv, AID: bv.aid, Title: "t"}); err != nil {
			t.Fatalf("第 %d 条: %v", i, err)
		}
	}
	all, err := s.list(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("list(0) = %d 条, want 3", len(all))
	}
	limited, err := s.list(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 {
		t.Fatalf("list(2) = %d 条, want 2", len(limited))
	}
}

// ★ validateOutputDir 是「输出目录安全校验」的判据。
//
// 只比相等是不够的："/etc/bilibili" 同样是系统目录下的路径。
func TestValidateOutputDir(t *testing.T) {
	for _, bad := range []string{
		"", "/", "/etc", "/etc/bilibili", "/usr/local/share/x", "/var", "/var/run/x",
		"relative/dir", "/proc/1", "/dev/shm/x", "/boot/x", "/lib/x",
	} {
		if _, err := validateOutputDir(bad); err == nil {
			t.Errorf("validateOutputDir(%q) 应拒绝，却通过了", bad)
		}
	}
	for _, ok := range []string{"/tmp/bilibili_exports", "/root/.local/share/bilibili", "/home/u/data"} {
		if _, err := validateOutputDir(ok); err != nil {
			t.Errorf("validateOutputDir(%q) 应接受: %v", ok, err)
		}
	}
}

func TestResolveDirRejectsTraversal(t *testing.T) {
	p := &Plugin{name: "bilibili", base: t.TempDir()}
	base := filepath.Join(p.base, "exports")
	cfg := pluginConfig{outputDir: base}

	// 相对路径应落在输出目录下。
	got, err := p.resolveDir("sub/dir", cfg)
	if err != nil {
		t.Fatalf("正常相对路径被拒: %v", err)
	}
	if !strings.HasPrefix(got, base) {
		t.Fatalf("相对路径没落到输出目录下: %q", got)
	}

	// ../ 跳出输出目录要被拦。
	if _, err := p.resolveDir("../../../etc", cfg); err == nil {
		t.Fatal("../../.. 越界未被拦住")
	}
	// 绝对路径也走同一套校验。
	if _, err := p.resolveDir("/etc", cfg); err == nil {
		t.Fatal("绝对路径 /etc 未被拦住")
	}
}

// 早先只登记过 av 号时，用 BV 号查也要命中（同一份记录）。
// 反过来的方向（先 BV 后 av）已在 TestStoreMissThenHit 里覆盖。
func TestStoreCrossKeyAfterPartialRecord(t *testing.T) {
	dir := t.TempDir()
	s := newStore(dir)
	// 模拟"只拿到过 aid"的场景（比如将来某个只给 aid 的接口）。
	if _, _, err := s.upsert(videoRecord{AID: 170001, Title: "只有 aid"}); err != nil {
		t.Fatal(err)
	}
	byBV, _ := parseRef("BV17x411w7KC")
	if got, err := s.lookup(byBV); err != nil || got == nil {
		t.Fatalf("用 BV 号查只有 aid 的记录应命中: %v %+v", err, got)
	}
	// 后续带 BVID 的登记应把另一个键补上，两个写法都能查到同一条。
	if _, _, err := s.upsert(videoRecord{BVID: byBV.BVID, AID: 170001, Title: "补全"}); err != nil {
		t.Fatal(err)
	}
	if n := s.count(); n != 1 {
		t.Fatalf("count = %d, want 1（同一份记录不能因为补键变成两条）", n)
	}
	av, _ := parseRef("av170001")
	for _, ref := range []videoRef{byBV, av} {
		got, err := s.lookup(ref)
		if err != nil || got == nil {
			t.Fatalf("%s 查不到: %v", ref, err)
		}
	}
}

func TestStoreRejectsRecordWithoutID(t *testing.T) {
	s := newStore(t.TempDir())
	if _, _, err := s.upsert(videoRecord{Title: "没有编号"}); err == nil {
		t.Fatal("缺编号的记录必须被拒绝（否则会全部挤到同一条上）")
	}
	if n := s.count(); n != 0 {
		t.Fatalf("count = %d, want 0", n)
	}
}
