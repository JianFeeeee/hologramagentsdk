package main

// Vikunja 插件（HologramAgent）
//
// 设计要点：
//   - 所有连接参数都是「插件配置项」（url / token / api_version / …），token 以 password 类型存储；
//     每次工具调用前重新读取配置，改完 token 不必重启插件。
//   - v1 与 v2 的差异（路径、方法、请求体形状、搜索参数名）集中在 call/compat 段处理，见各 Compatibility 注释。
//   - 另提供 vikunja_api 通用直通工具：任何本插件未封装的端点都能调，保证「完整能力」不出现死角。
//
// 版本差异速查（依据实例自带的 v1 swagger 与 v2 openapi 规范逐条核对）：
//
//	操作            v1                              v2
//	建任务          PUT  /projects/{id}/tasks        POST /projects/{id}/tasks
//	改任务          POST /tasks/{id}（必须整对象）    PATCH /tasks/{id}（merge-patch，可只给变更字段）
//	建评论          PUT  /tasks/{id}/comments        POST /tasks/{id}/comments
//	加标签          PUT  /tasks/{id}/labels          POST /tasks/{id}/labels（{"label_id":N}）
//	加指派          PUT  /tasks/{id}/assignees       POST /tasks/{id}/assignees
//	批量改          POST /tasks/bulk                 PUT  /tasks/bulk
//	搜任务          ?s=                              ?q=
//	时间跟踪        —                                /time-entries（v2 独有）
//	Todoist/Trello/微软待办 导入   有                 —（只有 TickTick/WeKan/CSV/Planka/Vikunja 文件）

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// ======== 配置键 ========

const (
	cfgURL        = "url"
	cfgToken      = "token"
	cfgAPIVersion = "api_version"
	cfgDefProject = "default_project_id"
	cfgMaxItems   = "max_items"
	cfgCompact    = "compact_output"
	cfgTimeout    = "timeout_seconds"
	cfgVerifyTLS  = "verify_tls"

	defaultURL = "https://vikunja.jianfgit.xyz"
)

// ======== 插件主体 ========

type Plugin struct {
	name string
	sdk  *sdk.PluginSDK
	http *http.Client

	baseURL  string
	token    string
	apiVer   string // "v1" 或 "v2"
	defProj  string
	maxItems int
	compact  bool
}

func NewPlugin(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Stop() error { return nil }

// ======== 参数/配置读取小工具 ========

func argStr(a map[string]interface{}, key string) string {
	if v, ok := a[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
		return strings.TrimSpace(fmt.Sprint(v))
	}
	return ""
}

func argInt(a map[string]interface{}, key string, def int) int {
	if v, ok := a[key]; ok && v != nil {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case int64:
			return int(n)
		case string:
			if n2, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
				return n2
			}
		}
	}
	return def
}

func argBool(a map[string]interface{}, key string) (bool, bool) {
	if v, ok := a[key]; ok && v != nil {
		switch b := v.(type) {
		case bool:
			return b, true
		case string:
			if b2, err := strconv.ParseBool(strings.TrimSpace(b)); err == nil {
				return b2, true
			}
		case float64:
			return b != 0, true
		}
	}
	return false, false
}

func argMap(a map[string]interface{}, key string) (map[string]interface{}, bool) {
	if v, ok := a[key]; ok && v != nil {
		if m, ok := v.(map[string]interface{}); ok {
			return m, true
		}
		if s, ok := v.(string); ok && strings.HasPrefix(strings.TrimSpace(s), "{") {
			var m map[string]interface{}
			if json.Unmarshal([]byte(s), &m) == nil {
				return m, true
			}
		}
	}
	return nil, false
}

// argID 读取 ID 参数：兼容数字与字符串两种传法
func argID(a map[string]interface{}, key string) string {
	return argStr(a, key)
}

// cfgStr 读取插件配置；core 侧 bool/int 以字符串存储，这里一并对齐
func cfgStr(st sdk.SettingsAPI, key, def string) string {
	if st == nil {
		return def
	}
	v, err := st.Get(key)
	if err != nil || v == nil {
		return def
	}
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func cfgInt(st sdk.SettingsAPI, key string, def int) int {
	s := cfgStr(st, key, "")
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

func cfgBool(st sdk.SettingsAPI, key string, def bool) bool {
	s := cfgStr(st, key, "")
	if s == "" {
		return def
	}
	if b, err := strconv.ParseBool(strings.TrimSpace(s)); err == nil {
		return b
	}
	return def
}

// ======== 配置装载 ========

// ensure 每次调用前重新装载配置：用户改完 token/地址即时生效，无需重启插件。
// sdk 为 nil（单测直接构造 Plugin）时保留已设置的字段。
func (p *Plugin) ensure() {
	if p.sdk == nil {
		if p.baseURL == "" {
			p.baseURL = defaultURL
		}
		if p.apiVer == "" {
			p.apiVer = "v2"
		}
		if p.maxItems <= 0 {
			p.maxItems = 25
		}
		if p.http == nil {
			p.http = &http.Client{Timeout: 20 * time.Second}
		}
		return
	}
	st := p.sdk.Settings()
	p.baseURL = strings.TrimRight(cfgStr(st, cfgURL, defaultURL), "/")
	p.token = strings.TrimSpace(cfgStr(st, cfgToken, ""))
	if v := cfgStr(st, cfgAPIVersion, "v2"); v == "v1" {
		p.apiVer = "v1"
	} else {
		p.apiVer = "v2"
	}
	p.defProj = cfgStr(st, cfgDefProject, "")
	if n := cfgInt(st, cfgMaxItems, 25); n > 0 {
		p.maxItems = n
	} else {
		p.maxItems = 25
	}
	p.compact = cfgBool(st, cfgCompact, true)

	timeout := cfgInt(st, cfgTimeout, 20)
	if timeout <= 0 {
		timeout = 20
	}
	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}
	if !cfgBool(st, cfgVerifyTLS, true) {
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // 用户显式关闭校验
	}
	p.http = client
}

// ======== HTTP 层 ========

type apiError struct {
	Status  int
	Code    int
	Message string
}

func (e *apiError) Error() string {
	hint := ""
	switch e.Status {
	case 401:
		hint = "（检查插件配置 vikunja.token 是否有效或已过期）"
	case 403:
		hint = "（API Token 权限范围不足，或该操作需要实例管理员）"
	case 404:
		hint = "（ID 不存在，或该端点在此 API 版本下不存在——可用 vikunja_status 看版本能力）"
	}
	if e.Code != 0 {
		return fmt.Sprintf("Vikunja HTTP %d code=%d: %s%s", e.Status, e.Code, e.Message, hint)
	}
	return fmt.Sprintf("Vikunja HTTP %d: %s%s", e.Status, e.Message, hint)
}

func qv(path string, kv ...string) string {
	if len(kv) == 0 {
		return path
	}
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			v.Set(kv[i], kv[i+1])
		}
	}
	if len(v) == 0 {
		return path
	}
	return path + "?" + v.Encode()
}

// call 按当前配置的 API 版本请求
func (p *Plugin) call(method, path string, body interface{}) (interface{}, error) {
	return p.callVer(p.apiVer, method, path, body, "")
}

// callVer 指定版本请求；ver 传 "v1"/"v2" 便于个别端点强制走另一版
func (p *Plugin) callVer(ver, method, path string, body interface{}, contentType string) (interface{}, error) {
	if p.token == "" {
		return nil, errors.New("未配置 API Token：请在插件配置 vikunja.token 填写（Vikunja → 设置 → API Tokens 生成，tk_ 开头）")
	}
	full := p.baseURL + "/api/" + ver + path
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("序列化请求体失败: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, full, rd)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		if contentType == "" {
			contentType = "application/json"
		}
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 Vikunja 失败（%s %s）: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 400 {
		var e struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Message != "" {
			return nil, &apiError{Status: resp.StatusCode, Code: e.Code, Message: e.Message}
		}
		return nil, &apiError{Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]interface{}{"ok": true}, nil
	}
	var out interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return string(raw), nil
	}
	return out, nil
}

// callVerified 返回 (结果, 错误)，供单测与兼容回退使用
func (p *Plugin) callVerified(method, path string, body interface{}) (interface{}, error) {
	return p.call(method, path, body)
}

// ======== 版本兼容表 ========

func (p *Plugin) isV1() bool { return p.apiVer == "v1" }

// 建立任务：v1=PUT，v2=POST（同一路径）
func (p *Plugin) createTaskMethod() string {
	if p.isV1() {
		return http.MethodPut
	}
	return http.MethodPost
}

// 更新任务：v2 走 PATCH(merge-patch)，v1 必须整对象 POST
func (p *Plugin) updateTaskMethod() string {
	if p.isV1() {
		return http.MethodPost
	}
	return http.MethodPatch
}

// 搜索参数名：v1 用 s，v2 用 q
func (p *Plugin) searchParam() string {
	if p.isV1() {
		return "s"
	}
	return "q"
}

func (p *Plugin) addCommentMethod() string {
	if p.isV1() {
		return http.MethodPut
	}
	return http.MethodPost
}

func (p *Plugin) addLabelMethod() string {
	if p.isV1() {
		return http.MethodPut
	}
	return http.MethodPost
}

func (p *Plugin) addAssigneeMethod() string {
	if p.isV1() {
		return http.MethodPut
	}
	return http.MethodPost
}

func (p *Plugin) bulkMethod() string {
	if p.isV1() {
		return http.MethodPost
	}
	return http.MethodPut
}

func (p *Plugin) createProjectMethod() string {
	if p.isV1() {
		return http.MethodPut
	}
	return http.MethodPost
}

func (p *Plugin) updateProjectMethod() string {
	if p.isV1() {
		return http.MethodPost
	}
	return http.MethodPatch
}

func (p *Plugin) createLabelMethod() string {
	if p.isV1() {
		return http.MethodPut
	}
	return http.MethodPost
}

func (p *Plugin) updateLabelMethod() string {
	if p.isV1() {
		return http.MethodPut
	}
	return http.MethodPatch
}

func (p *Plugin) createTeamMethod() string {
	if p.isV1() {
		return http.MethodPut
	}
	return http.MethodPost
}

func (p *Plugin) addTeamMemberMethod() string {
	if p.isV1() {
		return http.MethodPut
	}
	return http.MethodPost
}

func (p *Plugin) subscribeMethod() string {
	if p.isV1() {
		return http.MethodPut
	}
	return http.MethodPost
}

func (p *Plugin) notificationReadMethod() string {
	if p.isV1() {
		return http.MethodPost
	}
	return http.MethodPut
}

func (p *Plugin) createWebhookMethod() string {
	if p.isV1() {
		return http.MethodPut
	}
	return http.MethodPost
}

func (p *Plugin) updateWebhookMethod() string {
	if p.isV1() {
		return http.MethodPost
	}
	return http.MethodPut
}

func (p *Plugin) uploadAttachmentMethod() string {
	if p.isV1() {
		return http.MethodPut
	}
	return http.MethodPost
}

// ======== 输出精简 ========

