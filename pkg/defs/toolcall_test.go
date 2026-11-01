package defs_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/defs"
)

// TestToolCallLLMKitConversion 检查工具调用的模型转换。
func TestToolCallLLMKitConversion(t *testing.T) {
	// 1. 准备已就绪和已中断的调用状态。
	tests := []struct {
		name   string
		status defs.ToolCallStatus
	}{
		{
			name:   "ready",
			status: defs.ToolCallReady,
		},
		{
			name:   "interrupted",
			status: defs.ToolCallInterrupted,
		},
	}
	// 2. 检查字段往返及读取后状态为 ready。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				modelCall := &llmkit.ToolCall{
					ID:        "call-1",
					Name:      "read_file",
					Arguments: `{"path":"private.txt"}`,
				}
				call := &defs.ToolCall{
					CallID:    "call-1",
					Name:      "read_file",
					Arguments: `{"path":"private.txt"}`,
					Status:    tt.status,
				}
				if got := call.ToLLMKit(); !reflect.DeepEqual(got, modelCall) {
					t.Fatalf("ToLLMKit() = %#v, want %#v", got, modelCall)
				}
				want := call.Clone()
				want.Status = defs.ToolCallReady
				call.FromLLMKit(modelCall)
				if !reflect.DeepEqual(call, want) {
					t.Fatalf("FromLLMKit() = %#v, want %#v", call, want)
				}
			})
	}
}

// TestToolCallJSONStatus 检查工具调用状态的 JSON 编码。
func TestToolCallJSONStatus(t *testing.T) {
	// 1. 准备已就绪、已中断和参数生成中的调用。
	tests := []struct {
		name   string
		status defs.ToolCallStatus
		want   string
	}{
		{
			name:   "ready_call",
			status: defs.ToolCallReady,
			want:   `{"call_id":"call-1","name":"read_file","arguments":"{}","status":"ready"}`,
		},
		{
			name:   "interrupted_call",
			status: defs.ToolCallInterrupted,
			want:   `{"call_id":"call-1","name":"read_file","arguments":"{}","status":"interrupted"}`,
		},
		{
			name: "call_still_being_generated",
			want: `{"call_id":"call-1","name":"read_file","arguments":"{}"}`,
		},
	}
	// 2. 检查状态字段及空状态省略。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := json.Marshal(&defs.ToolCall{
				CallID:    "call-1",
				Name:      "read_file",
				Arguments: `{}`,
				Status:    tt.status,
			})
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != tt.want {
				t.Fatalf("ToolCall JSON = %s, want %s", encoded, tt.want)
			}
		})
	}
}

// TestToolResultLLMKitConversion 检查工具结果消息的往返转换。
func TestToolResultLLMKitConversion(t *testing.T) {
	// 1. 准备成功、错误、截断和缺失调用 ID 的结果。
	tests := []struct {
		name     string
		result   *defs.ToolResult
		wantBody string
		wantErr  bool
	}{
		{
			name: "success",
			result: &defs.ToolResult{
				CallID:  "call-1",
				Content: json.RawMessage(`"hello"`),
			},
			wantBody: `{"content":"hello"}`,
		},
		{
			name: "error_with_truncation",
			result: &defs.ToolResult{
				CallID:  "call-1",
				Content: json.RawMessage(`null`),
				Error: &defs.ToolError{
					Code:    "permission_denied",
					Message: "access denied",
				},
				Truncated: true,
			},
			wantBody: `{"content":null,"error":{"code":"permission_denied","message":"access denied"},"truncated":true}`,
		},
		{
			name:    "missing_call_id",
			result:  &defs.ToolResult{Content: json.RawMessage(`"hello"`)},
			wantErr: true,
		},
	}
	// 2. 检查模型正文、关联 ID、错误和原结果不变。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				before := tt.result.Clone()
				encoded, err := tt.result.ToLLMKit()
				if !reflect.DeepEqual(tt.result, before) {
					t.Fatal("ToLLMKit modified the tool result")
				}
				if (err != nil) != tt.wantErr {
					t.Fatalf("ToLLMKit() error = %v, wantErr %v", err, tt.wantErr)
				}
				if tt.wantErr {
					return
				}
				want := &llmkit.Message{
					Role:       llmkit.RoleTool,
					ToolCallID: tt.result.CallID,
					Content: []llmkit.ContentBlock{
						{
							Type: llmkit.ContentTypeText,
							Text: tt.wantBody,
						},
					},
				}
				if !reflect.DeepEqual(encoded, want) {
					t.Fatalf("ToLLMKit() = %#v, want %#v", encoded, want)
				}
				decoded := &defs.ToolResult{}
				if err := decoded.FromLLMKit(want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(decoded, tt.result) {
					t.Fatalf("FromLLMKit() = %#v, want %#v", decoded, tt.result)
				}
			})
	}
}

