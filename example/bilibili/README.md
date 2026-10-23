# bilibili · B站内容侧能力

给 agent 补上**B站内容**这一面：视频信息与分P、字幕、评论、热门流，
外加一个**纯本地**的 BV 号查重台账。

> **与 [`example/bili`](../bili) 的区别**：`bili` 做的是「下载」——调 yt-dlp 把视频
> 落到磁盘。本插件不下载、不依赖任何外部程序，只读内容与元信息。两者是独立的
> 示例，插件名不同（`bili` / `bilibili`），可以同时安装。

## 工具

| 工具 | 说明 | 网络 | 记忆 |
|---|---|---|---|
| `bilibili_video` | 视频主体信息 + 分P清单（标题、UP主、时长、发布时间、播放/点赞/评论等统计、每个分P的 `cid`）。`cid` 是字幕工具的必填入参 | 1 次 GET | 参与 |
| `bilibili_subtitles` | 列出某分P的字幕轨（语言、是否 AI 生成）；带 `lang` 时把该语言正文导出成本地 JSON | 1~3 次（含 wbi 签名） | NoMemory |
| `bilibili_comments` | 按页拉评论（含楼中楼第一层回复），可按热度/时间排序 | 1 次/页 | NoMemory |
| `bilibili_popular` | 综合热门流（当前榜），可翻页 | 1 次/页 | NoMemory |
| `bilibili_seen_check` | **纯本地**：查该视频是否在台账里，命中给出首次/最近见到时间与命中次数 | **不发请求** | NoMemory / ParallelSafe |
| `bilibili_seen_list` | **纯本地**：列出最近见过的视频 | **不发请求** | NoMemory / ParallelSafe |
| `bilibili_seen_forget` | **纯本地写**：从台账删掉一条 | 不发请求 | NoMemory |
| `bilibili_comment_post` | **写操作，默认关闭**：发评论 / 回复评论 | 1 次 POST | Serial |

### 参数

所有视频类工具的第一个参数都叫 `video`，接受三种写法（`parseRef` 统一归一）：

| 写法 | 例子 |
|---|---|
| BV 号 | `BV17x411w7KC`（前缀大小写不敏感，**编号部分大小写敏感**） |
| av 号 | `av170001` |
| 完整链接 | `https://www.bilibili.com/video/BV17x411w7KC?p=2&t=10`（参数会被正确剥离） |

`bilibili_subtitles`：

| 参数 | 必需 | 说明 |
|---|---|---|
| `video` | ✓ | 同上 |
| `cid` | | 分P的 cid。省略则自动取第 1 个分P（会多一次 `view` 请求） |
| `lang` | | 要导出的语言代码（`zh-CN` / `ai-zh` …）。省略则只列清单。支持前缀匹配（`zh` 能命中 `zh-CN`） |
| `save_dir` | | 导出目录。相对路径落到输出目录下，`..` 越界与系统目录都会被拒 |

`bilibili_comments`：

| 参数 | 默认 | 说明 |
|---|---|---|
| `video` | — | 同上（接口只要 aid，插件内部把 BV 换算成 aid） |
| `sort` | `hot` | `hot` 按热度 / `time` 按时间 |
| `pages` | 取配置 `max_pages` | 翻几页，**硬上限 10** |
| `per_page` | 取配置 `max_items` | 每页条数，**硬上限 100** |

`bilibili_popular`：`pages`（默认 1）、`per_page`（默认取配置）。

`bilibili_seen_list`：`limit`（默认 20，上限 200）。`bilibili_seen_forget`：`video`。

`bilibili_comment_post`：

| 参数 | 说明 |
|---|---|
| `video` | 同上 |
| `message` | 正文，1~1000 字（超限在本地就报错，不让平台回一句含糊的「参数错误」） |
| `root` | 回复某条主评论时填它的 `rpid` |
| `parent` | 回复楼中楼时填目标 `rpid`；省略则取 `root`（平台的楼中楼约定） |

## 配置项

