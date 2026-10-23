package main

// bilibili · B站内容侧插件（HologramAgent 示例）
//
// ## 与 example/bili 的分工
//
// example/bili 做的是「下载」：调 yt-dlp 把视频落到磁盘。
// 本插件做的是「内容」：视频信息与分 P、字幕、评论、热门流，
// 外加一个**纯本地**的 BV 号查重台账。两者共存，互不依赖。
//
// ## 能力边界（刻意的，不是没做完）
//
//   - 不需要登录态：匿名可用的 Web 接口就够覆盖上面这些能力。
//   - 唯一的写操作是「发评论」，**默认关闭**，需要两件事同时成立才放行：
//     配置开关 allow_comment_post=true、且 Cookie 里含 bili_jct（csrf）。
//     示例不该给出一个"一键刷评论"的玩具。
//   - 不碰短链（b23.tv）、不碰番剧/直播：它们的接口与编号体系不同。
//
// ## 为什么每次调用都重读配置
//
// 用户改配置（换 Cookie、关掉发评论）后不应该需要重启插件才能生效。
// 重读走 p.mu 保护，因此即使工具被并发调度也不会读到半个快照。

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// 配置键。Category 一律填插件名，WebUI 设置页据此归组；
// 键名本身不加前缀——前缀会让 Category 的归组作用重复。
const (
	cfgOutputDir   = "output_dir"
	cfgAPIBase     = "api_base"
	cfgCookie      = "cookie"
	cfgAllowPost   = "allow_comment_post"
	cfgMaxItems    = "max_items"
	cfgMaxPages    = "max_pages"
	cfgTimeout     = "request_timeout"
	cfgUserAgent   = "user_agent"
	cfgSeenOnFetch = "record_on_fetch"
	cfgPageDelay   = "page_delay_ms"
)

const (
	// 默认输出目录名（落在插件数据目录之下）。
	defaultOutputSubdir = "exports"

	// 分页上限的**硬**上限：配置再大也不超过它。
	// 「要有上限」不是建议而是约束——热门流/评论都是可以一直翻下去的，
	// 没有硬上限就意味着一次工具调用可能打几十个请求并撞风控。
	hardMaxPages = 10
	hardMaxItems = 100

	defaultMaxItems = 20
	defaultMaxPages = 3

	// 单次工具调用的时间预算（分页循环共用一份预算，而不是每页各给一份）。
	budgetSubtitles = 45 * time.Second
	budgetComments  = 45 * time.Second
	budgetPopular   = 45 * time.Second
	budgetSingle    = 30 * time.Second

	// 评论正文发给平台前的上限。平台侧也有上限，但本地先拦能给可读错误，
	// 而不是让平台回一句含糊的「参数错误」。
	maxCommentRunes = 1000
)

// defaultPageDelay 是翻页之间的最小间隔。
//
// 纯礼貌性节流：B 站在几十毫秒内连发多个请求时很容易吃 -412，而这个间隔
// 对使用者几乎无感。做成**变量**而不是常量，是为了单测能置 0 ——
// 测试不该为了"礼貌"白等十几秒。
var defaultPageDelay = 150 * time.Millisecond

// pluginConfig 是一次调用期间使用的配置快照。
//
// 用快照而不是"用的时候直接读字段"：handler 里会调用多次，
// 中途用户改配置会让一次调用用上两套配置（例如先按 A 目录校验、再写进 B 目录）。
type pluginConfig struct {
	outputDir   string
	apiBase     string
	cookie      string
	allowPost   bool
	maxItems    int
	maxPages    int
	timeout     time.Duration
	userAgent   string
	recordOnGet bool
	pageDelay   time.Duration
}

type Plugin struct {
	name string
	sdk  *sdk.PluginSDK

	// mu 保护下面这些"每次调用重读"的字段。工具可能被并发调度，
	// 即使本插件只对纯本地工具声明了 ParallelSafe，也不该留竞态。
	mu   sync.RWMutex
	cfg  pluginConfig
	base string // 插件数据目录（Settings().DataDir()）

	cliMu sync.Mutex
	cli   *client

	// overrideClient 是给单测用的注入点：指向 httptest 服务器。
	// 生产路径永远是 nil（配置变了就重建 client）。
	overrideClient *client

	st *store
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s

	// 数据目录由内核给，保证存在；输出目录默认落在它下面的 exports/，
	// 这样"默认值"天然满足 validateOutputDir（不是系统目录）。
	dataDir := ""
	if st := s.Settings(); st != nil {
		dataDir = st.DataDir()
	}
	if dataDir == "" {
		return fmt.Errorf("内核未提供插件数据目录（Settings().DataDir() 为空），无法落盘本地查重记录")
	}
	// base 必须在 reload 之前落到 p 上：reload 用它算默认输出目录。
	p.mu.Lock()
	p.base = dataDir
	p.mu.Unlock()
	p.st = newStore(dataDir)

	p.registerConfig(s)
	p.reload()
	if err := p.validateConfig(); err != nil {
		return err
	}

	p.registerTools()

	// 删除插件时清掉自己产生的数据（记录台账 + 导出目录）。
	// 与 Stop 的区别：Stop 在每次重载时都会触发，重载不该丢数据。
	s.RegisterOnRemoveHandler(p.cleanupData)

	cfg := p.snapshot()
	log.Printf("[%s] started (api=%s output=%s 查重记录=%d 条 发评论=%v)",
		p.name, cfg.apiBase, cfg.outputDir, p.st.count(), cfg.allowPost)
	return nil
}

