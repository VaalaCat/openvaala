package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/defs"
)

// TestAppServerStartupConfig 检查桌面启动查询只输出配置 JSON。
func TestAppServerStartupConfig(t *testing.T) {
	// 1. 列出独立查询、桌面参数和退出登录参数场景。
	tests := []struct {
		name      string
		arguments []string
	}{
		{
			name:      "standalone",
			arguments: []string{"app-server", "--print-startup-config"},
		},
		{
			name: "desktop_startup",
			arguments: []string{"-c", "features.code_mode_host=true", "app-server", "--analytics-default-enabled",
				"--print-startup-config", "--startup-client-info", `{"name":"codex_desktop","title":"Codex Desktop","version":"26.1021.31345"}`},
		},
		{
			name: "desktop_sign_out",
			arguments: []string{"app-server", "--print-startup-config", "--startup-sign-out",
				"--startup-client-info", `{"name":"codex_desktop","version":"26.1021.31345"}`},
		},
	}
	// 2. 使用不可访问的 serve 地址执行查询，检查 JSON、空错误输出和未创建连接文件。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			// 使用不可访问的 serve 地址查询本地启动配置。
			t.Setenv("OPENVAALA_CLIENT_URL", "ws://127.0.0.1:1/api/v1/openvaala")
			var output, stderr bytes.Buffer
			root := NewCLI(&CLIOpt{
				Stdout: &output,
				Stderr: &stderr,
			})
			root.SetArgs(tt.arguments)
			if err := root.ExecuteContext(t.Context()); err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(output.Bytes(), &got); err != nil {
				t.Fatalf("startup output is not one JSON object: %v; output=%q", err, output.String())
			}
			if got["appServerVersion"] != defs.GetBuildVersion() {
				t.Fatalf("appServerVersion = %v", got["appServerVersion"])
			}
			requirements, ok := got["configRequirements"].(map[string]any)
			if !ok {
				t.Fatalf("configRequirements = %v", got["configRequirements"])
			}
			if value, exists := requirements["requirements"]; !exists || value != nil {
				t.Fatalf("requirements = %v, exists = %v; want explicit null", value, exists)
			}
			if stderr.Len() != 0 {
				t.Fatalf("unexpected startup stderr: %s", stderr.String())
			}
			if _, err := os.Stat(filepath.Join(home, ".local", "state", "openvaala", "runtime-server.yaml")); !os.IsNotExist(err) {
				t.Fatalf("startup check should not start serve: %v", err)
			}
		})
	}
}

