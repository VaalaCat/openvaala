package executor_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
)

// TestGetExecutionOutputText 验证执行进度里的 JSON 字符串取出文本，其他内容原样返回。
func TestGetExecutionOutputText(t *testing.T) {
	// 1. 列出输入内容和期望文本，逐项调用并核对结果。
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "json_string",
			content: `"d"`,
			want:    "d",
		},
		{
			name:    "object_keeps_raw_json",
			content: `{"text":"a"}`,
			want:    `{"text":"a"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := executor.GetExecutionOutputText(json.RawMessage(tt.content)); got != tt.want {
				t.Fatalf("text = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestGetToolResultText 验证工具正文的文本提取结果。
func TestGetToolResultText(t *testing.T) {
	// 1. 列出 content、字符串、普通对象和 null，逐项核对提取文本。
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "content_field",
			content: `{"path":"a.txt","content":"line\n","eof":true}`,
			want:    "line\n",
		},
		{
			name:    "json_string",
			content: `"plain"`,
			want:    "plain",
		},
		{
			name:    "other_object_keeps_raw_json",
			content: `{"session_id":"s-1","status":"accepted"}`,
			want:    `{"session_id":"s-1","status":"accepted"}`,
		},
		{
			name:    "null_is_empty_text",
			content: `null`,
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := executor.GetToolResultText(json.RawMessage(tt.content)); got != tt.want {
				t.Fatalf("text = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestGetTurnExecutors 验证环境列表的顺序和根目录。
func TestGetTurnExecutors(t *testing.T) {
	// 1. 创建各类环境列表，核对 Turn 保存的 ID 和根目录。
	for _, tt := range []struct {
		name         string
		environments func(t *testing.T) []executor.ExecutionEnvironment
		want         []defs.TurnExecutor
	}{
		{
			name:         "empty",
			environments: func(*testing.T) []executor.ExecutionEnvironment { return nil },
		},
		{
			name: "order_and_root_directories",
			environments: func(t *testing.T) []executor.ExecutionEnvironment {
				return []executor.ExecutionEnvironment{newEchoEnvironment(t, "local", "bash"), newEchoEnvironment(t, "gpu", "bash")}
			},
			want: []defs.TurnExecutor{{
				ID:                 "local",
				WorkspaceDirectory: "/workspace/local",
			}, {
				ID:                 "gpu",
				WorkspaceDirectory: "/workspace/gpu",
			}},
		},
		{
			name: "nil_workspace_has_no_directory",
			environments: func(t *testing.T) []executor.ExecutionEnvironment {
				environment, err := executor.NewExecutionEnvironment(executor.NewExecutionEnvironmentOpt("local", "", nil))
				if err != nil {
					t.Fatal(err)
				}
				return []executor.ExecutionEnvironment{environment}
			},
			want: []defs.TurnExecutor{{ID: "local"}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := executor.GetTurnExecutors(tt.environments(t)); !slices.Equal(got, tt.want) {
				t.Fatalf("GetTurnExecutors = %+v, want %+v", got, tt.want)
			}
		})
	}
}
