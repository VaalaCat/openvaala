package defs_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/defs"
)

// TestItemLLMKitConversion 检查各类 Item 的模型消息往返转换。
func TestItemLLMKitConversion(t *testing.T) {
	// 1. 准备执行身份、地址、消息、推理、调用和结果样例。
	const testTurnID defs.TurnID = "turn-1"
	const testModelCallID defs.ModelCallID = "model-call-1"
	from := &defs.Address{
		Kind: defs.IdentityAgent,
		ID:   "writer",
	}
	to := &defs.Address{
		Kind: defs.IdentityUser,
		ID:   "alice",
	}
	tests := []struct {
		name         string
		item         *defs.Item
		modelMessage *llmkit.Message
	}{
		{
			name: "message",
			item: &defs.Item{
				ID:   defs.ItemID("item-message"),
				Kind: defs.ItemMessage,
				Message: &defs.Message{
					From: from,
					To:   to,
					Content: []*defs.Content{
						{
							Kind: defs.ContentText,
							Text: &defs.TextContent{Text: "hello"},
						},
					},
				},
			},
			modelMessage: &llmkit.Message{
				Role: llmkit.RoleAssistant,
				Content: []llmkit.ContentBlock{
					{
						Type: llmkit.ContentTypeText,
						Text: "hello",
					},
				},
			},
		},
		{
			name: "reasoning",
			item: &defs.Item{
				ID:   defs.ItemID("item-reasoning"),
				Kind: defs.ItemReasoning,
				Reasoning: &defs.Reasoning{
					Status:    defs.ReasoningCompleted,
					Summary:   []string{"checked"},
					Encrypted: "opaque",
				},
			},
			modelMessage: &llmkit.Message{
				Role: llmkit.RoleAssistant,
				Content: []llmkit.ContentBlock{
					{
						Type: llmkit.ContentTypeThinking,
						Reasoning: &llmkit.ReasoningContent{
							Summary:   []string{"checked"},
							Encrypted: "opaque",
						},
					},
				},
			},
		},
		{
			name: "independent_tool_result",
			item: &defs.Item{
				ID:   defs.ItemID("item-tool-result"),
				Kind: defs.ItemToolResult,
				ToolResult: &defs.ToolResult{
					CallID:  "call-1",
					Content: json.RawMessage(`"hello"`),
				},
			},
			modelMessage: &llmkit.Message{
				Role:       llmkit.RoleTool,
				ToolCallID: "call-1",
				Content: []llmkit.ContentBlock{
					{
						Type: llmkit.ContentTypeText,
						Text: `{"content":"hello"}`,
					},
				},
			},
		},
		{
			name: "tool_call",
			item: &defs.Item{
				ID:   defs.ItemID("item-tool-call"),
				Kind: defs.ItemToolCall,
				ToolCall: &defs.ToolCall{
					CallID:    "call-1",
					Name:      "read_file",
					Arguments: `{"path":"a.txt"}`,
					Status:    defs.ToolCallReady,
				},
			},
			modelMessage: &llmkit.Message{
				Role: llmkit.RoleAssistant,
				ToolCalls: []llmkit.ToolCall{
					{
						ID:        "call-1",
						Name:      "read_file",
						Arguments: `{"path":"a.txt"}`,
					},
				},
			},
		},
	}
	// 2. 检查往返内容并保留 Item、Turn 和模型调用 ID。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				encoded, err := tt.item.ToLLMKit(tt.modelMessage.Role)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(encoded, tt.modelMessage) {
					t.Fatalf("ToLLMKit() = %#v, want %#v", encoded, tt.modelMessage)
				}
				decoded := &defs.Item{
					ID:          tt.item.ID,
					TurnID:      testTurnID,
					ModelCallID: testModelCallID,
				}
				if err := decoded.FromLLMKit(tt.modelMessage, from, to); err != nil {
					t.Fatal(err)
				}
				want := tt.item.Clone()
				want.TurnID = testTurnID
				want.ModelCallID = testModelCallID
				if !reflect.DeepEqual(decoded, want) {
					t.Fatalf("FromLLMKit() = %#v, want %#v", decoded, want)
				}
			})
	}
}

