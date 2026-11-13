package desktop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/egoist/mygo/ui"
)

// ---- Desktop 阅读与编辑目标 ----

type readingTarget struct {
	ServerID       string
	ConversationID defs.ConversationID
	SessionID      defs.SessionID
	Request        uint64
}
type draftKey struct {
	ServerID       string
	ConversationID defs.ConversationID
}
type conversationDraft struct {
	Content []*defs.Content
	Pending bool
	Error   string
	// NeedsProvider 为真时 Error 是「还没有模型服务」，输入区在它旁边给出「添加模型服务」入口。
	NeedsProvider bool
}
type viewItem struct {
	Item      *defs.Item
	Ordinal   storage.Ordinal
	ItemIndex int
	Streaming bool
	ArrivedAt time.Time
}
type confirmation struct {
	Kind         string
	Conversation defs.Conversation
	ItemID       defs.ItemID
	TurnID       defs.TurnID
	ProviderID   string
	Provider     defs.ProviderConfig
	CatalogID    string
	Page         string
	Name         string
}

// ---- Composer 技能补全 ----

// skillCatalogKey 标识一组可用技能的来源：已有对话只用 ConversationID；草稿用 Agent、Executor 和目录。
type skillCatalogKey struct {
	ConversationID defs.ConversationID
	AgentName      string
	ExecutorID     string
	Directory      string
}

// skillCatalog 是一个来源下 skill/list 的结果，由 Desktop 在主线程持有；同一来源只请求一次。
type skillCatalog struct {
	Skills  []skill.SkillInfo
	Loading bool
	Err     error
}

// ---- WorkLocation 目录输入 ----

// directoryListing 是目录输入框当前的建议来源：ExecutorID 与 Parent 标识一次 executor/listDirectories 请求。
type directoryListing struct {
	ExecutorID string
	Parent     string
	// Directory 是服务端解析出的绝对上级目录，选中建议时用它拼出完整路径。
	Directory string
	Names     []string
	Loading   bool
	Err       error
}

// directoryEditing 是目录输入框的编辑状态：ExecutorID 是开始编辑时目录所属的 Executor。
type directoryEditing struct {
	ExecutorID string
	// IsFocused 记录上一帧输入框是否有焦点，用来发现失焦。
	IsFocused bool
	// IsSuggesting 为真时才显示建议：点击输入框或打字之后、选中建议或失焦之前。
	IsSuggesting bool
	// IsTyped 为真表示用户在这次聚焦里改过目录；改过之后「最近用过」才按输入前缀过滤。
	IsTyped bool
	// IsCaretAtEndRequested 让下一帧把光标放到末尾，选中建议后使用。
	IsCaretAtEndRequested bool
}

// ---- Desktop 图片资源 ----

type imageAttachment struct {
	Content *defs.Content
	Bitmap  *ui.Bitmap
	Bytes   int
}

type imageResource struct {
	Source  ui.ImageSource
	Loading bool
	Err     error
}

// imageHTTPError 是图片地址返回非 200 状态时的加载错误。
type imageHTTPError struct{ Status int }

// Error 返回带状态码的原文。
func (e *imageHTTPError) Error() string { return fmt.Sprintf("image HTTP %d", e.Status) }

// errUnsupportedImageURL 表示图片地址不是 HTTP(S) 或 data URL。
var errUnsupportedImageURL = errors.New("unsupported image URL")

// errNotImage 表示添加的文件不是图片。
var errNotImage = errors.New("not an image")

// errImageTooLarge 表示添加的图片文件超过 10 MiB。
var errImageTooLarge = errors.New("image exceeds 10 MiB")

// errInvalidImageDataURL 表示 data URL 缺少数据部分。
var errInvalidImageDataURL = errors.New("invalid image data URL")

// ---- Settings 设置行 ----

// settingInputOpt 是设置行里输入框的选项；Error 非空时显示在输入框下方并把边线改成危险色。
type settingInputOpt struct {
	Password, ReadOnly bool
	Error              string
}

// ---- Provider 编辑原文 ----

type providerDraft struct {
	ID, Protocol, BaseURL, APIKey       string
	EditingID, DefaultModelKey, IDError string
	Endpoints                           map[string]string
	Headers                             []*providerHeaderDraft
	Models                              []*providerModelDraft
	RemovedModels                       map[string]*providerModelDraft
}

// providerHeaderDraft 是服务编辑表单里的一行请求头：名称和值都是输入原文，编辑过程中允许名称为空。
type providerHeaderDraft struct{ Name, Value string }

