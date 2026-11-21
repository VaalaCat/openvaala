package dao

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"iter"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// checkpointEntry 是 Session checkpoint 流的一行，只携带一种载荷。
type checkpointEntry struct {
	// Session 保存本 Session 的全份待办；Turn 进度行不填。
	Session *defs.SessionCheckpoint `json:"session,omitempty"`
	// Turn 保存原 Turn 的单次进度；Session 待办行不填。
	Turn *defs.TurnCheckpoint `json:"turn,omitempty"`
}

// GetSessionCheckpoint 倒序读取 Session 最新的待办。
func (d *sessionDAOImpl) GetSessionCheckpoint(ctx context.Context, conversationID defs.ConversationID, sessionID defs.SessionID) (*defs.SessionCheckpoint, error) {
	// 1. 检查 metadata 删除事实，删除中的 Session 不再返回待办。
	if _, err := scanSessionMetadata(ctx, d.logs, conversationID, sessionID, false); err != nil {
		return nil, err
	}
	// 2. 查找最近一行 Session 待办，空待办原样返回。
	entry, err := getCheckpointEntry(ctx, d.logs, conversationID, sessionID, "")
	return entry.Session, err
}

// GetTurnCheckpoint 倒序读取指定 Turn 最新的进度。
func (d *turnDAOImpl) GetTurnCheckpoint(ctx context.Context, conversationID defs.ConversationID, sessionID defs.SessionID, turnID defs.TurnID) (*defs.TurnCheckpoint, error) {
	// 1. 检查轮次身份，查找该轮最近一行进度。
	if turnID == "" {
		return nil, errors.New("get Turn checkpoint requires a Turn ID")
	}
	if _, err := scanSessionMetadata(ctx, d.logs, conversationID, sessionID, false); err != nil {
		return nil, err
	}
	entry, err := getCheckpointEntry(ctx, d.logs, conversationID, sessionID, turnID)
	return entry.Turn, err
}

// AppendSessionCheckpoint 校验并追加一份 Session 待办。
func (d *sessionDAOImpl) AppendSessionCheckpoint(ctx context.Context, conversationID defs.ConversationID, sessionID defs.SessionID, checkpoint *defs.SessionCheckpoint) error {
	// 1. 将 Session 待办作为一行追加到 checkpoint 流。
	return appendCheckpointEntry(ctx, d.logs, conversationID, sessionID, checkpointEntry{Session: checkpoint})
}

// AppendTurnCheckpoint 校验并追加一次 Turn 进度。
func (d *turnDAOImpl) AppendTurnCheckpoint(ctx context.Context, conversationID defs.ConversationID, sessionID defs.SessionID, checkpoint *defs.TurnCheckpoint) error {
	// 1. 将 Turn 进度作为一行追加到所属 Session 的 checkpoint 流。
	return appendCheckpointEntry(ctx, d.logs, conversationID, sessionID, checkpointEntry{Turn: checkpoint})
}

// DeleteSessionCheckpoints 删除指定 Session 的 checkpoint 流。
func (d *sessionDAOImpl) DeleteSessionCheckpoints(ctx context.Context, conversationID defs.ConversationID, sessionID defs.SessionID) error {
	// 1. 按对话和 Session ID 删除整条流。
	if conversationID == "" || sessionID == "" {
		return errors.New("delete checkpoints requires a conversation ID and session ID")
	}
	if err := d.logs.Delete(ctx, storage.StreamPath(conversationID), checkpointStreamID(sessionID)); err != nil {
		return fmt.Errorf("delete Session %q checkpoints: %w", sessionID, err)
	}
	return nil
}

// checkpointStreamID 返回 Session 的待办和轮次进度共用的日志名。
func checkpointStreamID(sessionID defs.SessionID) storage.StreamID {
	// 1. 在 Session ID 前添加 checkpoint-。
	return storage.StreamID("checkpoint-" + sessionID)
}

