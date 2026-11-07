package runtimeserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/terminal"
)

// maxItemsPageBytes 是一页日志累计 JSON 字节数的截取值，超过后保留当前 Item 再截断。
// 一页可超出该值一条 Item；单条 Item 超过客户端帧上限时，客户端读取失败并重连。
const maxItemsPageBytes = 4 << 20

// openvaalaMethods 是 OpenVaala 协议的方法表：方法名 → 解析参数 → 调用对应的 Runtime 公开方法。
var openvaalaMethods = map[string]methodHandler{
	runtimeprotocol.InitializeMethod:      newMethodHandler(handleInitialize),
	"conversation/create":                 newMethodHandler(handleConversationCreate),
	"conversation/open":                   newMethodHandler(handleConversationOpen),
	"conversation/read":                   newMethodHandler(handleConversationRead),
	"conversation/list":                   newMethodHandler(handleConversationList),
	"conversation/setTitle":               newMethodHandler(handleConversationSetTitle),
	"conversation/fork":                   newMethodHandler(handleConversationFork),
	"conversation/setArchived":            newMethodHandler(handleConversationSetArchived),
	"conversation/delete":                 newMethodHandler(handleConversationDelete),
	"conversation/setExecutorSettings":    newMethodHandler(handleConversationSetExecutorSettings),
	"conversation/submitInput":            newMethodHandler(handleConversationSubmitInput),
	"conversation/steerInput":             newMethodHandler(handleConversationSteerInput),
	"conversation/interruptTurnWithInput": newMethodHandler(handleConversationInterruptTurnWithInput),
	"conversation/withdrawInput":          newMethodHandler(handleConversationWithdrawInput),
	"conversation/appendContextItems":     newMethodHandler(handleConversationAppendContextItems),
	"conversation/interruptTurn":          newMethodHandler(handleConversationInterruptTurn),
	"conversation/compact":                newMethodHandler(handleConversationCompact),
	"session/get":                         newMethodHandler(handleSessionGet),
	"session/list":                        newMethodHandler(handleSessionList),
	"session/turns/list":                  newMethodHandler(handleSessionTurnsList),
	"session/turns/get":                   newMethodHandler(handleSessionTurnsGet),
	"session/items/list":                  newMethodHandler(handleSessionItemsList),
	"session/contextUsage":                newMethodHandler(handleSessionContextUsage),
	"session/details":                     newMethodHandler(handleSessionDetails),
	"session/setModel":                    newMethodHandler(handleSessionSetModel),
	"model/list":                          newMethodHandler(handleModelList),
	"agent/list":                          newMethodHandler(handleAgentList),
	"executor/list":                       newMethodHandler(handleExecutorList),
	"executor/resolveDirectory":           newMethodHandler(handleExecutorResolveDirectory),
	"executor/listDirectories":            newMethodHandler(handleExecutorListDirectories),
	"provider/list":                       newMethodHandler(handleProviderList),
	"provider/set":                        newMethodHandler(handleProviderSet),
	"provider/delete":                     newMethodHandler(handleProviderDelete),
	"provider/fetchModels":                newMethodHandler(handleProviderFetchModels),
	"config/setDefaultModel":              newMethodHandler(handleConfigSetDefaultModel),
	"config/reload":                       newMethodHandler(handleConfigReload),
	"catalog/providers/list":              newMethodHandler(handleCatalogProvidersList),
	"catalog/models/list":                 newMethodHandler(handleCatalogModelsList),
	"skill/list":                          newMethodHandler(handleSkillList),
	"hook/list":                           newMethodHandler(handleHookList),
	"mcpServer/list":                      newMethodHandler(handleMCPServerList),
	"conversation/setClientMCPServers":    newMethodHandler(handleConversationSetClientMCPServers),
	"conversation/subscribe":              newMethodHandler(handleConversationSubscribe),
	"conversation/unsubscribe":            newMethodHandler(handleConversationUnsubscribe),
	"conversation/running/subscribe":      newMethodHandler(handleRunningConversationsSubscribe),
	"conversation/running/unsubscribe":    newMethodHandler(handleRunningConversationsUnsubscribe),
	"session/subscribe":                   newMethodHandler(handleSessionSubscribe),
	"session/unsubscribe":                 newMethodHandler(handleSessionUnsubscribe),
	"terminal/open":                       newMethodHandler(handleTerminalOpen),
	"terminal/attach":                     newMethodHandler(handleTerminalAttach),
	"terminal/close":                      newMethodHandler(handleTerminalClose),
	"terminal/reset":                      newMethodHandler(handleTerminalReset),
	"terminal/subscribe":                  newMethodHandler(handleTerminalsSubscribe),
	"terminal/unsubscribe":                newMethodHandler(handleTerminalsUnsubscribe),
}

// handleInitialize 检查协议版本并返回服务端身份。
func handleInitialize(_ context.Context, connection *serverConnection, params *runtimeprotocol.InitializeParams) (*runtimeprotocol.InitializeResult, error) {
	// 1. 检查协议版本，不匹配时返回无效请求。
	if params.ProtocolVersion != runtimeprotocol.ProtocolVersion {
		return nil, newJSONRPCError(runtimeprotocol.ErrorCodeInvalidRequest, "unsupported protocol version")
	}
	// 2. 返回服务名、协议版本和 ServerID。
	return &runtimeprotocol.InitializeResult{
		ServerInfo:      runtimeprotocol.ServerInfo{Name: "openvaala"},
		ProtocolVersion: runtimeprotocol.ProtocolVersion,
		ServerID:        connection.serverID,
	}, nil
}

// handleConversationCreate 按请求创建并启动对话。
func handleConversationCreate(ctx context.Context, connection *serverConnection, params *runtimeprotocol.CreateConversationParams) (*runtimeprotocol.ConversationResult, error) {
	// 1. 提交 Agent、模型、Executor 设置和环境；设置无效时返回 -32602；WithoutExecutors 的对话不装配 Executor 和 Skill。
	record, err := connection.runtime.CreateConversation(ctx, &runtime.CreateConversationRequest{
		AgentName:        params.AgentName,
		Model:            params.Model,
		ExecutorSettings: params.ExecutorSettings,
		Environment:      params.Environment,
		Ephemeral:        params.Ephemeral,
		WithoutExecutors: params.WithoutExecutors,
	})
	if err != nil {
		return nil, err
	}
	// 2. 返回已启动的新对话记录。
	return &runtimeprotocol.ConversationResult{Conversation: record}, nil
}

// handleConversationOpen 打开已有对话并返回最新记录。
func handleConversationOpen(ctx context.Context, connection *serverConnection, params *runtimeprotocol.OpenConversationParams) (*runtimeprotocol.ConversationResult, error) {
	// 1. 交给 Runtime 打开对话；已运行时直接取记录，否则按存储设置和请求环境装配，已删除的 Executor 只记 Warn。
	record, err := connection.runtime.OpenConversation(ctx, &runtime.OpenConversationRequest{
		ConversationID: params.ConversationID,
		Environment:    params.Environment,
	})
	if err != nil {
		return nil, err
	}
	// 2. 返回本次打开或原已运行对话的最新记录。
	return &runtimeprotocol.ConversationResult{Conversation: record}, nil
}

// handleConversationRead 读取对话记录。
func handleConversationRead(ctx context.Context, connection *serverConnection, params *runtimeprotocol.ConversationParams) (*runtimeprotocol.ConversationResult, error) {
	// 1. 按对话 ID 读取记录，对话没打开也能读。
	record, err := connection.runtime.GetConversation(ctx, &runtime.GetConversationRequest{ConversationID: params.ConversationID})
	if err != nil {
		return nil, err
	}
	// 2. 返回读取到的对话记录。
	return &runtimeprotocol.ConversationResult{Conversation: record}, nil
}

