package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/JianFeeeee/hologramagentsdk/sdk"
)

// editDocPy 内嵌 Python 后端脚本。
//
// 为何必须内嵌：hmapdev 的打包器只把 plugin.json + plugin.bin 放进 .hmap
// （加上可选的 README/LICENSE），**不打包任何资源文件**。
// 而本插件的全部业务逻辑就在 edit_doc.py 里，若让它单独存在于
// plugin_data/editdoc/，就会出现「Go 侧升级了、脚本忘了换」的版本漂移
// （mc 插件就踩过这个坑：工具注册了但子脚本还是旧的，报未知命令）。
// 内嵌 + 内容指纹校验可以让两侧永远同版本。
//
//go:embed embed/edit_doc.py
var embeddedScript []byte

// scriptStamp 返回内嵌脚本的内容指纹。
func scriptStamp() string {
	h := sha256.Sum256(embeddedScript)
	return hex.EncodeToString(h[:])[:16]
}

// scriptProtocolMarker 是 edit_doc.py 里的 v2 协议标记（见 embed/edit_doc.py 顶部）。
// resolveScript 靠它识别「script_path 指向的是不是 v2 脚本」，防止旧脚本被误用。
const scriptProtocolMarker = `EDITDOC_PROTOCOL = "v2"`

// Plugin 是 editdoc v2.0.0（全能办公）插件。
//
// 设计要点（与 v1.0.0 的关系）：
//   - edit_document 工具名与 file/operation/target/replacement/sheet/row/col/value
//     参数语义**完全保留**，老调用不会破。
//   - 全部实现委托给同目录下的 edit_doc.py，Go 侧只做参数编排 + 结构化返回。
//   - 所有子命令统一走 runScript，错误一律结构化（ok:false + error + hint），不 panic。
type Plugin struct {
	name       string
	sdk        *sdk.PluginSDK
	scriptPath string
	venvPython string
	timeoutSec int

	// ignoredScriptPath 记录「配置里的 script_path 因不是 v2 脚本而被忽略」的路径。
	// 非空时 runScript 会在返回体里带上 script_path_ignored，让上层看得见这次降级。
	ignoredScriptPath string
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s

	// ---- 配置项（v1 的 script_path / venv_python 原样保留，新增 timeout_sec）----
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "script_path", Default: "", Type: "string",
		DisplayName: "办公脚本路径",
		Description: "edit_doc.py 的绝对路径；留空（推荐）时使用 DataDir()/runtime/edit_doc.py（升级时按指纹自动更新）。" +
			"仅当该文件含 v2 协议标记时才会被采用，否则自动忽略并回退内嵌脚本",
		Category: p.name,
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "venv_python", Default: "", Type: "string",
		DisplayName: "venv Python 解释器",
		Description: "执行 edit_doc.py 使用的 Python 解释器（需装有 python-docx/openpyxl/python-pptx）；必须配置",
		Category:    p.name,
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "timeout_sec", Default: 60, Type: "int",
		DisplayName: "脚本超时（秒）",
		Description: "单次调用 edit_doc.py 的超时上限；大文件或格式转换（soffice 首次启动慢）建议调到 180",
		Category:    p.name, Min: 5, Max: 900,
	})

	p.reloadSettings()

	// ---- 工具注册 ----
	// 工具名不带插件名前缀：v1 的 edit_document 就是无前缀的，保持一致性。

	s.RegisterTool("read_document", sdk.ToolDef{
		Name: "read_document",
		Description: "读取办公文档内容并返回结构化数据。支持 .docx（段落+表格+页眉页脚）、" +
			".xlsx/.xlsm（逐表逐格，公式与计算值都给）、.pptx（逐页文本框与表格）、.xls、.csv/.md/.txt。" +
			"大文件用 offset/limit 分段读。改文件前先用它看内容。",
		NoMemory: true,
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"file":     map[string]interface{}{"type": "string", "description": "文档绝对路径"},
				"sheet":    map[string]interface{}{"type": "string", "description": "xlsx：只读某张表（可选）"},
				"offset":   map[string]interface{}{"type": "integer", "description": "分段读的起始偏移（docx=块序号, xlsx/pptx=页/表序号），默认 0"},
				"limit":    map[string]interface{}{"type": "integer", "description": "本次最多返回多少段，默认全部"},
				"formulas": map[string]interface{}{"type": "boolean", "description": "xlsx 是否附带公式的计算值，默认 true"},
			},
			"required": []string{"file"},
		},
	}, p.handleRead)

	s.RegisterTool("create_document", sdk.ToolDef{
		Name: "create_document",
		Description: "从零新建办公文档。支持 .docx（title+content，content 支持 Markdown：# 标题、**粗体**、*斜体*、- 列表）、" +
			".xlsx（sheets 多表：表名+表头+行数据）、.pptx（title+content，## 分页）、.md、.csv、.txt。" +
			"目录不存在会自动创建；文件已存在时需传 overwrite=true 才会覆盖（覆盖前自动 .bak 留档）。",
		NoMemory: true,
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"file":      map[string]interface{}{"type": "string", "description": "要创建的文件绝对路径（扩展名决定格式）"},
				"title":     map[string]interface{}{"type": "string", "description": "标题（docx/pptx/md）"},
				"subtitle":  map[string]interface{}{"type": "string", "description": "副标题（pptx 首页）"},
				"content":   map[string]interface{}{"type": "string", "description": "正文内容（docx/md/txt 支持 Markdown；pptx 用 ## 分页）"},
				"sheets":    map[string]interface{}{"type": "array", "description": "xlsx 多表：[{name, headers, rows}]", "items": map[string]interface{}{"type": "object"}},
				"headers":   map[string]interface{}{"type": "array", "description": "单表模式的表头（xlsx/csv）", "items": map[string]interface{}{"type": "string"}},
				"rows":      map[string]interface{}{"type": "array", "description": "单表模式的数据行（xlsx/csv）", "items": map[string]interface{}{"type": "array"}},
				"tables":    map[string]interface{}{"type": "array", "description": "docx 追加表格：[{headers, rows}]", "items": map[string]interface{}{"type": "object"}},
				"overwrite": map[string]interface{}{"type": "boolean", "description": "文件已存在时是否覆盖，默认 false（报错）"},
			},
			"required": []string{"file"},
		},
	}, p.handleCreate)

	s.RegisterTool("create_from_template", sdk.ToolDef{
		Name: "create_from_template",
		Description: "基于已有文件复制一份再套用替换（模板场景）。先 shutil.copy2 复制，再对 docx/xlsx/文本做文本替换。" +
			"replacements 是 {\"旧文本\":\"新文本\"} 映射；也可用 target/replacement 只替换一处。",
		NoMemory: true,
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"template":     map[string]interface{}{"type": "string", "description": "模板文件绝对路径"},
				"file":         map[string]interface{}{"type": "string", "description": "新文件绝对路径"},
				"replacements": map[string]interface{}{"type": "object", "description": "替换映射 {\"旧\":\"新\"}"},
				"target":       map[string]interface{}{"type": "string", "description": "单处替换的原文本（可选）"},
				"replacement":  map[string]interface{}{"type": "string", "description": "单处替换的新文本（可选）"},
				"overwrite":    map[string]interface{}{"type": "boolean", "description": "目标已存在时是否覆盖"},
			},
			"required": []string{"template", "file"},
		},
	}, p.handleCreateFromTemplate)

	// ---- 兼容性红线：工具名 + 参数与 v1.0.0 完全一致 ----
	s.RegisterTool("edit_document", sdk.ToolDef{
		Name: "edit_document",
		Description: "编辑 Office 文档（会覆盖原文件，编辑前自动 .bak 留档）。支持 .docx / .xlsx / .xlsm / .pptx。" +
			"operation 可选：replace_text（查找替换，支持 regex/case_sensitive/nth）、set_cell、insert_row、" +
			"delete_row、insert_paragraph、append_paragraph、set_style、" +
			"insert_image、insert_table、merge_cells、add_sheet、delete_sheet、rename_sheet。" +
			"注意：pptx 无插入行 API，insert_row 在 pptx 上语义为「表格末尾追加一行」；merge_cells 不支持 pptx。",
		NoMemory: true,
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				// v1 原有 8 个参数，语义不变
				"file":        map[string]interface{}{"type": "string", "description": "文档文件路径"},
				"operation":   map[string]interface{}{"type": "string", "description": "操作（见工具描述）"},
				"target":      map[string]interface{}{"type": "string", "description": "要查找的文本（replace_text）；set_style 时为空表示全文"},
				"replacement": map[string]interface{}{"type": "string", "description": "替换为的文本（replace_text）"},
				"sheet":       map[string]interface{}{"type": "string", "description": "工作表名称（xlsx）"},
				"row":         map[string]interface{}{"type": "integer", "description": "行号（set_cell/insert_row/delete_row）"},
				"col":         map[string]interface{}{"type": "integer", "description": "列号（set_cell）"},
				"value":       map[string]interface{}{"type": "string", "description": "写入值（set_cell/add_sheet/rename_sheet/段落文本）"},
				// v2 扩展参数
				"regex":          map[string]interface{}{"type": "boolean", "description": "target 按正则解释，默认 false"},
				"case_sensitive": map[string]interface{}{"type": "boolean", "description": "是否区分大小写，默认 true"},
				"nth":            map[string]interface{}{"type": "integer", "description": "只替换第 N 处（1 基），默认全部替换"},
				"count":          map[string]interface{}{"type": "integer", "description": "delete_row 删除几行，默认 1"},
				"index":          map[string]interface{}{"type": "integer", "description": "insert_paragraph 的插入位置（段落序号）"},
				"row2":           map[string]interface{}{"type": "integer", "description": "merge_cells 的结束行"},
				"col2":           map[string]interface{}{"type": "integer", "description": "merge_cells 的结束列"},
				"table":          map[string]interface{}{"type": "integer", "description": "docx 表格序号（merge_cells/insert_table），从 0 开始"},
				"headers":        map[string]interface{}{"type": "array", "description": "insert_table 的表头", "items": map[string]interface{}{"type": "string"}},
				"rows":           map[string]interface{}{"type": "array", "description": "insert_table 的数据行", "items": map[string]interface{}{"type": "array"}},
				"image":          map[string]interface{}{"type": "string", "description": "insert_image 的图片路径"},
				"width_inch":     map[string]interface{}{"type": "number", "description": "insert_image 的宽度（英寸）"},
				"slide":          map[string]interface{}{"type": "integer", "description": "pptx 页号（从 1 开始）"},
				"size":           map[string]interface{}{"type": "number", "description": "set_style 字号（磅）"},
				"bold":           map[string]interface{}{"type": "boolean", "description": "set_style 加粗"},
				"italic":         map[string]interface{}{"type": "boolean", "description": "set_style 斜体"},
				"color":          map[string]interface{}{"type": "string", "description": "set_style 字体颜色（如 FF0000）"},
				"bg_color":       map[string]interface{}{"type": "string", "description": "xlsx set_style 背景色"},
				"align":          map[string]interface{}{"type": "string", "description": "set_style 对齐：left/center/right/justify"},
				"dry_run":        map[string]interface{}{"type": "boolean", "description": "只校验不改动"},
			},
			"required": []string{"file", "operation"},
		},
	}, p.handleEdit)

	s.RegisterTool("convert_document", sdk.ToolDef{
		Name: "convert_document",
		Description: "办公文档格式转换。md→docx/html/pdf（pandoc）、xlsx→csv（openpyxl，多表自动分文件）、" +
			"pptx→txt/md（结构化抽取）、office→pdf（soffice）。缺外部依赖时会在返回里点名缺哪个，不静默失败。",
		NoMemory: true,
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"file":      map[string]interface{}{"type": "string", "description": "源文件绝对路径"},
				"to":        map[string]interface{}{"type": "string", "description": "目标格式：docx / pdf / html / csv / txt / md"},
				"output":    map[string]interface{}{"type": "string", "description": "输出路径（可选，默认同名换扩展名）"},
				"sheet":     map[string]interface{}{"type": "string", "description": "xlsx→csv 时指定工作表（可选，默认全部）"},
				"overwrite": map[string]interface{}{"type": "boolean", "description": "输出已存在时覆盖"},
				"timeout":   map[string]interface{}{"type": "integer", "description": "转换超时（秒），默认 120"},
			},
			"required": []string{"file", "to"},
		},
	}, p.handleConvert)

	s.RegisterTool("office_check", sdk.ToolDef{
		Name: "office_check",
		Description: "办公能力自检（独立可调用，不依赖真实文件）。返回：Python 解释器版本、每个依赖库" +
			"（python-docx/openpyxl/python-pptx/xlrd/markdown）的可用性与版本、外部工具（soffice/pandoc）、" +
			"以及格式×能力矩阵。换机器或升级插件后可用它一条命令验收。",
		NoMemory: true,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{},
		},
	}, p.handleCheck)

	// 首次启动就把脚本释放好，让 office_check 立刻可用（也便于运维确认落盘位置）
	if sp, err := p.resolveScript(); err == nil {
		fmt.Fprintf(os.Stderr, "[%s] 已就绪（脚本: %s, 指纹: %s）\n", p.name, sp, scriptStamp())
		if p.ignoredScriptPath != "" {
			fmt.Fprintf(os.Stderr, "[%s] 注意：script_path=%s 已被忽略（非 v2 脚本），当前使用内嵌脚本\n",
				p.name, p.ignoredScriptPath)
		}
	} else {
		fmt.Fprintf(os.Stderr, "[%s] 脚本释放失败: %v\n", p.name, err)
	}

	return nil
}

