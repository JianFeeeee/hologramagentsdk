package sdk

// 受限事件总线（EventsAPI）。
//
// ## 为什么现在才敢暴露
//
// 事件总线是内核内部机制：它连接内核模块、状态面、WebUI 与插件。
// 早先不给插件的理由是**没有边界**——一给就是"任何插件能发任何事件、能订阅任何事件"，
// 等于把内核内部的同步面变成公共 API，且没有任何审查。
//
// 2026-10 之后内核侧有了 security 模块（凭据闸门 + 内容审查 + 审计），
// 于是可以只开**受限的一条缝**：
//
//	· 发布：只能用白名单事件类型，且只能在自己的命名空间下扩展
//	· 载荷：出向经 security 审查（明文凭据会被换成 @{别名}），并限制大小
//	· 订阅：只能订阅白名单里的只读事件
//
// ## 插件能做什么
//
//	Publish("skill_detected", {...})           // 交接类事件（兼容层 → skillmgr）
//	Publish("plugin.<自己名字>.<任意>", {...})  // 自己的命名空间，供插件间协作
//	Subscribe("tools_ready", handler)          // 等内核就绪再做事的入口
//
// 越界会**报错**而不是静默丢弃：静默丢弃会让插件以为发成功了。

// EventsAPI 是插件可见的事件总线。
//
// 它**不是** *events.Bus 的投影：这是一个有边界、有审查的接口。
type EventsAPI interface {
	// Publish 发布一个事件。
	//
	// eventType 必须在白名单内（见 AllowedPublish），或形如
	// "plugin.<本插件名>.<后缀>"。载荷大小与键数有上限。
	Publish(eventType string, payload map[string]interface{}) error

	// Subscribe 订阅一个白名单内的事件（只读面）。
	//
	// 返回的 cancel 在插件 Stop 时调用可解除订阅；宿主卸载插件时也会自动解除。
	// handler 的 panic 会被总线隔离，不影响其他订阅者。
	Subscribe(eventType string, handler func(payload map[string]interface{})) (cancel func(), err error)

	// AllowedPublish 返回当前可发布的事件类型（白名单 + 自己的命名空间前缀）。
	AllowedPublish() []string

	// AllowedSubscribe 返回当前可订阅的事件类型。
	AllowedSubscribe() []string
}

// 插件可发布的事件类型白名单。
//
// ★ 加一项前先问：这个事件会不会让插件**间接驱动内核行为**？
// 会的话就不能加——事件是通知，不是未授权的执行入口。
var pluginPublishableEvents = []string{
	// skill_detected：兼容层（openclaw/clawhub 目录扫描）把发现的纯 SKILL 条目
	// 交给 skillmgr 接管。这是**职责链交接**，不是执行指令。
	"skill_detected",
}

// 插件可订阅的事件类型白名单（只读面）。
var pluginSubscribableEvents = []string{
	// tools_ready：全部插件加载完、工具注册齐。插件据此开始"需要工具才能做"的工作。
	"tools_ready",
	// security_ready：安全闸门装配完成（含"是否真的在生效"）。
	"security_ready",
	// device_ready：工具/设备数据面就绪。
	"device_ready",
}

// PluginPublishableEvents 返回可发布事件白名单的副本（供工具链校验与文档）。
func PluginPublishableEvents() []string {
	out := make([]string, len(pluginPublishableEvents))
	copy(out, pluginPublishableEvents)
	return out
}

// PluginSubscribableEvents 返回可订阅事件白名单的副本。
func PluginSubscribableEvents() []string {
	out := make([]string, len(pluginSubscribableEvents))
	copy(out, pluginSubscribableEvents)
	return out
}
