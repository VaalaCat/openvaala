package appserver

import (
	"context"
	"errors"
	"iter"
	"slices"
	"strconv"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// Thread 历史读取：用控制 Client 直接读存储，不用打开对话。普通读取读到日志末尾；
// 用打开响应给的起点游标（codexNewestHistoryCursor）读时，读到 connectionThread 记下的历史终点。

const (
	// defaultHistoryPageLimit 是 thread/list、thread/turns/list、thread/items/list 省略 limit 时的每页条数，和 Codex 一致。
	defaultHistoryPageLimit = 25
	// maxHistoryPageLimit 是这三个请求的每页上限，和 Codex 一致。
	maxHistoryPageLimit = 100
	// itemsScanPageSize 是读 Session 日志时每次向 serve 要的条数。
	itemsScanPageSize = 100
)

// readThread 读取历史 Thread，按请求附带全部 Turn。
func (c *appServerConnection) readThread(ctx context.Context, params codexThreadReadParams) (any, error) {
	// 1. 定位对话和 Session，读取 Session 记录并构造 Thread。
	thread, err := c.getHistoryThread(ctx, params.ThreadID)
	if err != nil {
		return nil, err
	}
	sessions, err := c.readSessions(ctx, thread.conversation.ID)
	if err != nil {
		return nil, getCodexError(err, params.ThreadID)
	}
	result := codexThreadReadResult{Thread: newCodexHistoryThread(thread, sessions, c.pathMappings, c.isWindowsClient, c.getThreadStatus(ctx, thread.threadID), c.version)}
	// 2. 请求包含 Turn 时读取完整历史，不截到实时推送起点。
	if params.IncludeTurns {
		if result.Thread.Turns, err = c.readThreadTurns(ctx, thread.conversation.ID, thread.sessionID, sessions, nil); err != nil {
			return nil, getCodexError(err, params.ThreadID)
		}
	}
	return result, nil
}

// listThreads 按来源类型分页列出根 Thread 或子 Agent Thread。
func (c *appServerConnection) listThreads(ctx context.Context, params codexThreadListParams) (any, error) {
	// 1. 解析排序和页大小，按 sourceKinds 选择根 Thread 或子 Agent 列表。
	direction, err := getSortDirection(params.SortDirection, storage.DirectionDesc)
	if err != nil {
		return nil, err
	}
	limit := getHistoryPageLimit(params.Limit)
	if isSubAgentSourceKinds(params.SourceKinds) {
		return c.listSubAgentThreads(ctx, params, direction, limit)
	}
	return c.listRootThreads(ctx, params, direction, limit)
}

// listRootThreads 按更新时间和筛选条件分页读取根 Thread。
func (c *appServerConnection) listRootThreads(ctx context.Context, params codexThreadListParams, direction storage.Direction, limit int) (codexThreadListResult, error) {
	// 1. 解析游标，把能换算的客户端 cwd 换成 serve 目录；一个都换不了时不按目录筛选。
	cursor, err := getThreadListCursor(params.Cursor)
	if err != nil {
		return codexThreadListResult{}, err
	}
	serverCwds := make(codexCwdFilter, 0, len(params.Cwd))
	for _, clientCwd := range params.Cwd {
		if serverCwd, ok := c.getServerPath(clientCwd); ok {
			serverCwds = append(serverCwds, serverCwd)
		}
	}
	// 2. 填写归档、标题和目录条件，请求对话列表。
	request := &runtimeclient.ListConversationsRequest{
		Order: storage.Order{
			By:        storage.OrderByUpdatedAt,
			Direction: direction,
		},
		Cursor:      cursor,
		Limit:       limit,
		Archived:    params.Archived != nil && *params.Archived,
		Directories: serverCwds,
	}
	if params.SearchTerm != nil {
		request.SearchTerm = *params.SearchTerm
	}
	page, err := c.client.ListConversations(ctx, request)
	if err != nil {
		return codexThreadListResult{}, err
	}
	// 3. 本页存在未记录模型的根 Session 时，查询一次 Agent 默认模型。
	var defaultModel defs.ModelRef
	if slices.ContainsFunc(page.Conversations, func(entry dao.ConversationListEntry) bool {
		return entry.RootSession.Model.Ref == (defs.ModelRef{})
	}) {
		models, err := c.client.ListModels(ctx, c.agentName)
		if err != nil {
			return codexThreadListResult{}, err
		}
		defaultModel = models.AgentDefaultModel
	}
	// 4. 转换根 Thread，填入本连接查询到的运行状态和下一页游标。
	result := codexThreadListResult{Data: make([]codexThread, 0, len(page.Conversations))}
	if page.NextCursor.ID != "" {
		result.NextCursor = newThreadListCursor(page.NextCursor)
	}
	for _, entry := range page.Conversations {
		model := entry.RootSession.Model
		if model.Ref == (defs.ModelRef{}) {
			model.Ref = defaultModel
		}
		result.Data = append(result.Data, newCodexRootThread(entry.Conversation, model, c.pathMappings, c.isWindowsClient,
			c.getThreadStatus(ctx, string(entry.Conversation.ID)), c.version))
	}
	return result, nil
}

// listSubAgentThreads 分页列出指定 Thread 的直接子 Agent 或全部后代。
func (c *appServerConnection) listSubAgentThreads(ctx context.Context, params codexThreadListParams, direction storage.Direction, limit int) (codexThreadListResult, error) {
	// 1. 检查 parentThreadId 和 ancestorThreadId 互斥；均为空时返回空页。
	result := codexThreadListResult{Data: []codexThread{}}
	ancestorThreadID, isChildrenOnly := params.AncestorThreadID, false
	switch {
	case params.AncestorThreadID != nil && params.ParentThreadID != nil:
		return result, &codexError{
			errorCodeInvalidParams,
			"ancestorThreadId and parentThreadId are mutually exclusive",
		}
	case params.ParentThreadID != nil:
		ancestorThreadID, isChildrenOnly = params.ParentThreadID, true
	case ancestorThreadID == nil:
		return result, nil
	}
	// 2. 解析游标，定位祖先 Thread 并读取所属对话的 Session。
	cursor, err := getUUIDCursor(params.Cursor)
	if err != nil {
		return result, err
	}
	ancestor, err := c.getHistoryThread(ctx, *ancestorThreadID)
	if err != nil {
		return result, err
	}
	sessions, err := c.readSessions(ctx, ancestor.conversation.ID)
	if err != nil {
		return result, getCodexError(err, *ancestorThreadID)
	}
	// 3. 筛选直接子 Session 或全部后代，按创建顺序分页并转换 Thread。
	subAgents := getSubAgentSessionsUnder(getSubAgentSessions(ancestor.conversation, sessions), ancestor.sessionID, isChildrenOnly)
	page, nextCursor := getSubAgentSessionsPage(subAgents, cursor, direction, limit)
	for _, subAgent := range page {
		status := c.getThreadStatus(ctx, string(subAgent.record.ID))
		result.Data = append(result.Data, newCodexChildThread(ancestor.conversation, subAgent, c.pathMappings, c.isWindowsClient, status, c.version))
	}
	result.NextCursor = nextCursor
	return result, nil
}

// listThreadTurns 分页读取 Turn，按 itemsView 附带本页 Item。
func (c *appServerConnection) listThreadTurns(ctx context.Context, params codexThreadTurnsListParams) (any, error) {
	// 1. 解析 Turn 游标、排序和 Item 展示方式。
	cursor, err := getTurnsCursor(params.Cursor)
	if err != nil {
		return nil, err
	}
	direction, err := getSortDirection(params.SortDirection, storage.DirectionDesc)
	if err != nil {
		return nil, err
	}
	itemsView, err := getTurnsItemsView(params.ItemsView)
	if err != nil {
		return nil, err
	}
	// 2. 定位 Thread，按 Turn ID 读取一页记录。
	thread, err := c.getHistoryThread(ctx, params.ThreadID)
	if err != nil {
		return nil, err
	}
	page, err := c.client.ListTurns(ctx, &runtimeclient.ListTurnsRequest{
		ConversationID: thread.conversation.ID,
		SessionID:      thread.sessionID,
		Order:          storage.Order{Direction: direction},
		Cursor:         dao.RecordCursor[defs.TurnID]{ID: defs.TurnID(cursor)},
		Limit:          getHistoryPageLimit(params.Limit),
	})
	if err != nil {
		return nil, getCodexError(err, params.ThreadID)
	}
	// 3. 填写下一页游标；notLoaded 或空页直接返回 Turn 记录。
	result := codexThreadTurnsListResult{Data: make([]codexTurn, 0, len(page.Turns))}
	if page.NextCursor.ID != "" {
		nextCursor := string(page.NextCursor.ID)
		result.NextCursor = &nextCursor
	}
	if itemsView == codexTurnItemsNotLoaded || len(page.Turns) == 0 {
		for _, record := range page.Turns {
			result.Data = append(result.Data, newCodexRecordTurn(record))
		}
		return result, nil
	}
	// 4. 读取本页 Turn 的范围和 Session 记录，起点游标按历史终点截取，转换完整 Item。
	itemsByTurn, err := c.readItemsByTurn(ctx, thread.conversation.ID, thread.sessionID, page.Turns, c.getThreadHistoryEnd(ctx, thread.threadID, params.Cursor))
	if err != nil {
		return nil, getCodexError(err, params.ThreadID)
	}
	sessions, err := c.readSessions(ctx, thread.conversation.ID)
	if err != nil {
		return nil, getCodexError(err, params.ThreadID)
	}
	result.Data = newThreadTurns(thread.conversation.ID, thread.conversation.RootSessionID, thread.sessionID, getAgentPaths(sessions), page.Turns, itemsByTurn)
	return result, nil
}

// listThreadItems 分页读取 Thread Item，可限定 Turn 和历史终点。
func (c *appServerConnection) listThreadItems(ctx context.Context, params codexThreadItemsListParams) (any, error) {
	// 1. 解析流水号游标和排序，定位 Thread。
	cursor, err := getItemsCursor(params.Cursor)
	if err != nil {
		return nil, err
	}
	direction, err := getSortDirection(params.SortDirection, storage.DirectionAsc)
	if err != nil {
		return nil, err
	}
	thread, err := c.getHistoryThread(ctx, params.ThreadID)
	if err != nil {
		return nil, err
	}
	// 2. 设置页大小和子 Session 标记，指定 Turn 时读取记录及范围；Turn 不存在返回空页。
	query := itemsPageQuery{
		order:             direction,
		cursor:            cursor,
		limit:             getHistoryPageLimit(params.Limit),
		isSubAgentSession: thread.sessionID != thread.conversation.RootSessionID,
	}
	var records []defs.Turn
	if params.TurnID != nil {
		records, err = c.client.GetTurns(ctx, &runtimeclient.GetTurnsRequest{
			ConversationID: thread.conversation.ID,
			SessionID:      thread.sessionID,
			TurnIDs:        []defs.TurnID{defs.TurnID(*params.TurnID)},
		})
		if err != nil {
			return nil, getCodexError(err, params.ThreadID)
		}
		if len(records) == 0 {
			return codexThreadItemsListResult{Data: []codexThreadItemEntry{}}, nil
		}
		query.turn = &records[0]
	}
	// 3. 读取一页日志，工具调用与结果配对后才截页。
	page, err := c.readItemsPage(ctx, thread.conversation.ID, thread.sessionID, query, c.getThreadHistoryEnd(ctx, thread.threadID, params.Cursor))
	if err != nil {
		return nil, getCodexError(err, params.ThreadID)
	}
	// 4. 按日志正序翻译 Item，倒序请求在翻译后反转条目。
	items := page.items
	if direction == storage.DirectionDesc {
		items = slices.Clone(items)
		slices.Reverse(items)
	}
	entries, err := c.getItemEntries(ctx, thread, items, records)
	if err != nil {
		return nil, getCodexError(err, params.ThreadID)
	}
	if direction == storage.DirectionDesc {
		slices.Reverse(entries)
	}
	// 5. 返回条目和原始 Item 流水号游标。
	result := codexThreadItemsListResult{Data: entries}
	if page.nextCursor != 0 {
		nextCursor := strconv.FormatUint(uint64(page.nextCursor), 10)
		result.NextCursor = &nextCursor
	}
	return result, nil
}

// getItemEntries 按 Turn 开始时的 Executor 设置转换一页 Item。
func (c *appServerConnection) getItemEntries(ctx context.Context, thread historyThread, items []*defs.Item, records []defs.Turn) ([]codexThreadItemEntry, error) {
	// 1. 空输入返回空条目，否则读取 Session 并按连续 Turn 切分。
	entries := []codexThreadItemEntry{}
	if len(items) == 0 {
		return entries, nil
	}
	sessions, err := c.readSessions(ctx, thread.conversation.ID)
	if err != nil {
		return nil, err
	}
	segments := splitItemsByTurn(items)
	turnIDs := make([]defs.TurnID, 0, len(segments))
	for _, segment := range segments {
		if turnID := segment[0].TurnID; turnID != "" && !slices.Contains(turnIDs, turnID) {
			turnIDs = append(turnIDs, turnID)
		}
	}
	// 2. 未提供 Turn 记录时批量读取本页用到的 Turn。
	if records == nil {
		records, err = c.client.GetTurns(ctx, &runtimeclient.GetTurnsRequest{
			ConversationID: thread.conversation.ID,
			SessionID:      thread.sessionID,
			TurnIDs:        turnIDs,
		})
		if err != nil {
			return nil, err
		}
	}
	// 3. 按 Turn ID 建立记录索引，各段使用对应 Executor 和 Agent 路径转换。
	recordsByID := make(map[defs.TurnID]defs.Turn, len(records))
	for _, record := range records {
		recordsByID[record.ID] = record
	}
	agentPaths := getAgentPaths(sessions)
	for _, segment := range segments {
		translator := newTurnItemTranslator(thread.conversation.ID, thread.conversation.RootSessionID, thread.sessionID, agentPaths, recordsByID[segment[0].TurnID])
		entries = append(entries, translator.getThreadItems(segment)...)
	}
	return entries, nil
}

// getHistoryThread 定位 Thread 的对话记录和日志所属 Session。
func (c *appServerConnection) getHistoryThread(ctx context.Context, threadID string) (historyThread, error) {
	// 1. 查询已登记的子 Thread；未登记时先按对话 ID 读取根 Thread。
	notLoaded := &codexError{
		errorCodeInvalidRequest,
		"thread not loaded: " + threadID,
	}
	conversationID, isChildThread := c.childThreadConversations[defs.SessionID(threadID)]
	if !isChildThread {
		record, err := c.client.GetConversation(ctx, defs.ConversationID(threadID))
		if err == nil {
			return historyThread{
				threadID:     threadID,
				conversation: record,
				sessionID:    record.RootSessionID,
			}, nil
		}
		if !errors.Is(err, runtime.ErrConversationNotFound) {
			return historyThread{}, getCodexError(err, threadID)
		}
		// 2. 对话不存在时按 Session ID 查询，根 Session ID 不作为 Thread ID，找到子 Session 后登记所属对话。
		childSession, err := c.client.GetSessionByID(ctx, defs.SessionID(threadID))
		if errors.Is(err, runtime.ErrSessionNotFound) || (err == nil && childSession.ParentSessionID == "") {
			return historyThread{}, notLoaded
		}
		if err != nil {
			return historyThread{}, getCodexError(err, threadID)
		}
		conversationID = childSession.ConversationID
		c.childThreadConversations[childSession.ID] = conversationID
	}
	// 3. 读取子 Thread 所属对话，返回存储位置；对话不存在时返回 thread not loaded。
	record, err := c.client.GetConversation(ctx, conversationID)
	if errors.Is(err, runtime.ErrConversationNotFound) {
		return historyThread{}, notLoaded
	}
	if err != nil {
		return historyThread{}, getCodexError(err, threadID)
	}
	return historyThread{
		threadID:     threadID,
		conversation: record,
		sessionID:    defs.SessionID(threadID),
	}, nil
}

// getThreadStatus 查询本连接跟随的 Thread 或其已知子 Session 的状态。
func (c *appServerConnection) getThreadStatus(ctx context.Context, threadID string) codexThreadStatus {
	// 1. 优先查已加载 Thread，未跟随的子 Thread 转向已加载根 Thread 查询。
	thread, sessionID := c.loadedThreads[threadID], defs.SessionID(threadID)
	if thread != nil {
		sessionID = thread.sessionID
	} else if conversationID, isChildThread := c.childThreadConversations[sessionID]; isChildThread {
		thread = c.loadedThreads[string(conversationID)]
	}
	// 2. 查不到时返回 notLoaded，否则向 Thread 请求状态；Thread 收下请求后立即回复。
	if thread == nil {
		return newCodexThreadStatus(codexThreadStatusNotLoaded)
	}
	request := &threadStatusRequested{
		sessionID: sessionID,
		reply:     make(chan codexThreadStatus, 1),
	}
	if err := c.sendThreadRequest(ctx, thread, request); err != nil {
		return newCodexThreadStatus(codexThreadStatusNotLoaded)
	}
	return <-request.reply
}

// getThreadHistoryEnd 用起点游标读历史时，向已加载的 Thread 取历史终点；其他情况返回 nil。
func (c *appServerConnection) getThreadHistoryEnd(ctx context.Context, threadID string, cursor *string) *historyReadEnd {
	// 1. 游标不是 codexNewestHistoryCursor，或 Thread 没有加载时返回 nil。
	if cursor == nil || *cursor != codexNewestHistoryCursor {
		return nil
	}
	thread := c.loadedThreads[threadID]
	if thread == nil {
		return nil
	}
	// 2. 向 Thread 查询历史终点，Thread 收下请求后立即回复；只带流水号，不带 activeTurnID。
	request := &threadHistoryEndRequested{reply: make(chan storage.Ordinal, 1)}
	if err := c.sendThreadRequest(ctx, thread, request); err != nil {
		return nil
	}
	return &historyReadEnd{ordinal: <-request.reply}
}

// readThreadTurns 读取 Session 全部 Turn 和日志，转换为完整历史。
func (c *appServerConnection) readThreadTurns(ctx context.Context, conversationID defs.ConversationID, sessionID defs.SessionID, sessions []defs.Session, readEnd *historyReadEnd) ([]codexTurn, error) {
	// 1. 按 Turn ID 正序读取全部记录。
	records, err := c.readTurnRecords(ctx, conversationID, sessionID)
	if err != nil {
		return nil, err
	}
	// 2. 按各 Turn 范围读取 Item，非空 readEnd 截取终点并排除对应活动 Turn 的未完成工具调用。
	itemsByTurn, err := c.readItemsByTurn(ctx, conversationID, sessionID, records, readEnd)
	if err != nil {
		return nil, err
	}
	// 3. 从已提供的 Session 记录取得根 Session，使用每轮 Executor 设置转换。
	var rootSessionID defs.SessionID
	for _, sessionRecord := range sessions {
		if sessionRecord.ParentSessionID == "" {
			rootSessionID = sessionRecord.ID
			break
		}
	}
	return newThreadTurns(conversationID, rootSessionID, sessionID, getAgentPaths(sessions), records, itemsByTurn), nil
}

// readSessions 读取对话全部 Session 并补齐模型选择。
func (c *appServerConnection) readSessions(ctx context.Context, conversationID defs.ConversationID) ([]defs.Session, error) {
	// 1. 按游标读完全部 Session。
	var sessions []defs.Session
	request := &runtimeclient.ListSessionsRequest{ConversationID: conversationID}
	for {
		page, err := c.client.ListSessions(ctx, request)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, page.Sessions...)
		if page.NextCursor == (dao.RecordCursor[defs.SessionID]{}) {
			break
		}
		request.Cursor = page.NextCursor
	}
	// 2. 记录都已带模型时返回，否则查询 Agent 默认模型并填入零值选择。
	if !slices.ContainsFunc(sessions, func(record defs.Session) bool { return record.Model.Ref == (defs.ModelRef{}) }) {
		return sessions, nil
	}
	models, err := c.client.ListModels(ctx, c.agentName)
	if err != nil {
		return nil, err
	}
	for index := range sessions {
		if sessions[index].Model.Ref == (defs.ModelRef{}) {
			sessions[index].Model.Ref = models.AgentDefaultModel
		}
	}
	return sessions, nil
}

