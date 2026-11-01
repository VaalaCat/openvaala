package mcpclient

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestGetModelToolName 验证模型工具名的字符替换和超长哈希。
func TestGetModelToolName(t *testing.T) {
	// 1. 准备普通、非法字符、非 ASCII 和长度上限用例。
	tests := []struct {
		name       string
		serverID   string
		toolName   string
		want       string
		wantLength int
	}{
		{
			name:     "plain name",
			serverID: "serena",
			toolName: "find_symbol",
			want:     "mcp__serena__find_symbol",
		},
		{
			name:     "invalid characters become underscores",
			serverID: "fs",
			toolName: "browser.navigate/v2",
			want:     "mcp__fs__browser_navigate_v2",
		},
		{
			name:     "non ASCII characters become underscores",
			serverID: "s",
			toolName: "查询",
			want:     "mcp__s____",
		},
		{
			name:     "dash is kept",
			serverID: "s",
			toolName: "list-files",
			want:     "mcp__s__list-files",
		},
		{
			name:     "exactly 64 characters is kept",
			serverID: "s",
			toolName: strings.Repeat("a", 56),
			want:     "mcp__s__" + strings.Repeat("a", 56),
		},
		{
			name:       "65 characters is hashed",
			serverID:   "s",
			toolName:   strings.Repeat("a", 57),
			want:       "mcp__s__" + strings.Repeat("a", 47) + "_a7ccd3b6",
			wantLength: 64,
		},
		{
			name:       "80 characters is hashed",
			serverID:   "s",
			toolName:   strings.Repeat("a", 80),
			want:       "mcp__s__" + strings.Repeat("a", 47) + "_65833658",
			wantLength: 64,
		},
		{
			name:       "hash uses the original full name",
			serverID:   "s",
			toolName:   strings.Repeat("a.", 40),
			want:       "mcp__s__" + strings.Repeat("a_", 23) + "a_ffdab39a",
			wantLength: 64,
		},
	}
	// 2. 逐项生成名称，核对字符内容和长度。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getModelToolName(tt.serverID, tt.toolName)
			if got != tt.want {
				t.Fatalf("getModelToolName(%q, %q) = %q, want %q", tt.serverID, tt.toolName, got, tt.want)
			}
			if tt.wantLength != 0 && len(got) != tt.wantLength {
				t.Fatalf("len(%q) = %d, want %d", got, len(got), tt.wantLength)
			}
		})
	}
}

// TestGetModelToolNames 验证批量工具名去重及顺序。
func TestGetModelToolNames(t *testing.T) {
	// 1. 准备无冲突、字符替换冲突和空列表用例。
	tests := []struct {
		name      string
		serverID  string
		toolNames []string
		want      []string
	}{
		{
			name:      "no collision",
			serverID:  "s",
			toolNames: []string{"a", "b"},
			want:      []string{"mcp__s__a", "mcp__s__b"},
		},
		{
			name:      "empty",
			serverID:  "s",
			toolNames: nil,
			want:      []string{},
		},
		{
			name:      "later tool with the same sanitized name is hashed",
			serverID:  "s",
			toolNames: []string{"a.b", "a_b"},
			want:      []string{"mcp__s__a_b", "mcp__s__a_b_7ad4bf32"},
		},
		{
			name:      "every later colliding tool is hashed from its own original name",
			serverID:  "s",
			toolNames: []string{"a.b", "a_b", "a b"},
			want:      []string{"mcp__s__a_b", "mcp__s__a_b_7ad4bf32", "mcp__s__a_b_1d75eb1c"},
		},
		{
			name:      "output keeps input order",
			serverID:  "s",
			toolNames: []string{"z", "a.b", "m", "a_b"},
			want:      []string{"mcp__s__z", "mcp__s__a_b", "mcp__s__m", "mcp__s__a_b_7ad4bf32"},
		},
	}
	// 2. 逐项批量生成名称，核对去重结果及顺序。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getModelToolNames(tt.serverID, tt.toolNames)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("getModelToolNames(%q, %q) = %q, want %q", tt.serverID, tt.toolNames, got, tt.want)
			}
		})
	}
}

