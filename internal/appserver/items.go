package appserver

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/executor/tools"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/google/uuid"
)

// codexItemTranslator 把一个 Session 日志里的 OpenVaala Item 翻成 Codex 的 ThreadItem。
// 翻译只依赖 Item 和下面这些只读事实，不持有运行状态；实时流和历史翻页走同一套。
type codexItemTranslator struct {
	// sessionID 是被翻译日志所属的 Session；spawn_agent 新建的子 Agent 路径接在它的 agent 路径后面。
	sessionID defs.SessionID
	// agentPaths 是对话里每个 Session 的 Codex agent 路径：根是 /root，子 Session 沿 ParentSessionID 链是 /root/<任务名>/<任务名>…。
	agentPaths map[defs.SessionID]string
	// workspaceDirectories 是每个 Executor 的工作目录，工具参数里的相对路径以它为基准。
	workspaceDirectories map[string]string
	// defaultExecutorID 是工具参数没有指定 executor 时使用的 Executor。
	defaultExecutorID string
}

// newCodexItemTranslator 创建指定 Session 的 Item 翻译器。
func newCodexItemTranslator(sessionID defs.SessionID, agentPaths map[defs.SessionID]string, workspaceDirectories map[string]string, defaultExecutorID string) *codexItemTranslator {
	// 1. 保存 Session、Agent 路径、Executor 目录和默认 Executor。
	return &codexItemTranslator{
		sessionID:            sessionID,
		agentPaths:           agentPaths,
		workspaceDirectories: workspaceDirectories,
		defaultExecutorID:    defaultExecutorID,
	}
}

// codexToolArguments 是翻译用到的各工具参数；每个工具只会带其中几项。参数解析不了（被中断的调用参数被截断，或类型不对）时，解析不了的字段保持空值。
type codexToolArguments struct {
	Executor  string `json:"executor"`
	Command   string `json:"command"`
	CWD       string `json:"cwd"`
	Patch     string `json:"patch"`
	TaskName  string `json:"task_name"`
	SessionID string `json:"session_id"`
}

// getThreadItems 将正序日志转换为 Codex Item 列表。
func (t *codexItemTranslator) getThreadItems(items []*defs.Item) []codexThreadItemEntry {
	// 1. 截取 session_fork 之后的内容，按 CallID 配对工具结果。
	items = getItemsAfterSessionFork(items)
	resultsByCall := getToolResultsByCall(items)
	entries := make([]codexThreadItemEntry, 0, len(items))
	// 2. 按原始位置转换工具调用或独立 Item，不显示的 Item 和无调用的结果跳过；回复 phase 只按本段后续输出确定，跨页输出不计入。
	for index, item := range items {
		var threadItem codexThreadItem
		var result *defs.Item
		if item.Kind == defs.ItemToolCall {
			result = resultsByCall[item]
			threadItem = t.getToolItem(item, result, item.ToolCall.Status == defs.ToolCallInterrupted)
		} else {
			threadItem = t.getStandaloneItem(item)
		}
		if message, isAgentMessage := threadItem.(*codexAgentMessageItem); isAgentMessage {
			message.Phase = getAgentMessagePhase(items, index)
		}
		if threadItem == nil {
			continue
		}
		// 3. 填写所属 Turn 和保存时间，工具的完成时间取结果的保存时间。
		entry := codexThreadItemEntry{
			TurnID:      string(item.TurnID),
			Item:        threadItem,
			StartedAtMs: getTimestampMs(item),
		}
		switch {
		case result != nil:
			entry.CompletedAtMs = getTimestampMs(result)
		case item.Kind != defs.ItemToolCall:
			entry.CompletedAtMs = getTimestampMs(item)
		}
		entries = append(entries, entry)
	}
	return entries
}

// getItemsAfterSessionFork 返回 Session 分叉标记之后的 Item。
func getItemsAfterSessionFork(items []*defs.Item) []*defs.Item {
	// 1. 查找 session_fork，存在时去掉分界及之前内容，否则返回全部。
	forkIndex := slices.IndexFunc(items, func(item *defs.Item) bool { return item.Kind == defs.ItemSessionFork })
	return items[forkIndex+1:]
}

// getToolResultsByCall 按 CallID 将结果配给之前最近的未配对调用。
func getToolResultsByCall(items []*defs.Item) map[*defs.Item]*defs.Item {
	// 1. 顺序登记调用，结果到达时配对并移除等待记录；未匹配的调用和结果不加入返回映射。
	resultsByCall := make(map[*defs.Item]*defs.Item)
	callsWithoutResult := make(map[string]*defs.Item)
	for _, item := range items {
		switch item.Kind {
		case defs.ItemToolCall:
			callsWithoutResult[item.ToolCall.CallID] = item
		case defs.ItemToolResult:
			if call, found := callsWithoutResult[item.ToolResult.CallID]; found {
				resultsByCall[call] = item
				delete(callsWithoutResult, item.ToolResult.CallID)
			}
		}
	}
	return resultsByCall
}

