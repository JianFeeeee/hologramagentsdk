package yaegi

import "testing"

// isSDKModule 必须同时认新旧两个 SDK module 名。
//
// 迁移（gitcode → github）后只认新名会让 findSDKGoPath 对**存量插件**
// （go.mod 里仍是旧名）找不到 SDK 源码路径，且**静默**退化到兜底分支。
func TestIsSDKModuleAcceptsBothNames(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"replace github.com/JianFeeeee/hologramagentsdk => ../sdk", true},
		{"replace gitcode.com/JianFeeeee/homeagent-sdk => /tmp/sdk-clone", true},
		{"github.com/JianFeeeee/hologramagentsdk/sdk", true},
		{"gitcode.com/JianFeeeee/homeagent-sdk/sdk", true},
		{"replace github.com/other/thing => ../x", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isSDKModule(c.s); got != c.want {
			t.Errorf("isSDKModule(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}
