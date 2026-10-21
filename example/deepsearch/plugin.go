package main

// 联网检索插件（HologramAgent）
//
// 解决的问题：原来 agent 只有 browser_* 那套「驱动浏览器抓页面」的能力，
// 而 browser_search 是「抓 Bing HTML + 正则解析」——标题取错（拿到来源行）、
// 摘要正则对现代 Bing 完全不命中，于是模型每次只能拿到「标题=URL 串、无摘要」，
// 表现为反复改词重搜。本插件把检索交给本地 SearXNG（多引擎聚合 + 结构化 JSON），
// 并补上「读完前 K 篇再回答」的深检索。
//
// 配置项（全部可在插件配置界面改）：
//   searxng_url          本地/远端 SearXNG 基地址（默认 http://127.0.0.1:8888）
//   manage_searxng       是否由本插件托管搜索后端（默认 true）：插件启动时拉起、停止时关闭
//                        （只关**自己拉起**的实例；发现已在运行则只接管，不认领关闭责任）
//   searxng_dir          托管时使用的 compose 目录（默认 /root/searxng-agent）
//   stop_searxng_on_exit 停止插件时是否关闭后端（默认 true；关掉可避免重载时反复重启）
//   max_results          默认返回条数（控制上下文体积）
//   language             默认语言（如 zh-CN）
//   safesearch           0/1/2
//   request_timeout      单次请求超时（秒）
//   fetch_max_chars      网页正文截断长度
//   proxy                可选 HTTP 代理（抓取被墙站点用，如 http://127.0.0.1:7890）
//
// 依赖的 SearXNG 侧配置（否则 /search?format=json 会回 403，而非网络问题）：
//   search: { formats: [html, json] }   且   server: { limiter: false }

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

const (
	cfgSearxURL      = "searxng_url"
	cfgMaxResults    = "max_results"
	cfgLanguage      = "language"
	cfgSafeSearch    = "safesearch"
	cfgTimeout       = "request_timeout"
	cfgFetchMaxChars = "fetch_max_chars"
	cfgProxy         = "proxy"
	cfgUserAgent     = "user_agent"

	defaultSearxURL = "http://127.0.0.1:8888"

	// maxSearchResults 单次返回给模型的条数上限（保护上下文体积）
	maxSearchResults = 20
	defaultUA        = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"
)

type Plugin struct {
	name string
	sdk  *sdk.PluginSDK
	http *http.Client

	searxURL  string
	maxItems  int
	language  string
	safeLevel int
	fetchMax  int
	userAgent string

	// SearXNG 生命周期托管（见 searxng.go）
	run         cmdRunner
	manageSearx bool
	searxDir    string
	stopOnExit  bool

	searxMu    sync.Mutex
	searxOwned bool // 本插件是否负责关闭它（只在真拉起/接管后为真）

	// 时间预算（零值取包内默认；单测注入短值以避免真等）
	bud searxBudget
}

func NewPlugin(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Stop() error {
	// 双重保险：正常停止走 StopHandler（内核 `plugin.stop` 会先跑 handlers 再调这里），
	// 但写在同一处更稳。shutdownSearxng 幂等，重复调用无害。
	p.shutdownSearxng()
	return nil
}

// ======== 参数与配置读取 ========

func argStr(a map[string]interface{}, key string) string {
	if v, ok := a[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
		return strings.TrimSpace(fmt.Sprint(v))
	}
	return ""
}

func argInt(a map[string]interface{}, key string, def int) int {
	if v, ok := a[key]; ok && v != nil {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case int64:
			return int(n)
		case string:
			if x, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
				return x
			}
		}
	}
	return def
}

func argBool(a map[string]interface{}, key string) bool {
	if v, ok := a[key]; ok && v != nil {
		switch b := v.(type) {
		case bool:
			return b
		case string:
			x, _ := strconv.ParseBool(strings.TrimSpace(b))
			return x
		case float64:
			return b != 0
		}
	}
	return false
}