// getStartedItem 返回参数收齐时显示的工具 Item。
func (t *codexItemTranslator) getStartedItem(call *defs.Item) codexThreadItem {
	// 1. 按无结果、未中断转换；等待结果的协作工具和补丁调用返回 nil，其他调用显示 inProgress。
	return t.getToolItem(call, nil, false)
}

// getCompletedItem 转换工具结束时显示的 Item。
func (t *codexItemTranslator) getCompletedItem(call, result *defs.Item) codexThreadItem {
	// 1. 按结果转换，不显示的调用返回 nil；没有结果时按中断显示 failed。
	return t.getToolItem(call, result, result == nil)
}

// getStandaloneItem 转换用户消息、回复、完成通知、推理和摘要。
func (t *codexItemTranslator) getStandaloneItem(item *defs.Item) codexThreadItem {
	// 1. 按 Item 类型转换已有内容，工具调用、工具结果和其他不显示的 Item 返回 nil。
	switch item.Kind {
	case defs.ItemMessage:
		return t.getMessageItem(item)
	case defs.ItemReasoning:
		return &codexReasoningItem{
			Type:    codexItemReasoning,
			ID:      string(item.ID),
			Summary: append([]string{}, item.Reasoning.Summary...),
			Content: append([]string{}, item.Reasoning.Content...),
		}
	case defs.ItemContextSummary:
		return &codexContextCompactionItem{
			Type: codexItemContextCompaction,
			ID:   string(item.ID),
		}
	}
	return nil
}

// getMessageItem 将用户输入、子 Agent 完成通知和模型回复转为 Codex Item。
func (t *codexItemTranslator) getMessageItem(item *defs.Item) codexThreadItem {
	// 1. 无来源不显示；用户转换文本和图片，子 Agent 通知转换完成活动，带 ModelCallID 的 Agent 消息转换回复。
	message := item.Message
	if message.From == nil {
		return nil
	}
	switch {
	case message.From.Kind == defs.IdentityUser:
		content := make([]codexUserInput, 0, len(message.Content))
		for _, part := range message.Content {
			if part.Kind == defs.ContentImage {
				content = append(content, &codexImageUserInput{
					Type: codexUserInputImage,
					URL:  part.Image.DataURL,
				})
				continue
			}
			content = append(content, &codexTextUserInput{
				Type:         codexUserInputText,
				Text:         part.Text.Text,
				TextElements: []struct{}{},
			})
		}
		return &codexUserMessageItem{
			Type:    codexItemUserMessage,
			ID:      string(item.ID),
			Content: content,
		}
	case message.SubagentNotification != nil:
		return newCodexSubAgentActivity(item.ID, codexActivityCompleted, message.From.ID, t.getSessionAgentPath(defs.SessionID(message.From.ID)))
	case message.From.Kind == defs.IdentityAgent && item.ModelCallID != "":
		return &codexAgentMessageItem{
			Type: codexItemAgentMessage,
			ID:   string(item.ID),
			Text: getMessageText(message),
		}
	}
	return nil
}

// getToolItem 按工具类型和结果转换调用。
func (t *codexItemTranslator) getToolItem(call, result *defs.Item, isInterrupted bool) codexThreadItem {
	// 1. 跳过缺少 CallID 或工具名的残片，以及 wait_agent、list_agents。
	name := call.ToolCall.Name
	if call.ToolCall.CallID == "" || name == "" || name == "wait_agent" || name == "list_agents" {
		return nil
	}
	// 2. 解析工具参数：截断的 JSON 一个字段也解不出，类型不对的字段保持零值；被中断的 bash 因此显示空命令和 failed。
	var arguments codexToolArguments
	_ = json.Unmarshal([]byte(call.ToolCall.Arguments), &arguments)
	// 3. bash 直接显示；补丁和协作工具既无结果也未中断时返回 nil，成功协作转换为活动，其他调用转换为 MCP 工具项。
	switch name {
	case "bash":
		return t.getCommandExecutionItem(call, result, &arguments, isInterrupted)
	case "apply_patch":
		if result == nil && !isInterrupted {
			return nil
		}
		return t.getFileChangeItem(call, result, &arguments, isInterrupted)
	case "spawn_agent", "send_message", "interrupt_agent":
		if activity := t.getSubAgentActivityItem(call, result, &arguments); activity != nil {
			return activity
		}
		if result == nil && !isInterrupted {
			return nil
		}
	}
	return t.getMcpToolCallItem(call, result, &arguments, isInterrupted)
}

