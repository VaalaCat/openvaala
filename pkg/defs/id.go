package defs

import "github.com/google/uuid"

// ItemID 唯一标识一条由 OpenVaala 创建的 Item，并按创建时间排序。
type ItemID string

// SessionID 唯一标识一个 Session，也就是一个 Agent 在对话里的一段上下文，并按创建时间排序。
type SessionID string

// TurnID 唯一标识一轮 Agent 执行，并按创建时间排序。
type TurnID string

// ModelCallID 唯一标识一次模型调用，并按创建时间排序。
type ModelCallID string

// HookRunID 唯一标识一次 hook 运行，并按创建时间排序。
type HookRunID string

// InputID 是调用方给一次输入提交起的去重标识，由调用方生成，与 Item 身份无关。
type InputID string

// NewItemID 创建按时间排序的 Item ID。
func NewItemID() ItemID {
	// 1. 生成 UUID v7 并转换为 Item ID。
	return ItemID(uuid.Must(uuid.NewV7()).String())
}

// NewHookRunID 创建按时间排序的 hook 运行 ID。
func NewHookRunID() HookRunID {
	// 1. 生成 UUID v7 并转换为 hook 运行 ID。
	return HookRunID(uuid.Must(uuid.NewV7()).String())
}

// NewSessionID 创建按时间排序的 Session ID。
func NewSessionID() SessionID {
	// 1. 生成 UUID v7 并转换为 Session ID。
	return SessionID(uuid.Must(uuid.NewV7()).String())
}

// NewTurnID 创建按时间排序的 Turn ID。
func NewTurnID() TurnID {
	// 1. 生成 UUID v7 并转换为 Turn ID。
	return TurnID(uuid.Must(uuid.NewV7()).String())
}

// NewModelCallID 创建按时间排序的模型调用 ID。
func NewModelCallID() ModelCallID {
	// 1. 生成 UUID v7 并转换为模型调用 ID。
	return ModelCallID(uuid.Must(uuid.NewV7()).String())
}

// ConversationID 唯一标识一个对话，并按创建时间排序。
type ConversationID string

// NewConversationID 创建按时间排序的对话 ID。
func NewConversationID() ConversationID {
	// 1. 生成 UUID v7 并转换为对话 ID。
	return ConversationID(uuid.Must(uuid.NewV7()).String())
}