// handleConversationList 分页列出指定归档状态的对话。
func handleConversationList(ctx context.Context, connection *serverConnection, params *runtimeprotocol.ListConversationsParams) (*runtimeprotocol.ListConversationsResult, error) {
	// 1. 补齐并校验排序。
	order, err := getRecordOrder(params.Order)
	if err != nil {
		return nil, err
	}
	// 2. 按请求筛选和游标读取一页记录，不要求对话已打开；未指定条数时使用默认值。
	page, err := connection.runtime.ListConversations(ctx, &dao.ListConversationsRequest{
		Directories: params.Directories,
		SearchTerm:  params.SearchTerm,
		Order:       order,
		Cursor:      params.Cursor,
		Limit:       getPageLimit(params.Limit),
		Archived:    params.Archived,
	})
	if err != nil {
		return nil, err
	}
	// 3. 返回列表和下一页游标，空列表编码为空数组。
	return &runtimeprotocol.ListConversationsResult{
		Conversations: getNonNilSlice(page.Items),
		NextCursor:    page.Next,
	}, nil
}

// handleConversationSetTitle 保存对话标题。
func handleConversationSetTitle(ctx context.Context, connection *serverConnection, params *runtimeprotocol.SetConversationTitleParams) (*runtimeprotocol.EmptyResult, error) {
	// 1. 提交对话 ID 和新标题，保存失败时返回错误。
	err := connection.runtime.SetConversationTitle(ctx, &runtime.SetConversationTitleRequest{
		ConversationID: params.ConversationID,
		Title:          params.Title,
	})
	if err != nil {
		return nil, err
	}
	// 2. 标题保存成功后返回空对象。
	return &runtimeprotocol.EmptyResult{}, nil
}

// handleConversationFork 复制对话到指定轮次，打开并返回分叉记录。
func handleConversationFork(ctx context.Context, connection *serverConnection, params *runtimeprotocol.ForkConversationParams) (*runtimeprotocol.ConversationResult, error) {
	// 1. 提交截取点、模型、Executor 设置、环境和是否临时；截取点之前没有轮次时新建未保存的对话，截取点或新增 Executor ID 无效时返回 -32602，来源已删除的 Executor 只记 Warn。
	record, err := connection.runtime.ForkConversation(ctx, &runtime.ForkConversationRequest{
		ConversationID:   params.ConversationID,
		BeforeTurnID:     params.BeforeTurnID,
		LastTurnID:       params.LastTurnID,
		Model:            params.Model,
		ExecutorSettings: params.ExecutorSettings,
		Environment:      params.Environment,
		Ephemeral:        params.Ephemeral,
	})
	if err != nil {
		return nil, err
	}
	// 2. 返回已打开的分叉对话记录。
	return &runtimeprotocol.ConversationResult{Conversation: record}, nil
}

// handleConversationSetArchived 更新对话归档状态。
func handleConversationSetArchived(ctx context.Context, connection *serverConnection, params *runtimeprotocol.SetConversationArchivedParams) (*runtimeprotocol.ConversationResult, error) {
	// 1. 提交归档状态，不打开或停止对话。
	record, err := connection.runtime.SetConversationArchived(ctx, &runtime.SetConversationArchivedRequest{
		ConversationID: params.ConversationID,
		Archived:       params.Archived,
	})
	if err != nil {
		return nil, err
	}
	// 2. 返回归档时间更新后的对话记录。
	return &runtimeprotocol.ConversationResult{Conversation: record}, nil
}

// handleConversationDelete 删除对话和全部 Session 历史。
func handleConversationDelete(ctx context.Context, connection *serverConnection, params *runtimeprotocol.ConversationParams) (*runtimeprotocol.EmptyResult, error) {
	// 1. 提交删除请求，Turn 正在执行时返回 -32001 且不删除；运行对话的订阅随实例关闭。
	if err := connection.runtime.DeleteConversation(ctx, &runtime.DeleteConversationRequest{ConversationID: params.ConversationID}); err != nil {
		return nil, err
	}
	// 2. 删除成功后返回空对象。
	return &runtimeprotocol.EmptyResult{}, nil
}

// handleConversationSetExecutorSettings 更新对话的完整 Executor 设置并返回记录。
func handleConversationSetExecutorSettings(ctx context.Context, connection *serverConnection, params *runtimeprotocol.SetExecutorSettingsParams) (*runtimeprotocol.ConversationResult, error) {
	// 1. 提交完整设置；运行对话重新装配并关闭订阅，未打开则按新设置打开，新增未知 ID 返回 -32602，正在执行 Turn 返回 -32001。
	record, err := connection.runtime.SetConversationExecutorSettings(ctx, &runtime.SetConversationExecutorSettingsRequest{
		ConversationID:   params.ConversationID,
		ExecutorSettings: params.ExecutorSettings,
	})
	if err != nil {
		return nil, err
	}
	// 2. 返回使用新 Executor 设置的对话记录。
	return &runtimeprotocol.ConversationResult{Conversation: record}, nil
}

// handleConversationSubmitInput 以用户身份向对话提交输入。
func handleConversationSubmitInput(ctx context.Context, connection *serverConnection, params *runtimeprotocol.SubmitInputParams) (*runtimeprotocol.SubmitInputResult, error) {
	// 1. 未指定输入模式时使用普通模式。
	mode := params.Mode
	if mode == "" {
		mode = session.SessionInputModeNormal
	}
	// 2. 发送方固定为用户，正文原样交给 Runtime，返回接收后的 ItemID。
	itemID, err := connection.runtime.SubmitInput(ctx, &runtime.SubmitInputRequest{
		ConversationID: params.ConversationID,
		InputID:        params.InputID,
		Mode:           mode,
		From:           &defs.Address{Kind: defs.IdentityUser},
		Content:        params.Content,
	})
	if err != nil {
		return nil, err
	}
	// 3. 封装输入 ItemID。
	return &runtimeprotocol.SubmitInputResult{ItemID: itemID}, nil
}

// handleConversationInterruptTurn 请求中断根 Session 的活动 Turn。
func handleConversationInterruptTurn(ctx context.Context, connection *serverConnection, params *runtimeprotocol.ConversationParams) (*runtimeprotocol.InterruptTurnResult, error) {
	// 1. 请求中断并取回 TurnID，不等待 Turn 结束。
	turnID, err := connection.runtime.InterruptTurn(ctx, &runtime.InterruptTurnRequest{ConversationID: params.ConversationID})
	if err != nil {
		return nil, err
	}
	// 2. 返回被中断的 TurnID，没有活动 Turn 时为空。
	return &runtimeprotocol.InterruptTurnResult{TurnID: turnID}, nil
}

// handleConversationCompact 请求根 Session 启动上下文压缩 Turn。
func handleConversationCompact(ctx context.Context, connection *serverConnection, params *runtimeprotocol.ConversationParams) (*runtimeprotocol.CompactContextResult, error) {
	// 1. 请求启动压缩；有活动 Turn 或排队输入时返回 -32001，进展经 Session 订阅推送。
	turnID, err := connection.runtime.CompactContext(ctx, &runtime.CompactContextRequest{ConversationID: params.ConversationID})
	if err != nil {
		return nil, err
	}
	// 2. 返回压缩 TurnID，不等待摘要生成。
	return &runtimeprotocol.CompactContextResult{TurnID: turnID}, nil
}

