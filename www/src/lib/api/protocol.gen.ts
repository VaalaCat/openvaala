// 由 internal/protocolgen 生成，不要手改

// From defs/message.go
/**
 * Address 是消息的发送方或接收方。
 */
export interface Address {
    kind: IdentityKind;
    /**
     * ID 在 Agent 地址里是 Session ID。
     */
    id: string;
}

// From runtime/types.go
/**
 * AgentInfo 是 ListAgents 返回的一个 Agent 的公开说明，不含系统指令。
 */
export interface AgentInfo {
    name: string;
    description?: string;
    /**
     * DefaultModel 是 Agent 的默认模型；Agent 没有指定默认模型时为零值。
     */
    default_model?: ModelRef;
}

// From agent/types.go
/**
 * AgentItemDelta 是 Agent 生成的一段内容增量，按 Item.Kind 填对应字段。
 * 增量里的切片是单独的副本，接收方可以直接留用；推理的分段和签名以完成事件里的完整 Item 为准。
 */
export interface AgentItemDelta {
    /**
     * Text 是追加到消息的文本。
     */
    text?: string;
    /**
     * ReasoningSummary 是追加的推理摘要片段。
     */
    summary?: string[];
    /**
     * ReasoningContent 是追加的推理内容片段。
     */
    content?: string[];
    /**
     * Arguments 是追加的工具参数。
     */
    arguments?: string;
}

// From defs/message.go
/**
 * AgentRelation 是 Agent 消息发送方相对接收方的位置。
 */
export type AgentRelation = "ancestor" | "child";

// From defs/message.go
/**
 * AgentSender 是 Agent 消息发送方的任务名，以及它和接收方的关系。
 */
export interface AgentSender {
    /**
     * TaskName 是发送方 Session 的任务名；主 Agent 没有任务名，为空。
     */
    task_name?: string;
    relation: AgentRelation;
}

// From agent/types.go
/**
 * AgentToolProgress 是工具执行中途的一段输出，最终结果另由 ToolResult Item 给出；事件的 ItemIndex 指向对应的 ToolCall。
 */
export interface AgentToolProgress {
    /**
     * CallID 是对应工具调用的 CallID，可以关联执行前已保存的 ToolCall。
     */
    call_id: string;
    /**
     * Stream 是这段输出所属的流类别。
     */
    stream: ExecutionStreamType;
    /**
     * Content 是这段输出的 JSON 内容。
     */
    content: unknown;
}

// From defs/item.go
/**
 * AgentsInstruction 是某个 Executor 上一份 AGENTS.md 的路径、正文和适用范围。
 */
export interface AgentsInstruction {
    executor: string;
    path: string;
    text: string;
    /**
     * IsGlobal 是否为适用于本 Executor 全部工作区的用户级规则？
     */
    is_global?: boolean;
}

// From runtimeprotocol/types.go
/**
 * AppendContextItemsParams 是 conversation/appendContextItems 的参数。
 */
export interface AppendContextItemsParams {
    conversation_id: ConversationID;
    /**
     * Items 可以是任何不需要模型调用、也不由运行时自己写入的种类，载荷必须有效；ID 由调用方生成，还没分配 turn_id 和 model_call_id。
     */
    items: (Item | null)[];
}

// From defs/provider.go
/**
 * CatalogProvider 是模型目录里的一个服务，供引导第 1 步选择；选中后预先填好 ID、协议和 base_url。
 */
export interface CatalogProvider {
    id: string;
    name: string;
    protocol: string;
    base_url: string;
    model_count: number;
}

// From runtimeprotocol/types.go
/**
 * ClientInfo 是客户端的名称和版本。
 */
export interface ClientInfo {
    name: string;
    version?: string;
}

// From defs/item.go
/**
 * ClientInstruction 是客户端写进上下文的一段指令，例如旁支的「之前的历史只作参考，别接着做」。
 */
export interface ClientInstruction {
    text: string;
}

// From runtimeprotocol/types.go
/**
 * ClientMCPClosedParams 是客户端发给 serve 的 clientMCP/closed 的参数：客户端在 stream_id 这条流连不上、或本机 MCP server 结束时发出；
 * error 是失败原因，正常结束时省略。
 */
export interface ClientMCPClosedParams {
    stream_id: string;
    error?: string;
}

// From runtimeprotocol/types.go
/**
 * ClientMCPConnectParams 是 serve 发给客户端的 clientMCP/connect 的参数：转发传输每次连接尝试时发出一次，
 * 请客户端按 server_id 的配置连上本机的 MCP server，此后这条流上的消息都带 stream_id。重连时换新的 stream_id。
 */
export interface ClientMCPConnectParams {
    stream_id: string;
    server_id: string;
}

// From runtimeprotocol/types.go
/**
 * ClientMCPDisconnectParams 是 serve 发给客户端的 clientMCP/disconnect 的参数：serve 一侧结束 stream_id 这条流时发出，
 * 客户端随之关闭与本机 MCP server 的连接。
 */
export interface ClientMCPDisconnectParams {
    stream_id: string;
}

// From runtimeprotocol/types.go
/**
 * ClientMCPMessageParams 是 clientMCP/message 的参数，两个方向通用：serve 发给客户端的是 MCP SDK 写出的消息，
 * 客户端发给 serve 的是本机 MCP server 发来的消息。message 是 stream_id 这条流上的一条完整 MCP JSON-RPC 消息原文。
 */
export interface ClientMCPMessageParams {
    stream_id: string;
    message: unknown;
}

// From runtimeprotocol/types.go
/**
 * CompactContextResult 是 conversation/compact 的结果：刚开始的压缩 Turn；压缩尚未完成。
 */
export interface CompactContextResult {
    turn_id: TurnID;
}

// From mcpclient/types.go
/**
 * ConnectionInfo 是一个 MCP 连接此刻的情况。
 */
export interface ConnectionInfo {
    server_id: string;
    state: ConnectionState;
    required: boolean;
    /**
     * Tools 是 server 当前提供的工具（已按 ToolNames 过滤）；没连上时为空。
     */
    tools: ConnectionTool[];
    /**
     * Error 是最近一次连接失败或断开的原因；连上后清空。
     */
    error?: string;
    /**
     * PluginName 是声明这个 server 的插件名；不是插件声明的为空。
     */
    plugin_name?: string;
}

// From mcpclient/types.go
/**
 * ConnectionState 是一个 MCP 连接现在的状态。
 */
export type ConnectionState = "connected" | "connecting" | "failed";

// From mcpclient/types.go
/**
 * ConnectionTool 是 server 提供的一个工具。
 */
export interface ConnectionTool {
    /**
     * Name 是模型看到的名字 mcp__<server id>__<原名>：非法字符换成下划线，替换后重名或超过 64 字符时改用带哈希的名字。
     */
    name: string;
    /**
     * OriginalName 是 server 里的原名。
     */
    original_name: string;
    description?: string;
    input_schema: unknown;
}

// From defs/message.go
/**
 * Content 是消息里的一项文本或图片，Kind 决定哪个字段有值。
 */
export interface Content {
    kind: ContentKind;
    text?: TextContent;
    image?: ImageContent;
}

// From defs/message.go
/**
 * ContentKind 区分文本和图片内容。
 */
export type ContentKind = "image" | "text";

// From defs/item.go
/**
 * ContextSummary 是压缩上下文时生成的累积历史摘要。
 */
export interface ContextSummary {
    text: string;
    /**
     * CoveredThroughItemID 是摘要覆盖到的最后一条原始 Item，包含它本身。
     */
    covered_through_item_id: ItemID;
}

// From runtime/types.go
/**
 * ContextUsage 是下一次请求的预计占用。
 */
export interface ContextUsage {
    /**
     * Window 是模型上下文窗口；0 表示窗口未知。
     */
    window: number;
    /**
     * Tokens 是预计的输入 token 数。
     */
    tokens: number;
    /**
     * Source 是 Tokens 的来源：实际基准加估算、整份估算，或还没有测量。
     */
    source: ContextUsageSource;
}

// From runtime/types.go
/**
 * ContextUsageSource 是预计占用的来源。
 */
export type ContextUsageSource = "actual" | "estimated" | "unknown";

// From defs/conversation.go
/**
 * Conversation 是一个对话的记录；每个对话有唯一的根 Session。
 */
export interface Conversation {
    id: ConversationID;
    /**
     * ExecutorSettings 是对话的 Executor 设置：新建时补上缺的 local 目录后保存，之后只由改设置修改；打开对话不写它。
     */
    executor_settings: ExecutorSettings;
    /**
     * UpdatedAt 是最后一次写入的 Unix 毫秒，由存储在写入时分配；写入时设置的值不会被保存。
     * 创建、分叉、改标题、改 Executor 设置、归档或取消归档、更新消息数都会刷新这个时间。
     */
    updated_at?: number;
    title?: string;
    root_session_id: SessionID;
    /**
     * MessageCount 是根 Session 中用户消息与主 Agent 回复的条数，每轮结束时累加。
     */
    message_count?: number;
    /**
     * ForkedFromID 是分叉来源的对话；不是分叉出来的为空。
     */
    forked_from_id?: ConversationID;
    /**
     * ArchivedAt 是归档时间（Unix 毫秒），0 表示没归档。
     */
    archived_at?: number;
}