func cfgStr(st sdk.SettingsAPI, key, def string) string {
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
	s := cfgStr(st, key, "")
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

// cfgBool 注意：core 侧 bool 以字符串（"true"/"false"）存储，这里一并对齐
func cfgBool(st sdk.SettingsAPI, key string, def bool) bool {
	s := cfgStr(st, key, "")
	if s == "" {
		return def
	}
	if b, err := strconv.ParseBool(s); err == nil {
		return b
	}
	return def
}

// ensure 每次调用前重读配置：改地址/代理即时生效
func (p *Plugin) ensure() {
	if p.sdk == nil {
		if p.searxURL == "" {
			p.searxURL = defaultSearxURL
		}
		if p.searxDir == "" {
			p.searxDir = defaultSearxDir
		}
		if p.run == nil {
			p.run = defaultRunner
		}
		if p.maxItems <= 0 {
			p.maxItems = 8
		}
		if p.fetchMax <= 0 {
			p.fetchMax = 4000
		}
		if p.userAgent == "" {
			p.userAgent = defaultUA
		}
		if p.http == nil {
			p.http = &http.Client{Timeout: 20 * time.Second}
		}
		return
	}
	st := p.sdk.Settings()
	p.searxURL = strings.TrimRight(cfgStr(st, cfgSearxURL, defaultSearxURL), "/")
	p.manageSearx = cfgBool(st, cfgManageSearx, true)
	p.stopOnExit = cfgBool(st, cfgStopOnExit, true)
	if d := cfgStr(st, cfgSearxDir, defaultSearxDir); d != "" {
		p.searxDir = d
	} else {
		p.searxDir = defaultSearxDir
	}
	p.run = defaultRunner
	if n := cfgInt(st, cfgMaxResults, 8); n > 0 {
		p.maxItems = n
	} else {
		p.maxItems = 8
	}
	p.language = cfgStr(st, cfgLanguage, "zh-CN")
	p.safeLevel = cfgInt(st, cfgSafeSearch, 0)
	if n := cfgInt(st, cfgFetchMaxChars, 4000); n > 0 {
		p.fetchMax = n
	} else {
		p.fetchMax = 4000
	}
	p.userAgent = cfgStr(st, cfgUserAgent, defaultUA)

	timeout := cfgInt(st, cfgTimeout, 20)
	if timeout <= 0 {
		timeout = 20
	}
	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	if raw := cfgStr(st, cfgProxy, ""); raw != "" {
		if pu, err := url.Parse(raw); err == nil {
			transport.Proxy = http.ProxyURL(pu)
		}
	}
	client.Transport = transport
	p.http = client
}

// ======== SearXNG 交互 ========

type searxResult struct {
	URL           string   `json:"url"`
	Title         string   `json:"title"`
	Content       string   `json:"content"`
	Engine        string   `json:"engine"`
	Engines       []string `json:"engines"`
	Category      string   `json:"category"`
	PublishedDate string   `json:"publishedDate"`
	Score         float64  `json:"score"`
}

type searxResponse struct {
	Query              string            `json:"query"`
	Results            []searxResult     `json:"results"`
	Answers            []string          `json:"answers"`
	Infoboxes          []json.RawMessage `json:"infoboxes"`
	Suggestions        []string          `json:"suggestions"`
	UnresponsiveEngine [][]string        `json:"unresponsive_engines"`
	Timings            map[string]any    `json:"timings"`
}

func (r *searxResult) engineNames() []string {
	set := map[string]bool{}
	for _, e := range r.Engines {
		if e != "" {
			set[e] = true
		}
	}
	if r.Engine != "" {
		set[r.Engine] = true
	}
	out := make([]string, 0, len(set))
	for e := range set {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// searxQuery 调用 SearXNG JSON 接口
func (p *Plugin) searxQuery(params url.Values) (*searxResponse, error) {
	if p.searxURL == "" {
		return nil, fmt.Errorf("未配置 searxng_url")
	}
	params.Set("format", "json")
	endpoint := p.searxURL + "/search?" + params.Encode()

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %v", err)
	}
	req.Header.Set("User-Agent", p.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("SearXNG 不可达（%s）：%v", p.searxURL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	if resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("SearXNG 返回 403：通常是实例未开启 json 输出（settings.yml 的 search.formats 需含 json）或 limiter 拦截了本调用")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("SearXNG 返回 HTTP %d：%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out searxResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析 SearXNG 响应失败（返回体不是 JSON，可能被拦截页替换）: %v", err)
	}
	return &out, nil
}

// dedupResults 按规范化 URL 去重（保留分数更高者），并按分数排序
func dedupResults(in []searxResult) []searxResult {
	norm := func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return raw
		}
		q := u.Query()
		for k := range q {
			if strings.HasPrefix(k, "utm_") || k == "ref" || k == "spm" {
				q.Del(k)
			}
		}
		u.RawQuery = q.Encode()
		u.Fragment = ""
		return strings.TrimRight(u.String(), "/")
	}
	best := map[string]searxResult{}
	order := []string{}
	for _, r := range in {
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		k := norm(r.URL)
		if prev, ok := best[k]; ok {
			if r.Score > prev.Score {
				best[k] = r
			}
			continue
		}
		best[k] = r
		order = append(order, k)
	}
	out := make([]searxResult, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// limitResults 把结果截断到请求条数。
//
// 为什么必须在插件侧截断：SearXNG 的 /search 没有「返回条数」参数 ——
// `?count=3`、`?limit=3` 与不带参数返回的条数一模一样（实测 2026-09-13，35 条）。
// 少了这一步，`count` 与插件配置 `max_results` 全部形同虚设：模型每次吞下 35~58 条
// 带摘要结果（还误以为「要 3 条给了 47 条」，我实测被 agent 当成事实报给用户过）。
func (p *Plugin) limitResults(args map[string]interface{}, in []searxResult) []searxResult {
	n := p.maxItems
	if n <= 0 {
		n = 8
	}
	if v := argInt(args, "count", 0); v > 0 {
		n = v
	}
	if n > maxSearchResults {
		n = maxSearchResults // 保护上下文：不因为模型要 200 条就真给 200 条
	}
	if len(in) > n {
		return in[:n]
	}
	return in
}

// formatResults 返回给模型的紧凑文本（每条：序号/标题/URL/摘要/时间）
// total 是去重后的命中总数，res 是实际返回给模型的那几条（可能已被截断）。
// 两者必须分开说：写成「命中 N 条」而实际只给几条，模型会把它当成"拿到了 N 条"。
func formatResults(q string, res []searxResult, total int, unresponsive [][]string, elapsed time.Duration) string {
	var b strings.Builder
	if total > len(res) {
		fmt.Fprintf(&b, "检索 %q：命中 %d 条，返回前 %d 条（%s）\n", q, total, len(res), elapsed.Round(time.Millisecond))
	} else {
		fmt.Fprintf(&b, "检索 %q：命中 %d 条（%s）\n", q, len(res), elapsed.Round(time.Millisecond))
	}
	engSet := map[string]bool{}
	withSnippet := 0
	for _, r := range res {
		for _, e := range r.engineNames() {
			engSet[e] = true
		}
		if strings.TrimSpace(r.Content) != "" {
			withSnippet++
		}
	}
	engs := make([]string, 0, len(engSet))
	for e := range engSet {
		engs = append(engs, e)
	}
	sort.Strings(engs)
	fmt.Fprintf(&b, "覆盖：%s；带摘要 %d/%d 条\n", strings.Join(engs, " + "), withSnippet, len(res))
	if len(unresponsive) > 0 {
		parts := make([]string, 0, len(unresponsive))
		for _, e := range unresponsive {
			if len(e) >= 2 {
				parts = append(parts, e[0]+"("+e[1]+")")
			}
		}
		if len(parts) > 0 {
			fmt.Fprintf(&b, "本次无结果的引擎：%s\n", strings.Join(parts, ", "))
		}
	}
	b.WriteString("\n")
	for i, r := range res {
		fmt.Fprintf(&b, "%d. %s\n", i+1, strings.TrimSpace(r.Title))
		fmt.Fprintf(&b, "   %s\n", r.URL)
		if d := strings.TrimSpace(r.PublishedDate); d != "" {
			if len(d) > 10 {
				d = d[:10]
			}
			fmt.Fprintf(&b, "   时间：%s\n", d)
		}
		if c := strings.TrimSpace(r.Content); c != "" {
			fmt.Fprintf(&b, "   摘要：%s\n", oneLine(c, 240))
		}
	}
	return strings.TrimSpace(b.String())
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// ======== 网页正文抽取 ========

var (
	reScript    = regexp.MustCompile(`(?is)<script\b.*?</script>`)
	reStyle     = regexp.MustCompile(`(?is)<style\b.*?</style>`)
	reNoscript  = regexp.MustCompile(`(?is)<noscript\b.*?</noscript>`)
	reSvg       = regexp.MustCompile(`(?is)<svg\b.*?</svg>`)
	reComment   = regexp.MustCompile(`(?s)<!--.*?-->`)
	reHead      = regexp.MustCompile(`(?is)<(head|header|footer|nav|aside|form)\b.*?</(head|header|footer|nav|aside|form)>`)
	reTag       = regexp.MustCompile(`(?s)<[^>]+>`)
	reTitleTag  = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reOgTitle   = regexp.MustCompile(`(?is)<meta[^>]+property=["']og:title["'][^>]+content=["']([^"']+)["']`)
	reArticle   = regexp.MustCompile(`(?is)<article\b[^>]*>(.*?)</article>`)
	reMain      = regexp.MustCompile(`(?is)<main\b[^>]*>(.*?)</main>`)
	reBody      = regexp.MustCompile(`(?is)<body\b[^>]*>(.*?)</body>`)
	reBlankLine = regexp.MustCompile(`\n{3,}`)
)

// htmlToText 轻量正文抽取：不去依赖 readibility 库，够 agent 用即可
func htmlToText(raw string, maxChars int) (title, text string) {
	src := raw
	if m := reTitleTag.FindStringSubmatch(src); len(m) > 1 {
		title = strings.TrimSpace(html.UnescapeString(reTag.ReplaceAllString(m[1], " ")))
	}
	if m := reOgTitle.FindStringSubmatch(src); len(m) > 1 && title == "" {
		title = strings.TrimSpace(html.UnescapeString(m[1]))
	}
	body := src
	for _, re := range []*regexp.Regexp{reArticle, reMain} {
		if m := re.FindStringSubmatch(src); len(m) > 1 && len(m[1]) > 200 {
			body = m[1]
			break
		}
	}
	if body == src {
		if m := reBody.FindStringSubmatch(src); len(m) > 1 {
			body = m[1]
		}
	}
	body = reComment.ReplaceAllString(body, " ")
	body = reScript.ReplaceAllString(body, " ")
	body = reStyle.ReplaceAllString(body, " ")
	body = reNoscript.ReplaceAllString(body, " ")
	body = reSvg.ReplaceAllString(body, " ")
	body = reHead.ReplaceAllString(body, " ")
	body = regexp.MustCompile(`(?i)</(p|div|li|tr|h[1-6]|section|article)>`).ReplaceAllString(body, "\n")
	body = regexp.MustCompile(`(?i)<br\s*/?>`).ReplaceAllString(body, "\n")
	body = reTag.ReplaceAllString(body, " ")
	body = html.UnescapeString(body)
	lines := strings.Split(body, "\n")
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.Join(strings.Fields(l), " ")
		if l == "" {
			continue
		}
		kept = append(kept, l)
	}
	text = strings.Join(kept, "\n")
	text = reBlankLine.ReplaceAllString(text, "\n\n")
	if maxChars > 0 {
		r := []rune(text)
		if len(r) > maxChars {
			text = string(r[:maxChars]) + "\n…（已截断）"
		}
	}
	return title, strings.TrimSpace(text)
}

// fetchPage 抓取并转正文
func (p *Plugin) fetchPage(rawURL string, maxChars int) (string, string, error) {
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return "", "", fmt.Errorf("只支持 http/https URL：%s", rawURL)
	}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("构造请求失败: %v", err)
	}
	req.Header.Set("User-Agent", p.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	resp, err := p.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("抓取失败（%s）：%v", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("抓取返回 HTTP %d：%s", resp.StatusCode, rawURL)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "html") && !strings.Contains(ct, "text") && !strings.Contains(ct, "xml") {
		return "", "", fmt.Errorf("不支持的内容类型 %q：%s", ct, rawURL)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", "", fmt.Errorf("读取响应失败: %v", err)
	}
	// 处理常见的中文站点 GBK 页面：只在检测到 charset=gb 时做最小映射
	body := string(raw)
	if strings.Contains(strings.ToLower(ct), "gbk") || strings.Contains(strings.ToLower(ct), "gb2312") {
		body = decodeGBKish(raw)
	}
	title, text := htmlToText(body, maxChars)
	if text == "" {
		return title, "", fmt.Errorf("正文为空（可能是需要 JS 渲染的页面，可改用浏览器工具）: %s", rawURL)
	}
	return title, text, nil
}

// decodeGBKish 极简 GBK→UTF-8 兜底：只处理 ASCII 与常见区间，避免引入额外依赖。
// 拿不准时保留原字节，宁可少转也不要把正文写坏。
func decodeGBKish(raw []byte) string {
	if utf8Valid(raw) {
		return string(raw)
	}
	return strings.ToValidUTF8(string(raw), "\uFFFD")
}

func utf8Valid(b []byte) bool {
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c < 0x80:
			i++
		case c&0xE0 == 0xC0:
			if i+1 >= len(b) {
				return false
			}
			i += 2
		case c&0xF0 == 0xE0:
			if i+2 >= len(b) {
				return false
			}
			i += 3
		case c&0xF8 == 0xF0:
			if i+3 >= len(b) {
				return false
			}
			i += 4
		default:
			return false
		}
	}
	return true
}

