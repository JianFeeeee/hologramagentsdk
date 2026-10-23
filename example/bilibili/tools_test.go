package main

// handler 层的判据：落盘、查重命中、**接口失败不静默**、写操作默认关闭。
//
// 全部用 httptest 冒充平台。这里刻意不断言"返回值是否非 nil"这种弱判据 ——
// 每条失败路径都断言**错误信息里必须出现的东西**（状态码、平台原话、
// 缺哪个配置项），这样才能在有人把错误吞掉时真的红。

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// TestMain 把翻页节流关掉：节流是给线上礼貌用的，测试不该为它白等。
func TestMain(m *testing.M) {
	defaultPageDelay = 0
	os.Exit(m.Run())
}

// ---- 假 SettingsAPI ----
//
// 只实现本插件真正用到的那几个方法，其余返回零值：这样 fake 不会因为
// SDK 新增接口就失效，也逼着实现只依赖必需的能力。

type fakeSettings struct {
	values map[string]interface{}
	defs   []sdk.ConfigDef
	dir    string
}

func newFakeSettings(dir string, kv map[string]interface{}) *fakeSettings {
	v := map[string]interface{}{}
	for k, val := range kv {
		v[k] = val
	}
	return &fakeSettings{values: v, dir: dir}
}

func (f *fakeSettings) Get(key string) (interface{}, error) {
	v, ok := f.values[key]
	if !ok {
		return nil, fmt.Errorf("no such key %q", key)
	}
	return v, nil
}
func (f *fakeSettings) Set(key string, value interface{}) error { f.values[key] = value; return nil }
func (f *fakeSettings) SetSecret(key string, value interface{}) error {
	f.values[key] = value
	return nil
}
func (f *fakeSettings) GetSecret(key string) (string, error) {
	if s, ok := f.values[key].(string); ok {
		return s, nil
	}
	return "", nil
}
func (f *fakeSettings) List(prefix string) ([]string, error) {
	var out []string
	for k := range f.values {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out, nil
}
func (f *fakeSettings) GetCore(string) (interface{}, error)           { return nil, nil }
func (f *fakeSettings) SetCore(string, interface{}) error             { return nil }
func (f *fakeSettings) ListCore(string) ([]string, error)             { return nil, nil }
func (f *fakeSettings) DataDir() string                               { return f.dir }
func (f *fakeSettings) GetPlugin(string, string) (interface{}, error) { return nil, nil }
func (f *fakeSettings) SetPlugin(string, string, interface{}) error   { return nil }
func (f *fakeSettings) ListPlugin(string, string) ([]string, error)   { return nil, nil }
func (f *fakeSettings) RegisterDef(def sdk.ConfigDef)                 { f.defs = append(f.defs, def) }
func (f *fakeSettings) Defs(string) []*sdk.ConfigDef                  { return nil }
func (f *fakeSettings) Dump() map[string]interface{}                  { return f.values }
func (f *fakeSettings) Plugins() []string                             { return nil }

// toolReg 记录注册进来的 ToolDef，便于断言"声明"本身。
type toolReg struct {
	defs []sdk.ToolDef
}

func (r *toolReg) register(name string, def sdk.ToolDef, _ sdk.ToolHandler) error {
	if name != def.Name {
		// 注册名与 Name 字段不一致会让内核按名调用时找不到工具。
		panic("注册名与 ToolDef.Name 不一致: " + name + " vs " + def.Name)
	}
	r.defs = append(r.defs, def)
	return nil
}

// newTestPlugin 起一个已 Start 的插件，HTTP 指向 srv。
func newTestPlugin(t *testing.T, kv map[string]interface{}, h http.Handler) (*Plugin, *httptest.Server, *sdk.PluginSDK) {
	t.Helper()
	p, srv, s, _ := newTestPluginWithReg(t, kv, h)
	return p, srv, s
}

func newTestPluginWithReg(t *testing.T, kv map[string]interface{}, h http.Handler) (*Plugin, *httptest.Server, *sdk.PluginSDK, *toolReg) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	dataDir := t.TempDir()
	sett := newFakeSettings(dataDir, kv)
	reg := &toolReg{}
	s := sdk.New("bilibili", sett, reg.register, nil, nil, nil)
	p := &Plugin{name: "bilibili"}
	if err := p.Start(s); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	// 注入测试客户端：覆盖配置里的 api_base，避免任何真实网络。
	cl := newClient(srv.URL, "")
	cl.http = srv.Client()
	p.overrideClient = cl
	return p, srv, s, reg
}

const viewJSON = `{
  "code": 0,
  "data": {
    "bvid": "BV17x411w7KC",
    "aid": 170001,
    "title": "测试视频标题",
    "desc": "简介内容",
    "pubdate": 1600000000,
    "duration": 300,
    "owner": {"mid": 12345, "name": "测试UP"},
    "stat": {"view": 1000, "danmaku": 10, "reply": 5, "favorite": 3, "coin": 4, "share": 2, "like": 100},
    "pages": [
      {"cid": 111, "page": 1, "part": "P1 开场", "duration": 150},
      {"cid": 222, "page": 2, "part": "P2 正片", "duration": 150}
    ]
  }
}`

// ---- 视频信息 + 自动登记查重台账 ----

