package appserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// WriteStartupConfig 写出 Codex 桌面启动检查结果。
func WriteStartupConfig(output io.Writer, version string) error {
	// 1. 将程序版本和无托管策略结果编码到输出流。
	return json.NewEncoder(output).Encode(codexStartupConfigResult{AppServerVersion: version})
}

// getCodexError 将请求处理错误转换为 Codex 协议错误。
func getCodexError(err error, threadID string) *codexError {
	// 1. 已有 Codex 错误原样返回。
	var protocolErr *codexError
	if errors.As(err, &protocolErr) {
		return protocolErr
	}
	// 2. Session 忙和对话不存在转成 -32600，对话不存在时附上请求的 Thread ID。
	if errors.Is(err, session.ErrSessionBusy) {
		return &codexError{
			errorCodeInvalidRequest,
			"session is busy",
		}
	}
	if errors.Is(err, runtime.ErrConversationNotFound) {
		return &codexError{
			errorCodeInvalidRequest,
			"thread not found: " + threadID,
		}
	}
	// 3. 服务端参数错误保留 -32602 和原文，其他错误使用 -32603。
	var responseErr *runtimeclient.ResponseError
	if errors.As(err, &responseErr) {
		if responseErr.Code == errorCodeInvalidParams {
			return &codexError{
				errorCodeInvalidParams,
				responseErr.Message,
			}
		}
		return &codexError{
			errorCodeInternal,
			responseErr.Message,
		}
	}
	return &codexError{
		errorCodeInternal,
		err.Error(),
	}
}

// getThreadForkError 将分叉截取点错误转换为 Codex 错误。
func getThreadForkError(err error, params codexThreadForkParams) *codexError {
	// 1. 将两个截取点冲突和轮次不存在转换为 -32600，填写请求里的轮次 ID。
	switch {
	case errors.Is(err, conversation.ErrForkBoundaryConflict):
		return &codexError{
			errorCodeInvalidRequest,
			"`beforeTurnId` cannot be combined with `lastTurnId`",
		}
	case errors.Is(err, conversation.ErrForkTurnNotFound) && params.BeforeTurnID != "":
		return &codexError{
			errorCodeInvalidRequest,
			"beforeTurnId '" + params.BeforeTurnID + "' was not found in the source thread",
		}
	case errors.Is(err, conversation.ErrForkTurnNotFound):
		return &codexError{
			errorCodeInvalidRequest,
			"lastTurnId '" + params.LastTurnID + "' was not found in the source thread",
		}
	}
	// 2. 其他错误按普通请求错误转换。
	return getCodexError(err, params.ThreadID)
}

// newClientInstructionItem 创建一条带文字的客户端指令 Item，ID 新生成，TurnID 由 Session 开始下一轮时分配。
func newClientInstructionItem(text string) *defs.Item {
	// 1. 生成 Item ID，填写种类和正文。
	return &defs.Item{
		ID:                defs.NewItemID(),
		Kind:              defs.ItemClientInstruction,
		ClientInstruction: &defs.ClientInstruction{Text: text},
	}
}

// getClientInstructionItems 把 thread/inject_items 的条目转成客户端指令 Item：每条 user 或 developer 的 message 按顺序拼接
// input_text 的文字成一条指令；任一条是其他种类、解析失败或没有文字时整批返回 -32602。
func getClientInstructionItems(rawItems []json.RawMessage) ([]*defs.Item, error) {
	// 1. 逐条解析条目，只认 user 和 developer 的 message。
	unsupported := &codexError{
		errorCodeInvalidParams,
		"thread/inject_items only supports user and developer message items",
	}
	items := make([]*defs.Item, 0, len(rawItems))
	for _, rawItem := range rawItems {
		var injected codexInjectedItem
		if err := json.Unmarshal(rawItem, &injected); err != nil ||
			injected.Type != "message" || (injected.Role != "user" && injected.Role != "developer") {
			return nil, unsupported
		}
		// 2. 按顺序直接拼接 input_text 的文字，没有文字的消息（例如只有图片）整批拒绝，否则生成一条客户端指令。
		var text strings.Builder
		for _, content := range injected.Content {
			if content.Type == "input_text" {
				text.WriteString(content.Text)
			}
		}
		if text.Len() == 0 {
			return nil, unsupported
		}
		items = append(items, newClientInstructionItem(text.String()))
	}
	return items, nil
}

