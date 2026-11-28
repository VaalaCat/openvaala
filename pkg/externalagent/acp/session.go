package acp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/externalagent"
	"github.com/VaalaCat/openvaala/pkg/telemetry"

	sdk "github.com/coder/acp-go-sdk"
)

type acpAgentSession struct {
	command    defs.ACPCommand
	openReq    externalagent.ExternalSessionOpenRequest
	ctx        context.Context
	events     chan<- externalagent.ExternalSessionEvent
	connection *acpConnection
	tasks      sync.WaitGroup
	// protocolMu 串行转换 SDK 通知和 RPC 结果；阻塞的 SDK 调用都在锁外执行。
	protocolMu     sync.Mutex
	sessionID      sdk.SessionId
	modes          *sdk.SessionModeState
	config         []sdk.SessionConfigOption
	commands       []defs.ExternalCommand
	replaying      bool
	closing        bool
	promptTurnID   defs.TurnID
	itemIndex      int
	chunk          *protocolChunk
	toolCalls      map[sdk.ToolCallId]*protocolToolCall
	measurement    *agent_context.ContextMeasurement
	preview        externalagent.ExternalSessionPreview
	previewHasUser bool
}

// Run 与原生 Run 一样等到取消或进程退出，SDK 的轮次任务和 stdio 在本次调用内收尾。
func (s *acpAgentSession) Run(ctx context.Context, events chan<- externalagent.ExternalSessionEvent) error {
	if events == nil {
		return errors.New("ACP session requires events")
	}
	req := s.openReq
	ctx, cancel := context.WithCancel(ctx)
	s.ctx, s.events = ctx, events
	defer func() {
		cancel()
		s.protocolMu.Lock()
		s.closing = true
		s.protocolMu.Unlock()
		if s.connection != nil {
			s.connection.close()
		}
		s.tasks.Wait()
	}()
	if b := req.Binding; b != nil {
		s.replaying = true
		s.sessionID = sdk.SessionId(b.SessionID)
		req.Cwd, req.ExecutorID = b.Cwd, b.ExecutorID
	}
	conn, err := s.startACPConnection(ctx, req.Workspace, req.Cwd, req.LaunchEnv)
	if err != nil {
		return err
	}
	s.connection = conn
	servers, names, err := mcpServers(req, conn.capabilities.McpCapabilities.Http)
	if err != nil {
		return err
	}
	if s.replaying {
		if !conn.capabilities.LoadSession {
			return errors.New("external agent does not support session loading")
		}
		load, err := mergeParamOverwrite(sdk.LoadSessionRequest{Cwd: req.Cwd, SessionId: s.sessionID, McpServers: servers}, sdk.AgentMethodSessionLoad, s.getParamOverwrites()...)
		if err != nil {
			return err
		}
		res, err := conn.client.LoadSession(ctx, load)
		if err != nil {
			return rpcError(err)
		}
		s.setSessionResponse(sdk.SessionId(req.Binding.SessionID), res.Modes, res.ConfigOptions)
	} else {
		create, err := mergeParamOverwrite(sdk.NewSessionRequest{Cwd: req.Cwd, McpServers: servers}, sdk.AgentMethodSessionNew, s.getParamOverwrites()...)
		if err != nil {
			return err
		}
		res, err := conn.client.NewSession(ctx, create)
		if err != nil {
			return rpcError(err)
		}
		if res.SessionId == "" {
			return errors.New("external agent returned an empty sessionId")
		}
		s.setSessionResponse(res.SessionId, res.Modes, res.ConfigOptions)
		for _, sel := range req.InitialOptions {
			if err := s.SetOption(ctx, sel); err != nil {
				return err
			}
		}
	}
	s.protocolMu.Lock()
	s.flushChunk(defs.ReasoningCompleted)
	s.replaying = false
	binding := defs.ExternalSession{AgentType: defs.AgentTypeACP, ExecutorID: req.ExecutorID, Cwd: req.Cwd, SessionID: string(s.sessionID)}
	if req.Binding != nil {
		binding.Metadata = req.Binding.Metadata
	}
	s.emit(&externalagent.ExternalSessionReadyEvent{
		Binding: binding, Options: externalOptions(s.modes, s.config), Commands: s.commands, MCPServers: names,
		Capabilities: defs.ExternalCapabilities{SupportsEphemeralSession: len(s.command.EphemeralParamOverwrite) > 0},
	})
	s.protocolMu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-conn.exited:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return conn.exitErr
	case <-conn.client.Done():
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-conn.exited:
			return conn.exitErr
		default:
			return errors.New("ACP connection closed")
		}
	}
}

