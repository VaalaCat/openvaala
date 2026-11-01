package defs_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/defs"
)

// TestMessageLLMKitConversion 检查消息与模型消息的往返转换和副本。
func TestMessageLLMKitConversion(t *testing.T) {
	// 1. 准备发送、接收地址及文本、图片消息。
	from := &defs.Address{
		Kind: defs.IdentityAgent,
		ID:   "reviewer",
	}
	to := &defs.Address{
		Kind: defs.IdentityAgent,
		ID:   "writer",
	}
	tests := []struct {
		name         string
		message      *defs.Message
		modelMessage *llmkit.Message
	}{
		{
			name: "text_and_image_with_explicit_perspective",
			message: &defs.Message{
				From: from,
				To:   to,
				Content: []*defs.Content{
					{
						Kind: defs.ContentText,
						Text: &defs.TextContent{Text: "Review this diagram"},
					},
					{
						Kind: defs.ContentImage,
						Image: &defs.ImageContent{
							DataURL: "data:image/png;base64,aGVsbG8=",
							Alt:     "architecture",
						},
					},
				},
			},
			modelMessage: &llmkit.Message{
				Role: llmkit.RoleUser,
				Content: []llmkit.ContentBlock{
					{
						Type: llmkit.ContentTypeText,
						Text: "Review this diagram",
					},
					{
						Type:     llmkit.ContentTypeImage,
						MimeType: "image/png",
						MediaB64: "aGVsbG8=",
						Metadata: map[string]any{"alt": "architecture"},
					},
				},
			},
		},
		{
			name: "http_image_keeps_media_url",
			message: &defs.Message{
				From: from,
				To:   to,
				Content: []*defs.Content{
					{
						Kind: defs.ContentText,
						Text: &defs.TextContent{Text: "Review this diagram"},
					},
					{
						Kind:  defs.ContentImage,
						Image: &defs.ImageContent{DataURL: "https://example.com/a.png"},
					},
				},
			},
			modelMessage: &llmkit.Message{
				Role: llmkit.RoleUser,
				Content: []llmkit.ContentBlock{
					{
						Type: llmkit.ContentTypeText,
						Text: "Review this diagram",
					},
					{
						Type:     llmkit.ContentTypeImage,
						MediaURL: "https://example.com/a.png",
					},
				},
			},
		},
	}
	// 2. 转换并回读，检查角色、内容和地址不共享。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				encoded, err := tt.message.ToLLMKit(tt.modelMessage.Role)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(encoded, tt.modelMessage) {
					t.Fatalf("ToLLMKit() = %#v, want %#v", encoded, tt.modelMessage)
				}
				decoded := &defs.Message{}
				if err := decoded.FromLLMKit(encoded, from, to); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(decoded, tt.message) {
					t.Fatalf("FromLLMKit() = %#v, want %#v", decoded, tt.message)
				}
				decoded.From.ID = "changed"
				decoded.To.ID = "changed"
				if from.ID != "reviewer" || to.ID != "writer" {
					t.Fatal("conversion aliases addresses")
				}
				decoded.Content[0].Text.Text = "changed"
				if tt.message.Content[0].Text.Text != tt.modelMessage.Content[0].Text {
					t.Fatal("conversion aliases input")
				}
			})
	}
}

// TestImageContentGetDataURLParts 检查图片 data URL 的拆分结果。
func TestImageContentGetDataURLParts(t *testing.T) {
	// 1. 准备 data URL、普通链接和缺失片段的样例。
	tests := []struct {
		name         string
		dataURL      string
		wantMimeType string
		wantData     string
		wantIsData   bool
	}{
		{
			name:         "png_data_url",
			dataURL:      "data:image/png;base64,AAAA",
			wantMimeType: "image/png",
			wantData:     "AAAA",
			wantIsData:   true,
		},
		{
			name:    "http_link",
			dataURL: "https://example.com/a.png",
		},
		{
			name:    "missing_comma",
			dataURL: "data:image/png;base64",
		},
		{
			name:    "not_base64",
			dataURL: "data:text/plain,hello",
		},
		{
			name:    "missing_mime_type",
			dataURL: "data:;base64,AAAA",
		},
		{
			name:    "empty",
			dataURL: "",
		},
	}
	// 2. 检查 MIME 类型、base64 数据和匹配结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			image := &defs.ImageContent{DataURL: tt.dataURL}
			mimeType, data, isDataURL := image.GetDataURLParts()
			if mimeType != tt.wantMimeType || data != tt.wantData || isDataURL != tt.wantIsData {
				t.Fatalf("GetDataURLParts() = %q, %q, %v; want %q, %q, %v", mimeType, data, isDataURL, tt.wantMimeType, tt.wantData, tt.wantIsData)
			}
		})
	}
}

