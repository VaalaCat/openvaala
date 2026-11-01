package runtimeprotocol

import (
	"encoding/json"
	"slices"
	"testing"

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
				"terminal/output", "terminal/exited",
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
