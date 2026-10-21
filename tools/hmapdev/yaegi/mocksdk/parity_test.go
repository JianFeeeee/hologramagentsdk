package mocksdk

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// structFields 解析 file 中的指定 struct，返回其字段名集合。
func structFields(t *testing.T, file, structName string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s 失败: %v", file, err)
	}
	return fieldsOf(t, fset, f, structName)
}

func fieldsOf(t *testing.T, fset *token.FileSet, f *ast.File, structName string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != structName {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return false
		}
		for _, fld := range st.Fields.List {
			for _, nm := range fld.Names {
				out[nm.Name] = true
			}
		}
		return false
	})
	return out
}

// sdkFieldsOf 用反射取公共 SDK ToolDef 的字段名。
func sdkFieldsOf(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	rt := reflect.TypeOf(sdk.ToolDef{})
	for i := 0; i < rt.NumField(); i++ {
		out[rt.Field(i).Name] = true
	}
	return out
}

func os_ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

func exprString(fset *token.FileSet, e ast.Expr) string {
	var sb strings.Builder
	if err := printer.Fprint(&sb, fset, e); err != nil {
		return ""
	}
	return sb.String()
}

// normalize 归一化类型串：抹掉包路径与指针/切片等修饰差异，只比基础类型。
func normalize(s string) string {
	// 抹掉所有空白：gofmt 打印 "interface{}" 而反射给 "interface {}"，
	// 这是**打印格式差异**，不是类型差异 —— 我第一版没抹空白，
	// 结果判据把每个复合类型都误报成"类型不符"。
	s = strings.Join(strings.Fields(s), "")
	if i := strings.LastIndex(s, "."); i >= 0 && !strings.Contains(s, "]") {
		s = s[i+1:]
	}
	return strings.TrimPrefix(s, "*")
}

var _ = fmt.Sprintf

// TestMockSDKToolDefMatchesSDK 保证 mocksdk 的 ToolDef 与公共 SDK **逐字段对齐**。
//
// 为什么必须有：mocksdk 是 yaegi 解释执行时的替身 SDK。插件作者用它本地调试，
// 少一个字段就会"写了声明却不报错"，直到编译安装后才发现声明没被内核读到。
// 这种不一致极难察觉，且**不会让任何现有测试失败** —— 所以要显式钉住。
//
// 这不是理论风险：RecallPolicy、ParallelSafe、Serial 三个字段就是先后漂移的
// （主 SDK 先加，mocksdk 没跟上）。
func TestMockSDKToolDefMatchesSDK(t *testing.T) {
	mockFields := structFields(t, "plugin.go", "ToolDef")
	sdkFields := sdkFieldsOf(t)

	var missing, extra []string
	for name := range sdkFields {
		if !mockFields[name] {
			missing = append(missing, name)
		}
	}
	for name := range mockFields {
		if !sdkFields[name] {
			extra = append(extra, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("mocksdk.ToolDef 缺少字段 %v —— 插件本地调试时写了声明不报错，装到内核却不生效", missing)
	}
	if len(extra) > 0 {
		t.Errorf("mocksdk.ToolDef 有公共 SDK 没有的字段 %v —— 替身比本体还多，必有一方理解错了", extra)
	}
}

// TestMockSDKToolDefTypesMatch 字段类型也要一致（不只是名字）。
func TestMockSDKToolDefTypesMatch(t *testing.T) {
	rt := reflect.TypeOf(sdk.ToolDef{})
	src, err := os_ReadFile("plugin.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "plugin.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "ToolDef" {
			return true
		}
		st := ts.Type.(*ast.StructType)
		for _, fld := range st.Fields.List {
			name := fld.Names[0].Name
			sf, exists := rt.FieldByName(name)
			if !exists {
				continue
			}
			want := sf.Type.String()
			got := exprString(fset, fld.Type)
			// 归一化包路径前缀
			got = normalize(got)
			if got != normalize(want) {
				t.Errorf("mocksdk.ToolDef.%s 类型是 %s，公共 SDK 是 %s", name, got, want)
			}
		}
		return false
	})
}
