<!-- 本页由 tools/apidoc/gensite 从源码生成，请勿手改；要改文档就改 sdk/*.go 的注释。 -->

# 记忆（Memory）

三层记忆的读写接口：**图记忆**（三元组关系）、**文档记忆**（带元数据的文档）、**文本记忆**（事件流水）。以及知识库。

## `DocMemoryAPI`

DocMemoryAPI provides access to the document vector store.

| 方法 | 说明 |
|---|---|
| [`Insert`](#docmemoryapiinsert) |  |
| [`InsertWithMedia`](#docmemoryapiinsertwithmedia) | InsertWithMedia 写入文档并关联媒体。attachments 里带 Data 的会落进 |
| [`Query`](#docmemoryapiquery) |  |
| [`Remove`](#docmemoryapiremove) |  |
| [`Stats`](#docmemoryapistats) |  |

### `DocMemoryAPI.Insert`

```go
Insert(doc *Doc) error
```

<small>`memory.go:76`</small>

### `DocMemoryAPI.InsertWithMedia`

```go
InsertWithMedia(doc *Doc, attachments []MediaAttachment) error
```

InsertWithMedia 写入文档并关联媒体。attachments 里带 Data 的会落进
内容寻址存储（相同字节只存一份），只带 Digest 的直接引用已有内容。
媒体成为文档直接持有的一等记忆块：文档向量会融合它们的原生向量，
因此图片按自己的向量被召回，不依赖任何生成的描述文本。

<small>`memory.go:81`</small>

### `DocMemoryAPI.Query`

```go
Query(text string, topK int) []*Doc
```

<small>`memory.go:75`</small>

### `DocMemoryAPI.Remove`

```go
Remove(id string)
```

<small>`memory.go:82`</small>

### `DocMemoryAPI.Stats`

```go
Stats() map[string]interface{}
```

<small>`memory.go:83`</small>

## `KnowledgeAPI`

KnowledgeAPI provides access to the knowledge store.

| 方法 | 说明 |
|---|---|
| [`Add`](#knowledgeapiadd) |  |
| [`List`](#knowledgeapilist) |  |
| [`Search`](#knowledgeapisearch) |  |

### `KnowledgeAPI.Add`

```go
Add(name, content string) error
```

<small>`knowledge.go:6`</small>

### `KnowledgeAPI.List`

```go
List() ([]string, error)
```

<small>`knowledge.go:7`</small>

### `KnowledgeAPI.Search`

```go
Search(query string, topK int) ([]*Knowledge, error)
```

<small>`knowledge.go:5`</small>

## `MemoryAPI`

MemoryAPI provides access to the graph memory (entity-relation store).

| 方法 | 说明 |
|---|---|
| [`Commit`](#memoryapicommit) |  |
| [`Introspect`](#memoryapiintrospect) |  |
| [`MergeEntities`](#memoryapimergeentities) |  |
| [`Purge`](#memoryapipurge) |  |
| [`Recall`](#memoryapirecall) |  |

### `MemoryAPI.Commit`

```go
Commit(triples []Triple) error
```

<small>`memory.go:6`</small>

### `MemoryAPI.Introspect`

```go
Introspect() (map[string]interface{}, error)
```

<small>`memory.go:7`</small>

### `MemoryAPI.MergeEntities`

```go
MergeEntities(source, target string) (int, error)
```

<small>`memory.go:8`</small>

### `MemoryAPI.Purge`

```go
Purge(criteria map[string]string, mode string) (int, error)
```

<small>`memory.go:9`</small>

### `MemoryAPI.Recall`

```go
Recall(query []string, depth int) ([]Entity, []Relation, error)
```

<small>`memory.go:5`</small>

## `SocialAPI`

SocialAPI provides read-only access to the social graph (person profiles and relationships).
External plugins can query person traits and social networks but cannot modify them.

| 方法 | 说明 |
|---|---|
| [`GetNetwork`](#socialapigetnetwork) |  |
| [`GetPerson`](#socialapigetperson) |  |
| [`GetRelations`](#socialapigetrelations) |  |
| [`GetTrait`](#socialapigettrait) |  |
| [`ListPersons`](#socialapilistpersons) |  |

### `SocialAPI.GetNetwork`

```go
GetNetwork(name string, depth int) ([]*PersonProfile, error)
```

<small>`memory.go:104`</small>

### `SocialAPI.GetPerson`

```go
GetPerson(name string) (*PersonProfile, error)
```

<small>`memory.go:101`</small>

### `SocialAPI.GetRelations`

```go
GetRelations(name string) ([]SocialRelation, error)
```

<small>`memory.go:103`</small>

### `SocialAPI.GetTrait`

```go
GetTrait(name, trait string) (string, bool)
```

<small>`memory.go:102`</small>

### `SocialAPI.ListPersons`

```go
ListPersons() ([]string, error)
```

<small>`memory.go:105`</small>

## `TextMemoryAPI`

TextMemoryAPI provides access to chronological text event storage.

| 方法 | 说明 |
|---|---|
| [`Append`](#textmemoryapiappend) |  |

### `TextMemoryAPI.Append`

```go
Append(evt TextEvent) error
```

<small>`memory.go:44`</small>

### `Doc`

```go
type Doc struct { ID string `json:"id"` Title string `json:"title"` Content string `json:"content"` Score …
```

Doc represents a document in the document store.

MediaDigests / Attachments 在 Query 返回时由内核填充（仅元数据，不带字节）。

<small>`memory.go:89`</small>

### `PluginSDK.DocMemory`

```go
func (s *PluginSDK) DocMemory() DocMemoryAPI
```

DocMemory returns the document memory API (may be nil if not available).

<small>`plugin.go:702`</small>

### `Entity`

```go
type Entity struct { Name string `json:"name"` Type string `json:"type"` MentionCount int `json:"mention_count"` }
```

Entity represents a named entity in the knowledge graph.

<small>`memory.go:13`</small>

### `Knowledge`

```go
type Knowledge struct { Name string `json:"name"` // Category 是该条目的父分类路径（如 "tech/go"），根下条目为空。 // // 为何加这个字段：对�� …
```

Knowledge represents a knowledge entry.

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`recoverydiag`](../examples/index.md#recoverydiag) | `example/recoverydiag/plugin.go:978` | `if p.sdk != nil && p.sdk.Knowledge() != nil {` |

<small>`knowledge.go:11`</small>

### `PluginSDK.Knowledge`

```go
func (s *PluginSDK) Knowledge() KnowledgeAPI
```

Knowledge returns the knowledge store API (may be nil if not available).

**示例插件里的真实用法**

| 插件 | 位置 | 代码 |
|---|---|---|
| [`recoverydiag`](../examples/index.md#recoverydiag) | `example/recoverydiag/plugin.go:978` | `if p.sdk != nil && p.sdk.Knowledge() != nil {` |

<small>`plugin.go:709`</small>

### `MediaAttachment`

```go
type MediaAttachment struct { Digest string `json:"digest,omitempty"` MIME string `json:"mime,omitempty"` Data []byte `json:"data,omitempty"` Name string `json:"name,omite …
```

TextEvent represents a single text memory event.
MediaAttachment 描述一份与记忆关联的媒体。

两个方向共用一个类型：
  - 写入（InsertWithMedia）：给 Data + MIME 就是新内容；只给 Digest 则是引用已有内容。
  - 读出（Query）：内核只填 Digest/MIME，**不回 Data**——
    一次检索可能命中几十张图，把字节全塞回插件会把 ABI 消息撑爆。
    需要字节时拿 Digest 单独取。

刻意没有 Description 字段：媒体不作为文本被索引，也不带任何生成的描述。
它只按自己的原生向量被检索与召回；附加文字请写在文档 / 三元组的文本里。

<small>`memory.go:58`</small>

### `PluginSDK.Memory`

```go
func (s *PluginSDK) Memory() MemoryAPI
```

Memory returns the graph memory API (may be nil if not available).

<small>`plugin.go:688`</small>

### `PersonProfile`

```go
type PersonProfile struct { Name string `json:"name"` Traits map[string]string `json:"traits,omitempty"` Relations []SocialRelation `json:"relations,omitemp …
```

PersonProfile represents a person's complete profile (traits + social relations).

<small>`memory.go:109`</small>

### `Relation`

```go
type Relation struct { SourceName string `json:"source_name"` TargetName string `json:"target_name"` RelationType string `json:"relation_type"` Confidence float6 …
```

Relation represents a relationship between two entities.

<small>`memory.go:20`</small>

### `PluginSDK.Social`

```go
func (s *PluginSDK) Social() SocialAPI
```

<small>`plugin.go:743`</small>

### `SocialRelation`

```go
type SocialRelation struct { Person string `json:"person"` Relation string `json:"relation"` }
```

SocialRelation represents a social relationship between two persons.

<small>`memory.go:116`</small>

### `TextEvent`

```go
type TextEvent struct { Role string `json:"role"` Content string `json:"content"` Timestamp int64 `json:"timestamp"` Channel …
```

<small>`memory.go:65`</small>

### `PluginSDK.TextMemory`

```go
func (s *PluginSDK) TextMemory() TextMemoryAPI
```

TextMemory returns the text memory API (may be nil if not available).

<small>`plugin.go:695`</small>

### `Triple`

```go
type Triple struct { Subject string `json:"subject"` Relation string `json:"relation"` Object string `json:"object"` Confidence float64 `json:"c …
```

Triple represents a subject-relation-object triple for the knowledge graph.

SentenceText 是这条三元组的原句，会写进 sentences 表；媒体引用挂在句子上，
所以 MediaDigests 非空时内核会保证句子存在（不给就自动合成一句）。

<small>`memory.go:31`</small>