// ======== 工具注册 ========

func schemas(props map[string]interface{}, required ...string) map[string]interface{} {
	m := map[string]interface{}{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func pStr(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}

func pInt(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "integer", "description": desc}
}

func pBool(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "boolean", "description": desc}
}

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	p.sdk = s
	s.SetAutoRestart(true)

	st := s.Settings()
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgSearxURL, Type: "string", Default: defaultSearxURL,
		DisplayName: "SearXNG 地址",
		Description: "本地 SearXNG 基地址（.60 上已有专用实例）。该实例必须开启 json 输出，否则请求会 403",
		Category:    "deepsearch",
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgMaxResults, Type: "int", Default: "8", Min: 1, Max: 50,
		DisplayName: "默认返回条数", Description: "控制上下文体积",
		Category: "deepsearch",
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgLanguage, Type: "string", Default: "zh-CN",
		DisplayName: "默认语言", Description: "如 zh-CN / en-US / all",
		Category: "deepsearch",
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgSafeSearch, Type: "int", Default: "0", Min: 0, Max: 2,
		DisplayName: "安全搜索", Description: "0 关 / 1 中 / 2 严",
		Category: "deepsearch",
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgTimeout, Type: "int", Default: "20", Min: 1, Max: 300,
		DisplayName: "请求超时（秒）", Category: "deepsearch",
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgFetchMaxChars, Type: "int", Default: "4000", Min: 500, Max: 50000,
		DisplayName: "正文截断长度", Description: "deepsearch_fetch 返回的正文上限（字符）",
		Category: "deepsearch",
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgProxy, Type: "string", Default: "",
		DisplayName: "抓取代理", Description: "可选，如 http://127.0.0.1:7890；仅影响本插件直连抓取，搜索本身由 SearXNG 侧出网",
		Category: "deepsearch",
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgUserAgent, Type: "string", Default: defaultUA,
		DisplayName: "User-Agent", Category: "deepsearch",
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgManageSearx, Type: "bool", Default: "true",
		DisplayName: "托管 SearXNG",
		Description: "开启后：插件启动时自动拉起搜索后端（docker compose up -d），插件停止时关闭**自己拉起的**实例。发现后端已在运行则只接管、不关闭（否则会带走别的实例的服务）。关掉则假定后端由外部维护（如 systemd）",
		Category:    "deepsearch",
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgSearxDir, Type: "string", Default: defaultSearxDir,
		DisplayName: "SearXNG compose 目录",
		Description: "托管时在该目录执行 docker compose up -d / stop",
		Category:    "deepsearch",
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgStopOnExit, Type: "bool", Default: "true",
		DisplayName: "停止时关闭后端",
		Description: "关闭插件时一并关闭 SearXNG。若插件频繁重载而想避免反复重启后端，可设为 false",
		Category:    "deepsearch",
	})

	p.ensure()

	// 生命周期托管：启动时拉起搜索后端，并把关闭动作注册为 stop handler。
	// 依据内核契约：plugin.stop 会先跑 StopHandlers（LIFO、幂等）再调 Stop()，
	// 宽限期 5 秒 —— 所以 shutdownSearxng 内部限时 4 秒。
	p.ensureSearxng()
	s.RegisterStopHandler(func() { p.shutdownSearxng() })

	p.registerTools()
	log.Printf("[%s] started (searxng=%s manage=%v)", p.name, p.searxURL, p.manageSearx)
	return nil
}

