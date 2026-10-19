package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	inst, err := NewPluginFactory("files", nil)
	if err != nil { t.Fatal(err) }
	return inst.(*Plugin)
}

// 精确替换：old 在文件里出现多次时 files_edit 拒绝，edit_line 按行号可改。
func TestEditLineReplacesNonUniqueLine(t *testing.T) {
	p := newTestPlugin(t)
	dir := t.TempDir()
	p.filesDir = dir
	file := filepath.Join(dir, "a.go")
	content := "return nil\nx := 1\nreturn nil\n"
	os.WriteFile(file, []byte(content), 0644)

	res, err := p.handleEditLine(map[string]interface{}{
		"path": file,
		"edits": []interface{}{map[string]interface{}{
			"line": float64(3), "op": "replace", "text": "return err",
		}},
	})
	if err != nil { t.Fatal(err) }
	_ = res
	got, _ := os.ReadFile(file)
	want := "return nil\nx := 1\nreturn err\n"
	if string(got) != want {
		t.Fatalf("第3行应被替换。\nwant: %q\ngot:  %q", want, string(got))
	}
}

func TestEditLineExpectGuard(t *testing.T) {
	p := newTestPlugin(t)
	dir := t.TempDir()
	p.filesDir = dir
	file := filepath.Join(dir, "a.txt")
	os.WriteFile(file, []byte("alpha\nbeta\ngamma\n"), 0644)

	// 本插件的约定：工具级错误经**返回值**（isError:true）回传而非 Go error，
	// 与 handleEdit 一致 —— 断言返回体而不是 err。
	res, _ := p.handleEditLine(map[string]interface{}{
		"path": file,
		"edits": []interface{}{map[string]interface{}{
			"line": float64(2), "op": "replace", "text": "BETA", "expect": "delta",
		}},
	})
	m, _ := res.(map[string]interface{})
	if m == nil || m["isError"] != true {
		t.Fatalf("expect 不匹配应返回 isError:true，实际: %v", res)
	}
	content, _ := m["content"].(string)
	if !strings.Contains(content, "expected") {
		t.Fatalf("错误内容应指出 expect 不匹配: %s", content)
	}
	got, _ := os.ReadFile(file)
	if string(got) != "alpha\nbeta\ngamma\n" { t.Fatal("失败时文件不得被改") }
}

func TestEditLineInsertAndDelete(t *testing.T) {
	p := newTestPlugin(t)
	dir := t.TempDir()
	p.filesDir = dir
	file := filepath.Join(dir, "a.txt")
	os.WriteFile(file, []byte("one\ntwo\n"), 0644)

	if _, err := p.handleEditLine(map[string]interface{}{
		"path": file,
		"edits": []interface{}{
			map[string]interface{}{"line": float64(2), "op": "insert", "text": "one-five\none-b"}, // 插在 two 前
			map[string]interface{}{"line": float64(1), "op": "delete"},
		},
	}); err != nil { t.Fatal(err) }
	got, _ := os.ReadFile(file)
	// 从后往前应用：先删 line1(one)，再在 line2（原 two）前插两行
	want := "one-five\none-b\ntwo\n"
	if string(got) != want { t.Fatalf("want %q got %q", want, string(got)) }
}

func TestEditLineAppendAtEnd(t *testing.T) {
	p := newTestPlugin(t)
	dir := t.TempDir()
	p.filesDir = dir
	file := filepath.Join(dir, "a.txt")
	os.WriteFile(file, []byte("a\nb\n"), 0644)
	if _, err := p.handleEditLine(map[string]interface{}{
		"path": file,
		"edits": []interface{}{map[string]interface{}{"line": float64(3), "op": "insert", "text": "c"}},
	}); err != nil { t.Fatal(err) }
	got, _ := os.ReadFile(file)
	if string(got) != "a\nb\nc\n" { t.Fatalf("want a\\nb\\nc\\n got %q", string(got)) }
}