func briefTask(v interface{}) interface{} {
	m, ok := v.(map[string]interface{})
	if !ok {
		return v
	}
	out := map[string]interface{}{
		"id":         m["id"],
		"title":      m["title"],
		"done":       m["done"],
		"project_id": m["project_id"],
	}
	for _, k := range []string{"due_date", "start_date", "end_date", "priority", "percent_done", "bucket_id", "is_favorite", "repeat_after"} {
		if val, ok := m[k]; ok && val != nil && fmt.Sprint(val) != "" && fmt.Sprint(val) != "0" && fmt.Sprint(val) != "false" {
			out[k] = val
		}
	}
	if labels, ok := m["labels"].([]interface{}); ok && len(labels) > 0 {
		names := make([]string, 0, len(labels))
		for _, l := range labels {
			if lm, ok := l.(map[string]interface{}); ok {
				if t := fmt.Sprint(lm["title"]); t != "<nil>" && t != "" {
					names = append(names, t)
				}
			}
		}
		if len(names) > 0 {
			out["labels"] = names
		}
	}
	if as, ok := m["assignees"].([]interface{}); ok && len(as) > 0 {
		names := make([]string, 0, len(as))
		for _, a := range as {
			if am, ok := a.(map[string]interface{}); ok {
				if u := fmt.Sprint(am["username"]); u != "<nil>" && u != "" {
					names = append(names, u)
				}
			}
		}
		if len(names) > 0 {
			out["assignees"] = names
		}
	}
	return out
}

func (p *Plugin) briefTasks(v interface{}) interface{} {
	arr, ok := v.([]interface{})
	if !ok {
		if m, ok := v.(map[string]interface{}); ok {
			if items, ok := m["items"].([]interface{}); ok {
				return p.projectList(items, briefTask)
			}
		}
		return v
	}
	return p.projectList(arr, briefTask)
}

func (p *Plugin) projectList(items []interface{}, fn func(interface{}) interface{}) []interface{} {
	out := make([]interface{}, 0, len(items))
	for _, it := range items {
		out = append(out, fn(it))
	}
	return out
}

func briefProject(v interface{}) interface{} {
	m, ok := v.(map[string]interface{})
	if !ok {
		return v
	}
	out := map[string]interface{}{"id": m["id"], "title": m["title"]}
	for _, k := range []string{"is_archived", "is_favorite", "hex_color", "parent_project_id", "position"} {
		if val, ok := m[k]; ok && val != nil && fmt.Sprint(val) != "false" && fmt.Sprint(val) != "0" && fmt.Sprint(val) != "" {
			out[k] = val
		}
	}
	return out
}

func briefLabel(v interface{}) interface{} {
	m, ok := v.(map[string]interface{})
	if !ok {
		return v
	}
	out := map[string]interface{}{"id": m["id"], "title": m["title"]}
	if hc, ok := m["hex_color"]; ok && hc != nil && fmt.Sprint(hc) != "" {
		out["hex_color"] = hc
	}
	return out
}

// briefList 按 fn 投影数组（非数组原样返回）
func briefList(v interface{}, fn func(interface{}) interface{}) interface{} {
	arr, ok := v.([]interface{})
	if !ok {
		return v
	}
	out := make([]interface{}, 0, len(arr))
	for _, it := range arr {
		out = append(out, fn(it))
	}
	return out
}

// ======== 工具注册 ========

func schemas(props map[string]interface{}, required ...string) map[string]interface{} {
	m := map[string]interface{}{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func pStr(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}

func pInt(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "integer", "description": desc}
}

func pBool(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "boolean", "description": desc}
}

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	p.sdk = s
	s.SetAutoRestart(true)

	st := s.Settings()
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgURL, Type: "string", Default: defaultURL,
		DisplayName: "Vikunja 地址", Description: "站点根地址（不要带 /api）",
		Category: "vikunja",
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgToken, Type: "password", Default: "", Secret: true,
		DisplayName: "API Token",
		Description: "Vikunja → 设置 → API Tokens 生成（tk_ 开头）。Token 的权限范围决定本插件能力上限：勾选全部范围即为完整能力",
		Category:    "vikunja", Required: true,
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgAPIVersion, Type: "select", Default: "v2", Options: []string{"v2", "v1"},
		DisplayName: "API 版本",
		Description: "v2（推荐，标准 REST，含时间跟踪等新能力）；v1 用于 v2 暂未提供的端点（如 Todoist/Trello/微软待办 导入）",
		Category:    "vikunja",
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgDefProject, Type: "string", Default: "",
		DisplayName: "默认项目 ID", Description: "新建任务未指定 project_id 时落到这里（留空则必须显式指定）",
		Category: "vikunja",
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgMaxItems, Type: "int", Default: "25", Min: 1, Max: 200,
		DisplayName: "列表返回条数", Description: "列表类工具的默认条数，控制上下文体积",
		Category: "vikunja",
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgCompact, Type: "bool", Default: "true",
		DisplayName: "精简输出", Description: "任务列表只返回关键字段；关闭则返回 Vikunja 完整对象（体积大）",
		Category: "vikunja",
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgTimeout, Type: "int", Default: "20", Min: 1, Max: 300,
		DisplayName: "超时（秒）", Category: "vikunja",
	})
	st.RegisterDef(sdk.ConfigDef{
		Key: cfgVerifyTLS, Type: "bool", Default: "true",
		DisplayName: "校验 TLS 证书", Description: "自签证书站点可关闭（不建议）",
		Category: "vikunja",
	})

	p.ensure()
	p.registerTools()
	log.Printf("[%s] started (url=%s api=%s)\n", p.name, p.baseURL, p.apiVer)
	return nil
}

