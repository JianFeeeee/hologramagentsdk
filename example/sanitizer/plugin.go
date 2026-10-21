// Package main 是一个外部插件示例（编译为 .so 通过 -buildmode=plugin）。
// 目标：在 Agent 全链路清洗文本，防止乱码（坏 UTF-8 / U+FFFD / ANSI 转义）污染上下文并被 LLM 复读，
// 同时保留原有"工具调用残留（思维泄漏）"清理。
//
// 挂载阶段：
//   - StageOnInput        : 清洗用户输入（RawMessage）
//   - StageAfterToolcall  : 清洗工具执行结果（ToolResults），坏字节不进 LLM 上下文
//   - StagePostAction     : 清洗 LLM 输出（LLMText），保留原有思维泄漏清理
//
// 依赖 ABI v2 的 stage 写回能力：插件对 StageContext 的修改会同步回内核。
//
// 编译：
//
//	go build -buildmode=plugin -o sanitizer.so .
//
// 安装到 HologramAgent 插件目录（如 plugins/sanitizer/plugin.so），
// HologramAgent 自动通过 tryLoadSO 加载。
package main

import (
	"log"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/JianFeeeee/hologramagentsdk/sdk"
)

var (
	toolCallTagRE     = regexp.MustCompile(`(?s)<tool_call[^>]*>.*?</tool_call>`)
	invokeTagRE       = regexp.MustCompile(`(?s)<invoke[^>]*>.*?</invoke>`)
	toolTagRE         = regexp.MustCompile(`(?s)<tool[^>]*>.*?</tool>`)
	functionTagRE     = regexp.MustCompile(`(?s)<function[^>]*>.*?</function>`)
	toolCodeBlockRE   = regexp.MustCompile("(?s)```(?:xml|json)?\\s*<tool_call[^>]*>.*?</tool_call>\\s*```")
	invokeCodeBlockRE = regexp.MustCompile("(?s)```(?:xml|json)?\\s*<invoke[^>]*>.*?</invoke>\\s*```")
	toolCodeBlockRE2  = regexp.MustCompile("(?s)```(?:xml|json)?\\s*<tool[^>]*>.*?</tool>\\s*```")
	chineseMarkerRE   = regexp.MustCompile(`(?s)【tool_call】.*?【/tool_call】`)
	multiNewlineRE    = regexp.MustCompile(`\n{3,}`)
	toolNameRE        = regexp.MustCompile(`^(cmd_run|terminal_create|terminal_write|memory_|knowledge_|doc_|social_|output_set_channel|output_send|llm_|plgreload|spawn_child|child_result|describe_image|transcribe_audio|ocr_image|timer_set|plugin_install|plugin_remove|qq_|a2a_|mcp_|healthcheck|files_|web_)`)
	placeholderRE     = regexp.MustCompile(`(?i)\{\{\s*tool\s*[:：][^}]*\}\}`)
	atToolRE          = regexp.MustCompile(`(?i)^@\s*tool\b`)
)

type Plugin struct{}

func (p *Plugin) Name() string { return "sanitizer" }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)

	// 1) 输入清洗
	s.RegisterStage(sdk.StageOnInput, func(ctx *sdk.StageContext) error {
		ctx.Lock()
		before := ctx.RawMessage
		ctx.RawMessage = cleanText(ctx.RawMessage)
		if before != ctx.RawMessage {
			log.Printf("[sanitizer] StageOnInput: cleaned %d bytes", len(before)-len(ctx.RawMessage))
		}
		ctx.Unlock()
		return nil
	})

	// 2) 工具结果清洗（坏字节/ANSI 不得进 LLM 上下文）
	s.RegisterStage(sdk.StageAfterToolcall, func(ctx *sdk.StageContext) error {
		ctx.Lock()
		defer ctx.Unlock()
		for i, tr := range ctx.ToolResults {
			if s, ok := tr.Result.(string); ok {
				clean := cleanText(s)
				if clean != s {
					ctx.ToolResults[i].Result = clean
					log.Printf("[sanitizer] StageAfterToolcall: tool=%s cleaned %d bytes", tr.Name, len(s)-len(clean))
				}
			}
		}
		return nil
	})

	// 3) LLM 输出清洗（保留原有思维泄漏清理 + 新增乱码清洗）
	s.RegisterStage(sdk.StagePostAction, func(ctx *sdk.StageContext) error {
		ctx.Lock()
		before := len(ctx.LLMText)
		ctx.LLMText = cleanToolCallLeakage(ctx.LLMText)
		ctx.LLMText = cleanText(ctx.LLMText)
		after := len(ctx.LLMText)
		ctx.Unlock()
		if before != after {
			log.Printf("[sanitizer] cleaned %d bytes (before=%d after=%d)", before-after, before, after)
		}
		return nil
	})
	log.Printf("[sanitizer] stage OnInput/AfterToolcall/PostAction registered")
	return nil
}

