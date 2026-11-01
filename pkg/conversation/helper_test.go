package conversation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
)

// TestLogConversationStopped 检查正常停止、ctx 取消和其他错误对应的日志级别和消息。
func TestLogConversationStopped(t *testing.T) {
	// 1. 列出正常、取消和异常停止的预期日志。
	tests := []struct {
		name    string
		err     error
		wantMsg string
		wantLvl string
	}{
		{
			name:    "nil_is_normal_stop",
			err:     nil,
			wantMsg: "conversation stopped",
			wantLvl: "INFO",
		},
		{
			name:    "canceled_is_normal_stop",
			err:     fmt.Errorf("root Session exited: %w", context.Canceled),
			wantMsg: "conversation stopped",
			wantLvl: "INFO",
		},
		{
			name:    "other_error_is_abnormal_stop",
			err:     errors.New("disk full"),
			wantMsg: "conversation stopped with error",
			wantLvl: "ERROR",
		},
	}
	// 2. 写入并解析日志，检查消息和级别。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buffer bytes.Buffer
			logConversationStopped(slog.New(slog.NewJSONHandler(&buffer, nil)), tt.err)
			var record map[string]any
			if err := json.Unmarshal(buffer.Bytes(), &record); err != nil {
				t.Fatalf("log output %q: %v", buffer.String(), err)
			}
			if record["msg"] != tt.wantMsg || record["level"] != tt.wantLvl {
				t.Fatalf("log = %v, want %q at %s", record, tt.wantMsg, tt.wantLvl)
			}
		})
	}
}

// TestEncodeToolResult 检查协作结果的编码：返回值不转义 HTML，操作或编码失败时填 collaboration_error。
func TestEncodeToolResult(t *testing.T) {
	// 1. 列出返回值编码、操作错误和编码错误。
	tests := []struct {
		name        string
		payload     any
		err         error
		wantContent string
		wantCode    string
	}{
		{
			name:        "payload_encoded",
			payload:     map[string]string{"task_name": "review"},
			wantContent: `{"task_name":"review"}`,
		},
		{
			name:        "html_characters_stay_literal",
			payload:     map[string]string{"text": "<a&b>"},
			wantContent: `{"text":"<a&b>"}`,
		},
		{
			name:        "operation_error",
			err:         errors.New("session not found"),
			wantContent: `null`,
			wantCode:    "collaboration_error",
		},
		{
			name:        "unencodable_payload",
			payload:     make(chan int),
			wantContent: ``,
			wantCode:    "collaboration_error",
		},
	}
	// 2. 编码工具结果，检查 CallID、正文和错误码。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := encodeToolResult("call-1", tt.payload, tt.err).Result
			if result.CallID != "call-1" || string(result.Content) != tt.wantContent {
				t.Fatalf("result = %s, want call-1 / %s", result.Content, tt.wantContent)
			}
			gotCode := ""
			if result.Error != nil {
				gotCode = result.Error.Code
			}
			if gotCode != tt.wantCode {
				t.Fatalf("error code = %q, want %q", gotCode, tt.wantCode)
			}
		})
	}
}

// TestTransformHistoryItems 检查历史副本去掉了内嵌图片和加密推理，原 Item 不变。
func TestTransformHistoryItems(t *testing.T) {
	// 1. 准备消息图片、工具图片和加密推理样本，保留图片 Alt。
	const dataURL = "data:image/png;base64,AAAA"
	tests := []struct {
		name string
		item *defs.Item
		want *defs.Item
	}{
		{
			name: "user_message_image",
			item: &defs.Item{
				ID:   "m1",
				Kind: defs.ItemMessage,
				Message: &defs.Message{Content: []*defs.Content{
					{
						Kind: defs.ContentText,
						Text: &defs.TextContent{Text: "看这张"},
					},
					{
						Kind: defs.ContentImage,
						Image: &defs.ImageContent{
							DataURL: dataURL,
							Alt:     "shot.png",
						},
					},
				}},
			},
			want: &defs.Item{
				ID:   "m1",
				Kind: defs.ItemMessage,
				Message: &defs.Message{Content: []*defs.Content{
					{
						Kind: defs.ContentText,
						Text: &defs.TextContent{Text: "看这张"},
					},
					{
						Kind:  defs.ContentImage,
						Image: &defs.ImageContent{Alt: "shot.png"},
					},
				}},
			},
		},
		{
			name: "tool_result_images",
			item: &defs.Item{
				ID:   "r1",
				Kind: defs.ItemToolResult,
				ToolResult: &defs.ToolResult{
					CallID:  "call-1",
					Content: json.RawMessage(`{"path":"a.png"}`),
					Images: []*defs.ImageContent{{
						DataURL: dataURL,
						Alt:     "a.png",
					}, {DataURL: dataURL}},
				},
			},
			want: &defs.Item{
				ID:   "r1",
				Kind: defs.ItemToolResult,
				ToolResult: &defs.ToolResult{
					CallID:  "call-1",
					Content: json.RawMessage(`{"path":"a.png"}`),
					Images:  []*defs.ImageContent{{Alt: "a.png"}, {}},
				},
			},
		},
		{
			name: "tool_result_without_images",
			item: &defs.Item{
				ID:   "r2",
				Kind: defs.ItemToolResult,
				ToolResult: &defs.ToolResult{
					CallID:  "call-2",
					Content: json.RawMessage(`"ok"`),
				},
			},
			want: &defs.Item{
				ID:   "r2",
				Kind: defs.ItemToolResult,
				ToolResult: &defs.ToolResult{
					CallID:  "call-2",
					Content: json.RawMessage(`"ok"`),
				},
			},
		},
		{
			name: "reasoning_secrets",
			item: &defs.Item{
				ID:   "t1",
				Kind: defs.ItemReasoning,
				Reasoning: &defs.Reasoning{
					Summary:   []string{"想"},
					Encrypted: "secret",
					RawJSON:   json.RawMessage(`{}`),
				},
			},
			want: &defs.Item{
				ID:        "t1",
				Kind:      defs.ItemReasoning,
				Reasoning: &defs.Reasoning{Summary: []string{"想"}},
			},
		},
	}
	// 2. 转换历史并核对返回内容，同时检查原 Item 未改变。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := tt.item.Clone()
			got := transformHistoryItems([]dao.LoggedItem{{Item: tt.item}})
			if len(got) != 1 || !reflect.DeepEqual(got[0], tt.want) {
				t.Fatalf("transformHistoryItems() = %+v, want %+v", got, tt.want)
			}
			if !reflect.DeepEqual(tt.item, original) {
				t.Fatalf("transformHistoryItems changed the logged item: %+v, want %+v", tt.item, original)
			}
		})
	}
}