func (p *Plugin) registerTools() {
	tp := p.name + "_"

	// ---- 自检 ----
	p.sdk.RegisterTool(tp+"status", sdk.ToolDef{
		Name: tp + "status", Description: "检查 Vikunja 连接与配置：地址、当前 Token 对应的用户、API 版本、服务器能力（含 CalDAV 地址）。排查前的第一步",
		Parameters: schemas(map[string]interface{}{}),
	}, p.handleStatus)

	// ---- 任务核心 ----
	p.sdk.RegisterTool(tp+"tasks", sdk.ToolDef{
		Name: tp + "tasks", Description: "列出/筛选任务。可按项目、完成状态、截止时间、标签、关键词筛选；返回精简字段",
		Parameters: schemas(map[string]interface{}{
			"project_id": pStr("项目 ID；留空则跨项目查询"),
			"done":       pStr("完成状态：any|done|undone（默认 undone）"),
			"due":        pStr("截止筛选：any|overdue|today|this_week|this_month|no_due（默认 any）"),
			"search":     pStr("标题关键词"),
			"filter":     pStr("Vikunja 原生过滤表达式，如 done = false && due_date < now+48h；与 due/search 可叠加"),
			"sort_by":    pStr("排序字段，如 due_date|priority|title|created（默认 due_date）"),
			"order_by":   pStr("asc|desc（默认 asc）"),
			"page":       pInt("页码，从 1 开始"),
			"limit":      pInt("返回条数，默认取插件配置的 max_items"),
		}),
	}, p.handleTasksList)

	p.sdk.RegisterTool(tp+"task_get", sdk.ToolDef{
		Name: tp + "task_get", Description: "获取单个任务的完整信息（含描述、提醒、重复规则、附件、评论数等）",
		Parameters: schemas(map[string]interface{}{"id": pStr("任务 ID")}, "id"),
	}, p.handleTaskGet)

	p.sdk.RegisterTool(tp+"task_create", sdk.ToolDef{
		Name: tp + "task_create", Description: "新建任务。可用自然语言式参数：标题、描述、截止时间、优先级、标签、指派",
		Parameters: schemas(map[string]interface{}{
			"project_id":   pStr("项目 ID（留空用插件配置的默认项目）"),
			"title":        pStr("任务标题"),
			"description":  pStr("任务描述（支持 Markdown）"),
			"due_date":     pStr("截止时间，RFC3339 或 2006-01-02 15:04；也接受 today/tomorrow/+3d 这类相对写法"),
			"start_date":   pStr("开始时间，格式同 due_date"),
			"end_date":     pStr("结束时间，格式同 due_date"),
			"priority":     pInt("优先级：0 无、1 低、2 中、3 高、4 紧急、5 最高"),
			"percent_done": pInt("完成百分比 0-100"),
			"labels":       pStr("标签 ID，逗号分隔"),
			"assignees":    pStr("指派用户名或用户 ID，逗号分隔"),
			"bucket_id":    pInt("看板桶 ID（可选）"),
		}, "title"),
	}, p.handleTaskCreate)

	p.sdk.RegisterTool(tp+"task_update", sdk.ToolDef{
		Name: tp + "task_update", Description: "修改任务的任意字段（只传要改的字段即可）。改项目、改截止、改优先级、改描述都用它",
		Parameters: schemas(map[string]interface{}{
			"id":           pStr("任务 ID"),
			"title":        pStr("新标题"),
			"description":  pStr("新描述"),
			"due_date":     pStr("新截止时间；传 clear 表示清空"),
			"start_date":   pStr("新开始时间；传 clear 表示清空"),
			"end_date":     pStr("新结束时间；传 clear 表示清空"),
			"priority":     pInt("新优先级 0-5"),
			"percent_done": pInt("完成百分比 0-100"),
			"done":         pBool("是否完成"),
			"project_id":   pStr("移动到该项目的 ID"),
			"repeat_after": pInt("重复间隔（秒），0 表示不重复"),
			"repeat_mode":  pInt("重复模式 0-2"),
			"is_favorite":  pBool("是否收藏"),
			"bucket_id":    pInt("移动到看板桶"),
			"position":     pInt("在看板/列表中的位置"),
			"raw_patch":    pStr("高级：直接给出 JSON 对象覆盖上述字段（v2 走 merge-patch；v1 会合并到完整对象后提交）"),
		}, "id"),
	}, p.handleTaskUpdate)

	p.sdk.RegisterTool(tp+"task_done", sdk.ToolDef{
		Name: tp + "task_done", Description: "标记任务完成或取消完成（语义化封装，比 task_update 更省参数）",
		Parameters: schemas(map[string]interface{}{
			"id":   pStr("任务 ID"),
			"done": pBool("true 完成 / false 取消完成（默认 true）"),
		}, "id"),
	}, p.handleTaskDone)

	p.sdk.RegisterTool(tp+"task_delete", sdk.ToolDef{
		Name: tp + "task_delete", Description: "删除任务（不可恢复）",
		Parameters: schemas(map[string]interface{}{"id": pStr("任务 ID")}, "id"),
		NoMemory:   true,
	}, p.handleTaskDelete)

	p.sdk.RegisterTool(tp+"task_bulk", sdk.ToolDef{
		Name: tp + "task_bulk", Description: "批量修改任务：一次对多个任务设置完成状态/项目/优先级/截止时间/标签",
		Parameters: schemas(map[string]interface{}{
			"task_ids":   pStr("任务 ID，逗号分隔"),
			"done":       pBool("批量设完成状态"),
			"project_id": pStr("批量移动到项目"),
			"priority":   pInt("批量设优先级"),
			"due_date":   pStr("批量设截止时间（clear 清空）"),
			"label_ids":  pStr("批量覆盖标签（标签 ID 逗号分隔，空字符串表示清空标签）"),
		}, "task_ids"),
	}, p.handleTaskBulk)

	// ---- 任务从属对象 ----
	p.sdk.RegisterTool(tp+"task_assignees", sdk.ToolDef{
		Name: tp + "task_assignees", Description: "查看/增删任务的指派对象（负责人）",
		Parameters: schemas(map[string]interface{}{
			"action": pStr("list|add|remove（默认 list）"),
			"id":     pStr("任务 ID"),
			"user":   pStr("用户名或用户 ID（add/remove 时必填）"),
		}, "id"),
	}, p.handleTaskAssignees)

	p.sdk.RegisterTool(tp+"task_labels", sdk.ToolDef{
		Name: tp + "task_labels", Description: "查看/增删任务的标签",
		Parameters: schemas(map[string]interface{}{
			"action":   pStr("list|add|remove（默认 list）"),
			"id":       pStr("任务 ID"),
			"label_id": pStr("标签 ID（add/remove 时必填）"),
		}, "id"),
	}, p.handleTaskLabels)

	p.sdk.RegisterTool(tp+"task_comments", sdk.ToolDef{
		Name: tp + "task_comments", Description: "任务的评论：列出、新增、修改、删除",
		Parameters: schemas(map[string]interface{}{
			"action":     pStr("list|add|update|delete（默认 list）"),
			"id":         pStr("任务 ID"),
			"comment":    pStr("评论内容（add/update 时必填）"),
			"comment_id": pStr("评论 ID（update/delete 时必填）"),
		}, "id"),
	}, p.handleTaskComments)

	p.sdk.RegisterTool(tp+"task_relations", sdk.ToolDef{
		Name: tp + "task_relations", Description: "任务关联：子任务、前置/后置依赖、相关任务",
		Parameters: schemas(map[string]interface{}{
			"action":        pStr("list|add|remove（默认 list）"),
			"id":            pStr("任务 ID"),
			"other_task_id": pStr("另一个任务 ID（add/remove 时必填）"),
			"kind":          pStr("关系类型：subtask|parenttask|related|duplicateof|blocking|blocked|precedes|follows（默认 subtask）"),
		}, "id"),
	}, p.handleTaskRelations)

	p.sdk.RegisterTool(tp+"task_attachments", sdk.ToolDef{
		Name: tp + "task_attachments", Description: "任务附件：列出、上传本地文件、删除（下载请用 vikunja_api 取 attachment 的 URL/内容）",
		Parameters: schemas(map[string]interface{}{
			"action":        pStr("list|upload|delete（默认 list）"),
			"id":            pStr("任务 ID"),
			"path":          pStr("要上传的本地文件绝对路径（upload 时必填）"),
			"attachment_id": pStr("附件 ID（delete 时必填）"),
		}, "id"),
	}, p.handleTaskAttachments)

	// ---- 项目 / 视图 / 桶 ----
	p.sdk.RegisterTool(tp+"projects", sdk.ToolDef{
		Name: tp + "projects", Description: "项目：列出、查看、新建、修改、删除、归档/取消归档",
		Parameters: schemas(map[string]interface{}{
			"action":            pStr("list|get|create|update|delete|archive（默认 list）"),
			"id":                pStr("项目 ID（get/update/delete/archive）"),
			"title":             pStr("项目名称（create/update）"),
			"description":       pStr("项目描述"),
			"hex_color":         pStr("颜色，如 #38bdf8"),
			"parent_project_id": pStr("父项目 ID（做子项目）"),
			"archived":          pBool("归档状态（archive）"),
			"is_favorite":       pBool("是否收藏"),
			"raw":               pStr("高级：直接给出 JSON 对象作为请求体"),
		}),
	}, p.handleProjects)

	p.sdk.RegisterTool(tp+"project_views", sdk.ToolDef{
		Name: tp + "project_views", Description: "项目的视图与看板桶：列视图、列桶内任务、把任务移入某个桶（看板拖动的等价操作）",
		Parameters: schemas(map[string]interface{}{
			"action":     pStr("list_views|list_buckets|bucket_tasks|move_to_bucket（默认 list_views）"),
			"project_id": pStr("项目 ID"),
			"view_id":    pStr("视图 ID"),
			"bucket_id":  pStr("桶 ID（bucket_tasks/move_to_bucket）"),
			"task_id":    pStr("任务 ID（move_to_bucket）"),
		}, "project_id"),
	}, p.handleProjectViews)

	// ---- 标签 / 保存的筛选器 ----
	p.sdk.RegisterTool(tp+"labels", sdk.ToolDef{
		Name: tp + "labels", Description: "标签：列出、新建、修改、删除",
		Parameters: schemas(map[string]interface{}{
			"action":    pStr("list|create|update|delete（默认 list）"),
			"id":        pStr("标签 ID（update/delete）"),
			"title":     pStr("标签名"),
			"hex_color": pStr("颜色，如 #22c55e"),
		}),
	}, p.handleLabels)

	p.sdk.RegisterTool(tp+"filters", sdk.ToolDef{
		Name: tp + "filters", Description: "保存的筛选器（Saved Filter）：列出、新建、修改、删除",
		Parameters: schemas(map[string]interface{}{
			"action": pStr("list|create|update|delete|get（默认 list）"),
			"id":     pStr("筛选器 ID"),
			"title":  pStr("名称"),
			"filter": pStr("过滤表达式，如 done = false && due_date < now+7d"),
			"raw":    pStr("高级：直接给出 JSON 请求体"),
		}),
	}, p.handleFilters)

	// ---- 团队 / 分享 ----
	p.sdk.RegisterTool(tp+"teams", sdk.ToolDef{
		Name: tp + "teams", Description: "团队与成员：列出、新建、改名、删除、加成员、移成员",
		Parameters: schemas(map[string]interface{}{
			"action":   pStr("list|get|create|update|delete|members|add_member|remove_member（默认 list）"),
			"id":       pStr("团队 ID"),
			"name":     pStr("团队名"),
			"username": pStr("成员用户名（add_member/remove_member）"),
		}),
	}, p.handleTeams)

	p.sdk.RegisterTool(tp+"sharing", sdk.ToolDef{
		Name: tp + "sharing", Description: "项目分享：列出分享、按用户/团队授权与撤权、链接分享（生成/查看/删除公开链接）",
		Parameters: schemas(map[string]interface{}{
			"action":     pStr("list|add_user|update_user|remove_user|add_team|remove_team|links|create_link|update_link|delete_link（默认 list）"),
			"project_id": pStr("项目 ID"),
			"share_id":   pStr("用户分享 ID / 团队分享 ID / 链接分享 ID"),
			"username":   pStr("用户名（add_user）"),
			"team_id":    pStr("团队 ID（add_team）"),
			"permission": pInt("权限位：0 只读、1 读写、2 管理（依 Vikunja 版本语义）"),
			"right":      pInt("链接分享权限位（create_link）"),
			"password":   pStr("链接分享密码（create_link，可选）"),
			"raw":        pStr("高级：直接给出 JSON 请求体"),
		}, "project_id"),
	}, p.handleSharing)

	// ---- 通知 / 订阅 / Webhook ----
	p.sdk.RegisterTool(tp+"notifications", sdk.ToolDef{
		Name: tp + "notifications", Description: "通知中心：列出、标记单条已读、全部已读",
		Parameters: schemas(map[string]interface{}{
			"action": pStr("list|mark_read|mark_all_read（默认 list）"),
			"id":     pStr("通知 ID（mark_read）"),
		}),
	}, p.handleNotifications)

	p.sdk.RegisterTool(tp+"subscriptions", sdk.ToolDef{
		Name: tp + "subscriptions", Description: "订阅/退订某个实体（task/project 等），订阅后可收到变更通知",
		Parameters: schemas(map[string]interface{}{
			"action":    pStr("subscribe|unsubscribe（默认 subscribe）"),
			"entity":    pStr("实体类型：task|project|namespace 等"),
			"entity_id": pStr("实体 ID"),
		}, "entity", "entity_id"),
	}, p.handleSubscriptions)

	p.sdk.RegisterTool(tp+"webhooks", sdk.ToolDef{
		Name: tp + "webhooks", Description: "Webhook：列出、新建、修改、删除、查看可用事件",
		Parameters: schemas(map[string]interface{}{
			"action":     pStr("list|create|update|delete|events（默认 list）"),
			"project_id": pStr("项目 ID"),
			"id":         pStr("Webhook ID（update/delete）"),
			"target_url": pStr("回调地址（create）"),
			"events":     pStr("事件名，逗号分隔，如 task.created,task.updated；可用 events 动作查全部"),
			"secret":     pStr("签名密钥（可选）"),
			"raw":        pStr("高级：直接给出 JSON 请求体"),
		}, "project_id"),
	}, p.handleWebhooks)

	// ---- 时间跟踪（v2 独有）----
	p.sdk.RegisterTool(tp+"time_entries", sdk.ToolDef{
		Name: tp + "time_entries", Description: "时间跟踪（仅 v2）：列出耗时记录、补录、修改、删除、开始/停止计时器",
		Parameters: schemas(map[string]interface{}{
			"action":     pStr("list|create|update|delete|timer_start|timer_stop|task_entries（默认 list）"),
			"id":         pStr("时长记录 ID / 任务 ID（task_entries 时为任务 ID）"),
			"task_id":    pStr("任务 ID（create/timer_start）"),
			"project_id": pStr("项目 ID（create，或与 task 二选一）"),
			"seconds":    pInt("耗时秒数（create/update）"),
			"start_time": pStr("开始时间（create）"),
			"end_time":   pStr("结束时间（create）"),
			"comment":    pStr("备注"),
		}),
	}, p.handleTimeEntries)

	// ---- 导入 ----
	p.sdk.RegisterTool(tp+"migrate", sdk.ToolDef{
		Name: tp + "migrate", Description: "从其它待办系统导入数据。自动选用正确 API 版本：Todoist/Trello/微软待办 只有 v1 支持，TickTick/WeKan/CSV/Planka/Vikunja 文件 用 v2",
		Parameters: schemas(map[string]interface{}{
			"action":  pStr("sources|auth_url|start|status（默认 sources）"),
			"source":  pStr("来源名：ticktick|wekan|csv|planka|vikunja-file|todoist|trello|microsoft-todo"),
			"code":    pStr("OAuth 回调 code（Todoist/Trello/微软待办）"),
			"payload": pStr("高级：直接给出 JSON 请求体（如 CSV 导入的配置对象）"),
		}),
	}, p.handleMigrate)

	// ---- 账号 / 实例管理 ----
	p.sdk.RegisterTool(tp+"user", sdk.ToolDef{
		Name: tp + "user", Description: "当前账号：我的信息、通用设置、登录会话（可踢设备）、API Token 管理、CalDAV 凭据",
		Parameters: schemas(map[string]interface{}{
			"action":       pStr("me|settings|update_settings|sessions|revoke_session|tokens|create_token|delete_token|change_password|caldav（默认 me）"),
			"settings":     pStr("高级：update_settings 的 JSON 对象"),
			"session_id":   pStr("会话 ID（revoke_session）"),
			"token_id":     pStr("Token ID（delete_token）"),
			"title":        pStr("新 Token 名称（create_token）"),
			"permissions":  pStr("新 Token 权限列表（create_token），可用 vikunja_api 调 /routes 查看可用项"),
			"old_password": pStr("旧密码（change_password）"),
			"new_password": pStr("新密码（change_password）"),
		}),
	}, p.handleUser)

	p.sdk.RegisterTool(tp+"admin", sdk.ToolDef{
		Name: tp + "admin", Description: "实例管理（需要实例管理员）：总览、用户列表/新建/删除/提权/停用、改用户密码、项目归属转移",
		Parameters: schemas(map[string]interface{}{
			"action":     pStr("overview|users|create_user|delete_user|set_admin|set_status|set_password|projects|reassign_owner（默认 overview）"),
			"user_id":    pStr("用户 ID"),
			"username":   pStr("新用户用户名（create_user）"),
			"email":      pStr("邮箱（create_user）"),
			"password":   pStr("密码（create_user/set_password）"),
			"admin":      pBool("是否实例管理员（set_admin）"),
			"status":     pStr("0 启用 / 1 停用（set_status）"),
			"project_id": pStr("项目 ID（reassign_owner）"),
			"owner_id":   pStr("新所有者用户 ID（reassign_owner）"),
		}),
	}, p.handleAdmin)

	p.sdk.RegisterTool(tp+"reactions", sdk.ToolDef{
		Name: tp + "reactions", Description: "表情回应：给任务或评论加/删/查 reaction",
		Parameters: schemas(map[string]interface{}{
			"action": pStr("list|add|remove（默认 list）"),
			"kind":   pStr("实体类型：tasks|comments"),
			"id":     pStr("实体 ID"),
			"value":  pStr("emoji 字符（add/remove）"),
		}, "kind", "id"),
	}, p.handleReactions)

	// ---- 兜底直通 ----
	p.sdk.RegisterTool(tp+"api", sdk.ToolDef{
		Name: tp + "api", Description: "通用 API 直通：调用任意 Vikunja 端点（本插件未封装的能力走这里）。path 相对 /api/<版本>，例如 GET /tasks/12、POST /tasks/12/relations",
		Parameters: schemas(map[string]interface{}{
			"method":      pStr("GET|POST|PUT|PATCH|DELETE（默认 GET）"),
			"path":        pStr("路径，如 /tasks 或 /projects/3/tasks"),
			"body":        pStr("JSON 字符串或对象（写操作时）"),
			"api_version": pStr("强制使用 v1 或 v2（留空用插件配置）"),
		}, "path"),
	}, p.handleRawAPI)
}