type providerModelDraft struct {
	Description                                                                                                                                              string
	Key, Alias, UpstreamModel, DisplayName, Protocol, ContextWindow, MaxOutputTokens, Thinking, ReasoningEfforts, DefaultReasoningEffort, Inputs, MergeRoles string
	InheritReasoning, InheritInputs, InheritMerge                                                                                                            bool
	Error                                                                                                                                                    string
}

// ---- Desktop 派生消息行 ----

// messageRow 是消息列表中的一行，Index 指向 Desktop.items 中该行的 Item；
// MergedErrors 是同一 Turn 内紧邻的后续模型错误在 Desktop.items 中的下标，它们和第一条错误合成一行。
type messageRow struct {
	Key          string
	Index        int
	MergedErrors []int
}

// ---- Sidebar 派生行 ----

// sidebarRow 是侧栏列表中的一行：Group 非空时是分组标题，否则是 Conversation 行。
type sidebarRow struct {
	Key          string
	Group        string
	Conversation defs.Conversation
}

// ---- Conversation 标题栏 ----

// sessionCrumb 是子 Session 标题栏面包屑的一级：点击切换阅读到这个 Session。
type sessionCrumb struct {
	SessionID defs.SessionID
	Name      string
}

// readingRequested 由主线程切换目标时交付，读取任务顺序执行订阅与首屏查询。
type readingRequested struct {
	Context context.Context
	Target  readingTarget
	Client  runtimeclient.Client
}

// ---- Terminal 运行状态 ----

// terminalPhase 是下 Dock 一个终端标签所处的阶段。
type terminalPhase int

const (
	// terminalOpening 表示 terminal/open 已发出，还没有拿到终端 ID。
	terminalOpening terminalPhase = iota
	// terminalRunning 表示终端进程在 Executor 上运行，输入和尺寸变化会发给它。
	terminalRunning
	// terminalReconnecting 表示终端还在 serve 上运行，但这条连接尚未接入：连接断开后等重连，或刚从运行列表加成标签；输入被忽略。
	terminalReconnecting
	// terminalExited 表示进程已结束、serve 上已没有这个终端或打开失败；标签保留，输入被忽略。
	terminalExited
)

// terminalTab 是下 Dock 的一个终端标签，由 Desktop 在主线程持有。
type terminalTab struct {
	// Key 是桌面端本地的稳定键，用于界面元素和打开结果匹配，不是服务端的终端 ID。
	Key string
	// TerminalID 是 serve 返回的终端 ID；打开完成前为空。
	TerminalID string
	// ExecutorID、Directory 是终端所在的 Executor 与起始目录；打开成功后以 serve 返回的为准。
	ExecutorID string
	Directory  string
	State      *kit.TerminalState
	Phase      terminalPhase
	// OpenRequest 是最近一次 terminal/open 请求的序号；迟到的旧请求结果不套用到新请求上。
	OpenRequest uint64
	// ReceivedCursor 是已经写进 State 的输出位置；断线重连从这里续读，新屏幕是 0。
	ReceivedCursor uint64
	// IsReplaying 是否正在回放接入时已有的输出？回放期间以及结束后的 100ms 内，终端的输入和尺寸变化不发给 serve。
	IsReplaying bool
	// WasTruncated 这次回放是否遇到过 serve 已丢弃的输出？
	WasTruncated bool
	// AttachedRows、AttachedCols 是最近一次接入时 serve 上 PTY 的行列数；回放结束时与屏幕尺寸比较。
	AttachedRows, AttachedCols int
	// Handle 绑定终端元素，打开成功后用它把键盘焦点交给终端。
	Handle         ui.Handle
	FocusRequested bool
}

// ---- Desktop 元素本地状态键 ----

// outputScrollKey 是普通输出块在元素上保存滚动状态的键。
type outputScrollKey struct{}

// commandOutputScrollKey 是命令输出块在元素上保存滚动状态的键。
type commandOutputScrollKey struct{}

// patchScrollKey 是补丁块在元素上保存滚动状态的键。
type patchScrollKey struct{}

// jsonTreeKey 是 JSON 视图在元素上保存解析树的键。
type jsonTreeKey struct{}

// jsonSourceKey 是 JSON 视图在元素上保存上次解析原文的键。
type jsonSourceKey struct{}

// settingsPageKey 是设置页在根元素上保存已显示页面的键。
type settingsPageKey struct{}

// settingsChangedKey 是设置页在根元素上保存页面切换时刻的键。
type settingsChangedKey struct{}