// TestGetToolNameParts 验证模型工具名拆成 server ID 和工具名，格式不对时返回 false。
func TestGetToolNameParts(t *testing.T) {
	// 1. 准备有效工具名及各类格式错误。
	tests := []struct {
		name         string
		modelName    string
		wantServerID string
		wantToolName string
		wantOK       bool
	}{
		{
			name:         "mcp tool",
			modelName:    "mcp__serena__find_symbol",
			wantServerID: "serena",
			wantToolName: "find_symbol",
			wantOK:       true,
		},
		{
			name:         "tool name keeps later double underscores",
			modelName:    "mcp__serena__a__b",
			wantServerID: "serena",
			wantToolName: "a__b",
			wantOK:       true,
		},
		{
			name:      "built-in tool",
			modelName: "bash",
		},
		{
			name:      "prefix only",
			modelName: "mcp__",
		},
		{
			name:      "missing tool part",
			modelName: "mcp__x",
		},
		{
			name:      "empty tool name",
			modelName: "mcp__x__",
		},
		{
			name:      "empty server id",
			modelName: "mcp____tool",
		},
		{
			name:      "single underscore prefix",
			modelName: "mcp_x__y",
		},
		{
			name:      "empty",
			modelName: "",
		},
	}
	// 2. 逐项拆分名称，核对 server ID、工具名和格式标记。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serverID, toolName, ok := GetToolNameParts(tt.modelName)
			if serverID != tt.wantServerID || toolName != tt.wantToolName || ok != tt.wantOK {
				t.Fatalf("GetToolNameParts(%q) = (%q, %q, %v), want (%q, %q, %v)", tt.modelName, serverID, toolName, ok, tt.wantServerID, tt.wantToolName, tt.wantOK)
			}
		})
	}
}

// TestGetModelInputSchema 验证缺失顶层 type 的补全和原 schema 保留。
func TestGetModelInputSchema(t *testing.T) {
	// 1. 准备对象、null、已有类型及无效 JSON 用例。
	tests := []struct {
		name    string
		schema  string
		want    string
		wantErr bool
	}{
		{
			name:   "empty object gets type",
			schema: `{}`,
			want:   `{"type":"object"}`,
		},
		{
			name:   "null gets type",
			schema: `null`,
			want:   `{"type":"object"}`,
		},
		{
			name:   "existing type is kept as is",
			schema: `{ "type": "object", "x": 1 }`,
			want:   `{ "type": "object", "x": 1 }`,
		},
		{
			name:   "non object type is kept",
			schema: `{"type":"string"}`,
			want:   `{"type":"string"}`,
		},
		{
			name:   "other keys are kept when type is added",
			schema: `{"properties":{"q":{"type":"string","description":"a<b"}},"required":["q"]}`,
			want:   `{"properties":{"q":{"type":"string","description":"a<b"}},"required":["q"],"type":"object"}`,
		},
		{
			name:    "array is invalid",
			schema:  `[]`,
			wantErr: true,
		},
		{
			name:    "broken JSON is invalid",
			schema:  `{`,
			wantErr: true,
		},
		{
			name:    "empty input is invalid",
			schema:  ``,
			wantErr: true,
		},
	}
	// 2. 逐项转换输入，核对返回内容和错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getModelInputSchema(json.RawMessage(tt.schema))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("getModelInputSchema(%q) = %s, want error", tt.schema, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("getModelInputSchema(%q) error = %v", tt.schema, err)
			}
			if string(got) != tt.want {
				t.Fatalf("getModelInputSchema(%q) = %s, want %s", tt.schema, got, tt.want)
			}
		})
	}
}

