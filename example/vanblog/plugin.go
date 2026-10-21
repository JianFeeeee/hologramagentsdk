package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

type Plugin struct {
	name       string
	sdk        *sdk.PluginSDK
	http       *http.Client
	baseURL    string
	token      string
	resetToken string
}

func NewPlugin(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}

func (p *Plugin) Name() string { return p.name }

func getSetting(s sdk.SettingsAPI, key string, fallback string) string {
	v, err := s.Get(key)
	if err != nil || v == nil {
		return fallback
	}
	if sv, ok := v.(string); ok {
		return sv
	}
	return fallback
}

func readArg(args map[string]interface{}, key string) string {
	if v, ok := args[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func readArgInt(args map[string]interface{}, key string, fallback int) int {
	if v, ok := args[key]; ok && v != nil {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int64:
			return int(n)
		}
	}
	return fallback
}

func readArgBool(args map[string]interface{}, key string) *bool {
	if v, ok := args[key]; ok {
		if b, ok := v.(bool); ok {
			return &b
		}
	}
	return nil
}

func errResult(msg string) map[string]interface{} {
	return map[string]interface{}{"isError": true, "content": msg}
}

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	p.sdk = s
	s.SetAutoRestart(true)

	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "url", Default: "https://blog.jianfgit.xyz", Type: "string",
		DisplayName: "VanBlog URL", Description: "VanBlog 站点基地址",
		Category: "vanblog",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "token", Default: "", Type: "password",
		DisplayName: "API Token", Description: "管理员 API Token（长期令牌，从后台 Token 管理创建）",
		Category: "vanblog",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "reset_token", Default: "", Type: "password",
		DisplayName: "重置密码 Token", Description: "用于 auth/restore 重置管理员密码的特殊 Token",
		Category: "vanblog",
	})

	p.http = &http.Client{Timeout: 30 * time.Second}
	p.baseURL = strings.TrimRight(getSetting(s.Settings(), "url", "https://blog.jianfgit.xyz"), "/")
	p.token = getSetting(s.Settings(), "token", "")
	p.resetToken = getSetting(s.Settings(), "reset_token", "")

	if p.token == "" {
		if coreVal, err := s.Settings().GetCore("plugin.vanblog.token"); err == nil && coreVal != nil {
			if sv, ok := coreVal.(string); ok && sv != "" {
				p.token = sv
				s.Settings().Set("token", sv)
				s.Settings().SetCore("plugin.vanblog.token", nil)
				log.Printf("[vanblog] migrated token from core config to plugin config")
			}
		}
	}

	tp := p.name + "_"

	s.RegisterTool(tp+"list_articles", sdk.ToolDef{
		Name: tp + "list_articles", Description: "列出 VanBlog 文章，支持分页和搜索",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"page":     map[string]interface{}{"type": "integer", "description": "页码从1开始", "default": 1},
				"pageSize": map[string]interface{}{"type": "integer", "description": "每页条数", "default": 50},
				"keyword":  map[string]interface{}{"type": "string", "description": "搜索关键词"},
				"category": map[string]interface{}{"type": "string", "description": "按分类筛选"},
				"tag":      map[string]interface{}{"type": "string", "description": "按标签筛选"},
				"sort":     map[string]interface{}{"type": "string", "description": "排序: newest|oldest"},
			},
		},
	}, p.handleListArticles)

	s.RegisterTool(tp+"get_article", sdk.ToolDef{
		Name: tp + "get_article", Description: "获取单篇文章完整内容",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"id": map[string]interface{}{"type": "string", "description": "文章 ID 或 pathname"},
			}, "required": []string{"id"},
		},
	}, p.handleGetArticle)

	s.RegisterTool(tp+"create_article", sdk.ToolDef{
		Name: tp + "create_article", Description: "新建文章，title 和 category 必填",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"title":    map[string]interface{}{"type": "string", "description": "标题"},
				"content":  map[string]interface{}{"type": "string", "description": "Markdown 正文"},
				"category": map[string]interface{}{"type": "string", "description": "分类"},
				"tags":     map[string]interface{}{"type": "string", "description": "标签逗号分隔"},
				"pinned":   map[string]interface{}{"type": "boolean", "description": "置顶"},
			}, "required": []string{"title", "category"},
		},
	}, p.handleCreateArticle)

	s.RegisterTool(tp+"update_article", sdk.ToolDef{
		Name: tp + "update_article", Description: "更新文章，只传需改字段",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"id":       map[string]interface{}{"type": "string", "description": "文章 ID"},
				"title":    map[string]interface{}{"type": "string", "description": "新标题"},
				"content":  map[string]interface{}{"type": "string", "description": "新正文"},
				"category": map[string]interface{}{"type": "string", "description": "新分类"},
				"tags":     map[string]interface{}{"type": "string", "description": "新标签逗号分隔"},
				"pinned":   map[string]interface{}{"type": "boolean", "description": "置顶"},
			}, "required": []string{"id"},
		},
	}, p.handleUpdateArticle)

	s.RegisterTool(tp+"delete_article", sdk.ToolDef{
		Name: tp + "delete_article", Description: "删除文章（软删除）",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"id": map[string]interface{}{"type": "string", "description": "文章 ID"},
			}, "required": []string{"id"},
		},
	}, p.handleDeleteArticle)

	s.RegisterTool(tp+"search_articles", sdk.ToolDef{
		Name: tp + "search_articles", Description: "按链接搜索文章",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"link": map[string]interface{}{"type": "string", "description": "文章链接"},
			}, "required": []string{"link"},
		},
	}, p.handleSearchArticles)

	s.RegisterTool(tp+"manage_drafts", sdk.ToolDef{
		Name: tp + "manage_drafts",
		Description: "管理草稿。命令: list|get|create|update|delete|publish",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command":  map[string]interface{}{"type": "string", "description": "list|get|create|update|delete|publish"},
				"id":       map[string]interface{}{"type": "string", "description": "草稿ID（get/update/delete/publish需要）"},
				"title":    map[string]interface{}{"type": "string", "description": "标题（create/update）"},
				"content":  map[string]interface{}{"type": "string", "description": "正文（create/update）"},
				"category": map[string]interface{}{"type": "string", "description": "分类（create/update）"},
				"tags":     map[string]interface{}{"type": "string", "description": "标签逗号分隔（create/update）"},
				"page":     map[string]interface{}{"type": "integer", "description": "页码（list）"},
				"pageSize": map[string]interface{}{"type": "integer", "description": "每页条数（list）"},
			}, "required": []string{"command"},
		},
	}, p.handleManageDrafts)

	s.RegisterTool(tp+"manage_categories", sdk.ToolDef{
		Name: tp + "manage_categories",
		Description: "管理分类。命令: list|get|create|update|delete",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string", "description": "list|get|create|update|delete"},
				"name":    map[string]interface{}{"type": "string", "description": "分类名（get/create/update/delete需要）"},
				"newName": map[string]interface{}{"type": "string", "description": "新名称（update）"},
			}, "required": []string{"command"},
		},
	}, p.handleManageCategories)

	s.RegisterTool(tp+"manage_tags", sdk.ToolDef{
		Name: tp + "manage_tags",
		Description: "管理标签。命令: list|get|rename|delete",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string", "description": "list|get|rename|delete"},
				"name":    map[string]interface{}{"type": "string", "description": "标签名"},
				"newName": map[string]interface{}{"type": "string", "description": "新名称（rename）"},
			}, "required": []string{"command"},
		},
	}, p.handleManageTags)

	s.RegisterTool(tp+"manage_images", sdk.ToolDef{
		Name: tp + "manage_images",
		Description: "管理图床图片。命令: list|all|upload|scan|export|delete|delete_all",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string", "description": "list|all|upload|scan|export|delete|delete_all"},
				"page":    map[string]interface{}{"type": "integer", "description": "页码（list）"},
				"pageSize": map[string]interface{}{"type": "integer", "description": "每页条数（list）"},
				"file":     map[string]interface{}{"type": "string", "description": "本地文件路径（upload）"},
				"sign":     map[string]interface{}{"type": "string", "description": "图片签名（delete）"},
				"type":     map[string]interface{}{"type": "string", "description": "图片类型（upload: favicon/watermark/空）"},
			}, "required": []string{"command"},
		},
	}, p.handleManageImages)

	s.RegisterTool(tp+"manage_links", sdk.ToolDef{
		Name: tp + "manage_links",
		Description: "管理友情链接。命令: list|create|update|delete",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string", "description": "list|create|update|delete"},
				"data":    map[string]interface{}{"type": "string", "description": "JSON数据（create/update）"},
				"name":    map[string]interface{}{"type": "string", "description": "链接名（delete）"},
			}, "required": []string{"command"},
		},
	}, p.handleManageLinks)

	s.RegisterTool(tp+"manage_social", sdk.ToolDef{
		Name: tp + "manage_social",
		Description: "管理社交链接。命令: list|types|create|update|delete",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string", "description": "list|types|create|update|delete"},
				"data":    map[string]interface{}{"type": "string", "description": "JSON数据（create/update）"},
				"stype":   map[string]interface{}{"type": "string", "description": "社交类型（delete）"},
			}, "required": []string{"command"},
		},
	}, p.handleManageSocial)

	s.RegisterTool(tp+"manage_rewards", sdk.ToolDef{
		Name: tp + "manage_rewards",
		Description: "管理打赏设置。命令: list|create|update|delete",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string", "description": "list|create|update|delete"},
				"data":    map[string]interface{}{"type": "string", "description": "JSON数据（create/update）"},
				"name":    map[string]interface{}{"type": "string", "description": "打赏项名（delete）"},
			}, "required": []string{"command"},
		},
	}, p.handleManageRewards)

	s.RegisterTool(tp+"manage_pages", sdk.ToolDef{
		Name: tp + "manage_pages",
		Description: "管理自定义页面。命令: list|get|create|update|delete|folder|file|upload|create_file|create_folder|update_file",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string", "description": "list|get|create|update|delete|folder|file|upload|create_file|create_folder|update_file"},
				"path":    map[string]interface{}{"type": "string", "description": "页面路径"},
				"data":    map[string]interface{}{"type": "string", "description": "JSON数据（create/update）"},
				"folder":  map[string]interface{}{"type": "string", "description": "文件夹路径（folder/file相关）"},
				"file":    map[string]interface{}{"type": "string", "description": "文件路径（file/upload相关）"},
				"content": map[string]interface{}{"type": "string", "description": "文件内容（create_file/update_file）"},
			}, "required": []string{"command"},
		},
	}, p.handleManagePages)

	s.RegisterTool(tp+"manage_settings", sdk.ToolDef{
		Name: tp + "manage_settings",
		Description: "管理站点设置。命令: get_static|set_static|get_waline|set_waline|get_layout|set_layout|get_login|set_login",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string", "description": "get_static|set_static|get_waline|set_waline|get_layout|set_layout|get_login|set_login"},
				"data":    map[string]interface{}{"type": "string", "description": "JSON设置数据（set_*命令）"},
			}, "required": []string{"command"},
		},
	}, p.handleManageSettings)

	s.RegisterTool(tp+"manage_about", sdk.ToolDef{
		Name: tp + "manage_about",
		Description: "管理关于页。命令: get|update",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string", "description": "get|update"},
				"content": map[string]interface{}{"type": "string", "description": "关于页内容（update）"},
			}, "required": []string{"command"},
		},
	}, p.handleManageAbout)

	s.RegisterTool(tp+"manage_site", sdk.ToolDef{
		Name: tp + "manage_site",
		Description: "管理站点信息。命令: get|update",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string", "description": "get|update"},
				"data":    map[string]interface{}{"type": "string", "description": "JSON站点数据（update）"},
			}, "required": []string{"command"},
		},
	}, p.handleManageSite)

	s.RegisterTool(tp+"manage_menu", sdk.ToolDef{
		Name: tp + "manage_menu",
		Description: "管理导航菜单。命令: get|update",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string", "description": "get|update"},
				"data":    map[string]interface{}{"type": "string", "description": "JSON菜单数据（update）"},
			}, "required": []string{"command"},
		},
	}, p.handleManageMenu)

	s.RegisterTool(tp+"get_analysis", sdk.ToolDef{
		Name: tp + "get_analysis", Description: "获取访问统计数据",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"tab": map[string]interface{}{"type": "string", "description": "overview|viewer|article", "default": "overview"},
			},
		},
	}, p.handleGetAnalysis)

	s.RegisterTool(tp+"get_logs", sdk.ToolDef{
		Name: tp + "get_logs", Description: "获取系统日志",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"page":     map[string]interface{}{"type": "integer", "description": "页码"},
				"pageSize": map[string]interface{}{"type": "integer", "description": "每页条数"},
				"event":    map[string]interface{}{"type": "string", "description": "按事件类型筛选"},
			},
		},
	}, p.handleGetLogs)

	s.RegisterTool(tp+"manage_backup", sdk.ToolDef{
		Name: tp + "manage_backup",
		Description: "备份管理。命令: export|import",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string", "description": "export|import"},
				"file":    map[string]interface{}{"type": "string", "description": "备份JSON文件路径（import）"},
			}, "required": []string{"command"},
		},
	}, p.handleManageBackup)

	s.RegisterTool(tp+"manage_caddy", sdk.ToolDef{
		Name: tp + "manage_caddy",
		Description: "管理Caddy配置。命令: get_https|set_https|get_log|clear_log|get_config",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string", "description": "get_https|set_https|get_log|clear_log|get_config"},
				"data":    map[string]interface{}{"type": "string", "description": "JSON数据（set_https）"},
			}, "required": []string{"command"},
		},
	}, p.handleManageCaddy)

	s.RegisterTool(tp+"manage_isr", sdk.ToolDef{
		Name: tp + "manage_isr",
		Description: "ISR增量渲染。命令: get|trigger|update",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string", "description": "get|trigger|update"},
				"data":    map[string]interface{}{"type": "string", "description": "JSON数据（update）"},
			}, "required": []string{"command"},
		},
	}, p.handleManageISR)

	s.RegisterTool(tp+"manage_pipelines", sdk.ToolDef{
		Name: tp + "manage_pipelines",
		Description: "管理流水线。命令: list|get|config|create|trigger|update|delete",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string", "description": "list|get|config|create|trigger|update|delete"},
				"id":      map[string]interface{}{"type": "string", "description": "流水线ID（get/trigger/update/delete）"},
				"data":    map[string]interface{}{"type": "string", "description": "JSON数据（create/update）"},
			}, "required": []string{"command"},
		},
	}, p.handleManagePipelines)

	s.RegisterTool(tp+"manage_collaborators", sdk.ToolDef{
		Name: tp + "manage_collaborators",
		Description: "管理协作者。命令: list|list_all|create|update|delete",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string", "description": "list|list_all|create|update|delete"},
				"data":    map[string]interface{}{"type": "string", "description": "JSON数据（create/update）"},
				"id":      map[string]interface{}{"type": "string", "description": "协作者ID（delete）"},
			}, "required": []string{"command"},
		},
	}, p.handleManageCollaborators)

	s.RegisterTool(tp+"manage_tokens", sdk.ToolDef{
		Name: tp + "manage_tokens",
		Description: "管理API Token。命令: list|create|delete",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command":     map[string]interface{}{"type": "string", "description": "list|create|delete"},
				"id":          map[string]interface{}{"type": "string", "description": "Token ID（delete）"},
				"expiresIn":   map[string]interface{}{"type": "integer", "description": "有效期毫秒（create，默认1年）"},
			}, "required": []string{"command"},
		},
	}, p.handleManageTokens)

	s.RegisterTool(tp+"auth", sdk.ToolDef{
		Name: tp + "auth",
		Description: "VanBlog认证。命令: login|logout|restore|update",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command":   map[string]interface{}{"type": "string", "description": "login|logout|restore|update"},
				"username":  map[string]interface{}{"type": "string", "description": "用户名（login）"},
				"password":  map[string]interface{}{"type": "string", "description": "密码（login/restore/update）"},
				"newName":   map[string]interface{}{"type": "string", "description": "新用户名（update）"},
				"newPassword": map[string]interface{}{"type": "string", "description": "新密码（update）"},
			}, "required": []string{"command"},
		},
	}, p.handleAuth)

	s.RegisterTool(tp+"get_meta", sdk.ToolDef{
		Name: tp + "get_meta", Description: "获取VanBlog元信息（版本、站点等）",
		Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	}, p.handleGetMeta)

	log.Printf("[vanblog] started: %s (url=%s, token=%t)", p.name, p.baseURL, p.token != "")
	return nil
}

