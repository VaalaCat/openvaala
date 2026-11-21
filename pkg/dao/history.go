package dao

import (
	"context"
	"fmt"
	"slices"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// LoadContextHistory 倒序读取 Session 日志，返回发给模型的上下文历史：最新摘要和它没覆盖的 Item。
func LoadContextHistory(
	ctx context.Context,
	items ItemQuery,
	conversationID defs.ConversationID,
	sessionID defs.SessionID,
	excludeTurnID defs.TurnID,
) ([]*defs.Item, error) {
	// 1. 倒序读取 Item，跳过 excludeTurnID 这一轮的 Item。
	var collected []*defs.Item
	var summary *defs.Item
	passedFork := false
	for item, err := range items.ScanItems(ctx, conversationID, sessionID, storage.DirectionDesc) {
		if err != nil {
			return nil, err
		}
		if excludeTurnID != "" && item.TurnID == excludeTurnID {
			continue
		}
		collected = append(collected, item)
		// 2. 记下是否读过分叉标记；最新摘要出现在分叉标记之前的继承历史里时，读到它就停止。
		if item.Kind == defs.ItemSessionFork {
			passedFork = true
			continue
		}
		if summary == nil && item.Kind == defs.ItemContextSummary {
			summary = item
			if passedFork {
				break
			}
			continue
		}
		// 3. 本 Session 的摘要读到它覆盖的最后一条 Item 时停止。
		if summary != nil && item.ID == summary.ContextSummary.CoveredThroughItemID {
			break
		}
	}
	// 4. 恢复正序，选出发给模型的上下文历史。
	slices.Reverse(collected)
	return defs.SelectContextHistoryItems(collected)
}

// GetModelRequestHistory 重建一次普通请求使用的摘要、分叉来源和保留段。
func (d *itemDAOImpl) GetModelRequestHistory(ctx context.Context, conversationID defs.ConversationID, sessionID defs.SessionID, checkpoint *defs.TurnCheckpoint) ([]*defs.Item, error) {
	// 1. 从指定请求的进度往前找到本轮已生效的压缩；没有压缩则沿用进入本轮时的历史。
	if checkpoint == nil || checkpoint.Request == nil || checkpoint.Request.Purpose != defs.ModelCallPurposeConversation {
		return nil, fmt.Errorf("expected conversation request checkpoint")
	}
	var compaction *defs.TurnCompactionCheckpoint
	foundRequest := false
	for entry, err := range scanCheckpointEntries(ctx, d.logs, conversationID, sessionID) {
		if err != nil {
			return nil, err
		}
		if entry.Turn == nil {
			continue
		}
		if entry.Turn.TurnID != checkpoint.TurnID {
			if foundRequest {
				break
			}
			continue
		}
		request := entry.Turn.Request
		if request == nil {
			continue
		}
		if !foundRequest {
			foundRequest = request.Purpose == defs.ModelCallPurposeConversation && request.ModelCallID == checkpoint.Request.ModelCallID
			continue
		}
		if request.Compaction != nil {
			compaction = request.Compaction
			break
		}
	}
	if !foundRequest {
		return nil, fmt.Errorf("missing checkpoint for model call %q", checkpoint.Request.ModelCallID)
	}
	// 2. 选取本轮之前的分叉和摘要，以及本轮该请求可见的唯一摘要。
	var history []*defs.Item
	foundSummary := false
	through := checkpoint.Request.ModelCallThroughItemID
	foundEnd, enteredTurn := false, false
	for item, err := range d.ScanItems(ctx, conversationID, sessionID, storage.DirectionAsc) {
		if err != nil {
			return nil, err
		}
		if item.TurnID == checkpoint.TurnID {
			enteredTurn = true
		} else if enteredTurn {
			break
		}
		if foundEnd && item.Kind != defs.ItemContextSummary && item.Kind != defs.ItemSessionFork {
			continue
		}
		if item.Kind == defs.ItemContextSummary && item.TurnID == checkpoint.TurnID {
			if compaction == nil || item.ID != compaction.SummaryItemID {
				continue
			}
			if item.ContextSummary.CoveredThroughItemID != compaction.CoveredThroughItemID {
				return nil, fmt.Errorf("summary %q covers an unexpected item", item.ID)
			}
			foundSummary = true
		}
		history = append(history, item)
		if item.ID == through {
			foundEnd = true
		}
		if enteredTurn && foundEnd && (compaction == nil || foundSummary) {
			break
		}
	}
	if compaction != nil && !foundSummary {
		return nil, fmt.Errorf("missing summary %q for model call %q", compaction.SummaryItemID, checkpoint.Request.ModelCallID)
	}
	// 3. 先应用当时的摘要与失败调用过滤，再截到原请求的逻辑末项。
	history, err := defs.SelectContextHistoryItems(history)
	if err != nil {
		return nil, err
	}
	end := slices.IndexFunc(history, func(item *defs.Item) bool { return item.ID == through })
	if end < 0 {
		return nil, fmt.Errorf("context history is missing input item %q", through)
	}
	return history[:end+1], nil
}