// getCommandExecutionItem 将 bash 调用转换为命令执行 Item。
func (t *codexItemTranslator) getCommandExecutionItem(call, result *defs.Item, arguments *codexToolArguments, isInterrupted bool) *codexCommandExecutionItem {
	// 1. 填写转义后的 bash -lc 命令、Executor 下的 cwd 和调用状态，还没有结果时直接返回。
	item := &codexCommandExecutionItem{
		Type:           codexItemCommandExecution,
		ID:             string(call.ID),
		Command:        getCodexShellCommand(arguments.Command),
		Cwd:            getAbsolutePath(t.getWorkspaceDirectory(arguments, result), arguments.CWD),
		Status:         getToolStatus(result, isInterrupted),
		CommandActions: []struct{}{},
		Source:         "agent",
	}
	if result == nil {
		return item
	}
	// 2. 解析命令输出和退出码（参数错误等没启动命令的结果不带这些），追加执行错误；没有输出时用调用的错误文本。
	command, _ := tools.ParseCommandResult(result.ToolResult.Content)
	aggregatedOutput := command.Output
	switch {
	case command.Error != "":
		if aggregatedOutput != "" && !strings.HasSuffix(aggregatedOutput, "\n") {
			aggregatedOutput += "\n"
		}
		aggregatedOutput += command.Error
	case aggregatedOutput == "" && result.ToolResult.Error != nil:
		aggregatedOutput = result.ToolResult.Error.Message
	}
	// 3. 命令失败或非零退出码标为 failed，后台运行但调用已返回时为 completed，填入输出和退出码。
	if command.IsFailed() {
		item.Status = codexToolStatusFailed
	}
	item.AggregatedOutput, item.ExitCode = &aggregatedOutput, command.ExitCode
	return item
}

// getFileChangeItem 将 apply_patch 调用转换为文件变更 Item。
func (t *codexItemTranslator) getFileChangeItem(call, result *defs.Item, arguments *codexToolArguments, isInterrupted bool) *codexFileChangeItem {
	// 1. 取得实际 Executor 目录，读取补丁结果和各段起始行。
	directory := t.getWorkspaceDirectory(arguments, result)
	var content struct {
		Outcome          string           `json:"outcome"`
		ChangeStartLines map[string][]int `json:"change_start_lines"`
	}
	if result != nil {
		_ = json.Unmarshal(result.ToolResult.Content, &content)
	}
	// 2. 解析补丁，转换绝对路径和 diff；无效补丁的变更列表为空。
	patchChanges, _ := tools.GetPatchFileChanges(arguments.Patch)
	changes := make([]codexFileUpdateChange, 0, len(patchChanges))
	for _, change := range patchChanges {
		changes = append(changes, codexFileUpdateChange{
			Path: getAbsolutePath(directory, change.Path),
			Kind: codexPatchChangeKind{Type: string(change.Kind)},
			Diff: getCodexFileChangeDiff(change, content.ChangeStartLines[change.Path]),
		})
	}
	// 3. 创建变更 Item，结果为 applied 时完成，部分应用或失败时标为 failed。
	item := &codexFileChangeItem{
		Type:    codexItemFileChange,
		ID:      string(call.ID),
		Changes: changes,
		Status:  getToolStatus(result, isInterrupted),
	}
	if result == nil {
		return item
	}
	item.Status = codexToolStatusFailed
	if content.Outcome == "applied" {
		item.Status = codexToolStatusCompleted
	}
	return item
}

// getCodexFileChangeDiff 将文件变更转换为 Codex diff。
func getCodexFileChangeDiff(change tools.PatchFileChange, startLines []int) string {
	// 1. 新增返回文件内容，删除返回空串。
	switch change.Kind {
	case tools.PatchFileAdd:
		return change.Content
	case tools.PatchFileDelete:
		return ""
	}
	// 2. 逐段统计旧新行数，用结果起始行和累计行差写 unified diff 头；没有对应行号时保留补丁段头。
	var diff strings.Builder
	lineOffset := 0
	for index, section := range change.Sections {
		if len(startLines) != len(change.Sections) {
			diff.WriteString(section.Header + "\n")
		} else {
			oldCount, newCount := 0, 0
			for _, line := range section.Lines {
				switch line[0] {
				case ' ':
					oldCount, newCount = oldCount+1, newCount+1
				case '-':
					oldCount++
				case '+':
					newCount++
				}
			}
			fmt.Fprintf(&diff, "@@ -%d,%d +%d,%d @@\n", startLines[index], oldCount, startLines[index]+lineOffset, newCount)
			lineOffset += newCount - oldCount
		}
		// 3. 写入每段原始变更行并返回 diff。
		for _, line := range section.Lines {
			diff.WriteString(line + "\n")
		}
	}
	return diff.String()
}

