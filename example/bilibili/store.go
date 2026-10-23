package main

// 本地视频记录库：BV 号查重的事实来源。
//
// ## 为什么需要它（而不是问平台）
//
// 「这个 BV 号我是不是已经看过了」在 B 站**没有**对应接口：收藏、历史、
// 稍后再看都要求登录态。而本插件刻意不依赖登录态（示例要能开箱即用），
// 于是查重只能基于**本插件自己见过的东西** —— 每次成功取回视频信息就落一条
// 本地记录。这是一份"我处理过什么"的台账，不是"平台上有哪些视频"的镜像。
//
// ## 为什么单独一个文件、还要加锁
//
// 记录是插件进程内的**共享可变状态**：工具调用可能并发（模型一轮发多个
// tool_call），读改写必须成对加锁；写盘走 tmp+rename，避免进程在写一半时
// 被杀留下半个 JSON —— 那种文件会让下一次加载直接失败，而使用者只会看到
// 「查重结果全空」。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// videoRecord 是本插件见过的一个视频。
type videoRecord struct {
	BVID      string `json:"bvid"`
	AID       int64  `json:"aid"`
	Title     string `json:"title"`
	UP        string `json:"up"`
	Duration  int64  `json:"duration_sec"`
	Pages     int    `json:"pages"`
	FirstSeen string `json:"first_seen"`
	LastSeen  string `json:"last_seen"`
	// SeenCount 是"被本插件取回信息的次数"，用于区分"偶尔碰到"与"反复处理"。
	SeenCount int `json:"seen_count"`
	// SubtitleFiles 记录导出过的字幕文件（相对数据目录），便于事后回看。
	SubtitleFiles []string `json:"subtitle_files,omitempty"`
}

type recordFile struct {
	Version int           `json:"version"`
	Updated string        `json:"updated"`
	Videos  []videoRecord `json:"videos"`
}

// store 是记录库。零值不可用，必须经 newStore 构造。
type store struct {
	dir string

	mu      sync.Mutex
	loaded  bool
	records map[string]*videoRecord // key 是规范化后的 BVID
}

func newStore(dir string) *store {
	return &store{dir: dir, records: map[string]*videoRecord{}}
}

func (s *store) path() string { return filepath.Join(s.dir, "records.json") }

// displayID 返回记录对外展示的编号。
//
// 为什么不直接用 r.BVID：平台在个别响应里只回 aid 不回 bvid，
// 而 fetchVideo 的守卫只拦「两者都空」（api.go 的 `BVID == "" && AID == 0`），
// 于是 BVID 为空、AID 非空的记录会正常进台账。直接渲染 r.BVID 会得到
// 一个空白编号（"命中： 已见过。" / " 1.   标题"），使用者看不出是哪条。
// 这里按 aid 兜底成 "avN"，与 videoRef.String() 的兜底口径一致。
func (r videoRecord) displayID() string {
	if r.BVID != "" {
		return r.BVID
	}
	if r.AID > 0 {
		return fmt.Sprintf("av%d", r.AID)
	}
	// 两者都空在 upsert 处已被拒绝，走到这里只可能是外部改坏的台账文件。
	// 给一个显式占位，不用空串——空白会被误读成「渲染丢了字段」。
	return "(编号缺失)"
}

// normKey 让同一个视频的不同写法落到同一条记录上。
//
// 优先用 BVID（用户几乎总是给 BV 号）；只有 av 号时用 "av<aid>" 兜底，
// 因为 aid→BV 的换算虽然可逆，但把用户给的 av 号悄悄换成 BV 号会让
// 记录/回显与用户输入对不上。
func normKey(r videoRef) string {
	if r.BVID != "" {
		return r.BVID
	}
	return fmt.Sprintf("av%d", r.AID)
}