func TestHandleVideoReturnsInfoAndRecords(t *testing.T) {
	p, _, _ := newTestPlugin(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/x/web-interface/view" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("bvid"); got != "BV17x411w7KC" {
			t.Errorf("请求里的 bvid = %q", got)
		}
		_, _ = w.Write([]byte(viewJSON))
	}))

	res, err := p.handleVideo(map[string]interface{}{"video": "https://www.bilibili.com/video/BV17x411w7KC?p=2&t=10"})
	if err != nil {
		t.Fatalf("handleVideo 报错: %v", err)
	}
	m := res.(map[string]interface{})
	content := m["content"].(string)
	for _, want := range []string{"测试视频标题", "测试UP", "cid=111", "cid=222", "未见过"} {
		if !strings.Contains(content, want) {
			t.Errorf("输出里缺 %q:\n%s", want, content)
		}
	}
	if m["aid"].(int64) != 170001 {
		t.Errorf("aid = %v", m["aid"])
	}

	// 取回信息后必须自动登记 —— 否则本地查重永远命中不了。
	hit, err := p.handleSeenCheck(map[string]interface{}{"video": "av170001"})
	if err != nil {
		t.Fatalf("seen_check 报错: %v", err)
	}
	if ok, _ := hit.(map[string]interface{})["hit"].(bool); !ok {
		t.Fatalf("取回后查重应命中: %+v", hit)
	}
}

func TestHandleSeenCheckMissIsExplicit(t *testing.T) {
	p, _, _ := newTestPlugin(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("seen_check 不应发网络请求，却请求了 %s", r.URL.Path)
	}))
	res, err := p.handleSeenCheck(map[string]interface{}{"video": "BV1xx411c7mD"})
	if err != nil {
		t.Fatalf("未命中不算错误: %v", err)
	}
	m := res.(map[string]interface{})
	if hit, _ := m["hit"].(bool); hit {
		t.Fatal("未命中却报命中")
	}
	if !strings.Contains(m["content"].(string), "未命中") {
		t.Fatalf("未命中必须明确说出来: %v", m["content"])
	}
}

func TestHandleVideoInvalidInput(t *testing.T) {
	p, _, _ := newTestPlugin(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("非法输入不应发请求，却请求了 %s", r.URL.Path)
	}))
	if _, err := p.handleVideo(map[string]interface{}{"video": "https://b23.tv/xyz"}); err == nil {
		t.Fatal("短链应被拒绝")
	}
	if _, err := p.handleVideo(map[string]interface{}{}); err == nil {
		t.Fatal("缺少 video 参数应报错")
	}
}

// ---- ★ 接口错误不静默 ----

// ★ HTTP 500 的响应体刻意做成「看起来成功」的形状（code=0 + 空 replies）。
//
// 为什么必须这样：如果 500 的 body 里 code 非 0，业务码检查也会报错，
// 于是**删掉状态码检查的变异照样能过**（实测过）。而现实里 500 更常见的
// 来源是网关/反代，响应体根本不是平台的业务 JSON。所以这条用例要单独
// 咬住「HTTP 状态码」这一层。
func TestComments500IsErrorNotEmptyList(t *testing.T) {
	p, _, _ := newTestPlugin(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":0,"data":{"page":{"count":0,"size":20,"pn":1},"replies":[]}}`))
	}))

	res, err := p.handleComments(map[string]interface{}{"video": "BV17x411w7KC"})
	if err == nil {
		t.Fatalf("HTTP 500 必须报错，却返回了「成功」结果（这正是要禁止的静默空列表）: %+v", res)
	}
	// ★ 断言具体内容：只判 err != nil 挡不住"错误信息被换成空串"这类变异。
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("错误信息必须带状态码 500: %v", err)
	}
}

// 500 + 业务错误体也要报错，且要带回平台原话。
func TestComments500WithBizBodyIsError(t *testing.T) {
	p, _, _ := newTestPlugin(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":-500,"message":"服务器内部错误"}`))
	}))
	_, err := p.handleComments(map[string]interface{}{"video": "BV17x411w7KC"})
	if err == nil {
		t.Fatal("HTTP 500 必须报错")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("错误信息必须带状态码 500: %v", err)
	}
}

func TestCommentsBizErrorIsError(t *testing.T) {
	p, _, _ := newTestPlugin(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// HTTP 200 + 业务码失败：这是最容易漏掉的形态。
		_, _ = w.Write([]byte(`{"code":-404,"message":"啥都木有"}`))
	}))
	_, err := p.handleComments(map[string]interface{}{"video": "BV17x411w7KC"})
	if err == nil {
		t.Fatal("业务码 -404（HTTP 200）必须报错")
	}
	if !strings.Contains(err.Error(), "啥都木有") {
		t.Fatalf("错误信息必须带回平台原话: %v", err)
	}
}

