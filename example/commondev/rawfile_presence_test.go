package main

// rawfile/ 内容**不进版本库**，却决定发布包能不能跑。
//
// ## 为什么需要这条判据
//
// `example/commondev/rawfile/.gitignore` 写着 `*`（只放行 .gitignore/.gitkeep）：
// 资源可能是上百兆的 node/pi 二进制，进版本库会让克隆变慢、每次升级产生
// 巨型 diff。这是设计意图，不是疏忽。
//
// 但代价是：**CI 从零克隆时 rawfile/ 是空的**。于是
//
//	本地打包 → 包里 571 个 runtime 条目（工作区里已备好）→ 冒烟通过
//	CI  打包 → 包里 0 个 runtime 条目 → 发布出去的插件起不来
//
// 而两次构建的**日志都写着"成功"**。这类"本地绿、CI 红/坏"我们已经
// 吃过一次（TestHmapdevTemplateImplementsAllPluginMgrMethods 读 gitignore
// 的文件，本地绿 CI 红）。
//
// ## 判据的作用
//
// 不能要求 rawfile/ 必须有内容（干净克隆本来就没有），但可以：
//   · 有内容时，必须包含我们自己的 sidecar（而不是只有 node_modules）
//   · 没有内容时，明确说明"这个构建产不出可用包" —— 让人看见，而不是静默

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ★★★ sidecar 是插件运行时的必需件，不能只靠"本地恰好有"。
func TestRuntimeSidecarsPresentOrLoudlyAbsent(t *testing.T) {
	root := filepath.Join("rawfile", "runtime")
	ents, err := os.ReadDir(root)
	if err != nil {
		// 干净克隆：rawfile/runtime 根本不存在。
		// 这不是测试失败，但必须**大声说出来** —— 因为此时打出的包不可用。
		t.Logf("★★ 注意：%s 不存在（干净克隆的正常状态）。"+
			"此状态下 build-examples.sh 打出的 commondev.hmap **不含 sidecar**，"+
			"插件在该包里无法投递任务。发布前必须在有 rawfile 资源的工作区打包。", root)
		return
	}
	var clis []string
	for _, e := range ents {
		if e.IsDir() {
			clis = append(clis, e.Name())
		}
	}
	if len(clis) == 0 {
		t.Logf("★★ 注意：%s 存在但没有 CLI 子目录 —— 同上，包不可用。", root)
		return
	}

	// 有内容时，每个 CLI 都必须有 sidecar.mjs（或缺省的 driver 脚本）
	for _, cli := range clis {
		sc := filepath.Join(root, cli, "sidecar.mjs")
		b, err := os.ReadFile(sc)
		if err != nil {
			// driver.json 可以指定别的脚本名
			dj := filepath.Join(root, cli, "driver.json")
			if _, derr := os.Stat(dj); derr == nil {
				continue // 由 driver.json 指定，正常
			}
			t.Fatalf("★ %s 有目录但没有 sidecar.mjs 也没有 driver.json —— "+
				"该 CLI 无法运行", cli)
		}
		// pi/zcode 的 sidecar 必须支持 interrupt（否则取消能力在包里失效）
		if cli == "pi" || cli == "zcode" {
			if !strings.Contains(string(b), `case "interrupt":`) {
				t.Fatalf("★ %s/sidecar.mjs 没有 interrupt —— "+
					"打包后 commondev_agent_interrupt 会失效（源码改了但资源没同步）", cli)
			}
		}
	}
	t.Logf("rawfile/runtime 下 %d 个 CLI 的 sidecar 齐备: %v", len(clis), clis)
}
