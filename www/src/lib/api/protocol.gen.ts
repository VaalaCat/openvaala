// 由 internal/protocolgen 生成，不要手改

// From worldserver/types.go
/**
 * ACLMatrix 以相同的节点顺序排列发送行和接收列。
 */
export interface ACLMatrix {
    nodes: ACLMatrixNode[];
    cells: ACLMatrixCell[][];
}

// From worldserver/types.go
export interface ACLMatrixCell {
    allowed: boolean;
    /**
     * RuleIndex 指向草稿或已保存列表中的规则，默认规则为 -1。
     */
    rule_index: number;
}

// From worldserver/types.go
export interface ACLMatrixNode {
    name: string;
    owner: string;
    tags: string[];
}

// From worldserver/types.go
/**
 * ACLRule 按 Priority 从小到大匹配，同优先级按提交顺序匹配。
 */
export interface ACLRule {
    priority: number;
    from: string[];
    to: string[];
    action: string;
}

// From defs/session.go
/**
 * ACPCommand 保存 ACP Agent 的启动命令、参数和环境变量。
 */
export interface ACPCommand {
    command: string;
    args?: string[];
    env?: Record<string, string>;
    /**
     * ParamOverwrite 按 ACP 方法名合并进驱动发出的请求。
     */
    // empty interface{} type, falling back to unknown
    param_overwrite?: Record<string, Record<string, unknown> | null>;
    /**
     * EphemeralParamOverwrite 只在临时会话叠加，非空表示支持不落盘的临时会话。
     */
    // empty interface{} type, falling back to unknown
    ephemeral_param_overwrite?: Record<string, Record<string, unknown> | null>;
}

// From worldserver/types.go
export interface AddDomainRequest {
    card: string;
}

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

// From worldserver/types.go
/**
 * AdminNode 将注册记录与当前在线公告合并。
 */
export interface AdminNode extends NodeRecord {
    domain: string;
    online: boolean;
    targets: AdvertisedTarget[];
}

// From worldserver/types.go
/**
 * AdvertisedTarget 描述节点通告的一个目标。
 */
export interface AdvertisedTarget {
    name: string;
    description?: string;
    visibility: string;
}

// From defs/session.go
/**
 * AgentConfig 是 agents/ 下一个 Agent 文件的内容；原生 default Agent 沿用未指定的内置字段。
 */
export interface AgentConfig {
    /**
     * Name 必须和文件名一致。
     */
    name: string;
    /**
     * Type 为空时使用原生 Agent。
     */
    type?: AgentType;
    /**
     * Description 是 Agent 的用途说明。
     */
    description?: string;
    /**
     * Instructions 是原生 Agent 的指令。
     */
    instructions?: string;
    /**
     * Model 是 Agent 的主模型；省略时加载后取默认模型，模型目录为空时保持为 nil。
     */
    model?: AgentModelConfig;
    /**
     * CompactModel 是生成上下文摘要使用的模型；省略时使用 Model。
     */
    compact_model?: AgentModelConfig;
    /**
     * Tools 是可用普通工具名，由使用方的工具目录解释；省略时可用全部普通工具。
     */
    tools?: string[];
    /**
     * Skills 是可用 skill 名，由使用方的 skill 目录解释；省略时可用全部 skill。
     */
    skills?: string[];
    /**
     * MCPServers 是可用的 MCP server id；省略时全部可用（包括客户端连接带来的 MCP server），[] 表示不用 MCP。
     */
    mcp_servers?: OptionalStringList;
    /**
     * MaxSteps 是一轮最多调用模型的次数；省略或 0 表示不限，不能为负。
     */
    max_steps?: number;
    /**
     * ACP 保存外部 ACP Agent 的启动命令。
     */
    acp?: ACPCommand;
}

// From runtime/types.go
/**
 * AgentInfo 是 ListAgents 返回的一个 Agent 的公开说明，不含系统指令。
 */
