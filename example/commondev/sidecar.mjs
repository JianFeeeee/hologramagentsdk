#!/usr/bin/env node
// commondev sidecar：把一个 pi 会话包成可反复 prompt 的 JSON-RPC 服务。
//
// 协议（与 clawhubadapter 同形）：
//   stdin  请求 {"jsonrpc":"2.0","id":N,"method":"prompt","params":{...}}
//   stdout 应答 {"jsonrpc":"2.0","id":N,"result":{...}}
//
// 方法：
//   ping    握手，返回 pi 版本与会话 cwd
//   prompt  投递一条消息给会话（★ 同一进程内，会话天然连续）
//   state   查当前会话状态（session_id / 消息数）
//
// 设计依据（均已在本机实测）：
//   · createAgentSession({cwd}) 可精确指定工作目录与会话，且同会话可连续追问
//   · 会话库用 sessionDir 与工作区隔离，不污染用户 ~/.pi
//   · 内层 pi 靠 PI_CODING_AGENT_DIR 用插件自己的 agent dir

import { createRequire } from "node:module";
import { createInterface } from "node:readline";

const require = createRequire(import.meta.url);

function parseArgs(argv) {
  const out = {};
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === "--workdir") out.workdir = argv[++i];
    else if (a === "--session-id") out.sessionId = argv[++i];
    else if (a === "--session-name") out.sessionName = argv[++i];
  }
  return out;
}

const args = parseArgs(process.argv.slice(2));
const workdir = args.workdir || process.cwd();
const sessionId = args.sessionId || undefined;
// U6：会话列表里的可读名（pi 侧仅作标注，不参与身份识别）。
const sessionName = args.sessionName || undefined;

// pi 的 SDK 从宿主安装位置解析；随包分发时走 runtime/node_modules。
let mod;
try {
  mod = await import("@earendil-works/pi-coding-agent");
} catch (e) {
  // 回退：用环境里已装的 pi（PI_WEB / 系统安装）
  const fallback =
    "/usr/lib/node_modules/@jmfederico/pi-web/node_modules/@earendil-works/pi-coding-agent/dist/index.js";
  mod = await import(fallback);
}

const { createAgentSession, SessionManager } = mod;

// piVersion 从模块导出或 package.json 取（U8：内层 pi 版本必须可回显、可锁定）。
// 取不到就是取不到 —— 返回 null 让 Go 侧如实记录，不编一个版本号。
let piVersion = mod.version ?? mod.VERSION ?? null;
try {
  const url = import.meta.resolve
    ? import.meta.resolve("@earendil-works/pi-coding-agent/package.json")
    : null;
  if (url) {
    const meta = await import(url, { with: { type: "json" } }).catch(() => null);
    piVersion = meta?.default?.version ?? piVersion;
  }
} catch {}

let session;
let started = false;
// inflight 是**已受理但未完成**的 prompt 数（异步投递后用于观测与优雅退出判断）
let inflight = 0;

async function ensureSession() {
  if (session) return session;

  // ★ 必须用 SessionManager.create(cwd, sessionDir, {id}) **显式指定会话 id**。
  //
  // 为何不能只靠 createAgentSession({cwd})：那样 pi 会**自己生成一个 UUID**
  // 作为会话 id，与 Go 侧台账记的 session_id 不一致 —— 于是
  // “投递后断言会话未变”这条纪律会拿两个不同的名字去比，永远失败
  //（实测：期望 br-cdtest-feat_alpha，实际 01a13959-bcc3-7007-…）。
  //
  // 而用固定 id 建 manager，则：① 首次投递建会话；② 重启/重投时
  // SessionManager 能打开**同一个**会话文件 ⇒ “续投同一会话”是真保障，不是运气。
  const sessionDir = process.env.PI_CODING_AGENT_SESSION_DIR || undefined;
  const manager = sessionId
    ? SessionManager.create(workdir, sessionDir, { id: sessionId })
    : SessionManager.create(workdir, sessionDir);

  const { session: s } = await createAgentSession({
    cwd: workdir,
    sessionManager: manager,
  });
  session = s;
  started = true;
  return session;
}

function send(obj) {
  process.stdout.write(JSON.stringify(obj) + "\n");
}

