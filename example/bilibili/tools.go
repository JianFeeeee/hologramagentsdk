package main

// 工具注册与 handler。
//
// ## 声明约定（照 docs/guide/parallel-tool-declaration.md）
//
//   - Name 放结构体首位（一眼能对上注册名）；声明类字段（NoMemory /
//     ParallelSafe / Timeout …）放末尾。
//   - **网络请求一律不声明 ParallelSafe**：默认串行是安全的。同批并发
//     多个请求会同时打同一个平台，正是风控最容易命中的形态；而且它们
//     共享 wbi 密钥缓存，并发刷新没有收益。
//   - 只有两个纯本地只读工具声明了 ParallelSafe。
//
// ## NoMemory 给谁
//
// 评论、字幕正文、热门流都是**大块外部内容**：把它们按原文塞进记忆计算
// 只会挤占上下文预算，而"上周某视频第三条评论说了什么"极少需要召回。
// 视频信息（标题/UP/时长）保留参与记忆 —— 那是用户真正会回头引用的事实。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

func (p *Plugin) registerTools() {
	tp := p.name + "_"

	// ---- 视频信息 + 分P ----
	p.sdk.RegisterTool(tp+"video", sdk.ToolDef{
		Name: tp + "video",
		Description: "获取B站视频的主体信息与分P清单：标题、UP主、时长、发布时间、播放/点赞/评论等统计、" +
			"以及每个分P的标题与 cid。cid 是字幕工具的必填入参。只读，不需要登录",
		Parameters: obj(map[string]interface{}{
			"video": str("BV 号（如 BV17x411w7KC）、av 号（如 av170001）或完整视频链接"),
		}, "video"),
	}, p.handleVideo)

	// ---- 字幕 ----
	p.sdk.RegisterTool(tp+"subtitles", sdk.ToolDef{
		Name: tp + "subtitles",
		Description: "列出某分P的字幕轨（语言、是否 AI 生成），可选把选中语言的字幕正文导出成本地文件。" +
			"省略 cid 时用第 1 个分P。匿名访问时平台常常不下发 AI 字幕（清单为空是平台行为，不是插件故障）",
		Parameters: obj(map[string]interface{}{
			"video":    str("BV 号 / av 号 / 视频链接"),
			"cid":      num("分P的 cid（从 bilibili_video 的 pages 里取）；省略则用第 1 个分P"),
			"lang":     str("要导出正文的语言代码（如 zh-CN / ai-zh）；省略则只列清单不导出"),
			"save_dir": str("导出目录（相对路径落到插件输出目录下）；省略则保存到输出目录"),
		}, "video"),
		// 分页/循环类工具把时间预算声明出来：内核默认 60s，这里给到 90s
		// 是为了让「翻满 10 页 + 慢网络」不至于被内核先掐断。
		Timeout:  "90s",
		NoMemory: true,
	}, p.handleSubtitles)

	// ---- 评论列表 ----
	p.sdk.RegisterTool(tp+"comments", sdk.ToolDef{
		Name: tp + "comments",
		Description: "按页拉取视频评论（含楼中楼的第一层回复），可按热度或时间排序。" +
			"翻页数有上限（配置 max_pages，硬上限 10），不会无限翻。接口报错会带状态码与平台原话返回，不静默给空列表",
		Parameters: obj(map[string]interface{}{
			"video":    str("BV 号 / av 号 / 视频链接"),
			"sort":     str("排序：hot（按热度，默认）| time（按时间）"),
			"pages":    num("翻几页（默认取配置 max_pages，硬上限 10）"),
			"per_page": num("每页条数（默认取配置 max_items，硬上限 100）"),
		}, "video"),
		Timeout:  "90s",
		NoMemory: true,
	}, p.handleComments)

	// ---- 热门流 ----
	p.sdk.RegisterTool(tp+"popular", sdk.ToolDef{
		Name: tp + "popular",
		Description: "拉取B站综合热门视频流（当前榜），支持翻页（受 max_pages 限制）。" +
			"常用来回答「最近有什么热门/大家在聊什么」，不适合查特定视频",
		Parameters: obj(map[string]interface{}{
			"pages":    num("翻几页（默认 1，硬上限 10）"),
			"per_page": num("每页条数（默认取配置 max_items，硬上限 100）"),
		}),
		Timeout:  "90s",
		NoMemory: true,
	}, p.handlePopular)

	// ---- 本地查重 ----
	p.sdk.RegisterTool(tp+"seen_check", sdk.ToolDef{
		Name: tp + "seen_check",
		Description: "【纯本地，不发任何网络请求】查询某个 BV 号/av 号/链接是否已在本地查重台账里：" +
			"命中则返回首次/最近见到时间、标题、UP、命中次数；未命中明确说「未命中」，不假装成功",
		Parameters: obj(map[string]interface{}{
			"video": str("BV 号 / av 号 / 视频链接"),
		}, "video"),
		NoMemory:     true,
		ParallelSafe: true,
	}, p.handleSeenCheck)

	p.sdk.RegisterTool(tp+"seen_list", sdk.ToolDef{
		Name:        tp + "seen_list",
		Description: "【纯本地】列出台账里最近见过的视频（按首次见到时间倒序），用于回看自己处理过什么",
		Parameters: obj(map[string]interface{}{
			"limit": num("返回条数（默认 20，上限 200）"),
		}),
		NoMemory:     true,
		ParallelSafe: true,
	}, p.handleSeenList)

	p.sdk.RegisterTool(tp+"seen_forget", sdk.ToolDef{
		Name:        tp + "seen_forget",
		Description: "【纯本地写】从台账里删掉一条记录（下次 bilibili_video 取回该视频时会重新登记）",
		Parameters: obj(map[string]interface{}{
			"video": str("BV 号 / av 号 / 视频链接"),
		}, "video"),
		NoMemory: true,
	}, p.handleSeenForget)

	// ---- 发评论（默认关闭） ----
	p.sdk.RegisterTool(tp+"comment_post", sdk.ToolDef{
		Name: tp + "comment_post",
		Description: "【写操作，默认关闭】给视频发评论或回复某条评论。" +
			"必须同时满足：配置 allow_comment_post=true 且 Cookie 含 bili_jct（csrf）。" +
			"任一不满足会直接拒绝并说明缺哪一项",
		Parameters: obj(map[string]interface{}{
			"video":   str("BV 号 / av 号 / 视频链接"),
			"message": str("评论正文（1~" + fmt.Sprint(maxCommentRunes) + " 字）"),
			"root":    num("要回复的根评论 rpid（回复某条主评论时填）"),
			"parent":  num("要回复的目标评论 rpid（回复楼中楼时填，通常与 root 同值）"),
		}, "video", "message"),
		// 写操作：即使已开启，也不允许被并发调度 —— 同批两条评论的先后
		// 提交顺序对使用者是可感知的（楼层关系），必须保序。
		Serial:   true,
		NoMemory: true,
	}, p.handleCommentPost)
}

