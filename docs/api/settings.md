<!-- 本页由 tools/apidoc/gensite 从源码生成，请勿手改；要改文档就改 sdk/*.go 的注释。 -->

# 配置（Settings）

声明插件自己的配置项，内核会把它渲染到 WebUI 的设置页，并为每个插件维护独立的配置表。

## `SettingsAPI`

| 方法 | 说明 |
|---|---|
| [`DataDir`](#settingsapidatadir) | DataDir returns the plugin-specific data directory (guaranteed to exist): |
| [`Defs`](#settingsapidefs) | Defs returns config definitions matching the prefix. |
| [`Dump`](#settingsapidump) | Dump returns all config values. |
| [`Get`](#settingsapiget) | Get reads the plugin's own config value (config_<name> table). |
| [`GetCore`](#settingsapigetcore) | GetCore reads the core config table. |
| [`GetPlugin`](#settingsapigetplugin) | GetPlugin reads another plugin's config table. |
| [`GetSecret`](#settingsapigetsecret) | GetSecret reads a value written by SetSecret (transparently decrypted). |
| [`List`](#settingsapilist) | List returns all keys matching the given prefix. |
| [`ListCore`](#settingsapilistcore) | ListCore lists core config keys matching the prefix. |
| [`ListPlugin`](#settingsapilistplugin) | ListPlugin lists another plugin's config keys matching the prefix. |
| [`Plugins`](#settingsapiplugins) | Plugins returns a list of all plugin config namespaces. |
| [`RegisterDef`](#settingsapiregisterdef) | RegisterDef registers a config definition for UI display. |
| [`Set`](#settingsapiset) | Set writes a config value to the plugin's own config table. |
| [`SetCore`](#settingsapisetcore) | SetCore writes to the core config table. |
| [`SetPlugin`](#settingsapisetplugin) | SetPlugin writes to another plugin's config table. |
| [`SetSecret`](#settingsapisetsecret) | SetSecret writes a value that is ALWAYS encrypted at rest, regardless of |

### `SettingsAPI.DataDir`

```go
DataDir() string
```

DataDir returns the plugin-specific data directory (guaranteed to exist):
<daemon data>/plugin_data/<plugin_name>. Plugins should persist any
runtime files (generated images, caches, downloads) here.

<small>`settings.go:40`</small>

### `SettingsAPI.Defs`

```go
Defs(prefix string) []*ConfigDef
```

Defs returns config definitions matching the prefix.

<small>`settings.go:55`</small>

### `SettingsAPI.Dump`

```go
Dump() map[string]interface{}
```

Dump returns all config values.

<small>`settings.go:58`</small>

### `SettingsAPI.Get`

```go
Get(key string) (interface{}, error)
```

Get reads the plugin's own config value (config_<name> table).

<small>`settings.go:5`</small>

### `SettingsAPI.GetCore`

```go
GetCore(key string) (interface{}, error)
```

GetCore reads the core config table.

<small>`settings.go:29`</small>

### `SettingsAPI.GetPlugin`

```go
GetPlugin(plugin, key string) (interface{}, error)
```

GetPlugin reads another plugin's config table.

<small>`settings.go:43`</small>

### `SettingsAPI.GetSecret`

```go
GetSecret(key string) (string, error)
```

GetSecret reads a value written by SetSecret (transparently decrypted).

<small>`settings.go:23`</small>

### `SettingsAPI.List`

```go
List(prefix string) ([]string, error)
```

List returns all keys matching the given prefix.

<small>`settings.go:26`</small>

### `SettingsAPI.ListCore`

```go
ListCore(prefix string) ([]string, error)
```

ListCore lists core config keys matching the prefix.

<small>`settings.go:35`</small>

### `SettingsAPI.ListPlugin`

```go
ListPlugin(plugin, prefix string) ([]string, error)
```

ListPlugin lists another plugin's config keys matching the prefix.

<small>`settings.go:49`</small>

### `SettingsAPI.Plugins`

```go
Plugins() []string
```

Plugins returns a list of all plugin config namespaces.

<small>`settings.go:61`</small>

### `SettingsAPI.RegisterDef`

```go
RegisterDef(def ConfigDef)
```

RegisterDef registers a config definition for UI display.

<small>`settings.go:52`</small>

### `SettingsAPI.Set`

```go
Set(key string, value interface{}) error
```

Set writes a config value to the plugin's own config table.

<small>`settings.go:8`</small>

### `SettingsAPI.SetCore`

```go
SetCore(key string, value interface{}) error
```

SetCore writes to the core config table.

<small>`settings.go:32`</small>

### `SettingsAPI.SetPlugin`

```go
SetPlugin(plugin, key string, value interface{}) error
```

SetPlugin writes to another plugin's config table.

<small>`settings.go:46`</small>

### `SettingsAPI.SetSecret`

```go
SetSecret(key string, value interface{}) error
```

SetSecret writes a value that is ALWAYS encrypted at rest, regardless of
the key name. Use it when the key name is neutral (e.g. "blob", "conn")
and so cannot be detected as sensitive automatically.

Returns an error (not a silent plaintext write) when no master key is
installed — a plugin that believes it stored a secret must not end up
with plaintext on disk.

The companion read is GetSecret. ConfigDef.Secret/Type=="password" and
sensitive key names are encrypted automatically and do NOT need this.

<small>`settings.go:20`</small>

### `ConfigDef`

```go
type ConfigDef struct { Key string `json:"key"` Default interface{} `json:"default,omitempty"` Type string `json:"type"` DisplayName string …
```

ConfigDef describes a configuration field for the WebUI.

<small>`settings.go:65`</small>

### `PluginSDK.Settings`

```go
func (s *PluginSDK) Settings() SettingsAPI
```

Settings returns the settings API for reading/writing plugin configuration.
sett 在 New 时一次性写入且无 setter，故不需要加锁。

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`feiyu_voice`](../examples/index.md#feiyu_voice) | `example/feiyu_voice/plugin.go:65` | `s.Settings().RegisterDef(sdk.ConfigDef{Key: "base_url", Default: "http://127.0.0.1:8765", Type: "string",` |
| [`rtvoice`](../examples/index.md#rtvoice) | `example/rtvoice/plugin.go:256` | `s.Settings().RegisterDef(sdk.ConfigDef{` |

<small>`plugin.go:685`</small>

