package main

// B 站 Web 接口的最小客户端（仅标准库）。
//
// ## 为什么自己写而不是引第三方 SDK
//
// 示例插件的价值在于「拷走就能改」，一旦引外部依赖，使用者就要跟着维护
// 第三方库的版本；而这里需要的东西很少：GET/POST + JSON + 一个 wbi 签名。
// 全程标准库，干净机器上 `go build` 就能过。
//
// ## 为什么错误处理写得这么啰嗦
//
// B 站接口的失败形态有**三种**，其中两种的 HTTP 状态码还是 200：
//
//	1. HTTP 层失败   404 / 412（风控）/ 429
//	2. 业务层失败    {"code":-404,"message":"啥都木有"}  ← HTTP 200
//	3. 风控/反代页   HTTP 200 但返回一段 HTML
//
// 只判 `err != nil` 是不够的：`json.Unmarshal` 对 `{"code":-404}` 完全
// 成功，解出来是个零值结构体 —— 调用方于是拿到「0 条评论」并当成功返回，
// 使用者被告知「这个视频没有评论」。所以这里把三种形态都翻成 error，
// 并且把**平台原话**（code/message）与 HTTP 状态码一并带回。

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// defaultAPIBase 是可被配置覆盖的站点根（自建反代 / 单测注入 httptest 用）。
	defaultAPIBase = "https://api.bilibili.com"

	// 不带浏览器 UA 时部分接口直接 412（风控）而不是 403。
	defaultUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

	// 单次响应体上限：热门流/评论一页正常几百 KB，8MB 足够，
	// 同时保证被塞进超大数据时不会把插件内存吃光。
	maxBodyBytes = 8 << 20
)

// apiError 是**已经翻成人话**的接口失败。
//
// 单独一个类型是为了让上层区分「平台说不」与「网络不通」：前者重试无意义
// （除了风控类），后者可能值得再试一次。
type apiError struct {
	Endpoint string
	Status   int
	Code     int
	Message  string
	// Risk 表示被风控/限流命中：这类失败要提示用户降频或配 Cookie，
	// 而不是让他们去改参数。
	Risk bool
}

func (e *apiError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "B站接口 %s 失败", e.Endpoint)
	if e.Status != 0 {
		fmt.Fprintf(&b, "（HTTP %d）", e.Status)
	}
	if e.Code != 0 {
		fmt.Fprintf(&b, "（code=%d）", e.Code)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, "：%s", e.Message)
	} else {
		b.WriteString("：平台未给出说明")
	}
	if e.Risk {
		b.WriteString("。这是风控/限流：请降低请求频率，或在配置里填 Cookie（登录态能显著降低风控概率）")
	}
	return b.String()
}

// wbiKeys 是 wbi 签名所需的两段密钥（来自 /x/web-interface/nav）。
type wbiKeys struct {
	imgKey string
	subKey string
}

// client 封装插件生命周期内的 HTTP 行为。
type client struct {
	base    string // 站点根，如 https://api.bilibili.com
	http    *http.Client
	ua      string
	cookie  string // 可选：登录态 Cookie 串（SESSDATA 等）
	referer string

	// wbi 密钥缓存：两个 key 每天轮换，缓存到进程内即可，
	// 签名被拒（-403）时清掉重取。
	wbiMu   sync.Mutex
	wbiKeys *wbiKeys
}

func newClient(base, cookie string) *client {
	if strings.TrimSpace(base) == "" {
		base = defaultAPIBase
	}
	return &client{
		base:    strings.TrimRight(base, "/"),
		http:    &http.Client{Timeout: 20 * time.Second},
		ua:      defaultUserAgent,
		cookie:  strings.TrimSpace(cookie),
		referer: "https://www.bilibili.com/",
	}
}

// get 发一次 GET 并返回**已通过业务码检查**的原始 JSON。
//
// params 里的值会被 URL 编码；allowCodes 里的业务码视为成功（例如
// /x/web-interface/nav 对匿名用户返回 -101，但响应体里仍带着 wbi 密钥）。
func (c *client) get(ctx context.Context, path string, params url.Values, allowCodes []int) ([]byte, error) {
	q := ""
	if len(params) > 0 {
		q = "?" + params.Encode()
	}
	return c.do(ctx, http.MethodGet, path, q, "", allowCodes)
}