// ---- handler：视频信息 ----

func (p *Plugin) handleVideo(args map[string]interface{}) (interface{}, error) {
	p.refresh()
	ref, err := parseRef(argStr(args, "video"))
	if err != nil {
		return nil, err
	}
	cfg := p.snapshot()

	// 查重前置：先看本地台账，命中就把"已见过"这个事实一并告诉使用者。
	// 这不替代真实接口（信息可能已变化），但能让"我是不是已经处理过"一眼可答。
	prev, lerr := p.st.lookup(ref)
	if lerr != nil {
		// 台账坏了就报错：静默当成"未命中"会让使用者以为记录被清空，
		// 而实际只是这个文件读不了。
		return nil, lerr
	}

	ctx, cancel := ctxWith(context.Background(), budgetSingle)
	defer cancel()

	data, err := p.apiClient().fetchVideo(ctx, ref)
	if err != nil {
		return nil, err
	}

	rec := videoRecord{
		BVID:     data.BVID,
		AID:      data.AID,
		Title:    data.Title,
		UP:       data.Owner.Name,
		Duration: data.Duration,
		Pages:    len(data.Pages),
	}
	// record_on_fetch=false 时只读不写：有些调用方只要信息，不想污染台账。
	if cfg.recordOnGet {
		if _, _, err := p.st.upsert(rec); err != nil {
			return nil, err
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "标题：%s\n", data.Title)
	fmt.Fprintf(&b, "编号：%s / av%d\n", data.BVID, data.AID)
	fmt.Fprintf(&b, "UP主：%s（mid %d）\n", data.Owner.Name, data.Owner.Mid)
	fmt.Fprintf(&b, "时长：%s\n", hhmmss(data.Duration))
	if data.Pubdate > 0 {
		fmt.Fprintf(&b, "发布：%s\n", time.Unix(data.Pubdate, 0).Format("2006-01-02 15:04:05"))
	}
	fmt.Fprintf(&b, "播放 %d / 弹幕 %d / 评论 %d / 点赞 %d / 投币 %d / 收藏 %d / 分享 %d\n",
		data.Stat.View, data.Stat.Danmaku, data.Stat.Reply, data.Stat.Like,
		data.Stat.Coin, data.Stat.Fav, data.Stat.Share)
	fmt.Fprintf(&b, "链接：https://www.bilibili.com/video/%s\n", data.BVID)
	switch {
	case prev != nil:
		fmt.Fprintf(&b, "本地台账：**已见过**（首次 %s，最近 %s，第 %d 次取回）\n",
			prev.FirstSeen, prev.LastSeen, prev.SeenCount)
	case cfg.recordOnGet:
		b.WriteString("本地台账：未见过（本次已登记）\n")
	default:
		// 关掉自动登记时必须说实话：否则下一次查重会"未命中"，
		// 而使用者以为这次已经记过了。
		fmt.Fprintf(&b, "本地台账：未见过（配置 %s=false，本次未登记）\n", cfgSeenOnFetch)
	}

	if len(data.Pages) > 1 {
		fmt.Fprintf(&b, "\n分P（共 %d 个，cid 供 bilibili_subtitles 使用）：\n", len(data.Pages))
		for _, pg := range data.Pages {
			fmt.Fprintf(&b, "  P%d  cid=%d  %s (%s)\n", pg.Page, pg.CID, pg.Part, hhmmss(pg.Duration))
		}
	} else if len(data.Pages) == 1 {
		fmt.Fprintf(&b, "\n分P：单P，cid=%d\n", data.Pages[0].CID)
	}
	if data.Desc != "" {
		desc := data.Desc
		if len([]rune(desc)) > 300 {
			desc = string([]rune(desc)[:300]) + "…"
		}
		fmt.Fprintf(&b, "\n简介：%s\n", desc)
	}

	return map[string]interface{}{
		"content":   b.String(),
		"bvid":      data.BVID,
		"aid":       data.AID,
		"title":     data.Title,
		"owner":     data.Owner.Name,
		"duration":  data.Duration,
		"page_list": pagesToAny(data.Pages),
	}, nil
}

// ---- handler：字幕 ----

func (p *Plugin) handleSubtitles(args map[string]interface{}) (interface{}, error) {
	p.refresh()
	ref, err := parseRef(argStr(args, "video"))
	if err != nil {
		return nil, err
	}
	cfg := p.snapshot()
	wantLang := strings.TrimSpace(argStr(args, "lang"))
	cid := int64(argInt(args, "cid", 0))

	ctx, cancel := ctxWith(context.Background(), budgetSubtitles)
	defer cancel()

	cli := p.apiClient()

	// 没给 cid 就得先问分P：字幕是**按分P**的，拿不到 cid 无法请求。
	if cid <= 0 {
		data, err := cli.fetchVideo(ctx, ref)
		if err != nil {
			return nil, err
		}
		if len(data.Pages) == 0 {
			return nil, fmt.Errorf("视频 %s 没有分P信息（接口返回 pages 为空），无法定位字幕", ref)
		}
		cid = data.Pages[0].CID
	}

	subs, err := cli.fetchSubtitles(ctx, ref, cid)
	if err != nil {
		return nil, err
	}
	if len(subs) == 0 {
		// 空清单在这里是**可解释的**：不是接口失败，而是平台没给。
		// 必须说清，否则使用者会以为插件漏了功能。
		msg := fmt.Sprintf("视频 %s 的 cid=%d 没有下发任何字幕轨。", ref, cid)
		if strings.TrimSpace(cfg.cookie) == "" {
			msg += "\n注意：当前是**匿名**访问，平台通常不下发 AI 字幕/CC 字幕。要拿字幕请在配置里填登录态 Cookie（含 SESSDATA）。"
		} else {
			msg += "该视频可能本身没有字幕。"
		}
		return map[string]interface{}{"content": msg, "cid": cid, "subtitle_tracks": []interface{}{}}, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "视频 %s  cid=%d  字幕轨 %d 条：\n", ref, cid, len(subs))
	for i, s := range subs {
		kind := "人工字幕"
		if s.IsAI {
			kind = "AI 字幕"
		}
		fmt.Fprintf(&b, "  [%d] %s（%s，%s）\n", i+1, s.Lan, s.LanDoc, kind)
	}

	if wantLang == "" {
		b.WriteString("\n（只列清单。要看/导出正文，请带 lang 参数重试，例如 \"lang\": \"" + subs[0].Lan + "\"）")
		return map[string]interface{}{
			"content":         b.String(),
			"cid":             cid,
			"subtitle_tracks": tracksToAny(subs),
		}, nil
	}

	picked := pickSubtitle(subs, wantLang)
	if picked == nil {
		return nil, fmt.Errorf("没有语言为 %q 的字幕轨；可选：%s", wantLang, strings.Join(lanList(subs), ", "))
	}

	body, err := cli.fetchSubtitleBody(ctx, picked.SubURL)
	if err != nil {
		return nil, err
	}
	if len(body.Body) == 0 {
		return nil, fmt.Errorf("字幕 %q 的正文为空（subtitle_url 返回空 body），平台可能对匿名访问做了裁剪", picked.Lan)
	}

	dir, err := p.resolveDir(argStr(args, "save_dir"), cfg)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建导出目录 %s 失败：%w", dir, err)
	}
	name := fmt.Sprintf("%s_cid%d_%s.json", ref.BVID, cid, sanitizeFilename(picked.Lan))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(renderSubtitleJSON(ref, picked, body)), 0o644); err != nil {
		return nil, fmt.Errorf("写入字幕文件 %s 失败：%w", path, err)
	}
	if err := p.st.addSubtitleFile(ref, name); err != nil {
		return nil, err
	}

	fmt.Fprintf(&b, "\n已导出正文（%d 条）→ %s\n", len(body.Body), path)
	lines := subtitlePreview(body, 40)
	b.WriteString("\n正文前 40 条：\n")
	b.WriteString(lines)

	return map[string]interface{}{
		"content":         b.String(),
		"cid":             cid,
		"lang":            picked.Lan,
		"file":            path,
		"line_count":      len(body.Body),
		"subtitle_tracks": tracksToAny(subs),
	}, nil
}

