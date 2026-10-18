package mocksdk

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

var (
	DebugLog = false
	mu       sync.Mutex
)

func logf(format string, args ...interface{}) {
	if DebugLog {
		fmt.Fprintf(os.Stderr, "[mocksdk] "+format+"\n", args...)
	}
}

type Plugin interface {
	Name() string
	Start(sdk *PluginSDK) error
	Stop() error
}

type ToolHandler func(args map[string]interface{}) (interface{}, error)

type StageHandler func(ctx *StageContext) error

type Stage string

const (
	StageOnInput        Stage = "on_input"
	StagePreAction      Stage = "pre_action"
	StagePostAction     Stage = "post_action"
	StageBeforeToolcall Stage = "before_toolcall"
	StageAfterToolcall  Stage = "after_toolcall"
	StageBeforeOutput   Stage = "before_output"
	StageAfterOutput    Stage = "after_output"
)

type StageContext struct {
	mu               sync.RWMutex
	RawMessage       string
	UserID           string
	GroupID          string
	ContextMsgs      []map[string]interface{}
	LLMText          string
	ReasoningContent string
	TokenUsage       map[string]int
	ToolCalls        []ToolCall
	ToolResults      []ToolResult
	FinalText        string
	Response         *string
	Phase            Stage
	Memory           []MemItem
	NoMemory         bool
	Extra            map[string]interface{}
	Errors           []string
}

type MemItem struct {
	Role    string  `json:"role"`
	Content string  `json:"content"`
	Score   float64 `json:"score"`
}

type ToolCall struct {
	ID        string                 `json:"id"`
	Name      string                 `json:"name"`
	Plugin    string                 `json:"plugin,omitempty"`
	Arguments map[string]interface{} `json:"arguments"`
}

type ToolResult struct {
	CallID  string      `json:"call_id"`
	Name    string      `json:"name"`
	Plugin  string      `json:"plugin,omitempty"`
	Success bool        `json:"success"`
	Result  interface{} `json:"result"`
}

type ToolDef struct {
	Name          string                 `json:"name"`
	Plugin        string                 `json:"plugin,omitempty"`
	Description   string                 `json:"description"`
	Parameters    map[string]interface{} `json:"parameters"`
	NoMemory      bool                   `json:"no_memory,omitempty"`
	Cleaner       func(string) string    `json:"-"`
	ContextPolicy string                 `json:"context_policy,omitempty"`
	RecallPolicy  string                 `json:"recall_policy,omitempty"`
	// ParallelSafe / Serial 与公共 SDK 的同名声明项**逐字对齐**。
	//
	// ⚠️ 不要只补主 SDK 就以为完事：mocksdk 是 yaegi 解释执行用的**替身**，
	// 少一个字段，插件作者在本地调试时"写了声明却不报错"，编译安装后才发现
	// 声明没生效 —— 这种不一致极难察觉（黑名单不会自动跟上新执行能力）。
	// TestMockSDKToolDefMatchesSDK 钉住这一致。
	ParallelSafe bool `json:"parallel_safe,omitempty"`
	Serial       bool `json:"serial,omitempty"`

	// Timeout 是工具自声明的超时（如 "5m"）。
	//
	// 与公共 SDK 的 ToolDef.Timeout 逐字对齐：内核按
	// max(声明值, 参数里的 timeout) 计算，并夹到 10 分钟上限。
	// 缺这个字段，本地 yaegi 调试时写 Timeout 不报错，
	// 装到内核后却发现声明被忽略（超时仍是一刀切的默认值）。
	Timeout string `json:"timeout,omitempty"`
}

// 上下文策略取值，与公共 SDK 一致。
const (
	ContextPolicyNone  = "none"
	ContextPolicyPrune = "prune"
)

// InjectOptions 与公共 SDK 同构：声明一次注入是否记入记忆、是否据此裁剪上下文、
// 以及用哪个已注册的通道 cleaner 清洗注入内容。
type InjectOptions struct {
	NoMemory      bool
	ContextPolicy string
	CleanerName   string
}