// getSigned 发一次带 wbi 签名的 GET。
//
// 签名要求参数先按字典序拼好再算 md5，因此这里**先算签名再编码**：
// 顺序反过来（先 Encode 再签）会得出一个看起来正常、但平台一律 -403 的 w_rid。
func (c *client) getSigned(ctx context.Context, path string, params url.Values, allowCodes []int) ([]byte, error) {
	keys, err := c.ensureWBI(ctx)
	if err != nil {
		return nil, err
	}
	signed := signParams(params, mixinKey(keys.imgKey, keys.subKey))
	body, err := c.get(ctx, path, signed, allowCodes)
	// -403 多半是密钥轮换：清缓存，让下一次调用重取（调用方无需处理）。
	var ae *apiError
	if err != nil && asAPIError(err, &ae) && (ae.Code == -403 || ae.Code == -352) {
		c.invalidateWBI()
	}
	return body, err
}

// getAbsolute 抓一个**站外**绝对 URL（字幕正文在 i0.hdslb.com）。
//
// 不走 do()：那条路径会先探业务码 code，而字幕正文是裸 JSON，没有 code 字段。
func (c *client) getAbsolute(ctx context.Context, full string) ([]byte, error) {
	// 只允许 http(s)：字幕地址来自**平台响应**，把一段任意 scheme 的字符串
	//（file://、gopher://）交给 http.Client 去处理不是我们想要的。
	if !strings.HasPrefix(full, "http://") && !strings.HasPrefix(full, "https://") {
		return nil, fmt.Errorf("拒不请求非 http(s) 地址：%q", full)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败（%s）：%w", full, err)
	}
	c.decorate(req)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("请求 %s 被取消或超时：%w", full, ctx.Err())
		}
		return nil, fmt.Errorf("请求 %s 失败：%w", full, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("读取 %s 的响应失败：%w", full, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &apiError{Endpoint: full, Status: resp.StatusCode, Message: snippet(raw)}
	}
	return raw, nil
}

// postForm 发一次 application/x-www-form-urlencoded POST（评论这类写操作用）。
func (c *client) postForm(ctx context.Context, path string, form url.Values, allowCodes []int) ([]byte, error) {
	return c.do(ctx, http.MethodPost, path, "", form.Encode(), allowCodes)
}

func (c *client) do(ctx context.Context, method, path, query, formBody string, allowCodes []int) ([]byte, error) {
	u := c.base + path + query
	var body io.Reader
	if formBody != "" {
		body = strings.NewReader(formBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败（%s）：%w", path, err)
	}
	if formBody != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	c.decorate(req)

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("请求 %s 被取消或超时：%w", path, ctx.Err())
		}
		return nil, fmt.Errorf("请求 %s 失败：%w", path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("读取 %s 的响应失败：%w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &apiError{
			Endpoint: path,
			Status:   resp.StatusCode,
			Message:  snippet(raw),
			Risk:     resp.StatusCode == http.StatusPreconditionFailed || resp.StatusCode == http.StatusTooManyRequests,
		}
	}
	// 风控页是一段 HTML：json.Unmarshal 会报 "invalid character '<'"，对使用者
	// 毫无意义，这里换成可读提示（并保留前 200 字便于排查）。
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "{") {
		return nil, &apiError{
			Endpoint: path,
			Status:   resp.StatusCode,
			Message:  "响应不是 JSON（可能是风控页或被反代改写）：" + snippet(raw),
			Risk:     strings.Contains(strings.ToLower(trimmed), "risk") || strings.Contains(trimmed, "风控"),
		}
	}

	// 先探业务码：B 站把 code 放在**外层**，数据在外层同级的 data 里。
	var env struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("解析 %s 的响应失败：%w（原文：%s）", path, err, snippet(raw))
	}
	if env.Code != 0 && !containsInt(allowCodes, env.Code) {
		msg := env.Message
		if msg == "" {
			msg = env.Msg
		}
		return nil, &apiError{
			Endpoint: path,
			Status:   resp.StatusCode,
			Code:     env.Code,
			Message:  msg,
			// -412 请求被拦截 / -799 请求过于频繁 / -352 风控校验失败
			Risk: env.Code == -412 || env.Code == -799 || env.Code == -352,
		}
	}
	return raw, nil
}

func (c *client) decorate(req *http.Request) {
	req.Header.Set("User-Agent", c.ua)
	// 评论接口校验 Referer：缺了会返回 -400，排查时容易被带偏成参数问题。
	req.Header.Set("Referer", c.referer)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	if c.cookie != "" {
		req.Header.Set("Cookie", c.cookie)
	}
}

