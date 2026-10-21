package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withSDKStore 把 SDK 存储指到临时目录（sdkStore 读 HOME），并造出给定版本目录。
func withSDKStore(t *testing.T, versions ...string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := filepath.Join(home, storeDirName, sdkDirName)
	if err := os.MkdirAll(store, 0755); err != nil {
		t.Fatal(err)
	}
	for _, v := range versions {
		if err := os.MkdirAll(filepath.Join(store, v), 0755); err != nil {
			t.Fatal(err)
		}
	}
}

// 项目声明中版本 → 挑该接口线里最新的补丁；声明完整版本 → 精确命中。
//
// 为什么允许中版本是关键判据：patch 位只含工具链/打包修复（接口不变），
// 让项目声明 "1.2" 而不是死钉 "1.2.0"，才能既跟得上工具链修复又不跨接口线。
func TestResolveSDKForProject(t *testing.T) {
	t.Run("区间写法（1.2）被拒并说明版本纪律", func(t *testing.T) {
		// 判据：SDK 的 patch 位恒为 .0 → 一条内核线只有一个 SDK 版本，
		// 区间写法会让人误以为「同一条线里还能挑版本」，所以直接拒。
		withSDKStore(t, "1.2.0")
		_, _, err := ResolveSDKForProject("1.2")
		if err == nil {
			t.Fatal("1.2 这种区间写法应被拒绝")
		}
		for _, want := range []string{"完整版本号", "patch 位恒为 .0"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("拒绝理由里应说清规矩（缺少 %q）: %v", want, err)
			}
		}
	})

	t.Run("完整版本精确命中", func(t *testing.T) {
		withSDKStore(t, "1.2.0", "1.3.0")
		_, ver, err := ResolveSDKForProject("1.2.0")
		if err != nil || ver != "1.2.0" {
			t.Fatalf("精确命中失败: ver=%s err=%v", ver, err)
		}
	})

	t.Run("存储目录带 v 前缀也能命中", func(t *testing.T) {
		withSDKStore(t, "v1.2.0")
		dir, ver, err := ResolveSDKForProject("1.2.0")
		if err != nil {
			t.Fatal(err)
		}
		if ver != "1.2.0" || !strings.HasSuffix(dir, "v1.2.0") {
			t.Fatalf("带 v 前缀的目录名未被识别: dir=%s ver=%s", dir, ver)
		}
	})

	t.Run("未命中要给出可执行命令与已装清单", func(t *testing.T) {
		withSDKStore(t, "1.2.0")
		_, _, err := ResolveSDKForProject("2.0.0")
		if err == nil {
			t.Fatal("应报错")
		}
		msg := err.Error()
		for _, want := range []string{"sdk=2.0.0", "hmapdev sdk install", "hmapdev sdk list", "1.2.0"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("错误信息缺少 %q（要可执行，不能只说 not found）:\n%s", want, msg)
			}
		}
	})

	t.Run("空存储也能给出安装指引", func(t *testing.T) {
		withSDKStore(t)
		_, _, err := ResolveSDKForProject("1.2.0")
		if err == nil || !strings.Contains(err.Error(), "hmapdev sdk install v1.2.0") {
			t.Fatalf("空存储时应提示装哪个版本: %v", err)
		}
	})

	t.Run("非法声明直接拒绝", func(t *testing.T) {
		withSDKStore(t, "1.2.0")
		for _, bad := range []string{"abc", "1", "1.2", "1.2.3.4", "-1.2.0"} {
			if _, _, err := ResolveSDKForProject(bad); err == nil {
				t.Fatalf("非法版本 %q 应被拒绝（宁可报错也不许当通配符）", bad)
			}
		}
	})

	t.Run("非版本目录不参与匹配", func(t *testing.T) {
		withSDKStore(t, "1.2.0", "backup-old", ".hidden", "current")
		_, ver, err := ResolveSDKForProject("1.2.0")
		if err != nil || ver != "1.2.0" {
			t.Fatalf("杂项目录不应干扰: ver=%s err=%v", ver, err)
		}
	})
}
