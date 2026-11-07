package tui

import (
	"io"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/skill"
)

// ── tuiModel 消息 ──

// clientEventMsg 由读取 Client 事件的命令在读到一条事件后发出；同一时刻只挂一个读取命令。
type clientEventMsg struct {
	event runtimeclient.ClientEvent
}

// clientEventsClosedMsg 由读取命令在 Client 事件流关闭（Client.Run 已返回）后发出，界面随之退出。
type clientEventsClosedMsg struct{}

// conversationCreatedMsg 由新建对话的命令在 CreateConversation 返回后发出；seq 与 navigationSeq 不同时结果已过时。
type conversationCreatedMsg struct {
	seq          int
	conversation defs.Conversation
	err          error
}

// conversationOpenedMsg 由打开对话的命令在 OpenConversation 返回后发出：首次同步已完成，或打开失败。
type conversationOpenedMsg struct {
	seq          int
	conversation defs.Conversation
	err          error
}

// conversationForkedMsg 由 /fork 和回退面板的分叉命令在 ForkConversation 返回后发出：成功时新对话已在 serve 上打开；
// seq 与 navigationSeq 不同时结果已过时。
type conversationForkedMsg struct {
	seq          int
	conversation defs.Conversation
	// composerText 是切到新对话后填进输入框的原文，/fork 时为空。
	composerText string
	// notice 是分叉成功后状态行显示的提示。
	notice string
	err    error
}

// inputSubmittedMsg 由提交命令在 SubmitInput 返回后发出；失败时 text 用来恢复草稿。
type inputSubmittedMsg struct {
	conversationID defs.ConversationID
	text           string
	mode           session.SessionInputMode
	// isDraftOpeningInput 这条输入是不是草稿打开流程依次提交的？只有这样的结果才接着发下一条或终止待发送队列。
	isDraftOpeningInput bool
	itemID              defs.ItemID
	err                 error
}

// turnInterruptedMsg 由中断命令在 InterruptTurn 返回后发出；失败时 err 非空，用于提示。
type turnInterruptedMsg struct {
	err error
}

// compactRequestedMsg 由 /compact 在 CompactContext 返回后发出；err 非空表示压缩 Turn 没有被接受，用于提示。
type compactRequestedMsg struct {
	err error
}

// panelResultMsg 由面板的拉取命令在请求返回后发出：seq 与当前面板的请求序号不同时丢弃；value 是各面板自己的结果类型，由面板断言。
type panelResultMsg struct {
	seq   int
	value any
	err   error
}

// modelListLoadedMsg 由每次连上、改过配置后拉取模型目录的命令在 ListModels 返回后发出。
type modelListLoadedMsg struct {
	list runtime.ModelList
	err  error
}

// configReloadedMsg 由 /reload 在 ReloadConfig 返回后发出：result 列出正在执行、这次没有换上新配置的对话。
type configReloadedMsg struct {
	result runtime.SetDefinitionsResult
	err    error
}

// skillListLoadedMsg 由拉取 Skill 目录的命令在 GetSkills 返回后发出；seq 与 skillListSeq 不同时（切换对话前发出的查询）结果已过时。
type skillListLoadedMsg struct {
	seq    int
	skills []skill.SkillInfo
	err    error
}

// modelSetMsg 由切换模型的命令在 SetSessionModel 返回后发出；seq 与 modelSetSeq 不同时结果已过时。
type modelSetMsg struct {
	seq     int
	session defs.Session
	err     error
}

// executorsLoadedMsg 由每次连上时拉取 Executor 列表的命令在 ListExecutors 返回后发出。
type executorsLoadedMsg struct {
	executors []conversation.ExecutorDetails
	err       error
}