func (s *acpAgentSession) setSessionResponse(id sdk.SessionId, modes *sdk.SessionModeState, options []sdk.SessionConfigOption) {
	s.protocolMu.Lock()
	defer s.protocolMu.Unlock()
	s.sessionID = id
	if modes != nil {
		s.modes = modes
	}
	if options != nil {
		s.config = options
	}
}

// StartTurn 与原生 HandleInput 分工不同，这里收到整批输入后启动一次 Prompt，异步报告终态。
func (s *acpAgentSession) StartTurn(ctx context.Context, id defs.TurnID, items []*defs.Item) error {
	blocks, err := s.promptBlocks(ctx, items)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.protocolMu.Lock()
	if s.closing || s.ctx.Err() != nil {
		s.protocolMu.Unlock()
		return errors.New("ACP session stopped")
	}
	if id == "" || s.promptTurnID != "" {
		s.protocolMu.Unlock()
		return errors.New("invalid or active ACP turn")
	}
	req, err := mergeParamOverwrite(sdk.PromptRequest{SessionId: s.sessionID, Prompt: blocks}, sdk.AgentMethodSessionPrompt, s.getParamOverwrites()...)
	if err != nil {
		s.protocolMu.Unlock()
		return err
	}
	s.promptTurnID = id
	s.itemIndex = 0
	s.toolCalls = nil
	s.measurement = nil
	written := make(chan struct{})
	s.connection.promptWritten.Store(&written)
	deliveryError := make(chan error, 1)
	s.tasks.Go(func() {
		res, err := s.connection.client.Prompt(s.ctx, req)
		s.protocolMu.Lock()
		defer s.protocolMu.Unlock()
		defer func() { s.promptTurnID = "" }()
		select {
		case <-written:
		default:
			deliveryError <- rpcError(err)
			return
		}
		if s.ctx.Err() != nil || s.closing {
			return
		}
		select {
		case <-s.connection.client.Done():
			return
		case <-s.connection.exited:
			return
		default:
		}
		end := &externalagent.ExternalTurnFinishedEvent{TurnID: id, Status: defs.TurnStatusCompleted}
		switch {
		case err != nil:
			end.Status, end.Err = defs.TurnStatusFailed, rpcError(err)
		case res.StopReason == sdk.StopReasonCancelled:
			end.Status = defs.TurnStatusInterrupted
		case res.StopReason != sdk.StopReasonEndTurn:
			telemetry.GetLogger(s.ctx).Info("ACP prompt stopped", "reason", res.StopReason)
		}
		status := defs.ReasoningCompleted
		if end.Status != defs.TurnStatusCompleted {
			status = defs.ReasoningInterrupted
		}
		s.flushChunk(status)
		if u := res.Usage; u != nil {
			end.Usage = &defs.TokenUsage{Input: u.InputTokens, Output: u.OutputTokens, CacheRead: ptrValue(u.CachedReadTokens), CacheWrite: ptrValue(u.CachedWriteTokens), Reasoning: ptrValue(u.ThoughtTokens)}
		}
		if s.measurement != nil {
			s.emit(&externalagent.ExternalAgentStreamEvent{TurnID: id, Event: &agent.AgentEvent{Type: agent.AgentEventContextMeasured, Measurement: s.measurement}})
		}
		s.emit(end)
	})
	s.protocolMu.Unlock()
	select {
	case <-written:
		return nil
	case err := <-deliveryError:
		return err
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

// SteerInput 对应原生轮中补充。ACP 没有该协议方法，由包装层选择中断或排队。
func (s *acpAgentSession) SteerInput(context.Context, defs.TurnID, []*defs.Item) error {
	return errors.New("ACP does not support steering")
}

// InterruptTurn 对应原生中断请求，只发送 Cancel，轮次结束由 Prompt 返回确认。
func (s *acpAgentSession) InterruptTurn(ctx context.Context) error {
	cancel, err := mergeParamOverwrite(sdk.CancelNotification{SessionId: s.sessionID}, sdk.AgentMethodSessionCancel, s.getParamOverwrites()...)
	if err != nil {
		return err
	}
	return s.connection.client.Cancel(ctx, cancel)
}

// SetOption 对应原生 SelectModel，也接收后端模式与配置项；通知可能早于 RPC 应答。
func (s *acpAgentSession) SetOption(ctx context.Context, sel defs.ExternalOptionSelection) error {
	s.protocolMu.Lock()
	id := s.sessionID
	options := externalOptions(s.modes, s.config)
	key := sel.ID
	switch sel.Kind {
	case "mode":
		if options.Mode != nil {
			sel.Kind, key = options.Mode.Kind, options.Mode.ID
		}
	case "model":
		if options.Model == nil {
			s.protocolMu.Unlock()
			return errors.New("ACP model option unavailable")
		}
		key = options.Model.ID
	}
	boolean := slices.ContainsFunc(s.config, func(o sdk.SessionConfigOption) bool { return o.Boolean != nil && string(o.Boolean.Id) == key })
	s.protocolMu.Unlock()
	if sel.Kind == "mode" {
		req, err := mergeParamOverwrite(sdk.SetSessionModeRequest{SessionId: id, ModeId: sdk.SessionModeId(sel.Value)}, sdk.AgentMethodSessionSetMode, s.getParamOverwrites()...)
		if err != nil {
			return err
		}
		_, err = s.connection.client.SetSessionMode(ctx, req)
		if err != nil {
			return rpcError(err)
		}
		s.protocolMu.Lock()
		defer s.protocolMu.Unlock()
		s.updateMode(req.ModeId)
		return nil
	}
	if key == "" || sel.Kind != "config" && sel.Kind != "model" {
		return errors.New("invalid ACP option")
	}
	var req sdk.SetSessionConfigOptionRequest
	if boolean {
		v, err := strconv.ParseBool(sel.Value)
		if err != nil {
			return err
		}
		req.Boolean = &sdk.SetSessionConfigOptionBoolean{Type: "boolean", SessionId: id, ConfigId: sdk.SessionConfigId(key), Value: v}
	} else {
		req.ValueId = &sdk.SetSessionConfigOptionValueId{SessionId: id, ConfigId: sdk.SessionConfigId(key), Value: sdk.SessionConfigValueId(sel.Value)}
	}
	req, err := mergeParamOverwrite(req, sdk.AgentMethodSessionSetConfigOption, s.getParamOverwrites()...)
	if err != nil {
		return err
	}
	res, err := s.connection.client.SetSessionConfigOption(ctx, req)
	if err != nil {
		return rpcError(err)
	}
	s.protocolMu.Lock()
	defer s.protocolMu.Unlock()
	s.config = res.ConfigOptions
	s.emit(&externalagent.ExternalOptionsUpdatedEvent{Options: externalOptions(s.modes, s.config)})
	return nil
}

func (s *acpAgentSession) updateMode(id sdk.SessionModeId) {
	if s.modes == nil {
		s.modes = &sdk.SessionModeState{}
	}
	if s.modes.CurrentModeId == id {
		return
	}
	s.modes.CurrentModeId = id
	s.emit(&externalagent.ExternalOptionsUpdatedEvent{Options: externalOptions(s.modes, s.config)})
}

func (s *acpAgentSession) emit(event externalagent.ExternalSessionEvent) {
	if s.events == nil {
		return
	}
	select {
	case s.events <- event:
	case <-s.ctx.Done():
	}
}

// SessionUpdate 将 SDK 的有序通知转成宿主事件。回放用户消息先合并协议分片。
func (s *acpAgentSession) SessionUpdate(ctx context.Context, n sdk.SessionNotification) error {
	s.protocolMu.Lock()
	defer s.protocolMu.Unlock()
	if s.closing || s.sessionID != "" && s.sessionID != n.SessionId {
		return nil
	}
	u := n.Update
	switch {
	case u.AvailableCommandsUpdate != nil && u.AvailableCommandsUpdate.SessionUpdate == "available_commands_update":
		var commands []defs.ExternalCommand
		for _, c := range u.AvailableCommandsUpdate.AvailableCommands {
			command := defs.ExternalCommand{Name: c.Name, Description: c.Description}
			if c.Input != nil && c.Input.Unstructured != nil {
				command.InputHint = c.Input.Unstructured.Hint
			}
			commands = append(commands, command)
		}
		s.commands = commands
		s.emit(&externalagent.ExternalCommandsUpdatedEvent{Commands: commands})
	case u.CurrentModeUpdate != nil && u.CurrentModeUpdate.SessionUpdate == "current_mode_update":
		s.updateMode(u.CurrentModeUpdate.CurrentModeId)
	case u.ConfigOptionUpdate != nil && u.ConfigOptionUpdate.SessionUpdate == "config_option_update":
		s.config = u.ConfigOptionUpdate.ConfigOptions
		s.emit(&externalagent.ExternalOptionsUpdatedEvent{Options: externalOptions(s.modes, s.config)})
	case u.UsageUpdate != nil && u.UsageUpdate.SessionUpdate == "usage_update":
		s.measurement = &agent_context.ContextMeasurement{IsUsageBased: true, RequestTokens: u.UsageUpdate.Used, Window: u.UsageUpdate.Size}
	case u.UserMessageChunk != nil && u.UserMessageChunk.SessionUpdate == "user_message_chunk" && s.replaying:
		s.handleChunk("user", ptrValue(u.UserMessageChunk.MessageId), u.UserMessageChunk.Content)
	case u.AgentMessageChunk != nil && u.AgentMessageChunk.SessionUpdate == "agent_message_chunk":
		s.handleChunk("assistant", ptrValue(u.AgentMessageChunk.MessageId), u.AgentMessageChunk.Content)
	case u.AgentThoughtChunk != nil && u.AgentThoughtChunk.SessionUpdate == "agent_thought_chunk":
		s.handleChunk("thought", ptrValue(u.AgentThoughtChunk.MessageId), u.AgentThoughtChunk.Content)
	case u.ToolCall != nil && u.ToolCall.SessionUpdate == "tool_call":
		c := u.ToolCall
		s.handleTool(c.ToolCallId, c.Title, c.RawInput, c.RawOutput, c.Content, c.Status)
	case u.ToolCallUpdate != nil && u.ToolCallUpdate.SessionUpdate == "tool_call_update":
		c := u.ToolCallUpdate
		s.handleTool(c.ToolCallId, ptrValue(c.Title), c.RawInput, c.RawOutput, c.Content, ptrValue(c.Status))
	default:
		telemetry.GetLogger(s.ctx).Debug("ignoring ACP session update", "session_id", n.SessionId)
	}
	return nil
}

// handleChunk 按角色和消息 ID 划分协议消息，实时文字沿用原生增量。
func (s *acpAgentSession) handleChunk(role, id string, content sdk.ContentBlock) {
	if !s.replaying && s.promptTurnID == "" {
		return
	}
	if s.chunk != nil && (s.chunk.role != role || s.chunk.id != id) {
		s.flushChunk(defs.ReasoningCompleted)
	}
	if s.chunk == nil {
		item := &defs.Item{ID: defs.NewItemID()}
		switch role {
		case "thought":
			item.Kind = defs.ItemReasoning
			item.Reasoning = &defs.Reasoning{}
		default:
			from := defs.IdentityAgent
			if role == "user" {
				from = defs.IdentityUser
			}
			item.Kind = defs.ItemMessage
			item.Message = &defs.Message{From: &defs.Address{Kind: from}}
			if role != "user" {
				item.Message.Content = []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{}}}
			}
		}
		s.chunk = &protocolChunk{role: role, id: id, index: s.itemIndex, item: item}
		s.itemIndex++
		if role != "user" {
			s.stream(agent.AgentEventItemStarted, s.chunk.index, item.Clone(), nil)
		}
	}
	chunk := s.chunk
	if b := content.Text; b != nil {
		delta := &agent.AgentItemDelta{Text: b.Text}
		if role == "thought" {
			delta = &agent.AgentItemDelta{ReasoningContent: []string{b.Text}}
		}
		if role == "user" {
			chunk.text.WriteString(b.Text)
			return
		}
		agent.ApplyItemDelta(chunk.item, delta)
		s.stream(agent.AgentEventItemDelta, chunk.index, nil, delta)
	} else if b := content.Image; b != nil && role != "thought" {
		if role == "user" {
			chunk.flushUserText()
		}
		chunk.item.Message.Content = append(chunk.item.Message.Content, &defs.Content{Kind: defs.ContentImage, Image: &defs.ImageContent{DataURL: "data:" + b.MimeType + ";base64," + b.Data}})
	}
}

