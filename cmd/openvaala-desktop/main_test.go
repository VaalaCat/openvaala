package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/defs"
)

// TestDesktopCommand 验证桌面命令树保留 CLI 帮助、版本和参数错误，不加载窗口或配置。
func TestDesktopCommand(t *testing.T) {
	// 1. 定义无图形会话下可以执行的公开命令行为；损坏的配置文件用来确认打开桌面前不加载配置。
	brokenConfig := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(brokenConfig, []byte("http: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
			wantOutput: []string{"openvaala version " + defs.GetBuildVersion()},
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
		{
			name:      "broken config does not stop the desktop command",
			arguments: []string{"--config-file", brokenConfig, "--url", "ws://127.0.0.1:1"},
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

// TestGetDesktopHTTPHeaders 验证原生界面读取 http.headers：配置有效时拿到合并内置默认后的 header，配置损坏时记一条警告并返回 nil，不阻止窗口打开。
func TestGetDesktopHTTPHeaders(t *testing.T) {
	// 1. 准备有效配置、损坏配置和不存在的配置文件。
	tests := []struct {
		name       string
		config     string // 为空表示配置文件不存在
		wantTeam   string // 期望 http.headers 里的 X-Team；为空时期望返回 nil
		wantWarned bool
	}{
		{
			name:     "valid_config_returns_merged_headers",
			config:   "http:\n  headers:\n    X-Team: infra\n",
			wantTeam: "infra",
		},
		{
			name:       "unknown_variable_returns_nil_and_warns",
			config:     "http:\n  headers:\n    X-Team: ${OPENVAALA_SESION_ID}\n",
			wantWarned: true,
		},
		{
			name:       "broken_yaml_returns_nil_and_warns",
			config:     "http: [\n",
			wantWarned: true,
		},
		{
			name:       "missing_config_file_returns_nil_and_warns",
			wantWarned: true,
		},
	}
	// 2. 用 --config-file 指向配置，记录警告日志，比较返回值。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if tt.config != "" {
				if err := os.WriteFile(path, []byte(tt.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			command := newDesktopCommand()
			if err := command.ParseFlags([]string{"--config-file", path}); err != nil {
				t.Fatal(err)
			}

			got := getDesktopHTTPHeaders(command)

			if tt.wantTeam == "" {
				if got != nil {
					t.Fatalf("getDesktopHTTPHeaders() = %v, want nil", got)
				}
			} else if _, hasDefault := got["User-Agent"]; got["X-Team"] != tt.wantTeam || !hasDefault {
				t.Fatalf("getDesktopHTTPHeaders() = %v, want X-Team %q merged with the built-in defaults", got, tt.wantTeam)
			}
			if warned := strings.Count(logs.String(), "level=WARN"); tt.wantWarned && warned != 1 || !tt.wantWarned && warned != 0 {
				t.Fatalf("warnings = %d, want warned %v: %s", warned, tt.wantWarned, logs.String())
			}
		})
	}
}
