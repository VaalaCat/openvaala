package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestDesktopCommand 验证桌面命令树保留 CLI 帮助、版本和参数错误，不加载窗口或配置。
func TestDesktopCommand(t *testing.T) {
	// 1. 定义无图形会话下可以执行的公开命令行为。
	cases := []struct {
		name       string
		arguments  []string
		wantOutput []string
		wantError  string
	}{
		{
			name:       "complete command tree",
			arguments:  []string{"--help"},
			wantOutput: []string{"app", "tui", "serve", "acp", "app-server", "codex", "web", "config", "plugin", "reindex"},
		},
		{
			name:       "explicit app help without configuration",
			arguments:  []string{"--config-file", filepath.Join(t.TempDir(), "missing.yaml"), "app", "--help"},
			wantOutput: []string{"Open the Web UI in a desktop window", "openvaala app", "--native"},
		},
		{
			name:       "native app help",
			arguments:  []string{"app", "--native", "--help"},
			wantOutput: []string{"openvaala app", "--native"},
		},
		{
			name:       "native root help",
			arguments:  []string{"--native", "--help"},
			wantOutput: []string{"--native"},
		},
		{
			name:      "native flag is not a serve flag",
			arguments: []string{"serve", "--native"},
			wantError: "invalid flags",
		},
		{
			name:       "version",
			arguments:  []string{"--version"},
			wantOutput: []string{"openvaala version " + version},
		},
		{
			name:       "serve help",
			arguments:  []string{"serve", "--help"},
			wantOutput: []string{"openvaala serve"},
		},
		{
			name:       "app server stdio help",
			arguments:  []string{"app-server", "--help"},
			wantOutput: []string{"openvaala app-server"},
		},
		{
			name:      "no desktop alias",
			arguments: []string{"desktop"},
			wantError: "unknown command",
		},
		{
			name:      "app rejects positional arguments",
			arguments: []string{"app", "unexpected"},
			wantError: "unknown command",
		},
		{
			name:      "remote terminal flag is not silently ignored",
			arguments: []string{"--url", "ws://127.0.0.1:1"},
			wantError: "openvaala tui --url",
		},
	}
	// 2. 通过实际命令解析和执行检查输出，不执行 GUI 或真实后台服务。
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			command := newDesktopCommand()
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&output)
			command.SetArgs(tc.arguments)
			err := command.ExecuteContext(context.Background())
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want containing %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range tc.wantOutput {
				if !strings.Contains(output.String(), text) {
					t.Fatalf("output does not contain %q: %s", text, output.String())
				}
			}
		})
	}
}
