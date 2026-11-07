package dao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
)

// itemDAOImpl 读写各 Session 的 Item 日志，读取时给用户分叉的根 Session 接上来源历史。
type itemDAOImpl struct {
	logs storage.LogStorage
	// historyBases 缓存各 Session 日志第 1 行的接续标记；值为 nil 表示没有标记。日志为空时不缓存。
	historyBases syncmap.SyncMap[sessionStreamKey, *sessionHistoryBase]
}

// sessionStreamKey 定位一个 Session 的 Item 日志。
type sessionStreamKey struct {
	conversationID defs.ConversationID
	sessionID      defs.SessionID
}

// sessionHistoryBase 是接续标记里记的来源位置。接续标记是用户分叉写在新根 Session 日志第 1 行、带来源对话的
// SessionFork Item；读历史时本 Session 前面接上来源 Session 序号小于 SourceEndOrdinal 的 Item。
type sessionHistoryBase struct {
	SourceConversationID defs.ConversationID
	SourceSessionID      defs.SessionID
	// SourceEndOrdinal 是第一条复制到本 Session 的来源 Item 的序号，本 Session 第 2 行的对外序号就是它。
	SourceEndOrdinal storage.Ordinal
}

// ListItemsRequest 是按对外序号分页读取一个 Session Item 日志的条件。
type ListItemsRequest struct {
	ConversationID defs.ConversationID
	SessionID      defs.SessionID
	Order          storage.Direction
	// Cursor 是上一页返回的 Next；0 时从这个方向的起点读。
	Cursor storage.Ordinal
	Limit  int
}

// LoggedItem 是写入或读出的一条 Item 和它的对外序号。
type LoggedItem struct {
	// Ordinal 是对外序号；用户分叉的根 Session 已经接在来源历史的序号之后。
	Ordinal storage.Ordinal
	Item    *defs.Item
}

// AppendItems 检查并一次追加一批 Item，返回带对外序号和保存时间的副本。
func (d *itemDAOImpl) AppendItems(
	ctx context.Context,
	conversationID defs.ConversationID,
	sessionID defs.SessionID,
	items ...*defs.Item,
) ([]LoggedItem, error) {
	// 1. 检查各 Item，复制后清空保存时间再编码，保存时间由存储分配。
	payloads := make([]json.RawMessage, len(items))
	for index, item := range items {
		if err := validateItem(item); err != nil {
			return nil, err
		}
		stored := *item
		stored.UpdatedAt = 0
		payload, err := json.Marshal(&stored)
		if err != nil {
			return nil, fmt.Errorf("encode Item %q: %w", item.ID, err)
		}
		payloads[index] = payload
	}
	// 2. 一次追加全部载荷，读取存储分配的顺序号和时间。
	infos, err := d.logs.Append(ctx, storage.StreamPath(conversationID), storage.StreamID(sessionID), payloads...)
	if err != nil {
		return nil, fmt.Errorf("append Items: %w", err)
	}
	// 3. 写入后再读取接续标记，和首批 Item 同批写入的分叉标记也会读到。
	base, err := d.getSessionHistoryBase(ctx, conversationID, sessionID)
	if err != nil {
		return nil, err
	}
	// 4. 换算对外序号，复制 Item 并填写保存时间。
	offset := base.getOrdinalOffset()
	saved := make([]LoggedItem, len(items))
	for index, item := range items {
		copied := item.Clone()
		copied.UpdatedAt = infos[index].UpdatedAt
		saved[index] = LoggedItem{
			Ordinal: infos[index].Ordinal + offset,
			Item:    copied,
		}
	}
	return saved, nil
}

