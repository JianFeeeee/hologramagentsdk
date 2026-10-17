package main

import (
	"errors"
	"os"
)

// readEmbeddedLuaTemplate 读 Lua 脚手架模板（若存在）。
func readEmbeddedLuaTemplate() (string, error) {
	// Lua 模板以 embed 或常量形式存在时取之；不存在则跳过。
	if tmplMainLua != "" {
		return tmplMainLua, nil
	}
	if b, err := os.ReadFile("templates/main.lua.tmpl"); err == nil {
		return string(b), nil
	}
	return "", errors.New("未找到 Lua 脚手架模板")
}