// workspaceDirectoryResolvedMsg 由 /cd 的解析命令在 ResolveWorkspaceDirectory 返回后发出。
type workspaceDirectoryResolvedMsg struct {
	// conversationID 是 /cd 发出时要修改的对话，草稿时为空。
	conversationID defs.ConversationID
	// settings 是 /cd 发出时这个对话（或草稿）的 Executor 设置副本。
	settings defs.ExecutorSettings
	// executorID 是解析目录所在的默认 Executor。
	executorID string
	// directory 是解析出的绝对路径。
	directory string
	err       error
}

// executorSettingsSetMsg 由 /cd、/executor 改对话设置的命令在 SetConversationExecutorSettings 返回后发出；conversation 是保存后的记录。
type executorSettingsSetMsg struct {
	conversationID defs.ConversationID
	conversation   defs.Conversation
	err            error
}

// noticeExpiredMsg 由临时通知的计时命令在到期时发出；id 与当前通知不同时，说明通知已被替换。
type noticeExpiredMsg struct {
	id int
}

// expandViewLeftMsg 由退出展开视图的计时命令在等待结束后发出，tuiModel 随后写出暂存的定稿行。
type expandViewLeftMsg struct{}

// liveFrameSettledMsg 由实时区域缩短后的计时命令在等待结束后发出，tuiModel 随后写出暂存的定稿行。
type liveFrameSettledMsg struct{}

// scrollbackPrintedMsg 由定稿输出命令或画面重建命令在完成后发出；重建失败时 err 非空。
type scrollbackPrintedMsg struct {
	err error
}

// ── tuiModel 终端命令 ──

// scrollbackRebuildCommand 是交给 tea.Exec 的命令：bubbletea 暂停事件循环与渲染器期间，它调用 tuiModel.rebuildScrollback，
// 按新宽度重写滚动历史并给实时帧留出行数；output 是 bubbletea 传进来的终端输出。
type scrollbackRebuildCommand struct {
	model  *tuiModel
	output io.Writer
}

// ── tuiModel 状态 ──

// notice 是状态行上的临时通知：到期或下一次按键时消失。
type notice struct {
	id      int
	text    string
	isError bool
}

// ── tuiModel 布局 ──

// liveLayout 是一帧实时区域各部分分到的行数，自上而下依次为尾部或面板、状态行、排队预览、slash 弹窗、输入框、footer。
type liveLayout struct {
	body     int
	status   int
	queue    int
	popup    int
	composer int
	// composerPadding 是输入框上方、下方各加的 surface 空行数，先于状态行等其他部分从剩余高度里扣除。
	composerPadding int
	footer          int
}

// ── tuiModel 样式 ──

// tuiStyles 是界面的文字与底色样式；notty 时都不加修饰。底色只用在每帧重绘的区域。
type tuiStyles struct {
	// user 是用户输入的文字样式。
	user lipgloss.Style
	// agent 是回复标记 ● 和思考中状态的文字样式。
	agent lipgloss.Style
	// faint 是辅助信息的暗色文字。
	faint   lipgloss.Style
	warning lipgloss.Style
	success lipgloss.Style
	failure lipgloss.Style
	// accent 是转动图标、提示符和选中项的强调色。
	accent lipgloss.Style
	// heading 是面板节标题。
	heading lipgloss.Style
	// hintKey 是按键提示里加粗的按键文字。
	hintKey lipgloss.Style
	// surface 是输入框、面板、弹窗与展开视图里用户输入的底色；不使用底色时不加修饰。
	surface lipgloss.Style
	// selection 是选中行：强调色文字加选中底色；不使用底色时等于 accent。
	selection lipgloss.Style
	// diffAdded、diffRemoved 是展开视图 diff 的增行、删行：成功、失败色文字加 diff 底色；不使用底色时分别等于 success、failure。
	diffAdded   lipgloss.Style
	diffRemoved lipgloss.Style

	// hasBackground 是否使用底色？输入块按它决定是否在上下各加一行空行。
	hasBackground bool
}

// styledText 是一段文字与它的样式，单行提示与 footer 按段上色时使用：各段套上样式后再按显示宽度截断或折行，
// 来自模型或服务端的原文由 formatStyledTexts 先去掉终端控制。
type styledText struct {
	style lipgloss.Style
	text  string
}