// ======== 处理器实现 ========

func (p *Plugin) handleStatus(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	out := map[string]interface{}{
		"url":                p.baseURL,
		"api_version":        p.apiVer,
		"token_set":          p.token != "",
		"caldav_url":         p.baseURL + "/dav/",
		"default_project_id": p.defProj,
	}
	info, err := p.callVer("v1", http.MethodGet, "/info", nil, "")
	if err == nil {
		out["server"] = info
	}
	if p.token == "" {
		out["user"] = nil
		out["hint"] = "尚未配置 API Token：在插件配置 vikunja.token 填入（Vikunja → 设置 → API Tokens）"
		return out, nil
	}
	me, err := p.call(http.MethodGet, "/user", nil)
	if err != nil {
		out["user_error"] = err.Error()
		return out, nil
	}
	if m, ok := me.(map[string]interface{}); ok {
		out["user"] = map[string]interface{}{
			"id": m["id"], "username": m["username"], "email": m["email"],
		}
	} else {
		out["user"] = me
	}
	out["note"] = "v1 独有能力：Todoist/Trello/微软待办 导入；v2 独有：时间跟踪、Bot、登录会话管理、看板桶任务、批量接口"
	return out, nil
}

func (p *Plugin) handleTasksList(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	limit := argInt(args, "limit", p.maxItems)
	page := argInt(args, "page", 1)
	if page <= 0 {
		page = 1
	}
	if limit <= 0 {
		limit = 25
	}
	donePref := strings.ToLower(argStr(args, "done"))
	if donePref == "" {
		donePref = "undone"
	}
	filter := argStr(args, "filter")
	switch donePref {
	case "done":
		filter = joinFilter(filter, "done = true")
	case "undone":
		filter = joinFilter(filter, "done = false")
	}
	switch strings.ToLower(argStr(args, "due")) {
	case "overdue":
		filter = joinFilter(filter, "due_date < now && due_date != null && done = false")
	case "today":
		filter = joinFilter(filter, "due_date < now+1d && due_date > now-1d")
	case "this_week":
		filter = joinFilter(filter, "due_date < now+7d")
	case "this_month":
		filter = joinFilter(filter, "due_date < now+31d")
	case "no_due":
		filter = joinFilter(filter, "due_date = null")
	}

	sortBy := argStr(args, "sort_by")
	orderBy := argStr(args, "order_by")
	if sortBy == "" {
		sortBy = "due_date"
	}
	if orderBy == "" {
		orderBy = "asc"
	}

	projectID := argID(args, "project_id")
	search := argStr(args, "search")
	base := "/tasks"
	if projectID != "" {
		base = "/projects/" + projectID + "/tasks"
	}
	path := qv(base,
		"page", strconv.Itoa(page),
		"per_page", strconv.Itoa(limit),
		"filter", filter,
		"sort_by", sortBy,
		"order_by", orderBy,
		p.searchParam(), search,
	)
	res, err := p.call(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if !p.compact {
		return res, nil
	}
	return map[string]interface{}{
		"count": len(asSlice(res)),
		"tasks": p.briefTasks(res),
	}, nil
}

func asSlice(v interface{}) []interface{} {
	if arr, ok := v.([]interface{}); ok {
		return arr
	}
	if m, ok := v.(map[string]interface{}); ok {
		if items, ok := m["items"].([]interface{}); ok {
			return items
		}
	}
	return nil
}

func joinFilter(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + " && " + b
}

func (p *Plugin) handleTaskGet(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	id := argID(args, "id")
	if id == "" {
		return nil, errors.New("缺少参数 id（任务 ID）")
	}
	return p.call(http.MethodGet, "/tasks/"+id, nil)
}

// taskBody 由工具参数拼出 Vikunja Task 风格的请求体（只含显式给出的字段）
func taskBody(args map[string]interface{}, includeTitle bool) map[string]interface{} {
	body := map[string]interface{}{}
	if includeTitle {
		if v := argStr(args, "title"); v != "" {
			body["title"] = v
		}
	} else if v := argStr(args, "title"); v != "" {
		body["title"] = v
	}
	if v := argStr(args, "description"); v != "" {
		body["description"] = v
	}
	for _, k := range []string{"due_date", "start_date", "end_date"} {
		if v := argStr(args, k); v != "" {
			if strings.EqualFold(v, "clear") {
				body[k] = nil
			} else {
				body[k] = normalizeTime(v)
			}
		}
	}
	if v, ok := argBool(args, "done"); ok {
		body["done"] = v
	}
	if v, ok := argBool(args, "is_favorite"); ok {
		body["is_favorite"] = v
	}
	for _, k := range []string{"priority", "percent_done", "repeat_after", "repeat_mode", "bucket_id", "position"} {
		if _, present := args[k]; present {
			body[k] = argInt(args, k, 0)
		}
	}
	if v := argID(args, "project_id"); v != "" {
		// 必须是 JSON 数字：发字符串 "1" 会被 Vikunja 判 422 expected integer
		// （v2 body.project_id 校验 integer；线上实测）
		body["project_id"] = parseID(v)
	}
	return body
}

// normalizeTime 容忍几种常见写法；无法解析时原样透传（Vikunja 会给出明确报错）
func normalizeTime(v string) interface{} {
	s := strings.TrimSpace(v)
	now := time.Now()
	switch strings.ToLower(s) {
	case "today":
		return time.Date(now.Year(), now.Month(), now.Day(), 18, 0, 0, 0, now.Location()).Format(time.RFC3339)
	case "tomorrow":
		t := now.AddDate(0, 0, 1)
		return time.Date(t.Year(), t.Month(), t.Day(), 18, 0, 0, 0, t.Location()).Format(time.RFC3339)
	}
	if strings.HasPrefix(s, "+") && strings.HasSuffix(s, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(s, "+"), "d")); err == nil {
			return now.AddDate(0, 0, n).Format(time.RFC3339)
		}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, now.Location()); err == nil {
			return t.Format(time.RFC3339)
		}
	}
	return s
}