func (p *Plugin) registerTools() {
	tp := p.name + "_"

	p.sdk.RegisterTool(tp+"search", sdk.ToolDef{
		Name: tp + "search",
		Description: "联网检索（首选工具）：经本地 SearXNG 聚合多个搜索引擎，返回带标题、URL、摘要、发布时间的结果。" +
			"需要事实、新闻、文档、报错信息时用它，而不是抓搜索引擎页面",
		Parameters: schemas(map[string]interface{}{
			"query":      pStr("检索词；中文/英文都可"),
			"count":      pInt("返回条数（默认取插件配置 max_results；上限 20）"),
			"engines":    pStr("指定引擎，逗号分隔（如 duckduckgo,brave,quark）；留空用默认聚合"),
			"category":   pStr("类别：general（默认）| news | it | science | images"),
			"time_range": pStr("时间范围：day|week|month|year（新闻类很有用）"),
			"language":   pStr("语言，如 all / zh-CN / en-US；留空用插件配置"),
			"raw":        pBool("true 则返回 SearXNG 原始 JSON（排查用）"),
		}, "query"),
	}, p.handleSearch)

	p.sdk.RegisterTool(tp+"news", sdk.ToolDef{
		Name:        tp + "news",
		Description: "新闻检索：等价于 search 的 news 类别，默认按最近一周过滤，并保留发布时间",
		Parameters: schemas(map[string]interface{}{
			"query":      pStr("检索词"),
			"count":      pInt("条数"),
			"time_range": pStr("day|week|month|year（默认 week）"),
			"language":   pStr("语言，默认插件配置"),
		}, "query"),
	}, p.handleNews)

	p.sdk.RegisterTool(tp+"fetch", sdk.ToolDef{
		Name:        tp + "fetch",
		Description: "抓取单个网页并抽取正文（去脚本/样式/导航），返回标题 + 纯文本，便于精读某条结果",
		Parameters: schemas(map[string]interface{}{
			"url":       pStr("目标 URL（http/https）"),
			"max_chars": pInt("正文上限字符数，默认取插件配置"),
		}, "url"),
	}, p.handleFetch)

	p.sdk.RegisterTool(tp+"deep", sdk.ToolDef{
		Name: tp + "deep",
		Description: "深检索：先检索、再并行抓取前 K 篇正文，一次性返回「候选清单 + 证据正文」。" +
			"适合需要事实核对、多来源交叉的问题——比反复换词重搜有效得多",
		Parameters: schemas(map[string]interface{}{
			"query":      pStr("检索词"),
			"count":      pInt("候选条数（默认 8）"),
			"top_k":      pInt("抓取前几篇正文（默认 3，最多 6）"),
			"max_chars":  pInt("每篇正文上限字符数（默认 2000）"),
			"time_range": pStr("时间范围：day|week|month|year"),
			"language":   pStr("语言"),
		}, "query"),
	}, p.handleDeep)

	p.sdk.RegisterTool(tp+"status", sdk.ToolDef{
		Name:        tp + "status",
		Description: "自检：SearXNG 是否可达、json 输出是否开启、哪些引擎真正在返回结果（检索出问题时第一步）",
		Parameters: schemas(map[string]interface{}{
			"probe": pStr("用于探测的查询词（默认 test）"),
		}),
	}, p.handleStatus)
}