func (s *acpAgentSession) flushChunk(status defs.ReasoningStatus) {
	c := s.chunk
	if c == nil {
		return
	}
	s.chunk = nil
	if c.role == "user" {
		c.flushUserText()
		if len(c.item.Message.Content) == 0 {
			return
		}
		parts := restoreUserContent(c.item.Message.Content)
		c.item.Message.Content = parts
		if s.events == nil {
			var words []string
			for _, part := range parts {
				if part.Text != nil {
					words = append(words, part.Text.Text)
				}
			}
			text := strings.Join(words, "\n")
			if !s.previewHasUser {
				s.preview.FirstUserMessage = text
				s.previewHasUser = true
			}
			s.preview.LastUserMessage = text
		}
	}
	kind := agent.AgentEventItemCompleted
	if c.item.Reasoning != nil {
		c.item.Reasoning.Status = status
		if status == defs.ReasoningInterrupted {
			kind = agent.AgentEventItemInterrupted
		}
	}
	s.stream(kind, c.index, c.item, nil)
}

func (s *acpAgentSession) stream(kind agent.AgentEventType, index int, item *defs.Item, delta *agent.AgentItemDelta) {
	s.emit(&externalagent.ExternalAgentStreamEvent{TurnID: s.promptTurnID, Replaying: s.replaying, Event: &agent.AgentEvent{Type: kind, ItemIndex: index, Item: item, Delta: delta}})
}