func (p *Plugin) handleTaskCreate(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	pid := argID(args, "project_id")
	if pid == "" {
		pid = p.defProj
	}
	if pid == "" {
		return nil, errors.New("未指定 project_id，且插件配置里没有默认项目（vikunja.default_project_id）")
	}
	if argStr(args, "title") == "" {
		return nil, errors.New("缺少参数 title（任务标题）")
	}
	body := taskBody(args, true)
	res, err := p.call(p.createTaskMethod(), "/projects/"+pid+"/tasks", body)
	if err != nil {
		return nil, err
	}
	// 标签与指派需要任务 ID，创建后补挂
	taskID := ""
	if m, ok := res.(map[string]interface{}); ok {
		taskID = fmt.Sprint(m["id"])
	}
	notes := []string{}
	if taskID != "" && taskID != "<nil>" {
		if labels := argStr(args, "labels"); labels != "" {
			for _, l := range splitCSV(labels) {
				if _, err := p.call(p.addLabelMethod(), "/tasks/"+taskID+"/labels", p.labelBody(l)); err != nil {
					notes = append(notes, "标签 "+l+" 添加失败: "+err.Error())
				}
			}
		}
		if asg := argStr(args, "assignees"); asg != "" {
			for _, u := range splitCSV(asg) {
				body, berr := p.assigneeBody(u)
				if berr != nil {
					notes = append(notes, "指派 "+u+" 失败: "+berr.Error())
					continue
				}
				if _, err := p.call(p.addAssigneeMethod(), "/tasks/"+taskID+"/assignees", body); err != nil {
					notes = append(notes, "指派 "+u+" 失败: "+err.Error())
				}
			}
		}
	}
	if len(notes) > 0 {
		return map[string]interface{}{"task": res, "warnings": notes}, nil
	}
	return res, nil
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, x := range parts {
		if t := strings.TrimSpace(x); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// labelBody v1 收 Label 对象，v2 收 {"label_id":N}
func (p *Plugin) labelBody(label string) map[string]interface{} {
	if p.isV1() {
		if id, err := strconv.Atoi(label); err == nil {
			return map[string]interface{}{"id": id, "title": label}
		}
		return map[string]interface{}{"title": label}
	}
	if id, err := strconv.Atoi(label); err == nil {
		return map[string]interface{}{"label_id": id}
	}
	return map[string]interface{}{"label_id": label}
}

func (p *Plugin) handleTaskUpdate(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	id := argID(args, "id")
	if id == "" {
		return nil, errors.New("缺少参数 id（任务 ID）")
	}
	body := taskBody(args, false)
	if raw, ok := argMap(args, "raw_patch"); ok {
		for k, v := range raw {
			body[k] = v
		}
	}
	if len(body) == 0 {
		return nil, errors.New("没有给出要修改的字段")
	}
	if !p.isV1() {
		// v2：PATCH 支持 merge-patch，只发变更字段
		res, err := p.callVer("v2", http.MethodPatch, "/tasks/"+id, body, "application/merge-patch+json")
		if err == nil {
			return res, nil
		}
		var ae *apiError
		if !errors.As(err, &ae) || (ae.Status != 400 && ae.Status != 405 && ae.Status != 415) {
			return nil, err
		}
		// merge-patch 被拒时退回「取回-合并-整体 PUT」
		return p.updateTaskByMerge(id, body)
	}
	// v1：POST /tasks/{id} 要求完整对象，先取回再合并
	return p.updateTaskByMerge(id, body)
}

// updateTaskByMerge 取回任务 → 合并变更 → 整体提交（v1 用 POST，v2 用 PUT）
func (p *Plugin) updateTaskByMerge(id string, changes map[string]interface{}) (interface{}, error) {
	cur, err := p.call(http.MethodGet, "/tasks/"+id, nil)
	if err != nil {
		return nil, err
	}
	obj, ok := cur.(map[string]interface{})
	if !ok {
		return nil, errors.New("取回任务失败：返回体不是对象")
	}
	for k, v := range changes {
		obj[k] = v
	}
	if _, ok := obj["id"]; !ok {
		obj["id"] = id
	}
	method := http.MethodPut
	if p.isV1() {
		method = http.MethodPost
	}
	return p.call(method, "/tasks/"+id, obj)
}

func (p *Plugin) handleTaskDone(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	id := argID(args, "id")
	if id == "" {
		return nil, errors.New("缺少参数 id（任务 ID）")
	}
	done := true
	if v, ok := argBool(args, "done"); ok {
		done = v
	}
	return p.updateTaskByMerge(id, map[string]interface{}{"done": done})
}

func (p *Plugin) handleTaskDelete(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	id := argID(args, "id")
	if id == "" {
		return nil, errors.New("缺少参数 id（任务 ID）")
	}
	if _, err := p.call(http.MethodDelete, "/tasks/"+id, nil); err != nil {
		return nil, err
	}
	return map[string]interface{}{"ok": true, "deleted_task_id": id}, nil
}

func (p *Plugin) handleTaskBulk(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	idsRaw := argStr(args, "task_ids")
	if idsRaw == "" {
		return nil, errors.New("缺少参数 task_ids（逗号分隔的任务 ID）")
	}
	taskIDs := make([]interface{}, 0)
	for _, s := range splitCSV(idsRaw) {
		if n, err := strconv.Atoi(s); err == nil {
			taskIDs = append(taskIDs, n)
		} else {
			taskIDs = append(taskIDs, s)
		}
	}
	body := map[string]interface{}{"task_ids": taskIDs}
	fields := map[string]interface{}{}
	if v, ok := argBool(args, "done"); ok {
		fields["done"] = v
	}
	if v := argID(args, "project_id"); v != "" {
		fields["project_id"] = v
	}
	if _, present := args["priority"]; present {
		fields["priority"] = argInt(args, "priority", 0)
	}
	if v := argStr(args, "due_date"); v != "" {
		if strings.EqualFold(v, "clear") {
			fields["due_date"] = nil
		} else {
			fields["due_date"] = normalizeTime(v)
		}
	}
	if _, present := args["label_ids"]; present {
		v := argStr(args, "label_ids")
		fields["labels"] = collectLabels(v)
	}
	if len(fields) > 0 {
		body["fields"] = fields
	}
	return p.call(p.bulkMethod(), "/tasks/bulk", body)
}

func collectLabels(v string) []interface{} {
	out := []interface{}{}
	for _, s := range splitCSV(v) {
		if n, err := strconv.Atoi(s); err == nil {
			out = append(out, map[string]interface{}{"id": n})
		} else {
			out = append(out, map[string]interface{}{"title": s})
		}
	}
	return out
}

func (p *Plugin) handleTaskAssignees(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	id := argID(args, "id")
	if id == "" {
		return nil, errors.New("缺少参数 id（任务 ID）")
	}
	action := strings.ToLower(argStr(args, "action"))
	if action == "" {
		action = "list"
	}
	switch action {
	case "list":
		return p.call(http.MethodGet, "/tasks/"+id+"/assignees", nil)
	case "add":
		user := argStr(args, "user")
		if user == "" {
			return nil, errors.New("add 需要参数 user（用户名或用户 ID）")
		}
		body, err := p.assigneeBody(user)
		if err != nil {
			return nil, err
		}
		return p.call(p.addAssigneeMethod(), "/tasks/"+id+"/assignees", body)
	case "remove":
		user := argStr(args, "user")
		if user == "" {
			return nil, errors.New("remove 需要参数 user（用户名或用户 ID）")
		}
		uid, err := p.resolveUserID(user)
		if err != nil {
			return nil, err
		}
		return p.call(http.MethodDelete, "/tasks/"+id+"/assignees/"+strconv.Itoa(uid), nil)
	}
	return nil, fmt.Errorf("未知 action: %s（可用 list|add|remove）", action)
}

func (p *Plugin) handleTaskLabels(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	id := argID(args, "id")
	if id == "" {
		return nil, errors.New("缺少参数 id（任务 ID）")
	}
	action := strings.ToLower(argStr(args, "action"))
	if action == "" {
		action = "list"
	}
	switch action {
	case "list":
		return p.call(http.MethodGet, "/tasks/"+id+"/labels", nil)
	case "add":
		label := argStr(args, "label_id")
		if label == "" {
			return nil, errors.New("add 需要参数 label_id")
		}
		return p.call(p.addLabelMethod(), "/tasks/"+id+"/labels", p.labelBody(label))
	case "remove":
		label := argStr(args, "label_id")
		if label == "" {
			return nil, errors.New("remove 需要参数 label_id")
		}
		return p.call(http.MethodDelete, "/tasks/"+id+"/labels/"+label, nil)
	}
	return nil, fmt.Errorf("未知 action: %s（可用 list|add|remove）", action)
}

func (p *Plugin) handleTaskComments(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	id := argID(args, "id")
	if id == "" {
		return nil, errors.New("缺少参数 id（任务 ID）")
	}
	action := strings.ToLower(argStr(args, "action"))
	if action == "" {
		action = "list"
	}
	switch action {
	case "list":
		return p.call(http.MethodGet, "/tasks/"+id+"/comments", nil)
	case "add":
		text := argStr(args, "comment")
		if text == "" {
			return nil, errors.New("add 需要参数 comment")
		}
		return p.call(p.addCommentMethod(), "/tasks/"+id+"/comments", map[string]interface{}{"comment": text})
	case "update":
		cid := argStr(args, "comment_id")
		text := argStr(args, "comment")
		if cid == "" || text == "" {
			return nil, errors.New("update 需要参数 comment_id 与 comment")
		}
		method := http.MethodPut
		if p.isV1() {
			method = http.MethodPost
		}
		return p.call(method, "/tasks/"+id+"/comments/"+cid, map[string]interface{}{"comment": text})
	case "delete":
		cid := argStr(args, "comment_id")
		if cid == "" {
			return nil, errors.New("delete 需要参数 comment_id")
		}
		return p.call(http.MethodDelete, "/tasks/"+id+"/comments/"+cid, nil)
	}
	return nil, fmt.Errorf("未知 action: %s（可用 list|add|update|delete）", action)
}

func (p *Plugin) handleTaskRelations(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	id := argID(args, "id")
	if id == "" {
		return nil, errors.New("缺少参数 id（任务 ID）")
	}
	action := strings.ToLower(argStr(args, "action"))
	if action == "" {
		action = "list"
	}
	kind := argStr(args, "kind")
	if kind == "" {
		kind = "subtask"
	}
	switch action {
	case "list":
		return p.call(http.MethodGet, "/tasks/"+id+"/relations", nil)
	case "add":
		other := argStr(args, "other_task_id")
		if other == "" {
			return nil, errors.New("add 需要参数 other_task_id")
		}
		body := map[string]interface{}{
			"task_id":       parseID(other),
			"relation_kind": kind,
		}
		return p.call(http.MethodPost, "/tasks/"+id+"/relations", body)
	case "remove":
		other := argStr(args, "other_task_id")
		if other == "" {
			return nil, errors.New("remove 需要参数 other_task_id")
		}
		return p.call(http.MethodDelete, "/tasks/"+id+"/relations/"+kind+"/"+other, nil)
	}
	return nil, fmt.Errorf("未知 action: %s（可用 list|add|remove）", action)
}

func parseID(s string) interface{} {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return s
}

// numOf 把 JSON 解出的值取成整数（SDK 放进 map 的数字是 float64）
func numOf(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return i
		}
	}
	return 0
}

// resolveUserID 把「用户名或 ID」解析成**数字** user_id。
//
// 为什么必须解析：v2 的 assignees 只接受 {"user_id":N}
//   - 发 {"username":"x"} → 422 unexpected property
//   - 发 {"user_id":"1"}  → 422 expected integer
//
// 两种都已在线上实测确认（2026-09-12）。
func (p *Plugin) resolveUserID(user string) (int, error) {
	u := strings.TrimSpace(user)
	if u == "" {
		return 0, errors.New("需要参数 user（用户名或用户 ID）")
	}
	if n, err := strconv.Atoi(u); err == nil {
		return n, nil
	}
	res, err := p.call(http.MethodGet, qv("/users", p.searchParam(), u), nil)
	if err != nil {
		return 0, fmt.Errorf("按用户名 %q 查用户失败: %w", u, err)
	}
	items := asSlice(res)
	var names []string
	for _, it := range items {
		m, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		name := fmt.Sprint(m["username"])
		names = append(names, name)
		if strings.EqualFold(name, u) {
			if id := numOf(m["id"]); id > 0 {
				return id, nil
			}
		}
	}
	// 只认精确匹配（不区分大小写）。不做「只有一条就用它」的模糊兜底：
	// 指派会写到别人的任务上，猜错人比让模型改用数字 ID 更贵。
	if len(names) > 0 {
		return 0, fmt.Errorf("找不到用户 %q（同名/相近的：%s）；也可直接传数字用户 ID", u, strings.Join(names, ", "))
	}
	return 0, fmt.Errorf("找不到用户 %q", u)
}

// assigneeBody 拼出指派请求体：只含数字 user_id（见 resolveUserID 的说明）
func (p *Plugin) assigneeBody(user string) (map[string]interface{}, error) {
	uid, err := p.resolveUserID(user)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"user_id": uid}, nil
}

func (p *Plugin) handleTaskAttachments(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	id := argID(args, "id")
	if id == "" {
		return nil, errors.New("缺少参数 id（任务 ID）")
	}
	action := strings.ToLower(argStr(args, "action"))
	if action == "" {
		action = "list"
	}
	switch action {
	case "list":
		return p.call(http.MethodGet, "/tasks/"+id+"/attachments", nil)
	case "upload":
		path := argStr(args, "path")
		if path == "" {
			return nil, errors.New("upload 需要参数 path（本地文件绝对路径）")
		}
		return p.uploadAttachment(id, path)
	case "delete":
		aid := argStr(args, "attachment_id")
		if aid == "" {
			return nil, errors.New("delete 需要参数 attachment_id")
		}
		return p.call(http.MethodDelete, "/tasks/"+id+"/attachments/"+aid, nil)
	}
	return nil, fmt.Errorf("未知 action: %s（可用 list|upload|delete）", action)
}

