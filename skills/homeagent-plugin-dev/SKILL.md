---
name: homeagent-plugin-dev
description: 开发、安装、调试 HomeAgent 插件。涉及 hmapdev 工具链、SDK 版本、plugin.bin 部署、插件注册排查时使用。触发词：HomeAgent 插件、hmapdev、plugin.bin、写插件、装插件、插件不生效、SDK 版本、RegisterTool、plugin_spawn。
---

# HomeAgent 插件开发与安装

> 本机（192.168.2.60）HomeAgent 的插件工具链手册。**所有命令都实测过**，
> 不是从文档抄的。改了本机环境后请回来更正。

## ★ 协议通道纪律：插件**绝不能**往 stdout 打印（高频踩坑）

子进程插件的 **stdout 是 JSON-RPC 协议通道**，**stderr 才是日志通道**
（SDK 侧已 `log.SetOutput(os.Stderr)`）。

```go
// ❌ 错误：写进 stdout ⇒ 内核收到裸文本 ⇒ 报「非法 JSON 帧」并**丢弃这行日志**
fmt.Printf("[%s] started\n", p.name)

// ✅ 正确：log 默认走 stderr，且会被内核收进插件日志
log.Printf("[%s] started", p.name)
```

**症状**（很容易误判成内核/协议故障）：

```
[proc] myplugin 收到非法 JSON 帧（19 字节）: invalid character 'm' looking for beginning of value
        ↑ 内容摘要: [myplugin] started
        可能原因：插件往 **stdout** 打印了日志，而 stdout 是JSON-RPC **协议通道**（stderr 才是日志通道）。
        修法：把插件里的 fmt.Printf/fmt.Println 改成 log.Printf（脚手架模板 1.4.0 起已修正）。
```

看到这条就说明**是你自己 printf 了** —— 日志本身丢了，还多一条看起来像协议故障的报错。

**实测代价**：生产环境累计 **1038 条**这种报错（weather 113 / rss 103 /
calendar 102 / 第三方 720），定位真因花了一整轮排查。

**例外**：**进程内插件**（`plugin.RegisterFactory`，编译进内核）的
`fmt.Println` 写的是内核真实终端，**不是**缺陷。判别方法：
插件源码是不是 `package main`（子进程）还是普通的库包（进程内）。

## 0. 先搞清三件事，别猜

```bash
hmapdev version        # 工具链版本 + SDK 模块 + 构建用 Go
hmapdev sdk current    # 当前活跃 SDK 版本
hmapdev sdk list       # 本机已装的所有 SDK 版本
```

**核心仓在** `/home/program/TrueAgent`（Go module `gitcode.com/JianFeeeee/HomeAgent`）。
**SDK 仓在** `third_party/homeagent-sdk`（独立 git 仓，独立版本号）。

## 1. 工具链：hmapdev

已装在 `/usr/local/bin/hmapdev`。这是插件开发的**唯一入口**。

| 命令 | 作用 |
| --- | --- |
| `hmapdev init <name>` | 生成 Go 插件脚手架 |
| `hmapdev init <name> --lua` | 生成 Lua 插件 |
| `hmapdev init <name> --type remotedevice` | 生成 C 语言远程设备适配器 |
| `hmapdev build` | 编译打包（`--outdir` / `--target os/arch`） |
| `hmapdev debug [dir]` | 解释执行 / 调试插件源码 |
| `hmapdev clean` | 清理 build/dist |
| `hmapdev sdk list/install/use/path/current/latest` | SDK 版本管理 |

⚠ **`hmapdev build` / `debug` 不支持 `--help`**：它们**不是**打印帮助，而是直接
去读当前目录的 `plg.json`，于是你会看到

    error: read plg.json: open plg.json: no such file or directory

这**不是故障**，只是没有 `--help`。要看 build 的可用 flag 用 `hmapdev --help`
（那里列出了 `--outdir` / `--target` / `--lua` / `--type`）。

## 2. ★ SDK 版本：最常见的坑

**插件编译时会校验 SDK 能力**，用**文档里的旧接口**生成的工程会直接构建失败，
报错形如「某能力需要更高版本 SDK」，并给出两条出路：

```bash
hmapdev sdk install <version>            # 1) 装对应版本
hmapdev sdk install --from <本地源码目录>   # 2) 直接用本地 SDK 源码
```

本机现状：`hmapdev sdk current` = **v1.4.0**，与核心仓 `internal/meta.Version` 一致。

