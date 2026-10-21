#!/usr/bin/env node
// ZCode sidecar：把 **ZCode CLI** 包成与 pi sidecar **同一个 JSON-RPC 契约**的服务。
//
// ## 为什么必须同契约
//
// 插件的 `startSidecar` / `sc.call("prompt")` 是唯一的调用面 ——
// 只要这个脚本实现 ping/prompt/state/close，**插件核心一行不用改**
// 就能跑 ZCode。这就是「通过不同 sidecar 支持不同 CLI」的落点。
//
// ## 与 pi sidecar 的关键差异（实测契约，来自 agentmail/zcode-mail-bridge）
//
//	pi sidecar              ZCode sidecar
//	────────────────────    ──────────────────────────────
//	常驻进程 + prompt()     **每轮起一个新进程**（CLI 是一次性的）
//	--session-id 建会话     --resume <id> 延续；首轮不给
//	SDK 直接返回文本        --output-format stream-json，末行 {type:"result"}
//	无授权概念              --mode **必传**（不传 = yolo，绕过全部授权询问）
//
// ## 会话 id 的处理（★ 与插件的契约）
//
// 插件在 prompt 后**断言 session_id 未变**（防会话漂移）。
// ZCode 首轮才产生真实 sessionId，所以这里：
//   · 首轮：把 CLI 返回的 sess_xxx 回报（插件接受它作为会话标识）
//   · 后续：用 --resume 传回去，并回报同一个 id
//
// ## 超时必须杀进程树
//
// ZCode 会派生工具子进程（bash 等）。只杀父进程会留下一堆孤儿继续跑，
// 而且它们仍占着工作目录。

import { spawn } from "node:child_process";
import { createInterface } from "node:readline";
import { existsSync, mkdirSync, writeFileSync, readFileSync } from "node:fs";
import path from "node:path";

function parseArgs(argv) {
  const out = {};
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === "--workdir") out.workdir = argv[++i];
    else if (a === "--session-id") out.sessionId = argv[++i];
    else if (a === "--session-name") out.sessionName = argv[++i];
    else if (a === "--cli") out.cliPath = argv[++i];
    else if (a === "--mode") out.mode = argv[++i];
    else if (a === "--session-dir") out.sessionDir = argv[++i];
  }
  return out;
}

const args = parseArgs(process.argv.slice(2));
const workdir = args.workdir || process.cwd();

// ★ mode 必须显式给：不传时 CLI 默认 **yolo**（源码 src/run.ts:
// DEFAULT_HEADLESS_PROMPT_MODE），那会绕过全部授权询问。
// 合法值：build | edit | plan | yolo | guarded。默认用 guarded。
const VALID_MODES = ["build", "edit", "plan", "yolo", "guarded"];
const mode = args.mode || process.env.ZCODE_MODE || "guarded";
if (!VALID_MODES.includes(mode)) {
  throw new Error(`--mode 必须是 ${VALID_MODES.join("|")} 之一（收到 ${mode}）`);
}

// ★★ 会话库落到**本工作区**（而不是用户全局 ~/.zcode/cli/db）。
//
// 实测（2026-10-16，真机）：
//   · ZCode 的会话库路径**硬编码用 homedir()**：
//     apps/zcode-cli/packages/adapters/src/storage/session-store/paths.ts
//     `join(homedir(), ".zcode", "cli", "db", "db.sqlite")`
//     ⇒ `ZCODE_DATA_BASE_DIR` **对它完全无效**（我们曾以为那个变量能隔离会话，
//       实测证明不能：26 个会话全落在全局库里）。
//   · 但 `<工作目录>/.zcode/config.json` 的 `storage.sessionDbPath`
//     **确实生效**（实测：指向新路径后新建了库，全局库不再被写）。
//
// 所以这里在启动前写一份工作区配置，把会话库收进本工作区 ——
// 一举解决三件事：
//   ① 每条工程线的会话互相隔离（多线并行时不会共用一份历史）
//   ② 不再污染用户的全局 ~/.zcode
//   ③ 台账删掉工作目录时，会话也跟着走（不会有孤儿会话留在全局库里）
//
// ★ 只在**不存在**时写：用户若手工放了 config.json（例如想指向现有全局库
// 以续谈历史），必须尊重它 —— 覆盖会让人"配了没生效"。
function ensureWorkspaceSessionStore() {
  const zdir = path.join(workdir, ".zcode");
  const cfgPath = path.join(zdir, "config.json");
  if (existsSync(cfgPath)) return { path: cfgPath, written: false };
  try {
    mkdirSync(zdir, { recursive: true });
    const db = path.join(workdir, args.sessionDir || ".zcode-sessions", "sessions.sqlite");
    mkdirSync(path.dirname(db), { recursive: true });
    writeFileSync(cfgPath, JSON.stringify({ storage: { sessionDbPath: db } }, null, 2) + "\n");
    return { path: cfgPath, written: true };
  } catch (e) {
    // 写不了不该让整轮失败：退回全局库仍能工作（只是隔离性丢失）。
    return { path: cfgPath, written: false, error: String(e && e.message || e) };
  }
}