// From defs/id.go
/**
 * ConversationID 唯一标识一个对话，并按创建时间排序。
 */
export type ConversationID = string;

// From dao/conversation.go
/**
 * ConversationListEntry 是对话列表里的一条：对话记录和它的根 Session。
 */
export interface ConversationListEntry {
    conversation: Conversation;
    root_session: Session;
}

// From runtimeprotocol/types.go
/**
 * ConversationParams 用于只需要对话身份的方法：read、interruptTurn、compact、delete、subscribe、unsubscribe、mcpServer/list。
 */
export interface ConversationParams {
    conversation_id: ConversationID;
}

// From runtimeprotocol/types.go
/**
 * ConversationResult 是返回一条对话记录的结果。
 */
export interface ConversationResult {
    conversation: Conversation;
}

// From runtimeprotocol/types.go
/**
 * ConversationTitleUpdatedParams 由连接在对话标题保存成功且变化后发出，空字符串表示清空标题。
 */
export interface ConversationTitleUpdatedParams extends NotificationTarget {
    title: string;
}

// From runtimeprotocol/types.go
/**
 * CreateConversationParams 是 conversation/create 的参数。
 */
export interface CreateConversationParams {
    agent_name: string;
    model?: ModelSelection;
    /**
     * ExecutorSettings 里没有 local 目录时使用 local 的默认目录（server.workspace_directory，没配时是 serve 的工作目录）。
     */
    executor_settings: ExecutorSettings;
    /**
     * Environment 为空时本机命令继承 serve 进程的环境。
     */
    environment?: string[];
    /**
     * Ephemeral 是否建临时对话？临时对话只在内存里，不进对话列表，没人用时丢弃。
     */
    ephemeral?: boolean;
    /**
     * WithoutExecutors 是否不装配 Executor 和 Skill？为 true 时 executor_settings 原样记下但不生效，environment 忽略。
     */
    without_executors?: boolean;
}

// From storage/page.go
/**
 * Direction 是 Record 与 Log 读取的方向。
 */
export type Direction = "asc" | "desc";

// From runtimeprotocol/types.go
/**
 * EmptyParams 用于无参数的方法，包括全局运行订阅和退订。
 */
export interface EmptyParams {
}

// From runtimeprotocol/types.go
/**
 * EmptyResult 是没有返回内容的方法的结果，编码为 {}。
 */
export interface EmptyResult {
}

// From executor/types.go
/**
 * ExecutionStreamType 是一段执行进度的来源。
 */
export type ExecutionStreamType = "terminal";

// From conversation/types.go
/**
 * ExecutorDetails 是一个 Executor 的展示字段：ID、种类、说明和工作区根目录。
 */
export interface ExecutorDetails {
    id: string;
    /**
     * Kind 是 Executor 的种类；界面选默认 Executor 时用它去掉 memory。
     */
    kind: ExecutorKind;
    description?: string;
    workspace_directory?: string;
}

// From defs/executor.go
/**
 * ExecutorKind 是 Executor 的种类；自定义 Executor 可以用自己的种类。
 */
export type ExecutorKind = "local" | "memory" | "ssh" | "virtual";

// From defs/conversation.go
/**
 * ExecutorSettings 是对话的 Executor 设置：默认用哪个、能用哪些、各自在哪个目录。
 */
export interface ExecutorSettings {
    /**
     * DefaultExecutorID 是默认 Executor：模型省略 executor 参数时用它，项目 Skills、用户主目录 Skills 和 AGENTS.md 从它读；
     * 为空时是筛选后列表的第一个。
     */
    default_executor_id?: string;
    /**
     * ExecutorIDs 是对话可用的 Executor，只决定能不能用，不决定顺序；为空时全部可用，包括以后新加的。
     */
    executor_ids?: string[];
    /**
     * WorkspaceDirectories 是各 Executor 在这个对话里的根目录，键为 Executor ID。local 总有一项；
     * 其他 Executor 只记 /cd 改过的，没有的用默认根目录（配置里的 workspace_directory）。
     */
    workspace_directories?: Record<string, string>;
}

// From defs/item.go
/**
 * ExecutorsChange 由 Turn 在本轮 Executor 列表和上一轮不同时写在本轮输入之前：默认换了、目录换了、增加或减少都在这一条里，
 * 作为提示发给模型。
 */
export interface ExecutorsChange {
    previous_executors: TurnExecutor[];
    executors: TurnExecutor[];
}

// From runtimeprotocol/types.go
/**
 * ForkConversationParams 是 conversation/fork 的参数。
 */
export interface ForkConversationParams {
    conversation_id: ConversationID;
    /**
     * BeforeTurnID 复制到这一轮之前（不含），LastTurnID 复制到这一轮（含）；最多给一个，都不给时复制到最后一条已保存的 Item，包括还在运行的轮次已保存的部分。
     */
    before_turn_id?: TurnID;
    last_turn_id?: TurnID;
    /**
     * Model 省略时照抄来源根 Session 的模型。
     */
    model?: ModelSelection;
    /**
     * ExecutorSettings 是新对话的完整设置，和 Environment 一起按 conversation/create 的规则装配。
     */
    executor_settings: ExecutorSettings;
    environment?: string[];
    /**
     * Ephemeral 是否分叉成临时对话？临时对话只在内存里，不进对话列表，没人用时丢弃。
     */
    ephemeral?: boolean;
}

// From runtimeprotocol/types.go
/**
 * GetSessionParams 是 session/get 的参数：只凭 session_id 找 Session，不需要知道它属于哪个对话。
 */
export interface GetSessionParams {
    session_id: SessionID;
}

// From runtimeprotocol/types.go
/**
 * GetTurnsParams 是 session/turns/get 的参数；Turn 记录的读取不要求对话已打开。
 */
export interface GetTurnsParams {
    conversation_id: ConversationID;
    session_id: SessionID;
    turn_ids: TurnID[];
}

// From runtimeprotocol/types.go
/**
 * GetTurnsResult 是 session/turns/get 的结果：按请求顺序找到的 Turn。
 */
export interface GetTurnsResult {
    turns: Turn[];
}

// From defs/hook.go
/**
 * HookEventName 是触发 hook 的时刻。
 */
export type HookEventName = "PostToolUse" | "PreToolUse" | "SessionStart" | "Stop" | "SubagentStart" | "SubagentStop" | "UserPromptSubmit";

// From hook/types.go
/**
 * HookInfo 是一个配置好的 hook，供 hooks/list 显示。
 */
export interface HookInfo {
    /**
     * Key 是 HookDefinition.Key。
     */
    key: string;
    event: HookEventName;
    /**
     * Matcher 是配置里 matcher 的原文，没写时为空。
     */
    matcher?: string;
    /**
     * Handler 是显示用的命令：command 原文，或 script 路径加 args。
     */
    handler: string;
    timeout_seconds: number;
    source_path: string;
    display_order: number;
    /**
     * CurrentHash 是这个 hook 定义的哈希：定义不变则不变，改了事件、matcher、命令、脚本、参数、超时或 on_failure 就变。
     */
    current_hash: string;
    /**
     * PluginName 是 hook 所属插件的名字；用户自己的 hook 为空。
     */
    plugin_name?: string;
}

// From defs/hook.go
/**
 * HookRun 是一个 hook 的一次运行：开始时为 running，结束后更新为最终状态和输出；有效果的运行作为 hook_run Item 保存。
 */
export interface HookRun {
    id: HookRunID;
    event: HookEventName;
    /**
     * CallID 是工具事件对应的调用。
     */
    call_id?: string;
    /**
     * ExecutorID 是运行这个 hook 的 Executor。
     */
    executor_id: string;
    /**
     * SourcePath 是定义这个 hook 的配置文件。
     */
    source_path: string;
    /**
     * Handler 是显示用的命令：command 原文，或 script 路径加 args。
     */
    handler: string;
    display_order: number;
    status: HookRunStatus;
    /**
     * StartedAt 和 CompletedAt 是 Unix 毫秒；运行中时 CompletedAt 为 0。
     */
    started_at: number;
    completed_at?: number;
    block_reason?: string;
    updated_tool_arguments?: string;
    additional_context?: string;
    stop_reason?: string;
    system_message?: string;
    /**
     * ErrorMessage 在 failed 时说明原因，例如超时、脚本不存在。
     */
    error_message?: string;
}

// From defs/id.go
/**
 * HookRunID 唯一标识一次 hook 运行，并按创建时间排序。
 */
export type HookRunID = string;

// From runtimeprotocol/types.go
/**
 * HookRunParams 由连接在 Hook 开始和结束时各发一次，携带同一 ID 的完整运行记录。
 * 工具与 Stop Hook 来自 Agent，SessionStart、SubagentStart、UserPromptSubmit Hook 来自 Turn。
 */
export interface HookRunParams extends NotificationTarget {
    turn_id: TurnID;
    hook_run: HookRun | null;
}

// From defs/hook.go
/**
 * HookRunStatus 是一次 hook 运行所处的阶段或结局。
 */
export type HookRunStatus = "blocked" | "completed" | "failed" | "running" | "stopped";