// readTurnRecords 按 Turn ID 正序分页读取 Session 全部 Turn 记录。
func (c *appServerConnection) readTurnRecords(ctx context.Context, conversationID defs.ConversationID, sessionID defs.SessionID) ([]defs.Turn, error) {
	// 1. 设置升序查询，沿下一页游标收集记录，读完后返回。
	var records []defs.Turn
	request := &runtimeclient.ListTurnsRequest{
		ConversationID: conversationID,
		SessionID:      sessionID,
		Order:          storage.Order{Direction: storage.DirectionAsc},
	}
	for {
		page, err := c.client.ListTurns(ctx, request)
		if err != nil {
			return nil, err
		}
		records = append(records, page.Turns...)
		if page.NextCursor == (dao.RecordCursor[defs.TurnID]{}) {
			return records, nil
		}
		request.Cursor = page.NextCursor
	}
}

// readItemsByTurn 读出各 Turn 范围内的 Item 并按 Turn 分组：先合并重叠和相邻的范围，再逐段读取。
func (c *appServerConnection) readItemsByTurn(ctx context.Context, conversationID defs.ConversationID, sessionID defs.SessionID, turns []defs.Turn, readEnd *historyReadEnd) (map[defs.TurnID][]*defs.Item, error) {
	// 1. 历史终点为 0 时返回空结果，否则算出每个 Turn 的读取范围：没结束的 Turn 读到 readEnd 或日志末尾，有 readEnd 时截到它。
	itemsByTurn := make(map[defs.TurnID][]*defs.Item)
	if readEnd != nil && readEnd.ordinal == 0 {
		return itemsByTurn, nil
	}
	var ranges []defs.TurnItemRange
	rangesByTurn := make(map[defs.TurnID]defs.TurnItemRange, len(turns))
	var tail storage.Ordinal
	isTailRead := false
	for _, turn := range turns {
		itemRange := turn.ItemsRange
		if itemRange.FirstOrdinal == 0 {
			continue
		}
		if itemRange.LastOrdinal == 0 {
			if readEnd != nil {
				itemRange.LastOrdinal = uint64(readEnd.ordinal)
			} else {
				if !isTailRead {
					page, err := c.client.ListItems(ctx, &runtimeclient.ListItemsRequest{
						ConversationID: conversationID,
						SessionID:      sessionID,
						Order:          storage.DirectionDesc,
						Limit:          1,
					})
					if err != nil {
						return nil, err
					}
					if len(page.Items) > 0 {
						tail = page.Items[0].Ordinal
					}
					isTailRead = true
				}
				itemRange.LastOrdinal = uint64(tail)
			}
		}
		if readEnd != nil {
			itemRange.LastOrdinal = min(itemRange.LastOrdinal, uint64(readEnd.ordinal))
		}
		if itemRange.LastOrdinal < itemRange.FirstOrdinal {
			continue
		}
		rangesByTurn[turn.ID] = itemRange
		ranges = append(ranges, itemRange)
	}
	// 2. 按范围起点排序，合并重叠和相邻范围。
	slices.SortFunc(ranges, func(a, b defs.TurnItemRange) int {
		if a.FirstOrdinal < b.FirstOrdinal {
			return -1
		}
		if a.FirstOrdinal > b.FirstOrdinal {
			return 1
		}
		return 0
	})
	var merged []defs.TurnItemRange
	for _, itemRange := range ranges {
		if last := len(merged) - 1; last >= 0 && itemRange.FirstOrdinal <= merged[last].LastOrdinal+1 {
			merged[last].LastOrdinal = max(merged[last].LastOrdinal, itemRange.LastOrdinal)
		} else {
			merged = append(merged, itemRange)
		}
	}
	// 3. 正序读取合并范围，只将目标 Turn 范围内的 Item 收入对应分组。
	for _, itemRange := range merged {
		for logged, err := range c.scanItemsRange(ctx, conversationID, sessionID, storage.DirectionAsc, 0, itemRange, nil) {
			if err != nil {
				return nil, err
			}
			target, found := rangesByTurn[logged.Item.TurnID]
			if found && uint64(logged.Ordinal) >= target.FirstOrdinal && uint64(logged.Ordinal) <= target.LastOrdinal {
				itemsByTurn[logged.Item.TurnID] = append(itemsByTurn[logged.Item.TurnID], logged.Item)
			}
		}
	}
	// 4. 指定历史终点时，去掉接入时在跑的 Turn 里还没有结果的工具调用。
	if readEnd != nil {
		for turnID, items := range itemsByTurn {
			itemsByTurn[turnID] = getItemsWithoutRunningToolCalls(items, readEnd.activeTurnID)
		}
	}
	return itemsByTurn, nil
}