// TestAppServerFlags 检查 app-server 参数解析、监听方式和拒绝的参数。
func TestAppServerFlags(t *testing.T) {
	// 1. 列出监听地址、Codex 参数和无效写法。
	const (
		configOverrideError = "--config takes key=value (a Codex configuration override); use --config-file for the OpenVaala configuration file"
		wsAuthError         = "app-server only listens on loopback, and the runtime server checks the token"
		listenHelp          = "use stdio://, unix://, unix://PATH or ws://IP:PORT"
	)
	stdio := func(string, string) appServerListen { return appServerListen{Kind: appServerListenStdio} }
	webSocket := func(address string) func(string, string) appServerListen {
		return func(string, string) appServerListen {
			return appServerListen{
				Kind:             appServerListenWebSocket,
				WebSocketAddress: netip.MustParseAddrPort(address),
			}
		}
	}
	tests := []struct {
		name string
		args []string
		// want 返回期望的监听方式，home 是测试的 HOME，directory 是当前目录；为 nil 时期望报 wantError。
		want        func(home, directory string) appServerListen
		wantIgnored []string
		wantError   string
	}{
		{
			name:        "codex_sdk_command_line",
			args:        []string{"--config", "k=v", "app-server"},
			want:        stdio,
			wantIgnored: []string{"--config"},
		},
		{
			name: "all_ignored_flags",
			args: []string{
				"app-server", "-c", "k=v", "--enable", "x", "--disable", "y", "--strict-config",
				"--analytics-default-enabled", "--code-mode-host", "u", "--remote-control",
			},
			want: stdio,
			wantIgnored: []string{
				"--config", "--enable", "--disable", "--strict-config",
				"--analytics-default-enabled", "--code-mode-host", "--remote-control",
			},
		},
		{
			name: "config_file_is_not_ignored",
			args: []string{"--config-file", "./config.yaml", "app-server"},
			want: stdio,
		},
		{
			name: "no_flags_is_stdio",
			args: []string{"app-server"},
			want: stdio,
		},
		{
			name: "stdio_flag",
			args: []string{"app-server", "--stdio"},
			want: stdio,
		},
		{
			name: "stdio_url",
			args: []string{"app-server", "--listen", "stdio://"},
			want: stdio,
		},
		{
			name: "stdio_flag_with_stdio_url",
			args: []string{"app-server", "--stdio", "--listen", "stdio://"},
			want: stdio,
		},
		{
			name: "default_socket_path",
			args: []string{"app-server", "--listen", "unix://"},
			want: func(home, _ string) appServerListen {
				return appServerListen{
					Kind:       appServerListenUnix,
					SocketPath: filepath.Join(home, ".local", "state", "openvaala", "app-server.sock"),
				}
			},
		},
		{
			name: "relative_socket_path_made_absolute",
			args: []string{"app-server", "--listen", "unix://sockets/codex.sock"},
			want: func(_, directory string) appServerListen {
				return appServerListen{
					Kind:       appServerListenUnix,
					SocketPath: filepath.Join(directory, "sockets", "codex.sock"),
				}
			},
		},
		{
			name: "absolute_socket_path_kept",
			args: []string{"app-server", "--listen", "unix:///run/user/codex.sock"},
			want: func(string, string) appServerListen {
				return appServerListen{
					Kind:       appServerListenUnix,
					SocketPath: "/run/user/codex.sock",
				}
			},
		},
		{
			name: "websocket_loopback",
			args: []string{"app-server", "--listen", "ws://127.0.0.1:4500"},
			want: webSocket("127.0.0.1:4500"),
		},
		{
			name: "websocket_port_zero",
			args: []string{"app-server", "--listen", "ws://127.0.0.1:0"},
			want: webSocket("127.0.0.1:0"),
		},
		{
			name: "websocket_loopback_range",
			args: []string{"app-server", "--listen", "ws://127.2.3.4:4500"},
			want: webSocket("127.2.3.4:4500"),
		},
		{
			name: "websocket_ipv6_loopback",
			args: []string{"app-server", "--listen", "ws://[::1]:4500"},
			want: webSocket("[::1]:4500"),
		},

		{
			name:      "config_override_without_equals",
			args:      []string{"app-server", "-c", "abc"},
			wantError: configOverrideError,
		},
		{
			name:      "old_config_path_on_root",
			args:      []string{"--config", "/path/to/config.yaml", "app-server"},
			wantError: configOverrideError,
		},
		{
			name:      "listen_off",
			args:      []string{"app-server", "--listen", "off"},
			wantError: "--listen off is not supported",
		},
		{
			name:      "listen_tcp",
			args:      []string{"app-server", "--listen", "tcp://127.0.0.1:1"},
			wantError: `--listen "tcp://127.0.0.1:1" is not supported; ` + listenHelp,
		},
		{
			name:      "listen_unix_without_slashes",
			args:      []string{"app-server", "--listen", "unix:app-server.sock"},
			wantError: `--listen "unix:app-server.sock" is not supported; ` + listenHelp,
		},
		{
			name:      "websocket_all_interfaces",
			args:      []string{"app-server", "--listen", "ws://0.0.0.0:1"},
			wantError: "app-server only listens on loopback; use client.url to reach a remote runtime server",
		},
		{
			name:      "websocket_public_address",
			args:      []string{"app-server", "--listen", "ws://10.0.0.5:4500"},
			wantError: "app-server only listens on loopback; use client.url to reach a remote runtime server",
		},
		{
			name:      "websocket_host_name",
			args:      []string{"app-server", "--listen", "ws://localhost:1"},
			wantError: "--listen ws:// needs IP:PORT, for example ws://127.0.0.1:4500",
		},
		{
			name:      "websocket_without_port",
			args:      []string{"app-server", "--listen", "ws://127.0.0.1"},
			wantError: "--listen ws:// needs IP:PORT, for example ws://127.0.0.1:4500",
		},
		{
			name:      "ws_auth",
			args:      []string{"app-server", "--ws-auth", "capability-token"},
			wantError: "--ws-auth is not supported: " + wsAuthError,
		},
		{
			name:      "ws_token_file_names_its_own_flag",
			args:      []string{"app-server", "--listen", "ws://127.0.0.1:0", "--ws-token-file", "/tmp/token"},
			wantError: "--ws-token-file is not supported: " + wsAuthError,
		},
		{
			name:      "stdio_conflicts_with_unix",
			args:      []string{"app-server", "--stdio", "--listen", "unix://"},
			wantError: "--stdio conflicts with --listen",
		},
		{
			name:      "proxy_subcommand",
			args:      []string{"app-server", "proxy"},
			wantError: `unknown command "proxy" for "openvaala app-server"`,
		},
	}
	// 2. 仅执行参数解析，检查监听结果、忽略参数或错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			directory, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Chdir(directory)
			writeTestFiles(t, map[string]string{filepath.Join(directory, "config.yaml"): "run:\n  agent_name: assistant\n"})

			// 替换业务处理函数，仅解析监听参数。
			var gotArgs *AppArgs
			var got appServerListen
			root := NewCLI(&CLIOpt{
				Stdout: io.Discard,
				Stderr: io.Discard,
			})
			target, _, err := root.Find([]string{"app-server"})
			if err != nil {
				t.Fatal(err)
			}
			target.RunE = BuildCLIHandler(func(_ context.Context, args *AppArgs) (err error) {
				gotArgs = args
				got, err = getAppServerListen(args)
				return err
			})
			root.SetArgs(tt.args)
			err = root.ExecuteContext(t.Context())

			if tt.want == nil {
				if err == nil || err.Error() != tt.wantError {
					t.Fatalf("error = %v, want %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if want := tt.want(home, directory); got != want {
				t.Fatalf("listen = %+v, want %+v", got, want)
			}
			if !slices.Equal(gotArgs.IgnoredCodexFlags, tt.wantIgnored) {
				t.Fatalf("IgnoredCodexFlags = %v, want %v", gotArgs.IgnoredCodexFlags, tt.wantIgnored)
			}
		})
	}
}