// From defs/message.go
/**
 * IdentityKind 区分用户和 Agent 地址。
 */
export type IdentityKind = "agent" | "user";

// From defs/message.go
/**
 * ImageContent 是一张图片：来源和说明。
 */
export interface ImageContent {
    data_url: string;
    alt?: string;
}

// From runtimeprotocol/protocol.go
/**
 * InitializeMethod 是建立连接后的第一个请求。
 */
export const InitializeMethod = "initialize";

// From runtimeprotocol/types.go
/**
 * InitializeParams 是 initialize 的参数：客户端信息和它使用的协议版本。
 */
export interface InitializeParams {
    client_info: ClientInfo;
    protocol_version: string;
}

// From runtimeprotocol/types.go
/**
 * InitializeResult 是 initialize 的结果。
 */
export interface InitializeResult {
    server_info: ServerInfo;
    protocol_version: string;
    /**
     * ServerID 标识这台机器上的 serve；客户端读到的本机 server_id 与它一致，就说明两边用的是同一个文件系统。
     */
    server_id: string;
}

// From defs/id.go
/**
 * InputID 是调用方给一次输入提交起的去重标识，由调用方生成，与 Item 身份无关。
 */
export type InputID = string;

// From runtimeprotocol/types.go
/**
 * InterruptTurnResult 是 conversation/interruptTurn 的结果：被中断的 TurnID，没有活动 Turn 时省略。
 */
export interface InterruptTurnResult {
    turn_id?: TurnID;
}

// From runtimeprotocol/types.go
/**
 * InterruptTurnWithInputParams 是 conversation/interruptTurnWithInput 的参数。
 */
export interface InterruptTurnWithInputParams {
    conversation_id: ConversationID;
    item_id: ItemID;
    turn_id: TurnID;
}

// From defs/item.go
/**
 * Item 保存一条消息、模型输出或历史记录，Kind 指定使用的载荷。
 */
export interface Item {
    id: ItemID;
    /**
     * UpdatedAt 是保存时的 Unix 毫秒，由存储分配；Item 保存后不再修改。
     */
    updated_at?: number;
    turn_id: TurnID;
    model_call_id?: ModelCallID;
    kind: ItemKind;
    message?: Message;
    reasoning?: Reasoning;
    tool_call?: ToolCall;
    tool_result?: ToolResult;
    model_error?: ModelError;
    context_summary?: ContextSummary;
    session_fork?: SessionFork;
    model_change?: ModelChange;
    executors_change?: ExecutorsChange;
    skill_instruction?: SkillInstruction;
    agents_instruction?: AgentsInstruction;
    memory_index?: MemoryIndex;
    client_instruction?: ClientInstruction;
    hook_run?: HookRun;
}

// From runtimeprotocol/types.go
/**
 * ItemEventParams 由连接在 Item 开始、增量、完成、中断或工具进度事件到达时发出，各通知只填对应字段。
 */
export interface ItemEventParams extends NotificationTarget {
    turn_id: TurnID;
    item_index: number;
    item?: Item;
    delta?: AgentItemDelta;
    tool_progress?: AgentToolProgress;
}

// From defs/id.go
/**
 * ItemID 唯一标识一条由 OpenVaala 创建的 Item，并按创建时间排序。
 */
export type ItemID = string;

// From defs/item.go
/**
 * ItemKind 区分消息、模型输出和历史记录的种类。
 */
export type ItemKind = "agents_instruction" | "client_instruction" | "context_summary" | "executors_change" | "hook_run" | "memory_index" | "message" | "model_change" | "model_error" | "reasoning" | "session_fork" | "skill_instruction" | "tool_call" | "tool_result" | "turn_interrupted";

// From runtimeprotocol/types.go
/**
 * ItemsSavedParams 由连接在一批 Item 写入 Session 日志后发出，条目按日志序号递增。
 */
export interface ItemsSavedParams extends NotificationTarget {
    turn_id: TurnID;
    items: LoggedItem[];
}

// From runtimeprotocol/types.go
/**
 * JSONRPCError 是响应中的错误；方法处理函数返回它时保留错误码，返回其他错误时由服务端按错误种类换成对应错误码。
 */
export interface JSONRPCError {
    code: number;
    message: string;
}

// From runtimeprotocol/types.go
/**
 * JSONRPCMessage 是服务端写出的一帧：响应带 ID 与 Result 或 Error，通知只带 Method 与 Params。
 * Result 与 Params 在服务端写出这一帧时才编码。
 */
export interface JSONRPCMessage {
    id?: unknown;
    method?: string;
    // empty interface{} type, falling back to unknown
    params?: unknown;
    // empty interface{} type, falling back to unknown
    result?: unknown;
    error?: JSONRPCError;
}

// From runtimeprotocol/types.go
/**
 * JSONRPCRequest 是客户端发来的一帧请求（省略 jsonrpc 字段）；Params 原样交给方法处理函数解析。
 */
export interface JSONRPCRequest {
    id: unknown;
    method: string;
    params: unknown;
}

// From runtime/types.go
/**
 * LastRequestUsage 是最近一次上报了用量的普通模型调用。
 */
export interface LastRequestUsage {
    model_call_id: ModelCallID;
    /**
     * Model 是这次普通调用使用的模型。
     */
    model: ModelRef;
    /**
     * Usage 是服务商上报的用量。
     */
    usage: TokenUsage;
    /**
     * EstimatedTokens 是调用前未经实际用量校正的本地估算。
     */
    estimated_tokens: number;
}

// From runtimeprotocol/types.go
/**
 * ListAgentsResult 是 agent/list 的结果：配置里的全部 Agent，按配置顺序。
 */
export interface ListAgentsResult {
    agents: AgentInfo[];
}

// From runtimeprotocol/types.go
/**
 * ListCatalogProvidersResult 是 catalog/providers/list 的结果：模型目录里能对应上协议和 base_url 的服务。
 */
export interface ListCatalogProvidersResult {
    providers: CatalogProvider[];
}

// From runtimeprotocol/types.go
/**
 * ListConversationsParams 是 conversation/list 的参数；archived 为 true 时只列归档的对话，省略或 false 时只列没归档的。
 */
export interface ListConversationsParams {
    directories?: string[];
    search_term?: string;
    order?: Order;
    cursor?: RecordCursor<ConversationID>;
    limit?: number;
    archived?: boolean;
}

// From runtimeprotocol/types.go
/**
 * ListConversationsResult 是 conversation/list 的结果：一页对话和下一页游标。
 */
export interface ListConversationsResult {
    conversations: ConversationListEntry[];
    next_cursor?: RecordCursor<ConversationID>;
}

// From runtimeprotocol/types.go
/**
 * ListDirectoriesParams 是 executor/listDirectories 的参数；directory 不包含候选筛选词。
 */
export interface ListDirectoriesParams {
    executor_id: string;
    directory: string;
    base_directory?: string;
}

// From runtimeprotocol/types.go
/**
 * ListExecutorsResult 是 executor/list 的结果：全部 Executor 和它们的默认根目录。
 */
export interface ListExecutorsResult {
    executors: ExecutorDetails[];
}

// From runtimeprotocol/types.go
/**
 * ListHooksResult 是 hook/list 的结果：配置里的全部 hook，按配置顺序排列。
 */
export interface ListHooksResult {
    hooks: (HookInfo | null)[];
}

// From runtimeprotocol/types.go
/**
 * ListItemsParams 是 session/items/list 的参数；一页 Item 的 JSON 累计超过服务端的页大小上限时提前截断。
 */
export interface ListItemsParams {
    conversation_id: ConversationID;
    session_id: SessionID;
    order?: Direction;
    cursor?: Ordinal;
    limit?: number;
}

// From runtimeprotocol/types.go
/**
 * ListItemsResult 是 session/items/list 的结果：一页日志和下一页游标。
 */
export interface ListItemsResult {
    items: LoggedItem[];
    next_cursor?: Ordinal;
}

// From runtimeprotocol/types.go
/**
 * ListMCPServersResult 是 mcpServer/list 的结果：对话里每个 Executor 上的每个 MCP 连接，按 Executor 在对话里的顺序排列。
 */
export interface ListMCPServersResult {
    servers: MCPServerInfo[];
}

// From runtimeprotocol/types.go
/**
 * ListModelsParams 是 model/list 的参数；agent_name 为空时不返回 Agent 默认模型。
 */
export interface ListModelsParams {
    agent_name: string;
}

// From runtimeprotocol/types.go
/**
 * ListProvidersResult 是 provider/list 的结果：配置里的全部 Provider，带 API key 原文；DefaultModel 是模型配置的默认模型
 * （provider/model），没有时为空；Defaults 是同次加载的模型默认配置。
 */
export interface ListProvidersResult {
    providers: ProviderConfig[];
    default_model: string;
    defaults: ModelDefaults;
}

// From runtimeprotocol/types.go
/**
 * ListSessionsParams 是 session/list 的参数。
 */
export interface ListSessionsParams {
    conversation_id: ConversationID;
    order?: Order;
    cursor?: RecordCursor<SessionID>;
    limit?: number;
}

// From runtimeprotocol/types.go
/**
 * ListSessionsResult 是 session/list 的结果：一页 Session 和下一页游标。
 */