// TestContentFromLLMKit 检查模型图片内容的转换。
func TestContentFromLLMKit(t *testing.T) {
	// 1. 准备独立 MIME 类型和 base64 图片样例。
	tests := []struct {
		name  string
		block *llmkit.ContentBlock
		want  *defs.Content
	}{
		{
			name: "base64_image",
			block: &llmkit.ContentBlock{
				Type:     llmkit.ContentTypeImage,
				MediaB64: "aGVsbG8=",
				MimeType: "image/png",
			},
			want: &defs.Content{
				Kind:  defs.ContentImage,
				Image: &defs.ImageContent{DataURL: "data:image/png;base64,aGVsbG8="},
			},
		},
	}
	// 2. 检查转换后的 data URL。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				got := &defs.Content{}
				if err := got.FromLLMKit(tt.block); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, tt.want) {
					t.Fatalf("FromLLMKit() = %#v, want %#v", got, tt.want)
				}
			})
	}
}

// TestMessageFromLLMKitRejectsUnsupportedContent 检查转换拒绝不支持的内容并保留原消息。
func TestMessageFromLLMKitRejectsUnsupportedContent(t *testing.T) {
	// 1. 准备音频、原始 JSON 和混合工具调用的样例。
	tests := []struct {
		name    string
		message *llmkit.Message
	}{
		{
			name: "unsupported_audio_after_text",
			message: &llmkit.Message{
				Role: llmkit.RoleAssistant,
				Content: []llmkit.ContentBlock{
					{
						Type: llmkit.ContentTypeText,
						Text: "new",
					},
					{Type: llmkit.ContentTypeAudio},
				},
			},
		},
		{
			name: "raw_message",
			message: &llmkit.Message{
				Role:    llmkit.RoleAssistant,
				RawJSON: json.RawMessage(`{"type":"unknown"}`),
			},
		},
		{
			name: "text_with_tool_call",
			message: &llmkit.Message{
				Role: llmkit.RoleAssistant,
				Content: []llmkit.ContentBlock{
					{
						Type: llmkit.ContentTypeText,
						Text: "hello",
					},
				},
				ToolCalls: []llmkit.ToolCall{
					{
						ID:   "call-a",
						Name: "read",
					},
				},
			},
		},
	}
	// 2. 检查转换报错且接收者不变。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				message := &defs.Message{
					Content: []*defs.Content{
						{
							Kind: defs.ContentText,
							Text: &defs.TextContent{Text: "keep"},
						},
					},
				}
				want := &defs.Message{
					Content: []*defs.Content{
						{
							Kind: defs.ContentText,
							Text: &defs.TextContent{Text: "keep"},
						},
					},
				}
				if err := message.FromLLMKit(tt.message, &defs.Address{}, &defs.Address{}); err == nil {
					t.Fatal("FromLLMKit() accepted unsupported content")
				}
				if !reflect.DeepEqual(message, want) {
					t.Fatalf("failed conversion modified receiver: %#v", message)
				}
			})
	}
}

// TestContextTextContent 检查附带材料标记的复制与编码。
func TestContextTextContent(t *testing.T) {
	// 1. 分别检查普通文本和附带材料经复制、JSON 往返及模型转换后的内容。
	for _, isContext := range []bool{false, true} {
		t.Run(fmt.Sprint(isContext), func(t *testing.T) {
			original := &defs.Content{
				Kind: defs.ContentText,
				Text: &defs.TextContent{
					Text:      "/skill:missing\nfile contents",
					IsContext: isContext,
				},
			}
			encoded, err := json.Marshal(original.Clone())
			if err != nil {
				t.Fatal(err)
			}
			var decoded defs.Content
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(original, &decoded) {
				t.Fatalf("roundtrip = %+v", decoded)
			}
			block, err := decoded.ToLLMKit()
			if err != nil || block.Text != original.Text.Text {
				t.Fatalf("model block = %+v, %v", block, err)
			}
		})
	}
}