// TestSelectForkHistorySkipsReasoningAndChanges 检查继承历史时按轮数选取，并跳过推理、模型切换和 Executor 变化。
func TestSelectForkHistorySkipsReasoningAndChanges(t *testing.T) {
	// 1. 准备含推理、模型切换和 Executor 变化的两轮历史。
	history := []*defs.Item{
		{
			ID:     "u1",
			TurnID: "T1",
			Kind:   defs.ItemMessage,
		},
		{
			ID:          "r1",
			TurnID:      "T1",
			ModelCallID: "m1",
			Kind:        defs.ItemReasoning,
		},
		{
			ID:          "a1",
			TurnID:      "T1",
			ModelCallID: "m1",
			Kind:        defs.ItemMessage,
		},
		{
			ID:     "c2",
			TurnID: "T2",
			Kind:   defs.ItemModelChange,
		},
		{
			ID:     "e2",
			TurnID: "T2",
			Kind:   defs.ItemExecutorsChange,
		},
		{
			ID:     "u2",
			TurnID: "T2",
			Kind:   defs.ItemMessage,
		},
		{
			ID:          "r2",
			TurnID:      "T2",
			ModelCallID: "m2",
			Kind:        defs.ItemReasoning,
		},
		{
			ID:          "a2",
			TurnID:      "T2",
			ModelCallID: "m2",
			Kind:        defs.ItemMessage,
		},
	}
	tests := []struct {
		name  string
		limit int
		want  []defs.ItemID
	}{
		{
			name:  "all_turns",
			limit: 0,
			want:  []defs.ItemID{"u1", "a1", "u2", "a2"},
		},
		{
			name:  "latest_turn",
			limit: 1,
			want:  []defs.ItemID{"u2", "a2"},
		},
		{
			name:  "more_than_available",
			limit: 5,
			want:  []defs.ItemID{"u1", "a1", "u2", "a2"},
		},
	}
	// 2. 按不同轮数继承，检查只保留选定轮次的消息。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []defs.ItemID
			for _, item := range selectForkHistory(history, tt.limit) {
				got = append(got, item.ID)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("selectForkHistory() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestGetUsedMCPServers 检查各 Agent 的 MCP 名单合并结果，以及结果保持 server 定义顺序。
func TestGetUsedMCPServers(t *testing.T) {
	// 1. 准备不同 Agent 名单及预期 server 顺序。
	servers := []*mcpclient.ServerDefinition{{ID: "c"}, {ID: "a"}, {ID: "b"}}
	tests := []struct {
		name   string
		agents []*agent.AgentDefinition
		want   []string
	}{
		{
			name: "no_agents",
			want: nil,
		},
		{
			name:   "agent_without_list_uses_all",
			agents: []*agent.AgentDefinition{{MCPServerIDs: defs.OptionalStringList{}}, {}},
			want:   []string{"c", "a", "b"},
		},
		{
			name:   "empty_list_uses_none",
			agents: []*agent.AgentDefinition{{MCPServerIDs: defs.OptionalStringList{}}},
			want:   nil,
		},
		{
			name:   "union_keeps_server_order",
			agents: []*agent.AgentDefinition{{MCPServerIDs: defs.OptionalStringList{"b"}}, {MCPServerIDs: defs.OptionalStringList{"c", "missing"}}},
			want:   []string{"c", "b"},
		},
	}
	// 2. 合并名单，检查空名单、不限制和并集的结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, server := range getUsedMCPServers(tt.agents, servers) {
				got = append(got, server.ID)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("getUsedMCPServers() = %v, want %v", got, tt.want)
			}
		})
	}
}