// ---- handler：评论 ----

func (p *Plugin) handleComments(args map[string]interface{}) (interface{}, error) {
	p.refresh()
	ref, err := parseRef(argStr(args, "video"))
	if err != nil {
		return nil, err
	}
	cfg := p.snapshot()

	sortBy := 2 // 热度
	switch strings.ToLower(strings.TrimSpace(argStr(args, "sort"))) {
	case "", "hot":
	case "time":
		sortBy = 0
	default:
		return nil, fmt.Errorf("sort 只支持 hot 或 time，得到 %q", argStr(args, "sort"))
	}

	pages := clampInt(argInt(args, "pages", cfg.maxPages), 1, hardMaxPages)
	perPage := clampInt(argInt(args, "per_page", cfg.maxItems), 1, hardMaxItems)

	if ref.AID <= 0 {
		// 评论接口只吃 aid；parseRef 已把 BV 换算成 aid，走到这里说明换算失败。
		return nil, fmt.Errorf("无法从 %q 得到 av 号（评论接口只接受 aid）", argStr(args, "video"))
	}

	ctx, cancel := ctxWith(context.Background(), budgetComments)
	defer cancel()

	cli := p.apiClient()
	var b strings.Builder
	var total int
	collected := 0
	fetched := 0

	for pn := 1; pn <= pages; pn++ {
		// 预算耗尽快停：翻页循环必须有上限与超时，否则一次调用可能
		// 打满十几个请求并撞风控。
		if err := ctx.Err(); err != nil {
			if collected > 0 {
				fmt.Fprintf(&b, "\n（第 %d 页起停止：时间/预算用尽）\n", pn)
				break
			}
			return nil, fmt.Errorf("拉取评论超时（已翻 %d 页）：%w", pn-1, err)
		}

		data, err := cli.fetchComments(ctx, ref, sortBy, pn, perPage)
		if err != nil {
			return nil, err
		}
		fetched++
		if pn < pages {
			// 节流：不加间隔时一次调用可能几十毫秒内连打多页，正是风控点。
			// 用 select 等 ctx，让"预算用尽"能立刻打断等待。
			select {
			case <-time.After(cfg.pageDelay):
			case <-ctx.Done():
			}
		}
		total = data.Page.Count
		replies := data.Replies
		if len(replies) == 0 {
			replies = data.TopReplies
		}
		if len(replies) == 0 {
			if collected == 0 {
				// 第 1 页就空：可以是真的没评论，但要说清是"平台说 0 条"。
				// header 只用 %s：videoRef.String() 自己就带 "(av…)"，
				// 再拼一次 （av%d） 会渲染成 "… (av80433022)（av80433022）"。
				fmt.Fprintf(&b, "视频 %s 暂无评论（接口返回 code=0，page.count=%d）\n", ref, data.Page.Count)
			}
			break
		}

		if collected == 0 {
			fmt.Fprintf(&b, "视频 %s 评论共 %d 条，按%s排序：\n", ref, total, sortLabel(sortBy))
		}
		for _, r := range replies {
			collected++
			fmt.Fprintf(&b, "\n#%d %s（mid %d）%s 赞 %d 回复 %d rpid=%d\n",
				collected, r.Member.Uname, r.Mid, time.Unix(r.CTime, 0).Format("2006-01-02 15:04"), r.Like, r.RCount, r.Rpid)
			b.WriteString("  " + strings.ReplaceAll(clip(r.Content.Message, 500), "\n", "\n  ") + "\n")
			for _, sub := range r.Replies {
				fmt.Fprintf(&b, "    ↳ %s：%s\n", sub.Member.Uname, clip(strings.ReplaceAll(sub.Content.Message, "\n", " "), 200))
			}
		}
		if collected >= total {
			break
		}
	}

	if collected == 0 && b.Len() == 0 {
		// 走到这里说明循环一次都没产出内容 —— 不该发生，但绝不能静默返回空串。
		return nil, fmt.Errorf("未取到任何评论（既非接口错误也未得到数据），请检查视频 %s 是否允许评论", ref)
	}

	return map[string]interface{}{
		"content":       b.String(),
		"bvid":          ref.BVID,
		"aid":           ref.AID,
		"total":         total,
		"returned":      collected,
		"pages_fetched": fetched,
	}, nil
}

