# 联网检索插件（HologramAgent）

给 agent 补上**真正的信息检索**能力：检索交给本地 SearXNG（多引擎聚合、结构化 JSON），
并补上「读完前 K 篇再回答」的深检索。

## 为什么需要它（背景）

agent 原本只有 `browser_*` 那套浏览器工具，联网检索实际只有 `browser_search` 一个入口，而它是
**「抓 Bing HTML + 正则解析」**：

| 缺陷 | 实测结果 |
|---|---|
| 标题取的是结果块里**第一个 `<a>`** | 拿到的是 Bing 的「来源行」而非标题 → `deepin.orghttps://www.deepin.org` |
| 摘要正则 `<div class="b_caption">.*?<p>` | 对现代 Bing **命中 0/10**（摘要已迁到 `p.b_lineclamp*`）→ 结果**完全没有摘要** |
| 用 `www.bing.com` | 程序化请求直接 302；`cn.bing.com` 才返回 10 个结果块 |
| 单引擎、无兜底、无去重、无站点读取 | 模型只能反复换词重搜（日志里 8 秒 6 连击） |

结果就是日志里那句用户反馈：**「你的搜索能力好像不太行啊」**。

## 依赖：本地 SearXNG（由本插件托管）

插件会**自己管后端**：

- **启动时**：探 `healthz`；已在跑就**直接接管**（不重启），没跑就 `docker compose up -d` 并等就绪（上限 6s）
- **停止时**：跑 `docker compose stop -t 2` 关闭它

配置项 `manage_searxng`（默认 true）与 `searxng_dir`（默认 `/root/searxng-agent`）控制这套行为；
`stop_searxng_on_exit`（默认 true）设 false 可让后端在插件停止后继续跑（**插件重载频繁时建议设 false**，
否则每次重载都会把后端重启一遍）。

### 生命周期契约（依据内核源码，非猜测）

| 环节 | 内核行为 |
|---|---|
| 停止插件 | 发 `plugin.stop` → 插件先跑 **RunStopHandlers（LIFO、幂等）** → 再 `Stop()` → `exit(0)` |
| 宽限期 | **5 秒**；未退出则直接 SIGKILL —— 所以关闭动作限时 4s（`searxShutdownBudget`） |
| stdin 关闭 | 同样会跑 handlers + `Stop()` |
| 崩溃/被 kill | 关闭动作不会执行，后端会留在运行态；下次启动探测到就直接接管（**更安全的失败方向**） |
| 自动重启 | `SetAutoRestart(true)` 由注入的 runtime 在 `plugin.start` 后经 `lifecycle.autoRestart` **显式上报**内核 |

### SearXNG 侧配置

部署在 **.60**，`127.0.0.1:8888`：

```
/root/searxng-agent/docker-compose.yml    # host 网络（要访问宿主 clash）
/root/searxng-agent/settings.yml         # json 输出 + limiter 关闭 + 出站走 clash
```

两个必须知道的坑：

1. **`search.formats` 必须含 `json`**，否则 `/search?format=json` 返回 **403**（看起来像网络问题，其实是配置）。
2. 该镜像默认 `GRANIAN_PORT=8080`，而 granian 的 `GRANIAN_*` **优先级高于 settings.yml**：
   .60 上 8080 被 hologramagent 占用 → 不改 `SEARXNG_PORT` 就是无休止的 `Address already in use` 崩溃循环。

实测可用的引擎（2026-09-12）：`duckduckgo`、`brave`、`google cse`；`quark` 时好时坏；
`baidu`/`google` 经代理出口触发 CAPTCHA，`sogou` 崩溃，`wikidata` 报 HTTP error（已关）。

## 工具

| 工具 | 说明 |
|---|---|
| `deepsearch_search` | 联网检索（首选）：标题 + URL + 摘要 + 发布时间，支持 `engines`/`category`/`time_range`/`language`，自动按 URL 去重并按分数排序；会回报**引擎覆盖度与无响应引擎** |
| `deepsearch_news` | 新闻检索：`news` 类别 + 默认最近一周；新闻为空时自动回退 general + 时间范围 |
| `deepsearch_fetch` | 抓单个网页并抽正文（去脚本/样式/导航），返回标题 + 纯文本，可设截断长度 |
| `deepsearch_deep` | **深检索**：检索 → 并行抓前 K 篇正文 → 一次返回「候选清单 + 证据正文」；单篇失败不影响整体 |
| `deepsearch_status` | 自检：healthz、json 是否可用、延迟、**哪些引擎真的在返回结果**（检索出问题先跑这个） |

## 配置项

| 键 | 默认 | 说明 |
|---|---|---|
| `searxng_url` | `http://127.0.0.1:8888` | 本地 SearXNG 地址 |
| `max_results` | `8` | 默认条数（控制上下文体积） |
| `language` | `zh-CN` | 检索语言 |
| `safesearch` | `0` | 0 关 / 1 中 / 2 严 |
| `request_timeout` | `20` | 单次请求超时（秒） |
| `fetch_max_chars` | `4000` | `deepsearch_fetch` 正文上限 |
| `proxy` | 空 | 仅作用于本插件直连抓取（搜索出网由 SearXNG 侧负责） |
| `user_agent` | Chrome UA | 抓取用 |

每次调用前重读配置，改完即时生效。

## 开发与验证

```bash
go test -count=1 -race ./...       # 11 项测试（httptest 打桩 SearXNG）

# 真实后端联调（默认跳过）：跑的就是当初失败的那条查询
DEEPSEARCH_LIVE_SEARXNG=http://127.0.0.1:8888 go test -run TestLiveSearxng -v ./...

hmapdev build                      # 产出 dist/deep_search_bundle.hmap
```

## 已知边界

- **知乎等站点对直连抓取返回 403**（反爬），`deepsearch_deep` 会如实标注该篇抓取失败并继续；
  这类页面请改用浏览器工具（`browser_navigate` + `browser_render`）。
- 引擎可用性随出口 IP 与目标站点风控变化；`deepsearch_status` 与每次结果里的「覆盖度」行就是给这个用的。
- 未做正文去重/相似度合并：同一事件的多篇转载会各占一条（摘要已能区分）。