// handleSessionGet 按 SessionID 查询 Session 记录。
func handleSessionGet(ctx context.Context, connection *serverConnection, params *runtimeprotocol.GetSessionParams) (*runtimeprotocol.SessionResult, error) {
	// 1. 读取 Session 及所属对话，不加载对话；不存在时返回 -32003。
	record, err := connection.runtime.GetSessionByID(ctx, &runtime.GetSessionByIDRequest{SessionID: params.SessionID})
	if err != nil {
		return nil, err
	}
	// 2. 返回按 SessionID 找到的 Session 记录及其所属对话 ID。
	return &runtimeprotocol.SessionResult{Session: record}, nil
}

// handleSessionList 分页读取对话内的 Session。
func handleSessionList(ctx context.Context, connection *serverConnection, params *runtimeprotocol.ListSessionsParams) (*runtimeprotocol.ListSessionsResult, error) {
	// 1. 补齐并校验排序。
	order, err := getRecordOrder(params.Order)
	if err != nil {
		return nil, err
	}
	// 2. 按请求筛选和游标读取一页记录，未指定条数时使用默认值。
	page, err := connection.runtime.ListSessions(ctx, &dao.ListSessionsRequest{
		ConversationID: params.ConversationID,
		Order:          order,
		Cursor:         params.Cursor,
		Limit:          getPageLimit(params.Limit),
	})
	if err != nil {
		return nil, err
	}
	// 3. 返回列表和下一页游标，空列表编码为空数组。
	return &runtimeprotocol.ListSessionsResult{
		Sessions:   getNonNilSlice(page.Items),
		NextCursor: page.Next,
	}, nil
}

// handleSessionTurnsList 分页读取 Session 的 Turn。
func handleSessionTurnsList(ctx context.Context, connection *serverConnection, params *runtimeprotocol.ListTurnsParams) (*runtimeprotocol.ListTurnsResult, error) {
	// 1. 补齐并校验排序。
	order, err := getRecordOrder(params.Order)
	if err != nil {
		return nil, err
	}
	// 2. 按请求筛选和游标读取一页记录，未指定条数时使用默认值。
	page, err := connection.runtime.ListTurns(ctx, &runtime.ListTurnsRequest{
		ConversationID: params.ConversationID,
		SessionID:      params.SessionID,
		Order:          order,
		Cursor:         params.Cursor,
		Limit:          getPageLimit(params.Limit),
	})
	if err != nil {
		return nil, err
	}
	// 3. 返回列表和下一页游标，空列表编码为空数组。
	return &runtimeprotocol.ListTurnsResult{
		Turns:      getNonNilSlice(page.Items),
		NextCursor: page.Next,
	}, nil
}

// handleSessionTurnsGet 按请求顺序返回存在的 Turn 记录。
func handleSessionTurnsGet(ctx context.Context, connection *serverConnection, params *runtimeprotocol.GetTurnsParams) (*runtimeprotocol.GetTurnsResult, error) {
	// 1. 批量读取指定 Session 的 Turn，跳过不存在的 ID，不要求对话已打开。
	turns, err := connection.runtime.GetTurns(ctx, &runtime.GetTurnsRequest{
		ConversationID: params.ConversationID,
		SessionID:      params.SessionID,
		TurnIDs:        params.TurnIDs,
	})
	if err != nil {
		return nil, err
	}
	// 2. 按请求 ID 顺序返回已找到的 Turn，空结果编码为数组。
	return &runtimeprotocol.GetTurnsResult{Turns: getNonNilSlice(turns)}, nil
}

// handleSessionItemsList 分页读取 Session 日志并按编码字节数截取。
func handleSessionItemsList(ctx context.Context, connection *serverConnection, params *runtimeprotocol.ListItemsParams) (*runtimeprotocol.ListItemsResult, error) {
	// 1. 补齐并校验日志方向。
	order, err := getLogOrder(params.Order)
	if err != nil {
		return nil, err
	}
	// 2. 按游标和条数读取日志。
	page, err := connection.runtime.ListItems(ctx, &runtime.ListItemsRequest{
		ConversationID: params.ConversationID,
		SessionID:      params.SessionID,
		Order:          order,
		Cursor:         params.Cursor,
		Limit:          getPageLimit(params.Limit),
	})
	if err != nil {
		return nil, err
	}
	// 3. 累计各 Item 的 JSON 字节数，超过上限且后面还有 Item 时在当前 Item 之后截断，游标指向它；单条再大也至少返回 1 条，空日志返回空数组。
	items, next := toLoggedItems(page.Items), page.Next
	pageBytes := 0
	for index, item := range items {
		encoded, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		pageBytes += len(encoded)
		if pageBytes > maxItemsPageBytes && index < len(items)-1 {
			items, next = items[:index+1], item.Ordinal
			break
		}
	}
	// 4. 返回本页日志和下一页游标。
	return &runtimeprotocol.ListItemsResult{
		Items:      items,
		NextCursor: next,
	}, nil
}

// handleMCPServerList 返回对话中各 Executor 的 MCP 连接情况。
func handleMCPServerList(ctx context.Context, connection *serverConnection, params *runtimeprotocol.ConversationParams) (*runtimeprotocol.ListMCPServersResult, error) {
	// 1. 查询当前连接，不等待连接完成，未打开对话时返回错误。
	servers, err := connection.runtime.GetMCPServers(ctx, &runtime.GetConversationRequest{ConversationID: params.ConversationID})
	if err != nil {
		return nil, err
	}
	// 2. 将空工具列表和空 server 列表编码为空数组。
	for index := range servers {
		servers[index].Tools = getNonNilSlice(servers[index].Tools)
	}
	return &runtimeprotocol.ListMCPServersResult{Servers: getNonNilSlice(servers)}, nil
}

// handleConversationSetClientMCPServers 替换本连接为对话登记的客户端 MCP server。
func handleConversationSetClientMCPServers(ctx context.Context, connection *serverConnection, params *runtimeprotocol.SetClientMCPServersParams) (*runtimeprotocol.EmptyResult, error) {
	// 1. 为各 server_id 创建经本连接转发的传输；调用 Runtime 前先记下对话登记。
	servers := make([]conversation.ClientMCPServer, len(params.ServerIDs))
	for index, serverID := range params.ServerIDs {
		servers[index] = conversation.ClientMCPServer{
			ID:        serverID,
			Transport: connection.newClientMCPTransport(serverID),
		}
	}
	if len(servers) > 0 {
		connection.clientMCPConversations[params.ConversationID] = struct{}{}
	}
	// 2. 交给已打开对话替换登记，空列表表示移除全部；server 在客户端运行。
	err := connection.runtime.SetClientMCPServers(ctx, &runtime.SetClientMCPServersRequest{
		ConversationID: params.ConversationID,
		ClientID:       connection.clientID,
		Servers:        servers,
	})
	if err != nil {
		return nil, err
	}
	// 3. 全部移除成功时清除本连接的对话登记。
	if len(servers) == 0 {
		delete(connection.clientMCPConversations, params.ConversationID)
	}
	return &runtimeprotocol.EmptyResult{}, nil
}

// handleSessionContextUsage 返回 Session 上下文占用详情。
func handleSessionContextUsage(ctx context.Context, connection *serverConnection, params *runtimeprotocol.SessionParams) (*runtime.SessionContextUsage, error) {
	// 1. 按对话和 Session 查询上下文占用。
	usage, err := connection.runtime.GetSessionContextUsage(ctx, &runtime.GetSessionContextUsageRequest{
		ConversationID: params.ConversationID,
		SessionID:      params.SessionID,
	})
	if err != nil {
		return nil, err
	}
	// 2. 将空 Item 列表编码为空数组并返回。
	usage.Items = getNonNilSlice(usage.Items)
	return &usage, nil
}