func (p *Plugin) uploadAttachment(taskID, path string) (interface{}, error) {
	if p.token == "" {
		return nil, errors.New("未配置 API Token")
	}
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("打开文件失败: %w", err)
	}
	defer f.Close()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("files", filepath.Base(path))
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(part, f); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequest(p.uploadAttachmentMethod(), p.baseURL+"/api/"+p.apiVer+"/tasks/"+taskID+"/attachments", &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("上传失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return nil, &apiError{Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
	}
	var out interface{}
	if json.Unmarshal(raw, &out) == nil {
		return out, nil
	}
	return map[string]interface{}{"ok": true, "raw": string(raw)}, nil
}

// ---- 项目 ----

func (p *Plugin) handleProjects(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	action := strings.ToLower(argStr(args, "action"))
	if action == "" {
		action = "list"
	}
	id := argID(args, "id")
	switch action {
	case "list":
		res, err := p.call(http.MethodGet, "/projects", nil)
		if err != nil {
			return nil, err
		}
		if !p.compact {
			return res, nil
		}
		return map[string]interface{}{"count": len(asSlice(res)), "projects": briefList(res, briefProject)}, nil
	case "get":
		if id == "" {
			return nil, errors.New("get 需要参数 id")
		}
		return p.call(http.MethodGet, "/projects/"+id, nil)
	case "create":
		body, ok := argMap(args, "raw")
		if !ok {
			body = map[string]interface{}{}
			if v := argStr(args, "title"); v != "" {
				body["title"] = v
			}
			if v := argStr(args, "description"); v != "" {
				body["description"] = v
			}
			if v := argStr(args, "hex_color"); v != "" {
				body["hex_color"] = v
			}
			if v := argStr(args, "parent_project_id"); v != "" {
				body["parent_project_id"] = parseID(v)
			}
		}
		if _, ok := body["title"]; !ok {
			return nil, errors.New("create 需要参数 title")
		}
		return p.call(p.createProjectMethod(), "/projects", body)
	case "update":
		if id == "" {
			return nil, errors.New("update 需要参数 id")
		}
		body, ok := argMap(args, "raw")
		if !ok {
			body = map[string]interface{}{}
			if v := argStr(args, "title"); v != "" {
				body["title"] = v
			}
			if v := argStr(args, "description"); v != "" {
				body["description"] = v
			}
			if v := argStr(args, "hex_color"); v != "" {
				body["hex_color"] = v
			}
			if b, ok := argBool(args, "is_favorite"); ok {
				body["is_favorite"] = b
			}
		}
		if len(body) == 0 {
			return nil, errors.New("update 没有给出要修改的字段（title/description/hex_color/is_favorite 或 raw）")
		}
		return p.projectUpdate(id, body)
	case "delete":
		if id == "" {
			return nil, errors.New("delete 需要参数 id")
		}
		if _, err := p.call(http.MethodDelete, "/projects/"+id, nil); err != nil {
			return nil, err
		}
		return map[string]interface{}{"ok": true, "deleted_project_id": id}, nil
	case "archive":
		if id == "" {
			return nil, errors.New("archive 需要参数 id")
		}
		archived := true
		if b, ok := argBool(args, "archived"); ok {
			archived = b
		}
		return p.projectUpdate(id, map[string]interface{}{"is_archived": archived})
	}
	return nil, fmt.Errorf("未知 action: %s（可用 list|get|create|update|delete|archive）", action)
}

// projectUpdate v2（2.4+）项目更新以 PUT 为主、PATCH 亦可；v1 用 POST
func (p *Plugin) projectUpdate(id string, changes map[string]interface{}) (interface{}, error) {
	if p.isV1() {
		cur, err := p.call(http.MethodGet, "/projects/"+id, nil)
		if err != nil {
			return nil, err
		}
		obj, ok := cur.(map[string]interface{})
		if !ok {
			return nil, errors.New("取回项目失败：返回体不是对象")
		}
		for k, v := range changes {
			obj[k] = v
		}
		return p.call(http.MethodPost, "/projects/"+id, obj)
	}
	res, err := p.callVer("v2", http.MethodPatch, "/projects/"+id, changes, "application/merge-patch+json")
	if err == nil {
		return res, nil
	}
	var ae *apiError
	if !errors.As(err, &ae) || (ae.Status != 400 && ae.Status != 405 && ae.Status != 415) {
		return nil, err
	}
	cur, err2 := p.call(http.MethodGet, "/projects/"+id, nil)
	if err2 != nil {
		return nil, err
	}
	obj, ok := cur.(map[string]interface{})
	if !ok {
		return nil, errors.New("取回项目失败：返回体不是对象")
	}
	for k, v := range changes {
		obj[k] = v
	}
	return p.call(http.MethodPut, "/projects/"+id, obj)
}

func (p *Plugin) handleProjectViews(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	action := strings.ToLower(argStr(args, "action"))
	if action == "" {
		action = "list_views"
	}
	pid := argID(args, "project_id")
	if pid == "" {
		return nil, errors.New("缺少参数 project_id")
	}
	vid := argStr(args, "view_id")
	bid := argStr(args, "bucket_id")
	switch action {
	case "list_views":
		return p.call(http.MethodGet, "/projects/"+pid+"/views", nil)
	case "list_buckets":
		if vid == "" {
			return nil, errors.New("list_buckets 需要参数 view_id")
		}
		return p.call(http.MethodGet, "/projects/"+pid+"/views/"+vid+"/buckets", nil)
	case "bucket_tasks":
		if vid == "" || bid == "" {
			return nil, errors.New("bucket_tasks 需要参数 view_id 与 bucket_id")
		}
		if p.isV1() {
			// v1 用 /projects/{id}/views/{view}/tasks 拿全部再看桶
			return p.call(http.MethodGet, "/projects/"+pid+"/views/"+vid+"/tasks", nil)
		}
		return p.call(http.MethodGet, "/projects/"+pid+"/views/"+vid+"/buckets/"+bid+"/tasks", nil)
	case "move_to_bucket":
		tid := argStr(args, "task_id")
		if vid == "" || bid == "" || tid == "" {
			return nil, errors.New("move_to_bucket 需要参数 view_id、bucket_id、task_id")
		}
		method := http.MethodPut
		if p.isV1() {
			method = http.MethodPost
		}
		path := "/projects/" + pid + "/views/" + vid + "/buckets/" + bid + "/tasks"
		return p.call(method, path, map[string]interface{}{"task_id": parseID(tid)})
	}
	return nil, fmt.Errorf("未知 action: %s（可用 list_views|list_buckets|bucket_tasks|move_to_bucket）", action)
}

// ---- 标签 / 筛选器 ----

func (p *Plugin) handleLabels(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	action := strings.ToLower(argStr(args, "action"))
	if action == "" {
		action = "list"
	}
	id := argID(args, "id")
	switch action {
	case "list":
		res, err := p.call(http.MethodGet, "/labels", nil)
		if err != nil {
			return nil, err
		}
		if !p.compact {
			return res, nil
		}
		return map[string]interface{}{"count": len(asSlice(res)), "labels": briefList(res, briefLabel)}, nil
	case "create":
		body := map[string]interface{}{}
		if v := argStr(args, "title"); v != "" {
			body["title"] = v
		}
		if v := argStr(args, "hex_color"); v != "" {
			body["hex_color"] = v
		}
		if _, ok := body["title"]; !ok {
			return nil, errors.New("create 需要参数 title")
		}
		return p.call(p.createLabelMethod(), "/labels", body)
	case "update":
		if id == "" {
			return nil, errors.New("update 需要参数 id")
		}
		body := map[string]interface{}{}
		if v := argStr(args, "title"); v != "" {
			body["title"] = v
		}
		if v := argStr(args, "hex_color"); v != "" {
			body["hex_color"] = v
		}
		if len(body) == 0 {
			return nil, errors.New("update 没有给出要修改的字段")
		}
		return p.call(p.updateLabelMethod(), "/labels/"+id, body)
	case "delete":
		if id == "" {
			return nil, errors.New("delete 需要参数 id")
		}
		if _, err := p.call(http.MethodDelete, "/labels/"+id, nil); err != nil {
			return nil, err
		}
		return map[string]interface{}{"ok": true, "deleted_label_id": id}, nil
	}
	return nil, fmt.Errorf("未知 action: %s（可用 list|create|update|delete）", action)
}

func (p *Plugin) handleFilters(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	action := strings.ToLower(argStr(args, "action"))
	if action == "" {
		action = "list"
	}
	id := argID(args, "id")
	switch action {
	case "list":
		return p.call(http.MethodGet, "/filters", nil)
	case "get":
		if id == "" {
			return nil, errors.New("get 需要参数 id")
		}
		return p.call(http.MethodGet, "/filters/"+id, nil)
	case "create":
		body, ok := argMap(args, "raw")
		if !ok {
			body = map[string]interface{}{}
			if v := argStr(args, "title"); v != "" {
				body["title"] = v
			}
			if v := argStr(args, "filter"); v != "" {
				body["filters"] = map[string]interface{}{"filter": v}
			}
		}
		if _, ok := body["title"]; !ok {
			return nil, errors.New("create 需要参数 title（filter 建议一并给出）")
		}
		method := p.createLabelMethod() // v1 PUT / v2 POST，与 labels 同源规则
		return p.call(method, "/filters", body)
	case "update":
		if id == "" {
			return nil, errors.New("update 需要参数 id")
		}
		body, ok := argMap(args, "raw")
		if !ok {
			body = map[string]interface{}{}
			if v := argStr(args, "title"); v != "" {
				body["title"] = v
			}
			if v := argStr(args, "filter"); v != "" {
				body["filters"] = map[string]interface{}{"filter": v}
			}
		}
		method := http.MethodPatch
		if p.isV1() {
			method = http.MethodPost
		}
		return p.call(method, "/filters/"+id, body)
	case "delete":
		if id == "" {
			return nil, errors.New("delete 需要参数 id")
		}
		if _, err := p.call(http.MethodDelete, "/filters/"+id, nil); err != nil {
			return nil, err
		}
		return map[string]interface{}{"ok": true, "deleted_filter_id": id}, nil
	}
	return nil, fmt.Errorf("未知 action: %s（可用 list|get|create|update|delete）", action)
}

// ---- 团队 / 分享 ----