// ======== 处理器 ========

func (p *Plugin) buildParams(args map[string]interface{}, forceCategory string, forceRange string) url.Values {
	v := url.Values{}
	v.Set("q", argStr(args, "query"))
	// 注意：SearXNG 的 /search **不认** count/limit 参数（实测 ?count=3 / ?limit=3 与不带
	// 参数返回完全相同的条数），所以「要几条」由插件自己截断，见 limitResults。
	if e := argStr(args, "engines"); e != "" {
		v.Set("engines", e)
	}
	cat := forceCategory
	if cat == "" {
		cat = argStr(args, "category")
	}
	if cat != "" {
		v.Set("categories", cat)
	}
	if forceRange == "" {
		forceRange = argStr(args, "time_range")
	}
	if forceRange != "" {
		v.Set("time_range", forceRange)
	}
	lang := argStr(args, "language")
	if lang == "" {
		lang = p.language
	}
	if lang != "" {
		v.Set("language", lang)
	}
	v.Set("safesearch", strconv.Itoa(p.safeLevel))
	v.Set("pageno", "1")
	return v
}

func (p *Plugin) handleSearch(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	query := argStr(args, "query")
	if query == "" {
		return nil, fmt.Errorf("缺少参数 query")
	}
	start := time.Now()
	resp, err := p.searxQuery(p.buildParams(args, "", ""))
	if err != nil {
		return nil, err
	}
	if argBool(args, "raw") {
		return resp, nil
	}
	results := dedupResults(resp.Results)
	if len(results) == 0 {
		return map[string]interface{}{"content": p.emptyHint(query, resp)}, nil
	}
	total := len(results)
	results = p.limitResults(args, results)
	txt := formatResults(query, results, total, resp.UnresponsiveEngine, time.Since(start))
	if len(resp.Answers) > 0 {
		txt = "直接答案：" + strings.Join(resp.Answers, "；") + "\n\n" + txt
	}
	return map[string]interface{}{"content": txt}, nil
}