func (p *Plugin) Stop() error { return nil }

func (p *Plugin) reloadSettings() {
	s := p.sdk.Settings()
	if v, err := s.Get("script_path"); err == nil {
		if str, ok := v.(string); ok {
			p.scriptPath = strings.TrimSpace(str)
		}
	}
	if v, err := s.Get("venv_python"); err == nil {
		if str, ok := v.(string); ok {
			p.venvPython = strings.TrimSpace(str)
		}
	}
	p.timeoutSec = 60
	if v, err := s.Get("timeout_sec"); err == nil {
		switch n := v.(type) {
		case float64:
			if n > 0 {
				p.timeoutSec = int(n)
			}
		case int64:
			if n > 0 {
				p.timeoutSec = int(n)
			}
		case int:
			if n > 0 {
				p.timeoutSec = n
			}
		}
	}
}

// resolveScript 返回 edit_doc.py 的可用路径。
//
// 优先级：
//  1. 配置 script_path 指向的文件，且**含 v2 协议标记**（便于运维手动覆盖调试）
//  2. DataDir()/runtime/edit_doc.py（内嵌脚本释放位置，指纹驱动的自动更新）
//
// 关键防呆：script_path 指向的文件若存在但**不含** v2 协议标记，说明它是旧版脚本
// （v1 的 argv 约定是 <file> <op> <json>，与 v2 的 <cmd> <json> 完全不同）。
// 若照样采用，6 个工具会全部报「文件不存在: create/read/edit/...」，等于把线上打挂。
// 因此这里主动忽略它、回退内嵌脚本，并置 p.ignoredScriptPath 让调用方能看到。
func (p *Plugin) resolveScript() (string, error) {
	p.ignoredScriptPath = ""
	if p.scriptPath != "" {
		if data, err := os.ReadFile(p.scriptPath); err == nil {
			if bytes.Contains(data, []byte(scriptProtocolMarker)) {
				return p.scriptPath, nil
			}
			// 存在但不是 v2 脚本：忽略，回退内嵌（绝不能让它把工具打挂）
			p.ignoredScriptPath = p.scriptPath
			fmt.Fprintf(os.Stderr,
				"[%s] 忽略 script_path=%s：缺少 v2 协议标记 %q，判定为旧版脚本；改用内嵌脚本\n",
				p.name, p.scriptPath, scriptProtocolMarker)
		}
	}
	// 释放内嵌脚本
	dir := ""
	if p.sdk != nil {
		dir = p.sdk.Settings().DataDir()
	}
	if dir == "" {
		return "", fmt.Errorf("拿不到插件数据目录（Settings().DataDir() 为空），且 script_path 未指向有效文件")
	}
	rt := filepath.Join(dir, "runtime")
	if err := os.MkdirAll(rt, 0755); err != nil {
		return "", fmt.Errorf("创建运行时目录失败: %w", err)
	}
	jsPath := filepath.Join(rt, "edit_doc.py")
	stampPath := filepath.Join(rt, ".stamp")
	want := scriptStamp()

	if got, err := os.ReadFile(stampPath); err == nil && string(got) == want {
		if _, e := os.Stat(jsPath); e == nil {
			return jsPath, nil
		}
	}
	if err := os.WriteFile(jsPath, embeddedScript, 0644); err != nil {
		return "", fmt.Errorf("释放内嵌脚本失败: %w", err)
	}
	_ = os.WriteFile(stampPath, []byte(want), 0644)
	return jsPath, nil
}