func (s *acpAgentSession) handleTool(id sdk.ToolCallId, title string, input, output any, content []sdk.ToolCallContent, status sdk.ToolCallStatus) {
	if id == "" || !s.replaying && s.promptTurnID == "" {
		return
	}
	s.flushChunk(defs.ReasoningCompleted)
	if s.toolCalls == nil {
		s.toolCalls = make(map[sdk.ToolCallId]*protocolToolCall)
	}
	call := s.toolCalls[id]
	if call == nil {
		call = &protocolToolCall{index: s.itemIndex, item: &defs.Item{ID: defs.NewItemID(), Kind: defs.ItemToolCall, ToolCall: &defs.ToolCall{CallID: string(id), Name: title}}}
		s.itemIndex++
		s.toolCalls[id] = call
		s.stream(agent.AgentEventItemStarted, call.index, call.item.Clone(), nil)
	}
	if title != "" {
		call.item.ToolCall.Name = title
	}
	if input != nil {
		raw, _ := json.Marshal(input)
		call.item.ToolCall.Arguments = string(raw)
	}
	if output != nil {
		call.rawOutput = output
	}
	if content != nil {
		call.content = content
	}
	if status != sdk.ToolCallStatusCompleted && status != sdk.ToolCallStatusFailed {
		return
	}
	call.item.ToolCall.Status = defs.ToolCallReady
	s.stream(agent.AgentEventItemCompleted, call.index, call.item, nil)
	delete(s.toolCalls, id)
	output = call.rawOutput
	if output == nil {
		output = call.content
	}
	raw, _ := json.Marshal(output)
	result := &defs.ToolResult{CallID: string(id), Content: raw}
	if status == sdk.ToolCallStatusFailed {
		result.Error = &defs.ToolError{Message: "ACP tool call failed"}
	}
	s.stream(agent.AgentEventItemCompleted, s.itemIndex, &defs.Item{ID: defs.NewItemID(), Kind: defs.ItemToolResult, ToolResult: result}, nil)
	s.itemIndex++
}