func (p *Plugin) emptyHint(query string, resp *searxResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "检索 %q 未返回结果。", query)
	if len(resp.UnresponsiveEngine) > 0 {
		parts := make([]string, 0, len(resp.UnresponsiveEngine))
		for _, e := range resp.UnresponsiveEngine {
			if len(e) >= 2 {
				parts = append(parts, e[0]+"("+e[1]+")")
			}
		}
		fmt.Fprintf(&b, " 无响应的引擎：%s。", strings.Join(parts, ", "))
	}
	if len(resp.Suggestions) > 0 {
		fmt.Fprintf(&b, " 建议改用：%s。", strings.Join(resp.Suggestions, " / "))
	}
	b.WriteString(" 可以换更短的关键词、去掉过于具体的限定，或改用 deepsearch_news 查新闻。")
	return b.String()
}

func (p *Plugin) handleNews(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	query := argStr(args, "query")
	if query == "" {
		return nil, fmt.Errorf("缺少参数 query")
	}
	if argStr(args, "time_range") == "" {
		args["time_range"] = "week"
	}
	start := time.Now()
	resp, err := p.searxQuery(p.buildParams(args, "news", ""))
	if err != nil {
		return nil, err
	}
	results := dedupResults(resp.Results)
	if len(results) == 0 {
		// 新闻类别颗粒度粗时退回 general + 时间范围
		resp2, err2 := p.searxQuery(p.buildParams(args, "", ""))
		if err2 == nil && len(resp2.Results) > 0 {
			results = dedupResults(resp2.Results)
			resp = resp2
		}
	}
	if len(results) == 0 {
		return map[string]interface{}{"content": p.emptyHint(query, resp)}, nil
	}
	total := len(results)
	results = p.limitResults(args, results)
	return map[string]interface{}{"content": formatResults(query+"（新闻）", results, total, resp.UnresponsiveEngine, time.Since(start))}, nil
}

