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
      await s.prompt(text);
      const got = s.sessionId ?? s.id ?? want ?? null;
      return {
        ok: true,
        // ★ 必须回报 session_id：Go 侧据此断言「会话漂移」
        session_id: got,
        reply: s.getLastAssistantText ? s.getLastAssistantText() : "",
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