type IOInjector interface {
	InjectInterruptText(source, channel, text string)
	InjectText(source, channel, text string)
	InjectTextNoMemory(source, channel, text string)
	// InjectInputSync 注入输入事件并同步等待 agent 回复（无回复时返回空串）。
	// 通道类插件（qq / a2a 等）靠它完成「收到入站 → agent 处理 → 回复取回」闭环，
	// 而 mock 此前只有带 flags 的 InjectInputSyncOpts、没有这个零值糖——
	// 于是一个能在 plugin.bin 里编译通过、在 yaegi 下却调不通的方法就长住了。
	InjectInputSync(source, channel, text string) string
	// 1.1.0 媒体注入。与公共 SDK 同构：插件在 yaegi 下调得通的方法，
	// 编成 plugin.bin 后必须也调得通，否则调试期与真实运行行为不一致。
	InjectInputMedia(source, channel, text string, blocks []ContentBlock)
	InjectInputMediaSync(source, channel, text string, blocks []ContentBlock) string
	InjectInterruptMedia(source, channel, text string, blocks []ContentBlock)
	SetToolBlocks(blocks []ContentBlock)

	// 1.2.0 带标志位的注入，与公共 SDK 同构。
	InjectTextOpts(source, channel, text string, opts InjectOptions)
	InjectInterruptTextOpts(source, channel, text string, opts InjectOptions)
	InjectInputSyncOpts(source, channel, text string, opts InjectOptions) string
	InjectInputMediaOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions)
	InjectInputMediaSyncOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions) string
	InjectInterruptMediaOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions)
}

// ContentBlock 与公共 SDK 同构（OpenAI 多模态内容块格式）。
type ContentBlock struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
	AudioURL *AudioURL `json:"audio_url,omitempty"`
}

type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type AudioURL struct {
	URL string `json:"url"`
}

type EventType string

const (
	EventRawInput      EventType = "raw_input"
	EventAgentOutput   EventType = "agent_output"
	EventAgentLLMChain EventType = "agent_llm_chain"
	EventToolCall      EventType = "tool_call"
	EventReasoning     EventType = "reasoning"
	EventStage         EventType = "stage"
	EventSystem        EventType = "system"
)

type Event struct {
	Type      EventType              `json:"type"`
	Source    string                 `json:"source"`
	Payload   map[string]interface{} `json:"payload"`
	Timestamp int64                  `json:"timestamp"`
}

type EventHandler func(evt *Event)

type EventSubscriber interface {
	Subscribe(eventType EventType, handler EventHandler) func()
}

type StageScope int

const (
	StageScopeGlobal   StageScope = 0
	StageScopeOwnTools StageScope = 1
)

