//go:build !windows || !cgo

package main

import (
	sdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

func NewPlugin(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return NewPluginFactory(name, config)
}