func (p *Plugin) handleTeams(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	action := strings.ToLower(argStr(args, "action"))
	if action == "" {
		action = "list"
	}
	id := argID(args, "id")
	switch action {
	case "list":
		return p.call(http.MethodGet, "/teams", nil)
	case "get":
		return p.call(http.MethodGet, "/teams/"+id, nil)
	case "create":
		name := argStr(args, "name")
		if name == "" {
			return nil, errors.New("create 需要参数 name")
		}
		return p.call(p.createTeamMethod(), "/teams", map[string]interface{}{"name": name})
	case "update":
		name := argStr(args, "name")
		if id == "" || name == "" {
			return nil, errors.New("update 需要参数 id 与 name")
		}
		method := http.MethodPatch
		if p.isV1() {
			method = http.MethodPost
		}
		return p.call(method, "/teams/"+id, map[string]interface{}{"name": name})
	case "delete":
		if _, err := p.call(http.MethodDelete, "/teams/"+id, nil); err != nil {
			return nil, err
		}
		return map[string]interface{}{"ok": true, "deleted_team_id": id}, nil
	case "members":
		return p.call(http.MethodGet, "/teams/"+id+"/members", nil)
	case "add_member":
		u := argStr(args, "username")
		if id == "" || u == "" {
			return nil, errors.New("add_member 需要参数 id 与 username")
		}
		body := map[string]interface{}{"username": u}
		if n, err := strconv.Atoi(u); err == nil {
			body["user_id"] = n
		}
		return p.call(p.addTeamMemberMethod(), "/teams/"+id+"/members", body)
	case "remove_member":
		u := argStr(args, "username")
		if id == "" || u == "" {
			return nil, errors.New("remove_member 需要参数 id 与 username（用户 ID）")
		}
		return p.call(http.MethodDelete, "/teams/"+id+"/members/"+u, nil)
	}
	return nil, fmt.Errorf("未知 action: %s（可用 list|get|create|update|delete|members|add_member|remove_member）", action)
}

func (p *Plugin) handleSharing(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	pid := argID(args, "project_id")
	if pid == "" {
		return nil, errors.New("缺少参数 project_id")
	}
	action := strings.ToLower(argStr(args, "action"))
	if action == "" {
		action = "list"
	}
	sid := argStr(args, "share_id")
	switch action {
	case "list":
		return p.call(http.MethodGet, "/projects/"+pid+"/shares", nil)
	case "add_user":
		u := argStr(args, "username")
		if u == "" {
			return nil, errors.New("add_user 需要参数 username")
		}
		body := map[string]interface{}{"username": u, "right": argInt(args, "right", 0)}
		if n, err := strconv.Atoi(u); err == nil {
			body["user_id"] = n
		}
		return p.call(http.MethodPut, "/projects/"+pid+"/users", body)
	case "update_user":
		if sid == "" {
			return nil, errors.New("update_user 需要参数 share_id")
		}
		return p.call(http.MethodPut, "/projects/"+pid+"/users/"+sid, map[string]interface{}{"right": argInt(args, "right", 0)})
	case "remove_user":
		if sid == "" {
			return nil, errors.New("remove_user 需要参数 share_id")
		}
		return p.call(http.MethodDelete, "/projects/"+pid+"/users/"+sid, nil)
	case "add_team":
		tid := argStr(args, "team_id")
		if tid == "" {
			return nil, errors.New("add_team 需要参数 team_id")
		}
		return p.call(http.MethodPut, "/projects/"+pid+"/teams", map[string]interface{}{"team_id": parseID(tid), "right": argInt(args, "right", 0)})
	case "remove_team":
		if sid == "" {
			return nil, errors.New("remove_team 需要参数 share_id")
		}
		return p.call(http.MethodDelete, "/projects/"+pid+"/teams/"+sid, nil)
	case "links":
		return p.call(http.MethodGet, "/projects/"+pid+"/shares", nil)
	case "create_link":
		body, ok := argMap(args, "raw")
		if !ok {
			body = map[string]interface{}{"right": argInt(args, "right", 0)}
			if v := argStr(args, "password"); v != "" {
				body["password"] = v
			}
		}
		return p.call(http.MethodPut, "/projects/"+pid+"/shares", body)
	case "update_link":
		if sid == "" {
			return nil, errors.New("update_link 需要参数 share_id")
		}
		body, ok := argMap(args, "raw")
		if !ok {
			body = map[string]interface{}{"right": argInt(args, "right", 0)}
			if v := argStr(args, "password"); v != "" {
				body["password"] = v
			}
		}
		return p.call(http.MethodPost, "/projects/"+pid+"/shares/"+sid, body)
	case "delete_link":
		if sid == "" {
			return nil, errors.New("delete_link 需要参数 share_id")
		}
		return p.call(http.MethodDelete, "/projects/"+pid+"/shares/"+sid, nil)
	}
	return nil, fmt.Errorf("未知 action: %s（可用 list|add_user|update_user|remove_user|add_team|remove_team|links|create_link|update_link|delete_link）", action)
}

// ---- 通知 / 订阅 / Webhook ----

func (p *Plugin) handleNotifications(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	action := strings.ToLower(argStr(args, "action"))
	if action == "" {
		action = "list"
	}
	switch action {
	case "list":
		return p.call(http.MethodGet, qv("/notifications", "page", "1", "per_page", strconv.Itoa(p.maxItems)), nil)
	case "mark_read":
		id := argStr(args, "id")
		if id == "" {
			return nil, errors.New("mark_read 需要参数 id")
		}
		return p.call(p.notificationReadMethod(), "/notifications/"+id, nil)
	case "mark_all_read":
		return p.call(http.MethodPost, "/notifications", map[string]interface{}{"read_all": true})
	}
	return nil, fmt.Errorf("未知 action: %s（可用 list|mark_read|mark_all_read）", action)
}

func (p *Plugin) handleSubscriptions(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	entity := argStr(args, "entity")
	eid := argStr(args, "entity_id")
	if entity == "" || eid == "" {
		return nil, errors.New("需要参数 entity（如 task）与 entity_id")
	}
	if strings.EqualFold(argStr(args, "action"), "unsubscribe") {
		return p.call(http.MethodDelete, "/subscriptions/"+entity+"/"+eid, nil)
	}
	return p.call(p.subscribeMethod(), "/subscriptions/"+entity+"/"+eid, nil)
}

func (p *Plugin) handleWebhooks(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	pid := argID(args, "project_id")
	if pid == "" {
		return nil, errors.New("缺少参数 project_id")
	}
	action := strings.ToLower(argStr(args, "action"))
	if action == "" {
		action = "list"
	}
	id := argStr(args, "id")
	switch action {
	case "list":
		return p.call(http.MethodGet, "/projects/"+pid+"/webhooks", nil)
	case "events":
		return p.call(http.MethodGet, "/projects/"+pid+"/webhooks", nil)
	case "create":
		body, ok := argMap(args, "raw")
		if !ok {
			body = map[string]interface{}{}
			if v := argStr(args, "target_url"); v != "" {
				body["target_url"] = v
			}
			if v := argStr(args, "events"); v != "" {
				evs := make([]interface{}, 0)
				for _, e := range splitCSV(v) {
					evs = append(evs, e)
				}
				body["events"] = evs
			}
			if v := argStr(args, "secret"); v != "" {
				body["secret"] = v
			}
		}
		if _, ok := body["target_url"]; !ok {
			return nil, errors.New("create 需要参数 target_url（events 建议一并给出，如 task.created）")
		}
		return p.call(p.createWebhookMethod(), "/projects/"+pid+"/webhooks", body)
	case "update":
		body, ok := argMap(args, "raw")
		if !ok {
			body = map[string]interface{}{}
			if v := argStr(args, "target_url"); v != "" {
				body["target_url"] = v
			}
			if v := argStr(args, "events"); v != "" {
				evs := make([]interface{}, 0)
				for _, e := range splitCSV(v) {
					evs = append(evs, e)
				}
				body["events"] = evs
			}
		}
		if id == "" {
			return nil, errors.New("update 需要参数 id")
		}
		return p.call(p.updateWebhookMethod(), "/projects/"+pid+"/webhooks/"+id, body)
	case "delete":
		if id == "" {
			return nil, errors.New("delete 需要参数 id")
		}
		return p.call(http.MethodDelete, "/projects/"+pid+"/webhooks/"+id, nil)
	}
	return nil, fmt.Errorf("未知 action: %s（可用 list|events|create|update|delete）", action)
}

// ---- 时间跟踪（v2 独有）----

func (p *Plugin) handleTimeEntries(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	if p.isV1() {
		return nil, errors.New("时间跟踪只有 v2 提供：请把插件配置 vikunja.api_version 改为 v2")
	}
	action := strings.ToLower(argStr(args, "action"))
	if action == "" {
		action = "list"
	}
	switch action {
	case "list":
		return p.call(http.MethodGet, qv("/time-entries", "page", "1", "per_page", strconv.Itoa(p.maxItems)), nil)
	case "task_entries":
		id := argStr(args, "id")
		if id == "" {
			return nil, errors.New("task_entries 需要参数 id（任务 ID）")
		}
		return p.call(http.MethodGet, "/tasks/"+id+"/time-entries", nil)
	case "create":
		body, err := p.timeEntryBody(args, false)
		if err != nil {
			return nil, err
		}
		return p.call(http.MethodPost, "/time-entries", body)
	case "update":
		id := argStr(args, "id")
		if id == "" {
			return nil, errors.New("update 需要参数 id")
		}
		body := map[string]interface{}{}
		if v := argStr(args, "comment"); v != "" {
			body["comment"] = v
		}
		if v := argStr(args, "start_time"); v != "" {
			body["start_time"] = normalizeTime(v)
		}
		if v := argStr(args, "end_time"); v != "" {
			if strings.EqualFold(v, "clear") {
				body["end_time"] = nil
			} else {
				body["end_time"] = normalizeTime(v)
			}
		}
		if len(body) == 0 {
			return nil, errors.New("update 没有给出要修改的字段（comment/start_time/end_time）")
		}
		return p.call(http.MethodPatch, "/time-entries/"+id, body)
	case "delete":
		id := argStr(args, "id")
		if id == "" {
			return nil, errors.New("delete 需要参数 id")
		}
		return p.call(http.MethodDelete, "/time-entries/"+id, nil)
	case "timer_start":
		// v2 没有独立的 start 端点：end_time 为空(null) 的 time entry 就是「计时中」
		body, err := p.timeEntryBody(args, true)
		if err != nil {
			return nil, err
		}
		return p.call(http.MethodPost, "/time-entries", body)
	case "timer_stop":
		return p.call(http.MethodPost, "/time-entries/timer/stop", nil)
	}
	return nil, fmt.Errorf("未知 action: %s（可用 list|task_entries|create|update|delete|timer_start|timer_stop）", action)
}