// getMcpToolCallItem 将其他工具调用转换为 MCP 工具 Item。
func (t *codexItemTranslator) getMcpToolCallItem(call, result *defs.Item, arguments *codexToolArguments, isInterrupted bool) *codexMcpToolCallItem {
	// 1. MCP 工具使用 server ID 和原工具名，Agent 工具使用 agent，其余使用实际 Executor。
	server, tool := "agent", call.ToolCall.Name
	if serverID, toolName, isMCPTool := mcpclient.GetToolNameParts(tool); isMCPTool {
		server, tool = serverID, toolName
	} else if !isAgentTool(tool) {
		server = t.getExecutorID(arguments, result)
	}
	// 2. 填写参数和状态，无结果时返回，错误结果只填写 error。
	item := &codexMcpToolCallItem{
		Type:      codexItemMcpToolCall,
		ID:        string(call.ID),
		Server:    server,
		Tool:      tool,
		Status:    getToolStatus(result, isInterrupted),
		Arguments: getArgumentsJSON(call.ToolCall.Arguments),
	}
	if result == nil {
		return item
	}
	if result.ToolResult.Error != nil {
		item.Error = &codexMcpToolCallError{Message: result.ToolResult.Error.Message}
		return item
	}
	// 3. 提取结果正文，命令结果使用输出文本，再附加图片和 JSON 原文。
	text := executor.GetToolResultText(result.ToolResult.Content)
	// read_command_output 的正文是命令输出。
	if command, isCommandResult := tools.ParseCommandResult(result.ToolResult.Content); isCommandResult {
		text = command.Output
	}
	content := []codexMcpContent{&codexMcpTextContent{
		Type: "text",
		Text: text,
	}}
	for _, image := range result.ToolResult.Images {
		mimeType, data, _ := image.GetDataURLParts()
		content = append(content, &codexMcpImageContent{
			Type:     "image",
			Data:     data,
			MimeType: mimeType,
		})
	}
	item.Result = &codexMcpToolResult{
		Content:           content,
		StructuredContent: result.ToolResult.Content,
	}
	return item
}

// getSubAgentActivityItem 将成功的子 Agent 协作调用转换为活动。
func (t *codexItemTranslator) getSubAgentActivityItem(call, result *defs.Item, arguments *codexToolArguments) *codexSubAgentActivityItem {
	// 1. 无结果或失败返回 nil；spawn_agent 从结果取得 Session ID 和任务路径，发送消息与中断使用参数目标。
	if result == nil || result.ToolResult.Error != nil {
		return nil
	}
	switch call.ToolCall.Name {
	case "spawn_agent":
		var content struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(result.ToolResult.Content, &content) != nil || content.SessionID == "" {
			return nil
		}
		path := t.getSessionAgentPath(t.sessionID) + "/" + arguments.TaskName
		return newCodexSubAgentActivity(call.ID, codexActivityStarted, content.SessionID, path)
	case "send_message":
		return t.newTargetedSubAgentActivity(call.ID, codexActivityInteracted, arguments.SessionID)
	case "interrupt_agent":
		return t.newTargetedSubAgentActivity(call.ID, codexActivityInterrupted, arguments.SessionID)
	}
	return nil
}

// newTargetedSubAgentActivity 创建指向指定 Session 的子 Agent 活动。
func (t *codexItemTranslator) newTargetedSubAgentActivity(id defs.ItemID, kind, targetSessionID string) *codexSubAgentActivityItem {
	// 1. 缺少目标时返回 nil，否则填入目标 Session 的 Agent 路径。
	if targetSessionID == "" {
		return nil
	}
	return newCodexSubAgentActivity(id, kind, targetSessionID, t.getSessionAgentPath(defs.SessionID(targetSessionID)))
}

// getSessionAgentPath 读取 Session 的 Agent 路径。
func (t *codexItemTranslator) getSessionAgentPath(sessionID defs.SessionID) string {
	// 1. 先查 agentPaths，没有时用 /root/<SessionID>。
	return cmp.Or(t.agentPaths[sessionID], "/root/"+string(sessionID))
}

// getExecutorID 返回工具调用实际使用的 Executor。
func (t *codexItemTranslator) getExecutorID(arguments *codexToolArguments, result *defs.Item) string {
	// 1. 优先使用结果中的执行者，否则使用参数 executor，最后使用默认 Executor。
	if result != nil && result.ToolResult.Executor != "" {
		return result.ToolResult.Executor
	}
	return cmp.Or(arguments.Executor, t.defaultExecutorID)
}

