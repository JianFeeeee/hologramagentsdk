# Vikunja 插件（HologramAgent）

把 [Vikunja](https://vikunja.io) 待办/任务管理接入 HologramAgent：用自然语言查任务、建任务、改期、完成、看板拖动、指派、评论、时间跟踪、导入数据等。

## 配置项（全部可在插件配置界面修改）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `url` | string | `https://vikunja.jianfgit.xyz` | 站点根地址，**不带** `/api` |
| `token` | password(secret) | 空 | **必填**。Vikunja → 设置 → API Tokens 生成（`tk_` 开头）。令牌的权限范围决定本插件能力上限：勾全范围即为完整能力 |
| `api_version` | select | `v2` | `v2`（推荐，标准 REST，含时间跟踪等新能力）或 `v1`（用于 v2 暂未提供的端点） |
| `default_project_id` | string | 空 | 新建任务未指定项目时落到这里；留空则必须显式指定 |
| `max_items` | int | `25` | 列表类工具的默认条数，控制上下文体积 |
| `compact_output` | bool | `true` | 任务/项目/标签列表只返回关键字段；关闭则返回 Vikunja 完整对象 |
| `timeout_seconds` | int | `20` | 单次 HTTP 超时 |
| `verify_tls` | bool | `true` | 自签证书站点可关闭（不建议） |

配置在**每次工具调用前重新读取**，因此换了 token 不必重启插件。

## 工具

| 工具 | 能力 |
|---|---|
| `vikunja_status` | 连接/配置自检：地址、token 对应的用户、API 版本、服务器能力、CalDAV 地址 |
| `vikunja_tasks` | 列任务：按项目、完成状态、截止（today/this_week/overdue/no_due）、关键词、原生 filter 表达式 |
| `vikunja_task_get` / `task_create` / `task_update` / `task_done` / `task_delete` | 任务增删改查（`task_update` 只传要改的字段） |
| `vikunja_task_bulk` | 批量改完成状态/项目/优先级/截止/标签 |
| `vikunja_task_assignees` / `task_labels` / `task_comments` / `task_relations` / `task_attachments` | 指派、标签、评论、关联（子任务/依赖/相关）、附件（支持上传本地文件） |
| `vikunja_projects` / `project_views` | 项目增删改查、归档；视图与看板桶（把任务移入桶＝看板拖动） |
| `vikunja_labels` / `filters` | 标签、保存的筛选器（Saved Filter） |
| `vikunja_teams` / `sharing` | 团队与成员；项目分享（用户/团队授权、链接分享含密码） |
| `vikunja_notifications` / `subscriptions` / `webhooks` | 通知、订阅、Webhook 管理 |
| `vikunja_time_entries` | 时间跟踪（**仅 v2**）：补录/修改/删除、开始与停止计时器 |
| `vikunja_migrate` | 从 TickTick/WeKan/CSV/Planka/Vikunja 文件（v2）与 Todoist/Trello/微软待办（v1）导入 |
| `vikunja_user` / `vikunja_admin` | 当前账号（设置、登录会话、API Token）与实例管理（用户增删/提权/停用/改密、项目归属转移，需实例管理员） |
| `vikunja_reactions` | 任务/评论的表情回应 |
| `vikunja_api` | **通用直通**：调任意端点，未封装的能力走这里（可强制指定 v1/v2），保证能力无死角 |

## v1 / v2 差异（已按实例自带规范逐条核对）

插件默认 v2，并自动处理下列差异：

| 操作 | v1 | v2 |
|---|---|---|
| 建任务 | `PUT /projects/{id}/tasks` | `POST /projects/{id}/tasks` |
| 改任务 | `POST /tasks/{id}`（必须整对象 → 插件自动取回-合并-提交） | `PATCH /tasks/{id}`（merge-patch，只发变更字段；被拒则回落取回-合并-PUT） |
| 搜索参数 | `?s=` | `?q=` |
| 加标签 | `PUT`（Label 对象） | `POST`（`{"label_id":N}`） |
| 批量改 | `POST /tasks/bulk` | `PUT /tasks/bulk` |
| 时间跟踪 | 不支持 | `/time-entries`（`end_time` 为 null 即计时中；停止用 `/time-entries/timer/stop`） |
| 导入 | Todoist / Trello / 微软待办 | TickTick / WeKan / CSV / Planka / Vikunja 文件 |

> 官方路线：v1 仍支持但新端点只进 v2，3.0 弃用、4.0 移除。除“导入”外建议一律用 v2。

## 开发与构建

```bash
cd third_party/homeagent-sdk/example/vikunja

go test -count=1 -race ./...   # 16 项测试（httptest 打桩，不需要真 token）
hmapdev build                  # 产出 dist/vikunja_bundle.hmap
```

`go.mod` 里的 `replace` 把 SDK 指向仓库内的 `third_party/homeagent-sdk`，因此无需联网拉私有模块。

### 部署到运行实例

`.hmap` 包内是 `plugin.json` + `plugin.bin.<os>.<arch>`，安装时按运行平台重命名入口文件：

```bash
unzip -o dist/vikunja_bundle.hmap -d /home/newqqagent/plugins/vikunja
cd /home/newqqagent/plugins/vikunja && mv plugin.bin.linux.amd64 plugin.bin
# 然后重载插件（或重启 hologramagent.service）
```

## 已知边界

- **附件下载**未单独封装：`task_attachments` 支持列出/上传/删除，下载请用 `vikunja_api` 访问附件 URL。
- **链接分享的字段**（`right`/`password`）按 Vikunja 版本语义透传；如遇 4xx，可直接用 `raw` 参数传完整 JSON。
- **批量改标签**的 `fields` 结构以 `BulkTask` 为准，未在真实实例上验证过（缺少可用 token），如有偏差请用 `vikunja_api` 直通。
- CalDAV 是客户端协议，插件只提供地址（`vikunja_status` 里的 `caldav_url`），不做 CalDAV 同步。