// handleSessionDetails 返回 Session 已加载的配置。
func handleSessionDetails(ctx context.Context, connection *serverConnection, params *runtimeprotocol.SessionParams) (*conversation.SessionDetails, error) {
	// 1. 按对话和 Session 查询详情。
	details, err := connection.runtime.GetSessionDetails(ctx, &runtime.GetSessionDetailsRequest{
		ConversationID: params.ConversationID,
		SessionID:      params.SessionID,
	})
	if err != nil {
		return nil, err
	}
	// 2. 将 Executor、Skill、工具和模型推理强度中的空列表编码为空数组；模型已不在目录时保留空模型。
	details.Executors = getNonNilSlice(details.Executors)
	details.Skills = getNonNilSlice(details.Skills)
	details.Tools = getNonNilSlice(details.Tools)
	// 所选模型不在目录里时 Model 和 CompactModel 都为 nil。
	if details.Model != nil {
		details.Model.ReasoningEfforts = getNonNilSlice(details.Model.ReasoningEfforts)
		details.CompactModel.ReasoningEfforts = getNonNilSlice(details.CompactModel.ReasoningEfforts)
	}
	return &details, nil
}

// handleSessionSetModel 保存 Session 所选模型并返回记录。
func handleSessionSetModel(ctx context.Context, connection *serverConnection, params *runtimeprotocol.SetSessionModelParams) (*runtimeprotocol.SessionResult, error) {
	// 1. 提交 Session 模型选择，运行信息经对话订阅另行推送。
	record, err := connection.runtime.SetSessionModel(ctx, &runtime.SetSessionModelRequest{
		ConversationID: params.ConversationID,
		SessionID:      params.SessionID,
		Model:          params.Model,
	})
	if err != nil {
		return nil, err
	}
	// 2. 返回模型选择保存后的 Session 记录。
	return &runtimeprotocol.SessionResult{Session: record}, nil
}

// handleModelList 返回公开模型目录和 Agent 默认模型。
func handleModelList(ctx context.Context, connection *serverConnection, params *runtimeprotocol.ListModelsParams) (*runtime.ModelList, error) {
	// 1. 查询指定 Agent 对应的模型目录。
	list, err := connection.runtime.ListModels(ctx, &runtime.ListModelsRequest{AgentName: params.AgentName})
	if err != nil {
		return nil, err
	}
	// 2. 将空推理强度列表编码为空数组后返回。
	for _, model := range list.Models {
		model.ReasoningEfforts = getNonNilSlice(model.ReasoningEfforts)
	}
	return &list, nil
}

// handleAgentList 返回全部 Agent 的名称、说明和默认模型。
func handleAgentList(ctx context.Context, connection *serverConnection, _ *runtimeprotocol.EmptyParams) (*runtimeprotocol.ListAgentsResult, error) {
	// 1. 读取当前定义中的 Agent，不含系统指令。
	agents, err := connection.runtime.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	// 2. 返回 Agent 列表，未配置时编码为空数组。
	return &runtimeprotocol.ListAgentsResult{Agents: getNonNilSlice(agents)}, nil
}

// handleExecutorList 返回全部 Executor 的说明和默认目录。
func handleExecutorList(ctx context.Context, connection *serverConnection, _ *runtimeprotocol.EmptyParams) (*runtimeprotocol.ListExecutorsResult, error) {
	// 1. 读取当前定义中的 Executor，不访问 Executor。
	executors, err := connection.runtime.ListExecutors(ctx)
	if err != nil {
		return nil, err
	}
	// 2. 返回 Executor 列表，未配置时编码为空数组。
	return &runtimeprotocol.ListExecutorsResult{Executors: getNonNilSlice(executors)}, nil
}

// handleExecutorResolveDirectory 解析 Executor 上的目录并检查它是目录。
func handleExecutorResolveDirectory(ctx context.Context, connection *serverConnection, params *runtimeprotocol.ResolveDirectoryParams) (*runtimeprotocol.ResolveDirectoryResult, error) {
	// 1. 提交 Executor、用户输入目录和基准目录。
	directory, err := connection.runtime.ResolveWorkspaceDirectory(ctx, &runtime.ResolveWorkspaceDirectoryRequest{
		ExecutorID:    params.ExecutorID,
		Directory:     params.Directory,
		BaseDirectory: params.BaseDirectory,
	})
	if err != nil {
		return nil, err
	}
	// 2. 返回解析出的 Executor 工作目录。
	return &runtimeprotocol.ResolveDirectoryResult{WorkspaceDirectory: directory}, nil
}

// handleExecutorListDirectories 返回解析后的目录及其普通直接子目录。
func handleExecutorListDirectories(ctx context.Context, connection *serverConnection, params *runtimeprotocol.ListDirectoriesParams) (*conversation.WorkspaceDirectories, error) {
	// 1. 提交所选 Executor、目录和相对路径基准，读取目录候选。
	return connection.runtime.GetWorkspaceDirectories(ctx, &runtime.GetWorkspaceDirectoriesRequest{
		ExecutorID:    params.ExecutorID,
		Directory:     params.Directory,
		BaseDirectory: params.BaseDirectory,
	})
}

// handleProviderList 返回 Provider 配置、默认模型和模型默认配置。
func handleProviderList(ctx context.Context, connection *serverConnection, _ *runtimeprotocol.EmptyParams) (*runtimeprotocol.ListProvidersResult, error) {
	// 1. 读取完整 Provider 配置、默认模型和模型默认配置，配置包含 API key 原文。
	providers, defaultModel, defaults, err := connection.configuration.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	// 2. 将空 Provider 列表编码为空数组，非零默认模型转成 provider/model，未设置时保留空字符串。
	result := &runtimeprotocol.ListProvidersResult{
		Providers: getNonNilSlice(providers),
		Defaults:  defaults,
	}
	if defaultModel != (defs.ModelRef{}) {
		result.DefaultModel = defaultModel.String()
	}
	return result, nil
}

// handleProviderSet 写入 Provider 配置并替换 Runtime 定义。
func handleProviderSet(ctx context.Context, connection *serverConnection, params *runtimeprotocol.SetProviderParams) (*runtime.SetDefinitionsResult, error) {
	// 1. 解析可选默认模型，格式不是 provider/model 时返回无效参数且不写文件。
	var defaultModel defs.ModelRef
	if params.DefaultModel != "" {
		ref, err := defs.ParseModelRef(params.DefaultModel)
		if err != nil {
			return nil, newJSONRPCError(runtimeprotocol.ErrorCodeInvalidParams, err.Error())
		}
		defaultModel = ref
	}
	// 2. 写入 Provider 和可选默认模型，再加载配置；加载失败时配置服务恢复文件，Runtime 不变。
	definitions, err := connection.configuration.SetProvider(ctx, params.Provider, defaultModel)
	if err != nil {
		return nil, err
	}
	// 3. 替换 Runtime 定义；替换失败不回滚已写文件，下次启动仍使用新文件。
	result, err := connection.runtime.SetDefinitions(ctx, definitions)
	if err != nil {
		return nil, err
	}
	// 4. 返回替换结果，空忙碌对话列表编码为空数组。
	result.BusyConversationIDs = getNonNilSlice(result.BusyConversationIDs)
	return &result, nil
}