// ListItems 按对外序号分页读取 Session 日志；用户分叉的根 Session 一页只读来源历史或本 Session 中的一段。
func (d *itemDAOImpl) ListItems(ctx context.Context, request ListItemsRequest) (storage.Page[LoggedItem, storage.Ordinal], error) {
	// 1. 检查页大小并读取接续标记，没有标记时直接读取本 Session。
	if request.Limit <= 0 {
		return storage.Page[LoggedItem, storage.Ordinal]{}, errors.New("item list limit must be positive")
	}
	base, err := d.getSessionHistoryBase(ctx, request.ConversationID, request.SessionID)
	if err != nil {
		return storage.Page[LoggedItem, storage.Ordinal]{}, err
	}
	if base == nil {
		return d.listSessionItems(ctx, request, nil)
	}
	// 2. 补齐并检查读取方向。
	order, err := request.Order.WithDefault()
	if err != nil {
		return storage.Page[LoggedItem, storage.Ordinal]{}, err
	}
	request.Order = order

	// 3. 正序时游标还在来源范围内就读来源；来源读完时把 Next 设为 SourceEndOrdinal-1，也就是本 Session 首行标记的位置，
	// 下一页从第 2 行读起；来源这一页没有内容时直接读本 Session。
	if order == storage.DirectionAsc {
		if request.Cursor < base.SourceEndOrdinal-1 {
			source := d.listSourceItems(ctx, base, order, request.Cursor, request.Limit)
			if len(source.Items) > 0 {
				if source.Next == 0 {
					source.Next = base.SourceEndOrdinal - 1
				}
				return source, nil
			}
			request.Cursor = base.SourceEndOrdinal - 1
		}
		return d.listSessionItems(ctx, request, base)
	}

	// 4. 倒序时游标为 0 或还在本 Session 范围内就先读本 Session；读完时把 Next 设为 SourceEndOrdinal，下一页转去读来源；
	// 本 Session 这一页没有内容时直接往前读来源。
	if request.Cursor == 0 || request.Cursor > base.SourceEndOrdinal {
		page, err := d.listSessionItems(ctx, request, base)
		if err != nil {
			return storage.Page[LoggedItem, storage.Ordinal]{}, err
		}
		if len(page.Items) > 0 {
			if page.Next == 0 {
				page.Next = base.SourceEndOrdinal
			}
			return page, nil
		}
		request.Cursor = base.SourceEndOrdinal
	}
	return d.listSourceItems(ctx, base, order, request.Cursor, request.Limit), nil
}

// listSessionItems 读取本 Session 的一页 Item，并换算对外序号。
func (d *itemDAOImpl) listSessionItems(
	ctx context.Context,
	request ListItemsRequest,
	base *sessionHistoryBase,
) (storage.Page[LoggedItem, storage.Ordinal], error) {
	// 1. 按 SourceEndOrdinal-2 的偏移把对外游标换算成日志行号，没有标记时偏移为零。
	var result storage.Page[LoggedItem, storage.Ordinal]
	offset := base.getOrdinalOffset()
	cursor := request.Cursor
	if cursor != 0 {
		cursor -= offset
	}
	// 2. 读取日志页。
	page, err := d.logs.List(ctx, storage.ListLogEntriesRequest{
		Path:     storage.StreamPath(request.ConversationID),
		StreamID: storage.StreamID(request.SessionID),
		Order:    request.Order,
		Cursor:   cursor,
		Limit:    request.Limit,
	})
	if err != nil {
		return result, fmt.Errorf("list Items: %w", err)
	}
	// 3. 跳过首行接续标记，解码检查 Item 并换算返回序号和下一页游标。
	result.Items = make([]LoggedItem, 0, len(page.Items))
	for _, entry := range page.Items {
		if base != nil && entry.Ordinal == 1 {
			continue
		}
		item, err := decodePayload[*defs.Item](entry.Payload)
		if err != nil {
			return storage.Page[LoggedItem, storage.Ordinal]{}, fmt.Errorf("decode Item at ordinal %d: %w", entry.Ordinal, err)
		}
		if err := validateItem(item); err != nil {
			return storage.Page[LoggedItem, storage.Ordinal]{}, fmt.Errorf("stored Item at ordinal %d: %w", entry.Ordinal, err)
		}
		item.UpdatedAt = entry.UpdatedAt
		result.Items = append(result.Items, LoggedItem{
			Ordinal: entry.Ordinal + offset,
			Item:    item,
		})
	}
	if page.Next != 0 {
		result.Next = page.Next + offset
	}
	return result, nil
}