// ---- handler：热门流 ----

func (p *Plugin) handlePopular(args map[string]interface{}) (interface{}, error) {
	p.refresh()
	cfg := p.snapshot()
	pages := clampInt(argInt(args, "pages", 1), 1, hardMaxPages)
	perPage := clampInt(argInt(args, "per_page", cfg.maxItems), 1, hardMaxItems)

	ctx, cancel := ctxWith(context.Background(), budgetPopular)
	defer cancel()

	cli := p.apiClient()
	var b strings.Builder
	n := 0
	b.WriteString("B站综合热门（当前榜）：\n")
	for pn := 1; pn <= pages; pn++ {
		if err := ctx.Err(); err != nil {
			if n > 0 {
				fmt.Fprintf(&b, "\n（第 %d 页起停止：时间/预算用尽）\n", pn)
				break
			}
			return nil, fmt.Errorf("拉取热门超时：%w", err)
		}
		items, err := cli.fetchPopular(ctx, pn, perPage)
		if err != nil {
			return nil, err
		}
		if pn < pages {
			select {
			case <-time.After(cfg.pageDelay):
			case <-ctx.Done():
			}
		}
		for _, it := range items {
			n++
			fmt.Fprintf(&b, "%2d. %s\n    %s | UP %s | %s | 播放 %d | 评论 %d\n",
				n, clip(it.Title, 120), it.BVID, it.Owner.Name, hhmmss(it.Duration), it.Stat.View, it.Stat.Reply)
		}
		if len(items) < perPage {
			break
		}
	}
	if n == 0 {
		return nil, fmt.Errorf("热门流未返回任何条目")
	}
	return map[string]interface{}{"content": b.String(), "count": n}, nil
}