type ConfigDef struct {
	Key         string   `json:"key"`
	Default     string   `json:"default"`
	Type        string   `json:"type"`
	DisplayName string   `json:"display_name"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Options     []string `json:"options,omitempty"`
}

type SettingsAPI interface {
	Get(key string) (interface{}, error)
	Set(key string, value interface{}) error
	List(prefix string) ([]string, error)
	GetCore(key string) (interface{}, error)
	SetCore(key string, value interface{}) error
	ListCore(prefix string) ([]string, error)
	GetPlugin(plugin, key string) (interface{}, error)
	SetPlugin(plugin, key string, value interface{}) error
	ListPlugin(plugin, prefix string) ([]string, error)
	RegisterDef(def ConfigDef)
	Defs(prefix string) []*ConfigDef
	Dump() map[string]interface{}
	Plugins() []string
}

type mockSettings struct{ data map[string]interface{} }

func (s *mockSettings) Get(key string) (interface{}, error) {
	v, ok := s.data[key]
	if !ok {
		return nil, nil
	}
	return v, nil
}
func (s *mockSettings) Set(key string, value interface{}) error { s.data[key] = value; return nil }
func (s *mockSettings) SetSecret(key string, value interface{}) error {
	// mock 无加密能力：与真实实现一致地**拒绝**明文落盘。
	if str, ok := value.(string); ok && str != "" {
		return fmt.Errorf("mock: 密钥未装配，拒绝以明文存储敏感项 %q", key)
	}
	return nil
}
func (s *mockSettings) GetSecret(key string) (string, error) { return "", nil }
func (s *mockSettings) List(prefix string) ([]string, error) {
	var ks []string
	for k := range s.data {
		if strings.HasPrefix(k, prefix) {
			ks = append(ks, k)
		}
	}
	return ks, nil
}
func (s *mockSettings) GetCore(key string) (interface{}, error)       { return nil, nil }
func (s *mockSettings) SetCore(key string, value interface{}) error   { return nil }
func (s *mockSettings) ListCore(prefix string) ([]string, error)      { return nil, nil }
func (s *mockSettings) GetPlugin(p, k string) (interface{}, error)    { return nil, nil }
func (s *mockSettings) SetPlugin(p, k string, v interface{}) error    { return nil }
func (s *mockSettings) ListPlugin(p, prefix string) ([]string, error) { return nil, nil }
func (s *mockSettings) RegisterDef(def ConfigDef) {
	logf("config def: %s = %s", def.Key, def.Default)
}
func (s *mockSettings) Defs(prefix string) []*ConfigDef { return nil }
func (s *mockSettings) Dump() map[string]interface{}    { return s.data }
func (s *mockSettings) Plugins() []string               { return nil }

type Entity struct {
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	Properties map[string]string `json:"properties,omitempty"`
}

type Relation struct {
	Subject   string `json:"subject"`
	Predicate string `json:"predicate"`
	Object    string `json:"object"`
}

// Triple 与公共 SDK 同构。
//
// ❗字段名曾是 `Predicate`，而公共 SDK 一直叫 `Relation`。
// yaegi 解释器下插件写 `Relation:` 会报未知字段，写 `Predicate:` 则在
// 编成 plugin.bin 时报错——谁都不对。没人发现是因为没有任何代码
// 对着 mocksdk 编译，漂移不会被编译器抓到。
type Triple struct {
	Subject      string   `json:"subject"`
	Relation     string   `json:"relation"`
	Object       string   `json:"object"`
	Confidence   float64  `json:"confidence,omitempty"`
	SubjectType  string   `json:"subject_type,omitempty"`
	ObjectType   string   `json:"object_type,omitempty"`
	SentenceText string   `json:"sentence_text,omitempty"`
	MediaDigests []string `json:"media_digests,omitempty"`
}

type MemoryAPI interface {
	Recall(q []string, depth int) ([]Entity, []Relation, error)
	Commit(triples []Triple) error
	Introspect() (map[string]interface{}, error)
	MergeEntities(source, target string) (int, error)
	Purge(conditions map[string]string, mode string) (int, error)
}

type mockMemory struct{}

func (mockMemory) Recall(q []string, d int) ([]Entity, []Relation, error) { return nil, nil, nil }
func (mockMemory) Commit(t []Triple) error                                { return nil }
func (mockMemory) Introspect() (map[string]interface{}, error)            { return map[string]interface{}{}, nil }
func (mockMemory) MergeEntities(s, t string) (int, error)                 { return 0, nil }
func (mockMemory) Purge(c map[string]string, m string) (int, error)       { return 0, nil }

type Doc struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Content string `json:"content"`
	Source  string `json:"source"`
	// 1.1.0：媒体字段。与公共 SDK 保持同构，否则插件在 yaegi 下跑得通、
	// 编成 plugin.bin 却编不过（或反之）。
	MediaDigests []string          `json:"media_digests,omitempty"`
	Attachments  []MediaAttachment `json:"attachments,omitempty"`
}

// MediaAttachment 与公共 SDK 同构：写入时给 Data+MIME，引用已有内容时只给 Digest。
type MediaAttachment struct {
	Digest      string `json:"digest,omitempty"`
	MIME        string `json:"mime,omitempty"`
	Data        []byte `json:"data,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

type DocMemoryAPI interface {
	Query(text string, topK int) []*Doc
	Insert(doc *Doc) error
	InsertWithMedia(doc *Doc, attachments []MediaAttachment) error
	Remove(id string)
	Stats() map[string]interface{}
}

type mockDocMemory struct{}

func (mockDocMemory) Query(t string, k int) []*Doc { return nil }
func (mockDocMemory) Insert(doc *Doc) error        { return nil }
func (mockDocMemory) InsertWithMedia(doc *Doc, atts []MediaAttachment) error {
	logf("doc_insert_with_media: %d 份附件", len(atts))
	return nil
}
func (mockDocMemory) Remove(id string)              {}
func (mockDocMemory) Stats() map[string]interface{} { return nil }

type TextEvent struct {
	Timestamp int64  `json:"timestamp"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	Source    string `json:"source"`
	// 1.1.0：附件。读回时内核从正文标记反解，写入时内核把标记并进正文。
	Attachments []MediaAttachment `json:"attachments,omitempty"`
}

type TextMemoryAPI interface {
	Append(evt TextEvent) error
}

type mockTextMemory struct{}

func (mockTextMemory) Append(evt TextEvent) error { return nil }

type Knowledge struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type KnowledgeAPI interface {
	Search(query string, topK int) ([]*Knowledge, error)
	Add(name, content string) error
	List() ([]string, error)
}

type mockKnowledge struct{}

func (mockKnowledge) Search(q string, k int) ([]*Knowledge, error) { return nil, nil }
func (mockKnowledge) Add(n, c string) error                        { return nil }
func (mockKnowledge) List() ([]string, error)                      { return nil, nil }

type PersonProfile struct {
	Name   string            `json:"name"`
	Traits map[string]string `json:"traits"`
}

type SocialRelation struct {
	Target   string `json:"target"`
	Relation string `json:"relation"`
}

type SocialAPI interface {
	GetPerson(name string) (*PersonProfile, error)
	GetTrait(name, trait string) (string, bool)
	GetRelations(name string) ([]SocialRelation, error)
	GetNetwork(name string, depth int) ([]*PersonProfile, error)
	ListPersons() ([]string, error)
}

type mockSocial struct{}

func (mockSocial) GetPerson(n string) (*PersonProfile, error)           { return nil, nil }
func (mockSocial) GetTrait(n, t string) (string, bool)                  { return "", false }
func (mockSocial) GetRelations(name string) ([]SocialRelation, error)   { return nil, nil }
func (mockSocial) GetNetwork(n string, d int) ([]*PersonProfile, error) { return nil, nil }
func (mockSocial) ListPersons() ([]string, error)                       { return nil, nil }

type LLMAPI interface {
	ListSources() []string
	SetSource(name string) error
	CurrentSource() string
}

type mockLLM struct{}

func (mockLLM) ListSources() []string    { return nil }
func (mockLLM) SetSource(n string) error { return nil }
func (mockLLM) CurrentSource() string    { return "" }

type IOInjectorImpl struct{}

func (IOInjectorImpl) InjectInterruptText(source, channel, text string) {
	logf("inject_interrupt: source=%s channel=%s", source, channel)
}
func (IOInjectorImpl) InjectText(source, channel, text string) {
	logf("inject_text: source=%s channel=%s", source, channel)
}
func (IOInjectorImpl) InjectTextNoMemory(source, channel, text string) {
	logf("inject_text_no_memory: source=%s channel=%s", source, channel)
}
func (IOInjectorImpl) InjectInputMedia(source, channel, text string, blocks []ContentBlock) {
	logf("inject_input_media: source=%s channel=%s blocks=%d", source, channel, len(blocks))
}
func (IOInjectorImpl) InjectInputMediaSync(source, channel, text string, blocks []ContentBlock) string {
	logf("inject_input_media_sync: source=%s channel=%s blocks=%d", source, channel, len(blocks))
	return ""
}
func (IOInjectorImpl) InjectInterruptMedia(source, channel, text string, blocks []ContentBlock) {
	logf("inject_interrupt_media: source=%s channel=%s blocks=%d", source, channel, len(blocks))
}
func (IOInjectorImpl) SetToolBlocks(blocks []ContentBlock) {
	logf("set_tool_blocks: blocks=%d", len(blocks))
}

// ---- 带 InjectOptions 的注入 ----

func (IOInjectorImpl) InjectInputSync(source, channel, text string) string {
	logf("inject_sync: source=%s channel=%s", source, channel)
	return ""
}

func (IOInjectorImpl) InjectTextOpts(source, channel, text string, opts InjectOptions) {
	logf("inject_text_opts: source=%s channel=%s no_memory=%v policy=%s", source, channel, opts.NoMemory, opts.ContextPolicy)
}
func (IOInjectorImpl) InjectInterruptTextOpts(source, channel, text string, opts InjectOptions) {
	logf("inject_interrupt_opts: source=%s channel=%s no_memory=%v policy=%s", source, channel, opts.NoMemory, opts.ContextPolicy)
}
func (IOInjectorImpl) InjectInputSyncOpts(source, channel, text string, opts InjectOptions) string {
	logf("inject_sync_opts: source=%s channel=%s no_memory=%v policy=%s", source, channel, opts.NoMemory, opts.ContextPolicy)
	return ""
}
func (IOInjectorImpl) InjectInputMediaOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions) {
	logf("inject_input_media_opts: source=%s channel=%s blocks=%d no_memory=%v policy=%s", source, channel, len(blocks), opts.NoMemory, opts.ContextPolicy)
}
func (IOInjectorImpl) InjectInputMediaSyncOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions) string {
	logf("inject_input_media_sync_opts: source=%s channel=%s blocks=%d no_memory=%v policy=%s", source, channel, len(blocks), opts.NoMemory, opts.ContextPolicy)
	return ""
}
func (IOInjectorImpl) InjectInterruptMediaOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions) {
	logf("inject_interrupt_media_opts: source=%s channel=%s blocks=%d no_memory=%v policy=%s", source, channel, len(blocks), opts.NoMemory, opts.ContextPolicy)
}

type PluginSDK struct {
	Name          string
	mu            sync.RWMutex
	toolDefs      map[string]ToolDef
	toolHandlers  map[string]ToolHandler
	stageHandlers map[string]StageHandler
	outChannels   map[string]ToolHandler
	Settings      SettingsAPI
	IO            IOInjector
}

func New(name string) *PluginSDK {
	return &PluginSDK{
		Name:          name,
		toolDefs:      make(map[string]ToolDef),
		toolHandlers:  make(map[string]ToolHandler),
		stageHandlers: make(map[string]StageHandler),
		outChannels:   make(map[string]ToolHandler),
		Settings:      &mockSettings{data: map[string]interface{}{}},
		IO:            IOInjectorImpl{},
	}
}

func (s *PluginSDK) RegisterTool(name string, def ToolDef, handler ToolHandler) {
	logf("register_tool: %s", name)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toolDefs[name] = def
	s.toolHandlers[name] = handler
}

func (s *PluginSDK) RegisterStage(stage Stage, handler StageHandler) {
	logf("register_stage: %s", string(stage))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stageHandlers[string(stage)] = handler
}

func (s *PluginSDK) RegisterOutputChannel(name string, caps int, desc string, handler ToolHandler) {
	logf("register_output_channel: %s", name)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outChannels[name] = handler
}

func (s *PluginSDK) RegisterPluginAPI(name string) {
	logf("register_api: %s", name)
}

func (s *PluginSDK) CallTool(name string, args map[string]interface{}) (interface{}, error) {
	s.mu.RLock()
	handler, ok := s.toolHandlers[name]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("tool not found: %s", name)
	}
	return handler(args)
}

func (s *PluginSDK) CallStage(stage string, ctx *StageContext) error {
	s.mu.RLock()
	handler, ok := s.stageHandlers[stage]
	s.mu.RUnlock()
	if !ok {
		return nil
	}
	return handler(ctx)
}

func (s *PluginSDK) ListTools() []ToolDef {
	s.mu.RLock()
	defer s.mu.RUnlock()
	defs := make([]ToolDef, 0, len(s.toolDefs))
	for _, def := range s.toolDefs {
		defs = append(defs, def)
	}
	return defs
}

func (s *PluginSDK) ListToolsJSON() string {
	defs := s.ListTools()
	b, _ := json.MarshalIndent(defs, "", "  ")
	return string(b)
}

func NewPluginSDK(name string) *PluginSDK {
	return New(name)
}