// 热门流同样要咬住状态码这一层（body 做成看起来成功的形状）。
func TestPopular500IsError(t *testing.T) {
	p, _, _ := newTestPlugin(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"code":0,"data":{"list":[]}}`))
	}))
	res, err := p.handlePopular(map[string]interface{}{})
	if err == nil {
		t.Fatalf("HTTP 502 必须报错，却返回了: %+v", res)
	}
	if !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("错误信息必须带状态码 502: %v", err)
	}
}

func TestPopularEmptyListIsError(t *testing.T) {
	p, _, _ := newTestPlugin(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// code=0 但 data.list 为空：多数是风控/反代改写，不能当"今天没热门"。
		_, _ = w.Write([]byte(`{"code":0,"data":{"list":[]}}`))
	}))
	res, err := p.handlePopular(map[string]interface{}{})
	if err == nil {
		t.Fatalf("热门为空必须报错，却返回了: %+v", res)
	}
	if !strings.Contains(err.Error(), "热门列表为空") {
		t.Fatalf("错误信息未说明原因: %v", err)
	}
}

func TestCommentsRespectPageCap(t *testing.T) {
	var hits int32
	p, _, _ := newTestPlugin(t, map[string]interface{}{"max_pages": "2", "max_items": "3"},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			pn := r.URL.Query().Get("pn")
			_, _ = w.Write([]byte(`{"code":0,"data":{"page":{"count":100,"size":3,"pn":` + pn + `},
				"replies":[{"rpid":1,"member":{"uname":"u"},"content":{"message":"m"},"ctime":1600000000}]}}`))
		}))

	// 参数里传一个荒唐的页数：必须被硬上限压住。
	if _, err := p.handleComments(map[string]interface{}{"video": "BV17x411w7KC", "pages": 999}); err != nil {
		t.Fatalf("报错: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != hardMaxPages {
		t.Fatalf("请求了 %d 页, want %d（硬上限没生效就会撞风控）", n, hardMaxPages)
	}

	// 配置 max_pages=2 时，不传 pages 只翻 2 页。
	atomic.StoreInt32(&hits, 0)
	if _, err := p.handleComments(map[string]interface{}{"video": "BV17x411w7KC"}); err != nil {
		t.Fatalf("报错: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("请求了 %d 页, want 2（配置的 max_pages 未生效）", n)
	}
}

// ---- 字幕：wbi 签名链路 + 导出文件 ----

func TestHandleSubtitlesExportsFileImpl(t *testing.T) {
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/x/web-interface/nav", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":-101,"message":"账号未登录","data":{"wbi_img":{
			"img_url":"https://i0.hdslb.com/bfs/wbi/` + testImgKey + `.png",
			"sub_url":"https://i0.hdslb.com/bfs/wbi/` + testSubKey + `.png"}}}`))
	})
	mux.HandleFunc("/x/player/wbi/v2", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("w_rid") == "" {
			t.Error("字幕接口必须带 w_rid 签名（缺了平台一律 -403）")
		}
		if r.URL.Query().Get("cid") != "111" {
			t.Errorf("cid = %q, want 111", r.URL.Query().Get("cid"))
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"subtitle":{"subtitles":[
			{"lan":"zh-CN","lan_doc":"中文（中国）","subtitle_url":"` + base + `/sub.json","is_ai":false}]}}}`))
	})
	mux.HandleFunc("/sub.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"body":[{"from":1.5,"to":3,"content":"第一句"},{"from":3,"to":5,"content":"第二句"}]}`))
	})

	p, srv, _ := newTestPlugin(t, nil, mux)
	base = srv.URL

	res, err := p.handleSubtitles(map[string]interface{}{
		"video": "BV17x411w7KC", "cid": 111, "lang": "zh-CN",
	})
	if err != nil {
		t.Fatalf("handleSubtitles 报错: %v", err)
	}
	m := res.(map[string]interface{})
	path, _ := m["file"].(string)
	if path == "" {
		t.Fatalf("未返回导出文件路径: %+v", m)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("导出文件读不到: %v", err)
	}
	for _, want := range []string{"BV17x411w7KC", "第一句", "第二句", `"lan": "zh-CN"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("导出文件缺 %q:\n%s", want, raw)
		}
	}
	// 文件必须落在输出目录（数据目录下的 exports/），不能乱丢。
	if !strings.HasPrefix(path, filepath.Join(p.dataDir(), defaultOutputSubdir)) {
		t.Errorf("导出路径 %q 不在输出目录下", path)
	}
}

func TestHandleSubtitlesEmptyTracksExplainsWhy(t *testing.T) {
	p, _, _ := newTestPlugin(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/x/web-interface/nav":
			_, _ = w.Write([]byte(`{"code":-101,"data":{"wbi_img":{
				"img_url":"https://i0.hdslb.com/bfs/wbi/` + testImgKey + `.png",
				"sub_url":"https://i0.hdslb.com/bfs/wbi/` + testSubKey + `.png"}}}`))
		case "/x/web-interface/view":
			_, _ = w.Write([]byte(viewJSON))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{"subtitle":{"subtitles":[]}}}`))
		}
	}))
	res, err := p.handleSubtitles(map[string]interface{}{"video": "BV17x411w7KC"})
	if err != nil {
		t.Fatalf("空字幕轨不是接口错误: %v", err)
	}
	content := res.(map[string]interface{})["content"].(string)
	if !strings.Contains(content, "匿名") {
		t.Fatalf("匿名环境下空字幕清单必须解释原因（否则像是插件漏了功能）: %s", content)
	}
}

// ---- ★ 写操作默认关闭 ----

func TestCommentPostDisabledByDefault(t *testing.T) {
	var hits int32
	p, _, _ := newTestPlugin(t, map[string]interface{}{
		// 关键：Cookie 齐全（含 bili_jct），只差开关 ——
		// 这样能证明"默认关闭"是开关本身在起作用，而不是碰巧缺 csrf。
		"cookie": "SESSDATA=abc; bili_jct=deadbeef",
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"code":0,"data":{"rpid":1}}`))
	}))

	res, err := p.handleCommentPost(map[string]interface{}{
		"video": "BV17x411w7KC", "message": "测试评论",
	})
	if err == nil {
		t.Fatalf("发评论默认必须关闭，却成功了: %+v", res)
	}
	if !strings.Contains(err.Error(), cfgAllowPost) {
		t.Fatalf("错误里必须点名是哪个开关: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("开关关闭时不该发出任何请求，实际发了 %d 个", n)
	}
}

func TestCommentPostNeedsCSRF(t *testing.T) {
	var hits int32
	p, _, _ := newTestPlugin(t, map[string]interface{}{
		"allow_comment_post": "true",
		"cookie":             "SESSDATA=abc", // 没有 bili_jct
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"code":0,"data":{"rpid":1}}`))
	}))
	_, err := p.handleCommentPost(map[string]interface{}{"video": "BV17x411w7KC", "message": "x"})
	if err == nil {
		t.Fatal("缺 bili_jct 时必须本地拒绝")
	}
	if !strings.Contains(err.Error(), "bili_jct") {
		t.Fatalf("错误里必须说清缺 bili_jct: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("本地就该拦下，不该发请求（发了 %d 个）", n)
	}
}