// ---- handler：本地查重 ----

func (p *Plugin) handleSeenCheck(args map[string]interface{}) (interface{}, error) {
	raw := argStr(args, "video")
	ref, err := parseRef(raw)
	if err != nil {
		return nil, err
	}
	rec, err := p.st.lookup(ref)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		// 未命中必须**明确**说未命中：只返回一个空 map 会让模型编一个结论。
		return map[string]interface{}{
			"content": fmt.Sprintf("未命中：%s 不在本地查重台账里（共 %d 条记录）。\n说明：台账只记录本插件取回过的视频；平台侧的历史/收藏/稍后再看需要登录态，本插件不读它们。",
				ref, p.st.count()),
			"hit":   false,
			"video": ref.String(),
		}, nil
	}
	return map[string]interface{}{
		"content": fmt.Sprintf("命中：%s 已见过。\n标题：%s\nUP主：%s\n时长：%s\n分P：%d\n首次见到：%s\n最近见到：%s\n累计取回：%d 次",
			rec.BVID, rec.Title, rec.UP, hhmmss(rec.Duration), rec.Pages, rec.FirstSeen, rec.LastSeen, rec.SeenCount),
		"hit":        true,
		"video":      ref.String(),
		"bvid":       rec.BVID,
		"title":      rec.Title,
		"up":         rec.UP,
		"first_seen": rec.FirstSeen,
		"last_seen":  rec.LastSeen,
		"seen_count": rec.SeenCount,
	}, nil
}

