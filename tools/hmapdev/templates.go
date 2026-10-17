package main

import _ "embed"

// tmplPlgJSON is the plg.json template
const tmplPlgJSON = `{
  "name": "{{.Plg.Name}}",
  "name_zh": "{{.Plg.NameZh}}",
  "name_en": "{{.Plg.NameEn}}",
  "version": "{{.Plg.Version}}",
  "description": "{{.Plg.Description}}",
  "author": "{{.Plg.Author}}",
  "entry": "{{.Plg.Entry}}",
  "sdk": "{{.Plg.SDK}}",
  "tags": [{{range $i, $t := .Plg.Tags}}{{if $i}}, {{end}}"{{$t}}"{{end}}],
  "targets": "{{.Plg.Targets}}",
  "rawfiles": []
}
`

const tmplGoMod = `module {{.ModulePath}}

go {{.GoVersion}}

require {{.SDKModule}} {{.SDKVersion}}
{{if .SDKLocalPath}}
// SDK 指向本机源码。
//
// ★ 为何默认走 replace 而不是直接拉模块（2026-10-08 更新）：
//   SDK 已从 gitcode 迁到 GitHub（module 路径 github.com/JianFeeeee/homeagentsdk）。
//   仓库虽在 GitHub，但本工程模板仍默认指向**本机源码**，原因与托管地无关：
//   （1）hmapdev 装的是本机 SDK（~/.homeagent/hmapdev/sdk），离线也能构建；
//   （2）插件必须与内核的 SDK 版本严格一致，走网络拉会引入版本漂移；
//   （3）内网/受限出口环境拉不到公共 proxy。
//   若你已有可访问的私有 proxy 或愿意走公网，可删掉本行。
replace {{.SDKModule}} => {{.SDKLocalPath}}
{{end}}`

const tmplPluginGo = `package main

import (
	"log"

	"github.com/JianFeeeee/homeagentsdk/sdk"
)

// ★★ 插件里**绝不能往 stdout 打印任何东西**（本注释是踩坑记录，2026-10-16）。
//
// 子进程插件的 **stdout 是 JSON-RPC 协议通道**（与内核的帧都走它），
// stderr 才是日志通道（SDK 侧 log.SetOutput(os.Stderr)）。
//
// 用 fmt.Printf 打一行日志 ⇒ 内核 readLoop 收到一段非 JSON 文本，
// 报「收到非法 JSON 帧（N 字节）」并**丢弃**。日志本身丢了，还刷了一条
// 看起来像协议故障的报错（极难定位到「是我自己 printf 的」）。
//
// ⇒ 一律用 log.Printf（默认走 stderr）。

type Plugin struct {
	name string
	sdk  *sdk.PluginSDK
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	p.sdk = s
	s.RegisterStopHandler(func() { log.Printf("[%s] stop handler running", p.name) })
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "plugin.{{.Plg.Name}}.example", Default: "hello", Type: "string",
		DisplayName: "示例配置", Description: "An example configuration key",
		Category: "{{.Plg.Name}}",
	})
	// ---- 通道（channel）：两个方向是分开的两件事 ----
	//
	// 入站 inputch ——「谁会往这个通道注入输入」。
	//   凡是用 s.InjectText*/InjectInput*/InjectInterrupt*(source, "<name>", ...) 注入的通道名，
	//   都要在这里登记：inputch 是内核最基本的**输入路由单位**，只有登记过的通道
	//   才能被「划给驻留子（resident sub-agent）」；没登记就划分会失败（inputch 未注册）。
	//   只登记出站通道时内核会兜底登记同名 inputch **并打告警**（兼容老插件）。
	chName := p.name
	_ = s.RegisterInputChannel(chName, sdk.ChannelDef{NoMemory: true})
	// 出站 output ——「output_send__<name> 的回复发给谁」。
	//    handler 收到 map：payload(string) / type(string) / meta(string|optional)。
	_ = s.RegisterOutputChannel(chName, sdk.CapText, "示例通道（回复由此返回）",
		sdk.ChannelDef{NoMemory: true}, func(args map[string]interface{}) (interface{}, error) {
			return map[string]interface{}{"status": "ok"}, nil
		})

	tp := p.name + "_"
	s.RegisterTool(tp+"hello", sdk.ToolDef{
		Name:        tp + "hello",
		Description: "A hello world tool",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		NoMemory:    false, // 工具输出对 LLM 注意力有信号价值时为 false，纯操作工具为 true
		// Cleaner: func(output string) string {
		//     // 工具输出参与向量化/jieba/蒸馏前，在此过滤噪音
		//     return output
		// },
	}, p.handleHello)
	log.Printf("[%s] started", p.name)
	return nil
}

func (p *Plugin) Stop() error { log.Printf("[%s] stopped", p.name); return nil }

func (p *Plugin) handleHello(args map[string]interface{}) (interface{}, error) {
	return map[string]interface{}{"content": "Hello from {{.Plg.Name}} plugin!"}, nil
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}
`