// timeEntryBody 拼 TimeEntry 请求体。
// 注意：v2 的 TimeEntry 没有 seconds 字段——带 end_time 即为已结束的记录，
// end_time 为 null 表示 live timer（计时中）。因此 seconds 参数在这里换算成 end_time。
func (p *Plugin) timeEntryBody(args map[string]interface{}, timerOnly bool) (map[string]interface{}, error) {
	body := map[string]interface{}{}
	if v := argStr(args, "task_id"); v != "" {
		body["task_id"] = parseID(v)
	}
	if v := argID(args, "project_id"); v != "" {
		body["project_id"] = parseID(v)
	}
	if _, ok := body["task_id"]; !ok {
		if _, ok2 := body["project_id"]; !ok2 {
			return nil, errors.New("需要 task_id 或 project_id（二者只能给一个）")
		}
	}
	if v := argStr(args, "comment"); v != "" {
		body["comment"] = v
	}
	startStr := argStr(args, "start_time")
	if startStr == "" {
		startStr = time.Now().Format(time.RFC3339)
	}
	startNorm := normalizeTime(startStr)
	body["start_time"] = startNorm
	if timerOnly {
		body["end_time"] = nil // 显式 null = live timer（比省略更少歧义）
		return body, nil
	}
	if secs := argInt(args, "seconds", 0); secs > 0 {
		if t, err := time.Parse(time.RFC3339, fmt.Sprint(startNorm)); err == nil {
			body["end_time"] = t.Add(time.Duration(secs) * time.Second).Format(time.RFC3339)
			return body, nil
		}
		return nil, errors.New("start_time 无法解析为 RFC3339，无法据 seconds 推算 end_time；请直接传 end_time")
	}
	if v := argStr(args, "end_time"); v != "" {
		if strings.EqualFold(v, "clear") {
			body["end_time"] = nil
		} else {
			body["end_time"] = normalizeTime(v)
		}
	} else {
		body["end_time"] = nil
	}
	return body, nil
}

// ---- 导入 ----

// migrateSpec 各来源的版本与方法（依据规范逐条核对）
type migrateSpec struct {
	ver    string
	method string
	oauth  bool
}

var migrateSources = map[string]migrateSpec{
	"ticktick":       {ver: "v2", method: http.MethodPost},
	"wekan":          {ver: "v2", method: http.MethodPost},
	"csv":            {ver: "v2", method: http.MethodPost},
	"planka":         {ver: "v2", method: http.MethodPost},
	"vikunja-file":   {ver: "v2", method: http.MethodPost},
	"todoist":        {ver: "v1", method: http.MethodPost, oauth: true},
	"trello":         {ver: "v1", method: http.MethodPost, oauth: true},
	"microsoft-todo": {ver: "v1", method: http.MethodPost, oauth: true},
}

func (p *Plugin) handleMigrate(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	action := strings.ToLower(argStr(args, "action"))
	if action == "" {
		action = "sources"
	}
	if action == "sources" {
		out := make([]map[string]interface{}, 0, len(migrateSources))
		for name, sp := range migrateSources {
			out = append(out, map[string]interface{}{
				"source": name, "api_version": sp.ver, "needs_oauth": sp.oauth,
			})
		}
		return map[string]interface{}{
			"available": out,
			"note":      "Todoist/Trello/微软待办 只有 v1 提供；其余用 v2。本工具会自动切换版本，不受插件默认版本影响",
		}, nil
	}
	src := strings.ToLower(argStr(args, "source"))
	sp, ok := migrateSources[src]
	if !ok {
		return nil, fmt.Errorf("未知来源 %q（可用 sources 动作查看）", src)
	}
	switch action {
	case "auth_url":
		if !sp.oauth {
			return nil, fmt.Errorf("来源 %s 不需要 OAuth 授权", src)
		}
		return p.callVer(sp.ver, http.MethodGet, "/migration/"+src+"/auth", nil, "")
	case "start":
		body, hasBody := argMap(args, "payload")
		if !hasBody {
			body = map[string]interface{}{}
			if v := argStr(args, "code"); v != "" {
				body["code"] = v
			}
		}
		if src == "csv" {
			return p.callVer("v2", http.MethodPost, "/migration/csv/migrate", body, "")
		}
		return p.callVer(sp.ver, sp.method, "/migration/"+src+"/migrate", body, "")
	case "status":
		if src == "csv" {
			return p.callVer("v2", http.MethodGet, "/migration/csv/status", nil, "")
		}
		if src == "planka" {
			return p.callVer("v2", http.MethodGet, "/migration/planka/status", nil, "")
		}
		if src == "microsoft-todo" {
			return p.callVer("v1", http.MethodGet, "/migration/microsoft-todo/status", nil, "")
		}
		return p.callVer(sp.ver, http.MethodGet, "/migration/"+src+"/status", nil, "")
	}
	return nil, fmt.Errorf("未知 action: %s（可用 sources|auth_url|start|status）", action)
}

// ---- 账号 ----

func (p *Plugin) handleUser(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	action := strings.ToLower(argStr(args, "action"))
	if action == "" {
		action = "me"
	}
	switch action {
	case "me":
		return p.call(http.MethodGet, "/user", nil)
	case "settings":
		return p.call(http.MethodGet, "/user/settings/general", nil)
	case "update_settings":
		body, ok := argMap(args, "settings")
		if !ok {
			return nil, errors.New("update_settings 需要参数 settings（JSON 对象）")
		}
		method := http.MethodPut
		if p.isV1() {
			method = http.MethodPost
		}
		return p.call(method, "/user/settings/general", body)
	case "sessions":
		if p.isV1() {
			return nil, errors.New("登录会话管理只有 v2 提供")
		}
		return p.call(http.MethodGet, "/user/sessions", nil)
	case "revoke_session":
		if p.isV1() {
			return nil, errors.New("登录会话管理只有 v2 提供")
		}
		id := argStr(args, "session_id")
		if id == "" {
			return nil, errors.New("revoke_session 需要参数 session_id")
		}
		return p.call(http.MethodDelete, "/user/sessions/"+id, nil)
	case "tokens":
		return p.call(http.MethodGet, "/tokens", nil)
	case "create_token":
		body := map[string]interface{}{}
		if v := argStr(args, "title"); v != "" {
			body["title"] = v
		}
		if v := argStr(args, "permissions"); v != "" {
			perms := make([]interface{}, 0)
			for _, x := range splitCSV(v) {
				perms = append(perms, x)
			}
			body["permissions"] = perms
		}
		method := http.MethodPost
		if p.isV1() {
			method = http.MethodPut
		}
		return p.call(method, "/tokens", body)
	case "delete_token":
		id := argStr(args, "token_id")
		if id == "" {
			return nil, errors.New("delete_token 需要参数 token_id")
		}
		return p.call(http.MethodDelete, "/tokens/"+id, nil)
	case "change_password":
		oldPw, newPw := argStr(args, "old_password"), argStr(args, "new_password")
		if oldPw == "" || newPw == "" {
			return nil, errors.New("change_password 需要参数 old_password 与 new_password")
		}
		return p.call(http.MethodPost, "/user/password", map[string]interface{}{"old_password": oldPw, "new_password": newPw})
	case "caldav":
		if p.isV1() {
			return p.call(http.MethodGet, "/user/settings/token/caldav", nil)
		}
		return p.call(http.MethodGet, "/user/settings/token/caldav", nil)
	}
	return nil, fmt.Errorf("未知 action: %s（可用 me|settings|update_settings|sessions|revoke_session|tokens|create_token|delete_token|change_password|caldav）", action)
}

func (p *Plugin) handleAdmin(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	if p.isV1() {
		return nil, errors.New("实例管理端点在本插件中按 v2 实现，请把 api_version 设为 v2")
	}
	action := strings.ToLower(argStr(args, "action"))
	if action == "" {
		action = "overview"
	}
	uid := argStr(args, "user_id")
	switch action {
	case "overview":
		return p.call(http.MethodGet, "/admin/overview", nil)
	case "users":
		return p.call(http.MethodGet, "/admin/users", nil)
	case "create_user":
		body := map[string]interface{}{}
		if v := argStr(args, "username"); v != "" {
			body["username"] = v
		}
		if v := argStr(args, "email"); v != "" {
			body["email"] = v
		}
		if v := argStr(args, "password"); v != "" {
			body["password"] = v
		}
		if _, ok := body["username"]; !ok {
			return nil, errors.New("create_user 需要参数 username（email/password 建议一并给出）")
		}
		return p.call(http.MethodPost, "/admin/users", body)
	case "delete_user":
		if uid == "" {
			return nil, errors.New("delete_user 需要参数 user_id")
		}
		return p.call(http.MethodDelete, "/admin/users/"+uid, nil)
	case "set_admin":
		if uid == "" {
			return nil, errors.New("set_admin 需要参数 user_id 与 admin")
		}
		adm := true
		if b, ok := argBool(args, "admin"); ok {
			adm = b
		}
		return p.call(http.MethodPatch, "/admin/users/"+uid+"/admin", map[string]interface{}{"is_admin": adm})
	case "set_status":
		if uid == "" {
			return nil, errors.New("set_status 需要参数 user_id 与 status")
		}
		st := argInt(args, "status", 0)
		return p.call(http.MethodPatch, "/admin/users/"+uid+"/status", map[string]interface{}{"status": st})
	case "set_password":
		if uid == "" {
			return nil, errors.New("set_password 需要参数 user_id 与 password")
		}
		pw := argStr(args, "password")
		if pw == "" {
			return nil, errors.New("set_password 需要参数 password")
		}
		return p.call(http.MethodPatch, "/admin/users/"+uid+"/password", map[string]interface{}{"password": pw})
	case "projects":
		return p.call(http.MethodGet, "/admin/projects", nil)
	case "reassign_owner":
		pid := argID(args, "project_id")
		oid := argStr(args, "owner_id")
		if pid == "" || oid == "" {
			return nil, errors.New("reassign_owner 需要参数 project_id 与 owner_id")
		}
		return p.call(http.MethodPatch, "/admin/projects/"+pid+"/owner", map[string]interface{}{"owner_id": parseID(oid)})
	}
	return nil, fmt.Errorf("未知 action: %s（可用 overview|users|create_user|delete_user|set_admin|set_status|set_password|projects|reassign_owner）", action)
}

func (p *Plugin) handleReactions(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	kind := strings.ToLower(argStr(args, "kind"))
	id := argStr(args, "id")
	if kind == "" || id == "" {
		return nil, errors.New("需要参数 kind（tasks|comments）与 id")
	}
	action := strings.ToLower(argStr(args, "action"))
	if action == "" {
		action = "list"
	}
	path := "/" + kind + "/" + id + "/reactions"
	switch action {
	case "list":
		return p.call(http.MethodGet, path, nil)
	case "add":
		v := argStr(args, "value")
		if v == "" {
			return nil, errors.New("add 需要参数 value（emoji）")
		}
		return p.call(http.MethodPost, path, map[string]interface{}{"value": v})
	case "remove":
		v := argStr(args, "value")
		if v == "" {
			return nil, errors.New("remove 需要参数 value（emoji）")
		}
		return p.call(http.MethodDelete, qv(path, "value", v), nil)
	}
	return nil, fmt.Errorf("未知 action: %s（可用 list|add|remove）", action)
}

// ---- 通用直通 ----

func (p *Plugin) handleRawAPI(args map[string]interface{}) (interface{}, error) {
	p.ensure()
	path := argStr(args, "path")
	if path == "" {
		return nil, errors.New("缺少参数 path")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	method := strings.ToUpper(argStr(args, "method"))
	if method == "" {
		method = http.MethodGet
	}
	ver := argStr(args, "api_version")
	if ver != "v1" && ver != "v2" {
		ver = p.apiVer
	}
	var body interface{}
	if raw := argStr(args, "body"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			return nil, fmt.Errorf("body 不是合法 JSON: %w", err)
		}
	} else if m, ok := args["body"]; ok && m != nil {
		body = m
	}
	return p.callVer(ver, method, path, body, "")
}
