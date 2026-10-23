package main

// 接口层：把「HTTP JSON」翻成插件内部的结构体。
//
// 只做两件事：拼参数、解结构。**不做**格式化输出，也不做业务判断
//（比如"要不要落一条本地记录"）—— 那些留在 handler 里，便于单测只测
// 解析逻辑而不用起插件。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ---- /x/web-interface/view ----

type apiOwner struct {
	Mid  int64  `json:"mid"`
	Name string `json:"name"`
}

type apiStat struct {
	View    int64 `json:"view"`
	Danmaku int64 `json:"danmaku"`
	Reply   int64 `json:"reply"`
	Fav     int64 `json:"favorite"`
	Coin    int64 `json:"coin"`
	Share   int64 `json:"share"`
	Like    int64 `json:"like"`
}

type apiPage struct {
	CID      int64  `json:"cid"`
	Page     int    `json:"page"`
	Part     string `json:"part"`
	Duration int64  `json:"duration"`
}

type apiVideoData struct {
	BVID     string    `json:"bvid"`
	AID      int64     `json:"aid"`
	Title    string    `json:"title"`
	Desc     string    `json:"desc"`
	Pubdate  int64     `json:"pubdate"`
	Duration int64     `json:"duration"`
	Owner    apiOwner  `json:"owner"`
	Stat     apiStat   `json:"stat"`
	Pages    []apiPage `json:"pages"`
}

// fetchVideo 取视频主体信息与分 P 列表。
//
// 同一个接口（/x/web-interface/view）同时给主体与 pages，所以「视频信息」
// 与「分P」不拆两个请求 —— 少一次请求就少一次被风控的机会。
func (c *client) fetchVideo(ctx context.Context, r videoRef) (*apiVideoData, error) {
	q := url.Values{}
	if r.BVID != "" {
		q.Set("bvid", r.BVID)
	} else {
		q.Set("aid", strconv.FormatInt(r.AID, 10))
	}
	raw, err := c.get(ctx, "/x/web-interface/view", q, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Data apiVideoData `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析视频信息失败：%w（原文：%s）", err, snippet(raw))
	}
	if out.Data.BVID == "" && out.Data.AID == 0 {
		// code=0 但 data 为空：不当作「成功但没有数据」，直接报错，
		// 否则上层会输出一个空标题的视频，使用者以为是插件坏了。
		return nil, fmt.Errorf("视频信息为空（接口返回 code=0 但没有 data）：BV 号可能不存在或已被删除")
	}
	return &out.Data, nil
}

// ---- 字幕 /x/player/wbi/v2 ----

// apiSubtitleItem 只保留本插件真正用到的字段：多解一份平台字段就要跟着
// 平台变更维护一份，而示例的价值在于"读得懂、改得快"。
type apiSubtitleItem struct {
	Lan    string `json:"lan"`
	LanDoc string `json:"lan_doc"`
	SubURL string `json:"subtitle_url"`
	IsAI   bool   `json:"is_ai"` // 部分返回里没有该字段，零值即 false
}

type apiSubtitleBody struct {
	Body []struct {
		From    float64 `json:"from"`
		To      float64 `json:"to"`
		Content string  `json:"content"`
	} `json:"body"`
}

// fetchSubtitles 取某分 P 的字幕清单。
//
// 这个端点是 wbi 签名的：不带 w_rid 会直接 -403，而错误信息里不会提到签名，
// 只说「访问权限不足」，很容易被误判成"需要大会员"。
//
// ★ 未登录时平台通常**不下发 AI 字幕**，返回的空清单是"真的空"而不是
// 接口坏了 —— 上层要把这一点写进输出，避免使用者以为插件漏了字幕。
func (c *client) fetchSubtitles(ctx context.Context, r videoRef, cid int64) ([]apiSubtitleItem, error) {
	q := url.Values{}
	q.Set("cid", strconv.FormatInt(cid, 10))
	// 只带一个编号：两个都给时平台按哪个取值没有文档保证，
	// 而"传了却被忽略"会产生很难查的偏差。
	if r.BVID != "" {
		q.Set("bvid", r.BVID)
	} else {
		q.Set("aid", strconv.FormatInt(r.AID, 10))
	}
	raw, err := c.getSigned(ctx, "/x/player/wbi/v2", q, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Data struct {
			Subtitle struct {
				Subtitles []apiSubtitleItem `json:"subtitles"`
			} `json:"subtitle"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析字幕清单失败：%w（原文：%s）", err, snippet(raw))
	}
	return out.Data.Subtitle.Subtitles, nil
}

// fetchSubtitleBody 下载字幕正文（subtitle_url 指向 i0.hdslb.com，是绝对/协议相对地址）。
func (c *client) fetchSubtitleBody(ctx context.Context, subURL string) (*apiSubtitleBody, error) {
	full := subURL
	if strings.HasPrefix(full, "//") {
		// 协议相对地址（平台常见写法）：按当前 API 的协议补全。
		if strings.HasPrefix(c.base, "http://") {
			full = "http:" + full
		} else {
			full = "https:" + full
		}
	}
	if !strings.HasPrefix(full, "http://") && !strings.HasPrefix(full, "https://") {
		return nil, fmt.Errorf("字幕地址不是 http(s) 绝对地址：%q", subURL)
	}
	raw, err := c.getAbsolute(ctx, full)
	if err != nil {
		return nil, err
	}
	var body apiSubtitleBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("解析字幕正文失败：%w（原文：%s）", err, snippet(raw))
	}
	return &body, nil
}

