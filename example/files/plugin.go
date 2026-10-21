package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/JianFeeeee/hologramagentsdk/sdk"
)

type Plugin struct {
	name     string
	sdk      *sdk.PluginSDK
	mu       sync.RWMutex
	filesDir string
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key:         "dir",
		Default:     "",
		Type:        "string",
		DisplayName: "文件系统根目录",
		Description: "文件操作允许访问的根目录；留空时使用默认沙箱目录（主数据目录/files_sandbox），不建议设为 /",
		Category:    "files",
	})

	dir := getSetting[string](s.Settings(), "dir", "")
	if strings.HasPrefix(dir, "~/") {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, dir[2:])
	}
	if dir == "" {
		dataDir, err := s.Settings().GetCore("core.daemon.data_dir")
		base := "."
		if err == nil {
			if ds, ok := dataDir.(string); ok && ds != "" {
				base = ds
			}
		}
		dir = filepath.Join(base, "files_sandbox")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve files.dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0755); err != nil {
		return fmt.Errorf("mkdir files.dir: %w", err)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	p.filesDir = abs

	tp := p.name + "_"

	s.RegisterTool(tp+"read", sdk.ToolDef{
		Name:        tp + "read",
		Description: fmt.Sprintf("Read file contents within the sandbox directory (%s). Supports offset/limit for large files.", p.filesDir),
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":   map[string]interface{}{"type": "string", "description": "File path relative to sandbox or absolute"},
				"offset": map[string]interface{}{"type": "integer", "description": "Starting line number (1-indexed, optional)"},
				"limit":  map[string]interface{}{"type": "integer", "description": "Max lines to return (optional)"},
			},
			"required": []string{"path"},
		},
		NoMemory: false,
		Cleaner: func(output string) string {
			var r struct{ Content string }
			if json.Unmarshal([]byte(output), &r) == nil && r.Content != "" {
				return r.Content
			}
			return output
		},
	}, p.handleRead)

	s.RegisterTool(tp+"write", sdk.ToolDef{
		Name:        tp + "write",
		Description: fmt.Sprintf("Write content to a file. Creates parent directories automatically. Sandbox: %s", p.filesDir),
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":    map[string]interface{}{"type": "string", "description": "File path"},
				"content": map[string]interface{}{"type": "string", "description": "Content to write"},
				"mode":    map[string]interface{}{"type": "string", "description": "Write mode: overwrite (default) | append | insert | create"},
				"line":    map[string]interface{}{"type": "integer", "description": "Line number for insert mode (1-indexed)"},
			},
			"required": []string{"path", "content"},
		},
	}, p.handleWrite)

	s.RegisterTool(tp+"edit", sdk.ToolDef{
		Name:        tp + "edit",
		Description: fmt.Sprintf("Apply exact string replacements to a file within the sandbox (%s). All edits are matched against the original file content.", p.filesDir),
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{"type": "string", "description": "File path relative to sandbox or absolute"},
				"edits": map[string]interface{}{
					"type": "array",
					"description": "One or more targeted replacements. Each old must match exactly once in the original file. Do not include overlapping edits.",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"old": map[string]interface{}{"type": "string", "description": "Exact text to find (must be unique)"},
							"new": map[string]interface{}{"type": "string", "description": "Replacement text"},
						},
						"required": []string{"old", "new"},
					},
				},
			},
			"required": []string{"path", "edits"},
		},
	}, p.handleEdit)

	s.RegisterTool(tp+"edit_line", sdk.ToolDef{
		Name: tp + "edit_line",
		Description: fmt.Sprintf("Replace or insert by **line number** within the sandbox (%s). "+
			"Use this instead of %sedit when the target text is not unique (e.g. a blank line, "+
			"a closing brace, or a repeated statement) — %sedit requires the old text to appear exactly "+
			"once and will refuse otherwise. Line numbers are 1-indexed and refer to the file as it is "+
			"before this call; multiple edits are applied bottom-up so earlier line numbers stay valid.",
			p.filesDir, p.name+"_", p.name+"_"),
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{"type": "string", "description": "File path relative to sandbox or absolute"},
				"edits": map[string]interface{}{
					"type": "array",
					"description": "Line-targeted operations. Do not overlap ranges. Applied from the last line to the first.",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"line": map[string]interface{}{"type": "integer", "description": "Target line (1-indexed)"},
							"op": map[string]interface{}{
								"type":        "string",
								"description": "replace (default): replace that line | insert: insert new text *before* that line | delete: remove that line",
							},
							"text": map[string]interface{}{"type": "string", "description": "New text (may contain \\n for multiple lines). Omit for delete."},
							"expect": map[string]interface{}{
								"type":        "string",
								"description": "Optional safety check: the current line must equal this exactly (ignoring trailing \\r). Guards against acting on a stale line number.",
							},
						},
						"required": []string{"line"},
					},
				},
			},
			"required": []string{"path", "edits"},
		},
	}, p.handleEditLine)

	s.RegisterTool(tp+"ls", sdk.ToolDef{
		Name:        tp + "ls",
		Description: fmt.Sprintf("List directory contents within the sandbox (%s). Directories are marked with / suffix.", p.filesDir),
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":  map[string]interface{}{"type": "string", "description": "Directory path (optional, defaults to sandbox root)"},
				"limit": map[string]interface{}{"type": "integer", "description": "Max entries (optional, default 500)"},
			},
		},
	}, p.handleLs)

	log.Printf("[%s] started, sandbox: %s", p.name, p.filesDir)
	return nil
}