// getWorkspaceDirectory 返回工具调用所用 Executor 的工作目录。
func (t *codexItemTranslator) getWorkspaceDirectory(arguments *codexToolArguments, result *defs.Item) string {
	// 1. 按调用实际 Executor 查询目录。
	return t.workspaceDirectories[t.getExecutorID(arguments, result)]
}

// isToolCallSplit 本页是否拆开了工具调用和结果？
func isToolCallSplit(items []*defs.Item, order storage.Direction) bool {
	// 1. 收集页内 ToolCall 和 ToolResult 的 CallID。
	callIDs, resultIDs := make(map[string]bool), make(map[string]bool)
	for _, item := range items {
		switch item.Kind {
		case defs.ItemToolCall:
			callIDs[item.ToolCall.CallID] = true
		case defs.ItemToolResult:
			resultIDs[item.ToolResult.CallID] = true
		}
	}
	// 2. 倒序检查无调用的结果，正序检查未中断且无结果的调用；找到时返回 true。
	for _, item := range items {
		switch {
		case order == storage.DirectionDesc && item.Kind == defs.ItemToolResult && !callIDs[item.ToolResult.CallID]:
			return true
		case order == storage.DirectionAsc && item.Kind == defs.ItemToolCall && item.ToolCall.Status != defs.ToolCallInterrupted && !resultIDs[item.ToolCall.CallID]:
			return true
		}
	}
	return false
}

// ── Thread、Turn 翻译 ──

// newCodexRootThread 从对话记录和模型选择创建根 Thread。
func newCodexRootThread(record defs.Conversation, model defs.ModelSelection, pathMappings config.PathMappings, isWindowsClient bool, status codexThreadStatus, version string) codexThread {
	// 1. 截取标题前 256 个字符，空标题和空强度使用 null，填写分叉来源。
	var name, reasoningEffort *string
	title := defs.TruncateConversationTitle(record.Title)
	if title != "" {
		name = &title
	}
	if model.ReasoningEffort != "" {
		effort := model.ReasoningEffort
		reasoningEffort = &effort
	}
	var forkedFromID *string
	if record.ForkedFromID != "" {
		source := string(record.ForkedFromID)
		forkedFromID = &source
	}
	// 2. Thread 与 Session ID 使用对话 ID，cwd 使用归属目录的客户端映射，时间转为 Unix 秒，允许直接输入。
	return codexThread{
		ID:                   string(record.ID),
		SessionID:            string(record.ID),
		Name:                 name,
		Preview:              title,
		Cwd:                  pathMappings.GetClientPath(record.ExecutorSettings.GetConversationDirectory(), isWindowsClient),
		ModelProvider:        model.Ref.ProviderID,
		Model:                model.Ref.String(),
		ReasoningEffort:      reasoningEffort,
		CreatedAt:            getUUIDv7Seconds(string(record.ID)),
		UpdatedAt:            record.UpdatedAt / 1000,
		Status:               status,
		Source:               codexAppServerSource("appServer"),
		CanAcceptDirectInput: true,
		HistoryMode:          "paginated",
		CLIVersion:           version,
		ForkedFromID:         forkedFromID,
		Turns:                []codexTurn{},
	}
}

// newCodexChildThread 从子 Session 和层级记录创建子 Thread。
func newCodexChildThread(record defs.Conversation, child subAgentSession, pathMappings config.PathMappings, isWindowsClient bool, status codexThreadStatus, version string) codexThread {
	// 1. 沿用对话标题和目录，替换子 Session ID、创建及更新时间，填入父 Thread、层数和 Agent 信息，禁止直接输入并清空分叉来源。
	thread := newCodexRootThread(record, child.record.Model, pathMappings, isWindowsClient, status, version)
	nickname, role, parentThreadID := child.record.TaskName, child.record.AgentName, child.parentThreadID
	thread.ID, thread.SessionID = string(child.record.ID), string(child.record.ID)
	thread.CreatedAt = getUUIDv7Seconds(string(child.record.ID))
	thread.UpdatedAt = cmp.Or(child.record.UpdatedAt/1000, thread.CreatedAt)
	thread.Source = codexSubAgentSource{SubAgent: codexSubAgentSpawn{ThreadSpawn: codexThreadSpawn{
		ParentThreadID: parentThreadID,
		Depth:          len(child.ancestorSessionIDs),
		AgentNickname:  nickname,
		AgentRole:      role,
	}}}
	thread.ParentThreadID, thread.AgentNickname, thread.AgentRole = &parentThreadID, &nickname, &role
	thread.CanAcceptDirectInput, thread.ForkedFromID = false, nil
	return thread
}

