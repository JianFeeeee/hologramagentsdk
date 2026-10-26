package main

// 肥鱼声库（feiyu voice）的 **HTTP 客户端插件**。
//
// ## 为什么只是客户端
//
// 声库本体（肥鱼 TTS / 肥鱼 RVC / SoulX-Singer SVS）是独立的推理服务：
// FastAPI + torch，需要模型权重。把它塞进插件意味着
//   · 内核要背几个 GB 的模型与 torch 依赖
//   · 换声线/换模型要重编插件
//   · 一台机器只能跑一个实例
//
// 做成客户端后：服务在哪台机器都能接，插件只负责"把请求送过去、
// 把产出的音频取回来"，然后把文件路径交给 `output_send__qq`。
//
// ## 工具面
//
//	feiyu_say(text, style)        文字 → 语音，返回本地音频文件路径
//	feiyu_sing(score, lyrics, …)  按谱唱（MIDI/ABC），返回本地音频文件路径
//	feiyu_convert(audio, …)       换声（RVC），返回本地音频文件路径
//	feiyu_job(job_id)             查任务状态（长任务用）
//
// ## 失败语义
//
// 服务不可达时**明确报"服务不可达 + 配哪个配置项"**，不静默降级成文本。
// 用户听到的应该是"语音没发出来"，而不是"她突然只打字不说话"。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

type Plugin struct {
	name string
	sdk  *sdk.PluginSDK
	cli  *http.Client

	baseURL string
	apiKey  string
	style   string
	outDir  string
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	p.sdk = s
	p.cli = &http.Client{Timeout: 300 * time.Second}
	s.SetAutoRestart(true)

	s.Settings().RegisterDef(sdk.ConfigDef{Key: "base_url", Default: "http://127.0.0.1:8765", Type: "string",
		DisplayName: "肥鱼声库服务地址", Description: "形如 http://host:port（服务端 uvicorn 监听地址）", Category: "feiyu_voice"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "api_key", Default: "", Type: "string",
		DisplayName: "服务 API Key", Description: "服务端 Bearer 令牌，留空表示服务未启用鉴权", Category: "feiyu_voice"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "style", Default: "cute", Type: "string",
		DisplayName: "默认声线", Description: "cute（可爱，默认）/ gentle（温柔）", Category: "feiyu_voice"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "out_dir", Default: "", Type: "string",
		DisplayName: "音频输出目录", Description: "留空则用系统临时目录；QQ 插件仍需可读该目录", Category: "feiyu_voice"})

	p.baseURL = strings.TrimRight(settingStr(s, "base_url", "http://127.0.0.1:8765"), "/")
	p.apiKey = settingStr(s, "api_key", "")
	p.style = settingStr(s, "style", "cute")
	p.outDir = settingStr(s, "out_dir", "")
	if p.outDir == "" {
		p.outDir = os.TempDir()
	}
	os.MkdirAll(p.outDir, 0755)

	s.RegisterTool(p.name+"_say", sdk.ToolDef{
		Name: p.name + "_say",
		Description: "用肥鱼声线把文字说出来，返回**本地音频文件路径**。" +
			"拿到路径后：发 QQ 语音用 output_send__qq type=voice（payload=该路径），" +
			"要发成文件用 type=file。只用于真的想开口说话的场景（安慰、打招呼、读一段），" +
			"普通聊天别用 —— 打字更快。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"text":  map[string]interface{}{"type": "string", "description": "要念的内容"},
				"style": map[string]interface{}{"type": "string", "description": "cute / gentle，留空用默认"},
			},
			"required": []string{"text"},
		},
	}, p.handleSay)

	s.RegisterTool(p.name+"_sing", sdk.ToolDef{
		Name: p.name + "_sing",
		Description: "按谱把歌词唱出来（SVS + 换声），返回**本地音频文件路径**。" +
			"需要乐谱文件（MIDI 或 ABC）与歌词；通常用 output_send__qq type=file 发送（语音条有长度限制）。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"score":   map[string]interface{}{"type": "string", "description": "乐谱文件路径（.mid / .abc），或已上传的 asset_id"},
				"lyrics":  map[string]interface{}{"type": "string", "description": "歌词；留空表示谱里自带"},
				"bpm":     map[string]interface{}{"type": "number", "description": "可选：节拍（ABC 谱常用）"},
				"track":   map[string]interface{}{"type": "number", "description": "可选：多轨 MIDI 选轨"},
				"segment": map[string]interface{}{"type": "number", "description": "可选：分段秒数 5–25"},
			},
			"required": []string{"score"},
		},
	}, p.handleSing)

	s.RegisterTool(p.name+"_convert", sdk.ToolDef{
		Name: p.name + "_convert",
		Description: "把一段干净人声换成肥鱼音色（RVC），返回**本地音频文件路径**。" +
			"适合：用户发来的清唱、已有配音素材。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"audio": map[string]interface{}{"type": "string", "description": "源音频文件路径（WAV/FLAC）"},
			},
			"required": []string{"audio"},
		},
	}, p.handleConvert)

	s.RegisterTool(p.name+"_job", sdk.ToolDef{
		Name:        p.name + "_job",
		Description: "查询声库任务状态（排队/运行/成功/失败）。长任务可用它轮询。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"job_id": map[string]interface{}{"type": "string", "description": "提交时返回的任务 id"},
			},
			"required": []string{"job_id"},
		},
	}, p.handleJob)

	return nil
}