export interface ListSessionsResult {
    sessions: Session[];
    next_cursor?: RecordCursor<SessionID>;
}

// From runtimeprotocol/types.go
/**
 * ListSkillsParams 是 skill/list 的参数；没有 conversation_id 时按 agent_name 和 executor_settings 查草稿 Skills。
 */
export interface ListSkillsParams {
    conversation_id?: ConversationID;
    agent_name?: string;
    executor_settings?: ExecutorSettings;
}

// From runtimeprotocol/types.go
/**
 * ListTurnsParams 是 session/turns/list 的参数；conversation_id 选这个对话的存储，Turn 记录的读取不要求对话已打开。
 */
export interface ListTurnsParams {
    conversation_id: ConversationID;
    session_id: SessionID;
    order?: Order;
    cursor?: RecordCursor<TurnID>;
    limit?: number;
}

// From runtimeprotocol/types.go
/**
 * ListTurnsResult 是 session/turns/list 的结果：一页 Turn 和下一页游标。
 */
export interface ListTurnsResult {
    turns: Turn[];
    next_cursor?: RecordCursor<TurnID>;
}

// From runtimeprotocol/types.go
/**
 * LoggedItem 是 Session 日志中的一条 Item 及其流水号。
 */
export interface LoggedItem {
    ordinal: Ordinal;
    item: Item | null;
}

// From conversation/types.go
/**
 * MCPServerInfo 是对话里一个执行环境上的一个 MCP 连接；客户端登记的 ExecutorID 为 defs.ClientExecutorID。
 */
export interface MCPServerInfo extends ConnectionInfo {
    executor_id: string;
}

// From defs/item.go
/**
 * MemoryIndex 是 Turn 开始时从 memory Executor 读进上下文的一份 MEMORY.md。
 */
export interface MemoryIndex {
    /**
     * Path 是 memory Executor 里的路径：user/MEMORY.md 或 project/MEMORY.md。
     */
    path: string;
    /**
     * Project 是项目记忆所属的项目：项目记忆目录相对记忆根目录的路径，例如 projects/local/-tmp-x；用户级记忆为空。
     */
    project?: string;
    /**
     * Text 最多 8 KiB，超出时在最后一个完整行处截断。
     */
    text: string;
    /**
     * IsTruncated 是否截断过？
     */
    is_truncated?: boolean;
}

// From defs/message.go
/**
 * Message 是一条消息：发送方、接收方地址和内容。
 */
export interface Message {
    from: Address | null;
    to: Address | null;
    content: (Content | null)[];
    /**
     * AgentSender 由 Conversation 在 Agent 之间转交消息时写入，模型请求据此标明消息来自哪个 Agent；用户消息为 nil。
     */
    agent_sender?: AgentSender;
    /**
     * SubagentNotification 由 Conversation 在子 Agent 的 Turn 结束、通知父 Agent 时写入；其他消息为 nil。
     */
    subagent_notification?: SubagentNotification;
}

// From defs/id.go
/**
 * ModelCallID 唯一标识一次模型调用，并按创建时间排序。
 */
export type ModelCallID = string;

// From defs/model.go
/**
 * ModelCapabilities 是模型的能力；目前只有 Input 有代码在读。
 */
export interface ModelCapabilities {
    /**
     * Input 是模型能接收的输入，例如 [text, image]。
     */
    input: ModelInput[];
}

// From defs/item.go
/**
 * ModelChange 由 Turn 在本轮模型和上一轮不同时写在本轮输入之前：作为提示发给模型，它之前的推理不再发送。
 */
export interface ModelChange {
    previous_model: ModelRef;
    model: ModelRef;
}

// From defs/provider.go
/**
 * ModelDefaults 是模型配置省略字段时使用的值，default 标签给出系统内置默认值。
 */
export interface ModelDefaults {
    context_window: number;
    max_output_tokens: number;
    thinking: boolean;
    reasoning_efforts: string[];
    default_reasoning_effort: string;
    merge_message_roles: string[];
    /**
     * Capabilities 是模型配置省略 capabilities 时使用的能力；Input 为空时按 [text]。
     */
    capabilities: ProviderModelCapabilities;
}

// From defs/model.go
/**
 * ModelDefinition 是模型目录中的一项：连接参数与请求参数，默认值已解析，构造后只读。
 */
export interface ModelDefinition {
    ref: ModelRef;
    display_name: string;
    context_window: number;
    max_output_tokens: number;
    thinking_enabled: boolean;
    /**
     * ReasoningEfforts 是可选的推理强度；为空表示不支持选择，请求不带强度。
     */
    reasoning_efforts: string[];
    default_reasoning_effort?: string;
    /**
     * MergeMessageRoles 是发送前需要合并相邻同角色消息的角色；为空表示不合并。
     */
    merge_message_roles?: string[];
    /**
     * ReasoningContext 是请求要求保留的思考上下文；为空表示请求不发送，服务端按默认处理，只用于 openai-responses 协议。
     */
    // this is likely an enum in an external package "github.com/VaalaCat/ai-gateway/pkg/llmkit/ir.ReasoningContext"
    reasoning_context?: string;
    capabilities: ModelCapabilities;
}

// From defs/item.go
/**
 * ModelError 保存模型返回的错误码和消息。
 */
export interface ModelError {
    code?: string;
    message: string;
}

// From defs/model.go
/**
 * ModelInput 是模型能接收的一种输入。
 */
export type ModelInput = "audio" | "image" | "pdf" | "text" | "video";

// From runtime/types.go
/**
 * ModelList 是 ListModels 返回的模型目录；每一项只含公开字段，不含连接参数和凭证。
 */
export interface ModelList {
    models: (ModelDefinition | null)[];
    /**
     * AgentDefaultModel 是请求中 Agent 的默认模型；没有指定 Agent 时为零值。
     */
    agent_default_model?: ModelRef;
}

// From defs/model.go
/**
 * ModelRef 是模型目录中一个模型的标识：Provider 文件的 id 加这个文件里的模型 id。
 */
export interface ModelRef {
    provider_id: string;
    model_id: string;
}

// From defs/model.go
/**
 * ModelSelection 是一次模型选择：用哪个模型、用多大的推理强度。
 */
export interface ModelSelection {
    ref: ModelRef;
    /**
     * ReasoningEffort 是选的推理强度；为空时用模型的默认强度。
     */
    reasoning_effort?: string;
}

// From runtimeprotocol/types.go
/**
 * NotificationTarget 是单对话通知的定位字段：对话、事件所属的 Session，以及通知来自哪种订阅；
 * 对话订阅的 subscription/closed 中 Session 为空。
 */
export interface NotificationTarget {
    conversation_id: ConversationID;
    session_id: SessionID;
    /**
     * Subscription 是 SubscriptionKindSession 或 SubscriptionKindConversation；客户端据此把通知分别交给内容与状态显示。
     */
    subscription: SubscriptionKind;
}

// From runtimeprotocol/types.go
/**
 * OpenConversationParams 是 conversation/open 的参数；Executor 设置取记录里的值，对话已在运行时忽略 environment，沿用首次打开时的。
 */
export interface OpenConversationParams {
    conversation_id: ConversationID;
    environment?: string[];
}

// From defs/provider.go
/**
 * OptionalStringList 是可以不写的字符串列表：nil 表示没写、沿用默认，[] 表示明确为空。
 * 重新序列化成 YAML 或 JSON 时，nil 不写出这个字段，[] 写出空列表。
 */
export type OptionalStringList = string[];

// From storage/page.go
/**
 * Order 描述 Record 的排序：按什么、往哪个方向；零值表示按 Key 升序。
 */
export interface Order {
    by?: OrderBy;
    direction?: Direction;
}

// From storage/page.go
/**
 * OrderBy 是 Record 的排序依据。
 */
export type OrderBy = "key" | "updated_at";

// From storage/log.go
/**
 * Ordinal 是 Stream 内从 1 开始、由 LogStorage 分配的严格顺序；0 只用于初始游标。
 */
export type Ordinal = number;

// From session/types.go
/**
 * PendingInput 是尚未交给 Turn 的一条用户输入的轻量展示内容。
 */
export interface PendingInput {
    item_id: ItemID;
    preview: string;
    image_count: number;
    is_steer: boolean;
}

// From runtimeprotocol/protocol.go
/**
 * ProtocolVersion 是 OpenVaala 协议版本，initialize 双方必须一致。
 */
export const ProtocolVersion = "6";

// From defs/provider.go
/**
 * ProviderConfig 是一个模型服务（models/<id>.yaml）：地址、凭证、默认协议和模型。
 */
export interface ProviderConfig {
    id: string;
    /**
     * Protocol 是模型没写 protocol 时用的协议：openai_chat、openai_responses 或 claude；省略为 openai_responses。
     */
    protocol?: string;
    /**
     * BaseURL 带版本前缀，例如 https://api.openai.com/v1；各协议默认在后面拼 /chat/completions、/responses、/messages。
     */
    base_url: string;
    /**
     * Endpoints 按协议写完整请求地址，写了的协议不再用 BaseURL 拼，可以是别的 host。
     */
    endpoints?: Record<string, string>;
    api_key: string;
    /**
     * Headers 是只发给这个 Provider 的 header 模板，同名覆盖 http.headers；值为 "" 表示不发。
     */
    headers?: Record<string, string>;
    /**
     * Models 是模型 id 到模型配置的映射；值为 nil 时全部用默认。
     */
    models: Record<string, ProviderModelConfig | null> | null;
}