// newCodexHistoryThread 按存储记录创建不带 Turn 的历史 Thread。
func newCodexHistoryThread(thread historyThread, sessions []defs.Session, pathMappings config.PathMappings, isWindowsClient bool, status codexThreadStatus, version string) codexThread {
	// 1. 找到子 Session 时创建子 Thread，否则读取根 Session 模型创建根 Thread，状态由调用方提供。
	subAgents := getSubAgentSessions(thread.conversation, sessions)
	if index := slices.IndexFunc(subAgents, func(subAgent subAgentSession) bool { return subAgent.record.ID == thread.sessionID }); index >= 0 {
		return newCodexChildThread(thread.conversation, subAgents[index], pathMappings, isWindowsClient, status, version)
	}
	var model defs.ModelSelection
	if index := slices.IndexFunc(sessions, func(record defs.Session) bool { return record.ID == thread.sessionID }); index >= 0 {
		model = sessions[index].Model
	}
	return newCodexRootThread(thread.conversation, model, pathMappings, isWindowsClient, status, version)
}

// newCodexThreadOpenResult 创建 Thread 打开结果和历史起点游标；hasExecutors 为 false（对话没有装配 Executor）时沙箱报告只读。
func newCodexThreadOpenResult(thread codexThread, hasExecutors bool) codexThreadOpenResult {
	// 1. 有 Executor 的 Thread 使用全访问沙箱，没有 Executor 的使用只读沙箱，复制模型和目录并填写 newest 游标。
	sandbox := codexSandboxPolicy{Type: "dangerFullAccess"}
	if !hasExecutors {
		sandbox.Type = "readOnly"
	}
	return codexThreadOpenResult{
		Thread:               thread,
		Model:                thread.Model,
		ModelProvider:        thread.ModelProvider,
		ReasoningEffort:      thread.ReasoningEffort,
		Cwd:                  thread.Cwd,
		ApprovalPolicy:       "never",
		ApprovalsReviewer:    "user",
		Sandbox:              sandbox,
		TurnsBackwardsCursor: codexNewestHistoryCursor,
		ItemsBackwardsCursor: codexNewestHistoryCursor,
	}
}

// getUUIDv7Seconds 读取 UUID 中的创建时间，返回 Unix 秒。
func getUUIDv7Seconds(id string) int64 {
	// 1. 解析 UUID，失败返回 0，成功读取时间并转为秒。
	parsed, err := uuid.Parse(id)
	if err != nil {
		return 0
	}
	seconds, _ := parsed.Time().UnixTime()
	return seconds
}

// getCodexThreadStatusType 转换 Session 的 Thread 状态。
func getCodexThreadStatusType(info runtime.SessionRuntimeInfo) string {
	// 1. 存在活动 Turn 返回 active，否则返回 idle。
	if info.ActiveTurnID != "" {
		return codexThreadStatusActive
	}
	return codexThreadStatusIdle
}

// newCodexThreadStatus 创建 Codex Thread 状态。
func newCodexThreadStatus(statusType string) codexThreadStatus {
	// 1. 填写状态类型，仅 active 初始化为空的非 nil activeFlags 列表。
	status := codexThreadStatus{Type: statusType}
	if statusType == codexThreadStatusActive {
		status.ActiveFlags = []string{}
	}
	return status
}

// getCodexThreadTokenUsage 将 Session 累计用量和上下文占用转换为 Thread 用量。
func getCodexThreadTokenUsage(info runtime.SessionRuntimeInfo) codexThreadTokenUsage {
	// 1. total 填累计计数及输入输出之和，last 只填上下文 token 数，已知上下文窗口时填入窗口大小。
	usage := codexThreadTokenUsage{
		Total: codexTokenUsageBreakdown{
			InputTokens:           info.Usage.Input,
			CachedInputTokens:     info.Usage.CacheRead,
			CacheWriteInputTokens: info.Usage.CacheWrite,
			OutputTokens:          info.Usage.Output,
			ReasoningOutputTokens: info.Usage.Reasoning,
			TotalTokens:           info.Usage.Input + info.Usage.Output,
		},
		Last: codexTokenUsageBreakdown{
			InputTokens: info.Context.Tokens,
			TotalTokens: info.Context.Tokens,
		},
	}
	if info.Context.Window > 0 {
		window := info.Context.Window
		usage.ModelContextWindow = &window
	}
	return usage
}

// getAgentPath 按任务名层级拼出 Agent 显示路径。
func getAgentPath(taskNames []string) string {
	// 1. 从 /root 开始逐级追加原始任务名。
	path := "/root"
	for _, taskName := range taskNames {
		path += "/" + taskName
	}
	return path
}