export interface AgentInfo {
    name: string;
    type: AgentType;
    description?: string;
    icon?: string;
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

// From defs/session.go
/**
 * AgentModelConfig 引用 Provider 中的模型别名。
 */
export interface AgentModelConfig {
    provider: string;
    model?: string;
}

// From runtimeprotocol/types.go
/**
 * AgentNameParams 指定 Agent 定义的文件名。
 */
export interface AgentNameParams {
    name: string;
}

// From runtime/types.go
export interface AgentOptionsRequest extends ExternalAgentLocation {
    selections?: ExternalOptionSelection[];
    refresh?: boolean;
}

// From agent/types.go
/**
 * AgentPreset 保存新增 ACP Agent 时可填充的配置。
 */
export interface AgentPreset {
    name: string;
    display_name: string;
    description: string;
    command: string;
    args?: string[];
    env?: Record<string, string>;
    // empty interface{} type, falling back to unknown
    ephemeral_param_overwrite?: Record<string, Record<string, unknown> | null>;
    icon: string;
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
     * 转出通知携带新入口，供等待这条消息的父会话直接跳转。
     */
    detached_conversation_id?: ConversationID;
    detached_session_id?: SessionID;
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

// From defs/session.go
/**
 * AgentType 标识 Agent 使用的执行类型。
 */
export type AgentType = "acp" | "native";

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

// From runtimeprotocol/types.go
/**
 * CallExecutorToolParams 在来源会话的执行环境中调用工具。
 */
export interface CallExecutorToolParams extends CollaborationSource {
    name: string;
    arguments?: unknown;
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
 * CollaborationSource 指定操作来源，使用会话令牌时由连接身份确定。
 */
export interface CollaborationSource {
    conversation_id: ConversationID;
    source_session_id?: SessionID;
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
 * Content 是消息里的一项内容，Kind 决定哪个字段有值。
 */
export interface Content {
    kind: ContentKind;
    text?: TextContent;
    image?: ImageContent;
    file?: FileRef;
}

// From defs/message.go
/**
 * ContentKind 区分文本、图片和文件附件。
 */
export type ContentKind = "file" | "image" | "text";

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
    initial_options?: ExternalOptionSelection[];
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

// From worldserver/types.go
/**
 * CreateJoinTokenRequest 指定入网码归属和过期时间，ExpiresAt 使用 Unix 毫秒。
 */
export interface CreateJoinTokenRequest {
    owner: string;
    tags: string[];
    /**
     * Reusable 入网码是否可供多个节点注册？
     */
    reusable: boolean;
    expires_at: number;
}

// From worldserver/types.go
/**
 * CreateJoinTokenResponse 在创建时一次性返回入网码明文。
 */
export interface CreateJoinTokenResponse extends JoinTokenInfo {
    token: string;
}

// From runtimeprotocol/types.go
/**
 * DeleteExecutorParams 是 executor/delete 的参数。
 */
export interface DeleteExecutorParams {
    executor_id: string;
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

// From defs/executor_config.go
/**
 * ExecutorConfig 是 executors/<id>.yaml 声明的额外 Executor 配置。
 */
export interface ExecutorConfig {
    id: string;
    /**
     * Kind 是 local（本机进程内执行）、virtual（go-bash 内存虚拟环境）或 ssh（经 SSH 访问远端机器）。
     */
    kind: ExecutorKind;
    /**
     * Description 是给模型看的说明，模型据此选 Executor。
     */
    description?: string;
    /**
     * WorkspaceDirectory 是内置工具的根目录：local 是本机目录，virtual 是虚拟文件系统里的路径，ssh 是远端绝对路径。
     */
    workspace_directory?: string;
    /**
     * SSH 是 kind 为 ssh 时的连接参数，其他 kind 不使用。
     */
    ssh?: SSHExecutorConfig;
}

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

// From runtime/types.go
/**
 * ExecutorExecRequest 指定执行机器、工作目录和逐项参数。
 */
export interface ExecutorExecRequest {
    conversation_id?: ConversationID;
    executor_id: string;
    cwd?: string;
    command: string;
    args?: string[];
}

// From runtime/types.go
export interface ExecutorExecResult {
    exit_code: number;
    stdout: string;
    stderr: string;
}

// From defs/executor.go
/**
 * ExecutorKind 是 Executor 的种类；自定义 Executor 可以用自己的种类。
 */
export type ExecutorKind = "client" | "local" | "memory" | "ssh" | "virtual";

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
 * ExecutorsChange 由 Turn 在本轮输入之前写入，通知模型首轮接入的动态 Executor 或与上一轮相比的环境变化。
 */
export interface ExecutorsChange {
    previous_executors: TurnExecutor[];
    executors: TurnExecutor[];
}

// From runtime/types.go
/**
 * ExternalAgentLocation 指定 Agent 定义以及外部进程使用的工作位置。
 */
export interface ExternalAgentLocation {
    agent_name: string;
    executor_id?: string;
    cwd?: string;
}

// From defs/session.go
/**
 * ExternalCapabilities 是外部会话公开给客户端的能力。
 */
export interface ExternalCapabilities {
    supports_steer: boolean;
    supports_ephemeral_session: boolean;
}

// From defs/session.go
/**
 * ExternalCommand 是外部后端上报的斜杠命令。
 */
export interface ExternalCommand {
    name: string;
    description?: string;
    input_hint?: string;
}

// From defs/session.go
/**
 * ExternalOptionChoice 是外部会话选项的一个可选值。
 */
export interface ExternalOptionChoice {
    value: string;
    label: string;
    description?: string;
}

// From defs/session.go
/**
 * ExternalOptionGroup 保存外部会话的一组可选项。
 */
export interface ExternalOptionGroup {
    /**
     * Kind 为 mode 或 config，对应后端设置选项的方法。
     */
    kind: string;
    id: string;
    label: string;
    current_value: string;
    options: ExternalOptionChoice[];
}

// From defs/session.go
/**
 * ExternalOptionSelection 指定要写入外部会话的选项值。
 */
export interface ExternalOptionSelection {
    kind: string;
    /**
     * ID 是后端的选项组 ID；Kind 为 model 时可省略。
     */
    id?: string;
    value: string;
}

// From defs/session.go
/**
 * ExternalOptions 保存外部会话上报的选项及其当前值。
 */
export interface ExternalOptions {
    mode?: ExternalOptionGroup;
    model?: ExternalOptionGroup;
    thought?: ExternalOptionGroup;
    others?: ExternalOptionGroup[];
}

// From defs/session.go
/**
 * ExternalSession 保存外部 Agent 会话的恢复信息。
 */
export interface ExternalSession {
    agent_type: AgentType;
    executor_id?: string;
    session_id?: string;
    cwd?: string;
    // empty interface{} type, falling back to unknown
    metadata?: Record<string, unknown>;
}

// From externalagent/types.go
/**
 * ExternalSessionInfo 是可导入会话的列表条目，更新时间为 Unix 毫秒。
 */
export interface ExternalSessionInfo {
    session_id: string;
    cwd: string;
    title: string;
    updated_at: number;
}

// From externalagent/types.go
/**
 * ExternalSessionPreview 保存预览中首末两条用户消息的正文。
 */
export interface ExternalSessionPreview {
    first_user_message: string;
    last_user_message: string;
}

// From runtime/types.go
export interface ExternalSessionRequest extends ExternalAgentLocation {
    external_session_id: string;
}

// From runtime/types.go
export interface ExternalSessionsRequest extends ExternalAgentLocation {
    query?: string;
    cursor?: string;
}

// From runtime/types.go
export interface ExternalSessionsResult {
    sessions: ExternalSessionInfo[];
    next_cursor?: string;
}

// From defs/message.go
/**
 * FileRef 记录已上传附件的元数据及其在目标 Executor 上的绝对路径。
 */
export interface FileRef {
    name: string;
    mime: string;
    size: number;
    path: string;
}

// From runtimeprotocol/types.go
/**
 * ForkConversationParams 是 conversation/fork 的参数。
 */
export interface ForkConversationParams {
    source_session_id?: SessionID;
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

// From world/types.go
export interface ForwardRequest {
    conversation_id: ConversationID;
    to: string;
    item_ids?: ItemID[];
    note?: string;
}

// From worldserver/types.go
export interface GetACLMatrixRequest {
    rules?: ACLRule[];
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
 * GetSkillParams 指定 Skill、相对文件路径和操作者查询范围。
 */
export interface GetSkillParams {
    file?: string;
    name: string;
    conversation_id?: ConversationID;
    agent_name?: string;
    executor_settings?: ExecutorSettings;
}

// From runtimeprotocol/types.go
/**
 * GetSkillResult 返回所选文件的正文，默认读取 SKILL.md。
 */
export interface GetSkillResult {
    content: string;
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

// From runtimeprotocol/types.go
export interface GetWorldResult {
    settings: WorldSettings;
    running: WorldRunning;
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
    /**
     * BlockedInputItemID 是被 UserPromptSubmit 拦下的组首消息 ID；其他 hook 为空。
     */
    blocked_input_item_id?: ItemID;
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

// From runtime/types.go
export interface ImportSessionRequest extends ExternalSessionRequest {
    title?: string;
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
    /**
     * ModelCallResult 是本次模型调用的完整结束事实；非末项或未完整结束时为空。
     */
    model_call_result?: ModelCallResult;
    /**
     * ModelCallUsage 是这次完整普通请求的用量和输入位置；只有保存调用报告的末项非空。
     */
    model_call_usage?: TurnContextUsage;
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
    /**
     * HandedOffInputItemID 是此项写成后才算完整交接的组首消息 ID；其他 Item 为空。
     */
    handed_off_input_item_id?: ItemID;
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
export type ItemKind = "agents_instruction" | "client_instruction" | "context_summary" | "executors_change" | "hook_run" | "memory_index" | "message" | "model_call_result" | "model_change" | "model_error" | "reasoning" | "session_fork" | "skill_instruction" | "tool_call" | "tool_result" | "turn_interrupted";

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
    data?: JSONRPCErrorData;
}

// From runtimeprotocol/types.go
/**
 * JSONRPCErrorData 保存业务错误码。
 */
export interface JSONRPCErrorData {
    code: string;
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

// From worldserver/types.go
/**
 * JoinTokenInfo 返回入网码状态，时间字段使用 Unix 毫秒。
 */
export interface JoinTokenInfo {
    id: string;
    owner: string;
    tags: string[];
    /**
     * Reusable 入网码是否可供多个节点注册？
     */
    reusable: boolean;
    used_count: number;
    expires_at: number;
    revoked_at: number;
    created_at: number;
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
 * ListAgentPresetsResult 是 catalog/agents/list 返回的 ACP Agent 预设。
 */
export interface ListAgentPresetsResult {
    presets: AgentPreset[];
}

// From conversation/types.go
/**
 * ListAgentsParam 指定会话列表过滤；SinceHours 为零时包括全部时间。
 */
export interface ListAgentsParam {
    session_id?: SessionID;
    statuses?: SessionState[];
    cwd?: string;
    since_hours?: number;
    labels?: Record<string, string>;
    all?: boolean;
    limit?: number;
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

// From worldserver/types.go
export interface ListDomainsResult {
    domain: string;
    domains: PeerDomain[];
    self_card: string;
}

// From runtimeprotocol/types.go
/**
 * ListExecutorConfigsResult 是 executor/config/list 的返回结果。
 */
export interface ListExecutorConfigsResult {
    executors: ExecutorConfig[];
}

// From runtimeprotocol/types.go
/**
 * ListExecutorsParams 指定要列出执行环境的对话。
 */
export interface ListExecutorsParams {
    /**
     * RefreshTools 只在原生模型调用工具时生效。
     */
    refresh_tools?: boolean;
    conversation_id?: ConversationID;
}

// From runtimeprotocol/types.go
/**
 * ListExecutorsResult 返回全局 Executor 或对话中的执行环境和默认项。
 */
export interface ListExecutorsResult {
    default_executor_id?: string;
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

// From world/types.go
export interface ListMessagesParams {
    direction?: string;
    peer_addr?: string[];
    kind?: string;
    status?: string;
    conversation_id?: string;
    thread_id?: string;
    limit?: number;
    cursor_id?: string;
}

// From runtimeprotocol/types.go
/**
 * ListModelsParams 是 model/list 的参数；agent_name 为空时不返回 Agent 默认模型。
 */
export interface ListModelsParams {
    agent_name?: string;
    conversation_id?: ConversationID;
    session_id?: SessionID;
    executor_id?: string;
    cwd?: string;
}

// From world/types.go
export interface ListPeersResult {
    recent: RecentContact[];
    peers: WorldPeer[];
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
 * ListSSHConfigHostsResult 是 executor/sshConfigHosts 的返回结果。
 */
export interface ListSSHConfigHostsResult {
    hosts: SSHConfigHost[];
}

// From runtimeprotocol/types.go
/**
 * ListSessionsParams 是 session/list 的参数。
 */
export interface ListSessionsParams {
    source_session_id?: SessionID;
    filter?: ListAgentsParam;
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
    applied_filter?: ListAgentsParam;
    total_matched?: number;
    truncated?: boolean;
    hint?: string;
    sessions: Session[];
    next_cursor?: RecordCursor<SessionID>;
}

// From runtimeprotocol/types.go
/**
 * ListSkillsParams 是 skill/list 的参数；没有 conversation_id 时按 agent_name 和 executor_settings 查草稿 Skills。
 */
export interface ListSkillsParams {
    query?: string;
    conversation_id?: ConversationID;
    agent_name?: string;
    executor_settings?: ExecutorSettings;
}

// From world/types.go
export interface ListThreadsParams {
    conversation_id?: string;
    limit?: number;
    cursor_id?: string;
    cursor_updated_at?: number;
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

// From worldserver/types.go
export interface ListUsersResult {
    users: WorldUser[];
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
 * MCPServerInfo 是对话里一个执行环境上的一个 MCP 连接。
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

// From world/types.go
export interface MessagePage {
    items: WorldMessage[];
    next_cursor?: RecordCursor<string>;
}

// From defs/id.go
/**
 * ModelCallID 唯一标识一次模型调用，并按创建时间排序。
 */
export type ModelCallID = string;

// From defs/item.go
/**
 * ModelCallResult 是一次普通模型调用完整结束时写入的调用级结果事实。
 */
export type ModelCallResult = "reply" | "tools_rejected";

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
    /**
     * Icon 是外部 Agent 的品牌图标。
     */
    icon?: string;
    /**
     * Description 是给 Agent 选模型时看的用途说明。
     */
    description?: string;
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
    // this is likely an enum in an external package "github.com/VaalaCat/llmkit/ir.ReasoningContext"
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

// From world/types.go
/**
 * NodeNotification 是节点发布的状态、联系人或消息更新。
 */
export interface NodeNotification {
    event: string;
    data: unknown;
}

// From worldserver/types.go
/**
 * NodeRecord 保存已入网节点的身份、归属和吊销状态。
 */
export interface NodeRecord {
    node_id: string;
    node_name: string;
    owner: string;
    // this is likely an enum in an external package "gorm.io/datatypes.JSONSlice[string]"
    tags: string[];
    sign_public: string;
    box_public: string;
    revoked_at: number;
    created_at: number;
}

// From world/types.go
/**
 * NodeStatus 是节点状态接口返回的运行、连接和离线队列信息。
 */
export interface NodeStatus {
    revoked: boolean;
    /**
     * 本机联邦是否启用？
     */
    enabled: boolean;
    admin_enabled: boolean;
    node_addr: string;
    server_url: string;
    /**
     * 本机是否已收到控制面的通讯录？
     */
    connected: boolean;
    pending_outbox_count: number;
    remote_access_local_only?: boolean;
    remote_access_relays?: RelayStatus[];
    remote_access_url?: string;
    relay_attachments?: RelayStatus[];
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

// From worldserver/types.go
/**
 * PeerDomain 保存管理员导入的域名片和添加时间。
 */
export interface PeerDomain {
    status?: string;
    domain: string;
    public_key: string;
    entries: PeerEntries;
    added_at?: number;
}

// From worldserver/types.go
/**
 * PeerEntries 保存按拨号顺序公布的直连与中转地址。
 */
export interface PeerEntries {
    direct_urls: string[];
    relays: string[];
}

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
 * ProviderModelConfig 将本地模型别名映射为上游模型名，给出用途说明和请求参数；省略的请求参数取 config.yaml 的 models.defaults。
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
    /**
     * Description 是给 Agent 选模型时看的用途说明。
     */
    description?: string;
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

// From runtimeprotocol/types.go
/**
 * ReadSessionHistoryParams 指定来源、目标和本地历史游标。
 */
export interface ReadSessionHistoryParams extends CollaborationSource, SessionHistoryParam {
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

// From world/types.go
/**
 * RecentContact 给出外域对端及最近活动的线程。
 */
export interface RecentContact {
    address: string;
    thread_id: string;
}

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

// From world/types.go
/**
 * RelayStatus 是节点到单个 relay 的挂靠状态。
 */
export interface RelayStatus {
    url: string;
    implicit: boolean;
    /**
     * 是否收到 relay 的挂靠确认？
     */
    connected: boolean;
}

// From runtimeprotocol/types.go
/**
 * ResetTokenResult 供发起重置的客户端重新认证。
 */
export interface ResetTokenResult {
    token: string;
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

// From defs/executor_config.go
/**
 * SSHConfigHost 是从 serve 机器 ~/.ssh/config 解析出的单条非通配符主机项，供客户端快捷填表。
 */
export interface SSHConfigHost {
    /**
     * Alias 是 Host 段的名字，可直接作为默认 Executor ID。
     */
    alias: string;
    /**
     * Address 是按 User、HostName、Port 拼好的 [user@]host[:port]。
     */
    address: string;
    /**
     * IdentityFile 是该 Host 配置的首个私钥路径（原样保留 ~ 便于阅读），未配置时为空。
     */
    identity_file?: string;
}

// From defs/executor_config.go
/**
 * SSHExecutorConfig 是 kind 为 ssh 时的连接参数。
 */
export interface SSHExecutorConfig {
    /**
     * Address 是 [user@]host[:port]。
     */
    address: string;
    /**
     * IdentityFile 是 serve 所在机器上的私钥路径，加载时展开 ~；为空时依次用 ssh-agent 和默认私钥。
     */
    identity_file?: string;
}

// From conversation/types.go
/**
 * SendMessageParam 是 send_message 的模型参数，向后代或直接父方提交一次输入。
 */
export interface SendMessageParam {
    session_id: SessionID;
    message: string;
    mode?: SessionInputMode;
    /**
     * Model 和 ReasoningEffort 只能用于后代；任一非空时先替换接收方的模型选择，再提交输入。
     */
    model?: string;
    reasoning_effort?: string;
}

// From world/types.go
export interface SendMessageRequest {
    to: string;
    message: string;
    subject?: string;
    thread_id?: string;
    source_conversation_id?: string;
    /**
     * 是否等到收到回复？
     */
    wait?: boolean;
}

// From runtimeprotocol/types.go
export interface SendSessionParams extends CollaborationSource, SendMessageParam {
    interrupt?: boolean;
}

// From runtimeprotocol/types.go
export interface SendSessionResult {
    input_item_id: ItemID;
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
    /**
     * External 保存外部后端的恢复句柄。
     */
    external?: ExternalSession;
    /**
     * Labels 是会话标签。
     */
    labels?: Record<string, string>;
    /**
     * Background 是否在默认会话列表中隐藏？
     */
    background?: boolean;
    /**
     * DetachedConversationID 是分离后新对话的 ID。
     */
    detached_conversation_id?: ConversationID;
    /**
     * DetachedFromConversationID 和 DetachedFromSessionID 是转为独立对话前的对话和会话 ID，仅新根保存。
     */
    detached_from_conversation_id?: ConversationID;
    detached_from_session_id?: SessionID;
    /**
     * SuppressFinishReport 是否跳过结束时发给父会话的回报？
     */
    suppress_finish_report?: boolean;
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
    mcp_servers?: string[];
    capabilities?: ExternalCapabilities;
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

// From runtimeprotocol/types.go
/**
 * SessionExternalStateUpdatedParams 由外部驱动就绪或选项、能力、命令发生变化时发给订阅方。
 */
export interface SessionExternalStateUpdatedParams extends NotificationTarget {
    options: ExternalOptions;
    capabilities: ExternalCapabilities;
    commands: ExternalCommand[];
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

// From conversation/types.go
/**
 * SessionHistoryParam 是 read_session_history 的模型参数，从新到旧分页读取后代 fork 之后的历史。
 */
export interface SessionHistoryParam {
    session_id: SessionID;
    cursor?: Ordinal;
    limit?: number;
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
 * SessionRuntimeInfo 是一个 Session 的运行信息；runtimeConversation 在它有变化时发给对话订阅者，
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

// From conversation/types.go
/**
 * SessionUpdate 指定要替换的会话元数据；省略的字段保留原值。
 */
export interface SessionUpdate {
    task_name?: string;
    labels?: Record<string, string>;
    background?: boolean;
}

// From runtimeprotocol/types.go
/**
 * SetAgentParams 保存整份 Agent 定义。
 */
export interface SetAgentParams {
    definition: AgentConfig;
}

// From runtimeprotocol/types.go
/**
 * SetClientExecutorParams 登记本连接为对话提供的执行环境，mcp_server_ids 为空时注销登记。
 * 登记时省略 executor_id 使用连接 ID，注销时省略则使用已登记的 ID。
 */
export interface SetClientExecutorParams {
    conversation_id: ConversationID;
    executor_id?: string;
    description?: string;
    mcp_server_ids: string[];
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
 * SetDefinitionsResult 是 SetDefinitions 返回的结果，列出已接收新定义、等待空闲后应用的对话。
 */
export interface SetDefinitionsResult {
    /**
     * PendingConversationIDs 是已准备新配置、等待全部 Session 空闲后自动应用的对话。
     */
    pending_conversation_ids: ConversationID[];
}

// From runtimeprotocol/types.go
/**
 * SetExecutorParams 是 executor/set 的参数。
 */
export interface SetExecutorParams {
    executor: ExecutorConfig;
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

// From runtimeprotocol/types.go
/**
 * SetSessionOptionParams 指定目标会话以及需要交给后端的选择。
 */
export interface SetSessionOptionParams extends SessionParams, ExternalOptionSelection {
}

// From runtimeprotocol/types.go
export interface SetWorldParams {
    settings: WorldSettings;
    join?: WorldJoinParams;
}

// From runtimeprotocol/types.go
export interface SetWorldResult {
    restarting: boolean;
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

// From conversation/types.go
/**
 * SpawnAgentParam 是 spawn_agent 的模型参数，创建一个直接子 Agent 并提交首个任务。
 */
export interface SpawnAgentParam {
    initial_options?: ExternalOptionSelection[];
    labels?: Record<string, string>;
    background?: boolean;
    executor_id?: string;
    cwd?: string;
    notify_on_finish?: boolean;
    task_name: string;
    message: string;
    fork_turns?: string;
    agent_name?: string;
    model?: string;
    reasoning_effort?: string;
}

// From conversation/types.go
/**
 * SpawnAgentResult 是 spawn_agent 的结果：Status 为 accepted 表示子 Session 已创建、首个任务已放进它的收件箱，子任务还没做完；
 * Session 已保存但首个任务没被接收时，工具返回错误，SessionID 写在错误消息里。
 */
export interface SpawnAgentResult {
    input_item_id: ItemID;
    session_id: SessionID;
    task_name: string;
    status: string;
}

// From runtimeprotocol/types.go
export interface SpawnSessionParams extends CollaborationSource, SpawnAgentParam {
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
export interface StopSessionParams extends CollaborationSource {
    session_id: SessionID;
}

// From runtimeprotocol/types.go
export interface StopSessionResult {
    turn_id?: TurnID;
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
    restoring?: boolean;
    options?: ExternalOptions;
    capabilities?: ExternalCapabilities;
    commands?: ExternalCommand[];
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
 * TerminalCaptureParams 指定终端和要读取的最近行数。
 */
export interface TerminalCaptureParams extends TerminalParams {
    lines?: number;
}

// From runtimeprotocol/types.go
/**
 * TerminalCaptureResult 返回去除 ANSI 控制序列的终端输出。
 */
export interface TerminalCaptureResult {
    text: string;
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

// From runtimeprotocol/types.go
/**
 * TestSSHExecutorParams 是 executor/test 的参数。
 */
export interface TestSSHExecutorParams {
    ssh: SSHExecutorConfig;
    workspace_directory: string;
}

// From runtimeprotocol/types.go
/**
 * TestSSHExecutorResult 是 executor/test 的返回结果；失败直接走 JSON-RPC error。
 */
export interface TestSSHExecutorResult {
    resolved_directory: string;
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

// From world/types.go
/**
 * ThreadBinding 将来信线程绑定到本地对话及该轮的转发预算。
 */
export interface ThreadBinding {
    peer_domain: string;
    target: string;
    thread_id: string;
    conversation_id: string;
    peer_addr: string;
    direction: string; // inbound | outbound
    remaining_hops: number; // 收到时保存的剩余跳数
    created_at: number;
}

// From world/types.go
export interface ThreadPage {
    items: ThreadBinding[];
    next_cursor?: RecordCursor<string>;
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
    // external type "github.com/VaalaCat/llmkit/ir.ProviderData", to include this type the package must be explicitly included in the parsing
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
     * IsCompactOnly 是否只运行手动压缩？普通 Turn 触发自动压缩时仍为 false。
     */
    is_compact_only?: boolean;
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
    // this is likely an enum in an external package "github.com/VaalaCat/llmkit/ir.ReasoningContext"
    reasoning_context?: string;
}

// From defs/turn.go
/**
 * TurnExecutor 是一个 Executor 和它在对话里的根目录与说明。
 */
export interface TurnExecutor {
    id: string;
    workspace_directory?: string;
    /**
     * Description 是执行环境的说明，动态环境接入时由变更通知展示。
     */
    description?: string;
    /**
     * IsDynamic 这个 Executor 是否可以在对话运行期间接入或移除？
     */
    is_dynamic?: boolean;
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
export interface UpdateSessionParams extends SessionParams, SessionUpdate {
}

// From runtimeprotocol/types.go
/**
 * UploadAttachmentParams 是 attachment/upload 的参数。
 */
export interface UploadAttachmentParams {
    conversation_id: ConversationID;
    executor_id: string;
    name: string;
    mime: string;
    /**
     * Data 在线上以 base64 字符串传输。
     */
    data: string;
}

// From conversation/types.go
/**
 * WaitAgentParam 是 wait_agent 的模型参数，等待当前 Turn 收到新的补充输入。
 */
export interface WaitAgentParam {
    /**
     * WaitFor 为 subagent_result 时先检查运行后代；省略时沿用普通等待。
     */
    wait_for?: string;
    timeout_seconds?: number;
}

// From conversation/types.go
/**
 * WaitAgentResult 说明唤醒原因与 Agent 发送方，并携带已完成子任务的最终正文。
 */
export interface WaitAgentResult {
    detached_session_id?: SessionID;
    text?: string;
    detached_conversation_id?: ConversationID;
    /**
     * Hint 在没有运行后代时提示如何查阅现状；其他原因时为空。
     */
    hint?: string;
    reason: string;
    from?: SessionID[];
}

// From runtimeprotocol/types.go
export interface WaitSessionParams extends CollaborationSource, WaitAgentParam {
}

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

// From runtimeprotocol/types.go
export interface WorldDomainParams {
    domain: string;
}

// From runtimeprotocol/types.go
export interface WorldJoinParams {
    invite_code: string;
}

// From world/types.go
/**
 * WorldMessage 保存本节点收发的消息、对话归属和处理结果。
 */
export interface WorldMessage {
    message_id: string;
    thread_id: string;
    direction: WorldMessageDirection;
    peer_addr: string;
    target?: string;
    kind: string; // message | reply
    conversation_id?: string;
    subject?: string;
    body: string;
    in_reply_to?: string;
    hop_limit: number;
    status: WorldMessageStatus;
    reply_body?: string;
    error_code?: string;
    error_text?: string;
    notify_conversation: boolean;
    received_sequence?: number;
    created_at: number;
    updated_at: number;
}

// From world/types.go
/**
 * WorldMessageDirection 标记消息相对于本节点的收发方向。
 */
export type WorldMessageDirection = "inbound" | "outbound";

// From runtimeprotocol/types.go
export interface WorldMessageParams {
    message_id: string;
}

// From world/types.go
/**
 * WorldMessageStatus 记录发件投递或收件处理的进度。
 */
export type WorldMessageStatus = "accepted" | "cancelled" | "completed" | "failed" | "pending_offline" | "remote_running" | "replied" | "running" | "sending" | "unknown";

// From runtimeprotocol/types.go
/**
 * WorldMessageUpdatedClosedParams 是节点关闭消息事件订阅时的通知。
 */
export interface WorldMessageUpdatedClosedParams {
    reason: string;
}

// From runtimeprotocol/types.go
export interface WorldNodeParams {
    node_id: string;
}

// From config/types.go
export interface WorldNodeSettings {
    enabled: boolean;
    name: string;
    domain: string;
    server_url: string;
    direct_urls: string[];
    default_target: string;
    advertise_targets: AdvertisedTarget[];
}

// From runtimeprotocol/types.go
export interface WorldOwnerParams {
    owner: string;
}

// From world/types.go
export interface WorldPeer {
    address: string;
    node_name: string;
    /**
     * 联系人是否可以投递？
     */
    online: boolean;
    description?: string;
    targets: AdvertisedTarget[];
}

// From config/types.go
/**
 * WorldRemoteAccessConfig 保存本机远程访问开关和链接显示名。
 */
export interface WorldRemoteAccessConfig {
    /**
     * 是否经 relay 开放本机 Runtime？
     */
    enabled: boolean;
    name?: string;
}

// From runtimeprotocol/types.go
export interface WorldRunning {
    remote_access_enabled?: boolean;
    node_enabled: boolean;
    server_enabled: boolean;
}

// From config/types.go
export interface WorldServerSettings {
    enabled: boolean;
    domain: string;
    is_database_created: boolean;
}

// From config/types.go
/**
 * WorldSettings 是设置页可见的本机节点和控制面配置。
 */
export interface WorldSettings {
    remote_access: WorldRemoteAccessConfig;
    relays: string[];
    relay_mode: string;
    web_url: string;
    node: WorldNodeSettings;
    server: WorldServerSettings;
}

// From runtimeprotocol/types.go
export interface WorldTokenParams {
    id: string;
}

// From worldserver/types.go
/**
 * WorldUser 汇总归属于同一个人的节点、有效入网码和规则。
 */
export interface WorldUser {
    owner: string;
    node_count: number;
    online_count: number;
    token_count: number;
    rule_count: number;
}

/**
 * 每个请求方法的参数类型。
 */
export interface MethodParams {
    "server/resetToken": EmptyParams;
    "world/getStatus": EmptyParams;
    "world/listPeers": EmptyParams;
    "world/sendMessage": SendMessageRequest;
    "world/forward": ForwardRequest;
    "world/listMessages": ListMessagesParams;
    "world/cancelMessage": WorldMessageParams;
    "world/listThreads": ListThreadsParams;
    "world/subscribe": EmptyParams;
    "world/unsubscribe": EmptyParams;
    "world/admin/listUsers": EmptyParams;
    "world/admin/getACLMatrix": GetACLMatrixRequest;
    "world/admin/listNodes": EmptyParams;
    "world/admin/revokeNode": WorldNodeParams;
    "world/admin/revokeOwner": WorldOwnerParams;
    "world/admin/listTokens": EmptyParams;
    "world/admin/createToken": CreateJoinTokenRequest;
    "world/admin/revokeToken": WorldTokenParams;
    "world/admin/listDomains": EmptyParams;
    "world/admin/addDomain": AddDomainRequest;
    "world/admin/acceptDomain": WorldDomainParams;
    "world/admin/rejectDomain": WorldDomainParams;
    "world/admin/deleteDomain": WorldDomainParams;
    "world/admin/getACL": EmptyParams;
    "world/admin/replaceACL": ACLRule[];
    "agent/getOptions": AgentOptionsRequest;
    "agent/listSessions": ExternalSessionsRequest;
    "agent/previewSession": ExternalSessionRequest;
    "session/setOption": SetSessionOptionParams;
    "session/reload": SessionParams;
    "session/import": ImportSessionRequest;
    "session/detach": SessionParams;
    "session/spawn": SpawnSessionParams;
    "session/send": SendSessionParams;
    "session/wait": WaitSessionParams;
    "session/stop": StopSessionParams;
    "session/update": UpdateSessionParams;
    "executor/exec": ExecutorExecRequest;
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
    "attachment/upload": UploadAttachmentParams;
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
    "session/history": ReadSessionHistoryParams;
    "session/contextUsage": SessionParams;
    "session/details": SessionParams;
    "session/setModel": SetSessionModelParams;
    "model/list": ListModelsParams;
    "agent/list": EmptyParams;
    "agent/get": AgentNameParams;
    "agent/set": SetAgentParams;
    "agent/delete": AgentNameParams;
    "executor/list": ListExecutorsParams;
    "executor/tool": CallExecutorToolParams;
    "executor/resolveDirectory": ResolveDirectoryParams;
    "executor/listDirectories": ListDirectoriesParams;
    "executor/config/list": EmptyParams;
    "executor/set": SetExecutorParams;
    "executor/delete": DeleteExecutorParams;
    "executor/test": TestSSHExecutorParams;
    "executor/sshConfigHosts": EmptyParams;
    "provider/list": EmptyParams;
    "provider/set": SetProviderParams;
    "provider/delete": ProviderIDParams;
    "provider/fetchModels": ProviderParams;
    "config/setDefaultModel": SetDefaultModelParams;
    "config/getWorld": EmptyParams;
    "config/setWorld": SetWorldParams;
    "config/reload": EmptyParams;
    "catalog/providers/list": EmptyParams;
    "catalog/agents/list": EmptyParams;
    "catalog/models/list": ProviderIDParams;
    "skill/list": ListSkillsParams;
    "skill/get": GetSkillParams;
    "hook/list": EmptyParams;
    "mcpServer/list": ConversationParams;
    "conversation/setClientExecutor": SetClientExecutorParams;
    "conversation/subscribe": ConversationParams;
    "conversation/unsubscribe": ConversationParams;
    "conversation/running/subscribe": EmptyParams;
    "conversation/running/unsubscribe": EmptyParams;
    "session/subscribe": SessionParams;
    "session/unsubscribe": SessionParams;
    "terminal/open": TerminalOpenParams;
    "terminal/attach": TerminalAttachParams;
    "terminal/input": TerminalInputParams;
    "terminal/close": TerminalParams;
    "terminal/capture": TerminalCaptureParams;
    "terminal/reset": TerminalParams;
    "terminal/subscribe": EmptyParams;
    "terminal/unsubscribe": EmptyParams;
}

/**
 * 每个请求方法的结果类型。
 */
export interface MethodResult {
    "server/resetToken": ResetTokenResult;
    "world/getStatus": NodeStatus;
    "world/listPeers": ListPeersResult;
    "world/sendMessage": WorldMessage;
    "world/forward": WorldMessage;
    "world/listMessages": MessagePage;
    "world/cancelMessage": WorldMessage;
    "world/listThreads": ThreadPage;
    "world/subscribe": EmptyResult;
    "world/unsubscribe": EmptyResult;
    "world/admin/listUsers": ListUsersResult;
    "world/admin/getACLMatrix": ACLMatrix;
    "world/admin/listNodes": AdminNode[];
    "world/admin/revokeNode": EmptyResult;
    "world/admin/revokeOwner": EmptyResult;
    "world/admin/listTokens": JoinTokenInfo[];
    "world/admin/createToken": CreateJoinTokenResponse;
    "world/admin/revokeToken": EmptyResult;
    "world/admin/listDomains": ListDomainsResult;
    "world/admin/addDomain": PeerDomain;
    "world/admin/acceptDomain": EmptyResult;
    "world/admin/rejectDomain": EmptyResult;
    "world/admin/deleteDomain": EmptyResult;
    "world/admin/getACL": ACLRule[];
    "world/admin/replaceACL": EmptyResult;
    "agent/getOptions": ExternalOptions;
    "agent/listSessions": ExternalSessionsResult;
    "agent/previewSession": ExternalSessionPreview;
    "session/setOption": EmptyResult;
    "session/reload": EmptyResult;
    "session/import": ConversationResult;
    "session/detach": ConversationResult;
    "session/spawn": SpawnAgentResult;
    "session/send": SendSessionResult;
    "session/wait": WaitAgentResult;
    "session/stop": StopSessionResult;
    "session/update": SessionResult;
    "executor/exec": ExecutorExecResult;
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
    "attachment/upload": FileRef;
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
    "session/history": ListItemsResult;
    "session/contextUsage": SessionContextUsage;
    "session/details": SessionDetails;
    "session/setModel": SessionResult;
    "model/list": ModelList;
    "agent/list": ListAgentsResult;
    "agent/get": AgentConfig;
    "agent/set": SetDefinitionsResult;
    "agent/delete": SetDefinitionsResult;
    "executor/list": ListExecutorsResult;
    "executor/tool": ToolResult;
    "executor/resolveDirectory": ResolveDirectoryResult;
    "executor/listDirectories": WorkspaceDirectories;
    "executor/config/list": ListExecutorConfigsResult;
    "executor/set": SetDefinitionsResult;
    "executor/delete": SetDefinitionsResult;
    "executor/test": TestSSHExecutorResult;
    "executor/sshConfigHosts": ListSSHConfigHostsResult;
    "provider/list": ListProvidersResult;
    "provider/set": SetDefinitionsResult;
    "provider/delete": SetDefinitionsResult;
    "provider/fetchModels": ProviderModelsResult;
    "config/setDefaultModel": SetDefinitionsResult;
    "config/getWorld": GetWorldResult;
    "config/setWorld": SetWorldResult;
    "config/reload": SetDefinitionsResult;
    "catalog/providers/list": ListCatalogProvidersResult;
    "catalog/agents/list": ListAgentPresetsResult;
    "catalog/models/list": ProviderModelsResult;
    "skill/list": SkillInfo[];
    "skill/get": GetSkillResult;
    "hook/list": ListHooksResult;
    "mcpServer/list": ListMCPServersResult;
    "conversation/setClientExecutor": EmptyResult;
    "conversation/subscribe": SubscribeConversationResult;
    "conversation/unsubscribe": EmptyResult;
    "conversation/running/subscribe": RunningConversations;
    "conversation/running/unsubscribe": EmptyResult;
    "session/subscribe": SubscribeSessionResult;
    "session/unsubscribe": EmptyResult;
    "terminal/open": TerminalOpenResult;
    "terminal/attach": TerminalAttachResult;
    "terminal/input": EmptyResult;
    "terminal/close": EmptyResult;
    "terminal/capture": TerminalCaptureResult;
    "terminal/reset": EmptyResult;
    "terminal/subscribe": TerminalListResult;
    "terminal/unsubscribe": EmptyResult;
}

/**
 * 每种服务端通知的参数类型。
 */
export interface NotificationParams {
    "world/messageUpdated": NodeNotification;
    "world/messageUpdatedClosed": WorldMessageUpdatedClosedParams;
    "conversation/runningChanged": RunningConversations;
    "conversation/runningClosed": RunningConversationsClosedParams;
    "subscription/closed": SubscriptionClosedParams;
    "session/externalStateUpdated": SessionExternalStateUpdatedParams;
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
