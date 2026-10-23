//go:build !windows || !cgo

package main

import (
	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// NewPlugin 是子进程插件的入口：hmapdev 注入的 main 包装只认这个函数名，
// 而真正的构造逻辑留在 NewPluginFactory 里，便于同包测试直接调用。
func NewPlugin(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return NewPluginFactory(name, config)
}