func (p *Plugin) Stop() error {
	log.Printf("[%s] stopped", p.name)
	return nil
}

// resolvePath resolves user-provided path to an absolute path within filesDir.
func (p *Plugin) resolvePath(userPath string) (string, error) {
	if userPath == "" {
		userPath = "."
	}
	if !filepath.IsAbs(userPath) {
		userPath = filepath.Join(p.filesDir, userPath)
	}
	abs, err := filepath.Abs(userPath)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	base := filepath.Clean(p.filesDir)
	if !withinSandbox(base, abs) {
		return "", fmt.Errorf("path outside sandbox: %s", userPath)
	}
	real, err := evalReal(base, abs)
	if err != nil {
		return "", err
	}
	if !withinSandbox(base, real) {
		return "", fmt.Errorf("path escapes sandbox via symlink: %s", userPath)
	}
	return real, nil
}

func withinSandbox(base, abs string) bool {
	if base == "/" {
		return true
	}
	return abs == base || strings.HasPrefix(abs, base+string(filepath.Separator))
}

func evalReal(base, abs string) (string, error) {
	existing := abs
	var tail []string
	for {
		real, err := filepath.EvalSymlinks(existing)
		if err == nil {
			full := real
			for i := len(tail) - 1; i >= 0; i-- {
				full = filepath.Join(full, tail[i])
			}
			return full, nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("resolve path: %w", err)
		}
		if link, lerr := os.Readlink(existing); lerr == nil {
			target := link
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(existing), target)
			}
			if t, aerr := filepath.Abs(target); aerr == nil {
				target = filepath.Clean(t)
			}
			if !withinSandbox(base, target) {
				return "", fmt.Errorf("path escapes sandbox via symlink: %s", abs)
			}
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", fmt.Errorf("resolve path: %w", err)
		}
		tail = append(tail, filepath.Base(existing))
		existing = parent
	}
}

// handleRead implements the read tool.
func (p *Plugin) handleRead(args map[string]interface{}) (interface{}, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return errorResult("path is required"), nil
	}

	absPath, err := p.resolvePath(path)
	if err != nil {
		return errorResult(err.Error()), nil
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return errorResult("file not found: " + path), nil
		}
		return errorResult("stat error: " + err.Error()), nil
	}
	if info.IsDir() {
		return errorResult("is a directory, use ls instead: " + path), nil
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return errorResult("read error: " + err.Error()), nil
	}

	text := string(data)
	lines := strings.Split(text, "\n")
	totalLines := len(lines)

	offset := 0
	if v, ok := args["offset"].(float64); ok && v > 0 {
		offset = int(v) - 1
	}
	if offset >= totalLines {
		return errorResult(fmt.Sprintf("offset %d exceeds file length (%d lines)", offset+1, totalLines)), nil
	}

	limit := totalLines - offset
	if v, ok := args["limit"].(float64); ok && v > 0 {
		if int(v) < limit {
			limit = int(v)
		}
	}

	end := offset + limit
	if end > totalLines {
		end = totalLines
	}

	selected := lines[offset:end]
	output := strings.Join(selected, "\n")

	truncated := false
	if limit < totalLines-offset {
		truncated = true
	}

	var sb strings.Builder
	sb.WriteString(output)
	if truncated {
		nextOffset := end + 1
		sb.WriteString(fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Use offset=%d to continue.]", offset+1, end, totalLines, nextOffset))
	} else if offset > 0 || end < totalLines {
		sb.WriteString(fmt.Sprintf("\n\n[%d lines total]", totalLines))
	}

	return map[string]interface{}{
		"content": sb.String(),
	}, nil
}