// TestCodexFlagsHiddenInHelp 检查帮助显示产品参数并隐藏 Codex 兼容参数。
func TestCodexFlagsHiddenInHelp(t *testing.T) {
	// 1. 列出各命令应显示和隐藏的帮助文字。
	tests := []struct {
		name string
		// command 是子命令路径，为 nil 时是根命令。
		command []string
		// wantShown 是帮助里要有的文字，wantHidden 是不该出现的 codex 兼容参数。
		wantShown  []string
		wantHidden []string
	}{
		{
			name:       "root",
			wantShown:  []string{"--config-file", "--agents-dir"},
			wantHidden: []string{"--enable", "--disable", "--strict-config", "-c, --config"},
		},
		{
			name:       "acp",
			command:    []string{"acp"},
			wantShown:  []string{"--config-file", "--url"},
			wantHidden: []string{"--enable", "--disable", "--strict-config", "-c, --config"},
		},
		{
			name:      "app_server",
			command:   []string{"app-server"},
			wantShown: []string{"--config-file", "--listen", "--stdio", "--url", `follows "codex app-server"`},
			wantHidden: []string{
				"--enable", "--disable", "--strict-config", "-c, --config", "--analytics-default-enabled", "--code-mode-host",
				"--remote-control", "--ws-auth", "--ws-token-file", "--ws-max-clock-skew-seconds",
			},
		},
	}
	// 2. 执行 --help 并检查输出。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			root := NewCLI(&CLIOpt{
				Stdout: &output,
				Stderr: &output,
			})
			root.SetArgs(append(slices.Clone(tt.command), "--help"))
			if err := root.ExecuteContext(t.Context()); err != nil {
				t.Fatal(err)
			}
			for _, text := range tt.wantShown {
				if !strings.Contains(output.String(), text) {
					t.Errorf("help does not mention %q:\n%s", text, output.String())
				}
			}
			for _, text := range tt.wantHidden {
				if strings.Contains(output.String(), text) {
					t.Errorf("help mentions %q:\n%s", text, output.String())
				}
			}
		})
	}
}