func (s *acpAgentSession) promptBlocks(ctx context.Context, items []*defs.Item) ([]sdk.ContentBlock, error) {
	var contents []*defs.Content
	for _, item := range items {
		if item == nil || item.Message == nil {
			return nil, errors.New("external prompt requires a message")
		}
		contents = append(contents, item.Message.Content...)
	}
	var blocks []sdk.ContentBlock
	for i, c := range contents {
		if err := c.Validate(); err != nil {
			return nil, err
		}
		if c.Kind == defs.ContentText {
			text := c.Text.Text
			if i != 0 {
				text = "\n\n" + text
			}
			blocks = append(blocks, sdk.TextBlock(text))
			continue
		}
		if c.Kind == defs.ContentFile {
			blocks = append(blocks, attachmentBlock(*c.File))
			continue
		}
		mime, data, ok := c.Image.GetDataURLParts()
		if !ok {
			return nil, errors.New("expected inline image data URL")
		}
		raw, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return nil, fmt.Errorf("decode image: %w", err)
		}
		if s.connection.capabilities.PromptCapabilities.Image && len(raw) <= 5<<20 {
			blocks = append(blocks, sdk.ImageBlock(data, mime))
		} else {
			file, err := s.saveImage(ctx, c.Image.Alt, mime, raw)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, attachmentBlock(file))
		}
	}
	if len(blocks) == 0 {
		return nil, errors.New("empty ACP prompt")
	}
	// SDK 使用 uint64 请求 ID。按最长 ID 预留完整信封和换行，正文、转义、附件和提示都计入预算。
	empty, err := json.Marshal(sdk.PromptRequest{SessionId: s.sessionID, Prompt: []sdk.ContentBlock{}})
	if err != nil {
		return nil, err
	}
	const envelope = `{"jsonrpc":"2.0","id":18446744073709551615,"method":"session/prompt","params":}` + "\n"
	size := len(envelope) + len(empty) + len(blocks) - 1 // 数组中相邻块的逗号
	blockBytes := make([]int, len(blocks))
	for i, block := range blocks {
		data, err := json.Marshal(block)
		if err != nil {
			return nil, err
		}
		blockBytes[i] = len(data)
		size += len(data)
	}
	// 只替换末尾仍内联的图片。块数和信封不变，替换前后的编码长度差即为整帧变化量。
	for i := len(blocks) - 1; size > maxACPFrameBytes && i >= 0; i-- {
		b := &blocks[i]
		if b.Image == nil {
			continue
		}
		raw, _ := base64.StdEncoding.DecodeString(b.Image.Data)
		file, err := s.saveImage(ctx, contents[i].Image.Alt, b.Image.MimeType, raw)
		if err != nil {
			return nil, err
		}
		*b = attachmentBlock(file)
		replaced, _ := json.Marshal(b)
		size += len(replaced) - blockBytes[i]
	}
	if size > maxACPFrameBytes {
		return nil, errors.New("ACP prompt exceeds 10 MiB frame limit")
	}
	return blocks, nil
}

// saveImage 在目标机的数据目录保存后端无法内联的图片；上传接口交来的 FileRef 直接透传。
func (s *acpAgentSession) saveImage(ctx context.Context, alt, mime string, raw []byte) (defs.FileRef, error) {
	ws := s.openReq.Workspace
	directory := s.openReq.AttachmentDirectory
	if !filepath.IsAbs(directory) {
		return defs.FileRef{}, errors.New("ACP image fallback requires an absolute conversation attachment directory")
	}
	name := filepath.Base(alt)
	if name == "." || name == ".." || name == string(filepath.Separator) || strings.ContainsAny(name, "\\\x00") {
		name = "image"
	}
	path := filepath.Join(directory, string(defs.NewItemID()), name)
	writer, err := ws.CreateFileWriter(ctx, path, 0600)
	if err != nil {
		return defs.FileRef{}, err
	}
	n, err := writer.Write(raw)
	if n != len(raw) && err == nil {
		err = io.ErrShortWrite
	}
	if err = errors.Join(err, writer.Close()); err != nil {
		return defs.FileRef{}, err
	}
	return defs.FileRef{Name: name, MIME: mime, Size: int64(n), Path: path}, nil
}