// newCodexResponse 创建请求响应，包含结果或转换后的错误。
func newCodexResponse(id json.RawMessage, result any, err error) codexMessage {
	// 1. 缺少请求 ID 时使用 null。
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	// 2. 错误响应使用 Codex 错误，成功响应带上结果。
	if err != nil {
		return codexMessage{
			ID:    id,
			Error: getCodexError(err, ""),
		}
	}
	return codexMessage{
		ID:     id,
		Result: result,
	}
}

// decodeCodexParams 解码 Codex 请求参数。
func decodeCodexParams(params json.RawMessage, target any) error {
	// 1. 没有参数时保留零值，否则解码到目标对象，失败时返回 -32602。
	if len(params) == 0 {
		return nil
	}
	if err := json.Unmarshal(params, target); err != nil {
		return &codexError{
			errorCodeInvalidParams,
			"invalid params: " + err.Error(),
		}
	}
	return nil
}

// getCodexRequestThreadID 读取请求里的 Thread ID。
func getCodexRequestThreadID(params json.RawMessage) string {
	// 1. 解码并返回 threadId，缺少或解码失败时返回空串。
	var target struct {
		ThreadID string `json:"threadId"`
	}
	_ = json.Unmarshal(params, &target)
	return target.ThreadID
}

// getCodexMcpServerStatuses 将 MCP 连接列表转换为 Codex server 状态。
func getCodexMcpServerStatuses(servers []conversation.MCPServerInfo, defaultExecutorID string) []codexMcpServerStatus {
	// 1. 统计每个 server ID 对应的连接数量。
	executorCounts := make(map[string]int, len(servers))
	for _, server := range servers {
		executorCounts[server.ServerID]++
	}
	// 2. 按输入顺序转换状态和工具；重名的非默认 Executor 连接加上 @executorID。
	statuses := make([]codexMcpServerStatus, 0, len(servers))
	for _, server := range servers {
		name := server.ServerID
		if executorCounts[server.ServerID] > 1 && server.ExecutorID != defaultExecutorID {
			name += "@" + server.ExecutorID
		}
		status := codexMcpServerStatus{
			Name:              name,
			RuntimeStatus:     getCodexMcpRuntimeStatus(server.State),
			Tools:             make(map[string]codexMcpTool, len(server.Tools)),
			Resources:         []struct{}{},
			ResourceTemplates: []struct{}{},
			AuthStatus:        codexMcpAuthUnsupported,
		}
		for _, tool := range server.Tools {
			status.Tools[tool.OriginalName] = codexMcpTool{
				Name:        tool.OriginalName,
				Description: tool.Description,
				InputSchema: tool.InputSchema,
			}
		}
		// 3. 填写连接错误和插件来源，收集转换结果。
		if server.Error != "" {
			status.ToolsError = &server.Error
		}
		if server.PluginName != "" {
			status.PluginID = &server.PluginName
		}
		statuses = append(statuses, status)
	}
	return statuses
}

// getCodexMcpRuntimeStatus 将 MCP 连接状态转换为 Codex runtimeStatus。
func getCodexMcpRuntimeStatus(state mcpclient.ConnectionState) *string {
	// 1. 连接中、已连接和失败分别转换为 starting、connected 和 failed，未知状态返回 nil。
	var status string
	switch state {
	case mcpclient.ConnectionStateConnecting:
		status = codexMcpRuntimeStarting
	case mcpclient.ConnectionStateConnected:
		status = codexMcpRuntimeConnected
	case mcpclient.ConnectionStateFailed:
		status = codexMcpRuntimeFailed
	default:
		return nil
	}
	return &status
}