// listSourceItems 读取分叉来源的一页历史，只保留序号小于 SourceEndOrdinal 的部分；读不到时返回空页。
func (d *itemDAOImpl) listSourceItems(
	ctx context.Context,
	base *sessionHistoryBase,
	order storage.Direction,
	cursor storage.Ordinal,
	limit int,
) storage.Page[LoggedItem, storage.Ordinal] {
	// 1. 按来源对话和 Session 递归调用 ListItems，失败时记录 Warn 并返回空页。
	page, err := d.ListItems(ctx, ListItemsRequest{
		ConversationID: base.SourceConversationID,
		SessionID:      base.SourceSessionID,
		Order:          order,
		Cursor:         cursor,
		Limit:          limit,
	})
	if err != nil {
		telemetry.GetLogger(ctx).Warn("read fork source history failed",
			"gen_ai.conversation.id", string(base.SourceConversationID), "openvaala.session.id", string(base.SourceSessionID), "error", err)
		return storage.Page[LoggedItem, storage.Ordinal]{Items: []LoggedItem{}}
	}
	// 2. 截掉序号不小于 SourceEndOrdinal 的内容，截过时把 Next 清零，表示来源已读完。
	end := slices.IndexFunc(page.Items, func(logged LoggedItem) bool { return logged.Ordinal >= base.SourceEndOrdinal })
	if end >= 0 {
		page.Items, page.Next = page.Items[:end], 0
	}
	return page
}

// getSessionHistoryBase 读取并缓存 Session 首行的历史接续标记。
func (d *itemDAOImpl) getSessionHistoryBase(
	ctx context.Context,
	conversationID defs.ConversationID,
	sessionID defs.SessionID,
) (*sessionHistoryBase, error) {
	// 1. 先查缓存。
	key := sessionStreamKey{
		conversationID: conversationID,
		sessionID:      sessionID,
	}
	if base, found := d.historyBases.Load(key); found {
		return base, nil
	}
	// 2. 读取首行；空日志返回 nil 且不缓存。
	page, err := d.logs.List(ctx, storage.ListLogEntriesRequest{
		Path:     storage.StreamPath(conversationID),
		StreamID: storage.StreamID(sessionID),
		Order:    storage.DirectionAsc,
		Limit:    1,
	})
	if err != nil {
		return nil, fmt.Errorf("list first Item: %w", err)
	}
	if len(page.Items) == 0 {
		return nil, nil
	}
	// 3. 解码首行；是带来源对话的分叉标记时检查它并取出来源位置。
	first, err := decodePayload[*defs.Item](page.Items[0].Payload)
	if err != nil {
		return nil, fmt.Errorf("decode Item at ordinal 1: %w", err)
	}
	var base *sessionHistoryBase
	if first != nil && first.Kind == defs.ItemSessionFork && first.SessionFork != nil && first.SessionFork.SourceConversationID != "" {
		if err := validateItem(first); err != nil {
			return nil, fmt.Errorf("stored Item at ordinal 1: %w", err)
		}
		base = &sessionHistoryBase{
			SourceConversationID: first.SessionFork.SourceConversationID,
			SourceSessionID:      first.SessionFork.SourceSessionID,
			SourceEndOrdinal:     storage.Ordinal(first.SessionFork.SourceEndOrdinal),
		}
	}
	// 4. 缓存接续标记；首行不是接续标记时缓存 nil。
	d.historyBases.Store(key, base)
	return base, nil
}

// getOrdinalOffset 返回日志行号换算为对外序号的偏移量。
func (base *sessionHistoryBase) getOrdinalOffset() storage.Ordinal {
	// 1. 没有接续标记时返回零，否则让第 2 行对应到 SourceEndOrdinal。
	if base == nil {
		return 0
	}
	return base.SourceEndOrdinal - 2
}

