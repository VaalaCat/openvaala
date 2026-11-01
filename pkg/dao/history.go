package dao

import (
	"context"
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
