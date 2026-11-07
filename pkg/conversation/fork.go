package conversation

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// ForkConversationRequest 是 ForkConversation 的参数。BeforeTurnID 和 LastTurnID 最多填一个；
// 都不填时复制到最后一条已保存的 Item，包括还在运行的轮次已保存的部分。
type ForkConversationRequest struct {
	// Source 是来源对话记录（已保存，可以已归档）。
	Source defs.Conversation
	// SourceQuery 读来源对话；ConversationOpt 的 Query、Mutation 读写分叉出的对话。
	SourceQuery dao.Query
	// ForkedConversationID 是分叉出的新对话 ID；为空时生成。
	ForkedConversationID defs.ConversationID
	// BeforeTurnID 非空时复制到这一轮之前，不含这一轮；回退重来时用。
	BeforeTurnID defs.TurnID
	// LastTurnID 非空时复制到这一轮，含这一轮；这一轮还在运行时只复制已保存的部分。
	LastTurnID defs.TurnID
	// Model 是新根 Session 的模型；零值时照抄来源根 Session 的模型。
	Model defs.ModelSelection
	// ExecutorSettings 原样存进新对话。
	ExecutorSettings defs.ExecutorSettings
}

// forkPageSize 是分叉时每次读 Turn 记录、读 Item 和写 Item 的条数。
const forkPageSize = 256

