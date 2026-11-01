package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestBuildCLIHandlerConfigFlags 检查配置参数和自动启动 serve 的命令。
func TestBuildCLIHandlerConfigFlags(t *testing.T) {
	// 1. 列出配置路径、环境变量和 Codex 覆盖参数场景。
	tests := []struct {
		name string
		args []string
		// environment 是测试进程里设置的环境变量，模拟用户的 shell。
		environment map[string]string
		// wantConfigFlags 返回期望的 AppArgs.ConfigFlags，directory 是命令运行时的当前目录。
		wantConfigFlags func(directory string) []string
		// wantConfigEnvironment 非空时，要求拉起 serve 的命令环境里只有这一条 OPENVAALA_CONFIG。
		wantConfigEnvironment func(directory string) string
	}{
		{
			name: "relative_config_file_made_absolute",
			args: []string{"--config-file", "./config.yaml"},
			wantConfigFlags: func(directory string) []string {
				return []string{"--config-file=" + filepath.Join(directory, "config.yaml")}
			},
		},
		{
			name:            "unset_flags_omitted",
			args:            []string{"--agent", "x"},
			wantConfigFlags: func(string) []string { return []string{"--agent=x"} },
		},
		{
			// 只有 --config-file 转成绝对路径；--agents-dir 和 --models-dir 由配置加载按配置文件所在目录展开，原样交给 serve。
			name: "only_config_file_made_absolute",
			args: []string{"--agent", "x", "--models-dir", "models", "--agents-dir=./agents", "--config-file", "config.yaml"},
			wantConfigFlags: func(directory string) []string {
				return []string{
					"--config-file=" + filepath.Join(directory, "config.yaml"),
					"--agents-dir=./agents",
					"--models-dir=models",
					"--agent=x",
				}
			},
		},
		{
			// ~ 开头的路径由配置加载按用户主目录展开，原样交给 serve。
			name:            "home_relative_config_file_kept",
			args:            []string{"--config-file=~/config.yaml"},
			wantConfigFlags: func(string) []string { return []string{"--config-file=~/config.yaml"} },
		},
		{
			// Codex 的 -c/--config 覆盖项收下不用，不转交给 serve；--enable、--disable、--strict-config 同样不转交。
			name: "codex_overrides_not_forwarded",
			args: []string{"-c", "model=x", "--config", "a.b=c", "--enable", "f", "--disable", "g", "--strict-config", "--agent", "x"},
			wantConfigFlags: func(string) []string {
				return []string{"--agent=x"}
			},
		},
		{
			name:            "no_flags",
			wantConfigFlags: func(string) []string { return nil },
		},
		{
			name:        "relative_env_config",
			environment: map[string]string{"OPENVAALA_CONFIG": "./config.yaml"},
			wantConfigFlags: func(string) []string {
				return nil
			},
			wantConfigEnvironment: func(directory string) string {
				return "OPENVAALA_CONFIG=" + filepath.Join(directory, "config.yaml")
			},
		},
	}
	// 2. 逐场景加载配置，检查 AppArgs 和 serve 的参数、目录、会话组及环境。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			directory, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Chdir(directory)
			writeTestFiles(t, map[string]string{
				filepath.Join(directory, "config.yaml"): "run:\n  agent_name: assistant\n",
				filepath.Join(home, "config.yaml"):      "run:\n  agent_name: assistant\n",
			})
			for name, value := range tt.environment {
				t.Setenv(name, value)
			}

			var got *AppArgs
			command := NewCLI(&CLIOpt{
				Stdout: io.Discard,
				Stderr: io.Discard,
			})
			command.RunE = BuildCLIHandler(func(_ context.Context, args *AppArgs) error {
				got = args
				return nil
			})
			command.SetArgs(tt.args)
			if err := command.ExecuteContext(t.Context()); err != nil {
				t.Fatal(err)
			}

			wantConfigFlags := tt.wantConfigFlags(directory)
			if !slices.Equal(got.ConfigFlags, wantConfigFlags) {
				t.Fatalf("ConfigFlags = %v, want %v", got.ConfigFlags, wantConfigFlags)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			if got.Executable != executable {
				t.Fatalf("Executable = %q, want %q", got.Executable, executable)
			}

			// 检查 serve 参数、主目录和独立会话组。
			serve, err := newRuntimeServerCommand(&RuntimeServerStartOpt{Executable: got.Executable, ConfigFlags: got.ConfigFlags, IdleTimeout: runtimeServerIdleTimeout})
			if err != nil {
				t.Fatal(err)
			}
			wantArgs := slices.Concat([]string{executable}, wantConfigFlags, []string{"serve", "--idle-timeout", "10s"})
			if !slices.Equal(serve.Args, wantArgs) {
				t.Fatalf("serve command args = %v, want %v", serve.Args, wantArgs)
			}
			if serve.Dir != home || serve.SysProcAttr == nil || !serve.SysProcAttr.Setsid {
				t.Fatalf("serve command dir = %q, SysProcAttr = %+v; want dir %q and a new session", serve.Dir, serve.SysProcAttr, home)
			}
			// 标准流均为空，对应 /dev/null。
			if serve.Stdin != nil || serve.Stdout != nil || serve.Stderr != nil {
				t.Fatalf("serve command stdio = %v, %v, %v; want all nil (/dev/null)", serve.Stdin, serve.Stdout, serve.Stderr)
			}
			if tt.wantConfigEnvironment == nil {
				return
			}
			var configEntries []string
			for _, entry := range serve.Env {
				if strings.HasPrefix(entry, "OPENVAALA_CONFIG=") {
					configEntries = append(configEntries, entry)
				}
			}
			if want := tt.wantConfigEnvironment(directory); !slices.Equal(configEntries, []string{want}) {
				t.Fatalf("serve command OPENVAALA_CONFIG entries = %v, want [%s]", configEntries, want)
			}
		})
	}
}