// scanItemsRange 从 cursor 之后逐条读取 Turn 范围内的 Session 日志，有 readEnd 时读到它为止。
func (c *appServerConnection) scanItemsRange(ctx context.Context, conversationID defs.ConversationID, sessionID defs.SessionID, order storage.Direction, cursor storage.Ordinal, itemRange defs.TurnItemRange, readEnd *historyReadEnd) iter.Seq2[runtimeprotocol.LoggedItem, error] {
	// 1. 创建日志迭代器；范围为空且没带游标，或历史终点为 0 时直接结束。
	return func(yield func(runtimeprotocol.LoggedItem, error) bool) {
		first, last := storage.Ordinal(itemRange.FirstOrdinal), storage.Ordinal(itemRange.LastOrdinal)
		if (first == 0 && cursor == 0) || (readEnd != nil && readEnd.ordinal == 0) {
			return
		}
		// 2. 范围缺少终点时使用 readEnd 或日志末尾，再按 readEnd 截取。
		if first != 0 && last == 0 {
			if readEnd != nil {
				last = readEnd.ordinal
			} else {
				page, err := c.client.ListItems(ctx, &runtimeclient.ListItemsRequest{
					ConversationID: conversationID,
					SessionID:      sessionID,
					Order:          storage.DirectionDesc,
					Limit:          1,
				})
				if err != nil {
					yield(runtimeprotocol.LoggedItem{}, err)
					return
				}
				if len(page.Items) == 0 {
					return
				}
				last = page.Items[0].Ordinal
			}
		}
		if readEnd != nil {
			last = min(last, readEnd.ordinal)
		}
		// 3. 游标大于范围终点时查日志末尾，游标超出日志时返回 -32602；范围为空时结束。
		if cursor > last {
			tail, err := c.client.ListItems(ctx, &runtimeclient.ListItemsRequest{
				ConversationID: conversationID,
				SessionID:      sessionID,
				Order:          storage.DirectionDesc,
				Limit:          1,
			})
			if err != nil {
				yield(runtimeprotocol.LoggedItem{}, err)
				return
			}
			if len(tail.Items) == 0 || cursor > tail.Items[0].Ordinal {
				yield(runtimeprotocol.LoggedItem{}, &codexError{
					errorCodeInvalidParams,
					"invalid cursor: " + strconv.FormatUint(uint64(cursor), 10),
				})
				return
			}
		}
		if first == 0 || last < first {
			return
		}
		// 4. 定好读取起点（游标那条本身不读）；倒序读第一页时，先单独读出终点那条 Item，再从终点往前读。
		if order == storage.DirectionAsc {
			cursor = max(cursor, first-1)
		} else if cursor == 0 || cursor > last {
			page, err := c.client.ListItems(ctx, &runtimeclient.ListItemsRequest{
				ConversationID: conversationID,
				SessionID:      sessionID,
				Order:          storage.DirectionAsc,
				Cursor:         last - 1,
				Limit:          1,
			})
			if err != nil {
				yield(runtimeprotocol.LoggedItem{}, err)
				return
			}
			if len(page.Items) > 0 && page.Items[0].Ordinal == last {
				if !yield(page.Items[0], nil) {
					return
				}
			}
			cursor = last
		}
		// 5. 沿 NextCursor 分页交付范围内 Item，短页继续读，越过范围或调用方停止时结束。
		for (order == storage.DirectionAsc && cursor < last) || (order == storage.DirectionDesc && cursor > first) {
			remaining := last - cursor
			if order == storage.DirectionDesc {
				remaining = cursor - first
			}
			page, err := c.client.ListItems(ctx, &runtimeclient.ListItemsRequest{
				ConversationID: conversationID,
				SessionID:      sessionID,
				Order:          order,
				Cursor:         cursor,
				Limit:          int(min(storage.Ordinal(itemsScanPageSize), remaining)),
			})
			if err != nil {
				yield(runtimeprotocol.LoggedItem{}, err)
				return
			}
			for _, logged := range page.Items {
				if logged.Ordinal < first || logged.Ordinal > last {
					return
				}
				if !yield(logged, nil) {
					return
				}
			}
			if page.NextCursor == 0 {
				return
			}
			cursor = page.NextCursor
		}
	}
}