// handleWrite implements the write tool.
func (p *Plugin) handleWrite(args map[string]interface{}) (interface{}, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return errorResult("path is required"), nil
	}
	content, _ := args["content"].(string)
	mode, _ := args["mode"].(string)
	if mode == "" {
		mode = "overwrite"
	}

	line := 0
	if v, ok := args["line"].(float64); ok && v > 0 {
		line = int(v)
	}

	absPath, err := p.resolvePath(path)
	if err != nil {
		return errorResult(err.Error()), nil
	}

	switch mode {
	case "create":
		if _, err := os.Stat(absPath); err == nil {
			return errorResult("file already exists: " + path), nil
		}
		dir := filepath.Dir(absPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return errorResult("mkdir error: " + err.Error()), nil
		}
		if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
			return errorResult("write error: " + err.Error()), nil
		}
		return map[string]interface{}{
			"content": fmt.Sprintf("Created %s (%d bytes)", path, len(content)),
		}, nil

	case "append":
		dir := filepath.Dir(absPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return errorResult("mkdir error: " + err.Error()), nil
		}
		f, err := os.OpenFile(absPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return errorResult("open error: " + err.Error()), nil
		}
		defer f.Close()
		if _, err := f.WriteString(content); err != nil {
			return errorResult("append error: " + err.Error()), nil
		}
		return map[string]interface{}{
			"content": fmt.Sprintf("Appended %d bytes to %s", len(content), path),
		}, nil

	case "insert":
		if line < 1 {
			return errorResult("line must be >= 1 for insert mode"), nil
		}
		data, err := os.ReadFile(absPath)
		if err != nil {
			if os.IsNotExist(err) {
				return errorResult("file not found: " + path), nil
			}
			return errorResult("read error: " + err.Error()), nil
		}
		lines := strings.Split(string(data), "\n")
		if line > len(lines)+1 {
			return errorResult(fmt.Sprintf("line %d exceeds file length (%d lines)", line, len(lines))), nil
		}
		idx := line - 1
		newLines := make([]string, 0, len(lines)+1)
		newLines = append(newLines, lines[:idx]...)
		newLines = append(newLines, content)
		newLines = append(newLines, lines[idx:]...)
		result := strings.Join(newLines, "\n")
		if err := os.WriteFile(absPath, []byte(result), 0644); err != nil {
			return errorResult("write error: " + err.Error()), nil
		}
		return map[string]interface{}{
			"content": fmt.Sprintf("Inserted %d bytes at line %d in %s", len(content), line, path),
		}, nil

	default: // overwrite
		dir := filepath.Dir(absPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return errorResult("mkdir error: " + err.Error()), nil
		}
		if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
			return errorResult("write error: " + err.Error()), nil
		}
		return map[string]interface{}{
			"content": fmt.Sprintf("Wrote %d bytes to %s", len(content), path),
		}, nil
	}
}

// handleEdit implements the edit tool.
func (p *Plugin) handleEdit(args map[string]interface{}) (interface{}, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return errorResult("path is required"), nil
	}

	absPath, err := p.resolvePath(path)
	if err != nil {
		return errorResult(err.Error()), nil
	}

	rawEdits, ok := args["edits"].([]interface{})
	if !ok || len(rawEdits) == 0 {
		return errorResult("edits must be a non-empty array"), nil
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return errorResult("file not found: " + path), nil
		}
		return errorResult("read error: " + err.Error()), nil
	}

	original := string(data)
	content := original
	applied := 0
	var errors []string

	for i, raw := range rawEdits {
		edit, ok := raw.(map[string]interface{})
		if !ok {
			errors = append(errors, fmt.Sprintf("edit[%d]: invalid format", i))
			continue
		}
		oldText, _ := edit["old"].(string)
		newText, _ := edit["new"].(string)
		if oldText == "" {
			errors = append(errors, fmt.Sprintf("edit[%d]: old is required", i))
			continue
		}

		count := strings.Count(content, oldText)
		if count == 0 {
			errors = append(errors, fmt.Sprintf("edit[%d]: could not find %q in %s", i, oldText, path))
			continue
		}
		if count > 1 {
			errors = append(errors, fmt.Sprintf("edit[%d]: found %d occurrences of %q, must be unique", i, count, oldText))
			continue
		}

		content = strings.Replace(content, oldText, newText, 1)
		applied++
	}

	if applied == 0 {
		msg := "no edits applied"
		if len(errors) > 0 {
			msg += ": " + strings.Join(errors, "; ")
		}
		return errorResult(msg), nil
	}

	if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
		return errorResult("write error: " + err.Error()), nil
	}

	msg := fmt.Sprintf("Successfully applied %d/%d edits to %s", applied, len(rawEdits), path)
	if len(errors) > 0 {
		msg += "\nWarnings:\n" + strings.Join(errors, "\n")
	}

	return map[string]interface{}{
		"content": msg,
	}, nil
}