// CLI 入口：驱动可经 --cli 指定，否则按常见安装位置找。
//
// ★ 返回 {path, tried} 而不是裸字符串：**必须知道路径是"声明的"还是"探测到的"**，
// 才能在报错里说清楚该改哪里。
//
// 实测真缺陷（2026-10-16）：随包 driver.json 声明了 `{rawfile}/zcode/zcode.cjs`，
// 但包里**没有**这个文件（28MB，按需分发）。旧实现 `if (args.cliPath) return args.cliPath`
// 直接把它当有效路径返回 ⇒ Node 在 spawn 时抛模块解析失败，用户看到的是一坨
// `Module._resolveFilename` 堆栈，**完全看不出"CLI 不存在"**，更不知道去哪儿配。
// 声明的路径同样必须验存。
function findCLI() {
  const cands = [
    { p: args.cliPath, source: "driver.json 的 cliPath" },
    { p: process.env.ZCODE_CLI, source: "环境变量 ZCODE_CLI" },
    { p: path.join(process.env.HOME || "", ".zcode/runtime/bin/zcode.mjs"), source: "默认探测" },
    { p: "/opt/ZCode/resources/glm/zcode.cjs", source: "默认探测" },
    { p: "/usr/local/bin/zcode", source: "默认探测" },
  ];
  const tried = [];
  for (const c of cands) {
    if (!c.p) continue;
    tried.push(`${c.p}（${c.source}）`);
    if (existsSync(c.p)) return { path: c.p, tried };
  }
  return { path: "", tried };
}

// ★ 必须区分两个不同的东西：
//
//   desiredSessionId  —— 插件传进来的**标签**（br-<project>-<branch>），
//                        它是插件台账里的会话名，**不是** ZCode 的会话 id
//   zcodeSessionId    —— ZCode 真实产出的 `sess_...`，只有它能用于 --resume
//
// 初版把 args.sessionId 直接当成 --resume 的目标，于是**首轮就带上
// `--resume br-xxx`**，ZCode 报 `Session not found: br-xxx` ——
// 表现为"第一次投递必定失败"，而第二次才可能成功（如果插件把
// 上一轮返回的 sess_... 传回来了）。
//
// 实测（2026-10-16）：真实 ZCode 0.16.9 上首轮即失败。
// 契约（参考实现在注释里也写了）：**首轮不给 --resume**，
// 从 `{type:"result"}` 行取回 sess_...，之后才用它续谈。
let desiredSessionId = args.sessionId || "";
let zcodeSessionId = "";
let turnCount = 0;
// inflight 是**已受理但未完成**的 prompt 数（异步投递后用于观测）
let inflight = 0;

function send(obj) {
  process.stdout.write(JSON.stringify(obj) + "\n");
}

/** 杀**进程树**（ZCode 会派生工具子进程）。 */
function killTree(child) {
  if (!child || child.killed) return;
  try {
    // 负号 = 进程组；spawn 时 detached 建了独立进程组。
    process.kill(-child.pid, "SIGTERM");
  } catch {
    try { child.kill("SIGTERM"); } catch {}
  }
  setTimeout(() => {
    try { process.kill(-child.pid, "SIGKILL"); } catch {}
  }, 3000).unref?.();
}

/** 跑一轮 ZCode，返回 {sessionId, response}。 */
// 启动即准备（幂等）。放在最外层执行一次，不放进 runTurn：
// 重复写配置没意义，且每轮都 stat 一次没必要。
const sessionStore = ensureWorkspaceSessionStore();

