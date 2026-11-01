package jsonutil

import (
	"encoding/json"
	"testing"
)

// TestMarshalWithoutHTMLEscape 检查不转义 HTML 的 JSON 编码。
func TestMarshalWithoutHTMLEscape(t *testing.T) {
	// 1. 准备 HTML 字符、嵌套载荷、空值及无效输入。
	tests := []struct {
		name    string
		value   any
		want    string
		wantErr bool
	}{
		{
			name:  "html_characters_stay_literal",
			value: "<a&b>",
			want:  `"<a&b>"`,
		},
		{
			name:  "nested_raw_message_keeps_html_characters",
			value: map[string]any{"content": json.RawMessage(`{"output":"<a&b>"}`)},
			want:  `{"content":{"output":"<a&b>"}}`,
		},
		{
			name: "struct_fields_and_unicode",
			value: struct {
				Text string `json:"text"`
			}{Text: "<你好>"},
			want: `{"text":"<你好>"}`,
		},
		{
			name:  "nil_is_null_without_trailing_newline",
			value: nil,
			want:  `null`,
		},
		{
			name:    "unsupported_value_returns_error",
			value:   make(chan int),
			wantErr: true,
		},
		{
			name:    "invalid_raw_message_returns_error",
			value:   json.RawMessage(`{`),
			wantErr: true,
		},
	}
	// 2. 检查编码内容、结尾换行和错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MarshalWithoutHTMLEscape(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("MarshalWithoutHTMLEscape() error = %v, wantErr %v", err, tt.wantErr)
			}
			if string(got) != tt.want {
				t.Errorf("MarshalWithoutHTMLEscape() = %q, want %q", got, tt.want)
			}
		})
	}
}