| 键 | 默认 | 说明 |
|---|---|---|
| `output_dir` | 空（= 插件数据目录下的 `exports/`） | 字幕导出的落盘目录。**必须是绝对路径，且不能是系统目录**（见下）。查重台账不受此项影响：它始终是插件数据目录下的 `records.json` |
| `api_base` | `https://api.bilibili.com` | 只有自建反代时才需要改 |
| `cookie` | 空 | 登录态 Cookie 串（`SESSDATA=…; bili_jct=…`）。以 `password` 类型存储 |
| `allow_comment_post` | `false` | **发评论总开关**。默认关闭，见下节 |
| `max_items` | 20 | 单页条数上限（硬上限 100） |
| `max_pages` | 3 | 翻页上限（硬上限 10） |
| `page_delay_ms` | 150 | 翻页间隔（毫秒）。B 站在几十毫秒内连发多个请求时容易吃 -412，这个间隔对使用者几乎无感。`0` = 不等（不建议） |
| `request_timeout` | 15 | 单次请求超时（秒） |
| `user_agent` | 浏览器 UA | 不带浏览器 UA 时部分接口直接 412 |
| `record_on_fetch` | `true` | `bilibili_video` 取回信息时是否自动登记台账 |

### `output_dir` 的安全校验

它是本插件写盘的两处之一（另一处是数据目录下的 `records.json`，由内核给定、
不可配），而它由用户配置。`validateOutputDir` 会拒绝：

- 空值、相对路径、根目录 `/`
- `/etc`、`/usr`、`/bin`、`/sbin`、`/boot`、`/dev`、`/proc`、`/sys`、`/lib`、`/lib64`、`/var/run`
  **以及它们下面的任何路径**（只比相等是不够的 —— `/etc/bilibili` 同样是系统目录）

启动时就校验：坏配置让插件**启动失败**，用户能在设置页里立刻看到；
而不是等第一次调用才失败（那会被模型理解成「工具坏了」并反复重试）。

工具参数 `save_dir` 走**同一套**判据 —— 参数是模型填的，模型会照抄用户随口说的
`/etc/xxx`，不能留一个更宽的旁路。

### 写操作：发评论默认关闭

`bilibili_comment_post` 需要**两道闸门同时成立**：

1. 配置 `allow_comment_post = true`（默认 `false`）
2. Cookie 里含 `bili_jct`（csrf token）

任一不满足，工具**在本地直接拒绝**并说明缺哪一项，不会向平台发任何请求。
这是刻意的设计：示例不该给使用者一个"一键刷评论"的玩具。

即使两道闸门都开了，该工具仍声明 `Serial: true`（写操作保序），
且不声明 `ParallelSafe`。

## 实现要点

- **零第三方依赖**：只用标准库。`go build` 在干净机器上就能过，使用者拷走即可改。
- **wbi 签名**（`client.go`）：字幕接口 `/x/player/wbi/v2` 不带 `w_rid` 一律 -403。
  签名规则是「参数按字典序 → 值先过滤 `!'()*` 并 URL 编码 → 拼 `mixin_key` → md5」。
  `signParams` 把过滤后的值**写回**参数表，保证「参与签名的串」与「真正发出去的串」
  逐字一致；`TestSignParamsRecomputable` 用产出的 `wts` 复算一遍 `w_rid` 来钉住它。
  未登录时 `/x/web-interface/nav` 返回 `code=-101` 但**响应体里仍带 wbi 密钥**，
  所以匿名也能签名（用 `allowCodes` 放过 -101）。
- **失败要响亮**（`client.go` 的 `decode`/`do`）：B 站有三种失败形态，其中两种 HTTP 状态码是 200：
  1. HTTP 非 200（404 / 412 风控 / 429）
  2. `{"code":-404,"message":"啥都木有"}` —— **`json.Unmarshal` 完全成功**，解出来是零值结构体
  3. 风控页：HTTP 200 但返回 HTML

  只判 `err != nil` 会让第 2 种变成「0 条评论」并被当成功返回，使用者被告知「这个视频没有评论」。
  所以三种都翻成 `*apiError`，并带回 **HTTP 状态码 + 业务码 + 平台原话**。
  热门流「`code=0` 但 `data.list` 为空」同样报错（那几乎总是风控/反代改写）。