// handleProviderDelete 删除 Provider 配置并替换 Runtime 定义。
func handleProviderDelete(ctx context.Context, connection *serverConnection, params *runtimeprotocol.ProviderIDParams) (*runtime.SetDefinitionsResult, error) {
	// 1. 删除 Provider 文件并加载配置；加载失败时恢复文件并返回错误。
	definitions, err := connection.configuration.DeleteProvider(ctx, params.ProviderID)
	if err != nil {
		return nil, err
	}
	// 2. 替换 Runtime 定义并返回结果，空忙碌对话列表编码为空数组。
	result, err := connection.runtime.SetDefinitions(ctx, definitions)
	if err != nil {
		return nil, err
	}
	result.BusyConversationIDs = getNonNilSlice(result.BusyConversationIDs)
	return &result, nil
}

// handleProviderFetchModels 查询 Provider 上游模型并补齐目录字段。
func handleProviderFetchModels(ctx context.Context, connection *serverConnection, params *runtimeprotocol.ProviderParams) (*runtimeprotocol.ProviderModelsResult, error) {
	// 1. 请求上游 /models；失败时返回错误，客户端可手动填写。
	models, err := connection.modelCatalog.FetchProviderModels(ctx, params.Provider)
	if err != nil {
		return nil, err
	}
	// 2. 返回按模型 ID 索引的上游模型配置。
	return &runtimeprotocol.ProviderModelsResult{Models: models}, nil
}

// handleConfigSetDefaultModel 写入默认模型并替换 Runtime 定义。
func handleConfigSetDefaultModel(ctx context.Context, connection *serverConnection, params *runtimeprotocol.SetDefaultModelParams) (*runtime.SetDefinitionsResult, error) {
	// 1. 解析 provider/model，格式无效时返回参数错误且不写文件。
	ref, err := defs.ParseModelRef(params.Model)
	if err != nil {
		return nil, newJSONRPCError(runtimeprotocol.ErrorCodeInvalidParams, err.Error())
	}
	// 2. 修改默认模型并加载配置。
	definitions, err := connection.configuration.SetDefaultModel(ctx, ref)
	if err != nil {
		return nil, err
	}
	// 3. 替换 Runtime 定义并返回结果，空忙碌对话列表编码为空数组。
	result, err := connection.runtime.SetDefinitions(ctx, definitions)
	if err != nil {
		return nil, err
	}
	result.BusyConversationIDs = getNonNilSlice(result.BusyConversationIDs)
	return &result, nil
}

// handleConfigReload 重新加载配置并替换 Runtime 定义。
func handleConfigReload(ctx context.Context, connection *serverConnection, _ *runtimeprotocol.EmptyParams) (*runtime.SetDefinitionsResult, error) {
	// 1. 读取整份配置，不修改文件，加载失败时 Runtime 不变。
	definitions, err := connection.configuration.Reload(ctx)
	if err != nil {
		return nil, err
	}
	// 2. 替换 Runtime 定义并返回结果，空忙碌对话列表编码为空数组。
	result, err := connection.runtime.SetDefinitions(ctx, definitions)
	if err != nil {
		return nil, err
	}
	result.BusyConversationIDs = getNonNilSlice(result.BusyConversationIDs)
	return &result, nil
}

// handleCatalogProvidersList 返回模型目录中的可选服务。
func handleCatalogProvidersList(ctx context.Context, connection *serverConnection, _ *runtimeprotocol.EmptyParams) (*runtimeprotocol.ListCatalogProvidersResult, error) {
	// 1. 查询模型目录，失败时返回错误，客户端只提供自定义服务。
	providers, err := connection.modelCatalog.ListCatalogProviders(ctx)
	if err != nil {
		return nil, err
	}
	// 2. 返回目录 Provider 列表，没有服务时编码为空数组。
	return &runtimeprotocol.ListCatalogProvidersResult{Providers: getNonNilSlice(providers)}, nil
}

// handleCatalogModelsList 返回指定服务的模型目录。
func handleCatalogModelsList(ctx context.Context, connection *serverConnection, params *runtimeprotocol.ProviderIDParams) (*runtimeprotocol.ProviderModelsResult, error) {
	// 1. 按服务 ID 查询模型 ID 到配置的映射。
	models, err := connection.modelCatalog.ListCatalogModels(ctx, params.ProviderID)
	if err != nil {
		return nil, err
	}
	// 2. 返回该服务按模型 ID 索引的目录配置。
	return &runtimeprotocol.ProviderModelsResult{Models: models}, nil
}

// handleRunningConversationsSubscribe 登记全局运行订阅并在响应后启动转发。
func handleRunningConversationsSubscribe(ctx context.Context, connection *serverConnection, _ *runtimeprotocol.EmptyParams) (*runtime.RunningConversations, error) {
	// 1. 使用全局种类登记订阅，重复订阅返回 already subscribed。
	key := subscriptionKey{kind: subscriptionKindRunningConversations}
	subscriptionCtx, registration, err := connection.registerSubscription(ctx, key)
	if err != nil {
		return nil, err
	}
	// 2. 订阅 Runtime，失败时撤销连接登记。
	subscription, err := connection.runtime.SubscribeRunningConversations(subscriptionCtx)
	if err != nil {
		connection.removeSubscription(key, registration)
		return nil, err
	}
	// 3. 初始响应先入队，再转发完整集合。
	connection.startAfterResponse(func() {
		forwardGlobalSubscription(ctx, connection, key, registration, subscriptionCtx, subscription.Events,
			func(running runtime.RunningConversations) *runtimeprotocol.JSONRPCMessage {
				return &runtimeprotocol.JSONRPCMessage{Method: "conversation/runningChanged", Params: running}
			},
			func() *runtimeprotocol.JSONRPCMessage {
				return &runtimeprotocol.JSONRPCMessage{
					Method: "conversation/runningClosed",
					Params: &runtimeprotocol.RunningConversationsClosedParams{Reason: getErrorText(subscription.GetCloseReason())},
				}
			})
	})
	return &subscription.Initial, nil
}

// handleRunningConversationsUnsubscribe 取消全局运行订阅并等待旧转发结束。
func handleRunningConversationsUnsubscribe(ctx context.Context, connection *serverConnection, _ *runtimeprotocol.EmptyParams) (*runtimeprotocol.EmptyResult, error) {
	// 1. 取消并等待转发结束，未订阅时直接成功。
	connection.cancelSubscription(ctx, subscriptionKey{kind: subscriptionKindRunningConversations})
	return &runtimeprotocol.EmptyResult{}, nil
}

// handleConversationSubscribe 订阅对话标题与全部 Session 运行信息。
func handleConversationSubscribe(ctx context.Context, connection *serverConnection, params *runtimeprotocol.ConversationParams) (*runtimeprotocol.SubscribeConversationResult, error) {
	// 1. 为订阅目标创建连接内登记和订阅上下文。
	key := subscriptionKey{kind: subscriptionKindTarget, conversationID: params.ConversationID}
	subscriptionCtx, registration, err := connection.registerSubscription(ctx, key)
	if err != nil {
		return nil, err
	}
	// 2. 登记 Runtime 订阅，失败时移除连接内登记。
	subscription, err := connection.runtime.SubscribeConversation(subscriptionCtx, &runtime.SubscribeConversationRequest{
		ConversationID: params.ConversationID,
	})
	if err != nil {
		connection.removeSubscription(key, registration)
		return nil, err
	}
	// 3. 登记响应入队后启动的转发任务，返回订阅时的标题和 Session 运行信息；转发任务随后发送标题或状态通知。
	connection.startAfterResponse(func() {
		forwardOpenVaalaSubscription(ctx, connection, key, registration, subscription.Events, newConversationNotification, subscription.GetCloseReason)
	})
	return &runtimeprotocol.SubscribeConversationResult{
		Title:    subscription.Title,
		Sessions: getNonNilSlice(subscription.Sessions),
	}, nil
}