// ── tuiModel 状态配色 ──

// displayState 是界面上按状态上色的状态：同一种状态在所有位置都用 getStateStyle 给出的颜色，显示的文字由各处自己决定。
type displayState int

const (
	// stateRunning 是运行中。
	stateRunning displayState = iota
	// stateGenerating 是根 Agent 生成中。
	stateGenerating
	// stateThinking 是根 Agent 思考中（有推理在生成）。
	stateThinking
	// stateCompleted 是完成或成功。
	stateCompleted
	// stateConnected 是已连接。
	stateConnected
	// stateFailed 是失败。
	stateFailed
	// stateInterrupted 是已中断。
	stateInterrupted
	// stateNotExecuted 是 Turn 结束时仍没有结果的工具调用。
	stateNotExecuted
	// stateReconnecting 是重连中。
	stateReconnecting
	// stateIdle 是空闲。
	stateIdle
	// stateStopped 是已停止。
	stateStopped
	// stateConnecting 是连接中。
	stateConnecting
)

// ── tuiModel 按键提示 ──

// keyHint 是一条按键提示：key 是按键，action 是按下后的功能；key 为空时只是一句操作说明（例如"打字过滤"）。
type keyHint struct {
	key    string
	action string
}

// ── panel 字段 ──

// panelField 是面板一节里的一行字段：label 是标签，value 是已带样式的值。
type panelField struct {
	label string
	value string
}

// panelSection 是面板里的一节字段：title 是节标题，fields 是节内各行。
type panelSection struct {
	title  string
	fields []panelField
}

// ── panel 按键结果 ──

// panelKeyResult 是面板处理一次按键的结果。
type panelKeyResult int

const (
	// panelKeyIgnored 表示面板不处理这个键，交给全局键。
	panelKeyIgnored panelKeyResult = iota
	// panelKeyHandled 表示面板已经处理了这个键。
	panelKeyHandled
	// panelKeyClosed 表示面板请求关闭（在最外层按 Esc）。
	panelKeyClosed
)

// ── sessionStatuses 查询结果 ──

// sessionTreeRow 是子 Agent 面板的一行：Session 的运行信息与它在 Session 树中的深度（根 Session 的子 Session 为 0）。
type sessionTreeRow struct {
	info  runtime.SessionRuntimeInfo
	depth int
}

// ── transcript 回复状态 ──

// replyStream 是一条根 Agent 回复的流式定稿状态。
type replyStream struct {
	markdown *markdownStream
	// source 是回复到目前为止的完整源文本（开始、增量、完成、保存时同步更新），展开视图读取；markdown 只保留未定稿部分。
	source strings.Builder
	// hasPrinted 是否已有段落写进 scrollback？第一段以 "● " 开头；中断时据此补"未保存"标记。
	hasPrinted bool
	// liveLines 是未定稿尾部的渲染结果，只在内容变化或换宽度时重新渲染。
	liveLines []string
	// hasText 回复是否已输出非空文本？第一次输出时关闭当前思考与工具组。
	hasText bool
	// isDiscarded 回复是否被丢弃了（中断且不保存，或重连同步时既没保存也不在同步结果里）？从没写出过段落的被丢弃回复
	// 不算写进历史的单元，展开视图跳过它。
	isDiscarded bool
}

// ── transcript 思考与工具组状态 ──

// stepGroup 是两段回复之间的一组思考与工具调用：从 Turn 内第一个还没归组的推理或 ToolCall 开始；下一段非空回复、
// 一条用户输入或单行提示、别的 Turn 的 Item 或 Turn 结束时关闭（transcript 不再把它当作 openStepGroup）；关闭且每个调用都有结果
// （或 Turn 已结束）后写进 scrollback。
type stepGroup struct {
	turnID defs.TurnID
	// steps 是组内的推理与工具调用，按首次出现的顺序。
	steps []*groupStep
	// startedAt 是 TUI 观察到第一步开始的时刻，运行中的耗时从它算起；历史回放的组为零值。
	startedAt time.Time
	// isPrinted 这组的汇总是否已经写进 scrollback？写过之后不再出现在实时区域。
	isPrinted bool
}