⇒ **动手前先 `hmapdev sdk current`**，别照着旧文档写。
⇒ 核心仓有**新能力但 SDK 未跟上**时（本机发生过），用 `--from` 指向本地源码：
   `hmapdev sdk install --from /home/program/TrueAgent/third_party/homeagent-sdk`

## 3. 开发流程

```bash
mkdir -p /home/newqqagent/plugindev && cd /home/newqqagent/plugindev
hmapdev init myplugin
cd myplugin
#   编辑源码：注册工具用 s.RegisterTool(name, sdk.ToolDef{...}, handler)
hmapdev build                     # 产出 dist/myplugin_bundle.hmap
hmapdev debug .                   # 不想装就能先跑一遍
```

### ★ 产物形态（实测，别猜）

`hmapdev build` 产出的是 **`dist/<name>_bundle.hmap`**，它是 **ZIP**
（魔数 `PK`，用 `unzip` 而不是 `tar` 解），内含多平台二进制：

    plugin.json
    plugin.bin.linux.amd64
    plugin.bin.darwin.amd64
    README.md

而**生产上** `/home/newqqagent/plugins/<name>/plugin.bin` 是**解包后的单个二进制**
（**不是** zip）。⇒ 部署时要用对应平台的 `plugin.bin.<os>.<arch>`，
不要把 `.hmap` 直接丢进去。

### 注册工具的形状（照 example 写，别自创）

**★ `sdk.ToolDef` 里没有 `Handler` 字段** —— handler 是 `RegisterTool` 的**第三个参数**，
不是结构体成员。把 `Handler:` 写进结构体会**编译失败**（`unknown field Handler`）。

```go
s.RegisterTool(tp+"mytool", sdk.ToolDef{
    Name:        tp + "mytool",     // ← Name 必填，且要放在结构体**首位**
    Description: "……",
    Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
    NoMemory:    true,               // 声明字段一律放**末尾**，仿 NoMemory 的写法
}, h.mytool)                         // ← handler 在这里，不在结构体内
```

（`ToolDef` 的全部字段：`Name` / `Plugin` / `Description` / `Parameters` / `NoMemory` /
`Cleaner` / `ContextPolicy` / `RecallPolicy` / `ParallelSafe` / `Serial`。）

**并发声明**（同轮多个 tool_call 时）：

- `ParallelSafe: true` —— 可并发（**三个条件都满足才可声明**：handler 线程安全 /
  不与同批工具争抢同一资源 / 执行顺序无关）
- `Serial: true` —— 必须串行，**优先级高于 `ParallelSafe`**
- **默认（都不写）= 整批串行**，是保守设计不是遗漏

## 3.5 `plg.json` —— 插件工程标志

`hmapdev build` 找的就是它。缺失时报
`error: read plg.json: open plg.json: no such file or directory`。

以 `example/qq/plg.json` 为准：

```json
{
  "name": "qq",              // 插件名（英文，装到 plugins/<name>/ 用它）
  "name_zh": "QQ消息",       // 展示名
  "version": "1.4.1",
  "description": "……",
  "author": "HomeAgent",
  "entry": "plugin.so",      // 入口（build 会替换为实际产物）
  "tags": ["qq", "messaging"],
  "targets": "linux/amd64",  // 目标平台
  "outdir": "dist"           // 产物目录
}
```

## 4. 安装到生产

```bash
# ① 从 bundle 里取出本机平台的二进制
cd myplugin && unzip -o dist/myplugin_bundle.hmap 'plugin.bin.linux.amd64'
# ② 放进插件目录（<name> 要与 plg.json 的 name 一致）
sudo mkdir -p /home/newqqagent/plugins/myplugin
sudo cp plugin.json /home/newqqagent/plugins/myplugin/
sudo cp plugin.bin.linux.amd64 /home/newqqagent/plugins/myplugin/plugin.bin
sudo chmod +x /home/newqqagent/plugins/myplugin/plugin.bin
# ③ 重启并确认工具真的注册了
sudo systemctl restart homeagent.service
journalctl -u homeagent.service --since "-2 min" | grep "registering tool: <你的工具名>"
```

`plugin.json` 是**安装标志**（生产目录里没有它，插件不会被识别）。

★ **内置 vs 独立二进制**（目录存在 ≠ 有 plugin.bin）：

```bash
for p in webui qq cmd seq; do
  printf "%-8s " $p
  ls /home/newqqagent/plugins/$p/plugin.bin >/dev/null 2>&1 \
    && echo "独立（要单独构建部署）" || echo "内置（随 homed 部署）"
done
```

实测：`webui`/`cmd`/`seq` 内置，`qq` 独立。

## 5. 排查：插件装了却不生效