// ForkConversation 复制来源根 Session 选中轮次的历史和 Turn 记录，保存成新的分叉对话，返回新对话记录。
func ForkConversation(ctx context.Context, request *ForkConversationRequest, opt *ConversationOpt) (defs.Conversation, error) {
	// 1. 校验截取参数和定义，读取来源根 Session，选出新根 Session 的模型；请求没给模型时照抄来源根 Session 的。
	if request.BeforeTurnID != "" && request.LastTurnID != "" {
		return defs.Conversation{}, ErrForkBoundaryConflict
	}
	if err := validateConversationDependencies(opt); err != nil {
		return defs.Conversation{}, err
	}
	if err := validateConversationDefinitions(opt.Definitions); err != nil {
		return defs.Conversation{}, err
	}
	agentDefinitions, models, err := validateConversationConfig(ctx, opt.Definitions, nil, nil)
	if err != nil {
		return defs.Conversation{}, err
	}
	source := request.Source
	sourceRoot, found, err := request.SourceQuery.GetSession(ctx, source.ID, source.RootSessionID)
	if err != nil {
		return defs.Conversation{}, err
	}
	if !found {
		return defs.Conversation{}, fmt.Errorf("root Session %s of Conversation %s was not found", source.RootSessionID, source.ID)
	}
	definition, found := agentDefinitions[sourceRoot.AgentName]
	if !found {
		return defs.Conversation{}, fmt.Errorf("root agent %q is not configured", sourceRoot.AgentName)
	}
	model := request.Model
	if model == (defs.ModelSelection{}) {
		model = sourceRoot.Model
	}
	selection, err := getRequestedModelSelection(models, model, definition.Model)
	if err != nil {
		return defs.Conversation{}, err
	}

	// 2. 按开始顺序分页读出来源的全部 Turn，按请求选出保留的轮次。
	var turns []defs.Turn
	turnRequest := dao.ListTurnsRequest{
		SessionID: source.RootSessionID,
		Order:     storage.Order{Direction: storage.DirectionAsc},
		Limit:     forkPageSize,
	}
	for {
		page, err := request.SourceQuery.ListTurns(ctx, turnRequest)
		if err != nil {
			return defs.Conversation{}, fmt.Errorf("read Turns of Conversation %s: %w", source.ID, err)
		}
		turns = append(turns, page.Items...)
		if page.Next == (dao.RecordCursor[defs.TurnID]{}) {
			break
		}
		turnRequest.Cursor = page.Next
	}
	keptTurns, err := selectForkTurns(turns, request.BeforeTurnID, request.LastTurnID)
	if err != nil {
		return defs.Conversation{}, err
	}

	// 3. 正序分页读取保留轮次的 Item，遇到不属于保留轮次的 Item 时停止。
	isKeptTurn := make(map[defs.TurnID]bool, len(keptTurns))
	for _, turn := range keptTurns {
		isKeptTurn[turn.ID] = true
	}
	var items []dao.LoggedItem
	itemRequest := dao.ListItemsRequest{
		ConversationID: source.ID,
		SessionID:      source.RootSessionID,
		Order:          storage.DirectionAsc,
		Limit:          forkPageSize,
	}
	for {
		page, err := request.SourceQuery.ListItems(ctx, itemRequest)
		if err != nil {
			return defs.Conversation{}, fmt.Errorf("read history of Conversation %s: %w", source.ID, err)
		}
		end := slices.IndexFunc(page.Items, func(logged dao.LoggedItem) bool { return !isKeptTurn[logged.Item.TurnID] })
		if end >= 0 {
			items = append(items, page.Items[:end]...)
			break
		}
		items = append(items, page.Items...)
		if page.Next == 0 {
			break
		}
		itemRequest.Cursor = page.Next
	}

	// 4. 校验 Item 序号从 1 起连续，且每个保留轮次的 Item 都读全了。
	for index, logged := range items {
		if logged.Ordinal != storage.Ordinal(index+1) {
			return defs.Conversation{}, fmt.Errorf("fork Conversation %s: source history is incomplete at ordinal %d", source.ID, index+1)
		}
	}
	for _, turn := range keptTurns {
		if turn.ItemsRange.LastOrdinal > uint64(len(items)) {
			return defs.Conversation{}, fmt.Errorf("fork Conversation %s: source history is incomplete for Turn %s", source.ID, turn.ID)
		}
	}

	// 5. 最近的摘要覆盖到的 Item 前面还有历史时，只从这个 Item 开始复制，并在最前面写一条 SessionFork，指向来源里没复制的那段。
	start := 0
	var copied []*defs.Item
	if boundary := getSummaryBoundaryIndex(items); boundary >= 0 && items[boundary].Ordinal > 1 {
		start = boundary
		copied = append(copied, &defs.Item{
			ID:   defs.NewItemID(),
			Kind: defs.ItemSessionFork,
			SessionFork: &defs.SessionFork{
				SourceSessionID:      source.RootSessionID,
				SourceConversationID: source.ID,
				SourceEndOrdinal:     int64(items[boundary].Ordinal),
			},
		})
	}
	for _, logged := range items[start:] {
		copied = append(copied, logged.Item)
	}
	// 6. 统计保留轮次里的全部消息数，没复制、由 SessionFork 指向来源的那段也算。
	messageCount := 0
	for _, logged := range items {
		if defs.IsConversationMessage(logged.Item) {
			messageCount++
		}
	}

	// 7. 依次保存新根 Session、Item、Turn，最后保存对话记录；中途失败时返回错误，已写入的数据留着。
	record := defs.Conversation{
		ID:               cmp.Or(request.ForkedConversationID, defs.NewConversationID()),
		RootSessionID:    defs.NewSessionID(),
		ExecutorSettings: request.ExecutorSettings,
		Title:            source.Title,
		MessageCount:     messageCount,
		ForkedFromID:     source.ID,
	}
	root := defs.Session{
		ID:             record.RootSessionID,
		ConversationID: record.ID,
		AgentName:      sourceRoot.AgentName,
		Model:          selection,
	}
	if err := opt.Mutation.SetSession(ctx, root); err != nil {
		return defs.Conversation{}, fmt.Errorf("fork Conversation %s into %s: save root Session %s: %w", source.ID, record.ID, root.ID, err)
	}
	for batch := range slices.Chunk(copied, forkPageSize) {
		if _, err := opt.Mutation.AppendItems(ctx, record.ID, root.ID, batch...); err != nil {
			return defs.Conversation{}, fmt.Errorf("fork Conversation %s into %s: copy Items: %w", source.ID, record.ID, err)
		}
	}
	// Turn 只换 ConversationID 和 SessionID，用量等其他字段照抄。
	for _, turn := range keptTurns {
		turn.ConversationID, turn.SessionID = record.ID, root.ID
		if err := opt.Mutation.SetTurn(ctx, turn); err != nil {
			return defs.Conversation{}, fmt.Errorf("fork Conversation %s into %s: copy Turn %s: %w", source.ID, record.ID, turn.ID, err)
		}
	}
	if err := opt.Mutation.SetConversation(ctx, record); err != nil {
		return defs.Conversation{}, fmt.Errorf("fork Conversation %s into %s: save record: %w", source.ID, record.ID, err)
	}
	return record, nil
}

// selectForkTurns 选择分叉要复制的 Turn。
func selectForkTurns(turns []defs.Turn, beforeTurnID, lastTurnID defs.TurnID) ([]defs.Turn, error) {
	// 1. 在按开始顺序排列的 Turn 中，按 beforeTurnID、lastTurnID 定位截取点，都不给时取全部；指定的轮次找不到时返回错误。
	var end int
	switch {
	case beforeTurnID != "":
		end = slices.IndexFunc(turns, func(turn defs.Turn) bool { return turn.ID == beforeTurnID })
		if end < 0 {
			return nil, ErrForkTurnNotFound
		}
	case lastTurnID != "":
		index := slices.IndexFunc(turns, func(turn defs.Turn) bool { return turn.ID == lastTurnID })
		if index < 0 {
			return nil, ErrForkTurnNotFound
		}
		end = index + 1
	default:
		end = len(turns)
	}
	// 2. 截取点之前没有轮次时返回 ErrNothingToFork，否则返回截取点之前的轮次。
	if end == 0 {
		return nil, ErrNothingToFork
	}
	return turns[:end], nil
}