// groupStep 是组内的一步：reasoning 与 call 恰有一个非 nil。
type groupStep struct {
	reasoning *groupReasoning
	call      *toolCall
}

// groupReasoning 是组内的一段推理：生成中按增量追加片段，完成或保存时换成 Item 上的完整摘要与正文。
type groupReasoning struct {
	itemID defs.ItemID
	// summary、content 是 TUI 自己持有的片段副本，不引用事件里的 Item。
	summary []string
	content []string
	// isGenerating 推理是否还在生成？运行中的树据此显示"思考中"。
	isGenerating bool
	// savedAt 是推理 Item 的保存时间（Unix 毫秒），汇总的耗时用它计算；未保存时为 0。
	savedAt int64
}

// toolCall 是组内的一次调用及其结果，按 CallID 配对。
type toolCall struct {
	call   defs.ToolCall
	result *defs.ToolResult
	// callSavedAt、resultSavedAt 是 ToolCall 与 ToolResult 的保存时间（Unix 毫秒），汇总的耗时用它们计算；未保存时为 0。
	callSavedAt   int64
	resultSavedAt int64
	// progress 是最近一行执行输出（来自 item/toolProgress）。
	progress string
	// isNotExecuted 所属 Turn 结束时这次调用是否仍没有结果？取消时保存成中断状态的调用也算。
	isNotExecuted bool
}

// toolCallState 是一次调用在汇总与详情中的状态。
type toolCallState int

const (
	toolCallRunning toolCallState = iota
	toolCallSucceeded
	toolCallFailed
	toolCallNotExecuted
)

// ── transcript 轮次开头输入 ──

// turnOpeningInput 是一轮开头那批用户消息，例如排队合成一批的多条、上一轮中断时没读到的补充消息。
type turnOpeningInput struct {
	turnID defs.TurnID
	// texts 是这一轮第一条带 ModelCallID 的 Item 之前全部用户消息的原文，按保存顺序。
	texts []string
	// hasModelOutput 这一轮是否已经有带 ModelCallID 的 Item？之后保存的用户消息是模型开始输出后插进来的补充消息，不算开头。
	hasModelOutput bool
}

// ── transcript 历史单元 ──

// historyEntryKind 是历史单元的种类。
type historyEntryKind int

const (
	historyUserInput historyEntryKind = iota
	historyReply
	historyStepGroup
	historyInstruction
	// historyHookRun 是一条已保存的 hook 运行：展开视图里每一条都显示，主视图只写出其中拦下、出错、结束本轮或带系统消息的提示行。
	historyHookRun
	historyLine
	// historyTurnDivider 是 Turn 分隔线：登记在一个新 Turn 的第一个单元之前，没有其他字段。
	historyTurnDivider
)

// historyEntry 是 transcript 写进历史的一个单元，按首次出现的顺序登记内容，展开视图按它重新渲染。
// kind 决定用哪个字段，Turn 分隔线不用任何字段。
type historyEntry struct {
	kind historyEntryKind
	// userInput 是用户输入正文。
	userInput string
	// replyID、reply 是根 Agent 回复；回复从 transcript.replies 删除后指针仍可读。
	replyID defs.ItemID
	reply   *replyStream
	// group 是思考与工具组。
	group *stepGroup
	// line 是单行提示按顺序的各段完整原文与样式：历史里截断成一行，展开视图里折行显示。
	line []styledText
	// instruction 是 Skill、AGENTS.md、记忆目录或客户端指令 Item。
	instruction *defs.Item
	// hookRun 是 hook 运行记录。
	hookRun *defs.HookRun
	// hookToolName 是 hook 所针对调用的工具名，调用不在 transcript 里时为空。
	hookToolName string
}
