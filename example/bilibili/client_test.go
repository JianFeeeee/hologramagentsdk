package main

// 接口层的判据：签名、错误翻译、站外字幕正文。
//
// 全部走 httptest —— 单元测试不打真网络（示例被使用者拷走后
// `go test ./...` 不该依赖 B 站可达、也不该替他们打请求）。

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
)

// 平台真实的 wbi 密钥形态（i0.hdslb.com 上的两张 png 文件名）。
const (
	testImgKey = "7cd084941338484aae1ad9425b84077c"
	testSubKey = "493c877c45418513e56f8c19b4810c91"
)

// ★ 乱序表写错的表现是「所有带签名的接口一律 -403」，而平台不会说哪一步错了。
// 用固定密钥钉住 mixin_key 的具体值。
func TestMixinKeyGolden(t *testing.T) {
	got := mixinKey(testImgKey, testSubKey)
	const want = "131dc124a63c805c4145934a704c4758"
	if got != want {
		t.Fatalf("mixinKey = %q, want %q（乱序表或拼接顺序错了）", got, want)
	}
}

// signParams 必须满足：w_rid = md5(按 key 字典序的 k=urlencode(v) 拼接 + mixin_key)。
//
// 断言方式：拿它自己产出的 wts 复算一遍，逐字比对。这能咬住
// 「先签名后编码」「漏了字典序」「漏了字符过滤」这类真实缺陷。
func TestSignParamsRecomputable(t *testing.T) {
	in := url.Values{}
	in.Set("cid", "12345")
	in.Set("bvid", "BV17x411w7KC")
	in.Set("foo", "a!b'c(d)e*f")

	out := signParams(in, "0123456789abcdef0123456789abcdef")

	if out.Get("wts") == "" {
		t.Fatal("缺少 wts")
	}
	rid := out.Get("w_rid")
	if len(rid) != 32 {
		t.Fatalf("w_rid 长度 %d, want 32（md5 hex）", len(rid))
	}

	// 复算：值必须已过滤 "!'()*"。
	keys := make([]string, 0, len(out))
	for k := range out {
		if k == "w_rid" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		v := strings.NewReplacer("!", "", "'", "", "(", "", ")", "", "*", "").Replace(out.Get(k))
		parts = append(parts, k+"="+url.QueryEscape(v))
	}
	sum := md5.Sum([]byte(strings.Join(parts, "&") + "0123456789abcdef0123456789abcdef"))
	if want := hex.EncodeToString(sum[:]); rid != want {
		t.Fatalf("w_rid = %s, 复算 = %s", rid, want)
	}

	// 过滤后的值必须写回参数表：否则「参与签名的串」与「发出去的串」不一致，
	// 平台一律 -403，而本地看不出任何异常。
	if got := out.Get("foo"); got != "abcdef" {
		t.Fatalf("过滤后的值未写回: %q", got)
	}
}

func TestLastPathStem(t *testing.T) {
	cases := map[string]string{
		"https://i0.hdslb.com/bfs/wbi/7cd084941338484aae1ad9425b84077c.png":     "7cd084941338484aae1ad9425b84077c",
		"https://i0.hdslb.com/bfs/wbi/493c877c45418513e56f8c19b4810c91.png?x=1": "493c877c45418513e56f8c19b4810c91",
		"": "",
	}
	for in, want := range cases {
		if got := lastPathStem(in); got != want {
			t.Errorf("lastPathStem(%q) = %q, want %q", in, got, want)
		}
	}
}