func (p *Plugin) handleFetch(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	raw := argStr(args, "url")
	if raw == "" {
		return nil, fmt.Errorf("缺少参数 url")
	}
	maxChars := argInt(args, "max_chars", p.fetchMax)
	title, text, err := p.fetchPage(raw, maxChars)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	if title != "" {
		fmt.Fprintf(&b, "标题：%s\n来源：%s\n\n", title, raw)
	} else {
		fmt.Fprintf(&b, "来源：%s\n\n", raw)
	}
	b.WriteString(text)
	return map[string]interface{}{"content": strings.TrimSpace(b.String())}, nil
}

func (p *Plugin) handleDeep(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	query := argStr(args, "query")
	if query == "" {
		return nil, fmt.Errorf("缺少参数 query")
	}
	topK := argInt(args, "top_k", 3)
	if topK < 1 {
		topK = 1
	}
	if topK > 6 {
		topK = 6
	}
	perDoc := argInt(args, "max_chars", 2000)
	if perDoc <= 0 {
		perDoc = 2000
	}
	start := time.Now()
	resp, err := p.searxQuery(p.buildParams(args, "", ""))
	if err != nil {
		return nil, err
	}
	results := dedupResults(resp.Results)
	if len(results) == 0 {
		return map[string]interface{}{"content": p.emptyHint(query, resp)}, nil
	}
	candidates := len(results)
	results = p.limitResults(args, results) // count 只影响候选池，精读条数另有 top_k

	type doc struct {
		idx   int
		title string
		url   string
		text  string
		err   string
	}
	pick := results
	if len(pick) > topK {
		pick = pick[:topK]
	}
	docs := make([]doc, len(pick))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	for i, r := range pick {
		wg.Add(1)
		go func(i int, r searxResult) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			title, text, err := p.fetchPage(r.URL, perDoc)
			d := doc{idx: i, title: r.Title, url: r.URL, text: text}
			if err != nil {
				d.err = err.Error()
			} else if title != "" {
				d.title = title
			}
			docs[i] = d
		}(i, r)
	}
	wg.Wait()

	var b strings.Builder
	fmt.Fprintf(&b, "深检索 %q —— 命中候选 %d 条，取前 %d 条精读（%s）\n\n", query, candidates, len(results), time.Since(start).Round(time.Millisecond))
	b.WriteString("【候选清单】\n")
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, strings.TrimSpace(r.Title), r.URL)
		if c := strings.TrimSpace(r.Content); c != "" {
			fmt.Fprintf(&b, "   摘要：%s\n", oneLine(c, 200))
		}
	}
	b.WriteString("\n【正文证据】\n")
	okCount := 0
	for _, d := range docs {
		fmt.Fprintf(&b, "\n--- [%d] %s\n%s\n", d.idx+1, d.title, d.url)
		if d.err != "" {
			fmt.Fprintf(&b, "（抓取失败，可改用 browser_navigate/browser_render 处理需要 JS 的页面：%s）\n", d.err)
			continue
		}
		okCount++
		b.WriteString(d.text)
		b.WriteString("\n")
	}
	if okCount == 0 {
		b.WriteString("\n（正文全部抓取失败：这批站点可能需要 JS 渲染或被反爬拦截，建议换来源或改用浏览器工具）\n")
	}
	return map[string]interface{}{"content": strings.TrimSpace(b.String())}, nil
}