// handleConversationUnsubscribe 取消对话订阅，等待旧转发任务结束。
func handleConversationUnsubscribe(ctx context.Context, connection *serverConnection, params *runtimeprotocol.ConversationParams) (*runtimeprotocol.EmptyResult, error) {
	// 1. 取消指定订阅，等待 subscription/closed 入队；未订阅时直接成功。
	connection.cancelSubscription(ctx, subscriptionKey{kind: subscriptionKindTarget, conversationID: params.ConversationID})
	return &runtimeprotocol.EmptyResult{}, nil
}

// handleSessionSubscribe 订阅一个 Session 的内容。
func handleSessionSubscribe(ctx context.Context, connection *serverConnection, params *runtimeprotocol.SessionParams) (*runtimeprotocol.SubscribeSessionResult, error) {
	// 1. 为订阅目标创建连接内登记和订阅上下文。
	key := subscriptionKey{
		kind:           subscriptionKindTarget,
		conversationID: params.ConversationID,
		sessionID:      params.SessionID,
	}
	subscriptionCtx, registration, err := connection.registerSubscription(ctx, key)
	if err != nil {
		return nil, err
	}
	// 2. 登记 Runtime 订阅，失败时移除连接内登记。
	subscription, err := connection.runtime.SubscribeSession(subscriptionCtx, &runtime.SubscribeSessionRequest{
		ConversationID: params.ConversationID,
		SessionID:      params.SessionID,
	})
	if err != nil {
		connection.removeSubscription(key, registration)
		return nil, err
	}
	// 3. 登记响应入队后启动的转发任务，返回已保存日志位置和生成中 Item；转发任务随后发送内容通知。
	connection.startAfterResponse(func() {
		forwardOpenVaalaSubscription(ctx, connection, key, registration, subscription.Events, newOpenVaalaNotification, subscription.GetCloseReason)
	})
	return &runtimeprotocol.SubscribeSessionResult{
		LastSavedOrdinal: subscription.LastSavedOrdinal,
		StreamingItems:   toStreamingItems(subscription.StreamingItems),
		PendingInputs:    subscription.PendingInputs,
	}, nil
}

// handleSessionUnsubscribe 取消 Session 内容订阅，等待旧转发任务结束。
func handleSessionUnsubscribe(ctx context.Context, connection *serverConnection, params *runtimeprotocol.SessionParams) (*runtimeprotocol.EmptyResult, error) {
	// 1. 取消指定订阅，等待 subscription/closed 入队；未订阅时直接成功。
	connection.cancelSubscription(ctx, subscriptionKey{
		kind:           subscriptionKindTarget,
		conversationID: params.ConversationID,
		sessionID:      params.SessionID,
	})
	return &runtimeprotocol.EmptyResult{}, nil
}

// forwardGlobalSubscription 转发全局订阅的完整集合，事件流被 serve 关闭时通知客户端；主动退订和断线不发送关闭通知。
func forwardGlobalSubscription[T any](
	ctx context.Context,
	connection *serverConnection,
	key subscriptionKey,
	registration *connectionSubscription,
	subscriptionCtx context.Context,
	events <-chan T,
	newChangedNotification func(T) *runtimeprotocol.JSONRPCMessage,
	newClosedNotification func() *runtimeprotocol.JSONRPCMessage,
) {
	// 1. 登记转发完成通知，逐条转发集合变化，连接结束时停止发送。
	defer close(registration.forwarderDone)
	for event := range events {
		if !connection.send(ctx, newChangedNotification(event)) {
			connection.removeSubscription(key, registration)
			return
		}
	}
	// 2. 事件流关闭后移除登记；订阅方没有主动退订、连接也还在时，发送带原因的关闭通知。
	shouldNotifyClosed := subscriptionCtx.Err() == nil && ctx.Err() == nil
	connection.removeSubscription(key, registration)
	if !shouldNotifyClosed {
		return
	}
	connection.send(ctx, newClosedNotification())
}

// forwardOpenVaalaSubscription 转发订阅通知，事件流关闭后通知客户端订阅结束。
func forwardOpenVaalaSubscription[T any](
	ctx context.Context,
	connection *serverConnection,
	key subscriptionKey,
	registration *connectionSubscription,
	events <-chan T,
	newNotification func(subscriptionKey, T) *runtimeprotocol.JSONRPCMessage,
	getCloseReason func() error,
) {
	// 1. 登记转发完成通知，逐条转换事件，跳过不生成通知的事件，连接结束时停止发送。
	defer close(registration.forwarderDone)
	for event := range events {
		message := newNotification(key, event)
		if message == nil {
			continue
		}
		if !connection.send(ctx, message) {
			return
		}
	}
	// 2. 事件流关闭后移除登记，并发送带关闭原因的 subscription/closed。
	connection.removeSubscription(key, registration)
	connection.send(ctx, &runtimeprotocol.JSONRPCMessage{
		Method: "subscription/closed",
		Params: &runtimeprotocol.SubscriptionClosedParams{
			NotificationTarget: newNotificationTarget(key, key.sessionID),
			Reason:             getErrorText(getCloseReason()),
		},
	})
}

// newConversationNotification 将 Session 运行信息或已保存标题变化转换为通知。
func newConversationNotification(key subscriptionKey, event runtime.ConversationEvent) *runtimeprotocol.JSONRPCMessage {
	// 1. 按事件种类填入 session/statusUpdated 或 conversation/titleUpdated。
	switch event := event.(type) {
	case runtime.SessionRuntimeInfo:
		return &runtimeprotocol.JSONRPCMessage{
			Method: "session/statusUpdated",
			Params: &runtimeprotocol.SessionStatusUpdatedParams{
				NotificationTarget: newNotificationTarget(key, event.Session.ID),
				Info:               event,
			},
		}
	case runtime.ConversationTitleChanged:
		return &runtimeprotocol.JSONRPCMessage{
			Method: "conversation/titleUpdated",
			Params: &runtimeprotocol.ConversationTitleUpdatedParams{
				NotificationTarget: newNotificationTarget(key, ""),
				Title:              event.Title,
			},
		}
	}
	panic(fmt.Sprintf("unsupported Runtime conversation event %T", event))
}

// openvaalaItemMethods 是 Agent 事件类型对应的 OpenVaala 通知名。
var openvaalaItemMethods = map[agent.AgentEventType]string{
	agent.AgentEventItemStarted:     "item/started",
	agent.AgentEventItemDelta:       "item/delta",
	agent.AgentEventItemCompleted:   "item/completed",
	agent.AgentEventItemInterrupted: "item/interrupted",
	agent.AgentEventToolProgress:    "item/toolProgress",
}

// newOpenVaalaNotification 将对话内容事件转换为 Session 通知。
func newOpenVaalaNotification(key subscriptionKey, event conversation.ConversationEvent) *runtimeprotocol.JSONRPCMessage {
	// 1. Session 启动和停止事件生成对应通知，其余 Session 事件交给 Session 通知转换。
	switch event := event.(type) {
	case *conversation.ConversationSessionStarted:
		return &runtimeprotocol.JSONRPCMessage{
			Method: "session/started",
			Params: &runtimeprotocol.SessionStartedParams{
				NotificationTarget: newNotificationTarget(key, event.Session.ID),
				Session:            event.Session,
				LastSavedOrdinal:   event.LastSavedOrdinal,
			},
		}
	case *conversation.ConversationSessionStopped:
		return &runtimeprotocol.JSONRPCMessage{
			Method: "session/stopped",
			Params: &runtimeprotocol.SessionStoppedParams{
				NotificationTarget: newNotificationTarget(key, event.SessionID),
				Error:              getErrorText(event.Err),
			},
		}
	case *conversation.ConversationSessionEvent:
		return newOpenVaalaSessionNotification(newNotificationTarget(key, event.SessionID), event.Event)
	}
	panic(fmt.Sprintf("unsupported Conversation event %T", event))
}