// readItemsPage 读取一页日志，工具调用与结果配对后截页。
func (c *appServerConnection) readItemsPage(ctx context.Context, conversationID defs.ConversationID, sessionID defs.SessionID, query itemsPageQuery, readEnd *historyReadEnd) (itemsPage, error) {
	// 1. 选择完整日志或指定 Turn 范围的迭代器。
	var page itemsPage
	var lastOrdinal storage.Ordinal
	isBeforeFork := query.turn == nil && query.isSubAgentSession && query.order == storage.DirectionAsc && query.cursor == 0
	items := c.scanItems(ctx, conversationID, sessionID, query.order, query.cursor, readEnd)
	if query.turn != nil {
		items = c.scanItemsRange(ctx, conversationID, sessionID, query.order, query.cursor, query.turn.ItemsRange, readEnd)
	}
	// 2. 子 Session 正序跳过 session_fork 及之前内容，倒序在分界结束；指定 Turn 时只保留这一轮的 Item。
	for logged, err := range items {
		if err != nil {
			return itemsPage{}, err
		}
		if isBeforeFork {
			isBeforeFork = logged.Item.Kind != defs.ItemSessionFork
			continue
		}
		if query.order == storage.DirectionDesc && logged.Item.Kind == defs.ItemSessionFork {
			break
		}
		if query.turn != nil && logged.Item.TurnID != query.turn.ID {
			continue
		}
		// 3. 达到条数且工具调用未被拆分时设置下一页游标，否则继续读到配对或日志结束。
		if len(page.items) >= query.limit && !isToolCallSplit(page.items, query.order) {
			page.nextCursor = lastOrdinal
			break
		}
		lastOrdinal = logged.Ordinal
		page.items = append(page.items, logged.Item)
	}
	return page, nil
}