// getCodexModel 将模型定义转换为 Codex 模型选项。
func getCodexModel(model *defs.ModelDefinition, isDefault bool) codexModel {
	// 1. 转换可选推理强度，未指定默认强度时使用 none。
	efforts := make([]codexReasoningEffortOption, 0, len(model.ReasoningEfforts))
	for _, effort := range model.ReasoningEfforts {
		efforts = append(efforts, codexReasoningEffortOption{ReasoningEffort: effort})
	}
	defaultEffort := model.DefaultReasoningEffort
	if defaultEffort == "" {
		defaultEffort = "none"
	}
	// 2. 填写模型引用、显示名、默认标记和推理强度。
	return codexModel{
		ID:                        model.Ref.String(),
		Model:                     model.Ref.String(),
		DisplayName:               model.DisplayName,
		Description:               model.DisplayName,
		IsDefault:                 isDefault,
		SupportedReasoningEfforts: efforts,
		DefaultReasoningEffort:    defaultEffort,
	}
}

// getCodexHookMetadata 将 hook 配置转换为 Codex hook 列表项。
func getCodexHookMetadata(info *hook.HookInfo) codexHookMetadata {
	// 1. 读取匹配条件，区分用户和插件来源。
	var matcher *string
	if info.Matcher != "" {
		matcher = &info.Matcher
	}
	source := codexHookSourceUser
	var pluginID *string
	if info.PluginName != "" {
		source, pluginID = codexHookSourcePlugin, &info.PluginName
	}
	// 2. 填写命令、超时、哈希和展示信息，标记为已启用和已信任。
	return codexHookMetadata{
		Key:          info.Key,
		EventName:    getCodexHookEventName(info.Event),
		HandlerType:  codexHookHandlerCommand,
		Command:      info.Handler,
		Matcher:      matcher,
		TimeoutSec:   info.TimeoutSeconds,
		SourcePath:   info.SourcePath,
		Source:       source,
		PluginID:     pluginID,
		DisplayOrder: info.DisplayOrder,
		Enabled:      true,
		CurrentHash:  info.CurrentHash,
		TrustStatus:  codexHookTrusted,
	}
}

// getCodexHookRunSummary 将 hook 运行记录转换为 Codex 运行摘要。
func getCodexHookRunSummary(run *defs.HookRun) codexHookRunSummary {
	// 1. 填写运行信息，将开始时间从毫秒换为 Unix 秒。
	summary := codexHookRunSummary{
		ID:            string(run.ID),
		EventName:     getCodexHookEventName(run.Event),
		HandlerType:   codexHookHandlerCommand,
		ExecutionMode: codexHookExecutionSync,
		Scope:         codexHookScopeTurn,
		SourcePath:    run.SourcePath,
		Source:        codexHookSourceUser,
		DisplayOrder:  run.DisplayOrder,
		Status:        string(run.Status),
		StartedAt:     run.StartedAt / 1000,
		Entries:       []codexHookOutputEntry{},
	}
	// 2. Session 和子 Agent 启动 hook 使用 Thread 范围，已结束的运行填入完成时间和耗时。
	if run.Event == defs.HookEventSessionStart || run.Event == defs.HookEventSubagentStart {
		summary.Scope = codexHookScopeThread
	}
	if run.Status != defs.HookRunRunning {
		completedAt, durationMs := run.CompletedAt/1000, run.CompletedAt-run.StartedAt
		summary.CompletedAt, summary.DurationMs = &completedAt, &durationMs
	}
	// 3. 按 context、feedback、stop、warning、error 顺序收集非空输出。
	for _, entry := range []codexHookOutputEntry{
		{
			Kind: codexHookEntryContext,
			Text: run.AdditionalContext,
		},
		{
			Kind: codexHookEntryFeedback,
			Text: run.BlockReason,
		},
		{
			Kind: codexHookEntryStop,
			Text: run.StopReason,
		},
		{
			Kind: codexHookEntryWarning,
			Text: run.SystemMessage,
		},
		{
			Kind: codexHookEntryError,
			Text: run.ErrorMessage,
		},
	} {
		if entry.Text != "" {
			summary.Entries = append(summary.Entries, entry)
		}
	}
	return summary
}

// getCodexHookEventName 返回首字母小写的 Codex hook 事件名。
func getCodexHookEventName(event defs.HookEventName) string {
	// 1. 将事件名的首字母转为小写，例如 PreToolUse 转为 preToolUse。
	name := string(event)
	return strings.ToLower(name[:1]) + name[1:]
}