// resolvePython 返回可用的 python 解释器（配置为空时回退到 PATH 里的 python3）。
func (p *Plugin) resolvePython() (string, error) {
	py := p.venvPython
	if py == "" {
		if cand, err := exec.LookPath("python3"); err == nil {
			py = cand
		}
	}
	if py == "" {
		return "", fmt.Errorf("venv_python 未配置且 PATH 中找不到 python3")
	}
	if _, err := os.Stat(py); err != nil {
		return "", fmt.Errorf("Python 解释器不存在: %s", py)
	}
	return py, nil
}

// scriptError 是结构化错误体，直接透传给上层 LLM。
type scriptError struct {
	Error string `json:"error"`
	Hint  string `json:"hint"`
}

// runScript 执行 edit_doc.py <cmd> <argsJSON>，返回解析后的 map。
//
// 统一错误处理：脚本缺失/解释器缺失/超时/非 JSON 输出，都转成带 hint 的结构化返回，
// 而不是抛裸 error（v1 是直接 return nil, err，上层只能看到一句无上下文的失败）。
func (p *Plugin) runScript(cmd string, args map[string]interface{}) (map[string]interface{}, error) {
	p.reloadSettings() // 支持改配置后无需重载插件

	scriptPath, err := p.resolveScript()
	if err != nil {
		return nil, fmt.Errorf("%s；请在插件配置 script_path 指定 edit_doc.py 的绝对路径", err)
	}
	py, err := p.resolvePython()
	if err != nil {
		return nil, fmt.Errorf("%s；请在插件配置 venv_python 指定 python 解释器", err)
	}

	argsJSON, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("参数序列化失败: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(p.timeoutSec)*time.Second)
	defer cancel()

	var stdout, stderr bytes.Buffer
	c := exec.CommandContext(ctx, py, scriptPath, cmd, string(argsJSON))
	c.Stdout = &stdout
	c.Stderr = &stderr
	runErr := c.Run()

	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("脚本执行超时（%d 秒）；大文件或格式转换可调高 timeout_sec 配置",
			p.timeoutSec)
	}

	out := strings.TrimSpace(stdout.String())
	if out == "" {
		msg := strings.TrimSpace(stderr.String())
		if runErr != nil {
			return nil, fmt.Errorf("脚本无输出且退出异常: %v; stderr: %s", runErr, truncate(msg, 300))
		}
		return nil, fmt.Errorf("脚本无输出（stderr: %s）", truncate(msg, 300))
	}
	// 脚本只保证最后一行是 JSON（soffice/pandoc 可能往 stdout 混东西）
	line := out
	if i := strings.LastIndex(out, "\n"); i >= 0 {
		line = strings.TrimSpace(out[i+1:])
	}

	var m map[string]interface{}
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return nil, fmt.Errorf("脚本输出不是合法 JSON: %s; 原文: %s",
			err, truncate(out, 300))
	}
	if se := strings.TrimSpace(stderr.String()); se != "" && runErr != nil {
		m["stderr"] = truncate(se, 300)
	}
	// script_path 因非 v2 脚本被忽略时，把这次降级明确告知上层
	if p.ignoredScriptPath != "" {
		m["script_path_ignored"] = p.ignoredScriptPath
		m["script_path_ignored_reason"] = "该文件缺少 v2 协议标记，判定为旧版脚本；本次已改用内嵌脚本"
	}
	return m, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// respond 把脚本返回整形成工具返回值：ok:true 时给 content/summary，