// handleEditLine implements the edit_line tool: line-number targeted edits.
//
// ## 为什么除了 edit 还要这个
//
// files_edit 的判据是「old 文本在文件里**恰好出现一次**」。这个约束在真实
// 改代码时经常不成立：
//
//	· 改一个 `}`、一个空行、`return nil` —— 全文件出现几十次，edit 直接拒绝；
//	· 改「第三个 case 分支里的 break」—— 文本与前两个字节相同，无法定位。
//
// 于是模型只能退化成 read 全文 + write 全文，而那是有损的：整段重写会
// 顺带改动空白/换行/文件末尾，diff 里再也看不出「只改了这一行」。
//
// 本工具按**行号**定位，不依赖文本唯一性；`expect` 提供可选的安全闸
// （行内容与预期不符就整体放弃，防止拿到过期行号后改错地方）。
func (p *Plugin) handleEditLine(args map[string]interface{}) (interface{}, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return errorResult("path is required"), nil
	}
	absPath, err := p.resolvePath(path)
	if err != nil {
		return errorResult(err.Error()), nil
	}

	rawEdits, ok := args["edits"].([]interface{})
	if !ok || len(rawEdits) == 0 {
		return errorResult("edits must be a non-empty array"), nil
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return errorResult("file not found: " + path), nil
		}
		return errorResult("read error: " + err.Error()), nil
	}
	original := string(data)

	// 按行切分并保留行尾信息：是否有 \r\n、末尾是否带换行。
	// 这两点必须原样还原，否则「精确替换一行」会顺手改掉整个文件的换行风格。
	crlf := strings.Contains(original, "\r\n")
	endsWithNewline := strings.HasSuffix(original, "\n")
	lines := strings.Split(original, "\n")
	if endsWithNewline {
		lines = lines[:len(lines)-1]
	}
	// 每行去掉 \r（若原文件是 CRLF），edit 内部统一按无 \r 的文本处理。
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}

	type op struct {
		line   int
		kind   string
		text   string
		expect string
		idx    int
	}
	var ops []op
	var errors []string

	for i, raw := range rawEdits {
		edit, ok := raw.(map[string]interface{})
		if !ok {
			errors = append(errors, fmt.Sprintf("edit[%d]: invalid format", i))
			continue
		}
		ln64, err := toInt64(edit["line"])
		if err != nil || ln64 < 1 {
			errors = append(errors, fmt.Sprintf("edit[%d]: line must be a positive integer", i))
			continue
		}
		kind, _ := edit["op"].(string)
		if kind == "" {
			kind = "replace"
		}
		if kind != "replace" && kind != "insert" && kind != "delete" {
			errors = append(errors, fmt.Sprintf("edit[%d]: op must be replace|insert|delete, got %q", i, kind))
			continue
		}
		text, _ := edit["text"].(string)
		if kind != "delete" && text == "" && edit["text"] == nil {
			errors = append(errors, fmt.Sprintf("edit[%d]: text is required for op=%s", i, kind))
			continue
		}
		expect, _ := edit["expect"].(string)
		ops = append(ops, op{line: int(ln64), kind: kind, text: text, expect: expect, idx: i})
	}

	if len(errors) > 0 {
		return errorResult("invalid edits: " + strings.Join(errors, "; ")), nil
	}

	// 行号允许等于 len(lines)+1 用于在文件末尾追加（insert）。
	for _, o := range ops {
		if o.line > len(lines)+1 {
			return errorResult(fmt.Sprintf(
				"edit[%d]: line %d is out of range (file has %d lines)", o.idx, o.line, len(lines))), nil
		}
	}

	// ★ 从后往前应用：这样前面编辑的行号不受影响，调用方可以用「一次性
	// 基于原始文件的行号」来描述多处修改，无需自己算偏移。
	sort.SliceStable(ops, func(a, b int) bool { return ops[a].line > ops[b].line })

	for _, o := range ops {
		pos := o.line - 1 // 0-indexed 插入/替换位置
		switch o.kind {
		case "insert":
			// 允许 line == len(lines)+1：追加到末尾。
			if pos > len(lines) {
				pos = len(lines)
			}
			newLines := strings.Split(o.text, "\n")
			lines = append(lines[:pos], append(newLines, lines[pos:]...)...)
		case "delete":
			if pos >= len(lines) {
				return errorResult(fmt.Sprintf(
					"edit[%d]: cannot delete line %d (file has %d lines)", o.idx, o.line, len(lines))), nil
			}
			if o.expect != "" && lines[pos] != o.expect {
				return errorResult(fmt.Sprintf(
					"edit[%d]: line %d is %q, expected %q — file changed since you read it; re-read and retry",
					o.idx, o.line, lines[pos], o.expect)), nil
			}
			lines = append(lines[:pos], lines[pos+1:]...)
		case "replace":
			if pos >= len(lines) {
				return errorResult(fmt.Sprintf(
					"edit[%d]: cannot replace line %d (file has %d lines); use op=insert to append",
					o.idx, o.line, len(lines))), nil
			}
			// expect 是安全闸：行号过期时宁可报错，也不要在错误位置写内容。
			if o.expect != "" && lines[pos] != o.expect {
				return errorResult(fmt.Sprintf(
					"edit[%d]: line %d is %q, expected %q — file changed since you read it; re-read and retry",
					o.idx, o.line, lines[pos], o.expect)), nil
			}
			newLines := strings.Split(o.text, "\n")
			lines = append(lines[:pos], append(newLines, lines[pos+1:]...)...)
		}
	}

	sep := "\n"
	if crlf {
		sep = "\r\n"
	}
	out := strings.Join(lines, sep)
	if endsWithNewline {
		out += sep
	}

	if err := os.WriteFile(absPath, []byte(out), 0644); err != nil {
		return errorResult("write error: " + err.Error()), nil
	}

	return map[string]interface{}{
		"content": fmt.Sprintf("Successfully applied %d line edit(s) to %s (%d lines now)",
			len(ops), path, len(lines)),
	}, nil
}