// getWebSocketCloseReason 截断 WebSocket 关闭原因并保留完整字符。
func getWebSocketCloseReason(reason string) string {
	// 1. 关闭原因最多保留 123 字节，未超限时返回原文。
	const maxCloseReasonBytes = 123
	if len(reason) <= maxCloseReasonBytes {
		return reason
	}
	// 2. 从上限向前寻找完整 UTF-8 字符起点，返回截取文本。
	end := maxCloseReasonBytes
	for end > 0 && !utf8.RuneStart(reason[end]) {
		end--
	}
	return reason[:end]
}

// isNormalConnectionEnd 连接是否因对端正常关闭或 app-server 退出而结束？
func isNormalConnectionEnd(err error) bool {
	// 1. 检查 EOF、上下文取消和 WebSocket 正常关闭或离开状态。
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return true
	}
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return true
	}
	return false
}

// getToolStatus 根据工具结果和中断状态返回 Codex 工具状态。
func getToolStatus(result *defs.Item, isInterrupted bool) string {
	// 1. 无结果时返回 failed 或 inProgress，有错误结果返回 failed，其他结果返回 completed。
	switch {
	case result == nil && isInterrupted:
		return codexToolStatusFailed
	case result == nil:
		return codexToolStatusInProgress
	case result.ToolResult.Error != nil:
		return codexToolStatusFailed
	}
	return codexToolStatusCompleted
}

// isAgentTool 这个工具是不是由 Agent 自己执行的子 Agent 协作工具？
func isAgentTool(name string) bool {
	// 1. 按名称比对子 Agent 协作工具。
	switch name {
	case "spawn_agent", "send_message", "interrupt_agent", "wait_agent", "list_agents", "read_session_history":
		return true
	}
	return false
}

// getMessageText 按顺序拼接消息里的文本内容。
func getMessageText(message *defs.Message) string {
	// 1. 遍历消息内容，拼接文本并跳过图片。
	var text strings.Builder
	for _, content := range message.Content {
		if content.Kind == defs.ContentText {
			text.WriteString(content.Text.Text)
		}
	}
	return text.String()
}

// isStreamingReply Item 是否是回复或推理？
func isStreamingReply(item *defs.Item) bool {
	// 1. 检查非空 Item 的类型。
	return item != nil && (item.Kind == defs.ItemMessage || item.Kind == defs.ItemReasoning)
}

// isTurnContinuedAfter Item 是否说明本轮在指定回复或推理之后还有输出？
func isTurnContinuedAfter(reply, item *defs.Item) bool {
	// 1. 中断标记和 hook 记录不计为后续输出。
	if item.Kind == defs.ItemTurnInterrupted || item.Kind == defs.ItemHookRun {
		return false
	}
	// 2. 工具调用、工具结果或 ModelCallID 不同的 Item 计为后续输出，包括 ID 为空的插话输入。
	return item.Kind == defs.ItemToolCall || item.Kind == defs.ItemToolResult || item.ModelCallID != reply.ModelCallID
}

// getAgentMessagePhase 按日志顺序确定回复的 phase。
func getAgentMessagePhase(items []*defs.Item, index int) string {
	// 1. 同一轮后续仍有输出时返回 commentary，否则返回 final_answer。
	reply := items[index]
	for _, item := range items[index+1:] {
		if item.TurnID == reply.TurnID && isTurnContinuedAfter(reply, item) {
			return codexMessagePhaseCommentary
		}
	}
	return codexMessagePhaseFinalAnswer
}

// getCodexShellCommand 将命令转换为 bash -lc 调用。
func getCodexShellCommand(command string) string {
	// 1. 空命令返回空串，检查命令是否只含安全字符。
	if command == "" {
		return ""
	}
	isSafe := !strings.ContainsFunc(command, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_@%+=:,./-", r))
	})
	// 2. 只含安全字符时原样拼接，否则用单引号包起来并转义里面的单引号。
	if isSafe {
		return "bash -lc " + command
	}
	return "bash -lc '" + strings.ReplaceAll(command, "'", `'"'"'`) + "'"
}

// getAbsolutePath 返回指定目录下的绝对路径。
func getAbsolutePath(directory, path string) string {
	// 1. 绝对路径只做规范化，其他路径与目录拼接；空路径返回目录。
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(directory, path)
}

