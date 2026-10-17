package main

// 通用小工具：时间、数值参数、HTTP 客户端。
//
// 这些原本散在 platform.go 里（那个文件是 git 平台专有的，已随平台逻辑一并
// 移出插件）。但它们本身与 git 无关 —— criteria / pi_tools / encstore 都在用，
// 所以单独留在这里。**新增平台专属代码不要再往本文件加。**

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

func nowRFC3339() string { return time.Now().Format(time.RFC3339) }

// numArg 取数值参数（JSON 数字进来是 float64）。
func numArg(args map[string]interface{}, key string) float64 {
	switch v := args[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	}
	return 0
}

// httpClient 是共享客户端（超时必须有：对端不可达时不能挂住工具调用）。
var httpClient = &http.Client{Timeout: 30 * time.Second}

// httpGetJSON 发一个 GET 请求，返回 body 与状态码。
//
// 不带认证头：本插件已不持有任何平台凭据。需要认证的调用应当
// 用 **safeshell** 执行 curl（或 CLI），凭据由内核变量库按 @{名字} 填充 ——
// 那是「凭据只保存不读取」这条边界要求的方式。
func httpGetJSON(url string, headers map[string]string) (string, int, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "commondev/0.1")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), resp.StatusCode, nil
}

// stripTokenFromURL 把 URL 里的凭据抹掉，用于日志/回显。
func stripTokenFromURL(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if at := strings.Index(rest, "@"); at >= 0 {
			return u[:i+3] + "***@" + rest[at+1:]
		}
	}
	return u
}