func (p *Plugin) Stop() error { return nil }

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{}, nil
}

// cleanToolCallLeakage 清洗 LLM 输出中的工具调用残留（思维泄漏）。
func cleanToolCallLeakage(content string) string {
	if content == "" {
		return content
	}

	before := len(content)

	content = toolCodeBlockRE.ReplaceAllString(content, "")
	content = invokeCodeBlockRE.ReplaceAllString(content, "")
	content = toolCodeBlockRE2.ReplaceAllString(content, "")

	content = toolCallTagRE.ReplaceAllString(content, "")
	content = invokeTagRE.ReplaceAllString(content, "")
	content = toolTagRE.ReplaceAllString(content, "")
	content = functionTagRE.ReplaceAllString(content, "")

	content = chineseMarkerRE.ReplaceAllString(content, "")

	lines := strings.Split(content, "\n")
	var cleaned []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			cleaned = append(cleaned, line)
			continue
		}
		if placeholderRE.MatchString(trimmed) || atToolRE.MatchString(trimmed) {
			continue
		}
		if m := toolNameRE.FindStringIndex(trimmed); m != nil {
			rest := trimmed[m[1]:]
			if strings.HasPrefix(rest, "(") && strings.Contains(rest, ")") {
				continue
			}
		}
		cleaned = append(cleaned, line)
	}
	content = strings.Join(cleaned, "\n")

	content = multiNewlineRE.ReplaceAllString(content, "\n\n")
	content = strings.TrimSpace(content)

	if len(content) != before {
		log.Printf("[sanitizer] cleanToolCallLeakage: %d bytes removed", before-len(content))
	}
	return content
}

// cleanText 清洗可能污染 LLM 上下文/输出的文本：
//  1. 剥离 ANSI 转义序列（\x1b[...m 等，源自终端输出）
//  2. 剔除无效 UTF-8 字节（strings.ToValidUTF8 语义）与已解码的 U+FFFD 替换符，
//     避免模型复读坏字节/替换符造成乱码（把坏段落整体丢弃比留残字更干净）
func cleanText(s string) string {
	if s == "" {
		return s
	}
	// 先剥离 ANSI 转义：ESC [ 参数 m / ESC ] 标题 / 其他 CSI 序列
	if strings.ContainsRune(s, 0x1b) {
		var sb strings.Builder
		sb.Grow(len(s))
		i := 0
		for i < len(s) {
			c := s[i]
			if c == 0x1b {
				// 跳过完整转义序列
				j := i + 1
				if j < len(s) {
					switch s[j] {
					case '[': // CSI: ESC [ <params> <letter>
						j++
						for j < len(s) && !(s[j] >= 0x40 && s[j] <= 0x7e) {
							j++
						}
						if j < len(s) {
							j++
						}
						i = j
						continue
					case ']': // OSC: ESC ] ... BEL / ST
						i = j + 1
						for i < len(s) && s[i] != 0x07 {
							i++
						}
						i++ // skip BEL
						continue
					default: // 单字符转义（ESC c ESC 7 等）
						i = j + 1
						continue
					}
				}
				i++
				continue
			}
			sb.WriteByte(c)
			i++
		}
		s = sb.String()
	}

	// 剔除无效 UTF-8 与 U+FFFD 替换符
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	if strings.ContainsRune(s, utf8.RuneError) {
		// 连 U+FFFD 也不留给模型复述
		var b strings.Builder
		b.Grow(len(s))
		for _, r := range s {
			if r != utf8.RuneError {
				b.WriteRune(r)
			}
		}
		s = b.String()
	}
	return s
}