// fallbackLuaSDK 是 sdk.lua 的内嵌回退副本（单一事实源为 SDK 仓 sdk/lua/sdk.lua）。
//
// 为什么不再内联一份手写 mock：三份 sdk.lua（内核内嵌 / 工具链模板 / 项目副本）
// 各自漂移过一次，结果就是“mock 有、内核没有”或反过来。改为从 assets/sdk.lua
// 内嵌 + 由 SDK 仓同步脚本搬运，并配契约测试守住。
//
//go:embed assets/sdk.lua
var fallbackLuaSDK string

const tmplMainLua = `-- {{.Plg.Name}} plugin
local plugin = { name = "{{.Plg.Name}}" }
function plugin.start(sdk)
  sdk.log("info", "{{.Plg.Name}} starting...")

  -- 通道：入站与出站分开登记。
  -- 入站 inputch：凡是用 sdk.inject_text/sdk.inject_interrupt(source, "<name>", ...) 注入的通道名
  --   都要登记；只有登记过的通道才能被「划给驻留子」（没登记会报 inputch 未注册）。
  sdk.register_input_channel("{{.Plg.Name}}", { no_memory = true })
  -- 出站 output：output_send__<name> 的回复由 handler 处理
  sdk.register_output_channel("{{.Plg.Name}}", 1, "示例通道（回复由此返回）", { no_memory = true },
    function(args) return { status = "ok" } end)

  sdk.register_tool("{{.Plg.Name}}_hello", {
    description = "A hello world tool",
    parameters = { type = "object", properties = {} }
  }, function(args) return { content = "Hello from {{.Plg.Name}} plugin!" } end)
  sdk.log("info", "{{.Plg.Name}} started")
end
function plugin.stop() sdk.log("info", "{{.Plg.Name}} stopped") end
return plugin
`

// ============================================================
// Remote Device Adapter Templates
// ============================================================