// TestToolResultGetModelContent 检查发给模型的工具结果正文。
func TestToolResultGetModelContent(t *testing.T) {
	// 1. 准备成功、错误、图片、Executor 和 HTML 字符样例。
	tests := []struct {
		name   string
		result *defs.ToolResult
		want   string
	}{
		{
			name: "success_without_call_id",
			result: &defs.ToolResult{
				CallID:  "call-1",
				Content: json.RawMessage(`{"ok":true}`),
			},
			want: `{"content":{"ok":true}}`,
		},
		{
			name: "error_with_truncation",
			result: &defs.ToolResult{
				CallID:  "call-1",
				Content: json.RawMessage(`null`),
				Error: &defs.ToolError{
					Code:    "permission_denied",
					Message: "access denied",
				},
				Truncated: true,
			},
			want: `{"content":null,"error":{"code":"permission_denied","message":"access denied"},"truncated":true}`,
		},
		{
			name:   "empty_result",
			result: &defs.ToolResult{},
			want:   `{}`,
		},
		{
			name: "images_not_in_model_content",
			result: &defs.ToolResult{
				CallID:  "call-1",
				Content: json.RawMessage(`{"path":"a.png"}`),
				Images: []*defs.ImageContent{{
					DataURL: "data:image/png;base64,AAAA",
					Alt:     "a.png",
				}},
			},
			want: `{"content":{"path":"a.png"}}`,
		},
		{
			name: "executor_not_in_model_content",
			result: &defs.ToolResult{
				CallID:   "call-1",
				Executor: "gpu",
				Content:  json.RawMessage(`"ok"`),
			},
			want: `{"content":"ok"}`,
		},
		{
			name: "html_characters_stay_literal",
			result: &defs.ToolResult{
				CallID:  "call-1",
				Content: json.RawMessage(`{"output":"<a&b>"}`),
				Error: &defs.ToolError{
					Code:    "failed",
					Message: "x<y&z>",
				},
			},
			want: `{"content":{"output":"<a&b>"},"error":{"code":"failed","message":"x<y&z>"}}`,
		},
	}
	// 2. 检查 JSON 正文及原结果不变。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := tt.result.Clone()
			got, err := tt.result.GetModelContent()
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("GetModelContent() = %s, want %s", got, tt.want)
			}
			if !reflect.DeepEqual(tt.result, before) {
				t.Error("GetModelContent modified the tool result")
			}
		})
	}
}

// TestToolResultClone 检查工具结果图片的复制。
func TestToolResultClone(t *testing.T) {
	// 1. 准备带图片和不带图片的结果。
	tests := []struct {
		name   string
		result *defs.ToolResult
	}{
		{
			name: "without_images",
			result: &defs.ToolResult{
				CallID:  "call-1",
				Content: json.RawMessage(`"ok"`),
			},
		},
		{
			name: "images_are_deep_copied",
			result: &defs.ToolResult{
				CallID:  "call-1",
				Content: json.RawMessage(`{"path":"a.png"}`),
				Images: []*defs.ImageContent{{
					DataURL: "data:image/png;base64,AAAA",
					Alt:     "a.png",
				}},
			},
		},
	}
	// 2. 修改原图片，检查副本未改变。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cloned := tt.result.Clone()
			if !reflect.DeepEqual(cloned, tt.result) {
				t.Fatalf("Clone() = %#v, want %#v", cloned, tt.result)
			}
			for index := range tt.result.Images {
				tt.result.Images[index].DataURL = "changed"
				tt.result.Images[index] = nil
			}
			for _, image := range cloned.Images {
				if image == nil || image.DataURL == "changed" {
					t.Fatalf("Clone shares Images with the original: %#v", cloned.Images)
				}
			}
		})
	}
}

// TestToolResultFromLLMKitRejectsInvalidIdentity 检查工具结果拒绝错误的调用关联。
func TestToolResultFromLLMKitRejectsInvalidIdentity(t *testing.T) {
	// 1. 准备消息缺失调用 ID 或正文携带调用 ID 的样例。
	tests := []struct {
		name    string
		message *llmkit.Message
	}{
		{
			name: "missing_message_call_id",
			message: &llmkit.Message{
				Role: llmkit.RoleTool,
				Content: []llmkit.ContentBlock{
					{
						Type: llmkit.ContentTypeText,
						Text: `{"content":"hello"}`,
					},
				},
			},
		},
		{
			name: "call_id_in_body",
			message: &llmkit.Message{
				Role:       llmkit.RoleTool,
				ToolCallID: "call-2",
				Content: []llmkit.ContentBlock{
					{
						Type: llmkit.ContentTypeText,
						Text: `{"call_id":"call-3","content":"hello"}`,
					},
				},
			},
		},
	}
	// 2. 检查读取失败且保留原结果。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				result := &defs.ToolResult{
					CallID:  "call-1",
					Content: json.RawMessage(`"hello"`),
				}
				want := &defs.ToolResult{
					CallID:  "call-1",
					Content: json.RawMessage(`"hello"`),
				}
				if err := result.FromLLMKit(tt.message); err == nil {
					t.Fatal("FromLLMKit() accepted invalid identity")
				}
				if !reflect.DeepEqual(result, want) {
					t.Fatalf("failed conversion modified receiver: %#v", result)
				}
			})
	}
}
