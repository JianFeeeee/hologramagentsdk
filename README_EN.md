# HologramAgent SDK

Plugin development SDK for building intelligent plugins that interact with the HologramAgent platform.

> 📖 **Full documentation**: <https://sdk.hologramagent.jianfgit.xyz/>
>
> Quick start / API reference / guides / example plugins all live there. **This README is a
> summary only** — the docs site is authoritative for interface details.
> Jump to: [Getting started](https://sdk.hologramagent.jianfgit.xyz/guide/getting-started/)
> · [API reference](https://sdk.hologramagent.jianfgit.xyz/api/) · [Capability boundary](https://sdk.hologramagent.jianfgit.xyz/guide/capability-boundary/)
>
> Agent-friendly plain-text entry points: [`llms.txt`](https://sdk.hologramagent.jianfgit.xyz/llms.txt)
> and [`llms-full.txt`](https://sdk.hologramagent.jianfgit.xyz/llms-full.txt) (the whole docs in one file).

## Version and Compatibility

Current: **SDK 1.3.0** (requires kernel **1.3.0+**).

**The version tracks the kernel's minor version, with the patch position pinned at `.0`**:

| Kernel version | Matching SDK |
|---|---|
| 1.0.0 / 1.0.1 / … / 1.0.4 | 1.0.0 |
| 1.1.0 / 1.1.1 / … / 1.1.N | **1.1.0** |
| 1.2.0 / 1.2.1 / … / 1.2.N | **1.2.0** |
| 1.3.0 onward | **1.3.0** |

The kernel's patch position is reserved for bugfixes and vulnerability fixes, which never touch the
public interface, so the SDK version has no reason to move with it — otherwise you would either be
forced to chase releases or suspect your version is stale, when not one character of the interface
has changed.

The SDK repository therefore publishes **exactly once per minor version** (`vX.Y.0`); kernel patches
such as `v1.3.1` do not trigger an SDK release. (A `v1.3.1` tag was mistakenly cut on 2026-09-13 and
has been withdrawn — any SDK tag with a non-zero patch position is wrong.)

## New in 1.3.0: Injection Priority and Dynamic Output Channels

- **`InjectOptions.Priority` / `PriorityL1`–`PriorityL4`** — a plugin declares the interrupt level of
  its own injection; the kernel schedules L1–L4, where **L4 is reserved for the kernel and
  kernel-level plugins**. The zero value is fully equivalent to the old three-argument call
  (queued, never preempting), so existing plugins need neither a code change nor a rebuild.
  Queued input has no level: anything can jump ahead of it.
- **`UnregisterOutputChannel` / `OutputChannelUnregistrar` / `SetOutputChannelUnregistrar`** —
  channels that die with their resource (one channel per remote device) can now be unregistered;
  previously they lingered and the model kept "successfully" sending into a dead channel.
- **Channel names must be legal and unique.** The name is spliced into the LLM function name
  `output_send__<name>`, so it may only contain `[A-Za-z0-9_-]`. A real production incident
  (2026-09-13): `device/<id>` made every LLM request fail with 403. Derive channel names from
  external IDs — never use the raw ID.

**Upgrading a 1.0.x plugin to 1.1.x: no code changes, no rebuild.** Everything added in 1.1.0 is
in the "plugin calls, kernel implements" direction, so not calling it means not being affected
(verified with an old `plugin.bin` built against SDK 0.9.2: it handshakes fine on the new kernel,
because the handshake validates the **major** version, not the exact SDK version). Rebuild only when you want
the new fields.

**Upgrading a 1.1.x plugin to 1.2.x: the interface is purely additive, but a rebuild is required.**
No public signature changed (the SDK adds `InjectOptions`, six `*Opts` variants and
`ChannelDef.ContextPolicy`), so not calling the new capabilities means not being affected — but the
kernel's **plugin protocol went to 2** (the fd3 layout of the unified shared-memory region changed,
and **rolling upgrades are not supported**). `plugin.bin` must therefore be rebuilt with the matching
`hmapdev` and installed **together with** the kernel; otherwise the handshake fails on protocol
version mismatch (the error says explicitly to rebuild with the matching hmapdev — it never
degrades silently).

## Injection Behaviour and Context Pruning (1.2.0)

"Should this go into memory" and "should the context be pruned based on this" used to be
something only `ToolDef` could declare. Since 1.2.0 **injections can declare them too**, sharing
the same semantics and values.

```go
type InjectOptions struct {
	NoMemory      bool   // true = excluded from memory computation (vectorize/keywords/distill); the
	                     //        original text still stays in context
	ContextPolicy string // ""/none = do not prune (default); prune = prune context based on this
	CleanerName   string // name of the compute-layer cleaner: run it first to get the effective
	                     // content, then compute/prune on that
}

const (
	ContextPolicyNone  = "none"
	ContextPolicyPrune = "prune"
)

// Six variants, one-to-one with the older three-argument methods, plus opts
InjectTextOpts(source, channel, text string, opts InjectOptions)
InjectInterruptTextOpts(source, channel, text string, opts InjectOptions)
InjectInputSyncOpts(source, channel, text string, opts InjectOptions) string
InjectInputMediaOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions)
InjectInputMediaSyncOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions) string
InjectInterruptMediaOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions)
```

Key points:

- **A zero-valued `InjectOptions{}` is key-for-key equivalent to the older three-argument methods**
  (recorded in memory, not pruned). The old methods remain as zero-value sugar (`InjectText`,
  `InjectInterruptText`, `InjectTextNoMemory`, …), so existing plugins keep working without a single
  line changed *or* a rebuild.
- **Pruning (`prune`) must be declared explicitly**: it archives/drops low-relevance events, which
  is a side effect, so it is off by default. The kernel only accepts `""` / `none` / `prune`
  (`ValidContextPolicy`); anything else is rejected.
- Pruning first goes through the plugin's registered **`Cleaner`** (named by `CleanerName`) to get
  the effective content, avoiding the inconsistency of "prune on the raw text, compute on the
  cleaned text".
- `ChannelDef` carries the same `context_policy` (1.2.0 also gave `ChannelDef` JSON tags — the
  definition crosses the process boundary, while `Cleaner` is a function that must be ignored; with
  no tags, newly added fields would be silently dropped).

## SDK API Surface

### Plugin Interface

Plugins implement the `Plugin` interface:

```go
type Plugin interface {
    Name() string
    Start(sdk *PluginSDK) error
    Stop() error
}
```

### PluginSDK Methods

The SDK instance injected via `Start(sdk *PluginSDK)` provides:

| Category | Method | Description |
|----------|--------|-------------|
| Stage Hooks | `RegisterStage(stage, handler, scope...)` | Register stage callback; scope: `StageScopeGlobal` (all, default) or `StageScopeOwnTools` (own tools only) |
| Input Channel | `RegisterInputChannel(name, def)` | Register input channel with `ChannelDef` (NoMemory/Cleaner) |
| Output Channel | `RegisterOutputChannel(name, caps, desc, def, handler)` | Register output channel with `ChannelDef` and capability bitmask |
| Tool Registration | `RegisterTool(name, def, handler)` | Register a tool for LLM invocation |
| Plugin API | `RegisterPluginAPI(name)` | Register plugin API for inter-plugin access |
| Graph Memory | `Memory()` | Access graph memory API (entity-relation store) |
| Text Memory | `TextMemory()` | Access text memory API (chronological events) |
| Doc Memory | `DocMemory()` | Access document memory API (vector store) |
| Social Graph | `Social()` | Access social graph API (read-only for external plugins) |
| Knowledge | `Knowledge()` | Access knowledge base API |
| LLM | `LLM()` | Access LLM provider manager API |
| Settings | `Settings()` | Access settings API |
| Events | `Events()` | Access event subscriber (subscribe-only for external plugins) |
| Inject | `InjectText(source, channel, text)` / `InjectInterruptText(source, channel, text)` / `InjectTextNoMemory(source, channel, text)` | Inject text into the agent pipeline |
| Media inject | `InjectInputMedia(source, channel, text, blocks)` / `InjectInputMediaSync(...)` / `InjectInterruptMedia(...)` | Inject input carrying images/audio (added in 1.1.0) |
| Auto-Restart | `SetAutoRestart(enabled)` / `AutoRestart()` | Control automatic restart on crash |

### Stage Hooks

```go
// Listen to all stage events globally
sdk.RegisterStage(StagePreAction, func(ctx *StageContext) error { return nil })

// Listen only to this plugin's own tool calls (before_toolcall / after_toolcall only)
sdk.RegisterStage(StageBeforeToolcall, myHandler, StageScopeOwnTools)
```

### ChannelDef

```go
type ChannelDef struct {
    NoMemory bool              // Channel input/output skips memory computation (vector/keyword/distill), original text preserved
    Cleaner  func(string) string // Optional: computation layer filter (does not modify original text)
}
```

`ChannelDef` controls channel behavior in the memory computation layer, with the same semantics as `ToolDef.NoMemory`/`Cleaner`.

### Input Channels

```go
sdk.RegisterInputChannel("qq", ChannelDef{
    NoMemory: true,
    Cleaner:  func(text string) string { return strings.TrimSpace(text) },
})
```

### Output Channels

```go
sdk.RegisterOutputChannel("my-channel", CapText|CapFile, "channel description", ChannelDef{}, handler)
```

The handler receives three arguments:
- `payload` (string) — message content. For `type=text` it's plain text, for `type=file/image` it's a URL
- `meta` (string) — optional JSON routing metadata (e.g. `{"group_id":123,"user_id":456}`)
- `type` (string) — content type enum (see below)

Capability flags:

| Flag | Value | Description |
|------|-------|-------------|
| `CapText` | 1 | Plain text output |
| `CapFile` | 2 | File output |
| `CapImage` | 4 | Image output |
| `CapAudio` | 8 | Audio output |
| `CapStructured` | 16 | Structured data output |

Type enum values:

| Value | Description |
|-------|-------------|
| `text` | plain text |
| `voice` / `audio` | audio/voice |
| `image` | image |
| `file` | file |

### IOInjector Channel Routing

| Method | Description |
|--------|-------------|
| `InjectText(source, channel, text)` | Inject text, record to memory, route to specified channel |
| `InjectInterruptText(source, channel, text)` | Inject interrupt text, interrupt current processing, route to specified channel |
| `InjectTextNoMemory(source, channel, text)` | Inject text without memory recording, route to specified channel |

### Multimodal Injection (added in 1.1.0)

| Method | Description |
|--------|-------------|
| `InjectInputMedia(source, channel, text, blocks)` | Inject media-bearing input, asynchronous |
| `InjectInputMediaSync(source, channel, text, blocks)` | Inject media-bearing input and wait for the reply text |
| `InjectInterruptMedia(source, channel, text, blocks)` | Inject a media-bearing interrupt that can preempt current processing |

`blocks` is `[]sdk.ContentBlock`, the same type `SetToolBlocks` takes:

```go
s.InjectInputMedia("myplugin", "webui", "take a look at this", []sdk.ContentBlock{{
    Type:     "image_url",
    ImageURL: &sdk.ImageURL{URL: "data:image/png;base64," + b64, Detail: "auto"},
}})
```

How this differs from `SetToolBlocks`: that one is only callable inside a tool handler and
its media reaches the model with the *next* tool message. These three let a plugin
**initiate a turn that carries media** — the media goes out with this turn's message and is
automatically stored in the media store with a memory reference attached.

`data:` URLs in the blocks are stored and deduplicated by the kernel; `http(s)` URLs are
passed to the model only and never stored (storing them would require the kernel to make
network requests, bringing timeouts, auth and SSRF into scope).

`source` identifies the origin, `channel` specifies the target output channel.

### Triple Extended Fields

The Triple data structure includes additional fields:

- `Confidence` — confidence score (0.0–1.0)
- `SubjectType` — subject type
- `ObjectType` — object type
- `SentenceText` — the original sentence (added in 1.1.0), written to the `sentences` table; media references hang off the sentence
- `MediaDigests` — associated media digests (added in 1.1.0)

### Media in Memory (added in 1.1.0)

Inside plain-text memory, media is represented as a **marker** of the form
`[<mime> <short digest>] <description>`:

```
[image/png a1b2c3d4e5f6] a purple-blue-red three-band chart
```

The description is the durable semantic memory (retrieval uses it); the digest is the key
back to the bytes (reverse lookup uses it). Markers are generated by the kernel — a plugin
never has to assemble one, it just **supplies the digest**.

#### Graph memory

```go
s.Memory().Commit([]sdk.Triple{{
    Subject: "palette", Relation: "contains", Object: "three-band",
    MediaDigests: []string{"a1b2c3d4e5f6"}, // short digest is fine, the kernel resolves it
}})
```

With no `SentenceText`, the kernel uses the marker itself as the sentence — media must have
a sentence to hang off, otherwise the reference has nowhere to attach.

#### Knowledge base

```go
s.DocMemory().InsertWithMedia(&sdk.Doc{
    Title:   "illustrated note",
    Content: "body",
}, []sdk.MediaAttachment{
    {MIME: "image/png", Data: pngBytes, Name: "chart.png"}, // new content, stored and deduped
    {Digest: "a1b2c3d4e5f6"},                               // reference existing content
})
```

`Insert` keeps its original signature; markers already present in the body are bound as
document-level references too. `Query` fills `MediaDigests` and `Attachments` (mime plus
description, **no bytes** — one query can match dozens of media items). Removing a document
releases its references.

#### Text memory

```go
s.TextMemory().Append(sdk.TextEvent{
    Role: "user", Content: "look at this",
    Attachments: []sdk.MediaAttachment{{MIME: "image/png", Data: pngBytes}},
})
```

`RecentEvents` decodes markers in the body back into `Attachments`.

The media store can be disabled kernel-side (`core.memory.media.enabled=false`); all of the
above then degrades to plain-text behaviour — no errors, no panics, identical to how it
behaved before this feature shipped.

### ToolDef Field Reference

The `def` parameter of `RegisterTool` is of type `sdk.ToolDef`, with the following fields:

| Field | Type | Description |
|-------|------|-------------|
| `Name` | `string` | Tool name, use plugin name prefix to avoid conflicts |
| `Description` | `string` | Tool description, LLM uses this for tool selection |
| `Parameters` | `map[string]interface{}` | JSON Schema parameter definition |
| `NoMemory` | `bool` | Default `false`; when `true`, output skips vector/jieba/distill computation (original text preserved) |
| `Cleaner` | `func(string) string` | Optional, filters output before computation layer (e.g., extract `.content` from JSON) |

For detailed design rationale of `NoMemory` and `Cleaner`, see `docs/en/PLUGIN_DEV.md` in the core repository.

### New Constructor

`New()` is called by the kernel when loading a plugin. Plugin developers do not need to construct PluginSDK manually:

```go
func New(name string, sett SettingsAPI, regTool ToolRegistrar, regStage StageRegistrar, regAPI APIRegistrar, regOutput OutputChannelRegistrar) *PluginSDK
```

Plugin developers only need to implement the `Plugin` interface and export a `NewPlugin()` entry function.

## hmapdev Toolchain

`hmapdev` provides full development workflow support and produces `.hmap` plugin bundles (the tool is
named after that package format). Prebuilt binaries ship as **release assets**
(linux/darwin/windows × amd64/arm64); download from
[Releases](https://github.com/JianFeeeee/hologramagentsdk/releases) and put it on your PATH:

> Rename note: the toolchain was called `plugindev` and is `hmapdev` since 1.2.0.
> The SDK store moved from `~/.hologram/plugindev/sdk` to `~/.hologram/hmapdev/sdk`
> (the old directory is still honored, so installed versions are not lost).

```bash
# From release assets (latest SDK release / linux amd64 shown)
curl -Lo hmapdev https://github.com/JianFeeeee/hologramagentsdk/releases/download/<version>/hmapdev_linux_amd64
chmod +x hmapdev

# Or build from source
cd tools/hmapdev && go build -o hmapdev .
```

> Binaries no longer ship inside the repository (the old `bin/` directory is retired): five
> platforms at 26-28MB each piled another copy into git history on every rebuild, and they are
> reproducible from source anyway.

| Command | Description |
|---------|-------------|
| `hmapdev init <name> [--lua]` | Initialize plugin project (generates plg.json, plugin.go or main.lua, go.mod, README.md) |
| `hmapdev build [flags]` | Build and package into a `.hmap` (supports cross-compilation and bundle mode) |
| `hmapdev clean` | Clean `build/` and `dist/` plus generated files |
| `hmapdev debug [dir]` | Load plugin source through the Yaegi Go interpreter and start an interactive REPL |
| `hmapdev sdk <command>` | SDK version management (list/install/use/path/current/latest) |

Supports both **Go** and **Lua** plugin languages.

### plg.json Manifest Format

```json
{
  "name": "weather",
  "name_zh": "天气查询",
  "name_en": "Weather",
  "version": "1.0.0",
  "description": "Weather plugin",
  "author": "HologramAgent",
  "entry": "plugin.bin",
  "tags": ["weather", "forecast"],
  "targets": "linux/amd64,windows/amd64",
  "outdir": "dist",
  "bundle": true,
  "replaces": {
    "github.com/example/pkg": "../local/pkg"
  },
  "source_dirs": [
    "../shared-lib"
  ]
}
```

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Plugin identifier |
| `name_zh` | string | Chinese name |
| `name_en` | string | English name |
| `version` | string | Version |
| `description` | string | Plugin description |
| `author` | string | Author |
| `entry` | string | Entry file (`plugin.bin` / `main.lua`). Since v1.0.0 Go plugins uniformly build to `plugin.bin`—no per-platform suffix |
| `tags` | string[] | Tags |
| `targets` | string | Build targets, comma-separated (e.g. `linux/amd64,windows/amd64`) |
| `outdir` | string | Output directory (default `dist`) |
| `bundle` | bool | Bundle mode (build all platforms at once) |
| `replaces` | object | Go module replacements, key=module path, value=local path |
| `source_dirs` | string[] | Additional source search paths (auto-imported at build time) |

### .hmap Package Format

`.hmap` is a ZIP archive containing:

- `plugin.json` — plugin metadata
- `plugin.bin` — Go compiled artifact (single-platform build)
- `plugin.bin.<goos>.<goarch>` — one per platform in bundle mode; on install pluginmgr picks
  the one matching the current platform and renames it to `plugin.bin`
- `main.lua` — Lua plugin entry (for Lua plugins)

> Since v1.0.0 `plugin.so`/`plugin.dll`/`plugin.dylib` are no longer used—the process boundary
> *is* the ABI boundary, so there is no platform-specific shared-library distinction. The new
> kernel will not load old artifacts; it emits an explicit rebuild hint instead.

## Plugin Lifecycle

### Start & Stop

- `Start(sdk *PluginSDK) error` — Plugin startup, receives SDK instance
- `Stop() error` — Plugin shutdown, release resources
- `sdk.RegisterStopHandler(fn func())` — Register a shutdown cleanup callback. The kernel (for built-in plugins) or z_bridge (for external plugins) runs all registered handlers **before** calling the plugin's `Stop()` (LIFO order, cleared after running — idempotent). Use it for persistence and cancelling background work: plugin memory is still fresh at that point, avoiding stale-state write-backs that resurrect deleted data.

### Remove Cleanup (onRemove)

`Stop` / `RegisterStopHandler` run whenever the plugin **stops** (including reload and disable); `RegisterOnRemoveHandler` runs **only once when the plugin is uninstalled (removed)** — never on reload or disable:

- `sdk.RegisterOnRemoveHandler(fn func())` — Register a remove cleanup callback. The kernel runs it **after** the plugin's `Stop()` in the `RemovePlugin` flow (LIFO order, cleared after running — idempotent). Use it to delete persistent files the plugin created itself (data/cache/state files).
- The kernel also cleans up on uninstall: tool registrations, the `disabled_plugins` record, the plugin's config definitions (`plugin.<name>.*`) and its config table (`config_<name>`) — the plugin's config section disappears completely after removal.
- Examples: `example/calendar` (removes events.json), `example/memo` (removes memos.json), `example/rss` (removes the subscription data dir), `example/weather` (removes the cache dir); the `hmapdev` template includes an onRemove demo.

```go
sdk.RegisterOnRemoveHandler(func() {
    os.Remove(filepath.Join(dataDir, "events.json"))
})
```

### Auto-Restart

```go
sdk.SetAutoRestart(true)
// Query state
enabled := sdk.AutoRestart()
```

The platform automatically restarts the plugin on crash, ensuring service availability.

Restarts are **rate-limited**. Defaults (kernel `internal/plugin/registry.go`):

| Parameter | Value | Meaning |
|---|---|---|
| `procRestartBackoff` | `1s` | Before restart #n, wait `n × 1s` (linear backoff, not immediate) |
| `procMaxRestarts` | `3` | Max restarts within the window |
| `procCrashWindow` | `5min` | No new crash within the window resets the count |

So the actual sequence is **1s → 2s → 3s**; the **4th** crash in the same 5-minute
window (`n > 3`) is **not** restarted automatically and needs manual intervention.
This is not instant, invisible recovery — if your plugin must be back in seconds,
reconnect and rebuild your own state in `OnStart`.

> ⚠️ `SetAutoRestart` is typically used to decide whether auto-restart is safe *after* an
> external connection has been established, and that connection setup usually happens in a
> background goroutine while the kernel reads the flag from another one — which is inherently
> concurrent. **SDK 1.1.0 locks this flag and all API fields** (`-race` reported 11 data races;
> in production this showed up as sporadic nil-dereference crashes during plugin reload). Upgrade
> if you are on anything earlier.

## Concurrency Contract for Plugin Developers

`PluginSDK` is a **shared object used by multiple goroutines**: the polling, listening and timer
callbacks you start in `Start()` all hold the same `*PluginSDK` and push messages into it, while
the kernel writes its API fields during load/reload. So:

- **Guaranteed by the SDK**: all API accessors (`Memory()`/`DocMemory()`/…), all injection methods,
  `SetAutoRestart`/`AutoRestart`, `RegisterTool`/`RegisterStage`, and
  `RunStopHandlers`/`RunOnRemoveHandlers` (idempotent; concurrent calls still run it once).
- **Your responsibility**: every field of `StageContext` is exported, and concurrent read/write
  must hold `ctx.Lock()`/`ctx.RLock()`. Especially `ctx.Extra` — **concurrent map writes are a
  fatal in Go, and `recover` cannot catch it**.

```go
ctx.Lock()
ctx.Extra["mykey"] = value
ctx.FinalText += "supplementary note"
ctx.Unlock()
```

## Restricted SDK vs Full SDK

External plugins (third-party distribution) use a **restricted SDK** that only exposes a safe subset:

| Restricted API | Allowed Operations |
|----------------|-------------------|
| `SocialAPI` | Read-only: `GetPerson`, `GetTrait`, `GetRelations`, `GetNetwork`, `ListPersons` |
| `EventSubscriber` | Subscribe-only: `Subscribe` (no `Publish`) |

Internal plugins (platform built-in) have full SDK access including SocialAPI write operations and EventPublisher.

## Example Plugins

| Plugin | Type | Description |
|--------|------|-------------|
| [weather](example/weather) | Go | Weather queries (wttr.in); demonstrates NoMemory/Cleaner/stage hooks/channels/text memory |
| [luademo](example/luademo) | Lua | Full-featured Lua example covering the whole v0.8.0 Lua SDK surface |
| [qq](example/qq) | Go | QQ messaging integration (NapCat), 17 tools, full input/output channel wiring |
| [a2a](example/a2a) | Go | Agent-to-Agent protocol communication |
| [ai_image](example/ai_image) | Go | AI image generation |
| [bili](example/bili) | Go | Bilibili video downloading |
| [browser](example/browser) | Go | Web search, page fetching, browser rendering |
| [calendar](example/calendar) | Go | Calendar management |
| [editdoc](example/editdoc) | Go | Document editing |
| [files](example/files) | Go | File management |
| [memo](example/memo) | Go | Memos (PreAction injection + scheduled reminders) |
| [music](example/music) | Go | Music playback |
| [ocr](example/ocr) | Go | Optical character recognition |
| [rss](example/rss) | Go | RSS subscriptions |
| [sanitizer](example/sanitizer) | Go | Content sanitization / safety filtering |

**Prebuilt example artifacts ship with every release**: besides the 5-platform `hmapdev`, an SDK
release contains the example plugins' `.hmap` files plus `SHA256SUMS`/`MANIFEST.txt`. The reason is
that plugin binaries are **layout-bound** to the kernel (major version + unified-region magic +
`field_count`), so shipping the toolchain without matching artifacts invites installing an old artifact —
which fails the handshake and looks like "the plugin is broken" rather than "the versions don't
match".

## Remote Device SDK

A C language SDK for developing **remote device access adapters** with zero external dependencies, compatible with embedded platforms.

### Architecture

```
┌─────────────────────────────────────────────────┐
│            ha_remotedevice (C SDK)              │
│  Protocol Engine │ WS Frames │ JSON │ State     │
│  Machine │ Transport Abstraction                │
└──────────┬──────────────────────────────────────┘
           │  Same C code, shared by device & app
    ┌──────┴──────────────────┐
    ▼                         ▼
┌──────────────┐    ┌──────────────────────────┐
│  ESP32 Bare   │    │  Linux App                │
│  Pure C       │    │  (Python ctypes / Go CGo /│
│  Simple Cmd   │    │   Node addon / C# P/Invoke)│
└──────────────┘    └──────────────────────────┘
```

### Declarative API Design

The device declares **what it is** and **what it can do** in code. The SDK handles all protocol details automatically:

```c
#include "ha_remotedevice.h"

/* Declare capabilities */
const char *caps[] = {"camera", "status", NULL};

ha_config_t config = {
    .transport = my_transport,     // User implements 4 functions
    .server    = "192.168.1.100:9890",
    .token     = "my-token",
    .device = {
        .device_id = "esp32-cam-1",
        .name      = "Front Door Camera",
        .kind      = "camera",
        .caps      = caps,
    },
    .on_cmd    = my_cmd_handler,   // Called when receiving commands
    .on_binary = my_data_handler,  // Called on binary data (TTS audio, etc.)
    .on_state  = my_state_handler, // Connection state changes
};

ha_client_t *client = ha_client_new(&config);
ha_client_start(client);
while (1) {
    ha_client_process(client);     // Main loop processing
}
```

### Transport Layer Abstraction

Users only need to implement 4 functions to adapt to different platforms:

```c
ha_transport_t my_transport = {
    .connect = my_tcp_connect,   // Establish TCP connection
    .send    = my_tcp_send,      // Send data
    .recv    = my_tcp_recv,      // Receive data (blocking)
    .close   = my_tcp_close,     // Close connection
    .ctx     = &my_platform_ctx,
};
```

### Protocol Support

| Feature | API |
|---------|-----|
| WS connection + handshake | Automatic via `ha_client_start` |
| Device registration (hello/bind) | Automatic on startup |
| Command receive (shell/homeagent) | `on_cmd` callback |
| Command result | `ha_client_send_result` |
| Binary chunked transfer (video) | `ha_client_send_data_chunked` |
| TTS audio receive | `on_binary` callback |
| Event reporting | `ha_client_send_event` |
| Status reporting | `ha_client_send_status` |
| Heartbeat keepalive | Automatic ping/pong |

### Usage

Initialize a project via the `hmapdev` toolchain:

```bash
hmapdev init my-adapter --type remotedevice
```

Generates `main.c` + `CMakeLists.txt`, can be built directly or used as a third-party library:

```cmake
add_subdirectory(path/to/ha_remotedevice)
target_link_libraries(my_app ha_remotedevice)
target_include_directories(my_app PRIVATE ${HA_REMOTEDEVICE_INCLUDE_DIR})
```

### Quick Start Guide

A complete step-by-step guide from zero to a device successfully connected to HologramAgent.

#### Step 1: Preparation

Create an access token on the HologramAgent platform:

```bash
# Create a device access token on the HologramAgent server
curl -X POST http://<hologram-server>:8080/api/v1/device/token \
  -H "Content-Type: application/json" \
  -d '{"device_id":"esp32-cam-1","name":"Front Door Camera","kind":"camera"}'
# Returns: {"token":"ha-dev-token-xxxxx"}
```

Save the returned `token` — you'll need it in the device configuration.

#### Step 2: Implement the Transport Layer (4 functions)

Implement the 4 function pointers of `ha_transport_t` for your platform. Here are common scenarios:

**Scenario A: Embedded device with TCP/IP stack (e.g., ESP32 + lwIP)**

```c
#include "ha_remotedevice.h"
#include "lwip/sockets.h"

static int esp_connect(void *ctx, const char *host, uint16_t port) {
    struct sockaddr_in addr;
    int sock = socket(AF_INET, SOCK_STREAM, 0);
    if (sock < 0) return -1;
    addr.sin_family = AF_INET;
    addr.sin_port = htons(port);
    inet_pton(AF_INET, host, &addr.sin_addr);
    int ret = connect(sock, (struct sockaddr *)&addr, sizeof(addr));
    if (ret < 0) { closesocket(sock); return -1; }
    *(int *)ctx = sock;
    return 0;
}

static int esp_send(void *ctx, const uint8_t *data, int len) {
    int sock = *(int *)ctx;
    return send(sock, (const char *)data, len, 0);
}

static int esp_recv(void *ctx, uint8_t *buf, int len) {
    int sock = *(int *)ctx;
    return recv(sock, (char *)buf, len, 0);
}

static void esp_close(void *ctx) {
    int sock = *(int *)ctx;
    closesocket(sock);
}

int esp_ctx = -1;
ha_transport_t transport = {
    .connect = esp_connect,
    .send    = esp_send,
    .recv    = esp_recv,
    .close   = esp_close,
    .ctx     = &esp_ctx,
};
```

**Scenario B: Serial (UART) passthrough module**

```c
static int uart_connect(void *ctx, const char *host, uint16_t port) {
    (void)host; (void)port;
    return uart_init((uart_ctx_t *)ctx, 115200);
}

static int uart_send(void *ctx, const uint8_t *data, int len) {
    return uart_write((uart_ctx_t *)ctx, data, len);
}

static int uart_recv(void *ctx, uint8_t *buf, int len) {
    return uart_read((uart_ctx_t *)ctx, buf, len);
}

static void uart_close(void *ctx) {
    uart_deinit((uart_ctx_t *)ctx);
}
```

> Note: For UART passthrough, a TCP bridge program must run on the other end to forward serial data to the HologramAgent WebSocket port.

#### Step 3: Declare Device Capabilities and Command Handlers

```c
#include "ha_remotedevice.h"

/* Declare device capabilities */
const char *caps[] = {"camera", "speaker", "status", NULL};

/* Handle camerasue command (take photo) */
static ha_status_t handle_camera(const char *req_id, const char *args,
                                 ha_cmd_result_t *result, void *userdata) {
    (void)req_id; (void)userdata;
    int duration = args[0] ? atoi(args) : 0;

    // Capture image, fill the result
    result->status = 0;
    result->output = "data:image/jpeg;base64,/9j/4AAQ...";  // base64 image data
    return HA_OK;
}

/* Handle shell command */
static ha_status_t handle_shell(const char *req_id, const char *args,
                                ha_cmd_result_t *result, void *userdata) {
    (void)req_id; (void)userdata;
    result->status = 0;
    result->output = "command executed";
    return HA_OK;
}

/* Declarative command handler table */
ha_cmd_handler_def_t handlers[] = {
    {.command = "shell",      .handler = handle_shell},
    {.command = "camerasue",  .handler = handle_camera},
    {.command = "screensee",  .handler = handle_camera},
    {.command = "speakeruse", .handler = handle_speaker},
    {.command = NULL},  /* terminator */
};
```

#### Step 4: Configure and Start the Client

```c
ha_config_t config = {
    .transport = transport,                 // Transport layer implementation
    .server    = "192.168.1.100:9890",      // HologramAgent server address
    .token     = "ha-dev-token-xxxxx",      // Token from Step 1
    .device = {
        .device_id = "esp32-cam-1",
        .name      = "Front Door Camera",
        .kind      = "camera",
        .caps      = caps,
        .info_json = "{\"chip\":\"ESP32-S3\",\"firmware\":\"v1.0\"}",
    },
    .handlers  = handlers,                  // Command handler table
    .on_binary = on_binary_data,            // Receive TTS audio etc.
    .on_state  = on_state_change,           // Connection state callback
    .ping_interval = 30,
};

ha_client_t *client = ha_client_new(&config);
ha_status_t ret = ha_client_start(client);
if (ret != HA_OK) {
    printf("Device connection failed: %d\n", ret);
    return;
}

/* Main loop */
while (1) {
    ha_client_process(client);  // Process protocol frames, heartbeats, commands

    /* Optional: device-initiated event reporting */
    ha_client_send_event(client, "motion_detected",
                         "{\"zone\":\"front_door\",\"confidence\":0.95}");

    /* Optional: report device status */
    ha_client_send_status(client, "online");

    vTaskDelay(100 / portTICK_PERIOD_MS);  // RTOS-style delay
}
```

#### Step 5: Verify the Connection

Check if the device is online on the HologramAgent server:

```bash
# List registered devices
curl http://<hologram-server>:8080/api/v1/device/list
# Expected output includes: {"device_id":"esp32-cam-1","status":"online",...}

# Send a command to the device (test camerasue)
curl -X POST http://<hologram-server>:8080/api/v1/device/esp32-cam-1/cmd \
  -H "Content-Type: application/json" \
  -d '{"cmd":"camerasue","args":"3"}'
# Expected: {"status":"ok","result":"data:image/jpeg;base64,..."}
```

#### Step 6: Debugging Tips

| Issue | Check |
|-------|-------|
| Connection failed | Verify `server` address and port are reachable; check `token` |
| WS handshake failed | Verify HologramAgent server WebSocket support is enabled |
| Command not responding | Confirm the command name is registered in `handlers` table; check `on_binary` |
| Reconnection issues | `max_reconnect` controls retry count; -1 = infinite |
| Low memory (embedded) | Define `HA_NO_ALLOC` to disable dynamic memory allocation |

### Location

- **SDK Source**: `remotedevice/`
- **hmapdev template**: `hmapdev init --type remotedevice`

## Building & Installing

### Build

```bash
hmapdev build
```

Outputs a `.hmap` package to the `dist/` directory (default is the multi-platform bundle; use `hmapdev build --no-bundle` for a single-target build).

### Install

Via the pluginmgr HTTP API (default port 9876, listening on 127.0.0.1 only, no auth):

```bash
# Local path
curl -X POST http://127.0.0.1:9876/plugins \
  -H "Content-Type: application/json" \
  -d '{"path": "/path/to/my-plugin.hmap"}'

# Upload binary directly
curl -X POST http://127.0.0.1:9876/plugins \
  --data-binary @dist/my-plugin.hmap
```

Or upload via the WebUI plugin management page, or manually place the `.hmap` in the plugin directory and restart the platform.

## License

The SDK is released under the **MIT license** — see [LICENSE](LICENSE).

**This permissiveness is deliberate**: the SDK is **statically linked** into your plugin
(its source ends up in the plugin binary). Under a copyleft license such as AGPL that would
force plugin authors to open-source their work; MIT exists precisely so that plugin authors
can **pick their own license** — closed-source, commercial or private — with no obligation to
contribute back and no need for any exception or commercial grant. The safety and vitality of
the third-party plugin ecosystem rest on this.

This is sound because the SDK is **fully self-contained**: `go.mod` has zero external
dependencies and `sdk/` imports only the Go standard library (`sync`), never any code from the
core repository — so the MIT grant conflicts with nothing.

Third-party components (Go dependencies: go-sqlite3, gojieba, bubbletea, … — MIT / BSD-3 /
Apache-2.0) keep their own licenses. The platform-side model and inference runtime
(Chinese-CLIP Apache-2.0, ONNX Runtime MIT) are not part of this SDK; their full license texts
ship with the release packages under `/usr/share/doc/hologram/licenses/`.