// TestNewToolResult 验证 MCP 内容转换为正文、占位文本和工具错误。
func TestNewToolResult(t *testing.T) {
	// 1. 准备文本、资源、多媒体和结构化结果。
	text := func(value string) mcp.Content { return &mcp.TextContent{Text: value} }
	tests := []struct {
		name        string
		result      *mcp.CallToolResult
		wantContent string
		wantError   *defs.ToolError
	}{
		{
			name:        "text blocks are joined by newlines",
			result:      &mcp.CallToolResult{Content: []mcp.Content{text("a"), text("b")}},
			wantContent: `"a\nb"`,
		},
		{
			name:        "single text keeps characters unescaped",
			result:      &mcp.CallToolResult{Content: []mcp.Content{text("<a & b>")}},
			wantContent: `"<a & b>"`,
		},
		{
			name:        "no content is an empty string",
			result:      &mcp.CallToolResult{},
			wantContent: `""`,
		},
		{
			name: "image becomes a placeholder",
			result: &mcp.CallToolResult{Content: []mcp.Content{
				text("a"),
				&mcp.ImageContent{
					Data:     make([]byte, 1024),
					MIMEType: "image/png",
				},
			}},
			wantContent: `"a\n[image omitted: image/png, 1 KB]"`,
		},
		{
			name: "small image is shown in bytes",
			result: &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{
				Data:     make([]byte, 1023),
				MIMEType: "image/jpeg",
			}}},
			wantContent: `"[image omitted: image/jpeg, 1023 B]"`,
		},
		{
			name: "image size is rounded to kilobytes",
			result: &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{
				Data:     make([]byte, 34*1024+600),
				MIMEType: "image/png",
			}}},
			wantContent: `"[image omitted: image/png, 35 KB]"`,
		},
		{
			name: "large image is shown in megabytes",
			result: &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{
				Data:     make([]byte, 3*1024*1024),
				MIMEType: "image/png",
			}}},
			wantContent: `"[image omitted: image/png, 3 MB]"`,
		},
		{
			name: "audio becomes a placeholder",
			result: &mcp.CallToolResult{Content: []mcp.Content{&mcp.AudioContent{
				Data:     make([]byte, 2048),
				MIMEType: "audio/wav",
			}}},
			wantContent: `"[audio omitted: audio/wav, 2 KB]"`,
		},
		{
			name:        "structured content is used when there is no text",
			result:      &mcp.CallToolResult{StructuredContent: map[string]any{"ok": true}},
			wantContent: `{"ok":true}`,
		},
		{
			name: "structured content is ignored when there is text",
			result: &mcp.CallToolResult{
				Content:           []mcp.Content{text("done")},
				StructuredContent: map[string]any{"ok": true},
			},
			wantContent: `"done"`,
		},
		{
			name: "structured content is ignored when there is a text resource",
			result: &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
					URI:  "file:///a.txt",
					Text: "hello",
				}}},
				StructuredContent: map[string]any{"ok": true},
			},
			wantContent: `"hello"`,
		},
		{
			name: "structured array is passed as is",
			result: &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.ImageContent{
					Data:     []byte("x"),
					MIMEType: "image/png",
				}},
				StructuredContent: []any{1, 2},
			},
			wantContent: `[1,2]`,
		},
		{
			name: "text resource uses its text",
			result: &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
					URI:      "file:///a.txt",
					MIMEType: "text/plain",
					Text:     "hello",
				}},
			}},
			wantContent: `"hello"`,
		},
		{
			name: "binary resource and resource link become placeholders",
			result: &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
					URI:      "file:///a.pdf",
					MIMEType: "application/pdf",
					Blob:     []byte("pdf"),
				}},
				&mcp.ResourceLink{
					URI:  "file:///b.pdf",
					Name: "b",
				},
			}},
			wantContent: `"[resource: file:///a.pdf]\n[resource: file:///b.pdf]"`,
		},
		{
			name:        "embedded resource without content is skipped",
			result:      &mcp.CallToolResult{Content: []mcp.Content{text("a"), &mcp.EmbeddedResource{}}},
			wantContent: `"a"`,
		},
		{
			name: "server error reports the joined text and no content",
			result: &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{text("repository"), text("not found")},
			},
			wantError: &defs.ToolError{
				Code:    "mcp_tool_error",
				Message: "repository\nnot found",
			},
		},
		{
			name: "server error without text reports structured content",
			result: &mcp.CallToolResult{
				IsError:           true,
				StructuredContent: map[string]any{"reason": "denied"},
			},
			wantError: &defs.ToolError{
				Code:    "mcp_tool_error",
				Message: `{"reason":"denied"}`,
			},
		},
	}
	// 2. 逐项转换输入，核对返回内容和错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newToolResult("call-1", tt.result)
			if got.CallID != "call-1" {
				t.Fatalf("CallID = %q, want call-1", got.CallID)
			}
			if string(got.Content) != tt.wantContent {
				t.Fatalf("Content = %s, want %s", got.Content, tt.wantContent)
			}
			if got.Truncated {
				t.Fatal("Truncated = true, want false")
			}
			if tt.wantError == nil {
				if got.Error != nil {
					t.Fatalf("Error = %+v, want nil", got.Error)
				}
				return
			}
			if got.Error == nil || *got.Error != *tt.wantError {
				t.Fatalf("Error = %+v, want %+v", got.Error, tt.wantError)
			}
		})
	}
}

