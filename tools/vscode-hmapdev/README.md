# hmapdev — HologramAgent 插件开发 VSCode 扩展

调试与构建 HologramAgent 插件工程的 IDE 支持：**plg.json 校验、SDK 版本解析、构建/运行、内核日志跟随**。

## 为什么需要它

插件的真实形态是「**独立子进程 + 内核侧握手**」，所以插件的三类问题几乎都在 IDE 之外发生：

1. **编不出来** —— 最常见的原因不是代码，而是项目没声明要用哪版 SDK，工具链拿了存储里的
   `current`（可能是陈旧的 `v0.8.0`），于是报一堆看不懂的 `undefined: sdk.XXX`；
2. **编出来但起不来** —— 产物与内核**协议绑定**（协议版本 + 共享内存魔数），用错工具链编出来的
   插件会在握手时被拒；
3. **起来了但行为不对** —— 真因往往只在内核日志里（建链失败、崩溃重启、工具报错）。

本扩展把这三件事拉进 IDE：**先把「用哪版 SDK」摆到明面上**，再让构建/运行/看日志变成一条动作链。

## 功能

| 功能 | 说明 |
|---|---|
| **plg.json 诊断** | 必需字段；`sdk` 必须是**完整版本号**（区间写法 `1.2` 会报错并说明「patch 位恒为 .0」）；声明的 SDK 若未安装在本地存储，直接给出 `hmapdev sdk install vX.Y.Z` |
| **状态栏** | `插件 · SDK <声明> · hmapdev <版本>`；工具链缺失或工程有错时变红/黄，tooltip 列出已装 SDK |
| **构建 / 清理 / 运行** | `hmapdev build`、`build --target all`、`clean`、`debug`（解释执行，快速迭代）——在集成终端里跑，可 Ctrl-C |
| **任务（Tasks）** | 同一批动作注册为 `hmapdev` 任务，可绑快捷键、串依赖；带 **Go 问题匹配器**，编译错误进 Problems 面板 |
| **跟随内核日志** | 读 `<dataDir>/log` 下最新的 `homed_*.log`，按插件名过滤后持续输出（真正的联调回路） |
| **SDK 管理** | 查看工具链版本、列出/安装/切换 SDK 版本（走 QuickPick，不用记命令） |
| **JSON 支持** | `plg.json` 的 schema 校验 + 骨架片段 |

## 安装

```bash
cd tools/vscode-hmapdev
npm install
npm run compile
```

然后二选一：

- **开发模式**：在 VSCode 里打开本目录，按 `F5`（Extension Development Host），把插件工程目录作为工作区打开；
- **安装到本机**：`npx @vscode/vsce package` 生成 `.vsix`，再 `code --install-extension hmapdev-vscode-0.1.0.vsix`。

前提：`hmapdev` 在 `PATH` 上（或设置 `hmapdev.path`）。

## 配置

| 设置 | 默认 | 说明 |
|---|---|---|
| `hmapdev.path` | `hmapdev` | 工具链可执行文件路径 |
| `hmapdev.kernelDataDir` | 空 | 内核数据目录（`homed -data` 的那个）；填了才能跟随内核日志 |
| `hmapdev.diagnoseSdk` | `true` | 是否校验声明的 SDK 是否已安装（需要能执行 hmapdev） |

## 用法（典型开发回路）

1. 打开插件工程（含 `plg.json`）→ 状态栏出现 `插件 · SDK <版本> · hmapdev <版本>`；
2. 若 `sdk` 报错（未声明 / 区间写法 / 未安装）→ 按提示执行 `hmapdev: 安装 SDK 版本…`，再 `hmapdev: 刷新状态`；
3. `hmapdev: 构建插件`（或 `构建（全部目标平台）`）→ 编译错误直接进 Problems；
4. 快速验证行为：`hmapdev: 运行插件（解释执行）`；
5. 与内核联调：设置 `hmapdev.kernelDataDir` → `hmapdev: 跟随内核日志`，只看本插件的行；
6. 改代码 → 重复 3/5。装进内核时记得**与内核同批替换**（协议绑定的产物不支持滚动升级）。

## 诚实的边界

- **这不是源码级调试器**：没有断点/单步。插件的 Go 代码要么编译成产物在内核里跑、要么用
  `hmapdev debug`（yaegi 解释执行）跑，两条路都不提供 DAP 调试会话。本扩展做的是
  「构建 + 运行 + 看内核日志 + 清单校验」，这也是插件问题实际能被定位的方式。
- **Windows 目标**：不支持（协议 2 的统一共享内存区未移植到 Windows，内核侧改走 WSL2），
  扩展只给提示，不假装能构建。
- **`sdk` 字段的语义**：它声明的是**本插件针对的 SDK 版本**（= 接口线），不是内核版本。
  SDK 版本跟随内核中版本、patch 位恒为 `.0`。
