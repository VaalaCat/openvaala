package cli

import (
	"context"
	"io"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/internal/config"
)

// TestCodexCommandRouting 检查 Codex 终端和桌面命令的参数分发。
func TestCodexCommandRouting(t *testing.T) {
	// 1. 列出终端、桌面目录和连接地址场景。
	tests := []struct {
		name          string
		arguments     []string
		wantCommand   string
		wantArguments []string
		wantRoot      []string
		wantURL       string
		wantError     string
	}{
		{
			name:        "terminal",
			arguments:   []string{"codex"},
			wantCommand: "codex",
		},
		{
			name:          "terminal_resume",
			arguments:     []string{"codex", "--", "resume", "--last"},
			wantCommand:   "codex",
			wantArguments: []string{"resume", "--last"},
		},
		{
			name:          "terminal_prompt",
			arguments:     []string{"codex", "--", "app"},
			wantCommand:   "codex",
			wantArguments: []string{"app"},
		},
		{
			name:          "terminal_options",
			arguments:     []string{"-c", "model=x", "codex", "--", "resume", "id"},
			wantCommand:   "codex",
			wantArguments: []string{"resume", "id"},
			wantRoot:      []string{"-c", "model=x"},
		},
		{
			name:        "desktop_default_directory",
			arguments:   []string{"codex", "app"},
			wantCommand: "app",
		},
		{
			name:          "desktop_directory",
			arguments:     []string{"codex", "app", "./项目 & notes"},
			wantCommand:   "app",
			wantArguments: []string{"./项目 & notes"},
		},
		{
			name:        "desktop_url",
			arguments:   []string{"codex", "app", "--url", "ws://localhost:8790/api/v1/openvaala"},
			wantCommand: "app",
			wantURL:     "ws://localhost:8790/api/v1/openvaala",
		},
		{
			name:        "desktop_url_before_subcommand",
			arguments:   []string{"codex", "--url", "ws://localhost:8790/api/v1/openvaala", "app"},
			wantCommand: "app",
			wantURL:     "ws://localhost:8790/api/v1/openvaala",
		},
		{
			name:      "desktop_extra_directory",
			arguments: []string{"codex", "app", "one", "two"},
			wantError: "accepts at most 1 arg(s), received 2",
		},
	}
	// 2. 执行命令，检查命中子命令和传递的参数。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Chdir(t.TempDir())
			root := NewCLI(&CLIOpt{
				Stdout: io.Discard,
				Stderr: io.Discard,
			})
			var gotCommand string
			var gotArgs *AppArgs
			for _, path := range [][]string{{"codex"}, {"codex", "app"}} {
				command, _, err := root.Find(path)
				if err != nil {
					t.Fatal(err)
				}
				command.RunE = BuildCLIHandler(func(_ context.Context, args *AppArgs) error {
					gotCommand, gotArgs = command.Name(), args
					return nil
				})
			}
			root.SetArgs(tt.arguments)
			err := root.ExecuteContext(t.Context())
			if tt.wantError != "" {
				if err == nil || err.Error() != tt.wantError {
					t.Fatalf("error = %v, want %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if gotCommand != tt.wantCommand || !slices.Equal(gotArgs.CodexArguments, tt.wantArguments) || !slices.Equal(gotArgs.CodexRootArguments, tt.wantRoot) || gotArgs.RuntimeURL != tt.wantURL {
				t.Fatalf("command = %s, arguments = %v, root arguments = %v, URL = %s", gotCommand, gotArgs.CodexArguments, gotArgs.CodexRootArguments, gotArgs.RuntimeURL)
			}
		})
	}
}