// load 惰性加载一次。文件不存在是**正常状态**（首次使用），不是错误。
func (s *store) load() error {
	if s.loaded {
		return nil
	}
	raw, err := os.ReadFile(s.path())
	if err != nil {
		if os.IsNotExist(err) {
			s.loaded = true
			return nil
		}
		return fmt.Errorf("读取本地记录 %s 失败：%w", s.path(), err)
	}
	var f recordFile
	if err := json.Unmarshal(raw, &f); err != nil {
		// 不静默清空：那样会让使用者以为"记录没了但一切正常"。
		// 报错并给出可执行的处置建议。
		return fmt.Errorf("本地记录 %s 已损坏（%v）：请删除该文件后重试（会失去历史查重数据）", s.path(), err)
	}
	for i := range f.Videos {
		v := f.Videos[i]
		s.records[v.BVID] = &v
		if v.AID > 0 {
			s.records[fmt.Sprintf("av%d", v.AID)] = &v
		}
	}
	s.loaded = true
	return nil
}

// persist 原子落盘（tmp + rename）。
func (s *store) persist() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("创建数据目录 %s 失败：%w", s.dir, err)
	}
	seen := map[*videoRecord]bool{}
	var list []videoRecord
	for _, v := range s.records {
		if seen[v] {
			continue
		}
		seen[v] = true
		list = append(list, *v)
	}
	// 按首次见到时间倒序：最新关注的在前面，列表截断时丢的是最旧的。
	sort.Slice(list, func(i, j int) bool { return list[i].FirstSeen > list[j].FirstSeen })

	f := recordFile{
		Version: 1,
		Updated: time.Now().Format(time.RFC3339),
		Videos:  list,
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化本地记录失败：%w", err)
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("写入本地记录失败：%w", err)
	}
	if err := os.Rename(tmp, s.path()); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("替换本地记录失败：%w", err)
	}
	return nil
}

// lookup 只查不写，用于「先查重再决定要不要拉接口」。
func (s *store) lookup(r videoRef) (*videoRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil, err
	}
	v := s.records[normKey(r)]
	if v == nil && r.BVID != "" && r.AID > 0 {
		// 用户给的 av 号可能已登记在 BVID 键下（或反之），两边都试。
		v = s.records[fmt.Sprintf("av%d", r.AID)]
	}
	if v == nil {
		return nil, nil
	}
	cp := *v
	return &cp, nil
}

// upsert 登记一次成功取回的视频，返回**此前是否已存在**（即查重命中）。
func (s *store) upsert(rec videoRecord) (existed bool, prev *videoRecord, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return false, nil, err
	}

	// 两个编号都为空时无法作为记录主键：key 会退化成 "av0"，
	// 一堆脏数据全挤到同一条上。宁可直接拒绝。
	if rec.BVID == "" && rec.AID <= 0 {
		return false, nil, fmt.Errorf("登记记录缺少编号（BVID 与 AID 均为空），拒绝写入")
	}
	key := rec.BVID
	if key == "" {
		key = fmt.Sprintf("av%d", rec.AID)
	}
	now := time.Now().Format(time.RFC3339)
	old := s.records[key]
	if old == nil && rec.BVID != "" {
		old = s.records[fmt.Sprintf("av%d", rec.AID)]
	}
	if old != nil {
		cp := *old
		prev = &cp
		existed = true
		// 早先只登记过 av 号（或反之）时补上另一个键，
		// 否则用另一种写法查重会"未命中"——那是同一份记录。
		if old.BVID == "" && rec.BVID != "" {
			old.BVID = rec.BVID
			s.records[rec.BVID] = old
		}
		if old.AID == 0 && rec.AID > 0 {
			old.AID = rec.AID
			s.records[fmt.Sprintf("av%d", rec.AID)] = old
		}
		old.LastSeen = now
		old.SeenCount++
		// 标题/UP 可能被改过，用最新的覆盖；FirstSeen 保持不动。
		if rec.Title != "" {
			old.Title = rec.Title
		}
		if rec.UP != "" {
			old.UP = rec.UP
		}
		if rec.Duration > 0 {
			old.Duration = rec.Duration
		}
		if rec.Pages > 0 {
			old.Pages = rec.Pages
		}
		if err := s.persist(); err != nil {
			return existed, prev, err
		}
		return existed, prev, nil
	}

	nr := rec
	nr.FirstSeen = now
	nr.LastSeen = now
	nr.SeenCount = 1
	s.records[key] = &nr
	if nr.AID > 0 {
		// 同一个指针挂在两个键下（BVID 与 avN），persist 会去重。
		s.records[fmt.Sprintf("av%d", nr.AID)] = &nr
	}
	if err := s.persist(); err != nil {
		return false, nil, err
	}
	return false, nil, nil
}

