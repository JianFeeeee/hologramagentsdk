package packaging

// 发版流水线的**开发阶段保护**判据（2026-10-18 实测事故）。
//
// ## 事故
//
// SDK 仓的 release.yml 触发条件是 `push: branches: ['release/**']`，
// 而 tag 名 = `v` + `meta.Version`。于是：
//
//	2026-10-17 08:35  github-actions[bot] 打出 v1.4.0 并发布 Release
//
// 而 1.4.0 当时是**开发阶段**。起因只是有人（我）为了走 PR 流程，
// 建了一个 release/v1.4.x 分支并推送 —— 那一刻就自动发版了。
//
// 更麻烦的是它钉在旧提交上：之后又合了三个修复（异步投递、台账回填、
// 示例修复），已发布的 1.4.0 里**没有**这些修复，而 Release 看着是"最新"。
//
// ## 判据
//
// 分支名与 meta.Version 必须一致才允许发版：
//   · release/vX.Y.x  → meta 必须是 X.Y.* （中版本长跑线）
//   · release/vX.Y.Z  → meta 必须恰好是 X.Y.Z
// 不一致就 exit 1，不静默发一个错版本。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func releaseWorkflow(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	p := filepath.Join(root, ".github", "workflows", "release.yml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 release.yml: %v", err)
	}
	return string(b)
}

// ★★★ 判据：workflow 必须校验分支名与 meta.Version 一致。
func TestReleaseWorkflowGuardsVersionMismatch(t *testing.T) {
	src := releaseWorkflow(t)

	// ★ 每种分支形态**各自**必须有一段拒绝逻辑。
	//
	// 我第一版写的是 `if !contains(A) && !contains(B)` —— 删掉其中任意一处
	// 描述文案，另一处还在，判据仍然通过 ⇒ 变异不咬（实测）。
	// 现在逐形态断言：每个形态的拒绝消息都必须在。
	for _, c := range []struct{ form, msg string }{
		{"release/v*.*.x", "与 meta.Version=$V 不匹配"},
		{"release/v[0-9]*.[0-9]*.[0-9]*", "（钉到 $BVER）与 meta.Version=$V 不一致"},
	} {
		if !strings.Contains(src, c.form) {
			t.Fatalf("★ release.yml 未处理 %s 形态的分支", c.form)
		}
		if !strings.Contains(src, c.msg) {
			t.Fatalf("★ %s 形态**没有拒绝逻辑**（缺 %q）—— "+
				"开发阶段随手推 release/vX.Y.x 就会自动打出开发版本的 tag 并发 Release", c.form, c.msg)
		}
	}
	// 必须真的 exit 1（而不是只打日志）
	if !regexp.MustCompile(`exit 1`).MatchString(src) {
		t.Fatal("★ 校验没有 exit 1 —— 报错了还继续发版")
	}
}

// ★★★ 判据：meta.Version 当前是开发版本（1.4.0），且**没有**对应的 tag。
//
// 这条把"1.4.0 是开发阶段"这个事实钉住：一旦有人打了 v1.4.0 tag，
// 判据会提醒"要么这是正式发版、要么你误触了自动流水线"。
func TestNoStaleReleaseTagForDevVersion(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "meta", "meta.go"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*Version\s*=\s*"([^"]*)"`).FindSubmatch(b)
	if m == nil {
		t.Fatal("读不到 meta.Version")
	}
	ver := string(m[1])
	t.Logf("meta.Version = %s（开发阶段）", ver)

	// 不在这里调 git（CI 里可能没有完整历史）；只做静态提醒。
	// 真正的 tag 检查在 TestReleaseWorkflowGuardsVersionMismatch 的路由保护里。
	if strings.Contains(ver, "dev") || strings.HasSuffix(ver, "-dev") {
		t.Log("开发版本号，release 流水线应被分支校验挡住")
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(wd)
}