// getCheckpointEntry 倒序查找 Session 待办或指定 Turn 进度。
func getCheckpointEntry(ctx context.Context, logs storage.LogStorage, conversationID defs.ConversationID, sessionID defs.SessionID, turnID defs.TurnID) (checkpointEntry, error) {
	// 1. 从最新行起查找所需的 Session 或 Turn 进度。
	for entry, err := range scanCheckpointEntries(ctx, logs, conversationID, sessionID) {
		if err != nil {
			return checkpointEntry{}, err
		}
		if (turnID == "" && entry.Session != nil) || (entry.Turn != nil && entry.Turn.TurnID == turnID) {
			return entry, nil
		}
	}
	return checkpointEntry{}, nil
}

// scanCheckpointEntries 按本 Session 日志序号倒序读取并校验进度。
func scanCheckpointEntries(ctx context.Context, logs storage.LogStorage, conversationID defs.ConversationID, sessionID defs.SessionID) iter.Seq2[checkpointEntry, error] {
	return func(yield func(checkpointEntry, error) bool) {
		// 1. 分页读取进度，解码每条记录。
		request := storage.ListLogEntriesRequest{Path: storage.StreamPath(conversationID), StreamID: checkpointStreamID(sessionID), Order: storage.DirectionDesc, Limit: scanPageSize}
		for {
			page, err := logs.List(ctx, request)
			if err != nil {
				yield(checkpointEntry{}, fmt.Errorf("list Session %q checkpoints: %w", sessionID, err))
				return
			}
			for _, line := range page.Items {
				entry, err := decodeCheckpointEntry(line.Payload)
				if err != nil {
					yield(checkpointEntry{}, fmt.Errorf("decode Session %q checkpoint ordinal %d: %w", sessionID, line.Ordinal, err))
					return
				}
				if !yield(entry, nil) {
					return
				}
			}
			if page.Next == 0 {
				return
			}
			request.Cursor = page.Next
		}
	}
}

// appendCheckpointEntry 校验载荷并追加一行。
func appendCheckpointEntry(ctx context.Context, logs storage.LogStorage, conversationID defs.ConversationID, sessionID defs.SessionID, entry checkpointEntry) error {
	// 1. 检查身份和载荷，再编码成日志行。
	if conversationID == "" || sessionID == "" {
		return errors.New("append checkpoint requires a conversation ID and session ID")
	}
	if err := entry.validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode Session %q checkpoint: %w", sessionID, err)
	}
	// 2. 向本 Session 的 checkpoint 流追加该行。
	if _, err := logs.Append(ctx, storage.StreamPath(conversationID), checkpointStreamID(sessionID), payload); err != nil {
		return fmt.Errorf("append Session %q checkpoint: %w", sessionID, err)
	}
	return nil
}

// decodeCheckpointEntry 解码并校验一行 checkpoint。
func decodeCheckpointEntry(payload json.RawMessage) (checkpointEntry, error) {
	// 1. 拒绝任意层级的重复键和未定义字段，再检查行的类型和对应载荷。
	var entry checkpointEntry
	if err := jsonv2.Unmarshal(payload, &entry, jsonv2.RejectUnknownMembers(true)); err != nil {
		return checkpointEntry{}, err
	}
	return entry, entry.validate()
}

// validate 检查一行 checkpoint 的载荷及其必填字段。
func (entry checkpointEntry) validate() error {
	// 1. 每行恰好有一种载荷，按类型检查该份载荷。
	if (entry.Session == nil) == (entry.Turn == nil) {
		return errors.New("checkpoint must contain one Session or Turn payload")
	}
	if entry.Session != nil {
		return validateSessionCheckpoint(entry.Session)
	}
	return validateTurnCheckpoint(entry.Turn)
}