// getArgumentsJSON 将工具参数转换为 JSON 值。
func getArgumentsJSON(arguments string) json.RawMessage {
	// 1. 空参数返回空对象，合法 JSON 原样返回，其他参数编码为 JSON 字符串。
	if strings.TrimSpace(arguments) == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(arguments)) {
		return json.RawMessage(arguments)
	}
	encoded, _ := json.Marshal(arguments)
	return encoded
}

// getTimestampMs 返回 Item 的保存时间。
func getTimestampMs(item *defs.Item) *int64 {
	// 1. UpdatedAt 为 0（还没保存）时返回 nil，否则返回它的 Unix 毫秒值。
	if item.UpdatedAt == 0 {
		return nil
	}
	updatedAt := item.UpdatedAt
	return &updatedAt
}

// newCodexSubAgentActivity 创建子 Agent 活动记录。
func newCodexSubAgentActivity(id defs.ItemID, kind, agentThreadID, agentPath string) *codexSubAgentActivityItem {
	// 1. 填写显示 Item ID、活动类型、涉及的 Thread ID 和 Agent 路径。
	return &codexSubAgentActivityItem{
		Type:          codexItemSubAgentActivity,
		ID:            string(id),
		Kind:          kind,
		AgentThreadID: agentThreadID,
		AgentPath:     agentPath,
	}
}

// newCodexNotification 创建带 JSON 参数的通知。
func newCodexNotification(method string, params any) (codexMessage, error) {
	// 1. 编码通知参数，成功后填写方法名和参数。
	encoded, err := json.Marshal(params)
	if err != nil {
		return codexMessage{}, err
	}
	return codexMessage{
		Method: method,
		Params: encoded,
	}, nil
}

// getLoadedThreadsPage 将已加载 Thread ID 排序后分页。
func getLoadedThreadsPage(threadIDs []string, cursor *string, limit *int) codexThreadLoadedListResult {
	// 1. 按 ID 排序，从游标之后确定起点。
	sorted := slices.Sorted(slices.Values(threadIDs))
	start := 0
	if cursor != nil {
		index, found := slices.BinarySearch(sorted, *cursor)
		start = index
		if found {
			start++
		}
	}
	// 2. 省略页大小时读到末尾，否则按至少一条截取。
	end := len(sorted)
	if limit != nil {
		end = min(start+max(*limit, 1), end)
	}
	// 3. 返回当前页，后面还有数据时用本页最后一个 ID 作为游标。
	result := codexThreadLoadedListResult{Data: append([]string{}, sorted[start:end]...)}
	if end < len(sorted) {
		last := sorted[end-1]
		result.NextCursor = &last
	}
	return result
}

// getStartedThreadItem 转换开始通知使用的 Item。
func getStartedThreadItem(item codexThreadItem) codexThreadItem {
	// 1. 复制回复或推理并清空内容，其他 Item 原样返回。
	switch item := item.(type) {
	case *codexAgentMessageItem:
		started := *item
		started.Text = ""
		return &started
	case *codexReasoningItem:
		started := *item
		started.Summary, started.Content = []string{}, []string{}
		return &started
	}
	return item
}

// getMissingText 返回完整文本中尚未发出的字节。
func getMissingText(full string, sentBytes int) string {
	// 1. 没有新内容时返回空串，否则截取已发送字节之后的文本。
	if len(full) <= sentBytes {
		return ""
	}
	return full[sentBytes:]
}

// getHistoryPageLimit 返回历史查询的分页条数。
func getHistoryPageLimit(limit *int) int {
	// 1. 缺省使用 25，指定时限制在 1 到 100。
	if limit == nil {
		return defaultHistoryPageLimit
	}
	return min(max(*limit, 1), maxHistoryPageLimit)
}

// getSortDirection 解析历史排序方向。
func getSortDirection(direction *string, defaultDirection storage.Direction) (storage.Direction, error) {
	// 1. 省略时使用默认方向，asc 和 desc 转为存储方向，其他值返回 -32602。
	switch {
	case direction == nil:
		return defaultDirection, nil
	case *direction == codexSortDesc:
		return storage.DirectionDesc, nil
	case *direction == codexSortAsc:
		return storage.DirectionAsc, nil
	}
	return "", &codexError{
		errorCodeInvalidParams,
		"invalid sortDirection: " + *direction,
	}
}