func (p *Plugin) Stop() error { return nil }

// ───────────────────────── 工具实现 ─────────────────────────

func (p *Plugin) handleSay(args map[string]interface{}) (interface{}, error) {
	text, _ := args["text"].(string)
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("text 不能为空")
	}
	style, _ := args["style"].(string)
	if style == "" {
		style = p.style
	}
	job, err := p.submit("speech", map[string]interface{}{"text": text, "style": style})
	if err != nil {
		return nil, err
	}
	return p.awaitAudio(job, "say")
}

func (p *Plugin) handleSing(args map[string]interface{}) (interface{}, error) {
	score, _ := args["score"].(string)
	if strings.TrimSpace(score) == "" {
		return nil, fmt.Errorf("score 不能为空（MIDI/ABC 路径或 asset_id）")
	}
	scoreID := score
	if !isAssetID(score) {
		id, err := p.upload(score)
		if err != nil {
			return nil, err
		}
		scoreID = id
	}
	payload := map[string]interface{}{"score_id": scoreID}
	if v, _ := args["lyrics"].(string); strings.TrimSpace(v) != "" {
		payload["lyrics"] = v
	}
	if v, ok := args["bpm"].(float64); ok && v > 0 {
		payload["bpm"] = v
	}
	if v, ok := args["track"].(float64); ok && v >= 0 {
		payload["track"] = int(v)
	}
	if v, ok := args["segment"].(float64); ok && v > 0 {
		payload["segment_seconds"] = v
	}
	job, err := p.submit("singing", payload)
	if err != nil {
		return nil, err
	}
	return p.awaitAudio(job, "sing")
}

func (p *Plugin) handleConvert(args map[string]interface{}) (interface{}, error) {
	src, _ := args["audio"].(string)
	if strings.TrimSpace(src) == "" {
		return nil, fmt.Errorf("audio 不能为空")
	}
	id, err := p.upload(src)
	if err != nil {
		return nil, err
	}
	job, err := p.submit("voice-conversion", map[string]interface{}{"source_id": id})
	if err != nil {
		return nil, err
	}
	return p.awaitAudio(job, "convert")
}

func (p *Plugin) handleJob(args map[string]interface{}) (interface{}, error) {
	id, _ := args["job_id"].(string)
	if id == "" {
		return nil, fmt.Errorf("job_id 不能为空")
	}
	body, err := p.do("GET", "/v1/jobs/"+id, nil, "")
	if err != nil {
		return nil, err
	}
	return string(body), nil
}