// From runtimeprotocol/types.go
/**
 * ProviderIDParams 是 provider/delete 的参数（配置里的 Provider id），也是 catalog/models/list 的参数（模型目录里的服务 id）。
 */
export interface ProviderIDParams {
    provider_id: string;
}

// From defs/provider.go
/**
 * ProviderModelCapabilities 是模型配置里的 capabilities 段。
 */
export interface ProviderModelCapabilities {
    /**
     * Input 为 nil 时取 config.yaml 的 models.defaults.capabilities.input。
     */
    input?: OptionalStringList;
}

// From defs/provider.go
/**
 * ProviderModelConfig 将本地模型别名映射为上游模型名，并给出请求参数；省略的字段取 config.yaml 的 models.defaults。
 */
export interface ProviderModelConfig {
    /**
     * Protocol 省略时用 Provider 的 protocol。
     */
    protocol?: string;
    upstream_model?: string;
    /**
     * DisplayName 省略时用模型 id。
     */
    display_name?: string;
    context_window?: number;
    max_output_tokens?: number;
    thinking?: boolean;
    /**
     * ReasoningEfforts 为 nil 时继承全局默认，[] 表示明确不支持选择。
     */
    reasoning_efforts?: OptionalStringList;
    default_reasoning_effort?: string;
    /**
     * MergeMessageRoles 为 nil 时继承全局默认，[] 表示明确不合并。
     */
    merge_message_roles?: OptionalStringList;
    /**
     * ReasoningContext 是请求要求保留的思考上下文，可写 all_turns 或 current_turn；省略时请求不发送、
     * 服务端按默认处理，不从 models.defaults 继承，只用于 openai-responses 协议。
     */
    reasoning_context?: string;
    capabilities?: ProviderModelCapabilities;
}

// From runtimeprotocol/types.go
/**
 * ProviderModelsResult 是 catalog/models/list、provider/fetchModels 的结果：模型 id 到模型配置。
 */
export interface ProviderModelsResult {
    models: Record<string, ProviderModelConfig | null> | null;
}

// From runtimeprotocol/types.go
/**
 * ProviderParams 是 provider/fetchModels 的参数：完整的 Provider。
 */
export interface ProviderParams {
    provider: ProviderConfig;
}

// From defs/reasoning.go
/**
 * Reasoning 是一段模型推理：正文、摘要、加密内容和完成状态。
 */
export interface Reasoning {
    status: ReasoningStatus;
    summary?: string[];
    content?: string[];
    encrypted?: string;
    raw_json?: unknown;
}

// From defs/reasoning.go
/**
 * ReasoningStatus 是推理内容已经完成还是被中断。
 */
export type ReasoningStatus = "completed" | "interrupted";

// From dao/dao.go
/**
 * RecordCursor 是对话、Session、Turn 列表的翻页位置；零值从头开始。
 */
export interface RecordCursor<ID extends string> {
    /**
     * ID 是上一页最后一条记录的 ID；为空时从头开始。
     */
    id: ID;
    /**
     * UpdatedAt 是上一页最后一条记录的更新时间；按 Key 排序时不用。
     */
    updated_at?: number;
}

// From runtimeprotocol/types.go
/**
 * ResolveDirectoryParams 是 executor/resolveDirectory 的参数；base_directory 为空时相对路径拼在这个 Executor 的默认根目录后面。
 */
export interface ResolveDirectoryParams {
    executor_id: string;
    directory: string;
    base_directory?: string;
}

// From runtimeprotocol/types.go
/**
 * ResolveDirectoryResult 是 executor/resolveDirectory 的结果：解析后的绝对目录。
 */
export interface ResolveDirectoryResult {
    workspace_directory: string;
}

// From runtime/types.go
/**
 * RunningConversations 列出至少有一个 Session 正在执行 Turn 的对话。
 */
export interface RunningConversations {
    conversation_ids: ConversationID[];
}

// From runtimeprotocol/types.go
/**
 * RunningConversationsClosedParams 由连接在全局运行订阅非主动关闭后发出，携带关闭原因。
 */
export interface RunningConversationsClosedParams {
    reason: string;
}

// From runtimeprotocol/types.go
/**
 * ServerInfo 是服务端名称。
 */
export interface ServerInfo {
    name: string;
}

// From defs/session.go
/**
 * Session 是一个 Agent 在对话里的一段上下文：所属对话、用哪个 Agent 和模型；子 Agent 还记着是谁创建的。
 */
export interface Session {
    id: SessionID;
    /**
     * UpdatedAt 是最后一次写入的 Unix 毫秒，由存储在写入时分配；写入时设置的值不会被保存。
     */
    updated_at?: number;
    conversation_id: ConversationID;
    agent_name: string;
    /**
     * TaskName 是 spawn_agent 给子 Agent 起的任务名；根 Session 为空。
     */
    task_name?: string;
    /**
     * ParentSessionID、ParentTurnID、ParentCallID 是创建这个子 Agent 的父 Session、Turn 和 spawn_agent 调用；根 Session 为空。
     */
    parent_session_id?: SessionID;
    parent_turn_id?: TurnID;
    parent_call_id?: string;
    /**
     * Model 是解析后的模型和推理强度，新建或切换模型时写入；零值按 Agent 默认模型处理。
     */
    model?: ModelSelection;
}

// From runtime/types.go
/**
 * SessionContextUsage 是一个 Session 的上下文占用详情，由已打开对话的 Session 记录、测量和模型调用信息整理。
 */
export interface SessionContextUsage {
    /**
     * Window 是下一次请求所用模型的上下文窗口；0 表示窗口未知。
     */
    window: number;
    /**
     * ThresholdTokens 是自动压缩阈值，按下一模型的窗口和最大输出计算；还没有测量时为 0。
     */
    threshold_tokens: number;
    /**
     * NextRequest 与同一时刻的 SessionRuntimeInfo.Context 相同。
     */
    next_request: ContextUsage;
    /**
     * LastRequest 在还没有上报过用量的普通调用时为 nil。
     */
    last_request?: LastRequestUsage;
    /**
     * SystemTokens、ToolTokens 是最近一次测量中系统提示词和工具定义的占用；还没有测量时为 0。
     */
    system_tokens: number;
    tool_tokens: number;
    /**
     * Items 是本轮最近一次统一测量的 Item 分类估算；还没有测量时为空。
     */
    // external type "github.com/VaalaCat/openvaala/pkg/agent/agent_context.ItemTokens", to include this type the package must be explicitly included in the parsing
    items: unknown[];
}

// From conversation/types.go
/**
 * SessionDetails 保存对话里一个 Session 的记录、模型、Executor、Skill 和工具信息。
 */
export interface SessionDetails {
    conversation_id: ConversationID;
    /**
     * Title 由 Runtime 按已保存的对话记录填写，GetSessionDetails 留空。
     */
    title: string;
    session: Session;
    /**
     * Model 是 Session 使用的模型定义（不含连接参数）；所选模型不在模型目录里时为空。
     */
    model: ModelDefinition | null;
    /**
     * CompactModel 是压缩使用的模型，Agent 没有单独配置时与 Model 相同；Model 为空时它也为空。
     */
    compact_model: ModelDefinition | null;
    /**
     * Executors 按配置顺序，第一个是默认 Executor。
     */
    executors: ExecutorDetails[];
    /**
     * Skills 是这个 Agent 可用的 Skill 名。
     */
    skills: string[];
    /**
     * Tools 是模型可见的工具名，包括协作工具。
     */
    tools: string[];
}

// From defs/item.go
/**
 * SessionFork 是日志里的一条历史分界，按 SourceConversationID 分两种：
 * 为空时是 spawn_agent 写在复制来的历史末尾的分界，后面的 Item 是子 Agent 自己的；
 * 不为空时是用户分叉写在新根 Session 日志第 1 行的接续标记，读历史时前面接上来源对话根 Session 的内容。
 */
export interface SessionFork {
    /**
     * SourceSessionID 是历史来源的 Session。
     */
    source_session_id: SessionID;
    /**
     * SourceConversationID 是用户分叉的来源对话；spawn_agent 写的分界为空。
     */
    source_conversation_id?: ConversationID;
    /**
     * SourceEndOrdinal 是来源日志里第一条复制过来的 Item 的序号（来源对外的序号）；接上的部分是 [1, SourceEndOrdinal)。
     */
    source_end_ordinal?: number;
}

// From defs/id.go
/**
 * SessionID 唯一标识一个 Session，也就是一个 Agent 在对话里的一段上下文，并按创建时间排序。
 */
export type SessionID = string;

// From session/types.go
/**
 * SessionInputMode 是一条输入的提交方式。Agent 发来的消息不插队，没挂上活动 Turn 的都排进 Agent 消息队列。
 */
export type SessionInputMode = "idle_only" | "interrupt" | "normal" | "steer";

// From runtimeprotocol/types.go
/**
 * SessionParams 用于定位对话内一个 Session 的方法：session/subscribe、session/unsubscribe、session/contextUsage、session/details。
 */