func TestCommentPostWhenEnabled(t *testing.T) {
	var gotForm map[string]string
	p, _, _ := newTestPlugin(t, map[string]interface{}{
		"allow_comment_post": "true",
		"cookie":             "SESSDATA=abc; bili_jct=deadbeef",
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("解析表单失败: %v", err)
		}
		gotForm = map[string]string{
			"type": r.PostForm.Get("type"), "oid": r.PostForm.Get("oid"),
			"message": r.PostForm.Get("message"), "csrf": r.PostForm.Get("csrf"),
			"root": r.PostForm.Get("root"), "parent": r.PostForm.Get("parent"),
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"rpid":98765}}`))
	}))

	res, err := p.handleCommentPost(map[string]interface{}{
		"video": "BV17x411w7KC", "message": "认真评论", "root": 555,
	})
	if err != nil {
		t.Fatalf("开启后应能发: %v", err)
	}
	m := res.(map[string]interface{})
	if m["rpid"].(int64) != 98765 {
		t.Fatalf("rpid = %v", m["rpid"])
	}
	want := map[string]string{
		"type": "1", "oid": "170001", "message": "认真评论", "csrf": "deadbeef",
		// 回复时 parent 缺省取 root（平台的楼中楼约定）。
		"root": "555", "parent": "555",
	}
	for k, v := range want {
		if gotForm[k] != v {
			t.Errorf("表单 %s = %q, want %q", k, gotForm[k], v)
		}
	}
}

func TestCommentPostRejectsEmptyAndLong(t *testing.T) {
	p, _, _ := newTestPlugin(t, map[string]interface{}{
		"allow_comment_post": "true",
		"cookie":             "bili_jct=deadbeef",
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("非法输入不该发请求: %s", r.URL.Path)
	}))
	if _, err := p.handleCommentPost(map[string]interface{}{"video": "BV17x411w7KC", "message": "  "}); err == nil {
		t.Fatal("空评论应被拒")
	}
	long := strings.Repeat("字", maxCommentRunes+1)
	if _, err := p.handleCommentPost(map[string]interface{}{"video": "BV17x411w7KC", "message": long}); err == nil {
		t.Fatal("超长评论应被拒")
	}
}

// ---- 配置安全 ----

func TestStartRejectsSystemOutputDir(t *testing.T) {
	sett := newFakeSettings(t.TempDir(), map[string]interface{}{"output_dir": "/etc"})
	s := sdk.New("bilibili", sett, nil, nil, nil, nil)
	p := &Plugin{name: "bilibili"}
	if err := p.Start(s); err == nil {
		t.Fatal("output_dir=/etc 必须让插件启动失败")
	}
}

func TestConfigDefsRegistered(t *testing.T) {
	_, _, s := newTestPlugin(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	st := s.Settings().(*fakeSettings)
	want := []string{cfgOutputDir, cfgAPIBase, cfgCookie, cfgAllowPost, cfgMaxItems, cfgMaxPages, cfgTimeout, cfgUserAgent, cfgSeenOnFetch}
	have := map[string]bool{}
	for _, d := range st.defs {
		have[d.Key] = true
	}
	for _, k := range want {
		if !have[k] {
			t.Errorf("配置项 %q 未注册（WebUI 里就不会出现）", k)
		}
	}
	// 危险开关的默认值必须是 false —— 示例不该有"一键刷评论"的默认值。
	for _, d := range st.defs {
		if d.Key == cfgAllowPost && d.Default != "false" {
			t.Fatalf("%s 的默认值是 %v，必须是 \"false\"", cfgAllowPost, d.Default)
		}
	}
}

// ★ 声明类字段的判据：写操作不并发、网络工具不声明 ParallelSafe、
// 大块内容给 NoMemory、必填参数进 required。
//
// 这些声明是**责任**而不是装饰：误声明 ParallelSafe 会让同批多个请求
// 同时打同一个平台（风控最容易命中的形态）；漏声明 required 会让模型
// 生成缺参数调用，再到 handler 里才报错。
func TestToolDeclarations(t *testing.T) {
	_, _, _, reg := newTestPluginWithReg(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	byName := map[string]sdk.ToolDef{}
	for _, d := range reg.defs {
		byName[d.Name] = d
	}
	for _, want := range []string{
		"bilibili_video", "bilibili_subtitles", "bilibili_comments", "bilibili_popular",
		"bilibili_seen_check", "bilibili_seen_list", "bilibili_seen_forget", "bilibili_comment_post",
	} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("工具 %q 未注册", want)
		}
	}

	// 只有纯本地只读工具可以并发。
	for name, d := range byName {
		if !d.ParallelSafe {
			continue
		}
		if name != "bilibili_seen_check" && name != "bilibili_seen_list" {
			t.Errorf("%s 声明了 ParallelSafe，但它会发网络请求/写盘（同批并发会同时打平台）", name)
		}
	}

	// 写操作必须 Serial，且绝不能是 ParallelSafe。
	post := byName["bilibili_comment_post"]
	if post.ParallelSafe {
		t.Error("comment_post 绝不能被并发调度")
	}
	if !post.Serial {
		t.Error("comment_post 必须声明 Serial（楼层顺序是可感知的）")
	}

	// 大块外部内容给 NoMemory。
	for _, name := range []string{"bilibili_comments", "bilibili_subtitles", "bilibili_popular"} {
		if !byName[name].NoMemory {
			t.Errorf("%s 应声明 NoMemory（大块外部内容进记忆只会挤占上下文）", name)
		}
	}

	// required 必须来自显式声明，且排序稳定。
	for name, want := range map[string][]string{
		"bilibili_video":        {"video"},
		"bilibili_comments":     {"video"},
		"bilibili_seen_check":   {"video"},
		"bilibili_comment_post": {"message", "video"},
	} {
		params := byName[name].Parameters
		reqAny, ok := params["required"]
		if !ok {
			t.Errorf("%s 缺少 required", name)
			continue
		}
		req := reqAny.([]string)
		if strings.Join(req, ",") != strings.Join(want, ",") {
			t.Errorf("%s 的 required = %v, want %v", name, req, want)
		}
	}

	// 参数里不该残留我们的内部标记（初版靠 __required 传状态，容易泄漏进 schema）。
	for name, d := range byName {
		props, _ := d.Parameters["properties"].(map[string]interface{})
		for k, v := range props {
			if m, ok := v.(map[string]interface{}); ok {
				if _, leaked := m["__required"]; leaked {
					t.Errorf("%s 的参数 %s 里残留了内部标记 __required", name, k)
				}
			}
		}
	}
}

// 卸载时只该删**默认的**导出子目录，不能顺手删掉用户配的共享目录。
func TestOnRemoveOnlyDeletesDefaultExports(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(shared, "别人的文件.txt")
	if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	p, _, _ := newTestPlugin(t, map[string]interface{}{"output_dir": shared},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	p.cleanupData()

	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("用户配的目录被删了: %v", err)
	}

	// 默认目录则应该被清掉。
	p2, _, _ := newTestPlugin(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	def := filepath.Join(p2.dataDir(), defaultOutputSubdir)
	if err := os.MkdirAll(def, 0o755); err != nil {
		t.Fatal(err)
	}
	p2.cleanupData()
	if _, err := os.Stat(def); !os.IsNotExist(err) {
		t.Fatalf("默认导出目录应被清理: %v", err)
	}
}

// ---- ★ 评论输出 header 的精确回归 ----
//
// ## 曾存在的缺陷（实装验收时打真 API 发现）
//
// tools.go 的评论 header 写成 `"视频 %s（av%d）评论共 …"` + `ref, ref.AID`，
// 而 videoRef.String()（bvid.go）本身已经返回 "BV1GJ411x7h7 (av80433022)"，
// 于是 av 号被拼了两次：
//
//	视频 BV1GJ411x7h7 (av80433022)（av80433022）评论共 238372 条，按热度排序：
//
// ## 为什么它能活到实装才被发现
//
// 43 条测试没有一条断言过评论输出的**正文 header** —— 所有失败路径用例都在
// 断言 err 与错误信息，成功路径只看 total/returned/pages_fetched 这些结构化
// 字段。⇒ 正文渲染是测试盲区。
//
// ## 为什么断言用「精确匹配 + 计数」而不是 strings.Contains
//
// `Contains(content, "评论共")` 对重复拼接照样绿（这正是不该用的写法）。
// 精确比对整行 + 计数 av 出现次数，才能咬住"多拼一次"。
func TestCommentsHeaderRendersVideoRefOnce(t *testing.T) {
	// 真实世界用例（用户实装验收时打的视频）：BV1GJ411x7h7 / aid 80433022
	const wantBVID = "BV1GJ411x7h7"
	const wantAID = 80433022

	p, _, _ := newTestPlugin(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		total := r.URL.Query().Get("ps")
		_ = total
		_, _ = w.Write([]byte(`{"code":0,"data":{
			"page":{"count":238372,"size":20,"pn":1},
			"replies":[{"rpid":1,"mid":9,"ctime":1600000000,"like":3,"rcount":0,
				"member":{"uname":"某人","mid":"9"},"content":{"message":"第一条"}}]}}`))
	}))

	res, err := p.handleComments(map[string]interface{}{"video": wantBVID})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	content := res.(map[string]interface{})["content"].(string)
	firstLine := strings.SplitN(content, "\n", 2)[0]

	// ① 整行精确匹配：把格式钉死（任何多余/缺失字符都会红）。
	wantHeader := "视频 BV1GJ411x7h7 (av80433022) 评论共 238372 条，按热度排序："
	if firstLine != wantHeader {
		t.Fatalf("评论 header 不精确:\n  got  = %q\n  want = %q", firstLine, wantHeader)
	}

	// ② 计数断言：av 号只能出现一次（重复拼接的缺陷在这里必红）。
	if n := strings.Count(firstLine, "av80433022"); n != 1 {
		t.Fatalf("header 里 av80433022 出现 %d 次, want 1（重复渲染缺陷）: %q", n, firstLine)
	}
	if n := strings.Count(content, "av80433022"); n != 1 {
		t.Fatalf("正文里 av80433022 出现 %d 次, want 1: %q", n, content)
	}
}

// 410 行的「暂无评论」分支同样必须只渲染一次（这条分支真实调用时也会走到）。
func TestCommentsEmptyHeaderRendersVideoRefOnce(t *testing.T) {
	p, _, _ := newTestPlugin(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// page.count=0 且 replies 为空 ⇒ 走「暂无评论」那条分支。
		_, _ = w.Write([]byte(`{"code":0,"data":{"page":{"count":0,"size":20,"pn":1},"replies":[]}}`))
	}))

	res, err := p.handleComments(map[string]interface{}{"video": "BV1GJ411x7h7"})
	if err != nil {
		t.Fatalf("无评论不算错误: %v", err)
	}
	content := res.(map[string]interface{})["content"].(string)
	firstLine := strings.SplitN(content, "\n", 2)[0]

	wantHeader := "视频 BV1GJ411x7h7 (av80433022) 暂无评论（接口返回 code=0，page.count=0）"
	if firstLine != wantHeader {
		t.Fatalf("暂无评论 header 不精确:\n  got  = %q\n  want = %q", firstLine, wantHeader)
	}
	if n := strings.Count(content, "av80433022"); n != 1 {
		t.Fatalf("正文里 av80433022 出现 %d 次, want 1（重复渲染缺陷）: %q", n, content)
	}
	if n := strings.Count(content, "av"); n != 1 {
		t.Fatalf("正文里 \"av\" 出现 %d 次, want 1: %q", n, content)
	}
}

// 排序标签也要钉住：header 里 hot→热度 / time→时间 不能错位。
// 这条同时防"改了格式串却把参数顺序搞乱"。
func TestCommentsHeaderSortLabel(t *testing.T) {
	for _, c := range []struct {
		sortArg string
		want    string
	}{
		{"hot", "按热度排序"},
		{"", "按热度排序"}, // 默认 hot
		{"time", "按时间排序"},
	} {
		p, _, _ := newTestPlugin(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"code":0,"data":{"page":{"count":5,"size":20,"pn":1},
				"replies":[{"rpid":1,"member":{"uname":"u"},"content":{"message":"m"},"ctime":1600000000}]}}`))
		}))
		args := map[string]interface{}{"video": "BV1GJ411x7h7"}
		if c.sortArg != "" {
			args["sort"] = c.sortArg
		}
		res, err := p.handleComments(args)
		if err != nil {
			t.Fatalf("sort=%q 报错: %v", c.sortArg, err)
		}
		firstLine := strings.SplitN(res.(map[string]interface{})["content"].(string), "\n", 2)[0]
		if !strings.Contains(firstLine, c.want) {
			t.Errorf("sort=%q 的 header 缺 %q: %q", c.sortArg, c.want, firstLine)
		}
	}
}

