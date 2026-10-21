//go:build !windows || !cgo

package main

import (
	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

func NewPlugin(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return NewPluginFactory(name, config)
}