func (p *Plugin) handleSeenList(args map[string]interface{}) (interface{}, error) {
	limit := clampInt(argInt(args, "limit", 20), 1, 200)
	recs, err := p.st.list(limit)
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return map[string]interface{}{
			"content": "本地查重台账为空。用 bilibili_video 取回任意视频后会自动登记。",
			"count":   0,
			"videos":  []interface{}{},
		}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "本地查重台账（共 %d 条，显示 %d 条）：\n", p.st.count(), len(recs))
	for i, r := range recs {
		fmt.Fprintf(&b, "%2d. %s  %s  | UP %s | %s | 首次 %s | 取回 %d 次\n",
			i+1, r.BVID, clip(r.Title, 60), r.UP, hhmmss(r.Duration), r.FirstSeen, r.SeenCount)
	}
	out := make([]interface{}, 0, len(recs))
	for _, r := range recs {
		out = append(out, map[string]interface{}{
			"bvid": r.BVID, "aid": r.AID, "title": r.Title, "up": r.UP,
			"first_seen": r.FirstSeen, "last_seen": r.LastSeen, "seen_count": r.SeenCount,
			"subtitle_files": r.SubtitleFiles,
		})
	}
	return map[string]interface{}{"content": b.String(), "count": len(recs), "videos": out}, nil
}

func (p *Plugin) handleSeenForget(args map[string]interface{}) (interface{}, error) {
	ref, err := parseRef(argStr(args, "video"))
	if err != nil {
		return nil, err
	}
	ok, err := p.st.remove(ref)
	if err != nil {
		return nil, err
	}
	if !ok {
		return map[string]interface{}{
			"content": fmt.Sprintf("台账里没有 %s，未做任何修改。", ref),
			"removed": false,
		}, nil
	}
	return map[string]interface{}{
		"content": fmt.Sprintf("已从台账删除 %s。", ref),
		"removed": true,
	}, nil
}

// ---- handler：发评论（默认关闭） ----