func (p *Plugin) handleStatus(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	out := map[string]interface{}{
		"searxng_url": p.searxURL,
		"max_results": p.maxItems,
		"language":    p.language,
		"proxy":       cfgStr(p.settings(), cfgProxy, ""),
	}
	// 1) 健康检查
	if req, err := http.NewRequest(http.MethodGet, p.searxURL+"/healthz", nil); err == nil {
		req.Header.Set("User-Agent", p.userAgent)
		if resp, err := p.http.Do(req); err == nil {
			out["healthz"] = resp.StatusCode
			_ = resp.Body.Close()
		} else {
			out["healthz_error"] = err.Error()
		}
	}
	// 2) 实测一次检索：这比只看端口有意义
	probe := argStr(args, "probe")
	if probe == "" {
		probe = "test"
	}
	v := url.Values{}
	v.Set("q", probe)
	v.Set("limit", "10")
	start := time.Now()
	resp, err := p.searxQuery(v)
	if err != nil {
		out["search_error"] = err.Error()
		out["hint"] = "若为 403：检查 SearXNG 的 search.formats 是否含 json、server.limiter 是否为 false"
		return out, nil
	}
	out["search_ok"] = true
	out["latency_ms"] = time.Since(start).Milliseconds()
	out["result_count"] = len(resp.Results)
	engs := map[string]int{}
	for _, r := range resp.Results {
		for _, e := range r.engineNames() {
			engs[e]++
		}
	}
	out["engines_returning_results"] = engs
	if len(resp.UnresponsiveEngine) > 0 {
		out["unresponsive_engines"] = resp.UnresponsiveEngine
	}
	return out, nil
}

func (p *Plugin) settings() sdk.SettingsAPI {
	if p.sdk == nil {
		return nil
	}
	return p.sdk.Settings()
}
