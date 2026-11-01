package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// demoPluginManifest 是名为 demo 的最小插件：没有 skill、hook 和 MCP server，只有名字和版本。
const demoPluginManifest = "name: demo\nversion: 1.2.3\n"

// badPluginManifest 是带有无效 MCP executors 配置的 bad 插件，加载时返回错误。
const badPluginManifest = "name: bad\nmcp_servers:\n  - {id: bad-docs, type: stdio, command: server, executors: [local]}\n"

// TestPluginCommands 检查插件安装、启停、更新、删除和损坏配置处理的命令输出。
func TestPluginCommands(t *testing.T) {
	// 1. 列出插件命令顺序、配置样例和预期结果。
	const noServe = "no runtime server is running; changes apply when it starts\n"
	// step 保存一次命令输入和预期结果；%[1]s 替换为 demo 来源目录，wantError 非空时检查错误。
	type step struct {
		args       []string
		wantStdout string
		wantError  string
	}
	tests := []struct {
		name string
		// files 是配置目录里的文件（相对 config.yaml 所在目录）；demo 插件的来源目录由测试固定创建。
		files map[string]string
		steps []step
	}{
		{
			name: "lifecycle_of_an_installed_plugin",
			steps: []step{
				{
					args:       []string{"plugin", "update"},
					wantStdout: "no plugins to update\n",
				},
				{
					args:       []string{"plugin", "install", "%[1]s"},
					wantStdout: "installed plugin demo\n" + noServe,
				},
				{
					args:      []string{"plugin", "install", "%[1]s"},
					wantError: `plugin "demo" is already installed`,
				},
				{
					args:       []string{"plugin", "list"},
					wantStdout: "demo  1.2.3  enabled  %[1]s\n",
				},
				{
					args:       []string{"plugin", "update"},
					wantStdout: "updated plugin demo\n" + noServe,
				},
				{
					args:       []string{"plugin", "disable", "demo"},
					wantStdout: "disabled plugin demo\n" + noServe,
				},
				{
					args:       []string{"plugin", "disable", "demo"},
					wantStdout: "plugin demo is already disabled\n",
				},
				{
					args:       []string{"plugin", "list"},
					wantStdout: "demo  1.2.3  disabled  %[1]s\n",
				},
				{
					args:       []string{"plugin", "enable", "demo"},
					wantStdout: "enabled plugin demo\n" + noServe,
				},
				{
					args:       []string{"plugin", "enable", "demo"},
					wantStdout: "plugin demo is already enabled\n",
				},
				{
					args:       []string{"plugin", "uninstall", "demo"},
					wantStdout: "uninstalled plugin demo\n" + noServe,
				},
				{
					args:       []string{"plugin", "list"},
					wantStdout: "",
				},
				{
					args:      []string{"plugin", "uninstall", "demo"},
					wantError: `plugin "demo" is not installed`,
				},
			},
		},
		{
			name: "install_with_flags",
			steps: []step{
				{
					args:      []string{"plugin", "install", "%[1]s", "--ref", "main"},
					wantError: "--ref requires a git source",
				},
				{
					args:      []string{"plugin", "install", "%[1]s", "--path", "../outside"},
					wantError: "--path must be a relative path",
				},
				{
					args:       []string{"plugin", "install", "%[1]s", "--path", "."},
					wantStdout: "installed plugin demo\n" + noServe,
				},
			},
		},
		{
			name: "install_disabled_plugin_tells_how_to_enable_it",
			files: map[string]string{
				"config.yaml": "run:\n  agent_name: assistant\nplugins:\n  disabled: [demo]\n",
			},
			steps: []step{
				{
					args:       []string{"plugin", "install", "%[1]s"},
					wantStdout: "installed plugin demo\nplugin demo is disabled; run openvaala plugin enable demo\n" + noServe,
				},
				{
					args:       []string{"plugin", "list"},
					wantStdout: "demo  1.2.3  disabled  %[1]s\n",
				},
				{
					args:       []string{"plugin", "enable", "demo"},
					wantStdout: "enabled plugin demo\n" + noServe,
				},
			},
		},
		{
			name: "update_named_plugin_without_install_record",
			files: map[string]string{
				"plugins/manual/.openvaala-plugin/plugin.yaml": "name: manual\n",
			},
			steps: []step{
				{
					args:       []string{"plugin", "update"},
					wantStdout: "no plugins to update\n",
				},
				{
					args:      []string{"plugin", "update", "manual"},
					wantError: `plugin "manual": not installed by openvaala plugin install`,
				},
				{
					args:       []string{"plugin", "list"},
					wantStdout: "manual  -  enabled  (manual)\n",
				},
			},
		},
		{
			name: "list_shows_directory_without_manifest",
			files: map[string]string{
				"plugins/empty/README.md": "not a plugin\n",
			},
			steps: []step{
				{
					args:       []string{"plugin", "list"},
					wantStdout: "empty  -  enabled  (manual)  not a plugin: missing supported plugin manifest\n",
				},
			},
		},
		{
			name: "disable_repairs_configuration_broken_by_plugin",
			files: map[string]string{
				"plugins/bad/.openvaala-plugin/plugin.yaml": badPluginManifest,
			},
			steps: []step{
				{
					args:      []string{"config", "check"},
					wantError: "plugin mcp server cannot set executors",
				},
				{
					args:       []string{"plugin", "list"},
					wantStdout: "bad  -  enabled  (manual)\n",
				},
				{
					args:       []string{"plugin", "disable", "bad"},
					wantStdout: "disabled plugin bad\n" + noServe,
				},
				{
					args:       []string{"config", "check"},
					wantStdout: "configuration valid: agent=assistant provider=local model=default\n",
				},
			},
		},
		{
			name: "uninstall_repairs_configuration_broken_by_plugin",
			files: map[string]string{
				"plugins/bad/.openvaala-plugin/plugin.yaml": badPluginManifest,
			},
			steps: []step{
				{
					args:      []string{"config", "check"},
					wantError: "plugin mcp server cannot set executors",
				},
				{
					args:       []string{"plugin", "uninstall", "bad"},
					wantStdout: "uninstalled plugin bad\n" + noServe,
				},
				{
					args:       []string{"config", "check"},
					wantStdout: "configuration valid: agent=assistant provider=local model=default\n",
				},
			},
		},
	}
	// 2. 准备本机插件源和配置，逐条执行命令并检查输出或错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			configPath := writeTestConfig(t, "http://127.0.0.1:1", "assistant")
			source := filepath.Join(t.TempDir(), "demo-source")
			writeTestFiles(t, map[string]string{filepath.Join(source, ".openvaala-plugin", "plugin.yaml"): demoPluginManifest})
			for path, content := range tt.files {
				writeTestFiles(t, map[string]string{filepath.Join(filepath.Dir(configPath), path): content})
			}
			for _, step := range tt.steps {
				args := make([]string, len(step.args))
				for index, arg := range step.args {
					args[index] = strings.ReplaceAll(arg, "%[1]s", source)
				}
				wantStdout := strings.ReplaceAll(step.wantStdout, "%[1]s", source)
				stdout, err := runCLI(t, configPath, args...)
				if step.wantError != "" {
					if err == nil || !strings.Contains(err.Error(), step.wantError) {
						t.Fatalf("%v error = %v, want containing %q", step.args, err, step.wantError)
					}
					continue
				}
				if err != nil || stdout != wantStdout {
					t.Fatalf("%v = %q, %v; want %q", step.args, stdout, err, wantStdout)
				}
			}
		})
	}
}
