package main

// 零散的小工具：路径解析、文件写出、Cookie 取值、文本渲染。
//
// 单独一个文件是为了让 tools.go 只留"注册 + handler"，
// 这些与业务无关的转换不必混在里面。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveDir 决定导出目录：显式参数优先，且同样要过安全校验。
//
// 为什么参数也要校验：工具参数是**模型填的**，而模型会照抄用户随口说的
// "/etc/xxx"。配置项与参数走同一套判据，才不会留下一个更宽的旁路。
func (p *Plugin) resolveDir(sub string, cfg pluginConfig) (string, error) {
	base := cfg.outputDir
	sub = strings.TrimSpace(sub)
	if sub == "" {
		// 配置项在 Start 时校验过，但用户可能在运行期改成系统目录
		//（reload 每次调用都会重读），所以写入前再校验一次。
		return validateOutputDir(base)
	}
	if filepath.IsAbs(sub) {
		return validateOutputDir(sub)
	}
	// 相对路径一律拼到输出目录下，并**再次规范化**：
	// "../../etc" 拼出来会跳出输出目录，Clean 之后会被下面的校验拦下。
	joined := filepath.Clean(filepath.Join(base, sub))
	if !strings.HasPrefix(joined+string(filepath.Separator), filepath.Clean(base)+string(filepath.Separator)) {
		return "", fmt.Errorf("save_dir %q 越出了输出目录 %s（不允许 .. 跳出去）", sub, base)
	}
	return validateOutputDir(joined)
}

// sanitizeFilename 把平台给的语言代码变成安全文件名。
//
// 语言代码由**平台响应**提供，不是我们自己写的常量 —— 直接拿它拼路径
// 意味着一个恶意/异常响应就能写到任意位置（"../" 之类）。
func sanitizeFilename(s string) string {
	if s == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// subtitleDoc 是导出文件的结构：既保留平台的原始时间轴，也带上下文，
// 便于事后不用再查一次接口就知道这段字幕属于哪个视频的哪个分P。
type subtitleDoc struct {
	BVID   string    `json:"bvid"`
	AID    int64     `json:"aid"`
	CID    int64     `json:"cid"`
	Lan    string    `json:"lan"`
	LanDoc string    `json:"lan_doc"`
	IsAI   bool      `json:"is_ai"`
	Source string    `json:"source_url"`
	Lines  int       `json:"line_count"`
	Body   []subLine `json:"body"`
}

type subLine struct {
	From    float64 `json:"from"`
	To      float64 `json:"to"`
	Content string  `json:"content"`
}

func renderSubtitleJSON(ref videoRef, sub *apiSubtitleItem, body *apiSubtitleBody) string {
	doc := subtitleDoc{
		BVID:   ref.BVID,
		AID:    ref.AID,
		Lan:    sub.Lan,
		LanDoc: sub.LanDoc,
		IsAI:   sub.IsAI,
		Source: sub.SubURL,
		Lines:  len(body.Body),
	}
	for _, l := range body.Body {
		doc.Body = append(doc.Body, subLine{From: l.From, To: l.To, Content: l.Content})
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		// 这个结构体全是基本类型，Marshal 不会失败；留一句兜底而不是 panic。
		return "{}"
	}
	return string(raw)
}

// subtitlePreview 渲染成带时间戳的行，方便模型直接引用"第几分第几秒说了什么"。
func subtitlePreview(body *apiSubtitleBody, limit int) string {
	var b strings.Builder
	for i, l := range body.Body {
		if limit > 0 && i >= limit {
			fmt.Fprintf(&b, "…（共 %d 条，完整内容见导出文件）\n", len(body.Body))
			break
		}
		fmt.Fprintf(&b, "  [%s] %s\n", ts(l.From), strings.TrimSpace(l.Content))
	}
	return b.String()
}

func ts(sec float64) string {
	total := int(sec)
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// cookieValue 从 Cookie 串里取指定键。
//
// 只做最朴素的解析：Cookie 串是用户自己粘进来的（"SESSDATA=…; bili_jct=…"），
// 用 net/http 的 Cookie 解析器反而会把不规范的部分整段丢掉。
func cookieValue(cookie, key string) string {
	for _, part := range strings.Split(cookie, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(k), key) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// removeDirIfExists 只删我们自己的目录；不存在不算错误。
func removeDirIfExists(dir string) error {
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