// newOpenVaalaSessionNotification 将 Session 事件转换为 Turn、Item、保存或 Hook 通知。
func newOpenVaalaSessionNotification(target runtimeprotocol.NotificationTarget, event session.SessionEvent) *runtimeprotocol.JSONRPCMessage {
	// 1. 按 Session 事件填入通知参数；Agent 测量和模型调用结束事件不生成内容通知。
	switch event := event.(type) {
	case *session.SessionPendingInputsUpdated:
		return &runtimeprotocol.JSONRPCMessage{
			Method: "session/pendingInputsUpdated",
			Params: &runtimeprotocol.SessionPendingInputsUpdatedParams{
				NotificationTarget: target,
				PendingInputs:      event.PendingInputs,
			},
		}
	case *session.SessionTurnStarted:
		return &runtimeprotocol.JSONRPCMessage{
			Method: "turn/started",
			Params: &runtimeprotocol.TurnStartedParams{
				NotificationTarget: target,
				TurnID:             event.TurnID,
			},
		}
	case *session.SessionTurnFinished:
		return &runtimeprotocol.JSONRPCMessage{
			Method: "turn/finished",
			Params: &runtimeprotocol.TurnFinishedParams{
				NotificationTarget: target,
				TurnID:             event.TurnID,
				Status:             event.Status,
				InputItemIDs:       getNonNilSlice(event.InputItemIDs),
				FinalModelCallID:   event.FinalModelCallID,
				Error:              getErrorText(event.Err),
			},
		}
	case *session.SessionItemsSaved:
		return &runtimeprotocol.JSONRPCMessage{
			Method: "items/saved",
			Params: &runtimeprotocol.ItemsSavedParams{
				NotificationTarget: target,
				TurnID:             event.TurnID,
				Items:              toLoggedItems(event.Items),
			},
		}
	case *session.SessionHookRunEvent:
		return newHookRunNotification(target, event.TurnID, event.Run)
	case *session.SessionAgentEvent:
		if event.Event.Type == agent.AgentEventHookRun {
			return newHookRunNotification(target, event.TurnID, event.Event.HookRun)
		}
		method, found := openvaalaItemMethods[event.Event.Type]
		if !found {
			return nil
		}
		return &runtimeprotocol.JSONRPCMessage{
			Method: method,
			Params: &runtimeprotocol.ItemEventParams{
				NotificationTarget: target,
				TurnID:             event.TurnID,
				ItemIndex:          event.Event.ItemIndex,
				Item:               event.Event.Item,
				Delta:              event.Event.Delta,
				ToolProgress:       event.Event.ToolProgress,
			},
		}
	}
	panic(fmt.Sprintf("unsupported Session event %T", event))
}

// newHookRunNotification 将 Hook 运行记录转换为开始或完成通知。
func newHookRunNotification(target runtimeprotocol.NotificationTarget, turnID defs.TurnID, run *defs.HookRun) *runtimeprotocol.JSONRPCMessage {
	// 1. running 状态使用 hook/started，其余状态使用 hook/completed。
	method := "hook/completed"
	if run.Status == defs.HookRunRunning {
		method = "hook/started"
	}
	// 2. 填入通知目标、TurnID 和完整 HookRun。
	return &runtimeprotocol.JSONRPCMessage{
		Method: method,
		Params: &runtimeprotocol.HookRunParams{
			NotificationTarget: target,
			TurnID:             turnID,
			HookRun:            run,
		},
	}
}

// handleSkillList 返回对话或草稿 Agent 的可用 Skill。
func handleSkillList(ctx context.Context, connection *serverConnection, params *runtimeprotocol.ListSkillsParams) ([]skill.SkillInfo, error) {
	// 1. 检查对话 ID 和 Agent 名称至少提供一个。
	if params.ConversationID == "" && params.AgentName == "" {
		return nil, newJSONRPCError(runtimeprotocol.ErrorCodeInvalidParams, "conversation_id or agent_name is required")
	}
	// 2. 提交对话 ID、Agent 名称和草稿 Executor 设置。
	return connection.runtime.GetSkills(ctx, &runtime.GetSkillsRequest{
		ConversationID:   params.ConversationID,
		AgentName:        params.AgentName,
		ExecutorSettings: params.ExecutorSettings,
	})
}

// handleHookList 按配置顺序返回 Hook 目录。
func handleHookList(ctx context.Context, connection *serverConnection, _ *runtimeprotocol.EmptyParams) (*runtimeprotocol.ListHooksResult, error) {
	// 1. 读取当前定义的 Hook 信息。
	hooks, err := connection.runtime.GetHooks(ctx)
	if err != nil {
		return nil, err
	}
	// 2. 按配置顺序返回 Hook 列表，未配置时编码为空数组。
	return &runtimeprotocol.ListHooksResult{Hooks: getNonNilSlice(hooks)}, nil
}

// handleConversationSteerInput 将用户排队操作交给根 Session。
func handleConversationSteerInput(ctx context.Context, connection *serverConnection, params *runtimeprotocol.SteerInputParams) (*runtimeprotocol.EmptyResult, error) {
	// 1. 检查必填身份参数。
	if params.ConversationID == "" || params.ItemID == "" || params.TurnID == "" {
		return nil, newJSONRPCError(runtimeprotocol.ErrorCodeInvalidParams, "conversation_id, item_id and turn_id are required")
	}
	// 2. 提交原子队列操作并返回结果。
	if err := connection.runtime.SteerInput(ctx, &runtime.SteerInputRequest{
		ConversationID: params.ConversationID,
		ItemID:         params.ItemID,
		TurnID:         params.TurnID,
	}); err != nil {
		return nil, err
	}
	return &runtimeprotocol.EmptyResult{}, nil
}

// handleConversationInterruptTurnWithInput 将用户排队操作交给根 Session。
func handleConversationInterruptTurnWithInput(ctx context.Context, connection *serverConnection, params *runtimeprotocol.InterruptTurnWithInputParams) (*runtimeprotocol.EmptyResult, error) {
	// 1. 检查必填身份参数。
	if params.ConversationID == "" || params.ItemID == "" || params.TurnID == "" {
		return nil, newJSONRPCError(runtimeprotocol.ErrorCodeInvalidParams, "conversation_id, item_id and turn_id are required")
	}
	// 2. 提交原子队列操作并返回结果。
	if err := connection.runtime.InterruptTurnWithInput(ctx, &runtime.InterruptTurnWithInputRequest{
		ConversationID: params.ConversationID,
		ItemID:         params.ItemID,
		TurnID:         params.TurnID,
	}); err != nil {
		return nil, err
	}
	return &runtimeprotocol.EmptyResult{}, nil
}

// handleConversationWithdrawInput 取走用户输入组，按请求返回用户消息内容。
func handleConversationWithdrawInput(ctx context.Context, connection *serverConnection, params *runtimeprotocol.WithdrawInputParams) (*runtimeprotocol.WithdrawInputResult, error) {
	// 1. 检查必填身份参数并原子取组。
	if params.ConversationID == "" || params.ItemID == "" {
		return nil, newJSONRPCError(runtimeprotocol.ErrorCodeInvalidParams, "conversation_id and item_id are required")
	}
	items, err := connection.runtime.WithdrawInput(ctx, &runtime.WithdrawInputRequest{
		ConversationID: params.ConversationID,
		ItemID:         params.ItemID,
	})
	if err != nil {
		return nil, err
	}
	// 2. 取回只返回用户消息的完整内容，撤回返回空对象。
	result := &runtimeprotocol.WithdrawInputResult{}
	if params.ReturnContent {
		result.Content = items[0].Message.Content
	}
	return result, nil
}

