package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/internal/cli"
	"github.com/VaalaCat/openvaala/pkg/defs"
)

func TestDesktopDiscoversRunningServeWithInvalidDefinitions(t *testing.T) {
	for _, directory := range []string{"models", "agents"} {
		t.Run(directory, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/healthz" {
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			runtimeURL := "ws://" + strings.TrimPrefix(server.URL, "http://") + "/api/v1/openvaala"
			for name, content := range map[string]string{
				"config.yaml":                  "data_dir: instance\n",
				directory + "/broken.yaml":     "broken: [\n",
				"instance/runtime-server.yaml": "url: " + runtimeURL + "\ntoken: existing\n",
			} {
				path := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			command := newDesktopCommand()
			if err := command.ParseFlags([]string{"--config-file", filepath.Join(root, "config.yaml")}); err != nil {
				t.Fatal(err)
			}
			target, cfg := cli.ResolveDesktopStartTarget(command)
			if target.URL != "" {
				t.Fatalf("expected local target, got %+v", target)
			}
			connection, err := cli.EnsureRuntimeServer(t.Context(), &cli.RuntimeServerStartOpt{Config: cfg})
			if err != nil || connection.URL != runtimeURL || connection.Token != "existing" {
				t.Fatalf("local connection = %+v, error = %v", connection, err)
			}
		})
	}
}

// TestDesktopCommand 验证桌面命令树保留 CLI 帮助、版本和参数错误，不加载窗口或配置。
func TestDesktopCommand(t *testing.T) {
	// 1. 定义无图形会话下可以执行的公开命令行为；损坏的配置文件用来确认打开桌面前不加载配置。
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
			wantOutput: []string{"Open the Web UI in a desktop window", "openvaala app", "--native", "--url"},
		},
		{
			name:       "native app help",
			arguments:  []string{"app", "--native", "--help"},
			wantOutput: []string{"openvaala app", "--native", "--url"},
		},
		{
			name:       "native root help",
			arguments:  []string{"--native", "--help"},
			wantOutput: []string{"--native", "--url"},
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

// TestDesktopStartTargetResolution 验证桌面命令按 --url > client.url 解析远端目标，且配置损坏时不阻断显式 --url。
func TestDesktopStartTargetResolution(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	brokenConfig := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(brokenConfig, []byte("http: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	remoteConfig := filepath.Join(t.TempDir(), "remote.yaml")
	if err := os.WriteFile(remoteConfig, []byte("client:\n  url: ws://10.0.0.5:8790/api/v1/openvaala\n  token: cfg-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missingConfig := filepath.Join(t.TempDir(), "missing.yaml")

	tests := []struct {
		name string
		args []string
		want cli.DesktopStartTarget
	}{
		{
			name: "flag url overrides client.url",
			args: []string{"--config-file", remoteConfig, "--url", "ws://10.0.0.9:8790/api/v1/openvaala"},
			want: cli.DesktopStartTarget{URL: "ws://10.0.0.9:8790/api/v1/openvaala", Token: "cfg-token", IsOverride: true},
		},
		{
			name: "flag url with inline token does not use client.token",
			args: []string{"--config-file", remoteConfig, "--url", "ws://10.0.0.9:8790/api/v1/openvaala?token=inline"},
			want: cli.DesktopStartTarget{URL: "ws://10.0.0.9:8790/api/v1/openvaala?token=inline", Token: "", IsOverride: true},
		},
		{
			name: "client.url used when flag omitted",
			args: []string{"--config-file", remoteConfig},
			want: cli.DesktopStartTarget{URL: "ws://10.0.0.5:8790/api/v1/openvaala", Token: "cfg-token", IsOverride: false},
		},
		{
			name: "broken config does not stop explicit --url",
			args: []string{"--config-file", brokenConfig, "--url", "ws://127.0.0.1:8790"},
			want: cli.DesktopStartTarget{URL: "ws://127.0.0.1:8790", Token: "", IsOverride: true},
		},
		{
			name: "missing config without flag falls back to local",
			args: []string{"--config-file", missingConfig},
			want: cli.DesktopStartTarget{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command := newDesktopCommand()
			if err := command.ParseFlags(tt.args); err != nil {
				t.Fatal(err)
			}
			got, _ := cli.ResolveDesktopStartTarget(command)
			if got != tt.want {
				t.Fatalf("ResolveDesktopStartTarget() = %+v, want %+v", got, tt.want)
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

			_, cfg := cli.ResolveDesktopStartTarget(command)
			var got map[string]string
			if cfg != nil {
				got = cfg.GetHTTPHeaders()
			}

			if tt.wantTeam == "" {
				if got != nil {
					t.Fatalf("desktop headers = %v, want nil", got)
				}
			} else if _, hasDefault := got["User-Agent"]; got["X-Team"] != tt.wantTeam || !hasDefault {
				t.Fatalf("desktop headers = %v, want X-Team %q merged with the built-in defaults", got, tt.wantTeam)
			}
			if warned := strings.Count(logs.String(), "level=WARN"); tt.wantWarned && warned != 1 || !tt.wantWarned && warned != 0 {
				t.Fatalf("warnings = %d, want warned %v: %s", warned, tt.wantWarned, logs.String())
			}
		})
	}
}