export interface SessionParams {
    conversation_id: ConversationID;
    session_id: SessionID;
}

// From session/types.go
/**
 * SessionPendingInputs 是待处理用户输入及仍接收补充输入的轮。
 */
export interface SessionPendingInputs {
    steer_turn_id?: TurnID;
    inputs: PendingInput[];
}

// From runtimeprotocol/types.go
/**
 * SessionPendingInputsUpdatedParams 由连接在收到待处理列表变化时发出。
 */
export interface SessionPendingInputsUpdatedParams extends NotificationTarget {
    pending_inputs: SessionPendingInputs;
}

// From runtimeprotocol/types.go
/**
 * SessionResult 是返回一条 Session 记录的结果。
 */
export interface SessionResult {
    session: Session;
}

// From runtime/types.go
/**
 * SessionRuntimeInfo 是一个 Session 的运行信息，可直接用 == 比较；runtimeConversation 在它有变化时发给对话订阅者，
 * 例如 Session 启停、Turn 开始或结束、用量和上下文占用变化、换模型。
 */
export interface SessionRuntimeInfo {
    session: Session;
    state: SessionState;
    active_turn_id?: TurnID;
    last_turn_id?: TurnID;
    last_turn_status?: TurnStatus;
    /**
     * ActiveTurnModel 是活动 Turn 使用的模型和推理强度；没有活动 Turn 时为零值。所选模型见 Session.Model。
     */
    active_turn_model?: ModelSelection;
    /**
     * Context 是下一次请求的预计占用。
     */
    context: ContextUsage;
    /**
     * Usage 是累计用量：对话加载时已保存 Turn 的用量之和，加上之后各次模型调用上报的用量。
     */
    usage: TokenUsage;
}

// From runtimeprotocol/types.go
/**
 * SessionStartedParams 由连接在收到 Session 启动事件时发出，携带 Session 记录和启动前读到的最新日志序号。
 */
export interface SessionStartedParams extends NotificationTarget {
    session: Session;
    last_saved_ordinal: Ordinal;
}

// From session/types.go
/**
 * SessionState 是 Session 在查询时所处的阶段。
 */
export type SessionState = "idle" | "running" | "stopped" | "stopping";

// From runtimeprotocol/types.go
/**
 * SessionStatusUpdatedParams 由连接在收到 Session 运行信息变化时发出，携带变化后的完整信息。
 */
export interface SessionStatusUpdatedParams extends NotificationTarget {
    info: SessionRuntimeInfo;
}

// From runtimeprotocol/types.go
/**
 * SessionStoppedParams 由连接在 Session.Run 返回后发出，error 是返回的错误文本。
 */
export interface SessionStoppedParams extends NotificationTarget {
    error?: string;
}

// From runtimeprotocol/types.go
/**
 * SetClientMCPServersParams 是 conversation/setClientMCPServers 的参数：server_ids 是本连接为这个对话登记的全部客户端 MCP server，
 * 换掉本连接之前为这个对话登记的；空列表表示全部移除。server 在客户端机器上运行，serve 只经过本连接转发它的 MCP 消息。
 */
export interface SetClientMCPServersParams {
    conversation_id: ConversationID;
    server_ids: string[];
}

// From runtimeprotocol/types.go
/**
 * SetConversationArchivedParams 是 conversation/setArchived 的参数：archived 为 true 时归档，false 时取消归档。
 */
export interface SetConversationArchivedParams {
    conversation_id: ConversationID;
    archived: boolean;
}

// From runtimeprotocol/types.go
/**
 * SetConversationTitleParams 是 conversation/setTitle 的参数。
 */
export interface SetConversationTitleParams {
    conversation_id: ConversationID;
    title: string;
}

// From runtimeprotocol/types.go
/**
 * SetDefaultModelParams 是 config/setDefaultModel 的参数：model 写成 provider/model。
 */
export interface SetDefaultModelParams {
    model: string;
}

// From runtime/types.go
/**
 * SetDefinitionsResult 是 SetDefinitions 返回的结果，列出这次没换上新定义的对话。
 */
export interface SetDefinitionsResult {
    /**
     * BusyConversationIDs 是正在执行 Turn 或有后台命令没结束、这次没有换上新定义的对话；它们空闲后再 reload 才用上新定义。
     */
    busy_conversation_ids: ConversationID[];
}

// From runtimeprotocol/types.go
/**
 * SetExecutorSettingsParams 是 conversation/setExecutorSettings 的参数；executor_settings 是完整的新设置，不是增量。
 */
export interface SetExecutorSettingsParams {
    conversation_id: ConversationID;
    executor_settings: ExecutorSettings;
}

// From runtimeprotocol/types.go
/**
 * SetProviderParams 是 provider/set 的参数：完整的 Provider，以及可选的、同时写进配置的默认模型（provider/model）。
 */
export interface SetProviderParams {
    provider: ProviderConfig;
    default_model?: string;
}

// From runtimeprotocol/types.go
/**
 * SetSessionModelParams 是 session/setModel 的参数。
 */
export interface SetSessionModelParams {
    conversation_id: ConversationID;
    session_id: SessionID;
    model: ModelSelection;
}

// From skill/skill.go
/**
 * SkillInfo 是补全列表的显示信息，不含正文和文件读取依赖。
 */
export interface SkillInfo {
    name: string;
    description: string;
    path: string;
    executor: string;
}

// From defs/item.go
/**
 * SkillInstruction 是用户在一条输入里点名的一个 Skill：名字、所在 Executor、文件路径和正文。
 */
export interface SkillInstruction {
    /**
     * InputItemID 是点名这个 Skill 的用户输入 Item。
     */
    input_item_id: ItemID;
    name: string;
    path: string;
    executor: string;
    text: string;
}

// From runtimeprotocol/types.go
/**
 * SteerInputParams 是 conversation/steerInput 的参数。
 */
export interface SteerInputParams {
    conversation_id: ConversationID;
    item_id: ItemID;
    turn_id: TurnID;
}

// From runtimeprotocol/types.go
/**
 * StreamingItem 是一个生成中的 Item 及它在本轮 Agent 输出中的位置，与通知的 item_index 对应。
 */
export interface StreamingItem {
    item_index: number;
    item: Item | null;
}

// From defs/message.go
/**
 * SubagentNotification 是子 Agent 的 Turn 结束并保存终态后发给父 Agent 的通知。
 */
export interface SubagentNotification {
    /**
     * SourceTurnID 是子 Agent 结束的那个 Turn。
     */
    source_turn_id: TurnID;
    /**
     * Status 是那个 Turn 保存的终态。
     */
    status: TurnStatus;
}

// From runtimeprotocol/types.go
/**
 * SubmitInputParams 是 conversation/submitInput 的参数。
 */
export interface SubmitInputParams {
    conversation_id: ConversationID;
    input_id?: InputID;
    /**
     * Mode 省略时使用普通模式。
     */
    mode?: SessionInputMode;
    /**
     * Content 原样交给 Runtime；Session 拒绝空内容、空条目、未知类型和缺少载荷的内容，图片的 data URL 格式不检查。
     */
    content: (Content | null)[];
}

// From runtimeprotocol/types.go
/**
 * SubmitInputResult 是 conversation/submitInput 的结果：这次输入的 ItemID。
 */
export interface SubmitInputResult {
    item_id: ItemID;
}

// From runtimeprotocol/types.go
/**
 * SubscribeConversationResult 是对话订阅的初始信息：登记时已保存的标题与全部 Session 的运行信息，按 SessionID 排序。
 */
export interface SubscribeConversationResult {
    title: string;
    sessions: SessionRuntimeInfo[];
}

// From runtimeprotocol/types.go
/**
 * SubscribeSessionResult 是 Session 内容订阅的初始信息。
 */
export interface SubscribeSessionResult {
    /**
     * LastSavedOrdinal 及之前的日志用 session/items/list 补齐。
     */
    last_saved_ordinal: Ordinal;
    /**
     * StreamingItems 是正在生成的 Item，后续 item/delta 按 item_index 接上。
     */
    streaming_items: StreamingItem[];
    pending_inputs: SessionPendingInputs;
}

// From runtimeprotocol/types.go
/**
 * SubscriptionClosedParams 由连接在订阅事件流关闭后发出，携带退订、过慢、对话退出、Runtime 停止或配置更新的原因；
 * 原因是配置更新时，客户端重新订阅并同步。
 */
export interface SubscriptionClosedParams extends NotificationTarget {
    reason: string;
}

// From runtimeprotocol/types.go
export type SubscriptionKind = "conversation" | "session";

// From runtimeprotocol/types.go
/**
 * TerminalAttachParams 是 terminal/attach 的参数：把这条连接接入已有终端。
 */
export interface TerminalAttachParams {
    terminal_id: string;
    /**
     * Cursor 是开始发送输出的字节位置，刷新后传 0。
     */
    cursor: number;
}

// From runtimeprotocol/types.go
/**
 * TerminalAttachResult 是 terminal/attach 的结果：终端的 Executor、起始目录和 PTY 当前尺寸。
 */