- **分页有上限、循环有预算、页间有节流**：`max_pages` 与硬上限 10 取小；翻页循环
  共用一份 `context` 预算（45s），预算耗尽时**已取到的结果照常返回并注明停在哪一页**，
  一页都没取到则报错。页间默认等 `page_delay_ms`（150ms）——节流等待也接 `ctx`，
  预算用尽能立刻打断，不会让"礼貌"变成"卡住"。
  工具层声明 `Timeout: "90s"`（内核默认 60s）：翻满 10 页 + 慢网络会被内核先掐断。
- **BV↔av 换算**（`bvid.go`）：评论接口只吃 `aid`，用户只给 BV 号。51 bit 异或固定盐 +
  base58 变体 + 两次位置交换。`TestBVAIDRoundTrip` 做往返断言。
- **本地查重台账**（`store.go`）：B 站**没有**「这个视频我是不是看过」的接口
  （收藏/历史/稍后再看都要登录态），所以台账只能基于**本插件自己见过的东西**：
  每次成功取回视频信息就落一条。写盘走 `tmp` + `rename`（避免被杀时留下半个 JSON）；
  文件损坏时**报错**而不是静默清空（静默清空会让使用者以为数据"正常地"丢了）。
  同一个视频的 BV 键与 `avN` 键指向同一条记录，`persist` 时去重。
- **声明纪律**：网络请求工具**不声明** `ParallelSafe`（默认串行是安全的；同批并发
  多个请求会同时打同一个平台，正是风控最容易命中的形态，且它们共享 wbi 密钥缓存）。
  只有两个纯本地只读工具声明了 `ParallelSafe`。
- **NoMemory** 给评论/字幕/热门流：这些是大块外部内容，按原文进记忆计算只会挤占
  上下文预算，而"上周某视频第三条评论说了什么"极少需要召回。视频信息保留参与记忆
  —— 标题/UP/时长是用户真正会回头引用的事实。
- **`main.go` + `plugin.go` 分离**：`plugin.go` 无 `func main`（由 `hmapdev` 注入），
  `main.go` 只包一层 `NewPluginFactory`。

## 明确没做

- **不需要登录态的能力，一律没做**：收藏夹、历史记录、稍后再看、投币/点赞、动态、
  关注列表 —— 这些都要登录 + csrf，且属于"写/账号"面，超出本示例的范围。
- **短链不展开**：`b23.tv` / `bili2233.cn` 要跟随 302 才能拿到 BV 号，属隐式请求。
  本插件直接报错并提示贴完整链接。
- **番剧/直播/动态/专栏**：它们的编号体系与接口都不同（`ep`/`room_id`/`dynamic_id`），
  不是本示例的目标。
- **游标翻页**：评论用的是 `/x/v2/reply` 的 `pn` 翻页，而不是 `/x/v2/reply/wbi/main`
  的 `pagination_str` 游标。游标能让「有上限」这件事在代码里看不出来，示例选了更直白的写法。
- **下载**：那是 [`example/bili`](../bili) 的事。

已知的平台侧限制（不是插件缺陷）：

- 匿名访问时平台**通常不下发 AI 字幕**，字幕清单会是空的。工具会明确说明这一点，
  而不是让使用者以为插件漏了功能。
- 评论/热门有频率限制，翻页多了容易吃 -412/-799。降频或填 Cookie 有明显改善。

## 构建

```bash
cd example/bilibili
hmapdev build                      # 或：hmapdev build --no-bundle --target linux/amd64
```

产物：`dist/bilibili_linux_amd64.hmap`（普通模式）或 `dist/bilibili_content_bundle.hmap`（bundle 模式）。

跑测试（**不打真网络**，全部走 `httptest`）：

```bash
go vet ./... && go test ./... -count=1
```

> `plugin.json` 是本插件目录下的**构建产物**（`hmapdev build` 生成，含 `sdk` 版本与
> `platforms` 字段），已由仓库根的 `.gitignore` 排除，不入库 —— 与其它示例一致。