// handleConversationAppendContextItems 把客户端注入的 Item 交给根 Session，写在下一轮输入之前。
func handleConversationAppendContextItems(ctx context.Context, connection *serverConnection, params *runtimeprotocol.AppendContextItemsParams) (*runtimeprotocol.EmptyResult, error) {
	// 1. 检查必填身份参数。
	if params.ConversationID == "" {
		return nil, newJSONRPCError(runtimeprotocol.ErrorCodeInvalidParams, "conversation_id is required")
	}
	// 2. 交给已打开对话暂存，Item 无效时整批返回错误。
	if err := connection.runtime.AppendContextItems(ctx, &runtime.AppendContextItemsRequest{
		ConversationID: params.ConversationID,
		Items:          params.Items,
	}); err != nil {
		return nil, err
	}
	return &runtimeprotocol.EmptyResult{}, nil
}

// handleTerminalOpen 新开一个终端并让本连接接入：在所选 Executor 上新建独立工作区并启动 TTY shell。
func handleTerminalOpen(ctx context.Context, connection *serverConnection, params *runtimeprotocol.TerminalOpenParams) (*runtimeprotocol.TerminalOpenResult, error) {
	// 1. 交给 Runtime 新建工作区并启动 shell；virtual Executor 和无效目录在这里返回错误。
	opened, err := connection.runtime.OpenTerminal(ctx, &runtime.OpenTerminalRequest{
		ExecutorID:     params.ExecutorID,
		Directory:      params.Directory,
		Environment:    params.Environment,
		Rows:           params.Rows,
		Cols:           params.Cols,
		ConversationID: params.ConversationID,
	})
	if err != nil {
		return nil, err
	}
	// 2. 读第一页并登记接入，响应入队之后才启动转发，客户端先拿到 terminal_id 再收到输出。
	firstOutput, err := opened.Terminal.ReadOutput(ctx, 0, 0)
	if err != nil {
		return nil, err
	}
	connection.attachTerminal(opened.Terminal, firstOutput)
	return &runtimeprotocol.TerminalOpenResult{
		TerminalID: firstOutput.Info.ID,
		ExecutorID: opened.ExecutorID,
		Directory:  opened.Directory,
	}, nil
}

// handleTerminalAttach 把本连接接入已有终端，从 cursor 起补发输出。
func handleTerminalAttach(ctx context.Context, connection *serverConnection, params *runtimeprotocol.TerminalAttachParams) (*runtimeprotocol.TerminalAttachResult, error) {
	// 1. 查找终端，找不到时返回错误，不新开 shell。
	runtimeTerminal, err := connection.runtime.GetTerminal(ctx, params.TerminalID)
	if err != nil {
		return nil, err
	}
	// 2. 取消并等待本连接已有的同一终端接入，新的从 cursor 重发。
	connection.cancelTerminalAttachment(ctx, params.TerminalID)
	// 3. 读第一页，游标越界时返回错误。
	firstOutput, err := runtimeTerminal.ReadOutput(ctx, params.Cursor, 0)
	if err != nil {
		return nil, err
	}
	// 4. 登记接入，响应入队后启动转发；响应里带 PTY 当前行列。
	connection.attachTerminal(runtimeTerminal, firstOutput)
	return &runtimeprotocol.TerminalAttachResult{
		ExecutorID: firstOutput.Info.EnvironmentID,
		Directory:  firstOutput.Info.Directory,
		Rows:       firstOutput.Info.Rows,
		Cols:       firstOutput.Info.Cols,
	}, nil
}

// handleTerminalClose 结束一个终端，终端可由任何连接打开；终端已经结束或不存在时成功，terminal/exited 由各接入自己发出。
func handleTerminalClose(ctx context.Context, connection *serverConnection, params *runtimeprotocol.TerminalParams) (*runtimeprotocol.EmptyResult, error) {
	// 1. 查找终端，不存在说明已经结束。
	runtimeTerminal, err := connection.runtime.GetTerminal(ctx, params.TerminalID)
	if errors.Is(err, terminal.ErrTerminalNotFound) {
		return &runtimeprotocol.EmptyResult{}, nil
	}
	if err != nil {
		return nil, err
	}
	// 2. 限时结束进程组。
	stopCtx, cancel := context.WithTimeout(ctx, terminalCloseTimeout)
	defer cancel()
	if _, err := runtimeTerminal.Stop(stopCtx); err != nil {
		return nil, err
	}
	return &runtimeprotocol.EmptyResult{}, nil
}

// handleTerminalReset 丢掉终端保留的输出并让全部接入的客户端清屏。
func handleTerminalReset(ctx context.Context, connection *serverConnection, params *runtimeprotocol.TerminalParams) (*runtimeprotocol.EmptyResult, error) {
	// 1. 查找终端，找不到时返回错误。
	runtimeTerminal, err := connection.runtime.GetTerminal(ctx, params.TerminalID)
	if err != nil {
		return nil, err
	}
	// 2. 清除保留的输出，已结束的终端返回错误。
	if err := runtimeTerminal.ClearOutput(ctx); err != nil {
		return nil, err
	}
	return &runtimeprotocol.EmptyResult{}, nil
}

// handleTerminalsSubscribe 登记运行中终端列表的订阅并在响应后启动转发。
func handleTerminalsSubscribe(ctx context.Context, connection *serverConnection, _ *runtimeprotocol.EmptyParams) (*runtimeprotocol.TerminalListResult, error) {
	// 1. 使用终端列表种类登记订阅，重复订阅返回 already subscribed。
	key := subscriptionKey{kind: subscriptionKindTerminals}
	subscriptionCtx, registration, err := connection.registerSubscription(ctx, key)
	if err != nil {
		return nil, err
	}
	// 2. 订阅 Runtime，失败时撤销连接登记。
	subscription, err := connection.runtime.SubscribeTerminals(subscriptionCtx)
	if err != nil {
		connection.removeSubscription(key, registration)
		return nil, err
	}
	// 3. 初始响应先入队，再转发完整列表。
	connection.startAfterResponse(func() {
		forwardGlobalSubscription(ctx, connection, key, registration, subscriptionCtx, subscription.Events,
			func(running []runtime.RunningTerminal) *runtimeprotocol.JSONRPCMessage {
				return &runtimeprotocol.JSONRPCMessage{
					Method: "terminal/listChanged",
					Params: &runtimeprotocol.TerminalListResult{Terminals: getTerminalItems(running)},
				}
			},
			func() *runtimeprotocol.JSONRPCMessage {
				return &runtimeprotocol.JSONRPCMessage{
					Method: "terminal/listClosed",
					Params: &runtimeprotocol.TerminalsClosedParams{Reason: getErrorText(subscription.GetCloseReason())},
				}
			})
	})
	return &runtimeprotocol.TerminalListResult{Terminals: getTerminalItems(subscription.Initial)}, nil
}

// handleTerminalsUnsubscribe 取消运行中终端列表的订阅并等待旧转发结束。
func handleTerminalsUnsubscribe(ctx context.Context, connection *serverConnection, _ *runtimeprotocol.EmptyParams) (*runtimeprotocol.EmptyResult, error) {
	// 1. 取消并等待转发结束，未订阅时直接成功。
	connection.cancelSubscription(ctx, subscriptionKey{kind: subscriptionKindTerminals})
	return &runtimeprotocol.EmptyResult{}, nil
}