// TestCodexAppLaunchConfiguration 检查桌面应用启动命令及后端配置。
func TestCodexAppLaunchConfiguration(t *testing.T) {
	// 1. 列出默认配置、显式路径、覆盖项和继承环境场景。
	tests := []struct {
		name        string
		arguments   []string
		environment map[string]string
		configFile  bool
	}{
		{name: "default_config_does_not_require_file"},
		{
			name:       "relative_config_file",
			arguments:  []string{"--config-file", "config.yaml"},
			configFile: true,
		},
		{
			name:        "relative_config_environment",
			environment: map[string]string{"OPENVAALA_CONFIG": "config.yaml"},
			configFile:  true,
		},
		{
			name: "explicit_overrides",
			arguments: []string{"--config-file", "config.yaml", "--agents-dir", "custom-agents", "--models-dir", "custom-models", "--agent", "assistant",
				"--url", "ws://localhost:9900/api/v1/openvaala?token=override"},
			environment: map[string]string{"OPENVAALA_RUN_AGENT_NAME": "default", "OPENVAALA_CLIENT_URL": "ws://localhost:8800", "OPENVAALA_CLIENT_TOKEN": "test-token"},
			configFile:  true,
		},
		{
			name:        "inherited_environment",
			environment: map[string]string{"OPENVAALA_CLIENT_URL": "ws://localhost:8800", "OPENVAALA_CLIENT_TOKEN": "test-token"},
		},
	}
	// 2. 收集启动参数，检查应用路径、工作目录深链接和环境，再从另一目录加载后端配置。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			directory := t.TempDir()
			t.Chdir(directory)
			if tt.configFile {
				writeTestFiles(t, map[string]string{filepath.Join(directory, "config.yaml"): "run:\n  agent_name: default\n"})
			}
			for name, value := range tt.environment {
				t.Setenv(name, value)
			}
			t.Setenv("CODEX_CLI_PATH", "/old/codex")
			t.Setenv("CODEX_APP_SERVER_FORCE_CLI", "0")
			root := NewCLI(&CLIOpt{
				Stdout: io.Discard,
				Stderr: io.Discard,
			})
			app, _, err := root.Find([]string{"codex", "app"})
			if err != nil {
				t.Fatal(err)
			}
			var gotArgs *AppArgs
			app.RunE = BuildCLIHandler(func(_ context.Context, args *AppArgs) error { gotArgs = args; return nil })
			root.SetArgs(append([]string{"codex", "app"}, tt.arguments...))
			if err := root.ExecuteContext(t.Context()); err != nil {
				t.Fatal(err)
			}
			workspace := filepath.Join(directory, "项目 & notes+#")
			application := "/Applications/Codex.app"
			command := newCodexAppLaunchCommand(t.Context(), gotArgs, application, workspace)
			if command.Path != "/usr/bin/open" || !slices.Equal(command.Args[1:3], []string{"-a", application}) {
				t.Fatalf("launch command = %v", command.Args)
			}
			link, err := url.Parse(command.Args[len(command.Args)-1])
			if err != nil || link.Scheme != "codex" || link.Host != "threads" || link.Path != "/new" || link.Query().Get("path") != workspace {
				t.Fatalf("workspace link = %s, error = %v", link, err)
			}
			environment := map[string]string{}
			for index := 3; index < len(command.Args)-1; index += 2 {
				if command.Args[index] != "--env" {
					t.Fatalf("launch option = %q, want --env", command.Args[index])
				}
				name, value, found := strings.Cut(command.Args[index+1], "=")
				if !found {
					t.Fatalf("invalid environment entry: %q", command.Args[index+1])
				}
				environment[name] = value
			}
			if environment["CODEX_CLI_PATH"] != gotArgs.Executable {
				t.Fatalf("backend environment = %v", environment)
			}
			// 桌面启动环境不强制 durable 云端连接改用本地 CLI。
			if environment["CODEX_APP_SERVER_FORCE_CLI"] == "1" {
				t.Fatal("desktop launch must not force cloud WebSocket connections onto the local CLI")
			}
			if value, found := environment["OPENVAALA_CONFIG"]; found != tt.configFile || found && value != gotArgs.ConfigPath {
				t.Fatalf("OPENVAALA_CONFIG = %q, present = %v", value, found)
			}
			// 从另一目录仅按启动环境加载后端配置，不传父 CLI 参数。
			t.Chdir(t.TempDir())
			for name, value := range environment {
				t.Setenv(name, value)
			}
			backendConfig, err := config.LoadConfig(&config.LoadConfigOpt{})
			if err != nil {
				t.Fatal(err)
			}
			wantURL := gotArgs.RuntimeURL
			if wantURL == "" {
				wantURL = gotArgs.Config.Client.URL
			}
			if backendConfig.Run.AgentName != gotArgs.Config.Run.AgentName || backendConfig.Agents.Directory != gotArgs.Config.Agents.Directory || backendConfig.Models.Directory != gotArgs.Config.Models.Directory || backendConfig.Client.URL != wantURL || backendConfig.Client.Token != gotArgs.Config.Client.Token {
				t.Fatalf("backend configuration differs from the launcher's configuration: %+v", backendConfig)
			}
		})
	}
}

// TestCodexAppUnsupportedPlatform 检查非 macOS 系统拒绝启动桌面应用。
func TestCodexAppUnsupportedPlatform(t *testing.T) {
	// 1. 在 macOS 跳过检查，其他系统执行桌面命令并检查错误。
	if runtime.GOOS == "darwin" {
		t.Skip("only verifies rejection on other platforms; must not launch a real desktop app")
	}
	for _, arguments := range [][]string{{"codex", "app"}, {"codex", "app", "/missing"}} {
		t.Run(strings.Join(arguments, "_"), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			root := NewCLI(&CLIOpt{
				Stdout: io.Discard,
				Stderr: io.Discard,
			})
			root.SetArgs(arguments)
			err := root.ExecuteContext(t.Context())
			if err == nil || err.Error() != "openvaala codex app is only supported on macOS" {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