// ============================================================================
// 三处正文渲染的精确断言（结构化字段覆盖率 ≠ 用户可见输出被覆盖）
// ============================================================================
//
// ## 为什么这批用例必须用「精确整行 + 计数」
//
// 之前 av 号被拼两次（"BV1GJ411x7h7 (av80433022)（av80433022）"）能活到实装，
// 就是因为没有一条测试看过正文。`strings.Contains(content, "热门")` 这类断言
// 对重复渲染、字段错位、编号空白**一律照样绿** —— 写了等于没写。
//
// 所以下面每条都做到：
//   · 整行 want 字符串写死（多一个字符、少一个空格都会红）
//   · 各字段给**互不相同**的值（播放 1111 / 评论 2222 / 时长 1:02:05 …）
//     ⇒ 参数错位必红：把 UP 名填进编号位、把评论数填进播放位都能被抓住
//   · 双数场景（总数 vs 显示数）断言两者不等且各自正确

// ---- 1) handlePopular 成功路径 ----
//
// 这行有 7 个渲染位、6 个字段，是最容易参数错位的形态。
func TestPopularContentExactRendering(t *testing.T) {
	p, _, _ := newTestPlugin(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"list":[{
			"bvid":"BV1aa411c7mD","title":"标题甲","duration":3725,
			"owner":{"mid":999,"name":"UP主乙"},
			"stat":{"view":1111,"reply":2222,"danmaku":3333,"like":4444,"coin":5555,"favorite":6666,"share":7777}}]}}`))
	}))

	res, err := p.handlePopular(map[string]interface{}{})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	content := res.(map[string]interface{})["content"].(string)

	// 整段精确比对：header + 条目行两行。
	// 3725 秒 = 1:02:05（大于 1 小时的分支）。
	want := "B站综合热门（当前榜）：\n" +
		" 1. 标题甲\n" +
		"    BV1aa411c7mD | UP UP主乙 | 1:02:05 | 播放 1111 | 评论 2222\n"
	if content != want {
		t.Fatalf("热门正文不精确:\n  got  = %q\n  want = %q", content, want)
	}

	// 计数断言：每个值只出现一次 —— 抓「同一字段被渲染两遍」这类重复。
	for _, v := range []string{"标题甲", "BV1aa411c7mD", "UP主乙", "1:02:05", "1111", "2222"} {
		if n := strings.Count(content, v); n != 1 {
			t.Errorf("%q 在正文里出现 %d 次, want 1:\n%s", v, n, content)
		}
	}
}

// 多页/多条：编号必须连续递增，且第二页的字段不能串到第一条上。
func TestPopularContentMultiPageNumbering(t *testing.T) {
	p, _, _ := newTestPlugin(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("pn") == "1" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"list":[
				{"bvid":"BV1aa411c7mD","title":"页1甲","duration":61,"owner":{"name":"UP1"},"stat":{"view":11,"reply":22}},
				{"bvid":"BV17x411w7KC","title":"页1乙","duration":62,"owner":{"name":"UP2"},"stat":{"view":33,"reply":44}}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"list":[
			{"bvid":"BV1J14y1M713","title":"页2丙","duration":63,"owner":{"name":"UP3"},"stat":{"view":55,"reply":66}}]}}`))
	}))
	res, err := p.handlePopular(map[string]interface{}{"pages": 2, "per_page": 2})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	content := res.(map[string]interface{})["content"].(string)
	want := "B站综合热门（当前榜）：\n" +
		" 1. 页1甲\n    BV1aa411c7mD | UP UP1 | 1:01 | 播放 11 | 评论 22\n" +
		" 2. 页1乙\n    BV17x411w7KC | UP UP2 | 1:02 | 播放 33 | 评论 44\n" +
		" 3. 页2丙\n    BV1J14y1M713 | UP UP3 | 1:03 | 播放 55 | 评论 66\n"
	if content != want {
		t.Fatalf("多页热门正文不精确:\n  got  = %q\n  want = %q", content, want)
	}
	if n := res.(map[string]interface{})["count"].(int); n != 3 {
		t.Errorf("count = %d, want 3", n)
	}
}