// TestNewToolResultTruncation 验证超限内容在完整 UTF-8 字符处截断。
func TestNewToolResultTruncation(t *testing.T) {
	// 1. 准备大文本、结构化内容和错误正文。
	const limit = 1024 * 1024
	structuredItems := make([]any, 0, 200000)
	for range 200000 {
		structuredItems = append(structuredItems, "item-xxxxxx")
	}
	tests := []struct {
		name          string
		result        *mcp.CallToolResult
		wantTruncated bool
		wantLength    int
		wantError     bool
	}{
		{
			name:          "Chinese text is cut at a character boundary",
			result:        &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("中", 400000)}}},
			wantTruncated: true,
			wantLength:    limit - limit%3,
		},
		{
			name:          "ASCII text is cut at the limit",
			result:        &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("a", limit+1)}}},
			wantTruncated: true,
			wantLength:    limit,
		},
		{
			name:       "text exactly at the limit is kept",
			result:     &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("a", limit)}}},
			wantLength: limit,
		},
		{
			name:          "text blocks are limited after joining",
			result:        &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("a", limit)}, &mcp.TextContent{Text: "tail"}}},
			wantTruncated: true,
			wantLength:    limit,
		},
		{
			name:          "oversized structured content becomes truncated text",
			result:        &mcp.CallToolResult{StructuredContent: structuredItems},
			wantTruncated: true,
			wantLength:    limit,
		},
		{
			name: "oversized error message is truncated",
			result: &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("错", 400000)}},
			},
			wantTruncated: true,
			wantLength:    limit - limit%3,
			wantError:     true,
		},
	}
	// 2. 逐项转换输入，核对返回内容和错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newToolResult("call-1", tt.result)
			if got.Truncated != tt.wantTruncated {
				t.Fatalf("Truncated = %v, want %v", got.Truncated, tt.wantTruncated)
			}
			var body string
			if tt.wantError {
				if got.Error == nil || len(got.Content) != 0 {
					t.Fatalf("Error = %+v, Content length = %d, want an error without content", got.Error, len(got.Content))
				}
				body = got.Error.Message
			} else if err := json.Unmarshal(got.Content, &body); err != nil {
				t.Fatalf("Content is not a JSON string: %v", err)
			}
			if !utf8.ValidString(body) {
				t.Fatal("body is not valid UTF-8")
			}
			if len(body) != tt.wantLength {
				t.Fatalf("body length = %d, want %d", len(body), tt.wantLength)
			}
		})
	}
}

// TestNewToolErrorResult 验证未取得 server 结果时的错误字段。
func TestNewToolErrorResult(t *testing.T) {
	// 1. 准备请求失败和连接不可用错误。
	tests := []struct {
		name string
		code string
		err  error
	}{
		{
			name: "request failed",
			code: "mcp_request_failed",
			err:  errors.New("tool not found"),
		},
		{
			name: "server unavailable",
			code: "mcp_server_unavailable",
			err:  errors.New("connection closed"),
		},
	}
	// 2. 逐项转换输入，核对返回内容和错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newToolErrorResult("call-1", tt.code, tt.err)
			want := &defs.ToolResult{
				CallID: "call-1",
				Error: &defs.ToolError{
					Code:    tt.code,
					Message: tt.err.Error(),
				},
			}
			if got.CallID != want.CallID || len(got.Content) != 0 || got.Truncated || got.Error == nil || *got.Error != *want.Error {
				t.Fatalf("newToolErrorResult() = %+v, want %+v", got, want)
			}
		})
	}
}