function runTurn(text, timeoutMs) {
  return new Promise((resolve, reject) => {
    const found = findCLI();
    if (!found.path) {
      reject(new Error(
        "找不到 ZCode CLI。已找过：\n  - " + found.tried.join("\n  - ") +
          "\n请把 driver.json 的 cliPath 指向真实 CLI（本机覆盖写到 " +
          "<插件数据>/runtime/zcode/driver.json），或设 ZCODE_CLI 环境变量。"));
      return;
    }
    const argv = [
      found.path,
      "--prompt", text,
      "--output-format", "stream-json",
      "--cwd", workdir,
      "--mode", mode,
    ];
    // ★ 会话延续：首轮不给 --resume（还没有 id），后续都给。
    // ★ 只有拿到 ZCode 真实 id 才 resume；首轮没有 → 不传。
    if (zcodeSessionId) argv.push("--resume", zcodeSessionId);

    const child = spawn(process.execPath, argv, {
      cwd: workdir,
      detached: true, // 独立进程组 ⇒ 能整组杀
      stdio: ["ignore", "pipe", "pipe"],
    });
    currentChild = child;

    let out = "";
    let err = "";
    let settled = false;
    const timer = setTimeout(() => {
      if (settled) return;
      settled = true;
      killTree(child);
      if (currentChild === child) currentChild = null;
      reject(new Error(`ZCode 回合超时（${Math.round(timeoutMs / 1000)}s）`));
    }, timeoutMs);

    child.stdout.on("data", (d) => { out += d.toString(); });
    child.stderr.on("data", (d) => { err += d.toString(); });

    child.on("error", (e) => {
      if (currentChild === child) currentChild = null;
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      reject(e);
    });
    child.on("close", (code) => {
      // 清空句柄（无论是否已 settled）：否则 interrupt 会拿到一个已死的
      // 子进程去杀，或更糟 —— 误杀掉**下一轮**的进程。
      if (currentChild === child) currentChild = null;
      if (settled) return;
      settled = true;
      clearTimeout(timer);

      // 逐行解析 stream-json：事件行随时来，末行是 {type:"result"}
      let result = null;
      let events = 0;
      for (const line of out.split("\n")) {
        const t = line.trim();
        if (!t || t[0] !== "{") continue;
        let obj;
        try { obj = JSON.parse(t); } catch { continue; }
        if (obj && obj.type === "result") result = obj;
        else events++;
      }
      if (!result) {
        reject(new Error(
          `ZCode 未返回 result 行（exit=${code}，事件 ${events} 条）` +
            (err ? `；stderr: ${err.slice(-400)}` : "")));
        return;
      }
      resolve({
        sessionId: typeof result.sessionId === "string" && result.sessionId
          ? result.sessionId
          : zcodeSessionId,
        response: typeof result.response === "string" ? result.response : "",
        events,
      });
    });

    // 供 close 时中断当前回合
    currentChild = child;
  });
}

let currentChild = null;