async function handle(method, params) {
  switch (method) {
    case "ping": {
      await ensureSession();
      return {
        ok: true,
        cwd: workdir,
        session_id: sessionId || null,
        messages: Array.isArray(session?.messages) ? session.messages.length : 0,
        // U8：把内层 pi 版本回显给 Go 侧（台账记录 + agent_up 回显）
        pi_version: piVersion,
        session_name: sessionName || null,
      };
    }
    case "prompt": {
      const text = params?.text ?? "";
      const want = params?.session_id || sessionId;
      const s = await ensureSession();
      // ★★ 投递语义（2026-10-17 修）：立即返回"已受理"，不等 pi 跑完。
      //
      // 原实现是 `await s.prompt(text)` —— 同步等整个任务结束再返回。
      // 而 pi 开工程线干活（克隆/改代码/跑测试）要几十秒到几分钟，
      // 于是内核的 60s 工具超时先到，用户看到「执行超时（60秒）」，
      // 而任务其实还在正常跑。通道的语义是"**投递**任务"，不是"等完成"。
      //
      // 现在：prompt 在后台跑；跑完后用 **主动通知**（id=0）把结果推回
      // Go 侧，由 deliverToPi 注册的 onNotify 注入回 agent（走 inputch）。
      const got = s.sessionId ?? s.id ?? want ?? null;

      // 记录在途数量，供 state 查询
      inflight++;
      (async () => {
        try {
          await s.prompt(text);
          const reply = s.getLastAssistantText ? s.getLastAssistantText() : "";
          send({
            jsonrpc: "2.0",
            id: 0, // ★ 0 = 主动通知（Go 侧 readLoop 按此分派）
            result: {
              event: "prompt_done",
              session_id: s.sessionId ?? s.id ?? got ?? null,
              reply: String(reply || "").slice(0, 4000),
            },
          });
        } catch (e) {
          send({
            jsonrpc: "2.0",
            id: 0,
            result: {
              event: "prompt_error",
              session_id: got,
              error: String((e && e.message) || e),
            },
          });
        } finally {
          inflight--;
        }
      })();

      return {
        ok: true,
        status: "accepted", // ★ 明确：已受理，未完成
        // ★ 必须回报 session_id：Go 侧据此断言「会话漂移」
        session_id: got,
      };
    }
    case "state": {
      const s = await ensureSession();
      return {
        ok: true,
        session_id: s.sessionId ?? sessionId ?? null,
        messages: Array.isArray(s.messages) ? s.messages.length : 0,
      };
    }
    // ★★ interrupt：取消正在运行的任务（2026-10-18 新增）。
    //
    // 为什么必须有：prompt 改成异步投递后，Go 侧只能"发出去"，无法收回。
    // 实测后果 —— 用户发现派错了任务、或某个 session 卡住时，**没有任何办法
    // 停下它**，只能等 pi 自己跑完（agent 在日志里如实回答"无法取消"）。
    //
    // pi 的 AgentSession 本身有 abort()：Abort current operation and wait
    // for agent to become idle。这里直接用它，不自己造机制。
    //
    // 语义说明（必须如实回给调用方）：
    //   · abort() 是**协作式**的：它在下一个可中断点生效，不是 SIGKILL。
    //     工具调用中途可能还要一小段时间才真正停下。
    //   · 返回 interrupted=true 表示"已发出中断且会话已回到空闲"；
    //     若当时没有在跑的任务，interrupted=false 但 ok=true（不算错误）。
    case "interrupt": {
      if (!session) return { ok: true, interrupted: false, reason: "no_session" };
      // ★ 判定"是否真的中断了"必须靠**中断前后 inflight 的变化**，不能只看
      // 中断前 >0。第一版写成 `interrupted: hadInflight`，判据也照这个断言，
      // 于是"把 abort() 删掉"这个变异**完全测不出来**：
      // 任务还在跑 → inflight 仍 >0 → 照样回报 interrupted=true。
      // 那正是最坏的情形：用户以为停了，任务还在改代码、推分支。
      // ★ 判定依据必须用 pi 自己的状态（isIdle），不能用本文件的 inflight：
      //
      //   inflight 是**包装层**的计数，只在后台 promise 的 finally 里递减，
      //   而 abort() 返回时那个 promise 还没收尾 ⇒ abort 成功也没减。
      //   实测（第一版）：before=1, inflight=1 ⇒ 误报 interrupted=false，
      //   而任务其实已经停了 —— 反向的谎报，同样有害（用户重复发任务）。
      //
      //   session.isIdle 是 pi 的权威视图："当前没有在跑的任务"。
      const wasIdle = session.isIdle;
      try {
        await session.abort();
      } catch (e) {
        return { ok: false, interrupted: false, error: String((e && e.message) || e) };
      }
      // abort() 语义是"中止并等待 agent 回到空闲"⇒ 返回时应已 idle。
      // 仍非 idle 说明中止没生效（或还在收尾），如实回报，不夸大成已中断。
      return {
        ok: true,
        interrupted: !wasIdle && session.isIdle,
        was_idle: wasIdle,
        now_idle: session.isIdle,
        inflight,
      };
    }
    case "close": {
      try {
        session?.dispose?.();
      } catch {}
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
  try {
    req = JSON.parse(line);
  } catch {
    return; // 非协议行，忽略
  }
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
  try {
    session?.dispose?.();
  } catch {}
  process.exit(0);
});