func (p *Plugin) Stop() error { return nil }

func (p *Plugin) registerConfig(s *sdk.PluginSDK) {
	st := s.Settings()
	cat := p.name
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgOutputDir, Type: "string", Default: "",
		DisplayName: "输出 / 缓存目录",
		Description: "字幕导出的落盘目录。留空 = 插件数据目录下的 exports/。" +
			"必须是绝对路径，且不能是系统目录（/etc、/usr、/var 等）。" +
			"查重台账不受此项影响，它始终在插件数据目录的 records.json",
		Category: cat,
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgAPIBase, Type: "string", Default: defaultAPIBase,
		DisplayName: "接口地址",
		Description: "默认 https://api.bilibili.com。只有自建反代时才需要改；改错会表现为「接口一律失败」",
		Category:    cat,
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgCookie, Type: "password", Default: "", Secret: true,
		DisplayName: "Cookie（可选）",
		Description: "登录态 Cookie 串（含 SESSDATA、bili_jct 等）。留空即以匿名身份访问：" +
			"字幕（AI 字幕常不下发）与风控表现都会更差；**发评论必须**有 Cookie 且含 bili_jct",
		Category: cat,
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgAllowPost, Type: "bool", Default: "false",
		DisplayName: "允许发评论（危险）",
		Description: "**默认关闭**。开启后 bilibili_comment_post 才可用，且仍需上面的 Cookie 含 bili_jct" +
			"（csrf 校验）。这是演示「带写能力的工具必须显式开启」的示例，不建议日常打开",
		Category: cat,
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgMaxItems, Type: "int", Default: strconv.Itoa(defaultMaxItems), Min: 1, Max: hardMaxItems,
		DisplayName: "单页条数", Description: "评论/热门每页条数上限，控制上下文体积",
		Category: cat,
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgMaxPages, Type: "int", Default: strconv.Itoa(defaultMaxPages), Min: 1, Max: hardMaxPages,
		DisplayName: "最多翻页数", Description: "分页类工具的翻页上限（硬上限 10）。翻页越多越容易触发风控",
		Category: cat,
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgTimeout, Type: "int", Default: "15", Min: 1, Max: 120,
		DisplayName: "单次请求超时（秒）", Category: cat,
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgUserAgent, Type: "string", Default: defaultUserAgent,
		DisplayName: "User-Agent", Description: "不带浏览器 UA 时部分接口直接 412（风控）",
		Category: cat,
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgPageDelay, Type: "int", Default: "150", Min: 0, Max: 5000,
		DisplayName: "翻页间隔（毫秒）",
		Description: "分页工具在两次请求之间的等待。B 站在几十毫秒内连发多个请求时容易吃 -412，" +
			"这个间隔对使用者几乎无感。设为 0 则不等待（不建议）",
		Category: cat,
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgSeenOnFetch, Type: "bool", Default: "true",
		DisplayName: "取回视频信息时登记查重台账",
		Description: "关闭后本地查重记录只在 bilibili_seen_check 命中时才更新，不会因为" +
			"「取过一次信息」就自动登记",
		Category: cat,
	})
}

// reload 把配置读进快照。每次工具调用前调用一次。
func (p *Plugin) reload() {
	c := pluginConfig{
		apiBase:     defaultAPIBase,
		maxItems:    defaultMaxItems,
		maxPages:    defaultMaxPages,
		timeout:     15 * time.Second,
		userAgent:   defaultUserAgent,
		recordOnGet: true,
		pageDelay:   defaultPageDelay,
	}
	var outputDir string
	if p.sdk != nil && p.sdk.Settings() != nil {
		st := p.sdk.Settings()
		outputDir = cfgString(st, cfgOutputDir, "")
		c.apiBase = strings.TrimRight(cfgString(st, cfgAPIBase, defaultAPIBase), "/")
		c.cookie = cfgString(st, cfgCookie, "")
		c.allowPost = cfgBool(st, cfgAllowPost, false)
		c.maxItems = clampInt(cfgInt(st, cfgMaxItems, defaultMaxItems), 1, hardMaxItems)
		c.maxPages = clampInt(cfgInt(st, cfgMaxPages, defaultMaxPages), 1, hardMaxPages)
		c.timeout = time.Duration(clampInt(cfgInt(st, cfgTimeout, 15), 1, 120)) * time.Second
		c.userAgent = cfgString(st, cfgUserAgent, defaultUserAgent)
		c.recordOnGet = cfgBool(st, cfgSeenOnFetch, true)
		c.pageDelay = time.Duration(clampInt(cfgInt(st, cfgPageDelay, int(defaultPageDelay/time.Millisecond)), 0, 5000)) * time.Millisecond
	}
	if outputDir == "" {
		outputDir = filepath.Join(p.base, defaultOutputSubdir)
	}
	c.outputDir = outputDir

	p.mu.Lock()
	p.cfg = c
	p.mu.Unlock()
}