async function handle(method, params) {
  switch (method) {
    case "ping": {
      // 不启动 CLI：握手要快且确定（10s 上限）。
      // 报出 CLI 版本 —— 插件的 ping 处理会认 pi_version/cli_version/version 三者之一。
      return {
        ok: true,
        cwd: workdir,
        // 握手阶段还没有 ZCode 会话；报插件给我们的标签即可
        // （插件侧台账用它，真实 sess_... 在首轮 prompt 后才产生）。
        session_id: zcodeSessionId || desiredSessionId || null,
        cli_version: process.env.ZCODE_VERSION || null,
        session_name: args.sessionName || null,
        backend: "zcode",
      };
    }
    case "prompt": {
      const text = params?.text ?? "";
      // ★ params.session_id 是**插件的标签**（br-xxx），不是 ZCode 的会话 id。
      // 绝不能拿它当 --resume 目标 —— 那正是首轮必然失败的根因。
      // 只在还没有真实 id 时记下它，供握手/state 回报用。
      if (params?.session_id && !zcodeSessionId) {
        desiredSessionId = String(params.session_id);
      }
      const timeoutMs = Number(params?.timeout_ms) > 0
        ? Number(params.timeout_ms)
        : 20 * 60 * 1000;

      // ★★ 前置校验：CLI 找不到必须**在受理阶段**就报错。
      //
      // 异步投递后，runTurn 的错误会被吞进后台通知 ⇒ 用户点了"投递"，
      // 收到"已受理"，几秒后才从通知里知道"CLI 根本不存在"。
      // 而配置错是**立刻可判定**的，不该推迟。实测：不做这一步，
      // 判据 TestZcodeMissingCLIErrorIsActionable 会一直等不到应答（卡死 2 分钟）。
      {
        const found = findCLI();
        if (!found.path) {
          throw new Error(
            "找不到 ZCode CLI。已找过：\n  - " + found.tried.join("\n  - ") +
              "\n请把 driver.json 的 cliPath 指向真实 CLI（本机覆盖写到 " +
              "<插件数据>/runtime/zcode/driver.json），或设 ZCODE_CLI 环境变量。");
        }
      }

      // ★★ 投递语义（2026-10-17 修）：立即回"已受理"，不等整轮跑完。
      //
      // 原实现 `await runTurn(...)` 同步等 zcode 把任务做完。而开工任务
      // （改代码/跑测试）要几十秒到 20 分钟 ⇒ 内核的 60s 工具超时先到，
      // 用户看到「执行超时（60秒）」，任务其实还在正常跑。
      // 通道语义是"**投递**任务"，不是"等完成"。
      //
      // 跑完后用主动通知（id=0）把结果推回 Go 侧（见 sidecar.go 的 onNotify）。
      inflight++;
      (async () => {
        try {
          const r = await runTurn(text, timeoutMs);
          if (r.sessionId) zcodeSessionId = r.sessionId;
          turnCount++;
          send({
            jsonrpc: "2.0",
            id: 0, // ★ 0 = 主动通知
            result: {
              event: "prompt_done",
              session_id: zcodeSessionId || desiredSessionId || null,
              reply: String(r.response || "").slice(0, 4000),
            },
          });
        } catch (e) {
          send({
            jsonrpc: "2.0",
            id: 0,
            result: {
              event: "prompt_error",
              session_id: zcodeSessionId || desiredSessionId || null,
              error: String((e && e.message) || e),
            },
          });
        } finally {
          inflight--;
        }
      })();

      return {
        ok: true,
        status: "accepted", // ★ 已受理，未完成
        // ★ 必须回报 session_id：Go 侧据此断言「会话未漂移」
        session_id: zcodeSessionId || desiredSessionId || null,
      };
    }
    case "state": {
      return {
        ok: true,
        session_id: zcodeSessionId || desiredSessionId || null,
        messages: turnCount,
      };
    }
    // ★★ interrupt：取消正在运行的那一轮（2026-10-18 新增）。
    //
    // zcode 是子进程模式，没有 pi 的 session.abort()。这里整组杀进程
    //（spawn 时 detached=true 建了独立进程组，见 killTree）。
    //
    // 语义：interrupted=true 表示当时确实有在跑的一轮并被终止；
    // 没有在跑的任务时 ok=true / interrupted=false（不算错误）。
    case "interrupt": {
      if (!currentChild) return { ok: true, interrupted: false, reason: "no_running_turn" };
      try {
        killTree(currentChild);
      } catch (e) {
        return { ok: false, interrupted: false, error: String((e && e.message) || e) };
      }
      const pid = currentChild.pid;
      currentChild = null;
      return { ok: true, interrupted: true, killed_pid: pid ?? null };
    }
    case "close": {
      killTree(currentChild);
      return { ok: true };
    }
    default:
      throw new Error(`unknown method: ${method}`);
  }
}

const rl = createInterface({ input: process.stdin });
rl.on("line", async (line) => {
  line = line.trim();
  if (!line) return;
  let req;
  try { req = JSON.parse(line); } catch { return; }
  try {
    const result = await handle(req.method, req.params || {});
    send({ jsonrpc: "2.0", id: req.id, result });
    if (req.method === "close") process.exit(0);
  } catch (e) {
    send({
      jsonrpc: "2.0",
      id: req.id,
      error: { code: -32000, message: String(e?.message || e) },
    });
  }
});

rl.on("close", () => {
  killTree(currentChild);
  process.exit(0);
});
