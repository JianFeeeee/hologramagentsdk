package main

// 远端端点表的判据。
//
// 核心要证明的**不是**"有个 endpoints.json"，而是：
//   ① 多台机器**互不干扰**（这是用户提出该需求的唯一理由）；
//   ② 本机路径**绝不**下发到远端（这是最危险的真实缺陷）；
//   ③ 同一机器上不同后端可以各有各的路径；
//   ④ 出错时错误信息**指出该改哪里**（否则用户只能猜）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// newEndpointPlugin 造一个带端点表的插件（数据目录在临时目录）。
func newEndpointPlugin(t *testing.T) *Plugin {
	t.Helper()
	sdkp := sdk.New("commondev-test", nil, nil, nil, nil, nil)
	p := &Plugin{sdk: sdkp}
	dir := t.TempDir()
	p.path = filepath.Join(dir, "ledger.json")
	if err := p.loadEndpoints(); err != nil {
		t.Fatal(err)
	}
	return p
}

// ★★★ 判据：两台机器的配置**互不干扰**。
//
// 这是用户提出该需求的原因：改动前只有一组全局 settings
// （ssh_node_path / ssh_runtime_root / ssh_workdir_root），两台机器时
// 后配的覆盖前一台，表现为"换台机器就报找不到 node/脚本"，
// 而报错指向的是**另一台机器的路径**。
func TestEndpointsArePerHost(t *testing.T) {
	p := newEndpointPlugin(t)

	set := func(host string, kv map[string]interface{}) {
		t.Helper()
		args := map[string]interface{}{"host": host, "verify": false}
		for k, v := range kv {
			args[k] = v
		}
		r, _ := p.handleEndpointSet(args)
		if m := r.(map[string]interface{}); m["error"] != nil {
			t.Fatalf("设置 %s 失败: %v", host, m["error"])
		}
	}

	set("alpha", map[string]interface{}{
		"cli_path": "/opt/alpha/bin/zcode.cjs", "node_path": "/usr/local/bin/node",
		"runtime_root": "/home/a/rt", "workdir_root": "/home/a/prog",
	})
	set("beta", map[string]interface{}{
		"cli_path": "/srv/beta/zcode.mjs", "node_path": "/opt/node/bin/node",
		"runtime_root": "/srv/rt", "workdir_root": "/srv/prog",
	})

	// ★ 每台机器解析出的都必须是**自己**那套
	drv := driverSpec{}
	ctx := driverCtx{}

	ea := p.resolveEndpoint("alpha", "zcode", drv, ctx)
	if ea.CLIPath != "/opt/alpha/bin/zcode.cjs" {
		t.Errorf("alpha 的 cliPath 被串了: %q", ea.CLIPath)
	}
	if ea.NodePath != "/usr/local/bin/node" || ea.RuntimeRoot != "/home/a/rt" {
		t.Errorf("alpha 的 node/runtime 被串了: %+v", ea)
	}
	if ea.WorkdirRoot != "/home/a/prog" {
		t.Errorf("alpha 的 workdirRoot 被串了: %q", ea.WorkdirRoot)
	}

	eb := p.resolveEndpoint("beta", "zcode", drv, ctx)
	if eb.CLIPath != "/srv/beta/zcode.mjs" {
		t.Errorf("beta 的 cliPath 被串了: %q", eb.CLIPath)
	}
	if eb.NodePath != "/opt/node/bin/node" || eb.RuntimeRoot != "/srv/rt" {
		t.Errorf("beta 的 node/runtime 被串了: %+v", eb)
	}
	if eb.WorkdirRoot != "/srv/prog" {
		t.Errorf("beta 的 workdirRoot 被串了: %q", eb.WorkdirRoot)
	}

	// 未配置的第三台：不能"借用"别人的值
	ec := p.resolveEndpoint("gamma", "zcode", drv, ctx)
	if ec.CLIPath != "" {
		t.Errorf("★ gamma 未配置却拿到了别人的 cliPath: %q", ec.CLIPath)
	}
}