// TestItemFromLLMKitRejectsCompositeMessage 检查 Item 转换拒绝混合消息。
func TestItemFromLLMKitRejectsCompositeMessage(t *testing.T) {
	// 1. 准备同时包含文本和工具调用的模型消息。
	tests := []struct {
		name    string
		message *llmkit.Message
	}{
		{
			name: "text_with_tool_call",
			message: &llmkit.Message{
				Role: llmkit.RoleAssistant,
				Content: []llmkit.ContentBlock{
					{
						Type: llmkit.ContentTypeText,
						Text: "new",
					},
				},
				ToolCalls: []llmkit.ToolCall{
					{
						ID:   "call-1",
						Name: "read",
					},
				},
			},
		},
	}
	// 2. 检查转换报错且原 Item 不变。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				item := &defs.Item{
					ID:          defs.ItemID("item-preserved"),
					TurnID:      defs.TurnID("turn-preserved"),
					ModelCallID: defs.ModelCallID("model-call-preserved"),
					Kind:        defs.ItemMessage,
					Message: &defs.Message{
						Content: []*defs.Content{
							{
								Kind: defs.ContentText,
								Text: &defs.TextContent{Text: "keep"},
							},
						},
					},
				}
				want := item.Clone()
				if err := item.FromLLMKit(tt.message, &defs.Address{}, &defs.Address{}); err == nil {
					t.Fatal("FromLLMKit() accepted a composite message")
				}
				if !reflect.DeepEqual(item, want) {
					t.Fatal("failed conversion modified receiver")
				}
			})
	}
}

// TestItemClonePreservesIdentity 检查 Item 副本保留身份和保存时间。
func TestItemClonePreservesIdentity(t *testing.T) {
	// 1. 创建带执行身份的消息并检查副本全部字段。
	item := &defs.Item{
		ID:          defs.ItemID("item-1"),
		UpdatedAt:   42,
		TurnID:      defs.TurnID("turn-1"),
		ModelCallID: defs.ModelCallID("model-call-1"),
		Kind:        defs.ItemMessage,
		Message: &defs.Message{
			Content: []*defs.Content{
				{
					Kind: defs.ContentText,
					Text: &defs.TextContent{Text: "hello"},
				},
			},
		},
	}
	if got := item.Clone(); !reflect.DeepEqual(got, item) {
		t.Fatalf("Clone() = %#v, want %#v", got, item)
	}
}

// TestModelErrorLLMKitConversion 检查模型错误的覆盖与转换。
func TestModelErrorLLMKitConversion(t *testing.T) {
	// 1. 准备旧错误和模型返回的新错误。
	tests := []struct {
		name    string
		initial *defs.ModelError
		payload *llmkit.ErrorPayload
		want    *defs.ModelError
	}{
		{
			name: "replaces_previous_error",
			initial: &defs.ModelError{
				Code:    "transport",
				Message: "connection reset",
			},
			payload: &llmkit.ErrorPayload{
				Code:    "overloaded",
				Message: "try later",
			},
			want: &defs.ModelError{
				Code:    "overloaded",
				Message: "try later",
			},
		},
	}
	// 2. 检查读取覆盖及反向转换结果。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				got := tt.initial.Clone()
				got.FromLLMKit(tt.payload)
				if !reflect.DeepEqual(got, tt.want) {
					t.Fatalf("FromLLMKit() = %#v, want %#v", got, tt.want)
				}
				if encoded := got.ToLLMKit(); !reflect.DeepEqual(encoded, tt.payload) {
					t.Fatalf("ToLLMKit() = %#v, want %#v", encoded, tt.payload)
				}
			})
	}
}

// TestItemContextSummaryClone 检查摘要 Item 复制后不共享载荷。
func TestItemContextSummaryClone(t *testing.T) {
	// 1. 分别复制空载荷和非空摘要，修改副本后检查原摘要。
	for _, payload := range []*defs.ContextSummary{nil, {
		Text:                 "summary",
		CoveredThroughItemID: "B",
	}} {
		item := &defs.Item{
			ID:             "S",
			TurnID:         "turn",
			ModelCallID:    "compact",
			Kind:           defs.ItemContextSummary,
			ContextSummary: payload,
		}
		copy := item.Clone()
		if !reflect.DeepEqual(copy, item) {
			t.Fatalf("Clone = %+v, want %+v", copy, item)
		}
		if copy.ContextSummary != nil {
			copy.ContextSummary.Text = "changed"
			copy.ContextSummary.CoveredThroughItemID = "C"
			if item.ContextSummary.Text != "summary" || item.ContextSummary.CoveredThroughItemID != "B" {
				t.Fatal("Clone shares summary payload")
			}
		}
	}
}