func (p *Plugin) Stop() error { return nil }

func (p *Plugin) do(method, path string, body interface{}) (interface{}, error) {
	return p.doRaw(method, path, body, nil)
}

func (p *Plugin) doRaw(method, path string, body interface{}, headers map[string]string) (interface{}, error) {
	url := p.baseURL + path
	var reqBody io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	if p.token != "" {
		req.Header.Set("token", p.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var result interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		return string(raw), nil
	}
	return result, nil
}

func jsonArg(args map[string]interface{}, key string) (map[string]interface{}, bool) {
	s := readArg(args, key)
	if s == "" {
		return nil, false
	}
	var m map[string]interface{}
	if json.Unmarshal([]byte(s), &m) != nil {
		return nil, false
	}
	return m, true
}

// ======== Article Handlers ========

func (p *Plugin) handleListArticles(args map[string]interface{}) (interface{}, error) {
	page := readArgInt(args, "page", 1)
	pageSize := readArgInt(args, "pageSize", 50)
	path := fmt.Sprintf("/api/admin/article?page=%d&pageSize=%d", page, pageSize)
	if v := readArg(args, "keyword"); v != "" {
		path += "&keyword=" + v
	}
	if v := readArg(args, "category"); v != "" {
		path += "&category=" + v
	}
	if v := readArg(args, "tag"); v != "" {
		path += "&tag=" + v
	}
	if v := readArg(args, "sort"); v != "" {
		path += "&sort=" + v
	}
	return p.do("GET", path, nil)
}

func (p *Plugin) handleGetArticle(args map[string]interface{}) (interface{}, error) {
	id := readArg(args, "id")
	if id == "" {
		return errResult("id 不能为空"), nil
	}
	return p.do("GET", "/api/admin/article/"+id, nil)
}

func (p *Plugin) handleCreateArticle(args map[string]interface{}) (interface{}, error) {
	title := readArg(args, "title")
	category := readArg(args, "category")
	if title == "" || category == "" {
		return errResult("title 和 category 不能为空"), nil
	}
	body := map[string]interface{}{"title": title, "category": category}
	if v := readArg(args, "content"); v != "" {
		body["content"] = v
	}
	if v := readArg(args, "tags"); v != "" {
		body["tags"] = strings.Split(v, ",")
	}
	if v := readArgBool(args, "pinned"); v != nil {
		body["pinned"] = *v
	}
	return p.do("POST", "/api/admin/article", body)
}

func (p *Plugin) handleUpdateArticle(args map[string]interface{}) (interface{}, error) {
	id := readArg(args, "id")
	if id == "" {
		return errResult("id 不能为空"), nil
	}
	body := map[string]interface{}{}
	if v := readArg(args, "title"); v != "" {
		body["title"] = v
	}
	if v := readArg(args, "content"); v != "" {
		body["content"] = v
	}
	if v := readArg(args, "category"); v != "" {
		body["category"] = v
	}
	if v := readArg(args, "tags"); v != "" {
		body["tags"] = strings.Split(v, ",")
	}
	if v := readArgBool(args, "pinned"); v != nil {
		body["pinned"] = *v
	}
	return p.do("PUT", "/api/admin/article/"+id, body)
}

func (p *Plugin) handleDeleteArticle(args map[string]interface{}) (interface{}, error) {
	id := readArg(args, "id")
	if id == "" {
		return errResult("id 不能为空"), nil
	}
	return p.do("DELETE", "/api/admin/article/"+id, nil)
}

func (p *Plugin) handleSearchArticles(args map[string]interface{}) (interface{}, error) {
	link := readArg(args, "link")
	if link == "" {
		return errResult("link 不能为空"), nil
	}
	return p.do("POST", "/api/admin/article/searchByLink", map[string]interface{}{"link": link})
}

// ======== Draft ========

func (p *Plugin) handleManageDrafts(args map[string]interface{}) (interface{}, error) {
	cmd := readArg(args, "command")
	switch cmd {
	case "list":
		page := readArgInt(args, "page", 1)
		pageSize := readArgInt(args, "pageSize", 50)
		return p.do("GET", fmt.Sprintf("/api/admin/draft?page=%d&pageSize=%d", page, pageSize), nil)
	case "get":
		id := readArg(args, "id")
		if id == "" {
			return errResult("id 不能为空"), nil
		}
		return p.do("GET", "/api/admin/draft/"+id, nil)
	case "create":
		body := map[string]interface{}{}
		if v := readArg(args, "title"); v != "" {
			body["title"] = v
		}
		if v := readArg(args, "content"); v != "" {
			body["content"] = v
		}
		if v := readArg(args, "category"); v != "" {
			body["category"] = v
		}
		if v := readArg(args, "tags"); v != "" {
			body["tags"] = strings.Split(v, ",")
		}
		return p.do("POST", "/api/admin/draft", body)
	case "update":
		id := readArg(args, "id")
		if id == "" {
			return errResult("id 不能为空"), nil
		}
		body := map[string]interface{}{}
		if v := readArg(args, "title"); v != "" {
			body["title"] = v
		}
		if v := readArg(args, "content"); v != "" {
			body["content"] = v
		}
		if v := readArg(args, "category"); v != "" {
			body["category"] = v
		}
		if v := readArg(args, "tags"); v != "" {
			body["tags"] = strings.Split(v, ",")
		}
		return p.do("PUT", "/api/admin/draft/"+id, body)
	case "delete":
		id := readArg(args, "id")
		if id == "" {
			return errResult("id 不能为空"), nil
		}
		return p.do("DELETE", "/api/admin/draft/"+id, nil)
	case "publish":
		id := readArg(args, "id")
		if id == "" {
			return errResult("id 不能为空"), nil
		}
		return p.do("POST", "/api/admin/draft/publish", map[string]interface{}{"id": id})
	default:
		return errResult("未知命令: " + cmd), nil
	}
}

// ======== Category ========

func (p *Plugin) handleManageCategories(args map[string]interface{}) (interface{}, error) {
	cmd := readArg(args, "command")
	switch cmd {
	case "list":
		return p.do("GET", "/api/admin/category/all", nil)
	case "get":
		name := readArg(args, "name")
		if name == "" {
			return errResult("name 不能为空"), nil
		}
		return p.do("GET", "/api/admin/category/"+name, nil)
	case "create":
		name := readArg(args, "name")
		if name == "" {
			return errResult("name 不能为空"), nil
		}
		return p.do("POST", "/api/admin/category", map[string]interface{}{"name": name})
	case "update":
		name := readArg(args, "name")
		if name == "" {
			return errResult("name 不能为空"), nil
		}
		newName := readArg(args, "newName")
		return p.do("PUT", "/api/admin/category/"+name, map[string]interface{}{"name": newName})
	case "delete":
		name := readArg(args, "name")
		if name == "" {
			return errResult("name 不能为空"), nil
		}
		return p.do("DELETE", "/api/admin/category/"+name, nil)
	default:
		return errResult("未知命令: " + cmd), nil
	}
}

// ======== Tag ========

func (p *Plugin) handleManageTags(args map[string]interface{}) (interface{}, error) {
	cmd := readArg(args, "command")
	switch cmd {
	case "list":
		return p.do("GET", "/api/admin/tag/all", nil)
	case "get":
		name := readArg(args, "name")
		if name == "" {
			return errResult("name 不能为空"), nil
		}
		return p.do("GET", "/api/admin/tag/"+name, nil)
	case "rename":
		name := readArg(args, "name")
		newName := readArg(args, "newName")
		if name == "" || newName == "" {
			return errResult("name 和 newName 不能为空"), nil
		}
		return p.do("PUT", "/api/admin/tag/"+name, map[string]interface{}{"name": newName})
	case "delete":
		name := readArg(args, "name")
		if name == "" {
			return errResult("name 不能为空"), nil
		}
		return p.do("DELETE", "/api/admin/tag/"+name, nil)
	default:
		return errResult("未知命令: " + cmd), nil
	}
}

// ======== Image ========

func (p *Plugin) handleManageImages(args map[string]interface{}) (interface{}, error) {
	cmd := readArg(args, "command")
	switch cmd {
	case "list":
		page := readArgInt(args, "page", 1)
		pageSize := readArgInt(args, "pageSize", 50)
		return p.do("GET", fmt.Sprintf("/api/admin/img?page=%d&pageSize=%d", page, pageSize), nil)
	case "all":
		return p.do("GET", "/api/admin/img/all", nil)
	case "upload":
		filePath := readArg(args, "file")
		if filePath == "" {
			return errResult("file 路径不能为空"), nil
		}
		imgType := readArg(args, "type")
		return p.uploadImage(filePath, imgType)
	case "scan":
		return p.do("POST", "/api/admin/img/scan", nil)
	case "export":
		return p.do("POST", "/api/admin/img/export", nil)
	case "delete":
		sign := readArg(args, "sign")
		if sign == "" {
			return errResult("sign 不能为空"), nil
		}
		return p.do("DELETE", "/api/admin/img/"+sign, nil)
	case "delete_all":
		return p.do("DELETE", "/api/admin/img/all/delete", nil)
	default:
		return errResult("未知命令: " + cmd), nil
	}
}

func (p *Plugin) uploadImage(filePath, imgType string) (interface{}, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("打开文件失败: %w", err)
	}
	defer f.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("image", filePath)
	if err != nil {
		return nil, fmt.Errorf("创建表单失败: %w", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return nil, fmt.Errorf("写入文件失败: %w", err)
	}
	if imgType != "" {
		w.WriteField("type", imgType)
	}
	w.Close()

	req, err := http.NewRequest("POST", p.baseURL+"/api/admin/img/upload", &buf)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("token", p.token)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("上传失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var result interface{}
	if json.Unmarshal(raw, &result) != nil {
		return string(raw), nil
	}
	return result, nil
}

// ======== Link ========

func (p *Plugin) handleManageLinks(args map[string]interface{}) (interface{}, error) {
	cmd := readArg(args, "command")
	switch cmd {
	case "list":
		return p.do("GET", "/api/admin/meta/link", nil)
	case "create":
		data, ok := jsonArg(args, "data")
		if !ok {
			return errResult("data JSON 不能为空"), nil
		}
		return p.do("POST", "/api/admin/meta/link", data)
	case "update":
		data, ok := jsonArg(args, "data")
		if !ok {
			return errResult("data JSON 不能为空"), nil
		}
		return p.do("PUT", "/api/admin/meta/link", data)
	case "delete":
		name := readArg(args, "name")
		if name == "" {
			return errResult("name 不能为空"), nil
		}
		return p.do("DELETE", "/api/admin/meta/link/"+name, nil)
	default:
		return errResult("未知命令: " + cmd), nil
	}
}

// ======== Social ========

func (p *Plugin) handleManageSocial(args map[string]interface{}) (interface{}, error) {
	cmd := readArg(args, "command")
	switch cmd {
	case "list":
		return p.do("GET", "/api/admin/meta/social", nil)
	case "types":
		return p.do("GET", "/api/admin/meta/social/types", nil)
	case "create":
		data, ok := jsonArg(args, "data")
		if !ok {
			return errResult("data JSON 不能为空"), nil
		}
		return p.do("POST", "/api/admin/meta/social", data)
	case "update":
		data, ok := jsonArg(args, "data")
		if !ok {
			return errResult("data JSON 不能为空"), nil
		}
		return p.do("PUT", "/api/admin/meta/social", data)
	case "delete":
		stype := readArg(args, "stype")
		if stype == "" {
			return errResult("stype 不能为空"), nil
		}
		return p.do("DELETE", "/api/admin/meta/social/"+stype, nil)
	default:
		return errResult("未知命令: " + cmd), nil
	}
}

// ======== Reward ========

func (p *Plugin) handleManageRewards(args map[string]interface{}) (interface{}, error) {
	cmd := readArg(args, "command")
	switch cmd {
	case "list":
		return p.do("GET", "/api/admin/meta/reward", nil)
	case "create":
		data, ok := jsonArg(args, "data")
		if !ok {
			return errResult("data JSON 不能为空"), nil
		}
		return p.do("POST", "/api/admin/meta/reward", data)
	case "update":
		data, ok := jsonArg(args, "data")
		if !ok {
			return errResult("data JSON 不能为空"), nil
		}
		return p.do("PUT", "/api/admin/meta/reward", data)
	case "delete":
		name := readArg(args, "name")
		if name == "" {
			return errResult("name 不能为空"), nil
		}
		return p.do("DELETE", "/api/admin/meta/reward/"+name, nil)
	default:
		return errResult("未知命令: " + cmd), nil
	}
}

// ======== Custom Page ========

func (p *Plugin) handleManagePages(args map[string]interface{}) (interface{}, error) {
	cmd := readArg(args, "command")
	switch cmd {
	case "list":
		return p.do("GET", "/api/admin/customPage/all", nil)
	case "get":
		path := readArg(args, "path")
		if path == "" {
			return p.do("GET", "/api/admin/customPage", nil)
		}
		return p.do("GET", "/api/admin/customPage?path="+path, nil)
	case "create":
		data, ok := jsonArg(args, "data")
		if !ok {
			return errResult("data JSON 不能为空"), nil
		}
		return p.do("POST", "/api/admin/customPage", data)
	case "update":
		data, ok := jsonArg(args, "data")
		if !ok {
			return errResult("data JSON 不能为空"), nil
		}
		return p.do("PUT", "/api/admin/customPage", data)
	case "delete":
		data, ok := jsonArg(args, "data")
		if !ok {
			return errResult("data JSON 不能为空"), nil
		}
		return p.do("DELETE", "/api/admin/customPage", data)
	case "folder":
		folder := readArg(args, "folder")
		return p.do("GET", "/api/admin/customPage/folder?folder="+folder, nil)
	case "file":
		file := readArg(args, "file")
		return p.do("GET", "/api/admin/customPage/file?file="+file, nil)
	case "upload":
		filePath := readArg(args, "file")
		if filePath == "" {
			return errResult("file 路径不能为空"), nil
		}
		return p.uploadCustomPageFile(filePath)
	case "create_file":
		file := readArg(args, "file")
		content := readArg(args, "content")
		return p.do("POST", "/api/admin/customPage/file", map[string]interface{}{"file": file, "content": content})
	case "create_folder":
		folder := readArg(args, "folder")
		return p.do("POST", "/api/admin/customPage/folder", map[string]interface{}{"folder": folder})
	case "update_file":
		file := readArg(args, "file")
		content := readArg(args, "content")
		return p.do("PUT", "/api/admin/customPage/file", map[string]interface{}{"file": file, "content": content})
	default:
		return errResult("未知命令: " + cmd), nil
	}
}

func (p *Plugin) uploadCustomPageFile(filePath string) (interface{}, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("打开文件失败: %w", err)
	}
	defer f.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", filePath)
	if err != nil {
		return nil, fmt.Errorf("创建表单失败: %w", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return nil, fmt.Errorf("写入文件失败: %w", err)
	}
	w.Close()

	req, err := http.NewRequest("POST", p.baseURL+"/api/admin/customPage/upload", &buf)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("token", p.token)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("上传失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var result interface{}
	if json.Unmarshal(raw, &result) != nil {
		return string(raw), nil
	}
	return result, nil
}

// ======== Settings ========

func (p *Plugin) handleManageSettings(args map[string]interface{}) (interface{}, error) {
	cmd := readArg(args, "command")
	switch cmd {
	case "get_static":
		return p.do("GET", "/api/admin/setting/static", nil)
	case "set_static":
		data, ok := jsonArg(args, "data")
		if !ok {
			return errResult("data JSON 不能为空"), nil
		}
		return p.do("PUT", "/api/admin/setting/static", data)
	case "get_waline":
		return p.do("GET", "/api/admin/setting/waline", nil)
	case "set_waline":
		data, ok := jsonArg(args, "data")
		if !ok {
			return errResult("data JSON 不能为空"), nil
		}
		return p.do("PUT", "/api/admin/setting/waline", data)
	case "get_layout":
		return p.do("GET", "/api/admin/setting/layout", nil)
	case "set_layout":
		data, ok := jsonArg(args, "data")
		if !ok {
			return errResult("data JSON 不能为空"), nil
		}
		return p.do("PUT", "/api/admin/setting/layout", data)
	case "get_login":
		return p.do("GET", "/api/admin/setting/login", nil)
	case "set_login":
		data, ok := jsonArg(args, "data")
		if !ok {
			return errResult("data JSON 不能为空"), nil
		}
		return p.do("PUT", "/api/admin/setting/login", data)
	default:
		return errResult("未知命令: " + cmd), nil
	}
}

// ======== About ========

func (p *Plugin) handleManageAbout(args map[string]interface{}) (interface{}, error) {
	cmd := readArg(args, "command")
	switch cmd {
	case "get":
		return p.do("GET", "/api/admin/meta/about", nil)
	case "update":
		content := readArg(args, "content")
		return p.do("PUT", "/api/admin/meta/about", map[string]interface{}{"content": content})
	default:
		return errResult("未知命令: " + cmd), nil
	}
}

// ======== Site ========

func (p *Plugin) handleManageSite(args map[string]interface{}) (interface{}, error) {
	cmd := readArg(args, "command")
	switch cmd {
	case "get":
		return p.do("GET", "/api/admin/meta/site", nil)
	case "update":
		data, ok := jsonArg(args, "data")
		if !ok {
			return errResult("data JSON 不能为空"), nil
		}
		return p.do("PUT", "/api/admin/meta/site", data)
	default:
		return errResult("未知命令: " + cmd), nil
	}
}

// ======== Menu ========

func (p *Plugin) handleManageMenu(args map[string]interface{}) (interface{}, error) {
	cmd := readArg(args, "command")
	switch cmd {
	case "get":
		return p.do("GET", "/api/admin/meta/menu", nil)
	case "update":
		data, ok := jsonArg(args, "data")
		if !ok {
			return errResult("data JSON 不能为空"), nil
		}
		return p.do("PUT", "/api/admin/meta/menu", data)
	default:
		return errResult("未知命令: " + cmd), nil
	}
}

// ======== Analysis ========

func (p *Plugin) handleGetAnalysis(args map[string]interface{}) (interface{}, error) {
	tab := readArg(args, "tab")
	if tab == "" {
		tab = "overview"
	}
	return p.do("GET", "/api/admin/analysis?tab="+tab, nil)
}

// ======== Log ========

func (p *Plugin) handleGetLogs(args map[string]interface{}) (interface{}, error) {
	page := readArgInt(args, "page", 1)
	pageSize := readArgInt(args, "pageSize", 50)
	path := fmt.Sprintf("/api/admin/log?page=%d&pageSize=%d", page, pageSize)
	if v := readArg(args, "event"); v != "" {
		path += "&event=" + v
	}
	return p.do("GET", path, nil)
}

// ======== Backup ========

func (p *Plugin) handleManageBackup(args map[string]interface{}) (interface{}, error) {
	cmd := readArg(args, "command")
	switch cmd {
	case "export":
		return p.do("GET", "/api/admin/backup/export", nil)
	case "import":
		filePath := readArg(args, "file")
		if filePath == "" {
			return errResult("file 路径不能为空"), nil
		}
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("读取备份文件失败: %w", err)
		}
		var body interface{}
		if json.Unmarshal(data, &body) != nil {
			return errResult("无效的备份JSON"), nil
		}
		return p.do("POST", "/api/admin/backup/import", body)
	default:
		return errResult("未知命令: " + cmd), nil
	}
}

// ======== Caddy ========

func (p *Plugin) handleManageCaddy(args map[string]interface{}) (interface{}, error) {
	cmd := readArg(args, "command")
	switch cmd {
	case "get_https":
		return p.do("GET", "/api/admin/caddy/https", nil)
	case "set_https":
		data, ok := jsonArg(args, "data")
		if !ok {
			return errResult("data JSON 不能为空"), nil
		}
		return p.do("PUT", "/api/admin/caddy/https", data)
	case "get_log":
		return p.do("GET", "/api/admin/caddy/log", nil)
	case "clear_log":
		return p.do("DELETE", "/api/admin/caddy/log", nil)
	case "get_config":
		return p.do("GET", "/api/admin/caddy/config", nil)
	default:
		return errResult("未知命令: " + cmd), nil
	}
}

// ======== ISR ========

func (p *Plugin) handleManageISR(args map[string]interface{}) (interface{}, error) {
	cmd := readArg(args, "command")
	switch cmd {
	case "get":
		return p.do("GET", "/api/admin/isr", nil)
	case "trigger":
		return p.do("POST", "/api/admin/isr", nil)
	case "update":
		data, ok := jsonArg(args, "data")
		if !ok {
			return errResult("data JSON 不能为空"), nil
		}
		return p.do("PUT", "/api/admin/isr", data)
	default:
		return errResult("未知命令: " + cmd), nil
	}
}

// ======== Pipeline ========

func (p *Plugin) handleManagePipelines(args map[string]interface{}) (interface{}, error) {
	cmd := readArg(args, "command")
	switch cmd {
	case "list":
		return p.do("GET", "/api/admin/pipeline", nil)
	case "config":
		return p.do("GET", "/api/admin/pipeline/config", nil)
	case "get":
		id := readArg(args, "id")
		if id == "" {
			return errResult("id 不能为空"), nil
		}
		return p.do("GET", "/api/admin/pipeline/"+id, nil)
	case "create":
		data, ok := jsonArg(args, "data")
		if !ok {
			return errResult("data JSON 不能为空"), nil
		}
		return p.do("POST", "/api/admin/pipeline", data)
	case "trigger":
		id := readArg(args, "id")
		if id == "" {
			return errResult("id 不能为空"), nil
		}
		return p.do("POST", "/api/admin/pipeline/trigger/"+id, nil)
	case "update":
		id := readArg(args, "id")
		if id == "" {
			return errResult("id 不能为空"), nil
		}
		data, ok := jsonArg(args, "data")
		if !ok {
			return errResult("data JSON 不能为空"), nil
		}
		return p.do("PUT", "/api/admin/pipeline/"+id, data)
	case "delete":
		id := readArg(args, "id")
		if id == "" {
			return errResult("id 不能为空"), nil
		}
		return p.do("DELETE", "/api/admin/pipeline/"+id, nil)
	default:
		return errResult("未知命令: " + cmd), nil
	}
}

// ======== Collaborator ========

func (p *Plugin) handleManageCollaborators(args map[string]interface{}) (interface{}, error) {
	cmd := readArg(args, "command")
	switch cmd {
	case "list":
		return p.do("GET", "/api/admin/collaborator", nil)
	case "list_all":
		return p.do("GET", "/api/admin/collaborator/list", nil)
	case "create":
		data, ok := jsonArg(args, "data")
		if !ok {
			return errResult("data JSON 不能为空"), nil
		}
		return p.do("POST", "/api/admin/collaborator", data)
	case "update":
		data, ok := jsonArg(args, "data")
		if !ok {
			return errResult("data JSON 不能为空"), nil
		}
		return p.do("PUT", "/api/admin/collaborator", data)
	case "delete":
		id := readArg(args, "id")
		if id == "" {
			return errResult("id 不能为空"), nil
		}
		return p.do("DELETE", "/api/admin/collaborator/"+id, nil)
	default:
		return errResult("未知命令: " + cmd), nil
	}
}

// ======== Token ========

func (p *Plugin) handleManageTokens(args map[string]interface{}) (interface{}, error) {
	cmd := readArg(args, "command")
	switch cmd {
	case "list":
		return p.do("GET", "/api/admin/token", nil)
	case "create":
		expiresIn := readArgInt(args, "expiresIn", 3153600000)
		return p.do("POST", "/api/admin/token", map[string]interface{}{"expiresIn": expiresIn})
	case "delete":
		id := readArg(args, "id")
		if id == "" {
			return errResult("id 不能为空"), nil
		}
		return p.do("DELETE", "/api/admin/token/"+id, nil)
	default:
		return errResult("未知命令: " + cmd), nil
	}
}

// ======== Auth ========

func (p *Plugin) handleAuth(args map[string]interface{}) (interface{}, error) {
	cmd := readArg(args, "command")
	switch cmd {
	case "login":
		username := readArg(args, "username")
		password := readArg(args, "password")
		if username == "" || password == "" {
			return errResult("username 和 password 不能为空"), nil
		}
		result, err := p.do("POST", "/api/admin/auth/login", map[string]interface{}{"username": username, "password": password})
		if err != nil {
			return nil, err
		}
		if m, ok := result.(map[string]interface{}); ok {
			if data, ok := m["data"].(map[string]interface{}); ok {
				if token, ok := data["token"].(string); ok && token != "" {
					p.token = token
					p.sdk.Settings().Set("token", token)
				}
			}
		}
		return result, nil
	case "logout":
		return p.do("POST", "/api/admin/auth/logout", nil)
	case "restore":
		password := readArg(args, "password")
		if password == "" {
			return errResult("password 不能为空"), nil
		}
		return p.doRaw("POST", "/api/admin/auth/restore", map[string]interface{}{"password": password}, map[string]string{"token": p.resetToken})
	case "update":
		body := map[string]interface{}{}
		if v := readArg(args, "newName"); v != "" {
			body["name"] = v
		}
		if v := readArg(args, "newPassword"); v != "" {
			body["password"] = v
		}
		return p.do("PUT", "/api/admin/auth", body)
	default:
		return errResult("未知命令: " + cmd), nil
	}
}

// ======== Meta ========

func (p *Plugin) handleGetMeta(args map[string]interface{}) (interface{}, error) {
	return p.do("GET", "/api/admin/meta", nil)
}
