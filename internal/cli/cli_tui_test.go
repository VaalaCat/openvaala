package cli

import (
	"io"
	"strings"
	"testing"
)

// TestTUIRequiresTerminal 检查 TUI 拒绝非终端输入。
func TestTUIRequiresTerminal(t *testing.T) {
	// 1. 列出默认入口和 TUI 子命令场景。
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "root_without_arguments",
			args: nil,
		},
		{
			name: "tui_subcommand",
			args: []string{"tui"},
		},
		{
			name: "tui_with_url",
			args: []string{"tui", "--url", "ws://127.0.0.1:1/api/v1/openvaala"},
		},
	}
	// 2. 使用字符串输入执行命令，检查交互终端错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			command := NewCLI(&CLIOpt{
				Stdout: io.Discard,
				Stderr: io.Discard,
			})
			command.SetArgs(tt.args)
			command.SetIn(strings.NewReader(""))
			if err := command.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "requires an interactive terminal") {
				t.Fatalf("error = %v, want terminal requirement", err)
			}
		})
	}
}

// TestGetConnectURL 检查面板连接地址中的 token 和无效地址处理。
func TestGetConnectURL(t *testing.T) {
	// 1. 列出配置 token、URL token 和无 token 场景。
	tests := []struct {
		name        string
		url         string
		configToken string
		want        string
	}{
		{
			name:        "config_token_added_to_query",
			url:         "ws://127.0.0.1:8790/api/v1/openvaala",
			configToken: "from-config",
			want:        "ws://127.0.0.1:8790/api/v1/openvaala?token=from-config",
		},
		{
			name:        "url_token_kept_as_is",
			url:         "ws://127.0.0.1:8790/api/v1/openvaala?token=from-url",
			configToken: "from-config",
			want:        "ws://127.0.0.1:8790/api/v1/openvaala?token=from-url",
		},
		{
			name:        "other_query_kept",
			url:         "ws://127.0.0.1:8790/api/v1/openvaala?debug=1",
			configToken: "from-config",
			want:        "ws://127.0.0.1:8790/api/v1/openvaala?debug=1&token=from-config",
		},
		{
			name:        "no_token_to_add",
			url:         "ws://127.0.0.1:8790/api/v1/openvaala",
			configToken: "",
			want:        "",
		},
		{
			name:        "invalid_url_hidden",
			url:         "ws://%zz",
			configToken: "from-config",
			want:        "",
		},
	}
	// 2. 生成连接地址并检查结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getConnectURL(tt.url, tt.configToken); got != tt.want {
				t.Fatalf("getConnectURL(%q, %q) = %q, want %q", tt.url, tt.configToken, got, tt.want)
			}
		})
	}
}