// ───────────────────────── HTTP 层 ─────────────────────────

// submit 提交一个任务，返回 job_id。
func (p *Plugin) submit(kind string, payload map[string]interface{}) (string, error) {
	raw, _ := json.Marshal(payload)
	body, err := p.do("POST", "/v1/"+kind, bytes.NewReader(raw), "application/json")
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &out)
	if out.ID == "" {
		// 兼容 job_id 命名
		var alt struct {
			JobID string `json:"job_id"`
		}
		json.Unmarshal(body, &alt)
		out.ID = alt.JobID
	}
	if out.ID == "" {
		return "", fmt.Errorf("声库未返回任务 id: %s", truncate(string(body), 200))
	}
	return out.ID, nil
}

// upload 上传素材（MIDI / 音频），返回 asset_id。
func (p *Plugin) upload(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("打开素材失败: %w", err)
	}
	defer f.Close()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", err
	}
	mw.Close()
	body, err := p.do("POST", "/v1/assets", &buf, mw.FormDataContentType())
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &out)
	if out.ID == "" {
		return "", fmt.Errorf("上传素材未返回 id: %s", truncate(string(body), 200))
	}
	return out.ID, nil
}

// awaitAudio 轮询任务直到成功，然后下载音频到本地，返回文件路径。
func (p *Plugin) awaitAudio(jobID, tag string) (string, error) {
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		body, err := p.do("GET", "/v1/jobs/"+jobID, nil, "")
		if err != nil {
			return "", err
		}
		var st struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		json.Unmarshal(body, &st)
		switch st.Status {
		case "success":
			return p.download(jobID, tag)
		case "failed", "error":
			return "", fmt.Errorf("声库任务失败: %s", st.Error)
		}
		time.Sleep(1500 * time.Millisecond)
	}
	return "", fmt.Errorf("声库任务超时（job=%s）", jobID)
}

// download 下载任务产物到本地。
func (p *Plugin) download(jobID, tag string) (string, error) {
	req, err := http.NewRequest("GET", p.baseURL+"/v1/jobs/"+jobID+"/audio", nil)
	if err != nil {
		return "", err
	}
	p.auth(req)
	res, err := p.cli.Do(req)
	if err != nil {
		return "", fmt.Errorf("声库音频下载失败: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 200))
		return "", fmt.Errorf("声库返回 %d: %s", res.StatusCode, truncate(string(b), 160))
	}
	dst := filepath.Join(p.outDir, fmt.Sprintf("feiyu_%s_%d.wav", tag, time.Now().UnixNano()))
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, res.Body); err != nil {
		return "", err
	}
	return dst, nil
}

// do 发起一次请求；服务不可达时给出**可执行的**报错（指到配置项）。
func (p *Plugin) do(method, path string, body io.Reader, contentType string) ([]byte, error) {
	req, err := http.NewRequest(method, p.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	p.auth(req)
	res, err := p.cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("肥鱼声库不可达（%s）：配 feiyu_voice.base_url 指向服务地址；"+
			"服务未装时这里必然失败，不是模型的问题。原始错误: %w", p.baseURL, err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("肥鱼声库返回 %d: %s", res.StatusCode, truncate(string(data), 240))
	}
	return data, nil
}

func (p *Plugin) auth(req *http.Request) {
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
}

// ───────────────────────── 小工具 ─────────────────────────

func settingStr(s *sdk.PluginSDK, key, fallback string) string {
	v, err := s.Settings().Get(key)
	if err != nil || v == nil {
		return fallback
	}
	if str, ok := v.(string); ok && strings.TrimSpace(str) != "" {
		return strings.TrimSpace(str)
	}
	return fallback
}

// isAssetID 判断参数是否已是服务端的 asset_id（32 位十六进制）。
func isAssetID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