const tmplRemoteDeviceMain = `#include <stdio.h>
#include <string.h>
#include <stdlib.h>

#include "ha_remotedevice.h"

/* ============================================================
 *  {{.Plg.Name}} — Remote Device Adapter
 *
 *  声明式远程设备接入示例。
 *  用户只需实现：
 *    1. ha_transport_t 的 4 个函数
 *    2. 声明 handlers 表（设备支持哪些命令 + 对应的处理函数）
 *  其余协议细节（WS 握手、hello/bind、心跳、重连、命令分发、结果回执）由 SDK 自动处理。
 * ============================================================ */

/* ====================== 传输层实现 ======================
 *
 * 请为你的平台实现以下 4 个函数：
 *   connect(ctx, host, port) — 建立 TCP 连接
 *   send(ctx, data, len)     — 发送数据
 *   recv(ctx, buf, len)      — 接收数据（阻塞，返回实际接收字节数）
 *   close(ctx)               — 关闭连接
 *
 * 示例：POSIX socket 实现
 */

#if defined(_WIN32) || defined(_WIN64)
/* Windows 平台需包含 winsock2.h */
#error "Please implement transport for your platform (see example below)"
#else
/* POSIX (Linux, macOS, ESP-IDF, Zephyr, etc.) */
#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#include <netdb.h>
#include <unistd.h>

struct transport_ctx {
    int sock;
};

static int transport_connect(void *ctx, const char *host, uint16_t port) {
    struct transport_ctx *tc = (struct transport_ctx *)ctx;
    struct hostent *he = gethostbyname(host);
    if (!he) return -1;
    tc->sock = socket(AF_INET, SOCK_STREAM, 0);
    if (tc->sock < 0) return -1;
    struct sockaddr_in addr;
    memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_port = htons(port);
    memcpy(&addr.sin_addr, he->h_addr_list[0], he->h_length);
    if (connect(tc->sock, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
        close(tc->sock);
        tc->sock = -1;
        return -1;
    }
    return 0;
}

static int transport_send(void *ctx, const uint8_t *data, int len) {
    struct transport_ctx *tc = (struct transport_ctx *)ctx;
    int sent = 0;
    while (sent < len) {
        int n = (int)send(tc->sock, data + sent, len - sent, 0);
        if (n <= 0) return -1;
        sent += n;
    }
    return sent;
}

static int transport_recv(void *ctx, uint8_t *buf, int len) {
    struct transport_ctx *tc = (struct transport_ctx *)ctx;
    int n = (int)recv(tc->sock, buf, len, 0);
    return n;
}

static void transport_close(void *ctx) {
    struct transport_ctx *tc = (struct transport_ctx *)ctx;
    if (tc->sock >= 0) {
        close(tc->sock);
        tc->sock = -1;
    }
}
#endif

/* ====================== 声明式命令处理 ======================
 *
 * 每个命令对应一个处理函数，通过填写 ha_cmd_result_t 返回数据。
 * SDK 自动回执结果，无需手动调用 send_result。
 *
 * 返回方式：
 *   1. 文本输出：填写 result->output
 *   2. 二进制数据：设置 result->has_binary=1 并填写 binary_data/len/mime
 *   3. 错误：设置 result->status=1 并填写 result->error
 *   4. 返回 HA_OK 表示处理成功，其他值表示处理失败
 */

/* ESP32-CAM 摄像头处理 */
static ha_status_t handle_camerasue(const char *req_id, const char *args,
                                    ha_cmd_result_t *result, void *userdata) {
    (void)req_id; (void)userdata;
    int duration = 0;
    if (args && args[0]) duration = atoi(args);
    printf("[camera] %s (duration=%ds)\n", duration ? "record" : "snapshot", duration);

    /* 返回文本结果（base64 图片） */
    result->status = 0;
    result->output = "data:image/jpeg;base64,/9j/4AAQ...";
    return HA_OK;
}

/* 屏幕截图处理 */
static ha_status_t handle_screensee(const char *req_id, const char *args,
                                    ha_cmd_result_t *result, void *userdata) {
    (void)req_id; (void)args; (void)userdata;
    printf("[screen] screenshot\n");
    result->status = 0;
    result->output = "data:image/png;base64,iVBORw0KGgo...";
    return HA_OK;
}

/* 语音播报处理 */
static ha_status_t handle_speakeruse(const char *req_id, const char *args,
                                     ha_cmd_result_t *result, void *userdata) {
    (void)req_id; (void)userdata;
    printf("[speaker] TTS: %s\n", args ? args : "");
    result->status = 0;
    result->output = "speakeruse done";
    return HA_OK;
}

/* 远程操控处理（computeruse） */
static ha_status_t handle_computeruse(const char *req_id, const char *args,
                                      ha_cmd_result_t *result, void *userdata) {
    (void)req_id; (void)userdata;
    const char *action = NULL;
    const char *json_str = NULL;
    ha_cmd_parse_json(args, &action, &json_str);
    printf("[computeruse] action=%s\n", action ? action : "unknown");
    result->status = 0;
    result->output = "computeruse done";
    return HA_OK;
}

/* 剪贴板读取 */
static ha_status_t handle_clipboardsee(const char *req_id, const char *args,
                                       ha_cmd_result_t *result, void *userdata) {
    (void)req_id; (void)args; (void)userdata;
    result->status = 0;
    result->output = "clipboard content";
    return HA_OK;
}

/* 剪贴板写入 */
static ha_status_t handle_clipboardsue(const char *req_id, const char *args,
                                       ha_cmd_result_t *result, void *userdata) {
    (void)req_id; (void)userdata;
    printf("[clipboard] write: %s\n", args ? args : "");
    result->status = 0;
    result->output = "clipboard written";
    return HA_OK;
}

/* 屏幕显示 */
static ha_status_t handle_screensue(const char *req_id, const char *args,
                                    ha_cmd_result_t *result, void *userdata) {
    (void)req_id; (void)userdata;
    printf("[screensue] show: %s\n", args ? args : "");
    result->status = 0;
    result->output = "screensue shown";
    return HA_OK;
}

/* Shell 命令处理 */
static ha_status_t handle_shell(const char *req_id, const char *args,
                                ha_cmd_result_t *result, void *userdata) {
    (void)req_id; (void)userdata;
    printf("[shell] cmd: %s\n", args ? args : "");
    result->status = 0;
    result->output = "shell output";
    return HA_OK;
}

/* 设备信息查询 */
static ha_status_t handle_deviceinfo(const char *req_id, const char *args,
                                     ha_cmd_result_t *result, void *userdata) {
    (void)req_id; (void)args; (void)userdata;
    result->status = 0;
    result->output = "{\"platform\":\"linux\",\"arch\":\"x86_64\"}";
    return HA_OK;
}

/* ====================== 连接状态回调 ====================== */

static void on_state(int connected, void *userdata) {
    (void)userdata;
    printf("[devicelink] state: %s\n", connected ? "connected" : "disconnected");
}

/* ====================== 主函数 ====================== */

int main(int argc, char *argv[]) {
    /* 传输层上下文 */
    struct transport_ctx tctx;
    tctx.sock = -1;

    ha_transport_t transport = {
        .connect = transport_connect,
        .send    = transport_send,
        .recv    = transport_recv,
        .close   = transport_close,
        .ctx     = &tctx,
    };

    /* ===== 声明式设备配置 ===== */

    /* 声明设备能力 */
    const char *caps[] = {
        "status", "cmdrun", "deviceinfo",
        "camerasue", "screensee", "speakeruse",
        "computeruse", "clipboardsee", "clipboardsue",
        "screensue",
        NULL
    };

    /* 声明命令处理表：设备支持哪些命令，以及对应的处理函数 */
    ha_cmd_handler_def_t handlers[] = {
        {.command = "shell",        .handler = handle_shell},
        {.command = "camerasue",    .handler = handle_camerasue},
        {.command = "screensee",    .handler = handle_screensee},
        {.command = "speakeruse",   .handler = handle_speakeruse},
        {.command = "computeruse",  .handler = handle_computeruse},
        {.command = "clipboardsee", .handler = handle_clipboardsee},
        {.command = "clipboardsue", .handler = handle_clipboardsue},
        {.command = "screensue",    .handler = handle_screensue},
        {.command = "deviceinfo",   .handler = handle_deviceinfo},
        {.command = NULL},  /* 标记结束 */
    };

    ha_config_t config = {
        .transport = transport,
        .server    = "127.0.0.1:9890",
        .token     = "your-token-here",
        .device = {
            .device_id = "{{.Plg.Name}}",
            .name      = "{{.Plg.NameEn}}",
            .kind      = "computer",
            .caps      = caps,
            .info_json = "{\"platform\":\"linux\",\"arch\":\"x86_64\"}",
        },
        .handlers  = handlers,   /* 声明式命令处理表 */
        .on_state  = on_state,
        .ping_interval = 30,
    };

    ha_client_t *client = ha_client_new(&config);
    if (!client) {
        fprintf(stderr, "Failed to create client\n");
        return 1;
    }

    printf("Starting remote device adapter: {{.Plg.Name}}\n");
    printf("  Server: %s\n", config.server);
    printf("  Device ID: %s\n", config.device.device_id);
    printf("  Kind: %s\n", config.device.kind);
    printf("  Caps: ");
    for (const char **p = caps; *p; p++) printf("%s ", *p);
    printf("\n");

    ha_status_t st = ha_client_start(client);
    if (st != HA_OK) {
        fprintf(stderr, "Failed to connect: %d\n", st);
        ha_client_destroy(client);
        return 1;
    }

    printf("Connected! Entering main loop...\n");

    /* 主循环 */
    while (1) {
        ha_status_t st = ha_client_process(client);
        if (st == HA_ERR_DISCONNECTED) {
            printf("Disconnected, exiting.\n");
            break;
        }
#if defined(_WIN32) || defined(_WIN64)
        Sleep(10);
#else
        usleep(10000);
#endif
    }

    ha_client_stop(client);
    ha_client_destroy(client);
    return 0;
}
`