// ★★★ 判据：同一机器上**不同后端各有各的路径**。
//
// 这是需求里"每个 cli 客户端有自己的 ip/cli 对"的一半：
// 同一台机器上 zcode 在 A、别的 CLI 在 B，必须能分别指定。
func TestEndpointsPerHostAndCLI(t *testing.T) {
	p := newEndpointPlugin(t)

	// 机器级默认
	if r, _ := p.handleEndpointSet(map[string]interface{}{
		"host": "box", "cli_path": "/usr/local/bin/default-cli", "verify": false,
	}); r.(map[string]interface{})["error"] != nil {
		t.Fatalf("设机器默认失败: %v", r)
	}
	// zcode 专属（应覆盖机器默认）
	if r, _ := p.handleEndpointSet(map[string]interface{}{
		"host": "box", "cli": "zcode", "cli_path": "/opt/zcode/zcode.cjs",
		"node_path": "/opt/node/bin/node", "verify": false,
	}); r.(map[string]interface{})["error"] != nil {
		t.Fatalf("设 zcode 专属失败: %v", r)
	}

	drv := driverSpec{}
	ctx := driverCtx{}

	z := p.resolveEndpoint("box", "zcode", drv, ctx)
	if z.CLIPath != "/opt/zcode/zcode.cjs" {
		t.Errorf("★ zcode 专属路径未覆盖机器默认: %q", z.CLIPath)
	}
	if z.NodePath != "/opt/node/bin/node" {
		t.Errorf("zcode 专属 node 未生效: %q", z.NodePath)
	}
	// 另一个后端：应拿到**机器默认**，而不是 zcode 的
	other := p.resolveEndpoint("box", "othercli", drv, ctx)
	if other.CLIPath != "/usr/local/bin/default-cli" {
		t.Errorf("★ 其他后端应回落到机器默认: %q", other.CLIPath)
	}
	if other.NodePath != "node" {
		t.Errorf("zcode 的 node 不该泄漏给其他后端: %q", other.NodePath)
	}
	// 解析来源必须可读（错误信息靠它指出该改哪里）
	if !strings.Contains(z.CLIPathFrom, "专属") {
		t.Errorf("CLIPathFrom 未标明来源: %q", z.CLIPathFrom)
	}
	if !strings.Contains(other.CLIPathFrom, "host") {
		t.Errorf("回落时应标明来源是 host 默认: %q", other.CLIPathFrom)
	}
}

// ★★★ 判据：**本机路径不得下发到远端**（本会话最危险的真实缺陷）。
//
// 改动前 `drv.buildArgs(ctx)` 会把 driver.json 的 `cliPath`
// （随包默认是 `{rawfile}/zcode/zcode.cjs`）展开成**本机**路径塞进 `--cli`，
// 然后整条命令发到远端 ⇒ 远端必然报 "No such file"，而用户会去
// **远端**找原因，真正的错在本地。
func TestRemoteCommandDoesNotCarryLocalPaths(t *testing.T) {
	ctx := driverCtx{
		Workdir: "/w", SessionID: "s", SessionName: "n",
		DataDir: "/d", PluginDir: "/p", RawfileDir: "/rf",
	}
	drv := driverSpec{
		Args:    []string{"--workdir", "{workdir}", "--session-id", "{sessionId}"},
		CLIPath: "{rawfile}/zcode/zcode.cjs", // ★ 本机占位符
	}

	// ① 端点表里配了远端真实路径 ⇒ 命令里只能出现远端那条
	ep := resolvedEndpoint{CLIPath: "/opt/remote/zcode.cjs"}
	_, args, _ := buildRemoteCommandFor(drv, ctx, "/usr/bin/node", ep)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "/rf/") || strings.Contains(joined, "{rawfile}") {
		t.Errorf("★ 远端命令里出现了**本机**路径: %v", args)
	}
	if !strings.Contains(joined, "/opt/remote/zcode.cjs") {
		t.Errorf("★ 远端命令里没有远端 CLI 路径: %v", args)
	}

	// ② 端点表没配的机器：必须在**启动 ssh 之前**响亮拒绝，
	//    而不是把本机路径发过去让远端报错。
	//
	//    判定依据是**占位符**（本机专属、展开后必然是本机路径），
	//    不是"任意看起来像本机的绝对路径"—— 后者在 localhost/同构机器上
	//    是合法的，强制拒绝会挡住真实用法。所以这里用占位符版本断言。
	errPH := validateRemoteCLIPath("{rawfile}/zcode/zcode.cjs", "zcode")
	if errPH == nil {
		t.Error("★ 含本机占位符的路径必须拒绝（展开后就是本机路径）")
	} else if !strings.Contains(errPH.Error(), "本机") {
		t.Errorf("错误应点明本机占位符问题: %v", errPH)
	}
	// 未配路径（空）必须拒绝并告知去哪儿配
	if err := validateRemoteCLIPath("", "zcode"); err == nil {
		t.Error("★ 未配远端路径时应拒绝，而不是把值发过去")
	}
	// 完全没配也拒绝，且**告知去哪儿配**
	err := validateRemoteCLIPath("", "zcode")
	if err == nil {
		t.Fatal("未配路径应拒绝")
	}
	for _, want := range []string{"endpoint_set", "cli_path", "zcode"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误应含 %q 以便用户知道怎么修: %v", want, err)
		}
	}
	// 远端真实路径应通过
	if err := validateRemoteCLIPath("/opt/remote/zcode.cjs", "zcode"); err != nil {
		t.Errorf("远端真实路径不该被拒: %v", err)
	}
}

