package runtimeprotocol

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
)

// TestProtocolNotificationNames 检查协议登记的 serve 通知和客户端通知名。
func TestProtocolNotificationNames(t *testing.T) {
	// 1. 准备协议登记的通知表和预期通知名。
	tests := []struct {
		name          string
		notifications []Notification
		want          []string
	}{
		{
			name:          "server_notifications",
			notifications: ServerNotifications,
			want: []string{
				"conversation/runningChanged", "conversation/runningClosed",
				"subscription/closed", "session/statusUpdated", "session/pendingInputsUpdated", "conversation/titleUpdated",
				"session/started", "session/stopped", "turn/started", "turn/finished", "items/saved",
				"item/started", "item/delta", "item/completed", "item/interrupted", "item/toolProgress",
				"hook/started", "hook/completed",
				"clientMCP/connect", "clientMCP/message", "clientMCP/disconnect",
				"terminal/output", "terminal/exited", "terminal/listChanged", "terminal/listClosed",
			},
		},
		{
			name:          "client_notifications",
			notifications: ClientNotifications,
			want:          []string{"clientMCP/message", "clientMCP/closed", "terminal/input", "terminal/resize"},
		},
	}
	// 2. 逐项核对通知名集合，登记时不允许重复。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := make([]string, len(tt.notifications))
			for index, notification := range tt.notifications {
				got[index] = notification.Name
			}
			slices.Sort(got)
			want := slices.Sorted(slices.Values(tt.want))
			if !slices.Equal(got, want) {
				t.Fatalf("notification names = %v, want %v", got, want)
			}
		})
	}
}

// TestRunningConversationsWire 检查全局运行消息只携带运行 ID 或关闭原因。
func TestRunningConversationsWire(t *testing.T) {
	// 1. 分别编码空集合、非空集合和关闭通知。
	for _, tt := range []struct {
		name  string
		value any
		want  string
	}{
		{"empty", runtime.RunningConversations{ConversationIDs: []defs.ConversationID{}}, `{"conversation_ids":[]}`},
		{"running", runtime.RunningConversations{ConversationIDs: []defs.ConversationID{"one"}}, `{"conversation_ids":["one"]}`},
		{"closed", RunningConversationsClosedParams{Reason: "runtime has stopped"}, `{"reason":"runtime has stopped"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.value)
			if err != nil || string(got) != tt.want {
				t.Fatalf("JSON = %s, %v; want %s", got, err, tt.want)
			}
		})
	}
}

// TestTerminalWire 检查终端消息在线上的字段：输出数据是 base64，空数据也编码成空串，列表项省略空的对话。
func TestTerminalWire(t *testing.T) {
	// 1. 分别编码带数据的输出、第一页为空的输出、有对话和没有对话的列表项、列表和关闭通知。
	for _, tt := range []struct {
		name  string
		value any
		want  string
	}{
		{"output", TerminalOutputParams{TerminalID: "t1", Data: []byte("hi"), NextCursor: 2, Truncated: true, HasMore: true}, `{"terminal_id":"t1","data":"aGk=","next_cursor":2,"truncated":true,"has_more":true}`},
		{"empty_first_output", TerminalOutputParams{TerminalID: "t1", Data: []byte{}, NextCursor: 7}, `{"terminal_id":"t1","data":"","next_cursor":7,"truncated":false,"has_more":false}`},
		{"item_with_conversation", TerminalItem{TerminalID: "t1", ExecutorID: "local", Directory: "/work", StartedAt: time.Date(2026, 11, 6, 1, 2, 3, 0, time.UTC), ConversationID: "c1"}, `{"terminal_id":"t1","executor_id":"local","directory":"/work","started_at":"2026-11-06T01:02:03Z","conversation_id":"c1"}`},
		{"item_without_conversation", TerminalItem{TerminalID: "t1", ExecutorID: "local", Directory: "/work", StartedAt: time.Date(2026, 11, 6, 1, 2, 3, 0, time.UTC)}, `{"terminal_id":"t1","executor_id":"local","directory":"/work","started_at":"2026-11-06T01:02:03Z"}`},
		{"empty_list", TerminalListResult{Terminals: []TerminalItem{}}, `{"terminals":[]}`},
		{"closed", TerminalsClosedParams{Reason: "runtime has stopped"}, `{"reason":"runtime has stopped"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.value)
			if err != nil || string(got) != tt.want {
				t.Fatalf("JSON = %s, %v; want %s", got, err, tt.want)
			}
		})
	}
}