按这个顺序查，**别跳步**：

```bash
# 1) 内置还是独立？内置的改了源码必须重编 homed，不是重启就行
ls /home/newqqagent/plugins/<name>/plugin.bin

# 2) 工具注册了吗
journalctl -u homeagent.service --since "-5 min" | grep "registering tool:" | grep -i <name>

# 3) 插件加载了吗
journalctl -u homeagent.service --since "-5 min" | grep -E "loaded: <name>"

# 4) 启动时有没有报错
journalctl -u homeagent.service --since "-5 min" | grep -iE "<name>.*(error|panic|failed)"
```

★ **改了内置插件的源码 ⇒ 必须重新构建并部署 homed**，重启服务不生效。
内嵌资源走 `//go:embed`，是编译进二进制的。

★ 判断线上跑的是哪次构建，看 commit 字段（**不要用 `strings`** ——
`//go:embed` 的资源在旧二进制里也可能出现新内容）：

```bash
K=$(sqlite3 /home/newqqagent/config.db "select value from config_webui where key='api_key';")
curl -s -H "X-API-Key: $K" http://127.0.0.1:8080/api/v1/status | grep -oE '"commit":"[^"]*"'
```

⚠ 必须带 `X-API-Key`：无认证时返回 **200 + 登录页 HTML**，只看状态码会误判。

## 6. 参考资料在哪（别凭记忆写 API）

| 内容 | 路径 |
| --- | --- |
| SDK 源码 | `third_party/homeagent-sdk/sdk/` |
| **可运行的示例插件** | `third_party/homeagent-sdk/example/`（a2a / acp / qq / bili …） |
| 文档站 | `https://sdk.homeagent.jianfgit.xyz`（本地源 `docs/`，构建 `tools/apidoc/build.sh`） |
| 能力边界（哪些 API 外部可用） | `docs/guide/capability-boundary.md` |
| 并发声明 | `docs/guide/parallel-tool-declaration.md` |
| Lua 适配器写法 | `docs/guide/first-lua-plugin.md` |
| 部署手册 | 核心仓 `docs/zh/deploy-runbook.md` |

★ **写插件前先翻 `example/`**：那里的代码是**编译通过**的，
比文档更可靠。文档与示例冲突时以示例为准。

## 7. 边界（别越界）

- **不要手改生成的文档**：`docs/api/*.md` 由 `tools/apidoc/gensite` 生成，
  首行写着「请勿手改」；要改就去改 `sdk/*.go` 的注释再重新生成。
- **不要在插件里硬编码 token / 密钥**：走 `Settings()` 或宿主注入。

### 7.1 敏感配置的落盘加密（用 `SetSecret` / `GetSecret`）

`SettingsAPI` 提供两个方法让插件**无需自己实现加密**（主密钥由内核统一管理）：

```go
// 强制加密落盘（不看键名）—— 返回 error 表示内核没装主密钥，
// 此时**拒绝写入**而不是静默存明文；不要吞掉这个 error。
if err := s.Settings().SetSecret("blob", token); err != nil {
    return err
}
// 读回（透明解密）
token, _ := s.Settings().GetSecret("blob")
```

**两条自动加密规则**（不写 `SetSecret` 也会加密）：

1. `ConfigDef{Secret: true}` —— 推荐，意图最明确
2. `ConfigDef{Type: "password"}` —— 旧写法，兼容保留
3. 键名含 `password` / `secret` / `token` / `api_key` 等词段 —— 兜底

⚠️ 什么时候**必须**用 `SetSecret`：键名是中性的（如 `blob`、`conn`、`payload`），
键名模式猜不到 —— 那就显式声明。

⚠️ 不要自己实现加密：主密钥由内核统一管理（`<dataDir>/security.key` 或
`HOMED_SECURITY_KEY`），插件各自持钥会让密钥散落、无法轮换。

**注意**：`Secret` 字段在旧版本里就已存在但**从未生效**（内核侧的转换漏了它，
见内核仓 `docs/zh/kernel-secret-firewall-design.md` §5.1）。若你的插件声明了
`Secret: true` 却看到明文落盘，说明内核侧还没接通 —— 升级内核，或改用
`SetSecret`（若该方法不存在，就是内核版本过旧）。

⚠️ **升级须知**：`SetSecret`/`GetSecret` 是**接口新增方法**。它们只增不减，
所以插件**消费**侧不受影响；但如果你自己实现了 `SettingsAPI`（如自测替身），
需要补上这两个方法。
- **不要为了让插件生效去改 `homed` 的源码** —— 先确认它是不是内置插件。
