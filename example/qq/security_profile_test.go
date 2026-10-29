package main

// 安全策略**声明式**的判据（2026-10-29）。
//
// ## 钉什么
//
// 本轮把 QQ 的权限判定从「插件自己判」改为「插件声明 + 内核执行」。
// 这类改动最容易在**两个地方静默退化**：
//
//	① securityProfileFor 按身份选错档 ⇒ 访客拿到 owner 档位
//	   （= 硬私有清单不生效，非所有者能读私人记忆）
//	② 声明时没把 hard_private_tools 真正交给内核 ⇒ 档位是空的，
//	   访客档位形同虚设 —— 而日志上"声明成功"看起来一切正常
//
// 本文件对两条都钉住。

import (
	"testing"

	"github.com/JianFeeeee/hologramagentsdk/sdk"
)

// captureSecurityAPI 记录插件声明了什么（它就是"契约"本身：
// 内核拿到的 DenyTools 必须与插件读到的硬私有清单一致）。
type captureSecurityAPI struct {
	profiles []sdk.SecurityProfile
	err      error
}

func (c *captureSecurityAPI) DeclareProfile(p sdk.SecurityProfile) error {
	if c.err != nil {
		return c.err
	}
	c.profiles = append(c.profiles, p)
	return nil
}

func (c *captureSecurityAPI) DeclareProfiles(ps []sdk.SecurityProfile) error {
	if c.err != nil {
		return c.err
	}
	c.profiles = append(c.profiles, ps...)
	return nil
}

// pluginWithSecurity 造一个已注入 SecurityAPI 的 SDK。
//
// 注意：sdk.PluginSDK 的 SetSecurityAPI 是公开方法（内核用它注入，
// 测试也用它），所以这里可以直接构造。
func pluginWithSecurity(t *testing.T, cap *captureSecurityAPI) *sdk.PluginSDK {
	t.Helper()
	s := &sdk.PluginSDK{}
	s.SetSecurityAPI(cap)
	return s
}

// ① 身份 → 档位映射（最容易写反的一处）。
func TestSecurityProfileForIdentity(t *testing.T) {
	p := &Plugin{}

	if got := p.securityProfileFor(true); got != securityProfileOwner {
		t.Errorf("所有者应走 %q，实际 %q", securityProfileOwner, got)
	}
	if got := p.securityProfileFor(false); got != securityProfileGuest {
		t.Errorf("非所有者应走 %q，实际 %q", securityProfileGuest, got)
	}
	// 反向保护：两个档位名不能相同（相同则访客与所有者无差别）。
	if securityProfileOwner == securityProfileGuest {
		t.Fatal("owner 与 guest 档位名相同 —— 访客将获得所有者的工具面")
	}
	// 档位名必须以插件名开头（内核侧强制 `<owner>.<档位>`）。
	for _, n := range []string{securityProfileOwner, securityProfileGuest} {
		if len(n) < 4 || n[:3] != "qq." {
			t.Errorf("档位名 %q 必须以 \"qq.\" 开头（内核按 <owner>.<档位> 校验）", n)
		}
	}
}

// ② ★ 声明必须把 hard_private_tools 的清单**真正交给内核**。
//
// 若这里传空（或忘了传），访客档位就没有任何 DenyTools ——
// 表现为"声明成功、什么都不拦"，比报错更难发现。
func TestDeclareSecurityProfilesPassesHardPrivateList(t *testing.T) {
	want := []string{"calendar_*", "memory_*", "cmd_*"}
	p := &Plugin{
		hardPrivateToolsSet: true,
		hardPrivateTools:    want,
	}
	cap := &captureSecurityAPI{}
	p.declareSecurityProfiles(pluginWithSecurity(t, cap))

	if len(cap.profiles) != 2 {
		t.Fatalf("应声明 2 个档位（owner/guest），实际 %d 个: %+v", len(cap.profiles), cap.profiles)
	}

	var guest, owner *sdk.SecurityProfile
	for i := range cap.profiles {
		switch cap.profiles[i].Name {
		case securityProfileGuest:
			guest = &cap.profiles[i]
		case securityProfileOwner:
			owner = &cap.profiles[i]
		}
	}
	if guest == nil || owner == nil {
		t.Fatalf("缺少 owner 或 guest 档位: %+v", cap.profiles)
	}

	// ★ 核心断言：访客档位必须带上完整硬私有清单
	if len(guest.DenyTools) != len(want) {
		t.Fatalf("★ 访客档位的 DenyTools 应为 %v，实际 %v —— 清单没交给内核，访客档形同虚设",
			want, guest.DenyTools)
	}
	for i := range want {
		if guest.DenyTools[i] != want[i] {
			t.Fatalf("访客档位 DenyTools[%d] 应为 %q，实际 %q", i, want[i], guest.DenyTools[i])
		}
	}

	// 所有者档位**不额外收窄**（空 DenyTools 的语义）。
	if len(owner.DenyTools) != 0 {
		t.Fatalf("所有者档位不应有 DenyTools（零收窄），实际 %v", owner.DenyTools)
	}
}

// ③ 宿主未接声明面时不 panic、不放开权限。
//
// 旧内核没有 SecurityAPI：此时应保留 beforeToolcall 的自判（双轨过渡），
// 而不是"声明失败就当没有策略"。
func TestDeclareSecurityProfilesWithoutAPI(t *testing.T) {
	p := &Plugin{hardPrivateToolsSet: true, hardPrivateTools: []string{"cmd_*"}}
	// 不注入 SecurityAPI ⇒ Security() 返回 nil
	p.declareSecurityProfiles(&sdk.PluginSDK{})
	// 不 panic 即通过；行为退回旧自判（beforeToolcall 另有既有判据覆盖）。
}

// ④ 未显式配置硬私有清单时，用内置默认（安全默认，不放开）。
func TestDeclareSecurityProfilesUsesDefaultList(t *testing.T) {
	p := &Plugin{hardPrivateToolsSet: false} // 未配置
	cap := &captureSecurityAPI{}
	p.declareSecurityProfiles(pluginWithSecurity(t, cap))

	var guest *sdk.SecurityProfile
	for i := range cap.profiles {
		if cap.profiles[i].Name == securityProfileGuest {
			guest = &cap.profiles[i]
		}
	}
	if guest == nil {
		t.Fatal("缺少访客档位")
	}
	if len(guest.DenyTools) == 0 {
		t.Fatal("★ 未配置时访客档位不应为空 —— 那等于对访客完全放开私人资源")
	}
}