// validateSessionCheckpoint 检查待办的轮次身份及各组原输入。
func validateSessionCheckpoint(checkpoint *defs.SessionCheckpoint) error {
	// 1. 检查未分轮队列里的批次和输入组。
	for _, batches := range [][]defs.SessionInputBatch{checkpoint.PendingUserInputs, checkpoint.PendingAgentMessages} {
		for _, batch := range batches {
			if len(batch) == 0 {
				return errors.New("checkpoint input batch has no groups")
			}
			for _, group := range batch {
				if err := validateSessionInputGroup(group); err != nil {
					return err
				}
			}
		}
	}
	// 2. 检查已分配轮次及该轮的开场和补充输入。
	if inputs := checkpoint.TurnInputs; inputs != nil {
		if inputs.TurnID == "" {
			return errors.New("checkpoint allocated inputs require a Turn ID")
		}
		if inputs.OpeningInput != nil {
			if err := validateSessionInputGroup(*inputs.OpeningInput); err != nil {
				return err
			}
		}
		for _, group := range inputs.SteerInputs {
			if err := validateSessionInputGroup(group); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateSessionInputGroup 检查输入组的原消息和紧随其后的 skill 正文。
func validateSessionInputGroup(group defs.SessionInputGroup) error {
	// 1. 检查组首消息及其 ID。
	if len(group.Items) == 0 || group.Items[0] == nil || group.Items[0].ID == "" {
		return errors.New("checkpoint input group requires an opening message with an Item ID")
	}
	input := group.Items[0]
	if err := input.ValidateInputMessage(); err != nil {
		return fmt.Errorf("checkpoint input %q: %w", input.ID, err)
	}
	// 2. 检查组内其余 Item 是关联到这条消息的 skill。
	for _, item := range group.Items[1:] {
		if item == nil || item.ID == "" || item.Kind != defs.ItemSkillInstruction || item.SkillInstruction == nil || item.SkillInstruction.InputItemID != input.ID {
			return fmt.Errorf("checkpoint input %q has an invalid skill Item", input.ID)
		}
		if err := item.ValidateContextItem(); err != nil {
			return fmt.Errorf("checkpoint input %q skill %q: %w", input.ID, item.ID, err)
		}
	}
	return nil
}

// validateTurnCheckpoint 检查轮次、步号以及该阶段的载荷。
func validateTurnCheckpoint(checkpoint *defs.TurnCheckpoint) error {
	// 1. 检查轮次和计数。
	if checkpoint.TurnID == "" || checkpoint.Step < 1 || checkpoint.StopHookContinuations < 0 || checkpoint.StopHookContinuations > 8 {
		return errors.New("Turn checkpoint requires a Turn ID, a positive step and a stop hook count from 0 through 8")
	}
	if checkpoint.StopHookContinuations > 0 && !checkpoint.IsContinuedByStopHook {
		return errors.New("Turn checkpoint stop hook continuation state is contradictory")
	}
	// 2. 检查阶段与载荷一一对应。
	switch checkpoint.Phase {
	case defs.TurnCheckpointPhaseTools:
		if checkpoint.Request != nil || checkpoint.Tools == nil || checkpoint.Tools.ModelCallID == "" || checkpoint.StopHookContinuations != 0 {
			return errors.New("tools checkpoint requires only a tools payload with a model call ID and zero stop hook continuations")
		}
		return nil
	case defs.TurnCheckpointPhaseRequest:
		if checkpoint.Tools != nil || checkpoint.Request == nil || checkpoint.Request.ModelCallID == "" {
			return errors.New("request checkpoint requires only a request payload with a model call ID")
		}
	default:
		return fmt.Errorf("unsupported Turn checkpoint phase %q", checkpoint.Phase)
	}
	// 3. 检查普通生成和摘要各自的原输入范围。
	request := checkpoint.Request
	switch request.Purpose {
	case defs.ModelCallPurposeConversation:
		if request.Compaction != nil || request.ModelCallThroughItemID == "" {
			return errors.New("conversation checkpoint requires only a model input range")
		}
	case defs.ModelCallPurposeCompaction:
		compaction := request.Compaction
		if request.ModelCallThroughItemID != "" || compaction == nil || compaction.SummaryItemID == "" || compaction.CoveredThroughItemID == "" ||
			compaction.CompactModelRef.ProviderID == "" || compaction.CompactModelRef.ModelID == "" {
			return errors.New("compaction checkpoint requires only a summary range with a summary item ID, covered item ID and model reference")
		}
	default:
		return fmt.Errorf("unsupported model call purpose %q", request.Purpose)
	}
	return nil
}