// ScanItems 分页读取 Session 日志，按指定方向逐条交给调用方。
func (d *itemDAOImpl) ScanItems(
	ctx context.Context,
	conversationID defs.ConversationID,
	sessionID defs.SessionID,
	order storage.Direction,
) iter.Seq2[*defs.Item, error] {
	// 1. 分页读取并逐条交给调用方，末页、读取错误或调用方停止时结束。
	return func(yield func(*defs.Item, error) bool) {
		var cursor storage.Ordinal
		for {
			page, err := d.ListItems(ctx, ListItemsRequest{
				ConversationID: conversationID,
				SessionID:      sessionID,
				Order:          order,
				Cursor:         cursor,
				Limit:          scanPageSize,
			})
			if err != nil {
				yield(nil, err)
				return
			}
			for _, logged := range page.Items {
				if !yield(logged.Item, nil) {
					return
				}
			}
			if page.Next == 0 {
				return
			}
			cursor = page.Next
		}
	}
}

// GetLastOrdinal 返回 Session 最新 Item 的对外序号。
func GetLastOrdinal(ctx context.Context, query ItemQuery, conversationID defs.ConversationID, sessionID defs.SessionID) (storage.Ordinal, error) {
	// 1. 倒序读取一条 Item，返回它的序号；空日志返回零，读取失败时返回错误。
	page, err := query.ListItems(ctx, ListItemsRequest{
		ConversationID: conversationID,
		SessionID:      sessionID,
		Order:          storage.DirectionDesc,
		Limit:          1,
	})
	if err != nil {
		return 0, fmt.Errorf("read last ordinal of Session %s: %w", sessionID, err)
	}
	if len(page.Items) == 0 {
		return 0, nil
	}
	return page.Items[0].Ordinal, nil
}

// validateItem 检查 Item ID 和各类载荷，错误里带上 Item ID。
func validateItem(item *defs.Item) error {
	// 1. 检查 Item ID、摘要和分叉标记。
	if item == nil || item.ID == "" {
		return errors.New("item requires an ID")
	}
	if err := item.ValidateContextSummary(); err != nil {
		return fmt.Errorf("item %q: %w", item.ID, err)
	}
	if err := item.ValidateSessionFork(); err != nil {
		return fmt.Errorf("item %q: %w", item.ID, err)
	}
	// 2. 检查 Skill、AGENTS.md 指令、记忆目录和客户端指令的类型与载荷，再检查它们的 TurnID 和必填字段。
	if err := item.ValidateKindPayload(); err != nil {
		return fmt.Errorf("item %q: %w", item.ID, err)
	}
	if instruction := item.SkillInstruction; instruction != nil {
		if item.TurnID == "" || instruction.InputItemID == "" || instruction.Name == "" || instruction.Path == "" {
			return fmt.Errorf("item %q: skill instruction requires a turn, input, name and path", item.ID)
		}
	}
	if instruction := item.AgentsInstruction; instruction != nil {
		if item.TurnID == "" || instruction.Executor == "" || instruction.Path == "" {
			return fmt.Errorf("item %q: agents instruction requires a turn, executor and path", item.ID)
		}
	}
	if index := item.MemoryIndex; index != nil {
		if item.TurnID == "" || index.Path == "" {
			return fmt.Errorf("item %q: memory index requires a turn and path", item.ID)
		}
	}
	// 3. 检查模型和 Executor 切换记录。
	if err := item.ValidateModelChange(); err != nil {
		return fmt.Errorf("item %q: %w", item.ID, err)
	}
	if err := item.ValidateExecutorsChange(); err != nil {
		return fmt.Errorf("item %q: %w", item.ID, err)
	}
	// 4. 检查工具结果里的每张图片都有 DataURL。
	if item.ToolResult != nil {
		for _, image := range item.ToolResult.Images {
			if image == nil || image.DataURL == "" {
				return fmt.Errorf("item %q: tool result image source is missing", item.ID)
			}
		}
	}
	return nil
}