// toInt64 宽容地把 JSON 数字（float64）或字符串转成 int64。
func toInt64(v interface{}) (int64, error) {
	switch n := v.(type) {
	case float64:
		return int64(n), nil
	case int:
		return int64(n), nil
	case int64:
		return n, nil
	case json.Number:
		return n.Int64()
	case string:
		var i int64
		if _, err := fmt.Sscanf(n, "%d", &i); err != nil {
			return 0, err
		}
		return i, nil
	}
	return 0, fmt.Errorf("cannot convert %T to int64", v)
}

// handleLs implements the ls tool.
func (p *Plugin) handleLs(args map[string]interface{}) (interface{}, error) {
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}

	absPath, err := p.resolvePath(path)
	if err != nil {
		return errorResult(err.Error()), nil
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return errorResult("path not found: " + path), nil
		}
		return errorResult("stat error: " + err.Error()), nil
	}
	if !info.IsDir() {
		return errorResult("not a directory: " + path), nil
	}

	entries, err := os.ReadDir(absPath)
	if err != nil {
		return errorResult("readdir error: " + err.Error()), nil
	}

	limit := 500
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})

	var lines []string
	entryLimitReached := false
	for i, entry := range entries {
		if i >= limit {
			entryLimitReached = true
			break
		}
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		lines = append(lines, name)
	}

	if len(lines) == 0 {
		return map[string]interface{}{
			"content": "(empty directory)",
		}, nil
	}

	output := strings.Join(lines, "\n")
	if entryLimitReached {
		output += fmt.Sprintf("\n\n[%d entries limit reached. Use limit=N for more.]", limit)
	}

	return map[string]interface{}{
		"content": output,
	}, nil
}

// errorResult returns a standardized error result.
func errorResult(msg string) map[string]interface{} {
	return map[string]interface{}{
		"isError": true,
		"content": msg,
	}
}

// getSetting reads a setting with generic type assertion.
func getSetting[T any](s sdk.SettingsAPI, key string, def T) T {
	v, err := s.Get(key)
	if err != nil || v == nil {
		return def
	}
	val, ok := v.(T)
	if !ok {
		return def
	}
	return val
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}