// getAgentPaths 沿各 Session 父链生成 Agent 路径。
func getAgentPaths(sessions []defs.Session) map[defs.SessionID]string {
	// 1. 按 Session ID 建立记录索引。
	sessionsByID := make(map[defs.SessionID]defs.Session, len(sessions))
	for _, record := range sessions {
		sessionsByID[record.ID] = record
	}
	// 2. 沿父链收集任务名，父记录缺失时结束，最多走 len(sessions) 级，再反转拼接路径。
	paths := make(map[defs.SessionID]string, len(sessions))
	for _, record := range sessions {
		var taskNames []string
		for ancestor, depth := record, 0; ancestor.ParentSessionID != "" && depth < len(sessions); depth++ {
			taskNames = append(taskNames, ancestor.TaskName)
			parent, found := sessionsByID[ancestor.ParentSessionID]
			if !found {
				break
			}
			ancestor = parent
		}
		slices.Reverse(taskNames)
		paths[record.ID] = getAgentPath(taskNames)
	}
	return paths
}

// getSubAgentSessions 收集子 Session 及其父 Thread 和祖先链。
func getSubAgentSessions(record defs.Conversation, sessions []defs.Session) []subAgentSession {
	// 1. 建立 Session 索引，按输入顺序遍历有父 Session 的记录。
	sessionsByID := make(map[defs.SessionID]defs.Session, len(sessions))
	for _, candidate := range sessions {
		sessionsByID[candidate.ID] = candidate
	}
	var subAgents []subAgentSession
	for _, candidate := range sessions {
		if candidate.ParentSessionID == "" {
			continue
		}
		// 2. 沿父链收集祖先，最多走 len(sessions) 级，父记录缺失时结束。
		var ancestorIDs []defs.SessionID
		for parentID := candidate.ParentSessionID; parentID != "" && len(ancestorIDs) < len(sessions); parentID = sessionsByID[parentID].ParentSessionID {
			ancestorIDs = append(ancestorIDs, parentID)
		}
		// 3. 父为根 Session 时使用对话 ID，否则使用父 Session ID，记录子 Session 层级。
		parentThreadID := string(candidate.ParentSessionID)
		if candidate.ParentSessionID == record.RootSessionID {
			parentThreadID = string(record.ID)
		}
		subAgents = append(subAgents, subAgentSession{
			record:             candidate,
			parentThreadID:     parentThreadID,
			ancestorSessionIDs: ancestorIDs,
		})
	}
	return subAgents
}

// getSubAgentSessionsUnder 筛选指定祖先下的子 Agent，保留原顺序。
func getSubAgentSessionsUnder(subAgents []subAgentSession, ancestorSessionID defs.SessionID, isChildrenOnly bool) []subAgentSession {
	// 1. 复制列表，只保留直接子 Session 或祖先链中包含目标的后代。
	return slices.DeleteFunc(slices.Clone(subAgents), func(subAgent subAgentSession) bool {
		if isChildrenOnly {
			return subAgent.record.ParentSessionID != ancestorSessionID
		}
		return !slices.Contains(subAgent.ancestorSessionIDs, ancestorSessionID)
	})
}

// getSubAgentSessionsPage 按 Session 创建顺序分页，返回下一页游标。
func getSubAgentSessionsPage(subAgents []subAgentSession, cursor string, direction storage.Direction, limit int) (page []subAgentSession, nextCursor *string) {
	// 1. 按 Session ID 排序，倒序时反转。
	sorted := slices.SortedFunc(slices.Values(subAgents), func(a, b subAgentSession) int { return cmp.Compare(a.record.ID, b.record.ID) })
	if direction == storage.DirectionDesc {
		slices.Reverse(sorted)
	}
	// 2. 过滤游标及之前记录，超过页大小时截取，以本页最后 ID 为游标。
	if cursor != "" {
		sorted = slices.DeleteFunc(sorted, func(subAgent subAgentSession) bool {
			if direction == storage.DirectionDesc {
				return string(subAgent.record.ID) >= cursor
			}
			return string(subAgent.record.ID) <= cursor
		})
	}
	if len(sorted) <= limit {
		return sorted, nil
	}
	last := string(sorted[limit-1].record.ID)
	return sorted[:limit], &last
}

// newCodexTurn 创建不带 Item 的 Codex Turn。
func newCodexTurn(turnID defs.TurnID, status string, turnError *codexTurnError) codexTurn {
	// 1. 填写 ID、状态和错误，使用空 Item 列表和 notLoaded。
	return codexTurn{
		ID:        string(turnID),
		Status:    status,
		Error:     turnError,
		Items:     []codexThreadItem{},
		ItemsView: codexTurnItemsNotLoaded,
	}
}