func (p *Plugin) handleCommentPost(args map[string]interface{}) (interface{}, error) {
	p.refresh()
	cfg := p.snapshot()

	// 闸门 1：配置开关。**先判这个**，即使 Cookie 齐全也不放行 ——
	// 让「默认关闭」是真的关闭，而不是靠缺 Cookie 偶然拦住。
	if !cfg.allowPost {
		return nil, fmt.Errorf("发评论已被禁用：配置项 %s 为 false（默认值）。"+
			"这是本示例有意的设计——写操作必须由使用者显式开启；开启后还需要 Cookie 里含 bili_jct 才能通过 csrf 校验", cfgAllowPost)
	}
	// 闸门 2：csrf token。没有它平台必回 -111，与其转发平台错误不如本地说清。
	csrf := cookieValue(cfg.cookie, "bili_jct")
	if csrf == "" {
		return nil, fmt.Errorf("已开启 %s，但 Cookie 里没有 bili_jct（csrf token）：平台会以 -111 拒绝。"+
			"请在配置的 Cookie 里带上登录后的 bili_jct", cfgAllowPost)
	}

	ref, err := parseRef(argStr(args, "video"))
	if err != nil {
		return nil, err
	}
	msg := strings.TrimSpace(argStr(args, "message"))
	if msg == "" {
		return nil, fmt.Errorf("message 不能为空")
	}
	if n := len([]rune(msg)); n > maxCommentRunes {
		return nil, fmt.Errorf("message 过长（%d 字）：上限 %d 字", n, maxCommentRunes)
	}
	if ref.AID <= 0 {
		return nil, fmt.Errorf("无法从 %q 得到 av 号（评论接口只接受 aid）", argStr(args, "video"))
	}

	root := int64(argInt(args, "root", 0))
	parent := int64(argInt(args, "parent", 0))
	if parent == 0 {
		parent = root
	}

	ctx, cancel := ctxWith(context.Background(), budgetSingle)
	defer cancel()

	rpid, err := p.apiClient().postComment(ctx, ref, msg, csrf, root, parent)
	if err != nil {
		return nil, err
	}
	kind := "评论"
	if root > 0 {
		kind = fmt.Sprintf("对 rpid=%d 的回复", root)
	}
	return map[string]interface{}{
		"content": fmt.Sprintf("已发布%s（rpid=%d）。\n注意：平台有审核与频率限制，刚发出可能暂时不可见。", kind, rpid),
		"rpid":    rpid,
		"video":   ref.String(),
	}, nil
}

// ---- 小工具 ----

// obj 拼 JSON Schema。
//
// required 显式列出参数名而不是在属性里打标记：后者要靠修改共享的 map
// 传递状态（初版就是那么写的），一旦有人复用同一个属性 map 就会互相污染。
func obj(props map[string]interface{}, required ...string) map[string]interface{} {
	m := map[string]interface{}{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		// 排序让 schema 稳定（map 迭代序随机会让工具清单每次都变）。
		sort.Strings(required)
		m["required"] = required
	}
	return m
}

func str(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}

func num(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "integer", "description": desc}
}

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

func hhmmss(sec int64) string {
	if sec <= 0 {
		return "0:00"
	}
	h := sec / 3600
	m := (sec % 3600) / 60
	s := sec % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func sortLabel(sortBy int) string {
	if sortBy == 0 {
		return "时间"
	}
	return "热度"
}

func pagesToAny(pages []apiPage) []interface{} {
	out := make([]interface{}, 0, len(pages))
	for _, pg := range pages {
		out = append(out, map[string]interface{}{
			"page": pg.Page, "cid": pg.CID, "part": pg.Part, "duration": pg.Duration,
		})
	}
	return out
}

func tracksToAny(subs []apiSubtitleItem) []interface{} {
	out := make([]interface{}, 0, len(subs))
	for _, s := range subs {
		out = append(out, map[string]interface{}{
			"lan": s.Lan, "lan_doc": s.LanDoc, "is_ai": s.IsAI,
		})
	}
	return out
}

func lanList(subs []apiSubtitleItem) []string {
	out := make([]string, 0, len(subs))
	for _, s := range subs {
		out = append(out, s.Lan)
	}
	return out
}

func pickSubtitle(subs []apiSubtitleItem, want string) *apiSubtitleItem {
	for i := range subs {
		if strings.EqualFold(subs[i].Lan, want) {
			return &subs[i]
		}
	}
	// "zh" 这类前缀写法也接受：用户往往只记得语言名，不记得平台的确切代码。
	for i := range subs {
		if strings.HasPrefix(strings.ToLower(subs[i].Lan), strings.ToLower(want)) {
			return &subs[i]
		}
	}
	return nil
}