// TestBuildCLIHandlerRuntimeURL 检查各客户端命令读取 --url。
func TestBuildCLIHandlerRuntimeURL(t *testing.T) {
	// 1. 列出根命令和各客户端命令。
	const runtimeURL = "ws://10.0.0.5:8790/api/v1/openvaala"
	tests := []struct {
		name string
		// command 是子命令路径，为 nil 时是根命令。
		command []string
	}{
		{name: "root"},
		{
			name:    "tui",
			command: []string{"tui"},
		},
		{
			name:    "acp",
			command: []string{"acp"},
		},
		{
			name:    "app_server",
			command: []string{"app-server"},
		},
		{
			name:    "codex",
			command: []string{"codex"},
		},
	}
	// 2. 执行命令并检查业务处理收到的连接地址。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			directory, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Chdir(directory)
			writeTestFiles(t, map[string]string{
				filepath.Join(directory, "config.yaml"): "run:\n  agent_name: assistant\n",
				filepath.Join(home, "config.yaml"):      "run:\n  agent_name: assistant\n",
			})

			var got *AppArgs
			root := NewCLI(&CLIOpt{
				Stdout: io.Discard,
				Stderr: io.Discard,
			})
			target, _, err := root.Find(tt.command)
			if err != nil {
				t.Fatal(err)
			}
			target.RunE = BuildCLIHandler(func(_ context.Context, args *AppArgs) error {
				got = args
				return nil
			})
			root.SetArgs(append(slices.Clone(tt.command), "--url", runtimeURL))
			if err := root.ExecuteContext(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got.RuntimeURL != runtimeURL {
				t.Fatalf("RuntimeURL = %q, want %q", got.RuntimeURL, runtimeURL)
			}
		})
	}
}

// TestCodexConfigOverrideCheck 检查所有命令拒绝不含等号的 Codex 配置覆盖项。
func TestCodexConfigOverrideCheck(t *testing.T) {
	// 1. 列出命令路径和无效覆盖项。
	const wantError = "--config takes key=value (a Codex configuration override); use --config-file for the OpenVaala configuration file"
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "tui_default",
			args: []string{"--config", "/x"},
		},
		{
			name: "tui",
			args: []string{"--config", "/x", "tui"},
		},
		{
			name: "acp",
			args: []string{"--config", "/x", "acp"},
		},
		{
			name: "app_server",
			args: []string{"app-server", "-c", "abc"},
		},
		{
			name: "codex",
			args: []string{"-c", "abc", "codex"},
		},
		{
			name: "serve",
			args: []string{"--config", "/x", "serve", "stop"},
		},
		{
			name: "reindex",
			args: []string{"--config", "/x", "reindex"},
		},
		{
			name: "config_show",
			args: []string{"--config", "/x", "config", "show"},
		},
		{
			name: "config_init",
			args: []string{"--config", "/x", "config", "init"},
		},
		{
			name: "completion",
			args: []string{"--config", "/x", "completion", "bash"},
		},
		{
			name: "second_override_without_equals",
			args: []string{"-c", "a=b", "-c", "abc", "reindex"},
		},
	}
	// 2. 执行命令并检查错误提示。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Chdir(t.TempDir())
			command := NewCLI(&CLIOpt{
				Stdout: io.Discard,
				Stderr: io.Discard,
			})
			command.SetArgs(tt.args)
			if err := command.ExecuteContext(t.Context()); err == nil || err.Error() != wantError {
				t.Fatalf("error = %v, want %q", err, wantError)
			}
		})
	}
}

// TestGetConfigFlagsWithoutLoading 检查原生入口只收集参数，不提前读取缺失或损坏的配置。
func TestGetConfigFlagsWithoutLoading(t *testing.T) {
	// 1. 覆盖缺失文件、损坏文件与未显式设置参数。
	for _, name := range []string{"missing", "malformed", "unset"} {
		t.Run(name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if name == "malformed" {
				if err := os.WriteFile("config.yaml", []byte("server: [invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			root := NewCLI(&CLIOpt{Stdout: io.Discard, Stderr: io.Discard})
			arguments := []string{"--config-file=./config.yaml", "--agent=assistant"}
			if name == "unset" {
				arguments = nil
			}
			if err := root.ParseFlags(arguments); err != nil {
				t.Fatal(err)
			}
			flags, err := GetConfigFlags(root.Flags())
			if err != nil {
				t.Fatal(err)
			}
			// 2. 缺失或损坏配置仍返回原启动参数，只有相对配置路径被绝对化。
			var want []string
			if name != "unset" {
				directory, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				want = []string{"--config-file=" + filepath.Join(directory, "config.yaml"), "--agent=assistant"}
			}
			if !slices.Equal(flags, want) {
				t.Fatalf("flags = %v, want %v", flags, want)
			}
		})
	}
}