// newThreadTurns 将 Turn 记录和分组 Item 转换为完整 Codex Turn。
func newThreadTurns(conversationID defs.ConversationID, rootSessionID, sessionID defs.SessionID, agentPaths map[defs.SessionID]string, records []defs.Turn, itemsByTurn map[defs.TurnID][]*defs.Item) []codexTurn {
	// 1. 按每轮记录的 Executor 设置转换 Item，标记 itemsView 为 full。
	turns := make([]codexTurn, 0, len(records))
	for _, record := range records {
		turn := newCodexRecordTurn(record)
		for _, entry := range newTurnItemTranslator(conversationID, rootSessionID, sessionID, agentPaths, record).getThreadItems(itemsByTurn[record.ID]) {
			turn.Items = append(turn.Items, entry.Item)
		}
		turn.ItemsView = codexTurnItemsFull
		turns = append(turns, turn)
	}
	return turns
}

// splitItemsByTurn 按 Turn ID 将日志 Item 切为连续段。
func splitItemsByTurn(items []*defs.Item) [][]*defs.Item {
	// 1. 同一 Turn 的相邻 Item 加入末段，Turn 变化时新建一段。
	var segments [][]*defs.Item
	for _, item := range items {
		if last := len(segments) - 1; last >= 0 && segments[last][0].TurnID == item.TurnID {
			segments[last] = append(segments[last], item)
			continue
		}
		segments = append(segments, []*defs.Item{item})
	}
	return segments
}