// ★★★ 判据：**声明了本机路径要当场拒绝并说明原因**。
func TestEndpointSetRejectsLocalPath(t *testing.T) {
	p := newEndpointPlugin(t)
	// 构造一个"看起来是本机"的路径：用本机真实存在的根
	hint := currentLocalRootHint()
	if hint == "" {
		t.Skip("无法确定本机根，跳过")
	}
	local := filepath.Join(hint, "some", "zcode.cjs")

	r, _ := p.handleEndpointSet(map[string]interface{}{
		"host": "box", "cli_path": local, "verify": false,
	})
	m := r.(map[string]interface{})
	if m["error"] == nil {
		t.Fatalf("★ 应拒绝本机路径: %s", local)
	}
	if !strings.Contains(fmtStr(m["error"]), "本机") {
		t.Errorf("错误应点明是本机路径: %v", m["error"])
	}
	// 占位符同样拒绝
	r2, _ := p.handleEndpointSet(map[string]interface{}{
		"host": "box", "cli_path": "{rawfile}/z/x.cjs", "verify": false,
	})
	if r2.(map[string]interface{})["error"] == nil {
		t.Error("★ 应拒绝含 {rawfile} 的路径")
	}
	// 远端真实路径应接受
	r3, _ := p.handleEndpointSet(map[string]interface{}{
		"host": "box", "cli_path": "/opt/remote/zcode.cjs", "verify": false,
	})
	if e := r3.(map[string]interface{})["error"]; e != nil {
		t.Errorf("远端路径不该被拒: %v", e)
	}
}

// ★★★ 判据：删除**不能留下幽灵子项**。
//
// 否则会出现"以为删了机器，实际后端覆盖还在，下次又把老路径拿出来用"。
func TestEndpointRemoveRefusesSilentPartial(t *testing.T) {
	p := newEndpointPlugin(t)
	p.handleEndpointSet(map[string]interface{}{"host": "box", "cli_path": "/opt/d", "verify": false})
	p.handleEndpointSet(map[string]interface{}{"host": "box", "cli": "zcode", "cli_path": "/opt/z", "verify": false})

	// 只给 host：必须拒绝（有子项）
	r, _ := p.handleEndpointRemove(map[string]interface{}{"host": "box"})
	m := r.(map[string]interface{})
	if m["status"] != "refused" {
		t.Fatalf("★ 有后端覆盖时应拒绝整体删除: %v", m)
	}
	// 拒绝后表必须**完整保留**（不能删一半）
	he := p.getHost("box", false)
	if he == nil || he.CLIPath != "/opt/d" || len(he.CLIs) != 1 {
		t.Fatalf("拒绝后配置被改动了: %+v", he)
	}

	// 先删子项，再删机器 → 成功
	if r, _ := p.handleEndpointRemove(map[string]interface{}{"host": "box", "cli": "zcode"}); r.(map[string]interface{})["removed"] != true {
		t.Fatalf("删子项失败: %v", r)
	}
	if r, _ := p.handleEndpointRemove(map[string]interface{}{"host": "box"}); r.(map[string]interface{})["removed"] != true {
		t.Fatalf("删机器失败: %v", r)
	}
	if p.getHost("box", false) != nil {
		t.Error("机器未真正删除")
	}

	// force 可以一并删
	p.handleEndpointSet(map[string]interface{}{"host": "box2", "cli_path": "/opt/d", "verify": false})
	p.handleEndpointSet(map[string]interface{}{"host": "box2", "cli": "zcode", "cli_path": "/opt/z", "verify": false})
	if r, _ := p.handleEndpointRemove(map[string]interface{}{"host": "box2", "force": true}); r.(map[string]interface{})["removed"] != true {
		t.Fatalf("force 删除失败: %v", r)
	}
	if p.getHost("box2", false) != nil {
		t.Error("force 未连带删掉子项")
	}
}