func snippet(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// asAPIError 是 errors.As 的小包装，避免每个调用点都 import errors。
func asAPIError(err error, target **apiError) bool {
	for err != nil {
		if ae, ok := err.(*apiError); ok {
			*target = ae
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// ---- wbi 签名 ----

// wbiMixinTable 是平台固定的乱序表：把 img_key+sub_key 按它重排后取前 32 位
// 得到 mixin_key。表是常量，写错的表现是「所有带签名的接口一律 -403」。
var wbiMixinTable = [64]int{
	46, 47, 18, 2, 53, 8, 23, 32, 15, 50, 10, 31, 58, 3, 45, 35,
	27, 43, 5, 49, 33, 9, 42, 19, 29, 28, 14, 39, 12, 38, 41, 13,
	37, 48, 7, 16, 24, 55, 40, 61, 26, 17, 0, 1, 60, 51, 30, 4,
	22, 25, 54, 21, 56, 59, 6, 63, 57, 62, 11, 36, 20, 34, 44, 52,
}

// mixinKey 按乱序表重排两段 key 并截断到 32 位。
func mixinKey(imgKey, subKey string) string {
	src := imgKey + subKey
	var b strings.Builder
	for _, idx := range wbiMixinTable {
		if idx < len(src) {
			b.WriteByte(src[idx])
		}
	}
	out := b.String()
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}

// signParams 给参数加上 wts 与 w_rid。
//
// 平台规则：按 key 字典序拼成 k=v&…（值先做 URL 编码，并**过滤**
// "!'()*" 这几个字符），末尾拼 mixin_key 求 md5。错任何一步都是 -403，
// 而平台不会说哪一步错了 —— 所以这里把过滤后的值**写回**参数表，
// 保证「参与签名的串」与「真正发出去的串」逐字一致。
func signParams(params url.Values, mk string) url.Values {
	out := url.Values{}
	// 每个键只保留**第一个**值：url.Values.Encode() 会把多值全发出去，
	// 而签名只用了 Get(k)，两者不一致就是必然 -403。本插件的接口都只有
	// 单值参数，这里显式收敛，免得将来有人加了个多值参数却查不出原因。
	for k, v := range params {
		if len(v) == 0 {
			continue
		}
		out.Set(k, sanitizeWBIValue(v[0]))
	}
	out.Set("wts", strconv.FormatInt(time.Now().Unix(), 10))

	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+url.QueryEscape(out.Get(k)))
	}
	sum := md5.Sum([]byte(strings.Join(parts, "&") + mk))
	out.Set("w_rid", hex.EncodeToString(sum[:]))
	return out
}

func sanitizeWBIValue(v string) string {
	return strings.NewReplacer("!", "", "'", "", "(", "", ")", "", "*", "").Replace(v)
}

// ensureWBI 取（并缓存）wbi 密钥。
//
// 未登录时 /x/web-interface/nav 返回 code=-101，但**响应体里仍带 wbi_img**，
// 所以匿名也能签名 —— 这一点很容易被误判成「必须先登录」。
func (c *client) ensureWBI(ctx context.Context) (*wbiKeys, error) {
	c.wbiMu.Lock()
	cached := c.wbiKeys
	c.wbiMu.Unlock()
	if cached != nil {
		return cached, nil
	}

	raw, err := c.get(ctx, "/x/web-interface/nav", nil, []int{-101})
	if err != nil {
		return nil, fmt.Errorf("获取 wbi 签名密钥失败：%w", err)
	}
	var nav struct {
		Data struct {
			WbiImg struct {
				ImgURL string `json:"img_url"`
				SubURL string `json:"sub_url"`
			} `json:"wbi_img"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &nav); err != nil {
		return nil, fmt.Errorf("解析 wbi 密钥失败：%w（原文：%s）", err, snippet(raw))
	}

	img := lastPathStem(nav.Data.WbiImg.ImgURL)
	sub := lastPathStem(nav.Data.WbiImg.SubURL)
	if img == "" || sub == "" {
		return nil, fmt.Errorf("获取 wbi 签名密钥失败：接口未返回 wbi_img（img_url=%q sub_url=%q）",
			nav.Data.WbiImg.ImgURL, nav.Data.WbiImg.SubURL)
	}
	k := &wbiKeys{imgKey: img, subKey: sub}
	c.wbiMu.Lock()
	c.wbiKeys = k
	c.wbiMu.Unlock()
	return k, nil
}

// invalidateWBI 清掉缓存，让下一次调用重新取密钥。
func (c *client) invalidateWBI() {
	c.wbiMu.Lock()
	c.wbiKeys = nil
	c.wbiMu.Unlock()
}

// lastPathStem 从
// "https://i0.hdslb.com/bfs/wbi/7cd084941338484aae1ad9425b84077c.png"
// 取出 "7cd084941338484aae1ad9425b84077c"。
func lastPathStem(raw string) string {
	if raw == "" {
		return ""
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.LastIndex(raw, "/"); i >= 0 {
		raw = raw[i+1:]
	}
	if i := strings.LastIndex(raw, "."); i >= 0 {
		raw = raw[:i]
	}
	return raw
}