// refresh 在每次工具调用前重读配置。
//
// 为什么不在 Start 里读一次就够：用户改 Cookie / 关掉发评论之后
// 不应该需要重启插件才生效；而 handler 里会多次读配置，中途变化会让
// 一次调用用上两套配置。所以入口处重读一次，之后只用 snapshot。
func (p *Plugin) refresh() {
	if p.sdk == nil || p.sdk.Settings() == nil {
		return
	}
	p.reload()
}

func (p *Plugin) snapshot() pluginConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

func (p *Plugin) dataDir() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.base
}

// validateConfig 在启动时就把坏配置拦住。
//
// 为什么不在每次调用时才报：启动时就报错，用户能在设置页里立刻看到问题；
// 而"调用时才失败"会被模型理解成"工具坏了"，然后反复重试同一个坏配置。
func (p *Plugin) validateConfig() error {
	cfg := p.snapshot()
	dir, err := validateOutputDir(cfg.outputDir)
	if err != nil {
		return fmt.Errorf("配置项 %s 不合法：%w", cfgOutputDir, err)
	}
	cfg.outputDir = dir
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
	return nil
}

// apiClient 返回可用的客户端；配置变化（地址/Cookie/UA）时重建。
func (p *Plugin) apiClient() *client {
	if p.overrideClient != nil {
		return p.overrideClient
	}
	cfg := p.snapshot()

	p.cliMu.Lock()
	defer p.cliMu.Unlock()
	if p.cli != nil && p.cli.base == cfg.apiBase && p.cli.cookie == cfg.cookie && p.cli.ua == cfg.userAgent {
		return p.cli
	}
	c := newClient(cfg.apiBase, cfg.cookie)
	c.ua = cfg.userAgent
	c.http.Timeout = cfg.timeout
	p.cli = c
	return c
}

func (p *Plugin) cleanupData() {
	cfg := p.snapshot()
	if err := p.st.removeAll(); err != nil {
		log.Printf("[%s] onRemove: 清理查重记录失败: %v", p.name, err)
	}
	// 只删**默认那个**导出子目录：用户可能把 output_dir 配成共享目录，
	// 里面还有别人的东西。判据用"恰好是 数据目录/exports"而不是
	// "basename 叫 exports"——后者会误删 /home/u/exports。
	want := filepath.Join(p.dataDir(), defaultOutputSubdir)
	if filepath.Clean(cfg.outputDir) == filepath.Clean(want) {
		if err := removeDirIfExists(cfg.outputDir); err != nil {
			log.Printf("[%s] onRemove: 清理导出目录失败: %v", p.name, err)
		}
	}
}

// ---- 配置读取小工具 ----

func cfgString(st sdk.SettingsAPI, key, def string) string {
	if st == nil {
		return def
	}
	v, err := st.Get(key)
	if err != nil || v == nil {
		return def
	}
	if s, ok := v.(string); ok {
		if strings.TrimSpace(s) == "" {
			return def
		}
		return strings.TrimSpace(s)
	}
	return fmt.Sprint(v)
}

func cfgInt(st sdk.SettingsAPI, key string, def int) int {
	s := cfgString(st, key, "")
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

// cfgBool：内核侧 bool 以字符串（"true"/"false"）存储，这里一并对齐。
func cfgBool(st sdk.SettingsAPI, key string, def bool) bool {
	s := cfgString(st, key, "")
	if s == "" {
		return def
	}
	if b, err := strconv.ParseBool(s); err == nil {
		return b
	}
	return def
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ctxWith 给一次工具调用一个**整体**预算。
//
// 为什么这里只管预算、不管单次请求超时：分页工具会发多次请求，
// 若把这个 context 的 deadline 设成单次请求超时（15s），"整体预算 45s"
// 就永远用不到 —— 第 4 页必然在 15s 处被砍。单次请求由 http.Client.Timeout
// 管（见 apiClient），两者各管一层。
func ctxWith(parent context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if budget <= 0 {
		budget = budgetSingle
	}
	return context.WithTimeout(parent, budget)
}
