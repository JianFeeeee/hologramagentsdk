# 第一个 Go 插件

以下是一个**能直接跑起来**的最小插件：注册一个工具、声明一项配置、处理停止与卸载。

## 1. 生成工程

```bash
hmapdev init myplugin
cd myplugin
```

生成的结构：

```
myplugin/
├── plg.json       — 插件元信息（名称、版本、入口、目标平台）
├── plugin.go      — 插件实现
├── go.mod         — 模块定义
├── README.md
└── thirdpart/     — 外部源码存放目录（可选）
```

`hmapdev build` 时会在构建目录自动生成子进程运行时（`z_proc_gen.go` 等），
**不需要手工创建，也不要提交**。

## 2. 插件实现

插件的全部契约是一个 `Plugin` 接口（[API 参考](../api/lifecycle.md#plugin)）：

| 方法 | 何时调用 |
|---|---|
| `Name() string` | 内核需要标识这个插件时 |
| `Start(*sdk.PluginSDK) error` | 插件加载后。**在这里注册工具、通道、配置** |
| `Stop() error` | 插件停止时（重载、禁用、内核退出都会触发） |

再加一个工厂函数。**名字必须是 `NewPluginFactory`** —— 生成的运行时按这个名字调用：

```go
func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
    return &Plugin{name: name}, nil
}
```

!!! warning "不要写成 `NewPlugin`"
    生成的子进程运行时调用的入口是 `NewPluginFactory`。仓库里有 3 个早期示例
    同时保留了两个名字（`NewPlugin` 只是遗留别名），但新插件只写
    `NewPluginFactory` 即可。写错名字的后果是**编译能过、加载时找不到入口**。

## 3. 一个完整的例子

这是一个「打招呼」工具，带一项配置：

```go
package main

import (
	"fmt"

	"github.com/JianFeeeee/hologramagentsdk/sdk"
)

type Plugin struct {
	name string
	sdk  *sdk.PluginSDK
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	p.sdk = s

	// ① 声明配置项：内核会把它渲染到 WebUI 设置页
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key:         "plugin.myplugin.greeting",
		Default:     "hello",
		Type:        "string",
		DisplayName: "问候语",
		Description: "打招呼时使用的前缀",
		Category:    "myplugin",
	})

	// ② 注册工具：模型看到 Description 后决定是否调用
	tp := p.name + "_"
	s.RegisterTool(tp+"hello", sdk.ToolDef{
		Name:        tp + "hello",
		Description: "向指定的人打招呼",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"who": map[string]interface{}{
					"type":        "string",
					"description": "要打招呼的对象",
				},
			},
			"required": []string{"who"},
		},
	}, p.handleHello)

	// ③ 卸载（插件被删除）前清理自己产生的数据。
	//    注意与 Stop 的区别：Stop 在每次重载时也会触发。
	s.RegisterOnRemoveHandler(func() {
		fmt.Printf("[%s] 清理数据\n", p.name)
	})

	return nil
}

func (p *Plugin) Stop() error { return nil }

func (p *Plugin) handleHello(args map[string]interface{}) (interface{}, error) {
	who, _ := args["who"].(string)

	greeting := "hello"
	if v, err := p.sdk.Settings().Get("plugin.myplugin.greeting"); err == nil && v != "" {
		greeting = v
	}

	return map[string]interface{}{
		"content": fmt.Sprintf("%s, %s!", greeting, who),
	}, nil
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}
```

## 4. 工具返回值的两条约定

`ToolHandler` 返回 `(interface{}, error)`，模型侧看到的是一条 tool message：

- **正常结果**：返回一个 map，把要展示给模型的文本放在 `content` 字段。
  未识别的字段也会一并传给模型，可以放结构化数据。
- **业务失败**：返回 `map[string]interface{}{"isError": true, "content": "原因"}`
  **并返回 nil error**。这样模型能看到失败原因并自行调整；
  若返回 Go 的 `error`，那是**工具调用本身出错**，语义不同。

```go
func errorResult(msg string) map[string]interface{} {
	return map[string]interface{}{"isError": true, "content": msg}
}
```

## 5. 构建与安装

```bash
hmapdev build            # 默认产出多平台 bundle
# → dist/myplugin_bundle.hmap

hmapdev build --no-bundle  # 只构建当前平台
# → dist/myplugin_linux_amd64.hmap
```

安装到内核：在 WebUI 的插件管理页上传 `.hmap`，或从 URL / 本地路径安装。
详见 [打包与发布](packaging.md)。

## 下一步

- [能力边界](capability-boundary.md) —— 哪些 API 外部插件能用
- [工具（Tools）](../api/tools.md) —— `ToolDef` 的完整字段
- [记忆（Memory）](../api/memory.md) —— 让插件读写长期记忆
- [示例插件](../examples/index.md) —— `example/rtvoice` 是完整的可读实现；
  其余插件见 [GitHub 组织 HologramAgent](https://github.com/HologramAgent)