// ---- 评论 /x/v2/reply ----

type apiReplyMember struct {
	Uname string `json:"uname"`
	Mid   string `json:"mid"`
}

type apiReplyContent struct {
	Message string `json:"message"`
}

type apiReply struct {
	Rpid    int64           `json:"rpid"`
	OID     int64           `json:"oid"`
	Mid     int64           `json:"mid"`
	Root    int64           `json:"root"`
	Parent  int64           `json:"parent"`
	CTime   int64           `json:"ctime"`
	Like    int64           `json:"like"`
	RCount  int             `json:"rcount"`
	Member  apiReplyMember  `json:"member"`
	Content apiReplyContent `json:"content"`
	Replies []apiReply      `json:"replies"`
}

type apiReplyPage struct {
	Count int `json:"count"`
}

type apiReplyData struct {
	Page    apiReplyPage `json:"page"`
	Replies []apiReply   `json:"replies"`
	// 部分场景（如被折叠）返回 top_replies 而不是 replies。
	TopReplies []apiReply `json:"top_replies"`
}

// fetchComments 拉一页评论。
//
// 用 /x/v2/reply（按 pn 翻页）而不是新的 /x/v2/reply/wbi/main（游标翻页）：
// 游标翻页要维护 pagination_str 的 next 游标，本示例的价值在于读懂流程，
// 而 pn 翻页能让「有上限、不无限翻」这件事在代码里一眼看清。
func (c *client) fetchComments(ctx context.Context, r videoRef, sortBy int, pn, ps int) (*apiReplyData, error) {
	q := url.Values{}
	q.Set("type", "1") // 1 = 视频
	q.Set("oid", strconv.FormatInt(r.AID, 10))
	q.Set("sort", strconv.Itoa(sortBy)) // 0=按时间 2=按热度
	q.Set("pn", strconv.Itoa(pn))
	q.Set("ps", strconv.Itoa(ps))
	raw, err := c.get(ctx, "/x/v2/reply", q, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Data apiReplyData `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析评论失败：%w（原文：%s）", err, snippet(raw))
	}
	return &out.Data, nil
}

// ---- 热门流 /x/web-interface/popular ----

type apiPopularItem struct {
	BVID     string   `json:"bvid"`
	Title    string   `json:"title"`
	Duration int64    `json:"duration"`
	Owner    apiOwner `json:"owner"`
	Stat     apiStat  `json:"stat"`
}

func (c *client) fetchPopular(ctx context.Context, pn, ps int) ([]apiPopularItem, error) {
	q := url.Values{}
	q.Set("pn", strconv.Itoa(pn))
	q.Set("ps", strconv.Itoa(ps))
	raw, err := c.get(ctx, "/x/web-interface/popular", q, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Data struct {
			List []apiPopularItem `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析热门列表失败：%w（原文：%s）", err, snippet(raw))
	}
	if len(out.Data.List) == 0 {
		// 热门流「成功但空」几乎总是风控/反代导致，而不是当天没热门。
		// 报错而不是返回空列表 —— 静默的空结果会被使用者当成"今天没内容"。
		return nil, fmt.Errorf("热门列表为空（接口返回 code=0 但 data.list 为 0 条）：可能是风控或反代改写了响应")
	}
	return out.Data.List, nil
}

// ---- 发评论 /x/v2/comment/add ----

// postComment 发一条评论或回复。
//
// ★ 这是本插件唯一的写操作，调用方必须已经完成两道闸门校验
// （配置开关 + csrf token），这里只负责发请求。
func (c *client) postComment(ctx context.Context, r videoRef, message, csrf string, root, parent int64) (int64, error) {
	form := url.Values{}
	form.Set("type", "1")
	form.Set("oid", strconv.FormatInt(r.AID, 10))
	form.Set("message", message)
	// csrf = Cookie 里的 bili_jct：平台用它防跨站，缺了必然 -111（csrf 校验失败）。
	form.Set("csrf", csrf)
	if root > 0 {
		form.Set("root", strconv.FormatInt(root, 10))
	}
	if parent > 0 {
		form.Set("parent", strconv.FormatInt(parent, 10))
	}
	raw, err := c.postForm(ctx, "/x/v2/comment/add", form, nil)
	if err != nil {
		return 0, err
	}
	var out struct {
		Data struct {
			Rpid  int64 `json:"rpid"`
			Reply struct {
				Rpid int64 `json:"rpid"`
			} `json:"reply"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, fmt.Errorf("解析发评论结果失败：%w（原文：%s）", err, snippet(raw))
	}
	if out.Data.Rpid != 0 {
		return out.Data.Rpid, nil
	}
	return out.Data.Reply.Rpid, nil
}