export interface TerminalAttachResult {
    executor_id: string;
    directory: string;
    /**
     * Rows、Cols 是 PTY 当前尺寸，客户端先按它回放。
     */
    rows: number;
    cols: number;
}

// From runtimeprotocol/types.go
/**
 * TerminalExitedParams 是 serve 发给接入该终端的连接的 terminal/exited 的参数：终端进程结束后发出一次，此后这个 terminal_id 不再有通知。
 */
export interface TerminalExitedParams {
    terminal_id: string;
    exit_code: number;
    /**
     * Error 是进程没能取得退出码的原因（如远端连接断开），正常结束或被关闭时省略。
     */
    error?: string;
}

// From runtimeprotocol/types.go
/**
 * TerminalInputParams 是客户端发给 serve 的 terminal/input 的参数：data 原样写进终端，JSON 里是 base64。
 */
export interface TerminalInputParams {
    terminal_id: string;
    data: string;
}

// From runtimeprotocol/types.go
/**
 * TerminalItem 是一个运行中的终端。
 */
export interface TerminalItem {
    terminal_id: string;
    executor_id: string;
    directory: string;
    started_at: string;
    /**
     * ConversationID 是打开终端的对话，打开时没有指定对话则省略。
     */
    conversation_id?: ConversationID;
}

// From runtimeprotocol/types.go
/**
 * TerminalListResult 是 terminal/subscribe 的结果，也是 terminal/listChanged 的参数：全部运行中的终端。
 */
export interface TerminalListResult {
    terminals: TerminalItem[];
}

// From runtimeprotocol/types.go
/**
 * TerminalOpenParams 是 terminal/open 的参数：在 executor_id 对应的 Executor 上新开一个终端，shell 的起始目录是 directory；打开后这条连接自动接入。
 */
export interface TerminalOpenParams {
    /**
     * ExecutorID 为空时用 local Executor；virtual Executor 不支持终端。
     */
    executor_id: string;
    /**
     * Directory 为空时用该 Executor 的默认工作目录；相对路径接在默认工作目录后面，~ 按 Executor 所在机器的主目录展开。
     */
    directory: string;
    /**
     * Rows、Cols 是终端初始行列数，为零时按 24×80。
     */
    rows: number;
    cols: number;
    /**
     * Environment 是客户端的环境变量（格式同 os.Environ），只给 local Executor 的 shell 用；为空时继承 serve 进程的环境。
     */
    environment?: string[];
    /**
     * ConversationID 是打开终端的对话，只决定标签页显示在哪。
     */
    conversation_id?: ConversationID;
}

// From runtimeprotocol/types.go
/**
 * TerminalOpenResult 是 terminal/open 的结果：终端 ID，以及实际使用的 Executor 和已解析的绝对起始目录。
 */
export interface TerminalOpenResult {
    terminal_id: string;
    executor_id: string;
    directory: string;
}

// From runtimeprotocol/types.go
/**
 * TerminalOutputParams 是 serve 发给接入该终端的连接的 terminal/output 的参数：一段输出，JSON 里是 base64，按产生顺序送达。
 */
export interface TerminalOutputParams {
    terminal_id: string;
    data: string;
    /**
     * NextCursor 是这段输出之后的位置，断网重连从这里续读。
     */
    next_cursor: number;
    /**
     * Truncated 请求的位置已被丢弃，Data 从最早保留处开始？
     */
    truncated: boolean;
    /**
     * HasMore 接入时已有的输出还没发完？为 false 时客户端结束回放；之后的新输出都是 false。
     */
    has_more: boolean;
}

// From runtimeprotocol/types.go
/**
 * TerminalParams 是 terminal/close、terminal/reset 的参数：指定一个终端。
 */
export interface TerminalParams {
    terminal_id: string;
}

// From runtimeprotocol/types.go
/**
 * TerminalResizeParams 是客户端发给 serve 的 terminal/resize 的参数：修改终端的行列数。
 */
export interface TerminalResizeParams {
    terminal_id: string;
    rows: number;
    cols: number;
}

// From runtimeprotocol/types.go
/**
 * TerminalsClosedParams 是 serve 发给订阅了终端列表的连接的 terminal/listClosed 的参数：订阅被 serve 关闭的原因，主动退订和断线不发。
 */
export interface TerminalsClosedParams {
    reason: string;
}

// From defs/message.go
/**
 * TextContent 是一段文字。
 */
export interface TextContent {
    text: string;
    /**
     * IsContext 是不是附带材料？附带材料不参与用户命令解析。
     */
    is_context?: boolean;
}

// From defs/turn.go
/**
 * TokenUsage 是一次或多次模型调用的 token 数，各家的统计方式已经统一：Input 包含缓存命中和缓存写入，
 * CacheRead 和 CacheWrite 是 Input 的一部分，Reasoning 是 Output 的一部分。
 */
export interface TokenUsage {
    input: number;
    output: number;
    cache_read: number;
    cache_write: number;
    reasoning: number;
}

// From defs/toolcall.go
/**
 * ToolCall 是模型发出的一次工具调用及参数。
 */
export interface ToolCall {
    call_id: string;
    name: string;
    arguments: string;
    /**
     * Status 为空时参数还在生成。
     */
    status?: ToolCallStatus;
    /**
     * ProviderData 是模型厂商附在这次调用上的签名等数据；下一次请求原样还给 llmkit，否则 Gemini 等会拒绝或丢失签名。
     */
    // external type "github.com/VaalaCat/ai-gateway/pkg/llmkit/ir.ToolCallProviderData", to include this type the package must be explicitly included in the parsing
    provider_data?: unknown;
}

// From defs/toolcall.go
/**
 * ToolCallStatus 是工具调用参数已经完整还是被中断。
 */
export type ToolCallStatus = "interrupted" | "ready";

// From defs/toolcall.go
/**
 * ToolError 是工具执行失败时的错误码和消息。
 */
export interface ToolError {
    code: string;
    message: string;
}

// From defs/toolcall.go
/**
 * ToolResult 是一次工具调用的结果，用 CallID 对应模型发出的那次调用。
 */
export interface ToolResult {
    call_id?: string;
    /**
     * Executor 是实际执行这次调用的 Executor；Agent 侧工具（spawn_agent 等）和没能分派的调用为空。
     */
    executor?: string;
    content?: unknown;
    /**
     * Images 是工具读到、要给模型看的图片；Alt 写模型传入的路径。发给模型时放进这串工具结果之后的一条用户消息。
     */
    images?: (ImageContent | null)[];
    error?: ToolError;
    /**
     * Truncated 工具输出是否超长被截断了？
     */
    truncated?: boolean;
}

// From defs/turn.go
/**
 * Turn 是一轮 Agent 执行的记录：状态、错误、Item 范围、用量，以及用到的模型和 Executor。
 */
export interface Turn {
    id: TurnID;
    /**
     * UpdatedAt 是最后一次写入的 Unix 毫秒，由存储在写入时分配；写入时设置的值不会被保存。
     */
    updated_at?: number;
    /**
     * ConversationID 是所属对话；DAO 据此找到对话目录里的 metadata.jsonl。
     */
    conversation_id: ConversationID;
    session_id: SessionID;
    status: TurnStatus;
    /**
     * FinalModelCallID 是本轮最后一次成功的普通模型调用；只在 completed 时填写。
     */
    final_model_call_id?: ModelCallID;
    /**
     * Error 是 Turn 失败或被中断时的错误说明。
     */
    error?: string;
    items_range?: TurnItemRange;
    /**
     * Usage 是本 Turn 全部模型调用（包括压缩）的用量合计，写入终态时填写；running 时为零。
     */
    usage?: TokenUsage;
    /**
     * ContextUsage 是最近一次完整普通调用的报告；本轮没有完整普通调用时沿用上一轮的报告，模型切换后为空。
     */
    context_usage?: TurnContextUsage;
    /**
     * Model 是本 Turn 实际使用的模型和推理强度，创建 running 记录时写入。
     */
    model?: ModelSelection;
    /**
     * Executors 是本 Turn 开始时对话可用的 Executor，按顺序，第一个是默认；创建 running 记录时写入。
     */
    executors?: TurnExecutor[];
}

// From defs/turn.go
/**
 * TurnContextUsage 保存最近一次完整普通调用的用量和原输入位置；本轮没有完整普通调用时沿用上一轮的报告。
 */
export interface TurnContextUsage {
    model_call_id: ModelCallID;
    /**
     * InputThroughItemID 是请求发出前上下文最后一条 Item，不含该次生成输出。
     */
    input_through_item_id: ItemID;
    /**
     * Usage 是服务端上报并已归一化的用量；没有上报时为零值。
     */
    usage?: TokenUsage;
    // this is likely an enum in an external package "github.com/VaalaCat/ai-gateway/pkg/llmkit/ir.ReasoningContext"
    reasoning_context?: string;
}

// From defs/turn.go
/**
 * TurnExecutor 是一个 Executor 和它在对话里的根目录。
 */
export interface TurnExecutor {
    id: string;
    workspace_directory: string;
}

// From runtimeprotocol/types.go
/**
 * TurnFinishedParams 由连接在 Turn 正常结束、失败或中断后发出；status 只在终态已保存时填写，输出另由 Item 通知发送。
 */