// getTurnsItemsView 解析 Turn 列表的 Item 展示方式。
func getTurnsItemsView(itemsView *string) (string, error) {
	// 1. 省略时使用 summary，只接受 notLoaded、summary 和 full，其他值返回 -32602。
	switch {
	case itemsView == nil:
		return codexTurnItemsSummary, nil
	case *itemsView == codexTurnItemsNotLoaded, *itemsView == codexTurnItemsSummary, *itemsView == codexTurnItemsFull:
		return *itemsView, nil
	}
	return "", &codexError{
		errorCodeInvalidParams,
		"invalid itemsView: " + *itemsView,
	}
}

// getUUIDCursor 解析 UUID 游标。
func getUUIDCursor(cursor *string) (string, error) {
	// 1. 缺少游标时返回空串，非 UUID 返回 -32602，其他值原样返回。
	if cursor == nil {
		return "", nil
	}
	if _, err := uuid.Parse(*cursor); err != nil {
		return "", &codexError{
			errorCodeInvalidParams,
			"invalid cursor: " + *cursor,
		}
	}
	return *cursor, nil
}

// getTurnsCursor 解析 Turn 列表游标。
func getTurnsCursor(cursor *string) (string, error) {
	// 1. 最新历史游标返回空串，其他游标按 UUID 解析。
	if cursor != nil && *cursor == codexNewestHistoryCursor {
		return "", nil
	}
	return getUUIDCursor(cursor)
}

// getItemsCursor 解析 Item 流水号游标。
func getItemsCursor(cursor *string) (storage.Ordinal, error) {
	// 1. 缺少游标或最新历史游标返回 0，其他游标必须为正整数，否则返回 -32602。
	if cursor == nil || *cursor == codexNewestHistoryCursor {
		return 0, nil
	}
	ordinal, err := strconv.ParseUint(*cursor, 10, 64)
	if err != nil || ordinal == 0 {
		return 0, &codexError{
			errorCodeInvalidParams,
			"invalid cursor: " + *cursor,
		}
	}
	return storage.Ordinal(ordinal), nil
}

// newThreadListCursor 将对话列表位置编码为 Thread 列表游标。
func newThreadListCursor(cursor dao.RecordCursor[defs.ConversationID]) *string {
	// 1. 将位置编码为 JSON，再转换为不带填充的 URL-safe base64 字符串。
	encoded, _ := json.Marshal(cursor)
	text := base64.RawURLEncoding.EncodeToString(encoded)
	return &text
}

// getThreadListCursor 解析 Thread 列表游标。
func getThreadListCursor(cursor *string) (dao.RecordCursor[defs.ConversationID], error) {
	// 1. 缺少游标时返回零值，否则解码 base64 和 JSON；位置缺少 ID 或解码失败时返回 -32602。
	if cursor == nil {
		return dao.RecordCursor[defs.ConversationID]{}, nil
	}
	var position dao.RecordCursor[defs.ConversationID]
	encoded, err := base64.RawURLEncoding.DecodeString(*cursor)
	if err != nil || json.Unmarshal(encoded, &position) != nil || position.ID == "" {
		return dao.RecordCursor[defs.ConversationID]{}, &codexError{
			errorCodeInvalidParams,
			"invalid cursor: " + *cursor,
		}
	}
	return position, nil
}

// isSubAgentSourceKinds 来源类型中是否包含子 Agent？
func isSubAgentSourceKinds(sourceKinds []string) bool {
	// 1. 检查是否包含 subAgent 或 subAgentThreadSpawn。
	return slices.ContainsFunc(sourceKinds, func(kind string) bool {
		return kind == codexSourceKindSubAgent || kind == codexSourceKindSubAgentThreadSpawn
	})
}

// getDefaultExecutorDetails 返回配置顺序中的第一个 Executor。
func getDefaultExecutorDetails(executors []conversation.ExecutorDetails) conversation.ExecutorDetails {
	// 1. 没有 Executor 时返回零值，否则返回第一项。
	if len(executors) == 0 {
		return conversation.ExecutorDetails{}
	}
	return executors[0]
}