// ★★★ 三种失败形态都必须翻成 error（这是本项目最容易被写错的一条）。
func TestClientErrorsAreLoud(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantSubstr string // 错误里必须出现的内容
		wantRisk   bool
	}{
		{
			name:       "http500",
			status:     http.StatusInternalServerError,
			body:       `{"code":0}`,
			wantSubstr: "HTTP 500",
		},
		{
			name:       "risk412",
			status:     http.StatusPreconditionFailed,
			body:       "blocked",
			wantSubstr: "HTTP 412",
			wantRisk:   true,
		},
		{
			// 业务码非 0 且 HTTP 200：json 能解开，但绝不能被当成成功。
			name:       "biz404",
			status:     http.StatusOK,
			body:       `{"code":-404,"message":"啥都木有"}`,
			wantSubstr: "啥都木有",
		},
		{
			// 风控页是 HTML：错误里要给出可读提示，而不是 "invalid character '<'"。
			name:       "html",
			status:     http.StatusOK,
			body:       "<html><body>risk control</body></html>",
			wantSubstr: "不是 JSON",
			wantRisk:   true, // body 里有 "risk"，按风控处理
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			cl := newClient(srv.URL, "")
			_, err := cl.get(context.Background(), "/x/web-interface/view", url.Values{"bvid": {"BV17x411w7KC"}}, nil)
			if err == nil {
				// 变异测试的靶子：把 decode 改成"失败也返回 nil"时，这里必须红。
				t.Fatalf("HTTP %d / body %q 必须报错，却返回了 nil", c.status, c.body)
			}
			if !strings.Contains(err.Error(), c.wantSubstr) {
				t.Fatalf("错误信息未带关键信息 %q: %v", c.wantSubstr, err)
			}
			var ae *apiError
			if !asAPIError(err, &ae) {
				t.Fatalf("错误不是 *apiError: %T %v", err, err)
			}
			if ae.Status != c.status {
				t.Errorf("apiError.Status = %d, want %d", ae.Status, c.status)
			}
			if ae.Risk != c.wantRisk {
				t.Errorf("apiError.Risk = %v, want %v", ae.Risk, c.wantRisk)
			}
		})
	}
}

// allowCodes 是给 /x/web-interface/nav 用的：未登录返回 -101 但响应体可用。
func TestClientAllowCodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":-101,"message":"账号未登录","data":{"wbi_img":{"img_url":"https://i0.hdslb.com/bfs/wbi/` + testImgKey + `.png","sub_url":"https://i0.hdslb.com/bfs/wbi/` + testSubKey + `.png"}}}`))
	}))
	defer srv.Close()

	cl := newClient(srv.URL, "")
	if _, err := cl.get(context.Background(), "/x/web-interface/nav", nil, nil); err == nil {
		t.Fatal("code=-101 不在 allowCodes 时必须报错")
	}
	raw, err := cl.get(context.Background(), "/x/web-interface/nav", nil, []int{-101})
	if err != nil {
		t.Fatalf("code=-101 在 allowCodes 里时应成功: %v", err)
	}
	if !strings.Contains(string(raw), testImgKey) {
		t.Fatal("响应体被丢弃了")
	}
}

// 字幕正文在 i0.hdslb.com，是**裸 JSON**（没有外层 code）。
// 走带业务码检查的 do() 会把它当失败，所以单独一条路径。
func TestFetchSubtitleBodyBareJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"body":[{"from":0.5,"to":2.0,"content":"第一句"}]}`))
	}))
	defer srv.Close()

	cl := newClient("https://api.example.invalid", "")
	body, err := cl.fetchSubtitleBody(context.Background(), srv.URL+"/sub.json")
	if err != nil {
		t.Fatalf("裸 JSON 字幕正文应解析成功: %v", err)
	}
	if len(body.Body) != 1 || body.Body[0].Content != "第一句" {
		t.Fatalf("解析结果不对: %+v", body)
	}

	// 非 http(s) 的地址必须在本地就被拦住：把它当 URL 去请求只会得到
	// 一句难懂的 scheme 错误。
	if _, err := cl.fetchSubtitleBody(context.Background(), "ftp://x/y.json"); err == nil ||
		!strings.Contains(err.Error(), "不是 http(s)") {
		t.Fatalf("非 http(s) 地址应被本地拒绝: %v", err)
	}
	if _, err := cl.fetchSubtitleBody(context.Background(), "file:///etc/passwd"); err == nil {
		t.Fatal("file:// 地址必须被拒绝（字幕地址来自平台响应，不能当任意 scheme 处理）")
	}
}

// signParams 多值收敛：Encode() 会把多值全发出去，而签名只用 Get(k)，
// 不一致就是必然 -403。
func TestSignParamsDropsExtraValues(t *testing.T) {
	in := url.Values{}
	in.Add("k", "v1")
	in.Add("k", "v2")
	out := signParams(in, "mk")
	if got := out["k"]; len(got) != 1 || got[0] != "v1" {
		t.Fatalf("多值未被收敛到第一个: %v", got)
	}
	if out.Get("w_rid") == "" {
		t.Fatal("缺 w_rid")
	}
}