export interface TurnFinishedParams extends NotificationTarget {
    turn_id: TurnID;
    status?: TurnStatus;
    input_item_ids: ItemID[];
    final_model_call_id?: ModelCallID;
    error?: string;
}

// From defs/id.go
/**
 * TurnID 唯一标识一轮 Agent 执行，并按创建时间排序。
 */
export type TurnID = string;

// From defs/turn.go
/**
 * TurnItemRange 是一轮已保存 Item 的对外序号范围，两端都包含；起点为零表示还没有 Item，运行中终点为零。
 */
export interface TurnItemRange {
    first_ordinal: number;
    last_ordinal: number;
}

// From runtimeprotocol/types.go
/**
 * TurnStartedParams 由连接在 Session 开始执行 Turn 时发出，携带 TurnID。
 */
export interface TurnStartedParams extends NotificationTarget {
    turn_id: TurnID;
}

// From defs/turn.go
/**
 * TurnStatus 是一轮执行正在运行还是以什么方式结束。
 */
export type TurnStatus = "completed" | "failed" | "interrupted" | "running";

// From runtimeprotocol/types.go
/**
 * WithdrawInputParams 是 conversation/withdrawInput 的参数。
 */
export interface WithdrawInputParams {
    conversation_id: ConversationID;
    item_id: ItemID;
    return_content: boolean;
}

// From runtimeprotocol/types.go
/**
 * WithdrawInputResult 是取回的用户消息内容；撤回时不返回内容。
 */
export interface WithdrawInputResult {
    content?: (Content | null)[];
}

// From conversation/types.go
/**
 * WorkspaceDirectories 是 Executor 上解析后的目录及其普通直接子目录名称。
 */
export interface WorkspaceDirectories {
    directory: string;
    directories: string[];
}

/**
 * 每个请求方法的参数类型。
 */
export interface MethodParams {
    "initialize": InitializeParams;
    "conversation/create": CreateConversationParams;
    "conversation/open": OpenConversationParams;
    "conversation/read": ConversationParams;
    "conversation/list": ListConversationsParams;
    "conversation/setTitle": SetConversationTitleParams;
    "conversation/fork": ForkConversationParams;
    "conversation/setArchived": SetConversationArchivedParams;
    "conversation/delete": ConversationParams;
    "conversation/setExecutorSettings": SetExecutorSettingsParams;
    "conversation/submitInput": SubmitInputParams;
    "conversation/steerInput": SteerInputParams;
    "conversation/interruptTurnWithInput": InterruptTurnWithInputParams;
    "conversation/withdrawInput": WithdrawInputParams;
    "conversation/appendContextItems": AppendContextItemsParams;
    "conversation/interruptTurn": ConversationParams;
    "conversation/compact": ConversationParams;
    "session/get": GetSessionParams;
    "session/list": ListSessionsParams;
    "session/turns/list": ListTurnsParams;
    "session/turns/get": GetTurnsParams;
    "session/items/list": ListItemsParams;
    "session/contextUsage": SessionParams;
    "session/details": SessionParams;
    "session/setModel": SetSessionModelParams;
    "model/list": ListModelsParams;
    "agent/list": EmptyParams;
    "executor/list": EmptyParams;
    "executor/resolveDirectory": ResolveDirectoryParams;
    "executor/listDirectories": ListDirectoriesParams;
    "provider/list": EmptyParams;
    "provider/set": SetProviderParams;
    "provider/delete": ProviderIDParams;
    "provider/fetchModels": ProviderParams;
    "config/setDefaultModel": SetDefaultModelParams;
    "config/reload": EmptyParams;
    "catalog/providers/list": EmptyParams;
    "catalog/models/list": ProviderIDParams;
    "skill/list": ListSkillsParams;
    "hook/list": EmptyParams;
    "mcpServer/list": ConversationParams;
    "conversation/setClientMCPServers": SetClientMCPServersParams;
    "conversation/subscribe": ConversationParams;
    "conversation/unsubscribe": ConversationParams;
    "conversation/running/subscribe": EmptyParams;
    "conversation/running/unsubscribe": EmptyParams;
    "session/subscribe": SessionParams;
    "session/unsubscribe": SessionParams;
    "terminal/open": TerminalOpenParams;
    "terminal/attach": TerminalAttachParams;
    "terminal/close": TerminalParams;
    "terminal/reset": TerminalParams;
    "terminal/subscribe": EmptyParams;
    "terminal/unsubscribe": EmptyParams;
}

/**
 * 每个请求方法的结果类型。
 */
export interface MethodResult {
    "initialize": InitializeResult;
    "conversation/create": ConversationResult;
    "conversation/open": ConversationResult;
    "conversation/read": ConversationResult;
    "conversation/list": ListConversationsResult;
    "conversation/setTitle": EmptyResult;
    "conversation/fork": ConversationResult;
    "conversation/setArchived": ConversationResult;
    "conversation/delete": EmptyResult;
    "conversation/setExecutorSettings": ConversationResult;
    "conversation/submitInput": SubmitInputResult;
    "conversation/steerInput": EmptyResult;
    "conversation/interruptTurnWithInput": EmptyResult;
    "conversation/withdrawInput": WithdrawInputResult;
    "conversation/appendContextItems": EmptyResult;
    "conversation/interruptTurn": InterruptTurnResult;
    "conversation/compact": CompactContextResult;
    "session/get": SessionResult;
    "session/list": ListSessionsResult;
    "session/turns/list": ListTurnsResult;
    "session/turns/get": GetTurnsResult;
    "session/items/list": ListItemsResult;
    "session/contextUsage": SessionContextUsage;
    "session/details": SessionDetails;
    "session/setModel": SessionResult;
    "model/list": ModelList;
    "agent/list": ListAgentsResult;
    "executor/list": ListExecutorsResult;
    "executor/resolveDirectory": ResolveDirectoryResult;
    "executor/listDirectories": WorkspaceDirectories;
    "provider/list": ListProvidersResult;
    "provider/set": SetDefinitionsResult;
    "provider/delete": SetDefinitionsResult;
    "provider/fetchModels": ProviderModelsResult;
    "config/setDefaultModel": SetDefinitionsResult;
    "config/reload": SetDefinitionsResult;
    "catalog/providers/list": ListCatalogProvidersResult;
    "catalog/models/list": ProviderModelsResult;
    "skill/list": SkillInfo[];
    "hook/list": ListHooksResult;
    "mcpServer/list": ListMCPServersResult;
    "conversation/setClientMCPServers": EmptyResult;
    "conversation/subscribe": SubscribeConversationResult;
    "conversation/unsubscribe": EmptyResult;
    "conversation/running/subscribe": RunningConversations;
    "conversation/running/unsubscribe": EmptyResult;
    "session/subscribe": SubscribeSessionResult;
    "session/unsubscribe": EmptyResult;
    "terminal/open": TerminalOpenResult;
    "terminal/attach": TerminalAttachResult;
    "terminal/close": EmptyResult;
    "terminal/reset": EmptyResult;
    "terminal/subscribe": TerminalListResult;
    "terminal/unsubscribe": EmptyResult;
}

/**
 * 每种服务端通知的参数类型。
 */
export interface NotificationParams {
    "conversation/runningChanged": RunningConversations;
    "conversation/runningClosed": RunningConversationsClosedParams;
    "subscription/closed": SubscriptionClosedParams;
    "session/pendingInputsUpdated": SessionPendingInputsUpdatedParams;
    "session/statusUpdated": SessionStatusUpdatedParams;
    "conversation/titleUpdated": ConversationTitleUpdatedParams;
    "session/started": SessionStartedParams;
    "session/stopped": SessionStoppedParams;
    "turn/started": TurnStartedParams;
    "turn/finished": TurnFinishedParams;
    "items/saved": ItemsSavedParams;
    "item/started": ItemEventParams;
    "item/delta": ItemEventParams;
    "item/completed": ItemEventParams;
    "item/interrupted": ItemEventParams;
    "item/toolProgress": ItemEventParams;
    "hook/started": HookRunParams;
    "hook/completed": HookRunParams;
    "clientMCP/connect": ClientMCPConnectParams;
    "clientMCP/message": ClientMCPMessageParams;
    "clientMCP/disconnect": ClientMCPDisconnectParams;
    "terminal/output": TerminalOutputParams;
    "terminal/exited": TerminalExitedParams;
    "terminal/listChanged": TerminalListResult;
    "terminal/listClosed": TerminalsClosedParams;
}

/**
 * 每种客户端通知的参数类型。
 */
export interface ClientNotificationParams {
    "clientMCP/message": ClientMCPMessageParams;
    "clientMCP/closed": ClientMCPClosedParams;
    "terminal/input": TerminalInputParams;
    "terminal/resize": TerminalResizeParams;
}

export const ErrorCodeParse = -32700;
export const ErrorCodeInvalidRequest = -32600;
export const ErrorCodeMethodNotFound = -32601;
export const ErrorCodeInvalidParams = -32602;
export const ErrorCodeRuntime = -32000;
export const ErrorCodeSessionBusy = -32001;
export const ErrorCodeConversationNotFound = -32002;
export const ErrorCodeSessionNotFound = -32003;
export const ErrorCodeTerminalNotFound = -32004;