// getCodexTurnStatus 转换 Codex Turn 状态。
func getCodexTurnStatus(status defs.TurnStatus) string {
	// 1. running 转为 inProgress，completed 和 interrupted 保留对应状态，其他返回 failed。
	switch status {
	case defs.TurnStatusRunning:
		return codexTurnInProgress
	case defs.TurnStatusCompleted:
		return codexTurnCompleted
	case defs.TurnStatusInterrupted:
		return codexTurnInterrupted
	}
	return codexTurnFailed
}

// getCodexTurnError 转换 Codex Turn 错误。
func getCodexTurnError(status, message string) *codexTurnError {
	// 1. 仅 failed 创建带记录原文的错误，其他状态返回 nil。
	if status != codexTurnFailed {
		return nil
	}
	return &codexTurnError{Message: message}
}

// newCodexRecordTurn 将 Turn 记录转换为不带 Item 的 Codex Turn。
func newCodexRecordTurn(record defs.Turn) codexTurn {
	// 1. 转换状态和失败信息，创建 notLoaded Turn。
	status := getCodexTurnStatus(record.Status)
	return newCodexTurn(record.ID, status, getCodexTurnError(status, record.Error))
}

// newTurnItemTranslator 按 Turn 开始时记录的 Executor 设置创建 Item 翻译器。
func newTurnItemTranslator(sessionID defs.SessionID, agentPaths map[defs.SessionID]string, record defs.Turn) *codexItemTranslator {
	// 1. 收集各 Executor 工作目录，第一个作为默认 Executor；记录无 Executor 时目录和默认 ID 为空。
	workspaceDirectories := make(map[string]string, len(record.Executors))
	var defaultExecutorID string
	for index, turnExecutor := range record.Executors {
		workspaceDirectories[turnExecutor.ID] = turnExecutor.WorkspaceDirectory
		if index == 0 {
			defaultExecutorID = turnExecutor.ID
		}
	}
	return newCodexItemTranslator(sessionID, agentPaths, workspaceDirectories, defaultExecutorID)
}

// outputSchemaInstruction 是 turn/start 带 outputSchema 时接在输入文字后面的格式要求，后面再接 schema 原文；开头的空行把它和输入文字隔开。
const outputSchemaInstruction = "\n\nRespond with only a JSON object that matches this JSON Schema. Do not wrap it in a code block or add any other text.\n"

// getCodexInputContent 将 Codex 输入转换为 OpenVaala 内容，按需追加 JSON 格式要求。
func getCodexInputContent(input []codexUserInputParam, outputSchema json.RawMessage) ([]*defs.Content, error) {
	// 1. 转换文本和 data URL 图片，本机图片按扩展名判断类型后读出并编码；skill 和 mention 跳过，
	// 不是 data URL 的图片、不是图片或读不了的本机文件和其他输入类型返回 -32602。
	content := make([]*defs.Content, 0, len(input))
	for _, part := range input {
		switch part.Type {
		case "text":
			content = append(content, &defs.Content{
				Kind: defs.ContentText,
				Text: &defs.TextContent{Text: part.Text},
			})
		case "image":
			if !strings.HasPrefix(part.URL, "data:") {
				return nil, &codexError{
					errorCodeInvalidParams,
					"unsupported input: image",
				}
			}
			content = append(content, &defs.Content{
				Kind:  defs.ContentImage,
				Image: &defs.ImageContent{DataURL: part.URL},
			})
		case "localImage":
			mimeType, _, _ := mime.ParseMediaType(mime.TypeByExtension(filepath.Ext(part.Path)))
			if !strings.HasPrefix(mimeType, "image/") {
				return nil, &codexError{
					errorCodeInvalidParams,
					"localImage is not an image file: " + part.Path,
				}
			}
			data, err := os.ReadFile(part.Path)
			if err != nil {
				return nil, &codexError{
					errorCodeInvalidParams,
					"cannot read localImage: " + err.Error(),
				}
			}
			dataURL := "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
			content = append(content, &defs.Content{
				Kind:  defs.ContentImage,
				Image: &defs.ImageContent{DataURL: dataURL},
			})
		case "skill", "mention":
		default:
			return nil, &codexError{
				errorCodeInvalidParams,
				"unsupported input: " + part.Type,
			}
		}
	}
	// 2. 没有输出 schema 或为 null 时直接返回。
	if len(outputSchema) == 0 || string(outputSchema) == "null" {
		return content, nil
	}
	// 3. 将格式要求和 schema 接在最后文本之后，无文本时新增一段不带开头空行的文本。
	instruction := outputSchemaInstruction + string(outputSchema)
	for _, part := range slices.Backward(content) {
		if part.Kind == defs.ContentText {
			part.Text.Text += instruction
			return content, nil
		}
	}
	return append(content, &defs.Content{
		Kind: defs.ContentText,
		Text: &defs.TextContent{Text: strings.TrimPrefix(instruction, "\n\n")},
	}), nil
}