// addSubtitleFile 把导出的字幕文件名记到对应记录上（记录不存在则忽略）。
func (s *store) addSubtitleFile(r videoRef, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	v := s.records[normKey(r)]
	if v == nil && r.AID > 0 {
		v = s.records[fmt.Sprintf("av%d", r.AID)]
	}
	if v == nil {
		return nil
	}
	for _, f := range v.SubtitleFiles {
		if f == name {
			return nil
		}
	}
	v.SubtitleFiles = append(v.SubtitleFiles, name)
	return s.persist()
}

// list 按首次见到时间倒序返回前 limit 条（limit<=0 表示不限）。
func (s *store) list(limit int) ([]videoRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil, err
	}
	seen := map[*videoRecord]bool{}
	var out []videoRecord
	for _, v := range s.records {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FirstSeen == out[j].FirstSeen {
			return out[i].BVID < out[j].BVID
		}
		return out[i].FirstSeen > out[j].FirstSeen
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// remove 删除一条记录，返回是否真的删掉了。
func (s *store) remove(r videoRef) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return false, err
	}
	v := s.records[normKey(r)]
	if v == nil && r.AID > 0 {
		v = s.records[fmt.Sprintf("av%d", r.AID)]
	}
	if v == nil {
		return false, nil
	}
	// 两个键都指向同一个指针，必须一起删。
	for k, rec := range s.records {
		if rec == v {
			delete(s.records, k)
		}
	}
	return true, s.persist()
}

// count 返回去重后的记录条数（供 health/说明使用）。
func (s *store) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return 0
	}
	seen := map[*videoRecord]bool{}
	n := 0
	for _, v := range s.records {
		if seen[v] {
			continue
		}
		seen[v] = true
		n++
	}
	return n
}

// validateOutputDir 校验配置里的输出/缓存目录。
//
// 为什么要拦：这个目录是本插件**唯一会写盘**的地方，而它是用户可配的。
// 配成 /etc、/usr、/ 这类系统目录时，一旦写入逻辑有 bug（比如拼错文件名）
// 后果就会溢出到系统里；示例代码被拷走后这种错误更容易发生。
//
// 判据：规范化后既不能是根，也不能落在系统管理目录**内部**
// （只比相等是不够的 —— "/etc/bilibili" 同样是系统目录，一样不该写）。
func validateOutputDir(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("输出目录为空")
	}
	cleaned := filepath.Clean(dir)
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("输出目录必须是绝对路径，得到 %q", dir)
	}
	if cleaned == string(filepath.Separator) {
		return "", fmt.Errorf("输出目录不能是根目录 %s", cleaned)
	}
	// 与 example/bili 的清单保持一致（外加 /lib、/lib64），并对标 /var 本身
	// ——/var/log 之类同样是系统管辖范围。
	for _, root := range []string{"/etc", "/usr", "/bin", "/sbin", "/boot", "/dev", "/proc", "/sys", "/lib", "/lib64", "/var"} {
		if cleaned == root || strings.HasPrefix(cleaned, root+string(filepath.Separator)) {
			return "", fmt.Errorf("输出目录不能是系统目录（%s 之下）：%s", root, cleaned)
		}
	}
	return cleaned, nil
}

// removeAll 删除整个台账文件（插件被卸载时用）。
//
// 与 remove 的区别：那个删一条，这个删文件本身。不存在不算错误 ——
// 卸载一个从未用过的插件不该报错。
func (s *store) removeAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = map[string]*videoRecord{}
	s.loaded = true
	if err := os.Remove(s.path()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