// ★★★ 判据：端点表**持久化且损坏响亮**。
//
// 静默用空表 ⇒ 所有远端配置凭空消失，表现为"我明明配过，怎么又要配"，
// 而用户会先怀疑自己。
func TestEndpointsPersistAndCorruptionIsLoud(t *testing.T) {
	dir := t.TempDir()
	sdkp := sdk.New("commondev-test", nil, nil, nil, nil, nil)
	p := &Plugin{sdk: sdkp}
	p.path = filepath.Join(dir, "ledger.json")
	if err := p.loadEndpoints(); err != nil {
		t.Fatal(err)
	}
	p.handleEndpointSet(map[string]interface{}{
		"host": "h1", "cli_path": "/opt/a", "runtime_root": "/rt/a",
		"workdir_root": "/wd/a", "verify": false,
	})

	// 重新加载（模拟重启）
	p2 := &Plugin{sdk: sdkp}
	p2.path = filepath.Join(dir, "ledger.json")
	if err := p2.loadEndpoints(); err != nil {
		t.Fatalf("重载失败: %v", err)
	}
	got := p2.resolveEndpoint("h1", "zcode", driverSpec{}, driverCtx{})
	if got.CLIPath != "/opt/a" || got.RuntimeRoot != "/rt/a" || got.WorkdirRoot != "/wd/a" {
		t.Fatalf("重启后配置丢失: %+v", got)
	}

	// 权限 0600（可能含内网地址/目录结构，不该 world-readable）
	st, err := os.Stat(p.endpointsPath())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("端点表权限应为 0600，实际 %v", st.Mode().Perm())
	}

	// 损坏 → 必须响亮
	if err := os.WriteFile(p.endpointsPath(), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	p3 := &Plugin{sdk: sdkp}
	p3.path = filepath.Join(dir, "ledger.json")
	if err := p3.loadEndpoints(); err == nil {
		t.Fatal("★ 端点表损坏时必须报错（静默用空表 = 所有远端配置消失）")
	} else if !strings.Contains(err.Error(), "损坏") {
		t.Errorf("错误应点明损坏: %v", err)
	}

	// 缺文件不是错误（首次运行）
	p4 := &Plugin{sdk: sdkp}
	p4.path = filepath.Join(t.TempDir(), "ledger.json")
	if err := p4.loadEndpoints(); err != nil {
		t.Errorf("缺文件不该报错: %v", err)
	}
	if p4.endpoints == nil || p4.endpoints.Hosts == nil {
		t.Error("缺文件后应初始化为空表（不是 nil）")
	}
}

// ★★★ 判据：旧配置（全局 settings）**仍然生效**（升级不破坏既有部署）。
//
// 但端点表**优先**：不能出现"配了端点表还被旧全局键盖住"。
func TestEndpointOverridesLegacySettings(t *testing.T) {
	p := newEndpointPlugin(t)
	// 用 stubSettings 提供旧键
	p.sdk = sdk.New("commondev-test", newStubSettingsWith(t.TempDir(), map[string]interface{}{
		"ssh_runtime_root": "/legacy/rt",
		"ssh_workdir_root": "/legacy/wd",
		"ssh_node_path":    "/legacy/node",
	}), nil, nil, nil, nil)

	// ① 旧键生效（升级不破坏）
	e := p.resolveEndpoint("h", "zcode", driverSpec{}, driverCtx{})
	if e.RuntimeRoot != "/legacy/rt" || e.WorkdirRoot != "/legacy/wd" || e.NodePath != "/legacy/node" {
		t.Fatalf("旧全局配置应仍生效: %+v", e)
	}

	// ② 端点表配了 → 必须压过旧键
	p.handleEndpointSet(map[string]interface{}{
		"host": "h", "runtime_root": "/new/rt", "workdir_root": "/new/wd",
		"node_path": "/new/node", "verify": false,
	})
	e2 := p.resolveEndpoint("h", "zcode", driverSpec{}, driverCtx{})
	if e2.RuntimeRoot != "/new/rt" {
		t.Errorf("★ 端点表未压过旧键: %q", e2.RuntimeRoot)
	}
	if e2.WorkdirRoot != "/new/wd" {
		t.Errorf("★ 端点表未压过旧 ssh_workdir_root: %q", e2.WorkdirRoot)
	}
	if e2.NodePath != "/new/node" {
		t.Errorf("★ 端点表未压过旧 ssh_node_path: %q", e2.NodePath)
	}
}

func fmtStr(v interface{}) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(strings.ReplaceAll(strings.TrimSpace(toString(v)), "\n", " "))
}

func toString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