const tmplRemoteDeviceCMake = `cmake_minimum_required(VERSION 3.10)
project({{.Plg.Name}} VERSION 0.1.0 LANGUAGES C)

# ============================================================
# {{.Plg.Name}} — Remote Device Adapter
# ============================================================

# 设置 SDK 路径（默认使用内置 SDK，也可通过 -DSDK_PATH=... 指定）
set(SDK_PATH "${CMAKE_CURRENT_SOURCE_DIR}/ha_remotedevice"
    CACHE PATH "Path to ha_remotedevice SDK")

# 添加 SDK 子目录
if(EXISTS "${SDK_PATH}/CMakeLists.txt")
    add_subdirectory(${SDK_PATH} ha_remotedevice)
else()
    message(FATAL_ERROR "ha_remotedevice SDK not found at ${SDK_PATH}")
endif()

# 创建设备适配器可执行文件
add_executable(${PROJECT_NAME}
    main.c
)

# 链接 SDK
target_link_libraries(${PROJECT_NAME} PRIVATE ha_remotedevice)

# 包含 SDK 头文件
target_include_directories(${PROJECT_NAME} PRIVATE
    ${HA_REMOTEDEVICE_INCLUDE_DIR}
)

# 编译选项
if(CMAKE_C_COMPILER_ID MATCHES "GNU|Clang")
    target_compile_options(${PROJECT_NAME} PRIVATE
        -Wall -Wextra -Wpedantic
        -Wno-unused-parameter
    )
endif()

# 安装
install(TARGETS ${PROJECT_NAME} RUNTIME DESTINATION bin)
`

const tmplReadme = `# {{.Plg.Name}}

{{.Plg.Description}}

## Build

` + "```bash" + `
hmapdev build
` + "```" + `

## Channels

入站与出站是分开登记的两件事：

| 方向 | API | 用途 |
|---|---|---|
| 入站 inputch | RegisterInputChannel(name, def) | 声明「谁会往这个通道注入输入」。**凡是用 InjectText*/InjectInput*/InjectInterrupt*(source, "<name>", ...) 注入的通道名都要登记** |
| 出站 output | RegisterOutputChannel(name, caps, desc, def, handler) | 声明 output_send__<name> 的回复发给谁；handler 收到 {payload,type,meta} |

def（ChannelDef）描述该通道在记忆计算层的行为：NoMemory: true = 该通道输入不进记忆；
Cleaner = 计算层清洗后再向量化/提关键词（原文不改）。

> 只登记出站通道、却用同名通道注入输入时，内核会兜底登记同名 inputch 并在日志里告警。
> 兜底只为兼容老插件 —— 请显式登记，让「这是入站通道」成为插件的明确意图。

## Install

Upload the .hmap file through the Plugin Manager API.
`
