package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

type Plugin struct {
	name string
	sdk  *sdk.PluginSDK
	cli  *http.Client
}

type searchResp struct {
	Result *struct {
		Songs     []songItem `json:"songs"`
		SongCount int        `json:"songCount"`
	} `json:"result"`
	Code int `json:"code"`
}

type songItem struct {
	ID       int64     `json:"id"`
	Name     string    `json:"name"`
	Artists  []artist  `json:"artists"`
	Album    albumInfo `json:"album"`
	Duration int       `json:"duration"`
	Mvid     int       `json:"mvid"`
	Fee      int       `json:"fee"`
}

type artist struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type albumInfo struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type lyricResp struct {
	Lrc  *lyricData `json:"lrc"`
	TLrc *lyricData `json:"tlyric"`
	Code int        `json:"code"`
}

type lyricData struct {
	Lyric string `json:"lyric"`
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s
	p.cli = &http.Client{Timeout: 15 * time.Second}

	s.RegisterTool(p.name+"_search", sdk.ToolDef{
		Name:        p.name + "_search",
		Description: "搜索歌曲，通过关键词查找音乐，返回歌曲列表",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"keyword": map[string]interface{}{
					"type":        "string",
					"description": "搜索关键词，如歌曲名、歌手名",
				},
				"limit": map[string]interface{}{
					"type":        "integer",
					"description": "返回结果数量（1-50），默认10",
				},
			},
			"required": []string{"keyword"},
		},
		Cleaner: func(output string) string {
			var r struct{ Content string }
			if json.Unmarshal([]byte(output), &r) == nil && r.Content != "" {
				return r.Content
			}
			return output
		},
	}, p.handleSearch)

	s.RegisterTool(p.name+"_lyrics", sdk.ToolDef{
		Name:        p.name + "_lyrics",
		Description: "获取歌曲歌词，通过歌曲ID查看歌词内容",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"song_id": map[string]interface{}{
					"type":        "integer",
					"description": "歌曲ID（从搜索结果的 id 字段获取）",
				},
			},
			"required": []string{"song_id"},
		},
	}, p.handleLyrics)

	return nil
}

func (p *Plugin) Stop() error { return nil }

func (p *Plugin) neRequest(path string, params map[string]string) ([]byte, error) {
	base := "https://music.163.com/api" + path
	reqURL := base + "?" + urlValues(params).Encode()
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Referer", "https://music.163.com/")
	resp, err := p.cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func urlValues(m map[string]string) url.Values {
	v := url.Values{}
	for k, val := range m {
		v.Set(k, val)
	}
	return v
}

func (p *Plugin) handleSearch(args map[string]interface{}) (interface{}, error) {
	keyword, _ := args["keyword"].(string)
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return map[string]interface{}{
			"content": "请输入搜索关键词",
			"isError": true,
		}, nil
	}

	limit := 10
	if v, ok := args["limit"].(float64); ok {
		limit = int(v)
		if limit < 1 {
			limit = 1
		}
		if limit > 50 {
			limit = 50
		}
	}

	body, err := p.neRequest("/search/get", map[string]string{
		"s":     keyword,
		"type":  "1",
		"limit": fmt.Sprint(limit),
	})
	if err != nil {
		return map[string]interface{}{
			"content": fmt.Sprintf("搜索失败：%v", err),
			"isError": true,
		}, nil
	}

	var resp searchResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return map[string]interface{}{
			"content": fmt.Sprintf("解析响应失败：%v", err),
			"isError": true,
		}, nil
	}

	if resp.Code != 200 || resp.Result == nil {
		return map[string]interface{}{
			"content": fmt.Sprintf("搜索失败，响应码：%d", resp.Code),
			"isError": true,
		}, nil
	}

	songs := resp.Result.Songs
	if len(songs) == 0 {
		return map[string]interface{}{
			"content": fmt.Sprintf("未找到与「%s」相关的歌曲", keyword),
		}, nil
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("找到 %d 首与「%s」相关的歌曲：\n", resp.Result.SongCount, keyword))
	for i, s := range songs {
		var artists []string
		for _, a := range s.Artists {
			artists = append(artists, a.Name)
		}
		dur := time.Duration(s.Duration) * time.Millisecond
		minutes := int(dur.Minutes())
		seconds := int(dur.Seconds()) % 60
		lines = append(lines, fmt.Sprintf("%d. %s - %s [%02d:%02d] (ID: %d)",
			i+1, s.Name, strings.Join(artists, "/"), minutes, seconds, s.ID))
	}

	type songResult struct {
		ID       int64    `json:"id"`
		Name     string   `json:"name"`
		Artists  []string `json:"artists"`
		Album    string   `json:"album"`
		Duration int      `json:"duration"`
	}

	var results []songResult
	for _, s := range songs {
		var artists []string
		for _, a := range s.Artists {
			artists = append(artists, a.Name)
		}
		results = append(results, songResult{
			ID:       s.ID,
			Name:     s.Name,
			Artists:  artists,
			Album:    s.Album.Name,
			Duration: s.Duration,
		})
	}

	return map[string]interface{}{
		"content": strings.Join(lines, "\n"),
		"songs":   results,
		"total":   resp.Result.SongCount,
	}, nil
}

func (p *Plugin) handleLyrics(args map[string]interface{}) (interface{}, error) {
	songID, ok := args["song_id"].(float64)
	if !ok {
		return map[string]interface{}{
			"content": "请提供有效的歌曲ID",
			"isError": true,
		}, nil
	}

	id := int64(songID)
	body, err := p.neRequest("/song/lyric", map[string]string{
		"id": fmt.Sprint(id),
		"lv": "-1",
		"kv": "-1",
		"tv": "-1",
	})
	if err != nil {
		return map[string]interface{}{
			"content": fmt.Sprintf("获取歌词失败：%v", err),
			"isError": true,
		}, nil
	}

	var resp lyricResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return map[string]interface{}{
			"content": fmt.Sprintf("解析歌词失败：%v", err),
			"isError": true,
		}, nil
	}

	if resp.Code != 200 {
		return map[string]interface{}{
			"content": fmt.Sprintf("获取歌词失败，响应码：%d", resp.Code),
			"isError": true,
		}, nil
	}

	lyric := ""
	if resp.Lrc != nil {
		lyric = resp.Lrc.Lyric
	}

	if lyric == "" {
		return map[string]interface{}{
			"content": fmt.Sprintf("歌曲 %d 暂无歌词", id),
		}, nil
	}

	// Clean up lyrics metadata lines and limit length
	lyric = cleanLyrics(lyric)
	if len(lyric) > 3000 {
		lyric = lyric[:3000] + "\n...（歌词过长已截断）"
	}

	tLyric := ""
	if resp.TLrc != nil && resp.TLrc.Lyric != "" {
		tLyric = cleanLyrics(resp.TLrc.Lyric)
		if len(tLyric) > 1000 {
			tLyric = tLyric[:1000] + "\n...（翻译过长已截断）"
		}
	}

	result := fmt.Sprintf("歌词：\n%s", lyric)
	if tLyric != "" {
		result += fmt.Sprintf("\n翻译：\n%s", tLyric)
	}

	return map[string]interface{}{
		"content": result,
		"lyric":   lyric,
		"tlyric":  tLyric,
	}, nil
}

func cleanLyrics(l string) string {
	lines := strings.Split(l, "\n")
	var cleaned []string
	for _, line := range lines {
		// Skip metadata lines like [ti:...], [ar:...], [al:...], [by:...], [offset:...]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		cleaned = append(cleaned, line)
	}
	return strings.Join(cleaned, "\n")
}