// ---- 2) handleSeenList ----
//
// header 里 `p.st.count()`（总数）与 `len(recs)`（显示数）是两个不同值。
// limit 生效时二者不等 —— 这正是最该钉住的地方（很容易写成同一个数）。
func TestSeenListContentExactRendering(t *testing.T) {
	p, _, _ := newTestPlugin(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	// 造 3 条记录，字段各不相同；SeenCount 靠多次 upsert 累出来。
	for _, s := range []struct {
		bv, title, up string
		aid, dur      int64
		times         int
	}{
		{"BV1aa411c7mD", "标题甲", "UP甲", 2, 61, 3},
		{"BV17x411w7KC", "标题乙", "UP乙", 170001, 3725, 7},
		{"BV1J14y1M713", "标题丙", "UP丙", 777927636, 8, 1},
	} {
		for i := 0; i < s.times; i++ {
			if _, _, err := p.st.upsert(videoRecord{
				BVID: s.bv, AID: s.aid, Title: s.title, UP: s.up, Duration: s.dur,
			}); err != nil {
				t.Fatalf("upsert: %v", err)
			}
		}
	}
	// 手动把 FirstSeen 定成可预测值（否则同一毫秒内的顺序不可靠）。
	//
	// ★ 必须按**去重后的指针**赋值：records 里同一个 *videoRecord 会挂在
	// 两个键下（BVID 与 avN），直接遍历 records 会把同一条记录赋值两次，
	// 得到哪一次的 FirstSeen 取决于 map 迭代顺序 ⇒ 测试非确定（实测会随机红）。
	seenPtr := map[*videoRecord]bool{}
	// 按 BVID **显式**指定日期：靠遍历计数赋值仍依赖 map 迭代顺序，
	// 哪条拿哪天不确定 ⇒ 测试照样会随机红（这就是第一版的坑）。
	wantDay := map[string]int{
		"BV17x411w7KC": 6, // 最新 → 排第 1
		"BV1aa411c7mD": 4, // 次新 → 排第 2
		"BV1J14y1M713": 2, // 最旧 → limit=2 时被截掉
	}
	p.st.mu.Lock()
	for _, v := range p.st.records {
		if seenPtr[v] {
			continue
		}
		seenPtr[v] = true
		d, ok := wantDay[v.BVID]
		if !ok {
			p.st.mu.Unlock()
			t.Fatalf("测试数据里出现了未预期的 BVID: %q", v.BVID)
		}
		v.FirstSeen = time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	}
	p.st.mu.Unlock()

	res, err := p.handleSeenList(map[string]interface{}{"limit": 2})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	m := res.(map[string]interface{})
	content := m["content"].(string)

	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("行数 = %d, want 3（1 header + 2 条目）:\n%s", len(lines), content)
	}

	// ★ header 精确比对：总数 3、显示数 2 —— **两个数必须不同且各自正确**。
	// 若实现把两者写成同一个（都用 len(recs) 或都用 count()），这里必红。
	wantHeader := "本地查重台账（共 3 条，显示 2 条）："
	if lines[0] != wantHeader {
		t.Fatalf("header 不精确:\n  got  = %q\n  want = %q", lines[0], wantHeader)
	}
	if strings.Contains(lines[0], "共 2 条") {
		t.Errorf("总数被错写成显示数（共 2 条）: %q", lines[0])
	}

	// limit=2 时按 FirstSeen 倒序取最新两条：01-06(乙) 与 01-04(甲)。
	// 每条整行精确比对，把 SeenCount（7 / 3）与时长（1:02:05 / 1:01）都钉住。
	wantLine1 := " 1. BV17x411w7KC  标题乙  | UP UP乙 | 1:02:05 | 首次 2026-01-06T00:00:00Z | 取回 7 次"
	if lines[1] != wantLine1 {
		t.Fatalf("条目 1 不精确:\n  got  = %q\n  want = %q", lines[1], wantLine1)
	}
	wantLine2 := " 2. BV1aa411c7mD  标题甲  | UP UP甲 | 1:01 | 首次 2026-01-04T00:00:00Z | 取回 3 次"
	if lines[2] != wantLine2 {
		t.Fatalf("条目 2 不精确:\n  got  = %q\n  want = %q", lines[2], wantLine2)
	}

	if n := m["count"].(int); n != 2 {
		t.Errorf("count = %d, want 2（= 显示数，不是总数）", n)
	}

	// limit 大于总数时，两个数应相等 —— 反向确认「不等」不是写死的。
	res2, err := p.handleSeenList(map[string]interface{}{"limit": 100})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	h2 := strings.SplitN(res2.(map[string]interface{})["content"].(string), "\n", 2)[0]
	if want := "本地查重台账（共 3 条，显示 3 条）："; h2 != want {
		t.Fatalf("limit>总数 时 header 不精确:\n  got  = %q\n  want = %q", h2, want)
	}
}