// TestItemValidateModelChange 检查模型切换 Item 的字段校验。
func TestItemValidateModelChange(t *testing.T) {
	// 1. 准备前后模型及缺失字段、类型不符和相同模型样例。
	refA, refB := defs.ModelRef{
		ProviderID: "p",
		ModelID:    "a",
	}, defs.ModelRef{
		ProviderID: "p",
		ModelID:    "b",
	}
	tests := []struct {
		name    string
		item    *defs.Item
		wantErr bool
	}{
		{
			name: "valid",
			item: &defs.Item{
				ID:     "c",
				TurnID: "T",
				Kind:   defs.ItemModelChange,
				ModelChange: &defs.ModelChange{
					PreviousModel: refA,
					Model:         refB,
				},
			},
		},
		{
			name: "other_kind_without_payload",
			item: &defs.Item{
				ID:     "m",
				TurnID: "T",
				Kind:   defs.ItemMessage,
			},
		},
		{
			name: "payload_on_other_kind",
			item: &defs.Item{
				ID:     "m",
				TurnID: "T",
				Kind:   defs.ItemMessage,
				ModelChange: &defs.ModelChange{
					PreviousModel: refA,
					Model:         refB,
				},
			},
			wantErr: true,
		},
		{
			name: "missing_turn",
			item: &defs.Item{
				ID:   "c",
				Kind: defs.ItemModelChange,
				ModelChange: &defs.ModelChange{
					PreviousModel: refA,
					Model:         refB,
				},
			},
			wantErr: true,
		},
		{
			name: "with_model_call",
			item: &defs.Item{
				ID:          "c",
				TurnID:      "T",
				ModelCallID: "m",
				Kind:        defs.ItemModelChange,
				ModelChange: &defs.ModelChange{
					PreviousModel: refA,
					Model:         refB,
				},
			},
			wantErr: true,
		},
		{
			name: "missing_payload",
			item: &defs.Item{
				ID:     "c",
				TurnID: "T",
				Kind:   defs.ItemModelChange,
			},
			wantErr: true,
		},
		{
			name: "zero_previous",
			item: &defs.Item{
				ID:          "c",
				TurnID:      "T",
				Kind:        defs.ItemModelChange,
				ModelChange: &defs.ModelChange{Model: refB},
			},
			wantErr: true,
		},
		{
			name: "same_models",
			item: &defs.Item{
				ID:     "c",
				TurnID: "T",
				Kind:   defs.ItemModelChange,
				ModelChange: &defs.ModelChange{
					PreviousModel: refB,
					Model:         refB,
				},
			},
			wantErr: true,
		},
	}
	// 2. 检查各样例是否返回预期错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.item.ValidateModelChange(); (err != nil) != tt.wantErr {
				t.Fatalf("ValidateModelChange() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestItemValidateSessionFork 检查分叉标记的字段校验。
func TestItemValidateSessionFork(t *testing.T) {
	// 1. 准备 spawn_agent 分界、用户分叉接续标记、其他种类带或不带分叉载荷，以及缺载荷、缺来源、带 Turn、
	// 来源对话和结束序号不配套、结束序号为 1 的错误样例。
	tests := []struct {
		name    string
		item    *defs.Item
		wantErr bool
	}{
		{
			name: "spawn_boundary",
			item: &defs.Item{
				ID:          "f",
				Kind:        defs.ItemSessionFork,
				SessionFork: &defs.SessionFork{SourceSessionID: "s"},
			},
		},
		{
			name: "user_fork_continuation",
			item: &defs.Item{
				ID:   "f",
				Kind: defs.ItemSessionFork,
				SessionFork: &defs.SessionFork{
					SourceSessionID:      "s",
					SourceConversationID: "c",
					SourceEndOrdinal:     30,
				},
			},
		},
		{
			name: "other_kind_without_payload",
			item: &defs.Item{
				ID:     "m",
				TurnID: "T",
				Kind:   defs.ItemMessage,
			},
		},
		{
			name: "payload_on_other_kind",
			item: &defs.Item{
				ID:          "m",
				TurnID:      "T",
				Kind:        defs.ItemMessage,
				SessionFork: &defs.SessionFork{SourceSessionID: "s"},
			},
			wantErr: true,
		},
		{
			name: "missing_payload",
			item: &defs.Item{
				ID:   "f",
				Kind: defs.ItemSessionFork,
			},
			wantErr: true,
		},
		{
			name: "missing_source_session",
			item: &defs.Item{
				ID:          "f",
				Kind:        defs.ItemSessionFork,
				SessionFork: &defs.SessionFork{},
			},
			wantErr: true,
		},
		{
			name: "with_turn",
			item: &defs.Item{
				ID:          "f",
				TurnID:      "T",
				Kind:        defs.ItemSessionFork,
				SessionFork: &defs.SessionFork{SourceSessionID: "s"},
			},
			wantErr: true,
		},
		{
			name: "conversation_without_end_ordinal",
			item: &defs.Item{
				ID:   "f",
				Kind: defs.ItemSessionFork,
				SessionFork: &defs.SessionFork{
					SourceSessionID:      "s",
					SourceConversationID: "c",
				},
			},
			wantErr: true,
		},
		{
			name: "end_ordinal_without_conversation",
			item: &defs.Item{
				ID:   "f",
				Kind: defs.ItemSessionFork,
				SessionFork: &defs.SessionFork{
					SourceSessionID:  "s",
					SourceEndOrdinal: 30,
				},
			},
			wantErr: true,
		},
		{
			name: "end_ordinal_one",
			item: &defs.Item{
				ID:   "f",
				Kind: defs.ItemSessionFork,
				SessionFork: &defs.SessionFork{
					SourceSessionID:      "s",
					SourceConversationID: "c",
					SourceEndOrdinal:     1,
				},
			},
			wantErr: true,
		},
	}
	// 2. 检查各样例是否返回预期错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.item.ValidateSessionFork(); (err != nil) != tt.wantErr {
				t.Fatalf("ValidateSessionFork() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestItemValidateExecutorsChange 检查 Executor 切换 Item 的字段校验。
func TestItemValidateExecutorsChange(t *testing.T) {
	// 1. 准备前后 Executor 列表及身份、类型和空列表样例。
	previous := []defs.TurnExecutor{{
		ID:                 "local",
		WorkspaceDirectory: "/a",
	}}
	current := []defs.TurnExecutor{{
		ID:                 "gpu",
		WorkspaceDirectory: "/data",
	}, {
		ID:                 "local",
		WorkspaceDirectory: "/a",
	}}
	tests := []struct {
		name    string
		item    *defs.Item
		wantErr bool
	}{
		{
			name: "valid",
			item: &defs.Item{
				ID:     "c",
				TurnID: "T",
				Kind:   defs.ItemExecutorsChange,
				ExecutorsChange: &defs.ExecutorsChange{
					PreviousExecutors: previous,
					Executors:         current,
				},
			},
		},
		{
			name: "valid_with_empty_current",
			item: &defs.Item{
				ID:              "c",
				TurnID:          "T",
				Kind:            defs.ItemExecutorsChange,
				ExecutorsChange: &defs.ExecutorsChange{PreviousExecutors: previous},
			},
		},
		{
			name: "other_kind_without_payload",
			item: &defs.Item{
				ID:     "m",
				TurnID: "T",
				Kind:   defs.ItemMessage,
			},
		},
		{
			name: "payload_on_other_kind",
			item: &defs.Item{
				ID:     "m",
				TurnID: "T",
				Kind:   defs.ItemMessage,
				ExecutorsChange: &defs.ExecutorsChange{
					PreviousExecutors: previous,
					Executors:         current,
				},
			},
			wantErr: true,
		},
		{
			name: "missing_turn",
			item: &defs.Item{
				ID:   "c",
				Kind: defs.ItemExecutorsChange,
				ExecutorsChange: &defs.ExecutorsChange{
					PreviousExecutors: previous,
					Executors:         current,
				},
			},
			wantErr: true,
		},
		{
			name: "with_model_call",
			item: &defs.Item{
				ID:          "c",
				TurnID:      "T",
				ModelCallID: "m",
				Kind:        defs.ItemExecutorsChange,
				ExecutorsChange: &defs.ExecutorsChange{
					PreviousExecutors: previous,
					Executors:         current,
				},
			},
			wantErr: true,
		},
		{
			name: "missing_payload",
			item: &defs.Item{
				ID:     "c",
				TurnID: "T",
				Kind:   defs.ItemExecutorsChange,
			},
			wantErr: true,
		},
		{
			name: "empty_previous",
			item: &defs.Item{
				ID:              "c",
				TurnID:          "T",
				Kind:            defs.ItemExecutorsChange,
				ExecutorsChange: &defs.ExecutorsChange{Executors: current},
			},
			wantErr: true,
		},
		{
			name: "same_executors",
			item: &defs.Item{
				ID:     "c",
				TurnID: "T",
				Kind:   defs.ItemExecutorsChange,
				ExecutorsChange: &defs.ExecutorsChange{
					PreviousExecutors: previous,
					Executors:         previous,
				},
			},
			wantErr: true,
		},
		{
			name: "directory_differs",
			item: &defs.Item{
				ID:     "c",
				TurnID: "T",
				Kind:   defs.ItemExecutorsChange,
				ExecutorsChange: &defs.ExecutorsChange{
					PreviousExecutors: previous,
					Executors: []defs.TurnExecutor{{
						ID:                 "local",
						WorkspaceDirectory: "/b",
					}},
				},
			},
		},
	}
	// 2. 检查各样例是否返回预期错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.item.ValidateExecutorsChange(); (err != nil) != tt.wantErr {
				t.Fatalf("ValidateExecutorsChange() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestItemExecutorsChangeClone 检查 Executor 切换的复制。
func TestItemExecutorsChangeClone(t *testing.T) {
	// 1. 复制切换记录，修改副本后检查原列表。
	item := &defs.Item{
		ID:     "c",
		TurnID: "T",
		Kind:   defs.ItemExecutorsChange,
		ExecutorsChange: &defs.ExecutorsChange{
			PreviousExecutors: []defs.TurnExecutor{{
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}},
			Executors: []defs.TurnExecutor{{
				ID:                 "gpu",
				WorkspaceDirectory: "/data",
			}},
		},
	}
	cloned := item.Clone()
	if !reflect.DeepEqual(cloned, item) || cloned.ExecutorsChange == item.ExecutorsChange {
		t.Fatalf("Clone() = %+v, want an independent copy", cloned)
	}
	cloned.ExecutorsChange.Executors[0].ID = "other"
	if item.ExecutorsChange.Executors[0].ID != "gpu" {
		t.Fatalf("Clone() shares the executors slice with the original")
	}
}

// TestExecutorsChangeGetDetails 检查 Executor 变化明细。
func TestExecutorsChangeGetDetails(t *testing.T) {
	// 1. 准备默认切换、新增、移除和目录变化样例。
	local := defs.TurnExecutor{
		ID:                 "local",
		WorkspaceDirectory: "/a",
	}
	gpu := defs.TurnExecutor{
		ID:                 "gpu",
		WorkspaceDirectory: "/data",
	}
	sandbox := defs.TurnExecutor{
		ID:                 "sandbox",
		WorkspaceDirectory: "/workspace",
	}
	movedGPU := defs.TurnExecutor{
		ID:                 "gpu",
		WorkspaceDirectory: "/data/proj",
	}
	tests := []struct {
		name     string
		previous []defs.TurnExecutor
		current  []defs.TurnExecutor
		want     defs.ExecutorsChangeDetails
	}{
		{
			name:     "new_default_is_not_added",
			previous: []defs.TurnExecutor{local},
			current:  []defs.TurnExecutor{gpu, local},
			want: defs.ExecutorsChangeDetails{
				NewDefault:        &gpu,
				PreviousDefaultID: "local",
			},
		},
		{
			name:     "added_after_default",
			previous: []defs.TurnExecutor{local},
			current:  []defs.TurnExecutor{local, sandbox},
			want:     defs.ExecutorsChangeDetails{Added: []defs.TurnExecutor{sandbox}},
		},
		{
			name:     "removed_default_falls_back",
			previous: []defs.TurnExecutor{gpu, local},
			current:  []defs.TurnExecutor{local},
			want: defs.ExecutorsChangeDetails{
				NewDefault:        &local,
				PreviousDefaultID: "gpu",
				Removed:           []defs.TurnExecutor{gpu},
			},
		},
		{
			name:     "all_removed",
			previous: []defs.TurnExecutor{local},
			want:     defs.ExecutorsChangeDetails{Removed: []defs.TurnExecutor{local}},
		},
		{
			name:     "directory_changed",
			previous: []defs.TurnExecutor{local},
			current: []defs.TurnExecutor{{
				ID:                 "local",
				WorkspaceDirectory: "/b",
			}},
			want: defs.ExecutorsChangeDetails{DirectoryChanges: []defs.ExecutorDirectoryChange{{
				ID:                "local",
				PreviousDirectory: "/a",
				Directory:         "/b",
			}}},
		},
		{
			name:     "new_default_directory_is_not_changed",
			previous: []defs.TurnExecutor{local, gpu},
			current:  []defs.TurnExecutor{movedGPU, local},
			want: defs.ExecutorsChangeDetails{
				NewDefault:        &movedGPU,
				PreviousDefaultID: "local",
			},
		},
	}
	// 2. 检查各类变化及新默认不重复列出。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			change := &defs.ExecutorsChange{
				PreviousExecutors: tt.previous,
				Executors:         tt.current,
			}
			if got := change.GetDetails(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("GetDetails() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestItemValidateInputMessage 检查 Session 输入消息的字段校验。
func TestItemValidateInputMessage(t *testing.T) {
	// 1. 准备普通文字、附带材料、图片、通知及缺失载荷样例。
	text := &defs.Content{
		Kind: defs.ContentText,
		Text: &defs.TextContent{Text: "你好"},
	}
	newMessage := func(content ...*defs.Content) *defs.Item {
		return &defs.Item{
			ID:   "i",
			Kind: defs.ItemMessage,
			Message: &defs.Message{
				From:    &defs.Address{Kind: defs.IdentityUser},
				Content: content,
			},
		}
	}
	notification := newMessage()
	notification.Message.SubagentNotification = &defs.SubagentNotification{
		SourceTurnID: "t",
		Status:       defs.TurnStatusCompleted,
	}
	tests := []struct {
		name    string
		item    *defs.Item
		wantErr bool
	}{
		{
			name: "text",
			item: newMessage(text),
		},
		{
			name: "context_text",
			item: newMessage(&defs.Content{
				Kind: defs.ContentText,
				Text: &defs.TextContent{
					Text:      "附带材料",
					IsContext: true,
				},
			}),
		},
		{
			name: "text_and_image",
			item: newMessage(text, &defs.Content{
				Kind:  defs.ContentImage,
				Image: &defs.ImageContent{DataURL: "data:image/png;base64,iVBORw0KGgo="},
			}),
		},
		{
			name: "subagent_notification_may_be_empty",
			item: notification,
		},
		{
			name:    "empty_content",
			item:    newMessage(),
			wantErr: true,
		},
		{
			name: "nil_content",
			item: &defs.Item{
				ID:      "i",
				Kind:    defs.ItemMessage,
				Message: &defs.Message{},
			},
			wantErr: true,
		},
		{
			name:    "nil_entry",
			item:    newMessage(nil),
			wantErr: true,
		},
		{
			name:    "valid_entry_then_nil_entry",
			item:    newMessage(text, nil),
			wantErr: true,
		},
		{
			name:    "text_without_payload",
			item:    newMessage(&defs.Content{Kind: defs.ContentText}),
			wantErr: true,
		},
		{
			name:    "image_without_payload",
			item:    newMessage(&defs.Content{Kind: defs.ContentImage}),
			wantErr: true,
		},
		{
			name: "image_without_source",
			item: newMessage(&defs.Content{
				Kind:  defs.ContentImage,
				Image: &defs.ImageContent{},
			}),
			wantErr: true,
		},
		{
			name: "unknown_kind",
			item: newMessage(&defs.Content{
				Kind: "audio",
				Text: &defs.TextContent{Text: "x"},
			}),
			wantErr: true,
		},
		{
			name:    "empty_kind",
			item:    newMessage(&defs.Content{Text: &defs.TextContent{Text: "x"}}),
			wantErr: true,
		},
		{
			name: "not_a_message",
			item: &defs.Item{
				ID:   "i",
				Kind: defs.ItemReasoning,
			},
			wantErr: true,
		},
		{
			name: "message_without_payload",
			item: &defs.Item{
				ID:   "i",
				Kind: defs.ItemMessage,
			},
			wantErr: true,
		},
	}
	// 2. 检查各输入是否返回预期错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.item.ValidateInputMessage(); (err != nil) != tt.wantErr {
				t.Fatalf("ValidateInputMessage() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestItemModelChangeClone 检查模型切换的复制。
func TestItemModelChangeClone(t *testing.T) {
	// 1. 创建切换记录，检查副本独立。
	item := &defs.Item{
		ID:     "c",
		TurnID: "T",
		Kind:   defs.ItemModelChange,
		ModelChange: &defs.ModelChange{
			PreviousModel: defs.ModelRef{
				ProviderID: "p",
				ModelID:    "a",
			},
			Model: defs.ModelRef{
				ProviderID: "p",
				ModelID:    "b",
			},
		},
	}
	cloned := item.Clone()
	if !reflect.DeepEqual(cloned, item) || cloned.ModelChange == item.ModelChange {
		t.Fatalf("Clone() = %+v, want an independent copy", cloned)
	}
}
