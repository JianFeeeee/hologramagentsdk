# 受限 SDK 与安全

外部插件与内置插件的区别不只是「能不能调某个函数」，还包含一层**安全边界**：
外部插件的进程不共享内核地址空间，能力通过显式注入点交过去。

## 三层隔离

| 层 | 机制 | 防住了什么 |
|---|---|---|
| **进程** | 插件跑在独立子进程 | 插件 panic / 内存越界**不会带崩内核** |
| **能力** | 只注入显式声明的接口 | 插件拿不到未授权的内核内部结构 |
| **权限** | 公开接口是内部接口的**只读子集** | 插件无法改写他人数据 |

第一种是 v1.0.0 从 C ABI 动态库改为子进程 + 共享内存的直接收益：
在此之前，插件 panic 会带崩 `homed`。

## 受限接口是怎么实现的

**按接口裁剪，而不是按方法裁剪。** 同一个概念在公开包与内部包里是**两个不同的
接口声明**，公开的那个只保留安全子集：

```go
// 公开 SDK：6 个只读方法
type SocialAPI interface {
    GetPerson(name string) (*PersonProfile, error)
    GetTrait(name, trait string) (string, bool)
    GetRelations(name string) ([]SocialRelation, error)
    GetNetwork(name string, depth int) ([]*PersonProfile, error)
    ListPersons() ([]string, error)
}
```

写操作只在内核内部接口里。这样外部插件**在类型层面就调不到**，
不是靠运行时检查拦截。

同理，`EventSubscriber` 公开版**刻意只有 `Subscribe`，没有 `Publish`**：

```go
// 插件可以订阅，但由内核决定投递哪些事件
type EventSubscriber interface {
    Subscribe(eventType EventType, handler EventHandler) func()
}
```

## 进程边界带来的约束

事件订阅是理解这层边界的典型例子。公开包里有一个 `Events() EventSubscriber`，
但**外部插件拿到的恒为 nil** —— 桥接运行时不注入它（`SetEventSubscriber`
在全仓没有调用点）。外部插件的事件订阅由生成的运行时通过 `events.subscribe`
RPC 完成，Lua 插件走内部 SDK 的 `Subscribe`。

这不是缺陷，而是进程边界的结果：跨进程无法共享内核的事件发布通道。
详见[能力边界](capability-boundary.md)。

## 共享内存中的数据面

工具调用帧、Cleaner、输入输出通道、媒体块、文档与知识正文**都走共享内存**，
RPC 只传偏移描述符。因此：

- 大对象不经 JSON 序列化，避免了大 payload 的性能与内存放大；
- StageContext 在同一份状态上读改写，消除了副本模型的 lost update
  （实测由 35.8~36.8% 降到 0）。

`SharedRef`（共享内存描述符）是**内部实现细节**，插件开发者看不到它 ——
公开 SDK 只暴露普通字符串与 map。

## 声明式安全：插件只声明，内核执行

上面讲的是**能力边界**（插件能调什么 API）。还有一层是**策略**：
同一个插件，不同来源的输入该有不同的工具权限（典型例子：QQ 的群主与访客）。

这层**不要在插件里自己判**。历史上 QQ 插件用 stage 管道自己实现了一套
（判身份 → 查白名单 → 查硬私有工具 → 写拒绝消息），后果是：

- 每个插件各写一套，判定必然漂移（QQ 拦了、别的入口没拦）
- 策略与被约束者在同一个进程里 —— 插件崩了或被替换，策略就没了
- 拒绝语义散落（写 denial / 返回 error 各有各的写法）

**正确形态**：插件**声明**档位，内核**执行**判定。

```go
// Start 阶段：声明两个命名档位（只声明，不判定）
if sec := s.Security(); sec != nil {
    err := sec.DeclareProfiles([]sdk.SecurityProfile{
        // 所有者：不额外限制
        {Name: "qq.owner", Owner: "qq"},
        // 访客：禁止开发类与私人资源工具
        {Name: "qq.guest", Owner: "qq",
         DenyTools:      []string{"cmd_*", "files_write", "stablemem_*"},
         SensitiveWords: []string{"内部代号"}},
    })
    if err != nil {
        log.Printf("[qq] declare security profiles failed: %v", err)
    }
}
```

```go
// 注入阶段：按发送者身份选档位
s.InjectInterruptTextOpts(source, channel, text, sdk.InjectOptions{
    SecurityProfile: "qq.guest",   // 由内核在该轮激活并执行
})
```

### 必须知道的四条语义

| 语义 | 说明 |
|---|---|
| **名字必须带自己的前缀** | 档位名形如 `<插件名>.<档位>`。跨插件覆盖被拒绝（否则任何插件都能改写 `qq.owner` = 提权口子） |
| **只能收窄，不能扩大** | 内核判定顺序是「全局 deny → 档位 deny → 主体 deny → 全局 allow」。全局黑名单永远先判，档位绕不过去 |
| **不声明 = 现有行为** | 不调 `DeclareProfiles`、注入时不带 `SecurityProfile`，一切沿用内核默认策略。这是**可选**能力 |
| **拼错档位名会报错，不静默回落** | 注入时带一个未声明的档位名，内核记日志并降级到默认策略。拼错档位名却默默放行，是安全事故的经典形态 |

### 两层档位的生命周期

| 层 | 生效范围 | 谁设置 |
|---|---|---|
| **sessionProfile** | **这一轮**。输入携带它，该轮结束即清除 | 每次注入时带 `InjectOptions.SecurityProfile` |
| **longtimeProfile** | **这个 agent 的一段生命**，跨轮保留 | 内核按 agent 管理（当前未从插件侧暴露设置入口） |

优先级：**session 高于 longtime**。这样访客的一条消息可以在长期为高权限的
agent 上临时收窄；反之若 long-time 优先，访客声明的收窄会被吞掉 = 提权。

### `s.Security()` 可能返回 nil

宿主未接声明面时（旧内核、或独立测试）`s.Security()` 返回 nil。
**不要因此失败启动** —— 保持插件原有行为即可，但建议留一行日志：

```go
if sec == nil {
    log.Printf("[qq] security declaration unavailable; keeping legacy in-plugin checks")
    return
}
```

### 相关文档

- [`api/`](../api/index.md) 的 API 参考（从源码注释生成）
- 内核侧实现：`internal/security/`（档位注册表、主体策略、判定链）

---

## 插件作者的实践建议

- **不要在 `Start` 里长时间阻塞** —— 内核在等待它返回。
- **工具处理器要可并发**：模型可能并发发起多个调用；共享状态用锁保护
  （`example/rtvoice` 用 `sync.RWMutex`；`memo` 等已迁插件见
  [`github.com/HologramAgent/memo`](https://github.com/HologramAgent/memo)）。
- **写文件用原子替换**（临时文件 + rename），避免进程被强杀时截断数据。
- **声明 `NoMemory`**：定时提醒、连接状态这类不是对话内容的东西，
  别让它们污染记忆（`InjectOptions{NoMemory: true}`）。
- **在 `-race` 下测**：插件重载瞬间的并发访问是历史高发缺陷。

## 许可与分发

SDK 是 **MIT**，插件可以**闭源分发**，可商用、可私有，无需回馈。
这是刻意的：SDK 随插件静态链接（源码进入插件二进制），用传染性许可会
强迫插件开源。内核本身是 AGPL-3.0-only，但那是内核的许可，与外部插件无关。