// 空台账分支：正文与 count 都要对（这条分支以前也没有断言）。
func TestSeenListEmptyContent(t *testing.T) {
	p, _, _ := newTestPlugin(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	res, err := p.handleSeenList(map[string]interface{}{})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	m := res.(map[string]interface{})
	want := "本地查重台账为空。用 bilibili_video 取回任意视频后会自动登记。"
	if got := m["content"].(string); got != want {
		t.Fatalf("空台账正文不精确:\n  got  = %q\n  want = %q", got, want)
	}
	if n := m["count"].(int); n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
}

// ---- 3) handleSeenForget 两条分支 ----
//
// 「删了」与「本来就没有」必须能区分：说错会误导用户以为删成功。
func TestSeenForgetBothBranchesExact(t *testing.T) {
	p, _, _ := newTestPlugin(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	// 用 BV1xx411c7mD（aid=2），正文短、断言可读。
	if _, _, err := p.st.upsert(videoRecord{BVID: "BV1xx411c7mD", AID: 2, Title: "要删的"}); err != nil {
		t.Fatal(err)
	}

	// 分支 A：真的删掉了。
	res, err := p.handleSeenForget(map[string]interface{}{"video": "BV1xx411c7mD"})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	a := res.(map[string]interface{})
	wantA := "已从台账删除 BV1xx411c7mD (av2)。"
	if got := a["content"].(string); got != wantA {
		t.Fatalf("删除成功正文不精确:\n  got  = %q\n  want = %q", got, wantA)
	}
	if ok, _ := a["removed"].(bool); !ok {
		t.Errorf("removed = false, want true")
	}

	// 分支 B：本来就没有 —— 正文必须与分支 A 不同，且明确说"未做任何修改"。
	res2, err := p.handleSeenForget(map[string]interface{}{"video": "BV1xx411c7mD"})
	if err != nil {
		t.Fatalf("未命中不算错误: %v", err)
	}
	b := res2.(map[string]interface{})
	wantB := "台账里没有 BV1xx411c7mD (av2)，未做任何修改。"
	if got := b["content"].(string); got != wantB {
		t.Fatalf("未命中正文不精确:\n  got  = %q\n  want = %q", got, wantB)
	}
	if ok, _ := b["removed"].(bool); ok {
		t.Errorf("removed = true, want false（未命中不能报删除成功）")
	}

	// 两句正文必须互不相同：模板如果被写成同一句，上面两条精确比对之一会红，
	// 这里再显式钉一次「必须可区分」的意图。
	if a["content"] == b["content"] {
		t.Error("删除成功与未命中的正文相同 —— 用户无法区分是否真的删掉了")
	}
}

// ---- 附带：只有 aid 的记录不能渲染成空白编号（本轮探针发现的真缺陷）----
//
// 平台在个别响应里只回 aid 不回 bvid，而 fetchVideo 的守卫只拦「两者都空」，
// 于是 BVID 为空的记录会正常进台账。原先 seen_check / seen_list 直接渲染
// rec.BVID，正文里编号位是空的（"命中： 已见过。" / " 1.   标题"）。
func TestSeenContentFallsBackToAIDWhenBVIDEmpty(t *testing.T) {
	p, _, _ := newTestPlugin(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	// 直接造一条「只有 aid」的记录（模拟平台只回 aid 的响应）。
	if _, _, err := p.st.upsert(videoRecord{AID: 170001, Title: "只有aid", UP: "UP甲", Duration: 120, Pages: 1}); err != nil {
		t.Fatal(err)
	}

	// seen_check：正文里的编号位必须是 av170001，不能是空白。
	res, err := p.handleSeenCheck(map[string]interface{}{"video": "av170001"})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	content := res.(map[string]interface{})["content"].(string)
	firstLine := strings.SplitN(content, "\n", 2)[0]
	if want := "命中：av170001 已见过。"; firstLine != want {
		t.Fatalf("只有 aid 时 seen_check 首行不精确:\n  got  = %q\n  want = %q", firstLine, want)
	}
	if strings.Contains(firstLine, "命中： 已见过") {
		t.Fatal("编号位渲染成空白（BVID 为空时没有兜底）")
	}

	// seen_list：条目行的编号位同理。
	res2, err := p.handleSeenList(map[string]interface{}{})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	line := strings.Split(strings.TrimRight(res2.(map[string]interface{})["content"].(string), "\n"), "\n")[1]
	if !strings.HasPrefix(line, " 1. av170001  ") {
		t.Fatalf("只有 aid 时 seen_list 条目行编号位不对:\n  got  = %q", line)
	}

	// 结构化字段 bvid 保持空（平台确实没给），不拿 avN 冒充。
	if bv, _ := res.(map[string]interface{})["bvid"].(string); bv != "" {
		t.Errorf("结构化字段 bvid = %q, want 空串（平台没给就不该编）", bv)
	}
}