// getItemsWithoutRunningToolCalls 移除指定活动 Turn 中仍在执行的工具调用。
func getItemsWithoutRunningToolCalls(items []*defs.Item, activeTurnID defs.TurnID) []*defs.Item {
	// 1. 配对工具结果，复制后删掉指定 Turn 里 Ready 且没有结果的调用；activeTurnID 为空时全部保留。
	resultsByCall := getToolResultsByCall(items)
	return slices.DeleteFunc(slices.Clone(items), func(item *defs.Item) bool {
		return activeTurnID != "" && item.TurnID == activeTurnID && item.Kind == defs.ItemToolCall && item.ToolCall.Status == defs.ToolCallReady && resultsByCall[item] == nil
	})
}

// scanItems 按指定方向逐条读取游标之后的 Session 日志。
func (c *appServerConnection) scanItems(ctx context.Context, conversationID defs.ConversationID, sessionID defs.SessionID, order storage.Direction, cursor storage.Ordinal, readEnd *historyReadEnd) iter.Seq2[runtimeprotocol.LoggedItem, error] {
	// 1. 创建迭代器，历史终点为 0 时不发请求。
	return func(yield func(runtimeprotocol.LoggedItem, error) bool) {
		if readEnd != nil && readEnd.ordinal == 0 {
			return
		}
		// 2. 设置方向、游标和每页条数。
		request := &runtimeclient.ListItemsRequest{
			ConversationID: conversationID,
			SessionID:      sessionID,
			Order:          order,
			Cursor:         cursor,
			Limit:          itemsScanPageSize,
		}
		// 3. 分页读取，首次请求失败且确认游标超出日志末尾时返回 -32602。
		for {
			page, err := c.client.ListItems(ctx, request)
			if err != nil {
				if cursor > 0 && request.Cursor == cursor && c.isItemsCursorBeyondTail(ctx, conversationID, sessionID, cursor) {
					err = &codexError{
						errorCodeInvalidParams,
						"invalid cursor: " + strconv.FormatUint(uint64(cursor), 10),
					}
				}
				yield(runtimeprotocol.LoggedItem{}, err)
				return
			}
			// 4. 按 readEnd 过滤或结束，交付符合范围的 Item，沿 NextCursor 接续。
			for _, logged := range page.Items {
				if readEnd != nil && logged.Ordinal > readEnd.ordinal {
					if order == storage.DirectionAsc {
						return
					}
					continue
				}
				if !yield(logged, nil) {
					return
				}
			}
			if page.NextCursor == 0 {
				return
			}
			request.Cursor = page.NextCursor
		}
	}
}

// isItemsCursorBeyondTail 流水号游标是否超过日志末尾？
func (c *appServerConnection) isItemsCursorBeyondTail(ctx context.Context, conversationID defs.ConversationID, sessionID defs.SessionID, cursor storage.Ordinal) bool {
	// 1. 倒序读取最后一条，失败时返回 false；日志为空或游标超过最后流水号时返回 true。
	last, err := c.client.ListItems(ctx, &runtimeclient.ListItemsRequest{
		ConversationID: conversationID,
		SessionID:      sessionID,
		Order:          storage.DirectionDesc,
		Limit:          1,
	})
	if err != nil {
		return false
	}
	return len(last.Items) == 0 || cursor > last.Items[0].Ordinal
}