// ok:false 时给 isError + error + hint。
func respondOffice(m map[string]interface{}) (interface{}, error) {
	if ok, _ := m["ok"].(bool); !ok {
		out := map[string]interface{}{
			"isError": true,
			"content": "操作失败: " + str(m["error"]),
			"error":   m["error"],
			"hint":    m["hint"],
		}
		for _, k := range []string{"path", "exists", "missing", "format", "cmd",
			"script_path_ignored", "script_path_ignored_reason"} {
			if v, has := m[k]; has {
				out[k] = v
			}
		}
		return out, nil
	}
	// 成功：把脚本的结构化字段原样上抛，便于 LLM 二次利用
	out := map[string]interface{}{}
	for k, v := range m {
		out[k] = v
	}
	if s, has := m["summary"]; has {
		out["content"] = s
	}
	return out, nil
}

func str(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// ---------------------------------------------------------------- handlers

func (p *Plugin) handleRead(args map[string]interface{}) (interface{}, error) {
	m, err := p.runScript("read", args)
	if err != nil {
		return toolErr("read_document", err), nil
	}
	return respondOffice(m)
}

func (p *Plugin) handleCreate(args map[string]interface{}) (interface{}, error) {
	m, err := p.runScript("create", args)
	if err != nil {
		return toolErr("create_document", err), nil
	}
	return respondOffice(m)
}

func (p *Plugin) handleCreateFromTemplate(args map[string]interface{}) (interface{}, error) {
	m, err := p.runScript("create_from_template", args)
	if err != nil {
		return toolErr("create_from_template", err), nil
	}
	return respondOffice(m)
}

func (p *Plugin) handleEdit(args map[string]interface{}) (interface{}, error) {
	// v1 兼容：file / operation 必填
	if str(args["file"]) == "" {
		return map[string]interface{}{"isError": true, "content": "edit_document 需要 file 参数"}, nil
	}
	if str(args["operation"]) == "" {
		return map[string]interface{}{"isError": true, "content": "edit_document 需要 operation 参数"}, nil
	}
	m, err := p.runScript("edit", args)
	if err != nil {
		return toolErr("edit_document", err), nil
	}
	return respondOffice(m)
}

func (p *Plugin) handleConvert(args map[string]interface{}) (interface{}, error) {
	m, err := p.runScript("convert", args)
	if err != nil {
		return toolErr("convert_document", err), nil
	}
	return respondOffice(m)
}

func (p *Plugin) handleCheck(args map[string]interface{}) (interface{}, error) {
	m, err := p.runScript("check", map[string]interface{}{})
	if err != nil {
		return toolErr("office_check", err), nil
	}
	return respondOffice(m)
}

func toolErr(tool string, err error) map[string]interface{} {
	return map[string]interface{}{
		"isError": true,
		"content": fmt.Sprintf("%s 失败：%s", tool, err.Error()),
		"error":   err.Error(),
	}
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}
